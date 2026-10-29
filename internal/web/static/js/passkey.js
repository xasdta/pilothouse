// Passkey key recovery (internal/web/passkey.go).
//
// The device key signs a new passkey into this device; later, if iOS clears
// the key, the passkey (Face ID / Touch ID) vouches for a fresh one. The
// passkey never signs approvals.
//
// iPhone Safari only shows the passkey sheet in direct response to a tap,
// so each button fetches its challenge (and, for restore, makes the new key)
// before it is tapped.
import { h, fill } from './dom.js';
import { api, signed, store, changed } from './api.js';
import { toast, refreshChrome } from './ui.js';

const b64u = (buf) => {
  const bytes = new Uint8Array(buf);
  let s = '';
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
};
const unb64u = (s) => Uint8Array.from(atob(s.replace(/-/g, '+').replace(/_/g, '/')), (c) => c.charCodeAt(0));
const creds = (ids) => ids.map((id) => ({ type: 'public-key', id: unb64u(id) }));

const FRESH_MS = 4 * 60 * 1000; // challenges live 5 minutes on the daemon

/** Passkeys need a secure page on a host name (not an IP address). */
export function passkeySupported() {
  const host = location.hostname;
  return !!(window.PublicKeyCredential && navigator.credentials && window.isSecureContext && self.PilothouseKeys) &&
    !/^[\d.]+$/.test(host) && !host.includes(':') && !host.startsWith('[');
}

/** Asks the browser not to evict this origin's storage (and the key in it). */
export function persistStorage() {
  const st = navigator.storage;
  if (!st || !st.persist) return;
  (st.persisted ? st.persisted() : Promise.resolve(false))
    .then((yes) => yes || st.persist())
    .catch(() => {});
}

function messageFor(e) {
  if (e && e.name === 'NotAllowedError') return 'Passkey cancelled.';
  if (e && e.name === 'InvalidStateError') return 'This passkey is already added.';
  return (e && e.message) || 'Passkey failed.';
}

function prepared(prepare) {
  let p = null;
  let at = 0;
  return {
    get() {
      if (!p || Date.now() - at > FRESH_MS) { p = prepare(); at = Date.now(); p.catch(() => { p = null; }); }
      return p;
    },
    reset() { p = null; },
  };
}

async function add(c) {
  const cred = await navigator.credentials.create({
    publicKey: {
      challenge: unb64u(c.challenge),
      rp: { id: c.rp_id, name: 'Pilothouse' },
      user: { id: unb64u(c.user_id), name: c.user_name, displayName: c.user_name },
      pubKeyCredParams: [{ type: 'public-key', alg: -7 }], // ES256: the daemon verifies P-256
      authenticatorSelection: { residentKey: 'preferred', userVerification: 'required' },
      excludeCredentials: creds(c.credentials),
      attestation: 'none',
      timeout: 120000,
    },
  });
  const r = cred.response;
  const spki = r.getPublicKey && r.getPublicKey();
  if (!spki || !r.getAuthenticatorData) throw new Error('This browser can’t add a passkey for Pilothouse.');
  return signed('/api/passkeys', 'passkey', 'add', {
    credential_id: b64u(cred.rawId),
    public_key: b64u(spki),
    client_data: b64u(r.clientDataJSON),
    auth_data: b64u(r.getAuthenticatorData()),
  });
}

async function restore({ rec, c }) {
  const cred = await navigator.credentials.get({
    publicKey: {
      challenge: unb64u(c.challenge),
      rpId: c.rp_id,
      allowCredentials: creds(c.credentials),
      userVerification: 'required',
      timeout: 120000,
    },
  });
  const r = cred.response;
  const me = await api('POST', '/api/device/restore', {
    credential_id: b64u(cred.rawId),
    client_data: b64u(r.clientDataJSON),
    auth_data: b64u(r.authenticatorData),
    signature: b64u(r.signature),
  });
  // The daemon now holds the new public key; keep its private half here.
  await self.PilothouseKeys.save(rec);
  return me;
}

/**
 * A button that runs a passkey ceremony on tap. prepare() runs ahead of the
 * tap; run(prep) does the ceremony; onDone(me) gets the updated /api/me.
 */
function ceremonyButton({ label, busy, cls, prep, run, onDone }) {
  const btn = h('button', { class: cls, type: 'button' }, label);
  prep.get();
  btn.addEventListener('click', async () => {
    btn.disabled = true;
    fill(btn, h('span', { class: 'spin', 'aria-hidden': 'true' }), busy);
    try {
      const me = await run(await prep.get());
      prep.reset();
      store.me = me;
      changed('data');
      onDone(me);
    } catch (e) {
      prep.reset();
      toast(messageFor(e), 'bad');
      prep.get();
    }
    btn.disabled = false;
    fill(btn, label);
  });
  return btn;
}

const addPrep = prepared(() => api('POST', '/api/passkeys/challenge', { purpose: 'register' }));
const restorePrep = prepared(async () => {
  const rec = await self.PilothouseKeys.generate();
  const c = await api('POST', '/api/passkeys/challenge', { purpose: 'restore', public_key: rec.spki });
  return { rec, c };
});

/** "Add passkey": registers one, signed by this device's key. */
export function addPasskeyButton(cls, onDone = () => {}) {
  return ceremonyButton({
    label: 'Add passkey', busy: 'Adding…', cls, prep: addPrep, run: add,
    onDone: (me) => { toast('Passkey added — it can restore this device’s key', 'ok'); onDone(me); },
  });
}

/** "Restore": replaces a lost signing key, vouched for by the passkey. */
export function restoreKeyButton(cls, onDone = () => {}) {
  return ceremonyButton({
    label: 'Restore', busy: 'Restoring…', cls, prep: restorePrep, run: restore,
    onDone: (me) => {
      store.keyMissing = false;
      refreshChrome();
      toast('Signing key restored', 'ok');
      onDone(me);
    },
  });
}
