package web

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Passkey key recovery.
//
// iOS can clear a web app's storage, and keeps Safari and the Home Screen
// app apart, so the non-extractable device key can disappear while the
// session cookie survives. A passkey lives in the platform keychain
// instead, so it can vouch for a replacement key:
//
//   - The device key authorizes adding a passkey (signed action "passkey").
//   - The passkey (with user verification) authorizes replacing the device
//     key (POST /api/device/restore), and nothing else.
//
// The cookie alone can do neither: it still never approves anything.
// Every approval stays signed by the device key.

// ActionPasskey adds a passkey to this device. Target: "add"; payload:
// passkeyAddBody.
const ActionPasskey = "passkey"

const (
	passkeyChallengeTTL  = 5 * time.Minute
	maxPasskeyChallenges = 256
	maxPasskeysPerDevice = 8

	purposeRegister = "register"
	purposeRestore  = "restore"

	// Authenticator data flags (WebAuthn §6.1).
	flagUP = 0x01 // user present
	flagUV = 0x04 // user verified
	flagAT = 0x40 // attested credential data included
)

type passkeyChallenge struct {
	device  string
	purpose string
	rpID    string
	newKey  string // restore: the device key the passkey will vouch for
	expires time.Time
}

// rpIDOf is the WebAuthn relying-party ID for this request: the host name
// the browser sees. IP addresses can't be RP IDs.
func rpIDOf(r *http.Request) (string, bool) {
	u, err := url.Parse(originOf(r).base)
	if err != nil {
		return "", false
	}
	host := u.Hostname()
	if host == "" || net.ParseIP(host) != nil {
		return "", false
	}
	return host, true
}

// issueChallenge stores a single-use challenge for d and returns it.
func (s *Server) issueChallenge(c passkeyChallenge) string {
	ch := randomToken(32)
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.challenges == nil {
		s.challenges = map[string]passkeyChallenge{}
	}
	for k, v := range s.challenges {
		if now.After(v.expires) || (v.device == c.device && v.purpose == c.purpose) {
			delete(s.challenges, k)
		}
	}
	if len(s.challenges) >= maxPasskeyChallenges {
		for k := range s.challenges {
			delete(s.challenges, k)
			break
		}
	}
	c.expires = now.Add(passkeyChallengeTTL)
	s.challenges[ch] = c
	return ch
}

// takeChallenge spends a challenge issued to device for purpose.
func (s *Server) takeChallenge(ch, device, purpose string) (passkeyChallenge, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.challenges[ch]
	if !ok {
		return passkeyChallenge{}, false
	}
	delete(s.challenges, ch)
	if c.device != device || c.purpose != purpose || s.now().After(c.expires) {
		return passkeyChallenge{}, false
	}
	return c, true
}

type passkeyChallengeBody struct {
	Purpose   string `json:"purpose"`
	PublicKey string `json:"public_key,omitempty"` // restore: the new device key
}

type passkeyChallengeResponse struct {
	Challenge string   `json:"challenge"` // base64url
	RPID      string   `json:"rp_id"`
	UserID    string   `json:"user_id"` // base64url
	UserName  string   `json:"user_name"`
	Creds     []string `json:"credentials"` // register: exclude; restore: allow
}

func (s *Server) handlePasskeyChallenge(w http.ResponseWriter, r *http.Request) {
	var b passkeyChallengeBody
	if !decodeBody(w, r, &b) {
		return
	}
	rpID, ok := rpIDOf(r)
	if !ok {
		writeUnsupported(w, "passkeys need a host name — open Pilothouse at its Tailscale or localhost URL")
		return
	}
	d := deviceOf(r)
	c := passkeyChallenge{device: d.ID, purpose: b.Purpose, rpID: rpID}
	switch b.Purpose {
	case purposeRegister:
	case purposeRestore:
		if _, err := parsePublicKey(b.PublicKey); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		c.newKey = strings.TrimSpace(b.PublicKey)
	default:
		writeErr(w, http.StatusBadRequest, "bad_request", "purpose must be register or restore")
		return
	}
	creds, err := s.passkeyIDs(r.Context(), d.ID, rpID)
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	if b.Purpose == purposeRestore && len(creds) == 0 {
		writeErr(w, http.StatusConflict, "no_passkey", "this device has no passkey — pair it again with `pilothouse pair`")
		return
	}
	if b.Purpose == purposeRegister && len(creds) >= maxPasskeysPerDevice {
		writeErr(w, http.StatusConflict, "too_many", "this device already has the most passkeys allowed")
		return
	}
	writeJSON(w, http.StatusOK, passkeyChallengeResponse{
		Challenge: s.issueChallenge(c),
		RPID:      rpID,
		UserID:    base64.RawURLEncoding.EncodeToString([]byte(d.ID)),
		UserName:  d.Name + " · " + s.machineName(),
		Creds:     creds,
	})
}

func (s *Server) passkeyIDs(ctx context.Context, device, rpID string) ([]string, error) {
	rows, err := s.d.DB.QueryContext(ctx, `SELECT cred_id FROM web_passkeys WHERE device_id = ? AND rp_id = ? ORDER BY created_at`, device, rpID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// clientData is the part of WebAuthn's clientDataJSON we check.
type clientData struct {
	Type        string `json:"type"`
	Challenge   string `json:"challenge"`
	Origin      string `json:"origin"`
	CrossOrigin bool   `json:"crossOrigin"`
}

// checkClientData verifies type, origin and the challenge, and spends it.
func (s *Server) checkClientData(r *http.Request, raw []byte, typ, purpose string) (passkeyChallenge, error) {
	var cd clientData
	if err := json.Unmarshal(raw, &cd); err != nil {
		return passkeyChallenge{}, errors.New("client data is not JSON")
	}
	if cd.Type != typ {
		return passkeyChallenge{}, errors.New("client data has the wrong type")
	}
	if cd.CrossOrigin || !strings.EqualFold(cd.Origin, originOf(r).base) {
		return passkeyChallenge{}, errors.New("client data is from another origin")
	}
	c, ok := s.takeChallenge(cd.Challenge, deviceOf(r).ID, purpose)
	if !ok {
		return passkeyChallenge{}, errors.New("the passkey challenge is wrong or expired — try again")
	}
	return c, nil
}

// authData is parsed WebAuthn authenticator data.
type authData struct {
	rpIDHash  []byte
	flags     byte
	signCount uint32
	credID    []byte // only when flagAT is set
}

func parseAuthData(b []byte) (authData, error) {
	if len(b) < 37 {
		return authData{}, errors.New("authenticator data is too short")
	}
	a := authData{rpIDHash: b[:32], flags: b[32], signCount: binary.BigEndian.Uint32(b[33:37])}
	if a.flags&flagAT != 0 {
		// aaguid(16) credentialIdLength(2) credentialId(L) credentialPublicKey…
		rest := b[37:]
		if len(rest) < 18 {
			return authData{}, errors.New("attested credential data is too short")
		}
		n := int(binary.BigEndian.Uint16(rest[16:18]))
		if len(rest) < 18+n {
			return authData{}, errors.New("credential id is truncated")
		}
		a.credID = rest[18 : 18+n]
	}
	return a, nil
}

// check verifies the RP ID hash and that the person was verified.
func (a authData) check(rpID string) error {
	h := sha256.Sum256([]byte(rpID))
	if !bytes.Equal(a.rpIDHash, h[:]) {
		return errors.New("passkey is for another site")
	}
	if a.flags&flagUP == 0 || a.flags&flagUV == 0 {
		return errors.New("passkey did not verify you (Face ID, Touch ID or passcode is required)")
	}
	return nil
}

type passkeyAddBody struct {
	CredentialID string `json:"credential_id"` // base64url
	PublicKey    string `json:"public_key"`    // base64url SPKI (getPublicKey())
	ClientData   string `json:"client_data"`   // base64url clientDataJSON
	AuthData     string `json:"auth_data"`     // base64url authenticator data
}

// handlePasskeyAdd registers a passkey, authorized by the device key.
func (s *Server) handlePasskeyAdd(w http.ResponseWriter, r *http.Request) {
	var b passkeyAddBody
	if !s.readSigned(w, r, ActionPasskey, "add", &b) {
		return
	}
	bad := func(msg string) { writeErr(w, http.StatusBadRequest, "bad_passkey", msg) }
	credID, err := decodeB64(b.CredentialID)
	if err != nil || len(credID) == 0 || len(credID) > 1023 {
		bad("credential_id is not valid")
		return
	}
	if _, err := parsePublicKey(b.PublicKey); err != nil {
		bad("passkey must use ECDSA P-256 (" + err.Error() + ")")
		return
	}
	cdRaw, err1 := decodeB64(b.ClientData)
	adRaw, err2 := decodeB64(b.AuthData)
	if err1 != nil || err2 != nil {
		bad("client_data and auth_data must be base64url")
		return
	}
	c, err := s.checkClientData(r, cdRaw, "webauthn.create", purposeRegister)
	if err != nil {
		bad(err.Error())
		return
	}
	ad, err := parseAuthData(adRaw)
	if err == nil {
		err = ad.check(c.rpID)
	}
	if err == nil && (ad.flags&flagAT == 0 || !bytes.Equal(ad.credID, credID)) {
		err = errors.New("credential id does not match the authenticator data")
	}
	if err != nil {
		bad(err.Error())
		return
	}
	d := deviceOf(r)
	if _, err := s.d.DB.ExecContext(r.Context(),
		`INSERT INTO web_passkeys (cred_id, device_id, rp_id, public_key, sign_count, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		base64.RawURLEncoding.EncodeToString(credID), d.ID, c.rpID, strings.TrimSpace(b.PublicKey), ad.signCount, ms(s.now())); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeErr(w, http.StatusConflict, "exists", "this passkey is already registered")
			return
		}
		s.writeCoreErr(w, err)
		return
	}
	s.log.Info("passkey added", "device", d.ID, "rp_id", c.rpID)
	s.afterDeviceChange(w, r, d.ID)
}

type restoreBody struct {
	CredentialID string `json:"credential_id"` // base64url
	ClientData   string `json:"client_data"`   // base64url clientDataJSON
	AuthData     string `json:"auth_data"`     // base64url authenticator data
	Signature    string `json:"signature"`     // base64url ASN.1 DER
}

// handleRestore replaces this device's signing key with the one named in
// the restore challenge, if a passkey registered to this device signed it.
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	if ok, wait := s.pairLimit.allow(clientKey(r)); !ok {
		w.Header().Set("Retry-After", itoa(int(wait.Seconds())+1))
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "too many attempts; wait a minute and try again")
		return
	}
	var b restoreBody
	if !decodeBody(w, r, &b) {
		return
	}
	d := deviceOf(r)
	fail := func(msg string) {
		s.pairLimit.fail(clientKey(r))
		writeErr(w, http.StatusForbidden, "bad_passkey", msg)
	}
	cdRaw, err1 := decodeB64(b.ClientData)
	adRaw, err2 := decodeB64(b.AuthData)
	sig, err3 := decodeB64(b.Signature)
	credID, err4 := decodeB64(b.CredentialID)
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "credential_id, client_data, auth_data and signature must be base64url")
		return
	}
	c, err := s.checkClientData(r, cdRaw, "webauthn.get", purposeRestore)
	if err != nil {
		fail(err.Error())
		return
	}
	var pubB64 string
	var count int64
	err = s.d.DB.QueryRowContext(r.Context(),
		`SELECT public_key, sign_count FROM web_passkeys WHERE cred_id = ? AND device_id = ? AND rp_id = ?`,
		base64.RawURLEncoding.EncodeToString(credID), d.ID, c.rpID).Scan(&pubB64, &count)
	if errors.Is(err, sql.ErrNoRows) {
		fail("that passkey isn’t registered to this device")
		return
	}
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	ad, err := parseAuthData(adRaw)
	if err == nil {
		err = ad.check(c.rpID)
	}
	if err != nil {
		fail(err.Error())
		return
	}
	pub, err := parsePublicKey(pubB64)
	if err != nil {
		fail("stored passkey is invalid")
		return
	}
	cdHash := sha256.Sum256(cdRaw)
	signed := sha256.Sum256(append(append([]byte{}, adRaw...), cdHash[:]...))
	if !ecdsa.VerifyASN1(pub, signed[:], sig) {
		fail("passkey signature does not verify")
		return
	}
	// Synced passkeys report 0; a counter that moves must move forward.
	if (count != 0 || ad.signCount != 0) && int64(ad.signCount) <= count {
		fail("passkey counter went backwards — it may have been cloned")
		return
	}
	now := ms(s.now())
	tx, err := s.d.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(r.Context(), `UPDATE web_passkeys SET sign_count = ?, last_used_at = ? WHERE cred_id = ?`,
		ad.signCount, now, base64.RawURLEncoding.EncodeToString(credID)); err != nil {
		s.writeCoreErr(w, err)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `UPDATE web_devices SET public_key = ? WHERE id = ? AND revoked_at IS NULL`, c.newKey, d.ID); err != nil {
		s.writeCoreErr(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		s.writeCoreErr(w, err)
		return
	}
	s.log.Info("device key restored with passkey", "device", d.ID)
	s.afterDeviceChange(w, r, d.ID)
}

// afterDeviceChange reloads the device, announces it and answers /api/me.
func (s *Server) afterDeviceChange(w http.ResponseWriter, r *http.Request, id string) {
	d, err := s.device(r.Context(), id)
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	s.emitDevice(r.Context(), d.Device)
	writeJSON(w, http.StatusOK, s.me(d))
}
