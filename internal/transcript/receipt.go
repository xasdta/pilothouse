package transcript

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

// A receipt is what a session's latest turn did, read off its transcript:
// the checks it ran (tests, lint, build) with their last result and
// whether they ran after the last edit, the files it changed, the commit
// it made, and the calls that were refused or failed. Nothing is
// summarised: every field is a count, a command or a parsed number.

// step is one tool call as the receipt needs it.
type step struct {
	at             time.Time
	kind           api.ToolKind
	tool           string
	command        string
	result         string
	isError        bool
	pending        bool
	denied         bool
	edits          []fileEdit
	added, removed int // when the source has no per-edit counts
	paths          []string
}

var (
	// lintCommand recognises linters and type checks.
	lintCommand = regexp.MustCompile(`(?:^|[\s;&|(])((?:npm|pnpm|yarn|bun)(?:\s+run)?\s+(?:lint|typecheck|check-types|type-check)(?::[\w-]+)?|eslint|golangci-lint|staticcheck|go\s+vet|gofmt\s+-l|ruff(?:\s+check)?|flake8|pylint|mypy|biome\s+(?:lint|check)|tsc\s+(?:[\w.=-]+\s+)*--noEmit|cargo\s+clippy|prettier\s+--check|rubocop|swiftlint|shellcheck)(?:\s|$|;|&|\|)`)
	// buildCommand recognises builds.
	buildCommand = regexp.MustCompile(`(?:^|[\s;&|(])((?:npm|pnpm|yarn|bun)(?:\s+run)?\s+build(?::[\w-]+)?|go\s+build|cargo\s+build|vite\s+build|next\s+build|astro\s+build|make\s+(?:build|all|release)|gradlew?\s+build|mvn\s+(?:[\w:.=-]+\s+)*package|dotnet\s+build|swift\s+build|xcodebuild|docker\s+build)(?:\s|$|;|&|\|)`)
	// searchCommand / readCommand classify plain shell reads.
	searchCommand = regexp.MustCompile(`^\s*(?:cd\s+\S+\s*(?:&&|;)\s*)?(?:grep|rg|ag|ack|find|fd|git\s+grep)\b`)
	readCommand   = regexp.MustCompile(`^\s*(?:cd\s+\S+\s*(?:&&|;)\s*)?(?:cat|head|tail|sed\s+-n|less|bat|wc|ls|tree|nl)\b`)
	commitCommand = regexp.MustCompile(`(?:^|[\s;&|(])git\s+(?:-C\s+\S+\s+)?commit(?:\s|$)`)
	commitMessage = regexp.MustCompile(`(?:^|\s)-[a-zA-Z]*m\s*(?:"((?:[^"\\]|\\.)*)"|'([^']*)'|(\S+))`)
	// passCount finds "48 passed", "48 passing", "Tests: 48 passed", "test result: ok. 48 passed".
	passCount = regexp.MustCompile(`(?i)\b(\d+)\s+(?:passed|passing|tests? passed)\b`)
)

// lintRunner and buildRunner name the check as people say it, or "".
func lintRunner(command string) string  { return runnerName(lintCommand, command) }
func buildRunner(command string) string { return runnerName(buildCommand, command) }

// commandHead is what a shell command runs, for recognising checks: the
// text before any heredoc, with quoted strings blanked, so a script that
// merely mentions "go test" (python3 - <<'EOF' …) isn't a test run.
func commandHead(command string) string {
	if i := strings.Index(command, "<<"); i >= 0 {
		command = command[:i]
	}
	var b strings.Builder
	var quote rune
	for _, r := range command {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
			b.WriteByte(' ')
		case r == '\'' || r == '"':
			quote = r
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func runnerName(re *regexp.Regexp, command string) string {
	m := re.FindStringSubmatch(command)
	if m == nil {
		return ""
	}
	return strings.Join(strings.Fields(m[1]), " ")
}

// commitSubject returns the first line of a git commit's message, "" when
// the command commits without -m, and ok=false when it isn't a commit.
func commitSubject(command string) (subject string, ok bool) {
	loc := commitCommand.FindStringIndex(command)
	if loc == nil {
		return "", false
	}
	rest := command[loc[1]:]
	if i := strings.Index(rest, "<<"); i >= 0 { // -m "$(cat <<'EOF' … EOF)"
		lines := strings.Split(rest[i:], "\n")
		for _, l := range lines[1:] {
			if l = strings.TrimSpace(l); l != "" {
				return l, true
			}
		}
		return "", true
	}
	m := commitMessage.FindStringSubmatch(rest)
	if m == nil {
		return "", true
	}
	msg := m[1] + m[2] + m[3]
	msg = strings.ReplaceAll(msg, `\"`, `"`)
	if i := strings.IndexAny(msg, "\n"); i >= 0 {
		msg = msg[:i]
	}
	return strings.TrimSpace(msg), true
}

// testsPassed reads how many tests passed from a runner's output, or 0.
func testsPassed(result string) int {
	ms := passCount.FindAllStringSubmatch(result, -1)
	if len(ms) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(ms[len(ms)-1][1])
	return n
}

func isSearch(s step) bool {
	t := strings.ToLower(s.tool)
	if s.kind == api.ToolRead && (strings.Contains(t, "grep") || strings.Contains(t, "glob") || strings.Contains(t, "search") || strings.Contains(t, "find")) {
		return true
	}
	return s.kind == api.ToolShell && searchCommand.MatchString(s.command)
}

func isRead(s step) bool {
	if s.kind == api.ToolRead {
		return !isSearch(s)
	}
	return s.kind == api.ToolShell && readCommand.MatchString(s.command)
}

// buildReceipt folds a turn's tool calls, oldest first, into a receipt.
func buildReceipt(turn int, since time.Time, steps []step) *api.Receipt {
	rc := &api.Receipt{Turn: turn, Since: since}
	type tally struct {
		last   *api.Check
		failed int // failed runs before the last one
	}
	checks := map[string]*tally{"tests": {}, "lint": {}, "build": {}}
	files := map[string]bool{}
	var lastEdit time.Time
	for _, s := range steps {
		switch {
		case s.denied:
			rc.Blocked++
			continue
		case s.isError && !s.pending && s.kind != api.ToolShell:
			rc.Failed++
			continue
		}
		switch {
		case isSearch(s):
			rc.Searches++
		case isRead(s):
			rc.Reads++
		}
		if s.kind == api.ToolEdit && !s.isError {
			rc.Edits++
			if s.at.After(lastEdit) {
				lastEdit = s.at
			}
			for _, e := range s.edits {
				files[e.path] = true
				rc.Added += e.added
				rc.Removed += e.removed
			}
			for _, p := range s.paths {
				files[p] = true
			}
			rc.Added += s.added
			rc.Removed += s.removed
		}
		if s.kind != api.ToolShell || s.command == "" {
			continue
		}
		if subj, ok := commitSubject(s.command); ok && commitCommand.MatchString(commandHead(s.command)) && !s.isError && !s.pending {
			rc.Commit = subj
			if rc.Commit == "" {
				rc.Commit = "(no message)"
			}
		}
		status := "passed"
		switch {
		case s.pending:
			status = "running"
		case s.isError:
			status = "failed"
		}
		head := commandHead(s.command)
		for kind, name := range map[string]string{"tests": testRunner(head), "lint": lintRunner(head), "build": buildRunner(head)} {
			if name == "" {
				continue
			}
			t := checks[kind]
			if t.last != nil && t.last.Status == "failed" {
				t.failed++
			}
			c := &api.Check{Name: name, Command: s.command, At: s.at, Status: status}
			if kind == "tests" && status == "passed" {
				c.Count = testsPassed(s.result)
			}
			t.last = c
		}
		// A search that finds nothing exits 1: not a failure worth counting.
		if s.isError && !s.pending && !isSearch(s) && testRunner(head) == "" && lintRunner(head) == "" && buildRunner(head) == "" {
			rc.Failed++
		}
	}
	rc.Files = len(files)
	if !lastEdit.IsZero() {
		le := lastEdit
		rc.LastEdit = &le
	}
	for kind, t := range checks {
		if t.last == nil {
			continue
		}
		t.last.FailedBefore = t.failed
		t.last.Fresh = lastEdit.IsZero() || t.last.At.After(lastEdit)
		switch kind {
		case "tests":
			rc.Tests = t.last
		case "lint":
			rc.Lint = t.last
		case "build":
			rc.Build = t.last
		}
	}
	return rc
}

// receipt is the receipt of the feed's latest turn.
func (f *feed) receipt() *api.Receipt {
	turn, since := 0, time.Time{}
	for i := len(f.items) - 1; i >= 0; i-- {
		if it := f.items[i]; it.Kind == api.ActivityPrompt && !it.Sidechain {
			turn, since = it.Turn, it.At
			break
		}
	}
	var steps []step
	for _, it := range f.items {
		if it.At.Before(since) || it.Sidechain || it.Kind != api.ActivityTool || it.asks > 0 || it.plan {
			continue
		}
		s := step{at: it.At, kind: it.ToolKind, tool: it.Tool, command: it.command, result: it.Result,
			isError: it.IsError, pending: it.Pending, denied: it.denied}
		for _, e := range it.edits {
			s.edits = append(s.edits, fileEdit{path: e.path, op: e.op, added: e.added, removed: e.removed})
		}
		steps = append(steps, s)
	}
	return buildReceipt(turn, since, steps)
}

// ReceiptOf builds the latest turn's receipt from feed items, for sources
// without a transcript feed of their own (OpenCode, the demo). Shell
// commands come from "Run: …" summaries; files from "Edit …" summaries,
// with line counts from files when given (keyed by path).
func ReceiptOf(items []api.ActivityItem, files []api.FileChange) *api.Receipt {
	turn, since := 0, time.Time{}
	for i := len(items) - 1; i >= 0; i-- {
		if it := items[i]; it.Kind == api.ActivityPrompt && !it.Sidechain {
			turn, since = it.Turn, it.At
			break
		}
	}
	counts := map[string]api.FileChange{}
	for _, fc := range files {
		counts[fc.Path] = fc
	}
	seen := map[string]bool{}
	var steps []step
	for _, it := range items {
		if it.At.Before(since) || it.Sidechain || it.Kind != api.ActivityTool {
			continue
		}
		s := step{at: it.At, kind: it.ToolKind, tool: it.Tool, result: it.Result, isError: it.IsError, pending: it.Pending}
		if s.kind == api.ToolShell {
			s.command = strings.TrimSpace(strings.TrimPrefix(it.Summary, "Run:"))
		}
		if s.isError && deniedText.MatchString(it.Result) {
			s.denied = true
		}
		if s.kind == api.ToolEdit {
			p := it.Summary
			for _, pre := range []string{"Edit:", "Write:", "Edit", "Write"} {
				p = strings.TrimPrefix(p, pre)
			}
			p = strings.TrimSpace(p)
			if p != "" {
				s.paths = []string{p}
				if fc, ok := counts[p]; ok && !seen[p] {
					s.added, s.removed = fc.Added, fc.Removed
					seen[p] = true
				}
			}
		}
		steps = append(steps, s)
	}
	return buildReceipt(turn, since, steps)
}

// deniedText recognises a refusal in a tool result.
var deniedText = regexp.MustCompile(`(?i)\b(?:denied|blocked by|refused|not allowed by)\b`)
