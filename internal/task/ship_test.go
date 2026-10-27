package task

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xasdta/pilothouse/internal/api"
)

// withOrigin gives repo a bare origin with main pushed.
func withOrigin(t *testing.T, repo string) string {
	t.Helper()
	origin := filepath.Join(t.TempDir(), "origin.git")
	mustGit(t, repo, "init", "-q", "--bare", origin)
	mustGit(t, repo, "remote", "add", "origin", origin)
	mustGit(t, repo, "push", "-q", "-u", "origin", "main")
	return origin
}

// pathWith puts a directory holding git (and, when gh is non-empty, a fake
// gh with that script) in front of a PATH without any real gh.
func pathWith(t *testing.T, gh string) {
	t.Helper()
	bin := t.TempDir()
	for _, name := range []string{"git", "true", "sh"} {
		p, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(p, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	if gh != "" {
		writeFile(t, filepath.Join(bin, "gh"), gh, 0o755)
	}
	// PATH is only the temp dir: a runner with gh in /usr/bin or /bin must
	// still look like a machine without it.
	t.Setenv("PATH", bin)
	if gh == "" {
		if p, err := exec.LookPath("gh"); err == nil {
			t.Fatalf("test PATH still finds gh at %s", p)
		}
	}
}

func newShipTask(t *testing.T, repo string) (*Manager, api.Task) {
	t.Helper()
	m := &Manager{Start: (&recStarter{}).start, Out: io.Discard, Err: io.Discard, Daemon: nopDaemon{}}
	tk, err := m.New(context.Background(), NewOptions{Prompt: "Add a greeting\n\nand more detail", Agent: "true", Dir: repo})
	if err != nil {
		t.Fatal(err)
	}
	return m, tk
}

func TestShipCommitsPushesAndOpensPR(t *testing.T) {
	testEnv(t)
	repo := newRepo(t)
	origin := withOrigin(t, repo)
	writeFile(t, filepath.Join(repo, ".env"), "TOKEN=hunter2\n", 0o600)
	writeTaskYAML(t, repo, "copy: [.env]\n")
	ghLog := filepath.Join(t.TempDir(), "gh.log")
	pathWith(t, "#!/bin/sh\necho \"$PWD|$*|$GH_PROMPT_DISABLED\" > "+ghLog+"\necho https://github.com/x/y/pull/7\n")
	m, tk := newShipTask(t, repo)
	// The agent committed one change and left another uncommitted, plus a new file.
	writeFile(t, filepath.Join(tk.Worktree, "README.md"), "hello\nagent\n", 0o644)
	mustGit(t, tk.Worktree, "commit", "-q", "-am", "agent: readme")
	writeFile(t, filepath.Join(tk.Worktree, "README.md"), "hello\nagent\nleftover\n", 0o644)
	writeFile(t, filepath.Join(tk.Worktree, "new.txt"), "new\n", 0o644)

	done, err := m.Ship(context.Background(), tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != api.TaskDone || done.PRURL != "https://github.com/x/y/pull/7" || !strings.Contains(done.Note, "opened https://github.com/x/y/pull/7") {
		t.Fatalf("task %+v", done)
	}
	if dirExists(tk.Worktree) {
		t.Error("worktree kept")
	}
	files := mustGit(t, origin, "log", "--name-only", "--format=", "refs/heads/"+tk.Branch)
	if !strings.Contains(files, "new.txt") || strings.Contains(files, ".env") {
		t.Fatalf("pushed files:\n%s", files)
	}
	if subj := mustGit(t, origin, "log", "-1", "--format=%s%n%b", "refs/heads/"+tk.Branch); !strings.HasPrefix(subj, "Add a greeting\n") || !strings.Contains(subj, "Pilothouse-Task: "+tk.ID) {
		t.Fatalf("commit message %q", subj)
	}
	logb, _ := os.ReadFile(ghLog)
	if want := repo + "|pr create --fill --head " + tk.Branch + " --base main|1"; strings.TrimSpace(string(logb)) != want {
		t.Fatalf("gh %q, want %q", logb, want)
	}
	if stored, _ := m.Get(tk.ID); stored.Status != api.TaskDone || stored.PRURL == "" {
		t.Fatalf("stored %+v", stored)
	}
	if _, err := m.Ship(context.Background(), tk.ID); err == nil || !strings.Contains(err.Error(), "already done") {
		t.Fatalf("second ship: %v", err)
	}
}

func TestShipWithoutGhOrOrigin(t *testing.T) {
	testEnv(t)
	repo := newRepo(t)
	pathWith(t, "")
	m, tk := newShipTask(t, repo)
	writeFile(t, filepath.Join(tk.Worktree, "a.txt"), "a\n", 0o644)
	// No origin: committed on the branch, the person is told how to push.
	done, err := m.Ship(context.Background(), tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != api.TaskDone || !strings.Contains(done.Note, "no origin remote") || !strings.Contains(done.Note, tk.Branch) {
		t.Fatalf("task %+v", done)
	}
	if files := mustGit(t, repo, "log", "--name-only", "--format=", tk.Branch); !strings.Contains(files, "a.txt") {
		t.Fatalf("not committed: %s", files)
	}

	// Origin but no gh: pushed, open the PR yourself.
	withOrigin(t, repo)
	m, tk = newShipTask(t, repo)
	writeFile(t, filepath.Join(tk.Worktree, "b.txt"), "b\n", 0o644)
	done, err = m.Ship(context.Background(), tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != api.TaskDone || done.PRURL != "" || !strings.Contains(done.Note, "gh (GitHub CLI) is not installed") {
		t.Fatalf("task %+v", done)
	}
	mustGit(t, repo, "ls-remote", "--exit-code", "origin", "refs/heads/"+tk.Branch)
}

func TestShipRefusals(t *testing.T) {
	testEnv(t)
	repo := newRepo(t)
	pathWith(t, "")
	writeFile(t, filepath.Join(repo, ".env"), "TOKEN=hunter2\n", 0o600)
	writeTaskYAML(t, repo, "copy: [.env]\n")
	ctx := context.Background()

	m, tk := newShipTask(t, repo)
	if _, err := m.Ship(ctx, tk.ID); err == nil || !strings.Contains(err.Error(), "nothing to ship") {
		t.Fatalf("empty: %v", err)
	}
	if !dirExists(tk.Worktree) {
		t.Fatal("refusal removed the worktree")
	}
	// The agent committed the copied .env: never pushed anywhere.
	mustGit(t, tk.Worktree, "add", "-f", ".env")
	mustGit(t, tk.Worktree, "commit", "-q", "-m", "oops")
	if _, err := m.Ship(ctx, tk.ID); err == nil || !strings.Contains(err.Error(), "committed .env") {
		t.Fatalf("copied file committed: %v", err)
	}
	if st, _ := m.Get(tk.ID); st.Status != api.TaskRunning {
		t.Fatalf("status after refusal %s", st.Status)
	}

	// A push that fails leaves the task open, and the message carries no
	// credentials from the remote URL.
	m, tk = newShipTask(t, repo)
	mustGit(t, repo, "remote", "add", "origin", "https://alice:s3cr3t-token@127.0.0.1:1/x/y.git")
	writeFile(t, filepath.Join(tk.Worktree, "c.txt"), "c\n", 0o644)
	got, err := m.Ship(ctx, tk.ID)
	if err == nil || !strings.Contains(err.Error(), "git push failed") || strings.Contains(err.Error(), "s3cr3t") || strings.Contains(got.Note, "s3cr3t") {
		t.Fatalf("push failure: %v / %q", err, got.Note)
	}
	if got.Status == api.TaskDone || !dirExists(tk.Worktree) {
		t.Fatalf("task after failed push %+v", got)
	}
	if files := mustGit(t, repo, "log", "--name-only", "--format=", tk.Branch); !strings.Contains(files, "c.txt") {
		t.Fatalf("leftovers not committed before the push: %s", files)
	}

	// A failed task has nothing to ship.
	failed := tk
	failed.ID, failed.Status = "t_failedtask", api.TaskFailed
	if err := saveTask(failed); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ship(ctx, failed.ID); err == nil || !strings.Contains(err.Error(), "never started") {
		t.Fatalf("failed task: %v", err)
	}
}

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"fatal: unable to access 'https://bob:pw@github.com/x/y.git/'": "fatal: unable to access 'https://***@github.com/x/y.git/'",
		"token ghp_abcdefghijklmnopqrstuvwxyz0123 rejected":            "token *** rejected",
		"ssh://git@github.com/x/y":                                     "ssh://***@github.com/x/y",
	} {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
}
