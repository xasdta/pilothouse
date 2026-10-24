package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/notify"
	"github.com/xasdta/pilothouse/internal/paths"
	"github.com/xasdta/pilothouse/internal/store"
	"github.com/xasdta/pilothouse/internal/task"
)

// Tasks from devices (F7; workstream G). A device (or the local API)
// starts a task exactly like `pilothouse task new`, through the same
// task.Manager steps: worktree and branch, task.yaml's copies and setup,
// then the agent under a detached holder. The daemon is the holder's
// parent, so the agent never descends from another agent or from the
// phone's request. tasks.json stays the record the CLI reads; the
// database mirrors it for snapshots and events.

var _ core.TaskRunner = (*Daemon)(nil)

// Agents a device may start as a task, by the command that runs them.
var taskAgents = map[api.AgentKind]string{
	api.AgentClaude: "claude",
	api.AgentCodex:  "codex",
}

const (
	maxRepos       = 50
	repoScan       = 100 // most recent session repo roots looked at
	repoInfoTTL    = 30 * time.Second
	maxTaskList    = 500
	taskStartLimit = 2 * time.Minute // worktree, copies and the agent's start
	taskFinishWait = 6 * time.Minute // a ship: commit, push, gh
	taskDiffWait   = 30 * time.Second
)

// taskState is the daemon's bookkeeping for tasks.
type taskState struct {
	startMu sync.Mutex // one Create at a time: slugs and branches are picked there

	mu   sync.Mutex
	busy map[string]bool // tasks being shipped or discarded

	repoMu sync.Mutex
	repos  map[string]repoEntry
}

type repoEntry struct {
	info api.RepoInfo
	ok   bool
	at   time.Time
}

// taskAPI is what the task manager reports to: the daemon itself.
type taskAPI struct{ d *Daemon }

func (a taskAPI) PutTask(ctx context.Context, t api.Task) (api.Task, error) {
	return a.d.PutTask(ctx, t)
}
func (a taskAPI) ReportOSC(ctx context.Context, id string, b api.OSCBody) error {
	return a.d.ReportOSC(ctx, id, b)
}
func (a taskAPI) Sessions(ctx context.Context) ([]api.Session, error) {
	return a.d.db.Sessions(ctx, time.Time{})
}

// logWriter turns the task manager's warnings into daemon log lines.
type logWriter struct{ d *Daemon }

func (w logWriter) Write(p []byte) (int, error) {
	for _, l := range strings.Split(strings.TrimSpace(string(p)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			w.d.log.Warn("task", "msg", l)
		}
	}
	return len(p), nil
}

func (d *Daemon) taskManager() *task.Manager {
	start := d.opts.TaskStarter
	if start == nil {
		start = task.SpawnStarter
	}
	// Progress and setup output (which may show task.yaml's commands) stay
	// out of the daemon log; the task log has the setup output.
	return &task.Manager{Daemon: taskAPI{d}, Start: start, Out: io.Discard, Err: logWriter{d}, Now: d.now}
}

// taskErr makes a task error safe to show: the task package's messages are
// written for people (git's own messages, what to do next).
func taskErr(err error) error {
	switch {
	case err == nil, errors.Is(err, core.ErrInvalid), errors.Is(err, core.ErrNotFound),
		errors.Is(err, core.ErrUnsupported), errors.Is(err, context.Canceled):
		return err
	}
	return core.Invalid("%s", err.Error())
}

// ---- repos ----

// Repos lists repositories tasks may start in: the main checkouts of
// repos sessions ran in, plus config tasks.repos, that still exist, most
// recently used first.
func (d *Daemon) Repos(ctx context.Context) ([]api.RepoInfo, error) {
	uses, err := d.db.RepoRoots(ctx, repoScan)
	if err != nil {
		return nil, err
	}
	byRoot := map[string]*api.RepoInfo{}
	add := func(dir string, last time.Time) {
		info, ok := d.repoInfo(ctx, dir)
		if !ok {
			return
		}
		if r := byRoot[info.Root]; r != nil {
			if last.After(r.LastUsed) {
				r.LastUsed = last
			}
			return
		}
		info.LastUsed = last
		byRoot[info.Root] = &info
	}
	for _, u := range uses {
		add(u.Root, u.LastUsed)
	}
	for _, r := range d.cfg.Tasks.Repos {
		if strings.HasPrefix(r, "~/") {
			r = filepath.Join(paths.UserHome(), r[2:])
		}
		if filepath.IsAbs(r) {
			add(filepath.Clean(r), time.Time{})
		}
	}
	out := make([]api.RepoInfo, 0, len(byRoot))
	for _, r := range byRoot {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastUsed.Equal(out[j].LastUsed) {
			return out[i].LastUsed.After(out[j].LastUsed)
		}
		return out[i].Root < out[j].Root
	})
	if len(out) > maxRepos {
		out = out[:maxRepos]
	}
	return out, nil
}

// repoInfo maps a directory to its main checkout (a task worktree, or any
// linked worktree, belongs to its repository) and that checkout's branch.
// Results are cached briefly: the repo list is read on every new-task form.
func (d *Daemon) repoInfo(ctx context.Context, dir string) (api.RepoInfo, bool) {
	now := d.now()
	d.tasks.repoMu.Lock()
	if e, ok := d.tasks.repos[dir]; ok && now.Sub(e.at) < repoInfoTTL {
		d.tasks.repoMu.Unlock()
		return e.info, e.ok
	}
	d.tasks.repoMu.Unlock()

	info, ok := lookupRepo(ctx, dir)
	d.tasks.repoMu.Lock()
	if d.tasks.repos == nil || len(d.tasks.repos) > 1024 {
		d.tasks.repos = map[string]repoEntry{}
	}
	d.tasks.repos[dir] = repoEntry{info: info, ok: ok, at: now}
	d.tasks.repoMu.Unlock()
	return info, ok
}

func lookupRepo(ctx context.Context, dir string) (api.RepoInfo, bool) {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return api.RepoInfo{}, false
	}
	out, err := gitQuiet(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir", "--show-toplevel")
	lines := strings.Split(out, "\n")
	if err != nil || len(lines) != 2 {
		return api.RepoInfo{}, false
	}
	common, top := lines[0], lines[1]
	root := top
	if filepath.Base(common) == ".git" {
		root = filepath.Dir(common)
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return api.RepoInfo{}, false
	}
	branch, _ := gitQuiet(ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD")
	return api.RepoInfo{Root: root, Name: filepath.Base(root), Branch: branch}, true
}

// gitQuiet runs a quick read-only git query next to agents that may be
// using the same repository.
func gitQuiet(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	b, err := cmd.Output()
	return strings.TrimSpace(string(b)), err
}

// ---- tasks ----

// Tasks lists tasks, newest first: tasks.json (which the CLI keeps even
// while the daemon is down) merged with the database, the newer record
// winning, and open tasks' status taken from their agent's session.
func (d *Daemon) Tasks(ctx context.Context) ([]api.Task, error) {
	local, lerr := task.Load()
	if lerr != nil {
		d.log.Warn("read tasks.json", "err", lerr)
	}
	stored, err := d.db.Tasks(ctx, time.Time{})
	if err != nil {
		return nil, err
	}
	byID := map[string]api.Task{}
	for _, t := range stored {
		byID[t.ID] = t
	}
	for _, t := range local {
		if o, ok := byID[t.ID]; !ok || !o.UpdatedAt.After(t.UpdatedAt) {
			byID[t.ID] = t
		}
	}
	out := make([]api.Task, 0, len(byID))
	for _, t := range byID {
		if (t.Status == api.TaskRunning || t.Status == api.TaskReview) && t.SessionID != "" {
			if s, err := d.db.Session(ctx, t.SessionID); err == nil {
				t.Status = task.StatusFor(s.State)
			}
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	if len(out) > maxTaskList {
		out = out[:maxTaskList]
	}
	return out, nil
}

// findTask returns a task by id (or unique prefix), from tasks.json, or
// from the database (and then records it in tasks.json, which the task
// manager works on).
func (d *Daemon) findTask(ctx context.Context, ref string) (api.Task, error) {
	local, err := task.Load()
	if err != nil {
		return api.Task{}, err
	}
	id := ref
	if full, err := d.db.ResolveTaskID(ctx, ref); err == nil {
		id = full
	}
	for _, t := range local {
		if t.ID == id {
			return t, nil
		}
	}
	t, err := d.db.Task(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return api.Task{}, core.ErrNotFound
	}
	if err != nil {
		return api.Task{}, err
	}
	return t, task.Save(t)
}

// StartTask starts an agent on a prompt in a new worktree of a known repo.
// Without setup commands it returns once the agent runs; with them it
// returns once the worktree is ready, and runs setup and the agent in the
// background (the task shows setup "running", then gets its session, or
// fails with a note and a notification).
func (d *Daemon) StartTask(ctx context.Context, ts api.TaskStart, by string) (api.Task, error) {
	prompt := strings.TrimSpace(ts.Prompt)
	switch {
	case prompt == "":
		return api.Task{}, core.Invalid("the task needs a prompt")
	case len(prompt) > task.MaxPrompt || strings.ContainsRune(prompt, 0) || !utf8.ValidString(prompt):
		return api.Task{}, core.Invalid("the prompt must be text of at most %d KiB", task.MaxPrompt>>10)
	case len(ts.Name) > 100:
		return api.Task{}, core.Invalid("the task name must be at most 100 characters")
	}
	if ts.Agent == "" {
		ts.Agent = api.AgentClaude
	}
	cmd, ok := taskAgents[ts.Agent]
	if !ok {
		return api.Task{}, core.Invalid("%q can't be started as a task (supported: claude, codex)", ts.Agent)
	}
	if _, err := exec.LookPath(cmd); err != nil {
		return api.Task{}, core.Invalid("%s is not installed on %s (%s is not on the Pilothouse daemon's PATH)", notify.AgentName(ts.Agent), d.machine, cmd)
	}
	if ts.Base != "" {
		if err := task.CheckRef(ts.Base); err != nil {
			return api.Task{}, core.Invalid("base %q %v", ts.Base, err)
		}
	}
	repo, err := d.knownRepo(ctx, ts.Repo)
	if err != nil {
		return api.Task{}, err
	}

	m := d.taskManager()
	sctx, cancel := context.WithTimeout(d.ctx, taskStartLimit)
	defer cancel()
	d.tasks.startMu.Lock()
	p, err := m.Prepare(sctx, task.NewOptions{Prompt: prompt, Agent: cmd, Base: ts.Base, Name: ts.Name, Dir: repo, StartedBy: by})
	var t api.Task
	if err == nil {
		t, err = m.Create(sctx, p)
	}
	d.tasks.startMu.Unlock()
	if err != nil {
		return api.Task{}, taskErr(err)
	}
	d.log.Info("task started", "task", t.ID, "repo", t.Repo, "agent", t.Agent, "by", by, "setup", p.RunsSetup())
	if p.RunsSetup() {
		d.goBackground(func() { _, _ = d.launchTask(d.ctx, m, p, t) })
		return t, nil
	}
	return d.launchTask(sctx, m, p, t)
}

// knownRepo checks a device's repo against Repos (exact root).
func (d *Daemon) knownRepo(ctx context.Context, repo string) (string, error) {
	if repo == "" || !filepath.IsAbs(repo) {
		return "", core.Invalid("repo must be the absolute path of a repository from the list")
	}
	repo = filepath.Clean(repo)
	repos, err := d.Repos(ctx)
	if err != nil {
		return "", err
	}
	for _, r := range repos {
		if r.Root == repo {
			return repo, nil
		}
	}
	return "", core.Invalid("%s is not a repository Pilothouse knows — run an agent there once, or add it to tasks.repos in %s", repo, paths.Config())
}

// launchTask runs setup (if any) and starts the agent, telling the person
// when the task could not start or is waiting at Claude's trust question.
func (d *Daemon) launchTask(ctx context.Context, m *task.Manager, p *task.Prepared, t api.Task) (api.Task, error) {
	out, err := m.Launch(ctx, p, t)
	if err != nil {
		d.log.Warn("task did not start", "task", t.ID, "err", err)
		if out.Status == api.TaskFailed {
			d.notifyTask(out, nil, core.NotifyBlocked, "Task didn't start", out.Note, true)
		}
		return out, taskErr(err)
	}
	if p.AsksTrust {
		d.notifyTask(out, nil, core.NotifyBlocked, "Task waiting",
			"Claude is asking whether to trust this folder — attach to the session to answer", true)
	}
	return out, nil
}

// TaskDiff is what a task changed relative to its base.
func (d *Daemon) TaskDiff(ctx context.Context, taskID string) (api.TaskDiff, error) {
	t, err := d.findTask(ctx, taskID)
	if err != nil {
		return api.TaskDiff{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, taskDiffWait)
	defer cancel()
	diff, err := task.Changes(ctx, t)
	return diff, taskErr(err)
}

// FinishTask ships (commit leftovers, push, PR) or discards a task. A
// ship waits until the agent has stopped working; a discard stops it.
// The work runs to completion even if the device disconnects.
func (d *Daemon) FinishTask(ctx context.Context, taskID string, f api.TaskFinish, by string) (api.Task, error) {
	if f.Action != "ship" && f.Action != "discard" {
		return api.Task{}, core.Invalid("action must be ship or discard")
	}
	t, err := d.findTask(ctx, taskID)
	if err != nil {
		return api.Task{}, err
	}
	d.tasks.mu.Lock()
	if d.tasks.busy == nil {
		d.tasks.busy = map[string]bool{}
	}
	if d.tasks.busy[t.ID] {
		d.tasks.mu.Unlock()
		return t, core.Invalid("task %s is already being shipped or discarded", t.Title)
	}
	d.tasks.busy[t.ID] = true
	d.tasks.mu.Unlock()
	defer func() {
		d.tasks.mu.Lock()
		delete(d.tasks.busy, t.ID)
		d.tasks.mu.Unlock()
	}()

	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), taskFinishWait)
	defer cancel()
	m := d.taskManager()
	var out api.Task
	switch f.Action {
	case "ship":
		if t.SessionID != "" {
			if s, err := d.db.Session(ctx, t.SessionID); err == nil && s.ExitedAt == nil &&
				(s.State == api.StateWorking || s.State == api.StateStarting) {
				return t, core.Invalid("the agent is still working — ship once it has finished, or discard the task")
			}
		}
		out, err = m.Ship(fctx, t.ID)
	case "discard":
		out, err = m.Abandon(fctx, t.ID, task.AbandonOptions{Force: true})
	}
	if err != nil {
		d.log.Warn("task "+f.Action+" failed", "task", t.ID, "by", by, "err", err)
		return out, taskErr(err)
	}
	d.log.Info("task finished", "task", t.ID, "action", f.Action, "by", by, "status", out.Status, "pr", out.PRURL)
	return out, nil
}

// ---- review notifications ----

// watchTasks follows session events and keeps open tasks' status in step
// with their agent: working → running, waiting/exited → review. Entering
// review sends "ready for review" (with a link to the task) when the
// person is away, in place of the generic "finished its turn".
func (d *Daemon) watchTasks() {
	for d.ctx.Err() == nil {
		after, err := d.db.MaxSeq(d.ctx)
		if err != nil {
			if !sleepCtx(d.ctx, time.Second) {
				return
			}
			continue
		}
		d.syncTaskStatuses(d.ctx) // catch up on anything missed while not subscribed
		sub, err := d.Subscribe(d.ctx, after)
		if err != nil || sub.Resync {
			if sub != nil {
				sub.Close()
			}
			if !sleepCtx(d.ctx, time.Second) {
				return
			}
			continue
		}
		for ev := range sub.C {
			if ev.Type != api.EventSessionUpdated && ev.Type != api.EventSessionExited {
				continue
			}
			var s api.Session
			if json.Unmarshal(ev.Data, &s) != nil || s.TaskID == "" {
				continue
			}
			d.taskSessionChanged(d.ctx, s, true)
		}
		sub.Close() // fell behind, or the daemon is stopping
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// syncTaskStatuses brings open tasks in line with their sessions, without
// notifying.
func (d *Daemon) syncTaskStatuses(ctx context.Context) {
	ts, err := d.db.Tasks(ctx, d.now().Add(-snapshotTaskWindow))
	if err != nil {
		return
	}
	for _, t := range ts {
		if (t.Status != api.TaskRunning && t.Status != api.TaskReview) || t.SessionID == "" {
			continue
		}
		if s, err := d.db.Session(ctx, t.SessionID); err == nil {
			d.taskSessionChanged(ctx, s, false)
		}
	}
}

// taskSessionChanged moves the session's task between running and review.
func (d *Daemon) taskSessionChanged(ctx context.Context, s api.Session, notifyReview bool) {
	want := task.StatusFor(s.State)
	var (
		t       api.Task
		changed bool
	)
	err := d.db.Write(ctx, func(tx *store.Tx) error {
		cur, err := tx.Task(ctx, s.TaskID)
		if err != nil || (cur.Status != api.TaskRunning && cur.Status != api.TaskReview) || cur.Status == want {
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			return err
		}
		cur.Status, cur.UpdatedAt = want, tx.Now()
		if err := tx.PutTask(ctx, cur); err != nil {
			return err
		}
		t, changed = cur, true
		_, err = tx.Emit(ctx, api.EventTaskUpdated, s.ID, "", cur)
		return err
	})
	if err != nil {
		if ctx.Err() == nil {
			d.log.Warn("task status", "task", s.TaskID, "err", err)
		}
		return
	}
	if !changed {
		return
	}
	if _, _, err := task.SetOpenStatus(t.ID, want, t.UpdatedAt); err != nil {
		d.log.Warn("task status in tasks.json", "task", t.ID, "err", err)
	}
	if want == api.TaskReview && notifyReview && d.Presence().State == api.PresenceAway {
		body := "The agent finished — review the changes, then ship or discard"
		if s.LastMessage != "" {
			body = notify.Short(s.LastMessage, 160)
		}
		d.notifyTask(t, &s, core.NotifyWaiting, "Ready for review", body, false)
	}
}

// notifyTask sends a notification about a task that opens it in the web
// app (/t/<id>). always: also when the person is at the desk (desktop
// only then); otherwise only when away, like "finished its turn".
func (d *Daemon) notifyTask(t api.Task, s *api.Session, kind core.NotificationKind, what, body string, always bool) {
	away := d.Presence().State == api.PresenceAway
	if !away && !always {
		return
	}
	d.notify(core.Notification{
		Kind:    kind,
		Title:   fmt.Sprintf("%s · %s", what, filepath.Base(t.Repo)),
		Body:    t.Title + ": " + body,
		URL:     "/t/" + t.ID,
		Session: s,
		Remote:  away,
	})
}
