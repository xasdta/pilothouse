// Package daemon is Pilothouse's per-user server. It owns the database, serves
// the local API on a Unix socket (BUILD-SPEC §4) and the web app on a
// loopback TCP listener, routes hook calls (§5), tracks presence and process
// liveness, and fans events out to every client.
//
// Run is the entry point; everything it depends on that touches the OS
// (adapters, presence, process inspection, policy, notifications, clock) is
// injectable through Options so tests can run a full daemon in-process.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/all"
	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/config"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/notify"
	"github.com/xasdta/pilothouse/internal/paths"
	"github.com/xasdta/pilothouse/internal/policy"
	"github.com/xasdta/pilothouse/internal/presence"
	"github.com/xasdta/pilothouse/internal/proc"
	"github.com/xasdta/pilothouse/internal/store"
	"github.com/xasdta/pilothouse/internal/task"
	"github.com/xasdta/pilothouse/internal/web"
)

// ErrAlreadyRunning is returned by Run when another daemon holds the lock.
var ErrAlreadyRunning = errors.New("another pilothouse daemon is already running (check with `pilothouse status`)")

// PolicyEngine is the part of *policy.Engine the daemon uses.
type PolicyEngine interface {
	Evaluate(in policy.Input) policy.Result
	Status() api.PolicyStatus
	Changed() bool
	// Reload re-reads the policy files. A file that fails to parse keeps
	// its last good rules (and reports the error in Status).
	Reload() api.PolicyStatus
}

// Options configures Run. Every field is optional.
type Options struct {
	// Config defaults to config.Load().
	Config *config.Config
	// Adapters defaults to the non-nil entries of all.List().
	Adapters []adapters.Adapter
	// Presence defaults to presence.NewDetector().
	Presence presence.Detector
	// Now defaults to time.Now.
	Now func() time.Time
	// PeerGuard vets callers of admin routes; defaults to ProcessGuard.
	PeerGuard PeerGuard
	// Alive reports whether a PID exists; defaults to proc.Alive.
	Alive func(pid int) bool
	// LoadPolicy builds the policy engine, once: the daemon keeps that
	// engine for its lifetime and calls Reload on it when files change, so
	// a broken edit never loses the last good rules. Defaults to
	// policy.Load with policy.DefaultOptions plus every adapter's protected
	// paths.
	LoadPolicy func() PolicyEngine
	// Notifier defaults to notify.New(cfg, webServer, log).
	Notifier core.Notifier
	// Log defaults to slog.Default().
	Log *slog.Logger
	// OnReady is called once the listeners accept connections.
	OnReady func(*Daemon)
	// PollInterval is the period of the presence, liveness and policy-reload
	// loops. Zero means 2 s.
	PollInterval time.Duration
	// DisableUpdateCheck turns off the daily update check regardless of config.
	DisableUpdateCheck bool
	// TaskStarter starts the agent of a task begun from a device;
	// defaults to task.SpawnStarter (a detached `pilothouse run` holder).
	TaskStarter task.Starter
}

// Daemon is a running daemon. It implements core.Core.
type Daemon struct {
	opts      Options
	cfg       *config.Config
	log       *slog.Logger
	adapters  map[api.AgentKind]adapters.Adapter
	detector  presence.Detector
	guard     PeerGuard
	alive     func(int) bool
	machine   string
	startedAt time.Time

	db       *store.DB
	broker   *broker
	waiters  *waiters
	git      *gitCache
	notifier core.Notifier
	tg       *telegramManager // telegram.go
	web      *web.Server
	webURL   string
	policy   atomic.Pointer[policyHolder]

	pres    presenceState
	plugins pluginHub // command channels of plugin agents (plugin.go)
	tasks   taskState // taskrun.go

	compacted bool // the database was compacted at least once (prune loop only)

	ctx  context.Context // cancelled when shutdown begins
	stop context.CancelFunc
	bg   sync.WaitGroup // background loops and timers
}

var _ core.Core = (*Daemon)(nil)

type policyHolder struct{ PolicyEngine }

// Run starts the daemon and blocks until ctx is cancelled or a client asks
// it to shut down. It returns ErrAlreadyRunning if another instance holds
// the lock.
func Run(ctx context.Context, opts Options) error {
	d, err := newDaemon(opts)
	if err != nil {
		return err
	}
	return d.run(ctx)
}

func newDaemon(opts Options) (*Daemon, error) {
	cfg := opts.Config
	if cfg == nil {
		var err error
		if cfg, err = config.Load(); err != nil {
			return nil, fmt.Errorf("load config: %w", err)
		}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 2 * time.Second
	}
	ads := opts.Adapters
	if ads == nil {
		for _, a := range all.List() {
			if a != nil {
				ads = append(ads, a)
			}
		}
	}
	d := &Daemon{
		opts:     opts,
		cfg:      cfg,
		log:      opts.Log,
		adapters: map[api.AgentKind]adapters.Adapter{},
		detector: opts.Presence,
		guard:    opts.PeerGuard,
		alive:    opts.Alive,
		machine:  machineName(cfg),
		waiters:  newWaiters(),
		git:      newGitCache(30*time.Second, opts.Now, gitRepoInfo),
	}
	for _, a := range ads {
		d.adapters[a.Kind()] = a
	}
	if d.detector == nil {
		d.detector = presence.NewDetector()
	}
	if d.guard == nil {
		d.guard = ProcessGuard(ads)
	}
	if d.alive == nil {
		d.alive = proc.Alive
	}
	return d, nil
}

// now is the daemon's clock: UTC without a monotonic reading, so times
// survive a JSON round trip unchanged.
func (d *Daemon) now() time.Time { return d.opts.Now().UTC().Round(0) }

func (d *Daemon) run(parent context.Context) (err error) {
	d.startedAt = d.now()
	if err := paths.Ensure(); err != nil {
		return fmt.Errorf("create %s: %w", paths.Home(), err)
	}
	unlock, err := acquireLock(filepath.Join(paths.Run(), "daemon.lock"))
	if err != nil {
		return err
	}
	defer unlock()

	if d.db, err = store.Open(paths.DB()); err != nil {
		return err
	}
	defer func() {
		if cerr := d.db.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close database: %w", cerr)
		}
	}()
	d.db.SetClock(d.now)
	seq, err := d.db.MaxSeq(parent)
	if err != nil {
		return err
	}
	d.broker = newBroker(d.db, seq)
	d.db.SetOnCommit(d.broker.publish)

	d.ctx, d.stop = context.WithCancel(parent)
	defer d.stop()

	if err := d.recoverRequests(d.ctx); err != nil {
		return err
	}
	if err := d.initPresence(d.ctx); err != nil {
		return err
	}
	d.initPolicy()
	d.initWeb()
	d.watchClosedRequests() // withdraw.go
	d.startTelegram()       // telegram.go

	unixSrv, unixLn, err := d.listenUnix()
	if err != nil {
		return err
	}
	webSrv, webLn := d.listenWeb()

	serveErr := make(chan error, 2)
	go func() { serveErr <- serve(unixSrv, unixLn) }()
	if webSrv != nil {
		go func() { serveErr <- serve(webSrv, webLn) }()
	}
	d.startLoops()
	d.log.Info("pilothouse daemon started", "socket", paths.Socket(), "web", d.webURL, "pid", os.Getpid())
	if d.opts.OnReady != nil {
		d.opts.OnReady(d)
	}

	select {
	case <-d.ctx.Done():
	case err = <-serveErr:
		d.log.Error("listener failed", "err", err)
	}
	d.shutdown(unixSrv, webSrv)
	d.log.Info("pilothouse daemon stopped")
	return err
}

func serve(srv *http.Server, ln net.Listener) error {
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// shutdown hands every held request back to its agent, stops the listeners
// and waits for background work. The database is closed by run afterwards.
func (d *Daemon) shutdown(servers ...*http.Server) {
	d.stop()
	d.waiters.close() // no hook may start holding after the release below
	d.releaseHeld(context.Background(), "the pilothouse daemon is stopping")
	d.broker.close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range servers {
		if s == nil {
			continue
		}
		if err := s.Shutdown(ctx); err != nil {
			_ = s.Close()
		}
	}
	d.bg.Wait()
	closeNotifier(d.notifier)
}

// closeNotifier flushes queued deliveries. *notify.Notifier has Close();
// injected notifiers may not.
func closeNotifier(n core.Notifier) {
	switch c := n.(type) {
	case interface{ Close() }:
		c.Close()
	case interface{ Close() error }:
		_ = c.Close()
	}
}

// Shutdown begins a graceful stop; Run returns once it completes.
func (d *Daemon) Shutdown() { d.stop() }

// listenUnix binds the API socket. The lock is held, so any socket file
// left on disk belongs to a dead daemon and is removed.
func (d *Daemon) listenUnix() (*http.Server, net.Listener, error) {
	sock := paths.Socket()
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("remove stale socket %s: %w", sock, err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, nil, fmt.Errorf("listen on %s: %w", sock, err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		_ = ln.Close()
		return nil, nil, fmt.Errorf("chmod %s: %w", sock, err)
	}
	srv := &http.Server{
		Handler:           d.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(d.log.Handler(), slog.LevelDebug),
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, connKey{}, c)
		},
	}
	return srv, ln, nil
}

// initWeb builds the web server. A failure there must not take down the
// local inbox, so it is logged and the daemon runs without the web app.
func (d *Daemon) initWeb() {
	ws, err := web.New(web.Deps{Core: d, DB: d.db.SQL(), Config: d.cfg, Log: d.log.With("component", "web"),
		LocalGuard: d.requireHuman})
	if err != nil {
		d.log.Error("web app disabled", "err", err)
	} else {
		d.web = ws
	}
	d.tg = newTelegramManager(d)
	d.notifier = d.opts.Notifier
	if d.notifier == nil {
		var p notify.Pusher
		if d.web != nil {
			p = d.web
		}
		// notify adds component=notify itself; Telegram lives in its own
		// package, so the daemon adds its channel.
		d.notifier = notify.New(d.cfg, p, d.log, telegramChannel{d.tg})
	}
}

// listenWeb binds the loopback web listener. If the port is taken the
// daemon keeps running without it (the CLI and TUI still work) and says so.
func (d *Daemon) listenWeb() (*http.Server, net.Listener) {
	if d.web == nil || d.cfg.Web.Addr == "" {
		return nil, nil
	}
	ln, err := net.Listen("tcp", d.cfg.Web.Addr)
	if err != nil {
		d.log.Error("web app disabled: cannot listen", "addr", d.cfg.Web.Addr, "err", err)
		return nil, nil
	}
	d.webURL = "http://" + ln.Addr().String()
	srv := &http.Server{
		Handler:           d.web.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(d.log.Handler(), slog.LevelDebug),
	}
	return srv, ln
}

// WebURL is the local web app URL ("" when the web listener is down).
func (d *Daemon) WebURL() string { return d.webURL }

// acquireLock takes an exclusive flock on path so only one daemon runs per
// PILOTHOUSE_RUN_DIR. The kernel drops the lock if the process dies.
func acquireLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	// The PID is informational (for people debugging); the flock is the lock.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0)
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}

// machineName is config `machine`, else $PILOTHOUSE_MACHINE, else the hostname.
func machineName(cfg *config.Config) string { return config.MachineName(cfg) }

// goBackground runs fn on a tracked goroutine so shutdown can wait for it.
func (d *Daemon) goBackground(fn func()) {
	d.bg.Add(1)
	go func() {
		defer d.bg.Done()
		fn()
	}()
}

// every runs fn each interval until shutdown.
func (d *Daemon) every(interval time.Duration, fn func(ctx context.Context)) {
	d.goBackground(func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-d.ctx.Done():
				return
			case <-t.C:
				fn(d.ctx)
			}
		}
	})
}

// notify sends n, logging (never returning) delivery errors: a broken
// notification channel must not fail the hook that triggered it.
func (d *Daemon) notify(n core.Notification) {
	if err := d.notifier.Notify(d.ctx, n); err != nil {
		d.log.Warn("notification failed", "kind", n.Kind, "err", err)
	}
}
