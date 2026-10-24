package policy

import (
	"path/filepath"

	"github.com/xasdta/pilothouse/internal/api"
)

// Listings for the Rules page and `pilothouse policy rules`.

// ManagedFile is the managed rules file this engine reads ("" for none).
func (e *Engine) ManagedFile() string { return e.opts.ManagedFile }

// Home is the user home directory path patterns expand ~ against.
func (e *Engine) Home() string { return e.home }

// RuleInfos lists every rule in evaluation order: built-in, policy.yaml,
// the managed file (disabled rules too), the presets that are on, then
// the repo files of repos (typically the repos of live sessions). Files
// with errors list the rules still in force from their last good version.
func (e *Engine) RuleInfos(repos []string) []api.RuleInfo {
	nanos := e.clock().UnixNano()
	var out []api.RuleInfo
	for _, r := range e.builtin {
		out = append(out, ruleInfo(r, api.RuleSourceBuiltin, BuiltinFile, true))
	}
	for _, r := range e.user.get(nanos, false).val {
		out = append(out, ruleInfo(r, api.RuleSourceUser, e.opts.UserFile, true))
	}
	if m := e.managed.get(nanos, false).val; m != nil {
		for _, mr := range m.doc.Rules {
			out = append(out, api.RuleInfo{RuleSpec: mr.Spec(), Source: api.RuleSourceManaged, File: e.opts.ManagedFile,
				Enabled: mr.On(), Managed: true, Created: mr.Created, By: mr.By})
		}
		for _, p := range m.presets {
			for _, r := range p.rules {
				ri := ruleInfo(r, api.RuleSourcePreset, PresetFilePfx+p.name, true)
				ri.Preset = p.name
				out = append(out, ri)
			}
		}
	}
	seen := map[string]bool{}
	for _, root := range repos {
		if root == "" {
			continue
		}
		root = filepath.Clean(expandHome(root, e.home))
		if seen[root] {
			continue
		}
		seen[root] = true
		rs := e.repoState(root, nanos)
		trusted := e.trust.get(nanos, false).val.has(rs)
		for _, r := range rs.file.get(nanos, false).val {
			out = append(out, ruleInfo(r, api.RuleSourceRepo, rs.file.path, trusted || r.action != api.BehaviorAllow))
		}
	}
	return out
}

func ruleInfo(r *rule, source, file string, enabled bool) api.RuleInfo {
	spec := api.RuleSpec{Name: r.name, Description: r.description, Action: r.action, When: r.whenSrc, Message: r.message}
	if len(r.matchSrc) > 0 || len(r.agents) > 0 {
		spec.Match = make(map[string][]string, len(r.matchSrc)+1)
		for k, v := range r.matchSrc {
			spec.Match[k] = append([]string(nil), v...)
		}
		if _, ok := spec.Match["agent"]; !ok && len(r.agents) > 0 {
			for _, a := range r.agents {
				spec.Match["agent"] = append(spec.Match["agent"], string(a))
			}
		}
	}
	return api.RuleInfo{RuleSpec: spec, Source: source, File: file, Enabled: enabled}
}

// PresetsView lists every preset and whether the managed file switches
// it on.
func (e *Engine) PresetsView() []api.Preset {
	ps := Presets()
	if m := e.managed.get(e.clock().UnixNano(), false).val; m != nil {
		for i := range ps {
			ps[i].Enabled = m.doc.PresetOn(ps[i].Name)
		}
	}
	return ps
}
