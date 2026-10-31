// Captures the website's product screenshots from the demo (not a test).
// OUT=<dir> node site-shots.mjs, then cwebp -q 88 into site/src/assets/shots.
import { startDemo, login, goto, scheme, sleep, PHONE } from './demo-lib.mjs';
const OUT = process.env.OUT;
const demo = await startDemo();
const b = await demo.browser();
const plan = [
  ['desktop-today', { width: 1440, height: 900, deviceScaleFactor: 2 }, '/'],
  ['phone-today', PHONE, '/'],
  ['phone-report-proofline', PHONE, '/s/s_proofline'],
  ['phone-report-pilothouse', PHONE, '/s/s_pilothouse01'],
  ['phone-report-webapp', PHONE, '/s/s_webapp007'],
  ['phone-question', PHONE, '/r/r_question05'],
];
for (const [name, vp, path] of plan) {
  for (const s of ['dark', 'light']) {
    const page = await b.newPage();
    await page.setViewport(vp);
    await scheme(page, s);
    await login(page, demo, path);
    await sleep(1200);
    await page.screenshot({ path: `${OUT}/${name}-${s}.png` });
    await page.close();
  }
}
await demo.stop();
process.exit(0);
