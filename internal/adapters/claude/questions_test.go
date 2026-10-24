package claude

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/api"
)

// The fixtures *_ask_user_question and *_exit_plan_mode* were captured
// from Claude Code 2.1.309 in an e2e run that answered them through the
// hook (tests/e2e/claude/s7_questions.py checks the same answers against
// the real CLI).

func TestParseAskUserQuestion(t *testing.T) {
	want := []api.Question{{Header: "Color", Text: "Which color do you prefer?", AllowOther: true,
		Options: []api.QuestionOption{{Label: "Red", Description: "Choose red"}, {Label: "Blue", Description: "Choose blue"}}}}
	for _, name := range []string{"pre_tool_ask_user_question", "permission_ask_user_question"} {
		ev := parseFixture(t, name)
		if !reflect.DeepEqual(ev.Questions, want) {
			t.Errorf("%s: questions = %+v", name, ev.Questions)
		}
		if ev.Summary != "Asks: Which color do you prefer?" || ev.ToolKind != api.ToolOther || ev.Plan != nil {
			t.Errorf("%s: %+v", name, ev)
		}
	}
	// PostToolUse (input now carries the answers) keeps the summary, so it
	// finds the request the toolUseID-less PermissionRequest opened; there
	// is nothing left to answer.
	post := parseFixture(t, "post_tool_ask_user_question")
	if post.Summary != "Asks: Which color do you prefer?" || post.Questions != nil {
		t.Errorf("post_tool: %+v", post)
	}
}

func TestParseExitPlanMode(t *testing.T) {
	ev := parseFixture(t, "permission_exit_plan_mode")
	if ev.Plan == nil || ev.Plan.Markdown != "# Plan\n\n1. Reply with the single word: DONE\n" ||
		ev.Plan.Path != "/home/user/.claude/plans/this-is-a-test-effervescent-moonbeam.md" {
		t.Fatalf("plan = %+v", ev.Plan)
	}
	if ev.Summary != "Plan ready: Reply with the single word: DONE" || ev.Questions != nil {
		t.Errorf("summary = %q", ev.Summary)
	}
	if pre := parseFixture(t, "pre_tool_exit_plan_mode"); pre.Plan == nil || pre.Summary != ev.Summary {
		t.Errorf("pre_tool: %+v", pre)
	}
	// The revised plan's PostToolUse has an empty tool_input; the plan
	// comes from tool_response, and the summary matches its request.
	rev := parseFixture(t, "permission_exit_plan_mode_revised")
	post := parseFixture(t, "post_tool_exit_plan_mode")
	if post.Plan != nil || post.Summary != rev.Summary || post.Summary == "Plan ready" {
		t.Errorf("post_tool summary %q, revised request %q", post.Summary, rev.Summary)
	}
}

func TestParseQuestionsRejectsWhatCantBeAsked(t *testing.T) {
	q := func(text string, labels ...string) map[string]any {
		var opts []map[string]any
		for _, l := range labels {
			opts = append(opts, map[string]any{"label": l, "description": ""})
		}
		return map[string]any{"question": text, "header": "H", "options": opts, "multiSelect": false}
	}
	many := []any{}
	for i := 0; i < 9; i++ {
		many = append(many, q(strings.Repeat("x", i+1), "a", "b"))
	}
	for name, in := range map[string]any{
		"no questions":     map[string]any{"questions": []any{}},
		"duplicate text":   map[string]any{"questions": []any{q("Same?", "a", "b"), q("Same?", "c", "d")}},
		"empty text":       map[string]any{"questions": []any{q(" ", "a", "b")}},
		"duplicate labels": map[string]any{"questions": []any{q("Which?", "a", "a")}},
		"too many":         map[string]any{"questions": many},
		"huge text":        map[string]any{"questions": []any{q(strings.Repeat("y", 2001), "a", "b")}},
		"not an object":    "questions",
	} {
		b, _ := json.Marshal(in)
		if qs, ok := parseQuestions(b); ok {
			t.Errorf("%s: parsed %+v", name, qs)
		}
		payload, _ := json.Marshal(map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "AskUserQuestion", "tool_input": in})
		ev, err := Adapter{}.ParseHook(adapters.HookEnvelope{Payload: payload})
		if err != nil || ev.Questions != nil || ev.Summary != "Use AskUserQuestion" {
			t.Errorf("%s: ev = %+v, %v", name, ev, err)
		}
	}
}

func TestEncodeQuestionAnswers(t *testing.T) {
	a := Adapter{}
	ev := parseFixture(t, "permission_ask_user_question")
	got := a.EncodeResponse(ev, adapters.HookOutcome{Behavior: api.BehaviorAllow,
		Answers: map[string]string{"Which color do you prefer?": "Blue"}})
	var out struct {
		HookSpecificOutput struct {
			HookEventName string `json:"hookEventName"`
			Decision      struct {
				Behavior     string          `json:"behavior"`
				UpdatedInput json.RawMessage `json:"updatedInput"`
			} `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(got.Stdout), &out); err != nil || got.ExitCode != 0 {
		t.Fatalf("%+v: %v", got, err)
	}
	d := out.HookSpecificOutput.Decision
	if out.HookSpecificOutput.HookEventName != "PermissionRequest" || d.Behavior != "allow" {
		t.Fatalf("decision = %+v", out)
	}
	// updatedInput is the input unchanged plus answers, which is exactly
	// what Claude accepted in the e2e run.
	var in, upd map[string]any
	_ = json.Unmarshal(ev.Input, &in)
	_ = json.Unmarshal(d.UpdatedInput, &upd)
	in["answers"] = map[string]any{"Which color do you prefer?": "Blue"}
	if !reflect.DeepEqual(in, upd) {
		t.Errorf("updatedInput = %s", d.UpdatedInput)
	}

	// Free text and multi-select (labels joined with ", ") pass through.
	multi := askEvent(t, `{"questions":[{"question":"Which <checks>?","header":"Checks","options":[{"label":"Lint","description":""},{"label":"Tests","description":""}],"multiSelect":true},{"question":"Anything else?","header":"Notes","options":[{"label":"No","description":""},{"label":"Yes","description":""}],"multiSelect":false}],"metadata":{"source":"x"}}`)
	got = a.EncodeResponse(multi, adapters.HookOutcome{Behavior: api.BehaviorAllow,
		Answers: map[string]string{"Which <checks>?": "Lint, Tests", "Anything else?": "ship it on Friday"}})
	if !strings.Contains(got.Stdout, `"answers":{"Anything else?":"ship it on Friday","Which <checks>?":"Lint, Tests"}`) ||
		!strings.Contains(got.Stdout, `"metadata":{"source":"x"}`) {
		t.Errorf("multi: %s", got.Stdout)
	}

	// Anything that would not answer every question exactly gets no
	// opinion: Claude's own dialog shows instead.
	for name, ans := range map[string]map[string]string{
		"none":    nil,
		"missing": {"Which <checks>?": "Lint"},
		"empty":   {"Which <checks>?": "Lint", "Anything else?": " "},
		"unknown": {"Which <checks>?": "Lint", "Anything else?": "No", "Other?": "x"},
		"renamed": {"Which checks?": "Lint", "Anything else?": "No"},
	} {
		if got := a.EncodeResponse(multi, adapters.HookOutcome{Behavior: api.BehaviorAllow, Answers: ans}); got != (adapters.HookResponse{}) {
			t.Errorf("%s: %+v", name, got)
		}
	}

	// Decline: deny with the person's reason, or a default one.
	got = a.EncodeResponse(ev, adapters.HookOutcome{Behavior: api.BehaviorDeny, Message: "I'll pick later"})
	if strings.TrimSpace(got.Stdout) != `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"I'll pick later"}}}` {
		t.Errorf("deny: %s", got.Stdout)
	}
	got = a.EncodeResponse(ev, adapters.HookOutcome{Behavior: api.BehaviorDeny})
	if !strings.Contains(got.Stdout, declinedQuestion) {
		t.Errorf("deny default: %s", got.Stdout)
	}
	if got := a.EncodeResponse(ev, adapters.HookOutcome{}); got != (adapters.HookResponse{}) {
		t.Errorf("no decision: %+v", got)
	}
}

func TestEncodePlanDecisions(t *testing.T) {
	a := Adapter{}
	ev := parseFixture(t, "permission_exit_plan_mode")
	got := a.EncodeResponse(ev, adapters.HookOutcome{Behavior: api.BehaviorAllow, Message: "Allowed in Pilothouse (cli)."})
	want := `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow","updatedInput":{"plan":"# Plan\n\n1. Reply with the single word: DONE\n","planFilePath":"/home/user/.claude/plans/this-is-a-test-effervescent-moonbeam.md"}}}}`
	if strings.TrimSpace(got.Stdout) != want {
		t.Errorf("approve:\n%s\nwant\n%s", got.Stdout, want)
	}
	got = a.EncodeResponse(ev, adapters.HookOutcome{Behavior: api.BehaviorDeny, Message: "Add a step that says KIWI."})
	if strings.TrimSpace(got.Stdout) != `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"Add a step that says KIWI."}}}` {
		t.Errorf("revise: %s", got.Stdout)
	}
	if got := a.EncodeResponse(ev, adapters.HookOutcome{Behavior: api.BehaviorDeny}); !strings.Contains(got.Stdout, declinedPlan) {
		t.Errorf("revise default: %s", got.Stdout)
	}
	// A plan whose input Pilothouse couldn't read is still approved
	// (plain allow) rather than echoing garbage.
	odd := adapters.HookEvent{Kind: adapters.HookPermission, Tool: "ExitPlanMode", Input: json.RawMessage(`"x"`)}
	if got := a.EncodeResponse(odd, adapters.HookOutcome{Behavior: api.BehaviorAllow}); strings.TrimSpace(got.Stdout) != `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}` {
		t.Errorf("odd: %s", got.Stdout)
	}
}

// TestPreToolNeverAllowsQuestionsOrPlans: an allow at PreToolUse does
// not answer AskUserQuestion (and would approve a plan unseen), so rules
// and grants can only deny these tools.
func TestPreToolNeverAllowsQuestionsOrPlans(t *testing.T) {
	a := Adapter{}
	for _, name := range []string{"pre_tool_ask_user_question", "pre_tool_exit_plan_mode"} {
		ev := parseFixture(t, name)
		if got := a.EncodeResponse(ev, adapters.HookOutcome{Behavior: api.BehaviorAllow, Message: "rule"}); got != (adapters.HookResponse{}) {
			t.Errorf("%s allow: %+v", name, got)
		}
		got := a.EncodeResponse(ev, adapters.HookOutcome{Behavior: api.BehaviorDeny, Message: "No questions in CI."})
		if !strings.Contains(got.Stdout, `"permissionDecision":"deny"`) || !strings.Contains(got.Stdout, "No questions in CI.") {
			t.Errorf("%s deny: %+v", name, got)
		}
		got = a.EncodeResponse(ev, adapters.HookOutcome{Behavior: api.BehaviorAllow, Context: "ctx"})
		if strings.Contains(got.Stdout, "permissionDecision") || !strings.Contains(got.Stdout, "ctx") {
			t.Errorf("%s context kept: %+v", name, got)
		}
	}
}

func askEvent(t *testing.T, input string) adapters.HookEvent {
	t.Helper()
	payload := `{"session_id":"s","cwd":"/w","hook_event_name":"PermissionRequest","tool_name":"AskUserQuestion","tool_input":` + input + `}`
	ev, err := Adapter{}.ParseHook(adapters.HookEnvelope{Payload: json.RawMessage(payload)})
	if err != nil || len(ev.Questions) == 0 {
		t.Fatalf("parse: %+v %v", ev, err)
	}
	return ev
}
