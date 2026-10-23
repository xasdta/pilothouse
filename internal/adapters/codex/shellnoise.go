package codex

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// ShellNoise runs `$SHELL -c true` the way Codex 0.164 runs every hook
// command and returns what it printed on stdout. Anything there (a banner
// from ~/.zshenv, `nvm` chatter, …) lands in front of a hook's JSON answer,
// so Codex can no longer read it: Pilothouse's denies survive (they use the
// exit code), but a remote "allow" falls back to Codex's own prompt and
// SessionStart context is dropped. `pilothouse doctor` reports a non-empty
// result. $SHELL unset means /bin/sh, as in Codex
// (codex-rs/hooks/src/engine/command_runner.rs, default_shell_program).
func ShellNoise() (string, error) {
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/sh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, sh, "-c", "true")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.Stdin = nil // /dev/null, like a hook's closed stdin after the payload
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return out.String(), fmt.Errorf("%s -c true did not finish within 10 s", sh)
		}
		return out.String(), fmt.Errorf("%s -c true: %w", sh, err)
	}
	return out.String(), nil
}
