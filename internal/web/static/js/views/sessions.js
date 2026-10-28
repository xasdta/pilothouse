import { h, toMs, fill, agentName, agentClass, money } from '../dom.js';
import { store } from '../api.js';
import { skeleton, emptyState, go } from '../ui.js';
import { sessionRow, section } from './rows.js';

const GROUPS = [
  ['Needs you', (s) => s.state === 'blocked', 'urgent'],
  ['Working', (s) => s.state === 'working' || s.state === 'starting', ''],
  ['Waiting for you', (s) => s.state === 'waiting' || s.state === 'errored' || s.state === 'unknown', ''],
  ['Ended', (s) => s.state === 'exited', ''],
];

export function sessionsView() {
  const root = h('div', { class: 'view' });
  let agent = '';

  function render() {
    if (!store.loaded) {
      fill(root, h('div', { class: 'page-head' }, h('div', null, h('h1', { class: 'page-title' }, 'Sessions'))), skeleton('row', 4));
      return;
    }
    const all = [...store.sessions.values()].sort((a, b) => toMs(b.updated_at) - toMs(a.updated_at));
    const agents = [...new Set(all.map((s) => s.agent))];
    if (agent && !agents.includes(agent)) agent = '';
    const shown = all.filter((s) => !agent || s.agent === agent);
    const live = all.filter((s) => s.state !== 'exited');
    const ended = all.length - live.length;
    const spend = live.reduce((t, s) => t + ((s.usage && s.usage.cost_usd) || 0), 0);
    const parts = [
      h('div', { class: 'page-head' }, h('div', null,
        h('h1', { class: 'page-title' }, 'Sessions'),
        h('p', { class: 'page-sub' }, `${live.length} active`, ended ? ` · ${ended} ended` : '', spend >= 0.01 ? ` · ${money(spend)} so far` : ''))),
    ];
    if (agents.length > 1) {
      const chip = (a, label) => h('button', {
        class: 'filter ' + (a ? agentClass(a) : ''), type: 'button', 'aria-pressed': String(agent === a),
        onclick: () => { agent = a; render(); },
      }, a ? h('span', { class: 'sw', 'aria-hidden': 'true' }) : null, label);
      parts.push(h('div', { class: 'filters', role: 'group', 'aria-label': 'Filter by agent' },
        chip('', 'All'), agents.map((a) => chip(a, agentName(a)))));
    }
    for (const [title, test, cls] of GROUPS) {
      const list = shown.filter(test);
      if (list.length) parts.push(section(title, list.length, h('div', { class: 'list' }, list.map(sessionRow)), cls));
    }
    if (!all.length) {
      parts.push(emptyState({
        icon: 'sessions',
        title: 'No sessions yet',
        text: 'Every Claude Code, Codex, OpenCode and Grok Build session on your computer shows up here, with what it’s doing right now.',
        teach: [
          ['terminal', 'Start an agent as usual — claude, codex, opencode or grok — and it appears within a second.'],
          ['reply', 'Start it with pilothouse run to reply and watch its terminal from this device.'],
        ],
        actions: [h('button', { class: 'btn primary small', type: 'button', onclick: () => go('/new') }, 'Start a task')],
      }));
    }
    fill(root, ...parts);
  }

  render();
  return { el: root, update: render };
}
