import { execSync } from 'node:child_process';
import { BASE, launch, pairViaUI, sleep } from './lib.mjs';
const variant = process.argv[2];
const args = variant === 'nobfcache' ? ['--disable-features=BackForwardCache'] : [];
const browser = await launch({ args });
const page = await browser.newPage();
await pairViaUI(page, 'nav ' + variant);
const cdp = await page.createCDPSession();
if (variant === 'nosw') { await cdp.send('Network.enable'); await cdp.send('Network.setBypassServiceWorker', { bypass: true }); }
const conns = () => execSync(`lsof -nP -iTCP:7592 -sTCP:ESTABLISHED | grep -c '^Google' || true`, { encoding: 'utf8' }).trim();
let res = 'no hang in 12';
for (let i = 0; i < 12; i++) {
  try {
    if (variant === 'reload' && i) await page.reload({ waitUntil: 'domcontentloaded', timeout: 6000 });
    else await page.goto(BASE + (i % 2 ? '/sessions' : '/settings'), { waitUntil: 'domcontentloaded', timeout: 6000 });
  } catch { res = `hang at load #${i + 1}, conns=${conns()}`; break; }
  await sleep(700);
}
console.log(variant, '→', res, 'final conns', conns());
await browser.close();
