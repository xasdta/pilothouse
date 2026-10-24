// Inbox: what needs you (request, question and plan cards), what's at the
// terminal, who's working, who's waiting, and what was answered lately.
// On desktop the cards live in the needs-you pane, so the centre shows
// the rest.

import { h, icon, toMs, ago, repoName, fill, add, sync, money, agentClass } from '../dom.js';
import { store, api } from '../api.js';
import { requestCard, pruneCards, settling } from '../card.js';
import { link, go, isWide, skeleton, emptyState } from '../ui.js';
import { sessionRow } from './rows.js';

const ACTIVE = new Set(['working', 'starting']);
const IDLE = new Set(['waiting', 'errored', 'unknown']);

/** A section created once and refilled, so cached cards keep focus. */
function block(title, cls = '') {
  const count = h('span', { class: 'count' });
  const body = h('div');
  const el = h('section', { class: 'section', hidden: true }, h('h2', { class: 'section-head ' + cls }, title, count), body);
  return {
    el, body,
    set(n, kids) {
      el.hidden = !n;
      count.hidden = !n;
      count.textContent = String(n);
      if (n) sync(body, kids);
      else sync(body);
    },
  };
}

export function inboxView() {
  const root = h('div', { class: 'view inbox' });
  const title = h('h1', { class: 'page-title' }, 'Inbox');
  const sub = h('p', { class: 'page-sub' });
  const today = h('div', { class: 'today', hidden: true });
  const loading = h('div');
  const needs = block('Needs you', 'urgent');
  const needsStack = h('div', { class: 'stack' });
  const terminal = block('At the terminal');
  const working = block('Working');
  const waiting = block('Waiting for you');
  const recent = block('Recently answered');
  const empty = h('div');
  add(root, h('div', { class: 'page-head' }, h('div', null, title, sub)), today, loading,
    needs.el, empty, terminal.el, working.el, waiting.el, recent.el);

  function render() {
    if (!store.loaded) {
      fill(sub, 'Connecting…');
      fill(loading, skeleton('card', 2));
      return;
    }
    fill(loading);
    const wide = isWide();
    const reqs = [...store.requests.values()];
    const open = reqs
      .filter((r) => r.status === 'open' || settling.has(r.id))
      .sort((a, b) => toMs(a.expires_at) - toMs(b.expires_at));
    const openNow = open.filter((r) => r.status === 'open');
    const atTerminal = reqs.filter((r) => r.status === 'stepped_aside').sort((a, b) => toMs(b.created_at) - toMs(a.created_at));
    const sessions = [...store.sessions.values()].sort((a, b) => toMs(b.updated_at) - toMs(a.updated_at));
    const busy = new Set([...openNow, ...atTerminal].map((r) => r.session_id));
    // Sessions blocked on something Pilothouse can't answer (a Codex question, a sign-in).
    const blocked = sessions.filter((s) => s.state === 'blocked' && !busy.has(s.id));
    const workingS = sessions.filter((s) => ACTIVE.has(s.state));
    const waitingS = sessions.filter((s) => IDLE.has(s.state));
    const answered = reqs
      .filter((r) => r.status === 'decided' && !settling.has(r.id) && Date.now() - toMs(r.closed_at) < 3600e3)
      .sort((a, b) => toMs(b.closed_at) - toMs(a.closed_at))
      .slice(0, 4);

    const n = openNow.length;
    fill(sub, store.machine ? [store.machine, h('span', { class: 'sep', 'aria-hidden': 'true' }, '·')] : null,
      n ? `${n} ${n === 1 ? 'thing needs' : 'things need'} you` : 'Nothing needs you');

    if (!wide && open.length) {
      sync(needsStack, open.map((r) => requestCard(r)));
      needs.set(open.length, needsStack);
    } else {
      needs.set(0);
    }
    pruneCards(new Set(wide ? [] : open.map((r) => r.id)));

    if (!sessions.length && !open.length) {
      fill(empty, noAgents());
    } else if (!open.length && !atTerminal.length && !blocked.length) {
      fill(empty, allClear(wide));
    } else if (wide && open.length) {
      fill(empty, h('p', { class: 'hint-card' }, icon('hourglass'),
        h('span', null, `${n} waiting in Needs you, on the right. Press `, h('kbd', null, 'j'), ' to step through them, ',
          h('kbd', null, 'a'), ' to allow, ', h('kbd', null, 'd'), ' to deny.')));
    } else {
      fill(empty);
    }
    terminal.set(atTerminal.length + blocked.length,
      h('div', { class: 'list' }, atTerminal.map(terminalRow), blocked.map(sessionRow)));
    working.set(workingS.length, h('div', { class: 'list' }, workingS.map(sessionRow)));
    waiting.set(waitingS.length, h('div', { class: 'list' }, waitingS.map(sessionRow)));
    recent.set(answered.length, h('div', { class: 'list' }, answered.map(answeredRow)));
  }

  // Today at a glance: what the rules did for you.
  let todayTimer = 0;
  async function loadToday() {
    try {
      // The insights page's default range, so both share the daemon's work.
      const in1 = await api('GET', '/api/insights?days=7');
      const d = in1.days && in1.days[in1.days.length - 1];
      if (!d) return;
      const chip = (value, label, cls = '') => link('/insights', { class: 'stat-chip ' + cls }, h('b', null, value), label);
      fill(today,
        chip(money(d.usage.cost_usd), 'today'),
        chip(String(d.auto_allowed), 'auto-allowed', 'good'),
        chip(String(d.asked), 'asked you'),
        d.blocked ? chip(String(d.blocked), 'blocked') : null);
      today.hidden = false;
    } catch {
      today.hidden = true; // no insights on this daemon
    }
    clearTimeout(todayTimer);
    todayTimer = setTimeout(loadToday, 60000);
  }

  render();
  loadToday();
  return { el: root, update: render, destroy() { clearTimeout(todayTimer); } };
}

function allClear(wide) {
  const away = store.presence && store.presence.state === 'away';
  return emptyState({
    mark: true,
    title: 'All clear',
    text: away
      ? 'You’re away, so anything your agents need comes straight here — and to your notifications.'
      : 'You’re at your desk, so agents ask in the terminal. Switch to Away and they come here instead.',
    teach: [
      ['shield', 'Rules answer routine tool calls before they reach you. Tap “Make it a rule” on any request.'],
      ['message', 'Questions and plans from Claude Code and OpenCode land here too, with their options.'],
      ['bell', 'With notifications on, Android and desktop can allow or deny right from the notification.'],
    ],
    actions: [
      h('button', { class: 'btn secondary small', type: 'button', onclick: () => go('/rules') }, icon('rules', 'sm'), 'Rules'),
      h('button', { class: 'btn secondary small', type: 'button', onclick: () => go('/settings') }, icon(away ? 'bell' : 'away', 'sm'), away ? 'Notifications' : 'Presence'),
    ],
    hint: wide ? 'Press ? for keyboard shortcuts.' : null,
  });
}

function noAgents() {
  return emptyState({
    icon: 'agent',
    title: 'No agents yet',
    text: 'Start Claude Code, Codex or OpenCode in any terminal on your computer — Pilothouse sees them on its own.',
    teach: [
      ['terminal', 'Run an agent with pilothouse run (for example pilothouse run claude) to reply to it and watch its terminal from here.'],
      ['plus', 'Or start a task from this device: it gets its own worktree and branch.'],
    ],
    actions: [h('button', { class: 'btn primary small', type: 'button', onclick: () => go('/new') }, icon('plus', 'sm'), 'New task')],
  });
}

function terminalRow(r) {
  const s = store.sessions.get(r.session_id);
  return link('/r/' + r.id, { class: 'row', 'data-nav': '', 'data-key': 'term-' + r.id },
    h('span', { class: 'avatar ' + agentClass(r.agent), 'aria-hidden': 'true' }, icon('terminal', 'sm'), h('span', { class: 'state blocked' })),
    h('span', { class: 'row-main' },
      h('span', { class: 'row-title' }, h('span', { class: 't' }, repoName(s, r)), s && s.branch ? h('span', { class: 'branch' }, s.branch) : null),
      h('span', { class: 'row-sub' }, 'Asked in the terminal · ', h('span', { class: 'mono' }, r.command || r.summary || r.tool))),
    h('span', { class: 'row-end' }, ago(r.created_at)),
    icon('chev', 'sm', 'chev'));
}

function answeredRow(r) {
  const s = store.sessions.get(r.session_id);
  const allow = r.decision && r.decision.behavior === 'allow';
  const verb = r.checkpoint === 'question' ? (allow ? 'Answered' : 'Declined')
    : r.checkpoint === 'plan' ? (allow ? 'Approved' : 'Sent back')
      : allow ? 'Allowed' : 'Denied';
  return link('/r/' + r.id, { class: 'row', 'data-nav': '', 'data-key': 'done-' + r.id },
    h('span', { class: 'avatar ' + (allow ? 'ok' : 'bad'), 'aria-hidden': 'true' }, icon(allow ? 'check' : 'x', 'sm')),
    h('span', { class: 'row-main' },
      h('span', { class: 'row-title' }, h('span', { class: 't' }, verb + ' · ' + repoName(s, r))),
      h('span', { class: 'row-sub' }, h('span', { class: 'mono' }, r.command || r.summary || r.tool))),
    h('span', { class: 'row-end' }, ago(r.closed_at)),
    icon('chev', 'sm', 'chev'));
}
