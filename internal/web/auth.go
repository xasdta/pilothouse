package web

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/config"
	"github.com/xasdta/pilothouse/internal/version"
)

const (
	cookieName   = "ph_dev"
	pairTTL      = 10 * time.Minute
	loginTTL     = 5 * time.Minute
	maxNameLen   = 60
	cookieMaxAge = 400 * 24 * time.Hour // browsers cap cookie lifetime at 400 days
)

// auth requires a valid device cookie and puts the device in the context.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d := s.authenticate(r)
		if d == nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "this device is not paired (run `pilothouse pair`)")
			return
		}
		s.touch(r.Context(), d.ID)
		next(w, r.WithContext(context.WithValue(r.Context(), ctxDevice, d)))
	}
}

func (s *Server) authenticate(r *http.Request) *deviceRow {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return nil
	}
	id, secret, ok := strings.Cut(c.Value, ".")
	if !ok || !strings.HasPrefix(id, "d_") || len(id) > 32 || secret == "" {
		return nil
	}
	d, err := s.device(r.Context(), id)
	if err != nil {
		if !errors.Is(err, errNoDevice) && r.Context().Err() == nil {
			s.log.Error("load device", "err", err)
		}
		return nil
	}
	if d.RevokedAt != nil || subtle.ConstantTimeCompare(hashSecret(secret), d.tokenHash) != 1 {
		return nil
	}
	return d
}

// touch updates last_seen_at at most once a minute per device.
func (s *Server) touch(ctx context.Context, id string) {
	now := s.now()
	s.mu.Lock()
	if now.Sub(s.lastSeen[id]) < time.Minute {
		s.mu.Unlock()
		return
	}
	s.lastSeen[id] = now
	s.mu.Unlock()
	if _, err := s.d.DB.ExecContext(ctx, `UPDATE web_devices SET last_seen_at = ? WHERE id = ?`, ms(now), id); err != nil {
		s.log.Warn("update last_seen", "err", err)
	}
}

// parsePublicKey accepts base64url (or std base64) SPKI of a P-256 key, as
// WebCrypto exportKey("spki") produces.
func parsePublicKey(b64 string) (*ecdsa.PublicKey, error) {
	der, err := decodeB64(b64)
	if err != nil {
		return nil, errors.New("public_key is not base64")
	}
	k, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, errors.New("public_key is not an SPKI public key")
	}
	ec, ok := k.(*ecdsa.PublicKey)
	if !ok || ec.Curve != elliptic.P256() {
		return nil, errors.New("public_key must be ECDSA P-256")
	}
	return ec, nil
}

func decodeB64(s string) ([]byte, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	if strings.ContainsAny(s, "+/") {
		return base64.RawStdEncoding.DecodeString(s)
	}
	return base64.RawURLEncoding.DecodeString(s)
}

// cleanName trims a device name and drops control characters.
func cleanName(n string) string {
	n = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(n))
	for utf8.RuneCountInString(n) > maxNameLen {
		_, size := utf8.DecodeLastRuneInString(n)
		n = n[:len(n)-size]
	}
	return strings.TrimSpace(n)
}

// clientKey identifies the caller for rate limiting. Tailscale Serve puts
// the tailnet peer address in X-Forwarded-For; only loopback peers (that
// is, the proxy) are trusted to set it.
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			return strings.TrimSpace(first)
		}
	}
	return host
}

type pairBody struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
}

type meResponse struct {
	Device    api.Device `json:"device"`
	Machine   string     `json:"machine"`
	Version   string     `json:"version"`
	VAPIDKey  string     `json:"vapid_public_key"`
	BaseURL   string     `json:"base_url"`
	PublicURL string     `json:"public_url,omitempty"`
}

// handlePair redeems a pairing or login code: it registers the device's
// public key and sets the session cookie.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	if ok, wait := s.pairLimit.allow(clientKey(r)); !ok {
		w.Header().Set("Retry-After", itoa(int(wait.Seconds())+1))
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "too many pairing attempts; wait a minute and try again")
		return
	}
	var b pairBody
	if !decodeBody(w, r, &b) {
		return
	}
	if _, err := parsePublicKey(b.PublicKey); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	name := cleanName(b.Name)
	if name == "" {
		name = "Phone"
	}
	if normalizeCode(b.Code) == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "code is required")
		return
	}
	_, ok, err := s.consumeCode(r.Context(), b.Code)
	if err != nil {
		s.log.Error("consume pair code", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if !ok {
		s.pairLimit.fail(clientKey(r))
		writeErr(w, http.StatusUnauthorized, "invalid_code", "this pairing code is wrong, used or expired — run `pilothouse pair` again")
		return
	}
	id, secret := newDeviceID(), randomToken(32)
	now := s.now()
	if _, err := s.d.DB.ExecContext(r.Context(),
		`INSERT INTO web_devices (id, name, public_key, token_hash, created_at, last_seen_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, name, strings.TrimSpace(b.PublicKey), hashSecret(secret), ms(now), ms(now)); err != nil {
		s.log.Error("insert device", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	d, err := s.device(r.Context(), id)
	if err != nil {
		s.log.Error("load device", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.log.Info("device paired", "device", id, "name", name)
	s.emitDevice(r.Context(), d.Device)
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    id + "." + secret,
		Path:     "/",
		MaxAge:   int(cookieMaxAge.Seconds()),
		HttpOnly: true,
		Secure:   originOf(r).https,
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, s.me(d))
	// Pairing again from a signed-in browser (its key was lost) replaces
	// the old device. Revoke it only after the new cookie has gone out, so
	// the app's live stream reconnects as the new device.
	if old := s.authenticate(r); old != nil {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if err := s.revoke(context.WithoutCancel(r.Context()), old.ID); err != nil {
			s.log.Warn("revoke replaced device", "device", old.ID, "err", err)
		} else {
			s.log.Info("device replaced", "old", old.ID, "new", id)
		}
	}
}

func (s *Server) me(d *deviceRow) meResponse {
	return meResponse{
		Device:    d.Device,
		Machine:   s.machineName(),
		Version:   version.Version,
		VAPIDKey:  s.vapid.Public,
		BaseURL:   s.BaseURL(),
		PublicURL: s.PublicURL(),
	}
}

// machineName is the name the daemon reports everywhere else: config
// `machine`, then $PILOTHOUSE_MACHINE, then the host name.
func (s *Server) machineName() string { return config.MachineName(s.d.Config) }

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.me(deviceOf(r)))
}

func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	name := cleanName(b.Name)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "name is required")
		return
	}
	d := deviceOf(r)
	if _, err := s.d.DB.ExecContext(r.Context(), `UPDATE web_devices SET name = ? WHERE id = ?`, name, d.ID); err != nil {
		s.writeCoreErr(w, err)
		return
	}
	d, err := s.device(r.Context(), d.ID)
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	s.emitDevice(r.Context(), d.Device)
	writeJSON(w, http.StatusOK, d.Device)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.revoke(r.Context(), deviceOf(r).ID); err != nil {
		s.writeCoreErr(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: originOf(r).https, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

// revoke disables a device, drops its push subscriptions and ends its
// live event streams.
func (s *Server) revoke(ctx context.Context, id string) error {
	res, err := s.d.DB.ExecContext(ctx, `UPDATE web_devices SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, ms(s.now()), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNoDevice
	}
	if _, err := s.d.DB.ExecContext(ctx, `DELETE FROM web_push_subs WHERE device_id = ?`, id); err != nil {
		return err
	}
	if _, err := s.d.DB.ExecContext(ctx, `DELETE FROM web_passkeys WHERE device_id = ?`, id); err != nil {
		return err
	}
	s.mu.Lock()
	for _, cancel := range s.streams[id] {
		cancel()
	}
	delete(s.streams, id)
	delete(s.lastSeen, id)
	s.mu.Unlock()
	if d, err := s.device(ctx, id); err == nil {
		s.emitDevice(ctx, d.Device)
	}
	s.log.Info("device revoked", "device", id)
	return nil
}

func (s *Server) emitDevice(ctx context.Context, d api.Device) {
	if _, err := s.d.Core.Emit(ctx, api.EventDeviceChanged, "", "", d); err != nil {
		s.log.Warn("emit device.changed", "err", err)
	}
}

// ---- Local-admin routes (Unix socket) ----

// RegisterLocal adds local-admin routes to the Unix-socket mux:
// POST /v1/pair, POST /v1/login-link, GET /v1/devices, DELETE /v1/devices/{id}.
// Each is wrapped with Deps.LocalGuard (the daemon's agent-caller guard).
func (s *Server) RegisterLocal(mux *http.ServeMux) {
	for pattern, h := range s.LocalRoutes() {
		mux.Handle(pattern, s.guard(h))
	}
}

// LocalRoutes returns the local-admin handlers keyed by ServeMux pattern,
// unguarded, for daemons that apply their own middleware per route.
func (s *Server) LocalRoutes() map[string]http.Handler {
	return map[string]http.Handler{
		"POST /v1/pair":           http.HandlerFunc(s.handleLocalPair),
		"POST /v1/login-link":     http.HandlerFunc(s.handleLocalLogin),
		"GET /v1/devices":         http.HandlerFunc(s.handleLocalDevices),
		"DELETE /v1/devices/{id}": http.HandlerFunc(s.handleLocalRevoke),
	}
}

func (s *Server) guard(h http.Handler) http.Handler {
	if s.d.LocalGuard != nil {
		return s.d.LocalGuard(h)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.log.Error("web: local route called without an agent-caller guard (Deps.LocalGuard is nil)", "path", r.URL.Path)
		writeErr(w, http.StatusInternalServerError, "internal", "agent-caller guard not configured")
	})
}

func (s *Server) handleLocalPair(w http.ResponseWriter, r *http.Request) {
	code, exp, err := s.createCode(r.Context(), "pair", pairTTL)
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.PairStart{
		Code:      code,
		URL:       s.BaseURL() + "/pair#" + code,
		ExpiresAt: exp.UTC(),
		Remote:    s.PublicURL() != "",
	})
}

// handleLocalLogin issues a login link for a browser on this machine, so
// it always uses the loopback URL.
func (s *Server) handleLocalLogin(w http.ResponseWriter, r *http.Request) {
	code, exp, err := s.createCode(r.Context(), "login", loginTTL)
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.LoginLink{URL: s.localURL() + "/login#" + code, ExpiresAt: exp.UTC()})
}

func (s *Server) handleLocalDevices(w http.ResponseWriter, r *http.Request) {
	ds, err := s.listDevices(r.Context())
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ds)
}

func (s *Server) handleLocalRevoke(w http.ResponseWriter, r *http.Request) {
	err := s.revoke(r.Context(), r.PathValue("id"))
	if errors.Is(err, errNoDevice) {
		writeErr(w, http.StatusNotFound, "not_found", "no paired device with that id (see `pilothouse devices`)")
		return
	}
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
