package daemon

import (
	"context"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/store"
)

// capsFor computes what a person can do with a session; saveSession
// stores it with the session so snapshots and events carry it. Each
// capability is filled in by the file that implements it.
func (d *Daemon) capsFor(ctx context.Context, r store.Reader, s api.Session) api.SessionCaps {
	var c api.SessionCaps
	if s.State == api.StateExited {
		return c
	}
	d.replyCaps(ctx, r, s, &c)    // reply.go
	d.pluginCaps(ctx, r, s, &c)   // plugin.go
	d.activityCaps(ctx, r, s, &c) // activity.go
	return c
}
