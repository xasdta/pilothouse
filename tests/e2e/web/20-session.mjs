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
  const first = apiR.find('GET', /\/api\/sessions\/s_web01\/activity$/)[0];
  check('activity fetched with limit=200', first && first.query.limit === '200', JSON.stringify(first && first.query));
  check('activity fetched since the last visit (localStorage)', first && first.query.since === lastSeen, first && first.query.since);
  const head = await text(page, '.sess-hero');
  check('header: live state', head.includes('Working'));
  check('header: usage chip with tokens and est. cost', /1\.2M\s+tokens/.test(head) && head.includes('$2.37'), head.replace(/\s+/g, ' ').slice(0, 160));
  await tap(page, '.usage-chip');
  const usagePanel = await text(page, '.usage-panel');
  check('usage chip opens a breakdown (input, cache read, model)', usagePanel.includes('Input') && usagePanel.includes('Cache read') && usagePanel.includes('claude-sonnet-4-5'), usagePanel.replace(/\s+/g, ' '));
  check('caps.watch → Open terminal', !!(await page.$('a[href="/s/s_web01/terminal"]')));
  check('caps.interrupt → Interrupt', !!(await page.$('#interrupt-btn')));
  check('caps.reply → reply box', !!(await page.$('#reply-text')));
  check('reply note says how it is delivered (terminal)', (await text(page, '#reply-note')).includes('terminal'));
  check('open request shown with the request card', (await text(page, '.sess')).includes('git push origin feat/session-page') && !!(await page.$('.sess article.req')));

  // ---- Catch me up ----
  const cu = await text(page, '.catchup');
  check('catch me up: lines from the API', (await page.$$eval('.catchup-lines li', (l) => l.length)) === 4, cu.slice(0, 120));
  check('catch me up: since and counts', cu.includes('since 55m ago') && cu.includes('19 tool calls') && cu.includes('1 error'), cu.replace(/\s+/g, ' '));

  // ---- Feed ----
  const turns = await page.$$eval('.act-feed > .act-turn', (els) => els.map((e) => e.tagName + (e.classList.contains('latest') ? ':latest' : '')));
  check('feed: 4 turns, newest first and open, older ones collapsed', JSON.stringify(turns) === JSON.stringify(['ARTICLE:latest', 'DETAILS', 'DETAILS', 'DETAILS']), JSON.stringify(turns));
  const latest = await text(page, '.act-turn.latest');
  check('latest turn starts with its prompt', latest.startsWith('You') && latest.includes('Now run the whole Go suite'));
  check('tool kinds have their own icons (shell, mcp)', !!(await page.$('.act-turn.latest .act-tool.k-shell')) && !!(await page.$('.act-turn.latest .act-tool.k-mcp')));
  check('pending tool call shows a spinner', !!(await page.$('.act-turn.latest .act-tool.pending .v2-spin')));
  check('agent working → live row with its state detail', (await text(page, '.act-live')).includes('go test -race ./internal/web'));
  // Expand a result.
  const det = await page.$('.act-turn.latest .act-tool.k-mcp details');
  await tap(page, await det.$('summary'));
  check('tool result expands', (await text(page, '.act-turn.latest .act-tool.k-mcp .act-result')).includes('#212'));
  // Older turn: errors, subagents, system.
  const turn3 = (await page.$$('.act-feed > details.act-turn'))[0];
  check('collapsed turn summary: prompt, steps and errors', (await turn3.evaluate((e) => e.querySelector('summary').innerText)).includes('Run the e2e suite') && (await turn3.evaluate((e) => e.querySelector('summary').innerText)).includes('2 errors'));
  await tap(page, await turn3.$('summary'));
  await sleep(150);
  const t3 = await turn3.evaluate((e) => e.innerText);
  check('error tool call styled as an error', !!(await turn3.$('.act-tool.err')));
  check('error tool result open-able with its output', t3.includes('Run: node tests/e2e/web/20-session.mjs'));
  check('API error shown on its own', !!(await turn3.$('.act-error')) && t3.includes('Overloaded'));
  check('subagent run grouped', !!(await turn3.$('.act-sub')) && t3.includes('Subagent') && t3.includes('2 steps'));
  check('finished subagent group starts collapsed', !(await turn3.$eval('.act-sub', (e) => e.open)));
  await tap(page, await turn3.$('.act-sub > summary'));
  const sub = await turn3.$eval('.act-sub', (e) => e.innerText);
  check('subagent group opens to its steps and finish line', sub.includes('Find tests/e2e/web/*.mjs') && sub.includes('Explore finished'), sub.replace(/\s+/g, ' ').slice(0, 160));
  check('system line shown', t3.includes('Context compacted'));

  // ---- Files changed ----
  check('files changed: 4 rows', (await page.$$eval('.file-row', (r) => r.length)) === 4);
  const files = await text(page, '.files');
  check('files changed: +/- per file, relative paths', files.includes('+412') && files.includes('−96') && files.includes('session.js') && !files.includes('/Users/dev'), files.replace(/\s+/g, ' ').slice(0, 120));

  // ---- Polling with the cursor ----
  const polled = await page.waitForFunction(() => !document.querySelector('.act-turn.latest .act-tool.pending'), { timeout: 8000 }).then(() => true, () => false);
  const next = apiR.find('GET', /\/api\/sessions\/s_web01\/activity$/).find((c) => c.query.cursor === 'c1');
  check('polls with the cursor while visible', !!next && !next.query.limit, JSON.stringify(next && next.query));
  check('a finished call replaces its pending row (same id)', polled && (await page.$$eval('.act-turn.latest .act-tool.k-shell', (els) => els.length)) === 3);
  check('new items appended to the latest turn', await waitText(page, 'All green. Pushing feat/session-page now.', 3000));
  check('usage chip follows the poll', (await text(page, '.sess-hero')).includes('$2.41'));

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
  check('sent reply shown at the top of the feed until the agent picks it up', (await text(page, '.act-feed')).startsWith('You'));
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
  const refetched = await page.waitForFunction(() => document.querySelector('.sess-hero .pill') && document.querySelector('.sess-hero .pill').innerText.includes('Waiting'), { timeout: 3000 }).then(() => true, () => false);
  check('session.updated updates the live state', refetched);
  await sleep(700);
  const after = apiR.find('GET', /\/api\/sessions\/s_web01\/activity$/);
  check('session.updated re-fetches activity at once (< 700 ms)', after.length > 0 && after[0].at - pushedAt < 700, after.length ? `${after[0].at - pushedAt} ms` : 'no fetch');
  check('no live "working" row once the agent waits', !(await page.$('.act-live')));
  check('"stop first" hidden while the agent waits', await page.$eval('.reply-stop', (e) => e.hidden));

  // ---- Catch-up dismissal and last visit ----
  await tap(page, '.catchup-done');
  check('"Got it" hides the catch-up', !(await page.$('.catchup')));
  const visit = await page.evaluate(() => JSON.parse(localStorage.getItem('pilothouse.visits')).s_web01);
  check('"Got it" records the visit', new Date(visit).getTime() >= t0, visit);

  // ---- Pagination ("show earlier") and long turns ----
  apiR.clear();
  await go(page, '/s/s_api02');
  await page.waitForSelector('.act-feed');
  const leaving = await page.evaluate(() => JSON.parse(localStorage.getItem('pilothouse.visits')).s_web01);
  check('leaving a session records the visit', new Date(leaving).getTime() >= new Date(visit).getTime());
  const nTurns = () => page.$$eval('.act-feed > .act-turn', (e) => e.length);
  const before = await nTurns();
  check('a full first page offers "Show earlier activity"', !!(await page.$('.act-more')) && before === 14, `turns ${before}`);
  check('message reply note for observed Claude', (await text(page, '#reply-note')).includes('message sent from Pilothouse'));
  check('long latest turn shows its last steps first', (await text(page, '.act-turn.latest')).includes('Show 6 earlier steps'));
  const shownSteps = await page.$$eval('.act-turn.latest .act-step', (e) => e.length);
  await tap(page, '.act-earlier button');
  const allSteps = await page.$$eval('.act-turn.latest .act-step', (e) => e.length);
  check('"Show earlier steps" reveals the whole turn', shownSteps === 8 && allSteps === 14, `${shownSteps} → ${allSteps}`);
  await tap(page, '.act-more');
  await page.waitForFunction((n) => document.querySelectorAll('.act-feed > .act-turn').length > n, { timeout: 5000 }, before).catch(() => {});
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
