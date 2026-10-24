// 30. The PWA's live terminal against a real daemon and a real holder (0.2,
// workstream B), in headless Chrome at phone size:
//   - pairing in the browser (non-extractable key), the view mints its
//     token with a signed `terminal` action and streams the replay;
//   - with nobody attached at the computer the view fits the phone and
//     resizes the agent's terminal; typing reaches the agent;
//   - with a terminal attached at the computer the size is fixed (the view
//     gets `size`, keeps it and sends no resize) and the desk terminal is
//     never detached.
// Needs an isolated daemon (see tests/e2e/claude/README.md) and its
// environment: PILOTHOUSE_HOME/_USER_HOME/_RUN_DIR, PH_BIN (the pilothouse
// binary), PH_BASE (its web URL, e.g. http://127.0.0.1:7593).
//   node 30-terminal-real.mjs
import { spawn, spawnSync, execFileSync } from 'node:child_process';
import path from 'node:path';
import fs from 'node:fs';
import os from 'node:os';
import { fileURLToPath } from 'node:url';
import { launch, check, summary, sleep } from './lib.mjs';

const BIN = process.env.PH_BIN;
const BASE = process.env.PH_BASE;
if (!BIN || !BASE || !process.env.PILOTHOUSE_RUN_DIR) {
  console.error('set PH_BIN, PH_BASE and the PILOTHOUSE_* variables of an isolated daemon');
  process.exit(2);
}
const DETACH = path.join(path.dirname(fileURLToPath(import.meta.url)), '..', 'claude', 'detach.py');
const person = (...args) => execFileSync('python3', [DETACH, BIN, ...args], { encoding: 'utf8' });
const sessions = () => JSON.parse(execFileSync(BIN, ['ls', '--all', '--json'], { encoding: 'utf8' })).sessions;
const PHONE = { width: 390, height: 844, deviceScaleFactor: 2, isMobile: true, hasTouch: true };
const rowsText = (page) => page.$eval('.xterm-rows', (e) => e.innerText).catch(() => '');
const waitRows = (page, needle, timeout = 8000) => page.waitForFunction(
  (n) => { const e = document.querySelector('.xterm-rows'); return e && e.innerText.includes(n); }, { timeout, polling: 100 }, needle,
).then(() => true, () => false);

// Runs argv in an 80x24 PTY like a terminal at the computer, copying its
// output to stdout until it exits or this process gets SIGTERM.
const DESK_PTY = `
import fcntl, os, pty, signal, struct, sys, termios
pid, fd = pty.fork()
if pid == 0:
    os.execv(sys.argv[1], sys.argv[1:])
fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 24, 80, 0, 0))
signal.signal(signal.SIGTERM, lambda *a: (os.kill(pid, signal.SIGTERM), os._exit(0)))
while True:
    try:
        b = os.read(fd, 65536)
    except OSError:
        break
    if not b:
        break
    sys.stdout.buffer.write(b); sys.stdout.flush()
`;

// A wrapped program that echoes lines and its terminal size.
const name = `view-${Date.now()}`;
const sid = execFileSync(BIN, ['run', '-d', '-n', name, '--', '/bin/sh', '-c',
  'echo VIEWREADY; while read l; do echo "typed:$l"; stty size | sed "s/^/size:/"; done'], { encoding: 'utf8' }).trim();
let desk = null;
const udd = fs.mkdtempSync(path.join(os.tmpdir(), 'ph-e2e-b-'));
const browser = await launch({ dir: udd });
try {
  for (let i = 0; i < 50 && !sessions().find((s) => s.id === sid)?.caps?.watch; i++) await sleep(100);
  check('wrapped session offers the live terminal', !!sessions().find((s) => s.id === sid)?.caps?.watch);

  const page = await browser.newPage();
  await page.emulate({ viewport: PHONE, userAgent: 'Mozilla/5.0 (iPhone) e2e' });
  const code = JSON.parse(person('pair', '--json', '--no-wait', '--no-qr')).code;
  await page.goto(`${BASE}/pair#${code}`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('#pair-name');
  const paired = page.waitForResponse((r) => r.url().endsWith('/api/pair'));
  await page.click('button[type=submit]');
  check('paired in the browser', (await paired).status() === 200);

  const terminalRequests = [];
  page.on('request', (r) => { if (r.url().includes('/terminal')) terminalRequests.push({ url: r.url(), method: r.method(), body: r.postData() || '' }); });

  // Nobody attached: the view fits the phone and resizes the agent's terminal.
  await page.goto(`${BASE}/s/${sid}/terminal`, { waitUntil: 'domcontentloaded' });
  check('replay renders', await waitRows(page, 'VIEWREADY'));
  for (let i = 0; i < 40 && !terminalRequests.some((r) => r.body.includes('"resize"')); i++) await sleep(100);
  const resize = terminalRequests.find((r) => r.body.includes('"resize"'));
  check('view sends its size when nothing is attached', !!resize, resize && resize.body);
  await page.click('.term-screen');
  await page.keyboard.type('hello');
  await page.keyboard.press('Enter');
  check('typing reaches the agent', await waitRows(page, 'typed:hello'));
  const fitted = (await rowsText(page)).match(/size:(\d+) (\d+)/);
  const want = resize && JSON.parse(resize.body).resize;
  check('the agent\'s terminal took the phone\'s size', fitted && want && +fitted[1] === want.rows && +fitted[2] === want.cols,
    `${fitted && fitted[0]} vs ${JSON.stringify(want)}`);
  check('frame not fixed', !(await page.$eval('.term-frame', (e) => e.classList.contains('fixed'))));

  // A terminal attached at the computer (an 80x24 PTY): the size is fixed,
  // the view keeps it.
  desk = spawn('python3', ['-c', DESK_PTY, BIN, 'attach', sid], { stdio: ['ignore', 'pipe', 'pipe'], env: process.env });
  let deskOut = '';
  desk.stdout.on('data', (b) => { deskOut += b.toString(); });
  await sleep(1500);
  const before = terminalRequests.length;
  await page.reload({ waitUntil: 'domcontentloaded' });
  check('replay renders again', await waitRows(page, 'VIEWREADY'));
  await page.waitForFunction(() => document.querySelector('.term-frame.fixed'), { timeout: 5000 }).then(() => true, () => false)
    .then((ok) => check('view keeps the computer\'s size (fixed)', ok));
  await page.click('.term-screen');
  await page.keyboard.type('again');
  await page.keyboard.press('Enter');
  check('typing still works', await waitRows(page, 'typed:again'));
  const after = terminalRequests.slice(before).filter((r) => r.body.includes('"resize"'));
  check('no resize sent while the computer\'s terminal is attached', after.length === 0, JSON.stringify(after));
  const lines = (await rowsText(page)).match(/size:\d+ \d+/g) || [];
  check('the desk terminal\'s size stayed', lines.length > 0 && lines[lines.length - 1] !== `size:${want.rows} ${want.cols}`, lines.join(' '));
  check('desk terminal still attached and saw the typing', desk.exitCode === null && deskOut.includes('typed:again') && !deskOut.includes('detached'));
} finally {
  if (desk && desk.exitCode === null) desk.kill('SIGTERM');
  await browser.close();
  fs.rmSync(udd, { recursive: true, force: true });
  // End the wrapped program (its holder lingers a few seconds, then exits).
  const s = sessions().find((x) => x.id === sid);
  if (s && s.pid) { try { process.kill(s.pid, 'SIGTERM'); } catch { /* gone */ } }
  spawnSync('true');
}
process.exit(summary() ? 1 : 0);
