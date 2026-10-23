// Package codex adapts the OpenAI Codex CLI through its lifecycle hooks.
//
// Codex (github.com/openai/codex, codex-rs/hooks) runs Claude-style hooks
// declared in ~/.codex/hooks.json or [hooks] in config.toml: SessionStart,
// SessionEnd, UserPromptSubmit, PreToolUse, PermissionRequest,
// PostToolUse, Stop, SubagentStart/Stop, Pre/PostCompact, Interrupt and
// model-request events. The feature flag `hooks` is stable and on by
// default (codex-rs/features/src/lib.rs). What Pilothouse gets from it:
//
//   - PreToolUse can block (exit 2 + reason on stderr, see encode.go).
//     "allow" without updatedInput and "ask" are rejected by Codex as
//     unsupported (hooks/src/engine/output_parser.rs): policy allow is a
//     no-op (Codex's own approval policy decides) and policy ask is sent
//     as a deny telling the agent a person must do it.
//   - PermissionRequest runs before Codex shows its approval prompt and
//     may answer allow or deny (decision.behavior), so remote approvals work.
//   - Hooks from user files run only once trusted (see trust.go);
//     InstallHooks records that trust in config.toml.
//
// docs/AGENTS.md has the full comparison with Claude Code.
package codex

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

// PermissionTimeout is the PermissionRequest hook timeout we install
// (Codex's default for command hooks is the same 600 s).
const PermissionTimeout = 600

// PreToolTimeout bounds PreToolUse; the daemon answers in milliseconds.
const PreToolTimeout = 30

// shortTimeout is for SessionEnd and Interrupt, which Codex clamps to 3 s.
const shortTimeout = 3

// Adapter is the Codex CLI.
type Adapter struct{}

// New returns the Codex adapter.
func New() adapters.Adapter { return Adapter{} }

func (Adapter) Kind() api.AgentKind { return api.AgentCodex }
func (Adapter) DisplayName() string { return "Codex" }

// ProcessNames: the npm launcher (node) spawns the native `codex` binary,
// so the native process is always in a hook's ancestry.
func (Adapter) ProcessNames() []string { return []string{"codex"} }

// Home is Codex's home directory, resolved the way Codex does
// (codex-rs/utils/home-dir, find_codex_home): $CODEX_HOME canonicalized
// (symlinks resolved; /tmp is /private/tmp on macOS), else ~/.codex as
// spelled from $HOME, not canonicalized. Trust keys embed this path, so it
// must match Codex exactly.
//
// Under $PILOTHOUSE_USER_HOME (tests, the e2e harness) $CODEX_HOME counts
// only when it lies inside that home, so a test can never reach a real
// Codex home through an inherited variable.
func Home() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		canon := canonical(h)
		uh := os.Getenv("PILOTHOUSE_USER_HOME")
		if uh == "" || within(canon, canonical(uh)) {
			return canon
		}
	}
	return filepath.Join(paths.UserHome(), ".codex")
}

// canonical is p made absolute with symlinks resolved; when p does not
// exist (Codex then refuses to start), its nearest existing ancestor is
// resolved instead.
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

// HooksPath is the hooks file Pilothouse manages.
func HooksPath() string { return filepath.Join(Home(), "hooks.json") }

// ConfigPath is Codex's user config, where hook trust lives.
func ConfigPath() string { return filepath.Join(Home(), "config.toml") }

func (Adapter) Detect() adapters.Detection {
	d := adapters.Detection{SettingsPath: HooksPath()}
	d.Binary, d.Version = hookkit.DetectBinary("codex")
	_, err := os.Stat(Home())
	d.Installed = d.Binary != "" || err == nil
	if d.Binary == "" && d.Installed {
		d.Note = Home() + " exists but `codex` is not on PATH"
	}
	return d
}

// MaxHold: Codex kills a PermissionRequest hook at its timeout (600 s as
// installed); nothing else may block.
func (Adapter) MaxHold(kind adapters.HookKind) time.Duration {
	if kind == adapters.HookPermission {
		return PermissionTimeout * time.Second
	}
	return 0
}

// ProtectedPaths: whoever edits these can add or trust a hook that
// approves everything. "**/" entries are glob patterns for project
// layers (<repo>/.codex/).
func (Adapter) ProtectedPaths() []string {
	return []string{
		ConfigPath(),
		HooksPath(),
		"**/.codex/config.toml",
		"**/.codex/hooks.json",
	}
}

// ResumeArgs: `codex resume <SESSION_ID>` (codex-rs/cli/src/main.rs).
func (Adapter) ResumeArgs(agentSessionID string) []string {
	if agentSessionID == "" {
		return nil
	}
	return []string{"codex", "resume", agentSessionID}
}
