// ⌘K command palette, keyboard shortcuts and the shortcuts help.
//
//   ⌘K / Ctrl K / /   palette: jump to sessions, requests, tasks, pages; run actions
//   j / k             next / previous item (request cards, then rows)
//   a  s  d           allow · allow for session · deny (plans: approve / request changes;
//                     questions: send / decline) on the selected or first request
//   1–9               pick an option on a question card
//   r                 reply to the selected session (or the one on screen)
//   n                 new task       g i/s/t/n/r/,  go to inbox/sessions/tasks/insights/rules/settings
//   Enter             open the selected item      ?  this help      Esc  close / clear
//
// Views opt in by marking elements: [data-nav] (j/k stops; with
// data-session / data-request for r and a/d) and [data-shortcut="reply"]
// on a reply box, which r focuses.
//
// Exported: initKeys({navigate}), openHelp(), refreshSelection().

import { h, icon, fill, add, agentName, agentClass, agentInitials, repoName, stateLabel, left, toMs, uid } from './dom.js';
import { store, api } from './api.js';
import { go, setPaletteOpener, toast, setTheme, theme, isMac, refreshChrome } from './ui.js';
import { controllerFor } from './card.js';

let selKey = '';

function keyOf(el) {
  if (!el) return '';
  if (el.dataset.request) return 'r:' + el.dataset.request;
  if (el.dataset.session) return 's:' + el.dataset.session;
  return el.dataset.key ? 'k:' + el.dataset.key : '';
}

function navItems() {
  const visible = (el) => el.offsetParent !== null || el.getClientRects().length > 0;
  const queue = [...document.querySelectorAll('.queue [data-nav]')].filter(visible);
  const main = [...document.querySelectorAll('main [data-nav]')].filter(visible);
  return [...queue, ...main];
}

function selected() {
  if (!selKey) return null;
  return navItems().find((el) => keyOf(el) === selKey) || null;
}

function select(el, focus = true) {
  for (const e of document.querySelectorAll('.is-selected')) e.classList.remove('is-selected');
  selKey = keyOf(el);
  if (!el) return;
  el.classList.add('is-selected');
  if (focus) el.focus({ preventScroll: true });
  el.scrollIntoView({ block: 'nearest', behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' });
}

/** Re-applies the selection after a re-render replaced elements. */
export function refreshSelection() {
  if (!selKey) return;
  const el = selected();
  for (const e of document.querySelectorAll('.is-selected')) if (e !== el) e.classList.remove('is-selected');
  if (el) el.classList.add('is-selected');
}

function move(step) {
  const items = navItems();
  if (!items.length) return;
  const cur = selected();
  let i = cur ? items.indexOf(cur) + step : step > 0 ? 0 : items.length - 1;
  i = Math.max(0, Math.min(items.length - 1, i));
  select(items[i]);
}

function targetCard() {
  const cur = selected();
  if (cur && cur.dataset.request) return controllerFor(cur);
  const active = document.activeElement && controllerFor(document.activeElement);
  if (active) return active;
  const first = navItems().find((el) => el.dataset.request && !el.classList.contains('resolved'));
  return first ? controllerFor(first) : null;
}

function act(what) {
  const c = targetCard();
  if (!c) { toast('Nothing waiting for an answer'); return; }
  select(c.card, false);
  c[what]();
}

function focusReply(tries = 0) {
  const box = document.querySelector('main [data-shortcut="reply"]');
  if (box) {
    box.focus();
    box.scrollIntoView({ block: 'nearest' });
    return true;
  }
  if (tries < 30) requestAnimationFrame(() => focusReply(tries + 1));
  return false;
}

function reply() {
  if (focusReply(99)) return;
  const cur = selected();
  let sid = cur && cur.dataset.session;
  if (!sid && cur && cur.dataset.request) {
    const r = store.requests.get(cur.dataset.request);
    sid = r && r.session_id;
  }
  if (!sid) {
    const m = /^\/s\/([\w-]+)/.exec(location.pathname);
    sid = m && m[1];
  }
  if (!sid) { toast('Select a session to reply to (j / k)'); return; }
  const s = store.sessions.get(sid);
  if (s && s.caps && !s.caps.reply) { toast(`${agentName(s.agent)} can’t receive replies in this session`, 'bad'); return; }
  go('/s/' + sid);
  focusReply();
}

function isTyping(t) {
  if (!t || !t.tagName) return false;
  const tag = t.tagName;
  return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || t.isContentEditable;
}

let gPending = 0;
const G = { i: '/', s: '/sessions', t: '/tasks', n: '/insights', r: '/rules', ',': '/settings' };

function onKey(e) {
  if (e.defaultPrevented) return;
  const k = e.key;
  if ((e.metaKey || e.ctrlKey) && !e.altKey && (k === 'k' || k === 'K')) {
    e.preventDefault();
    openPalette();
    return;
  }
  if (e.metaKey || e.ctrlKey || e.altKey || isTyping(e.target) || document.querySelector('dialog[open]')) return;
  if (gPending) {
    clearTimeout(gPending);
    gPending = 0;
    if (G[k]) { e.preventDefault(); go(G[k]); }
    return;
  }
  const cur = selected();
  switch (k) {
    case 'j': case 'ArrowDown': if (k === 'ArrowDown' && !cur) return; e.preventDefault(); move(1); break;
    case 'k': case 'ArrowUp': if (k === 'ArrowUp' && !cur) return; e.preventDefault(); move(-1); break;
    case 'a': e.preventDefault(); act('allow'); break;
    case 's': e.preventDefault(); act('allowSession'); break;
    case 'd': e.preventDefault(); act('deny'); break;
    case 'r': e.preventDefault(); reply(); break;
    case 'n': e.preventDefault(); go('/new'); break;
    case '/': e.preventDefault(); openPalette(); break;
    case '?': e.preventDefault(); openHelp(); break;
    case 'g': gPending = setTimeout(() => { gPending = 0; }, 1200); break;
    case 'Escape': if (cur) { select(null); e.preventDefault(); } break;
    case 'Enter':
      if (cur && cur.dataset.request && cur === document.activeElement) { e.preventDefault(); go('/r/' + cur.dataset.request); }
      break;
    default:
      if (/^[1-9]$/.test(k)) {
        const c = targetCard();
        if (c && c.key(k)) e.preventDefault();
      }
  }
}

// ---------- Palette ----------

function score(q, text) {
  // Subsequence match; earlier, contiguous and word-start hits score higher.
  const t = text.toLowerCase();
  let s = 0;
  let ti = 0;
  let run = 0;
  const hits = [];
  for (const ch of q) {
    const i = t.indexOf(ch, ti);
    if (i < 0) return null;
    run = i === ti ? run + 1 : 0;
    s += 1 + run * 2 + (i === 0 || /[\s/·_-]/.test(t[i - 1]) ? 3 : 0) - Math.min(i - ti, 6) * 0.15;
    hits.push(i);
    ti = i + 1;
  }
  return { s: s - t.length * 0.01, hits };
}

function highlight(text, hits) {
  if (!hits || !hits.length) return text;
  const out = [];
  let last = 0;
  const set = new Set(hits);
  let buf = '';
  let inMark = false;
  for (let i = 0; i < text.length; i++) {
    const m = set.has(i);
    if (m !== inMark) {
      if (buf) out.push(inMark ? h('mark', null, buf) : buf);
      buf = '';
      inMark = m;
    }
    buf += text[i];
    last = i;
  }
  if (buf) out.push(inMark ? h('mark', null, buf) : buf);
  void last;
  return out;
}

function commands() {
  const out = [];
  const now = Date.now();
  const open = [...store.requests.values()].filter((r) => r.status === 'open').sort((a, b) => toMs(a.expires_at) - toMs(b.expires_at));
  for (const r of open) {
    const s = store.sessions.get(r.session_id);
    const what = r.checkpoint === 'question' ? 'Answer: ' + ((r.questions && r.questions[0] && r.questions[0].question) || 'a question')
      : r.checkpoint === 'plan' ? 'Review the plan: ' + (r.summary || '').replace(/^Plan:\s*/, '')
        : (r.command ? 'Allow? ' + r.command : r.summary || r.tool);
    out.push({ group: 'Needs you', title: what, sub: `${repoName(s, r)} · ${agentName(r.agent)} · ${left(r.expires_at, now)} left`, ico: 'hourglass', agent: r.agent, run: () => go('/r/' + r.id) });
  }
  const sessions = [...store.sessions.values()].sort((a, b) => toMs(b.updated_at) - toMs(a.updated_at));
  for (const s of sessions) {
    out.push({ group: 'Sessions', title: repoName(s) + (s.branch ? ' · ' + s.branch : ''), sub: `${agentName(s.agent)} · ${stateLabel(s)}`, agent: s.agent, run: () => go('/s/' + s.id) });
  }
  for (const t of [...store.tasks.values()].sort((a, b) => toMs(b.updated_at) - toMs(a.updated_at)).slice(0, 8)) {
    out.push({ group: 'Tasks', title: t.title, sub: `${t.status === 'review' ? 'Ready for review' : t.status} · ${t.branch}`, ico: 'tasks', run: () => go('/t/' + t.id) });
  }
  const pages = [['Inbox', '/', 'inbox'], ['Sessions', '/sessions', 'sessions'], ['Tasks', '/tasks', 'tasks'], ['New task', '/new', 'plus'],
    ['Insights', '/insights', 'chart'], ['Rules', '/rules', 'rules'], ['Settings', '/settings', 'settings']];
  for (const [title, path, ico] of pages) out.push({ group: 'Go to', title, sub: path === '/new' ? 'Start an agent in a fresh worktree' : '', ico, run: () => go(path) });
  const t = theme();
  out.push(
    { group: 'Actions', title: 'Theme: dark', sub: t === 'dark' ? 'On' : '', ico: 'moon', keywords: 'appearance night', run: () => setTheme('dark') },
    { group: 'Actions', title: 'Theme: light', sub: t === 'light' ? 'On' : '', ico: 'sun', keywords: 'appearance day', run: () => setTheme('light') },
    { group: 'Actions', title: 'Theme: match the system', sub: t === 'system' ? 'On' : '', ico: 'monitor', keywords: 'appearance auto', run: () => setTheme('system') },
    { group: 'Actions', title: 'I’m away — send requests here', ico: 'away', keywords: 'presence', run: () => setPresence('away') },
    { group: 'Actions', title: 'I’m at my desk', ico: 'desk', keywords: 'presence here', run: () => setPresence('here') },
    { group: 'Actions', title: 'Presence: automatic', ico: 'auto', keywords: 'presence idle', run: () => setPresence('auto') },
    { group: 'Actions', title: 'Send a test notification', ico: 'bell', keywords: 'push', run: () => api('POST', '/api/push/test').then(() => toast('Sent — it should arrive in a moment', 'ok'), (e) => toast(e.message, 'bad')) },
    { group: 'Actions', title: 'Keyboard shortcuts', ico: 'keyboard', keywords: 'help keys', run: () => openHelp() },
  );
  return out;
}

async function setPresence(mode) {
  try {
    store.presence = await api('PUT', '/api/presence', { mode });
    refreshChrome();
    toast(mode === 'away' ? 'Away — requests come to your devices' : mode === 'here' ? 'At desk — requests open in the terminal' : 'Presence follows your computer', 'ok');
  } catch (e) {
    toast(e.message, 'bad');
  }
}

let dlg = null;

export function openPalette() {
  if (dlg && dlg.open) return;
  const listId = uid('palette-list');
  const input = h('input', {
    type: 'text', role: 'combobox', 'aria-expanded': 'true', 'aria-controls': listId, 'aria-autocomplete': 'list',
    'aria-label': 'Search sessions, requests, pages and actions', placeholder: 'Search sessions, requests, pages…', autocomplete: 'off', spellcheck: 'false',
  });
  const list = h('div', { class: 'palette-list', id: listId, role: 'listbox', 'aria-label': 'Results' });
  dlg = h('dialog', { class: 'palette', 'aria-label': 'Command palette' },
    h('div', { class: 'palette-input' }, icon('search'), input, h('kbd', null, 'esc')),
    list,
    h('div', { class: 'palette-foot', 'aria-hidden': 'true' },
      h('span', null, h('kbd', null, '↑'), h('kbd', null, '↓'), 'move'),
      h('span', null, h('kbd', null, '↵'), 'open'),
      h('span', null, h('kbd', null, '?'), 'shortcuts')));
  const all = commands();
  let shown = [];
  let active = 0;
  function render() {
    const q = input.value.trim().toLowerCase();
    if (q) {
      shown = all.map((c) => ({ c, m: score(q, c.title + ' ' + (c.sub || '') + ' ' + (c.keywords || '') + ' ' + c.group) }))
        .filter((x) => x.m)
        .sort((a, b) => b.m.s - a.m.s)
        .slice(0, 30)
        .map((x) => ({ ...x.c, hits: score(q, x.c.title) }));
    } else {
      shown = all.filter((c) => c.group !== 'Actions' || all.length < 12).slice(0, 40);
    }
    active = Math.min(active, Math.max(0, shown.length - 1));
    const groups = [];
    let cur = null;
    shown.forEach((c, i) => {
      if (!cur || cur.name !== c.group) {
        const hid = uid('pg');
        cur = { name: c.group, el: h('div', { role: 'group', 'aria-labelledby': hid }, h('div', { class: 'palette-group', id: hid, role: 'presentation' }, c.group)) };
        groups.push(cur);
      }
      const id = listId + '-' + i;
      const ico = c.agent
        ? h('span', { class: 'ico agent-ico ' + agentClass(c.agent), 'aria-hidden': 'true' }, agentInitials(c.agent))
        : h('span', { class: 'ico', 'aria-hidden': 'true' }, icon(c.ico || 'chev', 'sm'));
      const item = h('div', { class: 'palette-item', role: 'option', id, 'aria-selected': String(i === active) },
        ico, h('span', { class: 'txt' }, h('b', null, highlight(c.title, c.hits && c.hits.hits)), c.sub ? h('span', null, c.sub) : null));
      item.addEventListener('mousemove', () => { if (active !== i) { active = i; mark(); } });
      item.addEventListener('click', () => run(i));
      add(cur.el, item);
    });
    fill(list, groups.map((g) => g.el), shown.length ? null : h('div', { class: 'palette-empty' }, 'Nothing matches “' + input.value.trim() + '”'));
    mark();
  }
  function mark() {
    list.querySelectorAll('[role=option]').forEach((el, i) => el.setAttribute('aria-selected', String(i === active)));
    const el = list.querySelector(`[aria-selected="true"]`);
    if (el) {
      input.setAttribute('aria-activedescendant', el.id);
      el.scrollIntoView({ block: 'nearest' });
    } else input.removeAttribute('aria-activedescendant');
  }
  function run(i) {
    const c = shown[i];
    if (!c) return;
    dlg.close();
    c.run();
  }
  input.addEventListener('input', () => { active = 0; render(); });
  input.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowDown') { e.preventDefault(); active = Math.min(shown.length - 1, active + 1); mark(); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); active = Math.max(0, active - 1); mark(); }
    else if (e.key === 'Enter') { e.preventDefault(); run(active); }
    else if (e.key === '?' && !input.value) { e.preventDefault(); dlg.close(); openHelp(); }
  });
  dlg.addEventListener('click', (e) => { if (e.target === dlg) dlg.close(); });
  dlg.addEventListener('close', () => { dlg.remove(); dlg = null; });
  add(document.body, dlg);
  dlg.showModal();
  render();
  input.focus();
}

// ---------- Help ----------

export function openHelp() {
  if (document.querySelector('dialog.help[open]')) return;
  const mod = isMac() ? '⌘' : 'Ctrl';
  const row = (keys, text) => h('div', null, h('dt', null, keys.map((k) => h('kbd', null, k))), h('dd', null, text));
  const titleId = uid('help');
  const d = h('dialog', { class: 'help', 'aria-labelledby': titleId },
    h('div', { class: 'sheet-head' }, h('h2', { id: titleId }, 'Keyboard shortcuts'),
      h('button', { class: 'icon-btn', type: 'button', 'aria-label': 'Close', onclick: () => d.close() }, icon('x'))),
    h('div', { class: 'sheet-body' },
      h('h3', { class: 'keys-group' }, 'Requests'),
      h('dl', { class: 'keys' },
        row(['a'], 'Allow / approve'), row(['d'], 'Deny / request changes'),
        row(['s'], 'Allow for this session'), row(['1', '–', '9'], 'Pick an answer')),
      h('h3', { class: 'keys-group' }, 'Move around'),
      h('dl', { class: 'keys' },
        row(['j'], 'Next item'), row(['k'], 'Previous item'),
        row(['↵'], 'Open the selected item'), row(['esc'], 'Clear the selection'),
        row([mod, 'K'], 'Search and commands'), row(['/'], 'Search'),
        row(['g', 'i'], 'Inbox'), row(['g', 's'], 'Sessions'),
        row(['g', 't'], 'Tasks'), row(['g', 'r'], 'Rules')),
      h('h3', { class: 'keys-group' }, 'Agents'),
      h('dl', { class: 'keys' },
        row(['r'], 'Reply to a session'), row(['n'], 'New task'),
        row(['?'], 'This help'))));
  d.addEventListener('click', (e) => { if (e.target === d) d.close(); });
  d.addEventListener('close', () => d.remove());
  add(document.body, d);
  d.showModal();
}

export function initKeys() {
  setPaletteOpener(openPalette);
  document.addEventListener('keydown', onKey);
  // A click elsewhere ends keyboard selection.
  document.addEventListener('pointerdown', (e) => {
    if (selKey && !(e.target.closest && e.target.closest('.is-selected'))) select(null, false);
  });
}
