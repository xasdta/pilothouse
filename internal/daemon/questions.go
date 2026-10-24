package daemon

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/hookkit"
	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/store"
)

// Questions and plans (docs/PLAN-0.2.md F1). An agent's question tool
// (Claude AskUserQuestion) or plan approval (ExitPlanMode) reaches the
// permission checkpoint like any tool call and is routed the same way —
// held for devices while the person is away, handed to the agent's own
// dialog at the desk — but it opens a request with checkpoint question
// or plan, which is answered rather than approved.

// maxAnswerBytes bounds one answer (devices are held to the same limit).
const maxAnswerBytes = 2000

// asksEvent reports whether a hook is a tool that asks a person
// something (adapters fill Questions or Plan for those).
func asksEvent(ev adapters.HookEvent) bool { return len(ev.Questions) > 0 || ev.Plan != nil }

// asksRequest reports whether a request is answered rather than approved.
func asksRequest(r api.Request) bool {
	return r.Checkpoint == api.CheckpointQuestion || r.Checkpoint == api.CheckpointPlan
}

// shapeAsk turns a permission request for a question or plan tool into
// a question or plan request.
func shapeAsk(req *api.Request, ev adapters.HookEvent) {
	switch {
	case len(ev.Questions) > 0:
		req.Checkpoint = api.CheckpointQuestion
		req.Questions = append([]api.Question(nil), ev.Questions...)
		req.Summary = firstNonEmpty(ev.Summary, hookkit.QuestionSummary(ev.Questions))
	case ev.Plan != nil:
		plan := *ev.Plan
		req.Checkpoint = api.CheckpointPlan
		req.Plan = &plan
		req.Summary = firstNonEmpty(ev.Summary, hookkit.PlanSummary(plan.Markdown))
	}
}

// supersede closes the session's older pending requests for the same
// question or plan tool: an agent shows one such dialog at a time, so a
// new one means the person answered the last in the terminal. (Rejecting
// a plan in Claude's own dialog fires no post_tool hook; without this the
// first draft would keep the session "blocked" while Claude revises it.)
func (h *hookCall) supersede(ctx context.Context, tx *store.Tx, req api.Request) error {
	old, err := tx.Requests(ctx, store.RequestFilter{SessionID: req.SessionID, Statuses: store.PendingStatuses, Checkpoint: req.Checkpoint})
	if err != nil {
		return err
	}
	for _, r := range old {
		if r.Tool == req.Tool && r.ID != req.ID {
			if err := h.answerLocally(ctx, tx, r.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// askOutcomeMessage is the reason an agent hears when a person declines a
// question or a plan without saying why.
func askOutcomeMessage(req api.Request, dec *api.Decision) string {
	if dec.Behavior != api.BehaviorDeny || strings.TrimSpace(dec.Message) != "" {
		return decisionMessage(dec)
	}
	if req.Checkpoint == api.CheckpointPlan {
		return "The person did not approve this plan in Pilothouse (" + dec.By + ") and gave no feedback. Ask them what to change."
	}
	return "The person declined to answer in Pilothouse (" + dec.By + ")."
}

// validateDecision checks a decision against what its request asks, and
// normalizes the answers (trimmed; option labels spelled as offered;
// multi-select labels joined with ", "). Its errors are core.Invalid and
// written for the person who sent the answer.
func validateDecision(req api.Request, dec *api.Decision) error {
	switch req.Checkpoint {
	case api.CheckpointQuestion:
		if dec.Scope == api.ScopeSession {
			return core.Invalid("a question is answered once: it can't be allowed for the whole session")
		}
		if dec.Behavior == api.BehaviorDeny {
			if len(dec.Answers) > 0 {
				return core.Invalid("answers go with allow; to decline the question, deny it without answers")
			}
			return nil
		}
		answers, err := checkAnswers(req.Questions, dec.Answers)
		if err != nil {
			return err
		}
		dec.Answers = answers
	case api.CheckpointPlan:
		if dec.Scope == api.ScopeSession {
			return core.Invalid("a plan is approved once: it can't be approved for the whole session")
		}
		if len(dec.Answers) > 0 {
			return core.Invalid("a plan takes no answers: allow approves it, deny with a message asks for changes")
		}
	default:
		if len(dec.Answers) > 0 {
			return core.Invalid("answers only go with a question, and this request asks for permission: allow or deny it")
		}
	}
	return nil
}

// checkAnswers wants exactly one non-empty answer per question.
func checkAnswers(qs []api.Question, answers map[string]string) (map[string]string, error) {
	if len(qs) == 0 {
		return nil, core.Invalid("this request has no questions to answer; allow or deny it")
	}
	known := make(map[string]bool, len(qs))
	for _, q := range qs {
		known[q.Key()] = true
	}
	for k := range answers {
		if !known[k] {
			return nil, core.Invalid("%s is not one of this request's questions", quote(k))
		}
	}
	out := make(map[string]string, len(qs))
	for _, q := range qs {
		v, ok := answers[q.Key()]
		if !ok || strings.TrimSpace(v) == "" {
			return nil, core.Invalid("%s needs an answer: %s", quote(q.Text), choices(q))
		}
		a, err := checkAnswer(q, v)
		if err != nil {
			return nil, err
		}
		out[q.Key()] = a
	}
	return out, nil
}

// checkAnswer accepts an option label (any case), for multi-select a
// comma-separated list of labels, and, when the question allows it, the
// person's own words.
func checkAnswer(q api.Question, v string) (string, error) {
	v = strings.TrimSpace(v)
	if len(v) > maxAnswerBytes || !utf8.ValidString(v) {
		return "", core.Invalid("the answer to %s is too long (at most %d bytes)", quote(q.Text), maxAnswerBytes)
	}
	if l, ok := matchLabel(q, v); ok {
		return l, nil
	}
	if q.MultiSelect {
		var labels []string
		seen := map[string]bool{}
		all := true
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			l, ok := matchLabel(q, part)
			if !ok {
				all = false
				break
			}
			if !seen[l] {
				seen[l] = true
				labels = append(labels, l)
			}
		}
		if all && len(labels) > 0 {
			return api.JoinAnswers(labels), nil
		}
	}
	if q.AllowOther || len(q.Options) == 0 {
		return v, nil
	}
	return "", core.Invalid("%s has no option %s: %s", quote(q.Text), quote(v), choices(q))
}

// matchLabel finds the option v names: exactly, else ignoring case when
// that is unambiguous.
func matchLabel(q api.Question, v string) (string, bool) {
	match := ""
	for _, o := range q.Options {
		if o.Label == v {
			return o.Label, true
		}
		if strings.EqualFold(o.Label, v) {
			if match != "" {
				return "", false
			}
			match = o.Label
		}
	}
	return match, match != ""
}

// choices words what a question accepts: "choose Postgres, MySQL or
// SQLite (or write your own answer)".
func choices(q api.Question) string {
	if len(q.Options) == 0 {
		return "write your answer"
	}
	labels := make([]string, len(q.Options))
	for i, o := range q.Options {
		labels[i] = o.Label
	}
	s := "choose " + orList(labels)
	if q.MultiSelect {
		s = "choose one or more of " + orList(labels) + ", separated by commas"
	}
	if q.AllowOther {
		s += " (or write your own answer)"
	}
	return s
}

func orList(ss []string) string {
	if len(ss) <= 1 {
		return strings.Join(ss, "")
	}
	return strings.Join(ss[:len(ss)-1], ", ") + " or " + ss[len(ss)-1]
}

func quote(s string) string { return "“" + hookkit.OneLine(s, 80) + "”" }
