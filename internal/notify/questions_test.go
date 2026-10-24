package notify

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
)

func questionRequest(qs ...api.Question) *api.Request {
	return &api.Request{ID: "r_q1", Agent: api.AgentClaude, Checkpoint: api.CheckpointQuestion, Tool: "AskUserQuestion",
		Summary: "Asks: " + qs[0].Text, Questions: qs, Cwd: "/src/api", Status: api.RequestOpen, ExpiresAt: time.Now().Add(5 * time.Minute)}
}

func opts(labels ...string) []api.QuestionOption {
	var o []api.QuestionOption
	for _, l := range labels {
		o = append(o, api.QuestionOption{Label: l})
	}
	return o
}

// TestQuestionAndPlanWording: notifications say what is asked and list
// the options, so the person can decide before opening the app.
func TestQuestionAndPlanWording(t *testing.T) {
	db := api.Question{Text: "Which database?", Options: opts("Postgres", "MySQL", "SQLite"), AllowOther: true}
	s := &api.Session{Agent: api.AgentClaude, Title: "api", Branch: "main"}
	for _, tc := range []struct {
		r    *api.Request
		line string
	}{
		{questionRequest(db), "Claude Code asks: Which database? (Postgres, MySQL or SQLite) · api (main)"},
		{questionRequest(api.Question{Text: "Which checks?", MultiSelect: true, Options: opts("Lint", "Tests")}),
			"Claude Code asks: Which checks? (any of Lint or Tests) · api (main)"},
		{questionRequest(api.Question{Text: "Pick one", Options: opts("a", "b", "c", "d", "e")}, db, db),
			"Claude Code asks: Pick one (a, b, c, d …) +2 more questions · api (main)"},
		{questionRequest(api.Question{Text: "Anything else?", AllowOther: true}, db),
			"Claude Code asks: Anything else? +1 more question · api (main)"},
		{&api.Request{Agent: api.AgentClaude, Checkpoint: api.CheckpointPlan, Summary: "Plan ready: Add OAuth login", Plan: &api.Plan{Markdown: "# Add OAuth login"}},
			"Claude Code has a plan ready: Add OAuth login · api (main)"},
		{&api.Request{Agent: api.AgentClaude, Checkpoint: api.CheckpointPlan, Summary: "Plan ready", Plan: &api.Plan{}},
			"Claude Code has a plan ready for you to approve · api (main)"},
	} {
		if got := Line(core.Notification{Kind: core.NotifyRequest, Request: tc.r, Session: s}); got != tc.line {
			t.Errorf("Line = %q\n  want %q", got, tc.line)
		}
	}
	n := Render(core.Notification{Kind: core.NotifyBlocked, Request: questionRequest(db), Session: s})
	if n.Title != "Claude Code · api (main)" || n.Body != "Asks: Which database? (Postgres, MySQL or SQLite) — waiting in the terminal" {
		t.Errorf("blocked = %q / %q", n.Title, n.Body)
	}
}

// TestNtfyButtonsOnlyForPermissions: a bare Allow can't answer a question
// (and a plan should be read first), so those notifications only open the
// request in the app.
func TestNtfyButtonsOnlyForPermissions(t *testing.T) {
	isolate(t)
	ts, got := capture(t, 200)
	c := &Ntfy{Server: ts.URL, Topic: "t", Pusher: &fakePusher{public: "https://mac.ts.net"}, Client: ts.Client()}
	plan := &api.Request{ID: "r_p1", Agent: api.AgentClaude, Checkpoint: api.CheckpointPlan, Summary: "Plan ready: X", Status: api.RequestOpen}
	for _, r := range []*api.Request{questionRequest(api.Question{Text: "Which?", Options: opts("a", "b")}), plan} {
		if err := c.Send(context.Background(), Render(core.Notification{Kind: core.NotifyRequest, Request: r, Remote: true})); err != nil {
			t.Fatal(err)
		}
	}
	for i, id := range []string{"r_q1", "r_p1"} {
		h := got()[i].header
		if a := h.Get("Actions"); a != "view, Open, https://mac.ts.net/r/"+id || strings.Contains(a, "/api/actions/") {
			t.Errorf("%s: Actions = %q", id, a)
		}
	}
}
