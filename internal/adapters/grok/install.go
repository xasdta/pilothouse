package grok

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/hookkit"
)

var owned = hookkit.OwnedBy("grok")

// wiredEvents are the Grok events Pilothouse listens to. PreToolUse is the
// only one that may block; the others keep the session's state and last
// message current. This is one personal file, but Grok runs personal hooks
// only in a folder the person has trusted (headless `grok -p` runs trust
// their folder for them); see docs/AGENTS.md.
var wiredEvents = []struct {
	event, matcher string
	timeout        int
}{
	{"SessionStart", "", 0},
	{"SessionEnd", "", 5},
	{"UserPromptSubmit", "", 0},
	{"PreToolUse", "*", PreToolTimeout},
	{"PostToolUse", "*", 0},
	{"PostToolUseFailure", "*", 0},
	{"PermissionDenied", "*", 0},
	{"Stop", "", 0},
	{"StopFailure", "", 0},
	{"StopCancelled", "", 0},
	{"Notification", "", 0},
	{"SubagentStart", "", 0},
	{"SubagentStop", "", 0},
	{"PreCompact", "", 0},
	{"PostCompact", "", 0},
}

func specs(shimPath string) []hookkit.Spec {
	cmd := hookkit.ShimCommand(shimPath, "grok")
	out := make([]hookkit.Spec, len(wiredEvents))
	for i, w := range wiredEvents {
		out[i] = hookkit.Spec{Event: w.event, Matcher: w.matcher, Command: cmd, Timeout: w.timeout}
	}
	return out
}

// InstallHooks writes the handlers into ~/.grok/hooks/pilothouse.json,
// merging with what is already there and backing the file up first.
func (Adapter) InstallHooks(opts adapters.InstallOptions) (adapters.Change, error) {
	if opts.ShimPath == "" {
		return adapters.Change{}, errors.New("install: no pilothouse binary path")
	}
	p, err := hookkit.PlanJSON(HooksPath(), specs(opts.ShimPath), owned)
	if err != nil {
		return adapters.Change{}, err
	}
	summary := "Grok Build hooks are up to date in " + HooksPath()
	if p.Result.Changed {
		summary = fmt.Sprintf("Wire %d Grok Build hook events to %s", len(wiredEvents), hookkit.ShimCommand(opts.ShimPath, "grok"))
	}
	ch := p.Change(summary)
	if opts.DryRun {
		return ch, nil
	}
	return ch, p.Commit(&ch, time.Now())
}

// UninstallHooks removes Pilothouse's handlers and leaves every other hook
// and key untouched.
func (Adapter) UninstallHooks(opts adapters.InstallOptions) (adapters.Change, error) {
	p, err := hookkit.PlanJSON(HooksPath(), nil, owned)
	if err != nil {
		return adapters.Change{}, err
	}
	summary := "No Pilothouse hooks in " + HooksPath()
	if p.Result.Changed {
		summary = fmt.Sprintf("Remove %d Pilothouse hook handlers from %s", len(p.Result.Found), HooksPath())
	}
	ch := p.Change(summary)
	if opts.DryRun {
		return ch, nil
	}
	return ch, p.Commit(&ch, time.Now())
}

// Status reports wired events, whether they match what InstallHooks would
// write, and anything that stops Grok from running them.
func (Adapter) Status(opts adapters.InstallOptions) (adapters.HookStatus, error) {
	st := adapters.HookStatus{Path: HooksPath()}
	p, err := hookkit.PlanJSON(HooksPath(), specs(opts.ShimPath), owned)
	if err != nil {
		st.Problems = append(st.Problems, err.Error())
		return st, nil
	}
	have := map[string]bool{}
	for _, f := range p.Result.Found {
		if !have[f.Event] {
			st.Events = append(st.Events, f.Event)
		}
		have[f.Event] = true
	}
	st.Installed = len(st.Events) > 0
	st.Problems = hookkit.Problems(p.Result.Found)
	var missing []string
	for _, w := range wiredEvents {
		if !have[w.event] {
			missing = append(missing, w.event)
		}
	}
	if st.Installed && len(missing) > 0 {
		st.Problems = append(st.Problems, "not wired: "+strings.Join(missing, ", ")+"; run `pilothouse hooks install --agent grok`")
	}
	current := !p.Result.Changed
	if opts.ShimPath == "" {
		current = len(missing) == 0
	}
	st.Current = st.Installed && current && len(st.Problems) == 0
	return st, nil
}
