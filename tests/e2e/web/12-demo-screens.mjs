// 12. Screens on the demo core: every main page at 390×844 and 1440×900,
// light and dark; no horizontal scroll, no literal "null", no console
// errors; axe on every page in both themes.
//
//   PH_TMP=… PH_SHOTS=… node 12-demo-screens.mjs [route-filter]
import { startDemo, login, goto, watch, scheme, shot, check, summary, sleep, axeCheck, PHONE, DESKTOP, text } from './demo-lib.mjs';

const only = process.argv[2] || '';
const demo = await startDemo();
const browser = await demo.browser();
const page = await browser.newPage();
const logs = watch(page, 'screens');
await page.setViewport(PHONE);
await login(page, demo);

const routes = [
  ['inbox', '/'],
  ['request-plan', '/r/r_plan0006'],
  ['request-question', '/r/r_question05'],
  ['request-diff', '/r/r_tfedit04'],
  ['rules', '/rules'],
  ['settings', '/settings'],
].filter(([n]) => !only || n.includes(only));

for (const s of ['light', 'dark']) {
  await scheme(page, s);
  for (const [vn, vp] of [['phone', PHONE], ['desktop', DESKTOP]]) {
    await page.setViewport(vp);
    for (const [rn, path] of routes) {
      await goto(page, demo, path);
      await sleep(rn === 'rules' ? 700 : 350);
      await shot(page, `h-${rn}-${vn}-${s}`);
      const wide = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth + 1);
      check(`no horizontal scroll: ${rn} ${vn} ${s}`, !wide, wide ? 'scrollWidth=' + (await page.evaluate(() => document.documentElement.scrollWidth)) : '');
      const nul = (await text(page, 'main')).match(/.{0,30}\b(null|undefined|NaN)\b.{0,20}/g);
      check(`no literal null/undefined: ${rn} ${vn} ${s}`, !nul, nul ? nul.join(' | ') : '');
      if (vn === 'phone' || rn === 'inbox') await axeCheck(page, `${rn} ${vn} ${s}`);
    }
  }
}
// Long page once, for review.
await scheme(page, 'light');
await page.setViewport(PHONE);
await goto(page, demo, '/');
await shot(page, 'h-inbox-phone-light-full', { full: true });

check('no console errors or page errors', logs.length === 0, logs.slice(0, 5).join(' | '));
await demo.stop();
process.exit(summary());
