package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/insights"
	"github.com/xasdta/pilothouse/internal/store"
	"github.com/xasdta/pilothouse/internal/transcript"
)

// Activity feeds and insights (F3, F6; workstream C).

var (
	_ core.ActivityReader = (*Daemon)(nil)
	_ core.InsightsReader = (*Daemon)(nil)
)

const (
	// readableTTL / unreadableTTL cache the confinement check behind
	// caps.activity, which runs inside every session save.
	readableTTL   = time.Minute
	unreadableTTL = 2 * time.Second
	// findRetry is how often a transcript nobody named is searched for.
	findRetry = 30 * time.Second
	// maxLooked bounds the "last looked at" times kept.
	maxLooked = 10_000
	// replyMatchSlack: a prompt is a Pilothouse reply when its text equals
	// one sent at most this long before it was logged.
	replyMatchSlack = 5 * time.Second
	recentReplies   = 50
	recentRequests  = 200
)

// activityState is workstream C's per-daemon state: the transcript cache,
// prices and the bookkeeping around them. It is created on first use and
// dropped when the usage loop stops, so the shared Daemon struct needn't
// know about it.
type activityState struct {
	reader *transcript.Reader
	prices *insights.Prices
	roots  transcript.Roots
	loc    *time.Location

	mu       sync.Mutex
	looked   map[string]looks     // session → when people opened its feed
	readable map[string]checked   // agent + transcript path → confinement check
	found    map[string]foundPath // session → transcript found by searching
	saved    map[string]time.Time // session → when its usage was last saved
}

type looks struct{ prev, last time.Time }

type checked struct {
	ok bool
	at time.Time
}

type foundPath struct {
	path string
	at   time.Time
}

var activityStates sync.Map // *Daemon → *activityState

func (d *Daemon) act() *activityState {
	if st, ok := activityStates.Load(d); ok {
		return st.(*activityState)
	}
	prices := insights.NewPrices(d.cfg.Usage.Prices)
	roots := transcript.DefaultRoots()
	st := &activityState{
		prices: prices, roots: roots, loc: time.Local,
		reader:   transcript.NewReader(transcript.Options{Roots: roots, Pricer: prices, MaxSessions: 64}),
		looked:   map[string]looks{},
		readable: map[string]checked{},
		found:    map[string]foundPath{},
		saved:    map[string]time.Time{},
	}
	actual, _ := activityStates.LoadOrStore(d, st)
	return actual.(*activityState)
}

// Activity reads a session's transcript into a feed.
func (d *Daemon) Activity(ctx context.Context, sessionID string, q api.ActivityQuery) (api.Activity, error) {
	full, err := d.db.ResolveSessionID(ctx, sessionID)
	if err != nil {
		return api.Activity{}, err
	}
	s, err := d.db.Session(ctx, full)
	if err != nil {
		return api.Activity{}, err
	}
	st := d.act()
	now := d.now()
	out := api.Activity{SessionID: s.ID, Items: []api.ActivityItem{}}
	src, note, ok := st.source(s, now)
	if !ok {
		out.Note = note
		return out, nil
	}
	since := q.Since
	if since.IsZero() {
		since = st.lookedSince(s.ID, q.Cursor == "", now)
	}
	extra, err := d.catchUpExtra(ctx, s.ID, since)
	if err != nil {
		return api.Activity{}, err
	}
	res, err := st.reader.Read(s.ID, src, transcript.Query{Cursor: q.Cursor, Limit: q.Limit, Since: since, Now: now,
		CatchUp: true, Extra: extra})
	if err != nil {
		out.Note = d.readNote(s, err)
		return out, nil
	}
	if err := d.markReplies(ctx, s.ID, res.Items); err != nil {
		return api.Activity{}, err
	}
	out.Available, out.Items, out.Cursor, out.Files, out.CatchUp = true, res.Items, res.Cursor, res.Files, res.CatchUp
	if res.Usage != (api.Usage{}) {
		u := res.Usage
		out.Usage = &u
	}
	if res.Partial {
		out.Note = "This transcript is very large: the feed and usage start part way through it."
	}
	return out, nil
}

// source says where a session's transcript is, or why there is none.
func (st *activityState) source(s api.Session, now time.Time) (transcript.Source, string, bool) {
	switch s.Agent {
	case api.AgentClaude, api.AgentCodex, api.AgentGrok:
	default:
		return transcript.Source{}, "Pilothouse can't read this agent's transcript.", false
	}
	path := s.TranscriptPath
	if path == "" {
		path = st.find(s, now)
	}
	if path == "" {
		return transcript.Source{}, "No transcript yet: the agent hasn't written one.", false
	}
	return transcript.Source{Agent: s.Agent, Path: path, Cwd: s.Cwd}, "", true
}

// find searches for the transcript of a session whose hooks didn't say
// where it is (Codex sends transcript_path null until the rollout file
// exists), at most every findRetry.
func (st *activityState) find(s api.Session, now time.Time) string {
	if s.AgentSessionID == "" {
		return ""
	}
	st.mu.Lock()
	f, ok := st.found[s.ID]
	st.mu.Unlock()
	if ok && (f.path != "" || now.Sub(f.at) < findRetry) {
		return f.path
	}
	var path string
	switch s.Agent {
	case api.AgentCodex:
		path, _ = transcript.FindCodexRollout(st.roots.Codex, s.AgentSessionID, s.StartedAt, now)
	case api.AgentClaude:
		path, _ = transcript.FindClaude(st.roots.Claude, s.AgentSessionID)
	case api.AgentGrok:
		path, _ = transcript.FindGrokUpdates(st.roots.Grok, s.AgentSessionID)
	}
	st.mu.Lock()
	st.found[s.ID] = foundPath{path: path, at: now}
	st.mu.Unlock()
	return path
}

// lookedSince returns where a catch-up starts: the time a person opened
// this feed before the current look. A read without a cursor opens the
// feed; polls with a cursor keep the same start, so the summary stays put
// while the view is open. Zero (the first look) means from the beginning.
func (st *activityState) lookedSince(id string, opening bool, now time.Time) time.Time {
	st.mu.Lock()
	defer st.mu.Unlock()
	l := st.looked[id]
	if opening {
		if len(st.looked) >= maxLooked {
			for k := range st.looked { // drop an arbitrary entry: bounded, rarely hit
				delete(st.looked, k)
				break
			}
		}
		l.prev, l.last = l.last, now
		st.looked[id] = l
	}
	return l.prev
}

// catchUpExtra is what the store adds to a catch-up: requests waiting now
// and permission requests raised since.
func (d *Daemon) catchUpExtra(ctx context.Context, sessionID string, since time.Time) (transcript.CatchUpExtra, error) {
	var x transcript.CatchUpExtra
	reqs, err := d.db.Requests(ctx, store.RequestFilter{SessionID: sessionID, Limit: recentRequests})
	if err != nil {
		return x, err
	}
	for i := len(reqs) - 1; i >= 0; i-- { // oldest first
		r := reqs[i]
		if r.Status == api.RequestOpen || r.Status == api.RequestSteppedAside {
			x.Waiting = append(x.Waiting, firstNonEmpty(r.Summary, r.Tool))
		}
		granted := r.Decision != nil && strings.HasPrefix(r.Decision.By, "grant:")
		if r.Checkpoint == api.CheckpointPermission && !granted && r.CreatedAt.After(since) {
			x.Approvals++
		}
	}
	return x, nil
}

// markReplies labels prompts that were replies sent through Pilothouse:
// Claude logs a messaging-socket message as a "peer" prompt and typed
// PTY input as a human one; the session.reply event says which they were.
func (d *Daemon) markReplies(ctx context.Context, sessionID string, items []api.ActivityItem) error {
	hasPrompt := false
	for _, it := range items {
		if it.Kind == api.ActivityPrompt {
			hasPrompt = true
			break
		}
	}
	if !hasPrompt {
		return nil
	}
	evs, err := d.db.SessionEventsOfType(ctx, sessionID, api.EventSessionReply, recentReplies)
	if err != nil {
		return err
	}
	used := map[int]bool{}
	for i := len(evs) - 1; i >= 0; i-- { // oldest first
		var rec api.ReplyRecord
		if json.Unmarshal(evs[i].Data, &rec) != nil || strings.TrimSpace(rec.Text) == "" {
			continue
		}
		text := strings.TrimSpace(rec.Text)
		for j := range items {
			it := &items[j]
			if used[j] || it.Kind != api.ActivityPrompt || it.At.Before(rec.At.Add(-replyMatchSlack)) {
				continue
			}
			// Long prompts are capped in the feed ("…"): a prefix matches.
			t := strings.TrimSpace(it.Text)
			if t == text || (strings.HasSuffix(t, "…") && strings.HasPrefix(text, strings.TrimSuffix(t, "…"))) {
				it.Source, used[j] = "pilothouse", true
				break
			}
		}
	}
	return nil
}

// readNote explains a transcript that couldn't be read.
func (d *Daemon) readNote(s api.Session, err error) string {
	switch {
	case transcript.IsMissing(err):
		return "No transcript yet: the agent hasn't written one."
	case errors.Is(err, transcript.ErrOutside):
		return "The transcript is outside the agent's own folder, so Pilothouse doesn't read it."
	}
	d.log.Warn("read transcript", "session", s.ID, "err", err)
	return "Pilothouse couldn't read the transcript."
}

// Insights aggregates usage and decisions over a range of days.
func (d *Daemon) Insights(ctx context.Context, q api.InsightsQuery) (api.Insights, error) {
	st := d.act()
	now := d.now()
	rg := insights.NewRange(now, st.loc, q.Days)
	var rows []store.UsageRow
	var events []store.DecisionRow
	err := d.db.Read(ctx, func(r store.Reader) error {
		var err error
		if rows, err = r.UsageDays(ctx, rg.FromDay, rg.ToDay); err != nil {
			return err
		}
		events, err = r.DecisionRows(ctx, rg.From, now.Add(time.Nanosecond))
		return err
	})
	if err != nil {
		return api.Insights{}, err
	}
	return insights.Build(insights.Input{Range: rg, Loc: st.loc, Usage: rows, Events: events,
		BudgetUSD: d.cfg.Usage.DailyBudgetUSD, BurnUSDh: st.reader.SpentSince(now.Add(-time.Hour))}), nil
}

// activityCaps sets Activity when the session's transcript is readable.
// It runs inside session saves, so it only stats (cached), never parses.
func (d *Daemon) activityCaps(ctx context.Context, r store.Reader, s api.Session, c *api.SessionCaps) {
	st := d.act()
	path := s.TranscriptPath
	if path == "" {
		st.mu.Lock()
		path = st.found[s.ID].path
		st.mu.Unlock()
	}
	if path == "" {
		return
	}
	c.Activity = st.readableNow(s.Agent, path, d.now())
}

func (st *activityState) readableNow(agent api.AgentKind, path string, now time.Time) bool {
	key := string(agent) + "\x00" + path
	st.mu.Lock()
	ch, ok := st.readable[key]
	st.mu.Unlock()
	if ok && ((ch.ok && now.Sub(ch.at) < readableTTL) || (!ch.ok && now.Sub(ch.at) < unreadableTTL)) {
		return ch.ok
	}
	readable := false
	if real, err := transcript.Resolve(path, st.roots.For(agent)); err == nil {
		fi, err := os.Stat(real)
		readable = err == nil && fi.Mode().IsRegular()
	}
	st.mu.Lock()
	if len(st.readable) > maxLooked {
		st.readable = map[string]checked{}
	}
	st.readable[key] = checked{ok: readable, at: now}
	st.mu.Unlock()
	return readable
}
