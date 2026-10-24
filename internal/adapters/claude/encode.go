package claude

import (
	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/hookkit"
	"github.com/xasdta/pilothouse/internal/api"
)

// EncodeResponse answers Claude Code. Every answer exits 0 with JSON on
// stdout (or nothing): exit 2 would also block, but JSON carries the
// reason and is the documented structured form.
//
//   - pre_tool:   hookSpecificOutput.permissionDecision allow | deny | ask;
//     never allow for AskUserQuestion / ExitPlanMode, where an allow does
//     not answer the question (or would approve the plan unseen): those
//     are left to the permission checkpoint
//   - permission: hookSpecificOutput.decision.behavior allow | deny ("ask"
//     is meaningless here: no decision lets the native dialog show);
//     questions and plans are answered with decision.updatedInput
//     (questions.go)
//   - session_start, prompt_submit: additionalContext; prompt_submit deny
//     uses decision:block
//   - everything else: no output (those events have no decision control
//     we use; Stop's decision:block would keep the agent running)
func (Adapter) EncodeResponse(ev adapters.HookEvent, out adapters.HookOutcome) adapters.HookResponse {
	switch ev.Kind {
	case adapters.HookPreTool:
		if out.Behavior == api.BehaviorAllow && asksPerson(ev.Tool) {
			out.Behavior, out.Message = "", ""
		}
		return hookkit.PreToolResponse(out.Behavior, out.Message, out.Context)
	case adapters.HookPermission:
		if asksPerson(ev.Tool) {
			return answerAsk(ev, out)
		}
		if out.Behavior == api.BehaviorAllow || out.Behavior == api.BehaviorDeny {
			return hookkit.PermissionResponse(out.Behavior, out.Message)
		}
	case adapters.HookPromptSubmit:
		if out.Behavior == api.BehaviorDeny {
			return hookkit.BlockResponse(out.Message)
		}
		return hookkit.ContextResponse("UserPromptSubmit", out.Context)
	case adapters.HookSessionStart:
		return hookkit.ContextResponse("SessionStart", out.Context)
	}
	return hookkit.NoOpinion
}

// FailResponse: no opinion (Claude Code proceeds as if Pilothouse were
// absent), except a deny at pre_tool when approvals.fail_closed is on.
func (Adapter) FailResponse(kind adapters.HookKind, failClosed bool) adapters.HookResponse {
	return hookkit.FailResponse(kind, failClosed)
}
