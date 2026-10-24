package daemon

import (
	"context"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/store"
)

// Replies and the live terminal (F2, F3; workstream B).

var (
	_ core.Replier  = (*Daemon)(nil)
	_ core.Terminal = (*Daemon)(nil)
)

// Reply delivers a follow-up prompt or an interrupt to a session.
//
// Workstream E stand-in until B's dispatcher lands: only plugin sessions
// (pluginReply) are served here; B's Reply replaces this body and keeps
// the plugin branch (sessions of plugin agents go to pluginReply, which
// says precisely why a reply can't be delivered).
func (d *Daemon) Reply(ctx context.Context, sessionID string, r api.Reply, by string) (api.ReplyResult, error) {
	full, err := d.db.ResolveSessionID(ctx, sessionID)
	if err != nil {
		return api.ReplyResult{}, err
	}
	s, err := d.db.Session(ctx, full)
	if err != nil {
		return api.ReplyResult{}, err
	}
	if !isPluginAgent(s.Agent) {
		return api.ReplyResult{}, core.ErrUnsupported
	}
	res, err := d.pluginReply(ctx, s, r, by)
	if err != nil {
		return res, err
	}
	rec := api.ReplyRecord{SessionID: s.ID, Text: r.Text, Interrupt: r.Interrupt, Via: res.Via, By: by, At: d.now()}
	if _, err := d.Emit(ctx, api.EventSessionReply, s.ID, "", rec); err != nil {
		d.log.Warn("record reply", "session", s.ID, "err", err)
	}
	return res, nil
}

// WatchTerminal streams a wrapped session's terminal output.
func (d *Daemon) WatchTerminal(ctx context.Context, sessionID string, replay bool, out func([]byte) error) error {
	return core.ErrUnsupported
}

// TerminalInput types into a wrapped session's terminal.
func (d *Daemon) TerminalInput(ctx context.Context, sessionID string, data []byte, by string) error {
	return core.ErrUnsupported
}

// ResizeTerminal resizes a wrapped session's terminal when nobody is attached.
func (d *Daemon) ResizeTerminal(ctx context.Context, sessionID string, cols, rows uint16) error {
	return core.ErrUnsupported
}

// replyCaps sets Reply/ReplyVia/Interrupt/Watch for wrapped sessions and
// for observed sessions with a messaging endpoint.
func (d *Daemon) replyCaps(ctx context.Context, r store.Reader, s api.Session, c *api.SessionCaps) {}
