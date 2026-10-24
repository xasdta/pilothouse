package opencode

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/hookkit"
)

// pluginJS is the plugin installed into OpenCode's plugins directory,
// byte for byte.
//
//go:embed plugin/pilothouse.js
var pluginJS []byte

// PluginSource returns the plugin as installed (for tests and docs).
func PluginSource() []byte { return bytes.Clone(pluginJS) }

// ownedMarker is in the plugin's header comment: a pilothouse.js without
// it is someone else's plugin, and Pilothouse never touches it.
const ownedMarker = "@pilothouse-managed"

func ownedPlugin(data []byte) bool {
	head := data
	if len(head) > 1024 {
		head = head[:1024]
	}
	return bytes.Contains(head, []byte(ownedMarker))
}

// pluginEvents are the OpenCode hooks and events the plugin forwards
// (reported by Status).
var pluginEvents = []string{
	"tool.execute.before", "tool.execute.after", "chat.message",
	"permission.asked", "permission.replied", "question.asked", "question.replied", "question.rejected",
	"session.created", "session.updated", "session.idle", "session.deleted", "experimental.session.compacting",
}

// InstallHooks writes the plugin to <config>/plugins/pilothouse.js. It is
// idempotent, refuses to replace a pilothouse.js Pilothouse did not write,
// and follows a symlinked file (dotfiles). The previous version of our own
// plugin is replaced without a backup: there is nothing of the person's in
// it.
func (Adapter) InstallHooks(opts adapters.InstallOptions) (adapters.Change, error) {
	f, err := hookkit.ReadFile(PluginPath())
	if err != nil {
		return adapters.Change{}, err
	}
	if f.Exists && !ownedPlugin(f.Data) {
		return adapters.Change{}, fmt.Errorf("%s exists and was not written by Pilothouse; move it away and run this again", f.Path)
	}
	ch := adapters.Change{Path: f.Path, Changed: !f.Exists || !bytes.Equal(f.Data, pluginJS)}
	switch {
	case !ch.Changed:
		ch.Summary = "Pilothouse's OpenCode plugin is up to date in " + f.Path
	case f.Exists:
		ch.Summary = "Update Pilothouse's OpenCode plugin in " + f.Path
	default:
		ch.Summary = "Install Pilothouse's OpenCode plugin as " + f.Path
	}
	if opts.DryRun || !ch.Changed {
		return ch, nil
	}
	// Exists=false: no backup of our own previous version.
	_, err = hookkit.Commit(hookkit.File{Path: f.Path, Real: f.Real, Mode: 0o644}, pluginJS, time.Now())
	return ch, err
}

// UninstallHooks removes the plugin file if it is ours (and nothing else:
// permission settings setup changed stay, they are the person's now).
func (Adapter) UninstallHooks(opts adapters.InstallOptions) (adapters.Change, error) {
	path := PluginPath()
	f, err := hookkit.ReadFile(path)
	if err != nil {
		return adapters.Change{}, err
	}
	ch := adapters.Change{Path: path}
	switch {
	case !f.Exists:
		ch.Summary = "No Pilothouse plugin in " + filepath.Dir(path)
		return ch, nil
	case !ownedPlugin(f.Data):
		ch.Summary = path + " is not Pilothouse's plugin; left alone"
		return ch, nil
	}
	ch.Changed = true
	ch.Summary = "Remove Pilothouse's OpenCode plugin " + path
	if opts.DryRun {
		return ch, nil
	}
	// Remove the entry OpenCode loads (a symlink stays a symlink's business:
	// the link goes, its target is left).
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return ch, err
	}
	return ch, nil
}

// Status reports whether the plugin is installed, whether it is the one
// this build ships, and copies that would make OpenCode load it twice.
func (Adapter) Status(opts adapters.InstallOptions) (adapters.HookStatus, error) {
	st := adapters.HookStatus{Path: PluginPath()}
	f, err := hookkit.ReadFile(st.Path)
	if err != nil {
		st.Problems = append(st.Problems, err.Error())
		return st, nil
	}
	switch {
	case !f.Exists:
	case !ownedPlugin(f.Data):
		st.Problems = append(st.Problems, st.Path+" exists but is not Pilothouse's plugin; move it away, then run `pilothouse hooks install --agent opencode`")
	default:
		st.Installed = true
		st.Events = append([]string(nil), pluginEvents...)
		if !bytes.Equal(f.Data, pluginJS) {
			st.Problems = append(st.Problems, "the plugin is from another Pilothouse version; run `pilothouse hooks install --agent opencode`")
		}
	}
	for _, p := range duplicatePlugins() {
		st.Problems = append(st.Problems, "a second copy of the plugin in "+p+" makes OpenCode load it twice; remove that file")
	}
	st.Current = st.Installed && len(st.Problems) == 0
	return st, nil
}

// duplicatePlugins lists Pilothouse plugin files in the other directories
// OpenCode loads plugins from.
func duplicatePlugins() []string {
	var out []string
	cands := []string{filepath.Join(ConfigDir(), "plugin", PluginFile)}
	for _, d := range extraDirs() {
		cands = append(cands, filepath.Join(d, "plugins", PluginFile), filepath.Join(d, "plugin", PluginFile))
	}
	seen := map[string]bool{PluginPath(): true}
	for _, p := range cands {
		if seen[p] {
			continue
		}
		seen[p] = true
		if b, err := os.ReadFile(p); err == nil && ownedPlugin(b) {
			out = append(out, p)
		}
	}
	return out
}
