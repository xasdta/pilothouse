import { h, icon, agentName, repoName, agoLong, fill } from '../dom.js';
import { store, api } from '../api.js';
import { requestCard } from '../card.js';
import { link, toast } from '../ui.js';
import { avatar, section } from './rows.js';

/** /r/:id — one request, e.g. opened from a notification. */
export function requestView(id) {
  const root = h('div', { class: 'view' });
  let missing = false;

  function render() {
    const r = store.requests.get(id);
    if (!r) {
      fill(root, missing
        ? h('div', { class: 'empty' }, h('h2', null, 'Request not found'), h('p', null, 'It may be older than the retention window.'))
        : h('p', { class: 'quiet' }, 'Loading…'));
      return;
    }
    const s = store.sessions.get(r.session_id);
    fill(root,
      h('div', { class: 'page-head' },
        h('h1', { class: 'page-title' }, r.status === 'open' ? 'Needs you' : 'Request'),
        h('p', { class: 'page-sub' }, `${agentName(r.agent)} · ${agoLong(r.created_at)}`)),
      h('div', { class: 'stack' }, requestCard(r, { keep: true })),
      s ? section('Session', 0, h('div', { class: 'list' },
        link('/s/' + s.id, { class: 'row' }, avatar(s),
          h('span', { class: 'row-main' },
            h('span', { class: 'row-title' }, h('span', { class: 't' }, repoName(s)), s.branch ? h('span', { class: 'branch' }, s.branch) : null),
            h('span', { class: 'row-sub' }, 'Open timeline')),
          icon('chev', 'sm', 'chev')))) : null,
    );
  }

  if (!store.requests.has(id)) {
    api('GET', '/api/requests/' + encodeURIComponent(id))
      .then((r) => { store.requests.set(r.id, r); render(); })
      .catch((e) => { missing = true; render(); if (e.status !== 404) toast(e.message, 'bad'); });
  }
  render();
  return { el: root, update: render };
}
