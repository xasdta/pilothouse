package task

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/paths"
	"github.com/xasdta/pilothouse/internal/policy"
)

func writeFile(t *testing.T, p, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func writeTaskYAML(t *testing.T, repo, body string) {
	t.Helper()
	writeFile(t, filepath.Join(repo, ConfigFile), body, 0o644)
}

func TestConfigValidation(t *testing.T) {
	good, err := parseConfig("task.yaml", []byte("version: 1\ncopy: [.env, '.env.*', config/local/*.json]\nsetup: [npm ci]\nenv: {PORT: '3001'}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if good.SetupTimeout.Duration != DefaultSetupTimeout || len(good.Copy) != 3 || good.envList()[0] != "PORT=3001" {
		t.Fatalf("config %+v", good)
	}
	if c, err := parseConfig("task.yaml", nil); err != nil || c == nil {
		t.Fatalf("empty file: %v %v", c, err)
	}
	for body, want := range map[string]string{
		"version: 2\n":                                   "unsupported version",
		"copy: [../secrets]\n":                           "no ..",
		"copy: [a/../../b]\n":                            "no ..",
		"copy: [/etc/passwd]\n":                          "relative to the repository",
		"copy: ['~/.ssh/id_rsa']\n":                      "relative to the repository",
		"copy: [.git/config]\n":                          ".git",
		"copy: ['**/.env']\n":                            "** is not supported",
		"copy: [config/]\n":                              "clean relative path",
		"copy: [./.env]\n":                               "clean relative path",
		"copy: ['[']\n":                                  "not a valid glob",
		"setup: ['  ']\n":                                "empty",
		"setup_timeout: 5h\n":                            "setup_timeout",
		"env: {PILOTHOUSE_HOME: /tmp}\n":                 "reserved",
		"env: {'BAD-NAME': x}\n":                         "not a valid variable name",
		"cleanup: [rm -rf /]\n":                          "field cleanup not found",
		"copy: .env\n":                                   "cannot unmarshal",
		"setup_timeout: soon\n":                          "invalid duration",
		"copy: [\"" + strings.Repeat("a", 300) + "\"]\n": "longer than",
	} {
		if _, err := parseConfig("task.yaml", []byte(body)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", body, err, want)
		}
	}
}

func TestLoadConfigFileChecks(t *testing.T) {
	repo := t.TempDir()
	if c, err := LoadConfig(repo); c != nil || err != nil {
		t.Fatalf("no file: %v %v", c, err)
	}
	// A symlinked task.yaml is refused (it could point anywhere).
	outside := filepath.Join(t.TempDir(), "elsewhere.yaml")
	writeFile(t, outside, "setup: [touch pwned]\n", 0o644)
	if err := os.MkdirAll(filepath.Join(repo, ".pilothouse"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, ConfigFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(repo); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked task.yaml: %v", err)
	}
	// A FIFO must not hang the reader.
	_ = os.Remove(filepath.Join(repo, ConfigFile))
	if err := syscall.Mkfifo(filepath.Join(repo, ConfigFile), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := LoadConfig(repo); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("fifo: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("LoadConfig hung on a FIFO")
	}
	_ = os.Remove(filepath.Join(repo, ConfigFile))
	writeTaskYAML(t, repo, strings.Repeat("# padding\n", 8000))
	if _, err := LoadConfig(repo); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("huge file: %v", err)
	}
}

// TestCopyFilesConfined: copy: never leaves the repository, never writes
// through symlinks or over the checkout's files, skips what isn't a
// regular file, and keeps permissions.
func TestCopyFilesConfined(t *testing.T) {
	src, _ := filepath.EvalSymlinks(t.TempDir())
	dst, _ := filepath.EvalSymlinks(t.TempDir())
	secret := filepath.Join(t.TempDir(), "id_rsa")
	writeFile(t, secret, "PRIVATE KEY", 0o600)
	outsideDir := t.TempDir()
	writeFile(t, filepath.Join(outsideDir, "leak.json"), "outside", 0o644)

	writeFile(t, filepath.Join(src, ".env"), "TOKEN=abc\n", 0o600)
	writeFile(t, filepath.Join(src, ".env.local"), "DEBUG=1\n", 0o644)
	writeFile(t, filepath.Join(src, "config/local/a.json"), "{}", 0o644)
	writeFile(t, filepath.Join(src, "certs/dev/ca.pem"), "CA", 0o644)
	writeFile(t, filepath.Join(src, "README.md"), "main checkout edit", 0o644)
	writeFile(t, filepath.Join(src, ".git/config"), "[core]", 0o644)
	writeFile(t, filepath.Join(src, "real.txt"), "real", 0o644)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Symlink(secret, filepath.Join(src, ".env.ssh")))                           // file outside the repo
	must(os.Symlink(filepath.Join(src, "real.txt"), filepath.Join(src, ".env.alias"))) // file inside
	must(os.Symlink(outsideDir, filepath.Join(src, "config/linked")))                  // dir outside
	must(os.Symlink(filepath.Join(src, ".git", "config"), filepath.Join(src, ".env.git")))
	must(syscall.Mkfifo(filepath.Join(src, ".env.fifo"), 0o644))
	// The worktree already has README.md (tracked) and a symlinked dir.
	writeFile(t, filepath.Join(dst, "README.md"), "tracked", 0o644)
	trap := t.TempDir()
	must(os.Symlink(trap, filepath.Join(dst, "certs")))

	res, err := copyFiles(src, dst, []string{".env*", "config/*/*.json", "certs", "README.md", "*/config", "missing.txt"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(res.Copied, ",")
	if got != ".env,.env.alias,.env.local,config/local/a.json" {
		t.Fatalf("copied %q; skipped %q", got, res.Skipped)
	}
	skipped := strings.Join(res.Skipped, "\n")
	for _, want := range []string{
		".env.ssh: symlink to a file outside the repository",
		".env.git: symlink to a file outside the repository",
		".env.fifo: not a regular file",
		"config/linked: symlinked directory, not followed",
		"README.md: already in the worktree",
		"certs/dev/ca.pem: certs in the worktree is not a directory",
	} {
		if !strings.Contains(skipped, want) {
			t.Errorf("skipped lacks %q:\n%s", want, skipped)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "README.md")); string(b) != "tracked" {
		t.Errorf("tracked file overwritten: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, ".env.alias")); string(b) != "real" {
		t.Errorf("in-repo symlink copied as %q", b)
	}
	if fi, err := os.Lstat(filepath.Join(dst, ".env")); err != nil || fi.Mode().Perm() != 0o600 || !fi.Mode().IsRegular() {
		t.Errorf(".env: %v %v", fi, err)
	}
	for _, p := range []string{".env.ssh", ".git", "config/linked", "leak.json"} {
		if _, err := os.Lstat(filepath.Join(dst, p)); err == nil {
			t.Errorf("%s was copied", p)
		}
	}
	if ents, _ := os.ReadDir(trap); len(ents) != 0 {
		t.Errorf("wrote through the worktree's symlink: %v", ents)
	}
}

func TestCopyFilesLimits(t *testing.T) {
	src, _ := filepath.EvalSymlinks(t.TempDir())
	dst, _ := filepath.EvalSymlinks(t.TempDir())
	for i := 0; i < maxCopyFiles+5; i++ {
		writeFile(t, filepath.Join(src, "gen", "f"+strconv.Itoa(i)), "x", 0o644)
	}
	res, err := copyFiles(src, dst, []string{"gen"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Copied) != maxCopyFiles || !strings.Contains(strings.Join(res.Skipped, "\n"), "copy limit reached") {
		t.Fatalf("copied %d, skipped %v", len(res.Copied), res.Skipped)
	}
}

func trustRepo(t *testing.T, repo string) {
	t.Helper()
	if _, err := policy.TrustRepo(paths.TrustedRepos(), repo); err != nil {
		t.Fatal(err)
	}
}

// recStarter records what the agent would be started with.
type recStarter struct{ specs []StartSpec }

func (r *recStarter) start(_ context.Context, s StartSpec) (string, error) {
	r.specs = append(r.specs, s)
	return "s_recorded01", nil
}

// TestUntrustedSetupNotRun (security): an untrusted repo's setup commands
// and env never run; the files are still copied and the person is told,
// with the commands shown.
func TestUntrustedSetupNotRun(t *testing.T) {
	testEnv(t)
	repo := newRepo(t)
	writeFile(t, filepath.Join(repo, ".env"), "TOKEN=abc\n", 0o600)
	writeTaskYAML(t, repo, "copy: [.env]\nsetup: ['touch \"$PILOTHOUSE_TASK_REPO/pwned\"', 'echo hi']\nenv: {NODE_OPTIONS: '--require ./evil.js'}\n")
	rs := &recStarter{}
	var out bytes.Buffer
	m := &Manager{Start: rs.start, Out: &out, Err: &out}
	tk, err := m.New(context.Background(), NewOptions{Prompt: "untrusted", Agent: "true", Dir: repo, By: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "pwned")); err == nil {
		t.Fatal("an untrusted repo's setup command ran")
	}
	if tk.Setup == nil || tk.Setup.Status != api.SetupSkipped || tk.Setup.Commands != 2 || strings.Join(tk.Setup.Copied, ",") != ".env" {
		t.Fatalf("setup %+v", tk.Setup)
	}
	if !strings.Contains(tk.Note, "not trusted") || !strings.Contains(tk.Note, "pilothouse policy trust") || tk.By != "cli" {
		t.Fatalf("task %+v", tk)
	}
	for _, want := range []string{"were not run", `touch "$PILOTHOUSE_TASK_REPO/pwned"`, "echo hi", "pilothouse policy trust " + repo, "env from .pilothouse/task.yaml not applied", "copied 1 file"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if len(rs.specs) != 1 || rs.specs[0].Env != nil {
		t.Fatalf("agent env from an untrusted repo: %+v", rs.specs)
	}
	if b, _ := os.ReadFile(filepath.Join(tk.Worktree, ".env")); string(b) != "TOKEN=abc\n" {
		t.Fatalf(".env not copied: %q", b)
	}
	if _, err := os.Stat(LogPath(tk.ID)); err == nil {
		t.Error("a task log was written although nothing ran")
	}
}

func TestTrustedSetupRunsBeforeAgent(t *testing.T) {
	testEnv(t)
	repo := newRepo(t)
	trustRepo(t, repo)
	writeTaskYAML(t, repo, `setup:
  - echo "setup out $GREETING" && echo "setup err" >&2
  - printf '%s|%s' "$PILOTHOUSE_TASK_ID" "$PWD" > setup-ran.txt
env: {GREETING: hello}
`)
	rs := &recStarter{}
	var out, term bytes.Buffer
	m := &Manager{Start: rs.start, Out: &out, Err: &out, SetupOut: &term}
	tk, err := m.New(context.Background(), NewOptions{Prompt: "trusted", Agent: "true", Dir: repo})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if tk.Setup.Status != api.SetupOK || tk.Setup.Log != LogPath(tk.ID) || tk.Status != api.TaskRunning || tk.SessionID != "s_recorded01" {
		t.Fatalf("task %+v setup %+v", tk, tk.Setup)
	}
	b, err := os.ReadFile(filepath.Join(tk.Worktree, "setup-ran.txt"))
	realWT, _ := filepath.EvalSymlinks(tk.Worktree)
	if err != nil || string(b) != tk.ID+"|"+realWT {
		t.Fatalf("setup did not run in the worktree: %q %v", b, err)
	}
	logb, _ := os.ReadFile(LogPath(tk.ID))
	for _, want := range []string{"setup out hello", "setup err", "$ printf"} {
		if !strings.Contains(string(logb), want) || !strings.Contains(term.String(), want) {
			t.Errorf("log/terminal lacks %q:\nlog: %s\nterm: %s", want, logb, term.String())
		}
	}
	if fi, _ := os.Stat(LogPath(tk.ID)); fi.Mode().Perm() != 0o600 {
		t.Errorf("task log mode %v", fi.Mode())
	}
	env := strings.Join(rs.specs[0].Env, "\n")
	if !strings.Contains(env, "GREETING=hello") {
		t.Errorf("agent env lacks task.yaml env")
	}
	if ts, _ := loadTasks(); len(ts) != 1 || ts[0].Setup.Status != api.SetupOK {
		t.Errorf("stored %+v", ts)
	}
}

func TestSetupFailureKeepsFailedTask(t *testing.T) {
	testEnv(t)
	repo := newRepo(t)
	trustRepo(t, repo)
	writeTaskYAML(t, repo, "setup: ['echo first', 'echo broken >&2; exit 3', 'touch never']\n")
	rs := &recStarter{}
	m := &Manager{Start: rs.start, Out: io.Discard, Err: io.Discard}
	tk, err := m.New(context.Background(), NewOptions{Prompt: "fails", Agent: "true", Dir: repo})
	if err == nil || !strings.Contains(err.Error(), "setup command 2 of 3") || !strings.Contains(err.Error(), "exited with status 3") ||
		!strings.Contains(err.Error(), "pilothouse task abandon "+tk.ID) {
		t.Fatalf("err %v", err)
	}
	if len(rs.specs) != 0 {
		t.Fatal("the agent started after a failed setup")
	}
	if tk.Status != api.TaskFailed || tk.Setup.Status != api.SetupFailed || !strings.Contains(tk.Note, "exited with status 3") {
		t.Fatalf("task %+v", tk)
	}
	if _, err := os.Stat(filepath.Join(tk.Worktree, "never")); err == nil {
		t.Fatal("setup continued after a failure")
	}
	if !dirExists(tk.Worktree) {
		t.Fatal("worktree removed")
	}
	stored, _ := m.Get(tk.ID)
	if stored.Status != api.TaskFailed {
		t.Fatalf("stored status %s", stored.Status)
	}
	if _, err := m.Abandon(context.Background(), tk.ID, AbandonOptions{}); err != nil || dirExists(tk.Worktree) {
		t.Fatalf("abandon failed task: %v", err)
	}
}

// TestSetupTimeoutKillsProcessGroup: a setup command that outlives
// setup_timeout is killed with everything it started.
func TestSetupTimeoutKillsProcessGroup(t *testing.T) {
	testEnv(t)
	repo := newRepo(t)
	trustRepo(t, repo)
	pidFile := filepath.Join(t.TempDir(), "bg.pid")
	writeTaskYAML(t, repo, "setup_timeout: 1s\nsetup: ['sleep 60 & echo $! > "+pidFile+"; wait']\n")
	m := &Manager{Start: (&recStarter{}).start, Out: io.Discard, Err: io.Discard}
	start := time.Now()
	_, err := m.New(context.Background(), NewOptions{Prompt: "slow", Agent: "true", Dir: repo})
	if err == nil || !strings.Contains(err.Error(), "did not finish within setup_timeout (1s)") {
		t.Fatalf("err %v", err)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("timeout took %s", d)
	}
	b, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 0 {
		t.Fatalf("no pid: %q", b)
	}
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("the setup command's background process survived the timeout")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBrokenTaskYAMLRefusesStart(t *testing.T) {
	testEnv(t)
	repo := newRepo(t)
	writeTaskYAML(t, repo, "copy: [../../etc/passwd]\n")
	m, _ := newManager(t)
	_, err := m.New(context.Background(), NewOptions{Prompt: "nope", Agent: "true", Dir: repo})
	if err == nil || !strings.Contains(err.Error(), "must stay inside the repository") || !strings.Contains(err.Error(), "fix it or move it aside") {
		t.Fatalf("err %v", err)
	}
	if wt := mustGit(t, repo, "worktree", "list", "--porcelain"); strings.Count(wt, "worktree ") != 1 {
		t.Fatalf("worktree created: %s", wt)
	}
}

func TestPromptAndBaseChecks(t *testing.T) {
	testEnv(t)
	repo := newRepo(t)
	mustGit(t, repo, "commit", "-q", "--allow-empty", "-m", "second")
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "claude"), "#!/bin/sh\nexit 0\n", 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	writeClaudeConfig(t, paths.UserHome(), map[string]bool{repo: true})
	rs := &recStarter{}
	m := &Manager{Start: rs.start, Out: io.Discard, Err: io.Discard, Daemon: nopDaemon{}}
	ctx := context.Background()

	// A prompt that looks like an option reaches the agent as the prompt.
	if _, err := m.New(ctx, NewOptions{Prompt: "- fix a\n- fix b", Agent: "claude", Dir: repo}); err != nil {
		t.Fatal(err)
	}
	if a := rs.specs[0].Argv; len(a) != 3 || a[1] != "--" || a[2] != "- fix a\n- fix b" {
		t.Fatalf("argv %q", a)
	}
	if _, err := m.New(ctx, NewOptions{Prompt: "plain", Agent: "claude", Dir: repo}); err != nil {
		t.Fatal(err)
	}
	if a := rs.specs[1].Argv; len(a) != 2 || a[1] != "plain" {
		t.Fatalf("argv %q", a)
	}
	// OpenCode reads a bare argument as a project path: the prompt goes in --prompt.
	writeFile(t, filepath.Join(bin, "opencode"), "#!/bin/sh\nexit 0\n", 0o755)
	oc, err := m.New(ctx, NewOptions{Prompt: "- fix the build", Agent: "opencode", Dir: repo})
	if err != nil {
		t.Fatal(err)
	}
	if a := rs.specs[2].Argv; oc.Agent != api.AgentOpenCode || len(a) != 2 || filepath.Base(a[0]) != "opencode" || a[1] != "--prompt=- fix the build" {
		t.Fatalf("opencode: agent %s argv %q", oc.Agent, a)
	}
	if a := AgentArgv(api.AgentGeneric, "/bin/mytool", "-x"); len(a) != 2 || a[1] != "-x" {
		t.Fatalf("generic argv %q", a)
	}
	// Revision syntax is pinned to the commit it means now.
	tk, err := m.New(ctx, NewOptions{Prompt: "older", Agent: "true", Dir: repo, Base: "HEAD~1"})
	if err != nil {
		t.Fatal(err)
	}
	if want := mustGit(t, repo, "rev-parse", "HEAD~1"); tk.BaseRef != want {
		t.Fatalf("base %q, want %q", tk.BaseRef, want)
	}
	for _, c := range []struct {
		o    NewOptions
		want string
	}{
		{NewOptions{Prompt: "x", Agent: "true", Dir: repo, Base: "--upload-pack=touch /tmp/x"}, "must not start with -"},
		{NewOptions{Prompt: "x", Agent: "true", Dir: repo, Base: "main..HEAD"}, "not a branch, tag or commit"},
		{NewOptions{Prompt: "x\x00y", Agent: "true", Dir: repo}, "prompt must be text"},
		{NewOptions{Prompt: strings.Repeat("p", MaxPrompt+1), Agent: "true", Dir: repo}, "prompt must be text"},
		{NewOptions{Prompt: "x", Agent: "true", Dir: repo, Name: strings.Repeat("n", 101)}, "task name"},
	} {
		if _, err := m.New(ctx, c.o); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: got %v, want %q", c.o.Base+c.o.Name, err, c.want)
		}
	}
}

// TestCopiedFilesStayLocal: task.yaml's copies are not the agent's work:
// `task diff` doesn't print them and `task done` doesn't see them as
// uncommitted changes.
func TestCopiedFilesStayLocal(t *testing.T) {
	testEnv(t)
	repo := newRepo(t)
	writeFile(t, filepath.Join(repo, ".env"), "TOKEN=hunter2\n", 0o600)
	writeTaskYAML(t, repo, "copy: [.env]\n")
	m, _ := newManager(t)
	ctx := context.Background()
	tk, err := m.New(ctx, NewOptions{Prompt: "local copies", Agent: fakeAgentScript(t, true), Dir: repo})
	if err != nil {
		t.Fatal(err)
	}
	var d bytes.Buffer
	if err := m.Diff(ctx, tk.ID, false, &d); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.String(), "hunter2") || !strings.Contains(d.String(), "+task: local copies") {
		t.Fatalf("diff:\n%s", d.String())
	}
	if _, err := m.Done(ctx, tk.ID, DoneOptions{}); err != nil {
		t.Fatalf("done with only copies left: %v", err)
	}
	if dirExists(tk.Worktree) {
		t.Fatal("worktree kept")
	}
}

// nopDaemon is a DaemonAPI that records nothing.
type nopDaemon struct{}

func (nopDaemon) PutTask(_ context.Context, t api.Task) (api.Task, error) { return t, nil }
func (nopDaemon) ReportOSC(context.Context, string, api.OSCBody) error    { return nil }
func (nopDaemon) Sessions(context.Context) ([]api.Session, error)         { return nil, nil }
