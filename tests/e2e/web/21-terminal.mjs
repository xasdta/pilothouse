// 21. Live terminal (0.2): signed token, SSE stream (base64 data, exit,
// size), input with X-Terminal-Token, quick keys, resize on fit,
// reconnect with backoff, read-only fallbacks, CSP-clean xterm.
// Run: node 21-terminal.mjs (needs Go and Chrome; see v02-harness.mjs).
import { run, startDevserver, launch, login, router, sseServer, check, sleep, PHONE, watch, go, text, waitText, deviceKey, verifyAction, tap } from './v02-harness.mjs';
import * as F from './v02-fixtures.mjs';

const b64 = (s) => Buffer.from(s).toString('base64');
const rows = (page) => page.$eval('.xterm-rows', (e) => e.innerText).catch(() => '');
const waitRows = (page, needle, timeout = 6000) => page.waitForFunction(
  (n) => { const e = document.querySelector('.xterm-rows'); return e && e.innerText.includes(n); }, { timeout, polling: 100 }, needle,
).then(() => true, () => false);

await run(async () => {
  const dev = await startDevserver();
  const browser = await launch();
  const page = await browser.newPage();
  watch(page);
  const sse = await sseServer();
  const apiR = await router(page, sse);
  const w = F.world();
  F.mount(apiR, w);
  const web = F.mountTerminal(apiR, sse, 's_web01');
  const ro = F.mountTerminal(apiR, sse, 's_task05', { tokenStatus: 403, tokenError: 'typing is turned off for this session' });
  const deny = F.mountTerminal(apiR, sse, 's_api02', { inputStatus: 403 });
  const fixed = F.mountTerminal(apiR, sse, 's_notes4', { size: { cols: 120, rows: 30 } });
  await page.setViewport(PHONE);
  await login(page, dev);
  const spki = await deviceKey(page);

  // ---- Token, stream, render ----
  await go(page, '/s/s_web01/terminal');
  check('stream renders the replay', await waitRows(page, 'Welcome to'));
  const tokenCall = apiR.find('POST', /\/terminal\/token$/)[0];
  const v = tokenCall ? verifyAction(spki, 'terminal', 's_web01', tokenCall.body) : { ok: false, why: 'no token call' };
  check('token minted with a signed `terminal` action, payload {}', v.ok && JSON.stringify(v.payload) === '{}', v.why || JSON.stringify(v.payload));
  check('stream opened with replay=1', sse.connections('/term/s_web01').some((c) => c.search === '?replay=1'));
  check('status Live, typing on', (await text(page, '.term-bar')).includes('Live') && (await text(page, '.term-bar')).includes('You can type'));
  const orange = await page.evaluate(() => {
    const span = [...document.querySelectorAll('.xterm-rows span')].find((s) => s.textContent.includes('✻'));
    return span ? getComputedStyle(span).color : 'none';
  });
  check('truecolor cells keep their colour under the CSP (CSSOM shim)', orange === 'rgb(215, 119, 87)', orange);
  const red = await page.evaluate(() => {
    const span = [...document.querySelectorAll('.xterm-rows span')].find((s) => s.textContent === 'FAIL');
    return span ? getComputedStyle(span).color : 'none';
  });
  check('palette colours apply (adopted theme stylesheet)', red === 'rgb(255, 123, 114)', red); // the view's ANSI red
  check('no inline <style> left for the CSP to block', (await page.$$eval('.term-screen style', (s) => s.length)) === 0);
  check('no CSP violations with xterm open', (await page.evaluate(() => window.__csp.length)) === 0, (await page.evaluate(() => window.__csp.join(' | '))).slice(0, 200));

  // ---- Live output ----
  sse.push('/term/s_web01', 'data', b64('\r\n\x1b[1mhello from the agent\x1b[0m'));
  check('live output appears', await waitRows(page, 'hello from the agent'));

  // ---- Resize on fit ----
  await page.waitForFunction(() => true);
  await sleep(500);
  const r1 = web.inputs.filter((i) => i.resize);
  check('size sent after the token (fit to screen)', r1.length >= 1 && r1[0].resize.cols > 20 && r1[0].resize.rows > 10, JSON.stringify(r1[0]));
  await page.setViewport({ ...PHONE, width: 700, height: 800 });
  await sleep(900);
  const r2 = web.inputs.filter((i) => i.resize);
  check('resized again when the screen grows', r2.length >= 2 && r2[r2.length - 1].resize.cols > r1[0].resize.cols, JSON.stringify(r2.map((x) => x.resize)));
  await page.setViewport(PHONE);
  await sleep(400);

  // ---- Typing ----
  web.inputs.length = 0;
  await tap(page, '.term-screen');
  await page.keyboard.type('ls -la');
  await page.keyboard.press('Enter');
  await sleep(700);
  const typed = web.inputs.filter((i) => i.text !== undefined).map((i) => i.text).join('');
  check('typed keys are posted (batched)', typed === 'ls -la\r', JSON.stringify(typed));
  check('posts carry X-Terminal-Token and the CSRF header', web.inputs.length > 0 && web.inputs.every((i) => /^tok-1-/.test(i.token) && i.csrf === '1'));
  check('one token for many keystrokes', web.tokens === 1, String(web.tokens));
  check('fewer posts than keys (batched)', web.inputs.filter((i) => i.text !== undefined).length < 7, String(web.inputs.length));

  // ---- Quick keys ----
  const keyText = async (sel) => {
    web.inputs.length = 0;
    await tap(page, sel);
    await sleep(250);
    return web.inputs.map((i) => i.text).join('');
  };
  check('keypad visible on phones', await page.$eval('.term-keys', (e) => !e.hidden && e.getBoundingClientRect().height > 40));
  check('Esc → \\x1b', (await keyText('.k-esc')) === '\x1b');
  check('Ctrl-C → \\x03', (await keyText('.k-ctrlc')) === '\x03');
  check('Tab → \\t', (await keyText('.k-tab')) === '\t');
  check('↑ → CSI A', (await keyText('.k-up')) === '\x1b[A');
  check('← → CSI D', (await keyText('.k-left')) === '\x1b[D');
  check('Enter → \\r', (await keyText('.k-enter')) === '\r');
  check('y and n', (await keyText('.k-y')) === 'y' && (await keyText('.k-n')) === 'n');
  sse.push('/term/s_web01', 'data', b64('\x1b[?1h')); // application cursor keys (vim, less)
  await sleep(200);
  check('↓ follows application cursor mode → SS3 B', (await keyText('.k-down')) === '\x1bOB');
  check('quick keys keep focus in the terminal (keyboard stays up)', await page.evaluate(() => document.activeElement && document.activeElement.classList.contains('xterm-helper-textarea')));

  // ---- Reconnect with backoff ----
  const before = sse.connections('/term/s_web01').length;
  sse.fail('/term/s_web01', 503, { error: 'busy' });
  sse.end('/term/s_web01');
  check('drop → Reconnecting', await waitText(page, 'Reconnecting', 3000, '.term-status'));
  await sleep(4500);
  const tries = sse.connections('/term/s_web01').slice(before).map((c) => c.at);
  const gaps = tries.slice(1).map((t, i) => t - tries[i]);
  check('retries back off (each wait longer)', tries.length >= 3 && gaps.every((g, i) => i === 0 || g > gaps[i - 1] * 1.2), `${tries.length} tries, gaps ${gaps.join(',')} ms`);
  sse.reset('/term/s_web01');
  check('back to Live after the server recovers', await waitText(page, 'Live', 9000, '.term-status'));
  await waitRows(page, 'Welcome to');
  const count = (await rows(page)).split('Welcome to').length - 1;
  check('reconnect redraws from the replay (no duplicate screen)', count === 1, `${count} copies`);

  // ---- Exit ----
  sse.push('/term/s_web01', 'exit', { code: 0 });
  check('exit → Exited · code 0', await waitText(page, 'Exited · code 0', 3000, '.term-status'));
  check('exit → banner with a link to the session', (await text(page, '.term-banner')).includes('The session ended') && !!(await page.$('.term-banner a[href="/s/s_web01"]')));
  check('exit → keypad hidden', await page.$eval('.term-keys', (e) => e.hidden));
  const n = sse.connections('/term/s_web01').length;
  await sleep(2500);
  check('no reconnect after exit', sse.connections('/term/s_web01').length === n);

  // ---- Read-only: the token is refused ----
  await go(page, '/s/s_task05/terminal');
  check('read-only session still streams', await waitRows(page, 'Welcome to'));
  check('refused token → Watching, with the reason', await waitText(page, 'Watching', 4000, '.term-bar') && (await text(page, '.term-banner')).includes('Typing is turned off for this session.'), await text(page, '.term-banner'));
  check('read-only → keypad hidden', await page.$eval('.term-keys', (e) => e.hidden));
  ro.inputs.length = 0;
  await tap(page, '.term-screen');
  await page.keyboard.type('nope');
  await sleep(500);
  check('read-only → keys are not sent', ro.inputs.length === 0, String(ro.inputs.length));

  // ---- Read-only: input refused ----
  await go(page, '/s/s_api02/terminal');
  await waitRows(page, 'Welcome to');
  await sleep(500);
  await tap(page, '.term-screen');
  await page.keyboard.type('x');
  check('refused input → re-mints once, then Watching', await waitText(page, 'Watching', 4000, '.term-bar'));
  check('refused input → token re-minted once', deny.tokens === 2, String(deny.tokens));

  // ---- Fixed size from the computer ----
  await go(page, '/s/s_notes4/terminal');
  await waitRows(page, 'Welcome to');
  await sleep(600);
  const geo = await page.evaluate(() => {
    const f = document.querySelector('.term-frame');
    const row = document.querySelector('.xterm-rows > div');
    return { fixed: f.classList.contains('fixed'), frame: f.clientWidth, screen: document.querySelector('.xterm-screen').getBoundingClientRect().width, rows: document.querySelectorAll('.xterm-rows > div').length, row: row && row.textContent.length };
  });
  check('size event → the computer’s 120×30 is kept', geo.fixed && geo.rows === 30, JSON.stringify(geo));
  check('…with the text shrunk to fit the phone’s width', geo.screen <= geo.frame + 2 || geo.screen < geo.frame * 1.6, JSON.stringify(geo));
  check('fixed size → no resize posted', fixed.inputs.every((i) => !i.resize), JSON.stringify(fixed.inputs.slice(0, 3)));

  // ---- Not available (devserver: 501) ----
  await go(page, '/s/s_site03/terminal');
  check('501 → "isn’t available" with a way back', await waitText(page, 'The live terminal isn’t available', 6000) && !!(await page.$('.term-empty a[href="/s/s_site03"]')));

  const errs = page._logs.filter((l) => !/status of (40[34]|501|503)/.test(l));
  check('no console errors', errs.length === 0, errs.join(' | ').slice(0, 300));
  check('no CSP violations', (await page.evaluate(() => window.__csp.length)) === 0);
});
