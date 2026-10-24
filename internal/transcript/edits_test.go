package transcript

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

func TestLineDelta(t *testing.T) {
	cases := []struct {
		old, new       string
		added, removed int
	}{
		{"", "", 0, 0},
		{"", "a\nb\n", 2, 0},
		{"a\nb", "", 0, 2},
		{"a\nb\nc\n", "a\nX\nc\n", 1, 1},
		{"a\nb\nc", "a\nb\nc\nd", 1, 0},
		{"x\ny\nz", "y", 0, 2},
		{"same", "same", 0, 0},
		{"a\nb\nc\nd", "d\nc\nb\na", 3, 3},
	}
	for _, c := range cases {
		a, r := lineDelta(c.old, c.new)
		if a != c.added || r != c.removed {
			t.Errorf("lineDelta(%q, %q) = +%d/−%d, want +%d/−%d", c.old, c.new, a, r, c.added, c.removed)
		}
	}
	// Too big for the LCS table: every changed line counts.
	big := strings.Repeat("x\n", 2000)
	other := strings.Repeat("y\n", 2000)
	if a, r := lineDelta(big, other); a != 2000 || r != 2000 {
		t.Errorf("big delta = +%d/−%d", a, r)
	}
}

func TestClaudeEdits(t *testing.T) {
	in := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	cases := []struct {
		tool  string
		input any
		want  []fileEdit
	}{
		{"Edit", map[string]any{"file_path": "/r/a.go", "old_string": "x\n", "new_string": "y\nz\n"}, []fileEdit{{"/r/a.go", api.FileEdit, 2, 1}}},
		{"Edit", map[string]any{"file_path": "/r/new.go", "old_string": "", "new_string": "package x\n"}, []fileEdit{{"/r/new.go", api.FileCreate, 1, 0}}},
		{"MultiEdit", map[string]any{"file_path": "/r/b.go", "edits": []any{
			map[string]any{"old_string": "a", "new_string": "b"}, map[string]any{"old_string": "c\nd", "new_string": ""}}},
			[]fileEdit{{"/r/b.go", api.FileEdit, 1, 3}}},
		{"Write", map[string]any{"file_path": "/r/c.txt", "content": "1\n2\n3"}, []fileEdit{{"/r/c.txt", api.FileCreate, 3, 0}}},
		{"NotebookEdit", map[string]any{"notebook_path": "/r/n.ipynb", "new_source": "print(1)\n"}, []fileEdit{{"/r/n.ipynb", api.FileEdit, 1, 0}}},
		{"Edit", map[string]any{"old_string": "x"}, nil},
		{"Read", map[string]any{"file_path": "/r/a.go"}, nil},
	}
	for _, c := range cases {
		if got := claudeEdits(c.tool, in(c.input)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("claudeEdits(%s, %v) = %+v, want %+v", c.tool, c.input, got, c.want)
		}
	}
	if got := claudeEdits("Edit", json.RawMessage(`[1,2]`)); got != nil {
		t.Errorf("malformed input = %+v", got)
	}
}

func TestClaudeResultEdits(t *testing.T) {
	cases := []struct {
		raw  string
		want []fileEdit
		ok   bool
	}{
		{`{"filePath":"/r/a","structuredPatch":[{"lines":[" k","-a","+b","+c"]}]}`, []fileEdit{{"/r/a", api.FileEdit, 2, 1}}, true},
		{`{"type":"create","filePath":"/r/n","content":"1\n2\n","structuredPatch":[]}`, []fileEdit{{"/r/n", api.FileCreate, 2, 0}}, true},
		{`{"type":"update","filePath":"/r/o","structuredPatch":[{"lines":["-x","+y"]}]}`, []fileEdit{{"/r/o", api.FileEdit, 1, 1}}, true},
		{`{"type":"update","filePath":"/r/o","structuredPatch":[]}`, []fileEdit{{"/r/o", api.FileOverwrite, 0, 0}}, true},
		{`{"stdout":"x"}`, nil, false},
		{`"a string"`, nil, false},
		{``, nil, false},
	}
	for _, c := range cases {
		got, ok := claudeResultEdits(json.RawMessage(c.raw))
		if ok != c.ok || !reflect.DeepEqual(got, c.want) {
			t.Errorf("claudeResultEdits(%s) = %+v %v, want %+v %v", c.raw, got, ok, c.want, c.ok)
		}
	}
}

func TestPatchEdits(t *testing.T) {
	patch := `*** Begin Patch
*** Add File: docs/new.md
+# Title
+text
*** Update File: src/a.go
*** Move to: src/b.go
@@ func x
-old
+new
 keep
*** Delete File: tmp/gone.txt
*** Update File: /abs/c.go
@@
-1
-2
*** End Patch`
	want := []fileEdit{
		{"/repo/docs/new.md", api.FileCreate, 2, 0},
		{"/repo/src/b.go", api.FileRename, 1, 1},
		{"/repo/tmp/gone.txt", api.FileDelete, 0, 0},
		{"/abs/c.go", api.FileEdit, 0, 2},
	}
	if got := patchEdits(patch, "/repo"); !reflect.DeepEqual(got, want) {
		t.Errorf("patchEdits = %+v\nwant %+v", got, want)
	}
	cmd := "apply_patch <<'EOF'\n" + patch + "\nEOF\n"
	if got := patchBody(cmd); !strings.HasPrefix(got, "*** Begin Patch") || !strings.HasSuffix(got, "*** End Patch") {
		t.Errorf("patchBody = %q", got)
	}
	if patchBody("echo hi") != "" {
		t.Error("no patch in a plain command")
	}
}

func TestMergeOp(t *testing.T) {
	cases := []struct{ prev, next, want api.FileOp }{
		{api.FileEdit, api.FileEdit, api.FileEdit},
		{api.FileCreate, api.FileEdit, api.FileCreate},
		{api.FileCreate, api.FileDelete, api.FileDelete},
		{api.FileDelete, api.FileCreate, api.FileCreate},
		{api.FileEdit, api.FileOverwrite, api.FileOverwrite},
		{api.FileCreate, api.FileRename, api.FileRename},
	}
	for _, c := range cases {
		if got := mergeOp(c.prev, c.next); got != c.want {
			t.Errorf("mergeOp(%s, %s) = %s, want %s", c.prev, c.next, got, c.want)
		}
	}
}

func TestTestRunner(t *testing.T) {
	cases := map[string]string{
		"npm test":                       "npm test",
		"cd web && npm run test -- --ci": "npm run test",
		"pnpm test:unit":                 "pnpm test:unit",
		"go test ./...":                  "go test",
		"cargo test --all":               "cargo test",
		"python -m pytest -q":            "python -m pytest",
		"pytest tests/":                  "pytest",
		"make -j4 test":                  "make -j4 test",
		"./gradlew clean test":           "./gradlew clean test",
		"npx vitest run":                 "vitest",
		"echo test":                      "",
		"npm install":                    "",
		"git commit -m 'add go test'":    "", // quoted text is no test run
	}
	for cmd, want := range cases {
		if got := testRunner(cmd); got != want {
			t.Errorf("testRunner(%q) = %q, want %q", cmd, got, want)
		}
	}
}

// The catch-up sentences, from a feed built by hand.
func TestCatchUpSentences(t *testing.T) {
	f := newFeed(api.AgentClaude, "/repo", nil, time.UTC, 100)
	since := t0
	now := t0.Add(time.Hour)
	add := func(it *item) { f.add(it) }
	id := 0
	tool := func(at time.Time, summary, command string, edits []fileEdit, isErr bool) {
		id++
		it := &item{ActivityItem: api.ActivityItem{ID: itoa(id), Kind: api.ActivityTool, At: at, Summary: summary, IsError: isErr}, command: command, edits: edits, fold: true}
		add(it)
	}
	// Before the catch-up start: ignored.
	tool(t0.Add(-time.Minute), "Run: rm old", "rm old", nil, true)
	for i := 0; i < 10; i++ {
		tool(t0.Add(time.Minute), "Read: x", "", nil, false)
	}
	tool(t0.Add(2*time.Minute), "Edit: a.go", "", []fileEdit{{"/repo/a.go", api.FileEdit, 100, 20}}, false)
	tool(t0.Add(3*time.Minute), "Edit: b.go", "", []fileEdit{{"/repo/b.go", api.FileEdit, 20, 10}, {"/repo/c.go", api.FileCreate, 0, 0},
		{"/repo/d.go", api.FileEdit, 0, 0}, {"/repo/e.go", api.FileEdit, 0, 0}}, false)
	tool(now.Add(-2*time.Minute), "Run: npm test", "npm test", nil, false)
	id++
	add(&item{ActivityItem: api.ActivityItem{ID: itoa(id), Kind: api.ActivityTool, At: now.Add(-time.Minute), Summary: "Asked you 1 question"}, asks: 1})
	cu := f.catchUp(since, now, CatchUpExtra{Waiting: []string{"Run: git push origin main"}, Approvals: 2})
	want := []string{
		"Waiting for you: Run: git push origin main",
		"Tests: npm test passed (last run 2 min ago)",
		"Edited 5 files (+120/−30)",
		"Ran 13 tool calls",
		"Asked you 1 question",
		"Needed your approval 2 times",
	}
	if !reflect.DeepEqual(cu.Lines, want) {
		t.Errorf("lines = %q\nwant  %q", cu.Lines, want)
	}
	if cu.ToolCalls != 13 || cu.Files != 5 || cu.Errors != 0 || !cu.Since.Equal(since) {
		t.Errorf("counts = %+v", cu)
	}
	// Same input, same output.
	again := f.catchUp(since, now, CatchUpExtra{Waiting: []string{"Run: git push origin main"}, Approvals: 2})
	if !reflect.DeepEqual(again, cu) {
		t.Error("catch-up is not deterministic")
	}
	// Nothing after the start.
	quiet := f.catchUp(now, now.Add(time.Minute), CatchUpExtra{})
	if !reflect.DeepEqual(quiet.Lines, []string{"Nothing new since 10:00"}) {
		t.Errorf("quiet = %q", quiet.Lines)
	}
	if empty := newFeed(api.AgentClaude, "", nil, time.UTC, 10).catchUp(time.Time{}, now, CatchUpExtra{}); empty.Lines[0] != "Nothing yet" {
		t.Errorf("empty = %q", empty.Lines)
	}
}

func TestAgo(t *testing.T) {
	now := t0
	cases := map[time.Duration]string{
		10 * time.Second: "just now", 90 * time.Second: "1 min ago", 59 * time.Minute: "59 min ago",
		3 * time.Hour: "3 h ago",
	}
	for d, want := range cases {
		if got := ago(now.Add(-d), now); got != want {
			t.Errorf("ago(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestShortFormats(t *testing.T) {
	if s := shortDuration(125 * time.Second); s != "2m 5s" {
		t.Errorf("shortDuration = %q", s)
	}
	if s := shortDuration(2*time.Hour + 3*time.Minute); s != "2h 3m" {
		t.Errorf("shortDuration = %q", s)
	}
	if s := shortCount(182_400); s != "182k" {
		t.Errorf("shortCount = %q", s)
	}
	if s := shortCount(1_500_000); s != "1.5M" {
		t.Errorf("shortCount = %q", s)
	}
	if s := capTail("héllo wörld", 8); !strings.HasPrefix(s, "…") || len(s) > 8 {
		t.Errorf("capTail = %q", s)
	}
}
