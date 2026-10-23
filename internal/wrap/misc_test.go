package wrap

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/xasdta/pilothouse/internal/api"
)

func TestRing(t *testing.T) {
	r := newRing(8)
	r.Write([]byte("abc"))
	if got := string(r.Bytes()); got != "abc" {
		t.Fatalf("got %q", got)
	}
	r.Write([]byte("defgh")) // exactly full
	if got := string(r.Bytes()); got != "abcdefgh" {
		t.Fatalf("full: got %q", got)
	}
	r.Write([]byte("ij"))
	if got := string(r.Bytes()); got != "cdefghij" {
		t.Fatalf("wrapped: got %q", got)
	}
	r.Write([]byte("0123456789xyz")) // larger than the ring
	if got := string(r.Bytes()); got != "6789xyz" && got != "56789xyz" {
		t.Fatalf("oversize: got %q", got)
	}
	// After wrapping, replay starts after the first newline near the front.
	r = newRing(16)
	r.Write([]byte("partial line\nwhole line\n"))
	if got := string(r.Bytes()); got != "whole line\n" {
		t.Fatalf("newline trim: got %q", got)
	}
	// Many small writes keep exactly the last N bytes.
	r = newRing(ScrollbackSize)
	var all bytes.Buffer
	for i := 0; i < 20000; i++ {
		s := strings.Repeat("x", i%37) + "|"
		r.Write([]byte(s))
		all.WriteString(s)
	}
	want := all.Bytes()[all.Len()-ScrollbackSize:]
	got := r.Bytes()
	if !bytes.HasSuffix(want, got) || len(got) < ScrollbackSize-4096 {
		t.Fatalf("ring kept %d bytes that are not the stream's tail", len(got))
	}
}

func TestKindFromCommand(t *testing.T) {
	for cmd, want := range map[string]api.AgentKind{
		"claude":                       api.AgentClaude,
		"/opt/homebrew/bin/claude":     api.AgentClaude,
		"codex":                        api.AgentCodex,
		"/usr/local/bin/Codex":         api.AgentCodex,
		"/bin/sh":                      api.AgentGeneric,
		"claude-wrapper":               api.AgentGeneric,
		"/home/me/node_modules/.bin/x": api.AgentGeneric,
	} {
		if got := KindFromCommand(cmd); got != want {
			t.Errorf("%s: got %s, want %s", cmd, got, want)
		}
	}
}

func TestNewID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewSessionID()
		if len(id) != 12 || !strings.HasPrefix(id, "s_") || strings.ToLower(id) != id {
			t.Fatalf("bad id %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestScanDetach(t *testing.T) {
	type step struct {
		in      string
		wantOut string
		wantAct detachAction
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{"plain input", []step{{"ls\r", "ls\r", actNone}}},
		{"single press arms", []step{{"ab\x1c", "ab", actArm}}},
		{"double press in one read sends one literal", []step{{"\x1c\x1c", "\x1c", actDisarm}}},
		{"double press across reads", []step{{"\x1c", "", actArm}, {"\x1c", "\x1c", actDisarm}}},
		{"press then other key detaches", []step{{"\x1c", "", actArm}, {"q", "", actDetach}}},
		{"press then other key in one read", []step{{"x\x1cyz", "x", actDetach}}},
		{"triple press: literal then armed", []step{{"\x1c\x1c\x1c", "\x1c", actArm}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			armed := false
			for i, s := range tt.steps {
				out, act := scanDetach([]byte(s.in), &armed)
				if string(out) != s.wantOut || act != s.wantAct {
					t.Fatalf("step %d: got %q/%v, want %q/%v", i, out, act, s.wantOut, s.wantAct)
				}
			}
		})
	}
}

func TestModeTracker(t *testing.T) {
	m := newModeTracker()
	if len(m.restore()) != 0 || len(m.undo()) != 0 {
		t.Fatal("fresh tracker should be a no-op")
	}
	// A TUI starts: alt screen, hidden cursor, mouse, bracketed paste,
	// kitty keyboard push — split across reads.
	m.Feed([]byte("\x1b[?1049h\x1b[?25l\x1b[?10"))
	m.Feed([]byte("00;1006h\x1b[?2004h\x1b[>1u text \x1b[1;31m"))
	restore := string(m.restore())
	for _, want := range []string{"\x1b[?1049h", "\x1b[?25l", "\x1b[?1000h", "\x1b[?1006h", "\x1b[?2004h", "\x1b[>1u"} {
		if !strings.Contains(restore, want) {
			t.Errorf("restore %q lacks %q", restore, want)
		}
	}
	if !strings.HasPrefix(restore, "\x1b[?1049h") {
		t.Errorf("restore should enter the alternate screen first: %q", restore)
	}
	undo := string(m.undo())
	for _, want := range []string{"\x1b[<1u", "\x1b[?25h", "\x1b[?1000l", "\x1b[?1006l", "\x1b[?2004l", "\x1b[?1049l", "\x1b[0m"} {
		if !strings.Contains(undo, want) {
			t.Errorf("undo %q lacks %q", undo, want)
		}
	}
	if strings.Index(undo, "\x1b[?1049l") < strings.Index(undo, "\x1b[?25h") {
		t.Errorf("undo should leave the alternate screen last: %q", undo)
	}
	// The TUI cleans up after itself: nothing left to undo.
	m.Feed([]byte("\x1b[<u\x1b[?2004l\x1b[?1000;1006l\x1b[?25h\x1b[?1049l"))
	if u := m.undo(); len(u) != 0 {
		t.Fatalf("undo after cleanup = %q", u)
	}
	// RIS resets everything.
	m.Feed([]byte("\x1b[?1049h\x1bc"))
	if u := m.undo(); len(u) != 0 {
		t.Fatalf("undo after RIS = %q", u)
	}
	// Untracked and malformed sequences are ignored.
	m.Feed([]byte("\x1b[?9999h\x1b[?h\x1b[? 1049h\x1b[2J"))
	if u := m.undo(); len(u) != 0 {
		t.Fatalf("undo after junk = %q", u)
	}
}

// H5: wrapped Codex runs without its shared app-server daemon, so hooks
// inherit PILOTHOUSE_SESSION_ID. --no-daemon goes only where Codex accepts it.
func TestCodexNoDaemonArgv(t *testing.T) {
	const c = "/bin/codex"
	tests := []struct {
		in   []string
		want []string // nil: unchanged
	}{
		{[]string{c}, []string{c, "--no-daemon"}},
		{[]string{c, "fix the login bug"}, []string{c, "--no-daemon", "fix the login bug"}},
		{[]string{c, "--no-alt-screen", "-a", "on-request"}, []string{c, "--no-daemon", "--no-alt-screen", "-a", "on-request"}},
		{[]string{c, "-m", "resume", "hi there"}, []string{c, "--no-daemon", "-m", "resume", "hi there"}},
		{[]string{c, "-c", "model=x", "resume", "--last"}, []string{c, "-c", "model=x", "resume", "--no-daemon", "--last"}},
		{[]string{c, "fork", "abc"}, []string{c, "fork", "--no-daemon", "abc"}},
		{[]string{c, "--", "exec"}, []string{c, "--no-daemon", "--", "exec"}},
		{[]string{c, "--model=o3", "do it."}, []string{c, "--no-daemon", "--model=o3", "do it."}},
		{[]string{c, "exec", "hi"}, nil},
		{[]string{c, "agents"}, nil},
		{[]string{c, "queue", "x", "y"}, nil},
		{[]string{c, "remote-control"}, nil},
		{[]string{c, "some-hidden-cmd"}, nil},
		{[]string{c, "--remote", "ws://x:1"}, nil},
		{[]string{c, "--no-daemon"}, nil},
		{[]string{c, "--version"}, nil},
	}
	for _, tt := range tests {
		got, ok := codexNoDaemonArgv(tt.in)
		want := tt.want
		if want == nil {
			want = tt.in
		}
		if ok != (tt.want != nil) || !slices.Equal(got, want) {
			t.Errorf("%q: got %q ok=%v, want %q", tt.in, got, ok, want)
		}
	}
}

// The holder adds --no-daemon only for a Codex that advertises it.
func TestHolderCodexNoDaemon(t *testing.T) {
	testEnv(t)
	dir := t.TempDir()
	for name, help := range map[string]string{"new/codex": "  --no-daemon  Run without the shared background server", "old/codex": "  --no-alt-screen"} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		script := "#!/bin/sh\nif [ \"$1\" = --help ]; then echo '" + help + "'; exit 0; fi\necho \"args:$*\"; read x\n"
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range map[string]string{"new/codex": "args:--no-daemon -a never", "old/codex": "args:-a never"} {
		h := startHolder(t, HolderConfig{Argv: []string{filepath.Join(dir, name), "-a", "never"}})
		s := attachSession(t, h.sock, true)
		s.waitOutput(want)
		s.send("\r")
		h.wait(t)
	}
}
