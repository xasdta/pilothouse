package daemon

import (
	"path/filepath"
	"strings"

	"github.com/xasdta/pilothouse/internal/api"
)

// Session grants ("allow for this session", BUILD-SPEC §5).
//
// A shell grant covers a command *key*: the program plus its subcommand
// ("git push"), found after skipping the program's global flags and their
// values, so `git -c user.name=x commit` grants "git commit" — never all of
// git. When the subcommand can't be determined safely (an unknown flag that
// might take a value, a path or quoted word where the subcommand should
// be), the grant covers only that exact command. A bare program name is
// granted only for a command that was just the program name.

// globalFlags describes a program's options that may come before its
// subcommand: flags that take a separate value, and flags that don't.
// A flag in neither set makes the subcommand undeterminable.
type globalFlags struct {
	value   map[string]bool
	boolean map[string]bool
}

func flagSet(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

// wholeProgramTools create directories or look at metadata, never file
// contents, so "allow for this session" on one call can cover every call of
// the program (`mkdir -p a`, then `mkdir -p b`). Content readers such as cat
// stay exact: allowing `cat README.md` must not allow `cat ~/.ssh/id_ed25519`.
// Deny and ask rules still run first.
var wholeProgramTools = map[string]bool{
	"mkdir": true, "touch": true, "ls": true, "pwd": true, "which": true,
	"tree": true, "stat": true, "du": true, "df": true,
}

// subcommandTools are programs whose second word selects what they do.
var subcommandTools = map[string]globalFlags{
	"git": {
		value: flagSet("-c", "-C", "--git-dir", "--work-tree", "--namespace", "--super-prefix", "--config-env", "--list-cmds"),
		boolean: flagSet("-p", "-P", "--paginate", "--no-pager", "--bare", "--no-replace-objects", "--literal-pathspecs",
			"--glob-pathspecs", "--noglob-pathspecs", "--icase-pathspecs", "--no-optional-locks", "--no-advice"),
	},
	"npm": {
		value:   flagSet("--prefix", "-C", "--userconfig", "--registry", "--cache", "--loglevel", "-w", "--workspace"),
		boolean: flagSet("-g", "--global", "-s", "--silent", "-q", "--quiet", "-d", "--verbose", "-y", "--yes", "--workspaces", "-ws"),
	},
	"pnpm": {
		value:   flagSet("-C", "--dir", "--filter", "-F", "--loglevel"),
		boolean: flagSet("-r", "--recursive", "-w", "--workspace-root", "-s", "--silent", "-g", "--global"),
	},
	"yarn": {
		value:   flagSet("--cwd"),
		boolean: flagSet("-s", "--silent", "--verbose"),
	},
	"bun":   {boolean: flagSet("--silent")},
	"cargo": {value: flagSet("--color", "--config", "-Z", "-C"), boolean: flagSet("-v", "-vv", "-q", "--quiet", "--verbose", "--offline", "--frozen", "--locked")},
	"go":    {value: flagSet("-C")},
	"docker": {
		value:   flagSet("-c", "--context", "-H", "--host", "--config", "-l", "--log-level", "--tlscacert", "--tlscert", "--tlskey"),
		boolean: flagSet("-D", "--debug", "--tls", "--tlsverify"),
	},
	"kubectl": {
		value:   flagSet("-n", "--namespace", "--context", "--kubeconfig", "--cluster", "--user", "-s", "--server", "--as", "-v"),
		boolean: flagSet("--insecure-skip-tls-verify"),
	},
	"helm":      {value: flagSet("-n", "--namespace", "--kube-context", "--kubeconfig"), boolean: flagSet("--debug")},
	"gh":        {value: flagSet("-R", "--repo")},
	"terraform": {value: flagSet("-chdir")},
	"aws":       {value: flagSet("--profile", "--region", "--output", "--endpoint-url"), boolean: flagSet("--debug", "--no-cli-pager", "--no-paginate")},
	"gcloud":    {value: flagSet("--project", "--account", "--configuration"), boolean: flagSet("-q", "--quiet")},
	"brew":      {boolean: flagSet("-v", "--verbose", "-d", "--debug")},
	"pip":       {boolean: flagSet("-q", "--quiet", "-v", "--verbose")},
	"pip3":      {boolean: flagSet("-q", "--quiet", "-v", "--verbose")},
	"uv":        {value: flagSet("--directory", "--project"), boolean: flagSet("-q", "--quiet", "-v", "--verbose", "--offline")},
	"poetry":    {value: flagSet("-C", "--directory"), boolean: flagSet("-q", "--quiet", "-v", "--verbose", "-n", "--no-interaction")},
	"systemctl": {boolean: flagSet("--user", "--system", "-q", "--quiet", "--no-pager")},
	"make":      {value: flagSet("-C", "--directory", "-f", "--file", "--makefile", "-I", "-o", "-W"), boolean: flagSet("-s", "--silent", "-k", "-B", "-n", "--dry-run")},
	"npx":       {value: flagSet("-p", "--package"), boolean: flagSet("-y", "--yes", "--no", "-q", "--quiet")},
	"deno":      {boolean: flagSet("-q", "--quiet")},
}

// commandKey is the grant key of a shell command: "prog sub" when the
// subcommand can be found, "prog" for a command that is only a program
// name. ok is false when neither can be determined safely.
func commandKey(words []string) (string, bool) {
	if len(words) == 0 {
		return "", false
	}
	prog := words[0]
	if len(words) == 1 {
		return prog, !isFlagOrPath(prog)
	}
	if wholeProgramTools[filepath.Base(prog)] && !isFlagOrPath(prog) {
		return prog, true
	}
	flags, known := subcommandTools[filepath.Base(prog)]
	if !known {
		// Unknown program: a plain second word is its subcommand (or
		// target, as for `make deploy`); anything else is undeterminable.
		if isFlagOrPath(words[1]) {
			return "", false
		}
		return prog + " " + words[1], true
	}
	for i := 1; i < len(words); i++ {
		w := words[i]
		switch {
		case w == "--":
			return "", false
		case strings.HasPrefix(w, "+") && filepath.Base(prog) == "cargo":
			continue // toolchain selector: cargo +nightly build
		case strings.HasPrefix(w, "-"):
			name, _, inline := strings.Cut(w, "=")
			switch {
			case flags.value[name] && inline:
			case flags.value[name]:
				i++ // skip the flag's value
			case flags.boolean[name] && !inline:
			case inline && strings.HasPrefix(name, "--"):
				// --unknown=value carries its own value.
			default:
				return "", false
			}
		case isFlagOrPath(w):
			return "", false
		default:
			return prog + " " + w, true
		}
	}
	return "", false // only flags: no subcommand
}

// grantPrefix is what an "allow for this session" covers: for shell tools
// the command key, or the exact command when it has none; other tools: the
// whole tool ("").
func grantPrefix(req api.Request) string {
	if req.ToolKind != api.ToolShell {
		return ""
	}
	words := strings.Fields(req.Command)
	if key, ok := commandKey(words); ok {
		return key
	}
	return strings.Join(words, " ")
}

func isFlagOrPath(w string) bool {
	return strings.HasPrefix(w, "-") || strings.ContainsAny(w, "/.~=$") || strings.HasPrefix(w, "\"") || strings.HasPrefix(w, "'")
}

// shellControl are characters that chain or redirect commands. A grant for
// "git push" must not also allow "git push; rm -rf ~", so commands
// containing any of these never match a grant and go back to a person.
const shellControl = ";&|`$<>()\n\\"

// matchGrant returns the first grant covering a tool call: a shell call
// matches when its command key equals the grant, or it is exactly the
// granted command.
func matchGrant(grants []api.Grant, tool string, kind api.ToolKind, command string) (api.Grant, bool) {
	var words []string
	var key string
	var keyed bool
	if kind == api.ToolShell {
		words = strings.Fields(command)
		key, keyed = commandKey(words)
	}
	for _, g := range grants {
		if g.Tool != tool {
			continue
		}
		if g.Prefix == "" {
			if kind != api.ToolShell {
				return g, true
			}
			continue // a shell grant always has a prefix; an empty one is malformed
		}
		if strings.ContainsAny(command, shellControl) {
			continue
		}
		prefix := strings.Join(strings.Fields(g.Prefix), " ")
		if (keyed && key == prefix) || strings.Join(words, " ") == prefix {
			return g, true
		}
	}
	return api.Grant{}, false
}
