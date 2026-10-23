// Simulated paired phone for e2e tests (Node 20+ WebCrypto).
//   node phone.mjs pair   <base> <code> <state.json>
//   node phone.mjs decide <state.json> <allow|deny> <once|session> [summary-substring] [timeout-s] [message]
//   node phone.mjs snapshot <state.json>
// The key is made extractable only so the test can persist it between calls;
// the real PWA keeps a non-extractable key in IndexedDB.
import { readFileSync, writeFileSync } from 'node:fs';
const { subtle } = globalThis.crypto;
const b64 = (b) => Buffer.from(b).toString('base64url');
const [, , cmd, ...a] = process.argv;
const hdr = (base) => ({ 'Content-Type': 'application/json', Origin: base, 'X-Pilothouse': '1' });

if (cmd === 'pair') {
  const [base, code, file] = a;
  const keys = await subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, true, ['sign', 'verify']);
  const spki = b64(await subtle.exportKey('spki', keys.publicKey));
  const r = await fetch(`${base}/api/pair`, { method: 'POST', headers: hdr(base), body: JSON.stringify({ code, name: 'e2e phone', public_key: spki }) });
  const body = await r.text();
  if (r.status !== 200) { console.log('pair failed', r.status, body); process.exit(1); }
  const cookie = r.headers.get('set-cookie').split(';')[0];
  writeFileSync(file, JSON.stringify({ base, cookie, jwk: await subtle.exportKey('jwk', keys.privateKey), device: JSON.parse(body) }));
  console.log('paired', body);
  process.exit(0);
}
const st = JSON.parse(readFileSync(a[0], 'utf8'));
const auth = { ...hdr(st.base), Cookie: st.cookie };
const snap = async () => (await fetch(`${st.base}/api/snapshot`, { headers: auth })).json();
if (cmd === 'snapshot') { console.log(JSON.stringify(await snap(), null, 1)); process.exit(0); }
if (cmd === 'decide') {
  const [, behavior, scope, match = '', timeout = '120', message = ''] = a;
  const key = await subtle.importKey('jwk', st.jwk, { name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign']);
  const end = Date.now() + Number(timeout) * 1000;
  while (Date.now() < end) {
    const s = await snap();
    const req = (s.requests || []).find((r) => r.status === 'open' && r.checkpoint === 'permission' && r.summary.includes(match));
    if (req) {
      const ts = Date.now(), nonce = b64(crypto.getRandomValues(new Uint8Array(16)));
      const msg = ['pilothouse-decide:v1', req.id, behavior, scope, String(ts), nonce].join('\n');
      const sig = b64(await subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, key, new TextEncoder().encode(msg)));
      const body = JSON.stringify({ behavior, scope, message, ts, nonce, sig });
      const d = await fetch(`${st.base}/api/requests/${req.id}/decide`, { method: 'POST', headers: auth, body });
      console.log('decide', req.id, req.summary, 'route=' + req.route, '->', d.status, (await d.text()).slice(0, 200));
      const r2 = await fetch(`${st.base}/api/requests/${req.id}/decide`, { method: 'POST', headers: auth, body });
      console.log('replay', r2.status);
      process.exit(d.status === 200 ? 0 : 2);
    }
    await new Promise((r) => setTimeout(r, 300));
  }
  console.log('no open request matching', match); process.exit(3);
}
