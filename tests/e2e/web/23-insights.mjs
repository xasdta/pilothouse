// 23. Insights (0.2): range switch, daily chart (tooltip, keyboard,
// table twin), cost/tokens, breakdowns, rule tiles, top rules, budget
// states, 501 and empty states.
// Run: node 23-insights.mjs (needs Go and Chrome; see v02-harness.mjs).
import { run, startDevserver, launch, login, router, sseServer, check, sleep, PHONE, DESKTOP, watch, go, text, waitText, tap, setSize } from './v02-harness.mjs';
import * as F from './v02-fixtures.mjs';

await run(async () => {
  const dev = await startDevserver();
  const browser = await launch();
  const page = await browser.newPage();
  watch(page);
  const sse = await sseServer();
  const apiR = await router(page, sse);
  const w = F.world();
  F.mount(apiR, w);
  let mode = 'normal';
  apiR.on('GET', /^\/api\/insights$/, ({ url }) => {
    const days = Number(url.searchParams.get('days')) || 7;
    if (mode === '501') return { status: 501, json: { error: 'not supported here', code: 'unsupported' } };
    if (mode === 'empty') return { json: { ...F.insights(days, w.now), total: { input_tokens: 0, output_tokens: 0, cost_usd: 0 }, days: F.insights(days, w.now).days.map((d) => ({ date: d.date, usage: { input_tokens: 0, output_tokens: 0, cost_usd: 0 }, sessions: 0, auto_allowed: 0, asked: 0, blocked: 0 })), rules: {}, by_agent: [], by_model: [], by_repo: [] } };
    if (mode === 'over') return { json: F.insights(days, w.now, { daily_usd: 5, spent_usd: 6.2, percent: 124, burn_usd_per_hour: 2.4 }) };
    if (mode === 'nobudget') return { json: { ...F.insights(days, w.now), budget: null } };
    return { json: F.insights(days, w.now) };
  });
  await page.setViewport(PHONE);
  await login(page, dev);
  await page.evaluate(() => localStorage.removeItem('pilothouse.insights.days'));

  const fx7 = F.insights(7, w.now);
  await go(page, '/insights');
  await page.waitForSelector('svg.cols .bar', { timeout: 6000 });
  const q = () => apiR.find('GET', /\/api\/insights$/).map((c) => c.query.days);
  check('loads 7 days by default', q()[0] === '7', q().join(','));
  check('7 bars', (await page.$$eval('svg.cols .bar', (b) => b.length)) === 7);
  const hero = await text(page, '.ins-hero');
  check('hero: total est. cost, tokens and sessions', hero.includes('$' + fx7.total.cost_usd.toFixed(2)) && /tokens/.test(hero) && /sessions/.test(hero), hero.replace(/\s+/g, ' '));
  check('peak day labelled on its cap', (await page.$$eval('svg.cols .cap', (c) => c.length)) === 1);
  check('chart has a title and description', await page.$eval('svg.cols', (s) => !!s.querySelector('title') && s.querySelector('desc').textContent.includes('Highest')));

  // ---- Tooltip: pointer and keyboard ----
  const box = await (await page.$('svg.cols')).boundingBox();
  const left = await page.$eval('svg.cols', (s) => Number(s.dataset.left));
  const band = await page.$eval('svg.cols', (s) => Number(s.dataset.band));
  await page.mouse.move(box.x + left + band * 2.5, box.y + box.height / 2);
  await sleep(150);
  const tip = await text(page, '.chart-tip');
  const day2 = fx7.days[2];
  check('hover shows the day’s value', !(await page.$eval('.chart-tip', (t) => t.hidden)) && tip.includes('$' + day2.usage.cost_usd.toFixed(2)) && tip.includes(`${day2.sessions} sessions`), tip.replace(/\s+/g, ' '));
  check('hovered bar lifts, others dim', (await page.$$eval('svg.cols .bar.on', (b) => b.length)) === 1 && await page.$eval('.chart', (c) => c.classList.contains('has-on')));
  await page.mouse.move(5, 5);
  await sleep(100);
  check('tooltip hides when the pointer leaves', await page.$eval('.chart-tip', (t) => t.hidden));
  await page.$eval('svg.cols', (s) => s.focus()); // SVG: puppeteer's focus() only takes HTML elements
  await sleep(100);
  const last = fx7.days[6];
  check('focus shows the latest day', (await text(page, '.chart-tip')).includes('$' + last.usage.cost_usd.toFixed(2)));
  await page.keyboard.press('ArrowLeft');
  await sleep(100);
  check('arrow keys move between days', (await text(page, '.chart-tip')).includes('$' + fx7.days[5].usage.cost_usd.toFixed(2)));

  // ---- Table twin ----
  await tap(page, '.ins-card-head .linkish');
  check('table view: one row per day, newest first', (await page.$$eval('#daily-table tbody tr', (r) => r.length)) === 7 && (await page.$eval('#daily-table tbody tr td', (t) => t.textContent)) === '$' + last.usage.cost_usd.toFixed(2));
  check('table toggle is a disclosure', (await page.$eval('.ins-card-head .linkish', (b) => b.getAttribute('aria-expanded'))) === 'true');

  // ---- Range switch ----
  await tap(page, 'label[for$="-30"]');
  await page.waitForFunction(() => document.querySelectorAll('svg.cols .bar').length === 30, { timeout: 5000 }).catch(() => {});
  check('30 days → days=30 and 30 bars', q().includes('30') && (await page.$$eval('svg.cols .bar', (b) => b.length)) === 30);
  await tap(page, 'label[for$="-90"]');
  await page.waitForFunction(() => document.querySelectorAll('svg.cols .bar').length === 90, { timeout: 5000 }).catch(() => {});
  check('90 days → 90 bars, still inside the card', (await page.$$eval('svg.cols .bar', (b) => b.length)) === 90 && await page.$eval('svg.cols', (s) => s.getBoundingClientRect().right <= s.closest('.card').getBoundingClientRect().right));
  check('range remembered', (await page.evaluate(() => localStorage.getItem('pilothouse.insights.days'))) === '90');
  apiR.clear();
  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.waitForSelector('svg.cols .bar', { timeout: 8000 });
  check('reload keeps 90 days', q()[0] === '90');

  // ---- Tokens ----
  await tap(page, 'label[for*="metric"][for$="-tokens"]');
  await sleep(200);
  check('tokens: hero and chart switch measure', (await text(page, '#daily-h')) === 'Tokens per day' && (await text(page, '.ins-hero-label')).startsWith('Tokens'));
  await tap(page, 'label[for*="metric"][for$="-cost"]');

  // ---- Breakdown ----
  const groups = () => page.$$eval('.ins-card[aria-labelledby="by-h"] .hbar-label', (l) => l.map((x) => x.innerText.trim()));
  check('by agent (named)', JSON.stringify(await groups()) === JSON.stringify(['Claude Code', 'Codex', 'OpenCode']), JSON.stringify(await groups()));
  await tap(page, 'label[for*="seg-by"][for$="-model"]');
  check('by model, largest first', (await groups())[0] === 'claude-sonnet-4-5' && (await groups()).length === 4);
  await tap(page, 'label[for*="seg-by"][for$="-repo"]');
  check('by repo', (await groups()).length === 5 && (await groups())[0] === 'pilothouse');

  // ---- Rules ----
  const fx90 = F.insights(90, w.now);
  const tiles = await page.$$eval('.tile', (t) => t.map((x) => [x.querySelector('.tile-label').innerText.trim(), x.querySelector('.tile-value').innerText.trim()]));
  const tv = Object.fromEntries(tiles);
  check('six rule tiles', tiles.length === 6, JSON.stringify(tiles));
  check('auto-allowed / asked / blocked / granted', tv['Auto-allowed'] === new Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 }).format(fx90.rules.auto_allowed) && tv['Asked you'] === String(fx90.rules.asked) && tv.Blocked === String(fx90.rules.blocked) && tv['Allowed for session'] === String(fx90.rules.granted), JSON.stringify(tv));
  check('median answer time and minutes agents waited', tv['Median answer time'] === '38 s' && /min|h/.test(tv['Agents waited']), JSON.stringify(tv));
  check('sparklines on the first three tiles', (await page.$$eval('.tile svg.spark', (s) => s.length)) === 3);
  check('top rules and top blocked', (await text(page, '.top-allow')).includes('allow-tests') && (await text(page, '.top-block')).includes('deny-rm-rf'));

  // ---- Budget ----
  check('budget: on track with %', (await text(page, '.ins-budget')).includes('On track') && (await text(page, '.ins-budget')).includes('61 %'));
  check('budget meter is a labelled meter', (await page.$eval('.meter', (m) => m.getAttribute('role') + '|' + m.getAttribute('aria-label'))).startsWith('meter|Today’s spend'));
  mode = 'over';
  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.waitForSelector('.ins-budget', { timeout: 8000 });
  check('budget: over, meter full, status with icon + label', (await text(page, '.ins-budget')).includes('Over budget') && parseFloat(await page.$eval('.meter-fill', (f) => f.style.width)) === 100 && !!(await page.$('.ins-budget-status svg')));
  mode = 'nobudget';
  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.waitForSelector('.ins-budget', { timeout: 8000 });
  check('no budget → how to set one', (await text(page, '.ins-budget')).includes('daily_budget_usd'));

  // ---- Empty and unsupported ----
  mode = 'empty';
  await page.reload({ waitUntil: 'domcontentloaded' });
  check('nothing recorded → empty state', await waitText(page, 'Nothing recorded yet', 8000));
  mode = '501';
  await page.reload({ waitUntil: 'domcontentloaded' });
  check('501 → needs a newer Pilothouse', await waitText(page, 'Insights need a newer Pilothouse', 8000));

  // ---- Desktop layout ----
  mode = 'normal';
  await setSize(page, DESKTOP);
  await page.waitForSelector('svg.cols .bar', { timeout: 8000 });
  const grid = await page.$eval('.tiles', (t) => getComputedStyle(t).gridTemplateColumns.split(' ').length);
  check('desktop: tiles in three columns', grid === 3, String(grid));
  check('desktop: columns at most 24 px wide', await page.$$eval('svg.cols .bar', (b) => b.every((x) => x.getBoundingClientRect().width <= 24.5)));

  const errs = page._logs.filter((l) => !/status of 501/.test(l));
  check('no console errors', errs.length === 0, errs.join(' | ').slice(0, 300));
  check('no CSP violations', (await page.evaluate(() => window.__csp.length)) === 0);
});
