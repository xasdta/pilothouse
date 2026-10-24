/*
 * Theme: 'system' (default), 'light' or 'dark'. A classic script loaded
 * synchronously in <head> so the chosen theme applies before first paint
 * (inline scripts are forbidden by the CSP).
 *
 *   PilothouseTheme.get()        → 'system' | 'light' | 'dark'
 *   PilothouseTheme.set(mode)    stores it, applies it, fires 'pilothouse:theme'
 *   PilothouseTheme.effective()  → 'light' | 'dark' (what is on screen)
 */
(function (g) {
  'use strict';
  var KEY = 'ph-theme';
  var COLORS = { light: '#f4f5f8', dark: '#0a0c10' };
  var root = document.documentElement;

  function get() {
    try {
      var t = g.localStorage.getItem(KEY);
      return t === 'light' || t === 'dark' ? t : 'system';
    } catch (e) {
      return 'system';
    }
  }

  function effective() {
    var t = get();
    if (t !== 'system') return t;
    return g.matchMedia && g.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  }

  function apply() {
    var t = get();
    if (t === 'system') root.removeAttribute('data-theme');
    else root.setAttribute('data-theme', t);
    // The browser chrome follows: both media-specific theme-color metas
    // get the chosen colour, or their own when following the system.
    var metas = document.querySelectorAll('meta[name="theme-color"]');
    for (var i = 0; i < metas.length; i++) {
      var dark = (metas[i].getAttribute('media') || '').indexOf('dark') >= 0;
      metas[i].setAttribute('content', t === 'system' ? (dark ? COLORS.dark : COLORS.light) : COLORS[t]);
    }
  }

  function set(mode) {
    try {
      if (mode === 'light' || mode === 'dark') g.localStorage.setItem(KEY, mode);
      else g.localStorage.removeItem(KEY);
    } catch (e) { /* private mode: applies for this page only */ }
    if (mode === 'light' || mode === 'dark') root.setAttribute('data-theme', mode);
    else root.removeAttribute('data-theme');
    apply();
    try { g.dispatchEvent(new CustomEvent('pilothouse:theme', { detail: { mode: get(), effective: effective() } })); } catch (e) { /* old browsers */ }
  }

  apply();
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', apply);
  g.PilothouseTheme = { get: get, set: set, effective: effective };
})(window);
