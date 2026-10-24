package web

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/notify"
)

var _ notify.Withdrawer = (*Server)(nil)

// fakePushService records which endpoint got each push, with its headers.
type fakePushService struct {
	*httptest.Server
	mu   sync.Mutex
	hits []pushHit
}

func newFakePushService(t *testing.T) *fakePushService {
	f := &fakePushService{}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.hits = append(f.hits, pushHit{r.URL.Path, r.Header.Clone(), len(b)})
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(f.Close)
	return f
}

// take returns the endpoints hit since the last call, sorted, and the hits.
func (f *fakePushService) take() ([]string, []pushHit) {
	f.mu.Lock()
	defer f.mu.Unlock()
	hits := f.hits
	f.hits = nil
	var paths []string
	for _, h := range hits {
		paths = append(paths, h.path)
	}
	sort.Strings(paths)
	return paths, hits
}

func (e *env) subscribe(t *testing.T, ck, endpoint string) {
	t.Helper()
	k, _ := ecdh.P256().GenerateKey(rand.Reader)
	body := map[string]any{
		"endpoint": endpoint,
		"keys": map[string]string{
			"p256dh": base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()),
			"auth":   base64.RawURLEncoding.EncodeToString(randomBytes(16)),
		},
	}
	if w := e.do("POST", "/api/push/subscribe", body, reqOpt{cookie: ck}); w.Code != 204 {
		t.Fatalf("subscribe %s: %d %s", endpoint, w.Code, w.Body)
	}
}

func closed(r api.Request, st api.RequestStatus, d *api.Decision) api.Request {
	r.Status = st
	r.Decision = d
	now := time.Now()
	r.ClosedAt = &now
	return r
}

// When a request closes, the devices its push reached get one more push
// with the same tag/topic, except the device that answered; timeouts and
// requests that were never pushed send nothing.
func TestPushWithdrawal(t *testing.T) {
	fake := newFakePushService(t)
	e := newEnv(t, func(d *Deps) { d.PushClient = fake.Client() })
	_, pubA := genKey(t)
	ckA, devA := e.pair(pubA)
	_, pubB := genKey(t)
	ckB, _ := e.pair(pubB)
	e.do("PATCH", "/api/device", map[string]string{"name": "iPhone"}, reqOpt{cookie: ckA})
	e.subscribe(t, ckA, fake.URL+"/a")
	e.subscribe(t, ckB, fake.URL+"/b")
	ctx := context.Background()

	r1 := e.openRequest("r_wdraw00001")
	if err := e.srv.Push(ctx, notifyRequest(r1)); err != nil {
		t.Fatal(err)
	}
	if got, _ := fake.take(); strings.Join(got, ",") != "/a,/b" {
		t.Fatalf("request push reached %v", got)
	}

	// Answered on device A: only B is told.
	byA := closed(r1, api.RequestDecided, &api.Decision{Behavior: api.BehaviorAllow, By: "device:" + devA})
	if err := e.srv.Withdraw(ctx, byA); err != nil {
		t.Fatal(err)
	}
	got, hits := fake.take()
	if strings.Join(got, ",") != "/b" {
		t.Fatalf("withdrawal reached %v, want only the other device", got)
	}
	if h := hits[0].headers; h.Get("Topic") != "req-r_wdraw00001" || h.Get("Urgency") != "normal" || h.Get("TTL") == "0" {
		t.Fatalf("withdrawal headers = %v", h)
	}
	// Once only.
	if err := e.srv.Withdraw(ctx, byA); err != nil {
		t.Fatal(err)
	}
	if got, _ := fake.take(); len(got) != 0 {
		t.Fatalf("second withdrawal sent %v", got)
	}

	// Never pushed: nothing to take back (no push for requests that closed at the desk).
	if err := e.srv.Withdraw(ctx, closed(e.openRequest("r_wdraw00002"), api.RequestAnsweredLocally, nil)); err != nil {
		t.Fatal(err)
	}
	if got, _ := fake.take(); len(got) != 0 {
		t.Fatalf("withdrawal of an unpushed request sent %v", got)
	}

	// Timed out: the "waiting in the terminal" push already replaced it.
	r3 := e.openRequest("r_wdraw00003")
	_ = e.srv.Push(ctx, notifyRequest(r3))
	fake.take()
	if err := e.srv.Withdraw(ctx, closed(r3, api.RequestExpired, nil)); err != nil {
		t.Fatal(err)
	}
	if got, _ := fake.take(); len(got) != 0 {
		t.Fatalf("expired request withdrawn with %v", got)
	}

	// Answered in the terminal: every device that got it is told.
	r4 := e.openRequest("r_wdraw00004")
	_ = e.srv.Push(ctx, notifyRequest(r4))
	fake.take()
	if err := e.srv.Withdraw(ctx, closed(r4, api.RequestDecided, &api.Decision{Behavior: api.BehaviorDeny, By: "cli"})); err != nil {
		t.Fatal(err)
	}
	if got, _ := fake.take(); strings.Join(got, ",") != "/a,/b" {
		t.Fatalf("CLI answer withdrawn on %v", got)
	}

	// Test pushes to one device are not remembered for withdrawal.
	if w := e.do("POST", "/api/push/test", nil, reqOpt{cookie: ckA}); w.Code != 204 {
		t.Fatal(w.Code)
	}
	fake.take()
	if e.srv.DeviceName(ctx, devA) != "iPhone" || e.srv.DeviceName(ctx, "d_nope") != "" {
		t.Fatal("DeviceName")
	}
}

func TestPushPayloads(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := api.Request{ID: "r_pay0000001", SessionID: "s_1", Agent: api.AgentClaude, Checkpoint: api.CheckpointPermission,
		Summary: "Run: npm publish", Status: api.RequestOpen, ExpiresAt: now.Add(5 * time.Minute)}
	p, ttl, urg := payloadFor(core.Notification{Kind: core.NotifyRequest, Title: "t", Body: "b", Request: &r}, now)
	if p.Type != "request" || p.Kind != "request" || p.Tag != "req-r_pay0000001" || !p.Actions || p.Request == nil ||
		p.Request.ID != r.ID || p.Request.Checkpoint != "permission" || p.Request.Summary != "Run: npm publish" || ttl != 300 || urg != "high" {
		t.Fatalf("request payload = %+v ttl=%d urgency=%s", p, ttl, urg)
	}

	// No one-tap actions for questions and plans: they need the app.
	for _, cp := range []api.Checkpoint{api.CheckpointQuestion, api.CheckpointPlan} {
		q := r
		q.Checkpoint = cp
		if p, _, _ := payloadFor(core.Notification{Kind: core.NotifyRequest, Request: &q}, now); p.Actions || p.Request.Checkpoint != string(cp) {
			t.Fatalf("%s payload offers actions: %+v", cp, p)
		}
	}

	r.Status = api.RequestDecided
	r.Decision = &api.Decision{Behavior: api.BehaviorAllow, Scope: api.ScopeSession, By: "device:d_1"}
	rp := resolvedPayload(r, func(id string) string { return map[string]string{"d_1": "iPhone"}[id] })
	if rp.Type != pushTypeResolved || rp.Tag != p.Tag || rp.Status != "decided" || rp.By != "from iPhone" || rp.Actions ||
		rp.Title != "✓ Allowed for this session" || rp.Body != "Run: npm publish — from iPhone" || rp.Title == "" {
		t.Fatalf("resolved payload = %+v", rp)
	}
	r.Status, r.Decision = api.RequestAnsweredLocally, nil
	if rp := resolvedPayload(r, nil); rp.Title != "✓ Answered in the terminal" || rp.By != "" || rp.Body != "Run: npm publish" {
		t.Fatalf("answered locally = %+v", rp)
	}
}
