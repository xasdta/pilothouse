package hookkit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var owned = OwnedBy("claude")

func specs(shim string) []Spec {
	cmd := ShimCommand(shim, "claude")
	return []Spec{
		{Event: "PreToolUse", Matcher: "*", Command: cmd, Timeout: 30},
		{Event: "Stop", Command: cmd},
	}
}

const userSettings = `{
  "model": "opus",
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "/home/user/bin/lint.sh"
          }
        ]
      }
    ]
  },
  "permissions": {
    "allow": ["Bash(git status)"]
  }
}
`

func mustApply(t *testing.T, doc string, s []Spec) Result {
	t.Helper()
	res, err := Apply([]byte(doc), s, owned)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func decode(t *testing.T, doc []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(doc, &m); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, doc)
	}
	return m
}

func TestApplyEmptyDocument(t *testing.T) {
	for _, doc := range []string{"", "  \n", "{}"} {
		res := mustApply(t, doc, specs("/usr/local/bin/pilothouse"))
		if !res.Changed {
			t.Fatalf("%q: not changed", doc)
		}
		got := string(res.Doc)
		want := `{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "/usr/local/bin/pilothouse hook claude",
            "timeout": 30
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "/usr/local/bin/pilothouse hook claude"
          }
        ]
      }
    ]
  }
}
`
		if got != want {
			t.Errorf("%q:\n%s", doc, got)
		}
		if want := map[string]Ref{"PreToolUse": {"PreToolUse", 0, 0}, "Stop": {"Stop", 0, 0}}; !reflect.DeepEqual(res.Placed, want) {
			t.Errorf("Placed = %v", res.Placed)
		}
	}
}

func TestApplyPreservesUserContentAndIsIdempotent(t *testing.T) {
	res := mustApply(t, userSettings, specs("/usr/local/bin/pilothouse"))
	if !res.Changed {
		t.Fatal("not changed")
	}
	out := string(res.Doc)
	// Key order is kept: model, hooks, permissions.
	if !(strings.Index(out, `"model"`) < strings.Index(out, `"hooks"`) && strings.Index(out, `"hooks"`) < strings.Index(out, `"permissions"`)) {
		t.Errorf("key order changed:\n%s", out)
	}
	m := decode(t, res.Doc)
	pre := m["hooks"].(map[string]any)["PreToolUse"].([]any)
	if len(pre) != 2 || pre[0].(map[string]any)["matcher"] != "Bash" {
		t.Fatalf("user group not first: %v", pre)
	}
	if res.Placed["PreToolUse"] != (Ref{"PreToolUse", 1, 0}) {
		t.Errorf("Placed = %v", res.Placed)
	}
	if m["permissions"].(map[string]any)["allow"].([]any)[0] != "Bash(git status)" {
		t.Error("permissions lost")
	}

	again := mustApply(t, out, specs("/usr/local/bin/pilothouse"))
	if again.Changed || string(again.Doc) != out {
		t.Errorf("second apply changed the document:\n%s", Diff("s", out, string(again.Doc)))
	}
	if len(again.Found) != 2 || !again.Found[0].Current {
		t.Errorf("Found = %+v", again.Found)
	}

	// A user group added after ours must not make us move.
	m["hooks"].(map[string]any)["PreToolUse"] = append(pre, map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "late.sh"}}})
	b, _ := json.MarshalIndent(m, "", "  ")
	if res := mustApply(t, string(b), specs("/usr/local/bin/pilothouse")); res.Changed {
		t.Error("reordering on reinstall")
	}
}

func TestApplyReplacesStaleShim(t *testing.T) {
	first := mustApply(t, userSettings, specs("/old/pilothouse"))
	second := mustApply(t, string(first.Doc), specs("/new/pilothouse"))
	if !second.Changed {
		t.Fatal("not changed")
	}
	out := string(second.Doc)
	if strings.Contains(out, "/old/pilothouse") || strings.Count(out, "/new/pilothouse hook claude") != 2 {
		t.Errorf("stale entry kept:\n%s", out)
	}
	if !strings.Contains(out, "lint.sh") {
		t.Error("user hook lost")
	}
}

func TestUninstall(t *testing.T) {
	installed := mustApply(t, userSettings, specs("/usr/local/bin/pilothouse"))
	res := mustApply(t, string(installed.Doc), nil)
	if !res.Changed {
		t.Fatal("not changed")
	}
	if !sameJSON(res.Doc, []byte(userSettings)) {
		t.Errorf("uninstall did not restore:\n%s", res.Doc)
	}
	if again := mustApply(t, string(res.Doc), nil); again.Changed {
		t.Error("second uninstall changed")
	}
	// Only-ours: the hooks key disappears entirely.
	only := mustApply(t, `{"model":"x"}`, specs("/a/pilothouse"))
	gone := mustApply(t, string(only.Doc), nil)
	if m := decode(t, gone.Doc); m["hooks"] != nil || m["model"] != "x" {
		t.Errorf("got %s", gone.Doc)
	}
}

func TestApplySharedGroupAndMoved(t *testing.T) {
	doc := `{"hooks":{"Stop":[
	  {"hooks":[{"type":"command","command":"/x/pilothouse hook claude"},{"type":"command","command":"say done"}]},
	  {"hooks":[{"type":"command","command":"/x/pilothouse hook claude","timeout":5}]},
	  {"hooks":[{"type":"command","command":"notify.sh"}]}
	]}}`
	res := mustApply(t, doc, nil)
	want := map[Ref]Ref{
		{"Stop", 0, 1}: {"Stop", 0, 0},
		{"Stop", 2, 0}: {"Stop", 1, 0},
	}
	if !reflect.DeepEqual(res.Moved, want) {
		t.Errorf("Moved = %v", res.Moved)
	}
	stop := decode(t, res.Doc)["hooks"].(map[string]any)["Stop"].([]any)
	if len(stop) != 2 {
		t.Errorf("Stop = %v", stop)
	}
	if len(res.Found) != 2 {
		t.Errorf("Found = %+v", res.Found)
	}
}

func TestApplyRefusesBadInput(t *testing.T) {
	for _, doc := range []string{`{"hooks": `, `[1,2]`, `{"hooks": []}`, `{"hooks": {"PreToolUse": {}}}`, `{} {}`} {
		if _, err := Apply([]byte(doc), specs("/a/pilothouse"), owned); err == nil {
			t.Errorf("%q accepted", doc)
		}
	}
	// A malformed event we do not touch is left alone.
	res := mustApply(t, `{"hooks": {"Weird": 7}}`, specs("/a/pilothouse"))
	if decode(t, res.Doc)["hooks"].(map[string]any)["Weird"] != 7.0 {
		t.Error("foreign value lost")
	}
}

func TestDetectIndent(t *testing.T) {
	res := mustApply(t, "{\n\t\"a\": 1\n}\n", specs("/a/pilothouse"))
	if !strings.Contains(string(res.Doc), "\n\t\"a\": 1,\n\t\"hooks\"") {
		t.Errorf("tab indent not kept:\n%s", res.Doc)
	}
}

func TestOwnedBy(t *testing.T) {
	for cmd, want := range map[string]bool{
		"/usr/local/bin/pilothouse hook claude":     true,
		"pilothouse hook claude":                    true,
		"'/Users/a b/pilothouse' hook claude":       true,
		"/tmp/go-build1/pilothouse-dev hook claude": true,
		"/usr/local/bin/pilothouse hook codex":      false,
		"/usr/local/bin/pilothouse hook claudette":  false,
		"/usr/local/bin/notpilothouse hook claude":  false,
		"echo pilothouse":                           false,
		"/opt/pilothouse/bin/other hook claude":     false,
		"/usr/local/bin/pilothouse  hook  claude ":  true,
	} {
		if got := owned(cmd); got != want {
			t.Errorf("owned(%q) = %v, want %v", cmd, got, want)
		}
	}
}

func TestShellQuoteAndProgram(t *testing.T) {
	for in, want := range map[string]string{
		"/usr/local/bin/pilothouse": "/usr/local/bin/pilothouse",
		"/Users/a b/pilothouse":     "'/Users/a b/pilothouse'",
		"/it's/pilothouse":          `'/it'\''s/pilothouse'`,
		"":                          "''",
	} {
		q := ShellQuote(in)
		if q != want {
			t.Errorf("ShellQuote(%q) = %q, want %q", in, q, want)
		}
		if in != "" {
			if got := commandProgram(q + " hook claude"); got != in {
				t.Errorf("commandProgram(%q) = %q", q, got)
			}
		}
	}
}

func TestDiff(t *testing.T) {
	if Diff("f", "a\n", "a\n") != "" {
		t.Error("equal inputs diffed")
	}
	d := Diff("f", "1\n2\n3\n4\n5\n6\n7\n8\n9\n", "1\n2\n3\n4\nX\n6\n7\n8\n9\n")
	want := "--- f\n+++ f (pilothouse)\n@@ -2,7 +2,7 @@\n 2\n 3\n 4\n-5\n+X\n 6\n 7\n 8\n"
	if d != want {
		t.Errorf("got:\n%s\nwant:\n%s", d, want)
	}
	if d := Diff("f", "", "x\n"); !strings.Contains(d, "@@ -0,0 +1,1 @@\n+x\n") {
		t.Errorf("create diff:\n%s", d)
	}
}

func TestTextHelpers(t *testing.T) {
	if got := OneLine("git commit -m 'a\nb'", 80); got != "git commit -m 'a …" {
		t.Errorf("OneLine = %q", got)
	}
	if got := Truncate("abcdefgh", 5); got != "abcd…" {
		t.Errorf("Truncate = %q", got)
	}
	if got := CapBytes("aé", 2); got != "a" {
		t.Errorf("CapBytes = %q", got)
	}
	if got := RelPath("/p", "/p/src/a.go"); got != "src/a.go" {
		t.Errorf("RelPath = %q", got)
	}
	if got := RelPath("/p", "/etc/hosts"); got != "/etc/hosts" {
		t.Errorf("RelPath outside = %q", got)
	}
}

func TestCommitBackupAndSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "settings.json")
	if err := os.MkdirAll(filepath.Dir(real), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte(userSettings), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	p, err := PlanJSON(link, specs("/a/pilothouse"), owned)
	if err != nil {
		t.Fatal(err)
	}
	ch := p.Change("install")
	now := time.Unix(1700000000, 0)
	if err := p.Commit(&ch, now); err != nil {
		t.Fatal(err)
	}
	if ch.Backup != link+".pilothouse-bak-1700000000" {
		t.Errorf("Backup = %q", ch.Backup)
	}
	if b, _ := os.ReadFile(ch.Backup); string(b) != userSettings {
		t.Error("backup content differs")
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink replaced by a regular file")
	}
	if fi, _ := os.Stat(real); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	if b, _ := os.ReadFile(real); !strings.Contains(string(b), "pilothouse hook claude") {
		t.Error("target not updated")
	}
	// Same second again: a distinct backup name.
	p2, _ := PlanJSON(link, nil, owned)
	ch2 := p2.Change("uninstall")
	if err := p2.Commit(&ch2, now); err != nil {
		t.Fatal(err)
	}
	if ch2.Backup != link+".pilothouse-bak-1700000000-1" {
		t.Errorf("second Backup = %q", ch2.Backup)
	}
	// No temp files left behind.
	ents, _ := os.ReadDir(filepath.Dir(real))
	for _, e := range ents {
		if strings.Contains(e.Name(), "pilothouse-tmp") {
			t.Errorf("leftover %s", e.Name())
		}
	}
}

func TestPlanRefusesInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"model": "x",}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := PlanJSON(path, specs("/a/pilothouse"), owned)
	if err == nil || !strings.Contains(err.Error(), "refusing to modify") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != `{"model": "x",}` {
		t.Error("file touched")
	}
}

func TestProblems(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "pilothouse")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	found := []Found{
		{Command: ShimCommand(exe, "claude")},
		{Command: ShimCommand(filepath.Join(dir, "gone", "pilothouse"), "claude")},
		{Command: "pilothouse hook claude"},
	}
	probs := Problems(found)
	if len(probs) != 1 || !strings.Contains(probs[0], "does not exist") {
		t.Errorf("Problems = %v", probs)
	}
}

func TestDiffAppendAtEnd(t *testing.T) {
	d := Diff("f", "a\nb\n", "a\nb\nc\n")
	if want := "--- f\n+++ f (pilothouse)\n@@ -1,2 +1,3 @@\n a\n b\n+c\n"; d != want {
		t.Errorf("got:\n%s", d)
	}
}

// TestApplyKeepsUserFormatting: QA's hooks.json had one-line handler
// objects; installing and uninstalling must leave them as written (L10).
func TestApplyKeepsUserFormatting(t *testing.T) {
	const doc = `{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          { "type": "command", "command": "echo user-sessionstart >> /tmp/qa6/userhook.log" }
        ]
      }
    ],
    "Stop": [ { "hooks": [ { "type": "command", "command": "say done" } ] } ]
  },
  "other": {"a": [1, 2,  3]}
}
`
	res := mustApply(t, doc, specs("/usr/local/bin/pilothouse"))
	out := string(res.Doc)
	for _, keep := range []string{
		`          { "type": "command", "command": "echo user-sessionstart >> /tmp/qa6/userhook.log" }`,
		`{ "hooks": [ { "type": "command", "command": "say done" } ] }`,
		`"other": {"a": [1, 2,  3]}`,
	} {
		if !strings.Contains(out, keep) {
			t.Errorf("reformatted %q:\n%s", keep, out)
		}
	}
	decode(t, res.Doc)
	// Our own entries are indented like the file.
	if !strings.Contains(out, "\n        \"hooks\": [\n          {\n            \"type\": \"command\",\n            \"command\": \"/usr/local/bin/pilothouse hook claude\"") {
		t.Errorf("our entry not indented:\n%s", out)
	}
	// Uninstall restores the content, and every entry it did not have to
	// touch keeps its formatting (an array it removed ours from is
	// re-laid out; its remaining items are not).
	back := string(mustApply(t, out, nil).Doc)
	if !reflect.DeepEqual(decode(t, []byte(back)), decode(t, []byte(doc))) {
		t.Errorf("uninstall did not restore the content:\n%s", Diff("hooks.json", doc, back))
	}
	for _, keep := range []string{
		`          { "type": "command", "command": "echo user-sessionstart >> /tmp/qa6/userhook.log" }`,
		`      { "hooks": [ { "type": "command", "command": "say done" } ] }`,
		`"other": {"a": [1, 2,  3]}`,
	} {
		if !strings.Contains(back, keep) {
			t.Errorf("uninstall reformatted %q:\n%s", keep, back)
		}
	}
	found := map[string]Found{}
	for _, f := range mustApply(t, out, specs("/usr/local/bin/pilothouse")).Found {
		found[f.Event] = f
	}
	if f := found["PreToolUse"]; f.Matcher != "*" || f.Timeout != 30 {
		t.Errorf("PreToolUse Found = %+v", f)
	}
	if f := found["Stop"]; f.Matcher != "" || f.Timeout != 0 || f.Ref != (Ref{"Stop", 1, 0}) {
		t.Errorf("Stop Found = %+v", f)
	}
}

func TestOwnedByRenamedBinary(t *testing.T) {
	for _, shim := range []string{"/usr/local/bin/ph", "/opt/homebrew/bin/pilothouse", "/tmp/x y/ph"} {
		cmd := ShimCommand(shim, "claude")
		if !OwnedBy("claude")(cmd) {
			t.Errorf("OwnedBy(claude) does not recognise %q", cmd)
		}
		if OwnedBy("codex")(cmd) {
			t.Errorf("OwnedBy(codex) wrongly recognises %q", cmd)
		}
	}
	if got := ShimCommand("/opt/homebrew/bin/pilothouse", "claude"); strings.Contains(got, "#") {
		t.Errorf("a pilothouse-named binary needs no marker: %q", got)
	}
	for _, foreign := range []string{"/usr/bin/ph hook claude", "my-tool hook claude # pilothouse"} {
		if OwnedBy("claude")(foreign) {
			t.Errorf("foreign hook %q must not be treated as Pilothouse's", foreign)
		}
	}
}
