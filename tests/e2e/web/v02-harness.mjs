// Harness for the 0.2 view checks (20-24-*.mjs): runs the web devserver
// (go run -tags devserver: real static files, pairing, cookies and device
// keys, a fake 0.1 core), drives headless Chrome with a throwaway profile,
// and answers the 0.2 API with fixtures through request interception, so
// the views can be tested before the daemon implements those calls.
// SSE endpoints are routed to a local mock server that keeps streams open.
//
// Nothing here touches ~/.pilothouse, a running daemon or your own Chrome
// profile. Temp files go to $PH_TMP (default: a new dir under the OS temp
// dir) and are removed at exit unless PH_KEEP=1.
import puppeteer from 'puppeteer-core';
import { spawn, execFileSync } from 'node:child_process';
import crypto from 'node:crypto';
import fs from 'node:fs';
import http from 'node:http';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';

export const ROOT = new URL('../../../', import.meta.url).pathname;
export const CHROME = process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const BASE_TMP = process.env.PH_TMP || os.tmpdir();
fs.mkdirSync(BASE_TMP, { recursive: true });
export const TMP = fs.mkdtempSync(path.join(BASE_TMP, 'ph-e2e-i-'));
export const SHOTS = process.env.PH_SHOTS || path.join(BASE_TMP, 'ph-e2e-i-shots');
fs.mkdirSync(SHOTS, { recursive: true });

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// ---------- Results ----------

const results = [];
export function check(name, ok, detail = '') {
  results.push({ name, ok: !!ok, detail });
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? '  — ' + detail : ''}`);
  return !!ok;
}
export function summary() {
  const f = results.filter((r) => !r.ok);
  console.log(`\n${results.length - f.length}/${results.length} passed`);
  return f.length;
}

// ---------- Processes ----------

const cleanups = [];
export async function cleanup() {
  for (const fn of cleanups.reverse()) { try { await fn(); } catch { /* best effort */ } }
  cleanups.length = 0;
  // Screenshots live outside TMP and are kept.
  if (process.env.PH_KEEP !== '1') fs.rmSync(TMP, { recursive: true, force: true });
}

/** Runs a test body with cleanup and an exit code (failed checks). */
export async function run(body) {
  let code = 1;
  try {
    await body();
    code = summary();
  } catch (e) {
    console.error(e);
    summary();
  } finally {
    await cleanup();
  }
  process.exit(code);
}

function freePort() {
  return new Promise((resolve, reject) => {
    const s = net.createServer();
    s.listen(0, '127.0.0.1', () => { const { port } = s.address(); s.close(() => resolve(port)); });
    s.on('error', reject);
  });
}

let built = '';
/** Builds and starts the devserver; resolves to {base, login, pair}. */
export async function startDevserver() {
  if (!built) {
    built = path.join(TMP, 'devserver');
    execFileSync('go', ['build', '-tags', 'devserver', '-o', built, './internal/web/devserver'], { cwd: ROOT, stdio: 'inherit' });
  }
  const port = await freePort();
  const home = fs.mkdtempSync(path.join(TMP, 'dev-'));
  const proc = spawn(built, ['-addr', `127.0.0.1:${port}`], {
    stdio: ['ignore', 'pipe', 'pipe'],
    env: { ...process.env, TMPDIR: home, PILOTHOUSE_HOME: home, PILOTHOUSE_USER_HOME: home, PILOTHOUSE_RUN_DIR: home },
  });
  cleanups.push(() => new Promise((r) => { if (proc.exitCode !== null) return r(); proc.once('exit', r); proc.kill('SIGINT'); setTimeout(() => { proc.kill('SIGKILL'); r(); }, 3000); }));
  let out = '';
  const info = await new Promise((resolve, reject) => {
    const t = setTimeout(() => reject(new Error('devserver did not start: ' + out)), 20000);
    proc.stdout.on('data', (b) => {
      out += b;
      const login = out.match(/login: (\S+)/);
      const pair = out.match(/pair: {2}(\S+)/);
      if (login && pair) { clearTimeout(t); resolve({ login: login[1], pair: pair[1] }); }
    });
    proc.stderr.on('data', (b) => { if (process.env.PH_DEBUG) process.stderr.write(b); });
    proc.on('exit', (c) => reject(new Error('devserver exited ' + c + ': ' + out)));
  });
  return { base: `http://127.0.0.1:${port}`, ...info, proc };
}

export async function launch() {
  const userDataDir = fs.mkdtempSync(path.join(TMP, 'chrome-'));
  const browser = await puppeteer.launch({
    executablePath: CHROME,
    headless: 'new',
    userDataDir,
    args: ['--no-first-run', '--no-default-browser-check', '--disable-features=Translate', '--hide-scrollbars'],
    defaultViewport: null,
  });
  cleanups.push(() => browser.close());
  return browser;
}

export const PHONE = { width: 390, height: 844, isMobile: true, hasTouch: true, deviceScaleFactor: 2 };
export const DESKTOP = { width: 1440, height: 900, deviceScaleFactor: 1 };

/** Console errors, page errors and CSP violations seen by a page. */
export function watch(page) {
  const logs = [];
  page.on('console', (m) => { if (m.type() === 'error' || m.type() === 'warn') logs.push(`console.${m.type()}: ${m.text()}`); });
  page.on('pageerror', (e) => logs.push('pageerror: ' + e.message));
  page._logs = logs;
  return logs;
}

/** Signs this browser in with the devserver's one-time login link. */
export async function login(page, dev) {
  await page.evaluateOnNewDocument(() => {
    window.__csp = [];
    document.addEventListener('securitypolicyviolation', (e) => window.__csp.push(`${e.violatedDirective} ${e.blockedURI || ''} ${e.sample || ''}`));
  });
  await page.goto(dev.login, { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => location.pathname === '/' && document.querySelector('main .view .page-title'), { timeout: 15000 });
}

// ---------- Mock SSE ----------

/**
 * A local SSE server. Interception reroutes the app's streams here.
 * push(channel, event, data) writes to every open stream on a channel;
 * onConnect(channel, fn(res, url)) runs when one opens (fn may end it).
 */
export async function sseServer() {
  const open = new Map(); // channel → Set<res>
  const hooks = new Map();
  const fails = new Map();
  const log = [];
  const srv = http.createServer((req, res) => {
    const u = new URL(req.url, 'http://x');
    const ch = u.pathname;
    log.push({ ch, search: u.search, at: Date.now() });
    const hook = hooks.get(ch);
    const failing = fails.get(ch);
    if (failing) {
      res.writeHead(failing.status, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify(failing.body || {}));
      return;
    }
    res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store' });
    res.write('retry: 3000\n\n');
    if (!open.has(ch)) open.set(ch, new Set());
    open.get(ch).add(res);
    req.on('close', () => open.get(ch).delete(res));
    if (hook && hook.fn) hook.fn(res, u);
  });
  await new Promise((r) => srv.listen(0, '127.0.0.1', r));
  cleanups.push(() => new Promise((r) => { for (const set of open.values()) for (const res of set) res.destroy(); srv.close(() => r()); }));
  const write = (res, event, data) => res.write(`event: ${event}\ndata: ${typeof data === 'string' ? data : JSON.stringify(data)}\n\n`);
  return {
    port: srv.address().port,
    log,
    write,
    connections: (ch) => log.filter((l) => l.ch === ch),
    open: (ch) => (open.get(ch) ? open.get(ch).size : 0),
    push(ch, event, data) { for (const res of open.get(ch) || []) write(res, event, data); },
    end(ch) { for (const res of open.get(ch) || []) res.end(); },
    onConnect(ch, fn) { hooks.set(ch, { fn }); },
    /** Answers the channel with an HTTP error until reset(). */
    fail(ch, status, body) { fails.set(ch, { status, body }); },
    reset(ch) { fails.delete(ch); },
  };
}

// ---------- API interception ----------

/**
 * Answers /api/* calls from fixtures. on(method, regex, fn) where fn gets
 * {url, m, body, headers} and returns {json, status} | {sse: '/path'} |
 * 'pass' (to the devserver). Unmatched calls pass through. Every matched
 * call is recorded in calls.
 */
export async function router(page, sse) {
  const routes = [];
  const calls = [];
  await page.setRequestInterception(true);
  page.on('request', (req) => {
    if (req.isInterceptResolutionHandled()) return;
    let u;
    try { u = new URL(req.url()); } catch { return req.continue(); }
    if (!u.pathname.startsWith('/api/')) return req.continue();
    for (const r of routes) {
      if (r.method !== req.method()) continue;
      const m = u.pathname.match(r.re);
      if (!m) continue;
      const call = { method: req.method(), path: u.pathname, query: Object.fromEntries(u.searchParams), body: req.postData() || '', headers: req.headers(), at: Date.now() };
      calls.push(call);
      let out;
      try { out = r.fn({ url: u, m, body: call.body, headers: call.headers, call }); } catch (e) { out = { status: 500, json: { error: String(e) } }; }
      if (out === 'pass') return req.continue();
      if (out && out.sse) return req.continue({ url: `http://127.0.0.1:${sse.port}${out.sse}${u.search}` });
      if (out && out.abort) return req.abort('connectionreset');
      if (out && out.status === 204) return req.respond({ status: 204, body: '' });
      return req.respond({ status: (out && out.status) || 200, contentType: 'application/json', body: JSON.stringify((out && out.json) ?? {}) });
    }
    req.continue();
  });
  return {
    calls,
    on(method, re, fn) { routes.unshift({ method, re, fn }); },
    find: (method, re) => calls.filter((c) => c.method === method && re.test(c.path)),
    clear() { calls.length = 0; },
  };
}

// ---------- Signatures ----------

export async function deviceKey(page) {
  return page.evaluate(async () => (await self.PilothouseKeys.load()).spki);
}

/** Verifies a signed action body the way internal/web/signed.go does. */
export function verifyAction(spki, action, target, bodyText) {
  let b;
  try { b = JSON.parse(bodyText); } catch { return { ok: false, why: 'body is not JSON' }; }
  const keys = Object.keys(b).sort().join(',');
  if (keys !== 'nonce,payload,sig,ts') return { ok: false, why: 'body keys ' + keys };
  const hash = crypto.createHash('sha256').update(b.payload).digest('base64url');
  const canonical = ['pilothouse-action:v2', action, target, hash, String(b.ts), b.nonce].join('\n');
  const key = crypto.createPublicKey({ key: Buffer.from(spki, 'base64url'), format: 'der', type: 'spki' });
  const ok = crypto.verify('sha256', Buffer.from(canonical), { key, dsaEncoding: 'ieee-p1363' }, Buffer.from(b.sig, 'base64url'));
  let payload = null;
  try { payload = JSON.parse(b.payload); } catch { /* reported by ok=false below */ }
  const fresh = Math.abs(Date.now() - b.ts) < 120000 && /^[A-Za-z0-9_-]{16,64}$/.test(b.nonce);
  return { ok: ok && fresh && payload !== null, payload, why: ok ? (fresh ? '' : 'stale ts or bad nonce') : 'bad signature' };
}

// ---------- Pages ----------

export async function shot(page, name) {
  const p = path.join(SHOTS, name + '.png');
  await page.screenshot({ path: p, fullPage: true, captureBeyondViewport: true });
  return p;
}

export async function text(page, sel = 'main') {
  return page.$eval(sel, (e) => e.innerText).catch(() => '');
}

export async function waitText(page, needle, timeout = 8000, sel = 'main') {
  return page.waitForFunction(
    (s, n) => { const e = document.querySelector(s); return e && e.innerText.includes(n); },
    { timeout, polling: 100 }, sel, needle,
  ).then(() => true, () => false);
}

/**
 * Sets the viewport. Changing isMobile/hasTouch makes puppeteer reload the
 * page, so wait for the app to be back before going on.
 */
export async function setSize(page, vp) {
  const before = page.viewport() || {};
  const reloads = !!before.isMobile !== !!vp.isMobile || !!before.hasTouch !== !!vp.hasTouch;
  const nav = reloads ? page.waitForNavigation({ waitUntil: 'domcontentloaded', timeout: 10000 }).catch(() => {}) : null;
  await page.setViewport(vp);
  if (nav) await nav;
  await page.waitForSelector('main .view', { timeout: 10000 }).catch(() => {});
}

/**
 * Clicks an element after centring it: a plain click can land on the fixed
 * top or tab bar when the element is scrolled just under it.
 */
/** Waits until the screen has stopped moving (view slide-ins), as a finger would. */
export async function settled(page) {
  await page.waitForFunction(() => !document.getAnimations().some((a) => a.playState === 'running' && a.effect && a.effect.target && a.effect.target.classList && a.effect.target.classList.contains('view')), { timeout: 2000 }).catch(() => {});
}

export async function tap(page, sel) {
  await settled(page);
  await new Promise((r) => setTimeout(r, 120));
  for (let i = 0; ; i++) {
    const el = typeof sel === 'string' ? await page.waitForSelector(sel, { timeout: 5000 }) : sel;
    try {
      await el.evaluate((e) => e.scrollIntoView({ block: 'center', behavior: 'instant' }));
      await el.click();
      return;
    } catch (e) {
      // A view re-rendered between finding and clicking: find it again.
      if (typeof sel !== 'string' || i >= 2 || !/detached|not attached/i.test(e.message)) throw e;
    }
  }
}

/** Client-side navigation through the app's own router (keeps the store). */
export async function go(page, p) {
  await page.evaluate((to) => {
    history.pushState({ inApp: true }, '', to);
    dispatchEvent(new PopStateEvent('popstate', { state: { inApp: true } }));
  }, p);
  await page.waitForFunction((to) => location.pathname === to, {}, p);
}

const axeSrc = fs.readFileSync(new URL('./node_modules/axe-core/axe.min.js', import.meta.url), 'utf8');
/** axe-core violations on the current page (CSP bypassed for axe's script only). */
export async function axe(page) {
  const url = page.url();
  await page.setBypassCSP(true);
  await page.goto(url, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('main .view', { timeout: 8000 }).catch(() => {});
  await sleep(1200);
  await page.addScriptTag({ content: axeSrc });
  const res = await page.evaluate(async () => {
    const r = await window.axe.run(document, { resultTypes: ['violations'] });
    return r.violations.map((v) => ({ id: v.id, impact: v.impact, n: v.nodes.length, ex: v.nodes.slice(0, 3).map((n) => n.target.join(' ') + ' :: ' + (n.failureSummary || '').split('\n').slice(1, 3).join(' ')) }));
  });
  await page.setBypassCSP(false);
  return res;
}

/** No horizontal page scroll and no literal "null"/"undefined" in the view. */
export async function sanity(page, label) {
  const h = await page.evaluate(() => document.documentElement.scrollWidth - innerWidth);
  check(`${label}: no horizontal scroll`, h <= 1, `overflow ${h}px`);
  const t = await text(page);
  const bad = t.match(/.{0,30}\b(null|undefined|NaN|\[object Object\])\b.{0,20}/g);
  check(`${label}: no null/undefined/NaN in text`, !bad, bad ? bad.join(' | ') : '');
}
