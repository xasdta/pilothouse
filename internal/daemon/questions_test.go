package daemon

import (
	"strings"
	"testing"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/client"
	"github.com/xasdta/pilothouse/internal/core"
)

var (
	dbQuestion = api.Question{Header: "Database", Text: "Which database?", AllowOther: true,
		Options: []api.QuestionOption{{Label: "Postgres", Description: "what prod runs"}, {Label: "SQLite"}}}
	checksQuestion = api.Question{Header: "Checks", Text: "Which checks?", MultiSelect: true,
		Options: []api.QuestionOption{{Label: "Lint"}, {Label: "Unit tests"}, {Label: "E2E"}}}
)

func askHook(kind adapters.HookKind, qs ...api.Question) adapters.HookEvent {
	return adapters.HookEvent{Kind: kind, Tool: "AskUserQuestion", ToolKind: api.ToolOther,
		Summary: "Asks: " + qs[0].Text, Questions: qs}
}

func planHook(kind adapters.HookKind, md string) adapters.HookEvent {
	return adapters.HookEvent{Kind: kind, Tool: "ExitPlanMode", ToolKind: api.ToolOther,
		Summary: "Plan ready: Add OAuth", Plan: &api.Plan{Markdown: md, Path: "/w/.claude/plans/p.md"}}
}

func awayHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.presence.setAway(true)
	h.start(t)
	waitFor(t, "away", func() bool { return h.d.Presence().State == api.PresenceAway })
	return h
}

// TestQuestionHeldAndAnswered: away, a question is held like a
// permission, pushed with what it asks, and the answer reaches the hook
// in the request's own terms.
func TestQuestionHeldAndAnswered(t *testing.T) {
	h := awayHarness(t)
	res := make(chan fakeOutcome, 1)
	go func() { res <- h.hook(bg, t, askHook(adapters.HookPermission, dbQuestion, checksQuestion)) }()
	req := h.openRequest(t)
	if req.Checkpoint != api.CheckpointQuestion || req.Route != api.RouteDevices || len(req.Questions) != 2 ||
		req.Summary != "Asks: Which database?" {
		t.Fatalf("request = %+v", req)
	}
	waitFor(t, "notification", func() bool { return h.notes.find(core.NotifyRequest, true) })
	if n := h.notes.all()[len(h.notes.all())-1]; n.Request == nil || n.Request.Checkpoint != api.CheckpointQuestion {
		t.Fatalf("notification = %+v", n)
	}
	s, err := h.c.Session(bg, req.SessionID)
	if err != nil || s.State != api.StateBlocked || s.BlockKind != api.BlockQuestion || s.StateDetail != "Asks: Which database?" {
		t.Fatalf("session = %+v, %v", s, err)
	}

	// Invalid answers are refused with a sentence the person can act on,
	// and leave the request open.
	for name, tc := range map[string]struct {
		body api.DecideBody
		want string
	}{
		"no answers":     {api.DecideBody{Behavior: api.BehaviorAllow}, "“Which database?” needs an answer: choose Postgres or SQLite (or write your own answer)"},
		"one missing":    {api.DecideBody{Behavior: api.BehaviorAllow, Answers: map[string]string{"Which database?": "Postgres"}}, "“Which checks?” needs an answer: choose one or more of Lint, Unit tests or E2E, separated by commas"},
		"not an option":  {api.DecideBody{Behavior: api.BehaviorAllow, Answers: map[string]string{"Which database?": "Postgres", "Which checks?": "Lint, Fuzz"}}, "“Which checks?” has no option “Lint, Fuzz”"},
		"unknown key":    {api.DecideBody{Behavior: api.BehaviorAllow, Answers: map[string]string{"Which database?": "SQLite", "Which checks?": "Lint", "Why?": "x"}}, "“Why?” is not one of this request's questions"},
		"session scope":  {api.DecideBody{Behavior: api.BehaviorAllow, Scope: api.ScopeSession}, "can't be allowed for the whole session"},
		"deny + answers": {api.DecideBody{Behavior: api.BehaviorDeny, Answers: map[string]string{"Which database?": "Postgres"}}, "answers go with allow"},
	} {
		_, err := h.c.Decide(bg, req.ID, tc.body)
		if !client.IsCode(err, "invalid") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want invalid %q", name, err, tc.want)
		}
	}
	if r, _ := h.c.Request(bg, req.ID); r.Status != api.RequestOpen {
		t.Fatalf("refused answers closed the request: %+v", r)
	}

	// Labels in any case, multi-select as a comma list (deduplicated,
	// spelled as offered); the free-text "Other" answer is kept as typed.
	got, err := h.c.Decide(bg, req.ID, api.DecideBody{Behavior: api.BehaviorAllow,
		Answers: map[string]string{"Which database?": " postgres ", "Which checks?": "lint,unit TESTS, Lint"}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"Which database?": "Postgres", "Which checks?": "Lint, Unit tests"}
	if got.Decision == nil || !sameAnswers(got.Decision.Answers, want) {
		t.Fatalf("stored decision = %+v", got.Decision)
	}
	out := <-res
	if out.Behavior != api.BehaviorAllow || !sameAnswers(out.Answers, want) {
		t.Fatalf("hook = %+v", out)
	}
	// The first answer wins, even when a later one is malformed.
	if _, err := h.c.Decide(bg, req.ID, api.DecideBody{Behavior: api.BehaviorAllow}); !client.IsCode(err, "already_decided") {
		t.Fatalf("second answer: %v", err)
	}
}

func TestQuestionFreeTextDeclineAndStrictOptions(t *testing.T) {
	h := awayHarness(t)
	strict := api.Question{ID: "env", Text: "Deploy where?", Options: []api.QuestionOption{{Label: "staging"}, {Label: "prod"}}}
	free := api.Question{ID: "note", Text: "Anything else?", AllowOther: true} // no options: free text only
	res := make(chan fakeOutcome, 1)
	go func() { res <- h.hook(bg, t, askHook(adapters.HookPermission, strict, free)) }()
	req := h.openRequest(t)
	_, err := h.c.Decide(bg, req.ID, api.DecideBody{Behavior: api.BehaviorAllow, Answers: map[string]string{"env": "qa", "note": "no"}})
	if !client.IsCode(err, "invalid") || !strings.Contains(err.Error(), "“Deploy where?” has no option “qa”: choose staging or prod") ||
		strings.Contains(err.Error(), "your own answer") {
		t.Fatalf("strict option: %v", err)
	}
	if _, err := h.c.Decide(bg, req.ID, api.DecideBody{Behavior: api.BehaviorAllow,
		Answers: map[string]string{"env": "Prod", "note": "Ship it Friday, not before."}}); err != nil {
		t.Fatal(err)
	}
	if out := <-res; out.Answers["env"] != "prod" || out.Answers["note"] != "Ship it Friday, not before." {
		t.Fatalf("hook = %+v", out)
	}

	// Declining (deny) without a reason tells the agent who declined.
	go func() { res <- h.hook(bg, t, askHook(adapters.HookPermission, dbQuestion)) }()
	req = h.openRequest(t)
	if _, err := h.c.Decide(bg, req.ID, api.DecideBody{Behavior: api.BehaviorDeny}); err != nil {
		t.Fatal(err)
	}
	if out := <-res; out.Behavior != api.BehaviorDeny || out.Message != "The person declined to answer in Pilothouse (cli)." || out.Answers != nil {
		t.Fatalf("decline = %+v", out)
	}
}

// TestPlanApproveAndRevise: allow approves; deny carries the feedback;
// neither takes answers or a session scope.
func TestPlanApproveAndRevise(t *testing.T) {
	h := awayHarness(t)
	res := make(chan fakeOutcome, 1)
	go func() { res <- h.hook(bg, t, planHook(adapters.HookPermission, "# Add OAuth\n1. Register the app")) }()
	req := h.openRequest(t)
	if req.Checkpoint != api.CheckpointPlan || req.Plan == nil || req.Plan.Path != "/w/.claude/plans/p.md" ||
		req.Summary != "Plan ready: Add OAuth" || req.Route != api.RouteDevices {
		t.Fatalf("request = %+v", req)
	}
	if s, _ := h.c.Session(bg, req.SessionID); s.BlockKind != api.BlockQuestion || s.StateDetail != "Plan ready: Add OAuth" {
		t.Fatalf("session = %+v", s)
	}
	for _, body := range []api.DecideBody{
		{Behavior: api.BehaviorAllow, Scope: api.ScopeSession},
		{Behavior: api.BehaviorAllow, Answers: map[string]string{"x": "y"}},
	} {
		if _, err := h.c.Decide(bg, req.ID, body); !client.IsCode(err, "invalid") {
			t.Errorf("%+v: %v", body, err)
		}
	}
	if _, err := h.c.Decide(bg, req.ID, api.DecideBody{Behavior: api.BehaviorDeny, Message: "Use PKCE."}); err != nil {
		t.Fatal(err)
	}
	if out := <-res; out.Behavior != api.BehaviorDeny || out.Message != "Use PKCE." {
		t.Fatalf("revise = %+v", out)
	}

	go func() {
		res <- h.hook(bg, t, planHook(adapters.HookPermission, "# Add OAuth\n1. Register the app with PKCE"))
	}()
	req = h.openRequest(t)
	if _, err := h.c.Decide(bg, req.ID, api.DecideBody{Behavior: api.BehaviorAllow}); err != nil {
		t.Fatal(err)
	}
	if out := <-res; out.Behavior != api.BehaviorAllow {
		t.Fatalf("approve = %+v", out)
	}

	go func() { res <- h.hook(bg, t, planHook(adapters.HookPermission, "# Add OAuth")) }()
	req = h.openRequest(t)
	if _, err := h.c.Decide(bg, req.ID, api.DecideBody{Behavior: api.BehaviorDeny}); err != nil {
		t.Fatal(err)
	}
	if out := <-res; !strings.Contains(out.Message, "did not approve this plan") || !strings.Contains(out.Message, "Ask them what to change") {
		t.Fatalf("revise without feedback = %+v", out)
	}
}

// TestPermissionRefusesAnswers: answers only go with questions.
func TestPermissionRefusesAnswers(t *testing.T) {
	h := awayHarness(t)
	res := make(chan fakeOutcome, 1)
	go func() { res <- h.hook(bg, t, permission("git push", "")) }()
	req := h.openRequest(t)
	_, err := h.c.Decide(bg, req.ID, api.DecideBody{Behavior: api.BehaviorAllow, Answers: map[string]string{"q": "a"}})
	if !client.IsCode(err, "invalid") || !strings.Contains(err.Error(), "asks for permission") {
		t.Fatalf("answers on a permission: %v", err)
	}
	if _, err := h.c.Decide(bg, req.ID, api.DecideBody{Behavior: api.BehaviorAllow}); err != nil {
		t.Fatal(err)
	}
	<-res
}

// TestQuestionAtDesk: at the desk the question shows in the agent's own
// dialog; the post_tool hook (no tool_use_id on the permission side)
// closes it by tool and summary.
func TestQuestionAtDesk(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	if out := h.hook(bg, t, askHook(adapters.HookPermission, dbQuestion)); out.Behavior != "" {
		t.Fatalf("at the desk the hook must step aside: %+v", out)
	}
	rs, _ := h.c.Requests(bg, api.RequestSteppedAside)
	if len(rs) != 1 || rs[0].Checkpoint != api.CheckpointQuestion || rs[0].Route != api.RouteTerminal {
		t.Fatalf("requests = %+v", rs)
	}
	post := askHook(adapters.HookPostTool, dbQuestion)
	post.Questions, post.ToolUseID = nil, "toolu_1"
	h.hook(bg, t, post)
	if r, _ := h.c.Request(bg, rs[0].ID); r.Status != api.RequestAnsweredLocally {
		t.Fatalf("after post_tool: %+v", r)
	}
}

// TestRevisedPlanSupersedesDraft: rejecting a plan in Claude's own dialog
// fires nothing; the next draft closes the first as answered locally.
func TestRevisedPlanSupersedesDraft(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	h.hook(bg, t, planHook(adapters.HookPermission, "# Draft 1"))
	h.hook(bg, t, shell("ls")) // Claude revises the plan file meanwhile
	h.hook(bg, t, planHook(adapters.HookPermission, "# Draft 2"))
	pending, _ := h.c.Requests(bg, api.RequestSteppedAside)
	if len(pending) != 1 || pending[0].Plan.Markdown != "# Draft 2" {
		t.Fatalf("pending = %+v", pending)
	}
	closed, _ := h.c.Requests(bg, api.RequestAnsweredLocally)
	if len(closed) != 1 || closed[0].Plan.Markdown != "# Draft 1" {
		t.Fatalf("closed = %+v", closed)
	}
	// A permission request of another kind is not touched.
	h.hook(bg, t, permission("git push", ""))
	h.hook(bg, t, planHook(adapters.HookPermission, "# Draft 3"))
	pending, _ = h.c.Requests(bg, api.RequestSteppedAside)
	if len(pending) != 2 {
		t.Fatalf("pending = %+v", pending)
	}
}

// TestPreToolLeavesQuestionsToThePerson: rules and grants can deny a
// question or plan tool before it runs, but never allow it (an allow
// would not answer it, or would approve a plan unseen) or "ask".
func TestPreToolLeavesQuestionsToThePerson(t *testing.T) {
	h := awayHarness(t)
	h.policy.set("", api.BehaviorAllow, "allow-all")

	for _, ev := range []adapters.HookEvent{askHook(adapters.HookPreTool, dbQuestion), planHook(adapters.HookPreTool, "# P")} {
		if out := h.hook(bg, t, ev); out.Behavior != "" {
			t.Errorf("%s with an allow rule: %+v", ev.Tool, out)
		}
	}
	h.policy.set("", api.BehaviorAsk, "ask-all")
	if out := h.hook(bg, t, askHook(adapters.HookPreTool, dbQuestion)); out.Behavior != "" {
		t.Errorf("ask rule: %+v", out)
	}
	h.policy.set("", api.BehaviorDeny, "no-questions")
	if out := h.hook(bg, t, askHook(adapters.HookPreTool, dbQuestion)); out.Behavior != api.BehaviorDeny {
		t.Errorf("deny rule: %+v", out)
	}
	h.policy.set("", "", "")

	// A grant for the plan tool (0.1 treated ExitPlanMode as a permission
	// and could allow it for the session) answers neither checkpoint.
	res := make(chan fakeOutcome, 1)
	old := adapters.HookEvent{Kind: adapters.HookPermission, Tool: "ExitPlanMode", ToolKind: api.ToolOther, Summary: "Use ExitPlanMode"}
	go func() { res <- h.hook(bg, t, old) }()
	req := h.openRequest(t)
	if req.Checkpoint != api.CheckpointPermission {
		t.Fatalf("plan without a plan stays a permission: %+v", req)
	}
	if _, err := h.c.Decide(bg, req.ID, api.DecideBody{Behavior: api.BehaviorAllow, Scope: api.ScopeSession}); err != nil {
		t.Fatal(err)
	}
	<-res
	if out := h.hook(bg, t, planHook(adapters.HookPreTool, "# P")); out.Behavior != "" {
		t.Fatalf("grant at pre_tool: %+v", out)
	}
	go func() { res <- h.hook(bg, t, planHook(adapters.HookPermission, "# P")) }()
	req = h.openRequest(t)
	if req.Checkpoint != api.CheckpointPlan || req.Status != api.RequestOpen {
		t.Fatalf("grant answered a plan: %+v", req)
	}
	if _, err := h.c.Decide(bg, req.ID, api.DecideBody{Behavior: api.BehaviorDeny, Message: "no"}); err != nil {
		t.Fatal(err)
	}
	<-res
}

func sameAnswers(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
