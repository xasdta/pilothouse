// Today: the day's log. Every live session on one screen, grouped by what
// it needs from you, one card per group:
//
//   Needs you        open requests, oldest first; the oldest permission
//                    request is open in place (exact command, risk, what
//                    this turn proved, Deny · Hold to allow · Details)
//   At the computer  prompts only the terminal can answer
//   Working          the step it is on now, in words, with the raw
//                    command and how long it has been at it
//   Finished         new since you looked: the agent's last words and the
//                    turn's receipt (tests, lint, build, +/−, blocked)
//   Idle             folded
//
// Receipts come from GET /api/sessions/:id/activity?peek=1 (peek: reading
// them is not "looking", so the session's catch-up stays put).

import { h, svg, icon, toMs, ago, clock, repoName, fill, add, sync, money, agentClass, agentName, basename, requestVerb, plural } from '../dom.js';
import { store, api, changed } from '../api.js';
import { requestCard, pruneCards, settling } from '../card.js';
import { link, go, isWide, skeleton, emptyState } from '../ui.js';
import { markdownTitle } from '../markdown.js';

const WORKING = new Set(['working', 'unknown']);
const DONE = new Set(['waiting', 'errored']);

// Kept across visits to Today, like a scroll position.
let openId = null; // the request open in place
const closedByYou = new Set(); // requests you folded: never auto-open again
let idleOpen = false;
let endedOpen = false;

const receipts = new Map(); // session id → {key, data, loading}
const proofs = new Map(); // request id → proof element owned here

export function todayView() {
  const root = h('div', { class: 'view log' });
  const counts = h('p', { class: 'lcounts' });
  const band = h('div', { class: 'lband' }, counts, bandArt());
  const body = h('div', { class: 'ledger' });
  const foot = h('div', { class: 'lfoot' });
  const day = new Date().toLocaleDateString([], { weekday: 'long' });
  // The top bar shows the title; this is the page's heading for screen readers.
  add(root, h('h1', { class: 'page-title sr-only' }, `${day}’s log`), band, body, foot);
  let today = null; // {cost, auto} from insights
  // Cached so a re-render never moves the open decision (it would lose
  // focus mid-typing a deny reason): sections and decision rows are reused.
  const sections = {};
  const decRows = new Map(); // request id → {el, toggle, nbody, inner}

  function render() {
    if (!store.loaded) {
      fill(counts, 'Connecting…');
      fill(body, skeleton('card', 2));
      return;
    }
    const wide = isWide();
    const reqs = [...store.requests.values()];
    const open = reqs.filter((r) => r.status === 'open' || settling.has(r.id))
      .sort((a, b) => toMs(a.created_at) - toMs(b.created_at));
    const stepped = reqs.filter((r) => r.status === 'stepped_aside')
      .sort((a, b) => toMs(b.created_at) - toMs(a.created_at));
    const live = [...store.sessions.values()].filter((s) => s.state !== 'exited');
    const asking = new Set([...open, ...stepped].map((r) => r.session_id));
    const desk = live.filter((s) => s.state === 'blocked' && !asking.has(s.id));
    const working = live.filter((s) => WORKING.has(s.state)).sort((a, b) => toMs(a.updated_at) - toMs(b.updated_at));
    const finished = live.filter((s) => DONE.has(s.state) && (s.unseen || s.state === 'errored'))
      .sort((a, b) => toMs(b.updated_at) - toMs(a.updated_at));
    const shown = new Set([...asking, ...desk.map((s) => s.id), ...working.map((s) => s.id), ...finished.map((s) => s.id)]);
    const idle = live.filter((s) => !shown.has(s.id)).sort((a, b) => toMs(b.updated_at) - toMs(a.updated_at));

    // The open decision: keep yours; else the oldest permission request.
    const perms = open.filter((r) => kindOf(r) === 'permission');
    if (wide) openId = null;
    else if (!perms.some((r) => r.id === openId)) {
      const next = perms.find((r) => r.status === 'open' && !closedByYou.has(r.id));
      openId = next ? next.id : null;
    }

    // The open decision leads; the rest by how soon they hand back.
    open.sort((a, b) => (b.id === openId) - (a.id === openId) || toMs(a.expires_at) - toMs(b.expires_at));
    const nOpen = open.filter((r) => r.status === 'open').length;
    fill(counts,
      h('span', { class: nOpen ? 'need' : '' }, h('b', null, String(nOpen)), ' need you'), ' · ',
      h('span', null, h('b', null, String(working.length)), ' working'), ' · ',
      h('span', null, h('b', null, String(finished.length)), ' finished'),
      today && today.cost != null ? [' · ', link('/insights', { class: 'cost', 'aria-label': `About ${money(today.cost)} spent today, see Insights` }, '≈ ', h('b', null, money(today.cost)))] : null);

    const groups = [];
    groups.push(section('need', 'Needs you', nOpen,
      open.length ? open.map((r) => needRow(r, wide)) : [clearCard()]));
    if (stepped.length || desk.length) {
      groups.push(section('desk', 'At the computer', stepped.length + desk.length,
        [...stepped.map(steppedRow), ...desk.map(deskRow)]));
    }
    if (working.length) groups.push(section('work', 'Working', working.length, working.map(workRow)));
    if (finished.length) groups.push(section('fin', 'Finished', finished.length, finished.map(doneRow)));
    if (idle.length) groups.push(foldGroup('idle', idle, `${idle.length} idle`, 'nothing new since you looked'));
    // Sessions that ended today, folded: the rest of what the Sessions tab showed.
    const dayStart = new Date(); dayStart.setHours(0, 0, 0, 0);
    const ended = [...store.sessions.values()].filter((s) => s.state === 'exited' && toMs(s.exited_at || s.updated_at) >= dayStart.getTime())
      .sort((a, b) => toMs(b.exited_at || b.updated_at) - toMs(a.exited_at || a.updated_at));
    if (ended.length) groups.push(foldGroup('ended', ended, `${ended.length} ended today`, 'closed or finished for good'));
    if (!live.length && !open.length) groups.push(sections.none || (sections.none = noAgents()));
    sync(body, ...groups);
    const keep = new Set(open.map((r) => r.id));
    pruneCards(new Set(open.filter((r) => r.id === openId).map((r) => r.id)), 'ledger');
    for (const id of proofs.keys()) if (!keep.has(id)) proofs.delete(id);
    for (const id of decRows.keys()) if (!keep.has(id)) decRows.delete(id);

    // Receipts for the rows that show them.
    const want = [...finished];
    const o = open.find((r) => r.id === openId);
    if (o && store.sessions.get(o.session_id)) want.push(store.sessions.get(o.session_id));
    for (const s of want) loadReceipt(s);

    const away = store.presence && store.presence.state === 'away';
    fill(foot, icon('wheel', 'lg'),
      h('p', null, h('b', null, away ? 'Away' : 'At your desk'), away
        ? ' · requests come to this phone.' : ' · agents ask in the terminal; switch to Away to answer here.',
      today && today.auto ? [' Your rules allowed ', h('b', null, String(today.auto)), ` routine ${plural(today.auto, 'step', 'steps')} today.`] : null),
      h('span', null, `End of today’s log · ${live.length} ${plural(live.length, 'session', 'sessions')}`));
    tickClocks(root);
  }

  // ---- rows ----

  function needRow(r, wide) {
    const s = store.sessions.get(r.session_id);
    const kind = kindOf(r);
    const isOpen = r.id === openId;
    const head = [
      mark('need', kind === 'question' ? 'question' : kind === 'plan' ? 'plan' : 'clock', r.agent),
      h('span', { class: 'lb' },
        h('span', { class: 'l1' }, h('b', null, repoName(s, r)), h('em', null, askTitle(r)),
          kind === 'permission' && !wide ? icon('chevDown', 'sm', 'cv') : icon('chev', 'sm', 'cv')),
        h('span', { class: 'l2' }, needDetail(r, s, isOpen))),
    ];
    if (kind !== 'permission' || wide) {
      return h('div', { class: 'nrow' }, link('/r/' + r.id, { class: 'lrow', 'data-nav': '', 'data-request': r.id }, ...head));
    }
    let row = decRows.get(r.id);
    if (!row) {
      const bodyId = 'dec-' + r.id;
      const toggle = h('button', {
        class: 'lrow', type: 'button', 'aria-controls': bodyId, 'data-nav': '', 'data-request': r.id,
        onclick: () => {
          if (openId === r.id) { closedByYou.add(r.id); openId = null; } else { openId = r.id; closedByYou.delete(r.id); }
          render();
        },
      });
      const inner = h('div', { class: 'nin' });
      const nbody = h('div', { class: 'nbody', id: bodyId }, inner);
      row = { el: h('div', { class: 'nrow perm' }, toggle, nbody), toggle, nbody, inner };
      decRows.set(r.id, row);
    }
    fill(row.toggle, ...head);
    row.toggle.setAttribute('aria-expanded', String(isOpen));
    row.el.classList.toggle('open', isOpen);
    row.nbody.inert = !isOpen;
    if (isOpen) {
      if (!proofs.has(r.id)) proofs.set(r.id, h('div', { class: 'thisturn', 'aria-label': 'What this turn did' }));
      renderProof(proofs.get(r.id), r);
      const card = requestCard(r, { slot: 'ledger', variant: 'ledger', proof: proofs.get(r.id), animate: false });
      if (card.parentNode !== row.inner) fill(row.inner, card);
    }
    return row.el;
  }

  /** A cached group card: a header with its count, then its rows. */
  function section(cls, title, n, rows) {
    let g = sections[cls];
    if (!g) {
      const id = 'g-' + cls;
      const count = h('b');
      const list = h('div', { class: 'lg' });
      g = { el: h('section', { class: 'lgroup g-' + cls, 'aria-labelledby': id }, h('h2', { class: 'gh ' + cls, id }, title, count), list), count, list };
      sections[cls] = g;
    }
    g.count.textContent = String(n);
    sync(g.list, rows);
    return g.el;
  }

  function steppedRow(r) {
    const s = store.sessions.get(r.session_id);
    return h('div', { class: 'nrow' },
      link('/r/' + r.id, { class: 'lrow', 'data-nav': '' },
        mark('desk', 'monitor', r.agent),
        h('span', { class: 'lb' },
          h('span', { class: 'l1' }, h('b', null, repoName(s, r)), h('em', null, 'Asked in its terminal'),
            h('time', { 'data-ago': r.created_at }, ago(r.created_at)), icon('chev', 'sm', 'cv')),
          h('span', { class: 'l2 mono' }, r.command || r.summary || r.tool))));
  }

  function deskRow(s) {
    const what = s.block_kind === 'auth' ? 'Needs you to sign in' : s.block_kind === 'question' ? 'Asking a question in its terminal' : 'Asking in its terminal';
    return h('div', { class: 'nrow' },
      link('/s/' + s.id, { class: 'lrow', 'data-nav': '', 'data-session': s.id },
        mark('desk', 'monitor', s.agent),
        h('span', { class: 'lb' },
          h('span', { class: 'l1' }, h('b', null, repoName(s)), h('em', null, what),
            h('time', { 'data-ago': s.updated_at }, ago(s.updated_at)), icon('chev', 'sm', 'cv')),
          h('span', { class: 'l2' }, 'answer it on your computer'))));
  }

  function workRow(s) {
    const st = s.state === 'unknown'
      ? { label: 'Quiet for ' + ago(s.updated_at).replace(/^now$/, 'a moment'), raw: s.state_detail || '' }
      : plainStep(s.state_detail);
    return h('div', { class: 'nrow' },
      link('/s/' + s.id, { class: 'lrow', 'data-nav': '', 'data-session': s.id },
        mark(s.state === 'unknown' ? 'quiet' : 'work', null, s.agent),
        h('span', { class: 'lb' },
          h('span', { class: 'l1' }, h('b', null, repoName(s)), h('em', null, st.label),
            h('time', { class: 'el', 'data-since': s.updated_at, 'aria-label': 'on this step for' }, elapsed(s.updated_at))),
          h('span', { class: 'l2' + (st.mono ? ' mono' : '') }, st.raw || s.branch || agentName(s.agent)))));
  }

  function doneRow(s) {
    const failed = s.state === 'errored';
    const said = failed ? (s.state_detail || 'Stopped with an error') : (s.last_message || s.state_detail || 'Finished its turn');
    return h('div', { class: 'nrow' },
      link('/s/' + s.id, { class: 'lrow' + (s.unseen ? ' unread' : ''), 'data-nav': '', 'data-session': s.id },
        mark(failed ? 'bad' : 'fin', failed ? 'alert' : 'check', s.agent),
        h('span', { class: 'lb' },
          h('span', { class: 'l1' }, h('b', null, repoName(s)),
            s.unseen ? h('span', { class: 'new', title: 'New since you last looked' }, h('span', { class: 'sr-only' }, 'New')) : null,
            h('em', null, oneLine(said)),
            h('time', { datetime: s.updated_at, title: clock(s.updated_at), 'aria-label': 'at ' + clock(s.updated_at) }, clock(s.updated_at).replace(/\s?[AP]\.?M\.?$/i, '')),
            icon('chev', 'sm', 'cv')),
          h('span', { class: 'l2' }, failed ? h('span', { class: 'bad' }, 'Error') : receiptLine(s)))));
  }

  /** A folded group (idle, ended today): one row that opens to a short list. */
  function foldGroup(kind, list, title, sub) {
    const open = kind === 'idle' ? idleOpen : endedOpen;
    const rows = h('div', { class: 'idlelist', hidden: !open },
      list.map((s) => link('/s/' + s.id, { class: 'irow', 'data-session': s.id },
        h('span', { class: 'ag ' + agentClass(s.agent), 'aria-hidden': 'true' }),
        h('b', null, repoName(s)), s.branch ? h('span', { class: 'br' }, s.branch) : null,
        h('span', { class: 'st' }, s.state === 'starting' ? 'Ready' : ''),
        h('time', { 'data-ago': kind === 'ended' ? (s.exited_at || s.updated_at) : s.updated_at }, ago(kind === 'ended' ? (s.exited_at || s.updated_at) : s.updated_at)))));
    const btn = h('button', {
      class: 'lrow lidle', type: 'button', 'aria-expanded': String(open),
      onclick: () => { if (kind === 'idle') idleOpen = !idleOpen; else endedOpen = !endedOpen; render(); },
    }, mark(kind === 'ended' ? 'ended' : 'idle', kind === 'ended' ? 'check' : null, null),
    h('span', { class: 'lb' }, h('span', { class: 'l1' }, h('b', null, title), h('em', null, sub), icon('chevDown', 'sm', 'cv'))));
    let g = sections[kind];
    if (!g) {
      const lst = h('div', { class: 'lg' });
      g = { el: h('section', { class: 'lgroup lg-idle', 'aria-label': kind === 'idle' ? 'Idle sessions' : 'Sessions that ended today' }, lst), list: lst };
      sections[kind] = g;
    }
    fill(g.list, btn, rows);
    return g.el;
  }

  function clearCard() {
    return h('div', { class: 'lclear' }, boatArt(),
      h('div', null, h('b', null, 'All clear.'),
        h('span', null, store.presence && store.presence.state === 'away'
          ? 'Nothing on this phone needs you.'
          : 'You’re at your desk, so agents ask in the terminal.')));
  }

  // ---- receipts ----

  function loadReceipt(s) {
    if (!s.caps || !s.caps.activity) return;
    const key = s.updated_at;
    const have = receipts.get(s.id);
    if (have && (have.key === key || have.loading)) return;
    receipts.set(s.id, { key, data: have ? have.data : undefined, loading: true });
    api('GET', `/api/sessions/${encodeURIComponent(s.id)}/activity?limit=1&peek=1`)
      .then((a) => receipts.set(s.id, { key, data: (a && a.receipt) || null, loading: false }))
      .catch(() => receipts.set(s.id, { key, data: null, loading: false }))
      .finally(() => changed());
  }

  function receiptLine(s) {
    const rc = (receipts.get(s.id) || {}).data;
    if (!rc) return s.branch || agentName(s.agent);
    const parts = [];
    const t = rc.tests;
    if (t) {
      if (t.status === 'failed') parts.push(h('span', { class: 'bad' }, '✗ tests'));
      else if (t.status === 'running') parts.push(h('span', null, 'tests running'));
      else if (!t.fresh) parts.push(h('span', { class: 'warn' }, 'tests before last edit'));
      else parts.push(h('span', { class: 'ok' }, '✓ ' + (t.count ? `${t.count} tests` : 'tests')));
    }
    for (const [k, label] of [['lint', 'lint'], ['build', 'build']]) {
      const c = rc[k];
      if (!c || c.status === 'running') continue;
      parts.push(c.status === 'failed' ? h('span', { class: 'bad' }, '✗ ' + label) : h('span', { class: c.fresh ? 'ok' : 'warn' }, '✓ ' + label));
    }
    if (rc.files) parts.push(h('span', null, h('span', { class: 'add' }, '+' + rc.added), ' ', h('span', { class: 'del' }, '−' + rc.removed)));
    else if (rc.searches || rc.reads) {
      parts.push(h('span', null, 'read only'));
      const looked = [rc.searches ? `${rc.searches} ${plural(rc.searches, 'search', 'searches')}` : '', rc.reads ? `${rc.reads} ${plural(rc.reads, 'read', 'reads')}` : ''].filter(Boolean).join(', ');
      parts.push(h('span', null, looked));
      parts.push(h('span', null, '0 files'));
    }
    if (rc.blocked) parts.push(h('span', { class: 'bad' }, `${rc.blocked} blocked`));
    if (!parts.length) return s.branch || agentName(s.agent);
    return parts.flatMap((p, i) => (i ? [' · ', p] : [p]));
  }

  function renderProof(el, r) {
    const s = store.sessions.get(r.session_id);
    const got = s && receipts.get(s.id);
    const rc = got && got.data;
    el.hidden = !s || !s.caps || !s.caps.activity || (got && !got.loading && !rc);
    if (el.hidden) return;
    if (!rc) { fill(el, h('div', { class: 'muted' }, 'Reading this turn…')); return; }
    fillProof(el, rc);
  }

  // ---- today's numbers ----

  let todayTimer = 0;
  async function loadToday() {
    try {
      const ins = await api('GET', '/api/insights?days=7');
      const d = ins.days && ins.days[ins.days.length - 1];
      today = d ? { cost: d.usage ? d.usage.cost_usd : null, auto: d.auto_allowed || 0 } : null;
    } catch {
      today = null; // no insights on this daemon
    }
    render();
    clearTimeout(todayTimer);
    todayTimer = setTimeout(loadToday, 60000);
  }

  const clocks = setInterval(() => tickClocks(root), 1000);
  render();
  loadToday();
  return { el: root, update: render, destroy() { clearTimeout(todayTimer); clearInterval(clocks); } };
}


/** Fills a "this turn" box from a receipt: the tests line, then files and commit. */
export function fillProof(el, rc) {
  const t = rc.tests;
  el.className = 'thisturn' + (t && t.status === 'failed' ? ' is-bad' : t && t.status === 'passed' && !t.fresh ? ' is-warn' : '');
  let tests;
  if (!t) tests = h('div', { class: 'muted' }, icon('flask', 'sm'), h('span', null, 'No tests ran this turn'));
  else if (t.status === 'failed') tests = h('div', { class: 'bad' }, icon('x', 'sm'), h('span', null, h('b', null, 'Tests failed'), ' · ', h('span', { class: 'mono' }, t.name)));
  else if (t.status === 'running') tests = h('div', null, icon('hourglass', 'sm'), h('span', null, h('b', null, 'Tests running'), ' · ', h('span', { class: 'mono' }, t.name)));
  else if (!t.fresh) tests = h('div', { class: 'warn' }, icon('alert', 'sm'), h('span', null, h('b', null, 'Tests ran before the last edit'), ' · ', h('span', { class: 'mono' }, t.name)));
  else tests = h('div', { class: 'one', title: t.count ? `${t.count} tests passed` : null }, icon('check', 'sm'), h('span', { class: 't' }, h('b', null, 'Tests passed'), ' after the last edit · '), h('span', { class: 'mono' }, t.name));
  const changes = rc.files
    ? [`${rc.files} ${plural(rc.files, 'file', 'files')} `, h('span', { class: 'add' }, '+' + rc.added), ' ', h('span', { class: 'del' }, '−' + rc.removed)]
    : ['No files changed'];
  fill(el, tests, h('div', null, icon('branch', 'sm'), h('span', null, ...changes, rc.commit ? [' · commit “', rc.commit, '”'] : null)));
}

// ---------- pieces ----------

/** The status mark: a state icon with the agent's colour as a badge. */
function mark(cls, ic, agent) {
  return h('span', { class: 'mk ' + cls, 'aria-hidden': 'true' },
    h('i', null, ic ? icon(ic, 'sm') : null),
    agent ? h('span', { class: 'ag ' + agentClass(agent) }) : null);
}

function kindOf(r) {
  return r.checkpoint === 'question' ? 'question' : r.checkpoint === 'plan' ? 'plan' : 'permission';
}

/** What a request asks, as a question a person would ask. */
export function askTitle(r) {
  const kind = kindOf(r);
  if (kind === 'question') {
    const qs = r.questions || [];
    if (qs.length === 1) return oneLine(qs[0].question || 'A question');
    const topic = qs[0] && (qs[0].header || '');
    return `${qs.length} questions` + (topic ? ` about ${topic.toLowerCase()}` : '');
  }
  if (kind === 'plan') {
    const t = markdownTitle(r.plan && r.plan.markdown);
    return t ? `Review plan: ${t}` : 'Review a plan';
  }
  const risky = (code) => (r.risk || []).some((x) => x.code === code);
  if (r.tool_kind === 'shell' && r.command) {
    const c = r.command.trim();
    const push = c.match(/\bgit\s+push\b(?:\s+(?:-\S+\s+)*\S+\s+(?:\S+:)?([\w./-]+))?/);
    if (push) return push[1] ? `Push to ${push[1]}?` : 'Push?';
    if (/\bgit\s+commit\b/.test(c)) return 'Commit?';
    if (/\b(?:npm|pnpm|yarn)\s+publish\b|\bcargo\s+publish\b/.test(c)) return 'Publish a package?';
    if (risky('deploy')) return 'Deploy?';
    if (/\brm\s+-\w*r/.test(c) || risky('destructive')) return 'Delete files?';
    if (/\b(?:npm|pnpm|yarn|bun)\s+(?:i|install|add)\b|\bpip3?\s+install\b|\bbrew\s+install\b|\bgo\s+get\b|\bcargo\s+add\b/.test(c)) return 'Install packages?';
    if (risky('sudo')) return 'Run as root?';
    const prog = basename(c.replace(/^(?:sudo|env|time|npx|pnpx|bunx)\s+/, '').split(/\s+/)[0] || 'it');
    return `Run ${prog}?`;
  }
  if (r.tool_kind === 'edit') {
    if (risky('deploy') || risky('protected_branch')) return 'Edit a production file?';
    const p = (r.preview && r.preview.files && r.preview.files[0] && r.preview.files[0].path) || (r.paths && r.paths[0]);
    const n = (r.preview && r.preview.files && r.preview.files.length) || (r.paths && r.paths.length) || 1;
    return n > 1 ? `Edit ${n} files?` : p ? `Edit ${basename(p)}?` : 'Edit a file?';
  }
  if (r.tool_kind === 'web') return 'Fetch from the web?';
  return requestVerb(r).replace(/\.$/, '') + '?';
}

function needDetail(r, s, isOpen) {
  const kind = kindOf(r);
  const bits = [];
  if (s && s.branch) bits.push(s.branch);
  else bits.push(agentName(r.agent));
  if (kind === 'question') {
    const q = r.questions && r.questions[0];
    if (r.questions && r.questions.length > 1 && q) bits.push(oneLine(q.question));
  } else if (kind === 'plan') {
    const steps = ((r.plan && r.plan.markdown) || '').split('\n').filter((l) => /^\s*(?:\d+[.)]|[-*])\s+\S/.test(l)).length;
    if (steps) bits.push(`${steps} steps`);
  } else if (r.preview && r.preview.files && r.preview.files.length) {
    let a = 0, d = 0;
    for (const f of r.preview.files) { a += f.added || 0; d += f.removed || 0; }
    bits.push(h('span', null, h('span', { class: 'add' }, '+' + a), ' ', h('span', { class: 'del' }, '−' + d)));
  } else if (!isOpen && r.command) {
    bits.push(h('span', { class: 'mono' }, oneLine(r.command)));
  }
  if (r.status === 'open' && r.expires_at) bits.push(h('span', { 'data-expires': r.expires_at }, terminalIn(r.expires_at)));
  return bits.flatMap((b, i) => (i ? [' · ', b] : [b]));
}

function terminalIn(t, now = Date.now()) {
  const rem = toMs(t) - now;
  if (rem <= 0) return 'handing back…';
  if (rem < 60000) return 'terminal in under a minute';
  return `terminal in ${Math.ceil(rem / 60000)} min`;
}

/** A state detail ("Run: go test ./...") as a plain step and its raw form. */
export function plainStep(detail) {
  const d = (detail || '').trim();
  const m = d.replace(/^(?:Running|Ran)\b:?/, 'Run:').replace(/^Editing\b:?/, 'Edit:').replace(/^Reading\b:?/, 'Read:')
    .match(/^(Run|Edit|Write|Read|Search|Fetch|Use|Using|Used)\b:?\s*(.*)$/);
  if (d.startsWith('MCP ')) {
    const server = d.slice(4).split(':')[0];
    return { label: `Using ${server}`, raw: d.slice(4), mono: false };
  }
  if (!m) {
    if (/^Working on a prompt$/i.test(d) || /^Working$/i.test(d) || !d) return { label: 'Thinking', raw: 'working on your prompt' };
    return { label: oneLine(d), raw: '' };
  }
  const [, verb, rest] = m;
  switch (verb) {
    case 'Run': return { label: commandLabel(rest), raw: rest, mono: true };
    case 'Edit': case 'Write': return { label: `Editing ${basename(rest)}`, raw: rest, mono: true };
    case 'Read': return { label: `Reading ${basename(rest)}`, raw: rest, mono: true };
    case 'Search': return { label: 'Searching the code', raw: rest, mono: true };
    case 'Fetch': return { label: 'Reading the web', raw: rest, mono: true };
    default: return { label: `Using ${rest || 'a tool'}`, raw: '' };
  }
}

export function commandLabel(c) {
  const rules = [
    [/(?:^|[\s;&|(])(?:(?:npm|pnpm|yarn|bun)(?:\s+run)?\s+test|go\s+test|cargo\s+(?:test|nextest)|pytest|jest|vitest|mocha|rspec|make\s+test|dotnet\s+test|mix\s+test|deno\s+test|swift\s+test)\b/, 'Running the tests'],
    [/(?:^|[\s;&|(])(?:(?:npm|pnpm|yarn|bun)(?:\s+run)?\s+(?:lint|typecheck)|eslint|golangci-lint|staticcheck|go\s+vet|ruff|mypy|tsc\s+.*--noEmit|cargo\s+clippy)\b/, 'Checking the code'],
    [/(?:^|[\s;&|(])(?:(?:npm|pnpm|yarn|bun)(?:\s+run)?\s+build|go\s+build|cargo\s+build|vite\s+build|next\s+build|make\s+build|docker\s+build)\b/, 'Building'],
    [/\bgit\s+push\b/, 'Pushing'],
    [/\bgit\s+commit\b/, 'Committing'],
    [/\bgit\s+(?:status|diff|log|show|branch)\b/, 'Checking git'],
    [/\b(?:npm|pnpm|yarn|bun)\s+(?:i|install|add)\b|\bpip3?\s+install\b|\bgo\s+(?:get|mod\s+tidy)\b/, 'Installing packages'],
    [/^\s*(?:cd\s+\S+\s*(?:&&|;)\s*)?(?:grep|rg|ag|find|fd)\b/, 'Searching the code'],
    [/^\s*(?:cd\s+\S+\s*(?:&&|;)\s*)?(?:cat|head|tail|sed\s+-n|less|wc)\b/, 'Reading files'],
    [/^\s*(?:cd\s+\S+\s*(?:&&|;)\s*)?(?:ls|tree)\b/, 'Looking around'],
    [/\bcurl\b|\bwget\b/, 'Fetching a URL'],
  ];
  for (const [re, label] of rules) if (re.test(c)) return label;
  return 'Running a command';
}

function oneLine(s) {
  return String(s || '').replace(/\s+/g, ' ').trim();
}

function elapsed(t, now = Date.now()) {
  const s = Math.max(0, Math.floor((now - toMs(t)) / 1000));
  if (s >= 3600) return Math.floor(s / 3600) + 'h ' + String(Math.floor((s % 3600) / 60)).padStart(2, '0') + 'm';
  return Math.floor(s / 60) + ':' + String(s % 60).padStart(2, '0');
}

function tickClocks(root) {
  const now = Date.now();
  for (const el of root.querySelectorAll('[data-since]')) el.textContent = elapsed(el.getAttribute('data-since'), now);
  for (const el of root.querySelectorAll('[data-ago]')) el.textContent = ago(el.getAttribute('data-ago'), now);
  for (const el of root.querySelectorAll('[data-expires]')) el.textContent = terminalIn(el.getAttribute('data-expires'), now);
}

function noAgents() {
  return h('div', null, emptyState({
    icon: 'agent',
    title: 'No agents yet',
    text: 'Start Claude Code, Codex or OpenCode in any terminal on your computer — Pilothouse sees them on its own.',
    teach: [
      ['terminal', 'Run an agent with pilothouse run (for example pilothouse run claude) to reply to it and watch its terminal from here.'],
      ['plus', 'Or start a task from this device: it gets its own worktree and branch.'],
    ],
    actions: [h('button', { class: 'btn primary small', type: 'button', onclick: () => go('/new') }, icon('plus', 'sm'), 'New task')],
  }));
}

// ---------- artwork (inline SVG, coloured by CSS, so it follows the theme) ----------

function waves(y, step, n) {
  let d = `M0 ${y}`;
  for (let i = 0; i < n; i++) d += ` q${step / 2} -${step / 5} ${step} 0`;
  return svg('g', { class: 'a-waves' }, svg('path', { d }));
}

function bandArt() {
  return svg('svg', { viewBox: '0 0 390 40', preserveAspectRatio: 'xMaxYMax slice', class: 'art band-art', 'aria-hidden': 'true', focusable: 'false' },
    svg('defs', null, svg('linearGradient', { id: 'lbeamg', x1: 1, y1: 0, x2: 0, y2: 0 },
      svg('stop', { class: 'a-s1', offset: 0 }), svg('stop', { class: 'a-s2', offset: 1 }))),
    svg('g', { class: 'a-beam' }, svg('polygon', { class: 'a-beamfill', points: '352,12 150,-8 150,28 352,14' })),
    svg('rect', { class: 'a-sea', y: 31, width: 390, height: 9 }),
    svg('line', { class: 'a-line', x1: 0, y1: 31, x2: 390, y2: 31 }),
    waves(35, 12, 36),
    svg('path', { class: 'a-rock', d: 'M330 33c3-5 9-7 14-5 4-4 11-5 16-1 5-2 11 0 14 5 4 0 7 3 8 6h-55c0-2 1-4 3-5z' }),
    svg('polygon', { class: 'a-tower', points: '345,32 359,32 357,15 347,15' }),
    svg('polygon', { class: 'a-stripe', points: '345.4,28 358.6,28 358.3,25 345.7,25' }),
    svg('polygon', { class: 'a-stripe', points: '346.2,21 357.8,21 357.5,18 346.5,18' }),
    svg('rect', { class: 'a-tower', x: 345.5, y: 12.5, width: 13, height: 2.5 }),
    svg('circle', { class: 'a-glow', cx: 352, cy: 9, r: 6 }),
    svg('rect', { class: 'a-lamp', x: 348.5, y: 5.5, width: 7, height: 7 }),
    svg('polygon', { class: 'a-roof', points: '347,5.5 352,1 357,5.5' }));
}

function boatArt() {
  return svg('svg', { viewBox: '0 0 80 40', class: 'art boat-art', 'aria-hidden': 'true', focusable: 'false' },
    svg('line', { class: 'a-line', x1: 0, y1: 31, x2: 80, y2: 31 }),
    svg('g', { class: 'a-boat' },
      svg('path', { class: 'a-hull', d: 'M27 29 L53 29 L49 34 L31 34 Z' }),
      svg('path', { class: 'a-mast', d: 'M40 29 V8' }),
      svg('path', { class: 'a-sail', d: 'M41 9 L41 27 L52 27 Z' }),
      svg('path', { class: 'a-sail2', d: 'M39 12 L39 27 L31 27 Z' })),
    waves(36, 10, 10));
}
