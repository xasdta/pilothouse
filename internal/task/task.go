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
	Cols   uint16
	Rows   uint16
}

// Starter launches a task's agent in the background and returns its
// session id.
type Starter func(ctx context.Context, s StartSpec) (sessionID string, err error)

// SpawnStarter starts the agent under a wrapped-session holder, like
// `pilothouse run -d --task <id>`.
func SpawnStarter(ctx context.Context, s StartSpec) (string, error) {
	return wrap.Spawn(ctx, wrap.SpawnOptions{
		Name: s.Name, TaskID: s.TaskID, Argv: s.Argv, Dir: s.Dir, Cols: s.Cols, Rows: s.Rows,
	})
}

// Manager implements the `pilothouse task` commands.
type Manager struct {
	Daemon *client.Client // default client.New(); used only when reachable
	Start  Starter        // default SpawnStarter
	Out    io.Writer      // progress messages; default os.Stdout
	Err    io.Writer      // warnings; default os.Stderr
	Now    func() time.Time
}

func (m *Manager) daemon() *client.Client {
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
}

// New creates the worktree and branch, records the task and starts its
// agent. On failure everything it created is removed again.
func (m *Manager) New(ctx context.Context, o NewOptions) (t api.Task, err error) {
	if strings.TrimSpace(o.Prompt) == "" {
		return t, errors.New(`task needs a prompt: pilothouse task new "<what the agent should do>"`)
	}
	if o.Agent == "" {
		o.Agent = "claude"
	}
	agentPath, err := exec.LookPath(o.Agent)
	if err != nil {
		return t, fmt.Errorf("agent %q not found on your PATH — install it or pass --agent", o.Agent)
	}
	if o.Dir == "" {
		if o.Dir, err = os.Getwd(); err != nil {
			return t, err
		}
	}
	top, err := gitOutput(ctx, o.Dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return t, fmt.Errorf("%s is not inside a git repository — run `pilothouse task new` from your repo", o.Dir)
	}
	repo := mainRepoRoot(ctx, top)

	base := o.Base
	if base == "" {
		if base, err = gitOutput(ctx, top, "symbolic-ref", "--quiet", "--short", "HEAD"); err != nil {
			if base, err = gitOutput(ctx, top, "rev-parse", "HEAD"); err != nil {
				return t, errors.New("the repository has no commits yet — commit something first, or pass --base")
			}
		}
	}
	if !gitOK(ctx, top, "rev-parse", "--verify", "--quiet", base+"^{commit}") {
		return t, fmt.Errorf("base %q is not a commit, branch or tag in %s", base, repo)
	}

	existing, err := loadTasks()
	if err != nil {
		return t, err
	}
	want := Slugify(o.Prompt)
	if o.Name != "" {
		want = Slugify(o.Name)
	}
	wtRoot := filepath.Join(WorktreesRoot(), Slugify(filepath.Base(repo)))
	slug, err := uniqueSlug(ctx, top, wtRoot, want, existing)
	if err != nil {
		return t, err
	}
	now := m.now()
	t = api.Task{
		ID:        wrap.NewID("t_"),
		Title:     slug,
		Prompt:    o.Prompt,
		Agent:     wrap.KindFromCommand(o.Agent),
		Repo:      repo,
		Worktree:  filepath.Join(wtRoot, slug),
		Branch:    BranchPrefix + slug,
		BaseRef:   base,
		Status:    api.TaskRunning,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := os.MkdirAll(wtRoot, 0o700); err != nil {
		return t, err
	}
	// --no-track: a remote base (origin/main) must not become the task
	// branch's upstream; `done --pr` sets the real one.
	if _, err := gitOutput(ctx, top, "worktree", "add", "--no-track", "-b", t.Branch, t.Worktree, base); err != nil {
		return t, err
	}
	defer func() {
		if err != nil {
			m.rollback(t, top)
		}
	}()

	// Record before starting so the daemon knows the task when the
	// session registers with its task id.
	if err = saveTask(t); err != nil {
		return t, err
	}
	m.mirror(ctx, t)

	start := m.Start
	if start == nil {
		start = SpawnStarter
	}
	sid, err := start(ctx, StartSpec{
		TaskID: t.ID, Name: slug, Dir: t.Worktree,
		Argv: []string{agentPath, o.Prompt}, Cols: o.Cols, Rows: o.Rows,
	})
	if err != nil {
		return t, fmt.Errorf("start %s: %w", o.Agent, err)
	}
	t.SessionID = sid
	t.UpdatedAt = m.now()
	if err = saveTask(t); err != nil {
		return t, err
	}
	m.mirror(ctx, t)
	if t.Agent == api.AgentClaude && ClaudeAsksTrust(repo, t.Worktree) {
		m.surfaceTrustPrompt(ctx, t)
	}
	return t, nil
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
	if !o.KeepBranch {
		if sha, err := gitOutput(ctx, t.Repo, "rev-parse", "--short", "refs/heads/"+t.Branch); err == nil {
			if _, err := gitOutput(ctx, t.Repo, "branch", "-D", t.Branch); err != nil {
				return t, err
			}
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
