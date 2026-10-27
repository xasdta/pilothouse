package daemon

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/client"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/paths"
	"github.com/xasdta/pilothouse/internal/policy"
	"github.com/xasdta/pilothouse/internal/task"
	"github.com/xasdta/pilothouse/internal/web"
)

// taskEnv isolates git (no user config, hooks or signing) and puts fake
// `claude` and `codex`-less agents first on PATH. It returns the bin dir.
func taskEnv(t *testing.T) string {
	t.Helper()
	gitcfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(gitcfg, []byte("[user]\n\tname = Test\n\temail = test@example.com\n[init]\n\tdefaultBranch = main\n[commit]\n\tgpgsign = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", gitcfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	bin := t.TempDir()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(git, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	// The fake claude does what a real one would: edit, commit, leave work.
	writeExec(t, filepath.Join(bin, "claude"), "#!/bin/sh\necho \"$1\" >> README.md\ngit commit -q -am agent\necho wip >> README.md\necho new > notes.txt\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/bin:/bin")
	return bin
}

func writeExec(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newGitRepo(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "README.md")
	git(t, dir, "commit", "-q", "-m", "init")
	dir, _ = filepath.EvalSymlinks(dir)
	claudeTrusts(t, dir)
	return dir
}

// claudeTrusts records dir as a folder Claude Code was trusted in, in the
// test user home's ~/.claude.json, so its tasks don't stop at Claude's
// trust question.
func claudeTrusts(t *testing.T, dir string) {
	t.Helper()
	file := filepath.Join(paths.UserHome(), ".claude.json")
	cfg := map[string]map[string]map[string]bool{"projects": {}}
	if b, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
	cfg["projects"][dir] = map[string]bool{"hasTrustDialogAccepted": true}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(file, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeStarter stands in for the holder: it runs the agent to completion in
// the worktree (like a real agent's first turn) and registers a wrapped
// session with the daemon, which tests then drive with hooks.
type fakeStarter struct {
	h     *harness
	mu    sync.Mutex
	specs []task.StartSpec
	n     atomic.Int32
	fail  error
}

func (f *fakeStarter) start(ctx context.Context, s task.StartSpec) (string, error) {
	f.mu.Lock()
	f.specs = append(f.specs, s)
	f.mu.Unlock()
	if f.fail != nil {
		return "", f.fail
	}
	cmd := exec.CommandContext(ctx, s.Argv[0], s.Argv[1:]...)
	cmd.Dir = s.Dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("fake agent: %v: %s", err, out)
	}
	n := f.n.Add(1)
	sid := fmt.Sprintf("s_task%06d", n)
	_, err := f.h.d.RegisterWrapped(ctx, api.WrappedSessionBody{SessionID: sid, Agent: fakeKind, Cwd: s.Dir,
		PID: 7000 + int(n), TaskID: s.TaskID, Title: s.Name})
	return sid, err
}

func (f *fakeStarter) started() []task.StartSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]task.StartSpec(nil), f.specs...)
}

func newTaskHarness(t *testing.T) (*harness, *fakeStarter) {
	t.Helper()
	h := newHarness(t)
	taskEnv(t)
	fs := &fakeStarter{h: h}
	h.opts = func(o *Options) { o.TaskStarter = fs.start }
	return h, fs
}

// sessionIn records a session in dir, as an agent's first hook would.
func (h *harness) sessionIn(t *testing.T, dir, vendorID string) {
	t.Helper()
	h.hook(bg, t, adapters.HookEvent{Kind: adapters.HookSessionStart, Cwd: dir, AgentSessionID: vendorID})
}

func TestReposFromSessionsAndConfig(t *testing.T) {
	h, _ := newTaskHarness(t)
	older := newGitRepo(t, "older")
	newer := newGitRepo(t, "newer")
	configured := newGitRepo(t, "configured")
	git(t, newer, "checkout", "-q", "-b", "feature")
	gone := newGitRepo(t, "gone")
	notRepo := t.TempDir()
	wt := filepath.Join(t.TempDir(), "linked")
	git(t, older, "worktree", "add", "-q", "-b", "side", wt)
	h.cfg.Tasks.Repos = []string{configured, notRepo, "relative/ignored"}
	h.start(t)

	h.sessionIn(t, gone, "v-gone")
	h.sessionIn(t, filepath.Join(older, "."), "v-older")
	h.sessionIn(t, wt, "v-linked") // a linked worktree belongs to its repo
	time.Sleep(5 * time.Millisecond)
	h.sessionIn(t, newer, "v-newer")
	h.sessionIn(t, notRepo, "v-plain")
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}

	repos, err := h.c.Repos(bg)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range repos {
		got = append(got, r.Name+"@"+r.Branch)
	}
	if strings.Join(got, ",") != "newer@feature,older@main,configured@main" {
		t.Fatalf("repos %v (%+v)", got, repos)
	}
	if repos[0].Root != newer || repos[0].LastUsed.IsZero() || !repos[2].LastUsed.IsZero() {
		t.Fatalf("repos %+v", repos)
	}
}

// TestStartTaskValidation (security): only known repos, supported and
// installed agents, bounded prompts and sane bases; nothing is created on
// a refusal.
func TestStartTaskValidation(t *testing.T) {
	h, fs := newTaskHarness(t)
	known := newGitRepo(t, "known")
	other := newGitRepo(t, "other")
	h.cfg.Tasks.Repos = []string{known}
	h.start(t)

	for _, c := range []struct {
		ts   api.TaskStart
		want string
	}{
		{api.TaskStart{Repo: other, Prompt: "x"}, "not a repository Pilothouse knows"},
		{api.TaskStart{Repo: known + "/../other", Prompt: "x"}, "not a repository Pilothouse knows"},
		{api.TaskStart{Repo: "known", Prompt: "x"}, "absolute path"},
		{api.TaskStart{Repo: "", Prompt: "x"}, "absolute path"},
		{api.TaskStart{Repo: known, Prompt: "   "}, "needs a prompt"},
		{api.TaskStart{Repo: known, Prompt: strings.Repeat("p", task.MaxPrompt+1)}, "at most 32 KiB"},
		{api.TaskStart{Repo: known, Prompt: "a\x00b"}, "prompt must be text"},
		{api.TaskStart{Repo: known, Prompt: "x", Agent: "generic"}, "can't be started as a task"},
		{api.TaskStart{Repo: known, Prompt: "x", Agent: "sh"}, "can't be started as a task"},
		{api.TaskStart{Repo: known, Prompt: "x", Agent: api.AgentCodex}, "Codex is not installed"},
		{api.TaskStart{Repo: known, Prompt: "x", Base: "--upload-pack=touch /tmp/pwned"}, "must not start with -"},
		{api.TaskStart{Repo: known, Prompt: "x", Base: "no-such-branch"}, "is not a commit"},
		{api.TaskStart{Repo: known, Prompt: "x", Name: strings.Repeat("n", 101)}, "at most 100"},
	} {
		_, err := h.c.StartTask(bg, c.ts)
		if !client.IsCode(err, "invalid") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+.60v: got %v, want invalid %q", c.ts, err, c.want)
		}
	}
	if len(fs.started()) != 0 {
		t.Fatalf("an agent started: %+v", fs.started())
	}
	if ts, _ := task.Load(); len(ts) != 0 {
		t.Fatalf("tasks recorded: %+v", ts)
	}
	if wt := git(t, known, "worktree", "list"); strings.Count(wt, "\n") != 0 {
		t.Fatalf("worktree created: %s", wt)
	}
	// Starting and finishing change what an agent does: refused from an agent's process tree.
	h.rejectCallers(errors.New("called from inside an agent"))
	if _, err := h.c.StartTask(bg, api.TaskStart{Repo: known, Prompt: "x"}); !client.IsCode(err, "agent_caller") {
		t.Fatalf("start under the guard: %v", err)
	}
	if _, err := h.c.FinishTask(bg, "t_x", api.TaskFinish{Action: "discard"}); !client.IsCode(err, "agent_caller") {
		t.Fatalf("finish under the guard: %v", err)
	}
}

func TestStartTaskLocalDiffDiscard(t *testing.T) {
	h, fs := newTaskHarness(t)
	repo := newGitRepo(t, "app")
	h.start(t)
	h.sessionIn(t, repo, "v-app")

	tk, err := h.c.StartTask(bg, api.TaskStart{Repo: repo, Prompt: "Add a greeting"})
	if err != nil {
		t.Fatal(err)
	}
	if tk.By != "cli" || tk.Agent != api.AgentClaude || tk.SessionID != "s_task000001" || tk.Repo != repo ||
		tk.Branch != "pilothouse/add-a-greeting" || tk.BaseRef != "main" || tk.Status != api.TaskRunning {
		t.Fatalf("task %+v", tk)
	}
	specs := fs.started()
	if len(specs) != 1 || filepath.Base(specs[0].Argv[0]) != "claude" || specs[0].Argv[1] != "Add a greeting" || specs[0].Dir != tk.Worktree {
		t.Fatalf("started %+v", specs)
	}
	// Recorded where the CLI looks too.
	local, _ := task.Load()
	if len(local) != 1 || local[0].ID != tk.ID || local[0].SessionID != tk.SessionID || local[0].By != "cli" {
		t.Fatalf("tasks.json %+v", local)
	}
	ts, err := h.c.Tasks(bg)
	if err != nil || len(ts) != 1 || ts[0].ID != tk.ID {
		t.Fatalf("tasks %+v %v", ts, err)
	}

	d, err := h.c.TaskDiff(bg, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range d.Files {
		paths = append(paths, fmt.Sprintf("%s:%s:+%d", f.Path, f.Op, f.Added))
	}
	if strings.Join(paths, ",") != "README.md:edit:+2,notes.txt:create:+1" || !strings.Contains(d.Files[0].Diff, "+Add a greeting") {
		t.Fatalf("diff %v %+v", paths, d)
	}
	if _, err := h.c.TaskDiff(bg, "t_nope"); !client.IsCode(err, "not_found") {
		t.Fatalf("diff of unknown task: %v", err)
	}
	if _, err := h.c.FinishTask(bg, tk.ID, api.TaskFinish{Action: "merge"}); !client.IsCode(err, "invalid") {
		t.Fatalf("bad action: %v", err)
	}

	out, err := h.c.FinishTask(bg, tk.ID, api.TaskFinish{Action: "discard"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != api.TaskAbandon || !strings.Contains(out.Note, "Deleted branch pilothouse/add-a-greeting") {
		t.Fatalf("discarded %+v", out)
	}
	if _, err := os.Stat(tk.Worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree left: %v", err)
	}
	if b := git(t, repo, "branch", "--list", tk.Branch); b != "" {
		t.Fatalf("branch left: %q", b)
	}
	if st, _ := h.c.Task(bg, tk.ID); st.Status != api.TaskAbandon {
		t.Fatalf("daemon record %+v", st)
	}
	if _, err := h.c.FinishTask(bg, tk.ID, api.TaskFinish{Action: "discard"}); !client.IsCode(err, "invalid") || !strings.Contains(err.Error(), "already abandoned") {
		t.Fatalf("second discard: %v", err)
	}
}

// TestTaskCloseNothingToShip: a review task that changed no files can be
// closed: the agent stops, the worktree goes and the branch stays.
func TestTaskCloseNothingToShip(t *testing.T) {
	h, _ := newTaskHarness(t)
	repo := newGitRepo(t, "app")
	h.cfg.Tasks.Repos = []string{repo}
	h.start(t)
	// This task's agent changes nothing.
	bin := t.TempDir()
	writeExec(t, filepath.Join(bin, "claude"), "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	tk, err := h.c.StartTask(bg, api.TaskStart{Repo: repo, Prompt: "say hello"})
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"PILOTHOUSE_SESSION_ID": tk.SessionID}
	if _, err := h.hookEnv(bg, shell("ls"), env); err != nil {
		t.Fatal(err)
	}
	if _, err := h.hookEnv(bg, adapters.HookEvent{Kind: adapters.HookStop, Message: "Hello."}, env); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "review", func() bool {
		st, _ := h.c.Task(bg, tk.ID)
		return st.Status == api.TaskReview
	})
	diff, err := h.c.TaskDiff(bg, tk.ID)
	if err != nil || len(diff.Files) != 0 {
		t.Fatalf("diff %+v %v", diff, err)
	}

	out, err := h.c.FinishTask(bg, tk.ID, api.TaskFinish{Action: "close"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != api.TaskDone || out.PRURL != "" || !strings.Contains(out.Note, "No changes to ship") {
		t.Fatalf("closed %+v", out)
	}
	if _, err := os.Stat(tk.Worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree left: %v", err)
	}
	if b := git(t, repo, "branch", "--list", tk.Branch); b == "" {
		t.Fatal("branch removed")
	}
	if _, err := h.c.FinishTask(bg, tk.ID, api.TaskFinish{Action: "close"}); !client.IsCode(err, "invalid") || !strings.Contains(err.Error(), "not ready for review") {
		t.Fatalf("second close: %v", err)
	}
}

// TestTaskReadyForReview: a task's agent finishing its turn moves the task
// to review and sends one "ready for review" notification that links to
// the task (and no generic "finished"). It is remote while away and
// desktop-only at the desk.
func TestTaskReadyForReview(t *testing.T) {
	h, _ := newTaskHarness(t)
	repo := newGitRepo(t, "app")
	h.cfg.Tasks.Repos = []string{repo}
	h.presence.setAway(true)
	h.start(t)
	waitFor(t, "away", func() bool { return h.d.Presence().State == api.PresenceAway })

	tk, err := h.c.StartTask(bg, api.TaskStart{Repo: repo, Prompt: "review me"})
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"PILOTHOUSE_SESSION_ID": tk.SessionID}
	// Freshly started, idle at its input box: not ready for review yet.
	if err := h.c.ReportOSC(bg, tk.SessionID, api.OSCBody{State: "idle", App: "claude"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "idle", func() bool { s, _ := h.c.Session(bg, tk.SessionID); return s.State == api.StateWaiting })
	time.Sleep(100 * time.Millisecond)
	if st, _ := h.c.Task(bg, tk.ID); st.Status != api.TaskRunning || len(h.notes.all()) != 0 {
		t.Fatalf("idle before any work: status %s, notifications %+v", st.Status, h.notes.all())
	}
	if ts, _ := h.c.Tasks(bg); ts[0].Status != api.TaskRunning {
		t.Fatalf("listed as %s before any work", ts[0].Status)
	}
	if _, err := h.hookEnv(bg, shell("go test ./..."), env); err != nil {
		t.Fatal(err)
	}
	if _, err := h.hookEnv(bg, adapters.HookEvent{Kind: adapters.HookStop, Message: "All tests pass."}, env); err != nil {
		t.Fatal(err)
	}
	var n core.Notification
	waitFor(t, "ready for review", func() bool {
		for _, x := range h.notes.all() {
			if x.URL == "/t/"+tk.ID {
				n = x
				return true
			}
		}
		return false
	})
	if n.Kind != core.NotifyWaiting || !n.Remote || n.Title != "Ready for review · app" || n.Body != "review-me: All tests pass." || n.Session == nil {
		t.Fatalf("notification %+v", n)
	}
	for _, x := range h.notes.all() {
		if x.Kind == core.NotifyWaiting && x.URL == "" {
			t.Fatalf("generic waiting notification for a task session: %+v", x)
		}
	}
	waitFor(t, "review in tasks.json", func() bool {
		ts, _ := task.Load()
		return len(ts) == 1 && ts[0].Status == api.TaskReview
	})
	if st, _ := h.c.Task(bg, tk.ID); st.Status != api.TaskReview {
		t.Fatalf("db status %s", st.Status)
	}
	// Working again (a reply): running, no new notification.
	before := len(h.notes.all())
	if _, err := h.hookEnv(bg, shell("ls"), env); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "running again", func() bool {
		st, _ := h.c.Task(bg, tk.ID)
		return st.Status == api.TaskRunning
	})
	if len(h.notes.all()) != before {
		t.Fatalf("notified on running: %+v", h.notes.all()[before:])
	}
	// A task finished elsewhere is never moved back by its session.
	if _, err := h.c.FinishTask(bg, tk.ID, api.TaskFinish{Action: "discard"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.hookEnv(bg, adapters.HookEvent{Kind: adapters.HookStop, Message: "again"}, env); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if st, _ := h.c.Task(bg, tk.ID); st.Status != api.TaskAbandon {
		t.Fatalf("status after discard %s", st.Status)
	}
	if ts, _ := task.Load(); ts[0].Status != api.TaskAbandon {
		t.Fatalf("tasks.json after discard %s", ts[0].Status)
	}

	// At the desk: status changes and the desktop notification still fires.
	h.presence.setAway(false)
	waitFor(t, "here", func() bool { return h.d.Presence().State == api.PresencePresent })
	tk2, err := h.c.StartTask(bg, api.TaskStart{Repo: repo, Prompt: "at the desk"})
	if err != nil {
		t.Fatal(err)
	}
	before = len(h.notes.all())
	env2 := map[string]string{"PILOTHOUSE_SESSION_ID": tk2.SessionID}
	if _, err := h.hookEnv(bg, shell("ls"), env2); err != nil {
		t.Fatal(err)
	}
	if _, err := h.hookEnv(bg, adapters.HookEvent{Kind: adapters.HookStop, Message: "Desk result."}, env2); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "review at the desk", func() bool {
		st, _ := h.c.Task(bg, tk2.ID)
		return st.Status == api.TaskReview
	})
	var desk core.Notification
	waitFor(t, "desk notification", func() bool {
		for _, x := range h.notes.all()[before:] {
			if x.URL == "/t/"+tk2.ID {
				desk = x
				return true
			}
		}
		return false
	})
	if desk.Kind != core.NotifyWaiting || desk.Remote || desk.Title != "Ready for review · app" || desk.Body != "at-the-desk: Desk result." || desk.Session == nil {
		t.Fatalf("notified at the desk: %+v", desk)
	}
}

// TestClaudeTrustPromptNotified: a Claude task in a folder Claude was
// never trusted in waits at Claude's question; the session shows it and
// the person is told, with a link to the task.
func TestClaudeTrustPromptNotified(t *testing.T) {
	h, _ := newTaskHarness(t)
	repo := newGitRepo(t, "fresh")
	if err := os.Remove(filepath.Join(paths.UserHome(), ".claude.json")); err != nil {
		t.Fatal(err)
	}
	h.cfg.Tasks.Repos = []string{repo}
	h.start(t)
	tk, err := h.c.StartTask(bg, api.TaskStart{Repo: repo, Prompt: "untrusted folder"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tk.Note, "trust this folder") {
		t.Fatalf("note %q", tk.Note)
	}
	s, err := h.c.Session(bg, tk.SessionID)
	if err != nil || s.State != api.StateBlocked || s.BlockKind != api.BlockQuestion {
		t.Fatalf("session %+v %v", s, err)
	}
	var found bool
	for _, n := range h.notes.all() {
		if n.Kind == core.NotifyBlocked && n.URL == "/t/"+tk.ID && strings.Contains(n.Body, "trust this folder") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no trust notification: %+v", h.notes.all())
	}
	if _, err := os.Stat(filepath.Join(paths.UserHome(), ".claude.json")); !os.IsNotExist(err) {
		t.Fatal("Claude's config was written")
	}
}

// agentAdapter is a fake adapter for a real agent kind, with a settable
// binary and hook status.
type agentAdapter struct {
	fakeAdapter
	kind      api.AgentKind
	binary    atomic.Value // string
	installed atomic.Bool
}

func (a *agentAdapter) Kind() api.AgentKind    { return a.kind }
func (a *agentAdapter) ProcessNames() []string { return []string{string(a.kind)} }
func (a *agentAdapter) Detect() adapters.Detection {
	return adapters.Detection{Binary: a.binary.Load().(string)}
}
func (a *agentAdapter) Status(adapters.InstallOptions) (adapters.HookStatus, error) {
	return adapters.HookStatus{Installed: a.installed.Load()}, nil
}

// TestStartOpenCodeTask: OpenCode is found where its installer puts it
// (not on PATH), starts only once its Pilothouse plugin is installed, and
// gets the prompt with --prompt (a bare argument is a project path).
func TestStartOpenCodeTask(t *testing.T) {
	h, fs := newTaskHarness(t)
	ocDir := t.TempDir() // like ~/.opencode/bin: not on the daemon's PATH
	bin := filepath.Join(ocDir, "opencode")
	oc := &agentAdapter{fakeAdapter: h.adapter, kind: api.AgentOpenCode}
	oc.binary.Store(bin)
	writeExec(t, bin, "#!/bin/sh\nexit 0\n")
	withStarter := h.opts
	h.opts = func(o *Options) {
		withStarter(o)
		o.Adapters = []adapters.Adapter{h.adapter, oc}
	}
	repo := newGitRepo(t, "app")
	h.cfg.Tasks.Repos = []string{repo}
	h.start(t)

	start := api.TaskStart{Repo: repo, Agent: api.AgentOpenCode, Prompt: "-v is broken; fix it"}
	if _, err := h.c.StartTask(bg, start); !client.IsCode(err, "invalid") || !strings.Contains(err.Error(), "Pilothouse is not connected to OpenCode") {
		t.Fatalf("plugin not installed: %v", err)
	}
	oc.installed.Store(true)
	tk, err := h.c.StartTask(bg, start)
	if err != nil {
		t.Fatal(err)
	}
	specs := fs.started()
	if tk.Agent != api.AgentOpenCode || len(specs) != 1 || len(specs[0].Argv) != 2 ||
		specs[0].Argv[0] != bin || specs[0].Argv[1] != "--prompt=-v is broken; fix it" {
		t.Fatalf("task %+v, started %+v", tk, specs)
	}

	oc.binary.Store("")
	if _, err := h.c.StartTask(bg, start); !client.IsCode(err, "invalid") || !strings.Contains(err.Error(), "is not installed") {
		t.Fatalf("no binary: %v", err)
	}
}

func trustIn(t *testing.T, repo string) {
	t.Helper()
	if _, err := policy.TrustRepo(paths.TrustedRepos(), repo); err != nil {
		t.Fatal(err)
	}
}

func writeTaskYAML(t *testing.T, repo, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repo, ".pilothouse"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, task.ConfigFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestStartTaskSetupInBackground: setup commands run after StartTask
// returns; the agent starts once they succeed, and a failure marks the
// task failed and tells the person.
func TestStartTaskSetupInBackground(t *testing.T) {
	h, fs := newTaskHarness(t)
	repo := newGitRepo(t, "app")
	gate := filepath.Join(t.TempDir(), "gate")
	writeTaskYAML(t, repo, "setup:\n  - 'while [ ! -e "+gate+" ]; do sleep 0.02; done; echo deps > deps.txt'\n")
	trustIn(t, repo)
	h.cfg.Tasks.Repos = []string{repo}
	h.start(t)

	tk, err := h.c.StartTask(bg, api.TaskStart{Repo: repo, Prompt: "with setup"})
	if err != nil {
		t.Fatal(err)
	}
	if tk.SessionID != "" || tk.Setup == nil || tk.Setup.Status != api.SetupRunning || tk.Status != api.TaskRunning || len(fs.started()) != 0 {
		t.Fatalf("returned %+v (setup %+v)", tk, tk.Setup)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the agent after setup", func() bool {
		st, err := h.c.Task(bg, tk.ID)
		return err == nil && st.SessionID != "" && st.Setup != nil && st.Setup.Status == api.SetupOK
	})
	if b, err := os.ReadFile(filepath.Join(tk.Worktree, "deps.txt")); err != nil || string(b) != "deps\n" {
		t.Fatalf("setup output file %q %v", b, err)
	}
	if len(fs.started()) != 1 {
		t.Fatalf("starts %d", len(fs.started()))
	}

	writeTaskYAML(t, repo, "setup: ['echo nope >&2; exit 4']\n")
	tk, err = h.c.StartTask(bg, api.TaskStart{Repo: repo, Prompt: "broken setup"})
	if err != nil {
		t.Fatal(err)
	}
	var st api.Task
	waitFor(t, "failed task", func() bool {
		st, _ = h.c.Task(bg, tk.ID)
		return st.Status == api.TaskFailed
	})
	if st.Setup.Status != api.SetupFailed || !strings.Contains(st.Note, "exited with status 4") || st.SessionID != "" {
		t.Fatalf("failed task %+v", st)
	}
	waitFor(t, "a failure notification", func() bool { return h.notes.find(core.NotifyBlocked, false) })
	for _, n := range h.notes.all() {
		if n.Kind == core.NotifyBlocked && (n.URL != "/t/"+tk.ID || !strings.HasPrefix(n.Title, "Task didn't start")) {
			t.Fatalf("notification %+v", n)
		}
		if strings.Contains(n.Body, "nope") {
			t.Fatalf("setup output in a notification: %+v", n)
		}
	}
	if len(fs.started()) != 1 {
		t.Fatal("the agent started after a failed setup")
	}
	if _, err := h.c.FinishTask(bg, tk.ID, api.TaskFinish{Action: "ship"}); !client.IsCode(err, "invalid") || !strings.Contains(err.Error(), "never started") {
		t.Fatalf("ship a failed task: %v", err)
	}
	if out, err := h.c.FinishTask(bg, tk.ID, api.TaskFinish{Action: "discard"}); err != nil || out.Status != api.TaskAbandon {
		t.Fatalf("discard a failed task: %+v %v", out, err)
	}
}

// TestUntrustedSetupNotRunFromDevice (security): a repo's task.yaml can't
// make the daemon run commands unless the person trusts the repo.
func TestUntrustedSetupNotRunFromDevice(t *testing.T) {
	h, fs := newTaskHarness(t)
	repo := newGitRepo(t, "app")
	marker := filepath.Join(t.TempDir(), "pwned")
	writeTaskYAML(t, repo, "setup: ['touch "+marker+"']\nenv: {NODE_OPTIONS: '--require ./x.js'}\n")
	h.cfg.Tasks.Repos = []string{repo}
	h.start(t)
	tk, err := h.c.StartTask(bg, api.TaskStart{Repo: repo, Prompt: "untrusted"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("setup ran in an untrusted repo")
	}
	if tk.Setup.Status != api.SetupSkipped || !strings.Contains(tk.Note, "not trusted") || tk.SessionID == "" {
		t.Fatalf("task %+v", tk)
	}
	if specs := fs.started(); len(specs) != 1 || specs[0].Env != nil {
		t.Fatalf("agent env %+v", specs)
	}
}

func TestShipFromDaemon(t *testing.T) {
	h, _ := newTaskHarness(t)
	bin := taskEnv(t)
	repo := newGitRepo(t, "app")
	origin := filepath.Join(t.TempDir(), "origin.git")
	git(t, repo, "init", "-q", "--bare", origin)
	git(t, repo, "remote", "add", "origin", origin)
	git(t, repo, "push", "-q", "-u", "origin", "main")
	writeExec(t, filepath.Join(bin, "gh"), "#!/bin/sh\necho https://github.com/x/app/pull/3\n")
	h.cfg.Tasks.Repos = []string{repo}
	h.start(t)

	tk, err := h.c.StartTask(bg, api.TaskStart{Repo: repo, Prompt: "ship me"})
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"PILOTHOUSE_SESSION_ID": tk.SessionID}
	if _, err := h.hookEnv(bg, shell("make"), env); err != nil {
		t.Fatal(err)
	}
	if _, err := h.c.FinishTask(bg, tk.ID, api.TaskFinish{Action: "ship"}); !client.IsCode(err, "invalid") || !strings.Contains(err.Error(), "still working") {
		t.Fatalf("ship while working: %v", err)
	}
	if _, err := h.hookEnv(bg, adapters.HookEvent{Kind: adapters.HookStop}, env); err != nil {
		t.Fatal(err)
	}
	out, err := h.c.FinishTask(bg, tk.ID, api.TaskFinish{Action: "ship"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != api.TaskDone || out.PRURL != "https://github.com/x/app/pull/3" {
		t.Fatalf("shipped %+v", out)
	}
	if files := git(t, origin, "log", "--name-only", "--format=", "refs/heads/"+tk.Branch); !strings.Contains(files, "notes.txt") {
		t.Fatalf("leftovers not pushed: %s", files)
	}
	if st, _ := h.c.Task(bg, tk.ID); st.Status != api.TaskDone || st.PRURL == "" {
		t.Fatalf("daemon record %+v", st)
	}
}

func TestTasksMergesLocalRecord(t *testing.T) {
	h, _ := newTaskHarness(t)
	h.start(t)
	now := time.Now().UTC()
	// Made by the CLI while the daemon was down: only in tasks.json.
	offline := api.Task{ID: "t_offline001", Title: "offline", Status: api.TaskDone, CreatedAt: now.Add(-time.Hour), UpdatedAt: now}
	if err := task.Save(offline); err != nil {
		t.Fatal(err)
	}
	// Finished by the CLI while the daemon was down: the newer local record wins.
	stale := api.Task{ID: "t_stale00001", Title: "stale", Status: api.TaskRunning, CreatedAt: now.Add(-2 * time.Hour)}
	if _, err := h.c.PutTask(bg, stale); err != nil {
		t.Fatal(err)
	}
	stale.Status, stale.UpdatedAt = api.TaskDone, time.Now().UTC().Add(time.Minute)
	if err := task.Save(stale); err != nil {
		t.Fatal(err)
	}
	ts, err := h.c.Tasks(bg)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 || ts[0].ID != offline.ID || ts[1].ID != stale.ID || ts[1].Status != api.TaskDone {
		t.Fatalf("tasks %+v", ts)
	}
}

// ---- through the device API ----

type phone struct {
	t      *testing.T
	base   string
	cookie string
	key    *ecdsa.PrivateKey
	n      int
}

// pairPhone pairs a simulated device over the web listener, like
// tests/e2e/claude/phone.mjs.
func pairPhone(t *testing.T, h *harness) *phone {
	t.Helper()
	ps, err := h.c.Pair(bg)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	p := &phone{t: t, base: h.d.WebURL(), key: key}
	resp, body := p.post("/api/pair", map[string]string{"code": ps.Code, "name": "test phone", "public_key": base64.RawURLEncoding.EncodeToString(der)})
	if resp.StatusCode != 200 {
		t.Fatalf("pair: %d %s", resp.StatusCode, body)
	}
	p.cookie = strings.SplitN(resp.Header.Get("Set-Cookie"), ";", 2)[0]
	return p
}

func (p *phone) do(method, path string, body any) (*http.Response, []byte) {
	p.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, p.base+path, r)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", p.base)
	req.Header.Set("X-Pilothouse", "1")
	if p.cookie != "" {
		req.Header.Set("Cookie", p.cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (p *phone) post(path string, body any) (*http.Response, []byte) { return p.do("POST", path, body) }

// signed is a signed action body (web.CanonicalAction, P1363 signature).
func (p *phone) signed(action, target string, payload any) map[string]any {
	p.t.Helper()
	pb, _ := json.Marshal(payload)
	p.n++
	ts := time.Now().UnixMilli()
	nonce := fmt.Sprintf("taskphone-nonce-%08d", p.n)
	h := sha256.Sum256([]byte(web.CanonicalAction(action, target, string(pb), ts, nonce)))
	r, s, err := ecdsa.Sign(rand.Reader, p.key, h[:])
	if err != nil {
		p.t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return map[string]any{"payload": string(pb), "ts": ts, "nonce": nonce, "sig": base64.RawURLEncoding.EncodeToString(sig)}
}

func bodyErrCode(b []byte) string {
	var e api.Error
	_ = json.Unmarshal(b, &e)
	return e.Code
}

func TestTasksThroughSignedDeviceAPI(t *testing.T) {
	h, fs := newTaskHarness(t)
	repo := newGitRepo(t, "app")
	h.cfg.Tasks.Repos = []string{repo}
	// The web app checks the Host port against web.addr, so use a real one.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h.cfg.Web.Addr = ln.Addr().String()
	ln.Close()
	h.start(t)
	p := pairPhone(t, h)

	resp, body := p.do("GET", "/api/repos", nil)
	var repos []api.RepoInfo
	if resp.StatusCode != 200 || json.Unmarshal(body, &repos) != nil || len(repos) != 1 || repos[0].Root != repo {
		t.Fatalf("repos: %d %s", resp.StatusCode, body)
	}

	start := api.TaskStart{Repo: repo, Agent: api.AgentClaude, Prompt: "Fix the login bug"}
	// Unsigned, signed for another target, or by no device: refused before anything starts.
	if resp, body := p.post("/api/tasks", start); resp.StatusCode != 400 {
		t.Fatalf("unsigned: %d %s", resp.StatusCode, body)
	}
	if resp, body := p.post("/api/tasks", p.signed(web.ActionTask, "t_other", start)); resp.StatusCode != 403 || bodyErrCode(body) != "bad_signature" {
		t.Fatalf("wrong target: %d %s", resp.StatusCode, body)
	}
	if resp, body := p.post("/api/tasks", p.signed(web.ActionReply, "new", start)); resp.StatusCode != 403 {
		t.Fatalf("wrong action: %d %s", resp.StatusCode, body)
	}
	anon := *p
	anon.cookie = ""
	if resp, _ := anon.post("/api/tasks", p.signed(web.ActionTask, "new", start)); resp.StatusCode != 401 {
		t.Fatalf("no cookie: %d", resp.StatusCode)
	}
	if resp, body := p.post("/api/tasks", p.signed(web.ActionTask, "new", api.TaskStart{Repo: t.TempDir(), Prompt: "x"})); resp.StatusCode != 400 || bodyErrCode(body) != "invalid" ||
		!strings.Contains(string(body), "not a repository Pilothouse knows") {
		t.Fatalf("unknown repo: %d %s", resp.StatusCode, body)
	}
	if len(fs.started()) != 0 {
		t.Fatal("an agent started")
	}

	good := p.signed(web.ActionTask, "new", start)
	resp, body = p.post("/api/tasks", good)
	var tk api.Task
	if resp.StatusCode != 200 || json.Unmarshal(body, &tk) != nil {
		t.Fatalf("start: %d %s", resp.StatusCode, body)
	}
	if !strings.HasPrefix(tk.By, "device:d_") || tk.SessionID == "" || tk.Prompt != "Fix the login bug" {
		t.Fatalf("task %+v", tk)
	}
	if resp, body := p.post("/api/tasks", good); resp.StatusCode != 403 || bodyErrCode(body) != "replayed" {
		t.Fatalf("replay: %d %s", resp.StatusCode, body)
	}
	if n := len(fs.started()); n != 1 {
		t.Fatalf("%d agents started", n)
	}

	resp, body = p.do("GET", "/api/tasks", nil)
	var ts []api.Task
	if resp.StatusCode != 200 || json.Unmarshal(body, &ts) != nil || len(ts) != 1 || ts[0].By != tk.By {
		t.Fatalf("tasks: %d %s", resp.StatusCode, body)
	}
	resp, body = p.do("GET", "/api/tasks/"+tk.ID+"/diff", nil)
	var d api.TaskDiff
	if resp.StatusCode != 200 || json.Unmarshal(body, &d) != nil || len(d.Files) != 2 || d.Added != 3 {
		t.Fatalf("diff: %d %s", resp.StatusCode, body)
	}

	// Finish: the signature binds the task id.
	if resp, _ := p.post("/api/tasks/"+tk.ID+"/finish", p.signed(web.ActionTask, "new", api.TaskFinish{Action: "discard"})); resp.StatusCode != 403 {
		t.Fatalf("finish signed for another target: %d", resp.StatusCode)
	}
	resp, body = p.post("/api/tasks/"+tk.ID+"/finish", p.signed(web.ActionTask, tk.ID, api.TaskFinish{Action: "discard"}))
	var done api.Task
	if resp.StatusCode != 200 || json.Unmarshal(body, &done) != nil || done.Status != api.TaskAbandon {
		t.Fatalf("discard: %d %s", resp.StatusCode, body)
	}
}
