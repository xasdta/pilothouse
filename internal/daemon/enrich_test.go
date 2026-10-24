package daemon

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/paths"
)

// writeTranscript writes JSONL records under the (test) user's Claude
// projects directory and returns the path.
func writeTranscript(t testing.TB, dir string, records ...any) string {
	t.Helper()
	p := filepath.Join(paths.UserHome(), dir, "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, r := range records {
		line, _ := json.Marshal(r)
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func claudeText(role, text string) map[string]any {
	return map[string]any{"type": role, "message": map[string]any{"role": role, "content": []any{map[string]any{"type": "text", "text": text}}}}
}

func claudeTool() map[string]any {
	return map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant",
		"content": []any{map[string]any{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": "ls"}}}}}
}

func toolResult() map[string]any {
	return map[string]any{"type": "user", "message": map[string]any{"role": "user",
		"content": []any{map[string]any{"type": "tool_result", "content": "ok"}}}}
}

func isolatedHome(t *testing.T) {
	t.Setenv("PILOTHOUSE_USER_HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")
}

func TestLastWords(t *testing.T) {
	isolatedHome(t)
	claude := ".claude/projects/-src-app"
	p := writeTranscript(t, claude,
		claudeText("user", "Ship the release"),
		claudeText("assistant", "Tests pass. I'll push the tag now."),
		claudeTool(), toolResult(), claudeTool(),
	)
	if got := lastWords(p); got != "Tests pass. I'll push the tag now." {
		t.Fatalf("claude = %q", got)
	}
	// A prompt after the last text: the agent said nothing this turn.
	p = writeTranscript(t, claude, claudeText("assistant", "Done."), claudeText("user", "now deploy"), claudeTool())
	if got := lastWords(p); got != "" {
		t.Fatalf("stale words = %q", got)
	}
	// Prompts given as plain strings count too; sidechains (subagents) don't.
	side := claudeText("assistant", "subagent chatter")
	side["isSidechain"] = true
	p = writeTranscript(t, claude, claudeText("assistant", "Main thread."),
		map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": "go on"}}, side)
	if got := lastWords(p); got != "" {
		t.Fatalf("string prompt / sidechain = %q", got)
	}
	// Codex rollouts.
	codex := ".codex/sessions/2026/10/24"
	p = writeTranscript(t, codex,
		map[string]any{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "fix it"}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": "Patching   the\nparser."}}}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call", "name": "shell"}},
	)
	if got := lastWords(p); got != "Patching the parser." {
		t.Fatalf("codex = %q", got)
	}
	// Credentials are redacted and long words keep their end.
	p = writeTranscript(t, claude, claudeText("assistant", strings.Repeat("blah ", 200)+"using ghp_abcdefghijklmnopqrstuvwxyz0123456789 to push"))
	got := lastWords(p)
	if strings.Contains(got, "ghp_") || !strings.HasSuffix(got, "using [redacted] to push") || !strings.HasPrefix(got, "…") || len([]rune(got)) > maxContext {
		t.Fatalf("trimmed = %q", got)
	}
	// Only the tail is read; a cut first line is skipped.
	big := []any{claudeText("assistant", "early")}
	for i := 0; i < 2000; i++ {
		big = append(big, claudeTool())
	}
	big = append(big, claudeText("assistant", "late words"), claudeTool())
	if got := lastWords(writeTranscript(t, claude, big...)); got != "late words" {
		t.Fatalf("tail = %q", got)
	}
}

func TestLastWordsConfinement(t *testing.T) {
	isolatedHome(t)
	inside := writeTranscript(t, ".claude/projects/x", claudeText("assistant", "fine"))
	outside := filepath.Join(t.TempDir(), "evil.jsonl")
	if err := os.WriteFile(outside, []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"SECRET"}]}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(paths.UserHome(), ".claude", "projects", "x", "link.jsonl")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	notJSONL := filepath.Join(filepath.Dir(inside), "notes.txt")
	_ = os.WriteFile(notJSONL, []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"SECRET"}]}}`), 0o600)
	for _, p := range []string{outside, link, notJSONL, "relative.jsonl", filepath.Join(paths.UserHome(), ".claude", "projects", "..", "..", strings.TrimPrefix(outside, "/")), "/etc/passwd", ""} {
		if got := lastWords(p); got != "" {
			t.Errorf("%s: read %q", p, got)
		}
	}
	if got := lastWords(inside); got != "fine" {
		t.Fatalf("inside = %q", got)
	}
}

func TestEnrichedPermissionRequest(t *testing.T) {
	h := newHarness(t)
	h.presence.setAway(true)
	h.start(t)
	waitFor(t, "away", func() bool { return h.d.Presence().State == api.PresenceAway })
	repo := gitRepo(t, "main")
	transcript := writeTranscript(t, ".claude/projects/app", claudeText("assistant", "Pushing the fix to main."), claudeTool())

	open := func(ev adapters.HookEvent) api.Request {
		t.Helper()
		done := make(chan fakeOutcome, 1)
		go func() { done <- h.hook(bg, t, ev) }()
		r := h.openRequest(t)
		if _, err := h.c.Decide(bg, r.ID, api.DecideBody{Behavior: api.BehaviorDeny}); err != nil {
			t.Fatal(err)
		}
		<-done
		return r
	}
	ev := permission("git push -f origin main", "")
	ev.Cwd, ev.TranscriptPath = repo, transcript
	r := open(ev)
	if len(r.Risk) < 2 || r.Risk[0].Code != api.RiskForcePush || r.Risk[0].Text != "Force-pushes to main" || r.Context != "Pushing the fix to main." || r.Preview != nil {
		t.Fatalf("shell request = %+v", r)
	}

	src := filepath.Join(repo, "main.go")
	if err := os.WriteFile(src, []byte("package main\n\nfunc main() {\n\tprintln(1)\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(map[string]string{"file_path": src, "old_string": "println(1)", "new_string": "println(2)"})
	r = open(adapters.HookEvent{Kind: adapters.HookPermission, Tool: "Edit", ToolKind: api.ToolEdit, Paths: []string{src},
		Input: in, Summary: "Edit: main.go", Cwd: repo})
	if r.Preview == nil || len(r.Preview.Files) != 1 || r.Preview.Files[0].Path != "main.go" || r.Risk != nil ||
		r.Preview.Files[0].Diff != "@@ -1,5 +1,5 @@\n package main\n \n func main() {\n-\tprintln(1)\n+\tprintln(2)\n }\n" {
		t.Fatalf("edit request = %+v (%+v)", r, r.Preview)
	}
	// What was stored is what devices get (snapshot and events).
	got, err := h.c.Request(bg, r.ID)
	if err != nil || got.Preview == nil || got.Preview.Added != 1 {
		t.Fatalf("stored = %+v, %v", got.Preview, err)
	}
}

func TestEnrichNeverFails(t *testing.T) {
	d := &Daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, req := range []api.Request{
		{},
		{ToolKind: api.ToolEdit, Tool: "Edit", Input: json.RawMessage(`{"file_path": {}}`)},
		{ToolKind: api.ToolShell, Command: strings.Repeat("$(", 5000)},
		{ToolKind: api.ToolEdit, Tool: "apply_patch", Input: json.RawMessage(`{"command":"*** Begin Patch\n*** Update File: \n@@\n"}`)},
	} {
		d.enrichRequest(&req, &hookCall{ev: adapters.HookEvent{TranscriptPath: "/nonexistent.jsonl"}})
	}
}

func BenchmarkEnrichRequest(b *testing.B) {
	b.Setenv("PILOTHOUSE_USER_HOME", b.TempDir())
	repo := b.TempDir()
	var src strings.Builder
	for i := 0; i < 2000; i++ {
		src.WriteString("func f() int { return 1 }\n")
	}
	file := filepath.Join(repo, "big.go")
	if err := os.WriteFile(file, []byte(src.String()+"// end\n"), 0o600); err != nil {
		b.Fatal(err)
	}
	var records []any
	for i := 0; i < 200; i++ {
		records = append(records, claudeText("assistant", "Working on it, step by step."), claudeTool(), toolResult())
	}
	transcript := writeTranscript(b, ".claude/projects/bench", records...)
	in, _ := json.Marshal(map[string]string{"file_path": file, "old_string": "// end", "new_string": "// the end"})
	d := &Daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	reqs := []api.Request{
		{Tool: "Edit", ToolKind: api.ToolEdit, Paths: []string{file}, Input: in},
		{Tool: "Bash", ToolKind: api.ToolShell, Command: "cd src && go test ./... 2>&1 | tail -50 && git push origin main"},
	}
	h := &hookCall{cwd: repo, root: repo, branch: "main", ev: adapters.HookEvent{TranscriptPath: transcript}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := reqs[i%len(reqs)]
		d.enrichRequest(&r, h)
	}
}
