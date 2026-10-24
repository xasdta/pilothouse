package preview

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

type fixture struct {
	t    *testing.T
	home string
	repo string
	priv string // stands in for the Pilothouse home
}

func newFixture(t *testing.T) *fixture {
	home := t.TempDir()
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	f := &fixture{t: t, home: home, repo: filepath.Join(home, "src", "app"), priv: filepath.Join(home, ".pilothouse")}
	for _, d := range []string{f.repo, f.priv} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *fixture) write(rel, content string) string {
	f.t.Helper()
	p := rel
	if !filepath.IsAbs(rel) {
		p = filepath.Join(f.repo, rel)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *fixture) build(tool string, input any) *api.Preview {
	f.t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		f.t.Fatal(err)
	}
	return Build(Call{Tool: tool, Kind: api.ToolEdit, Input: raw, Cwd: f.repo, Repo: f.repo, Home: f.home, Private: []string{f.priv}})
}

func numbered(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

func one(t *testing.T, p *api.Preview) api.FilePreview {
	t.Helper()
	if p == nil || len(p.Files) != 1 {
		t.Fatalf("preview = %+v", p)
	}
	return p.Files[0]
}

func TestEditWithContext(t *testing.T) {
	f := newFixture(t)
	f.write("src/main.go", numbered(50))
	p := f.build("Edit", map[string]any{"file_path": filepath.Join(f.repo, "src/main.go"), "old_string": "line 20\n", "new_string": "line twenty\nline 20.5\n"})
	fp := one(t, p)
	want := "@@ -17,7 +17,8 @@\n line 17\n line 18\n line 19\n-line 20\n+line twenty\n+line 20.5\n line 21\n line 22\n line 23\n"
	if fp.Path != "src/main.go" || fp.Op != api.FileEdit || fp.Lang != "go" || fp.Diff != want || fp.Added != 2 || fp.Removed != 1 {
		t.Fatalf("file = %+v\n%s", fp, fp.Diff)
	}
	if p.Added != 2 || p.Removed != 1 || p.Truncated {
		t.Fatalf("totals = %+v", p)
	}
}

func TestEditReplaceAllAndMultiEdit(t *testing.T) {
	f := newFixture(t)
	f.write("a.py", "x = 1\ny = x\nz = x\n")
	fp := one(t, f.build("Edit", map[string]any{"file_path": "a.py", "old_string": "x", "new_string": "w", "replace_all": true}))
	if fp.Added != 3 || fp.Removed != 3 || fp.Lang != "py" {
		t.Fatalf("replace_all: %+v", fp)
	}
	fp = one(t, f.build("MultiEdit", map[string]any{"file_path": "a.py", "edits": []map[string]any{
		{"old_string": "x = 1", "new_string": "x = 2"},
		{"old_string": "z = x", "new_string": "z = x + 1"},
	}}))
	if !strings.HasPrefix(fp.Diff, "@@ -1,3 +1,3 @@\n-x = 1\n+x = 2\n y = x\n-z = x\n+z = x + 1\n") {
		t.Fatalf("multiedit:\n%s", fp.Diff)
	}
}

func TestEditStaleOrUnreadableFallsBackToInput(t *testing.T) {
	f := newFixture(t)
	f.write("b.txt", "unrelated\n")
	fp := one(t, f.build("Edit", map[string]any{"file_path": "b.txt", "old_string": "gone", "new_string": "new"}))
	if fp.Diff != "@@ -1 +1 @@\n-gone\n+new\n" || fp.Op != api.FileEdit {
		t.Fatalf("stale: %+v", fp)
	}
	// A new file through Edit with an empty old_string is a create.
	fp = one(t, f.build("Edit", map[string]any{"file_path": "new.md", "old_string": "", "new_string": "# hi\n"}))
	if fp.Op != api.FileCreate || fp.Diff != "@@ -0,0 +1 @@\n+# hi\n" || fp.Lang != "md" {
		t.Fatalf("create via edit: %+v", fp)
	}
}

func TestSecretsAndPrivateFilesAreNeverRead(t *testing.T) {
	f := newFixture(t)
	f.write(".env", "API_KEY=supersecret\nDEBUG=1\nOTHER=2\n")
	fp := one(t, f.build("Edit", map[string]any{"file_path": ".env", "old_string": "DEBUG=1", "new_string": "DEBUG=0"}))
	if strings.Contains(fp.Diff, "supersecret") || fp.Diff != "@@ -1 +1 @@\n-DEBUG=1\n+DEBUG=0\n" {
		t.Fatalf(".env context leaked: %q", fp.Diff)
	}
	// The Pilothouse home, agent settings, and symlinks into them.
	cfg := f.write(filepath.Join(f.priv, "config.yaml"), "token: abc123\nweb: x\n")
	fp = one(t, f.build("Edit", map[string]any{"file_path": cfg, "old_string": "web: x", "new_string": "web: y"}))
	if strings.Contains(fp.Diff, "abc123") {
		t.Fatalf("private home read: %q", fp.Diff)
	}
	settings := f.write(filepath.Join(f.home, ".claude", "settings.json"), `{"env":{"ANTHROPIC_API_KEY":"sk-secret"},"x":1}`+"\n")
	fp = one(t, f.build("Write", map[string]any{"file_path": settings, "content": "{}\n"}))
	if strings.Contains(fp.Diff, "sk-secret") || fp.Op != api.FileOverwrite || fp.Removed != 0 {
		t.Fatalf("settings read: %+v", fp)
	}
	link := filepath.Join(f.repo, "notes.txt")
	if err := os.Symlink(cfg, link); err != nil {
		t.Fatal(err)
	}
	fp = one(t, f.build("Write", map[string]any{"file_path": link, "content": "x\n"}))
	if strings.Contains(fp.Diff, "abc123") {
		t.Fatalf("symlink followed into private dir: %q", fp.Diff)
	}
	key := f.write(filepath.Join(f.home, ".ssh", "id_ed25519"), "-----BEGIN OPENSSH PRIVATE KEY-----\nxyz\n")
	fp = one(t, f.build("Write", map[string]any{"file_path": key, "content": "x\n"}))
	if strings.Contains(fp.Diff, "PRIVATE KEY") {
		t.Fatalf("ssh key read: %q", fp.Diff)
	}
}

func TestWriteCreateOverwriteBinaryHuge(t *testing.T) {
	f := newFixture(t)
	fp := one(t, f.build("Write", map[string]any{"file_path": "cmd/new.ts", "content": "export {}\n"}))
	if fp.Op != api.FileCreate || fp.Added != 1 || fp.Removed != 0 || fp.Lang != "ts" || fp.Path != "cmd/new.ts" {
		t.Fatalf("create: %+v", fp)
	}
	f.write("old.txt", "a\nb\n")
	fp = one(t, f.build("Write", map[string]any{"file_path": "old.txt", "content": "a\nc\n"}))
	if fp.Op != api.FileOverwrite || fp.Diff != "@@ -1,2 +1,2 @@\n a\n-b\n+c\n" {
		t.Fatalf("overwrite: %+v", fp)
	}
	f.write("img.png", "\x89PNG\x00\x00binary")
	fp = one(t, f.build("Write", map[string]any{"file_path": "img.png", "content": "text\n"}))
	if fp.Op != api.FileOverwrite || fp.Diff != "" {
		t.Fatalf("binary: %+v", fp)
	}
	fp = one(t, f.build("Write", map[string]any{"file_path": "blob.bin", "content": "a\x00b"}))
	if fp.Diff != "" || fp.Op != api.FileCreate {
		t.Fatalf("binary content: %+v", fp)
	}
	big := f.write("big.log", strings.Repeat("x", MaxFileBytes+10)+"\n")
	fp = one(t, f.build("Edit", map[string]any{"file_path": big, "old_string": "xxxx", "new_string": "yyyy"}))
	if fp.Diff != "@@ -1 +1 @@\n-xxxx\n+yyyy\n" {
		t.Fatalf("huge file read: %q", fp.Diff)
	}
	var long strings.Builder
	for i := 0; i < 4000; i++ {
		fmt.Fprintf(&long, "generated line %d with some padding text\n", i)
	}
	fp = one(t, f.build("Write", map[string]any{"file_path": "gen.txt", "content": long.String()}))
	if !fp.Truncated || len(fp.Diff) > MaxDiffBytes || fp.Added != 4000 {
		t.Fatalf("huge diff: truncated=%v len=%d added=%d", fp.Truncated, len(fp.Diff), fp.Added)
	}
	// Megabytes of content: counted, the start shown, no full diff.
	huge := strings.Repeat("0123456789abcdef\n", 120000) // ~2 MB
	start := time.Now()
	fp = one(t, f.build("Write", map[string]any{"file_path": "huge.txt", "content": huge}))
	if !fp.Truncated || fp.Added != 120000 || len(fp.Diff) > MaxDiffBytes || fp.Diff == "" || time.Since(start) > time.Second {
		t.Fatalf("2 MB write: truncated=%v added=%d len=%d took %v", fp.Truncated, fp.Added, len(fp.Diff), time.Since(start))
	}
	fp = one(t, f.build("Edit", map[string]any{"file_path": "huge.txt", "old_string": huge, "new_string": "x\n"}))
	if !fp.Truncated || fp.Removed != 120000 || fp.Added != 1 {
		t.Fatalf("2 MB edit: truncated=%v +%d -%d", fp.Truncated, fp.Added, fp.Removed)
	}
	if p := Build(Call{Tool: "Write", Input: json.RawMessage(`{"file_path":"x","content":"` + strings.Repeat("a", MaxInputBytes) + `"}`), Home: f.home}); p != nil {
		t.Fatal("oversized input previewed")
	}
}

func TestCRLFAndNoTrailingNewline(t *testing.T) {
	f := newFixture(t)
	f.write("win.txt", "one\r\ntwo\r\nthree\r\n")
	fp := one(t, f.build("Edit", map[string]any{"file_path": "win.txt", "old_string": "two", "new_string": "2"}))
	if fp.Diff != "@@ -1,3 +1,3 @@\n one\n-two\n+2\n three\n" {
		t.Fatalf("crlf: %q", fp.Diff)
	}
	f.write("tail.txt", "a\nb")
	fp = one(t, f.build("Edit", map[string]any{"file_path": "tail.txt", "old_string": "b", "new_string": "c"}))
	if fp.Diff != "@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+c\n\\ No newline at end of file\n" {
		t.Fatalf("no eol: %q", fp.Diff)
	}
}

func TestFIFODoesNotBlock(t *testing.T) {
	f := newFixture(t)
	fifo := filepath.Join(f.repo, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	done := make(chan *api.Preview, 1)
	go func() { done <- f.build("Write", map[string]any{"file_path": fifo, "content": "x\n"}) }()
	select {
	case p := <-done:
		if fp := one(t, p); fp.Op != api.FileOverwrite {
			t.Fatalf("fifo: %+v", fp)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("preview blocked on a FIFO")
	}
}

func TestNotebookEdit(t *testing.T) {
	f := newFixture(t)
	nb := f.write("nb.ipynb", `{"cells":[{"id":"c1","cell_type":"code","source":["import os\n","print(1)\n"]},{"id":"c2","cell_type":"markdown","source":"# Title"}],"metadata":{"kernelspec":{"language":"python"}}}`)
	fp := one(t, f.build("NotebookEdit", map[string]any{"notebook_path": nb, "cell_id": "c1", "new_source": "import os\nprint(2)"}))
	if fp.Diff != "@@ -1,2 +1,2 @@\n import os\n-print(1)\n+print(2)\n" || fp.Lang != "py" {
		t.Fatalf("replace: %+v", fp)
	}
	fp = one(t, f.build("NotebookEdit", map[string]any{"notebook_path": nb, "cell_id": "c2", "edit_mode": "delete"}))
	if fp.Removed != 1 || fp.Lang != "md" {
		t.Fatalf("delete: %+v", fp)
	}
	fp = one(t, f.build("NotebookEdit", map[string]any{"notebook_path": nb, "cell_id": "c2", "edit_mode": "insert", "cell_type": "code", "new_source": "x = 1"}))
	if fp.Added != 1 || fp.Removed != 0 {
		t.Fatalf("insert: %+v", fp)
	}
}

func TestOpenCodeShapes(t *testing.T) {
	f := newFixture(t)
	f.write("src/x.js", "let a = 1\n")
	fp := one(t, f.build("edit", map[string]any{"filePath": filepath.Join(f.repo, "src/x.js"), "oldString": "1", "newString": "2", "replaceAll": false}))
	if fp.Diff != "@@ -1 +1 @@\n-let a = 1\n+let a = 2\n" || fp.Lang != "js" {
		t.Fatalf("opencode edit: %+v", fp)
	}
	fp = one(t, f.build("write", map[string]any{"filePath": "src/y.js", "content": "export default 1\n"}))
	if fp.Op != api.FileCreate || fp.Added != 1 {
		t.Fatalf("opencode write: %+v", fp)
	}
}

const codexPatch = `*** Begin Patch
*** Add File: docs/new.md
+# New
+
+text
*** Update File: src/lib.rs
@@ fn main() {
-    println!("old");
+    println!("new");
*** Delete File: old.txt
*** Update File: src/a.rs
*** Move to: src/b.rs
@@
 fn a() {}
+fn b() {}
*** End Patch`

func TestApplyPatch(t *testing.T) {
	f := newFixture(t)
	f.write("src/lib.rs", "use std::io;\n\nfn helper() {\n    println!(\"old\");\n}\n\nfn main() {\n    println!(\"old\");\n}\n")
	f.write("old.txt", "bye\nnow\n")
	f.write("src/a.rs", "fn a() {}\n")
	p := f.build("apply_patch", map[string]any{"command": codexPatch})
	if p == nil || len(p.Files) != 4 {
		t.Fatalf("files = %+v", p)
	}
	add, upd, del, mv := p.Files[0], p.Files[1], p.Files[2], p.Files[3]
	if add.Op != api.FileCreate || add.Path != "docs/new.md" || add.Added != 3 {
		t.Fatalf("add: %+v", add)
	}
	// The anchor puts the hunk in main(), not helper(): real line numbers.
	if upd.Op != api.FileEdit || !strings.Contains(upd.Diff, "@@ -5,5 +5,5 @@") || upd.Added != 1 || upd.Removed != 1 {
		t.Fatalf("update:\n%s", upd.Diff)
	}
	if del.Op != api.FileDelete || del.Removed != 2 {
		t.Fatalf("delete: %+v", del)
	}
	if mv.Op != api.FileRename || mv.Path != "src/b.rs" || mv.Added != 1 {
		t.Fatalf("move: %+v", mv)
	}
	if p.Added != 5 || p.Removed != 3 {
		t.Fatalf("totals: +%d -%d", p.Added, p.Removed)
	}

	// Unreadable target: the hunks as written, numbered from 1.
	f2 := newFixture(t)
	p = f2.build("apply_patch", map[string]any{"command": "*** Begin Patch\n*** Update File: gone.go\n@@ func x() {\n a\n-b\n+c\n*** End Patch\n"})
	fp := one(t, p)
	if fp.Diff != "@@ -1,2 +1,2 @@ func x() {\n a\n-b\n+c\n" {
		t.Fatalf("fallback: %q", fp.Diff)
	}
}

func TestApplyPatchFuzzyAndEOF(t *testing.T) {
	out, ok := applyHunks("a\nb  \nc\nb\n", []patchHunk{{lines: []patchLine{{' ', "b"}, {'+', "x"}}, eof: true}})
	if !ok || out != "a\nb  \nc\nb\nx\n" {
		t.Fatalf("eof: %q %v", out, ok)
	}
	out, ok = applyHunks("a\nb  \nc\n", []patchHunk{{lines: []patchLine{{'-', "b"}, {'+', "B"}}}})
	if !ok || out != "a\nB\nc\n" {
		t.Fatalf("trailing-space match: %q %v", out, ok)
	}
	if _, ok := applyHunks("a\n", []patchHunk{{lines: []patchLine{{'-', "zzz"}}}}); ok {
		t.Fatal("non-matching hunk applied")
	}
}

func TestCodexShellPatch(t *testing.T) {
	f := newFixture(t)
	f.write("pkg/x.go", "package x\n")
	cmd := "cd pkg && apply_patch <<'EOF'\n*** Begin Patch\n*** Update File: x.go\n@@\n-package x\n+package y\n*** End Patch\nEOF\n"
	p := Build(Call{Tool: "Bash", Kind: api.ToolEdit, Command: cmd, Input: json.RawMessage(`{"command":` + jsonStr(cmd) + `}`),
		Cwd: f.repo, Repo: f.repo, Home: f.home, Private: []string{f.priv}})
	fp := one(t, p)
	if fp.Path != "pkg/x.go" || fp.Diff != "@@ -1 +1 @@\n-package x\n+package y\n" {
		t.Fatalf("shell patch: %+v", fp)
	}
	if Build(Call{Tool: "Bash", Kind: api.ToolShell, Command: "echo '*** Begin Patch'", Cwd: f.repo, Home: f.home}) != nil {
		t.Fatal("a mention of a patch is not a patch")
	}
}

func TestManyFilesAndJunk(t *testing.T) {
	f := newFixture(t)
	var b strings.Builder
	b.WriteString("*** Begin Patch\n")
	for i := 0; i < 25; i++ {
		fmt.Fprintf(&b, "*** Add File: f%d.txt\n+x\n", i)
	}
	b.WriteString("*** End Patch\n")
	p := f.build("apply_patch", map[string]any{"command": b.String()})
	if len(p.Files) != MaxFiles || !p.Truncated || p.Added != 25 {
		t.Fatalf("files=%d truncated=%v added=%d", len(p.Files), p.Truncated, p.Added)
	}
	for _, c := range []Call{
		{Tool: "Edit", Input: json.RawMessage(`not json`)},
		{Tool: "Edit", Input: json.RawMessage(`{"file_path": 3}`)},
		{Tool: "Bash", Kind: api.ToolShell, Command: "ls"},
		{Tool: "Read", Input: json.RawMessage(`{"file_path":"x"}`)},
		{Tool: "MultiEdit", Input: json.RawMessage(`{"file_path":"x","edits":"nope"}`)},
	} {
		c.Home = f.home
		if p := Build(c); p != nil {
			t.Errorf("%+v: preview %+v", c, p)
		}
	}
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func BenchmarkBuildEdit(b *testing.B) {
	f := newFixture(&testing.T{})
	dir := b.TempDir()
	f.repo, f.home = dir, dir
	path := filepath.Join(dir, "big.go")
	var src strings.Builder
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&src, "func f%d() int { return %d }\n", i, i)
	}
	if err := os.WriteFile(path, []byte(src.String()), 0o600); err != nil {
		b.Fatal(err)
	}
	in, _ := json.Marshal(map[string]any{"file_path": path, "old_string": "return 1500 }", "new_string": "return 1501 }"})
	c := Call{Tool: "Edit", Kind: api.ToolEdit, Input: in, Cwd: dir, Repo: dir, Home: dir, Private: []string{}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		Build(c)
	}
}
