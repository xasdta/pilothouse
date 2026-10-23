package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
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
)

const fakeKind api.AgentKind = "fake"

// fakeAdapter reads hook payloads that are already normalized HookEvents
// and answers with the outcome as JSON, so tests can assert on it.
type fakeAdapter struct{ maxHold time.Duration }

func (fakeAdapter) Kind() api.AgentKind        { return fakeKind }
func (fakeAdapter) DisplayName() string        { return "Fake Agent" }
func (fakeAdapter) ProcessNames() []string     { return []string{"fakeagent"} }
func (fakeAdapter) Detect() adapters.Detection { return adapters.Detection{} }
func (fakeAdapter) ProtectedPaths() []string   { return nil }
func (fakeAdapter) ResumeArgs(string) []string { return nil }
func (fakeAdapter) ParseHook(env adapters.HookEnvelope) (adapters.HookEvent, error) {
	var ev adapters.HookEvent
	err := json.Unmarshal(env.Payload, &ev)
	return ev, err
}

type fakeOutcome struct {
	Behavior api.Behavior `json:"behavior"`
	Message  string       `json:"message"`
}

func (fakeAdapter) EncodeResponse(_ adapters.HookEvent, out adapters.HookOutcome) adapters.HookResponse {
	b, _ := json.Marshal(fakeOutcome{Behavior: out.Behavior, Message: out.Message})
	return adapters.HookResponse{Stdout: string(b)}
}

func (fakeAdapter) FailResponse(kind adapters.HookKind, failClosed bool) adapters.HookResponse {
	return adapters.HookResponse{Stdout: `{"fail":true}`}
}

func (a fakeAdapter) MaxHold(kind adapters.HookKind) time.Duration {
	if kind == adapters.HookPermission {
		return a.maxHold
	}
	return 0
}
func (fakeAdapter) InstallHooks(adapters.InstallOptions) (adapters.Change, error) {
	return adapters.Change{}, nil
}
func (fakeAdapter) UninstallHooks(adapters.InstallOptions) (adapters.Change, error) {
	return adapters.Change{}, nil
}
func (fakeAdapter) Status(adapters.InstallOptions) (adapters.HookStatus, error) {
	return adapters.HookStatus{}, nil
}

// fakePresence reports a settable idle time.
type fakePresence struct{ idle atomic.Int64 }

func (p *fakePresence) Idle() (time.Duration, bool) { return time.Duration(p.idle.Load()), true }
func (p *fakePresence) Source() string              { return "fake" }
func (p *fakePresence) setAway(away bool) {
	if away {
		p.idle.Store(int64(time.Hour))
	} else {
		p.idle.Store(0)
	}
}

// fakePolicy decides by exact command.
type fakePolicy struct {
	mu    sync.Mutex
	rules map[string]policy.Result
}

func (p *fakePolicy) set(cmd string, action api.Behavior, rule string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rules == nil {
		p.rules = map[string]policy.Result{}
	}
	p.rules[cmd] = policy.Result{Action: action, Rule: rule, File: "test", Message: rule + " says " + string(action),
		Matched: []policy.Match{{Rule: rule, File: "test", Action: action}}}
}

func (p *fakePolicy) Evaluate(in policy.Input) policy.Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rules[in.Command]
}
func (p *fakePolicy) Status() api.PolicyStatus { return api.PolicyStatus{Rules: len(p.rules)} }
func (p *fakePolicy) Changed() bool            { return false }
func (p *fakePolicy) Reload() api.PolicyStatus { return p.Status() }

// recNotifier records notifications.
type recNotifier struct {
	mu     sync.Mutex
	ns     []core.Notification
	closed atomic.Bool
}

func (n *recNotifier) Close() { n.closed.Store(true) }

func (n *recNotifier) all() []core.Notification {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]core.Notification(nil), n.ns...)
}

func (n *recNotifier) Notify(_ context.Context, x core.Notification) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ns = append(n.ns, x)
	return nil
}

func (n *recNotifier) find(kind core.NotificationKind, remote bool) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, x := range n.ns {
		if x.Kind == kind && x.Remote == remote {
			return true
		}
	}
	return false
}

// harness runs a real daemon in-process on temporary directories.
type harness struct {
	cfg      *config.Config
	adapter  fakeAdapter
	presence *fakePresence
	policy   *fakePolicy
	notes    *recNotifier
	guardErr atomic.Value // error or nil-error wrapper
	dead     sync.Map     // pid → true when the process "exited"
	opts     func(*Options)

	d      *Daemon
	c      *client.Client
	cancel context.CancelFunc
	done   chan error
}

type guardResult struct{ err error }

// newHarness points every Pilothouse path at fresh temp dirs (BUILD-SPEC
// invariant 1). The run dir lives under /tmp to keep the socket path short.
func newHarness(tb testing.TB) *harness {
	tb.Helper()
	run, err := os.MkdirTemp("/tmp", "ag")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = os.RemoveAll(run) })
	tb.Setenv("PILOTHOUSE_HOME", tb.TempDir())
	tb.Setenv("PILOTHOUSE_USER_HOME", tb.TempDir())
	tb.Setenv("PILOTHOUSE_RUN_DIR", run)
	tb.Setenv("PILOTHOUSE_NO_UPDATE_CHECK", "1")

	cfg := config.Default()
	cfg.Web.Addr = "127.0.0.1:0"
	cfg.Approvals.HoldLimit = config.Duration{Duration: 5 * time.Second}
	cfg.Presence.NotifyAfter = config.Duration{Duration: 50 * time.Millisecond}
	h := &harness{
		cfg:      cfg,
		adapter:  fakeAdapter{maxHold: 10 * time.Minute},
		presence: &fakePresence{},
		policy:   &fakePolicy{},
		notes:    &recNotifier{},
	}
	h.guardErr.Store(guardResult{})
	return h
}

func (h *harness) start(tb testing.TB) {
	tb.Helper()
	var logOut io.Writer = io.Discard
	if os.Getenv("PILOTHOUSE_TEST_LOG") != "" {
		logOut = os.Stderr
	}
	opts := Options{
		Config:    h.cfg,
		Adapters:  []adapters.Adapter{h.adapter},
		Presence:  h.presence,
		PeerGuard: func(net.Conn) error { return h.guardErr.Load().(guardResult).err },
		Alive: func(pid int) bool {
			_, dead := h.dead.Load(pid)
			return !dead
		},
		LoadPolicy:         func() PolicyEngine { return h.policy },
		Notifier:           h.notes,
		Log:                slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: slog.LevelDebug})),
		PollInterval:       20 * time.Millisecond,
		DisableUpdateCheck: true,
	}
	if h.opts != nil {
		h.opts(&opts)
	}
	ready := make(chan *Daemon, 1)
	opts.OnReady = func(d *Daemon) { ready <- d }
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan error, 1)
	go func() { h.done <- Run(ctx, opts) }()
	select {
	case h.d = <-ready:
	case err := <-h.done:
		tb.Fatalf("daemon failed to start: %v", err)
	case <-time.After(10 * time.Second):
		tb.Fatal("daemon did not start")
	}
	h.c = client.NewSocket(paths.Socket())
	tb.Cleanup(func() { h.stop(tb) })
}

// stop shuts the daemon down and waits for Run to return. Safe to call twice.
func (h *harness) stop(tb testing.TB) {
	if h.cancel == nil {
		return
	}
	h.cancel()
	select {
	case err := <-h.done:
		if err != nil {
			tb.Errorf("daemon exited with error: %v", err)
		}
	case <-time.After(10 * time.Second):
		tb.Error("daemon did not stop")
	}
	h.cancel = nil
}

func (h *harness) rejectCallers(err error) { h.guardErr.Store(guardResult{err: err}) }

// hook sends a hook as the shim would, from fake agent process 4242.
func (h *harness) hook(ctx context.Context, tb testing.TB, ev adapters.HookEvent) fakeOutcome {
	tb.Helper()
	out, err := h.hookErr(ctx, ev)
	if err != nil {
		tb.Fatalf("hook %s: %v", ev.Kind, err)
	}
	return out
}

func (h *harness) hookErr(ctx context.Context, ev adapters.HookEvent) (fakeOutcome, error) {
	return h.hookEnv(ctx, ev, nil)
}

// fakeAgentAncestors is the shim's process chain: sh under fake agent 4242.
var fakeAgentAncestors = []adapters.Proc{{PID: 4300, PPID: 4242, Comm: "sh"}, {PID: 4242, PPID: 1, Comm: "fakeagent"}}

// hookEnv is hookErr with the shim's allow-listed environment.
func (h *harness) hookEnv(ctx context.Context, ev adapters.HookEvent, env map[string]string) (fakeOutcome, error) {
	return h.hookFrom(ctx, ev, env, fakeAgentAncestors)
}

// hookFrom is hookEnv from an explicit process chain.
func (h *harness) hookFrom(ctx context.Context, ev adapters.HookEvent, env map[string]string, ancestors []adapters.Proc) (fakeOutcome, error) {
	if ev.AgentSessionID == "" {
		ev.AgentSessionID = "vendor-1"
	}
	if ev.Cwd == "" {
		ev.Cwd = os.TempDir()
	}
	payload, _ := json.Marshal(ev)
	resp, err := h.c.Hook(ctx, adapters.HookEnvelope{
		Agent: fakeKind, Payload: payload, ShimPID: 5000, Cwd: ev.Cwd, Env: env,
		PilothouseSessionID: env["PILOTHOUSE_SESSION_ID"], // as the shim forwards it
		Ancestors:           ancestors,
		ReceivedAt:          time.Now(),
	})
	if err != nil {
		return fakeOutcome{}, err
	}
	var out fakeOutcome
	if err := json.Unmarshal([]byte(resp.Stdout), &out); err != nil {
		return fakeOutcome{}, errors.New("bad hook response: " + resp.Stdout)
	}
	return out, nil
}

// waitFor polls cond until it holds or 5 s pass.
func waitFor(tb testing.TB, what string, cond func() bool) {
	tb.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			tb.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// openRequest waits for exactly one open request and returns it.
func (h *harness) openRequest(tb testing.TB) api.Request {
	tb.Helper()
	var req api.Request
	waitFor(tb, "an open request", func() bool {
		rs, err := h.c.Requests(context.Background(), api.RequestOpen)
		if err != nil || len(rs) != 1 {
			return false
		}
		req = rs[0]
		return true
	})
	return req
}

func shell(cmd string) adapters.HookEvent {
	return adapters.HookEvent{Kind: adapters.HookPreTool, Tool: "Bash", ToolKind: api.ToolShell,
		Command: cmd, Summary: "Run: " + cmd}
}

func permission(cmd, toolUseID string) adapters.HookEvent {
	return adapters.HookEvent{Kind: adapters.HookPermission, Tool: "Bash", ToolKind: api.ToolShell,
		Command: cmd, Summary: "Run: " + cmd, ToolUseID: toolUseID}
}
