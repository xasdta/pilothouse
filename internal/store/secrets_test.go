package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

func TestSessionSecrets(t *testing.T) {
	db := openTest(t)
	now := time.Now().UTC().Round(0)
	putSession(t, db, api.Session{ID: "s_one", Agent: api.AgentClaude, UpdatedAt: now})
	putSession(t, db, api.Session{ID: "s_two", Agent: api.AgentClaude, UpdatedAt: now})

	if _, err := db.Secret(ctx, "s_one", SecretMessaging); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing secret: %v", err)
	}
	write := func(fn func(tx *Tx) error) {
		t.Helper()
		if err := db.Write(ctx, fn); err != nil {
			t.Fatal(err)
		}
	}
	write(func(tx *Tx) error { return tx.PutSecret(ctx, "s_one", SecretMessaging, "v1") })
	write(func(tx *Tx) error { return tx.PutSecret(ctx, "s_one", SecretMessaging, "v2") }) // replaces
	write(func(tx *Tx) error { return tx.PutSecret(ctx, "s_one", "other", "o") })
	write(func(tx *Tx) error { return tx.PutSecret(ctx, "s_two", SecretMessaging, "w") })
	if v, err := db.Secret(ctx, "s_one", SecretMessaging); err != nil || v != "v2" {
		t.Fatalf("secret = %q %v", v, err)
	}
	// A secret needs its session.
	err := db.Write(ctx, func(tx *Tx) error { return tx.PutSecret(ctx, "s_nope", SecretMessaging, "SECRETVALUE") })
	if err == nil || strings.Contains(err.Error(), "SECRETVALUE") {
		t.Fatalf("secret for a missing session: %v", err)
	}

	// Secrets never show up in the session row or its events.
	write(func(tx *Tx) error {
		s, err := tx.Session(ctx, "s_one")
		if err != nil {
			return err
		}
		_, err = tx.Emit(ctx, api.EventSessionUpdated, s.ID, "", s)
		return err
	})
	s, _ := db.Session(ctx, "s_one")
	b, _ := json.Marshal(s)
	evs, _ := db.SessionEvents(ctx, "s_one", 10)
	eb, _ := json.Marshal(evs)
	if strings.Contains(string(b)+string(eb), "v2") {
		t.Fatalf("secret leaked: %s %s", b, eb)
	}

	write(func(tx *Tx) error { return tx.DeleteSecret(ctx, "s_one", "other") })
	if _, err := db.Secret(ctx, "s_one", "other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted kind: %v", err)
	}
	if _, err := db.Secret(ctx, "s_one", SecretMessaging); err != nil {
		t.Fatalf("other kind deleted too: %v", err)
	}
	write(func(tx *Tx) error { return tx.DeleteSecret(ctx, "s_one", "") })
	write(func(tx *Tx) error { return tx.DeleteSecret(ctx, "s_one", "") }) // idempotent
	if _, err := db.Secret(ctx, "s_one", SecretMessaging); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete all: %v", err)
	}

	// Rows go with their session.
	write(func(tx *Tx) error {
		_, err := tx.q.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, "s_two")
		return err
	})
	var n int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM session_secrets WHERE session_id = 's_two'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("cascade: %d rows, %v", n, err)
	}
}
