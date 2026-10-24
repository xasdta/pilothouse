package transcript

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

const threadID = "019a0000-1111-7222-8333-444455556666"

func (e *testEnv) codexPath(day time.Time, stamp string) string {
	return filepath.Join(e.codex, day.Format("2006/01/02"), "rollout-"+stamp+"-"+threadID+".jsonl")
}

func codexSession() *codexLog {
	c := newCodexLog()
	c.add("session_meta", map[string]any{"id": threadID, "cwd": "/home/dev/src/web", "originator": "codex_cli_rs", "cli_version": "0.130.0"})
	c.add("turn_context", map[string]any{"cwd": "/home/dev/src/web", "model": "gpt-test", "approval_policy": "on-request"})
	c.message("developer", "<permissions instructions>sandboxed</permissions instructions>")
	c.message("user", "<environment_context>\n  <cwd>/home/dev/src/web</cwd>\n</environment_context>")
	c.message("user", "# AGENTS.md instructions for /home/dev/src/web\n\nbe nice")
	c.add("event_msg", map[string]any{"type": "task_started", "turn_id": "t1"})
	c.message("user", "Make the tests pass")
	c.add("event_msg", map[string]any{"type": "user_message", "message": "Make the tests pass"}) // legacy copy: skipped
	c.add("response_item", map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": "x"})
	c.call("exec_command", "call_1", map[string]any{"cmd": "npm test", "workdir": "/home/dev/src/web"})
	c.output("call_1", "Chunk ID: 1\nWall time: 2.5 seconds\nProcess exited with code 1\nOutput:\n1 failing")
	c.tokens(1000, 800, 50, 1000, 800, 50, 10)
	c.tokens(1000, 800, 50, 1000, 800, 50, 10) // repeated with the same total (rate-limit refresh)
	patch := "*** Begin Patch\n*** Update File: src/app.ts\n@@\n-old\n+new\n+more\n*** Add File: src/new.ts\n+export {}\n*** End Patch"
	c.add("response_item", map[string]any{"type": "custom_tool_call", "name": "apply_patch", "call_id": "call_2", "input": patch, "status": "completed"})
	c.add("response_item", map[string]any{"type": "custom_tool_call_output", "call_id": "call_2",
		"output": `{"output":"Success. Updated the following files:\nM src/app.ts\nA src/new.ts\n","metadata":{"exit_code":0,"duration_seconds":0.1}}`})
	c.call("shell", "call_3", map[string]any{"command": []string{"bash", "-lc", "npm test"}})
	c.output("call_3", "Exit code: 0\nWall time: 3 seconds\nOutput:\n12 passing")
	c.add("response_item", map[string]any{"type": "web_search_call", "status": "completed", "action": map[string]any{"type": "search", "query": "vitest config"}})
	c.call("mcp_tool", "call_4", map[string]any{"q": 1})
	c.lines[len(c.lines)-1] = strings.Replace(c.lines[len(c.lines)-1], `"name":"mcp_tool"`, `"name":"query","namespace":"mcp__db__"`, 1)
	c.output("call_4", "rows")
	c.tokens(3000, 2000, 150, 2000, 1200, 100, 20)
	c.message("assistant", "All tests pass now.")
	c.add("event_msg", map[string]any{"type": "task_complete", "turn_id": "t1", "last_agent_message": "All tests pass now."})
	c.add("turn_context", map[string]any{"model": "gpt-other"})
	c.message("user", "Now deploy")
	c.call("update_plan", "call_5", map[string]any{"plan": []any{}})
	c.add("event_msg", map[string]any{"type": "turn_aborted", "reason": "interrupted"})
	c.add("compacted", map[string]any{"message": "summary", "replacement_history": []any{}})
	c.add("event_msg", map[string]any{"type": "error", "message": "stream disconnected"})
	c.lines = append(c.lines, `{"timestamp":"bad","type":"response_item","payload":{"type":"message","role":"assistant","content":5}}`, `{{{`)
	return c
}

func TestCodexFeed(t *testing.T) {
	e := newEnv(t)
	path := e.codexPath(t0, "2026-10-20T09-00-00")
	writeFile(t, path, codexSession().take())
	src := Source{Agent: api.AgentCodex, Path: path, Cwd: "/home/dev/src/web"}
	res := mustRead(t, e.reader(), "c", src, Query{Limit: 1000, CatchUp: true, Now: t0.Add(30 * time.Minute)})

	var prompts []string
	for _, p := range byKind(res.Items, api.ActivityPrompt) {
		prompts = append(prompts, p.Text)
	}
	if !reflect.DeepEqual(prompts, []string{"Make the tests pass", "Now deploy"}) {
		t.Errorf("prompts = %q", prompts)
	}
	run := findItem(t, res.Items, "call_1")
	if run.Summary != "Run: npm test" || !run.IsError || run.Pending || run.Result != "1 failing" || run.Turn != 1 {
		t.Errorf("exec_command = %+v", run)
	}
	p := findItem(t, res.Items, "call_2")
	if p.ToolKind != api.ToolEdit || p.IsError || !strings.HasPrefix(p.Result, "Success.") || !strings.HasPrefix(p.Summary, "Edit:") {
		t.Errorf("apply_patch = %+v", p)
	}
	if sh := findItem(t, res.Items, "call_3"); sh.Summary != "Run: npm test" || sh.IsError {
		t.Errorf("shell = %+v", sh)
	}
	if mcp := findItem(t, res.Items, "call_4"); mcp.ToolKind != api.ToolMCP || mcp.Tool != "mcp__db__query" {
		t.Errorf("mcp call = %+v", mcp)
	}
	if plan := findItem(t, res.Items, "call_5"); plan.Summary != "Updated its plan" || plan.Pending {
		t.Errorf("update_plan = %+v (the abort settles it)", plan)
	}
	var web []api.ActivityItem
	for _, it := range res.Items {
		if it.ToolKind == api.ToolWeb {
			web = append(web, it)
		}
	}
	if len(web) != 1 || web[0].Summary != "Search the web: vitest config" || web[0].Pending {
		t.Errorf("web search = %+v", web)
	}
	if texts := byKind(res.Items, api.ActivityText); len(texts) != 1 || texts[0].Text != "All tests pass now." {
		t.Errorf("texts = %+v", texts)
	}
	var sys []string
	for _, s := range byKind(res.Items, api.ActivitySystem) {
		sys = append(sys, s.Text)
	}
	if !reflect.DeepEqual(sys, []string{"Finished its turn after 17s", "Interrupted by the person", "Compacted the conversation"}) {
		t.Errorf("system = %q", sys)
	}
	if errs := byKind(res.Items, api.ActivityError); len(errs) != 1 || errs[0].Text != "stream disconnected" {
		t.Errorf("errors = %+v", errs)
	}

	// Usage: the two distinct token_count "last" usages; OpenAI counts
	// cached tokens inside input, Tokens doesn't.
	want := api.Usage{InputTokens: (1000 - 800) + (2000 - 1200), CacheReadTokens: 800 + 1200, OutputTokens: 50 + 100,
		ReasoningTokens: 30, Model: "gpt-test", Turns: 1}
	if res.Usage != want {
		t.Errorf("usage = %+v\nwant    %+v", res.Usage, want)
	}
	wantFiles := []api.FileChange{{Path: "src/new.ts", Op: api.FileCreate, Added: 1}, {Path: "src/app.ts", Op: api.FileEdit, Added: 2, Removed: 1}}
	if !reflect.DeepEqual(res.Files, wantFiles) {
		t.Errorf("files = %+v", res.Files)
	}
	lines := strings.Join(res.CatchUp.Lines, "\n")
	for _, w := range []string{"Hit 2 errors", "Tests: npm test passed (last run 29 min ago)", "Edited 2 files (+3/−1)", "Ran 6 tool calls"} {
		if !strings.Contains(lines, w) {
			t.Errorf("catch-up lacks %q:\n%s", w, lines)
		}
	}
}

// A rollout from before the {timestamp, type, payload} envelope: bare
// response items.
func TestCodexLegacyItems(t *testing.T) {
	e := newEnv(t)
	path := e.codexPath(t0, "2025-01-01T00-00-00")
	writeFile(t, path, `{"id":"`+threadID+`","timestamp":"2025-01-01T00:00:00.000Z","instructions":null}
{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}
{"type":"function_call","name":"shell","arguments":"{\"command\":[\"ls\",\"-la\"]}","call_id":"c1"}
{"type":"function_call_output","call_id":"c1","output":"{\"output\":\"total 0\",\"metadata\":{\"exit_code\":2}}"}
`)
	res := mustRead(t, e.reader(), "l", Source{Agent: api.AgentCodex, Path: path}, Query{})
	if p := byKind(res.Items, api.ActivityPrompt); len(p) != 1 || p[0].Text != "hello" {
		t.Errorf("prompts = %+v", p)
	}
	if c := findItem(t, res.Items, "c1"); c.Summary != "Run: ls -la" || !c.IsError || c.Result != "total 0" {
		t.Errorf("call = %+v", c)
	}
}

func TestCodexOutput(t *testing.T) {
	cases := []struct {
		raw  string
		text string
		exit int // -1: none
	}{
		{`"plain text"`, "plain text", -1},
		{`"Wall time: 1s\nProcess exited with code 0\nOutput:\nok"`, "ok", 0},
		{`"Exit code: 127\nOutput:\nnot found"`, "not found", 127},
		{`"{\"output\":\"done\",\"metadata\":{\"exit_code\":3}}"`, "done", 3},
		{`[{"type":"input_text","text":"a"},{"type":"input_image","image_url":"data:"}]`, "a\n[image]", -1},
	}
	for _, c := range cases {
		text, exit := codexOutput([]byte(c.raw))
		got := -1
		if exit != nil {
			got = *exit
		}
		if text != c.text || got != c.exit {
			t.Errorf("codexOutput(%s) = %q, %d; want %q, %d", c.raw, text, got, c.text, c.exit)
		}
	}
}

func TestFindCodexRollout(t *testing.T) {
	e := newEnv(t)
	started := time.Date(2026, 10, 20, 23, 59, 0, 0, time.Local)
	older := e.codexPath(started, "2026-10-20T23-59-00")
	writeFile(t, older, "{}\n")
	// A rolled-back thread gets a new file named <thread>_<rollout>; the
	// newest wins.
	newer := filepath.Join(filepath.Dir(older), "rollout-2026-10-20T23-59-30-"+threadID+"_019a0000-9999-7222-8333-444455556666.jsonl")
	writeFile(t, newer, "{}\n")
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(older, past, past); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(filepath.Dir(older), "rollout-2026-10-20T10-00-00-019a0000-0000-0000-0000-000000000000.jsonl"), "{}\n")
	got, ok := FindCodexRollout(e.opts.Roots.Codex, threadID, started, started.Add(48*time.Hour))
	if !ok || got != newer {
		t.Errorf("found %q %v, want %q", got, ok, newer)
	}
	// Without a start time the search walks back from now.
	if got, ok := FindCodexRollout(e.opts.Roots.Codex, threadID, time.Time{}, started.Add(24*time.Hour)); !ok || got != newer {
		t.Errorf("walk back found %q %v", got, ok)
	}
	if _, ok := FindCodexRollout(e.opts.Roots.Codex, threadID, time.Time{}, started.AddDate(0, 3, 0)); ok {
		t.Error("search must stop a month back")
	}
	for _, bad := range []string{"", "../../etc", "a/b", threadID + "/../x", "short"} {
		if _, ok := FindCodexRollout(e.opts.Roots.Codex, bad, started, started); ok {
			t.Errorf("thread id %q must be refused", bad)
		}
	}
}

func TestFindClaude(t *testing.T) {
	e := newEnv(t)
	sid := "0b6e8f2a-1111-4222-8333-444455556666"
	p := e.claudePath(sid)
	writeFile(t, p, "{}\n")
	if got, ok := FindClaude(e.opts.Roots.Claude, sid); !ok || got != p {
		t.Errorf("FindClaude = %q %v", got, ok)
	}
	if _, ok := FindClaude(e.opts.Roots.Claude, "../"+sid); ok {
		t.Error("path traversal in a session id must be refused")
	}
}
