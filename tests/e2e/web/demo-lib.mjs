// Helpers for the PWA checks that run against the demo core (the web
// devserver, internal/demo): no daemon, no agents, no /tmp/qa2 setup.
//
//   PH_TMP=<dir>    where the devserver home, Chrome profile and build go (default: os.tmpdir())
//   PH_SHOTS=<dir>  where screenshots go (default: <PH_TMP>/ph-shots)
//   CHROME=<path>   Chrome binary (default: /Applications/Google Chrome.app/…)
//
// Each startDemo() gets a fresh demo world on free ports; stop() kills
// exactly the processes it started and removes exactly the dirs it made.
import puppeteer from 'puppeteer-core';
import { execFileSync, spawn } from 'node:child_process';
import fs from 'node:fs';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';

const ROOT = new URL('../../../', import.meta.url).pathname;
const TMP = process.env.PH_TMP || os.tmpdir();
export const SHOTS = process.env.PH_SHOTS || path.join(TMP, 'ph-shots');
const CHROME = process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
fs.mkdirSync(SHOTS, { recursive: true });

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const results = [];
export function check(name, ok, detail = '') {
  results.push({ name, ok: !!ok, detail });
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? '  — ' + detail : ''}`);
}
export function summary() {
  const f = results.filter((r) => !r.ok);
  console.log(`\n${results.length - f.length}/${results.length} passed`);
  for (const x of f) console.log('  failed:', x.name, x.detail);
  return f.length;
}

function freePort() {
  return new Promise((resolve, reject) => {
    const s = net.createServer();
    s.listen(0, '127.0.0.1', () => {
      const { port } = s.address();
      s.close(() => resolve(port));
    });
    s.on('error', reject);
  });
}

let built = '';
function build() {
  if (built) return built;
  const dir = fs.mkdtempSync(path.join(TMP, 'ph-h-bin-'));
  process.on('exit', () => fs.rmSync(dir, { recursive: true, force: true }));
  const out = path.join(dir, 'devserver');
  execFileSync('go', ['build', '-tags', 'devserver', '-o', out, './internal/web/devserver'], { cwd: ROOT, stdio: 'inherit' });
  built = out;
  return out;
}

/** Starts a devserver on the demo core; resolves to {base, admin, stop}. */
export async function startDemo({ live = false } = {}) {
  const dir = fs.mkdtempSync(path.join(TMP, 'ph-h-e2e-'));
  const bin = build();
  const port = await freePort();
  const adminPort = await freePort();
  const args = ['-addr', `127.0.0.1:${port}`, '-admin', `127.0.0.1:${adminPort}`, '-tmp', dir, '-quiet'];
  if (live) args.push('-live');
  const proc = spawn(bin, args, { stdio: ['ignore', 'pipe', 'pipe'] });
  let out = '';
  proc.stdout.on('data', (b) => { out += b; });
  proc.stderr.on('data', (b) => { out += b; });
  const deadline = Date.now() + 15000;
  while (!out.includes('admin:')) {
    if (Date.now() > deadline || proc.exitCode != null) throw new Error('devserver did not start:\n' + out);
    await sleep(50);
  }
  const base = `http://127.0.0.1:${port}`;
  const admin = `http://127.0.0.1:${adminPort}`;
  const browsers = [];
  return {
    base,
    admin,
    dir,
    log: () => out,
    async browser(opts = {}) {
      const b = await launch(dir, opts);
      browsers.push(b);
      return b;
    },
    async stop() {
      for (const b of browsers) await b.close().catch(() => {});
      proc.kill('SIGTERM');
      await new Promise((r) => { if (proc.exitCode != null) r(); else proc.once('exit', r); setTimeout(r, 3000); });
      fs.rmSync(dir, { recursive: true, force: true });
    },
  };
}

async function launch(dir, { args = [] } = {}) {
  const userDataDir = fs.mkdtempSync(path.join(dir, 'chrome-'));
  return puppeteer.launch({
    executablePath: CHROME,
    headless: 'new',
    userDataDir,
    args: ['--no-first-run', '--no-default-browser-check', '--disable-features=Translate', '--hide-scrollbars', ...args],
    defaultViewport: null,
  });
}

/** Signs a page in with a fresh one-time login link; waits for the inbox. */
export async function login(page, demo, path = '/') {
  const res = await fetch(demo.admin + '/v1/login-link', { method: 'POST' });
  const link = await res.json();
  await page.goto(link.url.replace(/^http:\/\/[^/]+/, demo.base), { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('main .page-title', { timeout: 15000 });
  await page.waitForFunction(() => !document.querySelector('[aria-busy="true"]'), { timeout: 10000 }).catch(() => {});
  if (path !== '/') await goto(page, demo, path);
}

/** In-app navigation (keeps the live connection, like a tap would). */
export async function goto(page, demo, path) {
  await page.evaluate((p) => { history.pushState({ inApp: true }, '', p); dispatchEvent(new PopStateEvent('popstate')); }, path);
  await page.waitForSelector('main .view', { timeout: 10000 });
  await sleep(150);
  // Let the view's slide-in finish, as a finger would.
  await page.waitForFunction(() => !document.getAnimations().some((a) => a.playState === 'running' && a.effect && a.effect.target && a.effect.target.classList && a.effect.target.classList.contains('view')), { timeout: 2000 }).catch(() => {});
}

export function watch(page, tag) {
  const logs = [];
  page.on('console', (m) => { if (m.type() === 'error' || m.type() === 'warning') logs.push(`[${tag}] console.${m.type()}: ${m.text()}`); });
  page.on('pageerror', (e) => logs.push(`[${tag}] pageerror: ${e.message}`));
  page._logs = logs;
  return logs;
}

export const PHONE = { width: 390, height: 844, isMobile: true, hasTouch: true, deviceScaleFactor: 2 };
export const DESKTOP = { width: 1440, height: 900, deviceScaleFactor: 1 };

export async function scheme(page, s) {
  await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: s }, { name: 'prefers-reduced-motion', value: 'reduce' }]);
}

export async function shot(page, name, opts = {}) {
  const p = path.join(SHOTS, name + '.png');
  await page.screenshot({ path: p, fullPage: !!opts.full });
  return p;
}

export async function text(page, sel = 'body') {
  return page.$eval(sel, (e) => e.innerText).catch(() => '');
}

export async function waitText(page, needle, timeout = 8000, sel = 'body') {
  return page.waitForFunction(
    (s, n) => { const e = document.querySelector(s); return e && e.innerText.includes(n); },
    { timeout, polling: 100 }, sel, needle,
  ).then(() => true, () => false);
}

const axeSrc = fs.readFileSync(new URL('./node_modules/axe-core/axe.min.js', import.meta.url), 'utf8');

/** axe violations on the current page (evaluated over CDP, so the CSP stays on). */
export async function axe(page, context = 'document') {
  const has = await page.evaluate(() => typeof window.axe !== 'undefined');
  if (!has) await page.evaluate(axeSrc);
  return page.evaluate(async (ctx) => {
    const r = await window.axe.run(ctx === 'document' ? document : document.querySelector(ctx), {
      resultTypes: ['violations'],
      runOnly: { type: 'tag', values: ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa', 'best-practice'] },
    });
    return r.violations.map((v) => ({ id: v.id, impact: v.impact, n: v.nodes.length, ex: v.nodes.slice(0, 3).map((n) => n.target.join(' ') + ' :: ' + (n.failureSummary || '').split('\n').slice(1, 2).join('')) }));
  }, context);
}

/** Fails a check for serious/critical violations; prints the rest. */
export async function axeCheck(page, label) {
  const v = await axe(page);
  for (const x of v) console.log(`  axe ${label}: [${x.impact}] ${x.id} ×${x.n}  ${x.ex.join(' || ')}`);
  const severe = v.filter((x) => x.impact === 'serious' || x.impact === 'critical');
  check(`axe: ${label}`, severe.length === 0, severe.map((x) => x.id).join(', '));
  return v;
}
