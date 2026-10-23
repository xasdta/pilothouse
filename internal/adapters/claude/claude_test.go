package claude

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
	"github.com/xasdta/pilothouse/internal/api"
)

var update = flag.Bool("update", false, "rewrite golden files")

// isolate points every Pilothouse path at temp dirs (BUILD-SPEC invariant 1):
// these tests write settings files and must never see the real home.
func isolate(t *testing.T) (userHome string) {
	t.Helper()
	userHome = t.TempDir()
	t.Setenv("PILOTHOUSE_HOME", t.TempDir())
	t.Setenv("PILOTHOUSE_USER_HOME", userHome)
	run, err := os.MkdirTemp("/tmp", "ag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(run) })
	t.Setenv("PILOTHOUSE_RUN_DIR", run)
	return userHome
}

// TestParseHookGolden parses every fixture in testdata/hooks (payloads
// captured from Claude Code 2.1.309, or copied from the hooks reference
// for events a headless run cannot trigger: files named doc_*) and
// compares the normalized event with testdata/golden.
func TestParseHookGolden(t *testing.T) {
	files, _ := filepath.Glob("testdata/hooks/*.json")
	if len(files) < 20 {
		t.Fatalf("only %d fixtures", len(files))
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		t.Run(name, func(t *testing.T) {
			payload, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			ev, err := Adapter{}.ParseHook(adapters.HookEnvelope{Agent: api.AgentClaude, Payload: payload, Cwd: "/fallback"})
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
			got := buf.Bytes()
			golden := filepath.Join("testdata", "golden", name+".json")
			if *update {
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run go test -update)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("ParseHook mismatch:\ngot:\n%s\nwant:\n%s", got, want)
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

// Spot checks on top of the goldens, so a bad -update cannot silently
// bless a regression in the fields the daemon depends on.
func TestParseHookKeyFields(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		kind    adapters.HookKind
		tool    api.ToolKind
		summary string
		paths   []string
	}{
		{"pre_tool_bash", adapters.HookPreTool, api.ToolShell, "Run: echo hi > out.txt", nil},
		{"pre_tool_write", adapters.HookPreTool, api.ToolEdit, "Write: notes.txt", []string{"/home/user/project/notes.txt"}},
		{"pre_tool_edit", adapters.HookPreTool, api.ToolEdit, "Edit: notes.txt", []string{"/home/user/project/notes.txt"}},
		{"pre_tool_read", adapters.HookPreTool, api.ToolRead, "Read: out.txt", []string{"/home/user/project/out.txt"}},
		{"doc_pre_tool_multiedit", adapters.HookPreTool, api.ToolEdit, "Edit: src/app.go", []string{"/home/user/project/src/app.go"}},
		{"doc_pre_tool_notebookedit", adapters.HookPreTool, api.ToolEdit, "Edit: analysis.ipynb", []string{"/home/user/project/analysis.ipynb"}},
		{"doc_pre_tool_glob", adapters.HookPreTool, api.ToolRead, "Find files: **/*.ts in src", []string{"/home/user/project/src"}},
		{"doc_pre_tool_grep", adapters.HookPreTool, api.ToolRead, "Search: TODO.*fix in .", []string{"/home/user/project"}},
		{"doc_pre_tool_webfetch", adapters.HookPreTool, api.ToolWeb, "Fetch: https://example.com/api", nil},
		{"doc_pre_tool_websearch", adapters.HookPreTool, api.ToolWeb, "Search the web: react hooks best practices", nil},
		{"doc_pre_tool_mcp", adapters.HookPreTool, api.ToolMCP, "MCP memory: create_entities", nil},
		{"pre_tool_toolsearch", adapters.HookPreTool, api.ToolOther, "ToolSearch: select:Glob,Grep", nil},
		{"permission_bash", adapters.HookPermission, api.ToolShell, "Run: echo hi > out.txt", nil},
		{"doc_permission_mcp", adapters.HookPermission, api.ToolMCP, "MCP github: create_pull_request", nil},
		{"post_tool_failure_read", adapters.HookPostTool, api.ToolRead, "Read: out.txt", []string{"/home/user/project/out.txt"}},
		{"session_start", adapters.HookSessionStart, "", "Session started", nil},
		{"session_end", adapters.HookSessionEnd, "", "Session ended", nil},
		{"doc_notification_permission", adapters.HookNotification, "", "Claude needs your permission to use Bash", nil},
		{"doc_subagent_stop", adapters.HookSubagentStop, "", "Subagent finished (Explore)", nil},
		{"doc_pre_compact", adapters.HookPreCompact, "", "Compacting context (manual)", nil},
		{"doc_unknown_event", adapters.HookUnknown, "", "CwdChanged", nil},
	} {
		ev := parseFixture(t, tc.fixture)
		if ev.Kind != tc.kind || ev.ToolKind != tc.tool || ev.Summary != tc.summary {
			t.Errorf("%s: kind=%s tool=%s summary=%q", tc.fixture, ev.Kind, ev.ToolKind, ev.Summary)
		}
		if strings.Join(ev.Paths, ",") != strings.Join(tc.paths, ",") {
			t.Errorf("%s: paths=%v want %v", tc.fixture, ev.Paths, tc.paths)
		}
	}
	ev := parseFixture(t, "pre_tool_bash")
	if ev.AgentSessionID == "" || ev.ToolUseID == "" || ev.Command != "echo hi > out.txt" || ev.Cwd != "/home/user/project" || ev.PermissionMode != "default" || ev.TranscriptPath == "" {
		t.Errorf("pre_tool_bash fields: %+v", ev)
	}
	if ev := parseFixture(t, "stop"); !strings.HasPrefix(ev.Message, "Blocked: the Bash write") {
		t.Errorf("stop message = %q", ev.Message)
	}
	if ev := parseFixture(t, "user_prompt_submit"); !strings.HasPrefix(ev.Message, "Do these steps") || ev.Kind != adapters.HookPromptSubmit {
		t.Errorf("prompt = %+v", ev)
	}
	if ev := parseFixture(t, "doc_notification_idle"); ev.NotificationType != "idle_prompt" {
		t.Errorf("notification type = %q", ev.NotificationType)
	}
}

func TestParseHookErrorsAndFallbacks(t *testing.T) {
	a := Adapter{}
	for _, payload := range []string{"", "not json", `{"session_id":"x"}`} {
		if _, err := a.ParseHook(adapters.HookEnvelope{Payload: json.RawMessage(payload)}); err == nil {
			t.Errorf("%q parsed", payload)
		}
	}
	// The event name may come from argv; cwd from the shim.
	ev, err := a.ParseHook(adapters.HookEnvelope{Event: "Stop", Cwd: "/w", Payload: json.RawMessage(`{"session_id":"s"}`)})
	if err != nil || ev.Kind != adapters.HookStop || ev.Cwd != "/w" || ev.Summary != "Finished its turn" {
		t.Errorf("fallbacks: %+v %v", ev, err)
	}
	long := strings.Repeat("x", 10000)
	ev, _ = a.ParseHook(adapters.HookEnvelope{Payload: json.RawMessage(`{"hook_event_name":"UserPromptSubmit","prompt":"` + long + `"}`)})
	if len(ev.Message) != 4096 || len([]rune(ev.Summary)) > 110 {
		t.Errorf("message not capped: %d / %d", len(ev.Message), len(ev.Summary))
	}
}

func TestEncodeResponse(t *testing.T) {
	a := Adapter{}
	ev := func(k adapters.HookKind) adapters.HookEvent { return adapters.HookEvent{Kind: k} }
	for _, tc := range []struct {
		name string
		ev   adapters.HookEvent
		out  adapters.HookOutcome
		want string
	}{
		{"pre allow", ev(adapters.HookPreTool), adapters.HookOutcome{Behavior: api.BehaviorAllow, Message: "rule ok"},
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","permissionDecisionReason":"rule ok"}}`},
		{"pre deny", ev(adapters.HookPreTool), adapters.HookOutcome{Behavior: api.BehaviorDeny, Message: "Force-push is blocked by policy. Open a PR instead."},
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"Force-push is blocked by policy. Open a PR instead."}}`},
		{"pre deny no message", ev(adapters.HookPreTool), adapters.HookOutcome{Behavior: api.BehaviorDeny},
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"Blocked by Pilothouse policy."}}`},
		{"pre ask", ev(adapters.HookPreTool), adapters.HookOutcome{Behavior: api.BehaviorAsk, Message: "Edits to agent settings need a person (pilothouse-self-protection)"},
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"ask","permissionDecisionReason":"Edits to agent settings need a person (pilothouse-self-protection)"}}`},
		{"pre none", ev(adapters.HookPreTool), adapters.HookOutcome{}, ``},
		{"permission allow", ev(adapters.HookPermission), adapters.HookOutcome{Behavior: api.BehaviorAllow},
			`{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`},
		{"permission deny", ev(adapters.HookPermission), adapters.HookOutcome{Behavior: api.BehaviorDeny, Message: "Not now <phone>"},
			`{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"Not now <phone>"}}}`},
		{"permission none", ev(adapters.HookPermission), adapters.HookOutcome{}, ``},
		{"permission ask", ev(adapters.HookPermission), adapters.HookOutcome{Behavior: api.BehaviorAsk}, ``},
		{"session context", ev(adapters.HookSessionStart), adapters.HookOutcome{Context: "Pilothouse is watching."},
			`{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"Pilothouse is watching."}}`},
		{"prompt block", ev(adapters.HookPromptSubmit), adapters.HookOutcome{Behavior: api.BehaviorDeny, Message: "no"},
			`{"decision":"block","reason":"no"}`},
		{"prompt none", ev(adapters.HookPromptSubmit), adapters.HookOutcome{}, ``},
		{"stop never blocks", ev(adapters.HookStop), adapters.HookOutcome{Behavior: api.BehaviorDeny}, ``},
		{"post", ev(adapters.HookPostTool), adapters.HookOutcome{Behavior: api.BehaviorAllow}, ``},
	} {
		got := a.EncodeResponse(tc.ev, tc.out)
		if strings.TrimSuffix(got.Stdout, "\n") != tc.want || got.ExitCode != 0 || got.Stderr != "" {
			t.Errorf("%s: got %+v\nwant %s", tc.name, got, tc.want)
		}
		if got.Stdout != "" && !json.Valid([]byte(got.Stdout)) {
			t.Errorf("%s: invalid JSON", tc.name)
		}
	}
}

func TestFailResponse(t *testing.T) {
	a := Adapter{}
	for _, k := range []adapters.HookKind{adapters.HookPreTool, adapters.HookPermission, adapters.HookStop, adapters.HookSessionEnd} {
		if r := a.FailResponse(k, false); r != (adapters.HookResponse{}) {
			t.Errorf("%s fail-open = %+v", k, r)
		}
	}
	r := a.FailResponse(adapters.HookPreTool, true)
	if !strings.Contains(r.Stdout, `"permissionDecision":"deny"`) || !strings.Contains(r.Stdout, "pilothouse start") || r.ExitCode != 0 {
		t.Errorf("fail-closed = %+v", r)
	}
	if r := a.FailResponse(adapters.HookPermission, true); r != (adapters.HookResponse{}) {
		t.Errorf("fail-closed permission = %+v (the native dialog should show)", r)
	}
}

func TestMetadata(t *testing.T) {
	home := isolate(t)
	a := Adapter{}
	if a.Kind() != api.AgentClaude || a.DisplayName() != "Claude Code" {
		t.Error("identity")
	}
	if a.MaxHold(adapters.HookPermission) != 600*time.Second || a.MaxHold(adapters.HookPreTool) != 0 {
		t.Error("MaxHold")
	}
	if got := a.ResumeArgs("abc"); strings.Join(got, " ") != "claude --resume abc" {
		t.Errorf("ResumeArgs = %v", got)
	}
	if a.ResumeArgs("") != nil {
		t.Error("ResumeArgs empty id")
	}
	pp := a.ProtectedPaths()
	if pp[0] != filepath.Join(home, ".claude", "settings.json") || !strings.HasPrefix(pp[2], "**/") {
		t.Errorf("ProtectedPaths = %v", pp)
	}
	if d := a.Detect(); d.SettingsPath != filepath.Join(home, ".claude", "settings.json") {
		t.Errorf("Detect = %+v", d)
	}
}

// TestParseBashApplyPatchPaths: a patch fed to an apply_patch program
// through a heredoc names files that path rules must see (M10); the call
// stays a shell command.
func TestParseBashApplyPatchPaths(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"session_id": "s1", "cwd": "/repo", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "t1",
		"tool_input": map[string]any{"command": "apply_patch <<'EOF'\n*** Begin Patch\n*** Add File: .env.local\n+x\n*** End Patch\nEOF\n"},
	})
	ev, err := Adapter{}.ParseHook(adapters.HookEnvelope{Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if ev.ToolKind != api.ToolShell || len(ev.Paths) != 1 || ev.Paths[0] != "/repo/.env.local" {
		t.Errorf("%+v", ev)
	}
}
