package policy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/xasdta/pilothouse/internal/api"
)

// The managed rules file, ~/.pilothouse/rules.yaml (paths.Rules()), holds
// rules created from the inbox, `pilothouse policy add` and suggestions,
// plus the presets that are switched on. Pilothouse writes it; people
// write policy.yaml. It is evaluated after policy.yaml: managed rules with
// source "managed", then each enabled preset's rules with source "preset".
//
// Rules from this file and from presets match shell commands per simple
// command (see shellmatch.go), so an allow rule for `npm test` never
// allows `npm test && curl … | sh`.

// ErrInvalidRule wraps every validation failure of a managed rule; the
// message after it is safe to show to a person.
var ErrInvalidRule = errors.New("invalid rule")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidRule, fmt.Sprintf(format, args...))
}

// ManagedFile is the parsed rules.yaml.
type ManagedFile struct {
	Version int             `yaml:"version"`
	Presets []ManagedPreset `yaml:"presets,omitempty"`
	Rules   []ManagedRule   `yaml:"rules"`
}

// ManagedPreset is a preset that is switched on.
type ManagedPreset struct {
	Name string    `yaml:"name"`
	By   string    `yaml:"by,omitempty"`
	At   time.Time `yaml:"at,omitempty"`
}

// ManagedRule is one rule plus who made it and whether it is on.
type ManagedRule struct {
	Name        string    `yaml:"name"`
	Description string    `yaml:"description,omitempty"`
	Match       globMap   `yaml:"match,omitempty"`
	When        string    `yaml:"when,omitempty"`
	Action      string    `yaml:"action"`
	Message     string    `yaml:"message,omitempty"`
	Enabled     *bool     `yaml:"enabled,omitempty"` // missing = on
	Created     time.Time `yaml:"created,omitempty"`
	By          string    `yaml:"by,omitempty"`
}

// globMap is a match block; each value may be one glob or a list.
type globMap map[string]globList

type globList []string

func (g *globList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*g = globList{n.Value}
		return nil
	}
	var l []string
	if err := n.Decode(&l); err != nil {
		return err
	}
	*g = l
	return nil
}

// On reports whether the rule is enabled.
func (r ManagedRule) On() bool { return r.Enabled == nil || *r.Enabled }

// Spec is the rule as an api.RuleSpec.
func (r ManagedRule) Spec() api.RuleSpec {
	s := api.RuleSpec{Name: r.Name, Description: r.Description, Action: api.Behavior(r.Action), When: r.When, Message: r.Message}
	if len(r.Match) > 0 {
		s.Match = make(map[string][]string, len(r.Match))
		for k, v := range r.Match {
			s.Match[k] = append([]string(nil), v...)
		}
	}
	return s
}

// NewManagedRule makes the stored form of spec.
func NewManagedRule(spec api.RuleSpec, by string, now time.Time) ManagedRule {
	r := ManagedRule{Name: spec.Name, Description: spec.Description, When: spec.When, Action: string(spec.Action),
		Message: spec.Message, Created: now.UTC().Truncate(time.Second), By: by}
	if len(spec.Match) > 0 {
		r.Match = make(globMap, len(spec.Match))
		for k, v := range spec.Match {
			r.Match[k] = append(globList(nil), v...)
		}
	}
	return r
}

// Rule finds a managed rule by name.
func (m *ManagedFile) Rule(name string) (int, bool) {
	for i, r := range m.Rules {
		if r.Name == name {
			return i, true
		}
	}
	return -1, false
}

// PresetOn reports whether a preset is switched on.
func (m *ManagedFile) PresetOn(name string) bool {
	for _, p := range m.Presets {
		if p.Name == name {
			return true
		}
	}
	return false
}

const managedHeader = `# Pilothouse managed rules: rules added from the inbox or with
# ` + "`pilothouse policy add`" + `, and the presets that are switched on.
# Pilothouse rewrites this file; put hand-written rules in policy.yaml.
# Reference: https://github.com/xasdta/pilothouse/blob/main/docs/POLICY.md
`

// parseManaged reads rules.yaml strictly. It does not compile the rules.
func parseManaged(file string, data []byte) (*ManagedFile, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, FileErrors{{File: file, Msg: "file is empty (delete it to start over)"}}
	}
	var mf ManagedFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&mf); err != nil && !errors.Is(err, io.EOF) {
		return nil, FileErrors{yamlError(file, err)}
	}
	if mf.Version != 1 {
		return nil, FileErrors{{File: file, Msg: fmt.Sprintf("unsupported version %d: this Pilothouse understands `version: 1`", mf.Version)}}
	}
	return &mf, nil
}

// yamlRule is a rule in the policy file format, for compiling.
type yamlRule struct {
	Name        string              `yaml:"name"`
	Description string              `yaml:"description,omitempty"`
	Match       map[string][]string `yaml:"match,omitempty"`
	When        string              `yaml:"when,omitempty"`
	Action      string              `yaml:"action"`
	Message     string              `yaml:"message,omitempty"`
}

// compileSpecs compiles rules given as specs with the policy file
// compiler, so they get exactly its validation. Errors are reported
// against label without line numbers (the YAML is generated).
func compileSpecs(label string, specs []api.RuleSpec, home string) ([]*rule, error) {
	doc := struct {
		Version int        `yaml:"version"`
		Rules   []yamlRule `yaml:"rules"`
	}{Version: 1, Rules: []yamlRule{}}
	for _, s := range specs {
		doc.Rules = append(doc.Rules, yamlRule{Name: s.Name, Description: s.Description, Match: s.Match,
			When: s.When, Action: string(s.Action), Message: s.Message})
	}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return nil, FileErrors{{File: label, Msg: err.Error()}}
	}
	rules, err := compilePolicy(label, data, home)
	var fe FileErrors
	if errors.As(err, &fe) {
		out := make(FileErrors, len(fe))
		for i, e := range fe {
			out[i] = &FileError{File: label, Msg: e.Msg}
		}
		return nil, out
	}
	for _, r := range rules {
		r.shellAware = true
	}
	return rules, err
}

// managedSet is a compiled rules.yaml.
type managedSet struct {
	doc     *ManagedFile
	rules   []*rule // enabled rules, in file order
	presets []compiledPreset
}

type compiledPreset struct {
	name  string
	rules []*rule
}

// compileManaged parses and compiles rules.yaml: its enabled rules and
// the presets it switches on. A file with any error is used whole or not
// at all, like policy files.
func compileManaged(file string, data []byte, home string) (*managedSet, error) {
	mf, err := parseManaged(file, data)
	if err != nil {
		return nil, err
	}
	if err := checkManaged(file, mf); err != nil {
		return nil, err
	}
	var specs []api.RuleSpec
	for _, r := range mf.Rules {
		if r.On() {
			specs = append(specs, r.Spec())
		}
	}
	rules, err := compileSpecs(file, specs, home)
	if err != nil {
		return nil, err
	}
	set := &managedSet{doc: mf, rules: rules}
	for _, p := range mf.Presets {
		pr, err := presetRules(p.Name, home)
		if err != nil {
			return nil, err
		}
		set.presets = append(set.presets, compiledPreset{name: p.Name, rules: pr})
	}
	return set, nil
}

// checkManaged validates what the compiler doesn't see: names across
// disabled rules too, and presets.
func checkManaged(file string, mf *ManagedFile) error {
	var errs FileErrors
	seen := map[string]bool{}
	for i, r := range mf.Rules {
		switch {
		case r.Name == "":
			errs = append(errs, &FileError{File: file, Msg: fmt.Sprintf("rule %d: missing `name:`", i+1)})
		case seen[r.Name]:
			errs = append(errs, &FileError{File: file, Msg: fmt.Sprintf("duplicate rule name %q", r.Name)})
		case !r.On():
			// Disabled rules are not compiled, so check their shape here.
			if _, err := compileSpecs(file, []api.RuleSpec{r.Spec()}, ""); err != nil {
				var fe FileErrors
				if errors.As(err, &fe) {
					errs = append(errs, fe...)
				}
			}
		}
		seen[r.Name] = true
	}
	on := map[string]bool{}
	for _, p := range mf.Presets {
		if _, ok := presetByName(p.Name); !ok {
			errs = append(errs, &FileError{File: file, Msg: fmt.Sprintf("unknown preset %q (known: %s)", p.Name, strings.Join(PresetNames(), ", "))})
		}
		if on[p.Name] {
			errs = append(errs, &FileError{File: file, Msg: fmt.Sprintf("preset %q is listed twice", p.Name)})
		}
		on[p.Name] = true
	}
	if len(errs) > 0 {
		return errs
	}
	return nil
}

// ReadManaged reads rules.yaml; a missing file is an empty one.
func ReadManaged(file string) (*ManagedFile, error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return &ManagedFile{Version: 1}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > maxPolicyBytes {
		return nil, FileErrors{{File: file, Msg: fmt.Sprintf("file is larger than %d bytes", maxPolicyBytes)}}
	}
	return parseManaged(file, data)
}

// MarshalManaged renders rules.yaml.
func MarshalManaged(mf *ManagedFile) ([]byte, error) {
	if mf.Rules == nil {
		mf.Rules = []ManagedRule{}
	}
	body, err := yaml.Marshal(mf)
	if err != nil {
		return nil, err
	}
	return append([]byte(managedHeader), body...), nil
}

// managedLockTimeout bounds the wait for another writer of rules.yaml.
const managedLockTimeout = 5 * time.Second

// EditManaged changes rules.yaml under an exclusive lock: it reads the
// file, lets fn change it, validates the result with the same compiler
// the engine uses (with home for path patterns), and replaces the file
// atomically. Nothing is written when fn or the validation fails; errors
// from validation wrap ErrInvalidRule.
func EditManaged(file, home string, fn func(*ManagedFile) error) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	unlock, err := lockFile(file+".lock", managedLockTimeout)
	if err != nil {
		return err
	}
	defer unlock()
	mf, err := ReadManaged(file)
	if err != nil {
		return fmt.Errorf("%w\n(fix or delete %s and try again)", err, file)
	}
	if err := fn(mf); err != nil {
		return err
	}
	mf.Version = 1
	data, err := MarshalManaged(mf)
	if err != nil {
		return err
	}
	if len(data) > maxPolicyBytes {
		return invalidf("too many managed rules (the file would exceed %d bytes)", maxPolicyBytes)
	}
	if len(mf.Rules) > maxRulesPerFile {
		return invalidf("too many managed rules (limit %d)", maxRulesPerFile)
	}
	if _, err := compileManaged(file, data, home); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidRule, errorText(err))
	}
	return writeFileAtomic(file, data, 0o600)
}

// errorText joins a FileErrors' messages without the file name prefix.
func errorText(err error) string {
	var fe FileErrors
	if !errors.As(err, &fe) {
		return err.Error()
	}
	msgs := make([]string, len(fe))
	for i, e := range fe {
		msgs[i] = e.Msg
	}
	return strings.Join(msgs, "; ")
}

// ---- validating rules from people -----------------------------------------

// Limits on a rule added from a device or the CLI.
const (
	maxSpecGlobs    = 20
	maxSpecGlobLen  = 512
	maxSpecTextLen  = 500
	maxManagedName  = 80
	builtinPrefixCI = "pilothouse-"
)

var managedNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidateOptions says who is adding a rule.
type ValidateOptions struct {
	// AllowWhen accepts a `when:` expression (local callers only: a CEL
	// expression from a device is not reviewable on a phone).
	AllowWhen bool
	// Home is the user's home, for checking path globs.
	Home string
}

// ValidateRuleSpec checks a rule someone wants to add before it reaches
// the file: a safe name, a known action, known match keys with sane globs,
// plain-text description and message, no `when:` unless allowed, and no
// allow rule so broad it would approve nearly everything.
func ValidateRuleSpec(s api.RuleSpec, o ValidateOptions) error {
	switch {
	case s.Name == "":
		return invalidf("a rule needs a name")
	case len(s.Name) > maxManagedName:
		return invalidf("rule names are at most %d characters", maxManagedName)
	case !managedNameRE.MatchString(s.Name) || strings.Contains(s.Name, ".."):
		return invalidf("rule name %q may only use letters, digits, '.', '_' and '-' (and must start with a letter or digit)", s.Name)
	case strings.HasPrefix(strings.ToLower(s.Name), builtinPrefixCI):
		return invalidf("rule names starting with %q are reserved for built-in rules", BuiltinPrefix)
	}
	switch s.Action {
	case api.BehaviorAllow, api.BehaviorAsk, api.BehaviorDeny:
	default:
		return invalidf("action must be allow, ask or deny")
	}
	if s.When != "" && !o.AllowWhen {
		return invalidf("rules with `when:` expressions can only be added in policy.yaml")
	}
	if len(s.Match) == 0 && s.When == "" {
		return invalidf("a rule needs at least one match: key (%s)", strings.Join(matchKeys, ", "))
	}
	for k, globs := range s.Match {
		if !contains(matchKeys, k) {
			return invalidf("unknown match key %q (valid keys: %s)", k, strings.Join(matchKeys, ", "))
		}
		if len(globs) == 0 || len(globs) > maxSpecGlobs {
			return invalidf("match.%s needs 1 to %d patterns", k, maxSpecGlobs)
		}
		for _, g := range globs {
			if g == "" || len(g) > maxSpecGlobLen || !utf8.ValidString(g) || hasControl(g, false) {
				return invalidf("match.%s: patterns must be 1-%d characters of plain text", k, maxSpecGlobLen)
			}
		}
	}
	for what, v := range map[string]string{"description": s.Description, "message": s.Message} {
		if len(v) > maxSpecTextLen || !utf8.ValidString(v) || hasControl(v, true) {
			return invalidf("%s must be plain text of at most %d characters", what, maxSpecTextLen)
		}
	}
	if len(s.When) > 4096 || hasControl(s.When, true) {
		return invalidf("when: is too long or has control characters")
	}
	if s.Action == api.BehaviorAllow {
		if err := checkAllowScope(s, o.Home); err != nil {
			return err
		}
	}
	// Compile it alone for glob and CEL errors, with readable messages.
	if _, err := compileSpecs("rule", []api.RuleSpec{s}, o.Home); err != nil {
		return invalidf("%s", errorText(err))
	}
	return nil
}

// hasControl reports control characters (newlines allowed in text).
func hasControl(s string, text bool) bool {
	for _, r := range s {
		if r == '\t' || (text && r == '\n') {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// checkAllowScope rejects allow rules that would approve almost
// everything: an allow rule must pin down a command, a tool or a path
// with a literal start, and path globs must be anchored below a
// directory narrower than the home directory.
func checkAllowScope(s api.RuleSpec, home string) error {
	pinned := false
	for _, g := range s.Match["command"] {
		if wildStart(g) {
			return invalidf("an allow rule's command pattern must start with the command (%q starts with a wildcard)", g)
		}
		pinned = true
	}
	for _, g := range s.Match["tool"] {
		if wildStart(g) {
			return invalidf("an allow rule's tool pattern must name the tool (%q starts with a wildcard)", g)
		}
		pinned = true
	}
	for _, g := range s.Match["path"] {
		if err := checkAllowPath(g, home); err != nil {
			return err
		}
		pinned = true
	}
	if !pinned {
		return invalidf("an allow rule must name a command, a tool or a path; this one would allow too much")
	}
	return nil
}

func wildStart(g string) bool {
	return g == "" || strings.ContainsRune("*?{\\", rune(g[0]))
}

func checkAllowPath(g, home string) error {
	p := expandHome(g, home)
	if !strings.HasPrefix(p, "/") {
		return invalidf("an allow rule's path pattern must be absolute or start with ~/ (%q matches at any depth)", g)
	}
	static := p
	if i := strings.IndexAny(p, "*?{\\"); i >= 0 {
		static = p[:i]
	}
	dir := filepath.Clean(static)
	if !strings.HasSuffix(static, "/") && static != p {
		dir = filepath.Dir(dir) // "/a/b*" is below /a
	}
	home = filepath.Clean(home)
	if dir == "/" || strings.Count(dir, "/") < 2 || (home != "" && (dir == home || strings.HasPrefix(home+"/", dir+"/"))) {
		return invalidf("an allow rule's path pattern %q covers too much; narrow it to a project directory", g)
	}
	return nil
}
