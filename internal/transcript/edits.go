package transcript

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/xasdta/pilothouse/internal/api"
)

// maxDiffCells bounds the LCS table behind a line count; bigger edits
// count every line as changed.
const maxDiffCells = 1 << 20

// countLines is the number of lines in s: "a\nb" and "a\nb\n" are both 2.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// lineDelta counts the lines added and removed when old becomes new, as
// a unified diff would.
func lineDelta(old, new string) (added, removed int) {
	a, b := splitLines(old), splitLines(new)
	for len(a) > 0 && len(b) > 0 && a[0] == b[0] {
		a, b = a[1:], b[1:]
	}
	for len(a) > 0 && len(b) > 0 && a[len(a)-1] == b[len(b)-1] {
		a, b = a[:len(a)-1], b[:len(b)-1]
	}
	if len(a) == 0 || len(b) == 0 || len(a)*len(b) > maxDiffCells {
		return len(b), len(a)
	}
	common := lcs(a, b)
	return len(b) - common, len(a) - common
}

// lcs is the length of the longest common subsequence (two rows).
func lcs(a, b []string) int {
	prev := make([]int32, len(b)+1)
	cur := make([]int32, len(b)+1)
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				cur[j] = prev[j+1] + 1
			} else {
				cur[j] = max(prev[j], cur[j+1])
			}
		}
		prev, cur = cur, prev
	}
	return int(prev[0])
}

// claudeEdits estimates what a Claude file tool call changes from its
// input; the tool's result (structuredPatch) replaces the estimate.
func claudeEdits(tool string, input json.RawMessage) []fileEdit {
	var in struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		OldString    string `json:"old_string"`
		NewString    string `json:"new_string"`
		Content      string `json:"content"`
		NewSource    string `json:"new_source"`
		EditMode     string `json:"edit_mode"`
		Edits        []struct {
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		} `json:"edits"`
	}
	if json.Unmarshal(input, &in) != nil {
		return nil
	}
	switch tool {
	case "Edit":
		if in.FilePath == "" {
			return nil
		}
		add, rem := lineDelta(in.OldString, in.NewString)
		op := api.FileEdit
		if in.OldString == "" {
			op = api.FileCreate // Edit with an empty old_string creates the file
		}
		return []fileEdit{{in.FilePath, op, add, rem}}
	case "MultiEdit":
		if in.FilePath == "" {
			return nil
		}
		e := fileEdit{path: in.FilePath, op: api.FileEdit}
		for _, x := range in.Edits {
			add, rem := lineDelta(x.OldString, x.NewString)
			e.added += add
			e.removed += rem
		}
		return []fileEdit{e}
	case "Write":
		if in.FilePath == "" {
			return nil
		}
		// Create or overwrite: only the result says which.
		return []fileEdit{{in.FilePath, api.FileCreate, countLines(in.Content), 0}}
	case "NotebookEdit":
		if in.NotebookPath == "" {
			return nil
		}
		e := fileEdit{path: in.NotebookPath, op: api.FileEdit, added: countLines(in.NewSource)}
		if in.EditMode == "delete" {
			e.added = 0
		}
		return []fileEdit{e}
	}
	return nil
}

// claudeResultEdits reads the exact change from an Edit/Write result
// (toolUseResult: {type: create|update, filePath, structuredPatch}).
func claudeResultEdits(raw json.RawMessage) ([]fileEdit, bool) {
	if len(raw) == 0 || raw[0] != '{' {
		return nil, false
	}
	var r struct {
		Type            string `json:"type"`
		FilePath        string `json:"filePath"`
		Content         string `json:"content"`
		StructuredPatch []struct {
			Lines []string `json:"lines"`
		} `json:"structuredPatch"`
	}
	if json.Unmarshal(raw, &r) != nil || r.FilePath == "" {
		return nil, false
	}
	if r.StructuredPatch == nil && r.Type != "create" {
		return nil, false
	}
	e := fileEdit{path: r.FilePath, op: api.FileEdit}
	switch r.Type {
	case "create":
		e.op = api.FileCreate
		if len(r.StructuredPatch) == 0 {
			e.added = countLines(r.Content)
		}
	case "update":
		e.op = api.FileOverwrite
	}
	for _, h := range r.StructuredPatch {
		for _, l := range h.Lines {
			switch {
			case strings.HasPrefix(l, "+"):
				e.added++
			case strings.HasPrefix(l, "-"):
				e.removed++
			}
		}
	}
	if r.Type == "update" && e.added+e.removed > 0 {
		// Write over an existing file reports a patch: it reads as an edit.
		e.op = api.FileEdit
	}
	return []fileEdit{e}, true
}

// patchEdits counts what an apply_patch envelope changes (Codex's
// grammar, codex-rs/apply-patch/src/parser.rs). Paths are relative to cwd.
func patchEdits(patch, cwd string) []fileEdit {
	var out []fileEdit
	cur := -1 // index in out of the file being patched
	abs := func(p string) string {
		p = strings.TrimSpace(p)
		if p != "" && !filepath.IsAbs(p) && cwd != "" {
			p = filepath.Join(cwd, p)
		}
		return p
	}
	start := func(p string, op api.FileOp) {
		cur = -1
		if p = abs(p); p != "" {
			out = append(out, fileEdit{path: p, op: op})
			cur = len(out) - 1
		}
	}
	for _, line := range strings.Split(patch, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "*** Add File: "):
			start(strings.TrimPrefix(line, "*** Add File: "), api.FileCreate)
		case strings.HasPrefix(line, "*** Update File: "):
			start(strings.TrimPrefix(line, "*** Update File: "), api.FileEdit)
		case strings.HasPrefix(line, "*** Delete File: "):
			start(strings.TrimPrefix(line, "*** Delete File: "), api.FileDelete)
			cur = -1 // a delete has no body
		case strings.HasPrefix(line, "*** Move to: "):
			if p := abs(strings.TrimPrefix(line, "*** Move to: ")); cur >= 0 && p != "" {
				out[cur].path, out[cur].op = p, api.FileRename
			}
		case strings.HasPrefix(line, "***"):
			// *** Begin Patch, *** End Patch, *** End of File
		case cur < 0:
		case strings.HasPrefix(line, "+"):
			out[cur].added++
		case strings.HasPrefix(line, "-"):
			out[cur].removed++
		}
	}
	return out
}

// patchBody is the apply_patch envelope inside text (a heredoc in a
// shell command), or "".
func patchBody(s string) string {
	i := strings.Index(s, "*** Begin Patch")
	if i < 0 {
		return ""
	}
	s = s[i:]
	if j := strings.Index(s, "*** End Patch"); j >= 0 {
		s = s[:j+len("*** End Patch")]
	}
	return s
}
