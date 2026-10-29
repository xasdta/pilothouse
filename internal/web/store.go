package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

// migrations are numbered and applied once each, recorded in
// web_migrations (the store's own schema version is not touched).
// Times are Unix milliseconds.
var migrations = []string{
	1: `
CREATE TABLE IF NOT EXISTS web_devices (
	id           TEXT PRIMARY KEY,
	name         TEXT NOT NULL,
	public_key   TEXT NOT NULL,
	token_hash   BLOB NOT NULL,
	created_at   INTEGER NOT NULL,
	last_seen_at INTEGER,
	revoked_at   INTEGER
);
CREATE TABLE IF NOT EXISTS web_push_subs (
	endpoint   TEXT PRIMARY KEY,
	device_id  TEXT NOT NULL REFERENCES web_devices(id) ON DELETE CASCADE,
	p256dh     TEXT NOT NULL,
	auth       TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS web_push_subs_device ON web_push_subs(device_id);
CREATE TABLE IF NOT EXISTS web_pair_codes (
	code_hash  BLOB PRIMARY KEY,
	kind       TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	used_at    INTEGER
);
CREATE TABLE IF NOT EXISTS web_nonces (
	device_id  TEXT NOT NULL,
	nonce      TEXT NOT NULL,
	expires_at INTEGER NOT NULL,
	PRIMARY KEY (device_id, nonce)
);
CREATE INDEX IF NOT EXISTS web_nonces_expiry ON web_nonces(expires_at);
CREATE TABLE IF NOT EXISTS web_action_tokens (
	token_hash BLOB PRIMARY KEY,
	request_id TEXT NOT NULL,
	behavior   TEXT NOT NULL,
	expires_at INTEGER NOT NULL,
	used_at    INTEGER
);
CREATE INDEX IF NOT EXISTS web_action_tokens_request ON web_action_tokens(request_id);
`,
	2: `
CREATE TABLE IF NOT EXISTS web_passkeys (
	cred_id      TEXT PRIMARY KEY,
	device_id    TEXT NOT NULL REFERENCES web_devices(id) ON DELETE CASCADE,
	rp_id        TEXT NOT NULL,
	public_key   TEXT NOT NULL,
	sign_count   INTEGER NOT NULL DEFAULT 0,
	created_at   INTEGER NOT NULL,
	last_used_at INTEGER
);
CREATE INDEX IF NOT EXISTS web_passkeys_device ON web_passkeys(device_id);
`,
}

func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS web_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("web: migrate: %w", err)
	}
	for v := 1; v < len(migrations); v++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("web: migrate: %w", err)
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM web_migrations WHERE version = ?`, v).Scan(&n); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("web: migrate: %w", err)
		}
		if n > 0 {
			_ = tx.Rollback()
			continue
		}
		if _, err := tx.ExecContext(ctx, migrations[v]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("web: migration %d: %w", v, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO web_migrations (version, applied_at) VALUES (?, ?)`, v, time.Now().UnixMilli()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("web: migration %d: %w", v, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("web: migration %d: %w", v, err)
		}
	}
	return nil
}

func hashSecret(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return b
}

func randomToken(n int) string { return base64.RawURLEncoding.EncodeToString(randomBytes(n)) }

var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// newDeviceID returns "d_" + 10 lowercase base32 chars (BUILD-SPEC §4).
func newDeviceID() string { return "d_" + idEncoding.EncodeToString(randomBytes(7))[:10] }

// Pairing codes use Crockford base32 (no I, L, O, U) so they can be read
// aloud or typed: 12 chars = 60 bits, shown as XXXX-XXXX-XXXX.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func newPairCode() string {
	b := randomBytes(12)
	out := make([]byte, 12)
	for i, c := range b {
		out[i] = crockford[int(c)&31]
	}
	return string(out[:4]) + "-" + string(out[4:8]) + "-" + string(out[8:])
}

// normalizeCode accepts what a person might type: any case, spaces,
// dashes, and the usual look-alikes.
func normalizeCode(c string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(c) {
		switch r {
		case '-', ' ', '\t':
			continue
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		b.WriteRune(r)
	}
	return b.String()
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMilli(v.Int64).UTC()
	return &t
}

// deviceRow is a device with its secret hash (never sent to clients).
type deviceRow struct {
	api.Device
	tokenHash []byte
}

const deviceCols = `d.id, d.name, d.public_key, d.token_hash, d.created_at, d.last_seen_at, d.revoked_at,
	EXISTS (SELECT 1 FROM web_push_subs p WHERE p.device_id = d.id),
	EXISTS (SELECT 1 FROM web_passkeys k WHERE k.device_id = d.id)`

func scanDevice(sc interface{ Scan(...any) error }) (*deviceRow, error) {
	var d deviceRow
	var created int64
	var seen, revoked sql.NullInt64
	if err := sc.Scan(&d.ID, &d.Name, &d.PublicKey, &d.tokenHash, &created, &seen, &revoked, &d.PushOK, &d.Passkey); err != nil {
		return nil, err
	}
	d.CreatedAt = time.UnixMilli(created).UTC()
	d.LastSeenAt = fromMS(seen)
	d.RevokedAt = fromMS(revoked)
	return &d, nil
}

func (s *Server) device(ctx context.Context, id string) (*deviceRow, error) {
	row := s.d.DB.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM web_devices d WHERE d.id = ?`, id)
	d, err := scanDevice(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNoDevice
	}
	return d, err
}

var errNoDevice = errors.New("no such device")

func (s *Server) listDevices(ctx context.Context) ([]api.Device, error) {
	rows, err := s.d.DB.QueryContext(ctx, `SELECT `+deviceCols+` FROM web_devices d WHERE d.revoked_at IS NULL ORDER BY d.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Device{}
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d.Device)
	}
	return out, rows.Err()
}

// createCode stores a hashed one-time code of the given kind ("pair" or
// "login") and returns it in display form.
func (s *Server) createCode(ctx context.Context, kind string, ttl time.Duration) (string, time.Time, error) {
	s.maybePrune(ctx)
	code := newPairCode()
	now := s.now()
	exp := now.Add(ttl)
	_, err := s.d.DB.ExecContext(ctx, `INSERT INTO web_pair_codes (code_hash, kind, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		hashSecret(normalizeCode(code)), kind, ms(now), ms(exp))
	return code, exp, err
}

// consumeCode marks a code used, atomically; it reports false if the code
// is unknown, used or expired.
func (s *Server) consumeCode(ctx context.Context, code string) (kind string, ok bool, err error) {
	now := ms(s.now())
	err = s.d.DB.QueryRowContext(ctx,
		`UPDATE web_pair_codes SET used_at = ? WHERE code_hash = ? AND used_at IS NULL AND expires_at > ? RETURNING kind`,
		now, hashSecret(normalizeCode(code)), now).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return kind, err == nil, err
}

// useNonce records a decision nonce for a device; false means replay.
func (s *Server) useNonce(ctx context.Context, deviceID, nonce string, exp time.Time) (bool, error) {
	s.maybePrune(ctx)
	res, err := s.d.DB.ExecContext(ctx, `INSERT OR IGNORE INTO web_nonces (device_id, nonce, expires_at) VALUES (?, ?, ?)`, deviceID, nonce, ms(exp))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// maybePrune deletes expired codes, nonces and tokens at most every 10
// minutes. Nonces are kept until well past the ±120 s window, so pruning
// never reopens a replay.
func (s *Server) maybePrune(ctx context.Context) {
	s.mu.Lock()
	now := s.now()
	if now.Sub(s.pruned) < 10*time.Minute {
		s.mu.Unlock()
		return
	}
	s.pruned = now
	s.mu.Unlock()
	cut := ms(now)
	for _, q := range []string{
		`DELETE FROM web_pair_codes WHERE expires_at < ?`,
		`DELETE FROM web_nonces WHERE expires_at < ?`,
		`DELETE FROM web_action_tokens WHERE expires_at < ?`,
	} {
		if _, err := s.d.DB.ExecContext(ctx, q, cut); err != nil {
			s.log.Warn("prune", "err", err)
		}
	}
}
