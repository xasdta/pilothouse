// Package web serves the phone/browser app (an installable PWA embedded
// with go:embed) and the device API on the daemon's loopback TCP listener,
// plus the local-admin routes (pairing, login links, devices) on the Unix
// socket.
//
// Security model (BUILD-SPEC §2 invariant 5, §11):
//   - The TCP listener is loopback-only; remote devices reach it through
//     Tailscale Serve. Every request's Host must be on the allow-list
//     (127.0.0.1/localhost:<port> or the host of web.public_url), which
//     defeats DNS rebinding.
//   - Devices pair with a one-time code and register a P-256 public key.
//     They authenticate with an HttpOnly cookie "ph_dev" = "<id>.<secret>";
//     only SHA-256(secret) is stored and compared in constant time.
//   - State-changing requests need `X-Pilothouse: 1` and a same-origin Origin.
//   - Decisions additionally carry an ECDSA P-256 signature made with the
//     device's non-extractable WebCrypto key, a timestamp within 120 s and
//     a single-use nonce. The TCP listener never accepts unsigned decisions
//     (ntfy action tokens are 256-bit single-use capabilities scoped to one
//     request and behavior).
//
// Wiring (daemon): web.New(Deps{…, LocalGuard: agentCallerGuard}) then
// mount Handler() on the TCP listener and call RegisterLocal(unixMux).
// RegisterLocal wraps every local route with Deps.LocalGuard; if it is
// nil the routes fail closed (500) so an unguarded build can't hand out
// pairing codes to agents.
package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/config"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/paths"
)

// Deps are the web server's collaborators.
type Deps struct {
	Core   core.Core
	DB     *sql.DB // shared SQLite handle; web owns tables prefixed web_
	Config *config.Config
	Log    *slog.Logger

	// LocalGuard wraps the local-admin routes added by RegisterLocal. The
	// daemon passes its agent-caller guard (403 agent_caller when an
	// ancestor of the peer is an agent process). Required: nil makes those
	// routes answer 500 instead of running unguarded.
	LocalGuard func(http.Handler) http.Handler
	// Now overrides the clock (tests).
	Now func() time.Time
	// PushClient is the HTTP client for Web Push deliveries (tests inject
	// one that trusts a fake push service). Default: 15 s timeout.
	PushClient *http.Client
	// KeysDir overrides where vapid.json lives. Default: paths.Keys().
	KeysDir string
	// PingEvery is the keep-alive interval on /api/events (tests shorten
	// it). Default: 15 s. The app reconnects after 45 s of silence.
	PingEvery time.Duration
}

// Server is the web app + device API.
type Server struct {
	d       Deps
	log     *slog.Logger
	now     func() time.Time
	vapid   vapidKeys
	handler http.Handler
	static  *staticFiles

	pairLimit *limiter
	terminals terminalTokens // terminal.go

	mu       sync.Mutex
	streams  map[string]map[*int]context.CancelFunc // device id → live SSE streams
	lastSeen map[string]time.Time
	pruned   time.Time

	challenges map[string]passkeyChallenge // passkey.go; guarded by mu

	pubMu      sync.Mutex
	pubURL     string
	pubMtime   time.Time
	pubChecked time.Time

	pushMu sync.Mutex
	pushed map[string]pushedRequest // request id → devices its push reached (push.go)
}

// New migrates web_* tables and loads/creates VAPID keys.
func New(d Deps) (*Server, error) {
	if d.Core == nil || d.DB == nil {
		return nil, errors.New("web: Core and DB are required")
	}
	if d.Config == nil {
		d.Config = config.Default()
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.PushClient == nil {
		d.PushClient = &http.Client{Timeout: 15 * time.Second}
	}
	if d.PingEvery <= 0 {
		d.PingEvery = 15 * time.Second
	}
	if d.KeysDir == "" {
		d.KeysDir = paths.Keys()
	}
	s := &Server{
		d:         d,
		log:       d.Log.With("component", "web"),
		now:       d.Now,
		pairLimit: newLimiter(d.Now),
		streams:   map[string]map[*int]context.CancelFunc{},
		lastSeen:  map[string]time.Time{},
	}
	if err := migrate(context.Background(), d.DB); err != nil {
		return nil, err
	}
	v, err := loadOrCreateVAPID(d.KeysDir)
	if err != nil {
		return nil, err
	}
	s.vapid = v
	st, err := loadStatic()
	if err != nil {
		return nil, err
	}
	s.static = st
	s.pubURL = normalizePublicURL(d.Config.Web.PublicURL)
	if fi, err := os.Stat(paths.Config()); err == nil {
		s.pubMtime = fi.ModTime()
	}
	s.pubChecked = s.now()
	s.handler = s.routes()
	return s, nil
}

// Handler serves the PWA (/) and the device API (/api/*) on the TCP listener.
func (s *Server) Handler() http.Handler { return s.handler }

// BaseURL is the URL devices should use: config web.public_url, else http://<web.addr>.
func (s *Server) BaseURL() string {
	if u := s.PublicURL(); u != "" {
		return u
	}
	return s.localURL()
}

// PublicURL is web.public_url (reachable off this machine), or "".
// It follows edits to config.yaml (e.g. by `pilothouse remote tailscale`)
// without a daemon restart: the file's mtime is checked at most every 2 s.
func (s *Server) PublicURL() string {
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	now := s.now()
	if now.Sub(s.pubChecked) < 2*time.Second && !now.Before(s.pubChecked) {
		return s.pubURL
	}
	s.pubChecked = now
	fi, err := os.Stat(paths.Config())
	if err != nil || fi.ModTime().Equal(s.pubMtime) {
		return s.pubURL
	}
	s.pubMtime = fi.ModTime()
	if c, err := config.Load(); err == nil {
		s.pubURL = normalizePublicURL(c.Web.PublicURL)
	} else {
		s.log.Warn("reload config for public_url", "err", err)
	}
	return s.pubURL
}

func (s *Server) localURL() string {
	addr := s.d.Config.Web.Addr
	if addr == "" {
		addr = "127.0.0.1:7517"
	}
	return "http://" + addr
}

func normalizePublicURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func (s *Server) port() string {
	_, p, err := net.SplitHostPort(s.d.Config.Web.Addr)
	if err != nil || p == "" {
		return "7517"
	}
	return p
}

// origin describes how a request reached us, after the Host allow-list.
type origin struct {
	base  string // scheme://host the browser sees
	https bool
}

// checkHost implements the DNS-rebinding defense: the Host header must be
// a loopback name with our port, or the public URL's host.
func (s *Server) checkHost(r *http.Request) (origin, bool) {
	host := strings.ToLower(r.Host)
	p := s.port()
	switch host {
	case "127.0.0.1:" + p, "localhost:" + p, "[::1]:" + p:
		return origin{base: "http://" + host}, true
	}
	if pub := s.PublicURL(); pub != "" {
		u, _ := url.Parse(pub)
		ph := strings.ToLower(u.Host)
		def := ":443"
		if u.Scheme == "http" {
			def = ":80"
		}
		if host == ph || host == ph+def {
			return origin{base: u.Scheme + "://" + ph, https: u.Scheme == "https"}, true
		}
	}
	return origin{}, false
}

type ctxKey int

const (
	ctxOrigin ctxKey = iota
	ctxDevice
)

func originOf(r *http.Request) origin {
	o, _ := r.Context().Value(ctxOrigin).(origin)
	return o
}

func deviceOf(r *http.Request) *deviceRow {
	d, _ := r.Context().Value(ctxDevice).(*deviceRow)
	return d
}

// cspPolicy forbids inline script and style: the PWA uses only files
// served from this origin.
const cspPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; manifest-src 'self'; worker-src 'self'; font-src 'self'; object-src 'none'; " +
	"base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", cspPolicy)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(self), microphone=(), geolocation=(), payment=(), usb=(), bluetooth=(), interest-cohort=()")
		o, ok := s.checkHost(r)
		if !ok {
			writeErr(w, http.StatusMisdirectedRequest, "bad_host", "unknown host "+r.Host+" (allowed: localhost or web.public_url)")
			return
		}
		if o.https {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxOrigin, o)))
	})
}

// csrf guards state-changing requests: a custom header (forces a CORS
// preflight for cross-site callers) plus an exact same-origin Origin.
func csrf(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Pilothouse") != "1" {
			writeErr(w, http.StatusForbidden, "csrf", "missing X-Pilothouse header")
			return
		}
		if o := r.Header.Get("Origin"); o == "" || !strings.EqualFold(o, originOf(r).base) {
			writeErr(w, http.StatusForbidden, "csrf", "cross-origin request refused")
			return
		}
		next(w, r)
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	// App shell: every client route serves index.html.
	for _, p := range []string{"GET /{$}", "GET /pair", "GET /login", "GET /settings", "GET /sessions", "GET /s/{id}", "GET /r/{id}"} {
		mux.HandleFunc(p, s.serveIndex)
	}
	mux.HandleFunc("GET /sw.js", s.serveStatic)
	mux.HandleFunc("GET /manifest.webmanifest", s.serveStatic)
	mux.HandleFunc("GET /favicon.ico", s.serveStatic)
	mux.HandleFunc("GET /css/", s.serveStatic)
	mux.HandleFunc("GET /js/", s.serveStatic)
	mux.HandleFunc("GET /icons/", s.serveStatic)

	// Unauthenticated API.
	mux.HandleFunc("POST /api/pair", csrf(s.handlePair))
	mux.HandleFunc("POST /api/actions/{token}", s.handleAction)

	// Device API.
	a := s.auth
	mux.HandleFunc("GET /api/me", a(s.handleMe))
	mux.HandleFunc("GET /api/vapid", a(s.handleVAPID))
	mux.HandleFunc("GET /api/snapshot", a(s.handleSnapshot))
	mux.HandleFunc("GET /api/events", a(s.handleEvents))
	mux.HandleFunc("GET /api/sessions/{id}", a(s.handleSession))
	mux.HandleFunc("GET /api/sessions/{id}/events", a(s.handleSessionEvents))
	mux.HandleFunc("POST /api/sessions/{id}/seen", a(csrf(s.handleSeen)))
	mux.HandleFunc("GET /api/requests/{id}", a(s.handleRequest))
	mux.HandleFunc("POST /api/requests/{id}/decide", a(csrf(s.handleDecide)))
	mux.HandleFunc("GET /api/presence", a(s.handlePresence))
	mux.HandleFunc("PUT /api/presence", a(csrf(s.handleSetPresence)))
	mux.HandleFunc("POST /api/push/subscribe", a(csrf(s.handleSubscribe)))
	mux.HandleFunc("POST /api/push/unsubscribe", a(csrf(s.handleUnsubscribe)))
	mux.HandleFunc("POST /api/push/test", a(csrf(s.handlePushTest)))
	mux.HandleFunc("PATCH /api/device", a(csrf(s.handleRename)))
	mux.HandleFunc("POST /api/logout", a(csrf(s.handleLogout)))
	mux.HandleFunc("POST /api/passkeys/challenge", a(csrf(s.handlePasskeyChallenge)))
	mux.HandleFunc("POST /api/passkeys", a(csrf(s.handlePasskeyAdd)))
	mux.HandleFunc("POST /api/device/restore", a(csrf(s.handleRestore)))
	s.routesV02(mux, a)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	return s.secure(mux)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, api.Error{Error: msg, Code: code})
}

// writeCoreErr maps core errors to API errors.
func (s *Server) writeCoreErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, core.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, core.ErrAlreadyClosed):
		writeErr(w, http.StatusConflict, "already_decided", "already answered")
	case errors.Is(err, core.ErrForbidden):
		writeErr(w, http.StatusForbidden, "forbidden", "forbidden")
	case errors.Is(err, core.ErrUnsupported):
		writeUnsupported(w, strings.TrimPrefix(err.Error(), core.ErrUnsupported.Error()+": "))
	case errors.Is(err, core.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "invalid", strings.TrimPrefix(err.Error(), core.ErrInvalid.Error()+": "))
	default:
		s.log.Error("core call failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

// decodeBody reads a small JSON body strictly.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}
