package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/config"
)

// fakeAwake is an awakeRunner that records starts and stops.
type fakeAwake struct {
	mu      sync.Mutex
	starts  int
	stops   int
	pids    []int
	fail    error
	running []*fakeAwakeProc
}

type fakeAwakeProc struct {
	f    *fakeAwake
	once sync.Once
	done chan struct{}
}

func newFakeAwake() *fakeAwake { return &fakeAwake{} }

func (f *fakeAwake) runner() *awakeRunner {
	return &awakeRunner{Tool: "fake", Start: func(pid int) (awakeProc, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.fail != nil {
			return nil, f.fail
		}
		f.starts++
		f.pids = append(f.pids, pid)
		p := &fakeAwakeProc{f: f, done: make(chan struct{})}
		f.running = append(f.running, p)
		return p, nil
	}}
}

func (f *fakeAwake) counts() (starts, stops int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.stops
}

// die ends the newest process as if someone killed it.
func (f *fakeAwake) die() {
	f.mu.Lock()
	p := f.running[len(f.running)-1]
	f.mu.Unlock()
	p.once.Do(func() { close(p.done) })
}

func (p *fakeAwakeProc) Stop() {
	p.once.Do(func() {
		p.f.mu.Lock()
		p.f.stops++
		p.f.mu.Unlock()
		close(p.done)
	})
}
func (p *fakeAwakeProc) Done() <-chan struct{} { return p.done }
func (p *fakeAwakeProc) Err() string           { return "killed" }

func awakeDaemon(mode string, r *awakeRunner) *Daemon {
	d := &Daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil)), cfg: config.Default()}
	d.replies.awake.mode, d.replies.awake.runner = keepAwakeMode(mode), r
	return d
}

func TestKeepAwakeHoldAndRelease(t *testing.T) {
	f := newFakeAwake()
	d := awakeDaemon("", f.runner())
	t0 := time.Unix(1_800_000_000, 0)

	d.updateKeepAwake(false, t0)
	if s, _ := f.counts(); s != 0 {
		t.Fatal("started with nothing to do")
	}
	d.updateKeepAwake(true, t0)
	d.updateKeepAwake(true, t0.Add(time.Second))
	if s, _ := f.counts(); s != 1 {
		t.Fatalf("starts = %d, want 1", s)
	}
	if st := d.KeepAwake(); !st.Active || st.Tool != "fake" || st.Mode != "working" || !st.Since.Equal(t0) {
		t.Fatalf("status %+v", st)
	}
	// Idle, but within the linger: still held.
	d.updateKeepAwake(false, t0.Add(time.Second+keepAwakeLinger-time.Millisecond))
	if _, st := f.counts(); st != 0 {
		t.Fatal("released before the linger")
	}
	d.updateKeepAwake(false, t0.Add(time.Second+keepAwakeLinger))
	if s, st := f.counts(); s != 1 || st != 1 || d.KeepAwake().Active {
		t.Fatalf("after linger: starts=%d stops=%d", s, st)
	}
	// Busy again: held again, for the daemon's own pid (caffeinate -w).
	d.updateKeepAwake(true, t0.Add(2*time.Minute))
	if s, _ := f.counts(); s != 2 || f.pids[1] <= 1 {
		t.Fatalf("restart: starts=%d pids=%v", s, f.pids)
	}
	// Killed from outside long after it started: restarted on the next check.
	f.die()
	d.updateKeepAwake(true, t0.Add(3*time.Minute))
	if s, _ := f.counts(); s != 3 {
		t.Fatalf("not restarted after dying: starts=%d", s)
	}
	// Died right after starting: a failure; wait before trying again.
	f.die()
	d.updateKeepAwake(true, t0.Add(3*time.Minute+time.Second))
	if s, _ := f.counts(); s != 3 || d.KeepAwake().Error == "" {
		t.Fatalf("respawned a failing inhibitor at once: starts=%d status=%+v", s, d.KeepAwake())
	}
	d.updateKeepAwake(true, t0.Add(3*time.Minute+time.Second+keepAwakeRetry))
	if s, _ := f.counts(); s != 4 {
		t.Fatalf("no retry after the wait: starts=%d", s)
	}
	// Shutdown releases it, and nothing starts afterwards.
	d.releaseKeepAwake()
	d.updateKeepAwake(true, t0.Add(time.Hour))
	if s, st := f.counts(); s != 4 || st != 2 {
		t.Fatalf("after release: starts=%d stops=%d", s, st)
	}
}

func TestKeepAwakeOffAndFailures(t *testing.T) {
	f := newFakeAwake()
	d := awakeDaemon(config.KeepAwakeOff, f.runner())
	d.updateKeepAwake(true, time.Now())
	if s, _ := f.counts(); s != 0 || d.KeepAwake().Mode != "off" {
		t.Fatal("keep_awake: off still started")
	}
	// No tool on this machine: nothing to do, no error.
	d = awakeDaemon("", nil)
	d.updateKeepAwake(true, time.Now())
	if st := d.KeepAwake(); st.Active || st.Tool != "" {
		t.Fatalf("status without a tool: %+v", st)
	}
	// A start failure is reported and retried after a wait.
	f = newFakeAwake()
	f.fail = errors.New("no logind")
	d = awakeDaemon("", f.runner())
	now := time.Now()
	d.updateKeepAwake(true, now)
	if st := d.KeepAwake(); st.Active || st.Error != "no logind" {
		t.Fatalf("status after failure: %+v", st)
	}
	f.mu.Lock()
	f.fail = nil
	f.mu.Unlock()
	d.updateKeepAwake(true, now.Add(time.Second))
	if s, _ := f.counts(); s != 0 {
		t.Fatal("retried at once")
	}
	d.updateKeepAwake(true, now.Add(keepAwakeRetry))
	if s, _ := f.counts(); s != 1 || d.KeepAwake().Error != "" {
		t.Fatalf("retry: starts=%d status=%+v", s, d.KeepAwake())
	}
}

// Through the daemon: a working session holds it; shutdown releases it.
func TestKeepAwakeFollowsSessions(t *testing.T) {
	h := newHarness(t)
	f := newFakeAwake()
	h.opts = func(o *Options) { o.keepAwake = f.runner() }
	h.start(t)
	ctx := context.Background()
	h.hook(ctx, t, adapters.HookEvent{Kind: adapters.HookPromptSubmit, Message: "go"})
	waitFor(t, "keep-awake to start", func() bool { s, _ := f.counts(); return s == 1 })
	if !h.d.KeepAwake().Active {
		t.Fatal("status not active")
	}
	h.stop(t)
	if s, st := f.counts(); s != 1 || st != 1 {
		t.Fatalf("shutdown: starts=%d stops=%d", s, st)
	}
}

// The real macOS inhibitor: tied to the pid it watches, and Stop ends it.
func TestCaffeinateRunner(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("caffeinate is macOS only")
	}
	r := defaultAwakeRunner()
	if r == nil {
		t.Skip("no caffeinate")
	}
	// Watching a process that exits: caffeinate exits by itself.
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	p, err := r.Start(sleeper.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
		t.Fatalf("caffeinate exited at once: %s", p.Err())
	case <-time.After(300 * time.Millisecond):
	}
	_ = sleeper.Process.Kill()
	_ = sleeper.Wait()
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		p.Stop()
		t.Fatal("caffeinate outlived the process it watched")
	}
	// Stop ends one watching a live process.
	p, err = r.Start(syscall.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	p.Stop()
	select {
	case <-p.Done():
	default:
		t.Fatal("Stop returned before caffeinate exited")
	}
}
