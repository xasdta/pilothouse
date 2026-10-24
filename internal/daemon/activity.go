package daemon

import (
	"context"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
)

// Activity feeds and insights (F3, F6; workstream C).

var (
	_ core.ActivityReader = (*Daemon)(nil)
	_ core.InsightsReader = (*Daemon)(nil)
)

// Activity reads a session's transcript into a feed.
func (d *Daemon) Activity(ctx context.Context, sessionID string, q api.ActivityQuery) (api.Activity, error) {
	return api.Activity{}, core.ErrUnsupported
}

// Insights aggregates usage and decisions over a range of days.
func (d *Daemon) Insights(ctx context.Context, q api.InsightsQuery) (api.Insights, error) {
	return api.Insights{}, core.ErrUnsupported
}
