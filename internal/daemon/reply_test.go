package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/claude"
	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/client"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/paths"
	"github.com/xasdta/pilothouse/internal/store"
	"github.com/xasdta/pilothouse/internal/wrap"
)

// lockedBuf is a log sink tests can read.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// claudeHarness is a daemon with the real Claude adapter and a debug log
// captured in logs.
func claudeHarness(t *testing.T) (*harness, *lockedBuf) {
	t.Helper()
	h := newHarness(t)
	logs := &lockedBuf{}
	h.opts = func(o *Options) {
		o.Adapters = append(o.Adapters, claude.New())
		o.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	h.start(t)
	return h, logs
}

// fakeMessaging is a stand-in for Claude Code's messaging socket: it
// records every NDJSON line it receives.
type fakeMessaging struct {
	sock  string
	ln    net.Listener
	mu    sync.Mutex
	lines []map[string]any
}

func startFakeMessaging(t *testing.T) *fakeMessaging {
	t.Helper()
	f := &fakeMessaging{sock: filepath.Join(paths.Run(), "cc.sock")}
	ln, err := net.Listen("unix", f.sock)
	if err != nil {
		t.Fatal(err)
	}
	f.ln = ln
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					var m map[string]any
					_ = json.Unmarshal(sc.Bytes(), &m)
					f.mu.Lock()
					f.lines = append(f.lines, m)
					f.mu.Unlock()
				}
			}()
		}
	}()
	return f
}

func (f *fakeMessaging) received() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.lines...)
}

// claudeHook sends a Claude Code hook for agent session "cc-1" as the shim
// would, with an optional messaging endpoint.
func claudeHook(t *testing.T, h *harness, event string, ep *adapters.MessagingEndpoint, extra map[string]any) {
	t.Helper()
	p := map[string]any{"session_id": "cc-1", "cwd": os.TempDir(), "hook_event_name": event,
		"transcript_path": "/tmp/none.jsonl"}
	for k, v := range extra {
		p[k] = v
	}
	payload, _ := json.Marshal(p)
	_, err := h.c.Hook(context.Background(), adapters.HookEnvelope{
		Agent: api.AgentClaude, Payload: payload, ShimPID: 5000, Cwd: os.TempDir(),
		Ancestors:  []adapters.Proc{{PID: 4400, PPID: 4343, Comm: "sh"}, {PID: 4343, PPID: 1, Comm: "claude"}},
		Messaging:  ep,
		ReceivedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("hook %s: %v", event, err)
	}
}

func claudeSession(t *testing.T, h *harness) api.Session {
	t.Helper()
	ss, err := h.c.Sessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ss {
		if s.AgentSessionID == "cc-1" {
			return s
		}
	}
	t.Fatalf("no claude session in %+v", ss)
	return api.Session{}
}

func apiErr(t *testing.T, err error) *client.APIError {
	t.Helper()
	var ae *client.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("want an API error, got %v", err)
	}
	return ae
}

// The messaging endpoint is used for replies and appears nowhere else:
// not in events, evidence, explain, the snapshot, API responses or logs.
func TestMessagingReplyAndNoLeaks(t *testing.T) {
	h, logs := claudeHarness(t)
	fm := startFakeMessaging(t)
	const token = "tok-SECRET-4f1e2d"
	ep := &adapters.MessagingEndpoint{Socket: fm.sock, Token: token}
	ctx := context.Background()

	claudeHook(t, h, "SessionStart", ep, map[string]any{"source": "startup"})
	s := claudeSession(t, h)
	if !s.Caps.Reply || s.Caps.ReplyVia != api.ReplyMessage || s.Caps.Interrupt || s.Caps.Watch {
		t.Fatalf("caps = %+v", s.Caps)
	}
	claudeHook(t, h, "PreToolUse", ep, map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "ls"}, "tool_use_id": "tu1"})
	claudeHook(t, h, "Stop", ep, map[string]any{"stop_hook_active": false, "last_assistant_message": "done"})

	res, err := h.c.Reply(ctx, s.ID, api.Reply{Text: "please add tests\n"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Via != api.ReplyMessage || res.Note == "" {
		t.Fatalf("result %+v", res)
	}
	waitFor(t, "message on the socket", func() bool { return len(fm.received()) == 2 })
	got := fm.received()
	if got[0]["type"] != "auth" || got[0]["token"] != token || got[1]["type"] != "user" {
		t.Fatalf("lines %v", got)
	}
	msg, _ := got[1]["message"].(map[string]any)
	content, _ := msg["content"].(string)
	if msg["role"] != "user" || !strings.HasPrefix(content, messagingPrefix) || !strings.HasSuffix(content, "\n\nplease add tests") ||
		!strings.Contains(content, "Pilothouse inbox on their computer") {
		t.Fatalf("message %q", content)
	}
	// Interrupting needs the terminal.
	_, err = h.c.Reply(ctx, s.ID, api.Reply{Interrupt: true})
	if ae := apiErr(t, err); ae.Code != "unsupported" || !strings.Contains(ae.Msg, "pilothouse run claude") {
		t.Fatalf("interrupt via message: %v", err)
	}

	// session.reply carries the text and channel, never the endpoint.
	evs, err := h.d.db.Events(ctx, 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	var rec *api.ReplyRecord
	for _, e := range evs {
		if e.Type == api.EventSessionReply {
			rec = new(api.ReplyRecord)
			_ = json.Unmarshal(e.Data, rec)
		}
	}
	if rec == nil || rec.Text != "please add tests" || rec.Via != api.ReplyMessage || rec.By != "cli" || rec.SessionID != s.ID || rec.At.IsZero() {
		t.Fatalf("reply record %+v", rec)
	}

	// Nothing a client or a log can see contains the token or the path.
	var seen []string
	add := func(what string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, what+": "+string(b))
	}
	add("events", evs)
	ev, _ := h.d.db.Evidence(ctx, s.ID, 1000)
	add("evidence", ev)
	ex, err := h.c.Explain(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	add("explain", ex)
	snap, _ := h.c.Snapshot(ctx)
	add("snapshot", snap)
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", paths.Socket())
	}}}
	for _, p := range []string{"/v1/snapshot", "/v1/sessions", "/v1/sessions/" + s.ID, "/v1/sessions/" + s.ID + "/events", "/v1/sessions/" + s.ID + "/explain", "/v1/events"} {
		resp, err := hc.Get("http://pilothouse" + p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		seen = append(seen, p+": "+string(b))
	}
	seen = append(seen, "logs: "+logs.String())
	for _, s := range seen {
		if strings.Contains(s, token) || strings.Contains(s, fm.sock) {
			t.Fatalf("messaging endpoint leaked in %s", s[:min(len(s), 300)])
		}
	}
	if !strings.Contains(logs.String(), "reply sent") {
		t.Fatal("logs were not captured")
	}

	// SessionEnd forgets the endpoint.
	claudeHook(t, h, "SessionEnd", nil, map[string]any{"reason": "exit"})
	if _, err := h.d.db.Secret(ctx, s.ID, store.SecretMessaging); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("endpoint kept after SessionEnd: %v", err)
	}
	if s := claudeSession(t, h); s.Caps.Reply {
		t.Fatalf("caps after end %+v", s.Caps)
	}
	_, err = h.c.Reply(ctx, s.ID, api.Reply{Text: "x"})
	if ae := apiErr(t, err); ae.Status != 400 || !strings.Contains(ae.Msg, "ended") {
		t.Fatalf("reply to an ended session: %v", err)
	}
}

func TestMessagingEndpointUpdatesAndGoneSocket(t *testing.T) {
	h, _ := claudeHarness(t)
	ctx := context.Background()
	// Implausible endpoints are ignored.
	claudeHook(t, h, "SessionStart", &adapters.MessagingEndpoint{Socket: "relative.sock", Token: "t"}, nil)
	s := claudeSession(t, h)
	if s.Caps.Reply {
		t.Fatalf("caps from a bad endpoint: %+v", s.Caps)
	}
	_, err := h.c.Reply(ctx, s.ID, api.Reply{Text: "x"})
	if ae := apiErr(t, err); ae.Code != "unsupported" || !strings.Contains(ae.Msg, "2.1.224") {
		t.Fatalf("no endpoint: %v", err)
	}
	// A socket nobody listens on: the reply fails clearly and the session
	// stops offering replies until a hook brings a live one.
	dead := filepath.Join(paths.Run(), "dead.sock")
	claudeHook(t, h, "UserPromptSubmit", &adapters.MessagingEndpoint{Socket: dead, Token: "t1"}, map[string]any{"prompt": "hi"})
	if s := claudeSession(t, h); !s.Caps.Reply {
		t.Fatalf("caps %+v", s.Caps)
	}
	_, err = h.c.Reply(ctx, s.ID, api.Reply{Text: "x"})
	if ae := apiErr(t, err); ae.Status != 400 || !strings.Contains(ae.Msg, "isn't listening") {
		t.Fatalf("dead socket: %v", err)
	}
	if s := claudeSession(t, h); s.Caps.Reply {
		t.Fatalf("caps after a dead socket %+v", s.Caps)
	}
	// Claude restarted (resume): the next hook brings the new socket.
	fm := startFakeMessaging(t)
	claudeHook(t, h, "Stop", &adapters.MessagingEndpoint{Socket: fm.sock, Token: "t2"}, nil)
	if _, err := h.c.Reply(ctx, s.ID, api.Reply{Text: "again"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "message", func() bool { return len(fm.received()) == 2 })
	if fm.received()[0]["token"] != "t2" {
		t.Fatalf("old token used: %v", fm.received())
	}
	// Endpoints from other agents are never stored.
	if _, err := h.hookEnvErr(ctx, &adapters.MessagingEndpoint{Socket: fm.sock, Token: "t3"}); err != nil {
		t.Fatal(err)
	}
	ss, _ := h.c.Sessions(ctx)
	for _, x := range ss {
		if x.Agent == fakeKind && x.Caps.Reply {
			t.Fatalf("fake-agent session got reply caps: %+v", x.Caps)
		}
	}
}

// Sessions that end without a SessionEnd (crash, expiry) lose their
// endpoint in the hourly sweep.
func TestSweepSecretsOfEndedSessions(t *testing.T) {
	h, _ := claudeHarness(t)
	ctx := context.Background()
	fm := startFakeMessaging(t)
	claudeHook(t, h, "SessionStart", &adapters.MessagingEndpoint{Socket: fm.sock, Token: "t"}, nil)
	s := claudeSession(t, h)
	h.d.sweepSecrets(ctx)
	if _, err := h.d.db.Secret(ctx, s.ID, store.SecretMessaging); err != nil {
		t.Fatalf("live session's endpoint swept: %v", err)
	}
	if err := h.d.markExited(ctx, s.ID, "Process exited", nil); err != nil {
		t.Fatal(err)
	}
	h.d.sweepSecrets(ctx)
	if _, err := h.d.db.Secret(ctx, s.ID, store.SecretMessaging); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ended session's endpoint kept: %v", err)
	}
}

// hookEnvErr sends a fake-agent hook that carries a messaging endpoint.
func (h *harness) hookEnvErr(ctx context.Context, ep *adapters.MessagingEndpoint) (adapters.HookResponse, error) {
	payload, _ := json.Marshal(adapters.HookEvent{Kind: adapters.HookSessionStart, AgentSessionID: "fake-ep", Cwd: os.TempDir()})
	return h.c.Hook(ctx, adapters.HookEnvelope{Agent: fakeKind, Payload: payload, Ancestors: fakeAgentAncestors, Messaging: ep, ReceivedAt: time.Now()})
}

func TestReplyValidationAndGuard(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	ctx := context.Background()
	h.hook(ctx, t, adapters.HookEvent{Kind: adapters.HookSessionStart})
	ss, _ := h.c.Sessions(ctx)
	id := ss[0].ID

	for _, c := range []struct {
		r    api.Reply
		code int
	}{
		{api.Reply{Text: " \n"}, 400},
		{api.Reply{Text: strings.Repeat("x", maxReplyBytes+1)}, 400},
	} {
		_, err := h.c.Reply(ctx, id, c.r)
		if ae := apiErr(t, err); ae.Status != c.code {
			t.Fatalf("%q: %v", c.r.Text[:min(len(c.r.Text), 20)], err)
		}
	}
	// (JSON can't carry invalid UTF-8; in-process callers can.)
	if _, err := h.d.Reply(ctx, id, api.Reply{Text: "bad \xff utf8"}, "cli"); !errors.Is(err, errBadRequest) {
		t.Fatalf("invalid UTF-8: %v", err)
	}
	if _, err := h.d.Reply(ctx, id, api.Reply{Text: "x"}, ""); !errors.Is(err, errBadRequest) {
		t.Fatalf("no author: %v", err)
	}
	if _, err := h.c.Reply(ctx, "s_nothere", api.Reply{Text: "x"}); apiErr(t, err).Status != 404 {
		t.Fatalf("unknown session: %v", err)
	}
	// An observed session of an agent without a channel: actionable 501.
	_, err := h.c.Reply(ctx, id, api.Reply{Text: "x"})
	if ae := apiErr(t, err); ae.Status != 501 || ae.Code != "unsupported" || !strings.Contains(ae.Msg, "pilothouse run") {
		t.Fatalf("unsupported: %v", err)
	}
	if got := noReplyReason(api.Session{Agent: api.AgentCodex}); got != "Run Codex with `pilothouse run codex` to reply from your phone" {
		t.Fatalf("codex reason %q", got)
	}
	// Agents can't reply to sessions (the local route is guarded).
	h.rejectCallers(errors.New("refused: inside an agent"))
	_, err = h.c.Reply(ctx, id, api.Reply{Text: "x"})
	if ae := apiErr(t, err); ae.Status != 403 || ae.Code != "agent_caller" {
		t.Fatalf("guard: %v", err)
	}
}

// ---- wrapped sessions ----

// byteAgent reads its terminal raw and prints every byte as [xx]; it turns
// bracketed paste on when asked.
func byteAgent(bracketed bool) string {
	on := ""
	if bracketed {
		on = `\033[?2004h`
	}
	return `stty raw -echo; printf '` + on + `ready\r\n'; while :; do b=$(dd bs=1 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n'); printf '[%s]' "$b"; done`
}

func hexBytes(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		fmt.Fprintf(&b, "[%02x]", s[i])
	}
	return b.String()
}

type holderProc struct {
	sid  string
	stop context.CancelFunc
	done chan struct{}
}

// startWrapped runs an in-process holder that registers with the harness
// daemon, and waits until the daemon knows what it supports.
func startWrapped(t *testing.T, h *harness, script string) *holderProc {
	t.Helper()
	hp := &holderProc{sid: wrap.NewSessionID(), done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	hp.stop = cancel
	go func() {
		defer close(hp.done)
		_, _ = wrap.RunHolder(ctx, wrap.HolderConfig{SessionID: hp.sid, Argv: []string{"/bin/sh", "-c", script},
			Linger: 200 * time.Millisecond, Daemon: client.NewSocket(paths.Socket())})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-hp.done:
		case <-time.After(15 * time.Second):
			t.Error("holder did not stop")
		}
	})
	waitFor(t, "wrapped session with terminal caps", func() bool {
		s, err := h.c.Session(context.Background(), hp.sid)
		return err == nil && s.Caps.Reply && s.Caps.Watch && s.Caps.ReplyVia == api.ReplyTerminal
	})
	return hp
}

// termWatch collects a StreamTerminal in the background.
type termWatch struct {
	mu     sync.Mutex
	out    bytes.Buffer
	sizes  [][2]uint16
	fixed  []bool // per size event
	exited *int
	err    error
	done   chan struct{}
	cancel context.CancelFunc
}

func watchTerm(t *testing.T, h *harness, sid string, replay bool) *termWatch {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := &termWatch{done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(w.done)
		err := h.d.StreamTerminal(ctx, sid, replay, func(ev core.TerminalEvent) error {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.out.Write(ev.Data)
			if ev.Cols > 0 {
				w.sizes = append(w.sizes, [2]uint16{ev.Cols, ev.Rows})
				w.fixed = append(w.fixed, ev.Fixed)
			}
			if ev.Exited {
				c := ev.ExitCode
				w.exited = &c
			}
			return nil
		})
		w.mu.Lock()
		w.err = err
		w.mu.Unlock()
	}()
	t.Cleanup(func() { cancel(); <-w.done })
	return w
}

func (w *termWatch) output() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.String()
}

func (w *termWatch) waitOutput(t *testing.T, s string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(w.output(), s) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q in %q", s, w.output())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReplyAndTerminalWrapped(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	ctx := context.Background()
	hp := startWrapped(t, h, byteAgent(true))
	s, _ := h.c.Session(ctx, hp.sid)
	if !s.Caps.Interrupt {
		t.Fatalf("caps %+v", s.Caps)
	}

	// A person's terminal is attached the whole time.
	att, err := wrap.Dial(ctx, paths.HolderSocket(hp.sid), wrap.Hello{Mode: wrap.ModeAttach, Cols: 100, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer att.Close()
	attFrames := make(chan wrap.Frame, 4096)
	go func() {
		for {
			f, err := att.Recv()
			if err != nil {
				close(attFrames)
				return
			}
			attFrames <- f
		}
	}()

	w := watchTerm(t, h, hp.sid, true)
	w.waitOutput(t, "ready")

	// A reply: one bracketed paste, then Enter.
	res, err := h.c.Reply(ctx, hp.sid, api.Reply{Text: "fix it\nplease"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Via != api.ReplyTerminal {
		t.Fatalf("via %s", res.Via)
	}
	w.waitOutput(t, hexBytes("\x1b[200~fix it\rplease\x1b[201~\r"))

	// An interrupt: Esc, a pause, then the text.
	if _, err := h.c.Reply(ctx, hp.sid, api.Reply{Text: "stop", Interrupt: true}); err != nil {
		t.Fatal(err)
	}
	w.waitOutput(t, hexBytes("\x1b\x1b[200~stop\x1b[201~\r"))
	if _, err := h.c.Reply(ctx, hp.sid, api.Reply{Interrupt: true}); err != nil {
		t.Fatal(err)
	}

	// Keystrokes from successive calls arrive in order.
	keys := "abcdefghijklmnop"
	for i := range keys {
		if err := h.d.TerminalInput(ctx, hp.sid, []byte{keys[i]}, "device:d_x"); err != nil {
			t.Fatal(err)
		}
	}
	w.waitOutput(t, hexBytes(keys))
	if err := h.d.TerminalInput(ctx, hp.sid, make([]byte, maxTerminalInput+1), "device:d_x"); err == nil {
		t.Fatal("oversized input accepted")
	}

	// Resizes don't apply while a terminal is attached; the watcher was
	// told the attached terminal's size.
	if err := h.d.ResizeTerminal(ctx, hp.sid, 40, 12); err != nil {
		t.Fatal(err)
	}
	if err := h.d.ResizeTerminal(ctx, hp.sid, 0, 12); err == nil {
		t.Fatal("zero size accepted")
	}

	// The attached terminal saw everything and was never detached.
	att.Close()
	var attOut bytes.Buffer
	for f := range attFrames {
		if f.Type == wrap.FrameDetach || f.Type == wrap.FrameExit {
			t.Fatalf("attached terminal got %s", f.Type)
		}
		if f.Type == wrap.FrameData {
			attOut.Write(f.Payload)
		}
	}
	if !strings.Contains(attOut.String(), hexBytes("fix it")) {
		t.Fatalf("attached terminal missed the reply: %q", attOut.String())
	}
	w.mu.Lock()
	sizes := append([][2]uint16(nil), w.sizes...)
	w.mu.Unlock()
	for _, sz := range sizes {
		if sz == [2]uint16{40, 12} {
			t.Fatalf("resize applied while attached: %v", sizes)
		}
	}
	// Detached now: resizes apply and the watcher hears about them.
	time.Sleep(50 * time.Millisecond)
	if err := h.d.ResizeTerminal(ctx, hp.sid, 40, 12); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "resize reaches the watcher", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.sizes) > 0 && w.sizes[len(w.sizes)-1] == [2]uint16{40, 12}
	})
	// The size was fixed by the attached terminal, and free once it left.
	w.mu.Lock()
	fixed := append([]bool(nil), w.fixed...)
	sizes = append([][2]uint16(nil), w.sizes...)
	w.mu.Unlock()
	if !fixed[0] || fixed[len(fixed)-1] {
		t.Fatalf("fixed flags %v for sizes %v", fixed, sizes)
	}

	evs, _ := h.c.SessionEvents(ctx, hp.sid, 100)
	n := 0
	for _, e := range evs {
		if e.Type == api.EventSessionReply {
			var rec api.ReplyRecord
			_ = json.Unmarshal(e.Data, &rec)
			if rec.Via != api.ReplyTerminal {
				t.Fatalf("record %+v", rec)
			}
			n++
		}
	}
	if n != 3 {
		t.Fatalf("%d session.reply events, want 3", n)
	}

	// The agent exits: the stream ends with its code; replies are refused.
	hp.stop()
	<-w.done
	if w.exited == nil {
		t.Fatalf("no exit event (err %v)", w.err)
	}
	waitFor(t, "session exited", func() bool {
		s, _ := h.c.Session(ctx, hp.sid)
		return s.ExitedAt != nil && !s.Caps.Reply
	})
	if _, err := h.c.Reply(ctx, hp.sid, api.Reply{Text: "x"}); apiErr(t, err).Status != 400 {
		t.Fatalf("reply after exit: %v", err)
	}
	if err := h.d.WatchTerminal(ctx, hp.sid, false, func([]byte) error { return nil }); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("watch after exit: %v", err)
	}
}

// While a wrapped agent shows a prompt, a plain reply is refused: its
// Enter would answer the prompt. With interrupt (Esc first) it goes.
func TestReplyRefusedWhileBlocked(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	ctx := context.Background()
	hp := startWrapped(t, h, `printf 'ready\033]7501;state=blocked;kind=permission\007\n'; while read l; do echo "got:$l"; done`)
	waitFor(t, "blocked", func() bool { s, _ := h.c.Session(ctx, hp.sid); return s.State == api.StateBlocked })
	w := watchTerm(t, h, hp.sid, true)
	_, err := h.c.Reply(ctx, hp.sid, api.Reply{Text: "yes do it"})
	if ae := apiErr(t, err); ae.Status != 400 || !strings.Contains(ae.Msg, "waiting for an answer") || !strings.Contains(ae.Msg, "interrupt") {
		t.Fatalf("reply while blocked: %v", err)
	}
	if strings.Contains(w.output(), "yes do it") {
		t.Fatal("refused reply was typed")
	}
	if _, err := h.c.Reply(ctx, hp.sid, api.Reply{Text: "do this instead", Interrupt: true}); err != nil {
		t.Fatal(err)
	}
	w.waitOutput(t, "got:\x1b") // a plain shell reads Esc as part of the line
	w.waitOutput(t, "do this instead")
}

// A generic program without bracketed paste gets the text typed.
func TestReplyWrappedPlainProgram(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	hp := startWrapped(t, h, `echo ready; while read l; do echo "got:$l"; done`)
	w := watchTerm(t, h, hp.sid, true)
	w.waitOutput(t, "ready")
	if _, err := h.c.Reply(context.Background(), hp.sid, api.Reply{Text: "one\ntwo"}); err != nil {
		t.Fatal(err)
	}
	w.waitOutput(t, "got:one")
	w.waitOutput(t, "got:two")
}

// oldHolderAt serves the 0.1 holder protocol on a session's socket and
// records the hello modes it is sent.
func oldHolderAt(t *testing.T, sid string) func() []string {
	t.Helper()
	ln, err := net.Listen("unix", paths.HolderSocket(sid))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	var modes []string
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				f, err := wrap.ReadFrame(c)
				if err != nil {
					return
				}
				var hello struct {
					Mode string `json:"mode"`
				}
				_ = json.Unmarshal(f.Payload, &hello)
				mu.Lock()
				modes = append(modes, hello.Mode)
				mu.Unlock()
				_ = wrap.WriteFrame(c, wrap.FrameHello, []byte(`{"v":1,"session_id":"`+sid+`","name":"old","agent":"claude","pid":1,"holder_pid":1}`))
			}()
		}
	}()
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), modes...)
	}
}

func TestOldHolderNeverGetsNewModes(t *testing.T) {
	h, _ := claudeHarness(t)
	ctx := context.Background()
	sid := wrap.NewSessionID()
	modes := oldHolderAt(t, sid)
	if _, err := h.c.RegisterWrapped(ctx, api.WrappedSessionBody{SessionID: sid, Agent: api.AgentClaude, Cwd: os.TempDir(),
		PID: os.Getpid(), HolderSock: paths.HolderSocket(sid)}); err != nil {
		t.Fatal(err)
	}
	s, _ := h.c.Session(ctx, sid)
	if s.Caps.Reply || s.Caps.Watch || s.Caps.Interrupt {
		t.Fatalf("old holder caps %+v", s.Caps)
	}
	_, err := h.c.Reply(ctx, sid, api.Reply{Text: "x"})
	if ae := apiErr(t, err); ae.Status != 501 || !strings.Contains(ae.Msg, "older version") {
		t.Fatalf("reply to an old holder: %v", err)
	}
	if err := h.d.WatchTerminal(ctx, sid, true, func([]byte) error { return nil }); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("watch an old holder: %v", err)
	}
	if err := h.d.TerminalInput(ctx, sid, []byte("x"), "cli"); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("input to an old holder: %v", err)
	}
	for _, m := range modes() {
		if m != "probe" {
			t.Fatalf("old holder was sent %q (modes %v)", m, modes())
		}
	}
	// A wrapped Claude session behind an old holder still has its
	// messaging socket.
	fm := startFakeMessaging(t)
	payload, _ := json.Marshal(map[string]any{"session_id": "cc-old", "cwd": os.TempDir(), "hook_event_name": "Stop"})
	if _, err := h.c.Hook(ctx, adapters.HookEnvelope{Agent: api.AgentClaude, Payload: payload, PilothouseSessionID: sid,
		Ancestors: []adapters.Proc{{PID: os.Getpid(), Comm: "claude"}}, Messaging: &adapters.MessagingEndpoint{Socket: fm.sock, Token: "t"},
		ReceivedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if s, _ := h.c.Session(ctx, sid); !s.Caps.Reply || s.Caps.ReplyVia != api.ReplyMessage {
		t.Fatalf("caps %+v", s.Caps)
	}
	res, err := h.c.Reply(ctx, sid, api.Reply{Text: "via the socket"})
	if err != nil || res.Via != api.ReplyMessage {
		t.Fatalf("reply %+v %v", res, err)
	}
}

func TestTerminalRefusesObservedAndForeignSockets(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	ctx := context.Background()
	h.hook(ctx, t, adapters.HookEvent{Kind: adapters.HookSessionStart})
	ss, _ := h.c.Sessions(ctx)
	if err := h.d.WatchTerminal(ctx, ss[0].ID, true, func([]byte) error { return nil }); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("observed: %v", err)
	}
	// A registration naming a socket elsewhere is never dialed.
	sid := wrap.NewSessionID()
	if _, err := h.c.RegisterWrapped(ctx, api.WrappedSessionBody{SessionID: sid, Cwd: os.TempDir(), PID: os.Getpid(),
		HolderSock: filepath.Join(t.TempDir(), "elsewhere.sock")}); err != nil {
		t.Fatal(err)
	}
	if err := h.d.TerminalInput(ctx, sid, []byte("x"), "cli"); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("foreign socket: %v", err)
	}
}
