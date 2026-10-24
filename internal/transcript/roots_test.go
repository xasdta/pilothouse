package transcript

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

// Transcript paths come from hook payloads, which the agent controls.
// Only regular .jsonl files inside the agent's own folder may be read.
func TestConfinement(t *testing.T) {
	e := newEnv(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.jsonl")
	writeFile(t, secret, `{"type":"user","message":{"role":"user","content":"secret"}}`+"\n")
	good := e.claudePath("sess-ok")
	writeFile(t, good, "{}\n")
	proj := filepath.Dir(good)

	link := filepath.Join(proj, "escape.jsonl")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	dirLink := filepath.Join(proj, "linkdir")
	if err := os.Symlink(outside, dirLink); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(proj, "inner.jsonl")
	writeFile(t, inner, "{}\n")
	toInner := filepath.Join(proj, "alias.jsonl")
	if err := os.Symlink(inner, toInner); err != nil {
		t.Fatal(err)
	}
	notJSONL := filepath.Join(proj, "notes.txt")
	writeFile(t, notJSONL, "x\n")
	toText := filepath.Join(proj, "text.jsonl")
	if err := os.Symlink(notJSONL, toText); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(proj, "pipe.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(proj, "dir.jsonl")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	roots := e.opts.Roots.Claude
	cases := []struct {
		name string
		path string
		want error // nil: allowed
	}{
		{"regular file inside", good, nil},
		{"symlink to a file inside", toInner, nil},
		{"symlink escaping the root", link, ErrOutside},
		{"through a symlinked directory", filepath.Join(dirLink, "secret.jsonl"), ErrOutside},
		{"dot-dot out of the root", filepath.Join(proj, "..", "..", "..", filepath.Base(outside), "secret.jsonl"), ErrOutside},
		{"dot-dot staying inside", filepath.Join(proj, "..", filepath.Base(proj), "sess-ok.jsonl"), nil},
		{"file outside", secret, ErrOutside},
		{"not .jsonl", notJSONL, ErrOutside},
		{"symlink to a non-jsonl file", toText, ErrOutside},
		{"relative path", "projects/x/sess-ok.jsonl", ErrOutside},
		{"empty", "", ErrOutside},
		{"NUL byte", good + "\x00.jsonl", ErrOutside},
		{"the root itself", e.claude + ".jsonl", ErrOutside},
		{"missing inside", filepath.Join(proj, "nope.jsonl"), ErrMissing},
		{"missing outside", filepath.Join(outside, "nope.jsonl"), ErrOutside},
		{"FIFO", fifo, ErrOutside},
		{"directory", dir, ErrOutside},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				f, _, err := Open(c.path, roots)
				if f != nil {
					f.Close()
				}
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, c.want) && !(c.want == nil && err == nil) {
					t.Errorf("Open(%q) = %v, want %v", c.path, err, c.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("Open(%q) blocked", c.path)
			}
		})
	}

	// A Claude transcript path is never read as a Codex rollout and back.
	if _, _, err := Open(good, e.opts.Roots.Codex); !errors.Is(err, ErrOutside) {
		t.Errorf("claude transcript under codex roots = %v", err)
	}
	r := e.reader()
	if _, err := r.Read("x", Source{Agent: api.AgentCodex, Path: good}, Query{}); !errors.Is(err, ErrOutside) {
		t.Errorf("reader with the wrong agent = %v", err)
	}
	if _, err := r.Read("y", Source{Agent: api.AgentGeneric, Path: good}, Query{}); !errors.Is(err, ErrOutside) {
		t.Errorf("reader for an agent without transcripts = %v", err)
	}
	if _, err := r.Read("z", Source{Agent: api.AgentClaude, Path: link}, Query{}); !errors.Is(err, ErrOutside) {
		t.Errorf("reader through an escaping symlink = %v", err)
	}
}

// The root itself may be a symlink (a dotfiles-managed ~/.claude).
func TestSymlinkedRoot(t *testing.T) {
	real := t.TempDir()
	home := t.TempDir()
	if err := os.Symlink(real, filepath.Join(home, ".claude")); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(real, "projects", "-x", "s.jsonl")
	writeFile(t, p, "{}\n")
	roots := []string{filepath.Join(home, ".claude", "projects")}
	viaLink := filepath.Join(home, ".claude", "projects", "-x", "s.jsonl")
	if f, _, err := Open(viaLink, roots); err != nil {
		t.Errorf("open through a symlinked root: %v", err)
	} else {
		f.Close()
	}
	if f, _, err := Open(p, roots); err != nil {
		t.Errorf("open by its real path: %v", err)
	} else {
		f.Close()
	}
}

// A subagent file that is a symlink out of the root is skipped.
func TestSidechainConfinement(t *testing.T) {
	e := newEnv(t)
	path := e.claudePath("sess-sc")
	c := newClaudeLog()
	c.prompt("go", "human")
	writeFile(t, path, c.String())
	outside := filepath.Join(t.TempDir(), "agent-x.jsonl")
	s := newClaudeLog()
	s.sidechain = true
	s.text("msg_x", "m-known", "leaked", usage(1000, 1000, 0, 0))
	writeFile(t, outside, s.String())
	sub := filepath.Join(path[:len(path)-len(".jsonl")], "subagents")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(sub, "agent-x.jsonl")); err != nil {
		t.Fatal(err)
	}
	res := mustRead(t, e.reader(), "k", Source{Agent: api.AgentClaude, Path: path}, Query{})
	if len(res.Items) != 1 || res.Usage.InputTokens != 0 {
		t.Errorf("a symlinked subagent file outside the root was read: %+v", res)
	}
}

func TestDefaultRoots(t *testing.T) {
	uh := t.TempDir()
	t.Setenv("PILOTHOUSE_USER_HOME", uh)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "/etc/somewhere-else")
	r := DefaultRoots()
	if len(r.Claude) != 1 || r.Claude[0] != filepath.Join(uh, ".claude", "projects") {
		t.Errorf("CLAUDE_CONFIG_DIR outside the test home must be ignored: %v", r.Claude)
	}
	if len(r.Codex) != 1 || r.Codex[0] != filepath.Join(uh, ".codex", "sessions") {
		t.Errorf("codex roots = %v", r.Codex)
	}
	inside := filepath.Join(uh, "alt-claude")
	t.Setenv("CLAUDE_CONFIG_DIR", inside)
	r = DefaultRoots()
	if len(r.Claude) != 2 || r.Claude[1] != filepath.Join(inside, "projects") {
		t.Errorf("CLAUDE_CONFIG_DIR inside the home counts: %v", r.Claude)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "relative/dir")
	if r = DefaultRoots(); len(r.Claude) != 1 {
		t.Errorf("a relative CLAUDE_CONFIG_DIR must be ignored: %v", r.Claude)
	}
}
