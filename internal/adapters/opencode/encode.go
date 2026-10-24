package opencode

import (
	"encoding/json"
	"strings"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/hookkit"
	"github.com/xasdta/pilothouse/internal/api"
)

// answer is what the plugin reads from HookResponse.Stdout.
type answer struct {
	// Action: "deny" (throw / reject the permission), "ask" (policy wants
	// a person: the plugin holds the call through pilothouse.ask),
	// "allow" (reply "once" to a permission), "answer" (reply to a
	// question). Empty: no opinion, OpenCode carries on by itself.
	Action  string     `json:"action,omitempty"`
	Message string     `json:"message,omitempty"`
	Rule    string     `json:"rule,omitempty"`
	Summary string     `json:"summary,omitempty"` // ask: what is waiting, for OpenCode's toast
	Answers [][]string `json:"answers,omitempty"` // in question order
}

// EncodeResponse answers the plugin:
//
//   - pre_tool: deny → {"action":"deny"} (the plugin throws: OpenCode
//     reports the message to the model as the tool's error). ask →
//     {"action":"ask"}: OpenCode has no prompt a plugin can raise, so the
//     plugin holds the call and asks again as pilothouse.ask, which the
//     daemon holds for a person at the desk too (HookEvent.Hold). Allow is
//     no opinion: OpenCode's own permission settings still apply.
//   - permission (permission.asked, pilothouse.ask): allow → "allow"
//     (reply "once"); deny → "deny" with the message (reject with
//     feedback, which the model reads).
//   - question: allow with answers → "answer" with the labels per
//     question in order; deny → "deny" (reject the question).
//   - everything else: nothing.
func (Adapter) EncodeResponse(ev adapters.HookEvent, out adapters.HookOutcome) adapters.HookResponse {
	var a answer
	switch ev.Kind {
	case adapters.HookPreTool:
		switch out.Behavior {
		case api.BehaviorDeny:
			a = answer{Action: "deny", Message: firstNonEmpty(strings.TrimSpace(out.Message), hookkit.DefaultDenyReason), Rule: out.Rule}
		case api.BehaviorAsk:
			a = answer{Action: "ask", Message: strings.TrimSpace(out.Message), Rule: out.Rule, Summary: ev.Summary}
		}
	case adapters.HookPermission:
		switch {
		case out.Behavior == api.BehaviorAllow && len(ev.Questions) > 0:
			a = answer{Action: "answer", Answers: questionAnswers(ev.Questions, out.Answers)}
		case out.Behavior == api.BehaviorAllow:
			a = answer{Action: "allow"}
		case out.Behavior == api.BehaviorDeny:
			a = answer{Action: "deny", Message: firstNonEmpty(strings.TrimSpace(out.Message), "Denied through Pilothouse.")}
		}
	}
	return respond(a)
}

// FailResponse: no opinion, except a deny before a tool call when
// approvals.fail_closed is on. (The plugin applies the same rule itself
// when it cannot reach the daemon at all.)
func (Adapter) FailResponse(kind adapters.HookKind, failClosed bool) adapters.HookResponse {
	if failClosed && kind == adapters.HookPreTool {
		return respond(answer{Action: "deny", Message: hookkit.FailClosedReason})
	}
	return hookkit.NoOpinion
}

func respond(a answer) adapters.HookResponse {
	if a.Action == "" {
		return hookkit.NoOpinion
	}
	b, _ := json.Marshal(a)
	return adapters.HookResponse{Stdout: string(b)}
}

// questionAnswers turns Decision.Answers (Question.Key() → label, or labels
// joined with ", " for multi-select, or free text) into OpenCode's
// question reply: one list of labels per question, in order.
func questionAnswers(qs []api.Question, answers map[string]string) [][]string {
	out := make([][]string, len(qs))
	for i, q := range qs {
		v := strings.TrimSpace(answers[q.Key()])
		switch {
		case v == "":
			out[i] = []string{}
		case q.MultiSelect:
			out[i] = splitLabels(q, v)
		default:
			out[i] = []string{v}
		}
	}
	return out
}

// splitLabels splits a multi-select answer back into the option labels it
// joined. Labels may themselves contain ", ", so the split follows the
// labels rather than the separator; an answer that is not a list of labels
// is free text and is sent as one answer.
func splitLabels(q api.Question, v string) []string {
	labels := make([]string, 0, len(q.Options))
	for _, o := range q.Options {
		labels = append(labels, o.Label)
	}
	var walk func(rest string) []string
	walk = func(rest string) []string {
		for _, l := range labels {
			if l == "" || !strings.HasPrefix(rest, l) {
				continue
			}
			tail := rest[len(l):]
			if tail == "" {
				return []string{l}
			}
			if t, ok := strings.CutPrefix(tail, api.MultiAnswerSep); ok {
				if more := walk(t); more != nil {
					return append([]string{l}, more...)
				}
			}
		}
		return nil
	}
	if got := walk(v); got != nil {
		return got
	}
	return []string{v}
}
