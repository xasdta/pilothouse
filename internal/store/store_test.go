package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "pilothouse.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

var ctx = context.Background()

func putSession(t *testing.T, db *DB, s api.Session) {
	t.Helper()
	if s.State == "" {
		s.State = api.StateStarting
	}
	if err := db.Write(ctx, func(tx *Tx) error { return tx.PutSession(ctx, s) }); err != nil {
		t.Fatal(err)
	}
}

func TestOpenIsIdempotentAndPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pilothouse.db")
	for i := 0; i < 2; i++ {
		db, err := Open(path)
		if err != nil {
			t.Fatalf("open #%d: %v", i, err)
		}
		v, ok, err := db.Meta(ctx, schemaKey)
		if err != nil || !ok || v != "1" {
			t.Fatalf("schema version = %q %v %v", v, ok, err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("db mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestSessionsAndPrefixLookup(t *testing.T) {
	db := openTest(t)
	now := time.Now().UTC().Round(0)
	exited := now.Add(-48 * time.Hour)
	putSession(t, db, api.Session{ID: "s_abc111", Agent: api.AgentClaude, AgentSessionID: "v1", PID: 42, UpdatedAt: now})
	putSession(t, db, api.Session{ID: "s_abc222", Agent: api.AgentClaude, UpdatedAt: now.Add(-time.Minute)})
	putSession(t, db, api.Session{ID: "s_old", Agent: api.AgentCodex, ExitedAt: &exited, State: api.StateExited, UpdatedAt: exited})

	tests := []struct {
		prefix  string
		want    string
		wantErr error
	}{
		{"s_abc111", "s_abc111", nil},
		{"s_abc1", "s_abc111", nil},
		{"abc2", "s_abc222", nil},
		{"s_abc", "", ErrAmbiguous},
		{"s_zzz", "", ErrNotFound},
		{"", "", ErrNotFound},
		{"_", "", ErrNotFound}, // '_' must not act as a wildcard
	}
	for _, tt := range tests {
		got, err := db.ResolveSessionID(ctx, tt.prefix)
		if !errors.Is(err, tt.wantErr) || got != tt.want {
			t.Errorf("ResolveSessionID(%q) = %q, %v; want %q, %v", tt.prefix, got, err, tt.want, tt.wantErr)
		}
	}

	s, err := db.SessionByAgentID(ctx, api.AgentClaude, "v1")
	if err != nil || s.ID != "s_abc111" {
		t.Fatalf("SessionByAgentID = %+v, %v", s, err)
	}
	if s, err := db.LiveSessionByPID(ctx, api.AgentClaude, 42); err != nil || s.ID != "s_abc111" {
		t.Fatalf("LiveSessionByPID = %+v, %v", s, err)
	}
	live, err := db.LiveSessions(ctx)
	if err != nil || len(live) != 2 {
		t.Fatalf("LiveSessions = %d, %v", len(live), err)
	}
	recent, err := db.Sessions(ctx, now.Add(-24*time.Hour))
	if err != nil || len(recent) != 2 {
		t.Fatalf("Sessions(24h) = %d, %v", len(recent), err)
	}
	all, err := db.Sessions(ctx, time.Time{})
	if err != nil || len(all) != 3 {
		t.Fatalf("Sessions(all) = %d, %v", len(all), err)
	}
}

func TestCloseRequestFirstAnswerWins(t *testing.T) {
	db := openTest(t)
	now := time.Now().UTC().Round(0)
	putSession(t, db, api.Session{ID: "s_1", Agent: api.AgentClaude, UpdatedAt: now})
	req := api.Request{ID: "r_1", SessionID: "s_1", Checkpoint: api.CheckpointPermission, Tool: "Bash",
		Summary: "Run: ls", Status: api.RequestOpen, CreatedAt: now}
	if err := db.Write(ctx, func(tx *Tx) error { return tx.InsertRequest(ctx, req, "tu1") }); err != nil {
		t.Fatal(err)
	}

	var wins, losses atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := db.Write(ctx, func(tx *Tx) error {
				_, err := tx.CloseRequest(ctx, "r_1", api.RequestDecided,
					&api.Decision{RequestID: "r_1", Behavior: api.BehaviorAllow, By: "cli", At: now}, now)
				return err
			})
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, ErrAlreadyClosed):
				losses.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 || losses.Load() != 19 {
		t.Fatalf("wins=%d losses=%d", wins.Load(), losses.Load())
	}
	got, err := db.Request(ctx, "r_1")
	if err != nil || got.Status != api.RequestDecided || got.Decision == nil || got.ClosedAt == nil {
		t.Fatalf("request after close = %+v, %v", got, err)
	}
}

func TestMatchPending(t *testing.T) {
	db := openTest(t)
	now := time.Now().UTC().Round(0)
	putSession(t, db, api.Session{ID: "s_1", Agent: api.AgentClaude, UpdatedAt: now})
	mk := func(id, tuid string, age time.Duration) {
		r := api.Request{ID: id, SessionID: "s_1", Checkpoint: api.CheckpointPermission, Tool: "Bash",
			Summary: "Run: ls", Status: api.RequestSteppedAside, CreatedAt: now.Add(-age)}
		if err := db.Write(ctx, func(tx *Tx) error { return tx.InsertRequest(ctx, r, tuid) }); err != nil {
			t.Fatal(err)
		}
	}
	mk("r_a", "tu-a", 2*time.Second)
	mk("r_b", "tu-b", time.Second)

	tests := []struct{ tuid, want string }{
		{"tu-a", "r_a"},     // exact tool_use_id beats recency
		{"", "r_b"},         // no id: newest with same tool+summary
		{"tu-other", "r_b"}, // unknown id falls back too
	}
	for _, tt := range tests {
		got, err := db.MatchPending(ctx, "s_1", tt.tuid, "Bash", "Run: ls")
		if err != nil || got.ID != tt.want {
			t.Errorf("MatchPending(%q) = %s, %v; want %s", tt.tuid, got.ID, err, tt.want)
		}
	}
	if _, err := db.MatchPending(ctx, "s_1", "", "Edit", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("no match err = %v", err)
	}
	// A request held for devices (open) matches too; closed ones never do.
	r := api.Request{ID: "r_held", SessionID: "s_1", Checkpoint: api.CheckpointPermission, Tool: "Bash",
		Summary: "Run: rm", Status: api.RequestOpen, CreatedAt: now}
	if err := db.Write(ctx, func(tx *Tx) error { return tx.InsertRequest(ctx, r, "tu-h") }); err != nil {
		t.Fatal(err)
	}
	if got, err := db.MatchPending(ctx, "s_1", "tu-h", "Bash", "Run: rm"); err != nil || got.ID != "r_held" {
		t.Errorf("held request: %s, %v", got.ID, err)
	}
	if err := db.Write(ctx, func(tx *Tx) error {
		_, err := tx.CloseRequest(ctx, "r_held", api.RequestDecided, nil, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.MatchPending(ctx, "s_1", "tu-h", "Bash", "Run: rm"); !errors.Is(err, ErrNotFound) {
		t.Errorf("closed request matched: %v", err)
	}
}

func TestOutboxPublishesOnlyCommittedEventsInOrder(t *testing.T) {
	db := openTest(t)
	var mu sync.Mutex
	var got []int64
	db.SetOnCommit(func(evs []api.Event) {
		mu.Lock()
		defer mu.Unlock()
		for _, e := range evs {
			got = append(got, e.Seq)
		}
	})

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = db.Write(ctx, func(tx *Tx) error {
				if _, err := tx.Emit(ctx, api.EventPresenceChanged, "", "", map[string]int{"n": 1}); err != nil {
					return err
				}
				_, err := tx.Emit(ctx, api.EventPresenceChanged, "", "", nil)
				return err
			})
		}()
	}
	wg.Wait()

	boom := errors.New("boom")
	if err := db.Write(ctx, func(tx *Tx) error {
		if _, err := tx.Emit(ctx, api.EventHook, "", "", nil); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 20 {
		t.Fatalf("published %d events, want 20", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("published out of order: %v", got)
		}
	}
	evs, err := db.Events(ctx, 0, 0)
	if err != nil || len(evs) != 20 {
		t.Fatalf("logged %d events, %v", len(evs), err)
	}
	if string(evs[0].Data) != `{"n":1}` || evs[1].Data != nil {
		t.Fatalf("event data = %s / %s", evs[0].Data, evs[1].Data)
	}
	// The rolled-back transaction also rolls back sqlite_sequence.
	if max, _ := db.MaxSeq(ctx); max != got[len(got)-1] {
		t.Fatalf("MaxSeq = %d, last published %d", max, got[len(got)-1])
	}
}

func TestPrune(t *testing.T) {
	db := openTest(t)
	old := time.Now().Add(-40 * 24 * time.Hour).UTC().Round(0)
	recent := time.Now().UTC().Round(0)
	clock := old
	db.SetClock(func() time.Time { return clock })

	putSession(t, db, api.Session{ID: "s_1", Agent: api.AgentClaude, UpdatedAt: old})
	err := db.Write(ctx, func(tx *Tx) error {
		closed := api.Request{ID: "r_old", SessionID: "s_1", Checkpoint: api.CheckpointPermission,
			Status: api.RequestDecided, CreatedAt: old, ClosedAt: &old}
		pending := api.Request{ID: "r_pending", SessionID: "s_1", Checkpoint: api.CheckpointPermission,
			Status: api.RequestSteppedAside, CreatedAt: old}
		for _, r := range []api.Request{closed, pending} {
			if err := tx.InsertRequest(ctx, r, ""); err != nil {
				return err
			}
		}
		for _, e := range []api.StateEvidence{
			{At: old, Layer: api.LayerHook, State: api.StateWorking},
			{At: old, Layer: api.LayerHook, State: api.StateWaiting},
		} {
			if err := tx.AddEvidence(ctx, "s_1", e); err != nil {
				return err
			}
		}
		if _, err := tx.Emit(ctx, api.EventHook, "s_1", "", nil); err != nil {
			return err
		}
		_, err := tx.Emit(ctx, api.EventSessionStarted, "s_1", "", nil)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	clock = recent
	if err := db.Write(ctx, func(tx *Tx) error {
		_, err := tx.Emit(ctx, api.EventHook, "s_1", "", nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	var res PruneResult
	if err := db.Write(ctx, func(tx *Tx) error {
		var err error
		res, err = tx.Prune(ctx, recent.Add(-30*24*time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if res != (PruneResult{Requests: 1, Events: 1, Evidence: 1}) {
		t.Fatalf("prune = %+v", res)
	}
	if _, err := db.Request(ctx, "r_pending"); err != nil {
		t.Fatalf("pending request pruned: %v", err)
	}
	ev, _ := db.Evidence(ctx, "s_1", 10)
	if len(ev) != 1 || ev[0].State != api.StateWaiting {
		t.Fatalf("evidence after prune = %+v", ev)
	}
	if ps, _ := db.PrunedSeq(ctx); ps != 1 {
		t.Fatalf("pruned seq = %d, want 1", ps)
	}
	evs, _ := db.Events(ctx, 0, 0)
	if len(evs) != 2 || evs[0].Type != api.EventSessionStarted {
		t.Fatalf("events after prune = %+v", evs)
	}
}

func TestGrantsTasksMetaAndRead(t *testing.T) {
	db := openTest(t)
	now := time.Now().UTC().Round(0)
	putSession(t, db, api.Session{ID: "s_1", Agent: api.AgentClaude, UpdatedAt: now})
	err := db.Write(ctx, func(tx *Tx) error {
		if err := tx.InsertGrant(ctx, api.Grant{ID: "g_1", SessionID: "s_1", Tool: "Bash", Prefix: "git push", DecisionBy: "cli", CreatedAt: now}); err != nil {
			return err
		}
		if err := tx.PutTask(ctx, api.Task{ID: "t_1", Title: "x", Status: api.TaskRunning, UpdatedAt: now}); err != nil {
			return err
		}
		return tx.SetMeta(ctx, "k", "v")
	})
	if err != nil {
		t.Fatal(err)
	}
	gs, err := db.Grants(ctx, "s_1")
	if err != nil || len(gs) != 1 || gs[0].Prefix != "git push" {
		t.Fatalf("grants = %+v, %v", gs, err)
	}
	if id, err := db.ResolveTaskID(ctx, "1"); err != nil || id != "t_1" {
		t.Fatalf("task id = %q, %v", id, err)
	}
	if v, ok, _ := db.Meta(ctx, "k"); !ok || v != "v" {
		t.Fatalf("meta = %q %v", v, ok)
	}
	err = db.Read(ctx, func(r Reader) error {
		ts, err := r.Tasks(ctx, now.Add(time.Hour)) // running tasks are always included
		if err != nil || len(ts) != 1 {
			t.Errorf("tasks = %+v, %v", ts, err)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func pragma(t *testing.T, db *DB, name string) int64 {
	t.Helper()
	var v int64
	if err := db.w.QueryRow(`PRAGMA ` + name).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// fillAndPrune writes n large hook events 40 days old, then prunes them.
func fillAndPrune(t *testing.T, db *DB, n int) PruneResult {
	t.Helper()
	old := time.Now().Add(-40 * 24 * time.Hour).UTC().Round(0)
	db.SetClock(func() time.Time { return old })
	payload := map[string]string{"input": strings.Repeat("x", 2048)}
	if err := db.Write(ctx, func(tx *Tx) error {
		for i := 0; i < n; i++ {
			if _, err := tx.Emit(ctx, api.EventHook, "", "", payload); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	db.SetClock(time.Now)
	var res PruneResult
	if err := db.Write(ctx, func(tx *Tx) error {
		var err error
		res, err = tx.Prune(ctx, time.Now().Add(-30*24*time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return res
}

// TestCompactAfterPrune is the QA L8 regression: pruning must give space
// back, not just free pages inside the file and leave a large WAL.
func TestCompactAfterPrune(t *testing.T) {
	db := openTest(t)
	if pragma(t, db, "auto_vacuum") != 2 {
		t.Fatalf("new database auto_vacuum = %d, want 2 (incremental)", pragma(t, db, "auto_vacuum"))
	}
	if res := fillAndPrune(t, db, 2000); res.Events != 2000 {
		t.Fatalf("pruned %+v", res)
	}
	var n int
	if err := db.r.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("events left: %d, %v", n, err)
	}
	cr, err := db.Compact(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cr.FreedPages == 0 || pragma(t, db, "freelist_count") != 0 {
		t.Fatalf("compact = %+v, freelist now %d", cr, pragma(t, db, "freelist_count"))
	}
	if st, err := os.Stat(db.Path() + "-wal"); err == nil && st.Size() != 0 {
		t.Fatalf("WAL not truncated: %d bytes", st.Size())
	}
}

// TestCompactConvertsLegacyDatabase: a database created before
// incremental auto-vacuum gets one full VACUUM once mostly free.
func TestCompactConvertsLegacyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE legacy(x)`); err != nil { // a table first: auto_vacuum stays NONE
		t.Fatal(err)
	}
	_ = raw.Close()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if pragma(t, db, "auto_vacuum") != 0 {
		t.Skip("sqlite enabled auto_vacuum on an existing database")
	}
	fillAndPrune(t, db, 2000)
	cr, err := db.Compact(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !cr.Vacuumed || pragma(t, db, "auto_vacuum") != 2 || pragma(t, db, "freelist_count") != 0 {
		t.Fatalf("legacy compact = %+v, auto_vacuum %d", cr, pragma(t, db, "auto_vacuum"))
	}
	// Converted: the next compaction is incremental, never a full VACUUM.
	fillAndPrune(t, db, 500)
	if cr, err := db.Compact(ctx); err != nil || cr.Vacuumed || cr.FreedPages == 0 {
		t.Fatalf("second compact = %+v, %v", cr, err)
	}
}
