package cli

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/opencode"
)

// openCodeHome makes a user home with an OpenCode config, for the real
// OpenCode adapter.
func openCodeHome(t *testing.T) (cfg string) {
	t.Helper()
	uh := t.TempDir()
	t.Setenv("PILOTHOUSE_USER_HOME", uh)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("PATH", t.TempDir()) // no opencode binary: the config dir is enough
	cfg = filepath.Join(uh, ".config", "opencode", "opencode.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("{\n  \"theme\": \"dark\" // mine\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestSetupInstallsOpenCodePluginAndOffersPermissions(t *testing.T) {
	ibxStart(t)
	cfg := openCodeHome(t)
	inboxAdapters = func() []adapters.Adapter { return []adapters.Adapter{opencode.New()} }
	out, err := ibxRun(t, "", "setup", "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	ibxContains(t, out, "Install Pilothouse's OpenCode plugin", "OpenCode settings", "OpenCode allows bash and edits without asking",
		`+  "permission": {`, `+    "bash": "ask",`, "? Make OpenCode ask before shell commands and file edits? [Y/n] yes",
		`✓ Set permission bash and edit to "ask"`, "backup:")
	if strings.Index(out, `"bash": "ask"`) > strings.Index(out, "? Make OpenCode ask") {
		t.Fatalf("diff shown after the question:\n%s", out)
	}
	if _, err := os.Stat(opencode.PluginPath()); err != nil {
		t.Fatalf("plugin not installed: %v", err)
	}
	b, _ := os.ReadFile(cfg)
	if !strings.Contains(string(b), `"edit": "ask"`) || !strings.Contains(string(b), "// mine") {
		t.Fatalf("config:\n%s", b)
	}
	// Run again: nothing left to offer.
	out, _ = ibxRun(t, "", "setup", "--yes")
	if strings.Contains(out, "OpenCode settings") {
		t.Fatalf("offered again:\n%s", out)
	}
}

func TestOfferSuggestionsDeclined(t *testing.T) {
	ibxEnv(t)
	cfg := openCodeHome(t)
	inboxAdapters = func() []adapters.Adapter { return []adapters.Adapter{opencode.New()} }
	before, _ := os.ReadFile(cfg)
	var w bytes.Buffer
	var notes []string
	ui := &setupUI{w: &w, in: bufio.NewReader(strings.NewReader("n\n"))}
	// Without the plugin installed there is nothing to offer.
	offerSuggestions(&w, ui, adapters.InstallOptions{}, func(s string) { notes = append(notes, s) })
	if w.Len() != 0 {
		t.Fatalf("offered without the plugin:\n%s", w.String())
	}
	if _, err := opencode.New().InstallHooks(adapters.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	offerSuggestions(&w, ui, adapters.InstallOptions{}, func(s string) { notes = append(notes, s) })
	if !strings.Contains(w.String(), "? Make OpenCode ask") || len(notes) != 1 || !strings.Contains(notes[0], "left as they are") {
		t.Fatalf("%s\n%v", w.String(), notes)
	}
	if after, _ := os.ReadFile(cfg); !bytes.Equal(before, after) {
		t.Fatal("declined, but the config changed")
	}
	if ents, _ := os.ReadDir(filepath.Dir(cfg)); len(ents) != 2 { // opencode.json + plugins/
		t.Fatalf("a backup was written for a declined change: %v", ents)
	}
}

func TestDoctorMentionsOpenCodeSettings(t *testing.T) {
	ibxEnv(t)
	openCodeHome(t)
	ad := opencode.New()
	if _, err := ad.InstallHooks(adapters.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	st, _ := ad.Status(adapters.InstallOptions{})
	level, detail, fix, ok := suggestionCheck(ad, st)
	if !ok || level != checkWarn || !strings.Contains(detail, "without asking") || !strings.Contains(fix, "pilothouse setup") {
		t.Fatalf("%v %q %q %v", level, detail, fix, ok)
	}
	if _, _, _, err := ad.(adapters.Suggester).Suggest(adapters.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if level, _, _, ok := suggestionCheck(ad, st); !ok || level != checkOK {
		t.Fatalf("after applying: %v %v", level, ok)
	}
	if _, _, _, ok := suggestionCheck(ad, adapters.HookStatus{}); ok {
		t.Fatal("suggestion reported without the plugin")
	}
}
