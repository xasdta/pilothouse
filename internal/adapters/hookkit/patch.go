package hookkit

import (
	"path/filepath"
	"regexp"
	"strings"
)

// patchPrefixes are the apply_patch envelope lines that name a file
// (Codex's grammar, codex-rs/apply-patch/src/parser.rs).
var patchPrefixes = []string{"*** Add File: ", "*** Update File: ", "*** Delete File: ", "*** Move to: "}

// PatchPaths lists the files an apply_patch envelope touches, absolute
// (patch paths are relative to dir), in patch order, without duplicates.
func PatchPaths(patch, dir string) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(patch, "\n") {
		line = strings.TrimRight(line, "\r")
		for _, prefix := range patchPrefixes {
			p, ok := strings.CutPrefix(line, prefix)
			if !ok {
				continue
			}
			if p = strings.TrimSpace(p); p == "" {
				continue
			}
			if !filepath.IsAbs(p) && dir != "" {
				p = filepath.Join(dir, p)
			}
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// exactPatchScript matches the only shell forms Codex intercepts and runs
// as its apply_patch tool instead of a shell (extract_apply_patch_from_bash
// in codex-rs/apply-patch/src/invocation.rs): the whole script is one
// statement, optionally `cd <word> &&` first, then `apply_patch` or
// `applypatch` with a heredoc.
var exactPatchScript = regexp.MustCompile(`\A\s*(?:cd[ \t]+([^\s'"\\;&|<>()$` + "`" + `]+)[ \t]*&&[ \t]*)?(?:apply_patch|applypatch)[ \t]*<<-?[ \t]*(['"]?)([A-Za-z_][A-Za-z0-9_]*)(['"]?)[ \t]*\n`)

// patchCommand finds `apply_patch` / `applypatch` used as a command word.
var patchCommand = regexp.MustCompile(`(^|[\s;&|()]|&&)(?:apply_patch|applypatch)([\s<]|$)`)

// ShellPatch inspects a shell command for an apply_patch heredoc, the way
// Codex models edit files when the apply_patch tool is not offered
// (`apply_patch <<'EOF'` … `*** Update File: x` … `EOF`). It returns the
// files the patch names, absolute (relative to cwd, or to the `cd`
// directory in the `cd dir && apply_patch` form).
//
// exact reports that the whole command is nothing but the patch — the
// form Codex intercepts and applies itself, so the call is an edit, not a
// shell command. For any other command that mentions apply_patch, the
// paths are a best-effort extra: the command may do more than patch.
func ShellPatch(command, cwd string) (paths []string, exact bool) {
	if !strings.Contains(command, "*** Begin Patch") || !patchCommand.MatchString(command) {
		return nil, false
	}
	if m := exactPatchScript.FindStringSubmatchIndex(command); m != nil {
		openQ, delim, closeQ := sub(command, m, 2), sub(command, m, 3), sub(command, m, 4)
		if openQ == closeQ {
			body := command[m[1]:]
			if end, ok := heredocEnd(body, delim); ok {
				dir := cwd
				if cdDir := sub(command, m, 1); cdDir != "" {
					dir = joinDir(cwd, cdDir)
				}
				return PatchPaths(body[:end], dir), true
			}
		}
	}
	// Anything else: every patch line in the text, relative to cwd (or a
	// leading `cd dir &&`).
	dir := cwd
	if m := leadingCd.FindStringSubmatch(command); m != nil {
		dir = joinDir(cwd, m[1])
	}
	return PatchPaths(command, dir), false
}

var leadingCd = regexp.MustCompile(`\A\s*cd[ \t]+([^\s'"\\;&|<>()$` + "`" + `]+)[ \t]*&&`)

func sub(s string, m []int, i int) string {
	if m[2*i] < 0 {
		return ""
	}
	return s[m[2*i]:m[2*i+1]]
}

// heredocEnd returns the offset in body of the line that closes the
// heredoc, which must be the last non-blank line of the command.
func heredocEnd(body, delim string) (int, bool) {
	off := 0
	for _, line := range strings.SplitAfter(body, "\n") {
		if strings.TrimSpace(line) == delim {
			rest := body[off+len(line):]
			return off, strings.TrimSpace(rest) == ""
		}
		off += len(line)
	}
	return 0, false
}

func joinDir(cwd, dir string) string {
	// "~/x" stays as is: policy expands ~ when it resolves paths.
	if filepath.IsAbs(dir) || cwd == "" || dir == "~" || strings.HasPrefix(dir, "~/") {
		return filepath.Clean(dir)
	}
	return filepath.Join(cwd, dir)
}
