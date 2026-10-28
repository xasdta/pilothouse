// 1. Pairing through the real UI.
import { BASE, launch, watch, pairViaUI, pairCode, shot, text, waitText, check, summary, cookieOf, sleep } from './lib.mjs';

const browser = await launch();
const page = await browser.newPage();
await page.setViewport({ width: 390, height: 844, isMobile: true, hasTouch: true, deviceScaleFactor: 2 });
watch(page, 'p1');

// Unpaired device goes to /pair with a code input.
await page.goto(BASE + '/', { waitUntil: 'networkidle0' });
check('unpaired / redirects to /pair', page.url().endsWith('/pair'), page.url());
check('manual code input shown', !!(await page.$('#pair-code')));
await shot(page, '01-pair-manual-mobile');

// Pair via link.
const r = await pairViaUI(page, 'QA Phone');
check('POST /api/pair 200', r.status === 200, `status ${r.status} ${JSON.stringify(r.body).slice(0, 120)}`);
check('code removed from URL/history', !page.url().includes(r.code), page.url());
const done = await waitText(page, 'You’re paired', 5000);
check('success screen shows "You’re paired"', done, (await text(page)).slice(0, 200));
check('success names device', (await text(page)).includes('QA Phone'));
check('primary action opens the inbox', (await text(page, '.pair-actions .btn.primary')) === 'Open inbox');
const secondary = await page.$('.pair-actions .btn.secondary');
if (secondary) check('notifications are optional', (await text(page, '.pair-actions .btn.secondary')) === 'Set up notifications');
await shot(page, '01-pair-done-mobile');

// Cookie flags.
const cdp = await page.createCDPSession();
const { cookies } = await cdp.send('Network.getAllCookies');
const c = cookieOf(cookies);
check('ph_dev cookie set', !!c);
if (c) {
  check('cookie HttpOnly', c.httpOnly === true);
  check('cookie SameSite=Strict', c.sameSite === 'Strict', c.sameSite);
  check('cookie not Secure on http loopback (expected per spec)', c.secure === false);
  check('cookie path=/', c.path === '/');
  check('cookie not visible to JS', !(await page.evaluate(() => document.cookie)).includes('ph_dev'));
}

// Key non-extractable, stored in IndexedDB.
const key = await page.evaluate(async () => {
  const rec = await self.PilothouseKeys.load();
  const out = { has: !!rec, extractable: rec && rec.privateKey.extractable, alg: rec && rec.privateKey.algorithm, usages: rec && rec.privateKey.usages };
  try { await crypto.subtle.exportKey('pkcs8', rec.privateKey); out.exported = true; } catch (e) { out.exported = false; out.err = e.name; }
  try { await crypto.subtle.exportKey('jwk', rec.privateKey); out.jwk = true; } catch (e) { out.jwk = false; }
  return out;
});
check('key stored in IndexedDB', key.has);
check('private key extractable=false', key.extractable === false, JSON.stringify(key));
check('exportKey(pkcs8/jwk) throws', key.exported === false && key.jwk === false, key.err);
check('key is ECDSA P-256', key.alg && key.alg.name === 'ECDSA' && key.alg.namedCurve === 'P-256');

// Open inbox (the primary action, even when notifications are off).
await page.click('.pair-actions .btn.primary');
await sleep(800);
await shot(page, '01-after-pair-next');
check('after pair → inbox', new URL(page.url()).pathname === '/', page.url());

// Code reuse rejected (fresh context, same code).
const ctx2 = await browser.createBrowserContext();
const p2 = await ctx2.newPage();
watch(p2, 'p2');
await p2.goto(`${BASE}/pair#${r.code}`, { waitUntil: 'networkidle0' });
await p2.waitForSelector('#pair-name');
const resp2 = p2.waitForResponse((x) => x.url().endsWith('/api/pair'));
await p2.click('button[type=submit]');
const rr2 = await resp2;
check('reused code → 401 invalid_code', rr2.status() === 401, String(rr2.status()));
const shown = await waitText(p2, 'didn’t work', 3000);
check('reused code error shown in UI', shown, (await text(p2)).slice(0, 300));
await shot(p2, '01-pair-reused-code');

// Garbage code.
await p2.goto(`${BASE}/pair#AAAA-BBBB-CCCC`, { waitUntil: 'networkidle0' });
await p2.waitForSelector('#pair-name');
const resp3 = p2.waitForResponse((x) => x.url().endsWith('/api/pair'));
await p2.click('button[type=submit]');
check('wrong code → 401', (await resp3).status() === 401);

// Manual entry path (no hash) with a valid code.
const code3 = pairCode();
const ctx3 = await browser.createBrowserContext();
const p3 = await ctx3.newPage();
watch(p3, 'p3');
await p3.goto(`${BASE}/pair`, { waitUntil: 'networkidle0' });
await p3.type('#pair-code', code3.toLowerCase().replace(/-/g, ' '));
const resp4 = p3.waitForResponse((x) => x.url().endsWith('/api/pair'));
await p3.click('button[type=submit]');
const r4 = await resp4;
check('manual code entry (lowercase, spaces) accepted', r4.status() === 200, String(r4.status()));
await ctx3.close();

for (const l of [...page._logs, ...p2._logs]) console.log(l);
await browser.close();
process.exit(summary());
