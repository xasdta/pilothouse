// Package wrap implements `pilothouse run`: a dtach-style holder process that
// owns an agent's PTY and outlives terminals and daemon restarts, the
// frame protocol on its Unix socket, the attach client, and the OSC 7501
// status parser (BUILD-SPEC §9).
package wrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/client"
	"github.com/xasdta/pilothouse/internal/paths"
)

const (
	// DefaultLinger is how long a holder stays up after its child exits,
	// so a client attaching late still learns the exit code.
	DefaultLinger = 5 * time.Second
	// termGrace is how long the child gets between SIGTERM and SIGKILL
	// when the holder itself is told to stop.
	termGrace = 10 * time.Second
	// helloTimeout bounds how long a connection may sit without a hello.
	helloTimeout = 5 * time.Second
	// peerQueue is how many output frames may wait for a slow client
	// before it is dropped. The PTY loop never blocks on a client.
	peerQueue = 1024
	// peerWriteTimeout drops a client that stops reading entirely.
	peerWriteTimeout = 30 * time.Second
	// drainWait bounds how long output still in the PTY is read after
	// the child exits; grandchildren holding the PTY open must not keep
	// the exit from being reported.
	drainWait = time.Second
)

// HolderConfig describes one wrapped session.
type HolderConfig struct {
	SessionID string        // required; see NewSessionID
	Name      string        // title shown in lists; default: basename of Dir
	TaskID    string        // set for `pilothouse task` sessions
	Agent     api.AgentKind // default: KindFromCommand(Argv[0])
	Argv      []string      // command and arguments; required
	Dir       string        // working directory; default: current
	Env       []string      // child environment; nil: os.Environ()
	Cols      uint16        // initial PTY size; default 80x24
	Rows      uint16
	Socket    string         // default: paths.HolderSocket(SessionID)
	Daemon    *client.Client // default: client.New()
	Linger    time.Duration  // default: DefaultLinger
	Log       *slog.Logger   // default: discard
}

// Holder owns a child process on a PTY and serves attach clients.
type Holder struct {
	cfg       HolderConfig
	log       *slog.Logger
	cmd       *exec.Cmd
	ptmx      *os.File
	ln        net.Listener
	rep       *reporter
	startedAt time.Time
	osc       OSCScanner // used only by the pump goroutine

	mu       sync.Mutex // guards everything below
	ring     *ring
	modes    *modeTracker
	cur      *peer // attached client, if any
	exited   bool
	exitCode int

	ptyWrite sync.Mutex // serialises input from successive clients
	conns    sync.WaitGroup

	openMu sync.Mutex
	open   map[net.Conn]struct{} // every connection, so shutdown can close those still in their hello
}

// RunHolder starts the child and serves its socket until the child has
// exited and the linger period is over. It returns the child's exit code.
// Cancelling ctx sends SIGTERM to the child's process group (SIGKILL after
// a grace period) and skips the linger.
func RunHolder(ctx context.Context, cfg HolderConfig) (int, error) {
	h, err := newHolder(cfg)
	if err != nil {
		return 1, err
	}
	return h.run(ctx)
}

func newHolder(cfg HolderConfig) (*Holder, error) {
	if cfg.SessionID == "" || len(cfg.Argv) == 0 {
		return nil, errors.New("wrap: holder needs a session id and a command")
	}
	if cfg.Dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		cfg.Dir = wd
	}
	if cfg.Name == "" {
		cfg.Name = filepath.Base(cfg.Dir)
	}
	if cfg.Agent == "" {
		cfg.Agent = KindFromCommand(cfg.Argv[0])
	}
	if cfg.Cols == 0 || cfg.Rows == 0 {
		cfg.Cols, cfg.Rows = 80, 24
	}
	if cfg.Socket == "" {
		cfg.Socket = paths.HolderSocket(cfg.SessionID)
	}
	if cfg.Daemon == nil {
		cfg.Daemon = client.New()
	}
	if cfg.Linger <= 0 {
		cfg.Linger = DefaultLinger
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Holder{
		cfg:   cfg,
		log:   cfg.Log.With("session", cfg.SessionID),
		ring:  newRing(ScrollbackSize),
		modes: newModeTracker(),
		open:  map[net.Conn]struct{}{},
	}, nil
}

func (h *Holder) run(ctx context.Context) (int, error) {
	ln, err := listenHolder(h.cfg.Socket)
	if err != nil {
		return 1, err
	}
	h.ln = ln
	defer os.Remove(h.cfg.Socket)
	defer ln.Close()

	if err := h.startChild(); err != nil {
		return 1, err
	}
	h.log.Info("child started", "pid", h.cmd.Process.Pid, "argv", h.cfg.Argv, "dir", h.cfg.Dir)

	repCtx, stopRep := context.WithCancel(context.WithoutCancel(ctx))
	defer stopRep()
	h.rep = newReporter(h.cfg.Daemon, api.WrappedSessionBody{
		SessionID:  h.cfg.SessionID,
		Agent:      h.cfg.Agent,
		Title:      h.cfg.Name,
		Cwd:        h.cfg.Dir,
		Cmdline:    h.cfg.Argv,
		PID:        h.cmd.Process.Pid,
		HolderPID:  os.Getpid(),
		HolderSock: h.cfg.Socket,
		TaskID:     h.cfg.TaskID,
	}, h.log)
	go h.rep.run(repCtx)

	h.osc.OnQuery = h.answerOSCQuery
	pumpDone := make(chan struct{})
	go func() { defer close(pumpDone); h.pump() }()
	acceptDone := make(chan struct{})
	go func() { defer close(acceptDone); h.accept() }()

	waitDone := make(chan struct{})
	go func() { defer close(waitDone); _ = h.cmd.Wait() }()

	stopped := false
	stop := ctx.Done()
	var kill <-chan time.Time
	for done := false; !done; {
		select {
		case <-waitDone:
			done = true
		case <-stop:
			stopped, stop = true, nil
			h.log.Info("stopping: sending SIGTERM to the agent")
			h.signalChild(syscall.SIGTERM)
			kill = time.After(termGrace)
		case <-kill:
			h.log.Warn("agent ignored SIGTERM; sending SIGKILL")
			h.signalChild(syscall.SIGKILL)
		}
	}
	code := exitCode(h.cmd.ProcessState)
	h.log.Info("child exited", "code", code)

	// Let the pump forward what the child printed last.
	select {
	case <-pumpDone:
	case <-time.After(drainWait):
	}
	h.mu.Lock()
	h.exited, h.exitCode = true, code
	if p := h.cur; p != nil {
		h.cur = nil
		p.enqueue(mustFrame(FrameExit, EncodeExit(code)))
		p.finish()
	}
	h.mu.Unlock()
	h.rep.Exit(code)

	if stopped {
		// Being stopped (logout, shutdown): report the exit if the daemon
		// is there to hear it, but don't linger for late attaches.
		if h.rep.everRegistered.Load() {
			select {
			case <-h.rep.Done():
			case <-time.After(2 * time.Second):
			}
		}
	} else {
		// Linger so a client attaching late still gets the exit code; the
		// reporter uses the same window to reach the daemon.
		select {
		case <-time.After(h.cfg.Linger):
		case <-ctx.Done():
		}
	}
	stopRep()
	<-h.rep.Done()

	_ = ln.Close()
	<-acceptDone
	h.mu.Lock()
	if h.cur != nil {
		h.cur.kill()
		h.cur = nil
	}
	h.mu.Unlock()
	h.openMu.Lock()
	for c := range h.open {
		c.Close()
	}
	h.openMu.Unlock()
	// Bounded: a client goroutine can be stuck writing input to a PTY that
	// a leftover grandchild holds open but never reads.
	connsDone := make(chan struct{})
	go func() { h.conns.Wait(); close(connsDone) }()
	select {
	case <-connsDone:
	case <-time.After(drainWait):
		h.log.Warn("a client connection did not finish; exiting anyway")
	}
	select {
	case <-pumpDone:
	default:
		// Something the agent started still holds the PTY. Hang up its
		// process group as a closing terminal would; the pump ends when
		// the last holder of the PTY goes.
		h.signalChild(syscall.SIGHUP)
	}
	_ = h.ptmx.Close()
	return code, nil
}

// listenHolder binds the holder socket, replacing a stale one left by a
// crashed holder but refusing to steal a live one.
func listenHolder(sock string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(sock); err == nil {
		if c, err := net.DialTimeout("unix", sock, 500*time.Millisecond); err == nil {
			c.Close()
			return nil, fmt.Errorf("wrap: a holder is already listening on %s", sock)
		}
		if err := os.Remove(sock); err != nil {
			return nil, fmt.Errorf("wrap: remove stale socket: %w", err)
		}
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("wrap: listen on %s: %w", sock, err)
	}
	// The run directory is already 0700 (paths.Ensure); the chmod makes the
	// socket itself private too, whatever the umask.
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func (h *Holder) startChild() error {
	cmd := exec.Command(h.cfg.Argv[0], h.cfg.Argv[1:]...)
	cmd.Dir = h.cfg.Dir
	base := h.cfg.Env
	if base == nil {
		base = os.Environ()
	}
	// `pilothouse run` started from inside another wrapped session must not
	// inherit that session's identity or task.
	env := make([]string, 0, len(base)+4)
	for _, kv := range base {
		if !strings.HasPrefix(kv, "PILOTHOUSE_SESSION_ID=") && !strings.HasPrefix(kv, "PILOTHOUSE_TASK_ID=") {
			env = append(env, kv)
		}
	}
	env = append(env, "PILOTHOUSE=1", "PILOTHOUSE_SESSION_ID="+h.cfg.SessionID)
	if h.cfg.TaskID != "" {
		env = append(env, "PILOTHOUSE_TASK_ID="+h.cfg.TaskID)
	}
	if !hasEnv(env, "TERM") {
		// The holder has no terminal of its own; agents need a TERM to
		// render. xterm-256color is what every modern emulator speaks.
		env = append(env, "TERM=xterm-256color")
	}
	cmd.Env = env
	if h.cfg.Agent == api.AgentCodex && !hasEnv(env, "PILOTHOUSE_CODEX_SHARED_DAEMON") {
		// Hooks must run in the wrapped session's process tree; see codex.go.
		if argv, ok := codexNoDaemonArgv(h.cfg.Argv); ok && codexSupportsNoDaemon(argv[0], env, h.cfg.Dir) {
			h.log.Info("running codex without its shared background server so hooks join this session", "flag", "--no-daemon")
			h.cfg.Argv = argv
			cmd.Args = argv
		}
	}
	// pty.Start puts the child in a new session with the PTY as its
	// controlling terminal, so its PID is also its process group ID.
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: h.cfg.Cols, Rows: h.cfg.Rows})
	if err != nil {
		return fmt.Errorf("wrap: start %s: %w", h.cfg.Argv[0], err)
	}
	h.cmd, h.ptmx, h.startedAt = cmd, ptmx, time.Now()
	return nil
}

func hasEnv(env []string, key string) bool {
	for _, kv := range env {
		if len(kv) > len(key) && kv[len(key)] == '=' && kv[:len(key)] == key {
			return true
		}
	}
	return false
}

// signalChild signals the child's whole process group, so tools the
// agent spawned stop with it.
func (h *Holder) signalChild(sig syscall.Signal) {
	if err := syscall.Kill(-h.cmd.Process.Pid, sig); err != nil {
		_ = h.cmd.Process.Signal(sig)
	}
}

// exitCode maps a wait status to a shell-style exit code.
func exitCode(ps *os.ProcessState) int {
	if ps == nil {
		return 1
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

// pump copies PTY output to the scrollback, the mode tracker, the OSC
// scanner and the attached client. It must never block on anything but
// the PTY: the daemon is reached through the reporter and clients through
// their own queues.
func (h *Holder) pump() {
	buf := make([]byte, dataChunk)
	for {
		n, err := h.ptmx.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			h.osc.Feed(chunk, h.rep.OSC)
			frame := mustFrame(FrameData, chunk)
			h.mu.Lock()
			h.ring.Write(chunk)
			h.modes.Feed(chunk)
			if p := h.cur; p != nil && !p.enqueue(frame) {
				h.log.Warn("client too slow; detaching it")
				h.cur = nil
				p.kill()
			}
			h.mu.Unlock()
		}
		if err != nil {
			// EIO once the child and everything it spawned closed the PTY.
			if !errors.Is(err, io.EOF) && !errors.Is(err, syscall.EIO) && !errors.Is(err, os.ErrClosed) {
				h.log.Warn("pty read", "err", err)
			}
			return
		}
	}
}

// answerOSCQuery tells the agent that its terminal speaks OSC 7501: the
// holder is the agent's terminal as far as status goes, whatever terminal
// is attached (or none). Claude Code reports status only after this reply.
// The write happens off the pump goroutine, which must never block on
// PTY input.
func (h *Holder) answerOSCQuery() {
	go func() {
		h.ptyWrite.Lock()
		defer h.ptyWrite.Unlock()
		if _, err := h.ptmx.Write([]byte(QueryReply)); err != nil {
			h.log.Debug("answer OSC 7501 query", "err", err)
			return
		}
		h.log.Debug("answered OSC 7501 query")
	}()
}

func (h *Holder) accept() {
	for {
		c, err := h.ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				h.log.Warn("accept", "err", err)
			}
			return
		}
		h.openMu.Lock()
		h.open[c] = struct{}{}
		h.openMu.Unlock()
		h.conns.Add(1)
		go func() {
			defer h.conns.Done()
			defer func() {
				h.openMu.Lock()
				delete(h.open, c)
				h.openMu.Unlock()
			}()
			h.serve(c)
		}()
	}
}

func (h *Holder) serverHello() ServerHello {
	return ServerHello{
		V:         ProtocolVersion,
		SessionID: h.cfg.SessionID,
		Name:      h.cfg.Name,
		Agent:     string(h.cfg.Agent),
		TaskID:    h.cfg.TaskID,
		PID:       h.cmd.Process.Pid,
		HolderPID: os.Getpid(),
		Cwd:       h.cfg.Dir,
		StartedAt: h.startedAt.Unix(),
		Exited:    h.exited,
		ExitCode:  h.exitCode,
	}
}

// serve handles one connection: hello, then input until it leaves.
func (h *Holder) serve(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(helloTimeout))
	f, err := ReadFrame(c)
	if err != nil {
		c.Close()
		return
	}
	if f.Type != FrameHello {
		_ = WriteFrame(c, FrameError, encodeText("expected hello"))
		c.Close()
		return
	}
	hello, err := DecodeHello(f.Payload)
	if err != nil {
		_ = WriteFrame(c, FrameError, encodeText(err.Error()))
		c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Time{})

	if hello.Mode == ModeProbe {
		h.mu.Lock()
		sh := h.serverHello()
		h.mu.Unlock()
		_ = c.SetWriteDeadline(time.Now().Add(helloTimeout))
		_ = WriteFrame(c, FrameHello, encodeHello(sh))
		c.Close()
		return
	}

	p := newPeer(c)
	go p.writeLoop()

	// Hand the terminal over atomically with respect to the pump, so the
	// new client sees the scrollback followed by live output with no gap
	// and no duplicate.
	h.mu.Lock()
	p.enqueue(mustFrame(FrameHello, encodeHello(h.serverHello())))
	if r := h.modes.restore(); len(r) > 0 {
		p.enqueue(mustFrame(FrameData, r))
	}
	if hello.Replay {
		for b := h.ring.Bytes(); len(b) > 0; {
			n := min(len(b), dataChunk)
			p.enqueue(mustFrame(FrameData, b[:n]))
			b = b[n:]
		}
	}
	if h.exited {
		p.enqueue(mustFrame(FrameExit, EncodeExit(h.exitCode)))
		p.finish()
		h.mu.Unlock()
		h.drain(p)
		return
	}
	if old := h.cur; old != nil {
		old.enqueue(mustFrame(FrameDetach, encodeText("attached from another terminal")))
		old.finish()
	}
	h.cur = p
	h.mu.Unlock()
	h.log.Info("client attached", "replay", hello.Replay)

	if hello.Cols > 0 && hello.Rows > 0 {
		h.resize(hello.Cols, hello.Rows)
	}
	reason := h.readInput(p)
	h.mu.Lock()
	if h.cur == p {
		h.cur = nil
	}
	p.finish()
	h.mu.Unlock()
	<-p.dead
	h.log.Info("client detached", "reason", reason)
}

// readInput forwards a client's input until it detaches, disconnects or
// is replaced.
func (h *Holder) readInput(p *peer) string {
	for {
		f, err := ReadFrame(p.conn)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return "disconnected"
			}
			return err.Error()
		}
		h.mu.Lock()
		current, exited := h.cur == p, h.exited
		h.mu.Unlock()
		if !current {
			return "replaced"
		}
		switch f.Type {
		case FrameData:
			if exited {
				continue // nobody is reading the PTY any more
			}
			h.ptyWrite.Lock()
			_, err := h.ptmx.Write(f.Payload)
			h.ptyWrite.Unlock()
			if err != nil {
				return "pty closed"
			}
		case FrameResize:
			if cols, rows, err := DecodeResize(f.Payload); err == nil {
				h.resize(cols, rows)
			}
		case FrameDetach:
			return "detach"
		case FrameError:
			return "client error"
		default:
			// Unknown or out-of-place frames are ignored so newer clients
			// can add optional frames without breaking older holders.
		}
	}
}

// drain waits for a finished peer, discarding its input.
func (h *Holder) drain(p *peer) {
	go func() { _, _ = io.Copy(io.Discard, p.conn) }()
	<-p.dead
}

func (h *Holder) resize(cols, rows uint16) {
	if err := pty.Setsize(h.ptmx, &pty.Winsize{Cols: cols, Rows: rows}); err != nil {
		h.log.Debug("resize", "err", err)
	}
}

func mustFrame(t FrameType, payload []byte) []byte {
	b, err := AppendFrame(make([]byte, 0, headerLen+len(payload)), t, payload)
	if err != nil {
		panic(err) // callers only pass payloads far below MaxFrame
	}
	return b
}

// peer is one connected attach client. Frames are queued and written by
// its own goroutine so a slow terminal never stalls the PTY.
type peer struct {
	conn     net.Conn
	out      chan []byte // nil entry = close after writing what's queued
	finished bool        // guarded by Holder.mu
	once     sync.Once
	dead     chan struct{}
}

func newPeer(c net.Conn) *peer {
	return &peer{conn: c, out: make(chan []byte, peerQueue), dead: make(chan struct{})}
}

// enqueue queues a frame; false means the queue is full. Callers hold
// Holder.mu, which orders enqueue against finish.
func (p *peer) enqueue(frame []byte) bool {
	if p.finished {
		return true
	}
	select {
	case p.out <- frame:
		return true
	default:
		return false
	}
}

// finish closes the connection once queued frames are written.
func (p *peer) finish() {
	if p.finished {
		return
	}
	p.finished = true
	select {
	case p.out <- nil:
	default:
		p.kill()
	}
}

func (p *peer) kill() {
	p.once.Do(func() {
		close(p.dead)
		p.conn.Close()
	})
}

func (p *peer) writeLoop() {
	defer p.kill()
	for {
		select {
		case b := <-p.out:
			if b == nil {
				return
			}
			_ = p.conn.SetWriteDeadline(time.Now().Add(peerWriteTimeout))
			if _, err := p.conn.Write(b); err != nil {
				return
			}
		case <-p.dead:
			return
		}
	}
}
