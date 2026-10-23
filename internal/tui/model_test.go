package tui

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/client"
)

var update = flag.Bool("update", false, "rewrite golden files")

var t0 = time.Date(2026, 10, 22, 15, 4, 5, 0, time.UTC)

type fakeBackend struct {
	mu        sync.Mutex
	snap      api.Snapshot
	snapErr   error
	snaps     int
	decides   []decideCall
	decideErr error
	presence  []api.PresenceMode
	presErr   error
	explain   api.Explain
	seen      []string
	requests  map[string]api.Request
	events    []api.Event
	streamErr error
}

type decideCall struct {
	id   string
	body api.DecideBody
}

func (f *fakeBackend) Snapshot(context.Context) (api.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snaps++
	return f.snap, f.snapErr
}

func (f *fakeBackend) Stream(ctx context.Context, after int64, fn func(api.Event)) error {
	f.mu.Lock()
	evs, err := f.events, f.streamErr
	f.mu.Unlock()
	for _, e := range evs {
		if e.Seq > after {
			fn(e)
		}
	}
	if err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

func (f *fakeBackend) Request(_ context.Context, id string) (api.Request, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.requests[id]; ok {
		return r, nil
	}
	return api.Request{}, &client.APIError{Status: 404, Code: "not_found", Msg: "not found"}
}

func (f *fakeBackend) Decide(_ context.Context, id string, b api.DecideBody) (api.Request, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decides = append(f.decides, decideCall{id, b})
	if f.decideErr != nil {
		return api.Request{}, f.decideErr
	}
	return api.Request{ID: id, Status: api.RequestDecided, Decision: &api.Decision{Behavior: b.Behavior, Scope: b.Scope, By: "tui"}}, nil
}

func (f *fakeBackend) SetPresence(_ context.Context, m api.PresenceMode) (api.Presence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.presence = append(f.presence, m)
	if f.presErr != nil {
		return api.Presence{}, f.presErr
	}
	st := api.PresencePresent
	if m == api.PresenceForceAway {
		st = api.PresenceAway
	}
	return api.Presence{State: st, Mode: m, Source: "manual"}, nil
}

func (f *fakeBackend) Explain(context.Context, string) (api.Explain, error) {
	return f.explain, nil
}

func (f *fakeBackend) MarkSeen(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, id)
	return nil
}

func ptr[T any](v T) *T { return &v }

// Fixture is shared with the PTY screenshot helper (screen_test.go).
func fixtureSnapshot() api.Snapshot {
	return api.Snapshot{
		Seq:     100,
		Machine: "studio.local",
		Version: "0.1.0",
		Presence: api.Presence{
			State: api.PresencePresent, Mode: api.PresenceAuto, Source: "os_idle", ChangedAt: t0.Add(-time.Hour),
		},
		Sessions: []api.Session{
			{ID: "s_k3m9q2xv7a", Agent: api.AgentClaude, Mode: api.ModeWrapped, Title: "pilothouse", Branch: "feat/inbox",
				Cwd: "/home/dev/src/pilothouse", State: api.StateBlocked, BlockKind: api.BlockPermission, StateLayer: api.LayerHook,
				StateDetail: "Asked to run git push", LastMessage: "Tests pass. Pushing the branch so CI can run.",
				StartedAt: t0.Add(-50 * time.Minute), UpdatedAt: t0.Add(-12 * time.Second)},
			{ID: "s_p4t8w6n2rb", Agent: api.AgentCodex, Mode: api.ModeObserved, Title: "webapp", Branch: "checkout-v2",
				Cwd: "/home/dev/src/webapp", State: api.StateBlocked, BlockKind: api.BlockPermission, StateLayer: api.LayerHook,
				StartedAt: t0.Add(-20 * time.Minute), UpdatedAt: t0.Add(-64 * time.Second)},
			{ID: "s_c2d7f5h1jq", Agent: api.AgentClaude, Mode: api.ModeObserved, Title: "api", Branch: "main",
				Cwd: "/home/dev/src/api", State: api.StateBlocked, BlockKind: api.BlockQuestion, StateLayer: api.LayerHook,
				StateDetail: "Asked: Which database should the migration target, Postgres 16 or 17?",
				StartedAt:   t0.Add(-2 * time.Hour), UpdatedAt: t0.Add(-3*time.Minute - 20*time.Second)},
			{ID: "s_w1x3y5z7ab", Agent: api.AgentClaude, Mode: api.ModeWrapped, Title: "docs", Branch: "main",
				Cwd: "/home/dev/src/docs", State: api.StateWorking, StateLayer: api.LayerHook,
				StateDetail: "Ran: npm run build", StartedAt: t0.Add(-40 * time.Minute), UpdatedAt: t0.Add(-5 * time.Second)},
			{ID: "s_m8n6b4v2cx", Agent: api.AgentCodex, Mode: api.ModeObserved, Title: "infra", Branch: "tf-upgrade",
				Cwd: "/home/dev/src/infra", State: api.StateWorking, StateLayer: api.LayerHook,
				StateDetail: "Editing modules/vpc/main.tf", StartedAt: t0.Add(-35 * time.Minute), UpdatedAt: t0.Add(-41 * time.Second)},
			{ID: "s_q9r7s5t3uv", Agent: api.AgentClaude, Mode: api.ModeObserved, Title: "pilothouse-site", Branch: "main",
				Cwd: "/home/dev/src/pilothouse-site", State: api.StateWaiting, StateLayer: api.LayerHook, Unseen: true,
				LastMessage: "Done. The landing page now loads in 380 ms. Want me to add the pricing section next?",
				StartedAt:   t0.Add(-3 * time.Hour), UpdatedAt: t0.Add(-2*time.Minute - 5*time.Second)},
			{ID: "s_e5f6g7h8ij", Agent: api.AgentGeneric, Mode: api.ModeWrapped, Title: "scratch",
				Cwd: "/home/dev/scratch", State: api.StateErrored, StateLayer: api.LayerOSC, StateDetail: "Reported an error",
				StartedAt: t0.Add(-5 * time.Hour), UpdatedAt: t0.Add(-25 * time.Minute)},
			{ID: "s_z0y9x8w7vu", Agent: api.AgentClaude, Mode: api.ModeObserved, Title: "old-spike", Branch: "spike/sqlite",
				Cwd: "/home/dev/src/old-spike", State: api.StateExited, StateLayer: api.LayerHook,
				StartedAt: t0.Add(-6 * time.Hour), UpdatedAt: t0.Add(-3 * time.Hour), ExitedAt: ptr(t0.Add(-3 * time.Hour)), ExitCode: ptr(0)},
		},
		Requests: []api.Request{
			{ID: "r_7hq2m4k9pd", SessionID: "s_k3m9q2xv7a", Agent: api.AgentClaude, Checkpoint: api.CheckpointPermission,
				Tool: "Bash", ToolKind: api.ToolShell, Command: "git push origin feat/inbox --force-with-lease",
				Summary: "Run: git push origin feat/inbox --force-with-lease", Cwd: "/home/dev/src/pilothouse",
				Status: api.RequestOpen, Route: api.RouteDevices, PolicyRule: "ask-on-push",
				CreatedAt: t0.Add(-12 * time.Second), ExpiresAt: t0.Add(8*time.Minute + 48*time.Second)},
			{ID: "r_b3c5d7e9fg", SessionID: "s_p4t8w6n2rb", Agent: api.AgentCodex, Checkpoint: api.CheckpointPermission,
				Tool: "apply_patch", ToolKind: api.ToolEdit, Paths: []string{"/home/dev/src/webapp/src/checkout/cart.ts", "/home/dev/src/webapp/src/checkout/total.ts"},
				Summary: "Edit 2 files in src/checkout", Cwd: "/home/dev/src/webapp",
				Status: api.RequestSteppedAside, Route: api.RouteTerminal,
				CreatedAt: t0.Add(-64 * time.Second), ExpiresAt: t0.Add(9 * time.Minute)},
			{ID: "r_closed0001", SessionID: "s_w1x3y5z7ab", Agent: api.AgentClaude, Status: api.RequestDecided,
				CreatedAt: t0.Add(-time.Hour)},
		},
	}
}

func plainRenderer() *lipgloss.Renderer {
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(termenv.Ascii)
	return r
}

func newTestModel(t *testing.T, be *fakeBackend, w, h int) *Model {
	t.Helper()
	t.Setenv("PILOTHOUSE_USER_HOME", "/home/dev")
	m := New(Options{Backend: be, Exe: "/usr/local/bin/pilothouse", Now: func() time.Time { return t0 }, Renderer: plainRenderer(), NoTick: true})
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m
}

func loaded(t *testing.T, w, h int) (*Model, *fakeBackend) {
	t.Helper()
	be := &fakeBackend{snap: fixtureSnapshot()}
	m := newTestModel(t, be, w, h)
	_, cmd := m.Update(snapshotMsg{snap: be.snap})
	if cmd == nil {
		t.Fatal("snapshot must start the stream")
	}
	t.Cleanup(m.stopStream)
	return m, be
}

func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func press(m *Model, keys ...string) tea.Cmd {
	var cmd tea.Cmd
	for _, k := range keys {
		_, cmd = m.Update(key(k))
	}
	return cmd
}

// exec runs a command and feeds its message back, like the runtime does.
func execCmd(t *testing.T, m *Model, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command")
	}
	msg := cmd()
	if msg != nil {
		m.Update(msg)
	}
	return msg
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	// Trailing spaces are layout padding; strip them so diffs stay readable.
	lines := strings.Split(got, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	got = strings.Join(lines, "\n") + "\n"
	p := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/tui -update)", err)
	}
	if string(want) != got {
		t.Fatalf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func TestGoldenViews(t *testing.T) {
	for _, sz := range []struct{ w, h int }{{120, 35}, {90, 30}, {70, 30}, {60, 24}} {
		m, _ := loaded(t, sz.w, sz.h)
		golden(t, "inbox_"+strconv.Itoa(sz.w)+"x"+strconv.Itoa(sz.h), m.View())
	}
}

func TestGoldenOtherScreens(t *testing.T) {
	m, be := loaded(t, 100, 30)
	press(m, "?")
	golden(t, "help_100x30", m.View())
	press(m, "esc")

	be.explain = api.Explain{
		Session: fixtureSnapshot().Sessions[0],
		Rule:    "2: an open permission request exists",
		Evidence: []api.StateEvidence{
			{At: t0.Add(-12 * time.Second), Layer: api.LayerHook, State: api.StateBlocked, Kind: api.BlockPermission, Detail: "PermissionRequest Bash: git push origin feat/inbox --force-with-lease", Fresh: true},
			{At: t0.Add(-15 * time.Second), Layer: api.LayerHook, State: api.StateWorking, Detail: "PreToolUse Bash", Fresh: true},
			{At: t0.Add(-14 * time.Minute), Layer: api.LayerOSC, State: api.StateWorking, Detail: "OSC 7501 state=working", Fresh: false},
		},
	}
	cmd := press(m, "e")
	execCmd(t, m, cmd)
	golden(t, "explain_100x30", m.View())
	press(m, "esc")

	press(m, "n")
	golden(t, "deny_empty_100x30", m.View())
	for _, r := range "use a PR" {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	golden(t, "deny_100x30", m.View())
}

func TestDaemonDownScreen(t *testing.T) {
	be := &fakeBackend{snapErr: client.ErrDaemonDown}
	m := newTestModel(t, be, 80, 20)
	golden(t, "connecting_80x20", m.View())
	_, cmd := m.Update(snapshotMsg{err: client.ErrDaemonDown})
	if cmd == nil {
		t.Fatal("must schedule a retry")
	}
	golden(t, "down_80x20", m.View())
	if !strings.Contains(m.View(), "pilothouse start") {
		t.Fatal("down screen must say how to start the daemon")
	}
}

func TestEmptyAndNarrow(t *testing.T) {
	be := &fakeBackend{snap: api.Snapshot{Seq: 1, Machine: "box", Presence: api.Presence{State: api.PresenceAway, Mode: api.PresenceForceAway}}}
	m := newTestModel(t, be, 80, 16)
	m.Update(snapshotMsg{snap: be.snap})
	defer m.stopStream()
	golden(t, "empty_80x16", m.View())

	m.Update(tea.WindowSizeMsg{Width: 50, Height: 10})
	if !strings.Contains(m.View(), "at least 60 columns") {
		t.Fatalf("narrow view:\n%s", m.View())
	}
}

func TestViewFitsTerminal(t *testing.T) {
	for _, sz := range []struct{ w, h int }{{60, 12}, {60, 24}, {70, 30}, {120, 35}, {200, 60}, {80, 8}} {
		m, _ := loaded(t, sz.w, sz.h)
		for _, mode := range []string{"", "?", "esc", "enter"} {
			if mode != "" {
				press(m, mode)
			}
			lines := strings.Split(m.View(), "\n")
			if len(lines) != sz.h {
				t.Fatalf("%dx%d %q: %d lines", sz.w, sz.h, mode, len(lines))
			}
			for i, l := range lines {
				if lw := lipgloss.Width(l); lw > sz.w {
					t.Fatalf("%dx%d %q: line %d is %d wide: %q", sz.w, sz.h, mode, i, lw, l)
				}
			}
		}
	}
}

func TestSectionsAndOrder(t *testing.T) {
	m, _ := loaded(t, 120, 35)
	var keys []string
	for _, r := range m.rows {
		keys = append(keys, strconv.Itoa(r.section)+":"+r.key)
	}
	want := []string{
		"0:r:r_7hq2m4k9pd", // held first
		"0:r:r_b3c5d7e9fg", // stepped aside
		"0:s:s_c2d7f5h1jq", // blocked on a question, no request
		"1:s:s_w1x3y5z7ab", "1:s:s_m8n6b4v2cx",
		"2:s:s_q9r7s5t3uv", "2:s:s_e5f6g7h8ij",
		"3:s:s_z0y9x8w7vu",
	}
	if strings.Join(keys, " ") != strings.Join(want, " ") {
		t.Fatalf("rows:\n%v\nwant\n%v", keys, want)
	}
	v := m.View()
	for _, s := range []string{"NEEDS YOU 3", "WORKING 2", "WAITING 2", "RECENTLY EXITED 1", "$ git push origin feat/inbox", "held · y a n", "in its terminal", "3 need you"} {
		if !strings.Contains(v, s) {
			t.Errorf("view lacks %q", s)
		}
	}
}

func TestAllowOnceAndForSession(t *testing.T) {
	m, be := loaded(t, 120, 35)
	execCmd(t, m, press(m, "y"))
	if len(be.decides) != 1 || be.decides[0].id != "r_7hq2m4k9pd" || be.decides[0].body.Behavior != api.BehaviorAllow || be.decides[0].body.Scope != api.ScopeOnce {
		t.Fatalf("decides = %+v", be.decides)
	}
	if !strings.Contains(m.View(), "✓ Allowed once: $ git push") {
		t.Fatalf("flash missing:\n%s", m.View())
	}
	if m.rows[0].key == "r:r_7hq2m4k9pd" {
		t.Fatal("answered request must leave the list")
	}
	// The selection moved to the next item: the stepped-aside request.
	if m.selKey != "r:r_b3c5d7e9fg" {
		t.Fatalf("selection = %s", m.selKey)
	}

	// Reload and allow for session.
	m.Update(snapshotMsg{snap: fixtureSnapshot()})
	m.cursor, m.selKey = 0, "r:r_7hq2m4k9pd"
	m.rebuild()
	execCmd(t, m, press(m, "a"))
	if be.decides[1].body.Scope != api.ScopeSession {
		t.Fatalf("decide = %+v", be.decides[1])
	}
	if !strings.Contains(m.View(), "Allowed for this session") {
		t.Fatal("flash missing")
	}
}

func TestDenyWithReason(t *testing.T) {
	m, be := loaded(t, 120, 35)
	if cmd := press(m, "n"); m.mode != modeDeny {
		t.Fatalf("mode = %v cmd=%v", m.mode, cmd)
	}
	for _, r := range "use a PR" {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	// While typing, y/a/n are text, not actions.
	press(m, "y")
	if len(be.decides) != 0 {
		t.Fatal("typing must not decide")
	}
	execCmd(t, m, press(m, "enter"))
	if len(be.decides) != 1 || be.decides[0].body.Behavior != api.BehaviorDeny || be.decides[0].body.Message != "use a PRy" {
		t.Fatalf("decides = %+v", be.decides)
	}
	if !strings.Contains(m.View(), "✗ Denied: $ git push") {
		t.Fatalf("view:\n%s", m.View())
	}

	// Esc cancels without deciding.
	m.Update(snapshotMsg{snap: fixtureSnapshot()})
	m.cursor, m.selKey = 0, "r:r_7hq2m4k9pd"
	m.rebuild()
	press(m, "n", "esc")
	if m.mode != modeList || len(be.decides) != 1 {
		t.Fatal("esc must cancel")
	}
}

func TestCannotAnswerSteppedAsideOrSessions(t *testing.T) {
	m, be := loaded(t, 120, 35)
	press(m, "j") // stepped-aside codex request (observed)
	if cmd := press(m, "y"); cmd != nil {
		t.Fatal("must not decide a stepped-aside request")
	}
	if !strings.Contains(m.View(), "answer it in that terminal") {
		t.Fatalf("view:\n%s", m.View())
	}
	press(m, "j", "j") // working wrapped session
	press(m, "n")
	if m.mode == modeDeny || len(be.decides) != 0 {
		t.Fatal("sessions can't be denied")
	}
	if !strings.Contains(m.View(), "Nothing to answer here") {
		t.Fatal("flash missing")
	}
}

func TestAlreadyDecidedAndAgentCaller(t *testing.T) {
	m, be := loaded(t, 120, 35)
	be.decideErr = &client.APIError{Status: 409, Code: "already_decided", Msg: "request already closed"}
	be.requests = map[string]api.Request{"r_7hq2m4k9pd": {ID: "r_7hq2m4k9pd", Status: api.RequestDecided}}
	follow := execCmd(t, m, press(m, "y"))
	if !strings.Contains(m.View(), "Already answered elsewhere: the first answer wins") {
		t.Fatalf("view:\n%s", m.View())
	}
	// The model fetches the request to learn its final state.
	_, cmd := m.Update(follow)
	execCmd(t, m, cmd)
	if _, ok := m.requests["r_7hq2m4k9pd"]; ok {
		t.Fatal("closed request must be dropped")
	}

	m2, be2 := loaded(t, 120, 35)
	be2.decideErr = &client.APIError{Status: 403, Code: "agent_caller", Msg: "decisions are not accepted from agent processes"}
	execCmd(t, m2, press(m2, "y"))
	if !strings.Contains(m2.View(), "Refused: this terminal runs inside an agent session") {
		t.Fatalf("view:\n%s", m2.View())
	}
	if _, ok := m2.requests["r_7hq2m4k9pd"]; !ok {
		t.Fatal("refused request must stay")
	}
}

func TestEventsUpdateTheList(t *testing.T) {
	m, _ := loaded(t, 120, 35)
	m.cursor, m.selKey = 3, "s:s_w1x3y5z7ab"
	ev := func(seq int64, typ api.EventType, v any) eventMsg {
		b, _ := json.Marshal(v)
		return eventMsg{gen: m.gen, ev: api.Event{Seq: seq, Type: typ, Data: b}}
	}
	// A new held request opens above the selection; selection stays put.
	nr := api.Request{ID: "r_new0000001", SessionID: "s_m8n6b4v2cx", Agent: api.AgentCodex, Tool: "shell", Command: "terraform apply",
		Status: api.RequestOpen, CreatedAt: t0.Add(-time.Second), Cwd: "/home/dev/src/infra"}
	m.Update(ev(101, api.EventRequestOpened, nr))
	if m.selKey != "s:s_w1x3y5z7ab" || m.rows[m.cursor].key != "s:s_w1x3y5z7ab" {
		t.Fatalf("selection jumped to %s", m.rows[m.cursor].key)
	}
	if !strings.Contains(m.View(), "$ terraform apply") || !strings.Contains(m.View(), "4 need you") {
		t.Fatal("new request missing")
	}
	// Duplicate (seq already seen) is ignored.
	nr.Status = api.RequestDecided
	m.Update(ev(101, api.EventRequestClosed, nr))
	if _, ok := m.requests["r_new0000001"]; !ok {
		t.Fatal("duplicate event applied")
	}
	m.Update(ev(102, api.EventRequestClosed, nr))
	if _, ok := m.requests["r_new0000001"]; ok {
		t.Fatal("closed request still listed")
	}
	// Session finishes its turn: moves to Waiting.
	s := fixtureSnapshot().Sessions[3]
	s.State, s.LastMessage, s.UpdatedAt = api.StateWaiting, "Build is green.", t0
	m.Update(ev(103, api.EventSessionUpdated, s))
	if r := m.rows[m.cursor]; r.key != "s:s_w1x3y5z7ab" || r.section != sectionWaiting {
		t.Fatalf("row = %+v", r)
	}
	m.Update(ev(104, api.EventPresenceChanged, api.Presence{State: api.PresenceAway, Mode: api.PresenceAuto, Source: "os_idle"}))
	if !strings.Contains(m.View(), "● away · auto") {
		t.Fatalf("header:\n%s", strings.SplitN(m.View(), "\n", 2)[0])
	}
	if m.seq != 104 {
		t.Fatalf("seq = %d", m.seq)
	}
	// Events from a replaced stream are ignored.
	m.Update(eventMsg{gen: m.gen - 1, ev: api.Event{Seq: 200, Type: api.EventPresenceChanged}})
	if m.seq != 104 {
		t.Fatal("stale stream event applied")
	}
}

func TestStreamResyncAndReconnect(t *testing.T) {
	m, be := loaded(t, 120, 35)
	before := be.snaps
	_, cmd := m.Update(streamEndMsg{gen: m.gen, err: client.ErrResync})
	execCmd(t, m, cmd)
	if be.snaps != before+1 {
		t.Fatal("resync must reload the snapshot")
	}
	if m.conn != connLive {
		t.Fatal("must be live after reload")
	}

	_, cmd = m.Update(streamEndMsg{gen: m.gen, err: io.ErrUnexpectedEOF})
	if cmd == nil || m.conn != connReconnecting {
		t.Fatal("disconnect must schedule a reconnect")
	}
	v := m.View()
	if !strings.Contains(v, "Offline: lost connection to the daemon") || !strings.Contains(v, "◌ offline") {
		t.Fatalf("banner missing:\n%s", v)
	}
	// The data is still shown while offline.
	if !strings.Contains(v, "$ git push origin") {
		t.Fatal("last known state must stay visible")
	}
	m.Update(retryMsg{gen: m.gen})
	m.Update(snapshotMsg{snap: fixtureSnapshot()})
	if m.conn != connLive || !strings.Contains(m.View(), "Reconnected") {
		t.Fatal("must reconnect")
	}
	if backoff(0) != 500*time.Millisecond || backoff(3) != 4*time.Second || backoff(20) != 10*time.Second {
		t.Fatal("backoff")
	}
}

func TestStreamDeliversEvents(t *testing.T) {
	be := &fakeBackend{snap: fixtureSnapshot()}
	sess := fixtureSnapshot().Sessions[4]
	sess.State = api.StateWaiting
	data, _ := json.Marshal(sess)
	be.events = []api.Event{{Seq: 99, Type: api.EventSessionUpdated, Data: data}, {Seq: 101, Type: api.EventSessionUpdated, Data: data}}
	m := newTestModel(t, be, 120, 35)
	_, cmd := m.Update(snapshotMsg{snap: be.snap})
	defer m.stopStream()
	msg := cmd()
	em, ok := msg.(eventMsg)
	if !ok || em.ev.Seq != 101 {
		t.Fatalf("msg = %#v", msg)
	}
	_, next := m.Update(msg)
	if next == nil {
		t.Fatal("listener must be re-armed")
	}
	if m.sessions[sess.ID].State != api.StateWaiting {
		t.Fatal("event not applied")
	}
}

func TestPresenceCycle(t *testing.T) {
	m, be := loaded(t, 120, 35)
	execCmd(t, m, press(m, "p"))
	execCmd(t, m, press(m, "p"))
	execCmd(t, m, press(m, "p"))
	got := []api.PresenceMode{}
	got = append(got, be.presence...)
	want := []api.PresenceMode{api.PresenceForceHere, api.PresenceForceAway, api.PresenceAuto}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("presence calls = %v", got)
		}
	}
	be.presErr = &client.APIError{Status: 403, Code: "agent_caller", Msg: "no"}
	execCmd(t, m, press(m, "p"))
	if !strings.Contains(m.View(), "Refused") {
		t.Fatal("403 must be explained")
	}
}

func TestEnterAttachesOrExpands(t *testing.T) {
	m, _ := loaded(t, 120, 35)
	if cmd := press(m, "enter"); cmd == nil {
		t.Fatal("wrapped session: enter must attach")
	}
	if m.streaming {
		t.Fatal("stream must pause while attached")
	}
	m.Update(attachDoneMsg{})
	m.Update(snapshotMsg{snap: fixtureSnapshot()})
	m.cursor, m.selKey = 2, "s:s_c2d7f5h1jq"
	m.rebuild()
	press(m, "enter")
	if !m.expanded {
		t.Fatal("observed session: enter expands")
	}
	press(m, "esc")
	if m.expanded {
		t.Fatal("esc collapses")
	}
}

func TestMarkSeenOnSelect(t *testing.T) {
	m, be := loaded(t, 120, 35)
	m.cursor, m.selKey = 4, "s:s_m8n6b4v2cx"
	cmd := press(m, "j") // onto the unseen waiting session
	if m.selKey != "s:s_q9r7s5t3uv" {
		t.Fatalf("sel = %s", m.selKey)
	}
	execCmd(t, m, cmd)
	if len(be.seen) != 1 || be.seen[0] != "s_q9r7s5t3uv" {
		t.Fatalf("seen = %v", be.seen)
	}
	if m.sessions["s_q9r7s5t3uv"].Unseen {
		t.Fatal("local unseen flag must clear")
	}
}

func TestNavigationAndScroll(t *testing.T) {
	m, _ := loaded(t, 80, 14)
	press(m, "G")
	if m.cursor != len(m.rows)-1 {
		t.Fatal("G goes to the end")
	}
	if !strings.Contains(m.View(), "old-spike") {
		t.Fatalf("selected row must be visible:\n%s", m.View())
	}
	if !strings.Contains(m.View(), "↑") {
		t.Fatal("scroll indicator missing")
	}
	press(m, "g")
	if m.cursor != 0 || m.scroll != 0 {
		t.Fatalf("g: cursor %d scroll %d", m.cursor, m.scroll)
	}
	press(m, "k")
	if m.cursor != 0 {
		t.Fatal("k at top stays")
	}
}

func TestQuit(t *testing.T) {
	m, _ := loaded(t, 120, 35)
	cmd := press(m, "q")
	if cmd == nil {
		t.Fatal("q must quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("q must return tea.Quit")
	}
}

func TestAge(t *testing.T) {
	tests := map[time.Duration]string{
		-time.Second:                   "0s",
		12 * time.Second:               "12s",
		64 * time.Second:               "1m04s",
		9*time.Minute + 59*time.Second: "9m59s",
		25 * time.Minute:               "25m",
		3*time.Hour + 5*time.Minute:    "3h05m",
		50 * time.Hour:                 "2d",
	}
	for d, want := range tests {
		if got := Age(d); got != want {
			t.Errorf("Age(%v) = %s want %s", d, got, want)
		}
	}
}

func TestDescribeErr(t *testing.T) {
	if !strings.Contains(describeErr(client.ErrDaemonDown, "x"), "pilothouse start") {
		t.Fatal("daemon down must say how to start")
	}
	if !strings.Contains(describeErr(errors.New("boom"), "answer"), "Couldn't answer: boom") {
		t.Fatal("generic")
	}
}

func TestBurstKeysAreSplit(t *testing.T) {
	m, _ := loaded(t, 120, 35)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("jj")})
	if m.cursor != 2 {
		t.Fatalf("cursor = %d, want 2", m.cursor)
	}
	// In the deny prompt a burst is text.
	m.cursor, m.selKey = 0, "r:r_7hq2m4k9pd"
	press(m, "n")
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("no")})
	if m.input.Value() != "no" {
		t.Fatalf("input = %q", m.input.Value())
	}
}

func TestParseBackground(t *testing.T) {
	tests := []struct {
		in       string
		dark, ok bool
	}{
		{"\x1b]11;rgb:1e1e/1e1e/2e2e\x1b\\\x1b[?62;22c", true, true},
		{"\x1b]11;rgb:ffff/ffff/ffff\x07", false, true},
		{"\x1b]11;rgb:fd/f6/e3\x1b\\", false, true}, // solarized light, 8-bit
		{"\x1b[?1;2c", false, false},                // OSC 11 unsupported
		{"", false, false},
	}
	for _, tt := range tests {
		dark, ok := parseBackground(tt.in)
		if dark != tt.dark || ok != tt.ok {
			t.Errorf("parseBackground(%q) = %v,%v", tt.in, dark, ok)
		}
	}
	t.Setenv("COLORFGBG", "0;15")
	if darkBackground(os.Stdin, os.Stdout, time.Millisecond) {
		t.Error("COLORFGBG 0;15 is a light background")
	}
	t.Setenv("COLORFGBG", "15;0")
	if !darkBackground(os.Stdin, os.Stdout, time.Millisecond) {
		t.Error("COLORFGBG 15;0 is a dark background")
	}
}
