// Package state resolves a session's state from the evidence the daemon has
// collected about it (BUILD-SPEC §7). It is pure: the daemon gathers the
// inputs, Resolve picks the state and names the rule that picked it so
// `pilothouse explain` can show its reasoning.
package state

import (
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

const (
	// StaleWorking is how long "working" evidence stays believable. Agents
	// emit hooks constantly while working; twenty silent minutes means we
	// no longer know what the session is doing.
	StaleWorking = 20 * time.Minute
	// ContradictionWindow: an OSC report that disagrees with a hook less than
	// this much older is treated as terminal-rendering lag, not news.
	ContradictionWindow = 2 * time.Second
	// StartGrace is how long a session that has only reported starting is
	// shown as "starting". Agents sit at their input box after starting, so
	// a session that has said nothing since is idle, not still starting.
	// The grace keeps a session started with a prompt from flashing
	// "waiting" before that prompt's first hook arrives.
	StartGrace = 30 * time.Second
)

// Rule names which resolver rule decided the state.
type Rule string

const (
	RuleExited      Rule = "exited"             // 1: process gone, holder exit or SessionEnd
	RulePermission  Rule = "permission-request" // 2: a permission request is pending
	RuleHook        Rule = "hook"               // 3: latest fresh hook evidence
	RuleOSC         Rule = "osc"                // 4: OSC evidence newer than any hook
	RuleHookOverOSC Rule = "hook-over-osc"      // 4: OSC contradicted a hook < 2 s older; hook kept
	RuleStale       Rule = "stale-working"      // 5: last evidence is old "working"
	RuleStarting    Rule = "starting"           // 6: nothing to go on yet
	RuleIdleStart   Rule = "idle-since-start"   // 3: started, then nothing for StartGrace
)

// Evidence is one observation about a session.
type Evidence struct {
	At     time.Time
	Layer  api.Layer
	State  api.State
	Kind   api.BlockKind
	Detail string
}

// Input is everything the resolver looks at.
type Input struct {
	Now time.Time
	// Exited is true when the holder reported exit, a SessionEnd hook
	// arrived, or layer L0 found the process gone.
	Exited     bool
	ExitDetail string
	// PendingPermission is true while an open or stepped-aside request
	// (permission, question or plan) exists for the session. PendingKind
	// says what it waits for; empty means permission.
	PendingPermission bool
	PermissionDetail  string
	PendingKind       api.BlockKind
	// HasHooks is true when the session's agent has a hook adapter. Without
	// one, OSC is the best evidence available and wins outright.
	HasHooks bool
	// Evidence must be ordered oldest first; among equal timestamps the
	// later entry is the newer.
	Evidence []Evidence
}

// Result is the resolved state.
type Result struct {
	State  api.State
	Kind   api.BlockKind
	Layer  api.Layer
	Detail string
	Rule   Rule
	// Disagreement describes an OSC report that was overruled by a hook.
	Disagreement string
	// Fresh[i] reports whether Evidence[i] still counts: it is the latest of
	// its layer and not stale.
	Fresh []bool
}

// Resolve applies the rules of BUILD-SPEC §7 in order; the first that
// applies wins.
func Resolve(in Input) Result {
	hook := latest(in.Evidence, api.LayerHook)
	osc := latest(in.Evidence, api.LayerOSC)
	res := Result{Fresh: make([]bool, len(in.Evidence))}
	if hook >= 0 && !stale(in.Evidence[hook], in.Now) {
		res.Fresh[hook] = true
	}
	if osc >= 0 && !stale(in.Evidence[osc], in.Now) {
		res.Fresh[osc] = true
	}

	switch {
	case in.Exited:
		return res.with(api.StateExited, "", api.LayerProcess, orDefault(in.ExitDetail, "Session ended"), RuleExited)
	case in.PendingPermission:
		kind := in.PendingKind
		if kind == "" {
			kind = api.BlockPermission
		}
		return res.with(api.StateBlocked, kind, api.LayerHook,
			orDefault(in.PermissionDetail, "Waiting for permission"), RulePermission)
	}

	// Rule 4 is checked before rule 3 because it overrides it: OSC that is
	// newer than every hook (or the only signal there is) wins.
	if osc >= 0 && res.Fresh[osc] {
		o := in.Evidence[osc]
		newer := hook < 0 || !in.HasHooks || after(in.Evidence, osc, hook)
		if newer {
			if in.HasHooks && hook >= 0 && res.Fresh[hook] && contradicts(o, in.Evidence[hook]) &&
				o.At.Sub(in.Evidence[hook].At) < ContradictionWindow {
				h := in.Evidence[hook]
				res.Disagreement = "terminal reported " + describe(o) + " " +
					o.At.Sub(h.At).Round(time.Millisecond).String() + " after a hook said " + describe(h)
				return res.with(h.State, h.Kind, h.Layer, h.Detail, RuleHookOverOSC)
			}
			return res.with(o.State, o.Kind, o.Layer, o.Detail, RuleOSC)
		}
	}
	if hook >= 0 && res.Fresh[hook] {
		h := in.Evidence[hook]
		if h.State == api.StateStarting && in.Now.Sub(h.At) > StartGrace {
			return res.with(api.StateWaiting, "", h.Layer, "Ready for a prompt", RuleIdleStart)
		}
		return res.with(h.State, h.Kind, h.Layer, h.Detail, RuleHook)
	}
	if n := len(in.Evidence); n > 0 {
		last := lastObservation(in.Evidence)
		if last >= 0 && in.Evidence[last].State == api.StateWorking && stale(in.Evidence[last], in.Now) {
			return res.with(api.StateUnknown, "", in.Evidence[last].Layer,
				"No activity for "+in.Now.Sub(in.Evidence[last].At).Round(time.Minute).String(), RuleStale)
		}
	}
	return res.with(api.StateStarting, "", "", "Starting", RuleStarting)
}

func (r Result) with(s api.State, k api.BlockKind, l api.Layer, detail string, rule Rule) Result {
	if s != api.StateBlocked {
		k = ""
	}
	r.State, r.Kind, r.Layer, r.Detail, r.Rule = s, k, l, detail, rule
	return r
}

// latest returns the index of the newest evidence of layer, or -1.
func latest(ev []Evidence, layer api.Layer) int {
	idx := -1
	for i, e := range ev {
		if e.Layer == layer && (idx < 0 || !e.At.Before(ev[idx].At)) {
			idx = i
		}
	}
	return idx
}

// lastObservation is the newest hook or OSC evidence; other layers (L0
// exits, manual notes) say nothing about whether the agent is working.
func lastObservation(ev []Evidence) int {
	idx := -1
	for i, e := range ev {
		if e.Layer != api.LayerHook && e.Layer != api.LayerOSC {
			continue
		}
		if idx < 0 || !e.At.Before(ev[idx].At) {
			idx = i
		}
	}
	return idx
}

// after reports whether ev[i] is newer than ev[j] (ties go to the later entry).
func after(ev []Evidence, i, j int) bool {
	if ev[i].At.Equal(ev[j].At) {
		return i > j
	}
	return ev[i].At.After(ev[j].At)
}

func stale(e Evidence, now time.Time) bool {
	return e.State == api.StateWorking && now.Sub(e.At) > StaleWorking
}

func contradicts(a, b Evidence) bool { return a.State != b.State || a.Kind != b.Kind }

func describe(e Evidence) string {
	if e.Kind != "" {
		return string(e.State) + "/" + string(e.Kind)
	}
	return string(e.State)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// NeedsAttention reports whether moving from (prevState, prevKind) to
// (state, kind) should mark the session Unseen: entering blocked, waiting,
// errored or exited, or changing why it is blocked.
func NeedsAttention(prevState api.State, prevKind api.BlockKind, state api.State, kind api.BlockKind) bool {
	switch state {
	case api.StateWaiting:
		// A session that started and was never used has finished nothing.
		return prevState != state && prevState != api.StateStarting
	case api.StateBlocked, api.StateErrored, api.StateExited:
		return prevState != state || prevKind != kind
	}
	return false
}

// FromOSC maps an OSC 7501 state to session state. ok is false for states
// the protocol doesn't define.
func FromOSC(oscState, kind string) (api.State, api.BlockKind, bool) {
	switch oscState {
	case "idle", "done":
		return api.StateWaiting, "", true
	case "working":
		return api.StateWorking, "", true
	case "blocked":
		switch k := api.BlockKind(kind); k {
		case api.BlockPermission, api.BlockQuestion, api.BlockAuth:
			return api.StateBlocked, k, true
		default:
			return api.StateBlocked, api.BlockQuestion, true
		}
	case "error":
		return api.StateErrored, "", true
	}
	return "", "", false
}
