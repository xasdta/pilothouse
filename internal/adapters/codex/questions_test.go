package codex

import (
	"os"
	"strings"
	"testing"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/api"
)

// TestRequestUserInput: Codex's question tool is read-only for
// Pilothouse (no PermissionRequest fires, hooks can't answer it): the
// questions are parsed so the session shows what it is blocked on, a deny
// rule still blocks the call, and an ask rule must not turn into a deny
// that cancels the question.
func TestRequestUserInput(t *testing.T) {
	a := Adapter{}
	payload, err := os.ReadFile("testdata/hooks/pre_tool_request_user_input.json")
	if err != nil {
		t.Fatal(err)
	}
	ev, err := a.ParseHook(adapters.HookEnvelope{Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.Questions) != 1 || ev.Questions[0].Key() != "db" || len(ev.Questions[0].Options) != 2 ||
		ev.Summary != "Asks: Which database should the migration target?" || ev.ToolKind != api.ToolOther {
		t.Fatalf("ev = %+v", ev)
	}
	if got := a.EncodeResponse(ev, adapters.HookOutcome{Behavior: api.BehaviorAsk, Message: "rule"}); got.ExitCode != 0 || got.Stderr != "" {
		t.Errorf("ask must not deny a question: %+v", got)
	}
	if got := a.EncodeResponse(ev, adapters.HookOutcome{Behavior: api.BehaviorDeny, Message: "No questions in CI."}); got.ExitCode != 2 || !strings.Contains(got.Stderr, "No questions in CI") {
		t.Errorf("deny: %+v", got)
	}
	// Other tools keep the ask-as-deny fallback.
	bash := adapters.HookEvent{Kind: adapters.HookPreTool, Tool: "Bash"}
	if got := a.EncodeResponse(bash, adapters.HookOutcome{Behavior: api.BehaviorAsk}); got.ExitCode != 2 {
		t.Errorf("bash ask: %+v", got)
	}
	// Garbled input still parses as an event, with nothing to show.
	bad := []byte(`{"hook_event_name":"PreToolUse","tool_name":"request_user_input","tool_input":"oops"}`)
	if ev, err := a.ParseHook(adapters.HookEnvelope{Payload: bad}); err != nil || ev.Questions != nil || ev.Summary != "Asks a question" {
		t.Errorf("garbled: %+v %v", ev, err)
	}
	// A post_tool hook keeps the summary but asks nothing any more.
	post := []byte(strings.Replace(string(payload), `"PreToolUse"`, `"PostToolUse"`, 1))
	if ev, err := a.ParseHook(adapters.HookEnvelope{Payload: post}); err != nil || ev.Questions != nil || !strings.HasPrefix(ev.Summary, "Asks: ") {
		t.Errorf("post_tool: %+v %v", ev, err)
	}
}
