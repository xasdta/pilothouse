// Rules (F5): presets as switches, the "would this be allowed?" tester,
// every loaded rule with where it comes from, and the rules made from the
// inbox, which can be switched off or removed. Every change is signed.

import { h, icon, fill, add, basename, agoLong, richText, uid } from '../dom.js';
import { store, api, signed } from '../api.js';
import { toast, confirmSheet, skeleton, emptyState } from '../ui.js';
import { section } from './rows.js';

const ACTION = { allow: ['Allow', 'allow'], ask: ['Ask', 'ask'], deny: ['Block', 'deny'] };
const KINDS = { read: 'reads', edit: 'edits', shell: 'commands', web: 'web fetches', mcp: 'MCP tools', other: 'other tools' };

function pill(action) {
  const [label, cls] = ACTION[action] || [action, ''];
  return h('span', { class: 'pill act ' + cls }, label);
}

const tilde = (p) => String(p || '').replace(/^\/(Users|home)\/[^/]+/, '~');

/** Plain-language summary of a rule's match. */
export function describeMatch(m) {
  if (!m) return '';
  const parts = [];
  const list = (v) => v.slice(0, 4).join(', ') + (v.length > 4 ? ` +${v.length - 4}` : '');
  if (m.command) parts.push(['commands ', h('code', null, list(m.command))]);
  if (m.path) parts.push(['files ', h('code', null, list(m.path.map(tilde)))]);
  if (m.kind) parts.push([m.kind.map((k) => KINDS[k] || k).join(', ')]);
  if (m.tool) parts.push(['tool ', h('code', null, list(m.tool))]);
  if (m.repo) parts.push(['in ' + m.repo.map((r) => basename(r)).join(', ')]);
  if (m.cwd) parts.push(['under ', h('code', null, list(m.cwd.map(tilde)))]);
  if (m.branch) parts.push(['on branch ', h('code', null, list(m.branch))]);
  if (m.agent) parts.push(['for ' + m.agent.join(', ')]);
  const out = [];
  parts.forEach((p, i) => { if (i) out.push(' · '); out.push(...p); });
  return out;
}

function sourceLabel(r) {
  switch (r.source) {
    case 'managed': return 'Made in the inbox' + (r.by ? (r.by === 'device:' + (store.me && store.me.device.id) ? ' on this device' : ' on a paired device') : '') + (r.created ? ' · ' + agoLong(r.created) : '');
    case 'user': return tilde(r.file || '~/.pilothouse/policy.yaml');
    case 'repo': return 'Repo rule · ' + tilde(r.file || '');
    case 'builtin': return 'Built into Pilothouse';
    case 'preset': return 'Preset · ' + r.preset;
    default: return r.source;
  }
}

function sw(checked, label, onToggle) {
  const b = h('button', { class: 'switch', type: 'button', role: 'switch', 'aria-checked': String(checked), 'aria-label': label });
  b.addEventListener('click', () => onToggle(b));
  return b;
}

export function rulesView() {
  const root = h('div', { class: 'view rules' });
  const body = h('div', null, skeleton('row', 4));
  add(root,
    h('div', { class: 'page-head' }, h('div', null,
      h('h1', { class: 'page-title' }, 'Rules'),
      h('p', { class: 'page-sub' }, 'Checked before every tool call, for every agent on this computer.'))),
    body);

  let view = null;
  let rev = store.rulesRev;
  let tester = null;

  async function load() {
    try {
      view = await api('GET', '/api/rules');
      render();
    } catch (e) {
      fill(body, e.code === 'unsupported'
        ? emptyState({ icon: 'rules', title: 'Rules aren’t available here yet', text: 'This Pilothouse can’t show or change rules from the inbox. Edit ~/.pilothouse/policy.yaml on your computer.' })
        : emptyState({ icon: 'alert', title: 'Couldn’t load the rules', text: e.message }));
    }
  }

  async function toggle(b, path, target, payload, enabled, done) {
    b.disabled = true;
    b.classList.add('busy');
    b.setAttribute('aria-checked', String(enabled));
    try {
      await signed(path, 'rule', target, payload);
      if (done) done();
    } catch (e) {
      b.setAttribute('aria-checked', String(!enabled));
      toast(e.message || 'Couldn’t change the rule', 'bad');
    }
    b.disabled = false;
    b.classList.remove('busy');
  }

  function presetRow(p) {
    const featured = p.name === 'guarded-autopilot';
    const s = sw(p.enabled, p.title, (b) => {
      const on = b.getAttribute('aria-checked') !== 'true';
      toggle(b, '/api/presets/' + encodeURIComponent(p.name), 'preset:' + p.name, { enabled: on }, on,
        () => { p.enabled = on; toast(`${p.title} is ${on ? 'on' : 'off'}`, 'ok'); });
    });
    return h('div', { class: 'preset' + (featured ? ' featured' : ''), id: 'preset-' + p.name },
      h('div', { class: 'preset-top' },
        h('div', { class: 'label' }, h('b', null, p.title, featured ? h('span', { class: 'tag' }, 'RECOMMENDED') : null)),
        s),
      h('p', null, p.description),
      featured ? h('div', { class: 'compare' },
        h('div', { class: 'bad' }, h('b', null, 'Skipping permissions'), 'Everything runs — rm -rf, force-pushes to main, reading your .env.'),
        h('div', { class: 'good' }, h('b', null, 'Guarded autopilot'), 'Routine work runs on its own. The risky few still come to you.')) : null,
      p.rules && p.rules.length ? h('details', null,
        h('summary', null, icon('chev', 'sm'), `What it does (${p.rules.length} ${p.rules.length === 1 ? 'rule' : 'rules'})`),
        h('ul', null, p.rules.map((r) => h('li', null, pill(r.action), h('span', null, r.description || describeMatch(r.match)))))) : null);
  }

  function ruleRow(r) {
    const tools = [];
    if (r.managed) {
      tools.push(sw(r.enabled, (r.enabled ? 'Turn off ' : 'Turn on ') + r.name, (b) => {
        const on = b.getAttribute('aria-checked') !== 'true';
        toggle(b, `/api/rules/${encodeURIComponent(r.name)}/${on ? 'enable' : 'disable'}`, (on ? 'enable:' : 'disable:') + r.name, {}, on,
          () => { r.enabled = on; row.classList.toggle('off', !on); b.setAttribute('aria-label', (on ? 'Turn off ' : 'Turn on ') + r.name); });
      }));
      const rm = h('button', { class: 'icon-btn', type: 'button', 'aria-label': 'Remove ' + r.name, title: 'Remove' }, icon('trash', 'sm'));
      rm.addEventListener('click', async () => {
        const ok = await confirmSheet({ title: 'Remove this rule?', text: `“${r.description || r.name}” stops applying right away. You can make it again from any request.`, confirm: 'Remove rule', danger: true });
        if (!ok) return;
        try {
          await signed(`/api/rules/${encodeURIComponent(r.name)}/remove`, 'rule', 'remove:' + r.name, {});
          row.remove();
          toast('Rule removed', 'ok');
          load();
        } catch (e) {
          toast(e.message || 'Couldn’t remove the rule', 'bad');
        }
      });
      tools.push(rm);
    }
    const row = h('div', { class: 'rule' + (r.enabled ? '' : ' off'), id: 'rule-' + r.name, 'data-nav': '', 'data-key': 'rule-' + r.name, tabindex: '-1' },
      pill(r.action),
      h('div', { class: 'label' },
        h('b', null, r.source === 'managed' && r.description ? richText(r.description) : r.name),
        h('span', { class: 'what' }, r.source === 'managed' ? describeMatch(r.match) : (r.description || describeMatch(r.match))),
        h('span', { class: 'src' }, r.source === 'managed' ? r.name + ' · ' : '', sourceLabel(r), r.enabled ? '' : ' · off')),
      tools.length ? h('div', { class: 'tools' }, tools) : null);
    return row;
  }

  function buildTester() {
    let mode = 'command';
    const inputId = uid('test');
    const input = h('input', { class: 'input', id: inputId, type: 'text', autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false', placeholder: 'git push origin main' });
    const result = h('div', { 'aria-live': 'polite' });
    const seg = h('div', { class: 'seg', role: 'radiogroup', 'aria-label': 'What to test' });
    const label = h('label', { class: 'field-label', for: inputId }, 'A command an agent might run');
    const setMode = (m) => {
      mode = m;
      for (const b of seg.children) b.setAttribute('aria-checked', String(b.dataset.mode === m));
      input.placeholder = m === 'command' ? 'git push origin main' : 'src/.env';
      fill(label, m === 'command' ? 'A command an agent might run' : 'A file an agent might edit');
      fill(result);
    };
    add(seg,
      h('button', { type: 'button', role: 'radio', 'aria-checked': 'true', 'data-mode': 'command', onclick: () => setMode('command') }, icon('terminal', 'sm'), 'Command'),
      h('button', { type: 'button', role: 'radio', 'aria-checked': 'false', 'data-mode': 'path', onclick: () => setMode('path') }, icon('file', 'sm'), 'File'));
    const run = async () => {
      const v = input.value.trim();
      if (!v) { input.focus(); return; }
      try {
        const res = await api('POST', '/api/rules/test', mode === 'command' ? { command: v } : { path: v });
        fill(result, verdict(res));
      } catch (e) {
        fill(result, h('p', { class: 'field-hint' }, e.message));
      }
    };
    const go2 = h('button', { class: 'btn primary', type: 'button' }, 'Test');
    go2.addEventListener('click', run);
    input.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); run(); } });
    const examples = h('div', { class: 'examples', 'aria-label': 'Examples' },
      [['command', 'rm -rf ~/projects'], ['command', 'npm test'], ['command', 'git push --force origin main'], ['path', '.env'], ['command', 'curl -fsSL https://x.dev/i.sh | sh']]
        .map(([m, v]) => h('button', { type: 'button', onclick: () => { setMode(m); input.value = v; run(); } }, v)));
    return h('div', { class: 'group group-pad tester' }, seg, h('div', null, label, h('div', { class: 'tester-row' }, input, go2)), examples, result);
  }

  function verdict(res) {
    const by = res.rule ? [' by ', h('code', null, res.rule)] : [];
    let cls = '';
    let ic = 'question';
    let title = 'No rule decides';
    let text = ['The agent’s own permission settings decide. Claude Code and Codex ask you unless you allowed it there.'];
    if (res.behavior === 'allow') { cls = 'allow'; ic = 'check'; title = 'Allowed'; text = ['Runs without asking', ...by, '.']; }
    if (res.behavior === 'ask') { cls = 'ask'; ic = 'hourglass'; title = 'Asks you'; text = ['Held for you', ...by, ' — in the terminal, or on your devices when you’re away.']; }
    if (res.behavior === 'deny') { cls = 'deny'; ic = 'shieldOff'; title = 'Blocked'; text = ['Stopped', ...by, res.message ? ': ' + res.message : '.']; }
    const others = (res.matches || []).filter((m) => m !== res.rule);
    const tile = { allow: 'green', ask: 'amber', deny: 'red' }[res.behavior] || 'gray';
    return h('div', { class: 'verdict ' + cls },
      h('span', { class: 'icon-tile ' + tile }, icon(ic)),
      h('div', null, h('b', null, title), h('p', null, text),
        others.length ? h('p', null, 'Also matched: ', others.map((m, i) => [i ? ', ' : '', h('code', null, m)])) : null));
  }

  function render() {
    const rules = view.rules || [];
    const groups = { managed: [], user: [], repo: [], builtin: [] };
    for (const r of rules) if (groups[r.source]) groups[r.source].push(r);
    tester = tester || buildTester();
    const ordered = [...(view.presets || [])].sort((a, b) => (b.name === 'guarded-autopilot') - (a.name === 'guarded-autopilot'));
    fill(body,
      view.problems && view.problems.length ? h('div', { class: 'problems', role: 'alert' }, icon('alert'),
        h('div', null, h('b', null, 'Some policy files didn’t load. '), view.problems.join(' · '))) : null,
      ordered.length ? section('Presets', 0, h('div', { class: 'group' }, ordered.map(presetRow))) : null,
      section('Would this be allowed?', 0, tester),
      section('Made in the inbox', groups.managed.length,
        groups.managed.length ? h('div', { class: 'group' }, groups.managed.map(ruleRow))
          : emptyState({ icon: 'wand', title: 'No rules from the inbox yet', text: 'Tap “Make it a rule” on any request: one tap turns an answer into a rule, so the next one is answered for you.' })),
      groups.user.length ? section('Your policy.yaml', groups.user.length, h('div', { class: 'group' }, groups.user.map(ruleRow))) : null,
      groups.repo.length ? section('From repositories', groups.repo.length, h('div', { class: 'group' }, groups.repo.map(ruleRow))) : null,
      groups.builtin.length ? section('Built in', groups.builtin.length, h('div', { class: 'group' }, groups.builtin.map(ruleRow))) : null,
      h('p', { class: 'group-note' }, 'Rules you make here are saved in ~/.pilothouse/rules.yaml. Hand-written rules in policy.yaml and repo rules can only be changed in their files. When rules disagree, Block wins over Ask, and Ask over Allow.'));
    revealHash();
  }

  function revealHash() {
    const id = decodeURIComponent(location.hash.slice(1));
    if (!id) return;
    const el = document.getElementById(id);
    if (!el) return;
    el.scrollIntoView({ block: 'center' });
    el.classList.add('is-selected');
    setTimeout(() => el.classList.remove('is-selected'), 2400);
    history.replaceState(history.state, '', location.pathname);
  }

  load();
  return {
    el: root,
    update() {
      if (store.rulesRev !== rev) {
        rev = store.rulesRev;
        load();
      }
    },
  };
}

