package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/config"
	"github.com/xasdta/pilothouse/internal/policy"
	"github.com/xasdta/pilothouse/internal/state"
	"github.com/xasdta/pilothouse/internal/store"
)

const (
	retentionInterval  = time.Hour
	pruneFirstDelay    = time.Minute
	staleSweepInterval = time.Minute
)

// startLoops starts the periodic background work.
func (d *Daemon) startLoops() {
	poll := d.opts.PollInterval
	d.every(poll, func(ctx context.Context) {
		if err := d.updatePresence(ctx, nil); err != nil && ctx.Err() == nil {
			d.log.Warn("presence update", "err", err)
		}
	})
	d.every(poll, d.checkLiveness)
	d.every(poll, func(context.Context) {
		if d.engine().Changed() {
			d.log.Info("policy files changed; reloading")
			d.reloadPolicy()
		}
	})
	d.every(staleSweepInterval, d.sweepStale)
	d.every(staleSweepInterval, func(ctx context.Context) { d.expireIdle(ctx, d.now()) })
	d.goBackground(func() {
		// First prune shortly after start (not during it: startup should
		// be quick), then hourly.
		first := time.NewTimer(pruneFirstDelay)
		defer first.Stop()
		select {
		case <-d.ctx.Done():
			return
		case <-first.C:
		}
		d.prune(d.ctx)
		t := time.NewTicker(retentionInterval)
		defer t.Stop()
		for {
			select {
			case <-d.ctx.Done():
				return
			case <-t.C:
				d.prune(d.ctx)
			}
		}
	})
	d.startUpdateCheck()
	d.startUsage()               // usage.go (workstream C)
	d.goBackground(d.watchTasks) // taskrun.go (workstream G)
}

// ---- policy ----

func (d *Daemon) engine() PolicyEngine { return d.policy.Load().PolicyEngine }

// initPolicy builds the daemon's one policy engine. policy.Load never
// fails: broken files report errors in the status.
func (d *Daemon) initPolicy() {
	var e PolicyEngine
	if d.opts.LoadPolicy != nil {
		e = d.opts.LoadPolicy()
	} else {
		e = d.loadDefaultPolicy()
	}
	d.policy.Store(&policyHolder{e})
	d.logPolicyProblems(e.Status())
}

// reloadPolicy re-reads the policy files on the existing engine, which
// keeps each file's last good rules when an edit breaks it (QA H3: building
// a new engine here used to drop every user rule on a typo).
func (d *Daemon) reloadPolicy() api.PolicyStatus {
	st := d.engine().Reload()
	d.logPolicyProblems(st)
	return st
}

func (d *Daemon) logPolicyProblems(st api.PolicyStatus) {
	for _, msg := range st.Errors {
		d.log.Warn("policy problem", "err", msg)
	}
}

func (d *Daemon) loadDefaultPolicy() PolicyEngine {
	var protected []string
	for _, a := range d.adapters {
		protected = append(protected, a.ProtectedPaths()...)
	}
	// DefaultOptions always protects the Pilothouse home and socket directory.
	e, _ := policy.Load(policy.DefaultOptions(protected...))
	return e
}

// PolicyStatus reports the loaded policy files.
func (d *Daemon) PolicyStatus() api.PolicyStatus { return d.engine().Status() }

// ReloadPolicy re-reads every policy file now.
func (d *Daemon) ReloadPolicy() api.PolicyStatus { return d.reloadPolicy() }

// ---- liveness (layer L0) ----

// checkLiveness marks sessions whose agent process is gone as exited.
func (d *Daemon) checkLiveness(ctx context.Context) {
	sessions, err := d.db.LiveSessions(ctx)
	if err != nil {
		if ctx.Err() == nil {
			d.log.Warn("liveness check", "err", err)
		}
		return
	}
	for _, s := range sessions {
		if s.PID > 0 && !d.alive(s.PID) {
			if err := d.markExited(ctx, s.ID, "Process exited", nil); err != nil && ctx.Err() == nil {
				d.log.Warn("mark session exited", "session", s.ID, "err", err)
			}
		}
	}
}

// markExited records that a session's process ended. Requests it still had
// pending can no longer be answered, so they expire.
func (d *Daemon) markExited(ctx context.Context, id, detail string, code *int) error {
	var closed []api.Request
	err := d.db.Write(ctx, func(tx *store.Tx) error {
		s, err := tx.Session(ctx, id)
		if err != nil {
			return err
		}
		if s.ExitedAt != nil {
			// Already exited (SessionEnd usually beats the holder): keep
			// the first exit time, but a holder's exit code is still news.
			if code == nil || s.ExitCode != nil {
				return nil
			}
			before := s
			c := *code
			s.ExitCode = &c
			_, err = d.saveSession(ctx, tx, &before, s)
			return err
		}
		before := s
		now := tx.Now()
		s.ExitedAt, s.ExitCode = &now, code
		if err := tx.AddEvidence(ctx, id, api.StateEvidence{At: now, Layer: api.LayerProcess,
			State: api.StateExited, Detail: detail}); err != nil {
			return err
		}
		pending, err := tx.Requests(ctx, store.RequestFilter{SessionID: id, Statuses: store.PendingStatuses})
		if err != nil {
			return err
		}
		for _, r := range pending {
			cr, err := tx.CloseRequest(ctx, r.ID, api.RequestExpired, nil, now)
			if errors.Is(err, store.ErrAlreadyClosed) {
				continue
			}
			if err != nil {
				return err
			}
			if _, err := tx.Emit(ctx, api.EventRequestClosed, id, r.ID, cr); err != nil {
				return err
			}
			closed = append(closed, cr)
		}
		_, err = d.saveSession(ctx, tx, &before, s)
		return err
	})
	for _, r := range closed {
		d.waiters.resolve(r.ID, r)
	}
	return err
}

// sweepStale re-resolves sessions that have looked "working" for longer
// than state.StaleWorking, so silence turns into "unknown" without waiting
// for the next hook.
func (d *Daemon) sweepStale(ctx context.Context) {
	sessions, err := d.db.LiveSessions(ctx)
	if err != nil {
		return
	}
	cutoff := d.now().Add(-state.StaleWorking)
	for _, s := range sessions {
		if s.State != api.StateWorking || s.UpdatedAt.After(cutoff) {
			continue
		}
		if err := d.db.Write(ctx, func(tx *store.Tx) error { return d.touchSession(ctx, tx, s.ID) }); err != nil && ctx.Err() == nil {
			d.log.Warn("stale sweep", "session", s.ID, "err", err)
		}
	}
}

// ---- idle expiry ----

// idleExpiry is how long a session of agent may stay silent before it is
// assumed gone (0 = never).
func (d *Daemon) idleExpiry(agent api.AgentKind) time.Duration {
	exp := d.cfg.SessionIdleExpiry.Duration
	if exp > 0 && agent == api.AgentCodex && exp > config.CodexSessionIdleExpiry {
		exp = config.CodexSessionIdleExpiry
	}
	return exp
}

// expireIdle marks exited the sessions nobody can watch that went silent:
// those without a PID of their own (Codex's shared app-server runs the
// hooks of every TUI session, and /quit fires no SessionEnd) or whose PID
// several live sessions share. A session with a private, live PID is never
// expired, however idle: its process is the better evidence. Open requests
// of an expired session expire with it.
func (d *Daemon) expireIdle(ctx context.Context, now time.Time) {
	sessions, err := d.db.LiveSessions(ctx)
	if err != nil {
		return
	}
	pids := map[int]int{}
	for _, s := range sessions {
		if s.PID > 0 {
			pids[s.PID]++
		}
	}
	for _, s := range sessions {
		exp := d.idleExpiry(s.Agent)
		if exp <= 0 || (s.PID > 0 && pids[s.PID] == 1 && d.alive(s.PID)) {
			continue
		}
		last := s.UpdatedAt
		if ev, err := d.db.Evidence(ctx, s.ID, 1); err == nil && len(ev) == 1 && ev[0].At.After(last) {
			last = ev[0].At
		}
		if now.Sub(last) < exp {
			continue
		}
		detail := "No activity for " + exp.String() + "; assumed ended"
		if err := d.markExited(ctx, s.ID, detail, nil); err != nil && ctx.Err() == nil {
			d.log.Warn("expire idle session", "session", s.ID, "err", err)
			continue
		}
		d.log.Info("idle session expired", "session", s.ID, "agent", s.Agent, "idle", now.Sub(last).Round(time.Minute))
	}
}

// ---- retention ----

func (d *Daemon) prune(ctx context.Context) {
	keep := d.cfg.Retention.Duration
	if keep <= 0 {
		return
	}
	var res store.PruneResult
	err := d.db.Write(ctx, func(tx *store.Tx) error {
		var err error
		res, err = tx.Prune(ctx, tx.Now().Add(-keep))
		return err
	})
	if err != nil {
		if ctx.Err() == nil {
			d.log.Warn("retention prune", "err", err)
		}
		return
	}
	if res != (store.PruneResult{}) {
		d.log.Info("pruned old records", "requests", res.Requests, "events", res.Events, "evidence", res.Evidence)
	}
	// Deleted rows only free pages inside the file; give them back (and
	// truncate the WAL). Runs on the first prune too, for the one-time
	// conversion of databases created without incremental auto-vacuum.
	if res != (store.PruneResult{}) || !d.compacted {
		d.compacted = true
		cr, err := d.db.Compact(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			d.log.Warn("database compaction", "err", err)
		case cr.Vacuumed || cr.FreedPages > 0:
			d.log.Info("database compacted", "vacuum", cr.Vacuumed, "freed_pages", cr.FreedPages)
		}
	}
}
