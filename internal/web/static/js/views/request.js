import { h, icon, agentName, repoName, agoLong, clock, fill } from '../dom.js';
import { store, api } from '../api.js';
import { requestCard } from '../card.js';
import { link, toast, skeleton, emptyState } from '../ui.js';
import { avatar, section, sessionLine } from './rows.js';

function decider(by) {
  const me = store.me && 'device:' + store.me.device.id;
  if (!by) return '';
  if (by === me) return 'this device';
  if (by.startsWith('device:')) return 'another paired device';
  if (by === 'cli' || by === 'tui') return 'the computer';
  if (by.startsWith('telegram')) return 'Telegram';
  if (by === 'ntfy') return 'ntfy';
  if (by.startsWith('policy:')) return 'the rule ' + by.slice(7);
  if (by.startsWith('grant:')) return 'a session grant';
  return by;
}

/** /r/:id — one request, e.g. opened from a notification. */
export function requestView(id) {
  const root = h('div', { class: 'view' });
  let missing = false;

  function render() {
    const r = store.requests.get(id);
    if (!r) {
      fill(root, missing
        ? emptyState({ icon: 'hourglass', title: 'Request not found', text: 'It may be older than the retention window, or from before this daemon restarted.' })
        : skeleton('card', 1));
      return;
    }
    const s = store.sessions.get(r.session_id);
    const d = r.decision;
    const facts = [];
    if (d) {
      facts.push(['Answered by', decider(d.by)], ['When', `${clock(d.at)} · ${agoLong(d.at)}`]);
      if (d.message) facts.push([r.checkpoint === 'plan' ? 'Feedback' : 'Message', d.message]);
      for (const q of r.questions || []) {
        const a = d.answers && d.answers[q.id || q.question];
        if (a) facts.push([q.header || q.question, a]);
      }
    }
    fill(root,
      h('div', { class: 'page-head' },
        h('div', null,
          h('h1', { class: 'page-title' }, r.status === 'open' ? 'Needs you' : r.checkpoint === 'plan' ? 'Plan' : r.checkpoint === 'question' ? 'Question' : 'Request'),
          h('p', { class: 'page-sub' }, `${agentName(r.agent)} · asked ${agoLong(r.created_at)}`))),
      h('div', { class: 'stack' }, requestCard(r, { keep: true, slot: 'detail', headingLevel: 2 })),
      facts.length ? section('Answer', 0, h('div', { class: 'group' },
        h('dl', { class: 'kv answer-kv' }, facts.flatMap(([k, v]) => [h('dt', null, k), h('dd', null, v)])))) : null,
      s ? section('Session', 0, h('div', { class: 'list' },
        link('/s/' + s.id, { class: 'row', 'data-nav': '', 'data-session': s.id }, avatar(s),
          h('span', { class: 'row-main' },
            h('span', { class: 'row-title' }, h('span', { class: 't' }, repoName(s)), s.branch ? h('span', { class: 'branch' }, s.branch) : null),
            h('span', { class: 'row-sub' }, sessionLine(s))),
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
