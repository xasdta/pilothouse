package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/all"
	"github.com/xasdta/pilothouse/internal/paths"
)

// hkEnv isolates a test (BUILD-SPEC invariant 1) and returns the env for
// a child pilothouse process.
func hkEnv(t *testing.T) []string {
	t.Helper()
	home, user := t.TempDir(), t.TempDir()
	run, err := os.MkdirTemp("/tmp", "ag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(run) })
	t.Setenv("PILOTHOUSE_HOME", home)
	t.Setenv("PILOTHOUSE_USER_HOME", user)
	t.Setenv("PILOTHOUSE_RUN_DIR", run)
	t.Setenv("CODEX_HOME", "")
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PILOTHOUSE_") && !strings.HasPrefix(kv, "TMUX=") && !strings.HasPrefix(kv, "CODEX_HOME=") {
			env = append(env, kv)
		}
	}
	return append(env, "PILOTHOUSE_HOME="+home, "PILOTHOUSE_USER_HOME="+user, "PILOTHOUSE_RUN_DIR="+run, "PILOTHOUSE_NO_UPDATE_CHECK=1")
}

var (
	hkBinOnce sync.Once
	hkBin     string
	hkBinErr  error
)

// TestMain removes the pilothouse binary the shim tests build (shared by
// several tests, so no single t.TempDir can own it).
func TestMain(m *testing.M) {
	code := m.Run()
	if hkBin != "" {
		os.RemoveAll(filepath.Dir(hkBin))
	}
	os.Exit(code)
}

// hkBinary builds cmd/pilothouse once per test run.
func hkBinary(t *testing.T) string {
	t.Helper()
	hkBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "pilothouse-bin")
		if err != nil {
			hkBinErr = err
			return
		}
		hkBin = filepath.Join(dir, "pilothouse")
		out, err := exec.Command("go", "build", "-o", hkBin, "github.com/xasdta/pilothouse/cmd/pilothouse").CombinedOutput()
		if err != nil {
			hkBinErr = err
			t.Logf("%s", out)
		}
	})
	if hkBinErr != nil {
		t.Fatalf("build pilothouse: %v", hkBinErr)
	}
	return hkBin
}

func hkFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "adapters", "claude", "testdata", "hooks", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type hkResult struct {
	stdout, stderr string
	code           int
	took           time.Duration
}

func hkRunShim(t *testing.T, env []string, stdin []byte, args ...string) hkResult {
	t.Helper()
	cmd := exec.Command(hkBinary(t), append([]string{"hook"}, args...)...)
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	start := time.Now()
	err := cmd.Run()
	res := hkResult{out.String(), errb.String(), 0, time.Since(start)}
	if ee, ok := err.(*exec.ExitError); ok {
		res.code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return res
}

// hkFakeDaemon serves POST /v1/hook on the isolated socket.
func hkFakeDaemon(t *testing.T, handle func(adapters.HookEnvelope) (adapters.HookResponse, time.Duration)) <-chan adapters.HookEnvelope {
	t.Helper()
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", paths.Socket())
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan adapters.HookEnvelope, 4)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/hook" {
			http.NotFound(w, r)
			return
		}
		var env adapters.HookEnvelope
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		got <- env
		resp, delay := handle(env)
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return got
}

func TestShimNoDaemonFailsOpen(t *testing.T) {
	env := hkEnv(t)
	for _, fx := range []string{"pre_tool_bash", "permission_bash", "session_end", "stop"} {
		res := hkRunShim(t, env, hkFixture(t, fx), "claude")
		if res.code != 0 || res.stdout != "" || res.stderr != "" {
			t.Errorf("%s: %+v", fx, res)
		}
		// With no daemon the dial fails at once; the bound only guards against a
		// hang. It includes process start, which is slow under -race and load.
		if res.took > 3*time.Second {
			t.Errorf("%s: took %v", fx, res.took)
		}
	}
	// Garbage, empty input and unknown agents: still silent success.
	for _, tc := range []struct {
		stdin string
		args  []string
	}{{"not json", []string{"claude"}}, {"", []string{"codex"}}, {"{}", []string{"gemini"}}, {"{}", nil}} {
		if res := hkRunShim(t, env, []byte(tc.stdin), tc.args...); res.code != 0 || res.stdout != "" || res.stderr != "" {
			t.Errorf("%q %v: %+v", tc.stdin, tc.args, res)
		}
	}
}

func TestShimNoDaemonFailClosed(t *testing.T) {
	env := hkEnv(t)
	if err := os.WriteFile(paths.Config(), []byte("approvals:\n  fail_closed: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := hkRunShim(t, env, hkFixture(t, "pre_tool_bash"), "claude")
	if res.code != 0 || !strings.Contains(res.stdout, `"permissionDecision":"deny"`) || !json.Valid([]byte(res.stdout)) {
		t.Errorf("pre_tool: %+v", res)
	}
	// Permission requests still fall through to the native dialog.
	if res := hkRunShim(t, env, hkFixture(t, "permission_bash"), "claude"); res.stdout != "" || res.code != 0 {
		t.Errorf("permission: %+v", res)
	}
}

func TestShimForwardsToDaemon(t *testing.T) {
	env := hkEnv(t)
	env = append(env, "TMUX=/tmp/tmux-1/default,1,0", "PILOTHOUSE_SESSION_ID=s_abcdefghij", "SECRET_TOKEN=nope")
	want := adapters.HookResponse{Stdout: `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"no"}}` + "\n"}
	got := hkFakeDaemon(t, func(adapters.HookEnvelope) (adapters.HookResponse, time.Duration) { return want, 0 })
	payload := hkFixture(t, "pre_tool_bash")
	res := hkRunShim(t, env, payload, "claude", "PreToolUse")
	if res.code != 0 || res.stdout != want.Stdout || res.stderr != "" {
		t.Fatalf("relay: %+v", res)
	}
	env1 := <-got
	// The payload arrives re-encoded as JSON (compacted), so compare values.
	var gotP, wantP any
	_ = json.Unmarshal(env1.Payload, &gotP)
	_ = json.Unmarshal(payload, &wantP)
	if env1.Agent != "claude" || env1.Event != "PreToolUse" || gotP == nil || !reflect.DeepEqual(gotP, wantP) {
		t.Errorf("envelope: agent=%s event=%s payload=%s", env1.Agent, env1.Event, env1.Payload)
	}
	if env1.ShimPID <= 0 || len(env1.Ancestors) == 0 || env1.Ancestors[0].PID != os.Getpid() {
		t.Errorf("process context: pid=%d ancestors=%+v (want parent %d)", env1.ShimPID, env1.Ancestors, os.Getpid())
	}
	if env1.PilothouseSessionID != "s_abcdefghij" || env1.Env["TMUX"] == "" || env1.Env["SECRET_TOKEN"] != "" || env1.Env["PILOTHOUSE_SESSION_ID"] == "" {
		t.Errorf("env: session=%q env=%v", env1.PilothouseSessionID, env1.Env)
	}
	if env1.Cwd == "" || env1.ReceivedAt.IsZero() {
		t.Errorf("cwd/time: %+v", env1)
	}
}

func TestShimRelaysExitCodeAndStderr(t *testing.T) {
	env := hkEnv(t)
	hkFakeDaemon(t, func(adapters.HookEnvelope) (adapters.HookResponse, time.Duration) {
		return adapters.HookResponse{Stderr: "blocked\n", ExitCode: 2}, 0
	})
	res := hkRunShim(t, env, hkFixture(t, "pre_tool_bash"), "claude")
	if res.code != 2 || res.stderr != "blocked\n" || res.stdout != "" {
		t.Errorf("%+v", res)
	}
}

// A daemon that stalls must not stall a non-blocking hook past 2 s.
func TestShimSlowDaemonTimesOut(t *testing.T) {
	env := hkEnv(t)
	hkFakeDaemon(t, func(adapters.HookEnvelope) (adapters.HookResponse, time.Duration) {
		return adapters.HookResponse{Stdout: "late"}, 10 * time.Second
	})
	res := hkRunShim(t, env, hkFixture(t, "pre_tool_bash"), "claude")
	if res.code != 0 || res.stdout != "" {
		t.Errorf("%+v", res)
	}
	if res.took > 3*time.Second {
		t.Errorf("took %v", res.took)
	}
}

// Permission hooks may be held: the shim waits for the daemon.
func TestShimPermissionWaits(t *testing.T) {
	env := hkEnv(t)
	allow := adapters.HookResponse{Stdout: `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}` + "\n"}
	hkFakeDaemon(t, func(adapters.HookEnvelope) (adapters.HookResponse, time.Duration) {
		return allow, 2500 * time.Millisecond
	})
	res := hkRunShim(t, env, hkFixture(t, "permission_bash"), "claude")
	if res.stdout != allow.Stdout || res.took < 2500*time.Millisecond {
		t.Errorf("%+v", res)
	}
}

func TestReadHookStdin(t *testing.T) {
	if b, err := readHookStdin(strings.NewReader(`{"a":1}`), time.Second); err != nil || string(b) != `{"a":1}` {
		t.Errorf("small: %q %v", b, err)
	}
	if _, err := readHookStdin(strings.NewReader(""), time.Second); err == nil {
		t.Error("empty accepted")
	}
	if b, err := readHookStdin(bytes.NewReader(make([]byte, 5<<20)), 5*time.Second); err != nil || len(b) != 5<<20 {
		t.Errorf("5 MB payload: %d bytes, %v", len(b), err)
	}
	if _, err := readHookStdin(bytes.NewReader(make([]byte, maxHookPayload+1)), 5*time.Second); !errors.Is(err, errHookTooLarge) {
		t.Errorf("oversized: %v", err)
	}
	pr, pw := net.Pipe() // never closed: the writer hangs
	defer pr.Close()
	defer pw.Close()
	start := time.Now()
	if _, err := readHookStdin(pr, 100*time.Millisecond); err == nil || time.Since(start) > time.Second {
		t.Error("hung writer not timed out")
	}
}

// TestShimOversizedPayload is the QA M6 regression: a payload over the
// limit is never "allowed by omission": with fail_closed a pre_tool is
// denied, and a warning goes to stderr; a PostToolUse is left alone.
func TestShimOversizedPayload(t *testing.T) {
	hkEnv(t)
	if err := os.WriteFile(paths.Config(), []byte("approvals:\n  fail_closed: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	big := func(event string) []byte {
		var b bytes.Buffer
		b.WriteString(`{"session_id":"s1","hook_event_name":"` + event + `","tool_name":"Write","tool_input":{"content":"`)
		b.Write(bytes.Repeat([]byte("a"), maxHookPayload))
		b.WriteString(`"}}`)
		return b.Bytes()
	}
	var out, errb bytes.Buffer
	runShim([]string{"claude"}, bytes.NewReader(big("PreToolUse")), &out, &errb)
	if !strings.Contains(out.String(), `"permissionDecision":"deny"`) || !strings.Contains(errb.String(), "not checked by policy") {
		t.Fatalf("oversized pre_tool: stdout %q stderr %q", out.String(), errb.String())
	}
	out.Reset()
	errb.Reset()
	runShim([]string{"claude"}, bytes.NewReader(big("PostToolUse")), &out, &errb)
	if strings.Contains(out.String(), "deny") || !strings.Contains(errb.String(), "not checked by policy") {
		t.Fatalf("oversized post_tool: stdout %q stderr %q", out.String(), errb.String())
	}
}

// TestShimForwardsLargePayload: a 5 MB payload (over the old 4 MB cap)
// reaches the daemon for policy evaluation.
func TestShimForwardsLargePayload(t *testing.T) {
	env := hkEnv(t)
	got := hkFakeDaemon(t, func(adapters.HookEnvelope) (adapters.HookResponse, time.Duration) {
		return adapters.HookResponse{Stdout: `{"ok":true}`}, 0
	})
	payload := []byte(`{"session_id":"s1","hook_event_name":"PreToolUse","tool_name":"Write","tool_use_id":"t","cwd":"/tmp","tool_input":{"file_path":"/tmp/x","content":"` +
		strings.Repeat("a", 5<<20) + `"}}`)
	res := hkRunShim(t, env, payload, "claude")
	if res.stdout != `{"ok":true}` {
		t.Fatalf("shim: %+v", res)
	}
	select {
	case e := <-got:
		if len(e.Payload) != len(payload) {
			t.Fatalf("forwarded %d bytes, want %d", len(e.Payload), len(payload))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon never got the hook")
	}
}

func TestHookTimeout(t *testing.T) {
	cl := all.Get("claude")
	if hookTimeout(cl, adapters.HookPermission, 0) != 600*time.Second || hookTimeout(cl, adapters.HookPreTool, 0) != 2*time.Second || hookTimeout(cl, adapters.HookSessionEnd, 0) != time.Second ||
		hookTimeout(cl, adapters.HookPreTool, 5<<20) != 7*time.Second || hookTimeout(cl, adapters.HookPreTool, 60<<20) != 20*time.Second {
		t.Error("timeouts")
	}
}

func TestHooksCommands(t *testing.T) {
	env := hkEnv(t)
	bin := hkBinary(t)
	run := func(args ...string) (string, string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		return out.String(), errb.String(), err
	}
	user := os.Getenv("PILOTHOUSE_USER_HOME")
	settings := filepath.Join(user, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte("{\n  \"model\": \"opus\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, stderr, err := run("hooks", "install", "--agent", "claude", "--dry-run")
	if err != nil || !strings.Contains(out, "would update "+settings) || !strings.Contains(out, "+  \"hooks\": {") || !strings.Contains(out, "Dry run") {
		t.Fatalf("dry run: %v\n%s\n%s", err, out, stderr)
	}
	if !strings.Contains(stderr, "temporary directory") {
		t.Errorf("no warning about the temp binary: %q", stderr)
	}
	if b, _ := os.ReadFile(settings); strings.Contains(string(b), "hooks") {
		t.Fatal("dry run wrote")
	}

	out, _, err = run("hooks", "install", "--agent", "claude")
	if err != nil || !strings.Contains(out, "backup  "+settings+".pilothouse-bak-") {
		t.Fatalf("install: %v\n%s", err, out)
	}
	resolved, _ := filepath.EvalSymlinks(bin)
	if b, _ := os.ReadFile(settings); !strings.Contains(string(b), resolved+" hook claude") {
		t.Errorf("settings:\n%s", b)
	}
	out, _, err = run("hooks", "status", "--agent", "claude")
	if err != nil || !strings.Contains(out, "installed, current") {
		t.Errorf("status: %v\n%s", err, out)
	}
	out, _, _ = run("hooks", "status", "--json")
	var rows []hookStatusJSON
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != len(all.List()) || !rows[0].Status.Current || rows[1].Status.Installed {
		t.Errorf("status --json: %v\n%s", err, out)
	}
	out, _, err = run("hooks", "install", "--agent", "claude")
	if err != nil || !strings.Contains(out, "· Claude Code: Claude Code hooks are up to date") {
		t.Errorf("reinstall: %v\n%s", err, out)
	}
	out, _, err = run("hooks", "uninstall")
	if err != nil || !strings.Contains(out, "Remove 10 Pilothouse hook handlers") {
		t.Errorf("uninstall: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(settings); strings.TrimSpace(string(b)) != "{\n  \"model\": \"opus\"\n}" {
		t.Errorf("after uninstall:\n%s", b)
	}
	if _, _, err := run("hooks", "install", "--agent", "gemini"); err == nil {
		t.Error("unknown agent accepted")
	}
}

func TestHookTargetsNothingFound(t *testing.T) {
	hkEnv(t)
	t.Setenv("PATH", t.TempDir()) // no agent binaries either
	if _, err := hookTargets("", true); err == nil || !strings.Contains(err.Error(), "--agent") {
		t.Errorf("err = %v", err)
	}
	if ts, err := hookTargets("", false); err != nil || len(ts) != len(all.List()) {
		t.Errorf("status targets: %v %v", ts, err)
	}
	if ts, err := hookTargets("Codex", true); err != nil || len(ts) != 1 {
		t.Errorf("explicit: %v %v", ts, err)
	}
}

func TestRunShimInProcessPanicsFailOpen(t *testing.T) {
	hkEnv(t)
	var out, errb bytes.Buffer
	// A reader that panics stands in for any bug on the shim path.
	code := runShim([]string{"claude"}, panicReader{}, &out, &errb)
	if code != 0 || out.Len() != 0 || errb.Len() != 0 {
		t.Errorf("code=%d out=%q err=%q", code, out.String(), errb.String())
	}
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("boom") }
