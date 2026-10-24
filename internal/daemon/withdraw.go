package daemon

// Workstream F: withdraw notifications when requests close (docs/PLAN-0.2.md
// F9, "Push withdrawal" contract). Every request.closed event for a request
// that was held for devices goes to the notifier's Withdraw, which tells
// each remote channel that notified it: Web Push replaces the notification
// ("Answered ✓"), ntfy clears it, Telegram edits the message.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

// withdrawer is implemented by *notify.Notifier. Injected notifiers (tests)
// may not implement it; then nothing is withdrawn.
type withdrawer interface {
	Withdraw(ctx context.Context, r api.Request) error
}

// watchClosedRequests follows the event log for closed requests until
// shutdown.
func (d *Daemon) watchClosedRequests() {
	w, ok := d.notifier.(withdrawer)
	if !ok {
		return
	}
	d.goBackground(func() {
		for d.ctx.Err() == nil {
			after, err := d.db.MaxSeq(d.ctx)
			if err != nil {
				d.pause(time.Second)
				continue
			}
			sub, err := d.Subscribe(d.ctx, after)
			if err != nil {
				d.pause(time.Second)
				continue
			}
			for ev := range sub.C {
				if ev.Type != api.EventRequestClosed {
					continue
				}
				var r api.Request
				if json.Unmarshal(ev.Data, &r) != nil || r.Route != api.RouteDevices || r.Status == api.RequestOpen {
					continue
				}
				if err := w.Withdraw(d.ctx, r); err != nil && d.ctx.Err() == nil {
					d.log.Warn("withdraw notifications", "request", r.ID, "err", err)
				}
			}
			sub.Close()
			// The stream ended: shutdown, or this watcher fell behind and
			// resubscribes from now (a missed withdrawal only leaves a
			// stale notification whose buttons answer "already decided").
			if sub.Resync {
				d.pause(100 * time.Millisecond)
			}
		}
	})
}

// pause waits for d or shutdown.
func (d *Daemon) pause(dur time.Duration) {
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-d.ctx.Done():
	case <-t.C:
	}
}
