package claude

import (
	"encoding/json"
	"strings"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/hookkit"
	"github.com/xasdta/pilothouse/internal/api"
)

// kinds maps Claude Code hook events to Pilothouse's. PostToolUseFailure is a
// post_tool too: the tool ran, so a person (or a rule) let it through.
var kinds = map[string]adapters.HookKind{
	"SessionStart":       adapters.HookSessionStart,
	"SessionEnd":         adapters.HookSessionEnd,
	"UserPromptSubmit":   adapters.HookPromptSubmit,
	"PreToolUse":         adapters.HookPreTool,
	"PermissionRequest":  adapters.HookPermission,
	"PostToolUse":        adapters.HookPostTool,
	"PostToolUseFailure": adapters.HookPostTool,
	"Notification":       adapters.HookNotification,
	"Stop":               adapters.HookStop,
	"SubagentStop":       adapters.HookSubagentStop,
	"PreCompact":         adapters.HookPreCompact,
}

// ParseHook normalizes one Claude Code hook payload. Unknown events parse
// as HookUnknown so newer Claude Code versions never break the shim.
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
	switch {
	case asksPerson(p.ToolName):
		describeAsk(&ev, env.Payload)
	case p.ToolName != "":
		describeTool(&ev)
	}
	return ev, nil
}

// describeTool fills ToolKind, Command, Paths and Summary from the tool
// name and input, per the tool input schemas in the hooks reference.
func describeTool(ev *adapters.HookEvent) {
	in := ev.Input
	field := func(name string) string { return hookkit.Field(in, name) }
	rel := func(p string) string { return hookkit.RelPath(ev.Cwd, p) }
	switch ev.Tool {
	case "Bash", "PowerShell":
		ev.ToolKind = api.ToolShell
		ev.Command = field("command")
		ev.Summary = "Run: " + hookkit.OneLine(ev.Command, 160)
		// An apply_patch heredoc (Codex-style patch tools on PATH) names
		// the files it edits; path rules must see them. The call stays a
		// shell command: Claude runs it through the shell.
		if paths, _ := hookkit.ShellPatch(ev.Command, ev.Cwd); len(paths) > 0 {
			ev.Paths = paths
		}
	case "Edit", "MultiEdit", "Write", "NotebookEdit":
		ev.ToolKind = api.ToolEdit
		p := field("file_path")
		if ev.Tool == "NotebookEdit" {
			p = field("notebook_path")
		}
		ev.Paths = nonEmpty(p)
		verb := "Edit"
		if ev.Tool == "Write" {
			verb = "Write"
		}
		ev.Summary = verb + ": " + rel(p)
	case "Read":
		ev.ToolKind = api.ToolRead
		ev.Paths = nonEmpty(field("file_path"))
		ev.Summary = "Read: " + rel(field("file_path"))
	case "Glob", "Grep", "LS":
		ev.ToolKind = api.ToolRead
		dir := field("path")
		ev.Paths = nonEmpty(dir)
		ev.Summary = searchSummary(ev.Tool, field("pattern"), rel(dir))
	case "WebFetch":
		ev.ToolKind = api.ToolWeb
		ev.Summary = "Fetch: " + hookkit.Truncate(field("url"), 160)
	case "WebSearch":
		ev.ToolKind = api.ToolWeb
		ev.Summary = "Search the web: " + hookkit.OneLine(field("query"), 120)
	default:
		if server, name, ok := hookkit.MCPName(ev.Tool); ok {
			ev.ToolKind = api.ToolMCP
			ev.Summary = "MCP " + server + ": " + name
			return
		}
		ev.ToolKind = api.ToolOther
		ev.Summary = otherSummary(ev.Tool, in)
	}
}

func searchSummary(tool, pattern, dir string) string {
	var s string
	switch tool {
	case "Glob":
		s = "Find files: " + hookkit.OneLine(pattern, 100)
	case "Grep":
		s = "Search: " + hookkit.OneLine(pattern, 100)
	default:
		return "List: " + dir
	}
	if dir != "" {
		s += " in " + dir
	}
	return s
}

// otherSummary names the tool and, when the input has an obvious human
// description (Agent/Task tools), shows it.
func otherSummary(tool string, in json.RawMessage) string {
	for _, f := range []string{"description", "subject", "query"} {
		if v := strings.TrimSpace(hookkit.Field(in, f)); v != "" {
			return tool + ": " + hookkit.OneLine(v, 120)
		}
	}
	return "Use " + tool
}

func nonEmpty(p string) []string {
	if p == "" {
		return nil
	}
	return []string{p}
}
