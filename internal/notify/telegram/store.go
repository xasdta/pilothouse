package telegram

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// migrations are numbered and applied once each, recorded in
// telegram_migrations (like internal/web's web_* tables, and apart from
// the store's own schema version). Times are Unix milliseconds.
var migrations = []string{
	1: `
CREATE TABLE IF NOT EXISTS telegram_messages (
	chat_id    INTEGER NOT NULL,
	message_id INTEGER NOT NULL,
	kind       TEXT NOT NULL,         -- request | session | feedback
	request_id TEXT NOT NULL DEFAULT '',
	session_id TEXT NOT NULL DEFAULT '',
	text       TEXT NOT NULL DEFAULT '', -- rendered body, kept for the edit on close
	state      TEXT NOT NULL DEFAULT '', -- JSON answer progress (questions)
	created_at INTEGER NOT NULL,
	closed_at  INTEGER,
	PRIMARY KEY (chat_id, message_id)
);
CREATE INDEX IF NOT EXISTS telegram_messages_request ON telegram_messages(request_id);
CREATE TABLE IF NOT EXISTS telegram_tokens (
	token_hash BLOB PRIMARY KEY,
	chat_id    INTEGER NOT NULL,
	message_id INTEGER NOT NULL DEFAULT 0,
	request_id TEXT NOT NULL,
	action     TEXT NOT NULL,
	expires_at INTEGER NOT NULL,
	used_at    INTEGER
);
CREATE INDEX IF NOT EXISTS telegram_tokens_request ON telegram_tokens(request_id);
CREATE INDEX IF NOT EXISTS telegram_tokens_message ON telegram_tokens(chat_id, message_id);
CREATE TABLE IF NOT EXISTS telegram_state (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`,
}

// Migrate creates or updates the telegram_* tables.
func Migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS telegram_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("telegram: migrate: %w", err)
	}
	for v := 1; v < len(migrations); v++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("telegram: migrate: %w", err)
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM telegram_migrations WHERE version = ?`, v).Scan(&n); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("telegram: migrate: %w", err)
		}
		if n > 0 {
			_ = tx.Rollback()
			continue
		}
		if _, err := tx.ExecContext(ctx, migrations[v]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("telegram: migration %d: %w", v, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO telegram_migrations (version, applied_at) VALUES (?, ?)`, v, time.Now().UnixMilli()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("telegram: migration %d: %w", v, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("telegram: migration %d: %w", v, err)
		}
	}
	return nil
}

// Message kinds.
const (
	kindRequest  = "request"  // a request with buttons
	kindSession  = "session"  // "finished its turn": a reply prompts the agent
	kindFeedback = "feedback" // ForceReply prompt for plan changes
)

// msgRow is a message the bot sent and may edit or route replies from.
type msgRow struct {
	ChatID, MessageID int64
	Kind              string
	RequestID         string
	SessionID         string
	Text              string
	State             string
	CreatedAt         time.Time
	Closed            bool
}

type db struct {
	sql *sql.DB
	now func() time.Time
}

func (d *db) saveMessage(ctx context.Context, m msgRow) error {
	_, err := d.sql.ExecContext(ctx, `
INSERT INTO telegram_messages (chat_id, message_id, kind, request_id, session_id, text, state, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(chat_id, message_id) DO UPDATE SET kind = excluded.kind, request_id = excluded.request_id, session_id = excluded.session_id,
	text = excluded.text, state = excluded.state`,
		m.ChatID, m.MessageID, m.Kind, m.RequestID, m.SessionID, m.Text, m.State, d.now().UnixMilli())
	return err
}

const msgCols = `chat_id, message_id, kind, request_id, session_id, text, state, created_at, closed_at IS NOT NULL`

func scanMsg(sc interface{ Scan(...any) error }) (msgRow, error) {
	var m msgRow
	var created int64
	err := sc.Scan(&m.ChatID, &m.MessageID, &m.Kind, &m.RequestID, &m.SessionID, &m.Text, &m.State, &created, &m.Closed)
	m.CreatedAt = time.UnixMilli(created)
	return m, err
}

// message returns a stored message, or ok=false.
func (d *db) message(ctx context.Context, chat, id int64) (msgRow, bool, error) {
	m, err := scanMsg(d.sql.QueryRowContext(ctx, `SELECT `+msgCols+` FROM telegram_messages WHERE chat_id = ? AND message_id = ?`, chat, id))
	if errors.Is(err, sql.ErrNoRows) {
		return msgRow{}, false, nil
	}
	return m, err == nil, err
}

// requestMessages returns the open request messages for a request.
func (d *db) requestMessages(ctx context.Context, requestID string) ([]msgRow, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+msgCols+` FROM telegram_messages WHERE request_id = ? AND kind = ? AND closed_at IS NULL`, requestID, kindRequest)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []msgRow
	for rows.Next() {
		m, err := scanMsg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// hasRequestMessage reports whether a request was ever sent with buttons.
func (d *db) hasRequestMessage(ctx context.Context, requestID string) (bool, error) {
	var n int
	err := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM telegram_messages WHERE request_id = ? AND kind = ?`, requestID, kindRequest).Scan(&n)
	return n > 0, err
}

func (d *db) setState(ctx context.Context, chat, id int64, state string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE telegram_messages SET state = ? WHERE chat_id = ? AND message_id = ?`, state, chat, id)
	return err
}

// closeRequest marks every message of a request closed (the request
// message and any feedback prompt) and spends its buttons.
func (d *db) closeRequest(ctx context.Context, requestID string) error {
	now := d.now().UnixMilli()
	if _, err := d.sql.ExecContext(ctx, `UPDATE telegram_messages SET closed_at = ? WHERE request_id = ? AND closed_at IS NULL`, now, requestID); err != nil {
		return err
	}
	_, err := d.sql.ExecContext(ctx, `UPDATE telegram_tokens SET used_at = ? WHERE request_id = ? AND used_at IS NULL`, now, requestID)
	return err
}

// ---- button tokens ----

// tokenPrefix marks Pilothouse callback data: "t:" + 22 base64url chars,
// well inside Telegram's 64-byte limit.
const tokenPrefix = "t:"

func hashToken(tok string) []byte {
	h := sha256.Sum256([]byte(tok))
	return h[:]
}

// mint stores a single-use token for one button: bound to the chat, the
// request and the action, valid until exp. The message id is filled in by
// bind once the message exists.
func (d *db) mint(ctx context.Context, chat int64, requestID, action string, exp time.Time) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	_, err := d.sql.ExecContext(ctx, `INSERT INTO telegram_tokens (token_hash, chat_id, request_id, action, expires_at) VALUES (?, ?, ?, ?, ?)`,
		hashToken(tok), chat, requestID, action, exp.UnixMilli())
	return tokenPrefix + tok, err
}

// bind ties freshly minted tokens to the message that carries them.
func (d *db) bind(ctx context.Context, data []string, chat, messageID int64) error {
	for _, cd := range data {
		tok, ok := cutToken(cd)
		if !ok {
			continue
		}
		if _, err := d.sql.ExecContext(ctx, `UPDATE telegram_tokens SET message_id = ? WHERE token_hash = ? AND chat_id = ?`, messageID, hashToken(tok), chat); err != nil {
			return err
		}
	}
	return nil
}

// discard deletes tokens whose message was never sent.
func (d *db) discard(ctx context.Context, data []string) {
	for _, cd := range data {
		if tok, ok := cutToken(cd); ok {
			_, _ = d.sql.ExecContext(ctx, `DELETE FROM telegram_tokens WHERE token_hash = ?`, hashToken(tok))
		}
	}
}

func cutToken(data string) (string, bool) {
	if len(data) < len(tokenPrefix)+16 || len(data) > 64 || data[:len(tokenPrefix)] != tokenPrefix {
		return "", false
	}
	return data[len(tokenPrefix):], true
}

// tokenResult says why a button press was accepted or refused.
type tokenResult int

const (
	tokenOK tokenResult = iota
	tokenUnknown
	tokenUsed
	tokenExpired
)

// spend uses a token pressed in (chat, messageID): it must exist for that
// chat and message, be unused and unexpired. Pressing any button of a
// keyboard spends all of them, so a stale keyboard can never act twice;
// the bot sends a fresh keyboard when the request is still open.
func (d *db) spend(ctx context.Context, data string, chat, messageID int64) (requestID, action string, res tokenResult, err error) {
	tok, ok := cutToken(data)
	if !ok {
		return "", "", tokenUnknown, nil
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return "", "", tokenUnknown, err
	}
	defer func() { _ = tx.Rollback() }()
	var exp int64
	var used sql.NullInt64
	var msg int64
	err = tx.QueryRowContext(ctx, `SELECT request_id, action, expires_at, used_at, message_id FROM telegram_tokens WHERE token_hash = ? AND chat_id = ?`,
		hashToken(tok), chat).Scan(&requestID, &action, &exp, &used, &msg)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", "", tokenUnknown, nil
	case err != nil:
		return "", "", tokenUnknown, err
	case msg != messageID:
		return "", "", tokenUnknown, nil
	case used.Valid:
		return requestID, action, tokenUsed, nil
	case exp <= d.now().UnixMilli():
		return requestID, action, tokenExpired, nil
	}
	now := d.now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `UPDATE telegram_tokens SET used_at = ? WHERE chat_id = ? AND message_id = ? AND used_at IS NULL`, now, chat, messageID); err != nil {
		return "", "", tokenUnknown, err
	}
	if err := tx.Commit(); err != nil {
		return "", "", tokenUnknown, err
	}
	return requestID, action, tokenOK, nil
}

// ---- poll offset ----

func (d *db) offset(ctx context.Context) int64 {
	var v string
	if err := d.sql.QueryRowContext(ctx, `SELECT value FROM telegram_state WHERE key = 'offset'`).Scan(&v); err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

func (d *db) setOffset(ctx context.Context, off int64) error {
	_, err := d.sql.ExecContext(ctx, `INSERT INTO telegram_state (key, value) VALUES ('offset', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		strconv.FormatInt(off, 10))
	return err
}

// prune drops spent or expired tokens after a day and messages after two
// weeks (replies to older messages get a "too old" answer).
func (d *db) prune(ctx context.Context) error {
	now := d.now()
	if _, err := d.sql.ExecContext(ctx, `DELETE FROM telegram_tokens WHERE expires_at < ? OR used_at < ?`,
		now.Add(-24*time.Hour).UnixMilli(), now.Add(-24*time.Hour).UnixMilli()); err != nil {
		return err
	}
	_, err := d.sql.ExecContext(ctx, `DELETE FROM telegram_messages WHERE created_at < ?`, now.Add(-14*24*time.Hour).UnixMilli())
	return err
}
