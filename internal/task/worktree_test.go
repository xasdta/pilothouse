package task

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/paths"
	"github.com/xasdta/pilothouse/internal/policy"
)

func TestWorktreesRoot(t *testing.T) {
	testEnv(t)
	t.Setenv(WorktreesEnv, "")
	if got, want := WorktreesRoot(), filepath.Join(paths.UserHome(), ".pilothouse-worktrees"); got != want {
		t.Errorf("default = %s, want %s", got, want)
	}
	if strings.HasPrefix(WorktreesRoot(), paths.Home()) {
		t.Error("worktrees inside the Pilothouse home")
	}
	dir := t.TempDir()
	t.Setenv(WorktreesEnv, dir)
	if WorktreesRoot() != dir {
		t.Errorf("override = %s", WorktreesRoot())
	}
}

// TestTaskWorktreeNotSelfProtected (H2): inside a task's worktree, the
// agent's everyday calls are not stopped by self-protection, for new
// tasks and for tasks made when worktrees lived in ~/.pilothouse.
func TestTaskWorktreeNotSelfProtected(t *testing.T) {
	testEnv(t)
	repo := newRepo(t)
	m, _ := newManager(t)
	ctx := context.Background()
	newTask, err := m.New(ctx, NewOptions{Prompt: "new layout", Agent: fakeAgentScript(t, true), Dir: repo})
	if err != nil {
		t.Fatal(err)
	}
	// A task from before the move: same code, old location.
	t.Setenv(WorktreesEnv, filepath.Join(paths.Home(), "worktrees"))
	oldTask, err := m.New(ctx, NewOptions{Prompt: "old layout", Agent: fakeAgentScript(t, true), Dir: repo})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(WorktreesEnv, "")
	if !strings.HasPrefix(oldTask.Worktree, paths.Home()) {
		t.Fatalf("legacy worktree at %s", oldTask.Worktree)
	}
	e, _ := policy.Load(policy.DefaultOptions())
	for _, tk := range []api.Task{newTask, oldTask} {
		for _, in := range []policy.Input{
			{Tool: "Bash", ToolKind: api.ToolShell, Command: "git status"},
			{Tool: "Bash", ToolKind: api.ToolShell, Command: "git commit -am wip"},
			{Tool: "Edit", ToolKind: api.ToolEdit, Paths: []string{filepath.Join(tk.Worktree, "README.md")}},
		} {
			in.Agent, in.Cwd, in.Repo = api.AgentClaude, tk.Worktree, tk.Worktree
			if r := e.Evaluate(in); r.Rule == policy.SelfProtectionRule {
				t.Errorf("%s in %s: %s", in.Command+strings.Join(in.Paths, ""), tk.Worktree, r.Message)
			}
		}
	}
	// The stored path keeps working for the legacy task.
	if _, err := m.Abandon(ctx, oldTask.ID, AbandonOptions{}); err != nil {
		t.Fatal(err)
	}
	if dirExists(oldTask.Worktree) {
		t.Error("legacy worktree not removed")
	}
}

func writeClaudeConfig(t *testing.T, dir string, projects map[string]bool) {
	t.Helper()
	cfg := map[string]any{"numStartups": 3, "projects": map[string]any{}}
	for p, ok := range projects {
		cfg["projects"].(map[string]any)[p] = map[string]any{"allowedTools": []string{}, "hasTrustDialogAccepted": ok}
	}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeAsksTrust(t *testing.T) {
	testEnv(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home := paths.UserHome()
	repo, wt := "/w/app", "/x/.pilothouse-worktrees/app/fix"
	if !ClaudeAsksTrust(repo, wt) {
		t.Error("no ~/.claude.json: Claude asks")
	}
	writeClaudeConfig(t, home, map[string]bool{"/w/other": true, repo: false})
	if !ClaudeAsksTrust(repo, wt) {
		t.Error("untrusted repo")
	}
	writeClaudeConfig(t, home, map[string]bool{repo: true})
	if ClaudeAsksTrust(repo, wt) {
		t.Error("trusted main repo covers its worktrees")
	}
	writeClaudeConfig(t, home, map[string]bool{wt: true})
	if ClaudeAsksTrust(repo, wt) {
		t.Error("trusted worktree")
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ClaudeAsksTrust(repo, wt) {
		t.Error("unreadable config: don't guess")
	}
	// CLAUDE_CONFIG_DIR inside the (test) user home is honored, outside
	// it is ignored so tests never read a real config.
	cfgDir := filepath.Join(home, "claude-cfg")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeClaudeConfig(t, cfgDir, map[string]bool{repo: true})
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	if ClaudeAsksTrust(repo, wt) {
		t.Error("CLAUDE_CONFIG_DIR not used")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if ClaudeAsksTrust(repo, wt) {
		t.Error("CLAUDE_CONFIG_DIR outside the test home was read")
	}
}

// TestNewSurfacesClaudeTrustPrompt (M4): a Claude task in a repo Claude
// has never been trusted in says so and shows as blocked on a question;
// in a trusted repo it does neither. Claude's config is never written.
func TestNewSurfacesClaudeTrustPrompt(t *testing.T) {
	testEnv(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	fd := startFakeDaemon(t, nil)
	repo := newRepo(t)
	bin := t.TempDir()
	claude := filepath.Join(bin, "claude")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx := context.Background()
	var out bytes.Buffer
	m := &Manager{Start: func(context.Context, StartSpec) (string, error) { return "s_claude0001", nil }, Out: &out, Err: &out}

	tk, err := m.New(ctx, NewOptions{Prompt: "untrusted", Agent: "claude", Dir: repo})
	if err != nil {
		t.Fatal(err)
	}
	if tk.Agent != api.AgentClaude || !strings.Contains(out.String(), "folder-trust question") || !strings.Contains(out.String(), "pilothouse attach s_claude0001") {
		t.Errorf("no trust note:\n%s", out.String())
	}
	fd.mu.Lock()
	got := fd.osc["s_claude0001"]
	fd.mu.Unlock()
	if got.State != "blocked" || got.Kind != "question" || got.App != "claude" || got.Raw != TrustPromptDetail {
		t.Errorf("osc = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(paths.UserHome(), ".claude.json")); !os.IsNotExist(err) {
		t.Error("Claude's config was written")
	}

	// Trusted repo: nothing to surface.
	writeClaudeConfig(t, paths.UserHome(), map[string]bool{repo: true})
	before, _ := os.ReadFile(filepath.Join(paths.UserHome(), ".claude.json"))
	out.Reset()
	m.Start = func(context.Context, StartSpec) (string, error) { return "s_claude0002", nil }
	if _, err := m.New(ctx, NewOptions{Prompt: "trusted", Agent: "claude", Dir: repo}); err != nil {
		t.Fatal(err)
	}
	fd.mu.Lock()
	_, reported := fd.osc["s_claude0002"]
	fd.mu.Unlock()
	if reported || strings.Contains(out.String(), "folder-trust") {
		t.Errorf("trusted repo surfaced a prompt:\n%s", out.String())
	}
	if after, _ := os.ReadFile(filepath.Join(paths.UserHome(), ".claude.json")); !bytes.Equal(before, after) {
		t.Error("Claude's config was modified")
	}
}
