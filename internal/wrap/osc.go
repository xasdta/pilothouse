package wrap

import (
	"strings"

	"github.com/xasdta/pilothouse/internal/api"
)

// OSC 7501 is the Program Status Protocol (layer L1): an agent, or a
// wrapper script around one, prints
//
//	ESC ] 7501 ; state=<s> [:app=<a>] [:kind=<k>] [:id=<id>] … (BEL | ESC \)
//
// and the holder turns it into evidence for the daemon. Fields are
// separated by ':' (Codex, codex-rs/tui/src/terminal_program_status.rs, and
// Claude Code) or ';' (Pilothouse's original form; both are accepted).
// Terminals ignore OSC numbers they don't know, so the sequence is passed
// through to the attached terminal unchanged.
//
// Claude Code only emits status after it has asked the terminal whether it
// speaks the protocol: it writes the query "ESC ] 7501 ; ? ST" and treats
// any OSC 7501 reply whose data starts with '?' as "supported" (Claude Code
// 2.1.309: the probe sends OSC 7501 ";?" and matches
// `e.type==="osc" && e.code===7501 && e.data.startsWith("?")`; see
// https://code.claude.com/docs/en/terminal-config, "See session status in
// your terminal"). The scanner reports such queries through OnQuery so the
// holder can answer them with QueryReply.

const (
	oscNumber = "7501"
	// maxOSCPayload bounds what is buffered for one 7501 sequence; real
	// ones are a few dozen bytes.
	maxOSCPayload = 512
	// maxOSCSkip bounds how long the scanner follows an unterminated OSC
	// of any number. Past it the scanner resynchronises at the next ESC,
	// so a stray "ESC ]" in binary output can't blind it forever.
	maxOSCSkip = 64 << 10
)

type oscState uint8

const (
	oscGround  oscState = iota
	oscEsc              // saw ESC
	oscBody             // inside ESC ] … collecting
	oscBodyEsc          // inside the body, saw ESC (maybe ST)
)

// OSCScanner finds OSC 7501 sequences in a terminal byte stream. It keeps
// state between Feed calls, so sequences split across PTY reads are found.
// Memory is bounded by maxOSCPayload regardless of input. Not safe for
// concurrent use.
type OSCScanner struct {
	// OnQuery, when set, is called for every OSC 7501 support query
	// ("ESC ] 7501 ; ? ST") found in the stream.
	OnQuery func()

	st      oscState
	buf     []byte // payload of the current OSC, while it may still be 7501
	skipped int    // bytes seen in the current OSC body
	ignore  bool   // current OSC is not 7501, or is oversized/garbled
}

// Feed scans p and calls emit for every complete, valid 7501 sequence.
func (s *OSCScanner) Feed(p []byte, emit func(api.OSCBody)) {
	for _, c := range p {
		switch s.st {
		case oscGround:
			if c == 0x1b {
				s.st = oscEsc
			}
		case oscEsc:
			switch c {
			case ']':
				s.st = oscBody
				s.buf = s.buf[:0]
				s.skipped = 0
				s.ignore = false
			case 0x1b:
				// ESC ESC: the second one may start a sequence.
			default:
				s.st = oscGround
			}
		case oscBody:
			switch c {
			case 0x07: // BEL terminator (xterm)
				s.finish(emit)
			case 0x1b:
				s.st = oscBodyEsc
			case 0x18, 0x1a: // CAN, SUB abort a control string (ECMA-48)
				s.st = oscGround
			default:
				s.collect(c)
			}
		case oscBodyEsc:
			if c == '\\' { // ESC \ is ST, the standard terminator
				s.finish(emit)
				continue
			}
			// Any other ESC x aborts the OSC; the ESC may begin a new one.
			s.st = oscEsc
			if c == ']' {
				s.st = oscBody
				s.buf = s.buf[:0]
				s.skipped = 0
				s.ignore = false
			} else if c != 0x1b {
				s.st = oscGround
			}
		}
	}
}

func (s *OSCScanner) collect(c byte) {
	s.skipped++
	if s.skipped > maxOSCSkip {
		s.st = oscGround
		return
	}
	if s.ignore {
		return
	}
	// Only printable ASCII is meaningful in a 7501 payload. Anything else
	// means this is not one of ours (or is corrupted): stop buffering.
	if c < 0x20 || c > 0x7e || len(s.buf) >= maxOSCPayload {
		s.ignore = true
		return
	}
	s.buf = append(s.buf, c)
	// Decide early whether this is 7501 so other OSCs (titles, hyperlinks,
	// clipboard) are skipped without buffering.
	if n := len(s.buf); n <= len(oscNumber) {
		if c != oscNumber[n-1] {
			s.ignore = true
		}
	} else if n == len(oscNumber)+1 && c != ';' {
		s.ignore = true
	}
}

func (s *OSCScanner) finish(emit func(api.OSCBody)) {
	s.st = oscGround
	if s.ignore || len(s.buf) <= len(oscNumber)+1 {
		return
	}
	if s.buf[len(oscNumber)+1] == '?' {
		if s.OnQuery != nil {
			s.OnQuery()
		}
		return
	}
	if b, ok := ParseOSC7501(string(s.buf[len(oscNumber)+1:])); ok {
		emit(b)
	}
}

// QueryReply is the answer to an OSC 7501 support query: the protocol
// number followed by "?" (what Claude Code's probe matches on), ST-terminated.
const QueryReply = "\x1b]" + oscNumber + ";?\x1b\\"

var (
	validOSCStates = map[string]bool{"idle": true, "working": true, "blocked": true, "done": true, "error": true}
	validOSCKinds  = map[string]bool{"permission": true, "question": true, "auth": true}
)

// ParseOSC7501 parses the key=value list after "7501;". Fields may be
// separated by ':' or ';'. state is required and must be one of
// idle|working|blocked|done|error, or clear (the agent withdrew its status,
// e.g. on exit), which is reported as idle. kind, when present, must be
// permission|question|auth. A status carrying a non-empty id= describes a
// sub-task (Claude Code reports background tasks that way), not the
// session, and is not reported. Unknown keys are ignored so agents can add
// fields without breaking older holders.
func ParseOSC7501(payload string) (api.OSCBody, bool) {
	b := api.OSCBody{Raw: payload}
	var id string
	for _, field := range strings.FieldsFunc(payload, func(r rune) bool { return r == ';' || r == ':' }) {
		k, v, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "state":
			b.State = v
		case "app":
			if len(v) <= 64 {
				b.App = v
			}
		case "kind":
			b.Kind = v
		case "id":
			id = v
		}
	}
	if id != "" {
		return api.OSCBody{}, false
	}
	if b.State == "clear" {
		b.State, b.Kind = "idle", ""
	}
	if !validOSCStates[b.State] {
		return api.OSCBody{}, false
	}
	if b.Kind != "" && !validOSCKinds[b.Kind] {
		b.Kind = ""
	}
	return b, true
}
