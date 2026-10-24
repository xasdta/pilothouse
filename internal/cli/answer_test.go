package cli

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

// ibxAsks adds a held question, a held plan and a stepped-aside question
// to the fake daemon's snapshot.
func ibxAsks(d *ibxDaemon) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.snap.Requests = append(d.snap.Requests,
		api.Request{ID: "r_q1question", SessionID: "s_q9r7s5t3uv", Agent: api.AgentClaude, Checkpoint: api.CheckpointQuestion,
			Tool: "AskUserQuestion", Summary: "Asks: Which database? (+1 more)", Cwd: "/home/dev/src/site", Status: api.RequestOpen,
			Route: api.RouteDevices, CreatedAt: ibxT0.Add(-30 * time.Second), Questions: []api.Question{
				{Header: "Database", Text: "Which database?", AllowOther: true, Options: []api.QuestionOption{
					{Label: "Postgres", Description: "what prod runs"}, {Label: "SQLite"}}},
				{Header: "Checks", Text: "Which checks?", MultiSelect: true, AllowOther: true, Options: []api.QuestionOption{
					{Label: "Lint"}, {Label: "Unit tests"}, {Label: "E2E"}}},
			}},
		api.Request{ID: "r_p1plan0001", SessionID: "s_q9r7s5t3uv", Agent: api.AgentClaude, Checkpoint: api.CheckpointPlan,
			Tool: "ExitPlanMode", Summary: "Plan ready: Add OAuth login", Cwd: "/home/dev/src/site", Status: api.RequestOpen,
			Route: api.RouteDevices, CreatedAt: ibxT0.Add(-20 * time.Second), Plan: &api.Plan{Markdown: "# Add OAuth login\n1. Register"}},
		api.Request{ID: "r_q2aside001", SessionID: "s_p4t8w6n2rb", Agent: api.AgentClaude, Checkpoint: api.CheckpointQuestion,
			Tool: "AskUserQuestion", Summary: "Asks: Ship?", Status: api.RequestSteppedAside, Route: api.RouteTerminal,
			CreatedAt: ibxT0.Add(-10 * time.Second), Questions: []api.Question{{Text: "Ship?", AllowOther: true,
				Options: []api.QuestionOption{{Label: "Yes"}, {Label: "No"}}}}},
	)
}

func TestLsShowsQuestionsAndPlans(t *testing.T) {
	d := ibxStart(t)
	ibxAsks(d)
	out, err := ibxRun(t, "", "ls")
	if err != nil {
		t.Fatal(err)
	}
	ibxContains(t, out, "NEEDS YOU (6)",
		"Asks: Which database? [Postgres | SQLite] +1", "pilothouse answer r_q1qu …",
		"Plan: Add OAuth login", "pilothouse approve|deny -m r_p1pl",
		"Asks: Ship? [Yes | No]")
}

func TestAnswerCommand(t *testing.T) {
	d := ibxStart(t)
	ibxAsks(d)

	// No answers: the questions with numbered options, and how to answer.
	out, err := ibxRun(t, "", "answer", "r_q1")
	if err == nil || !strings.Contains(err.Error(), "give one answer per question (2)") {
		t.Fatalf("err = %v", err)
	}
	ibxContains(t, out, "r_q1question asks:", "1. Which database?  [Database]", "1) Postgres — what prod runs", "2) SQLite",
		"or your own words", "2. Which checks?  [Checks]  (choose any: comma-separated)", "3) E2E",
		"Answer: pilothouse answer r_q1qu <answer 1> <answer 2>")

	// Wrong count.
	_, err = ibxRun(t, "", "answer", "r_q1", "Postgres")
	if err == nil || !strings.Contains(err.Error(), "asks 2 questions and you gave 1 answer") {
		t.Fatalf("count: %v", err)
	}

	// Numbers map to labels, comma lists for multi-select; text passes on.
	out, err = ibxRun(t, "", "answer", "r_q1", "2", "1, 3")
	if err != nil {
		t.Fatal(err)
	}
	ibxContains(t, out, "✓ Answered r_q1question:", "Which database? → SQLite", "Which checks? → Lint, E2E")
	out, err = ibxRun(t, "", "answer", "r_q1", "the staging one", "Lint,Unit tests")
	if err != nil {
		t.Fatal(err)
	}
	ibxContains(t, out, "Which database? → the staging one", "Which checks? → Lint, Unit tests")

	d.mu.Lock()
	calls := append([]ibxDecideCall(nil), d.decides...)
	d.mu.Unlock()
	want := []map[string]string{
		{"Which database?": "SQLite", "Which checks?": "Lint, E2E"},
		{"Which database?": "the staging one", "Which checks?": "Lint, Unit tests"},
	}
	if len(calls) != 2 {
		t.Fatalf("decides = %v", calls)
	}
	for i, c := range calls {
		if c.id != "r_q1question" || c.body.Behavior != api.BehaviorAllow || c.body.Scope != "" || c.header != "cli" ||
			fmt.Sprint(c.body.Answers) != fmt.Sprint(want[i]) {
			t.Errorf("decide %d = %+v, want answers %v", i, c, want[i])
		}
	}

	// The daemon's validation message reaches the person.
	d.mu.Lock()
	d.invalid = "“Which checks?” has no option “Fuzz”: choose one or more of Lint, Unit tests or E2E"
	d.mu.Unlock()
	_, err = ibxRun(t, "", "answer", "r_q1", "1", "Fuzz")
	if err == nil || !strings.Contains(err.Error(), "has no option “Fuzz”") || !strings.Contains(err.Error(), "pilothouse answer r_q1qu") {
		t.Fatalf("invalid: %v", err)
	}
	d.mu.Lock()
	d.invalid = ""
	d.mu.Unlock()

	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"plan":          {[]string{"answer", "r_p1", "yes"}, "is a plan, not a question — approve it with `pilothouse approve r_p1pl`"},
		"permission":    {[]string{"answer", "r_7hq", "yes"}, "asks for permission, not an answer"},
		"stepped aside": {[]string{"answer", "r_q2", "Yes"}, "showing in the agent's own question dialog — answer it in the terminal"},
		"unknown":       {[]string{"answer", "r_nope", "x"}, `nothing matches "r_nope"`},
	} {
		if _, err := ibxRun(t, "", tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}

	d.mu.Lock()
	d.refuse = true
	d.mu.Unlock()
	if _, err := ibxRun(t, "", "answer", "r_q1", "1", "1"); err == nil || !strings.Contains(err.Error(), "agents can't approve themselves") {
		t.Fatalf("agent caller: %v", err)
	}
}

func TestApproveDenyPlansAndQuestions(t *testing.T) {
	d := ibxStart(t)
	ibxAsks(d)
	out, err := ibxRun(t, "", "approve", "r_p1")
	if err != nil {
		t.Fatal(err)
	}
	ibxContains(t, out, "✓ Approved plan r_p1plan0001: Add OAuth login")
	out, err = ibxRun(t, "", "deny", "r_p1", "-m", "Use PKCE")
	if err != nil {
		t.Fatal(err)
	}
	ibxContains(t, out, "✎ Asked for changes to plan r_p1plan0001: Add OAuth login", "The agent was told: Use PKCE")
	out, err = ibxRun(t, "", "deny", "r_p1")
	if err != nil {
		t.Fatal(err)
	}
	ibxContains(t, out, "✗ Rejected plan r_p1plan0001", `-m "what to change"`)
	out, err = ibxRun(t, "", "deny", "r_q1", "-m", "later")
	if err != nil {
		t.Fatal(err)
	}
	ibxContains(t, out, "✗ Declined to answer r_q1question: Asks: Which database?")

	before := len(d.decides)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"approve a question": {[]string{"approve", "r_q1"}, "is a question — answer it with `pilothouse answer r_q1qu <answer>`"},
		"plan for session":   {[]string{"approve", "r_p1", "--session"}, "a plan is approved once — drop --session"},
	} {
		if _, err := ibxRun(t, "", tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	if len(d.decides) != before {
		t.Fatal("refused forms must not reach the daemon")
	}
}

func TestCLIAnswerMapping(t *testing.T) {
	q := api.Question{Text: "Which?", MultiSelect: true, Options: []api.QuestionOption{{Label: "a"}, {Label: "2"}, {Label: "c"}}}
	for in, want := range map[string]string{
		"1":        "a",
		"2":        "2", // an option literally labelled "2" wins over the number
		"3":        "c",
		"4":        "4",
		"1,3":      "a, c",
		" c , 1 ":  "c, a",
		"free":     "free",
		"0":        "0",
		"1, other": "a, other",
	} {
		if got := cliAnswer(q, in); got != want {
			t.Errorf("cliAnswer(%q) = %q, want %q", in, got, want)
		}
	}
	single := api.Question{Text: "One?", Options: []api.QuestionOption{{Label: "x"}, {Label: "y"}}}
	if got := cliAnswer(single, "1,2"); got != "1,2" {
		t.Errorf("single-select comma list = %q", got)
	}
}
