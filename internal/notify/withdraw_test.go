package notify

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
)

// withdrawChannel is a remote channel that records withdrawals.
type withdrawChannel struct {
	fakeChannel
	wmu       sync.Mutex
	withdrawn []api.Request
}

func (c *withdrawChannel) Withdraw(_ context.Context, r api.Request) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.withdrawn = append(c.withdrawn, r)
	return nil
}

func (c *withdrawChannel) ids() []string {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	var out []string
	for _, r := range c.withdrawn {
		out = append(out, r.ID)
	}
	return out
}

func closedReq(id string, st api.RequestStatus) api.Request {
	r := *sampleRequest()
	r.ID, r.Status = id, st
	return r
}

func TestWithdrawOnlyWhatWasSentRemotely(t *testing.T) {
	isolate(t)
	desk := &fakeChannel{name: "desktop"}
	tg := &withdrawChannel{fakeChannel: fakeChannel{name: "telegram", remote: true}}
	hook := &fakeChannel{name: "webhook", remote: true} // can't withdraw
	n := NewWith(nil, time.Second, desk, tg, hook)
	ctx := context.Background()

	sent := sampleRequest()
	sent.ID = "r_sent"
	_ = n.Notify(ctx, core.Notification{Kind: core.NotifyRequest, Request: sent, Remote: true})
	local := sampleRequest()
	local.ID = "r_local"
	_ = n.Notify(ctx, core.Notification{Kind: core.NotifyBlocked, Request: local, Remote: false})

	_ = n.Withdraw(ctx, closedReq("r_local", api.RequestAnsweredLocally)) // only on the desktop
	_ = n.Withdraw(ctx, closedReq("r_never", api.RequestDecided))         // never notified
	_ = n.Withdraw(ctx, closedReq("r_sent", api.RequestOpen))             // still open: ignored
	_ = n.Withdraw(ctx, closedReq("r_sent", api.RequestDecided))
	_ = n.Withdraw(ctx, closedReq("r_sent", api.RequestDecided)) // once only
	n.Close()

	if got := strings.Join(tg.ids(), ","); got != "r_sent" {
		t.Fatalf("withdrawn = %q, want r_sent once", got)
	}
	if desk.count() != 2 || hook.count() != 1 {
		t.Fatalf("withdrawals must not reach Send: desktop=%d webhook=%d", desk.count(), hook.count())
	}
	if err := n.Withdraw(ctx, closedReq("r_x", api.RequestDecided)); err != nil {
		t.Fatalf("withdraw of an unknown request after close: %v", err)
	}
}

func TestRichBodies(t *testing.T) {
	r := sampleRequest()
	r.Risk = []api.RiskLabel{{Code: api.RiskProtectedBranch, Level: api.RiskHigh, Text: "Pushes to main"}, {Code: api.RiskNetwork, Level: api.RiskMedium}}
	r.Context = "Tests pass,\nso I'll push the fix now."
	got := Render(core.Notification{Kind: core.NotifyRequest, Request: r, Session: sampleSession()})
	want := "Wants to run `git push origin main`\n⚠ Pushes to main · network\n“Tests pass, so I'll push the fix now.”"
	if got.Body != want {
		t.Fatalf("body = %q\nwant  %q", got.Body, want)
	}

	q := &api.Request{ID: "r_q", Agent: api.AgentClaude, Checkpoint: api.CheckpointQuestion, Cwd: "/src/app",
		Questions: []api.Question{{Text: "Which auth method?", Options: []api.QuestionOption{{Label: "OAuth"}, {Label: "API key"}}},
			{Text: "Add tests?", MultiSelect: true}}}
	got = Render(core.Notification{Kind: core.NotifyRequest, Request: q})
	if got.Body != "Asks: Which auth method? (+1 more)\nOptions: OAuth · API key" {
		t.Fatalf("question body = %q", got.Body)
	}
	q.Questions[0].MultiSelect = true
	if d := Details(q); len(d) != 1 || d[0] != "Pick any: OAuth · API key" {
		t.Fatalf("multi-select = %q", d)
	}

	p := &api.Request{ID: "r_p", Agent: api.AgentClaude, Checkpoint: api.CheckpointPlan,
		Plan: &api.Plan{Markdown: "\n## Plan: retry webhooks\n\n1. Add backoff"}, Preview: &api.Preview{Files: make([]api.FilePreview, 3), Added: 12, Removed: 4}}
	got = Render(core.Notification{Kind: core.NotifyRequest, Request: p})
	if got.Body != "Wants a plan approved: Plan: retry webhooks\n✎ 3 files, +12 −4" {
		t.Fatalf("plan body = %q", got.Body)
	}
	if Line(core.Notification{Kind: core.NotifyRequest, Request: p}) != "Claude Code wants a plan approved: Plan: retry webhooks" {
		t.Fatal(Line(core.Notification{Kind: core.NotifyRequest, Request: p}))
	}
}

func TestResolvedLines(t *testing.T) {
	names := func(id string) string { return map[string]string{"d_1": "iPhone"}[id] }
	r := *sampleRequest()
	for _, c := range []struct {
		st   api.RequestStatus
		cp   api.Checkpoint
		dec  *api.Decision
		want string
	}{
		{api.RequestDecided, "", &api.Decision{Behavior: api.BehaviorAllow, By: "device:d_1"}, "✓ Allowed from iPhone"},
		{api.RequestDecided, "", &api.Decision{Behavior: api.BehaviorAllow, Scope: api.ScopeSession, By: "telegram:42"}, "✓ Allowed for this session from Telegram"},
		{api.RequestDecided, "", &api.Decision{Behavior: api.BehaviorDeny, By: "cli"}, "✗ Denied from the CLI"},
		{api.RequestDecided, "", &api.Decision{Behavior: api.BehaviorDeny, By: "device:d_gone"}, "✗ Denied from a paired device"},
		{api.RequestDecided, api.CheckpointPlan, &api.Decision{Behavior: api.BehaviorDeny, By: "tui"}, "✎ Changes requested from the inbox"},
		{api.RequestDecided, api.CheckpointPlan, &api.Decision{Behavior: api.BehaviorAllow, By: "ntfy"}, "✓ Plan approved from ntfy"},
		{api.RequestDecided, api.CheckpointQuestion, &api.Decision{Behavior: api.BehaviorAllow, By: "policy:auto"}, "✓ Answered by rule auto"},
		{api.RequestAnsweredLocally, "", nil, "✓ Answered in the terminal"},
		{api.RequestSteppedAside, "", nil, "↩ Moved to the terminal"},
		{api.RequestExpired, "", nil, "⏱ Timed out — answer in the terminal"},
		{api.RequestOpen, "", nil, ""},
	} {
		r.Status, r.Checkpoint, r.Decision = c.st, c.cp, c.dec
		if got := ResolvedLine(r, names); got != c.want {
			t.Errorf("%s/%s/%+v: %q, want %q", c.st, c.cp, c.dec, got, c.want)
		}
	}
}

// ntfy: request notifications carry a sequence ID; withdrawing clears it
// (PUT …/clear) unless it timed out, and an old server's 404 is fine.
func TestNtfyWithdraw(t *testing.T) {
	isolate(t)
	ts, got := capture(t, 200)
	c := &Ntfy{Server: ts.URL, Topic: "phone-topic", Token: "tk_x", Client: ts.Client()}
	ctx := context.Background()
	r := sampleRequest()
	if err := c.Send(ctx, Render(core.Notification{Kind: core.NotifyRequest, Request: r, Remote: true})); err != nil {
		t.Fatal(err)
	}
	if h := got()[0].header; h.Get("X-Sequence-ID") != "req-r_abc" {
		t.Fatalf("sequence id = %q", h.Get("X-Sequence-ID"))
	}
	if err := c.Withdraw(ctx, closedReq("r_abc", api.RequestDecided)); err != nil {
		t.Fatal(err)
	}
	clear := got()[1]
	if clear.path != "/phone-topic/req-r_abc/clear" || clear.header.Get("Authorization") != "Bearer tk_x" {
		t.Fatalf("clear = %s %v", clear.path, clear.header)
	}
	// Once only, and never for requests it didn't publish.
	_ = c.Withdraw(ctx, closedReq("r_abc", api.RequestDecided))
	_ = c.Withdraw(ctx, closedReq("r_other", api.RequestDecided))
	if n := len(got()); n != 2 {
		t.Fatalf("requests = %d", n)
	}
	// A timeout keeps the "waiting in the terminal" notice.
	_ = c.Send(ctx, Render(core.Notification{Kind: core.NotifyBlocked, Request: r, Remote: true}))
	_ = c.Withdraw(ctx, closedReq("r_abc", api.RequestExpired))
	if n := len(got()); n != 3 {
		t.Fatalf("expired request cleared: %d requests", n)
	}

	old, _ := capture(t, 404)
	oc := &Ntfy{Server: old.URL, Topic: "t", Client: old.Client()}
	oc.sent = map[string]time.Time{"r_abc": time.Now()}
	if err := oc.Withdraw(ctx, closedReq("r_abc", api.RequestDecided)); err != nil {
		t.Fatalf("404 from a pre-2.16 server must not be an error: %v", err)
	}
}
