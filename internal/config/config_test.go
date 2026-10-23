package config

import (
	"os"
	"strings"
	"testing"
	"time"
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
