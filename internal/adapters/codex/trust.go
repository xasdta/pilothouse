package codex

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/xasdta/pilothouse/internal/adapters/hookkit"
)

// Codex runs a hook from ~/.codex/hooks.json only once it is trusted:
// config.toml must hold
//
//	[hooks.state."<hooks.json path>:<event label>:<group>:<handler>"]
//	trusted_hash = "sha256:<hex>"
//
// where the hash covers the normalized hook declaration. Otherwise Codex
// lists the hook as "new, review required" and skips it. Installing a hook
// on the user's explicit request records that trust the way Codex's own
// /hooks review does. Sources (github.com/openai/codex, codex-rs/):
// hooks/src/engine/discovery.rs (hook_hash, NormalizedHookIdentity,
// normalize_command_hook, hook_trust_status), hooks/src/lib.rs (hook_key,
// hook_event_key_label), config/src/fingerprint.rs (version_for_toml),
// config/src/hook_config.rs (HookStateToml).

// markerComment precedes every trust table we write, so uninstall finds
// them even if hooks.json was edited by hand in between.
const markerComment = "# pilothouse-managed: trust for a Pilothouse hook in hooks.json (pilothouse hooks uninstall removes it)"

// isMarker recognizes markerComment, including earlier wordings.
func isMarker(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "# pilothouse-managed:")
}

// eventLabels mirrors hook_event_key_label.
var eventLabels = map[string]string{
	"PreModelRequest":   "pre_model_request",
	"PostModelRequest":  "post_model_request",
	"PreToolUse":        "pre_tool_use",
	"PermissionRequest": "permission_request",
	"PostToolUse":       "post_tool_use",
	"PreCompact":        "pre_compact",
	"PostCompact":       "post_compact",
	"SessionStart":      "session_start",
	"SessionEnd":        "session_end",
	"UserPromptSubmit":  "user_prompt_submit",
	"SubagentStart":     "subagent_start",
	"SubagentStop":      "subagent_stop",
	"Stop":              "stop",
	"Interrupt":         "interrupt",
}

// matcherEvents are the events whose matcher Codex keeps
// (matcher_pattern_for_event); for the rest it is dropped before hashing.
var matcherEvents = map[string]bool{
	"PreToolUse": true, "PermissionRequest": true, "PostToolUse": true,
	"SessionStart": true, "SessionEnd": true, "SubagentStart": true,
	"SubagentStop": true, "PreCompact": true, "PostCompact": true,
}

// trustKey mirrors hook_key: "<source path>:<event label>:<group>:<handler>".
func trustKey(hooksPath string, r hookkit.Ref) string {
	return hooksPath + ":" + eventLabels[r.Event] + ":" + strconv.Itoa(r.Group) + ":" + strconv.Itoa(r.Handler)
}

// normalizedTimeout mirrors normalize_command_hook: SessionEnd and
// Interrupt default to 1 s and are clamped to [1, 3]; everything else
// defaults to 600 s.
func normalizedTimeout(event string, timeout int) int {
	if event == "SessionEnd" || event == "Interrupt" {
		if timeout == 0 {
			return 1
		}
		return min(max(timeout, 1), 3)
	}
	if timeout == 0 {
		return 600
	}
	return timeout
}

// trustHash mirrors hook_hash + version_for_toml: SHA-256 over the
// key-sorted compact JSON of {event_name, matcher?, hooks: [handler]},
// where the handler carries type, command, the normalized timeout and
// async (absent options are dropped because the value passes through
// TOML, which has no null).
func trustHash(s hookkit.Spec) string {
	identity := map[string]any{
		"event_name": eventLabels[s.Event],
		"hooks": []any{map[string]any{
			"type":    "command",
			"command": s.Command,
			"timeout": normalizedTimeout(s.Event, s.Timeout),
			"async":   false,
		}},
	}
	if s.Matcher != "" && matcherEvents[s.Event] {
		identity["matcher"] = s.Matcher
	}
	// encoding/json sorts map keys; serde_json does not escape HTML
	// characters or U+2028/2029, so neither may we.
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(identity)
	raw := bytes.TrimRight(b.Bytes(), "\n")
	raw = bytes.ReplaceAll(raw, []byte(` `), []byte(" "))
	raw = bytes.ReplaceAll(raw, []byte(` `), []byte(" "))
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// trustEdit describes how the trust tables must change.
type trustEdit struct {
	stale  map[string]bool   // our old entries: delete unless re-wanted with the same hash
	rename map[string]string // foreign hooks that moved in hooks.json
	want   map[string]string // our handlers: key → trusted hash
}

// ErrUnsupportedLayout means config.toml declares hook state in a form
// this editor does not rewrite (inline tables, dotted keys).
type ErrUnsupportedLayout struct{ Detail string }

func (e ErrUnsupportedLayout) Error() string {
	return "config.toml " + e.Detail + "; Pilothouse edits only [hooks.state.\"…\"] tables. Trust Pilothouse's hooks in Codex instead (it asks at startup, or use /hooks)"
}

// applyTrust rewrites config.toml source so the trust tables match e,
// preserving everything else. It returns src unchanged when nothing
// needs to change.
func applyTrust(src string, e trustEdit) (string, error) {
	doc, err := parseTOML(src)
	if err != nil {
		return "", fmt.Errorf("config.toml: %w", err)
	}
	if len(e.want) > 0 || len(e.rename) > 0 {
		if err := checkLayout(doc, e); err != nil {
			return "", err
		}
	}
	lines := doc.lines()
	drop := make([]bool, len(lines))
	satisfied := map[string]bool{}
	edited := false
	targets := map[string]bool{}
	for _, to := range e.rename {
		targets[to] = true
	}
	// Pass 1: our entries are kept (when exactly right) or dropped;
	// entries of foreign hooks that moved follow them.
	var untouched []int
	for ti, t := range doc.tables {
		if !isStateTable(t) {
			continue
		}
		key := t.path[2]
		hash, _ := doc.stringAt("hooks", "state", key, "trusted_hash")
		marked := t.line > 0 && isMarker(lines[t.line-1])
		ours := e.stale[key] || marked
		want, wanted := e.want[key]
		switch to, moved := e.rename[key]; {
		case moved && !ours:
			lines[t.line] = renameHeader(lines[t.line], to)
			edited = true
		case wanted && hash == want && !satisfied[key]:
			satisfied[key] = true // already right: leave it where it is
		case wanted || ours:
			dropRegion(doc, lines, drop, ti, marked)
			edited = true
		default:
			untouched = append(untouched, ti)
		}
	}
	// Pass 2: a leftover entry sitting on a position a moved hook now
	// occupies is obsolete, and keeping it would define the key twice.
	for _, ti := range untouched {
		if targets[doc.tables[ti].path[2]] {
			dropRegion(doc, lines, drop, ti, false)
			edited = true
		}
	}
	var add []string
	for key := range e.want {
		if !satisfied[key] {
			add = append(add, key)
		}
	}
	if !edited && len(add) == 0 {
		return src, nil
	}
	sort.Strings(add)
	out := joinKept(lines, drop)
	for _, key := range add {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		if out != "" && !strings.HasSuffix(out, "\n\n") {
			out += "\n"
		}
		out += markerComment + "\n" + headerFor([]string{"hooks", "state", key}) + "\n" +
			"trusted_hash = " + quoteString(e.want[key]) + "\n"
	}
	return out, nil
}

func isStateTable(t tomlTable) bool {
	return !t.array && len(t.path) == 3 && t.path[0] == "hooks" && t.path[1] == "state"
}

// checkLayout refuses layouts where adding or renaming a
// [hooks.state."k"] table would produce invalid TOML (a key defined
// twice) or would miss an existing definition.
func checkLayout(doc *tomlDoc, e trustEdit) error {
	for _, kv := range doc.kvs {
		if equalPath(kv.path, []string{"hooks"}) || equalPath(kv.path, []string{"hooks", "state"}) {
			return ErrUnsupportedLayout{"sets " + strings.Join(kv.path, ".") + " as a value"}
		}
	}
	for _, t := range doc.tables {
		if t.array && (equalPath(t.path, []string{"hooks"}) || equalPath(t.path, []string{"hooks", "state"})) {
			return ErrUnsupportedLayout{"uses [[" + strings.Join(t.path, ".") + "]]"}
		}
		if hasPrefix(t.path, []string{"hooks", "state"}) && len(t.path) > 3 {
			return ErrUnsupportedLayout{"has nested table " + headerFor(t.path)}
		}
	}
	targets := map[string]bool{}
	for k := range e.want {
		targets[k] = true
	}
	for _, k := range e.rename {
		targets[k] = true
	}
	for k := range targets {
		for _, kv := range doc.kvsUnder("hooks", "state", k) {
			if kv.table < 0 || !isStateTable(doc.tables[kv.table]) || doc.tables[kv.table].path[2] != k {
				return ErrUnsupportedLayout{fmt.Sprintf("sets hooks.state.%s with dotted keys or an inline table (line %d)", quoteKey(k), kv.line+1)}
			}
		}
	}
	return nil
}

// dropRegion marks table ti's lines for deletion: its marker comment (if
// any), the header, and its body up to the next header, except comment
// lines directly above that header (they describe the next table).
func dropRegion(doc *tomlDoc, lines []string, drop []bool, ti int, marked bool) {
	start := doc.tables[ti].line
	if marked {
		start--
	}
	end := doc.tableEnd(ti)
	if end > len(lines) {
		end = len(lines)
	}
	if ti+1 < len(doc.tables) {
		for end-1 > doc.tables[ti].line && strings.HasPrefix(strings.TrimSpace(lines[end-1]), "#") {
			end--
		}
	}
	for i := start; i < end; i++ {
		drop[i] = true
	}
}

// joinKept rebuilds the file without dropped lines, collapsing the blank
// lines a deletion leaves on both sides of itself.
func joinKept(lines []string, drop []bool) string {
	var out []string
	prevDropped := false
	for i, l := range lines {
		if drop[i] {
			prevDropped = true
			continue
		}
		blank := strings.TrimSpace(l) == ""
		if prevDropped && blank && (len(out) == 0 || strings.TrimSpace(out[len(out)-1]) == "") {
			continue // keep prevDropped: swallow the whole blank run
		}
		prevDropped = false
		out = append(out, l)
	}
	// A deletion at the end must not leave trailing blank lines behind.
	for len(out) > 1 && strings.TrimSpace(out[len(out)-1]) == "" && strings.TrimSpace(out[len(out)-2]) == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// renameHeader rewrites a [hooks.state."old"] header line to the new key,
// keeping indentation and any trailing comment.
func renameHeader(line, key string) string {
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	sc := &tomlScanner{s: line, i: len(indent) + 1}
	if _, err := sc.key(); err != nil {
		return indent + headerFor([]string{"hooks", "state", key})
	}
	sc.skipBlank()
	rest := ""
	if sc.i < len(line) && line[sc.i] == ']' {
		rest = line[sc.i+1:]
	}
	return indent + headerFor([]string{"hooks", "state", key}) + rest
}

// trustState reads the trusted hash for key in any layout (for status).
func trustState(doc *tomlDoc, key string) (string, bool) {
	return doc.stringAt("hooks", "state", key, "trusted_hash")
}

// hooksFeatureOff reports [features] hooks = false (or the legacy
// codex_hooks key), which turns lifecycle hooks off entirely.
func hooksFeatureOff(doc *tomlDoc) bool {
	for _, k := range []string{"hooks", "codex_hooks"} {
		if v, ok := doc.rawAt("features", k); ok && strings.TrimSpace(v) == "false" {
			return true
		}
	}
	return false
}
