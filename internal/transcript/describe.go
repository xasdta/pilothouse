package transcript

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/claude"
	"github.com/xasdta/pilothouse/internal/adapters/codex"
	"github.com/xasdta/pilothouse/internal/api"
)

var (
	claudeAdapter = claude.New()
	codexAdapter  = codex.New()
)

// described is what the agent's adapter says about a tool call: the same
// summary ("Run: npm test", "Edit: x.go") an approval card shows.
type described struct {
	kind    api.ToolKind
	summary string
	command string
}

// describe runs the adapter's own hook parser on a synthetic PreToolUse
// payload, so the feed and the inbox describe tool calls identically.
func describe(agent api.AgentKind, tool string, input json.RawMessage, cwd string) described {
	var ad adapters.Adapter
	switch agent {
	case api.AgentClaude:
		ad = claudeAdapter
	case api.AgentCodex:
		ad = codexAdapter
	}
	if len(input) == 0 || input[0] != '{' {
		input = json.RawMessage(`{}`)
	}
	d := described{kind: api.ToolOther, summary: "Use " + tool}
	if ad == nil || tool == "" {
		return d
	}
	payload, err := json.Marshal(struct {
		Event string          `json:"hook_event_name"`
		Tool  string          `json:"tool_name"`
		Input json.RawMessage `json:"tool_input"`
		Cwd   string          `json:"cwd"`
	}{"PreToolUse", tool, input, cwd})
	if err != nil {
		return d
	}
	ev, err := ad.ParseHook(adapters.HookEnvelope{Agent: agent, Event: "PreToolUse", Payload: payload, Cwd: cwd})
	if err != nil {
		return d
	}
	if ev.ToolKind != "" {
		d.kind = ev.ToolKind
	}
	if ev.Summary != "" {
		d.summary = ev.Summary
	}
	d.command = ev.Command
	return d
}

// testCommand recognises test runs among shell commands, for catch-up's
// "Tests: npm test passed". The match is the runner as people say it.
var testCommand = regexp.MustCompile(`(?:^|[\s;&|(])((?:npm|pnpm|yarn|bun)(?:\s+run)?\s+test(?::[\w-]+)?|go\s+test|cargo\s+(?:test|nextest)|python3?\s+-m\s+(?:pytest|unittest)|pytest|jest|vitest|mocha|rspec|(?:bundle\s+exec\s+)?rake\s+(?:test|spec)|mvn(?:w)?\s+(?:[\w:.=-]+\s+)*test|(?:\./)?gradlew?\s+(?:[\w:.=-]+\s+)*test|make\s+(?:[\w.=-]+\s+)*(?:test|check)|ctest|tox|phpunit|dotnet\s+test|mix\s+test|deno\s+test|swift\s+test|bazel\s+test|nox)(?:\s|$|;|&|\|)`)

// testRunner returns how to name the test run in command, or "".
func testRunner(command string) string {
	m := testCommand.FindStringSubmatch(command)
	if m == nil {
		return ""
	}
	return strings.Join(strings.Fields(m[1]), " ")
}
