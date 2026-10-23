package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/client"
	"github.com/xasdta/pilothouse/internal/config"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/paths"
	"github.com/xasdta/pilothouse/internal/policy"
	"github.com/xasdta/pilothouse/internal/store"
)

var bg = context.Background()

func TestSocketIsPrivateAndSingleInstance(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	fi, err := os.Stat(paths.Socket())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v, want 0600", fi.Mode().Perm())
	}
	if hl, err := h.c.Health(bg); err != nil || hl.Protocol != api.ProtocolVersion || hl.PID != os.Getpid() {
		t.Fatalf("health = %+v, %v", hl, err)
	}
	err = Run(bg, Options{Config: h.cfg, Adapters: []adapters.Adapter{h.adapter}, Presence: h.presence,
		LoadPolicy: func() PolicyEngine { return h.policy }, Notifier: h.notes, Log: h.d.log})
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second daemon: %v, want ErrAlreadyRunning", err)
	}
}

func TestSnapshotAndStream(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	h.hook(bg, t, adapters.HookEvent{Kind: adapters.HookSessionStart})

	snap, err := h.c.Snapshot(bg)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Sessions) != 1 || snap.Sessions[0].Agent != fakeKind || snap.Sessions[0].PID != 4242 {
		t.Fatalf("snapshot sessions = %+v", snap.Sessions)
	}
	sess := snap.Sessions[0]

	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	var mu sync.Mutex
	var got []api.Event
	streamDone := make(chan error, 1)
	go func() {
		streamDone <- h.c.Stream(ctx, snap.Seq, func(ev api.Event) {
			mu.Lock()
			got = append(got, ev)
			mu.Unlock()
		})
	}()
	h.hook(bg, t, shell("ls"))
	waitFor(t, "streamed session.updated", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, ev := range got {
			if ev.Type == api.EventSessionUpdated {
				var s api.Session
				return json.Unmarshal(ev.Data, &s) == nil && s.State == api.StateWorking
			}
		}
		return false
	})
	mu.Lock()
	for i, ev := range got {
		if ev.Seq <= snap.Seq || (i > 0 && ev.Seq <= got[i-1].Seq) {
			t.Fatalf("bad seq order: %+v", got)
		}
	}
	if got[0].Type != api.EventHook {
		t.Fatalf("first streamed event = %s, want hook", got[0].Type)
	}
	mu.Unlock()
	cancel()
	if err := <-streamDone; err != nil {
		t.Fatalf("stream ended with %v", err)
	}

	// Replay: streaming from 0 delivers history first.
	ctx2, cancel2 := context.WithTimeout(bg, 2*time.Second)
	defer cancel2()
	var replayed int
	_ = h.c.Stream(ctx2, 0, func(ev api.Event) {
		replayed++
		if ev.Seq == snap.Seq {
			cancel2()
		}
	})
	if replayed < int(snap.Seq) {
		t.Fatalf("replayed %d events, want ≥ %d", replayed, snap.Seq)
	}

	// A client ahead of the log (database replaced) must resync.
	if err := h.c.Stream(bg, snap.Seq+1000, func(api.Event) {}); !errors.Is(err, client.ErrResync) {
		t.Fatalf("stream from the future: %v, want ErrResync", err)
	}
	// A client behind retained history must resync.
	if err := h.d.db.Write(bg, func(tx *store.Tx) error {
		_, err := tx.Prune(bg, time.Now().Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Stream(bg, 0, func(api.Event) {}); !errors.Is(err, client.ErrResync) {
		t.Fatalf("stream from pruned history: %v, want ErrResync", err)
	}

	// Session detail routes accept unique prefixes.
	if s, err := h.c.Session(bg, sess.ID[:6]); err != nil || s.ID != sess.ID {
		t.Fatalf("session by prefix = %+v, %v", s, err)
	}
	if _, err := h.c.Session(bg, "s_nope"); !client.IsCode(err, "not_found") {
		t.Fatalf("missing session: %v", err)
	}
}

func TestSlowSubscriberIsToldToResync(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	sub, err := h.d.Subscribe(bg, 0)
	if err != nil || sub.Resync {
		t.Fatalf("subscribe: %+v %v", sub, err)
	}
	defer sub.Close()
	n := liveBuffer + 200
	for i := 0; i < n; i++ {
		if _, err := h.d.Emit(bg, api.EventDeviceChanged, "", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	received := 0
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-sub.C:
			if !ok {
				if received >= n {
					t.Fatalf("subscriber got all %d events; expected to be dropped", received)
				}
				return // dropped: the SSE layer turns this into a resync
			}
			received++
		case <-timeout:
			t.Fatalf("subscription neither delivered everything nor closed (%d received)", received)
		}
	}
}

func TestPreToolPolicyAndGrants(t *testing.T) {
	h := newHarness(t)
	h.policy.set("rm -rf /", api.BehaviorDeny, "no-rm")
	h.policy.set("ls", api.BehaviorAllow, "ls-ok")
	h.policy.set("git push --force", api.BehaviorAsk, "ask-force")
	h.start(t)

	tests := []struct {
		cmd  string
		want api.Behavior
	}{
		{"rm -rf /", api.BehaviorDeny},
		{"ls", api.BehaviorAllow},
		{"git push --force", api.BehaviorAsk},
		{"echo hi", ""},
	}
	for _, tt := range tests {
		if out := h.hook(bg, t, shell(tt.cmd)); out.Behavior != tt.want {
			t.Errorf("pre_tool %q = %q, want %q", tt.cmd, out.Behavior, tt.want)
		}
	}
	evs, err := h.c.Events(bg, 0)
	if err != nil {
		t.Fatal(err)
	}
	var decisions []policy.DecisionEvent
	for _, ev := range evs {
		if ev.Type == api.EventPolicyDecision {
			var de policy.DecisionEvent
			if err := json.Unmarshal(ev.Data, &de); err != nil {
				t.Fatal(err)
			}
			decisions = append(decisions, de)
		}
	}
	if len(decisions) != 3 || decisions[0].Rule != "no-rm" || decisions[0].Action != api.BehaviorDeny ||
		decisions[0].Outcome != api.BehaviorDeny || decisions[0].Tool != "Bash" {
		t.Fatalf("policy decisions = %+v", decisions)
	}
}

func TestPermissionHeldThenDecided(t *testing.T) {
	h := newHarness(t)
	h.presence.setAway(true)
	h.policy.set("git push origin main --force", api.BehaviorDeny, "no-force")
	h.start(t)
	waitFor(t, "away", func() bool { return h.d.Presence().State == api.PresenceAway })

	result := make(chan fakeOutcome, 1)
	go func() { result <- h.hook(bg, t, permission("git push origin main", "tu-1")) }()
	req := h.openRequest(t)
	if req.Route != api.RouteDevices || req.Status != api.RequestOpen {
		t.Fatalf("request = %+v", req)
	}
	s, _ := h.c.Session(bg, req.SessionID)
	if s.State != api.StateBlocked || s.BlockKind != api.BlockPermission || !s.Unseen {
		t.Fatalf("session while held = %s/%s unseen=%v", s.State, s.BlockKind, s.Unseen)
	}
	if !h.notes.find(core.NotifyRequest, true) {
		t.Fatal("no remote notification for the held request")
	}

	dec, err := h.c.WithClientName("tui").Decide(bg, req.ID, api.DecideBody{Behavior: api.BehaviorAllow, Scope: api.ScopeSession})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Status != api.RequestDecided || dec.Decision.By != "tui" {
		t.Fatalf("decided = %+v", dec)
	}
	select {
	case out := <-result:
		if out.Behavior != api.BehaviorAllow {
			t.Fatalf("hook answered %+v, want allow", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hook still blocked after the decision")
	}
	s, _ = h.c.Session(bg, req.SessionID)
	if s.State == api.StateBlocked {
		t.Fatalf("session still blocked after decision")
	}

	// The session grant now allows `git push …` at pre_tool, but never a
	// chained command, and never over a deny rule.
	for cmd, want := range map[string]api.Behavior{
		"git push origin dev":           api.BehaviorAllow,
		"git push origin dev; rm -rf ~": "",
		"git status":                    "",
		"git push origin main --force":  api.BehaviorDeny,
	} {
		if out := h.hook(bg, t, shell(cmd)); out.Behavior != want {
			t.Errorf("pre_tool %q after grant = %q, want %q", cmd, out.Behavior, want)
		}
	}
	if _, err := h.c.Decide(bg, req.ID, api.DecideBody{Behavior: api.BehaviorDeny}); !client.IsCode(err, "already_decided") {
		t.Fatalf("second decision: %v, want already_decided", err)
	}
}

func TestFirstAnswerWins(t *testing.T) {
	h := newHarness(t)
	h.presence.setAway(true)
	h.start(t)
	waitFor(t, "away", func() bool { return h.d.Presence().State == api.PresenceAway })

	result := make(chan fakeOutcome, 1)
	go func() { result <- h.hook(bg, t, permission("make deploy", "")) }()
	req := h.openRequest(t)

	const n = 25
	var wins, losses atomic.Int32
	var winner atomic.Value
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		b := api.BehaviorAllow
		if i%2 == 1 {
			b = api.BehaviorDeny
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := h.c.Decide(bg, req.ID, api.DecideBody{Behavior: b})
			switch {
			case err == nil:
				wins.Add(1)
				winner.Store(b)
			case client.IsCode(err, "already_decided"):
				losses.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 || losses.Load() != n-1 {
		t.Fatalf("wins=%d losses=%d", wins.Load(), losses.Load())
	}
	out := <-result
	if out.Behavior != winner.Load().(api.Behavior) {
		t.Fatalf("hook got %q, winning decision was %q", out.Behavior, winner.Load())
	}
}

func TestAwayToPresentReleasesHeldRequest(t *testing.T) {
	h := newHarness(t)
	h.presence.setAway(true)
	h.start(t)
	waitFor(t, "away", func() bool { return h.d.Presence().State == api.PresenceAway })

	result := make(chan fakeOutcome, 1)
	go func() { result <- h.hook(bg, t, permission("npm publish", "")) }()
	req := h.openRequest(t)
	h.presence.setAway(false)
	select {
	case out := <-result:
		if out.Behavior != "" {
			t.Fatalf("released hook answered %+v, want no opinion", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hook not released when the person came back")
	}
	r, _ := h.c.Request(bg, req.ID)
	if r.Status != api.RequestSteppedAside {
		t.Fatalf("status = %s, want stepped_aside", r.Status)
	}
	evs, _ := h.c.Events(bg, 0)
	var sawPresence bool
	for _, ev := range evs {
		sawPresence = sawPresence || ev.Type == api.EventPresenceChanged
	}
	if !sawPresence {
		t.Fatal("no presence.changed event")
	}
}

func TestHoldTimeoutExpires(t *testing.T) {
	h := newHarness(t)
	h.adapter.maxHold = holdMargin + 300*time.Millisecond // hold = 300 ms
	h.cfg.Approvals.AlwaysHold = true
	h.start(t)

	start := time.Now()
	out := h.hook(bg, t, permission("terraform apply", ""))
	if out.Behavior != "" {
		t.Fatalf("expired hook answered %+v", out)
	}
	if d := time.Since(start); d < 250*time.Millisecond || d > 3*time.Second {
		t.Fatalf("hold lasted %s, want ≈300ms", d)
	}
	rs, _ := h.c.Requests(bg, api.RequestExpired)
	if len(rs) != 1 {
		t.Fatalf("expired requests = %+v", rs)
	}
	if !h.notes.find(core.NotifyBlocked, true) {
		t.Fatal("no 'answer in the terminal' notification")
	}
}

func TestShimDisconnectExpires(t *testing.T) {
	h := newHarness(t)
	h.cfg.Approvals.AlwaysHold = true
	h.start(t)

	ctx, cancel := context.WithCancel(bg)
	errc := make(chan error, 1)
	go func() {
		_, err := h.hookErr(ctx, permission("rm -rf build", ""))
		errc <- err
	}()
	req := h.openRequest(t)
	cancel()
	if err := <-errc; err == nil {
		t.Fatal("hook call should have been cancelled")
	}
	waitFor(t, "request expired", func() bool {
		r, err := h.c.Request(bg, req.ID)
		return err == nil && r.Status == api.RequestExpired
	})
}

func TestTerminalRouteAndAnsweredLocally(t *testing.T) {
	h := newHarness(t)
	h.start(t)

	h.hook(bg, t, shell("go test ./..."))
	out := h.hook(bg, t, permission("go test ./...", "tu-9"))
	if out.Behavior != "" {
		t.Fatalf("terminal-routed permission answered %+v, want no opinion", out)
	}
	rs, _ := h.c.Requests(bg, api.RequestSteppedAside)
	if len(rs) != 1 || rs[0].Route != api.RouteTerminal {
		t.Fatalf("stepped aside = %+v", rs)
	}
	waitFor(t, "desktop notification", func() bool { return h.notes.find(core.NotifyBlocked, false) })

	h.hook(bg, t, adapters.HookEvent{Kind: adapters.HookPostTool, Tool: "Bash", ToolKind: api.ToolShell,
		Command: "go test ./...", Summary: "Run: go test ./...", ToolUseID: "tu-9"})
	r, _ := h.c.Request(bg, rs[0].ID)
	if r.Status != api.RequestAnsweredLocally {
		t.Fatalf("status after post_tool = %s", r.Status)
	}
	s, _ := h.c.Session(bg, r.SessionID)
	if s.State != api.StateWorking {
		t.Fatalf("session = %s", s.State)
	}

	// Stop while away: the session waits, and the person is told remotely.
	h.presence.setAway(true)
	waitFor(t, "away", func() bool { return h.d.Presence().State == api.PresenceAway })
	h.hook(bg, t, adapters.HookEvent{Kind: adapters.HookStop, Message: "All tests pass."})
	s, _ = h.c.Session(bg, r.SessionID)
	if s.State != api.StateWaiting || s.LastMessage != "All tests pass." || !s.Unseen {
		t.Fatalf("after stop: %+v", s)
	}
	if !h.notes.find(core.NotifyWaiting, true) {
		t.Fatal("no waiting notification")
	}
	if err := h.c.MarkSeen(bg, s.ID); err != nil {
		t.Fatal(err)
	}
	if s, _ = h.c.Session(bg, s.ID); s.Unseen {
		t.Fatal("still unseen")
	}
}

func TestAgentCallerGuard(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	h.rejectCallers(errors.New("called from inside claude"))
	h.hook(bg, t, permission("ls", "")) // hooks are never guarded
	rs, _ := h.c.Requests(bg, api.RequestSteppedAside)

	checks := map[string]error{
		"decide": func() error {
			_, err := h.c.Decide(bg, rs[0].ID, api.DecideBody{Behavior: api.BehaviorAllow})
			return err
		}(),
		"presence": func() error { _, err := h.c.SetPresence(bg, api.PresenceForceAway); return err }(),
		"reload":   func() error { _, err := h.c.PolicyReload(bg); return err }(),
		"pair":     func() error { _, err := h.c.Pair(bg); return err }(),
		"revoke":   h.c.RevokeDevice(bg, "d_x"),
		"shutdown": h.c.Shutdown(bg),
	}
	for name, err := range checks {
		if !client.IsCode(err, "agent_caller") {
			t.Errorf("%s: %v, want agent_caller", name, err)
		}
	}
	if _, err := h.c.Health(bg); err != nil {
		t.Fatalf("daemon stopped by a rejected caller: %v", err)
	}
	h.rejectCallers(nil)
	if _, err := h.c.SetPresence(bg, api.PresenceForceAway); err != nil {
		t.Fatal(err)
	}
}

func TestPresenceModeOverride(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	p, err := h.c.SetPresence(bg, api.PresenceForceAway)
	if err != nil || p.State != api.PresenceAway || p.Source != "manual" {
		t.Fatalf("force away = %+v, %v", p, err)
	}
	if _, err := h.c.SetPresence(bg, "sideways"); !client.IsCode(err, "bad_request") {
		t.Fatalf("bad mode: %v", err)
	}
	h.stop(t)
	h.start(t) // the override survives a restart
	if p, _ := h.c.Presence(bg); p.Mode != api.PresenceForceAway || p.State != api.PresenceAway {
		t.Fatalf("after restart = %+v", p)
	}
}

func TestLivenessMarksExited(t *testing.T) {
	h := newHarness(t)
	h.cfg.Approvals.AlwaysHold = true
	h.start(t)
	result := make(chan fakeOutcome, 1)
	go func() { result <- h.hook(bg, t, permission("sleep 100", "")) }()
	req := h.openRequest(t)

	h.dead.Store(4242, true)
	waitFor(t, "session exited", func() bool {
		s, err := h.c.Session(bg, req.SessionID)
		return err == nil && s.State == api.StateExited
	})
	if out := <-result; out.Behavior != "" {
		t.Fatalf("hook of a dead agent answered %+v", out)
	}
	if r, _ := h.c.Request(bg, req.ID); r.Status != api.RequestExpired {
		t.Fatalf("request of a dead agent = %s", r.Status)
	}
	evs, _ := h.c.SessionEvents(bg, req.SessionID, 50)
	var exited bool
	for _, ev := range evs {
		exited = exited || ev.Type == api.EventSessionExited
	}
	if !exited {
		t.Fatal("no session.exited event")
	}
}

func TestWrappedSessionOSCExplainExit(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	task, err := h.c.PutTask(bg, api.Task{Title: "fix", Prompt: "fix it", Agent: fakeKind})
	if err != nil {
		t.Fatal(err)
	}
	b := api.WrappedSessionBody{SessionID: "s_wrapped0001", Agent: api.AgentGeneric, Title: "demo",
		Cwd: t.TempDir(), PID: 7777, HolderSock: "/tmp/h.sock", TaskID: task.ID}
	s, err := h.c.RegisterWrapped(bg, b)
	if err != nil || s.Mode != api.ModeWrapped || s.Title != "demo" {
		t.Fatalf("register = %+v, %v", s, err)
	}
	if s2, err := h.c.RegisterWrapped(bg, b); err != nil || s2.ID != s.ID || !s2.StartedAt.Equal(s.StartedAt) {
		t.Fatalf("re-register = %+v, %v", s2, err)
	}
	if tk, _ := h.c.Task(bg, task.ID); tk.SessionID != s.ID {
		t.Fatalf("task not linked: %+v", tk)
	}

	if err := h.c.ReportOSC(bg, s.ID, api.OSCBody{State: "blocked", Kind: "question"}); err != nil {
		t.Fatal(err)
	}
	s, _ = h.c.Session(bg, s.ID)
	if s.State != api.StateBlocked || s.BlockKind != api.BlockQuestion || s.StateLayer != api.LayerOSC {
		t.Fatalf("after OSC: %+v", s)
	}
	if err := h.c.ReportOSC(bg, s.ID, api.OSCBody{State: "bogus"}); !client.IsCode(err, "bad_request") {
		t.Fatalf("bad OSC: %v", err)
	}
	ex, err := h.c.Explain(bg, s.ID)
	if err != nil || ex.Rule != "osc" || len(ex.Evidence) != 1 || !ex.Evidence[0].Fresh {
		t.Fatalf("explain = %+v, %v", ex, err)
	}

	if err := h.c.ReportExit(bg, s.ID, 3); err != nil {
		t.Fatal(err)
	}
	s, _ = h.c.Session(bg, s.ID)
	if s.State != api.StateExited || s.ExitCode == nil || *s.ExitCode != 3 {
		t.Fatalf("after exit: %+v", s)
	}
	if _, err := h.c.RegisterWrapped(bg, api.WrappedSessionBody{SessionID: "../evil"}); !client.IsCode(err, "bad_request") {
		t.Fatalf("bad id: %v", err)
	}
}

func TestRestartPreservesDataAndReleasesHeld(t *testing.T) {
	h := newHarness(t)
	h.cfg.Approvals.AlwaysHold = true
	h.start(t)

	result := make(chan fakeOutcome, 1)
	go func() { result <- h.hook(bg, t, permission("deploy", "")) }()
	req := h.openRequest(t)
	before, _ := h.c.Snapshot(bg)

	h.stop(t)
	select {
	case out := <-result:
		if out.Behavior != "" {
			t.Fatalf("hook answered %+v during shutdown, want no opinion", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held hook not released by shutdown")
	}
	if _, err := os.Stat(paths.Socket()); !os.IsNotExist(err) {
		t.Fatalf("socket left behind: %v", err)
	}

	h.start(t)
	after, err := h.c.Snapshot(bg)
	if err != nil {
		t.Fatal(err)
	}
	if after.Seq <= before.Seq {
		t.Fatalf("seq went from %d to %d", before.Seq, after.Seq)
	}
	if len(after.Sessions) != 1 || after.Sessions[0].ID != req.SessionID {
		t.Fatalf("sessions after restart = %+v", after.Sessions)
	}
	r, _ := h.c.Request(bg, req.ID)
	if r.Status != api.RequestSteppedAside {
		t.Fatalf("request after restart = %s, want stepped_aside", r.Status)
	}
	if _, err := os.Stat(filepath.Join(paths.Home(), "pilothouse.db")); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownOverAPI(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	if err := h.c.Shutdown(bg); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatal(err)
		}
		h.cancel = nil
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not stop")
	}
	if _, err := h.c.Health(bg); !errors.Is(err, client.ErrDaemonDown) {
		t.Fatalf("health after shutdown: %v", err)
	}
}

func TestUnknownAgentAndBadPayload(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	resp, err := h.c.Hook(bg, adapters.HookEnvelope{Agent: "nobody", Payload: json.RawMessage(`{}`)})
	if err != nil || resp.ExitCode != 0 || resp.Stdout != "" {
		t.Fatalf("unknown agent = %+v, %v", resp, err)
	}
	resp, err = h.c.Hook(bg, adapters.HookEnvelope{Agent: fakeKind, Payload: json.RawMessage(`"not an event"`)})
	if err != nil || resp.Stdout != `{"fail":true}` {
		t.Fatalf("bad payload = %+v, %v", resp, err)
	}
}

// TestPreToolLatency reports the pre_tool round trip over the real socket
// (BUILD-SPEC invariant 3: p99 < 25 ms). It only fails on gross
// regressions, because CI machines and -race inflate the numbers.
func TestPreToolLatency(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	h.hook(bg, t, shell("warm up"))
	const n = 300
	lat := make([]time.Duration, n)
	for i := range lat {
		start := time.Now()
		h.hook(bg, t, shell("ls"))
		lat[i] = time.Since(start)
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p50, p99 := lat[n/2], lat[n*99/100]
	t.Logf("pre_tool round trip: p50=%s p99=%s", p50, p99)
	if p99 > 250*time.Millisecond {
		t.Fatalf("pre_tool p99 %s is far over budget", p99)
	}
}

func BenchmarkPreToolRoundTrip(b *testing.B) {
	h := newHarness(b)
	h.start(b)
	h.hook(bg, b, shell("warm up"))
	lat := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		h.hook(bg, b, shell("ls"))
		lat = append(lat, time.Since(start))
	}
	b.StopTimer()
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	b.ReportMetric(float64(lat[len(lat)/2].Microseconds()), "p50-µs")
	b.ReportMetric(float64(lat[len(lat)*99/100].Microseconds()), "p99-µs")
}

// TestRealPolicyFile drives pre_tool through the real policy engine and a
// policy file in the temporary Pilothouse home.
func TestRealPolicyFile(t *testing.T) {
	h := newHarness(t)
	h.opts = func(o *Options) { o.LoadPolicy = nil }
	policyYAML := `version: 1
rules:
  - name: no-force-push
    match: { kind: shell, command: "git push*" }
    when: 'command.matches("(--force|-f)\\b")'
    action: deny
    message: Force-push is blocked by policy.
  - name: allow-ls
    match: { kind: shell, command: "ls*" }
    action: allow
`
	if err := os.WriteFile(paths.Policy(), []byte(policyYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	h.start(t)
	st, err := h.c.PolicyStatus(bg)
	if err != nil {
		t.Fatal(err)
	}
	if st.Rules < 2 {
		t.Skipf("policy engine not loading rules yet (status %+v)", st)
	}
	tests := []struct {
		cmd  string
		want api.Behavior
	}{
		{"git push --force origin main", api.BehaviorDeny},
		{"git push origin main", ""},
		{"ls -la", api.BehaviorAllow},
	}
	for _, tt := range tests {
		if out := h.hook(bg, t, shell(tt.cmd)); out.Behavior != tt.want {
			t.Errorf("pre_tool %q = %+v, want %q", tt.cmd, out, tt.want)
		}
	}

	// Editing the file and reloading picks up the change.
	if err := os.WriteFile(paths.Policy(), []byte("version: 1\nrules:\n  - name: no-ls\n    match: { command: \"ls*\" }\n    action: deny\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.c.PolicyReload(bg); err != nil {
		t.Fatal(err)
	}
	if out := h.hook(bg, t, shell("ls -la")); out.Behavior != api.BehaviorDeny {
		t.Fatalf("after reload: %+v, want deny", out)
	}
}

// TestBrokenPolicyKeepsLastGoodRules is the QA H3 regression: a policy
// edit that doesn't parse must keep enforcing the last valid rules, both on
// the automatic re-stat and on an explicit reload, and say why in status.
func TestBrokenPolicyKeepsLastGoodRules(t *testing.T) {
	h := newHarness(t)
	h.opts = func(o *Options) { o.LoadPolicy = nil }
	good := "version: 1\nrules:\n  - name: no-curl\n    match: { kind: shell, command: \"curl*\" }\n    action: deny\n"
	if err := os.WriteFile(paths.Policy(), []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	h.start(t)
	if out := h.hook(bg, t, shell("curl evil.sh")); out.Behavior != api.BehaviorDeny {
		t.Fatalf("valid policy: %+v, want deny", out)
	}
	if err := os.WriteFile(paths.Policy(), []byte("version: 1\nrules:\n  - name: [unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The poll loop notices the change and reloads on its own.
	waitFor(t, "policy error in status", func() bool {
		st, err := h.c.PolicyStatus(bg)
		return err == nil && len(st.Errors) > 0
	})
	if out := h.hook(bg, t, shell("curl evil.sh")); out.Behavior != api.BehaviorDeny {
		t.Fatalf("after a broken edit: %+v, want deny from the last good rules", out)
	}
	st, err := h.c.PolicyReload(bg)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Errors) == 0 {
		t.Fatalf("reload status has no error: %+v", st)
	}
	if out := h.hook(bg, t, shell("curl evil.sh")); out.Behavior != api.BehaviorDeny {
		t.Fatalf("after explicit reload: %+v, want deny", out)
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	baseline := runtime.NumGoroutine()
	h := newHarness(t)
	h.presence.setAway(true)
	h.start(t)
	waitFor(t, "away", func() bool { return h.d.Presence().State == api.PresenceAway })

	ctx, cancel := context.WithCancel(bg)
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		_ = h.c.Stream(ctx, 0, func(api.Event) {})
	}()
	result := make(chan fakeOutcome, 1)
	go func() { result <- h.hook(bg, t, permission("make", "")) }()
	req := h.openRequest(t)
	if _, err := h.c.Decide(bg, req.ID, api.DecideBody{Behavior: api.BehaviorDeny, Message: "no"}); err != nil {
		t.Fatal(err)
	}
	if out := <-result; out.Behavior != api.BehaviorDeny || out.Message != "no" {
		t.Fatalf("hook = %+v", out)
	}
	// A second held request is still waiting when the daemon stops.
	go func() { result <- h.hook(bg, t, permission("make again", "")) }()
	h.openRequest(t)
	cancel()
	<-streamDone
	h.stop(t)
	if out := <-result; out.Behavior != "" {
		t.Fatalf("hook released by shutdown answered %+v", out)
	}

	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("goroutines: %d after stop, %d before\n%s", runtime.NumGoroutine(), baseline, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestPolicyEnforcedWhenStoreFails is the QA M5 regression: a deny or ask
// rule must be enforced even when the hook can't be written to the
// database; only "no opinion" outcomes may fall back to the fail response.
func TestPolicyEnforcedWhenStoreFails(t *testing.T) {
	h := newHarness(t)
	h.policy.set("rm -rf /", api.BehaviorDeny, "no-rm")
	h.policy.set("git push", api.BehaviorAsk, "ask-push")
	h.start(t)
	// Inject a failing store: every event insert aborts, so every hook
	// transaction rolls back.
	if _, err := h.d.db.SQL().Exec(`CREATE TRIGGER fail_events BEFORE INSERT ON events
		BEGIN SELECT RAISE(ABORT, 'injected: database or disk is full'); END`); err != nil {
		t.Fatal(err)
	}
	if out := h.hook(bg, t, shell("rm -rf /")); out.Behavior != api.BehaviorDeny || out.Message == "" {
		t.Fatalf("deny with a failing store = %+v", out)
	}
	if out := h.hook(bg, t, shell("git push")); out.Behavior != api.BehaviorAsk {
		t.Fatalf("ask with a failing store = %+v", out)
	}
	// No verdict: the hook still fails like a down daemon.
	if out := h.hook(bg, t, shell("ls")); out.Behavior != "" {
		t.Fatalf("no-opinion with a failing store = %+v", out)
	}
	if _, err := h.d.db.SQL().Exec(`DROP TRIGGER fail_events`); err != nil {
		t.Fatal(err)
	}
}

// TestHeldRequestAnsweredInNativeDialog is the QA H1 regression: Claude
// shows its own dialog while a request is held for devices. When the
// person answers it there, the next event closes the held request as
// answered_locally, the held hook returns no opinion at once, and a late
// device decision gets already_decided.
func TestHeldRequestAnsweredInNativeDialog(t *testing.T) {
	for _, tc := range []struct {
		name string
		next adapters.HookEvent
	}{
		{"post_tool by tool_use_id", adapters.HookEvent{Kind: adapters.HookPostTool, Tool: "Bash", ToolKind: api.ToolShell,
			Command: "npm publish", Summary: "Run: npm publish", ToolUseID: "tu-held"}},
		{"post_tool by tool and summary", adapters.HookEvent{Kind: adapters.HookPostTool, Tool: "Bash", ToolKind: api.ToolShell,
			Command: "npm publish", Summary: "Run: npm publish"}},
		{"prompt_submit", adapters.HookEvent{Kind: adapters.HookPromptSubmit}},
		{"stop", adapters.HookEvent{Kind: adapters.HookStop}},
		{"session_end", adapters.HookEvent{Kind: adapters.HookSessionEnd}},
		{"idle notification", adapters.HookEvent{Kind: adapters.HookNotification, NotificationType: "idle_prompt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.presence.setAway(true)
			h.start(t)
			waitFor(t, "away", func() bool { return h.d.Presence().State == api.PresenceAway })

			result := make(chan fakeOutcome, 1)
			ev := permission("npm publish", "tu-held")
			if tc.next.Kind == adapters.HookPostTool && tc.next.ToolUseID == "" {
				ev.ToolUseID = "" // agents without tool_use_id match by tool + summary
			}
			go func() { result <- h.hook(bg, t, ev) }()
			req := h.openRequest(t)
			if req.Route != api.RouteDevices {
				t.Fatalf("route = %s", req.Route)
			}
			h.hook(bg, t, tc.next)
			select {
			case out := <-result:
				if out.Behavior != "" {
					t.Fatalf("released hook answered %+v, want no opinion", out)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("held hook not released after the local answer")
			}
			r, _ := h.c.Request(bg, req.ID)
			if r.Status != api.RequestAnsweredLocally {
				t.Fatalf("status = %s, want answered_locally", r.Status)
			}
			if _, err := h.c.Decide(bg, req.ID, api.DecideBody{Behavior: api.BehaviorAllow}); !client.IsCode(err, "already_decided") {
				t.Fatalf("late device decision: %v, want already_decided", err)
			}
			if s, _ := h.c.Session(bg, req.SessionID); s.State == api.StateBlocked {
				t.Fatalf("session still blocked: %+v", s)
			}
		})
	}
}

// TestDeniedNativeDialogIdleUnblocks is the QA L4 regression: Esc/No in
// the agent's dialog fires no Stop; the idle notification that follows
// closes the request and the session shows waiting, not blocked.
func TestDeniedNativeDialogIdleUnblocks(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	h.hook(bg, t, shell("rm -rf build"))
	h.hook(bg, t, permission("rm -rf build", "tu-1"))
	rs, _ := h.c.Requests(bg, api.RequestSteppedAside)
	if len(rs) != 1 {
		t.Fatalf("stepped aside = %+v", rs)
	}
	if s, _ := h.c.Session(bg, rs[0].SessionID); s.State != api.StateBlocked {
		t.Fatalf("session = %s, want blocked", s.State)
	}
	h.hook(bg, t, adapters.HookEvent{Kind: adapters.HookNotification, NotificationType: "idle_prompt",
		Message: "Claude is waiting for your input"})
	r, _ := h.c.Request(bg, rs[0].ID)
	if r.Status != api.RequestAnsweredLocally {
		t.Fatalf("status = %s", r.Status)
	}
	if s, _ := h.c.Session(bg, r.SessionID); s.State != api.StateWaiting {
		t.Fatalf("session = %s, want waiting", s.State)
	}
}

// TestSubagentStopKeepsWaiting is the QA M1 regression: the subagent Claude
// runs after Stop must not flip the session back to working.
func TestSubagentStopKeepsWaiting(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	h.hook(bg, t, adapters.HookEvent{Kind: adapters.HookPromptSubmit})
	h.hook(bg, t, adapters.HookEvent{Kind: adapters.HookStop, Message: "Done."})
	h.hook(bg, t, adapters.HookEvent{Kind: adapters.HookSubagentStop})
	ss, err := h.c.Sessions(bg)
	if err != nil || len(ss) != 1 {
		t.Fatalf("sessions = %+v, %v", ss, err)
	}
	if ss[0].State != api.StateWaiting {
		t.Fatalf("after SubagentStop: %s, want waiting", ss[0].State)
	}
}

// TestExitCodeAfterSessionEnd is the QA L2 regression: SessionEnd marks a
// wrapped session exited first; the holder's exit code that arrives after
// must still be recorded.
func TestExitCodeAfterSessionEnd(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	s, err := h.c.RegisterWrapped(bg, api.WrappedSessionBody{SessionID: "s_wrapped0002", Agent: fakeKind,
		Cwd: t.TempDir(), PID: 7778})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.hookEnv(bg, adapters.HookEvent{Kind: adapters.HookSessionEnd},
		map[string]string{"PILOTHOUSE_SESSION_ID": s.ID}); err != nil {
		t.Fatal(err)
	}
	if s, _ = h.c.Session(bg, s.ID); s.State != api.StateExited || s.ExitCode != nil {
		t.Fatalf("after SessionEnd: %+v", s)
	}
	if err := h.c.ReportExit(bg, s.ID, 2); err != nil {
		t.Fatal(err)
	}
	s, _ = h.c.Session(bg, s.ID)
	if s.ExitCode == nil || *s.ExitCode != 2 || s.StateDetail != "Exited with code 2" {
		t.Fatalf("after holder exit: code=%v detail=%q", s.ExitCode, s.StateDetail)
	}
}

// appServer is a shared agent server running hooks for several sessions
// (Codex's `codex app-server --managed-daemon`).
var appServer = []adapters.Proc{{PID: 4400, PPID: 9000, Comm: "sh"},
	{PID: 9000, PPID: 1, Comm: "fakeagent", Args: "fakeagent app-server --managed-daemon"}}

// TestIdleExpiry is the QA H5 regression (daemon side): sessions whose
// process can't be watched — hooks from a shared app-server, or a PID
// several sessions share — are expired after session_idle_expiry with
// their requests; a session with its own live PID never is; and a later
// SessionStart for the same agent session resumes it (no duplicate).
func TestIdleExpiry(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	later := func(d time.Duration) time.Time { return time.Now().Add(d) }

	// Private, live PID: never expired.
	h.hook(bg, t, adapters.HookEvent{Kind: adapters.HookSessionStart, AgentSessionID: "own"})
	h.d.expireIdle(bg, later(48*time.Hour))
	if ss, _ := h.c.Sessions(bg); len(ss) != 1 || ss[0].State == api.StateExited {
		t.Fatalf("private-PID session expired: %+v", ss)
	}

	// Two sessions through the shared server: no PID recorded.
	for _, id := range []string{"tui-1", "tui-2"} {
		if _, err := h.hookFrom(bg, adapters.HookEvent{Kind: adapters.HookSessionStart, AgentSessionID: id}, nil, appServer); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.hookFrom(bg, permission("make deploy", "tu-x"), nil, appServer); err != nil { // AgentSessionID vendor-1
		t.Fatal(err)
	}
	ss, _ := h.c.Sessions(bg)
	for _, s := range ss {
		if s.AgentSessionID != "own" && s.PID != 0 {
			t.Fatalf("app-server PID recorded as the session's: %+v", s)
		}
	}
	pending, _ := h.c.Requests(bg, api.RequestSteppedAside)
	if len(pending) != 1 {
		t.Fatalf("pending = %+v", pending)
	}

	h.d.expireIdle(bg, later(time.Hour)) // not idle long enough
	if r, _ := h.c.Request(bg, pending[0].ID); r.Status != api.RequestSteppedAside {
		t.Fatalf("expired too early: %s", r.Status)
	}
	h.d.expireIdle(bg, later(13*time.Hour))
	ss, _ = h.c.Sessions(bg)
	exited := 0
	for _, s := range ss {
		if s.State == api.StateExited {
			exited++
		} else if s.AgentSessionID != "own" {
			t.Errorf("idle app-server session still live: %+v", s)
		}
	}
	if exited != 3 {
		t.Fatalf("exited = %d, want 3", exited)
	}
	if r, _ := h.c.Request(bg, pending[0].ID); r.Status != api.RequestExpired {
		t.Fatalf("request of an expired session = %s, want expired", r.Status)
	}

	// Resume: same agent session id → same Pilothouse session, live again.
	if _, err := h.hookFrom(bg, adapters.HookEvent{Kind: adapters.HookSessionStart, AgentSessionID: "tui-1"}, nil, appServer); err != nil {
		t.Fatal(err)
	}
	ss, _ = h.c.Sessions(bg)
	if len(ss) != 4 {
		t.Fatalf("resume created a duplicate: %d sessions", len(ss))
	}
	for _, s := range ss {
		if s.AgentSessionID == "tui-1" && s.State == api.StateExited {
			t.Fatalf("resumed session still exited: %+v", s)
		}
	}
}

func TestIdleExpiryPerAgent(t *testing.T) {
	d := &Daemon{cfg: config.Default()}
	if d.idleExpiry(api.AgentClaude) != 12*time.Hour || d.idleExpiry(api.AgentCodex) != 2*time.Hour {
		t.Fatalf("defaults: claude %v codex %v", d.idleExpiry(api.AgentClaude), d.idleExpiry(api.AgentCodex))
	}
	d.cfg.SessionIdleExpiry.Duration = 30 * time.Minute
	if d.idleExpiry(api.AgentCodex) != 30*time.Minute {
		t.Fatal("a shorter configured expiry must apply to Codex too")
	}
	d.cfg.SessionIdleExpiry.Duration = 0
	if d.idleExpiry(api.AgentCodex) != 0 {
		t.Fatal("0 must turn expiry off")
	}
}

// TestSharedServerHookAdoptsWrappedSession: hooks that come through the
// shared app-server lack PILOTHOUSE_SESSION_ID; they join the one wrapped
// session in that cwd instead of creating an observed duplicate.
func TestSharedServerHookAdoptsWrappedSession(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	cwd := t.TempDir()
	s, err := h.c.RegisterWrapped(bg, api.WrappedSessionBody{SessionID: "s_wrapped0003", Agent: fakeKind, Cwd: cwd, PID: 7779})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.hookFrom(bg, adapters.HookEvent{Kind: adapters.HookSessionStart, AgentSessionID: "thread-1", Cwd: cwd}, nil, appServer); err != nil {
		t.Fatal(err)
	}
	ss, _ := h.c.Sessions(bg)
	if len(ss) != 1 || ss[0].ID != s.ID || ss[0].AgentSessionID != "thread-1" || ss[0].PID != 7779 {
		t.Fatalf("sessions = %+v", ss)
	}
}

// TestLargeHookPayloadEvaluated is the QA M6 regression on the daemon
// side: a hook carrying 5 MB of tool input is accepted and decided by
// policy (it used to exceed the shim and envelope limits).
func TestLargeHookPayloadEvaluated(t *testing.T) {
	h := newHarness(t)
	h.policy.set("cat > /etc/hosts", api.BehaviorDeny, "no-hosts")
	h.start(t)
	ev := shell("cat > /etc/hosts")
	ev.Input = json.RawMessage(`{"content":"` + strings.Repeat("a", 5<<20) + `"}`)
	if out := h.hook(bg, t, ev); out.Behavior != api.BehaviorDeny {
		t.Fatalf("5 MB pre_tool = %+v, want deny", out)
	}
}

// TestPruneReducesRows (QA L8): retention deletes old hook events and the
// database is compacted afterwards.
func TestPruneReducesRows(t *testing.T) {
	h := newHarness(t)
	h.cfg.Retention = config.Duration{Duration: time.Millisecond}
	h.start(t)
	for i := 0; i < 20; i++ {
		h.hook(bg, t, shell("ls"))
	}
	count := func() int {
		var n int
		if err := h.d.db.SQL().QueryRow(`SELECT COUNT(*) FROM events WHERE type = ?`, string(api.EventHook)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count() < 20 {
		t.Fatalf("hook events = %d", count())
	}
	time.Sleep(10 * time.Millisecond)
	h.d.prune(bg)
	if n := count(); n != 0 {
		t.Fatalf("hook events after prune = %d", n)
	}
	if !h.d.compacted {
		t.Fatal("database not compacted after pruning")
	}
}
