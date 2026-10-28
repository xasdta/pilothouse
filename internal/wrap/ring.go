package wrap

import (
	"bytes"
	"crypto/rand"
	"encoding/base32"
	"path/filepath"
	"strings"

	"github.com/xasdta/pilothouse/internal/api"
)

// ScrollbackSize is how much recent output a holder keeps for `--replay`.
const ScrollbackSize = 256 << 10

// ring is a fixed-size byte ring holding the most recent output.
type ring struct {
	buf  []byte
	pos  int // next write position
	full bool
}

func newRing(n int) *ring { return &ring{buf: make([]byte, n)} }

func (r *ring) Write(p []byte) {
	if len(p) >= len(r.buf) {
		copy(r.buf, p[len(p)-len(r.buf):])
		r.pos, r.full = 0, true
		return
	}
	n := copy(r.buf[r.pos:], p)
	if n < len(p) {
		copy(r.buf, p[n:])
		r.full = true
	}
	r.pos = (r.pos + len(p)) % len(r.buf)
	if r.pos == 0 && len(p) > 0 {
		r.full = true
	}
}

// Bytes returns the retained output, oldest first. Once the ring has
// wrapped, the oldest bytes are usually the tail of a cut-off line or
// escape sequence, so replay starts after the first newline when one is
// near the front.
func (r *ring) Bytes() []byte {
	if !r.full {
		return bytes.Clone(r.buf[:r.pos])
	}
	out := make([]byte, 0, len(r.buf))
	out = append(out, r.buf[r.pos:]...)
	out = append(out, r.buf[:r.pos]...)
	if i := bytes.IndexByte(out[:min(len(out), 4096)], '\n'); i >= 0 {
		out = out[i+1:]
	}
	return out
}

var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// NewSessionID returns "s_" + 10 random lowercase base32 characters
// (BUILD-SPEC §4). The holder picks its own id so it can export
// PILOTHOUSE_SESSION_ID before the daemon has heard of it.
func NewSessionID() string { return NewID("s_") }

// NewID returns prefix + 10 crypto-random lowercase base32 characters.
func NewID(prefix string) string {
	var b [7]byte // 56 bits → 12 chars; trimmed to 10 (50 bits)
	_, _ = rand.Read(b[:])
	return prefix + idEncoding.EncodeToString(b[:])[:10]
}

// KindFromCommand picks the agent kind from the command's basename.
func KindFromCommand(cmd string) api.AgentKind {
	switch strings.ToLower(filepath.Base(cmd)) {
	case "claude":
		return api.AgentClaude
	case "codex":
		return api.AgentCodex
	case "opencode":
		return api.AgentOpenCode
	case "grok", "grok-build":
		return api.AgentGrok
	}
	return api.AgentGeneric
}
