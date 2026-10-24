package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/opencode"
	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/paths"
)

// pluginHarness is a daemon with the OpenCode adapter and a clock tests
// can move forward (channel liveness).
type pluginHarness struct {
	*harness
	offset atomic.Int64
	http   *http.Client
}

func newPluginHarness(t *testing.T) *pluginHarness {
	t.Helper()
	ph := &pluginHarness{harness: newHarness(t)}
	ph.opts = func(o *Options) {
		o.Adapters = append(o.Adapters, opencode.New())
		o.Now = func() time.Time { return time.Now().Add(time.Duration(ph.offset.Load())) }
	}
	ph.http = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", paths.Socket())
		},
	}}
	old := pluginAckWait
	pluginAckWait = 2 * time.Second
	t.Cleanup(func() { pluginAckWait = old })
	return ph
}

func (ph *pluginHarness) advance(d time.Duration) { ph.offset.Add(int64(d)) }

func newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (ph *pluginHarness) post(t *testing.T, path, token string, body any) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if s, ok := body.(string); ok {
		buf.WriteString(s)
	} else if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", "http://pilothouse"+path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set(PluginTokenHeader, token)
	}
	resp, err := ph.http.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// pluginAnswer is what the plugin reads from a hook response.
type pluginAnswer struct {
	Action  string     `json:"action"`
	Message string     `json:"message"`
	Rule    string     `json:"rule"`
	Answers [][]string `json:"answers"`
}

// hook sends a plugin hook for OpenCode session ses as token's process.
func (ph *pluginHarness) hook(t *testing.T, token, ses, event string, data map[string]any) pluginAnswer {
	t.Helper()
	p := map[string]any{"v": 1, "event": event, "session_id": ses, "agent_session_id": ses, "directory": "/tmp/repo"}
	for k, v := range data {
		p[k] = v
	}
	payload, _ := json.Marshal(p)
	env := adapters.HookEnvelope{Agent: api.AgentOpenCode, Event: event, Payload: payload, ShimPID: 7000,
		Ancestors: []adapters.Proc{{PID: 7000, PPID: 1, Comm: "opencode"}}, Cwd: "/tmp/repo", ReceivedAt: time.Now()}
	status, body := ph.post(t, "/v1/plugin/hook", token, env)
	if status != 200 {
		t.Fatalf("hook %s: %d %s", event, status, body)
	}
	var resp adapters.HookResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	var a pluginAnswer
	if resp.Stdout != "" {
		if err := json.Unmarshal([]byte(resp.Stdout), &a); err != nil {
			t.Fatalf("bad answer %q", resp.Stdout)
		}
	}
	return a
}

func bash(cmd, callID string) map[string]any {
	return map[string]any{"tool": "bash", "call_id": callID, "args": map[string]any{"command": cmd}}
}

// pollResult is one command poll.
type pollResult struct {
	status int
	cmds   []pluginCommand
	body   string
}

// poll starts a long poll as token's process and returns its result channel.
func (ph *pluginHarness) poll(t *testing.T, token string, sessions []string, wait int) <-chan pollResult {
	out := make(chan pollResult, 1)
	go func() {
		status, body := ph.post(t, "/v1/plugin/commands", token, map[string]any{"sessions": sessions, "wait": wait})
		var r struct {
			Commands []pluginCommand `json:"commands"`
		}
		_ = json.Unmarshal(body, &r)
		out <- pollResult{status, r.Commands, string(body)}
	}()
	return out
}

func (ph *pluginHarness) session(t *testing.T, agentSession string) api.Session {
	t.Helper()
	var s api.Session
	waitFor(t, "session "+agentSession, func() bool {
		ss, err := ph.c.Sessions(bg)
		if err != nil {
			return false
		}
		for _, x := range ss {
			if x.AgentSessionID == agentSession {
				s = x
				return true
			}
		}
		return false
	})
	return s
}

func (ph *pluginHarness) caps(t *testing.T, id string) api.SessionCaps {
	t.Helper()
	s, err := ph.c.Session(bg, id)
	if err != nil {
		t.Fatal(err)
	}
	return s.Caps
}

func TestPluginPolicyDeny(t *testing.T) {
	ph := newPluginHarness(t)
	ph.policy.set("rm -rf /", api.BehaviorDeny, "no-rm")
	ph.start(t)
	tok := newToken()
	a := ph.hook(t, tok, "ses_a", "tool.execute.before", bash("rm -rf /", "call_1"))
	if a.Action != "deny" || a.Rule != "no-rm" || a.Message == "" {
		t.Fatalf("deny answer %+v", a)
	}
	if a := ph.hook(t, tok, "ses_a", "tool.execute.before", bash("ls", "call_2")); a.Action != "" {
		t.Fatalf("no rule: %+v", a)
	}
	s := ph.session(t, "ses_a")
	if s.Agent != api.AgentOpenCode || s.PID != 7000 {
		t.Fatalf("session %+v", s)
	}
}

// TestPluginAskHeldAtDesk: OpenCode has no prompt a plugin can raise, so
// a policy ask is held for a person even at the desk, and a session grant
// can't answer it.
func TestPluginAskHeldAtDesk(t *testing.T) {
	ph := newPluginHarness(t)
	ph.policy.set("git push", api.BehaviorAsk, "ask-push")
	ph.start(t)
	tok := newToken()
	if a := ph.hook(t, tok, "ses_a", "tool.execute.before", bash("git push", "call_1")); a.Action != "ask" || a.Rule != "ask-push" {
		t.Fatalf("pre_tool ask: %+v", a)
	}
	// An earlier "allow for this session" for bash must not answer it.
	done := make(chan pluginAnswer, 1)
	go func() {
		done <- ph.hook(t, tok, "ses_a", "pilothouse.ask", map[string]any{"tool": "bash", "call_id": "call_0", "args": map[string]any{"command": "ls"}})
	}()
	first := ph.openRequest(t)
	if _, err := ph.c.Decide(bg, first.ID, api.DecideBody{Behavior: api.BehaviorAllow, Scope: api.ScopeSession}); err != nil {
		t.Fatal(err)
	}
	if a := <-done; a.Action != "allow" {
		t.Fatalf("first ask: %+v", a)
	}

	go func() { done <- ph.hook(t, tok, "ses_a", "pilothouse.ask", bash("git push", "call_1")) }()
	req := ph.openRequest(t)
	if req.Route != api.RouteDevices || req.Status != api.RequestOpen || req.Summary != "Run: git push" {
		t.Fatalf("held ask = %+v (presence %s)", req, ph.d.Presence().State)
	}
	if _, err := ph.c.Decide(bg, req.ID, api.DecideBody{Behavior: api.BehaviorDeny, Message: "not today"}); err != nil {
		t.Fatal(err)
	}
	if a := <-done; a.Action != "deny" || a.Message != "not today" {
		t.Fatalf("decided ask: %+v", a)
	}
}

func TestPluginPermissionStepsAsideAtDesk(t *testing.T) {
	ph := newPluginHarness(t)
	ph.start(t)
	tok := newToken()
	perm := map[string]any{"request_id": "per_1", "tool": "bash", "call_id": "call_1", "args": map[string]any{"command": "make deploy"},
		"permission": map[string]any{"permission": "bash", "patterns": []string{"make deploy"}}}
	if a := ph.hook(t, tok, "ses_a", "permission.asked", perm); a.Action != "" {
		t.Fatalf("at the desk OpenCode's own dialog answers: %+v", a)
	}
	rs, _ := ph.c.Requests(bg, api.RequestSteppedAside)
	if len(rs) != 1 || rs[0].Summary != "Run: make deploy" || rs[0].Agent != api.AgentOpenCode {
		t.Fatalf("stepped aside = %+v", rs)
	}
	// The person answers in OpenCode.
	ph.hook(t, tok, "ses_a", "permission.replied", map[string]any{"request_id": "per_1", "reply": "once"})
	r, err := ph.c.Request(bg, rs[0].ID)
	if err != nil || r.Status != api.RequestAnsweredLocally {
		t.Fatalf("after reply: %+v %v", r, err)
	}
}

func TestPluginPermissionHeldAway(t *testing.T) {
	ph := newPluginHarness(t)
	ph.presence.setAway(true)
	ph.start(t)
	waitFor(t, "away", func() bool { return ph.d.Presence().State == api.PresenceAway })
	tok := newToken()
	perm := func(id string) map[string]any {
		return map[string]any{"request_id": id, "tool": "bash", "call_id": "c-" + id, "args": map[string]any{"command": "make " + id},
			"permission": map[string]any{"permission": "bash"}}
	}
	done := make(chan pluginAnswer, 1)
	go func() { done <- ph.hook(t, tok, "ses_a", "permission.asked", perm("per_1")) }()
	req := ph.openRequest(t)
	if _, err := ph.c.Decide(bg, req.ID, api.DecideBody{Behavior: api.BehaviorAllow}); err != nil {
		t.Fatal(err)
	}
	if a := <-done; a.Action != "allow" {
		t.Fatalf("allowed from a device: %+v", a)
	}
	// Answered in OpenCode while held: the hold ends with no opinion.
	go func() { done <- ph.hook(t, tok, "ses_a", "permission.asked", perm("per_2")) }()
	req = ph.openRequest(t)
	ph.hook(t, tok, "ses_a", "permission.replied", map[string]any{"request_id": "per_2", "reply": "reject"})
	if a := <-done; a.Action != "" {
		t.Fatalf("answered locally: %+v", a)
	}
	if r, _ := ph.c.Request(bg, req.ID); r.Status != api.RequestAnsweredLocally {
		t.Fatalf("status %s", r.Status)
	}
	// A reply for another (already decided) request closes nothing.
	go func() { done <- ph.hook(t, tok, "ses_a", "permission.asked", perm("per_3")) }()
	req = ph.openRequest(t)
	ph.hook(t, tok, "ses_a", "permission.replied", map[string]any{"request_id": "per_1", "reply": "once"})
	if r, _ := ph.c.Request(bg, req.ID); r.Status != api.RequestOpen {
		t.Fatalf("an unrelated reply closed the held request: %s", r.Status)
	}
	if _, err := ph.c.Decide(bg, req.ID, api.DecideBody{Behavior: api.BehaviorDeny}); err != nil {
		t.Fatal(err)
	}
	if a := <-done; a.Action != "deny" || a.Message == "" {
		t.Fatalf("denied: %+v", a)
	}
}

func TestPluginQuestionAnsweredRemotely(t *testing.T) {
	ph := newPluginHarness(t)
	ph.presence.setAway(true)
	ph.start(t)
	waitFor(t, "away", func() bool { return ph.d.Presence().State == api.PresenceAway })
	tok := newToken()
	done := make(chan pluginAnswer, 1)
	go func() {
		done <- ph.hook(t, tok, "ses_a", "question.asked", map[string]any{"request_id": "que_1", "questions": []map[string]any{
			{"question": "Which color?", "header": "Color", "options": []map[string]string{{"label": "Red"}, {"label": "Blue"}}},
			{"question": "Which sizes?", "multiple": true, "options": []map[string]string{{"label": "S"}, {"label": "M"}, {"label": "L"}}},
		}})
	}()
	req := ph.openRequest(t)
	if req.Checkpoint != api.CheckpointQuestion || len(req.Questions) != 2 || req.Questions[0].Key() != "que_1#1" {
		t.Fatalf("question request %+v", req)
	}
	if _, err := ph.c.Decide(bg, req.ID, api.DecideBody{Behavior: api.BehaviorAllow,
		Answers: map[string]string{"que_1#1": "Blue", "que_1#2": api.JoinAnswers([]string{"S", "L"})}}); err != nil {
		t.Fatal(err)
	}
	a := <-done
	if a.Action != "answer" || len(a.Answers) != 2 || strings.Join(a.Answers[0], ",") != "Blue" || strings.Join(a.Answers[1], ",") != "S,L" {
		t.Fatalf("answers %+v", a)
	}
}

func TestPluginReplyDelivered(t *testing.T) {
	ph := newPluginHarness(t)
	ph.start(t)
	tok := newToken()
	ph.hook(t, tok, "ses_a", "session.created", nil)
	s := ph.session(t, "ses_a")
	if s.Caps.Reply {
		t.Fatal("reply possible before the plugin polls")
	}
	if _, err := ph.d.Reply(bg, s.ID, api.Reply{Text: "hi"}, "cli"); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("reply without a channel: %v", err)
	}
	polled := ph.poll(t, tok, []string{"ses_a"}, 5)
	waitFor(t, "caps.reply", func() bool { c := ph.caps(t, s.ID); return c.Reply && c.ReplyVia == api.ReplyPlugin && c.Interrupt })

	type result struct {
		res api.ReplyResult
		err error
	}
	replied := make(chan result, 1)
	go func() {
		res, err := ph.c.Reply(bg, s.ID, api.Reply{Text: "Run the tests again  \n"})
		replied <- result{res, err}
	}()
	p := <-polled
	if p.status != 200 || len(p.cmds) != 1 || p.cmds[0].SessionID != "ses_a" || p.cmds[0].Text != "Run the tests again" {
		t.Fatalf("poll %+v", p)
	}
	if st, b := ph.post(t, "/v1/plugin/commands/"+p.cmds[0].ID+"/ack", tok, pluginAck{OK: true}); st != 204 {
		t.Fatalf("ack %d %s", st, b)
	}
	r := <-replied
	if r.err != nil || r.res.Via != api.ReplyPlugin {
		t.Fatalf("reply %+v %v", r.res, r.err)
	}
	evs, _ := ph.c.SessionEvents(bg, s.ID, 20)
	found := false
	for _, ev := range evs {
		if ev.Type == api.EventSessionReply {
			var rec api.ReplyRecord
			_ = json.Unmarshal(ev.Data, &rec)
			found = rec.Via == api.ReplyPlugin && rec.By == "cli" && strings.HasPrefix(rec.Text, "Run the tests")
		}
		if bytes.Contains(ev.Data, []byte(tok)) {
			t.Fatal("the channel token reached the event log")
		}
	}
	if !found {
		t.Fatal("no session.reply event")
	}

	// The plugin reports OpenCode's refusal.
	polled = ph.poll(t, tok, nil, 5)
	go func() {
		res, err := ph.d.Reply(bg, s.ID, api.Reply{Text: "again"}, "cli")
		replied <- result{res, err}
	}()
	p = <-polled
	ph.post(t, "/v1/plugin/commands/"+p.cmds[0].ID+"/ack", tok, pluginAck{Error: "Session not found"})
	if r := <-replied; !errors.Is(r.err, core.ErrUnsupported) || !strings.Contains(r.err.Error(), "Session not found") {
		t.Fatalf("refused reply: %v", r.err)
	}

	// Taken but never acknowledged: the reply says so instead of hanging.
	polled = ph.poll(t, tok, nil, 5)
	go func() {
		res, err := ph.d.Reply(bg, s.ID, api.Reply{Text: "third"}, "cli")
		replied <- result{res, err}
	}()
	<-polled
	if r := <-replied; r.err == nil || !strings.Contains(r.err.Error(), "did not confirm") {
		t.Fatalf("unconfirmed reply: %v", r.err)
	}
}

// TestPluginChannelIsolation: the routes are open to any local process
// (agents included). A channel only ever sees its own sessions' replies.
func TestPluginChannelIsolation(t *testing.T) {
	ph := newPluginHarness(t)
	ph.start(t)
	owner, thief := newToken(), newToken()
	ph.hook(t, owner, "ses_a", "session.created", nil)
	s := ph.session(t, "ses_a")
	ownerPoll := ph.poll(t, owner, []string{"ses_a"}, 5)
	waitFor(t, "caps.reply", func() bool { return ph.caps(t, s.ID).Reply })

	// No token or a malformed one: refused.
	for _, tok := range []string{"", "short", newToken() + "x", strings.Repeat("!", 43)} {
		if st, _ := ph.post(t, "/v1/plugin/commands", tok, map[string]any{"wait": 1}); st != http.StatusUnauthorized {
			t.Errorf("token %q: status %d", tok, st)
		}
	}
	// A thief polling (and announcing nothing) gets nothing of ses_a's.
	thiefPoll := ph.poll(t, thief, nil, 2)
	sent := make(chan error, 1)
	go func() { _, err := ph.d.Reply(bg, s.ID, api.Reply{Text: "secret plan"}, "cli"); sent <- err }()
	if p := <-ownerPoll; len(p.cmds) != 1 || p.cmds[0].Text != "secret plan" {
		t.Fatalf("owner poll %+v", p)
	} else {
		// The thief can't acknowledge the owner's command either.
		if st, _ := ph.post(t, "/v1/plugin/commands/"+p.cmds[0].ID+"/ack", thief, pluginAck{OK: true}); st != http.StatusNotFound {
			t.Fatalf("thief ack status %d", st)
		}
		ph.post(t, "/v1/plugin/commands/"+p.cmds[0].ID+"/ack", owner, pluginAck{OK: true})
	}
	if err := <-sent; err != nil {
		t.Fatalf("reply to the owner: %v", err)
	}
	if p := <-thiefPoll; p.status != 200 || len(p.cmds) != 0 {
		t.Fatalf("thief poll %+v", p)
	}

	// The thief claims ses_a through a hook and an announce while the owner
	// is live: the session is contested and replies are refused.
	ph.hook(t, thief, "ses_a", "tool.execute.before", bash("ls", "call_9"))
	waitFor(t, "caps.reply off", func() bool { return !ph.caps(t, s.ID).Reply })
	if _, err := ph.d.Reply(bg, s.ID, api.Reply{Text: "x"}, "cli"); !errors.Is(err, errPluginContested) {
		t.Fatalf("contested reply: %v", err)
	}
	// Announcing an unknown session claims nothing.
	<-ph.poll(t, thief, []string{"ses_unknown"}, 1)
	if _, owners := ph.d.plugins.stats(); owners != 1 {
		t.Fatalf("an announce created owners: %d", owners)
	}

	// Once the thief's channel is gone, the owner's next report clears it.
	ph.advance(pluginLive + time.Second)
	ownerPoll = ph.poll(t, owner, []string{"ses_a"}, 1)
	waitFor(t, "caps.reply back", func() bool { return ph.caps(t, s.ID).Reply })
	<-ownerPoll
}

func TestPluginTakeoverAfterOwnerGone(t *testing.T) {
	ph := newPluginHarness(t)
	ph.start(t)
	first, second := newToken(), newToken()
	ph.hook(t, first, "ses_a", "session.created", nil)
	s := ph.session(t, "ses_a")
	<-ph.poll(t, first, []string{"ses_a"}, 1)
	// The first OpenCode quit; the session is resumed in a new process.
	ph.advance(pluginLive + time.Second)
	ph.hook(t, second, "ses_a", "chat.message", map[string]any{"text": "continue"})
	p := ph.poll(t, second, []string{"ses_a"}, 5)
	waitFor(t, "caps.reply", func() bool { return ph.caps(t, s.ID).Reply })
	sent := make(chan error, 1)
	go func() { _, err := ph.d.Reply(bg, s.ID, api.Reply{Text: "go on"}, "cli"); sent <- err }()
	r := <-p
	if len(r.cmds) != 1 || r.cmds[0].Text != "go on" {
		t.Fatalf("new owner poll %+v", r)
	}
	ph.post(t, "/v1/plugin/commands/"+r.cmds[0].ID+"/ack", second, pluginAck{OK: true})
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
}

func TestPluginCapsDropWhenChannelGoes(t *testing.T) {
	old := pluginSweep
	pluginSweep = 20 * time.Millisecond
	t.Cleanup(func() { pluginSweep = old })
	ph := newPluginHarness(t)
	ph.start(t)
	tok := newToken()
	ph.hook(t, tok, "ses_a", "session.created", nil)
	s := ph.session(t, "ses_a")
	<-ph.poll(t, tok, []string{"ses_a"}, 1)
	waitFor(t, "caps.reply", func() bool { return ph.caps(t, s.ID).Reply })
	ph.advance(pluginLive + time.Second)
	waitFor(t, "caps.reply off", func() bool { return !ph.caps(t, s.ID).Reply })
}

func TestPluginHookWithoutTokenBindsNothing(t *testing.T) {
	ph := newPluginHarness(t)
	ph.start(t)
	ph.hook(t, "", "ses_a", "session.created", nil)
	s := ph.session(t, "ses_a")
	<-ph.poll(t, newToken(), nil, 1)
	if _, owners := ph.d.plugins.stats(); owners != 0 || ph.caps(t, s.ID).Reply {
		t.Fatalf("owners %d caps %+v", owners, ph.caps(t, s.ID))
	}
	// Only plugin agents may use the plugin route.
	payload, _ := json.Marshal(adapters.HookEvent{Kind: adapters.HookStop})
	if st, _ := ph.post(t, "/v1/plugin/hook", newToken(), adapters.HookEnvelope{Agent: fakeKind, Payload: payload}); st != http.StatusBadRequest {
		t.Fatalf("fake agent on the plugin route: %d", st)
	}
}

func TestPluginPollEndsOnShutdown(t *testing.T) {
	ph := newPluginHarness(t)
	ph.start(t)
	p := ph.poll(t, newToken(), nil, 30)
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	ph.stop(t)
	select {
	case <-p:
	case <-time.After(3 * time.Second):
		t.Fatal("poll still open after shutdown")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("shutdown took %s with a poll open", time.Since(start))
	}
}
