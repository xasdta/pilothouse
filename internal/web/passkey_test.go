package web

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

const pkHost = "localhost:7517"

var pkB64 = base64.RawURLEncoding

// authenticator is a fake WebAuthn platform authenticator (a passkey).
type authenticator struct {
	t      *testing.T
	key    *ecdsa.PrivateKey
	pub    string // base64url SPKI, as getPublicKey() returns
	credID []byte
	rpID   string
	flags  byte
	count  uint32
}

func newAuthenticator(t *testing.T) *authenticator {
	k, pub := genKey(t)
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	return &authenticator{t: t, key: k, pub: pub, credID: id, rpID: "localhost", flags: flagUP | flagUV}
}

func (a *authenticator) authData(attested bool) []byte {
	h := sha256.Sum256([]byte(a.rpID))
	b := append([]byte{}, h[:]...)
	f := a.flags
	if attested {
		f |= flagAT
	}
	b = append(b, f)
	b = binary.BigEndian.AppendUint32(b, a.count)
	if attested {
		b = append(b, make([]byte, 16)...) // aaguid
		b = binary.BigEndian.AppendUint16(b, uint16(len(a.credID)))
		b = append(b, a.credID...)
		b = append(b, 0xa0) // stand-in COSE key; the server uses getPublicKey()
	}
	return b
}

func clientDataJSON(typ, challenge, origin string) []byte {
	b, _ := json.Marshal(clientData{Type: typ, Challenge: challenge, Origin: origin})
	return b
}

func (a *authenticator) create(challenge string) passkeyAddBody {
	return passkeyAddBody{
		CredentialID: pkB64.EncodeToString(a.credID),
		PublicKey:    a.pub,
		ClientData:   pkB64.EncodeToString(clientDataJSON("webauthn.create", challenge, "http://"+pkHost)),
		AuthData:     pkB64.EncodeToString(a.authData(true)),
	}
}

func (a *authenticator) get(challenge string, cd []byte) restoreBody {
	a.t.Helper()
	if cd == nil {
		cd = clientDataJSON("webauthn.get", challenge, "http://"+pkHost)
	}
	ad := a.authData(false)
	ch := sha256.Sum256(cd)
	h := sha256.Sum256(append(append([]byte{}, ad...), ch[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, h[:])
	if err != nil {
		a.t.Fatal(err)
	}
	return restoreBody{CredentialID: pkB64.EncodeToString(a.credID), ClientData: pkB64.EncodeToString(cd),
		AuthData: pkB64.EncodeToString(ad), Signature: pkB64.EncodeToString(sig)}
}

func (e *env) challenge(ck, purpose, newKey string) passkeyChallengeResponse {
	e.t.Helper()
	w := e.do("POST", "/api/passkeys/challenge", passkeyChallengeBody{Purpose: purpose, PublicKey: newKey}, reqOpt{cookie: ck, host: pkHost})
	if w.Code != 200 {
		e.t.Fatalf("challenge %s: %d %s", purpose, w.Code, w.Body)
	}
	var c passkeyChallengeResponse
	mustJSON(e.t, w, &c)
	return c
}

// addPasskey registers a for the device (signed with its key k).
func (e *env) addPasskey(ck string, k *ecdsa.PrivateKey, a *authenticator) {
	e.t.Helper()
	c := e.challenge(ck, purposeRegister, "")
	w := e.do("POST", "/api/passkeys", e.signAction(k, ActionPasskey, "add", a.create(c.Challenge)), reqOpt{cookie: ck, host: pkHost})
	if w.Code != 200 {
		e.t.Fatalf("add passkey: %d %s", w.Code, w.Body)
	}
}

func (e *env) storedKey(id string) string {
	e.t.Helper()
	var k string
	if err := e.db.QueryRow(`SELECT public_key FROM web_devices WHERE id = ?`, id).Scan(&k); err != nil {
		e.t.Fatal(err)
	}
	return k
}

func TestPasskeyRestoresSigningKey(t *testing.T) {
	e := newEnv(t)
	k, pub := genKey(t)
	ck, id := e.pair(pub)
	a := newAuthenticator(t)

	c := e.challenge(ck, purposeRegister, "")
	if c.RPID != "localhost" || string(mustB64(t, c.UserID)) != id || len(c.Creds) != 0 {
		t.Fatalf("register challenge = %+v", c)
	}
	// The cookie alone can't add a passkey: the device key must sign it.
	if w := e.do("POST", "/api/passkeys", a.create(c.Challenge), reqOpt{cookie: ck, host: pkHost}); w.Code != 400 {
		t.Fatalf("unsigned add: %d %s", w.Code, w.Body)
	}
	other, _ := genKey(t)
	if w := e.do("POST", "/api/passkeys", e.signAction(other, ActionPasskey, "add", a.create(c.Challenge)), reqOpt{cookie: ck, host: pkHost}); w.Code != 403 {
		t.Fatalf("add signed by another key: %d %s", w.Code, w.Body)
	}
	e.addPasskey(ck, k, a)
	var me meResponse
	mustJSON(t, e.do("GET", "/api/me", nil, reqOpt{cookie: ck, host: pkHost}), &me)
	if !me.Device.Passkey {
		t.Fatal("me.device.passkey = false after adding one")
	}

	// The key is lost. The phone makes a new one and the passkey vouches for it.
	k2, pub2 := genKey(t)
	rc := e.challenge(ck, purposeRestore, pub2)
	if len(rc.Creds) != 1 || rc.Creds[0] != pkB64.EncodeToString(a.credID) {
		t.Fatalf("restore challenge creds = %v", rc.Creds)
	}
	w := e.do("POST", "/api/device/restore", a.get(rc.Challenge, nil), reqOpt{cookie: ck, host: pkHost})
	if w.Code != 200 {
		t.Fatalf("restore: %d %s", w.Code, w.Body)
	}
	if got := e.storedKey(id); got != pub2 {
		t.Fatalf("device key = %s, want the new key", got)
	}
	// The new key signs; the old one no longer does.
	r := e.openRequest("r_after")
	if w := e.do("POST", "/api/requests/"+r.ID+"/decide", e.signed(k, r.ID, api.BehaviorAllow, api.ScopeOnce), reqOpt{cookie: ck}); w.Code != 403 {
		t.Fatalf("old key decide: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/requests/"+r.ID+"/decide", e.signed(k2, r.ID, api.BehaviorAllow, api.ScopeOnce), reqOpt{cookie: ck}); w.Code != 200 {
		t.Fatalf("new key decide: %d %s", w.Code, w.Body)
	}

	// Revoking the device removes its passkeys.
	if w := e.localDo("DELETE", "/v1/devices/"+id); w.Code != 204 {
		t.Fatalf("revoke: %d", w.Code)
	}
	var n int
	_ = e.db.QueryRow(`SELECT COUNT(*) FROM web_passkeys`).Scan(&n)
	if n != 0 {
		t.Fatalf("passkeys after revoke = %d", n)
	}
}

func TestPasskeyRestoreRejections(t *testing.T) {
	e := newEnv(t)
	k, pub := genKey(t)
	ck, id := e.pair(pub)
	_, newPub := genKey(t)

	// No passkey yet: nothing to restore with.
	if w := e.do("POST", "/api/passkeys/challenge", passkeyChallengeBody{Purpose: purposeRestore, PublicKey: newPub}, reqOpt{cookie: ck, host: pkHost}); w.Code != 409 || errCode(t, w) != "no_passkey" {
		t.Fatalf("no passkey: %d %s", w.Code, w.Body)
	}
	// IP hosts can't be WebAuthn RP IDs.
	if w := e.do("POST", "/api/passkeys/challenge", passkeyChallengeBody{Purpose: purposeRegister}, reqOpt{cookie: ck}); w.Code != 501 {
		t.Fatalf("IP host: %d %s", w.Code, w.Body)
	}
	// Without a cookie there is no device to restore.
	if w := e.do("POST", "/api/passkeys/challenge", passkeyChallengeBody{Purpose: purposeRegister}, reqOpt{host: pkHost}); w.Code != 401 {
		t.Fatalf("no cookie: %d %s", w.Code, w.Body)
	}

	a := newAuthenticator(t)
	e.addPasskey(ck, k, a)

	restore := func(name string, body restoreBody, wantCode int) {
		t.Helper()
		e.clock.Add(time.Minute) // stay under the restore rate limit
		w := e.do("POST", "/api/device/restore", body, reqOpt{cookie: ck, host: pkHost})
		if w.Code != wantCode {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body)
		}
		if wantCode != 200 && e.storedKey(id) != pub {
			t.Fatalf("%s changed the device key", name)
		}
	}
	fresh := func() string { return e.challenge(ck, purposeRestore, newPub).Challenge }

	// A register challenge can't be used to restore.
	restore("register challenge", a.get(e.challenge(ck, purposeRegister, "").Challenge, nil), 403)
	// Another origin.
	restore("wrong origin", a.get("", clientDataJSON("webauthn.get", fresh(), "https://evil.example")), 403)
	// A create-type client data.
	restore("wrong type", a.get("", clientDataJSON("webauthn.create", fresh(), "http://"+pkHost)), 403)
	// Signed by a key that isn't the registered passkey.
	impostor := newAuthenticator(t)
	impostor.credID = a.credID
	restore("wrong key", impostor.get(fresh(), nil), 403)
	// A passkey that didn't verify the person.
	a.flags = flagUP
	restore("no user verification", a.get(fresh(), nil), 403)
	a.flags = flagUP | flagUV
	// A passkey for another site.
	a.rpID = "example.com"
	restore("wrong rp id", a.get(fresh(), nil), 403)
	a.rpID = "localhost"
	// A passkey registered to another device.
	k3, pub3 := genKey(t)
	ck3, _ := e.pair(pub3)
	b := newAuthenticator(t)
	e.addPasskey(ck3, k3, b)
	restore("other device's passkey", b.get(fresh(), nil), 403)

	// Good once; the challenge can't be replayed.
	good := a.get(fresh(), nil)
	restore("good", good, 200)
	if e.storedKey(id) != newPub {
		t.Fatal("key not replaced")
	}
	w := e.do("POST", "/api/device/restore", good, reqOpt{cookie: ck, host: pkHost})
	if w.Code != 403 && w.Code != 429 {
		t.Fatalf("replay: %d %s", w.Code, w.Body)
	}
}

func TestPasskeyCounterMustAdvance(t *testing.T) {
	e := newEnv(t)
	k, pub := genKey(t)
	ck, _ := e.pair(pub)
	a := newAuthenticator(t)
	a.count = 5
	e.addPasskey(ck, k, a)
	_, newPub := genKey(t)
	if w := e.do("POST", "/api/device/restore", a.get(e.challenge(ck, purposeRestore, newPub).Challenge, nil), reqOpt{cookie: ck, host: pkHost}); w.Code != 403 {
		t.Fatalf("same counter: %d %s", w.Code, w.Body)
	}
	a.count = 6
	if w := e.do("POST", "/api/device/restore", a.get(e.challenge(ck, purposeRestore, newPub).Challenge, nil), reqOpt{cookie: ck, host: pkHost}); w.Code != 200 {
		t.Fatalf("advanced counter: %d %s", w.Code, w.Body)
	}
}

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := pkB64.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Pairing again from a browser that is still signed in (its key was lost)
// replaces that device instead of leaving a stale one behind.
func TestPairAgainReplacesSignedInDevice(t *testing.T) {
	e := newEnv(t)
	_, pub := genKey(t)
	ck, oldID := e.pair(pub)
	e.subscribe(t, ck, "https://push.example/phone")
	_, pub2 := genKey(t)
	w := e.do("POST", "/api/pair", pairBody{Code: e.pairCode(), Name: "Test phone", PublicKey: pub2}, reqOpt{cookie: ck})
	if w.Code != 200 {
		t.Fatalf("pair again: %d %s", w.Code, w.Body)
	}
	var me meResponse
	mustJSON(t, w, &me)
	if me.Device.ID == oldID {
		t.Fatal("pair again reused the old device")
	}
	if w := e.do("GET", "/api/me", nil, reqOpt{cookie: ck}); w.Code != 401 {
		t.Fatalf("old cookie after pair again: %d", w.Code)
	}
	var ds []api.Device
	mustJSON(t, e.localDo("GET", "/v1/devices"), &ds)
	if len(ds) != 1 || ds[0].ID != me.Device.ID {
		t.Fatalf("devices = %+v", ds)
	}
	// The browser's push subscription moved to the new device.
	var owner string
	if err := e.db.QueryRow(`SELECT device_id FROM web_push_subs WHERE endpoint = ?`, "https://push.example/phone").Scan(&owner); err != nil || owner != me.Device.ID {
		t.Fatalf("push subscription owner = %q, %v; want %s", owner, err, me.Device.ID)
	}
	if !ds[0].PushOK {
		t.Fatal("new device doesn't report push")
	}
}
