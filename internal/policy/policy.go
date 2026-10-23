// Package policy evaluates user, repo and built-in rules at the pre_tool
// checkpoint.
//
// Rules come from three places, evaluated in this order:
//
//  1. Built-in rules (pilothouse-self-protection), always on.
//  2. The user file, ~/.pilothouse/policy.yaml.
//  3. The repo file, <repo>/.pilothouse/policy.yaml, discovered from
//     Input.Repo at evaluation time. Repos not listed in
//     trusted-repos.yaml may only add deny and ask rules: an agent can
//     write to the repo it works in, so an untrusted repo file must not
//     be able to loosen anything.
//
// Every matching rule is reported; the decision is the strictest one
// (deny > ask > allow), ties going to the earliest rule. Files are
// compiled once and re-checked on disk at most every two seconds, so
// Evaluate is a few microseconds of glob and CEL work.
package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/paths"
)

// Input describes one tool call.
type Input struct {
	Agent      api.AgentKind   `json:"agent"`
	Tool       string          `json:"tool"`
	ToolKind   api.ToolKind    `json:"kind"`
	Command    string          `json:"command,omitempty"`
	Paths      []string        `json:"paths,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
	Cwd        string          `json:"cwd"`
	Repo       string          `json:"repo,omitempty"`
	Branch     string          `json:"branch,omitempty"`
	Mode       api.Mode        `json:"mode,omitempty"`
	Checkpoint api.Checkpoint  `json:"checkpoint"`
	Now        time.Time       `json:"-"` // zero = time.Now()
}

// Result is the combined verdict. Action "" means no rule matched.
// Precedence: deny > ask > allow.
type Result struct {
	Action  api.Behavior `json:"action,omitempty"`
	Rule    string       `json:"rule,omitempty"`    // name of the deciding rule
	File    string       `json:"file,omitempty"`    // file it came from ("builtin" for built-ins)
	Message string       `json:"message,omitempty"` // shown to the agent on deny/ask
	Matched []Match      `json:"matched,omitempty"` // every rule that matched
}

// Match is one rule that matched. Ignored is set when the rule matched
// but did not count: an allow rule in an untrusted repo, or a `when:`
// expression that failed at run time.
type Match struct {
	Rule    string       `json:"rule"`
	File    string       `json:"file"`
	Action  api.Behavior `json:"action"`
	Ignored string       `json:"ignored,omitempty"` // e.g. "allow rule in untrusted repo"
}

// Options locates the policy inputs.
type Options struct {
	UserFile         string   // paths.Policy()
	TrustedReposFile string   // paths.TrustedRepos()
	ProtectedPaths   []string // from every adapter + paths.Home()
	// Unprotected are directories inside a protected path that agents
	// may work in: task worktrees made by Pilothouse before v0.2 under
	// ~/.pilothouse/worktrees. Settings files inside them stay protected.
	Unprotected []string
	// PilothouseHome lets the self-protection rule resolve $PILOTHOUSE_HOME in
	// shell commands. Optional.
	PilothouseHome string
}

// DefaultOptions returns the standard locations. extraProtected are the
// adapters' ProtectedPaths(); the Pilothouse home and socket directory are
// always protected.
func DefaultOptions(extraProtected ...string) Options {
	prot := []string{paths.Home(), paths.Run()}
	for _, p := range extraProtected {
		if p != "" {
			prot = append(prot, p)
		}
	}
	// Task worktrees used to live in the Pilothouse home (they now default
	// to ~/.pilothouse-worktrees, see internal/task); agents of tasks made
	// back then must still be able to work without a prompt per call.
	unprot := []string{filepath.Join(paths.Home(), "worktrees")}
	if wt := os.Getenv("PILOTHOUSE_WORKTREES"); wt != "" {
		unprot = append(unprot, wt) // same variable as task.WorktreesRoot
	}
	return Options{
		UserFile:         paths.Policy(),
		TrustedReposFile: paths.TrustedRepos(),
		ProtectedPaths:   prot,
		Unprotected:      unprot,
		PilothouseHome:   paths.Home(),
	}
}

// File labels used in Result.File and api.PolicyFile.Scope.
const (
	BuiltinFile = "builtin"
	ScopeUser   = "user"
	ScopeRepo   = "repo"
	ScopeBuilt  = "builtin"
)

// RepoPolicyFile is the repo policy location relative to the repo root.
const RepoPolicyFile = ".pilothouse/policy.yaml"

const (
	recheckEvery = 2 * time.Second // disk re-stat interval per file
	maxRepos     = 256             // repo files cached at once
)

// Engine is safe for concurrent use.
type Engine struct {
	opts           Options
	home           string
	pilothouseHome string
	protected      *protectedSet // folded absolute roots (symlinks resolved too) and globs
	builtin        []*rule
	user           *slot[[]*rule]
	trust          *slot[trustSet]

	reposMu sync.RWMutex
	repos   map[string]*repoState

	rootsMu     sync.RWMutex
	rootsByBase map[string]*protectedSet // per-repo protected sets

	symMu   sync.Mutex
	symDirs map[string]symEntry // directory → symlink-resolved, short TTL

	// clock is the wall clock for cache ageing; tests replace it.
	clock func() time.Time
}

type repoState struct {
	root     string // cleaned path as the daemon reported it
	canon    string // symlinks resolved, to match trusted-repos.yaml
	file     *slot[[]*rule]
	lastUsed atomic.Int64
}

// Load builds an engine. It never returns nil: on errors the engine keeps
// the built-in rules and reports problems in the status.
func Load(opts Options) (*Engine, api.PolicyStatus) {
	e := &Engine{
		opts:           opts,
		home:           paths.UserHome(),
		pilothouseHome: opts.PilothouseHome,
		repos:          map[string]*repoState{},
		rootsByBase:    map[string]*protectedSet{},
		symDirs:        map[string]symEntry{},
		clock:          time.Now,
	}
	e.protected = e.protectedSet(opts.ProtectedPaths)
	e.protected.except = e.protectedSet(opts.Unprotected).roots
	e.builtin = []*rule{{
		name:        SelfProtectionRule,
		description: "Agents must not administer Pilothouse or edit Pilothouse/agent settings without a person",
		action:      api.BehaviorAsk,
		check:       selfProtection,
	}}
	e.user = newPolicySlot(opts.UserFile, e.compileFile)
	e.trust = newSlot(opts.TrustedReposFile, parseTrustSet)
	return e, e.Reload()
}

// Reload re-reads every known file now, regardless of the re-check
// interval. A file that fails to parse keeps its last good rules.
func (e *Engine) Reload() api.PolicyStatus {
	now := e.clock().UnixNano()
	e.user.get(now, true)
	e.trust.get(now, true)
	e.reposMu.RLock()
	repos := make([]*repoState, 0, len(e.repos))
	for _, r := range e.repos {
		repos = append(repos, r)
	}
	e.reposMu.RUnlock()
	for _, r := range repos {
		r.file.get(now, true)
	}
	return e.Status()
}

// Changed reports whether the user policy file or the trust list changed
// on disk since they were last loaded. Repo files are re-checked by
// Evaluate itself.
func (e *Engine) Changed() bool {
	return e.user.changed() || e.trust.changed()
}

// Status describes the loaded files.
func (e *Engine) Status() api.PolicyStatus {
	st := api.PolicyStatus{Files: []api.PolicyFile{{
		Path: BuiltinFile, Scope: ScopeBuilt, Rules: len(e.builtin), Trusted: true,
	}}}
	st.Rules = len(e.builtin)
	add := func(pf api.PolicyFile, snap *snapshot[[]*rule]) {
		if snap != nil {
			pf.Rules = len(snap.val)
			if snap.err != nil {
				pf.Error = snap.err.Error()
				st.Errors = append(st.Errors, errorLines(snap.err)...)
				if snap.hadGood {
					st.Errors = append(st.Errors, fmt.Sprintf("%s: still using its last valid version (%d rules) until this is fixed", pf.Path, len(snap.val)))
				}
			}
			st.Rules += len(snap.val)
		}
		st.Files = append(st.Files, pf)
	}
	add(api.PolicyFile{Path: e.opts.UserFile, Scope: ScopeUser, Trusted: true}, e.user.cur.Load())
	if ts := e.trust.cur.Load(); ts != nil && ts.err != nil {
		st.Errors = append(st.Errors, errorLines(ts.err)...)
	}

	e.reposMu.RLock()
	repos := make([]*repoState, 0, len(e.repos))
	for _, r := range e.repos {
		repos = append(repos, r)
	}
	e.reposMu.RUnlock()
	sort.Slice(repos, func(i, j int) bool { return repos[i].root < repos[j].root })
	trust := e.trust.cur.Load()
	for _, r := range repos {
		snap := r.file.cur.Load()
		if snap == nil || !snap.exists {
			continue
		}
		add(api.PolicyFile{Path: r.file.path, Scope: ScopeRepo, Trusted: trust != nil && trust.val.has(r)}, snap)
	}
	return st
}

func errorLines(err error) []string {
	var fe FileErrors
	if errors.As(err, &fe) {
		out := make([]string, len(fe))
		for i, e := range fe {
			out[i] = e.Error()
		}
		return out
	}
	return []string{err.Error()}
}

// Evaluate applies every rule to one tool call. It never blocks on a
// person and touches the disk at most once per file every two seconds.
func (e *Engine) Evaluate(in Input) Result {
	wall := e.clock()
	c := evalCtx{in: &in, eng: e, home: e.home, now: in.Now, wall: wall.UnixNano()}
	if c.now.IsZero() {
		c.now = wall
	}
	if in.Cwd != "" {
		c.cwd = filepath.Clean(expandHome(in.Cwd, e.home))
	}
	if in.Repo != "" {
		c.repo = filepath.Clean(expandHome(in.Repo, e.home))
	}
	nanos := wall.UnixNano()

	var d decider
	d.apply(&c, e.builtin, BuiltinFile, true)
	if u := e.user.get(nanos, false); len(u.val) > 0 {
		d.apply(&c, u.val, e.opts.UserFile, true)
	}
	if c.repo != "" {
		rs := e.repoState(c.repo, nanos)
		if snap := rs.file.get(nanos, false); len(snap.val) > 0 {
			trusted := e.trust.get(nanos, false).val.has(rs)
			d.apply(&c, snap.val, rs.file.path, trusted)
		}
	}
	return d.result()
}

// decider accumulates matches and keeps the strictest one.
type decider struct {
	res      Result
	bestRank int
}

func rank(b api.Behavior) int {
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

func (d *decider) apply(c *evalCtx, rules []*rule, file string, trusted bool) {
	for _, r := range rules {
		ok, msg, err := r.eval(c)
		if err != nil {
			d.res.Matched = append(d.res.Matched, Match{Rule: r.name, File: file, Action: r.action,
				Ignored: "when: failed at run time: " + err.Error()})
			continue
		}
		if !ok {
			continue
		}
		m := Match{Rule: r.name, File: file, Action: r.action}
		if !trusted && r.action == api.BehaviorAllow {
			m.Ignored = "allow rule in untrusted repo (run `pilothouse policy trust` to enable)"
			d.res.Matched = append(d.res.Matched, m)
			continue
		}
		if r.action == api.BehaviorAllow && c.in.ToolKind == api.ToolShell && len(c.in.Paths) > 0 && r.usesPaths() {
			// A shell command's paths are only the files an apply_patch
			// heredoc in it names; the command may do anything else too.
			// They are good enough to deny or ask on, never to allow.
			m.Ignored = shellPathAllowIgnored
			d.res.Matched = append(d.res.Matched, m)
			continue
		}
		d.res.Matched = append(d.res.Matched, m)
		if rk := rank(r.action); rk > d.bestRank {
			d.bestRank = rk
			d.res.Action, d.res.Rule, d.res.File = r.action, r.name, file
			d.res.Message = r.messageFor(msg)
		}
	}
}

func (d *decider) result() Result { return d.res }

const shellPathAllowIgnored = "allow rule decided on the files a shell command patches; the command may do more, so only deny and ask rules use them"

// usesPaths reports whether the rule looks at the call's files: a `path:`
// match, or a `when:` that mentions path or paths.
func (r *rule) usesPaths() bool {
	for _, m := range r.match {
		if m.field == fPath {
			return true
		}
	}
	return strings.Contains(r.whenSrc, "path")
}

func (r *rule) messageFor(dynamic string) string {
	switch {
	case dynamic != "":
		return dynamic
	case r.message != "":
		return r.message
	case r.action == api.BehaviorDeny:
		return fmt.Sprintf("Blocked by Pilothouse policy rule %q.", r.name)
	case r.action == api.BehaviorAsk:
		return fmt.Sprintf("Pilothouse policy rule %q asks a person to confirm this.", r.name)
	}
	return ""
}

// eval reports whether the rule matches. err is a run-time `when:`
// failure (e.g. a missing map key), which counts as "no match".
func (r *rule) eval(c *evalCtx) (ok bool, msg string, err error) {
	if r.check != nil {
		ok, msg = r.check(c)
		return ok, msg, nil
	}
	if len(r.agents) > 0 && !slices.Contains(r.agents, c.in.Agent) {
		return false, "", nil
	}
	for i := range r.match {
		if !c.matchField(&r.match[i]) {
			return false, "", nil
		}
	}
	if r.when == nil {
		return true, "", nil
	}
	defer func() {
		// cel-go should not panic, but a policy file must never be able
		// to take the daemon down.
		if p := recover(); p != nil {
			ok, err = false, fmt.Errorf("panic: %v", p)
		}
	}()
	out, _, err := r.when.Eval(activation{c})
	if err != nil {
		return false, "", err
	}
	b, isBool := out.(types.Bool)
	if !isBool {
		return false, "", fmt.Errorf("returned %s, not a bool", out.Type().TypeName())
	}
	return bool(b), "", nil
}

// evalCtx is the per-call view of an Input. Derived values (absolute
// paths, decoded input, protected roots) are computed on first use.
type evalCtx struct {
	in        *Input
	eng       *Engine
	home      string
	cwd, repo string
	now       time.Time
	wall      int64 // engine clock, for cache ageing

	paths     []string
	pathsDone bool
	pathsVal  ref.Val
	input     map[string]any
	inputDone bool
	inputVal  ref.Val
	roots     *protectedSet
	rootsDone bool
}

// celPaths and celInput convert once per call: several rules may read
// the same variable.
func (c *evalCtx) celPaths() ref.Val {
	if c.pathsVal == nil {
		c.pathsVal = types.NewStringList(types.DefaultTypeAdapter, c.absPaths())
	}
	return c.pathsVal
}

func (c *evalCtx) celInput() ref.Val {
	if c.inputVal == nil {
		c.inputVal = types.DefaultTypeAdapter.NativeToValue(c.inputMap())
	}
	return c.inputVal
}

func (c *evalCtx) absPaths() []string {
	if !c.pathsDone {
		c.pathsDone = true
		for _, p := range c.in.Paths {
			if p != "" {
				c.paths = append(c.paths, absPath(p, c.cwd, c.home))
			}
		}
	}
	return c.paths
}

func (c *evalCtx) inputMap() map[string]any {
	if !c.inputDone {
		c.inputDone = true
		c.input = decodeInput(c.in.Input)
	}
	return c.input
}

func (c *evalCtx) matchField(fm *fieldMatch) bool {
	if fm.field == fPath {
		for _, p := range c.absPaths() {
			for _, g := range fm.globs {
				if g.match(p) {
					return true
				}
			}
		}
		return false
	}
	var v string
	switch fm.field {
	case fAgent:
		v = string(c.in.Agent)
	case fTool:
		v = c.in.Tool
	case fKind:
		v = string(c.in.ToolKind)
	case fCommand:
		v = c.in.Command
	case fCwd:
		v = c.cwd
	case fRepo:
		v = c.repo
	case fBranch:
		v = c.in.Branch
	}
	for _, g := range fm.globs {
		if g.match(v) {
			return true
		}
	}
	return false
}

// protectedRoots adds the per-repo settings files to the engine-wide
// protected paths: the repo's own Pilothouse policy (an agent could delete
// a deny rule from it) and project-level agent settings (which can grant
// permissions).
func (c *evalCtx) protectedRoots() *protectedSet {
	if c.rootsDone {
		return c.roots
	}
	c.rootsDone = true
	base := c.repo
	if base == "" {
		base = c.cwd
	}
	c.roots = c.eng.rootsFor(base)
	return c.roots
}

// rootsFor returns the engine-wide protected set plus base's own
// settings files, cached per base directory.
func (e *Engine) rootsFor(base string) *protectedSet {
	if base == "" {
		return e.protected
	}
	e.rootsMu.RLock()
	r, ok := e.rootsByBase[base]
	e.rootsMu.RUnlock()
	if ok {
		return r
	}
	r = e.protected.withRoots(
		foldPath(filepath.Join(base, ".pilothouse")),
		foldPath(filepath.Join(base, ".claude", "settings.json")),
		foldPath(filepath.Join(base, ".claude", "settings.local.json")),
		foldPath(filepath.Join(base, ".codex", "config.toml")),
	)
	e.rootsMu.Lock()
	if len(e.rootsByBase) >= maxCachedDirs {
		clear(e.rootsByBase)
	}
	e.rootsByBase[base] = r
	e.rootsMu.Unlock()
	return r
}

func (c *evalCtx) isProtected(p string) bool {
	roots := c.protectedRoots()
	if roots.has(foldPath(p)) {
		return true
	}
	if r, changed := c.eng.resolveSymlinks(p, c.wall); changed {
		return roots.has(foldPath(r))
	}
	return false
}

// symEntry is a cached directory resolution.
type symEntry struct {
	resolved string
	until    int64
}

const maxCachedDirs = 4096

// resolveSymlinks resolves p the way a write to it would: the file itself
// if it is a symlink (one lstat), otherwise its directory, whose
// resolution is cached for recheckEvery. Edit tools hit the same few
// directories over and over, and a full EvalSymlinks costs one lstat per
// path component.
func (e *Engine) resolveSymlinks(p string, now int64) (string, bool) {
	if fi, err := os.Lstat(p); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return resolveSymlinks(p)
	}
	dir, file := filepath.Split(p)
	dir = filepath.Clean(dir)
	e.symMu.Lock()
	ent, ok := e.symDirs[dir]
	e.symMu.Unlock()
	if !ok || now >= ent.until || now < ent.until-int64(recheckEvery) {
		r, _ := resolveSymlinks(dir)
		ent = symEntry{resolved: r, until: now + int64(recheckEvery)}
		e.symMu.Lock()
		if len(e.symDirs) >= maxCachedDirs {
			clear(e.symDirs)
		}
		e.symDirs[dir] = ent
		e.symMu.Unlock()
	}
	out := filepath.Join(ent.resolved, file)
	return out, out != p
}

// protectedSet builds the engine-wide protected set. Entries with glob
// syntax become path-style globs (relative ones match at any depth, as in
// policy files); the rest are literal roots, also added with symlinks
// resolved. Invalid globs are skipped: Options come from code, not users.
func (e *Engine) protectedSet(in []string) *protectedSet {
	set := &protectedSet{}
	add := func(p string) {
		p = foldPath(p)
		if p != "" && p != "/" && !slices.Contains(set.roots, p) {
			set.roots = append(set.roots, p)
		}
	}
	for _, p := range in {
		if p == "" {
			continue
		}
		if hasGlobMeta(p) {
			if g, err := compileGlob(foldPath(pathPattern(p, e.home)), globPath); err == nil {
				set.globs = append(set.globs, g)
			}
			continue
		}
		abs := absPath(p, "", e.home)
		if !filepath.IsAbs(abs) {
			if a, err := filepath.Abs(abs); err == nil {
				abs = a
			}
		}
		add(abs)
		// /tmp → /private/tmp on macOS: agents may use either spelling.
		if r, changed := resolveSymlinks(abs); changed {
			add(r)
		}
	}
	return set
}

func (e *Engine) repoState(root string, nanos int64) *repoState {
	e.reposMu.RLock()
	rs := e.repos[root]
	e.reposMu.RUnlock()
	if rs == nil {
		e.reposMu.Lock()
		if rs = e.repos[root]; rs == nil {
			if len(e.repos) >= maxRepos {
				e.evictOldestRepo()
			}
			canon := root
			if r, err := filepath.EvalSymlinks(root); err == nil {
				canon = r
			}
			rs = &repoState{root: root, canon: canon,
				file: newPolicySlot(filepath.Join(root, RepoPolicyFile), e.compileFile)}
			e.repos[root] = rs
		}
		e.reposMu.Unlock()
	}
	rs.lastUsed.Store(nanos)
	return rs
}

// evictOldestRepo drops the least recently used repo (caller holds the
// write lock). It is re-read from disk if that repo is used again.
func (e *Engine) evictOldestRepo() {
	var oldest string
	var at int64 = 1<<63 - 1
	for k, r := range e.repos {
		if u := r.lastUsed.Load(); u < at {
			oldest, at = k, u
		}
	}
	delete(e.repos, oldest)
}

func (e *Engine) compileFile(path string, data []byte) ([]*rule, error) {
	return compilePolicy(path, data, e.home)
}

// ---- file cache -------------------------------------------------------

// snapshot is one load of a file. val is the last value that parsed;
// err is set when the newest version on disk failed to.
type snapshot[T any] struct {
	val     T
	err     error
	stamp   fileStamp
	exists  bool
	hadGood bool // val came from a successful parse
}

// slot caches one file. Readers never block on a reload: they get the
// current snapshot while one goroutine re-stats (and, if changed,
// recompiles) the file.
type slot[T any] struct {
	path string
	// emptyErr, if set, makes a 0-byte file an error instead of "no
	// content" (policy files: see refresh).
	emptyErr string
	parse    func(path string, data []byte) (T, error)
	checked  atomic.Int64 // wall-clock nanos of the last stat
	mu       sync.Mutex   // serialises refreshes
	cur      atomic.Pointer[snapshot[T]]
}

func newSlot[T any](path string, parse func(string, []byte) (T, error)) *slot[T] {
	return &slot[T]{path: path, parse: parse}
}

// readFile is os.ReadFile; tests swap it to simulate a concurrent writer.
var readFile = os.ReadFile

// emptyPolicyMsg explains why an empty policy file is not "no rules".
const emptyPolicyMsg = "policy file is empty — keeping the previous rules (write `version: 1` + `rules: []` to clear them, or delete the file)"

func newPolicySlot(path string, parse func(string, []byte) ([]*rule, error)) *slot[[]*rule] {
	s := newSlot(path, parse)
	s.emptyErr = emptyPolicyMsg
	return s
}

func (s *slot[T]) get(now int64, force bool) *snapshot[T] {
	snap := s.cur.Load()
	fresh := func() bool { return now-s.checked.Load() < int64(recheckEvery) && now >= s.checked.Load() }
	if !force && snap != nil && fresh() {
		return snap
	}
	if !force && snap != nil {
		// Someone else is refreshing: use what we have rather than wait.
		if !s.mu.TryLock() {
			return snap
		}
	} else {
		s.mu.Lock()
	}
	defer s.mu.Unlock()
	if cur := s.cur.Load(); !force && cur != nil && fresh() {
		return cur
	}
	next, settled := s.refresh(s.cur.Load())
	s.cur.Store(next)
	if settled {
		s.checked.Store(now)
	} // else: caught mid-write; the next call checks again straight away
	return next
}

// refresh re-reads the file if its stamp changed. settled is false when
// the file was caught being written; the returned snapshot then keeps the
// previous rules and a zero stamp, so the next check reads it again.
//
// Why: editors and os.WriteFile truncate and then write. A read in that
// window sees an empty or partial file, and an empty policy is valid YAML
// with no rules — every deny rule would silently vanish until the next
// change. So an empty file is an error (keep the last good rules), and a
// file whose size or mtime moves while we read it is retried.
func (s *slot[T]) refresh(prev *snapshot[T]) (snap *snapshot[T], settled bool) {
	if s.path == "" {
		return &snapshot[T]{}, true
	}
	st, err := statFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return &snapshot[T]{}, true // removed or never created: no rules, no error
	}
	keep := func(err error, stamp fileStamp) *snapshot[T] {
		n := &snapshot[T]{err: err, stamp: stamp, exists: true}
		if prev != nil {
			n.val, n.hadGood = prev.val, prev.hadGood
		}
		return n
	}
	if err != nil {
		return keep(err, st), true
	}
	if prev != nil && prev.exists && prev.stamp == st {
		return prev, true
	}
	if st.size > maxPolicyBytes {
		return keep(&FileError{File: s.path, Msg: fmt.Sprintf("file is larger than %d bytes", maxPolicyBytes)}, st), true
	}
	data, err := readFile(s.path)
	if err != nil {
		return keep(err, st), true
	}
	after, err := statFile(s.path)
	if err != nil || after != st || int64(len(data)) != st.size {
		// Mid-write (or replaced while reading): keep what we had, with its
		// error state, and don't remember this stamp as loaded.
		n := keep(nil, fileStamp{})
		if prev != nil {
			n.err = prev.err
		}
		return n, false
	}
	if len(data) == 0 && s.emptyErr != "" {
		return keep(&FileError{File: s.path, Msg: s.emptyErr}, st), true
	}
	v, err := s.parse(s.path, data)
	if err != nil {
		return keep(err, st), true
	}
	return &snapshot[T]{val: v, stamp: st, exists: true, hadGood: true}, true
}

// changed stats the file and compares it with the loaded snapshot.
func (s *slot[T]) changed() bool {
	snap := s.cur.Load()
	if snap == nil || s.path == "" {
		return false
	}
	st, err := statFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return snap.exists
	}
	if err != nil {
		return false
	}
	return !snap.exists || st != snap.stamp
}

// fileStamp identifies a version of a file without reading it. The inode
// catches atomic replace-by-rename within one mtime tick.
type fileStamp struct {
	mtime int64
	size  int64
	ino   uint64
}

func statFile(p string) (fileStamp, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return fileStamp{}, err
	}
	if fi.IsDir() {
		return fileStamp{}, fmt.Errorf("%s is a directory", p)
	}
	return fileStamp{mtime: fi.ModTime().UnixNano(), size: fi.Size(), ino: inode(fi)}, nil
}

// ---- trust ------------------------------------------------------------

// trustSet is the parsed trusted-repos.yaml: canonical repo roots.
type trustSet map[string]struct{}

func (t trustSet) has(r *repoState) bool {
	if t == nil {
		return false
	}
	_, a := t[r.root]
	_, b := t[r.canon]
	return a || b
}

func parseTrustSet(path string, data []byte) (trustSet, error) {
	repos, err := parseTrustedRepos(path, data)
	if err != nil {
		return nil, err
	}
	set := make(trustSet, len(repos)*2)
	for _, r := range repos {
		clean := filepath.Clean(r.Path)
		set[clean] = struct{}{}
		if c, err := filepath.EvalSymlinks(clean); err == nil {
			set[c] = struct{}{}
		}
	}
	return set, nil
}
