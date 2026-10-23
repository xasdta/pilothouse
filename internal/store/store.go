// Package store persists Pilothouse's state in SQLite (modernc.org/sqlite,
// pure Go). It owns the sessions, requests, events, grants, tasks, evidence
// and meta tables.
//
// Writes go through DB.Write, which runs one transaction on a single writer
// connection under a mutex. Events appended inside that transaction (the
// outbox) are handed to the commit hook in seq order while the mutex is still
// held, so subscribers observe exactly the committed order. Reads use a
// separate connection pool and never block on writers (WAL).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
)

// Errors returned by lookups. ErrNotFound and ErrAlreadyClosed are the core
// sentinels so callers can map them without knowing about the store.
var (
	ErrNotFound      = core.ErrNotFound
	ErrAlreadyClosed = core.ErrAlreadyClosed
	ErrAmbiguous     = errors.New("ambiguous id prefix")
)

// DB is an open Pilothouse database. It is safe for concurrent use.
type DB struct {
	Reader // reads on the reader pool

	path   string
	w      *sql.DB // exactly one connection: SQLite has one writer anyway
	r      *sql.DB // reader pool
	shared *sql.DB // handed to other packages (web) for their own tables

	mu       sync.Mutex // serializes Write so commit + publish happen in seq order
	onCommit func([]api.Event)
	now      func() time.Time
}

// Open opens (creating if needed) the database at path and applies
// migrations. The file is created with mode 0600.
func Open(path string) (*DB, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	_ = f.Close()
	_ = os.Chmod(path, 0o600)

	db := &DB{path: path, now: time.Now}
	if db.w, err = openPool(path, "immediate", 1); err != nil {
		return nil, err
	}
	if db.r, err = openPool(path, "deferred", 8); err != nil {
		_ = db.w.Close()
		return nil, err
	}
	if db.shared, err = openPool(path, "immediate", 4); err != nil {
		_ = db.w.Close()
		_ = db.r.Close()
		return nil, err
	}
	db.Reader = Reader{q: db.r}
	if err := db.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	// The WAL side files are created with the process umask; tighten them.
	for _, p := range []string{path + "-wal", path + "-shm"} {
		_ = os.Chmod(p, 0o600)
	}
	return db, nil
}

// openPool opens one database/sql handle. txlock=immediate makes BEGIN take
// the write lock up front, so a writer waits on busy_timeout instead of
// failing with SQLITE_BUSY when it upgrades a read transaction.
func openPool(path, txlock string, maxConns int) (*sql.DB, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	// A new database frees deleted pages incrementally (DB.Compact). The
	// setting must precede WAL mode and the first table; on an existing
	// database it changes nothing until Compact's one-time VACUUM.
	q.Add("_pragma", "auto_vacuum(INCREMENTAL)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", txlock)
	dsn := "file:" + path + "?" + q.Encode()
	h, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	h.SetMaxOpenConns(maxConns)
	h.SetMaxIdleConns(maxConns)
	h.SetConnMaxLifetime(0)
	if err := h.Ping(); err != nil {
		_ = h.Close()
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	return h, nil
}

// SQL returns a connection pool other packages may use for their own tables
// (the web package keeps web_* tables here). Its writes are serialized
// against the store's by SQLite's lock (busy_timeout 5 s); callers must not
// hold a transaction open while calling back into the daemon.
func (db *DB) SQL() *sql.DB { return db.shared }

// Path returns the database file path.
func (db *DB) Path() string { return db.path }

// SetOnCommit installs fn, called after every successful Write with the
// events that transaction appended, in seq order. fn runs while the write
// lock is held, so it must not block or call Write.
func (db *DB) SetOnCommit(fn func([]api.Event)) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.onCommit = fn
}

// SetClock replaces the clock used to stamp transactions (tests).
func (db *DB) SetClock(now func() time.Time) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.now = now
}

// Close closes every pool.
func (db *DB) Close() error {
	var errs []error
	for _, h := range []*sql.DB{db.shared, db.r, db.w} {
		if h != nil {
			errs = append(errs, h.Close())
		}
	}
	return errors.Join(errs...)
}

// Write runs fn in one write transaction. Every mutation and the events
// describing it commit together or not at all.
func (db *DB) Write(ctx context.Context, fn func(tx *Tx) error) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	stx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin write: %w", err)
	}
	tx := &Tx{Reader: Reader{q: stx}, tx: stx, now: db.now().UTC().Round(0)}
	if err := fn(tx); err != nil {
		_ = stx.Rollback()
		return err
	}
	if err := stx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	if db.onCommit != nil && len(tx.events) > 0 {
		db.onCommit(tx.events)
	}
	return nil
}

// Read runs fn in one read-only transaction so every query sees the same
// snapshot of the database (used for Snapshot: seq and rows agree).
func (db *DB) Read(ctx context.Context, fn func(r Reader) error) error {
	stx, err := db.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin read: %w", err)
	}
	defer func() { _ = stx.Rollback() }()
	return fn(Reader{q: stx})
}

// querier is satisfied by *sql.DB and *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Reader holds every read query. DB embeds one on the reader pool; Tx embeds
// one on the write transaction so read-modify-write sequences are consistent.
type Reader struct{ q querier }

// Tx is a write transaction. Methods that change state do not emit events
// on their own: the caller appends the matching event with Emit in the same
// transaction.
type Tx struct {
	Reader
	tx     *sql.Tx
	now    time.Time
	events []api.Event
}

// CompactResult describes what Compact did.
type CompactResult struct {
	Vacuumed   bool // a full VACUUM ran (one-time conversion or mostly-free file)
	FreedPages int64
}

// Compact returns space freed by deletes (retention) to the filesystem:
// it checkpoints and truncates the WAL, then releases free pages with
// incremental_vacuum. A database created before auto_vacuum=INCREMENTAL
// was enabled can't vacuum incrementally; it gets one full VACUUM, which
// also converts it, when more than half of its pages are free. Compact
// holds the write lock, so no Write runs meanwhile.
func (db *DB) Compact(ctx context.Context) (CompactResult, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	var res CompactResult
	pragmaInt := func(name string) (int64, error) {
		var v int64
		err := db.w.QueryRowContext(ctx, `PRAGMA `+name).Scan(&v)
		return v, err
	}
	before, err := pragmaInt("freelist_count")
	if err != nil {
		return res, fmt.Errorf("compact: %w", err)
	}
	pages, err := pragmaInt("page_count")
	if err != nil {
		return res, fmt.Errorf("compact: %w", err)
	}
	mode, err := pragmaInt("auto_vacuum")
	if err != nil {
		return res, fmt.Errorf("compact: %w", err)
	}
	switch {
	case mode != 2 && pages > 0 && before*2 > pages:
		if _, err := db.w.ExecContext(ctx, `PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
			return res, fmt.Errorf("compact: %w", err)
		}
		if _, err := db.w.ExecContext(ctx, `VACUUM`); err != nil {
			return res, fmt.Errorf("compact: vacuum: %w", err)
		}
		res.Vacuumed = true
	case mode == 2 && before > 0:
		if _, err := db.w.ExecContext(ctx, `PRAGMA incremental_vacuum`); err != nil {
			return res, fmt.Errorf("compact: incremental vacuum: %w", err)
		}
	}
	if _, err := db.w.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return res, fmt.Errorf("compact: checkpoint: %w", err)
	}
	after, err := pragmaInt("freelist_count")
	if err != nil {
		return res, fmt.Errorf("compact: %w", err)
	}
	res.FreedPages = before - after
	return res, nil
}
