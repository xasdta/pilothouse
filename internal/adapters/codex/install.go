package codex

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/hookkit"
)

var owned = hookkit.OwnedBy("codex")

// wiredEvents are the Codex events Pilothouse listens to. Codex has no
// Notification event; PermissionRequest is the "needs a person" signal.
// SessionEnd and Interrupt get 3 s (Codex's maximum for them; default 1 s)
// so the shim's own 1–2 s budget decides, not Codex's kill.
var wiredEvents = []struct {
	event, matcher string
	timeout        int
}{
	{"SessionStart", "", 0},
	{"SessionEnd", "", shortTimeout},
	{"UserPromptSubmit", "", 0},
	{"PreToolUse", "*", PreToolTimeout},
	{"PermissionRequest", "*", PermissionTimeout},
	{"PostToolUse", "*", 0},
	{"Stop", "", 0},
	{"SubagentStop", "", 0},
	{"Interrupt", "", shortTimeout},
}

func specs(shimPath string) []hookkit.Spec {
	cmd := hookkit.ShimCommand(shimPath, "codex")
	out := make([]hookkit.Spec, len(wiredEvents))
	for i, w := range wiredEvents {
		out[i] = hookkit.Spec{Event: w.event, Matcher: w.matcher, Command: cmd, Timeout: w.timeout}
	}
	return out
}

// plan is the two-file edit: hooks.json, then trust in config.toml.
type plan struct {
	hooks     hookkit.Plan
	config    hookkit.File
	newConfig string
	trustErr  error // config.toml could not be edited; hooks stay untrusted
}

func makePlan(sp []hookkit.Spec) (plan, error) {
	hp, err := hookkit.PlanJSON(HooksPath(), sp, owned)
	if err != nil {
		return plan{}, err
	}
	p := plan{hooks: hp}
	if p.config, err = hookkit.ReadFile(ConfigPath()); err != nil {
		return plan{}, err
	}
	edit := trustEdit{stale: map[string]bool{}, rename: map[string]string{}, want: map[string]string{}}
	path := HooksPath()
	for _, f := range hp.Result.Found {
		edit.stale[trustKey(path, f.Ref)] = true
	}
	for from, to := range hp.Result.Moved {
		edit.rename[trustKey(path, from)] = trustKey(path, to)
	}
	for _, s := range sp {
		edit.want[trustKey(path, hp.Result.Placed[s.Event])] = trustHash(s)
	}
	p.newConfig, p.trustErr = applyTrust(string(p.config.Data), edit)
	if p.trustErr != nil {
		p.newConfig = string(p.config.Data)
	}
	return p, nil
}

func (p plan) configChanged() bool { return p.newConfig != string(p.config.Data) }

func (p plan) change(summary string) adapters.Change {
	ch := p.hooks.Change(summary)
	ch.Changed = ch.Changed || p.configChanged()
	if p.configChanged() {
		ch.More = []adapters.Change{{
			Path:    p.config.Path,
			Changed: true,
			Before:  string(p.config.Data),
			After:   p.newConfig,
			Summary: "Record Codex hook trust for Pilothouse's hooks",
		}}
	}
	return ch
}

// commit writes hooks.json first: if config.toml then fails, Codex sees
// untrusted hooks and asks the person, which is safe; the reverse order
// could leave trust for hooks that do not exist.
func (p plan) commit(ch *adapters.Change) error {
	now := time.Now()
	if err := p.hooks.Commit(ch, now); err != nil {
		return err
	}
	if !p.configChanged() {
		return nil
	}
	backup, err := hookkit.Commit(p.config, []byte(p.newConfig), now)
	ch.More[0].Backup = backup
	if err != nil {
		return fmt.Errorf("hooks are in %s but trusting them in %s failed: %w", HooksPath(), ConfigPath(), err)
	}
	return nil
}

// InstallHooks writes our handlers into ~/.codex/hooks.json (merged,
// backed up, idempotent) and trusts exactly those handlers in
// ~/.codex/config.toml, preserving every comment and unrelated line.
// If config.toml uses a layout we do not rewrite, the hooks are installed
// untrusted and Codex asks the person to review them at startup.
func (Adapter) InstallHooks(opts adapters.InstallOptions) (adapters.Change, error) {
	if opts.ShimPath == "" {
		return adapters.Change{}, errors.New("install: no pilothouse binary path")
	}
	p, err := makePlan(specs(opts.ShimPath))
	if err != nil {
		return adapters.Change{}, err
	}
	summary := "Codex hooks are up to date in " + HooksPath()
	if p.hooks.Result.Changed || p.configChanged() {
		summary = fmt.Sprintf("Wire %d Codex hook events to %s", len(wiredEvents), hookkit.ShimCommand(opts.ShimPath, "codex"))
	}
	if p.trustErr != nil {
		summary += fmt.Sprintf(" (not trusted automatically: %v)", p.trustErr)
	}
	ch := p.change(summary)
	if opts.DryRun {
		return ch, nil
	}
	return ch, p.commit(&ch)
}

// UninstallHooks removes our handlers and their trust entries, and keeps
// the trust of the person's own hooks pointing at the right positions.
func (Adapter) UninstallHooks(opts adapters.InstallOptions) (adapters.Change, error) {
	p, err := makePlan(nil)
	if err != nil {
		return adapters.Change{}, err
	}
	summary := "No Pilothouse hooks in " + HooksPath()
	if p.hooks.Result.Changed || p.configChanged() {
		summary = fmt.Sprintf("Remove %d Pilothouse hook handlers from %s", len(p.hooks.Result.Found), HooksPath())
	}
	if p.trustErr != nil {
		summary += fmt.Sprintf(" (trust entries in config.toml left as they are: %v)", p.trustErr)
	}
	ch := p.change(summary)
	if opts.DryRun {
		return ch, nil
	}
	return ch, p.commit(&ch)
}

// Status reports wired events, whether they match what InstallHooks would
// write, and anything that stops Codex from running them.
func (Adapter) Status(opts adapters.InstallOptions) (adapters.HookStatus, error) {
	st := adapters.HookStatus{Path: HooksPath()}
	p, err := makePlan(specs(opts.ShimPath))
	if err != nil {
		st.Problems = append(st.Problems, err.Error())
		return st, nil
	}
	have := map[string]bool{}
	for _, f := range p.hooks.Result.Found {
		if !have[f.Event] {
			st.Events = append(st.Events, f.Event)
		}
		have[f.Event] = true
	}
	st.Installed = len(st.Events) > 0
	st.Problems = hookkit.Problems(p.hooks.Result.Found)
	var missing []string
	for _, w := range wiredEvents {
		if !have[w.event] {
			missing = append(missing, w.event)
		}
	}
	if st.Installed && len(missing) > 0 {
		st.Problems = append(st.Problems, "not wired: "+strings.Join(missing, ", ")+"; run `pilothouse hooks install --agent codex`")
	}
	doc, perr := parseTOML(string(p.config.Data))
	switch {
	case perr != nil:
		st.Problems = append(st.Problems, fmt.Sprintf("%s: %v", ConfigPath(), perr))
	case hooksFeatureOff(doc):
		st.Problems = append(st.Problems, "[features] hooks = false in config.toml turns Codex hooks off")
	case st.Installed:
		// Check trust the way Codex will: the key from the handler's real
		// position in hooks.json under Codex's own spelling of the path
		// (Home), the hash from the declaration as written there.
		if n := untrusted(doc, p.hooks.Result.Found); n > 0 {
			st.Problems = append(st.Problems, fmt.Sprintf("not trusted: Codex will skip %d Pilothouse hooks until they are trusted; run `pilothouse hooks install --agent codex`, or approve them in Codex (/hooks)", n))
		}
		if other := otherTrustPaths(doc); len(other) > 0 {
			st.Problems = append(st.Problems, fmt.Sprintf("Pilothouse's trust entries in %s name %s, but Codex reads %s (CODEX_HOME or a symlinked home?); run `pilothouse hooks install --agent codex` again", ConfigPath(), strings.Join(other, ", "), HooksPath()))
		}
	}
	current := !p.hooks.Result.Changed && !p.configChanged()
	if opts.ShimPath == "" {
		current = len(missing) == 0
	}
	st.Current = st.Installed && current && len(st.Problems) == 0
	return st, nil
}

// untrusted counts our handlers in hooks.json whose trust entry is
// missing or does not match the hash Codex computes for them.
func untrusted(doc *tomlDoc, found []hookkit.Found) int {
	n := 0
	for _, f := range found {
		spec := hookkit.Spec{Event: f.Event, Matcher: f.Matcher, Command: f.Command, Timeout: f.Timeout}
		if _, known := eventLabels[f.Event]; !known {
			continue // an event Codex does not have never runs anyway
		}
		if h, _ := trustState(doc, trustKey(HooksPath(), f.Ref)); h != trustHash(spec) {
			n++
		}
	}
	return n
}

// otherTrustPaths lists hooks.json paths named by our marked trust
// entries other than the one Codex reads: an install made under another
// spelling of the Codex home, which Codex will not match.
func otherTrustPaths(doc *tomlDoc) []string {
	lines := doc.lines()
	seen := map[string]bool{}
	var out []string
	for _, t := range doc.tables {
		if !isStateTable(t) || t.line == 0 || !isMarker(lines[t.line-1]) {
			continue
		}
		path, ok := trustKeyPath(t.path[2])
		if !ok || path == HooksPath() || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out
}

// trustKeyPath is the source path part of a trust key
// "<path>:<event label>:<group>:<handler>".
func trustKeyPath(key string) (string, bool) {
	parts := strings.Split(key, ":")
	if len(parts) < 4 {
		return "", false
	}
	return strings.Join(parts[:len(parts)-3], ":"), true
}
