import { h, icon, toMs, ago, repoName, fill, add } from '../dom.js';
import { store } from '../api.js';
import { requestCard, pruneCards, settling } from '../card.js';
import { link } from '../ui.js';
import { sessionRow, section } from './rows.js';

const ACTIVE = new Set(['working', 'starting']);
const IDLE = new Set(['waiting', 'errored', 'unknown']);

export function inboxView() {
  const root = h('div', { class: 'view' });
  const head = h('div', { class: 'page-head' });
  const body = h('div');
  add(root, head, body);

  function render() {
    const reqs = [...store.requests.values()];
    const open = reqs
      .filter((r) => r.status === 'open' || settling.has(r.id))
      .sort((a, b) => toMs(a.expires_at) - toMs(b.expires_at));
    const atTerminal = reqs
      .filter((r) => r.status === 'stepped_aside')
      .sort((a, b) => toMs(b.created_at) - toMs(a.created_at));
    const sessions = [...store.sessions.values()].sort((a, b) => toMs(b.updated_at) - toMs(a.updated_at));
    const openBySession = new Set([...open, ...atTerminal].map((r) => r.session_id));
    // Sessions blocked on something Pilothouse can't answer (a question, a sign-in).
    const blocked = sessions.filter((s) => s.state === 'blocked' && !openBySession.has(s.id));
    const working = sessions.filter((s) => ACTIVE.has(s.state));
    const waiting = sessions.filter((s) => IDLE.has(s.state));
    const recent = reqs
      .filter((r) => r.status === 'decided' && !settling.has(r.id) && Date.now() - toMs(r.closed_at) < 3600e3)
      .sort((a, b) => toMs(b.closed_at) - toMs(a.closed_at))
      .slice(0, 4);

    const n = open.length;
    fill(head,
      h('h1', { class: 'page-title' }, 'Inbox'),
      h('p', { class: 'page-sub' }, store.machine ? store.machine + ' · ' : '',
        n ? `${n} ${n === 1 ? 'request needs' : 'requests need'} you` : 'Nothing needs you'));

    const parts = [];
    if (n) {
      parts.push(section('Needs you', n, h('div', { class: 'stack' }, open.map((r) => requestCard(r))), 'urgent'));
    } else if (!atTerminal.length && !blocked.length) {
      parts.push(emptyState());
    }
    pruneCards(new Set(open.map((r) => r.id)));

    const terminalRows = [
      ...atTerminal.map((r) => terminalRow(r)),
      ...blocked.map((s) => sessionRow(s)),
    ];
    if (terminalRows.length) {
      parts.push(section('At the terminal', terminalRows.length, h('div', { class: 'list' }, terminalRows)));
    }
    if (working.length) parts.push(section('Working', working.length, h('div', { class: 'list' }, working.map((s) => sessionRow(s)))));
    if (waiting.length) parts.push(section('Waiting', waiting.length, h('div', { class: 'list' }, waiting.map((s) => sessionRow(s)))));
    if (recent.length) parts.push(section('Recently answered', 0, h('div', { class: 'list' }, recent.map((r) => answeredRow(r)))));
    if (!sessions.length && !n) {
      parts.push(h('p', { class: 'quiet' }, 'No agent sessions yet. Start Claude Code or Codex on your computer.'));
    }
    fill(body, ...parts);
  }

  render();
  return { el: root, update: render };
}

function emptyState() {
  const away = store.presence && store.presence.state === 'away';
  return h('div', { class: 'empty' },
    h('div', { class: 'halo' }, h('img', { class: 'mark', src: '/icons/icon.svg', alt: '' })),
    h('h2', null, 'All clear'),
    h('p', null, 'When an agent asks for permission while you’re away, it shows up here.'),
    h('p', { class: 'hint' }, away
      ? 'You’re away — new requests come to this device.'
      : 'You’re at your desk — requests open in the terminal.'));
}

function terminalRow(r) {
  const s = store.sessions.get(r.session_id);
  return link('/r/' + r.id, { class: 'row' },
    h('span', { class: 'avatar ' + (r.agent === 'claude' || r.agent === 'codex' ? r.agent : 'generic') }, icon('terminal', 'sm'),
      h('span', { class: 'state blocked' })),
    h('span', { class: 'row-main' },
      h('span', { class: 'row-title' }, h('span', { class: 't' }, repoName(s, r)), s && s.branch ? h('span', { class: 'branch' }, s.branch) : null),
      h('span', { class: 'row-sub' }, 'Asked in the terminal · ', h('span', { class: 'mono' }, r.command || r.summary || r.tool))),
    h('span', { class: 'row-end' }, ago(r.created_at)),
    icon('chev', 'sm', 'chev'));
}

function answeredRow(r) {
  const s = store.sessions.get(r.session_id);
  const allow = r.decision && r.decision.behavior === 'allow';
  return link('/r/' + r.id, { class: 'row' },
    h('span', { class: 'avatar ' + (allow ? 'ok' : 'bad') }, icon(allow ? 'check' : 'x', 'sm')),
    h('span', { class: 'row-main' },
      h('span', { class: 'row-title' }, h('span', { class: 't' }, (allow ? 'Allowed' : 'Denied') + ' · ' + repoName(s, r))),
      h('span', { class: 'row-sub' }, h('span', { class: 'mono' }, r.command || r.summary || r.tool))),
    h('span', { class: 'row-end' }, ago(r.closed_at)),
    icon('chev', 'sm', 'chev'));
}

