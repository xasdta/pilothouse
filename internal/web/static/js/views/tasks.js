// Tasks (F7; workstream I): the list, a new task, and one task with its
// progress, its diff and Ship / Discard. Starting and finishing are
// signed `task` actions (target "new" or the task id).

import { h, icon, agentName, agentClass, agoLong, basename, stateLabel, fill, add, toMs } from '../dom.js';
import { store, api, onEvent } from '../api.js';
import { link, go, toast } from '../ui.js';
import { diffView } from '../diff.js';
import { section } from './rows.js';
import { ico, postAction, actionError, spinner, byLabel, diffStat } from './session.js';

const STATUS = {
  running: { label: 'Running', icon: 'play' },
  review: { label: 'Review', icon: 'eye' },
  done: { label: 'Done', icon: 'check' },
  abandoned: { label: 'Discarded', icon: 'x' },
};
const AGENTS = ['claude', 'codex'];
const LAST_REPO = 'pilothouse.task.repo';
const LAST_AGENT = 'pilothouse.task.agent';

function statusChip(t) {
  const s = STATUS[t.status] || { label: t.status || 'Unknown', icon: 'info' };
  return h('span', { class: 'task-chip ' + (t.status || '') }, ico(s.icon, 'sm'), s.label);
}

function startedBy(t) {
  const who = byLabel(t.by);
  return who ? (who === 'the computer' ? 'at the computer' : 'from ' + who) : '';
}

const store2 = { tasks: null, err: null, loading: null };

/** Loads (or reloads) the task list; resolves when done. */
function loadTasks() {
  if (!store2.loading) {
    store2.loading = api('GET', '/api/tasks')
      .then((ts) => { store2.tasks = Array.isArray(ts) ? ts : []; store2.err = null; })
      .catch((e) => { store2.err = e; })
      .finally(() => { store2.loading = null; });
  }
  return store2.loading;
}

function putTask(t) {
  if (!t || !t.id) return;
  if (!store2.tasks) store2.tasks = [];
  const i = store2.tasks.findIndex((x) => x.id === t.id);
  if (i >= 0) store2.tasks[i] = t; else store2.tasks.unshift(t);
}

// task.updated carries the whole task.
onEvent((ev) => { if (ev.type === 'task.updated' && ev.data) putTask(ev.data); });

function unsupportedState(what) {
  return h('div', { class: 'empty' },
    h('div', { class: 'halo' }, ico('branch', 'lg')),
    h('h2', null, `${what} need a newer Pilothouse`),
    h('p', null, 'Update Pilothouse on your computer to start agents from here, review their diffs and ship them.'));
}

// ---------- List ----------

export function tasksView() {
  const root = h('div', { class: 'view tasks' });
  const head = h('div', { class: 'page-head v2-head' });
  const body = h('div');
  add(root, head, body);

  let key = '';
  function render() {
    const ts = store2.tasks;
    // Re-render only when the tasks or their sessions' live state change.
    const live = (ts || []).filter((t) => t.status === 'running' && t.session_id).map((t) => {
      const s = store.sessions.get(t.session_id);
      return s ? s.state + (s.state_detail || '') : '';
    });
    const k = JSON.stringify([ts, live, store2.err && store2.err.status]);
    if (k === key) return;
    key = k;
    const running = (ts || []).filter((t) => t.status === 'running');
    const review = (ts || []).filter((t) => t.status === 'review');
    const done = (ts || []).filter((t) => t.status !== 'running' && t.status !== 'review')
      .sort((a, b) => toMs(b.updated_at) - toMs(a.updated_at)).slice(0, 20);
    const sub = ts ? [review.length ? `${review.length} to review` : null, running.length ? `${running.length} running` : null].filter(Boolean).join(' · ') || 'Nothing running' : '';
    fill(head,
      h('div', null, h('h1', { class: 'page-title' }, 'Tasks'), sub ? h('p', { class: 'page-sub' }, sub) : null),
      store2.err && store2.err.status === 501 ? null : link('/new', { class: 'btn primary compact', id: 'new-task' }, ico('plus', 'sm'), 'New task'));
    if (!ts) {
      if (store2.err && store2.err.status === 501) fill(body, unsupportedState('Tasks'));
      else if (store2.err) fill(body, h('div', { class: 'empty' }, h('h2', null, 'Couldn’t load tasks'), h('p', null, store2.err.message), h('button', { class: 'btn secondary', type: 'button', onclick: refresh }, 'Try again')));
      else fill(body, h('div', { class: 'list skel-block', 'aria-busy': 'true' }, h('span', { class: 'sr-only' }, 'Loading tasks…')));
      return;
    }
    if (!ts.length) {
      fill(body, h('div', { class: 'empty' },
        h('div', { class: 'halo' }, ico('branch', 'lg')),
        h('h2', null, 'Start work from anywhere'),
        h('p', null, 'A task runs an agent on your prompt in its own git worktree. You review the diff here, then ship it as a pull request — or discard it.'),
        link('/new', { class: 'btn primary compact task-empty-btn' }, ico('plus', 'sm'), 'New task')));
      return;
    }
    fill(body,
      review.length ? section('Ready for review', review.length, h('div', { class: 'list' }, review.map(taskRow)), 'urgent') : null,
      running.length ? section('Running', running.length, h('div', { class: 'list' }, running.map(taskRow))) : null,
      done.length ? section('Finished', 0, h('div', { class: 'list' }, done.map(taskRow))) : null);
  }

  function refresh() { loadTasks().then(render); render(); }
  const off = onEvent((ev) => { if (ev.type === 'task.updated') render(); });
  refresh();
  return { el: root, update: render, destroy: off };
}

function taskRow(t) {
  const s = t.session_id && store.sessions.get(t.session_id);
  const detail = t.status === 'running' && s ? (s.state_detail || stateLabel(s)) : null;
  return link('/t/' + t.id, { class: 'row task-row' },
    h('span', { class: 'task-tile ' + (t.status || ''), 'aria-hidden': 'true' }, ico((STATUS[t.status] || {}).icon || 'branch', 'sm')),
    h('span', { class: 'row-main' },
      h('span', { class: 'task-row-top' }, h('span', { class: 'task-title' }, t.title || t.prompt || 'Task'), statusChip(t)),
      h('span', { class: 'row-sub' }, basename(t.repo), t.branch ? h('span', { class: 'task-branch' }, ' · ' + t.branch) : null),
      h('span', { class: 'task-meta' },
        h('span', { class: 'agent-dot ' + agentClass(t.agent) }), agentName(t.agent),
        startedBy(t) ? ' · ' + startedBy(t) : '', ' · ', h('time', { datetime: t.created_at }, agoLong(t.created_at))),
      detail ? h('span', { class: 'task-detail' }, detail) : null),
    icon('chev', 'sm', 'chev'));
}

// ---------- New task ----------

export function newTaskView() {
  const root = h('div', { class: 'view new-task' });
  const err = h('p', { class: 'form-error', role: 'alert', id: 'task-err' });
  const repoBox = h('div', { class: 'repo-list', role: 'radiogroup', 'aria-labelledby': 'repo-legend' });
  const prompt = h('textarea', {
    class: 'input', id: 'task-prompt', rows: 5, maxlength: 32768, required: true, 'aria-describedby': 'task-prompt-hint',
    placeholder: 'e.g. Retry failed webhook deliveries with exponential backoff and add tests for it.',
  });
  const base = h('input', { class: 'input', id: 'task-base', autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false', maxlength: 200 });
  const name = h('input', { class: 'input', id: 'task-name', autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false', maxlength: 60, placeholder: 'from the prompt', pattern: '[A-Za-z0-9._/-]*' });
  const submit = h('button', { class: 'btn primary block', type: 'submit' }, ico('play', 'sm'), 'Start task');
  let repos = null;
  let repoErr = null;
  let agent = localStorage.getItem(LAST_AGENT) || 'claude';
  if (!AGENTS.includes(agent)) agent = 'claude';
  let repo = localStorage.getItem(LAST_REPO) || '';

  const agentBox = h('div', { class: 'agent-pick', role: 'radiogroup', 'aria-labelledby': 'agent-legend' },
    AGENTS.map((a) => {
      const id = 'agent-' + a;
      const input = h('input', { type: 'radio', name: 'agent', id, value: a, class: 'sr-only' });
      input.checked = a === agent;
      input.addEventListener('change', () => { agent = a; });
      return h('label', { class: 'agent-opt', for: id }, input, h('span', { class: 'agent-dot ' + agentClass(a) }), agentName(a));
    }));

  const form = h('form', { class: 'card task-form', novalidate: true },
    h('fieldset', null, h('legend', { id: 'repo-legend', class: 'field-label' }, 'Repository'), repoBox),
    h('fieldset', null, h('legend', { id: 'agent-legend', class: 'field-label' }, 'Agent'), agentBox),
    h('div', null,
      h('label', { class: 'field-label', for: 'task-prompt' }, 'What should it do?'),
      prompt,
      h('p', { class: 'field-hint', id: 'task-prompt-hint' }, 'The agent starts on a new branch in its own worktree. Nothing is pushed until you ship it.')),
    h('details', { class: 'task-opts' },
      h('summary', null, 'Branch options', ico('down', 'sm')),
      h('div', { class: 'task-opts-in' },
        h('div', null, h('label', { class: 'field-label', for: 'task-base' }, 'Start from (base branch)'), base),
        h('div', null, h('label', { class: 'field-label', for: 'task-name' }, 'New branch name'), name))),
    err,
    submit);

  function renderRepos() {
    if (repoErr) {
      fill(repoBox, h('p', { class: 'repo-none' }, repoErr.status === 501
        ? 'Starting tasks needs a newer Pilothouse on your computer.'
        : 'Couldn’t load your repositories: ' + repoErr.message));
      submit.disabled = true;
      return;
    }
    if (!repos) {
      fill(repoBox, h('div', { class: 'repo-opt skel-block', 'aria-busy': 'true' }, h('span', { class: 'sr-only' }, 'Loading repositories…')));
      return;
    }
    if (!repos.length) {
      fill(repoBox, h('p', { class: 'repo-none' }, 'Pilothouse hasn’t seen a repository yet. Run an agent in one on your computer, then come back.'));
      submit.disabled = true;
      return;
    }
    if (!repos.some((r) => r.root === repo)) repo = repos[0].root;
    fill(repoBox, repos.map((r, i) => {
      const id = 'repo-' + i;
      const input = h('input', { type: 'radio', name: 'repo', id, value: r.root, class: 'sr-only' });
      input.checked = r.root === repo;
      input.addEventListener('change', () => { repo = r.root; syncBase(); });
      return h('label', { class: 'repo-opt', for: id }, input,
        h('span', { class: 'repo-main' },
          h('span', { class: 'repo-name' }, r.name || basename(r.root)),
          h('span', { class: 'repo-path' }, r.root)),
        r.branch ? h('span', { class: 'repo-branch' }, ico('branch', 'sm'), r.branch) : null,
        h('span', { class: 'repo-check', 'aria-hidden': 'true' }, icon('check', 'sm')));
    }));
    syncBase();
    submit.disabled = false;
  }

  function syncBase() {
    const r = repos && repos.find((x) => x.root === repo);
    base.placeholder = r && r.branch ? r.branch : 'the repository’s current branch';
  }

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    err.textContent = '';
    const text = prompt.value.trim();
    if (!repo) { err.textContent = 'Pick a repository.'; return; }
    if (!text) { err.textContent = 'Tell the agent what to do.'; prompt.focus(); return; }
    if (name.value && !/^[A-Za-z0-9._/-]+$/.test(name.value)) { err.textContent = 'Branch names can use letters, digits, “.”, “_”, “-” and “/”.'; name.focus(); return; }
    const payload = { repo, agent, prompt: text };
    if (base.value.trim()) payload.base = base.value.trim();
    if (name.value.trim()) payload.name = name.value.trim();
    submit.disabled = true;
    fill(submit, spinner(), 'Starting…');
    const res = await postAction('/api/tasks', 'task', 'new', payload);
    submit.disabled = false;
    fill(submit, ico('play', 'sm'), 'Start task');
    if (!res.ok) {
      err.textContent = actionError(res, 'Starting tasks needs a newer Pilothouse on your computer.');
      return;
    }
    try { localStorage.setItem(LAST_REPO, repo); localStorage.setItem(LAST_AGENT, agent); } catch { /* not remembered */ }
    putTask(res.data);
    toast('Task started', 'ok');
    go('/t/' + res.data.id, true);
  });
  prompt.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); form.requestSubmit(); }
  });

  add(root,
    h('div', { class: 'page-head' },
      h('h1', { class: 'page-title' }, 'New task'),
      h('p', { class: 'page-sub' }, 'An agent works on it in its own worktree. You review the diff before anything is pushed.')),
    form);
  renderRepos();
  api('GET', '/api/repos').then((rs) => {
    repos = (Array.isArray(rs) ? rs : []).sort((a, b) => toMs(b.last_used) - toMs(a.last_used));
    renderRepos();
  }, (e) => { repoErr = e; renderRepos(); });

  return { el: root, update: () => {}, focus: () => prompt.focus() };
}

// ---------- One task ----------

export function taskView(id) {
  const root = h('div', { class: 'view task' });
  const headEl = h('div');
  const progEl = h('div');
  const diffEl = h('div');
  const actEl = h('div');
  add(root, headEl, progEl, diffEl, actEl);
  let diff = null;
  let diffErr = null;
  let diffFor = '';
  let busy = '';
  let destroyed = false;
  let refetch = 0;

  const task = () => (store2.tasks || []).find((t) => t.id === id);

  function render() {
    const t = task();
    if (!t) {
      fill(headEl, store2.err && store2.err.status === 501 ? unsupportedState('Tasks')
        : store2.tasks || store2.err ? h('div', { class: 'empty' }, h('h2', null, 'Task not found'), h('p', null, 'It may have been removed on your computer.'), link('/tasks', { class: 'btn secondary' }, 'All tasks'))
          : h('div', { class: 'card skel-block', 'aria-busy': 'true' }, h('span', { class: 'sr-only' }, 'Loading…')));
      fill(progEl); fill(diffEl); fill(actEl);
      return;
    }
    const by = startedBy(t);
    fill(headEl, h('div', { class: 'card task-hero' },
      h('div', { class: 'task-hero-top' }, statusChip(t), h('span', { class: 'task-when' }, 'Started ', h('time', { datetime: t.created_at }, agoLong(t.created_at)))),
      h('h1', null, t.title || 'Task'),
      h('p', { class: 'task-where' },
        h('span', { class: 'repo-name' }, basename(t.repo)),
        h('span', { class: 'task-branches' }, ico('branch', 'sm'), h('code', null, t.branch || '—'), t.base_ref ? [h('span', { class: 'arrow', 'aria-hidden': 'true' }, ' ← '), h('span', { class: 'sr-only' }, ' from '), h('code', null, t.base_ref)] : null)),
      h('p', { class: 'task-meta' }, h('span', { class: 'agent-dot ' + agentClass(t.agent) }), agentName(t.agent), by ? ' · started ' + by : ''),
      t.prompt ? h('details', { class: 'task-prompt' }, h('summary', null, 'Prompt', ico('down', 'sm')), h('p', null, t.prompt)) : null,
      t.pr_url && /^https:\/\//.test(t.pr_url) ? h('a', { class: 'btn secondary compact task-pr', href: t.pr_url, target: '_blank', rel: 'noopener noreferrer' }, ico('pr', 'sm'), 'View pull request', ico('ext', 'sm')) : null));
    renderProgress(t);
    renderDiff(t);
    renderActions(t);
    if (diffFor !== t.updated_at) loadDiff(t);
  }

  function renderProgress(t) {
    const s = t.session_id && store.sessions.get(t.session_id);
    const steps = [
      { key: 'start', label: 'Started', sub: t.base_ref ? `New branch from ${t.base_ref}` : 'New worktree' },
      { key: 'work', label: 'Working', sub: t.status === 'running' && s ? (s.state_detail || stateLabel(s)) : null },
      { key: 'review', label: 'Review', sub: t.status === 'review' ? 'The agent finished — look over the changes' : null },
      t.status === 'abandoned'
        ? { key: 'end', label: 'Discarded', sub: 'Worktree and branch removed', bad: true }
        : { key: 'end', label: 'Shipped', sub: 'Pushed with a pull request' },
    ];
    const at = { running: 1, review: 2, done: 4, abandoned: 4 }[t.status] ?? 1;
    fill(progEl, section('Progress', 0, h('div', { class: 'card task-prog' },
      h('ol', { class: 'steps-v2' }, steps.map((st, i) => {
        const state = i < at ? 'done' : i === at ? 'now' : 'todo';
        return h('li', { class: `step ${state}${st.bad ? ' bad' : ''}`, 'aria-current': state === 'now' ? 'step' : null },
          h('span', { class: 'step-dot' }, state === 'done' ? ico(st.bad ? 'x' : 'check', 'sm') : state === 'now' && t.status === 'running' ? spinner('sm') : null),
          h('span', { class: 'step-t' }, h('b', null, st.label), st.sub && state !== 'todo' ? h('span', null, st.sub) : null));
      })),
      t.session_id ? link('/s/' + t.session_id, { class: 'task-session' },
        ico('history', 'sm'),
        h('span', null, h('b', null, 'Open session'), s ? h('span', { class: 'pill ' + s.state }, stateLabel(s)) : null),
        icon('chev', 'sm', 'chev')) : null)));
  }

  // ---- Diff ----
  async function loadDiff(t) {
    diffFor = t.updated_at;
    try {
      diff = await api('GET', `/api/tasks/${encodeURIComponent(id)}/diff`);
      diffErr = null;
    } catch (e) {
      diffErr = e;
    }
    if (destroyed) return;
    renderDiff(task() || t);
  }

  function renderDiff(t) {
    if (diffErr && !diff) {
      fill(diffEl, section('Changes', 0, h('p', { class: 'quiet' }, diffErr.status === 501 ? 'Diffs need a newer Pilothouse on your computer.' : 'Couldn’t load the changes.')));
      return;
    }
    if (!diff) {
      fill(diffEl, section('Changes', 0, h('div', { class: 'card skel-block', 'aria-busy': 'true' }, h('span', { class: 'sr-only' }, 'Loading changes…'))));
      return;
    }
    const files = diff.files || [];
    const head = h('h2', { class: 'section-head' }, 'Changes', files.length ? h('span', { class: 'count' }, String(files.length)) : null,
      files.length ? h('span', { class: 'files-total' }, h('span', { class: 'n-add' }, '+' + (diff.added || 0)), ' ', h('span', { class: 'n-del' }, '−' + (diff.removed || 0))) : null);
    if (!files.length) {
      fill(diffEl, h('section', { class: 'section' }, head, h('div', { class: 'act-empty' }, ico('branch', 'lg'),
        h('p', null, t.status === 'running' ? 'No changes yet — they show up here as the agent works.' : 'This task changed nothing.'))));
      return;
    }
    fill(diffEl, h('section', { class: 'section' }, head,
      h('div', { class: 'task-files' }, files.map((f, i) => fileDiff(f, i, files.length))),
      diff.truncated ? h('p', { class: 'quiet' }, 'Some files are left out. See the full diff on your computer.') : null));
  }

  const openFiles = new Set();
  function fileDiff(f, i, n) {
    const det = h('details', { class: 'card file-diff' });
    det.open = openFiles.has(f.path) || (n <= 3 && !openFiles.has('closed:' + f.path));
    det.addEventListener('toggle', () => {
      if (det.open) { openFiles.add(f.path); openFiles.delete('closed:' + f.path); } else { openFiles.delete(f.path); openFiles.add('closed:' + f.path); }
    });
    const cut = f.path.lastIndexOf('/');
    add(det,
      h('summary', null,
        h('span', { class: 'file-path' }, h('span', { class: 'base' }, cut >= 0 ? f.path.slice(cut + 1) : f.path), cut > 0 ? h('span', { class: 'dir' }, f.path.slice(0, cut)) : null),
        f.op && f.op !== 'edit' ? h('span', { class: 'op-tag ' + f.op }, f.op === 'create' ? 'new' : f.op === 'delete' ? 'deleted' : f.op) : null,
        diffStat(f.added || 0, f.removed || 0),
        ico('down', 'sm', 'act-chev')));
    // One FilePreview: diffView returns just its lines for this card.
    add(det, diffView(f, { lang: f.lang }));
    return det;
  }

  // ---- Ship / Discard ----
  function renderActions(t) {
    if (t.status !== 'review' && t.status !== 'running') { fill(actEl); return; }
    const ship = h('button', { class: 'btn primary', type: 'button', id: 'task-ship', disabled: t.status !== 'review' || !!busy }, busy === 'ship' ? spinner() : ico('pr', 'sm'), 'Ship');
    const discard = h('button', { class: 'btn danger', type: 'button', id: 'task-discard', disabled: !!busy }, busy === 'discard' ? spinner() : ico('trash', 'sm'), 'Discard');
    ship.addEventListener('click', () => finish(t, 'ship'));
    discard.addEventListener('click', () => finish(t, 'discard'));
    fill(actEl, h('div', { class: 'task-actions' },
      h('div', { class: 'task-actions-in' }, discard, ship),
      h('p', null, t.status === 'review'
        ? `Ship pushes ${t.branch} and opens a pull request into ${t.base_ref || 'its base branch'}.`
        : 'You can ship once the agent finishes. Discard stops it and removes its worktree.')));
  }

  async function finish(t, action) {
    const ok = await confirmSheet(action === 'ship'
      ? { title: 'Ship this task?', body: [`Pushes `, h('code', null, t.branch), ` and opens a pull request into `, h('code', null, t.base_ref || 'the base branch'), ` with gh on your computer.`], confirm: 'Push and open PR', icon: 'pr' }
      : { title: 'Discard this task?', body: [`Stops the agent and deletes its worktree and the branch `, h('code', null, t.branch), ` on your computer. This can’t be undone.`], confirm: 'Discard', danger: true, icon: 'trash' });
    if (!ok || destroyed) return;
    busy = action;
    renderActions(t);
    const res = await postAction(`/api/tasks/${encodeURIComponent(t.id)}/finish`, 'task', t.id, { action });
    busy = '';
    if (destroyed) return;
    if (res.ok) {
      putTask(res.data);
      toast(action === 'ship' ? (res.data && res.data.pr_url ? 'Pull request opened' : 'Shipped') : 'Task discarded', 'ok');
    } else {
      toast(actionError(res, 'Your computer can’t finish tasks yet.'), 'bad');
    }
    render();
  }

  const off = onEvent((ev) => {
    if (ev.type !== 'task.updated' || !ev.data || ev.data.id !== id) return;
    clearTimeout(refetch);
    refetch = setTimeout(render, 150);
  });

  render();
  loadTasks().then(() => { if (!destroyed) render(); });
  return {
    el: root,
    update: () => { if (task()) renderProgress(task()); },
    destroy() { destroyed = true; off(); clearTimeout(refetch); },
  };
}

// ---------- Confirm sheet ----------

/** A modal confirmation (bottom sheet on phones). Resolves true on confirm. */
export function confirmSheet({ title, body, confirm, danger = false, icon: ic = 'info' }) {
  return new Promise((resolve) => {
    const yes = h('button', { class: 'btn ' + (danger ? 'danger solid' : 'primary'), type: 'button', value: 'yes' }, confirm);
    const no = h('button', { class: 'btn secondary', type: 'button', value: 'no' }, 'Cancel');
    const dlg = h('dialog', { class: 'sheet', 'aria-labelledby': 'sheet-t', 'aria-describedby': 'sheet-b' },
      h('div', { class: 'sheet-ic' + (danger ? ' bad' : '') }, ico(ic)),
      h('h2', { id: 'sheet-t' }, title),
      h('p', { id: 'sheet-b' }, body),
      h('div', { class: 'sheet-btns' }, no, yes));
    let done = false;
    const close = (v) => {
      if (done) return;
      done = true;
      if (dlg.open) dlg.close();
      dlg.remove();
      resolve(v);
    };
    yes.addEventListener('click', () => close(true));
    no.addEventListener('click', () => close(false));
    dlg.addEventListener('cancel', (e) => { e.preventDefault(); close(false); });
    dlg.addEventListener('click', (e) => { if (e.target === dlg) close(false); }); // backdrop
    add(document.body, dlg);
    dlg.showModal();
    no.focus();
  });
}
