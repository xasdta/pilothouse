package cli

// Tests for workstream F's commands, against a fake daemon on a Unix
// socket in a short /tmp dir. Helpers are prefixed "ibx" so they never
// collide with other workstreams' tests in this package.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/client"
	"github.com/xasdta/pilothouse/internal/presence"
	"github.com/xasdta/pilothouse/internal/service"
	"github.com/xasdta/pilothouse/internal/tui"
	"github.com/xasdta/pilothouse/internal/version"
	"github.com/xasdta/pilothouse/internal/wrap"
)

var ibxT0 = time.Date(2026, 10, 22, 15, 4, 5, 0, time.UTC)

type ibxDecideCall struct {
	id     string
	body   api.DecideBody
	header string
}

type ibxDaemon struct {
	mu       sync.Mutex
	snap     api.Snapshot
	events   []api.Event
	decides  []ibxDecideCall
	presence []string // mode|header
	notified int
	refuse   bool   // simulate an agent caller (403)
	invalid  string // answer decisions with 400 invalid and this message
	policy   api.PolicyStatus
	devices  []api.Device
	explain  api.Explain
}

func ibxError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(api.Error{Error: msg, Code: code})
}

func ibxJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (d *ibxDaemon) findRequest(ref string) (api.Request, int) {
	var hits []api.Request
	for _, r := range d.snap.Requests {
		if strings.HasPrefix(r.ID, ref) {
			hits = append(hits, r)
		}
	}
	switch len(hits) {
	case 0:
		return api.Request{}, 404
	case 1:
		return hits[0], 200
	}
	return api.Request{}, 400
}

func (d *ibxDaemon) findSession(ref string) (api.Session, bool) {
	for _, s := range d.snap.Sessions {
		if strings.HasPrefix(s.ID, ref) {
			return s, true
		}
	}
	return api.Session{}, false
}

func (d *ibxDaemon) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		ibxJSON(w, api.Health{Version: version.Version, Protocol: api.ProtocolVersion, PID: 4242, Machine: "box"})
	})
	mux.HandleFunc("GET /v1/snapshot", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		ibxJSON(w, d.snap)
	})
	mux.HandleFunc("GET /v1/requests", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		ibxJSON(w, d.snap.Requests)
	})
	mux.HandleFunc("GET /v1/requests/{id}", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		req, st := d.findRequest(r.PathValue("id"))
		if st != 200 {
			ibxError(w, st, "not_found", "request not found")
			return
		}
		ibxJSON(w, req)
	})
	mux.HandleFunc("POST /v1/requests/{id}/decide", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		var b api.DecideBody
		_ = json.NewDecoder(r.Body).Decode(&b)
		d.decides = append(d.decides, ibxDecideCall{r.PathValue("id"), b, r.Header.Get(client.ClientHeader)})
		if d.refuse {
			ibxError(w, 403, "agent_caller", "decisions are not accepted from agent processes")
			return
		}
		if d.invalid != "" {
			ibxError(w, 400, "invalid", d.invalid)
			return
		}
		req, st := d.findRequest(r.PathValue("id"))
		if st != 200 {
			ibxError(w, 404, "not_found", "request not found")
			return
		}
		if req.ID == "r_raced00001" {
			ibxError(w, 409, "already_decided", "request already closed by device:d_phone")
			return
		}
		req.Status = api.RequestDecided
		req.Decision = &api.Decision{RequestID: req.ID, Behavior: b.Behavior, Scope: b.Scope, Message: b.Message, Answers: b.Answers, By: r.Header.Get(client.ClientHeader)}
		ibxJSON(w, req)
	})
	mux.HandleFunc("GET /v1/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		s, ok := d.findSession(r.PathValue("id"))
		if !ok {
			ibxError(w, 404, "not_found", "session not found")
			return
		}
		ibxJSON(w, s)
	})
	mux.HandleFunc("GET /v1/sessions/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		var out []api.Event
		for i := len(d.events) - 1; i >= 0 && len(out) < limit; i-- {
			if d.events[i].SessionID == r.PathValue("id") {
				out = append(out, d.events[i])
			}
		}
		ibxJSON(w, out)
	})
	mux.HandleFunc("GET /v1/sessions/{id}/explain", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if !strings.HasPrefix(d.explain.Session.ID, r.PathValue("id")) {
			ibxError(w, 404, "not_found", "session not found")
			return
		}
		ibxJSON(w, d.explain)
	})
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		d.mu.Lock()
		var out []api.Event
		for _, e := range d.events {
			if e.Seq > after {
				out = append(out, e)
			}
		}
		d.mu.Unlock()
		if r.URL.Query().Get("follow") != "1" {
			if out == nil {
				out = []api.Event{}
			}
			ibxJSON(w, out)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		// Live events: one new event after the log was printed.
		live := api.Event{Seq: 999, At: ibxT0, Type: api.EventPresenceChanged, Data: json.RawMessage(`{"state":"away","mode":"auto","source":"os_idle","idle_secs":120}`)}
		b, _ := json.Marshal(live)
		fmt.Fprintf(w, ": ping\n\nevent: event\ndata: %s\n\n", b)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("PUT /v1/presence", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		var b api.PresenceBody
		_ = json.NewDecoder(r.Body).Decode(&b)
		d.presence = append(d.presence, string(b.Mode)+"|"+r.Header.Get(client.ClientHeader))
		if d.refuse {
			ibxError(w, 403, "agent_caller", "presence changes are not accepted from agent processes")
			return
		}
		st := api.PresencePresent
		if b.Mode == api.PresenceForceAway {
			st = api.PresenceAway
		}
		src := "manual"
		if b.Mode == api.PresenceAuto {
			src = "os_idle"
		}
		ibxJSON(w, api.Presence{State: st, Mode: b.Mode, Source: src, IdleSecs: 4})
	})
	mux.HandleFunc("POST /v1/notify/test", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.notified++
		d.mu.Unlock()
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /v1/policy/status", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		ibxJSON(w, d.policy)
	})
	mux.HandleFunc("GET /v1/devices", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		ibxJSON(w, d.devices)
	})
	return mux
}

// ibxEnv points every Pilothouse path at temp dirs (BUILD-SPEC invariant 1)
// and swaps every system seam for a fake.
func ibxEnv(t *testing.T) string {
	t.Helper()
	run, err := os.MkdirTemp("/tmp", "ag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(run) })
	t.Setenv("PILOTHOUSE_RUN_DIR", run)
	t.Setenv("PILOTHOUSE_HOME", filepath.Join(t.TempDir(), "home"))
	t.Setenv("PILOTHOUSE_USER_HOME", "/home/dev")
	t.Setenv("PILOTHOUSE_NO_UPDATE_CHECK", "1")

	saved := []func(){}
	save := func(restore func()) { saved = append(saved, restore) }
	{
		v := inboxAdapters
		save(func() { inboxAdapters = v })
		inboxAdapters = func() []adapters.Adapter { return nil }
	}
	{
		v := inboxServiceMgr
		save(func() { inboxServiceMgr = v })
		inboxServiceMgr = func(string) serviceManager { return &ibxService{kind: ""} }
	}
	{
		v := inboxStartDaemon
		save(func() { inboxStartDaemon = v })
		inboxStartDaemon = func(context.Context, string) error { return errors.New("not in tests") }
	}
	{
		v := inboxExecutable
		save(func() { inboxExecutable = v })
		inboxExecutable = func() (string, error) { return "/opt/pilothouse/bin/pilothouse", nil }
	}
	{
		v := inboxRunTUI
		save(func() { inboxRunTUI = v })
		inboxRunTUI = func(context.Context, tui.Options) error { return errors.New("tui not expected") }
	}
	{
		v := inboxDetector
		save(func() { inboxDetector = v })
		inboxDetector = func() presence.Detector { return ibxDetector{} }
	}
	{
		v := inboxLookPath
		save(func() { inboxLookPath = v })
		inboxLookPath = func(n string) (string, error) {
			if n == "pilothouse" {
				return "/opt/pilothouse/bin/pilothouse", nil
			}
			return "/usr/bin/" + n, nil
		}
	}
	{
		v := inboxShellNoise
		save(func() { inboxShellNoise = v })
		inboxShellNoise = func(context.Context) (string, string, error) { return "/bin/zsh", "", nil }
	}
	{
		v := inboxHolders
		save(func() { inboxHolders = v })
		// The wrapped fixture session has a live holder.
		inboxHolders = func(context.Context) ([]wrap.HolderInfo, error) {
			return []wrap.HolderInfo{
				{ServerHello: wrap.ServerHello{SessionID: "s_k3m9q2xv7a"}},
				{ServerHello: wrap.ServerHello{SessionID: "s_gone000000", Exited: true}},
			}, nil
		}
	}
	{
		v1, v2, v3, v4, v5 := inboxIsTerminal, inboxStdinTerminal, inboxLocation, inboxNow, inboxHealthWait
		save(func() {
			inboxIsTerminal, inboxStdinTerminal, inboxLocation, inboxNow, inboxHealthWait = v1, v2, v3, v4, v5
		})
		inboxIsTerminal = func() bool { return false }
		inboxStdinTerminal = func() bool { return false }
		inboxLocation = time.UTC
		inboxNow = func() time.Time { return ibxT0 }
		inboxHealthWait = 300 * time.Millisecond
	}
	t.Cleanup(func() {
		for _, r := range saved {
			r()
		}
	})
	return run
}

func ibxStart(t *testing.T) *ibxDaemon {
	t.Helper()
	run := ibxEnv(t)
	d := &ibxDaemon{snap: ibxSnapshot(), events: ibxEvents()}
	d.explain = api.Explain{Session: d.snap.Sessions[0], Rule: "2: an open permission request exists", Evidence: []api.StateEvidence{
		{At: ibxT0.Add(-12 * time.Second), Layer: api.LayerHook, State: api.StateBlocked, Kind: api.BlockPermission, Detail: "PermissionRequest Bash", Fresh: true},
		{At: ibxT0.Add(-20 * time.Minute), Layer: api.LayerOSC, State: api.StateWorking, Detail: "OSC 7501 working"},
	}}
	ln, err := net.Listen("unix", filepath.Join(run, "pilothouse.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(run, "pilothouse.sock"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(d.handler())
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return d
}

func ibxSnapshot() api.Snapshot {
	exit0 := 0
	exited := ibxT0.Add(-2 * time.Hour)
	return api.Snapshot{
		Seq: 17, Machine: "box", Version: version.Version,
		Presence: api.Presence{State: api.PresencePresent, Mode: api.PresenceAuto, Source: "os_idle"},
		Sessions: []api.Session{
			{ID: "s_k3m9q2xv7a", Agent: api.AgentClaude, Mode: api.ModeWrapped, Title: "pilothouse", Branch: "main", Cwd: "/home/dev/src/pilothouse",
				State: api.StateBlocked, BlockKind: api.BlockPermission, StateLayer: api.LayerHook, StateDetail: "Asked to run git push",
				StartedAt: ibxT0.Add(-time.Hour), UpdatedAt: ibxT0.Add(-12 * time.Second)},
			{ID: "s_p4t8w6n2rb", Agent: api.AgentCodex, Mode: api.ModeObserved, Title: "webapp", Cwd: "/home/dev/src/webapp",
				State: api.StateBlocked, BlockKind: api.BlockPermission, StartedAt: ibxT0.Add(-30 * time.Minute), UpdatedAt: ibxT0.Add(-time.Minute)},
			{ID: "s_q9r7s5t3uv", Agent: api.AgentClaude, Mode: api.ModeObserved, Title: "site", Cwd: "/home/dev/src/site", Unseen: true,
				State: api.StateWaiting, LastMessage: "Done.\nWant me to add pricing?", StartedAt: ibxT0.Add(-20 * time.Minute), UpdatedAt: ibxT0.Add(-2 * time.Minute)},
			{ID: "s_z0y9x8w7vu", Agent: api.AgentClaude, Mode: api.ModeObserved, Title: "old", Cwd: "/home/dev/src/old",
				State: api.StateExited, StartedAt: ibxT0.Add(-5 * time.Hour), UpdatedAt: exited, ExitedAt: &exited, ExitCode: &exit0},
		},
		Requests: []api.Request{
			{ID: "r_7hq2m4k9pd", SessionID: "s_k3m9q2xv7a", Agent: api.AgentClaude, Checkpoint: api.CheckpointPermission, Tool: "Bash",
				ToolKind: api.ToolShell, Command: "git push origin main", Summary: "Run: git push origin main", Cwd: "/home/dev/src/pilothouse",
				Status: api.RequestOpen, Route: api.RouteDevices, CreatedAt: ibxT0.Add(-12 * time.Second)},
			{ID: "r_b3c5d7e9fg", SessionID: "s_p4t8w6n2rb", Agent: api.AgentCodex, Checkpoint: api.CheckpointPermission, Tool: "apply_patch",
				Paths: []string{"/home/dev/src/webapp/a.ts"}, Status: api.RequestSteppedAside, Route: api.RouteTerminal, CreatedAt: ibxT0.Add(-time.Minute)},
			{ID: "r_raced00001", SessionID: "s_q9r7s5t3uv", Agent: api.AgentClaude, Tool: "Bash", Command: "rm -rf build", Status: api.RequestOpen, CreatedAt: ibxT0.Add(-5 * time.Second)},
			{ID: "r_done000001", SessionID: "s_q9r7s5t3uv", Agent: api.AgentClaude, Tool: "Bash", Command: "ls", Status: api.RequestDecided,
				Decision: &api.Decision{Behavior: api.BehaviorAllow, By: "device:d_phone"}, CreatedAt: ibxT0.Add(-time.Hour)},
		},
	}
}

func ibxEvents() []api.Event {
	mk := func(seq int64, typ api.EventType, sid string, v any) api.Event {
		b, _ := json.Marshal(v)
		return api.Event{Seq: seq, At: ibxT0.Add(time.Duration(seq) * time.Second), Type: typ, SessionID: sid, Data: b}
	}
	s := ibxSnapshot()
	req := s.Requests[0]
	decided := req
	decided.Status = api.RequestDecided
	decided.Decision = &api.Decision{Behavior: api.BehaviorDeny, Message: "open a PR", By: "device:d_phone"}
	return []api.Event{
		mk(10, api.EventSessionStarted, "s_k3m9q2xv7a", s.Sessions[0]),
		mk(11, api.EventHook, "s_k3m9q2xv7a", adapters.HookEvent{Kind: adapters.HookPreTool, VendorEvent: "PreToolUse", Tool: "Bash", Summary: "Run: git push origin main"}),
		mk(12, api.EventRequestOpened, "s_k3m9q2xv7a", req),
		mk(13, api.EventSessionUpdated, "s_k3m9q2xv7a", s.Sessions[0]),
		mk(14, api.EventRequestClosed, "s_k3m9q2xv7a", decided),
		mk(15, api.EventPolicyDecision, "s_p4t8w6n2rb", map[string]string{"rule": "no-force-push", "action": "deny", "outcome": "deny", "summary": "Run: git push -f"}),
		mk(16, api.EventSessionExited, "s_z0y9x8w7vu", s.Sessions[3]),
		mk(17, api.EventPresenceChanged, "", api.Presence{State: api.PresenceAway, Mode: api.PresenceForceAway, Source: "manual"}),
	}
}

func ibxRun(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	return ibxRunCtx(t, context.Background(), stdin, args...)
}

func ibxRunCtx(t *testing.T, ctx context.Context, stdin string, args ...string) (string, error) {
	t.Helper()
	root := newRoot()
	var out bytes.Buffer
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	err := root.ExecuteContext(ctx)
	return out.String(), err
}

func ibxContains(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

func TestInboxLs(t *testing.T) {
	ibxStart(t)
	out, err := ibxRun(t, "", "ls")
	if err != nil {
		t.Fatal(err)
	}
	ibxContains(t, out, "NEEDS YOU (3)", "r_7hq2m4k9pd", "$ git push origin main", "pilothouse approve|deny r_7hq2",
		"in its terminal", "SESSIONS", "s_k3m9q2xv7a", "blocked/permission", "⏎ s_k3m9q2xv7a", "Asked to run git push",
		"⏎ attachable: pilothouse attach s_k3m9",
		"waiting •", "Done. Want me to add pricing?")
	if strings.Contains(out, "s_z0y9x8w7vu") || strings.Contains(out, "r_done000001") {
		t.Fatalf("exited sessions/closed requests must be hidden:\n%s", out)
	}
	out, err = ibxRun(t, "", "ls", "--all")
	if err != nil || !strings.Contains(out, "s_z0y9x8w7vu") {
		t.Fatalf("--all: %v\n%s", err, out)
	}
	out, err = ibxRun(t, "", "ls", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got lsOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(got.Sessions) != 3 || len(got.Requests) != 3 || fmt.Sprint(got.Attachable) != "[s_k3m9q2xv7a]" {
		t.Fatalf("json: %d sessions %d requests attachable %v", len(got.Sessions), len(got.Requests), got.Attachable)
	}

	// Holder gone (crashed or agent exited): no attach hint.
	inboxHolders = func(context.Context) ([]wrap.HolderInfo, error) { return nil, nil }
	out, err = ibxRun(t, "", "ls")
	if err != nil || strings.Contains(out, "⏎") || strings.Contains(out, "pilothouse attach") {
		t.Fatalf("dead holder still attachable: %v\n%s", err, out)
	}
}

func TestInboxApproveDeny(t *testing.T) {
	d := ibxStart(t)
	out, err := ibxRun(t, "", "approve", "r_7hq")
	if err != nil {
		t.Fatal(err)
	}
	ibxContains(t, out, "✓ Allowed r_7hq2m4k9pd once: $ git push origin main")
	out, err = ibxRun(t, "", "approve", "s_k3m", "--session")
	if err != nil {
		t.Fatal(err)
	}
	ibxContains(t, out, "for the rest of this session")
	out, err = ibxRun(t, "", "deny", "r_7hq", "-m", "open a PR instead")
	if err != nil {
		t.Fatal(err)
	}
	ibxContains(t, out, "✗ Denied r_7hq2m4k9pd", "The agent was told: open a PR instead")

	d.mu.Lock()
	defer d.mu.Unlock()
	want := []ibxDecideCall{
		{"r_7hq2m4k9pd", api.DecideBody{Behavior: api.BehaviorAllow, Scope: api.ScopeOnce}, "cli"},
		{"r_7hq2m4k9pd", api.DecideBody{Behavior: api.BehaviorAllow, Scope: api.ScopeSession}, "cli"},
		{"r_7hq2m4k9pd", api.DecideBody{Behavior: api.BehaviorDeny, Message: "open a PR instead"}, "cli"},
	}
	if fmt.Sprint(d.decides) != fmt.Sprint(want) {
		t.Fatalf("decides:\n%v\nwant\n%v", d.decides, want)
	}
}

func TestInboxDecideErrors(t *testing.T) {
	d := ibxStart(t)
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"raced", []string{"approve", "r_raced"}, "already answered (the first answer wins): request already closed by device:d_phone"},
		{"stepped aside", []string{"approve", "r_b3c"}, "showing in the agent's own permission dialog — answer it in the terminal"},
		{"closed", []string{"deny", "r_done"}, "already closed (decided by device:d_phone)"},
		{"unknown", []string{"approve", "r_nope"}, `nothing matches "r_nope"`},
		{"session without request", []string{"approve", "s_z0y"}, "has no request waiting"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ibxRun(t, "", tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
	d.mu.Lock()
	d.refuse = true
	d.mu.Unlock()
	_, err := ibxRun(t, "", "approve", "r_7hq")
	if err == nil || !strings.Contains(err.Error(), "agents can't approve themselves") {
		t.Fatalf("403: %v", err)
	}
}

func TestInboxDaemonDown(t *testing.T) {
	ibxEnv(t) // no daemon listening
	for _, args := range [][]string{{"ls"}, {"approve", "r_1"}, {"deny", "r_1"}, {"log"}, {"explain", "s_1"}, {"here"}, {"notify", "test"}} {
		_, err := ibxRun(t, "", args...)
		if err == nil || !strings.Contains(err.Error(), "isn't running — start it with `pilothouse start`") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
}

func TestInboxLog(t *testing.T) {
	ibxStart(t)
	out, err := ibxRun(t, "", "log")
	if err != nil {
		t.Fatal(err)
	}
	ibxContains(t, out,
		"15:04:15  s_k3m9q2  ▶ claude started in ~/src/pilothouse (wrapped)",
		"15:04:16  s_k3m9q2  ↪ PreToolUse: Run: git push origin main",
		"? $ git push origin main — held for your devices",
		"· blocked (permission): Asked to run git push",
		"✗ denied by device:d_phone: $ git push origin main — “open a PR”",
		"⚖ policy rule=no-force-push action=deny outcome=deny summary=Run: git push -f",
		"■ exited with code 0",
		"-         ◐ presence: away (away, manual)",
	)
	out, err = ibxRun(t, "", "log", "--json", "-n", "2")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("-n 2 --json: %d lines\n%s", len(lines), out)
	}
	var ev api.Event
	if err := json.Unmarshal([]byte(lines[1]), &ev); err != nil || ev.Seq != 17 {
		t.Fatalf("ndjson: %v %+v", err, ev)
	}
	out, err = ibxRun(t, "", "log", "--session", "s_k3m")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "s_z0y9x8") || !strings.Contains(out, "▶ claude started") {
		t.Fatalf("--session:\n%s", out)
	}
	if i, j := strings.Index(out, "▶"), strings.Index(out, "✗ denied"); i > j {
		t.Fatalf("--session must print oldest first:\n%s", out)
	}
}

func TestInboxLogFollow(t *testing.T) {
	ibxStart(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := newRoot()
	var mu sync.Mutex
	var buf bytes.Buffer
	w := ibxLockedWriter{mu: &mu, b: &buf}
	root.SetArgs([]string{"log", "-f", "-n", "1"})
	root.SetOut(w)
	root.SetErr(w)
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		s := buf.String()
		mu.Unlock()
		if strings.Contains(s, "presence: away (auto, os_idle, idle 2m00s)") {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("no live event:\n%s", s)
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("log -f returned %v after cancel", err)
	}
}

type ibxLockedWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w ibxLockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func TestInboxExplain(t *testing.T) {
	ibxStart(t)
	out, err := ibxRun(t, "", "explain", "s_k3m")
	if err != nil {
		t.Fatal(err)
	}
	ibxContains(t, out, "s_k3m9q2xv7a  claude · pilothouse · main (wrapped)", "because  2: an open permission request exists",
		"EVIDENCE", "*  12s ago", "L1-osc", "OSC 7501 working")
	if _, err := ibxRun(t, "", "explain", "s_nope"); err == nil || !strings.Contains(err.Error(), "nothing matches") {
		t.Fatalf("unknown session: %v", err)
	}
	out, err = ibxRun(t, "", "explain", "s_k3m", "--json")
	if err != nil || !json.Valid([]byte(out)) {
		t.Fatalf("json: %v %s", err, out)
	}
}

func TestInboxPresence(t *testing.T) {
	d := ibxStart(t)
	cases := map[string]string{
		"here": "Presence: here (until you run `pilothouse auto`). Permission requests show in the agent's own dialog.",
		"away": "Presence: away (until you run `pilothouse auto`). Permission requests are held and sent to your paired devices.",
		"auto": "Presence: auto — now here (idle 4s).",
	}
	for _, mode := range []string{"here", "away", "auto"} {
		out, err := ibxRun(t, "", mode)
		if err != nil {
			t.Fatal(err)
		}
		ibxContains(t, out, cases[mode])
	}
	d.mu.Lock()
	got := strings.Join(d.presence, ",")
	d.refuse = true
	d.mu.Unlock()
	if got != "here|cli,away|cli,auto|cli" {
		t.Fatalf("presence calls = %s", got)
	}
	if _, err := ibxRun(t, "", "away"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("403: %v", err)
	}
}

func TestInboxVersionAndNotify(t *testing.T) {
	d := ibxStart(t)
	out, err := ibxRun(t, "", "version", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var v versionInfo
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.Protocol != api.ProtocolVersion || v.Version != version.Version || v.Go == "" || v.OS == "" {
		t.Fatalf("version: %v %+v", err, v)
	}
	out, _ = ibxRun(t, "", "version")
	ibxContains(t, out, "pilothouse "+version.Version, "protocol 1")

	out, err = ibxRun(t, "", "notify", "test")
	if err != nil {
		t.Fatal(err)
	}
	ibxContains(t, out, "✓ Test notification sent to: desktop")
	if d.notified != 1 {
		t.Fatal("notify test not sent")
	}
}

func TestInboxDefaultCommand(t *testing.T) {
	ibxStart(t)
	out, err := ibxRun(t, "")
	if err != nil || !strings.Contains(out, "Usage:") {
		t.Fatalf("non-TTY default must print help: %v\n%s", err, out)
	}
	var got tui.Options
	inboxIsTerminal = func() bool { return true }
	inboxRunTUI = func(_ context.Context, o tui.Options) error { got = o; return nil }
	if _, err := ibxRun(t, ""); err != nil {
		t.Fatal(err)
	}
	if got.Backend == nil || got.Exe != "/opt/pilothouse/bin/pilothouse" {
		t.Fatalf("tui options = %+v", got)
	}
	inboxIsTerminal = func() bool { return false }
	if _, err := ibxRun(t, "", "inbox"); err == nil || !strings.Contains(err.Error(), "pilothouse ls") {
		t.Fatalf("inbox without a TTY: %v", err)
	}
}

// ---------------------------------------------------------------- service, doctor, setup fakes

type ibxService struct {
	mu        sync.Mutex
	kind      string
	calls     []string
	installed bool
	running   bool
	linger    *bool
}

func (s *ibxService) rec(c string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, c)
}
func (s *ibxService) Kind() string { return s.kind }
func (s *ibxService) Path() string {
	return "/home/dev/Library/LaunchAgents/dev.pilothouse.daemon.plist"
}
func (s *ibxService) Install(context.Context) (service.Result, error) {
	s.rec("install")
	changed := !s.installed
	s.installed, s.running = true, true
	res := service.Result{Path: s.Path(), Changed: changed}
	if changed {
		res.Actions = []string{"wrote " + s.Path(), "loaded dev.pilothouse.daemon (starts at login)"}
	} else {
		res.Actions = []string{"already loaded dev.pilothouse.daemon"}
	}
	return res, nil
}
func (s *ibxService) Uninstall(context.Context) (service.Result, error) {
	s.rec("uninstall")
	s.installed, s.running = false, false
	return service.Result{Path: s.Path(), Changed: true, Actions: []string{"removed " + s.Path()}}, nil
}
func (s *ibxService) Status(context.Context) (service.Status, error) {
	s.rec("status")
	return service.Status{Kind: s.kind, Path: s.Path(), Installed: s.installed, Current: s.installed, Loaded: s.installed,
		Running: s.running, PID: 4242, Linger: s.linger}, nil
}
func (s *ibxService) Start(context.Context) error   { s.rec("start"); s.running = true; return nil }
func (s *ibxService) Stop(context.Context) error    { s.rec("stop"); s.running = false; return nil }
func (s *ibxService) Restart(context.Context) error { s.rec("restart"); return nil }

type ibxDetector struct{ none bool }

func (d ibxDetector) Idle() (time.Duration, bool) {
	if d.none {
		return 0, false
	}
	return 3 * time.Second, true
}
func (d ibxDetector) Source() string {
	if d.none {
		return presence.SourceNone
	}
	return presence.SourceMacHID
}

type ibxAdapter struct {
	mu        sync.Mutex
	kind      api.AgentKind
	name      string
	installed bool // agent present on the machine
	hooks     bool // our hooks installed
	installs  []adapters.InstallOptions
	path      string // settings file Status reports; default /home/dev/.claude/settings.json
}

func (a *ibxAdapter) Kind() api.AgentKind      { return a.kind }
func (a *ibxAdapter) DisplayName() string      { return a.name }
func (a *ibxAdapter) ProcessNames() []string   { return []string{string(a.kind)} }
func (a *ibxAdapter) ProtectedPaths() []string { return nil }
func (a *ibxAdapter) ResumeArgs(string) []string {
	return nil
}
func (a *ibxAdapter) Detect() adapters.Detection {
	return adapters.Detection{Installed: a.installed, Version: "2.1.0", SettingsPath: "/home/dev/.claude/settings.json"}
}
func (a *ibxAdapter) ParseHook(adapters.HookEnvelope) (adapters.HookEvent, error) {
	return adapters.HookEvent{}, nil
}
func (a *ibxAdapter) EncodeResponse(adapters.HookEvent, adapters.HookOutcome) adapters.HookResponse {
	return adapters.HookResponse{}
}
func (a *ibxAdapter) FailResponse(adapters.HookKind, bool) adapters.HookResponse {
	return adapters.HookResponse{}
}
func (a *ibxAdapter) MaxHold(adapters.HookKind) time.Duration { return 0 }
func (a *ibxAdapter) InstallHooks(o adapters.InstallOptions) (adapters.Change, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.installs = append(a.installs, o)
	ch := adapters.Change{Path: "/home/dev/.claude/settings.json", Changed: !a.hooks, Summary: "add hooks for 9 events"}
	if !o.DryRun {
		a.hooks = true
		ch.Backup = "/home/dev/.claude/settings.json.pilothouse-bak-1"
	}
	return ch, nil
}
func (a *ibxAdapter) UninstallHooks(adapters.InstallOptions) (adapters.Change, error) {
	return adapters.Change{}, nil
}
func (a *ibxAdapter) Status(adapters.InstallOptions) (adapters.HookStatus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.path
	if p == "" {
		p = "/home/dev/.claude/settings.json"
	}
	return adapters.HookStatus{Installed: a.hooks, Current: a.hooks, Events: make([]string, 9), Path: p}, nil
}

func TestInboxService(t *testing.T) {
	ibxStart(t)
	svc := &ibxService{kind: "launchd"}
	inboxServiceMgr = func(exe string) serviceManager {
		if exe != "/opt/pilothouse/bin/pilothouse" {
			t.Errorf("service exe = %s", exe)
		}
		return svc
	}
	out, err := ibxRun(t, "", "service", "install")
	if err != nil {
		t.Fatal(err)
	}
	ibxContains(t, out, "✓ wrote /home/dev/Library/LaunchAgents/dev.pilothouse.daemon.plist", "✓ daemon running (pid 4242")
	out, err = ibxRun(t, "", "service", "status")
	if err != nil {
		t.Fatal(err)
	}
	ibxContains(t, out, "manager    launchd", "installed  yes", "running    yes", "pid        4242")
	for _, sub := range []string{"stop", "start", "restart", "uninstall"} {
		if _, err := ibxRun(t, "", "service", sub); err != nil {
			t.Fatal(sub, err)
		}
	}
	if got := strings.Join(svc.calls, ","); got != "install,status,stop,start,restart,uninstall" {
		t.Fatalf("calls = %s", got)
	}
	out, err = ibxRun(t, "", "service", "status", "--json")
	if err != nil || !json.Valid([]byte(out)) {
		t.Fatalf("status --json: %v %s", err, out)
	}

	svc.kind = ""
	if _, err := ibxRun(t, "", "service", "install"); err == nil || !strings.Contains(err.Error(), "pilothouse start") {
		t.Fatalf("unsupported: %v", err)
	}
	inboxExecutable = func() (string, error) { return "/tmp/go-build123/b001/exe/pilothouse", nil }
	if _, err := ibxRun(t, "", "service", "install"); err == nil || !strings.Contains(err.Error(), "go run") {
		t.Fatalf("go-run binary must be refused: %v", err)
	}
}

func TestInboxDoctor(t *testing.T) {
	d := ibxStart(t)
	claude := &ibxAdapter{kind: api.AgentClaude, name: "Claude Code", installed: true, hooks: true}
	codex := &ibxAdapter{kind: api.AgentCodex, name: "Codex", installed: false}
	inboxAdapters = func() []adapters.Adapter { return []adapters.Adapter{claude, nil, codex} }
	svc := &ibxService{kind: "launchd", installed: true, running: true}
	inboxServiceMgr = func(string) serviceManager { return svc }
	d.policy = api.PolicyStatus{Rules: 7, Files: []api.PolicyFile{{Path: "/x/policy.yaml", Scope: "user", Rules: 4}, {Scope: "builtin", Rules: 3}}}
	d.devices = []api.Device{{ID: "d_phone", Name: "iPhone"}}

	out, err := ibxRun(t, "", "doctor")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	ibxContains(t, out,
		"✓ binary", "/opt/pilothouse/bin/pilothouse",
		"✓ daemon", "running (pid 4242",
		"✓ socket", "(0600)",
		"✓ hooks: Claude Code", "9 events wired",
		"· hooks: Codex", "agent not found",
		"✓ service", "launchd, running",
		"✓ presence", "macos_hid works",
		"✓ notifications", "1 paired device(s)",
		"· remote", "pilothouse remote tailscale",
		"✓ policy", "7 rules from 1 file(s)",
		"Everything looks good.",
	)

	// Broken things: hooks missing, policy error, no idle source.
	claude.hooks = false
	d.mu.Lock()
	d.policy.Errors = []string{"/x/policy.yaml: line 3: unknown action \"block\""}
	d.mu.Unlock()
	inboxDetector = func() presence.Detector { return ibxDetector{none: true} }
	out, err = ibxRun(t, "", "doctor")
	var ee ExitError
	if !errors.As(err, &ee) || ee.Code != 1 {
		t.Fatalf("doctor must exit 1 on problems: %v", err)
	}
	ibxContains(t, out, "✗ hooks: Claude Code", "→ `pilothouse setup` or `pilothouse hooks install`",
		"✗ policy", "unknown action", "! presence", "pilothouse away", "2 problem(s)")

	out, _ = ibxRun(t, "", "doctor", "--json")
	var checks []doctorCheck
	if err := json.Unmarshal([]byte(out), &checks); err != nil || len(checks) < 8 {
		t.Fatalf("json: %v\n%s", err, out)
	}
}

// A shell that prints on `$SHELL -c true` corrupts Codex hook replies;
// doctor warns when Codex is installed.
func TestInboxDoctorShellNoise(t *testing.T) {
	ibxStart(t)
	codex := &ibxAdapter{kind: api.AgentCodex, name: "Codex", installed: true, hooks: true}
	inboxAdapters = func() []adapters.Adapter { return []adapters.Adapter{codex} }
	out, _ := ibxRun(t, "", "doctor")
	ibxContains(t, out, "✓ shell", "/bin/zsh -c true` prints nothing")

	inboxShellNoise = func(context.Context) (string, string, error) { return "/bin/zsh", "Welcome back!", nil }
	out, _ = ibxRun(t, "", "doctor")
	ibxContains(t, out, "! shell", `prints "Welcome back!"`, "Codex can't read hook replies", "~/.zshenv")

	// Without Codex the check is skipped.
	codex.installed = false
	out, _ = ibxRun(t, "", "doctor")
	if strings.Contains(out, "shell") {
		t.Fatalf("shell check without Codex:\n%s", out)
	}
}

// The real probe: a shell whose startup prints is caught.
func TestShellNoiseProbe(t *testing.T) {
	dir := t.TempDir()
	sh := filepath.Join(dir, "noisysh")
	if err := os.WriteFile(sh, []byte("#!/bin/sh\necho hello from zshenv\nexec /bin/sh \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", sh)
	if _, noise, err := shellNoise(context.Background()); err != nil || noise != "hello from zshenv" {
		t.Fatalf("noise=%q err=%v", noise, err)
	}
	t.Setenv("SHELL", "/bin/sh")
	if _, noise, err := shellNoise(context.Background()); err != nil || noise != "" {
		t.Fatalf("quiet shell: noise=%q err=%v", noise, err)
	}
}

// Hooks that call a missing binary or a versioned Homebrew Cellar path are
// reported as stale.
func TestInboxDoctorStaleHooks(t *testing.T) {
	ibxStart(t)
	dir := t.TempDir()
	cellar := filepath.Join(dir, "homebrew", "Cellar", "pilothouse", "0.1.1", "bin", "pilothouse")
	if err := os.MkdirAll(filepath.Dir(cellar), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cellar, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(dir, "settings.json")
	write := func(bin string) {
		body := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"'` + bin + `' hook claude"}]}]}}`
		if err := os.WriteFile(settings, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	claude := &ibxAdapter{kind: api.AgentClaude, name: "Claude Code", installed: true, hooks: true, path: settings}
	inboxAdapters = func() []adapters.Adapter { return []adapters.Adapter{claude} }

	write(filepath.Join(dir, "gone", "pilothouse"))
	out, err := ibxRun(t, "", "doctor")
	if err == nil {
		t.Fatalf("missing hook binary must fail doctor:\n%s", out)
	}
	ibxContains(t, out, "✗ hooks: Claude Code", "stale", "doesn't exist")

	write(cellar)
	out, _ = ibxRun(t, "", "doctor")
	ibxContains(t, out, "! hooks: Claude Code", "stale", "versioned Homebrew path")
}

// Setup records the stable (unresolved) binary path, e.g. Homebrew's
// bin/pilothouse symlink into the Cellar; doctor must not call that a
// different binary from the one on PATH.
func TestSamePathResolvesSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "Cellar", "pilothouse")
	link := filepath.Join(dir, "bin", "pilothouse")
	for _, d := range []string{filepath.Dir(real), filepath.Dir(link)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(real, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(other, []byte("y"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !samePath(link, real) || !samePath(real, link) || !samePath(link, link) {
		t.Fatal("symlink and target must be the same binary")
	}
	if samePath(link, other) || samePath("/opt/a/pilothouse", "/opt/b/pilothouse") {
		t.Fatal("different binaries reported as the same")
	}
}

// A different pilothouse first on PATH is still reported.
func TestInboxDoctorOtherBinaryOnPath(t *testing.T) {
	ibxStart(t)
	inboxLookPath = func(n string) (string, error) { return "/usr/local/bin/" + n, nil }
	out, _ := ibxRun(t, "", "doctor")
	ibxContains(t, out, "! binary", "on your PATH is /usr/local/bin/pilothouse")
}

func TestInboxDoctorDaemonDown(t *testing.T) {
	ibxEnv(t)
	out, err := ibxRun(t, "", "doctor")
	if err == nil {
		t.Fatal("want exit 1")
	}
	ibxContains(t, out, "✗ daemon", "not running", "→ `pilothouse start`")
}

func TestInboxSetup(t *testing.T) {
	d := ibxStart(t)
	claude := &ibxAdapter{kind: api.AgentClaude, name: "Claude Code", installed: true}
	codex := &ibxAdapter{kind: api.AgentCodex, name: "Codex", installed: false}
	inboxAdapters = func() []adapters.Adapter { return []adapters.Adapter{claude, codex} }
	svc := &ibxService{kind: "launchd"}
	inboxServiceMgr = func(string) serviceManager { return svc }

	// Without a terminal and without --yes, setup changes nothing.
	if _, err := ibxRun(t, "", "setup"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("non-interactive: %v", err)
	}
	if len(claude.installs) != 0 {
		t.Fatal("nothing may be installed without consent")
	}

	out, err := ibxRun(t, "", "setup", "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	ibxContains(t, out, "binary  /opt/pilothouse/bin/pilothouse", "• Claude Code 2.1.0  add hooks for 9 events",
		"· Codex", "not found", "? Install Pilothouse hooks for Claude Code? [Y/n] yes",
		"backup: ~/.claude/settings.json.pilothouse-bak-1", "✓ loaded dev.pilothouse.daemon", "✓ daemon running (pid 4242",
		"✓ test notification sent", "pilothouse policy init", "pilothouse pair")
	if len(claude.installs) != 2 || !claude.installs[0].DryRun || claude.installs[1].DryRun || claude.installs[1].ShimPath != "/opt/pilothouse/bin/pilothouse" {
		t.Fatalf("installs = %+v", claude.installs)
	}
	if d.notified != 1 {
		t.Fatal("test notification not sent")
	}

	// Idempotent: second run changes nothing.
	out, err = ibxRun(t, "", "setup", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	ibxContains(t, out, "✓ Claude Code 2.1.0  hooks installed and current", "already loaded")
	if len(claude.installs) != 2 {
		t.Fatalf("second run reinstalled hooks: %+v", claude.installs)
	}

	// Interactive "no" answers skip both steps.
	claude.hooks = false
	svc.installed = false
	inboxStdinTerminal = func() bool { return true }
	out, err = ibxRun(t, "n\nn\n", "setup")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	ibxContains(t, out, "hooks skipped", "login service skipped")
	if len(claude.installs) != 3 { // just the dry run
		t.Fatalf("installs = %d", len(claude.installs))
	}
}

func TestInboxSetupNoDaemon(t *testing.T) {
	ibxEnv(t)
	started := false
	inboxStartDaemon = func(context.Context, string) error { started = true; return nil }
	out, err := ibxRun(t, "", "setup", "--yes")
	if !started {
		t.Fatal("without a service manager setup must fall back to `pilothouse start`")
	}
	var ee ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v", err)
	}
	ibxContains(t, out, "No supported agent found yet", "the daemon isn't answering", "pilothouse doctor")
}

func TestFormatEventLineFallback(t *testing.T) {
	inboxLocation = time.UTC
	got := FormatEventLine(api.Event{Seq: 1, At: ibxT0, Type: api.EventDeviceChanged, Data: json.RawMessage(`{"id":"d_1"}`)})
	if got != `15:04:05  -         device.changed {"id":"d_1"}` {
		t.Fatalf("got %q", got)
	}
}

// ---------------------------------------------------------------- terminal queries

var (
	ibxBinOnce sync.Once
	ibxBin     string
	ibxBinErr  error
)

// ibxBinary builds cmd/pilothouse once for the PTY regression tests.
func ibxBinary(t *testing.T) string {
	t.Helper()
	ibxBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "ph-ibx")
		if err != nil {
			ibxBinErr = err
			return
		}
		ibxBin = filepath.Join(dir, "pilothouse")
		out, err := exec.Command("go", "build", "-o", ibxBin, "github.com/xasdta/pilothouse/cmd/pilothouse").CombinedOutput()
		if err != nil {
			ibxBinErr = fmt.Errorf("%v\n%s", err, out)
		}
	})
	if ibxBinErr != nil {
		t.Fatalf("build: %v", ibxBinErr)
	}
	return ibxBin
}

// TestNoTerminalQueries runs the real binary in a pseudo-terminal that
// never answers (like a dumb or remote terminal). Only the inbox may ask
// the terminal anything: a query here would stall every command (termenv
// waits up to 5 s) and a late reply would leak into the next program's
// input — fatal for the hook shim, whose stdout is the agent's protocol.
func TestNoTerminalQueries(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := ibxBinary(t)
	// Exec once untimed: macOS verifies a new binary on its first run,
	// which is not what this test measures.
	_ = exec.Command(bin, "version").Run()
	run, err := os.MkdirTemp("/tmp", "ph")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(run)
	home := t.TempDir()
	env := append(os.Environ(),
		"PILOTHOUSE_HOME="+filepath.Join(home, "ph"),
		"PILOTHOUSE_USER_HOME="+home,
		"PILOTHOUSE_RUN_DIR="+run, // no daemon listens here
		"PILOTHOUSE_NO_UPDATE_CHECK=1",
		"TERM=xterm-256color", "COLORTERM=truecolor",
	)
	payload := `{"session_id":"x","hook_event_name":"SessionStart","cwd":"/tmp","source":"startup"}`
	tests := []struct {
		name  string
		args  []string
		stdin string // "" = the PTY itself is stdin
	}{
		{"version", []string{"version"}, ""},
		{"hook claude", []string{"hook", "claude"}, payload},
		{"ls (daemon down)", []string{"ls"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := exec.Command(bin, tt.args...)
			c.Env = env
			if tt.stdin != "" {
				// Agents pipe the payload in; stdout/stderr are still the terminal.
				c.Stdin = strings.NewReader(tt.stdin)
			}
			// The PTY is the controlling terminal even when stdin is a pipe
			// (Ctty is the child's fd 1), as for a hook run by an agent.
			attrs := &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 1}
			start := time.Now()
			ptmx, err := pty.StartWithAttrs(c, &pty.Winsize{Cols: 100, Rows: 30}, attrs)
			if err != nil {
				t.Fatal(err)
			}
			defer ptmx.Close()
			var out bytes.Buffer
			drained := make(chan struct{})
			go func() { _, _ = io.Copy(&out, ptmx); close(drained) }() // read, never answer
			done := make(chan error, 1)
			go func() { done <- c.Wait() }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				_ = c.Process.Kill()
				t.Fatal("command hung")
			}
			elapsed := time.Since(start)
			ptmx.Close()
			<-drained
			if elapsed > 500*time.Millisecond {
				t.Errorf("took %v in a non-answering terminal (limit 500ms)", elapsed)
			}
			if q := ibxQueries(out.Bytes()); q != "" {
				t.Errorf("queried the terminal: %q\noutput: %q", q, out.String())
			}
		})
	}
}

// ibxQueries returns the first terminal query sequence in b: OSC colour
// queries (ESC ] 1x ; ?), cursor position (CSI 6n) or device attributes
// (CSI c / CSI > c).
func ibxQueries(b []byte) string {
	s := string(b)
	for _, q := range []string{"\x1b]10;?", "\x1b]11;?", "\x1b]12;?", "\x1b[6n", "\x1b[c", "\x1b[>c", "\x1b[0c"} {
		if strings.Contains(s, q) {
			return q
		}
	}
	return ""
}

func TestNotifyChannelsOnlyListsWhatCanDeliver(t *testing.T) {
	saved := desktopLookPath
	desktopLookPath = func(string) (string, error) { return "", errors.New("not installed") }
	t.Cleanup(func() { desktopLookPath = saved })
	home := t.TempDir()
	t.Setenv("PILOTHOUSE_HOME", home)
	t.Setenv("PILOTHOUSE_USER_HOME", t.TempDir())
	run, err := os.MkdirTemp("/tmp", "ag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(run) })
	t.Setenv("PILOTHOUSE_RUN_DIR", run) // no daemon: no paired devices to count
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("notify:\n  desktop: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ch := notifyChannels(context.Background()); len(ch) != 0 {
		t.Fatalf("no desktop, no devices, no ntfy: want no channels, got %v", ch)
	}
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("notify:\n  desktop: false\n  ntfy:\n    topic: t1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ch := notifyChannels(context.Background()); len(ch) != 1 || ch[0] != "ntfy topic t1" {
		t.Fatalf("want only ntfy, got %v", ch)
	}
}

// Tests that expect a desktop channel must not depend on the machine having
// notify-send (CI runners don't).
func init() {
	desktopLookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
}
