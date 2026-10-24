package risk

import (
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/policy"
)

const (
	high   = api.RiskHigh
	medium = api.RiskMedium
	low    = api.RiskLow
)

// script classifies a shell command line. cwd follows `cd` within the
// line; remote is set for commands that run on another machine (ssh), where
// local paths mean nothing.
func (f *finder) script(s, cwd string, depth int, remote bool) {
	if depth > maxDepth || strings.TrimSpace(s) == "" {
		return
	}
	line := policy.ParseShell(s)
	for i := range line.Commands {
		f.simple(&line, i, &cwd, depth, remote)
	}
	for _, sub := range line.Subs {
		f.script(sub, cwd, depth+1, remote)
	}
}

// run is one simple command being classified.
type run struct {
	f      *finder
	line   *policy.ShellLine
	i      int
	cmd    *policy.ShellCommand
	cwd    *string
	depth  int
	remote bool
	wraps  []string
}

func (f *finder) simple(line *policy.ShellLine, i int, cwd *string, depth int, remote bool) {
	cmd := &line.Commands[i]
	argv, wraps := policy.UnwrapCommand(cmd.Words)
	x := &run{f: f, line: line, i: i, cmd: cmd, cwd: cwd, depth: depth, remote: remote, wraps: wraps}
	for _, w := range wraps {
		if w == "sudo" || w == "doas" {
			f.add(api.RiskSudo, high, "Runs as root ("+w+")")
		}
	}
	x.redirects()
	if len(argv) == 0 {
		if len(wraps) > 0 && wraps[len(wraps)-1] == "env" {
			f.add(api.RiskSecrets, medium, "Prints environment variables, which may hold credentials")
		}
		return
	}
	x.exec(argv)
}

func (x *run) has(wrapper string) bool {
	for _, w := range x.wraps {
		if w == wrapper {
			return true
		}
	}
	return false
}

// exec classifies the command that actually runs.
func (x *run) exec(argv []string) {
	f := x.f
	prog := policy.CommandName(argv[0])
	args := argv[1:]
	x.runsFile(argv[0])
	switch {
	case prog == "cd" || prog == "pushd":
		x.cd(args)
		return
	case shells[prog]:
		x.shell(args)
		return
	case prog == "eval":
		x.fetchedCode()
		f.script(strings.Join(args, " "), *x.cwd, x.depth+1, x.remote)
		return
	case prog == "source" || prog == ".":
		x.fetchedCode()
		if len(args) > 0 {
			x.runsFile(args[0])
		}
	case prog == "su":
		f.add(api.RiskSudo, high, "Runs as another user (su)")
		for j := 0; j+1 < len(args); j++ {
			if args[j] == "-c" || args[j] == "--command" {
				f.script(args[j+1], *x.cwd, x.depth+1, x.remote)
			}
		}
		return
	case prog == "pkexec" || prog == "run0":
		f.add(api.RiskSudo, high, "Runs as root ("+prog+")")
		if rest, _ := policy.UnwrapCommand(args); len(rest) > 0 {
			x.exec(rest)
		}
		return
	case prog == "ssh":
		x.ssh(args)
		return
	case interpreters[prog]:
		x.fetchedCode()
		x.pipedScript()
		if len(args) >= 2 && args[0] == "-m" && (args[1] == "pip" || args[1] == "pip3") {
			x.pip("pip", args[2:])
		}
		if j := firstPositional(args); j >= 0 && args[j] != "-" {
			x.runsFile(args[j])
		}
	case prog == "rm":
		x.rm(args)
	case prog == "find":
		x.find(args)
	case prog == "git":
		x.git(args)
	}
	x.deploy(prog, args)
	x.install(prog, args)
	x.network(prog, args)
	x.database(prog, args)
	x.system(prog, args)
	x.perms(prog, args)
	x.credentials(prog, args)
	x.files(prog, args)
}

var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "mksh": true, "fish": true, "ash": true, "yash": true, "tcsh": true, "csh": true}

var interpreters = map[string]bool{
	"python": true, "python2": true, "python3": true, "perl": true, "ruby": true, "node": true, "deno": true,
	"php": true, "lua": true, "osascript": true, "pwsh": true, "powershell": true, "tclsh": true,
}

var fetchers = map[string]bool{"curl": true, "wget": true, "http": true, "https": true, "xh": true, "xhs": true, "curlie": true, "aria2c": true, "fetch": true}

// ---- shells and downloaded code ---------------------------------------

func (x *run) shell(args []string) {
	f := x.f
	x.fetchedCode()
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
				f.script(w, *x.cwd, x.depth+1, x.remote)
			} else {
				x.runsFile(w)
			}
			return
		}
	}
	for _, r := range x.cmd.Redirects {
		switch r.Op {
		case "<<<":
			f.script(r.Target, *x.cwd, x.depth+1, x.remote)
		case "<<", "<<-":
			f.script(r.Body, *x.cwd, x.depth+1, x.remote)
		case "<":
			x.runsFile(r.Target)
		}
	}
	x.pipedScript()
}

// pipedScript: a shell or interpreter reading its program from a pipe
// that starts with a download (curl … | sh).
func (x *run) pipedScript() {
	for j := x.i; j > 0 && x.line.Commands[j].PipedIn; j-- {
		argv, _ := policy.UnwrapCommand(x.line.Commands[j-1].Words)
		if len(argv) > 0 && fetchers[policy.CommandName(argv[0])] {
			x.f.add(api.RiskNetwork, high, "Runs a script downloaded from "+hostOr(urlHost(argv[1:])))
			return
		}
	}
}

var fetchSubRE = regexp.MustCompile("(?:\\$\\(|<\\(|`)\\s*(?:sudo\\s+)?(?:curl|wget|xh|http)\\b")

// fetchedCode: `bash -c "$(curl …)"`, `source <(curl …)`, `eval "$(wget …)"`.
func (x *run) fetchedCode() {
	for _, w := range x.cmd.Words {
		if m := fetchSubRE.FindStringIndex(w); m != nil {
			x.f.add(api.RiskNetwork, high, "Runs a script downloaded from "+hostOr(urlHost(strings.Fields(w[m[1]:]))))
			return
		}
	}
}

// runsFile: running a file a fetch earlier in the line downloaded.
func (x *run) runsFile(arg string) {
	if len(x.f.downloaded) == 0 {
		return
	}
	if p, ok := x.f.resolve(arg, *x.cwd); ok {
		if host, ok := x.f.downloaded[p]; ok {
			x.f.add(api.RiskNetwork, high, "Runs a script downloaded from "+hostOr(host))
		}
	}
}

func (x *run) cd(args []string) {
	switch {
	case len(args) == 0:
		*x.cwd = x.f.home
	case args[0] == "-" || strings.HasPrefix(args[0], "-"):
		if len(args) > 1 && args[0] != "-" {
			x.cd(args[1:])
			return
		}
		*x.cwd = "" // unknown from here on
	default:
		if p, ok := x.f.resolve(args[0], *x.cwd); ok {
			*x.cwd = p
		} else {
			*x.cwd = ""
		}
	}
}

func (x *run) ssh(args []string) {
	f := x.f
	const valueLetters = "bcDEeFIiJLlmOoPpQRSWw"
	j := 0
	for j < len(args) {
		a := args[j]
		if a == "--" {
			j++
			break
		}
		if len(a) < 2 || a[0] != '-' {
			break
		}
		for k := 1; k < len(a); k++ {
			if strings.IndexByte(valueLetters, a[k]) >= 0 {
				if k == len(a)-1 {
					j++ // the value is the next word
				}
				break
			}
		}
		j++
	}
	if j >= len(args) {
		return
	}
	host := args[j]
	if at := strings.LastIndexByte(host, '@'); at >= 0 {
		host = host[at+1:]
	}
	rest := args[j+1:]
	var script string
	for _, r := range x.cmd.Redirects {
		if r.Op == "<<" || r.Op == "<<-" {
			script = r.Body
		}
	}
	if len(rest) == 0 && script == "" {
		f.add(api.RiskNetwork, low, "Opens a shell on "+host)
		return
	}
	f.add(api.RiskNetwork, medium, "Runs commands on "+host)
	if len(rest) > 0 {
		script = strings.Join(rest, " ")
	}
	f.script(script, "", x.depth+1, true)
}

// ---- redirections and file arguments ----------------------------------

func (x *run) redirects() {
	f := x.f
	if x.remote {
		return
	}
	for _, r := range x.cmd.Redirects {
		switch r.Op {
		case ">", ">>", ">|", "&>", "&>>", "<>", ">&":
			if isFD(r.Target) {
				continue
			}
			if p, ok := f.resolve(r.Target, *x.cwd); ok {
				f.write(p)
			}
		case "<":
			if p, ok := f.resolve(r.Target, *x.cwd); ok {
				f.read(p, false)
			}
		}
	}
}

func isFD(s string) bool {
	if s == "-" {
		return true
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// contentReaders print what they read.
var contentReaders = map[string]bool{
	"cat": true, "less": true, "more": true, "head": true, "tail": true, "bat": true, "grep": true, "egrep": true,
	"fgrep": true, "rg": true, "ag": true, "base64": true, "xxd": true, "od": true, "hexdump": true, "strings": true,
	"jq": true, "yq": true, "awk": true, "sed": true, "sort": true, "uniq": true, "nl": true, "cut": true, "diff": true,
	"open": true, "code": true, "vim": true, "vi": true, "nano": true, "emacs": true, "pbcopy": true,
}

// listers only look at metadata.
var listers = map[string]bool{
	"ls": true, "stat": true, "test": true, "[": true, "file": true, "du": true, "tree": true, "exa": true,
	"eza": true, "lsd": true, "realpath": true, "readlink": true, "wc": true, "find": true, "fd": true,
}

// textOnly commands take text, not paths.
var textOnly = map[string]bool{"echo": true, "printf": true, "cd": true, "pushd": true, "export": true, "true": true, "false": true, ":": true}

// files labels the paths a command reads or writes: credentials touched
// and writes outside the repo.
func (x *run) files(prog string, args []string) {
	f := x.f
	if x.remote || textOnly[prog] {
		return
	}
	writeAll := func(pos []string) {
		for _, a := range pos {
			if p, ok := f.resolve(a, *x.cwd); ok {
				f.write(p)
			}
		}
	}
	switch prog {
	case "tee", "touch", "mkdir", "rmdir", "unlink", "shred", "rm":
		writeAll(positionals(args, nil))
		return
	case "truncate":
		writeAll(positionals(args, flagSet("-s", "--size", "-r", "--reference")))
		return
	case "chmod", "chown", "chgrp":
		if pos := positionals(args, flagSet("--reference")); len(pos) > 1 {
			writeAll(pos[1:])
		}
		return
	case "cp", "mv", "install", "ln", "rsync", "ditto":
		vf := flagSet("-t", "--target-directory", "-S", "--suffix", "-m", "--mode", "-o", "--owner", "-g", "--group", "-e", "--rsh", "--exclude", "--include", "--filter", "-f")
		pos := positionals(args, vf)
		dst := flagValue(args, "-t", "--target-directory")
		srcs := pos
		if dst == "" && len(pos) >= 2 {
			dst, srcs = pos[len(pos)-1], pos[:len(pos)-1]
		}
		if prog == "rsync" {
			if _, remote := remotePath(dst); remote {
				return // remoteCopy labels it
			}
		}
		if dst != "" {
			writeAll([]string{dst})
		}
		for _, s := range srcs {
			if p, ok := f.resolve(s, *x.cwd); ok {
				if prog == "mv" {
					f.write(p)
				} else {
					f.read(p, false)
				}
			}
		}
		return
	case "sed", "perl":
		if !shortFlag(args, "i") && !longFlag(args, "--in-place") {
			break
		}
		pos := positionals(args, flagSet("-e", "--expression", "-f", "--file"))
		if flagValue(args, "-e", "--expression") == "" && len(pos) > 0 {
			pos = pos[1:] // the script
		}
		writeAll(pos)
		return
	case "dd":
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "of="); ok {
				if p, ok := f.resolve(v, *x.cwd); ok && !diskDevice(p) {
					f.write(p)
				}
			}
			if v, ok := strings.CutPrefix(a, "if="); ok {
				if p, ok := f.resolve(v, *x.cwd); ok {
					f.read(p, false)
				}
			}
		}
		return
	}
	listing := listers[prog]
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			eq := strings.IndexByte(a, '=')
			if eq < 0 {
				continue
			}
			a = a[eq+1:]
		}
		p, ok := f.resolve(a, *x.cwd)
		if !ok {
			continue
		}
		what, secret := f.secretPath(p, false)
		switch {
		case !secret:
		case listing:
			f.add(api.RiskSecrets, medium, "Looks at "+what)
		case contentReaders[prog]:
			f.add(api.RiskSecrets, high, "Reads "+what)
		default:
			f.add(api.RiskSecrets, high, "Uses "+what)
		}
	}
}

// ---- rm, find ------------------------------------------------------------

func (x *run) rm(args []string) {
	f := x.f
	if !shortFlag(args, "rR") && !longFlag(args, "--recursive") {
		return
	}
	if longFlag(args, "--no-preserve-root") {
		f.add(api.RiskDestructive, high, "Recursively deletes / (--no-preserve-root)")
	}
	targets := positionals(args, nil)
	if len(targets) == 0 && x.has("xargs") {
		f.add(api.RiskDestructive, medium, "Recursively deletes whatever another command lists (xargs rm -r)")
	}
	for _, t := range targets {
		exp := expandHome(t, f.home)
		if strings.ContainsAny(exp, "$`") {
			f.add(api.RiskDestructive, medium, "Recursively deletes a path computed at run time ("+clip(t, 60)+")")
			continue
		}
		if x.remote {
			switch {
			case t == "~" || t == "~/" || t == "~/*" || t == "*" || t == "." || t == "..":
				f.add(api.RiskDestructive, high, "Recursively deletes "+t+" on another machine")
			case filepath.IsAbs(t):
				p := filepath.Clean(t)
				if hasGlobMeta(filepath.Base(p)) {
					p = filepath.Dir(p)
				}
				if f.broadDir(p) && !under(p, f.home) {
					f.add(api.RiskDestructive, high, "Recursively deletes "+t+" on another machine")
				}
			}
			continue
		}
		p, ok := f.resolve(t, *x.cwd)
		if !ok {
			continue
		}
		if what, broad := f.broadDelete(p, hasGlobMeta(filepath.Base(t))); broad {
			f.add(api.RiskDestructive, high, "Recursively deletes "+what)
		}
	}
}

func (x *run) find(args []string) {
	f := x.f
	var roots []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") || a == "(" || a == "!" {
			break
		}
		roots = append(roots, a)
	}
	if len(roots) == 0 {
		roots = []string{"."}
	}
	deletes := false
	for j := 0; j < len(args); j++ {
		switch args[j] {
		case "-delete":
			deletes = true
		case "-exec", "-execdir", "-ok", "-okdir":
			k := j + 1
			for k < len(args) && args[k] != ";" && args[k] != "+" {
				k++
			}
			if tmpl := args[j+1 : k]; len(tmpl) > 0 {
				switch policy.CommandName(tmpl[0]) {
				case "rm", "shred", "unlink", "rmdir":
					deletes = true
				}
				f.script(quoteJoin(tmpl), *x.cwd, x.depth+1, x.remote)
			}
			j = k
		}
	}
	if !deletes {
		return
	}
	for _, r := range roots {
		if p, ok := f.resolve(r, *x.cwd); ok && f.broadDir(p) {
			f.add(api.RiskDestructive, high, "Deletes the files find matches under "+f.show(p))
			return
		}
	}
	f.add(api.RiskDestructive, medium, "Deletes the files find matches")
}

// ---- git -----------------------------------------------------------------

// isProtectedBranch: main, master and release branches.
func isProtectedBranch(b string) bool {
	return b == "main" || b == "master" || strings.HasPrefix(b, "release")
}

// deployRemotes are git remotes a push to deploys.
var deployRemotes = map[string]bool{"heroku": true, "dokku": true, "piku": true, "production": true, "prod": true}

func (x *run) git(args []string) {
	f := x.f
	other := false // -C / --git-dir: another repository
	sub, rest := "", []string(nil)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-C" || a == "--git-dir" || a == "--work-tree":
			other = true
			i++
		case a == "-c" || a == "--namespace" || a == "--config-env" || a == "--super-prefix" || a == "--exec-path":
			i++
		case strings.HasPrefix(a, "--git-dir=") || strings.HasPrefix(a, "--work-tree="):
			other = true
		case strings.HasPrefix(a, "-"):
		default:
			sub, rest = a, args[i+1:]
			i = len(args)
		}
	}
	branch := f.c.Branch
	if other || x.remote || (*x.cwd != "" && f.repo != "" && !under(*x.cwd, f.repo)) {
		branch = ""
	}
	switch sub {
	case "push":
		x.gitPush(rest, branch)
	case "commit":
		if isProtectedBranch(branch) && !longFlag(rest, "--dry-run") {
			f.add(api.RiskProtectedBranch, medium, "Commits directly to "+branch)
		}
	case "merge", "cherry-pick", "revert", "am", "rebase":
		if isProtectedBranch(branch) && !longFlag(rest, "--abort", "--quit", "--skip", "--show-current-patch") {
			f.add(api.RiskProtectedBranch, medium, "Changes "+branch+" directly (git "+sub+")")
		}
	case "reset":
		if longFlag(rest, "--hard") {
			f.add(api.RiskDestructive, high, "Discards uncommitted changes (git reset --hard)")
		}
	case "clean":
		if (shortFlag(rest, "f") || longFlag(rest, "--force")) && !shortFlag(rest, "n") && !longFlag(rest, "--dry-run") {
			text := "Deletes untracked files (git clean)"
			if shortFlag(rest, "xX") {
				text = "Deletes untracked and ignored files (git clean -x)"
			}
			f.add(api.RiskDestructive, high, text)
		}
	case "checkout", "restore":
		staged := sub == "restore" && (shortFlag(rest, "S") || longFlag(rest, "--staged")) && !shortFlag(rest, "W") && !longFlag(rest, "--worktree")
		whole := false
		for _, p := range positionals(rest, flagSet("-s", "--source", "-b", "-B", "--orphan")) {
			if p == "." || p == ":/" || p == "*" {
				whole = true
			}
		}
		switch {
		case staged:
		case whole:
			f.add(api.RiskDestructive, medium, "Discards local changes (git "+sub+" .)")
		case sub == "checkout" && (shortFlag(rest, "f") || longFlag(rest, "--force")):
			f.add(api.RiskDestructive, medium, "Discards local changes (git checkout --force)")
		}
	case "branch":
		if shortFlag(rest, "D") || (longFlag(rest, "--delete") && longFlag(rest, "--force")) {
			names := positionals(rest, nil)
			level := medium
			for _, n := range names {
				if isProtectedBranch(n) {
					level = high
				}
			}
			f.add(api.RiskDestructive, level, "Deletes branch "+clip(strings.Join(names, ", "), 60)+" even if unmerged")
		}
	case "stash":
		if len(rest) > 0 && (rest[0] == "drop" || rest[0] == "clear") {
			f.add(api.RiskDestructive, medium, "Drops stashed changes (git stash "+rest[0]+")")
		}
	case "filter-branch", "filter-repo":
		f.add(api.RiskDestructive, high, "Rewrites the repository's history (git "+sub+")")
	}
}

func (x *run) gitPush(args []string, branch string) {
	f := x.f
	var force, lease, all, del, dry, prune, tags bool
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "--") {
			name, _, inline := strings.Cut(a, "=")
			switch name {
			case "--force":
				force = true
			case "--force-with-lease", "--force-if-includes":
				lease = true
			case "--mirror":
				force, all = true, true
			case "--all", "--branches":
				all = true
			case "--delete":
				del = true
			case "--dry-run":
				dry = true
			case "--prune":
				prune = true
			case "--tags":
				tags = true
			case "--repo", "--receive-pack", "--exec", "--push-option":
				if !inline {
					i++
				}
			}
			continue
		}
		if len(a) > 1 && a[0] == '-' {
			for k := 1; k < len(a); k++ {
				switch a[k] {
				case 'f':
					force = true
				case 'd':
					del = true
				case 'n':
					dry = true
				case 'o':
					if k == len(a)-1 {
						i++
					}
					k = len(a)
				}
			}
			continue
		}
		pos = append(pos, a)
	}
	if dry {
		return
	}
	remote, refspecs := "", pos
	if len(pos) > 0 {
		remote, refspecs = pos[0], pos[1:]
	}
	var dsts []string
	for _, r := range refspecs {
		if strings.HasPrefix(r, "+") {
			force = true
			r = r[1:]
		}
		src, dst, colon := strings.Cut(r, ":")
		if !colon {
			dst = src
		} else if src == "" {
			del = true
		}
		if dst == "HEAD" || dst == "@" {
			dst = branch
		}
		if dst = strings.TrimPrefix(dst, "refs/heads/"); dst != "" {
			dsts = append(dsts, dst)
		}
	}
	if len(refspecs) == 0 && !tags && !all && branch != "" {
		dsts = append(dsts, branch)
	}
	target := ""
	for _, d := range dsts {
		if isProtectedBranch(d) {
			target = d
			break
		}
	}
	if all && target == "" {
		target = "every branch"
	}
	to := ""
	switch {
	case target != "":
		to = " to " + target
	case len(dsts) > 0:
		to = " to " + clip(dsts[0], 40)
	}
	switch {
	case force:
		f.add(api.RiskForcePush, high, "Force-pushes"+to)
	case lease:
		f.add(api.RiskForcePush, medium, "Force-pushes with lease"+to)
	}
	if target != "" && !del {
		f.add(api.RiskProtectedBranch, high, "Pushes"+to)
	}
	if del {
		f.add(api.RiskDestructive, medium, "Deletes a remote branch")
	}
	if prune {
		f.add(api.RiskDestructive, medium, "Deletes remote branches that are gone locally (--prune)")
	}
	if deployRemotes[remote] {
		f.add(api.RiskDeploy, high, "Deploys by pushing to "+remote)
	}
}

// ---- helpers -------------------------------------------------------------

func flagSet(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// shortFlag reports whether a short option cluster in args (-rf) has one
// of letters. Words after "--" are not options.
func shortFlag(args []string, letters string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsAny(a[1:], letters) {
			return true
		}
	}
	return false
}

// longFlag reports whether args has one of the long options, alone or
// with =value.
func longFlag(args []string, names ...string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		for _, n := range names {
			if a == n || strings.HasPrefix(a, n+"=") {
				return true
			}
		}
	}
	return false
}

// flagValue returns the value of the first of names in args ("-t dir",
// "--target-directory=dir").
func flagValue(args []string, names ...string) string {
	for i, a := range args {
		for _, n := range names {
			if a == n && i+1 < len(args) {
				return args[i+1]
			}
			if v, ok := strings.CutPrefix(a, n+"="); ok && strings.HasPrefix(n, "--") {
				return v
			}
		}
	}
	return ""
}

// positionals returns the words that are not options, skipping the values
// of valueFlags. Everything after "--" is positional.
func positionals(args []string, valueFlags map[string]bool) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return append(out, args[i+1:]...)
		case valueFlags[a]:
			i++
		case len(a) > 1 && a[0] == '-':
		default:
			out = append(out, a)
		}
	}
	return out
}

func firstPositional(args []string) int {
	for i, a := range args {
		if a == "--" {
			if i+1 < len(args) {
				return i + 1
			}
			return -1
		}
		if !strings.HasPrefix(a, "-") {
			return i
		}
	}
	return -1
}

// subcommand returns the first positional word, skipping options and the
// values of valueFlags, and the words after it.
func subcommand(args []string, valueFlags map[string]bool) (string, []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return "", nil
		case valueFlags[a]:
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return a, args[i+1:]
		}
	}
	return "", nil
}

// urlHost is the host of the first URL in args ("" when there is none).
func urlHost(args []string) string {
	for _, a := range args {
		if !strings.Contains(a, "://") {
			continue
		}
		if i := strings.Index(a, "http"); i > 0 {
			a = a[i:] // "--url=https://…", "'https://…"
		}
		if u, err := url.Parse(strings.Trim(a, `'"`)); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
	}
	return ""
}

func hostOr(h string) string {
	if h == "" {
		return "the internet"
	}
	return h
}

// remotePath recognises scp/rsync remote paths: [user@]host:path,
// host::module, rsync://host/….
func remotePath(s string) (string, bool) {
	if rest, ok := strings.CutPrefix(s, "rsync://"); ok {
		h, _, _ := strings.Cut(rest, "/")
		return h, h != ""
	}
	i := strings.IndexByte(s, ':')
	if i <= 0 || strings.ContainsRune(s[:i], '/') {
		return "", false
	}
	h := s[:i]
	if at := strings.LastIndexByte(h, '@'); at >= 0 {
		h = h[at+1:]
	}
	return h, h != ""
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// quoteJoin re-quotes words so they parse back as the same command.
func quoteJoin(words []string) string {
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

func listNames(names []string) string {
	if len(names) <= 3 {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:3], ", ") + " +" + strconv.Itoa(len(names)-3) + " more"
}
