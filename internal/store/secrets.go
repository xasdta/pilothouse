package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---- session secrets ----
//
// session_secrets holds private per-session values — today Claude Code's
// messaging-socket endpoint (path and token). Nothing here is part of
// api.Session, events, evidence or any API response; rows go away with
// their session (ON DELETE CASCADE) or when the daemon deletes them.

// SecretMessaging is the kind of a session's messaging endpoint (JSON).
const SecretMessaging = "messaging"

// PutSecret stores (or replaces) a session's secret of kind.
func (tx *Tx) PutSecret(ctx context.Context, sessionID, kind, value string) error {
	_, err := tx.q.ExecContext(ctx, `
INSERT INTO session_secrets(session_id, kind, value, updated_at) VALUES(?, ?, ?, ?)
ON CONFLICT(session_id, kind) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		sessionID, kind, value, nanos(tx.Now()))
	if err != nil {
		// The value is never part of an error message.
		return fmt.Errorf("store %s secret for session %s: %w", kind, sessionID, err)
	}
	return nil
}

// Secret returns a session's secret of kind, or ErrNotFound.
func (r Reader) Secret(ctx context.Context, sessionID, kind string) (string, error) {
	var v string
	err := r.q.QueryRowContext(ctx, `SELECT value FROM session_secrets WHERE session_id = ? AND kind = ?`,
		sessionID, kind).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read %s secret for session %s: %w", kind, sessionID, err)
	}
	return v, nil
}

// SecretSessionsEnded lists sessions that have exited but still have
// secrets.
func (r Reader) SecretSessionsEnded(ctx context.Context) ([]string, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT DISTINCT k.session_id FROM session_secrets k
		JOIN sessions s ON s.id = k.session_id WHERE s.exited_at IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("list ended sessions with secrets: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list ended sessions with secrets: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// DeleteSecret removes a session's secret of kind; kind "" removes all of
// the session's secrets. Deleting what isn't there is not an error.
func (tx *Tx) DeleteSecret(ctx context.Context, sessionID, kind string) error {
	q, args := `DELETE FROM session_secrets WHERE session_id = ?`, []any{sessionID}
	if kind != "" {
		q += ` AND kind = ?`
		args = append(args, kind)
	}
	if _, err := tx.q.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("delete secrets of session %s: %w", sessionID, err)
	}
	return nil
}
