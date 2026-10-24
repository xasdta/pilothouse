package hookkit

import (
	"strings"
	"testing"

	"github.com/xasdta/pilothouse/internal/api"
)

func TestPlanTitleAndSummary(t *testing.T) {
	for md, want := range map[string]string{
		"# Add OAuth login\n\n1. Register the app":                 "Add OAuth login",
		"# Plan\n\n1. Reply with the single word: DONE\n":          "Reply with the single word: DONE",
		"# Plan: Migrate to **Postgres 17**\n- dump\n- restore":    "Migrate to Postgres 17",
		"## Implementation plan\n## Phase 1: schema\n":             "Phase 1: schema",
		"Here is what I'll do:\n\n# Fix the flaky test":            "Fix the flaky test",
		"---\n> - Rename `x` to `y`\n":                             "Rename x to y",
		"```go\ncode\n```\n# Real title":                           "Real title",
		"":                                                         "",
		"   \n\n":                                                  "",
		"# " + strings.Repeat("long ", 60):                         strings.TrimSpace(Truncate(strings.Repeat("long ", 60), 120)),
		"First line\nsecond line":                                  "First line",
		"# Overview\nCaches the token for an hour.\n# Steps\n1. a": "Steps",
	} {
		if got := PlanTitle(md); got != want {
			t.Errorf("PlanTitle(%q) = %q, want %q", md, got, want)
		}
	}
	if s := PlanSummary("# Add OAuth login"); s != "Plan ready: Add OAuth login" {
		t.Errorf("PlanSummary = %q", s)
	}
	if s := PlanSummary(""); s != "Plan ready" {
		t.Errorf("empty PlanSummary = %q", s)
	}
}

func TestQuestionSummary(t *testing.T) {
	q := func(text string) api.Question {
		return api.Question{Text: text, Options: []api.QuestionOption{{Label: "a"}, {Label: "b"}}, AllowOther: true}
	}
	if s := QuestionSummary([]api.Question{q("Which database?")}); s != "Asks: Which database?" {
		t.Errorf("one: %q", s)
	}
	if s := QuestionSummary([]api.Question{q("Which database?\nMore detail"), q("Which region?"), q("Size?")}); s != "Asks: Which database? … (+2 more)" {
		t.Errorf("three: %q", s)
	}
	if s := QuestionSummary(nil); s != "Asks a question" {
		t.Errorf("none: %q", s)
	}
}

func TestCheckQuestions(t *testing.T) {
	ok := api.Question{Text: "Which?", Options: []api.QuestionOption{{Label: "a"}, {Label: "b"}}}
	free := api.Question{ID: "q1", Text: "Anything else?", AllowOther: true}
	if !CheckQuestions([]api.Question{ok, free}) {
		t.Fatal("valid questions rejected")
	}
	many := make([]api.Question, MaxQuestions+1)
	for i := range many {
		many[i] = ok
		many[i].Text = strings.Repeat("q", i+1)
	}
	opts := make([]api.QuestionOption, MaxOptions+1)
	for i := range opts {
		opts[i].Label = strings.Repeat("o", i+1)
	}
	for name, qs := range map[string][]api.Question{
		"none":          nil,
		"too many":      many,
		"same key":      {ok, ok},
		"blank text":    {{Text: " ", Options: ok.Options}},
		"long key":      {{Text: strings.Repeat("k", MaxQuestionBytes+1), Options: ok.Options}},
		"bad utf8":      {{Text: "\xff", Options: ok.Options}},
		"blank label":   {{Text: "Which?", Options: []api.QuestionOption{{Label: ""}, {Label: "b"}}}},
		"dup label":     {{Text: "Which?", Options: []api.QuestionOption{{Label: "b"}, {Label: "b"}}}},
		"many options":  {{Text: "Which?", Options: opts}},
		"unanswerable":  {{Text: "Which?"}},
		"same id twice": {{ID: "x", Text: "A?", AllowOther: true}, {ID: "x", Text: "B?", AllowOther: true}},
	} {
		if CheckQuestions(qs) {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTidyQuestionKeepsKeys(t *testing.T) {
	q := api.Question{Text: "  Which?  ", Header: strings.Repeat("h", 100),
		Options: []api.QuestionOption{{Label: " a ", Description: strings.Repeat("d", 1000)}}}
	got := TidyQuestion(q)
	if got.Text != q.Text || got.Options[0].Label != " a " {
		t.Errorf("text and labels must stay exact: %+v", got)
	}
	if len([]rune(got.Header)) > 40 || len(got.Options[0].Description) > 500 {
		t.Errorf("display fields not capped: %d / %d", len(got.Header), len(got.Options[0].Description))
	}
	if len(q.Options[0].Description) != 1000 {
		t.Error("TidyQuestion must not modify its argument")
	}
}
