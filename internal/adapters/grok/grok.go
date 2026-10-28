// Package grok adapts xAI's Grok Build CLI through its lifecycle hooks.
//
// Grok reads Claude-style hook files from $GROK_HOME/hooks/*.json (default
// ~/.grok/hooks) and from .grok/hooks in a project. Pilothouse installs a
// file of its own in the personal directory. The events, payload fields and
// PreToolUse decision contract are documented at
// https://docs.x.ai/build/features/hooks:
//
//   - PreToolUse is the only blocking event. An explicit deny stops the
//     call and an ask forces Grok's permission prompt; a timeout, crash or
//     malformed answer fails open. Grok has no PermissionRequest hook, so a
//     prompt cannot be answered from another device: an ask waits at the
//     terminal.
//   - Grok runs personal hooks only after the folder is trusted; a headless
//     `grok -p` run trusts its folder for personal hooks. Project hooks
//     always need trust. Pilothouse never writes project hooks.
//   - SessionStart/End, UserPromptSubmit, PostToolUse(+Failure), Stop
//     (+Failure/Cancelled), Notification, SubagentStart/Stop and compaction
//     events keep session state current.
//
// docs/AGENTS.md has the comparison with the other agents.
package grok

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/hookkit"
	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/paths"
)

// PreToolTimeout bounds PreToolUse; the daemon answers in milliseconds.
const PreToolTimeout = 30

// Adapter is the Grok Build CLI.
type Adapter struct{}

// New returns the Grok adapter.
func New() adapters.Adapter { return Adapter{} }

func (Adapter) Kind() api.AgentKind { return api.AgentGrok }
func (Adapter) DisplayName() string { return "Grok Build" }

// ProcessNames: the launcher and native binary both call themselves grok.
func (Adapter) ProcessNames() []string { return []string{"grok", "grok-build"} }

// Home is Grok's home directory: $GROK_HOME (canonicalized), else ~/.grok.
// Under $PILOTHOUSE_USER_HOME (tests, the e2e harness) an inherited
// GROK_HOME counts only when it lies inside that home, so a test can never
// reach the person's real Grok home.
func Home() string {
	if h := os.Getenv("GROK_HOME"); h != "" {
		canon := canonical(h)
		uh := os.Getenv("PILOTHOUSE_USER_HOME")
		if uh == "" || within(canon, canonical(uh)) {
			return canon
		}
	}
	return filepath.Join(paths.UserHome(), ".grok")
}

// HooksPath is the personal hook file Pilothouse manages.
func HooksPath() string { return filepath.Join(Home(), "hooks", "pilothouse.json") }

// ConfigPath is Grok's user config.
func ConfigPath() string { return filepath.Join(Home(), "config.toml") }

func (Adapter) Detect() adapters.Detection {
	d := adapters.Detection{SettingsPath: HooksPath()}
	d.Binary, d.Version = hookkit.DetectBinary("grok")
	_, err := os.Stat(Home())
	d.Installed = d.Binary != "" || err == nil
	if d.Binary == "" && d.Installed {
		d.Note = Home() + " exists but `grok` is not on PATH"
	}
	return d
}

// MaxHold: only PreToolUse may block, and Grok kills it at the installed
// timeout.
func (Adapter) MaxHold(kind adapters.HookKind) time.Duration {
	if kind == adapters.HookPreTool {
		return PreToolTimeout * time.Second
	}
	return 0
}

// ProtectedPaths: editing these can add or trust hooks that approve
// everything. Personal hook files and the project's hook directory are
// both in reach.
func (Adapter) ProtectedPaths() []string {
	return []string{
		ConfigPath(),
		HooksPath(),
		filepath.Join(Home(), "trusted_folders.toml"),
		"**/.grok/config.toml",
		"**/.grok/hooks/*.json",
		"**/.grok/hooks/**",
	}
}

// ResumeArgs resumes both TUI and headless sessions.
func (Adapter) ResumeArgs(agentSessionID string) []string {
	if agentSessionID == "" {
		return nil
	}
	return []string{"grok", "--resume", agentSessionID}
}

func canonical(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	dir, rest := p, ""
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			return p
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(r, rest)
		}
	}
}

func within(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}
