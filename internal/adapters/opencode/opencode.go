// Package opencode adapts OpenCode (opencode.ai) through a plugin.
//
// OpenCode has no command hooks. It loads JavaScript plugins from
// {plugin,plugins}/*.{js,ts} in its config directories and runs them inside
// its own process, so Pilothouse installs plugin/pilothouse.js (embedded
// here) into the global config directory. The plugin talks to the daemon
// over its Unix socket:
//
//   - POST /v1/plugin/hook with a HookEnvelope whose payload is the
//     plugin's own normalized event (parse.go), answered with a
//     HookResponse whose Stdout is a small JSON answer (encode.go);
//   - GET /v1/plugin/commands, a long poll that delivers the replies a
//     person sends to the plugin's sessions (daemon/plugin.go), which the
//     plugin runs with OpenCode's client.session.promptAsync.
//
// What OpenCode gives a plugin (verified against OpenCode 1.18.25):
// tool.execute.before runs before every tool call and before OpenCode's
// own permission check, and an error thrown there fails the call with
// that message (policy deny); permission.asked / question.asked events
// announce OpenCode's own dialogs, which the plugin answers through the
// server's /permission/{id}/reply and /question/{id}/reply routes; there
// is no way for a plugin to make OpenCode show a prompt, and the
// permission.ask plugin hook is never called.
//
// docs/AGENTS.md has the full comparison with Claude Code and Codex.
package opencode

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/hookkit"
	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/paths"
)

// MaxHold bounds a held permission, question or policy "ask". OpenCode
// puts no timeout on plugin calls; the daemon holds for at most
// approvals.hold_limit anyway.
const MaxHold = time.Hour

// Adapter is OpenCode.
type Adapter struct{}

// New returns the OpenCode adapter.
func New() adapters.Adapter { return Adapter{} }

func (Adapter) Kind() api.AgentKind { return api.AgentOpenCode }
func (Adapter) DisplayName() string { return "OpenCode" }

// ProcessNames: OpenCode is one native binary (the npm package's launcher
// execs it), and plugins run inside it.
func (Adapter) ProcessNames() []string { return []string{"opencode"} }

// ConfigDir is OpenCode's global config directory, resolved the way
// OpenCode does (xdg-basedir): $XDG_CONFIG_HOME/opencode, else
// ~/.config/opencode, on every platform. OpenCode always loads it, whether
// or not $OPENCODE_CONFIG_DIR adds another directory.
//
// Under $PILOTHOUSE_USER_HOME (tests, the e2e harness) $XDG_CONFIG_HOME
// counts only when it lies inside that home, so a test can never reach a
// real OpenCode config through an inherited variable.
func ConfigDir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" && filepath.IsAbs(x) && inUserHome(x) {
		return filepath.Join(x, "opencode")
	}
	return filepath.Join(paths.UserHome(), ".config", "opencode")
}

// extraDirs are the other directories OpenCode loads plugins from for
// every project: ~/.opencode and $OPENCODE_CONFIG_DIR. A Pilothouse plugin
// there would load a second time.
func extraDirs() []string {
	dirs := []string{filepath.Join(paths.UserHome(), ".opencode")}
	if d := os.Getenv("OPENCODE_CONFIG_DIR"); d != "" && filepath.IsAbs(d) && inUserHome(d) {
		dirs = append(dirs, d)
	}
	return dirs
}

func inUserHome(p string) bool {
	uh := os.Getenv("PILOTHOUSE_USER_HOME")
	if uh == "" {
		return true
	}
	rel, err := filepath.Rel(uh, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// PluginPath is the plugin file Pilothouse manages.
func PluginPath() string { return filepath.Join(ConfigDir(), "plugins", PluginFile) }

// PluginFile is the plugin's file name in a plugins directory.
const PluginFile = "pilothouse.js"

func (Adapter) Detect() adapters.Detection {
	d := adapters.Detection{SettingsPath: PluginPath()}
	if os.Getenv("PILOTHOUSE_USER_HOME") == "" {
		d.Binary, d.Version = hookkit.DetectBinary("opencode")
	} else if p, err := exec.LookPath("opencode"); err == nil {
		// Tests and the e2e harness: `opencode --version` creates
		// OpenCode's XDG directories, which must stay untouched.
		d.Binary = p
	}
	if d.Binary == "" {
		// The official installer puts the binary in ~/.opencode/bin and
		// adds it to PATH in shell startup files, which a login service
		// or a fresh `pilothouse setup` may not have read.
		if p := filepath.Join(paths.UserHome(), ".opencode", "bin", "opencode"); isExecutable(p) {
			d.Binary = p
		}
	}
	_, err := os.Stat(ConfigDir())
	d.Installed = d.Binary != "" || err == nil
	if d.Binary == "" && d.Installed {
		d.Note = ConfigDir() + " exists but `opencode` is not on PATH"
	}
	return d
}

func isExecutable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0
}

// MaxHold: permission prompts, questions and policy asks may be held (the
// plugin waits as long as the daemon does); nothing else blocks.
func (Adapter) MaxHold(kind adapters.HookKind) time.Duration {
	if kind == adapters.HookPermission {
		return MaxHold
	}
	return 0
}

// ProtectedPaths: whoever edits OpenCode's config or drops a plugin into a
// plugin directory can allow every tool call or answer every prompt. The
// global config directory's config files and plugin directories are
// absolute entries; "**/" entries cover project configs (opencode.json at
// any level, .opencode/ directories) and ~/.opencode.
func (Adapter) ProtectedPaths() []string {
	dir := ConfigDir()
	return []string{
		filepath.Join(dir, "opencode.json"),
		filepath.Join(dir, "opencode.jsonc"),
		filepath.Join(dir, "config.json"),
		filepath.Join(dir, "plugins"),
		filepath.Join(dir, "plugin"),
		"**/opencode.json",
		"**/opencode.jsonc",
		"**/.opencode/opencode.json",
		"**/.opencode/opencode.jsonc",
		"**/.opencode/plugins/*",
		"**/.opencode/plugin/*",
	}
}

// ResumeArgs: `opencode --session <id>` continues a session.
func (Adapter) ResumeArgs(agentSessionID string) []string {
	if agentSessionID == "" {
		return nil
	}
	return []string{"opencode", "--session", agentSessionID}
}
