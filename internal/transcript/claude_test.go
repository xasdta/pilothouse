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

// claudeSession is a two-turn session exercising every record kind the
// feed reads, plus noise it must skip.
func claudeSession() *claudeLog {
	c := newClaudeLog()
	c.raw(`{"type":"permission-mode","permissionMode":"default","sessionId":"sess-1"}`)
	c.raw(`{"type":"file-history-snapshot","messageId":"x","snapshot":{"trackedFileBackups":{}}}`)
	c.raw(`{"type":"api-request-blob","hash":"h","message":{"role":"user","content":"ignored"}}`)
	c.raw(`{"parentUuid":null,"type":"attachment","attachment":{"type":"skill_listing"},"uuid":"att-1"}`)
	c.raw(`{"type":"some-future-record","what":"ever"}`)
	c.raw(`not json at all`)
	c.raw(`{"type":"user","message":`) // truncated record
	c.prompt("<local-command-caveat>Caveat: ignore</local-command-caveat>", "")
	c.prompt("Fix the login bug", "human") // turn 1
	// One response, three blocks (thinking, text, tool_use) sharing the id
	// and the usage: counted once.
	u1 := usage(10, 40, 1000, 200)
	c.block("msg_1", "m-known", map[string]any{"type": "thinking", "thinking": "", "signature": "sig"}, u1)
	c.text("msg_1", "m-known", "Looking at the login code.", u1)
	c.toolUse("msg_1", "m-known", "toolu_bash", "Bash", map[string]any{"command": "npm test", "description": "Run tests"}, u1)
	c.result("toolu_bash", "FAIL src/login.test.ts\n1 failed", true, map[string]any{"stdout": "", "stderr": "", "interrupted": false})
	// Second response: an Edit with its exact patch reported back.
	u2 := usage(5, 30, 1200, 50)
	c.toolUse("msg_2", "m-known", "toolu_edit", "Edit", map[string]any{"file_path": "/home/dev/src/app/src/login.ts",
		"old_string": "a\nb\n", "new_string": "a\nc\nd\n"}, u2)
	c.result("toolu_edit", "The file was updated.", false, map[string]any{"filePath": "/home/dev/src/app/src/login.ts",
		"structuredPatch": []any{map[string]any{"oldStart": 1, "oldLines": 2, "newStart": 1, "newLines": 3,
			"lines": []string{" a", "-b", "+c", "+d"}}}})
	// A Write the person refused: it must not count as a change.
	c.toolUse("msg_2", "m-known", "toolu_write", "Write", map[string]any{"file_path": "/home/dev/src/app/secret.txt", "content": "x\ny\n"}, u2)
	c.result("toolu_write", "The user doesn't want to proceed with this tool use.", true, nil)
	u3 := usage(2, 25, 1300, 0)
	c.toolUse("msg_3", "m-known", "toolu_bash2", "Bash", map[string]any{"command": "npm test"}, u3)
	c.result("toolu_bash2", []any{map[string]any{"type": "text", "text": "Tests: 12 passed"}}, false, nil)
	c.text("msg_3b", "m-known", "Fixed: the session cookie was never refreshed.", usage(1, 60, 1400, 0))
	c.system("turn_duration", map[string]any{"durationMs": 125000})
	// Turn 2 from another session (peer), a slash command, an API error.
	c.prompt("Also update the docs", "peer")
	c.toolUse("msg_4", "m-known", "toolu_ask", "AskUserQuestion", map[string]any{"questions": []any{
		map[string]any{"question": "Which file?", "header": "Docs", "options": []any{}, "multiSelect": false}}}, usage(1, 5, 0, 0))
	c.result("toolu_ask", "User answered: README", false, nil)
	c.add(map[string]any{"type": "assistant", "isApiErrorMessage": true, "message": map[string]any{
		"id": "msg_err", "role": "assistant", "model": "<synthetic>", "content": []any{map[string]any{"type": "text", "text": "API Error: overloaded"}},
		"usage": usage(0, 0, 0, 0)}})
	c.system("compact_boundary", map[string]any{"compactMetadata": map[string]any{"trigger": "auto", "preTokens": 182000}})
	c.prompt("<command-name>/model</command-name>\n<command-args>haiku</command-args>", "human")
	c.prompt("<task-notification><summary>Build finished</summary></task-notification>", "task-notification")
	return c
}

func TestClaudeFeed(t *testing.T) {
	e := newEnv(t)
	path := e.claudePath("sess-1")
	writeFile(t, path, claudeSession().String())
	src := Source{Agent: api.AgentClaude, Path: path, Cwd: "/home/dev/src/app"}
	res := mustRead(t, e.reader(), "s1", src, Query{Limit: 1000, CatchUp: true, Now: t0.Add(time.Hour)})

	prompts := byKind(res.Items, api.ActivityPrompt)
	var got []string
	for _, p := range prompts {
		got = append(got, p.Source+":"+p.Text)
	}
	want := []string{"human:Fix the login bug", "peer:Also update the docs", "human:/model haiku"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("prompts = %q, want %q", got, want)
	}
	if prompts[0].Turn != 1 || prompts[1].Turn != 2 || prompts[2].Turn != 3 {
		t.Errorf("turns = %d %d %d", prompts[0].Turn, prompts[1].Turn, prompts[2].Turn)
	}

	bash := findItem(t, res.Items, "toolu_bash")
	if bash.Kind != api.ActivityTool || bash.Summary != "Run: npm test" || bash.ToolKind != api.ToolShell ||
		!bash.IsError || bash.Pending || !strings.Contains(bash.Result, "1 failed") || bash.Turn != 1 {
		t.Errorf("bash item = %+v", bash)
	}
	if ok := findItem(t, res.Items, "toolu_bash2"); ok.IsError || ok.Result != "Tests: 12 passed" {
		t.Errorf("second bash = %+v", ok)
	}
	if ask := findItem(t, res.Items, "toolu_ask"); ask.Summary != "Asked you 1 question" || ask.Turn != 2 {
		t.Errorf("ask = %+v", ask)
	}
	texts := byKind(res.Items, api.ActivityText)
	if len(texts) != 2 || texts[1].Text != "Fixed: the session cookie was never refreshed." {
		t.Errorf("texts = %+v", texts)
	}
	if errs := byKind(res.Items, api.ActivityError); len(errs) != 1 || errs[0].Text != "API Error: overloaded" {
		t.Errorf("errors = %+v", errs)
	}
	var sys []string
	for _, s := range byKind(res.Items, api.ActivitySystem) {
		sys = append(sys, s.Text)
	}
	wantSys := []string{"Finished its turn after 2m 5s", "Compacted the conversation (was 182k tokens)", "A background task reported back: Build finished"}
	if !reflect.DeepEqual(sys, wantSys) {
		t.Errorf("system items = %q, want %q", sys, wantSys)
	}
	for _, it := range res.Items {
		if it.Pending {
			t.Errorf("item %s still pending", it.ID)
		}
		if it.ID == "" || it.At.IsZero() {
			t.Errorf("item without id/time: %+v", it)
		}
	}

	// Usage: each response once, the synthetic error never.
	wantUsage := api.Usage{InputTokens: 10 + 5 + 2 + 1 + 1, OutputTokens: 40 + 30 + 25 + 60 + 5,
		CacheReadTokens: 1000 + 1200 + 1300 + 1400, CacheWriteTokens: 200 + 50, Model: "m-known",
		Turns: 2} // the /model command reached no model
	wantUsage.CostUSD = fakePricer{}.Cost("m-known", Tokens{Input: wantUsage.InputTokens, Output: wantUsage.OutputTokens,
		CacheRead: wantUsage.CacheReadTokens, CacheWrite: wantUsage.CacheWriteTokens}, false)
	if d := res.Usage.CostUSD - wantUsage.CostUSD; d > 1e-12 || d < -1e-12 {
		t.Errorf("cost = %v, want %v", res.Usage.CostUSD, wantUsage.CostUSD)
	}
	res.Usage.CostUSD = wantUsage.CostUSD
	if res.Usage != wantUsage {
		t.Errorf("usage = %+v\nwant    %+v", res.Usage, wantUsage)
	}

	// Files: the edit with its exact counts; the refused write is gone.
	wantFiles := []api.FileChange{{Path: "src/login.ts", Op: api.FileEdit, Added: 2, Removed: 1}}
	if !reflect.DeepEqual(res.Files, wantFiles) {
		t.Errorf("files = %+v, want %+v", res.Files, wantFiles)
	}

	wantLines := []string{
		"Hit 2 errors (last: API Error: overloaded)",
		"1 tool call was denied",
		"Tests: npm test passed (last run 59 min ago)",
		"Edited 1 file (+2/−1): src/login.ts",
		"Ran 4 tool calls",
		"Asked you 1 question",
		"Compacted its context",
		"Last said: “Fixed: the session cookie was never refreshed.”",
	}
	if res.CatchUp == nil || !reflect.DeepEqual(res.CatchUp.Lines, wantLines) {
		t.Errorf("catch-up = %q\nwant %q", res.CatchUp.Lines, wantLines)
	}
	if res.CatchUp.ToolCalls != 4 || res.CatchUp.Files != 1 || res.CatchUp.Errors != 2 {
		t.Errorf("catch-up counts = %+v", res.CatchUp)
	}
}

// Usage repeated on every block of a response, and growing as blocks
// stream in, counts once with the largest values.
func TestClaudeUsageDedupe(t *testing.T) {
	e := newEnv(t)
	path := e.claudePath("sess-u")
	c := newClaudeLog()
	c.prompt("go", "human")
	c.text("msg_a", "m-known", "one", usage(3, 1, 100, 10))
	c.text("msg_a", "m-known", "two", usage(3, 9, 100, 10)) // output grew
	c.toolUse("msg_a", "m-known", "toolu_1", "Read", map[string]any{"file_path": "/x"}, usage(3, 9, 100, 10))
	c.text("msg_b", "other-model", "three", usage(1, 1, 0, 0))
	writeFile(t, path, c.String())
	r := e.reader()
	res := mustRead(t, r, "u", Source{Agent: api.AgentClaude, Path: path}, Query{})
	if res.Usage.InputTokens != 4 || res.Usage.OutputTokens != 10 || res.Usage.CacheReadTokens != 100 || res.Usage.CacheWriteTokens != 10 {
		t.Errorf("usage = %+v", res.Usage)
	}
	if res.Usage.Model != "other-model" {
		t.Errorf("model = %q, want the most recent", res.Usage.Model)
	}
	// Only the known model costs anything; the other's tokens still count.
	wantCost := fakePricer{}.Cost("m-known", Tokens{Input: 3, Output: 9, CacheRead: 100, CacheWrite: 10}, false)
	if d := res.Usage.CostUSD - wantCost; d > 1e-12 || d < -1e-12 {
		t.Errorf("cost = %v, want %v", res.Usage.CostUSD, wantCost)
	}
	u, err := r.Usage("u", Source{Agent: api.AgentClaude, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Days) != 2 || u.Days[0].Model != "m-known" || u.Days[0].Turns != 1 || u.Days[1].Turns != 0 {
		t.Errorf("days = %+v", u.Days)
	}
}

func TestClaudeFastModeAnd1hCache(t *testing.T) {
	e := newEnv(t)
	path := e.claudePath("sess-f")
	c := newClaudeLog()
	u := usage(10, 10, 0, 100)
	u["speed"] = "fast"
	u["cache_creation"] = map[string]any{"ephemeral_5m_input_tokens": 40, "ephemeral_1h_input_tokens": 60}
	c.text("msg_f", "m-known", "hi", u)
	writeFile(t, path, c.String())
	r := NewReader(Options{Roots: e.opts.Roots, Pricer: recordingPricer{t: t, want1h: 60, wantFast: true}, Location: time.UTC})
	mustRead(t, r, "f", Source{Agent: api.AgentClaude, Path: path}, Query{})
}

type recordingPricer struct {
	t        *testing.T
	want1h   int64
	wantFast bool
}

func (p recordingPricer) Cost(model string, tk Tokens, fast bool) float64 {
	if tk.CacheWrite1h != p.want1h || fast != p.wantFast {
		p.t.Errorf("priced %+v fast=%v", tk, fast)
	}
	return 0
}

func TestClaudeSidechains(t *testing.T) {
	e := newEnv(t)
	path := e.claudePath("sess-s")
	c := newClaudeLog()
	c.prompt("Research the API", "human")
	c.text("msg_0", "m-known", "On it.", usage(1, 1, 0, 0))
	c.prompt("Then summarize", "human")
	c.toolUse("msg_1", "m-known", "toolu_agent", "Agent", map[string]any{"description": "Read the API docs", "prompt": "…", "subagent_type": "Explore"}, usage(1, 1, 0, 0))
	c.result("toolu_agent", "The API has three endpoints.", false, map[string]any{"agentId": "a1", "status": "completed"})
	writeFile(t, path, c.String())

	sub := filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents")
	s := newClaudeLog()
	s.sidechain = true
	s.at = t0.Add(10 * time.Second)
	s.prompt("Read the API docs and report", "")
	s.toolUse("msg_s1", "m-sub", "toolu_s1", "WebFetch", map[string]any{"url": "https://example.com/api", "prompt": "list endpoints"}, usage(100, 10, 0, 0))
	s.result("toolu_s1", "three endpoints", false, nil)
	s.text("msg_s2", "m-sub", "Three endpoints.", usage(50, 20, 0, 0))
	writeFile(t, filepath.Join(sub, "agent-a1.jsonl"), s.String())
	// A fork of that subagent starts with a copy of its context: the same
	// records, ids and usage. They count once.
	writeFile(t, filepath.Join(sub, "agent-a2.jsonl"), s.String())
	writeFile(t, filepath.Join(sub, "agent-a1.meta.json"), `{"agentType":"Explore","description":"Read the API docs","toolUseId":"toolu_agent"}`)
	writeFile(t, filepath.Join(sub, "notes.txt"), "not a transcript")

	res := mustRead(t, e.reader(), "s", Source{Agent: api.AgentClaude, Path: path}, Query{Limit: 100, CatchUp: true, Now: t0.Add(time.Hour)})
	agent := findItem(t, res.Items, "toolu_agent")
	if agent.Kind != api.ActivitySubagent || agent.Sidechain || agent.Summary != "Agent: Read the API docs" {
		t.Errorf("agent call = %+v", agent)
	}
	fetch := findItem(t, res.Items, "toolu_s1")
	if !fetch.Sidechain || fetch.Turn != 2 || fetch.ToolKind != api.ToolWeb || fetch.Pending {
		t.Errorf("subagent tool = %+v", fetch)
	}
	for _, p := range byKind(res.Items, api.ActivityPrompt) {
		if p.Sidechain {
			t.Errorf("a subagent's task prompt shows as a prompt: %+v", p)
		}
	}
	// Subagent tokens count; the copied response doesn't; the session's
	// model stays the main conversation's.
	if res.Usage.InputTokens != 2+100+50 || res.Usage.OutputTokens != 2+10+20 || res.Usage.Model != "m-known" || res.Usage.Turns != 2 {
		t.Errorf("usage = %+v", res.Usage)
	}
	if !strings.Contains(strings.Join(res.CatchUp.Lines, "\n"), "Started 1 subagent") {
		t.Errorf("catch-up = %q", res.CatchUp.Lines)
	}
}

// Older Claude Code versions wrote subagent records into the main file.
func TestClaudeInlineSidechain(t *testing.T) {
	e := newEnv(t)
	path := e.claudePath("sess-i")
	c := newClaudeLog()
	c.prompt("go", "human")
	c.toolUse("msg_1", "m-known", "toolu_main", "Bash", map[string]any{"command": "make"}, usage(1, 1, 0, 0))
	c.sidechain = true
	c.prompt("subtask prompt", "")
	c.toolUse("msg_s", "m-known", "toolu_side", "Bash", map[string]any{"command": "ls"}, usage(1, 1, 0, 0))
	c.sidechain = false
	c.result("toolu_main", "ok", false, nil)
	writeFile(t, path, c.String())
	res := mustRead(t, e.reader(), "i", Source{Agent: api.AgentClaude, Path: path}, Query{})
	if side := findItem(t, res.Items, "toolu_side"); !side.Sidechain || !side.Pending {
		t.Errorf("inline sidechain tool = %+v (its own chain: the main response doesn't settle it)", side)
	}
	if main := findItem(t, res.Items, "toolu_main"); main.Pending || main.Result != "ok" {
		t.Errorf("main tool = %+v", main)
	}
	if n := len(byKind(res.Items, api.ActivityPrompt)); n != 1 {
		t.Errorf("%d prompts, want 1", n)
	}
}

func TestClaudePendingAndInterrupt(t *testing.T) {
	e := newEnv(t)
	path := e.claudePath("sess-p")
	c := newClaudeLog()
	c.prompt("run it", "human")
	c.toolUse("msg_1", "m-known", "toolu_long", "Bash", map[string]any{"command": "npm run build"}, usage(1, 1, 0, 0))
	writeFile(t, path, c.take())
	r := e.reader()
	src := Source{Agent: api.AgentClaude, Path: path}
	res := mustRead(t, r, "p", src, Query{CatchUp: true, Now: t0.Add(time.Minute)})
	if it := findItem(t, res.Items, "toolu_long"); !it.Pending {
		t.Errorf("tool without a result should be pending: %+v", it)
	}
	// The person interrupts: Claude logs the cancelled result and a notice.
	c.result("toolu_long", "Interrupted by user", true, nil)
	c.add(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}})
	appendFile(t, path, c.take())
	res = mustRead(t, r, "p", src, Query{Cursor: res.Cursor})
	if len(res.Items) != 2 {
		t.Fatalf("cursor read returned %d items, want the updated call and the notice: %+v", len(res.Items), res.Items)
	}
	if res.Items[0].ID != "toolu_long" || res.Items[0].Pending || !res.Items[0].IsError {
		t.Errorf("updated call = %+v", res.Items[0])
	}
	if res.Items[1].Kind != api.ActivitySystem || res.Items[1].Text != "Interrupted by the person" {
		t.Errorf("notice = %+v", res.Items[1])
	}
	if n := len(byKind(res.Items, api.ActivityPrompt)); n != 0 {
		t.Errorf("the interrupt notice is not a prompt")
	}
}

func TestClassifyClaudePrompt(t *testing.T) {
	cases := []struct {
		text, origin string
		kind         promptKind
		shown        string
	}{
		{"hello", "", promptHuman, "hello"},
		{"hello", "human", promptHuman, "hello"},
		{"hello", "peer", promptPeer, "hello"},
		{"", "human", promptSkip, ""},
		{"<command-name>/clear</command-name>", "", promptHuman, "/clear"},
		{"<bash-input>git status</bash-input>", "", promptHuman, "! git status"},
		{"<bash-stdout>On branch main</bash-stdout>", "", promptSkip, ""},
		{"<local-command-stdout>ok</local-command-stdout>", "", promptSkip, ""},
		{"<system-reminder>x</system-reminder>", "", promptSkip, ""},
		{"[Request interrupted by user for tool use]", "", promptInterrupt, ""},
		{"<task-notification>no summary</task-notification>", "", promptNotice, "A background task reported back"},
		{"anything", "task-notification", promptNotice, "A background task reported back"},
	}
	for _, c := range cases {
		kind, shown := classifyClaudePrompt(c.text, c.origin)
		if kind != c.kind || shown != c.shown {
			t.Errorf("classify(%q, %q) = %v %q, want %v %q", c.text, c.origin, kind, shown, c.kind, c.shown)
		}
	}
}

// Text and results are capped; a huge tool output keeps its end.
func TestClaudeCaps(t *testing.T) {
	e := newEnv(t)
	path := e.claudePath("sess-c")
	c := newClaudeLog()
	c.prompt(strings.Repeat("p", 10_000), "human")
	c.toolUse("msg_1", "m-known", "toolu_1", "Bash", map[string]any{"command": "cat big"}, usage(1, 1, 0, 0))
	c.result("toolu_1", strings.Repeat("x", 50_000)+"THE END", false, nil)
	writeFile(t, path, c.String())
	res := mustRead(t, e.reader(), "c", Source{Agent: api.AgentClaude, Path: path}, Query{})
	if p := byKind(res.Items, api.ActivityPrompt)[0]; len(p.Text) > maxText || !strings.HasSuffix(p.Text, "…") {
		t.Errorf("prompt text not capped: %d bytes", len(p.Text))
	}
	if it := findItem(t, res.Items, "toolu_1"); len(it.Result) > maxResult || !strings.HasSuffix(it.Result, "THE END") {
		t.Errorf("result not tail-capped: %d bytes", len(it.Result))
	}
}

func TestFeedKeepsNewestItems(t *testing.T) {
	e := newEnv(t)
	path := e.claudePath("sess-n")
	c := newClaudeLog()
	for i := 0; i < 50; i++ {
		c.prompt("prompt", "human")
	}
	writeFile(t, path, c.String())
	r := NewReader(Options{Roots: e.opts.Roots, MaxItems: 10, Location: time.UTC})
	res := mustRead(t, r, "n", Source{Agent: api.AgentClaude, Path: path}, Query{Limit: 1000})
	if len(res.Items) != 10 || res.Items[0].Turn != 41 || res.Items[9].Turn != 50 {
		t.Errorf("kept %d items, turns %d..%d", len(res.Items), res.Items[0].Turn, res.Items[len(res.Items)-1].Turn)
	}
	res = mustRead(t, r, "n", Source{Agent: api.AgentClaude, Path: path}, Query{Limit: 3})
	if len(res.Items) != 3 || res.Items[2].Turn != 50 {
		t.Errorf("limit keeps the newest: %+v", res.Items)
	}
}

func TestReaderEvictsSessions(t *testing.T) {
	e := newEnv(t)
	r := NewReader(Options{Roots: e.opts.Roots, MaxSessions: 2, Location: time.UTC})
	for _, id := range []string{"a", "b", "c"} {
		path := e.claudePath("sess-" + id)
		c := newClaudeLog()
		c.prompt("hi "+id, "human")
		writeFile(t, path, c.String())
		mustRead(t, r, id, Source{Agent: api.AgentClaude, Path: path}, Query{})
	}
	r.mu.Lock()
	n := len(r.sessions)
	_, hasA := r.sessions["a"]
	r.mu.Unlock()
	if n != 2 || hasA {
		t.Errorf("cache holds %d sessions (a: %v), want 2 without the oldest", n, hasA)
	}
	r.Forget("b")
	if _, err := os.Stat(e.claudePath("sess-b")); err != nil {
		t.Fatal(err)
	}
}
