// 11. Shell on the demo core: phone and desktop layouts, ⌘K palette,
// keyboard shortcuts and help, dark mode (system and manual), the rules
// page (presets, inbox rules, tester), and the service worker (precache,
// push → notification with actions, resolved → "Answered ✓", same tag).
//
//   PH_TMP=… PH_SHOTS=… node 11-demo-shell.mjs
import { startDemo, login, goto, watch, check, summary, sleep, shot, text, waitText, axeCheck, scheme, PHONE, DESKTOP } from './demo-lib.mjs';

const demo = await startDemo();
const browser = await demo.browser();
const page = await browser.newPage();
const logs = watch(page, 'shell');
await scheme(page, 'light');
await page.setViewport(DESKTOP);
await login(page, demo);
const visible = (sel) => page.$eval(sel, (e) => !!(e.offsetParent || e.getClientRects().length)).catch(() => false);
const path = () => page.evaluate(() => location.pathname);
const press = async (...keys) => { for (const k of keys) { await page.keyboard.press(k); await sleep(60); } };

// ---------- Layouts ----------
check('desktop: sidebar, view and needs-you pane', await visible('.sidebar') && await visible('main') && await visible('.queue'));
check('desktop: no tab bar', !(await visible('.tabbar')));
check('desktop: the queue holds the four requests', (await page.$$('.queue article.req')).length === 4);
check('desktop: inbox centre doesn’t repeat the cards', (await page.$$('main article.req')).length === 0);
check('desktop: sidebar lists sessions', (await page.$$('.sidebar .side-session')).length >= 8);
await page.setViewport(PHONE);
await sleep(200);
check('phone: tab bar with five tabs, no side panes', await visible('.tabbar') && (await page.$$('.tabbar .tab')).length === 5 && !(await visible('.sidebar')) && !(await visible('.queue')));
check('phone: the cards move into the inbox', (await page.$$('main article.req')).length === 4);
await page.setViewport(DESKTOP);
await sleep(200);

// ---------- Palette ----------
await page.keyboard.down('Control'); await page.keyboard.press('k'); await page.keyboard.up('Control');
await page.waitForSelector('dialog.palette[open]', { timeout: 3000 }).catch(() => {});
check('⌘K / Ctrl K opens the palette', await visible('dialog.palette[open]'));
check('palette: focus is in the search field', await page.evaluate(() => document.activeElement && document.activeElement.getAttribute('role') === 'combobox'));
await axeCheck(page, 'palette');
await page.keyboard.type('proof');
await sleep(100);
const first = await text(page, 'dialog.palette [aria-selected="true"]');
check('palette: fuzzy match puts the session first', first.includes('proofline'), first);
await shot(page, 'h-palette-desktop-light');
await press('Enter');
await sleep(300);
check('palette: Enter jumps to the session', (await path()) === '/s/s_proofline', await path());
await press('/');
await page.waitForSelector('dialog.palette[open]');
await page.keyboard.type('git push');
await sleep(100);
check('palette: open requests are searchable', (await text(page, 'dialog.palette [aria-selected="true"]')).includes('git push origin main'));
await press('Escape');
await sleep(150);
check('palette: Esc closes it', !(await page.$('dialog.palette[open]')));

// ---------- Shortcuts ----------
await goto(page, demo, '/');
await press('j');
const sel1 = await page.$eval('.is-selected', (e) => e.dataset.request || e.dataset.session || '').catch(() => '');
check('j selects the first request in the queue', sel1 === 'r_gitpush03', sel1);
await press('j');
const sel2 = await page.$eval('.is-selected', (e) => e.dataset.request || '').catch(() => '');
check('j moves to the next one', sel2 && sel2 !== sel1, sel2);
await press('k');
check('k moves back', (await page.$eval('.is-selected', (e) => e.dataset.request).catch(() => '')) === sel1);
await shot(page, 'h-keys-selected-desktop-light');
await press('a');
check('a allows the selected request', await waitText(page, 'Allowed', 3000, '.queue article[data-request="r_gitpush03"]'));
await sleep(1600);
const gp = await page.evaluate(() => fetch('/api/requests/r_gitpush03').then((r) => r.json()));
check('a sent a signed allow that the daemon accepted', gp.status === 'decided' && gp.decision.behavior === 'allow', gp.status);
// Question: select it and pick with number keys.
for (let i = 0; i < 6; i++) {
  const cur = await page.$eval('.is-selected', (e) => e.dataset.request || '').catch(() => '');
  if (cur === 'r_question05') break;
  await press('j');
}
await press('1');
check('1 picks the first option of the selected question', (await page.$eval('.queue article[data-request="r_question05"] .opt', (b) => b.getAttribute('aria-checked'))) === 'true');
for (let i = 0; i < 6; i++) {
  const cur = await page.$eval('.is-selected', (e) => e.dataset.request || '').catch(() => '');
  if (cur === 'r_plan0006') break;
  await press('j');
}
await press('d');
check('d on a plan opens “Request changes” with the cursor in the feedback',
  await page.$eval('.queue article[data-request="r_plan0006"]', (c) => c.classList.contains('revising') && document.activeElement === c.querySelector('.revise-form textarea')).catch(() => false));
await press('Escape'); // closes the feedback form (handled by the textarea)
check('Esc in the feedback closes it', await page.$eval('.queue article[data-request="r_plan0006"]', (c) => !c.classList.contains('revising')).catch(() => false));
await page.evaluate(() => document.activeElement && document.activeElement.blur());
await press('Escape');
await press('?');
await page.waitForSelector('dialog.help[open]', { timeout: 2000 }).catch(() => {});
check('? opens the shortcuts help', await visible('dialog.help[open]'));
await axeCheck(page, 'help');
await shot(page, 'h-help-desktop-light');
await press('Escape');
await sleep(150);
await press('g', 's');
check('g s goes to sessions', (await path()) === '/sessions');
await press('g', 'r');
check('g r goes to rules', (await path()) === '/rules');
await press('n');
check('n starts a new task', (await path()) === '/new');
// r on a selected session (phone: j walks the list).
await page.setViewport(PHONE);
await goto(page, demo, '/sessions');
await press('j');
const sid = await page.$eval('.is-selected', (e) => e.dataset.session || '').catch(() => '');
await press('r');
await sleep(300);
check('r on a selected session opens it to reply', sid && (await path()) === '/s/' + sid, `${sid} → ${await path()}`);
await page.setViewport(DESKTOP);

// ---------- Dark mode ----------
await goto(page, demo, '/settings');
const bg = () => page.evaluate(() => getComputedStyle(document.body).backgroundColor);
await scheme(page, 'dark');
await sleep(100);
check('system dark: dark background', (await bg()) === 'rgb(10, 12, 16)', await bg());
await scheme(page, 'light');
await sleep(100);
check('system light: light background', (await bg()) === 'rgb(244, 245, 248)', await bg());
await page.click('[data-theme-mode="dark"]');
await sleep(100);
check('manual dark overrides a light system', (await bg()) === 'rgb(10, 12, 16)' && (await page.evaluate(() => document.documentElement.dataset.theme)) === 'dark');
await page.reload({ waitUntil: 'domcontentloaded' });
await page.waitForSelector('main .page-title');
check('the choice survives a reload (applied before first paint)', (await page.evaluate(() => document.documentElement.dataset.theme)) === 'dark' && (await bg()) === 'rgb(10, 12, 16)');
check('theme-color follows the choice', (await page.$$eval('meta[name="theme-color"]', (m) => m.map((x) => x.content))).every((c) => c === '#0a0c10'));
await shot(page, 'h-settings-desktop-manual-dark');
await page.click('[data-theme-mode="system"]');
await sleep(100);
check('System follows the device again', (await bg()) === 'rgb(244, 245, 248)' && !(await page.evaluate(() => document.documentElement.dataset.theme)));
await page.emulateMediaFeatures([{ name: 'prefers-reduced-motion', value: 'reduce' }]);
check('reduced motion: animations are off', (await page.evaluate(() => {
  const el = document.querySelector('.view');
  return parseFloat(getComputedStyle(el).animationDuration);
})) < 0.01);

// ---------- Rules page ----------
const signedRules = [];
page.on('response', (r) => { if (/\/api\/(rules|presets)\//.test(r.url()) && r.request().method() === 'POST') signedRules.push([r.url().replace(demo.base, ''), r.status()]); });
await goto(page, demo, '/rules');
await page.waitForSelector('#preset-protect-main .switch');
const pm = '#preset-protect-main .switch';
check('protect-main starts off', (await page.$eval(pm, (b) => b.getAttribute('aria-checked'))) === 'false');
await page.click(pm);
await sleep(600);
check('preset switch: signed POST succeeds', signedRules.some(([u, s]) => u === '/api/presets/protect-main' && s === 204), JSON.stringify(signedRules));
await page.reload({ waitUntil: 'domcontentloaded' });
await page.waitForSelector(pm);
check('preset switch: stays on after a reload', (await page.$eval(pm, (b) => b.getAttribute('aria-checked'))) === 'true');
const dock = '#rule-ask-docker-push .switch';
await page.click(dock);
await sleep(500);
check('inbox rule: switched on with a signed enable', signedRules.some(([u, s]) => u === '/api/rules/ask-docker-push/enable' && s === 204) && (await page.$eval(dock, (b) => b.getAttribute('aria-checked'))) === 'true');
check('read-only rules have no switch', !(await page.$('#rule-self-protection .switch')) && !(await page.$('#rule-ask-before-push .switch')));
await page.click('#rule-allow-go-test .icon-btn');
await page.waitForSelector('dialog.sheet[open]');
await axeCheck(page, 'confirm sheet');
await page.click('dialog.sheet[open] .btn.danger');
await sleep(700);
check('inbox rule: removed after confirming', !(await page.$('#rule-allow-go-test')) && signedRules.some(([u, s]) => u === '/api/rules/allow-go-test/remove' && s === 204));
await page.click('.tester input');
await page.keyboard.type('git push --force origin main');
await press('Enter');
await page.waitForSelector('.verdict');
const v1 = await text(page, '.verdict');
check('tester: a force-push to main is blocked once Protect main is on', v1.includes('Blocked') && v1.includes('protect-main-force'), v1.replace(/\s+/g, ' '));
await page.click('.examples button:nth-child(2)');
await sleep(300);
const v2 = await text(page, '.verdict');
check('tester: examples run (npm test → allowed by guarded autopilot)', v2.includes('Allowed'), v2.replace(/\s+/g, ' '));
await shot(page, 'h-rules-tester-desktop-light');
await axeCheck(page, 'rules desktop');
await page.goto(demo.base + '/rules#rule-protect-prod', { waitUntil: 'domcontentloaded' });
await page.waitForSelector('#rule-protect-prod');
await sleep(300);
check('a card’s rule link lands on the rule', await page.$eval('#rule-protect-prod', (e) => e.classList.contains('is-selected')));

// ---------- Service worker ----------
const swOK = await page.evaluate(async () => {
  const reg = await navigator.serviceWorker.ready;
  const keys = await caches.keys();
  const c = await caches.open('pilothouse-shell-v2');
  const n = (await c.keys()).length;
  return { active: !!reg.active, keys, n };
});
check('service worker: active, shell precached', swOK.active && swOK.keys.includes('pilothouse-shell-v2') && swOK.n >= 30, JSON.stringify(swOK));
const ctx = browser.defaultBrowserContext();
await ctx.overridePermissions(demo.base, ['notifications']);
const cdp = await page.createCDPSession();
const regs = [];
cdp.on('ServiceWorker.workerRegistrationUpdated', (e) => regs.push(...e.registrations));
await cdp.send('ServiceWorker.enable');
await sleep(300);
const reg = regs.find((r) => r.scopeURL.startsWith(demo.base));
const push = (data) => cdp.send('ServiceWorker.deliverPushMessage', { origin: demo.base, registrationId: reg.registrationId, data: JSON.stringify(data) });
const notes = () => page.evaluate(async () => (await (await navigator.serviceWorker.ready).getNotifications()).map((n) => ({ tag: n.tag, title: n.title, body: n.body, actions: (n.actions || []).map((a) => a.action), data: n.data })));
if (reg) {
  await push({ kind: 'request', title: 'pilothouse wants to run git push', body: 'git push origin main', tag: 'req-r_x1', url: '/r/r_x1', request: { id: 'r_x1', checkpoint: 'permission', summary: 'Run: git push origin main' }, actions: true });
  await push({ kind: 'request', title: 'notes asks 2 questions', body: 'Which search index?', tag: 'req-r_x2', url: '/r/r_x2', request: { id: 'r_x2', checkpoint: 'question' }, actions: true });
  await sleep(600);
  let n = await notes();
  const perm = n.find((x) => x.tag === 'req-r_x1');
  const ques = n.find((x) => x.tag === 'req-r_x2');
  check('push: permission request shows Allow / Deny', perm && perm.actions.join() === 'allow,deny', JSON.stringify(perm));
  check('push: questions get no action buttons (they need the app)', ques && ques.actions.length === 0, JSON.stringify(ques));
  await push({ type: 'resolved', tag: 'req-r_x1', request_id: 'r_x1', status: 'decided', by: 'cli' });
  await sleep(600);
  n = await notes();
  const done = n.filter((x) => x.tag === 'req-r_x1');
  check('push: resolved replaces it — same tag, “Answered ✓”, no buttons', done.length === 1 && done[0].body.startsWith('Answered ✓') && done[0].body.includes('at the computer') && done[0].actions.length === 0 && done[0].title.includes('git push'), JSON.stringify(done));
  // The daemon's own wording (internal/web/push.go resolvedPayload) wins.
  await push({ type: 'request', kind: 'request', title: 'infra wants to edit', body: 'Edit terraform/prod/main.tf', tag: 'req-r_x5', request_id: 'r_x5', request: { id: 'r_x5', checkpoint: 'permission', summary: 'Edit' }, actions: true });
  await sleep(600);
  await push({ type: 'resolved', kind: 'resolved', title: '✓ Allowed', body: 'Edit terraform/prod/main.tf — from iPhone', tag: 'req-r_x5', url: '/r/r_x5', request_id: 'r_x5', status: 'decided', by: 'from iPhone' });
  await sleep(600);
  n = await notes();
  const f5 = n.filter((x) => x.tag === 'req-r_x5');
  check('push: the daemon’s resolved text is shown as written', f5.length === 1 && f5[0].title === '✓ Allowed' && f5[0].body.endsWith('from iPhone') && f5[0].actions.length === 0, JSON.stringify(f5));
  // 0.1 servers tag with the bare id and nest nothing.
  await push({ kind: 'request', title: 'old', body: 'x', tag: 'r_x3', request_id: 'r_x3', actions: true });
  await sleep(600);
  await push({ data: { type: 'resolved', request_id: 'r_x3', status: 'expired' } });
  await sleep(600);
  n = await notes();
  check('push: resolved finds a notification by request id and says what happened', n.some((x) => x.data && x.data.request_id === 'r_x3' && x.body.startsWith('Timed out')) && n.filter((x) => x.data && x.data.request_id === 'r_x3').length === 1, JSON.stringify(n.filter((x) => x.data && x.data.request_id === 'r_x3')));
} else {
  check('push: service worker registration found over CDP', false, JSON.stringify(regs));
}

check('no console or page errors', logs.length === 0, logs.slice(0, 4).join(' | '));
await demo.stop();
process.exit(summary());
