package wrap

import (
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// modeTracker follows the terminal modes an agent's TUI switches on, so
//
//   - the holder can switch them on again for a newly attached terminal
//     (a reattached TUI repaints on SIGWINCH but never re-enters the
//     alternate screen or re-enables mouse reporting by itself), and
//   - the attach client can switch them off on detach, leaving the user's
//     shell usable (no alternate screen, visible cursor, no mouse or
//     kitty-keyboard escape codes typed into the prompt).
//
// It parses CSI sequences only, with bounded state.
type modeTracker struct {
	st     uint8 // 0 ground, 1 ESC, 2 CSI
	params []byte
	// modes holds DEC private modes that differ from their default.
	modes map[int]bool
	// kitty is the stack of kitty keyboard protocol flag pushes (CSI > f u).
	kitty []string
}

// trackedModes are the DEC private modes worth restoring, with their
// power-on defaults. Alternate screen comes first so it is entered before
// the others are applied on restore.
var trackedModes = []struct {
	n   int
	def bool
}{
	{1049, false}, {1047, false}, {47, false}, // alternate screen
	{1, false},                                  // application cursor keys
	{25, true},                                  // cursor visible
	{1000, false}, {1002, false}, {1003, false}, // mouse tracking
	{1005, false}, {1006, false}, {1015, false}, // mouse encodings
	{1004, false}, // focus events
	{2004, false}, // bracketed paste
}

const maxCSIParams = 64

func newModeTracker() *modeTracker { return &modeTracker{modes: map[int]bool{}} }

func (m *modeTracker) Feed(p []byte) {
	for _, c := range p {
		switch m.st {
		case 0:
			if c == 0x1b {
				m.st = 1
			}
		case 1:
			switch c {
			case '[':
				m.st = 2
				m.params = m.params[:0]
			case 'c': // RIS: full reset
				m.reset()
				m.st = 0
			case 0x1b:
			default:
				m.st = 0
			}
		case 2:
			switch {
			case c >= 0x30 && c <= 0x3f: // parameter bytes, incl. ? > <
				if len(m.params) < maxCSIParams {
					m.params = append(m.params, c)
				}
			case c >= 0x20 && c <= 0x2f: // intermediates: not a sequence we track
				m.params = append(m.params[:0], 0)
			case c >= 0x40 && c <= 0x7e:
				m.apply(c)
				m.st = 0
			case c == 0x1b:
				m.st = 1
			default: // C0 controls inside CSI are executed; ignore them
			}
		}
	}
}

func (m *modeTracker) reset() {
	clear(m.modes)
	m.kitty = m.kitty[:0]
}

func (m *modeTracker) apply(final byte) {
	if len(m.params) == 0 {
		return
	}
	lead, rest := m.params[0], string(m.params[1:])
	switch {
	case lead == '?' && (final == 'h' || final == 'l'):
		on := final == 'h'
		for _, f := range strings.Split(rest, ";") {
			n, err := strconv.Atoi(f)
			if err != nil {
				continue
			}
			for _, t := range trackedModes {
				if t.n == n {
					if on == t.def {
						delete(m.modes, n)
					} else {
						m.modes[n] = on
					}
				}
			}
		}
	case lead == '>' && final == 'u':
		if len(m.kitty) < 16 {
			m.kitty = append(m.kitty, rest)
		}
	case lead == '<' && final == 'u':
		n := 1
		if v, err := strconv.Atoi(rest); err == nil && v > 0 {
			n = v
		}
		m.kitty = m.kitty[:max(0, len(m.kitty)-n)]
	}
}

// restore returns the sequences that put a fresh terminal into the
// tracked state.
func (m *modeTracker) restore() []byte {
	var b strings.Builder
	for _, t := range trackedModes {
		if on, ok := m.modes[t.n]; ok {
			b.WriteString(decMode(t.n, on))
		}
	}
	for _, f := range m.kitty {
		b.WriteString("\x1b[>" + f + "u")
	}
	return []byte(b.String())
}

// undo returns the sequences that return the terminal to its defaults.
// The alternate screen is left last so the other resets apply to the
// screen the TUI was using, then the user's own screen comes back.
func (m *modeTracker) undo() []byte {
	var b strings.Builder
	if len(m.kitty) > 0 {
		b.WriteString("\x1b[<" + strconv.Itoa(len(m.kitty)) + "u")
	}
	for _, t := range slices.Backward(trackedModes) {
		if _, ok := m.modes[t.n]; ok {
			b.WriteString(decMode(t.n, t.def))
		}
	}
	if b.Len() > 0 {
		b.WriteString("\x1b[0m")
	}
	return []byte(b.String())
}

func decMode(n int, on bool) string {
	if on {
		return "\x1b[?" + strconv.Itoa(n) + "h"
	}
	return "\x1b[?" + strconv.Itoa(n) + "l"
}

// bracketedPaste reports whether the agent turned bracketed paste (DEC
// mode 2004) on.
func (m *modeTracker) bracketedPaste() bool { return m.modes[2004] }

// Bracketed-paste markers (xterm).
const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// pasteBytes is what the PTY receives for a paste of text, the way a
// terminal pastes: newlines as CR, wrapped in bracketed-paste markers
// when the agent asked for them. Control characters other than tab and
// newline (ESC first of all), DEL and C1 controls are dropped, as are
// invalid UTF-8 bytes, so pasted text can't end the paste early or
// smuggle key sequences in.
func pasteBytes(text []byte, bracketed bool) []byte {
	out := make([]byte, 0, len(text)+len(pasteStart)+len(pasteEnd))
	if bracketed {
		out = append(out, pasteStart...)
	}
	for i := 0; i < len(text); {
		r, n := utf8.DecodeRune(text[i:])
		switch {
		case r == utf8.RuneError && n <= 1:
		case r == '\r':
			if i+1 < len(text) && text[i+1] == '\n' {
				n = 2
			}
			out = append(out, '\r')
		case r == '\n':
			out = append(out, '\r')
		case r == '\t':
			out = append(out, '\t')
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
		default:
			out = append(out, text[i:i+n]...)
		}
		i += n
	}
	if bracketed {
		out = append(out, pasteEnd...)
	}
	return out
}
