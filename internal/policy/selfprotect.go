package policy

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/xasdta/pilothouse/internal/api"
)

// SelfProtectionRule is the name of the built-in rule that makes a person
// confirm any attempt by an agent to administer Pilothouse or edit the files
// that configure Pilothouse and the agents. It is evaluated before every
// other rule, cannot be disabled, and — because ask outranks allow — no
// allow rule or session grant can silence it. Only a deny rule can make
// the outcome stricter.
const SelfProtectionRule = "pilothouse-self-protection"

// adminCommands are pilothouse subcommands that approve requests, change
// rules, routing, pairing or hooks. Reaching any of these from inside an
// agent is how an agent would approve itself.
var adminCommands = map[string]bool{
	"approve": true, "deny": true, "policy": true, "pair": true, "devices": true,
	"remote": true, "service": true, "hooks": true, "here": true, "away": true,
	"auto": true, "stop": true, "open": true, "setup": true,
}

// readOnlyPolicy are `pilothouse policy` subcommands that only read: an agent
// helping someone write rules may run them freely.
var readOnlyPolicy = map[string]bool{"check": true, "test": true, "status": true, "replay": true, "help": true}

// otherCommands are the remaining pilothouse subcommands; seeing one ends the
// search for a subcommand (so `pilothouse log --grep approve` is not a hit).
var otherCommands = map[string]bool{
	"ls": true, "log": true, "explain": true, "version": true, "status": true, "doctor": true,
	"help": true, "completion": true, "hook": true, "attach": true, "task": true, "daemon": true,
	"start": true, "notify": true, "inbox": true, "__holder": true,
}

// Command wrappers that run their arguments as a command. valueFlags are
// options that consume the next word.
var wrappers = map[string]map[string]bool{
	"sudo":       {"-u": true, "-g": true, "-h": true, "-p": true, "-C": true, "-D": true, "-r": true, "-t": true, "-U": true, "-T": true},
	"doas":       {"-u": true, "-C": true},
	"nohup":      nil,
	"time":       nil,
	"command":    nil,
	"builtin":    nil,
	"exec":       {"-a": true},
	"nice":       {"-n": true},
	"ionice":     {"-c": true, "-n": true, "-p": true},
	"stdbuf":     nil,
	"caffeinate": {"-t": true, "-w": true},
	"chronic":    nil,
	"unbuffer":   nil,
	"setsid":     nil,
	"timeout":    {"-s": true, "-k": true, "--signal": true, "--kill-after": true},
	"gtimeout":   {"-s": true, "-k": true},
	"watch":      {"-n": true},
	"xargs":      {"-I": true, "-n": true, "-P": true, "-L": true, "-s": true, "-d": true, "-E": true, "-a": true},
	"busybox":    nil,
	"then":       nil, "do": nil, "else": nil, "elif": nil, "if": nil, "while": nil, "until": nil, "!": nil,
}

var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "mksh": true, "fish": true, "ash": true, "yash": true, "tcsh": true, "csh": true}

// interpreters can run code given inline (-c/-e); we can't follow that
// code, so their presence switches on the conservative whole-line scan.
var interpreters = map[string]bool{
	"python": true, "python2": true, "python3": true, "perl": true, "ruby": true, "node": true,
	"deno": true, "bun": true, "php": true, "osascript": true, "lua": true, "tclsh": true,
	"awk": true, "gawk": true, "pwsh": true, "powershell": true, "source": true, ".": true,
}

// readers only read their arguments, so naming a protected file is fine
// (`cat ~/.pilothouse/config.yaml`). Some can write with a flag; those flags
// are listed in readerWriteFlags and turn the reader into a writer.
var readers = map[string]bool{
	"cat": true, "less": true, "more": true, "head": true, "tail": true, "ls": true, "stat": true,
	"file": true, "wc": true, "grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true,
	"ack": true, "diff": true, "cmp": true, "md5": true, "md5sum": true, "sha1sum": true,
	"sha256sum": true, "shasum": true, "b2sum": true, "cksum": true, "bat": true, "tree": true,
	"du": true, "df": true, "realpath": true, "readlink": true, "basename": true, "dirname": true,
	"echo": true, "printf": true, "test": true, "[": true, "jq": true, "yq": true, "sed": true,
	"sort": true, "find": true, "xxd": true, "hexdump": true, "od": true, "strings": true,
	"nl": true, "column": true, "uniq": true, "cut": true, "tr": true, "fd": true, "exa": true,
	"eza": true, "lsof": true,
}

var readerWriteFlags = map[string][]string{
	"sed":  {"-i", "--in-place", "-I"},
	"yq":   {"-i", "--inplace"},
	"sort": {"-o", "--output"},
	"find": {"-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprint0", "-fprintf", "-fls"},
}

// spAnalyzer inspects one tool call for the self-protection rule.
type spAnalyzer struct {
	home           string
	pilothouseHome string
	roots          *protectedSet
	cwd            string // the call's working directory, for the loose scan
	raw            string
	indirect       bool   // the line feeds text to an interpreter
	reason         string // first finding, human readable
}

const maxShellDepth = 8

func (a *spAnalyzer) hit(format string, args ...any) {
	if a.reason == "" {
		a.reason = fmt.Sprintf(format, args...)
	}
}

// selfProtection is the check function of the built-in rule.
func selfProtection(c *evalCtx) (bool, string) {
	kind := c.in.ToolKind
	if kind == api.ToolRead || kind == api.ToolWeb {
		return false, ""
	}
	a := &spAnalyzer{home: c.home, pilothouseHome: c.eng.pilothouseHome, raw: c.in.Command}
	if c.in.Command != "" {
		a.roots, a.cwd = c.protectedRoots(), c.cwd
		a.analyze(c.in.Command, c.cwd, 0)
		if a.reason == "" && a.indirect {
			a.looseScan()
		}
	}
	if a.reason == "" && kind != api.ToolShell {
		for _, p := range c.absPaths() {
			if c.isProtected(p) {
				if a.roots == nil {
					a.roots = c.protectedRoots()
				}
				a.hit("edits %s", a.describe(p))
				break
			}
		}
	}
	if a.reason == "" {
		return false, ""
	}
	return true, "Pilothouse self-protection: this " + a.reason + ". A person must confirm it."
}

func (a *spAnalyzer) analyze(s, cwd string, depth int) {
	if depth > maxShellDepth || a.reason != "" {
		return
	}
	p := parseShell(s)
	for i := range p.cmds {
		a.command(&p.cmds[i], &cwd, depth)
		if a.reason != "" {
			return
		}
	}
	for _, sub := range p.subs {
		a.analyze(sub, cwd, depth+1)
	}
}

// command analyses one simple command. cwd tracks `cd` so that
// `cd ~/.pilothouse && rm policy.yaml` resolves correctly.
func (a *spAnalyzer) command(cmd *shCmd, cwd *string, depth int) {
	for _, r := range cmd.redirs {
		if !strings.ContainsRune(r.op, '>') {
			continue
		}
		if (r.op == ">&" || r.op == "<&") && (isDigits(r.target) || r.target == "-") {
			continue // fd duplication (2>&1), not a file
		}
		if p, ok := a.protectedArg(r.target, *cwd); ok {
			a.hit("writes to %s", a.describe(p))
			return
		}
	}
	words := cmd.words
	viaXargs := false
	i := 0
	for i < len(words) {
		w := words[i]
		if isAssignment(w) {
			i++
			continue
		}
		base := cmdBase(w)
		if vf, ok := wrappers[base]; ok {
			viaXargs = viaXargs || base == "xargs"
			i = skipOptions(words, i+1, vf)
			if (base == "timeout" || base == "gtimeout") && i < len(words) {
				i++ // the duration
			}
			continue
		}
		if base == "env" {
			i = a.skipEnv(words, i+1, *cwd, depth)
			continue
		}
		break
	}
	if i >= len(words) {
		return
	}
	w, args := words[i], words[i+1:]
	base := cmdBase(w)
	switch {
	case base == "pilothouse":
		a.pilothouse(args, viaXargs, *cwd, depth)
		return
	case shells[base]:
		a.shell(cmd, args, *cwd, depth)
		return
	case base == "eval":
		a.analyze(strings.Join(args, " "), *cwd, depth+1)
		return
	case base == "cd" || base == "pushd":
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			*cwd = absPath(args[0], *cwd, a.home)
		} else if len(args) == 0 {
			*cwd = a.home
		}
		return
	case base == "find":
		a.findExec(args, *cwd, depth)
	case interpreters[base]:
		a.indirect = true
	case strings.ContainsRune(w, '$'):
		// The program is computed at run time ("$AG approve"); if the line
		// mentions pilothouse at all, fall back to the whole-line scan.
		if strings.Contains(strings.ToLower(a.raw), "pilothouse") {
			if sub := firstNonFlag(args); adminCommands[sub] {
				a.hit("runs a computed command (%s %s) on a line that mentions pilothouse", w, sub)
				return
			}
			a.indirect = true
		}
	}
	if readers[base] && !hasAnyFlag(args, readerWriteFlags[base]) {
		return
	}
	for _, arg := range args {
		if p, ok := a.protectedArg(arg, *cwd); ok {
			if isBareWord(arg) {
				// `git status` in a protected directory: "status" only
				// resolves to a protected path because of where it runs.
				a.hit("runs `%s %s` in %s", base, arg, a.describe(filepath.Dir(p)))
			} else {
				a.hit("runs %s on %s", base, a.describe(p))
			}
			return
		}
	}
}

// isBareWord: an argument that names no directory (no "/", not ".", "..",
// "~"), so it only points into a protected directory through the cwd.
func isBareWord(arg string) bool {
	return !strings.ContainsRune(arg, '/') && arg != "." && arg != ".." && !strings.HasPrefix(arg, "~") && !strings.HasPrefix(arg, "$")
}

// describe names a protected path for a person: the path, the protected
// root it is in, and what that root holds.
func (a *spAnalyzer) describe(p string) string {
	what := protectedKind(p, a.pilothouseHome)
	root := a.roots.root(foldPath(p))
	if root == "" || root == foldPath(p) {
		return tildePath(p, a.home) + " (" + what + ")"
	}
	shown := root
	if len(p) >= len(root) && strings.EqualFold(p[:len(root)], root) {
		shown = p[:len(root)] // the person's spelling, not the folded one
	}
	return tildePath(p, a.home) + ", inside " + tildePath(shown, a.home) + " (" + protectedKind(shown, a.pilothouseHome) + ")"
}

// protectedKind says what a protected path holds.
func protectedKind(p, pilothouseHome string) string {
	fp := foldPath(p)
	if pilothouseHome != "" {
		home := foldPath(filepath.Clean(pilothouseHome))
		if fp == home || strings.HasPrefix(fp, home+"/") {
			if rest := strings.TrimPrefix(fp, home); rest == "/run" || strings.HasPrefix(rest, "/run/") {
				return "Pilothouse's sockets"
			}
			return "Pilothouse's own settings, keys and database"
		}
	}
	switch {
	case strings.Contains(fp, "/.claude/") || strings.HasSuffix(fp, "/.claude"):
		return "Claude Code settings, which can approve tool calls"
	case strings.Contains(fp, "/.codex/") || strings.HasSuffix(fp, "/.codex"):
		return "Codex settings, which can approve tool calls"
	case strings.Contains(fp, "/.pilothouse/") || strings.HasSuffix(fp, "/.pilothouse"):
		return "the repo's Pilothouse policy"
	}
	return "Pilothouse or agent settings"
}

// pilothouse checks a pilothouse invocation for an admin subcommand.
func (a *spAnalyzer) pilothouse(args []string, viaXargs bool, cwd string, depth int) {
	unknown := 0
	for j := 0; j < len(args); j++ {
		w := args[j]
		if w == "--" || strings.HasPrefix(w, "-") {
			continue
		}
		if adminCommands[w] {
			if w == "policy" {
				if sub := firstNonFlag(args[j+1:]); readOnlyPolicy[sub] {
					return
				}
			}
			a.hit("runs `pilothouse %s`", w)
			return
		}
		if w == "run" {
			// `pilothouse run [-n name] [-d] [--task id] [--] cmd…`
			rest := skipOptions(args, j+1, map[string]bool{"-n": true, "--name": true, "--task": true})
			a.analyze(shellJoin(args[rest:]), cwd, depth+1)
			return
		}
		if otherCommands[w] {
			return
		}
		// Possibly the value of a global flag: look one word further.
		if unknown++; unknown > 1 {
			return
		}
	}
	if viaXargs {
		// `… | xargs pilothouse`: the subcommand arrives on stdin.
		a.hit("runs pilothouse with arguments supplied by xargs")
	}
}

// shell handles sh/bash/zsh…: -c scripts and here-strings are analysed,
// stdin-fed shells switch on the whole-line scan.
func (a *spAnalyzer) shell(cmd *shCmd, args []string, cwd string, depth int) {
	hasC := false
	for j := 0; j < len(args); j++ {
		w := args[j]
		switch {
		case w == "--":
			continue
		case (w == "-o" || w == "+o") && j+1 < len(args):
			j++
		case strings.HasPrefix(w, "--"):
			continue
		case strings.HasPrefix(w, "-") || strings.HasPrefix(w, "+"):
			if strings.ContainsRune(w[1:], 'c') {
				hasC = true
			}
		default:
			if hasC {
				a.analyze(w, cwd, depth+1)
			}
			return // a script file: not followed
		}
	}
	for _, r := range cmd.redirs {
		switch r.op {
		case "<<<":
			a.analyze(r.target, cwd, depth+1)
			return
		case "<", "<<", "<<-":
			a.indirect = true
		}
	}
	if cmd.pipedIn {
		a.indirect = true // `… | sh`: runs whatever came before as code
	}
}

// skipEnv skips `env [-i] [-u NAME] [-C dir] [NAME=value]…`. `env -S
// "cmd args"` splits a string into a command, so that string is analysed.
func (a *spAnalyzer) skipEnv(words []string, i int, cwd string, depth int) int {
	for i < len(words) {
		w := words[i]
		switch {
		case w == "-u" || w == "-C" || w == "--unset" || w == "--chdir":
			i += 2
		case w == "-S" || w == "--split-string":
			if i+1 < len(words) {
				a.analyze(words[i+1], cwd, depth+1)
			}
			i += 2
		case strings.HasPrefix(w, "-S") && len(w) > 2:
			a.analyze(w[2:], cwd, depth+1)
			i++
		case strings.HasPrefix(w, "-") || isAssignment(w):
			i++
		default:
			return i
		}
	}
	return i
}

// findExec analyses `find … -exec cmd {} ;` command templates.
func (a *spAnalyzer) findExec(args []string, cwd string, depth int) {
	for j := 0; j < len(args); j++ {
		switch args[j] {
		case "-exec", "-execdir", "-ok", "-okdir":
			k := j + 1
			for k < len(args) && args[k] != ";" && args[k] != "+" {
				k++
			}
			a.analyze(shellJoin(args[j+1:k]), cwd, depth+1)
			j = k
		}
	}
}

// looseScan is the conservative fallback for lines that feed text to an
// interpreter (`| sh`, `python -c`, `osascript -e`): it looks for an
// pilothouse admin invocation or a protected path anywhere in the text,
// quoted or not.
func (a *spAnalyzer) looseScan() {
	norm := strings.Map(func(r rune) rune {
		if r == '\'' || r == '"' || r == '\\' {
			return -1
		}
		return r
	}, a.raw)
	fields := strings.FieldsFunc(norm, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(";&|()<>`$={},[]", r)
	})
	for i, f := range fields {
		if cmdBase(f) != "pilothouse" {
			continue
		}
		for j := i + 1; j < len(fields) && j <= i+3; j++ {
			g := fields[j]
			if strings.HasPrefix(g, "-") {
				continue
			}
			if adminCommands[g] && !(g == "policy" && j+1 < len(fields) && readOnlyPolicy[fields[j+1]]) {
				a.hit("passes `pilothouse %s` to an interpreter", g)
				return
			}
			break
		}
	}
	folded := foldPath(norm)
	for _, form := range a.rootForms() {
		if strings.Contains(folded, form) {
			a.hit("passes a protected path (%s) to an interpreter", form)
			return
		}
	}
	// Glob entries can't be found by substring: resolve every path-like
	// token of the text and test it ("open('.claude/settings.json','w')").
	if len(a.roots.globs) == 0 {
		return
	}
	tokens := strings.FieldsFunc(norm, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(";&|()<>`={},[]+", r)
	})
	for _, tok := range tokens {
		if !strings.ContainsRune(tok, '/') && !strings.HasPrefix(tok, ".") {
			continue
		}
		p := absPath(a.expandVars(tok), a.cwd, a.home)
		if a.roots.has(foldPath(p)) {
			a.hit("passes a protected path (%s) to an interpreter", tildePath(p, a.home))
			return
		}
	}
}

// rootForms spells each protected root the ways a script might: absolute,
// and relative to home as ~/x, $HOME/x and ${HOME}/x.
func (a *spAnalyzer) rootForms() []string {
	var out []string
	home := foldPath(a.home)
	for _, r := range a.roots.roots {
		out = append(out, r)
		if home != "" && strings.HasPrefix(r, home+"/") {
			rel := r[len(home)+1:]
			out = append(out, "~/"+rel, foldPath("$HOME/"+rel), foldPath("${HOME}/"+rel))
		}
	}
	return out
}

// protectedArg resolves a command argument as a path (also the value of
// --opt=value and dd's of=) and reports whether it is protected. The path
// comes back absolute.
func (a *spAnalyzer) protectedArg(arg, cwd string) (string, bool) {
	if arg == "" || arg == "-" {
		return "", false
	}
	cands := []string{arg}
	if eq := strings.IndexByte(arg, '='); eq > 0 && eq < len(arg)-1 {
		cands = append(cands, arg[eq+1:])
	} else if strings.HasPrefix(arg, "-") {
		return "", false
	}
	for _, c := range cands {
		c = a.expandVars(c)
		p := absPath(c, cwd, a.home)
		if a.roots.has(foldPath(p)) {
			return p, true
		}
	}
	return "", false
}

func (a *spAnalyzer) expandVars(w string) string {
	if a.pilothouseHome != "" {
		for _, v := range []string{"${PILOTHOUSE_HOME}", "$PILOTHOUSE_HOME"} {
			if w == v || strings.HasPrefix(w, v+"/") {
				return a.pilothouseHome + w[len(v):]
			}
		}
	}
	return w
}

// skipOptions returns the index of the first non-option word at or after
// i. Options listed in valueFlags consume the following word; numeric
// words (nice -10, watch 2) are skipped too.
func skipOptions(words []string, i int, valueFlags map[string]bool) int {
	for i < len(words) {
		w := words[i]
		switch {
		case w == "--":
			return i + 1
		case valueFlags[w]:
			i += 2
		case strings.HasPrefix(w, "-") && len(w) > 1:
			i++
		default:
			return i
		}
	}
	return i
}

func firstNonFlag(words []string) string {
	for _, w := range words {
		if !strings.HasPrefix(w, "-") {
			return w
		}
	}
	return ""
}

// hasAnyFlag reports whether args contain one of flags, including long
// flags with =value and short flags inside a cluster or with an attached
// value ("-ni", "-i.bak", "-o/file").
func hasAnyFlag(args, flags []string) bool {
	for _, a := range args {
		for _, f := range flags {
			switch {
			case a == f:
				return true
			case strings.HasPrefix(f, "--") || len(f) != 2:
				if strings.HasPrefix(a, f+"=") {
					return true
				}
			case len(a) > 1 && a[0] == '-' && a[1] != '-':
				// Leading letters of a short-option cluster.
				for i := 1; i < len(a) && isLetter(a[i]); i++ {
					if a[i] == f[1] {
						return true
					}
				}
			}
		}
	}
	return false
}

func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// shellJoin re-quotes words so they can be re-parsed as one command.
func shellJoin(words []string) string {
	var b strings.Builder
	for i, w := range words {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteByte('\'')
		b.WriteString(strings.ReplaceAll(w, "'", `'\''`))
		b.WriteByte('\'')
	}
	return b.String()
}

// tildePath shortens p for messages.
func tildePath(p, home string) string {
	if home != "" && (p == home || strings.HasPrefix(p, home+"/")) {
		return "~" + p[len(home):]
	}
	return p
}

// resolveSymlinks returns p with symlinks resolved, or its parent's
// resolution joined with the base name when p does not exist yet (a new
// file in a symlinked directory). Used only for edit tools, which are
// rare enough that the syscalls don't matter.
func resolveSymlinks(p string) (string, bool) {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r, r != p
	}
	dir, file := filepath.Split(p)
	if r, err := filepath.EvalSymlinks(filepath.Clean(dir)); err == nil {
		out := filepath.Join(r, file)
		return out, out != p
	}
	return p, false
}
