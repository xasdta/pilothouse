package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// Signed actions (docs/PLAN-0.2.md §5). Everything a device does that
// changes what an agent may do or will do — answering with answers,
// replying, typing into a terminal, editing rules, starting or shipping a
// task — is signed with the device's non-extractable key, like decisions.
//
// The device signs
//
//	pilothouse-action:v2\n{action}\n{target}\n{b64url(sha256(payload))}\n{ts}\n{nonce}
//
// where payload is the exact JSON text it sends in the body's "payload"
// string. Sending the payload as a string means both sides hash the same
// bytes; there is no JSON canonicalization to disagree about.

// ActionContext prefixes every signed action. It differs from
// SignatureContext, so a v1 decision signature can't be replayed as an
// action or the other way round.
const ActionContext = "pilothouse-action:v2"

// Signed action names. The target names what the action applies to.
const (
	ActionDecide   = "decide"   // target: request id; payload: respondBody
	ActionReply    = "reply"    // target: session id; payload: api.Reply
	ActionTerminal = "terminal" // target: session id; payload: {} (mints an input token)
	ActionRule     = "rule"     // target: "add" | "remove:<name>" | "enable:<name>" | "disable:<name>" | "preset:<name>"
	ActionTask     = "task"     // target: "new" | task id
)

// maxSignedBody bounds a signed request body (the payload is inside it).
const maxSignedBody = 64 << 10

// CanonicalAction is the exact byte string a device signs for an action.
func CanonicalAction(action, target, payload string, ts int64, nonce string) string {
	h := sha256.Sum256([]byte(payload))
	return ActionContext + "\n" + action + "\n" + target + "\n" +
		base64.RawURLEncoding.EncodeToString(h[:]) + "\n" + strconv.FormatInt(ts, 10) + "\n" + nonce
}

// signedBody is the body of every signed action request.
type signedBody struct {
	Payload string `json:"payload"` // JSON text, exactly as signed
	TS      int64  `json:"ts"`      // Unix milliseconds
	Nonce   string `json:"nonce"`
	Sig     string `json:"sig"` // base64url IEEE P1363 r||s
}

// readSigned reads a signed action from the authenticated device on r,
// verifies it for (action, target), spends its nonce and decodes the
// payload strictly into v. On any failure it writes the error response
// and returns false.
func (s *Server) readSigned(w http.ResponseWriter, r *http.Request, action, target string, v any) bool {
	d := deviceOf(r)
	if d == nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "pair this device first")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSignedBody)
	var b signedBody
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return false
	}
	if !nonceRE.MatchString(b.Nonce) {
		writeErr(w, http.StatusBadRequest, "bad_request", "nonce must be 16-64 base64url characters")
		return false
	}
	if !s.verifyDeviceSig(w, r, d, CanonicalAction(action, target, b.Payload, b.TS, b.Nonce), b.Sig, b.TS, b.Nonce) {
		return false
	}
	pd := json.NewDecoder(bytes.NewReader([]byte(b.Payload)))
	pd.DisallowUnknownFields()
	if err := pd.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid payload: "+err.Error())
		return false
	}
	if pd.More() {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid payload: trailing data")
		return false
	}
	return true
}

// verifyDeviceSig checks ts against the clock, the signature against the
// device's key, and spends the nonce. It writes the error and returns
// false on failure. Shared by v1 decisions and v2 actions.
func (s *Server) verifyDeviceSig(w http.ResponseWriter, r *http.Request, d *deviceRow, canonical, sigB64 string, ts int64, nonce string) bool {
	skew := s.now().Sub(time.UnixMilli(ts))
	if skew > MaxClockSkew || skew < -MaxClockSkew {
		writeErr(w, http.StatusForbidden, "stale_signature", "signature timestamp is too far from the computer's clock — check the time on this device")
		return false
	}
	pub, err := parsePublicKey(d.PublicKey)
	if err != nil {
		writeErr(w, http.StatusForbidden, "bad_signature", "device key is invalid; pair this device again")
		return false
	}
	sig, err := decodeB64(sigB64)
	if err != nil || !VerifyP1363(pub, []byte(canonical), sig) {
		writeErr(w, http.StatusForbidden, "bad_signature", "signature does not match this device's key")
		return false
	}
	// The nonce outlives the whole acceptance window on both sides.
	fresh, err := s.useNonce(r.Context(), d.ID, nonce, time.UnixMilli(ts).Add(2*MaxClockSkew+time.Minute))
	if err != nil {
		s.writeCoreErr(w, err)
		return false
	}
	if !fresh {
		writeErr(w, http.StatusForbidden, "replayed", "this signed request was already used")
		return false
	}
	return true
}

// writeUnsupported answers a call the core or the session can't serve.
func writeUnsupported(w http.ResponseWriter, msg string) {
	if msg == "" {
		msg = "not supported here"
	}
	writeErr(w, http.StatusNotImplemented, "unsupported", msg)
}
