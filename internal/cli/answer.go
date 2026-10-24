package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/client"
)

// Questions and plans from the CLI (docs/PLAN-0.2.md F1): `pilothouse
// answer` answers a held question; `approve` / `deny -m` approve a plan or
// ask for changes. The daemon validates every answer (the same rules as
// for the phone); the CLI only maps option numbers to labels and words
// what happened.

func newAnswerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "answer <request-or-session-id> [answer]...",
		Short: "Answer a question an agent is waiting on",
		Long: `Answer a held question (Claude Code's AskUserQuestion). Give one answer per
question, in the order they were asked: an option's label or number, for a
multi-select question a comma-separated list ("1,3" or "Lint,Tests"), or your
own words when the question accepts them. Quote answers that contain spaces.
With no answers, it prints the questions and their options.

To decline instead: pilothouse deny <id> -m "why".`,
		Example: `  pilothouse answer r_7hq                      # show the questions
  pilothouse answer r_7hq Postgres
  pilothouse answer r_7hq 2 "Lint, Unit tests"
  pilothouse answer s_k3m "Use the staging database"`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAnswer(cmd, args[0], args[1:])
		},
	}
}

func runAnswer(cmd *cobra.Command, ref string, given []string) error {
	c := inboxClient()
	ctx := cmd.Context()
	req, err := resolveRequestRef(ctx, c, ref)
	if err != nil {
		return err
	}
	switch req.Checkpoint {
	case api.CheckpointQuestion:
	case api.CheckpointPlan:
		return fmt.Errorf("%s is a plan, not a question — approve it with `pilothouse approve %s`, or ask for changes with `pilothouse deny %s -m \"what to change\"`",
			req.ID, inboxShortRef(req.ID), inboxShortRef(req.ID))
	default:
		return fmt.Errorf("%s asks for permission, not an answer — use `pilothouse approve %s` or `pilothouse deny %s`",
			req.ID, inboxShortRef(req.ID), inboxShortRef(req.ID))
	}
	if err := requireOpen(ctx, c, req); err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	n := len(req.Questions)
	if len(given) == 0 {
		printQuestions(w, req)
		return fmt.Errorf("give one answer per question (%d)", n)
	}
	if len(given) != n {
		printQuestions(w, req)
		return fmt.Errorf("%s asks %d question%s and you gave %d answer%s — give one per question, in order, and quote answers that contain spaces",
			req.ID, n, plural(n), len(given), plural(len(given)))
	}
	answers := make(map[string]string, n)
	for i, q := range req.Questions {
		answers[q.Key()] = cliAnswer(q, given[i])
	}
	out, err := c.Decide(ctx, req.ID, api.DecideBody{Behavior: api.BehaviorAllow, Answers: answers})
	if err != nil {
		var ae *client.APIError
		if errors.As(err, &ae) && ae.Code == "invalid" {
			return fmt.Errorf("%s — see the options with `pilothouse answer %s`", ae.Msg, inboxShortRef(req.ID))
		}
		return inboxErr(err, ref)
	}
	if out.Decision != nil && len(out.Decision.Answers) > 0 {
		answers = out.Decision.Answers // as the daemon stored them
	}
	fmt.Fprintf(w, "✓ Answered %s:\n", req.ID)
	for _, q := range req.Questions {
		fmt.Fprintf(w, "  %s → %s\n", oneLineText(q.Text), answers[q.Key()])
	}
	return nil
}

// cliAnswer maps option numbers to labels ("2" → the second option; for
// a multi-select question "1,3" → "Lint, E2E"). Anything else is passed
// on as typed: the daemon matches labels and accepts free text.
func cliAnswer(q api.Question, s string) string {
	s = strings.TrimSpace(s)
	if l, ok := optionByNumber(q, s); ok {
		return l
	}
	if !q.MultiSelect || !strings.Contains(s, ",") {
		return s
	}
	parts := strings.Split(s, ",")
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if l, ok := optionByNumber(q, p); ok {
			p = l
		}
		parts[i] = p
	}
	return api.JoinAnswers(parts)
}

// optionByNumber reads s as a 1-based option number, unless an option is
// literally labelled s.
func optionByNumber(q api.Question, s string) (string, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > len(q.Options) {
		return "", false
	}
	for _, o := range q.Options {
		if o.Label == s {
			return "", false
		}
	}
	return q.Options[n-1].Label, true
}

// printQuestions lists a question request's questions with numbered
// options, the way `pilothouse answer` reads them.
func printQuestions(w io.Writer, r api.Request) {
	fmt.Fprintf(w, "%s asks:\n", r.ID)
	for i, q := range r.Questions {
		head := fmt.Sprintf("  %d. %s", i+1, oneLineText(q.Text))
		if q.Header != "" {
			head += "  [" + q.Header + "]"
		}
		if q.MultiSelect {
			head += "  (choose any: comma-separated)"
		}
		fmt.Fprintln(w, head)
		for j, o := range q.Options {
			line := fmt.Sprintf("       %d) %s", j+1, o.Label)
			if o.Description != "" {
				line += " — " + inboxClip(oneLineText(o.Description), 80)
			}
			fmt.Fprintln(w, line)
		}
		if q.AllowOther {
			if len(q.Options) > 0 {
				fmt.Fprintln(w, "       or your own words")
			} else {
				fmt.Fprintln(w, "       (your own words)")
			}
		}
	}
	args := make([]string, len(r.Questions))
	for i := range args {
		args[i] = fmt.Sprintf("<answer %d>", i+1)
	}
	if len(args) == 1 {
		args[0] = "<answer>"
	}
	fmt.Fprintf(w, "Answer: pilothouse answer %s %s\n", inboxShortRef(r.ID), strings.Join(args, " "))
}

// requireOpen refuses a request the daemon no longer holds: one shown in
// the agent's own dialog (an answer here would not reach it) or closed.
func requireOpen(ctx context.Context, c *client.Client, req api.Request) error {
	switch req.Status {
	case api.RequestOpen:
		return nil
	case api.RequestSteppedAside:
		hint := "answer it in the terminal where the agent runs"
		if s, err := c.Session(ctx, req.SessionID); err == nil && s.Mode == api.ModeWrapped {
			hint = "attach with `pilothouse attach " + inboxShortRef(s.ID) + "` and answer it there"
		}
		what := "permission dialog"
		switch req.Checkpoint {
		case api.CheckpointQuestion:
			what = "question dialog"
		case api.CheckpointPlan:
			what = "plan dialog"
		}
		return fmt.Errorf("%s is showing in the agent's own %s — %s", req.ID, what, hint)
	}
	by := ""
	if req.Decision != nil {
		by = " by " + req.Decision.By
	}
	return fmt.Errorf("%s was already closed (%s%s) — the first answer wins", req.ID, strings.ReplaceAll(string(req.Status), "_", " "), by)
}

// askDecideCheck refuses approve/deny forms that don't fit a question or
// plan, before anything is sent.
func askDecideCheck(req api.Request, body api.DecideBody) error {
	ref := inboxShortRef(req.ID)
	switch req.Checkpoint {
	case api.CheckpointQuestion:
		if body.Behavior == api.BehaviorAllow {
			return fmt.Errorf("%s is a question — answer it with `pilothouse answer %s <answer>` (run `pilothouse answer %s` to see the options), or decline with `pilothouse deny %s`",
				req.ID, ref, ref, ref)
		}
	case api.CheckpointPlan:
		if body.Scope == api.ScopeSession {
			return fmt.Errorf("a plan is approved once — drop --session: `pilothouse approve %s`", ref)
		}
	}
	return nil
}

// askDecidedLine words a decision on a question or plan; ok is false for
// permission requests.
func askDecidedLine(w io.Writer, req api.Request, body api.DecideBody) bool {
	switch req.Checkpoint {
	case api.CheckpointQuestion:
		fmt.Fprintf(w, "✗ Declined to answer %s: %s\n", req.ID, inboxRequestWhat(req))
	case api.CheckpointPlan:
		title := strings.TrimPrefix(strings.TrimPrefix(req.Summary, "Plan ready"), ": ")
		switch {
		case body.Behavior == api.BehaviorAllow:
			fmt.Fprintf(w, "✓ Approved plan %s: %s\n", req.ID, title)
		case body.Message != "":
			fmt.Fprintf(w, "✎ Asked for changes to plan %s: %s\n  The agent was told: %s\n", req.ID, title, body.Message)
		default:
			fmt.Fprintf(w, "✗ Rejected plan %s: %s\n  Next time add -m \"what to change\": the agent now has to ask you.\n", req.ID, title)
		}
	default:
		return false
	}
	return true
}

// askWhat is the WHAT column of a question or plan request.
func askWhat(r api.Request) (string, bool) {
	switch r.Checkpoint {
	case api.CheckpointQuestion:
		if len(r.Questions) == 0 {
			return oneLineText(r.Summary), true
		}
		q := r.Questions[0]
		s := "? " + oneLineText(q.Text)
		if len(q.Options) > 0 {
			labels := make([]string, len(q.Options))
			for i, o := range q.Options {
				labels[i] = o.Label
			}
			s += " [" + strings.Join(labels, " | ") + "]"
		}
		if len(r.Questions) > 1 {
			s += fmt.Sprintf(" +%d", len(r.Questions)-1)
		}
		return s, true
	case api.CheckpointPlan:
		return "Plan: " + strings.TrimPrefix(strings.TrimPrefix(r.Summary, "Plan ready"), ": "), true
	}
	return "", false
}

// askHint is the ANSWER column of a held question or plan request.
func askHint(r api.Request) (string, bool) {
	ref := inboxShortRef(r.ID)
	switch r.Checkpoint {
	case api.CheckpointQuestion:
		return "pilothouse answer " + ref + " …", true
	case api.CheckpointPlan:
		return "pilothouse approve|deny -m " + ref, true
	}
	return "", false
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
