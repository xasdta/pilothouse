// 10. Request cards on the demo core: question answers (options,
// multi-select, free text), plan revise → revised plan → approve, the
// diff card, "Make it a rule" (with "also answer"), stale protection
// across devices, and optimistic rollback. Every answer must be a signed
// v2 respond.
//
//   PH_TMP=… PH_SHOTS=… node 10-demo-cards.mjs
import { startDemo, login, goto, watch, check, summary, sleep, shot, text, waitText, axeCheck, scheme, PHONE } from './demo-lib.mjs';

const demo = await startDemo();
const browser = await demo.browser();
const ctxA = await browser.createBrowserContext();
const ctxB = await browser.createBrowserContext();
const A = await ctxA.newPage();
const B = await ctxB.newPage();
await A.setViewport(PHONE);
await B.setViewport(PHONE);
const logsA = watch(A, 'A');
watch(B, 'B');
await scheme(A, 'light');
await login(A, demo);
await login(B, demo);

// Every answer the page sends, decoded.
const sent = [];
A.on('request', (r) => {
  if (!r.url().includes('/respond') && !r.url().endsWith('/api/rules')) return;
  try {
    const body = JSON.parse(r.postData());
    sent.push({ url: r.url(), payload: JSON.parse(body.payload), signed: !!body.sig && !!body.nonce && !!body.ts });
  } catch { sent.push({ url: r.url(), bad: true }); }
});
const lastSent = (part) => [...sent].reverse().find((s) => s.url.includes(part));
/** The last body sent to a URL containing part, once it is on the wire. */
async function waitSent(part, after = 0) {
  for (let i = 0; i < 60; i++) {
    const x = sent.slice(after).reverse().find((s) => s.url.includes(part));
    if (x) return x;
    await sleep(50);
  }
  return null;
}
/** Clicks after scrolling to the middle of the screen (clear of the fixed bars). */
async function tap(el) {
  await el.evaluate((e) => e.scrollIntoView({ block: 'center' }));
  await el.click();
}
const getJSON = (page, path) => page.evaluate((p) => fetch(p, { credentials: 'same-origin' }).then((r) => r.json()), path);
const card = (page, id) => page.$(`main article[data-request="${id}"]`);

check('inbox shows four cards that need you', (await A.$$('main article.req')).length === 4, String((await A.$$('main article.req')).length));

// ---------- Question ----------
const q = await card(A, 'r_question05');
const sendBtn = async () => q.$('.actions .btn.primary');
const disabled = async () => (await sendBtn()).evaluate((b) => b.disabled);
check('question: Send answer starts disabled', await disabled());
const opts = await q.$$('.opt');
check('question: options for both questions (+ Other…)', opts.length === 8, String(opts.length));
check('question: single-select options are radios, multi-select are checkboxes',
  (await opts[0].evaluate((b) => b.getAttribute('role'))) === 'radio' && (await opts[4].evaluate((b) => b.getAttribute('role'))) === 'checkbox');
await opts[3].click(); // Other…
await sleep(100);
const other = await q.$('.other-input');
check('question: Other… reveals a text field', other && !(await other.evaluate((i) => i.hidden)));
await other.type('Postgres tsvector');
check('question: still disabled until every question is answered', await disabled(), await text(A, 'main article[data-request="r_question05"] .answer-hint'));
await opts[4].click(); // Title
await opts[6].click(); // Tags
await opts[6].click(); // untick Tags
await opts[5].click(); // Body
check('question: enabled once answered', !(await disabled()));
await shot(A, 'h-flow-question-filled');
await (await sendBtn()).click();
check('question: shows the answer at once', await waitText(A, 'Answered', 3000, 'main article[data-request="r_question05"]'));
await sleep(500);
let s = lastSent('r_question05');
check('question: answer is a signed respond with answers by question id',
  s && s.signed && s.payload.behavior === 'allow' && s.payload.answers.q1 === 'Postgres tsvector' && s.payload.answers.q2 === 'Title, Body', JSON.stringify(s && s.payload));
const rq = await getJSON(A, '/api/requests/r_question05');
check('question: the daemon recorded the answers', rq.status === 'decided' && rq.decision.answers.q2 === 'Title, Body', JSON.stringify(rq.decision));

// ---------- Plan: request changes, then approve the revision ----------
const p = await card(A, 'r_plan0006');
await p.evaluate((el) => el.scrollIntoView());
const [revise] = await p.$$('.actions .btn.secondary');
await tap(revise);
const fb = await p.$('.revise-form textarea');
check('plan: Request changes opens a feedback box', fb && (await fb.evaluate((t) => document.activeElement === t)));
const fbSend = await p.$('.revise-form .btn.primary');
check('plan: Send feedback needs text', await fbSend.evaluate((b) => b.disabled));
await fb.type('Keep passwords for two releases, not one.');
await shot(A, 'h-flow-plan-feedback');
await tap(fbSend);
check('plan: shows “Changes requested”', await waitText(A, 'Changes requested', 3000, 'main article[data-request="r_plan0006"]'));
s = await waitSent('r_plan0006');
check('plan: feedback is a signed deny with the message', s && s.signed && s.payload.behavior === 'deny' && s.payload.message.includes('two releases'), JSON.stringify(s && s.payload));
const revised = await A.waitForFunction(() => {
  const c = [...document.querySelectorAll('main article.req.k-plan')].find((el) => el.dataset.request !== 'r_plan0006');
  return c && c.innerText.includes('What changed') && c.dataset.request;
}, { timeout: 12000 }).then((h) => h.jsonValue(), () => '');
check('plan: the agent comes back with a revised plan that includes the feedback', !!revised);
if (revised) {
  const rc = await card(A, revised);
  await rc.evaluate((el) => el.scrollIntoView());
  await shot(A, 'h-flow-plan-revised');
  await tap(await rc.$('.actions .btn.primary'));
  check('plan: approve shows “Plan approved”', await waitText(A, 'Plan approved', 3000, `main article[data-request="${revised}"]`));
  s = await waitSent(revised);
  check('plan: approve is a signed allow', s && s.signed && s.payload.behavior === 'allow');
}

// ---------- Diff card ----------
const tf = await card(A, 'r_tfedit04');
await tf.evaluate((el) => el.scrollIntoView());
check('diff: one file with +3 −3', (await tf.$$('.dfile')).length === 1 && (await tf.$eval('.dfile-head .stat', (e) => e.innerText)).replace(/\s+/g, ' ').includes('+3 −3'));
// The inbox card shows a short diff; the rest is one tap away.
check('diff: added and removed lines are marked', (await tf.$$('.dl.add')).length >= 1 && (await tf.$$('.dl.del')).length >= 1);
check('diff: the inbox card offers the rest of the diff', (await text(A, 'main article[data-request="r_tfedit04"]')).includes('more lines'));
check('diff: line numbers follow the hunk header', (await tf.$eval('.dl.ctx .n', (e) => e.textContent)) === '14');
const head = await tf.$('.dfile-head');
await tap(head);
check('diff: a file collapses', (await head.evaluate((b) => b.getAttribute('aria-expanded'))) === 'false' && (await tf.$eval('.dbody', (b) => getComputedStyle(b).display)) === 'none');
await tap(head);
check('diff: and opens again', (await head.evaluate((b) => b.getAttribute('aria-expanded'))) === 'true');
// The rule that fired is on the request page, not the inbox card.
check('diff: risk label on the card, rule left to the request page', (await text(A, 'main article[data-request="r_tfedit04"]')).includes('Changes production infrastructure') && !(await text(A, 'main article[data-request="r_tfedit04"]')).includes('protect-prod'));

// ---------- Components: diff "show more", hostile Markdown ----------
const comp = await A.evaluate(async () => {
  const { diffView } = await import('/js/diff.js');
  const { renderMarkdown } = await import('/js/markdown.js');
  const lines = Array.from({ length: 60 }, (_, i) => '+line ' + i + ' <b>not bold</b>').join('\n');
  const box = document.createElement('div');
  document.body.appendChild(box);
  const d = diffView({ files: [{ path: '/repo/a/b.txt', op: 'create', added: 60, removed: 0, diff: '@@ -0,0 +1,60 @@\n' + lines }] }, { cwd: '/repo', maxLines: 40 });
  box.appendChild(d);
  const more = d.querySelector('.dmore');
  const out = { label: more && more.textContent, rows: d.querySelectorAll('.dl').length, bold: d.querySelectorAll('b').length, path: d.querySelector('.path').textContent };
  more.click();
  out.after = d.querySelectorAll('.dl').length;
  const md = renderMarkdown('# T\n\nHi <img src=x onerror="window.__pwned=1"> [click](javascript:window.__pwned=2) **b** `c`\n\n<script>window.__pwned=3</script>\n\n- [x] done\n- [ ] todo\n\n| a | b |\n|---|---|\n| 1 | 2 |');
  box.appendChild(md);
  out.md = { imgs: md.querySelectorAll('img,script,a,iframe').length, text: md.textContent, strong: md.querySelectorAll('strong').length, code: md.querySelectorAll('code').length, tasks: md.querySelectorAll('li.task').length, table: md.querySelectorAll('td').length };
  box.remove();
  return out;
});
check('diff component: 40 rows, then “Show 21 more lines” (hunk + 60)', comp.label === 'Show 21 more lines' && comp.rows === 40, JSON.stringify(comp));
check('diff component: Show more reveals all lines', comp.after === 61, String(comp.after));
check('diff component: diff text is never HTML, paths are relative', comp.bold === 0 && comp.path.includes('a/b.txt') && !comp.path.includes('/repo'));
check('markdown: no elements from agent text (img, script, links)', comp.md.imgs === 0 && !(await A.evaluate(() => window.__pwned)), JSON.stringify(comp.md));
check('markdown: formatting, code, tasks and tables render', comp.md.strong === 1 && comp.md.code === 1 && comp.md.tasks === 2 && comp.md.table === 2, JSON.stringify(comp.md));
check('markdown: links show as text with their URL', comp.md.text.includes('click') && comp.md.text.includes('(javascript:window.__pwned=2)'));

// ---------- Make it a rule (+ also answer) ----------
await tap(await tf.$('.card-foot .link-btn'));
await A.waitForSelector('dialog.sheet[open] .sug', { timeout: 5000 });
await shot(A, 'h-flow-rule-sheet');
await axeCheck(A, 'rule sheet');
const sugs = await A.$$eval('dialog.sheet .sug', (els) => els.map((e) => e.innerText.replace(/\s+/g, ' ')));
check('rule sheet: suggests allowing edits under the folder in this repo', sugs.some((x) => x.includes('Allow edits under') && x.includes('terraform/prod/**')), sugs.join(' | '));
const alsoOn = await A.$eval('dialog.sheet .also input', (i) => i.checked && !i.closest('[hidden]'));
check('rule sheet: “Also answer this request” is offered and on', alsoOn);
await (await A.$('dialog.sheet .sug .btn.primary')).click();
check('rule sheet: the rule is added', await waitText(A, 'Rule added', 4000));
s = await waitSent('/api/rules');
check('rule sheet: adding is a signed rule action with the spec', s && s.signed && s.payload.action === 'allow' && s.payload.match && s.payload.match.path, JSON.stringify(s && s.payload));
const rules = await getJSON(A, '/api/rules');
check('rule sheet: the rule is listed as managed', rules.rules.some((r) => r.managed && r.name === s.payload.name));
await sleep(600);
const tfNow = await getJSON(A, '/api/requests/r_tfedit04');
check('rule sheet: “also answer” allowed this request too', tfNow.status === 'decided' && tfNow.decision.behavior === 'allow');

// ---------- Optimistic rollback ----------
// The git push request: the answer fails to send, so the card must come back.
await A.setRequestInterception(true);
const failing = (r) => (r.url().includes('/respond') ? r.abort('internetdisconnected') : r.continue());
A.on('request', failing);
await goto(A, demo, '/');
const any = await card(A, 'r_gitpush03');
let rolled = false;
if (any) {
  const id = await any.evaluate((el) => el.dataset.request);
  const primary = await any.$('.actions .btn.primary');
  const isPerm = await any.evaluate((el) => el.classList.contains('k-permission'));
  if (isPerm && primary) {
    await any.evaluate((el) => {
      window.__sawResolved = false;
      new MutationObserver(() => { if (el.classList.contains('resolved')) window.__sawResolved = true; }).observe(el, { attributes: true, attributeFilter: ['class'] });
    });
    await tap(primary);
    await sleep(300);
    const optimistic = await A.evaluate(() => window.__sawResolved);
    rolled = await A.waitForFunction((rid) => {
      const c = document.querySelector(`main article[data-request="${rid}"]`);
      return c && !c.classList.contains('resolved') && !c.querySelector('.actions .btn.primary').disabled;
    }, { timeout: 4000 }, id).then(() => true, () => false);
    check('rollback: the card answered at once (optimistic)', optimistic);
    check('rollback: and came back when sending failed', rolled);
    check('rollback: with a toast that says why', await waitText(A, 'Can’t reach your computer', 2000));
  }
}
if (!any) check('rollback: an open permission request to test with', false);
A.off('request', failing);
await A.setRequestInterception(false);

// ---------- Stale protection: answered on another device ----------
await goto(A, demo, '/');
const push = await card(A, 'r_gitpush03');
check('stale: git push card is open on A', push && !(await push.$eval('.actions .btn.primary', (b) => b.disabled)));
await B.bringToFront();
const bCard = await card(B, 'r_gitpush03');
await (await bCard.$('.actions .btn.primary')).click();
await waitText(B, 'Allowed', 3000, 'main article[data-request="r_gitpush03"]');
await A.bringToFront();
const disabledOnA = await A.waitForFunction(() => {
  const c = document.querySelector('main article[data-request="r_gitpush03"]');
  return !c || (c.classList.contains('resolved') && [...c.querySelectorAll('.actions button')].every((b) => b.disabled));
}, { timeout: 4000 }).then(() => true, () => false);
check('stale: A’s card disables the moment B answers', disabledOnA);
check('stale: A says who answered', await waitText(A, 'on another device', 2000) || !(await card(A, 'r_gitpush03')));

check('no console or page errors on A', logsA.filter((l) => !l.includes('ERR_INTERNET_DISCONNECTED') && !l.includes('Failed to load resource')).length === 0, logsA.slice(0, 4).join(' | '));
await demo.stop();
process.exit(summary());
