// Live terminal for wrapped sessions (F3; workstream I). xterm.js is
// vendored under /vendor/xterm and loaded only when this view opens.
//
// Contract (docs/WORKSTREAMS-0.2.md, internal/web/terminal.go):
//   POST /api/sessions/{id}/terminal/token  signed `terminal`, payload {}
//        → {token, expires_at}; one signature covers 10 minutes of typing.
//   GET  /api/sessions/{id}/terminal?replay=1  SSE: `data` (base64 output),
//        `exit` ({"code": n}); optional `size` ({"cols","rows"}) when the
//        computer's terminal has a fixed size.
//   POST /api/sessions/{id}/terminal/input  X-Terminal-Token,
//        {"data": "<base64>"} or {"resize": {"cols", "rows"}}.
// The stream is read with fetch (not EventSource) so a 501/404 is told
// apart from a dropped connection, and reconnects back off.

import { h, agentName, repoName, fill, add, toMs } from '../dom.js';
import { store, api } from '../api.js';
import { link } from '../ui.js';
import { ico, postAction, actionError, spinner } from './session.js';

const XTERM = '/vendor/xterm/xterm.mjs';
const FIT = '/vendor/xterm/addon-fit.mjs';
const XTERM_CSS = '/vendor/xterm/xterm.css';
const FONT_KEY = 'pilothouse.termFont';
const MAX_INPUT = 48 << 10; // bytes per POST; bigger pastes are split

let libs = null;
function loadXterm() {
  if (!libs) {
    let css = document.querySelector(`link[href="${XTERM_CSS}"]`);
    const cssReady = css ? Promise.resolve() : new Promise((resolve) => {
      css = h('link', { rel: 'stylesheet', href: XTERM_CSS });
      css.addEventListener('load', resolve);
      css.addEventListener('error', resolve);
      add(document.head, css);
    });
    libs = Promise.all([import(XTERM), import(FIT), cssReady])
      .then(([x, f]) => ({ Terminal: x.Terminal, FitAddon: f.FitAddon }))
      .catch((e) => { libs = null; throw e; });
  }
  return libs;
}

// ---------- CSP shims for xterm's DOM renderer ----------
//
// The CSP (style-src 'self') refuses inline <style> elements and style
// attributes. xterm 6 creates three <style> elements while opening (its
// theme, cell size and scrollbar rules) and colours truecolor cells with
// setAttribute('style'). Both are routed through the CSSOM instead, which
// the CSP allows: the <style> elements are created as inert <template>s
// whose text is mirrored into constructable stylesheets, and cell styles
// go through element.style.

function openWithAdoptedStyles(term, parent) {
  const made = [];
  const own = Object.prototype.hasOwnProperty.call(document, 'createElement');
  const orig = document.createElement;
  document.createElement = function (tag, opts) {
    if (String(tag).toLowerCase() === 'style' && typeof CSSStyleSheet === 'function' && 'adoptedStyleSheets' in document) {
      const el = orig.call(document, 'template');
      el.dataset.xtermStyle = '';
      made.push(el);
      return el;
    }
    return orig.call(document, tag, opts);
  };
  try {
    term.open(parent);
  } finally {
    if (own) document.createElement = orig;
    else delete document.createElement;
  }
  const sheets = made.map((el) => {
    const sheet = new CSSStyleSheet();
    let text = null;
    const sync = () => {
      if (el.textContent === text) return;
      text = el.textContent;
      try { sheet.replaceSync(text); } catch { /* keep the last good rules */ }
    };
    sync();
    const mo = new MutationObserver(sync);
    mo.observe(el, { childList: true, characterData: true, subtree: true });
    return { sheet, mo };
  });
  document.adoptedStyleSheets = [...document.adoptedStyleSheets, ...sheets.map((s) => s.sheet)];
  patchCellStyles(term);
  return () => {
    for (const s of sheets) s.mo.disconnect();
    const gone = new Set(sheets.map((s) => s.sheet));
    document.adoptedStyleSheets = document.adoptedStyleSheets.filter((s) => !gone.has(s));
  };
}

function patchCellStyles(term) {
  try {
    const renderer = term._core._renderService._renderer.value;
    const proto = Object.getPrototypeOf(renderer._rowFactory);
    if (typeof proto._addStyle === 'function' && !proto._addStyle.cssom) {
      const viaCSSOM = function (el, style) { el.style.cssText += style + ';'; };
      viaCSSOM.cssom = true;
      proto._addStyle = viaCSSOM;
    }
  } catch {
    // Another xterm build: truecolor cells fall back to the palette.
  }
}

// ---------- Bytes ----------

function toB64(text) {
  const bytes = new TextEncoder().encode(text);
  let s = '';
  for (let i = 0; i < bytes.length; i += 0x8000) s += String.fromCharCode.apply(null, bytes.subarray(i, i + 0x8000));
  return btoa(s);
}

function fromB64(b64) {
  const s = atob(b64);
  const out = new Uint8Array(s.length);
  for (let i = 0; i < s.length; i++) out[i] = s.charCodeAt(i);
  return out;
}

/** Splits text so no chunk encodes to more than max UTF-8 bytes. */
function chunks(text, max) {
  const out = [];
  let cur = '';
  let n = 0;
  for (const ch of text) {
    const b = new TextEncoder().encode(ch).length;
    if (n + b > max && cur) { out.push(cur); cur = ''; n = 0; }
    cur += ch;
    n += b;
  }
  if (cur) out.push(cur);
  return out;
}

// ---------- Quick keys ----------

const CSI = (app, c) => (app ? '\x1bO' : '\x1b[') + c;
// `area` places each key in the CSS grid: two rows on phones with the
// arrows as an inverted T, one row on wide screens.
const KEYS = [
  { area: 'esc', label: 'Esc', name: 'Escape', seq: () => '\x1b' },
  { area: 'ctrlc', label: 'Ctrl-C', name: 'Control C', seq: () => '\x03' },
  { area: 'tab', label: 'Tab', name: 'Tab', seq: () => '\t' },
  { area: 'up', icon: 'up', name: 'Up arrow', seq: (app) => CSI(app, 'A') },
  { area: 'down', icon: 'down', name: 'Down arrow', seq: (app) => CSI(app, 'B') },
  { area: 'left', icon: 'left', name: 'Left arrow', seq: (app) => CSI(app, 'D') },
  { area: 'right', icon: 'right', name: 'Right arrow', seq: (app) => CSI(app, 'C') },
  { area: 'enter', label: 'Enter', name: 'Enter', seq: () => '\r' },
  { area: 'y', label: 'y', name: 'y', seq: () => 'y' },
  { area: 'n', label: 'n', name: 'n', seq: () => 'n' },
];

// ---------- The view ----------

export function terminalView(id) {
  const enc = encodeURIComponent(id);
  const root = h('div', { class: 'view term-view' });
  const title = h('div', { class: 'term-title' });
  const statusEl = h('span', { class: 'term-status', role: 'status', 'aria-live': 'polite' });
  const modeEl = h('span', { class: 'term-mode' });
  // The accessible name starts with the visible text (WCAG 2.5.3).
  const fontDown = h('button', { class: 'term-tool', type: 'button', 'aria-label': 'A− smaller text', title: 'Smaller text' }, 'A−');
  const fontUp = h('button', { class: 'term-tool', type: 'button', 'aria-label': 'A+ larger text', title: 'Larger text' }, 'A+');
  const bar = h('div', { class: 'term-bar' }, statusEl, modeEl, h('span', { class: 'spacer' }), fontDown, fontUp);
  const screen = h('div', { class: 'term-screen', role: 'region', 'aria-label': 'Terminal' });
  const frame = h('div', { class: 'term-frame' }, screen);
  const keysEl = h('div', { class: 'term-keys', role: 'toolbar', 'aria-label': 'Terminal keys', hidden: true });
  const banner = h('div', { class: 'term-banner', hidden: true });
  add(root, title, bar, frame, banner, keysEl);

  let destroyed = false;
  let term = null;
  let fit = null;
  let undoStyles = null;
  let ro = null;
  let ctrl = null;
  let retryTimer = 0;
  let backoff = 0;
  let connectedAt = 0;
  let exited = false;
  let gone = false; // no terminal to show at all
  let gotData = false; // the stream delivered its first frame
  let fixedSize = null; // {cols, rows} from the computer, when it sends one
  let token = null;
  let minting = null;
  let readOnly = '';
  let queue = [];
  let flushTimer = 0;
  let flushing = false;
  let resizeTimer = 0;
  let lastSize = '';
  let fontSize = Number(localStorage.getItem(FONT_KEY)) || (matchMedia('(max-width: 600px)').matches ? 12 : 13);

  function renderTitle() {
    const s = store.sessions.get(id);
    fill(title,
      h('h1', { class: 'term-h' }, s ? repoName(s) : 'Terminal'),
      s ? h('span', { class: 'term-sub' }, agentName(s.agent), s.branch ? ' · ' + s.branch : '') : null,
      h('span', { class: 'spacer' }),
      link('/s/' + id, { class: 'term-link' }, 'Activity', ico('right', 'sm')));
  }

  function setStatus(kind, text) {
    statusEl.className = 'term-status ' + kind;
    fill(statusEl, kind === 'connecting' || kind === 'reconnecting' ? spinner('sm') : h('span', { class: 'dot' }), h('span', null, text));
  }

  function setMode() {
    if (exited) {
      fill(modeEl);
    } else if (readOnly) {
      modeEl.className = 'term-mode ro';
      fill(modeEl, ico('eye', 'sm'), 'Watching');
      modeEl.title = readOnly;
    } else if (token) {
      modeEl.className = 'term-mode rw';
      fill(modeEl, ico('keyboard', 'sm'), 'You can type');
      modeEl.title = '';
    } else {
      fill(modeEl);
    }
    keysEl.hidden = !!readOnly || exited || !token;
    if (term) term.options.disableStdin = !!readOnly || exited;
  }

  function showBanner(kind, kids) {
    banner.hidden = false;
    banner.className = 'term-banner ' + kind;
    fill(banner, kids);
  }

  function goReadOnly(reason) {
    if (gone) return;
    readOnly = reason || 'Typing isn’t allowed in this session — you can watch.';
    token = null;
    queue = [];
    setMode();
    showBanner('info', [ico('eye', 'sm'), h('span', null, readOnly)]);
  }

  // ---- Input ----
  async function ensureToken() {
    if (readOnly || exited) return null;
    if (token && toMs(token.expires_at) - Date.now() > 30000) return token.token;
    if (!minting) {
      minting = (async () => {
        const res = await postAction(`/api/sessions/${enc}/terminal/token`, 'terminal', id, {});
        if (destroyed) return null;
        if (res.ok && res.data && res.data.token) {
          token = res.data;
          setMode();
          return token.token;
        }
        token = null;
        if (res.status === 0) {
          showBanner('warn', [ico('info', 'sm'), h('span', null, actionError(res))]);
          return null;
        }
        goReadOnly(actionError(res, 'Typing isn’t available for this session — you can watch.'));
        return null;
      })().finally(() => { minting = null; });
    }
    return minting;
  }

  async function postInput(body, retried = false) {
    const t = await ensureToken();
    if (!t) return false;
    let res;
    try {
      res = await fetch(`/api/sessions/${enc}/terminal/input`, {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json', 'X-Pilothouse': '1', 'X-Terminal-Token': t },
        body: JSON.stringify(body),
      });
    } catch {
      showBanner('warn', [ico('info', 'sm'), h('span', null, 'Can’t reach your computer — keys weren’t sent.')]);
      return false;
    }
    if (res.ok) {
      if (!banner.hidden && banner.classList.contains('warn')) banner.hidden = true;
      return true;
    }
    const d = await res.json().catch(() => ({}));
    if ((res.status === 401 || res.status === 403) && !retried && d.code !== 'unauthorized') {
      token = null; // expired or revoked: mint a fresh one once
      return postInput(body, true);
    }
    if (res.status === 401) { location.assign('/pair'); return false; }
    if (res.status === 409 || res.status === 404) return false; // the session ended; the stream says so
    goReadOnly(d.error || 'Typing isn’t allowed in this session — you can watch.');
    return false;
  }

  function send(text) {
    if (readOnly || exited || !text) return;
    queue.push(text);
    if (!flushTimer) flushTimer = setTimeout(flush, 12);
  }

  /** Raw bytes (xterm's onBinary: some mouse reports), sent as is. */
  async function sendBinary(bytes) {
    if (readOnly || exited || !bytes) return;
    await flush();
    postInput({ data: btoa(bytes) });
  }

  async function flush() {
    flushTimer = 0;
    if (flushing) return;
    flushing = true;
    while (queue.length && !destroyed) {
      const text = queue.join('');
      queue = [];
      for (const part of chunks(text, MAX_INPUT)) {
        if (!(await postInput({ data: toB64(part) }))) { queue = []; break; }
      }
    }
    flushing = false;
  }

  function sendResize() {
    if (!term || !gotData || fixedSize || readOnly || exited || !token) return;
    const size = `${term.cols}x${term.rows}`;
    if (size === lastSize) return;
    lastSize = size;
    postInput({ resize: { cols: term.cols, rows: term.rows } });
  }

  // ---- Layout ----
  function relayout() {
    if (!term || !fit) return;
    if (fixedSize) {
      // The computer's terminal sets the size: shrink the text until that
      // many columns fit (down to 7 px), then let the frame scroll sideways.
      if (term.cols !== fixedSize.cols || term.rows !== fixedSize.rows) term.resize(fixedSize.cols, fixedSize.rows);
      let cell = 0;
      try { cell = term._core._renderService.dimensions.css.cell.width; } catch { /* not rendered yet */ }
      if (cell > 0) {
        const cur = term.options.fontSize;
        const fits = Math.floor((cur * (frame.clientWidth - 12)) / (cell * fixedSize.cols));
        const want = Math.max(7, Math.min(fontSize, fits));
        if (want !== cur) term.options.fontSize = want;
      }
      return;
    }
    if (term.options.fontSize !== fontSize) term.options.fontSize = fontSize;
    try { fit.fit(); } catch { /* not laid out yet */ }
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(sendResize, 150);
  }

  function setFont(d) {
    fontSize = Math.max(8, Math.min(22, fontSize + d));
    try { localStorage.setItem(FONT_KEY, String(fontSize)); } catch { /* not remembered */ }
    relayout();
  }
  fontDown.addEventListener('click', () => setFont(-1));
  fontUp.addEventListener('click', () => setFont(1));

  function theme() {
    const cs = getComputedStyle(document.documentElement);
    const v = (n, d) => cs.getPropertyValue(n).trim() || d;
    // The code background is dark in both themes. These ANSI colours read
    // at ≥ 4.5:1 on it (xterm's defaults don't: its red is 3.1:1), and
    // minimumContrastRatio lifts anything an agent paints darker.
    return {
      background: v('--code-bg', '#10141a'),
      foreground: v('--code-text', '#e6edf3'),
      cursor: v('--code-text', '#e6edf3'),
      cursorAccent: v('--code-bg', '#10141a'),
      selectionBackground: 'rgba(121, 167, 255, 0.35)',
      black: '#484f58', red: '#ff7b72', green: '#3fb950', yellow: '#d29922',
      blue: '#58a6ff', magenta: '#bc8cff', cyan: '#39c5cf', white: '#b1bac4',
      brightBlack: '#6e7681', brightRed: '#ffa198', brightGreen: '#56d364', brightYellow: '#e3b341',
      brightBlue: '#79c0ff', brightMagenta: '#d2a8ff', brightCyan: '#56d4dd', brightWhite: '#ffffff',
    };
  }
  const scheme = matchMedia('(prefers-color-scheme: dark)');
  const onScheme = () => { if (term) term.options.theme = theme(); };

  // ---- Stream ----
  async function connect() {
    clearTimeout(retryTimer);
    if (destroyed || exited) return;
    if (ctrl) ctrl.abort();
    const mine = new AbortController();
    ctrl = mine;
    setStatus(connectedAt ? 'reconnecting' : 'connecting', connectedAt ? 'Reconnecting…' : 'Connecting…');
    let res;
    try {
      res = await fetch(`/api/sessions/${enc}/terminal?replay=1`, {
        signal: mine.signal, credentials: 'same-origin', cache: 'no-store', headers: { Accept: 'text/event-stream' },
      });
    } catch {
      if (!mine.signal.aborted) retry();
      return;
    }
    if (!res.ok) {
      const d = await res.json().catch(() => ({}));
      if (res.status === 401) { location.assign('/pair'); return; }
      if (res.status === 501 || res.status === 404 || res.status === 403) {
        unavailable(res.status, d.error);
        return;
      }
      retry();
      return;
    }
    connectedAt = Date.now();
    gotData = false; // resend the size with this connection's first frame
    lastSize = '';
    term.reset(); // the replay redraws the screen from the scrollback
    setStatus('live', 'Live');
    if (!banner.hidden && banner.classList.contains('warn')) banner.hidden = true;
    const reader = res.body.getReader();
    const dec = new TextDecoder();
    let buf = '';
    let ev = { event: 'message', data: [] };
    try {
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let nl;
        while ((nl = buf.indexOf('\n')) >= 0) {
          let line = buf.slice(0, nl);
          buf = buf.slice(nl + 1);
          if (line.endsWith('\r')) line = line.slice(0, -1);
          if (line === '') {
            dispatch(ev);
            ev = { event: 'message', data: [] };
            if (exited) { mine.abort(); return; }
            continue;
          }
          if (line[0] === ':') continue;
          const c = line.indexOf(':');
          const field = c < 0 ? line : line.slice(0, c);
          let val = c < 0 ? '' : line.slice(c + 1);
          if (val[0] === ' ') val = val.slice(1);
          if (field === 'event') ev.event = val;
          else if (field === 'data') ev.data.push(val);
        }
      }
    } catch {
      if (mine.signal.aborted) return;
    }
    if (ctrl !== mine || destroyed || exited) return;
    if (Date.now() - connectedAt > 5000) backoff = 0; // it was up for a while: start over
    retry();
  }

  function dispatch(ev) {
    const data = ev.data.join('\n');
    switch (ev.event) {
      case 'data':
        try { term.write(fromB64(data)); } catch { /* not base64: skip the frame */ }
        if (!gotData) {
          // A `size` event comes before the replay; only now is it known
          // whether this screen may set the size.
          gotData = true;
          sendResize();
        }
        break;
      case 'size':
      case 'resize': {
        let s = {};
        try { s = JSON.parse(data); } catch { /* ignore */ }
        if (s.cols > 1 && s.rows > 1 && s.cols <= 1000 && s.rows <= 500) {
          fixedSize = { cols: s.cols | 0, rows: s.rows | 0 };
          frame.classList.add('fixed');
          relayout();
        }
        break;
      }
      case 'exit': {
        let code = null;
        try { code = JSON.parse(data).code; } catch { /* no code */ }
        ended(code);
        break;
      }
    }
  }

  function retry() {
    if (destroyed || exited) return;
    const delay = Math.round(Math.min(15000, 500 * 2 ** backoff) * (0.8 + Math.random() * 0.4));
    backoff = Math.min(backoff + 1, 6);
    setStatus('reconnecting', `Reconnecting in ${Math.max(1, Math.round(delay / 1000))} s`);
    retryTimer = setTimeout(connect, delay);
  }

  function ended(code) {
    exited = true;
    clearTimeout(retryTimer);
    setStatus('ended', code == null ? 'Ended' : `Exited · code ${code}`);
    setMode();
    showBanner(code ? 'bad' : 'info', [
      ico('info', 'sm'),
      h('span', null, code ? `The agent exited with code ${code}.` : 'The session ended.'),
      link('/s/' + id, { class: 'term-banner-link' }, 'See what it did'),
    ]);
  }

  function unavailable(status, msg) {
    if (gone) return;
    gone = true;
    exited = true;
    setStatus('off', 'Not available');
    setMode();
    frame.hidden = true;
    fontDown.hidden = true;
    fontUp.hidden = true;
    // The server's message is a fragment ("live terminal is not available yet").
    const said = msg && msg !== 'not supported here' ? msg[0].toUpperCase() + msg.slice(1) + (/[.!?]$/.test(msg) ? '' : '.') : '';
    const s = store.sessions.get(id);
    fill(banner);
    banner.hidden = true;
    add(root, h('div', { class: 'empty term-empty' },
      h('div', { class: 'halo' }, ico('terminal', 'lg')),
      h('h2', null, status === 404 ? 'Session not found' : 'The live terminal isn’t available'),
      h('p', null, status === 404 ? 'It may have ended more than a day ago.'
        : said ? said
          : s && s.mode !== 'wrapped' ? 'Only sessions started with pilothouse run have a terminal Pilothouse can show.'
            : 'Update Pilothouse on your computer to watch terminals from here.'),
      link('/s/' + id, { class: 'btn secondary' }, 'Back to the session')));
  }

  // ---- Boot ----
  async function start() {
    setStatus('connecting', 'Loading…');
    let x;
    try {
      x = await loadXterm();
    } catch {
      if (!destroyed) unavailable(0, 'Couldn’t load the terminal. Reload the app and try again.');
      return;
    }
    if (destroyed) return;
    term = new x.Terminal({
      fontFamily: getComputedStyle(document.body).getPropertyValue('--mono').trim() || 'ui-monospace, Menlo, monospace',
      fontSize,
      lineHeight: 1.15,
      cursorBlink: false,
      scrollback: 5000,
      convertEol: false,
      allowProposedApi: false,
      screenReaderMode: false,
      minimumContrastRatio: 4.5,
      theme: theme(),
    });
    fit = new x.FitAddon();
    term.loadAddon(fit);
    undoStyles = openWithAdoptedStyles(term, screen);
    term.onData(send);
    term.onBinary(sendBinary);
    scheme.addEventListener('change', onScheme);
    ro = new ResizeObserver(() => relayout());
    ro.observe(frame);
    relayout();
    screen.addEventListener('click', () => term && term.focus());

    for (const k of KEYS) {
      // Text keys are named by their label; icon keys by their name.
      const b = h('button', { class: 'term-key k-' + k.area, type: 'button', 'aria-label': k.icon ? k.name : null, title: k.label && k.label !== k.name ? k.name : null },
        k.icon ? ico(k.icon, 'sm') : k.label);
      // Keep focus (and the phone keyboard) on the terminal.
      b.addEventListener('mousedown', (e) => e.preventDefault());
      b.addEventListener('click', () => send(k.seq(term.modes.applicationCursorKeysMode)));
      add(keysEl, b);
    }

    connect();
    const s = store.sessions.get(id);
    if (s && s.caps && s.caps.watch === false && s.state !== 'exited') {
      // Not a wrapped session: the stream will say so; don't sign anything.
      return;
    }
    await ensureToken();
    if (token) { lastSize = ''; sendResize(); }
  }

  // Phones freeze background pages, which can leave the stream dead without
  // an error: reconnect at once when shown again after a while (or if it
  // was already retrying).
  let hiddenAt = 0;
  function onVisibility() {
    if (document.hidden) { hiddenAt = Date.now(); return; }
    if (destroyed || exited || !term) return;
    if (!ctrl || statusEl.classList.contains('reconnecting') || (hiddenAt && Date.now() - hiddenAt > 20000)) {
      backoff = 0;
      connect();
    }
  }
  document.addEventListener('visibilitychange', onVisibility);
  // The visual viewport shrinks when the phone keyboard opens.
  const vv = window.visualViewport;
  const onViewport = () => { root.style.setProperty('--vvh', Math.round(vv.height) + 'px'); };
  if (vv) { vv.addEventListener('resize', onViewport); onViewport(); }

  renderTitle();
  setMode();
  if (!store.sessions.has(id)) {
    api('GET', '/api/sessions/' + enc).then((s) => { store.sessions.set(s.id, s); if (!destroyed) renderTitle(); }, () => {});
  }
  start();

  return {
    el: root,
    update: renderTitle,
    destroy() {
      destroyed = true;
      clearTimeout(retryTimer);
      clearTimeout(flushTimer);
      clearTimeout(resizeTimer);
      if (ctrl) ctrl.abort();
      if (ro) ro.disconnect();
      scheme.removeEventListener('change', onScheme);
      document.removeEventListener('visibilitychange', onVisibility);
      if (vv) vv.removeEventListener('resize', onViewport);
      if (term) term.dispose();
      if (undoStyles) undoStyles();
    },
  };
}
