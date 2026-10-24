package cli

import (
	"fmt"
	"io"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/tui"
)

// Agent settings Pilothouse suggests besides its hooks (adapters.Suggester;
// OpenCode: ask before shell commands and edits). Setup shows the exact
// change and applies it only when the person says yes; doctor mentions an
// open suggestion.

// offerSuggestions asks, for every agent whose hooks are in place, whether
// to apply the settings change its adapter suggests.
func offerSuggestions(w io.Writer, ui *setupUI, opts adapters.InstallOptions, note func(string)) {
	for _, a := range liveAdapters() {
		sg, ok := a.(adapters.Suggester)
		if !ok || !a.Detect().Installed {
			continue
		}
		if st, err := a.Status(opts); err != nil || !st.Installed {
			continue // without the hooks the settings change does nothing for Pilothouse
		}
		dry := opts
		dry.DryRun = true
		ch, question, why, err := sg.Suggest(dry)
		if err != nil {
			fmt.Fprintf(w, "\n  ! %s settings: %v\n", a.DisplayName(), err)
			note("! " + a.DisplayName() + " settings: " + err.Error())
			continue
		}
		if !ch.Changed {
			continue
		}
		fmt.Fprintf(w, "\n%s settings\n", a.DisplayName())
		fmt.Fprintf(w, "  %s\n", why)
		fmt.Fprintf(w, "  • %s\n", ch.Summary)
		fmt.Fprintf(w, "      file: %s (a backup is written first)\n", tui.ShortPath(ch.Path))
		printSetupDiff(w, ch)
		if !ui.ask(question) {
			note("· " + a.DisplayName() + " settings left as they are")
			continue
		}
		ch, _, _, err = sg.Suggest(opts)
		if err != nil {
			fmt.Fprintf(w, "  ✗ %v\n", err)
			note("✗ " + a.DisplayName() + " settings: " + err.Error())
			continue
		}
		msg := "  ✓ " + ch.Summary
		if ch.Backup != "" {
			msg += " (backup: " + tui.ShortPath(ch.Backup) + ")"
		}
		fmt.Fprintln(w, msg)
		note("✓ " + a.DisplayName() + " settings updated")
	}
}

// suggestionCheck is doctor's line about an open settings suggestion for
// an agent whose hooks are installed; ok is false when there is none.
func suggestionCheck(a adapters.Adapter, st adapters.HookStatus) (level checkLevel, detail, fix string, ok bool) {
	sg, is := a.(adapters.Suggester)
	if !is || !st.Installed {
		return 0, "", "", false
	}
	ch, _, why, err := sg.Suggest(adapters.InstallOptions{DryRun: true})
	switch {
	case err != nil:
		return checkWarn, err.Error(), "", true
	case ch.Changed:
		return checkWarn, why, "`pilothouse setup` (it shows the change to " + tui.ShortPath(ch.Path) + " and asks first)", true
	}
	return checkOK, ch.Summary, "", true
}
