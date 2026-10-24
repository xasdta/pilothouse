package opencode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xasdta/pilothouse/internal/adapters"
)

// TestSuggestGolden: each testdata/settings/<name>.in is a global config
// file (opencode.json, or opencode.jsonc when the name says so); the
// .out golden is what Suggest writes, "" meaning no change.
func TestSuggestGolden(t *testing.T) {
	files, _ := filepath.Glob("testdata/settings/*.in")
	if len(files) < 8 {
		t.Fatalf("only %d settings fixtures", len(files))
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".in")
		t.Run(name, func(t *testing.T) {
			isolate(t)
			in, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			file := "opencode.json"
			if strings.Contains(name, "jsonc") {
				file = "opencode.jsonc"
			}
			path := filepath.Join(ConfigDir(), file)
			if err := os.MkdirAll(ConfigDir(), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, in, 0o600); err != nil {
				t.Fatal(err)
			}
			ch, _, _, err := Adapter{}.Suggest(adapters.InstallOptions{DryRun: true})
			if err != nil {
				t.Fatal(err)
			}
			if b, _ := os.ReadFile(path); string(b) != string(in) {
				t.Fatal("dry run changed the file")
			}
			got := ""
			if ch.Changed {
				got = ch.After
				if _, err := parseJSONC([]byte(got)); err != nil {
					t.Fatalf("result does not parse: %v\n%s", err, got)
				}
			}
			writeGolden(t, filepath.Join("testdata", "settings", name+".out"), []byte(got))
		})
	}
}

func TestSuggestApplyBacksUp(t *testing.T) {
	isolate(t)
	path := filepath.Join(ConfigDir(), "opencode.json")
	if err := os.MkdirAll(ConfigDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	orig := "{\n  \"model\": \"anthropic/claude\"\n}\n"
	if err := os.WriteFile(path, []byte(orig), 0o640); err != nil {
		t.Fatal(err)
	}
	ch, q, why, err := Adapter{}.Suggest(adapters.InstallOptions{})
	if err != nil || !ch.Changed || q == "" || why == "" {
		t.Fatalf("%+v %q %q %v", ch, q, why, err)
	}
	if b, _ := os.ReadFile(ch.Backup); string(b) != orig {
		t.Fatalf("backup %q", ch.Backup)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"bash": "ask"`) || !strings.Contains(string(b), `"model": "anthropic/claude"`) {
		t.Fatalf("applied:\n%s", b)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v not kept", fi.Mode().Perm())
	}
	// Now nothing is left to suggest.
	if ch, _, _, err := (Adapter{}).Suggest(adapters.InstallOptions{DryRun: true}); err != nil || ch.Changed {
		t.Fatalf("second suggest %+v %v", ch, err)
	}
}

func TestSuggestCreatesConfig(t *testing.T) {
	isolate(t)
	ch, _, _, err := Adapter{}.Suggest(adapters.InstallOptions{})
	if err != nil || !ch.Changed || ch.Backup != "" {
		t.Fatalf("%+v %v", ch, err)
	}
	b, err := os.ReadFile(filepath.Join(ConfigDir(), "opencode.json"))
	if err != nil || !strings.Contains(string(b), `"edit": "ask"`) {
		t.Fatalf("%s %v", b, err)
	}
}

func TestSuggestFollowsMergeOrder(t *testing.T) {
	isolate(t)
	if err := os.MkdirAll(ConfigDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	// config.json asks for both, but opencode.json (merged later) allows
	// bash: only bash needs changing, and in opencode.jsonc, which wins.
	write := func(name, s string) {
		if err := os.WriteFile(filepath.Join(ConfigDir(), name), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("config.json", `{"permission": {"bash": "ask", "edit": "ask"}}`)
	write("opencode.json", `{"permission": {"bash": "allow"}}`)
	write("opencode.jsonc", "// mine\n{}\n")
	ch, _, _, err := Adapter{}.Suggest(adapters.InstallOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(ch.Path, "opencode.jsonc") || !strings.Contains(ch.After, `"bash": "ask"`) || strings.Contains(ch.After, "edit") {
		t.Fatalf("%s:\n%s", ch.Path, ch.After)
	}
	if !strings.HasPrefix(ch.After, "// mine\n") {
		t.Fatal("comment lost")
	}
}

func TestSuggestRefusesBrokenConfig(t *testing.T) {
	isolate(t)
	if err := os.MkdirAll(ConfigDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ConfigDir(), "opencode.json")
	if err := os.WriteFile(path, []byte(`{"permission": `), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := (Adapter{}).Suggest(adapters.InstallOptions{}); err == nil {
		t.Fatal("broken config accepted")
	}
	if b, _ := os.ReadFile(path); string(b) != `{"permission": ` {
		t.Fatal("broken config was modified")
	}
}

func TestParseJSONC(t *testing.T) {
	good := []string{
		`{}`, `{"a": 1,}`, "{\n// c\n\"a\": [1, 2,], /* x */ \"b\": {\"c\": null}\n}", `"s"`, `[{"a": "b\"c"}]`,
	}
	for _, s := range good {
		if _, err := parseJSONC([]byte(s)); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	bad := []string{`{`, `{"a"}`, `{"a": }`, `{"a": 1 "b": 2}`, `{a: 1}`, `/* open`, `{"a": tru}`, `{} x`, "\"a\nb\"",
		strings.Repeat("[", 100) + strings.Repeat("]", 100)}
	for _, s := range bad {
		if _, err := parseJSONC([]byte(s)); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	v, _ := parseJSONC([]byte(`{"a": "x", "a": "y"}`))
	if v.get("a").str != "y" {
		t.Error("duplicate key: the last one wins")
	}
}
