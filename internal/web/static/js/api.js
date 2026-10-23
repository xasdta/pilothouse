// Device API client and the live state store (snapshot + SSE).

export class ApiError extends Error {
  constructor(status, code, message) {
    super(message || code || 'Request failed');
    this.status = status;
    this.code = code;
  }
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
  presence: null,
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
let backoff = 0;
let hiddenAt = 0;
let lastHeard = 0;

// A connection can die silently (Wi-Fi handoff, sleeping proxy) without an
// error event. The server sends `event: ping` every 15 s; after 45 s with
// nothing at all, reconnect.
const SILENCE_MS = 45000;
const heard = () => { lastHeard = Date.now(); };

function setConn(c) {
  if (store.conn !== c) {
    store.conn = c;
    changed('conn');
  }
}

export async function resync() {
  const snap = await api('GET', '/api/snapshot');
  loadSnapshot(snap);
}

export function connect() {
  clearTimeout(retryTimer);
  if (es) { es.close(); es = null; }
  setConn(store.conn === 'live' ? 'live' : 'connecting');
  const src = new EventSource('/api/events?after=' + store.seq);
  es = src;
  heard();
  src.addEventListener('open', () => {
    heard();
    backoff = 0;
    setConn('live');
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
  setConn(backoff >= 2 ? 'offline' : 'connecting');
  // A 401 means this device was revoked or signed out elsewhere.
  if (!err || err.status !== 401) {
    try { await api('GET', '/api/me'); } catch (e) { err = e; }
  }
  if (err && err.status === 401) { location.assign('/pair'); return; }
  const delay = Math.min(30000, 1000 * 2 ** backoff) * (0.75 + Math.random() * 0.5);
  backoff = Math.min(backoff + 1, 6);
  retryTimer = setTimeout(connect, delay);
}

// Phones freeze background pages; reconnect at once when shown again.
document.addEventListener('visibilitychange', () => {
  if (document.hidden) { hiddenAt = Date.now(); return; }
  if (!store.loaded) return;
  if (!es || store.conn !== 'live' || Date.now() - hiddenAt > 20000) {
    backoff = 0;
    connect();
  }
});
addEventListener('online', () => { if (store.loaded) { backoff = 0; connect(); } });

setInterval(() => {
  if (!es || !store.loaded || Date.now() - lastHeard < SILENCE_MS) return;
  console.warn('event stream silent for 45 s; reconnecting');
  es.close();
  es = null;
  setConn('connecting');
  backoff = 0;
  connect();
}, 5000);

// Back/forward cache: a page kept in the bfcache would hold its stream open
// (browsers allow only 6 connections per host on HTTP/1.1, so a few
// navigations would hang the app). Close it on the way out and reopen it,
// catching up from store.seq, when the page is restored.
addEventListener('pagehide', () => {
  clearTimeout(retryTimer);
  if (es) { es.close(); es = null; }
});
addEventListener('pageshow', (e) => {
  if (e.persisted && store.loaded) { backoff = 0; connect(); }
});
