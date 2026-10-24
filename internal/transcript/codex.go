package transcript

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

// Codex rollouts ($CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl, read
// the way codex-rs/rollout writes them): one {timestamp, type, payload}
// per line. The feed comes from "response_item" (message, function_call,
// custom_tool_call, local_shell_call, web_search_call and their outputs),
// usage from "event_msg" token_count, the model from "turn_context", turn
// ends from task_complete / turn_aborted. The legacy event_msg copies of
// messages (user_message, agent_message) are skipped: response_item has
// them already.

type codexLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexPayload struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Arguments json.RawMessage `json:"arguments"`
	CallID    string          `json:"call_id"`
	Input     json.RawMessage `json:"input"`
	Output    json.RawMessage `json:"output"`
	Action    json.RawMessage `json:"action"`
	Info      *struct {
		Total codexTokens `json:"total_token_usage"`
		Last  codexTokens `json:"last_token_usage"`
	} `json:"info"`
	Message json.RawMessage `json:"message"`
	Reason  json.RawMessage `json:"reason"`
	Model   string          `json:"model"`
	Cwd     string          `json:"cwd"`
}

type codexTokens struct {
	Input      int64 `json:"input_tokens"`
	Cached     int64 `json:"cached_input_tokens"`
	CacheWrite int64 `json:"cache_write_input_tokens"`
	Output     int64 `json:"output_tokens"`
	Reasoning  int64 `json:"reasoning_output_tokens"`
	Total      int64 `json:"total_tokens"`
}

// tokens converts OpenAI's accounting (cached and cache-write tokens are
// part of input, reasoning part of output) to Tokens.
func (c codexTokens) tokens() Tokens {
	return Tokens{
		Input:      max(0, c.Input-c.Cached-c.CacheWrite),
		CacheRead:  c.Cached,
		CacheWrite: c.CacheWrite,
		Output:     c.Output,
		Reasoning:  c.Reasoning,
	}
}

type codexParser struct {
	f         *feed
	lastAt    time.Time
	model     string       // from the latest turn_context
	total     *codexTokens // last total_token_usage counted
	turnStart time.Time
}

func (p *codexParser) line(b []byte, off int64) {
	var l codexLine
	if json.Unmarshal(b, &l) != nil {
		return
	}
	at := p.when(l.Timestamp)
	if len(l.Payload) == 0 {
		// Rollouts from before the {timestamp,type,payload} envelope wrote
		// bare response items.
		l.Payload, l.Type = b, "response_item"
	}
	var pl codexPayload
	if json.Unmarshal(l.Payload, &pl) != nil {
		return
	}
	id := "c" + strconv.FormatInt(off, 10) // a line's offset is stable: rollouts only grow
	switch l.Type {
	case "session_meta":
		if p.f.cwd == "" {
			p.f.cwd = pl.Cwd
		}
	case "turn_context":
		if pl.Model != "" {
			p.model = pl.Model
		}
	case "response_item":
		p.responseItem(pl, id, at)
	case "event_msg":
		p.event(pl, id, at)
	case "compacted":
		p.f.add(&item{ActivityItem: api.ActivityItem{ID: id, Kind: api.ActivitySystem, At: at,
			Text: "Compacted the conversation"}})
	}
}

func (p *codexParser) when(ts string) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		p.lastAt = t.UTC()
	}
	return p.lastAt
}

func (p *codexParser) responseItem(pl codexPayload, id string, at time.Time) {
	switch pl.Type {
	case "message":
		text := codexText(pl.Content)
		switch pl.Role {
		case "user":
			p.userMessage(text, id, at)
		case "assistant":
			if strings.TrimSpace(text) == "" {
				return
			}
			p.f.settle("") // the model answered: its tool calls returned
			p.f.add(&item{ActivityItem: api.ActivityItem{ID: id, Kind: api.ActivityText, At: at, Text: strings.TrimSpace(text)}})
		}
	case "function_call", "custom_tool_call", "local_shell_call":
		p.toolCall(pl, id, at)
	case "function_call_output", "custom_tool_call_output":
		text, exit := codexOutput(pl.Output)
		p.f.result(pl.CallID, text, false, exit)
	case "web_search_call":
		var a struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(pl.Action, &a)
		s := "Search the web"
		if a.Query != "" {
			s += ": " + oneLine(a.Query)
		}
		p.f.add(&item{ActivityItem: api.ActivityItem{ID: id, Kind: api.ActivityTool, At: at,
			Tool: "web_search", ToolKind: api.ToolWeb, Summary: s}, fold: true})
	case "compaction", "context_compaction":
		p.f.add(&item{ActivityItem: api.ActivityItem{ID: id, Kind: api.ActivitySystem, At: at,
			Text: "Compacted the conversation"}})
	}
}

// codexContextTag matches the wrappers Codex puts around the context it
// injects as user messages (<environment_context>, <user_instructions>,
// <turn_aborted> …): those aren't prompts.
var codexContextTag = regexp.MustCompile(`^<([a-z][a-z0-9_-]*)>`)

func (p *codexParser) userMessage(text, id string, at time.Time) {
	text = strings.TrimSpace(text)
	switch {
	case text == "", strings.HasPrefix(text, "# AGENTS.md instructions"):
		return
	case strings.HasPrefix(text, "<turn_aborted>"):
		p.f.settle("")
		p.f.add(&item{ActivityItem: api.ActivityItem{ID: id, Kind: api.ActivitySystem, At: at, Text: "Interrupted by the person"}})
		return
	case codexContextTag.MatchString(text):
		return
	}
	p.f.prompt(at)
	p.f.add(&item{ActivityItem: api.ActivityItem{ID: id, Kind: api.ActivityPrompt, At: at, Text: text, Source: "human"}})
}

// codexText joins a message's input_text / output_text items.
func codexText(raw json.RawMessage) string {
	var items []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &items) != nil {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var parts []string
	for _, it := range items {
		switch it.Type {
		case "input_text", "output_text", "text":
			parts = append(parts, it.Text)
		case "input_image":
			parts = append(parts, "[image]")
		}
	}
	return strings.Join(parts, "\n")
}

// rawString decodes a JSON string field that may hold JSON itself
// (function_call.arguments) or plain text (custom_tool_call.input).
func rawString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(bytes.TrimSpace(raw))
}

func (p *codexParser) toolCall(pl codexPayload, id string, at time.Time) {
	callID := firstNonEmpty(pl.CallID, id)
	name := pl.Name
	if pl.Namespace != "" {
		name = strings.TrimSuffix(pl.Namespace, "__") + "__" + pl.Name // mcp__server__tool
	}
	var args map[string]json.RawMessage
	_ = json.Unmarshal([]byte(rawString(pl.Arguments)), &args)
	str := func(k string) string {
		var s string
		_ = json.Unmarshal(args[k], &s)
		return s
	}
	words := func(raw json.RawMessage) string {
		var argv []string
		if json.Unmarshal(raw, &argv) != nil {
			return ""
		}
		if len(argv) == 3 && (argv[1] == "-lc" || argv[1] == "-c") {
			return argv[2] // bash -lc '<script>'
		}
		return strings.Join(argv, " ")
	}
	var (
		hookTool  = name
		hookInput json.RawMessage
		command   string
		patch     string
	)
	switch {
	case pl.Type == "local_shell_call":
		var a struct {
			Command json.RawMessage `json:"command"`
		}
		_ = json.Unmarshal(pl.Action, &a)
		hookTool, command = "Bash", words(a.Command)
	case name == "exec_command":
		hookTool, command = "Bash", str("cmd")
	case name == "shell":
		hookTool, command = "Bash", words(args["command"])
	case name == "shell_command":
		hookTool, command = "Bash", str("command")
	case name == "apply_patch":
		patch = rawString(pl.Input)
		if pl.Type == "function_call" {
			patch = firstNonEmpty(str("input"), str("patch"))
		}
		hookInput, _ = json.Marshal(map[string]string{"command": patch})
	case name == "write_stdin":
		hookInput, _ = json.Marshal(map[string]string{"chars": str("chars")})
	default:
		hookInput = json.RawMessage(rawString(pl.Arguments))
	}
	if hookTool == "Bash" {
		hookInput, _ = json.Marshal(map[string]string{"command": command})
		patch = patchBody(command)
	}
	d := describe(api.AgentCodex, hookTool, hookInput, p.f.cwd)
	it := &item{ActivityItem: api.ActivityItem{ID: callID, Kind: api.ActivityTool, At: at, Tool: name,
		ToolKind: d.kind, Summary: d.summary, Pending: true}, command: d.command}
	switch name {
	case "update_plan":
		it.Summary = "Updated its plan"
	case "spawn_agent":
		it.Kind = api.ActivitySubagent
	case "request_user_input":
		it.asks = 1
		it.Summary = "Asked you a question (answer in the terminal)"
	}
	if added := p.f.add(it); added == it && patch != "" {
		it.edits = patchEdits(patch, p.f.cwd)
		if len(it.edits) > 0 {
			it.ToolKind = api.ToolEdit
		}
		p.f.applyEdits(it.edits, 1)
	}
}

// exitCode finds the exit status Codex writes into a command's output.
var exitCode = regexp.MustCompile(`(?m)^(?:Process exited with code|Exit code:)\s*(-?\d+)`)

// codexOutput reads a call's output: plain text, content items, or the
// older {"output": …, "metadata": {"exit_code": n}} JSON in a string.
func codexOutput(raw json.RawMessage) (string, *int) {
	text := rawString(raw)
	if raw = bytes.TrimSpace(raw); len(raw) > 0 && raw[0] == '[' {
		text = codexText(raw)
	}
	var wrapped struct {
		Output   *string `json:"output"`
		Metadata struct {
			ExitCode *int `json:"exit_code"`
		} `json:"metadata"`
	}
	if strings.HasPrefix(strings.TrimSpace(text), "{") && json.Unmarshal([]byte(text), &wrapped) == nil && wrapped.Output != nil {
		text = *wrapped.Output
		if wrapped.Metadata.ExitCode != nil {
			return text, wrapped.Metadata.ExitCode
		}
	}
	if m := exitCode.FindStringSubmatch(text); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return codexOutputBody(text), &n
		}
	}
	return text, nil
}

// codexOutputBody drops the "Wall time: … Output:" header Codex puts
// before a command's output.
func codexOutputBody(text string) string {
	if i := strings.Index(text, "\nOutput:\n"); i >= 0 && i < 400 {
		return text[i+len("\nOutput:\n"):]
	}
	return text
}

func (p *codexParser) event(pl codexPayload, id string, at time.Time) {
	switch pl.Type {
	case "token_count":
		if pl.Info == nil {
			return
		}
		// token_count is repeated (rate-limit refreshes) with an unchanged
		// total: only a new total is a new response, and its "last" usage
		// is that response's.
		if p.total != nil && *p.total == pl.Info.Total {
			return
		}
		tot := pl.Info.Total
		p.total = &tot
		if t := pl.Info.Last.tokens(); !t.Zero() {
			p.f.addUsage("", p.model, at, t, false, true)
		}
	case "task_started", "turn_started":
		p.turnStart = at
	case "task_complete", "turn_complete":
		p.f.settle("")
		s := "Finished its turn"
		if !p.turnStart.IsZero() && at.After(p.turnStart) {
			s += " after " + shortDuration(at.Sub(p.turnStart))
		}
		p.turnStart = time.Time{}
		p.f.add(&item{ActivityItem: api.ActivityItem{ID: id, Kind: api.ActivitySystem, At: at, Text: s}})
	case "turn_aborted":
		p.f.settle("")
		s := "Interrupted by the person"
		if r := rawString(pl.Reason); r != "" && r != "interrupted" {
			s = "Turn ended: " + strings.ReplaceAll(r, "_", " ")
		}
		p.f.add(&item{ActivityItem: api.ActivityItem{ID: id, Kind: api.ActivitySystem, At: at, Text: s}})
	case "error":
		p.f.add(&item{ActivityItem: api.ActivityItem{ID: id, Kind: api.ActivityError, At: at,
			Text: firstNonEmpty(strings.TrimSpace(rawString(pl.Message)), "Codex reported an error")}})
	}
}
