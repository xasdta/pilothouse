package daemon

import (
	"context"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/store"
)

// Agents integrated through a plugin running inside the agent (OpenCode;
// workstream E). Replies travel daemon → plugin over a command channel
// the plugin keeps open.

// pluginReply delivers a reply through the session's plugin channel.
// Reply (reply.go) calls it for sessions whose agent uses a plugin.
func (d *Daemon) pluginReply(ctx context.Context, s api.Session, r api.Reply, by string) (api.ReplyResult, error) {
	return api.ReplyResult{}, core.ErrUnsupported
}

// pluginCaps sets Reply via plugin while the session's plugin is connected.
func (d *Daemon) pluginCaps(ctx context.Context, r store.Reader, s api.Session, c *api.SessionCaps) {}
