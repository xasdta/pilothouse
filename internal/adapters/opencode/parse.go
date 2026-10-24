package opencode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/hookkit"
	"github.com/xasdta/pilothouse/internal/api"
)

// Protocol is the payload version plugin/pilothouse.js sends.
const Protocol = 1

// Plugin events (payload.event). Most are OpenCode's own hook and bus
// event names; pilothouse.ask is the plugin's follow-up to a policy "ask".
const (
	EventSessionCreated  = "session.created"
	EventSessionUpdated  = "session.updated" // title changed
	EventSessionDeleted  = "session.deleted"
	EventSessionIdle     = "session.idle"
	EventCompacting      = "session.compacting"
	EventChatMessage     = "chat.message"
	EventToolBefore      = "tool.execute.before"
	EventToolAfter       = "tool.execute.after"
	EventPermissionAsked = "permission.asked"
	EventPermissionReply = "permission.replied"
	EventQuestionAsked   = "question.asked"
	EventQuestionReply   = "question.replied"
	EventQuestionReject  = "question.rejected"
	EventAsk             = "pilothouse.ask"
)

var kinds = map[string]adapters.HookKind{
	EventSessionCreated:  adapters.HookSessionStart,
	EventSessionUpdated:  adapters.HookNotification,
	EventSessionDeleted:  adapters.HookSessionEnd,
	EventSessionIdle:     adapters.HookStop,
	EventCompacting:      adapters.HookPreCompact,
	EventChatMessage:     adapters.HookPromptSubmit,
	EventToolBefore:      adapters.HookPreTool,
	EventToolAfter:       adapters.HookPostTool,
	EventPermissionAsked: adapters.HookPermission,
	EventQuestionAsked:   adapters.HookPermission,
	EventAsk:             adapters.HookPermission,
	// The person answered OpenCode's own dialog: like a post_tool hook it
	// closes the pending request, matched by the dialog's id.
	EventPermissionReply: adapters.HookPostTool,
	EventQuestionReply:   adapters.HookPostTool,
	EventQuestionReject:  adapters.HookPostTool,
}

// payload is what the plugin sends (plugin/pilothouse.js, hook()).
type payload struct {
	V              int             `json:"v"`
	Event          string          `json:"event"`
	SessionID      string          `json:"session_id"`       // the session a person sees (a subagent's parent)
	AgentSessionID string          `json:"agent_session_id"` // the session the event is about
	Directory      string          `json:"directory"`
	Title          string          `json:"title"`
	Text           string          `json:"text"` // prompt (chat.message), last assistant text (session.idle)
	Tool           string          `json:"tool"`
	CallID         string          `json:"call_id"`
	Args           json.RawMessage `json:"args"`
	RequestID      string          `json:"request_id"` // permission or question id
	Permission     *permission     `json:"permission"`
	Questions      []question      `json:"questions"`
	Reply          string          `json:"reply"`   // once | always | reject | answer
	Rule           string          `json:"rule"`    // pilothouse.ask
	Message        string          `json:"message"` // pilothouse.ask: the rule's message
}

// permission is OpenCode's permission.asked event (PermissionRequest).
type permission struct {
	Permission string         `json:"permission"` // bash, edit, webfetch, external_directory, doom_loop, …
	Patterns   []string       `json:"patterns"`
	Metadata   map[string]any `json:"metadata"`
	Always     []string       `json:"always"`
}

// question is one of question.asked's questions (QuestionInfo).
type question struct {
	Question string `json:"question"`
	Header   string `json:"header"`
	Options  []struct {
		Label       string `json:"label"`
		Description string `json:"description"`
	} `json:"options"`
	Multiple bool  `json:"multiple"`
	Custom   *bool `json:"custom"` // free text allowed; OpenCode's default is true
}


// ParseHook normalizes one plugin payload.
func (Adapter) ParseHook(env adapters.HookEnvelope) (adapters.HookEvent, error) {
	var p payload
	if len(bytes.TrimSpace(env.Payload)) == 0 {
		return adapters.HookEvent{}, errors.New("empty plugin payload")
	}
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return adapters.HookEvent{}, fmt.Errorf("plugin payload: %w", err)
	}
	if p.Event == "" {
		p.Event = env.Event
	}
	if p.Event == "" {
		return adapters.HookEvent{}, errors.New("plugin payload has no event")
	}
	if p.V > Protocol {
		return adapters.HookEvent{}, fmt.Errorf("plugin protocol v%d is newer than this pilothouse (v%d): upgrade pilothouse", p.V, Protocol)
	}
	if string(bytes.TrimSpace(p.Args)) == "null" {
		p.Args = nil
	}
	kind, ok := kinds[p.Event]
	if !ok {
		kind = adapters.HookUnknown
	}
	ev := adapters.HookEvent{
		Kind:           kind,
		VendorEvent:    p.Event,
		AgentSessionID: firstNonEmpty(p.SessionID, p.AgentSessionID),
		Cwd:            firstNonEmpty(p.Directory, env.Cwd),
	}
	switch p.Event {
	case EventSessionCreated:
		ev.Summary = "Session started"
		if t := strings.TrimSpace(p.Title); t != "" && !defaultTitle(t) {
			ev.Summary += ": " + hookkit.OneLine(t, 100)
		}
	case EventSessionUpdated:
		ev.NotificationType = "title"
		ev.Message = hookkit.CapBytes(strings.TrimSpace(p.Title), 200)
		ev.Summary = "Title: " + hookkit.OneLine(p.Title, 100)
	case EventSessionDeleted:
		ev.Summary = "Session ended"
	case EventSessionIdle:
		ev.Message = hookkit.CapBytes(p.Text, hookkit.MaxMessage)
		ev.Summary = "Finished its turn"
		if strings.TrimSpace(ev.Message) != "" {
			ev.Summary = "Finished: " + hookkit.OneLine(ev.Message, 100)
		}
	case EventCompacting:
		ev.Summary = "Compacting context"
	case EventChatMessage:
		ev.Message = hookkit.CapBytes(p.Text, hookkit.MaxMessage)
		ev.Summary = "Prompt: " + hookkit.OneLine(p.Text, 100)
	case EventToolBefore, EventToolAfter:
		ev.ToolUseID, ev.Tool, ev.Input = p.CallID, p.Tool, p.Args
		describeTool(&ev)
	case EventAsk:
		ev.ToolUseID, ev.Tool, ev.Input = p.CallID, p.Tool, p.Args
		describeTool(&ev)
		ev.Hold = true
	case EventPermissionAsked:
		ev.ToolUseID = p.RequestID
		describePermission(&ev, p)
	case EventQuestionAsked:
		ev.ToolUseID, ev.Tool, ev.ToolKind = p.RequestID, "question", api.ToolOther
		ev.Input, _ = json.Marshal(map[string]any{"questions": p.Questions})
		qs, ok := questions(p.RequestID, p.Questions)
		ev.Summary = hookkit.QuestionSummary(qs)
		if !ok {
			// Not something Pilothouse can ask (too many questions, an
			// option without a label, …): OpenCode's own dialog answers
			// it, and the session shows as waiting on a question.
			ev.Kind, ev.NotificationType, ev.Message = adapters.HookNotification, "question", ev.Summary
			break
		}
		ev.Questions = qs
	case EventPermissionReply, EventQuestionReply, EventQuestionReject:
		// No tool: the request is matched by its id only, never by a
		// tool and summary that another pending request might share.
		ev.ToolUseID = p.RequestID
		ev.Summary = "Answered in OpenCode"
		if r := strings.TrimSpace(p.Reply); r != "" {
			ev.Summary += " (" + hookkit.OneLine(r, 20) + ")"
		}
	default:
		ev.Summary = p.Event
	}
	return ev, nil
}

// defaultTitle: OpenCode names a new session "New session - <date>" until
// it generates a title from the first prompt.
func defaultTitle(t string) bool { return strings.HasPrefix(t, "New session") }

// questions converts question.asked's questions. OpenCode gives the
// request one id (que_…) and answers it with one list of labels per
// question, in order; each question's ID is that id plus its position, so
// answers are keyed unambiguously even when two questions share a text.
// Texts and labels are kept exactly as sent (answers name the labels);
// ok is false when the set can't be asked through Pilothouse
// (hookkit.CheckQuestions).
func questions(requestID string, in []question) ([]api.Question, bool) {
	if requestID == "" || len(in) > hookkit.MaxQuestions {
		return nil, false
	}
	out := make([]api.Question, 0, len(in))
	for i, q := range in {
		aq := api.Question{
			ID:          questionID(requestID, i),
			Header:      q.Header,
			Text:        q.Question,
			MultiSelect: q.Multiple,
			AllowOther:  q.Custom == nil || *q.Custom,
		}
		for _, o := range q.Options {
			aq.Options = append(aq.Options, api.QuestionOption{Label: o.Label, Description: o.Description})
		}
		if len(aq.Text) > hookkit.MaxQuestionBytes {
			return nil, false
		}
		out = append(out, hookkit.TidyQuestion(aq))
	}
	return out, hookkit.CheckQuestions(out)
}

// questionID is the Question.ID of question i of an OpenCode question
// request: the request id, then "#" and the 1-based position.
func questionID(requestID string, i int) string {
	return fmt.Sprintf("%s#%d", requestID, i+1)
}

// describeTool classifies OpenCode's built-in tools (packages/opencode/src/
// tool: bash {command, workdir}; edit {filePath, oldString, newString};
// write {filePath, content}; read {filePath}; glob/grep {pattern, path};
// list {path}; apply_patch {patchText}; webfetch {url}; websearch/
// codesearch {query}; task {description, prompt, subagent_type}; question;
// todowrite; skill) and MCP tools, which OpenCode names <server>_<tool>.
func describeTool(ev *adapters.HookEvent) {
	field := func(name string) string { return hookkit.Field(ev.Input, name) }
	file := func(name string) string {
		p := field(name)
		if p != "" && !filepath.IsAbs(p) && ev.Cwd != "" {
			p = filepath.Join(ev.Cwd, p)
		}
		return p
	}
	switch ev.Tool {
	case "bash":
		ev.ToolKind = api.ToolShell
		ev.Command = field("command")
		ev.Summary = "Run: " + hookkit.OneLine(ev.Command, 160)
		if paths, exact := hookkit.ShellPatch(ev.Command, ev.Cwd); len(paths) > 0 {
			ev.Paths = paths
			if exact {
				ev.ToolKind = api.ToolEdit
				ev.Summary = editSummary(paths, ev.Cwd)
			}
		}
	case "edit", "write", "multiedit":
		ev.ToolKind = api.ToolEdit
		if p := file("filePath"); p != "" {
			ev.Paths = []string{p}
		}
		verb := "Edit"
		if ev.Tool == "write" {
			verb = "Write"
		}
		ev.Summary = verb + ": " + hookkit.RelPath(ev.Cwd, firstNonEmpty(file("filePath"), "a file"))
	case "apply_patch", "patch":
		ev.ToolKind = api.ToolEdit
		ev.Paths = hookkit.PatchPaths(field("patchText"), ev.Cwd)
		ev.Summary = editSummary(ev.Paths, ev.Cwd)
	case "read":
		ev.ToolKind = api.ToolRead
		if p := file("filePath"); p != "" {
			ev.Paths = []string{p}
			ev.Summary = "Read: " + hookkit.RelPath(ev.Cwd, p)
		} else {
			ev.Summary = "Read a file"
		}
	case "glob", "grep", "list":
		ev.ToolKind = api.ToolRead
		if p := file("path"); p != "" {
			ev.Paths = []string{p}
		}
		what := firstNonEmpty(field("pattern"), hookkit.RelPath(ev.Cwd, field("path")), ".")
		ev.Summary = strings.ToUpper(ev.Tool[:1]) + ev.Tool[1:] + ": " + hookkit.OneLine(what, 120)
	case "webfetch":
		ev.ToolKind = api.ToolWeb
		ev.Summary = "Fetch: " + hookkit.OneLine(field("url"), 160)
	case "websearch", "codesearch":
		ev.ToolKind = api.ToolWeb
		ev.Summary = "Search: " + hookkit.OneLine(field("query"), 160)
	case "task":
		ev.ToolKind = api.ToolOther
		ev.Summary = "Subagent: " + hookkit.OneLine(firstNonEmpty(field("description"), field("subagent_type"), "task"), 120)
	case "question":
		ev.ToolKind = api.ToolOther
		ev.Summary = "Ask the user"
	case "todowrite", "todoread", "skill", "invalid":
		ev.ToolKind = api.ToolOther
		ev.Summary = "Use " + ev.Tool
	default:
		if server, name, ok := mcpName(ev.Tool); ok {
			ev.ToolKind = api.ToolMCP
			ev.Summary = "MCP " + server + ": " + name
			return
		}
		ev.ToolKind = api.ToolOther
		ev.Summary = "Use " + ev.Tool
	}
}

// mcpName: OpenCode names MCP tools "<server>_<tool>" (sanitized). Built-in
// tool names have no underscore except apply_patch, handled above.
func mcpName(tool string) (server, name string, ok bool) {
	server, name, ok = strings.Cut(tool, "_")
	if !ok || server == "" || name == "" {
		return "", "", false
	}
	return server, name, true
}

// describePermission fills a permission.asked hook from the tool call the
// plugin saw before it (tool and args) and the permission's own fields.
// OpenCode asks for the tool itself (bash, edit, webfetch, …) or for a
// side condition of the call: external_directory (a path outside the
// project) and doom_loop (the same call repeated).
func describePermission(ev *adapters.HookEvent, p payload) {
	perm := permission{}
	if p.Permission != nil {
		perm = *p.Permission
	}
	ev.Tool, ev.Input = firstNonEmpty(p.Tool, perm.Permission, "permission"), p.Args
	if len(ev.Input) == 0 && perm.Metadata != nil {
		ev.Input, _ = json.Marshal(perm.Metadata)
	}
	describeTool(ev)
	if ev.Command == "" {
		if c, _ := perm.Metadata["command"].(string); c != "" && ev.ToolKind == api.ToolShell {
			ev.Command = c
		}
	}
	if len(ev.Paths) == 0 {
		if f, _ := perm.Metadata["filepath"].(string); f != "" {
			ev.Paths = []string{f}
		}
	}
	switch perm.Permission {
	case "external_directory":
		dirs := strings.Join(perm.Patterns, ", ")
		ev.Summary = "Work outside the project (" + hookkit.OneLine(dirs, 100) + "): " + ev.Summary
		for _, d := range perm.Patterns {
			ev.Paths = append(ev.Paths, strings.TrimSuffix(strings.TrimSuffix(d, "*"), "/"))
		}
	case "doom_loop":
		ev.Summary = "Repeat the same call again: " + ev.Summary
	case "", ev.Tool:
	default:
		if ev.Summary == "Use "+ev.Tool {
			ev.Summary = "Use " + ev.Tool + " (" + perm.Permission + "): " + hookkit.OneLine(strings.Join(perm.Patterns, ", "), 100)
		}
	}
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

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
