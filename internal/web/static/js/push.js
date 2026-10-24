// Web Push: capability detection (with the iOS Home Screen rule),
// subscribe / unsubscribe, keeping the daemon's copy in sync, and closing
// a request's notification when this device answers it.

import { api, store } from './api.js';

export const isIOS = () =>
  /iPhone|iPad|iPod/.test(navigator.userAgent) || (/Macintosh/.test(navigator.userAgent) && navigator.maxTouchPoints > 1);

export const isStandalone = () =>
  matchMedia('(display-mode: standalone)').matches || navigator.standalone === true;

/**
 * 'ios-install' | 'insecure' | 'unsupported' | 'denied' | 'on' | 'off'
 */
export async function pushStatus() {
  if (!window.isSecureContext) return 'insecure';
  const supported = 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window;
  if (isIOS() && !isStandalone()) return 'ios-install';
  if (!supported) return 'unsupported';
  if (Notification.permission === 'denied') return 'denied';
  const sub = await currentSubscription();
  return sub && Notification.permission === 'granted' ? 'on' : 'off';
}

async function registration() {
  return navigator.serviceWorker.ready;
}

async function currentSubscription() {
  try {
    const reg = await Promise.race([registration(), new Promise((r) => setTimeout(() => r(null), 3000))]);
    return reg ? await reg.pushManager.getSubscription() : null;
  } catch {
    return null;
  }
}

function keyBytes(b64) {
  const s = atob(b64.replace(/-/g, '+').replace(/_/g, '/') + '='.repeat((4 - (b64.length % 4)) % 4));
  return Uint8Array.from(s, (c) => c.charCodeAt(0));
}

function sameKey(sub, key) {
  const k = sub.options && sub.options.applicationServerKey;
  if (!k) return true;
  const a = new Uint8Array(k);
  if (a.length !== key.length) return false;
  return a.every((v, i) => v === key[i]);
}

/** Asks for permission (must run from a tap) and subscribes. */
export async function enablePush() {
  const perm = await Notification.requestPermission();
  if (perm !== 'granted') throw new Error(perm === 'denied' ? 'Notifications are blocked for this site.' : 'Notifications weren’t allowed.');
  const reg = await registration();
  const key = keyBytes(store.me.vapid_public_key);
  let sub = await reg.pushManager.getSubscription();
  if (sub && !sameKey(sub, key)) {
    await sub.unsubscribe();
    sub = null;
  }
  if (!sub) sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key });
  await api('POST', '/api/push/subscribe', sub.toJSON());
}

export async function disablePush() {
  const sub = await currentSubscription();
  if (!sub) return;
  await api('POST', '/api/push/unsubscribe', { endpoint: sub.endpoint }).catch(() => {});
  await sub.unsubscribe();
}

/** On boot: re-send an existing subscription (e.g. after re-pairing). */
export async function syncPush() {
  if (!('Notification' in window) || Notification.permission !== 'granted') return;
  const sub = await currentSubscription();
  if (!sub) return;
  if (!sameKey(sub, keyBytes(store.me.vapid_public_key))) {
    await sub.unsubscribe().catch(() => {});
    return;
  }
  await api('POST', '/api/push/subscribe', sub.toJSON()).catch(() => {});
}

/**
 * Closes this request's notifications on this device: the device that
 * answers gets no "resolved" push, so the app tidies up itself. Never
 * throws.
 */
export async function closeRequestNotifications(id) {
  try {
    if (!('serviceWorker' in navigator) || !id) return;
    const reg = await Promise.race([registration(), new Promise((r) => setTimeout(() => r(null), 1500))]);
    if (!reg || !reg.getNotifications) return;
    for (const n of await reg.getNotifications()) {
      if (n.tag === 'req-' + id || n.tag === id || (n.data && n.data.request_id === id)) n.close();
    }
  } catch { /* nothing to tidy */ }
}
