// Simulated paired phone for the Linux e2e suite (Node >= 20, no deps).
//
//   node phone.mjs pair   <base> <state.json> <code>
//       pairs with a one-time code (POST /api/pair) and saves the device
//       cookie plus its P-256 key (exported here only so later invocations
//       can reuse it; the real PWA keeps it non-extractable).
//   node phone.mjs decide <base> <state.json> <behavior> [scope] [message] [timeout_s]
//       waits for an open request routed to devices, answers it with a
//       signed decision, then probes the rejections: replay of the same
//       signed body, a bad signature, a stale timestamp, and a second fresh
//       decision on the closed request. Prints one JSON object.
//   node phone.mjs probe  <base> <state.json>
//       unauthenticated / CSRF probes. Prints one JSON object.
//
// Signed message (internal/web/decide.go, CanonicalDecision):
//   pilothouse-decide:v1\n{id}\n{behavior}\n{scope}\n{ts}\n{nonce}
// ECDSA P-256 SHA-256, P1363 signature, base64url.
import { readFileSync, writeFileSync } from 'node:fs';

const { subtle } = globalThis.crypto;
const b64 = (b) => Buffer.from(b).toString('base64url');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const [, , cmd, base, statePath, ...rest] = process.argv;
const hdr = { 'Content-Type': 'application/json', Origin: base, 'X-Pilothouse': '1' };

async function body(res) {
  const t = await res.text();
  try { return JSON.parse(t); } catch { return t; }
}

async function pair(code) {
  const keys = await subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, true, ['sign', 'verify']);
  const spki = b64(await subtle.exportKey('spki', keys.publicKey));
  const res = await fetch(`${base}/api/pair`, {
    method: 'POST', headers: hdr,
    body: JSON.stringify({ code, name: 'e2e phone', public_key: spki }),
  });
  const b = await body(res);
  const out = { pair_status: res.status, body: b };
  if (res.status === 200) {
    const cookie = (res.headers.get('set-cookie') || '').split(';')[0];
    const jwk = await subtle.exportKey('jwk', keys.privateKey);
    writeFileSync(statePath, JSON.stringify({ cookie, jwk, device: b }), { mode: 0o600 });
    out.cookie_set = cookie.startsWith('ph_dev=');
    // a used code must not pair a second device
    const again = await fetch(`${base}/api/pair`, {
      method: 'POST', headers: hdr,
      body: JSON.stringify({ code, name: 'e2e phone 2', public_key: spki }),
    });
    out.reuse_status = again.status;
  }
  console.log(JSON.stringify(out));
  process.exit(res.status === 200 ? 0 : 1);
}

async function load() {
  const st = JSON.parse(readFileSync(statePath, 'utf8'));
  const key = await subtle.importKey('jwk', st.jwk, { name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign']);
  return { ...st, key, auth: { ...hdr, Cookie: st.cookie } };
}

async function sign(key, id, behavior, scope, ts, nonce) {
  const msg = ['pilothouse-decide:v1', id, behavior, scope, String(ts), nonce].join('\n');
  return b64(await subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, key, new TextEncoder().encode(msg)));
}

const newNonce = () => b64(crypto.getRandomValues(new Uint8Array(16)));

async function post(st, id, payload) {
  const res = await fetch(`${base}/api/requests/${id}/decide`, {
    method: 'POST', headers: st.auth, body: JSON.stringify(payload),
  });
  const b = await body(res);
  return { status: res.status, code: typeof b === 'object' ? b.code : undefined, body: b };
}

async function decide(behavior, scope = 'once', message = '', timeoutS = '30') {
  const st = await load();
  const deadline = Date.now() + Number(timeoutS) * 1000;
  let req;
  while (!req && Date.now() < deadline) {
    const res = await fetch(`${base}/api/snapshot`, { headers: { Cookie: st.cookie } });
    if (res.status === 200) {
      const snap = await res.json();
      req = (snap.requests || []).find((r) => r.status === 'open' && r.route === 'devices');
    }
    if (!req) await sleep(150);
  }
  if (!req) {
    console.log(JSON.stringify({ error: 'no open request routed to devices' }));
    process.exit(1);
  }
  const out = { request: { id: req.id, agent: req.agent, tool: req.tool, summary: req.summary, route: req.route } };

  // 1. bad signature (signed for the other behavior) must be refused
  {
    const ts = Date.now(), nonce = newNonce();
    const other = behavior === 'allow' ? 'deny' : 'allow';
    const sig = await sign(st.key, req.id, other, scope, ts, nonce);
    const r = await post(st, req.id, { behavior, scope, message, ts, nonce, sig });
    out.badsig = { status: r.status, code: r.code };
  }
  // 2. stale timestamp (10 minutes old) must be refused
  {
    const ts = Date.now() - 600_000, nonce = newNonce();
    const sig = await sign(st.key, req.id, behavior, scope, ts, nonce);
    const r = await post(st, req.id, { behavior, scope, message, ts, nonce, sig });
    out.stale = { status: r.status, code: r.code };
  }
  // 3. the real decision
  const ts = Date.now(), nonce = newNonce();
  const sig = await sign(st.key, req.id, behavior, scope, ts, nonce);
  const payload = { behavior, scope, message, ts, nonce, sig };
  const d = await post(st, req.id, payload);
  out.decide = { status: d.status, code: d.code, req_status: d.body && d.body.status, by: d.body && d.body.decision && d.body.decision.by };
  // 4. replay of the exact signed body
  const r2 = await post(st, req.id, payload);
  out.replay = { status: r2.status, code: r2.code };
  // 5. a fresh, validly signed second answer loses (first answer wins)
  {
    const ts2 = Date.now(), n2 = newNonce();
    const sig2 = await sign(st.key, req.id, behavior, scope, ts2, n2);
    const r = await post(st, req.id, { behavior, scope, message, ts: ts2, nonce: n2, sig: sig2 });
    out.second = { status: r.status, code: r.code };
  }
  console.log(JSON.stringify(out));
  process.exit(d.status === 200 ? 0 : 1);
}

async function probe() {
  const st = await load();
  const out = {};
  out.snapshot_nocookie = (await fetch(`${base}/api/snapshot`)).status;
  out.snapshot_cookie = (await fetch(`${base}/api/snapshot`, { headers: { Cookie: st.cookie } })).status;
  // state-changing call without the CSRF header
  const noCsrf = await fetch(`${base}/api/requests/r_aaaaaaaaaa/decide`, {
    method: 'POST', headers: { 'Content-Type': 'application/json', Cookie: st.cookie, Origin: base },
    body: '{}',
  });
  out.decide_nocsrf = noCsrf.status;
  const page = await fetch(`${base}/`);
  out.index_status = page.status;
  out.csp = page.headers.get('content-security-policy') || '';
  out.xfo = page.headers.get('x-frame-options') || '';
  console.log(JSON.stringify(out));
}

switch (cmd) {
  case 'pair': await pair(rest[0]); break;
  case 'decide': await decide(...rest); break;
  case 'probe': await probe(); break;
  default:
    console.error('usage: phone.mjs pair|decide|probe <base> <state.json> …');
    process.exit(2);
}
