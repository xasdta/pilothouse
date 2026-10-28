package grok

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/hookkit"
	"github.com/xasdta/pilothouse/internal/api"
)

// kinds maps Grok's event names, after normalization, to Pilothouse's.
// PostToolUseFailure is a post_tool too: the tool ran, so a person (or a
// rule) let it through. StopFailure and StopCancelled still end the turn;
// their error or cancellation reason is kept as the message.
// PermissionDenied is informational only: the tool did not run, and no
// dialog is waiting.
var kinds = map[string]adapters.HookKind{
	"sessionstart":       adapters.HookSessionStart,
	"sessionend":         adapters.HookSessionEnd,
	"userpromptsubmit":   adapters.HookPromptSubmit,
	"pretooluse":         adapters.HookPreTool,
	"posttooluse":        adapters.HookPostTool,
	"posttoolusefailure": adapters.HookPostTool,
	"permissiondenied":   adapters.HookUnknown,
	"stop":               adapters.HookStop,
	"stopfailure":        adapters.HookStop,
	"stopcancelled":      adapters.HookStop,
	"notification":       adapters.HookNotification,
	"subagentstart":      adapters.HookUnknown,
	"subagentstop":       adapters.HookSubagentStop,
	"precompact":         adapters.HookPreCompact,
	"postcompact":        adapters.HookUnknown,
}

// ParseHook normalizes one Grok hook payload. Grok documents camelCase
// fields (hookEventName, sessionId, toolName, toolInput), while its
// Claude-compatibility path also accepts the snake_case names Pilothouse
// uses with Claude and Codex; both are read here.
func (Adapter) ParseHook(env adapters.HookEnvelope) (adapters.HookEvent, error) {
	if len(bytes.TrimSpace(env.Payload)) == 0 {
		return adapters.HookEvent{}, fmt.Errorf("empty hook payload")
	}
	p, err := decodePayload(env.Payload)
	if err != nil {
		return adapters.HookEvent{}, fmt.Errorf("hook payload: %w", err)
	}
	if p.event == "" {
		p.event = env.Event
	}
	if p.event == "" {
		return adapters.HookEvent{}, fmt.Errorf("hook payload has no hookEventName")
	}
	if p.cwd == "" {
		p.cwd = env.Cwd
	}

	kind, ok := kinds[normalizeEvent(p.event)]
	if !ok {
		kind = adapters.HookUnknown
	}
	ev := adapters.HookEvent{
		Kind:             kind,
		VendorEvent:      p.event,
		AgentSessionID:   p.sessionID,
		Cwd:              p.cwd,
		TranscriptPath:   p.transcriptPath,
		ToolUseID:        p.toolUseID,
		Tool:             p.toolName,
		Input:            p.toolInput,
		PermissionMode:   p.permissionMode,
		NotificationType: p.notificationType,
	}
	if ev.Tool != "" {
		describeTool(&ev)
	}
	describeEvent(&ev, p)
	return ev, nil
}

func normalizeEvent(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// describeEvent adds the state carried by non-tool events.
func describeEvent(ev *adapters.HookEvent, p payload) {
	reason := strings.TrimSpace(firstNonEmpty(p.reason, p.message, p.error))
	switch ev.Kind {
	case adapters.HookPromptSubmit:
		ev.Message = hookkit.CapBytes(p.prompt, hookkit.MaxMessage)
		ev.Summary = "Prompt: " + hookkit.OneLine(p.prompt, 100)
	case adapters.HookStop:
		switch normalizeEvent(ev.VendorEvent) {
		case "stopfailure", "stopcancelled":
			if reason == "" {
				reason = "the turn did not finish"
			}
			ev.Message = hookkit.CapBytes(reason, hookkit.MaxMessage)
			ev.Summary = "Stopped: " + hookkit.OneLine(reason, 100)
			return
		}
		msg := strings.TrimSpace(firstNonEmpty(p.lastMessage, p.message))
		ev.Message = hookkit.CapBytes(msg, hookkit.MaxMessage)
		if msg == "" {
			ev.Summary = "Finished its turn"
		} else {
			ev.Summary = "Finished: " + hookkit.OneLine(msg, 100)
		}
	case adapters.HookSubagentStop:
		ev.Summary = withDetail("Subagent finished", firstNonEmpty(p.agentType, p.subagentType), "")
	case adapters.HookSessionStart:
		ev.Summary = withDetail("Session started", p.source, "startup")
	case adapters.HookSessionEnd:
		ev.Summary = withDetail("Session ended", p.reason, "other")
	case adapters.HookPreCompact:
		ev.Summary = withDetail("Compacting context", p.trigger, "")
	case adapters.HookNotification:
		ev.Message = hookkit.CapBytes(p.message, hookkit.MaxMessage)
		ev.Summary = firstNonEmpty(hookkit.OneLine(p.message, 120), "Notification")
	case adapters.HookUnknown:
		ev.Summary = humanEvent(p.event)
		if normalizeEvent(p.event) == "permissiondenied" {
			detail := strings.TrimSpace(firstNonEmpty(p.message, p.reason, ev.Summary))
			ev.Summary = "Permission denied"
			if detail != "" && !strings.EqualFold(detail, p.event) {
				ev.Summary += ": " + hookkit.OneLine(detail, 100)
			}
		}
	}
}

func humanEvent(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "Event"
	}
	return s
}

func withDetail(s, detail, boring string) string {
	if detail == "" || detail == boring {
		return s
	}
	return s + " (" + detail + ")"
}

// payload is the union of Grok's native camelCase fields and the
// snake_case names from Claude-compatible hook files.
type payload struct {
	event            string
	sessionID        string
	cwd              string
	transcriptPath   string
	permissionMode   string
	toolName         string
	toolInput        json.RawMessage
	toolUseID        string
	prompt           string
	message          string
	notificationType string
	lastMessage      string
	source           string
	reason           string
	trigger          string
	agentType        string
	subagentType     string
	error            string
}

func decodePayload(raw json.RawMessage) (payload, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return payload{}, err
	}
	str := func(names ...string) string {
		for _, n := range names {
			var s string
			if v, ok := m[n]; ok && json.Unmarshal(v, &s) == nil {
				return s
			}
		}
		return ""
	}
	p := payload{
		event:            str("hookEventName", "hook_event_name", "event"),
		sessionID:        str("sessionId", "session_id"),
		cwd:              str("cwd", "workspaceRoot", "workspace_root"),
		transcriptPath:   str("transcriptPath", "transcript_path"),
		permissionMode:   str("permissionMode", "permission_mode"),
		toolName:         str("toolName", "tool_name"),
		toolUseID:        str("toolUseId", "tool_use_id"),
		prompt:           str("prompt"),
		message:          str("message"),
		notificationType: str("notificationType", "notification_type"),
		lastMessage:      str("lastAssistantMessage", "last_assistant_message", "lastMessage", "last_message"),
		source:           str("source"),
		reason:           str("reason"),
		trigger:          str("trigger"),
		agentType:        str("agentType", "agent_type"),
		subagentType:     str("subagentType", "subagent_type"),
		error:            str("error"),
	}
	for _, n := range []string{"toolInput", "tool_input"} {
		if v, ok := m[n]; ok && len(bytes.TrimSpace(v)) > 0 && !bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			p.toolInput = v
			break
		}
	}
	return p, nil
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// field reads one string from a tool input, accepting the camelCase names
// Grok uses as well as snake_case from compatibility hooks.
func field(input json.RawMessage, names ...string) string {
	var m map[string]json.RawMessage
	if len(input) == 0 || json.Unmarshal(input, &m) != nil {
		return ""
	}
	for _, n := range names {
		var s string
		if v, ok := m[n]; ok && json.Unmarshal(v, &s) == nil {
			return s
		}
	}
	return ""
}

func describeTool(ev *adapters.HookEvent) {
	tool := strings.ToLower(ev.Tool)
	switch tool {
	case "bash", "shell", "terminal", "run_terminal_cmd", "run_terminal_command", "exec_command":
		ev.ToolKind = api.ToolShell
		ev.Command = firstNonEmpty(field(ev.Input, "command", "cmd", "script"))
		ev.Summary = "Run: " + hookkit.OneLine(ev.Command, 160)
		if p, exact := hookkit.ShellPatch(ev.Command, ev.Cwd); len(p) > 0 {
			ev.Paths = p
			if exact {
				ev.ToolKind = api.ToolEdit
				ev.Summary = editSummary(ev.Paths, ev.Cwd)
			}
		}
	case "search_replace", "hashline_edit", "edit", "multi_edit", "multiedit", "write", "write_file",
		"edit_file", "apply_patch", "str_replace_editor", "str_replace", "edit_notebook", "notebook_edit":
		ev.ToolKind = api.ToolEdit
		if tool == "apply_patch" {
			patch := firstNonEmpty(field(ev.Input, "command", "patch", "input"), ev.Command)
			ev.Paths = hookkit.PatchPaths(patch, ev.Cwd)
		}
		if len(ev.Paths) == 0 {
			ev.Paths = pathValues(field(ev.Input, "file_path", "filePath", "path", "file", "filename", "target_file", "target_notebook"))
		}
		ev.Summary = editSummary(ev.Paths, ev.Cwd)
	case "read_file", "read", "view":
		ev.ToolKind = api.ToolRead
		ev.Paths = pathValues(field(ev.Input, "file_path", "filePath", "path"))
		ev.Summary = "Read: " + pathsSummary(ev.Paths, ev.Cwd)
	case "grep", "grep_search", "search":
		ev.ToolKind = api.ToolRead
		dir := field(ev.Input, "path", "dir", "directory")
		ev.Paths = pathValues(dir)
		ev.Summary = searchSummary("Search", field(ev.Input, "pattern", "query"), dir)
	case "list_dir", "ls", "list_files", "glob":
		ev.ToolKind = api.ToolRead
		dir := field(ev.Input, "path", "dir", "directory")
		ev.Paths = pathValues(dir)
		ev.Summary = searchSummary("List", "", dir)
	case "web_fetch":
		ev.ToolKind = api.ToolWeb
		ev.Summary = "Fetch: " + hookkit.Truncate(field(ev.Input, "url"), 160)
	case "web_search":
		ev.ToolKind = api.ToolWeb
		ev.Summary = "Search the web: " + hookkit.OneLine(field(ev.Input, "query"), 120)
	case "task", "agent", "spawn_subagent":
		ev.ToolKind = api.ToolOther
		ev.Summary = "Start subagent"
		if d := firstNonEmpty(field(ev.Input, "description", "prompt", "agent_type", "agentType")); d != "" {
			ev.Summary += ": " + hookkit.OneLine(d, 100)
		}
	case "use_tool":
		ev.ToolKind = api.ToolOther
		ev.Summary = "Use tool"
		if n := firstNonEmpty(field(ev.Input, "tool_name", "toolName", "name")); n != "" {
			ev.Summary += ": " + hookkit.OneLine(n, 100)
		}
	case "search_tool":
		ev.ToolKind = api.ToolOther
		ev.Summary = "Find a tool: " + hookkit.OneLine(field(ev.Input, "query"), 100)
	default:
		if server, name, ok := hookkit.MCPName(ev.Tool); ok {
			ev.ToolKind = api.ToolMCP
			ev.Summary = "MCP " + server + ": " + name
			return
		}
		// Grok namespaces MCP tools as server__tool (without mcp__).
		if server, name, ok := strings.Cut(ev.Tool, "__"); ok && server != "" && name != "" {
			ev.ToolKind = api.ToolMCP
			ev.Summary = "MCP " + server + ": " + name
			return
		}
		ev.ToolKind = api.ToolOther
		ev.Summary = "Use " + ev.Tool
		for _, f := range []string{"description", "message", "prompt", "subject", "query"} {
			if v := strings.TrimSpace(field(ev.Input, f)); v != "" {
				ev.Summary = ev.Tool + ": " + hookkit.OneLine(v, 120)
				break
			}
		}
	}
}

func searchSummary(verb, pattern, dir string) string {
	s := verb
	if pattern != "" {
		s += ": " + hookkit.OneLine(pattern, 100)
	}
	if dir != "" {
		if pattern != "" {
			s += " in " + hookkit.RelPath("", dir)
		} else {
			s += ": " + hookkit.RelPath("", dir)
		}
	}
	return s
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

func pathsSummary(files []string, cwd string) string {
	if len(files) == 0 {
		return "a file"
	}
	s := hookkit.RelPath(cwd, files[0])
	if len(files) > 1 {
		s += fmt.Sprintf(" (+%d more)", len(files)-1)
	}
	return s
}

func pathValues(p string) []string {
	if strings.TrimSpace(p) == "" {
		return nil
	}
	return []string{p}
}
