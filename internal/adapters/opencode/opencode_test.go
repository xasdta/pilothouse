package opencode

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/policy"
)

var update = flag.Bool("update", false, "rewrite golden files")

// isolate points every Pilothouse path at temp dirs (BUILD-SPEC invariant
// 1). XDG_CONFIG_HOME and OPENCODE_CONFIG_DIR are decoys outside the user
// home: PILOTHOUSE_USER_HOME must win over them.
func isolate(t *testing.T) (userHome string) {
	t.Helper()
	userHome = t.TempDir()
	t.Setenv("PILOTHOUSE_HOME", t.TempDir())
	t.Setenv("PILOTHOUSE_USER_HOME", userHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "decoy"))
	t.Setenv("OPENCODE_CONFIG_DIR", filepath.Join(t.TempDir(), "decoy2"))
	return userHome
}

func writeGolden(t *testing.T, golden string, got []byte) {
	t.Helper()
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
		t.Errorf("%s mismatch:\ngot:\n%s\nwant:\n%s", golden, got, want)
	}
}

func encodeJSON(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func parse(t *testing.T, payload []byte) adapters.HookEvent {
	t.Helper()
	ev, err := Adapter{}.ParseHook(adapters.HookEnvelope{Agent: api.AgentOpenCode, Payload: payload, Cwd: "/elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// TestParseHookGolden: fixtures are plugin payloads (plugin/pilothouse.js)
// built from OpenCode 1.18.25's events and tool arguments.
func TestParseHookGolden(t *testing.T) {
	files, _ := filepath.Glob("testdata/hooks/*.json")
	if len(files) < 25 {
		t.Fatalf("only %d fixtures", len(files))
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		t.Run(name, func(t *testing.T) {
			payload, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			ev := parse(t, payload)
			writeGolden(t, filepath.Join("testdata", "golden", name+".json"), encodeJSON(t, ev))
		})
	}
}

func fixture(t *testing.T, name string) adapters.HookEvent {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "hooks", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return parse(t, b)
}

func TestParseHookKinds(t *testing.T) {
	cases := map[string]adapters.HookKind{
		"tool_before_bash": adapters.HookPreTool, "tool_after_bash": adapters.HookPostTool,
		"permission_bash": adapters.HookPermission, "question_single": adapters.HookPermission,
		"ask": adapters.HookPermission, "permission_replied": adapters.HookPostTool,
		"question_rejected": adapters.HookPostTool, "session_created": adapters.HookSessionStart,
		"session_idle": adapters.HookStop, "chat_message": adapters.HookPromptSubmit,
		"session_deleted": adapters.HookSessionEnd, "compacting": adapters.HookPreCompact,
		"session_updated_title": adapters.HookNotification,
	}
	for name, want := range cases {
		if got := fixture(t, name).Kind; got != want {
			t.Errorf("%s: kind %s, want %s", name, got, want)
		}
	}
}

func TestParseHookDetails(t *testing.T) {
	ev := fixture(t, "permission_bash")
	if ev.Tool != "bash" || ev.ToolKind != api.ToolShell || ev.Command != "git push origin main" ||
		ev.ToolUseID != "per_16b2338ac001vcFIqYJbypy2WI" || ev.Summary != "Run: git push origin main" {
		t.Errorf("permission_bash: %+v", ev)
	}
	if ev.Hold {
		t.Error("an OpenCode permission prompt must not be held at the desk")
	}
	if ev := fixture(t, "ask"); !ev.Hold || ev.Kind != adapters.HookPermission || ev.Command != "git push --force" {
		t.Errorf("ask: %+v", ev)
	}
	// The dialog's answer matches its request by id only.
	if ev := fixture(t, "permission_replied"); ev.Tool != "" || ev.ToolUseID != "per_16b2338ac001vcFIqYJbypy2WI" {
		t.Errorf("replied: tool %q id %q", ev.Tool, ev.ToolUseID)
	}
	// Relative paths resolve against the session's directory.
	if ev := fixture(t, "tool_before_write_relative"); len(ev.Paths) != 1 || ev.Paths[0] != "/work/repo/notes/todo.md" {
		t.Errorf("write paths %v", ev.Paths)
	}
	if ev := fixture(t, "permission_external_directory"); !strings.HasPrefix(ev.Summary, "Work outside the project") ||
		!contains(ev.Paths, "/etc") || ev.Command != "ls /etc" {
		t.Errorf("external_directory: %+v", ev)
	}
	if ev := fixture(t, "permission_without_call"); ev.Command != "rm -rf build" || ev.ToolKind != api.ToolShell {
		t.Errorf("permission from metadata: %+v", ev)
	}
	// A subagent's tool call belongs to the session a person sees.
	if ev := fixture(t, "subagent_tool"); ev.AgentSessionID != "ses_e94dcca81ffed3xpV80ryjnePM" {
		t.Errorf("subagent session %q", ev.AgentSessionID)
	}
	if ev := fixture(t, "session_idle"); !strings.HasPrefix(ev.Message, "Done. I pushed") {
		t.Errorf("idle message %q", ev.Message)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestParseHookQuestions(t *testing.T) {
	ev := fixture(t, "question_multi")
	if len(ev.Questions) != 2 {
		t.Fatalf("questions %+v", ev.Questions)
	}
	q := ev.Questions[0]
	if q.ID != "que_multi#1" || q.Key() != "que_multi#1" || !q.MultiSelect || q.AllowOther || len(q.Options) != 3 {
		t.Errorf("q0 %+v", q)
	}
	if q := ev.Questions[1]; q.ID != "que_multi#2" || q.MultiSelect || !q.AllowOther {
		t.Errorf("q1 %+v (OpenCode allows free text unless custom is false)", q)
	}
	if ev.Summary != "Asks: Which checks?" || ev.Tool != "question" {
		t.Errorf("summary %q tool %q", ev.Summary, ev.Tool)
	}
}

func TestParseHookRejects(t *testing.T) {
	for name, payload := range map[string]string{
		"empty":    "",
		"garbage":  "{",
		"no event": `{"v":1}`,
		"future":   `{"v":99,"event":"session.idle"}`,
	} {
		if _, err := (Adapter{}).ParseHook(adapters.HookEnvelope{Payload: []byte(payload)}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	ev, err := Adapter{}.ParseHook(adapters.HookEnvelope{Payload: []byte(`{"v":1,"event":"something.new","session_id":"s"}`)})
	if err != nil || ev.Kind != adapters.HookUnknown {
		t.Errorf("unknown event: %v %+v", err, ev)
	}
	// The event may come from the envelope alone.
	ev, err = Adapter{}.ParseHook(adapters.HookEnvelope{Event: "session.idle", Payload: []byte(`{"v":1,"session_id":"s"}`)})
	if err != nil || ev.Kind != adapters.HookStop {
		t.Errorf("envelope event: %v %+v", err, ev)
	}
}

func TestQuestionsAreBounded(t *testing.T) {
	var qs []map[string]any
	for i := 0; i < 20; i++ {
		var opts []map[string]string
		for j := 0; j < 30; j++ {
			opts = append(opts, map[string]string{"label": strings.Repeat("x", 500), "description": "d"})
		}
		qs = append(qs, map[string]any{"question": strings.Repeat("q", 5000), "options": opts})
	}
	b, _ := json.Marshal(map[string]any{"v": 1, "event": "question.asked", "session_id": "s", "request_id": "que_1", "questions": qs})
	ev := parse(t, b)
	if len(ev.Questions) != maxQuestions || len(ev.Questions[0].Options) != maxOptions ||
		len(ev.Questions[0].Text) > 2000 || len(ev.Questions[0].Options[0].Label) > 200 {
		t.Errorf("not bounded: %d questions, %d options, text %d", len(ev.Questions), len(ev.Questions[0].Options), len(ev.Questions[0].Text))
	}
}

func decodeAnswer(t *testing.T, r adapters.HookResponse) answer {
	t.Helper()
	if r.ExitCode != 0 || r.Stderr != "" {
		t.Fatalf("plugin responses are JSON on stdout only: %+v", r)
	}
	var a answer
	if r.Stdout == "" {
		return a
	}
	if err := json.Unmarshal([]byte(r.Stdout), &a); err != nil {
		t.Fatalf("bad answer %q: %v", r.Stdout, err)
	}
	return a
}

func TestEncodeResponse(t *testing.T) {
	ad := Adapter{}
	pre := fixture(t, "tool_before_bash")
	perm := fixture(t, "permission_bash")
	cases := []struct {
		name string
		ev   adapters.HookEvent
		out  adapters.HookOutcome
		want answer
	}{
		{"pre deny", pre, adapters.HookOutcome{Behavior: api.BehaviorDeny, Message: "No pushes.", Rule: "no-push"},
			answer{Action: "deny", Message: "No pushes.", Rule: "no-push"}},
		{"pre deny without message", pre, adapters.HookOutcome{Behavior: api.BehaviorDeny}, answer{Action: "deny", Message: "Blocked by Pilothouse policy."}},
		{"pre ask", pre, adapters.HookOutcome{Behavior: api.BehaviorAsk, Message: "Ask first", Rule: "ask-push"},
			answer{Action: "ask", Message: "Ask first", Rule: "ask-push", Summary: "Run: git push origin main"}},
		{"pre allow is no opinion", pre, adapters.HookOutcome{Behavior: api.BehaviorAllow}, answer{}},
		{"pre none", pre, adapters.HookOutcome{}, answer{}},
		{"perm allow", perm, adapters.HookOutcome{Behavior: api.BehaviorAllow}, answer{Action: "allow"}},
		{"perm deny", perm, adapters.HookOutcome{Behavior: api.BehaviorDeny, Message: "Not now"}, answer{Action: "deny", Message: "Not now"}},
		{"perm deny default", perm, adapters.HookOutcome{Behavior: api.BehaviorDeny}, answer{Action: "deny", Message: "Denied through Pilothouse."}},
		{"perm none", perm, adapters.HookOutcome{}, answer{}},
		{"stop", fixture(t, "session_idle"), adapters.HookOutcome{Behavior: api.BehaviorDeny}, answer{}},
	}
	for _, c := range cases {
		got := decodeAnswer(t, ad.EncodeResponse(c.ev, c.out))
		gb, _ := json.Marshal(got)
		wb, _ := json.Marshal(c.want)
		if !bytes.Equal(gb, wb) {
			t.Errorf("%s: got %s, want %s", c.name, gb, wb)
		}
	}
}

func TestEncodeQuestionAnswers(t *testing.T) {
	ad := Adapter{}
	single := fixture(t, "question_single")
	a := decodeAnswer(t, ad.EncodeResponse(single, adapters.HookOutcome{Behavior: api.BehaviorAllow,
		Answers: map[string]string{"que_16b233996001qsrPGesT3b34ed#1": "Blue"}}))
	if a.Action != "answer" || len(a.Answers) != 1 || strings.Join(a.Answers[0], "|") != "Blue" {
		t.Errorf("single: %+v", a)
	}
	multi := fixture(t, "question_multi")
	a = decodeAnswer(t, ad.EncodeResponse(multi, adapters.HookOutcome{Behavior: api.BehaviorAllow,
		Answers: map[string]string{"que_multi#1": api.JoinAnswers([]string{"Lint", "Unit, fast"}), "que_multi#2": "Maybe later"}}))
	if a.Action != "answer" || len(a.Answers) != 2 {
		t.Fatalf("multi: %+v", a)
	}
	if got := strings.Join(a.Answers[0], "|"); got != "Lint|Unit, fast" {
		t.Errorf("multi-select split %q (a label containing \", \" must survive)", got)
	}
	if got := strings.Join(a.Answers[1], "|"); got != "Maybe later" {
		t.Errorf("free text %q", got)
	}
	// Unanswered questions get an empty answer; nothing is invented.
	a = decodeAnswer(t, ad.EncodeResponse(multi, adapters.HookOutcome{Behavior: api.BehaviorAllow, Answers: map[string]string{"que_multi#2": "Yes"}}))
	if len(a.Answers) != 2 || len(a.Answers[0]) != 0 || a.Answers[1][0] != "Yes" {
		t.Errorf("partial: %+v", a)
	}
	// A question that is denied is rejected.
	if a := decodeAnswer(t, ad.EncodeResponse(single, adapters.HookOutcome{Behavior: api.BehaviorDeny})); a.Action != "deny" {
		t.Errorf("deny: %+v", a)
	}
}

func TestSplitLabels(t *testing.T) {
	q := api.Question{MultiSelect: true, Options: []api.QuestionOption{{Label: "A"}, {Label: "A, B"}, {Label: "C"}}}
	for in, want := range map[string]string{
		"A":       "A",
		"A, B":    "A, B", // "A" then "B" is no split ("B" is not a label): the label "A, B"
		"A, B, C": "A, B|C",
		"C, A":    "C|A",
		"C, A, B": "C|A, B",
		"D, E":    "D, E", // not labels: free text, one answer
	} {
		parts := splitLabels(q, in)
		if got := strings.Join(parts, "|"); got != want {
			t.Errorf("%q: split %q, want %q", in, got, want)
		}
		if strings.Join(parts, api.MultiAnswerSep) != in {
			t.Errorf("%q: split %q does not rebuild the answer", in, parts)
		}
	}
}

func TestFailResponse(t *testing.T) {
	ad := Adapter{}
	if a := decodeAnswer(t, ad.FailResponse(adapters.HookPreTool, true)); a.Action != "deny" || !strings.Contains(a.Message, "fail_closed") {
		t.Errorf("fail closed: %+v", a)
	}
	for _, k := range []adapters.HookKind{adapters.HookPreTool, adapters.HookPermission, adapters.HookStop} {
		if r := ad.FailResponse(k, false); r.Stdout != "" || r.ExitCode != 0 {
			t.Errorf("%s fail open: %+v", k, r)
		}
	}
	if r := ad.FailResponse(adapters.HookPermission, true); r.Stdout != "" {
		t.Errorf("fail_closed only blocks tool calls: %+v", r)
	}
}

func TestMaxHold(t *testing.T) {
	ad := Adapter{}
	if ad.MaxHold(adapters.HookPermission) < 10*60e9 {
		t.Error("permission prompts must be holdable past the default hold limit")
	}
	if ad.MaxHold(adapters.HookPreTool) != 0 || ad.MaxHold(adapters.HookStop) != 0 {
		t.Error("only permission hooks may block")
	}
}

func TestConfigDir(t *testing.T) {
	uh := isolate(t)
	if got, want := ConfigDir(), filepath.Join(uh, ".config", "opencode"); got != want {
		t.Errorf("decoy XDG_CONFIG_HOME outside the user home was used: %s", got)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(uh, "xdg"))
	if got, want := ConfigDir(), filepath.Join(uh, "xdg", "opencode"); got != want {
		t.Errorf("XDG_CONFIG_HOME inside the home: %s, want %s", got, want)
	}
	t.Setenv("XDG_CONFIG_HOME", "relative/dir")
	if got := ConfigDir(); got != filepath.Join(uh, ".config", "opencode") {
		t.Errorf("relative XDG_CONFIG_HOME must be ignored (xdg-basedir): %s", got)
	}
	if got := PluginPath(); got != filepath.Join(uh, ".config", "opencode", "plugins", "pilothouse.js") {
		t.Errorf("plugin path %s", got)
	}
}

func TestProtectedPathsCoverPluginsAndConfig(t *testing.T) {
	uh := isolate(t)
	opts := policy.DefaultOptions(Adapter{}.ProtectedPaths()...)
	opts.UserFile = filepath.Join(t.TempDir(), "none.yaml")
	e, _ := policy.Load(opts)
	for _, p := range []string{
		filepath.Join(uh, ".config", "opencode", "plugins", "pilothouse.js"),
		filepath.Join(uh, ".config", "opencode", "plugins", "evil.js"),
		filepath.Join(uh, ".config", "opencode", "opencode.json"),
		"/work/repo/.opencode/plugins/x.ts",
		"/work/repo/opencode.json",
		filepath.Join(uh, ".opencode", "plugins", "x.js"),
	} {
		r := e.Evaluate(policy.Input{Agent: api.AgentOpenCode, Tool: "write", ToolKind: api.ToolEdit, Paths: []string{p},
			Cwd: "/work/repo", Checkpoint: api.CheckpointPreTool})
		if r.Rule != policy.SelfProtectionRule || r.Action != api.BehaviorAsk {
			t.Errorf("%s: %s %s, want self-protection ask", p, r.Action, r.Rule)
		}
	}
	r := e.Evaluate(policy.Input{Agent: api.AgentOpenCode, Tool: "write", ToolKind: api.ToolEdit,
		Paths: []string{"/work/repo/src/main.go"}, Cwd: "/work/repo", Checkpoint: api.CheckpointPreTool})
	if r.Rule == policy.SelfProtectionRule {
		t.Errorf("ordinary file protected: %+v", r)
	}
}

func TestResumeArgs(t *testing.T) {
	if got := strings.Join(Adapter{}.ResumeArgs("ses_1"), " "); got != "opencode --session ses_1" {
		t.Errorf("resume %q", got)
	}
	if (Adapter{}).ResumeArgs("") != nil {
		t.Error("resume without id")
	}
}
