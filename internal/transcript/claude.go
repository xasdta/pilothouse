package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

// Claude Code transcripts (undocumented; read the way Claude Code 2.1.x
// writes them): one JSON record per line with type, uuid, timestamp,
// isSidechain. "user" records carry prompts or tool results; "assistant"
// records carry ONE content block each (text, thinking, tool_use), the
// blocks of one response sharing message.id and repeating its usage;
// "system" records mark compaction, turn ends and API errors. Everything
// else (api-request*, file-history-*, ai-title, permission-mode,
// queue-operation, attachments …) is skipped, as is any record type this
// reader doesn't know.

type claudeRecord struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	UUID             string          `json:"uuid"`
	Timestamp        string          `json:"timestamp"`
	IsSidechain      bool            `json:"isSidechain"`
	IsMeta           bool            `json:"isMeta"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	IsAPIError       bool            `json:"isApiErrorMessage"`
	Origin           json.RawMessage `json:"origin"`
	Message          json.RawMessage `json:"message"`
	ToolUseResult    json.RawMessage `json:"toolUseResult"`
	Content          json.RawMessage `json:"content"`
	DurationMs       float64         `json:"durationMs"`
	CompactMetadata  json.RawMessage `json:"compactMetadata"`
}

type claudeMessage struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   json.RawMessage `json:"usage"`
}

type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type claudeUsage struct {
	InputTokens   int64 `json:"input_tokens"`
	OutputTokens  int64 `json:"output_tokens"`
	CacheCreation int64 `json:"cache_creation_input_tokens"`
	CacheRead     int64 `json:"cache_read_input_tokens"`
	Detail        *struct {
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	Speed string `json:"speed"`
}

// claudeKeep are the record types the feed reads.
var claudeKeep = map[string]bool{"user": true, "assistant": true, "system": true}

// claudeParser reads one Claude transcript file into a feed: the main
// conversation, or one subagent's sidechain.
type claudeParser struct {
	f         *feed
	sidechain bool   // a subagents/agent-*.jsonl file
	chain     string // pending-tool scope ("" for the main conversation)
	turn      int    // sidechains: the turn of the call that started them
	lastAt    time.Time
}

func (p *claudeParser) line(b []byte, _ int64) {
	if t, ok := leadingType(b); ok && !claudeKeep[t] {
		return // metadata records lead with their type: skip without decoding
	}
	var r claudeRecord
	if json.Unmarshal(b, &r) != nil || !claudeKeep[r.Type] {
		return
	}
	at := p.when(r.Timestamp)
	chain, side := p.chain, p.sidechain
	if r.IsSidechain && !p.sidechain {
		// Older Claude Code wrote subagent records into the main file.
		chain, side = "inline-sidechain", true
	}
	switch r.Type {
	case "user":
		p.user(r, at, chain, side)
	case "assistant":
		p.assistant(r, at, chain, side)
	case "system":
		p.system(r, at, chain, side)
	}
}

// leadingType reads the type of a record whose first key is "type".
func leadingType(b []byte) (string, bool) {
	const prefix = `{"type":"`
	if !bytes.HasPrefix(b, []byte(prefix)) {
		return "", false
	}
	rest := b[len(prefix):]
	end := bytes.IndexByte(rest, '"')
	if end < 0 || end > 64 {
		return "", false
	}
	return string(rest[:end]), true
}

func (p *claudeParser) when(ts string) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		p.lastAt = t.UTC()
	}
	return p.lastAt
}

func (p *claudeParser) newItem(id string, kind api.ActivityKind, at time.Time, chain string, side bool) *item {
	it := &item{ActivityItem: api.ActivityItem{ID: id, Kind: kind, At: at, Sidechain: side}, chain: chain}
	if side && p.turn > 0 {
		it.Turn = p.turn
	}
	return it
}

func (p *claudeParser) user(r claudeRecord, at time.Time, chain string, side bool) {
	var m claudeMessage
	if r.IsMeta || r.IsCompactSummary || json.Unmarshal(r.Message, &m) != nil {
		return
	}
	var texts []string
	images := 0
	var results []claudeBlock
	switch c := bytes.TrimSpace(m.Content); {
	case len(c) > 0 && c[0] == '"':
		var s string
		if json.Unmarshal(c, &s) == nil {
			texts = append(texts, s)
		}
	case len(c) > 0 && c[0] == '[':
		var raws []json.RawMessage
		_ = json.Unmarshal(c, &raws)
		for _, raw := range raws {
			var b claudeBlock
			if json.Unmarshal(raw, &b) != nil {
				continue
			}
			switch b.Type {
			case "text":
				texts = append(texts, b.Text)
			case "image":
				images++
			case "tool_result":
				results = append(results, b)
			}
		}
	}
	for _, b := range results {
		it := p.f.result(b.ToolUseID, resultText(b.Content), b.IsError, nil)
		if it != nil && it.IsError {
			it.denied = isDenial(it.Result)
		}
		if it == nil || it.IsError || len(results) != 1 {
			continue
		}
		// toolUseResult describes this record's single result: an edit's
		// exact patch.
		if edits, ok := claudeResultEdits(r.ToolUseResult); ok && (len(it.edits) > 0 || it.Tool == "Write") {
			p.f.setEdits(it, edits)
		}
	}
	text := strings.TrimSpace(strings.Join(texts, "\n"))
	if len(results) > 0 {
		if strings.HasPrefix(text, "[Request interrupted") {
			p.f.add(p.newItem(r.UUID, api.ActivitySystem, at, chain, side)).setText("Interrupted by the person")
		}
		return
	}
	if side {
		return // a subagent's task prompt: its Agent call already says what it is
	}
	if images > 0 {
		text = strings.TrimSpace(text + fmt.Sprintf(" [%d image%s]", images, plural(images)))
	}
	kind, shown := classifyClaudePrompt(text, originKind(r.Origin))
	switch kind {
	case promptSkip:
		return
	case promptInterrupt:
		p.f.settle(chain)
		p.f.add(p.newItem(r.UUID, api.ActivitySystem, at, chain, side)).setText("Interrupted by the person")
	case promptNotice:
		p.f.add(p.newItem(r.UUID, api.ActivitySystem, at, chain, side)).setText(shown)
	case promptHuman, promptPeer:
		p.f.prompt(at)
		it := p.newItem(r.UUID, api.ActivityPrompt, at, chain, side)
		it.Text, it.Source = shown, "human"
		if kind == promptPeer {
			it.Source = "peer"
		}
		p.f.add(it)
	}
}

// isDenial recognises the result Claude Code logs when a tool call was
// refused (by the person, a permission rule or a hook, Pilothouse's
// included) rather than run and failed.
func isDenial(result string) bool {
	for _, s := range []string{"doesn't want to proceed with this tool use", "tool use was rejected",
		"has been denied", "Blocked by Pilothouse", "Denied in Pilothouse", "Denied through Pilothouse"} {
		if strings.Contains(result, s) {
			return true
		}
	}
	return false
}

func (it *item) setText(s string) {
	if it != nil && it.Text == "" {
		it.Text = s
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

type promptKind int

const (
	promptSkip promptKind = iota
	promptHuman
	promptPeer
	promptInterrupt
	promptNotice // something the person didn't type, worth a line
)

// classifyClaudePrompt sorts the text of a user record: what a person
// typed (or another session sent, origin "peer"), slash commands, and the
// wrappers Claude Code adds for local commands and notifications.
func classifyClaudePrompt(text, origin string) (promptKind, string) {
	switch {
	case text == "":
		return promptSkip, ""
	case strings.HasPrefix(text, "[Request interrupted"):
		return promptInterrupt, ""
	case origin == "task-notification" || strings.HasPrefix(text, "<task-notification>"):
		s := "A background task reported back"
		if sum := between(text, "<summary>", "</summary>"); sum != "" {
			s += ": " + oneLine(sum)
		}
		return promptNotice, s
	case strings.HasPrefix(text, "<command-name>"):
		cmd := strings.TrimSpace(between(text, "<command-name>", "</command-name>"))
		if args := strings.TrimSpace(between(text, "<command-args>", "</command-args>")); args != "" {
			cmd += " " + args
		}
		return promptHuman, cmd
	case strings.HasPrefix(text, "<bash-input>"):
		return promptHuman, "! " + strings.TrimSpace(between(text, "<bash-input>", "</bash-input>"))
	case strings.HasPrefix(text, "<local-command-"), strings.HasPrefix(text, "<bash-std"),
		strings.HasPrefix(text, "<system-reminder>"), strings.HasPrefix(text, "<command-message>"):
		return promptSkip, ""
	case origin == "peer":
		return promptPeer, text
	}
	return promptHuman, text
}

func between(s, open, close string) string {
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	s = s[i+len(open):]
	if j := strings.Index(s, close); j >= 0 {
		return s[:j]
	}
	return s
}

// originKind reads origin.kind (human | peer | task-notification).
func originKind(raw json.RawMessage) string {
	var o struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal(raw, &o) == nil {
		return o.Kind
	}
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

// resultText flattens a tool_result's content (a string or text blocks).
func resultText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			parts = append(parts, "[image]")
		}
	}
	return strings.Join(parts, "\n")
}

func (p *claudeParser) assistant(r claudeRecord, at time.Time, chain string, side bool) {
	var m claudeMessage
	if json.Unmarshal(r.Message, &m) != nil {
		return
	}
	var blocks []claudeBlock
	var raws []json.RawMessage
	if json.Unmarshal(m.Content, &raws) == nil {
		for _, raw := range raws {
			var b claudeBlock
			if json.Unmarshal(raw, &b) == nil {
				blocks = append(blocks, b)
			}
		}
	}
	if r.IsAPIError || m.Model == "<synthetic>" {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" {
				parts = append(parts, b.Text)
			}
		}
		it := p.newItem(r.UUID, api.ActivityError, at, chain, side)
		it.Text = firstNonEmpty(strings.TrimSpace(strings.Join(parts, "\n")), "The API returned an error")
		p.f.add(it)
		return
	}
	if m.ID != "" && p.f.lastMsg[chain] != m.ID {
		// A new response: the previous one's tool calls have all returned.
		p.f.settle(chain)
		p.f.lastMsg[chain] = m.ID
	}
	p.countUsage(m, at, side)
	for i, b := range blocks {
		id := r.UUID
		if i > 0 {
			id += ":" + strconv.Itoa(i)
		}
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			it := p.newItem(id, api.ActivityText, at, chain, side)
			it.Text = strings.TrimSpace(b.Text)
			p.f.add(it)
		case "tool_use", "server_tool_use":
			p.toolUse(b, at, chain, side)
		default:
			if b.ToolUseID != "" && strings.HasSuffix(b.Type, "_tool_result") {
				// Server tools (web search) answer inside the response.
				p.f.result(b.ToolUseID, "", false, nil)
			}
		}
	}
}

func (p *claudeParser) countUsage(m claudeMessage, at time.Time, side bool) {
	if len(m.Usage) == 0 {
		return
	}
	var u claudeUsage
	if json.Unmarshal(m.Usage, &u) != nil {
		return
	}
	t := Tokens{Input: u.InputTokens, Output: u.OutputTokens, CacheRead: u.CacheRead, CacheWrite: u.CacheCreation}
	if u.Detail != nil {
		t.CacheWrite1h = min(u.Detail.Ephemeral1h, u.CacheCreation)
	}
	p.f.addUsage(m.ID, m.Model, at, t, u.Speed == "fast", !side)
}

// headLine is the pass over the start of a transcript too big to parse
// whole: it counts usage and turns only. Byte checks pick the lines worth
// decoding, so the bulk (tool output, request logs) is never decoded.
func (p *claudeParser) headLine(b []byte, _ int64) {
	usage := bytes.Contains(b, []byte(`"type":"assistant"`)) && bytes.Contains(b, []byte(`"usage"`))
	prompt := !p.sidechain && bytes.Contains(b, []byte(`"type":"user"`)) && !bytes.Contains(b, []byte(`"tool_use_id"`))
	if !usage && !prompt {
		return
	}
	if t, ok := leadingType(b); ok && !claudeKeep[t] {
		return
	}
	var r claudeRecord
	var m claudeMessage
	if json.Unmarshal(b, &r) != nil || json.Unmarshal(r.Message, &m) != nil {
		return
	}
	at := p.when(r.Timestamp)
	side := p.sidechain || r.IsSidechain
	switch r.Type {
	case "assistant":
		if !r.IsAPIError && m.Model != "<synthetic>" {
			p.countUsage(m, at, side)
		}
	case "user":
		if side || r.IsMeta || r.IsCompactSummary {
			return
		}
		var text string
		if json.Unmarshal(m.Content, &text) != nil {
			var blocks []claudeBlock
			_ = json.Unmarshal(m.Content, &blocks)
			for _, bl := range blocks {
				if bl.Type == "text" {
					text += bl.Text
				}
			}
		}
		if kind, _ := classifyClaudePrompt(strings.TrimSpace(text), originKind(r.Origin)); kind == promptHuman || kind == promptPeer {
			p.f.prompt(at)
		}
	}
}

func (p *claudeParser) toolUse(b claudeBlock, at time.Time, chain string, side bool) {
	if b.ID == "" {
		return
	}
	d := describe(api.AgentClaude, b.Name, b.Input, p.f.cwd)
	it := p.newItem(b.ID, api.ActivityTool, at, chain, side)
	it.Tool, it.ToolKind, it.Summary, it.command, it.Pending = b.Name, d.kind, d.summary, d.command, true
	switch b.Name {
	case "Agent", "Task":
		it.Kind = api.ActivitySubagent
	case "AskUserQuestion":
		var in struct {
			Questions []json.RawMessage `json:"questions"`
		}
		_ = json.Unmarshal(b.Input, &in)
		it.asks = max(1, len(in.Questions))
		it.Summary = "Asked you " + countNoun(it.asks, "question")
	case "ExitPlanMode":
		it.plan = true
		it.Summary = "Asked you to approve a plan"
	}
	if added := p.f.add(it); added == it {
		it.edits = claudeEdits(b.Name, b.Input)
		p.f.applyEdits(it.edits, 1)
	}
}

func (p *claudeParser) system(r claudeRecord, at time.Time, chain string, side bool) {
	switch r.Subtype {
	case "compact_boundary":
		var meta struct {
			PreTokens int64 `json:"preTokens"`
		}
		_ = json.Unmarshal(r.CompactMetadata, &meta)
		s := "Compacted the conversation"
		if meta.PreTokens > 0 {
			s += " (was " + shortCount(meta.PreTokens) + " tokens)"
		}
		p.f.add(p.newItem(r.UUID, api.ActivitySystem, at, chain, side)).setText(s)
	case "turn_duration":
		p.f.settle(chain)
		s := "Finished its turn"
		if r.DurationMs > 0 {
			s += " after " + shortDuration(time.Duration(r.DurationMs)*time.Millisecond)
		}
		p.f.add(p.newItem(r.UUID, api.ActivitySystem, at, chain, side)).setText(s)
	case "api_error":
		var s string
		_ = json.Unmarshal(r.Content, &s)
		it := p.newItem(r.UUID, api.ActivityError, at, chain, side)
		it.Text = firstNonEmpty(strings.TrimSpace(s), "API error; Claude Code is retrying")
		p.f.add(it)
	}
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func countNoun(n int, noun string) string {
	return strconv.Itoa(n) + " " + noun + plural(n)
}

// shortCount renders 182000 as "182k".
func shortCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
	case n >= 1000:
		return strconv.FormatInt((n+500)/1000, 10) + "k"
	}
	return strconv.FormatInt(n, 10)
}

// shortDuration renders 125s as "2m 5s".
func shortDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		m, s := int(d.Minutes()), int(d.Seconds())%60
		if s == 0 {
			return strconv.Itoa(m) + "m"
		}
		return fmt.Sprintf("%dm %ds", m, s)
	}
	h, m := int(d.Hours()), int(d.Minutes())%60
	return fmt.Sprintf("%dh %dm", h, m)
}
