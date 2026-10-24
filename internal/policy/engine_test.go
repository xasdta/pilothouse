package policy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

// testEnv points every Pilothouse location at temp dirs (BUILD-SPEC §2.1).
type testEnv struct {
	home   string // fake user home
	agHome string // fake ~/.pilothouse
	run    string
	repo   string // a fake git repo root
}

func newTestEnv(t testing.TB) *testEnv {
	t.Helper()
	home := t.TempDir()
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r // macOS: /var → /private/var, keeps expectations simple
	}
	run, err := os.MkdirTemp("/tmp", "ag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(run) })
	te := &testEnv{home: home, agHome: filepath.Join(home, ".pilothouse"), run: run, repo: filepath.Join(home, "src", "app")}
	t.Setenv("PILOTHOUSE_HOME", te.agHome)
	t.Setenv("PILOTHOUSE_USER_HOME", te.home)
	t.Setenv("PILOTHOUSE_RUN_DIR", te.run)
	for _, d := range []string{te.agHome, te.repo} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return te
}

func (te *testEnv) userFile() string { return filepath.Join(te.agHome, "policy.yaml") }
func (te *testEnv) repoFile() string { return filepath.Join(te.repo, RepoPolicyFile) }

func writeFile(t testing.TB, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (te *testEnv) load(t testing.TB) (*Engine, api.PolicyStatus) {
	t.Helper()
	return Load(DefaultOptions(filepath.Join(te.home, ".claude", "settings.json"), filepath.Join(te.home, ".codex", "config.toml")))
}

// fakeClock lets tests step past the 2 s re-check interval.
type fakeClock struct{ ns atomic.Int64 }

func (c *fakeClock) now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *fakeClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

func withClock(e *Engine) *fakeClock {
	c := &fakeClock{}
	c.ns.Store(time.Now().UnixNano())
	e.clock = c.now
	return c
}

func shell(cmd string) Input {
	return Input{Agent: api.AgentClaude, Tool: "Bash", ToolKind: api.ToolShell, Command: cmd, Cwd: "/tmp", Checkpoint: api.CheckpointPreTool}
}

func ruleNames(ms []Match) []string {
	var out []string
	for _, m := range ms {
		s := m.Rule
		if m.Ignored != "" {
			s += "(ignored)"
		}
		out = append(out, s)
	}
	return out
}

func TestPrecedence(t *testing.T) {
	te := newTestEnv(t)
	writeFile(t, te.userFile(), `version: 1
rules:
  - {name: allow-all-shell, match: {kind: shell}, action: allow}
  - {name: ask-git, match: {command: "git *"}, action: ask, message: ask msg}
  - {name: deny-push, match: {command: "git push*"}, action: deny, message: no pushing}
  - {name: deny-push-2, match: {command: "git push*"}, action: deny, message: second}
  - {name: allow-ls, match: {command: "ls*"}, action: allow}
`)
	e, st := te.load(t)
	if len(st.Errors) != 0 {
		t.Fatal(st.Errors)
	}
	tests := []struct {
		cmd     string
		action  api.Behavior
		rule    string
		msg     string
		matched string
	}{
		{"git push origin x", api.BehaviorDeny, "deny-push", "no pushing", "allow-all-shell,ask-git,deny-push,deny-push-2"},
		{"git status", api.BehaviorAsk, "ask-git", "ask msg", "allow-all-shell,ask-git"},
		{"ls -la", api.BehaviorAllow, "allow-all-shell", "", "allow-all-shell,allow-ls"},
	}
	for _, tt := range tests {
		r := e.Evaluate(shell(tt.cmd))
		if r.Action != tt.action || r.Rule != tt.rule || r.Message != tt.msg || r.File != te.userFile() {
			t.Errorf("%q: got %+v", tt.cmd, r)
		}
		if got := strings.Join(ruleNames(r.Matched), ","); got != tt.matched {
			t.Errorf("%q: matched %s, want %s", tt.cmd, got, tt.matched)
		}
	}
	if r := e.Evaluate(Input{Tool: "Read", ToolKind: api.ToolRead}); r.Action != "" || len(r.Matched) != 0 {
		t.Errorf("no rule should match: %+v", r)
	}
}

func TestDefaultMessages(t *testing.T) {
	te := newTestEnv(t)
	writeFile(t, te.userFile(), "version: 1\nrules:\n  - {name: d, match: {command: x}, action: deny}\n  - {name: a, match: {command: y}, action: ask}\n")
	e, _ := te.load(t)
	if m := e.Evaluate(shell("x")).Message; !strings.Contains(m, `"d"`) {
		t.Errorf("deny message %q", m)
	}
	if m := e.Evaluate(shell("y")).Message; !strings.Contains(m, `"a"`) {
		t.Errorf("ask message %q", m)
	}
}

func TestAgentsFilter(t *testing.T) {
	te := newTestEnv(t)
	writeFile(t, te.userFile(), `version: 1
rules:
  - {name: codex-only, agents: [codex], match: {kind: web}, action: deny}
  - {name: match-agent, match: {agent: "{claude,generic}", kind: web}, action: ask}
`)
	e, _ := te.load(t)
	for agent, want := range map[api.AgentKind]api.Behavior{
		api.AgentCodex: api.BehaviorDeny, api.AgentClaude: api.BehaviorAsk, api.AgentGeneric: api.BehaviorAsk,
	} {
		r := e.Evaluate(Input{Agent: agent, Tool: "WebFetch", ToolKind: api.ToolWeb})
		if r.Action != want {
			t.Errorf("%s: %s, want %s", agent, r.Action, want)
		}
	}
}

func TestMatchFields(t *testing.T) {
	te := newTestEnv(t)
	writeFile(t, te.userFile(), `version: 1
rules:
  - {name: by-path, match: {path: "~/secrets/**"}, action: deny}
  - {name: by-rel-path, match: {path: "*.pem"}, action: deny}
  - {name: by-cwd, match: {cwd: "~/src/**", tool: Write}, action: ask}
  - {name: by-repo, match: {repo: "~/src/app", tool: Edit}, action: ask}
  - {name: by-branch, match: {branch: "release/*", kind: shell}, action: ask}
`)
	e, _ := te.load(t)
	tests := []struct {
		name string
		in   Input
		rule string
	}{
		{"tilde path", Input{ToolKind: api.ToolRead, Paths: []string{"~/secrets/a/b"}}, "by-path"},
		{"relative path made absolute", Input{ToolKind: api.ToolRead, Paths: []string{"../secrets/x"}, Cwd: te.home + "/src"}, "by-path"},
		{"any path", Input{ToolKind: api.ToolRead, Paths: []string{"/ok", "/x/server.pem"}}, "by-rel-path"},
		{"no path", Input{ToolKind: api.ToolRead}, ""},
		{"cwd", Input{Tool: "Write", Cwd: "~/src/app/sub"}, "by-cwd"},
		{"cwd outside", Input{Tool: "Write", Cwd: "/elsewhere"}, ""},
		{"repo", Input{Tool: "Edit", Repo: te.repo}, "by-repo"},
		{"branch", Input{ToolKind: api.ToolShell, Command: "ls", Branch: "release/1.2"}, "by-branch"},
		{"branch nested", Input{ToolKind: api.ToolShell, Command: "ls", Branch: "release/1/2"}, ""},
	}
	for _, tt := range tests {
		if r := e.Evaluate(tt.in); r.Rule != tt.rule {
			t.Errorf("%s: rule %q, want %q (%+v)", tt.name, r.Rule, tt.rule, r)
		}
	}
}

func TestCEL(t *testing.T) {
	te := newTestEnv(t)
	writeFile(t, te.userFile(), `version: 1
rules:
  - name: glob-fn
    when: 'paths.exists(p, glob(p, "~/keys/**")) && path.glob("~/keys/*")'
    action: deny
  - name: has-word
    when: 'has_word(command, "--force") && command.has_word("push")'
    action: deny
  - name: input-map
    when: 'has(input.url) && input.url.startsWith("http://")'
    action: ask
  - name: night
    when: 'hour >= 22 && weekday == 6'
    action: ask
  - name: strings-ext
    when: 'command.lowerAscii().split(" ").exists(w, w == "danger")'
    action: deny
  - name: meta
    when: 'agent == "codex" && tool == "shell" && kind == "shell" && mode == "wrapped" && cwd == "/w" && repo == "/w" && branch == "b" && checkpoint == "pre_tool"'
    action: ask
  - name: runtime-error
    when: 'input.missing == "x"'
    action: deny
  - name: dyn-not-bool
    when: 'input.n'
    action: deny
`)
	e, st := te.load(t)
	if len(st.Errors) != 0 {
		t.Fatal(st.Errors)
	}
	sat := time.Date(2026, 10, 24, 23, 0, 0, 0, time.Local) // a Saturday
	tests := []struct {
		name string
		in   Input
		rule string
	}{
		{"glob", Input{ToolKind: api.ToolRead, Paths: []string{"~/keys/k1"}}, "glob-fn"},
		{"glob no", Input{ToolKind: api.ToolRead, Paths: []string{"~/keys/a/k1"}}, ""},
		{"has_word", Input{Command: "git push --force"}, "has-word"},
		{"has_word lease", Input{Command: "git push --force-with-lease"}, ""},
		{"input", Input{Input: json.RawMessage(`{"url":"http://x"}`)}, "input-map"},
		{"input https", Input{Input: json.RawMessage(`{"url":"https://x"}`)}, ""},
		{"input not object", Input{Input: json.RawMessage(`[1,2]`)}, ""},
		{"time", Input{Command: "x", Now: sat}, "night"},
		{"time weekday", Input{Command: "x", Now: sat.Add(-20 * time.Hour)}, ""},
		{"strings", Input{Command: "run DANGER now"}, "strings-ext"},
		{"vars", Input{Agent: "codex", Tool: "shell", ToolKind: "shell", Mode: "wrapped", Cwd: "/w", Repo: "/w", Branch: "b", Checkpoint: "pre_tool", Command: "ls"}, "meta"},
	}
	for _, tt := range tests {
		r := e.Evaluate(tt.in)
		if r.Rule != tt.rule {
			t.Errorf("%s: rule %q, want %q (%v)", tt.name, r.Rule, tt.rule, r.Matched)
		}
	}
	// Run-time errors are reported as ignored matches, never decisions.
	r := e.Evaluate(Input{Input: json.RawMessage(`{"n": 3}`)})
	if r.Action != "" {
		t.Fatalf("errors must not decide: %+v", r)
	}
	var ignored []string
	for _, m := range r.Matched {
		if m.Ignored != "" {
			ignored = append(ignored, m.Rule)
		}
	}
	sort.Strings(ignored)
	if strings.Join(ignored, ",") != "dyn-not-bool,input-map,runtime-error" && strings.Join(ignored, ",") != "dyn-not-bool,runtime-error" {
		t.Errorf("ignored = %v", ignored)
	}
}

func TestRepoTrust(t *testing.T) {
	te := newTestEnv(t)
	writeFile(t, te.repoFile(), `version: 1
rules:
  - {name: repo-allow-make, match: {command: "make*"}, action: allow}
  - {name: repo-deny-deploy, match: {command: "make deploy*"}, action: deny}
  - {name: repo-ask-curl, match: {command: "curl*"}, action: ask}
`)
	e, _ := te.load(t)
	clk := withClock(e)
	in := shell("make build")
	in.Repo = te.repo

	r := e.Evaluate(in)
	if r.Action != "" || len(r.Matched) != 1 || r.Matched[0].Ignored == "" {
		t.Fatalf("untrusted allow must be ignored: %+v", r)
	}
	in.Command = "make deploy"
	if r := e.Evaluate(in); r.Action != api.BehaviorDeny || r.File != te.repoFile() {
		t.Fatalf("untrusted deny must apply: %+v", r)
	}
	in.Command = "curl x"
	if r := e.Evaluate(in); r.Action != api.BehaviorAsk {
		t.Fatalf("untrusted ask must apply: %+v", r)
	}
	st := e.Status()
	if len(st.Files) != 4 || st.Files[2].Scope != ScopeManaged || st.Files[3].Scope != ScopeRepo || st.Files[3].Trusted {
		t.Fatalf("status = %+v", st.Files)
	}

	added, err := TrustRepo(DefaultOptions().TrustedReposFile, te.repo)
	if err != nil || !added {
		t.Fatal(added, err)
	}
	if again, _ := TrustRepo(DefaultOptions().TrustedReposFile, te.repo); again {
		t.Fatal("second trust should report already trusted")
	}
	clk.advance(3 * time.Second)
	in.Command = "make build"
	if r := e.Evaluate(in); r.Action != api.BehaviorAllow || r.Matched[0].Ignored != "" {
		t.Fatalf("trusted allow must apply: %+v", r)
	}
	if !e.Status().Files[2].Trusted {
		t.Fatal("status should show trusted")
	}

	// Trust via a symlinked path counts too.
	link := filepath.Join(te.home, "link")
	if err := os.Symlink(te.repo, link); err != nil {
		t.Fatal(err)
	}
	in.Repo = link
	if r := e.Evaluate(in); r.Action != api.BehaviorAllow {
		t.Fatalf("symlinked repo: %+v", r)
	}

	if removed, err := UntrustRepo(DefaultOptions().TrustedReposFile, te.repo); err != nil || !removed {
		t.Fatal(removed, err)
	}
	clk.advance(3 * time.Second)
	in.Repo = te.repo
	if r := e.Evaluate(in); r.Action != "" {
		t.Fatalf("untrusted again: %+v", r)
	}
	if removed, _ := UntrustRepo(DefaultOptions().TrustedReposFile, te.repo); removed {
		t.Fatal("double untrust")
	}
}

func TestRepoFileCachedAndRediscovered(t *testing.T) {
	te := newTestEnv(t)
	e, _ := te.load(t)
	clk := withClock(e)
	in := shell("terraform apply")
	in.Repo = te.repo
	if r := e.Evaluate(in); r.Action != "" {
		t.Fatal("no repo file yet")
	}
	writeFile(t, te.repoFile(), "version: 1\nrules:\n  - {name: no-tf, match: {command: \"terraform apply*\"}, action: deny}\n")
	if r := e.Evaluate(in); r.Action != "" {
		t.Fatal("within 2 s the cached (absent) file is used")
	}
	clk.advance(2100 * time.Millisecond)
	if r := e.Evaluate(in); r.Action != api.BehaviorDeny {
		t.Fatalf("after 2 s the new file is found: %+v", r)
	}
	os.Remove(te.repoFile())
	clk.advance(2100 * time.Millisecond)
	if r := e.Evaluate(in); r.Action != "" {
		t.Fatalf("deleted file drops its rules: %+v", r)
	}
}

func TestLastGoodOnError(t *testing.T) {
	te := newTestEnv(t)
	good := "version: 1\nrules:\n  - {name: no-push, match: {command: \"git push*\"}, action: deny}\n"
	writeFile(t, te.userFile(), good)
	e, st := te.load(t)
	if st.Rules != 2 || len(st.Errors) != 0 {
		t.Fatalf("status %+v", st)
	}
	if e.Changed() {
		t.Fatal("unchanged")
	}
	// Break the file: a different size guarantees a new stamp.
	writeFile(t, te.userFile(), "version: 1\nrules:\n  - {name: no-push, match: {command: \"git push*\"}, action: denyy}\n")
	if !e.Changed() {
		t.Fatal("Changed should notice the edit")
	}
	st = e.Reload()
	if len(st.Errors) < 2 || !strings.Contains(st.Errors[0], "policy.yaml:3:") || !strings.Contains(st.Errors[1], "last valid version (1 rules)") {
		t.Fatalf("errors = %q", st.Errors)
	}
	if st.Files[1].Error == "" || st.Files[1].Rules != 1 {
		t.Fatalf("file status %+v", st.Files[1])
	}
	if r := e.Evaluate(shell("git push")); r.Action != api.BehaviorDeny {
		t.Fatal("last good rules must still apply")
	}
	writeFile(t, te.userFile(), good+"  - {name: no-pull, match: {command: \"git pull*\"}, action: deny}\n")
	st = e.Reload()
	if len(st.Errors) != 0 || st.Rules != 3 {
		t.Fatalf("fixed: %+v", st)
	}
	if r := e.Evaluate(shell("git pull")); r.Action != api.BehaviorDeny {
		t.Fatal("new rules apply")
	}
}

func TestInvalidAtFirstLoad(t *testing.T) {
	te := newTestEnv(t)
	writeFile(t, te.userFile(), "version: 1\nrules: [")
	e, st := te.load(t)
	if e == nil {
		t.Fatal("Load returned nil")
	}
	if len(st.Errors) != 1 || st.Rules != 1 {
		t.Fatalf("status %+v", st)
	}
	// Built-ins still work.
	if r := e.Evaluate(shell("pilothouse approve r_1")); r.Action != api.BehaviorAsk {
		t.Fatalf("builtin: %+v", r)
	}
}

func TestMissingFilesAreFine(t *testing.T) {
	te := newTestEnv(t)
	e, st := te.load(t)
	if len(st.Errors) != 0 || st.Rules != 1 || len(st.Files) != 3 {
		t.Fatalf("status %+v", st)
	}
	if e.Changed() {
		t.Fatal("nothing changed")
	}
	writeFile(t, te.userFile(), "version: 1\n")
	if !e.Changed() {
		t.Fatal("new file is a change")
	}
	// Zero options must not panic either.
	e2, _ := Load(Options{})
	_ = e2.Evaluate(shell("ls"))
}

func TestBrokenTrustFile(t *testing.T) {
	te := newTestEnv(t)
	writeFile(t, filepath.Join(te.agHome, "trusted-repos.yaml"), "version: 1\nrepos: [{pth: /x}]\n")
	_, st := te.load(t)
	if len(st.Errors) != 1 || !strings.Contains(st.Errors[0], "trusted-repos.yaml") {
		t.Fatalf("errors %q", st.Errors)
	}
	if _, err := TrustRepo(filepath.Join(te.agHome, "trusted-repos.yaml"), te.repo); err == nil {
		t.Fatal("trust must refuse to rewrite a broken file")
	}
}

func TestTrustFilePermissions(t *testing.T) {
	te := newTestEnv(t)
	f := DefaultOptions().TrustedReposFile
	if _, err := TrustRepo(f, te.repo); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(f)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v err %v", fi.Mode(), err)
	}
	repos, err := ReadTrustedRepos(f)
	if err != nil || len(repos) != 1 || repos[0].Path != te.repo || repos[0].TrustedAt.IsZero() {
		t.Fatalf("repos %+v err %v", repos, err)
	}
}

func TestConcurrentEvaluateAndReload(t *testing.T) {
	te := newTestEnv(t)
	writeFile(t, te.userFile(), "version: 1\nrules:\n  - {name: d, match: {command: \"rm*\"}, action: deny}\n")
	writeFile(t, te.repoFile(), "version: 1\nrules:\n  - {name: r, match: {command: \"curl*\"}, action: ask}\n")
	e, _ := te.load(t)
	clk := withClock(e)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				in := shell("rm -rf x")
				in.Repo = te.repo
				if r := e.Evaluate(in); r.Action != api.BehaviorDeny {
					t.Errorf("goroutine %d: %+v", g, r)
					return
				}
				in.Repo = fmt.Sprintf("%s/r%d", te.home, i%300) // churn the repo cache past maxRepos
				e.Evaluate(in)
				_ = e.Status()
			}
		}(g)
	}
	for i := 0; i < 50; i++ {
		// Alternate valid and invalid versions; deny stays in force throughout.
		if i%2 == 0 {
			writeFile(t, te.userFile(), "version: 1\nrules:\n  - {name: d, match: {command: \"rm*\"}, action: deny}\n# "+strings.Repeat("x", i)+"\n")
		} else {
			writeFile(t, te.userFile(), "version: 1\nrules: [ broken "+strings.Repeat("x", i))
		}
		clk.advance(2100 * time.Millisecond)
		if i%5 == 0 {
			e.Reload()
		}
		_ = e.Changed()
		time.Sleep(time.Millisecond)
	}
	close(stop)
	wg.Wait()
}

func TestStarterPolicyBehaviour(t *testing.T) {
	te := newTestEnv(t)
	writeFile(t, te.userFile(), StarterPolicy)
	e, st := te.load(t)
	if len(st.Errors) != 0 {
		t.Fatal(st.Errors)
	}
	onMain := func(in Input) Input { in.Branch = "main"; return in }
	onFeat := func(in Input) Input { in.Branch = "feature/x"; return in }
	read := func(p string) Input {
		return Input{Agent: api.AgentClaude, Tool: "Read", ToolKind: api.ToolRead, Paths: []string{p}, Cwd: te.repo}
	}
	tests := []struct {
		name string
		in   Input
		want api.Behavior
		rule string
	}{
		{"force push", onFeat(shell("git push --force origin x")), api.BehaviorDeny, "no-force-push"},
		{"force push -f", onFeat(shell("git push -f")), api.BehaviorDeny, "no-force-push"},
		{"force push +refspec", onFeat(shell("git push origin +feature/x")), api.BehaviorDeny, "no-force-push"},
		{"lease is not force", onFeat(shell("git push --force-with-lease origin feature/x")), "", ""},
		{"rm -rf /", shell("rm -rf /"), api.BehaviorDeny, "no-rm-rf-root"},
		{"sudo rm -rf ~", shell("sudo rm -fr ~"), api.BehaviorDeny, "no-rm-rf-root"},
		{"rm -rf $HOME/", shell("rm -r -f $HOME/"), api.BehaviorDeny, "no-rm-rf-root"},
		{"rm -rf build", shell("rm -rf build"), "", ""},
		{"rm then cd /", shell("rm -rf build && cd /"), "", ""},
		{"rm / not recursive", shell("rm /tmp/x"), "", ""},
		{"read .env", read(".env"), api.BehaviorDeny, "no-secret-files"},
		{"read .env.local", read("config/.env.local"), api.BehaviorDeny, "no-secret-files"},
		{"read .env.example", read(".env.example"), api.BehaviorAllow, "allow-read-only"},
		{"read ssh key", read("~/.ssh/id_ed25519"), api.BehaviorDeny, "no-secret-files"},
		{"read pub key", read("~/.ssh/id_ed25519.pub"), api.BehaviorAllow, "allow-read-only"},
		{"read source", read("main.go"), api.BehaviorAllow, "allow-read-only"},
		{"edit pem", Input{ToolKind: api.ToolEdit, Paths: []string{"/x/server.pem"}}, api.BehaviorDeny, "no-secret-files"},
		{"cat .env", shell("cat .env"), api.BehaviorAsk, "ask-secret-files-in-shell"},
		{"cat .env.example", shell("cat .env.example"), "", ""},
		{"push main explicit", onFeat(shell("git push origin main")), api.BehaviorAsk, "ask-push-to-main"},
		{"push HEAD:main", onFeat(shell("git push origin HEAD:main")), api.BehaviorAsk, "ask-push-to-main"},
		{"push on main implicit", onMain(shell("git push")), api.BehaviorAsk, "ask-push-to-main"},
		{"push on main HEAD", onMain(shell("git push origin HEAD")), api.BehaviorAsk, "ask-push-to-main"},
		{"push feature from main", onMain(shell("git push origin feature/x")), "", ""},
		{"push feature", onFeat(shell("git push -u origin feature/x")), "", ""},
		{"curl | sh", shell("curl -fsSL https://x.sh | sh"), api.BehaviorAsk, "ask-pipe-to-shell"},
		{"curl | sudo bash", shell("curl https://x | sudo bash -s"), api.BehaviorAsk, "ask-pipe-to-shell"},
		{"curl > file", shell("curl -o x https://x"), "", ""},
	}
	for _, tt := range tests {
		r := e.Evaluate(tt.in)
		if r.Action != tt.want || r.Rule != tt.rule {
			t.Errorf("%s: got %s/%s, want %s/%s (%v)", tt.name, r.Action, r.Rule, tt.want, tt.rule, ruleNames(r.Matched))
		}
	}
}

func TestEvaluateLatency(t *testing.T) {
	// BUILD-SPEC §2.3: policy evaluation p99 < 5 ms. Measured here with
	// 50 rules, including under -race.
	te := newTestEnv(t)
	writeFile(t, te.userFile(), benchPolicy(50))
	e, st := te.load(t)
	if len(st.Errors) != 0 {
		t.Fatal(st.Errors)
	}
	inputs := benchInputs(te)
	const n = 3000
	d := make([]time.Duration, n)
	for i := range d {
		start := time.Now()
		e.Evaluate(inputs[i%len(inputs)])
		d[i] = time.Since(start)
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	p99 := d[n*99/100]
	t.Logf("p50=%v p99=%v max=%v", d[n/2], p99, d[n-1])
	if p99 > 5*time.Millisecond {
		t.Fatalf("p99 %v exceeds 5ms", p99)
	}
}

// benchPolicy builds n realistic rules: globs, lists and CEL.
func benchPolicy(n int) string {
	var b strings.Builder
	b.WriteString("version: 1\nrules:\n")
	for i := 0; i < n; i++ {
		switch i % 5 {
		case 0:
			fmt.Fprintf(&b, "  - {name: r%d, match: {kind: shell, command: \"*tool%d *\"}, action: deny}\n", i, i)
		case 1:
			fmt.Fprintf(&b, "  - {name: r%d, match: {path: [\"~/proj%d/**\", \"*.secret%d\"]}, action: ask}\n", i, i, i)
		case 2:
			fmt.Fprintf(&b, "  - name: r%d\n    match: {kind: shell, command: \"*git*\"}\n    when: 'has_word(command, \"--flag%d\") || command.matches(\"x%d[0-9]+\")'\n    action: deny\n", i, i, i)
		case 3:
			fmt.Fprintf(&b, "  - {name: r%d, match: {tool: \"{Write,Edit}\", repo: \"~/src/r%d\"}, action: allow}\n", i, i)
		case 4:
			fmt.Fprintf(&b, "  - name: r%d\n    when: 'paths.exists(p, glob(p, \"**/gen%d/**\")) && hour >= 0'\n    action: ask\n", i, i)
		}
	}
	return b.String()
}

func benchInputs(te *testEnv) []Input {
	return []Input{
		{Agent: api.AgentClaude, Tool: "Bash", ToolKind: api.ToolShell, Command: "cd /Users/me/src/app && git push origin feature/new-thing --set-upstream", Cwd: te.repo, Repo: te.repo, Branch: "feature/x"},
		{Agent: api.AgentClaude, Tool: "Edit", ToolKind: api.ToolEdit, Paths: []string{"src/internal/server/handler.go"}, Input: json.RawMessage(`{"file_path":"src/internal/server/handler.go","old_string":"a","new_string":"b"}`), Cwd: te.repo, Repo: te.repo},
		{Agent: api.AgentCodex, Tool: "shell", ToolKind: api.ToolShell, Command: "go test ./... -run TestX -count=1", Cwd: te.repo, Repo: te.repo},
		{Agent: api.AgentClaude, Tool: "Read", ToolKind: api.ToolRead, Paths: []string{"/Users/me/src/app/README.md"}, Cwd: te.repo},
	}
}

func BenchmarkEvaluate50Rules(b *testing.B) {
	te := newTestEnv(b)
	writeFile(b, te.userFile(), benchPolicy(50))
	e, st := te.load(b)
	if len(st.Errors) != 0 {
		b.Fatal(st.Errors)
	}
	inputs := benchInputs(te)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		e.Evaluate(inputs[i%len(inputs)])
		i++
	}
}

func BenchmarkEvaluate50RulesShell(b *testing.B) {
	te := newTestEnv(b)
	writeFile(b, te.userFile(), benchPolicy(50))
	e, _ := te.load(b)
	in := benchInputs(te)[0]
	b.ReportAllocs()
	for b.Loop() {
		e.Evaluate(in)
	}
}

func BenchmarkEvaluateParallel(b *testing.B) {
	te := newTestEnv(b)
	writeFile(b, te.userFile(), benchPolicy(50))
	e, _ := te.load(b)
	inputs := benchInputs(te)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			e.Evaluate(inputs[i%len(inputs)])
			i++
		}
	})
}

func BenchmarkLoad50Rules(b *testing.B) {
	te := newTestEnv(b)
	writeFile(b, te.userFile(), benchPolicy(50))
	for b.Loop() {
		te.load(b)
	}
}

func TestEmptyPolicyFileKeepsRules(t *testing.T) {
	te := newTestEnv(t)
	good := "version: 1\nrules:\n  - {name: no-push, match: {command: \"git push*\"}, action: deny}\n"
	writeFile(t, te.userFile(), good)
	e, _ := te.load(t)

	// Truncated (an editor mid-save, or someone emptying the file): the
	// deny rule must survive and the status must say why.
	writeFile(t, te.userFile(), "")
	st := e.Reload()
	if r := e.Evaluate(shell("git push")); r.Action != api.BehaviorDeny {
		t.Fatalf("empty file dropped the rules: %+v", r)
	}
	if st.Files[1].Rules != 1 || !strings.Contains(st.Files[1].Error, "policy file is empty") ||
		len(st.Errors) != 2 || !strings.Contains(st.Errors[1], "last valid version") {
		t.Fatalf("status = %+v", st)
	}

	// The documented way to clear rules works.
	writeFile(t, te.userFile(), "version: 1\nrules: []\n")
	st = e.Reload()
	if r := e.Evaluate(shell("git push")); r.Action != "" || len(st.Errors) != 0 || st.Files[1].Rules != 0 {
		t.Fatalf("explicit empty rules: %+v / %+v", r, st)
	}

	// Deleting the file also clears them, without an error.
	writeFile(t, te.userFile(), good)
	e.Reload()
	os.Remove(te.userFile())
	if st := e.Reload(); len(st.Errors) != 0 || e.Evaluate(shell("git push")).Action != "" {
		t.Fatalf("deleted file: %+v", st)
	}
}

func TestEmptyPolicyFileAtFirstLoad(t *testing.T) {
	te := newTestEnv(t)
	writeFile(t, te.userFile(), "")
	_, st := te.load(t)
	if len(st.Errors) != 1 || !strings.Contains(st.Errors[0], "policy file is empty") || st.Rules != 1 {
		t.Fatalf("status = %+v", st)
	}
	// An empty repo policy is reported the same way.
	writeFile(t, te.repoFile(), "")
	e, _ := te.load(t)
	in := shell("ls")
	in.Repo = te.repo
	e.Evaluate(in)
	found := false
	for _, f := range e.Status().Files {
		if f.Scope == ScopeRepo && strings.Contains(f.Error, "policy file is empty") {
			found = true
		}
	}
	if !found {
		t.Fatalf("repo status = %+v", e.Status().Files)
	}
}

func TestMidWriteIsRetried(t *testing.T) {
	te := newTestEnv(t)
	good := "version: 1\nrules:\n  - {name: no-push, match: {command: \"git push*\"}, action: deny}\n"
	writeFile(t, te.userFile(), good)
	e, _ := te.load(t)
	clk := withClock(e)

	// A writer finishes while we read: the read returns the half-written
	// content (valid YAML, no rules), then the file changes under us.
	writes := 0
	readFile = func(name string) ([]byte, error) {
		data, err := os.ReadFile(name)
		if writes == 0 && name == te.userFile() {
			writes++
			writeFile(t, name, good+"  - {name: no-pull, match: {command: \"git pull*\"}, action: deny}\n")
		}
		return data, err
	}
	t.Cleanup(func() { readFile = os.ReadFile })

	writeFile(t, te.userFile(), "version: 1\n") // the "truncated" first half
	clk.advance(3 * time.Second)
	if r := e.Evaluate(shell("git push")); r.Action != api.BehaviorDeny {
		t.Fatalf("mid-write read must keep the previous rules: %+v", r)
	}
	if len(e.Status().Errors) != 0 {
		t.Fatalf("a mid-write read is not an error: %v", e.Status().Errors)
	}
	// No clock advance: the next call retries at once and sees the final file.
	if r := e.Evaluate(shell("git pull")); r.Action != api.BehaviorDeny {
		t.Fatalf("retry should load the finished file: %+v", r)
	}
	if r := e.Evaluate(shell("git push")); r.Action != api.BehaviorDeny {
		t.Fatalf("finished file: %+v", r)
	}
}

// TestShellPathsNeverAllow: a shell command's paths are the files an
// apply_patch heredoc in it names (M10). Deny and ask rules use them; an
// allow rule must not, since the command may do anything else as well.
func TestShellPathsNeverAllow(t *testing.T) {
	te := newTestEnv(t)
	writeFile(t, te.userFile(), `version: 1
rules:
  - {name: allow-src, match: {path: "src/**"}, action: allow}
  - {name: allow-when-src, match: {kind: shell}, when: 'paths.all(p, glob(p, "**/src/**"))', action: allow}
  - {name: no-env, match: {path: ".env*"}, action: deny}
`)
	e, _ := te.load(t)
	patch := func(cmd string, paths ...string) Input {
		in := shell(cmd)
		in.Cwd, in.Paths = te.repo, paths
		return in
	}
	r := e.Evaluate(patch("rm -rf / ; apply_patch <<'EOF'\n*** Begin Patch\n*** Update File: src/a.go\n*** End Patch\nEOF", "src/a.go"))
	if r.Action != "" || len(r.Matched) != 2 || r.Matched[0].Ignored == "" || r.Matched[1].Ignored == "" {
		t.Errorf("path allow on a shell command: %+v", r)
	}
	if r := e.Evaluate(patch("apply_patch <<'EOF' …", ".env.local")); r.Action != api.BehaviorDeny || r.Rule != "no-env" {
		t.Errorf("deny on shell paths: %+v", r)
	}
	// Edits (Codex's own apply_patch, or the exact heredoc form) still allow.
	in := patch("apply_patch <<'EOF' …", "src/a.go")
	in.ToolKind = api.ToolEdit
	if r := e.Evaluate(in); r.Action != api.BehaviorAllow || r.Rule != "allow-src" {
		t.Errorf("edit: %+v", r)
	}
	// A shell command without paths is unaffected.
	if r := e.Evaluate(patch("ls")); r.Action != api.BehaviorAllow || r.Rule != "allow-when-src" {
		t.Errorf("plain shell: %+v", r)
	}
}
