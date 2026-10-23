package web

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/config"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/notify"
	"github.com/xasdta/pilothouse/internal/web/webtest"
)

var _ notify.Pusher = (*Server)(nil)

const (
	testHost   = "127.0.0.1:7517"
	testOrigin = "http://127.0.0.1:7517"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type env struct {
	t     *testing.T
	fc    *webtest.FakeCore
	srv   *Server
	h     http.Handler
	db    *sql.DB
	cfg   *config.Config
	clock *clock
	local *http.ServeMux
}

// isolate points every Pilothouse path at temp dirs (BUILD-SPEC invariant 1).
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("PILOTHOUSE_HOME", home)
	t.Setenv("PILOTHOUSE_USER_HOME", t.TempDir())
	run, err := os.MkdirTemp("/tmp", "ag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(run) })
	t.Setenv("PILOTHOUSE_RUN_DIR", run)
	return home
}

func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newEnv(t *testing.T, mut ...func(*Deps)) *env {
	t.Helper()
	home := isolate(t)
	c := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	fc := webtest.New()
	fc.Now = c.Now
	cfg := config.Default()
	cfg.Web.Addr = testHost
	d := Deps{
		Core: fc, DB: openDB(t, filepath.Join(home, "test.db")), Config: cfg, Now: c.Now,
		LocalGuard: func(h http.Handler) http.Handler { return h },
	}
	for _, m := range mut {
		m(&d)
	}
	srv, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	local := http.NewServeMux()
	srv.RegisterLocal(local)
	return &env{t: t, fc: fc, srv: srv, h: srv.Handler(), db: d.DB, cfg: cfg, clock: c, local: local}
}

type reqOpt struct {
	cookie string
	host   string
	origin string // "-" = none
	noCSRF bool
	header map[string]string
}

func (e *env) do(method, path string, body any, o reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	r := httptest.NewRequest(method, path, rd)
	r.RemoteAddr = "127.0.0.1:50123" // the listener is loopback-only
	r.Host = e.cfg.Web.Addr
	if o.host != "" {
		r.Host = o.host
	}
	if method != "GET" {
		if !o.noCSRF {
			r.Header.Set("X-Pilothouse", "1")
		}
		switch o.origin {
		case "":
			r.Header.Set("Origin", "http://"+r.Host)
		case "-":
		default:
			r.Header.Set("Origin", o.origin)
		}
	}
	if o.cookie != "" {
		r.Header.Set("Cookie", cookieName+"="+o.cookie)
	}
	for k, v := range o.header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func (e *env) localDo(method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	e.local.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

func (e *env) pairCode() string {
	e.t.Helper()
	w := e.localDo("POST", "/v1/pair")
	if w.Code != 200 {
		e.t.Fatalf("POST /v1/pair: %d %s", w.Code, w.Body)
	}
	var p api.PairStart
	mustJSON(e.t, w, &p)
	return p.Code
}

func genKey(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return k, base64.RawURLEncoding.EncodeToString(der)
}

// pair redeems a fresh code for pub and returns the cookie value and device id.
func (e *env) pair(pub string) (string, string) {
	e.t.Helper()
	w := e.do("POST", "/api/pair", pairBody{Code: e.pairCode(), Name: "Test phone", PublicKey: pub}, reqOpt{})
	if w.Code != 200 {
		e.t.Fatalf("pair: %d %s", w.Code, w.Body)
	}
	var me meResponse
	mustJSON(e.t, w, &me)
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName {
			return c.Value, me.Device.ID
		}
	}
	e.t.Fatal("no cookie set")
	return "", ""
}

func mustJSON(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
}

func errCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var e api.Error
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	return e.Code
}

func signP1363(t *testing.T, k *ecdsa.PrivateKey, msg string) string {
	t.Helper()
	h := sha256.Sum256([]byte(msg))
	r, s, err := ecdsa.Sign(rand.Reader, k, h[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return base64.RawURLEncoding.EncodeToString(sig)
}

var nonceN int

func (e *env) signed(k *ecdsa.PrivateKey, id string, b api.Behavior, sc api.Scope) decideBody {
	nonceN++
	nonce := fmt.Sprintf("nonce-%016d", nonceN)
	ts := e.clock.Now().UnixMilli()
	return decideBody{Behavior: b, Scope: sc, TS: ts, Nonce: nonce, Sig: signP1363(e.t, k, CanonicalDecision(id, b, sc, ts, nonce))}
}

func (e *env) openRequest(id string) api.Request {
	now := e.clock.Now()
	r := api.Request{ID: id, SessionID: "s_test", Agent: api.AgentClaude, Checkpoint: api.CheckpointPermission, Tool: "Bash",
		ToolKind: api.ToolShell, Command: "git push origin main", Summary: "Run: git push origin main", Status: api.RequestOpen,
		Route: api.RouteDevices, CreatedAt: now, ExpiresAt: now.Add(9 * time.Minute)}
	e.fc.PutRequest(r)
	return r
}

// ---------------------------------------------------------------------------

func TestMigrateIdempotent(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 2; i++ {
		if err := migrate(context.Background(), e.db); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM web_migrations`).Scan(&n); err != nil || n != len(migrations)-1 {
		t.Fatalf("migrations recorded = %d, %v", n, err)
	}
}

func TestPairHappyPath(t *testing.T) {
	e := newEnv(t)
	w := e.localDo("POST", "/v1/pair")
	var p api.PairStart
	mustJSON(t, w, &p)
	if p.Remote || !strings.HasPrefix(p.URL, testOrigin+"/pair#") || len(normalizeCode(p.Code)) != 12 {
		t.Fatalf("pair start = %+v", p)
	}
	if got := p.ExpiresAt.Sub(e.clock.Now()); got != pairTTL {
		t.Fatalf("expiry = %v", got)
	}
	_, pub := genKey(t)
	// Codes are forgiving about case and dashes.
	w = e.do("POST", "/api/pair", pairBody{Code: strings.ToLower(strings.ReplaceAll(p.Code, "-", " ")), Name: "  Ada’s\x00 iPhone ", PublicKey: pub}, reqOpt{})
	if w.Code != 200 {
		t.Fatalf("pair: %d %s", w.Code, w.Body)
	}
	var me meResponse
	mustJSON(t, w, &me)
	if me.Device.Name != "Ada’s iPhone" || !strings.HasPrefix(me.Device.ID, "d_") || len(me.Device.ID) != 12 || me.VAPIDKey == "" {
		t.Fatalf("me = %+v", me)
	}
	ck := w.Result().Cookies()
	if len(ck) != 1 || !ck[0].HttpOnly || ck[0].SameSite != http.SameSiteStrictMode || ck[0].Secure || ck[0].Path != "/" {
		t.Fatalf("cookie = %+v", ck)
	}
	if w := e.do("GET", "/api/me", nil, reqOpt{cookie: ck[0].Value}); w.Code != 200 {
		t.Fatalf("me: %d", w.Code)
	}
	// The secret is stored hashed only.
	var stored []byte
	if err := e.db.QueryRow(`SELECT token_hash FROM web_devices`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	_, secret, _ := strings.Cut(ck[0].Value, ".")
	if string(stored) == secret || len(stored) != 32 {
		t.Fatal("token not hashed")
	}
	var ds []api.Device
	mustJSON(t, e.localDo("GET", "/v1/devices"), &ds)
	if len(ds) != 1 || ds[0].ID != me.Device.ID || ds[0].PublicKey != pub {
		t.Fatalf("devices = %+v", ds)
	}
	snap, _ := e.fc.Snapshot(context.Background())
	evs, _ := e.fc.Subscribe(context.Background(), 0)
	defer evs.Close()
	found := false
	for i := int64(0); i < snap.Seq; i++ {
		if ev := <-evs.C; ev.Type == api.EventDeviceChanged {
			found = true
		}
	}
	if !found {
		t.Fatal("no device.changed event")
	}
}

func TestPairCodeReuseExpiryAndBadInput(t *testing.T) {
	e := newEnv(t)
	_, pub := genKey(t)
	code := e.pairCode()
	if w := e.do("POST", "/api/pair", pairBody{Code: code, PublicKey: pub}, reqOpt{}); w.Code != 200 {
		t.Fatalf("first use: %d %s", w.Code, w.Body)
	}
	w := e.do("POST", "/api/pair", pairBody{Code: code, PublicKey: pub}, reqOpt{})
	if w.Code != 401 || errCode(t, w) != "invalid_code" {
		t.Fatalf("reuse: %d %s", w.Code, w.Body)
	}

	code = e.pairCode()
	e.clock.Add(pairTTL + time.Second)
	if w := e.do("POST", "/api/pair", pairBody{Code: code, PublicKey: pub}, reqOpt{}); w.Code != 401 {
		t.Fatalf("expired: %d %s", w.Code, w.Body)
	}

	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	der384, _ := x509.MarshalPKIXPublicKey(&p384.PublicKey)
	for name, key := range map[string]string{
		"garbage": "not base64 !!",
		"rsa":     base64.RawURLEncoding.EncodeToString(der),
		"p384":    base64.RawURLEncoding.EncodeToString(der384),
	} {
		if w := e.do("POST", "/api/pair", pairBody{Code: e.pairCode(), PublicKey: key}, reqOpt{}); w.Code != 400 {
			t.Errorf("%s key: %d %s", name, w.Code, w.Body)
		}
	}
	if w := e.do("POST", "/api/pair", map[string]any{"code": "x", "public_key": pub, "extra": 1}, reqOpt{}); w.Code != 400 {
		t.Errorf("unknown field: %d", w.Code)
	}
}

func TestPairRateLimit(t *testing.T) {
	e := newEnv(t)
	_, pub := genKey(t)
	var last *httptest.ResponseRecorder
	for i := 0; i < 8; i++ {
		last = e.do("POST", "/api/pair", pairBody{Code: "AAAA-BBBB-CCCC", PublicKey: pub}, reqOpt{})
		if last.Code == 429 {
			break
		}
	}
	if last.Code != 429 || errCode(t, last) != "rate_limited" || last.Header().Get("Retry-After") == "" {
		t.Fatalf("no rate limit: %d %s", last.Code, last.Body)
	}
	// A different tailnet peer (via the proxy's X-Forwarded-For) has its own bucket…
	w := e.do("POST", "/api/pair", pairBody{Code: e.pairCode(), PublicKey: pub}, reqOpt{header: map[string]string{"X-Forwarded-For": "100.64.0.9"}})
	if w.Code != 200 {
		t.Fatalf("other client: %d %s", w.Code, w.Body)
	}
	// …and the first one recovers with time.
	e.clock.Add(2 * time.Minute)
	if w := e.do("POST", "/api/pair", pairBody{Code: e.pairCode(), PublicKey: pub}, reqOpt{}); w.Code != 200 {
		t.Fatalf("after wait: %d %s", w.Code, w.Body)
	}
}

func TestLoginLink(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Config.Web.PublicURL = "https://mac.tail1.ts.net" })
	var l api.LoginLink
	mustJSON(t, e.localDo("POST", "/v1/login-link"), &l)
	if !strings.HasPrefix(l.URL, testOrigin+"/login#") || l.ExpiresAt.Sub(e.clock.Now()) != loginTTL {
		t.Fatalf("login link = %+v (must use the loopback URL)", l)
	}
	_, code, _ := strings.Cut(l.URL, "#")
	_, pub := genKey(t)
	if w := e.do("POST", "/api/pair", pairBody{Code: code, Name: "Chrome on Mac", PublicKey: pub}, reqOpt{}); w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	var p api.PairStart
	mustJSON(t, e.localDo("POST", "/v1/pair"), &p)
	if !p.Remote || !strings.HasPrefix(p.URL, "https://mac.tail1.ts.net/pair#") {
		t.Fatalf("pair with public URL = %+v", p)
	}
}

func TestAuthFailures(t *testing.T) {
	e := newEnv(t)
	_, pub := genKey(t)
	ck, id := e.pair(pub)
	devID, _, _ := strings.Cut(ck, ".")
	for name, c := range map[string]string{
		"none":         "",
		"no dot":       "garbage",
		"wrong secret": devID + ".AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"unknown id":   "d_zzzzzzzzzz.secret",
		"not a device": "x_1.secret",
	} {
		w := e.do("GET", "/api/snapshot", nil, reqOpt{cookie: c})
		if w.Code != 401 || errCode(t, w) != "unauthorized" {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if w := e.do("GET", "/api/snapshot", nil, reqOpt{cookie: ck}); w.Code != 200 {
		t.Fatalf("valid cookie: %d", w.Code)
	}
	if w := e.localDo("DELETE", "/v1/devices/"+id); w.Code != 204 {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/snapshot", nil, reqOpt{cookie: ck}); w.Code != 401 {
		t.Fatalf("revoked device still works: %d", w.Code)
	}
	if w := e.localDo("DELETE", "/v1/devices/"+id); w.Code != 404 {
		t.Fatalf("double revoke: %d", w.Code)
	}
	var ds []api.Device
	mustJSON(t, e.localDo("GET", "/v1/devices"), &ds)
	if len(ds) != 0 {
		t.Fatalf("revoked device listed: %+v", ds)
	}
}

func TestHostAllowList(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Config.Web.PublicURL = "https://mac.tail1.ts.net/" })
	for _, tc := range []struct {
		host string
		want int
	}{
		{"127.0.0.1:7517", 200},
		{"localhost:7517", 200},
		{"[::1]:7517", 200},
		{"mac.tail1.ts.net", 200},
		{"MAC.tail1.ts.net:443", 200},
		{"evil.example", 421},
		{"127.0.0.1:9999", 421},
		{"127.0.0.1", 421},
		{"mac.tail1.ts.net.evil.example", 421},
		{"", 421},
	} {
		w := e.do("GET", "/", nil, reqOpt{host: tc.host})
		if tc.host == "" {
			r := httptest.NewRequest("GET", "/", nil)
			r.Host = ""
			w = httptest.NewRecorder()
			e.h.ServeHTTP(w, r)
		}
		if w.Code != tc.want {
			t.Errorf("Host %q: %d, want %d", tc.host, w.Code, tc.want)
		}
	}
}

func TestCSRFAndOrigin(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Config.Web.PublicURL = "https://mac.tail1.ts.net" })
	_, pub := genKey(t)
	ck, _ := e.pair(pub)
	e.fc.PutSession(api.Session{ID: "s_test", Agent: api.AgentClaude, State: api.StateWaiting, Unseen: true})
	for _, tc := range []struct {
		name string
		o    reqOpt
		want int
	}{
		{"no header", reqOpt{noCSRF: true}, 403},
		{"no origin", reqOpt{origin: "-"}, 403},
		{"cross origin", reqOpt{origin: "https://evil.example"}, 403},
		{"other allowed host as origin", reqOpt{origin: "http://localhost:7517"}, 403},
		{"https origin on http host", reqOpt{origin: "https://127.0.0.1:7517"}, 403},
		{"same origin", reqOpt{}, 204},
		{"public host", reqOpt{host: "mac.tail1.ts.net", origin: "https://mac.tail1.ts.net"}, 204},
	} {
		tc.o.cookie = ck
		w := e.do("POST", "/api/sessions/s_test/seen", nil, tc.o)
		if w.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, w.Code, w.Body, tc.want)
		}
		if tc.want == 403 && errCode(t, w) != "csrf" {
			t.Errorf("%s: code %q", tc.name, errCode(t, w))
		}
	}
	// Pairing is CSRF-protected too.
	if w := e.do("POST", "/api/pair", pairBody{Code: e.pairCode(), PublicKey: pub}, reqOpt{origin: "https://evil.example"}); w.Code != 403 {
		t.Fatalf("cross-origin pair: %d", w.Code)
	}
}

func TestSecurityHeadersAndStatic(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Config.Web.PublicURL = "https://mac.tail1.ts.net" })
	w := e.do("GET", "/", nil, reqOpt{})
	h := w.Header()
	if w.Code != 200 || !strings.Contains(w.Body.String(), `<script type="module" src="/js/app.js">`) {
		t.Fatalf("index: %d", w.Code)
	}
	if csp := h.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || strings.Contains(csp, "unsafe-inline") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP = %q", csp)
	}
	for k, v := range map[string]string{"X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff"} {
		if h.Get(k) != v {
			t.Errorf("%s = %q", k, h.Get(k))
		}
	}
	if h.Get("Permissions-Policy") == "" || h.Get("Strict-Transport-Security") != "" {
		t.Errorf("Permissions-Policy %q, HSTS on http %q", h.Get("Permissions-Policy"), h.Get("Strict-Transport-Security"))
	}
	if w := e.do("GET", "/", nil, reqOpt{host: "mac.tail1.ts.net"}); w.Header().Get("Strict-Transport-Security") == "" {
		t.Error("no HSTS on https")
	}
	// No inline scripts or styles anywhere in the shell.
	body := w.Body.String()
	if strings.Contains(body, "<script>") || strings.Contains(body, "style=") || strings.Contains(body, "<style") {
		t.Error("index.html has inline script/style")
	}
	for path, ct := range map[string]string{
		"/sw.js": "text/javascript", "/js/app.js": "text/javascript", "/js/keystore.js": "text/javascript",
		"/css/app.css": "text/css", "/manifest.webmanifest": "application/manifest+json",
		"/icons/icon-192.png": "image/png", "/icons/icon.svg": "image/svg+xml", "/favicon.ico": "image/png",
		"/s/s_abc": "text/html", "/r/r_abc": "text/html", "/pair": "text/html", "/settings": "text/html",
	} {
		w := e.do("GET", path, nil, reqOpt{})
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), ct) {
			t.Errorf("%s: %d %q", path, w.Code, w.Header().Get("Content-Type"))
		}
	}
	w = e.do("GET", "/js/app.js", nil, reqOpt{})
	if w2 := e.do("GET", "/js/app.js", nil, reqOpt{header: map[string]string{"If-None-Match": w.Header().Get("ETag")}}); w2.Code != 304 {
		t.Errorf("ETag revalidation: %d", w2.Code)
	}
	for _, p := range []string{"/js/nope.js", "/index.html", "/static/index.html", "/../web.go"} {
		if w := e.do("GET", p, nil, reqOpt{}); w.Code != 404 && w.Code != 301 && w.Code != 307 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	if w := e.do("GET", "/api/nope", nil, reqOpt{}); w.Code != 404 || errCode(t, w) != "not_found" {
		t.Errorf("/api/nope: %d %s", w.Code, w.Body)
	}
}

func TestLocalRoutesFailClosedWithoutGuard(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.LocalGuard = nil })
	for _, p := range []string{"/v1/pair", "/v1/login-link"} {
		if w := e.localDo("POST", p); w.Code != 500 {
			t.Errorf("%s without guard: %d", p, w.Code)
		}
	}
	// The guard wraps every local route.
	guarded := 0
	e2 := newEnv(t, func(d *Deps) {
		d.LocalGuard = func(http.Handler) http.Handler {
			guarded++
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeErr(w, 403, "agent_caller", "no") })
		}
	})
	if guarded != 4 {
		t.Fatalf("guard applied to %d routes, want 4", guarded)
	}
	if w := e2.localDo("POST", "/v1/pair"); w.Code != 403 {
		t.Fatalf("guarded pair: %d", w.Code)
	}
}

type sigFixture struct {
	RequestID string `json:"request_id"`
	Behavior  string `json:"behavior"`
	Scope     string `json:"scope"`
	TS        int64  `json:"ts"`
	Nonce     string `json:"nonce"`
	PublicKey string `json:"public_key"`
	Canonical string `json:"canonical"`
	Sig       string `json:"sig"`
}

func loadFixture(t *testing.T) sigFixture {
	t.Helper()
	b, err := os.ReadFile("testdata/webcrypto-sig.json")
	if err != nil {
		t.Fatal(err)
	}
	var f sigFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestVerifyWebCryptoFixture(t *testing.T) {
	f := loadFixture(t)
	if got := CanonicalDecision(f.RequestID, api.Behavior(f.Behavior), api.Scope(f.Scope), f.TS, f.Nonce); got != f.Canonical {
		t.Fatalf("canonical mismatch:\n%q\n%q", got, f.Canonical)
	}
	pub, err := parsePublicKey(f.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := decodeB64(f.Sig)
	if !VerifyP1363(pub, []byte(f.Canonical), sig) {
		t.Fatal("WebCrypto signature rejected")
	}
	if VerifyP1363(pub, []byte(f.Canonical+"x"), sig) || VerifyP1363(pub, []byte(f.Canonical), sig[:63]) {
		t.Fatal("tampered input accepted")
	}
	if CanonicalDecision("r", api.BehaviorDeny, "", 5, "n") != "pilothouse-decide:v1\nr\ndeny\nonce\n5\nn" {
		t.Fatal("empty scope must sign as once")
	}
}

func TestDecideWithWebCryptoSignature(t *testing.T) {
	f := loadFixture(t)
	e := newEnv(t)
	e.clock.t = time.UnixMilli(f.TS).UTC().Add(30 * time.Second)
	ck, devID := e.pair(f.PublicKey)
	e.openRequest(f.RequestID)
	good := decideBody{Behavior: api.Behavior(f.Behavior), Scope: api.Scope(f.Scope), TS: f.TS, Nonce: f.Nonce, Sig: f.Sig}
	path := "/api/requests/" + f.RequestID + "/decide"

	for name, b := range map[string]decideBody{
		"behavior": {Behavior: api.BehaviorDeny, Scope: good.Scope, TS: good.TS, Nonce: good.Nonce, Sig: good.Sig},
		"deny":     {Behavior: api.BehaviorDeny, Scope: api.ScopeOnce, TS: good.TS, Nonce: good.Nonce, Sig: good.Sig},
		"scope":    {Behavior: good.Behavior, Scope: api.ScopeOnce, TS: good.TS, Nonce: good.Nonce, Sig: good.Sig},
		"ts":       {Behavior: good.Behavior, Scope: good.Scope, TS: good.TS + 1, Nonce: good.Nonce, Sig: good.Sig},
		"nonce":    {Behavior: good.Behavior, Scope: good.Scope, TS: good.TS, Nonce: good.Nonce + "x", Sig: good.Sig},
		"sig":      {Behavior: good.Behavior, Scope: good.Scope, TS: good.TS, Nonce: good.Nonce, Sig: "AAAA" + good.Sig[4:]},
	} {
		w := e.do("POST", path, b, reqOpt{cookie: ck})
		// deny+session is refused before the signature check (400).
		if (w.Code != 403 || errCode(t, w) != "bad_signature") && !(name == "behavior" && w.Code == 400) {
			t.Errorf("tampered %s: %d %s", name, w.Code, w.Body)
		}
	}
	// Signature for another request id.
	e.openRequest("r_other00001")
	if w := e.do("POST", "/api/requests/r_other00001/decide", good, reqOpt{cookie: ck}); w.Code != 403 {
		t.Errorf("signature replayed onto another request: %d", w.Code)
	}

	w := e.do("POST", path, good, reqOpt{cookie: ck})
	if w.Code != 200 {
		t.Fatalf("decide: %d %s", w.Code, w.Body)
	}
	var r api.Request
	mustJSON(t, w, &r)
	if r.Status != api.RequestDecided || r.Decision.By != "device:"+devID || r.Decision.Scope != api.ScopeSession || r.Decision.Behavior != api.BehaviorAllow {
		t.Fatalf("request = %+v / %+v", r, r.Decision)
	}
	// Replaying the exact signed body fails on the nonce.
	if w := e.do("POST", path, good, reqOpt{cookie: ck}); w.Code != 403 || errCode(t, w) != "replayed" {
		t.Fatalf("replay: %d %s", w.Code, w.Body)
	}
}

func TestDecideClockSkewAndValidation(t *testing.T) {
	e := newEnv(t)
	k, pub := genKey(t)
	ck, _ := e.pair(pub)
	e.openRequest("r_skew000001")
	path := "/api/requests/r_skew000001/decide"

	b := e.signed(k, "r_skew000001", api.BehaviorAllow, api.ScopeOnce)
	e.clock.Add(MaxClockSkew + time.Second)
	if w := e.do("POST", path, b, reqOpt{cookie: ck}); w.Code != 403 || errCode(t, w) != "stale_signature" {
		t.Fatalf("old ts: %d %s", w.Code, w.Body)
	}
	b = e.signed(k, "r_skew000001", api.BehaviorAllow, api.ScopeOnce)
	e.clock.Add(-(MaxClockSkew + time.Second))
	if w := e.do("POST", path, b, reqOpt{cookie: ck}); w.Code != 403 || errCode(t, w) != "stale_signature" {
		t.Fatalf("future ts: %d %s", w.Code, w.Body)
	}
	e.clock.Add(MaxClockSkew + time.Second)

	bad := e.signed(k, "r_skew000001", api.BehaviorDeny, api.ScopeSession)
	if w := e.do("POST", path, bad, reqOpt{cookie: ck}); w.Code != 400 {
		t.Errorf("deny for session: %d", w.Code)
	}
	ask := e.signed(k, "r_skew000001", api.BehaviorAsk, api.ScopeOnce)
	if w := e.do("POST", path, ask, reqOpt{cookie: ck}); w.Code != 400 {
		t.Errorf("behavior ask: %d", w.Code)
	}
	short := e.signed(k, "r_skew000001", api.BehaviorAllow, api.ScopeOnce)
	short.Nonce = "short"
	if w := e.do("POST", path, short, reqOpt{cookie: ck}); w.Code != 400 {
		t.Errorf("short nonce: %d", w.Code)
	}
	if w := e.do("POST", path, e.signed(k, "r_skew000001", api.BehaviorAllow, api.ScopeOnce), reqOpt{cookie: ck, noCSRF: true}); w.Code != 403 {
		t.Errorf("decide without CSRF header: %d", w.Code)
	}
	// Unsigned decisions never pass, even with a valid cookie.
	if w := e.do("POST", path, map[string]any{"behavior": "allow"}, reqOpt{cookie: ck}); w.Code == 200 {
		t.Fatal("unsigned decision accepted")
	}
	// Deny with a message works.
	d := e.signed(k, "r_skew000001", api.BehaviorDeny, api.ScopeOnce)
	d.Message = "use a PR"
	if w := e.do("POST", path, d, reqOpt{cookie: ck}); w.Code != 200 {
		t.Fatalf("deny: %d %s", w.Code, w.Body)
	}
	if ds := e.fc.Decisions(); len(ds) != 1 || ds[0].Message != "use a PR" {
		t.Fatalf("decisions = %+v", ds)
	}
	// Unknown request.
	if w := e.do("POST", "/api/requests/r_missing0001/decide", e.signed(k, "r_missing0001", api.BehaviorAllow, ""), reqOpt{cookie: ck}); w.Code != 404 {
		t.Errorf("missing request: %d", w.Code)
	}
}

func TestDecideRaceFirstAnswerWins(t *testing.T) {
	e := newEnv(t)
	type dev struct {
		k  *ecdsa.PrivateKey
		ck string
	}
	var devs []dev
	for i := 0; i < 6; i++ {
		k, pub := genKey(t)
		ck, _ := e.pair(pub)
		devs = append(devs, dev{k, ck})
	}
	e.openRequest("r_race000001")
	bodies := make([]decideBody, len(devs))
	for i, d := range devs {
		b := api.BehaviorAllow
		if i%2 == 1 {
			b = api.BehaviorDeny
		}
		bodies[i] = e.signed(d.k, "r_race000001", b, api.ScopeOnce)
	}
	codes := make([]int, len(devs))
	var wg sync.WaitGroup
	for i := range devs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := e.do("POST", "/api/requests/r_race000001/decide", bodies[i], reqOpt{cookie: devs[i].ck})
			codes[i] = w.Code
			if w.Code == 409 && errCode(t, w) != "already_decided" {
				t.Errorf("409 code = %q", errCode(t, w))
			}
		}()
	}
	wg.Wait()
	ok, conflict := 0, 0
	for _, c := range codes {
		switch c {
		case 200:
			ok++
		case 409:
			conflict++
		}
	}
	if ok != 1 || conflict != len(devs)-1 {
		t.Fatalf("codes = %v", codes)
	}
}

// ---- SSE ----

type sseClient struct {
	events chan [2]string // event, data
	done   chan error
	cancel context.CancelFunc
}

func (e *env) live(t *testing.T) *httptest.Server {
	ts := httptest.NewServer(e.h)
	t.Cleanup(ts.Close)
	e.cfg.Web.Addr = ts.Listener.Addr().String()
	return ts
}

func openSSE(t *testing.T, ts *httptest.Server, ck, query string, hdr map[string]string) *sseClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/events"+query, nil)
	req.Header.Set("Cookie", cookieName+"="+ck)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		cancel()
		t.Fatalf("events: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	c := &sseClient{events: make(chan [2]string, 64), done: make(chan error, 1), cancel: cancel}
	go func() {
		defer resp.Body.Close()
		c.done <- readSSE(ctx, resp.Body, func(ev string, data []byte) error {
			c.events <- [2]string{ev, string(data)}
			return nil
		})
	}()
	t.Cleanup(cancel)
	return c
}

// readSSE is a minimal text/event-stream parser for tests.
func readSSE(ctx context.Context, r io.Reader, fn func(string, []byte) error) error {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	ev := ""
	var data []string
	for {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		for {
			i := strings.IndexByte(string(buf), '\n')
			if i < 0 {
				break
			}
			line := string(buf[:i])
			buf = buf[i+1:]
			switch {
			case line == "":
				if len(data) > 0 {
					_ = fn(ev, []byte(strings.Join(data, "\n")))
				}
				ev, data = "", nil
			case strings.HasPrefix(line, "event: "):
				ev = line[7:]
			case strings.HasPrefix(line, "data: "):
				data = append(data, line[6:])
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

func (c *sseClient) next(t *testing.T) (string, string) {
	t.Helper()
	select {
	case e := <-c.events:
		return e[0], e[1]
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SSE event")
	}
	return "", ""
}

func TestSSE(t *testing.T) {
	e := newEnv(t)
	ts := e.live(t)
	_, pub := genKey(t)
	ck, devID := e.pair(pub)
	start := e.fc.Seq()

	c := openSSE(t, ts, ck, fmt.Sprintf("?after=%d", start), nil)
	e.fc.Emit(context.Background(), api.EventHook, "s_1", "", map[string]string{"kind": "pre_tool"})
	e.openRequest("r_sse0000001")
	ev, data := c.next(t)
	var got api.Event
	if err := json.Unmarshal([]byte(data), &got); err != nil || ev != "event" || got.Type != api.EventRequestOpened || got.Seq != start+2 {
		t.Fatalf("got %s %s (hook events must be filtered)", ev, data)
	}

	// Last-Event-ID wins over ?after (EventSource reconnects).
	c2 := openSSE(t, ts, ck, "?after=0", map[string]string{"Last-Event-ID": fmt.Sprint(start + 1)})
	if _, data := c2.next(t); !strings.Contains(data, `"request.opened"`) {
		t.Fatalf("Last-Event-ID replay: %s", data)
	}

	// Revoking the device ends its streams.
	if w := e.localDo("DELETE", "/v1/devices/"+devID); w.Code != 204 {
		t.Fatal(w.Code)
	}
	for _, cl := range []*sseClient{c, c2} {
		select {
		case <-cl.done:
		case <-time.After(5 * time.Second):
			t.Fatal("stream survived revocation")
		}
	}
}

func TestSSEResync(t *testing.T) {
	e := newEnv(t)
	ts := e.live(t)
	_, pub := genKey(t)
	ck, _ := e.pair(pub)
	for i := 0; i < 5; i++ {
		e.openRequest(fmt.Sprintf("r_resync%04d", i))
	}
	// History before seq 4 was pruned: after=1 must resync.
	e.fc.MinSeq = 4
	c := openSSE(t, ts, ck, "?after=1", nil)
	if ev, data := c.next(t); ev != "control" || !strings.Contains(data, `"resync_required":true`) {
		t.Fatalf("got %s %s", ev, data)
	}
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream not closed after resync")
	}
	e.fc.MinSeq = 0

	// A subscriber that falls behind is dropped and told to resync.
	e.fc.SubBuffer = 1
	c = openSSE(t, ts, ck, fmt.Sprintf("?after=%d", e.fc.Seq()), nil)
	deadline := time.Now().Add(2 * time.Second)
	for e.fc.Subscribers() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	for i := 0; i < 50; i++ {
		e.fc.Emit(context.Background(), api.EventSessionUpdated, "s_x", "", api.Session{ID: "s_x"})
	}
	for {
		ev, data := c.next(t)
		if ev == "control" {
			if !strings.Contains(data, "resync_required") {
				t.Fatal(data)
			}
			break
		}
	}
}

// ---- Action tokens (ntfy) ----

func TestActionTokens(t *testing.T) {
	e := newEnv(t)
	e.openRequest("r_action0001")
	if u := e.srv.ActionURL("r_action0001", api.BehaviorAllow); u != "" {
		t.Fatalf("action URL without public URL: %q", u)
	}
	e.cfg.Web.PublicURL = "https://mac.tail1.ts.net"
	e.srv.pubURL = normalizePublicURL(e.cfg.Web.PublicURL)
	allow := e.srv.ActionURL("r_action0001", api.BehaviorAllow)
	deny := e.srv.ActionURL("r_action0001", api.BehaviorDeny)
	if !strings.HasPrefix(allow, "https://mac.tail1.ts.net/api/actions/") || allow == deny {
		t.Fatalf("allow=%q deny=%q", allow, deny)
	}
	if e.srv.ActionURL("r_missing0001", api.BehaviorAllow) != "" || e.srv.ActionURL("r_action0001", api.BehaviorAsk) != "" {
		t.Fatal("action URL for unknown request or bad behavior")
	}
	// Tokens are stored hashed.
	tok := allow[strings.LastIndex(allow, "/")+1:]
	var n int
	_ = e.db.QueryRow(`SELECT COUNT(*) FROM web_action_tokens WHERE token_hash = ?`, tok).Scan(&n)
	if n != 0 {
		t.Fatal("raw token stored")
	}
	pathOf := func(u string) string { return strings.TrimPrefix(u, "https://mac.tail1.ts.net") }
	host := reqOpt{host: "mac.tail1.ts.net", origin: "-", noCSRF: true}

	if w := e.do("GET", pathOf(allow), nil, host); w.Code == 200 {
		t.Fatal("GET must not spend a token")
	}
	w := e.do("POST", pathOf(allow), nil, host)
	if w.Code != 200 {
		t.Fatalf("action: %d %s", w.Code, w.Body)
	}
	if ds := e.fc.Decisions(); len(ds) != 1 || ds[0].By != "ntfy" || ds[0].Behavior != api.BehaviorAllow {
		t.Fatalf("decisions = %+v", ds)
	}
	if w := e.do("POST", pathOf(allow), nil, host); w.Code != 410 {
		t.Fatalf("reuse: %d", w.Code)
	}
	if w := e.do("POST", pathOf(deny), nil, host); w.Code != 410 {
		t.Fatalf("sibling token after use: %d", w.Code)
	}
	if w := e.do("POST", "/api/actions/short", nil, host); w.Code != 404 {
		t.Fatalf("short token: %d", w.Code)
	}

	// Tokens expire with their request.
	e.openRequest("r_action0002")
	late := e.srv.ActionURL("r_action0002", api.BehaviorDeny)
	e.clock.Add(9*time.Minute + time.Second)
	if w := e.do("POST", pathOf(late), nil, host); w.Code != 410 {
		t.Fatalf("expired token: %d", w.Code)
	}
	// A token for a request answered elsewhere reports 409.
	e.clock.Add(-(9*time.Minute + time.Second))
	e.openRequest("r_action0003")
	u := e.srv.ActionURL("r_action0003", api.BehaviorAllow)
	_, _ = e.fc.Decide(context.Background(), api.Decision{RequestID: "r_action0003", Behavior: api.BehaviorDeny, By: "cli"})
	if w := e.do("POST", pathOf(u), nil, host); w.Code != 409 {
		t.Fatalf("answered elsewhere: %d", w.Code)
	}
}

func TestPublicURLFollowsConfigFile(t *testing.T) {
	e := newEnv(t)
	if e.srv.PublicURL() != "" || e.srv.BaseURL() != testOrigin {
		t.Fatalf("base = %q", e.srv.BaseURL())
	}
	c := config.Default()
	c.Web.PublicURL = "https://box.tail9.ts.net"
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	e.clock.Add(3 * time.Second)
	if got := e.srv.BaseURL(); got != "https://box.tail9.ts.net" {
		t.Fatalf("after config change: %q", got)
	}
	if w := e.do("GET", "/", nil, reqOpt{host: "box.tail9.ts.net"}); w.Code != 200 {
		t.Fatalf("new public host refused: %d", w.Code)
	}
}

// ---- Web Push ----

func TestVAPIDKeysPersisted(t *testing.T) {
	isolate(t)
	dir := filepath.Join(t.TempDir(), "keys")
	k1, err := loadOrCreateVAPID(dir)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "vapid.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("vapid.json mode: %v %v", fi.Mode(), err)
	}
	k2, err := loadOrCreateVAPID(dir)
	if err != nil || k1 != k2 {
		t.Fatal("keys regenerated")
	}
	pub, _ := base64.RawURLEncoding.DecodeString(k1.Public)
	if len(pub) != 65 || pub[0] != 4 {
		t.Fatalf("public key not an uncompressed point: %d", len(pub))
	}
	if err := os.WriteFile(filepath.Join(dir, "vapid.json"), []byte(`{"public_key":"x","private_key":"y"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateVAPID(dir); err == nil {
		t.Fatal("corrupt keys accepted")
	}
}

type pushHit struct {
	path    string
	headers http.Header
	size    int
}

func TestPushDeliveryAndCleanup(t *testing.T) {
	var mu sync.Mutex
	var hits []pushHit
	fake := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		hits = append(hits, pushHit{r.URL.Path, r.Header.Clone(), len(b)})
		mu.Unlock()
		switch r.URL.Path {
		case "/gone":
			w.WriteHeader(http.StatusGone)
		case "/broken":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer fake.Close()
	e := newEnv(t, func(d *Deps) { d.PushClient = fake.Client() })
	_, pub := genKey(t)
	ck, devID := e.pair(pub)

	sub := func(path string) map[string]any {
		k, _ := ecdh.P256().GenerateKey(rand.Reader)
		return map[string]any{
			"endpoint":       fake.URL + path,
			"expirationTime": nil,
			"keys": map[string]string{
				"p256dh": base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()),
				"auth":   base64.RawURLEncoding.EncodeToString(randomBytes(16)),
			},
		}
	}
	for _, p := range []string{"/ok", "/gone"} {
		if w := e.do("POST", "/api/push/subscribe", sub(p), reqOpt{cookie: ck}); w.Code != 204 {
			t.Fatalf("subscribe %s: %d %s", p, w.Code, w.Body)
		}
	}
	httpSub := sub("/x")
	httpSub["endpoint"] = "http://push.example/x"
	if w := e.do("POST", "/api/push/subscribe", httpSub, reqOpt{cookie: ck}); w.Code != 400 {
		t.Fatalf("http endpoint accepted: %d", w.Code)
	}
	var ds []api.Device
	mustJSON(t, e.localDo("GET", "/v1/devices"), &ds)
	if !ds[0].PushOK {
		t.Fatal("device push_ok not set")
	}

	req := e.openRequest("r_push000001")
	n := notifyRequest(req)
	if err := e.srv.Push(context.Background(), n); err != nil {
		t.Fatalf("push: %v", err)
	}
	mu.Lock()
	if len(hits) != 2 {
		t.Fatalf("hits = %d", len(hits))
	}
	for _, h := range hits {
		if !strings.HasPrefix(h.headers.Get("Authorization"), "vapid t=") || h.headers.Get("Content-Encoding") != "aes128gcm" ||
			h.headers.Get("Urgency") != "high" || h.headers.Get("Topic") != "r_push000001" || h.headers.Get("TTL") == "0" || h.size == 0 {
			t.Errorf("push request %s: %v", h.path, h.headers)
		}
	}
	hits = nil
	mu.Unlock()
	var left int
	_ = e.db.QueryRow(`SELECT COUNT(*) FROM web_push_subs`).Scan(&left)
	if left != 1 {
		t.Fatalf("410 subscription not removed: %d left", left)
	}
	if err := e.srv.Push(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(hits) != 1 || hits[0].path != "/ok" {
		t.Fatalf("second push hits = %+v", hits)
	}
	mu.Unlock()

	// Errors are reported; test pushes go to the calling device only.
	if w := e.do("POST", "/api/push/subscribe", sub("/broken"), reqOpt{cookie: ck}); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if err := e.srv.Push(context.Background(), n); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("broken endpoint error = %v", err)
	}
	if w := e.do("POST", "/api/push/unsubscribe", map[string]string{"endpoint": fake.URL + "/broken"}, reqOpt{cookie: ck}); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if w := e.do("POST", "/api/push/test", nil, reqOpt{cookie: ck}); w.Code != 204 {
		t.Fatalf("push test: %d %s", w.Code, w.Body)
	}
	// Revoking a device drops its subscriptions.
	if w := e.localDo("DELETE", "/v1/devices/"+devID); w.Code != 204 {
		t.Fatal(w.Code)
	}
	_ = e.db.QueryRow(`SELECT COUNT(*) FROM web_push_subs`).Scan(&left)
	if left != 0 {
		t.Fatalf("subscriptions survive revocation: %d", left)
	}
}

// ---- Misc endpoints ----

func TestDeviceEndpoints(t *testing.T) {
	e := newEnv(t)
	_, pub := genKey(t)
	ck, devID := e.pair(pub)
	e.fc.PutSession(api.Session{ID: "s_test", Agent: api.AgentClaude, State: api.StateWaiting, Unseen: true})
	e.openRequest("r_misc000001")

	var snap api.Snapshot
	mustJSON(t, e.do("GET", "/api/snapshot", nil, reqOpt{cookie: ck}), &snap)
	if len(snap.Sessions) != 1 || len(snap.Requests) != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if w := e.do("GET", "/api/sessions/s_test", nil, reqOpt{cookie: ck}); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := e.do("GET", "/api/sessions/s_nope", nil, reqOpt{cookie: ck}); w.Code != 404 {
		t.Fatal(w.Code)
	}
	var evs []api.Event
	mustJSON(t, e.do("GET", "/api/sessions/s_test/events?limit=5", nil, reqOpt{cookie: ck}), &evs)
	if len(evs) == 0 {
		t.Fatal("no session events")
	}
	if w := e.do("POST", "/api/sessions/s_test/seen", nil, reqOpt{cookie: ck}); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if s, _ := e.fc.Session(context.Background(), "s_test"); s.Unseen {
		t.Fatal("seen not applied")
	}
	if w := e.do("GET", "/api/requests/r_misc000001", nil, reqOpt{cookie: ck}); w.Code != 200 {
		t.Fatal(w.Code)
	}
	var p api.Presence
	mustJSON(t, e.do("PUT", "/api/presence", api.PresenceBody{Mode: api.PresenceForceAway}, reqOpt{cookie: ck}), &p)
	if p.Mode != api.PresenceForceAway || p.State != api.PresenceAway {
		t.Fatalf("presence = %+v", p)
	}
	if w := e.do("PUT", "/api/presence", api.PresenceBody{Mode: "sleeping"}, reqOpt{cookie: ck}); w.Code != 400 {
		t.Fatal(w.Code)
	}
	var d api.Device
	mustJSON(t, e.do("PATCH", "/api/device", map[string]string{"name": " Work phone "}, reqOpt{cookie: ck}), &d)
	if d.Name != "Work phone" || d.ID != devID {
		t.Fatalf("rename = %+v", d)
	}
	if w := e.do("PATCH", "/api/device", map[string]string{"name": "  "}, reqOpt{cookie: ck}); w.Code != 400 {
		t.Fatal(w.Code)
	}
	var vapid map[string]string
	mustJSON(t, e.do("GET", "/api/vapid", nil, reqOpt{cookie: ck}), &vapid)
	if vapid["public_key"] != e.srv.vapid.Public {
		t.Fatal("vapid key")
	}
	w := e.do("POST", "/api/logout", nil, reqOpt{cookie: ck})
	if w.Code != 204 || !strings.Contains(w.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("logout: %d %q", w.Code, w.Header().Get("Set-Cookie"))
	}
	if w := e.do("GET", "/api/me", nil, reqOpt{cookie: ck}); w.Code != 401 {
		t.Fatal("logged-out cookie still valid")
	}
}

func TestCodesAndNames(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		c := newPairCode()
		if len(c) != 14 || c[4] != '-' || c[9] != '-' || strings.ContainsAny(c, "ILOU") || seen[c] {
			t.Fatalf("bad code %q", c)
		}
		seen[c] = true
		if id := newDeviceID(); len(id) != 12 || !strings.HasPrefix(id, "d_") || strings.ToLower(id) != id {
			t.Fatalf("bad id %q", id)
		}
	}
	if normalizeCode("abcd-efgh o1l") != "ABCDEFGH011" {
		t.Fatal(normalizeCode("abcd-efgh o1l"))
	}
	if got := cleanName(strings.Repeat("é", 100)); len([]rune(got)) != maxNameLen {
		t.Fatalf("name not trimmed: %d", len([]rune(got)))
	}
}

func notifyRequest(r api.Request) core.Notification {
	return core.Notification{Kind: core.NotifyRequest, Title: "Claude Code · pilothouse", Body: "Wants to run `git push origin main`", Request: &r, Remote: true}
}

// W5: the stream carries a real `event: ping` (with data, so EventSource
// dispatches it and the app's watchdog sees it), keeping the `: ping`
// comment for older readers.
func TestSSEPingEvent(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.PingEvery = 50 * time.Millisecond })
	ts := e.live(t)
	_, pub := genKey(t)
	ck, _ := e.pair(pub)
	c := openSSE(t, ts, ck, fmt.Sprintf("?after=%d", e.fc.Seq()), nil)
	if ev, data := c.next(t); ev != "ping" || data != "{}" {
		t.Fatalf("got %q %q, want ping {}", ev, data)
	}

	// Raw framing: comment line plus event in one block.
	req, _ := http.NewRequest("GET", ts.URL+"/api/events", nil)
	req.Header.Set("Cookie", cookieName+"="+ck)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw strings.Builder
	buf := make([]byte, 512)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(raw.String(), "data: {}\n\n") && time.Now().Before(deadline) {
		n, err := resp.Body.Read(buf)
		raw.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if !strings.Contains(raw.String(), ": ping\nevent: ping\ndata: {}\n\n") {
		t.Fatalf("raw stream %q lacks the ping block", raw.String())
	}
}

// W4: /api/me reports the same machine name as the daemon (config
// `machine`, then $PILOTHOUSE_MACHINE), not the raw host name.
func TestMeMachineName(t *testing.T) {
	t.Setenv("PILOTHOUSE_MACHINE", "env-box")
	e := newEnv(t)
	_, pub := genKey(t)
	ck, _ := e.pair(pub)
	machine := func() string {
		w := e.do("GET", "/api/me", nil, reqOpt{cookie: ck})
		var m struct {
			Machine string `json:"machine"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &m) != nil {
			t.Fatalf("me: %d %s", w.Code, w.Body)
		}
		return m.Machine
	}
	if got := machine(); got != "env-box" {
		t.Fatalf("machine = %q, want env-box ($PILOTHOUSE_MACHINE)", got)
	}
	e.cfg.Machine = "cfg-box"
	if got := machine(); got != "cfg-box" {
		t.Fatalf("machine = %q, want cfg-box (config wins)", got)
	}
}

// W1: the native replaceChildren/append render null/undefined children as
// the text "null"; the app must go through dom.js fill()/add(), which skip
// them. Only dom.js itself may call the native methods.
func TestStaticJSUsesNullSafeDOMHelpers(t *testing.T) {
	var bad []string
	err := fs.WalkDir(staticFS, "static/js", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".js") || p == "static/js/dom.js" {
			return err
		}
		b, err := staticFS.ReadFile(p)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, ".replaceChildren(") || strings.Contains(line, ".append(") {
				bad = append(bad, fmt.Sprintf("%s:%d: %s", p, i+1, strings.TrimSpace(line)))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Fatalf("use fill()/add() from dom.js instead of native DOM insertion:\n%s", strings.Join(bad, "\n"))
	}
}
