// The desktop side panes (≥ 1024 px): the session list in the sidebar and
// the needs-you queue on the right. Phones never render them.

import { h, icon, toMs, ago, sync, fill, repoName, stateLabel, agentClass } from './dom.js';
import { askTitle } from './views/today.js';
import { store } from './api.js';
import { panes, link, isWide } from './ui.js';
import { requestCard, pruneCards, settling } from './card.js';
import { avatar, sessionLine } from './views/rows.js';

const qcards = new Map(); // request id → cached pane card (keeps a typed deny reason)

/** The pane's card: who and what in Today's words, then the decision itself. */
function queueCard(r) {
  const s = store.sessions.get(r.session_id);
  const perm = r.checkpoint !== 'question' && r.checkpoint !== 'plan';
  let c = qcards.get(r.id);
  if (!c) {
    const head = h('div', { class: 'qhead' });
    const body = h('div', { class: 'qbody' });
    c = { el: h('section', { class: 'qcard' + (perm ? ' perm' : ''), 'data-key': r.id }, head, body), head, body };
    qcards.set(r.id, c);
  }
  fill(c.head, h('span', { class: 'ag ' + agentClass(r.agent), 'aria-hidden': 'true' }), h('b', null, repoName(s, r)),
    s && s.branch ? h('span', { class: 'br' }, s.branch) : null, h('time', null, ago(r.created_at)));
  // Questions and plans keep their full card here: number keys pick options,
  // d asks for changes (palette.js).
  const card = requestCard(r, perm ? { slot: 'queue', variant: 'ledger', animate: false } : { slot: 'queue', animate: false });
  if (card.parentNode !== c.body) fill(c.body, h('p', { class: 'qask' }, askTitle(r)), card);
  else c.body.firstChild.textContent = askTitle(r);
  return c.el;
}

const ORDER = { blocked: 0, working: 1, starting: 1, waiting: 2, errored: 2, unknown: 3, exited: 4 };

let emptyQueue = null;

/** Re-renders both panes for the current route. */
export function renderPanes(path) {
  if (!isWide() || !store.loaded) return;
  const p = panes();

  const sessions = [...store.sessions.values()]
    .sort((a, b) => (ORDER[a.state] ?? 3) - (ORDER[b.state] ?? 3) || toMs(b.updated_at) - toMs(a.updated_at));
  const live = sessions.filter((s) => s.state !== 'exited');
  const ended = sessions.filter((s) => s.state === 'exited').slice(0, 3);
  const current = (/^\/s\/([\w-]+)/.exec(path || '') || [])[1];
  fill(p.sessions, [...live, ...ended].map((s) => link('/s/' + s.id, {
    class: 'side-session', 'aria-current': s.id === current ? 'page' : null, title: `${repoName(s)} — ${stateLabel(s)}`,
  }, avatar(s), h('span', { class: 'txt' }, h('b', null, repoName(s)), h('small', null, sessionLine(s))))),
  sessions.length ? null : h('p', { class: 'quiet' }, 'No sessions yet'));

  const open = [...store.requests.values()]
    .filter((r) => r.status === 'open' || settling.has(r.id))
    .sort((a, b) => toMs(a.expires_at) - toMs(b.expires_at));
  const n = open.filter((r) => r.status === 'open').length;
  p.queueCount.hidden = n === 0;
  p.queueCount.textContent = String(n);
  if (!open.length) {
    emptyQueue = emptyQueue || h('div', { class: 'queue-empty' }, icon('check'),
      h('b', null, 'Nothing needs you'),
      h('span', null, 'Requests, questions and plans from your agents land here.'));
    sync(p.queue, emptyQueue);
  } else {
    sync(p.queue, open.map(queueCard));
  }
  pruneCards(new Set(open.map((r) => r.id)), 'queue');
  for (const id of qcards.keys()) if (!open.some((r) => r.id === id)) qcards.delete(id);
}
