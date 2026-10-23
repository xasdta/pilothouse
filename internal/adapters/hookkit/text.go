package hookkit

import (
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ownedMarker ends a hook command whose binary isn't named pilothouse*
// (installed as `ph`, say), so OwnedBy still recognises it. Both agents run
// hook commands through a shell, which ignores the comment.
const ownedMarker = " #pilothouse"

// ShimCommand is the hook command line for agent: the shim path, quoted
// for sh when needed, then `hook <agent>`.
func ShimCommand(shimPath, agent string) string {
	cmd := ShellQuote(shimPath) + " hook " + agent
	if !strings.HasPrefix(filepath.Base(shimPath), "pilothouse") {
		cmd += ownedMarker
	}
	return cmd
}

// OwnedBy returns a matcher for hook commands Pilothouse installed for agent:
// any command whose program is named pilothouse* followed by `hook <agent>`.
// Matching the name rather than the exact path lets a reinstall from a new
// location replace a stale entry instead of adding a second one.
func OwnedBy(agent string) Owned {
	named := regexp.MustCompile(`(^|[/\s'"])pilothouse[^/\s'"]*['"]?\s+hook\s+` + regexp.QuoteMeta(agent) + `(\s|$)`)
	marked := regexp.MustCompile(`\shook\s+` + regexp.QuoteMeta(agent) + `\s+#pilothouse\s*$`)
	return func(cmd string) bool { return named.MatchString(cmd) || marked.MatchString(cmd) }
}

// ShellQuote quotes s for POSIX sh if it contains anything but safe
// characters.
func ShellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, unsafeShellRune) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func unsafeShellRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	}
	return !strings.ContainsRune("@%+=:,./-_", r)
}

// Truncate shortens s to at most n runes, marking the cut with "…".
func Truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// OneLine collapses s to its first non-empty line, marking dropped lines
// with "…", then truncates it to n runes. Summaries are single lines.
func OneLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i]) + " …"
	}
	return Truncate(s, n)
}

// CapBytes truncates s to at most n bytes on a rune boundary.
func CapBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// RelPath shows p relative to cwd when p is inside it: "src/app.go" reads
// better on a phone than a full home path.
func RelPath(cwd, p string) string {
	if cwd == "" || !filepath.IsAbs(p) {
		return p
	}
	rel, err := filepath.Rel(cwd, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return p
	}
	return rel
}
