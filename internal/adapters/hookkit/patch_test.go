package hookkit

import (
	"strings"
	"testing"
)

// TestShellPatch: Codex models without an apply_patch tool patch through
// the shell (`apply_patch <<'EOF'`), and path rules must still see the
// files (M10). Commands are shaped like the ones real Codex 0.164 sent in
// tests/e2e/codex (scenario=patch_env).
func TestShellPatch(t *testing.T) {
	const cwd = "/repo"
	patch := "*** Begin Patch\n*** Add File: .env.local\n+x\n*** Update File: src/a.go\n*** Move to: src/b.go\n@@\n-a\n+b\n*** Delete File: old.txt\n*** End Patch\n"
	all := "/repo/.env.local,/repo/src/a.go,/repo/src/b.go,/repo/old.txt"
	for _, tc := range []struct {
		name, cmd string
		paths     string
		exact     bool
	}{
		{"quoted heredoc", "apply_patch <<'EOF'\n" + patch + "EOF\n", all, true},
		{"double-quoted, applypatch", "applypatch <<\"PATCH\"\n" + patch + "PATCH", all, true},
		{"bare delimiter", "apply_patch <<EOF\n" + patch + "EOF\n\n", all, true},
		{"cd form", "cd sub && apply_patch <<'EOF'\n" + patch + "EOF\n",
			"/repo/sub/.env.local,/repo/sub/src/a.go,/repo/sub/src/b.go,/repo/sub/old.txt", true},
		{"cd absolute", "cd /other && apply_patch <<'EOF'\n*** Begin Patch\n*** Update File: .env\n*** End Patch\nEOF", "/other/.env", true},
		{"cd home", "cd ~/w && apply_patch <<'EOF'\n*** Begin Patch\n*** Update File: .env\n*** End Patch\nEOF", "~/w/.env", true},
		{"absolute patch path", "apply_patch <<'EOF'\n*** Begin Patch\n*** Update File: /etc/hosts\n*** End Patch\nEOF\n", "/etc/hosts", true},
		// Not the whole command: Codex runs it in the shell; paths are a
		// best-effort extra and the call stays a shell command.
		{"trailing command", "apply_patch <<'EOF'\n" + patch + "EOF\necho done\n", all, false},
		{"leading command", "git status && apply_patch <<'EOF'\n" + patch + "EOF\n", all, false},
		{"mismatched quotes", "apply_patch <<'EOF\"\n" + patch + "EOF\n", all, false},
		{"unterminated", "apply_patch <<'EOF'\n" + patch, all, false},
		// Not a patch.
		{"no envelope", "apply_patch <<'EOF'\nhello\nEOF\n", "", false},
		{"patch text without apply_patch", "cat <<'EOF'\n" + patch + "EOF\n", "", false},
		{"apply_patch_helper is another program", "apply_patch_helper <<'EOF'\n" + patch + "EOF\n", "", false},
		{"plain command", "git push origin main", "", false},
	} {
		paths, exact := ShellPatch(tc.cmd, cwd)
		if strings.Join(paths, ",") != tc.paths || exact != tc.exact {
			t.Errorf("%s: paths=%v exact=%v, want %s %v", tc.name, paths, exact, tc.paths, tc.exact)
		}
	}
}
