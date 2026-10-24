// 2. Live inbox: held requests appear without reload; Allow / Deny+reason /
// Allow-for-session reach the waiting hook; two devices race → one 409.
import { BASE, launch, watch, pairViaUI, shot, text, waitText, check, summary, hold, readOut, sleep } from './lib.mjs';

const browser = await launch();
const ctxA = await browser.createBrowserContext();
const ctxB = await browser.createBrowserContext();
const A = await ctxA.newPage();
const B = await ctxB.newPage();
await A.setViewport({ width: 390, height: 844, isMobile: true, hasTouch: true, deviceScaleFactor: 2 });
await B.setViewport({ width: 1280, height: 860 });
watch(A, 'A'); watch(B, 'B');

const pa = await pairViaUI(A, 'Device A');
const pb = await pairViaUI(B, 'Device B');
check('A paired', pa.status === 200); check('B paired', pb.status === 200);
for (const p of [A, B]) {
  await p.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await p.waitForSelector('.page-title');
  await p.evaluate(() => { window.__noReload = 1; });
}
await sleep(1000);
await shot(A, '02-inbox-empty-mobile');

// Track decide responses per page.
const decides = { A: [], B: [] };
// 0.2 cards answer with the signed v2 /respond (0.1 used /decide).
const isAnswer = (u) => u.includes('/decide') || u.includes('/respond');
A.on('response', (r) => { if (isAnswer(r.url())) decides.A.push(r.status()); });
B.on('response', (r) => { if (isAnswer(r.url())) decides.B.push(r.status()); });

const cardSel = (needle) => `//article[contains(@class,'req')][.//pre[contains(., '${needle}')]]`;
async function card(page, needle, timeout = 8000) {
  try { return await page.waitForSelector('xpath/' + cardSel(needle), { timeout }); } catch { return null; }
}
async function clickIn(page, needle, sel) {
  const c = await card(page, needle);
  const b = await c.$(sel);
  await b.click();
}

// ---- Allow ----
hold('allow', 'echo qa-allow');
const t0 = Date.now();
const cA = await card(A, 'qa-allow');
check('request appears live on A without reload', !!cA && (await A.evaluate(() => window.__noReload)) === 1, `${Date.now() - t0} ms`);
check('request appears live on B', !!(await card(B, 'qa-allow')));
check('tab title shows count', (await A.title()).startsWith('(1)'), await A.title());
await shot(A, '02-inbox-request-mobile');
await shot(B, '02-inbox-request-desktop');
await clickIn(A, 'qa-allow', '.actions .btn.primary');
const outAllow = await readOut('allow');
let j = {}; try { j = JSON.parse(outAllow); } catch { /* */ }
check('hook prints allow JSON', j.hookSpecificOutput && j.hookSpecificOutput.decision && j.hookSpecificOutput.decision.behavior === 'allow', outAllow);
check('A shows "Allowed"', await waitText(A, 'Allowed', 3000));
await sleep(400);
await shot(A, '02-inbox-allowed-mobile');
check('B drops the card and lists it under Recently answered', await waitText(B, 'RECENTLY ANSWERED', 3000) || await waitText(B, 'Recently answered', 100));
check('card header has no literal "null" (repo without branch)', !(await text(A, 'main')).includes('projnull'), 'card.js:214');

// ---- Deny with reason ----
hold('deny', 'git push origin main');
await card(A, 'git push origin main');
await clickIn(A, 'git push origin main', '.actions .btn.danger');
await sleep(300);
const reason = await (await card(A, 'git push origin main')).$('textarea');
await reason.type('Use a feature branch, not main');
await shot(A, '02-inbox-deny-form-mobile');
await clickIn(A, 'git push origin main', '.deny-form .btn.danger');
const outDeny = await readOut('deny');
try { j = JSON.parse(outDeny); } catch { j = {}; }
const dd = j.hookSpecificOutput && j.hookSpecificOutput.decision;
check('hook prints deny JSON', dd && dd.behavior === 'deny', outDeny);
check('deny message carries reason', dd && (dd.message || '').includes('Use a feature branch'), dd && dd.message);

// ---- Allow for session ----
const sid = hold('sess', 'npm test --silent');
await card(A, 'npm test');
await clickIn(A, 'npm test', '.actions .btn.secondary');
const outSess = await readOut('sess');
try { j = JSON.parse(outSess); } catch { j = {}; }
const ds = j.hookSpecificOutput && j.hookSpecificOutput.decision;
check('hook prints allow for session', ds && ds.behavior === 'allow', outSess);
// Same session, same prefix → granted automatically (no card).
hold('sess2', 'npm test --watch=false', sid);
const outSess2 = await readOut('sess2', 6000);
check('second request in same session auto-allowed by grant', /"allow"/.test(outSess2), outSess2 || '(no output within 6s — held?)');

// ---- Race: both devices click Allow at once ----
decides.A.length = 0; decides.B.length = 0;
hold('race', 'echo qa-race');
await card(A, 'qa-race'); await card(B, 'qa-race');
const ba = await (await card(A, 'qa-race')).$('.actions .btn.primary');
const bb = await (await card(B, 'qa-race')).$('.actions .btn.primary');
await Promise.all([ba.click(), bb.click()]);
const outRace = await readOut('race');
await sleep(1500);
const all = [...decides.A, ...decides.B].sort();
check('race: exactly one 200 and one 409', JSON.stringify(all) === '[200,409]', `A=${decides.A} B=${decides.B}`);
const loser = decides.A[0] === 409 ? A : B;
const loserTxt = await text(loser, 'body');
check('loser shows "Already answered"', /Already answered/i.test(loserTxt), loserTxt.slice(0, 400));
check('loser card shows "on another device"', /on another device/.test(loserTxt));
await shot(loser, '02-race-loser');
check('race: hook got exactly one answer (allow)', /"allow"/.test(outRace), outRace);

for (const l of [...A._logs, ...B._logs]) console.log(l);
await browser.close();
process.exit(summary());
