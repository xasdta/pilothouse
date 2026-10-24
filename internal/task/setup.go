package task

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/paths"
	"github.com/xasdta/pilothouse/internal/policy"
)

// maxTaskLog caps what setup writes to a task's log.
const maxTaskLog = 4 << 20

// LogPath is a task's log: the output of its setup commands.
func LogPath(taskID string) string {
	return filepath.Join(paths.Logs(), "task-"+taskID+".log")
}

// RepoTrusted reports whether repo is on the trust list that
// `pilothouse policy trust` manages (trusted-repos.yaml). Only trusted
// repos get their task.yaml setup commands run and env applied. A list
// that cannot be read trusts nothing.
func RepoTrusted(repo string) (bool, error) {
	repos, err := policy.ReadTrustedRepos(paths.TrustedRepos())
	if err != nil {
		return false, err
	}
	canon, err := policy.CanonicalRepo(repo)
	if err != nil {
		return false, err
	}
	for _, r := range repos {
		if r.Path == canon || filepath.Clean(r.Path) == filepath.Clean(repo) {
			return true, nil
		}
		if c, err := filepath.EvalSymlinks(r.Path); err == nil && c == canon {
			return true, nil
		}
	}
	return false, nil
}

// setupEnv is the environment of setup commands: ours, task.yaml's env,
// and where the task lives.
func setupEnv(t api.Task, cfg *Config) []string {
	env := append(os.Environ(), cfg.envList()...)
	return append(env,
		"PILOTHOUSE_TASK_ID="+t.ID,
		"PILOTHOUSE_TASK_REPO="+t.Repo,
		"PILOTHOUSE_TASK_WORKTREE="+t.Worktree,
	)
}

// runSetup runs cfg's setup commands in the task's worktree, one after
// another with /bin/sh -c and no stdin, all within cfg.SetupTimeout. Their
// output goes to the task log (capped) and to also, when it is not nil. A
// command that fails stops the setup. When the time is up (or ctx ends),
// each command's whole process group is killed.
func runSetup(ctx context.Context, t api.Task, cfg *Config, also io.Writer) error {
	if err := paths.Ensure(); err != nil {
		return err
	}
	f, err := os.OpenFile(LogPath(t.ID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	var w io.Writer = &capWriter{w: f, left: maxTaskLog}
	if also != nil {
		w = io.MultiWriter(w, also)
	}
	w = &syncWriter{w: w}
	fmt.Fprintf(w, "# pilothouse task %s (%s): setup from %s, %s\n", t.ID, t.Title, ConfigFile, time.Now().Format(time.RFC3339))

	tctx, cancel := context.WithTimeout(ctx, cfg.SetupTimeout.Duration)
	defer cancel()
	env := setupEnv(t, cfg)
	for i, c := range cfg.Setup {
		fmt.Fprintf(w, "$ %s\n", c)
		start := time.Now()
		cmd := exec.CommandContext(tctx, "/bin/sh", "-c", c)
		cmd.Dir = t.Worktree
		cmd.Env = env
		cmd.Stdout, cmd.Stderr = w, w // stdin: /dev/null
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		// A background process the command leaves behind may keep the
		// output pipe open; don't wait for it.
		cmd.WaitDelay = 5 * time.Second
		err := cmd.Run()
		if errors.Is(err, exec.ErrWaitDelay) {
			err = nil
		}
		what := fmt.Sprintf("setup command %d of %d (`%s`)", i+1, len(cfg.Setup), Short(c, 60))
		switch {
		case err == nil:
			fmt.Fprintf(w, "# ok (%s)\n", time.Since(start).Round(time.Millisecond))
			continue
		case errors.Is(tctx.Err(), context.DeadlineExceeded):
			err = fmt.Errorf("%s did not finish within setup_timeout (%s)", what, cfg.SetupTimeout.Duration)
		case ctx.Err() != nil:
			err = fmt.Errorf("%s was interrupted", what)
		default:
			var ee *exec.ExitError
			if errors.As(err, &ee) && ee.ExitCode() >= 0 {
				err = fmt.Errorf("%s exited with status %d", what, ee.ExitCode())
			} else {
				err = fmt.Errorf("%s failed: %v", what, err)
			}
		}
		fmt.Fprintf(w, "# %v\n", err)
		return err
	}
	return nil
}

// capWriter writes up to left bytes, then notes the truncation once and
// swallows the rest (a chatty command must not fail because of its log).
type capWriter struct {
	w    io.Writer
	left int64
	cut  bool
}

func (c *capWriter) Write(p []byte) (int, error) {
	if c.left <= 0 {
		if !c.cut {
			c.cut = true
			_, _ = io.WriteString(c.w, "\n[pilothouse: output truncated]\n")
		}
		return len(p), nil
	}
	n := int64(len(p))
	if n > c.left {
		n = c.left
	}
	_, err := c.w.Write(p[:n])
	c.left -= n
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// syncWriter serializes writes from a command's stdout and stderr copiers.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// Short trims s to n runes on one line, with an ellipsis.
func Short(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}
