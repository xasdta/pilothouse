package telegram

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/web/webtest"
)

const chatID = int64(4242)

var bg = context.Background()

// replyCore adds core.Replier to the web test core.
type replyCore struct {
	*webtest.FakeCore
	mu      sync.Mutex
	replies []string // session|text|by
	err     error
}

func (c *replyCore) Reply(_ context.Context, sid string, r api.Reply, by string) (api.ReplyResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return api.ReplyResult{}, c.err
	}
	c.replies = append(c.replies, sid+"|"+r.Text+"|"+by)
	return api.ReplyResult{Via: api.ReplyTerminal, Note: "typed into the terminal"}, nil
}

type fixture struct {
	t      *testing.T
	mock   *mockAPI
	core   *replyCore
	bot    *Bot
	keys   Keys
	paired []Chat
	now    time.Time
}

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "t.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newFixture(t *testing.T, mut ...func(*Options)) *fixture {
	t.Helper()
	t.Setenv("PILOTHOUSE_HOME", t.TempDir())
	f := &fixture{t: t, mock: newMock(t), core: &replyCore{FakeCore: webtest.New()}, now: time.Now()}
	o := Options{
		Client: f.mock.client(), DB: openDB(t), Core: f.core, ChatID: chatID, Decisions: true,
		Keys: func() Keys { return f.keys },
		OnPaired: func(_ context.Context, c Chat, _ string) error {
			f.paired = append(f.paired, c)
			f.keys.PairHash = ""
			return nil
		},
		Machine:    "studio",
		PublicURL:  func() string { return "https://studio.tail1.ts.net" },
		DeviceName: func(id string) string { return map[string]string{"d_phone": "iPhone"}[id] },
		PollWait:   time.Second, ErrorBackoff: 5 * time.Millisecond,
	}
	for _, m := range mut {
		m(&o)
	}
	b, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	f.bot = b
	return f
}

func (f *fixture) open(r api.Request) api.Request {
	if r.SessionID == "" {
		r.SessionID = "s_1"
	}
	r.Agent, r.Status, r.Route = api.AgentClaude, api.RequestOpen, api.RouteDevices
	r.CreatedAt, r.ExpiresAt = time.Now(), time.Now().Add(8*time.Minute)
	f.core.PutSession(api.Session{ID: r.SessionID, Agent: api.AgentClaude, Title: "pilothouse", Branch: "main", State: api.StateBlocked})
	f.core.PutRequest(r)
	return r
}

// notifyRequest sends r as the notifier would and returns the message.
func (f *fixture) notifyRequest(r api.Request) sent {
	f.t.Helper()
	s, _ := f.core.Session(bg, r.SessionID)
	if err := f.bot.Send(bg, core.Notification{Kind: core.NotifyRequest, Request: &r, Session: &s, Remote: true}); err != nil {
		f.t.Fatal(err)
	}
	return f.mock.lastSent(f.t)
}

func (f *fixture) decision(id string) *api.Decision {
	r, err := f.core.Request(bg, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return r.Decision
}

func permissionReq(id, cmd string) api.Request {
	return api.Request{ID: id, Checkpoint: api.CheckpointPermission, Tool: "Bash", ToolKind: api.ToolShell, Command: cmd, Summary: "Run: " + cmd, Cwd: "/src/pilothouse"}
}

func TestPairing(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.ChatID = 0 })
	code := f.keys.NewPairCode(time.Now())

	// Strangers, wrong codes, group chats: ignored without an answer.
	f.bot.Handle(bg, text(77, 77, "hello"))
	f.bot.Handle(bg, text(77, 77, "/start wrongcodewrongcode12"))
	grp := text(-100, 77, "/start "+code)
	grp.Message.Chat.Type = "group"
	f.bot.Handle(bg, grp)
	if f.mock.sentCount() != 0 || len(f.paired) != 0 || f.bot.Chat() != 0 {
		t.Fatalf("unpaired bot answered: sent=%d paired=%v", f.mock.sentCount(), f.paired)
	}
	// Notifications before pairing go nowhere.
	if err := f.bot.Send(bg, core.Notification{Kind: core.NotifyTest, Title: "t"}); err != nil || f.mock.sentCount() != 0 {
		t.Fatalf("send before pairing: %v %d", err, f.mock.sentCount())
	}

	f.bot.Handle(bg, text(chatID, chatID, "/start "+code))
	if len(f.paired) != 1 || f.paired[0].ID != chatID || f.bot.Chat() != chatID {
		t.Fatalf("paired = %+v chat=%d", f.paired, f.bot.Chat())
	}
	if s := f.mock.lastSent(t); s.ChatID != chatID || !strings.Contains(s.Text, "Connected to studio") {
		t.Fatalf("welcome = %+v", s)
	}
	// The code is single-use (OnPaired cleared it): another chat can't take over.
	f.bot.Handle(bg, text(99, 99, "/start "+code))
	if f.bot.Chat() != chatID || len(f.paired) != 1 {
		t.Fatal("pairing code reused")
	}
	// Other chats stay ignored after pairing.
	n := f.mock.sentCount()
	f.bot.Handle(bg, text(99, 99, "/status"))
	if f.mock.sentCount() != n {
		t.Fatal("answered another chat")
	}

	// An expired code is refused.
	f2 := newFixture(t, func(o *Options) { o.ChatID = 0 })
	code2 := f2.keys.NewPairCode(time.Now().Add(-PairTTL - time.Second))
	f2.bot.Handle(bg, text(chatID, chatID, "/start "+code2))
	if f2.bot.Chat() != 0 {
		t.Fatal("expired code accepted")
	}
}

func TestPermissionButtons(t *testing.T) {
	f := newFixture(t)
	r := f.open(permissionReq("r_perm000001", "git push origin <main> && echo '&'"))
	s := f.notifyRequest(r)
	if !strings.Contains(s.Text, "<pre>git push origin &lt;main&gt; &amp;&amp; echo '&amp;'</pre>") || !strings.Contains(s.Text, "<b>Claude Code</b> · pilothouse (main)") {
		t.Fatalf("text not escaped or missing context:\n%s", s.Text)
	}
	bs := s.buttons()
	if len(bs) != 4 || bs[3].URL != "https://studio.tail1.ts.net/r/r_perm000001" {
		t.Fatalf("buttons = %+v", bs)
	}
	for _, b := range bs[:3] {
		if len(b.CallbackData) > 64 || !strings.HasPrefix(b.CallbackData, tokenPrefix) || strings.Contains(b.CallbackData, "r_perm") {
			t.Fatalf("callback data %q: must be an opaque token within 64 bytes", b.CallbackData)
		}
	}

	f.bot.Handle(bg, press(chatID, chatID, s.ID, button(t, bs, "Allow for session")))
	d := f.decision(r.ID)
	if d == nil || d.Behavior != api.BehaviorAllow || d.Scope != api.ScopeSession || d.By != "telegram:4242" {
		t.Fatalf("decision = %+v", d)
	}
	e := f.mock.lastEdit(t)
	if e.MessageID != s.ID || !strings.Contains(e.Text, "✓ Allowed for this session from Telegram") || len(parseButtons(e.Markup)) != 0 {
		t.Fatalf("edit = %+v", e)
	}
	if a := f.mock.lastAnswer(t); a.Text != "✓ Allowed for this session" {
		t.Fatalf("answer = %+v", a)
	}

	// Replay: the same button, and the other buttons of that keyboard, do nothing.
	edits := f.mock.editCount()
	f.bot.Handle(bg, press(chatID, chatID, s.ID, button(t, bs, "Allow for session")))
	if a := f.mock.lastAnswer(t); a.Text != "This button was already used." {
		t.Fatalf("replay answer = %+v", a)
	}
	f.bot.Handle(bg, press(chatID, chatID, s.ID, button(t, bs, "Deny")))
	if d := f.decision(r.ID); d.Behavior != api.BehaviorAllow || f.mock.editCount() != edits {
		t.Fatalf("stale keyboard acted: %+v", d)
	}
	// Forged data and data from another message are refused.
	f.bot.Handle(bg, press(chatID, chatID, s.ID, "t:AAAAAAAAAAAAAAAAAAAAAA"))
	if a := f.mock.lastAnswer(t); a.Text != "This button no longer works." {
		t.Fatalf("forged = %+v", a)
	}
}

func TestWrongChatIgnored(t *testing.T) {
	f := newFixture(t)
	r := f.open(permissionReq("r_wrong00001", "rm -rf build"))
	s := f.notifyRequest(r)
	allow := button(t, s.buttons(), "✅ Allow")
	// Another chat, or another user in our chat id: no answer, no decision.
	f.bot.Handle(bg, press(99, 99, s.ID, allow))
	f.bot.Handle(bg, press(chatID, 99, s.ID, allow))
	if f.mock.answerCount() != 0 || f.decision(r.ID) != nil {
		t.Fatalf("foreign press acted: answers=%d", f.mock.answerCount())
	}
	// The right chat, but the token is bound to a different message.
	f.bot.Handle(bg, press(chatID, chatID, s.ID+1, allow))
	if f.decision(r.ID) != nil {
		t.Fatal("token used on another message")
	}
	f.bot.Handle(bg, press(chatID, chatID, s.ID, allow))
	if d := f.decision(r.ID); d == nil || d.Behavior != api.BehaviorAllow {
		t.Fatalf("decision = %+v", d)
	}
}

func TestDecisionsOff(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.Decisions = false })
	r := f.open(permissionReq("r_off0000001", "make deploy"))
	s := f.notifyRequest(r)
	for _, b := range s.buttons() {
		if b.CallbackData != "" {
			t.Fatalf("notify-only bot offered %q", b.Text)
		}
	}
	if len(s.buttons()) != 1 || !strings.Contains(s.buttons()[0].Text, "Open in the app") {
		t.Fatalf("buttons = %+v", s.buttons())
	}
	// Even a token minted earlier can't act.
	tok, err := f.bot.db.mint(bg, chatID, r.ID, "allow", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	_ = f.bot.db.bind(bg, []string{tok}, chatID, s.ID)
	f.bot.Handle(bg, press(chatID, chatID, s.ID, tok))
	if f.decision(r.ID) != nil || !strings.Contains(f.mock.lastAnswer(t).Text, "turned off") {
		t.Fatal("decision with decisions off")
	}
	// Replies are refused too.
	sess, _ := f.core.Session(bg, "s_1")
	_ = f.bot.Send(bg, core.Notification{Kind: core.NotifyWaiting, Session: &sess, Remote: true})
	fin := f.mock.lastSent(t)
	f.bot.Handle(bg, replyTo(chatID, fin.ID, "keep going"))
	if len(f.core.replies) != 0 || !strings.Contains(f.mock.lastSent(t).Text, "turned off") {
		t.Fatalf("reply with decisions off: %v", f.core.replies)
	}
}

func TestEditOnClose(t *testing.T) {
	f := newFixture(t)
	r := f.open(permissionReq("r_close00001", "npm publish"))
	s := f.notifyRequest(r)

	// Answered on a phone: the message says so and loses its buttons.
	closed, err := f.core.Decide(bg, api.Decision{RequestID: r.ID, Behavior: api.BehaviorDeny, Message: "not today", By: "device:d_phone"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.bot.Withdraw(bg, closed); err != nil {
		t.Fatal(err)
	}
	e := f.mock.lastEdit(t)
	if !strings.Contains(e.Text, "✗ Denied from iPhone") || !strings.Contains(e.Text, "“not today”") || len(parseButtons(e.Markup)) != 0 {
		t.Fatalf("edit = %q %s", e.Text, e.Markup)
	}
	// Its buttons are dead, and a second withdrawal edits nothing.
	f.bot.Handle(bg, press(chatID, chatID, s.ID, button(t, s.buttons(), "✅ Allow")))
	if a := f.mock.lastAnswer(t); a.Text != "This button was already used." {
		t.Fatalf("answer = %+v", a)
	}
	n := f.mock.editCount()
	_ = f.bot.Withdraw(bg, closed)
	if f.mock.editCount() != n {
		t.Fatal("closed message edited twice")
	}

	// Timed out: one edit, and the follow-up "blocked" notice sends nothing new.
	r2 := f.open(permissionReq("r_close00002", "terraform apply"))
	f.notifyRequest(r2)
	sentBefore := f.mock.sentCount()
	r2.Status = api.RequestExpired
	_ = f.bot.Send(bg, core.Notification{Kind: core.NotifyBlocked, Request: &r2, Remote: true})
	_ = f.bot.Withdraw(bg, r2)
	if f.mock.sentCount() != sentBefore || !strings.Contains(f.mock.lastEdit(t).Text, "⏱ Timed out — answer in the terminal") {
		t.Fatalf("expired: sent %d→%d, edit %q", sentBefore, f.mock.sentCount(), f.mock.lastEdit(t).Text)
	}
	// A blocked notice for a request never sent here is a plain message.
	r3 := permissionReq("r_close00003", "make")
	r3.Agent = api.AgentCodex
	_ = f.bot.Send(bg, core.Notification{Kind: core.NotifyBlocked, Request: &r3, Remote: true})
	if s := f.mock.lastSent(t); !strings.Contains(s.Text, "waiting in the terminal") || len(s.buttons()) != 0 {
		t.Fatalf("blocked = %+v", s)
	}

	// Pressing a button of a request someone else answered first edits it.
	r4 := f.open(permissionReq("r_close00004", "ls"))
	s4 := f.notifyRequest(r4)
	if _, err := f.core.Decide(bg, api.Decision{RequestID: r4.ID, Behavior: api.BehaviorAllow, By: "cli"}); err != nil {
		t.Fatal(err)
	}
	f.bot.Handle(bg, press(chatID, chatID, s4.ID, button(t, s4.buttons(), "❌ Deny")))
	if a := f.mock.lastAnswer(t); !strings.Contains(a.Text, "✓ Allowed from the CLI") {
		t.Fatalf("answer = %+v", a)
	}
	if e := f.mock.lastEdit(t); e.MessageID != s4.ID || !strings.Contains(e.Text, "from the CLI") {
		t.Fatalf("edit = %+v", e)
	}
}

func TestQuestionMultiSelect(t *testing.T) {
	f := newFixture(t)
	r := f.open(api.Request{ID: "r_quest00001", Checkpoint: api.CheckpointQuestion, Tool: "AskUserQuestion", Cwd: "/src/pilothouse",
		Questions: []api.Question{
			{Header: "Checks", Text: "Which checks should run?", MultiSelect: true,
				Options: []api.QuestionOption{{Label: "Lint", Description: "eslint"}, {Label: "Unit"}, {Label: "E2E <slow>"}}},
			{Text: "Ship it?", Options: []api.QuestionOption{{Label: "Yes"}, {Label: "No"}}},
		}})
	s := f.notifyRequest(r)
	if !strings.Contains(s.Text, "asks 2 questions") || !strings.Contains(s.Text, "E2E &lt;slow&gt;") || !strings.Contains(s.Text, "eslint") {
		t.Fatalf("text = %s", s.Text)
	}
	bs := s.buttons()
	if len(bs) != 5 || bs[0].Text != "☐ Lint" || bs[3].Text != "Done ✓" || bs[4].Text != "Answer in the app" {
		t.Fatalf("buttons = %+v", bs)
	}
	// Done with nothing picked: an alert, and the buttons stay usable.
	f.bot.Handle(bg, press(chatID, chatID, s.ID, button(t, bs, "Done")))
	if a := f.mock.lastAnswer(t); !a.Alert || !strings.Contains(a.Text, "at least one") {
		t.Fatalf("empty done = %+v", a)
	}
	cur := func() []Button { return parseButtons(f.mock.lastEdit(t).Markup) }
	f.bot.Handle(bg, press(chatID, chatID, s.ID, button(t, cur(), "Lint")))
	f.bot.Handle(bg, press(chatID, chatID, s.ID, button(t, cur(), "E2E")))
	f.bot.Handle(bg, press(chatID, chatID, s.ID, button(t, cur(), "Unit")))
	f.bot.Handle(bg, press(chatID, chatID, s.ID, button(t, cur(), "☑ Unit"))) // toggle off again
	if got := cur(); got[0].Text != "☑ Lint" || got[1].Text != "☐ Unit" || got[2].Text != "☑ E2E <slow>" {
		t.Fatalf("toggles = %+v", got)
	}
	f.bot.Handle(bg, press(chatID, chatID, s.ID, button(t, cur(), "Done")))
	if e := f.mock.lastEdit(t); !strings.Contains(e.Text, "→ <b>Lint, E2E &lt;slow&gt;</b>") || !strings.Contains(e.Text, "👉 Ship it?") {
		t.Fatalf("after first question:\n%s", e.Text)
	}
	if f.decision(r.ID) != nil {
		t.Fatal("decided before the last question")
	}
	f.bot.Handle(bg, press(chatID, chatID, s.ID, button(t, cur(), "Yes")))
	d := f.decision(r.ID)
	want := map[string]string{"Which checks should run?": "Lint, E2E <slow>", "Ship it?": "Yes"}
	if d == nil || d.Behavior != api.BehaviorAllow || fmt.Sprint(d.Answers) != fmt.Sprint(want) {
		t.Fatalf("decision = %+v", d)
	}
	if e := f.mock.lastEdit(t); !strings.Contains(e.Text, "✓ Answered from Telegram") || len(parseButtons(e.Markup)) != 0 {
		t.Fatalf("final edit = %s", e.Text)
	}
}

func TestPlanFeedbackViaReply(t *testing.T) {
	f := newFixture(t)
	r := f.open(api.Request{ID: "r_plan000001", Checkpoint: api.CheckpointPlan, Tool: "ExitPlanMode", Cwd: "/src/pilothouse",
		Plan: &api.Plan{Markdown: "## Retry webhooks\n1. Add <backoff>\n2. Test it"}})
	s := f.notifyRequest(r)
	if !strings.Contains(s.Text, "<blockquote expandable>## Retry webhooks\n1. Add &lt;backoff&gt;") {
		t.Fatalf("plan text = %s", s.Text)
	}
	f.bot.Handle(bg, press(chatID, chatID, s.ID, button(t, s.buttons(), "Request changes")))
	prompt := f.mock.lastSent(t)
	if !strings.Contains(string(prompt.Markup), `"force_reply":true`) || prompt.ReplyTo == nil || prompt.ReplyTo.MessageID != s.ID {
		t.Fatalf("prompt = %+v %s", prompt, prompt.Markup)
	}
	// The plan keeps working buttons while the person types.
	if bs := parseButtons(f.mock.lastEdit(t).Markup); len(bs) < 2 || bs[0].CallbackData == button(t, s.buttons(), "Approve") {
		t.Fatalf("buttons not refreshed: %+v", bs)
	}

	f.bot.Handle(bg, replyTo(chatID, prompt.ID, "Use exponential backoff, max 3 tries"))
	d := f.decision(r.ID)
	if d == nil || d.Behavior != api.BehaviorDeny || d.Message != "Use exponential backoff, max 3 tries" || d.By != "telegram:4242" {
		t.Fatalf("decision = %+v", d)
	}
	if e := f.mock.lastEdit(t); e.MessageID != s.ID || !strings.Contains(e.Text, "✎ Changes requested from Telegram") || !strings.Contains(e.Text, "“Use exponential backoff") {
		t.Fatalf("edit = %s", e.Text)
	}
	// A late second reply to the prompt: already answered.
	f.bot.Handle(bg, replyTo(chatID, prompt.ID, "also add logs"))
	if !strings.Contains(f.mock.lastSent(t).Text, "Already answered") {
		t.Fatalf("second feedback = %q", f.mock.lastSent(t).Text)
	}

	// Replying straight to an open plan message works too; Approve approves.
	r2 := f.open(api.Request{ID: "r_plan000002", Checkpoint: api.CheckpointPlan, Plan: &api.Plan{Markdown: "Plan B"}})
	s2 := f.notifyRequest(r2)
	f.bot.Handle(bg, replyTo(chatID, s2.ID, "smaller steps"))
	if d := f.decision(r2.ID); d == nil || d.Behavior != api.BehaviorDeny || d.Message != "smaller steps" {
		t.Fatalf("direct reply = %+v", d)
	}
	r3 := f.open(api.Request{ID: "r_plan000003", Checkpoint: api.CheckpointPlan, Plan: &api.Plan{Markdown: "Plan C"}})
	s3 := f.notifyRequest(r3)
	f.bot.Handle(bg, press(chatID, chatID, s3.ID, button(t, s3.buttons(), "Approve")))
	if d := f.decision(r3.ID); d == nil || d.Behavior != api.BehaviorAllow {
		t.Fatalf("approve = %+v", d)
	}
}

func TestReplyToFinished(t *testing.T) {
	f := newFixture(t)
	f.core.PutSession(api.Session{ID: "s_done", Agent: api.AgentClaude, Title: "site", State: api.StateWaiting,
		LastMessage: "Done. Want <pricing>?", Caps: api.SessionCaps{Reply: true}})
	sess, _ := f.core.Session(bg, "s_done")
	if err := f.bot.Send(bg, core.Notification{Kind: core.NotifyWaiting, Session: &sess, Remote: true}); err != nil {
		t.Fatal(err)
	}
	fin := f.mock.lastSent(t)
	if !strings.Contains(fin.Text, "Want &lt;pricing&gt;?") || !strings.Contains(fin.Text, "Reply to this message") {
		t.Fatalf("finished = %s", fin.Text)
	}
	f.bot.Handle(bg, replyTo(chatID, fin.ID, "Yes, add the pricing page"))
	if len(f.core.replies) != 1 || f.core.replies[0] != "s_done|Yes, add the pricing page|telegram:4242" {
		t.Fatalf("replies = %v", f.core.replies)
	}
	if s := f.mock.lastSent(t); !strings.Contains(s.Text, "✓ Sent — typed into the terminal") || s.ReplyTo == nil {
		t.Fatalf("confirmation = %+v", s)
	}

	// Unsupported (workstream B not merged, or an observed Codex session): the reason is shown.
	f.core.err = fmt.Errorf("%w: observed Codex sessions can't receive replies", core.ErrUnsupported)
	f.bot.Handle(bg, replyTo(chatID, fin.ID, "again"))
	if s := f.mock.lastSent(t); !strings.Contains(s.Text, "Couldn't send this: observed Codex sessions can't receive replies") {
		t.Fatalf("unsupported = %q", s.Text)
	}
	f.core.err = core.ErrUnsupported
	f.bot.Handle(bg, replyTo(chatID, fin.ID, "again"))
	if s := f.mock.lastSent(t); !strings.Contains(s.Text, "can't receive replies yet") {
		t.Fatalf("bare unsupported = %q", s.Text)
	}
	f.core.err = nil

	// A reply that arrives late (queued while the daemon was down) is not delivered.
	late := replyTo(chatID, fin.ID, "stale instruction")
	late.Message.Date = time.Now().Add(-time.Hour).Unix()
	f.bot.Handle(bg, late)
	if len(f.core.replies) != 1 || !strings.Contains(f.mock.lastSent(t).Text, "late") {
		t.Fatalf("late reply delivered: %v", f.core.replies)
	}
	// Replies to unknown messages, or to an open permission request, explain.
	f.bot.Handle(bg, replyTo(chatID, 9999, "hi"))
	if !strings.Contains(f.mock.lastSent(t).Text, "isn't one Pilothouse can route") {
		t.Fatal(f.mock.lastSent(t).Text)
	}
	r := f.open(permissionReq("r_rep0000001", "ls"))
	s := f.notifyRequest(r)
	f.bot.Handle(bg, replyTo(chatID, s.ID, "no"))
	if !strings.Contains(f.mock.lastSent(t).Text, "Use the buttons") || f.decision(r.ID) != nil {
		t.Fatal(f.mock.lastSent(t).Text)
	}

	// A core without replies at all.
	g := newFixture(t, func(o *Options) { o.Core = webtest.New() })
	g.core.FakeCore = nil
	sess2 := api.Session{ID: "s_x", Agent: api.AgentCodex}
	_ = g.bot.Send(bg, core.Notification{Kind: core.NotifyWaiting, Session: &sess2, Remote: true})
	g.bot.Handle(bg, replyTo(chatID, g.mock.lastSent(t).ID, "hello"))
	if !strings.Contains(g.mock.lastSent(t).Text, "aren't available") {
		t.Fatal(g.mock.lastSent(t).Text)
	}
}

func TestStatusAndHelp(t *testing.T) {
	f := newFixture(t)
	f.open(permissionReq("r_stat000001", "git push"))
	r2 := permissionReq("r_stat000002", "npm test")
	r2.Route = api.RouteTerminal
	r2.SessionID, r2.Status, r2.Agent = "s_1", api.RequestOpen, api.AgentCodex
	f.core.PutRequest(r2)
	before := f.mock.sentCount()
	f.bot.Handle(bg, text(chatID, chatID, "/status"))
	f.mock.mu.Lock()
	msgs := f.mock.sent[before:]
	f.mock.mu.Unlock()
	if len(msgs) != 2 {
		t.Fatalf("want the status and one resent request, got %d", len(msgs))
	}
	st := msgs[0].Text
	if !strings.Contains(st, "2 requests are waiting") || !strings.Contains(st, "Claude Code wants to run `git push`") ||
		!strings.Contains(st, "Codex wants to run `npm test` · pilothouse (main) (in the terminal)") || !strings.Contains(st, "Sessions: 1 blocked") {
		t.Fatalf("status = %s", st)
	}
	if len(msgs[1].buttons()) == 0 {
		t.Fatal("held request resent without buttons")
	}
	// A second /status doesn't resend it.
	f.bot.Handle(bg, text(chatID, chatID, "/status"))
	if f.mock.sentCount() != before+3 {
		t.Fatalf("sent = %d", f.mock.sentCount()-before)
	}
	f.bot.Handle(bg, text(chatID, chatID, "what now?"))
	if !strings.Contains(f.mock.lastSent(t).Text, "/status") {
		t.Fatal(f.mock.lastSent(t).Text)
	}
	f.bot.Handle(bg, Update{Message: &Message{MessageID: 5, From: &User{ID: chatID}, Chat: Chat{ID: chatID, Type: "private"}}}) // a sticker: no text
}

func TestRunPollsWithBackoffAndOffset(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.ChatID = 0 })
	code := f.keys.NewPairCode(time.Now())
	f.mock.fail["getUpdates"] = []int{502, 502}
	f.mock.push(text(chatID, chatID, "/start "+code))
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { f.bot.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	// Wait for the offset too: Chat() is set inside Handle before Run saves
	// it, so cancelling as soon as pairing succeeds can race the save.
	for (f.bot.Chat() == 0 || f.bot.db.offset(bg) != 2) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop with its context")
	}
	if f.bot.Chat() != chatID {
		t.Fatal("pairing over polling failed")
	}
	if n := f.mock.countCalls("getUpdates"); n < 3 {
		t.Fatalf("getUpdates calls = %d: failures must be retried", n)
	}
	if off := f.bot.db.offset(bg); off != 2 {
		t.Fatalf("offset = %d, want the next update id persisted", off)
	}
}

func TestClient(t *testing.T) {
	m := newMock(t)
	c := m.client()
	// 429 waits retry_after, then succeeds.
	m.fail["sendMessage"] = []int{429}
	start := time.Now()
	if _, err := c.Send(bg, SendMessage{ChatID: 1, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < 900*time.Millisecond {
		t.Fatalf("429 retried after %s, want Telegram's retry_after (1 s)", el)
	}
	// 5xx: bounded retries, then the error.
	m.fail["sendMessage"] = []int{500, 500, 500, 500, 500}
	if _, err := c.Send(bg, SendMessage{ChatID: 1, Text: "x"}); !IsCode(err, 500) {
		t.Fatalf("err = %v", err)
	}
	// A 429 longer than the caller's deadline gives up instead of blocking.
	m.fail["sendMessage"] = []int{429}
	ctx, cancel := context.WithTimeout(bg, 50*time.Millisecond)
	_, err := c.Send(ctx, SendMessage{ChatID: 1, Text: "x"})
	cancel()
	if err == nil {
		t.Fatal("deadline ignored")
	}
	// Editing to the same content is fine.
	e := EditMessageText{ChatID: 1, MessageID: 5, Text: "same"}
	if err := c.Edit(bg, e); err != nil {
		t.Fatal(err)
	}
	if err := c.Edit(bg, e); err != nil {
		t.Fatalf("not-modified edit: %v", err)
	}
	if u, err := c.GetMe(bg); err != nil || u.Username != "pilot_test_bot" {
		t.Fatalf("getMe = %+v %v", u, err)
	}
	// A bad token is a 401.
	bad := &Client{Token: "1:bad", Base: m.URL, HTTP: m.Client()}
	if _, err := bad.GetMe(bg); !IsCode(err, 401) {
		t.Fatalf("bad token: %v", err)
	}

	// Network errors never show the token (it is part of the URL).
	dead := &Client{Token: testToken, Base: "http://127.0.0.1:1"}
	_, err = dead.GetMe(bg)
	if err == nil || strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), "AAtest") {
		t.Fatalf("network error leaks the token: %v", err)
	}
	var te *Error
	if !errors.As(err, &te) || te.Method != "getMe" {
		t.Fatalf("err = %#v", err)
	}
}

func TestKeysFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "keys", "telegram.json")
	if k, err := LoadKeys(p); err != nil || k.Token != "" {
		t.Fatalf("missing file = %+v %v", k, err)
	}
	k := Keys{Token: testToken, BotUsername: "pilot_test_bot"}
	now := time.Now()
	code := k.NewPairCode(now)
	if err := SaveKeys(p, k); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v %v", fi.Mode(), err)
	}
	raw, _ := os.ReadFile(p)
	if strings.Contains(string(raw), code) {
		t.Fatal("pairing code stored in clear")
	}
	got, _ := LoadKeys(p)
	if !got.CheckCode(code, now) || got.CheckCode(code+"x", now) || got.CheckCode(code, now.Add(PairTTL+time.Second)) || got.CheckCode("short", now) {
		t.Fatal("CheckCode")
	}
	if StartLink("pilot_test_bot", code) != "https://t.me/pilot_test_bot?start="+code {
		t.Fatal(StartLink("pilot_test_bot", code))
	}
	if !ValidToken(testToken) || ValidToken("123:short") || ValidToken("abc:"+strings.Repeat("x", 30)) {
		t.Fatal("ValidToken")
	}
	if c, ok := startCode("/start@pilot_test_bot abc"); !ok || c != "abc" {
		t.Fatal("startCode with bot name")
	}

	// SpendCode: once only, never a newer code, never an expired one.
	if err := SpendCode(p, code, now.Add(PairTTL+time.Second)); !errors.Is(err, ErrCodeChanged) {
		t.Fatalf("expired code spent: %v", err)
	}
	if err := SpendCode(p, code, now); err != nil {
		t.Fatal(err)
	}
	if err := SpendCode(p, code, now); !errors.Is(err, ErrCodeChanged) {
		t.Fatalf("code spent twice: %v", err)
	}
	newer, err := UpdateKeys(p, func(k *Keys) error { k.NewPairCode(now); return nil })
	if err != nil || newer.Token != testToken {
		t.Fatalf("update = %+v %v", newer.BotUsername, err)
	}
	if err := SpendCode(p, code, now); !errors.Is(err, ErrCodeChanged) {
		t.Fatal("an old code cleared the newer one")
	}
	if k, _ := LoadKeys(p); k.PairHash != newer.PairHash {
		t.Fatal("newer code lost")
	}
	if err := RemoveKeys(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("not removed")
	}
	if err := RemoveKeys(filepath.Join(t.TempDir(), "none", "telegram.json")); err != nil {
		t.Fatalf("remove missing: %v", err)
	}
}

// The CLI rewriting the key file while the daemon spends a code never
// loses the CLI's new token (both are locked read-modify-writes).
func TestKeysConcurrentUpdates(t *testing.T) {
	p := filepath.Join(t.TempDir(), "keys", "telegram.json")
	if err := SaveKeys(p, Keys{Token: testToken}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := UpdateKeys(p, func(k *Keys) error {
				k.BotID++
				time.Sleep(time.Millisecond)
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if k, _ := LoadKeys(p); k.BotID != 16 || k.Token != testToken {
		t.Fatalf("updates lost: bot_id=%d", k.BotID)
	}
}

func TestWaitForStart(t *testing.T) {
	m := newMock(t)
	var k Keys
	code := k.NewPairCode(time.Now())
	m.push(text(77, 77, "/start nope-nope-nope-nope"))
	m.push(text(chatID, chatID, "/start "+code))
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	c, err := WaitForStart(ctx, m.client(), k, nil)
	if err != nil || c.ID != chatID {
		t.Fatalf("chat = %+v %v", c, err)
	}
	m.mu.Lock()
	last := m.confirms[len(m.confirms)-1]
	m.mu.Unlock()
	if last != 3 {
		t.Fatalf("updates not confirmed: last offset %d", last)
	}
}
