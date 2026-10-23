package codex

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/hookkit"
	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/policy"
)

var update = flag.Bool("update", false, "rewrite golden files")

// isolate points every Pilothouse path at temp dirs (BUILD-SPEC invariant 1).
// CODEX_HOME is set to a decoy too: PILOTHOUSE_USER_HOME must win over it.
func isolate(t *testing.T) (userHome string) {
	t.Helper()
	userHome = t.TempDir()
	t.Setenv("PILOTHOUSE_HOME", t.TempDir())
	t.Setenv("PILOTHOUSE_USER_HOME", userHome)
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "decoy"))
	run, err := os.MkdirTemp("/tmp", "ag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(run) })
	t.Setenv("PILOTHOUSE_RUN_DIR", run)
	return userHome
}

// TestParseHookGolden: fixtures are built from Codex's generated input
// schemas (codex-rs/hooks/schema/generated) and the payload builders in
// codex-rs/core/src/tools (Codex is not installed where these were made).
func TestParseHookGolden(t *testing.T) {
	files, _ := filepath.Glob("testdata/hooks/*.json")
	if len(files) < 15 {
		t.Fatalf("only %d fixtures", len(files))
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		t.Run(name, func(t *testing.T) {
			payload, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			ev, err := Adapter{}.ParseHook(adapters.HookEnvelope{Agent: api.AgentCodex, Payload: payload})
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			enc.SetIndent("", "  ")
			if err := enc.Encode(ev); err != nil {
				t.Fatal(err)
			}
			golden := filepath.Join("testdata", "golden", name+".json")
			if *update {
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, buf.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run go test -update)", err)
			}
			if !bytes.Equal(buf.Bytes(), want) {
				t.Errorf("mismatch:\ngot:\n%s\nwant:\n%s", buf.Bytes(), want)
			}
		})
	}
}

func parseFixture(t *testing.T, name string) adapters.HookEvent {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("testdata", "hooks", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	ev, err := Adapter{}.ParseHook(adapters.HookEnvelope{Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestParseHookKeyFields(t *testing.T) {
	for _, tc := range []struct {
		fixture, summary string
		kind             adapters.HookKind
		tool             api.ToolKind
		paths            []string
	}{
		{"pre_tool_bash", "Run: git push origin main", adapters.HookPreTool, api.ToolShell, nil},
		{"pre_tool_apply_patch", "Edit: src/app.go, docs/new.md, old.txt (+2 more)", adapters.HookPreTool, api.ToolEdit,
			[]string{"/home/user/project/src/app.go", "/home/user/project/docs/new.md", "/home/user/project/old.txt", "/home/user/project/lib/x.go", "/home/user/project/lib/y.go"}},
		// Real Codex 0.164 payloads (tests/e2e/codex scenario=patch_env): with
		// no apply_patch tool the model patches through exec_command (M10).
		{"pre_tool_bash_apply_patch_heredoc", "Edit: .env.local", adapters.HookPreTool, api.ToolEdit,
			[]string{"/private/tmp/qa6/repo/.env.local"}},
		{"pre_tool_bash_apply_patch_cd", "Edit: sub/.env", adapters.HookPreTool, api.ToolEdit,
			[]string{"/private/tmp/qa6/repo/sub/.env"}},
		{"pre_tool_bash_apply_patch_compound", "Run: git status && applypatch <<EOF …", adapters.HookPreTool, api.ToolShell,
			[]string{"/private/tmp/qa6/repo/config/.env.production"}},
		{"pre_tool_mcp", "MCP memory: create_entities", adapters.HookPreTool, api.ToolMCP, nil},
		{"pre_tool_spawn_agent", "spawn_agent: Review the diff for bugs", adapters.HookPreTool, api.ToolOther, nil},
		{"permission_bash", "Run: npm install left-pad", adapters.HookPermission, api.ToolShell, nil},
		{"permission_apply_patch", "Edit: /etc/hosts.new", adapters.HookPermission, api.ToolEdit, []string{"/etc/hosts.new"}},
		{"permission_write_stdin", "Type into running command: rm -rf build", adapters.HookPermission, api.ToolShell, nil},
		{"post_tool_bash", "Run: go test ./...", adapters.HookPostTool, api.ToolShell, nil},
		{"stop", "Finished: Fixed the test; all packages pass.", adapters.HookStop, "", nil},
		{"stop_null_message", "Finished its turn", adapters.HookStop, "", nil},
		{"interrupt", "Interrupted", adapters.HookStop, "", nil},
		{"subagent_stop", "Subagent finished (reviewer)", adapters.HookSubagentStop, "", nil},
		{"session_start", "Session started", adapters.HookSessionStart, "", nil},
		{"session_start_null_transcript", "Session started (resume)", adapters.HookSessionStart, "", nil},
		{"session_end", "Session ended", adapters.HookSessionEnd, "", nil},
		{"pre_compact", "Compacting context (auto)", adapters.HookPreCompact, "", nil},
		{"unknown_post_model_request", "PostModelRequest", adapters.HookUnknown, "", nil},
	} {
		ev := parseFixture(t, tc.fixture)
		if ev.Kind != tc.kind || ev.ToolKind != tc.tool || ev.Summary != tc.summary {
			t.Errorf("%s: kind=%s tool=%s summary=%q", tc.fixture, ev.Kind, ev.ToolKind, ev.Summary)
		}
		if strings.Join(ev.Paths, ",") != strings.Join(tc.paths, ",") {
			t.Errorf("%s: paths=%v", tc.fixture, ev.Paths)
		}
	}
	ev := parseFixture(t, "pre_tool_bash")
	if ev.Command != "git push origin main" || ev.ToolUseID != "call_abc123" || ev.AgentSessionID == "" || !strings.Contains(ev.TranscriptPath, "rollout-") {
		t.Errorf("fields: %+v", ev)
	}
	if ev := parseFixture(t, "session_start_null_transcript"); ev.TranscriptPath != "" || ev.PermissionMode != "bypassPermissions" {
		t.Errorf("null transcript: %+v", ev)
	}
}

func TestEncodeResponse(t *testing.T) {
	a := Adapter{}
	ev := func(k adapters.HookKind) adapters.HookEvent { return adapters.HookEvent{Kind: k} }
	type resp = adapters.HookResponse
	deny := func(reason string) resp { return resp{Stderr: reason + "\n", ExitCode: 2} }
	for _, tc := range []struct {
		name string
		ev   adapters.HookEvent
		out  adapters.HookOutcome
		want resp
	}{
		// Denies are exit 2 + stderr (H4): stdout noise from shell startup
		// files cannot hide them. No trailing period: Codex appends
		// ". Command: …" itself (L10).
		{"pre deny", ev(adapters.HookPreTool), adapters.HookOutcome{Behavior: api.BehaviorDeny, Message: "No force-push."},
			deny("No force-push")},
		{"pre deny needs a reason", ev(adapters.HookPreTool), adapters.HookOutcome{Behavior: api.BehaviorDeny},
			deny("Blocked by Pilothouse policy")},
		{"pre allow unsupported", ev(adapters.HookPreTool), adapters.HookOutcome{Behavior: api.BehaviorAllow, Message: "ok"}, resp{}},
		// Codex cannot ask at PreToolUse; ask becomes a deny (M9).
		{"pre ask from self-protection", ev(adapters.HookPreTool), adapters.HookOutcome{Behavior: api.BehaviorAsk,
			Rule: "pilothouse-self-protection", Message: "Pilothouse self-protection: this writes to ~/.pilothouse/x. A person must confirm it."},
			deny("Pilothouse: a person must approve this, and Codex cannot ask them from a hook — ask the user to run it themselves (Pilothouse self-protection: this writes to ~/.pilothouse/x. A person must confirm it)")},
		{"pre ask self-protection without rule name", ev(adapters.HookPreTool), adapters.HookOutcome{Behavior: api.BehaviorAsk,
			Message: "Pilothouse self-protection: this runs `pilothouse approve`. A person must confirm it."},
			deny("Pilothouse: a person must approve this, and Codex cannot ask them from a hook — ask the user to run it themselves (Pilothouse self-protection: this runs `pilothouse approve`. A person must confirm it)")},
		{"pre ask from a user rule", ev(adapters.HookPreTool), adapters.HookOutcome{Behavior: api.BehaviorAsk, Rule: "ask-push", Message: "Pushing needs a person's OK."},
			deny("Pilothouse: a person must approve this, and Codex cannot ask them from a hook — ask the user to run it themselves or allow it with a Pilothouse rule (Pushing needs a person's OK)")},
		{"permission allow", ev(adapters.HookPermission), adapters.HookOutcome{Behavior: api.BehaviorAllow},
			resp{Stdout: `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}` + "\n"}},
		{"permission deny", ev(adapters.HookPermission), adapters.HookOutcome{Behavior: api.BehaviorDeny, Message: "Denied from phone"},
			deny("Denied from phone")},
		{"permission deny needs a reason", ev(adapters.HookPermission), adapters.HookOutcome{Behavior: api.BehaviorDeny},
			deny("Denied through Pilothouse.")},
		{"permission none", ev(adapters.HookPermission), adapters.HookOutcome{}, resp{}},
		{"permission ask is no opinion", ev(adapters.HookPermission), adapters.HookOutcome{Behavior: api.BehaviorAsk}, resp{}},
		{"prompt deny", ev(adapters.HookPromptSubmit), adapters.HookOutcome{Behavior: api.BehaviorDeny, Message: "no"}, deny("no")},
		{"session context", ev(adapters.HookSessionStart), adapters.HookOutcome{Context: "ctx"},
			resp{Stdout: `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"ctx"}}` + "\n"}},
		{"stop", ev(adapters.HookStop), adapters.HookOutcome{Behavior: api.BehaviorDeny}, resp{}},
	} {
		if got := a.EncodeResponse(tc.ev, tc.out); got != tc.want {
			t.Errorf("%s:\n got %+v\nwant %+v", tc.name, got, tc.want)
		}
	}
	if r := a.FailResponse(adapters.HookPreTool, true); r.ExitCode != 2 || r.Stdout != "" || !strings.Contains(r.Stderr, "approvals.fail_closed") || strings.HasSuffix(r.Stderr, ".\n") {
		t.Errorf("fail closed: %+v", r)
	}
	if r := a.FailResponse(adapters.HookPermission, true); r != (resp{}) {
		t.Errorf("fail closed permission: %+v", r)
	}
	if r := a.FailResponse(adapters.HookPreTool, false); r != (resp{}) {
		t.Errorf("fail open: %+v", r)
	}
}

// TestAskReasonUsesPolicyRuleName pins the rule name the adapter keys on.
func TestAskReasonUsesPolicyRuleName(t *testing.T) {
	if policy.SelfProtectionRule != "pilothouse-self-protection" {
		t.Fatal(policy.SelfProtectionRule)
	}
}

// TestShellNoise: a shell whose startup prints to stdout is reported,
// a quiet one is not.
func TestShellNoise(t *testing.T) {
	dir := t.TempDir()
	noisy := filepath.Join(dir, "noisy-sh")
	if err := os.WriteFile(noisy, []byte("#!/bin/sh\necho 'ZSHENV NOISE'\nexec /bin/sh \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", noisy)
	out, err := ShellNoise()
	if err != nil || out != "ZSHENV NOISE\n" {
		t.Errorf("noisy: %q, %v", out, err)
	}
	t.Setenv("SHELL", "/bin/sh")
	if out, err := ShellNoise(); err != nil || out != "" {
		t.Errorf("quiet: %q, %v", out, err)
	}
	t.Setenv("SHELL", filepath.Join(dir, "missing"))
	if _, err := ShellNoise(); err == nil {
		t.Error("missing shell: no error")
	}
}

func TestMetadata(t *testing.T) {
	home := isolate(t)
	a := Adapter{}
	if Home() != filepath.Join(home, ".codex") {
		t.Errorf("Home = %s (CODEX_HOME must not win under PILOTHOUSE_USER_HOME)", Home())
	}
	if a.MaxHold(adapters.HookPermission) != 600*time.Second || a.MaxHold(adapters.HookPreTool) != 0 {
		t.Error("MaxHold")
	}
	if strings.Join(a.ResumeArgs("019a"), " ") != "codex resume 019a" || a.ResumeArgs("") != nil {
		t.Error("ResumeArgs")
	}
	if pp := a.ProtectedPaths(); pp[0] != filepath.Join(home, ".codex", "config.toml") || len(pp) != 4 {
		t.Errorf("ProtectedPaths = %v", pp)
	}
	if a.Kind() != api.AgentCodex || a.ProcessNames()[0] != "codex" {
		t.Error("identity")
	}
}

func TestHomeHonorsCodexHomeOutsideTests(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PILOTHOUSE_USER_HOME", "")
	t.Setenv("CODEX_HOME", link)
	want, _ := filepath.EvalSymlinks(real)
	if Home() != want {
		t.Errorf("Home = %s, want canonical %s", Home(), want)
	}
}

// Vectors from Codex's own test suite (codex-rs/core/tests/suite/hooks.rs,
// declaration-only trust for a plugin's hooks.json).
func TestTrustHashMatchesCodex(t *testing.T) {
	cmd := "python3 -I -S ${PLUGIN_ROOT}/hooks/pre_tool_use_hook.py"
	for event, want := range map[string]string{
		"PreToolUse":  "sha256:c9ee1aca1b5d8c72b99df6571c87d61350b09f7f4594ae30e7250a2027e9d76b",
		"PostToolUse": "sha256:9eef43ad6f26b1f9b94f318a50445f8bb62c363da08152a2966b73d308bf42ba",
	} {
		if got := trustHash(hookkit.Spec{Event: event, Matcher: "^Bash$", Command: cmd}); got != want {
			t.Errorf("%s: %s, want %s", event, got, want)
		}
	}
	// Matchers are dropped for events without matcher support, and the
	// SessionEnd timeout is clamped, as Codex normalizes before hashing.
	a := trustHash(hookkit.Spec{Event: "Stop", Matcher: "*", Command: "x"})
	if b := trustHash(hookkit.Spec{Event: "Stop", Command: "x"}); a != b {
		t.Error("Stop matcher not ignored")
	}
	if trustHash(hookkit.Spec{Event: "SessionEnd", Command: "x", Timeout: 9}) != trustHash(hookkit.Spec{Event: "SessionEnd", Command: "x", Timeout: 3}) {
		t.Error("SessionEnd timeout not clamped")
	}
	if trustHash(hookkit.Spec{Event: "PreToolUse", Command: "a<b"}) == "" {
		t.Error("empty hash")
	}
}

func TestTrustKey(t *testing.T) {
	got := trustKey("/home/u/.codex/hooks.json", hookkit.Ref{Event: "PermissionRequest", Group: 2, Handler: 0})
	if got != "/home/u/.codex/hooks.json:permission_request:2:0" {
		t.Error(got)
	}
}
