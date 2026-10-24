package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/paths"
)

func TestMachineName(t *testing.T) {
	t.Setenv("PILOTHOUSE_MACHINE", "")
	h, _ := os.Hostname()
	want := strings.TrimSuffix(h, ".local")
	if want == "" {
		want = "localhost"
	}
	if got := MachineName(nil); got != want {
		t.Fatalf("nil cfg: got %q want %q", got, want)
	}
	t.Setenv("PILOTHOUSE_MACHINE", " build-box ")
	if got := MachineName(&Config{}); got != "build-box" {
		t.Fatalf("env: got %q", got)
	}
	if got := MachineName(&Config{Machine: " studio "}); got != "studio" {
		t.Fatalf("config: got %q", got)
	}
}

func TestSessionIdleExpiry(t *testing.T) {
	t.Setenv("PILOTHOUSE_HOME", t.TempDir())
	c, err := Load()
	if err != nil || c.SessionIdleExpiry.Duration != DefaultSessionIdleExpiry {
		t.Fatalf("default = %v, %v", c.SessionIdleExpiry, err)
	}
	c.SessionIdleExpiry.Duration = 10 * time.Second
	if c.Validate() == nil {
		t.Fatal("10s accepted")
	}
	c.SessionIdleExpiry.Duration = 0
	if c.Validate() != nil {
		t.Fatal("0 (off) rejected")
	}
}

// Concurrent read-modify-writes (setup, CLI commands, the daemon pairing a
// Telegram chat) never lose each other's changes.
func TestUpdateKeepsConcurrentChanges(t *testing.T) {
	t.Setenv("PILOTHOUSE_HOME", t.TempDir())
	const n = 24
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := Update(nil, func(c *Config) error {
				c.Tasks.Repos = append(c.Tasks.Repos, fmt.Sprintf("/src/r%02d", i))
				time.Sleep(time.Millisecond) // widen the window a plain Load+Save would lose in
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	c, err := Load()
	if err != nil || len(c.Tasks.Repos) != n {
		t.Fatalf("repos = %d (%v), want %d: concurrent updates were lost", len(c.Tasks.Repos), err, n)
	}
	if fi, err := os.Stat(paths.Config()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("config mode: %v %v", fi, err)
	}
}

func TestUpdateBaseAndErrors(t *testing.T) {
	t.Setenv("PILOTHOUSE_HOME", t.TempDir())
	// No file: start from a copy of base, which itself stays untouched.
	base := Default()
	base.Notify.Telegram.APIBase = "http://127.0.0.1:1"
	base.Tasks.Repos = []string{"/a"}
	got, err := Update(base, func(c *Config) error {
		c.Notify.Telegram.ChatID = 42
		c.Tasks.Repos[0] = "/changed"
		return nil
	})
	if err != nil || got.Notify.Telegram.APIBase != "http://127.0.0.1:1" || got.Notify.Telegram.ChatID != 42 {
		t.Fatalf("got %+v, %v", got.Notify.Telegram, err)
	}
	if base.Notify.Telegram.ChatID != 0 || base.Tasks.Repos[0] != "/a" {
		t.Fatal("Update changed base")
	}
	// With a file, base is ignored: the file wins.
	other := Default()
	other.Machine = "from-base"
	if c, _ := Update(other, func(*Config) error { return nil }); c.Machine != "" || c.Notify.Telegram.ChatID != 42 {
		t.Fatalf("file ignored: %+v", c)
	}
	// fn failing saves nothing.
	if _, err := Update(nil, func(c *Config) error { c.Machine = "x"; return errors.New("no") }); err == nil {
		t.Fatal("fn error swallowed")
	}
	if c, _ := Load(); c.Machine != "" {
		t.Fatal("saved after fn failed")
	}
	// A broken file is reported, not overwritten.
	if err := os.WriteFile(paths.Config(), []byte("web: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(nil, func(*Config) error { return nil }); err == nil {
		t.Fatal("broken file accepted")
	}
	if b, _ := os.ReadFile(paths.Config()); string(b) != "web: [\n" {
		t.Fatal("broken file overwritten")
	}
}
