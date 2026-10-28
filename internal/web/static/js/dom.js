// Tiny DOM helpers, icons and formatting. No inline styles anywhere: the
// CSP forbids them, so components toggle classes instead (CSSOM property
// writes like el.style.transform are fine).
//
// Exported API (stable; workstream I builds on it):
//   h(tag, attrs, ...kids)      element; attrs: class, text, on<event>, value, aria-*, …
//   fill(el, ...kids) / add(el, ...kids)   replace / append children (null-safe; use
//                               these, never the native replaceChildren/append)
//   sync(el, ...kids)           like fill, but nodes already in place stay put (keeps
//                               focus inside cached cards across re-renders)
//   icon(name, size?, extra?)   <svg class="ic …">; size '' | 'xs' | 'sm' | 'lg'
//   appIcon(cls)                the app mark
//   agentName(a) agentInitials(a) agentClass(a) agentBadge(a)
//   basename(p) relPath(p, cwd) repoName(session, req)
//   toMs(t) ago(t, now) agoLong(t, now) left(t, now) clock(t)
//   grantPrefix(req) requestVerb(req) stateLabel(session)
//   money(usd) tokens(n) plural(n, one, many) richText(text)  (`code` spans, no HTML)
//   uid(prefix) vibrate(ms)

const SVG = 'http://www.w3.org/2000/svg';

/**
 * h('div', {class: 'x', onclick: fn, 'aria-label': '…'}, child, …)
 * Children may be strings, nodes, arrays, null/false (skipped).
 */
export function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (v == null || v === false) continue;
      if (k === 'class') el.className = v;
      else if (k === 'text') el.textContent = v;
      else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
      else if (k === 'value') el.value = v;
      else el.setAttribute(k, v === true ? '' : String(v));
    }
  }
  append(el, kids);
  return el;
}

/**
 * fill(el, ...kids) replaces el's children; add(el, ...kids) appends.
 * Unlike the native replaceChildren/append they skip null/undefined/false
 * (which the DOM would render as the text "null") and flatten arrays.
 * Use these everywhere instead of the native methods.
 */
export function fill(el, ...kids) {
  el.replaceChildren();
  append(el, kids);
  return el;
}

export function add(el, ...kids) {
  append(el, kids);
  return el;
}

/**
 * sync(el, ...kids) makes el's children exactly kids (nodes; strings become
 * text) while leaving nodes that are already in place untouched, so a
 * re-render keeps focus, selection and scroll inside cached elements (a
 * card whose deny reason is being typed). Use it for lists of cached nodes.
 */
export function sync(el, ...kids) {
  const nodes = [];
  const flat = (ks) => {
    for (const k of ks) {
      if (k == null || k === false) continue;
      if (Array.isArray(k)) flat(k);
      else nodes.push(k instanceof Node ? k : document.createTextNode(String(k)));
    }
  };
  flat(kids);
  nodes.forEach((n, i) => {
    const at = el.childNodes[i];
    if (at !== n) el.insertBefore(n, at || null);
  });
  while (el.childNodes.length > nodes.length) el.lastChild.remove();
  return el;
}

function append(el, kids) {
  for (const k of kids) {
    if (k == null || k === false) continue;
    if (Array.isArray(k)) append(el, k);
    else el.append(k instanceof Node ? k : String(k));
  }
}

// Icon paths: 24×24, stroke-based, drawn for this app.
const ICONS = {
  inbox: ['M3 13.5h4.6l1.4 2.5h6l1.4-2.5H21', 'M5.2 5.6 3 13.5V18a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-4.5l-2.2-7.9A2 2 0 0 0 16.9 4H7.1a2 2 0 0 0-1.9 1.6Z'],
  sessions: ['M4 5h16', 'M4 12h16', 'M4 19h10'],
  terminal: ['m5 8 4 4-4 4', 'M12 17h7', 'M3.5 4h17a1.5 1.5 0 0 1 1.5 1.5v13a1.5 1.5 0 0 1-1.5 1.5h-17A1.5 1.5 0 0 1 2 18.5v-13A1.5 1.5 0 0 1 3.5 4Z'],
  settings: ['M12 15.2a3.2 3.2 0 1 0 0-6.4 3.2 3.2 0 0 0 0 6.4Z', 'M19.4 15a1.6 1.6 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.6 1.6 0 0 0-1.8-.3 1.6 1.6 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.6 1.6 0 0 0-1-1.5 1.6 1.6 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.6 1.6 0 0 0 .3-1.8 1.6 1.6 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.6 1.6 0 0 0 1.5-1 1.6 1.6 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.6 1.6 0 0 0 1.8.3H9a1.6 1.6 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.6 1.6 0 0 0 1 1.5 1.6 1.6 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.6 1.6 0 0 0-.3 1.8V9a1.6 1.6 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.6 1.6 0 0 0-1.5 1Z'],
  more: ['M5 12h.01', 'M12 12h.01', 'M19 12h.01'],
  back: ['m15 18-6-6 6-6'],
  chev: ['m9 18 6-6-6-6'],
  chevDown: ['m6 9 6 6 6-6'],
  check: ['M5 12.5 10 17.5 19 7'],
  x: ['M18 6 6 18', 'm6 6 12 12'],
  plus: ['M12 5v14', 'M5 12h14'],
  clock: ['M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18Z', 'M12 7v5l3 2'],
  file: ['M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8Z', 'M14 3v5h5', 'M9 13h6', 'M9 17h4'],
  pencil: ['M4 20h4L19 9a2.8 2.8 0 0 0-4-4L4 16Z', 'm13.5 6.5 4 4'],
  globe: ['M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18Z', 'M3 12h18', 'M12 3a14 14 0 0 1 0 18', 'M12 3a14 14 0 0 0 0 18'],
  plug: ['M9 2v6', 'M15 2v6', 'M6 8h12v3a6 6 0 0 1-12 0Z', 'M12 17v5'],
  search: ['M11 18a7 7 0 1 0 0-14 7 7 0 0 0 0 14Z', 'm20 20-4-4'],
  shield: ['M12 3 4.5 6v6c0 4.4 3.2 7.8 7.5 9 4.3-1.2 7.5-4.6 7.5-9V6Z', 'm9 12 2 2 4-4'],
  shieldOff: ['M12 3 4.5 6v6c0 4.4 3.2 7.8 7.5 9 4.3-1.2 7.5-4.6 7.5-9V6Z', 'M9.5 9.5l5 5', 'M14.5 9.5l-5 5'],
  rules: ['M4 6h9', 'M4 12h6', 'M4 18h9', 'M17 4v4', 'M15 6h4', 'M14 12h6', 'M17 16v4', 'M15 18h4'],
  bell: ['M6 8a6 6 0 1 1 12 0c0 7 3 9 3 9H3s3-2 3-9', 'M10.3 21a1.9 1.9 0 0 0 3.4 0'],
  share: ['M12 3v12', 'm7.5 7.5 4.5-4.5 4.5 4.5', 'M5 12v7a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2v-7'],
  plusSquare: ['M5 3h14a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2Z', 'M12 8v8', 'M8 12h8'],
  desk: ['M3 5h18v11H3z', 'M8 20h8', 'M12 16v4'],
  away: ['M20.5 14.5A8.5 8.5 0 0 1 9.5 3.5a8.5 8.5 0 1 0 11 11Z'],
  moon: ['M20.5 14.5A8.5 8.5 0 0 1 9.5 3.5a8.5 8.5 0 1 0 11 11Z'],
  sun: ['M12 16a4 4 0 1 0 0-8 4 4 0 0 0 0 8Z', 'M12 2v2', 'M12 20v2', 'm4.9 4.9 1.4 1.4', 'm17.7 17.7 1.4 1.4', 'M2 12h2', 'M20 12h2', 'm4.9 19.1 1.4-1.4', 'm17.7 6.3 1.4-1.4'],
  monitor: ['M3 4h18v12H3z', 'M8 20h8', 'M12 16v4'],
  auto: ['M12 3v3', 'M12 18v3', 'M3 12h3', 'M18 12h3', 'M12 16a4 4 0 1 0 0-8 4 4 0 0 0 0 8Z'],
  phone: ['M8 2h8a2 2 0 0 1 2 2v16a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2Z', 'M11 18h2'],
  key: ['M6.5 11h11a1.5 1.5 0 0 1 1.5 1.5v7a1.5 1.5 0 0 1-1.5 1.5h-11A1.5 1.5 0 0 1 5 19.5v-7A1.5 1.5 0 0 1 6.5 11Z', 'M8.5 11V7.5a3.5 3.5 0 0 1 7 0V11', 'M12 15v2'],
  play: ['M7 4.5v15l12-7.5Z'],
  stop: ['M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18Z', 'M9.5 9.5h5v5h-5z'],
  user: ['M12 12a4 4 0 1 0 0-8 4 4 0 0 0 0 8Z', 'M4 21a8 8 0 0 1 16 0'],
  message: ['M21 12a8 8 0 0 1-11.6 7.1L4 20.5l1.4-4.9A8 8 0 1 1 21 12Z'],
  reply: ['m9 14-5-5 5-5', 'M4 9h10.5a5.5 5.5 0 0 1 0 11H11'],
  alert: ['M12 9v4', 'M12 17h.01', 'M10.3 3.9 2.4 17.5A2 2 0 0 0 4.1 20.5h15.8a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0Z'],
  flame: ['M12 22c4 0 7-2.7 7-6.8 0-3.6-2.5-6.2-4.3-8.2-.6 2-1.6 3.2-3 3.7.3-3.3-1-6.3-3.7-8.7C8.4 5.6 5 8.9 5 15.2 5 19.3 8 22 12 22Z'],
  lock: ['M6.5 11h11a1.5 1.5 0 0 1 1.5 1.5v7a1.5 1.5 0 0 1-1.5 1.5h-11A1.5 1.5 0 0 1 5 19.5v-7A1.5 1.5 0 0 1 6.5 11Z', 'M8.5 11V7.5a3.5 3.5 0 0 1 7 0V11'],
  rocket: ['M5 15c-1.5 1.3-2 5-2 5s3.7-.5 5-2c.7-.8.7-2.1-.1-2.9a2.1 2.1 0 0 0-2.9-.1Z', 'm12 15-3-3a22 22 0 0 1 2-3.9A12.9 12.9 0 0 1 22 2c0 2.7-.8 7.5-6 11a22.4 22.4 0 0 1-4 2Z', 'M9 12H4s.6-3 2-4c1.6-1.1 5 0 5 0', 'M12 15v5s3-.6 4-2c1.1-1.6 0-5 0-5'],
  branch: ['M6 3v12', 'M18 9a3 3 0 1 0 0-6 3 3 0 0 0 0 6Z', 'M6 21a3 3 0 1 0 0-6 3 3 0 0 0 0 6Z', 'M18 9a9 9 0 0 1-9 9'],
  download: ['M12 3v12', 'm7 10 5 5 5-5', 'M5 21h14'],
  upload: ['M12 21V9', 'm7 14 5-5 5 5', 'M5 3h14'],
  folder: ['M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2Z'],
  hourglass: ['M6 2h12', 'M6 22h12', 'M7 2v4a5 5 0 0 0 10 0V2', 'M7 22v-4a5 5 0 0 1 10 0v4'],
  link: ['M10 14a5 5 0 0 0 7.1 0l3-3a5 5 0 0 0-7.1-7.1l-1 1', 'M14 10a5 5 0 0 0-7.1 0l-3 3a5 5 0 0 0 7.1 7.1l1-1'],
  logout: ['M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4', 'm16 17 5-5-5-5', 'M21 12H9'],
  wifiOff: ['M2 2l20 20', 'M8.5 16.4a5 5 0 0 1 7 0', 'M5 12.9a10 10 0 0 1 5.2-2.8', 'M19 12.9a10 10 0 0 0-2.4-1.6', 'M2 8.8a15 15 0 0 1 4.2-2.6', 'M22 8.8A15 15 0 0 0 10.7 5', 'M12 20h.01'],
  spark: ['M12 3v4', 'M12 17v4', 'M3 12h4', 'M17 12h4', 'm5.6 5.6 2.8 2.8', 'm15.6 15.6 2.8 2.8', 'm5.6 18.4 2.8-2.8', 'm15.6 8.4 2.8-2.8'],
  wand: ['m15 4 1.5 1.5', 'M19 3v2', 'M21 7h-2', 'M14 10 4 20', 'm12.5 5.5 6 6', 'M15 2.5v1', 'M20.5 9h1'],
  tasks: ['M9 5h11', 'M9 12h11', 'M9 19h11', 'm3.5 5 1.2 1.2L7 4', 'm3.5 12 1.2 1.2L7 11', 'm3.5 19 1.2 1.2L7 18'],
  chart: ['M4 20V10', 'M10 20V4', 'M16 20v-7', 'M22 20H2'],
  command: ['M9 6v12', 'M15 6v12', 'M6 9h12', 'M6 15h12', 'M9 6a3 3 0 1 0-3 3', 'M15 6a3 3 0 1 1 3 3', 'M9 18a3 3 0 1 1-3-3', 'M15 18a3 3 0 1 0 3-3'],
  keyboard: ['M3 6h18a1 1 0 0 1 1 1v10a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1Z', 'M6 10h.01', 'M10 10h.01', 'M14 10h.01', 'M18 10h.01', 'M7 14h10'],
  question: ['M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18Z', 'M9.5 9.2a2.6 2.6 0 0 1 5 .8c0 1.8-2.5 2.4-2.5 4', 'M12 17h.01'],
  plan: ['M9 4 3 6v14l6-2 6 2 6-2V4l-6 2Z', 'M9 4v14', 'M15 6v14'],
  trash: ['M4 7h16', 'M10 11v6', 'M14 11v6', 'M5 7l1 12a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2l1-12', 'M9 7V4h6v3'],
  eye: ['M2 12s3.6-7 10-7 10 7 10 7-3.6 7-10 7S2 12 2 12Z', 'M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6Z'],
  info: ['M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18Z', 'M12 11v6', 'M12 7.5h.01'],
  flask: ['M9 3h6', 'M10 3v6L4.5 18.5A1.7 1.7 0 0 0 6 21h12a1.7 1.7 0 0 0 1.5-2.5L14 9V3', 'M7 15h10'],
  agent: ['M12 3v2', 'M5 9a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2Z', 'M9.5 12h.01', 'M14.5 12h.01', 'M9.5 15.5h5'],
};

/** icon('check', 'sm') → <svg class="ic sm">. */
export function icon(name, size = '', extra = '') {
  const svg = document.createElementNS(SVG, 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('class', ['ic', size, extra].filter(Boolean).join(' '));
  svg.setAttribute('aria-hidden', 'true');
  svg.setAttribute('focusable', 'false');
  for (const d of ICONS[name] || []) {
    const p = document.createElementNS(SVG, 'path');
    p.setAttribute('d', d);
    svg.append(p);
  }
  return svg;
}

export function appIcon(cls = 'mark') {
  return h('img', { class: cls, src: '/icons/icon.svg', alt: '', width: 26, height: 26 });
}

// ---------- Agents ----------

const AGENTS = { claude: 'Claude Code', codex: 'Codex', opencode: 'OpenCode', grok: 'Grok Build', generic: 'Agent' };
const AGENT_INITIALS = { claude: 'CC', codex: 'Cx', opencode: 'Oc', grok: 'Gk', generic: '>_' };
export const agentName = (a) => AGENTS[a] || (a ? a[0].toUpperCase() + a.slice(1) : 'Agent');
export const agentInitials = (a) => AGENT_INITIALS[a] || (a || '?').slice(0, 2);
export const agentClass = (a) => (a === 'claude' || a === 'codex' || a === 'opencode' || a === 'grok' ? a : 'generic');

export function agentBadge(agent) {
  return h('span', { class: 'agent ' + agentClass(agent) }, agentName(agent));
}

// ---------- Paths ----------

export function basename(p) {
  if (!p) return '';
  const parts = String(p).replace(/\/+$/, '').split('/');
  return parts[parts.length - 1] || p;
}

/** p relative to cwd when inside it. */
export function relPath(p, cwd) {
  if (!p) return '';
  const dir = cwd ? cwd.replace(/\/?$/, '/') : '';
  return dir && p.startsWith(dir) ? p.slice(dir.length) : p;
}

/** Repo label for a session: its title, else the repo/cwd directory name. */
export function repoName(s, req) {
  if (s && s.title) return s.title;
  if (s && s.repo_root) return basename(s.repo_root);
  return basename((s && s.cwd) || (req && req.cwd) || '');
}

// ---------- Time ----------

const ms = (t) => (t ? new Date(t).getTime() : 0);
export { ms as toMs };

/** "now", "45s", "12m", "3h", "2d". */
export function ago(t, now = Date.now()) {
  const d = Math.max(0, (now - ms(t)) / 1000);
  if (d < 10) return 'now';
  if (d < 60) return Math.floor(d) + 's';
  if (d < 3600) return Math.floor(d / 60) + 'm';
  if (d < 86400) return Math.floor(d / 3600) + 'h';
  return Math.floor(d / 86400) + 'd';
}

export function agoLong(t, now = Date.now()) {
  const a = ago(t, now);
  return a === 'now' ? 'just now' : a + ' ago';
}

/** Countdown "8:05" (or "1h 02m"). */
export function left(t, now = Date.now()) {
  const s = Math.max(0, Math.floor((ms(t) - now) / 1000));
  if (s >= 3600) return Math.floor(s / 3600) + 'h ' + String(Math.floor((s % 3600) / 60)).padStart(2, '0') + 'm';
  return Math.floor(s / 60) + ':' + String(s % 60).padStart(2, '0');
}

export function clock(t) {
  const d = new Date(t);
  return d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
}

// ---------- Numbers and text ----------

/** "$0.42", "$12.30", "$1.2k". */
export function money(usd) {
  const v = Number(usd) || 0;
  if (v >= 1000) return '$' + (v / 1000).toFixed(1) + 'k';
  if (v > 0 && v < 0.01) return '<$0.01';
  return '$' + v.toFixed(2);
}

/** "940", "12.4k", "3.1M". */
export function tokens(n) {
  const v = Number(n) || 0;
  if (v >= 1e6) return (v / 1e6).toFixed(v >= 1e7 ? 0 : 1) + 'M';
  if (v >= 1e3) return (v / 1e3).toFixed(v >= 1e4 ? 0 : 1) + 'k';
  return String(v);
}

export const plural = (n, one, many) => (n === 1 ? one : many);

/**
 * Text with `backtick` spans as <code>, built from text nodes: safe for
 * any untrusted string (rule titles, suggestions).
 */
export function richText(text) {
  const out = [];
  const parts = String(text || '').split('`');
  parts.forEach((p, i) => {
    if (!p) return;
    out.push(i % 2 ? h('code', null, p) : p);
  });
  return out;
}

let uidN = 0;
/** A document-unique id for label/aria wiring. */
export const uid = (prefix = 'u') => `${prefix}-${++uidN}`;

// ---------- Requests and sessions ----------

/** What a session-scoped grant would cover (mirrors the daemon's rule). */
export function grantPrefix(req) {
  if (req.tool_kind !== 'shell' || !req.command) return req.tool;
  const w = req.command.trim().split(/\s+/);
  if (w.length > 1 && !/^[-./~]/.test(w[1]) && !w[1].includes('/')) return w[0] + ' ' + w[1];
  return w[0];
}

/** Verb phrase for a request: "Wants to run". */
export function requestVerb(req) {
  if (req.checkpoint === 'question') return (req.questions && req.questions.length > 1) ? `Asks ${req.questions.length} questions` : 'Asks you';
  if (req.checkpoint === 'plan') return 'Wants a plan approved';
  switch (req.tool_kind) {
    case 'shell': return 'Wants to run';
    case 'edit': return (req.paths && req.paths.length > 1) ? `Wants to edit ${req.paths.length} files` : 'Wants to edit';
    case 'read': return 'Wants to read';
    case 'web': return 'Wants to fetch';
    case 'mcp': return 'Wants to use';
    default: return 'Wants to use';
  }
}

export function stateLabel(s) {
  switch (s.state) {
    case 'working': return 'Working';
    case 'waiting': return 'Waiting for you';
    case 'blocked':
      if (s.block_kind === 'question') return 'Has a question';
      if (s.block_kind === 'auth') return 'Needs sign-in';
      if (s.state_detail && /plan/i.test(s.state_detail)) return 'Wants a plan approved';
      return 'Needs approval';
    case 'errored': return 'Error';
    case 'exited': return 'Ended';
    case 'starting': return 'Starting';
    default: return 'Unknown';
  }
}

export function vibrate(ms = 10) {
  // Only after a real tap; browsers refuse (and log) otherwise.
  if (!navigator.vibrate || (navigator.userActivation && !navigator.userActivation.isActive)) return;
  try { navigator.vibrate(ms); } catch { /* not supported */ }
}
