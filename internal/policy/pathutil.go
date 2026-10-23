package policy

import (
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// expandHome replaces a leading "~", "$HOME" or "${HOME}" with home.
// "~user" forms are left alone: resolving other users' homes from a
// policy file would be surprising.
func expandHome(p, home string) string {
	if home == "" {
		return p
	}
	for _, pre := range []string{"~", "${HOME}", "$HOME"} {
		if p == pre {
			return home
		}
		if strings.HasPrefix(p, pre+"/") {
			return home + p[len(pre):]
		}
	}
	return p
}

// absPath makes a tool path absolute the way the agent would resolve it:
// "~" expanded, relative paths joined to cwd, then cleaned.
func absPath(p, cwd, home string) string {
	if p == "" {
		return ""
	}
	p = expandHome(p, home)
	if !filepath.IsAbs(p) && cwd != "" {
		p = filepath.Join(expandHome(cwd, home), p)
	}
	return filepath.Clean(p)
}

// pathPattern turns a user-written path glob into one matched against
// absolute paths: "~" is expanded, and a relative pattern matches at any
// depth ("*.pem" → "**/*.pem", ".env*" → "**/.env*").
func pathPattern(p, home string) string {
	p = expandHome(p, home)
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "**") {
		return p
	}
	return "**/" + strings.TrimPrefix(p, "./")
}

// caseInsensitiveFS is true where the default filesystem ignores case
// (APFS/HFS+ on macOS), so "~/.PILOTHOUSE" and "~/.pilothouse" are the same
// directory for protected-path checks.
var caseInsensitiveFS = runtime.GOOS == "darwin"

func foldPath(p string) string {
	if caseInsensitiveFS {
		return strings.ToLower(p)
	}
	return p
}

// underAny reports whether p (absolute, cleaned, folded) equals or is
// inside one of roots (same form).
func underAny(p string, roots []string) bool {
	for _, r := range roots {
		if p == r || (strings.HasPrefix(p, r) && (strings.HasSuffix(r, "/") || p[len(r)] == '/')) {
			return true
		}
	}
	return false
}

// protectedSet is what the self-protection rule guards. Literal entries
// are roots: the path itself and everything under it. Entries with glob
// syntax (adapters use "**/.claude/settings.json" for project settings in
// any repo) are path-style globs; they too cover the matched path and
// everything under it. Immutable once built, so it is shared freely.
type protectedSet struct {
	roots []string // absolute, cleaned, folded
	globs []*glob  // compiled from folded, absolute-or-"**/" patterns
	// except are carve-outs from broader roots (same form as roots): a
	// path under one is protected only by a root that is itself inside
	// the carve-out (a worktree's own .claude/settings.json) or by a glob.
	except []string
}

// has reports whether p (absolute, cleaned, folded) is protected.
func (s *protectedSet) has(p string) bool {
	if s == nil {
		return false
	}
	if s.root(p) != "" {
		return true
	}
	for _, g := range s.globs {
		if g.match(p) {
			return true
		}
		// "Under a match": try each ancestor. Globs keep their literal
		// suffix, so most of these are rejected by one HasSuffix.
		for i := len(p) - 1; i > 0; i-- {
			if p[i] == '/' && g.match(p[:i]) {
				return true
			}
		}
	}
	return false
}

// root returns the literal root that protects p, the most specific one
// when several do, or "" when none does (globs are not consulted).
func (s *protectedSet) root(p string) string {
	if s == nil {
		return ""
	}
	exempt := underAny(p, s.except)
	best := ""
	for _, r := range s.roots {
		if !underAny(p, []string{r}) {
			continue
		}
		if exempt && !underAny(r, s.except) {
			continue // a broad root (~/.pilothouse) that the carve-out opens
		}
		if len(r) > len(best) {
			best = r
		}
	}
	return best
}

// withRoots returns a copy of s with extra literal roots (same form).
func (s *protectedSet) withRoots(extra ...string) *protectedSet {
	return &protectedSet{roots: append(slices.Clip(s.roots), extra...), globs: s.globs, except: s.except}
}
