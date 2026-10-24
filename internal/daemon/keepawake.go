package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/config"
)

// Keep-awake (config keep_awake: working | off). While any session is
// working or blocked on a person, the daemon keeps a sleep inhibitor
// running — `caffeinate -i` on macOS, `systemd-inhibit --what=idle:sleep`
// on Linux — and ends it a minute after the last one stops, and at
// shutdown. The inhibitor is tied to the daemon's lifetime twice over: it
// runs in its own process group that the daemon kills, and it watches
// the daemon's pid itself (caffeinate -w; a kill -0 loop under
// systemd-inhibit), so even a daemon killed with SIGKILL leaves nothing
// behind. Idle sleep only: closing a laptop's lid still sleeps it.

const (
	// keepAwakeLinger keeps the computer awake this long after the last
	// session stopped working.
	keepAwakeLinger = time.Minute
	// keepAwakeRetry is the wait after an inhibitor failed to start or
	// died right away.
	keepAwakeRetry = time.Minute
	// awakeStopWait bounds how long Stop waits before SIGKILL.
	awakeStopWait = 2 * time.Second
)

// awakeProc is a running sleep inhibitor.
type awakeProc interface {
	Stop()                 // ends it (and anything it started) and waits, bounded
	Done() <-chan struct{} // closed once it has exited
	Err() string           // why it exited, when it did on its own
}

// awakeRunner starts sleep inhibitors that also end on their own when
// process pid exits.
type awakeRunner struct {
	Tool  string // shown in status, e.g. "caffeinate -i"
	Start func(pid int) (awakeProc, error)
}

// keepAwake is the inhibitor's state (replyState.awake).
type keepAwake struct {
	mu       sync.Mutex
	inited   bool
	mode     string
	runner   *awakeRunner // nil: no tool on this machine
	proc     awakeProc
	since    time.Time
	lastBusy time.Time
	retryAt  time.Time
	lastErr  string
	closed   bool // shutdown: never start again
}

// KeepAwakeStatus describes keep-awake, for `pilothouse doctor`.
type KeepAwakeStatus struct {
	Mode   string    `json:"mode"`           // working | off
	Tool   string    `json:"tool,omitempty"` // "" when none is available
	Active bool      `json:"active"`         // an inhibitor is running now
	Since  time.Time `json:"since,omitempty"`
	Error  string    `json:"error,omitempty"` // last start failure
}

// KeepAwakeSupport describes keep-awake on this machine for a configured
// mode, without a daemon (Active is always false).
func KeepAwakeSupport(mode string) KeepAwakeStatus {
	st := KeepAwakeStatus{Mode: keepAwakeMode(mode)}
	if r := defaultAwakeRunner(); r != nil {
		st.Tool = r.Tool
	}
	return st
}

// KeepAwake reports the daemon's keep-awake state.
func (d *Daemon) KeepAwake() KeepAwakeStatus {
	k := &d.replies.awake
	k.mu.Lock()
	defer k.mu.Unlock()
	st := KeepAwakeStatus{Mode: keepAwakeMode(k.mode), Error: k.lastErr}
	if k.runner != nil {
		st.Tool = k.runner.Tool
	}
	if k.proc != nil {
		st.Active, st.Since = true, k.since
	}
	return st
}

func keepAwakeMode(m string) string {
	if m == config.KeepAwakeOff {
		return config.KeepAwakeOff
	}
	return config.KeepAwakeWorking
}

// startKeepAwake follows session states and holds or releases the
// inhibitor; shutdown releases it.
func (d *Daemon) startKeepAwake() {
	k := &d.replies.awake
	k.mu.Lock()
	k.inited, k.mode = true, keepAwakeMode(d.cfg.KeepAwake)
	if d.opts.keepAwake != nil {
		k.runner = d.opts.keepAwake
	} else {
		k.runner = defaultAwakeRunner()
	}
	enabled := k.mode != config.KeepAwakeOff && k.runner != nil
	k.mu.Unlock()
	if !enabled {
		return
	}
	d.every(d.opts.PollInterval, func(ctx context.Context) {
		busy, err := d.agentsBusy(ctx)
		if err != nil {
			return // keep the current state until the next check
		}
		d.updateKeepAwake(busy, d.now())
	})
	d.goBackground(func() {
		<-d.ctx.Done()
		d.releaseKeepAwake()
	})
}

// agentsBusy reports whether any session is working or waiting on a person.
func (d *Daemon) agentsBusy(ctx context.Context) (bool, error) {
	ss, err := d.db.LiveSessions(ctx)
	if err != nil {
		return false, err
	}
	for _, s := range ss {
		if s.State == api.StateWorking || s.State == api.StateBlocked {
			return true, nil
		}
	}
	return false, nil
}

// updateKeepAwake holds the inhibitor while busy and releases it
// keepAwakeLinger after the last busy check.
func (d *Daemon) updateKeepAwake(busy bool, now time.Time) {
	k := &d.replies.awake
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed || k.runner == nil || k.mode == config.KeepAwakeOff {
		return
	}
	if k.proc != nil {
		select {
		case <-k.proc.Done():
			// It ended on its own (killed, or failed). Started just now: a
			// failure; wait before trying again instead of spinning.
			why := firstNonEmpty(k.proc.Err(), "exited")
			if now.Sub(k.since) < 5*time.Second {
				k.lastErr, k.retryAt = k.runner.Tool+": "+why, now.Add(keepAwakeRetry)
			}
			d.log.Warn("keep-awake ended unexpectedly", "tool", k.runner.Tool, "reason", why)
			k.proc = nil
		default:
		}
	}
	if busy {
		k.lastBusy = now
		if k.proc != nil || now.Before(k.retryAt) {
			return
		}
		p, err := k.runner.Start(os.Getpid())
		if err != nil {
			k.lastErr, k.retryAt = err.Error(), now.Add(keepAwakeRetry)
			d.log.Warn("could not keep the computer awake", "tool", k.runner.Tool, "err", err)
			return
		}
		k.proc, k.since, k.lastErr = p, now, ""
		d.log.Info("keeping the computer awake while agents work", "tool", k.runner.Tool)
		return
	}
	if k.proc != nil && now.Sub(k.lastBusy) >= keepAwakeLinger {
		k.proc.Stop()
		k.proc = nil
		d.log.Info("agents are idle; the computer may sleep again")
	}
}

// releaseKeepAwake stops the inhibitor for good (shutdown).
func (d *Daemon) releaseKeepAwake() {
	k := &d.replies.awake
	k.mu.Lock()
	defer k.mu.Unlock()
	k.closed = true
	if k.proc != nil {
		k.proc.Stop()
		k.proc = nil
		d.log.Info("keep-awake released")
	}
}

// defaultAwakeRunner picks this platform's inhibitor, or nil.
func defaultAwakeRunner() *awakeRunner {
	switch runtime.GOOS {
	case "darwin":
		path, err := exec.LookPath("caffeinate")
		if err != nil {
			return nil
		}
		return &awakeRunner{Tool: "caffeinate -i", Start: func(pid int) (awakeProc, error) {
			// -i: no idle sleep; -w: exit when the daemon does.
			return startAwake(exec.Command(path, "-i", "-w", strconv.Itoa(pid)))
		}}
	case "linux":
		path, err := exec.LookPath("systemd-inhibit")
		if err != nil {
			return nil
		}
		return &awakeRunner{Tool: "systemd-inhibit --what=idle:sleep", Start: func(pid int) (awakeProc, error) {
			watch := fmt.Sprintf("while kill -0 %d 2>/dev/null; do sleep 5; done", pid)
			return startAwake(exec.Command(path, "--what=idle:sleep", "--who=Pilothouse",
				"--why=Coding agents are working", "--mode=block", "/bin/sh", "-c", watch))
		}}
	}
	return nil
}

// execAwake is an inhibitor process in its own process group.
type execAwake struct {
	cmd    *exec.Cmd
	done   chan struct{}
	stderr *limitBuffer
}

func startAwake(cmd *exec.Cmd) (awakeProc, error) {
	cmd.SysProcAttr = awakeSysProcAttr()
	tb := &limitBuffer{max: 512}
	cmd.Stdout, cmd.Stderr = nil, tb
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &execAwake{cmd: cmd, done: make(chan struct{}), stderr: tb}
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

func (p *execAwake) Done() <-chan struct{} { return p.done }

func (p *execAwake) Err() string {
	msg := strings.TrimSpace(p.stderr.String())
	if ps := p.cmd.ProcessState; ps != nil && msg == "" {
		msg = ps.String()
	}
	return msg
}

// Stop ends the whole process group (systemd-inhibit and its watcher
// shell), escalating to SIGKILL.
func (p *execAwake) Stop() {
	pgid := p.cmd.Process.Pid
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-p.done:
	case <-time.After(awakeStopWait):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-p.done
	}
}

// limitBuffer keeps the first max bytes written to it (an error message).
type limitBuffer struct {
	mu  sync.Mutex
	b   []byte
	max int
}

func (t *limitBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if room := t.max - len(t.b); room > 0 {
		t.b = append(t.b, p[:min(len(p), room)]...)
	}
	return len(p), nil
}

func (t *limitBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
