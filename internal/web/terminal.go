package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
)

// Live terminal for wrapped sessions (F3, workstream B). A device mints a
// short-lived, session-bound input token with one signed ActionTerminal
// (so keystrokes need no per-key signature), streams output over SSE and
// posts input with the token.
//
//	POST /api/sessions/{id}/terminal/token   signed "terminal", target = session id, payload {}
//	  → {"token": "…", "expires_at": "<RFC 3339>"}
//	GET  /api/sessions/{id}/terminal?replay=1   (cookie only: EventSource can't send headers)
//	  event: size  {"cols": c, "rows": r}   first, then on every change (addition to the contract)
//	  event: data  <base64 of output bytes>
//	  event: exit  {"code": n}              the agent exited (n is -1 when unknown); the stream ends
//	  event: error {"error": "…"}           the stream ended otherwise (addition); reconnect with replay=1
//	  event: ping  {} every 15 s
//	POST /api/sessions/{id}/terminal/input   header X-Terminal-Token, CSRF headers
//	  {"data": "<base64>"} or {"resize": {"cols": c, "rows": r}} → 204
//
// A token works only for the session it was minted for, from the device
// that minted it (whose cookie must still be valid), for 10 minutes; a
// rejected token answers 403 terminal_token and the app mints a new one.
// Tokens live in memory only: a daemon restart invalidates them. Watching
// is read-only and never detaches or resizes a terminal attached with
// `pilothouse attach`; resizes apply only while no terminal is attached.

const (
	// TerminalTokenTTL is how long a terminal input token is valid.
	TerminalTokenTTL = 10 * time.Minute
	// maxTerminalTokens bounds the tokens kept at once (oldest go first).
	maxTerminalTokens = 256
	// maxTerminalInput bounds one input POST (decoded bytes).
	maxTerminalInput = 64 << 10
	// maxTerminalSize bounds a resize.
	maxTerminalSize = 1000
	// terminalBuffer is how many output events may wait for a slow
	// client; beyond that the holder drops the stream and the app
	// reconnects with replay.
	terminalBuffer = 64
)

// terminalTokens holds minted tokens by the SHA-256 of their value.
type terminalTokens struct {
	mu sync.Mutex
	m  map[[32]byte]terminalToken
}

type terminalToken struct {
	session, device string
	expires         time.Time
}

func (tt *terminalTokens) mint(session, device string, now time.Time) (string, time.Time, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", time.Time{}, err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw[:])
	exp := now.Add(TerminalTokenTTL)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if tt.m == nil {
		tt.m = map[[32]byte]terminalToken{}
	}
	for k, v := range tt.m {
		if !now.Before(v.expires) {
			delete(tt.m, k)
		}
	}
	for len(tt.m) >= maxTerminalTokens {
		var oldest [32]byte
		first := true
		for k, v := range tt.m {
			if first || v.expires.Before(tt.m[oldest].expires) {
				oldest, first = k, false
			}
		}
		delete(tt.m, oldest)
	}
	tt.m[sha256.Sum256([]byte(tok))] = terminalToken{session: session, device: device, expires: exp}
	return tok, exp, nil
}

// valid reports whether tok was minted for session by device and is unexpired.
func (tt *terminalTokens) valid(tok, session, device string, now time.Time) bool {
	if tok == "" || len(tok) > 128 {
		return false
	}
	tt.mu.Lock()
	defer tt.mu.Unlock()
	t, ok := tt.m[sha256.Sum256([]byte(tok))]
	return ok && t.session == session && t.device == device && now.Before(t.expires)
}

// liveTerminal checks that the core has a terminal and that path id names
// (exactly) a session with a live terminal. It writes the error itself.
func (s *Server) liveTerminal(w http.ResponseWriter, r *http.Request) (core.Terminal, string, bool) {
	tc, ok := s.d.Core.(core.Terminal)
	if !ok {
		writeUnsupported(w, "the live terminal is not available here")
		return nil, "", false
	}
	id := r.PathValue("id")
	sess, err := s.d.Core.Session(r.Context(), id)
	if err != nil {
		s.writeCoreErr(w, err)
		return nil, "", false
	}
	if sess.ID != id { // a prefix: tokens and signatures name full ids
		writeErr(w, http.StatusNotFound, "not_found", "not found")
		return nil, "", false
	}
	if sess.ExitedAt != nil {
		writeErr(w, http.StatusBadRequest, "invalid", "this session has ended")
		return nil, "", false
	}
	if !sess.Caps.Watch {
		writeUnsupported(w, "this session has no live terminal: only sessions started with `pilothouse run` (by a current pilothouse) have one")
		return nil, "", false
	}
	return tc, id, true
}

type terminalTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Server) handleTerminalToken(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.d.Core.(core.Terminal); !ok {
		writeUnsupported(w, "the live terminal is not available here")
		return
	}
	id := r.PathValue("id")
	var b struct{}
	if !s.readSigned(w, r, ActionTerminal, id, &b) {
		return
	}
	if _, _, ok := s.liveTerminal(w, r); !ok {
		return
	}
	d := deviceOf(r)
	tok, exp, err := s.terminals.mint(id, d.ID, s.now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "could not create a token")
		return
	}
	s.log.Info("terminal opened", "session", id, "by", by(r))
	writeJSON(w, http.StatusOK, terminalTokenResponse{Token: tok, ExpiresAt: exp.UTC()})
}

func (s *Server) handleTerminalStream(w http.ResponseWriter, r *http.Request) {
	tc, id, ok := s.liveTerminal(w, r)
	if !ok {
		return
	}
	replay := r.URL.Query().Get("replay") == "1"
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	d := deviceOf(r)
	key := s.addStream(d.ID, cancel) // revoking the device ends it
	defer s.removeStream(d.ID, key)

	evc := make(chan core.TerminalEvent, terminalBuffer)
	errc := make(chan error, 1)
	push := func(ev core.TerminalEvent) error {
		select {
		case evc <- ev:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	go func() {
		if st, ok := tc.(core.TerminalStreamer); ok {
			errc <- st.StreamTerminal(ctx, id, replay, push)
			return
		}
		err := tc.WatchTerminal(ctx, id, replay, func(b []byte) error { return push(core.TerminalEvent{Data: b}) })
		if err == nil && ctx.Err() == nil {
			err = push(core.TerminalEvent{Exited: true, ExitCode: -1})
		}
		errc <- err
	}()

	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{}) // long-lived
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if rc.Flush() != nil {
		return
	}
	ping := time.NewTicker(s.d.PingEvery)
	defer ping.Stop()
	write := func(ev core.TerminalEvent) (exited bool, err error) {
		var buf bytes.Buffer
		if ev.Cols > 0 && ev.Rows > 0 {
			b, _ := json.Marshal(map[string]uint16{"cols": ev.Cols, "rows": ev.Rows})
			fmt.Fprintf(&buf, "event: size\ndata: %s\n\n", b)
		}
		if len(ev.Data) > 0 {
			fmt.Fprintf(&buf, "event: data\ndata: %s\n\n", base64.StdEncoding.EncodeToString(ev.Data))
		}
		if ev.Exited {
			b, _ := json.Marshal(map[string]int{"code": ev.ExitCode})
			fmt.Fprintf(&buf, "event: exit\ndata: %s\n\n", b)
		}
		if _, err := w.Write(buf.Bytes()); err != nil {
			return false, err
		}
		return ev.Exited, nil
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\nevent: ping\ndata: {}\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		case ev := <-evc:
			exited, err := write(ev)
			if err != nil {
				return
			}
			// Flush when nothing else is queued: bursts go out together.
			if exited || len(evc) == 0 {
				if rc.Flush() != nil {
					return
				}
			}
			if exited {
				return
			}
		case err := <-errc:
			for len(evc) > 0 {
				if exited, werr := write(<-evc); werr != nil || exited {
					_ = rc.Flush()
					return
				}
			}
			if err != nil && ctx.Err() == nil {
				b, _ := json.Marshal(api.Error{Error: streamEndReason(err), Code: "stream_ended"})
				fmt.Fprintf(w, "event: error\ndata: %s\n\n", b)
			}
			_ = rc.Flush()
			return
		}
	}
}

// streamEndReason is the person-readable part of a stream error: core
// validation messages as they are, anything else (socket errors) generic.
func streamEndReason(err error) string {
	for _, sentinel := range []error{core.ErrInvalid, core.ErrUnsupported} {
		if errors.Is(err, sentinel) {
			return strings.TrimPrefix(err.Error(), sentinel.Error()+": ")
		}
	}
	return "the terminal stream ended; reconnect to continue"
}

// terminalInput is the body of POST /api/sessions/{id}/terminal/input.
type terminalInput struct {
	Data   *string `json:"data,omitempty"` // base64 (standard, padded)
	Resize *struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	} `json:"resize,omitempty"`
}

func (s *Server) handleTerminalInput(w http.ResponseWriter, r *http.Request) {
	tc, ok := s.d.Core.(core.Terminal)
	if !ok {
		writeUnsupported(w, "the live terminal is not available here")
		return
	}
	id := r.PathValue("id")
	if !s.terminals.valid(r.Header.Get("X-Terminal-Token"), id, deviceOf(r).ID, s.now()) {
		writeErr(w, http.StatusForbidden, "terminal_token", "the terminal token is missing, expired or for another session; open the terminal again")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, int64(base64.StdEncoding.EncodedLen(maxTerminalInput)+1024))
	var b terminalInput
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	var err error
	switch {
	case (b.Data == nil) == (b.Resize == nil):
		writeErr(w, http.StatusBadRequest, "bad_request", "send either data or resize")
		return
	case b.Data != nil:
		data, derr := base64.StdEncoding.DecodeString(*b.Data)
		if derr != nil || len(data) > maxTerminalInput {
			writeErr(w, http.StatusBadRequest, "bad_request", "data must be base64, at most 64 KB decoded")
			return
		}
		err = tc.TerminalInput(r.Context(), id, data, by(r))
	default:
		c, rows := b.Resize.Cols, b.Resize.Rows
		if c < 1 || rows < 1 || c > maxTerminalSize || rows > maxTerminalSize {
			writeErr(w, http.StatusBadRequest, "bad_request", "cols and rows must be 1-1000")
			return
		}
		err = tc.ResizeTerminal(r.Context(), id, uint16(c), uint16(rows))
	}
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
