package opencode

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xasdta/pilothouse/internal/adapters"
)

func TestInstallUninstallPlugin(t *testing.T) {
	isolate(t)
	ad := Adapter{}
	opts := adapters.InstallOptions{ShimPath: "/usr/local/bin/pilothouse"}

	st, _ := ad.Status(opts)
	if st.Installed || st.Current || st.Path != PluginPath() {
		t.Fatalf("fresh status %+v", st)
	}
	// Dry run writes nothing.
	ch, err := ad.InstallHooks(adapters.InstallOptions{DryRun: true})
	if err != nil || !ch.Changed || !strings.HasPrefix(ch.Summary, "Install") {
		t.Fatalf("dry run %+v %v", ch, err)
	}
	if _, err := os.Stat(PluginPath()); err == nil {
		t.Fatal("dry run wrote the plugin")
	}
	ch, err = ad.InstallHooks(opts)
	if err != nil || !ch.Changed || ch.Backup != "" {
		t.Fatalf("install %+v %v", ch, err)
	}
	got, err := os.ReadFile(PluginPath())
	if err != nil || !bytes.Equal(got, PluginSource()) {
		t.Fatalf("installed plugin differs from the embedded one (%v)", err)
	}
	if fi, _ := os.Stat(PluginPath()); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	st, _ = ad.Status(opts)
	if !st.Installed || !st.Current || len(st.Events) == 0 || len(st.Problems) > 0 {
		t.Fatalf("status after install %+v", st)
	}
	// Idempotent.
	if ch, err := ad.InstallHooks(opts); err != nil || ch.Changed {
		t.Fatalf("second install %+v %v", ch, err)
	}

	// An older version of our plugin is replaced, without a backup.
	if err := os.WriteFile(PluginPath(), []byte("// @pilothouse-managed old\nexport default {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, _ = ad.Status(opts)
	if !st.Installed || st.Current || len(st.Problems) != 1 || !strings.Contains(st.Problems[0], "another Pilothouse version") {
		t.Fatalf("stale status %+v", st)
	}
	if ch, err := ad.InstallHooks(opts); err != nil || !ch.Changed || !strings.HasPrefix(ch.Summary, "Update") {
		t.Fatalf("update %+v %v", ch, err)
	}
	if ents, _ := os.ReadDir(filepath.Dir(PluginPath())); len(ents) != 1 {
		t.Errorf("plugins dir holds %d files, want only the plugin (no backups of our own file)", len(ents))
	}

	ch, err = ad.UninstallHooks(adapters.InstallOptions{})
	if err != nil || !ch.Changed {
		t.Fatalf("uninstall %+v %v", ch, err)
	}
	if _, err := os.Stat(PluginPath()); !os.IsNotExist(err) {
		t.Fatal("plugin still there after uninstall")
	}
	if ch, err := ad.UninstallHooks(adapters.InstallOptions{}); err != nil || ch.Changed {
		t.Fatalf("second uninstall %+v %v", ch, err)
	}
}

func TestNeverTouchesSomeoneElsesPlugin(t *testing.T) {
	isolate(t)
	ad := Adapter{}
	other := []byte("export const Mine = async () => ({})\n")
	if err := os.MkdirAll(filepath.Dir(PluginPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PluginPath(), other, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ad.InstallHooks(adapters.InstallOptions{}); err == nil || !strings.Contains(err.Error(), "not written by Pilothouse") {
		t.Fatalf("install over a foreign plugin: %v", err)
	}
	if ch, err := ad.UninstallHooks(adapters.InstallOptions{}); err != nil || ch.Changed {
		t.Fatalf("uninstall of a foreign plugin: %+v %v", ch, err)
	}
	if got, _ := os.ReadFile(PluginPath()); !bytes.Equal(got, other) {
		t.Fatal("foreign plugin modified")
	}
	st, _ := ad.Status(adapters.InstallOptions{})
	if st.Installed || len(st.Problems) != 1 {
		t.Fatalf("status %+v", st)
	}
	// Other plugins in the directory are never looked at or touched.
	herdr := filepath.Join(filepath.Dir(PluginPath()), "herdr.js")
	if err := os.WriteFile(herdr, []byte("export default {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(PluginPath()); err != nil {
		t.Fatal(err)
	}
	if _, err := ad.InstallHooks(adapters.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ad.UninstallHooks(adapters.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(herdr); err != nil || string(b) != "export default {}\n" {
		t.Fatalf("another plugin was touched: %q %v", b, err)
	}
}

func TestInstallFollowsSymlink(t *testing.T) {
	isolate(t)
	dots := filepath.Join(t.TempDir(), "pilothouse.js")
	if err := os.WriteFile(dots, []byte("// @pilothouse-managed old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(PluginPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dots, PluginPath()); err != nil {
		t.Fatal(err)
	}
	if _, err := (Adapter{}).InstallHooks(adapters.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(PluginPath()); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced by a regular file")
	}
	if b, _ := os.ReadFile(dots); !bytes.Equal(b, PluginSource()) {
		t.Fatal("symlink target not updated")
	}
}

func TestStatusReportsDuplicates(t *testing.T) {
	uh := isolate(t)
	ad := Adapter{}
	if _, err := ad.InstallHooks(adapters.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	dup := filepath.Join(uh, ".opencode", "plugins", PluginFile)
	if err := os.MkdirAll(filepath.Dir(dup), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dup, PluginSource(), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgDir := filepath.Join(uh, "occfg")
	t.Setenv("OPENCODE_CONFIG_DIR", cfgDir)
	dup2 := filepath.Join(cfgDir, "plugin", PluginFile)
	if err := os.MkdirAll(filepath.Dir(dup2), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dup2, PluginSource(), 0o644); err != nil {
		t.Fatal(err)
	}
	st, _ := ad.Status(adapters.InstallOptions{})
	if st.Current || len(st.Problems) != 2 || !strings.Contains(st.Problems[0]+st.Problems[1], dup) || !strings.Contains(st.Problems[0]+st.Problems[1], dup2) {
		t.Fatalf("duplicates not reported: %+v", st)
	}
}

func TestPluginHeader(t *testing.T) {
	src := PluginSource()
	if !ownedPlugin(src) {
		t.Fatal("embedded plugin lacks the owned marker in its header")
	}
	// The shim's stale-path check scans settings files for
	// "<path>/pilothouse hook "; the plugin must not look like one.
	if bytes.Contains(src, []byte("/pilothouse hook ")) {
		t.Fatal("plugin text looks like a hook command")
	}
	if !bytes.Contains(src, []byte(`export default { id: "pilothouse", server }`)) {
		t.Fatal("OpenCode 1.x loads a file plugin from a default export with id and server")
	}
}
