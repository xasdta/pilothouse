package task

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/paths"
)

// testEnv isolates Pilothouse paths and git configuration (no user hooks,
// signing or templates) for one test.
func testEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PILOTHOUSE_HOME", t.TempDir())
	t.Setenv("PILOTHOUSE_USER_HOME", t.TempDir())
	run, err := os.MkdirTemp("/tmp", "ag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(run) })
	t.Setenv("PILOTHOUSE_RUN_DIR", run)
	gitcfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(gitcfg, []byte("[user]\n\tname = Test\n\temail = test@example.com\n[init]\n\tdefaultBranch = main\n[commit]\n\tgpgsign = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", gitcfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitOutput(context.Background(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// newRepo creates a repository with one commit on main.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "My Repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", "README.md")
	mustGit(t, dir, "commit", "-q", "-m", "init")
	dir, _ = filepath.EvalSymlinks(dir)
	return dir
}

// fakeAgentScript writes an "agent" that commits one change and leaves one
// uncommitted edit and one untracked file, like a real agent mid-task.
func fakeAgentScript(t *testing.T, commitOnly bool) string {
	t.Helper()
	script := "#!/bin/sh\nset -e\n" +
		`echo "task: $1" >> README.md` + "\n" +
		"git add README.md\ngit commit -q -m \"agent: $1\"\n"
	if !commitOnly {
		script += "echo wip >> README.md\necho new > notes.txt\n"
	}
	p := filepath.Join(t.TempDir(), "fake-agent")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// syncStarter runs the agent to completion in the worktree instead of
// spawning a holder, so task tests don't depend on the wrap package.
func syncStarter(t *testing.T) Starter {
	return func(ctx context.Context, s StartSpec) (string, error) {
		cmd := exec.CommandContext(ctx, s.Argv[0], s.Argv[1:]...)
		cmd.Dir = s.Dir
		cmd.Env = append(os.Environ(), "PILOTHOUSE_TASK_ID="+s.TaskID)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fake agent: %v\n%s", err, out)
		}
		return "s_fakesessn", nil
	}
}

func newManager(t *testing.T) (*Manager, *bytes.Buffer) {
	var out bytes.Buffer
	return &Manager{Start: syncStarter(t), Out: &out, Err: &out}, &out
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"Fix the login bug":                                  "fix-the-login-bug",
		"  Don't break   CI!! ":                              "dont-break-ci",
		"add a --json flag to the export command please now": "add-a-json-flag-to-the",
		"Ünïcode → stays ascii":                              "n-code-stays-ascii",
		"":                                                   "task",
		"!!!":                                                "task",
		strings.Repeat("a", 60):                              strings.Repeat("a", 40),
		"refactor internal/store/sqlite.go for WAL mode": "refactor-internal-store-sqlite-go-for",
	} {
		got := Slugify(in)
		if got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
		if len(got) > maxSlugLen || strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") || strings.Contains(got, "--") {
			t.Errorf("Slugify(%q) = %q is not a clean slug", in, got)
		}
	}
}

func TestNewDiffDone(t *testing.T) {
	testEnv(t)
	repo := newRepo(t)
	agent := fakeAgentScript(t, false)
	m, _ := newManager(t)
	ctx := context.Background()

	tk, err := m.New(ctx, NewOptions{Prompt: "Add a greeting!", Agent: agent, Dir: repo})
	if err != nil {
		t.Fatal(err)
	}
	// Worktrees live outside the protected Pilothouse home (H2).
	wantWT := filepath.Join(paths.UserHome(), ".pilothouse-worktrees", "my-repo", "add-a-greeting")
	if tk.Title != "add-a-greeting" || tk.Branch != "pilothouse/add-a-greeting" || tk.Worktree != wantWT ||
		tk.BaseRef != "main" || tk.Repo != repo || tk.SessionID != "s_fakesessn" || tk.Status != api.TaskRunning ||
		!strings.HasPrefix(tk.ID, "t_") || tk.Agent != api.AgentGeneric {
		t.Fatalf("task %+v", tk)
	}
	// The user's checkout is untouched: same branch, clean, same commit.
	if st := mustGit(t, repo, "status", "--porcelain"); st != "" {
		t.Fatalf("main worktree dirty: %q", st)
	}
	if b := mustGit(t, repo, "symbolic-ref", "--short", "HEAD"); b != "main" {
		t.Fatalf("main worktree switched to %s", b)
	}
	if n := mustGit(t, repo, "rev-list", "--count", "main"); n != "1" {
		t.Fatalf("main moved: %s commits", n)
	}

	// Same prompt again: a unique slug.
	tk2, err := m.New(ctx, NewOptions{Prompt: "Add a greeting!", Agent: agent, Dir: repo})
	if err != nil {
		t.Fatal(err)
	}
	if tk2.Title != "add-a-greeting-2" {
		t.Fatalf("second slug %q", tk2.Title)
	}
	// From inside a task's worktree, the task still belongs to the main repo.
	tk3, err := m.New(ctx, NewOptions{Prompt: "nested", Name: "Custom Name", Agent: agent, Dir: tk.Worktree, Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if tk3.Repo != repo || tk3.Title != "custom-name" {
		t.Fatalf("nested task %+v", tk3)
	}

	// Diff: the commit, the uncommitted edit and the untracked file.
	var d bytes.Buffer
	if err := m.Diff(ctx, tk.ID, false, &d); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"+task: Add a greeting!", "+wip", "notes.txt", "+new"} {
		if !strings.Contains(d.String(), want) {
			t.Errorf("diff lacks %q:\n%s", want, d.String())
		}
	}
	d.Reset()
	if err := m.Diff(ctx, tk.Title, true, &d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.String(), "README.md") || !strings.Contains(d.String(), "notes.txt | new file (untracked)") {
		t.Errorf("diffstat:\n%s", d.String())
	}

	// Done refuses a dirty worktree.
	if _, err := m.Done(ctx, tk.ID, DoneOptions{}); err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("done on dirty worktree: %v", err)
	}
	// Commit the rest, then done --keep.
	mustGit(t, tk.Worktree, "add", "-A")
	mustGit(t, tk.Worktree, "commit", "-q", "-m", "rest")
	done, err := m.Done(ctx, tk.ID[:6], DoneOptions{Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != api.TaskDone {
		t.Fatalf("status %s", done.Status)
	}
	if _, err := os.Stat(tk.Worktree); err != nil {
		t.Fatalf("--keep removed the worktree: %v", err)
	}
	if _, err := m.Done(ctx, tk.ID, DoneOptions{}); err == nil || !strings.Contains(err.Error(), "already done") {
		t.Fatalf("second done: %v", err)
	}
	// Once the worktree is gone, diff falls back to base...branch.
	mustGit(t, repo, "worktree", "remove", tk.Worktree)
	d.Reset()
	if err := m.Diff(ctx, tk.ID, false, &d); err != nil || !strings.Contains(d.String(), "+new") {
		t.Fatalf("diff after removal: %v\n%s", err, d.String())
	}

	// Done without --keep removes the worktree but keeps the branch.
	mustGit(t, tk2.Worktree, "add", "-A")
	mustGit(t, tk2.Worktree, "commit", "-q", "-m", "rest")
	if _, err := m.Done(ctx, tk2.ID, DoneOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tk2.Worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree not removed: %v", err)
	}
	mustGit(t, repo, "show-ref", "--verify", "refs/heads/"+tk2.Branch)

	// Done --force discards uncommitted work.
	if _, err := m.Done(ctx, tk3.ID, DoneOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if st := mustGit(t, repo, "status", "--porcelain"); st != "" {
		t.Fatalf("main worktree dirty at the end: %q", st)
	}
}

func TestAbandon(t *testing.T) {
	testEnv(t)
	repo := newRepo(t)
	m, out := newManager(t)
	ctx := context.Background()
	tk, err := m.New(ctx, NewOptions{Prompt: "throw away", Agent: fakeAgentScript(t, false), Dir: repo})
	if err != nil {
		t.Fatal(err)
	}
	sha := mustGit(t, repo, "rev-parse", "--short", tk.Branch)
	ab, err := m.Abandon(ctx, tk.ID, AbandonOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ab.Status != api.TaskAbandon {
		t.Fatalf("status %s", ab.Status)
	}
	if _, err := os.Stat(tk.Worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
	if gitOK(ctx, repo, "show-ref", "--verify", "--quiet", "refs/heads/"+tk.Branch) {
		t.Fatal("branch not deleted")
	}
	if !strings.Contains(out.String(), "git branch "+tk.Branch+" "+sha) {
		t.Fatalf("no recovery hint: %s", out.String())
	}
	if _, err := m.Abandon(ctx, tk.ID, AbandonOptions{}); err == nil {
		t.Fatal("abandoning twice succeeded")
	}

	// --keep-branch keeps the work reachable.
	tk2, err := m.New(ctx, NewOptions{Prompt: "keep my branch", Agent: fakeAgentScript(t, true), Dir: repo})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Abandon(ctx, tk2.Title, AbandonOptions{KeepBranch: true}); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "show-ref", "--verify", "refs/heads/"+tk2.Branch)
}

func TestDonePR(t *testing.T) {
	testEnv(t)
	repo := newRepo(t)
	origin := filepath.Join(t.TempDir(), "origin.git")
	mustGit(t, repo, "init", "-q", "--bare", origin)
	mustGit(t, repo, "remote", "add", "origin", origin)
	mustGit(t, repo, "push", "-q", "-u", "origin", "main")

	// A fake gh records its arguments.
	bin := t.TempDir()
	ghLog := filepath.Join(t.TempDir(), "gh.log")
	gh := "#!/bin/sh\necho \"$PWD|$*\" > " + ghLog + "\necho https://github.com/x/y/pull/1\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(gh), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	m, out := newManager(t)
	ctx := context.Background()
	tk, err := m.New(ctx, NewOptions{Prompt: "ship it", Agent: fakeAgentScript(t, true), Dir: repo})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Done(ctx, tk.ID, DoneOptions{PR: true}); err != nil {
		t.Fatalf("done --pr: %v\n%s", err, out.String())
	}
	if _, err := gitOutput(ctx, origin, "show-ref", "--verify", "refs/heads/"+tk.Branch); err != nil {
		t.Fatalf("branch not pushed: %v", err)
	}
	logb, _ := os.ReadFile(ghLog)
	if want := repo + "|pr create --fill --head " + tk.Branch + " --base main"; strings.TrimSpace(string(logb)) != want {
		t.Fatalf("gh called with %q, want %q", logb, want)
	}
	if !strings.Contains(out.String(), "pull/1") {
		t.Fatalf("gh output not shown: %s", out.String())
	}

	// No commits beyond base: refused before pushing.
	tk2, err := m.New(ctx, NewOptions{Prompt: "nothing", Agent: "true", Dir: repo})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Done(ctx, tk2.ID, DoneOptions{PR: true}); err == nil || !strings.Contains(err.Error(), "no commits") {
		t.Fatalf("empty PR: %v", err)
	}
}

func TestNewErrors(t *testing.T) {
	testEnv(t)
	repo := newRepo(t)
	m, _ := newManager(t)
	ctx := context.Background()
	cases := []struct {
		o    NewOptions
		want string
	}{
		{NewOptions{Prompt: "x", Agent: "true", Dir: t.TempDir()}, "not inside a git repository"},
		{NewOptions{Prompt: "x", Agent: "true", Dir: repo, Base: "nope"}, `base "nope" is not a commit`},
		{NewOptions{Prompt: "x", Agent: "no-such-agent-xyz", Dir: repo}, "not found on your PATH"},
		{NewOptions{Prompt: "  ", Agent: "true", Dir: repo}, "needs a prompt"},
	}
	for _, c := range cases {
		if _, err := m.New(ctx, c.o); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: got %v, want %q", c.o, err, c.want)
		}
	}
	if _, err := m.Get("t_missing"); err == nil || !strings.Contains(err.Error(), "no task matches") {
		t.Errorf("get missing: %v", err)
	}
}

func TestStartFailureRollsBack(t *testing.T) {
	testEnv(t)
	repo := newRepo(t)
	m := &Manager{Out: io.Discard, Err: io.Discard, Start: func(context.Context, StartSpec) (string, error) {
		return "", errors.New("holder exploded")
	}}
	_, err := m.New(context.Background(), NewOptions{Prompt: "doomed", Agent: "true", Dir: repo})
	if err == nil || !strings.Contains(err.Error(), "holder exploded") {
		t.Fatalf("got %v", err)
	}
	if gitOK(context.Background(), repo, "show-ref", "--verify", "--quiet", "refs/heads/pilothouse/doomed") {
		t.Fatal("branch left behind")
	}
	if wt := mustGit(t, repo, "worktree", "list", "--porcelain"); strings.Contains(wt, "doomed") {
		t.Fatalf("worktree left behind: %s", wt)
	}
	if ts, _ := loadTasks(); len(ts) != 0 {
		t.Fatalf("task record left behind: %+v", ts)
	}
}

// fakeDaemon answers the task and session routes on paths.Socket().
type fakeDaemon struct {
	mu       sync.Mutex
	tasks    []api.Task
	sessions []api.Session
	osc      map[string]api.OSCBody // session id → last OSC report
}

func startFakeDaemon(t *testing.T, sessions []api.Session) *fakeDaemon {
	t.Helper()
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	fd := &fakeDaemon{sessions: sessions, osc: map[string]api.OSCBody{}}
	ln, err := net.Listen("unix", paths.Socket())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fd.mu.Lock()
		defer fd.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/tasks":
			var tk api.Task
			_ = json.NewDecoder(r.Body).Decode(&tk)
			fd.tasks = append(fd.tasks, tk)
			_ = json.NewEncoder(w).Encode(tk)
		case r.Method == "GET" && r.URL.Path == "/v1/sessions":
			_ = json.NewEncoder(w).Encode(fd.sessions)
		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/v1/sessions/") && strings.HasSuffix(r.URL.Path, "/osc"):
			var b api.OSCBody
			_ = json.NewDecoder(r.Body).Decode(&b)
			fd.osc[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/sessions/"), "/osc")] = b
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(404)
			_ = json.NewEncoder(w).Encode(api.Error{Error: "not found", Code: "not_found"})
		}
	}))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return fd
}

func TestDaemonMirrorAndStatus(t *testing.T) {
	testEnv(t)
	repo := newRepo(t)
	ctx := context.Background()
	var n int
	m := &Manager{Out: io.Discard, Err: io.Discard, Start: func(context.Context, StartSpec) (string, error) {
		n++
		return []string{"s_working000", "s_waiting000", "s_exited0000", "s_blocked000"}[n-1], nil
	}}
	fd := startFakeDaemon(t, []api.Session{
		{ID: "s_working000", State: api.StateWorking},
		{ID: "s_waiting000", State: api.StateWaiting},
		{ID: "s_exited0000", State: api.StateExited},
		{ID: "s_blocked000", State: api.StateBlocked},
	})
	var ids []string
	for _, p := range []string{"one", "two", "three", "four"} {
		tk, err := m.New(ctx, NewOptions{Prompt: p, Agent: "true", Dir: repo})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, tk.ID)
	}
	fd.mu.Lock()
	mirrored := len(fd.tasks)
	last := fd.tasks[len(fd.tasks)-1]
	fd.mu.Unlock()
	// Each task is sent before its agent starts and again with its session.
	if mirrored != 8 || last.SessionID != "s_blocked000" {
		t.Fatalf("mirrored %d, last %+v", mirrored, last)
	}
	views, err := m.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]api.TaskStatus{}
	for _, v := range views {
		got[v.ID] = v.Status
	}
	want := []api.TaskStatus{api.TaskRunning, api.TaskReview, api.TaskReview, api.TaskRunning}
	for i, id := range ids {
		if got[id] != want[i] {
			t.Errorf("task %d: status %s, want %s", i, got[id], want[i])
		}
	}
	if views[0].ID != ids[3] {
		t.Errorf("list is not newest first")
	}
}

func TestListWithoutDaemon(t *testing.T) {
	testEnv(t)
	repo := newRepo(t)
	m, _ := newManager(t)
	ctx := context.Background()
	tk, err := m.New(ctx, NewOptions{Prompt: "offline", Agent: "true", Dir: repo})
	if err != nil {
		t.Fatal(err)
	}
	views, err := m.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// No daemon, no holder listening: the agent is gone, so it's ready for review.
	if len(views) != 1 || views[0].ID != tk.ID || views[0].Status != api.TaskReview || views[0].SessionState != api.StateExited {
		t.Fatalf("views %+v", views)
	}
	b, err := os.ReadFile(StorePath())
	if err != nil || !bytes.Contains(b, []byte(tk.ID)) {
		t.Fatalf("local store: %v %s", err, b)
	}
	if fi, _ := os.Stat(StorePath()); fi.Mode().Perm() != 0o600 {
		t.Fatalf("store mode %v", fi.Mode())
	}
}

func TestStoreConcurrentWrites(t *testing.T) {
	testEnv(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := saveTask(api.Task{ID: "t_" + strings.Repeat("x", i+1)}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	ts, err := loadTasks()
	if err != nil || len(ts) != 20 {
		t.Fatalf("got %d tasks, %v", len(ts), err)
	}
}
