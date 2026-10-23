package task

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/xasdta/pilothouse/internal/api"
)

// ansiColor matches the SGR sequences git emits for --color=always.
var ansiColor = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// stubPagerSeams swaps the pager seams for one test and puts them back.
func stubPagerSeams(t *testing.T) {
	t.Helper()
	term, argv, look, env := isTerminalWriter, pagerArgv, pagerLookPath, lookupEnv
	t.Cleanup(func() {
		isTerminalWriter, pagerArgv, pagerLookPath, lookupEnv = term, argv, look, env
	})
}

// markingPager points the pager at `sed`, which prefixes every line it
// reads, so a test can tell a paged diff from a plain one.
func markingPager(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sed"); err != nil {
		t.Skip("sed is not on PATH")
	}
	isTerminalWriter = func(io.Writer) bool { return true }
	pagerArgv = func() ([]string, bool) { return []string{"sed", `s/^/paged: /`}, true }
}

func TestPageWriterPassesThroughWhenNotTerminal(t *testing.T) {
	stubPagerSeams(t)
	var buf bytes.Buffer
	out, color, finish := pageWriter(context.Background(), &buf)
	if out != io.Writer(&buf) {
		t.Fatalf("writer changed for a non-terminal: %T", out)
	}
	if color {
		t.Error("colour requested for a non-terminal")
	}
	finish()
}

func TestPageWriterPassesThroughWhenPagingOff(t *testing.T) {
	stubPagerSeams(t)
	isTerminalWriter = func(io.Writer) bool { return true }
	lookupEnv = func(key string) (string, bool) { return "", key == "PAGER" }

	var buf bytes.Buffer
	out, _, finish := pageWriter(context.Background(), &buf)
	if out != io.Writer(&buf) {
		t.Fatalf("writer changed when paging is off: %T", out)
	}
	finish()
}

func TestPageWriterFallsBackWhenPagerMissing(t *testing.T) {
	stubPagerSeams(t)
	isTerminalWriter = func(io.Writer) bool { return true }
	pagerArgv = func() ([]string, bool) { return []string{"pilothouse-no-such-pager"}, true }
	pagerLookPath = func(string) (string, error) { return "", errors.New("not found") }

	var buf bytes.Buffer
	out, _, finish := pageWriter(context.Background(), &buf)
	if out != io.Writer(&buf) {
		t.Fatalf("writer changed when the pager is missing: %T", out)
	}
	finish()
}

func TestPageWriterPipesThroughPager(t *testing.T) {
	stubPagerSeams(t)
	markingPager(t)

	var buf bytes.Buffer
	out, _, finish := pageWriter(context.Background(), &buf)
	if _, err := io.WriteString(out, "one\ntwo\n"); err != nil {
		t.Fatal(err)
	}
	finish()
	if got, want := buf.String(), "paged: one\npaged: two\n"; got != want {
		t.Fatalf("pager produced %q, want %q", got, want)
	}
}

func TestPagerEnv(t *testing.T) {
	stubPagerSeams(t)
	for _, tc := range []struct {
		name   string
		env    map[string]string
		want   string
		wantOK bool
	}{
		{"nothing set", nil, defaultPager, true},
		{"PAGER alone", map[string]string{"PAGER": "bat"}, "bat", true},
		{"GIT_PAGER beats PAGER", map[string]string{"PAGER": "bat", "GIT_PAGER": "delta"}, "delta", true},
		{"empty PAGER disables", map[string]string{"PAGER": ""}, "", false},
		{"empty GIT_PAGER disables", map[string]string{"GIT_PAGER": ""}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := tc.env
			lookupEnv = func(key string) (string, bool) {
				v, ok := env[key]
				return v, ok
			}
			got, ok := pagerEnv()
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("pagerEnv() = %q, %v; want %q, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestPagerArgvSplitsOnWhitespace(t *testing.T) {
	stubPagerSeams(t)
	lookupEnv = func(key string) (string, bool) { return "less -FRX", key == "PAGER" }

	argv, ok := pagerArgv()
	if !ok || len(argv) != 2 || argv[0] != "less" || argv[1] != "-FRX" {
		t.Fatalf("pagerArgv() = %v, %v; want [less -FRX], true", argv, ok)
	}
}

// diffTask makes one task with a commit, an uncommitted edit and an
// untracked file, the way TestNewDiffDone does.
func diffTask(t *testing.T) (*Manager, context.Context, api.Task) {
	t.Helper()
	testEnv(t)
	repo := newRepo(t)
	agent := fakeAgentScript(t, false)
	m, _ := newManager(t)
	ctx := context.Background()
	tk, err := m.New(ctx, NewOptions{Prompt: "Add a greeting!", Agent: agent, Dir: repo})
	if err != nil {
		t.Fatal(err)
	}
	return m, ctx, tk
}

// stripANSI drops the SGR sequences git emits for --color=always. git
// colours every token on its own, so a colored line never contains the
// plain text of a diff hunk as one contiguous substring.
func stripANSI(s string) string {
	return ansiColor.ReplaceAllString(s, "")
}

func TestDiffPagesTerminalOutput(t *testing.T) {
	stubPagerSeams(t)
	markingPager(t)
	m, ctx, tk := diffTask(t)

	var d bytes.Buffer
	if err := m.Diff(ctx, tk.ID, false, &d); err != nil {
		t.Fatal(err)
	}
	var commitLine string
	for _, line := range strings.Split(d.String(), "\n") {
		if plain := stripANSI(line); strings.Contains(plain, "+task: Add a greeting!") {
			commitLine = plain
		}
	}
	if commitLine == "" {
		t.Fatalf("diff lacks the commit line:\n%s", d.String())
	}
	if !strings.HasPrefix(commitLine, "paged: ") {
		t.Errorf("terminal diff was not paged: %q", commitLine)
	}
	// git colours a diff only when its own stdout is a terminal, and
	// with a pager that stdout is the pager's pipe, so the colour has
	// to be asked for explicitly.
	if !strings.Contains(d.String(), "\x1b[") {
		t.Errorf("paged diff lost git's colouring:\n%s", d.String())
	}
}

func TestDiffLeavesPipedOutputPlain(t *testing.T) {
	stubPagerSeams(t)
	m, ctx, tk := diffTask(t)

	var d bytes.Buffer
	if err := m.Diff(ctx, tk.ID, false, &d); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.String(), "paged: ") {
		t.Errorf("piped diff was paged:\n%s", d.String())
	}
	if strings.Contains(d.String(), "\x1b[") {
		t.Errorf("piped diff was colourised:\n%s", d.String())
	}
	if !strings.Contains(d.String(), "+task: Add a greeting!") {
		t.Errorf("diff lacks the commit line:\n%s", d.String())
	}
}
