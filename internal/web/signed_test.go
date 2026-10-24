package web

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/web/webtest"
)

func (e *env) signAction(k *ecdsa.PrivateKey, action, target string, payload any) signedBody {
	e.t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		e.t.Fatal(err)
	}
	nonceN++
	nonce := fmt.Sprintf("nonce-act-%016d", nonceN)
	ts := e.clock.Now().UnixMilli()
	return signedBody{Payload: string(b), TS: ts, Nonce: nonce, Sig: signP1363(e.t, k, CanonicalAction(action, target, string(b), ts, nonce))}
}

type actionFixture struct {
	Action    string `json:"action"`
	Target    string `json:"target"`
	Payload   string `json:"payload"`
	TS        int64  `json:"ts"`
	Nonce     string `json:"nonce"`
	PublicKey string `json:"public_key"`
	Canonical string `json:"canonical"`
	Sig       string `json:"sig"`
}

// The fixture is produced by the PWA's own keystore.js (canonicalAction)
// and real WebCrypto, so this pins the Go and JavaScript formats together.
func TestSignedActionWebCryptoFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/webcrypto-action.json")
	if err != nil {
		t.Fatal(err)
	}
	var f actionFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if got := CanonicalAction(f.Action, f.Target, f.Payload, f.TS, f.Nonce); got != f.Canonical {
		t.Fatalf("canonical mismatch:\n%q\n%q", got, f.Canonical)
	}
	pub, err := parsePublicKey(f.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := decodeB64(f.Sig)
	if !VerifyP1363(pub, []byte(f.Canonical), sig) {
		t.Fatal("WebCrypto action signature rejected")
	}

	// End to end through POST /api/requests/{id}/respond.
	e := newEnv(t)
	e.clock.t = timeMillis(f.TS)
	ck, _ := e.pair(f.PublicKey)
	r := e.openRequest(f.Target)
	w := e.do("POST", "/api/requests/"+r.ID+"/respond", signedBody{Payload: f.Payload, TS: f.TS, Nonce: f.Nonce, Sig: f.Sig}, reqOpt{cookie: ck})
	if w.Code != 200 {
		t.Fatalf("respond: %d %s", w.Code, w.Body)
	}
	ds := e.fc.Decisions()
	if len(ds) != 1 || ds[0].Answers["Which color?"] != "Blue, Green" || ds[0].Behavior != api.BehaviorAllow {
		t.Fatalf("decision = %+v", ds)
	}
}

func TestRespondSignedActionRejections(t *testing.T) {
	e := newEnv(t)
	k, pub := genKey(t)
	ck, devID := e.pair(pub)
	r1, r2 := e.openRequest("r_one"), e.openRequest("r_two")
	payload := respondBody{Behavior: api.BehaviorAllow, Answers: map[string]string{"Q": "A"}}

	// Signed for r1, posted to r2: the target is part of the signature.
	w := e.do("POST", "/api/requests/"+r2.ID+"/respond", e.signAction(k, ActionDecide, r1.ID, payload), reqOpt{cookie: ck})
	if w.Code != 403 || errCode(t, w) != "bad_signature" {
		t.Fatalf("wrong target: %d %s", w.Code, w.Body)
	}
	// Payload swapped after signing.
	sb := e.signAction(k, ActionDecide, r1.ID, payload)
	sb.Payload = `{"behavior":"allow","answers":{"Q":"B"}}`
	if w := e.do("POST", "/api/requests/"+r1.ID+"/respond", sb, reqOpt{cookie: ck}); w.Code != 403 || errCode(t, w) != "bad_signature" {
		t.Fatalf("tampered payload: %d %s", w.Code, w.Body)
	}
	// Another action name over the same target and payload.
	if w := e.do("POST", "/api/requests/"+r1.ID+"/respond", e.signAction(k, ActionReply, r1.ID, payload), reqOpt{cookie: ck}); w.Code != 403 {
		t.Fatalf("wrong action: %d %s", w.Code, w.Body)
	}
	// Unknown payload fields are refused (strict decoding).
	if w := e.do("POST", "/api/requests/"+r1.ID+"/respond", e.signAction(k, ActionDecide, r1.ID, map[string]any{"behavior": "allow", "extra": 1}), reqOpt{cookie: ck}); w.Code != 400 {
		t.Fatalf("unknown field: %d %s", w.Code, w.Body)
	}
	// Answers only go with allow.
	if w := e.do("POST", "/api/requests/"+r1.ID+"/respond", e.signAction(k, ActionDecide, r1.ID, respondBody{Behavior: api.BehaviorDeny, Answers: map[string]string{"Q": "A"}}), reqOpt{cookie: ck}); w.Code != 400 {
		t.Fatalf("deny with answers: %d %s", w.Code, w.Body)
	}
	// Good, then replayed.
	good := e.signAction(k, ActionDecide, r1.ID, payload)
	if w := e.do("POST", "/api/requests/"+r1.ID+"/respond", good, reqOpt{cookie: ck}); w.Code != 200 {
		t.Fatalf("good: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/requests/"+r1.ID+"/respond", good, reqOpt{cookie: ck}); w.Code != 403 || errCode(t, w) != "replayed" {
		t.Fatalf("replay: %d %s", w.Code, w.Body)
	}
	// A v1 decide signature is not a v2 action signature.
	v1 := e.signed(k, r2.ID, api.BehaviorAllow, api.ScopeOnce)
	if w := e.do("POST", "/api/requests/"+r2.ID+"/respond", signedBody{Payload: `{"behavior":"allow"}`, TS: v1.TS, Nonce: v1.Nonce, Sig: v1.Sig}, reqOpt{cookie: ck}); w.Code != 403 {
		t.Fatalf("v1 sig as v2: %d %s", w.Code, w.Body)
	}
	if ds := e.fc.Decisions(); len(ds) != 1 || ds[0].By != "device:"+devID {
		t.Fatalf("decisions = %+v", ds)
	}
	// Unsigned and cookie-less calls never reach the core.
	if w := e.do("POST", "/api/requests/"+r2.ID+"/respond", payload, reqOpt{cookie: ck}); w.Code != 400 {
		t.Fatalf("unsigned: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/requests/"+r2.ID+"/respond", e.signAction(k, ActionDecide, r2.ID, payload), reqOpt{}); w.Code != 401 {
		t.Fatalf("no cookie: %d %s", w.Code, w.Body)
	}
}

// replyCore adds the optional Replier capability to the fake core.
type replyCore struct {
	*webtest.FakeCore
	mu      sync.Mutex
	replies []api.Reply
	by      []string
}

func (c *replyCore) Reply(_ context.Context, id string, r api.Reply, by string) (api.ReplyResult, error) {
	if id == "s_codex" {
		return api.ReplyResult{}, fmt.Errorf("%w: run Codex with `pilothouse run codex` to reply from your phone", core.ErrUnsupported)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.replies = append(c.replies, r)
	c.by = append(c.by, by)
	return api.ReplyResult{Via: api.ReplyMessage}, nil
}

func TestCapabilitiesOptionalAndReplySigned(t *testing.T) {
	// The plain fake core has no Replier: 501, before any signature work.
	e := newEnv(t)
	k, pub := genKey(t)
	ck, _ := e.pair(pub)
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/sessions/s1/reply"}, {"GET", "/api/sessions/s1/activity"}, {"GET", "/api/insights"},
		{"GET", "/api/rules"}, {"GET", "/api/repos"}, {"GET", "/api/tasks"}, {"GET", "/api/requests/r1/suggestions"},
	} {
		if w := e.do(c.method, c.path, e.signAction(k, ActionReply, "s1", api.Reply{Text: "hi"}), reqOpt{cookie: ck}); w.Code != 501 || errCode(t, w) != "unsupported" {
			t.Fatalf("%s %s: %d %s", c.method, c.path, w.Code, w.Body)
		}
	}

	rc := &replyCore{}
	e = newEnv(t, func(d *Deps) { rc.FakeCore = d.Core.(*webtest.FakeCore); d.Core = rc })
	k, pub = genKey(t)
	ck, devID := e.pair(pub)
	w := e.do("POST", "/api/sessions/s_one/reply", e.signAction(k, ActionReply, "s_one", api.Reply{Text: "also add tests\n"}), reqOpt{cookie: ck})
	if w.Code != 200 {
		t.Fatalf("reply: %d %s", w.Code, w.Body)
	}
	var res api.ReplyResult
	mustJSON(t, w, &res)
	if res.Via != api.ReplyMessage || len(rc.replies) != 1 || rc.replies[0].Text != "also add tests" || rc.by[0] != "device:"+devID {
		t.Fatalf("res=%+v replies=%+v by=%v", res, rc.replies, rc.by)
	}
	// Signed for one session, sent to another.
	if w := e.do("POST", "/api/sessions/s_two/reply", e.signAction(k, ActionReply, "s_one", api.Reply{Text: "x"}), reqOpt{cookie: ck}); w.Code != 403 {
		t.Fatalf("cross-session: %d %s", w.Code, w.Body)
	}
	// Empty and oversized replies.
	if w := e.do("POST", "/api/sessions/s_one/reply", e.signAction(k, ActionReply, "s_one", api.Reply{Text: "  \n"}), reqOpt{cookie: ck}); w.Code != 400 {
		t.Fatalf("empty: %d %s", w.Code, w.Body)
	}
	// ErrUnsupported carries a person-readable reason.
	w = e.do("POST", "/api/sessions/s_codex/reply", e.signAction(k, ActionReply, "s_codex", api.Reply{Text: "x"}), reqOpt{cookie: ck})
	if w.Code != 501 || errCode(t, w) != "unsupported" {
		t.Fatalf("unsupported: %d %s", w.Code, w.Body)
	}
	var ae api.Error
	mustJSON(t, w, &ae)
	if ae.Error != "run Codex with `pilothouse run codex` to reply from your phone" {
		t.Fatalf("message = %q", ae.Error)
	}
}

func timeMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }
