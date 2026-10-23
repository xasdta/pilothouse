// Tiny DOM helpers, icons and formatting. No inline styles anywhere: the
// CSP forbids them, so components toggle classes instead.

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
  back: ['m15 18-6-6 6-6'],
  chev: ['m9 18 6-6-6-6'],
  check: ['M5 12.5 10 17.5 19 7'],
  x: ['M18 6 6 18', 'm6 6 12 12'],
  clock: ['M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18Z', 'M12 7v5l3 2'],
  file: ['M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8Z', 'M14 3v5h5', 'M9 13h6', 'M9 17h4'],
  pencil: ['M4 20h4L19 9a2.8 2.8 0 0 0-4-4L4 16Z', 'm13.5 6.5 4 4'],
  globe: ['M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18Z', 'M3 12h18', 'M12 3a14 14 0 0 1 0 18', 'M12 3a14 14 0 0 0 0 18'],
  plug: ['M9 2v6', 'M15 2v6', 'M6 8h12v3a6 6 0 0 1-12 0Z', 'M12 17v5'],
  search: ['M11 18a7 7 0 1 0 0-14 7 7 0 0 0 0 14Z', 'm20 20-4-4'],
  shield: ['M12 3 4.5 6v6c0 4.4 3.2 7.8 7.5 9 4.3-1.2 7.5-4.6 7.5-9V6Z', 'm9 12 2 2 4-4'],
  bell: ['M6 8a6 6 0 1 1 12 0c0 7 3 9 3 9H3s3-2 3-9', 'M10.3 21a1.9 1.9 0 0 0 3.4 0'],
  share: ['M12 3v12', 'm7.5 7.5 4.5-4.5 4.5 4.5', 'M5 12v7a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2v-7'],
  plusSquare: ['M5 3h14a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2Z', 'M12 8v8', 'M8 12h8'],
  desk: ['M3 5h18v11H3z', 'M8 20h8', 'M12 16v4'],
  away: ['M20.5 14.5A8.5 8.5 0 0 1 9.5 3.5a8.5 8.5 0 1 0 11 11Z'],
  auto: ['M12 3v3', 'M12 18v3', 'M3 12h3', 'M18 12h3', 'M12 16a4 4 0 1 0 0-8 4 4 0 0 0 0 8Z'],
  phone: ['M8 2h8a2 2 0 0 1 2 2v16a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2Z', 'M11 18h2'],
  key: ['M6.5 11h11a1.5 1.5 0 0 1 1.5 1.5v7a1.5 1.5 0 0 1-1.5 1.5h-11A1.5 1.5 0 0 1 5 19.5v-7A1.5 1.5 0 0 1 6.5 11Z', 'M8.5 11V7.5a3.5 3.5 0 0 1 7 0V11', 'M12 15v2'],
  play: ['M7 4.5v15l12-7.5Z'],
  stop: ['M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18Z', 'M9.5 9.5h5v5h-5z'],
  user: ['M12 12a4 4 0 1 0 0-8 4 4 0 0 0 0 8Z', 'M4 21a8 8 0 0 1 16 0'],
  message: ['M21 12a8 8 0 0 1-11.6 7.1L4 20.5l1.4-4.9A8 8 0 1 1 21 12Z'],
  alert: ['M12 9v4', 'M12 17h.01', 'M10.3 3.9 2.4 17.5A2 2 0 0 0 4.1 20.5h15.8a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0Z'],
  hourglass: ['M6 2h12', 'M6 22h12', 'M7 2v4a5 5 0 0 0 10 0V2', 'M7 22v-4a5 5 0 0 1 10 0v4'],
  link: ['M10 14a5 5 0 0 0 7.1 0l3-3a5 5 0 0 0-7.1-7.1l-1 1', 'M14 10a5 5 0 0 0-7.1 0l-3 3a5 5 0 0 0 7.1 7.1l1-1'],
  logout: ['M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4', 'm16 17 5-5-5-5', 'M21 12H9'],
  wifiOff: ['M2 2l20 20', 'M8.5 16.4a5 5 0 0 1 7 0', 'M5 12.9a10 10 0 0 1 5.2-2.8', 'M19 12.9a10 10 0 0 0-2.4-1.6', 'M2 8.8a15 15 0 0 1 4.2-2.6', 'M22 8.8A15 15 0 0 0 10.7 5', 'M12 20h.01'],
  spark: ['M12 3v4', 'M12 17v4', 'M3 12h4', 'M17 12h4', 'm5.6 5.6 2.8 2.8', 'm15.6 15.6 2.8 2.8', 'm5.6 18.4 2.8-2.8', 'm15.6 8.4 2.8-2.8'],
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

// ---------- Formatting ----------

const AGENTS = { claude: 'Claude Code', codex: 'Codex', generic: 'Agent' };
const AGENT_INITIALS = { claude: 'CC', codex: 'Cx', generic: '>_' };
export const agentName = (a) => AGENTS[a] || (a ? a[0].toUpperCase() + a.slice(1) : 'Agent');
export const agentInitials = (a) => AGENT_INITIALS[a] || (a || '?').slice(0, 2);
export const agentClass = (a) => (a === 'claude' || a === 'codex' ? a : 'generic');

export function agentBadge(agent) {
  return h('span', { class: 'agent ' + agentClass(agent) }, agentName(agent));
}

export function basename(p) {
  if (!p) return '';
  const parts = String(p).replace(/\/+$/, '').split('/');
  return parts[parts.length - 1] || p;
}

/** p relative to cwd when inside it. */
export function relPath(p, cwd) {
  const dir = cwd ? cwd.replace(/\/?$/, '/') : '';
  return dir && p.startsWith(dir) ? p.slice(dir.length) : p;
}

/** Repo label for a session: its title, else the repo/cwd directory name. */
export function repoName(s, req) {
  if (s && s.title) return s.title;
  if (s && s.repo_root) return basename(s.repo_root);
  return basename((s && s.cwd) || (req && req.cwd) || '');
}

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

/** What a session-scoped grant would cover (mirrors the daemon's rule). */
export function grantPrefix(req) {
  if (req.tool_kind !== 'shell' || !req.command) return req.tool;
  const w = req.command.trim().split(/\s+/);
  if (w.length > 1 && !/^[-./~]/.test(w[1]) && !w[1].includes('/')) return w[0] + ' ' + w[1];
  return w[0];
}

/** Verb phrase for a request: ["Wants to run", "Bash"]. */
export function requestVerb(req) {
  switch (req.tool_kind) {
    case 'shell': return 'Wants to run';
    case 'edit': return (req.paths && req.paths.length > 1) ? `Wants to edit ${req.paths.length} files` : 'Wants to edit';
    case 'read': return 'Wants to read';
    case 'web': return 'Wants to fetch';
    case 'mcp': return 'Wants to use';
    default: return req.checkpoint === 'question' ? 'Asks' : 'Wants to use';
  }
}

export function stateLabel(s) {
  switch (s.state) {
    case 'working': return 'Working';
    case 'waiting': return 'Waiting for you';
    case 'blocked':
      return s.block_kind === 'question' ? 'Has a question' : s.block_kind === 'auth' ? 'Needs sign-in' : 'Needs approval';
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
