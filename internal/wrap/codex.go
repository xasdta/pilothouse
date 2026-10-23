package wrap

import (
	"bytes"
	"context"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Codex's interactive CLI (0.16x) runs sessions on a shared background
// `codex app-server --managed-daemon` by default. That daemon, not the TUI
// in the holder's PTY, runs the hooks, so they inherit the daemon's
// environment and PID: PILOTHOUSE_SESSION_ID never reaches them and the
// hook events show up as a second, observed session. `--no-daemon` ("Run
// without the shared background server, even if it is already running")
// makes the TUI run its own embedded app-server as its child, so hooks
// inherit the wrapped session's environment. It is accepted by the TUI and
// by `resume` / `fork`, and rejected with `--remote`, `agents`, `queue` and
// `remote-control` (codex 0.164.0 `--help`, and its error strings
// "--no-daemon cannot be used with …").

// codexValueFlags are top-level Codex options that take a value, so the
// word after them is not a subcommand or prompt.
var codexValueFlags = map[string]bool{
	"-c": true, "--config": true, "--enable": true, "--disable": true,
	"--remote": true, "--remote-auth-token-env": true, "-i": true, "--image": true,
	"-m": true, "--model": true, "--local-provider": true, "-p": true, "--profile": true,
	"-s": true, "--sandbox": true, "-C": true, "--cd": true, "--add-dir": true,
	"-a": true, "--ask-for-approval": true,
}

// codexNoDaemonSubcommands accept --no-daemon after the subcommand name.
var codexNoDaemonSubcommands = map[string]bool{"resume": true, "fork": true}

// subcommandLike matches a word that could be a Codex subcommand (known
// or hidden). An initial prompt almost always has spaces or punctuation.
var subcommandLike = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// codexNoDaemonArgv returns argv (codex and its arguments) with
// --no-daemon added where Codex accepts it: for the interactive TUI
// (optionally with a prompt) right after the program, for resume/fork
// right after the subcommand. Anything else (exec, review, mcp, unknown
// words that may be hidden subcommands, --remote, --help/--version, or an
// explicit --no-daemon) is returned unchanged with ok false.
func codexNoDaemonArgv(argv []string) (out []string, ok bool) {
	if len(argv) == 0 {
		return argv, false
	}
	args := argv[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break // the rest is the prompt
		}
		if strings.HasPrefix(a, "-") {
			name, _, hasValue := strings.Cut(a, "=")
			switch name {
			case "--no-daemon", "--remote", "-h", "--help", "-V", "--version":
				return argv, false
			}
			if codexValueFlags[name] && !hasValue {
				i++ // skip the value
			}
			continue
		}
		// First positional word: a subcommand or the initial prompt.
		if codexNoDaemonSubcommands[a] {
			return slices.Concat(argv[:i+2], []string{"--no-daemon"}, argv[i+2:]), true
		}
		if subcommandLike.MatchString(a) {
			return argv, false
		}
		break
	}
	return slices.Concat(argv[:1], []string{"--no-daemon"}, argv[1:]), true
}

// codexSupportsNoDaemon asks the Codex binary whether it knows
// --no-daemon (older versions would refuse to start with it).
func codexSupportsNoDaemon(exe string, env []string, dir string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "--help")
	cmd.Env, cmd.Dir = env, dir
	out, _ := cmd.Output()
	return bytes.Contains(out, []byte("--no-daemon"))
}
