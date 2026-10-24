// Package webtest provides an in-memory core.Core for the web package's
// tests and its development harness (internal/web/devserver). It mimics
// the daemon's contract: strictly increasing Seq, first-answer-wins
// decisions, and resync when a subscriber asks for history it no longer
// has or falls behind.
package webtest

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
)

// FakeCore is a goroutine-safe core.Core backed by maps.
type FakeCore struct {
	mu       sync.Mutex
	seq      int64
	events   []api.Event
	sessions map[string]api.Session
	requests map[string]api.Request
	tasks    map[string]api.Task
	presence api.Presence
	subs     map[*sub]struct{}

	// MinSeq is the oldest Seq still "retained": Subscribe(after) with
	// after < MinSeq-1 reports Resync, as the daemon does after pruning.
	MinSeq int64
	// SubBuffer is the per-subscriber channel size; a subscriber that
	// falls further behind is dropped (its channel closed).
	SubBuffer int
	Machine   string
	Version   string
	// Now is the clock; defaults to time.Now.
	Now func() time.Time
}

type sub struct {
	ch     chan api.Event
	closed bool
}

// New returns an empty FakeCore with presence "present/auto".
func New() *FakeCore {
	f := &FakeCore{
		sessions:  map[string]api.Session{},
		requests:  map[string]api.Request{},
		tasks:     map[string]api.Task{},
		subs:      map[*sub]struct{}{},
		SubBuffer: 256,
		Machine:   "devbox",
		Version:   "0.1.0-dev",
		Now:       time.Now,
	}
	f.presence = api.Presence{State: api.PresencePresent, Mode: api.PresenceAuto, Source: "os_idle", ChangedAt: time.Now()}
	return f
}

var _ core.Core = (*FakeCore)(nil)

// PutSession inserts or replaces a session and emits session.updated
// (session.started when new).
func (f *FakeCore) PutSession(s api.Session) {
	f.mu.Lock()
	_, existed := f.sessions[s.ID]
	f.sessions[s.ID] = s
	t := api.EventSessionUpdated
	if !existed {
		t = api.EventSessionStarted
	}
	f.emitLocked(t, s.ID, "", s)
	f.mu.Unlock()
}

// PutRequest inserts or replaces a request and emits request.opened
// (request.closed if it is already closed).
func (f *FakeCore) PutRequest(r api.Request) {
	f.mu.Lock()
	f.requests[r.ID] = r
	t := api.EventRequestOpened
	if r.ClosedAt != nil {
		t = api.EventRequestClosed
	}
	f.emitLocked(t, r.SessionID, r.ID, r)
	f.mu.Unlock()
}

// UpdateSession applies fn to a stored session and emits session.updated
// (session.exited when fn moved it to exited). It reports false when the
// session doesn't exist.
func (f *FakeCore) UpdateSession(id string, fn func(*api.Session)) (api.Session, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	if !ok {
		return api.Session{}, false
	}
	was := s.State
	fn(&s)
	f.sessions[id] = s
	t := api.EventSessionUpdated
	if s.State == api.StateExited && was != api.StateExited {
		t = api.EventSessionExited
	}
	f.emitLocked(t, s.ID, "", s)
	return s, true
}

// Sessions returns every session, most recently updated first.
func (f *FakeCore) Sessions() []api.Session {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]api.Session, 0, len(f.sessions))
	for _, s := range f.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

// Requests returns every request, newest first.
func (f *FakeCore) Requests() []api.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]api.Request, 0, len(f.requests))
	for _, r := range f.requests {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// ExpireRequests closes every open request whose ExpiresAt is before now
// (status expired, handed back to the terminal), as the daemon's hold
// limit does, and returns them.
func (f *FakeCore) ExpireRequests(now time.Time) []api.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []api.Request
	for id, r := range f.requests {
		if r.Status != api.RequestOpen || r.ExpiresAt.IsZero() || r.ExpiresAt.After(now) {
			continue
		}
		r.Status = api.RequestExpired
		t := now
		r.ClosedAt = &t
		f.requests[id] = r
		f.emitLocked(api.EventRequestClosed, r.SessionID, r.ID, r)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// PutTask inserts or replaces a task and emits task.updated.
func (f *FakeCore) PutTask(t api.Task) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasks[t.ID] = t
	f.emitLocked(api.EventTaskUpdated, t.SessionID, "", t)
}

// Task returns one task.
func (f *FakeCore) Task(id string) (api.Task, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	return t, ok
}

// Tasks returns every task, most recently updated first.
func (f *FakeCore) Tasks() []api.Task {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]api.Task, 0, len(f.tasks))
	for _, t := range f.tasks {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

// Decisions returns every decision accepted so far.
func (f *FakeCore) Decisions() []api.Decision {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []api.Decision
	for _, r := range f.requests {
		if r.Decision != nil {
			out = append(out, *r.Decision)
		}
	}
	return out
}

// Seq returns the latest event sequence number.
func (f *FakeCore) Seq() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seq
}

// Subscribers returns the number of live subscriptions.
func (f *FakeCore) Subscribers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}

func (f *FakeCore) Snapshot(context.Context) (api.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	snap := api.Snapshot{Seq: f.seq, Machine: f.Machine, Version: f.Version, Presence: f.presence}
	for _, s := range f.sessions {
		snap.Sessions = append(snap.Sessions, s)
	}
	for _, r := range f.requests {
		snap.Requests = append(snap.Requests, r)
	}
	for _, t := range f.tasks {
		snap.Tasks = append(snap.Tasks, t)
	}
	sort.Slice(snap.Sessions, func(i, j int) bool { return snap.Sessions[i].UpdatedAt.After(snap.Sessions[j].UpdatedAt) })
	sort.Slice(snap.Requests, func(i, j int) bool { return snap.Requests[i].CreatedAt.After(snap.Requests[j].CreatedAt) })
	sort.Slice(snap.Tasks, func(i, j int) bool { return snap.Tasks[i].UpdatedAt.After(snap.Tasks[j].UpdatedAt) })
	return snap, nil
}

func (f *FakeCore) Subscribe(ctx context.Context, after int64) (*core.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.MinSeq > 0 && after < f.MinSeq-1 {
		ch := make(chan api.Event)
		close(ch)
		return &core.Subscription{C: ch, Resync: true, Close: func() {}}, nil
	}
	var backlog []api.Event
	for _, e := range f.events {
		if e.Seq > after {
			backlog = append(backlog, e)
		}
	}
	s := &sub{ch: make(chan api.Event, len(backlog)+f.SubBuffer)}
	for _, e := range backlog {
		s.ch <- e
	}
	f.subs[s] = struct{}{}
	var once sync.Once
	closeFn := func() {
		once.Do(func() {
			f.mu.Lock()
			f.dropLocked(s)
			f.mu.Unlock()
		})
	}
	go func() {
		<-ctx.Done()
		closeFn()
	}()
	return &core.Subscription{C: s.ch, Close: closeFn}, nil
}

func (f *FakeCore) dropLocked(s *sub) {
	if s.closed {
		return
	}
	s.closed = true
	delete(f.subs, s)
	close(s.ch)
}

func (f *FakeCore) find(m map[string]bool, id string) string {
	if m[id] {
		return id
	}
	match := ""
	for k := range m {
		if strings.HasPrefix(k, id) {
			if match != "" {
				return ""
			}
			match = k
		}
	}
	return match
}

func (f *FakeCore) Session(_ context.Context, id string) (api.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := map[string]bool{}
	for k := range f.sessions {
		keys[k] = true
	}
	if k := f.find(keys, id); k != "" {
		return f.sessions[k], nil
	}
	return api.Session{}, core.ErrNotFound
}

func (f *FakeCore) SessionEvents(_ context.Context, sessionID string, limit int) ([]api.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sessions[sessionID]; !ok {
		return nil, core.ErrNotFound
	}
	if limit <= 0 {
		limit = 100
	}
	var out []api.Event
	for i := len(f.events) - 1; i >= 0 && len(out) < limit; i-- {
		if f.events[i].SessionID == sessionID {
			out = append(out, f.events[i])
		}
	}
	return out, nil
}

func (f *FakeCore) MarkSeen(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[sessionID]
	if !ok {
		return core.ErrNotFound
	}
	if s.Unseen {
		s.Unseen = false
		f.sessions[sessionID] = s
		f.emitLocked(api.EventSessionUpdated, s.ID, "", s)
	}
	return nil
}

func (f *FakeCore) Request(_ context.Context, id string) (api.Request, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.requests[id]; ok {
		return r, nil
	}
	return api.Request{}, core.ErrNotFound
}

// Decide is a compare-and-set on status open|stepped_aside.
func (f *FakeCore) Decide(_ context.Context, d api.Decision) (api.Request, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.requests[d.RequestID]
	if !ok {
		return api.Request{}, core.ErrNotFound
	}
	if r.Status != api.RequestOpen && r.Status != api.RequestSteppedAside {
		return r, core.ErrAlreadyClosed
	}
	now := f.Now()
	if d.At.IsZero() {
		d.At = now
	}
	r.Status = api.RequestDecided
	r.Decision = &d
	r.ClosedAt = &now
	f.requests[r.ID] = r
	f.emitLocked(api.EventRequestClosed, r.SessionID, r.ID, r)
	if s, ok := f.sessions[r.SessionID]; ok && s.State == api.StateBlocked {
		s.State = api.StateWorking
		s.BlockKind = api.BlockNone
		s.StateDetail = ""
		s.UpdatedAt = now
		f.sessions[s.ID] = s
		f.emitLocked(api.EventSessionUpdated, s.ID, "", s)
	}
	return r, nil
}

func (f *FakeCore) Presence() api.Presence {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.presence
}

func (f *FakeCore) SetPresenceMode(_ context.Context, m api.PresenceMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.presence.Mode = m
	switch m {
	case api.PresenceForceAway:
		f.presence.State, f.presence.Source = api.PresenceAway, "manual"
	case api.PresenceForceHere:
		f.presence.State, f.presence.Source = api.PresencePresent, "manual"
	default:
		f.presence.Source = "os_idle"
	}
	f.presence.ChangedAt = f.Now()
	f.emitLocked(api.EventPresenceChanged, "", "", f.presence)
	return nil
}

func (f *FakeCore) Emit(_ context.Context, t api.EventType, sessionID, requestID string, data any) (api.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.emitLocked(t, sessionID, requestID, data), nil
}

func (f *FakeCore) emitLocked(t api.EventType, sessionID, requestID string, data any) api.Event {
	f.seq++
	raw, _ := json.Marshal(data)
	ev := api.Event{Seq: f.seq, At: f.Now().UTC(), Type: t, SessionID: sessionID, RequestID: requestID, Data: raw}
	f.events = append(f.events, ev)
	for s := range f.subs {
		select {
		case s.ch <- ev:
		default:
			// Fell behind: the client must resync, as with the daemon.
			f.dropLocked(s)
		}
	}
	return ev
}
