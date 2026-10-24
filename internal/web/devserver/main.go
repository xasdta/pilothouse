//go:build devserver

// Command devserver runs the web app against the demo core
// (internal/demo) — sessions across Claude Code, Codex and OpenCode,
// requests with diffs and risk labels, a question, a plan, activity,
// insights, rules and tasks — for UI work, screenshots and the web e2e
// suite:
//
//	go run -tags devserver ./internal/web/devserver -addr 127.0.0.1:7600 [-live] [-admin 127.0.0.1:7601] [-tmp dir]
//
// It prints a one-time login link and a pairing link. -live keeps the
// agents busy and raises a new request every 30 s (otherwise the demo
// only reacts to what you do, which keeps screenshots stable). -admin
// serves the local admin routes (POST /v1/login-link, /v1/pair) on a
// second loopback port so tests can mint more links.
//
// Everything lives in a temp dir (PILOTHOUSE_HOME is overridden); nothing
// touches ~/.pilothouse.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/config"
	"github.com/xasdta/pilothouse/internal/demo"
	"github.com/xasdta/pilothouse/internal/web"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7600", "listen address")
	live := flag.Bool("live", false, "keep agents busy and raise new requests")
	legacyDemo := flag.Bool("demo", false, "same as -live")
	admin := flag.String("admin", "", "loopback address for the local admin routes (login links, pairing codes)")
	quietLog := flag.Bool("quiet", false, "log warnings only")
	tmp := flag.String("tmp", "", "parent directory for the throwaway home (default: the system temp dir)")
	flag.Parse()

	dir, err := os.MkdirTemp(*tmp, "pilothouse-dev")
	must(err)
	defer os.RemoveAll(dir)
	must(os.Setenv("PILOTHOUSE_HOME", dir))
	must(os.Setenv("PILOTHOUSE_USER_HOME", dir))

	db, err := sql.Open("sqlite", filepath.Join(dir, "dev.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	must(err)
	defer db.Close()

	dc := demo.New(demo.Options{Machine: "studio", Quiet: !*live && !*legacyDemo})

	cfg := config.Default()
	cfg.Web.Addr = *addr
	level := slog.LevelDebug
	if *quietLog {
		level = slog.LevelWarn
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	srv, err := web.New(web.Deps{
		Core: dc, DB: db, Config: cfg, Log: log,
		LocalGuard: func(h http.Handler) http.Handler { return h },
		KeysDir:    filepath.Join(dir, "keys"),
	})
	must(err)

	local := http.NewServeMux()
	srv.RegisterLocal(local)
	rec := httptest.NewRecorder()
	local.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/login-link", nil))
	var link api.LoginLink
	must(json.Unmarshal(rec.Body.Bytes(), &link))
	rec = httptest.NewRecorder()
	local.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/pair", nil))
	var pair api.PairStart
	must(json.Unmarshal(rec.Body.Bytes(), &pair))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go dc.Run(ctx)

	ln, err := net.Listen("tcp", *addr)
	must(err)
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	servers := []*http.Server{hs}
	if *admin != "" {
		host, _, err := net.SplitHostPort(*admin)
		if err != nil || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
			must(fmt.Errorf("-admin must be a loopback address, got %q", *admin))
		}
		aln, err := net.Listen("tcp", *admin)
		must(err)
		as := &http.Server{Handler: local, ReadHeaderTimeout: 5 * time.Second}
		servers = append(servers, as)
		go func() { _ = as.Serve(aln) }()
	}
	fmt.Printf("pilothouse devserver on http://%s\n  login: %s\n  pair:  %s\n", *addr, link.URL, pair.URL)
	if *admin != "" {
		fmt.Printf("  admin: http://%s (POST /v1/login-link)\n", *admin)
	}
	go func() {
		<-ctx.Done()
		for _, s := range servers {
			_ = s.Shutdown(context.Background())
		}
	}()
	if err := hs.Serve(ln); err != http.ErrServerClosed {
		must(err)
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "devserver:", err)
		os.Exit(1)
	}
}
