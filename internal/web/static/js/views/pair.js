import { h, icon, fill, add } from '../dom.js';
import { api } from '../api.js';
import { pushStatus } from '../push.js';

/** A friendly default name: "iPhone", "Pixel", "Chrome on Mac"… */
export function guessName() {
  const ua = navigator.userAgent;
  if (/iPhone/.test(ua)) return 'iPhone';
  if (/iPad/.test(ua) || (/Macintosh/.test(ua) && navigator.maxTouchPoints > 1)) return 'iPad';
  const pixel = ua.match(/Pixel \d+[a-zA-Z ]*/);
  if (pixel) return pixel[0].trim();
  if (/Android/.test(ua)) return 'Android phone';
  const browser = /Edg\//.test(ua) ? 'Edge' : /Firefox\//.test(ua) ? 'Firefox' : /Chrome\//.test(ua) ? 'Chrome' : /Safari\//.test(ua) ? 'Safari' : 'Browser';
  const os = /Mac OS X/.test(ua) ? 'Mac' : /Windows/.test(ua) ? 'Windows' : /Linux/.test(ua) ? 'Linux' : 'this computer';
  return `${browser} on ${os}`;
}

function messageFor(err) {
  switch (err.code) {
    case 'invalid_code': return 'That code didn’t work — it may be used or expired. Run pilothouse pair again and enter the new code.';
    case 'rate_limited': return 'Too many tries. Wait a minute and try again.';
    case 'network': return 'Can’t reach your computer. Is the daemon running?';
    default: return err.message || 'Pairing failed.';
  }
}

/**
 * Pair screen. mode 'login' (from `pilothouse open`) signs in automatically.
 * onDone(me) is called after the cookie is set.
 */
export function pairView({ code, mode, onDone }) {
  const root = h('div', { class: 'view pair' });
  const hero = (title, text) => h('div', { class: 'pair-hero' },
    h('img', { class: 'appicon', src: '/icons/icon-192.png', alt: '', width: 84, height: 84 }),
    h('h1', null, title), h('p', null, text));

  async function redeem(c, name) {
    if (!self.PilothouseKeys) throw new Error('Keys unavailable');
    const publicKey = await self.PilothouseKeys.create();
    return api('POST', '/api/pair', { code: c, name, public_key: publicKey });
  }

  if (mode === 'login' && code) {
    const err = h('p', { class: 'form-error', role: 'alert' });
    add(root, hero('Signing in…', 'Connecting this browser to your computer.'), err);
    redeem(code, guessName()).then(onDone, (e) => {
      fill(root, hero('Couldn’t sign in', messageFor(e)),
        h('div', { class: 'terminal-hint' }, h('span', { class: 'prompt' }, '$'), 'pilothouse open'));
    });
    return { el: root };
  }

  // Without a code in the link, the person types the one `pilothouse pair` shows.
  // A code from the link that fails is shown as failed, and the field to
  // type a new one appears.
  let linkCode = code;
  const err = h('p', { class: 'form-error', role: 'alert', 'aria-live': 'assertive' });
  const codeInput = h('input', {
    class: 'input code-input', id: 'pair-code', autocomplete: 'one-time-code', autocapitalize: 'characters',
    spellcheck: 'false', inputmode: 'text', placeholder: 'XXXX-XXXX-XXXX', maxlength: 20, required: true,
  });
  const codeField = (label) => h('div', null, h('label', { class: 'field-label', for: 'pair-code' }, label), codeInput);
  const codeSlot = h('div', { class: 'code-slot' },
    code
      ? h('div', { class: 'code-chip' }, icon('check', 'sm'), h('span', { class: 'sr-only' }, 'Pairing code '), code)
      : codeField('Pairing code'));
  const nameInput = h('input', { class: 'input', id: 'pair-name', value: guessName(), maxlength: 60, autocomplete: 'off', required: true });
  const submit = h('button', { class: 'btn primary block', type: 'submit' }, 'Pair this device');
  const form = h('form', { novalidate: true },
    codeSlot,
    h('div', null, h('label', { class: 'field-label', for: 'pair-name' }, 'Name this device'), nameInput),
    err,
    submit);

  function linkCodeFailed() {
    const failed = linkCode;
    linkCode = '';
    fill(codeSlot,
      h('div', { class: 'code-chip bad' }, icon('x', 'sm'),
        h('span', { class: 'sr-only' }, 'Pairing code '), h('s', null, failed), h('span', { class: 'sr-only' }, ' didn’t work')),
      codeField('Enter a new code'));
  }

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const c = linkCode || codeInput.value.trim();
    if (!c) { err.textContent = 'Enter the code shown by pilothouse pair.'; codeInput.focus(); return; }
    submit.disabled = true;
    fill(submit, h('span', { class: 'spin', 'aria-hidden': 'true' }), 'Pairing…');
    err.textContent = '';
    try {
      const me = await redeem(c, nameInput.value.trim() || guessName());
      showDone(me);
    } catch (e2) {
      err.textContent = messageFor(e2);
      submit.disabled = false;
      fill(submit, 'Pair this device');
      if (e2.code !== 'network' && e2.code !== 'rate_limited') {
        if (linkCode) linkCodeFailed();
        else codeInput.select();
        codeInput.focus();
      }
    }
  });

  add(root,
    hero('Pair with Pilothouse', code
      ? 'Answer your coding agents from this device when you’re away from your desk.'
      : 'Run pilothouse pair on your computer and scan the QR code, or type the code here.'),
    form,
    h('p', { class: 'fineprint' }, icon('key', 'sm'),
      'A private key is created on this device and never leaves it. Every answer you send is signed with it.'));

  async function showDone(me) {
    const st = await pushStatus().catch(() => 'unsupported');
    // Pairing is complete either way: notifications are an option, not a gate.
    const offerNotifications = st === 'off' || st === 'ios-install';
    const open = h('button', { class: 'btn primary block', type: 'button', onclick: () => onDone(me, false) }, 'Open inbox');
    const actions = h('div', { class: 'pair-actions' },
      open,
      offerNotifications
        ? h('button', { class: 'btn secondary block', type: 'button', onclick: () => onDone(me, true) }, 'Set up notifications')
        : null);
    fill(root,
      h('div', { class: 'pair-hero' },
        h('div', { class: 'done-mark' }, icon('check')),
        h('h1', null, 'You’re paired'),
        h('p', null, `${me.device.name} can now answer for ${me.machine}.`)),
      actions,
      offerNotifications
        ? h('p', { class: 'fineprint' }, icon('bell', 'sm'), 'You can turn on notifications later in Settings.')
        : null);
    open.focus();
  }

  return { el: root, focus: () => (code ? nameInput : codeInput).focus() };
}
