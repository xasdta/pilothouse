package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xasdta/pilothouse/internal/api"
)

const (
	sgHome = "/Users/ann"
	sgRepo = "/Users/ann/src/api-server"
)

func sgShell(cmd string) api.Request {
	in, _ := json.Marshal(map[string]string{"command": cmd})
	return api.Request{ID: "r_1", Agent: api.AgentClaude, Checkpoint: api.CheckpointPermission, Tool: "Bash",
		ToolKind: api.ToolShell, Command: cmd, Input: in, Cwd: sgRepo}
}

func titles(sg []api.RuleSuggestion) string {
	var out []string
	for _, s := range sg {
		out = append(out, s.Title)
	}
	return strings.Join(out, " | ")
}

func TestSuggestionsShell(t *testing.T) {
	tests := []struct {
		cmd, branch, want string
	}{
		{cmd: "npm test", want: "Always allow `npm test` in api-server"},
		{cmd: "npm test -- --grep auth", want: "Always allow `npm test` in api-server"},
		{cmd: "cd src && go test ./... 2>&1 | tail -30", want: "Always allow `go test` in api-server"},
		{cmd: "npm run build && npm test", want: "Always allow `npm run`, `npm test` in api-server"},
		{cmd: "mkdir -p build/out", want: "Always allow `mkdir` in api-server"},
		{cmd: "git push origin feature/x", want: "Always ask before `git push`"},
		{cmd: "git push origin main", want: "Always ask before `git push`"},
		{cmd: "git push -f origin main", want: "Always ask before `git push` | Never allow force-pushes"},
		{cmd: "rm -rf /", want: "Always ask before `rm` | Never allow `rm -rf`"},
		{cmd: "git reset --hard HEAD~1", want: "Always ask before `git reset` | Never allow `git reset --hard`"},
		{cmd: "terraform apply -auto-approve", want: "Always ask before `terraform apply` | Never allow `terraform apply`"},
		{cmd: "npm test && kubectl apply -f k8s", want: "Always ask before `kubectl apply` | Never allow `kubectl apply`"},
		{cmd: "npm install lodash", want: "Always ask before `npm install`"},
		// Never allow-everything, never a suggestion that wouldn't match.
		{cmd: "bash -c 'npm test'", want: ""},
		{cmd: "sudo npm test", want: ""},
		{cmd: "CI=1 npm test", want: ""},
		{cmd: "python3 manage.py test", want: ""},
		{cmd: "node scripts/build.js", want: ""},
		{cmd: "npm test > out.log", want: ""},
		{cmd: "npm test $FLAGS", want: ""},
		{cmd: "cd /tmp && npm test", want: ""},
		{cmd: "docker run --rm alpine sh", want: ""},
		{cmd: "eval \"$(direnv export bash)\"", want: ""},
		{cmd: "xargs -n1 echo", want: ""},
		{cmd: "", want: ""},
	}
	for _, tc := range tests {
		got := suggestionsFor(sgShell(tc.cmd), sgRepo, tc.branch, sgHome, nil)
		if titles(got) != tc.want {
			t.Errorf("%q:\n  got  %q\n  want %q", tc.cmd, titles(got), tc.want)
		}
		for _, s := range got {
			if !strings.HasPrefix(s.ID, "sg_") || s.Rule.Name == "" || s.Rule.When != "" {
				t.Errorf("%q: bad suggestion %+v", tc.cmd, s)
			}
			if s.Rule.Action == api.BehaviorAllow && (s.Scope != "repo" || s.Rule.Match["repo"][0] != sgRepo) {
				t.Errorf("%q: allow not scoped to the repo: %+v", tc.cmd, s)
			}
		}
	}
	// Outside a repo there is nothing to scope an allow to.
	if got := suggestionsFor(sgShell("npm test"), "", "", sgHome, nil); len(got) != 0 {
		t.Fatalf("no repo: %s", titles(got))
	}
}

func TestSuggestionsEditsAndMCP(t *testing.T) {
	edit := func(paths ...string) api.Request {
		return api.Request{ID: "r_2", Tool: "Edit", ToolKind: api.ToolEdit, Paths: paths, Cwd: sgRepo}
	}
	for _, tc := range []struct {
		req  api.Request
		want string
	}{
		{edit(sgRepo + "/src/auth/login.ts"), "Allow edits under `src/**` in api-server"},
		{edit("src/a.go", "src/b/c.go"), "Allow edits under `src/**` in api-server"},
		{edit(sgRepo + "/README.md"), "Allow edits to `README.md` in api-server"},
		{edit(sgRepo+"/src/a.go", sgRepo+"/docs/b.md"), ""},
		{edit(sgRepo + "/.env"), ""},
		{edit(sgRepo + "/.github/workflows/ci.yml"), ""},
		{edit(sgRepo + "/config/server.pem"), ""},
		{edit(sgHome + "/.zshrc"), ""},
		{api.Request{ID: "r_3", Tool: "mcp__github__get_issue", ToolKind: api.ToolMCP}, "Always allow `mcp__github__get_issue` | Always ask before `mcp__github__get_issue`"},
		{api.Request{ID: "r_4", Tool: "WebFetch", ToolKind: api.ToolWeb}, ""},
	} {
		got := suggestionsFor(tc.req, sgRepo, "main", sgHome, nil)
		if titles(got) != tc.want {
			t.Errorf("%+v:\n  got  %q\n  want %q", tc.req.Paths, titles(got), tc.want)
		}
	}
	got := suggestionsFor(edit(sgRepo+"/src/x.go"), sgRepo, "", sgHome, nil)
	if p := got[0].Rule.Match["path"][0]; p != sgRepo+"/src/**" {
		t.Fatalf("path glob = %q", p)
	}
}

func TestSuggestionNamesAndExisting(t *testing.T) {
	req := sgShell("npm test")
	first := suggestionsFor(req, sgRepo, "", sgHome, nil)
	if len(first) != 1 || first[0].Rule.Name != "allow-npm-test-in-api-server" {
		t.Fatalf("first = %+v", first)
	}
	// The name is taken by another rule: a fresh one.
	taken := []api.RuleInfo{{RuleSpec: api.RuleSpec{Name: "allow-npm-test-in-api-server", Action: api.BehaviorDeny}, Source: api.RuleSourceUser}}
	if got := suggestionsFor(req, sgRepo, "", sgHome, taken); got[0].Rule.Name != "allow-npm-test-in-api-server-2" || got[0].ID == first[0].ID {
		t.Fatalf("renamed = %+v", got)
	}
	// Already a managed rule: not suggested again.
	existing := []api.RuleInfo{{RuleSpec: first[0].Rule, Source: api.RuleSourceManaged, Managed: true}}
	if got := suggestionsFor(req, sgRepo, "", sgHome, existing); len(got) != 0 {
		t.Fatalf("duplicate suggested: %+v", got)
	}
	// Stable ids.
	if again := suggestionsFor(req, sgRepo, "", sgHome, nil); again[0].ID != first[0].ID {
		t.Fatal("suggestion id not stable")
	}
	// Glob syntax in a repo path is escaped.
	odd := "/Users/ann/src/a{b,c}*"
	got := suggestionsFor(api.Request{ID: "r", Tool: "Bash", ToolKind: api.ToolShell, Command: "make", Cwd: odd}, odd, "", sgHome, nil)
	if len(got) != 1 || got[0].Rule.Match["repo"][0] != `/Users/ann/src/a\{b\,c\}\*` {
		t.Fatalf("escaped repo: %+v", got)
	}
}
