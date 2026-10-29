/*
 * Device key store and signed decisions. A classic script shared by the
 * page (window.PilothouseKeys) and the service worker (importScripts), so
 * both sign decisions with the same canonical format.
 *
 * The ECDSA P-256 private key is generated non-extractable and kept in
 * IndexedDB: it can sign on this device but can never be read out.
 */
(function (g) {
  'use strict';

  var DB_NAME = 'pilothouse';
  var STORE = 'kv';
  var KEY = 'device-key';

  function openDB() {
    return new Promise(function (resolve, reject) {
      var req = g.indexedDB.open(DB_NAME, 1);
      req.onupgradeneeded = function () { req.result.createObjectStore(STORE); };
      req.onsuccess = function () { resolve(req.result); };
      req.onerror = function () { reject(req.error); };
    });
  }

  function tx(mode, fn) {
    return openDB().then(function (db) {
      return new Promise(function (resolve, reject) {
        var t = db.transaction(STORE, mode);
        var req = fn(t.objectStore(STORE)); // an IDBRequest
        t.oncomplete = function () { db.close(); resolve(req ? req.result : undefined); };
        t.onerror = function () { db.close(); reject(t.error); };
        t.onabort = function () { db.close(); reject(t.error); };
      });
    });
  }

  function b64url(buf) {
    var bytes = new Uint8Array(buf);
    var s = '';
    for (var i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
    return g.btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  }

  function randomNonce() {
    var b = new Uint8Array(18);
    g.crypto.getRandomValues(b);
    return b64url(b);
  }

  /** The exact string the daemon verifies (internal/web CanonicalDecision). */
  function canonical(id, behavior, scope, ts, nonce) {
    return ['pilothouse-decide:v1', id, behavior, scope || 'once', String(ts), nonce].join('\n');
  }

  function load() {
    return tx('readonly', function (st) { return st.get(KEY); });
  }

  function noKey() {
    try { g.dispatchEvent(new g.Event('pilothouse:nokey')); } catch (e) { /* older browser */ }
  }

  /**
   * Makes a fresh key pair without storing it; resolves to the record
   * {privateKey, publicKey, spki, created}. save() stores it.
   */
  function generate() {
    var subtle = g.crypto && g.crypto.subtle;
    if (!subtle) return Promise.reject(new Error('This browser cannot create keys here (a secure https connection is required).'));
    return subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign', 'verify'])
      .then(function (pair) {
        return subtle.exportKey('spki', pair.publicKey).then(function (spki) {
          return { privateKey: pair.privateKey, publicKey: pair.publicKey, spki: b64url(spki), created: Date.now() };
        });
      });
  }

  /** Stores rec as this device's key (replacing any old one); resolves to the SPKI. */
  function save(rec) {
    return tx('readwrite', function (st) { return st.put(rec, KEY); }).then(function () { return rec.spki; });
  }

  /** Creates and stores a fresh key pair; resolves to the SPKI. */
  function create() {
    return generate().then(save);
  }

  function clear() {
    return tx('readwrite', function (st) { return st.delete(KEY); });
  }

  /**
   * Signs and posts a decision. Resolves to {ok, status, data}; never
   * rejects on HTTP errors so callers can roll back optimistic UI.
   */
  function decide(id, behavior, scope, message) {
    scope = scope || 'once';
    return load().then(function (rec) {
      if (!rec || !rec.privateKey) {
        noKey();
        return { ok: false, status: 0, data: { code: 'no_key', error: 'This device has no signing key. Pair it again.' } };
      }
      var ts = Date.now();
      var nonce = randomNonce();
      var msg = new TextEncoder().encode(canonical(id, behavior, scope, ts, nonce));
      return g.crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, rec.privateKey, msg).then(function (sig) {
        var body = { behavior: behavior, scope: scope, ts: ts, nonce: nonce, sig: b64url(sig) };
        if (message) body.message = message;
        return g.fetch('/api/requests/' + encodeURIComponent(id) + '/decide', {
          method: 'POST',
          credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json', 'X-Pilothouse': '1' },
          body: JSON.stringify(body)
        }).then(function (res) {
          return res.json().catch(function () { return {}; }).then(function (data) {
            return { ok: res.ok, status: res.status, data: data };
          });
        });
      });
    }).catch(function (err) {
      return { ok: false, status: 0, data: { code: 'network', error: (err && err.message) || 'Network error' } };
    });
  }

  /** The exact string the daemon verifies for 0.2 actions (internal/web CanonicalAction). */
  function canonicalAction(action, target, payloadHash, ts, nonce) {
    return ['pilothouse-action:v2', action, target, payloadHash, String(ts), nonce].join('\n');
  }

  function sha256b64url(text) {
    return g.crypto.subtle.digest('SHA-256', new TextEncoder().encode(text)).then(b64url);
  }

  /**
   * Signs an action. payload is any JSON value; it is sent as the exact
   * JSON text that was hashed. Resolves to the request body
   * {payload, ts, nonce, sig}; rejects with code 'no_key' when unpaired.
   */
  function signAction(action, target, payload) {
    var text = JSON.stringify(payload === undefined ? {} : payload);
    return load().then(function (rec) {
      if (!rec || !rec.privateKey) {
        noKey();
        var e = new Error('This device has no signing key. Pair it again.');
        e.code = 'no_key';
        throw e;
      }
      var ts = Date.now();
      var nonce = randomNonce();
      return sha256b64url(text).then(function (h) {
        var msg = new TextEncoder().encode(canonicalAction(action, target, h, ts, nonce));
        return g.crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, rec.privateKey, msg);
      }).then(function (sig) {
        return { payload: text, ts: ts, nonce: nonce, sig: b64url(sig) };
      });
    });
  }

  /**
   * Signs an action and POSTs it to path. Resolves to {ok, status, data};
   * never rejects, so callers can roll back optimistic UI.
   */
  function postSigned(path, action, target, payload) {
    return signAction(action, target, payload).then(function (body) {
      return g.fetch(path, {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json', 'X-Pilothouse': '1' },
        body: JSON.stringify(body)
      }).then(function (res) {
        if (res.status === 204) return { ok: true, status: 204, data: null };
        return res.json().catch(function () { return {}; }).then(function (data) {
          return { ok: res.ok, status: res.status, data: data };
        });
      });
    }).catch(function (err) {
      return { ok: false, status: 0, data: { code: (err && err.code) || 'network', error: (err && err.message) || 'Network error' } };
    });
  }

  /**
   * Answers a request (0.2): behavior, optional scope, message and answers
   * ({questionKey: answer}). Same result shape as decide().
   */
  function respond(id, body) {
    return postSigned('/api/requests/' + encodeURIComponent(id) + '/respond', 'decide', id, body);
  }

  g.PilothouseKeys = {
    load: load, generate: generate, save: save, create: create, clear: clear, canonical: canonical, decide: decide,
    canonicalAction: canonicalAction, signAction: signAction, postSigned: postSigned, respond: respond
  };
})(self);
