// /r/:id — one request as a decision page: what it wants in full, why the
// agent wants it (its own words), what this turn did (detected, not
// claimed), why you are being asked, what happens if you wait, and a bar of
// answers at the bottom. Questions and plans get the same frame with their
// options or steps. The answers themselves live in card.js (variant
// 'decision'): signed, optimistic, rolled back on failure.

import { h, svg, icon, agentName, agentClass, repoName, agoLong, clock, fill, sync, basename, plural, toMs } from '../dom.js';
import { store, api } from '../api.js';
import { requestCard, pruneCards } from '../card.js';
import { link, toast, skeleton, emptyState, setTopTitle } from '../ui.js';
import { markdownTitle } from '../markdown.js';
import { avatar, section, sessionLine } from './rows.js';
import { askTitle } from './today.js';

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

/** The short word above the title: what state the request is in. */
function kicker(r) {
  const allow = r.decision && r.decision.behavior === 'allow';
  switch (r.status) {
    case 'open': return { cls: 'need', icon: 'clock', text: 'Needs you' };
    case 'decided':
      if (r.checkpoint === 'question') return allow ? { cls: 'ok', icon: 'check', text: 'Answered' } : { cls: 'bad', icon: 'x', text: 'Declined' };
      if (r.checkpoint === 'plan') return allow ? { cls: 'ok', icon: 'check', text: 'Approved' } : { cls: 'bad', icon: 'reply', text: 'Changes requested' };
      return allow ? { cls: 'ok', icon: 'check', text: 'Allowed' } : { cls: 'bad', icon: 'x', text: 'Denied' };
    case 'expired': return { cls: 'quiet', icon: 'hourglass', text: 'Timed out' };
    case 'answered_locally': return { cls: 'quiet', icon: 'terminal', text: 'Answered in the terminal' };
    case 'stepped_aside': return { cls: 'quiet', icon: 'terminal', text: 'Waiting in the terminal' };
    default: return { cls: 'quiet', icon: 'info', text: 'Closed' };
  }
}

let rulesCache = null; // name → RuleInfo, read once per app load
function loadRules() {
  if (!rulesCache) {
    rulesCache = api('GET', '/api/rules')
      .then((d) => new Map(((d && d.rules) || []).map((r) => [r.name, r])))
      .catch(() => new Map());
  }
  return rulesCache;
}

export function requestView(id) {
  const root = h('div', { class: 'view dpage' });
  let missing = false;

  // Sections the decision card shows; built once, kept up to date here.
  const quote = h('blockquote', { class: 'dquote' });
  const why = h('section', { class: 'dsec', 'aria-labelledby': 'd-why' }, label('d-why', 'Why it wants to'), quote);
  const turn = h('section', { class: 'dsec', 'aria-labelledby': 'd-turn' });
  const asked = h('section', { class: 'dsec', 'aria-labelledby': 'd-asked' });
  const wait = h('section', { class: 'dsec', 'aria-labelledby': 'd-wait' });
  const qquote = h('blockquote', { class: 'dquote' });
  let activity; // {key, data} from the peek read
  let rules = null;
  // Stable containers: a re-render refills them but never moves the card,
  // so typing a deny reason or an "Other" answer keeps its focus.
  const headEl = h('header', { class: 'dhead' });
  const answerEl = h('div');
  const sessEl = h('div');

  function render() {
    const r = store.requests.get(id);
    if (!r) {
      fill(root, missing
        ? emptyState({ icon: 'hourglass', title: 'Request not found', text: 'It may be older than the retention window, or from before this daemon restarted.' })
        : skeleton('card', 1));
      return;
    }
    const s = store.sessions.get(r.session_id);
    const kind = r.checkpoint === 'question' ? 'question' : r.checkpoint === 'plan' ? 'plan' : 'permission';
    setTopTitle(kind === 'question' ? 'Questions' : kind === 'plan' ? 'Plan' : 'Decision');
    const k = kicker(r);
    const title = kind === 'plan' ? (markdownTitle(r.plan && r.plan.markdown) || 'Review a plan') : askTitle(r);
    const agent = agentName(r.agent);

    // ---- the sections ----
    fill(quote, r.context ? '“' + r.context + '”' : '');
    why.hidden = !r.context;
    fill(qquote, r.context ? '“' + r.context + '”' : '');
    qquote.hidden = !r.context;
    renderTurn(r, s);
    renderAsked(r, agent);
    renderWait(r, agent);

    // Built once per visit (opts are read on build only); later calls refresh it.
    const opts = { keep: true, slot: 'decision', variant: 'decision', headingLevel: 2, animate: false };
    if (kind === 'permission') {
      opts.after = [why, turn, asked, wait];
    } else {
      opts.before = [qquote];
      opts.after = [wait];
    }
    const cardEl = requestCard(r, opts);

    // ---- the answer, once there is one ----
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

    fill(headEl,
        h('div', { class: 'dkick ' + k.cls }, icon(k.icon, 'sm'), k.text),
        h('h1', { class: 'page-title dtitle' + (kind === 'question' ? ' small' : '') }, title),
        h('p', { class: 'dwho' },
          h('span', { class: 'ag ' + agentClass(r.agent), 'aria-hidden': 'true' }),
          h('b', null, repoName(s, r)),
          s && s.branch ? h('span', null, ' · ' + s.branch) : null,
          h('span', null, ' · ' + agent),
          h('span', { class: 'when' }, ' · asked ', h('time', { datetime: r.created_at }, agoLong(r.created_at)))));
    fill(answerEl, facts.length ? section('Answer', 0, h('div', { class: 'group' },
      h('dl', { class: 'kv answer-kv' }, facts.flatMap(([kk, v]) => [h('dt', null, kk), h('dd', null, v)])))) : null);
    fill(sessEl, s ? section('Session', 0, h('div', { class: 'list' },
        link('/s/' + s.id, { class: 'row', 'data-nav': '', 'data-session': s.id }, avatar(s),
          h('span', { class: 'row-main' },
            h('span', { class: 'row-title' }, h('span', { class: 't' }, repoName(s)), s.branch ? h('span', { class: 'branch' }, s.branch) : null),
            h('span', { class: 'row-sub' }, sessionLine(s))),
          icon('chev', 'sm', 'chev')))) : null);
    sync(root, headEl, cardEl, answerEl, sessEl);
    if (kind === 'permission' && s) loadTurn(s);
  }

  // ---- This turn: detected facts from the transcript ----
  function loadTurn(s) {
    if (!s.caps || !s.caps.activity) return;
    const key = s.id + '|' + s.updated_at;
    if (activity && (activity.key === key || activity.loading)) return;
    activity = { key, data: activity ? activity.data : undefined, loading: true };
    api('GET', `/api/sessions/${encodeURIComponent(s.id)}/activity?limit=1&peek=1`)
      .then((a) => { activity = { key, data: a || null, loading: false }; })
      .catch(() => { activity = { key, data: null, loading: false }; })
      .finally(() => render());
  }

  function renderTurn(r, s) {
    const a = activity && activity.data;
    const rc = a && a.receipt;
    turn.hidden = r.checkpoint !== 'permission' || !s || !s.caps || !s.caps.activity || (activity && !activity.loading && !rc);
    if (turn.hidden) return;
    if (!rc) { fill(turn, label('d-turn', 'This turn'), h('p', { class: 'dmuted' }, 'Reading what this turn did…')); return; }
    const rows = [];
    const check = (c, what, count) => {
      if (!c) return;
      const st = c.status === 'failed' ? 'bad' : c.status === 'running' ? 'run' : c.fresh ? 'ok' : 'warn';
      const head = c.status === 'failed' ? `${what} failed`
        : c.status === 'running' ? `${what} running`
          : !c.fresh ? `${what} ran before the last edit` : `${what} passed` + (count ? ` · ${count}` : '');
      rows.push(h('div', { class: 'chk ' + st },
        h('span', { class: 'st', 'aria-hidden': 'true' }, icon(st === 'ok' ? 'check' : st === 'bad' ? 'x' : st === 'run' ? 'hourglass' : 'alert', 'sm')),
        h('div', { class: 'cb' },
          h('div', { class: 'ch' }, h('b', null, head), h('time', { datetime: c.at }, agoLong(c.at))),
          h('span', { class: 'raw' }, c.command || c.name),
          c.failed_before ? h('span', { class: 'note' }, `Failed ${c.failed_before === 1 ? 'once' : c.failed_before + ' times'} before this run`) : null)));
    };
    check(rc.tests, 'Tests', rc.tests && rc.tests.count);
    check(rc.lint, 'Lint');
    check(rc.build, 'Build');
    if (!rc.tests) rows.push(h('div', { class: 'chk none' }, h('span', { class: 'st', 'aria-hidden': 'true' }, icon('flask', 'sm')), h('div', { class: 'cb' }, h('div', { class: 'ch' }, h('b', null, 'No tests ran this turn')))));
    const files = (a.files || []).slice(0, 6);
    const more = Math.max(0, (a.files || []).length - files.length);
    const sum = [];
    if (rc.since && !rc.since.startsWith('0001-')) sum.push(`${Math.max(1, Math.round((Date.now() - toMs(rc.since)) / 60000))} min`);
    sum.push(rc.files ? `${rc.files} ${plural(rc.files, 'file', 'files')} changed` : 'no files changed');
    if (rc.reads) sum.push(`${rc.reads} ${plural(rc.reads, 'read', 'reads')}`);
    if (rc.searches) sum.push(`${rc.searches} ${plural(rc.searches, 'search', 'searches')}`);
    if (rc.blocked) sum.push(`${rc.blocked} blocked`);
    const u = s && s.usage;
    if (u && u.cost_usd >= 0.01) sum.push(`≈ $${u.cost_usd.toFixed(2)} so far`);
    fill(turn,
      label('d-turn', 'This turn'),
      rc.commit ? h('div', { class: 'commit' }, icon('branch', 'sm'), h('span', { class: 'mono' }, rc.commit)) : null,
      files.length ? h('div', { class: 'files', 'aria-label': 'Files changed in this session' },
        h('p', { class: 'dmuted small' }, 'Files changed in this session'),
        files.map((f) => {
          const dir = f.path.includes('/') ? f.path.slice(0, f.path.lastIndexOf('/')) : '';
          return h('div', { class: 'frow' },
            icon(f.op === 'create' ? 'file' : f.op === 'delete' ? 'trash' : 'pencil', 'sm'),
            h('div', { class: 'fn' }, h('b', null, basename(f.path)), dir ? h('span', null, dir) : null),
            h('div', { class: 'nums' }, f.added ? h('span', { class: 'add' }, '+' + f.added) : null, f.removed ? h('span', { class: 'del' }, '−' + f.removed) : null));
        }),
        more ? h('p', { class: 'dmuted small' }, `and ${more} more`) : null) : null,
      h('div', { class: 'checks' }, rows),
      h('p', { class: 'dmuted' }, 'This turn: ' + sum.join(' · ')));
  }

  // ---- Why you're asked ----
  function renderAsked(r, agent) {
    asked.hidden = r.checkpoint !== 'permission';
    if (asked.hidden) return;
    if (!r.policy_rule) {
      fill(asked, label('d-asked', 'Why you’re asked'), h('p', null, `${agent} asked on its own: no rule allows this, so it needs a person.`));
      return;
    }
    const info = rules && rules.get(r.policy_rule);
    const where = info && info.source === 'user' ? ' in your policy.yaml' : info && info.source === 'repo' ? ' in this repo’s policy' : info && info.source === 'preset' ? ' from a preset' : info && info.source === 'managed' ? ' you made from Pilothouse' : info && info.source === 'builtin' ? ' built into Pilothouse' : '';
    fill(asked, label('d-asked', 'Why you’re asked'),
      h('p', null, 'Your rule ', link('/rules#rule-' + r.policy_rule, { class: 'rname' }, r.policy_rule), where,
        info && info.description ? [': ', info.description] : ' asked for a person.'));
    if (!rules) loadRules().then((m) => { rules = m; if (m.has(r.policy_rule)) render(); });
  }

  // ---- If you wait ----
  const RING = 2 * Math.PI * 22;
  const ringFg = svg('circle', { class: 'fg', cx: 26, cy: 26, r: 22 });
  const ringText = h('span', { class: 'rt' });
  const ring = h('div', { class: 'ring', 'aria-hidden': 'true' },
    svg('svg', { viewBox: '0 0 52 52' }, svg('circle', { class: 'bg', cx: 26, cy: 26, r: 22 }), ringFg), ringText);
  ringFg.style.strokeDasharray = String(RING);
  const waitText = h('p', { class: 'dmuted' });
  const waitLive = h('span', { class: 'sr-only', 'aria-live': 'off' });
  function renderWait(r, agent) {
    wait.hidden = r.status !== 'open' || !r.expires_at;
    if (wait.hidden) return;
    fill(waitText, `When this runs out, ${agent} shows its own prompt on your computer instead.`);
    if (!wait.firstChild) {
      fill(wait, label('d-wait', 'If you wait'),
        h('div', { class: 'wcard' }, ring, h('div', null, h('b', null, 'It goes back to the terminal'), waitText, waitLive)));
    }
    tickWait(r);
  }
  function tickWait(r = store.requests.get(id), now = Date.now()) {
    if (!r || r.status !== 'open' || !r.expires_at) return;
    const total = Math.max(1, toMs(r.expires_at) - toMs(r.created_at));
    const rem = Math.max(0, toMs(r.expires_at) - now);
    const s = Math.floor(rem / 1000);
    ringText.textContent = `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`;
    ringFg.style.strokeDashoffset = String(RING * (1 - rem / total));
    ring.classList.toggle('low', rem < 60000);
    waitLive.textContent = `${Math.ceil(rem / 60000)} minutes left`;
  }
  const clockTimer = setInterval(() => tickWait(), 1000);

  if (!store.requests.has(id)) {
    api('GET', '/api/requests/' + encodeURIComponent(id))
      .then((r) => { store.requests.set(r.id, r); render(); })
      .catch((e) => { missing = true; render(); if (e.status !== 404) toast(e.message, 'bad'); });
  }
  render();
  return {
    el: root,
    update: render,
    destroy() {
      clearInterval(clockTimer);
      pruneCards(new Set(), 'decision'); // the next visit builds a card with its own sections
    },
  };
}

function label(id, text) {
  return h('h2', { class: 'dlbl', id }, text);
}
