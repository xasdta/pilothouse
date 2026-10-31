// 13. The lively demo (what `pilothouse demo` runs): agents keep working,
// a new request arrives on its own within ~30 s, and answering it makes
// the agent carry on. Also checks the narrowest desktop layout (1024 px).
//
//   PH_TMP=… PH_SHOTS=… node 13-demo-live.mjs
import { startDemo, login, watch, check, summary, sleep, shot, scheme } from './demo-lib.mjs';

const demo = await startDemo({ live: true });
const browser = await demo.browser();
const page = await browser.newPage();
const logs = watch(page, 'live');
await scheme(page, 'light');
await page.setViewport({ width: 1024, height: 768 });
await login(page, demo);

const centre = await page.$eval('main', (m) => Math.round(m.getBoundingClientRect().width));
check('1024 px: the centre keeps a usable width', centre >= 420, `${centre}px`);
const before = await page.$$eval('.queue article.req', (els) => els.map((e) => e.dataset.request));
const feedBefore = await page.evaluate(() => fetch('/api/sessions/s_apisrv003/activity?limit=1').then((r) => r.json()));

const fresh = await page.waitForFunction((old) => {
  const ids = [...document.querySelectorAll('.queue article.req')].map((e) => e.dataset.request);
  return ids.find((id) => !old.includes(id)) || false;
}, { timeout: 45000, polling: 250 }, before).then((h) => h.jsonValue(), () => '');
check('a new request arrives on its own', !!fresh, fresh);
await shot(page, 'h-live-1024-light');

const feedAfter = await page.evaluate(() => fetch('/api/sessions/s_apisrv003/activity?limit=1').then((r) => r.json()));
check('working agents keep adding activity', feedAfter.cursor !== feedBefore.cursor, `${feedBefore.cursor} → ${feedAfter.cursor}`);

if (fresh) {
  const r = await page.evaluate((id) => fetch('/api/requests/' + id).then((x) => x.json()), fresh);
  const c = await page.$(`.queue article[data-request="${fresh}"]`);
  await c.evaluate((el) => el.scrollIntoView({ block: 'center' }));
  if (r.checkpoint === 'permission') {
    // Permission cards allow on press-and-hold.
    const hold = await c.$('.btn.hold');
    const b = await hold.boundingBox();
    await page.mouse.move(b.x + b.width / 2, b.y + b.height / 2);
    await page.mouse.down();
    await sleep(1300);
    await page.mouse.up();
    await sleep(3500);
    const s = await page.evaluate((sid) => fetch('/api/sessions/' + sid).then((x) => x.json()), r.session_id);
    const act = await page.evaluate((sid) => fetch('/api/sessions/' + sid + '/activity?limit=3').then((x) => x.json()), r.session_id);
    check('after Allow the agent carries on', s.state === 'working' && act.items.some((i) => i.kind === 'text' || (i.kind === 'tool' && !i.pending)), `${s.state} ${JSON.stringify(act.items.map((i) => i.kind + ':' + (i.summary || i.text || '').slice(0, 30)))}`);
  }
}
check('no console or page errors', logs.length === 0, logs.slice(0, 4).join(' | '));
await demo.stop();
process.exit(summary());
