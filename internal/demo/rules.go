package demo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
)

// Rules from the inbox (F5) on canned rules. The tester really evaluates
// them: globs over agent, tool, kind, command, path, cwd and repo, with
// deny over ask over allow like the daemon's engine.

const (
	userPolicy    = Home + "/.pilothouse/policy.yaml"
	managedPolicy = Home + "/.pilothouse/rules.yaml"
)

func seedPresets() []api.Preset {
	return []api.Preset{
		{
			Name:  "guarded-autopilot",
			Title: "Guarded autopilot",
			Description: "The safe alternative to --dangerously-skip-permissions. Routine reads, edits and commands run without asking; " +
				"destructive commands are blocked; pushes, deploys, publishing and secrets still ask you.",
			Enabled: true,
			Rules: []api.RuleSpec{
				{Name: "autopilot-deny-destructive", Action: api.BehaviorDeny, Description: "Never run commands that destroy work or data.",
					Match:   map[string][]string{"command": {"rm -rf /*", "rm -rf ~*", "rm -rf .", "git reset --hard*", "git clean -fdx*", "*DROP TABLE*", "*DROP DATABASE*", "mkfs*", "dd if=*"}},
					Message: "Blocked by guarded autopilot: this command destroys work. Ask the person to run it themselves."},
				{Name: "autopilot-ask-risky", Action: api.BehaviorAsk, Description: "Pushes, deploys and publishing always ask you.",
					Match: map[string][]string{"command": {"git push*", "npm publish*", "pnpm publish*", "docker push*", "kubectl apply*", "kubectl delete*", "kubectl rollout*", "terraform apply*", "fly deploy*", "vercel --prod*"}}},
				{Name: "autopilot-ask-secrets", Action: api.BehaviorAsk, Description: "Anything touching .env files, keys or credentials asks you.",
					Match: map[string][]string{"path": {"**/.env", "**/.env.*", "~/.ssh/**", "**/*.pem", "~/.aws/**"}}},
				{Name: "autopilot-allow-routine", Action: api.BehaviorAllow, Description: "Reads, edits and shell commands inside your repos run without asking.",
					Match: map[string][]string{"kind": {"read", "edit", "shell"}, "cwd": {Home + "/src/**"}}},
			},
		},
		{
			Name:        "protect-main",
			Title:       "Protect main",
			Description: "Pushes to main or master always ask you, and force-pushes to them are blocked — even when other rules allow git.",
			Rules: []api.RuleSpec{
				{Name: "protect-main-force", Action: api.BehaviorDeny, Description: "No force-pushes to main or master.",
					Match:   map[string][]string{"command": {"git push --force* main*", "git push -f* main*", "git push --force* master*", "git push * +main*"}},
					Message: "Force-pushing to main is blocked. Push a branch and open a pull request."},
				{Name: "protect-main-push", Action: api.BehaviorAsk, Description: "Pushes to main or master ask you.",
					Match: map[string][]string{"command": {"git push * main*", "git push * master*"}}},
			},
		},
		{
			Name:        "no-secrets",
			Title:       "No secrets",
			Description: "Agents can't read or print .env files, SSH keys or cloud credentials.",
			Enabled:     true,
			Rules: []api.RuleSpec{
				{Name: "no-secrets-files", Action: api.BehaviorDeny, Description: "Secret files stay unread.",
					Match:   map[string][]string{"path": {"**/.env", "**/.env.local", "**/.env.production", "~/.ssh/id_*", "~/.aws/credentials"}},
					Message: "Secret files are off limits. Ask the person for the value you need."},
				{Name: "no-secrets-print", Action: api.BehaviorDeny, Description: "No printing the environment or secret files.",
					Match: map[string][]string{"command": {"printenv*", "env", "cat *.env*", "cat ~/.ssh/*"}}},
			},
		},
		{
			Name:        "deploys-need-approval",
			Title:       "Deploys need approval",
			Description: "kubectl, terraform apply, fly deploy, vercel --prod and helm upgrade always ask you, whatever else is allowed.",
			Rules: []api.RuleSpec{
				{Name: "deploys-ask", Action: api.BehaviorAsk, Description: "Every deploy asks you.",
					Match: map[string][]string{"command": {"kubectl *", "terraform apply*", "fly deploy*", "vercel --prod*", "helm upgrade*", "helm install*"}}},
			},
		},
	}
}

func seedRules(t time.Time) []api.RuleInfo {
	return []api.RuleInfo{
		{RuleSpec: api.RuleSpec{Name: "allow-pnpm-test-proofline", Action: api.BehaviorAllow, Description: "Always allow `pnpm test` in proofline",
			Match: map[string][]string{"command": {"pnpm test*"}, "repo": {Home + "/src/proofline"}}},
			Source: api.RuleSourceManaged, File: managedPolicy, Enabled: true, Managed: true, Created: t.Add(-3 * day), By: "device:d_iphone0001"},
		{RuleSpec: api.RuleSpec{Name: "allow-go-test", Action: api.BehaviorAllow, Description: "Always allow `go test` everywhere",
			Match: map[string][]string{"command": {"go test*"}}},
			Source: api.RuleSourceManaged, File: managedPolicy, Enabled: true, Managed: true, Created: t.Add(-6 * day), By: "device:d_iphone0001"},
		{RuleSpec: api.RuleSpec{Name: "ask-docker-push", Action: api.BehaviorAsk, Description: "Always ask before `docker push`",
			Match: map[string][]string{"command": {"docker push*"}}},
			Source: api.RuleSourceManaged, File: managedPolicy, Enabled: false, Managed: true, Created: t.Add(-12 * day), By: "device:d_pixel00002"},

		{RuleSpec: api.RuleSpec{Name: "allow-read-only", Action: api.BehaviorAllow, Description: "Reading and searching files never needs you.",
			Match: map[string][]string{"kind": {"read"}}},
			Source: api.RuleSourceUser, File: userPolicy, Enabled: true},
		{RuleSpec: api.RuleSpec{Name: "ask-before-push", Action: api.BehaviorAsk, Description: "Ask before any git push.",
			Match: map[string][]string{"command": {"git push*"}}},
			Source: api.RuleSourceUser, File: userPolicy, Enabled: true},
		{RuleSpec: api.RuleSpec{Name: "no-curl-pipe-sh", Action: api.BehaviorDeny, Description: "Never pipe a downloaded script into a shell.",
			Match:   map[string][]string{"command": {"curl *| sh*", "curl *| bash*", "wget *| sh*"}},
			Message: "Download the script and read it first."},
			Source: api.RuleSourceUser, File: userPolicy, Enabled: true},

		{RuleSpec: api.RuleSpec{Name: "protect-prod", Action: api.BehaviorAsk, Description: "Changes to production infrastructure ask you.",
			Match: map[string][]string{"path": {"**/terraform/prod/**"}}},
			Source: api.RuleSourceRepo, File: Home + "/src/infra/.pilothouse/policy.yaml", Enabled: true},

		{RuleSpec: api.RuleSpec{Name: "self-protection", Action: api.BehaviorDeny, Description: "Agents can't change Pilothouse's own settings, rules or keys.",
			Match:   map[string][]string{"kind": {"edit", "shell"}, "path": {"~/.pilothouse/**"}},
			Message: "Pilothouse's own files can only be changed by you."},
			Source: api.RuleSourceBuiltin, Enabled: true},
		{RuleSpec: api.RuleSpec{Name: "protect-agent-settings", Action: api.BehaviorAsk, Description: "Changes to your agents' settings and hooks ask you.",
			Match: map[string][]string{"path": {"~/.claude/settings*.json", "~/.codex/config.toml", "~/.config/opencode/**"}}},
			Source: api.RuleSourceBuiltin, Enabled: true},
	}
}

// Rules lists the loaded rules in evaluation order (managed, presets,
// policy.yaml, repo, built in) and the presets.
func (c *Core) Rules(ctx context.Context) (api.RulesView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return api.RulesView{Rules: c.loadedLocked(), Presets: clonePresets(c.presets)}, nil
}

func (c *Core) loadedLocked() []api.RuleInfo {
	var out []api.RuleInfo
	for _, r := range c.rules {
		if r.Source == api.RuleSourceManaged {
			out = append(out, cloneRule(r))
		}
	}
	for _, p := range c.presets {
		if !p.Enabled {
			continue
		}
		for _, spec := range p.Rules {
			out = append(out, api.RuleInfo{RuleSpec: cloneSpec(spec), Source: api.RuleSourcePreset, Preset: p.Name, Enabled: true})
		}
	}
	for _, r := range c.rules {
		if r.Source != api.RuleSourceManaged {
			out = append(out, cloneRule(r))
		}
	}
	return out
}

func cloneSpec(s api.RuleSpec) api.RuleSpec {
	m := make(map[string][]string, len(s.Match))
	for k, v := range s.Match {
		m[k] = append([]string(nil), v...)
	}
	s.Match = m
	return s
}

func cloneRule(r api.RuleInfo) api.RuleInfo {
	r.RuleSpec = cloneSpec(r.RuleSpec)
	return r
}

func clonePresets(ps []api.Preset) []api.Preset {
	out := make([]api.Preset, len(ps))
	for i, p := range ps {
		out[i] = p
		out[i].Rules = make([]api.RuleSpec, len(p.Rules))
		for j, r := range p.Rules {
			out[i].Rules[j] = cloneSpec(r)
		}
	}
	return out
}

var (
	ruleNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	matchKeys  = map[string]bool{"agent": true, "tool": true, "kind": true, "command": true, "path": true, "cwd": true, "repo": true, "branch": true}
)

// validSpec checks a rule from a device the way the daemon must.
func validSpec(s api.RuleSpec) error {
	switch {
	case !ruleNameRE.MatchString(s.Name):
		return core.Invalid("rule names are 1-64 lowercase letters, digits and dashes")
	case s.Action != api.BehaviorAllow && s.Action != api.BehaviorAsk && s.Action != api.BehaviorDeny:
		return core.Invalid("a rule's action is allow, ask or deny")
	case s.When != "":
		return core.Invalid("rules with `when:` expressions can only be added in policy.yaml")
	case len(s.Match) == 0:
		return core.Invalid("a rule needs at least one match")
	case len(s.Description) > 200 || len(s.Message) > 500:
		return core.Invalid("description or message is too long")
	}
	for k, vs := range s.Match {
		if !matchKeys[k] {
			return core.Invalid("unknown match key %q", k)
		}
		if len(vs) == 0 || len(vs) > 20 {
			return core.Invalid("match %s needs 1-20 patterns", k)
		}
		for _, v := range vs {
			if strings.TrimSpace(v) == "" || len(v) > 300 {
				return core.Invalid("empty or too long pattern in %s", k)
			}
		}
	}
	return nil
}

// AddRule adds a managed rule (to ~/.pilothouse/rules.yaml in the daemon).
func (c *Core) AddRule(ctx context.Context, spec api.RuleSpec, by string) (api.RuleInfo, error) {
	if err := validSpec(spec); err != nil {
		return api.RuleInfo{}, err
	}
	c.mu.Lock()
	for _, r := range c.loadedLocked() {
		if r.Name == spec.Name {
			c.mu.Unlock()
			return api.RuleInfo{}, core.Invalid("a rule named %s already exists", spec.Name)
		}
	}
	info := api.RuleInfo{RuleSpec: cloneSpec(spec), Source: api.RuleSourceManaged, File: managedPolicy,
		Enabled: true, Managed: true, Created: c.now().UTC(), By: by}
	c.rules = append([]api.RuleInfo{info}, c.rules...)
	c.mu.Unlock()
	c.changed("add", spec.Name, by)
	return cloneRule(info), nil
}

func (c *Core) managedLocked(name string) (int, error) {
	for i, r := range c.rules {
		if r.Name != name {
			continue
		}
		if !r.Managed {
			where := "its policy file"
			switch r.Source {
			case api.RuleSourceBuiltin:
				where = "Pilothouse itself (built-in rules can't be changed)"
			case api.RuleSourceUser, api.RuleSourceRepo:
				where = r.File
			}
			return -1, core.Invalid("%s is defined in %s; change it there", name, where)
		}
		return i, nil
	}
	for _, p := range c.presets {
		for _, r := range p.Rules {
			if r.Name == name {
				return -1, core.Invalid("%s belongs to the %s preset; switch the preset off instead", name, p.Title)
			}
		}
	}
	return -1, core.ErrNotFound
}

// SetRuleEnabled turns a managed rule on or off.
func (c *Core) SetRuleEnabled(ctx context.Context, name string, enabled bool, by string) error {
	c.mu.Lock()
	i, err := c.managedLocked(name)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	same := c.rules[i].Enabled == enabled
	c.rules[i].Enabled = enabled
	c.mu.Unlock()
	if !same {
		op := "disable"
		if enabled {
			op = "enable"
		}
		c.changed(op, name, by)
	}
	return nil
}

// RemoveRule deletes a managed rule.
func (c *Core) RemoveRule(ctx context.Context, name string, by string) error {
	c.mu.Lock()
	i, err := c.managedLocked(name)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	c.rules = append(c.rules[:i], c.rules[i+1:]...)
	c.mu.Unlock()
	c.changed("remove", name, by)
	return nil
}

// SetPreset switches a preset on or off.
func (c *Core) SetPreset(ctx context.Context, name string, enabled bool, by string) error {
	c.mu.Lock()
	idx := -1
	for i, p := range c.presets {
		if p.Name == name {
			idx = i
		}
	}
	if idx < 0 {
		c.mu.Unlock()
		return core.ErrNotFound
	}
	same := c.presets[idx].Enabled == enabled
	c.presets[idx].Enabled = enabled
	c.mu.Unlock()
	if !same {
		op := "preset_off"
		if enabled {
			op = "preset_on"
		}
		c.changed(op, name, by)
	}
	return nil
}

func (c *Core) changed(op, name, by string) {
	_, _ = c.Emit(context.Background(), api.EventPolicyChanged, "", "", api.RuleChange{Op: op, Name: name, By: by, At: c.now().UTC()})
}

// TestRule evaluates a hypothetical tool call against the enabled rules.
func (c *Core) TestRule(ctx context.Context, in api.RuleTestInput) (api.RuleTestResult, error) {
	in.Command = strings.TrimSpace(in.Command)
	in.Path = strings.TrimSpace(in.Path)
	if in.Command == "" && in.Path == "" {
		return api.RuleTestResult{}, core.Invalid("enter a command or a file path")
	}
	if len(in.Command) > 4000 || len(in.Path) > 4000 {
		return api.RuleTestResult{}, core.Invalid("that's too long to test")
	}
	if in.Agent == "" {
		in.Agent = api.AgentClaude
	}
	kind := api.ToolShell
	if in.Tool == "" {
		in.Tool = "Bash"
		if in.Command == "" {
			in.Tool = "Edit"
		}
	}
	switch strings.ToLower(in.Tool) {
	case "edit", "write", "multiedit", "apply_patch", "patch":
		kind = api.ToolEdit
	case "read", "glob", "grep", "ls", "list":
		kind = api.ToolRead
	case "webfetch", "websearch", "fetch":
		kind = api.ToolWeb
	}
	if in.Cwd == "" {
		in.Cwd = Home + "/src/pilothouse"
	}
	call := toolCall{agent: string(in.Agent), tool: in.Tool, kind: string(kind), command: in.Command, path: absPath(in.Path, in.Cwd), cwd: in.Cwd}
	c.mu.Lock()
	rules := c.loadedLocked()
	c.mu.Unlock()
	return evaluate(rules, call), nil
}

type toolCall struct{ agent, tool, kind, command, path, cwd string }

func strength(b api.Behavior) int {
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

func evaluate(rules []api.RuleInfo, call toolCall) api.RuleTestResult {
	type hit struct {
		r     api.RuleInfo
		order int
	}
	var hits []hit
	for i, r := range rules {
		if r.Enabled && matches(r.RuleSpec, call) {
			hits = append(hits, hit{r, i})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return strength(hits[i].r.Action) > strength(hits[j].r.Action) })
	res := api.RuleTestResult{Matches: []string{}}
	for _, h := range hits {
		res.Matches = append(res.Matches, h.r.Name)
	}
	if len(hits) > 0 {
		res.Behavior, res.Rule, res.Message = hits[0].r.Action, hits[0].r.Name, hits[0].r.Message
	}
	return res
}

func matches(s api.RuleSpec, call toolCall) bool {
	for k, pats := range s.Match {
		var val string
		pathLike := false
		switch k {
		case "agent":
			val = call.agent
		case "tool":
			val = call.tool
		case "kind":
			val = call.kind
		case "command":
			val = call.command
		case "path":
			val, pathLike = call.path, true
		case "cwd", "repo":
			val, pathLike = call.cwd, true
		default: // branch: unknown for a hypothetical call
			return false
		}
		if val == "" {
			return false
		}
		ok := false
		for _, p := range pats {
			if globMatch(p, val, pathLike) || (k == "repo" && strings.HasPrefix(val+"/", expandHome(p)+"/")) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		return Home + p[1:]
	}
	return p
}

func absPath(p, cwd string) string {
	if p == "" {
		return ""
	}
	p = expandHome(p)
	if !strings.HasPrefix(p, "/") {
		p = path.Join(cwd, p)
	}
	return path.Clean(p)
}

var (
	globMu    sync.Mutex
	globCache = map[string]*regexp.Regexp{}
)

// globMatch: in paths `*` stays within a segment and `**` crosses them;
// in commands and names `*` matches anything. {a,b} picks one.
func globMatch(pattern, s string, pathLike bool) bool {
	if pathLike {
		pattern = expandHome(pattern)
	}
	key := fmt.Sprint(pathLike, "\x00", pattern)
	globMu.Lock()
	re, ok := globCache[key]
	globMu.Unlock()
	if !ok {
		var b strings.Builder
		b.WriteString("^")
		depth := 0
		for i := 0; i < len(pattern); i++ {
			ch := pattern[i]
			switch {
			case ch == '*' && i+1 < len(pattern) && pattern[i+1] == '*':
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			case ch == '*':
				if pathLike {
					b.WriteString("[^/]*")
				} else {
					b.WriteString(".*")
				}
			case ch == '?':
				b.WriteString(".")
			case ch == '{':
				depth++
				b.WriteString("(?:")
			case ch == '}' && depth > 0:
				depth--
				b.WriteString(")")
			case ch == ',' && depth > 0:
				b.WriteString("|")
			default:
				b.WriteString(regexp.QuoteMeta(string(ch)))
			}
		}
		for ; depth > 0; depth-- {
			b.WriteString(")")
		}
		b.WriteString("$")
		var err error
		re, err = regexp.Compile(b.String())
		if err != nil {
			return false
		}
		globMu.Lock()
		globCache[key] = re
		globMu.Unlock()
	}
	return re.MatchString(s)
}

// SuggestRules proposes one-tap rules for a request: allow the command
// here or everywhere, always ask, never allow; for edits, a path glob.
func (c *Core) SuggestRules(ctx context.Context, requestID string) ([]api.RuleSuggestion, error) {
	r, err := c.FakeCore.Request(ctx, requestID)
	if err != nil {
		return nil, err
	}
	s, _ := c.session(r.SessionID)
	repo := s.RepoRoot
	repoName := s.Title
	if repoName == "" {
		repoName = path.Base(r.Cwd)
	}
	high := false
	for _, l := range r.Risk {
		if l.Level == api.RiskHigh {
			high = true
		}
	}
	var out []api.RuleSuggestion
	addS := func(title, scope, reason string, spec api.RuleSpec) {
		h := sha256.Sum256([]byte(requestID + "\x00" + spec.Name))
		out = append(out, api.RuleSuggestion{ID: "sg_" + hex.EncodeToString(h[:6]), Title: title, Scope: scope, Reason: reason, Rule: spec})
	}
	switch {
	case r.Checkpoint != api.CheckpointPermission:
		return []api.RuleSuggestion{}, nil
	case r.ToolKind == api.ToolShell && r.Command != "":
		prefix := commandPrefix(r.Command)
		slug := slugify(prefix)
		count := c.approvals(prefix)
		reason := ""
		if count >= 3 {
			reason = fmt.Sprintf("You allowed %s %d× this week", prefix, count)
		}
		if !high {
			if repo != "" {
				addS(fmt.Sprintf("Always allow `%s` in %s", prefix, repoName), "repo", reason, api.RuleSpec{
					Name: "allow-" + slug + "-" + slugify(repoName), Action: api.BehaviorAllow,
					Description: fmt.Sprintf("Always allow `%s` in %s", prefix, repoName),
					Match:       map[string][]string{"command": {prefix + "*"}, "repo": {repo}}})
			}
			addS(fmt.Sprintf("Always allow `%s` everywhere", prefix), "everywhere", "", api.RuleSpec{
				Name: "allow-" + slug, Action: api.BehaviorAllow,
				Description: fmt.Sprintf("Always allow `%s` everywhere", prefix),
				Match:       map[string][]string{"command": {prefix + "*"}}})
		}
		addS(fmt.Sprintf("Always ask before `%s`", prefix), "everywhere", "Even when another rule or a session grant allows it", api.RuleSpec{
			Name: "ask-" + slug, Action: api.BehaviorAsk,
			Description: fmt.Sprintf("Always ask before `%s`", prefix),
			Match:       map[string][]string{"command": {prefix + "*"}}})
		never := r.Command
		if len(never) > 80 {
			never = prefix
		}
		reason = ""
		if high {
			reason = r.Risk[0].Text
		}
		addS(fmt.Sprintf("Never allow `%s`", never), "everywhere", reason, api.RuleSpec{
			Name: "deny-" + slugify(never), Action: api.BehaviorDeny,
			Description: fmt.Sprintf("Never allow `%s`", never),
			Match:       map[string][]string{"command": {never + "*"}},
			Message:     "Blocked by a rule you made in Pilothouse."})
	case r.ToolKind == api.ToolEdit && len(r.Paths) > 0:
		rel := strings.TrimPrefix(r.Paths[0], strings.TrimSuffix(r.Cwd, "/")+"/")
		dir := path.Dir(rel)
		glob := dir + "/**"
		if dir == "." {
			glob = "**"
		}
		addS(fmt.Sprintf("Allow edits under `%s` in %s", glob, repoName), "repo", "", api.RuleSpec{
			Name: "allow-edits-" + slugify(dir) + "-" + slugify(repoName), Action: api.BehaviorAllow,
			Description: fmt.Sprintf("Allow edits under `%s` in %s", glob, repoName),
			Match:       map[string][]string{"kind": {"edit"}, "path": {"**/" + strings.TrimPrefix(glob, "**")}, "repo": {repo}}})
		addS(fmt.Sprintf("Always ask before editing `%s`", glob), "repo", "", api.RuleSpec{
			Name: "ask-edits-" + slugify(dir) + "-" + slugify(repoName), Action: api.BehaviorAsk,
			Description: fmt.Sprintf("Always ask before editing `%s` in %s", glob, repoName),
			Match:       map[string][]string{"kind": {"edit"}, "path": {"**/" + strings.TrimPrefix(glob, "**")}, "repo": {repo}}})
	default:
		addS(fmt.Sprintf("Always allow %s", r.Tool), "everywhere", "", api.RuleSpec{
			Name: "allow-" + slugify(r.Tool), Action: api.BehaviorAllow,
			Description: fmt.Sprintf("Always allow %s", r.Tool),
			Match:       map[string][]string{"tool": {r.Tool}}})
	}
	// Name clashes get a suffix, so a tap never fails on a duplicate.
	c.mu.Lock()
	names := map[string]bool{}
	for _, ri := range c.loadedLocked() {
		names[ri.Name] = true
	}
	c.mu.Unlock()
	for i := range out {
		base := out[i].Rule.Name
		if len(base) > 60 {
			base = strings.TrimRight(base[:60], "-")
		}
		name := base
		for n := 2; names[name]; n++ {
			name = fmt.Sprintf("%s-%d", base, n)
		}
		out[i].Rule.Name = name
		names[name] = true
	}
	return out, nil
}

// approvals is how often a command prefix was allowed this week (canned).
func (c *Core) approvals(prefix string) int {
	switch {
	case strings.HasPrefix(prefix, "pnpm test"), strings.HasPrefix(prefix, "npm test"), strings.HasPrefix(prefix, "go test"):
		return 7
	case strings.HasPrefix(prefix, "npm run"), strings.HasPrefix(prefix, "make"):
		return 5
	case strings.HasPrefix(prefix, "git push"):
		return 4
	}
	return 1
}

// commandPrefix mirrors grant prefixes: the first word, plus a subcommand.
func commandPrefix(cmd string) string {
	w := strings.Fields(cmd)
	if len(w) == 0 {
		return cmd
	}
	if len(w) > 1 && !strings.HasPrefix(w[1], "-") && !strings.ContainsAny(w[1], "/.~=") {
		return w[0] + " " + w[1]
	}
	return w[0]
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	if s == "" {
		s = "rule"
	}
	return s
}
