package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/store"
	"github.com/xasdta/pilothouse/internal/wrap"
)

// Replies and the live terminal (F2, F3; workstream B).
//
// A reply reaches a session the best way it can:
//
//   - wrapped sessions (`pilothouse run`): typed into the agent's terminal
//     through the holder, the text as one bracketed paste and then Enter
//     (an interrupt is Esc first);
//   - observed Claude Code sessions: Claude's own messaging socket, whose
//     path and token the hook shim reported (messaging.go);
//   - agents integrated through a plugin (OpenCode): pluginReply.
//
// Every reply is recorded as a session.reply event (api.ReplyRecord).

var (
	_ core.Replier          = (*Daemon)(nil)
	_ core.Terminal         = (*Daemon)(nil)
	_ core.TerminalStreamer = (*Daemon)(nil)
)

const (
	// maxReplyBytes bounds a follow-up prompt (as web.MaxReplyBytes).
	maxReplyBytes = 16 << 10
	// interruptPause lets the agent act on Esc before more input arrives;
	// right after a lone Esc, the ESC that starts a paste would read as
	// Alt-Esc.
	interruptPause = 600 * time.Millisecond
	// submitPause separates a paste from the Enter that submits it, so the
	// agent has taken the whole paste in before it sees Enter.
	submitPause = 200 * time.Millisecond
)

// replyState is the daemon's in-memory state for workstream B.
type replyState struct {
	mu      sync.Mutex
	holders map[string][]string    // session id → its holder's caps (learnHolder)
	inputs  map[string]*holderLink // session id → open input connection (terminal.go)
	locks   map[string]*sync.Mutex // session id → serializes replies

	awake keepAwake // keepawake.go
}

// sessionLock serializes replies to one session, so two replies can't
// interleave their keystrokes.
func (rs *replyState) sessionLock(id string) *sync.Mutex {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.locks == nil {
		rs.locks = map[string]*sync.Mutex{}
	}
	l := rs.locks[id]
	if l == nil {
		l = &sync.Mutex{}
		rs.locks[id] = l
	}
	return l
}

// Reply delivers a follow-up prompt or an interrupt to a session.
func (d *Daemon) Reply(ctx context.Context, sessionID string, r api.Reply, by string) (api.ReplyResult, error) {
	r.Text = strings.TrimRight(r.Text, " \t\r\n")
	switch {
	case by == "":
		return api.ReplyResult{}, badRequest("reply has no author")
	case r.Text == "" && !r.Interrupt:
		return api.ReplyResult{}, badRequest("nothing to send")
	case len(r.Text) > maxReplyBytes:
		return api.ReplyResult{}, badRequest("message is too long (max %d KB)", maxReplyBytes>>10)
	case !utf8.ValidString(r.Text):
		return api.ReplyResult{}, badRequest("message is not valid UTF-8")
	}
	id, err := d.db.ResolveSessionID(ctx, sessionID)
	if err != nil {
		return api.ReplyResult{}, err
	}
	s, err := d.db.Session(ctx, id)
	if err != nil {
		return api.ReplyResult{}, err
	}
	if s.ExitedAt != nil {
		return api.ReplyResult{}, core.Invalid("this session has ended")
	}

	lock := d.replies.sessionLock(s.ID)
	lock.Lock()
	defer lock.Unlock()

	res, err := d.deliverReply(ctx, s, r, by)
	if err != nil {
		return api.ReplyResult{}, err
	}
	rec := api.ReplyRecord{SessionID: s.ID, Text: r.Text, Interrupt: r.Interrupt, Via: res.Via, By: by}
	if err := d.db.Write(ctx, func(tx *store.Tx) error {
		rec.At = tx.Now()
		_, err := tx.Emit(ctx, api.EventSessionReply, s.ID, "", rec)
		return err
	}); err != nil {
		// Delivered already: a missing record must not turn into a retry.
		d.log.Error("reply sent but not recorded", "session", s.ID, "err", err)
	}
	d.log.Info("reply sent", "session", s.ID, "via", res.Via, "by", by, "interrupt", r.Interrupt, "bytes", len(r.Text))
	return res, nil
}

// deliverReply picks the channel for s and sends r through it.
func (d *Daemon) deliverReply(ctx context.Context, s api.Session, r api.Reply, by string) (api.ReplyResult, error) {
	var termErr error
	if s.Mode == api.ModeWrapped {
		termErr = d.replyTerminal(ctx, s, r)
		if termErr == nil {
			return api.ReplyResult{Via: api.ReplyTerminal}, nil
		}
		if !errors.Is(termErr, core.ErrUnsupported) {
			return api.ReplyResult{}, termErr
		}
		// A holder too old for input: Claude's messaging socket may still work.
	}
	if s.Agent == api.AgentClaude {
		ep, ok, err := d.messagingEndpoint(ctx, s.ID)
		if err != nil {
			return api.ReplyResult{}, err
		}
		if ok {
			if r.Interrupt {
				return api.ReplyResult{}, fmt.Errorf("%w: Pilothouse can only interrupt Claude sessions started with `pilothouse run claude`; send the message without interrupting", core.ErrUnsupported)
			}
			if err := d.replyMessaging(ctx, s, ep, r.Text, by); err != nil {
				return api.ReplyResult{}, err
			}
			return api.ReplyResult{Via: api.ReplyMessage, Note: messagingNote}, nil
		}
	}
	res, err := d.pluginReply(ctx, s, r, by)
	if err == nil {
		return res, nil
	}
	if err != core.ErrUnsupported { // a plugin's own reason (or failure) wins
		return api.ReplyResult{}, err
	}
	if termErr != nil {
		return api.ReplyResult{}, termErr
	}
	return api.ReplyResult{}, fmt.Errorf("%w: %s", core.ErrUnsupported, noReplyReason(s))
}

// noReplyReason says what would make replies to s possible.
func noReplyReason(s api.Session) string {
	switch s.Agent {
	case api.AgentClaude:
		return "Pilothouse hasn't learned this Claude session's messaging socket yet: it needs Claude Code 2.1.224 or later and arrives with the session's next hook. " +
			"Or start Claude with `pilothouse run claude` to reply from your phone"
	case api.AgentCodex:
		return "Run Codex with `pilothouse run codex` to reply from your phone"
	}
	return "Replies need a session started with `pilothouse run`"
}

// replyTerminal types r into a wrapped session's terminal: Esc first for
// an interrupt, then the text as one paste (bracketed when the agent
// asked for it, so a multi-line prompt doesn't submit at its first line)
// and Enter.
func (d *Daemon) replyTerminal(ctx context.Context, s api.Session, r api.Reply) error {
	return d.withInput(ctx, s, func(c *wrap.Conn) error {
		if r.Interrupt {
			if err := c.Send(wrap.FrameData, []byte{0x1b}); err != nil {
				return err
			}
			if r.Text == "" {
				return nil
			}
			if err := sleepCtx(ctx, interruptPause); err != nil {
				return err
			}
		}
		if err := c.Send(wrap.FramePaste, []byte(r.Text)); err != nil {
			return err
		}
		if err := sleepCtx(ctx, submitPause); err != nil {
			return err
		}
		return c.Send(wrap.FrameData, []byte{'\r'})
	})
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// replyCaps sets Reply/ReplyVia/Interrupt/Watch for wrapped sessions and
// for observed sessions with a messaging endpoint. It runs inside the
// transaction that saves s, so it reads what the holder supports from
// memory (learnHolder) instead of probing.
func (d *Daemon) replyCaps(ctx context.Context, r store.Reader, s api.Session, c *api.SessionCaps) {
	if s.Mode == api.ModeWrapped {
		caps, known := d.replies.holderCaps(s.ID)
		switch {
		case known:
			if hasCap(caps, wrap.CapInput) {
				c.Reply, c.ReplyVia, c.Interrupt = true, api.ReplyTerminal, true
			}
			c.Watch = hasCap(caps, wrap.CapWatch)
		case s.Caps.ReplyVia == api.ReplyTerminal || s.Caps.Watch:
			// Not probed since this daemon started (its holder registers
			// again within seconds): keep what was known.
			c.Reply, c.ReplyVia, c.Interrupt, c.Watch = s.Caps.Reply, s.Caps.ReplyVia, s.Caps.Interrupt, s.Caps.Watch
		}
	}
	if !c.Reply && s.Agent == api.AgentClaude {
		if _, err := r.Secret(ctx, s.ID, store.SecretMessaging); err == nil {
			c.Reply, c.ReplyVia = true, api.ReplyMessage
		}
	}
}

func hasCap(caps []string, c string) bool {
	for _, x := range caps {
		if x == c {
			return true
		}
	}
	return false
}

// startReplyLoops starts workstream B's background work: keep-awake, the
// hourly removal of messaging endpoints whose session ended, and closing
// open terminal connections at shutdown.
func (d *Daemon) startReplyLoops() {
	d.startKeepAwake()
	d.every(time.Hour, d.sweepSecrets)
	d.goBackground(func() {
		<-d.ctx.Done()
		d.closeInputs()
	})
}
