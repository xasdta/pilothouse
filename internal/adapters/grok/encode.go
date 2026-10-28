package grok

import (
	"encoding/json"
	"strings"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/hookkit"
	"github.com/xasdta/pilothouse/internal/api"
)

// AskPrefix starts the reason of a PreToolUse "ask".
const AskPrefix = "Pilothouse: approval required"

// EncodeResponse answers Grok within its hook contract
// (https://docs.x.ai/build/features/hooks):
//
//   - PreToolUse denies with {"decision":"deny","reason":"…"} and asks with
//     {"decision":"ask","reason":"…"}. An ask forces Grok's own permission
//     prompt even when a rule, a saved grant or auto mode would have
//     approved the call. allow is no opinion: Grok's own permission mode
//     still decides.
//   - UserPromptSubmit and passive events ignore stdout, so there is
//     nothing useful to send for them.
//
// A deny must never be expressed as "no output": that would let the call
// run. Grok also fails open on malformed output or a timeout, which is why
// the shim gives up quickly and the installed timeout is short.
func (Adapter) EncodeResponse(ev adapters.HookEvent, out adapters.HookOutcome) adapters.HookResponse {
	if ev.Kind != adapters.HookPreTool {
		return hookkit.NoOpinion
	}
	switch out.Behavior {
	case api.BehaviorDeny:
		return decisionResponse("deny", firstNonEmpty(out.Message, hookkit.DefaultDenyReason))
	case api.BehaviorAsk:
		return decisionResponse("ask", askReason(out))
	}
	return hookkit.NoOpinion
}

// FailResponse: fail closed at PreToolUse when the person asked for it.
func (Adapter) FailResponse(kind adapters.HookKind, failClosed bool) adapters.HookResponse {
	if failClosed && kind == adapters.HookPreTool {
		return decisionResponse("deny", hookkit.FailClosedReason)
	}
	return hookkit.NoOpinion
}

// decisionResponse writes Grok's top-level decision form, shared by its
// native hooks and Claude-compatible hook files.
func decisionResponse(decision, reason string) adapters.HookResponse {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = hookkit.DefaultDenyReason
	}
	reason = strings.Join(strings.Fields(reason), " ")
	raw, err := json.Marshal(struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}{decision, reason})
	if err != nil {
		return hookkit.NoOpinion
	}
	return adapters.HookResponse{Stdout: string(raw) + "\n"}
}

// askReason words a PreToolUse ask for Grok's permission prompt.
func askReason(out adapters.HookOutcome) string {
	why := strings.Join(strings.Fields(out.Message), " ")
	if why == "" {
		return AskPrefix + "."
	}
	return AskPrefix + ": " + why
}
