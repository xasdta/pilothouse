package policy

import "strings"

// The shell reader, exported for internal/risk. It is the same reader the
// self-protection rule uses, in heredoc mode: here-document bodies are
// text attached to their redirection rather than commands.

// ShellCommand is one simple command of a shell line.
type ShellCommand struct {
	Words     []string // quotes and escapes removed
	Redirects []ShellRedirect
	PipedIn   bool // stdin comes from the previous command of a pipeline
	// Vars: a word or redirection target expands a parameter ($X, ${X},
	// $1 …) outside single quotes; its value is only known at run time.
	Vars bool
}

// ShellRedirect is one redirection of a ShellCommand.
type ShellRedirect struct {
	Op     string // ">", ">>", ">|", "&>", "&>>", "<", "<>", "<<", "<<-", "<<<", ">&", "<&"
	Target string // file, fd number, here-document delimiter or here-string text
	Body   string // here-document text (Op "<<" or "<<-")
	// Expands: the here-document's delimiter was unquoted, so the shell
	// expands parameters and runs substitutions in Body.
	Expands bool
}

// ShellLine is a parsed command line.
type ShellLine struct {
	Commands []ShellCommand
	// Subs are the bodies of $(…), `…`, <(…) and >(…), including those
	// inside unquoted here-documents, in order. They are not parsed.
	Subs []string
}

// ParseShell reads a command line into simple commands. It never fails:
// unterminated quotes or groups run to the end of the line.
func ParseShell(s string) ShellLine {
	p := parseShellDocs(s)
	out := ShellLine{Commands: make([]ShellCommand, len(p.cmds)), Subs: p.subs}
	for i, c := range p.cmds {
		sc := ShellCommand{Words: c.words, PipedIn: c.pipedIn, Vars: c.vars}
		for _, r := range c.redirs {
			sc.Redirects = append(sc.Redirects, ShellRedirect{Op: r.op, Target: r.target, Body: r.body, Expands: r.expands})
		}
		out.Commands[i] = sc
	}
	return out
}

// UnwrapCommand skips what runs a command without being it — leading
// NAME=value assignments and wrappers such as sudo, env, time, nohup,
// nice, timeout, xargs, exec and if/then/do — and returns the words of the
// command that actually runs plus the wrappers seen, by base name.
// `env -S "cmd args"` is split into words.
func UnwrapCommand(words []string) (argv, seen []string) {
	i := 0
	for i < len(words) {
		w := words[i]
		if isAssignment(w) {
			i++
			continue
		}
		base := cmdBase(w)
		if vf, ok := wrappers[base]; ok {
			seen = append(seen, base)
			i = skipOptions(words, i+1, vf)
			if (base == "timeout" || base == "gtimeout") && i < len(words) {
				i++ // the duration
			}
			continue
		}
		if base == "env" {
			seen = append(seen, base)
			j, split := skipEnvWords(words, i+1)
			if split != nil {
				return append(split, words[j:]...), seen
			}
			i = j
			continue
		}
		break
	}
	return words[i:], seen
}

// skipEnvWords skips env's options and assignments. split is set for
// `env -S "…"`: the string's words.
func skipEnvWords(words []string, i int) (int, []string) {
	for i < len(words) {
		w := words[i]
		switch {
		case w == "-u" || w == "-C" || w == "--unset" || w == "--chdir":
			i += 2
		case w == "-S" || w == "--split-string":
			if i+1 < len(words) {
				return i + 2, strings.Fields(words[i+1])
			}
			return len(words), nil
		case strings.HasPrefix(w, "-S") && len(w) > 2:
			return i + 1, strings.Fields(w[2:])
		case strings.HasPrefix(w, "-") || isAssignment(w):
			i++
		default:
			return i, nil
		}
	}
	return i, nil
}

// CommandName is the lower-cased executable name of a command word:
// "/usr/bin/Git" → "git".
func CommandName(word string) string { return cmdBase(word) }
