package hookkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
)

// Spec is one hook handler Pilothouse wants wired for one vendor event, in
// the Claude-style hooks schema both agents use:
//
//	{"hooks": {"<Event>": [{"matcher": "*", "hooks": [{"type": "command", "command": "…", "timeout": 30}]}]}}
type Spec struct {
	Event   string
	Matcher string // "" omits the field (match everything)
	Command string
	Timeout int // seconds; 0 omits the field (vendor default)
}

// handlerJSON is the handler object Spec describes, keys in a stable order.
func (s Spec) handlerJSON() json.RawMessage {
	h := object{
		{"type", marshalString("command")},
		{"command", marshalString(s.Command)},
	}
	if s.Timeout > 0 {
		h = append(h, member{"timeout", marshalInt(s.Timeout)})
	}
	return h.marshal()
}

func (s Spec) groupJSON() json.RawMessage {
	var g object
	if s.Matcher != "" {
		g = append(g, member{"matcher", marshalString(s.Matcher)})
	}
	g = append(g, member{"hooks", marshalArray([]json.RawMessage{s.handlerJSON()})})
	return g.marshal()
}

// Ref locates one handler: hooks.<Event>[Group].hooks[Handler]. Codex keys
// its per-hook trust state by this position.
type Ref struct {
	Event   string
	Group   int
	Handler int
}

// Found is one of our handlers seen in the original document.
type Found struct {
	Ref
	Command string
	Matcher string // the group's matcher ("" when absent)
	Timeout int    // the handler's timeout in seconds (0 when absent)
	Current bool   // matches the Spec for its event exactly
}

// Result is the outcome of Apply.
type Result struct {
	Doc     []byte // the new document; the original bytes when !Changed
	Changed bool
	Found   []Found        // our handlers in the original document
	Placed  map[string]Ref // event → position of our handler in Doc
	Moved   map[Ref]Ref    // foreign handlers whose position changed
}

// Owned reports whether a handler command belongs to Pilothouse.
type Owned func(command string) bool

// Apply makes the hooks object under the top-level "hooks" key contain
// exactly one handler per spec (specs must have distinct events) and no
// other handler that owned() claims, leaving everything else untouched.
// An existing handler that already matches its spec stays where it is, so
// running Apply twice is a no-op. specs == nil removes all of ours.
//
// doc may be empty (treated as {}). Invalid JSON, or a "hooks" value of
// the wrong shape, is an error: we never overwrite what we cannot read.
func Apply(doc []byte, specs []Spec, owned Owned) (Result, error) {
	res := Result{Placed: map[string]Ref{}, Moved: map[Ref]Ref{}}
	top := object{}
	if len(bytes.TrimSpace(doc)) > 0 {
		var err error
		if top, err = parseObject(doc); err != nil {
			return res, fmt.Errorf("invalid JSON: %w", err)
		}
	}
	hooks := object{}
	if raw, ok := top.get("hooks"); ok {
		var err error
		if hooks, err = parseObject(raw); err != nil {
			return res, fmt.Errorf(`"hooks" is not a JSON object`)
		}
	}
	want := map[string]Spec{}
	for _, s := range specs {
		want[s.Event] = s
	}

	ed := editor{owned: owned, want: want, res: &res}
	var newHooks object
	for _, m := range hooks {
		val, keep, err := ed.event(m.key, m.val)
		if err != nil {
			return res, err
		}
		if keep {
			newHooks = append(newHooks, member{m.key, val})
		}
	}
	for _, s := range specs {
		if _, done := res.Placed[s.Event]; done {
			continue
		}
		groups := []json.RawMessage{s.groupJSON()}
		if i := newHooks.index(s.Event); i >= 0 {
			existing, _ := parseArray(newHooks[i].val)
			res.Placed[s.Event] = Ref{s.Event, len(existing), 0}
			newHooks[i].val = marshalArray(append(existing, groups...))
		} else {
			res.Placed[s.Event] = Ref{s.Event, 0, 0}
			newHooks = append(newHooks, member{s.Event, marshalArray(groups)})
		}
		ed.changed = true
	}
	if !ed.changed {
		res.Doc = doc
		return res, nil
	}
	res.Changed = true
	_, hadHooks := top.get("hooks")
	switch {
	case len(newHooks) > 0:
		top = top.set("hooks", newHooks.marshal())
	case hadHooks && len(hooks) > 0:
		// Every event list was ours alone: leave no empty husk behind.
		top = top.without("hooks")
	}
	res.Doc = format(top.marshal(), doc)
	return res, nil
}

type editor struct {
	owned   Owned
	want    map[string]Spec
	res     *Result
	changed bool
}

// event rewrites one hooks.<event> array. keep=false drops the key, which
// happens only when the array held nothing but our handlers.
func (ed *editor) event(name string, raw json.RawMessage) (json.RawMessage, bool, error) {
	groups, err := parseArray(raw)
	if err != nil {
		if _, wanted := ed.want[name]; wanted {
			return nil, false, fmt.Errorf("hooks.%s is not an array", name)
		}
		return raw, true, nil // not ours to judge
	}
	var out []json.RawMessage
	touched := false
	for gi, g := range groups {
		ng, keep, dirty := ed.group(name, gi, len(out), g)
		touched = touched || dirty
		if keep {
			out = append(out, ng)
		}
	}
	if !touched {
		return raw, true, nil
	}
	ed.changed = true
	if len(out) == 0 && len(groups) > 0 {
		return nil, false, nil
	}
	return marshalArray(out), true, nil
}

// group filters our handlers out of one matcher group, keeping the first
// one that already matches its spec. newIndex is where the group lands.
func (ed *editor) group(event string, gi, newIndex int, raw json.RawMessage) (json.RawMessage, bool, bool) {
	gobj, err := parseObject(raw)
	if err != nil {
		return raw, true, false
	}
	hraw, ok := gobj.get("hooks")
	if !ok {
		return raw, true, false
	}
	handlers, err := parseArray(hraw)
	if err != nil {
		return raw, true, false
	}
	spec, wanted := ed.want[event]
	var kept []json.RawMessage
	dirty := false
	for hi, h := range handlers {
		cmd, isCmd := commandOf(h)
		if !isCmd || !ed.owned(cmd) {
			from := Ref{event, gi, hi}
			if to := (Ref{event, newIndex, len(kept)}); to != from {
				ed.res.Moved[from] = to
			}
			kept = append(kept, h)
			continue
		}
		current := wanted && sameJSON(h, spec.handlerJSON()) && matcherOf(gobj) == spec.Matcher
		ed.res.Found = append(ed.res.Found, Found{Ref: Ref{event, gi, hi}, Command: cmd,
			Matcher: matcherOf(gobj), Timeout: timeoutOf(h), Current: current})
		if _, placed := ed.res.Placed[event]; current && !placed {
			ed.res.Placed[event] = Ref{event, newIndex, len(kept)}
			kept = append(kept, h)
			continue
		}
		dirty = true
	}
	if !dirty {
		return raw, true, false
	}
	if len(kept) == 0 {
		// The group existed only for our handler(s): drop it, and later
		// groups shift up (recorded in Moved as they are visited).
		return nil, false, true
	}
	return gobj.set("hooks", marshalArray(kept)).marshal(), true, true
}

func commandOf(h json.RawMessage) (string, bool) {
	var v struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	if json.Unmarshal(h, &v) != nil || v.Type != "command" {
		return "", false
	}
	return v.Command, true
}

func timeoutOf(h json.RawMessage) int {
	var v struct {
		Timeout float64 `json:"timeout"`
	}
	_ = json.Unmarshal(h, &v)
	return int(v.Timeout)
}

func matcherOf(g object) string {
	raw, ok := g.get("matcher")
	if !ok {
		return ""
	}
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

// sameJSON compares two JSON values semantically (key order and spacing
// ignored).
func sameJSON(a, b json.RawMessage) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}
