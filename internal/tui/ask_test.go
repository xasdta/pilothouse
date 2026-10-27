package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/client"
)

const planMD = `# Add OAuth login

Users sign in with GitHub instead of a password.

## Steps
1. Register the OAuth app and store the client secret in **Vault**
2. Add ` + "`/auth/callback`" + ` and exchange the code for a token
   - keep the session cookie HttpOnly
3. Remove the password form

> Rollback: revert the migration; sessions stay valid.

` + "```sh\nmake migrate\n```" + `
`

// askSnapshot is the fixture plus a held question (two questions, the
// second multi-select), a held plan and a question shown in the agent's
// own dialog.
func askSnapshot() api.Snapshot {
	s := fixtureSnapshot()
	s.Requests = append(s.Requests,
		api.Request{ID: "r_q7a8b9c0de", SessionID: "s_c2d7f5h1jq", Agent: api.AgentClaude, Checkpoint: api.CheckpointQuestion,
			Tool: "AskUserQuestion", ToolKind: api.ToolOther, Summary: "Asks: Which database should the migration target? (+1 more)",
			Cwd: "/home/dev/src/api", Status: api.RequestOpen, Route: api.RouteDevices,
			Context:   "Two databases are configured; I need to know which one the migration is for.",
			CreatedAt: t0.Add(-40 * time.Second), ExpiresAt: t0.Add(8 * time.Minute),
			Questions: []api.Question{
				{Header: "Database", Text: "Which database should the migration target?", AllowOther: true, Options: []api.QuestionOption{
					{Label: "Postgres 16", Description: "What production runs today"},
					{Label: "Postgres 17", Description: "Newer; needs a staging run first"}}},
				{Header: "Checks", Text: "Which checks should run after it?", MultiSelect: true, AllowOther: true, Options: []api.QuestionOption{
					{Label: "Lint"}, {Label: "Unit tests"}, {Label: "E2E", Description: "slow: about 9 minutes"}}},
			}},
		api.Request{ID: "r_p1a2n3x4yz", SessionID: "s_q9r7s5t3uv", Agent: api.AgentClaude, Checkpoint: api.CheckpointPlan,
			Tool: "ExitPlanMode", ToolKind: api.ToolOther, Summary: "Plan ready: Add OAuth login", Cwd: "/home/dev/src/pilothouse-site",
			Status: api.RequestOpen, Route: api.RouteDevices, CreatedAt: t0.Add(-30 * time.Second), ExpiresAt: t0.Add(9 * time.Minute),
			Plan: &api.Plan{Markdown: planMD, Path: "/home/dev/.claude/plans/oauth.md"}},
	)
	return s
}

func loadedAsks(t *testing.T, w, h int) (*Model, *fakeBackend) {
	t.Helper()
	be := &fakeBackend{snap: askSnapshot()}
	m := newTestModel(t, be, w, h)
	m.Update(snapshotMsg{snap: be.snap})
	t.Cleanup(m.stopStream)
	return m, be
}

func selectKey(t *testing.T, m *Model, key string) {
	t.Helper()
	for i, r := range m.rows {
		if r.key == key {
			m.cursor, m.selKey = i, key
			m.clampScroll()
			return
		}
	}
	t.Fatalf("no row %s", key)
}

func typeText(m *Model, s string) {
	for _, r := range s {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func TestGoldenQuestionsAndPlans(t *testing.T) {
	m, _ := loadedAsks(t, 120, 35)
	golden(t, "asks_question_row_120x35", m.View()) // held first: the question is selected
	selectKey(t, m, "r:r_p1a2n3x4yz")
	golden(t, "asks_plan_row_120x35", m.View())

	m, _ = loadedAsks(t, 100, 30)
	selectKey(t, m, "r:r_q7a8b9c0de")
	press(m, "enter")
	golden(t, "question_100x30", m.View())
	press(m, "2") // Postgres 17 → question 2
	press(m, "1", "down", "down", " ")
	golden(t, "question_multi_100x30", m.View())
	press(m, "o")
	typeText(m, "smoke tests only")
	golden(t, "question_other_100x30", m.View())

	m, _ = loadedAsks(t, 100, 30)
	selectKey(t, m, "r:r_p1a2n3x4yz")
	press(m, "enter")
	golden(t, "plan_100x30", m.View())
	press(m, "c")
	typeText(m, "use PKCE")
	golden(t, "plan_changes_100x30", m.View())

	m, _ = loadedAsks(t, 64, 16)
	selectKey(t, m, "r:r_p1a2n3x4yz")
	press(m, "enter", "j", "j")
	golden(t, "plan_scrolled_64x16", m.View())
}

func TestAnswerQuestionsInThePanel(t *testing.T) {
	m, be := loadedAsks(t, 120, 35)
	selectKey(t, m, "r:r_q7a8b9c0de")
	if !strings.Contains(m.View(), "held · answer ⏎") || !strings.Contains(m.View(), "enter answer") {
		t.Fatalf("row hint / footer:\n%s", m.View())
	}
	// y and a don't answer a question.
	if cmd := press(m, "y"); cmd != nil || !strings.Contains(m.View(), "Pick an answer") {
		t.Fatalf("y on a question:\n%s", m.View())
	}
	press(m, "a")
	if len(be.decides) != 0 {
		t.Fatal("y/a must not decide a question")
	}

	press(m, "enter")
	if m.mode != modeQuestion || m.ask.qi != 0 {
		t.Fatalf("mode = %v", m.mode)
	}
	press(m, "down", "enter") // second option of question 1
	if m.ask.qi != 1 || m.ask.answers["Which database should the migration target?"] != "Postgres 17" {
		t.Fatalf("after q1: %+v", m.ask)
	}
	if !strings.Contains(m.View(), "So far: Which database should the migration target? → Postgres 17") {
		t.Fatalf("recap missing:\n%s", m.View())
	}
	// Multi-select: digits and space toggle, enter sends the set.
	press(m, "1", "3", "3", "3")
	press(m, "down") // cursor wraps 3 → Other…
	press(m, "up")
	if !m.ask.picked[0] || !m.ask.picked[2] {
		t.Fatalf("picked = %v", m.ask.picked)
	}
	cmd := press(m, "enter")
	if m.mode != modeList {
		t.Fatalf("mode after the last answer = %v", m.mode)
	}
	execCmd(t, m, cmd)
	if len(be.decides) != 1 {
		t.Fatalf("decides = %+v", be.decides)
	}
	d := be.decides[0]
	want := map[string]string{"Which database should the migration target?": "Postgres 17", "Which checks should run after it?": "Lint, E2E"}
	if d.id != "r_q7a8b9c0de" || d.body.Behavior != api.BehaviorAllow || d.body.Scope != "" || len(d.body.Answers) != 2 ||
		d.body.Answers["Which database should the migration target?"] != want["Which database should the migration target?"] ||
		d.body.Answers["Which checks should run after it?"] != want["Which checks should run after it?"] {
		t.Fatalf("decide = %+v", d)
	}
	if !strings.Contains(m.View(), "✓ Answered: Which database should the migration target? → Postgres 17 · Which checks") {
		t.Fatalf("flash:\n%s", m.View())
	}
	if _, ok := m.requests["r_q7a8b9c0de"]; ok {
		t.Fatal("answered question must leave the list")
	}
}

func TestQuestionDigitsOwnWordsAndDecline(t *testing.T) {
	m, be := loadedAsks(t, 120, 35)
	selectKey(t, m, "r:r_q7a8b9c0de")
	press(m, "2") // from the list: opens on option 2, doesn't answer
	if m.mode != modeQuestion || m.ask.cursor != 1 || len(be.decides) != 0 {
		t.Fatalf("digit from list: mode %v cursor %d", m.mode, m.ask.cursor)
	}
	press(m, "o")
	if m.mode != modeOther {
		t.Fatal("o types an own answer")
	}
	press(m, "enter") // empty: stays
	if m.mode != modeOther || !strings.Contains(m.View(), "Type your answer") {
		t.Fatalf("empty own answer:\n%s", m.View())
	}
	typeText(m, "the analytics replica")
	press(m, "enter")
	if m.mode != modeQuestion || m.ask.answers["Which database should the migration target?"] != "the analytics replica" {
		t.Fatalf("own answer: %+v", m.ask)
	}
	// esc leaves without sending.
	press(m, "esc")
	if m.mode != modeList || len(be.decides) != 0 {
		t.Fatal("esc must not answer")
	}

	// n declines, with an optional reason; esc from the reason goes back.
	press(m, "enter", "n")
	if m.mode != modeDeny || !strings.Contains(m.View(), "Decline r_q7a8b") {
		t.Fatalf("decline prompt:\n%s", m.View())
	}
	press(m, "esc")
	if m.mode != modeQuestion {
		t.Fatalf("esc from decline = %v", m.mode)
	}
	press(m, "n")
	typeText(m, "ask me after lunch")
	execCmd(t, m, press(m, "enter"))
	if len(be.decides) != 1 || be.decides[0].body.Behavior != api.BehaviorDeny || be.decides[0].body.Message != "ask me after lunch" {
		t.Fatalf("decline = %+v", be.decides)
	}
	if !strings.Contains(m.View(), "✗ Declined to answer: Asks: Which database") {
		t.Fatalf("flash:\n%s", m.View())
	}
}

func TestQuestionClosedElsewhereOrInvalid(t *testing.T) {
	m, _ := loadedAsks(t, 120, 35)
	selectKey(t, m, "r:r_q7a8b9c0de")
	press(m, "enter")
	closed := askSnapshot().Requests[3]
	closed.Status = api.RequestDecided
	closed.Decision = &api.Decision{Behavior: api.BehaviorAllow, By: "device:d_phone"}
	b, _ := json.Marshal(closed)
	m.Update(eventMsg{gen: m.gen, ev: api.Event{Seq: 200, Type: api.EventRequestClosed, Data: b}})
	if m.mode != modeList || !strings.Contains(m.View(), "Answered elsewhere") {
		t.Fatalf("closed elsewhere:\n%s", m.View())
	}
	// Also while typing a reason to decline it.
	m.Update(snapshotMsg{snap: askSnapshot()})
	selectKey(t, m, "r:r_q7a8b9c0de")
	press(m, "enter", "n")
	m.Update(eventMsg{gen: m.gen, ev: api.Event{Seq: 300, Type: api.EventRequestClosed, Data: b}})
	if m.mode != modeList || m.ask.id != "" {
		t.Fatalf("closed while declining: mode %v", m.mode)
	}

	m, be := loadedAsks(t, 120, 35)
	be.decideErr = &client.APIError{Status: 400, Code: "invalid", Msg: "“Which checks should run after it?” has no option “Fuzz”"}
	selectKey(t, m, "r:r_q7a8b9c0de")
	press(m, "enter", "1", "1")
	execCmd(t, m, press(m, "enter"))
	if !strings.Contains(m.View(), "Not sent: “Which checks should run after it?” has no option") {
		t.Fatalf("invalid:\n%s", m.View())
	}
	if _, ok := m.requests["r_q7a8b9c0de"]; !ok {
		t.Fatal("a refused answer keeps the request")
	}
}

func TestSteppedAsideQuestionIsAnsweredInTheTerminal(t *testing.T) {
	s := askSnapshot()
	s.Requests[3].Status, s.Requests[3].Route = api.RequestSteppedAside, api.RouteTerminal
	be := &fakeBackend{snap: s}
	m := newTestModel(t, be, 120, 35)
	m.Update(snapshotMsg{snap: s})
	defer m.stopStream()
	selectKey(t, m, "r:r_q7a8b9c0de")
	press(m, "1")
	if m.mode != modeList || !strings.Contains(m.View(), "answer it in that terminal") {
		t.Fatalf("stepped aside:\n%s", m.View())
	}
	press(m, "enter")
	if m.mode == modeQuestion {
		t.Fatal("enter must not open a question shown in the agent's dialog")
	}
}

func TestPlanApproveAndRequestChanges(t *testing.T) {
	m, be := loadedAsks(t, 100, 30)
	selectKey(t, m, "r:r_p1a2n3x4yz")
	if !strings.Contains(m.View(), "held · review ⏎") && !strings.Contains(m.View(), "enter read") {
		t.Fatalf("plan row:\n%s", m.View())
	}
	press(m, "a")
	if len(be.decides) != 0 || !strings.Contains(m.View(), "A plan is approved once") {
		t.Fatalf("a on a plan row:\n%s", m.View())
	}
	// Read it, scroll, ask for changes.
	press(m, "enter")
	if m.mode != modePlan || !strings.Contains(m.View(), "Add OAuth login") {
		t.Fatalf("plan view:\n%s", m.View())
	}
	press(m, "j", "j", "k")
	if m.ask.scroll != 1 {
		t.Fatalf("scroll = %d", m.ask.scroll)
	}
	press(m, "c")
	typeText(m, "Use PKCE, no client secret")
	press(m, "esc") // back to the plan, nothing sent
	if m.mode != modePlan || len(be.decides) != 0 {
		t.Fatalf("esc from changes: %v", m.mode)
	}
	press(m, "c")
	typeText(m, "Use PKCE, no client secret")
	execCmd(t, m, press(m, "enter"))
	if len(be.decides) != 1 || be.decides[0].body.Behavior != api.BehaviorDeny || be.decides[0].body.Message != "Use PKCE, no client secret" {
		t.Fatalf("changes = %+v", be.decides)
	}
	if !strings.Contains(m.View(), "✎ Asked for changes to the plan: Add OAuth login") {
		t.Fatalf("flash:\n%s", m.View())
	}

	// From the list: y approves, n asks for changes (esc goes back to the list).
	m, be = loadedAsks(t, 100, 30)
	selectKey(t, m, "r:r_p1a2n3x4yz")
	press(m, "n")
	if m.mode != modeChanges {
		t.Fatal("n on a plan asks for changes")
	}
	press(m, "esc")
	if m.mode != modeList {
		t.Fatalf("esc from list changes = %v", m.mode)
	}
	execCmd(t, m, press(m, "y"))
	if len(be.decides) != 1 || be.decides[0].body.Behavior != api.BehaviorAllow || be.decides[0].body.Scope != "" {
		t.Fatalf("approve = %+v", be.decides)
	}
	if !strings.Contains(m.View(), "✓ Approved the plan: Add OAuth login") {
		t.Fatalf("flash:\n%s", m.View())
	}

	// In the plan view, a approves.
	m, be = loadedAsks(t, 100, 30)
	selectKey(t, m, "r:r_p1a2n3x4yz")
	press(m, "enter")
	execCmd(t, m, press(m, "a"))
	if len(be.decides) != 1 || be.decides[0].body.Behavior != api.BehaviorAllow || m.mode != modeList {
		t.Fatalf("a in plan view = %+v (mode %v)", be.decides, m.mode)
	}
}

func TestReplyBox(t *testing.T) {
	s := fixtureSnapshot()
	s.Sessions[5].Caps = api.SessionCaps{Reply: true, ReplyVia: api.ReplyMessage} // pilothouse-site, waiting
	be := &fakeBackend{snap: s}
	m := newTestModel(t, be, 120, 35)
	m.Update(snapshotMsg{snap: s})
	defer m.stopStream()

	selectKey(t, m, "s:s_w1x3y5z7ab") // no reply caps
	if cmd := press(m, "r"); cmd != nil || m.mode != modeList || !strings.Contains(m.View(), "can't take replies") {
		t.Fatalf("r without caps:\n%s", m.View())
	}

	selectKey(t, m, "s:s_q9r7s5t3uv")
	if !strings.Contains(m.View(), "r reply") || !strings.Contains(m.View(), "r to reply") {
		t.Fatalf("reply hints missing:\n%s", m.View())
	}
	press(m, "r")
	if m.mode != modeReply {
		t.Fatal("r opens the reply box")
	}
	typeText(m, "Yes, add the pricing section")
	golden(t, "reply_120x35", m.View())
	// Until the daemon side lands the reply answers 501: say so, calmly.
	execCmd(t, m, press(m, "enter"))
	if len(be.replies) != 1 || be.replies[0].id != "s_q9r7s5t3uv" || be.replies[0].r.Text != "Yes, add the pricing section" {
		t.Fatalf("replies = %+v", be.replies)
	}
	if !strings.Contains(m.View(), "Couldn't send the reply: not supported here") || m.mode != modeList {
		t.Fatalf("501:\n%s", m.View())
	}

	be.replyRes = &api.ReplyResult{Via: api.ReplyMessage, Note: "Claude sees this as a message from Pilothouse"}
	press(m, "r")
	typeText(m, "go on")
	execCmd(t, m, press(m, "enter"))
	if !strings.Contains(m.View(), "✓ Sent to pilothouse-site · main · Claude sees this as a message from Pilothouse") {
		t.Fatalf("sent:\n%s", m.View())
	}
	// Esc and empty replies send nothing.
	n := len(be.replies)
	press(m, "r", "esc")
	press(m, "r")
	press(m, "enter")
	if len(be.replies) != n {
		t.Fatal("esc / empty must not send")
	}
}

func TestActivityInSessionDetail(t *testing.T) {
	be := &fakeBackend{snap: fixtureSnapshot(), activity: map[string]api.Activity{
		"s_w1x3y5z7ab": {SessionID: "s_w1x3y5z7ab", Available: true, Items: []api.ActivityItem{
			{ID: "1", Kind: api.ActivityPrompt, At: t0.Add(-3 * time.Minute), Text: "Build the docs site", Source: "human"},
			{ID: "2", Kind: api.ActivityTool, At: t0.Add(-2 * time.Minute), Summary: "Run: npm ci"},
			{ID: "3", Kind: api.ActivityTool, At: t0.Add(-90 * time.Second), Summary: "Run: npm run lint", IsError: true},
			{ID: "4", Kind: api.ActivityText, At: t0.Add(-time.Minute), Text: "Lint failed on two files; fixing them."},
			{ID: "5", Kind: api.ActivityTool, At: t0.Add(-5 * time.Second), Summary: "Run: npm run build", Pending: true},
		}},
	}}
	m := newTestModel(t, be, 120, 35)
	m.Update(snapshotMsg{snap: be.snap})
	defer m.stopStream()
	m.cursor, m.selKey = 2, "s:s_c2d7f5h1jq"
	execCmd(t, m, press(m, "j")) // onto the docs session
	if m.selKey != "s:s_w1x3y5z7ab" || len(be.activityCalls) != 1 {
		t.Fatalf("sel %s calls %v", m.selKey, be.activityCalls)
	}
	golden(t, "activity_120x35", m.View())
	v := m.View()
	for _, want := range []string{"recent", "✗ Run: npm run lint", "“Lint failed on two files; fixing them.”", "… Run: npm run build"} {
		if !strings.Contains(v, want) {
			t.Errorf("activity lacks %q", want)
		}
	}
	// Fresh: moving away and back doesn't refetch right away.
	execCmd(t, m, press(m, "k"))
	if cmd := press(m, "j"); cmd != nil {
		t.Fatal("a fresh feed must not be fetched again")
	}
	if n := len(be.activityCalls); n != 2 { // the session above (no feed), then nothing for the cached one
		t.Fatalf("calls = %v", be.activityCalls)
	}

	// A daemon without activity (501) is asked once, then left alone.
	be2 := &fakeBackend{snap: fixtureSnapshot()}
	m2 := newTestModel(t, be2, 120, 35)
	m2.Update(snapshotMsg{snap: be2.snap})
	defer m2.stopStream()
	execCmd(t, m2, press(m2, "j"))
	press(m2, "j")
	press(m2, "j")
	if len(be2.activityCalls) != 1 || !strings.Contains(m2.View(), "WORKING") || strings.Contains(m2.View(), "recent") {
		t.Fatalf("501: calls %v\n%s", be2.activityCalls, m2.View())
	}
}

func TestNewModesFitTheTerminal(t *testing.T) {
	for _, sz := range []struct{ w, h int }{{60, 12}, {60, 24}, {80, 8}, {120, 35}} {
		for _, keys := range [][]string{
			{"enter"}, {"enter", "2"}, {"enter", "o"}, {"enter", "n"},
		} {
			m, _ := loadedAsks(t, sz.w, sz.h)
			selectKey(t, m, "r:r_q7a8b9c0de")
			press(m, keys...)
			checkFits(t, m, sz.w, sz.h, keys)
		}
		for _, keys := range [][]string{{"enter"}, {"enter", "G"}, {"enter", "c"}, {"n"}} {
			m, _ := loadedAsks(t, sz.w, sz.h)
			selectKey(t, m, "r:r_p1a2n3x4yz")
			press(m, keys...)
			checkFits(t, m, sz.w, sz.h, keys)
		}
	}
}

func checkFits(t *testing.T, m *Model, w, h int, keys []string) {
	t.Helper()
	lines := strings.Split(m.View(), "\n")
	if len(lines) != h {
		t.Fatalf("%dx%d %v: %d lines", w, h, keys, len(lines))
	}
	for i, l := range lines {
		if lw := lipgloss.Width(l); lw > w {
			t.Fatalf("%dx%d %v: line %d is %d wide: %q", w, h, keys, i, lw, l)
		}
	}
}

func TestRenderMarkdown(t *testing.T) {
	m, _ := loaded(t, 100, 30)
	got := strings.Join(m.renderMarkdown(planMD, 40), "\n")
	for _, want := range []string{
		"Add OAuth login", "Steps",
		"1. Register the OAuth app and store",
		"   client secret in Vault",   // hanging indent, ** dropped
		"  • keep the session cookie", // nested bullet
		"│ Rollback: revert the migration;",
		"  make migrate", // fenced code
	} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "**") || strings.Contains(got, "```") || strings.Contains(got, "##") {
		t.Errorf("markers left:\n%s", got)
	}
	for _, l := range m.renderMarkdown(planMD, 40) {
		if lipgloss.Width(l) > 40 {
			t.Errorf("line wider than 40: %q", l)
		}
	}
}

func TestRefreshMovedToShiftR(t *testing.T) {
	m, be := loaded(t, 120, 35)
	before := be.snaps
	execCmd(t, m, press(m, "R"))
	if be.snaps != before+1 {
		t.Fatal("R reloads")
	}
}
