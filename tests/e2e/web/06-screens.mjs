// 6. Screens (mobile/desktop × light/dark), settings flows, a11y (axe).
import fs from 'node:fs';
import { spawnSync } from 'node:child_process';
import { BASE, QA, launch, watch, pairViaUI, shot, text, waitText, check, summary, holdJSON, sleep, sh } from './lib.mjs';

const RUN = Date.now().toString(36);
const FIX = new URL('../../../internal/adapters/claude/testdata/hooks', import.meta.url).pathname;
const SID = `55555555-0000-0000-0000-${RUN.padStart(12, '0').slice(-12)}`;
const CWD = '/tmp/qa2/uhome/webapp';
fs.mkdirSync(CWD, { recursive: true });
function fixture(name, over = {}) {
  let s = fs.readFileSync(`${FIX}/${name}.json`, 'utf8').replaceAll('/home/user/project', CWD).replaceAll('/home/user', '/tmp/qa2/uhome');
  const j = JSON.parse(s);
  return { ...j, session_id: SID, cwd: CWD, ...over };
}
function fire(payload) {
  const r = spawnSync('bash', ['-c', `source ${QA}/env.sh; $PH hook claude`], { input: JSON.stringify(payload), encoding: 'utf8', timeout: 8000 });
  return r.stdout;
}

// Seed a session with a timeline.
fire(fixture('session_start'));
fire(fixture('user_prompt_submit'));
for (const f of ['pre_tool_bash', 'post_tool_bash', 'pre_tool_write', 'pre_tool_edit', 'post_tool_edit', 'post_tool_failure_read']) {
  try { fire(fixture(f)); } catch { /* */ }
}
fire(fixture('stop'));
// Open requests: an MCP tool and a Bash command, in another session.
const SID2 = SID.replace(/^55/, '66');
holdJSON('mcp', fixture('doc_permission_mcp', { session_id: SID2 }));
holdJSON('bash6', { ...fixture('permission_bash', { session_id: SID2 }), tool_input: { command: `git push origin feature/very-long-branch-name-${RUN} --set-upstream && npm run build && npm test -- --coverage`, description: 'push' } });
await sleep(1500);
const snap = JSON.parse(sh('curl -s --unix-socket /tmp/qa2/run/pilothouse.sock http://x/v1/snapshot'));
const sess = snap.sessions.find((s) => s.agent_session_id === SID);
const reqMcp = snap.requests.find((r) => r.status === 'open' && r.tool_kind === 'mcp');
check('seeded session + MCP request', !!sess && !!reqMcp, `${sess && sess.id} ${reqMcp && reqMcp.id}`);

const browser = await launch(); // bfcache on: the app closes its stream on pagehide (W3)
const ctx = await browser.createBrowserContext();
const page = await ctx.newPage();
watch(page, 'P');
page.on('dialog', (d) => d.accept());
await page.setViewport({ width: 390, height: 844, isMobile: true, hasTouch: true, deviceScaleFactor: 2 });
const pr = await pairViaUI(page, 'Screens phone');
check('paired', pr.status === 200);
const doneTxt = await text(page);
console.log('pair-done text:', doneTxt.replace(/\s+/g, ' ').slice(0, 160));
check('pair success uses configured machine name', doneTxt.includes(snap.machine), `snapshot.machine=${snap.machine}`);

const routes = [['inbox', '/'], ['sessions', '/sessions'], ['session', `/s/${sess.id}`], ['request', `/r/${reqMcp.id}`], ['settings', '/settings']];
const views = [['mobile', { width: 390, height: 844, isMobile: true, hasTouch: true, deviceScaleFactor: 2 }], ['desktop', { width: 1280, height: 800, deviceScaleFactor: 1 }]];
const axeSrc = fs.readFileSync(new URL('./node_modules/axe-core/axe.min.js', import.meta.url), 'utf8');
const axeAll = {};
for (const scheme of ['light', 'dark']) {
  await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: scheme }]);
  for (const [vn, vp] of views) {
    await page.setViewport(vp);
    for (const [rn, path] of routes) {
      try { await page.goto(BASE + path, { waitUntil: 'domcontentloaded', timeout: 10000 }); }
      catch (e) { console.log(`goto ${rn} ${vn} ${scheme} failed: ${e.message}; url=${page.url()}`); check(`navigate ${rn} ${vn} ${scheme}`, false, e.message); continue; }
      await page.waitForSelector('main .view', { timeout: 8000 }).catch(() => {});
      await sleep(rn === 'session' ? 1800 : 1200);
      await shot(page, `06-${rn}-${vn}-${scheme}`);
      const hscroll = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth + 1);
      if (hscroll) check(`no horizontal scroll ${rn} ${vn} ${scheme}`, false, `scrollWidth=${await page.evaluate(() => document.documentElement.scrollWidth)}`);
      const nul = (await text(page, 'main')).replace(/\n/g, ' ⏎ ').match(/.{0,40}null.{0,20}/g);
      if (nul) check(`no literal "null" in ${rn} ${vn} ${scheme}`, false, nul.join(' | '));
      if (scheme === 'light' && vn === 'mobile' || scheme === 'dark' && vn === 'mobile') {
        // axe needs the page CSP bypassed for its own script only.
        await page.setBypassCSP(true);
        await page.reload({ waitUntil: 'domcontentloaded', timeout: 10000 }).catch((e) => console.log('reload fail', rn, e.message));
        await page.waitForSelector('main .view', { timeout: 8000 }).catch(() => {});
        await sleep(1200);
        await page.addScriptTag({ content: axeSrc });
        const res = await page.evaluate(async () => {
          const r = await axe.run(document, { resultTypes: ['violations'] });
          return r.violations.map((v) => ({ id: v.id, impact: v.impact, n: v.nodes.length, ex: v.nodes.slice(0, 2).map((n) => n.target.join(' ') + ' :: ' + (n.failureSummary || '').split('\n')[1]) }));
        });
        axeAll[`${rn}-${scheme}`] = res;
        await page.setBypassCSP(false);
      }
    }
  }
}
console.log('\n=== axe violations ===');
for (const [k, v] of Object.entries(axeAll)) for (const x of v) console.log(`${k}: [${x.impact}] ${x.id} ×${x.n}  ${x.ex.join(' || ')}`);
const severe = Object.entries(axeAll).flatMap(([k, v]) => v.filter((x) => x.impact === 'serious' || x.impact === 'critical').map((x) => `${k}:${x.id}`));
check('axe: no serious/critical violations (all screens, light + dark)', Object.keys(axeAll).length > 0 && severe.length === 0, severe.join(', '));

// ---- Settings: rename ----
await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: 'light' }]);
await page.setViewport(views[0][1]);
await page.goto(BASE + '/settings', { waitUntil: 'domcontentloaded' });
await page.waitForSelector('#device-name');
await page.$eval('#device-name', (e) => { e.value = ''; });
await page.type('#device-name', 'Renamed QA phone');
const ren = page.waitForResponse((r) => r.url().endsWith('/api/device'));
await page.click('.name-form button');
const rr = await ren;
check('rename PATCH 200', rr.status() === 200);
const dbName = sh(`sqlite3 /tmp/qa2/home/pilothouse.db "select name from web_devices where name like 'Renamed%' and revoked_at is null"`).trim();
check('rename persisted in daemon DB', dbName === 'Renamed QA phone', dbName);
check('toast "Renamed"', await waitText(page, 'Renamed', 2000, '.toast'));
// Long name, control chars.
const weird = await page.evaluate(async () => (await (await fetch('/api/device', { method: 'PATCH', headers: { 'X-Pilothouse': '1', 'Content-Type': 'application/json' }, body: JSON.stringify({ name: 'A\u0007B' + 'x'.repeat(100) + '<img src=x>' }) })).json()));
check('rename strips control chars + caps 60 runes', weird.name && !weird.name.includes('\u0007') && [...weird.name].length <= 60, JSON.stringify(weird.name));

// ---- Toast without kind (via push off) ----
const bcdp = await browser.target().createCDPSession();
await bcdp.send('Browser.grantPermissions', { origin: BASE, permissions: ['notifications'], browserContextId: ctx.id });
await page.reload({ waitUntil: 'domcontentloaded' });
await sleep(1500);
const onBtn = await page.$('xpath///button[contains(., "Turn on notifications")]');
if (onBtn) {
  await onBtn.click();
  await waitText(page, 'Notifications are on', 60000);
  const off = await page.waitForSelector('xpath///button[contains(., "Turn off")]', { timeout: 5000 }).catch(() => null);
  if (off) {
    await off.click();
    await sleep(800);
    const t = await text(page, '.toast');
    check('toast "Notifications are off" has no literal null', !t.includes('null'), JSON.stringify(t));
    await shot(page, '06-toast-off');
  }
}

// ---- Sign out revokes ----
const p2 = await ctx.newPage(); // same device, second tab
await p2.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
await sleep(1500);
await page.bringToFront();
await page.goto(BASE + '/settings', { waitUntil: 'domcontentloaded' });
await sleep(1200);
const myId = await page.evaluate(async () => (await (await fetch('/api/me')).json()).device.id);
const so = await page.$('xpath///button[contains(., "Sign out this device")]');
await so.click();
await page.waitForFunction(() => location.pathname === '/pair', { timeout: 8000 }).catch(() => {});
await sleep(1000);
check('sign out → /pair', new URL(page.url()).pathname === '/pair', page.url());
const after = await page.evaluate(async () => ({ me: (await fetch('/api/me')).status, key: !!(await self.PilothouseKeys.load()) }));
check('after sign out /api/me 401', after.me === 401, String(after.me));
check('after sign out signing key cleared from IndexedDB', after.key === false);
const rev = sh(`sqlite3 /tmp/qa2/home/pilothouse.db "select count(*) from web_devices where id='${myId}' and revoked_at is not null"`).trim();
check('device revoked in DB', rev === '1', rev);
await p2.bringToFront();
await sleep(3000);
check('other tab of revoked device goes to /pair', new URL(p2.url()).pathname === '/pair', p2.url());
await shot(page, '06-after-signout');

for (const l of page._logs) if (!/status of 401/.test(l)) console.log(l);
await browser.close();
process.exit(summary());
