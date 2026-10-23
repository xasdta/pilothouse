package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBrew lays out <tmp>/Cellar/pilothouse/0.1.1/bin/pilothouse and the
// <tmp>/bin/pilothouse symlink Homebrew creates; it returns the prefix.
func fakeBrew(t *testing.T, link bool) (prefix, cellarBin string) {
	t.Helper()
	prefix, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cellarBin = filepath.Join(prefix, "Cellar", "pilothouse", "0.1.1", "bin", "pilothouse")
	if err := os.MkdirAll(filepath.Dir(cellarBin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cellarBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if link {
		if err := os.Symlink("../Cellar/pilothouse/0.1.1/bin/pilothouse", filepath.Join(prefix, "bin", "pilothouse")); err != nil {
			t.Fatal(err)
		}
	}
	return prefix, cellarBin
}

func stubExe(t *testing.T, exe string, lookPath func(string) (string, error)) {
	t.Helper()
	oe, ol := inboxExecutable, inboxLookPath
	t.Cleanup(func() { inboxExecutable, inboxLookPath = oe, ol })
	inboxExecutable = func() (string, error) { return exe, nil }
	inboxLookPath = lookPath
}

func notFound(string) (string, error) { return "", errors.New("not found") }

// TestResolvedExeAvoidsCellar is the Homebrew regression: hooks and the
// service must call a path `brew upgrade` keeps, not the versioned Cellar
// binary os.Executable resolves to.
func TestResolvedExeAvoidsCellar(t *testing.T) {
	t.Run("PATH entry that is this binary", func(t *testing.T) {
		prefix, cellar := fakeBrew(t, true)
		onPath := filepath.Join(prefix, "bin", "pilothouse")
		stubExe(t, cellar, func(string) (string, error) { return onPath, nil })
		if got, err := resolvedExe(); err != nil || got != onPath {
			t.Fatalf("resolvedExe = %q, %v; want %q", got, err, onPath)
		}
		if why := unstableExe(onPath); strings.Contains(why, "brew upgrade") {
			t.Fatalf("stable path flagged as versioned: %s", why)
		}
	})
	t.Run("not on PATH: brew prefix bin", func(t *testing.T) {
		prefix, cellar := fakeBrew(t, true)
		stubExe(t, cellar, notFound)
		if got, _ := resolvedExe(); got != filepath.Join(prefix, "bin", "pilothouse") {
			t.Fatalf("resolvedExe = %q", got)
		}
	})
	t.Run("PATH has another pilothouse", func(t *testing.T) {
		prefix, cellar := fakeBrew(t, true)
		other := filepath.Join(t.TempDir(), "pilothouse")
		if err := os.WriteFile(other, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		stubExe(t, cellar, func(string) (string, error) { return other, nil })
		if got, _ := resolvedExe(); got != filepath.Join(prefix, "bin", "pilothouse") {
			t.Fatalf("resolvedExe = %q, must not pick a different binary", got)
		}
	})
	t.Run("no stable path: versioned path is refused", func(t *testing.T) {
		_, cellar := fakeBrew(t, false)
		stubExe(t, cellar, notFound)
		got, _ := resolvedExe()
		if got != cellar {
			t.Fatalf("resolvedExe = %q", got)
		}
		if why := unstableExe(got); !strings.Contains(why, "brew upgrade") {
			t.Fatalf("unstableExe = %q", why)
		}
	})
	t.Run("plain install resolves symlinks", func(t *testing.T) {
		dir, _ := filepath.EvalSymlinks(t.TempDir())
		bin := filepath.Join(dir, "pilothouse")
		if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "ph-link")
		if err := os.Symlink(bin, link); err != nil {
			t.Fatal(err)
		}
		stubExe(t, link, notFound)
		if got, _ := resolvedExe(); got != bin {
			t.Fatalf("resolvedExe = %q, want %q", got, bin)
		}
	})
}

func TestStaleHookProblems(t *testing.T) {
	prefix, cellar := fakeBrew(t, true)
	stable := filepath.Join(prefix, "bin", "pilothouse")
	settings := filepath.Join(t.TempDir(), "settings.json")
	write := func(body string) {
		if err := os.WriteFile(settings, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"` + stable + ` hook claude"}]}]}}`)
	if p := StaleHookProblems(settings); len(p) != 0 {
		t.Fatalf("current hooks flagged: %v", p)
	}
	write(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"` + cellar + ` hook claude"}]}]}}`)
	if p := StaleHookProblems(settings); len(p) != 1 || !strings.Contains(p[0], "stale") || !strings.Contains(p[0], "Homebrew") {
		t.Fatalf("Cellar hooks: %v", p)
	}
	write(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"'/gone/dir with space/pilothouse' hook claude"}]}]}}`)
	if p := StaleHookProblems(settings); len(p) != 1 || !strings.Contains(p[0], "doesn't exist") {
		t.Fatalf("missing binary: %v", p)
	}
	if p := StaleHookProblems(filepath.Join(t.TempDir(), "none.json")); p != nil {
		t.Fatalf("missing file: %v", p)
	}
}
