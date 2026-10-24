package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// schemaKey is the meta row holding the applied migration number. A meta row
// (rather than PRAGMA user_version) leaves user_version free for nobody to
// fight over: other packages migrate their own tables on the same file.
const schemaKey = "store_schema_version"

// migrations are applied in order; entry i brings the schema to version i+1.
// Never edit an applied migration: append a new one. Statements use IF NOT
// EXISTS so re-running a partially applied step is harmless.
var migrations = []string{
	// 1: initial schema.
	`
CREATE TABLE IF NOT EXISTS sessions (
	id               TEXT PRIMARY KEY,
	agent            TEXT NOT NULL,
	agent_session_id TEXT NOT NULL DEFAULT '',
	pid              INTEGER NOT NULL DEFAULT 0,
	state            TEXT NOT NULL,
	exited_at        INTEGER,
	updated_at       INTEGER NOT NULL,
	data             TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_agent_sid ON sessions(agent, agent_session_id);
CREATE INDEX IF NOT EXISTS sessions_exited ON sessions(exited_at);

CREATE TABLE IF NOT EXISTS requests (
	id          TEXT PRIMARY KEY,
	session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	checkpoint  TEXT NOT NULL,
	status      TEXT NOT NULL,
	tool        TEXT NOT NULL DEFAULT '',
	summary     TEXT NOT NULL DEFAULT '',
	tool_use_id TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	closed_at   INTEGER,
	data        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS requests_status ON requests(status, created_at);
CREATE INDEX IF NOT EXISTS requests_session ON requests(session_id, status);
CREATE INDEX IF NOT EXISTS requests_closed ON requests(closed_at);

CREATE TABLE IF NOT EXISTS events (
	seq        INTEGER PRIMARY KEY AUTOINCREMENT,
	at         INTEGER NOT NULL,
	type       TEXT NOT NULL,
	session_id TEXT NOT NULL DEFAULT '',
	request_id TEXT NOT NULL DEFAULT '',
	data       TEXT
);
CREATE INDEX IF NOT EXISTS events_session ON events(session_id, seq);
CREATE INDEX IF NOT EXISTS events_at ON events(at);

CREATE TABLE IF NOT EXISTS grants (
	id          TEXT PRIMARY KEY,
	session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	tool        TEXT NOT NULL,
	prefix      TEXT NOT NULL DEFAULT '',
	decision_by TEXT NOT NULL,
	created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS grants_session ON grants(session_id);

CREATE TABLE IF NOT EXISTS tasks (
	id         TEXT PRIMARY KEY,
	session_id TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL,
	updated_at INTEGER NOT NULL,
	data       TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS evidence (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	at         INTEGER NOT NULL,
	layer      TEXT NOT NULL,
	state      TEXT NOT NULL,
	kind       TEXT NOT NULL DEFAULT '',
	detail     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS evidence_session ON evidence(session_id, id);
CREATE INDEX IF NOT EXISTS evidence_at ON evidence(at);
`,
	// 2 (0.2): private per-session values the API never returns (Claude's
	// messaging-socket path and token), and daily usage per session/model
	// for insights. Allocated up front for all 0.2 workstreams.
	`
CREATE TABLE IF NOT EXISTS session_secrets (
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	kind       TEXT NOT NULL,
	value      TEXT NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (session_id, kind)
);
CREATE TABLE IF NOT EXISTS usage_daily (
	session_id  TEXT NOT NULL,
	day         TEXT NOT NULL,
	agent       TEXT NOT NULL,
	model       TEXT NOT NULL DEFAULT '',
	repo        TEXT NOT NULL DEFAULT '',
	input       INTEGER NOT NULL DEFAULT 0,
	output      INTEGER NOT NULL DEFAULT 0,
	cache_read  INTEGER NOT NULL DEFAULT 0,
	cache_write INTEGER NOT NULL DEFAULT 0,
	reasoning   INTEGER NOT NULL DEFAULT 0,
	cost_usd    REAL NOT NULL DEFAULT 0,
	turns       INTEGER NOT NULL DEFAULT 0,
	updated_at  INTEGER NOT NULL,
	PRIMARY KEY (session_id, day, model)
);
CREATE INDEX IF NOT EXISTS usage_daily_day ON usage_daily(day);
`,
}

// SchemaVersion is the schema version this build migrates to.
func SchemaVersion() int { return len(migrations) }

func (db *DB) migrate(ctx context.Context) error {
	if _, err := db.w.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("create meta table: %w", err)
	}
	cur, err := db.schemaVersion(ctx)
	if err != nil {
		return err
	}
	if cur > len(migrations) {
		return fmt.Errorf("database schema v%d is newer than this pilothouse (v%d): upgrade pilothouse", cur, len(migrations))
	}
	for v := cur; v < len(migrations); v++ {
		if err := db.applyMigration(ctx, v+1, migrations[v]); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) schemaVersion(ctx context.Context) (int, error) {
	var s string
	err := db.w.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, schemaKey).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("bad schema version %q", s)
	}
	return v, nil
}

func (db *DB) applyMigration(ctx context.Context, version int, stmt string) error {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migration %d: %w", version, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("migration %d: %w", version, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		schemaKey, strconv.Itoa(version)); err != nil {
		return fmt.Errorf("migration %d: %w", version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration %d: %w", version, err)
	}
	return nil
}
