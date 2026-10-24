package wrap

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// Holder socket protocol (BUILD-SPEC §9). Every message is a frame:
//
//	[type u8][len u32 big-endian][payload: len bytes]
//
// The client opens with a hello (JSON payload carrying the protocol
// version); the holder answers with its own hello, and only then do data,
// resize, detach, exit and error frames flow. Lengths are checked before
// anything is allocated so a hostile peer cannot make either side buffer
// more than MaxFrame bytes.
//
// Holders added in 0.2 list optional features in ServerHello.Caps: the
// watch and input hello modes and the paste frame. Holders outlive
// upgrades of the pilothouse binary, and a 0.1 holder refuses (and an
// older design would have attached for) any mode it doesn't know, so a
// client must probe first and use a newer mode only when the holder lists
// it; DialCap does exactly that.

// FrameType identifies a frame on the holder socket.
type FrameType uint8

const (
	FrameHello  FrameType = 1 // JSON Hello / ServerHello
	FrameData   FrameType = 2 // raw terminal bytes (input or output)
	FrameResize FrameType = 3 // cols u16, rows u16 (big-endian)
	FrameDetach FrameType = 4 // client→holder: leaving; holder→client: you were detached (payload: reason text)
	FrameExit   FrameType = 5 // holder→client: child exited, code i32 (big-endian)
	FrameError  FrameType = 6 // UTF-8 text; the sender closes after it
	// FramePaste (client→holder, CapPaste) is text to paste: the holder
	// wraps it in bracketed-paste markers when the agent turned bracketed
	// paste on, so a multi-line prompt arrives as one paste instead of a
	// line per Enter. Control characters other than tab and newline are
	// dropped first, so pasted text can't end the paste early or inject
	// key sequences.
	FramePaste FrameType = 7
)

func (t FrameType) String() string {
	switch t {
	case FrameHello:
		return "hello"
	case FrameData:
		return "data"
	case FrameResize:
		return "resize"
	case FrameDetach:
		return "detach"
	case FrameExit:
		return "exit"
	case FrameError:
		return "error"
	case FramePaste:
		return "paste"
	}
	return fmt.Sprintf("frame(%d)", uint8(t))
}

const (
	// ProtocolVersion is carried in both hellos; peers refuse a mismatch so
	// an old attach client never misreads a newer holder (holders outlive
	// upgrades of the pilothouse binary).
	ProtocolVersion = 1

	// MaxFrame bounds every payload.
	MaxFrame = 1 << 20
	// maxHello bounds hello payloads, which are small JSON objects.
	maxHello = 4 << 10
	// maxText bounds error and detach reason payloads.
	maxText = 4 << 10
	// dataChunk is the largest data payload either side sends; reads from
	// the PTY and from stdin are this size, so frames stay small and
	// interleave well with control frames.
	dataChunk = 32 << 10

	headerLen = 5
)

// ErrFrameTooLarge is returned for a header announcing more than MaxFrame bytes.
var ErrFrameTooLarge = errors.New("wrap: frame exceeds 1 MB limit")

// Frame is one decoded message.
type Frame struct {
	Type    FrameType
	Payload []byte
}

// AppendFrame appends the encoding of one frame to dst.
func AppendFrame(dst []byte, t FrameType, payload []byte) ([]byte, error) {
	if len(payload) > MaxFrame {
		return dst, ErrFrameTooLarge
	}
	dst = append(dst, byte(t), 0, 0, 0, 0)
	binary.BigEndian.PutUint32(dst[len(dst)-4:], uint32(len(payload)))
	return append(dst, payload...), nil
}

// WriteFrame writes one frame with a single Write call, so concurrent
// writers serialised by the caller never interleave partial frames.
func WriteFrame(w io.Writer, t FrameType, payload []byte) error {
	b, err := AppendFrame(make([]byte, 0, headerLen+len(payload)), t, payload)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// ReadFrame reads one frame. It returns io.EOF only on a clean boundary;
// a frame cut short yields io.ErrUnexpectedEOF.
func ReadFrame(r io.Reader) (Frame, error) {
	var h [headerLen]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > MaxFrame {
		return Frame{}, ErrFrameTooLarge
	}
	f := Frame{Type: FrameType(h[0])}
	if n == 0 {
		return f, nil
	}
	f.Payload = make([]byte, n)
	if _, err := io.ReadFull(r, f.Payload); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return Frame{}, err
	}
	return f, nil
}

// EncodeResize builds a resize payload.
func EncodeResize(cols, rows uint16) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b, cols)
	binary.BigEndian.PutUint16(b[2:], rows)
	return b
}

// DecodeResize parses a resize payload. Zero sizes are rejected: a 0×0
// window makes most TUIs divide by zero or render nothing.
func DecodeResize(p []byte) (cols, rows uint16, err error) {
	if len(p) != 4 {
		return 0, 0, fmt.Errorf("wrap: resize payload is %d bytes, want 4", len(p))
	}
	cols, rows = binary.BigEndian.Uint16(p), binary.BigEndian.Uint16(p[2:])
	if cols == 0 || rows == 0 {
		return 0, 0, fmt.Errorf("wrap: invalid window size %dx%d", cols, rows)
	}
	return cols, rows, nil
}

// EncodeExit builds an exit payload.
func EncodeExit(code int) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(int32(code)))
	return b
}

// DecodeExit parses an exit payload.
func DecodeExit(p []byte) (int, error) {
	if len(p) != 4 {
		return 0, fmt.Errorf("wrap: exit payload is %d bytes, want 4", len(p))
	}
	return int(int32(binary.BigEndian.Uint32(p))), nil
}

// DecodeText parses an error or detach payload: bounded, valid UTF-8.
func DecodeText(p []byte) (string, error) {
	if len(p) > maxText {
		return "", fmt.Errorf("wrap: text payload is %d bytes, max %d", len(p), maxText)
	}
	if !utf8.Valid(p) {
		return "", errors.New("wrap: text payload is not valid UTF-8")
	}
	return string(p), nil
}

// encodeText truncates s to the text limit on a rune boundary.
func encodeText(s string) []byte {
	if len(s) <= maxText {
		return []byte(s)
	}
	b := []byte(s[:maxText])
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return b
}

// HelloMode says what a client wants from the holder.
type HelloMode string

const (
	// ModeAttach takes over the terminal: the previous client is detached.
	ModeAttach HelloMode = "attach"
	// ModeProbe only asks for the holder's ServerHello; the holder closes
	// the connection right after and never disturbs an attached client.
	ModeProbe HelloMode = "probe"
	// ModeWatch (CapWatch) streams output read-only: scrollback first
	// when Replay is set, then live data, resize frames when the
	// terminal's size changes and the exit frame. Several watchers may
	// connect; none of them touches the attached client, and a watcher
	// that falls behind is dropped.
	ModeWatch HelloMode = "watch"
	// ModeInput (CapInput) writes data and paste frames to the agent's
	// terminal without attaching; resize frames (CapResize) apply only
	// while no client is attached. The holder sends nothing after its
	// hello.
	ModeInput HelloMode = "input"
)

// Holder capabilities listed in ServerHello.Caps.
const (
	CapWatch  = "watch"
	CapInput  = "input"
	CapResize = "resize"
	CapPaste  = "paste"
)

// holderCaps is what this build's holder supports.
var holderCaps = []string{CapWatch, CapInput, CapResize, CapPaste}

// Hello is the client's first frame.
type Hello struct {
	V      int       `json:"v"`
	Mode   HelloMode `json:"mode"`
	Replay bool      `json:"replay,omitempty"` // send the scrollback ring before live output
	Cols   uint16    `json:"cols,omitempty"`
	Rows   uint16    `json:"rows,omitempty"`
}

// ServerHello is the holder's answer: who it is and whether its child is
// still running.
type ServerHello struct {
	V         int    `json:"v"`
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
	Agent     string `json:"agent"`
	TaskID    string `json:"task_id,omitempty"`
	PID       int    `json:"pid"`        // agent process
	HolderPID int    `json:"holder_pid"` // the holder itself
	Cwd       string `json:"cwd"`
	StartedAt int64  `json:"started_at"` // unix seconds
	Exited    bool   `json:"exited"`
	ExitCode  int    `json:"exit_code"`
	// Caps lists optional protocol features (0.2+; empty for older holders).
	Caps []string `json:"caps,omitempty"`
	// Cols and Rows are the agent terminal's current size (0.2+).
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
}

// Has reports whether the holder lists capability c.
func (h ServerHello) Has(c string) bool {
	for _, x := range h.Caps {
		if x == c {
			return true
		}
	}
	return false
}

func encodeHello(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Both hello types are plain structs of strings, ints and string slices.
		panic(err)
	}
	return b
}

// decodeHello unmarshals a hello payload and checks its version.
func decodeHello(p []byte, v any, version func() int) error {
	if len(p) > maxHello {
		return fmt.Errorf("wrap: hello is %d bytes, max %d", len(p), maxHello)
	}
	if err := json.Unmarshal(p, v); err != nil {
		return fmt.Errorf("wrap: bad hello: %w", err)
	}
	if got := version(); got != ProtocolVersion {
		return fmt.Errorf("wrap: holder protocol version %d, this pilothouse speaks %d — use the pilothouse binary that started the session", got, ProtocolVersion)
	}
	return nil
}

// DecodeHello parses a client hello.
func DecodeHello(p []byte) (Hello, error) {
	var h Hello
	if err := decodeHello(p, &h, func() int { return h.V }); err != nil {
		return h, err
	}
	switch h.Mode {
	case ModeAttach, ModeProbe, ModeWatch, ModeInput:
	default:
		return h, fmt.Errorf("wrap: unknown hello mode %q", h.Mode)
	}
	return h, nil
}

// DecodeServerHello parses a holder hello.
func DecodeServerHello(p []byte) (ServerHello, error) {
	var h ServerHello
	err := decodeHello(p, &h, func() int { return h.V })
	return h, err
}
