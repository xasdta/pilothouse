package demo

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
)

var ctx = context.Background()

func newDemo(t *testing.T) *Core {
	t.Helper()
	return New(Options{Quiet: true})
}

// settle runs every scheduled reaction as if their time had come.
func settle(c *Core) { c.runDue(time.Now().Add(time.Hour)) }

func TestSeedIsBelievable(t *testing.T) {
	c := newDemo(t)
	snap, err := c.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	agents := map[api.AgentKind]bool{}
	for _, s := range snap.Sessions {
		agents[s.Agent] = true
		if s.State != api.StateExited && s.Usage == nil {
			t.Errorf("%s has no usage", s.ID)
		}
	}
	for _, a := range []api.AgentKind{api.AgentClaude, api.AgentCodex, agentOpenCode} {
		if !agents[a] {
			t.Errorf("no %s session", a)
		}
	}
	kinds := map[api.Checkpoint]int{}
	for _, r := range snap.Requests {
		if r.Status != api.RequestOpen {
			continue
		}
		kinds[r.Checkpoint]++
		s, err := c.Session(ctx, r.SessionID)
		if err != nil || s.State != api.StateBlocked {
			t.Errorf("%s: session %s not blocked (%v)", r.ID, r.SessionID, s.State)
		}
		if !r.ExpiresAt.After(time.Now()) {
			t.Errorf("%s already expired", r.ID)
		}
	}
	if kinds[api.CheckpointPermission] < 2 || kinds[api.CheckpointQuestion] != 1 || kinds[api.CheckpointPlan] != 1 {
		t.Errorf("open requests by kind = %v", kinds)
	}
	tf, _ := c.Request(ctx, "r_tfedit04")
	if tf.Preview == nil || len(tf.Preview.Files) == 0 || tf.Preview.Files[0].Diff == "" || len(tf.Risk) == 0 || tf.PolicyRule == "" || tf.Context == "" {
		t.Errorf("edit request lacks preview, risk, rule or context: %+v", tf)
	}
	if len(snap.Tasks) < 3 {
		t.Errorf("tasks = %d", len(snap.Tasks))
	}
	if snap.Presence.State != api.PresenceAway {
		t.Errorf("presence = %v, want away (requests go to devices)", snap.Presence.State)
	}
	// The event log is in time order, as the daemon's is.
	sub, _ := c.Subscribe(ctx, 0)
	defer sub.Close()
	var last time.Time
	for i := 0; i < int(c.Seq()); i++ {
		ev := <-sub.C
		if ev.At.Before(last.Add(-time.Second)) {
			t.Fatalf("event %d (%s) at %v is before %v", ev.Seq, ev.Type, ev.At, last)
		}
		last = ev.At
	}
}

func TestDiffHunksAddUp(t *testing.T) {
	c := newDemo(t)
	var previews []api.FilePreview
	for _, r := range c.Requests() {
		if r.Preview != nil {
			previews = append(previews, r.Preview.Files...)
		}
	}
	for _, p := range []*api.Preview{benchPreview(), contrastPreview(), terraformPreview()} {
		previews = append(previews, p.Files...)
	}
	for _, d := range []api.TaskDiff{faqDiff(), flakyDiff("t")} {
		previews = append(previews, d.Files...)
	}
	previews = append(previews, taskFiles("api-server", "x")...)
	previews = append(previews, taskFiles("site", "x")...)
	for _, f := range previews {
		add, rem := 0, 0
		var oldN, newN, wantOld, wantNew int
		for _, l := range strings.Split(strings.TrimSuffix(f.Diff, "\n"), "\n") {
			switch {
			case strings.HasPrefix(l, "@@"):
				if wantOld != oldN || wantNew != newN {
					t.Errorf("%s: hunk counts %d/%d, lines %d/%d", f.Path, wantOld, wantNew, oldN, newN)
				}
				var a, b, c2, d int
				if _, err := sscanHunk(l, &a, &b, &c2, &d); err != nil {
					t.Fatalf("%s: bad hunk %q", f.Path, l)
				}
				wantOld, wantNew, oldN, newN = b, d, 0, 0
			case strings.HasPrefix(l, "+"):
				add++
				newN++
			case strings.HasPrefix(l, "-"):
				rem++
				oldN++
			default:
				oldN++
				newN++
			}
		}
		if wantOld != oldN || wantNew != newN {
			t.Errorf("%s: last hunk counts %d/%d, lines %d/%d", f.Path, wantOld, wantNew, oldN, newN)
		}
		if add != f.Added || rem != f.Removed {
			t.Errorf("%s: +%d −%d in the diff, +%d −%d declared", f.Path, add, rem, f.Added, f.Removed)
		}
	}
}

func sscanHunk(l string, a, b, c, d *int) (int, error) {
	// "@@ -a,b +c,d @@ …"
	f := strings.Fields(l)
	if len(f) < 3 {
		return 0, errors.New("short")
	}
	parse := func(s string, x, y *int) error {
		parts := strings.SplitN(s[1:], ",", 2)
		*x, *y = atoi(parts[0]), 1
		if len(parts) == 2 {
			*y = atoi(parts[1])
		}
		if *x < 0 || *y < 0 {
			return errors.New("bad")
		}
		return nil
	}
	if err := parse(f[1], a, b); err != nil {
		return 0, err
	}
	return 4, parse(f[2], c, d)
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return -1
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func TestQuestionAnswersAreValidated(t *testing.T) {
	c := newDemo(t)
	bad := []map[string]string{
		nil,                                   // nothing answered
		{"q1": "SQLite FTS5"},                 // q2 missing
		{"q1": "SQLite FTS5", "q2": "Colour"}, // not an option, no free text
		{"q1": "SQLite FTS5", "q2": "Title", "q9": "x"},
		{"q1": "", "q2": "Title"},
	}
	for _, a := range bad {
		_, err := c.Decide(ctx, api.Decision{RequestID: "r_question05", Behavior: api.BehaviorAllow, Answers: a, By: "device:x"})
		if !errors.Is(err, core.ErrInvalid) {
			t.Errorf("answers %v: err = %v, want invalid", a, err)
		}
	}
	if _, err := c.Decide(ctx, api.Decision{RequestID: "r_gitpush03", Behavior: api.BehaviorAllow, Answers: map[string]string{"x": "y"}, By: "device:x"}); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("answers on a permission: %v", err)
	}
	// q1 allows free text; q2 is multi-select over its labels.
	r, err := c.Decide(ctx, api.Decision{RequestID: "r_question05", Behavior: api.BehaviorAllow, By: "device:x",
		Answers: map[string]string{"q1": "Postgres tsvector", "q2": api.JoinAnswers([]string{"Title", "Body", "Tags"})}})
	if err != nil || r.Status != api.RequestDecided {
		t.Fatalf("valid answers: %v %v", r.Status, err)
	}
	if _, err := c.Decide(ctx, api.Decision{RequestID: "r_question05", Behavior: api.BehaviorDeny, By: "device:y"}); !errors.Is(err, core.ErrAlreadyClosed) {
		t.Errorf("second answer: %v, want already closed", err)
	}
	settle(c)
	act, _ := c.Activity(ctx, "s_notes0004", api.ActivityQuery{})
	text := feedText(act)
	if !strings.Contains(text, "Postgres tsvector") || !strings.Contains(text, "Title, Body, Tags") {
		t.Errorf("the answer doesn't show in the feed:\n%s", text)
	}
	s, _ := c.Session(ctx, "s_notes0004")
	if s.State != api.StateWorking {
		t.Errorf("after answering, session is %s", s.State)
	}
}

func feedText(a api.Activity) string {
	var b strings.Builder
	for _, it := range a.Items {
		b.WriteString(string(it.Kind) + " " + it.Summary + " " + it.Text + " → " + it.Result + "\n")
	}
	return b.String()
}

func TestPlanRevisionAndApproval(t *testing.T) {
	c := newDemo(t)
	if _, err := c.Decide(ctx, api.Decision{RequestID: "r_plan0006", Behavior: api.BehaviorDeny, Message: "Keep passwords for two releases, not one.", By: "device:x"}); err != nil {
		t.Fatal(err)
	}
	settle(c)
	var revised *api.Request
	for _, r := range c.sortedOpen() {
		if r.Checkpoint == api.CheckpointPlan {
			revised = &r
		}
	}
	if revised == nil || revised.ID == "r_plan0006" || !strings.Contains(revised.Plan.Markdown, "Keep passwords for two releases") {
		t.Fatalf("no revised plan with the feedback: %+v", revised)
	}
	if strings.Count(revised.Plan.Markdown, "## What changed") != 1 {
		t.Errorf("revised plan:\n%s", revised.Plan.Markdown)
	}
	if _, err := c.Decide(ctx, api.Decision{RequestID: revised.ID, Behavior: api.BehaviorAllow, By: "device:x"}); err != nil {
		t.Fatal(err)
	}
	settle(c)
	s, _ := c.Session(ctx, "s_webapp007")
	act, _ := c.Activity(ctx, s.ID, api.ActivityQuery{})
	if s.State != api.StateWorking || !strings.Contains(feedText(act), "Plan approved") {
		t.Errorf("after approval: %s\n%s", s.State, feedText(act))
	}
}

func TestPermissionReactions(t *testing.T) {
	c := newDemo(t)
	if _, err := c.Decide(ctx, api.Decision{RequestID: "r_gitpush03", Behavior: api.BehaviorAllow, Scope: api.ScopeOnce, By: "device:x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Decide(ctx, api.Decision{RequestID: "r_tfedit04", Behavior: api.BehaviorDeny, Message: "Not on a Friday", By: "device:x"}); err != nil {
		t.Fatal(err)
	}
	settle(c)
	ph, _ := c.Activity(ctx, "s_pilothouse01", api.ActivityQuery{})
	if !strings.Contains(feedText(ph), "main -> main") {
		t.Errorf("push result missing:\n%s", feedText(ph))
	}
	inf, _ := c.Activity(ctx, "s_infra0006", api.ActivityQuery{})
	last := inf.Items[len(inf.Items)-1]
	if !strings.Contains(feedText(inf), "Denied from Pilothouse: Not on a Friday") || !strings.Contains(last.Text, "Not on a Friday") {
		t.Errorf("deny not reflected:\n%s", feedText(inf))
	}
	for _, id := range []string{"s_pilothouse01", "s_infra0006"} {
		s, _ := c.Session(ctx, id)
		if s.State != api.StateWaiting || s.LastMessage == "" {
			t.Errorf("%s: %s %q", id, s.State, s.LastMessage)
		}
	}
	for _, it := range ph.Items {
		if it.Pending {
			t.Errorf("still pending after the answer: %+v", it)
		}
	}
}

func TestActivityCursorAndLimit(t *testing.T) {
	c := newDemo(t)
	all, err := c.Activity(ctx, "s_pilothouse01", api.ActivityQuery{})
	if err != nil || !all.Available || len(all.Items) < 10 || all.Usage == nil || len(all.Files) == 0 || all.CatchUp == nil {
		t.Fatalf("activity: %+v %v", all, err)
	}
	if all.Items[0].Kind != api.ActivityPrompt || all.Items[0].Turn != 1 {
		t.Errorf("first item %+v", all.Items[0])
	}
	two, _ := c.Activity(ctx, "s_pilothouse01", api.ActivityQuery{Limit: 2})
	if len(two.Items) != 2 || two.Items[1].ID != all.Items[len(all.Items)-1].ID {
		t.Errorf("limit keeps the newest: %+v", two.Items)
	}
	none, _ := c.Activity(ctx, "s_pilothouse01", api.ActivityQuery{Cursor: all.Cursor})
	if len(none.Items) != 0 || none.Cursor != all.Cursor {
		t.Errorf("cursor at the end returned %d items", len(none.Items))
	}
	if _, err := c.Reply(ctx, "s_pilothouse01", api.Reply{Text: "Open a PR instead of pushing to main"}, "device:x"); err != nil {
		t.Fatal(err)
	}
	newer, _ := c.Activity(ctx, "s_pilothouse01", api.ActivityQuery{Cursor: all.Cursor})
	if len(newer.Items) != 1 || newer.Items[0].Kind != api.ActivityPrompt || newer.Items[0].Source != "pilothouse" || newer.Items[0].Turn != 2 {
		t.Fatalf("reply in feed: %+v", newer.Items)
	}
	if _, err := c.Activity(ctx, "s_pilothouse01", api.ActivityQuery{Cursor: "bogus"}); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("bad cursor: %v", err)
	}
	if _, err := c.Activity(ctx, "s_nope", api.ActivityQuery{}); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("unknown session: %v", err)
	}
	s, _ := c.Session(ctx, "s_pilothouse01")
	if s.Usage.CostUSD != newer.Usage.CostUSD || s.Usage.CostUSD <= all.Usage.CostUSD {
		t.Errorf("usage: session %v, feed %v, before %v", s.Usage.CostUSD, newer.Usage.CostUSD, all.Usage.CostUSD)
	}
}

func TestCatchUp(t *testing.T) {
	c := newDemo(t)
	a, _ := c.Activity(ctx, "s_pilothouse01", api.ActivityQuery{Since: time.Now().Add(-time.Hour)})
	cu := a.CatchUp
	joined := strings.Join(cu.Lines, "\n")
	if cu.ToolCalls < 8 || cu.Files != 3 || cu.Errors != 1 {
		t.Errorf("catch-up counts: %+v", cu)
	}
	for _, want := range []string{"Waiting for you: git push origin main", "Tests pass again after a failure (go test -race ./...)", "Changed 3 files: card.js, app.css, fakecore.go."} {
		if !strings.Contains(joined, want) {
			t.Errorf("catch-up lacks %q:\n%s", want, joined)
		}
	}
	if err := c.MarkSeen(ctx, "s_pilothouse01"); err != nil {
		t.Fatal(err)
	}
	a, _ = c.Activity(ctx, "s_pilothouse01", api.ActivityQuery{})
	if a.CatchUp.ToolCalls != 0 || a.CatchUp.Lines[0] != "Nothing new since you last looked." {
		t.Errorf("after looking: %+v", a.CatchUp)
	}
}

func TestReplies(t *testing.T) {
	c := newDemo(t)
	sub, _ := c.Subscribe(ctx, c.Seq())
	defer sub.Close()
	res, err := c.Reply(ctx, "s_notes0004", api.Reply{Text: "Use the fastest option"}, "device:x")
	if err != nil || res.Via != api.ReplyPlugin {
		t.Fatalf("plugin reply: %+v %v", res, err)
	}
	found := false
	for len(sub.C) > 0 {
		if ev := <-sub.C; ev.Type == api.EventSessionReply && strings.Contains(string(ev.Data), "Use the fastest option") && strings.Contains(string(ev.Data), `"by":"device:x"`) {
			found = true
		}
	}
	if !found {
		t.Error("no session.reply event")
	}
	settle(c)
	act, _ := c.Activity(ctx, "s_notes0004", api.ActivityQuery{Limit: 1})
	if act.Items[0].Kind != api.ActivityText {
		t.Errorf("no answer after the reply: %+v", act.Items)
	}
	if _, err := c.Reply(ctx, "s_site000002", api.Reply{Text: "hi"}, "device:x"); !errors.Is(err, core.ErrUnsupported) || !strings.Contains(err.Error(), "pilothouse run codex") {
		t.Errorf("observed codex reply: %v", err)
	}
	if _, err := c.Reply(ctx, "s_infra0006", api.Reply{Interrupt: true}, "device:x"); !errors.Is(err, core.ErrUnsupported) {
		t.Errorf("interrupt observed: %v", err)
	}
	if _, err := c.Reply(ctx, "s_dotfile05", api.Reply{Text: "hi"}, "device:x"); !errors.Is(err, core.ErrUnsupported) {
		t.Errorf("reply to ended session: %v", err)
	}
	if _, err := c.Reply(ctx, "s_apisrv003", api.Reply{Interrupt: true}, "device:x"); err != nil {
		t.Fatal(err)
	}
	s, _ := c.Session(ctx, "s_apisrv003")
	if s.State != api.StateWaiting {
		t.Errorf("after interrupt: %s", s.State)
	}
}

func TestRules(t *testing.T) {
	c := newDemo(t)
	v, _ := c.Rules(ctx)
	sources := map[string]int{}
	for _, r := range v.Rules {
		sources[r.Source]++
		if r.Managed != (r.Source == api.RuleSourceManaged) {
			t.Errorf("%s: managed %v from %s", r.Name, r.Managed, r.Source)
		}
	}
	for _, s := range []string{api.RuleSourceBuiltin, api.RuleSourceUser, api.RuleSourceManaged, api.RuleSourcePreset, api.RuleSourceRepo} {
		if sources[s] == 0 {
			t.Errorf("no %s rules", s)
		}
	}
	if len(v.Presets) != 4 || v.Presets[0].Name != "guarded-autopilot" || !strings.Contains(v.Presets[0].Description, "--dangerously-skip-permissions") {
		t.Errorf("presets: %+v", v.Presets)
	}

	sub, _ := c.Subscribe(ctx, c.Seq())
	defer sub.Close()
	sg, err := c.SuggestRules(ctx, "r_gitpush03")
	if err != nil || len(sg) < 2 {
		t.Fatalf("suggestions: %v %v", sg, err)
	}
	for _, s := range sg {
		if s.Rule.Action == api.BehaviorAllow {
			t.Errorf("a high-risk push shouldn't suggest allowing it: %+v", s)
		}
	}
	sg, _ = c.SuggestRules(ctx, "r_npmtest01")
	var allowHere api.RuleSuggestion
	for _, s := range sg {
		if s.Scope == "repo" && s.Rule.Action == api.BehaviorAllow {
			allowHere = s
		}
	}
	if allowHere.ID == "" || allowHere.Title != "Always allow `pnpm test` in proofline" || allowHere.Reason == "" {
		t.Fatalf("suggestions for pnpm test: %+v", sg)
	}
	if allowHere.Rule.Name == "allow-pnpm-test-proofline" {
		t.Error("suggested name clashes with an existing rule")
	}
	if q, _ := c.SuggestRules(ctx, "r_question05"); len(q) != 0 {
		t.Errorf("questions get no rule suggestions: %v", q)
	}
	info, err := c.AddRule(ctx, allowHere.Rule, "device:x")
	if err != nil || !info.Managed || info.Source != api.RuleSourceManaged || info.By != "device:x" {
		t.Fatalf("add: %+v %v", info, err)
	}
	if ev := <-sub.C; ev.Type != api.EventPolicyChanged || !strings.Contains(string(ev.Data), `"op":"add"`) {
		t.Errorf("event %s %s", ev.Type, ev.Data)
	}
	if _, err := c.AddRule(ctx, allowHere.Rule, "device:x"); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("duplicate add: %v", err)
	}
	for _, bad := range []api.RuleSpec{
		{Name: "Bad Name", Action: api.BehaviorAllow, Match: map[string][]string{"command": {"x"}}},
		{Name: "x", Action: "maybe", Match: map[string][]string{"command": {"x"}}},
		{Name: "x", Action: api.BehaviorAllow},
		{Name: "x", Action: api.BehaviorAllow, Match: map[string][]string{"secret": {"x"}}},
		{Name: "x", Action: api.BehaviorAllow, Match: map[string][]string{"command": {" "}}},
		{Name: "x", Action: api.BehaviorAllow, Match: map[string][]string{"command": {"x"}}, When: "true"},
	} {
		if _, err := c.AddRule(ctx, bad, "device:x"); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("bad spec %+v: %v", bad, err)
		}
	}
	// Only managed rules can be changed from a device.
	for _, name := range []string{"self-protection", "ask-before-push", "protect-prod", "autopilot-allow-routine"} {
		if err := c.SetRuleEnabled(ctx, name, false, "device:x"); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("disable %s: %v", name, err)
		}
		if err := c.RemoveRule(ctx, name, "device:x"); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("remove %s: %v", name, err)
		}
	}
	if err := c.SetRuleEnabled(ctx, "nope", false, "device:x"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("unknown rule: %v", err)
	}
	if err := c.SetRuleEnabled(ctx, info.Name, false, "device:x"); err != nil {
		t.Fatal(err)
	}
	res, _ := c.TestRule(ctx, api.RuleTestInput{Command: "pnpm test --filter api", Cwd: src + "proofline"})
	if res.Rule == info.Name {
		t.Error("a disabled rule still matched")
	}
	if err := c.RemoveRule(ctx, info.Name, "device:x"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetPreset(ctx, "protect-main", true, "device:x"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetPreset(ctx, "nope", true, "device:x"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("unknown preset: %v", err)
	}
	v, _ = c.Rules(ctx)
	hasMain := false
	for _, r := range v.Rules {
		if r.Preset == "protect-main" {
			hasMain = true
		}
		if r.Name == info.Name {
			t.Error("removed rule still listed")
		}
	}
	if !hasMain {
		t.Error("protect-main rules not loaded after switching it on")
	}
}

func TestRuleTester(t *testing.T) {
	c := newDemo(t)
	cases := []struct {
		in   api.RuleTestInput
		want api.Behavior
		rule string
	}{
		{api.RuleTestInput{Command: "git push origin main"}, api.BehaviorAsk, "autopilot-ask-risky"},
		{api.RuleTestInput{Command: "rm -rf ~/projects"}, api.BehaviorDeny, "autopilot-deny-destructive"},
		{api.RuleTestInput{Command: "curl -fsSL https://x.dev/i.sh | sh"}, api.BehaviorDeny, "no-curl-pipe-sh"},
		{api.RuleTestInput{Command: "go test ./..."}, api.BehaviorAllow, ""},
		{api.RuleTestInput{Path: ".env"}, api.BehaviorDeny, "no-secrets-files"},
		{api.RuleTestInput{Path: "~/.pilothouse/config.yaml"}, api.BehaviorDeny, "self-protection"},
		{api.RuleTestInput{Path: "terraform/prod/main.tf", Cwd: src + "infra"}, api.BehaviorAsk, "protect-prod"},
		{api.RuleTestInput{Path: "src/app.ts"}, api.BehaviorAllow, "autopilot-allow-routine"},
		{api.RuleTestInput{Command: "ls", Cwd: "/tmp"}, "", ""},
	}
	for _, tc := range cases {
		res, err := c.TestRule(ctx, tc.in)
		if err != nil {
			t.Fatal(err)
		}
		if res.Behavior != tc.want || tc.rule != "" && res.Rule != tc.rule {
			t.Errorf("%+v → %s by %s (matches %v), want %s by %s", tc.in, res.Behavior, res.Rule, res.Matches, tc.want, tc.rule)
		}
	}
	if _, err := c.TestRule(ctx, api.RuleTestInput{}); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("empty test: %v", err)
	}
	// Turning guarded autopilot off changes the answer.
	_ = c.SetPreset(ctx, "guarded-autopilot", false, "device:x")
	res, _ := c.TestRule(ctx, api.RuleTestInput{Command: "rm -rf ~/projects"})
	if res.Behavior != "" {
		t.Errorf("without autopilot: %+v", res)
	}
}

func TestGlob(t *testing.T) {
	for _, tc := range []struct {
		pat, s string
		path   bool
		want   bool
	}{
		{"git push*", "git push --force origin main", false, true},
		{"git push*", "git pull", false, false},
		{"**/.env", "/Users/dev/src/a/.env", true, true},
		{"**/.env", "/Users/dev/src/a/.env.local", true, false},
		{"~/.ssh/**", Home + "/.ssh/id_ed25519", true, true},
		{Home + "/src/**", Home + "/src/a/b", true, true},
		{"src/*.ts", "src/a/b.ts", true, false},
		{"*.{ts,tsx}", "a.tsx", true, true},
		{"a.b", "axb", false, false},
	} {
		if got := globMatch(tc.pat, tc.s, tc.path); got != tc.want {
			t.Errorf("glob %q %q = %v", tc.pat, tc.s, got)
		}
	}
}

func TestInsightsAddUp(t *testing.T) {
	c := newDemo(t)
	for _, days := range []int{0, 1, 7, 30, 90, 500} {
		in, err := c.Insights(ctx, api.InsightsQuery{Days: days})
		if err != nil {
			t.Fatal(err)
		}
		want := days
		switch {
		case days == 0:
			want = 7
		case days > 90:
			want = 90
		}
		if len(in.Days) != want {
			t.Fatalf("days=%d: %d days", days, len(in.Days))
		}
		var cost float64
		var tokens int64
		auto := 0
		for _, d := range in.Days {
			cost += d.Usage.CostUSD
			tokens += d.Usage.InputTokens
			auto += d.AutoAllowed
		}
		near := func(a, b float64) bool { return math.Abs(a-b) < 0.05+0.001*b }
		if !near(cost, in.Total.CostUSD) || tokens != in.Total.InputTokens || auto != in.Rules.AutoAllowed {
			t.Errorf("days=%d: day sums %v/%d/%d, totals %v/%d/%d", days, cost, tokens, auto, in.Total.CostUSD, in.Total.InputTokens, in.Rules.AutoAllowed)
		}
		for name, groups := range map[string][]api.UsageGroup{"agent": in.ByAgent, "model": in.ByModel, "repo": in.ByRepo} {
			var gc float64
			var gt int64
			for _, g := range groups {
				gc += g.Usage.CostUSD
				gt += g.Usage.InputTokens
			}
			if !near(gc, in.Total.CostUSD) || gt != in.Total.InputTokens {
				t.Errorf("days=%d by %s: %v/%d vs %v/%d", days, name, gc, gt, in.Total.CostUSD, in.Total.InputTokens)
			}
		}
		if in.Budget == nil || in.Budget.SpentUSD != in.Days[len(in.Days)-1].Usage.CostUSD {
			t.Errorf("budget %+v", in.Budget)
		}
		if in.Total.CostUSD <= 0 || in.Rules.MedianAnswerSecs <= 0 || len(in.Rules.TopRules) == 0 {
			t.Errorf("days=%d: empty insights %+v", days, in.Rules)
		}
	}
	a, _ := c.Insights(ctx, api.InsightsQuery{Days: 30})
	b, _ := c.Insights(ctx, api.InsightsQuery{Days: 30})
	if a.Days[0] != b.Days[0] {
		t.Error("past days change between reads")
	}
}

func TestTasks(t *testing.T) {
	c := newDemo(t)
	repos, _ := c.Repos(ctx)
	if len(repos) < 5 {
		t.Fatalf("repos %v", repos)
	}
	for _, r := range repos {
		if strings.Contains(r.Root, "worktrees") {
			t.Errorf("task worktree listed as a repo: %s", r.Root)
		}
	}
	if _, err := c.StartTask(ctx, api.TaskStart{Repo: "/etc", Prompt: "x"}, "device:x"); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("unknown repo: %v", err)
	}
	if _, err := c.StartTask(ctx, api.TaskStart{Repo: src + "site", Agent: "gemini", Prompt: "x"}, "device:x"); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("unknown agent: %v", err)
	}
	task, err := c.StartTask(ctx, api.TaskStart{Repo: src + "site", Prompt: "Format prices with Intl.NumberFormat"}, "device:x")
	if err != nil || task.Status != api.TaskRunning || task.Branch != "ph/format-prices-with-intl-numberformat" || task.SessionID == "" {
		t.Fatalf("start: %+v %v", task, err)
	}
	if _, err := c.FinishTask(ctx, task.ID, api.TaskFinish{Action: "ship"}, "device:x"); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("ship while running: %v", err)
	}
	settle(c)
	got, _ := c.FakeCore.Task(task.ID)
	d, _ := c.TaskDiff(ctx, task.ID)
	if got.Status != api.TaskReview || len(d.Files) != 2 || d.Added == 0 {
		t.Fatalf("after the run: %s, diff %+v", got.Status, d)
	}
	act, _ := c.Activity(ctx, task.SessionID, api.ActivityQuery{})
	if act.Items[0].Source != "pilothouse" || len(act.Files) != 2 {
		t.Errorf("task feed: %s", feedText(act))
	}
	shipped, err := c.FinishTask(ctx, task.ID, api.TaskFinish{Action: "ship"}, "device:x")
	if err != nil || shipped.Status != api.TaskDone {
		t.Fatalf("ship: %+v %v", shipped, err)
	}
	s, _ := c.Session(ctx, task.SessionID)
	if s.State != api.StateExited || !strings.Contains(s.LastMessage, "pull request") {
		t.Errorf("session after ship: %s %q", s.State, s.LastMessage)
	}
	if _, err := c.FinishTask(ctx, task.ID, api.TaskFinish{Action: "discard"}, "device:x"); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("discard a shipped task: %v", err)
	}
	if _, err := c.FinishTask(ctx, "t_flaky02", api.TaskFinish{Action: "discard"}, "device:x"); err != nil {
		t.Errorf("discard running: %v", err)
	}
	if _, err := c.TaskDiff(ctx, "t_nope"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("unknown task: %v", err)
	}
	if fd, _ := c.TaskDiff(ctx, "t_faq01"); len(fd.Files) != 2 {
		t.Errorf("faq diff %+v", fd)
	}
}

func TestTerminal(t *testing.T) {
	c := newDemo(t)
	if err := c.WatchTerminal(ctx, "s_infra0006", true, func([]byte) error { return nil }); !errors.Is(err, core.ErrUnsupported) {
		t.Errorf("observed session terminal: %v", err)
	}
	wctx, cancel := context.WithCancel(ctx)
	var mu sync.Mutex
	var got strings.Builder
	done := make(chan error, 1)
	go func() {
		done <- c.WatchTerminal(wctx, "s_pilothouse01", true, func(b []byte) error {
			mu.Lock()
			got.Write(b)
			mu.Unlock()
			return nil
		})
	}()
	waitFor := func(needle string) {
		t.Helper()
		for i := 0; i < 200; i++ {
			mu.Lock()
			ok := strings.Contains(got.String(), needle)
			mu.Unlock()
			if ok {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("terminal never showed %q:\n%s", needle, stripANSI(got.String()))
	}
	waitFor("Claude Code")
	waitFor("git push origin main")
	waitFor("Waiting for you")
	if err := c.TerminalInput(ctx, "s_pilothouse01", []byte("run the tests\x7f\x7f\x7f\x7f\x7fsuite"), "device:x"); err != nil {
		t.Fatal(err)
	}
	waitFor("run the suite")
	if err := c.TerminalInput(ctx, "s_pilothouse01", []byte("\r"), "device:x"); err != nil {
		t.Fatal(err)
	}
	waitFor("sent from Pilothouse")
	act, _ := c.Activity(ctx, "s_pilothouse01", api.ActivityQuery{Limit: 1})
	if act.Items[0].Text != "run the suite" {
		t.Errorf("typed line didn't become a prompt: %+v", act.Items[0])
	}
	if err := c.TerminalInput(ctx, "s_pilothouse01", make([]byte, maxTerminalInput+1), "device:x"); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("oversized input: %v", err)
	}
	if err := c.ResizeTerminal(ctx, "s_pilothouse01", 80, 24); err != nil {
		t.Error(err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("watch ended with %v", err)
	}
	c.mu.Lock()
	n := len(c.terms["s_pilothouse01"].watchers)
	c.mu.Unlock()
	if n != 0 {
		t.Errorf("%d watchers left", n)
	}
}

func TestExpiryHandsBack(t *testing.T) {
	now := time.Now()
	c := New(Options{Quiet: true, Now: func() time.Time { return now }})
	closed := c.ExpireRequests(now.Add(11 * time.Minute))
	for _, r := range closed {
		c.expired(r)
	}
	if len(closed) != 4 {
		t.Fatalf("expired %d requests", len(closed))
	}
	s, _ := c.Session(ctx, "s_pilothouse01")
	if s.State != api.StateBlocked || !strings.HasPrefix(s.StateDetail, "Waiting in the terminal") {
		t.Errorf("after expiry: %s %q", s.State, s.StateDetail)
	}
}

func TestRunIsLivelyAndStops(t *testing.T) {
	c := New(Options{})
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { c.Run(rctx); close(done) }()
	c.work()
	c.raiseNext()
	open := 0
	for _, r := range c.Requests() {
		if r.Status == api.RequestOpen {
			open++
		}
	}
	if open != 5 {
		t.Errorf("open requests after one raise: %d", open)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run didn't stop")
	}
}

func TestConcurrentUse(t *testing.T) {
	c := New(Options{})
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.Run(rctx)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_, _ = c.Activity(ctx, "s_pilothouse01", api.ActivityQuery{})
				_, _ = c.Insights(ctx, api.InsightsQuery{Days: 7})
				_, _ = c.Rules(ctx)
				_, _ = c.TestRule(ctx, api.RuleTestInput{Command: "git push"})
				_, _ = c.Reply(ctx, "s_apisrv003", api.Reply{Text: "status?"}, "device:x")
				c.work()
				c.tickTerminals()
				_, _ = c.Snapshot(ctx)
			}
		}(i)
	}
	wg.Wait()
}
