package claude

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/hookkit"
	"github.com/xasdta/pilothouse/internal/api"
)

// Claude Code's two tools that ask a person something (docs/AGENTS.md,
// "Questions and plans"). Both reach the PermissionRequest hook, which
// answers them:
//
//   - AskUserQuestion {questions: [{question, header, options: [{label,
//     description}], multiSelect}]}: allow with decision.updatedInput =
//     the input plus answers {<question text>: <label>}. A multi-select
//     answer is the labels joined with ", "; any other text is taken as
//     the person's own ("Other") answer. Claude refuses an answer whose
//     questions differ from what it asked, so the input is echoed as is.
//   - ExitPlanMode {plan, planFilePath} (Claude adds both before the
//     hooks run): allow with updatedInput = the input approves the plan;
//     deny with a message asks for changes and Claude revises the plan.
//
// An allow at PreToolUse does not answer either tool, so Pilothouse never
// sends one for them (see EncodeResponse).
const (
	toolAskUserQuestion = "AskUserQuestion"
	toolExitPlanMode    = "ExitPlanMode"
)

// asksPerson reports whether tool is answered by a person, not approved.
func asksPerson(tool string) bool { return tool == toolAskUserQuestion || tool == toolExitPlanMode }

type questionInput struct {
	Questions []struct {
		Question string `json:"question"`
		Header   string `json:"header"`
		Options  []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options"`
		MultiSelect bool `json:"multiSelect"`
	} `json:"questions"`
}

// parseQuestions reads AskUserQuestion's input. ok is false when it does
// not parse or cannot be asked through Pilothouse (hookkit.CheckQuestions);
// the request then stays a plain permission request answered in Claude's
// own dialog.
func parseQuestions(in json.RawMessage) ([]api.Question, bool) {
	var qi questionInput
	if len(in) == 0 || json.Unmarshal(in, &qi) != nil {
		return nil, false
	}
	qs := make([]api.Question, 0, len(qi.Questions))
	for _, q := range qi.Questions {
		aq := api.Question{Header: q.Header, Text: q.Question, MultiSelect: q.MultiSelect,
			AllowOther: true} // Claude always offers "Other" with free text
		for _, o := range q.Options {
			aq.Options = append(aq.Options, api.QuestionOption{Label: o.Label, Description: o.Description})
		}
		qs = append(qs, hookkit.TidyQuestion(aq))
	}
	if !hookkit.CheckQuestions(qs) {
		return nil, false
	}
	return qs, true
}

type planInput struct {
	Plan         *string `json:"plan"`
	PlanFilePath string  `json:"planFilePath"`
}

// parsePlan reads ExitPlanMode's input; ok is false without a plan.
func parsePlan(in json.RawMessage) (api.Plan, bool) {
	var pi planInput
	if len(in) == 0 || json.Unmarshal(in, &pi) != nil || pi.Plan == nil {
		return api.Plan{}, false
	}
	return api.Plan{Markdown: hookkit.CapBytes(*pi.Plan, hookkit.MaxPlanBytes), Path: pi.PlanFilePath}, true
}

// planFromResponse reads the plan from a PostToolUse payload: there
// ExitPlanMode's tool_input is empty and the plan is in tool_response.
func planFromResponse(payload json.RawMessage) string {
	var p struct {
		ToolResponse struct {
			Plan string `json:"plan"`
		} `json:"tool_response"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return ""
	}
	return p.ToolResponse.Plan
}

// describeAsk fills the question or plan of an AskUserQuestion or
// ExitPlanMode hook. Questions and Plan are set where a person can still
// answer (pre_tool, permission); every kind gets the same summary, so the
// post_tool hook matches the request.
func describeAsk(ev *adapters.HookEvent, payload json.RawMessage) {
	ev.ToolKind = api.ToolOther
	asking := ev.Kind == adapters.HookPreTool || ev.Kind == adapters.HookPermission
	switch ev.Tool {
	case toolAskUserQuestion:
		qs, ok := parseQuestions(ev.Input)
		if !ok {
			ev.Summary = "Use " + ev.Tool
			return
		}
		if asking {
			ev.Questions = qs
		}
		ev.Summary = hookkit.QuestionSummary(qs)
	case toolExitPlanMode:
		plan, ok := parsePlan(ev.Input)
		md := plan.Markdown
		if !ok && ev.Kind == adapters.HookPostTool {
			md = planFromResponse(payload)
		}
		if ok && asking {
			ev.Plan = &plan
		}
		ev.Summary = hookkit.PlanSummary(md)
	}
}

// Default messages when a person declines without saying why.
const (
	declinedQuestion = "The person declined to answer these questions through Pilothouse."
	declinedPlan     = "The person did not approve this plan through Pilothouse and gave no feedback. Ask them what to change."
)

// answerAsk answers AskUserQuestion or ExitPlanMode at the permission
// checkpoint. Anything it cannot express faithfully gets no opinion, so
// Claude's own dialog shows.
func answerAsk(ev adapters.HookEvent, out adapters.HookOutcome) adapters.HookResponse {
	if out.Behavior == api.BehaviorDeny {
		def := declinedQuestion
		if ev.Tool == toolExitPlanMode {
			def = declinedPlan
		}
		return hookkit.PermissionResponse(api.BehaviorDeny, firstNonEmpty(out.Message, def))
	}
	if out.Behavior != api.BehaviorAllow {
		return hookkit.NoOpinion
	}
	if ev.Tool == toolExitPlanMode {
		if isObject(ev.Input) {
			return hookkit.PermissionAllowInput(ev.Input)
		}
		return hookkit.PermissionResponse(api.BehaviorAllow, "")
	}
	in, ok := withAnswers(ev.Input, out.Answers)
	if !ok {
		return hookkit.NoOpinion
	}
	return hookkit.PermissionAllowInput(in)
}

// withAnswers is AskUserQuestion's input with answers added. Every other
// field keeps its exact JSON. ok is false unless answers has a non-empty
// answer for every question and nothing else.
func withAnswers(in json.RawMessage, answers map[string]string) (json.RawMessage, bool) {
	qs, ok := parseQuestions(in)
	if !ok || len(answers) != len(qs) {
		return nil, false
	}
	for _, q := range qs {
		if strings.TrimSpace(answers[q.Text]) == "" {
			return nil, false
		}
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(in, &m) != nil {
		return nil, false
	}
	a, err := marshalText(answers)
	if err != nil {
		return nil, false
	}
	m["answers"] = a
	out, err := marshalText(m)
	if err != nil {
		return nil, false
	}
	return out, true
}

// marshalText is json.Marshal without HTML escaping, so the answer reads
// in the event log exactly as the person typed it.
func marshalText(v any) (json.RawMessage, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

func isObject(b json.RawMessage) bool {
	b = bytes.TrimSpace(b)
	return len(b) > 0 && b[0] == '{' && json.Valid(b)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
