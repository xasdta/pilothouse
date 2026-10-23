// 8. Regression checks for the QA2 web findings (W1–W6), against a real
// daemon: no literal "null", hidden-tab updates, bfcache navigations,
// SSE ping + silent-drop watchdog, failed pair code retry, push progress,
// offline screen copy.
import fs from 'node:fs';
import { execSync } from 'node:child_process';
import { BASE, QA, launch, watch, pairViaUI, pairCode, shot, text, waitText, check, summary, holdJSON, sleep, sh } from './lib.mjs';

const RUN = Date.now().toString(36);
const CWD = '/tmp/qa2/uhome/nobranch'; // not a git repo: the card has no branch
fs.mkdirSync(CWD, { recursive: true });
const sid = (p) => `${p}-0000-0000-0000-${RUN.padStart(12, '0').slice(-12)}`;
const nulls = (s) => (s.replace(/\n/g, ' ⏎ ').match(/.{0,30}\bnull\b.{0,20}/g) || []);

const browser = await launch(); // bfcache left ON on purpose (W3)
const page = await browser.newPage();
watch(page, 'P');
await page.setViewport({ width: 390, height: 844, isMobile: true, hasTouch: true, deviceScaleFactor: 2 });

// ---- W6: a failed code from the link shows an error state and a retry field ----
await page.goto(`${BASE}/pair#AAAA-BBBB-CCCC`, { waitUntil: 'networkidle0' });
await page.waitForSelector('#pair-name');
const bad = page.waitForResponse((r) => r.url().endsWith('/api/pair'));
await page.click('button[type=submit]');
check('W6 bad link code → 401', (await bad).status() === 401);
await sleep(300);
const st = await page.evaluate(() => ({
  bad: !!document.querySelector('.code-chip.bad'),
  ok: !!document.querySelector('.code-chip:not(.bad)'),
  input: !!document.querySelector('#pair-code'),
  err: (document.querySelector('.form-error') || {}).innerText || '',
}));
check('W6 failed code chip shows error state (no ✓ chip)', st.bad && !st.ok, JSON.stringify(st));
check('W6 failed code → field to enter a new code', st.input, JSON.stringify(st));
await shot(page, '08-pair-code-failed');
const good = pairCode();
await page.type('#pair-code', good);
const ok = page.waitForResponse((r) => r.url().endsWith('/api/pair'));
await page.click('button[type=submit]');
check('W6 retry with a new code pairs', (await ok).status() === 200);
check('W6 retry → "You’re paired"', await waitText(page, 'You’re paired', 5000));

// ---- W1: no literal "null" (no-branch card, MCP card, toast) ----
const FIX = new URL('../../../internal/adapters/claude/testdata/hooks', import.meta.url).pathname;
const fx = (n, over) => ({ ...JSON.parse(fs.readFileSync(`${FIX}/${n}.json`, 'utf8').replaceAll('/home/user/project', CWD).replaceAll('/home/user', '/tmp/qa2/uhome')), cwd: CWD, ...over });
await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
await page.waitForSelector('.page-title');
holdJSON('w1mcp', fx('doc_permission_mcp', { session_id: sid('77777777') }));
holdJSON('w1bash', { ...fx('permission_bash', { session_id: sid('88888888') }), tool_input: { command: `echo w1-${RUN}`, description: 'x' } });
check('W1 cards appear', await waitText(page, `echo w1-${RUN}`, 10000, 'main'));
await sleep(800);
const mainTxt = await text(page, 'main');
check('W1 inbox (MCP + no-branch cards) has no literal "null"', nulls(mainTxt).length === 0, nulls(mainTxt).join(' | ') || mainTxt.slice(0, 160));
check('W1 MCP card rendered', mainTxt.includes('Wants to use'));
await page.evaluate(() => import('/js/ui.js').then((m) => m.toast('Notifications are off')));
await sleep(200);
const toastTxt = await text(page, '.toast');
check('W1 kind-less toast has no literal "null"', toastTxt.trim() === 'Notifications are off', JSON.stringify(toastTxt));
for (const [p, label] of [[`/s/`, 'session'], ['/sessions', 'sessions']]) {
  const snap = JSON.parse(sh('curl -s --unix-socket /tmp/qa2/run/pilothouse.sock http://x/v1/snapshot'));
  const s = snap.sessions.find((x) => x.agent_session_id === sid('77777777'));
  await page.goto(BASE + (p === '/s/' ? '/s/' + s.id : p), { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('main .view');
  await sleep(1200);
  const t = await text(page, 'main');
  check(`W1 ${label} view has no literal "null"`, nulls(t).length === 0, nulls(t).join(' | '));
}

// ---- W5: server sends `event: ping`; client reconnects after a silent drop ----
await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
await page.waitForSelector('.page-title');
const cdp = await page.createCDPSession();
await cdp.send('Network.enable');
let pings = 0;
cdp.on('Network.eventSourceMessageReceived', (e) => { if (e.eventName === 'ping') pings++; });
const t0 = Date.now();
while (!pings && Date.now() - t0 < 20000) await sleep(250);
check('W5 `event: ping` reaches the page within 20 s', pings > 0, `${Date.now() - t0} ms`);
const dpid = fs.readFileSync(`${QA}/daemon.pid`, 'utf8').trim();
execSync(`kill -STOP ${dpid}`); // TCP stays up, nothing arrives: a silent drop
const tStop = Date.now();
const noticed = await page.waitForFunction(() => document.querySelector('.banner.show') || document.querySelector('.conn.connecting,.conn.offline'),
  { timeout: 65000, polling: 500 }).then(() => true, () => false);
const tNoticed = Date.now() - tStop;
execSync(`kill -CONT ${dpid}`);
check('W5 silent drop noticed by the watchdog (≤ 60 s)', noticed && tNoticed <= 60000, `${tNoticed} ms`);
const back = await page.waitForFunction(() => !document.querySelector('.banner.show') && !document.querySelector('.conn.connecting,.conn.offline'),
  { timeout: 60000, polling: 500 }).then(() => true, () => false);
check('W5 live again after the daemon resumes', back);

// ---- W2: a hidden tab keeps updating (title badge + list) ----
await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
await page.waitForSelector('.page-title');
await sleep(1000);
const cover = await browser.newPage();
await cover.goto('about:blank');
await cover.bringToFront();
await sleep(500);
const vis = await page.evaluate(() => document.visibilityState);
const n0 = Number(((await page.title()).match(/^\((\d+)\)/) || [0, 0])[1]);
holdJSON('w2', { ...fx('permission_bash', { session_id: sid('99999999') }), tool_input: { command: `echo w2-hidden-${RUN}`, description: 'x' } });
const gotTitle = await page.waitForFunction((n) => (document.title.match(/^\((\d+)\)/) || [0, 0])[1] > n, { timeout: 15000, polling: 250 }, n0).then(() => true, () => false);
check('W2 hidden tab: title badge count updates', gotTitle, `visibility=${vis} title=${await page.title()} (was ${n0})`);
const gotList = await page.evaluate((needle) => document.querySelector('main').innerText.includes(needle), `w2-hidden-${RUN}`);
check('W2 hidden tab: inbox list updated', gotList);
await page.bringToFront();
await cover.close();

// ---- W3: bfcache must not exhaust the HTTP/1.1 connection pool ----
const conns = () => execSync(`lsof -nP -iTCP:7592 -sTCP:ESTABLISHED | grep -c '^Google' || true`, { encoding: 'utf8' }).trim();
let hang = '';
for (let i = 0; i < 12 && !hang; i++) {
  try { await page.goto(BASE + (i % 2 ? '/sessions' : '/settings'), { waitUntil: 'domcontentloaded', timeout: 8000 }); }
  catch { hang = `load #${i + 1} hung, conns=${conns()}`; }
  await sleep(600);
}
for (let i = 0; i < 4 && !hang; i++) {
  try { await (i % 2 ? page.goForward({ timeout: 8000 }) : page.goBack({ timeout: 8000 })); }
  catch { hang = `history step #${i + 1} hung, conns=${conns()}`; }
  await sleep(600);
}
check('W3 12 navigations + back/forward with bfcache on: no hang', !hang, hang || `conns=${conns()}`);
const live = await page.waitForFunction(() => document.querySelector('.conn') && !document.querySelector('.conn.connecting,.conn.offline') && !document.querySelector('.banner.show'),
  { timeout: 15000 }).then(() => true, () => false);
check('W3 page restored from history is live', live);

// ---- W6: push subscribe shows progress while pending ----
const bcdp = await browser.target().createCDPSession();
await bcdp.send('Browser.grantPermissions', { origin: BASE, permissions: ['notifications'] });
await page.goto(BASE + '/settings', { waitUntil: 'domcontentloaded' });
const on = await page.waitForSelector('xpath///button[contains(., "Turn on notifications")]', { timeout: 8000 }).catch(() => null);
if (on) {
  await on.click();
  const prog = await page.evaluate(() => ({
    spin: !!document.querySelector('button .spin'),
    btn: [...document.querySelectorAll('button')].map((b) => b.innerText).find((t) => /Turning on/.test(t)) || '',
    status: (document.querySelector('.push-progress') || {}).innerText || '',
  }));
  check('W6 push subscribe shows spinner + status while pending', prog.spin && /Turning on/.test(prog.btn) && prog.status.length > 0, JSON.stringify(prog));
  await shot(page, '08-push-progress');
  const done = await waitText(page, 'Notifications are on', 60000);
  check('INFO push subscribe completed', done);
  const off = done && await page.$('xpath///button[contains(., "Turn off")]');
  if (off) { await off.click(); await sleep(600); }
} else check('W6 push: "Turn on notifications" button present', false);

// ---- W6: offline error screen does not repeat its heading ----
const ctx = await browser.createBrowserContext();
const p3 = await ctx.newPage();
await p3.setViewport({ width: 390, height: 844, isMobile: true, hasTouch: true, deviceScaleFactor: 2 });
await p3.goto(BASE + '/', { waitUntil: 'domcontentloaded' }); // loads the app shell (unpaired → /pair)
await sleep(1500);
await p3.setRequestInterception(true);
p3.on('request', (r) => (r.url().includes('/api/') ? r.abort('internetdisconnected') : r.continue()));
await p3.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
await waitText(p3, 'Can’t reach your computer', 8000);
const errTxt = await text(p3, 'main');
const reps = (errTxt.match(/Can’t reach your computer/g) || []).length;
check('W6 offline screen says "Can’t reach your computer" once', reps === 1, JSON.stringify(errTxt));
await shot(p3, '08-offline-screen');
await ctx.close();

// Release the requests this script left held (other scripts count open cards).
const DETACH = new URL('../claude/detach.py', import.meta.url).pathname;
for (const r of JSON.parse(sh('curl -s --unix-socket /tmp/qa2/run/pilothouse.sock "http://x/v1/requests?status=open"') || '[]')) {
  const s = JSON.parse(sh('curl -s --unix-socket /tmp/qa2/run/pilothouse.sock http://x/v1/sessions/' + r.session_id) || '{}');
  if (['77777777', '88888888', '99999999'].some((p) => (s.agent_session_id || '').startsWith(p))) sh(`python3 ${DETACH} $PH deny ${r.id} -m e2e-cleanup`);
}

for (const l of page._logs) if (!/status of 40[14]|ERR_INTERNET_DISCONNECTED/.test(l)) console.log(l);
await browser.close();
process.exit(summary());
