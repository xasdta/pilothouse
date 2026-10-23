import { h, icon, agentName, repoName, stateLabel, agoLong, clock, relPath, fill, add } from '../dom.js';
import { store, api, onEvent } from '../api.js';
import { requestCard } from '../card.js';
import { link, toast } from '../ui.js';
import { avatar, section, sessionLine } from './rows.js';

/** Session detail: header, any open request, last message, timeline. */
export function sessionView(id) {
  const root = h('div', { class: 'view' });
  const top = h('div');
  const tl = h('div');
  add(root, top, tl);
  let events = null;
  let loadErr = null;
  let refetch = 0;

  async function loadEvents() {
    try {
      events = await api('GET', `/api/sessions/${encodeURIComponent(id)}/events?limit=200`);
      loadErr = null;
    } catch (e) {
      loadErr = e;
    }
    renderTimeline();
  }

  async function ensureSession() {
    if (store.sessions.has(id)) return;
    try {
      const s = await api('GET', '/api/sessions/' + encodeURIComponent(id));
      store.sessions.set(s.id, s);
      render();
    } catch (e) {
      loadErr = e;
      render();
    }
  }

  function render() {
    const s = store.sessions.get(id);
    if (!s) {
      fill(top, loadErr
        ? h('div', { class: 'empty' }, h('h2', null, 'Session not found'), h('p', null, 'It may have ended more than a day ago.'))
        : h('p', { class: 'quiet' }, 'Loading…'));
      return;
    }
    const open = [...store.requests.values()].filter((r) => r.session_id === id && r.status === 'open');
    const kv = [
      ['Agent', agentName(s.agent) + (s.mode === 'wrapped' ? ' · started with pilothouse run' : '')],
      ['Folder', s.cwd],
      s.branch ? ['Branch', s.branch] : null,
      ['Started', `${clock(s.started_at)} · ${agoLong(s.started_at)}`],
      s.task_id ? ['Task', s.task_id] : null,
    ].filter(Boolean);
    fill(top,
      h('div', { class: 'card hero' },
        h('div', { class: 'hero-top' },
          avatar(s),
          h('div', null,
            h('h1', null, repoName(s)),
            h('div', { class: 'sub' }, h('span', { class: 'pill ' + s.state }, stateLabel(s)), ' · updated ', agoLong(s.updated_at)))),
        h('p', { class: 'detail' }, sessionLine(s)),
        s.last_message && sessionLine(s) !== s.last_message ? h('div', { class: 'quote' }, s.last_message) : null,
        h('dl', { class: 'kv' }, kv.flatMap(([k, v]) => [h('dt', null, k), h('dd', { class: k === 'Folder' ? 'mono' : '' }, v)]))),
      open.length ? section('Needs you', open.length, h('div', { class: 'stack' }, open.map((r) => requestCard(r))), 'urgent') : null,
    );
  }

  function renderTimeline() {
    if (loadErr && !events) {
      fill(tl, section('Timeline', 0, h('p', { class: 'quiet' }, 'Couldn’t load the timeline.')));
      return;
    }
    if (!events) {
      fill(tl, section('Timeline', 0, h('p', { class: 'quiet' }, 'Loading…')));
      return;
    }
    const items = timelineItems(events);
    fill(tl, section('Timeline', 0, items.length
      ? h('ol', { class: 'timeline' }, items)
      : h('p', { class: 'quiet' }, 'Nothing has happened yet.')));
  }

  const off = onEvent((ev) => {
    if (ev.session_id !== id) return;
    clearTimeout(refetch);
    refetch = setTimeout(loadEvents, 400);
  });

  render();
  renderTimeline();
  ensureSession();
  loadEvents();
  api('POST', `/api/sessions/${encodeURIComponent(id)}/seen`).catch(() => {});

  return {
    el: root,
    update: render,
    destroy() { off(); clearTimeout(refetch); },
  };
}

const TOOL_ICONS = { shell: 'terminal', edit: 'pencil', read: 'file', web: 'globe', mcp: 'plug', other: 'spark' };

function item(ev, cls, ic, what, more, mono) {
  return h('li', { class: 'tl ' + cls },
    h('span', { class: 'node' }, icon(ic)),
    h('div', { class: 'body' },
      h('div', { class: 'what' }, what),
      more ? h('div', { class: 'more' + (mono ? ' mono' : '') }, more) : null),
    h('time', { datetime: ev.at, title: new Date(ev.at).toLocaleString() }, clock(ev.at)));
}

function decisionText(r) {
  const d = r.decision;
  const me = store.me && 'device:' + store.me.device.id;
  if (r.status === 'decided' && d) {
    const verb = d.behavior === 'allow' ? (d.scope === 'session' ? 'Allowed for the session' : 'Allowed') : 'Denied';
    let by = '';
    if (d.by === me) by = ' from this device';
    else if (d.by && d.by.startsWith('device:')) by = ' from another device';
    else if (d.by === 'cli' || d.by === 'tui') by = ' at the computer';
    else if (d.by === 'ntfy') by = ' from ntfy';
    else if (d.by && d.by.startsWith('policy:')) by = ' by policy ' + d.by.slice(7);
    return [verb + by, d.behavior === 'allow' ? 'ok' : 'bad', d.behavior === 'allow' ? 'check' : 'x', d.message];
  }
  if (r.status === 'answered_locally') return ['Answered in the terminal', '', 'terminal'];
  if (r.status === 'expired') return ['Timed out — handed back to the terminal', '', 'hourglass'];
  if (r.status === 'stepped_aside') return ['Shown in the terminal', '', 'terminal'];
  return null;
}

/** Turns raw session events (newest first) into timeline entries. */
export function timelineItems(events) {
  const out = [];
  // Session state changes: report only transitions, oldest→newest.
  const states = new Map();
  let prev = '';
  for (const ev of [...events].sort((a, b) => a.seq - b.seq)) {
    if (ev.type !== 'session.updated' || !ev.data) continue;
    const st = ev.data.state + (ev.data.block_kind || '');
    if (st !== prev) states.set(ev.seq, ev.data);
    prev = st;
  }
  for (const ev of events) {
    const d = ev.data || {};
    switch (ev.type) {
      case 'hook':
        switch (d.kind) {
          case 'pre_tool': {
            const paths = (d.paths || []).map((p) => relPath(p, d.cwd)).join('\n');
            const detail = d.command || paths;
            const title = d.summary || d.tool || 'Tool call';
            out.push(item(ev, '', TOOL_ICONS[d.tool_kind] || 'spark', title, detail && !title.includes(detail) ? detail : '', !!detail));
            break;
          }
          case 'prompt_submit':
            out.push(item(ev, 'you', 'user', 'You sent a prompt', d.message));
            break;
          case 'stop':
            out.push(item(ev, 'ok', 'message', 'Finished its turn', d.message));
            break;
          case 'notification':
            if (d.message) out.push(item(ev, 'ask', 'bell', d.message));
            break;
          // session_start/session_end hooks duplicate session.started/exited.
        }
        break;
      case 'request.opened':
        out.push(item(ev, 'ask', 'hourglass', 'Asked for approval', d.command || d.summary || d.tool, !!d.command));
        break;
      case 'request.closed': {
        const t = decisionText(d);
        if (t) out.push(item(ev, t[1], t[2], t[0], t[3] || (d.command || d.summary), !t[3] && !!d.command));
        break;
      }
      case 'policy.decision': {
        const rule = d.rule || d.policy_rule || d.name || 'policy';
        const b = d.behavior || d.action || '';
        out.push(item(ev, b === 'deny' ? 'bad' : b === 'allow' ? 'ok' : 'ask', 'shield', `Policy ${rule}${b ? ' → ' + b : ''}`, d.message || d.summary));
        break;
      }
      case 'session.updated': {
        const s = states.get(ev.seq);
        // Permission blocks are shown by their request.opened entry.
        if (s && s.state !== 'working' && s.state !== 'starting' && !(s.state === 'blocked' && s.block_kind === 'permission')) {
          out.push(item(ev, s.state === 'blocked' ? 'ask' : s.state === 'errored' ? 'bad' : '', s.state === 'waiting' ? 'message' : 'alert', stateLabel(s), s.state_detail));
        }
        break;
      }
      case 'session.started':
        out.push(item(ev, '', 'play', 'Session started'));
        break;
      case 'session.exited':
        out.push(item(ev, '', 'stop', d.exit_code ? `Exited with code ${d.exit_code}` : 'Session ended'));
        break;
    }
  }
  return out;
}

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
