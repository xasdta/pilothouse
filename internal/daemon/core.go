package daemon

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/state"
	"github.com/xasdta/pilothouse/internal/store"
	"github.com/xasdta/pilothouse/internal/version"
)

// Snapshot window sizes (BUILD-SPEC §4).
const (
	snapshotExitedWindow = 24 * time.Hour
	snapshotClosedLimit  = 50
	snapshotTaskWindow   = 7 * 24 * time.Hour
	evidenceWindow       = 50 // evidence rows the resolver looks at
)

// errBadRequest marks errors caused by the caller's input (HTTP 400).
var errBadRequest = errors.New("bad request")

func badRequest(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errBadRequest, fmt.Sprintf(format, args...))
}

// Snapshot returns current sessions, requests, tasks and presence, plus the
// seq they correspond to; all read in one transaction so they agree.
func (d *Daemon) Snapshot(ctx context.Context) (api.Snapshot, error) {
	now := d.now()
	snap := api.Snapshot{Machine: d.machine, Version: version.Version, Presence: d.Presence()}
	err := d.db.Read(ctx, func(r store.Reader) error {
		var err error
		if snap.Seq, err = r.MaxSeq(ctx); err != nil {
			return err
		}
		if snap.Sessions, err = r.Sessions(ctx, now.Add(-snapshotExitedWindow)); err != nil {
			return err
		}
		pending, err := r.Requests(ctx, store.RequestFilter{Statuses: store.PendingStatuses})
		if err != nil {
			return err
		}
		closed, err := r.Requests(ctx, store.RequestFilter{Closed: true, Limit: snapshotClosedLimit})
		if err != nil {
			return err
		}
		snap.Requests = append(pending, closed...)
		snap.Tasks, err = r.Tasks(ctx, now.Add(-snapshotTaskWindow))
		return err
	})
	return snap, err
}

// Subscribe tails the event log from after (see broker.subscribe).
func (d *Daemon) Subscribe(ctx context.Context, after int64) (*core.Subscription, error) {
	return d.broker.subscribe(ctx, after)
}

// Session returns a session by id or unique prefix.
func (d *Daemon) Session(ctx context.Context, id string) (api.Session, error) {
	full, err := d.db.ResolveSessionID(ctx, id)
	if err != nil {
		return api.Session{}, err
	}
	return d.db.Session(ctx, full)
}

// SessionEvents returns a session's latest events, newest first.
func (d *Daemon) SessionEvents(ctx context.Context, sessionID string, limit int) ([]api.Event, error) {
	full, err := d.db.ResolveSessionID(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return d.db.SessionEvents(ctx, full, limit)
}

// MarkSeen clears a session's Unseen flag.
func (d *Daemon) MarkSeen(ctx context.Context, sessionID string) error {
	full, err := d.db.ResolveSessionID(ctx, sessionID)
	if err != nil {
		return err
	}
	return d.db.Write(ctx, func(tx *store.Tx) error {
		s, err := tx.Session(ctx, full)
		if err != nil || !s.Unseen {
			return err
		}
		before := s
		s.Unseen = false
		_, err = d.saveSession(ctx, tx, &before, s)
		return err
	})
}

// Request returns a request by id or unique prefix.
func (d *Daemon) Request(ctx context.Context, id string) (api.Request, error) {
	full, err := d.db.ResolveRequestID(ctx, id)
	if err != nil {
		return api.Request{}, err
	}
	return d.db.Request(ctx, full)
}

// Decide closes a pending request with a person's answer. The close is a
// compare-and-set: the first decision wins and later ones get
// core.ErrAlreadyClosed. An allow with scope=session also stores a grant
// that the pre_tool checkpoint applies to later calls.
func (d *Daemon) Decide(ctx context.Context, dec api.Decision) (api.Request, error) {
	switch dec.Behavior {
	case api.BehaviorAllow, api.BehaviorDeny:
	default:
		return api.Request{}, badRequest("behavior must be allow or deny, not %q", dec.Behavior)
	}
	switch dec.Scope {
	case "":
		dec.Scope = api.ScopeOnce
	case api.ScopeOnce, api.ScopeSession:
	default:
		return api.Request{}, badRequest("scope must be once or session, not %q", dec.Scope)
	}
	if dec.Behavior == api.BehaviorDeny {
		dec.Scope = api.ScopeOnce
	}
	if dec.By == "" {
		return api.Request{}, badRequest("decision has no author")
	}
	id, err := d.db.ResolveRequestID(ctx, dec.RequestID)
	if err != nil {
		return api.Request{}, err
	}
	dec.RequestID = id

	var out api.Request
	err = d.db.Write(ctx, func(tx *store.Tx) error {
		dec.At = tx.Now()
		// What a valid answer is depends on what the request asks
		// (questions.go). A closed request reports already-closed below,
		// whatever the answer: the first one won.
		cur, err := tx.Request(ctx, id)
		if err != nil {
			return err
		}
		if cur.Status == api.RequestOpen || cur.Status == api.RequestSteppedAside {
			if err := validateDecision(cur, &dec); err != nil {
				return err
			}
		}
		req, err := tx.CloseRequest(ctx, id, api.RequestDecided, &dec, tx.Now())
		if err != nil {
			return err
		}
		if dec.Behavior == api.BehaviorAllow && dec.Scope == api.ScopeSession {
			g := api.Grant{ID: newID("g_"), SessionID: req.SessionID, Tool: req.Tool,
				Prefix: grantPrefix(req), DecisionBy: dec.By, CreatedAt: tx.Now()}
			if err := tx.InsertGrant(ctx, g); err != nil {
				return err
			}
		}
		if _, err := tx.Emit(ctx, api.EventRequestClosed, req.SessionID, req.ID, req); err != nil {
			return err
		}
		out = req
		return d.touchSession(ctx, tx, req.SessionID)
	})
	if err != nil {
		return api.Request{}, err
	}
	d.waiters.resolve(id, out)
	d.log.Info("request decided", "request", id, "behavior", dec.Behavior, "scope", dec.Scope, "by", dec.By)
	return out, nil
}

// Emit appends an arbitrary event (used by the web package for
// device.changed) and fans it out.
func (d *Daemon) Emit(ctx context.Context, t api.EventType, sessionID, requestID string, data any) (api.Event, error) {
	var ev api.Event
	err := d.db.Write(ctx, func(tx *store.Tx) error {
		var err error
		ev, err = tx.Emit(ctx, t, sessionID, requestID, data)
		return err
	})
	return ev, err
}

// Explain returns the evidence behind a session's state and the rule that
// chose it.
func (d *Daemon) Explain(ctx context.Context, id string) (api.Explain, error) {
	full, err := d.db.ResolveSessionID(ctx, id)
	if err != nil {
		return api.Explain{}, err
	}
	var out api.Explain
	err = d.db.Read(ctx, func(r store.Reader) error {
		s, err := r.Session(ctx, full)
		if err != nil {
			return err
		}
		in, newestFirst, err := d.stateInput(ctx, r, s, d.now())
		if err != nil {
			return err
		}
		res := state.Resolve(in)
		n := len(newestFirst)
		for i := range newestFirst {
			newestFirst[i].Fresh = res.Fresh[n-1-i] // in.Evidence is oldest first
		}
		out = api.Explain{Session: s, Evidence: newestFirst, Rule: string(res.Rule)}
		if res.Disagreement != "" {
			out.Rule += " (" + res.Disagreement + ")"
		}
		return nil
	})
	return out, err
}

// ---- session state ----

// stateInput gathers the resolver's inputs for s. It also returns the
// evidence newest first, for Explain.
func (d *Daemon) stateInput(ctx context.Context, r store.Reader, s api.Session, now time.Time) (state.Input, []api.StateEvidence, error) {
	evs, err := r.Evidence(ctx, s.ID, evidenceWindow)
	if err != nil {
		return state.Input{}, nil, err
	}
	in := state.Input{
		Now:      now,
		Exited:   s.ExitedAt != nil,
		HasHooks: d.adapters[s.Agent] != nil,
		Evidence: make([]state.Evidence, len(evs)),
	}
	for i, e := range evs {
		in.Evidence[len(evs)-1-i] = state.Evidence{At: e.At, Layer: e.Layer, State: e.State, Kind: e.Kind, Detail: e.Detail}
	}
	if s.ExitCode != nil {
		in.ExitDetail = "Exited with code " + strconv.Itoa(*s.ExitCode)
	}
	pending, err := r.Requests(ctx, store.RequestFilter{SessionID: s.ID, Statuses: store.PendingStatuses, Limit: 1})
	if err != nil {
		return state.Input{}, nil, err
	}
	if len(pending) > 0 {
		in.PendingPermission = true
		if p := pending[0]; asksRequest(p) {
			// "Asks: Which database?" / "Plan ready: …" already say it.
			in.PendingKind, in.PermissionDetail = api.BlockQuestion, p.Summary
		} else {
			in.PermissionDetail = "Waiting for permission: " + p.Summary
		}
	}
	return in, evs, nil
}

// saveSession resolves s's state, then persists it with one event if
// anything changed: session.started for a new session (before == nil),
// session.exited when it has just exited, session.updated otherwise.
func (d *Daemon) saveSession(ctx context.Context, tx *store.Tx, before *api.Session, s api.Session) (api.Session, error) {
	in, _, err := d.stateInput(ctx, tx.Reader, s, tx.Now())
	if err != nil {
		return s, err
	}
	res := state.Resolve(in)
	if state.NeedsAttention(s.State, s.BlockKind, res.State, res.Kind) {
		s.Unseen = true
	}
	s.State, s.BlockKind, s.StateLayer, s.StateDetail = res.State, res.Kind, res.Layer, res.Detail
	s.Caps = d.capsFor(ctx, tx.Reader, s)
	if before != nil && sameSession(*before, s) {
		return s, nil
	}
	s.UpdatedAt = tx.Now()
	if err := tx.PutSession(ctx, s); err != nil {
		return s, err
	}
	typ := api.EventSessionUpdated
	switch {
	case before == nil:
		typ = api.EventSessionStarted
	case s.State == api.StateExited && before.State != api.StateExited:
		typ = api.EventSessionExited
	}
	_, err = tx.Emit(ctx, typ, s.ID, "", s)
	return s, err
}

// touchSession re-resolves a session after something it depends on (its
// requests) changed.
func (d *Daemon) touchSession(ctx context.Context, tx *store.Tx, id string) error {
	s, err := tx.Session(ctx, id)
	if err != nil {
		return err
	}
	_, err = d.saveSession(ctx, tx, &s, s)
	return err
}

// sameSession compares sessions ignoring UpdatedAt, so a hook that changes
// nothing visible writes no row and emits no event.
func sameSession(a, b api.Session) bool {
	a.UpdatedAt, b.UpdatedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}

// ---- requests ----

// closeRequest moves a request from one of `from` to `to` (compare-and-set),
// logs request.closed, re-resolves its session and wakes its waiter.
func (d *Daemon) closeRequest(ctx context.Context, id string, from []api.RequestStatus, to api.RequestStatus) (api.Request, error) {
	var out api.Request
	err := d.db.Write(ctx, func(tx *store.Tx) error {
		req, err := tx.TransitionRequest(ctx, id, from, to, nil, tx.Now())
		if err != nil {
			return err
		}
		if _, err := tx.Emit(ctx, api.EventRequestClosed, req.SessionID, req.ID, req); err != nil {
			return err
		}
		out = req
		return d.touchSession(ctx, tx, req.SessionID)
	})
	if err != nil {
		return api.Request{}, err
	}
	d.waiters.resolve(id, out)
	return out, nil
}

// closeInTx closes a pending request inside a caller's transaction (the
// caller saves the session afterwards and resolves any waiter once the
// transaction commits). ok is false when it was already closed.
func closeInTx(ctx context.Context, tx *store.Tx, id string, to api.RequestStatus) (api.Request, bool, error) {
	req, err := tx.CloseRequest(ctx, id, to, nil, tx.Now())
	if errors.Is(err, store.ErrAlreadyClosed) {
		return api.Request{}, false, nil
	}
	if err != nil {
		return api.Request{}, false, err
	}
	_, err = tx.Emit(ctx, api.EventRequestClosed, req.SessionID, req.ID, req)
	return req, err == nil, err
}

// releaseHeld hands every held request back to its agent's own dialog
// (status stepped_aside, no-opinion answer). Used when the person comes
// back to the desk and when the daemon stops.
func (d *Daemon) releaseHeld(ctx context.Context, reason string) {
	for _, id := range d.waiters.ids() {
		_, err := d.closeRequest(ctx, id, []api.RequestStatus{api.RequestOpen}, api.RequestSteppedAside)
		if err != nil && !errors.Is(err, store.ErrAlreadyClosed) {
			d.log.Warn("release held request", "request", id, "err", err)
			continue
		}
		if err == nil {
			d.log.Info("request handed back to the terminal", "request", id, "reason", reason)
		}
	}
}

// recoverRequests expires requests a previous daemon was holding: their
// hook shims lost the connection when it stopped.
func (d *Daemon) recoverRequests(ctx context.Context) error {
	open, err := d.db.Requests(ctx, store.RequestFilter{Statuses: []api.RequestStatus{api.RequestOpen}})
	if err != nil {
		return err
	}
	for _, r := range open {
		if _, err := d.closeRequest(ctx, r.ID, []api.RequestStatus{api.RequestOpen}, api.RequestExpired); err != nil &&
			!errors.Is(err, store.ErrAlreadyClosed) {
			return fmt.Errorf("expire stale request %s: %w", r.ID, err)
		}
	}
	return nil
}
