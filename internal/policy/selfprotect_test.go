package policy

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/xasdta/pilothouse/internal/api"
)

func TestSelfProtectionShell(t *testing.T) {
	te := newTestEnv(t)
	// An allow-everything user rule must not silence the built-in ask.
	writeFile(t, te.userFile(), "version: 1\nrules:\n  - {name: yolo, match: {tool: \"*\"}, action: allow}\n")
	e, _ := te.load(t)
	sock := filepath.Join(te.run, "pilothouse.sock")

	tests := []struct {
		cmd  string
		want bool
	}{
		// --- admin subcommands, however they are spelled -------------------
		{"pilothouse approve r_123", true},
		{"./pilothouse approve r_1", true},
		{"/usr/local/bin/pilothouse deny r_1", true},
		{"PILOTHOUSE approve r_1", true}, // case-insensitive filesystems
		{`"pilothouse" approve r_1`, true},
		{`pilot''house approve r_1`, true},
		{`p\ilothouse approve r_1`, true},
		{`$'\x70ilothouse' approve r_1`, true},
		{"pilothouse --verbose remote off", true},
		{"pilothouse pair", true},
		{"pilothouse devices revoke d_1", true},
		{"pilothouse service install", true},
		{"pilothouse hooks uninstall", true},
		{"pilothouse here", true},
		{"pilothouse away", true},
		{"pilothouse auto", true},
		{"pilothouse stop", true},
		{"pilothouse open", true},
		{"pilothouse setup", true},
		{"pilothouse policy init --force", true},
		{"pilothouse policy trust .", true},
		{"pilothouse policy untrust", true},
		{"pilothouse policy", true},
		// --- wrappers and chains --------------------------------------------
		{"env X=1 pilothouse approve r_1", true},
		{"env -i PATH=/bin pilothouse here", true},
		{"env -u FOO pilothouse away", true},
		{`env -S "pilothouse approve r_1"`, true},
		{"X=1 Y=2 pilothouse pair", true},
		{"sudo -u me pilothouse service install", true},
		{"nohup pilothouse stop &", true},
		{"timeout 5 pilothouse hooks uninstall", true},
		{"nice -n 10 pilothouse approve r_1", true},
		{"time pilothouse approve r_1", true},
		{"command pilothouse approve r_1", true},
		{"ls; pilothouse approve r_1", true},
		{"true && pilothouse away", true},
		{"false || pilothouse away", true},
		{"echo y | pilothouse approve r_1", true},
		{"(pilothouse approve r_1)", true},
		{"{ pilothouse approve r_1; }", true},
		{"if true; then pilothouse approve r_1; fi", true},
		{"for i in 1; do pilothouse approve r_$i; done", true},
		{"cd /tmp &&\npilothouse approve r_1", true},
		{"ls \\\n && pilothouse approve r_1", true},
		// --- nested shells and substitutions ---------------------------------
		{`sh -c "pilothouse approve r_1"`, true},
		{`bash -lc 'pilothouse policy trust .'`, true},
		{`zsh -c 'cd x; pilothouse deny r_1'`, true},
		{`bash -c "bash -c 'pilothouse approve r_1'"`, true},
		{`echo $(pilothouse approve r_1)`, true},
		{"echo `pilothouse approve r_1`", true},
		{`echo "$(pilothouse approve r_1)"`, true},
		{`cat <(pilothouse approve r_1)`, true},
		{`eval "pilothouse approve r_1"`, true},
		{`bash <<< "pilothouse approve r_1"`, true},
		{`echo "pilothouse approve r_1" | sh`, true},
		{`printf 'pilothouse approve r_1' | bash -s`, true},
		{`python3 -c "import os; os.system('pilothouse approve r_1')"`, true},
		{`osascript -e 'do shell script "pilothouse approve r_1"'`, true},
		{`echo r_1 | xargs pilothouse approve`, true},
		{`echo approve r_1 | xargs pilothouse`, true},
		{`find . -name x -exec pilothouse approve {} \;`, true},
		{"pilothouse run -- pilothouse approve r_1", true},
		{"pilothouse run -n x -- sh -c 'pilothouse deny r_1'", true},
		{`AG=pilothouse; $AG approve r_1`, true},
		{`busybox sh -c "pilothouse approve r_1"`, true},
		// --- writes to protected paths ----------------------------------------
		{"echo x > ~/.pilothouse/policy.yaml", true},
		{"echo x >> $HOME/.pilothouse/config.yaml", true},
		{"echo x >| ${HOME}/.pilothouse/config.yaml", true},
		{"cat x 2> ~/.pilothouse/x", true},
		{"echo x &> ~/.pilothouse/x", true},
		{"echo x>~/.pilothouse/x", true},
		{`sed -i '' s/a/b/ ~/.pilothouse/policy.yaml`, true},
		{`sed -i.bak s/a/b/ ~/.pilothouse/policy.yaml`, true},
		{`sed -ni s/a/b/p ~/.pilothouse/policy.yaml`, true},
		{"echo x >& ~/.pilothouse/x", true},
		{"tee ~/.claude/settings.json < x", true},
		{"tee -a ~/.codex/config.toml", true},
		{"cp /tmp/x ~/.pilothouse/trusted-repos.yaml", true},
		{"mv x .claude/settings.local.json", true}, // relative to the repo (cwd)
		{"rm .pilothouse/policy.yaml", true},       // the repo's own policy
		{"cd ~/.pilothouse && rm policy.yaml", true},
		{"cd && rm -rf .pilothouse", true},
		{`rm -rf "$PILOTHOUSE_HOME"`, true},
		{`sqlite3 ~/.pilothouse/pilothouse.db "update requests set status='decided'"`, true},
		{"curl --unix-socket " + sock + " -X POST http://x/v1/requests/r_1/decide", true},
		{"sort -o ~/.pilothouse/policy.yaml x", true},
		{"find ~/.pilothouse -name '*.yaml' -delete", true},
		{"dd if=/dev/zero of=" + te.agHome + "/pilothouse.db", true},
		{"ln -sf /tmp/evil ~/.pilothouse/policy.yaml", true},
		{"chmod 777 ~/.pilothouse", true},
		{`python3 -c "open('` + te.agHome + `/policy.yaml','w').write('')"`, true},
		{`node -e "require('fs').writeFileSync(process.env.HOME+'/.pilothouse/policy.yaml','')"`, false}, // computed path: documented limit
		{"vim ~/.pilothouse/policy.yaml", true},
		{"yq -i '.rules = []' ~/.pilothouse/policy.yaml", true},

		// --- must NOT trigger --------------------------------------------------
		// Text that merely mentions pilothouse is not an invocation (decision
		// documented in POLICY.md); it is only scanned when fed to an interpreter.
		{"echo pilothouse approve r_1", false},
		{`echo "pilothouse approve r_1" > notes.txt`, false},
		{`git commit -m "pilothouse approve: fix race"`, false},
		{`grep -rn "pilothouse approve" docs/`, false},
		{"pilothouse ls", false},
		{"pilothouse log --grep approve", false},
		{"pilothouse status", false},
		{"pilothouse explain s_1", false},
		{"pilothouse", false},
		{"pilothouse version", false},
		{"pilothouse policy test --command 'pilothouse approve r_1'", false},
		{"pilothouse policy check", false},
		{"pilothouse policy status", false},
		{"pilothouse run -- claude", false},
		{"pilothouse-dev approve r_1", false},
		{"mypilothouse approve r_1", false},
		{"cat ~/.pilothouse/policy.yaml", false},
		{"ls -la ~/.pilothouse", false},
		{"grep approve ~/.pilothouse/logs/daemon.log", false},
		{"sed s/a/b/ ~/.pilothouse/policy.yaml", false},
		{"find ~/.pilothouse -name '*.yaml'", false},
		{"go test ./...", false},
		{"rm -rf node_modules", false},
		{"echo hi > /tmp/out.txt", false},
		{"cp ~/.pilothousex/a /tmp/b", false}, // prefix of a protected path is not inside it
		{"echo 2>&1", false},
		{"cat ~/.pilothouse/policy.yaml 2>&1 | head", false},
		{"sed -n 1,5p ~/.pilothouse/policy.yaml", false},
		{"sort -u ~/.pilothouse/x", false},
		{"bash scripts/build.sh", false},
		{"python3 -c 'print(1)'", false},
		{"", false},
	}
	for _, tt := range tests {
		in := shell(tt.cmd)
		in.Cwd, in.Repo = te.repo, te.repo
		r := e.Evaluate(in)
		got := r.Rule == SelfProtectionRule
		if got != tt.want {
			t.Errorf("%q: self-protection=%v, want %v (%s)", tt.cmd, got, tt.want, r.Message)
			continue
		}
		if got && (r.Action != api.BehaviorAsk || r.File != BuiltinFile || !strings.HasPrefix(r.Message, "Pilothouse self-protection: this ")) {
			t.Errorf("%q: result %+v", tt.cmd, r)
		}
		if !got && r.Action != api.BehaviorAllow && tt.cmd != "" {
			t.Errorf("%q: the allow-all rule should decide, got %+v", tt.cmd, r)
		}
	}
}

func TestSelfProtectionDenyStillWins(t *testing.T) {
	te := newTestEnv(t)
	writeFile(t, te.userFile(), "version: 1\nrules:\n  - {name: no-pilothouse, match: {command: \"*pilothouse*\"}, action: deny}\n")
	e, _ := te.load(t)
	r := e.Evaluate(shell("pilothouse approve r_1"))
	if r.Action != api.BehaviorDeny || r.Rule != "no-pilothouse" || len(r.Matched) != 2 {
		t.Fatalf("%+v", r)
	}
}

func TestSelfProtectionEditTools(t *testing.T) {
	te := newTestEnv(t)
	e, _ := te.load(t)
	link := filepath.Join(te.home, "innocent")
	if err := os.Symlink(te.agHome, link); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		kind  api.ToolKind
		paths []string
		cwd   string
		want  bool
	}{
		{"policy file", api.ToolEdit, []string{"~/.pilothouse/policy.yaml"}, "", true},
		{"absolute", api.ToolEdit, []string{te.agHome + "/config.yaml"}, "", true},
		{"agent settings", api.ToolEdit, []string{te.home + "/.claude/settings.json"}, "", true},
		{"codex config", api.ToolEdit, []string{"~/.codex/config.toml"}, "", true},
		{"repo policy relative", api.ToolEdit, []string{".pilothouse/policy.yaml"}, te.repo, true},
		{"project claude settings", api.ToolEdit, []string{".claude/settings.local.json"}, te.repo, true},
		{"dot-dot escape", api.ToolEdit, []string{"../../.pilothouse/policy.yaml"}, te.repo, true},
		{"via symlink", api.ToolEdit, []string{link + "/policy.yaml"}, "", true},
		{"new file via symlinked dir", api.ToolEdit, []string{link + "/new.yaml"}, "", true},
		{"one of many", api.ToolEdit, []string{"/tmp/a", "~/.pilothouse/x"}, "", true},
		{"mcp writer", api.ToolMCP, []string{"~/.pilothouse/x"}, "", true},
		{"other tool", api.ToolOther, []string{"~/.pilothouse/x"}, "", true},
		{"read is fine", api.ToolRead, []string{"~/.pilothouse/policy.yaml"}, "", false},
		{"web is fine", api.ToolWeb, []string{"~/.pilothouse/policy.yaml"}, "", false},
		{"source file", api.ToolEdit, []string{"src/main.go"}, te.repo, false},
		{"claude dir other file", api.ToolEdit, []string{".claude/commands/x.md"}, te.repo, false},
		{"lookalike", api.ToolEdit, []string{"~/.pilothouse-notes.md"}, "", false},
	}
	for _, tt := range tests {
		r := e.Evaluate(Input{Agent: api.AgentClaude, Tool: "Edit", ToolKind: tt.kind, Paths: tt.paths, Cwd: tt.cwd, Repo: tt.cwd})
		if got := r.Rule == SelfProtectionRule; got != tt.want {
			t.Errorf("%s: got %v want %v (%+v)", tt.name, got, tt.want, r)
		}
	}
	if runtime.GOOS == "darwin" {
		r := e.Evaluate(Input{ToolKind: api.ToolEdit, Paths: []string{"~/.PILOTHOUSE/policy.yaml"}})
		if r.Rule != SelfProtectionRule {
			t.Error("case-insensitive filesystem: ~/.PILOTHOUSE must be protected")
		}
	}
}

func TestParseShell(t *testing.T) {
	p := parseShell(`A=1 echo "a b" 'c d' e\ f >out 2>>err | grep -v "$(id)" && x <<< 'here'; y &`)
	if len(p.cmds) != 4 {
		t.Fatalf("cmds = %+v", p.cmds)
	}
	c0 := p.cmds[0]
	if strings.Join(c0.words, "|") != "A=1|echo|a b|c d|e f" {
		t.Errorf("words = %q", c0.words)
	}
	if len(c0.redirs) != 2 || c0.redirs[0] != (shRedir{">", "out"}) || c0.redirs[1] != (shRedir{">>", "err"}) {
		t.Errorf("redirs = %+v", c0.redirs)
	}
	if !p.cmds[1].pipedIn || p.cmds[2].pipedIn {
		t.Errorf("pipes: %+v", p.cmds)
	}
	if p.cmds[2].redirs[0] != (shRedir{"<<<", "here"}) {
		t.Errorf("here-string: %+v", p.cmds[2].redirs)
	}
	if len(p.subs) != 1 || p.subs[0] != "id" {
		t.Errorf("subs = %q", p.subs)
	}
	// Unterminated constructs must not panic or loop.
	for _, s := range []string{`"abc`, `'abc`, "`abc", "$(abc", "$'abc", `\`, "<(x", "a >", "$'\\x", "$'\\1"} {
		parseShell(s)
	}
}

func FuzzSelfProtection(f *testing.F) {
	for _, s := range []string{"pilothouse approve r_1", `sh -c "x"`, "echo $(a `b` <(c))", "a > b", `$'\x41'`} {
		f.Add(s)
	}
	te := newTestEnv(f)
	e, _ := te.load(f)
	f.Fuzz(func(t *testing.T, cmd string) {
		e.Evaluate(shell(cmd)) // must not panic or hang
	})
}

func BenchmarkSelfProtectionShell(b *testing.B) {
	te := newTestEnv(b)
	e, _ := te.load(b)
	in := shell(`cd /Users/me/src/app && git add -A && git commit -m "fix: handle empty input" && git push origin feature/x 2>&1 | tail -5`)
	b.ReportAllocs()
	for b.Loop() {
		e.Evaluate(in)
	}
}

// Adapters protect project settings in every repo with "**/" globs.
func TestSelfProtectionGlobEntries(t *testing.T) {
	te := newTestEnv(t)
	opts := DefaultOptions(
		filepath.Join(te.home, ".claude", "settings.json"),
		"**/.claude/settings.json", "**/.claude/settings.local.json",
		"**/.codex/config.toml", "**/.codex/hooks.json",
		"~/.secrets-dir/**/*.key", // a home-anchored glob
		"**/.guarded",             // covers everything beneath it too
	)
	e, _ := Load(opts)
	cwd := "/work/repo"
	edit := func(p string) Input {
		return Input{Agent: api.AgentClaude, Tool: "Write", ToolKind: api.ToolEdit, Paths: []string{p}, Cwd: cwd, Repo: cwd}
	}
	sh := func(cmd string) Input {
		in := shell(cmd)
		in.Cwd, in.Repo = cwd, cwd
		return in
	}
	tests := []struct {
		name string
		in   Input
		want bool
	}{
		{"other repo local settings", edit("/work/other/.claude/settings.local.json"), true},
		{"other repo settings", edit("/work/other/.claude/settings.json"), true},
		{"relative into sibling repo", edit("../other/.claude/settings.local.json"), true},
		{"codex hooks anywhere", edit("/x/y/.codex/hooks.json"), true},
		{"home-anchored glob", edit("~/.secrets-dir/a/b/server.key"), true},
		{"under a glob match", edit("/a/b/.guarded/deep/file"), true},
		{"glob match itself", edit("/a/b/.guarded"), true},
		{"plain settings.json", edit("/work/repo/src/settings.json"), false},
		{"claude dir other file", edit("/work/other/.claude/commands/x.md"), false},
		{"lookalike suffix", edit("/work/other/x.claude/settings.json"), false},
		{"home-anchored glob elsewhere", edit("/tmp/.secrets-dir/a/server.key"), false},
		{"guarded lookalike", edit("/a/.guardedx/f"), false},
		{"redirect", sh("echo x > /work/other/.claude/settings.json"), true},
		{"sed -i relative", sh("sed -i s/a/b/ ../other/.claude/settings.local.json"), true},
		{"tee", sh("tee /w/.codex/hooks.json < x"), true},
		{"cp", sh("cp /tmp/x /w/.claude/settings.json"), true},
		{"mv", sh("mv x ../other/.codex/config.toml"), true},
		{"cd then write", sh("cd /work/other/.claude && echo {} > settings.local.json"), true},
		{"interpreter relative", sh(`python3 -c "open('../other/.claude/settings.local.json','w')"`), true},
		{"interpreter absolute", sh(`node -e "fs.writeFileSync('/w/.codex/hooks.json','')"`), true},
		{"interpreter unrelated", sh(`python3 -c "open('src/settings.json','w')"`), false},
		{"reader", sh("cat /w/.claude/settings.json"), false},
		{"write unrelated settings", sh("echo {} > src/settings.json"), false},
	}
	for _, tt := range tests {
		r := e.Evaluate(tt.in)
		if got := r.Rule == SelfProtectionRule; got != tt.want {
			t.Errorf("%s: protected=%v, want %v (%s)", tt.name, got, tt.want, r.Message)
		}
	}
	if runtime.GOOS == "darwin" {
		if r := e.Evaluate(edit("/work/other/.CLAUDE/Settings.json")); r.Rule != SelfProtectionRule {
			t.Error("case-insensitive filesystem: glob entries must match case-insensitively")
		}
	}
}

func TestProtectedSetHas(t *testing.T) {
	g, _ := compileGlob("**/.claude/settings.json", globPath)
	s := &protectedSet{roots: []string{"/home/u/.pilothouse"}, globs: []*glob{g}}
	for p, want := range map[string]bool{
		"/home/u/.pilothouse":             true,
		"/home/u/.pilothouse/policy.yaml": true,
		"/home/u/.pilothousex":            false,
		"/r/.claude/settings.json":        true,
		"/r/.claude/settings.json/x":      true,
		"/r/.claude/settings.jsonx":       false,
		"/r/src/settings.json":            false,
		"/":                               false,
	} {
		if got := s.has(p); got != want {
			t.Errorf("has(%q) = %v, want %v", p, got, want)
		}
	}
	var nilSet *protectedSet
	if nilSet.has("/x") {
		t.Error("nil set protects nothing")
	}
}

func BenchmarkSelfProtectionEditGlobs(b *testing.B) {
	te := newTestEnv(b)
	e, _ := Load(DefaultOptions("**/.claude/settings.json", "**/.claude/settings.local.json", "**/.codex/config.toml", "**/.codex/hooks.json"))
	in := Input{Tool: "Edit", ToolKind: api.ToolEdit, Paths: []string{te.repo + "/internal/server/handlers/user_profile.go"}, Cwd: te.repo, Repo: te.repo}
	b.ReportAllocs()
	for b.Loop() {
		e.Evaluate(in)
	}
}

// TestSelfProtectionTaskWorktrees (H2): agents of tasks whose worktree is
// in the Pilothouse home (tasks made before worktrees moved out) must be
// able to work there without a prompt per call; Pilothouse's own files
// and the worktree's agent settings stay protected.
func TestSelfProtectionTaskWorktrees(t *testing.T) {
	te := newTestEnv(t)
	e, _ := te.load(t)
	wt := filepath.Join(te.agHome, "worktrees", "app", "fix-login")
	newWT := filepath.Join(te.home, ".pilothouse-worktrees", "app", "fix-login")
	in := func(dir string, i Input) Input {
		i.Agent, i.Cwd, i.Repo = api.AgentClaude, dir, dir
		return i
	}
	sh := func(dir, cmd string) Input { return in(dir, shell(cmd)) }
	edit := func(dir, p string) Input {
		return in(dir, Input{Tool: "Edit", ToolKind: api.ToolEdit, Paths: []string{p}})
	}
	for _, dir := range []string{wt, newWT} {
		for _, tt := range []struct {
			name string
			in   Input
			want bool
		}{
			{"git status", sh(dir, "git status"), false},
			{"git commit", sh(dir, "git add -A && git commit -m wip"), false},
			{"go test", sh(dir, "go test ./..."), false},
			{"write a file", sh(dir, "echo hi > main.go"), false},
			{"edit a file", edit(dir, filepath.Join(dir, "main.go")), false},
			{"edit relative", edit(dir, "src/a.go"), false},
			{"worktree's claude settings", edit(dir, filepath.Join(dir, ".claude", "settings.json")), true},
			{"worktree's repo policy", sh(dir, "rm .pilothouse/policy.yaml"), true},
			{"pilothouse config from the worktree", sh(dir, "rm "+filepath.Join(te.agHome, "config.yaml")), true},
			{"climb out", sh(dir, "cd ../../.. && rm policy.yaml"), dir == wt},
			{"pilothouse approve", sh(dir, "pilothouse approve r_1"), true},
		} {
			r := e.Evaluate(tt.in)
			if got := r.Rule == SelfProtectionRule; got != tt.want {
				t.Errorf("%s in %s: protected=%v, want %v (%s)", tt.name, tildePath(dir, te.home), got, tt.want, r.Message)
			}
		}
	}
}

// TestSelfProtectionReasonNamesThePath: the reason says which protected
// path a call touches and what it holds, instead of treating a
// subcommand as a file name ("runs git on …/status").
func TestSelfProtectionReasonNamesThePath(t *testing.T) {
	te := newTestEnv(t)
	e, _ := te.load(t)
	keys := filepath.Join(te.agHome, "keys")
	for _, tt := range []struct {
		in   Input
		want string
	}{
		{Input{Agent: api.AgentClaude, Tool: "Bash", ToolKind: api.ToolShell, Command: "git status", Cwd: keys},
			"Pilothouse self-protection: this runs `git status` in ~/.pilothouse/keys, inside ~/.pilothouse (Pilothouse's own settings, keys and database). A person must confirm it."},
		{shell("rm ~/.pilothouse/policy.yaml"),
			"Pilothouse self-protection: this runs rm on ~/.pilothouse/policy.yaml, inside ~/.pilothouse (Pilothouse's own settings, keys and database). A person must confirm it."},
		{shell("echo x > ~/.claude/settings.json"),
			"Pilothouse self-protection: this writes to ~/.claude/settings.json (Claude Code settings, which can approve tool calls). A person must confirm it."},
		{Input{Tool: "Write", ToolKind: api.ToolEdit, Paths: []string{"~/.pilothouse"}},
			"Pilothouse self-protection: this edits ~/.pilothouse (Pilothouse's own settings, keys and database). A person must confirm it."},
		{Input{Tool: "Write", ToolKind: api.ToolEdit, Paths: []string{"/r/.codex/config.toml"}, Cwd: "/r", Repo: "/r"},
			"Pilothouse self-protection: this edits /r/.codex/config.toml (Codex settings, which can approve tool calls). A person must confirm it."},
	} {
		if r := e.Evaluate(tt.in); r.Message != tt.want {
			t.Errorf("%q:\n got %s\nwant %s", tt.in.Command+strings.Join(tt.in.Paths, ","), r.Message, tt.want)
		}
	}
}

func TestProtectedSetExcept(t *testing.T) {
	s := &protectedSet{
		roots:  []string{"/h/.pilothouse", "/h/.pilothouse/worktrees/r/s/.claude/settings.json"},
		except: []string{"/h/.pilothouse/worktrees"},
	}
	for p, want := range map[string]bool{
		"/h/.pilothouse/config.yaml":                         true,
		"/h/.pilothouse/worktrees":                           false,
		"/h/.pilothouse/worktrees/r/s/main.go":               false,
		"/h/.pilothouse/worktrees/r/s/.claude/settings.json": true,
		"/h/.pilothouse/worktreesx":                          true,
	} {
		if got := s.has(p); got != want {
			t.Errorf("has(%q) = %v, want %v", p, got, want)
		}
	}
	// A carve-out can never open a root that lies inside it.
	s = &protectedSet{roots: []string{"/h/.pilothouse"}, except: []string{"/"}}
	if !s.has("/h/.pilothouse/keys") {
		t.Error("carve-out above a root opened it")
	}
}
