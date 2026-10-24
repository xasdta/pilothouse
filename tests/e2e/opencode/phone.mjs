// Simulated paired phone speaking the 0.2 signed actions (Node 20+ WebCrypto).
//   node phone.mjs pair     <base> <code> <state.json>
//   node phone.mjs respond  <state.json> <checkpoint> <summary-substring> <body-json> [timeout-s]
//   node phone.mjs reply    <state.json> <agent-session-id> <text> [interrupt]
//   node phone.mjs snapshot <state.json>
// Signing follows internal/web/signed.go and static/js/keystore.js:
//   pilothouse-action:v2\n{action}\n{target}\n{b64url(sha256(payload))}\n{ts}\n{nonce}
// The key is extractable only so the test can keep it between calls; the
// real PWA's key never leaves IndexedDB.
import { createHash } from 'node:crypto';
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
  writeFileSync(file, JSON.stringify({ base, cookie, jwk: await subtle.exportKey('jwk', keys.privateKey) }));
  console.log('paired');
  process.exit(0);
}

const st = JSON.parse(readFileSync(a[0], 'utf8'));
const auth = { ...hdr(st.base), Cookie: st.cookie };
const snap = async () => (await fetch(`${st.base}/api/snapshot`, { headers: auth })).json();
const key = await subtle.importKey('jwk', st.jwk, { name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign']);

async function signed(action, target, payload) {
  const text = JSON.stringify(payload);
  const ts = Date.now();
  const nonce = b64(crypto.getRandomValues(new Uint8Array(18)));
  const h = createHash('sha256').update(text).digest('base64url');
  const msg = ['pilothouse-action:v2', action, target, h, String(ts), nonce].join('\n');
  const sig = b64(await subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, key, new TextEncoder().encode(msg)));
  return JSON.stringify({ payload: text, ts, nonce, sig });
}

if (cmd === 'snapshot') {
  console.log(JSON.stringify(await snap(), null, 1));
  process.exit(0);
}

if (cmd === 'respond') {
  const [, checkpoint, match, bodyJSON, timeout = '60'] = a;
  const end = Date.now() + Number(timeout) * 1000;
  while (Date.now() < end) {
    const s = await snap();
    const req = (s.requests || []).find((r) => r.status === 'open' && r.checkpoint === checkpoint && r.summary.includes(match));
    if (req) {
      const payload = JSON.parse(bodyJSON);
      // Question answers are keyed by Question.Key(): "#n" stands for the
      // n-th question's id.
      if (payload.answers) {
        const out = {};
        for (const [k, v] of Object.entries(payload.answers)) {
          const m = k.match(/^#(\d+)$/);
          const q = m && req.questions[Number(m[1]) - 1];
          out[q ? (q.id || q.question) : k] = v;
        }
        payload.answers = out;
      }
      const body = await signed('decide', req.id, payload);
      const r = await fetch(`${st.base}/api/requests/${req.id}/respond`, { method: 'POST', headers: auth, body });
      console.log('respond', req.id, JSON.stringify(req.summary), 'route=' + req.route, '->', r.status, (await r.text()).slice(0, 300));
      const again = await fetch(`${st.base}/api/requests/${req.id}/respond`, { method: 'POST', headers: auth, body });
      console.log('replay', again.status);
      process.exit(r.status === 200 && again.status === 403 ? 0 : 2);
    }
    await new Promise((r) => setTimeout(r, 300));
  }
  console.log('no open', checkpoint, 'request matching', JSON.stringify(match));
  process.exit(3);
}

if (cmd === 'reply') {
  const [, agentSession, text, interrupt] = a;
  const s = await snap();
  const sess = (s.sessions || []).find((x) => x.agent_session_id === agentSession);
  if (!sess) { console.log('no session', agentSession); process.exit(3); }
  const payload = { text };
  if (interrupt) payload.interrupt = true;
  const body = await signed('reply', sess.id, payload);
  const r = await fetch(`${st.base}/api/sessions/${sess.id}/reply`, { method: 'POST', headers: auth, body });
  console.log('reply', sess.id, 'caps=' + JSON.stringify(sess.caps), '->', r.status, (await r.text()).slice(0, 300));
  // A signed reply can't be replayed, nor retargeted at another session.
  const again = await fetch(`${st.base}/api/sessions/${sess.id}/reply`, { method: 'POST', headers: auth, body });
  console.log('replay', again.status);
  process.exit(r.status === 200 && again.status === 403 ? 0 : 2);
}

console.log('unknown command', cmd);
process.exit(64);
