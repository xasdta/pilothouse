package task

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/term"
)

// A long task diff scrolls off a terminal, so it is paged the way `git
// diff` pages itself. Paging only ever applies to a terminal: a pipe, a
// redirect or a test buffer keeps the plain stream so the output can be
// captured and compared.

// defaultPager is what git falls back to when nothing names a pager. -F
// quits at the first screenful so a short diff leaves the terminal
// untouched, -R passes git's colours through, and -X skips the
// initialise/deinitialise sequence that would clear the screen on exit.
const defaultPager = "less -FRX"

// isTerminalWriter reports whether w is the terminal a human is reading.
// It is a var so tests can reach both sides of the pager decision
// without a tty.
var isTerminalWriter = func(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// pagerArgv is the pager to run, or ok=false when paging is off. It is a
// var so tests can point it at a pager that is guaranteed to exist.
var pagerArgv = func() (argv []string, ok bool) {
	prog, ok := pagerEnv()
	if !ok {
		return nil, false
	}
	return strings.Fields(prog), true
}

// pagerEnv resolves the pager the way git does: GIT_PAGER wins over
// PAGER, and an explicitly empty value means "never page" -- that is how
// `PAGER=cat` and `git --no-pager` are spelled.
func pagerEnv() (string, bool) {
	for _, key := range []string{"GIT_PAGER", "PAGER"} {
		if v, set := lookupEnv(key); set {
			return v, v != ""
		}
	}
	return defaultPager, true
}

// lookupEnv is os.LookupEnv, indirected so the precedence above can be
// tested without touching the process environment.
var lookupEnv = os.LookupEnv

// pagerLookPath finds the pager binary; a var so tests can substitute.
var pagerLookPath = exec.LookPath

// pageWriter wraps w for the span of one diff. It returns the writer the
// diff should be written to, whether git needs to be told to colour its
// output, and a finish func that closes the pager's stdin and waits for
// the pager to exit. finish is never nil and is safe to call even when
// nothing was paged.
//
// Colour needs saying explicitly because git only colours a diff when
// its own stdout is a terminal, and with a pager that stdout is the
// pager's pipe. git solves the same problem with color.pager=true.
//
// Every failure to page falls back to writing straight to w: someone who
// asked to see a diff would rather read it unpaged than be told their
// pager is broken.
func pageWriter(ctx context.Context, w io.Writer) (out io.Writer, color bool, finish func()) {
	plain := func() (io.Writer, bool, func()) { return w, false, func() {} }

	if !isTerminalWriter(w) {
		return plain()
	}
	argv, ok := pagerArgv()
	if !ok || len(argv) == 0 {
		return plain()
	}
	bin, err := pagerLookPath(argv[0])
	if err != nil {
		return plain()
	}

	cmd := exec.CommandContext(ctx, bin, argv[1:]...)
	// The pager inherits the destination the diff was already headed
	// for, which is the terminal we just tested.
	cmd.Stdout, cmd.Stderr = w, os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return plain()
	}
	if err := cmd.Start(); err != nil {
		return plain()
	}
	return stdin, true, func() {
		// Closing stdin is what tells the pager the diff is over, so it
		// flushes its last screenful and exits. The pager's exit status
		// is dropped on purpose: the command's status has to describe
		// the diff, not how the reader left the pager.
		_ = stdin.Close()
		_ = cmd.Wait()
	}
}
