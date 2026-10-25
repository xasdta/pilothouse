package wrap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xasdta/pilothouse/internal/paths"
)

// Conn is a client connection to a holder socket.
type Conn struct {
	c     net.Conn
	Hello ServerHello
	wmu   sync.Mutex
}

// HolderError is an error frame sent by a holder.
type HolderError struct{ Msg string }

func (e *HolderError) Error() string { return "holder: " + e.Msg }

// Dial connects to a holder and exchanges hellos. ctx bounds the dial and
// the handshake only.
func Dial(ctx context.Context, socket string, h Hello) (*Conn, error) {
	h.V = ProtocolVersion
	if h.Mode == "" {
		h.Mode = ModeAttach
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(helloTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = c.SetDeadline(deadline)
	if err := WriteFrame(c, FrameHello, encodeHello(h)); err != nil {
		c.Close()
		return nil, err
	}
	f, err := ReadFrame(c)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("wrap: holder handshake: %w", err)
	}
	switch f.Type {
	case FrameHello:
	case FrameError:
		c.Close()
		msg, _ := DecodeText(f.Payload)
		return nil, &HolderError{Msg: msg}
	default:
		c.Close()
		return nil, fmt.Errorf("wrap: holder sent %s before hello", f.Type)
	}
	sh, err := DecodeServerHello(f.Payload)
	if err != nil {
		c.Close()
		return nil, err
	}
	_ = c.SetDeadline(time.Time{})
	return &Conn{c: c, Hello: sh}, nil
}

// Send writes one frame; safe for concurrent use.
func (c *Conn) Send(t FrameType, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return WriteFrame(c.c, t, payload)
}

// Recv reads the next frame. Only one goroutine may call it.
func (c *Conn) Recv() (Frame, error) { return ReadFrame(c.c) }

// SetWriteDeadline bounds the next Sends (a holder whose agent stops
// reading its terminal stops reading input too).
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.c.SetWriteDeadline(t) }

// Close closes the connection without a detach frame; the holder treats
// it like a terminal going away.
func (c *Conn) Close() error { return c.c.Close() }

// ErrOldHolder is returned by DialCap when the holder predates the mode
// asked for: it was started by an older pilothouse (holders outlive
// upgrades of the binary).
var ErrOldHolder = errors.New("this session was started by an older version of pilothouse; start it again with `pilothouse run` to use this")

// capFor is the capability a hello mode needs ("" for the 0.1 modes).
func capFor(m HelloMode) string {
	switch m {
	case ModeWatch:
		return CapWatch
	case ModeInput:
		return CapInput
	}
	return ""
}

// DialCap connects in a 0.2 mode (watch or input) only after a probe
// shows the holder supports it, so an older holder never receives a
// hello it doesn't understand. Use it instead of Dial for those modes.
func DialCap(ctx context.Context, socket string, h Hello) (*Conn, error) {
	need := capFor(h.Mode)
	if need == "" {
		return nil, fmt.Errorf("wrap: DialCap is for watch and input modes, not %q", h.Mode)
	}
	sh, err := Probe(ctx, socket)
	if err != nil {
		return nil, err
	}
	if !sh.Has(need) {
		return nil, ErrOldHolder
	}
	c, err := Dial(ctx, socket, h)
	if err != nil {
		return nil, err
	}
	if c.Hello.SessionID != sh.SessionID || !c.Hello.Has(need) {
		c.Close()
		return nil, ErrOldHolder
	}
	return c, nil
}

// Probe asks a holder who it is without attaching.
func Probe(ctx context.Context, socket string) (ServerHello, error) {
	c, err := Dial(ctx, socket, Hello{Mode: ModeProbe})
	if err != nil {
		return ServerHello{}, err
	}
	c.Close()
	return c.Hello, nil
}

// HolderInfo is a live holder found in the run directory.
type HolderInfo struct {
	Socket string
	ServerHello
}

// ListHolders probes every holder socket in paths.Run(). Sockets nobody
// answers on are stale (a crashed holder) and are skipped.
func ListHolders(ctx context.Context) ([]HolderInfo, error) {
	ents, err := os.ReadDir(paths.Run())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out []HolderInfo
	)
	for _, e := range ents {
		name := e.Name()
		if !strings.HasPrefix(name, "h-") || !strings.HasSuffix(name, ".sock") {
			continue
		}
		sock := filepath.Join(paths.Run(), name)
		wg.Add(1)
		go func() {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			sh, err := Probe(pctx, sock)
			if err != nil {
				return
			}
			mu.Lock()
			out = append(out, HolderInfo{Socket: sock, ServerHello: sh})
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	return out, nil
}

// Alive reports whether the holder of sessionID is up and its agent still
// running.
func Alive(ctx context.Context, sessionID string) bool {
	pctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	sh, err := Probe(pctx, paths.HolderSocket(sessionID))
	return err == nil && !sh.Exited
}

// Stop asks the holder of sessionID to end its agent (SIGTERM to the
// holder, which forwards it to the agent's process group and escalates to
// SIGKILL after a grace period) and waits until the holder is gone.
func Stop(ctx context.Context, sessionID string) error {
	sock := paths.HolderSocket(sessionID)
	pctx, cancel := context.WithTimeout(ctx, time.Second)
	sh, err := Probe(pctx, sock)
	cancel()
	if err != nil {
		return nil // nothing listening: already stopped
	}
	if err := syscall.Kill(sh.HolderPID, syscall.SIGTERM); err != nil {
		return fmt.Errorf("stop session %s: %w", sessionID, err)
	}
	deadline := time.After(termGrace + 5*time.Second)
	for {
		pctx, cancel := context.WithTimeout(ctx, time.Second)
		_, err := Probe(pctx, sock)
		cancel()
		if err != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("session %s did not stop (holder pid %d)", sessionID, sh.HolderPID)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
