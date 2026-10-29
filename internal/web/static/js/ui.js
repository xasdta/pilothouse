// App chrome: the shell (phone: top bar + tab bar; desktop ≥ 1024 px:
// sidebar · current view · needs-you pane), connection banner, toasts,
// sheets, skeletons, empty states and navigation.
//
// Exported API (stable; workstream I builds on it):
//   go(path, replace?)           client-side navigation
//   link(path, attrs, ...kids)   <a> that navigates client-side
//   toast(text, kind?, opts?)    kind '' | 'ok' | 'bad'; opts {action: {label, run}, ms}
//   openSheet({title, sub?, body, label?, onClose?}) → {el, body, close()}
//                                bottom sheet on phones, centred dialog on desktop
//                                (native <dialog>: focus trap, Esc, inert page)
//   confirmSheet({title, text, confirm, danger?}) → Promise<boolean>
//   skeleton('card' | 'row', n?) loading placeholders
//   emptyState({icon?, title, text?, hint?, teach?: [[icon, text]…], actions?: [el]})
//   isWide()  onLayout(fn)       desktop layout (≥ 1024 px) and changes to it
//   setTheme(mode) / theme()     'system' | 'light' | 'dark'
//   openPalette()                the ⌘K palette (wired by palette.js)
//   chrome(opts) refreshChrome() mountShell(root) setNavigator(fn) panes()  (app.js)

import { h, icon, appIcon, fill, add } from './dom.js';
import { store } from './api.js';
import { passkeySupported, restoreKeyButton } from './passkey.js';

let navigateFn = () => {};
export function setNavigator(fn) { navigateFn = fn; }
/** Client-side navigation. */
export function go(path, replace = false) { navigateFn(path, replace); }

/** <a> that navigates client-side. */
export function link(path, attrs, ...kids) {
  return h('a', {
    ...attrs,
    href: path,
    onclick: (e) => {
      if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
      e.preventDefault();
      if (attrs && typeof attrs.onclick === 'function') attrs.onclick(e);
      go(path);
    },
  }, ...kids);
}

// ---------- Layout ----------

const wideMQ = matchMedia('(min-width: 1024px)');
export const isWide = () => wideMQ.matches;
const layoutFns = new Set();
wideMQ.addEventListener('change', () => { for (const fn of layoutFns) fn(isWide()); });
/** fn(wide) whenever the layout switches between phone and desktop. */
export function onLayout(fn) {
  layoutFns.add(fn);
  return () => layoutFns.delete(fn);
}

let paletteFn = () => {};
export function setPaletteOpener(fn) { paletteFn = fn; }
export function openPalette() { paletteFn(); }

export const isMac = () => /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);

// ---------- Theme ----------

export function theme() { return self.PilothouseTheme ? self.PilothouseTheme.get() : 'system'; }
export function setTheme(mode) { if (self.PilothouseTheme) self.PilothouseTheme.set(mode); }

// ---------- Shell ----------

const el = {};

const TABS = [
  ['/', 'inbox', 'Inbox'],
  ['/sessions', 'sessions', 'Sessions'],
  ['/tasks', 'tasks', 'Tasks'],
  ['/insights', 'chart', 'Insights'],
  ['/settings', 'more', 'More'],
];
const NAV = [
  ['/', 'inbox', 'Inbox'],
  ['/sessions', 'sessions', 'Sessions'],
  ['/tasks', 'tasks', 'Tasks'],
  ['/insights', 'chart', 'Insights'],
  ['/rules', 'rules', 'Rules'],
  ['/settings', 'settings', 'Settings'],
];

function presenceChip(cls) {
  return h('button', { class: 'chip presence ' + cls, type: 'button', onclick: () => go('/settings') });
}

export function mountShell(root) {
  // Phone/tablet top bar; on desktop it only appears for back navigation.
  el.left = h('div', { class: 'brand' });
  el.presence = presenceChip('');
  el.conn = h('span', { class: 'conn', role: 'img' });
  el.search = h('button', { class: 'icon-btn search-btn-top', type: 'button', 'aria-label': 'Search and commands', onclick: () => openPalette() }, icon('search'));
  el.topbar = h('header', { class: 'topbar' },
    h('div', { class: 'topbar-in' }, el.left, h('div', { class: 'spacer' }), el.search, el.presence, el.conn));
  el.banner = h('div', { class: 'banner', role: 'status', 'aria-live': 'polite' });
  el.main = h('main', { id: 'main', tabindex: '-1' });

  // Desktop sidebar.
  el.navBadge = h('span', { class: 'badge', hidden: true });
  el.nav = {};
  for (const [path, ic, label] of NAV) {
    el.nav[path] = link(path, { class: 'nav-item' }, icon(ic), h('span', null, label), path === '/' ? el.navBadge : null);
  }
  el.machine = h('span', { class: 'machine' });
  el.sidePresence = presenceChip('');
  el.sideConn = h('span', { class: 'conn', role: 'img' });
  el.sideSessions = h('div', { class: 'side-sessions' });
  el.sidebar = h('aside', { class: 'sidebar', 'aria-label': 'Navigation' },
    h('div', { class: 'side-brand' }, h('div', { class: 'brand' }, appIcon(), h('span', null, 'Pilothouse', el.machine))),
    h('button', { class: 'search-btn', type: 'button', onclick: () => openPalette() },
      icon('search', 'sm'), h('span', null, 'Search or jump to…'), h('kbd', null, isMac() ? '⌘K' : 'Ctrl K')),
    h('nav', { 'aria-label': 'Main' }, Object.values(el.nav)),
    h('div', { class: 'side-head', id: 'side-sessions-head' }, 'Sessions'),
    h('nav', { 'aria-labelledby': 'side-sessions-head' }, el.sideSessions),
    h('div', { class: 'side-foot' }, el.sidePresence, el.sideConn));

  // Desktop needs-you pane.
  el.queueCount = h('span', { class: 'count', hidden: true });
  el.queueBody = h('div', { class: 'stack' });
  el.queue = h('aside', { class: 'queue', 'aria-labelledby': 'queue-title' },
    h('div', { class: 'queue-head' }, h('h2', { id: 'queue-title' }, 'Needs you'), el.queueCount),
    el.queueBody);

  // Phone tab bar.
  el.badge = h('span', { class: 'badge', hidden: true });
  el.tabs = {};
  for (const [path, ic, label] of TABS) {
    el.tabs[path] = link(path, { class: 'tab' }, icon(ic), h('span', null, label), path === '/' ? el.badge : null);
  }
  el.tabbar = h('nav', { class: 'tabbar', 'aria-label': 'Tabs' }, h('div', { class: 'tabbar-in' }, Object.values(el.tabs)));
  el.toasts = h('div', { class: 'toasts' });

  el.center = h('div', { class: 'center' }, el.topbar, el.banner, el.main);
  el.shell = h('div', { class: 'shell' }, el.sidebar, el.center, el.queue);
  fill(root, el.shell, el.tabbar, el.toasts);
  addEventListener('scroll', () => el.topbar.classList.toggle('scrolled', scrollY > 4), { passive: true });
  return el.main;
}

/** Containers the desktop panes render into (panes.js). */
export function panes() {
  return { sessions: el.sideSessions, queue: el.queueBody, queueCount: el.queueCount, queueEl: el.queue };
}

/**
 * Configures the chrome for a view.
 * opts.back: label for a back button (else the brand); opts.backTo: where
 * it goes without history; opts.tab: active tab path, or null to hide the
 * tab bar; opts.route: the path shown (desktop nav highlight);
 * opts.backOnPhone: the back button is for phones only (desktop has the nav);
 * opts.chrome=false: a bare screen (pairing) without bars or panes.
 */
export function chrome(opts = {}) {
  if (opts.back) {
    el.left.className = '';
    fill(el.left, h('button', {
      class: 'back', type: 'button',
      onclick: () => (history.state && history.state.inApp ? history.back() : go(opts.backTo || '/', true)),
    }, icon('back', 'lg'), opts.back));
  } else {
    el.left.className = 'brand';
    fill(el.left, appIcon(), h('span', null, 'Pilothouse'));
  }
  el.topbar.classList.toggle('has-back', !!opts.back);
  el.topbar.classList.toggle('back-phone', !!opts.backOnPhone);
  const bare = opts.chrome === false;
  const showTabs = !bare && opts.tab !== null && opts.tab !== undefined;
  el.tabbar.hidden = !showTabs;
  el.main.classList.toggle('bare', !showTabs);
  el.toasts.classList.toggle('bare', !showTabs);
  el.shell.classList.toggle('bare-shell', bare);
  for (const [path, a] of Object.entries(el.tabs)) {
    if (path === opts.tab) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  }
  const route = opts.route || opts.tab || '';
  for (const [path, a] of Object.entries(el.nav)) {
    if (path === route) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  }
  el.presence.hidden = bare;
  el.conn.hidden = bare;
  el.search.hidden = bare;
  refreshChrome();
}

let lastCount = -1;
export function refreshChrome() {
  if (!el.shell) return;
  const p = store.presence;
  if (p) {
    const away = p.state === 'away';
    for (const chip of [el.presence, el.sidePresence]) {
      chip.className = 'chip presence ' + (away ? 'away' : 'here');
      fill(chip, h('span', { class: 'dot' }), away ? 'Away' : 'At desk');
      // The accessible name starts with the visible text (WCAG 2.5.3).
      chip.setAttribute('aria-label', away
        ? 'Away: requests come to this device. Change in settings.'
        : 'At desk: requests show in the terminal. Change in settings.');
    }
  }
  const connLabel = store.conn === 'live' ? 'Connected' : store.conn === 'offline' ? 'Offline' : 'Connecting';
  for (const c of [el.conn, el.sideConn]) {
    c.className = 'conn ' + (store.conn === 'live' ? '' : store.conn);
    c.setAttribute('aria-label', connLabel);
    c.title = connLabel;
  }
  el.machine.textContent = store.machine || '';
  // A missing signing key is more urgent than a reconnect: decisions cannot
  // be sent at all until this device is paired again.
  el.banner.classList.toggle('key', !!store.keyMissing);
  if (store.keyMissing) {
    el.banner.classList.add('show');
    // A passkey can vouch for a new key right here; otherwise pair again.
    const canRestore = !!(store.me && store.me.device.passkey) && passkeySupported();
    if (el.banner.dataset.key !== String(canRestore)) {
      el.banner.dataset.key = String(canRestore);
      fill(el.banner,
        icon('key', 'sm'),
        canRestore
          ? h('span', null, h('b', null, 'Signing key missing'), ' — restore it with your passkey.')
          : h('span', null, h('b', null, 'Pair this device again'), ' — its signing key is missing.'),
        canRestore ? restoreKeyButton('btn small') : link('/pair', { class: 'btn small' }, 'Pair'));
    }
  } else {
    delete el.banner.dataset.key;
    const showBanner = store.loaded && store.conn !== 'live';
    el.banner.classList.toggle('show', showBanner);
    fill(el.banner,
      icon(store.conn === 'offline' ? 'wifiOff' : 'hourglass', 'sm'),
      store.conn === 'offline' ? `Can’t reach ${store.machine || 'your computer'} — retrying` : 'Reconnecting…');
  }
  let n = 0;
  for (const r of store.requests.values()) if (r.status === 'open') n++;
  for (const b of [el.badge, el.navBadge]) {
    b.hidden = n === 0;
    b.textContent = n > 99 ? '99+' : String(n);
    if (n > lastCount && lastCount >= 0) { b.classList.remove('pop'); void b.offsetWidth; b.classList.add('pop'); }
  }
  el.badge.setAttribute('aria-label', `${n} waiting`);
  el.navBadge.setAttribute('aria-label', `${n} waiting`);
  lastCount = n;
  document.title = n ? `(${n}) Pilothouse` : 'Pilothouse';
  if ('setAppBadge' in navigator) {
    (n ? navigator.setAppBadge(n) : navigator.clearAppBadge()).catch(() => {});
  }
}

// ---------- Toasts ----------

/** toast('Saved', 'ok' | 'bad', {action: {label, run}, ms}) */
export function toast(text, kind = '', opts = {}) {
  if (!el.toasts) return;
  const t = h('div', { class: 'toast ' + kind, role: kind === 'bad' ? 'alert' : 'status' },
    kind ? icon(kind === 'ok' ? 'check' : 'alert', 'sm') : null,
    h('span', null, text));
  let timer = 0;
  const close = () => {
    clearTimeout(timer);
    t.classList.add('out');
    setTimeout(() => t.remove(), 220);
  };
  if (opts.action) {
    add(t, h('button', { type: 'button', onclick: () => { close(); opts.action.run(); } }, opts.action.label));
  }
  add(el.toasts, t);
  while (el.toasts.children.length > 3) el.toasts.firstElementChild.remove();
  timer = setTimeout(close, opts.ms || (opts.action ? 6000 : 3200));
  return close;
}

// ---------- Sheets ----------

/**
 * Opens a modal sheet. body is a node (or array). Returns {el, body,
 * close}. The sheet is removed from the DOM when it closes.
 */
export function openSheet({ title, sub, body, label, onClose, cls = '' }) {
  const titleId = 'sheet-' + Math.random().toString(36).slice(2, 8);
  const content = h('div', { class: 'sheet-body' }, sub ? h('p', { class: 'sheet-sub' }, sub) : null, body);
  const dlg = h('dialog', { class: 'sheet ' + cls, 'aria-labelledby': titleId },
    h('div', { class: 'sheet-grab', 'aria-hidden': 'true' }),
    h('div', { class: 'sheet-head' },
      h('h2', { id: titleId }, title),
      h('button', { class: 'icon-btn', type: 'button', 'aria-label': label || 'Close', onclick: () => dlg.close() }, icon('x'))),
    content);
  dlg.addEventListener('click', (e) => { if (e.target === dlg) dlg.close(); }); // backdrop
  dlg.addEventListener('close', () => {
    dlg.remove();
    if (onClose) onClose();
  });
  add(document.body, dlg);
  dlg.showModal();
  return { el: dlg, body: content, close: () => dlg.close() };
}

/** A yes/no sheet; resolves true when confirmed. */
export function confirmSheet({ title, text, confirm = 'OK', danger = false }) {
  return new Promise((resolve) => {
    let ok = false;
    const yes = h('button', { class: 'btn block ' + (danger ? 'danger' : 'primary'), type: 'button' }, confirm);
    const no = h('button', { class: 'btn block ghost', type: 'button' }, 'Cancel');
    const s = openSheet({ title, body: [text ? h('p', { class: 'sheet-sub' }, text) : null, h('div', { class: 'stack' }, yes, no)], onClose: () => resolve(ok) });
    yes.addEventListener('click', () => { ok = true; s.close(); });
    no.addEventListener('click', () => s.close());
    yes.focus();
  });
}

// ---------- Placeholders ----------

/** Loading placeholders shaped like request cards or list rows. */
export function skeleton(kind = 'card', n = 2) {
  const one = () => (kind === 'row'
    ? h('div', { class: 'skel-row' }, h('div', { class: 'skel av' }),
      h('div', { class: 'lines' }, h('div', { class: 'skel skel-line w40' }), h('div', { class: 'skel skel-line w80' })))
    : h('div', { class: 'card skel-card' },
      h('div', { class: 'skel skel-line w40' }), h('div', { class: 'skel skel-line w80' }),
      h('div', { class: 'skel skel-block' }), h('div', { class: 'skel-btns' }, h('div', { class: 'skel' }), h('div', { class: 'skel' }))));
  const items = Array.from({ length: n }, one);
  return h('div', { class: kind === 'row' ? 'list' : 'stack', 'aria-busy': 'true', 'aria-label': 'Loading' }, items);
}

/** An empty state that teaches what will show up here and how. */
export function emptyState({ icon: ic, mark, title, text, hint, teach, actions, cls = '' }) {
  return h('div', { class: 'empty ' + cls },
    h('div', { class: 'halo' }, mark ? h('img', { class: 'mark', src: '/icons/icon.svg', alt: '' }) : icon(ic || 'inbox')),
    h('h2', null, title),
    text ? h('p', null, text) : null,
    teach && teach.length ? h('ul', { class: 'teach' }, teach.map(([i, t]) => h('li', null, icon(i, 'sm'), h('span', null, t)))) : null,
    actions && actions.length ? h('div', { class: 'actions-row' }, actions) : null,
    hint ? h('p', { class: 'hint' }, hint) : null);
}
