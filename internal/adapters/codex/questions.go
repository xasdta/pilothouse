package codex

import (
	"encoding/json"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/hookkit"
	"github.com/xasdta/pilothouse/internal/api"
)

// toolRequestUserInput is Codex's question tool
// (codex-rs/core/src/tools/handlers/request_user_input.rs). Its PreToolUse
// hook fires with the model's arguments ({questions: [{id, header,
// question, options: [{label, description}]}]}, the schema in
// request_user_input_spec.rs), but no PermissionRequest follows and a
// hook can only deny it: the answer travels through Codex's own UI (or
// its app-server), never a hook. So Pilothouse reads the questions to
// show the session as blocked on them, and the person answers in the
// terminal.
const toolRequestUserInput = "request_user_input"

type userInputArgs struct {
	Questions []struct {
		ID       string `json:"id"`
		Header   string `json:"header"`
		Question string `json:"question"`
		IsOther  bool   `json:"isOther"`
		Options  []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options"`
	} `json:"questions"`
}

// describeUserInput fills a request_user_input hook. Questions are set at
// pre_tool only (read-only: they tell the state resolver the session now
// waits on a person, and keep allow/ask rules from deciding the call).
func describeUserInput(ev *adapters.HookEvent) {
	ev.ToolKind = api.ToolOther
	var args userInputArgs
	if len(ev.Input) == 0 || json.Unmarshal(ev.Input, &args) != nil {
		ev.Summary = "Asks a question"
		return
	}
	qs := make([]api.Question, 0, len(args.Questions))
	for _, q := range args.Questions {
		aq := api.Question{ID: q.ID, Header: q.Header, Text: q.Question,
			// Codex offers a free-text "None of the above" answer when
			// isOther is set or a question has no options.
			AllowOther: q.IsOther || len(q.Options) == 0}
		for _, o := range q.Options {
			aq.Options = append(aq.Options, api.QuestionOption{Label: o.Label, Description: o.Description})
		}
		qs = append(qs, hookkit.TidyQuestion(aq))
	}
	ev.Summary = hookkit.QuestionSummary(qs)
	if ev.Kind == adapters.HookPreTool && hookkit.CheckQuestions(qs) {
		ev.Questions = qs
	}
}
