package daemon

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/store"
)

// The usage loop (F6): every few seconds it reads what live sessions'
// transcripts gained, stores Session.Usage (so session events carry it)
// and the daily totals behind insights, and checks the daily budget.

const (
	// usageExitedWindow: sessions that exited this recently are still
	// refreshed, so their last turn is counted.
	usageExitedWindow = 10 * time.Minute
	// usageBackfill: the first pass after start also counts sessions that
	// exited this recently (while the daemon was down, or before 0.2),
	// at most usageBackfillMax of them.
	usageBackfill    = 7 * 24 * time.Hour
	usageBackfillMax = 100

	// budgetMetaKey stores "<local day>:<percent>" of the last budget
	// alert, so a restart doesn't repeat it.
	budgetMetaKey = "usage_budget_alerted"
	// notifyBudget is the notification kind of budget alerts.
	notifyBudget core.NotificationKind = "budget"
)

// usageInterval is the loop period; usageSaveGap is how often one
// session's usage may be re-saved (each save emits session.updated).
// Both scale with PollInterval so tests run fast: 10 s and 30 s by
// default.
func (d *Daemon) usageInterval() time.Duration { return 5 * d.opts.PollInterval }
func (d *Daemon) usageSaveGap() time.Duration  { return 15 * d.opts.PollInterval }

// startUsage starts the usage loop. Its first pass runs at once.
func (d *Daemon) startUsage() {
	d.goBackground(func() {
		defer activityStates.Delete(d)
		d.refreshUsage(d.ctx, true)
		t := time.NewTicker(d.usageInterval())
		defer t.Stop()
		for {
			select {
			case <-d.ctx.Done():
				return
			case <-t.C:
				d.refreshUsage(d.ctx, false)
			}
		}
	})
}

// refreshUsage reads every recent session's usage, upserts usage_daily,
// re-saves sessions whose usage changed (rate-limited) and alerts on the
// budget.
func (d *Daemon) refreshUsage(ctx context.Context, backfill bool) {
	st := d.act()
	now := d.now()
	window := usageExitedWindow
	if backfill {
		window = usageBackfill
	}
	sessions, err := d.db.Sessions(ctx, now.Add(-window))
	if err != nil {
		if ctx.Err() == nil {
			d.log.Warn("usage refresh", "err", err)
		}
		return
	}
	var (
		rows    []store.UsageRow
		changed = map[string]api.Usage{}
		old     int
	)
	for _, s := range sessions {
		if ctx.Err() != nil {
			return
		}
		recent := s.ExitedAt == nil || now.Sub(*s.ExitedAt) < usageExitedWindow
		if !recent {
			if old++; old > usageBackfillMax {
				continue
			}
		}
		hadPath := s.TranscriptPath != "" || st.foundPath(s.ID) != ""
		src, _, ok := st.source(s, now)
		if !ok {
			continue
		}
		res, err := st.reader.Usage(s.ID, src)
		if !recent {
			st.reader.Forget(s.ID) // counted once; don't keep its feed
		}
		if err != nil {
			continue
		}
		for _, du := range res.Days {
			if du.Tokens.Zero() && du.Turns == 0 {
				continue
			}
			rows = append(rows, store.UsageRow{SessionID: s.ID, Day: du.Day, Agent: s.Agent, Model: du.Model,
				Repo: s.RepoRoot, Input: du.Tokens.Input, Output: du.Tokens.Output, CacheRead: du.Tokens.CacheRead,
				CacheWrite: du.Tokens.CacheWrite, Reasoning: du.Tokens.Reasoning, CostUSD: du.CostUSD, Turns: du.Turns})
		}
		u := res.Usage
		newPath := !hadPath && s.ExitedAt == nil // a found transcript changes caps.activity
		if (u != (api.Usage{}) && (s.Usage == nil || *s.Usage != u) && st.maySave(s.ID, now, d.usageSaveGap())) || newPath {
			changed[s.ID] = u
		}
	}
	if len(rows) == 0 && len(changed) == 0 {
		d.checkBudget(ctx, now)
		return
	}
	err = d.db.Write(ctx, func(tx *store.Tx) error {
		if err := tx.UpsertUsage(ctx, rows); err != nil {
			return err
		}
		for id, u := range changed {
			s, err := tx.Session(ctx, id)
			if err != nil {
				return err
			}
			before := s
			if u != (api.Usage{}) {
				u := u
				s.Usage = &u
			}
			if _, err := d.saveSession(ctx, tx, &before, s); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if ctx.Err() == nil {
			d.log.Warn("save usage", "err", err)
		}
		return
	}
	st.markSaved(changed, now)
	d.checkBudget(ctx, now)
}

func (st *activityState) foundPath(id string) string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.found[id].path
}

func (st *activityState) maySave(id string, now time.Time, gap time.Duration) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	last, ok := st.saved[id]
	return !ok || now.Sub(last) >= gap
}

func (st *activityState) markSaved(ids map[string]api.Usage, now time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for id := range ids {
		st.saved[id] = now
	}
	if len(st.saved) > maxLooked {
		for id, t := range st.saved {
			if now.Sub(t) > time.Hour {
				delete(st.saved, id)
			}
		}
	}
}

// checkBudget sends a notification when today's estimated spend first
// reaches 80 % and 100 % of usage.daily_budget_usd. Crossing both at once
// sends only the 100 % one.
func (d *Daemon) checkBudget(ctx context.Context, now time.Time) {
	budget := d.cfg.Usage.DailyBudgetUSD
	if budget <= 0 {
		return
	}
	day := now.In(d.act().loc).Format("2006-01-02")
	spent, err := d.db.SpentOn(ctx, day)
	if err != nil {
		return
	}
	level := 0
	switch {
	case spent >= budget:
		level = 100
	case spent >= 0.8*budget:
		level = 80
	}
	if level == 0 {
		return
	}
	var send bool
	err = d.db.Write(ctx, func(tx *store.Tx) error {
		v, _, err := tx.Meta(ctx, budgetMetaKey)
		if err != nil {
			return err
		}
		lastDay, lastLevel := parseBudgetAlert(v)
		if lastDay == day && lastLevel >= level {
			return nil
		}
		send = true
		return tx.SetMeta(ctx, budgetMetaKey, day+":"+strconv.Itoa(level))
	})
	if err != nil || !send {
		return
	}
	n := core.Notification{Kind: notifyBudget, Remote: true}
	if level == 100 {
		n.Title = "Today's agent budget is used up"
		n.Body = fmt.Sprintf("Agents have spent an estimated $%.2f of your $%.2f daily budget. Pilothouse doesn't stop them: see `pilothouse usage`.", spent, budget)
	} else {
		n.Title = "Agents used 80% of today's budget"
		n.Body = fmt.Sprintf("An estimated $%.2f of your $%.2f daily budget is spent.", spent, budget)
	}
	d.log.Info("budget alert", "percent", level, "spent_usd", fmt.Sprintf("%.2f", spent), "budget_usd", budget)
	d.notify(n)
}

func parseBudgetAlert(v string) (day string, level int) {
	day, lv, ok := strings.Cut(v, ":")
	if !ok {
		return "", 0
	}
	level, _ = strconv.Atoi(lv)
	return day, level
}
