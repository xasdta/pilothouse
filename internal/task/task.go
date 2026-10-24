// Package task runs prompts as tasks: each gets its own git worktree and
// branch (pilothouse/<slug>) and a wrapped agent session, so several agents
// can work on one repository without touching the user's checkout
// (BUILD-SPEC §10). Every git operation goes through the git binary and
// only ever changes the task's own worktree and branch.
package task

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/client"
	"github.com/xasdta/pilothouse/internal/paths"
	"github.com/xasdta/pilothouse/internal/wrap"
)

// BranchPrefix namespaces task branches.
const BranchPrefix = "pilothouse/"

// WorktreesEnv overrides where task worktrees are created.
const WorktreesEnv = "PILOTHOUSE_WORKTREES"

// WorktreesRoot is the directory task worktrees are created in:
// $PILOTHOUSE_WORKTREES, else ~/.pilothouse-worktrees; each task gets
// <root>/<repo>/<slug>. It is deliberately outside the Pilothouse home
// (~/.pilothouse), which the built-in self-protection rule guards: an
// agent working in a worktree there would need a person for every tool
// call. Tasks made before this (worktrees in ~/.pilothouse/worktrees) keep
// the path they recorded, and policy exempts that directory too.
func WorktreesRoot() string {
	if d := os.Getenv(WorktreesEnv); d != "" {
		if abs, err := filepath.Abs(d); err == nil {
			return abs
		}
		return d
	}
	return filepath.Join(paths.UserHome(), ".pilothouse-worktrees")
}

// StartSpec is what a Starter needs to launch a task's agent.
type StartSpec struct {
	TaskID string
	Name   string // session name (the task slug)
	Dir    string // the worktree
	Argv   []string
	Env    []string // agent environment; nil: inherit
	Cols   uint16
	Rows   uint16
}

// Starter launches a task's agent in the background and returns its
// session id.
type Starter func(ctx context.Context, s StartSpec) (sessionID string, err error)

// SpawnStarter starts the agent under a wrapped-session holder, like
// `pilothouse run -d --task <id>`. The holder runs in its own session, so
// it outlives whoever started it (the CLI, or the daemon for a device).
func SpawnStarter(ctx context.Context, s StartSpec) (string, error) {
	return wrap.Spawn(ctx, wrap.SpawnOptions{
		Name: s.Name, TaskID: s.TaskID, Argv: s.Argv, Env: s.Env, Dir: s.Dir, Cols: s.Cols, Rows: s.Rows,
	})
}

// DaemonAPI is the part of the daemon a Manager reports tasks to.
// *client.Client implements it; its calls fail with client.ErrDaemonDown
// while no daemon runs, and are then skipped. The daemon passes itself when
// it starts a task for a device.
type DaemonAPI interface {
	PutTask(ctx context.Context, t api.Task) (api.Task, error)
	ReportOSC(ctx context.Context, sessionID string, b api.OSCBody) error
	Sessions(ctx context.Context) ([]api.Session, error)
}

// Manager implements the `pilothouse task` commands and, for the daemon,
// tasks started from devices: both go through Prepare, Create and Launch.
type Manager struct {
	Daemon DaemonAPI // default client.New(); used only when reachable
	Start  Starter   // default SpawnStarter
	Out    io.Writer // progress messages; default os.Stdout
	Err    io.Writer // warnings; default os.Stderr
	// SetupOut also gets the output of task.yaml's setup commands (the
	// CLI passes the terminal). The task log always gets it.
	SetupOut io.Writer
	Now      func() time.Time
}

func (m *Manager) daemon() DaemonAPI {
	if m.Daemon == nil {
		m.Daemon = client.New()
	}
	return m.Daemon
}

func (m *Manager) out() io.Writer {
	if m.Out == nil {
		return os.Stdout
	}
	return m.Out
}

func (m *Manager) errw() io.Writer {
	if m.Err == nil {
		return os.Stderr
	}
	return m.Err
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

// NewOptions configures `task new`.
type NewOptions struct {
	Prompt string
	Agent  string // claude | codex | any command taking the prompt as its argument
	Base   string // default: current branch (or HEAD's commit when detached)
	Name   string // slug; default: from the prompt
	Dir    string // where the command runs; must be inside a git repo
	Cols   uint16 // initial terminal size for the agent
	Rows   uint16
	// StartedBy is recorded on the task: "cli", "tui", "device:<id>".
	StartedBy string
}

// Bounds on what starts a task.
const (
	MaxPrompt  = 32 << 10
	maxRefLen  = 200
	maxNameLen = 100
)

// Prepared is a validated `task new`: what Create and Launch need.
type Prepared struct {
	opts      NewOptions
	agentPath string
	kind      api.AgentKind
	top       string // git top level of opts.Dir
	repo      string // main repository root
	base      string
	cfg       *Config // the repo's task.yaml; nil when it has none
	trusted   bool    // the repo is on the trust list (setup and env apply)
	trustErr  error   // reading the trust list failed (counts as untrusted)
	// AsksTrust is set by Launch when Claude will stop at its
	// folder-trust question before reading the prompt.
	AsksTrust bool
}

// RunsSetup reports whether Launch will run setup commands before the
// agent starts. They can take minutes.
func (p *Prepared) RunsSetup() bool { return p.cfg != nil && len(p.cfg.Setup) > 0 && p.trusted }

// Repo is the main repository root the task belongs to.
func (p *Prepared) Repo() string { return p.repo }

// New creates the worktree and branch, records the task and starts its
// agent (after task.yaml's setup, if any). If the setup or the agent's
// start fails after setup commands ran, the task is kept as failed so its
// worktree can be looked at; any other failure removes everything New
// created.
func (m *Manager) New(ctx context.Context, o NewOptions) (api.Task, error) {
	p, err := m.Prepare(ctx, o)
	if err != nil {
		return api.Task{}, err
	}
	t, err := m.Create(ctx, p)
	if err != nil {
		return t, err
	}
	return m.Launch(ctx, p, t)
}

// Prepare checks a new task without changing anything: the prompt, the
// agent (installed), the repository, the base, and the repo's task.yaml.
func (m *Manager) Prepare(ctx context.Context, o NewOptions) (*Prepared, error) {
	if strings.TrimSpace(o.Prompt) == "" {
		return nil, errors.New(`task needs a prompt: pilothouse task new "<what the agent should do>"`)
	}
	if len(o.Prompt) > MaxPrompt || strings.ContainsRune(o.Prompt, 0) || !utf8.ValidString(o.Prompt) {
		return nil, fmt.Errorf("the prompt must be text of at most %d KiB", MaxPrompt>>10)
	}
	if len(o.Name) > maxNameLen {
		return nil, fmt.Errorf("the task name must be at most %d characters", maxNameLen)
	}
	if o.Agent == "" {
		o.Agent = "claude"
	}
	agentPath, err := exec.LookPath(o.Agent)
	if err != nil {
		return nil, fmt.Errorf("agent %q not found on your PATH — install it or pass --agent", o.Agent)
	}
	if o.Dir == "" {
		if o.Dir, err = os.Getwd(); err != nil {
			return nil, err
		}
	}
	top, err := gitOutput(ctx, o.Dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("%s is not inside a git repository — run `pilothouse task new` from your repo", o.Dir)
	}
	p := &Prepared{opts: o, agentPath: agentPath, kind: wrap.KindFromCommand(o.Agent), top: top, repo: mainRepoRoot(ctx, top)}

	p.base = o.Base
	if p.base == "" {
		if p.base, err = gitOutput(ctx, top, "symbolic-ref", "--quiet", "--short", "HEAD"); err != nil {
			if p.base, err = gitOutput(ctx, top, "rev-parse", "HEAD"); err != nil {
				return nil, errors.New("the repository has no commits yet — commit something first, or pass --base")
			}
		}
	}
	if err := CheckRef(p.base); err != nil {
		return nil, fmt.Errorf("base %q %v", p.base, err)
	}
	sha, err := gitOutput(ctx, top, "rev-parse", "--verify", "--quiet", p.base+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("base %q is not a commit, branch or tag in %s", p.base, p.repo)
	}
	if strings.ContainsAny(p.base, "~^@:{}") {
		// The base is reused later (diff, ship) from the task's worktree,
		// where HEAD~2 would mean something else: pin it.
		p.base = sha
	}

	if p.cfg, err = LoadConfig(p.repo); err != nil {
		return nil, fmt.Errorf("%v — fix it or move it aside to start tasks here", err)
	}
	if p.cfg != nil && (len(p.cfg.Setup) > 0 || len(p.cfg.Env) > 0) {
		p.trusted, p.trustErr = RepoTrusted(p.repo)
	}
	return p, nil
}

// CheckRef refuses a base that git could read as an option, or that no
// branch, tag or revision can look like. Whether it names a commit is
// git's to say.
func CheckRef(ref string) error {
	switch {
	case ref == "":
		return errors.New("is empty")
	case len(ref) > maxRefLen:
		return fmt.Errorf("is longer than %d characters", maxRefLen)
	case strings.HasPrefix(ref, "-"):
		return errors.New("must not start with -")
	case strings.ContainsAny(ref, " \t\r\n\x00\x7f") || strings.Contains(ref, ".."):
		return errors.New("is not a branch, tag or commit name")
	}
	return nil
}

// Create makes the worktree and branch, copies task.yaml's files into it
// and records the task (status running, no session yet). On failure it
// removes what it made.
func (m *Manager) Create(ctx context.Context, p *Prepared) (t api.Task, err error) {
	existing, err := loadTasks()
	if err != nil {
		return t, err
	}
	o := p.opts
	want := Slugify(o.Prompt)
	if o.Name != "" {
		want = Slugify(o.Name)
	}
	wtRoot := filepath.Join(WorktreesRoot(), Slugify(filepath.Base(p.repo)))
	slug, err := uniqueSlug(ctx, p.top, wtRoot, want, existing)
	if err != nil {
		return t, err
	}
	now := m.now()
	t = api.Task{
		ID:        wrap.NewID("t_"),
		Title:     slug,
		Prompt:    o.Prompt,
		Agent:     p.kind,
		Repo:      p.repo,
		Worktree:  filepath.Join(wtRoot, slug),
		Branch:    BranchPrefix + slug,
		BaseRef:   p.base,
		Status:    api.TaskRunning,
		StartedBy: o.StartedBy,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := os.MkdirAll(wtRoot, 0o700); err != nil {
		return t, err
	}
	// --no-track: a remote base (origin/main) must not become the task
	// branch's upstream; `done --pr` sets the real one.
	if _, err := gitOutput(ctx, p.top, "worktree", "add", "--no-track", "-b", t.Branch, t.Worktree, p.base); err != nil {
		return t, err
	}
	defer func() {
		if err != nil {
			m.rollback(t, p.top)
		}
	}()
	if p.cfg != nil {
		t.Setup = m.prepareWorktree(p, &t)
	}

	// Record before starting so the daemon knows the task when the
	// session registers with its task id.
	if err = saveTask(t); err != nil {
		return t, err
	}
	m.mirror(ctx, t)
	return t, nil
}

// prepareWorktree copies task.yaml's files into the new worktree and says
// what will happen to its setup commands.
func (m *Manager) prepareWorktree(p *Prepared, t *api.Task) *api.TaskSetup {
	cfg := p.cfg
	st := &api.TaskSetup{Commands: len(cfg.Setup), Status: api.SetupNone}
	if len(cfg.Copy) > 0 {
		res, err := copyFiles(p.repo, t.Worktree, cfg.Copy)
		if err != nil {
			fmt.Fprintf(m.errw(), "warning: could not copy files into the worktree: %v\n", err)
		}
		st.Copied = res.Copied
		if n := len(res.Copied); n > 0 {
			fmt.Fprintf(m.out(), "copied %s from %s: %s\n", plural(n, "file"), p.repo, strings.Join(firstN(res.Copied, 8), ", "))
		}
		for _, s := range res.Skipped {
			fmt.Fprintf(m.errw(), "note: not copied: %s\n", s)
		}
	}
	switch {
	case len(cfg.Setup) == 0:
	case p.trusted:
		st.Status, st.Log = api.SetupRunning, LogPath(t.ID)
	default:
		st.Status = api.SetupSkipped
		why := p.repo + " is not trusted"
		if p.trustErr != nil {
			why = "the trust list could not be read (" + p.trustErr.Error() + ")"
		}
		t.Note = fmt.Sprintf("The setup commands in %s were not run: %s. Trust it with `pilothouse policy trust %s`.", ConfigFile, why, p.repo)
		fmt.Fprintf(m.out(), "note: %s has %s, but they were not run: %s:\n", ConfigFile, plural(len(cfg.Setup), "setup command"), why)
		for _, c := range cfg.Setup {
			fmt.Fprintf(m.out(), "    %s\n", c)
		}
		fmt.Fprintf(m.out(), "  trust the repo to run them for new tasks (pilothouse policy trust %s), or run them yourself in %s\n", p.repo, t.Worktree)
	}
	if len(cfg.Env) > 0 && !p.trusted {
		fmt.Fprintf(m.out(), "note: env from %s not applied (the repo is not trusted)\n", ConfigFile)
	}
	return st
}

// Launch runs task.yaml's setup commands (trusted repos only) and starts
// the agent. A failure after setup commands ran marks the task failed and
// keeps the worktree; a failure without setup removes the task again.
func (m *Manager) Launch(ctx context.Context, p *Prepared, t api.Task) (api.Task, error) {
	if p.RunsSetup() {
		fmt.Fprintf(m.out(), "running %s from %s (log: %s)…\n", plural(len(p.cfg.Setup), "setup command"), ConfigFile, LogPath(t.ID))
		if err := runSetup(ctx, t, p.cfg, m.SetupOut); err != nil {
			t.Setup.Status = api.SetupFailed
			return m.fail(ctx, t, fmt.Errorf("setup failed: %w (log: %s)", err, LogPath(t.ID)))
		}
		t.Setup.Status = api.SetupOK
	}

	o := p.opts
	argv := []string{p.agentPath}
	if (p.kind == api.AgentClaude || p.kind == api.AgentCodex) && strings.HasPrefix(o.Prompt, "-") {
		argv = append(argv, "--") // the prompt is not an option
	}
	argv = append(argv, o.Prompt)
	var env []string
	if p.trusted && p.cfg != nil && len(p.cfg.Env) > 0 {
		env = append(os.Environ(), p.cfg.envList()...)
	}
	start := m.Start
	if start == nil {
		start = SpawnStarter
	}
	sid, err := start(ctx, StartSpec{
		TaskID: t.ID, Name: t.Title, Dir: t.Worktree,
		Argv: argv, Env: env, Cols: o.Cols, Rows: o.Rows,
	})
	if err != nil {
		err = fmt.Errorf("start %s: %w", o.Agent, err)
		if p.RunsSetup() {
			return m.fail(ctx, t, err)
		}
		m.rollback(t, p.top)
		return t, err
	}
	t.SessionID = sid
	t.UpdatedAt = m.now()
	if t.Agent == api.AgentClaude && ClaudeAsksTrust(p.repo, t.Worktree) {
		p.AsksTrust = true
		t.Note = strings.TrimSpace(t.Note + " Claude is asking whether to trust this folder — attach to answer: pilothouse attach " + sid)
	}
	if err := saveTask(t); err != nil {
		return t, err
	}
	m.mirror(ctx, t)
	if p.AsksTrust {
		m.surfaceTrustPrompt(ctx, t)
	}
	return t, nil
}

// fail records that a task could not start. The worktree stays for a look
// (`pilothouse task abandon` removes it).
func (m *Manager) fail(ctx context.Context, t api.Task, cause error) (api.Task, error) {
	t.Status, t.Note, t.UpdatedAt = api.TaskFailed, Short(cause.Error(), 300), m.now()
	if err := saveTask(t); err != nil {
		fmt.Fprintf(m.errw(), "warning: could not record task %s as failed: %v\n", t.ID, err)
	}
	m.mirror(context.WithoutCancel(ctx), t)
	return t, fmt.Errorf("%w — the worktree is kept at %s; remove it with `pilothouse task abandon %s`", cause, t.Worktree, t.ID)
}

// rollback undoes a half-created task. Best effort: it runs on an error
// path and reports what it could not clean up.
func (m *Manager) rollback(t api.Task, top string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := gitOutput(ctx, top, "worktree", "remove", "--force", t.Worktree); err != nil {
		fmt.Fprintf(m.errw(), "warning: could not remove worktree %s: %v\n", t.Worktree, err)
	}
	if _, err := gitOutput(ctx, top, "branch", "-D", t.Branch); err != nil {
		fmt.Fprintf(m.errw(), "warning: could not delete branch %s: %v\n", t.Branch, err)
	}
	_ = deleteTask(t.ID)
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return strconv.Itoa(n) + " " + what + "s"
}

func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(append([]string(nil), s[:n]...), fmt.Sprintf("… (%d more)", len(s)-n))
}

// mainRepoRoot maps a worktree's top level to the main repository root,
// so tasks started from inside another worktree still belong to the repo.
func mainRepoRoot(ctx context.Context, top string) string {
	common, err := gitOutput(ctx, top, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || filepath.Base(common) != ".git" {
		return top
	}
	return filepath.Dir(common)
}

func uniqueSlug(ctx context.Context, repo, wtRoot, want string, existing []api.Task) (string, error) {
	taken := map[string]bool{}
	for _, t := range existing {
		taken[strings.TrimPrefix(t.Branch, BranchPrefix)] = true
	}
	for i := 1; i <= 100; i++ {
		s := want
		if i > 1 {
			suffix := "-" + strconv.Itoa(i)
			s = strings.TrimRight(want[:min(len(want), maxSlugLen-len(suffix))], "-") + suffix
		}
		if taken[s] {
			continue
		}
		if gitOK(ctx, repo, "show-ref", "--verify", "--quiet", "refs/heads/"+BranchPrefix+s) {
			continue
		}
		if _, err := os.Lstat(filepath.Join(wtRoot, s)); err == nil {
			continue
		}
		return s, nil
	}
	return "", fmt.Errorf("could not find a free name for %q — pass --name", want)
}

// mirror sends a task to the daemon if it is running. Tasks work without
// the daemon; it only needs them for the inbox and the web app.
func (m *Manager) mirror(ctx context.Context, t api.Task) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := m.daemon().PutTask(cctx, t); err != nil && !errors.Is(err, client.ErrDaemonDown) {
		fmt.Fprintf(m.errw(), "warning: could not record task %s in the daemon: %v\n", t.ID, err)
	}
}

// Get finds a task by id, unique id prefix (with or without "t_"), or slug.
func (m *Manager) Get(ref string) (api.Task, error) {
	tasks, err := loadTasks()
	if err != nil {
		return api.Task{}, err
	}
	var matches []api.Task
	for _, t := range tasks {
		if t.ID == ref || t.Title == ref {
			return t, nil
		}
		if strings.HasPrefix(t.ID, ref) || strings.HasPrefix(t.ID, "t_"+ref) {
			matches = append(matches, t)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return api.Task{}, fmt.Errorf("no task matches %q — list tasks with `pilothouse task ls --all`", ref)
	}
	var ids []string
	for _, t := range matches {
		ids = append(ids, t.ID+" ("+t.Title+")")
	}
	return api.Task{}, fmt.Errorf("%q matches several tasks: %s", ref, strings.Join(ids, ", "))
}

// View is a task with its live status.
type View struct {
	api.Task
	SessionState api.State // "" when unknown
}

// List returns every task, newest first, with status derived from its
// session: a working (or blocked, starting) agent is "running"; one that
// is waiting for a prompt, has errored or exited is ready for "review".
func (m *Manager) List(ctx context.Context) ([]View, error) {
	tasks, err := loadTasks()
	if err != nil {
		return nil, err
	}
	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	sessions, derr := m.daemon().Sessions(dctx)
	cancel()
	byID := map[string]api.Session{}
	if derr == nil {
		for _, s := range sessions {
			byID[s.ID] = s
		}
	}
	views := make([]View, 0, len(tasks))
	for i := len(tasks) - 1; i >= 0; i-- {
		v := View{Task: tasks[i]}
		if v.Status == api.TaskRunning && v.SessionID != "" {
			if s, ok := byID[v.SessionID]; ok {
				v.SessionState = s.State
			} else if derr != nil {
				// No daemon: ask the holder directly.
				if wrap.Alive(ctx, v.SessionID) {
					v.SessionState = api.StateWorking
				} else {
					v.SessionState = api.StateExited
				}
			} else {
				// The daemon has no record and nothing is listening.
				v.SessionState = api.StateExited
				if wrap.Alive(ctx, v.SessionID) {
					v.SessionState = api.StateWorking
				}
			}
			v.Status = StatusFor(v.SessionState)
		}
		views = append(views, v)
	}
	return views, nil
}

// StatusFor maps a session state to the status of a task that is not
// done or abandoned.
func StatusFor(s api.State) api.TaskStatus {
	switch s {
	case api.StateWaiting, api.StateExited, api.StateErrored:
		return api.TaskReview
	}
	return api.TaskRunning
}

// Diff writes what the task changed relative to the point where its
// branch left the base: commits plus uncommitted and untracked work in
// the worktree. Once the worktree is gone it is `git diff base...branch`.
func (m *Manager) Diff(ctx context.Context, ref string, stat bool, w io.Writer) error {
	t, err := m.Get(ref)
	if err != nil {
		return err
	}
	flags := []string{"diff"}
	if stat {
		flags = append(flags, "--stat")
	}
	if !dirExists(t.Worktree) {
		return gitStream(ctx, t.Repo, w, append(flags, t.BaseRef+"..."+t.Branch, "--"))
	}
	mb, err := gitOutput(ctx, t.Worktree, "merge-base", t.BaseRef, "HEAD")
	if err != nil {
		return fmt.Errorf("can't find where %s left %s: %w", t.Branch, t.BaseRef, err)
	}
	if err := gitStream(ctx, t.Worktree, w, append(flags, mb, "--")); err != nil {
		return err
	}
	// New files the agent has not added yet are part of its work too.
	// They are shown without staging them, so the task's index is left
	// exactly as the agent made it.
	untracked, err := gitOutput(ctx, t.Worktree, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	for _, f := range strings.Split(untracked, "\x00") {
		if f == "" {
			continue
		}
		if stat {
			fmt.Fprintf(w, " %s | new file (untracked)\n", f)
			continue
		}
		if err := gitStream(ctx, t.Worktree, w, []string{"diff", "--no-index", "--", os.DevNull, f}, 1); err != nil {
			return err
		}
	}
	return nil
}

// DoneOptions configures `task done`.
type DoneOptions struct {
	PR    bool // push and open a pull request with gh
	Keep  bool // keep the worktree
	Force bool // discard uncommitted changes / ignore a running agent
}

// Done finishes a task.
func (m *Manager) Done(ctx context.Context, ref string, o DoneOptions) (api.Task, error) {
	t, err := m.openTask(ctx, ref, o.Force)
	if err != nil {
		return t, err
	}
	wt := dirExists(t.Worktree)
	if wt {
		dirty, err := gitOutput(ctx, t.Worktree, "status", "--porcelain")
		if err != nil {
			return t, err
		}
		if dirty != "" && !o.Force {
			return t, fmt.Errorf("worktree %s has uncommitted changes — commit them (cd %s && git add -A && git commit) or pass --force to discard them", t.Worktree, t.Worktree)
		}
	}
	if o.PR {
		if err := m.openPR(ctx, t); err != nil {
			return t, err
		}
	}
	if wt && !o.Keep {
		if err := m.stopAgent(ctx, t); err != nil {
			return t, err
		}
		if err := removeWorktree(ctx, t, o.Force); err != nil {
			return t, err
		}
	}
	t.Status, t.UpdatedAt = api.TaskDone, m.now()
	if err := saveTask(t); err != nil {
		return t, err
	}
	m.mirror(ctx, t)
	return t, nil
}

func (m *Manager) openPR(ctx context.Context, t api.Task) error {
	gh, err := exec.LookPath("gh")
	if err != nil {
		return fmt.Errorf("gh (GitHub CLI) not found — install it from https://cli.github.com, or push yourself: git -C %s push -u origin %s", t.Repo, t.Branch)
	}
	if _, err := gitOutput(ctx, t.Repo, "remote", "get-url", "origin"); err != nil {
		return fmt.Errorf("the repository has no \"origin\" remote to push %s to", t.Branch)
	}
	ahead, err := gitOutput(ctx, t.Repo, "rev-list", "--count", t.BaseRef+".."+t.Branch)
	if err != nil {
		return err
	}
	if ahead == "0" {
		return fmt.Errorf("%s has no commits beyond %s — nothing to open a pull request for", t.Branch, t.BaseRef)
	}
	fmt.Fprintf(m.out(), "pushing %s to origin…\n", t.Branch)
	if err := gitInteractive(ctx, t.Repo, m.out(), m.errw(), "push", "-u", "origin", t.Branch); err != nil {
		return err
	}
	args := []string{"pr", "create", "--fill", "--head", t.Branch}
	// Target the task's base when it is a branch the remote has; else
	// gh picks the repository's default branch.
	if gitOK(ctx, t.Repo, "show-ref", "--verify", "--quiet", "refs/remotes/origin/"+t.BaseRef) {
		args = append(args, "--base", t.BaseRef)
	}
	cmd := exec.CommandContext(ctx, gh, args...)
	cmd.Dir = t.Repo
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, m.out(), m.errw()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh pr create failed (%v) — the branch is pushed; open the PR on GitHub, then run `pilothouse task done %s`", err, t.ID)
	}
	return nil
}

// AbandonOptions configures `task abandon`.
type AbandonOptions struct {
	KeepBranch bool // keep pilothouse/<slug>
	Force      bool // even while the agent is running
}

// Abandon removes the task's worktree (discarding its changes) and, unless
// KeepBranch, its branch. The branch's last commit is printed so it can be
// recovered.
func (m *Manager) Abandon(ctx context.Context, ref string, o AbandonOptions) (api.Task, error) {
	t, err := m.openTask(ctx, ref, o.Force)
	if err != nil {
		return t, err
	}
	if err := m.stopAgent(ctx, t); err != nil {
		return t, err
	}
	if dirExists(t.Worktree) {
		if err := removeWorktree(ctx, t, true); err != nil {
			return t, err
		}
	} else {
		_, _ = gitOutput(ctx, t.Repo, "worktree", "prune")
	}
	t.Note = ""
	if !o.KeepBranch {
		if sha, err := gitOutput(ctx, t.Repo, "rev-parse", "--short", "refs/heads/"+t.Branch); err == nil {
			if _, err := gitOutput(ctx, t.Repo, "branch", "-D", t.Branch); err != nil {
				return t, err
			}
			t.Note = fmt.Sprintf("Deleted branch %s (was %s; restore with: git branch %s %s).", t.Branch, sha, t.Branch, sha)
			fmt.Fprintf(m.out(), "deleted branch %s (was %s; restore with: git branch %s %s)\n", t.Branch, sha, t.Branch, sha)
		}
	}
	t.Status, t.UpdatedAt = api.TaskAbandon, m.now()
	if err := saveTask(t); err != nil {
		return t, err
	}
	m.mirror(ctx, t)
	return t, nil
}

// openTask loads a task that is still open and, unless force, whose agent
// has stopped.
func (m *Manager) openTask(ctx context.Context, ref string, force bool) (api.Task, error) {
	t, err := m.Get(ref)
	if err != nil {
		return t, err
	}
	if t.Status == api.TaskDone || t.Status == api.TaskAbandon {
		return t, fmt.Errorf("task %s (%s) is already %s", t.ID, t.Title, t.Status)
	}
	if t.SessionID != "" && !force && wrap.Alive(ctx, t.SessionID) {
		return t, fmt.Errorf("the agent for task %s is still running — exit it first (pilothouse attach %s), or pass --force", t.Title, t.SessionID)
	}
	return t, nil
}

// stopAgent ends a task's agent that is still running (only reachable
// with --force) before its worktree is deleted from under it.
func (m *Manager) stopAgent(ctx context.Context, t api.Task) error {
	if t.SessionID == "" || !wrap.Alive(ctx, t.SessionID) {
		return nil
	}
	fmt.Fprintf(m.out(), "stopping the agent (session %s)…\n", t.SessionID)
	return wrap.Stop(ctx, t.SessionID)
}

func removeWorktree(ctx context.Context, t api.Task, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	if _, err := gitOutput(ctx, t.Repo, append(args, t.Worktree)...); err != nil {
		return err
	}
	return nil
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
