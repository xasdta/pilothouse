package policy

import (
	"path/filepath"
	"strings"

	"github.com/xasdta/pilothouse/internal/api"
)

// MatchSpec reports whether a managed rule made from spec would match
// in, with the same shell-aware semantics the managed file gets. It is
// for rule suggestions: checking a suggestion against the call it came
// from, and counting past approvals it would have covered. home expands ~.
func MatchSpec(spec api.RuleSpec, in Input, home string) (bool, error) {
	rules, err := compileSpecs("suggestion", []api.RuleSpec{spec}, home)
	if err != nil || len(rules) != 1 {
		return false, err
	}
	c := evalCtx{in: &in, home: home, now: in.Now}
	if in.Cwd != "" {
		c.cwd = filepath.Clean(expandHome(in.Cwd, home))
	}
	if in.Repo != "" {
		c.repo = filepath.Clean(expandHome(in.Repo, home))
	}
	ok, _, err := rules[0].eval(&c)
	return ok, err
}

// GuessToolKind classifies the common vendor tool names like the
// adapters do, for hypothetical calls (`policy test`, the rule tester).
func GuessToolKind(tool string) api.ToolKind {
	switch tool {
	case "Bash", "PowerShell", "shell", "exec_command", "local_shell", "unified_exec", "bash":
		return api.ToolShell
	case "Write", "Edit", "MultiEdit", "NotebookEdit", "apply_patch", "edit", "write", "multiedit", "patch":
		return api.ToolEdit
	case "Read", "Glob", "Grep", "LS", "NotebookRead", "view_image", "read", "glob", "grep", "list":
		return api.ToolRead
	case "WebFetch", "WebSearch", "web_search", "webfetch":
		return api.ToolWeb
	}
	if strings.HasPrefix(tool, "mcp__") {
		return api.ToolMCP
	}
	return api.ToolOther
}

// EscapeGlob quotes glob syntax in a literal path or command, so it can
// be used as a pattern that matches only itself.
func EscapeGlob(s string) string {
	if !strings.ContainsAny(s, `*?{}\,`) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`*?{}\,`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
