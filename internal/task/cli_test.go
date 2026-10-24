package task

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/wrap"
)

// TestCLIEndToEnd drives `pilothouse task …` through the real binary: the
// agent runs under a detached holder, with no daemon.
func TestCLIEndToEnd(t *testing.T) {
	testEnv(t)
	t.Cleanup(func() {
		hs, _ := wrap.ListHolders(context.Background())
		for _, h := range hs {
			_ = syscall.Kill(-h.PID, syscall.SIGKILL)
			_ = syscall.Kill(h.HolderPID, syscall.SIGKILL)
		}
	})
	_, file, _, _ := runtime.Caller(0)
	bin := filepath.Join(t.TempDir(), "pilothouse")
	build := exec.Command("go", "build", "-o", bin, "./cmd/pilothouse")
	build.Dir = filepath.Join(filepath.Dir(file), "..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	repo := newRepo(t)
	agent := fakeAgentScript(t, true)

	pilothouse := func(args ...string) (string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	mustRun := func(args ...string) string {
		t.Helper()
		out, err := pilothouse(args...)
		if err != nil {
			t.Fatalf("pilothouse %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}

	out := mustRun("task", "new", "--agent", agent, "Document the API")
	id := regexp.MustCompile(`task (t_[a-z2-7]{10}) \(document-the-api\) started`).FindStringSubmatch(out)
	if id == nil || !strings.Contains(out, "pilothouse attach s_") {
		t.Fatalf("task new output:\n%s", out)
	}
	wt := filepath.Join(os.Getenv("PILOTHOUSE_USER_HOME"), ".pilothouse-worktrees", "my-repo", "document-the-api")

	// The agent commits and exits; the task becomes ready for review.
	deadline := time.Now().Add(15 * time.Second)
	for {
		out = mustRun("task", "ls")
		if strings.Contains(out, "review") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task never reached review:\n%s", out)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(out, id[1]) || !strings.Contains(out, wt) || !regexp.MustCompile(`(?m)^ID +TASK +STATUS +BY +SESSION`).MatchString(out) ||
		!regexp.MustCompile(`review +cli +s_`).MatchString(out) {
		t.Fatalf("task ls:\n%s", out)
	}
	if out = mustRun("task", "diff", "document-the-api", "--stat"); !strings.Contains(out, "README.md") {
		t.Fatalf("task diff --stat:\n%s", out)
	}
	if out = mustRun("task", "diff", id[1]); !strings.Contains(out, "+task: Document the API") {
		t.Fatalf("task diff:\n%s", out)
	}
	if out = mustRun("task", "done", id[1], "--keep"); !strings.Contains(out, "done; branch pilothouse/document-the-api kept") {
		t.Fatalf("task done:\n%s", out)
	}
	if out = mustRun("task", "ls"); !strings.Contains(out, "no open tasks") {
		t.Fatalf("task ls after done:\n%s", out)
	}
	if out = mustRun("task", "ls", "--all"); !strings.Contains(out, "done") {
		t.Fatalf("task ls --all:\n%s", out)
	}

	// A running agent blocks abandon until --force.
	sleeper := filepath.Join(t.TempDir(), "sleeper")
	if err := os.WriteFile(sleeper, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustRun("task", "new", "--agent", sleeper, "--name", "slow one", "wait")
	if out, err := pilothouse("task", "abandon", "slow-one"); err == nil || !strings.Contains(out, "still running") {
		t.Fatalf("abandon while running: %v\n%s", err, out)
	}
	if out = mustRun("task", "abandon", "--force", "slow-one"); !strings.Contains(out, "stopping the agent") || !strings.Contains(out, "abandoned") {
		t.Fatalf("abandon --force:\n%s", out)
	}
	hs, _ := wrap.ListHolders(context.Background())
	for _, h := range hs {
		if h.Name == "slow-one" {
			t.Fatalf("agent still running after abandon --force: %+v", h)
		}
	}
	if out, err := pilothouse("task", "diff", "t_zzzz"); err == nil || !strings.Contains(out, "no task matches") {
		t.Fatalf("diff unknown: %v\n%s", err, out)
	}
}
