package daemon

import (
	"context"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
)

// Replies and the live terminal (F2, F3; workstream B).

var (
	_ core.Replier  = (*Daemon)(nil)
	_ core.Terminal = (*Daemon)(nil)
)

// Reply delivers a follow-up prompt or an interrupt to a session.
func (d *Daemon) Reply(ctx context.Context, sessionID string, r api.Reply, by string) (api.ReplyResult, error) {
	return api.ReplyResult{}, core.ErrUnsupported
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
