// 22. Tasks (0.2): list with status chips and who started each, new task
// (signed `task` "new"), task detail with progress and diff, Ship and
// Discard behind a confirmation (signed `task` <id>), live updates.
// Run: node 22-tasks.mjs (needs Go and Chrome; see v02-harness.mjs).
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
  F.mount(apiR, w);
  await page.setViewport(PHONE);
  await login(page, dev);
  const spki = await deviceKey(page);
  const me = await page.evaluate(async () => (await (await fetch('/api/me')).json()).device.id);
  w.tasks[0].by = 'device:' + me;

  let startN = 0;
  apiR.on('POST', /^\/api\/tasks$/, ({ body }) => {
    startN++;
    if (startN === 1) return { status: 400, json: { error: 'unknown repository /Users/dev/src/proofline', code: 'invalid' } };
    const p = JSON.parse(JSON.parse(body).payload);
    const t = { id: 't_new001', title: 'Fix flaky webhook test', prompt: p.prompt, agent: p.agent, repo: p.repo, worktree: '/Users/dev/.pilothouse/worktrees/proofline-retry-fix', branch: 'ph/' + (p.name || 'task'), base_ref: p.base || 'main', status: 'running', by: 'device:' + me, created_at: new Date().toISOString(), updated_at: new Date().toISOString() };
    w.tasks.unshift(t);
    return { json: t };
  });
  apiR.on('POST', /^\/api\/tasks\/([\w-]+)\/finish$/, ({ m, body }) => {
    const p = JSON.parse(JSON.parse(body).payload);
    const t = w.tasks.find((x) => x.id === m[1]);
    const done = { ...t, status: p.action === 'ship' ? 'done' : 'abandoned', updated_at: new Date().toISOString(), pr_url: p.action === 'ship' ? 'https://github.com/dev/proofline/pull/88' : undefined };
    Object.assign(t, done);
    return { json: done };
  });

  // ---- List ----
  await go(page, '/tasks');
  await page.waitForSelector('.task-row', { timeout: 6000 });
  const list = await text(page);
  check('sections: ready for review, running, finished', list.includes('READY FOR REVIEW') && list.includes('RUNNING') && list.includes('FINISHED'), list.slice(0, 120));
  const chips = await page.$$eval('.task-row .task-chip', (c) => c.map((x) => x.className.split(' ')[1] + ':' + x.innerText.trim()));
  check('status chips for review, running, done, abandoned', JSON.stringify(chips) === JSON.stringify(['review:Review', 'running:Running', 'done:Done', 'abandoned:Discarded']), JSON.stringify(chips));
  const rowsTxt = await page.$$eval('.task-row', (r) => r.map((x) => x.innerText.replace(/\s+/g, ' ')));
  check('who started each: this device / the computer / another device', rowsTxt[0].includes('from this device') && rowsTxt[1].includes('at the computer') && rowsTxt[2].includes('from another device'), rowsTxt.slice(0, 3).join(' | '));
  check('a task without "by" says nothing about who', !/from|at the computer/.test(rowsTxt[3].split('·').slice(-2).join('')), rowsTxt[3]);
  check('running task shows its session’s live state', rowsTxt[1].includes('Running npm run build'));
  check('sub-heading counts', (await text(page, '.page-sub')).includes('1 to review') && (await text(page, '.page-sub')).includes('1 running'));

  // Live: a task.updated event adds a row.
  sse.push('/events', 'event', { seq: 1e9 + 5, at: new Date().toISOString(), type: 'task.updated', data: { ...w.tasks[1], id: 't_evt001', title: 'Bump Node to 22', status: 'running', created_at: new Date().toISOString() } });
  check('task.updated shows up without a reload', await waitText(page, 'Bump Node to 22', 3000));

  // ---- New task ----
  await tap(page, '#new-task');
  await page.waitForSelector('.repo-opt input', { timeout: 6000 });
  const repos = await page.$$eval('.repo-name', (r) => r.map((x) => x.innerText));
  check('repo picker lists /api/repos, most recent first', JSON.stringify(repos) === JSON.stringify(['pilothouse', 'api-server', 'proofline', 'site']), JSON.stringify(repos));
  check('first repo picked by default', await page.$eval('#repo-0', (e) => e.checked));
  check('agents: Claude and Codex; no OpenCode before Pilothouse has seen it', !!(await page.$('#agent-claude')) && !!(await page.$('#agent-codex')) && !(await page.$('#agent-opencode')));
  apiR.clear();
  await tap(page, '.task-form button[type=submit]');
  check('empty prompt → asks for one, nothing sent', (await text(page, '#task-err')).includes('Tell the agent what to do') && apiR.find('POST', /\/api\/tasks$/).length === 0);
  await tap(page, 'label[for="repo-2"]');
  await tap(page, 'label[for="agent-codex"]');
  await page.type('#task-prompt', 'The webhook retry test is flaky on CI. Make it deterministic.');
  await tap(page, '.task-opts summary');
  check('base branch placeholder follows the repo', (await page.$eval('#task-base', (e) => e.placeholder)) === 'main');
  await page.type('#task-base', 'main');
  await page.type('#task-name', 'retry-fix');
  await tap(page, '.task-form button[type=submit]');
  check('server error shown in the form', await waitText(page, 'Unknown repository', 3000, '#task-err'));
  await tap(page, '.task-form button[type=submit]');
  await page.waitForFunction(() => location.pathname === '/t/t_new001', { timeout: 5000 }).catch(() => {});
  const calls = apiR.find('POST', /\/api\/tasks$/);
  const v = calls[1] ? verifyAction(spki, 'task', 'new', calls[1].body) : { ok: false, why: 'no call' };
  check('new task is a signed `task` action with target "new"', v.ok, v.why);
  check('payload: repo, agent, prompt, base, name', v.payload && v.payload.repo === '/Users/dev/src/proofline' && v.payload.agent === 'codex' && v.payload.prompt.startsWith('The webhook retry test') && v.payload.base === 'main' && v.payload.name === 'retry-fix', JSON.stringify(v.payload));
  check('lands on the new task', new URL(page.url()).pathname === '/t/t_new001' && await waitText(page, 'Fix flaky webhook test', 3000));
  check('remembers repo and agent for next time', (await page.evaluate(() => [localStorage.getItem('pilothouse.task.repo'), localStorage.getItem('pilothouse.task.agent')])).join() === '/Users/dev/src/proofline,codex');

  // ---- Task detail ----
  await go(page, '/t/t_retry01');
  await page.waitForSelector('.file-diff', { timeout: 6000 });
  const hero = await text(page, '.task-hero');
  check('detail: title, repo, branch ← base, who', hero.includes('Retry failed webhooks') && hero.includes('proofline') && hero.includes('ph/webhook-retries') && hero.includes('main') && hero.includes('started from this device'), hero.replace(/\s+/g, ' '));
  check('progress: Review is the current step', (await page.$eval('.step[aria-current="step"]', (e) => e.innerText)).startsWith('Review'));
  check('progress: links to the task’s session', !!(await page.$('a.task-session[href="/s/s_task05"]')) && (await text(page, '.task-session')).includes('Waiting'));
  const diff = await text(page, '.task-files');
  check('diff: one card per file with +/-', (await page.$$eval('.file-diff', (e) => e.length)) === 3 && diff.includes('+34') && diff.includes('−8'));
  check('diff: added and removed lines with line numbers', (await page.$$eval('.file-diff .tl-add', (e) => e.length)) > 20 && (await page.$$eval('.file-diff .tl-del', (e) => e.length)) >= 5);
  check('diff: hunk headers and a "new" tag', (await page.$$eval('.tl-hunk', (e) => e.length)) === 4 && diff.includes('new'));
  check('diff: line numbers follow the hunk', await page.$eval('.file-diff .tl-ctx .ln', (e) => e.textContent === '12'));
  check('diff: rendered by the shared diff component', (await page.$$eval('.file-diff .diff.bare .dfile', (e) => e.length)) === 3);
  const wide = await page.$eval('.file-diff .dbody', (b) => ({ ox: getComputedStyle(b).overflowX, sw: b.scrollWidth, cw: b.clientWidth }));
  check('diff: long lines scroll sideways inside the card on a phone (not clipped)', wide.sw <= wide.cw || wide.ox === 'auto' || wide.ox === 'scroll', JSON.stringify(wide));
  // Fold a file.
  await tap(page, '.file-diff summary');
  check('a file’s diff folds', !(await page.$eval('.file-diff', (e) => e.open)));

  // ---- Ship ----
  apiR.clear();
  await tap(page, '#task-ship');
  await page.waitForSelector('dialog.sheet[open]');
  check('ship asks first, naming branch and base', (await text(page, 'dialog.sheet')).includes('ph/webhook-retries') && (await text(page, 'dialog.sheet')).includes('main'));
  await tap(page, 'dialog.sheet .btn.secondary');
  await sleep(200);
  check('cancel sends nothing', !(await page.$('dialog.sheet')) && apiR.find('POST', /finish$/).length === 0);
  await tap(page, '#task-ship');
  await page.waitForSelector('dialog.sheet[open]');
  await tap(page, 'dialog.sheet .btn.primary');
  await waitText(page, 'View pull request', 4000);
  let c = apiR.find('POST', /\/api\/tasks\/t_retry01\/finish$/)[0];
  let vf = c ? verifyAction(spki, 'task', 't_retry01', c.body) : { ok: false, why: 'no call' };
  check('ship is a signed `task` action with the task id', vf.ok && vf.payload.action === 'ship', vf.why || JSON.stringify(vf.payload));
  check('after ship: Done, PR link, no actions', (await text(page, '.task-hero')).includes('Done') && (await page.$eval('.task-pr', (a) => a.href)) === 'https://github.com/dev/proofline/pull/88' && !(await page.$('#task-ship')));
  check('after ship: progress complete', (await page.$$eval('.step.done', (e) => e.length)) === 4);

  // ---- Discard (running task) ----
  await go(page, '/t/t_dark002');
  await page.waitForSelector('#task-discard', { timeout: 6000 });
  check('running task: Ship waits for review', await page.$eval('#task-ship', (b) => b.disabled));
  check('truncated diff says so', (await text(page)).includes('cut short'));
  apiR.clear();
  await tap(page, '#task-discard');
  await page.waitForSelector('dialog.sheet[open]');
  check('discard warns it can’t be undone', (await text(page, 'dialog.sheet')).includes('can’t be undone'));
  await page.keyboard.press('Escape');
  await sleep(200);
  check('Escape cancels', !(await page.$('dialog.sheet')) && apiR.find('POST', /finish$/).length === 0);
  await tap(page, '#task-discard');
  await page.waitForSelector('dialog.sheet[open]');
  await tap(page, 'dialog.sheet .btn.danger');
  await waitText(page, 'Discarded', 4000, '.task-hero');
  c = apiR.find('POST', /\/api\/tasks\/t_dark002\/finish$/)[0];
  vf = c ? verifyAction(spki, 'task', 't_dark002', c.body) : { ok: false, why: 'no call' };
  check('discard is a signed `task` action with the task id', vf.ok && vf.payload.action === 'discard', vf.why || JSON.stringify(vf.payload));
  check('after discard: Discarded step, no actions', (await text(page, '.task-prog')).includes('Discarded') && !(await page.$('#task-discard')));

  // ---- Failed task: its note, setup that failed, Discard only ----
  const failed = { ...w.tasks[2], id: 't_fail005', title: 'Upgrade the ORM', status: 'failed', session_id: '', note: 'setup command 2 of 2 exited with status 3', setup: { copied: ['.env', '.env.local'], commands: 2, status: 'failed', log: '/Users/dev/.pilothouse/tasks/t_fail005/setup.log' }, created_at: new Date().toISOString(), updated_at: new Date().toISOString() };
  w.tasks.push(failed);
  sse.push('/events', 'event', { seq: 1e9 + 6, at: new Date().toISOString(), type: 'task.updated', data: failed });
  await go(page, '/tasks');
  await page.waitForSelector('.task-row', { timeout: 6000 });
  const failRow = await page.$$eval('.task-row', (r) => r.map((x) => x.innerText.replace(/\s+/g, ' ')).find((t) => t.includes('Upgrade the ORM')) || '');
  check('failed task: “Failed” chip and its note in the list', failRow.includes('Failed') && failRow.includes('exited with status 3'), failRow);
  await go(page, '/t/t_fail005');
  await page.waitForSelector('.task-prog', { timeout: 6000 });
  check('failed task: the note in the header', (await text(page, '.task-note.bad')).includes('setup command 2 of 2 exited with status 3'));
  const prog = await text(page, '.task-prog');
  check('failed task: setup step says what failed and where its log is, and what was copied', prog.includes('Setup') && prog.includes('A setup command failed') && prog.includes('setup.log') && prog.includes('.env, .env.local'), prog.replace(/\s+/g, ' '));
  check('failed task: ends at a Failed step, no Working/Review', prog.includes('Failed') && !prog.includes('Working') && !prog.includes('Review'));
  check('failed task: Discard but no Ship', !!(await page.$('#task-discard')) && !(await page.$('#task-ship')));
  // Setup still running: the Setup step is current, Working waits.
  const settingUp = { ...failed, id: 't_setup06', title: 'Add rate limits', status: 'running', note: '', setup: { commands: 1, status: 'running' } };
  w.tasks.push(settingUp);
  sse.push('/events', 'event', { seq: 1e9 + 7, at: new Date().toISOString(), type: 'task.updated', data: settingUp });
  await go(page, '/t/t_setup06');
  await page.waitForSelector('.task-prog', { timeout: 6000 });
  check('setup running: Setup is the current step', (await page.$eval('.step[aria-current="step"]', (e) => e.innerText)).includes('Running 1 setup command'));
  // OpenCode: offered once Pilothouse has seen it.
  sse.push('/events', 'event', { seq: 1e9 + 8, at: new Date().toISOString(), type: 'task.updated', data: { ...settingUp, id: 't_oc007', agent: 'opencode', title: 'OpenCode task' } });
  await sleep(300);
  await go(page, '/new');
  await page.waitForSelector('.repo-opt input', { timeout: 6000 });
  check('agents: OpenCode offered once Pilothouse has seen it', !!(await page.$('#agent-opencode')));

  // ---- Not supported, not found ----
  apiR.on('GET', /^\/api\/tasks\/t_retry01\/diff$/, () => ({ status: 501, json: { error: 'not supported here', code: 'unsupported' } }));
  await go(page, '/t/t_nope99');
  check('unknown task → not found', await waitText(page, 'Task not found', 4000));

  const errs = page._logs.filter((l) => !/status of (400|404|501)/.test(l)); // the refused start, the diff.js probe
  check('no console errors', errs.length === 0, errs.join(' | ').slice(0, 300));
  check('no CSP violations', (await page.evaluate(() => window.__csp.length)) === 0);
});
