package wrap

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// inputConn opens a ModeInput connection (through DialCap).
func inputConn(t *testing.T, sock string) *Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := DialCap(ctx, sock, Hello{Mode: ModeInput})
	if err != nil {
		t.Fatalf("input: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func send(t *testing.T, c *Conn, ft FrameType, p string) {
	t.Helper()
	if err := c.Send(ft, []byte(p)); err != nil {
		t.Fatalf("send %s: %v", ft, err)
	}
}

func TestHolderCapsInHello(t *testing.T) {
	testEnv(t)
	h := startHolder(t, HolderConfig{Argv: []string{"/bin/sh", "-c", "read x"}, Cols: 90, Rows: 33})
	sh, err := Probe(context.Background(), h.sock)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{CapWatch, CapInput, CapResize, CapPaste} {
		if !sh.Has(c) {
			t.Errorf("holder hello lacks %q: %v", c, sh.Caps)
		}
	}
	if sh.Cols != 90 || sh.Rows != 33 {
		t.Fatalf("size in hello %dx%d, want 90x33", sh.Cols, sh.Rows)
	}
}

// Watchers see replay and live output and never disturb the attached
// terminal; an input connection types without attaching; anything a
// watcher sends is ignored; everyone gets the exit.
func TestHolderWatchAndInput(t *testing.T) {
	testEnv(t)
	h := startHolder(t, HolderConfig{
		Argv: []string{"/bin/sh", "-c", `echo start; while read l; do echo "echo:$l"; [ "$l" = quit ] && exit 7; done`},
	})
	a := attachSession(t, h.sock, true)
	a.waitOutput("start")

	w1 := watchSession(t, h.sock, true)
	w1.waitOutput("start") // replayed
	w2 := watchSession(t, h.sock, false)
	if !w1.c.Hello.Has(CapWatch) || w2.c.Hello.SessionID != h.sid {
		t.Fatalf("watch hello %+v", w2.c.Hello)
	}

	in := inputConn(t, h.sock)
	send(t, in, FrameData, "one\r")
	for _, s := range []*session{a, w1, w2} {
		s.waitOutput("echo:one")
	}
	a.send("two\r")
	w2.waitOutput("echo:two")

	// A watcher's input and resize frames go nowhere.
	if err := w1.c.Send(FrameData, []byte("evil\r")); err != nil {
		t.Fatal(err)
	}
	_ = w1.c.Send(FrameResize, EncodeResize(10, 5))
	send(t, in, FrameData, "three\r")
	a.waitOutput("echo:three")
	if strings.Contains(a.output(), "echo:evil") {
		t.Fatal("a watcher's input reached the agent")
	}
	if len(w2.framesOf(FrameResize)) != 0 {
		t.Fatal("a watcher's resize was applied")
	}
	// Nobody was detached by the watchers or the input connection.
	if len(a.framesOf(FrameDetach)) != 0 {
		t.Fatalf("attached client was detached: %v", a.frames)
	}
	if info, err := Probe(context.Background(), h.sock); err != nil || info.Exited {
		t.Fatalf("probe %+v %v", info, err)
	}

	send(t, in, FrameData, "quit\r")
	for _, s := range []*session{a, w1, w2} {
		if code, _ := DecodeExit(s.waitFrame(FrameExit).Payload); code != 7 {
			t.Fatalf("exit %d", code)
		}
	}
	// A watcher arriving during the linger gets the replay and the exit.
	late := watchSession(t, h.sock, true)
	late.waitOutput("echo:quit")
	late.waitFrame(FrameExit)
	if code := h.wait(t); code != 7 {
		t.Fatalf("holder code %d", code)
	}
}

// rawAgent echoes the hex of the next n bytes it reads in raw mode.
func rawAgent(bracketed bool, n int) string {
	on := ""
	if bracketed {
		on = `\033[?2004h`
	}
	return `stty raw -echo; printf '` + on + `ready\r\n'; ` +
		`b=$(dd bs=1 count=` + strconv.Itoa(n) + ` 2>/dev/null | od -An -tx1 | tr -d ' \n'); printf 'got:%s\r\n' "$b"; stty sane; read x`
}

func hexOf(s string) string { return hex.EncodeToString([]byte(s)) }

func TestHolderPaste(t *testing.T) {
	testEnv(t)
	// With bracketed paste on: markers around the text, newlines as CR,
	// an embedded end marker defused (its ESC dropped).
	want := pasteStart + "a\rb[201~c" + pasteEnd
	h := startHolder(t, HolderConfig{Argv: []string{"/bin/sh", "-c", rawAgent(true, len(want))}})
	w := watchSession(t, h.sock, true)
	w.waitOutput("ready")
	in := inputConn(t, h.sock)
	send(t, in, FramePaste, "a\nb\x1b[201~c")
	w.waitOutput("got:" + hexOf(want))
	send(t, in, FrameData, "\r")
	h.wait(t)

	// Without bracketed paste the text is typed as is (newline → CR).
	want = "x\ry"
	h = startHolder(t, HolderConfig{Argv: []string{"/bin/sh", "-c", rawAgent(false, len(want))}})
	w = watchSession(t, h.sock, true)
	w.waitOutput("ready")
	in = inputConn(t, h.sock)
	send(t, in, FramePaste, "x\r\ny")
	w.waitOutput("got:" + hexOf(want))
	send(t, in, FrameData, "\r")
	h.wait(t)
}

func TestPasteBytes(t *testing.T) {
	for _, c := range []struct {
		in, want  string
		bracketed bool
	}{
		{"hello", "hello", false},
		{"hello", pasteStart + "hello" + pasteEnd, true},
		{"one\ntwo\r\nthree\rfour", "one\rtwo\rthree\rfour", false},
		{"tab\there", "tab\there", false},
		{"esc\x1b[201~\x1b[A", "esc[201~[A", false},
		{"ctl\x00\x03\x04\x7f!", "ctl!", false},
		{"c1\u009b31m", "c131m", false},
		{"utf8 é ✓ 🙂", "utf8 é ✓ 🙂", false},
		{"bad\xff\xfebytes", "badbytes", false},
		{"", pasteStart + pasteEnd, true},
	} {
		if got := string(pasteBytes([]byte(c.in), c.bracketed)); got != c.want {
			t.Errorf("pasteBytes(%q, %v) = %q, want %q", c.in, c.bracketed, got, c.want)
		}
	}
}

// Resizes from an input connection apply only while nobody is attached;
// watchers learn every size change.
func TestHolderInputResize(t *testing.T) {
	testEnv(t)
	h := startHolder(t, HolderConfig{Argv: []string{"/bin/sh", "-c", `while read l; do stty size; done`}})
	w := watchSession(t, h.sock, false)
	in := inputConn(t, h.sock)
	if err := in.Send(FrameResize, EncodeResize(50, 20)); err != nil {
		t.Fatal(err)
	}
	f := w.waitFrame(FrameResize)
	if c, r, _ := DecodeResize(f.Payload); c != 50 || r != 20 {
		t.Fatalf("watcher resize %dx%d", c, r)
	}
	send(t, in, FrameData, "\r")
	w.waitOutput("20 50")
	if sh, _ := Probe(context.Background(), h.sock); sh.Cols != 50 || sh.Rows != 20 {
		t.Fatalf("probe size %dx%d", sh.Cols, sh.Rows)
	}

	a := attachSession(t, h.sock, false) // 100x30
	waitFor(t, 5*time.Second, "attach resize reaches the watcher", func() bool { return len(w.framesOf(FrameResize)) == 2 })
	if err := in.Send(FrameResize, EncodeResize(40, 10)); err != nil {
		t.Fatal(err)
	}
	a.send("\r")
	a.waitOutput("30 100")
	send(t, in, FrameData, "\r")
	waitFor(t, 5*time.Second, "second size line", func() bool { return strings.Count(a.output(), "30 100") >= 2 })
	if strings.Contains(w.output(), "10 40") || len(w.framesOf(FrameResize)) != 2 {
		t.Fatalf("resize applied while attached: %q %d", w.output(), len(w.framesOf(FrameResize)))
	}
	h.stop()
	h.wait(t)
}

// A watcher that stops reading is dropped; the holder and later watchers
// carry on.
func TestHolderDropsSlowWatcher(t *testing.T) {
	testEnv(t)
	h := startHolder(t, HolderConfig{
		Argv: []string{"/bin/sh", "-c", `read go; head -c 8000000 /dev/zero | tr '\0' x; echo; echo done; read x`},
	})
	raw, err := net.Dial("unix", h.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := WriteFrame(raw, FrameHello, encodeHello(Hello{V: ProtocolVersion, Mode: ModeWatch})); err != nil {
		t.Fatal(err)
	}
	in := inputConn(t, h.sock)
	send(t, in, FrameData, "\r")

	// Wait for the output to finish, as seen by a well-behaved watcher
	// that joins late with replay (the end of the scrollback is enough).
	waitFor(t, 60*time.Second, "agent output done", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		c, err := DialCap(ctx, h.sock, Hello{Mode: ModeWatch, Replay: true})
		if err != nil {
			return false
		}
		defer c.Close()
		var out bytes.Buffer
		_ = c.c.SetReadDeadline(time.Now().Add(time.Second))
		for {
			f, err := c.Recv()
			if err != nil {
				break
			}
			out.Write(f.Payload)
			if bytes.Contains(out.Bytes(), []byte("done")) {
				return true
			}
		}
		return false
	})

	// The slow watcher's connection was closed: reading it to the end
	// yields EOF with no exit frame, long before 8 MB arrived.
	_ = raw.SetReadDeadline(time.Now().Add(20 * time.Second))
	n := 0
	for {
		f, err := ReadFrame(raw)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("slow watcher read: %v (after %d bytes)", err, n)
			}
			break
		}
		if f.Type == FrameExit {
			t.Fatal("slow watcher was not dropped")
		}
		n += len(f.Payload)
	}
	if n >= 8000000 {
		t.Fatalf("slow watcher got all %d bytes", n)
	}
	if _, err := Probe(context.Background(), h.sock); err != nil {
		t.Fatalf("holder gone: %v", err)
	}
	send(t, in, FrameData, "\r")
	h.wait(t)
}

func TestHolderWatcherLimit(t *testing.T) {
	testEnv(t)
	h := startHolder(t, HolderConfig{Argv: []string{"/bin/sh", "-c", "read x"}})
	for i := 0; i < maxWatchers; i++ {
		watchSession(t, h.sock, false)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := DialCap(ctx, h.sock, Hello{Mode: ModeWatch})
	var he *HolderError
	if !errors.As(err, &he) || !strings.Contains(he.Msg, "already watching") {
		t.Fatalf("watcher %d: %v", maxWatchers+1, err)
	}
	send(t, inputConn(t, h.sock), FrameData, "\r")
	h.wait(t)
}

// oldHolder speaks the 0.1 protocol: no caps, and it records every hello
// mode it is sent. (A real 0.1 holder refuses unknown modes; an attach
// would take the user's terminal.)
type oldHolder struct {
	sock  string
	mu    sync.Mutex
	modes []HelloMode
}

func startOldHolder(t *testing.T) *oldHolder {
	t.Helper()
	o := &oldHolder{sock: filepath.Join(os.Getenv("PILOTHOUSE_RUN_DIR"), "h-s_old.sock")}
	ln, err := net.Listen("unix", o.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				f, err := ReadFrame(c)
				if err != nil {
					return
				}
				var h struct {
					Mode HelloMode `json:"mode"`
				}
				_ = json.Unmarshal(f.Payload, &h)
				o.mu.Lock()
				o.modes = append(o.modes, h.Mode)
				o.mu.Unlock()
				_ = WriteFrame(c, FrameHello, []byte(`{"v":1,"session_id":"s_old","name":"old","agent":"claude","pid":1,"holder_pid":1}`))
			}()
		}
	}()
	return o
}

func TestDialCapNeverSendsNewModesToOldHolders(t *testing.T) {
	testEnv(t)
	o := startOldHolder(t)
	ctx := context.Background()
	for _, m := range []HelloMode{ModeWatch, ModeInput} {
		if _, err := DialCap(ctx, o.sock, Hello{Mode: m}); !errors.Is(err, ErrOldHolder) {
			t.Fatalf("%s on an old holder: %v", m, err)
		}
	}
	if _, err := DialCap(ctx, o.sock, Hello{Mode: ModeAttach}); err == nil {
		t.Fatal("DialCap accepted attach mode")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !slices.Equal(o.modes, []HelloMode{ModeProbe, ModeProbe}) {
		t.Fatalf("old holder saw modes %v, want only probes", o.modes)
	}
}
