package transcript

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/adapters/hookkit"
	"github.com/xasdta/pilothouse/internal/api"
)

// Grok Build keeps its durable transcript in
// $GROK_HOME/sessions/<url-encoded-cwd>/<session-id>/updates.jsonl: one
// ACP (or xAI extension) notification per line, wrapped in
// {timestamp, method, params}. Text streams as chunks; tool calls arrive
// as tool_call and are completed by tool_call_update. chat_history.jsonl is
// derived from this file, so the update stream is what we read.

type grokLine struct {
	Timestamp json.RawMessage `json:"timestamp"`
	Method    string          `json:"method"`
	Params    struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
		Meta      struct {
			EventID          string `json:"eventId"`
			AgentTimestampMS int64  `json:"agentTimestampMs"`
		} `json:"_meta"`
	} `json:"params"`
}

type grokLocation struct {
	Path string `json:"path"`
}

type grokUpdate struct {
	SessionUpdate string          `json:"sessionUpdate"`
	Content       json.RawMessage `json:"content"`
	ToolCallID    string          `json:"toolCallId"`
	Title         string          `json:"title"`
	Kind          string          `json:"kind"`
	Status        string          `json:"status"`
	RawInput      json.RawMessage `json:"rawInput"`
	RawOutput     json.RawMessage `json:"rawOutput"`
	Locations     []grokLocation  `json:"locations"`
}

type grokParser struct {
	f         *feed
	lastAt    time.Time
	user      *item
	assistant *item
}

func (p *grokParser) line(b []byte, off int64) {
	var l grokLine
	if json.Unmarshal(b, &l) != nil {
		return
	}
	// Very old sessions wrote a bare SessionNotification (sessionId and
	// update at the top level); treat it as params.
	if len(bytes.TrimSpace(l.Params.Update)) == 0 {
		var raw struct {
			SessionID string          `json:"sessionId"`
			Update    json.RawMessage `json:"update"`
			Meta      struct {
				EventID          string `json:"eventId"`
				AgentTimestampMS int64  `json:"agentTimestampMs"`
			} `json:"_meta"`
		}
		if json.Unmarshal(b, &raw) != nil || len(raw.Update) == 0 {
			return
		}
		l.Params.SessionID, l.Params.Update = raw.SessionID, raw.Update
		l.Params.Meta.EventID, l.Params.Meta.AgentTimestampMS = raw.Meta.EventID, raw.Meta.AgentTimestampMS
	}
	var u grokUpdate
	if json.Unmarshal(l.Params.Update, &u) != nil {
		return
	}
	at := p.when(l.Timestamp, l.Params.Meta.AgentTimestampMS)
	id := l.Params.Meta.EventID
	if id == "" {
		id = "g" + strconv.FormatInt(off, 10)
	}
	switch u.SessionUpdate {
	case "user_message_chunk":
		p.textChunk(u, id, at, true)
	case "agent_message_chunk":
		p.textChunk(u, id, at, false)
	case "tool_call", "tool_call_update":
		p.tool(u, id, at)
	case "auto_compact_started", "compaction_checkpoint":
		p.assistant = nil
		p.f.add(&item{ActivityItem: api.ActivityItem{ID: id, Kind: api.ActivitySystem, At: at,
			Text: "Compacted the conversation"}})
	}
}

func (p *grokParser) when(raw json.RawMessage, ms int64) time.Time {
	if ms > 0 {
		p.lastAt = time.UnixMilli(ms).UTC()
		return p.lastAt
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil && n > 0 {
		p.lastAt = time.Unix(n, 0).UTC()
		return p.lastAt
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			p.lastAt = t.UTC()
		}
	}
	return p.lastAt
}

func (p *grokParser) textChunk(u grokUpdate, id string, at time.Time, user bool) {
	text := grokContentText(u.Content)
	if text == "" {
		return
	}
	if user {
		p.assistant = nil
		if p.user == nil {
			p.f.prompt(at)
			p.user = p.f.add(&item{ActivityItem: api.ActivityItem{
				ID: id, Kind: api.ActivityPrompt, At: at, Source: "human"}})
		}
		p.user.Text = capHead(p.user.Text+text, maxText)
		p.f.touch(p.user)
		return
	}
	p.user = nil
	if p.assistant == nil {
		p.assistant = p.f.add(&item{ActivityItem: api.ActivityItem{
			ID: id, Kind: api.ActivityText, At: at}})
	}
	p.assistant.Text = capHead(p.assistant.Text+text, maxText)
	p.f.touch(p.assistant)
}

func (p *grokParser) tool(u grokUpdate, eventID string, at time.Time) {
	callID := u.ToolCallID
	if callID == "" {
		callID = eventID
	}
	id := "g" + callID
	p.user, p.assistant = nil, nil
	it, exists := p.f.byID[id]
	if !exists {
		it = &item{ActivityItem: api.ActivityItem{ID: id, Kind: api.ActivityTool, At: at, Tool: grokToolName(u.Kind), Pending: true}}
		describeGrokTool(it, u)
		if added := p.f.add(it); added != it {
			it = added
		}
		it.edits = grokEdits(u.Kind, u.RawInput, u.Locations, p.f.cwd)
		p.f.applyEdits(it.edits, 1)
	} else {
		if u.Kind != "" || u.Title != "" || len(u.RawInput) > 0 || len(u.Locations) > 0 {
			describeGrokTool(it, u)
			p.f.touch(it)
		}
	}

	status := strings.ToLower(u.Status)
	if status == "" || status == "pending" || status == "in_progress" {
		return
	}
	text, isError, exit, diff := grokResult(u)
	isError = isError || status == "failed"
	if diff != nil {
		p.f.setEdits(it, []fileEdit{*diff})
	}
	p.f.result(id, text, isError, exit)
}

func describeGrokTool(it *item, u grokUpdate) {
	kind := strings.ToLower(u.Kind)
	if kind == "" {
		switch it.ToolKind {
		case api.ToolShell:
			kind = "execute"
		case api.ToolEdit:
			kind = "edit"
		case api.ToolRead:
			kind = "read"
		case api.ToolWeb:
			kind = "fetch"
		}
	}
	title := strings.TrimSpace(u.Title)
	paths := grokPaths(u.RawInput, u.Locations)
	it.ToolKind = api.ToolOther
	switch kind {
	case "execute":
		it.ToolKind = api.ToolShell
		cmd := grokField(u.RawInput, "command", "cmd")
		if cmd == "" {
			cmd = strings.TrimSuffix(strings.TrimPrefix(title, "Run `"), "`")
		}
		it.command = cmd
		if cmd != "" {
			it.Summary = "Run: " + hookkit.OneLine(cmd, 160)
		} else {
			it.Summary = "Run a command"
		}
	case "edit", "write":
		it.ToolKind = api.ToolEdit
		it.Summary = grokEditSummary(paths, title)
	case "delete":
		it.ToolKind = api.ToolEdit
		it.Summary = firstNonEmptyString(title, "Delete: "+grokPathsSummary(paths, "a file"))
	case "move":
		it.ToolKind = api.ToolEdit
		it.Summary = firstNonEmptyString(title, "Move: "+grokPathsSummary(paths, "a file"))
	case "read":
		it.ToolKind = api.ToolRead
		it.Summary = "Read: " + grokPathsSummary(paths, title)
	case "search":
		it.ToolKind = api.ToolRead
		it.Summary = "Search: " + firstNonEmptyString(title, grokField(u.RawInput, "pattern", "query"))
	case "list_dir", "list":
		it.ToolKind = api.ToolRead
		it.Summary = "List: " + grokPathsSummary(paths, firstNonEmptyString(grokField(u.RawInput, "path", "dir", "directory"), title))
	case "lsp", "memory_search", "memory_get":
		it.ToolKind = api.ToolRead
		it.Summary = firstNonEmptyString(title, "Inspect symbols")
	case "fetch", "web_fetch":
		it.ToolKind = api.ToolWeb
		if url := grokField(u.RawInput, "url"); url != "" {
			it.Summary = firstNonEmptyString(title, "Fetch: "+hookkit.OneLine(url, 160))
		} else {
			it.Summary = firstNonEmptyString(title, "Fetch a URL")
		}
	case "web_search":
		it.ToolKind = api.ToolWeb
		if query := grokField(u.RawInput, "query", "search_term"); query != "" {
			it.Summary = firstNonEmptyString(title, "Search the web: "+hookkit.OneLine(query, 120))
		} else {
			it.Summary = firstNonEmptyString(title, "Search the web")
		}
	case "task":
		it.Summary = "Start subagent"
		if title != "" {
			it.Summary += ": " + hookkit.OneLine(title, 100)
		}
	case "ask_user":
		it.Summary = firstNonEmptyString(title, "Ask the user")
	case "plan", "enter_plan", "exit_plan":
		it.Summary = firstNonEmptyString(title, "Update the plan")
	case "think":
		it.Summary = firstNonEmptyString(title, "Updated its plan")
	default:
		it.Summary = firstNonEmptyString(title, "Use a tool")
	}
	if it.Summary == "" {
		it.Summary = "Use a tool"
	}
}

func grokToolName(kind string) string {
	switch strings.ToLower(kind) {
	case "execute":
		return "bash"
	case "edit", "write":
		return "edit"
	case "delete":
		return "delete"
	case "move":
		return "move"
	case "read":
		return "read"
	case "search":
		return "grep"
	case "list_dir", "list":
		return "list"
	case "lsp":
		return "lsp"
	case "fetch", "web_fetch":
		return "web_fetch"
	case "web_search":
		return "web_search"
	case "task":
		return "task"
	case "ask_user":
		return "ask_user"
	case "plan", "enter_plan", "exit_plan":
		return "plan"
	case "think":
		return "todo"
	}
	return "tool"
}

func grokPaths(raw json.RawMessage, locs []grokLocation) []string {
	var out []string
	for _, l := range locs {
		if l.Path != "" {
			out = append(out, l.Path)
		}
	}
	if len(out) == 0 {
		if p := grokField(raw, "file_path", "filePath", "path", "target_file", "target_directory"); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func grokEditSummary(paths []string, title string) string {
	if len(paths) == 0 {
		return firstNonEmptyString(title, "Edit files")
	}
	var names []string
	for _, p := range paths[:min(len(paths), 3)] {
		names = append(names, filepath.Base(p))
	}
	s := "Edit: " + strings.Join(names, ", ")
	if len(paths) > 3 {
		s += " (+" + strconv.Itoa(len(paths)-3) + " more)"
	}
	return s
}

func grokPathsSummary(paths []string, title string) string {
	if len(paths) == 0 {
		return firstNonEmptyString(title, "a file")
	}
	s := paths[0]
	if len(paths) > 1 {
		s += " (+" + strconv.Itoa(len(paths)-1) + " more)"
	}
	return s
}

// grokEdits estimates a file tool's effect from its input. The tool's
// diff result replaces the estimate when the update carries one.
func grokEdits(kind string, raw json.RawMessage, locs []grokLocation, cwd string) []fileEdit {
	kind = strings.ToLower(kind)
	if kind != "edit" && kind != "write" && kind != "apply_patch" {
		return nil
	}
	if kind == "apply_patch" {
		patch := grokField(raw, "patch", "input", "command")
		if patch != "" {
			return patchEdits(patch, cwd)
		}
	}
	paths := grokPaths(raw, locs)
	if len(paths) == 0 {
		return nil
	}
	p := paths[0]
	if !filepath.IsAbs(p) && cwd != "" {
		p = filepath.Join(cwd, p)
	}
	oldS := grokField(raw, "old_string", "oldString", "old_text")
	newS := grokField(raw, "new_string", "newString", "new_text")
	content := grokField(raw, "content", "new_source")
	if kind == "write" || oldS == "" {
		return []fileEdit{{path: p, op: api.FileCreate, added: countLines(firstNonEmptyString(content, newS))}}
	}
	add, rem := lineDelta(oldS, newS)
	return []fileEdit{{path: p, op: api.FileEdit, added: add, removed: rem}}
}

// grokResult returns the text, failure and exit code from a completed
// update. Text content is preferred; rawOutput carries the typed output.
func grokResult(u grokUpdate) (string, bool, *int, *fileEdit) {
	text, diff := grokToolContent(u.Content)
	var errText string
	var exit *int
	var raw struct {
		Type    string          `json:"type"`
		Output  json.RawMessage `json:"output"`
		Prompt  string          `json:"output_for_prompt"`
		Content string          `json:"content"`
		Text    string          `json:"text"`
		Message string          `json:"message"`
		Error   string          `json:"error"`
		Exit    *int            `json:"exit_code"`
	}
	if len(u.RawOutput) > 0 && json.Unmarshal(u.RawOutput, &raw) == nil {
		switch {
		case raw.Prompt != "":
			text = raw.Prompt
		case len(raw.Output) > 0:
			text = grokRawBytes(raw.Output)
		case raw.Content != "":
			text = raw.Content
		case raw.Text != "":
			text = raw.Text
		case raw.Message != "":
			text = raw.Message
		case raw.Error != "":
			errText = raw.Error
		}
		exit = raw.Exit
	}
	if text == "" {
		text = errText
	}
	return text, errText != "" || (exit != nil && *exit != 0), exit, diff
}

func grokToolContent(raw json.RawMessage) (string, *fileEdit) {
	if len(raw) == 0 {
		return "", nil
	}
	var one struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Path    string          `json:"path"`
		OldText string          `json:"oldText"`
		NewText string          `json:"newText"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &one) == nil && one.Type != "" {
		if one.Type == "diff" && one.Path != "" {
			add, rem := lineDelta(one.OldText, one.NewText)
			return "", &fileEdit{path: one.Path, op: api.FileEdit, added: add, removed: rem}
		}
		if one.Type == "text" {
			return one.Text, nil
		}
		if len(one.Content) > 0 {
			return grokContentText(one.Content), nil
		}
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return "", nil
	}
	var texts []string
	var diff *fileEdit
	for _, p := range parts {
		t, d := grokToolContent(p)
		if d != nil {
			diff = d
		}
		if t != "" {
			texts = append(texts, t)
		}
	}
	return strings.Join(texts, "\n"), diff
}

// grokContentText joins the text blocks of an ACP content value. Images
// are noted so a prompt that is only an image still appears.
func grokContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var one struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &one) == nil && one.Type != "" {
		switch one.Type {
		case "text":
			return one.Text
		case "image":
			return "[image]"
		}
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var texts []string
	for _, p := range parts {
		switch p.Type {
		case "text":
			texts = append(texts, p.Text)
		case "image":
			texts = append(texts, "[image]")
		}
	}
	return strings.Join(texts, "\n")
}

func grokField(raw json.RawMessage, names ...string) string {
	var m map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
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

// grokRawBytes decodes the byte array BashOutput serializes.
func grokRawBytes(raw json.RawMessage) string {
	var bs []byte
	if json.Unmarshal(raw, &bs) == nil {
		return string(bs)
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

func firstNonEmptyString(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
