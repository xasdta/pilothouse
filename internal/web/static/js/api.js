// Device API client and the live state store (snapshot + SSE).
//
// Exported API (stable; workstream I builds on it):
//   api(method, path, body?)      JSON fetch; throws ApiError {status, code, message}
//   signed(path, action, target, payload)  signed v2 action (keystore.js); resolves to
//                                 the response JSON (null on 204), throws ApiError
//   store                         live state: me, seq, machine, sessions (Map), requests
//                                 (Map), tasks (Map), presence, conn, loaded, rulesRev
//                                 (bumped on policy.changed)
//   subscribe(fn)                 fn(kind) once per frame after changes; returns unsubscribe
//   onEvent(fn)                   every raw event as it arrives; returns unsubscribe
//   changed(kind)                 schedule a notification (after local store edits)
//   resync() connect()            used by app.js

export class ApiError extends Error {
  constructor(status, code, message) {
    super(message || code || 'Request failed');
    this.status = status;
    this.code = code;
  }
}

/**
 * Signs and posts a 0.2 action (see internal/web/signed.go). Same errors
 * as api(); a device without a key gets code 'no_key'.
 */
export async function signed(path, action, target, payload) {
  const res = await self.PilothouseKeys.postSigned(path, action, target, payload);
  if (res.ok) return res.data;
  const d = res.data || {};
  if (res.status === 401) location.assign('/pair');
  throw new ApiError(res.status, d.code, d.code === 'stale_signature'
    ? 'This device’s clock is off — fix the time and try again.'
    : d.error || (res.status === 0 ? 'Can’t reach your computer.' : 'Request failed'));
}

export async function api(method, path, body) {
  const opts = { method, credentials: 'same-origin', headers: {} };
  if (method !== 'GET' && method !== 'HEAD') opts.headers['X-Pilothouse'] = '1';
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(path, opts);
  } catch {
    throw new ApiError(0, 'network', 'Can’t reach your computer.');
  }
  if (res.status === 204) return null;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new ApiError(res.status, data.code, data.error);
  return data;
}

// ---------- Store ----------

const listeners = new Set();

export const store = {
  me: null,
  seq: 0,
  machine: '',
  sessions: new Map(),
  requests: new Map(),
  tasks: new Map(),
  presence: null,
  rulesRev: 0,
  conn: 'connecting', // live | connecting | offline
  loaded: false,
};

export function subscribe(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

let queued = false;
function flush(kind) {
  if (!queued) return;
  queued = false;
  for (const fn of listeners) fn(kind);
}
/**
 * Coalesces change notifications into one per frame. Hidden tabs get no
 * animation frames, so there the flush runs as a microtask instead (the
 * title badge must keep counting while the tab is in the background). The
 * timeout covers a tab hidden after the frame was requested.
 */
export function changed(kind = 'data') {
  if (queued) return;
  queued = true;
  if (document.hidden) {
    queueMicrotask(() => flush(kind));
    return;
  }
  requestAnimationFrame(() => flush(kind));
  setTimeout(() => flush(kind), 250);
}

export function loadSnapshot(snap) {
  store.seq = snap.seq;
  store.machine = snap.machine;
  store.presence = snap.presence;
  store.sessions = new Map(snap.sessions.map((s) => [s.id, s]));
  store.requests = new Map(snap.requests.map((r) => [r.id, r]));
  store.tasks = new Map((snap.tasks || []).map((t) => [t.id, t]));
  store.loaded = true;
  changed();
}

function applyEvent(ev) {
  if (ev.seq <= store.seq) return;
  store.seq = ev.seq;
  const d = ev.data;
  switch (ev.type) {
    case 'session.started':
    case 'session.updated':
    case 'session.exited':
      if (d && d.id) store.sessions.set(d.id, d);
      break;
    case 'request.opened':
    case 'request.closed':
      if (d && d.id) store.requests.set(d.id, d);
      break;
    case 'presence.changed':
      if (d) store.presence = d;
      break;
    case 'task.updated':
      if (d && d.id) store.tasks.set(d.id, d);
      break;
    case 'policy.changed':
      store.rulesRev++;
      break;
    case 'device.changed':
      if (d && store.me && d.id === store.me.device.id) {
        if (d.revoked_at) { location.assign('/pair'); return; }
        store.me.device = d;
      }
      break;
  }
  for (const fn of eventHooks) fn(ev);
  changed();
}

const eventHooks = new Set();
/** Raw event listener (session detail refreshes its timeline). */
export function onEvent(fn) {
  eventHooks.add(fn);
  return () => eventHooks.delete(fn);
}

// ---------- Live connection ----------

let es = null;
let retryTimer = 0;
let degradeTimer = 0;
let backoff = 0;
let hiddenAt = 0;
let lastHeard = 0;
let everLive = false;
let failingSince = 0;

// A connection can die silently (Wi-Fi handoff, sleeping proxy) without an
// error event. The server sends `event: ping` every 15 s; after 30 s with
// nothing at all, reconnect.
const SILENCE_MS = 30000;
// Phones suspend the page and its stream. Keep the last healthy state
// through a short blip or a background pause; only surface a reconnect after
// this long while the page is visible.
const DEGRADE_MS = 8000;
const heard = () => { lastHeard = Date.now(); };

function setConn(c) {
  if (store.conn !== c) {
    store.conn = c;
    changed('conn');
  }
}

function live() {
  everLive = true;
  failingSince = 0;
  clearTimeout(degradeTimer);
  degradeTimer = 0;
  setConn('live');
}

function degrade(c) {
  // Before the first successful connection, show the honest state at once.
  if (!everLive) { setConn(c); return; }
  if (!failingSince) failingSince = Date.now();
  if (document.hidden) return;
  clearTimeout(degradeTimer);
  const wait = Math.max(0, DEGRADE_MS - (Date.now() - failingSince));
  degradeTimer = setTimeout(() => {
    degradeTimer = 0;
    if (failingSince && !document.hidden) setConn(c);
  }, wait);
}

export async function resync() {
  const snap = await api('GET', '/api/snapshot');
  loadSnapshot(snap);
}

export function connect() {
  clearTimeout(retryTimer);
  if (es) { es.close(); es = null; }
  // Once we have been live, reconnects are silent: the badge and Settings
  // keep saying Connected until a failure has lasted past DEGRADE_MS.
  if (!everLive) setConn('connecting');
  const src = new EventSource('/api/events?after=' + store.seq);
  es = src;
  heard();
  src.addEventListener('open', () => {
    heard();
    backoff = 0;
    live();
  });
  src.addEventListener('ping', heard);
  src.addEventListener('event', (m) => {
    heard();
    try { applyEvent(JSON.parse(m.data)); } catch (e) { console.warn('bad event', e); }
  });
  src.addEventListener('control', async (m) => {
    heard();
    let c = {};
    try { c = JSON.parse(m.data); } catch { /* ignore */ }
    if (!c.resync_required) return;
    src.close();
    if (es === src) es = null;
    try {
      await resync();
      connect();
    } catch (e) {
      fail(e);
    }
  });
  src.addEventListener('error', () => {
    if (es !== src) return;
    src.close();
    es = null;
    fail();
  });
}

async function fail(err) {
  degrade(backoff >= 2 ? 'offline' : 'connecting');
  // A 401 means this device was revoked or signed out elsewhere.
  if (!err || err.status !== 401) {
    try { await api('GET', '/api/me'); } catch (e) { err = e; }
  }
  if (err && err.status === 401) { location.assign('/pair'); return; }
  const delay = Math.min(30000, 1000 * 2 ** backoff) * (0.75 + Math.random() * 0.5);
  backoff = Math.min(backoff + 1, 6);
  retryTimer = setTimeout(connect, delay);
}

// Phones freeze background pages. On return, reconnect at once: an open
// EventSource can look alive while its TCP connection is long gone.
document.addEventListener('visibilitychange', () => {
  if (document.hidden) { hiddenAt = Date.now(); return; }
  if (!store.loaded) return;
  const stale = Date.now() - lastHeard > 20000;
  if (!es || store.conn !== 'live' || stale || Date.now() - hiddenAt > 3000) {
    backoff = 0;
    failingSince = 0;
    connect();
  }
});
addEventListener('online', () => {
  if (!store.loaded) return;
  backoff = 0;
  failingSince = 0;
  connect();
});

setInterval(() => {
  if (document.hidden) return;
  if (!es || !store.loaded || Date.now() - lastHeard < SILENCE_MS) return;
  console.warn(`event stream silent for ${SILENCE_MS / 1000} s; reconnecting`);
  es.close();
  es = null;
  backoff = 0;
  connect();
}, 5000);

// Back/forward cache: a page kept in the bfcache would hold its stream open
// (browsers allow only 6 connections per host on HTTP/1.1, so a few
// navigations would hang the app). Close it on the way out and reopen it,
// catching up from store.seq, when the page is restored.
addEventListener('pagehide', () => {
  clearTimeout(retryTimer);
  clearTimeout(degradeTimer);
  degradeTimer = 0;
  if (es) { es.close(); es = null; }
});
addEventListener('pageshow', (e) => {
  if (e.persisted && store.loaded) { backoff = 0; failingSince = 0; connect(); }
});
