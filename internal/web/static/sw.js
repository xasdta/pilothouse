/*
 * Pilothouse service worker: offline shell, Web Push notifications,
 * Allow / Deny actions on permission requests (signed with this device's
 * key via PilothouseKeys.respond), the app badge, and resolved requests:
 * when a request is answered anywhere, its notification is replaced by a
 * short "Answered ✓" one with the same tag — never dropped silently (iOS
 * revokes subscriptions whose pushes show nothing).
 *
 * Push payloads (internal/web/push.go): {kind, title, body, tag, url,
 * request_id, session_id, actions, expires_at} plus, from 0.2,
 * request: {id, checkpoint, summary} and, for withdrawals,
 * type: "resolved" with request_id, status, by. Fields may also arrive
 * nested under `data`.
 */
'use strict';

importScripts('/js/keystore.js');

const CACHE = 'pilothouse-shell-v2';
// Every file the app needs offline. internal/web's tests check that this
// list matches the embedded js/ and css/ files.
const SHELL = [
  '/',
  '/css/app.css',
  '/css/views.css',
  '/js/theme.js',
  '/js/keystore.js',
  '/js/app.js',
  '/js/api.js',
  '/js/card.js',
  '/js/diff.js',
  '/js/dom.js',
  '/js/markdown.js',
  '/js/palette.js',
  '/js/panes.js',
  '/js/push.js',
  '/js/ui.js',
  '/js/views/inbox.js',
  '/js/views/insights.js',
  '/js/views/pair.js',
  '/js/views/request.js',
  '/js/views/rows.js',
  '/js/views/rules.js',
  '/js/views/session.js',
  '/js/views/sessions.js',
  '/js/views/settings.js',
  '/js/views/tasks.js',
  '/js/views/terminal.js',
  '/vendor/xterm/xterm.mjs',
  '/vendor/xterm/addon-fit.mjs',
  '/vendor/xterm/xterm.css',
  '/manifest.webmanifest',
  '/icons/icon.svg',
  '/icons/icon-192.png',
  '/icons/badge-96.png',
];

self.addEventListener('install', (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim()),
  );
});

// Network first (the daemon is on loopback or the tailnet, so it's fast and
// always the freshest), falling back to the cached shell when offline. The
// API is never cached.
self.addEventListener('fetch', (e) => {
  const req = e.request;
  const url = new URL(req.url);
  if (req.method !== 'GET' || url.origin !== self.location.origin || url.pathname.startsWith('/api/')) return;
  e.respondWith((async () => {
    try {
      const res = await fetch(req);
      if (res.ok && SHELL.includes(url.pathname)) {
        const copy = res.clone();
        caches.open(CACHE).then((c) => c.put(req, copy)).catch(() => {});
      }
      return res;
    } catch (err) {
      const cache = await caches.open(CACHE);
      const hit = await cache.match(req, { ignoreSearch: true });
      if (hit) return hit;
      if (req.mode === 'navigate') {
        const shell = await cache.match('/');
        if (shell) return shell;
      }
      throw err;
    }
  })());
});

// ---------- Push ----------

const ICON = '/icons/icon-192.png';
const BADGE = '/icons/badge-96.png';

/** Flattens a payload: fields may sit at the top level or under `data`. */
function normalize(p) {
  const d = p && typeof p.data === 'object' && p.data ? { ...p, ...p.data } : { ...(p || {}) };
  const req = d.request && typeof d.request === 'object' ? d.request : null;
  d.request_id = d.request_id || (req && req.id) || '';
  d.checkpoint = d.checkpoint || (req && req.checkpoint) || '';
  d.summary = d.summary || (req && req.summary) || '';
  d.resolved = d.type === 'resolved' || d.kind === 'resolved';
  return d;
}

/** Notifications showing this request, whatever tag the server used. */
async function notificationsFor(id, tag) {
  const all = await self.registration.getNotifications();
  return all.filter((n) => (tag && n.tag === tag) || (id && n.data && n.data.request_id === id));
}

/** "Answered ✓ …" for payloads without server-written text. */
function answeredText(status, by) {
  if (status === 'expired') return 'Timed out — handed back to the terminal';
  if (status === 'answered_locally') return 'Answered ✓ in the terminal';
  if (status === 'stepped_aside') return 'Answered ✓ — waiting in the terminal';
  const who = !by ? ''
    : by === 'cli' || by === 'tui' ? ' at the computer'
      : by.startsWith('telegram') ? ' in Telegram'
        : by === 'ntfy' ? ' from ntfy'
          : by.startsWith('policy:') ? ' by a rule'
            : by.startsWith('device:') ? ' on a paired device'
              : by.includes(' ') ? ' ' + by : ''; // already words: "from iPhone"
  return 'Answered ✓' + who;
}

/** The app badge: how many request notifications are still waiting. */
async function updateBadge(count) {
  const nav = self.navigator;
  if (!nav || !('setAppBadge' in nav)) return;
  let n = count;
  if (typeof n !== 'number') {
    const all = await self.registration.getNotifications();
    n = all.filter((x) => x.data && x.data.request_id && !x.data.resolved).length;
  }
  try {
    if (n > 0) await nav.setAppBadge(n);
    else await nav.clearAppBadge();
  } catch { /* not allowed here */ }
}

async function showResolved(d) {
  const id = d.request_id;
  const prev = await notificationsFor(id, d.tag);
  const tag = d.tag || (prev[0] && prev[0].tag) || (id ? 'req-' + id : 'pilothouse');
  const title = (prev[0] && prev[0].title) || d.title || 'Pilothouse';
  // Close copies shown under another tag (0.1 servers tagged with the bare id).
  for (const n of prev) if (n.tag !== tag) n.close();
  // The daemon writes the text ("✓ Allowed", "… — from iPhone"); older
  // payloads only say what happened.
  await self.registration.showNotification(d.title || title, {
    body: d.title && d.body ? d.body : answeredText(d.status, d.by || ''),
    tag,
    renotify: false,
    icon: ICON,
    badge: BADGE,
    timestamp: Date.now(),
    data: { url: d.url || (id ? '/r/' + id : '/'), request_id: id, resolved: true },
  });
  await updateBadge(typeof d.open_count === 'number' ? d.open_count : undefined);
}

async function showRequest(d) {
  const id = d.request_id;
  const isRequest = !!id && (d.kind === 'request' || !!d.checkpoint || d.actions);
  // Buttons only for permission requests: questions and plans need the app.
  const permission = d.checkpoint ? d.checkpoint === 'permission' : !!d.actions;
  const opts = {
    body: d.body || d.summary || '',
    tag: d.tag || (id ? 'req-' + id : 'pilothouse'),
    renotify: true,
    icon: ICON,
    badge: BADGE,
    timestamp: Date.now(),
    requireInteraction: isRequest,
    data: { url: d.url || (id ? '/r/' + id : '/'), request_id: id, checkpoint: d.checkpoint || (permission ? 'permission' : ''), expires_at: d.expires_at || 0 },
  };
  if (id && permission && d.actions !== false) {
    opts.actions = [
      { action: 'allow', title: 'Allow' },
      { action: 'deny', title: 'Deny' },
    ];
  }
  await self.registration.showNotification(d.title || 'Pilothouse', opts);
  if (isRequest) await updateBadge(typeof d.open_count === 'number' ? d.open_count : undefined);
}

self.addEventListener('push', (e) => {
  let p = {};
  try { p = e.data ? e.data.json() : {}; } catch { p = { title: 'Pilothouse', body: e.data && e.data.text() }; }
  const d = normalize(p);
  e.waitUntil(d.resolved ? showResolved(d) : showRequest(d));
});

async function focusOrOpen(url) {
  const wins = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
  for (const w of wins) {
    if (new URL(w.url).origin !== self.location.origin) continue;
    await w.focus();
    w.postMessage({ type: 'navigate', url });
    return;
  }
  await self.clients.openWindow(url);
}

async function tell(text, ok) {
  const wins = await self.clients.matchAll({ type: 'window' });
  for (const w of wins) w.postMessage({ type: 'decided', text, ok });
}

self.addEventListener('notificationclick', (e) => {
  const n = e.notification;
  const data = n.data || {};
  n.close();
  if ((e.action === 'allow' || e.action === 'deny') && data.request_id) {
    e.waitUntil((async () => {
      const body = e.action === 'allow' ? { behavior: 'allow', scope: 'once' } : { behavior: 'deny', message: 'Denied from a notification.' };
      const res = await self.PilothouseKeys.respond(data.request_id, body);
      if (res.ok) {
        await tell(e.action === 'allow' ? 'Allowed' : 'Denied', true);
        await updateBadge();
        return;
      }
      if (res.status === 409) {
        await self.registration.showNotification('Already answered', {
          body: 'Someone answered this request on another device.',
          tag: n.tag, renotify: false, icon: ICON, badge: BADGE,
          data: { url: data.url || '/', request_id: data.request_id, resolved: true },
        });
        await updateBadge();
        return;
      }
      // Anything else (signed out, clock skew, offline): open the request.
      await focusOrOpen(data.url || '/');
    })());
    return;
  }
  e.waitUntil(Promise.all([focusOrOpen(data.url || '/'), updateBadge()]));
});

self.addEventListener('notificationclose', (e) => {
  e.waitUntil(updateBadge());
});

// The push service rotated the subscription: re-subscribe and tell the daemon.
self.addEventListener('pushsubscriptionchange', (e) => {
  const old = e.oldSubscription;
  const key = old && old.options && old.options.applicationServerKey;
  if (!key) return;
  e.waitUntil(self.registration.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key })
    .then((sub) => fetch('/api/push/subscribe', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-Pilothouse': '1' },
      body: JSON.stringify(sub.toJSON()),
    }))
    .catch(() => {}));
});
