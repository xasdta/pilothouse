// The desktop side panes (≥ 1024 px): the session list in the sidebar and
// the needs-you queue on the right. Phones never render them.

import { h, icon, toMs, sync, fill, repoName, stateLabel } from './dom.js';
import { store } from './api.js';
import { panes, link, isWide } from './ui.js';
import { requestCard, pruneCards, settling } from './card.js';
import { avatar, sessionLine } from './views/rows.js';

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
    sync(p.queue, open.map((r) => requestCard(r, { slot: 'queue' })));
  }
  pruneCards(new Set(open.map((r) => r.id)), 'queue');
}
