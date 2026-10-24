package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/policy"
	"github.com/xasdta/pilothouse/internal/risk"
	"github.com/xasdta/pilothouse/internal/store"
)

// Rules from the inbox (F5; workstream D). Changes go to the managed rules
// file (policy.EditManaged: locked, validated, atomic); built-in,
// hand-written, preset and repo rules are read-only here. Every change
// reloads the engine and emits policy.changed with an api.RuleChange.

var _ core.RuleEditor = (*Daemon)(nil)

// ruleEngine is what the rule editor needs beyond PolicyEngine;
// *policy.Engine has it, test fakes usually don't.
type ruleEngine interface {
	PolicyEngine
	RuleInfos(repos []string) []api.RuleInfo
	PresetsView() []api.Preset
	ManagedFile() string
	Home() string
}

func (d *Daemon) ruleEngine() (ruleEngine, error) {
	re, ok := d.engine().(ruleEngine)
	if !ok || re.ManagedFile() == "" {
		return nil, fmt.Errorf("%w: this daemon's policy engine has no managed rules file", core.ErrUnsupported)
	}
	return re, nil
}

// liveRepos are the repo roots of live sessions, whose repo rules the
// Rules page lists.
func (d *Daemon) liveRepos(ctx context.Context) []string {
	live, err := d.db.LiveSessions(ctx)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range live {
		if s.RepoRoot != "" && !seen[s.RepoRoot] {
			seen[s.RepoRoot] = true
			out = append(out, s.RepoRoot)
		}
	}
	sort.Strings(out)
	return out
}

// Rules lists every loaded rule and the presets.
func (d *Daemon) Rules(ctx context.Context) (api.RulesView, error) {
	re, err := d.ruleEngine()
	if err != nil {
		return api.RulesView{}, err
	}
	return api.RulesView{
		Rules:    re.RuleInfos(d.liveRepos(ctx)),
		Presets:  re.PresetsView(),
		Problems: re.Status().Errors,
	}, nil
}

// localCaller reports whether by names a person at this machine (CLI or
// TUI over the guarded Unix socket) rather than a paired device.
func localCaller(by string) bool { return by == "cli" || by == "tui" }

// ruleErr maps a managed-file error for the API: validation problems and
// rules that aren't managed are core.ErrInvalid with a readable message,
// unknown rules core.ErrNotFound.
func ruleErr(re ruleEngine, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, policy.ErrInvalidRule):
		return core.Invalid("%s", strings.TrimPrefix(err.Error(), policy.ErrInvalidRule.Error()+": "))
	case errors.Is(err, policy.ErrNoManagedRule):
		return fmt.Errorf("%w: %v", core.ErrNotFound, err)
	}
	return fmt.Errorf("updating %s: %w", re.ManagedFile(), err)
}

// changed reloads the engine and records a rule change.
func (d *Daemon) changed(ctx context.Context, op, name, by string) {
	d.reloadPolicy()
	ch := api.RuleChange{Op: op, Name: name, By: by, At: d.now()}
	if _, err := d.Emit(ctx, api.EventPolicyChanged, "", "", ch); err != nil {
		d.log.Warn("policy.changed event not recorded", "op", op, "rule", name, "err", err)
	}
	d.log.Info("rules changed", "op", op, "rule", name, "by", by)
}

// AddRule adds a managed rule. Devices can't add `when:` expressions, and
// no rule may reuse the name of another loaded rule (built-in, user,
// preset, repo or managed).
func (d *Daemon) AddRule(ctx context.Context, spec api.RuleSpec, by string) (api.RuleInfo, error) {
	re, err := d.ruleEngine()
	if err != nil {
		return api.RuleInfo{}, err
	}
	if by == "" {
		return api.RuleInfo{}, badRequest("rule has no author")
	}
	spec.Name = strings.TrimSpace(spec.Name)
	spec.Description = strings.TrimSpace(spec.Description)
	spec.Message = strings.TrimSpace(spec.Message)
	spec.When = strings.TrimSpace(spec.When)
	if err := policy.ValidateRuleSpec(spec, policy.ValidateOptions{AllowWhen: localCaller(by), Home: re.Home()}); err != nil {
		return api.RuleInfo{}, ruleErr(re, err)
	}
	if err := policy.NameInUse(spec.Name, re.RuleInfos(d.liveRepos(ctx))); err != nil {
		return api.RuleInfo{}, ruleErr(re, err)
	}
	now := d.now()
	if err := policy.AddManagedRule(re.ManagedFile(), re.Home(), spec, by, now); err != nil {
		return api.RuleInfo{}, ruleErr(re, err)
	}
	d.changed(ctx, "add", spec.Name, by)
	return api.RuleInfo{RuleSpec: spec, Source: api.RuleSourceManaged, File: re.ManagedFile(), Enabled: true,
		Managed: true, Created: now.Truncate(time.Second), By: by}, nil
}

// notManaged explains a name the managed file doesn't have.
func (d *Daemon) notManaged(ctx context.Context, re ruleEngine, name string, err error) error {
	if errors.Is(err, policy.ErrNoManagedRule) {
		err = policy.NotManagedError(name, re.RuleInfos(d.liveRepos(ctx)))
	}
	return ruleErr(re, err)
}

// SetRuleEnabled turns a managed rule on or off.
func (d *Daemon) SetRuleEnabled(ctx context.Context, name string, enabled bool, by string) error {
	re, err := d.ruleEngine()
	if err != nil {
		return err
	}
	changed, err := policy.SetManagedRuleEnabled(re.ManagedFile(), re.Home(), name, enabled)
	if err != nil || !changed {
		return d.notManaged(ctx, re, name, err)
	}
	op := "disable"
	if enabled {
		op = "enable"
	}
	d.changed(ctx, op, name, by)
	return nil
}

// RemoveRule deletes a managed rule.
func (d *Daemon) RemoveRule(ctx context.Context, name string, by string) error {
	re, err := d.ruleEngine()
	if err != nil {
		return err
	}
	if err := policy.RemoveManagedRule(re.ManagedFile(), re.Home(), name); err != nil {
		return d.notManaged(ctx, re, name, err)
	}
	d.changed(ctx, "remove", name, by)
	return nil
}

// SetPreset switches a preset on or off.
func (d *Daemon) SetPreset(ctx context.Context, name string, enabled bool, by string) error {
	re, err := d.ruleEngine()
	if err != nil {
		return err
	}
	changed, err := policy.SetManagedPreset(re.ManagedFile(), re.Home(), name, enabled, by, d.now())
	if err != nil || !changed {
		return ruleErr(re, err)
	}
	op := "preset_off"
	if enabled {
		op = "preset_on"
	}
	d.changed(ctx, op, name, by)
	return nil
}

// Limits on a hypothetical call.
const (
	maxTestCommand = 16 << 10
	maxTestPath    = 4096
)

var agentKindRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// TestRule evaluates a hypothetical tool call against the rules: "would
// this be allowed?". Nothing is recorded.
func (d *Daemon) TestRule(ctx context.Context, in api.RuleTestInput) (api.RuleTestResult, error) {
	switch {
	case in.Command == "" && in.Path == "":
		return api.RuleTestResult{}, core.Invalid("give a command or a path to test")
	case len(in.Command) > maxTestCommand || !utf8.ValidString(in.Command):
		return api.RuleTestResult{}, core.Invalid("the command is too long")
	case len(in.Path) > maxTestPath || len(in.Cwd) > maxTestPath || !utf8.ValidString(in.Path+in.Cwd):
		return api.RuleTestResult{}, core.Invalid("the path is too long")
	case in.Agent != "" && !agentKindRE.MatchString(string(in.Agent)):
		return api.RuleTestResult{}, core.Invalid("unknown agent %q", in.Agent)
	case len(in.Tool) > 128:
		return api.RuleTestResult{}, core.Invalid("tool name is too long")
	}
	pin := testInput(in)
	if pin.Cwd != "" {
		// A hypothetical file may be in a directory that doesn't exist
		// yet: ask git about the nearest one that does.
		pin.Repo, pin.Branch = d.git.lookup(ctx, existingDir(pin.Cwd))
	}
	pin.Now = d.opts.Now()
	res := d.engine().Evaluate(pin)
	out := api.RuleTestResult{Behavior: res.Action, Rule: res.Rule, Message: res.Message}
	var ms []policy.Match
	for _, m := range res.Matched {
		if m.Ignored == "" {
			ms = append(ms, m)
		}
	}
	sort.SliceStable(ms, func(i, j int) bool { return behaviorRank(ms[i].Action) > behaviorRank(ms[j].Action) })
	for _, m := range ms {
		out.Matches = append(out.Matches, m.Rule)
	}
	return out, nil
}

// existingDir is p or its nearest existing ancestor directory.
func existingDir(p string) string {
	for p = filepath.Clean(p); ; p = filepath.Dir(p) {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return p
		}
		if p == filepath.Dir(p) {
			return p
		}
	}
}

// testInput builds the policy input a real hook would carry.
func testInput(in api.RuleTestInput) policy.Input {
	agent := in.Agent
	if agent == "" {
		agent = api.AgentClaude
	}
	tool := in.Tool
	if tool == "" {
		tool = "Edit"
		if in.Command != "" {
			tool = "Bash"
		}
	}
	kind := policy.GuessToolKind(tool)
	cwd := in.Cwd
	if cwd == "" && filepath.IsAbs(in.Path) {
		cwd = filepath.Dir(in.Path)
	}
	pin := policy.Input{Agent: agent, Tool: tool, ToolKind: kind, Command: in.Command, Cwd: cwd, Checkpoint: api.CheckpointPreTool}
	raw := map[string]string{}
	if in.Command != "" {
		raw["command"] = in.Command
	}
	if in.Path != "" {
		pin.Paths = []string{in.Path}
		raw["file_path"] = in.Path
	}
	pin.Input, _ = json.Marshal(raw)
	return pin
}

func behaviorRank(b api.Behavior) int {
	switch b {
	case api.BehaviorDeny:
		return 3
	case api.BehaviorAsk:
		return 2
	case api.BehaviorAllow:
		return 1
	}
	return 0
}

// ---- suggestions -----------------------------------------------------------

const (
	suggestWindow  = 7 * 24 * time.Hour
	suggestHistory = 2000 // closed requests looked at for repeated approvals
	minRepeats     = 2
)

// SuggestRules proposes one-tap rules for a request, built from its
// command key (as session grants are): allow it in this repo, ask before
// it everywhere, never allow it, or allow edits under its directory.
// Suggestions that match repeated approvals in the last week come first,
// with a reason. None of them ever allows everything, and each one
// matches the request it came from.
func (d *Daemon) SuggestRules(ctx context.Context, requestID string) ([]api.RuleSuggestion, error) {
	re, err := d.ruleEngine()
	if err != nil {
		return nil, err
	}
	req, err := d.Request(ctx, requestID)
	if err != nil {
		return nil, err
	}
	var sess api.Session
	if req.SessionID != "" {
		sess, _ = d.db.Session(ctx, req.SessionID)
	}
	repo, branch := sess.RepoRoot, sess.Branch
	if repo == "" && req.Cwd != "" {
		repo, branch = d.git.lookup(ctx, req.Cwd)
	}
	home := re.Home()
	out := suggestionsFor(req, repo, branch, home, re.RuleInfos(nil))
	d.rankByApprovals(ctx, out, home)
	return out, nil
}

// suggestionsFor builds the suggestions for a request and keeps those
// that match it, pass the same validation as a device's AddRule and
// aren't already a managed rule; names are made unique.
func suggestionsFor(req api.Request, repo, branch, home string, infos []api.RuleInfo) []api.RuleSuggestion {
	taken := map[string]bool{}
	for _, ri := range infos {
		taken[ri.Name] = true
	}
	in := requestInput(req, repo, branch)
	var out []api.RuleSuggestion
	for _, s := range buildSuggestions(req, repo, branch, home) {
		if ok, err := policy.MatchSpec(s.Rule, in, home); err != nil || !ok {
			continue // a suggestion must cover the call it was made for
		}
		if policy.ValidateRuleSpec(s.Rule, policy.ValidateOptions{Home: home}) != nil || existingRule(infos, s.Rule) {
			continue
		}
		s.Rule.Name = uniqueName(s.Rule.Name, taken)
		taken[s.Rule.Name] = true
		s.ID = suggestionID(req.ID, s.Rule)
		out = append(out, s)
	}
	return out
}

func requestInput(req api.Request, repo, branch string) policy.Input {
	return policy.Input{Agent: req.Agent, Tool: req.Tool, ToolKind: req.ToolKind, Command: req.Command, Paths: req.Paths,
		Input: req.Input, Cwd: req.Cwd, Repo: repo, Branch: branch, Checkpoint: api.CheckpointPreTool}
}

// rankByApprovals counts, for each allow suggestion, the requests a
// person allowed in the last week that it would have covered, and moves
// the ones with repeats to the front.
func (d *Daemon) rankByApprovals(ctx context.Context, sg []api.RuleSuggestion, home string) {
	var allowIdx []int
	for i, s := range sg {
		if s.Rule.Action == api.BehaviorAllow {
			allowIdx = append(allowIdx, i)
		}
	}
	if len(allowIdx) == 0 {
		return
	}
	past, err := d.db.Requests(ctx, store.RequestFilter{Closed: true, Limit: suggestHistory})
	if err != nil {
		return
	}
	since := d.now().Add(-suggestWindow)
	sessions := map[string]api.Session{}
	counts := make([]int, len(sg))
	for _, r := range past {
		dec := r.Decision
		if r.CreatedAt.Before(since) || r.Status != api.RequestDecided || dec == nil || dec.Behavior != api.BehaviorAllow ||
			strings.HasPrefix(dec.By, "grant:") || strings.HasPrefix(dec.By, "policy:") {
			continue
		}
		s, ok := sessions[r.SessionID]
		if !ok {
			s, _ = d.db.Session(ctx, r.SessionID)
			sessions[r.SessionID] = s
		}
		in := requestInput(r, s.RepoRoot, s.Branch)
		for _, i := range allowIdx {
			if ok, _ := policy.MatchSpec(sg[i].Rule, in, home); ok {
				counts[i]++
			}
		}
	}
	for i := range sg {
		if counts[i] >= minRepeats {
			sg[i].Reason = fmt.Sprintf("You allowed this %d times this week", counts[i])
		}
	}
	idx := make([]int, len(sg))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		ca, cb := counts[idx[a]], counts[idx[b]]
		if ca < minRepeats {
			ca = 0
		}
		if cb < minRepeats {
			cb = 0
		}
		return ca > cb
	})
	sorted := make([]api.RuleSuggestion, len(sg))
	for i, j := range idx {
		sorted[i] = sg[j]
	}
	copy(sg, sorted)
}

// existingRule: a managed rule with the same action and match exists.
func existingRule(infos []api.RuleInfo, spec api.RuleSpec) bool {
	for _, ri := range infos {
		if ri.Managed && ri.Action == spec.Action && reflect.DeepEqual(ri.Match, spec.Match) {
			return true
		}
	}
	return false
}

func uniqueName(name string, taken map[string]bool) string {
	if !taken[name] {
		return name
	}
	for i := 2; ; i++ {
		if n := fmt.Sprintf("%s-%d", name, i); !taken[n] {
			return n
		}
	}
}

func suggestionID(requestID string, spec api.RuleSpec) string {
	b, _ := json.Marshal(spec)
	h := sha256.Sum256(append([]byte(requestID+"\n"), b...))
	return "sg_" + hex.EncodeToString(h[:8])
}

// neverAllow are programs an allow suggestion must not cover: they run
// arbitrary code or commands, or reach other machines.
var neverAllow = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true, "ksh": true, "eval": true, "exec": true,
	"sudo": true, "doas": true, "su": true, "env": true, "xargs": true, "nohup": true, "time": true, "timeout": true,
	"python": true, "python3": true, "node": true, "ruby": true, "perl": true, "php": true, "deno": true, "osascript": true,
	"ssh": true, "scp": true, "curl": true, "wget": true, "nc": true, "rm": true, "mv": true, "cp": true, "dd": true,
	"chmod": true, "chown": true, "kill": true, "pkill": true, "source": true, ".": true, "pilothouse": true,
}

// neverAllowKeys are subcommands a suggestion never allows: they run
// arbitrary code, or (git push) are what people want to see.
var neverAllowKeys = map[string]bool{
	"docker run": true, "docker exec": true, "podman run": true, "kubectl exec": true,
	"git config": true, "npm exec": true, "pnpm dlx": true, "pnpm exec": true, "yarn dlx": true, "bun x": true,
	"git push": true,
}

// askWorthy keys get an "always ask" suggestion even without a risk label.
var askWorthy = map[string]bool{"git push": true}

// shellKey is the key of one simple command of a line.
type shellKey struct {
	key     string
	program bool // no subcommand could be found: the key is the program
	labels  []api.RiskLabel
}

// shellKeys returns the command keys of the line's simple commands with
// the risk of each, and whether the line is simple enough to allow: every
// command keyed (or harmless: cd, echo, a filter reading the pipe), none
// behind sudo/env/time or assignments.
func shellKeys(command, cwd, repo, branch string) (keys []shellKey, allowable bool) {
	line := policy.ParseShell(command)
	allowable = len(line.Commands) > 0
	seen := map[string]bool{}
	for _, c := range line.Commands {
		argv, wraps := policy.UnwrapCommand(c.Words)
		if len(argv) == 0 {
			continue
		}
		prog := policy.CommandName(argv[0])
		switch prog {
		case "cd", "echo", "printf", "true", "false", ":", "pwd":
			continue
		case "head", "tail", "grep", "wc", "sort", "uniq", "jq", "cat", "rg":
			if c.PipedIn {
				continue // a filter on the output
			}
		}
		if len(wraps) > 0 || len(argv) != len(c.Words) {
			allowable = false
		}
		key, ok := commandKey(argv)
		if !ok {
			allowable = false
			key = prog
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, shellKey{key: key, program: !ok,
			labels: risk.Classify(risk.Call{Kind: api.ToolShell, Command: strings.Join(argv, " "), Cwd: cwd, Repo: repo, Branch: branch})})
	}
	return keys, allowable
}

// keyGlobs are the command globs for a key: "prog sub" (or a program
// whose arguments are all alike, like mkdir) matches with any arguments;
// a bare program name matches only itself, unless program says the key
// stands for every call of the program.
func keyGlobs(key string, program bool) []string {
	k := policy.EscapeGlob(key)
	if !program && !strings.Contains(key, " ") && !wholeProgramTools[key] {
		return []string{k}
	}
	return []string{k, k + " *"}
}

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string, n int) string {
	s = strings.Trim(slugRE.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > n {
		s = strings.Trim(s[:n], "-")
	}
	if s == "" {
		s = "rule"
	}
	return s
}

func severe(ls []api.RiskLabel) (api.RiskLabel, bool) {
	for _, l := range ls {
		if l.Level == api.RiskHigh || l.Level == api.RiskMedium {
			return l, true
		}
	}
	return api.RiskLabel{}, false
}

// denyPatterns are the "Never allow …" suggestions for the most common
// dangerous commands, as precise globs (shell-aware matching applies).
var denyPatterns = []struct {
	code, title, name string
	globs             []string
	match             func(key string, ls []api.RiskLabel) bool
}{
	{api.RiskForcePush, "Never allow force-pushes", "never-force-push",
		[]string{"git push {-f,--force,--mirror}{, *}", "git push * {-f,--force,--mirror}{, *}", "git push * +*"},
		func(key string, _ []api.RiskLabel) bool { return key == "git push" }},
	{api.RiskDestructive, "Never allow `rm -rf`", "never-rm-rf",
		[]string{"rm {-r,-R,-rf,-fr,-Rf,-fR,-rfv,--recursive}{, *}", "rm * {-r,-R,-rf,-fr,-Rf,-fR,--recursive}{, *}"},
		func(key string, _ []api.RiskLabel) bool { return strings.HasPrefix(key, "rm") }},
	{api.RiskDestructive, "Never allow `git reset --hard`", "never-git-reset-hard",
		[]string{"git reset{, *} --hard{, *}"},
		func(key string, _ []api.RiskLabel) bool { return key == "git reset" }},
	{api.RiskDestructive, "Never allow `git clean`", "never-git-clean",
		[]string{"git clean{, *}"},
		func(key string, _ []api.RiskLabel) bool { return key == "git clean" }},
}

// buildSuggestions makes the candidate suggestions for a request, most
// useful first. SuggestRules checks, names and ranks them.
func buildSuggestions(req api.Request, repo, branch, home string) []api.RuleSuggestion {
	var out []api.RuleSuggestion
	repoName := filepath.Base(repo)
	repoGlob := policy.EscapeGlob(repo)
	switch req.ToolKind {
	case api.ToolShell:
		if req.Command == "" || len(req.Command) > maxTestCommand {
			return nil
		}
		keys, allowable := shellKeys(req.Command, req.Cwd, repo, branch)
		var safe []string
		var unsafe []shellKey
		for _, k := range keys {
			_, bad := severe(k.labels)
			code := neverAllow[policy.CommandName(strings.Fields(k.key)[0])] || neverAllowKeys[k.key]
			switch {
			case bad || askWorthy[k.key]:
				unsafe = append(unsafe, k)
			case code || k.program:
				allowable = false // nothing to suggest for it, and the line can't be allowed
			default:
				safe = append(safe, k.key)
			}
		}
		if repo != "" && allowable && len(unsafe) == 0 && len(safe) > 0 && len(safe) <= 3 {
			var globs, shown []string
			for _, k := range safe {
				globs = append(globs, keyGlobs(k, false)...)
				shown = append(shown, "`"+k+"`")
			}
			out = append(out, api.RuleSuggestion{
				Title: "Always allow " + strings.Join(shown, ", ") + " in " + repoName, Scope: "repo",
				Rule: api.RuleSpec{Name: "allow-" + slug(strings.Join(safe, " "), 40) + "-in-" + slug(repoName, 30),
					Description: "Added from an approval card", Action: api.BehaviorAllow,
					Match: map[string][]string{"kind": {"shell"}, "command": globs, "repo": {repoGlob}}},
			})
		}
		if len(unsafe) > 0 { // the first dangerous command is enough to talk about
			k := unsafe[0]
			reason := ""
			if l, ok := severe(k.labels); ok {
				reason = l.Text
			}
			out = append(out, api.RuleSuggestion{Title: "Always ask before `" + k.key + "`", Scope: "everywhere", Reason: reason,
				Rule: api.RuleSpec{Name: "ask-" + slug(k.key, 60), Description: "Added from an approval card", Action: api.BehaviorAsk,
					Match: map[string][]string{"kind": {"shell"}, "command": keyGlobs(k.key, k.program)}}})
			specific := false
			for _, dp := range denyPatterns {
				if hasCode(k.labels, dp.code) && dp.match(k.key, k.labels) {
					specific = true
					out = append(out, api.RuleSuggestion{Title: dp.title, Scope: "everywhere", Reason: reason,
						Rule: api.RuleSpec{Name: dp.name, Description: "Added from an approval card", Action: api.BehaviorDeny,
							Match: map[string][]string{"kind": {"shell"}, "command": dp.globs}}})
				}
			}
			if _, high := highLabel(k.labels); high && !k.program && !specific {
				out = append(out, api.RuleSuggestion{Title: "Never allow `" + k.key + "`", Scope: "everywhere", Reason: reason,
					Rule: api.RuleSpec{Name: "never-" + slug(k.key, 60), Description: "Added from an approval card", Action: api.BehaviorDeny,
						Match: map[string][]string{"kind": {"shell"}, "command": keyGlobs(k.key, false)}}})
			}
		}
	case api.ToolEdit:
		if repo == "" || len(req.Paths) == 0 {
			return nil
		}
		dir := ""
		for i, p := range req.Paths {
			abs := p
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(req.Cwd, p)
			}
			abs = filepath.Clean(abs)
			rel, err := filepath.Rel(repo, abs)
			if err != nil || rel == "." || strings.HasPrefix(rel, "..") || risk.IsSecretPath(abs, home) {
				return nil
			}
			top := rel
			if j := strings.IndexByte(rel, '/'); j >= 0 {
				top = rel[:j] + "/**"
			}
			if strings.HasPrefix(top, ".") { // .github, .claude, .env…: not one tap away
				return nil
			}
			if i == 0 {
				dir = top
			} else if dir != top {
				return nil
			}
		}
		title, name := "Allow edits under `"+dir+"` in "+repoName, "allow-edits-"+slug(strings.TrimSuffix(dir, "/**"), 30)+"-in-"+slug(repoName, 30)
		if !strings.HasSuffix(dir, "/**") {
			title = "Allow edits to `" + dir + "` in " + repoName
		}
		out = append(out, api.RuleSuggestion{Title: title, Scope: "repo",
			Rule: api.RuleSpec{Name: name, Description: "Added from an approval card", Action: api.BehaviorAllow,
				Match: map[string][]string{"kind": {"edit"}, "path": {repoGlob + "/" + policy.EscapeGlob(strings.TrimSuffix(dir, "/**")) + suffixIf(dir)}}}})
	case api.ToolMCP:
		if req.Tool == "" {
			return nil
		}
		out = append(out, api.RuleSuggestion{Title: "Always allow `" + req.Tool + "`", Scope: "everywhere",
			Rule: api.RuleSpec{Name: "allow-" + slug(req.Tool, 70), Description: "Added from an approval card", Action: api.BehaviorAllow,
				Match: map[string][]string{"tool": {policy.EscapeGlob(req.Tool)}}}})
		out = append(out, api.RuleSuggestion{Title: "Always ask before `" + req.Tool + "`", Scope: "everywhere",
			Rule: api.RuleSpec{Name: "ask-" + slug(req.Tool, 70), Description: "Added from an approval card", Action: api.BehaviorAsk,
				Match: map[string][]string{"tool": {policy.EscapeGlob(req.Tool)}}}})
	}
	return out
}

func suffixIf(dir string) string {
	if strings.HasSuffix(dir, "/**") {
		return "/**"
	}
	return ""
}

func hasCode(ls []api.RiskLabel, code string) bool {
	for _, l := range ls {
		if l.Code == code {
			return true
		}
	}
	return false
}

// highLabel finds a high destructive or deploy finding: the ones worth a
// "Never allow" for the whole subcommand (a push to main is not a reason
// to never push).
func highLabel(ls []api.RiskLabel) (api.RiskLabel, bool) {
	for _, l := range ls {
		if l.Level == api.RiskHigh && (l.Code == api.RiskDestructive || l.Code == api.RiskDeploy) {
			return l, true
		}
	}
	return api.RiskLabel{}, false
}
