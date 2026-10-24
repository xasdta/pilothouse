package web

import (
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/web/webtest"
)

// termCore adds the Terminal capabilities to the fake core.
type termCore struct {
	*webtest.FakeCore
	mu      sync.Mutex
	inputs  []string
	by      []string
	resizes [][2]uint16
	streams chan *fakeStream
}

type fakeStream struct {
	id     string
	replay bool
	ev     func(core.TerminalEvent) error
	ctx    context.Context
	end    chan error
}

func newTermCore(fc *webtest.FakeCore) *termCore {
	return &termCore{FakeCore: fc, streams: make(chan *fakeStream, 8)}
}

func (c *termCore) StreamTerminal(ctx context.Context, id string, replay bool, ev func(core.TerminalEvent) error) error {
	fs := &fakeStream{id: id, replay: replay, ev: ev, ctx: ctx, end: make(chan error, 1)}
	c.streams <- fs
	select {
	case err := <-fs.end:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *termCore) WatchTerminal(ctx context.Context, id string, replay bool, out func([]byte) error) error {
	return c.StreamTerminal(ctx, id, replay, func(ev core.TerminalEvent) error { return out(ev.Data) })
}

func (c *termCore) TerminalInput(_ context.Context, id string, data []byte, by string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inputs = append(c.inputs, id+":"+string(data))
	c.by = append(c.by, by)
	return nil
}

func (c *termCore) ResizeTerminal(_ context.Context, id string, cols, rows uint16) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resizes = append(c.resizes, [2]uint16{cols, rows})
	return nil
}

func (c *termCore) got() ([]string, []string, [][2]uint16) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.inputs...), append([]string(nil), c.by...), append([][2]uint16(nil), c.resizes...)
}

func (c *termCore) nextStream(t *testing.T) *fakeStream {
	t.Helper()
	select {
	case fs := <-c.streams:
		return fs
	case <-time.After(5 * time.Second):
		t.Fatal("no terminal stream started")
	}
	return nil
}

// watchOnlyCore has WatchTerminal but not StreamTerminal.
type watchOnlyCore struct {
	*webtest.FakeCore
	out chan func([]byte) error
}

func (c *watchOnlyCore) WatchTerminal(ctx context.Context, _ string, _ bool, out func([]byte) error) error {
	c.out <- out
	<-ctx.Done()
	return nil
}
func (c *watchOnlyCore) TerminalInput(context.Context, string, []byte, string) error { return nil }
func (c *watchOnlyCore) ResizeTerminal(context.Context, string, uint16, uint16) error {
	return nil
}

func termSessions(fc *webtest.FakeCore, now time.Time) {
	live := api.SessionCaps{Reply: true, ReplyVia: api.ReplyTerminal, Interrupt: true, Watch: true}
	fc.PutSession(api.Session{ID: "s_term000001", Agent: api.AgentClaude, Mode: api.ModeWrapped, State: api.StateWorking, Caps: live, StartedAt: now})
	fc.PutSession(api.Session{ID: "s_other00001", Agent: api.AgentClaude, Mode: api.ModeWrapped, State: api.StateWorking, Caps: live, StartedAt: now})
	fc.PutSession(api.Session{ID: "s_observed01", Agent: api.AgentClaude, Mode: api.ModeObserved, State: api.StateWorking, StartedAt: now})
	ended := now
	fc.PutSession(api.Session{ID: "s_ended00001", Agent: api.AgentClaude, Mode: api.ModeWrapped, State: api.StateExited, ExitedAt: &ended, StartedAt: now})
}

func (e *env) mintTerminal(k *ecdsa.PrivateKey, ck, id string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do("POST", "/api/sessions/"+id+"/terminal/token", e.signAction(k, ActionTerminal, id, struct{}{}), reqOpt{cookie: ck})
}

func termInput(e *env, ck, id, tok string, body any, o ...reqOpt) *httptest.ResponseRecorder {
	opt := reqOpt{cookie: ck, header: map[string]string{"X-Terminal-Token": tok}}
	if len(o) > 0 {
		opt.noCSRF = o[0].noCSRF
	}
	return e.do("POST", "/api/sessions/"+id+"/terminal/input", body, opt)
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestTerminalUnsupportedWithoutCapability(t *testing.T) {
	e := newEnv(t)
	k, pub := genKey(t)
	ck, _ := e.pair(pub)
	termSessions(e.fc, e.clock.Now())
	for _, w := range []*httptest.ResponseRecorder{
		e.mintTerminal(k, ck, "s_term000001"),
		e.do("GET", "/api/sessions/s_term000001/terminal", nil, reqOpt{cookie: ck}),
		termInput(e, ck, "s_term000001", "x", map[string]string{"data": b64("x")}),
	} {
		if w.Code != 501 || errCode(t, w) != "unsupported" {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	}
}

func TestTerminalTokenScoping(t *testing.T) {
	var tc *termCore
	e := newEnv(t, func(d *Deps) { tc = newTermCore(d.Core.(*webtest.FakeCore)); d.Core = tc })
	termSessions(e.fc, e.clock.Now())
	k, pub := genKey(t)
	ck, devID := e.pair(pub)
	k2, pub2 := genKey(t)
	ck2, _ := e.pair(pub2)

	w := e.mintTerminal(k, ck, "s_term000001")
	if w.Code != 200 {
		t.Fatalf("mint: %d %s", w.Code, w.Body)
	}
	var tok terminalTokenResponse
	mustJSON(t, w, &tok)
	if len(tok.Token) < 40 || !tok.ExpiresAt.Equal(e.clock.Now().Add(TerminalTokenTTL)) {
		t.Fatalf("token %+v", tok)
	}

	// Input and resize with the token.
	if w := termInput(e, ck, "s_term000001", tok.Token, map[string]string{"data": b64("ls\r")}); w.Code != 204 {
		t.Fatalf("input: %d %s", w.Code, w.Body)
	}
	if w := termInput(e, ck, "s_term000001", tok.Token, map[string]any{"resize": map[string]int{"cols": 50, "rows": 20}}); w.Code != 204 {
		t.Fatalf("resize: %d %s", w.Code, w.Body)
	}
	inputs, bys, resizes := tc.got()
	if len(inputs) != 1 || inputs[0] != "s_term000001:ls\r" || bys[0] != "device:"+devID || len(resizes) != 1 || resizes[0] != [2]uint16{50, 20} {
		t.Fatalf("core got %q %v %v", inputs, bys, resizes)
	}

	// The token is bound to its session and device.
	rejected := func(what string, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != 403 || errCode(t, w) != "terminal_token" {
			t.Fatalf("%s: %d %s", what, w.Code, w.Body)
		}
	}
	rejected("other session", termInput(e, ck, "s_other00001", tok.Token, map[string]string{"data": b64("x")}))
	rejected("other device", termInput(e, ck2, "s_term000001", tok.Token, map[string]string{"data": b64("x")}))
	rejected("no token", termInput(e, ck, "s_term000001", "", map[string]string{"data": b64("x")}))
	rejected("made-up token", termInput(e, ck, "s_term000001", strings.Repeat("A", 43), map[string]string{"data": b64("x")}))
	if w := termInput(e, "", "s_term000001", tok.Token, map[string]string{"data": b64("x")}); w.Code != 401 {
		t.Fatalf("no cookie: %d", w.Code)
	}
	if w := termInput(e, ck, "s_term000001", tok.Token, map[string]string{"data": b64("x")}, reqOpt{noCSRF: true}); w.Code != 403 || errCode(t, w) != "csrf" {
		t.Fatalf("no CSRF header: %d %s", w.Code, w.Body)
	}

	// Bad bodies.
	for name, body := range map[string]any{
		"both":       map[string]any{"data": b64("x"), "resize": map[string]int{"cols": 1, "rows": 1}},
		"neither":    map[string]any{},
		"not base64": map[string]string{"data": "%%%"},
		"zero size":  map[string]any{"resize": map[string]int{"cols": 0, "rows": 10}},
		"huge size":  map[string]any{"resize": map[string]int{"cols": 5000, "rows": 10}},
		"oversized":  map[string]string{"data": b64(strings.Repeat("x", maxTerminalInput+1))},
		"unknown":    map[string]any{"data": b64("x"), "extra": 1},
	} {
		if w := termInput(e, ck, "s_term000001", tok.Token, body); w.Code != 400 {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if inputs, _, _ := tc.got(); len(inputs) != 1 {
		t.Fatalf("rejected input reached the core: %q", inputs)
	}

	// Minting: signed for one session, posted for another; replayed;
	// sessions without a live terminal; a prefix.
	sb := e.signAction(k, ActionTerminal, "s_other00001", struct{}{})
	if w := e.do("POST", "/api/sessions/s_term000001/terminal/token", sb, reqOpt{cookie: ck}); w.Code != 403 || errCode(t, w) != "bad_signature" {
		t.Fatalf("cross-session mint: %d %s", w.Code, w.Body)
	}
	good := e.signAction(k, ActionTerminal, "s_other00001", struct{}{})
	if w := e.do("POST", "/api/sessions/s_other00001/terminal/token", good, reqOpt{cookie: ck}); w.Code != 200 {
		t.Fatalf("mint other: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/sessions/s_other00001/terminal/token", good, reqOpt{cookie: ck}); w.Code != 403 || errCode(t, w) != "replayed" {
		t.Fatalf("replayed mint: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/sessions/s_term000001/terminal/token", e.signAction(k, ActionReply, "s_term000001", struct{}{}), reqOpt{cookie: ck}); w.Code != 403 {
		t.Fatalf("reply signature as terminal: %d %s", w.Code, w.Body)
	}
	if w := e.mintTerminal(k, ck, "s_observed01"); w.Code != 501 {
		t.Fatalf("observed: %d %s", w.Code, w.Body)
	}
	if w := e.mintTerminal(k, ck, "s_ended00001"); w.Code != 400 {
		t.Fatalf("ended: %d %s", w.Code, w.Body)
	}
	if w := e.mintTerminal(k, ck, "s_nothere001"); w.Code != 404 {
		t.Fatalf("unknown: %d %s", w.Code, w.Body)
	}
	if w := e.mintTerminal(k, ck, "s_term"); w.Code != 404 {
		t.Fatalf("prefix: %d %s", w.Code, w.Body)
	}
	// Device 2 mints its own token, which device 1 can't use.
	w = e.mintTerminal(k2, ck2, "s_term000001")
	var tok2 terminalTokenResponse
	mustJSON(t, w, &tok2)
	rejected("token of device 2 from device 1", termInput(e, ck, "s_term000001", tok2.Token, map[string]string{"data": b64("x")}))
	if w := termInput(e, ck2, "s_term000001", tok2.Token, map[string]string{"data": b64("y")}); w.Code != 204 {
		t.Fatalf("device 2: %d %s", w.Code, w.Body)
	}

	// Ten minutes later the tokens are dead.
	e.clock.Add(TerminalTokenTTL)
	rejected("expired", termInput(e, ck, "s_term000001", tok.Token, map[string]string{"data": b64("x")}))
}

func TestTerminalTokensBounded(t *testing.T) {
	var tt terminalTokens
	now := time.Now()
	first, _, _ := tt.mint("s_a", "d_a", now)
	for i := 0; i < maxTerminalTokens+10; i++ {
		if _, _, err := tt.mint("s_a", "d_a", now.Add(time.Duration(i+1)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	if len(tt.m) != maxTerminalTokens {
		t.Fatalf("%d tokens kept", len(tt.m))
	}
	if tt.valid(first, "s_a", "d_a", now) {
		t.Fatal("oldest token survived eviction")
	}
}

// openTerminal opens the SSE stream; it fails the test on a non-SSE answer.
func openTerminal(t *testing.T, ts *httptest.Server, ck, id, query string) *sseClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/sessions/"+id+"/terminal"+query, nil)
	req.Header.Set("Cookie", cookieName+"="+ck)
	resp, err := ts.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		cancel()
		t.Fatalf("terminal stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	c := &sseClient{events: make(chan [2]string, 64), done: make(chan error, 1), cancel: cancel}
	go func() {
		defer resp.Body.Close()
		c.done <- readSSE(ctx, resp.Body, func(ev string, data []byte) error {
			c.events <- [2]string{ev, string(data)}
			return nil
		})
	}()
	t.Cleanup(cancel)
	return c
}

// nextNonPing skips keep-alives.
func (c *sseClient) nextNonPing(t *testing.T) (string, string) {
	t.Helper()
	for {
		ev, data := c.next(t)
		if ev != "ping" {
			return ev, data
		}
	}
}

func TestTerminalStream(t *testing.T) {
	var tc *termCore
	e := newEnv(t, func(d *Deps) {
		tc = newTermCore(d.Core.(*webtest.FakeCore))
		d.Core = tc
		d.PingEvery = 50 * time.Millisecond
	})
	termSessions(e.fc, e.clock.Now())
	ts := e.live(t)
	_, pub := genKey(t)
	ck, devID := e.pair(pub)

	c := openTerminal(t, ts, ck, "s_term000001", "?replay=1")
	fs := tc.nextStream(t)
	if fs.id != "s_term000001" || !fs.replay {
		t.Fatalf("stream %+v", fs)
	}
	// A terminal attached at the computer fixes the size: `size` comes
	// before the replay and on every change; when it leaves, `free`.
	// Sizes nobody fixed are the viewer's own business: no event.
	_ = fs.ev(core.TerminalEvent{Cols: 120, Rows: 40, Fixed: true})
	_ = fs.ev(core.TerminalEvent{Data: []byte("hello \x1b[1mworld\x1b[0m")})
	_ = fs.ev(core.TerminalEvent{Cols: 100, Rows: 30, Fixed: true})
	_ = fs.ev(core.TerminalEvent{Cols: 100, Rows: 30})
	_ = fs.ev(core.TerminalEvent{Cols: 45, Rows: 20})
	_ = fs.ev(core.TerminalEvent{Data: []byte("x")})
	for _, want := range [][2]string{
		{"size", `{"cols":120,"rows":40}`},
		{"data", b64("hello \x1b[1mworld\x1b[0m")},
		{"size", `{"cols":100,"rows":30}`},
		{"free", `{"cols":100,"rows":30}`},
		{"data", b64("x")},
	} {
		if ev, data := c.nextNonPing(t); ev != want[0] || data != want[1] {
			t.Fatalf("got %s %s, want %s %s", ev, data, want[0], want[1])
		}
	}
	// Keep-alives flow while the terminal is quiet.
	if ev, _ := c.next(t); ev != "ping" {
		t.Fatalf("want a ping, got %s", ev)
	}
	_ = fs.ev(core.TerminalEvent{Exited: true, ExitCode: 3})
	fs.end <- nil
	if ev, data := c.nextNonPing(t); ev != "exit" || data != `{"code":3}` {
		t.Fatalf("exit: %s %s", ev, data)
	}
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not end after exit")
	}

	// A stream that ends badly says so (generically: no socket errors).
	c = openTerminal(t, ts, ck, "s_term000001", "")
	fs = tc.nextStream(t)
	if fs.replay {
		t.Fatal("replay without ?replay=1")
	}
	fs.end <- errors.New("read unix /tmp/x/h-s_term000001.sock: connection reset")
	ev, data := c.nextNonPing(t)
	if ev != "error" || strings.Contains(data, "/tmp/") || !strings.Contains(data, "reconnect") {
		t.Fatalf("error event: %s %s", ev, data)
	}

	// The client going away cancels the core's stream.
	c = openTerminal(t, ts, ck, "s_term000001", "")
	fs = tc.nextStream(t)
	c.cancel()
	select {
	case <-fs.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("core stream outlived the client")
	}

	// Revoking the device ends its terminal streams.
	c = openTerminal(t, ts, ck, "s_term000001", "")
	fs = tc.nextStream(t)
	if w := e.localDo("DELETE", "/v1/devices/"+devID); w.Code != 204 {
		t.Fatal(w.Code)
	}
	select {
	case <-fs.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("terminal stream survived revocation")
	}

	// No live terminal: a JSON error, not a stream.
	_, pub = genKey(t)
	ck, _ = e.pair(pub)
	for id, code := range map[string]int{"s_observed01": 501, "s_ended00001": 400, "s_nothere001": 404} {
		if w := e.do("GET", "/api/sessions/"+id+"/terminal", nil, reqOpt{cookie: ck}); w.Code != code {
			t.Fatalf("%s: %d %s", id, w.Code, w.Body)
		}
	}
	if w := e.do("GET", "/api/sessions/s_term000001/terminal", nil, reqOpt{}); w.Code != 401 {
		t.Fatalf("no cookie: %d", w.Code)
	}
}

// A core with only WatchTerminal: data flows, and the exit code is unknown.
func TestTerminalStreamWatchOnlyCore(t *testing.T) {
	var wc *watchOnlyCore
	e := newEnv(t, func(d *Deps) {
		wc = &watchOnlyCore{FakeCore: d.Core.(*webtest.FakeCore), out: make(chan func([]byte) error, 1)}
		d.Core = wc
	})
	termSessions(e.fc, e.clock.Now())
	ts := e.live(t)
	_, pub := genKey(t)
	ck, _ := e.pair(pub)
	c := openTerminal(t, ts, ck, "s_term000001", "")
	var out func([]byte) error
	select {
	case out = <-wc.out:
	case <-time.After(5 * time.Second):
		t.Fatal("no watch")
	}
	_ = out([]byte("hi"))
	if ev, data := c.nextNonPing(t); ev != "data" || data != b64("hi") {
		t.Fatalf("%s %s", ev, data)
	}
	c.cancel()
}
