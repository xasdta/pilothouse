package web

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
	"unicode/utf8"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
)

// VAPIDSubscriber identifies this application server to push services
// (the JWT "sub" claim).
const VAPIDSubscriber = "https://pilothouse.dev"

// vapidKeys are this daemon's Web Push application-server keys:
// base64url, uncompressed P-256 point (public) and 32-byte scalar (private).
type vapidKeys struct {
	Public  string `json:"public_key"`
	Private string `json:"private_key"`
}

// loadOrCreateVAPID reads <dir>/vapid.json, generating it (0600) on first
// use. Regenerating would invalidate every push subscription, so a
// corrupt file is an error rather than silently replaced.
func loadOrCreateVAPID(dir string) (vapidKeys, error) {
	p := filepath.Join(dir, "vapid.json")
	b, err := os.ReadFile(p)
	if err == nil {
		var k vapidKeys
		if err := json.Unmarshal(b, &k); err != nil {
			return vapidKeys{}, fmt.Errorf("web: %s: %w", p, err)
		}
		if err := k.validate(); err != nil {
			return vapidKeys{}, fmt.Errorf("web: %s: %w (delete it to generate new keys; paired devices must re-enable notifications)", p, err)
		}
		return k, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return vapidKeys{}, fmt.Errorf("web: read VAPID keys: %w", err)
	}
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return vapidKeys{}, err
	}
	k := vapidKeys{
		Public:  base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()),
		Private: base64.RawURLEncoding.EncodeToString(priv.Bytes()),
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return vapidKeys{}, err
	}
	out, _ := json.MarshalIndent(k, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return vapidKeys{}, err
	}
	if err := os.Rename(tmp, p); err != nil {
		return vapidKeys{}, err
	}
	return k, nil
}

func (k vapidKeys) validate() error {
	priv, err := base64.RawURLEncoding.DecodeString(k.Private)
	if err != nil {
		return errors.New("private_key is not base64url")
	}
	key, err := ecdh.P256().NewPrivateKey(priv)
	if err != nil {
		return errors.New("private_key is not a P-256 key")
	}
	if base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()) != k.Public {
		return errors.New("public_key does not match private_key")
	}
	return nil
}

func (s *Server) handleVAPID(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"public_key": s.vapid.Public})
}

// pushSubscription is PushSubscription.toJSON().
type pushSubscription struct {
	Endpoint       string   `json:"endpoint"`
	ExpirationTime *float64 `json:"expirationTime,omitempty"`
	Keys           struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

func (s *Server) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	var b pushSubscription
	if !decodeBody(w, r, &b) {
		return
	}
	u, err := url.Parse(b.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || len(b.Endpoint) > 2048 {
		writeErr(w, http.StatusBadRequest, "bad_request", "endpoint must be an https URL")
		return
	}
	if p, err := decodeB64(b.Keys.P256dh); err != nil || len(p) != 65 {
		writeErr(w, http.StatusBadRequest, "bad_request", "keys.p256dh must be an uncompressed P-256 point")
		return
	}
	if a, err := decodeB64(b.Keys.Auth); err != nil || len(a) < 16 {
		writeErr(w, http.StatusBadRequest, "bad_request", "keys.auth must be 16 bytes")
		return
	}
	d := deviceOf(r)
	if _, err := s.d.DB.ExecContext(r.Context(), `
INSERT INTO web_push_subs (endpoint, device_id, p256dh, auth, created_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(endpoint) DO UPDATE SET device_id = excluded.device_id, p256dh = excluded.p256dh, auth = excluded.auth`,
		b.Endpoint, d.ID, b.Keys.P256dh, b.Keys.Auth, ms(s.now())); err != nil {
		s.writeCoreErr(w, err)
		return
	}
	s.deviceChanged(r.Context(), d.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Endpoint string `json:"endpoint"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	d := deviceOf(r)
	if _, err := s.d.DB.ExecContext(r.Context(), `DELETE FROM web_push_subs WHERE endpoint = ? AND device_id = ?`, b.Endpoint, d.ID); err != nil {
		s.writeCoreErr(w, err)
		return
	}
	s.deviceChanged(r.Context(), d.ID)
	w.WriteHeader(http.StatusNoContent)
}

// handlePushTest sends a test notification to the calling device only.
func (s *Server) handlePushTest(w http.ResponseWriter, r *http.Request) {
	d := deviceOf(r)
	n := core.Notification{Kind: core.NotifyTest, Title: "Pilothouse is connected", Body: "Notifications from " + s.machineName() + " will appear here.", Remote: true}
	sent, err := s.push(r.Context(), n, d.ID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "push_failed", err.Error())
		return
	}
	if sent == 0 {
		writeErr(w, http.StatusConflict, "no_subscription", "notifications are not enabled on this device")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deviceChanged(ctx context.Context, id string) {
	if d, err := s.device(ctx, id); err == nil {
		s.emitDevice(ctx, d.Device)
	}
}

// pushPayload is what the service worker receives (sw.js).
type pushPayload struct {
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Tag       string `json:"tag"`
	URL       string `json:"url"` // same-origin path to open on click
	RequestID string `json:"request_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Actions   bool   `json:"actions,omitempty"` // offer signed Allow/Deny buttons
	ExpiresAt int64  `json:"expires_at,omitempty"`
}

// Push sends a Web Push notification to every paired device with a subscription.
func (s *Server) Push(ctx context.Context, n core.Notification) error {
	_, err := s.push(ctx, n, "")
	return err
}

var topicRE = regexp.MustCompile(`[^A-Za-z0-9_-]`)

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func (s *Server) push(ctx context.Context, n core.Notification, onlyDevice string) (int, error) {
	p := pushPayload{Kind: string(n.Kind), Title: truncate(n.Title, 120), Body: truncate(n.Body, 400), Tag: "pilothouse-" + string(n.Kind), URL: "/"}
	ttl, urgency := 3600, webpush.UrgencyNormal
	switch {
	case n.Request != nil:
		p.RequestID, p.SessionID = n.Request.ID, n.Request.SessionID
		p.Tag, p.URL = n.Request.ID, "/r/"+n.Request.ID
		p.Actions = n.Kind == core.NotifyRequest && n.Request.Status == api.RequestOpen
		if !n.Request.ExpiresAt.IsZero() {
			p.ExpiresAt = n.Request.ExpiresAt.UnixMilli()
			ttl = max(int(time.Until(n.Request.ExpiresAt).Seconds()), 30)
		}
		urgency = webpush.UrgencyHigh
	case n.Session != nil:
		p.SessionID, p.Tag, p.URL = n.Session.ID, n.Session.ID, "/s/"+n.Session.ID
	}
	msg, _ := json.Marshal(p)
	topic := topicRE.ReplaceAllString(p.Tag, "")
	if len(topic) > 32 {
		topic = topic[:32]
	}

	q := `SELECT p.endpoint, p.p256dh, p.auth FROM web_push_subs p JOIN web_devices d ON d.id = p.device_id WHERE d.revoked_at IS NULL`
	args := []any{}
	if onlyDevice != "" {
		q += ` AND d.id = ?`
		args = append(args, onlyDevice)
	}
	rows, err := s.d.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	var subs []webpush.Subscription
	for rows.Next() {
		var sub webpush.Subscription
		if err := rows.Scan(&sub.Endpoint, &sub.Keys.P256dh, &sub.Keys.Auth); err != nil {
			rows.Close()
			return 0, err
		}
		subs = append(subs, sub)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
		sent int
		sem  = make(chan struct{}, 8)
	)
	for i := range subs {
		sub := &subs[i]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			// webpush-go appends padding to the message in place: a full
			// slice expression makes each send copy instead of sharing.
			err := s.sendOne(ctx, msg[:len(msg):len(msg)], sub, &webpush.Options{
				HTTPClient:      s.d.PushClient,
				Subscriber:      VAPIDSubscriber,
				VAPIDPublicKey:  s.vapid.Public,
				VAPIDPrivateKey: s.vapid.Private,
				TTL:             ttl,
				Urgency:         urgency,
				Topic:           topic,
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, errGone):
			case err != nil:
				errs = append(errs, err)
			default:
				sent++
			}
		}()
	}
	wg.Wait()
	return sent, errors.Join(errs...)
}

// errGone is returned for subscriptions the push service no longer knows.
var errGone = errors.New("push subscription expired")

func (s *Server) sendOne(ctx context.Context, msg []byte, sub *webpush.Subscription, o *webpush.Options) error {
	resp, err := webpush.SendNotificationWithContext(ctx, msg, sub, o)
	if err != nil {
		return fmt.Errorf("web push to %s: %w", hostOf(sub.Endpoint), err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		// The browser unsubscribed or the subscription expired: forget it.
		if _, err := s.d.DB.ExecContext(context.WithoutCancel(ctx), `DELETE FROM web_push_subs WHERE endpoint = ?`, sub.Endpoint); err != nil {
			s.log.Warn("drop push subscription", "err", err)
		}
		s.log.Info("dropped expired push subscription", "push_service", hostOf(sub.Endpoint))
		return errGone
	case resp.StatusCode/100 != 2:
		return fmt.Errorf("web push to %s: %s", hostOf(sub.Endpoint), resp.Status)
	}
	return nil
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return "push service"
}
