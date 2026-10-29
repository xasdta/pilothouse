// 15. Pairing by scanning the QR code inside the app, with Chrome's fake
// camera showing a real `pilothouse pair` QR code:
//   - a new device scans on the pair screen (the browser's BarcodeDetector);
//   - a signed-in device that lost its key taps Pair again and scans, with
//     BarcodeDetector removed so the bundled jsQR decoder is used, as on iPhone.
//
//   PH_TMP=… PH_SHOTS=… node 15-demo-scan.mjs
import { execFileSync } from 'node:child_process';
import path from 'node:path';
import { startDemo, login, watch, check, summary, sleep, shot, waitText, scheme, PHONE } from './demo-lib.mjs';

const ROOT = new URL('../../../', import.meta.url).pathname;
const demo = await startDemo();

async function pairCode() {
  return (await (await fetch(demo.admin + '/v1/pair', { method: 'POST' })).json()).code;
}

/** A browser whose camera shows a QR code of text. */
async function cameraBrowser(text, name) {
  const file = path.join(demo.dir, name + '.mjpeg');
  execFileSync('go', ['run', './tests/e2e/web/qrframe', text, file], { cwd: ROOT, stdio: 'inherit' });
  return demo.browser({ args: ['--use-fake-ui-for-media-stream', '--use-fake-device-for-media-stream', '--use-file-for-fake-video-capture=' + file] });
}

// ---- A new device: scan on the pair screen ----
{
  const code = await pairCode();
  const b = await cameraBrowser(`${demo.base}/pair#${code}`, 'new');
  const page = await b.newPage();
  await page.setViewport(PHONE);
  await scheme(page, 'light');
  const logs = watch(page, 'new');
  await page.goto(demo.base + '/pair', { waitUntil: 'networkidle0' });
  logs.length = 0; // a signed-out /api/me answers 401 on load, by design
  const scan = await page.waitForSelector('button.scan-btn', { timeout: 5000 }).catch(() => null);
  check('pair screen offers Scan QR code', !!scan);
  await shot(page, '15-pair-scan-button');
  await scan.click();
  await page.waitForSelector('dialog.scan-sheet', { timeout: 5000 });
  await sleep(300);
  await shot(page, '15-scan-sheet');
  check('scanning pairs the device', await waitText(page, 'You’re paired', 15000));
  check('the camera is off after scanning', await page.evaluate(() => !document.querySelector('dialog.scan-sheet')));
  check('no console errors (new device)', logs.length === 0, logs.join('\n'));
}

// ---- Lost key: Pair again, scan, with jsQR (no BarcodeDetector, as on iPhone) ----
{
  const code = await pairCode();
  const b = await cameraBrowser(`${demo.base}/pair#${code}`, 'repair');
  const page = await b.newPage();
  await page.evaluateOnNewDocument(() => { delete self.BarcodeDetector; });
  await page.setViewport(PHONE);
  await scheme(page, 'light');
  const logs = watch(page, 'repair');
  await login(page, demo);
  const oldMe = await page.evaluate(() => fetch('/api/me').then((r) => r.json()));
  await page.evaluate(() => self.PilothouseKeys.clear());
  await page.reload({ waitUntil: 'domcontentloaded' });
  check('key card offers Pair again', await waitText(page, 'Pair again', 8000, '.attention'));
  await page.click('.attention a.btn');
  const scan = await page.waitForSelector('button.scan-btn', { timeout: 5000 }).catch(() => null);
  check('Pair again offers Scan QR code', !!scan);
  await shot(page, '15-repair-scan-button');
  await scan.click();
  check('scanning with jsQR pairs again', await waitText(page, 'You’re paired', 20000));
  check('jsQR was used', await page.evaluate(() => typeof self.jsQR === 'function'));
  await page.evaluate(() => [...document.querySelectorAll('button')].find((x) => x.innerText.includes('Open inbox')).click());
  await sleep(800);
  const newMe = await page.evaluate(() => fetch('/api/me').then((r) => r.json()));
  check('the device was replaced', newMe.device && newMe.device.id !== oldMe.device.id);
  check('key card gone', await page.$eval('.attention', (x) => x.hidden));
  check('no console errors (pair again)', logs.length === 0, logs.join('\n'));
}

// ---- A QR code for another computer is refused, and the camera keeps looking ----
{
  const b = await cameraBrowser('https://other-mac.example.ts.net/pair#AAAA-BBBB-CCCC', 'other');
  const page = await b.newPage();
  await page.setViewport(PHONE);
  await page.goto(demo.base + '/pair', { waitUntil: 'networkidle0' });
  await (await page.waitForSelector('button.scan-btn')).click();
  check('another computer’s code is explained', await waitText(page, 'That code is for other-mac.example.ts.net', 10000, 'dialog.scan-sheet'));
  await shot(page, '15-scan-wrong-computer');
  await page.keyboard.press('Escape');
  await sleep(300);
  check('closing the scanner leaves the code field', !!(await page.$('#pair-code')));
}

await demo.stop();
process.exit(summary() ? 1 : 0);
