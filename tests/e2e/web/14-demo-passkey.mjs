// 14. Passkey key recovery on the demo core, with Chrome's virtual
// authenticator: add a passkey in Settings, lose the signing key (as iOS
// does), restore it from the banner with the passkey, and check the daemon
// now holds the restored key. On an IP-address URL the passkey row is hidden.
//
//   PH_TMP=… PH_SHOTS=… node 14-demo-passkey.mjs
import { startDemo, watch, check, summary, sleep, shot, waitText, scheme, PHONE } from './demo-lib.mjs';

const demo = await startDemo();
// Passkeys need a host name: localhost is a secure context and a valid RP ID.
const base = demo.base.replace('127.0.0.1', 'localhost');
const browser = await demo.browser();
const page = await browser.newPage();
await page.setViewport(PHONE);
await scheme(page, 'light');
const logs = watch(page, 'P');

const cdp = await page.createCDPSession();
await cdp.send('WebAuthn.enable');
const { authenticatorId } = await cdp.send('WebAuthn.addVirtualAuthenticator', {
  options: { protocol: 'ctap2', transport: 'internal', hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true },
});

const link = await (await fetch(demo.admin + '/v1/login-link', { method: 'POST' })).json();
await page.goto(link.url.replace(/^http:\/\/[^/]+/, base), { waitUntil: 'domcontentloaded' });
await page.waitForSelector('main .page-title', { timeout: 15000 });

const me = () => page.evaluate(() => fetch('/api/me').then((r) => r.json()));
const settings = async () => {
  await page.evaluate(() => { history.pushState({ inApp: true }, '', '/settings'); dispatchEvent(new PopStateEvent('popstate')); });
  await page.waitForSelector('main .view.settings', { timeout: 10000 });
  await sleep(300);
};

// ---- Add a passkey ----
await settings();
check('settings shows the Passkey row', await waitText(page, 'Lets Face ID or Touch ID restore', 5000, 'main'));
const addBtn = await page.waitForSelector('main button.btn.small.secondary:not([disabled])', { timeout: 5000 });
check('Add passkey button', (await addBtn.evaluate((b) => b.innerText)).includes('Add passkey'));
await addBtn.click();
check('toast: passkey added', await waitText(page, 'Passkey added', 8000));
check('/api/me reports the passkey', (await me()).device.passkey === true);
const { credentials } = await cdp.send('WebAuthn.getCredentials', { authenticatorId });
check('authenticator holds one passkey for localhost', credentials.length === 1 && credentials[0].rpId === 'localhost', JSON.stringify(credentials.map((c) => c.rpId)));
await settings();
check('settings shows Passkey: On', await waitText(page, 'On — restores the signing key', 5000, 'main'));
await page.evaluate(() => [...document.querySelectorAll('main b')].find((b) => b.innerText === 'Passkey').scrollIntoView({ block: 'center' }));
await sleep(200);
await shot(page, '14-passkey-on');

// ---- Lose the key, restore it ----
const before = (await me()).device.public_key;
await page.evaluate(() => self.PilothouseKeys.clear());
await page.reload({ waitUntil: 'domcontentloaded' });
await page.waitForSelector('main .page-title', { timeout: 15000 });
check('banner offers Restore', await waitText(page, 'restore it with your passkey', 8000, '.banner'));
await sleep(500); // the restore challenge is fetched before the tap
await shot(page, '14-passkey-banner');
const restoreBtn = await page.waitForSelector('.banner.key button:not([disabled])', { timeout: 5000 });
await restoreBtn.click();
check('toast: key restored', await waitText(page, 'Signing key restored', 8000));
await sleep(300);
check('banner gone', await page.$eval('.banner', (b) => !b.classList.contains('key')));
const after = await me();
const local = await page.evaluate(() => self.PilothouseKeys.load().then((r) => r && r.spki));
check('daemon holds a new key', after.device.public_key && after.device.public_key !== before);
check('this browser holds the same key', local === after.device.public_key);
check('no console errors', logs.length === 0, logs.join('\n'));
const signedOK = await page.evaluate(async () => {
  const res = await self.PilothouseKeys.postSigned('/api/passkeys', 'passkey', 'add', {});
  return res.data && res.data.code; // signature accepted; the empty payload is then refused
});
check('the restored key signs actions', signedOK === 'bad_passkey', String(signedOK));

// ---- Pairing offers a passkey ----
const code = (await (await fetch(demo.admin + '/v1/pair', { method: 'POST' })).json()).code;
const page3 = await (await browser.createBrowserContext()).newPage();
await page3.setViewport(PHONE);
await page3.goto(`${base}/pair#${code}`, { waitUntil: 'networkidle0' });
await page3.waitForSelector('#pair-name');
await page3.click('button[type=submit]');
check('pair done screen offers a passkey', await waitText(page3, 'Keep this pairing if iOS clears the app', 8000));
await shot(page3, '14-passkey-pair-done');

// ---- IP-address URLs can't use passkeys ----
const link2 = await (await fetch(demo.admin + '/v1/login-link', { method: 'POST' })).json();
const page2 = await (await browser.createBrowserContext()).newPage();
await page2.goto(link2.url.replace(/^http:\/\/[^/]+/, demo.base), { waitUntil: 'domcontentloaded' });
await page2.waitForSelector('main .page-title', { timeout: 15000 });
await page2.evaluate(() => { history.pushState({ inApp: true }, '', '/settings'); dispatchEvent(new PopStateEvent('popstate')); });
await page2.waitForSelector('main .view.settings', { timeout: 10000 });
await sleep(300);
check('no Passkey row on 127.0.0.1', !(await page2.$eval('main', (m) => m.innerText.includes('Passkey'))));

await demo.stop();
process.exit(summary() ? 1 : 0);
