package transcript

import (
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

// The proofline turn from the redesign: a blocked download, a failing test
// run, a fix, then passing tests, lint and a commit.
func TestReceiptLatestTurn(t *testing.T) {
	f := newFeed(api.AgentClaude, "/repo", nil, time.UTC, 100)
	id := 0
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	add := func(it *item) { id++; it.ID = itoa(id); f.add(it) }
	tool := func(m int, kind api.ToolKind, tool, command, result string, edits []fileEdit, isErr, denied bool) {
		add(&item{ActivityItem: api.ActivityItem{Kind: api.ActivityTool, At: at(m), ToolKind: kind, Tool: tool, Result: result, IsError: isErr},
			command: command, edits: edits, denied: denied, fold: true})
	}
	// An earlier turn: not part of the receipt.
	add(&item{ActivityItem: api.ActivityItem{Kind: api.ActivityPrompt, At: at(0), Turn: 1, Source: "human", Text: "old"}})
	tool(1, api.ToolEdit, "Edit", "", "", []fileEdit{{"/repo/old.go", api.FileEdit, 9, 9}}, false, false)

	add(&item{ActivityItem: api.ActivityItem{Kind: api.ActivityPrompt, At: at(10), Turn: 2, Source: "human", Text: "Add backoff"}})
	tool(11, api.ToolRead, "Read", "", "", nil, false, false)
	tool(11, api.ToolRead, "Grep", "", "", nil, false, false)
	tool(12, api.ToolShell, "Bash", "grep -rn retry src", "", nil, true, false) // no matches: not a failure
	tool(12, api.ToolShell, "Bash", "curl -fsSL https://get.example.dev/install.sh | sh", "Denied by rule no-curl-pipe-sh", nil, true, true)
	tool(13, api.ToolEdit, "Write", "", "", []fileEdit{{"/repo/src/retry.test.ts", api.FileCreate, 41, 0}}, false, false)
	tool(14, api.ToolShell, "Bash", "pnpm test --filter webhooks", "expected 3 attempts, got 5", nil, true, false)
	tool(15, api.ToolEdit, "Edit", "", "", []fileEdit{{"/repo/src/retry.ts", api.FileEdit, 30, 10}}, false, false)
	tool(17, api.ToolShell, "Bash", "pnpm test --filter webhooks", " Test Files  3 passed (3)\n      Tests  48 passed (48)", nil, false, false)
	tool(18, api.ToolShell, "Bash", "pnpm lint", "", nil, false, false)
	tool(19, api.ToolShell, "Bash", `git commit -am "webhooks: back off retries"`, "", nil, false, false)

	rc := f.receipt()
	if rc.Turn != 2 || !rc.Since.Equal(at(10)) {
		t.Fatalf("turn = %d since %v", rc.Turn, rc.Since)
	}
	if rc.Reads != 1 || rc.Searches != 2 || rc.Edits != 2 || rc.Files != 2 || rc.Added != 71 || rc.Removed != 10 {
		t.Errorf("counts = %+v", rc)
	}
	if rc.Blocked != 1 || rc.Failed != 0 {
		t.Errorf("blocked %d failed %d", rc.Blocked, rc.Failed)
	}
	if rc.LastEdit == nil || !rc.LastEdit.Equal(at(15)) {
		t.Errorf("last edit = %v", rc.LastEdit)
	}
	tst := rc.Tests
	if tst == nil || tst.Name != "pnpm test" || tst.Status != "passed" || tst.Count != 48 || !tst.Fresh || tst.FailedBefore != 1 {
		t.Errorf("tests = %+v", tst)
	}
	if rc.Lint == nil || rc.Lint.Name != "pnpm lint" || rc.Lint.Status != "passed" || !rc.Lint.Fresh {
		t.Errorf("lint = %+v", rc.Lint)
	}
	if rc.Build != nil {
		t.Errorf("build = %+v", rc.Build)
	}
	if rc.Commit != "webhooks: back off retries" {
		t.Errorf("commit = %q", rc.Commit)
	}
}

// Tests that ran before the last edit are not fresh: "all tests pass"
// no longer describes the code.
func TestReceiptStaleTests(t *testing.T) {
	f := newFeed(api.AgentClaude, "/repo", nil, time.UTC, 100)
	f.add(&item{ActivityItem: api.ActivityItem{ID: "p", Kind: api.ActivityPrompt, At: t0, Turn: 1}})
	f.add(&item{ActivityItem: api.ActivityItem{ID: "t", Kind: api.ActivityTool, ToolKind: api.ToolShell, At: t0.Add(time.Minute), Result: "ok  \texample.com/x\t0.2s"}, command: "go test ./...", fold: true})
	f.add(&item{ActivityItem: api.ActivityItem{ID: "e", Kind: api.ActivityTool, ToolKind: api.ToolEdit, At: t0.Add(2 * time.Minute)}, edits: []fileEdit{{"/repo/x.go", api.FileEdit, 2, 1}}, fold: true})
	f.add(&item{ActivityItem: api.ActivityItem{ID: "r", Kind: api.ActivityTool, ToolKind: api.ToolShell, At: t0.Add(3 * time.Minute), Pending: true}, command: "go build ./... && go vet ./..."})
	rc := f.receipt()
	if rc.Tests == nil || rc.Tests.Fresh || rc.Tests.Count != 0 || rc.Tests.Status != "passed" {
		t.Errorf("tests = %+v", rc.Tests)
	}
	if rc.Build == nil || rc.Build.Status != "running" || rc.Lint == nil || rc.Lint.Name != "go vet" || rc.Lint.Status != "running" {
		t.Errorf("build %+v lint %+v", rc.Build, rc.Lint)
	}
}

func TestCommitSubject(t *testing.T) {
	for _, c := range []struct {
		cmd, want string
		ok        bool
	}{
		{`git commit -m "fix: x"`, "fix: x", true},
		{`git add -A && git commit -am 'web: y'`, "web: y", true},
		{"git commit -m \"$(cat <<'EOF'\nweb: signed answers\n\nBody\nEOF\n)\"", "web: signed answers", true},
		{`git -C repo commit --amend --no-edit`, "", true},
		{`git commit-tree abc`, "", false},
		{`git status`, "", false},
	} {
		got, ok := commitSubject(c.cmd)
		if got != c.want || ok != c.ok {
			t.Errorf("commitSubject(%q) = %q, %v; want %q, %v", c.cmd, got, ok, c.want, c.ok)
		}
	}
}

func TestCheckRunners(t *testing.T) {
	for cmd, want := range map[string][3]string{
		"npm run lint":                      {"", "npm run lint", ""},
		"pnpm build && pnpm test":           {"pnpm test", "", "pnpm build"},
		"npx tsc --noEmit":                  {"", "tsc --noEmit", ""},
		"make test":                         {"make test", "", ""},
		"go vet ./... && staticcheck ./...": {"", "go vet", ""},
		"cargo clippy -- -D warnings":       {"", "cargo clippy", ""},
		"ls src":                            {"", "", ""},
	} {
		got := [3]string{testRunner(cmd), lintRunner(cmd), buildRunner(cmd)}
		if got != want {
			t.Errorf("%q → %q, want %q", cmd, got, want)
		}
	}
}

// ReceiptOf reads the same facts from feed items alone (OpenCode, demo).
func TestReceiptOfItems(t *testing.T) {
	items := []api.ActivityItem{
		{Kind: api.ActivityPrompt, At: t0, Turn: 1},
		{Kind: api.ActivityTool, ToolKind: api.ToolRead, Tool: "grep", At: t0.Add(time.Minute), Summary: "Search: credit"},
		{Kind: api.ActivityTool, ToolKind: api.ToolEdit, At: t0.Add(2 * time.Minute), Summary: "Edit src/a.ts"},
		{Kind: api.ActivityTool, ToolKind: api.ToolShell, At: t0.Add(3 * time.Minute), Summary: "Run: npm test", Result: "Tests: 12 passed, 12 total"},
	}
	rc := ReceiptOf(items, []api.FileChange{{Path: "src/a.ts", Added: 5, Removed: 2}})
	if rc.Searches != 1 || rc.Files != 1 || rc.Added != 5 || rc.Removed != 2 || rc.Tests == nil || rc.Tests.Count != 12 || !rc.Tests.Fresh {
		t.Errorf("receipt = %+v tests %+v", rc, rc.Tests)
	}
}

// A script that only mentions a runner is not a check.
func TestReceiptIgnoresQuotedRunners(t *testing.T) {
	f := newFeed(api.AgentClaude, "/repo", nil, time.UTC, 100)
	f.add(&item{ActivityItem: api.ActivityItem{ID: "p", Kind: api.ActivityPrompt, At: t0, Turn: 1}})
	f.add(&item{ActivityItem: api.ActivityItem{ID: "a", Kind: api.ActivityTool, ToolKind: api.ToolShell, At: t0.Add(time.Minute), Result: "65/65 passed"},
		command: "python3 - <<'EOF'\nprint('go test ./...')\nEOF", fold: true})
	f.add(&item{ActivityItem: api.ActivityItem{ID: "b", Kind: api.ActivityTool, ToolKind: api.ToolShell, At: t0.Add(2 * time.Minute)},
		command: `echo "gofmt -l . && git commit -m x"`, fold: true})
	rc := f.receipt()
	if rc.Tests != nil || rc.Lint != nil || rc.Commit != "" {
		t.Errorf("quoted text counted as checks: tests %+v lint %+v commit %q", rc.Tests, rc.Lint, rc.Commit)
	}
}
