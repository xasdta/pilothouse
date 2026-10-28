// Request cards: what the agent wants and the buttons to answer it.
//
//   permission  highlighted command / file diff, risk labels, the rule that
//               fired, the agent's last words; Deny · Allow for session · Allow;
//               "Make it a rule" (suggestions → one signed tap)
//   question    options as radio/checkbox chips (multi-select, "Other…" free
//               text); Decline · Send answer
//   plan        rendered Markdown; Request changes (with feedback) · Approve
//
// Every answer is a signed v2 action (PilothouseKeys.respond). Answers are
// optimistic: the card shows the outcome at once and rolls back if the
// daemon refuses (409 = answered elsewhere: the card shows who did). A
// card disables the moment its request closes anywhere, in every place
// it is shown.
//
// Exported API (stable; workstream I shows cards in the session view):
//   requestCard(req, opts?) → element (cached per request and slot; call again to refresh)
//     opts.slot     'main' (default) | any name: separate instance (desktop queue uses 'queue')
//     opts.keep     stay after answering (request page)
//     opts.animate  false: no entrance animation
//     opts.headingLevel  level for headings inside a plan (default 3: under a section h2)
//   pruneCards(visibleIds, slot?)   forget cards no longer shown
//   settling                        Set of request ids animating out after an answer here
//   controllerFor(el) → {id, allow(), allowSession(), deny(), focus(), key(k)} | null
//   commandView(cmd) → <pre class="code"> with shell highlighting
//   highlightShell(cmd) → nodes
//   openRuleSheet(req)              the "Make it a rule" sheet

import { h, icon, agentBadge, agentClass, agentName, repoName, ago, left, toMs, requestVerb, grantPrefix, vibrate, relPath, fill, add, richText, uid } from './dom.js';
import { store, api, changed, signed } from './api.js';
import { toast, openSheet, go, link, skeleton } from './ui.js';
import { diffView } from './diff.js';
import { closeRequestNotifications } from './push.js';
import { renderMarkdown, markdownTitle } from './markdown.js';

const cards = new Map(); // `${slot}|${id}` → controller
const byCard = new WeakMap(); // <article> → controller
/** Requests answered here whose cards are still animating out. */
export const settling = new Set();
/** Answers on their way to the daemon: request id → optimistic outcome. */
const inflight = new Map();

/** Returns the (cached) card element for a request, refreshed. */
export function requestCard(req, opts = {}) {
  const slot = opts.slot || 'main';
  const key = slot + '|' + req.id;
  let c = cards.get(key);
  if (!c) {
    c = build(req, { ...opts, slot });
    cards.set(key, c);
  }
  c.update(req);
  return c.wrap;
}

/** Forget cards for requests no longer shown (keeps memory bounded). */
export function pruneCards(visibleIds, slot = 'main') {
  for (const [key, c] of cards) {
    if (!key.startsWith(slot + '|')) continue;
    if (!visibleIds.has(c.id) && !settling.has(c.id)) cards.delete(key);
  }
}

/** The controller of the card that contains el. */
export function controllerFor(el) {
  const card = el && el.closest ? el.closest('article.req') : null;
  return card ? byCard.get(card) || null : null;
}

// ---------- Shell highlighting ----------

const OPS = ['&&', '||', '>>', '2>&1', '2>', '&>', '|&', '|', ';', '>', '<', '&'];
const PREFIX_CMDS = new Set(['sudo', 'env', 'time', 'nohup', 'exec', 'xargs', 'nice', 'command', 'npx', 'pnpx', 'bunx']);

/** Shell command as highlighted nodes (command words, flags, strings, operators, variables). */
export function highlightShell(cmd) {
  const out = [];
  const s = String(cmd || '');
  let i = 0;
  let expectCmd = true;
  const push = (cls, text) => out.push(cls ? h('span', { class: 'hl-' + cls }, text) : text);
  while (i < s.length) {
    const c = s[i];
    if (c === ' ' || c === '\t' || c === '\n') {
      let j = i;
      while (j < s.length && /\s/.test(s[j])) j++;
      push('', s.slice(i, j));
      i = j;
      continue;
    }
    const op = OPS.find((o) => s.startsWith(o, i));
    if (op) {
      push('op', op);
      i += op.length;
      expectCmd = op !== '>' && op !== '>>' && op !== '<' && op !== '2>' && op !== '&>';
      continue;
    }
    if (c === '#' && (i === 0 || /\s/.test(s[i - 1]))) {
      push('comment', s.slice(i));
      break;
    }
    // A word, with quotes and $vars inside it.
    let j = i;
    const parts = [];
    let plain = '';
    const flushPlain = () => { if (plain) { parts.push(['', plain]); plain = ''; } };
    while (j < s.length && !/\s/.test(s[j]) && !OPS.some((o) => s.startsWith(o, j))) {
      const ch = s[j];
      if (ch === '"' || ch === "'") {
        flushPlain();
        let k = j + 1;
        while (k < s.length && s[k] !== ch) k += s[k] === '\\' && ch === '"' ? 2 : 1;
        parts.push(['str', s.slice(j, Math.min(k + 1, s.length))]);
        j = k + 1;
        continue;
      }
      if (ch === '$') {
        flushPlain();
        const m = /^\$(\{[^}]*\}|\([^)]*\)|[A-Za-z_][A-Za-z0-9_]*|[0-9#?@*$!-])/.exec(s.slice(j));
        if (m) { parts.push(['var', m[0]]); j += m[0].length; continue; }
      }
      plain += ch;
      j++;
    }
    flushPlain();
    const word = s.slice(i, j);
    if (parts.length === 1 && parts[0][0] === '') {
      let cls = '';
      if (expectCmd && /^[A-Za-z_][A-Za-z0-9_]*=/.test(word)) cls = 'var';
      else if (expectCmd) { cls = 'cmd'; expectCmd = PREFIX_CMDS.has(word); }
      else if (word.startsWith('-')) cls = 'flag';
      else if (/^(\.{0,2}\/|~)/.test(word) || /\/[\w.-]/.test(word)) cls = 'path';
      push(cls, word);
    } else {
      if (expectCmd) expectCmd = false;
      for (const [cls, t] of parts) push(cls, t);
    }
    i = j;
  }
  return out;
}

/** <pre class="code">$ command</pre>, highlighted. */
export function commandView(cmd) {
  return h('pre', { class: 'code', tabindex: '0', 'aria-label': 'Command' },
    h('span', { class: 'prompt', 'aria-hidden': 'true' }, '$'), highlightShell(cmd));
}

// ---------- Pieces ----------

function inputField(req, ...keys) {
  if (!req.input || typeof req.input !== 'object') return '';
  for (const k of keys) if (typeof req.input[k] === 'string') return req.input[k];
  return '';
}

function pathsBlock(req) {
  const pre = h('pre', { class: 'code', tabindex: '0', 'aria-label': 'Files' });
  req.paths.slice(0, 6).forEach((p, i) => {
    const r = relPath(p, req.cwd);
    const cut = r.lastIndexOf('/');
    if (i) add(pre, '\n');
    if (cut > 0) add(pre, h('span', { class: 'path-dir' }, r.slice(0, cut + 1)), r.slice(cut + 1));
    else add(pre, r);
  });
  if (req.paths.length > 6) add(pre, '\n', h('span', { class: 'path-dir' }, `+ ${req.paths.length - 6} more`));
  return pre;
}

function details(req) {
  if (req.tool_kind === 'shell' && req.command) return commandView(req.command);
  if (req.preview && req.preview.files && req.preview.files.length) return diffView(req.preview, { cwd: req.cwd, maxLines: 24 });
  if (req.paths && req.paths.length) return pathsBlock(req);
  if (req.tool_kind === 'web') return h('pre', { class: 'code', tabindex: '0' }, inputField(req, 'url', 'query') || req.summary || req.tool);
  let text = req.summary || req.tool;
  if (req.input && typeof req.input === 'object' && Object.keys(req.input).length) {
    try { text = JSON.stringify(req.input, null, 2); } catch { /* keep the summary */ }
    if (text.length > 900) text = text.slice(0, 900) + '\n…';
  }
  return h('pre', { class: 'code', tabindex: '0', 'aria-label': 'Tool input' }, text);
}

const RISK_ICONS = { destructive: 'flame', force_push: 'alert', protected_branch: 'branch', deploy: 'rocket', secrets: 'lock', network: 'globe', install: 'download', sudo: 'key', outside_repo: 'folder' };

function riskRow(req) {
  if (!req.risk || !req.risk.length) return null;
  return h('div', { class: 'risks' }, req.risk.slice(0, 4).map((r) => h('span', {
    class: 'risk ' + (r.level || 'low'),
    title: (r.level || 'low') + ' risk',
  }, icon(RISK_ICONS[r.code] || 'alert'), h('span', { class: 'sr-only' }, (r.level || 'low') + ' risk: '), r.text || r.code)));
}

function who(by) {
  const me = store.me && 'device:' + store.me.device.id;
  if (!by || by === me) return '';
  if (by.startsWith('device:')) return ' on another device';
  if (by === 'cli' || by === 'tui') return ' at the computer';
  if (by === 'ntfy') return ' from ntfy';
  if (by.startsWith('telegram')) return ' in Telegram';
  if (by.startsWith('policy:')) return ' by a rule';
  if (by.startsWith('grant:')) return ' by a session grant';
  return '';
}

/** How a closed request ended, for its card. */
function outcomeFor(req) {
  const d = req.decision;
  if (req.status === 'decided' && d) {
    const allow = d.behavior === 'allow';
    let what;
    if (req.checkpoint === 'question') what = allow ? 'Answered' : 'Declined';
    else if (req.checkpoint === 'plan') what = allow ? 'Plan approved' : 'Changes requested';
    else what = allow ? (d.scope === 'session' ? 'Allowed for this session' : 'Allowed') : 'Denied';
    return { cls: allow ? 'allow' : 'deny', icon: allow ? 'check' : 'x', text: what + who(d.by) };
  }
  switch (req.status) {
    case 'answered_locally': return { cls: 'neutral', icon: 'terminal', text: 'Answered in the terminal' };
    case 'expired': return { cls: 'neutral', icon: 'hourglass', text: 'Timed out — handed back to the terminal' };
    case 'stepped_aside': return { cls: 'neutral', icon: 'terminal', text: 'Waiting in the terminal' };
    default: return null;
  }
}

function errorText(res) {
  const d = res.data || {};
  if (d.code === 'stale_signature') return 'This device’s clock is off — fix the time and try again';
  if (d.code === 'no_key') return 'This device has no signing key — pair it again';
  if (res.status === 0) return 'Can’t reach your computer — try again';
  return d.error || 'Couldn’t send — try again';
}

// ---------- The card ----------

function build(req, opts) {
  const els = {};
  const kind = req.checkpoint === 'question' ? 'question' : req.checkpoint === 'plan' ? 'plan' : 'permission';
  const whatId = uid('req-what');
  const whereId = uid('req-where');
  const card = h('article', {
    class: `card req k-${kind} ${agentClass(req.agent)}` + (opts.animate === false ? '' : ' enter'),
    'data-nav': '', 'data-request': req.id, tabindex: '-1', 'aria-labelledby': `${whatId} ${whereId}`,
  });
  const wrap = h('div', { class: 'collapse' }, h('div', null, card));

  els.timerBar = h('i');
  els.timer = h('div', { class: 'timer', 'aria-hidden': 'true' }, els.timerBar);
  els.where = h('span', { class: 'where', id: whereId });
  els.age = h('time', { class: 'age' });
  els.context = h('button', { class: 'req-context', type: 'button', 'aria-expanded': 'false' });
  els.context.addEventListener('click', () => {
    const open = els.context.classList.toggle('open');
    els.context.setAttribute('aria-expanded', String(open));
  });
  els.what = h('div', { class: 'req-what', id: whatId });
  els.body = h('div');
  els.risks = h('div');
  els.meta = h('div', { class: 'meta-row' });
  els.left = h('span', { class: 'item left' });
  els.outcome = h('div', { class: 'outcome', role: 'status', 'aria-live': 'polite' });

  let current = req;
  let busy = false;
  let mine = false; // this instance sent the answer
  const controls = []; // everything that must disable when the request closes

  // ---- sending ----
  function showOutcome(o) {
    els.outcome.className = 'outcome ' + o.cls;
    fill(els.outcome, icon(o.icon, '', 'check'), h('span', null, o.text));
    card.classList.add('resolved');
    card.classList.remove('denying', 'revising');
  }
  function clearOutcome() {
    card.classList.remove('resolved');
    fill(els.outcome);
  }
  function setEnabled(on) {
    for (const c of controls) c.disabled = !on;
  }
  function settle(delay) {
    settling.add(current.id);
    setTimeout(() => {
      card.classList.add('leaving');
      wrap.classList.add('gone');
      setTimeout(() => {
        settling.delete(current.id);
        wrap.remove();
        changed();
      }, 380);
    }, delay);
  }

  async function send(body, optimistic, haptic) {
    if (busy || current.status !== 'open' || inflight.has(current.id)) return false;
    busy = true;
    mine = true;
    vibrate(haptic || 12);
    const id = current.id;
    inflight.set(id, optimistic);
    settling.add(id);
    showOutcome(optimistic);
    setEnabled(false);
    changed();
    const res = await self.PilothouseKeys.respond(id, body);
    busy = false;
    inflight.delete(id);
    if (res.ok) {
      store.requests.set(res.data.id, res.data);
      changed();
      closeRequestNotifications(id);
      if (opts.keep) settling.delete(id); else settle(900);
      return true;
    }
    if (res.status === 409) {
      toast('Already answered on another device', 'bad');
      try {
        const fresh = await api('GET', '/api/requests/' + encodeURIComponent(id));
        store.requests.set(fresh.id, fresh);
        const o = outcomeFor(fresh);
        if (o) showOutcome(o);
      } catch { /* keep the optimistic text */ }
      changed();
      if (opts.keep) settling.delete(id); else settle(1800);
      return false;
    }
    // Roll back.
    mine = false;
    settling.delete(id);
    clearOutcome();
    setEnabled(current.status === 'open');
    syncValidity();
    toast(errorText(res), 'bad');
    changed();
    if (res.status === 401) location.assign('/pair');
    return false;
  }

  // ---- permission ----
  let allowBtn;
  let sessionBtn;
  let denyBtn;
  let reason;
  let denyConfirm;
  let ruleBtn;
  function buildPermission() {
    allowBtn = h('button', { class: 'btn primary', type: 'button', title: 'Allow this one call' }, icon('check'), 'Allow once');
    sessionBtn = h('button', { class: 'btn secondary', type: 'button', title: 'Allow this tool or command prefix until the session ends' }, 'Allow for session');
    denyBtn = h('button', { class: 'btn danger', type: 'button' }, 'Deny');
    controls.push(allowBtn, sessionBtn, denyBtn);
    const reasonId = uid('reason');
    reason = h('textarea', { class: 'input', id: reasonId, rows: 2, maxlength: 2000, placeholder: 'e.g. Not on main — use a feature branch' });
    denyConfirm = h('button', { class: 'btn danger', type: 'button' }, 'Deny');
    const denyCancel = h('button', { class: 'btn ghost', type: 'button' }, 'Cancel');
    controls.push(reason, denyConfirm);
    ruleBtn = h('button', { class: 'link-btn quiet-link', type: 'button' }, icon('wand', 'sm'), 'Make it a rule…');
    ruleBtn.addEventListener('click', () => openRuleSheet(current));
    allowBtn.addEventListener('click', () => allowOnce());
    sessionBtn.addEventListener('click', () => allowSession());
    denyBtn.addEventListener('click', () => openDeny());
    denyCancel.addEventListener('click', () => { card.classList.remove('denying'); denyBtn.focus(); });
    denyConfirm.addEventListener('click', () => {
      card.classList.remove('denying');
      const msg = reason.value.trim();
      send({ behavior: 'deny', scope: 'once', ...(msg ? { message: msg } : {}) }, { cls: 'deny', icon: 'x', text: 'Denied' }, [8, 40, 8]);
    });
    reason.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); denyConfirm.click(); }
      if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); denyCancel.click(); }
    });
    return [
      h('div', { class: 'actions' + (opts.slot === 'main' || opts.slot === 'detail' ? ' wide-row' : '') }, denyBtn, sessionBtn, allowBtn),
      h('div', { class: 'card-foot' }, ruleBtn, h('span', { class: 'spacer' }),
        opts.keep ? null : link('/r/' + req.id, { class: 'link-btn quiet-link' }, 'Details', h('span', { class: 'sr-only' }, ' of this request'), icon('chev', 'sm'))),
      h('div', { class: 'deny-form' },
        h('label', { class: 'field-label', for: reasonId }, 'Tell the agent why (optional)'),
        reason,
        h('div', { class: 'row-btns' }, denyCancel, denyConfirm)),
    ];
  }
  function allowOnce() { if (kind === 'permission') send({ behavior: 'allow', scope: 'once' }, { cls: 'allow', icon: 'check', text: 'Allowed' }); }
  function allowSession() { if (kind === 'permission') send({ behavior: 'allow', scope: 'session' }, { cls: 'allow', icon: 'check', text: 'Allowed for this session' }); }
  function openDeny() {
    if (current.status !== 'open' || busy) return;
    card.classList.add('denying');
    reason.focus({ preventScroll: true });
    card.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
  }

  // ---- question ----
  const qstate = []; // per question: {q, chosen: Set(labels), other: bool, otherInput, buttons}
  let sendAnswer;
  let declineBtn;
  let hint;
  function syncValidity() {
    if (kind !== 'question' || !sendAnswer) return;
    const done = qstate.every((s) => s.chosen.size > 0 || (s.other && s.otherInput.value.trim()));
    const open = current.status === 'open' && !busy;
    sendAnswer.disabled = !open || !done;
    const missing = qstate.filter((s) => !(s.chosen.size > 0 || (s.other && s.otherInput.value.trim()))).length;
    hint.textContent = done ? '' : missing === qstate.length && qstate.length > 1
      ? `Pick an answer for each of the ${qstate.length} questions`
      : missing === 1 && qstate.length > 1 ? 'One more to answer' : 'Pick an answer';
  }
  function answers() {
    const out = {};
    for (const s of qstate) {
      const labels = (s.q.options || []).map((o) => o.label).filter((l) => s.chosen.has(l));
      const other = s.other ? s.otherInput.value.trim() : '';
      if (other) labels.push(other);
      out[s.q.id || s.q.question] = labels.join(', ');
    }
    return out;
  }
  function buildQuestion() {
    const parts = [];
    (req.questions || []).forEach((q, qi) => {
      const st = { q, chosen: new Set(), other: false, otherInput: null, buttons: [] };
      qstate.push(st);
      const textId = uid('q');
      const multi = !!q.multi_select;
      const role = multi ? 'checkbox' : 'radio';
      const opts2 = h('div', { class: 'q-options', role: multi ? 'group' : 'radiogroup', 'aria-labelledby': textId });
      const pick = (label, isOther) => {
        if (current.status !== 'open' || busy) return;
        if (multi) {
          if (isOther) st.other = !st.other;
          else if (st.chosen.has(label)) st.chosen.delete(label);
          else st.chosen.add(label);
        } else {
          st.chosen.clear();
          st.other = !!isOther;
          if (!isOther) st.chosen.add(label);
        }
        for (const b of st.buttons) {
          const on = b._other ? st.other : st.chosen.has(b._label);
          b.setAttribute('aria-checked', String(on));
        }
        if (st.otherInput) {
          st.otherInput.hidden = !st.other;
          if (st.other && isOther) st.otherInput.focus();
        }
        syncValidity();
      };
      const optButton = (label, desc, n, isOther) => {
        const b = h('button', { class: 'opt', type: 'button', role, 'aria-checked': 'false' },
          h('span', { class: 'mark', 'aria-hidden': 'true' }, icon('check')),
          h('span', { class: 'label' }, label),
          desc ? h('span', { class: 'desc' }, desc) : null,
          n <= 9 ? h('kbd', { class: 'key', 'aria-hidden': 'true' }, String(n)) : null);
        b._label = label;
        b._other = !!isOther;
        b.addEventListener('click', () => pick(label, isOther));
        st.buttons.push(b);
        controls.push(b);
        return b;
      };
      (q.options || []).forEach((o, i) => add(opts2, optButton(o.label, o.description, i + 1, false)));
      if (q.allow_other) {
        add(opts2, optButton('Other…', 'Type your own answer', (q.options || []).length + 1, true));
        st.otherInput = h('input', { class: 'input other-input', type: 'text', maxlength: 2000, placeholder: 'Your answer', hidden: true, 'aria-label': 'Your answer to: ' + q.question });
        st.otherInput.addEventListener('input', syncValidity);
        st.otherInput.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); if (!sendAnswer.disabled) sendAnswer.click(); } });
        controls.push(st.otherInput);
        add(opts2, st.otherInput);
      }
      parts.push(h('fieldset', { class: 'q' },
        h('legend', null,
          q.header ? h('span', { class: 'q-head' }, q.header) : null,
          h('span', { class: 'q-text', id: textId }, q.question),
          multi ? h('span', { class: 'q-multi' }, 'Choose any that apply') : null),
        opts2));
      st.index = qi;
    });
    hint = h('p', { class: 'answer-hint', 'aria-live': 'polite' });
    sendAnswer = h('button', { class: 'btn primary', type: 'button' }, icon('check'), 'Send answer');
    declineBtn = h('button', { class: 'btn secondary', type: 'button' }, 'Decline');
    controls.push(declineBtn);
    sendAnswer.addEventListener('click', () => {
      const a = answers();
      const summary = Object.values(a).join(' · ');
      send({ behavior: 'allow', answers: a }, { cls: 'allow', icon: 'check', text: 'Answered: ' + summary });
    });
    declineBtn.addEventListener('click', () => send({ behavior: 'deny', message: 'The person declined to answer from Pilothouse.' }, { cls: 'deny', icon: 'x', text: 'Declined' }, [8, 40, 8]));
    return [...parts, els.meta, hint, h('div', { class: 'actions two' }, declineBtn, sendAnswer)];
  }
  /** Number keys pick options of the first unanswered question. */
  function questionKey(k) {
    const n = parseInt(k, 10);
    if (!n || !qstate.length) return false;
    const st = qstate.find((s) => !(s.chosen.size || s.other)) || qstate[qstate.length - 1];
    const b = st.buttons[n - 1];
    if (!b) return false;
    b.click();
    return true;
  }

  // ---- plan ----
  let approveBtn;
  let reviseBtn;
  let feedback;
  function buildPlan() {
    const md = (req.plan && req.plan.markdown) || '';
    const long = md.length > 700 || md.split('\n').length > 16;
    const box = h('div', { class: 'plan' + (long ? '' : ' open'), tabindex: '0', role: 'region', 'aria-label': 'Plan' }, renderMarkdown(md, { dropTitle: !!markdownTitle(md), baseLevel: opts.headingLevel || 3 }));
    const more = long ? h('div', { class: 'plan-more' }, h('button', { class: 'link-btn', type: 'button', 'aria-expanded': 'false' }, icon('chevDown', 'sm'), 'Show the whole plan')) : null;
    if (more) {
      const b = more.firstChild;
      b.addEventListener('click', () => {
        const open = box.classList.toggle('open');
        b.setAttribute('aria-expanded', String(open));
        fill(b, icon('chevDown', 'sm'), open ? 'Show less' : 'Show the whole plan');
      });
    }
    approveBtn = h('button', { class: 'btn primary', type: 'button' }, icon('check'), 'Approve plan');
    reviseBtn = h('button', { class: 'btn secondary', type: 'button' }, 'Request changes');
    controls.push(approveBtn, reviseBtn);
    const fbId = uid('feedback');
    feedback = h('textarea', { class: 'input', id: fbId, rows: 3, maxlength: 2000, placeholder: 'e.g. Keep passwords for two releases, and skip the Playwright test for now' });
    const fbSend = h('button', { class: 'btn primary', type: 'button', disabled: true }, 'Send feedback');
    const fbCancel = h('button', { class: 'btn ghost', type: 'button' }, 'Cancel');
    controls.push(feedback);
    feedback.addEventListener('input', () => { fbSend.disabled = !feedback.value.trim() || current.status !== 'open'; });
    feedback.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && (e.metaKey || e.ctrlKey) && !fbSend.disabled) { e.preventDefault(); fbSend.click(); }
      if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); fbCancel.click(); }
    });
    approveBtn.addEventListener('click', () => send({ behavior: 'allow' }, { cls: 'allow', icon: 'check', text: 'Plan approved' }));
    reviseBtn.addEventListener('click', () => openRevise());
    fbCancel.addEventListener('click', () => { card.classList.remove('revising'); reviseBtn.focus(); });
    fbSend.addEventListener('click', () => {
      card.classList.remove('revising');
      send({ behavior: 'deny', message: feedback.value.trim() }, { cls: 'deny', icon: 'reply', text: 'Changes requested' }, [8, 40, 8]);
    });
    return [
      box, more,
      req.plan && req.plan.path ? h('div', { class: 'plan-path' }, relPath(req.plan.path, '')) : null,
      els.meta,
      h('div', { class: 'actions two' }, reviseBtn, approveBtn),
      h('div', { class: 'sub-form revise-form' },
        h('label', { class: 'field-label', for: fbId }, 'What should change?'),
        feedback,
        h('div', { class: 'row-btns' }, fbCancel, fbSend)),
    ];
  }
  function openRevise() {
    if (current.status !== 'open' || busy) return;
    card.classList.add('revising');
    feedback.focus({ preventScroll: true });
    card.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
  }

  const tail = kind === 'question' ? buildQuestion() : kind === 'plan' ? buildPlan() : buildPermission();
  add(card, els.timer,
    h('div', { class: 'req-top' }, agentBadge(req.agent), els.where, els.age),
    els.context, els.what, els.body, els.risks, kind === 'permission' ? els.meta : null, tail, els.outcome);
  if (kind !== 'permission') fill(els.body); // body is part of the tail

  function tick(now = Date.now()) {
    els.age.textContent = ago(current.created_at, now);
    els.age.dateTime = current.created_at;
    if (current.status !== 'open' || !current.expires_at) {
      els.left.hidden = true;
      return;
    }
    const total = toMs(current.expires_at) - toMs(current.created_at);
    const rem = toMs(current.expires_at) - now;
    const frac = total > 0 ? Math.max(0, Math.min(1, rem / total)) : 0;
    els.timerBar.style.transform = `scaleX(${frac.toFixed(4)})`; // CSSOM, allowed by CSP
    const low = rem < 60000;
    els.timer.classList.toggle('low', low);
    els.left.classList.toggle('low', low);
    els.left.hidden = false;
    fill(els.left, icon('clock', 'sm'), rem > 0 ? 'Hands back in ' + left(current.expires_at, now) : 'Handing back…');
  }

  let renderedBodyFor = '';
  function update(r) {
    const wasOpen = current.status === 'open';
    current = r;
    const s = store.sessions.get(r.session_id);
    fill(els.where, repoName(s, r), s && s.branch ? h('span', { class: 'branch' }, ' · ' + s.branch) : null);
    els.context.hidden = !r.context;
    if (r.context) {
      fill(els.context, h('span', { class: 'sr-only' }, agentName(r.agent) + ' says: '), '“' + r.context + '”');
    }
    const verb = requestVerb(r);
    if (kind === 'plan') {
      const title = markdownTitle(r.plan && r.plan.markdown);
      fill(els.what, verb, title ? h('span', { class: 'title' }, title) : null);
    } else if (kind === 'question') {
      fill(els.what, verb);
    } else {
      fill(els.what, verb, h('span', { class: 'tool' }, r.tool));
      // The details only change if the request itself does (a new id never reuses a card).
      const sig = r.id + (r.preview ? r.preview.files.length : '') + (r.command || '');
      if (sig !== renderedBodyFor) {
        fill(els.body, details(r));
        renderedBodyFor = sig;
      }
      sessionBtn.title = `Also allow ${grantPrefix(r)} for the rest of this session`;
      sessionBtn.setAttribute('aria-label', `Allow for session: also allow ${grantPrefix(r)} until this session ends`);
      allowBtn.setAttribute('aria-label', `Allow once: ${verb.toLowerCase()} ${r.command || r.summary || r.tool}`);
    }
    const rr = riskRow(r);
    fill(els.risks, rr);
    const metas = [];
    if (r.policy_rule) {
      metas.push(link('/rules#rule-' + r.policy_rule, { class: 'item', title: 'The rule that asked you' }, icon('shield', 'sm'), 'Rule ', h('b', null, r.policy_rule)));
    } else if (kind === 'permission') {
      metas.push(h('span', { class: 'item' }, icon('info', 'sm'), `${agentName(r.agent)} asked on its own`));
    }
    metas.push(els.left);
    fill(els.meta, ...metas);
    if (ruleBtn) ruleBtn.hidden = r.status !== 'open' && !opts.keep;

    const actionable = r.status === 'open';
    const pending = inflight.get(r.id);
    if (pending && !mine) {
      showOutcome(pending);
      setEnabled(false);
    } else if (!busy && !(mine && settling.has(r.id))) {
      const o = actionable ? null : outcomeFor(r);
      if (o) showOutcome(o);
      else if (!pending) clearOutcome();
      setEnabled(actionable);
      if (wasOpen && !actionable && !mine && document.activeElement && card.contains(document.activeElement)) card.focus({ preventScroll: true });
    }
    syncValidity();
    tick();
  }

  const ctl = {
    id: req.id,
    kind,
    wrap,
    card,
    update,
    tick,
    allow: () => (kind === 'permission' ? allowOnce() : kind === 'plan' ? approveBtn.click() : (!sendAnswer.disabled && sendAnswer.click())),
    allowSession: () => allowSession(),
    deny: () => (kind === 'permission' ? openDeny() : kind === 'plan' ? openRevise() : declineBtn.click()),
    focus: () => card.focus({ preventScroll: false }),
    key: (k) => (kind === 'question' ? questionKey(k) : false),
  };
  byCard.set(card, ctl);
  return ctl;
}

// One clock for every visible card.
setInterval(() => {
  const now = Date.now();
  for (const c of cards.values()) if (c.wrap.isConnected) c.tick(now);
}, 1000);

// ---------- Make it a rule ----------

const ACTION_LABEL = { allow: 'Always allow', ask: 'Always ask', deny: 'Never allow' };

/** Suggested rules for a request, each one signed tap away. */
export function openRuleSheet(req) {
  const listEl = h('div', null, skeleton('row', 3));
  const alsoBox = h('input', { type: 'checkbox', id: uid('also'), checked: true });
  const also = h('label', { class: 'also', for: alsoBox.id, hidden: true }, alsoBox, h('span', null, 'Also answer this request the same way'));
  const sheet = openSheet({
    title: 'Make it a rule',
    sub: 'Rules are checked before every tool call, so next time Pilothouse answers for you — on every device and agent.',
    body: [listEl, also],
  });
  api('GET', '/api/requests/' + encodeURIComponent(req.id) + '/suggestions').then((sg) => {
    if (!sg || !sg.length) {
      fill(listEl, h('p', { class: 'quiet' }, 'No suggestions for this request. You can write any rule in ~/.pilothouse/policy.yaml.'));
      return;
    }
    const live = store.requests.get(req.id) || req;
    also.hidden = live.status !== 'open' || !sg.some((s) => s.rule.action !== 'ask');
    fill(listEl, sg.map((s) => suggestionRow(s, req, alsoBox, sheet)));
    const first = listEl.querySelector('button');
    if (first) first.focus();
  }, (e) => {
    fill(listEl, h('p', { class: 'quiet' }, e.code === 'unsupported'
      ? 'This Pilothouse can’t make rules from the inbox yet. Add them to ~/.pilothouse/policy.yaml.'
      : e.message));
  });
}

function suggestionRow(s, req, alsoBox, sheet) {
  const act = s.rule.action;
  const btn = h('button', { class: 'btn small ' + (act === 'allow' ? 'primary' : act === 'deny' ? 'danger' : 'secondary'), type: 'button', 'aria-label': 'Add rule: ' + s.title.replace(/`/g, '') }, 'Add');
  const row = h('div', { class: 'sug' },
    h('div', { class: 't' }, richText(s.title)),
    h('div', { class: 'r' },
      h('span', { class: 'pill ' + act }, ACTION_LABEL[act] || act),
      h('span', null, s.scope === 'repo' ? 'This repo' : 'Everywhere'),
      s.reason ? h('span', null, '· ' + s.reason) : null),
    btn);
  btn.addEventListener('click', async () => {
    btn.disabled = true;
    fill(btn, h('span', { class: 'spin', 'aria-hidden': 'true' }), 'Adding');
    try {
      await signed('/api/rules', 'rule', 'add', s.rule);
    } catch (e) {
      btn.disabled = false;
      fill(btn, 'Add');
      toast(e.message || 'Couldn’t add the rule', 'bad');
      return;
    }
    row.classList.add('sug', 'added');
    fill(btn, icon('check', 'sm'), 'Added');
    vibrate(12);
    const live = store.requests.get(req.id) || req;
    let answered = '';
    if (alsoBox.checked && !alsoBox.closest('[hidden]') && live.status === 'open' && act !== 'ask') {
      const body = act === 'allow' ? { behavior: 'allow', scope: 'once' } : { behavior: 'deny', message: 'Blocked by a rule just made in Pilothouse.' };
      const res = await self.PilothouseKeys.respond(req.id, body);
      if (res.ok) {
        store.requests.set(res.data.id, res.data);
        changed();
        closeRequestNotifications(req.id);
        answered = act === 'allow' ? ' and allowed this one' : ' and denied this one';
      }
    }
    toast('Rule added' + answered, 'ok', { action: { label: 'View rules', run: () => go('/rules') } });
    setTimeout(() => sheet.close(), 500);
  });
  return row;
}

