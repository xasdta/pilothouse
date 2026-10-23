// App chrome: top bar, tab bar, connection banner, toasts, navigation.

import { h, icon, appIcon, fill } from './dom.js';
import { store } from './api.js';

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
      go(path);
    },
  }, ...kids);
}

const el = {};

export function mountShell(root) {
  el.left = h('div', { class: 'brand' });
  el.presence = h('button', { class: 'chip', type: 'button', onclick: () => go('/settings') });
  el.conn = h('span', { class: 'conn', role: 'img' });
  el.topbar = h('header', { class: 'topbar' },
    h('div', { class: 'topbar-in' }, el.left, h('div', { class: 'spacer' }), el.presence, el.conn));
  el.banner = h('div', { class: 'banner', role: 'status', 'aria-live': 'polite' });
  el.main = h('main', { id: 'main', tabindex: '-1' });
  el.badge = h('span', { class: 'badge', hidden: true });
  el.tabs = {
    '/': link('/', { class: 'tab' }, icon('inbox'), h('span', null, 'Inbox'), el.badge),
    '/sessions': link('/sessions', { class: 'tab' }, icon('sessions'), h('span', null, 'Sessions')),
    '/settings': link('/settings', { class: 'tab' }, icon('settings'), h('span', null, 'Settings')),
  };
  el.tabbar = h('nav', { class: 'tabbar', 'aria-label': 'Main' }, h('div', { class: 'tabbar-in' }, ...Object.values(el.tabs)));
  el.toast = h('div', { class: 'toast', role: 'status', 'aria-live': 'assertive' });
  fill(root, el.topbar, el.banner, el.main, el.tabbar, el.toast);
  addEventListener('scroll', () => el.topbar.classList.toggle('scrolled', scrollY > 4), { passive: true });
  return el.main;
}

/**
 * Configures the chrome for a view.
 * opts.back: label for a back button (else the brand); opts.tab: active tab
 * path or null to hide the tab bar; opts.chrome=false hides top bar extras.
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
  const showTabs = opts.tab !== null && opts.tab !== undefined;
  el.tabbar.hidden = !showTabs;
  el.main.classList.toggle('bare', !showTabs);
  for (const [path, a] of Object.entries(el.tabs)) {
    if (path === opts.tab) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  }
  el.presence.hidden = opts.chrome === false;
  el.conn.hidden = opts.chrome === false;
  refreshChrome();
}

export function refreshChrome() {
  const p = store.presence;
  if (p) {
    const away = p.state === 'away';
    el.presence.className = 'chip ' + (away ? 'away' : 'here');
    fill(el.presence, h('span', { class: 'dot' }), away ? 'Away' : 'At desk');
    // The accessible name starts with the visible text (WCAG 2.5.3).
    el.presence.setAttribute('aria-label', away
      ? 'Away: requests come to this device. Change in settings.'
      : 'At desk: requests show in the terminal. Change in settings.');
  }
  el.conn.className = 'conn ' + (store.conn === 'live' ? '' : store.conn);
  el.conn.setAttribute('aria-label', store.conn === 'live' ? 'Live' : store.conn === 'offline' ? 'Offline' : 'Connecting');
  const showBanner = store.loaded && store.conn !== 'live';
  el.banner.classList.toggle('show', showBanner);
  fill(el.banner,
    icon(store.conn === 'offline' ? 'wifiOff' : 'hourglass', 'sm'),
    store.conn === 'offline' ? `Can’t reach ${store.machine || 'your computer'} — retrying` : 'Reconnecting…');
  let n = 0;
  for (const r of store.requests.values()) if (r.status === 'open') n++;
  el.badge.hidden = n === 0;
  el.badge.textContent = n > 99 ? '99+' : String(n);
  document.title = n ? `(${n}) Pilothouse` : 'Pilothouse';
  if ('setAppBadge' in navigator) {
    (n ? navigator.setAppBadge(n) : navigator.clearAppBadge()).catch(() => {});
  }
}

let toastTimer = 0;
/** toast('Saved', 'ok' | 'bad') */
export function toast(text, kind = '') {
  clearTimeout(toastTimer);
  el.toast.className = 'toast show ' + kind;
  fill(el.toast, kind ? icon(kind === 'ok' ? 'check' : 'alert', 'sm') : null, h('span', null, text));
  toastTimer = setTimeout(() => el.toast.classList.remove('show'), 3200);
}
