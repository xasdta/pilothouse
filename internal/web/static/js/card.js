// The request card: what the agent wants, and Allow / Allow for session /
// Deny. Decisions are optimistic: the card shows the outcome at once and
// rolls back if the daemon refuses (409 = someone answered first).

import { h, icon, agentBadge, repoName, ago, left, toMs, requestVerb, grantPrefix, vibrate, relPath, fill, add } from './dom.js';
import { store, api, changed } from './api.js';
import { toast } from './ui.js';

const cards = new Map(); // request id → controller
/** Requests answered here whose cards are still animating out. */
export const settling = new Set();

/** Returns the (cached) card element for a request, refreshed. */
export function requestCard(req, opts = {}) {
  let c = cards.get(req.id);
  if (!c) {
    c = build(req, opts);
    cards.set(req.id, c);
  }
  c.update(req);
  return c.wrap;
}

/** Forget cards for requests no longer shown (keeps memory bounded). */
export function pruneCards(visibleIds) {
  for (const id of cards.keys()) {
    if (!visibleIds.has(id) && !settling.has(id)) cards.delete(id);
  }
}

function inputField(req, ...keys) {
  if (!req.input || typeof req.input !== 'object') return '';
  for (const k of keys) if (typeof req.input[k] === 'string') return req.input[k];
  return '';
}

function codeBlock(req) {
  const pre = h('pre', { class: 'code', tabindex: '0', 'aria-label': 'Details' });
  if (req.tool_kind === 'shell' && req.command) {
    add(pre, h('span', { class: 'prompt' }, '$'), req.command);
  } else if (req.paths && req.paths.length) {
    req.paths.slice(0, 6).forEach((p, i) => {
      const r = relPath(p, req.cwd);
      const cut = r.lastIndexOf('/');
      if (i) add(pre, '\n');
      if (cut > 0) add(pre, h('span', { class: 'path-dir' }, r.slice(0, cut + 1)), r.slice(cut + 1));
      else add(pre, r);
    });
    if (req.paths.length > 6) add(pre, '\n', h('span', { class: 'path-dir' }, `+ ${req.paths.length - 6} more`));
  } else if (req.tool_kind === 'web') {
    add(pre, inputField(req, 'url', 'query') || req.summary || req.tool);
  } else {
    add(pre, req.summary || req.tool);
  }
  return pre;
}

function outcomeFor(req) {
  const me = store.me && 'device:' + store.me.device.id;
  const d = req.decision;
  if (req.status === 'decided' && d) {
    const allow = d.behavior === 'allow';
    let who = '';
    if (d.by === me) who = '';
    else if (d.by && d.by.startsWith('device:')) who = ' on another device';
    else if (d.by === 'cli' || d.by === 'tui') who = ' at the computer';
    else if (d.by === 'ntfy') who = ' from ntfy';
    else if (d.by && d.by.startsWith('policy:')) who = ' by policy';
    else if (d.by && d.by.startsWith('grant:')) who = ' by a session grant';
    const what = allow ? (d.scope === 'session' ? 'Allowed for this session' : 'Allowed') : 'Denied';
    return { cls: allow ? 'allow' : 'deny', icon: allow ? 'check' : 'x', text: what + who };
  }
  switch (req.status) {
    case 'answered_locally': return { cls: 'neutral', icon: 'terminal', text: 'Answered in the terminal' };
    case 'expired': return { cls: 'neutral', icon: 'hourglass', text: 'Timed out — handed back to the terminal' };
    case 'stepped_aside': return { cls: 'neutral', icon: 'terminal', text: 'Waiting in the terminal' };
    default: return null;
  }
}

function build(req, opts) {
  const els = {};
  const card = h('article', { class: 'card req' + (opts.animate === false ? '' : ' enter'), 'aria-label': 'Request from ' + req.agent });
  const wrap = h('div', { class: 'collapse' }, h('div', null, card));

  els.timerBar = h('i');
  els.timer = h('div', { class: 'timer', 'aria-hidden': 'true' }, els.timerBar);
  els.where = h('span', { class: 'where' });
  els.age = h('time', { class: 'age' });
  els.what = h('div', { class: 'req-what' });
  els.code = h('div');
  els.meta = h('div', { class: 'meta-row' });
  els.left = h('span', { class: 'item left' });

  const allow = h('button', { class: 'btn primary', type: 'button' }, icon('check'), 'Allow');
  const session = h('button', { class: 'btn secondary', type: 'button' }, 'Allow for session');
  const deny = h('button', { class: 'btn danger', type: 'button' }, 'Deny');
  els.buttons = [allow, session, deny];
  const actions = h('div', { class: 'actions' }, deny, session, allow);

  const reasonId = 'reason-' + req.id;
  const reason = h('textarea', { class: 'input', id: reasonId, rows: 2, maxlength: 2000, placeholder: 'e.g. Not on main — use a feature branch' });
  const denyConfirm = h('button', { class: 'btn danger', type: 'button' }, 'Deny');
  const denyCancel = h('button', { class: 'btn ghost', type: 'button' }, 'Cancel');
  const denyForm = h('div', { class: 'deny-form' },
    h('label', { class: 'field-label', for: reasonId }, 'Tell the agent why (optional)'),
    reason,
    h('div', { class: 'row-btns' }, denyCancel, denyConfirm));

  els.outcome = h('div', { class: 'outcome', role: 'status', 'aria-live': 'polite' });

  add(card, els.timer,
    h('div', { class: 'req-top' }, agentBadge(req.agent), els.where, els.age),
    els.what, els.code, els.meta, actions, denyForm, els.outcome);

  let current = req;
  let busy = false;

  function showOutcome(o) {
    els.outcome.className = 'outcome ' + o.cls;
    fill(els.outcome, icon(o.icon, '', 'check'), h('span', null, o.text));
    card.classList.add('resolved');
  }

  function settle(delay) {
    settling.add(current.id);
    setTimeout(() => {
      card.classList.add('leaving');
      wrap.classList.add('gone');
      setTimeout(() => {
        settling.delete(current.id);
        wrap.remove();
        changed();
      }, 380);
    }, delay);
  }

  async function decide(behavior, scope, message) {
    if (busy) return;
    busy = true;
    vibrate(behavior === 'allow' ? 12 : [8, 40, 8]);
    const label = behavior === 'deny' ? 'Denied' : scope === 'session' ? 'Allowed for this session' : 'Allowed';
    showOutcome({ cls: behavior === 'allow' ? 'allow' : 'deny', icon: behavior === 'allow' ? 'check' : 'x', text: label });
    settling.add(current.id);
    const res = await self.PilothouseKeys.decide(current.id, behavior, scope, message);
    busy = false;
    if (res.ok) {
      store.requests.set(res.data.id, res.data);
      if (opts.keep) settling.delete(current.id); else settle(900);
      return;
    }
    if (res.status === 409) {
      toast('Already answered on another device', 'bad');
      try {
        const fresh = await api('GET', '/api/requests/' + encodeURIComponent(current.id));
        store.requests.set(fresh.id, fresh);
        const o = outcomeFor(fresh);
        if (o) showOutcome(o);
      } catch { /* keep the optimistic text */ }
      if (opts.keep) settling.delete(current.id); else settle(1800);
      return;
    }
    // Roll back.
    settling.delete(current.id);
    card.classList.remove('resolved');
    const msg = res.data && res.data.code === 'stale_signature'
      ? 'This phone’s clock is off — fix the time and try again'
      : (res.data && res.data.error) || 'Couldn’t send — try again';
    toast(msg, 'bad');
    if (res.status === 401) location.assign('/pair');
  }

  allow.addEventListener('click', () => decide('allow', 'once'));
  session.addEventListener('click', () => decide('allow', 'session'));
  deny.addEventListener('click', () => {
    card.classList.add('denying');
    reason.focus({ preventScroll: true });
    card.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
  });
  denyCancel.addEventListener('click', () => {
    card.classList.remove('denying');
    deny.focus();
  });
  denyConfirm.addEventListener('click', () => {
    card.classList.remove('denying');
    decide('deny', 'once', reason.value.trim());
  });
  reason.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) denyConfirm.click();
    if (e.key === 'Escape') denyCancel.click();
  });

  function tick(now = Date.now()) {
    els.age.textContent = ago(current.created_at, now);
    els.age.dateTime = current.created_at;
    if (current.status !== 'open' || !current.expires_at) {
      els.left.hidden = true;
      return;
    }
    const total = toMs(current.expires_at) - toMs(current.created_at);
    const rem = toMs(current.expires_at) - now;
    const frac = total > 0 ? Math.max(0, Math.min(1, rem / total)) : 0;
    els.timerBar.style.transform = `scaleX(${frac.toFixed(4)})`; // CSSOM, allowed by CSP
    const low = rem < 60000;
    els.timer.classList.toggle('low', low);
    els.left.classList.toggle('low', low);
    els.left.hidden = false;
    fill(els.left, icon('clock', 'sm'), rem > 0 ? left(current.expires_at, now) + ' left' : 'Expiring…');
  }

  function update(r) {
    current = r;
    const s = store.sessions.get(r.session_id);
    fill(els.where, repoName(s, r), s && s.branch ? h('span', { class: 'branch' }, ' · ' + s.branch) : null);
    const verb = requestVerb(r);
    fill(els.what, verb, r.tool_kind === 'mcp' || r.tool_kind === 'other' ? null : h('span', { class: 'tool' }, ' · ' + r.tool));
    fill(els.code, codeBlock(r));
    const metas = [];
    if (r.policy_rule) metas.push(h('span', { class: 'item' }, icon('shield', 'sm'), r.policy_rule));
    metas.push(els.left);
    fill(els.meta, ...metas);
    session.title = `Also allow ${grantPrefix(r)} for the rest of this session`;
    session.setAttribute('aria-label', `Allow for session: also allow ${grantPrefix(r)} until this session ends`);
    allow.setAttribute('aria-label', `Allow once: ${verb.toLowerCase()} ${r.command || r.summary || r.tool}`);
    const actionable = r.status === 'open';
    if (!busy && !settling.has(r.id)) {
      const o = actionable ? null : outcomeFor(r);
      if (o) showOutcome(o);
      else card.classList.remove('resolved');
    }
    for (const b of els.buttons) b.disabled = !actionable;
    tick();
  }

  return { wrap, update, tick: (now) => tick(now) };
}

// One clock for every visible card.
setInterval(() => {
  const now = Date.now();
  for (const c of cards.values()) if (c.wrap.isConnected) c.tick(now);
}, 1000);
