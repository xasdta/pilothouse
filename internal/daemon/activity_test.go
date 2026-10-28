package daemon

// Workstream C: activity feeds, the usage loop, budget alerts and
// insights, through the local API of a real in-process daemon. Helpers are
// prefixed "act" so they never collide with other workstreams' tests.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/store"
)

// actPutSession stores a session as hooks would have created it.
func actPutSession(t *testing.T, h *harness, s api.Session) api.Session {
	t.Helper()
	now := time.Now().UTC().Round(0)
	if s.StartedAt.IsZero() {
		s.StartedAt = now
	}
	s.UpdatedAt, s.State, s.Mode = now, api.StateWorking, api.ModeObserved
	var out api.Session
	if err := h.d.db.Write(bg, func(tx *store.Tx) error {
		var err error
		out, err = h.d.saveSession(bg, tx, nil, s)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// actClaudeFile is where Claude Code keeps a transcript under the test home.
func actClaudeFile(t *testing.T, sid string) string {
	t.Helper()
	p := filepath.Join(os.Getenv("PILOTHOUSE_USER_HOME"), ".claude", "projects", "-src-app", sid+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func actAppend(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func actJSON(v map[string]any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

var actSeq int

func actPrompt(at time.Time, text, origin string) string {
	actSeq++
	return actJSON(map[string]any{"type": "user", "uuid": fmt.Sprintf("p-%d", actSeq), "timestamp": at.Format(time.RFC3339Nano),
		"message": map[string]any{"role": "user", "content": text}, "origin": map[string]any{"kind": origin}})
}

func actReply(at time.Time, msgID, model, text string, in, out int) string {
	actSeq++
	return actJSON(map[string]any{"type": "assistant", "uuid": fmt.Sprintf("a-%d", actSeq), "timestamp": at.Format(time.RFC3339Nano),
		"message": map[string]any{"id": msgID, "role": "assistant", "model": model,
			"content": []any{map[string]any{"type": "text", "text": text}},
			"usage":   map[string]any{"input_tokens": in, "output_tokens": out}}})
}

func actTool(at time.Time, msgID, id, name string, input map[string]any) string {
	actSeq++
	return actJSON(map[string]any{"type": "assistant", "uuid": fmt.Sprintf("a-%d", actSeq), "timestamp": at.Format(time.RFC3339Nano),
		"message": map[string]any{"id": msgID, "role": "assistant", "model": "claude-opus-5-5",
			"content": []any{map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}},
			"usage":   map[string]any{"input_tokens": 1, "output_tokens": 1}}})
}

func actResult(at time.Time, id, content string, isErr bool) string {
	actSeq++
	return actJSON(map[string]any{"type": "user", "uuid": fmt.Sprintf("r-%d", actSeq), "timestamp": at.Format(time.RFC3339Nano),
		"message": map[string]any{"role": "user", "content": []any{map[string]any{
			"type": "tool_result", "tool_use_id": id, "content": content, "is_error": isErr}}}})
}

func TestActivityThroughLocalAPI(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	path := actClaudeFile(t, "sess-a")
	now := time.Now().UTC()
	actAppend(t, path,
		`{"type":"permission-mode","permissionMode":"default"}`,
		actPrompt(now.Add(-time.Minute), "Fix the build", "human"),
		actTool(now.Add(-50*time.Second), "msg_1", "toolu_1", "Bash", map[string]any{"command": "make"}),
		actResult(now.Add(-40*time.Second), "toolu_1", "ok", false),
		actTool(now.Add(-30*time.Second), "msg_2", "toolu_2", "Edit", map[string]any{"file_path": "/src/app/main.go", "old_string": "a", "new_string": "b\nc"}),
		actResult(now.Add(-20*time.Second), "toolu_2", "updated", false),
		actReply(now.Add(-10*time.Second), "msg_3", "claude-opus-5-5", "Fixed.", 1000, 500),
	)
	s := actPutSession(t, h, api.Session{ID: "s_act1", Agent: api.AgentClaude, AgentSessionID: "sess-a",
		Cwd: "/src/app", TranscriptPath: path})
	if !s.Caps.Activity {
		t.Errorf("caps.activity = false for a readable transcript")
	}

	act, err := h.c.Activity(bg, "s_act1", api.ActivityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if !act.Available || act.SessionID != "s_act1" || len(act.Items) != 4 || act.Cursor == "" || act.Note != "" {
		t.Fatalf("activity = %+v", act)
	}
	if act.Usage == nil || act.Usage.InputTokens != 1002 || act.Usage.OutputTokens != 502 || act.Usage.CostUSD <= 0 || act.Usage.Model != "claude-opus-5-5" {
		t.Errorf("usage = %+v", act.Usage)
	}
	if len(act.Files) != 1 || act.Files[0].Path != "main.go" || act.Files[0].Added != 2 || act.Files[0].Removed != 1 {
		t.Errorf("files = %+v", act.Files)
	}
	if act.CatchUp == nil || !strings.Contains(strings.Join(act.CatchUp.Lines, "\n"), "Ran 2 tool calls") {
		t.Errorf("catch-up = %+v", act.CatchUp)
	}

	// The cursor returns only what's new.
	again, err := h.c.Activity(bg, "s_act1", api.ActivityQuery{Cursor: act.Cursor})
	if err != nil || len(again.Items) != 0 {
		t.Fatalf("cursor read = %+v %v", again, err)
	}
	actAppend(t, path, actPrompt(time.Now().UTC(), "Thanks", "human"))
	newer, err := h.c.Activity(bg, "s_act", api.ActivityQuery{Cursor: act.Cursor, Limit: 10}) // id prefix
	if err != nil || len(newer.Items) != 1 || newer.Items[0].Text != "Thanks" {
		t.Fatalf("after append = %+v %v", newer, err)
	}

	// The usage loop stores the totals with the session; session events
	// carry them.
	waitFor(t, "session usage", func() bool {
		s, err := h.c.Session(bg, "s_act1")
		return err == nil && s.Usage != nil && s.Usage.OutputTokens == 502
	})
	evs, err := h.c.SessionEvents(bg, "s_act1", 50)
	if err != nil {
		t.Fatal(err)
	}
	carried := false
	for _, ev := range evs {
		var es api.Session
		if ev.Type == api.EventSessionUpdated && json.Unmarshal(ev.Data, &es) == nil && es.Usage != nil {
			carried = true
		}
	}
	if !carried {
		t.Error("no session.updated event carries usage")
	}

	// Unknown session.
	if _, err := h.c.Activity(bg, "s_nope", api.ActivityQuery{}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("unknown session = %v", err)
	}
}

func TestActivityUnavailable(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	outside := filepath.Join(t.TempDir(), "evil.jsonl")
	actAppend(t, outside, actPrompt(time.Now(), "secret", "human"))
	cases := []struct {
		s    api.Session
		note string
	}{
		{api.Session{ID: "s_out", Agent: api.AgentClaude, TranscriptPath: outside}, "outside the agent's own folder"},
		{api.Session{ID: "s_etc", Agent: api.AgentClaude, TranscriptPath: "/etc/passwd"}, "outside the agent's own folder"},
		{api.Session{ID: "s_gen", Agent: api.AgentGeneric}, "can't read this agent's transcript"},
		{api.Session{ID: "s_none", Agent: api.AgentClaude}, "No transcript yet"},
		{api.Session{ID: "s_later", Agent: api.AgentClaude, TranscriptPath: actClaudeFile(t, "not-written-yet")}, "No transcript yet"},
	}
	for _, c := range cases {
		s := actPutSession(t, h, c.s)
		if s.Caps.Activity {
			t.Errorf("%s: caps.activity set", c.s.ID)
		}
		act, err := h.c.Activity(bg, c.s.ID, api.ActivityQuery{})
		if err != nil {
			t.Fatalf("%s: %v", c.s.ID, err)
		}
		if act.Available || !strings.Contains(act.Note, c.note) || act.Items == nil || len(act.Items) != 0 {
			t.Errorf("%s: activity = %+v, want note %q", c.s.ID, act, c.note)
		}
	}
}

// Codex sends transcript_path null until the rollout exists: the daemon
// finds the rollout by thread id, and caps follow.
func TestActivityFindsCodexRollout(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	thread := "019a2222-3333-7444-8555-666677778888"
	started := time.Now()
	s := actPutSession(t, h, api.Session{ID: "s_codex", Agent: api.AgentCodex, AgentSessionID: thread, StartedAt: started.UTC().Round(0)})
	if s.Caps.Activity {
		t.Fatal("caps.activity before the rollout exists")
	}
	dir := filepath.Join(os.Getenv("PILOTHOUSE_USER_HOME"), ".codex", "sessions", started.Format("2006/01/02"))
	rollout := filepath.Join(dir, "rollout-"+started.Format("2006-01-02T15-04-05")+"-"+thread+".jsonl")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ts := started.UTC().Format(time.RFC3339Nano)
	actAppend(t, rollout,
		`{"timestamp":"`+ts+`","type":"turn_context","payload":{"model":"gpt-6-sol"}}`,
		`{"timestamp":"`+ts+`","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello codex"}]}}`,
		`{"timestamp":"`+ts+`","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":10,"reasoning_output_tokens":0,"total_tokens":110},"last_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":10,"reasoning_output_tokens":0,"total_tokens":110}}}}`,
	)
	act, err := h.c.Activity(bg, "s_codex", api.ActivityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if !act.Available || len(act.Items) != 1 || act.Items[0].Text != "hello codex" || act.Usage == nil || act.Usage.Model != "gpt-6-sol" {
		t.Fatalf("activity = %+v", act)
	}
	waitFor(t, "caps.activity after discovery", func() bool {
		s, err := h.c.Session(bg, "s_codex")
		return err == nil && s.Caps.Activity && s.Usage != nil
	})
}

// Replies sent through Pilothouse show as such, not as typed prompts.
func TestActivityMarksPilothouseReplies(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	path := actClaudeFile(t, "sess-r")
	s := actPutSession(t, h, api.Session{ID: "s_rep", Agent: api.AgentClaude, TranscriptPath: path})
	sent := time.Now().UTC().Round(0)
	if _, err := h.d.Emit(bg, api.EventSessionReply, s.ID, "", api.ReplyRecord{SessionID: s.ID, Text: "Please also add tests",
		Via: api.ReplyMessage, By: "device:d_1", At: sent}); err != nil {
		t.Fatal(err)
	}
	actAppend(t, path,
		actPrompt(sent.Add(-time.Hour), "Please also add tests", "human"), // typed earlier, by the person
		actPrompt(sent.Add(time.Second), "Please also add tests", "peer"),
		actPrompt(sent.Add(2*time.Second), "a real peer message", "peer"),
	)
	act, err := h.c.Activity(bg, s.ID, api.ActivityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range act.Items {
		got = append(got, it.Source)
	}
	if strings.Join(got, ",") != "human,pilothouse,peer" {
		t.Errorf("sources = %q", got)
	}
}

// The catch-up starts where the person last opened the feed; polls with
// a cursor don't move it; open requests come first.
func TestActivityCatchUpSince(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	path := actClaudeFile(t, "sess-c")
	old := time.Now().UTC().Add(-time.Hour)
	actAppend(t, path, actPrompt(old, "old work", "human"),
		actTool(old, "msg_o", "toolu_o", "Bash", map[string]any{"command": "ls"}), actResult(old, "toolu_o", "x", false))
	s := actPutSession(t, h, api.Session{ID: "s_cu", Agent: api.AgentClaude, TranscriptPath: path})
	first, err := h.c.Activity(bg, s.ID, api.ActivityQuery{})
	if err != nil || !strings.Contains(strings.Join(first.CatchUp.Lines, "\n"), "Ran 1 tool call") || !first.CatchUp.Since.IsZero() {
		t.Fatalf("first look = %+v %v", first.CatchUp, err)
	}
	time.Sleep(10 * time.Millisecond)
	later := time.Now().UTC().Add(time.Second)
	actAppend(t, path, actTool(later, "msg_n", "toolu_n", "Bash", map[string]any{"command": "npm test"}),
		actResult(later, "toolu_n", "fail", true))
	if err := h.d.db.Write(bg, func(tx *store.Tx) error {
		now := tx.Now()
		return tx.InsertRequest(bg, api.Request{ID: "r_cu", SessionID: s.ID, Agent: api.AgentClaude, Checkpoint: api.CheckpointPermission,
			Tool: "Bash", Summary: "Run: git push", Status: api.RequestOpen, CreatedAt: now, ExpiresAt: now.Add(time.Minute)}, "")
	}); err != nil {
		t.Fatal(err)
	}
	poll, err := h.c.Activity(bg, s.ID, api.ActivityQuery{Cursor: first.Cursor})
	if err != nil || !poll.CatchUp.Since.IsZero() {
		t.Fatalf("a poll moved the catch-up start: %+v %v", poll.CatchUp, err)
	}
	second, err := h.c.Activity(bg, s.ID, api.ActivityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Join(second.CatchUp.Lines, "\n")
	if second.CatchUp.Since.IsZero() || !strings.HasPrefix(lines, "Waiting for you: Run: git push") ||
		!strings.Contains(lines, "Tests: npm test failed") || !strings.Contains(lines, "Needed your approval once") ||
		strings.Contains(lines, "Ran 2") {
		t.Errorf("second look = %q since %v", second.CatchUp.Lines, second.CatchUp.Since)
	}
	// An explicit since wins.
	all, err := h.c.Activity(bg, s.ID, api.ActivityQuery{Since: old.Add(-time.Minute)})
	if err != nil || !strings.Contains(strings.Join(all.CatchUp.Lines, "\n"), "Ran 2 tool calls") {
		t.Errorf("explicit since = %+v %v", all.CatchUp, err)
	}
}

func actNotes(h *harness, kind core.NotificationKind) []core.Notification {
	var out []core.Notification
	for _, n := range h.notes.all() {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}

// Budget alerts fire once per day at 80 % and 100 %, survive a restart,
// and insights report the spend.
func TestBudgetAlertsAndInsights(t *testing.T) {
	h := newHarness(t)
	h.cfg.Usage.DailyBudgetUSD = 10
	h.policy.set("rm -rf /", api.BehaviorDeny, "no-rm")
	h.start(t)
	path := actClaudeFile(t, "sess-b")
	now := time.Now().UTC()
	// Opus 5.5 output is $20 per million tokens: 450k tokens = $9 (90 %).
	actAppend(t, path, actPrompt(now, "big job", "human"), actReply(now, "msg_b1", "claude-opus-5-5", "part one", 0, 450_000))
	actPutSession(t, h, api.Session{ID: "s_bud", Agent: api.AgentClaude, TranscriptPath: path, RepoRoot: "/src/app"})
	waitFor(t, "80% alert", func() bool { return len(actNotes(h, notifyBudget)) == 1 })
	n := actNotes(h, notifyBudget)[0]
	if !n.Remote || !strings.Contains(n.Title, "80%") || !strings.Contains(n.Body, "$9.00 of your $10.00") {
		t.Errorf("80%% alert = %+v", n)
	}
	time.Sleep(5 * h.d.usageInterval())
	if got := len(actNotes(h, notifyBudget)); got != 1 {
		t.Fatalf("the 80%% alert repeated: %d", got)
	}
	actAppend(t, path, actReply(time.Now().UTC(), "msg_b2", "claude-opus-5-5", "part two", 0, 100_000)) // $11
	waitFor(t, "100% alert", func() bool { return len(actNotes(h, notifyBudget)) == 2 })
	if n := actNotes(h, notifyBudget)[1]; !strings.Contains(n.Title, "used up") || !strings.Contains(n.Body, "$11.00") {
		t.Errorf("100%% alert = %+v", n)
	}
	h.hook(bg, t, shell("rm -rf /")) // blocked by a rule

	in, err := h.c.Insights(bg, api.InsightsQuery{Days: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Days) != 3 || in.Total.OutputTokens != 550_000 || in.Total.CostUSD < 10.99 || in.Total.CostUSD > 11.01 {
		t.Errorf("insights total = %+v", in.Total)
	}
	if len(in.ByModel) != 1 || in.ByModel[0].Key != "claude-opus-5-5" || len(in.ByRepo) != 1 || in.ByRepo[0].Key != "/src/app" ||
		len(in.ByAgent) != 1 || in.ByAgent[0].Sessions != 1 {
		t.Errorf("groups = %+v %+v %+v", in.ByAgent, in.ByModel, in.ByRepo)
	}
	if in.Rules.Blocked != 1 || len(in.Rules.TopBlocked) != 1 || in.Rules.TopRules[0].Key != "no-rm" {
		t.Errorf("rules = %+v", in.Rules)
	}
	if in.Budget == nil || in.Budget.DailyUSD != 10 || in.Budget.Percent < 109 || in.Budget.BurnUSDh < 10.99 {
		t.Errorf("budget = %+v", in.Budget)
	}

	// A restart remembers today's alerts.
	h.stop(t)
	h.notes = &recNotifier{}
	h.start(t)
	time.Sleep(5 * h.d.usageInterval())
	if got := len(actNotes(h, notifyBudget)); got != 0 {
		t.Errorf("alerts repeated after a restart: %d", got)
	}
	if _, err := h.c.Insights(bg, api.InsightsQuery{Days: 91}); err != nil {
		t.Errorf("days above the maximum are clamped, not refused: %v", err)
	}
}

// The usage loop backfills sessions that exited while the daemon was
// down, and doesn't keep their feeds.
func TestUsageBackfill(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	path := actClaudeFile(t, "sess-old")
	day := time.Now().UTC().Add(-48 * time.Hour)
	actAppend(t, path, actPrompt(day, "yesterday's work", "human"), actReply(day, "msg_old", "claude-sonnet-5-5", "done", 10, 20))
	exited := time.Now().UTC().Add(-24 * time.Hour).Round(0)
	if err := h.d.db.Write(bg, func(tx *store.Tx) error {
		return tx.PutSession(bg, api.Session{ID: "s_old", Agent: api.AgentClaude, TranscriptPath: path, State: api.StateExited,
			StartedAt: day.Round(0), UpdatedAt: exited, ExitedAt: &exited})
	}); err != nil {
		t.Fatal(err)
	}
	h.stop(t)
	h.start(t)
	waitFor(t, "backfilled usage", func() bool {
		in, err := h.c.Insights(bg, api.InsightsQuery{Days: 7})
		return err == nil && in.Total.OutputTokens == 20
	})
	st := h.d.act()
	st.reader.Forget("unused") // the call is safe on unknown keys
	if spent := st.reader.SpentSince(time.Time{}); spent != 0 {
		t.Errorf("a backfilled session's feed stayed in memory (spent %v)", spent)
	}
}

// OpenCode keeps its transcript in SQLite; Activity reads it directly.
func TestActivityOpenCode(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	path := filepath.Join(os.Getenv("PILOTHOUSE_USER_HOME"), ".local", "share", "opencode", "opencode.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, cost REAL, tokens_input INTEGER, tokens_output INTEGER,
			tokens_reasoning INTEGER, tokens_cache_read INTEGER, tokens_cache_write INTEGER, model TEXT)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT, time_created INTEGER, time_updated INTEGER, data TEXT)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT, session_id TEXT, time_created INTEGER, time_updated INTEGER, data TEXT)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	const sid = "ses_oc"
	now := time.Now().UTC()
	ms := now.UnixMilli()
	for _, q := range []struct {
		stmt string
		args []any
	}{
		{`INSERT INTO session (id, cost, tokens_input, tokens_output, tokens_reasoning,
			tokens_cache_read, tokens_cache_write, model) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			[]any{sid, 0.12, 800, 40, 3, 200, 0, `{"id":"oc-model"}`}},
		{`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`,
			[]any{"m1", sid, ms - 3000, ms - 3000, `{"role":"user"}`}},
		{`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`,
			[]any{"m2", sid, ms - 2000, ms - 1000, `{"role":"assistant"}`}},
		{`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?, ?)`,
			[]any{"p1", "m1", sid, ms - 3000, ms - 3000, `{"type":"text","text":"fix the build"}`}},
		{`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?, ?)`,
			[]any{"p2", "m2", sid, ms - 2000, ms - 1000, `{"type":"tool","tool":"bash","state":{"status":"completed","input":{"command":"go test ./..."},"output":"ok"}}`}},
	} {
		if _, err := db.Exec(q.stmt, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("OPENCODE_DB", path)
	actPutSession(t, h, api.Session{ID: "s_oc", Agent: api.AgentOpenCode, AgentSessionID: sid, Cwd: "/src/app"})
	act, err := h.c.Activity(bg, "s_oc", api.ActivityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if !act.Available || len(act.Items) != 2 {
		t.Fatalf("activity = %+v", act)
	}
	if act.Items[0].Kind != api.ActivityPrompt || act.Items[1].Summary != "Run: go test ./..." {
		t.Fatalf("items = %+v", act.Items)
	}
	if act.Usage == nil || act.Usage.CostUSD != 0.12 || act.Usage.InputTokens != 800 {
		t.Fatalf("usage = %+v", act.Usage)
	}
}
