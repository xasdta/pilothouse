package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
)

// A small JSONC reader (JSON with // and /* */ comments and trailing
// commas, which OpenCode accepts in every config file). It records where
// each value sits in the source, so an edit can splice in new text and
// leave every comment and the person's formatting as they were.

type jvalue struct {
	start, end int // byte span of the value in the source
	kind       byte
	str        string    // kind '"'
	members    []jmember // kind '{'
	close      int       // kind '{': offset of the closing brace
}

type jmember struct {
	key   string
	value *jvalue
}

func (v *jvalue) get(key string) *jvalue {
	if v == nil || v.kind != '{' {
		return nil
	}
	var out *jvalue
	for i := range v.members {
		if v.members[i].key == key {
			out = v.members[i].value // the last duplicate wins, as in JSON.parse
		}
	}
	return out
}

const maxJSONCDepth = 64

type jparser struct {
	src []byte
	pos int
}

func parseJSONC(src []byte) (*jvalue, error) {
	p := &jparser{src: src}
	if err := p.skip(); err != nil {
		return nil, err
	}
	v, err := p.value(0)
	if err != nil {
		return nil, err
	}
	if err := p.skip(); err != nil {
		return nil, err
	}
	if p.pos != len(src) {
		return nil, p.errf("unexpected text after the value")
	}
	return v, nil
}

func (p *jparser) errf(format string, args ...any) error {
	line, col := 1, 1
	for _, c := range p.src[:min(p.pos, len(p.src))] {
		if c == '\n' {
			line, col = line+1, 1
		} else {
			col++
		}
	}
	return fmt.Errorf("line %d, column %d: %s", line, col, fmt.Sprintf(format, args...))
}

// skip moves past whitespace and comments.
func (p *jparser) skip() error {
	for p.pos < len(p.src) {
		switch c := p.src[p.pos]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			p.pos++
		case c == '/' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '/':
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
		case c == '/' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '*':
			end := -1
			for i := p.pos + 2; i+1 < len(p.src); i++ {
				if p.src[i] == '*' && p.src[i+1] == '/' {
					end = i + 2
					break
				}
			}
			if end < 0 {
				return p.errf("unterminated comment")
			}
			p.pos = end
		default:
			return nil
		}
	}
	return nil
}

func (p *jparser) value(depth int) (*jvalue, error) {
	if depth > maxJSONCDepth {
		return nil, p.errf("nested too deeply")
	}
	if p.pos >= len(p.src) {
		return nil, p.errf("unexpected end of file")
	}
	switch c := p.src[p.pos]; c {
	case '{':
		return p.object(depth)
	case '[':
		return p.array(depth)
	case '"':
		return p.string()
	default:
		start := p.pos
		for p.pos < len(p.src) && !isDelim(p.src[p.pos]) {
			p.pos++
		}
		lit := p.src[start:p.pos]
		var x any
		if len(lit) == 0 || json.Unmarshal(lit, &x) != nil {
			return nil, p.errf("unexpected %q", string(lit))
		}
		return &jvalue{start: start, end: p.pos, kind: 'l'}, nil
	}
}

func isDelim(c byte) bool {
	switch c {
	case ',', '}', ']', ':', ' ', '\t', '\n', '\r', '/', '"':
		return true
	}
	return false
}

func (p *jparser) string() (*jvalue, error) {
	start := p.pos
	p.pos++
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case '\\':
			p.pos += 2
			continue
		case '"':
			p.pos++
			var s string
			if err := json.Unmarshal(p.src[start:p.pos], &s); err != nil {
				return nil, p.errf("bad string: %v", err)
			}
			return &jvalue{start: start, end: p.pos, kind: '"', str: s}, nil
		case '\n':
			return nil, p.errf("newline in string")
		}
		p.pos++
	}
	return nil, p.errf("unterminated string")
}

func (p *jparser) object(depth int) (*jvalue, error) {
	v := &jvalue{start: p.pos, kind: '{'}
	p.pos++
	for {
		if err := p.skip(); err != nil {
			return nil, err
		}
		if p.pos >= len(p.src) {
			return nil, p.errf("unterminated object")
		}
		if p.src[p.pos] == '}' {
			v.close = p.pos
			p.pos++
			v.end = p.pos
			return v, nil
		}
		if p.src[p.pos] != '"' {
			return nil, p.errf("expected a key")
		}
		k, err := p.string()
		if err != nil {
			return nil, err
		}
		if err := p.skip(); err != nil {
			return nil, err
		}
		if p.pos >= len(p.src) || p.src[p.pos] != ':' {
			return nil, p.errf("expected ':'")
		}
		p.pos++
		if err := p.skip(); err != nil {
			return nil, err
		}
		val, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		v.members = append(v.members, jmember{key: k.str, value: val})
		if err := p.skip(); err != nil {
			return nil, err
		}
		if p.pos < len(p.src) && p.src[p.pos] == ',' {
			p.pos++
			continue
		}
		if p.pos < len(p.src) && p.src[p.pos] == '}' {
			continue
		}
		return nil, p.errf("expected ',' or '}'")
	}
}

func (p *jparser) array(depth int) (*jvalue, error) {
	v := &jvalue{start: p.pos, kind: '['}
	p.pos++
	for {
		if err := p.skip(); err != nil {
			return nil, err
		}
		if p.pos >= len(p.src) {
			return nil, p.errf("unterminated array")
		}
		if p.src[p.pos] == ']' {
			p.pos++
			v.end = p.pos
			return v, nil
		}
		if _, err := p.value(depth + 1); err != nil {
			return nil, err
		}
		if err := p.skip(); err != nil {
			return nil, err
		}
		if p.pos < len(p.src) && p.src[p.pos] == ',' {
			p.pos++
			continue
		}
		if p.pos < len(p.src) && p.src[p.pos] == ']' {
			continue
		}
		return nil, p.errf("expected ',' or ']'")
	}
}

// ---------- edits ----------

// splice is one replacement of src[start:end].
type splice struct {
	start, end int
	text       string
}

func applySplices(src []byte, ss []splice) ([]byte, error) {
	out := make([]byte, 0, len(src)+256)
	pos := 0
	for _, s := range ss {
		if s.start < pos || s.end < s.start || s.end > len(src) {
			return nil, errors.New("overlapping edits")
		}
		out = append(out, src[pos:s.start]...)
		out = append(out, s.text...)
		pos = s.end
	}
	return append(out, src[pos:]...), nil
}

// indentOf is the leading whitespace of the line holding offset i.
func indentOf(src []byte, i int) string {
	ls := i
	for ls > 0 && src[ls-1] != '\n' {
		ls--
	}
	le := ls
	for le < len(src) && (src[le] == ' ' || src[le] == '\t') {
		le++
	}
	return string(src[ls:le])
}

// unit guesses one level of indentation from an object's members.
func unit(src []byte, obj *jvalue) string {
	base := indentOf(src, obj.start)
	for _, m := range obj.members {
		if in := indentOf(src, m.value.start); len(in) > len(base) && onOwnLine(src, m.value.start) {
			return in[len(base):]
		}
	}
	return "  "
}

// onOwnLine reports whether the member holding the value at i starts its
// line (as opposed to {"a": 1, "b": 2} on one line).
func onOwnLine(src []byte, i int) bool {
	for j := i - 1; j >= 0; j-- {
		switch src[j] {
		case '\n':
			return true
		case '{', ',':
			return false
		}
	}
	return false
}

// kv is a member to add.
type kv struct {
	key   string
	value any
}

// appendMembers adds members at the end of obj: a comma after the current
// last member (unless a trailing comma is already there), then each member
// on its own line before the closing brace.
func appendMembers(src []byte, obj *jvalue, add []kv) ([]splice, error) {
	base := indentOf(src, obj.start)
	if onOwnLineBrace(src, obj) {
		base = indentOf(src, obj.close)
	}
	ind := base + unit(src, obj)
	var member string
	for i, m := range add {
		val, err := json.MarshalIndent(m.value, ind, unit(src, obj))
		if err != nil {
			return nil, err
		}
		k, _ := json.Marshal(m.key)
		if i > 0 {
			member += ","
		}
		member += "\n" + ind + string(k) + ": " + string(val)
	}
	var ss []splice
	if n := len(obj.members); n > 0 && !hasComma(src, obj.members[n-1].value.end, obj.close) {
		at := obj.members[n-1].value.end
		ss = append(ss, splice{at, at, ","})
	}
	// Before the whitespace that precedes the closing brace.
	at := obj.close
	for at > obj.start+1 && (src[at-1] == ' ' || src[at-1] == '\t' || src[at-1] == '\n' || src[at-1] == '\r') {
		at--
	}
	if len(ss) > 0 && ss[0].start > at {
		at = ss[0].start
	}
	text := member
	if at == obj.close || !containsNewline(src[at:obj.close]) {
		text += "\n" + base
	}
	return append(ss, splice{at, at, text}), nil
}

func onOwnLineBrace(src []byte, obj *jvalue) bool {
	for j := obj.close - 1; j > obj.start; j-- {
		switch src[j] {
		case '\n':
			return true
		case ' ', '\t', '\r':
			continue
		}
		return false
	}
	return false
}

func containsNewline(b []byte) bool {
	for _, c := range b {
		if c == '\n' {
			return true
		}
	}
	return false
}

// hasComma reports whether a ',' (outside comments) lies in src[from:to].
func hasComma(src []byte, from, to int) bool {
	p := &jparser{src: src[:to], pos: from}
	for p.pos < to {
		if err := p.skip(); err != nil || p.pos >= to {
			return false
		}
		if src[p.pos] == ',' {
			return true
		}
		p.pos++
	}
	return false
}
