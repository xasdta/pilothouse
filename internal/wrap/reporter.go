package wrap

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/client"
)

// reporter keeps the daemon informed about one wrapped session. It owns
// every daemon call the holder makes so the PTY copy loop never waits on
// the daemon: the loop only stores the latest OSC state under a mutex.
//
// The daemon is optional. Until registration succeeds (retried every
// retryEvery), OSC reports are held back. While registered, the reporter
// checks the daemon's health every healthEvery: if the daemon went away,
// or came back as a new process (its pid changed, e.g. after a restart
// between two checks), it registers again (registration is idempotent)
// and re-sends the latest state, so a restarted daemon catches up even
// when the agent has nothing new to report.
type reporter struct {
	c           *client.Client
	body        api.WrappedSessionBody
	log         *slog.Logger
	retryEvery  time.Duration
	healthEvery time.Duration
	callLimit   time.Duration

	mu   sync.Mutex
	osc  *api.OSCBody // latest unsent OSC report (coalesced)
	last *api.OSCBody // latest OSC report the daemon accepted
	exit *int         // pending exit report
	// exitDelivered is set once the daemon has the exit (or rejected it
	// for good); nothing is reported after that.
	exitDelivered bool

	// everRegistered tells the holder whether a daemon has been seen at
	// all, so a holder stopped with no daemon doesn't wait to report.
	everRegistered atomic.Bool
	wake           chan struct{}
	stopped        chan struct{}
}

// reporterHealthEvery is how often a registered reporter checks that the
// daemon it registered with is still the one running (a var for tests).
var reporterHealthEvery = 5 * time.Second

func newReporter(c *client.Client, body api.WrappedSessionBody, log *slog.Logger) *reporter {
	return &reporter{
		c: c, body: body, log: log,
		retryEvery:  2 * time.Second,
		healthEvery: reporterHealthEvery,
		callLimit:   2 * time.Second,
		wake:        make(chan struct{}, 1),
		stopped:     make(chan struct{}),
	}
}

// OSC records the latest status; never blocks.
func (r *reporter) OSC(b api.OSCBody) {
	r.mu.Lock()
	r.osc = &b
	r.mu.Unlock()
	r.poke()
}

// Exit records the child's exit code; never blocks.
func (r *reporter) Exit(code int) {
	r.mu.Lock()
	r.exit = &code
	r.mu.Unlock()
	r.poke()
}

func (r *reporter) poke() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Done is closed when run returns.
func (r *reporter) Done() <-chan struct{} { return r.stopped }

// run reports until ctx ends or the exit has been delivered.
func (r *reporter) run(ctx context.Context) {
	defer close(r.stopped)
	registered := false
	daemonPID := 0 // pid of the daemon registered with; 0 = unknown
	timer := time.NewTimer(0)
	defer timer.Stop()
	checkHealth := false
	for {
		if registered && checkHealth {
			if pid, gone := r.health(ctx); gone {
				r.log.Info("daemon unreachable; will register again when it is back")
				registered = false
			} else if pid != 0 && daemonPID != 0 && pid != daemonPID {
				r.log.Info("daemon restarted; registering again", "old_pid", daemonPID, "pid", pid)
				registered = false
			}
		}
		checkHealth = false
		if !registered {
			if err := r.call(ctx, func(cctx context.Context) error {
				_, err := r.c.RegisterWrapped(cctx, r.body)
				return err
			}); err == nil {
				registered = true
				if r.everRegistered.Swap(true) {
					r.resendLast()
					r.log.Info("registered with daemon again")
				} else {
					r.log.Info("registered with daemon")
				}
				daemonPID, _ = r.health(ctx)
			} else if ctx.Err() == nil {
				r.log.Debug("daemon registration failed; will retry", "err", err)
			}
		}
		if registered {
			registered = r.flush(ctx)
			r.mu.Lock()
			done := r.exitDelivered
			r.mu.Unlock()
			if done {
				return
			}
		}
		if registered {
			timer.Reset(r.healthEvery)
		} else {
			timer.Reset(r.retryEvery)
		}
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-timer.C:
			checkHealth = true
		}
	}
}

// health asks the daemon for its pid. gone is true when the daemon could
// not be reached at all; a daemon that answers with an error is still
// there (pid 0 = unknown).
func (r *reporter) health(ctx context.Context) (pid int, gone bool) {
	var h api.Health
	err := r.call(ctx, func(cctx context.Context) error {
		var err error
		h, err = r.c.Health(cctx)
		return err
	})
	if err != nil {
		var ae *client.APIError
		return 0, !errors.As(err, &ae) && ctx.Err() == nil
	}
	return h.PID, false
}

// resendLast queues the last accepted OSC state again, unless a newer one
// is already waiting, so a daemon that lost track of the session (restart,
// DB reset) learns its current state without waiting for the agent to
// report a change.
func (r *reporter) resendLast() {
	r.mu.Lock()
	if r.osc == nil && r.last != nil {
		r.osc = r.last
	}
	r.mu.Unlock()
}

// flush sends the pending OSC and exit reports. It returns false when the
// daemon has to be registered with again.
func (r *reporter) flush(ctx context.Context) bool {
	for {
		r.mu.Lock()
		osc, exit := r.osc, r.exit
		r.mu.Unlock()
		var err error
		switch {
		case osc != nil:
			err = r.call(ctx, func(cctx context.Context) error { return r.c.ReportOSC(cctx, r.body.SessionID, *osc) })
			if err == nil || isPermanent(err) {
				r.mu.Lock()
				if r.osc == osc { // a newer one may have arrived meanwhile
					r.osc = nil
				}
				if err == nil {
					r.last = osc
				}
				r.mu.Unlock()
			}
		case exit != nil:
			err = r.call(ctx, func(cctx context.Context) error { return r.c.ReportExit(cctx, r.body.SessionID, *exit) })
			if err == nil || isPermanent(err) {
				r.mu.Lock()
				r.exit = nil
				r.exitDelivered = true
				r.mu.Unlock()
			}
		default:
			return true
		}
		if err != nil {
			if isPermanent(err) {
				r.log.Warn("daemon rejected report", "err", err)
				continue
			}
			// Daemon down or session unknown (e.g. daemon DB reset):
			// register again, then retry.
			r.log.Debug("report failed; re-registering", "err", err)
			return false
		}
	}
}

func (r *reporter) call(ctx context.Context, fn func(context.Context) error) error {
	cctx, cancel := context.WithTimeout(ctx, r.callLimit)
	defer cancel()
	return fn(cctx)
}

// isPermanent reports whether retrying the same report can never succeed.
// not_found is not permanent: it means the daemon forgot the session and
// a re-registration will fix it.
func isPermanent(err error) bool {
	var ae *client.APIError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Status/100 == 4 && ae.Code != "not_found" && ae.Status != 404
}
