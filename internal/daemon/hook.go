package daemon

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/policy"
	"github.com/xasdta/pilothouse/internal/state"
	"github.com/xasdta/pilothouse/internal/store"
)

const (
	// holdMargin keeps our hold shorter than the agent's hook timeout, so
	// we answer before the agent kills the shim.
	holdMargin = 15 * time.Second
	// maxLoggedInput caps the tool input kept in a hook event. Policy replay
	// rarely needs more, and a 10 MB file write shouldn't bloat the log.
	maxLoggedInput = 256 << 10
	maxLastMessage = 500
)

// hookCall is one hook being routed, with what the daemon worked out about
// it before opening the write transaction.
type hookCall struct {
	ad       adapters.Adapter
	env      adapters.HookEnvelope
	ev       adapters.HookEvent
	cwd      string
	root     string
	branch   string
	agentPID int
	shared   bool // the hook ran under a shared agent server (Codex app-server)
	mode     api.Mode
	// answered are requests this hook showed were answered in the agent's
	// own dialog; their waiters are released after the commit.
	answered []api.Request
}

// permissionPlan is a permission request about to be opened.
type permissionPlan struct {
	req       api.Request
	toolUseID string
	hold      time.Duration
	wait      <-chan api.Request // nil when routed to the terminal
	pol       policy.Result      // verdict at the permission checkpoint
	granted   bool               // a session grant answered it; nobody is asked
	// mustHold: the agent has no dialog for it (HookEvent.Hold), so it is
	// held for a person at the desk too and never answered by a grant.
	mustHold bool
}

// Hook routes one hook call (BUILD-SPEC §5) and returns what the shim must
// print. It never fails: on internal errors it answers like the shim would
// with the daemon down (fail open unless approvals.fail_closed).
func (d *Daemon) Hook(ctx context.Context, env adapters.HookEnvelope) adapters.HookResponse {
	ad := d.adapters[env.Agent]
	if ad == nil {
		return adapters.HookResponse{} // unknown agent: no opinion
	}
	ev, err := ad.ParseHook(env)
	if err != nil {
		d.log.Debug("unparseable hook", "agent", env.Agent, "event", env.Event, "err", err)
		return ad.FailResponse(adapters.HookUnknown, false)
	}
	resp, err := d.routeHook(ctx, ad, env, ev)
	if err != nil {
		lvl := slog.LevelError
		if isTimeout(ctx, err) {
			// The shim gave up (its own timeout) or the agent killed it:
			// expected under load, not a daemon fault.
			lvl = slog.LevelWarn
		}
		d.log.Log(ctx, lvl, "hook failed", "agent", env.Agent, "kind", ev.Kind, "err", err)
		return ad.FailResponse(ev.Kind, d.cfg.Approvals.FailClosed)
	}
	return resp
}

func (d *Daemon) routeHook(ctx context.Context, ad adapters.Adapter, env adapters.HookEnvelope, ev adapters.HookEvent) (adapters.HookResponse, error) {
	h := &hookCall{ad: ad, env: env, ev: ev, cwd: firstNonEmpty(ev.Cwd, env.Cwd), mode: api.ModeObserved}
	if env.PilothouseSessionID != "" {
		h.mode = api.ModeWrapped
	}
	h.root, h.branch = d.git.lookup(ctx, h.cwd)
	h.agentPID = agentPID(ad, env.Ancestors)
	h.shared = underSharedServer(ad, env.Ancestors)

	// Policy is evaluated before the write transaction so the (pure, CPU
	// bound) rule evaluation doesn't hold the database write lock.
	var pol policy.Result
	if ev.Kind == adapters.HookPreTool || ev.Kind == adapters.HookPermission {
		pol = d.engine().Evaluate(policy.Input{
			Agent: env.Agent, Tool: ev.Tool, ToolKind: ev.ToolKind, Command: ev.Command, Paths: ev.Paths,
			Input: ev.Input, Cwd: h.cwd, Repo: h.root, Branch: h.branch, Mode: h.mode,
			Checkpoint: checkpointOf(ev.Kind), Now: d.opts.Now(),
		})
	}
	var plan *permissionPlan
	if ev.Kind == adapters.HookPermission {
		// The waiter is registered before the request is committed, so a
		// decision that lands immediately after the commit can't be missed.
		plan = d.planPermission(h, pol)
	}

	var outcome adapters.HookOutcome
	var sess api.Session
	err := d.db.Write(ctx, func(tx *store.Tx) error {
		s, err := d.sessionForHook(ctx, tx, h)
		if err != nil {
			return err
		}
		before := s
		applyHook(&s, h, tx.Now())
		// A task may be registered before its session exists (`task new`
		// calls PutTask first); link it as soon as either side shows up.
		if err := linkTask(ctx, tx, s.TaskID, s.ID); err != nil {
			return err
		}
		if _, err := tx.Emit(ctx, api.EventHook, s.ID, "", loggedEvent(ev)); err != nil {
			return err
		}
		if err := addHookEvidence(ctx, tx, s.ID, ev); err != nil {
			return err
		}
		if outcome, err = d.applyHookEffects(ctx, tx, s, h, pol, plan); err != nil {
			return err
		}
		sess, err = d.saveSession(ctx, tx, &before, s)
		return err
	})
	if err != nil {
		if plan != nil && plan.wait != nil {
			d.waiters.remove(plan.req.ID)
		}
		// The verdict doesn't depend on the database: a deny or ask must
		// hold even when recording the hook failed (locked or full disk).
		if out, ok := policyVerdict(ev, pol); ok {
			d.log.Error("hook not recorded; enforcing the policy verdict anyway",
				"agent", env.Agent, "kind", ev.Kind, "behavior", out.Behavior, "rule", pol.Rule, "err", err)
			return ad.EncodeResponse(ev, out), nil
		}
		return adapters.HookResponse{}, err
	}

	for _, r := range h.answered {
		if r.Route == api.RouteDevices {
			d.log.Info("held request answered in the agent's own dialog", "request", r.ID, "by", ev.Kind)
		}
		d.waiters.resolve(r.ID, r)
	}

	switch ev.Kind {
	case adapters.HookPermission:
		if plan.granted {
			if plan.wait != nil {
				d.waiters.remove(plan.req.ID)
			}
			d.log.Info("permission allowed by session grant", "request", plan.req.ID, "by", plan.req.Decision.By)
			outcome = adapters.HookOutcome{Behavior: api.BehaviorAllow, Message: decisionMessage(plan.req.Decision)}
		} else {
			outcome = d.holdPermission(ctx, plan, sess)
		}
	case adapters.HookStop:
		// A task's session says "ready for review" instead (taskrun.go).
		if d.Presence().State == api.PresenceAway && sess.TaskID == "" {
			// Empty Title/Body: notify renders the standard wording.
			d.notify(core.Notification{Kind: core.NotifyWaiting, Session: &sess, Remote: true})
		}
	}
	return ad.EncodeResponse(ev, outcome), nil
}

// applyHookEffects does the per-kind work of §5 inside the hook transaction.
func (d *Daemon) applyHookEffects(ctx context.Context, tx *store.Tx, s api.Session, h *hookCall, pol policy.Result, plan *permissionPlan) (adapters.HookOutcome, error) {
	ev := h.ev
	switch ev.Kind {
	case adapters.HookPreTool:
		return d.decidePreTool(ctx, tx, s, ev, pol)
	case adapters.HookPermission:
		plan.req.SessionID = s.ID
		if err := d.grantPermission(ctx, tx, plan); err != nil {
			return adapters.HookOutcome{}, err
		}
		if plan.granted {
			if err := tx.InsertRequest(ctx, plan.req, plan.toolUseID); err != nil {
				return adapters.HookOutcome{}, err
			}
			_, err := tx.Emit(ctx, api.EventRequestClosed, s.ID, plan.req.ID, plan.req)
			return adapters.HookOutcome{}, err
		}
		if asksRequest(plan.req) {
			if err := h.supersede(ctx, tx, plan.req); err != nil {
				return adapters.HookOutcome{}, err
			}
		}
		if err := tx.InsertRequest(ctx, plan.req, plan.toolUseID); err != nil {
			return adapters.HookOutcome{}, err
		}
		_, err := tx.Emit(ctx, api.EventRequestOpened, s.ID, plan.req.ID, plan.req)
		return adapters.HookOutcome{}, err
	case adapters.HookPostTool:
		// The person answered the agent's own dialog: the tool ran. That
		// dialog shows even while the request is held for devices, so a
		// held (open) request is answered here too, and its hook released.
		req, err := tx.MatchPending(ctx, s.ID, ev.ToolUseID, ev.Tool, ev.Summary)
		if errors.Is(err, store.ErrNotFound) {
			return adapters.HookOutcome{}, nil
		}
		if err != nil {
			return adapters.HookOutcome{}, err
		}
		return adapters.HookOutcome{}, h.answerLocally(ctx, tx, req.ID)
	case adapters.HookPromptSubmit, adapters.HookStop, adapters.HookSessionEnd:
		// The turn moved on, so whatever the dialog showed was answered
		// (a denied dialog fires no Stop: the next prompt closes it).
		return adapters.HookOutcome{}, h.answerAllLocally(ctx, tx, s.ID)
	case adapters.HookNotification:
		// "Waiting for your input": no dialog is up any more (Esc/No in
		// the native dialog fires nothing else).
		if state.IsIdleNotification(ev) {
			return adapters.HookOutcome{}, h.answerAllLocally(ctx, tx, s.ID)
		}
	}
	return adapters.HookOutcome{}, nil
}

// isTimeout reports whether err comes from the hook call's context ending
// (shim timeout or disconnect) rather than a fault.
func isTimeout(ctx context.Context, err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || ctx.Err() != nil
}

// answerLocally closes a pending request as answered in the agent's own
// dialog. A held request's waiter is released after the commit (see
// routeHook) and its hook answers "no opinion"; a device decision arriving
// later gets already_decided.
func (h *hookCall) answerLocally(ctx context.Context, tx *store.Tx, id string) error {
	req, ok, err := closeInTx(ctx, tx, id, api.RequestAnsweredLocally)
	if ok {
		h.answered = append(h.answered, req)
	}
	return err
}

// answerAllLocally closes every pending request of a session as answered
// locally.
func (h *hookCall) answerAllLocally(ctx context.Context, tx *store.Tx, sessionID string) error {
	reqs, err := tx.Requests(ctx, store.RequestFilter{SessionID: sessionID, Statuses: store.PendingStatuses})
	if err != nil {
		return err
	}
	for _, r := range reqs {
		if err := h.answerLocally(ctx, tx, r.ID); err != nil {
			return err
		}
	}
	return nil
}

// decidePreTool combines policy and grants: deny > ask > grant allow >
// policy allow > no opinion. Grants are applied after deny/ask rules so a
// deny rule always wins over "allow for this session". When a rule or grant
// decided, a policy.decision event (policy.DecisionEvent, which `policy
// replay` pairs with the hook by tool_use_id) records it.
//
// A tool that asks the person something (a question, a plan) is decided
// by deny rules only: an allow would not answer it — or would approve a
// plan nobody read — and an ask adds nothing, since it reaches the person
// anyway. The permission checkpoint asks them.
func (d *Daemon) decidePreTool(ctx context.Context, tx *store.Tx, s api.Session, ev adapters.HookEvent, pol policy.Result) (adapters.HookOutcome, error) {
	de := policy.DecisionEvent{ToolUseID: ev.ToolUseID, Tool: ev.Tool, Summary: ev.Summary, Result: pol}
	out, decided := policyVerdict(ev, pol)
	if !decided && !asksEvent(ev) {
		grants, err := tx.Grants(ctx, s.ID)
		if err != nil {
			return out, err
		}
		if g, ok := matchGrant(grants, ev.Tool, ev.ToolKind, ev.Command); ok {
			out = adapters.HookOutcome{Behavior: api.BehaviorAllow, Message: "Allowed for this session in Pilothouse."}
			de.Grant = g.ID
		} else if pol.Action == api.BehaviorAllow {
			out = adapters.HookOutcome{Behavior: api.BehaviorAllow, Message: pol.Message}
		}
	}
	if out.Behavior == "" {
		return out, nil
	}
	de.Outcome = out.Behavior
	_, err := tx.Emit(ctx, api.EventPolicyDecision, s.ID, "", de)
	return out, err
}

// policyVerdict is the pre_tool outcome policy alone decides: deny or ask.
// It needs no database, so it is enforced even when the hook can't be
// recorded.
func policyVerdict(ev adapters.HookEvent, pol policy.Result) (adapters.HookOutcome, bool) {
	if ev.Kind != adapters.HookPreTool {
		return adapters.HookOutcome{}, false
	}
	switch pol.Action {
	case api.BehaviorDeny:
		return adapters.HookOutcome{Behavior: api.BehaviorDeny, Rule: pol.Rule,
			Message: firstNonEmpty(pol.Message, "Blocked by Pilothouse policy rule "+pol.Rule+".")}, true
	case api.BehaviorAsk:
		if asksEvent(ev) {
			return adapters.HookOutcome{}, false // the question itself reaches the person
		}
		return adapters.HookOutcome{Behavior: api.BehaviorAsk, Rule: pol.Rule, Message: pol.Message}, true
	}
	return adapters.HookOutcome{}, false
}

// grantPermission answers a permission request from a session grant, when
// one covers the call and policy didn't say deny or ask at this checkpoint.
// This matters most for Codex, whose PreToolUse hook ignores "allow": there
// the permission checkpoint is the only place a grant can take effect.
// Policy "allow" is deliberately not applied here: a call that reached the
// permission checkpoint after an "ask" (which Codex can't express at
// pre_tool) must still reach a person. Questions and plans are never
// granted: only the person can answer them (a grant for ExitPlanMode left
// over from 0.1 would otherwise approve every plan unseen).
func (d *Daemon) grantPermission(ctx context.Context, tx *store.Tx, p *permissionPlan) error {
	if p.pol.Action == api.BehaviorDeny || p.pol.Action == api.BehaviorAsk || asksRequest(p.req) || p.mustHold {
		return nil
	}
	grants, err := tx.Grants(ctx, p.req.SessionID)
	if err != nil {
		return err
	}
	g, ok := matchGrant(grants, p.req.Tool, p.req.ToolKind, p.req.Command)
	if !ok {
		return nil
	}
	now := tx.Now()
	p.granted = true
	p.req.Status, p.req.Route, p.req.ClosedAt = api.RequestDecided, "", &now
	p.req.Decision = &api.Decision{RequestID: p.req.ID, Behavior: api.BehaviorAllow, Scope: api.ScopeOnce,
		Message: "Allowed for this session in Pilothouse.", By: "grant:" + g.ID, At: now}
	return nil
}

// planPermission builds the request for a permission hook and picks its
// route: devices when the person is away (or always_hold), else the
// terminal's own dialog. A question or plan tool opens a question or plan
// request (questions.go), routed the same way.
func (d *Daemon) planPermission(h *hookCall, pol policy.Result) *permissionPlan {
	now := d.now()
	hold := d.cfg.Approvals.HoldLimit.Duration
	if max := h.ad.MaxHold(adapters.HookPermission) - holdMargin; max < hold {
		hold = max
	}
	ev := h.ev
	p := &permissionPlan{toolUseID: ev.ToolUseID, hold: hold, pol: pol, mustHold: ev.Hold}
	p.req = api.Request{
		ID: newID("r_"), Agent: h.env.Agent, Checkpoint: api.CheckpointPermission,
		Tool: ev.Tool, ToolKind: ev.ToolKind, Command: ev.Command, Paths: ev.Paths, Input: ev.Input,
		Summary: firstNonEmpty(ev.Summary, ev.Tool), Cwd: h.cwd,
		Route: api.RouteTerminal, Status: api.RequestSteppedAside,
		CreatedAt: now, ExpiresAt: now.Add(max(hold, 0)),
	}
	if pol.Action != "" {
		p.req.PolicyRule = pol.Rule
	}
	shapeAsk(&p.req, ev)
	d.enrichRequest(&p.req, h)
	away := d.Presence().State == api.PresenceAway
	if hold > 0 && (d.cfg.Approvals.AlwaysHold || away || p.mustHold) {
		if ch, ok := d.waiters.add(p.req.ID); ok {
			p.wait = ch
			p.req.Route, p.req.Status = api.RouteDevices, api.RequestOpen
		}
	}
	return p
}

// holdPermission waits for the outcome of a permission request routed to
// devices; terminal-routed requests return no opinion at once so the
// agent shows its own dialog.
func (d *Daemon) holdPermission(ctx context.Context, p *permissionPlan, sess api.Session) adapters.HookOutcome {
	req := p.req
	if p.wait == nil {
		d.scheduleBlockedNotice(req, sess)
		return adapters.HookOutcome{}
	}
	defer d.waiters.remove(req.ID)
	d.notify(core.Notification{Kind: core.NotifyRequest, Request: &req, Session: &sess, Remote: true})

	timer := time.NewTimer(p.hold)
	defer timer.Stop()
	var final api.Request
	select {
	case final = <-p.wait:
	case <-timer.C:
		final = d.endHold(p, api.RequestExpired, "hold limit reached")
	case <-ctx.Done():
		final = d.endHold(p, api.RequestExpired, "hook shim disconnected")
	case <-d.ctx.Done():
		// Shutdown releases every held request, but one that registered
		// during the release pass releases itself here.
		final = d.endHold(p, api.RequestSteppedAside, "the pilothouse daemon is stopping")
	}

	switch final.Status {
	case api.RequestDecided:
		if dec := final.Decision; dec != nil {
			return adapters.HookOutcome{Behavior: dec.Behavior, Message: askOutcomeMessage(final, dec), Answers: dec.Answers}
		}
	case api.RequestExpired:
		// The person is still away: tell their devices to use the terminal.
		d.notify(core.Notification{Kind: core.NotifyBlocked, Request: &final, Session: &sess, Remote: true})
	}
	return adapters.HookOutcome{}
}

// endHold closes a held request (expired, or stepped aside on shutdown).
// If someone else closed it first, their result is returned instead.
func (d *Daemon) endHold(p *permissionPlan, to api.RequestStatus, reason string) api.Request {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := d.closeRequest(ctx, p.req.ID, []api.RequestStatus{api.RequestOpen}, to)
	if err == nil {
		d.log.Info("hold ended", "request", p.req.ID, "status", to, "reason", reason)
		return r
	}
	if errors.Is(err, store.ErrAlreadyClosed) {
		// The winner resolves our waiter right after its commit.
		select {
		case r := <-p.wait:
			return r
		case <-time.After(2 * time.Second):
		}
		if r, err := d.db.Request(ctx, p.req.ID); err == nil {
			return r
		}
	}
	d.log.Error("end hold", "request", p.req.ID, "err", err)
	return p.req
}

// scheduleBlockedNotice sends a desktop notification if a native dialog is
// still unanswered after presence.notify_after.
func (d *Daemon) scheduleBlockedNotice(req api.Request, sess api.Session) {
	after := d.cfg.Presence.NotifyAfter.Duration
	if after <= 0 {
		return
	}
	d.goBackground(func() {
		t := time.NewTimer(after)
		defer t.Stop()
		select {
		case <-d.ctx.Done():
			return
		case <-t.C:
		}
		r, err := d.db.Request(d.ctx, req.ID)
		if err != nil || r.Status != api.RequestSteppedAside {
			return
		}
		d.notify(core.Notification{Kind: core.NotifyBlocked, Request: &r, Session: &sess, Remote: false})
	})
}

// sessionForHook finds the hook's session or creates it: by Pilothouse id for
// wrapped sessions, else by the agent's session id, else by agent PID.
func (d *Daemon) sessionForHook(ctx context.Context, tx *store.Tx, h *hookCall) (api.Session, error) {
	err := store.ErrNotFound
	var s api.Session
	switch id := h.env.PilothouseSessionID; {
	case id != "" && validSessionID(id):
		s, err = tx.Session(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			// The hook beat the holder's registration; the session is
			// completed when the holder registers.
			return d.createSession(ctx, tx, h, id)
		}
	case h.ev.AgentSessionID != "":
		s, err = tx.SessionByAgentID(ctx, h.env.Agent, h.ev.AgentSessionID)
	case h.agentPID > 0:
		s, err = tx.LiveSessionByPID(ctx, h.env.Agent, h.agentPID)
	}
	if err == nil {
		return s, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return s, err
	}
	if s, ok, err := adoptWrapped(ctx, tx, h); ok || err != nil {
		return s, err
	}
	return d.createSession(ctx, tx, h, newID("s_"))
}

// adoptWrapped links a hook that arrived through a shared agent server to
// the `pilothouse run` session it belongs to. Such hooks run with the
// server's environment, so PILOTHOUSE_SESSION_ID is missing; without this
// every wrapped Codex TUI session would also show up as a second, observed
// one. Only an unambiguous match is adopted: exactly one live wrapped
// session of the same agent and cwd that has no agent session id yet.
func adoptWrapped(ctx context.Context, tx *store.Tx, h *hookCall) (api.Session, bool, error) {
	if !h.shared || h.env.PilothouseSessionID != "" || h.ev.AgentSessionID == "" {
		return api.Session{}, false, nil
	}
	live, err := tx.LiveSessions(ctx)
	if err != nil {
		return api.Session{}, false, err
	}
	var match []api.Session
	for _, s := range live {
		if s.Mode == api.ModeWrapped && s.Agent == h.env.Agent && s.AgentSessionID == "" && s.Cwd == h.cwd {
			match = append(match, s)
		}
	}
	if len(match) != 1 {
		return api.Session{}, false, nil
	}
	return match[0], true, nil
}

func (d *Daemon) createSession(ctx context.Context, tx *store.Tx, h *hookCall, id string) (api.Session, error) {
	now := tx.Now()
	s := api.Session{
		ID: id, Agent: h.env.Agent, Mode: h.mode, Machine: d.machine,
		Title: titleFor(h.root, h.cwd, h.env.Agent), Cwd: h.cwd, RepoRoot: h.root, Branch: h.branch,
		PID: h.agentPID, AgentSessionID: h.ev.AgentSessionID, TranscriptPath: h.ev.TranscriptPath,
		TaskID: h.env.Env["PILOTHOUSE_TASK_ID"], State: api.StateStarting, StartedAt: now, UpdatedAt: now,
	}
	return d.saveSession(ctx, tx, nil, s)
}

// applyHook folds what a hook tells us about the session into it.
func applyHook(s *api.Session, h *hookCall, now time.Time) {
	ev := h.ev
	if ev.AgentSessionID != "" {
		s.AgentSessionID = ev.AgentSessionID
	}
	if ev.TranscriptPath != "" {
		s.TranscriptPath = ev.TranscriptPath
	}
	if s.Cwd == "" {
		s.Cwd = h.cwd
	}
	if h.root != "" && (s.RepoRoot == "" || s.RepoRoot == h.root) {
		s.RepoRoot, s.Branch = h.root, h.branch
	}
	if s.TaskID == "" {
		s.TaskID = h.env.Env["PILOTHOUSE_TASK_ID"]
	}
	if s.ExitedAt != nil && ev.Kind != adapters.HookSessionEnd {
		// A resumed conversation (same agent session id) is alive again.
		s.ExitedAt, s.ExitCode, s.PID = nil, nil, h.agentPID
	}
	if s.PID == 0 {
		s.PID = h.agentPID
	}
	switch ev.Kind {
	case adapters.HookStop:
		if msg := strings.TrimSpace(ev.Message); msg != "" {
			s.LastMessage = truncate(msg, maxLastMessage)
		}
	case adapters.HookSessionEnd:
		if s.ExitedAt == nil {
			t := now
			s.ExitedAt = &t
		}
	}
}

// addHookEvidence records the state a hook is evidence of. Permission hooks
// add none: the pending request itself keeps the session blocked (resolver
// rule 2), and once it closes the session should fall back to the hook
// before it, not stay "blocked".
func addHookEvidence(ctx context.Context, tx *store.Tx, sessionID string, ev adapters.HookEvent) error {
	if ev.Kind == adapters.HookPermission {
		return nil
	}
	st, kind, detail, ok := state.FromHook(ev)
	if !ok {
		return nil
	}
	return tx.AddEvidence(ctx, sessionID, api.StateEvidence{At: tx.Now(), Layer: api.LayerHook,
		State: st, Kind: kind, Detail: truncate(detail, 200)})
}

// loggedEvent is the hook as kept in the event log: normalized, with very
// large tool input dropped.
func loggedEvent(ev adapters.HookEvent) adapters.HookEvent {
	if len(ev.Input) > maxLoggedInput {
		ev.Input = nil
	}
	return ev
}

// agentPID is the nearest ancestor of the hook shim that is the agent, or
// 0 when that process is a shared agent server (Codex's `codex app-server
// --managed-daemon` runs the hooks of every TUI session): its PID says
// nothing about whether one session is still alive, so such sessions are
// left to idle expiry instead of PID liveness.
func agentPID(ad adapters.Adapter, ancestors []adapters.Proc) int {
	names := ad.ProcessNames()
	for _, p := range ancestors {
		base := filepath.Base(p.Comm)
		for _, n := range names {
			if base == n {
				if sharedAgentServer(p) {
					return 0
				}
				return p.PID
			}
		}
	}
	return 0
}

// underSharedServer reports whether the hook's agent ancestor is a shared
// agent server.
func underSharedServer(ad adapters.Adapter, ancestors []adapters.Proc) bool {
	names := ad.ProcessNames()
	for _, p := range ancestors {
		for _, n := range names {
			if filepath.Base(p.Comm) == n {
				return sharedAgentServer(p)
			}
		}
	}
	return false
}

// sharedAgentServer reports whether p is an agent's multi-session server.
func sharedAgentServer(p adapters.Proc) bool {
	for _, f := range strings.Fields(p.Args) {
		if f == "app-server" {
			return true
		}
	}
	return false
}

func checkpointOf(k adapters.HookKind) api.Checkpoint {
	if k == adapters.HookPermission {
		return api.CheckpointPermission
	}
	return api.CheckpointPreTool
}

func decisionMessage(dec *api.Decision) string {
	if dec.Message != "" {
		return dec.Message
	}
	if dec.Behavior == api.BehaviorDeny {
		return "Denied in Pilothouse (" + dec.By + ")."
	}
	return "Allowed in Pilothouse (" + dec.By + ")."
}

func titleFor(root, cwd string, agent api.AgentKind) string {
	switch {
	case root != "":
		return filepath.Base(root)
	case cwd != "":
		return filepath.Base(cwd)
	}
	return string(agent)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// truncate cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
