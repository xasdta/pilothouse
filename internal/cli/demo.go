package cli

// Workstream F: `pilothouse demo` — the real web app against a sandboxed
// demo core with pretend agents (internal/demo), so a person can try the
// phone flow in 30 seconds without touching their daemon, hooks or data.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/config"
	"github.com/xasdta/pilothouse/internal/demo"
	"github.com/xasdta/pilothouse/internal/paths"
	"github.com/xasdta/pilothouse/internal/store"
	"github.com/xasdta/pilothouse/internal/web"
)

// Seams for tests.
var (
	// demoServe runs `tailscale serve` in the foreground for the demo's
	// lifetime (Tailscale drops a foreground serve when its process ends).
	demoServe = demoServeTailscale
	// demoReady is told the demo's local URL once it serves (tests).
	demoReady = func(string) {}
)

type demoOptions struct {
	addr     string
	noOpen   bool
	local    bool
	httpPort int
}

func newDemoCmd() *cobra.Command {
	var o demoOptions
	cmd := &cobra.Command{
		Use:   "demo",
		Short: "Try the phone app with pretend agents (sandboxed; touches nothing)",
		Long: `Runs the Pilothouse web app against a demo inbox with pretend agents that keep
asking for things, so you can try approving, answering and replying from your
phone or this computer's browser in 30 seconds.

Everything lives in a temporary directory that is deleted when you press
Ctrl-C. Your daemon, hooks, paired devices and settings are not touched.

To try it on your phone, the demo needs HTTPS that the phone can reach: when
Tailscale is connected with HTTPS certificates on, the demo serves itself on
your tailnet at https://<this machine>:8443 until it stops (pass --local to
skip that). Otherwise it runs in this computer's browser only.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runDemo(ctx, cmd.OutOrStdout(), o)
		},
	}
	cmd.Flags().StringVar(&o.addr, "addr", "127.0.0.1:0", "listen address (default: a free port on loopback)")
	cmd.Flags().BoolVar(&o.noOpen, "no-open", false, "don't open the browser")
	cmd.Flags().BoolVar(&o.local, "local", false, "this computer only: don't serve the demo on your tailnet")
	cmd.Flags().IntVar(&o.httpPort, "tailscale-port", 8443, "HTTPS port on your tailnet for the demo")
	return cmd
}

// demoEnv points every Pilothouse path at dir for the demo's lifetime and
// returns a function that restores the previous environment.
func demoEnv(dir string) (func(), error) {
	vars := map[string]string{
		"PILOTHOUSE_HOME":      dir,
		"PILOTHOUSE_RUN_DIR":   filepath.Join(dir, "run"),
		"PILOTHOUSE_USER_HOME": filepath.Join(dir, "user"),
	}
	type prev struct {
		v  string
		ok bool
	}
	saved := map[string]prev{}
	for k, v := range vars {
		old, ok := os.LookupEnv(k)
		saved[k] = prev{old, ok}
		if err := os.Setenv(k, v); err != nil {
			return nil, err
		}
	}
	return func() {
		for k, p := range saved {
			if p.ok {
				_ = os.Setenv(k, p.v)
			} else {
				_ = os.Unsetenv(k)
			}
		}
	}, nil
}

func runDemo(ctx context.Context, w io.Writer, o demoOptions) error {
	dir, err := os.MkdirTemp("", "pilothouse-demo-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	restore, err := demoEnv(dir)
	if err != nil {
		return err
	}
	defer restore()
	if err := paths.Ensure(); err != nil {
		return err
	}

	ln, err := net.Listen("tcp", o.addr)
	if err != nil {
		return fmt.Errorf("demo: listen on %s: %w", o.addr, err)
	}
	defer ln.Close()
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("demo: %s is not a loopback address; the web app only listens on this machine", o.addr)
	}

	cfg := config.Default()
	cfg.Web.Addr = ln.Addr().String()
	cfg.Notify.Desktop = false
	cfg.Machine = "demo"

	// Phone access over the tailnet, for the demo's lifetime only.
	var stopServe func()
	if !o.local {
		if url, stop, why := demoTailscale(ctx, port, o.httpPort); url != "" {
			cfg.Web.PublicURL, stopServe = url, stop
		} else if why != "" {
			fmt.Fprintln(w, "· "+why)
		}
	}
	if stopServe != nil {
		defer stopServe()
	}

	db, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer db.Close()
	logf, err := os.OpenFile(filepath.Join(paths.Logs(), "demo.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	log := slog.New(slog.NewTextHandler(logf, nil))

	core := demo.New(demo.Options{Machine: cfg.Machine})
	dctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go core.Run(dctx)

	srv, err := web.New(web.Deps{
		Core: core, DB: db.SQL(), Config: cfg, Log: log,
		// The local routes are never served: the demo calls them in-process.
		LocalGuard: func(h http.Handler) http.Handler { return h },
		KeysDir:    paths.Keys(),
	})
	if err != nil {
		return err
	}
	var login api.LoginLink
	var pair api.PairStart
	if err := demoLocal(srv, "/v1/login-link", &login); err != nil {
		return err
	}
	if err := demoLocal(srv, "/v1/pair", &pair); err != nil {
		return err
	}

	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second,
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelDebug)}
	serveErr := make(chan error, 1)
	go func() { serveErr <- hs.Serve(ln) }()

	local := "http://" + net.JoinHostPort(host, port)
	fmt.Fprintln(w, "Pilothouse demo — pretend agents in a sandbox. Your daemon, hooks and data are not touched.")
	fmt.Fprintln(w)
	if pair.Remote {
		fmt.Fprintln(w, "On your phone: scan this with the camera, name the device, and answer the requests:")
		tgQR(w, pair.URL)
		fmt.Fprintf(w, "  %s  (one-time; on your tailnet only)\n\n", pair.URL)
	} else {
		fmt.Fprintln(w, "On your phone: needs Tailscale with HTTPS certificates (phones need HTTPS for keys and")
		fmt.Fprintln(w, "notifications). Without it, try the demo in this computer's browser.")
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "On this computer: %s\n", login.URL)
	if !o.noOpen {
		if err := rmtOpenBrowser(login.URL); err == nil {
			fmt.Fprintln(w, "  (opened in your browser)")
		}
	}
	fmt.Fprintf(w, "\nsandbox: %s\nPress Ctrl-C to stop; the sandbox is deleted.\n", dir)
	demoReady(local)

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("demo: %w", err)
		}
	}
	cancel()
	sctx, scancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer scancel()
	_ = hs.Shutdown(sctx)
	fmt.Fprintln(w, "\nDemo stopped; sandbox deleted.")
	return nil
}

// demoLocal calls one of the web server's local-admin routes in-process.
func demoLocal(srv *web.Server, path string, v any) error {
	mux := http.NewServeMux()
	srv.RegisterLocal(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
	if rec.Code != http.StatusOK {
		return fmt.Errorf("demo: %s: %d %s", path, rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	return json.Unmarshal(rec.Body.Bytes(), v)
}

// demoTailscale serves the demo on the tailnet when Tailscale is ready and
// the port is free. It returns the public URL and a stop function, or why
// it didn't (empty when Tailscale simply isn't installed).
func demoTailscale(ctx context.Context, port string, httpsPort int) (url string, stop func(), why string) {
	bin, err := rmtTailscale.binary()
	if err != nil {
		return "", nil, ""
	}
	st, err := rmtTailscale.status(ctx, bin)
	host := strings.TrimSuffix(st.Self.DNSName, ".")
	switch {
	case err != nil || st.BackendState != "Running":
		return "", nil, "Tailscale isn't connected: the demo runs on this computer only"
	case host == "" || len(st.CertDomains) == 0:
		return "", nil, "Tailscale has no HTTPS certificates for this machine (https://login.tailscale.com/admin/dns): the demo runs on this computer only"
	}
	// Never take over a port someone already serves on.
	if out, err := rmtTailscale.output(ctx, bin, "serve", "status", "--json"); err == nil && demoPortTaken(out, httpsPort) {
		return "", nil, fmt.Sprintf("Tailscale already serves something on port %d (try --tailscale-port): the demo runs on this computer only", httpsPort)
	}
	stop, err = demoServe(ctx, bin, fmt.Sprintf("--https=%d", httpsPort), "http://127.0.0.1:"+port)
	if err != nil {
		return "", nil, "couldn't start `tailscale serve` (" + err.Error() + "): the demo runs on this computer only"
	}
	if httpsPort == 443 {
		return "https://" + host, stop, ""
	}
	return fmt.Sprintf("https://%s:%d", host, httpsPort), stop, ""
}

// demoPortTaken reads `tailscale serve status --json` for an HTTPS port.
func demoPortTaken(out []byte, port int) bool {
	var st struct {
		TCP map[string]json.RawMessage `json:"TCP"`
		Web map[string]json.RawMessage `json:"Web"`
	}
	if json.Unmarshal(out, &st) != nil {
		return false
	}
	p := fmt.Sprint(port)
	if _, ok := st.TCP[p]; ok {
		return true
	}
	for hp := range st.Web {
		if strings.HasSuffix(hp, ":"+p) {
			return true
		}
	}
	return false
}

// demoServeTailscale starts a foreground `tailscale serve`, which stops
// serving when the process ends; stop interrupts it and waits.
func demoServeTailscale(ctx context.Context, bin string, args ...string) (func(), error) {
	c := exec.Command(bin, append([]string{"serve"}, args...)...)
	var out strings.Builder
	c.Stdout, c.Stderr = &out, &out
	if err := c.Start(); err != nil {
		return nil, err
	}
	exited := make(chan error, 1)
	go func() { exited <- c.Wait() }()
	select {
	case err := <-exited: // it should keep running; exiting means it failed
		msg := strings.TrimSpace(out.String())
		if msg == "" && err != nil {
			msg = err.Error()
		}
		return nil, errors.New(inboxTrunc(msg, 200))
	case <-time.After(1500 * time.Millisecond):
	case <-ctx.Done():
	}
	return func() {
		_ = c.Process.Signal(os.Interrupt)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = c.Process.Kill()
			<-exited
		}
	}, nil
}
