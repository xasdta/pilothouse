// 24. The 0.2 views at 390×844 and 1440×900, light and dark: screenshots,
// no horizontal scroll, no null/undefined, and axe-core (no serious or
// critical violations). Screenshots go to $PH_SHOTS.
// Run: PH_SHOTS=/some/dir node 24-screens-v02.mjs
import { run, startDevserver, launch, login, router, sseServer, check, sleep, watch, go, shot, axe, sanity, setSize, tap, SHOTS } from './v02-harness.mjs';
import * as F from './v02-fixtures.mjs';

const PHONE = { width: 390, height: 844, isMobile: true, hasTouch: true, deviceScaleFactor: 1 };
const DESKTOP = { width: 1440, height: 900, deviceScaleFactor: 1 };
const VIEWS = [
  ['session', '/s/s_web01', '.act-feed'],
  ['session-fallback', '/s/s_notes4', '.act-notice'],
  ['terminal', '/s/s_web01/terminal', '.xterm-rows'],
  ['tasks', '/tasks', '.task-row'],
  ['new-task', '/new', '.repo-opt input'],
  ['task', '/t/t_retry01', '.file-diff'],
  ['insights', '/insights', 'svg.cols .bar'],
];

await run(async () => {
  const dev = await startDevserver();
  const browser = await launch();
  const page = await browser.newPage();
  watch(page);
  const sse = await sseServer();
  const apiR = await router(page, sse);
  const w = F.world();
  F.mount(apiR, w);
  F.mountTerminal(apiR, sse, 's_web01');
  await page.setViewport(PHONE);
  await login(page, dev);
  const me = await page.evaluate(async () => (await (await fetch('/api/me')).json()).device.id);
  w.tasks[0].by = 'device:' + me;
  await page.evaluate(() => localStorage.setItem('pilothouse.visits', JSON.stringify({ s_web01: new Date(Date.now() - 55 * 60e3).toISOString() })));

  const shots = [];
  const violations = {};
  for (const [size, vp] of [['390x844', PHONE], ['1440x900', DESKTOP]]) {
    await setSize(page, vp);
    for (const scheme of ['light', 'dark']) {
      await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: scheme }]);
      for (const [name, path, ready] of VIEWS) {
        await go(page, path);
        await page.waitForSelector(ready, { timeout: 8000 }).catch(() => {});
        await sleep(name === 'terminal' ? 900 : 500);
        await page.evaluate(() => scrollTo(0, 0));
        const label = `${name} ${size} ${scheme}`;
        // Desktop: the viewport only. A full-page capture resizes the
        // viewport across the shell's 1024 px breakpoint, which re-renders
        // the view mid-capture (it comes out half faded in).
        if (name === 'terminal' || vp === DESKTOP) {
          const p = `${SHOTS}/i-${name}-${size}-${scheme}.png`;
          await page.screenshot({ path: p });
          shots.push(p);
        } else {
          shots.push(await shot(page, `i-${name}-${size}-${scheme}`));
        }
        await sanity(page, label);
        check(`${label}: no connection banner`, !(await page.$eval('.banner', (b) => b.classList.contains('show')).catch(() => false)));
      }
      if (size === '390x844') {
        // The ship confirmation, as a bottom sheet.
        await go(page, '/t/t_retry01');
        await page.waitForSelector('#task-ship', { timeout: 8000 });
        await tap(page, '#task-ship');
        await page.waitForSelector('dialog.sheet[open]');
        await sleep(350);
        const p = `${SHOTS}/i-ship-sheet-${size}-${scheme}.png`;
        await page.screenshot({ path: p });
        shots.push(p);
        await page.keyboard.press('Escape');
      }
    }
  }

  // axe at phone size, both schemes (it reloads each page with the CSP
  // bypassed for its own script).
  await setSize(page, PHONE);
  for (const scheme of ['light', 'dark']) {
    await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: scheme }]);
    for (const [name, path, ready] of VIEWS) {
      await go(page, path);
      await page.waitForSelector(ready, { timeout: 8000 }).catch(() => {});
      violations[`${name}-${scheme}`] = await axe(page);
    }
  }
  console.log('\n=== axe violations ===');
  for (const [k, v] of Object.entries(violations)) for (const x of v) console.log(`${k}: [${x.impact}] ${x.id} ×${x.n}  ${x.ex.join(' || ')}`);
  const severe = Object.entries(violations).flatMap(([k, v]) => v.filter((x) => x.impact === 'serious' || x.impact === 'critical').map((x) => `${k}:${x.id}`));
  check('axe: no serious/critical violations (7 views, light + dark)', Object.keys(violations).length === 14 && severe.length === 0, severe.join(', '));

  console.log('\nScreenshots:\n' + shots.join('\n'));
  const errs = page._logs.filter((l) => !/status of (404|501)/.test(l));
  check('no console errors', errs.length === 0, errs.join(' | ').slice(0, 300));
});
