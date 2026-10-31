// Session page (F2, F3; workstream I): live state and usage, the
// session's open requests, "catch me up", reply and interrupt, the
// activity feed grouped by turn, and the files the agent changed. When
// the daemon can't read the agent's transcript it falls back to the
// hook-event timeline.
//
// Also exports the small helpers the other 0.2 views share (signed
// actions, number formatting, extra icons), so they need no new modules.

import { h, icon, inkStamp, agentName, repoName, stateLabel, agoLong, clock, relPath, fill, add, toMs } from '../dom.js';
import { store, api, onEvent } from '../api.js';
import { requestCard } from '../card.js';
import { renderMarkdown } from '../markdown.js';
import { askTitle, plainStep, commandLabel, fillProof } from './today.js';
import { link, toast } from '../ui.js';
import { avatar, section, sessionLine } from './rows.js';

// ---------- Shared helpers (session, terminal, tasks, insights) ----------

const SVGNS = 'http://www.w3.org/2000/svg';

// Extra 24×24 stroke icons in the style of dom.js.
const ICONS = {
  send: ['M4 12 20 4l-4 16-4-7Z', 'M12 13l8-9'],
  bot: ['M12 4v3', 'M6 8h12a2 2 0 0 1 2 2v7a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2v-7a2 2 0 0 1 2-2Z', 'M9 13h.01', 'M15 13h.01', 'M10 16.5h4'],
  plus: ['M12 5v14', 'M5 12h14'],
  minus: ['M5 12h14'],
  trash: ['M4 7h16', 'M10 11v6', 'M14 11v6', 'M6 7l1 12a2 2 0 0 0 2 2h6a2 2 0 0 0 2-2l1-12', 'M9 7V4h6v3'],
  down: ['m6 9 6 6 6-6'],
  branch: ['M6 3v12', 'M18 9a3 3 0 1 0 0-6 3 3 0 0 0 0 6Z', 'M6 21a3 3 0 1 0 0-6 3 3 0 0 0 0 6Z', 'M18 9a9 9 0 0 1-9 9'],
  pr: ['M6 3a3 3 0 1 0 0 6 3 3 0 0 0 0-6Z', 'M6 9v12', 'M18 21a3 3 0 1 0 0-6 3 3 0 0 0 0 6Z', 'M18 15V8a2 2 0 0 0-2-2h-4', 'm14 3-3 3 3 3'],
  coins: ['M9 14a6 6 0 1 0 0-12 6 6 0 0 0 0 12Z', 'M18.1 10.4a6 6 0 1 1-7.7 7.7', 'M7 6h2.5v4'],
  keyboard: ['M3 6h18a1 1 0 0 1 1 1v10a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1Z', 'M6 10h.01', 'M10 10h.01', 'M14 10h.01', 'M18 10h.01', 'M8 14h8'],
  refresh: ['M20 11a8 8 0 0 0-14.9-3.9L3 9', 'M3 4v5h5', 'M4 13a8 8 0 0 0 14.9 3.9L21 15', 'M21 20v-5h-5'],
  eye: ['M2 12s3.6-7 10-7 10 7 10 7-3.6 7-10 7S2 12 2 12Z', 'M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6Z'],
  layers: ['m12 3 9 5-9 5-9-5Z', 'm3 13 9 5 9-5'],
  info: ['M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18Z', 'M12 11v5', 'M12 8h.01'],
  sparkles: ['M10 3 8.6 7.6 4 9l4.6 1.4L10 15l1.4-4.6L16 9l-4.6-1.4Z', 'M18 14l-.8 2.2L15 17l2.2.8L18 20l.8-2.2L21 17l-2.2-.8Z'],
  history: ['M3 12a9 9 0 1 0 3-6.7L3 8', 'M3 3v5h5', 'M12 7v5l3 2'],
  ext: ['M14 4h6v6', 'M20 4 10 14', 'M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5'],
  up: ['m6 15 6-6 6 6'],
  left: ['m15 18-6-6 6-6'],
  right: ['m9 18 6-6-6-6'],
  enter: ['M20 5v7a3 3 0 0 1-3 3H5', 'm9 11-4 4 4 4'],
};

/** ico('send', 'sm') → <svg class="ic sm">, like dom.js icon() for the extra set. */
export function ico(name, size = '', extra = '') {
  if (!ICONS[name]) return icon(name, size, extra);
  const svg = document.createElementNS(SVGNS, 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('class', ['ic', size, extra].filter(Boolean).join(' '));
  svg.setAttribute('aria-hidden', 'true');
  svg.setAttribute('focusable', 'false');
  for (const d of ICONS[name]) {
    const p = document.createElementNS(SVGNS, 'path');
    p.setAttribute('d', d);
    add(svg, p);
  }
  return svg;
}

/** A spinner that screen readers skip (the surrounding text says what's happening). */
export const spinner = (cls = '') => h('span', { class: 'v2-spin ' + cls, 'aria-hidden': 'true' });

/**
 * Signs and posts a 0.2 action (keystore.js postSigned). Resolves to
 * {ok, status, data}; never rejects. A revoked device goes back to pairing.
 */
export async function postAction(path, action, target, payload) {
  if (!self.PilothouseKeys || !self.PilothouseKeys.postSigned) {
    return { ok: false, status: 0, data: { code: 'no_key', error: 'This browser can’t sign actions. Reload the app.' } };
  }
  const res = await self.PilothouseKeys.postSigned(path, action, target, payload);
  if (res.status === 401) location.assign('/pair');
  return res;
}

/** The daemon's errors are lower-case fragments; show them as sentences. */
export function sentence(s) {
  s = String(s || '').trim();
  return s ? s[0].toUpperCase() + s.slice(1) + (/[.!?…]$/.test(s) ? '' : '.') : s;
}

/** A sentence for a failed signed action. */
export function actionError(res, unsupported = 'Your computer can’t do that yet.') {
  const d = (res && res.data) || {};
  if (d.code === 'stale_signature') return 'This device’s clock is off — fix the time and try again.';
  if (d.code === 'replayed') return 'That was already sent. Try again.';
  if (res && res.status === 501) return d.error && d.error !== 'not supported here' ? sentence(d.error) : unsupported;
  if (res && res.status === 0) return sentence(d.error) || 'Can’t reach your computer.';
  return sentence(d.error) || 'Couldn’t send — try again.';
}

const compact = new Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 });
const whole = new Intl.NumberFormat();

/** 842 · 4.2K · 12.4M */
export function fmtTokens(n) {
  n = Number(n) || 0;
  return n < 1000 ? whole.format(n) : compact.format(n);
}

/** $0 · <$0.01 · $1.24 · $48.20 · $312 · $1.2K */
export function fmtUSD(x) {
  x = Number(x) || 0;
  if (x === 0) return '$0';
  if (x < 0.01) return '<$0.01';
  if (x < 100) return '$' + x.toFixed(2);
  if (x < 1000) return '$' + Math.round(x);
  return '$' + compact.format(x);
}

export function totalTokens(u) {
  if (!u) return 0;
  return (u.input_tokens || 0) + (u.output_tokens || 0) + (u.cache_read_tokens || 0) + (u.cache_write_tokens || 0) + (u.reasoning_tokens || 0);
}

/** "38 s", "4.5 min", "2.1 h". */
export function fmtDuration(secs) {
  secs = Number(secs) || 0;
  if (secs < 90) return Math.round(secs) + ' s';
  if (secs < 5400) return (secs / 60).toFixed(secs < 600 ? 1 : 0).replace(/\.0$/, '') + ' min';
  return (secs / 3600).toFixed(1).replace(/\.0$/, '') + ' h';
}

/** Who did something, from a Decision.By-style string. */
export function byLabel(by) {
  const me = store.me && store.me.device && 'device:' + store.me.device.id;
  if (!by) return '';
  if (by === me) return 'this device';
  if (by.startsWith('device:')) return 'another device';
  if (by === 'cli' || by === 'tui') return 'the computer';
  if (by.startsWith('telegram')) return 'Telegram';
  return by;
}

// ---------- Last visit (catch me up) ----------

const VISITS = 'pilothouse.visits';
const MAX_VISITS = 200;

function readVisits() {
  try { return JSON.parse(localStorage.getItem(VISITS) || '{}') || {}; } catch { return {}; }
}

/** When this device last looked at a session (ISO string), or ''. */
export function lastVisit(id) {
  const v = readVisits()[id];
  return typeof v === 'string' ? v : '';
}

export function markVisited(id, at = new Date()) {
  const v = readVisits();
  v[id] = at.toISOString();
  const keys = Object.keys(v);
  if (keys.length > MAX_VISITS) {
    keys.sort((a, b) => toMs(v[a]) - toMs(v[b]));
    for (const k of keys.slice(0, keys.length - MAX_VISITS)) delete v[k];
  }
  try { localStorage.setItem(VISITS, JSON.stringify(v)); } catch { /* private mode: no catch-up memory */ }
}

// ---------- Usage chip ----------

/**
 * The tokens/cost chip with a breakdown that opens on tap. opts.open and
 * opts.onToggle(open) let a view keep the panel open across re-renders.
 */
export function usageChip(u, opts = {}) {
  if (!u) return null;
  const tokens = totalTokens(u);
  const panelId = 'usage-' + Math.random().toString(36).slice(2, 8);
  const rows = [
    ['Input', u.input_tokens], ['Output', u.output_tokens],
    ['Cache write', u.cache_write_tokens], ['Cache read', u.cache_read_tokens],
    ['Reasoning', u.reasoning_tokens],
  ].filter(([, v]) => v);
  const panel = h('div', { class: 'usage-panel', id: panelId, hidden: !opts.open },
    h('dl', null, rows.flatMap(([k, v]) => [h('dt', null, k), h('dd', null, whole.format(v))]),
      u.model ? [h('dt', null, 'Model'), h('dd', { class: 'mono' }, u.model)] : null,
      u.turns ? [h('dt', null, 'Turns'), h('dd', null, String(u.turns))] : null),
    h('p', null, u.cost_usd ? 'Cost is estimated from list prices.' : 'No cost estimate for this model.'));
  const chip = h('button', { class: 'usage-chip', type: 'button', 'aria-expanded': String(!!opts.open), 'aria-controls': panelId },
    ico('coins', 'sm'),
    h('span', null, h('b', null, fmtTokens(tokens)), ' tokens'),
    u.cost_usd ? h('span', { class: 'usage-cost' }, '≈ ' + fmtUSD(u.cost_usd)) : null);
  chip.addEventListener('click', () => {
    const open = chip.getAttribute('aria-expanded') !== 'true';
    chip.setAttribute('aria-expanded', String(open));
    panel.hidden = !open;
    if (opts.onToggle) opts.onToggle(open);
  });
  return h('div', { class: 'usage' }, chip, panel);
}

// ---------- Session page ----------

const TOOL_ICONS = { shell: 'terminal', edit: 'pencil', read: 'file', web: 'globe', mcp: 'plug', other: 'spark' };
const VIA_NOTE = {
  message: 'Claude Code may hold this until you allow it in its terminal. Start Claude with “pilothouse run claude” to avoid that.',
  terminal: 'Typed into the agent’s terminal, followed by Enter.',
  plugin: 'Sent through the agent’s Pilothouse plugin.',
};
const ACTIVE = new Set(['working', 'starting']);
const FIRST_LIMIT = 200;
const MAX_LIMIT = 1000;
const STEPS_SHOWN = 8; // a long turn shows its last steps first

/** Session detail: header, open requests, catch-up, reply, activity, files. */
export function sessionView(id) {
  const enc = encodeURIComponent(id);
  const root = h('div', { class: 'view sess rep' });
  const headEl = h('div', { class: 'rhead' });
  const needsEl = h('div');
  const catchEl = h('div', { class: 'rbody' });
  const replyEl = h('div', { class: 'rcompose' });
  const feedEl = h('section', { class: 'rsec', 'aria-labelledby': 'act-h' });
  const filesEl = h('div');
  const detailsEl = h('div');
  add(root, headEl, needsEl, catchEl, feedEl, filesEl, detailsEl, replyEl);
  let receipt = null;

  const since = lastVisit(id);
  let loadErr = null;
  let destroyed = false;

  // Activity state.
  let actState = 'loading'; // loading | ok | unavailable | unsupported | error
  let actNote = '';
  let usage = null;
  let files = null;
  let catchUp = null;
  let catchDismissed = false;
  let cursor = '';
  let limit = FIRST_LIMIT;
  let more = false; // the first page was full: older items exist
  const items = new Map(); // id → item, in feed order
  const sent = []; // replies sent from here, until the feed shows them
  const openSteps = new Set();
  const openTurns = new Set();
  const fullTurns = new Set();
  let errors = 0;
  let timer = 0;
  let refetch = 0;
  let inflight = null;
  let again = false; // false | 'poll' | 'full'
  let feedKey = '';
  const openClamps = new Set();

  // Fallback timeline (hook events).
  let events = null;
  let eventsErr = null;

  // ---- Header ----
  let interruptArmed = 0;
  let headKey = '';
  let usageOpen = false;
  let moreOpen = false;
  function renderHead() {
    const s = store.sessions.get(id);
    const key = JSON.stringify([s || null, usage, receipt && receipt.since, interruptArmed > Date.now(), !!loadErr]);
    if (key === headKey) return;
    headKey = key;
    if (!s) {
      fill(headEl, loadErr
        ? h('div', { class: 'empty' }, h('h1', { class: 'page-title' }, 'Session not found'), h('p', null, 'It may have ended more than a day ago.'))
        : h('div', { class: 'skel-block rk-skel', 'aria-busy': 'true' }, h('span', { class: 'sr-only' }, 'Loading…')));
      return;
    }
    const u = usage || s.usage;
    const took = receipt && receipt.since && toMs(receipt.since) ? Math.max(0, (toMs(s.updated_at) - toMs(receipt.since)) / 1000) : 0;
    const meta = [agentName(s.agent), statusWords(s), took >= 60 ? fmtDuration(took) : null, u && u.cost_usd >= 0.01 ? '≈ ' + fmtUSD(u.cost_usd) : null].filter(Boolean);
    fill(headEl,
      h('h1', { class: 'page-title rk' }, h('span', { class: 'ag ' + s.agent, 'aria-hidden': 'true' }), h('b', null, repoName(s)),
        s.branch ? h('span', { class: 'br' }, ' · ' + s.branch) : null),
      h('p', { class: 'rmeta' }, meta.join(' · ')));
    fill(detailsEl, h('details', { class: 'rdetails', open: moreOpen, ontoggle: (e) => { moreOpen = e.currentTarget.open; } },
      h('summary', null, 'Session details', ico('down', 'sm')),
      h('dl', { class: 'kv' },
        h('dt', null, 'Folder'), h('dd', { class: 'mono' }, s.cwd),
        s.branch ? [h('dt', null, 'Branch'), h('dd', null, s.branch)] : null,
        h('dt', null, 'Mode'), h('dd', null, s.mode === 'wrapped' ? 'Started with pilothouse run' : 'Observed through hooks'),
        h('dt', null, 'Started'), h('dd', null, `${clock(s.started_at)} · ${agoLong(s.started_at)}`)),
      u ? h('div', { class: 'rusage' }, usageChip(u, { open: usageOpen, onToggle: (o) => { usageOpen = o; } })) : null,
      s.task_id ? link(`/t/${s.task_id}`, { class: 'btn secondary sess-act' }, ico('branch', 'sm'), 'Open its task') : null));
    renderNeeds(true);
  }

  async function onInterrupt() {
    if (interruptArmed <= Date.now()) {
      interruptArmed = Date.now() + 3500;
      renderNeeds(true);
      const b = document.getElementById('interrupt-btn');
      if (b) b.focus({ preventScroll: true });
      setTimeout(() => { if (!destroyed) renderNeeds(true); }, 3600);
      return;
    }
    interruptArmed = 0;
    renderNeeds(true);
    const res = await postAction(`/api/sessions/${enc}/reply`, 'reply', id, { interrupt: true });
    if (res.ok) {
      toast('Interrupted', 'ok');
      kick();
    } else {
      toast(actionError(res, 'This session can’t be interrupted from here.'), 'bad');
    }
  }

  // ---- What it needs or is doing now: a decision, the terminal, or the live step ----
  let needsKey = '';
  const proofEl = h('div', { class: 'thisturn' });
  function renderNeeds(force = false) {
    const s = store.sessions.get(id);
    const open = [...store.requests.values()]
      .filter((r) => r.session_id === id && r.status === 'open')
      .sort((a, b) => toMs(a.expires_at) - toMs(b.expires_at));
    const perm = open.find((r) => r.checkpoint !== 'question' && r.checkpoint !== 'plan');
    if (perm) requestCard(perm, { slot: 'session', variant: 'ledger', proof: proofEl, animate: false }); // refresh in place
    if (receipt) fillProof(proofEl, receipt); else proofEl.hidden = true;
    if (receipt) proofEl.hidden = false;
    const key = [open.map((r) => r.id).join(','), s && s.state, s && s.state_detail, s && s.block_kind, interruptArmed > Date.now()].join('|');
    if (key === needsKey && !force) return; // re-filling would blur a half-typed deny reason
    if (key === needsKey && perm) return;
    needsKey = key;
    if (!s) { fill(needsEl); return; }
    const caps = s.caps || {};
    const live = s.state !== 'exited';
    const termBtn = caps.watch && live ? link(`/s/${s.id}/terminal`, { class: 'btn secondary' }, icon('terminal', 'sm'), 'Terminal') : null;
    const armedN = interruptArmed > Date.now();
    const intrBtn = caps.interrupt && live ? h('button', {
      class: 'btn ' + (armedN ? 'danger armed' : 'ghost deny'), type: 'button', id: 'interrupt-btn', 'aria-live': 'polite', onclick: onInterrupt,
    }, icon('stop', 'sm'), armedN ? 'Tap again to stop' : 'Interrupt') : null;
    if (open.length) {
      fill(needsEl, h('section', { class: 'rnow need', 'aria-labelledby': 'now-h' },
        h('h2', { class: 'lbl need', id: 'now-h' }, 'Needs you', h('b', null, String(open.length))),
        open.map((r) => (r === perm
          ? h('div', { class: 'rdec' }, h('p', { class: 'rask' }, askTitle(r)), requestCard(r, { slot: 'session', variant: 'ledger', proof: proofEl, animate: false }))
          : link('/r/' + r.id, { class: 'rgo' }, h('span', null, askTitle(r)), icon('chev', 'sm')))),
        termBtn || intrBtn ? h('div', { class: 'racts pad' }, termBtn, intrBtn) : null));
      return;
    }
    if (s.state === 'blocked') {
      const words = s.block_kind === 'auth' ? `${agentName(s.agent)} needs you to sign in, in its terminal` : `${agentName(s.agent)} is asking something in its terminal`;
      fill(needsEl, h('section', { class: 'rnow desk', 'aria-labelledby': 'now-h' },
        h('span', { class: 'deskic', 'aria-hidden': 'true' }, icon('monitor')),
        h('h2', { id: 'now-h' }, words),
        s.state_detail ? h('div', { class: 'said' }, h('small', null, agentName(s.agent) + ' says'), s.state_detail) : null,
        h('p', null, 'Pilothouse can see this prompt but can’t answer it. Answer it on your computer and this session carries on.'),
        termBtn ? h('div', { class: 'racts' }, termBtn) : null));
      return;
    }
    if (ACTIVE.has(s.state) || s.state === 'unknown') {
      const st = s.state === 'unknown' ? { label: 'Quiet for ' + agoLong(s.updated_at).replace(/ ago$/, ''), raw: s.state_detail || '' } : plainStep(s.state_detail);
      const intr = intrBtn;
      fill(needsEl, h('section', { class: 'rnow live', 'aria-labelledby': 'now-h' },
        h('h2', { class: 'lbl work', id: 'now-h' }, 'Right now'),
        h('p', { class: 'livestep' }, h('span', { class: 'spin', 'aria-hidden': 'true' }), st.label),
        st.raw ? h('p', { class: 'raw' }, st.raw) : null,
        h('p', { class: 'elapsed' }, 'on this step for ', h('time', { 'data-since': s.updated_at }, elapsedText(s.updated_at))),
        termBtn || intr ? h('div', { class: 'racts' }, termBtn, intr) : null));
      return;
    }
    fill(needsEl);
  }

  // ---- The report: what you asked, what it says, proof, changes ----
  let reportKey = '';
  const openChecks = new Set();
  let saysOpen = false;
  function renderCatchUp() { renderReport(); }
  function renderReport() {
    const s = store.sessions.get(id);
    if (actState !== 'ok') { fill(catchEl); return; }
    const all = [...items.values()];
    const turns = groupTurns(all);
    const t = turns[turns.length - 1];
    if (!t) { fill(catchEl); reportKey = ''; return; }
    // Rebuild only when what the report shows changes, not on every poll:
    // re-filling replays the stamps and fade-ins (looks like flicker).
    const askedNow = t.prompts.length ? t.prompts[t.prompts.length - 1].text : '';
    const saidNow = ([...t.steps].reverse().find((it) => it.kind === 'text' && !it.sidechain && it.text) || {}).text || '';
    // Only what the proof shows: check results, not counts that tick with every step.
    const ck = (c) => c && [c.name, c.status, c.fresh, c.count, c.failed_before].join(':');
    const rcSig = receipt ? [ck(receipt.tests), ck(receipt.lint), ck(receipt.build), receipt.edits > 0,
      receipt.edits ? '' : receipt.searches + '/' + receipt.reads].join('|') : '';
    const sig = [t.key, askedNow, saidNow, rcSig, saysOpen, [...openChecks].join(',')].join('|');
    if (sig === reportKey) return;
    if (reportKey) catchEl.classList.add('settled'); // later rebuilds don't animate
    reportKey = sig;
    const parts = [];
    const asked = t.prompts.length ? t.prompts[t.prompts.length - 1] : null;
    if (asked && asked.text) {
      parts.push(h('h2', { class: 'lbl' }, asked.source === 'pilothouse' ? 'You asked · from Pilothouse' : asked.source === 'peer' ? 'Another agent asked' : 'You asked'),
        h('blockquote', { class: 'goalq' }, asked.text));
    }
    const said = [...t.steps].reverse().find((it) => it.kind === 'text' && !it.sidechain && it.text);
    if (said) {
      const md = renderMarkdown(said.text, { baseLevel: 3 });
      const long = said.text.length > 420 || said.text.split('\n').length > 8;
      parts.push(h('h2', { class: 'lbl' }, `${agentName(s ? s.agent : '')} says`),
        h('div', { class: 'md says' + (long && !saysOpen ? ' clamp' : '') }, md),
        long ? h('button', { class: 'readall', type: 'button', 'aria-expanded': String(saysOpen), onclick: () => { saysOpen = !saysOpen; reportKey = ''; renderReport(); } }, saysOpen ? 'Show less' : 'Read all') : null);
    }
    const rc = receipt;
    if (rc && (rc.tests || rc.lint || rc.build || rc.edits)) {
      const checks = [['tests', 'Tests'], ['lint', 'Lint'], ['build', 'Build']].filter(([k]) => rc[k]);
      parts.push(h('h2', { class: 'lbl' }, 'Proof'));
      const stamps = checks.filter(([k]) => rc[k].status !== 'running').map(([k, name]) => stamp(k, name, rc[k]));
      if (stamps.length) parts.push(h('div', { class: 'stamps' }, stamps, h('p', { class: 'stampnote' }, 'Checked by commands that ran in this turn, not by the agent’s word.')));
      if (checks.length) parts.push(h('div', { class: 'checks' }, checks.map(([k, name]) => checkRow(k, name, rc[k], t))));
      else parts.push(h('p', { class: 'rnone' }, icon('flask', 'sm'), 'No tests, lint or build ran this turn.'));
    } else if (rc && (rc.searches || rc.reads) && !rc.edits) {
      parts.push(h('h2', { class: 'lbl' }, 'Proof'), h('p', { class: 'rnone ro' }, icon('search', 'sm'), `Read only: ${rc.searches} ${rc.searches === 1 ? 'search' : 'searches'}, ${rc.reads} ${rc.reads === 1 ? 'read' : 'reads'}, nothing changed.`));
    }
    fill(catchEl, parts);
    renderFiles();
  }

  function stamp(kind, name, c) {
    const ok = c.status === 'passed' && c.fresh;
    const mid = c.status === 'failed' ? '✗' : !c.fresh ? '!' : kind === 'tests' ? (c.count ? String(c.count) : 'OK') : 'OK';
    const top = c.status === 'failed' ? `${name.toUpperCase()} FAILED` : !c.fresh ? 'STALE' : kind === 'tests' ? 'TESTS PASSED' : name.toUpperCase();
    return inkStamp(top, mid, c.name.toUpperCase(), ok ? 'ok' : c.status === 'failed' ? 'bad' : 'stale', mid.length > 3 ? 22 : 27);
  }

  function checkRow(kind, name, c, t) {
    const key = kind;
    const open = openChecks.has(key);
    const run = [...t.steps].reverse().find((it) => it.kind === 'tool' && (it.summary || '').includes(c.command.slice(0, 40)));
    const title = c.status === 'failed' ? `${name} failed` : c.status === 'running' ? `${name} running` : !c.fresh ? `${name} ran before the last edit` : `${name} passed${kind === 'tests' && c.count ? ' · ' + c.count : ''}`;
    const cls = c.status === 'failed' ? 'bad' : c.status === 'running' ? 'run' : !c.fresh ? 'warn' : 'ok';
    return h('button', { class: 'rchk' + (open ? ' open' : ''), type: 'button', 'aria-expanded': String(open), onclick: () => { if (open) openChecks.delete(key); else openChecks.add(key); reportKey = ''; renderReport(); } },
      h('span', { class: 'ch' }, h('span', { class: 'st ' + cls, 'aria-hidden': 'true' }, icon(cls === 'ok' ? 'check' : cls === 'bad' ? 'x' : cls === 'run' ? 'hourglass' : 'alert', 'sm')),
        h('span', { class: 'ct' }, title), h('time', { datetime: c.at }, agoLong(c.at)), icon('chevDown', 'sm', 'cv')),
      h('span', { class: 'raw' }, c.command),
      c.failed_before ? h('span', { class: 'note' }, `Failed ${c.failed_before === 1 ? 'once' : c.failed_before + ' times'} first` + failReason(t, c)) : null,
      open && run && run.result ? h('pre', { class: 'out' }, run.result) : null);
  }

  // ---- Reply ----
  const replyText = h('textarea', {
    // data-shortcut: the `r` key (palette.js) focuses this box.
    class: 'input reply-input', id: 'reply-text', 'data-shortcut': 'reply', rows: 1, maxlength: 16384, autocomplete: 'off',
    'aria-describedby': 'reply-note', enterkeyhint: 'send',
  });
  const replyLabel = h('label', { class: 'sr-only', for: 'reply-text' }, 'Reply');
  const stopFirst = h('input', { type: 'checkbox', id: 'reply-stop' });
  const stopRow = h('label', { class: 'reply-stop', for: 'reply-stop' }, stopFirst, 'Stop the current step first');
  const sendBtn = h('button', { class: 'btn primary reply-send', type: 'submit', 'aria-label': 'Send reply' }, ico('send', 'sm'), h('span', null, 'Send'));
  const replyNote = h('p', { class: 'reply-note', id: 'reply-note' });
  const replyForm = h('form', { class: 'card reply', novalidate: true },
    replyLabel, h('div', { class: 'reply-row' }, replyText, sendBtn), h('div', { class: 'reply-foot' }, stopRow, replyNote));
  let sending = false;
  replyForm.addEventListener('submit', (e) => { e.preventDefault(); sendReply(); });
  replyText.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); sendReply(); }
  });
  // Grow with the text (CSSOM height, allowed by the CSP), up to 40 % of the screen.
  const grow = () => {
    replyText.style.height = 'auto';
    replyText.style.height = Math.min(replyText.scrollHeight + 2, Math.round(innerHeight * 0.4)) + 'px';
  };
  replyText.addEventListener('input', () => { sendBtn.disabled = sending || !replyText.value.trim(); grow(); });

  function renderReply() {
    const s = store.sessions.get(id);
    const caps = (s && s.caps) || {};
    if (!s || !caps.reply || s.state === 'exited') {
      fill(replyEl, s && !caps.reply && s.state !== 'exited' && s.mode === 'observed' && s.agent === 'codex'
        ? h('p', { class: 'reply-none' }, ico('info', 'sm'), h('span', null, 'To reply from here, start Codex with ', h('code', null, 'pilothouse run codex'), '.'))
        : null);
      return;
    }
    replyText.placeholder = `Message ${agentName(s.agent)}…`;
    replyNote.textContent = VIA_NOTE[caps.reply_via] || '';
    stopRow.hidden = !caps.interrupt || !ACTIVE.has(s.state);
    sendBtn.disabled = sending || !replyText.value.trim();
    if (!replyForm.isConnected) fill(replyEl, replyForm);
  }

  async function sendReply() {
    const text = replyText.value.replace(/\s+$/, '');
    if (!text.trim() || sending) return;
    if (new TextEncoder().encode(text).length > 16384) {
      toast('That message is too long (16 KB at most).', 'bad');
      return;
    }
    const payload = { text };
    if (!stopRow.hidden && stopFirst.checked) payload.interrupt = true;
    sending = true;
    sendBtn.disabled = true;
    fill(sendBtn, spinner(), h('span', null, 'Sending'));
    const res = await postAction(`/api/sessions/${enc}/reply`, 'reply', id, payload);
    sending = false;
    fill(sendBtn, ico('send', 'sm'), h('span', null, 'Send'));
    if (res.ok) {
      replyText.value = '';
      grow();
      stopFirst.checked = false;
      sent.push({ text: text.trim(), at: new Date().toISOString(), note: (res.data && res.data.note) || '' });
      toast('Sent', 'ok');
      renderFeed();
      kick();
    } else {
      toast(actionError(res, 'This session can’t receive replies.'), 'bad');
    }
    renderReply();
  }

  // ---- Activity: loading ----
  function apply(a, full) {
    if (full) items.clear();
    for (const it of a.items || []) {
      if (!it || !it.id) continue;
      items.set(it.id, it);
    }
    if (full) more = (a.items || []).length >= limit && limit < MAX_LIMIT;
    if (a.cursor) cursor = a.cursor;
    if (a.usage) usage = a.usage;
    if (a.files) files = a.files;
    if (a.receipt) receipt = a.receipt;
    if (full && a.catch_up && !catchUp) catchUp = a.catch_up;
    actState = a.available === false ? 'unavailable' : 'ok';
    actNote = a.note || '';
    // Drop local "sent" markers the feed now shows.
    for (let i = sent.length - 1; i >= 0; i--) {
      const s = sent[i];
      const seen = [...items.values()].some((it) => it.kind === 'prompt' && toMs(it.at) >= toMs(s.at) - 60000 && (it.text || '').trim() === s.text);
      if (seen || Date.now() - toMs(s.at) > 120000) sent.splice(i, 1);
    }
  }

  function interval() {
    const s = store.sessions.get(id);
    if (errors) return Math.min(30000, 3000 * 2 ** errors);
    if (actState === 'unavailable') return 30000;
    if (s && (ACTIVE.has(s.state) || s.state === 'blocked')) return 3000;
    return 10000;
  }

  function schedule() {
    clearTimeout(timer);
    if (destroyed || document.hidden || actState === 'unsupported') return;
    timer = setTimeout(poll, interval());
  }

  /** Fetches activity (only newer items once there's a cursor). */
  async function poll(full = false) {
    if (destroyed) return;
    if (inflight) { again = full || again === 'full' ? 'full' : 'poll'; return inflight; }
    clearTimeout(timer);
    const q = new URLSearchParams();
    if (cursor && !full) {
      q.set('cursor', cursor);
    } else {
      q.set('limit', String(limit));
      if (since) q.set('since', since);
    }
    const first = actState === 'loading';
    inflight = (async () => {
      try {
        const a = await api('GET', `/api/sessions/${enc}/activity?${q}`);
        if (destroyed) return;
        apply(a, full || !cursor);
        errors = 0;
      } catch (e) {
        if (destroyed) return;
        if (e.status === 501) {
          actState = 'unsupported';
        } else if (e.status === 404) {
          actState = 'error';
          loadErr = e;
          renderHead();
        } else {
          errors++;
          if (actState === 'loading') actState = 'error';
        }
      }
      if (actState !== 'ok' && !events && !eventsErr) loadEvents();
      renderFeed();
      renderFiles();
      if (first) renderCatchUp();
      renderHead();
    })();
    await inflight;
    inflight = null;
    if (again) {
      const f = again === 'full';
      again = false;
      return poll(f);
    }
    schedule();
  }

  /** Re-fetch soon (coalesces bursts of events). */
  function kick(delay = 250) {
    clearTimeout(refetch);
    refetch = setTimeout(() => {
      if (actState === 'unsupported' || actState === 'unavailable' || actState === 'error') loadEvents();
      if (actState !== 'unsupported') poll();
    }, delay);
  }

  async function loadEvents() {
    try {
      events = await api('GET', `/api/sessions/${enc}/events?limit=200`);
      eventsErr = null;
    } catch (e) {
      eventsErr = e;
    }
    if (!destroyed) renderFeed();
  }

  function loadEarlier() {
    limit = Math.min(MAX_LIMIT, limit * 3);
    poll(true);
  }

  // ---- How it got there: each turn as a short voyage in plain words ----
  function renderFeed() {
    const s0 = store.sessions.get(id);
    const sig = [actState, more, sent.length, s0 && s0.state, s0 && s0.state_detail, events ? events.length : -1, !!eventsErr, [...openTurns].join(','), [...openSteps].join(','),
      ...[...items.values()].map((it) => `${it.id}:${it.pending ? 1 : 0}:${it.is_error ? 1 : 0}:${(it.result || '').length}:${(it.text || '').length}`),
    ].join('|');
    if (sig === feedKey) return;
    feedKey = sig;
    renderReport();
    const head = h('h2', { class: 'lbl', id: 'act-h' }, 'How it got there');
    if (actState === 'loading') {
      fill(feedEl, head, h('div', { class: 'act-skel', 'aria-busy': 'true' },
        h('span', { class: 'sr-only' }, 'Loading activity…'),
        [0, 1, 2, 3].map(() => h('div', { class: 'skel-row' }, h('i'), h('b')))));
      return;
    }
    if (actState !== 'ok') {
      fill(feedEl, h('h2', { class: 'lbl', id: 'act-h' }, 'Timeline'), fallbackNotice(), fallbackTimeline());
      return;
    }
    const turns = groupTurns([...items.values()]);
    const s = store.sessions.get(id);
    const parts = [head];
    if (sent.length) parts.push(sentTurn());
    if (!turns.length && !sent.length) {
      parts.push(h('div', { class: 'act-empty' }, ico('history', 'lg'),
        h('p', null, s && ACTIVE.has(s.state) ? 'The agent is starting — its first steps show up here.' : 'Nothing has happened in this session yet.')));
    }
    const latest = turns[turns.length - 1];
    if (latest) parts.push(h('div', { class: 'act-feed voyage-wrap' }, voyage(latest, true, s)));
    const earlier = turns.slice(0, -1).reverse();
    if (earlier.length) {
      parts.push(h('h2', { class: 'lbl' }, 'Earlier', h('b', null, String(earlier.length))));
      parts.push(h('div', { class: 'earlier' }, earlier.map((t) => earlierTurn(t, s))));
    }
    if (more) {
      parts.push(h('button', { class: 'btn ghost block act-more', type: 'button', onclick: loadEarlier }, ico('history', 'sm'), 'Show earlier activity'));
    } else if (limit >= MAX_LIMIT && items.size >= MAX_LIMIT) {
      parts.push(h('p', { class: 'quiet' }, 'Older activity isn’t shown here.'));
    }
    if (feedEl.childNodes.length) feedEl.classList.add('settled');
    fill(feedEl, parts);
  }

  function earlierTurn(t, s) {
    const det = h('details', { class: 'eturn' });
    if (openTurns.has(t.key)) det.open = true;
    const p = t.prompts[0];
    const stats = turnStats(t);
    add(det, h('summary', null,
      h('span', { class: 'ep' }, p ? (p.text || 'Prompt') : t.n ? `Turn ${t.n}` : 'Session start'),
      h('span', { class: 'em' }, [stats.tools ? `${stats.tools} ${stats.tools === 1 ? 'step' : 'steps'}` : null, stats.errors ? `${stats.errors} failed` : null, t.at ? clock(t.at) : null].filter(Boolean).join(' · ')),
      ico('down', 'sm', 'cv')));
    const body = h('div', { class: 'ebody' });
    add(det, body);
    if (det.open) fill(body, voyage(t, false, s));
    det.addEventListener('toggle', () => { if (det.open) { openTurns.add(t.key); fill(body, voyage(t, false, s)); } else openTurns.delete(t.key); });
    return det;
  }

  /** A turn's steps as voyage nodes: reads and searches folded, checks named, bumps kept. */
  function voyage(t, latest, s) {
    const nodes = [];
    const steps = groupSteps(t.steps);
    const lastText = [...t.steps].reverse().find((it) => it.kind === 'text' && !it.sidechain);
    let reads = [];
    const flushReads = () => {
      if (!reads.length) return;
      const searches = reads.filter(isSearch).length;
      const nread = reads.length - searches;
      const label = searches && nread ? `Explored the code · ${searches} ${searches === 1 ? 'search' : 'searches'}, ${nread} ${nread === 1 ? 'read' : 'reads'}`
        : searches ? `Searched the code${searches > 1 ? ' · ' + searches + ' times' : ''}` : `Read ${nread} ${nread === 1 ? 'file' : 'files'}`;
      const group = reads;
      const key = 'r:' + group[0].id;
      nodes.push(node('read', searches ? 'search' : 'file', label, null, group.length > 1 ? key : null,
        group.length > 1 ? group.map((it) => h('span', { class: 'raw' }, rawOf(it))) : [h('span', { class: 'raw' }, rawOf(group[0]))]));
      reads = [];
    };
    for (const st of steps) {
      if (st.group) {
        flushReads();
        const n = st.items.filter((x) => x.kind === 'tool').length;
        const running = st.items.some((x) => x.pending);
        const key = 'g:' + (st.head ? st.head.id : st.items[0].id);
        const lines = st.items.filter((x) => x.kind === 'tool' || x.kind === 'text').map((x) => h('span', { class: 'raw' }, x.kind === 'text' ? oneLine(x.text, 140) : rawOf(x)));
        if (st.tail) lines.push(h('span', { class: 'sub' }, st.tail.summary || st.tail.text || 'Finished'));
        nodes.push(node('sub' + (running ? ' now' : ''), 'layers', 'Used a helper agent' + (st.head && (st.head.summary || st.head.text) ? ': ' + (st.head.summary || st.head.text) : ''),
          `${n} ${n === 1 ? 'step' : 'steps'}` + (running ? ' · running' : ''), key, lines, false, null, true));
        continue;
      }
      const it = st;
      if (it.kind === 'tool' && (it.tool_kind === 'read' || readShell(it)) && !it.is_error) { reads.push(it); continue; }
      flushReads();
      if (it.kind === 'tool') nodes.push(toolNode(it));
      else if (it.kind === 'text' && it !== lastText) nodes.push(node('say', 'message', null, null, null, [h('q', { class: 'vq' }, oneLine(it.text, 200))]));
      else if (it.kind === 'error') nodes.push(node('fail', 'alert', it.summary || 'Error', it.text ? oneLine(it.text, 160) : null));
      else if (it.kind === 'prompt' && !it.sidechain) nodes.push(node('say', 'user', 'You said', oneLine(it.text, 160)));
      else if (it.kind === 'system') nodes.push(node('sys', 'spark', it.text || it.summary || ''));
    }
    flushReads();
    const working = latest && s && ACTIVE.has(s.state);
    const u = usage || (s && s.usage);
    if (working) nodes.push(node('now', null, s.state_detail ? plainStep(s.state_detail).label : 'Working', 'now', null, null, true));
    else if (latest && s && s.state !== 'blocked') {
      const took = receipt && receipt.since && toMs(receipt.since) ? Math.max(0, (toMs(s.updated_at) - toMs(receipt.since)) / 1000) : 0;
      nodes.push(node('end', 'anchor', s.state === 'exited' ? 'Session ended' : 'Finished', [took >= 60 ? fmtDuration(took) : null, u && u.cost_usd >= 0.01 ? '≈ ' + fmtUSD(u.cost_usd) : null].filter(Boolean).join(' · ') || null));
    }
    return h('ol', { class: 'voyage' }, nodes);
  }

  function toolNode(it) {
    const raw = rawOf(it);
    const cmd = (it.summary || '').replace(/^Run:\s*/, '');
    const denied = it.is_error && /\b(denied|blocked|refused|not allowed)\b/i.test(it.result || '');
    const res = (it.result || '').trim().split('\n').filter(Boolean);
    const lastLine = res.length ? res[res.length - 1] : '';
    let cls = 'step', ic = 'spark', label;
    if (it.tool_kind === 'shell') {
      const kind = /(?:^|[\s;&|(])(?:(?:npm|pnpm|yarn|bun)(?:\s+run)?\s+test|go\s+test|cargo\s+test|pytest|jest|vitest|mocha|make\s+test)\b/.test(cmd) ? 'Tests'
        : /(?:^|[\s;&|(])(?:(?:npm|pnpm|yarn|bun)(?:\s+run)?\s+(?:lint|typecheck)|eslint|golangci-lint|staticcheck|go\s+vet|ruff|cargo\s+clippy)\b/.test(cmd) ? 'Lint'
          : /(?:^|[\s;&|(])(?:(?:npm|pnpm|yarn|bun)(?:\s+run)?\s+build|go\s+build|cargo\s+build|vite\s+build|next\s+build)\b/.test(cmd) ? 'Build' : '';
      const commit = cmd.match(/\bgit\s+commit\b.*?-[a-zA-Z]*m\s*["']([^"'\n]+)/);
      if (denied) { cls = 'block'; ic = 'shieldOff'; const l = commandLabel(cmd); label = 'Blocked: ' + l[0].toLowerCase() + l.slice(1); }
      else if (it.pending) { cls = 'now'; ic = 'hourglass'; label = kind ? `${kind} running` : commandLabel(cmd); }
      else if (kind) { cls = it.is_error ? 'fail' : 'ok'; ic = it.is_error ? 'x' : 'check'; label = it.is_error ? `${kind} failed` : `${kind} passed`; }
      else if (commit) { cls = 'ok'; ic = 'branch'; label = `Committed “${commit[1]}”`; }
      else if (/\bgit\s+push\b/.test(cmd)) { cls = it.is_error ? 'fail' : 'ok'; ic = 'upload'; label = it.is_error ? 'Push failed' : 'Pushed'; }
      else { cls = it.is_error ? 'fail' : 'step'; ic = it.is_error ? 'x' : 'terminal'; label = it.is_error ? commandLabel(cmd) + ' failed' : commandLabel(cmd); }
    } else if (it.tool_kind === 'edit') {
      ic = 'pencil';
      const path = (it.summary || '').replace(/^(?:Edit|Write):?\s*/, '');
      label = (/^Write/.test(it.summary || '') ? 'Wrote ' : 'Edited ') + (path.split('/').pop() || 'a file');
      if (it.is_error) { cls = denied ? 'block' : 'fail'; ic = denied ? 'shieldOff' : 'x'; label = (denied ? 'Blocked: ' : 'Couldn’t edit ') + (path.split('/').pop() || 'a file'); }
    } else if (it.tool_kind === 'web') { ic = 'globe'; label = 'Read the web'; }
    else if (it.tool_kind === 'mcp') { ic = 'plug'; label = 'Used ' + ((it.summary || '').replace(/^MCP\s+/, '').split(':')[0] || it.tool); }
    else { label = it.summary || it.tool || 'Used a tool'; }
    const sub = it.is_error && !denied && lastLine ? oneLine(lastLine, 140) : null;
    return node(cls + ' k-' + (it.tool_kind || 'other') + (it.pending ? ' pending' : ''), ic, label, sub, it.result ? 'o:' + it.id : null, [h('span', { class: 'raw' }, raw)], it.pending, it.result);
  }

  /** One voyage node; key makes it expandable (its raw lines, or the output). */
  function node(cls, ic, label, sub, key, extra, spin, output, folded) {
    const open = key && openSteps.has(key);
    const shownExtra = !extra ? null : folded ? (open ? extra : null) : key && !output && !open ? extra.slice(0, 1) : extra;
    const body = h('div', { class: 'vt2' }, label ? h('b', null, label) : null, sub ? h('span', { class: 'sub' }, sub) : null,
      shownExtra, open && output ? h('pre', { class: 'out' }, output) : null);
    const mark = h('span', { class: 'vm', 'aria-hidden': 'true' }, spin ? h('span', { class: 'spin' }) : ic ? icon(ic, 'sm') : null);
    if (!key) return h('li', { class: 'vs ' + cls }, mark, body);
    return h('li', { class: 'vs ' + cls },
      mark, h('button', { class: 'vbtn', type: 'button', 'aria-expanded': String(!!open), onclick: () => { if (open) openSteps.delete(key); else openSteps.add(key); feedKey = ''; renderFeed(); } }, body));
  }

  function countTurns() {
    const set = new Set();
    for (const it of items.values()) set.add(it.turn || 0);
    return set.size;
  }

  function sentTurn() {
    return h('article', { class: 'act-turn latest' },
      sent.slice().reverse().map((s) => h('div', { class: 'act-prompt from-ph sending' },
        h('div', { class: 'act-who' }, ico('send', 'sm'), h('b', null, 'You'), h('span', null, ' · from this device'),
          h('span', { class: 'act-time' }, 'just now')),
        h('p', { class: 'act-ptext' }, s.text),
        h('p', { class: 'act-sent' }, spinner('sm'), s.note || 'Delivered — waiting for the agent to pick it up'))));
  }

  function turnEl(t, latest, s) {
    const steps = groupSteps(t.steps);
    const working = latest && s && ACTIVE.has(s.state);
    const stats = turnStats(t);
    if (latest) {
      return h('article', { class: 'act-turn latest', 'aria-label': `Turn ${t.n || 0}, latest` },
        t.prompts.map(promptEl),
        h('ol', { class: 'act-steps' }, stepsFor(t, steps)),
        working ? h('div', { class: 'act-live', role: 'status' }, spinner(), stateLabel(s) === 'Working' && s.state_detail ? s.state_detail : 'Working…') : null);
    }
    const det = h('details', { class: 'act-turn' });
    if (openTurns.has(t.key)) det.open = true;
    const p = t.prompts[0];
    const summary = h('summary', null,
      h('span', { class: 'act-tsum' },
        h('span', { class: 'act-tprompt' }, p ? (p.text || 'Prompt') : t.n ? `Turn ${t.n}` : 'Session start'),
        h('span', { class: 'act-tmeta' }, [
          stats.tools ? `${stats.tools} ${stats.tools === 1 ? 'step' : 'steps'}` : null,
          stats.errors ? `${stats.errors} ${stats.errors === 1 ? 'error' : 'errors'}` : null,
          t.at ? clock(t.at) : null,
        ].filter(Boolean).join(' · '))),
      ico('down', 'sm', 'act-chev'));
    add(det, summary);
    const body = h('div', { class: 'act-tbody' });
    add(det, body);
    const build = () => fill(body, t.prompts.map(promptEl), h('ol', { class: 'act-steps' }, stepsFor(t, steps)));
    if (det.open) build();
    det.addEventListener('toggle', () => {
      if (det.open) { openTurns.add(t.key); build(); } else openTurns.delete(t.key);
    });
    return det;
  }

  function stepsFor(t, steps) {
    if (steps.length <= STEPS_SHOWN + 2 || fullTurns.has(t.key)) return steps.map(stepEl);
    const hidden = steps.length - STEPS_SHOWN;
    return [
      h('li', { class: 'act-earlier' }, h('button', {
        class: 'btn ghost', type: 'button',
        onclick: () => { fullTurns.add(t.key); feedKey = ''; renderFeed(); },
      }, `Show ${hidden} earlier ${hidden === 1 ? 'step' : 'steps'}`)),
      ...steps.slice(-STEPS_SHOWN).map(stepEl),
    ];
  }

  function promptEl(p) {
    const who = p.source === 'pilothouse' ? ['You', ' · from Pilothouse'] : p.source === 'peer' ? ['Another agent', ''] : ['You', ''];
    return h('div', { class: 'act-prompt' + (p.source === 'pilothouse' ? ' from-ph' : '') },
      h('div', { class: 'act-who' }, icon('user', 'sm'), h('b', null, who[0]), who[1] ? h('span', null, who[1]) : null,
        h('time', { class: 'act-time', datetime: p.at }, clock(p.at))),
      clampText(p.text || '', 'act-ptext', 6, p.id));
  }

  function stepEl(st) {
    if (st.group) return subagentEl(st);
    const it = st;
    switch (it.kind) {
      case 'text':
        return h('li', { class: 'act-step act-text' },
          h('span', { class: 'act-node' }, ico('bot', 'sm')),
          h('div', { class: 'act-body' }, clampText(it.text || '', 'act-msg', 8, it.id)));
      case 'error':
        return h('li', { class: 'act-step act-error', role: 'note' },
          h('span', { class: 'act-node' }, icon('alert', 'sm')),
          h('div', { class: 'act-body' }, h('b', null, it.summary || 'Error'), it.text ? h('p', null, it.text) : null));
      case 'system':
        return h('li', { class: 'act-step act-system' }, h('span', null, it.text || it.summary || ''));
      case 'subagent':
        return h('li', { class: 'act-step act-sub-mark' },
          h('span', { class: 'act-node' }, ico('layers', 'sm')),
          h('div', { class: 'act-body' }, h('span', { class: 'act-sum' }, it.summary || it.text || 'Subagent')));
      case 'prompt':
        return h('li', { class: 'act-step' }, promptEl(it));
      default:
        return toolEl(it);
    }
  }

  function toolEl(it) {
    const kind = it.tool_kind || 'other';
    const ic = kind === 'read' && /^(grep|glob|search|find)/i.test(it.tool || '') ? 'search' : TOOL_ICONS[kind] || 'spark';
    const cls = 'act-step act-tool k-' + kind + (it.is_error ? ' err' : '') + (it.pending ? ' pending' : '');
    const status = it.pending
      ? h('span', { class: 'act-status' }, spinner('sm'), h('span', { class: 'sr-only' }, 'running'))
      : it.is_error ? h('span', { class: 'act-status bad' }, icon('alert', 'sm'), h('span', { class: 'sr-only' }, 'failed')) : null;
    const label = h('span', { class: 'act-sum' }, it.summary || it.tool || 'Tool call');
    const time = h('time', { class: 'act-time', datetime: it.at }, clock(it.at));
    if (!it.result) {
      return h('li', { class: cls },
        h('span', { class: 'act-node' }, icon(ic, 'sm')),
        h('div', { class: 'act-body' }, h('div', { class: 'act-line' }, label, status, time)));
    }
    const det = h('details', { class: 'act-det' },
      h('summary', null, h('div', { class: 'act-line' }, label, status, time, ico('down', 'sm', 'act-chev'))),
      h('pre', { class: 'act-result', tabindex: '0', 'aria-label': 'Output' }, it.result));
    if (openSteps.has(it.id)) det.open = true;
    det.addEventListener('toggle', () => { if (det.open) openSteps.add(it.id); else openSteps.delete(it.id); });
    return h('li', { class: cls }, h('span', { class: 'act-node' }, icon(ic, 'sm')), h('div', { class: 'act-body' }, det));
  }

  function subagentEl(g) {
    const n = g.items.filter((x) => x.kind === 'tool').length;
    const running = g.items.some((x) => x.pending);
    const errs = g.items.filter((x) => x.is_error || x.kind === 'error').length;
    const key = 'g:' + (g.head ? g.head.id : g.items[0].id);
    const det = h('details', { class: 'act-sub' });
    det.open = openSteps.has(key) || (running && !openSteps.has('closed:' + key));
    det.addEventListener('toggle', () => {
      if (det.open) { openSteps.add(key); openSteps.delete('closed:' + key); } else { openSteps.delete(key); openSteps.add('closed:' + key); }
    });
    add(det,
      h('summary', null,
        h('span', { class: 'act-node' }, ico('layers', 'sm')),
        h('span', { class: 'act-sub-t' },
          h('b', null, 'Subagent'),
          g.head && (g.head.summary || g.head.text) ? h('span', null, ' · ' + (g.head.summary || g.head.text)) : null),
        h('span', { class: 'act-sub-meta' },
          running ? spinner('sm') : null,
          `${n} ${n === 1 ? 'step' : 'steps'}`, errs ? h('span', { class: 'bad' }, ` · ${errs} failed`) : null),
        ico('down', 'sm', 'act-chev')),
      h('ol', { class: 'act-steps' }, g.items.map(stepEl)),
      g.tail ? h('p', { class: 'act-sub-tail' }, g.tail.summary || g.tail.text || 'Subagent finished') : null);
    return h('li', { class: 'act-step act-subwrap' }, det);
  }

  /** Long text, clamped with a "Show more" toggle. */
  function clampText(text, cls, lines, key) {
    const was = key && openClamps.has(key);
    const p = h('p', { class: cls + ' clamp clamp-' + lines + (was ? ' open' : '') }, text);
    const long = text.length > lines * 70 || text.split('\n').length > lines;
    if (!long) return p;
    const btn = h('button', { class: 'more-btn', type: 'button', 'aria-expanded': String(!!was) }, was ? 'Show less' : 'Show more');
    btn.addEventListener('click', () => {
      const open = p.classList.toggle('open');
      btn.textContent = open ? 'Show less' : 'Show more';
      btn.setAttribute('aria-expanded', String(open));
      if (key) { if (open) openClamps.add(key); else openClamps.delete(key); }
    });
    return h('div', { class: 'clamp-wrap' }, p, btn);
  }

  // ---- Fallback: hook-event timeline ----
  function fallbackNotice() {
    let title;
    let text;
    if (actState === 'unavailable') {
      title = 'Live activity isn’t available for this session';
      text = actNote || 'Pilothouse can’t read this agent’s transcript, so this is the timeline from its hooks.';
    } else if (actState === 'unsupported') {
      title = 'Live activity needs a newer Pilothouse';
      text = 'Update Pilothouse on your computer to see tool calls, replies and files here. Meanwhile, this is the timeline from the agent’s hooks.';
    } else {
      title = 'Couldn’t load the activity';
      text = 'Showing the timeline from the agent’s hooks instead. Pull to refresh or come back in a moment.';
    }
    return h('div', { class: 'act-notice', role: 'note' }, ico('info', 'sm'), h('div', null, h('b', null, title), h('p', null, text)));
  }

  function fallbackTimeline() {
    if (eventsErr && !events) return h('p', { class: 'quiet' }, 'Couldn’t load the timeline.');
    if (!events) return h('p', { class: 'quiet' }, 'Loading…');
    const list = timelineItems(events);
    return list.length ? h('ol', { class: 'timeline' }, list) : h('p', { class: 'quiet' }, 'Nothing has happened yet.');
  }

  // ---- Changes ----
  let allFiles = false;
  function renderFiles() {
    if (!files || !files.length) { fill(filesEl); return; }
    const s = store.sessions.get(id);
    const added = files.reduce((n, f) => n + (f.added || 0), 0);
    const removed = files.reduce((n, f) => n + (f.removed || 0), 0);
    const shown = allFiles ? files : files.slice(0, 8);
    fill(filesEl, h('section', { class: 'rsec', 'aria-labelledby': 'files-h' },
      h('h2', { class: 'lbl', id: 'files-h' }, `Changes · ${files.length} ${files.length === 1 ? 'file' : 'files'} `,
        h('span', { class: 'n-add' }, '+' + added), h('span', { class: 'n-del' }, '−' + removed)),
      h('ul', { class: 'list files rfiles' }, shown.map((f) => fileRow(f, s && s.cwd))),
      files.length > shown.length ? h('button', {
        class: 'btn ghost block act-more', type: 'button', onclick: () => { allFiles = true; renderFiles(); },
      }, `Show all ${files.length} files`) : null));
  }

  // ---- Lifecycle ----
  function render() {
    renderHead();
    renderNeeds();
    renderReply();
    renderReport();
    if (actState !== 'loading') renderFeed(); // the "Working…" row follows the live state
  }

  const off = onEvent((ev) => {
    if (ev.session_id !== id) return;
    if (ev.type === 'session.updated' || ev.type === 'session.reply' || ev.type === 'session.exited') kick();
  });
  function onVisibility() {
    if (document.hidden) {
      clearTimeout(timer);
      markVisited(id);
    } else if (actState !== 'unsupported') {
      poll();
    }
  }
  document.addEventListener('visibilitychange', onVisibility);

  async function ensureSession() {
    if (store.sessions.has(id)) return;
    try {
      const s = await api('GET', '/api/sessions/' + enc);
      store.sessions.set(s.id, s);
    } catch (e) {
      loadErr = e;
    }
    if (!destroyed) render();
  }

  const clocks = setInterval(() => {
    for (const el of root.querySelectorAll('[data-since]')) el.textContent = elapsedText(el.getAttribute('data-since'));
  }, 1000);
  render();
  renderFeed();
  ensureSession();
  poll();
  api('POST', `/api/sessions/${enc}/seen`).catch(() => {});

  return {
    el: root,
    update: render,
    destroy() {
      destroyed = true;
      clearInterval(clocks);
      off();
      clearTimeout(timer);
      clearTimeout(refetch);
      document.removeEventListener('visibilitychange', onVisibility);
      markVisited(id);
    },
  };
}

// ---------- Report helpers ----------

/** "finished 3 min ago", "working", "needs you" — the state in words. */
function statusWords(s) {
  switch (s.state) {
    case 'working': return 'working';
    case 'starting': return 'ready for a prompt';
    case 'blocked': return 'waiting on you';
    case 'errored': return 'stopped with an error ' + agoLong(s.updated_at);
    case 'exited': return 'ended ' + agoLong(s.updated_at);
    case 'unknown': return 'quiet';
    default: return 'finished ' + agoLong(s.updated_at);
  }
}

/** ": expected 3 attempts, got 5" — the telling line of the check's last failed run. */
function failReason(t, c) {
  const fail = [...t.steps].reverse().find((it) => it.kind === 'tool' && it.is_error && (it.summary || '').includes(c.command.slice(0, 40)));
  const lines = ((fail && fail.result) || '').split('\n').map((l) => l.trim()).filter(Boolean);
  const telling = lines.find((l) => /expected|assert|want|got|error:/i.test(l) && !/^(FAIL|ok)\b/.test(l)) || '';
  return telling ? ': ' + oneLine(telling.replace(/^[^:]*_test\.go:\d+:\s*/, '').replace(/^[×✗x]\s*/, ''), 90) : ', then passed';
}

function elapsedText(t) {
  const sec = Math.max(0, Math.floor((Date.now() - toMs(t)) / 1000));
  if (sec >= 3600) return Math.floor(sec / 3600) + 'h ' + String(Math.floor((sec % 3600) / 60)).padStart(2, '0') + 'm';
  return Math.floor(sec / 60) + ':' + String(sec % 60).padStart(2, '0');
}

function oneLine(t, max) {
  const s = String(t || '').replace(/\s+/g, ' ').trim();
  return s.length > max ? s.slice(0, max - 1) + '…' : s;
}

function isSearch(it) {
  return /grep|glob|search|find/i.test(it.tool || '') || /^Search\b/.test(it.summary || '') || /^Run:\s*(?:cd\s+\S+\s*(?:&&|;)\s*)?(?:grep|rg|ag|find|fd)\b/.test(it.summary || '');
}

function readShell(it) {
  return it.tool_kind === 'shell' && /^Run:\s*(?:cd\s+\S+\s*(?:&&|;)\s*)?(?:grep|rg|ag|find|fd|cat|head|tail|sed\s+-n|less|wc|ls|tree)\b/.test(it.summary || '');
}

function rawOf(it) {
  return (it.summary || it.tool || '').replace(/^(?:Run|Read|Edit|Write|Search|Fetch):?\s+/, '');
}

// ---------- Feed structure ----------

/** Items → turns in order: {key, n, at, prompts[], steps[]}. */
export function groupTurns(list) {
  const turns = new Map();
  for (const it of list) {
    const n = it.turn || 0;
    let t = turns.get(n);
    if (!t) {
      t = { key: 't' + n, n, at: it.at, prompts: [], steps: [] };
      turns.set(n, t);
    }
    if (it.kind === 'prompt' && !it.sidechain && !t.steps.length) t.prompts.push(it);
    else t.steps.push(it);
  }
  return [...turns.values()].sort((a, b) => a.n - b.n);
}

/** Folds subagent runs (a subagent marker plus its sidechain items) into groups. */
export function groupSteps(steps) {
  const out = [];
  let g = null;
  for (const it of steps) {
    if (it.kind === 'subagent' && !it.sidechain) {
      if (g && g.items.length) { g.tail = it; g = null; continue; }
      g = { group: true, head: it, items: [] };
      out.push(g);
      continue;
    }
    if (it.sidechain) {
      if (!g) { g = { group: true, head: null, items: [] }; out.push(g); }
      g.items.push(it);
      continue;
    }
    if (g && !g.items.length && g.head) {
      // A marker without sidechain items yet: show it as a plain step.
      out.pop();
      out.push(g.head);
    }
    g = null;
    out.push(it);
  }
  if (g && !g.items.length && g.head) { out.pop(); out.push(g.head); }
  return out;
}

function turnStats(t) {
  let tools = 0;
  let errors = 0;
  for (const it of t.steps) {
    if (it.kind === 'tool') tools++;
    if (it.is_error || it.kind === 'error') errors++;
  }
  return { tools, errors };
}

const OP_ICONS = { create: 'plus', delete: 'trash', rename: 'right', overwrite: 'pencil', edit: 'pencil' };
const OP_LABEL = { create: 'Created', delete: 'Deleted', rename: 'Renamed', overwrite: 'Rewrote', edit: 'Edited' };

/** One row of a "files changed" list, with a five-block diffstat. */
export function fileRow(f, cwd) {
  const r = relPath(f.path || '', cwd || '');
  const cut = r.lastIndexOf('/');
  return h('li', { class: 'file-row op-' + (f.op || 'edit') },
    h('span', { class: 'file-op', title: OP_LABEL[f.op] || 'Edited' }, ico(OP_ICONS[f.op] || 'pencil', 'sm'),
      h('span', { class: 'sr-only' }, (OP_LABEL[f.op] || 'Edited') + ' ')),
    h('span', { class: 'file-path', title: r },
      h('span', { class: 'base' }, cut > 0 ? r.slice(cut + 1) : r),
      cut > 0 ? h('span', { class: 'dir' }, r.slice(0, cut)) : null),
    diffStat(f.added || 0, f.removed || 0));
}

export function diffStat(added, removed) {
  const total = added + removed;
  const blocks = [];
  const plus = total ? Math.round((added / total) * 5) : 0;
  for (let i = 0; i < 5; i++) blocks.push(h('i', { class: !total ? '' : i < plus ? 'a' : 'd' }));
  return h('span', { class: 'diffstat' },
    h('span', { class: 'n-add' }, '+' + added), h('span', { class: 'n-del' }, '−' + removed),
    h('span', { class: 'blocks', 'aria-hidden': 'true' }, blocks));
}

// ---------- Fallback timeline from hook events (0.1) ----------

function item(ev, cls, ic, what, more, mono) {
  return h('li', { class: 'tl ' + cls },
    h('span', { class: 'node' }, icon(ic)),
    h('div', { class: 'body' },
      h('div', { class: 'what' }, what),
      more ? h('div', { class: 'more' + (mono ? ' mono' : '') }, more) : null),
    h('time', { datetime: ev.at, title: new Date(ev.at).toLocaleString() }, clock(ev.at)));
}

function decisionText(r) {
  const d = r.decision;
  const me = store.me && 'device:' + store.me.device.id;
  if (r.status === 'decided' && d) {
    const verb = d.behavior === 'allow' ? (d.scope === 'session' ? 'Allowed for the session' : 'Allowed') : 'Denied';
    let by = '';
    if (d.by === me) by = ' from this device';
    else if (d.by && d.by.startsWith('device:')) by = ' from another device';
    else if (d.by === 'cli' || d.by === 'tui') by = ' at the computer';
    else if (d.by === 'ntfy') by = ' from ntfy';
    else if (d.by && d.by.startsWith('policy:')) by = ' by policy ' + d.by.slice(7);
    return [verb + by, d.behavior === 'allow' ? 'ok' : 'bad', d.behavior === 'allow' ? 'check' : 'x', d.message];
  }
  if (r.status === 'answered_locally') return ['Answered in the terminal', '', 'terminal'];
  if (r.status === 'expired') return ['Timed out — handed back to the terminal', '', 'hourglass'];
  if (r.status === 'stepped_aside') return ['Shown in the terminal', '', 'terminal'];
  return null;
}

/** Turns raw session events (newest first) into timeline entries. */
export function timelineItems(events) {
  const out = [];
  // Session state changes: report only transitions, oldest→newest.
  const states = new Map();
  let prev = '';
  for (const ev of [...events].sort((a, b) => a.seq - b.seq)) {
    if (ev.type !== 'session.updated' || !ev.data) continue;
    const st = ev.data.state + (ev.data.block_kind || '');
    if (st !== prev) states.set(ev.seq, ev.data);
    prev = st;
  }
  for (const ev of events) {
    const d = ev.data || {};
    switch (ev.type) {
      case 'hook':
        switch (d.kind) {
          case 'pre_tool': {
            const paths = (d.paths || []).map((p) => relPath(p, d.cwd)).join('\n');
            const detail = d.command || paths;
            const title = d.summary || d.tool || 'Tool call';
            out.push(item(ev, '', TOOL_ICONS[d.tool_kind] || 'spark', title, detail && !title.includes(detail) ? detail : '', !!detail));
            break;
          }
          case 'prompt_submit':
            out.push(item(ev, 'you', 'user', 'You sent a prompt', d.message));
            break;
          case 'stop':
            out.push(item(ev, 'ok', 'message', 'Finished its turn', d.message));
            break;
          case 'notification':
            if (d.message) out.push(item(ev, 'ask', 'bell', d.message));
            break;
          // session_start/session_end hooks duplicate session.started/exited.
        }
        break;
      case 'request.opened':
        out.push(item(ev, 'ask', 'hourglass', 'Asked for approval', d.command || d.summary || d.tool, !!d.command));
        break;
      case 'request.closed': {
        const t = decisionText(d);
        if (t) out.push(item(ev, t[1], t[2], t[0], t[3] || (d.command || d.summary), !t[3] && !!d.command));
        break;
      }
      case 'policy.decision': {
        const rule = d.rule || d.policy_rule || d.name || 'policy';
        const b = d.behavior || d.action || '';
        out.push(item(ev, b === 'deny' ? 'bad' : b === 'allow' ? 'ok' : 'ask', 'shield', `Policy ${rule}${b ? ' → ' + b : ''}`, d.message || d.summary));
        break;
      }
      case 'session.reply': {
        out.push(item(ev, 'you', 'message', d.interrupt && !d.text ? 'Interrupted from Pilothouse' : 'Replied from Pilothouse', d.text));
        break;
      }
      case 'session.updated': {
        const s = states.get(ev.seq);
        // Permission blocks are shown by their request.opened entry.
        if (s && s.state !== 'working' && s.state !== 'starting' && !(s.state === 'blocked' && s.block_kind === 'permission')) {
          out.push(item(ev, s.state === 'blocked' ? 'ask' : s.state === 'errored' ? 'bad' : '', s.state === 'waiting' ? 'message' : 'alert', stateLabel(s), s.state_detail));
        }
        break;
      }
      case 'session.started':
        out.push(item(ev, '', 'play', 'Session started'));
        break;
      case 'session.exited':
        out.push(item(ev, '', 'stop', d.exit_code ? `Exited with code ${d.exit_code}` : 'Session ended'));
        break;
    }
  }
  return out;
}
