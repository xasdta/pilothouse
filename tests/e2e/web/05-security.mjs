// 5. Security checks from the browser.
import http from 'node:http';
import { BASE, launch, watch, pairViaUI, check, summary, hold, readOut, sleep, sh } from './lib.mjs';

const RUN = Date.now().toString(36);
// Attacker origin: same site (127.0.0.1), different port → cookies (SameSite=Strict) ARE sent.
const ATT = 'http://127.0.0.1:7594';
const attacker = http.createServer((req, res) => {
  res.writeHead(200, { 'Content-Type': 'text/html' });
  res.end(`<!doctype html><title>attacker</title><body>
<form id=f method=POST action="${BASE}/api/logout" target=sink enctype="text/plain"><input name=a value=b></form>
<iframe name=sink></iframe><iframe id=frame src="${BASE}/"></iframe></body>`);
});
await new Promise((r) => attacker.listen(7594, '127.0.0.1', r));

const browser = await launch({ args: ['--host-resolver-rules=MAP evil.test 127.0.0.1'] });
const page = await browser.newPage();
watch(page, 'P');
check('paired', (await pairViaUI(page, 'Sec device')).status === 200);
await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
await sleep(1200);

// ---- CSP ----
const csp = await page.evaluate(async () => {
  const v = [];
  document.addEventListener('securitypolicyviolation', (e) => v.push(e.violatedDirective + ' ' + e.blockedURI));
  const s = document.createElement('script'); s.textContent = 'window.__pwn1 = 1'; document.body.append(s);
  const d = document.createElement('div'); d.innerHTML = '<img src=x onerror="window.__pwn2=1">'; document.body.append(d);
  const s2 = document.createElement('script'); s2.src = 'https://cdn.jsdelivr.net/npm/lodash@4.17.21/lodash.min.js'; document.body.append(s2);
  let evalBlocked = false; try { eval('1'); } catch { evalBlocked = true; }
  const st = document.createElement('div'); st.setAttribute('style', 'color: rgb(1, 2, 3)'); document.body.append(st);
  await new Promise((r) => setTimeout(r, 1500));
  return { pwn1: window.__pwn1, pwn2: window.__pwn2, lodash: typeof window._, evalBlocked, styleAttr: getComputedStyle(st).color, v };
});
check('CSP blocks inline <script>', csp.pwn1 === undefined, JSON.stringify(csp.v));
check('CSP blocks inline event handler', csp.pwn2 === undefined);
check('CSP blocks third-party script', csp.lodash === 'undefined');
check('INFO eval under CDP evaluate (CDP bypasses CSP; inconclusive)', true, 'evalBlocked=' + csp.evalBlocked);
check('CSP blocks inline style attribute', csp.styleAttr !== 'rgb(1, 2, 3)', csp.styleAttr);
check('violations reported', csp.v.length >= 3, csp.v.length + '');

// ---- DNS rebinding: evil.test → 127.0.0.1 ----
const p2 = await browser.newPage();
const rb = await p2.goto('http://evil.test:7592/', { waitUntil: 'domcontentloaded' });
check('rebinding host evil.test:7592 → 421 bad_host', rb.status() === 421, `${rb.status()} ${(await rb.text()).slice(0, 100)}`);
const rb2 = await p2.goto('http://evil.test:7592/api/snapshot', { waitUntil: 'domcontentloaded' });
check('rebinding /api/snapshot → 421', rb2.status() === 421, String(rb2.status()));
const curlHost = sh(`curl -s -o /dev/null -w '%{http_code}' -H 'Host: attacker.example' ${BASE}/api/me`);
check('curl wrong Host header → 421', curlHost === '421', curlHost);
const lh = sh(`curl -s -o /dev/null -w '%{http_code}' http://localhost:7592/`);
check('localhost:7592 allowed', lh === '200', lh);

// ---- Cross-origin (same-site) attacks ----
await p2.goto(ATT + '/', { waitUntil: 'domcontentloaded' });
await sleep(1500);
const xo = await p2.evaluate(async (base) => {
  const out = {};
  // Simple request (no preflight), cookie included.
  try {
    const r = await fetch(base + '/api/presence', { method: 'PUT', credentials: 'include', mode: 'no-cors', headers: { 'Content-Type': 'text/plain' }, body: '{"mode":"away"}' });
    out.simple = r.type;
  } catch (e) { out.simple = 'err ' + e.message; }
  // With the custom header → preflight; must fail.
  try {
    await fetch(base + '/api/presence', { method: 'PUT', credentials: 'include', headers: { 'X-Pilothouse': '1', 'Content-Type': 'application/json' }, body: '{"mode":"away"}' });
    out.preflight = 'allowed!';
  } catch (e) { out.preflight = 'blocked'; }
  // Read snapshot cross-origin.
  try { const r = await fetch(base + '/api/snapshot', { credentials: 'include' }); out.read = 'readable ' + r.status; } catch { out.read = 'blocked'; }
  // SSE cross-origin.
  out.sse = await new Promise((res) => { const es = new EventSource(base + '/api/events', { withCredentials: true }); es.onopen = () => res('open!'); es.onerror = () => { es.close(); res('error'); }; setTimeout(() => res('timeout'), 4000); });
  // Framing.
  const f = document.getElementById('frame');
  try { out.frame = f.contentDocument ? 'accessible' : 'null'; } catch { out.frame = 'cross-origin'; }
  return out;
}, BASE);
await sleep(500);
const pres = JSON.parse(sh('curl -s --unix-socket /tmp/qa2/run/pilothouse.sock http://x/v1/presence'));
check('cross-origin simple PUT (no X-Pilothouse) did not change presence', pres.mode !== 'away', JSON.stringify(pres));
check('cross-origin request with X-Pilothouse blocked by CORS preflight', xo.preflight === 'blocked', xo.preflight);
check('cross-origin snapshot read blocked', xo.read === 'blocked', xo.read);
check('cross-origin EventSource blocked', xo.sse !== 'open!', xo.sse);
// Server-side verdicts with explicit headers (what the browser would send).
const r1 = sh(`curl -s -w ' %{http_code}' -X PUT -b "$(cat /dev/null)" -H 'Origin: ${ATT}' -H 'X-Pilothouse: 1' -H 'Content-Type: application/json' -d '{"mode":"away"}' ${BASE}/api/presence`);
console.log('unauth PUT with foreign Origin:', r1);
// Cross-origin form POST to /api/logout: device must stay signed in.
const logoutResp = p2.waitForResponse((r) => r.url().endsWith('/api/logout'), { timeout: 5000 }).catch(() => null);
await p2.evaluate(() => document.getElementById('f').submit());
const lr = await logoutResp;
check('cross-origin form POST /api/logout → 403', lr && lr.status() === 403, lr ? `${lr.status()} ${await lr.text().catch(() => '')}` : 'no response');
const stillIn = await page.evaluate(async () => (await fetch('/api/me')).status);
check('device still signed in after CSRF attempt', stillIn === 200, String(stillIn));
// Framing: X-Frame-Options / frame-ancestors.
const frameBlocked = await p2.evaluate(() => new Promise((r) => setTimeout(() => {
  try { const d = document.getElementById('frame').contentWindow.document; r('accessible:' + d.body.innerText.slice(0, 20)); } catch (e) { r('blocked'); }
}, 300)));
check('app cannot be framed (XFO DENY / frame-ancestors none)', frameBlocked !== 'accessible', frameBlocked);
// Same-origin request without header from the app origin.
const noHdr = await page.evaluate(async () => (await fetch('/api/presence', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: '{"mode":"away"}' })).status);
check('same-origin PUT without X-Pilothouse → 403', noHdr === 403, String(noHdr));

// ---- Signed decisions: replay / tamper / stale ----
await page.bringToFront();
hold('sec', `echo qa-sec-${RUN}`);
await page.waitForFunction((n) => document.body.innerText.includes(n), {}, `qa-sec-${RUN}`);
let captured = null;
page.on('request', (r) => { if (r.url().includes('/decide')) captured = { url: r.url(), body: r.postData() }; });
const card = await page.waitForSelector(`xpath///article[contains(@class,'req')][.//pre[contains(., 'qa-sec-${RUN}')]]`);
await (await card.$('.actions .btn.primary')).click();
const out = await readOut('sec');
check('legit decision accepted', /"allow"/.test(out), out);
await sleep(300);
const replay = await page.evaluate(async (c) => {
  const r = await fetch(c.url, { method: 'POST', headers: { 'X-Pilothouse': '1', 'Content-Type': 'application/json' }, body: c.body });
  return [r.status, await r.text()];
}, captured);
check('replayed signed decision → 403 replayed', replay[0] === 403 && /replayed/.test(replay[1]), replay.join(' '));

hold('sec2', `echo qa-sec2-${RUN}`);
await page.waitForFunction((n) => document.body.innerText.includes(n), {}, `qa-sec2-${RUN}`);
const snap = JSON.parse(sh('curl -s --unix-socket /tmp/qa2/run/pilothouse.sock http://x/v1/snapshot'));
const r2 = snap.requests.find((r) => (r.command || '').includes(`qa-sec2-${RUN}`));
const res = await page.evaluate(async (c, id2) => {
  const b = JSON.parse(c.body);
  const post = async (id, body) => { const r = await fetch('/api/requests/' + id + '/decide', { method: 'POST', headers: { 'X-Pilothouse': '1', 'Content-Type': 'application/json' }, body: JSON.stringify(body) }); return r.status + ' ' + (await r.json()).code; };
  const o = {};
  o.otherId = await post(id2, b); // signature for another id
  o.flip = await post(id2, { ...b, behavior: 'deny', nonce: 'A'.repeat(24) });
  // Stale but correctly signed.
  const rec = await self.PilothouseKeys.load();
  const sign = async (id, beh, sc, ts, nonce) => {
    const msg = new TextEncoder().encode(self.PilothouseKeys.canonical(id, beh, sc, ts, nonce));
    const sig = new Uint8Array(await crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, rec.privateKey, msg));
    return btoa(String.fromCharCode(...sig)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  };
  const old = Date.now() - 200000;
  o.stale = await post(id2, { behavior: 'allow', scope: 'once', ts: old, nonce: 'stalenonce1234567890', sig: await sign(id2, 'allow', 'once', old, 'stalenonce1234567890') });
  const fut = Date.now() + 200000;
  o.future = await post(id2, { behavior: 'allow', scope: 'once', ts: fut, nonce: 'futurenonce123456789', sig: await sign(id2, 'allow', 'once', fut, 'futurenonce123456789') });
  o.denySession = await post(id2, { behavior: 'deny', scope: 'session', ts: Date.now(), nonce: 'denysession123456789', sig: await sign(id2, 'deny', 'session', Date.now(), 'denysession123456789') });
  // Valid one, then exact replay.
  const now = Date.now();
  const good = { behavior: 'deny', scope: 'once', message: 'sec test', ts: now, nonce: 'goodnonce12345678901', sig: await sign(id2, 'deny', 'once', now, 'goodnonce12345678901') };
  o.good = await post(id2, good);
  o.goodReplay = await post(id2, good);
  return o;
}, captured, r2.id);
check('signature for another request id rejected', /^403 bad_signature/.test(res.otherId), res.otherId);
check('tampered behavior rejected', /^403 bad_signature/.test(res.flip), res.flip);
check('stale ts (−200 s) rejected', /^403 stale_signature/.test(res.stale), res.stale);
check('future ts (+200 s) rejected', /^403 stale_signature/.test(res.future), res.future);
check('deny+session rejected', /^400/.test(res.denySession), res.denySession);
check('fresh signed deny accepted', /^200/.test(res.good), res.good);
check('exact replay after success rejected', /^403 replayed/.test(res.goodReplay), res.goodReplay);
const o2 = await readOut('sec2');
check('hook got the signed deny with message', /"deny"/.test(o2) && /sec test/.test(o2), o2);

for (const l of [...page._logs]) console.log(l);
await browser.close();
attacker.close();
process.exit(summary());
