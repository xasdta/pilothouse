// Pilothouse web app entry: boot, routing, live updates.

import { h, fill } from './dom.js';
import { api, store, subscribe, resync, connect } from './api.js';
import { mountShell, chrome, refreshChrome, setNavigator, toast } from './ui.js';
import { syncPush } from './push.js';
import { inboxView } from './views/inbox.js';
import { sessionsView } from './views/sessions.js';
import { sessionView } from './views/session.js';
import { requestView } from './views/request.js';
import { terminalView } from './views/terminal.js';
import { tasksView, taskView, newTaskView } from './views/tasks.js';
import { insightsView } from './views/insights.js';
import { rulesView } from './views/rules.js';
import { settingsView } from './views/settings.js';
import { pairView } from './views/pair.js';

const root = document.getElementById('app');
let main = null;
let view = null;

const routes = [
  [/^\/$/, () => inboxView(), { tab: '/' }],
  [/^\/sessions$/, () => sessionsView(), { tab: '/sessions' }],
  [/^\/settings$/, () => settingsView(), { tab: '/settings' }],
  [/^\/s\/([\w-]+)$/, (m) => sessionView(m[1]), { back: 'Back', tab: null }],
  [/^\/r\/([\w-]+)$/, (m) => requestView(m[1]), { back: 'Back', tab: null }],
  [/^\/s\/([\w-]+)\/terminal$/, (m) => terminalView(m[1]), { back: 'Back', tab: null }],
  [/^\/tasks$/, () => tasksView(), { tab: '/tasks' }],
  [/^\/new$/, () => newTaskView(), { back: 'Back', tab: null }],
  [/^\/t\/([\w-]+)$/, (m) => taskView(m[1]), { back: 'Back', tab: null }],
  [/^\/insights$/, () => insightsView(), { tab: '/insights' }],
  [/^\/rules$/, () => rulesView(), { tab: '/rules' }],
];

function render(path) {
  if (view && view.destroy) view.destroy();
  let match = null;
  let entry = routes[0];
  for (const r of routes) {
    match = path.match(r[0]);
    if (match) { entry = r; break; }
  }
  if (!match) { history.replaceState(null, '', '/'); match = ['/']; }
  chrome(entry[2]);
  view = entry[1](match);
  fill(main, view.el);
  scrollTo(0, 0);
  main.focus({ preventScroll: true });
}

function navigate(path, replace = false) {
  if (path === location.pathname && !replace) return;
  if (replace) history.replaceState({ inApp: true }, '', path);
  else history.pushState({ inApp: true }, '', path);
  render(path);
}

function registerSW() {
  if (!('serviceWorker' in navigator) || !window.isSecureContext) return;
  navigator.serviceWorker.register('/sw.js', { scope: '/' }).catch((e) => console.warn('service worker:', e));
  navigator.serviceWorker.addEventListener('message', (e) => {
    if (e.data && e.data.type === 'navigate' && typeof e.data.url === 'string' && e.data.url.startsWith('/')) navigate(e.data.url);
    if (e.data && e.data.type === 'decided') toast(e.data.text, e.data.ok ? 'ok' : 'bad');
  });
}

function showPair(code, mode) {
  main = main || mountShell(root);
  chrome({ tab: null, chrome: false });
  const v = pairView({
    code, mode,
    onDone: (me, wantsNotifications) => startApp(me, wantsNotifications ? '/settings' : '/'),
  });
  fill(main, v.el);
  if (v.focus) v.focus();
}

async function startApp(me, path) {
  store.me = me;
  main = main || mountShell(root);
  setNavigator(navigate);
  subscribe((kind) => {
    refreshChrome();
    if (view && view.update) view.update(kind);
  });
  history.replaceState({ inApp: false }, '', path);
  render(path);
  try {
    await resync();
  } catch (e) {
    toast(e.message, 'bad');
  }
  connect();
  syncPush().catch(() => {});
}

async function boot() {
  registerSW();
  addEventListener('popstate', () => { if (store.me) render(location.pathname); });
  // A pairing link opened in a tab that already shows /pair changes only the
  // fragment (no reload): pick the code up anyway.
  addEventListener('hashchange', () => {
    if (location.hash.length > 1 && (location.pathname === '/pair' || location.pathname === '/login')) location.reload();
  });
  const path = location.pathname;
  const code = decodeURIComponent(location.hash.slice(1)).trim();
  if (code) history.replaceState(null, '', path); // never keep the code in history

  if ((path === '/pair' || path === '/login') && code) {
    showPair(code, path === '/login' ? 'login' : 'pair');
    return;
  }
  let me;
  try {
    me = await api('GET', '/api/me');
  } catch (e) {
    if (e.status === 401) {
      history.replaceState(null, '', '/pair');
      showPair('', 'pair');
      return;
    }
    main = main || mountShell(root);
    chrome({ tab: null, chrome: false });
    fill(main, errorScreen(e));
    return;
  }
  startApp(me, path === '/pair' || path === '/login' ? '/' : path);
}

function errorScreen(e) {
  // Network failures already say "Can’t reach your computer"; don't repeat
  // the heading in the body.
  const body = e && e.message && e.code !== 'network'
    ? e.message
    : 'The Pilothouse daemon isn’t answering. Check that it’s running on your computer (pilothouse start), then try again.';
  return h('div', { class: 'view pair' },
    h('div', { class: 'empty' },
      h('h2', null, 'Can’t reach your computer'),
      h('p', null, body)),
    h('button', { class: 'btn primary block', type: 'button', onclick: () => location.reload() }, 'Try again'));
}

boot();
