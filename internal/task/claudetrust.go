package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/client"
	"github.com/xasdta/pilothouse/internal/paths"
)

// Claude Code asks "Do you trust the files in this folder?" the first
// time it runs in a project, before it reads the prompt. A task's agent
// runs detached, so nobody sees that question and the task just sits
// there (QA M4).
//
// Pilothouse does not answer it for the person. Trust is what keeps a
// repository's own .claude/settings.json (hooks, MCP servers, permission
// rules) from running unreviewed, so pre-trusting a folder on the
// person's behalf would remove the one check Claude makes before
// executing repo-provided code. Instead `task new` checks, read-only,
// whether Claude will ask, says so, and marks the session as blocked on a
// question so it shows up in the inbox, on the phone and in `task ls`
// until someone attaches and answers.
//
// Where Claude keeps trust: projects[<path>].hasTrustDialogAccepted in its
// global config, $CLAUDE_CONFIG_DIR/.claude.json or ~/.claude.json (Claude
// Code's own error messages tell people to set exactly that key). For a
// git worktree Claude looks the project up under the main repository's
// root, so a task in a repo the person already uses Claude in is trusted;
// a repo Claude has never run in is not.

// claudeConfigPath is Claude Code's global config file. Under
// $PILOTHOUSE_USER_HOME (tests), $CLAUDE_CONFIG_DIR counts only inside it,
// so a test never reads a real Claude config.
func claudeConfigPath() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		uh := os.Getenv("PILOTHOUSE_USER_HOME")
		if rel, err := filepath.Rel(uh, d); uh == "" || (err == nil && rel != ".." && !strings.HasPrefix(rel, "../")) {
			return filepath.Join(d, ".claude.json")
		}
	}
	return filepath.Join(paths.UserHome(), ".claude.json")
}

// ClaudeAsksTrust reports whether Claude Code will stop at its
// folder-trust question when started in worktree (a worktree of repo). It
// only reads Claude's config; when that cannot be read as JSON it says
// false rather than guess.
func ClaudeAsksTrust(repo, worktree string) bool {
	b, err := os.ReadFile(claudeConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return true // Claude never ran as this user: it will ask
	}
	if err != nil {
		return false
	}
	var cfg struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return false
	}
	for _, dir := range []string{repo, worktree} {
		if dir == "" {
			continue
		}
		cands := []string{filepath.Clean(dir)}
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			cands = append(cands, r)
		}
		for _, c := range cands {
			if cfg.Projects[c].HasTrustDialogAccepted {
				return false
			}
		}
	}
	return true
}

// TrustPromptDetail is what the person is told about a task waiting at
// Claude's folder-trust question.
const TrustPromptDetail = "Claude is asking whether to trust this folder — attach to answer"

// surfaceTrustPrompt tells the person that the task's Claude is waiting at
// its folder-trust question and marks the session blocked on a question,
// as Claude's own OSC 7501 status would if it reported one at that point.
// The next hook (Claude reads the prompt once trust is given) supersedes it.
func (m *Manager) surfaceTrustPrompt(ctx context.Context, t api.Task) {
	fmt.Fprintf(m.errw(), "note: Claude Code has not been trusted in %s yet, so the agent is waiting at its folder-trust question (%s): pilothouse attach %s\n",
		t.Repo, TrustPromptDetail, t.SessionID)
	body := api.OSCBody{State: "blocked", App: "claude", Kind: "question", Raw: TrustPromptDetail}
	// The holder registers the session with the daemon as it starts; give
	// it a moment.
	deadline := time.Now().Add(3 * time.Second)
	for {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := m.daemon().ReportOSC(cctx, t.SessionID, body)
		cancel()
		if err == nil || errors.Is(err, client.ErrDaemonDown) || time.Now().After(deadline) || ctx.Err() != nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}
