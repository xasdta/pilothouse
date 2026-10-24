package hookkit

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/api"
)

// NoOpinion is exit 0 with no output: the agent carries on as if no hook
// were installed.
var NoOpinion = adapters.HookResponse{}

// DefaultDenyReason is used when a deny carries no message: both agents
// require a non-empty reason (Codex rejects a deny without one).
const DefaultDenyReason = "Blocked by Pilothouse policy."

// FailClosedReason is shown when the daemon is unreachable and
// approvals.fail_closed is on.
const FailClosedReason = "Pilothouse is not running and approvals.fail_closed is on, so tool calls are blocked. Start it with `pilothouse start`, or set approvals.fail_closed: false."

type preToolOutput struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	AdditionalContext        string `json:"additionalContext,omitempty"`
}

type permissionDecision struct {
	Behavior     string          `json:"behavior"`
	Message      string          `json:"message,omitempty"`
	UpdatedInput json.RawMessage `json:"updatedInput,omitempty"`
}

type permissionOutput struct {
	HookEventName string             `json:"hookEventName"`
	Decision      permissionDecision `json:"decision"`
}

type contextOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

type envelope struct {
	Decision           string `json:"decision,omitempty"`
	Reason             string `json:"reason,omitempty"`
	HookSpecificOutput any    `json:"hookSpecificOutput,omitempty"`
}

// PreToolResponse answers a PreToolUse hook with
// hookSpecificOutput.permissionDecision (allow | deny | ask).
func PreToolResponse(b api.Behavior, reason, context string) adapters.HookResponse {
	out := preToolOutput{HookEventName: "PreToolUse", AdditionalContext: context}
	if b != "" {
		out.PermissionDecision = string(b)
		out.PermissionDecisionReason = reason
		if b == api.BehaviorDeny && strings.TrimSpace(reason) == "" {
			out.PermissionDecisionReason = DefaultDenyReason
		}
	}
	if out.PermissionDecision == "" && out.AdditionalContext == "" {
		return NoOpinion
	}
	return jsonResponse(envelope{HookSpecificOutput: out})
}

// PermissionResponse answers a PermissionRequest hook with
// hookSpecificOutput.decision.behavior (allow | deny).
func PermissionResponse(b api.Behavior, message string) adapters.HookResponse {
	d := permissionDecision{Behavior: string(b)}
	if b == api.BehaviorDeny {
		d.Message = message
		if strings.TrimSpace(message) == "" {
			d.Message = "Denied through Pilothouse."
		}
	}
	return jsonResponse(envelope{HookSpecificOutput: permissionOutput{HookEventName: "PermissionRequest", Decision: d}})
}

// PermissionAllowInput answers a PermissionRequest hook with allow and
// decision.updatedInput: the tool runs with input instead of what the
// agent asked for. Claude Code takes the answers to AskUserQuestion and
// the approval of ExitPlanMode this way. input must be a JSON object.
func PermissionAllowInput(input json.RawMessage) adapters.HookResponse {
	d := permissionDecision{Behavior: string(api.BehaviorAllow), UpdatedInput: input}
	return jsonResponse(envelope{HookSpecificOutput: permissionOutput{HookEventName: "PermissionRequest", Decision: d}})
}

// ContextResponse adds context for the model (SessionStart,
// UserPromptSubmit).
func ContextResponse(event, context string) adapters.HookResponse {
	if context == "" {
		return NoOpinion
	}
	return jsonResponse(envelope{HookSpecificOutput: contextOutput{HookEventName: event, AdditionalContext: context}})
}

// BlockResponse uses the top-level decision:block form (UserPromptSubmit).
func BlockResponse(reason string) adapters.HookResponse {
	if strings.TrimSpace(reason) == "" {
		reason = DefaultDenyReason
	}
	return jsonResponse(envelope{Decision: "block", Reason: reason})
}

func jsonResponse(v any) adapters.HookResponse {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return NoOpinion
	}
	return adapters.HookResponse{Stdout: b.String()}
}

// FailResponse is what the shim prints when the daemon cannot answer.
func FailResponse(kind adapters.HookKind, failClosed bool) adapters.HookResponse {
	if failClosed && kind == adapters.HookPreTool {
		return PreToolResponse(api.BehaviorDeny, FailClosedReason, "")
	}
	return NoOpinion
}

// DetectBinary looks up an agent's binary on PATH and asks it for its
// version (first line of `<bin> --version`, 3 s budget).
func DetectBinary(name string) (path, version string) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return path, ""
	}
	version, _, _ = strings.Cut(strings.TrimSpace(string(out)), "\n")
	return path, version
}
