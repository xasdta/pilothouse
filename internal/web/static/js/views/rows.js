import { h, icon, ago, repoName, stateLabel, agentInitials, agentClass, agentName } from '../dom.js';
import { link } from '../ui.js';

export function section(title, count, content, cls = '') {
  return h('section', { class: 'section' },
    h('h2', { class: 'section-head ' + cls }, title, count ? h('span', { class: 'count' }, String(count)) : null),
    content);
}

export function avatar(s) {
  return h('span', { class: 'avatar ' + agentClass(s.agent), 'aria-hidden': 'true' },
    agentInitials(s.agent), h('span', { class: 'state ' + s.state }));
}

/** One line describing what a session is doing now. */
export function sessionLine(s) {
  if (s.state_detail) return s.state_detail;
  if (s.state === 'waiting' && s.last_message) return s.last_message;
  if (s.state === 'exited') return s.exit_code ? `Exited with code ${s.exit_code}` : 'Session ended';
  return stateLabel(s);
}

export function sessionRow(s) {
  // No aria-label: the name comes from the visible text (WCAG 2.5.3), with
  // the agent and state (shown only as colour/initials) for screen readers.
  return link('/s/' + s.id, { class: 'row' },
    avatar(s),
    h('span', { class: 'sr-only' }, `${agentName(s.agent)}, ${stateLabel(s)}: `),
    h('span', { class: 'row-main' },
      h('span', { class: 'row-title' },
        h('span', { class: 't' }, repoName(s)),
        s.branch ? h('span', { class: 'branch' }, s.branch) : null),
      h('span', { class: 'row-sub' }, sessionLine(s))),
    h('span', { class: 'row-end' },
      h('time', { datetime: s.updated_at }, ago(s.updated_at)),
      s.unseen ? h('span', { class: 'unseen', title: 'New since you last looked' }) : null),
    icon('chev', 'sm', 'chev'));
}
