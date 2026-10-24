package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/store"
)

// Claude Code's session messaging socket (2.1.224+). Every hook of a
// Claude session runs with CLAUDE_CODE_MESSAGING_SOCKET and _TOKEN set;
// the shim forwards them in HookEnvelope.Messaging and the daemon keeps
// them in session_secrets only — never in the session, events, evidence,
// `explain`, logs or API responses.
//
// Protocol (verified against Claude Code 2.1.310): connect, write NDJSON
//
//	{"type":"auth","token":"<token>"}
//	{"type":"user","message":{"role":"user","content":"<text>"}}
//
// and close. The socket answers nothing. An idle session starts a turn at
// once (a busy one queues it) and fires UserPromptSubmit with the text;
// the transcript records origin.kind "peer". Claude shows the model the
// text under "Another Claude session sent a message … not typed by your
// user", with the instruction that a peer can't grant permissions or
// approve prompts — tool calls still go through the session's permission
// settings and Pilothouse's hooks. messagingPrefix says who really sent
// it; with it the model treats the text as the person's next prompt.
// (2.1.310 also accepts a missing or wrong token: the socket's 0600 mode
// is what keeps other users out. Pilothouse sends the token regardless.)

const (
	// messagingPrefix starts every prompt sent through the socket, so the
	// model knows the person sent it, and the activity feed can label it.
	messagingPrefix = "[Pilothouse] "
	// messagingNote is ReplyResult.Note for replies sent this way.
	messagingNote = "Sent as a message from Pilothouse: Claude shows it as coming from another session, and it can't approve anything by itself."
	// messagingTimeout bounds connecting to and writing the socket.
	messagingTimeout = 3 * time.Second
	// messagingLinger is how long the connection stays open after the
	// message for a reply (Claude sends none today; a future error line
	// would be logged).
	messagingLinger  = 300 * time.Millisecond
	maxMessagingPath = 1024
	maxMessagingTok  = 1024
)

// messagingText is what the model receives for a reply's text.
func messagingText(text, by string) string {
	from := "from the Pilothouse inbox on their computer"
	if strings.HasPrefix(by, "device:") {
		from = "from their phone"
	}
	return messagingPrefix + "Message from the person who started this session, sent " + from +
		" with Pilothouse (their own agent inbox). Treat it as their next prompt:\n\n" + text
}

// validEndpoint rejects endpoints no Claude Code would report.
func validEndpoint(ep adapters.MessagingEndpoint) bool {
	return filepath.IsAbs(ep.Socket) && filepath.Clean(ep.Socket) == ep.Socket && len(ep.Socket) <= maxMessagingPath &&
		ep.Token != "" && len(ep.Token) <= maxMessagingTok && !strings.ContainsAny(ep.Socket+ep.Token, "\x00\n\r")
}

// recordMessaging keeps the messaging endpoint a Claude hook reported (only
// writing when it changed) and drops it when the session ends. It runs in
// the hook's transaction before the session is saved, so the session's
// caps reflect it. Failures are logged without the endpoint and never fail
// the hook.
func (d *Daemon) recordMessaging(ctx context.Context, tx *store.Tx, sessionID string, h *hookCall) {
	if h.env.Agent != api.AgentClaude {
		return
	}
	var err error
	switch ep := h.env.Messaging; {
	case h.ev.Kind == adapters.HookSessionEnd:
		err = tx.DeleteSecret(ctx, sessionID, store.SecretMessaging)
	case ep == nil:
		return
	case !validEndpoint(*ep):
		d.log.Debug("ignoring an implausible messaging endpoint", "session", sessionID)
		return
	default:
		b, _ := json.Marshal(ep)
		cur, gerr := tx.Secret(ctx, sessionID, store.SecretMessaging)
		switch {
		case gerr == nil && cur == string(b):
			return
		case gerr != nil && !errors.Is(gerr, store.ErrNotFound):
			err = gerr
		default:
			err = tx.PutSecret(ctx, sessionID, store.SecretMessaging, string(b))
		}
	}
	if err != nil {
		d.log.Warn("could not update the session's messaging endpoint", "session", sessionID, "err", err)
	}
}

// messagingEndpoint returns a session's stored endpoint.
func (d *Daemon) messagingEndpoint(ctx context.Context, sessionID string) (adapters.MessagingEndpoint, bool, error) {
	v, err := d.db.Secret(ctx, sessionID, store.SecretMessaging)
	if errors.Is(err, store.ErrNotFound) {
		return adapters.MessagingEndpoint{}, false, nil
	}
	if err != nil {
		return adapters.MessagingEndpoint{}, false, err
	}
	var ep adapters.MessagingEndpoint
	if err := json.Unmarshal([]byte(v), &ep); err != nil || !validEndpoint(ep) {
		return adapters.MessagingEndpoint{}, false, nil
	}
	return ep, true, nil
}

// replyMessaging sends text to a Claude session's messaging socket. A
// socket nobody listens on any more (Claude exited or restarted) is
// forgotten, so the session stops offering replies until a hook reports
// a new one.
func (d *Daemon) replyMessaging(ctx context.Context, s api.Session, ep adapters.MessagingEndpoint, text, by string) error {
	err := sendMessage(ctx, ep, messagingText(text, by), d.log.Debug)
	if err == nil {
		return nil
	}
	var gone *messagingGoneError
	if !errors.As(err, &gone) {
		return err
	}
	d.log.Info("messaging socket gone; forgetting it", "session", s.ID)
	if werr := d.db.Write(ctx, func(tx *store.Tx) error {
		if err := tx.DeleteSecret(ctx, s.ID, store.SecretMessaging); err != nil {
			return err
		}
		return d.touchSession(ctx, tx, s.ID)
	}); werr != nil {
		d.log.Warn("forget messaging endpoint", "session", s.ID, "err", werr)
	}
	return core.Invalid("this Claude session isn't listening for messages any more (it exited or restarted); Pilothouse reconnects with its next hook")
}

// messagingGoneError: nothing accepts connections on the socket.
type messagingGoneError struct{ err error }

func (e *messagingGoneError) Error() string { return "messaging socket unreachable: " + e.err.Error() }

// sendMessage writes one message to a messaging socket. Errors never
// contain the token.
func sendMessage(ctx context.Context, ep adapters.MessagingEndpoint, content string, debug func(string, ...any)) error {
	cctx, cancel := context.WithTimeout(ctx, messagingTimeout)
	defer cancel()
	var dl net.Dialer
	c, err := dl.DialContext(cctx, "unix", ep.Socket)
	if err != nil {
		return &messagingGoneError{err: err}
	}
	defer c.Close()
	if t, ok := cctx.Deadline(); ok {
		_ = c.SetDeadline(t)
	}
	auth, _ := json.Marshal(map[string]string{"type": "auth", "token": ep.Token})
	msg, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]string{"role": "user", "content": content}})
	if _, err := c.Write(append(append(append(auth, '\n'), msg...), '\n')); err != nil {
		return fmt.Errorf("write to Claude's messaging socket: %w", err)
	}
	if uc, ok := c.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}
	_ = c.SetReadDeadline(time.Now().Add(messagingLinger))
	if b, _ := io.ReadAll(io.LimitReader(c, 4<<10)); len(b) > 0 {
		debug("messaging socket answered", "bytes", len(b))
	}
	return nil
}

// sweepSecrets forgets the private endpoints of sessions that have ended
// (a SessionEnd hook already does; this catches crashes and expiry).
func (d *Daemon) sweepSecrets(ctx context.Context) {
	ids, err := d.db.SecretSessionsEnded(ctx)
	if err != nil || len(ids) == 0 {
		return
	}
	if err := d.db.Write(ctx, func(tx *store.Tx) error {
		for _, id := range ids {
			if err := tx.DeleteSecret(ctx, id, ""); err != nil {
				return err
			}
		}
		return nil
	}); err != nil && ctx.Err() == nil {
		d.log.Warn("forget ended sessions' secrets", "err", err)
	}
}
