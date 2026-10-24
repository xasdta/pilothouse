// Package preview shows what a file tool call would change (docs/PLAN-0.2.md
// F4): a unified diff per file for Claude's Edit, MultiEdit, Write and
// NotebookEdit, Codex's apply_patch (the tool, or the `apply_patch <<EOF`
// shell form) and OpenCode's edit, write and multiedit.
//
// When the file can be read, the diff has real context and line numbers;
// otherwise it is built from the tool input alone and its line numbers
// count from the start of the change. Reads are bounded (MaxFileBytes per
// file, regular files only, never blocking on a FIFO) and never touch
// credential files, Pilothouse's own home or agent settings: a preview is
// stored with the request and shown on paired devices, so it must not
// carry more than the tool input already does from such files.
package preview

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/paths"
	"github.com/xasdta/pilothouse/internal/risk"
)

// Limits on what one preview reads and returns.
const (
	MaxFileBytes = 256 << 10 // read per file; larger files are diffed from the input alone
	MaxDiffBytes = 20 << 10  // diff text per file
	MaxFiles     = 20        // files per preview
	contextLines = 3
	binarySniff  = 8 << 10
	// MaxInputBytes bounds the tool input a preview decodes (hook payloads
	// can carry many megabytes); larger calls get no preview.
	MaxInputBytes = 4 << 20
	// maxDiffInput bounds the text one file's diff works on.
	maxDiffInput = 1 << 20
)

// Call is the tool call to preview.
type Call struct {
	Tool    string
	Kind    api.ToolKind
	Command string // shell tools: an apply_patch heredoc is previewed
	Input   json.RawMessage
	Cwd     string
	Repo    string // paths are shown relative to it
	// Home is the user's home ("" = paths.UserHome()). Private are
	// directories whose files are never read for context ("" entries are
	// ignored; nil = the Pilothouse home).
	Home    string
	Private []string
}

// Build returns the preview, or nil when the call changes no files or its
// input can't be understood. It never fails.
func Build(c Call) *api.Preview {
	if len(c.Input) > MaxInputBytes || len(c.Command) > MaxInputBytes {
		return nil
	}
	b := newBuilder(c)
	var in map[string]any
	if len(c.Input) > 0 {
		_ = json.Unmarshal(c.Input, &in)
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			if s, ok := in[k].(string); ok && s != "" {
				return s
			}
		}
		return ""
	}
	flag := func(keys ...string) bool {
		for _, k := range keys {
			if v, ok := in[k].(bool); ok {
				return v
			}
		}
		return false
	}
	switch c.Tool {
	case "Edit", "edit":
		b.edits(str("file_path", "filePath"), []change{{
			old: str("old_string", "oldString"), new: str("new_string", "newString"),
			all: flag("replace_all", "replaceAll"),
		}})
	case "MultiEdit", "multiedit":
		var cs []change
		list, _ := in["edits"].([]any)
		for _, e := range list {
			m, _ := e.(map[string]any)
			s := func(k1, k2 string) string {
				if v, ok := m[k1].(string); ok {
					return v
				}
				v, _ := m[k2].(string)
				return v
			}
			all, _ := m["replace_all"].(bool)
			if v, ok := m["replaceAll"].(bool); ok {
				all = v
			}
			cs = append(cs, change{old: s("old_string", "oldString"), new: s("new_string", "newString"), all: all})
		}
		if len(cs) > 0 {
			b.edits(str("file_path", "filePath"), cs)
		}
	case "Write", "write":
		if p := str("file_path", "filePath"); p != "" {
			content, _ := in["content"].(string)
			b.write(p, content)
		}
	case "NotebookEdit":
		b.notebook(str("notebook_path"), str("cell_id"), str("new_source"), str("cell_type"), str("edit_mode"))
	case "apply_patch", "patch":
		b.patch(str("command", "input", "patch", "patchText"), b.cwd)
	default:
		if c.Command != "" && strings.Contains(c.Command, "*** Begin Patch") {
			if body, dir, ok := shellPatch(c.Command, b.cwd); ok {
				b.patch(body, dir)
			}
		}
	}
	if len(b.files) == 0 {
		return nil
	}
	p := &api.Preview{Files: b.files, Truncated: b.dropped}
	if len(p.Files) > MaxFiles {
		p.Files, p.Truncated = p.Files[:MaxFiles], true
	}
	for _, f := range b.files { // totals count every file, shown or not
		p.Added += f.Added
		p.Removed += f.Removed
	}
	return p
}

type builder struct {
	c       Call
	home    string
	cwd     string
	repo    string
	private []string
	files   []api.FilePreview
	dropped bool
}

func newBuilder(c Call) *builder {
	b := &builder{c: c, home: c.Home}
	if b.home == "" {
		b.home = paths.UserHome()
	}
	b.home = filepath.Clean(b.home)
	if c.Cwd != "" {
		b.cwd = filepath.Clean(expandHome(c.Cwd, b.home))
	}
	if c.Repo != "" {
		b.repo = filepath.Clean(expandHome(c.Repo, b.home))
	}
	priv := c.Private
	if priv == nil {
		priv = []string{paths.Home()}
	}
	for _, p := range priv {
		if p != "" {
			b.private = append(b.private, filepath.Clean(p))
		}
	}
	// Agent settings can hold API keys (Claude's "env" block, Codex auth).
	b.private = append(b.private,
		filepath.Join(b.home, ".claude", "settings.json"), filepath.Join(b.home, ".claude", "settings.local.json"),
		filepath.Join(b.home, ".claude.json"), filepath.Join(b.home, ".codex"))
	return b
}

func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return home + p[1:]
	}
	return p
}

func (b *builder) abs(p string) string {
	p = expandHome(p, b.home)
	if !filepath.IsAbs(p) && b.cwd != "" {
		p = filepath.Join(b.cwd, p)
	}
	return filepath.Clean(p)
}

// show names a file for a phone screen: relative to the repo (else the
// cwd) when inside it, ~/… in the home directory.
func (b *builder) show(p string) string {
	for _, root := range []string{b.repo, b.cwd} {
		if root != "" && strings.HasPrefix(p, root+"/") {
			return p[len(root)+1:]
		}
	}
	if strings.HasPrefix(p, b.home+"/") {
		return "~" + p[len(b.home):]
	}
	return p
}

func (b *builder) add(fp api.FilePreview) {
	if len(b.files) >= MaxFiles {
		b.dropped = true
		fp.Diff = "" // counted in the totals, not shown
	}
	b.files = append(b.files, fp)
}

// file is what reading a path found.
type file struct {
	exists   bool
	readable bool // content is the whole file
	binary   bool
	content  string
}

func (b *builder) isPrivate(p string) bool {
	if risk.IsSecretPath(p, b.home) {
		return true
	}
	for _, r := range b.private {
		if p == r || strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
}

// read loads p for context when that is allowed and cheap.
func (b *builder) read(p string) file {
	fi, err := os.Stat(p)
	if err != nil {
		return file{}
	}
	f := file{exists: true}
	if b.isPrivate(p) || !fi.Mode().IsRegular() || fi.Size() > MaxFileBytes {
		return f
	}
	if real, err := filepath.EvalSymlinks(p); err != nil || b.isPrivate(real) {
		return f
	}
	h, err := openNoBlock(p)
	if err != nil {
		return f
	}
	defer h.Close()
	if st, err := h.Stat(); err != nil || !st.Mode().IsRegular() {
		return f
	}
	data, err := io.ReadAll(io.LimitReader(h, MaxFileBytes+1))
	if err != nil || len(data) > MaxFileBytes {
		return f
	}
	f.readable, f.binary, f.content = true, isBinary(data), string(data)
	return f
}

func isBinary(data []byte) bool {
	return bytes.IndexByte(data[:min(len(data), binarySniff)], 0) >= 0
}

// normalize drops CR from CRLF line ends so a file's line endings don't
// turn every line into a change.
func normalize(s string) string {
	if strings.Contains(s, "\r\n") {
		return strings.ReplaceAll(s, "\r\n", "\n")
	}
	return s
}

// diffFile fills fp's diff from before to after.
func diffFile(fp *api.FilePreview, before, after string) {
	if isBinary([]byte(before)) || isBinary([]byte(after)) {
		fp.Lang = ""
		return // binary: no text diff
	}
	if len(before)+len(after) > maxDiffInput {
		// Too big to diff on the hook path: a new file shows its start,
		// anything else just its size in lines.
		fp.Truncated = true
		fp.Added, fp.Removed = lineCount(after), lineCount(before)
		if before == "" {
			start := after[:MaxDiffBytes]
			if i := strings.LastIndexByte(start, '\n'); i > 0 {
				start = start[:i+1]
			}
			fp.Diff = unifiedDiff("", normalize(start), contextLines, MaxDiffBytes).text
		}
		return
	}
	u := unifiedDiff(normalize(before), normalize(after), contextLines, MaxDiffBytes)
	fp.Diff, fp.Added, fp.Removed, fp.Truncated = u.text, u.added, u.removed, u.truncated
}

func lineCount(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// ---- Edit / MultiEdit / write ----------------------------------------------

type change struct {
	old, new string
	all      bool
}

// applyChanges applies edits in order the way the tools do. ok is false
// when one doesn't apply (stale input, or an old string not in the file).
func applyChanges(content string, cs []change) (string, bool) {
	for _, c := range cs {
		old, nw := normalize(c.old), normalize(c.new)
		switch {
		case old == "" && content == "":
			content = nw
		case old == "" || !strings.Contains(content, old):
			return "", false
		case c.all:
			content = strings.ReplaceAll(content, old, nw)
		default:
			content = strings.Replace(content, old, nw, 1)
		}
	}
	return content, true
}

func (b *builder) edits(path string, cs []change) {
	if path == "" {
		return
	}
	p := b.abs(path)
	fp := api.FilePreview{Path: b.show(p), Op: api.FileEdit, Lang: langOf(p)}
	f := b.read(p)
	if !f.exists && len(cs) > 0 && cs[0].old == "" {
		fp.Op = api.FileCreate
	}
	if f.readable && !f.binary {
		if after, ok := applyChanges(normalize(f.content), cs); ok {
			diffFile(&fp, f.content, after)
			b.add(fp)
			return
		}
	} else if !f.exists {
		if after, ok := applyChanges("", cs); ok {
			diffFile(&fp, "", after)
			b.add(fp)
			return
		}
	}
	// From the input alone: one diff per change, numbered from its start.
	var text strings.Builder
	for _, c := range cs {
		if len(c.old)+len(c.new) > maxDiffInput {
			fp.Added, fp.Removed, fp.Truncated = fp.Added+lineCount(c.new), fp.Removed+lineCount(c.old), true
			continue
		}
		var u unified
		if !isBinary([]byte(c.old)) && !isBinary([]byte(c.new)) {
			u = unifiedDiff(normalize(withEOL(c.old)), normalize(withEOL(c.new)), contextLines, MaxDiffBytes)
		}
		fp.Added += u.added
		fp.Removed += u.removed
		if text.Len()+len(u.text) > MaxDiffBytes {
			fp.Truncated = true
			continue
		}
		fp.Truncated = fp.Truncated || u.truncated
		text.WriteString(u.text)
	}
	fp.Diff = text.String()
	b.add(fp)
}

// withEOL ends a snippet with a newline, so a snippet isn't reported as
// lacking one at the end of the file.
func withEOL(s string) string {
	if s == "" || strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

func (b *builder) write(path, content string) {
	p := b.abs(path)
	fp := api.FilePreview{Path: b.show(p), Op: api.FileCreate, Lang: langOf(p)}
	f := b.read(p)
	before := ""
	if f.exists {
		fp.Op = api.FileOverwrite
		if f.readable {
			if f.binary {
				b.add(fp)
				return
			}
			before = f.content
		}
	}
	diffFile(&fp, before, content)
	b.add(fp)
}

// ---- NotebookEdit -------------------------------------------------------------

func (b *builder) notebook(path, cellID, source, cellType, mode string) {
	if path == "" {
		return
	}
	p := b.abs(path)
	fp := api.FilePreview{Path: b.show(p), Op: api.FileEdit, Lang: "python"}
	if cellType == "markdown" {
		fp.Lang = "md"
	}
	old := ""
	if f := b.read(p); f.readable && !f.binary {
		var nb struct {
			Cells []struct {
				ID       string          `json:"id"`
				CellType string          `json:"cell_type"`
				Source   json.RawMessage `json:"source"`
			} `json:"cells"`
			Metadata struct {
				Kernelspec struct {
					Language string `json:"language"`
				} `json:"kernelspec"`
			} `json:"metadata"`
		}
		if json.Unmarshal([]byte(f.content), &nb) == nil {
			if l := nb.Metadata.Kernelspec.Language; l != "" && cellType != "markdown" {
				fp.Lang = langOf("x." + map[string]string{"python": "py", "r": "r", "julia": "jl"}[strings.ToLower(l)])
			}
			for i, c := range nb.Cells {
				if c.ID == cellID || (cellID == "" && i == 0) || cellID == "cell-"+strconv.Itoa(i) {
					old = cellSource(c.Source)
					if cellType == "" && c.CellType == "markdown" {
						fp.Lang = "md"
					}
					break
				}
			}
		}
	}
	switch mode {
	case "insert":
		diffFile(&fp, "", withEOL(source))
	case "delete":
		diffFile(&fp, withEOL(old), "")
	default:
		diffFile(&fp, withEOL(old), withEOL(source))
	}
	b.add(fp)
}

// cellSource reads a notebook cell's source: a string or a list of lines.
func cellSource(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var lines []string
	if json.Unmarshal(raw, &lines) == nil {
		return strings.Join(lines, "")
	}
	return ""
}

// ---- languages ------------------------------------------------------------

var langByExt = map[string]string{
	".go": "go", ".ts": "ts", ".tsx": "tsx", ".js": "js", ".jsx": "jsx", ".mjs": "js", ".cjs": "js",
	".py": "py", ".pyi": "py", ".rb": "rb", ".rs": "rs", ".java": "java", ".kt": "kotlin", ".kts": "kotlin",
	".swift": "swift", ".c": "c", ".h": "c", ".cc": "cpp", ".cpp": "cpp", ".cxx": "cpp", ".hpp": "cpp", ".hh": "cpp",
	".cs": "csharp", ".php": "php", ".sh": "sh", ".bash": "sh", ".zsh": "sh", ".fish": "fish", ".ps1": "powershell",
	".sql": "sql", ".html": "html", ".htm": "html", ".css": "css", ".scss": "scss", ".sass": "scss", ".less": "less",
	".json": "json", ".jsonc": "json", ".yaml": "yaml", ".yml": "yaml", ".toml": "toml", ".ini": "ini", ".xml": "xml",
	".md": "md", ".mdx": "md", ".vue": "vue", ".svelte": "svelte", ".lua": "lua", ".dart": "dart", ".scala": "scala",
	".ex": "elixir", ".exs": "elixir", ".erl": "erlang", ".hs": "haskell", ".ml": "ocaml", ".r": "r", ".jl": "julia",
	".tf": "hcl", ".hcl": "hcl", ".proto": "proto", ".graphql": "graphql", ".gql": "graphql", ".zig": "zig",
	".nix": "nix", ".clj": "clojure", ".el": "lisp", ".vim": "vim", ".ipynb": "json", ".csv": "csv", ".txt": "text",
}

// langOf is the highlighting hint for a path.
func langOf(p string) string {
	base := filepath.Base(p)
	switch strings.ToLower(base) {
	case "dockerfile", "containerfile":
		return "dockerfile"
	case "makefile", "gnumakefile":
		return "make"
	case "go.mod", "go.sum":
		return "go"
	}
	if strings.HasPrefix(base, "Dockerfile.") {
		return "dockerfile"
	}
	return langByExt[strings.ToLower(filepath.Ext(base))]
}
