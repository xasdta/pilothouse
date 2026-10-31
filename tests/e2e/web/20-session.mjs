// 20. Session page (0.2): header, caps-driven actions, open requests,
// catch-me-up, the activity feed (turns, tool kinds, results, errors,
// subagents, pending), cursor polling and pagination, re-fetch on
// session.updated, signed reply and interrupt, graceful fallbacks.
// Run: node 20-session.mjs (needs Go and Chrome; see v02-harness.mjs).
import { run, startDevserver, launch, login, router, sseServer, check, sleep, PHONE, watch, go, text, waitText, deviceKey, verifyAction, tap } from './v02-harness.mjs';
import * as F from './v02-fixtures.mjs';

await run(async () => {
  const dev = await startDevserver();
  const browser = await launch();
  const page = await browser.newPage();
  watch(page);
  const sse = await sseServer();
  const apiR = await router(page, sse);
  const w = F.world();
  const TOTAL = 450;
  F.mount(apiR, w, {
    activity: {
      // A long history: 15 items to a turn; only the newest `limit` come back.
      s_api02: (url) => {
        if (url.searchParams.get('cursor')) return { json: { session_id: 's_api02', available: true, items: [], cursor: 'L' + TOTAL } };
        const limit = Number(url.searchParams.get('limit')) || 200;
        return { json: F.activityLong(w, Math.min(limit, TOTAL), 's_api02', 15, TOTAL) };
      },
    },
  });
  await page.setViewport(PHONE);
  await login(page, dev);
  const spki = await deviceKey(page);
  check('device has a signing key', !!spki);

  // A last visit 55 minutes ago: the catch-up starts there.
  const lastSeen = new Date(Date.now() - 55 * 60e3).toISOString();
  await page.evaluate((v) => localStorage.setItem('pilothouse.visits', JSON.stringify({ s_web01: v })), lastSeen);
  const t0 = Date.now();
  await go(page, '/s/s_web01');
  await page.waitForSelector('.act-feed', { timeout: 8000 });

  // ---- First load ----
  // Today's receipts read the same endpoint with peek=1; skip those.
  const first = apiR.find('GET', /\/api\/sessions\/s_web01\/activity$/).filter((r) => r.query.peek !== '1')[0];
  check('activity fetched with limit=200', first && first.query.limit === '200', JSON.stringify(first && first.query));
  check('activity fetched since the last visit (localStorage)', first && first.query.since === lastSeen, first && first.query.since);
  const head = await text(page, '.rhead');
  check('header: live state', head.includes('working'));
  check('header: estimated cost', head.includes('$2.37'), head.replace(/\s+/g, ' ').slice(0, 160));
  await tap(page, 'details.rdetails > summary');
  check('details: usage chip with tokens', /1\.2M\s+tokens/.test(await text(page, '.rdetails')));
  await tap(page, '.usage-chip');
  const usagePanel = await text(page, '.usage-panel');
  check('usage chip opens a breakdown (input, cache read, model)', usagePanel.includes('Input') && usagePanel.includes('Cache read') && usagePanel.includes('claude-sonnet-4-5'), usagePanel.replace(/\s+/g, ' '));
  check('caps.watch → Open terminal', !!(await page.$('a[href="/s/s_web01/terminal"]')));
  check('caps.interrupt → Interrupt', !!(await page.$('#interrupt-btn')));
  check('caps.reply → reply box', !!(await page.$('#reply-text')));
  check('reply note says how it is delivered (terminal)', (await text(page, '#reply-note')).includes('terminal'));
  check('open request shown with the request card', (await text(page, '.sess')).includes('git push origin feat/session-page') && !!(await page.$('.sess article.req')));

  // ---- The report: you asked, it says ----
  check('report: “You asked” is the latest prompt', (await text(page, '.goalq')).includes('Now run the whole Go suite'));
  check('report: the agent’s last words under “… says”', !!(await page.$('.md.says')));

  // ---- How it got there ----
  check('voyage: the latest turn, with earlier turns folded below', !!(await page.$('.voyage-wrap .voyage')) && (await page.$$('.earlier > details.eturn')).length === 3, String((await page.$$('.earlier > details.eturn')).length));
  check('tool kinds have their own nodes (shell, mcp)', !!(await page.$('.voyage-wrap .vs.k-shell')) && !!(await page.$('.voyage-wrap .vs.k-mcp')));
  check('pending tool call shows a spinner', !!(await page.$('.voyage-wrap .vs.pending .spin')));
  check('agent working → its step in plain words', (await text(page, '.voyage-wrap')).includes('Running the tests'));
  // Expand a result.
  await tap(page, '.voyage-wrap .vs.k-mcp .vbtn');
  check('tool result expands', (await text(page, '.voyage-wrap .vs.k-mcp .out')).includes('#212'));
  // Older turn: errors, subagents, system.
  const turn3 = (await page.$$('.earlier > details.eturn'))[0];
  check('folded turn summary: prompt, steps and failures', (await turn3.evaluate((e) => e.querySelector('summary').innerText)).includes('Run the e2e suite') && (await turn3.evaluate((e) => e.querySelector('summary').innerText)).includes('2 failed'));
  await tap(page, await turn3.$('summary'));
  await sleep(150);
  const t3 = await turn3.evaluate((e) => e.innerText);
  check('failed tool call styled as a failure', !!(await turn3.$('.vs.fail')));
  check('failed tool call shows its command', t3.includes('node tests/e2e/web/20-session.mjs'));
  check('API error shown on its own', t3.includes('Overloaded'));
  check('subagent run grouped', !!(await turn3.$('.vs.sub')) && t3.includes('helper agent') && t3.includes('2 steps'));
  check('finished subagent group starts folded', !t3.includes('Find tests/e2e/web/*.mjs'));
  await tap(page, await turn3.$('.vs.sub .vbtn'));
  await sleep(150);
  const sub = await page.$eval('.earlier > details.eturn .vs.sub', (e) => e.innerText);
  check('subagent group opens to its steps and finish line', sub.includes('tests/e2e/web/*.mjs') && sub.includes('Explore finished'), sub.replace(/\s+/g, ' ').slice(0, 160));
  check('system line shown', (await page.$eval('.earlier > details.eturn', (e) => e.innerText)).includes('Context compacted'));

  // ---- Files changed ----
  check('files changed: 4 rows', (await page.$$eval('.file-row', (r) => r.length)) === 4);
  const files = await text(page, '.files');
  check('files changed: +/- per file, relative paths', files.includes('+412') && files.includes('−96') && files.includes('session.js') && !files.includes('/Users/dev'), files.replace(/\s+/g, ' ').slice(0, 120));

  // ---- Polling with the cursor ----
  const polled = await page.waitForFunction(() => !document.querySelector('.voyage-wrap .vs.pending'), { timeout: 8000 }).then(() => true, () => false);
  const next = apiR.find('GET', /\/api\/sessions\/s_web01\/activity$/).find((c) => c.query.cursor === 'c1');
  check('polls with the cursor while visible', !!next && !next.query.limit, JSON.stringify(next && next.query));
  check('a finished call replaces its pending row (same id)', polled && (await page.$$eval('.voyage-wrap .vs.k-shell', (els) => els.length)) >= 1);
  check('new items appended to the latest turn', await waitText(page, 'All green. Pushing feat/session-page now.', 3000));
  check('cost follows the poll', (await text(page, '.rhead')).includes('$2.41'));

  // ---- Reply (signed) ----
  apiR.clear();
  await page.type('#reply-text', 'Also run the linter before you push.');
  await tap(page, '.reply-send');
  await waitText(page, 'from this device', 3000);
  let call = apiR.find('POST', /\/api\/sessions\/s_web01\/reply$/)[0];
  let v = call ? verifyAction(spki, 'reply', 's_web01', call.body) : { ok: false, why: 'no call' };
  check('reply is a signed `reply` action for the session', v.ok, v.why);
  check('reply payload is {text}', v.payload && v.payload.text === 'Also run the linter before you push.' && !v.payload.interrupt, JSON.stringify(v.payload));
  check('reply has the CSRF header', call && call.headers['x-pilothouse'] === '1');
  check('reply box cleared and toast shown', (await page.$eval('#reply-text', (e) => e.value)) === '' && (await text(page, '.toast')).includes('Sent'));
  check('sent reply shown in the feed until the agent picks it up', (await text(page, '.act-turn.latest')).startsWith('You'));
  // Stop first.
  apiR.clear();
  await page.type('#reply-text', 'Stop — use the staging database instead.');
  await tap(page, '#reply-stop');
  await page.keyboard.down('Meta');
  await page.focus('#reply-text');
  await page.keyboard.press('Enter');
  await page.keyboard.up('Meta');
  await sleep(800);
  call = apiR.find('POST', /\/api\/sessions\/s_web01\/reply$/)[0];
  v = call ? verifyAction(spki, 'reply', 's_web01', call.body) : { ok: false, why: 'no call (⌘↵)' };
  check('⌘↵ sends; "stop first" adds interrupt:true', v.ok && v.payload.interrupt === true && v.payload.text.startsWith('Stop'), JSON.stringify(v.payload || v.why));
  // A refused reply keeps the text.
  apiR.on('POST', /^\/api\/sessions\/s_web01\/reply$/, () => ({ status: 403, json: { error: 'signature timestamp is too far from the computer’s clock — check the time on this device', code: 'stale_signature' } }));
  await page.type('#reply-text', 'keep me');
  await tap(page, '.reply-send');
  check('refused reply: clock hint shown, text kept', await waitText(page, 'clock is off', 3000, '.toast') && (await page.$eval('#reply-text', (e) => e.value)) === 'keep me');
  apiR.on('POST', /^\/api\/sessions\/s_web01\/reply$/, () => ({ json: { via: 'terminal' } }));
  await page.$eval('#reply-text', (e) => { e.value = ''; e.dispatchEvent(new Event('input')); });

  // ---- The `r` shortcut focuses the reply box ----
  await page.evaluate(() => { if (document.activeElement) document.activeElement.blur(); scrollTo(0, 0); });
  await page.keyboard.press('r');
  await sleep(150);
  check('`r` focuses the reply box', await page.evaluate(() => document.activeElement && document.activeElement.id === 'reply-text' && document.activeElement.dataset.shortcut === 'reply'));
  check('…without typing the r', (await page.$eval('#reply-text', (e) => e.value)) === '');
  await page.evaluate(() => document.activeElement.blur());

  // ---- Interrupt (signed, two taps) ----
  apiR.clear();
  await tap(page, '#interrupt-btn');
  check('interrupt asks for a second tap', (await text(page, '#interrupt-btn')).includes('Tap again'));
  check('first tap sends nothing', apiR.find('POST', /reply$/).length === 0);
  await tap(page, '#interrupt-btn');
  await sleep(600);
  call = apiR.find('POST', /\/api\/sessions\/s_web01\/reply$/)[0];
  v = call ? verifyAction(spki, 'reply', 's_web01', call.body) : { ok: false, why: 'no call' };
  check('second tap sends a signed interrupt', v.ok && v.payload.interrupt === true && !v.payload.text, JSON.stringify(v.payload || v.why));

  // ---- Re-fetch on session.updated ----
  const s = w.sessions.find((x) => x.id === 's_web01');
  const waiting = { ...s, state: 'waiting', state_detail: '', last_message: 'Pushed. Anything else?', updated_at: new Date().toISOString() };
  await sleep(400);
  apiR.clear();
  const pushedAt = Date.now();
  sse.push('/events', 'event', { seq: 1e9 + 1, at: new Date().toISOString(), type: 'session.updated', session_id: 's_web01', data: waiting });
  const refetched = await page.waitForFunction(() => document.querySelector('.rhead') && document.querySelector('.rhead').innerText.includes('finished'), { timeout: 3000 }).then(() => true, () => false);
  check('session.updated updates the live state', refetched);
  await sleep(700);
  const after = apiR.find('GET', /\/api\/sessions\/s_web01\/activity$/);
  check('session.updated re-fetches activity at once (< 700 ms)', after.length > 0 && after[0].at - pushedAt < 700, after.length ? `${after[0].at - pushedAt} ms` : 'no fetch');
  check('no “Right now” card once the agent waits', !(await page.$('.rnow.live')));
  check('"stop first" hidden while the agent waits', await page.$eval('.reply-stop', (e) => e.hidden));

  // ---- Last visit ----
  const visit = new Date(t0).toISOString();

  // ---- Pagination ("show earlier") and long turns ----
  apiR.clear();
  await go(page, '/s/s_api02');
  await page.waitForSelector('.voyage');
  const leaving = await page.evaluate(() => JSON.parse(localStorage.getItem('pilothouse.visits')).s_web01);
  check('leaving a session records the visit', new Date(leaving).getTime() >= new Date(visit).getTime());
  const nTurns = () => page.$$eval('.voyage-wrap, .earlier > details.eturn', (e) => e.length);
  const before = await nTurns();
  check('a full first page offers "Show earlier activity"', !!(await page.$('.act-more')) && before === 14, `turns ${before}`);
  check('message reply note for observed Claude points to pilothouse run', (await text(page, '#reply-note')).includes('pilothouse run claude'));
  check('long latest turn: every step on the voyage', (await page.$$eval('.voyage-wrap .vs', (e) => e.length)) >= 8);
  await tap(page, '.act-more');
  await page.waitForFunction((n) => document.querySelectorAll('.voyage-wrap, .earlier > details.eturn').length > n, { timeout: 5000 }, before).catch(() => {});
  const more = apiR.find('GET', /\/api\/sessions\/s_api02\/activity$/).map((c) => c.query.limit).filter(Boolean);
  check('"Show earlier" asks for a bigger page', more.includes('600'), more.join(','));
  check('earlier turns appear', (await nTurns()) === 30, `turns ${await nTurns()}`);
  check('all of the history loaded → no more button', !(await page.$('.act-more')));

  // ---- Fallbacks ----
  await go(page, '/s/s_site03');
  await page.waitForSelector('.act-notice', { timeout: 5000 }).catch(() => {});
  const site = await text(page);
  check('501 → says activity needs a newer Pilothouse', site.includes('Live activity needs a newer Pilothouse'));
  check('501 → hook timeline shown instead', !!(await page.$('ol.timeline li')));
  check('no caps → no reply box, terminal or interrupt', !(await page.$('#reply-text')) && !(await page.$('#interrupt-btn')) && !(await page.$('a[href$="/terminal"]')));
  check('observed Codex → hint how to make it repliable', site.includes('pilothouse run codex'));
  await go(page, '/s/s_notes4');
  await page.waitForSelector('.act-notice', { timeout: 5000 }).catch(() => {});
  const notes = await text(page);
  check('available=false → its note is shown', notes.includes('isn’t available for this session') && notes.includes('where Pilothouse can’t read it'));
  check('available=false → hook timeline shown', notes.includes('You sent a prompt') && notes.includes('Index every note on save.'));
  await go(page, '/s/s_nope99');
  check('unknown session → not found', await waitText(page, 'Session not found', 5000));

  const errs = page._logs.filter((l) => !/status of 40[34]|status of 501/.test(l));
  check('no console errors', errs.length === 0, errs.join(' | '));
  check('no CSP violations', (await page.evaluate(() => window.__csp.length)) === 0);
});
