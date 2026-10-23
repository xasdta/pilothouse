package codex

import (
	"fmt"
	"strings"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/hookkit"
	"github.com/xasdta/pilothouse/internal/api"
)

// kinds maps Codex hook events (codex-rs/hooks/src/lib.rs HOOK_EVENT_NAMES)
// to Pilothouse's. Interrupt (the person pressed Esc) ends the turn the same
// way Stop does: the session now waits for its next prompt.
var kinds = map[string]adapters.HookKind{
	"SessionStart":      adapters.HookSessionStart,
	"SessionEnd":        adapters.HookSessionEnd,
	"UserPromptSubmit":  adapters.HookPromptSubmit,
	"PreToolUse":        adapters.HookPreTool,
	"PermissionRequest": adapters.HookPermission,
	"PostToolUse":       adapters.HookPostTool,
	"Stop":              adapters.HookStop,
	"Interrupt":         adapters.HookStop,
	"SubagentStop":      adapters.HookSubagentStop,
	"PreCompact":        adapters.HookPreCompact,
}

// ParseHook normalizes one Codex hook payload (schemas:
// codex-rs/hooks/schema/generated/*.command.input.schema.json).
func (Adapter) ParseHook(env adapters.HookEnvelope) (adapters.HookEvent, error) {
	p, err := hookkit.ParsePayload(env)
	if err != nil {
		return adapters.HookEvent{}, err
	}
	kind, ok := kinds[p.Event]
	if !ok {
		kind = adapters.HookUnknown
	}
	ev := p.HookEvent(kind)
	if p.Event == "Interrupt" {
		ev.Summary = "Interrupted"
	}
	if p.ToolName != "" {
		describeTool(&ev)
	}
	return ev, nil
}

// describeTool classifies Codex's hook tool names
// (codex-rs/core/src/tools/hook_names.rs and the handlers' payloads):
// "Bash" {command} for exec_command and network-access approvals,
// "apply_patch" {command: <patch>}, "mcp__<server>__<tool>" with the raw
// arguments, "write_stdin" {chars, …} for input to a running command,
// plus spawn_agent, request_permissions and extension tools.
func describeTool(ev *adapters.HookEvent) {
	field := func(name string) string { return hookkit.Field(ev.Input, name) }
	switch ev.Tool {
	case "Bash":
		ev.ToolKind = api.ToolShell
		ev.Command = field("command")
		ev.Summary = "Run: " + hookkit.OneLine(ev.Command, 160)
		// Without an apply_patch tool (unknown model slugs), the model
		// patches through exec_command: `apply_patch <<'EOF' … EOF`. When
		// that is the whole command Codex applies the patch itself, so it
		// is an edit of the files the patch names; path rules must see them.
		if paths, exact := hookkit.ShellPatch(ev.Command, ev.Cwd); len(paths) > 0 {
			ev.Paths = paths
			if exact {
				ev.ToolKind = api.ToolEdit
				ev.Summary = editSummary(paths, ev.Cwd)
			}
		}
	case "write_stdin":
		// Keystrokes into a running command (often a shell): treat them as
		// a command so shell rules still apply.
		ev.ToolKind = api.ToolShell
		ev.Command = strings.TrimSpace(field("chars"))
		ev.Summary = "Type into running command: " + hookkit.OneLine(ev.Command, 120)
	case "apply_patch":
		ev.ToolKind = api.ToolEdit
		ev.Paths = hookkit.PatchPaths(field("command"), ev.Cwd)
		ev.Summary = editSummary(ev.Paths, ev.Cwd)
	case "request_permissions":
		ev.ToolKind = api.ToolOther
		ev.Summary = "Request permissions"
		if r := field("reason"); r != "" {
			ev.Summary += ": " + hookkit.OneLine(r, 120)
		}
	default:
		if server, name, ok := hookkit.MCPName(ev.Tool); ok {
			ev.ToolKind = api.ToolMCP
			ev.Summary = "MCP " + server + ": " + name
			return
		}
		ev.ToolKind = api.ToolOther
		ev.Summary = "Use " + ev.Tool
		for _, f := range []string{"description", "message", "prompt"} {
			if v := strings.TrimSpace(field(f)); v != "" {
				ev.Summary = ev.Tool + ": " + hookkit.OneLine(v, 120)
				break
			}
		}
	}
}

func editSummary(files []string, cwd string) string {
	if len(files) == 0 {
		return "Edit files"
	}
	var names []string
	for _, f := range files[:min(len(files), 3)] {
		names = append(names, hookkit.RelPath(cwd, f))
	}
	s := "Edit: " + strings.Join(names, ", ")
	if len(files) > 3 {
		s += fmt.Sprintf(" (+%d more)", len(files)-3)
	}
	return s
}
