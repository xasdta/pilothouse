// 4. Service worker + Web Push (real FCM delivery), notification action Allow.
// HEADLESS=0 to run headful.
import { BASE, launch, watch, pairViaUI, shot, text, waitText, check, summary, hold, readOut, sleep, sh } from './lib.mjs';

const headless = process.env.HEADLESS !== '0';
console.log('mode:', headless ? 'headless(new)' : 'headful');
const RUN = Date.now().toString(36);
const browser = await launch({ headless });
const page = await browser.newPage();
await page.setViewport({ width: 390, height: 844 });
watch(page, 'P');

check('paired', (await pairViaUI(page, 'Push device')).status === 200);
const bcdp = await browser.target().createCDPSession();
await bcdp.send('Browser.grantPermissions', { origin: BASE, permissions: ['notifications'] });
await page.goto(BASE + '/settings', { waitUntil: 'domcontentloaded' });
await page.waitForSelector('.page-title');
await sleep(1500);
check('Notification.permission granted', (await page.evaluate(() => Notification.permission)) === 'granted');
const reg = await page.evaluate(async () => { const r = await navigator.serviceWorker.ready; return { scope: r.scope, state: r.active && r.active.state }; });
check('SW ready + activated', reg.state === 'activated', JSON.stringify(reg));
await shot(page, '04-settings-notif-off');

// Enable via the real button.
const btn = await page.waitForSelector('xpath///button[contains(., "Turn on notifications")]', { timeout: 5000 }).catch(() => null);
check('"Turn on notifications" button shown', !!btn);
const subResp = page.waitForResponse((r) => r.url().endsWith('/api/push/subscribe'), { timeout: 90000 }).catch(() => null);
const tSub = Date.now();
if (btn) await btn.click();
const sr = await subResp;
check('POST /api/push/subscribe 204', sr && sr.status() === 204, sr ? `${sr.status()} after ~${Math.round((Date.now() - tSub) / 1000)}s` : 'no request (subscribe failed?)');
await sleep(1000);
const on = await waitText(page, 'Notifications are on', 5000);
check('settings shows "Notifications are on"', on, (await text(page, 'main')).slice(0, 200));
const sub = await page.evaluate(async () => { const r = await navigator.serviceWorker.ready; const s = await r.pushManager.getSubscription(); return s && s.endpoint; });
check('push subscription endpoint is FCM', /fcm\.googleapis\.com|googleapis/.test(sub || ''), sub && sub.slice(0, 60));
await shot(page, '04-settings-notif-on');

async function waitNotif(pred, timeout = 30000) {
  const end = Date.now() + timeout;
  while (Date.now() < end) {
    const ns = await page.evaluate(async () => {
      const r = await navigator.serviceWorker.ready;
      return (await r.getNotifications()).map((n) => ({ title: n.title, body: n.body, tag: n.tag, data: n.data, actions: n.actions, requireInteraction: n.requireInteraction }));
    });
    const hit = ns.find(pred);
    if (hit) return hit;
    await sleep(500);
  }
  return null;
}

// Test push.
const t0 = Date.now();
const testBtn = await page.$('xpath///button[contains(., "Send a test")]');
if (testBtn) await testBtn.click();
const tn = await waitNotif((n) => /connected/i.test(n.title));
check('test push delivered via FCM → SW notification', !!tn, tn ? `${Date.now() - t0} ms: ${tn.title} / ${tn.body}` : 'none in 30s');

// Presence → Away through the UI; daemon must agree.
await (await page.$('xpath///button[@role="radio"][contains(., "Away")]')).click();
await sleep(800);
const pres = JSON.parse(sh('curl -s --unix-socket /tmp/qa2/run/pilothouse.sock http://x/v1/presence'));
check('Settings "Away" changes daemon presence', pres.mode === 'away' && pres.state === 'away', JSON.stringify(pres));
check('top chip shows Away', (await page.$eval('.chip', (e) => e.innerText)).includes('Away'));
await shot(page, '04-settings-away');

// Held request while away → push with actions.
await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
const t1 = Date.now();
hold('push', `echo qa-push-${RUN}`);
const rn = await waitNotif((n) => n.data && n.data.request_id && (n.body || '').includes(`qa-push-${RUN}`) || (n.title || '').includes(`qa-push-${RUN}`));
check('request push delivered', !!rn, rn ? `${Date.now() - t1} ms: ${JSON.stringify(rn).slice(0, 300)}` : 'none in 30s');
check('request notification has Allow/Deny actions', rn && rn.actions && rn.actions.map((a) => a.action).join() === 'allow,deny', rn && JSON.stringify(rn.actions));
check('request notification requireInteraction', rn && rn.requireInteraction === true);
console.log('Notification.maxActions =', await page.evaluate(() => Notification.maxActions));

// Notification action Allow: dispatch notificationclick inside the SW with the real notification.
if (rn) {
  const swT = await browser.waitForTarget((t) => t.type() === 'service_worker' && t.url().endsWith('/sw.js'));
  const sw = await swT.worker();
  const r = await sw.evaluate(async (rid) => {
    const ns = await self.registration.getNotifications();
    const n = ns.find((x) => x.data && x.data.request_id === rid);
    if (!n) return 'no notification';
    const ev = new NotificationEvent('notificationclick', { notification: n, action: 'allow' });
    try { self.dispatchEvent(ev); } catch (e) { return 'dispatch threw ' + e.message; }
    return 'dispatched';
  }, rn.data.request_id);
  console.log('SW dispatch:', r);
  const out = await readOut('push', 15000);
  check('SW notification action "allow" signs + posts → hook allow', /"behavior":"allow"/.test(out), out || '(no hook output)');
  const by = JSON.parse(sh(`curl -s --unix-socket /tmp/qa2/run/pilothouse.sock http://x/v1/requests/${rn.data.request_id}`));
  check('decision attributed to device', by.decision && /^device:/.test(by.decision.by), JSON.stringify(by.decision));
  check('page toast "Allowed" from SW message', await waitText(page, 'Allowed', 4000));
}

// Back to auto for later tests.
await page.evaluate(() => fetch('/api/presence', { method: 'PUT', headers: { 'X-Pilothouse': '1', 'Content-Type': 'application/json' }, body: JSON.stringify({ mode: 'auto' }) }));
for (const l of page._logs) console.log(l);
await browser.close();
process.exit(summary());
