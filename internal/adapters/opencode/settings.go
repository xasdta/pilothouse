package opencode

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/hookkit"
)

// OpenCode's default permissions allow nearly everything ({"*": "allow"},
// with doom_loop and external_directory set to ask), so it rarely shows a
// permission prompt Pilothouse could hold for a person who is away. Setup
// offers to make it ask before shell commands and file edits.

var _ adapters.Suggester = Adapter{}

// askTools are the permissions setup sets to "ask".
var askTools = []string{"bash", "edit"}

// configFiles are the global config files, in the order OpenCode merges
// them (later files win).
func configFiles() []string {
	dir := ConfigDir()
	return []string{filepath.Join(dir, "config.json"), filepath.Join(dir, "opencode.json"), filepath.Join(dir, "opencode.jsonc")}
}

type configFile struct {
	file hookkit.File
	root *jvalue
}

// Suggest plans (or, without DryRun, applies) "permission": {"bash":
// "ask", "edit": "ask"} in the global config, for each of the two that is
// currently "allow" (explicitly or by default). A permission the person
// set to deny, ask or a per-pattern object is left alone. The edit goes
// into the config file that wins (opencode.jsonc, else opencode.json, else
// config.json, else a new opencode.json), keeps comments and formatting,
// and the file is backed up first.
func (Adapter) Suggest(opts adapters.InstallOptions) (adapters.Change, string, string, error) {
	const question = "Make OpenCode ask before shell commands and file edits?"
	const why = "OpenCode allows bash and edits without asking by default, so there is no prompt Pilothouse can hold for you while you're away (policy rules still apply)."
	files, err := readConfigs()
	if err != nil {
		return adapters.Change{}, question, why, err
	}
	var need []string
	for _, t := range askTools {
		if effective(files, t) == "allow" {
			need = append(need, t)
		}
	}
	target := files[len(files)-1]
	for i := len(files) - 1; i >= 0; i-- {
		if files[i].file.Exists {
			target = files[i]
			break
		}
	}
	if !target.file.Exists {
		target = files[1] // create opencode.json
	}
	ch := adapters.Change{Path: target.file.Path, Before: string(target.file.Data)}
	if len(need) == 0 {
		ch.After = ch.Before
		ch.Summary = "OpenCode already asks before shell commands and edits (or you chose otherwise)"
		return ch, question, why, nil
	}
	after, err := askEdit(target, need)
	if err != nil {
		return adapters.Change{}, question, why, fmt.Errorf("%s: %w", target.file.Path, err)
	}
	// Check the result the way OpenCode will read it.
	check := append([]configFile(nil), files...)
	for i := range check {
		if check[i].file.Path == target.file.Path {
			root, err := parseJSONC(after)
			if err != nil {
				return adapters.Change{}, question, why, fmt.Errorf("%s: the edit would not parse (%v); nothing was changed", target.file.Path, err)
			}
			check[i] = configFile{file: hookkit.File{Path: target.file.Path, Exists: true, Data: after}, root: root}
		}
	}
	for _, t := range need {
		if got := effective(check, t); got != "ask" {
			return adapters.Change{}, question, why, fmt.Errorf("%s: after the edit permission.%s would be %q; nothing was changed", target.file.Path, t, got)
		}
	}
	ch.Changed, ch.After = true, string(after)
	ch.Summary = fmt.Sprintf("Set permission %s to \"ask\" in %s", strings.Join(need, " and "), target.file.Path)
	if opts.DryRun {
		return ch, question, why, nil
	}
	ch.Backup, err = hookkit.Commit(target.file, after, time.Now())
	return ch, question, why, err
}

func readConfigs() ([]configFile, error) {
	var out []configFile
	for _, p := range configFiles() {
		f, err := hookkit.ReadFile(p)
		if err != nil {
			return nil, err
		}
		cf := configFile{file: f}
		if f.Exists && len(strings.TrimSpace(string(f.Data))) > 0 {
			if cf.root, err = parseJSONC(f.Data); err != nil {
				return nil, fmt.Errorf("can't read %s: %w", p, err)
			}
			if cf.root.kind != '{' {
				return nil, fmt.Errorf("can't read %s: not a JSON object", p)
			}
		}
		out = append(out, cf)
	}
	return out, nil
}

// effective is what OpenCode would use for permission tool: a string
// ("allow", "ask", "deny"), "pattern" for a per-pattern object, or
// "allow" when nothing is set (OpenCode's default).
func effective(files []configFile, tool string) string {
	// Deep merge of "permission" in file order: objects merge key by key,
	// anything else replaces.
	var str string
	obj := map[string]*jvalue{}
	isObj := true
	for _, f := range files {
		perm := f.root.get("permission")
		switch {
		case perm == nil:
		case perm.kind == '{':
			if !isObj {
				obj, isObj = map[string]*jvalue{}, true
			}
			for _, m := range perm.members {
				obj[m.key] = m.value
			}
		case perm.kind == '"':
			str, isObj, obj = perm.str, false, map[string]*jvalue{}
		}
	}
	if !isObj {
		return str
	}
	v := obj[tool]
	if v == nil {
		v = obj["*"]
	}
	switch {
	case v == nil:
		return "allow"
	case v.kind == '"':
		return v.str
	case v.kind == '{':
		return "pattern"
	}
	return "unknown"
}

// askEdit sets permission.<tool> = "ask" for each tool in the target file.
func askEdit(cf configFile, tools []string) ([]byte, error) {
	if cf.root == nil {
		perm := map[string]string{}
		for _, t := range tools {
			perm[t] = "ask"
		}
		b, err := json.MarshalIndent(map[string]any{"$schema": "https://opencode.ai/config.json", "permission": perm}, "", "  ")
		return append(b, '\n'), err
	}
	src := cf.file.Data
	perm := cf.root.get("permission")
	var ss []splice
	switch {
	case perm == nil:
		want := map[string]string{}
		for _, t := range tools {
			want[t] = "ask"
		}
		s, err := appendMembers(src, cf.root, []kv{{"permission", want}})
		if err != nil {
			return nil, err
		}
		ss = s
	case perm.kind == '"':
		want := map[string]string{"*": perm.str}
		for _, t := range tools {
			want[t] = "ask"
		}
		b, err := json.MarshalIndent(want, indentOf(src, perm.start), unit(src, cf.root))
		if err != nil {
			return nil, err
		}
		ss = []splice{{perm.start, perm.end, string(b)}}
	case perm.kind == '{':
		// Replace "allow" values in place; add the missing keys at the end.
		var missing []kv
		for _, t := range tools {
			if v := perm.get(t); v != nil {
				if v.kind != '"' {
					return nil, fmt.Errorf("permission.%s is not a string", t)
				}
				ss = append(ss, splice{v.start, v.end, `"ask"`})
			} else {
				missing = append(missing, kv{t, "ask"})
			}
		}
		if len(missing) > 0 {
			s, err := appendMembers(src, perm, missing)
			if err != nil {
				return nil, err
			}
			ss = append(ss, s...)
		}
		sortSplices(ss)
	default:
		return nil, fmt.Errorf("permission is neither a string nor an object")
	}
	return applySplices(src, ss)
}

func sortSplices(ss []splice) {
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && ss[j].start < ss[j-1].start; j-- {
			ss[j], ss[j-1] = ss[j-1], ss[j]
		}
	}
}
