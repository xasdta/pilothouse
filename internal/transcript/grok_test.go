package transcript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

const grokSessionID = "019b0000-1111-7222-8333-444455556666"

func (e *testEnv) grokPath() string {
	p := filepath.Join(e.grok, "%2Fhome%2Fdev%2Fsrc%2Fweb", grokSessionID, "updates.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		e.t.Fatal(err)
	}
	return p
}

type grokLog struct {
	at    time.Time
	n     int
	lines []string
}

func (g *grokLog) add(update map[string]any) {
	g.n++
	meta := map[string]any{"eventId": "e" + string(rune('0'+g.n)), "agentTimestampMs": g.at.UnixMilli()}
	line, _ := json.Marshal(map[string]any{
		"timestamp": g.at.Unix(), "method": "session/update",
		"params": map[string]any{"sessionId": grokSessionID, "update": update, "_meta": meta},
	})
	g.lines = append(g.lines, string(line))
	g.at = g.at.Add(time.Second)
}

func (g *grokLog) take() string {
	out := ""
	for _, l := range g.lines {
		out += l + "\n"
	}
	return out
}

func grokSession() *grokLog {
	g := &grokLog{at: t0}
	chunk := func(kind, text string) map[string]any {
		return map[string]any{"sessionUpdate": kind, "content": map[string]any{"type": "text", "text": text}}
	}
	g.add(chunk("user_message_chunk", "Make the "))
	g.add(chunk("user_message_chunk", "tests pass"))
	g.add(chunk("agent_thought_chunk", "thinking"))
	g.add(chunk("agent_message_chunk", "On it"))
	g.add(chunk("agent_message_chunk", "."))
	g.add(map[string]any{
		"sessionUpdate": "tool_call", "toolCallId": "call_1", "title": "npm test", "kind": "execute",
		"status": "pending", "rawInput": map[string]any{"command": "npm test"},
	})
	g.add(map[string]any{
		"sessionUpdate": "tool_call_update", "toolCallId": "call_1", "status": "completed",
		"content":   []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "1 failing"}}},
		"rawOutput": map[string]any{"type": "Bash", "output": []byte("1 failing"), "output_for_prompt": "1 failing", "exit_code": 1},
	})
	g.add(map[string]any{
		"sessionUpdate": "tool_call", "toolCallId": "call_2", "title": "Edit `src/app.ts`", "kind": "edit",
		"status": "pending", "rawInput": map[string]any{"file_path": "src/app.ts", "old_string": "old", "new_string": "new\nmore"},
		"locations": []any{map[string]any{"path": "/home/dev/src/web/src/app.ts"}},
	})
	g.add(map[string]any{
		"sessionUpdate": "tool_call_update", "toolCallId": "call_2", "status": "completed",
		"content": []any{map[string]any{"type": "diff", "path": "/home/dev/src/web/src/app.ts", "oldText": "old\nline", "newText": "new\nmore\nline"}},
	})
	g.add(chunk("user_message_chunk", "Now deploy"))
	return g
}

func TestGrokFeed(t *testing.T) {
	e := newEnv(t)
	path := e.grokPath()
	writeFile(t, path, grokSession().take())
	src := Source{Agent: api.AgentGrok, Path: path, Cwd: "/home/dev/src/web"}
	res := mustRead(t, e.reader(), "g", src, Query{Limit: 1000, CatchUp: true, Now: t0.Add(20 * time.Second)})

	var prompts []string
	for _, p := range byKind(res.Items, api.ActivityPrompt) {
		prompts = append(prompts, p.Text)
	}
	if !reflect.DeepEqual(prompts, []string{"Make the tests pass", "Now deploy"}) {
		t.Errorf("prompts = %q", prompts)
	}
	if texts := byKind(res.Items, api.ActivityText); len(texts) != 1 || texts[0].Text != "On it." {
		t.Errorf("texts = %+v", texts)
	}
	run := findItem(t, res.Items, "gcall_1")
	if run.Summary != "Run: npm test" || !run.IsError || run.Pending || run.Result != "1 failing" || run.Turn != 1 {
		t.Errorf("bash = %+v", run)
	}
	edit := findItem(t, res.Items, "gcall_2")
	if edit.ToolKind != api.ToolEdit || edit.IsError || edit.Pending || edit.Summary != "Edit: app.ts" {
		t.Errorf("edit = %+v", edit)
	}
	if len(res.Files) != 1 || res.Files[0].Path != "src/app.ts" || res.Files[0].Added != 2 || res.Files[0].Removed != 1 {
		t.Errorf("files = %+v", res.Files)
	}
}

func TestGrokToolKinds(t *testing.T) {
	e := newEnv(t)
	path := e.grokPath()
	g := &grokLog{at: t0}
	for _, u := range []map[string]any{
		{"sessionUpdate": "tool_call", "toolCallId": "web", "kind": "web_fetch",
			"status": "completed", "rawInput": map[string]any{"url": "https://example.com"}},
		{"sessionUpdate": "tool_call", "toolCallId": "list", "kind": "list_dir",
			"status": "completed", "rawInput": map[string]any{"path": "src"}},
		{"sessionUpdate": "tool_call", "toolCallId": "del", "kind": "delete",
			"status": "completed", "locations": []any{map[string]any{"path": "/repo/a.go"}}},
		{"sessionUpdate": "tool_call", "toolCallId": "task", "kind": "task",
			"status": "completed", "title": "Explore repo"},
	} {
		g.add(u)
	}
	writeFile(t, path, g.take())
	res := mustRead(t, e.reader(), "g", Source{Agent: api.AgentGrok, Path: path}, Query{Limit: 100})
	for id, want := range map[string]struct {
		kind    api.ToolKind
		summary string
	}{
		"gweb":  {api.ToolWeb, "Fetch: https://example.com"},
		"glist": {api.ToolRead, "List: src"},
		"gdel":  {api.ToolEdit, "Delete: /repo/a.go"},
		"gtask": {api.ToolOther, "Start subagent: Explore repo"},
	} {
		it := findItem(t, res.Items, id)
		if it.ToolKind != want.kind || it.Summary != want.summary {
			t.Errorf("%s = %s %q", id, it.ToolKind, it.Summary)
		}
	}
}

func TestGrokLegacyLine(t *testing.T) {
	e := newEnv(t)
	path := e.grokPath()
	writeFile(t, path, `{"sessionId":"s","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"hi"}},"_meta":{"agentTimestampMs":1700000000000}}`+"\n")
	res := mustRead(t, e.reader(), "g", Source{Agent: api.AgentGrok, Path: path}, Query{Limit: 10})
	if len(res.Items) != 1 || res.Items[0].Kind != api.ActivityPrompt || res.Items[0].Text != "hi" {
		t.Errorf("legacy line = %+v", res.Items)
	}
}

func TestFindGrokUpdates(t *testing.T) {
	e := newEnv(t)
	path := e.grokPath()
	writeFile(t, path, "{}\n")
	got, ok := FindGrokUpdates(e.opts.Roots.Grok, grokSessionID)
	if !ok || got != path {
		t.Errorf("FindGrokUpdates = %q, %v; want %q", got, ok, path)
	}
	if _, ok := FindGrokUpdates(e.opts.Roots.Grok, "../../etc/passwd"); ok {
		t.Error("accepted a session id that could escape the root")
	}
}
