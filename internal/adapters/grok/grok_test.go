package grok

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/hookkit"
	"github.com/xasdta/pilothouse/internal/api"
)

func isolate(t *testing.T) (userHome string) {
	t.Helper()
	userHome = t.TempDir()
	t.Setenv("PILOTHOUSE_HOME", t.TempDir())
	t.Setenv("PILOTHOUSE_USER_HOME", userHome)
	t.Setenv("GROK_HOME", filepath.Join(t.TempDir(), "decoy"))
	run, err := os.MkdirTemp("/tmp", "grok")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(run) })
	t.Setenv("PILOTHOUSE_RUN_DIR", run)
	return userHome
}

func parse(t *testing.T, raw string) adapters.HookEvent {
	t.Helper()
	ev, err := Adapter{}.ParseHook(adapters.HookEnvelope{Agent: api.AgentGrok, Payload: []byte(raw)})
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestParseHookNativeEvents(t *testing.T) {
	for _, tc := range []struct {
		name, raw, summary string
		kind               adapters.HookKind
		tool               api.ToolKind
		paths              []string
	}{
		{"session start", `{"hookEventName":"SessionStart","sessionId":"s1","cwd":"/repo","source":"startup"}`,
			"Session started", adapters.HookSessionStart, "", nil},
		{"prompt", `{"hookEventName":"UserPromptSubmit","sessionId":"s1","cwd":"/repo","prompt":"Fix the build"}`,
			"Prompt: Fix the build", adapters.HookPromptSubmit, "", nil},
		{"bash", `{"hookEventName":"PreToolUse","sessionId":"s1","cwd":"/repo","toolName":"run_terminal_cmd","toolInput":{"command":"go test ./..."}}`,
			"Run: go test ./...", adapters.HookPreTool, api.ToolShell, nil},
		{"edit", `{"hookEventName":"PreToolUse","sessionId":"s1","cwd":"/repo","toolName":"search_replace","toolInput":{"file_path":"/repo/src/app.go"}}`,
			"Edit: src/app.go", adapters.HookPreTool, api.ToolEdit, []string{"/repo/src/app.go"}},
		{"read", `{"hookEventName":"PreToolUse","sessionId":"s1","cwd":"/repo","toolName":"read_file","toolInput":{"path":"/repo/README.md"}}`,
			"Read: README.md", adapters.HookPreTool, api.ToolRead, []string{"/repo/README.md"}},
		{"grep", `{"hookEventName":"PreToolUse","sessionId":"s1","cwd":"/repo","toolName":"grep_search","toolInput":{"pattern":"TODO","path":"src"}}`,
			"Search: TODO in src", adapters.HookPreTool, api.ToolRead, []string{"src"}},
		{"web", `{"hookEventName":"PreToolUse","sessionId":"s1","cwd":"/repo","toolName":"web_search","toolInput":{"query":"Go 1.26 release"}}`,
			"Search the web: Go 1.26 release", adapters.HookPreTool, api.ToolWeb, nil},
		{"mcp", `{"hookEventName":"PreToolUse","sessionId":"s1","cwd":"/repo","toolName":"github__create_issue","toolInput":{}}`,
			"MCP github: create_issue", adapters.HookPreTool, api.ToolMCP, nil},
		{"post", `{"hookEventName":"PostToolUse","sessionId":"s1","cwd":"/repo","toolName":"bash","toolInput":{"command":"make"}}`,
			"Run: make", adapters.HookPostTool, api.ToolShell, nil},
		{"post failure", `{"hookEventName":"PostToolUseFailure","sessionId":"s1","cwd":"/repo","toolName":"bash","toolInput":{"command":"make"}}`,
			"Run: make", adapters.HookPostTool, api.ToolShell, nil},
		{"stop", `{"hookEventName":"Stop","sessionId":"s1","cwd":"/repo","lastAssistantMessage":"Tests pass."}`,
			"Finished: Tests pass.", adapters.HookStop, "", nil},
		{"stop failure", `{"hookEventName":"StopFailure","sessionId":"s1","cwd":"/repo","reason":"rate limited"}`,
			"Stopped: rate limited", adapters.HookStop, "", nil},
		{"stop cancelled snake", `{"hookEventName":"stop_cancelled","sessionId":"s1","cwd":"/repo","reason":"user_interrupt"}`,
			"Stopped: user_interrupt", adapters.HookStop, "", nil},
		{"edit hashline", `{"hookEventName":"pre_tool_use","sessionId":"s1","cwd":"/repo","toolName":"hashline_edit","toolInput":{"path":"/repo/x.go"}}`,
			"Edit: x.go", adapters.HookPreTool, api.ToolEdit, []string{"/repo/x.go"}},
		{"edit notebook", `{"hookEventName":"PreToolUse","sessionId":"s1","cwd":"/repo","toolName":"edit_notebook","toolInput":{"target_notebook":"/repo/n.ipynb"}}`,
			"Edit: n.ipynb", adapters.HookPreTool, api.ToolEdit, []string{"/repo/n.ipynb"}},
		{"denied", `{"hookEventName":"PermissionDenied","sessionId":"s1","cwd":"/repo","message":"user said no"}`,
			"Permission denied: user said no", adapters.HookUnknown, "", nil},
		{"notification", `{"hookEventName":"Notification","sessionId":"s1","cwd":"/repo","message":"Waiting for input","notificationType":"idle"}`,
			"Waiting for input", adapters.HookNotification, "", nil},
		{"session end", `{"hookEventName":"SessionEnd","sessionId":"s1","cwd":"/repo","reason":"logout"}`,
			"Session ended (logout)", adapters.HookSessionEnd, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := parse(t, tc.raw)
			if ev.Kind != tc.kind || ev.ToolKind != tc.tool || ev.Summary != tc.summary {
				t.Errorf("kind=%s tool=%s summary=%q", ev.Kind, ev.ToolKind, ev.Summary)
			}
			if strings.Join(ev.Paths, ",") != strings.Join(tc.paths, ",") {
				t.Errorf("paths=%v", ev.Paths)
			}
		})
	}
}

func TestParseHookSnakeCase(t *testing.T) {
	ev := parse(t, `{"hook_event_name":"PreToolUse","session_id":"s1","cwd":"/repo","permission_mode":"auto",
		"tool_name":"search_replace","tool_input":{"filePath":"/repo/x.go"},"tool_use_id":"call_1"}`)
	if ev.Kind != adapters.HookPreTool || ev.AgentSessionID != "s1" || ev.ToolUseID != "call_1" ||
		ev.PermissionMode != "auto" || ev.ToolKind != api.ToolEdit {
		t.Fatalf("%+v", ev)
	}
	if len(ev.Paths) != 1 || ev.Paths[0] != "/repo/x.go" || ev.Summary != "Edit: x.go" {
		t.Errorf("paths=%v summary=%q", ev.Paths, ev.Summary)
	}
}

func TestParseHookUnknownAndErrors(t *testing.T) {
	if ev := parse(t, `{"hookEventName":"PostCompact","cwd":"/repo"}`); ev.Kind != adapters.HookUnknown || ev.Summary != "PostCompact" {
		t.Errorf("%+v", ev)
	}
	if _, err := (Adapter{}).ParseHook(adapters.HookEnvelope{Payload: []byte(`{}`)}); err == nil {
		t.Error("payload without an event accepted")
	}
	if _, err := (Adapter{}).ParseHook(adapters.HookEnvelope{Payload: []byte(`{nope`)}); err == nil {
		t.Error("invalid JSON accepted")
	}
}

func TestEncodeResponse(t *testing.T) {
	a := Adapter{}
	ev := adapters.HookEvent{Kind: adapters.HookPreTool}
	for _, tc := range []struct {
		name, decision string
		out            adapters.HookOutcome
		want           string
	}{
		{"deny", "deny", adapters.HookOutcome{Behavior: api.BehaviorDeny, Message: "No force-push."}, "No force-push."},
		{"deny default", "deny", adapters.HookOutcome{Behavior: api.BehaviorDeny}, hookkit.DefaultDenyReason},
		{"ask", "ask", adapters.HookOutcome{Behavior: api.BehaviorAsk, Rule: "ask-push", Message: "Pushing needs a person's OK."}, "Pushing needs a person's OK."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := a.EncodeResponse(ev, tc.out)
			var got struct {
				Decision string `json:"decision"`
				Reason   string `json:"reason"`
			}
			if err := json.Unmarshal([]byte(resp.Stdout), &got); err != nil {
				t.Fatalf("stdout %q: %v", resp.Stdout, err)
			}
			if got.Decision != tc.decision || !strings.Contains(got.Reason, tc.want) {
				t.Errorf("%+v", got)
			}
		})
	}
	if resp := a.EncodeResponse(ev, adapters.HookOutcome{Behavior: api.BehaviorAllow}); resp.Stdout != "" || resp.ExitCode != 0 {
		t.Errorf("allow = %+v", resp)
	}
	if resp := a.EncodeResponse(adapters.HookEvent{Kind: adapters.HookPostTool}, adapters.HookOutcome{Behavior: api.BehaviorDeny}); resp.Stdout != "" {
		t.Errorf("post_tool deny = %+v", resp)
	}
}

func TestFailResponse(t *testing.T) {
	a := Adapter{}
	if resp := a.FailResponse(adapters.HookPreTool, true); !strings.Contains(resp.Stdout, `"decision":"deny"`) {
		t.Errorf("%+v", resp)
	}
	if resp := a.FailResponse(adapters.HookPreTool, false); resp.Stdout != "" || resp.ExitCode != 0 {
		t.Errorf("%+v", resp)
	}
}

func TestAskReason(t *testing.T) {
	if got := askReason(adapters.HookOutcome{Behavior: api.BehaviorAsk}); got != AskPrefix+"." {
		t.Errorf("empty reason: %q", got)
	}
	got := askReason(adapters.HookOutcome{Behavior: api.BehaviorAsk, Message: "  Pushing \n needs a person.  "})
	if got != AskPrefix+": Pushing needs a person." {
		t.Errorf("reason: %q", got)
	}
}

const userHooks = `{
  "description": "my hooks",
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "/home/user/bin/check.sh"}]}
    ]
  }
}
`

func writeHooks(t *testing.T, home, doc string) {
	t.Helper()
	dir := filepath.Join(home, ".grok", "hooks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if doc != "" {
		if err := os.WriteFile(filepath.Join(dir, "pilothouse.json"), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func fakeShim(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pilothouse")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInstallRoundTrip(t *testing.T) {
	home := isolate(t)
	writeHooks(t, home, userHooks)
	shim := fakeShim(t)
	a := Adapter{}
	opts := adapters.InstallOptions{ShimPath: shim}

	dry, err := a.InstallHooks(adapters.InstallOptions{ShimPath: shim, DryRun: true})
	if err != nil || !dry.Changed {
		t.Fatalf("%+v %v", dry, err)
	}
	if read(t, HooksPath()) != userHooks {
		t.Fatal("dry run wrote")
	}

	ch, err := a.InstallHooks(opts)
	if err != nil || ch.Backup == "" {
		t.Fatalf("%+v %v", ch, err)
	}
	var doc struct {
		Description string                                  `json:"description"`
		Hooks       map[string][]map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(read(t, HooksPath())), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Description != "my hooks" || len(doc.Hooks) != len(wiredEvents) || len(doc.Hooks["PreToolUse"]) != 2 {
		t.Errorf("hooks:\n%s", read(t, HooksPath()))
	}
	if st, err := a.Status(opts); err != nil || !st.Installed || !st.Current || len(st.Problems) != 0 {
		t.Errorf("status: %+v %v", st, err)
	}
	if again, _ := a.InstallHooks(opts); again.Changed {
		t.Errorf("second install changed: %+v", again)
	}

	un, err := a.UninstallHooks(opts)
	if err != nil || !un.Changed {
		t.Fatalf("%+v %v", un, err)
	}
	var restored, original any
	_ = json.Unmarshal([]byte(read(t, HooksPath())), &restored)
	_ = json.Unmarshal([]byte(userHooks), &original)
	x, _ := json.Marshal(restored)
	y, _ := json.Marshal(original)
	if string(x) != string(y) {
		t.Errorf("not restored:\n%s", read(t, HooksPath()))
	}
}

func TestInstallFreshHome(t *testing.T) {
	home := isolate(t)
	a := Adapter{}
	shim := fakeShim(t)
	ch, err := a.InstallHooks(adapters.InstallOptions{ShimPath: shim})
	if err != nil || ch.Backup != "" {
		t.Fatalf("%+v %v", ch, err)
	}
	if ch.Path != filepath.Join(home, ".grok", "hooks", "pilothouse.json") {
		t.Errorf("path %s", ch.Path)
	}
	if st, _ := a.Status(adapters.InstallOptions{}); !st.Installed || !st.Current {
		t.Errorf("status without a shim path: %+v", st)
	}
	if _, err := a.UninstallHooks(adapters.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if st, _ := a.Status(adapters.InstallOptions{}); st.Installed {
		t.Errorf("still installed: %+v", st)
	}
}

func TestStatusReportsMissingEvents(t *testing.T) {
	home := isolate(t)
	writeHooks(t, home, `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"pilothouse hook grok"}]}]}}`)
	st, err := Adapter{}.Status(adapters.InstallOptions{ShimPath: fakeShim(t)})
	if err != nil {
		t.Fatal(err)
	}
	if st.Current || !st.Installed || len(st.Problems) == 0 || !strings.Contains(st.Problems[0], "not wired") {
		t.Errorf("%+v", st)
	}
}

func TestInstallRefusesInvalidJSON(t *testing.T) {
	home := isolate(t)
	writeHooks(t, home, "{nope")
	if _, err := (Adapter{}).InstallHooks(adapters.InstallOptions{ShimPath: fakeShim(t)}); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	if read(t, HooksPath()) != "{nope" {
		t.Error("file touched")
	}
}

func TestHomeUsesGrokHomeInsideUserHome(t *testing.T) {
	userHome := isolate(t)
	dir := filepath.Join(userHome, "custom")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", dir)
	real, _ := filepath.EvalSymlinks(dir)
	if got := Home(); got != real {
		t.Errorf("Home = %q, want %q", got, real)
	}
	if got := HooksPath(); got != filepath.Join(real, "hooks", "pilothouse.json") {
		t.Errorf("HooksPath = %q", got)
	}
}
