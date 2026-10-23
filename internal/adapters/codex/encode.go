package codex

import (
	"strings"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/hookkit"
	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/policy"
)

// EncodeResponse answers Codex within what its hook runner accepts
// (codex-rs/hooks/src/events/*.rs and engine/output_parser.rs).
//
// Codex runs hooks through the person's shell ($SHELL -c in 0.164, -lc on
// main), so whatever a shell startup file prints lands on stdout in front
// of our JSON, Codex can no longer parse it, and the answer is lost. Every
// deny is therefore sent as exit code 2 with the reason on stderr, which
// Codex honors for PreToolUse, PermissionRequest and UserPromptSubmit
// whatever stdout holds (the `Some(2)` arms in pre_tool_use.rs,
// permission_request.rs, user_prompt_submit.rs). Only answers that have no
// exit-code form use JSON: a PermissionRequest allow and additionalContext.
// If those are garbled, Codex falls back to its own prompt (fail safe);
// `pilothouse doctor` warns about a noisy shell (ShellNoise).
//
//   - pre_tool: deny → exit 2. Codex has no "ask" at PreToolUse, and
//     ignoring it would let the call run without the person a rule
//     demands, so ask is also sent as a deny that tells the agent a person
//     must do it (see askReason). allow is unsupported (Codex reports the
//     hook as failed), so it prints nothing and Codex's own approval
//     policy applies.
//   - permission: allow → decision.behavior JSON; deny → exit 2. No
//     updatedInput/updatedPermissions/interrupt: Codex fails closed on them.
//   - session_start, prompt_submit: additionalContext; prompt_submit deny
//     → exit 2.
//   - everything else: nothing.
func (Adapter) EncodeResponse(ev adapters.HookEvent, out adapters.HookOutcome) adapters.HookResponse {
	switch ev.Kind {
	case adapters.HookPreTool:
		switch out.Behavior {
		case api.BehaviorDeny:
			return denyExit(preToolReason(out.Message, hookkit.DefaultDenyReason))
		case api.BehaviorAsk:
			return denyExit(preToolReason(askReason(out), ""))
		}
		return hookkit.PreToolResponse("", "", out.Context)
	case adapters.HookPermission:
		switch out.Behavior {
		case api.BehaviorAllow:
			return hookkit.PermissionResponse(api.BehaviorAllow, "")
		case api.BehaviorDeny:
			return denyExit(firstNonEmpty(out.Message, "Denied through Pilothouse."))
		}
	case adapters.HookPromptSubmit:
		if out.Behavior == api.BehaviorDeny {
			return denyExit(firstNonEmpty(out.Message, hookkit.DefaultDenyReason))
		}
		return hookkit.ContextResponse("UserPromptSubmit", out.Context)
	case adapters.HookSessionStart:
		return hookkit.ContextResponse("SessionStart", out.Context)
	}
	return hookkit.NoOpinion
}

// FailResponse: no opinion, except a deny at pre_tool when
// approvals.fail_closed is on (sent as exit 2, like every Codex deny).
func (Adapter) FailResponse(kind adapters.HookKind, failClosed bool) adapters.HookResponse {
	if failClosed && kind == adapters.HookPreTool {
		return denyExit(preToolReason(hookkit.FailClosedReason, ""))
	}
	return hookkit.NoOpinion
}

// AskPrefix starts the reason of a pre_tool "ask" that Codex receives as a
// deny.
const AskPrefix = "Pilothouse: a person must approve this"

// askReason words a pre_tool ask for Codex, which cannot show a prompt
// from PreToolUse. The agent should hand the step to the person: the
// built-in self-protection rule cannot be loosened by any rule, so then the
// person must do it themselves; for the person's own ask rules they may
// also change the rule.
func askReason(out adapters.HookOutcome) string {
	why := strings.TrimSpace(out.Message)
	selfProtect := out.Rule == policy.SelfProtectionRule ||
		(out.Rule == "" && strings.HasPrefix(why, "Pilothouse self-protection:"))
	s := AskPrefix + ", and Codex cannot ask them from a hook — "
	if selfProtect {
		s += "ask the user to run it themselves"
	} else {
		s += "ask the user to run it themselves or allow it with a Pilothouse rule"
	}
	if why != "" {
		s += " (" + strings.TrimRight(why, ". ") + ")"
	}
	return s
}

// preToolReason trims the final period: Codex shows a PreToolUse block as
// "Command blocked by PreToolUse hook: <reason>. Command: …"
// (codex-rs/core/src/hook_runtime.rs), which would double it.
func preToolReason(reason, fallback string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = fallback
	}
	return strings.TrimRight(reason, ". ")
}

// denyExit is a deny Codex reads from the exit status alone: code 2, the
// reason on stderr (which must not be empty, or Codex reports the hook as
// failed and lets the call run).
func denyExit(reason string) adapters.HookResponse {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = strings.TrimRight(hookkit.DefaultDenyReason, ".")
	}
	return adapters.HookResponse{Stderr: reason + "\n", ExitCode: 2}
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
