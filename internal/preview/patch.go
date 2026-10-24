package preview

import (
	"regexp"
	"strings"

	"github.com/xasdta/pilothouse/internal/api"
)

// Codex's apply_patch envelope (codex-rs/apply-patch, parser.rs):
//
//	*** Begin Patch
//	*** Add File: path        followed by +lines
//	*** Delete File: path
//	*** Update File: path     optionally *** Move to: path, then hunks:
//	@@ [context line]         ' ' context, '-' removed, '+' added lines,
//	                          optionally ending with *** End of File
//	*** End Patch

type patchLine struct {
	kind byte // ' ', '-', '+'
	text string
}

type patchHunk struct {
	anchor string // the text after "@@", a line to find before the hunk
	lines  []patchLine
	eof    bool
}

type patchFile struct {
	op    api.FileOp
	path  string
	move  string
	add   []string
	hunks []patchHunk
}

// parsePatch reads every file section of a patch. It is lenient like
// Codex's parser: a missing "@@" before the first hunk and blank lines
// inside a hunk (empty context) are accepted.
func parsePatch(s string) []patchFile {
	var files []patchFile
	var cur *patchFile
	var hunk *patchHunk
	started := false
	flushHunk := func() {
		if cur != nil && hunk != nil && len(hunk.lines) > 0 {
			cur.hunks = append(cur.hunks, *hunk)
		}
		hunk = nil
	}
	flushFile := func() {
		flushHunk()
		if cur != nil {
			files = append(files, *cur)
		}
		cur = nil
	}
	for _, line := range strings.Split(normalize(s), "\n") {
		trim := strings.TrimSpace(line)
		switch {
		case trim == "*** Begin Patch":
			started = true
			continue
		case !started:
			continue
		case trim == "*** End Patch":
			flushFile()
			return files
		case strings.HasPrefix(trim, "*** Add File: "):
			flushFile()
			cur = &patchFile{op: api.FileCreate, path: strings.TrimSpace(strings.TrimPrefix(trim, "*** Add File: "))}
			continue
		case strings.HasPrefix(trim, "*** Delete File: "):
			flushFile()
			cur = &patchFile{op: api.FileDelete, path: strings.TrimSpace(strings.TrimPrefix(trim, "*** Delete File: "))}
			continue
		case strings.HasPrefix(trim, "*** Update File: "):
			flushFile()
			cur = &patchFile{op: api.FileEdit, path: strings.TrimSpace(strings.TrimPrefix(trim, "*** Update File: "))}
			continue
		}
		if cur == nil {
			continue
		}
		switch cur.op {
		case api.FileCreate:
			if t, ok := strings.CutPrefix(line, "+"); ok {
				cur.add = append(cur.add, t)
			}
			continue
		case api.FileDelete:
			continue
		}
		switch {
		case strings.HasPrefix(trim, "*** Move to: "):
			cur.move = strings.TrimSpace(strings.TrimPrefix(trim, "*** Move to: "))
			cur.op = api.FileRename
		case trim == "*** End of File":
			if hunk != nil {
				hunk.eof = true
			}
		case strings.HasPrefix(line, "@@"):
			flushHunk()
			hunk = &patchHunk{anchor: strings.TrimSpace(strings.TrimPrefix(line, "@@"))}
		default:
			if hunk == nil {
				hunk = &patchHunk{}
			}
			kind, text := byte(' '), line
			if line != "" && (line[0] == ' ' || line[0] == '-' || line[0] == '+') {
				kind, text = line[0], line[1:]
			}
			hunk.lines = append(hunk.lines, patchLine{kind, text})
		}
	}
	flushFile() // no "*** End Patch": keep what was read
	return files
}

// applyHunks applies an update to content the way Codex does: each hunk's
// anchor line, then its old lines, are found from where the previous hunk
// ended, matching exactly, then ignoring trailing, then all surrounding
// whitespace. ok is false when a hunk doesn't apply.
func applyHunks(content string, hunks []patchHunk) (string, bool) {
	lines, eol := splitText(content)
	var out []string
	pos := 0
	for _, h := range hunks {
		if h.anchor != "" {
			i := seek(lines, pos, []string{h.anchor}, false)
			if i < 0 {
				return "", false
			}
			out = append(out, lines[pos:i+1]...)
			pos = i + 1
		}
		var old, nw []string
		for _, l := range h.lines {
			if l.kind != '+' {
				old = append(old, l.text)
			}
			if l.kind != '-' {
				nw = append(nw, l.text)
			}
		}
		at := len(lines)
		if len(old) > 0 {
			if at = seek(lines, pos, old, h.eof); at < 0 {
				return "", false
			}
		}
		out = append(out, lines[pos:at]...)
		out = append(out, nw...)
		pos = at + len(old)
	}
	out = append(out, lines[pos:]...)
	s := strings.Join(out, "\n")
	if len(out) > 0 && (eol || content == "") {
		s += "\n"
	}
	return s, true
}

// seek finds want in lines at or after pos (preferring the end of the file
// for an "End of File" hunk).
func seek(lines []string, pos int, want []string, eof bool) int {
	for _, eq := range []func(a, b string) bool{
		func(a, b string) bool { return a == b },
		func(a, b string) bool { return strings.TrimRight(a, " \t") == strings.TrimRight(b, " \t") },
		func(a, b string) bool { return strings.TrimSpace(a) == strings.TrimSpace(b) },
	} {
		match := func(i int) bool {
			for j, w := range want {
				if !eq(lines[i+j], w) {
					return false
				}
			}
			return true
		}
		if eof {
			if i := len(lines) - len(want); i >= pos && match(i) {
				return i
			}
		}
		for i := pos; i+len(want) <= len(lines); i++ {
			if match(i) {
				return i
			}
		}
	}
	return -1
}

// renderHunks shows hunks that can't be placed in the file as they are in
// the patch, numbered from 1.
func renderHunks(fp *api.FilePreview, hunks []patchHunk) {
	var out strings.Builder
	a, b := 1, 1
	for _, h := range hunks {
		var na, nb int
		var body strings.Builder
		for _, l := range h.lines {
			if l.kind != '+' {
				na++
			} else {
				fp.Added++
			}
			if l.kind != '-' {
				nb++
			} else {
				fp.Removed++
			}
			body.WriteByte(l.kind)
			body.WriteString(l.text)
			body.WriteByte('\n')
		}
		header := "@@ -" + span(a, na) + " +" + span(b, nb) + " @@"
		if h.anchor != "" {
			header += " " + h.anchor
		}
		chunk := header + "\n" + body.String()
		if out.Len()+len(chunk) > MaxDiffBytes {
			fp.Truncated = true
		} else {
			out.WriteString(chunk)
		}
		a, b = a+na, b+nb
	}
	fp.Diff = out.String()
}

func (b *builder) patch(text, dir string) {
	if text == "" {
		return
	}
	saved := b.cwd
	b.cwd = dir
	defer func() { b.cwd = saved }()
	for _, pf := range parsePatch(text) {
		if pf.path == "" {
			continue
		}
		p := b.abs(pf.path)
		fp := api.FilePreview{Path: b.show(p), Op: pf.op, Lang: langOf(p)}
		switch pf.op {
		case api.FileCreate:
			content := strings.Join(pf.add, "\n")
			if len(pf.add) > 0 {
				content += "\n"
			}
			diffFile(&fp, "", content)
		case api.FileDelete:
			if f := b.read(p); f.readable && !f.binary {
				diffFile(&fp, f.content, "")
			}
		default:
			if pf.move != "" {
				dst := b.abs(pf.move)
				fp.Path, fp.Lang = b.show(dst), langOf(dst)
			}
			f := b.read(p)
			applied := false
			if f.readable && !f.binary {
				if after, ok := applyHunks(normalize(f.content), pf.hunks); ok {
					diffFile(&fp, f.content, after)
					applied = true
				}
			}
			if !applied {
				renderHunks(&fp, pf.hunks)
			}
		}
		b.add(fp)
	}
}

// leadingCd matches the `cd dir &&` Codex may put before apply_patch.
var leadingCd = regexp.MustCompile(`\A\s*cd[ \t]+([^\s'"\\;&|<>()$` + "`" + `]+)[ \t]*&&`)

// shellPatch extracts the patch from a shell command that runs
// `apply_patch <<'EOF' … EOF`, and the directory it applies in.
func shellPatch(cmd, cwd string) (body, dir string, ok bool) {
	if !strings.Contains(cmd, "apply_patch") && !strings.Contains(cmd, "applypatch") {
		return "", "", false
	}
	start := strings.Index(cmd, "*** Begin Patch")
	if start < 0 {
		return "", "", false
	}
	end := strings.Index(cmd[start:], "*** End Patch")
	if end < 0 {
		end = len(cmd) - start
	} else {
		end += len("*** End Patch")
	}
	dir = cwd
	if m := leadingCd.FindStringSubmatch(cmd); m != nil {
		d := m[1]
		switch {
		case strings.HasPrefix(d, "/"):
			dir = d
		case cwd != "":
			dir = cwd + "/" + d
		}
	}
	return cmd[start : start+end], dir, true
}
