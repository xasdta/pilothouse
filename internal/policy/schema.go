package policy

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"cel.dev/cel-go/cel"
	"gopkg.in/yaml.v3"

	"github.com/xasdta/pilothouse/internal/api"
)

// Limits that keep a policy file from becoming a performance problem at
// the pre_tool checkpoint. Real files are a few KB with tens of rules.
const (
	maxPolicyBytes  = 1 << 20
	maxRulesPerFile = 1000
	maxNameLen      = 100
)

// BuiltinPrefix is reserved for rule names shipped with Pilothouse.
const BuiltinPrefix = "pilothouse-"

// FileError is one problem in a policy file. Line is 1-based; 0 means
// the problem is not tied to a line (unreadable file, empty document).
type FileError struct {
	File string `json:"file"`
	Line int    `json:"line,omitempty"`
	Msg  string `json:"message"`
}

func (e *FileError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%s:%d: %s", e.File, e.Line, e.Msg)
	}
	return fmt.Sprintf("%s: %s", e.File, e.Msg)
}

// FileErrors lists every problem found in one file, in file order, so a
// person can fix them all in one pass instead of one per `policy check`.
type FileErrors []*FileError

func (es FileErrors) Error() string {
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "\n")
}

// matchField is one key of a rule's `match:` block.
type matchField uint8

const (
	fAgent matchField = iota
	fTool
	fKind
	fCommand
	fPath
	fCwd
	fRepo
	fBranch
)

var matchFieldNames = map[string]matchField{
	"agent": fAgent, "tool": fTool, "kind": fKind, "command": fCommand,
	"path": fPath, "cwd": fCwd, "repo": fRepo, "branch": fBranch,
}

// fieldMatch is one `match:` key: the field matches if any glob does.
type fieldMatch struct {
	field matchField
	globs []*glob
}

// rule is a compiled policy rule.
type rule struct {
	name        string
	description string
	message     string
	action      api.Behavior
	line        int
	agents      []api.AgentKind // empty = every agent
	match       []fieldMatch    // AND-ed
	when        cel.Program     // nil = no `when:`
	whenSrc     string
	matchSrc    map[string][]string // the match: block as written, for listings
	// shellAware rules (managed and preset) match `command:` per simple
	// command (shellmatch.go).
	shellAware bool
	// check replaces match/when for built-in rules implemented in Go.
	check func(*evalCtx) (bool, string)
}

var (
	knownKinds  = []string{string(api.ToolShell), string(api.ToolEdit), string(api.ToolRead), string(api.ToolWeb), string(api.ToolMCP), string(api.ToolOther)}
	knownAgents = []string{string(api.AgentClaude), string(api.AgentCodex), string(api.AgentOpenCode), string(api.AgentGeneric)}
	topKeys     = []string{"version", "rules"}
	ruleKeys    = []string{"name", "description", "match", "when", "action", "message", "agents"}
	matchKeys   = []string{"agent", "tool", "kind", "command", "path", "cwd", "repo", "branch"}
	actions     = []string{string(api.BehaviorAllow), string(api.BehaviorAsk), string(api.BehaviorDeny)}
)

// parser accumulates every error in a file instead of stopping at the first.
type parser struct {
	file string
	home string
	errs FileErrors
}

func (p *parser) errf(n *yaml.Node, format string, args ...any) {
	line := 0
	if n != nil {
		line = n.Line
	}
	p.errs = append(p.errs, &FileError{File: p.file, Line: line, Msg: fmt.Sprintf(format, args...)})
}

// compilePolicy parses and compiles one policy file. On any error it
// returns nil rules and a FileErrors: a half-compiled file could drop a
// deny rule, so a file is used whole or not at all.
func compilePolicy(file string, data []byte, home string) ([]*rule, error) {
	p := &parser{file: file, home: home}
	if len(data) > maxPolicyBytes {
		p.errf(nil, "file is larger than %d bytes", maxPolicyBytes)
		return nil, p.errs
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		fe := yamlError(file, err)
		if fe.Line > 0 {
			if lines := strings.Split(string(data), "\n"); fe.Line <= len(lines) {
				l := lines[fe.Line-1]
				if strings.Contains(l[:len(l)-len(strings.TrimLeft(l, " \t"))], "\t") {
					fe.Msg += " (YAML does not allow tabs for indentation; use spaces)"
				}
			}
		}
		p.errs = append(p.errs, fe)
		return nil, p.errs
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return nil, nil // empty file: valid, no rules
	}
	root := deref(doc.Content[0])
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		return nil, nil // only comments
	}
	if root.Kind != yaml.MappingNode {
		p.errf(root, "expected a mapping with `version: 1` and `rules:`")
		return nil, p.errs
	}
	var rulesNode *yaml.Node
	sawVersion := false
	p.eachKey(root, topKeys, "top level", func(k string, kn, v *yaml.Node) {
		switch k {
		case "version":
			sawVersion = true
			if v.Kind != yaml.ScalarNode || v.Value != "1" {
				p.errf(v, "unsupported version %q: this Pilothouse understands `version: 1`", v.Value)
			}
		case "rules":
			rulesNode = v
		}
	})
	if !sawVersion {
		p.errf(root, "missing `version: 1`")
	}
	var rules []*rule
	if rulesNode != nil && !(rulesNode.Kind == yaml.ScalarNode && rulesNode.Tag == "!!null") {
		if rulesNode.Kind != yaml.SequenceNode {
			p.errf(rulesNode, "`rules:` must be a list of rules")
		} else {
			if len(rulesNode.Content) > maxRulesPerFile {
				p.errf(rulesNode, "too many rules (%d, limit %d)", len(rulesNode.Content), maxRulesPerFile)
			}
			names := map[string]int{}
			for i, rn := range rulesNode.Content {
				if r := p.rule(deref(rn), i); r != nil {
					if prev, dup := names[r.name]; dup {
						p.errf(rn, "duplicate rule name %q (first defined on line %d)", r.name, prev)
					}
					names[r.name] = r.line
					rules = append(rules, r)
				}
			}
		}
	}
	if len(p.errs) > 0 {
		return nil, p.errs
	}
	return rules, nil
}

// eachKey walks a mapping, rejecting unknown and duplicate keys.
func (p *parser) eachKey(m *yaml.Node, known []string, where string, fn func(k string, kn, v *yaml.Node)) {
	seen := map[string]int{}
	for i := 0; i+1 < len(m.Content); i += 2 {
		kn, v := m.Content[i], deref(m.Content[i+1])
		k := kn.Value
		if prev, dup := seen[k]; dup {
			p.errf(kn, "duplicate key %q in %s (first on line %d)", k, where, prev)
			continue
		}
		seen[k] = kn.Line
		if !contains(known, k) {
			msg := fmt.Sprintf("unknown key %q in %s", k, where)
			if s := suggest(k, known); s != "" {
				msg += fmt.Sprintf(" (did you mean %q?)", s)
			} else {
				msg += " (valid keys: " + strings.Join(known, ", ") + ")"
			}
			p.errf(kn, "%s", msg)
			continue
		}
		fn(k, kn, v)
	}
}

func (p *parser) rule(n *yaml.Node, idx int) *rule {
	if n.Kind != yaml.MappingNode {
		p.errf(n, "rule %d: expected a mapping with name:, match:/when: and action:", idx+1)
		return nil
	}
	r := &rule{line: n.Line}
	before := len(p.errs)
	var matchNode, whenNode *yaml.Node
	p.eachKey(n, ruleKeys, fmt.Sprintf("rule %d", idx+1), func(k string, kn, v *yaml.Node) {
		switch k {
		case "name":
			r.name = p.str(v, "name")
		case "description":
			r.description = p.str(v, "description")
		case "message":
			r.message = p.str(v, "message")
		case "action":
			a := p.str(v, "action")
			if contains(actions, a) {
				r.action = api.Behavior(a)
			} else if a != "" || v.Kind == yaml.ScalarNode {
				p.errf(v, "action must be allow, ask or deny, not %q", a)
			}
		case "agents":
			for _, s := range p.strList(v, "agents") {
				if !contains(knownAgents, s.Value) {
					p.errf(s, "unknown agent %q (known: %s)", s.Value, strings.Join(knownAgents, ", "))
					continue
				}
				r.agents = append(r.agents, api.AgentKind(s.Value))
			}
		case "match":
			matchNode = v
		case "when":
			whenNode = v
		}
	})
	label := fmt.Sprintf("rule %d", idx+1)
	switch {
	case r.name == "":
		p.errf(n, "%s: missing `name:`", label)
	case strings.TrimSpace(r.name) != r.name || strings.ContainsAny(r.name, "\n\t"):
		p.errf(n, "rule name %q must not contain leading/trailing spaces, tabs or newlines", r.name)
	case len(r.name) > maxNameLen:
		p.errf(n, "rule name is longer than %d characters", maxNameLen)
	case strings.HasPrefix(r.name, BuiltinPrefix):
		p.errf(n, "rule name %q: names starting with %q are reserved for built-in rules", r.name, BuiltinPrefix)
	default:
		label = fmt.Sprintf("rule %q", r.name)
	}
	if r.action == "" && !p.hasKey(n, "action") {
		p.errf(n, "%s: missing `action:` (allow, ask or deny)", label)
	}
	if matchNode == nil && whenNode == nil {
		p.errf(n, "%s: needs `match:` and/or `when:` (to match every call use `match: {tool: \"*\"}`)", label)
	}
	if matchNode != nil {
		r.match, r.matchSrc = p.match(matchNode, label)
	}
	if whenNode != nil {
		src := p.str(whenNode, "when")
		if strings.TrimSpace(src) == "" {
			p.errf(whenNode, "%s: `when:` is empty", label)
		} else if prog, err := compileWhen(src); err != nil {
			p.errf(whenNode, "%s: when: %v", label, err)
		} else {
			r.when, r.whenSrc = prog, src
		}
	}
	if len(p.errs) > before {
		return nil
	}
	return r
}

func (p *parser) match(n *yaml.Node, label string) ([]fieldMatch, map[string][]string) {
	if n.Kind != yaml.MappingNode {
		p.errf(n, "%s: `match:` must be a mapping, e.g. {tool: Bash, command: \"git push*\"}", label)
		return nil, nil
	}
	if len(n.Content) == 0 {
		p.errf(n, "%s: `match:` is empty", label)
		return nil, nil
	}
	var out []fieldMatch
	src := map[string][]string{}
	p.eachKey(n, matchKeys, label+" match", func(k string, kn, v *yaml.Node) {
		f := matchFieldNames[k]
		fm := fieldMatch{field: f}
		for _, s := range p.strList(v, "match."+k) {
			pat := s.Value
			src[k] = append(src[k], pat)
			mode := globText
			switch f {
			case fKind:
				if !hasGlobMeta(pat) && !contains(knownKinds, pat) {
					p.errf(s, "%s: unknown kind %q (known: %s)", label, pat, strings.Join(knownKinds, ", "))
					continue
				}
			case fAgent:
				if !hasGlobMeta(pat) && !contains(knownAgents, pat) {
					p.errf(s, "%s: unknown agent %q (known: %s)", label, pat, strings.Join(knownAgents, ", "))
					continue
				}
			case fPath, fCwd, fRepo:
				mode, pat = globPath, pathPattern(pat, p.home)
			case fBranch:
				mode = globPath
			}
			g, err := compileGlob(pat, mode)
			if err != nil {
				p.errf(s, "%s: match.%s: %v", label, k, err)
				continue
			}
			fm.globs = append(fm.globs, g)
		}
		if len(fm.globs) > 0 {
			out = append(out, fm)
		}
	})
	return out, src
}

// str reads a scalar. Non-scalars are reported, returning "".
func (p *parser) str(n *yaml.Node, key string) string {
	if n.Kind != yaml.ScalarNode {
		p.errf(n, "%s must be a string", key)
		return ""
	}
	if n.Tag == "!!null" {
		return ""
	}
	return n.Value
}

// strList accepts a scalar or a list of scalars ("tool: [Write, Edit]").
func (p *parser) strList(n *yaml.Node, key string) []*yaml.Node {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag == "!!null" {
			p.errf(n, "%s is empty", key)
			return nil
		}
		return []*yaml.Node{n}
	case yaml.SequenceNode:
		if len(n.Content) == 0 {
			p.errf(n, "%s is an empty list", key)
			return nil
		}
		var out []*yaml.Node
		for _, c := range n.Content {
			c = deref(c)
			if c.Kind != yaml.ScalarNode {
				p.errf(c, "%s: list items must be strings", key)
				continue
			}
			out = append(out, c)
		}
		return out
	}
	p.errf(n, "%s must be a string or a list of strings", key)
	return nil
}

func (p *parser) hasKey(m *yaml.Node, key string) bool {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return true
		}
	}
	return false
}

func deref(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

var yamlLineRe = regexp.MustCompile(`line (\d+)`)

// yamlError turns a yaml.v3 syntax error into a FileError with its line.
func yamlError(file string, err error) *FileError {
	msg := strings.TrimPrefix(err.Error(), "yaml: ")
	fe := &FileError{File: file, Msg: msg}
	if m := yamlLineRe.FindStringSubmatchIndex(msg); m != nil {
		fe.Line, _ = strconv.Atoi(msg[m[2]:m[3]])
		// Drop the now-redundant "line N: " prefix.
		fe.Msg = strings.TrimPrefix(strings.TrimPrefix(msg[m[1]:], ":"), " ")
		if fe.Msg == "" {
			fe.Msg = msg
		}
	}
	return fe
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// suggest returns the known word closest to s if it is a likely typo.
func suggest(s string, known []string) string {
	best, bestD := "", 3
	for _, k := range known {
		if d := editDistance(strings.ToLower(s), k); d < bestD {
			best, bestD = k, d
		}
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
