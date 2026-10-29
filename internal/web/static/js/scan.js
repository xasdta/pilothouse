// In-app QR scanner for pairing.
//
// The iPhone Camera app opens a scanned link in Safari, which keeps its own
// storage, so a key made there can't sign in the Home Screen app. Scanning
// from inside the app keeps the pairing (and its key) here.
//
// Uses the browser's BarcodeDetector where there is one (Chrome on Android),
// otherwise jsQR (static/vendor/jsqr, Apache-2.0), loaded only when needed.
import { h, icon, fill, add } from './dom.js';
import { openSheet } from './ui.js';

const CODE_RE = /^[0-9A-Z]{4}-?[0-9A-Z]{4}-?[0-9A-Z]{4}$/i;

/** True when this page can use the camera. */
export const canScan = () => !!(window.isSecureContext && navigator.mediaDevices && navigator.mediaDevices.getUserMedia);

/**
 * The pairing code in scanned text: a /pair#CODE link to this computer, or
 * a bare code. Returns {code} or {error}.
 */
export function codeFromScan(text) {
  const t = String(text || '').trim();
  if (CODE_RE.test(t)) return { code: t.toUpperCase() };
  let u;
  try { u = new URL(t); } catch { return { error: 'That QR code isn’t a Pilothouse pairing code.' }; }
  const code = decodeURIComponent(u.hash.slice(1)).trim();
  if (u.pathname !== '/pair' || !CODE_RE.test(code)) return { error: 'That QR code isn’t a Pilothouse pairing code.' };
  if (u.origin !== location.origin) return { error: `That code is for ${u.host}, not this app (${location.host}).` };
  return { code };
}

let jsqrLoad = null;
function loadJsQR() {
  if (self.jsQR) return Promise.resolve(self.jsQR);
  if (!jsqrLoad) {
    jsqrLoad = new Promise((resolve, reject) => {
      const s = document.createElement('script');
      s.src = '/vendor/jsqr/jsQR.js';
      s.onload = () => resolve(self.jsQR);
      s.onerror = () => { jsqrLoad = null; reject(new Error('The scanner didn’t load. Type the code instead.')); };
      add(document.head, s);
    });
  }
  return jsqrLoad;
}

async function makeDetector() {
  if ('BarcodeDetector' in self) {
    try {
      const formats = await self.BarcodeDetector.getSupportedFormats();
      if (formats.includes('qr_code')) {
        const d = new self.BarcodeDetector({ formats: ['qr_code'] });
        return async (video) => { const r = await d.detect(video); return r[0] ? r[0].rawValue : ''; };
      }
    } catch { /* fall back to jsQR */ }
  }
  const jsQR = await loadJsQR();
  const canvas = document.createElement('canvas');
  const ctx = canvas.getContext('2d', { willReadFrequently: true });
  return async (video) => {
    const scale = Math.min(1, 720 / Math.max(video.videoWidth, video.videoHeight));
    canvas.width = Math.round(video.videoWidth * scale);
    canvas.height = Math.round(video.videoHeight * scale);
    ctx.drawImage(video, 0, 0, canvas.width, canvas.height);
    const img = ctx.getImageData(0, 0, canvas.width, canvas.height);
    const r = jsQR(img.data, img.width, img.height, { inversionAttempts: 'attemptBoth' });
    return r ? r.data : '';
  };
}

function cameraMessage(e) {
  switch (e && e.name) {
    case 'NotAllowedError':
    case 'SecurityError':
      return 'Camera access is off for Pilothouse. Turn it on in the iPhone Settings app, or type the code instead.';
    case 'NotFoundError':
    case 'OverconstrainedError':
      return 'No camera found. Type the code instead.';
    default:
      return (e && e.message) || 'The camera didn’t start. Type the code instead.';
  }
}

/** Opens the scanner; resolves to a pairing code, or '' if closed. */
export function scanPairCode() {
  return new Promise((resolve) => {
    let stream = null;
    let done = false;
    let timer = 0;
    const video = h('video', { class: 'scan-video', playsinline: '', muted: '', autoplay: '', 'aria-hidden': 'true' });
    video.muted = true;
    const status = h('p', { class: 'scan-status', role: 'status', 'aria-live': 'polite' }, 'Starting the camera…');
    const frame = h('div', { class: 'scan-frame' }, video, h('div', { class: 'scan-target', 'aria-hidden': 'true' }));
    const sheet = openSheet({
      title: 'Scan the pairing code',
      sub: 'Point the camera at the QR code that pilothouse pair shows on your computer.',
      body: [frame, status],
      cls: 'scan-sheet',
      onClose: () => finish(''),
    });

    function finish(code) {
      if (done) return;
      done = true;
      clearTimeout(timer);
      if (stream) for (const t of stream.getTracks()) t.stop();
      resolve(code);
      sheet.close();
    }
    function fail(msg) {
      frame.classList.add('off');
      status.classList.add('bad');
      fill(status, icon('alert', 'sm'), msg);
    }

    (async () => {
      let detect;
      try {
        stream = await navigator.mediaDevices.getUserMedia({ video: { facingMode: { ideal: 'environment' } }, audio: false });
        if (done) { for (const t of stream.getTracks()) t.stop(); return; }
        video.srcObject = stream;
        await video.play().catch(() => {});
        detect = await makeDetector();
      } catch (e) {
        fail(cameraMessage(e));
        return;
      }
      status.textContent = 'Looking for a QR code…';
      const tick = async () => {
        if (done) return;
        if (video.readyState >= 2 && video.videoWidth) {
          try {
            const text = await detect(video);
            if (text) {
              const r = codeFromScan(text);
              if (r.code) { finish(r.code); return; }
              status.textContent = r.error;
            }
          } catch { /* a frame that couldn't be read; try the next */ }
        }
        timer = setTimeout(tick, 150);
      };
      tick();
    })();
  });
}
