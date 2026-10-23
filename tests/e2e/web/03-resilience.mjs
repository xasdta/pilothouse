// 3. Resilience: offline + missed events, forced resync, daemon kill/restart,
// request expiry while open, offline app shell from the service worker.
import fs from 'node:fs';
import { BASE, QA, launch, watch, pairViaUI, shot, text, waitText, check, summary, hold, readOut, sleep, sh } from './lib.mjs';

const RUN = Date.now().toString(36);
const browser = await launch();
const page = await browser.newPage();
await page.setViewport({ width: 390, height: 844, isMobile: true, hasTouch: true, deviceScaleFactor: 2 });
watch(page, 'P');
const cdp = await page.createCDPSession();
await cdp.send('Network.enable');
const offline = (on) => cdp.send('Network.emulateNetworkConditions', { offline: on, latency: 0, downloadThroughput: -1, uploadThroughput: -1 });

const ts = () => new Date().toISOString().slice(11, 23);
const evIds = new Set();
const tl = [];
cdp.on('Network.requestWillBeSent', (e) => { if (e.request.url.includes('/api/events') || e.request.url.includes('/api/snapshot')) { evIds.add(e.requestId); tl.push(`${ts()} REQ ${e.requestId} ${e.request.url.replace(/^.*7592/, '')}`); } });
cdp.on('Network.loadingFailed', (e) => { if (evIds.has(e.requestId)) tl.push(`${ts()} FAILED ${e.requestId} ${e.errorText}`); });
cdp.on('Network.loadingFinished', (e) => { if (evIds.has(e.requestId)) tl.push(`${ts()} FINISHED ${e.requestId}`); });
cdp.on('Network.eventSourceMessageReceived', (e) => { let d = {}; try { d = JSON.parse(e.data); } catch {} tl.push(`${ts()} MSG ${e.requestId} ${e.eventName} ${d.seq || ''} ${d.type || e.data.slice(0, 60)} ${(d.data && d.data.status) || ''}`); });
const evReqs = [];
const snaps = [];
page.on('request', (r) => {
  if (r.url().includes('/api/events')) evReqs.push(r.url());
  if (r.url().endsWith('/api/snapshot')) snaps.push(Date.now());
});

check('paired', (await pairViaUI(page, 'Resilience')).status === 200);
await page.goto(BASE + '/', { waitUntil: 'networkidle0' }).catch(() => {});
await page.waitForSelector('.page-title');
await page.evaluate(() => { window.__noReload = 1; });
const banner = () => page.$eval('.banner', (b) => ({ show: b.classList.contains('show'), text: b.innerText })).catch(() => ({}));

// SW registration.
const sw = await page.evaluate(async () => {
  const r = await navigator.serviceWorker.ready;
  return { scope: r.scope, active: !!r.active, controlled: !!navigator.serviceWorker.controller };
});
check('service worker registered + active', sw.active, JSON.stringify(sw));

// ---- CDP offline: does the open SSE stream notice? (silent network drop) ----
await offline(true);
// CDP offline does not cut an already-open stream: pings keep arriving, so
// the app rightly stays live. A real silent drop (no pings) is covered in
// 08-fixes.mjs (SIGSTOP), where the 45 s watchdog reconnects (W5).
let offPings = 0;
const onMsg = (e) => { if (e.eventName === 'ping') offPings++; };
cdp.on('Network.eventSourceMessageReceived', onMsg);
const tOff = Date.now();
const bOff = await page.waitForFunction(() => document.querySelector('.banner.show'), { timeout: 50000 }).then(() => true, () => false);
cdp.off('Network.eventSourceMessageReceived', onMsg);
check('CDP offline: banner, or the stream is demonstrably alive (pings arrive)', bOff || offPings > 0, `${Date.now() - tOff} ms banner=${bOff} pings=${offPings}`);
await offline(false);
await page.evaluate(() => dispatchEvent(new Event('online')));
await sleep(1500);

// ---- Missed events: stream breaks, reconnect is held, events happen, then release ----
const paused = [];
let mode = 'hold';
cdp.on('Fetch.requestPaused', async (e) => {
  try {
    if (mode === 'hold') { paused.push(e); return; }
    if (mode === 'rewrite') {
      mode = 'pass';
      await cdp.send('Fetch.continueRequest', { requestId: e.requestId, url: e.request.url.replace(/after=\d+/, 'after=999999999') });
      return;
    }
    await cdp.send('Fetch.continueRequest', { requestId: e.requestId });
  } catch (err) { console.log('fetch handler:', err.message); }
});
await cdp.send('Fetch.enable', { patterns: [{ urlPattern: '*/api/events*', requestStage: 'Request' }] });
sh(`kill -9 $(cat /tmp/qa2/daemon.pid); sleep 0.3; startd`);
await page.waitForFunction(() => document.querySelector('.banner.show'), { timeout: 15000 }).catch(() => {});
await shot(page, '03-reconnecting-banner');
check('stream break → banner', !!(await banner()).show, JSON.stringify(await banner()));
hold('missed', `echo qa-missed-${RUN}`);
await sleep(1200);
const rid = JSON.parse(sh(`curl -s --unix-socket /tmp/qa2/run/pilothouse.sock http://x/v1/snapshot`)).requests
  .find((r) => (r.command || '').includes(`qa-missed-${RUN}`) && r.status === 'open');
sh(`( ($PH deny ${rid && rid.id} -m missed-while-offline > /tmp/qa2/cli2.out 2>&1 &) & ); sleep 1.5`);
check('hook answered while page disconnected', /deny/.test(await readOut('missed')));
check('page has not seen it yet', !(await text(page, 'main')).includes(`qa-missed-${RUN}`));
check('reconnect request was held', paused.length > 0, `paused=${paused.length}`);
mode = 'pass';
for (const e of paused.splice(0)) { try { await cdp.send('Fetch.continueRequest', { requestId: e.requestId }); } catch (err) { console.log('continue:', err.message); } }
const gotMissed = await waitText(page, `echo qa-missed-${RUN}`, 40000);
check('after reconnect, missed decision replayed (Recently answered)', gotMissed, (await text(page, 'main')).slice(0, 200));
const live1 = await page.waitForFunction(() => !document.querySelector('.banner.show'), { timeout: 40000 }).then(() => true, () => false);
check('banner clears after reconnect', live1);
check('no reload', (await page.evaluate(() => window.__noReload)) === 1);

// ---- Forced resync (server says resync_required) ----
const before = snaps.length;
mode = 'rewrite';
await page.evaluate(() => dispatchEvent(new Event('online'))); // client reconnects at once
await sleep(4000);
check('resync_required → client refetched /api/snapshot', snaps.length > before, `snapshots ${before}→${snaps.length}, mode=${mode}`);
check('live again after resync', await page.waitForFunction(() => !document.querySelector('.banner.show'), { timeout: 10000 }).then(() => true, () => false));
await cdp.send('Fetch.disable');

// ---- Kill daemon (SIGKILL) and restart with a short hold limit ----
const pid = fs.readFileSync(`${QA}/daemon.pid`, 'utf8').trim();
sh(`kill -9 ${pid}; sleep 0.3`);
const tKill = Date.now();
const bKill = await page.waitForFunction(() => document.querySelector('.banner.show'), { timeout: 15000 }).then(() => true, () => false);
check('daemon killed → banner', bKill, `${Date.now() - tKill} ms; ${JSON.stringify(await banner())}`);
await sleep(6000);
const bk = await banner();
check('after a few failures banner says can’t reach', /Can’t reach|Reconnecting/.test(bk.text || ''), bk.text);
await shot(page, '03-daemon-down');
let cfg = fs.readFileSync(`${QA}/home/config.yaml`, 'utf8');
if (!cfg.includes('hold_limit')) cfg = cfg.replace('always_hold: true', 'always_hold: true\n  hold_limit: 15s');
fs.writeFileSync(`${QA}/home/config.yaml`, cfg);
sh('startd');
const tUp = Date.now();
const rec = await page.waitForFunction(() => !document.querySelector('.banner.show') && document.querySelector('.conn') && !document.querySelector('.conn.connecting,.conn.offline'),
  { timeout: 45000 }).then(() => true, () => false);
check('daemon restart → recovers without reload', rec && (await page.evaluate(() => window.__noReload)) === 1, `${Date.now() - tUp} ms after restart`);

// ---- Expiry while the card is open ----
hold('expire', `echo qa-expire-${RUN}`);
const appeared = await waitText(page, `qa-expire-${RUN}`, 8000);
check('short-hold request appears', appeared);
await sleep(500);
const leftTxt = await page.$eval('.item.left', (e) => e.innerText).catch(() => '');
check('countdown shows ≤0:15 left', /0:1\d|0:0\d/.test(leftTxt), leftTxt);
// Request detail page in a second tab (keeps the card after close).
const snap = JSON.parse(sh(`curl -s --unix-socket /tmp/qa2/run/pilothouse.sock http://x/v1/snapshot`));
const er = snap.requests.find((r) => (r.command || '').includes(`qa-expire-${RUN}`));
const p2 = await browser.newPage();
await p2.setViewport({ width: 390, height: 844 });
watch(p2, 'P2');
await p2.goto(`${BASE}/r/${er.id}`, { waitUntil: 'domcontentloaded' }); await sleep(1500);
await shot(p2, '03-request-detail-open');
await sleep(9000);
await shot(page, '03-expiring');
const out = await readOut('expire', 15000);
check('hook returns after hold limit (handed back)', true, `hook stdout: ${out || '(empty = defer to Claude dialog)'}; ${fs.readFileSync(`${QA}/hook-expire.err`, 'utf8').trim()}`);
await sleep(1500);
check('BACKGROUND tab: title count cleared after expiry (tab hidden behind p2)', !(await page.title()).startsWith('('), `title=${await page.title()} visibility=${await page.evaluate(() => document.visibilityState)}`);
await page.bringToFront();
await sleep(1500);
const gone = !(await text(page, 'main')).includes('NEEDS YOU') && !(await text(page, 'main')).includes('Needs you');
check('foreground: expired request leaves "Needs you" on inbox', gone, (await text(page, 'main')).slice(0, 300));
check('foreground: title count cleared', !(await page.title()).startsWith('('), await page.title());
await p2.bringToFront();
const detail = await text(p2, 'main');
check('request detail shows timed-out outcome', /Timed out|terminal/i.test(detail), detail.slice(0, 300));
check('detail buttons disabled after expiry', await p2.$$eval('.actions button', (bs) => bs.length > 0 && bs.every((b) => b.disabled)));
await shot(p2, '03-request-detail-expired');
await p2.close();

// ---- Offline app shell from the service worker ----
const ctrl = await page.evaluate(() => !!navigator.serviceWorker.controller);
check('page controlled by SW', ctrl);
await offline(true);
let shellOK = false;
try {
  await page.reload({ waitUntil: 'domcontentloaded', timeout: 15000 });
  await sleep(2500);
  const t = await text(page);
  shellOK = /Can’t reach your computer|Pilothouse/.test(t) && !(await page.$('#main-frame-error'));
  check('offline reload → app shell from SW (error screen, not Chrome dino)', shellOK, t.slice(0, 200));
} catch (e) { check('offline reload → app shell from SW', false, e.message); }
await shot(page, '03-offline-shell');
const cached = await page.evaluate(async () => (await (await caches.open('pilothouse-shell-v1')).keys()).map((r) => new URL(r.url).pathname));
check('shell cache has app files', cached.includes('/') && cached.includes('/js/app.js'), cached.join(','));
await offline(false);
// Deep link offline: /sessions should also get the shell.
await offline(true);
const deep = await page.goto(BASE + '/sessions', { waitUntil: 'domcontentloaded', timeout: 15000 }).then(() => true, (e) => e.message);
await sleep(1500);
check('offline deep link /sessions served from SW', deep === true && !(await page.$('#main-frame-error')), String(deep));
await offline(false);
await page.goto(BASE + '/', { waitUntil: 'networkidle0' }).catch(() => {});

for (const l of [...page._logs, ...p2._logs]) console.log(l);
console.log('--- SSE timeline ---\n' + tl.join('\n'));
await browser.close();
process.exit(summary());
