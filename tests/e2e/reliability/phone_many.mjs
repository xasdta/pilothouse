// Simulated paired phone for the soak: pairs with a one-time code, then
// answers EVERY held permission request with a signed decision, surviving
// stream resyncs (snapshot → stream after=seq → on control/end, repeat).
// Usage: node phone_many.mjs <base> <code> <outfile.jsonl> [behavior] [maxAnswers]
import { appendFileSync } from 'node:fs';
const [,, base, code, out, behavior = 'allow', maxStr = '1000000'] = process.argv;
const max = Number(maxStr);
const { subtle } = globalThis.crypto;
const b64 = (b) => Buffer.from(b).toString('base64url');
const keys = await subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign', 'verify']);
const spki = b64(await subtle.exportKey('spki', keys.publicKey));
const hdr = { 'Content-Type': 'application/json', Origin: base, 'X-Pilothouse': '1' };
const pr = await fetch(`${base}/api/pair`, { method: 'POST', headers: hdr, body: JSON.stringify({ code, name: 'soak phone', public_key: spki }) });
if (pr.status !== 200) { console.error('pair failed', pr.status, await pr.text()); process.exit(1); }
const cookie = pr.headers.get('set-cookie').split(';')[0];
const auth = { ...hdr, Cookie: cookie };
const log = (o) => appendFileSync(out, JSON.stringify({ t: Date.now(), ...o }) + '\n');
log({ ev: 'paired' });

const answered = new Set();
let resyncs = 0, answers = 0;
async function answer(req) {
  if (req.checkpoint !== 'permission' || req.status !== 'open' || req.route !== 'devices' || answered.has(req.id)) return;
  answered.add(req.id);
  const t0 = Date.now();
  const ts = Date.now(), nonce = b64(crypto.getRandomValues(new Uint8Array(16))), scope = 'once';
  const msg = ['pilothouse-decide:v1', req.id, behavior, scope, String(ts), nonce].join('\n');
  const sig = b64(await subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, keys.privateKey, new TextEncoder().encode(msg)));
  const d = await fetch(`${base}/api/requests/${req.id}/decide`, { method: 'POST', headers: auth, body: JSON.stringify({ behavior, scope, ts, nonce, sig }) });
  const body = await d.text();
  answers++;
  log({ ev: 'decide', id: req.id, status: d.status, ms: Date.now() - t0, created: req.created_at, err: d.status === 200 ? undefined : body.slice(0, 200) });
  if (answers >= max) { log({ ev: 'done', answers, resyncs }); process.exit(0); }
}

for (;;) {
  const s = await (await fetch(`${base}/api/snapshot`, { headers: { Cookie: cookie } })).json();
  for (const r of s.requests || []) answer(r).catch((e) => log({ ev: 'err', e: String(e) }));
  try {
    const res = await fetch(`${base}/api/events?after=${s.seq}`, { headers: { Cookie: cookie, Accept: 'text/event-stream' } });
    const reader = res.body.getReader();
    let buf = '', resync = false;
    while (!resync) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += Buffer.from(value).toString();
      let i;
      while ((i = buf.indexOf('\n\n')) >= 0) {
        const block = buf.slice(0, i); buf = buf.slice(i + 2);
        const evName = (block.split('\n').find((l) => l.startsWith('event:')) || '').slice(6).trim();
        const data = block.split('\n').filter((l) => l.startsWith('data:')).map((l) => l.slice(5).trim()).join('\n');
        if (!data) continue;
        if (evName === 'control') { resync = true; break; }
        const ev = JSON.parse(data);
        if (ev.type === 'request.opened') answer(ev.data).catch((e) => log({ ev: 'err', e: String(e) }));
      }
    }
    reader.cancel().catch(() => {});
  } catch (e) { log({ ev: 'stream_err', e: String(e) }); await new Promise((r) => setTimeout(r, 500)); }
  resyncs++;
  log({ ev: 'resync', resyncs });
}
