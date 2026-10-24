// Simulated phone for tasks: signed v2 actions (internal/web/signed.go),
// signed exactly like the PWA's keystore.js (canonicalAction). Uses the
// state file `phone.mjs pair` writes.
//   node phone_task.mjs repos  <state.json>
//   node phone_task.mjs start  <state.json> <repo> <agent> <prompt>
//   node phone_task.mjs tasks  <state.json>
//   node phone_task.mjs diff   <state.json> <task-id>
//   node phone_task.mjs finish <state.json> <task-id> <ship|discard>
// Each prints one JSON line. `start` also replays its signed request and
// tries an unknown repo, and reports both statuses (expect 403 and 400).
import { readFileSync } from 'node:fs';
const { subtle } = globalThis.crypto;
const b64 = (b) => Buffer.from(b).toString('base64url');
const [, , cmd, file, ...a] = process.argv;
const st = JSON.parse(readFileSync(file, 'utf8'));
const hdr = { 'Content-Type': 'application/json', Origin: st.base, 'X-Pilothouse': '1', Cookie: st.cookie };
const key = await subtle.importKey('jwk', st.jwk, { name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign']);

async function signed(action, target, payload) {
  const text = JSON.stringify(payload);
  const hash = b64(await subtle.digest('SHA-256', new TextEncoder().encode(text)));
  const ts = Date.now();
  const nonce = b64(crypto.getRandomValues(new Uint8Array(18)));
  const msg = ['pilothouse-action:v2', action, target, hash, String(ts), nonce].join('\n');
  const sig = b64(await subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, key, new TextEncoder().encode(msg)));
  return JSON.stringify({ payload: text, ts, nonce, sig });
}

async function call(method, path, body) {
  const r = await fetch(st.base + path, { method, headers: hdr, body });
  const text = await r.text();
  let json = null;
  try { json = JSON.parse(text); } catch { /* not JSON */ }
  return { status: r.status, body: json ?? text };
}

const print = (o) => console.log(JSON.stringify(o));
if (cmd === 'repos') print(await call('GET', '/api/repos'));
else if (cmd === 'tasks') print(await call('GET', '/api/tasks'));
else if (cmd === 'diff') print(await call('GET', `/api/tasks/${encodeURIComponent(a[0])}/diff`));
else if (cmd === 'start') {
  const [repo, agent, prompt] = a;
  const body = await signed('task', 'new', { repo, agent, prompt });
  const res = await call('POST', '/api/tasks', body);
  const replay = await call('POST', '/api/tasks', body);
  const unknown = await call('POST', '/api/tasks', await signed('task', 'new', { repo: '/nonexistent/repo', agent, prompt }));
  print({ ...res, replay: replay.status, replay_code: replay.body?.code, unknown: unknown.status, unknown_error: unknown.body?.error });
} else if (cmd === 'finish') {
  const [id, action] = a;
  print(await call('POST', `/api/tasks/${encodeURIComponent(id)}/finish`, await signed('task', id, { action })));
} else {
  console.error('unknown command', cmd);
  process.exit(2);
}
