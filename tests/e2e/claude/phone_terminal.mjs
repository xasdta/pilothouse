// Simulated phone driving the live terminal API (Node 20+ WebCrypto).
//   node phone_terminal.mjs <state.json> <session-id> <text-to-type> [other-session-id]
// <state.json> comes from `node phone.mjs pair`. Mints a terminal token with
// a signed "terminal" action, streams the terminal (replay=1), types the
// text and Enter through the input route, sends a resize, and checks that
// tokens are refused without a token and for another session. Prints one
// JSON line with what happened.
import { readFileSync } from 'node:fs';
const { subtle } = globalThis.crypto;
const b64url = (b) => Buffer.from(b).toString('base64url');
const [, , stateFile, sid, text, otherSid = 's_nothere000'] = process.argv;
const st = JSON.parse(readFileSync(stateFile, 'utf8'));
const hdr = { 'Content-Type': 'application/json', Origin: st.base, 'X-Pilothouse': '1', Cookie: st.cookie };
const key = await subtle.importKey('jwk', st.jwk, { name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign']);
const out = { session: sid };

async function signAction(action, target, payload) {
  const text = JSON.stringify(payload);
  const h = b64url(await subtle.digest('SHA-256', new TextEncoder().encode(text)));
  const ts = Date.now(), nonce = b64url(crypto.getRandomValues(new Uint8Array(18)));
  const msg = ['pilothouse-action:v2', action, target, h, String(ts), nonce].join('\n');
  const sig = b64url(await subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, key, new TextEncoder().encode(msg)));
  return { payload: text, ts, nonce, sig };
}
const input = (id, token, body) => fetch(`${st.base}/api/sessions/${id}/terminal/input`, {
  method: 'POST', headers: { ...hdr, 'X-Terminal-Token': token }, body: JSON.stringify(body) });
const data = (s) => ({ data: Buffer.from(s).toString('base64') });

// No token: refused.
out.no_token = (await input(sid, '', data('x'))).status;

// Mint.
const m = await fetch(`${st.base}/api/sessions/${sid}/terminal/token`, {
  method: 'POST', headers: hdr, body: JSON.stringify(await signAction('terminal', sid, {})) });
const minted = await m.json();
out.mint = m.status;
out.expires_in_s = Math.round((Date.parse(minted.expires_at) - Date.now()) / 1000);
const token = minted.token;
out.other_session = (await input(otherSid, token, data('x'))).status;

// Stream.
const ac = new AbortController();
const resp = await fetch(`${st.base}/api/sessions/${sid}/terminal?replay=1`, { headers: { Cookie: st.cookie }, signal: ac.signal });
out.stream = resp.status;
let screen = Buffer.alloc(0);
const events = {};
let exitCode = null;
(async () => {
  const dec = new TextDecoder();
  let buf = '';
  try {
    for await (const chunk of resp.body) {
      buf += dec.decode(chunk, { stream: true });
      let i;
      while ((i = buf.indexOf('\n\n')) >= 0) {
        const block = buf.slice(0, i); buf = buf.slice(i + 2);
        let ev = 'message', d = '';
        for (const line of block.split('\n')) {
          if (line.startsWith('event: ')) ev = line.slice(7);
          else if (line.startsWith('data: ')) d += line.slice(6);
        }
        events[ev] = (events[ev] || 0) + 1;
        if (ev === 'data') screen = Buffer.concat([screen, Buffer.from(d, 'base64')]);
        if (ev === 'size') out.size = JSON.parse(d);
        if (ev === 'exit') exitCode = JSON.parse(d).code;
      }
    }
  } catch { /* aborted */ }
})();
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const plain = () => screen.toString('utf8').replace(/\x1b\[[0-9;?<>=]*[ -\/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b./g, '');
for (let i = 0; i < 50 && screen.length === 0; i++) await sleep(100);
out.replay_bytes = screen.length;
out.replay_shows_claude = /Claude Code|❯/.test(plain());

// Type the prompt, then Enter on its own (like a person).
out.type = (await input(sid, token, data(text))).status;
await sleep(400);
out.enter = (await input(sid, token, data('\r'))).status;
out.resize = (await input(sid, token, { resize: { cols: 100, rows: 30 } })).status;
const before = screen.length;
for (let i = 0; i < 100 && screen.length < before + 200; i++) await sleep(100);
out.live_bytes = screen.length - before;
out.echoed = plain().includes(text.slice(0, 20));
await sleep(1000);
ac.abort();
out.events = events;
out.exit = exitCode;
console.log(JSON.stringify(out));
process.exit(0);
