package state

import (
	"strings"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/api"
)

// FromHook maps a normalized hook event to the state it is evidence of
// (BUILD-SPEC §7 mapping). ok is false for hooks that say nothing about the
// session's state, such as an unrecognised notification.
func FromHook(ev adapters.HookEvent) (st api.State, kind api.BlockKind, detail string, ok bool) {
	switch ev.Kind {
	case adapters.HookSessionStart:
		return api.StateStarting, "", "Session started", true
	case adapters.HookPreTool:
		return api.StateWorking, "", orDefault(ev.Summary, "Using "+ev.Tool), true
	case adapters.HookPostTool:
		return api.StateWorking, "", orDefault(ev.Summary, "Used "+ev.Tool), true
	case adapters.HookPromptSubmit:
		return api.StateWorking, "", "Working on a prompt", true
	case adapters.HookPreCompact:
		return api.StateWorking, "", "Working", true
	case adapters.HookSubagentStop:
		// A subagent finishing says nothing about the parent: Claude Code
		// runs one (prompt suggestions) right after Stop, and treating it
		// as work kept idle sessions "working" for a minute.
		return "", "", "", false
	case adapters.HookStop:
		return api.StateWaiting, "", "Finished its turn", true
	case adapters.HookSessionEnd:
		return api.StateExited, "", "Session ended", true
	case adapters.HookPermission:
		return api.StateBlocked, api.BlockPermission, "Asked to " + lowerFirst(orDefault(ev.Summary, "use "+ev.Tool)), true
	case adapters.HookNotification:
		return fromNotification(ev)
	}
	return "", "", "", false
}

// fromNotification classifies a notification by its type (Claude Code:
// permission_prompt, idle_prompt, elicitation_dialog, auth_success).
func fromNotification(ev adapters.HookEvent) (api.State, api.BlockKind, string, bool) {
	t := strings.ToLower(ev.NotificationType)
	msg := ev.Message
	switch {
	case strings.Contains(t, "permission"):
		return api.StateBlocked, api.BlockPermission, orDefault(msg, "Waiting for permission"), true
	case IsIdleNotification(ev):
		return api.StateWaiting, "", orDefault(msg, "Waiting for input"), true
	case strings.Contains(t, "elicitation"), strings.Contains(t, "question"):
		return api.StateBlocked, api.BlockQuestion, orDefault(msg, "Asked a question"), true
	case strings.HasPrefix(t, "auth") && !strings.Contains(t, "success"):
		return api.StateBlocked, api.BlockAuth, orDefault(msg, "Needs to sign in"), true
	}
	return "", "", "", false
}

// IsIdleNotification reports whether ev says the agent is idle at its
// prompt, waiting for input (Claude Code: idle_prompt).
func IsIdleNotification(ev adapters.HookEvent) bool {
	return ev.Kind == adapters.HookNotification && strings.Contains(strings.ToLower(ev.NotificationType), "idle")
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}
