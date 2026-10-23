// Shared helpers for the Pilothouse PWA end-to-end checks.
import puppeteer from 'puppeteer-core';
import { execSync, spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

export const BASE = process.env.PH_BASE || 'http://127.0.0.1:7592';
export const SHOTS = process.env.PH_SHOTS || '/private/tmp/qa2/shots';
export const CHROME = '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
export const QA = '/tmp/qa2';
fs.mkdirSync(SHOTS, { recursive: true });

const results = [];
export function check(name, ok, detail = '') {
  results.push({ name, ok: !!ok, detail });
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? '  — ' + detail : ''}`);
}
export function summary() {
  const f = results.filter((r) => !r.ok);
  console.log(`\n${results.length - f.length}/${results.length} passed`);
  return f.length;
}

export function sh(cmd) {
  return execSync(`bash -c 'source ${QA}/env.sh; ${cmd.replace(/'/g, `'\\''`)}'`, { encoding: 'utf8' });
}

/** A fresh pairing code from the daemon (double-forked to escape the agent guard). */
export function pairCode() {
  const out = sh('paircode');
  return JSON.parse(out).code;
}

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

export async function launch({ headless = true, dir, args = [] } = {}) {
  const userDataDir = dir || fs.mkdtempSync(path.join(os.tmpdir(), 'ph-e2e-'));
  const browser = await puppeteer.launch({
    executablePath: CHROME,
    headless: headless ? 'new' : false,
    userDataDir,
    args: ['--no-first-run', '--no-default-browser-check', '--disable-features=Translate', ...args],
    defaultViewport: null,
  });
  browser._udd = userDataDir;
  return browser;
}

export function watch(page, tag) {
  const logs = [];
  page.on('console', (m) => logs.push(`[${tag}] console.${m.type()}: ${m.text()}`));
  page.on('pageerror', (e) => logs.push(`[${tag}] pageerror: ${e.message}`));
  page.on('requestfailed', (r) => {
    const u = r.url();
    if (!u.includes('/api/events')) logs.push(`[${tag}] requestfailed: ${u} ${r.failure() && r.failure().errorText}`);
  });
  page._logs = logs;
  return logs;
}

/** Pairs a page through the real UI. Returns the /api/pair JSON. */
export async function pairViaUI(page, name) {
  const code = pairCode();
  await page.goto(`${BASE}/pair#${code}`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('#pair-name');
  await page.$eval('#pair-name', (el) => { el.value = ''; });
  await page.type('#pair-name', name);
  const resp = page.waitForResponse((r) => r.url().endsWith('/api/pair'));
  await page.click('button[type=submit]');
  const r = await resp;
  const body = await r.json().catch(() => ({}));
  return { status: r.status(), body, code, headers: r.headers() };
}

export async function shot(page, name) {
  const p = path.join(SHOTS, name + '.png');
  await page.screenshot({ path: p, fullPage: true });
  return p;
}

export async function text(page, sel = 'body') {
  return page.$eval(sel, (e) => e.innerText).catch(() => '');
}

export async function waitText(page, needle, timeout = 10000, sel = 'body') {
  return page.waitForFunction(
    (s, n) => { const e = document.querySelector(s); return e && e.innerText.includes(n); },
    { timeout, polling: 100 }, sel, needle,
  ).then(() => true, () => false);
}

export function readOut(name, timeout = 10000) {
  const f = `${QA}/hook-${name}.out`;
  const end = Date.now() + timeout;
  return new Promise((resolve) => {
    const t = setInterval(() => {
      const s = fs.existsSync(f) ? fs.readFileSync(f, 'utf8') : '';
      if (s.trim() || Date.now() > end) { clearInterval(t); resolve(s.trim()); }
    }, 100);
  });
}

let reqN = 0;
/** Starts a held PermissionRequest in the background (hook shim). */
export function hold(name, cmd, sid) {
  reqN++;
  sid = sid || `22222222-0000-0000-0000-${String(Date.now() % 1e9).padStart(9, '0')}${String(reqN).padStart(3, '0')}`;
  for (const ext of ['out', 'err']) try { fs.unlinkSync(`${QA}/hook-${name}.${ext}`); } catch { /* none */ }
  spawnSync('bash', ['-c', `source ${QA}/env.sh; hookreq ${name} ${sid} "${cmd}"`], { stdio: 'ignore' });
  return sid;
}

export function cookieOf(cookies) {
  return cookies.find((c) => c.name === 'ph_dev');
}

/** Starts a held hook call with an arbitrary Claude payload (session_id must be set). */
export function holdJSON(name, payload) {
  const f = `${QA}/hook-${name}`;
  for (const ext of ['out', 'err']) try { fs.unlinkSync(`${f}.${ext}`); } catch { /* none */ }
  fs.writeFileSync(`${f}.in`, JSON.stringify(payload));
  const start = JSON.stringify({ session_id: payload.session_id, transcript_path: '/tmp/qa2/uhome/t.jsonl', cwd: payload.cwd, hook_event_name: 'SessionStart', source: 'startup' });
  fs.writeFileSync(`${f}.start`, start);
  spawnSync('bash', ['-c', `source ${QA}/env.sh; $PH hook claude < ${f}.start >/dev/null; ( $PH hook claude < ${f}.in > ${f}.out 2>${f}.err & )`], { stdio: 'ignore' });
}
