package task

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

// Time limits for the steps of a ship. Nothing waits for a person: git and
// gh run without a terminal.
const (
	commitTimeout = 2 * time.Minute
	pushTimeout   = 3 * time.Minute
	ghTimeout     = 2 * time.Minute
)

// Ship finishes a task for a person who reviewed its diff away from the
// terminal (the phone's Ship button). It stops the agent, commits what the
// agent left uncommitted (without the files task.yaml copied in), pushes
// the branch to origin and opens a pull request with gh when it is
// installed, then removes the worktree and keeps the branch. The task is
// done once its work is safely on the branch; Task.Note says where it went
// and Task.PRURL links the pull request.
//
// It refuses, changing nothing, when there is nothing to ship or when a
// file task.yaml copied in (.env and the like) was committed on the
// branch. A failed push leaves the task open, with the work committed.
func (m *Manager) Ship(ctx context.Context, ref string) (api.Task, error) {
	t, err := m.Get(ref)
	if err != nil {
		return t, err
	}
	switch t.Status {
	case api.TaskDone, api.TaskAbandon:
		return t, fmt.Errorf("task %s (%s) is already %s", t.ID, t.Title, t.Status)
	case api.TaskFailed:
		return t, fmt.Errorf("task %s (%s) never started, so there is nothing to ship — discard it", t.ID, t.Title)
	}
	wt := dirExists(t.Worktree)
	dirty := false
	if wt {
		if dirty, err = hasLeftovers(ctx, t); err != nil {
			return t, err
		}
	}
	ahead, err := gitOutput(ctx, t.Repo, "rev-list", "--count", t.BaseRef+".."+"refs/heads/"+t.Branch)
	if err != nil {
		return t, err
	}
	if ahead == "0" && !dirty {
		return t, fmt.Errorf("nothing to ship: %s has no changes beyond %s", t.Branch, t.BaseRef)
	}
	if err := checkCopiedNotCommitted(ctx, t); err != nil {
		return t, err
	}

	if err := m.stopAgent(ctx, t); err != nil {
		return t, err
	}
	if dirty {
		if err := commitLeftovers(ctx, t); err != nil {
			return t, err
		}
	}

	if _, err := gitOutput(ctx, t.Repo, "remote", "get-url", "origin"); err != nil {
		t.Note = fmt.Sprintf("The work is committed on %s in %s. The repository has no origin remote, so push it yourself: git -C %s push -u <remote> %s",
			t.Branch, t.Repo, t.Repo, t.Branch)
		return m.shipped(ctx, t, wt)
	}
	if out, err := runQuiet(ctx, pushTimeout, t.Repo, "git", "push", "--porcelain", "-u", "origin", "refs/heads/"+t.Branch+":refs/heads/"+t.Branch); err != nil {
		t.Note = fmt.Sprintf("git push failed (%s). The work is committed on %s; ship again, or push it yourself.", lastLines(out, err), t.Branch)
		t.UpdatedAt = m.now()
		if serr := saveTask(t); serr == nil {
			m.mirror(ctx, t)
		}
		return t, errors.New(t.Note)
	}
	gh, err := exec.LookPath("gh")
	if err != nil {
		t.Note = fmt.Sprintf("Pushed %s to origin. gh (GitHub CLI) is not installed here, so open the pull request on GitHub.", t.Branch)
		return m.shipped(ctx, t, wt)
	}
	args := []string{"pr", "create", "--fill", "--head", t.Branch}
	if gitOK(ctx, t.Repo, "show-ref", "--verify", "--quiet", "refs/remotes/origin/"+t.BaseRef) {
		args = append(args, "--base", t.BaseRef)
	}
	out, err := runQuiet(ctx, ghTimeout, t.Repo, gh, args...)
	// gh prints the new PR's URL, or names the existing one in its error.
	t.PRURL = prURLRE.FindString(out)
	switch {
	case t.PRURL != "" && err == nil:
		t.Note = fmt.Sprintf("Pushed %s to origin and opened %s.", t.Branch, t.PRURL)
	case t.PRURL != "":
		t.Note = fmt.Sprintf("Pushed %s to origin; its pull request is %s.", t.Branch, t.PRURL)
	default:
		t.Note = fmt.Sprintf("Pushed %s to origin, but gh could not open a pull request (%s); open it on GitHub.", t.Branch, lastLines(out, err))
	}
	return m.shipped(ctx, t, wt)
}

func (m *Manager) shipped(ctx context.Context, t api.Task, wt bool) (api.Task, error) {
	if wt {
		// Everything worth keeping is committed; what is left is ignored
		// files and task.yaml's copies.
		if err := removeWorktree(ctx, t, true); err != nil {
			fmt.Fprintf(m.errw(), "warning: could not remove worktree %s: %v\n", t.Worktree, err)
		}
	}
	t.Status, t.UpdatedAt = api.TaskDone, m.now()
	if err := saveTask(t); err != nil {
		return t, err
	}
	m.mirror(ctx, t)
	return t, nil
}

// hasLeftovers reports whether the worktree has changes the agent did not
// commit, not counting task.yaml's copies.
func hasLeftovers(ctx context.Context, t api.Task) (bool, error) {
	out, _, err := gitRead(ctx, t.Worktree, maxGitMeta, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return false, err
	}
	copied := copiedSet(t)
	for _, rec := range strings.Split(string(out), "\x00") {
		if len(rec) < 4 {
			continue // also the source name that follows a rename
		}
		if !copied[rec[3:]] {
			return true, nil
		}
	}
	return false, nil
}

func copiedSet(t api.Task) map[string]bool {
	m := map[string]bool{}
	if t.Setup != nil {
		for _, c := range t.Setup.Copied {
			m[c] = true
		}
	}
	return m
}

// checkCopiedNotCommitted refuses to ship a branch on which the agent
// committed one of the files task.yaml copied in: they are local files
// (secrets, often) that must not reach a remote.
func checkCopiedNotCommitted(ctx context.Context, t api.Task) error {
	if t.Setup == nil || len(t.Setup.Copied) == 0 {
		return nil
	}
	args := append([]string{"--literal-pathspecs", "diff", "--name-only", "-z", t.BaseRef + "..." + "refs/heads/" + t.Branch, "--"}, t.Setup.Copied...)
	out, err := gitOutput(ctx, t.Repo, args...)
	if err != nil {
		return err
	}
	if names := strings.Trim(out, "\x00"); names != "" {
		return fmt.Errorf("not shipping: %s committed %s, which %s copied from your checkout — remove it from the branch's history first",
			t.Branch, strings.ReplaceAll(names, "\x00", ", "), ConfigFile)
	}
	return nil
}

// commitLeftovers commits everything the agent left uncommitted except
// task.yaml's copies. Commit hooks run, as they would for the person.
func commitLeftovers(ctx context.Context, t api.Task) error {
	if _, err := gitOutput(ctx, t.Worktree, "add", "-A"); err != nil {
		return err
	}
	if copied := t.Setup; copied != nil && len(copied.Copied) > 0 {
		args := append([]string{"--literal-pathspecs", "rm", "--cached", "-q", "--ignore-unmatch", "--"}, copied.Copied...)
		if _, err := gitOutput(ctx, t.Worktree, args...); err != nil {
			return err
		}
	}
	if gitOK(ctx, t.Worktree, "diff", "--cached", "--quiet") {
		return nil
	}
	subject := Short(strings.SplitN(strings.TrimSpace(t.Prompt), "\n", 2)[0], 72)
	body := "Changes the agent left uncommitted, committed by Pilothouse when the task was shipped.\n\nPilothouse-Task: " + t.ID
	if out, err := runQuiet(ctx, commitTimeout, t.Worktree, "git", "commit", "-q", "-m", subject, "-m", body); err != nil {
		return fmt.Errorf("git commit failed (%s) — the changes are still in %s", lastLines(out, err), t.Worktree)
	}
	return nil
}

// runQuiet runs a command with no stdin and no terminal prompts, within
// timeout, and returns its combined output (bounded).
func runQuiet(ctx context.Context, timeout time.Duration, dir, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true", "GIT_PAGER=cat",
		"GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1")
	var out bytes.Buffer
	lb := &limitedBuffer{b: &out, left: 16 << 10}
	cmd.Stdout, cmd.Stderr = lb, lb
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("timed out after %s", timeout)
	}
	return out.String(), err
}

var (
	prURLRE = regexp.MustCompile(`https?://[^\s"']+/pull/\d+`)
	// Credentials in remote URLs and GitHub tokens never reach a message.
	credURLRE = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/\s@]+@`)
	tokenRE   = regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{16,})\b`)
)

// redact removes credentials from command output before it is shown.
func redact(s string) string {
	s = credURLRE.ReplaceAllString(s, "${1}***@")
	return tokenRE.ReplaceAllString(s, "***")
}

// lastLines is the end of a command's output (or its error), redacted, on
// one line, for a message.
func lastLines(out string, err error) string {
	var keep []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			keep = append(keep, l)
		}
	}
	if len(keep) > 3 {
		keep = keep[len(keep)-3:]
	}
	msg := strings.Join(keep, "; ")
	if msg == "" && err != nil {
		msg = err.Error()
	}
	return Short(redact(msg), 300)
}
