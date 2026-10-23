import { h, toMs, fill } from '../dom.js';
import { store } from '../api.js';
import { sessionRow, section } from './rows.js';

export function sessionsView() {
  const root = h('div', { class: 'view' });

  function render() {
    const all = [...store.sessions.values()].sort((a, b) => toMs(b.updated_at) - toMs(a.updated_at));
    const live = all.filter((s) => s.state !== 'exited');
    const ended = all.filter((s) => s.state === 'exited');
    const parts = [
      h('div', { class: 'page-head' },
        h('h1', { class: 'page-title' }, 'Sessions'),
        h('p', { class: 'page-sub' }, `${live.length} active${ended.length ? ` · ${ended.length} ended today` : ''}`)),
    ];
    if (live.length) parts.push(section('Active', live.length, h('div', { class: 'list' }, live.map(sessionRow))));
    if (ended.length) parts.push(section('Ended', ended.length, h('div', { class: 'list' }, ended.map(sessionRow))));
    if (!all.length) {
      parts.push(h('div', { class: 'empty' },
        h('h2', null, 'No sessions yet'),
        h('p', null, 'Start Claude Code or Codex on your computer and it shows up here.')));
    }
    fill(root, ...parts);
  }

  render();
  return { el: root, update: render };
}
