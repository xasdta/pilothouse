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
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/notify"
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
//
// Every request notification uses the tag "req-<id>". When the request
// closes, Withdraw sends one more push with that tag and type "resolved"
// (status, by, and ready-made title/body) so the service worker replaces
// the notification with a short "Answered ✓" one. It is never a silent
// push: iOS revokes subscriptions that receive pushes without showing a
// notification.
type pushPayload struct {
	// Type is the notification kind (request, blocked, waiting, test) or
	// "resolved" for a withdrawal. Kind repeats it for 0.1 service workers.
	Type      string       `json:"type"`
	Kind      string       `json:"kind"`
	Title     string       `json:"title"`
	Body      string       `json:"body"`
	Tag       string       `json:"tag"`
	URL       string       `json:"url"` // same-origin path to open on click
	RequestID string       `json:"request_id,omitempty"`
	SessionID string       `json:"session_id,omitempty"`
	Request   *pushRequest `json:"request,omitempty"` // request pushes: what notification actions need
	Actions   bool         `json:"actions,omitempty"` // offer signed Allow/Deny buttons
	ExpiresAt int64        `json:"expires_at,omitempty"`
	// Resolved pushes only.
	Status string `json:"status,omitempty"` // the request's status after it closed
	By     string `json:"by,omitempty"`     // who answered, for people: "from iPhone", "from the CLI"
}

// pushRequest is the request summary carried by request pushes.
type pushRequest struct {
	ID         string `json:"id"`
	Checkpoint string `json:"checkpoint"`
	Summary    string `json:"summary"`
}

// pushTypeResolved is the payload type of a withdrawal.
const pushTypeResolved = "resolved"

// PushTag is the notification tag of a request's pushes.
func PushTag(requestID string) string { return "req-" + requestID }

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

// pushTopic is the Topic header for a tag: a push service keeps only the
// newest undelivered message per topic, so a withdrawal that overtakes its
// request replaces it before the phone ever sees it.
func pushTopic(tag string) string {
	topic := topicRE.ReplaceAllString(tag, "")
	if len(topic) > 32 {
		topic = topic[:32]
	}
	return topic
}

// payloadFor builds the push for a notification, with its TTL (seconds)
// and urgency.
func payloadFor(n core.Notification, now time.Time) (pushPayload, int, webpush.Urgency) {
	p := pushPayload{Type: string(n.Kind), Kind: string(n.Kind), Title: truncate(n.Title, 120), Body: truncate(n.Body, 400), Tag: "pilothouse-" + string(n.Kind), URL: "/"}
	ttl, urgency := 3600, webpush.UrgencyNormal
	switch {
	case n.Request != nil:
		r := n.Request
		p.RequestID, p.SessionID = r.ID, r.SessionID
		p.Tag, p.URL = PushTag(r.ID), "/r/"+r.ID
		p.Request = &pushRequest{ID: r.ID, Checkpoint: string(r.Checkpoint), Summary: truncate(r.Summary, 200)}
		// One-tap Allow/Deny only for permissions: a bare allow can't
		// answer a question, and a plan should be read first.
		p.Actions = n.Kind == core.NotifyRequest && r.Status == api.RequestOpen &&
			(r.Checkpoint == api.CheckpointPermission || r.Checkpoint == "")
		if !r.ExpiresAt.IsZero() {
			p.ExpiresAt = r.ExpiresAt.UnixMilli()
			ttl = max(int(r.ExpiresAt.Sub(now).Seconds()), 30)
		}
		urgency = webpush.UrgencyHigh
	case n.Session != nil:
		p.SessionID, p.Tag, p.URL = n.Session.ID, n.Session.ID, "/s/"+n.Session.ID
	}
	if len(n.URL) > 1 && n.URL[0] == '/' && n.URL[1] != '/' { // the sender's deep link (a task: /t/<id>)
		p.URL = n.URL
	}
	return p, ttl, urgency
}

// resolvedPayload is the push that replaces a closed request's notification.
func resolvedPayload(r api.Request, deviceName func(string) string) pushPayload {
	title := notify.Mark(r) + " " + notify.Outcome(r)
	body := notify.Short(r.Summary, 120)
	if body == "" {
		body = notify.AgentName(r.Agent) + " " + notify.Action(&r)
	}
	by := notify.DecidedBy(r, deviceName)
	if by != "" {
		body += " — " + by
	}
	return pushPayload{Type: pushTypeResolved, Kind: pushTypeResolved, Title: truncate(title, 120), Body: truncate(body, 400),
		Tag: PushTag(r.ID), URL: "/r/" + r.ID, RequestID: r.ID, SessionID: r.SessionID, Status: string(r.Status), By: by}
}

// pushTarget is one subscription and the device it belongs to.
type pushTarget struct {
	device string
	sub    webpush.Subscription
}

// targets lists the subscriptions of paired, unrevoked devices; only
// limits it to those device ids when non-nil.
func (s *Server) targets(ctx context.Context, only map[string]bool) ([]pushTarget, error) {
	rows, err := s.d.DB.QueryContext(ctx, `SELECT d.id, p.endpoint, p.p256dh, p.auth FROM web_push_subs p JOIN web_devices d ON d.id = p.device_id WHERE d.revoked_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pushTarget
	for rows.Next() {
		var t pushTarget
		if err := rows.Scan(&t.device, &t.sub.Endpoint, &t.sub.Keys.P256dh, &t.sub.Keys.Auth); err != nil {
			return nil, err
		}
		if only == nil || only[t.device] {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}

func (s *Server) push(ctx context.Context, n core.Notification, onlyDevice string) (int, error) {
	p, ttl, urgency := payloadFor(n, s.now())
	var only map[string]bool
	if onlyDevice != "" {
		only = map[string]bool{onlyDevice: true}
	}
	ts, err := s.targets(ctx, only)
	if err != nil {
		return 0, err
	}
	reached, err := s.deliver(ctx, p, ts, ttl, urgency)
	if onlyDevice == "" && n.Request != nil && (n.Kind == core.NotifyRequest || n.Kind == core.NotifyBlocked) && len(reached) > 0 {
		s.rememberPush(n.Request.ID, reached, s.now().Add(time.Duration(ttl)*time.Second))
	}
	return len(reached), err
}

// Withdraw replaces a closed request's notification on every device that
// got it (notify.Withdrawer). The device that answered is skipped — it
// already knows, and a new notification there would only buzz again. A
// timed-out request is left alone: its "waiting in the terminal" push
// already replaced it.
func (s *Server) Withdraw(ctx context.Context, r api.Request) error {
	devs, until := s.forgetPush(r.ID)
	if len(devs) == 0 || r.Status == api.RequestOpen || r.Status == api.RequestExpired {
		return nil
	}
	if r.Decision != nil {
		if id, ok := strings.CutPrefix(r.Decision.By, "device:"); ok {
			delete(devs, id)
		}
	}
	if len(devs) == 0 {
		return nil
	}
	ts, err := s.targets(ctx, devs)
	if err != nil {
		return err
	}
	// Live as long as the request's own push could still arrive, plus a
	// margin, so a phone that was offline still gets its stale buttons
	// replaced.
	ttl := min(max(int(until.Sub(s.now()).Seconds())+600, 60), 3600)
	p := resolvedPayload(r, func(id string) string { return s.DeviceName(ctx, id) })
	_, err = s.deliver(ctx, p, ts, ttl, webpush.UrgencyNormal)
	return err
}

// DeviceName is a paired device's name, or "" when unknown.
func (s *Server) DeviceName(ctx context.Context, id string) string {
	var name string
	if err := s.d.DB.QueryRowContext(ctx, `SELECT name FROM web_devices WHERE id = ?`, id).Scan(&name); err != nil {
		return ""
	}
	return name
}

// maxPushed bounds the requests remembered for withdrawal.
const maxPushed = 2048

// pushedRequest is which devices a request's push reached, and until when
// the push service may still deliver it.
type pushedRequest struct {
	devices map[string]bool
	until   time.Time
}

func (s *Server) rememberPush(id string, devices map[string]bool, until time.Time) {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	if s.pushed == nil {
		s.pushed = map[string]pushedRequest{}
	}
	now := s.now()
	if len(s.pushed) >= maxPushed {
		for k, v := range s.pushed {
			if now.Sub(v.until) > time.Hour || len(s.pushed) >= maxPushed {
				delete(s.pushed, k)
			}
		}
	}
	prev := s.pushed[id]
	if prev.devices == nil {
		prev.devices = map[string]bool{}
	}
	for d := range devices {
		prev.devices[d] = true
	}
	if until.After(prev.until) {
		prev.until = until
	}
	s.pushed[id] = prev
}

func (s *Server) forgetPush(id string) (map[string]bool, time.Time) {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	p, ok := s.pushed[id]
	if !ok {
		return nil, time.Time{}
	}
	delete(s.pushed, id)
	return p.devices, p.until
}

// deliver sends p to every target and returns the devices it reached.
func (s *Server) deliver(ctx context.Context, p pushPayload, ts []pushTarget, ttl int, urgency webpush.Urgency) (map[string]bool, error) {
	msg, _ := json.Marshal(p)
	topic := pushTopic(p.Tag)
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		errs    []error
		reached = map[string]bool{}
		sem     = make(chan struct{}, 8)
	)
	for i := range ts {
		t := &ts[i]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			// webpush-go appends padding to the message in place: a full
			// slice expression makes each send copy instead of sharing.
			err := s.sendOne(ctx, msg[:len(msg):len(msg)], &t.sub, &webpush.Options{
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
				reached[t.device] = true
			}
		}()
	}
	wg.Wait()
	return reached, errors.Join(errs...)
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
