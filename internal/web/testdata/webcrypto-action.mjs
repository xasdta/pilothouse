// Generates the signed-action (v2) fixture used by web tests:
//   node internal/web/testdata/webcrypto-action.mjs > internal/web/testdata/webcrypto-action.json
// It loads the PWA's own keystore.js and calls its canonicalAction, then
// signs with exactly the WebCrypto calls signAction makes, so the Go
// verifier is tested against the shipped JavaScript.
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

const { subtle } = globalThis.crypto;
const b64url = (buf) => Buffer.from(buf).toString('base64url');

const ctx = { self: {}, TextEncoder, crypto: globalThis.crypto, btoa };
ctx.self = ctx;
vm.runInNewContext(readFileSync(new URL('../static/js/keystore.js', import.meta.url), 'utf8'), ctx);
const keys = ctx.PilothouseKeys;

const pair = await subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign', 'verify']);
const spki = await subtle.exportKey('spki', pair.publicKey);
const fixture = {
  action: 'decide',
  target: 'r_fixture002',
  payload: JSON.stringify({ behavior: 'allow', answers: { 'Which color?': 'Blue, Green' } }),
  ts: 1767225600000, // 2026-01-01T00:00:00Z
  nonce: 'bm9uY2UtYWN0aW9uLXYyLTE',
};
const hash = b64url(await subtle.digest('SHA-256', new TextEncoder().encode(fixture.payload)));
const canonical = keys.canonicalAction(fixture.action, fixture.target, hash, fixture.ts, fixture.nonce);
const sig = await subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, pair.privateKey, new TextEncoder().encode(canonical));
console.log(JSON.stringify({ ...fixture, public_key: b64url(spki), canonical, sig: b64url(sig) }, null, 2));
