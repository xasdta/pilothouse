package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/paths"
	"github.com/xasdta/pilothouse/internal/store"
	"github.com/xasdta/pilothouse/internal/wrap"
)

// The live terminal of wrapped sessions (F3): watch connections to the
// holder for output, one kept-open input connection per session for
// keystrokes, pastes and resizes. Holders outlive binary upgrades, so
// every connection goes through wrap.DialCap: a holder from an older
// pilothouse only ever sees a probe.

const (
	// maxTerminalInput bounds one TerminalInput call.
	maxTerminalInput = 64 << 10
	// maxTerminalSize bounds ResizeTerminal.
	maxTerminalSize = 1000
	// inputChunk is the largest data frame sent to a holder.
	inputChunk = 16 << 10
	// inputIdle closes a session's input connection after this long unused.
	inputIdle = 2 * time.Minute
	// holderTimeout bounds dialing a holder and each write to it.
	holderTimeout = 5 * time.Second
	// probeTimeout bounds the probe when a holder registers.
	probeTimeout = time.Second
)

var errOldHolder = fmt.Errorf("%w: this session was started by an older version of pilothouse, which can't do this; start it again with `pilothouse run`", core.ErrUnsupported)

// learnHolder probes the holder of a session that is registering, so
// caps (computed inside transactions) know what it supports. A holder
// that doesn't answer is forgotten.
func (d *Daemon) learnHolder(ctx context.Context, sessionID string) {
	if !validSessionID(sessionID) {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	sh, err := wrap.Probe(pctx, paths.HolderSocket(sessionID))
	if err != nil || sh.SessionID != sessionID || sh.Exited {
		d.replies.setHolderCaps(sessionID, nil, false)
		return
	}
	d.replies.setHolderCaps(sessionID, sh.Caps, true)
}

// holderCaps returns what the session's holder supports, if known.
func (rs *replyState) holderCaps(id string) ([]string, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	c, ok := rs.holders[id]
	return c, ok
}

// setHolderCaps records (known) or forgets a holder's caps. Holders whose
// socket is gone are forgotten on the way, which bounds the map.
func (rs *replyState) setHolderCaps(id string, caps []string, known bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.holders == nil {
		rs.holders = map[string][]string{}
	}
	if !known {
		delete(rs.holders, id)
		return
	}
	if caps == nil {
		caps = []string{}
	}
	rs.holders[id] = caps
	for other := range rs.holders {
		if other == id {
			continue
		}
		if _, err := os.Stat(paths.HolderSocket(other)); errors.Is(err, os.ErrNotExist) {
			delete(rs.holders, other)
		}
	}
}

// liveHolder checks that sessionID is a running wrapped session and
// returns it with its holder's socket.
func (d *Daemon) liveHolder(ctx context.Context, sessionID string) (api.Session, string, error) {
	id, err := d.db.ResolveSessionID(ctx, sessionID)
	if err != nil {
		return api.Session{}, "", err
	}
	s, err := d.db.Session(ctx, id)
	if err != nil {
		return api.Session{}, "", err
	}
	if s.Mode != api.ModeWrapped {
		return s, "", fmt.Errorf("%w: the live terminal is only available for sessions started with `pilothouse run`", core.ErrUnsupported)
	}
	if s.ExitedAt != nil {
		return s, "", core.Invalid("this session has ended")
	}
	// Only the canonical socket of this session id: the registration
	// that set HolderSock came from an unauthenticated local caller.
	sock := paths.HolderSocket(s.ID)
	if s.HolderSock != "" && s.HolderSock != sock {
		return s, "", fmt.Errorf("%w: this session's terminal is outside Pilothouse's run directory", core.ErrUnsupported)
	}
	return s, sock, nil
}

// dialHolder connects to a session's holder in a 0.2 mode.
func (d *Daemon) dialHolder(ctx context.Context, s api.Session, sock string, h wrap.Hello) (*wrap.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, holderTimeout)
	defer cancel()
	c, err := wrap.DialCap(dctx, sock, h)
	switch {
	case errors.Is(err, wrap.ErrOldHolder):
		d.noteHolderCaps(ctx, s, []string{})
		return nil, errOldHolder
	case err != nil:
		var he *wrap.HolderError
		if errors.As(err, &he) {
			return nil, core.Invalid("%s", he.Msg)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, core.Invalid("can't reach this session's terminal; it may have just ended")
	}
	if c.Hello.SessionID != s.ID {
		c.Close()
		return nil, core.Invalid("the terminal at this session's socket belongs to another session")
	}
	d.noteHolderCaps(ctx, s, c.Hello.Caps)
	return c, nil
}

// noteHolderCaps records caps learned while connecting and re-saves the
// session when they change what it offers.
func (d *Daemon) noteHolderCaps(ctx context.Context, s api.Session, caps []string) {
	old, known := d.replies.holderCaps(s.ID)
	if known && equalCaps(old, caps) {
		return
	}
	d.replies.setHolderCaps(s.ID, caps, true)
	if err := d.touchSessionNow(ctx, s.ID); err != nil {
		d.log.Debug("update session caps", "session", s.ID, "err", err)
	}
}

// touchSessionNow re-saves a session in its own transaction.
func (d *Daemon) touchSessionNow(ctx context.Context, id string) error {
	return d.db.Write(ctx, func(tx *store.Tx) error { return d.touchSession(ctx, tx, id) })
}

func equalCaps(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// WatchTerminal streams a wrapped session's terminal output. It returns
// nil when the agent exits.
func (d *Daemon) WatchTerminal(ctx context.Context, sessionID string, replay bool, out func([]byte) error) error {
	return d.StreamTerminal(ctx, sessionID, replay, func(ev core.TerminalEvent) error {
		if len(ev.Data) > 0 {
			return out(ev.Data)
		}
		return nil
	})
}

// StreamTerminal streams output, size reports and the exit of a wrapped
// session's terminal over a read-only watch connection: it never detaches
// or resizes a terminal attached with `pilothouse attach`.
func (d *Daemon) StreamTerminal(ctx context.Context, sessionID string, replay bool, ev func(core.TerminalEvent) error) error {
	s, sock, err := d.liveHolder(ctx, sessionID)
	if err != nil {
		return err
	}
	c, err := d.dialHolder(ctx, s, sock, wrap.Hello{Mode: wrap.ModeWatch, Replay: replay})
	if err != nil {
		return err
	}
	defer c.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			c.Close() // unblocks Recv
		case <-stop:
		}
	}()
	if c.Hello.Cols > 0 && c.Hello.Rows > 0 {
		if err := ev(core.TerminalEvent{Cols: c.Hello.Cols, Rows: c.Hello.Rows}); err != nil {
			return err
		}
	}
	for {
		f, err := c.Recv()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return errors.New("the terminal stream was closed (the viewer fell behind, or the session is stopping)")
			}
			return fmt.Errorf("terminal stream: %w", err)
		}
		var e core.TerminalEvent
		switch f.Type {
		case wrap.FrameData:
			e.Data = f.Payload
		case wrap.FrameResize:
			cols, rows, err := wrap.DecodeResize(f.Payload)
			if err != nil {
				continue
			}
			e.Cols, e.Rows = cols, rows
		case wrap.FrameExit:
			code, _ := wrap.DecodeExit(f.Payload)
			return ev(core.TerminalEvent{Exited: true, ExitCode: code})
		case wrap.FrameError:
			msg, _ := wrap.DecodeText(f.Payload)
			return errors.New("terminal: " + msg)
		default:
			continue
		}
		if err := ev(e); err != nil {
			return err
		}
	}
}

// TerminalInput types data into a wrapped session's terminal.
func (d *Daemon) TerminalInput(ctx context.Context, sessionID string, data []byte, by string) error {
	if len(data) > maxTerminalInput {
		return badRequest("terminal input is limited to %d KB per call", maxTerminalInput>>10)
	}
	if len(data) == 0 {
		return nil
	}
	s, _, err := d.liveHolder(ctx, sessionID)
	if err != nil {
		return err
	}
	return d.withInput(ctx, s, func(c *wrap.Conn) error {
		for b := data; len(b) > 0; {
			n := min(len(b), inputChunk)
			if err := c.Send(wrap.FrameData, b[:n]); err != nil {
				return err
			}
			b = b[n:]
		}
		return nil
	})
}

// ResizeTerminal resizes a wrapped session's terminal; the holder ignores
// it while a terminal is attached (the attached terminal's size wins).
func (d *Daemon) ResizeTerminal(ctx context.Context, sessionID string, cols, rows uint16) error {
	if cols == 0 || rows == 0 || cols > maxTerminalSize || rows > maxTerminalSize {
		return badRequest("terminal size must be 1-%d columns and rows", maxTerminalSize)
	}
	s, _, err := d.liveHolder(ctx, sessionID)
	if err != nil {
		return err
	}
	return d.withInput(ctx, s, func(c *wrap.Conn) error {
		return c.Send(wrap.FrameResize, wrap.EncodeResize(cols, rows))
	})
}

// holderLink is a session's open input connection. Keeping one per
// session (instead of one per call) keeps keystrokes from successive
// calls in order: the holder reads each connection on its own goroutine.
type holderLink struct {
	mu    sync.Mutex
	c     *wrap.Conn
	timer *time.Timer
}

// withInput runs fn on the session's input connection, dialing it when
// needed. A connection that fails is dropped and fn retried once on a
// fresh one, unless the failure was the deadline (the agent isn't reading
// its terminal; a retry would only wait again).
func (d *Daemon) withInput(ctx context.Context, s api.Session, fn func(*wrap.Conn) error) error {
	sock := paths.HolderSocket(s.ID)
	if s.HolderSock != "" && s.HolderSock != sock {
		return fmt.Errorf("%w: this session's terminal is outside Pilothouse's run directory", core.ErrUnsupported)
	}
	l := d.replies.link(s.ID)
	l.mu.Lock()
	defer l.mu.Unlock()
	for attempt := 0; ; attempt++ {
		fresh := false
		if l.c == nil {
			c, err := d.dialHolder(ctx, s, sock, wrap.Hello{Mode: wrap.ModeInput})
			if err != nil {
				return err
			}
			l.c, fresh = c, true
		}
		_ = l.c.SetWriteDeadline(time.Now().Add(holderTimeout))
		err := fn(l.c)
		if err == nil {
			l.idle(inputIdle)
			return nil
		}
		l.c.Close()
		l.c = nil
		var ne net.Error
		if fresh || attempt > 0 || ctx.Err() != nil || (errors.As(err, &ne) && ne.Timeout()) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return core.Invalid("couldn't type into this session's terminal: %v", err)
		}
	}
}

// idle (re)arms the close-when-unused timer. Callers hold l.mu.
func (l *holderLink) idle(after time.Duration) {
	if l.timer != nil {
		l.timer.Stop()
	}
	l.timer = time.AfterFunc(after, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.c != nil {
			l.c.Close()
			l.c = nil
		}
	})
}

// link returns the session's input link, creating it.
func (rs *replyState) link(id string) *holderLink {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.inputs == nil {
		rs.inputs = map[string]*holderLink{}
	}
	l := rs.inputs[id]
	if l == nil {
		l = &holderLink{}
		rs.inputs[id] = l
	}
	return l
}

// closeInputs closes every input connection (shutdown).
func (d *Daemon) closeInputs() {
	d.replies.mu.Lock()
	links := make([]*holderLink, 0, len(d.replies.inputs))
	for id, l := range d.replies.inputs {
		links = append(links, l)
		delete(d.replies.inputs, id)
	}
	d.replies.mu.Unlock()
	for _, l := range links {
		l.mu.Lock()
		if l.timer != nil {
			l.timer.Stop()
		}
		if l.c != nil {
			l.c.Close()
			l.c = nil
		}
		l.mu.Unlock()
	}
}
