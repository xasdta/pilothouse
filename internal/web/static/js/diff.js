// Diff preview: renders api.Preview, api.TaskDiff or FilePreview[] as
// unified diffs with old/new line numbers, +/- colours, per-file
// collapse and "show more". Text goes in as text nodes only.
//
// Exported API (stable; workstream I uses it for task diffs):
//   diffView(source, opts?) → element
//     source: {files, added, removed, truncated} (Preview / TaskDiff) | FilePreview[] | FilePreview
//     opts.cwd       shorten absolute paths under it
//     opts.open      'auto' (default: all files when ≤ 2, else the first) | 'all' | 'none'
//     opts.maxLines  lines per file before "Show N more lines" (default 40; 0 = all)
//     opts.summary   show the "N files +A −R" line (default: when > 1 file)
//   diffStat(added, removed, {bar}?) → element "+12 −3" with an optional 5-cell bar
//   parseUnified(text) → [{kind: 'hunk'|'add'|'del'|'ctx'|'note', text, old, new}]
//   fileLabel(path, cwd) → [dir, name]

import { h, icon, fill, add, relPath, plural } from './dom.js';

let seq = 0;

/** Parses a unified diff (with or without ---/+++ headers) into lines. */
export function parseUnified(text) {
  const out = [];
  let o = 1;
  let n = 1;
  const lines = String(text || '').replace(/\r\n/g, '\n').split('\n');
  if (lines.length && lines[lines.length - 1] === '') lines.pop();
  for (const l of lines) {
    if (l.startsWith('--- ') || l.startsWith('+++ ') || l.startsWith('diff --git') || l.startsWith('index ')) continue;
    const m = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@ ?(.*)$/.exec(l);
    if (m) {
      o = +m[1];
      n = +m[2];
      out.push({ kind: 'hunk', text: m[3] || '', old: null, new: null });
      continue;
    }
    if (l.startsWith('\\')) {
      out.push({ kind: 'note', text: l.replace(/^\\\s*/, ''), old: null, new: null });
      continue;
    }
    const c = l[0];
    if (c === '+') out.push({ kind: 'add', text: l.slice(1), old: null, new: n++ });
    else if (c === '-') out.push({ kind: 'del', text: l.slice(1), old: o++, new: null });
    else out.push({ kind: 'ctx', text: c === ' ' ? l.slice(1) : l, old: o++, new: n++ });
  }
  return out;
}

/** "+12 −3", with a 5-cell proportion bar when opts.bar. */
export function diffStat(added, removed, opts = {}) {
  const a = added || 0;
  const r = removed || 0;
  const kids = [
    h('span', { class: 'add' }, '+' + a),
    h('span', { class: 'del' }, '−' + r),
  ];
  if (opts.bar && a + r > 0) {
    const cells = 5;
    let ca = Math.round((a / (a + r)) * cells);
    if (a > 0 && ca === 0) ca = 1;
    if (r > 0 && ca === cells) ca = cells - 1;
    kids.push(h('span', { class: 'bar', 'aria-hidden': 'true' },
      Array.from({ length: cells }, (_, i) => h('i', { class: i < ca ? 'a' : 'd' }))));
  }
  return h('span', { class: 'stat', 'aria-label': `${a} ${plural(a, 'line', 'lines')} added, ${r} removed` }, kids);
}

/** [dir/, name] of a path, relative to cwd when inside it. */
export function fileLabel(path, cwd) {
  const p = relPath(path || '', cwd);
  const cut = p.lastIndexOf('/');
  return cut >= 0 ? [p.slice(0, cut + 1), p.slice(cut + 1)] : ['', p];
}

const OPS = { create: 'new', delete: 'deleted', rename: 'renamed', overwrite: 'rewrite' };

function lineRow(l) {
  const sign = l.kind === 'add' ? '+' : l.kind === 'del' ? '−' : '';
  if (l.kind === 'hunk') {
    return h('div', { class: 'dl hunk' }, h('span', { class: 'n' }), h('span', { class: 'n' }), h('span', { class: 's' }, '⋯'), h('span', { class: 't' }, l.text));
  }
  if (l.kind === 'note') {
    return h('div', { class: 'dl note' }, h('span', { class: 'n' }), h('span', { class: 'n' }), h('span', { class: 's' }), h('span', { class: 't' }, l.text));
  }
  return h('div', { class: 'dl ' + l.kind },
    h('span', { class: 'n' }, l.old == null ? '' : String(l.old)),
    h('span', { class: 'n' }, l.new == null ? '' : String(l.new)),
    h('span', { class: 's', 'aria-hidden': 'true' }, sign),
    h('span', { class: 't' }, l.kind === 'add' ? h('span', { class: 'sr-only' }, 'added: ') : l.kind === 'del' ? h('span', { class: 'sr-only' }, 'removed: ') : null, l.text || ' '));
}

function fileView(f, opts, open) {
  const id = 'dfile-' + ++seq;
  const [dir, name] = fileLabel(f.path, opts.cwd);
  const lines = parseUnified(f.diff);
  const root = h('section', { class: 'dfile' + (open ? '' : ' closed') });
  const head = h('button', { class: 'dfile-head', type: 'button', 'aria-expanded': String(open), 'aria-controls': id },
    icon('chevDown', 'sm', 'chev'),
    h('span', { class: 'path', title: relPath(f.path, opts.cwd) }, h('bdi', null, h('span', { class: 'dir' }, dir), name)),
    OPS[f.op] ? h('span', { class: 'op ' + f.op }, OPS[f.op]) : null,
    diffStat(f.added, f.removed));
  const table = h('div', { class: 'dtable' });
  const body = h('div', { class: 'dbody', id, tabindex: '0', role: 'region', 'aria-label': `Diff of ${name}` }, table);
  const max = opts.maxLines === 0 ? Infinity : (opts.maxLines || 40);
  let shown = 0;
  function render(limit) {
    const slice = lines.slice(shown, limit);
    add(table, slice.map(lineRow));
    shown += slice.length;
  }
  if (!lines.length) {
    add(table, lineRow({ kind: 'note', text: f.op === 'delete' ? 'File deleted.' : 'No line changes to show.' }));
  } else {
    render(max);
  }
  add(root, head, body);
  if (shown < lines.length) {
    const more = h('button', { class: 'dmore', type: 'button' });
    const label = () => fill(more, `Show ${lines.length - shown} more ${plural(lines.length - shown, 'line', 'lines')}`);
    label();
    more.addEventListener('click', () => {
      render(lines.length);
      more.remove();
    });
    add(body, more);
  }
  if (f.truncated) add(table, lineRow({ kind: 'note', text: 'The diff is cut here — the rest is too long to send to your phone.' }));
  head.addEventListener('click', () => {
    const isOpen = root.classList.toggle('closed') === false;
    head.setAttribute('aria-expanded', String(isOpen));
  });
  return root;
}

/** The diff of one or more files; see the header for opts. */
export function diffView(source, opts = {}) {
  let files = [];
  let added = 0;
  let removed = 0;
  let truncated = false;
  if (Array.isArray(source)) files = source;
  else if (source && Array.isArray(source.files)) {
    files = source.files;
    added = source.added;
    removed = source.removed;
    truncated = !!source.truncated;
  } else if (source && source.path) files = [source];
  if (!added && !removed) {
    for (const f of files) {
      added += f.added || 0;
      removed += f.removed || 0;
    }
  }
  const mode = opts.open || 'auto';
  const root = h('div', { class: 'diff' });
  const showSummary = opts.summary == null ? files.length > 1 : opts.summary;
  if (showSummary) {
    add(root, h('div', { class: 'diff-summary' },
      h('span', null, `${files.length} ${plural(files.length, 'file', 'files')}`), diffStat(added, removed, { bar: true })));
  }
  files.forEach((f, i) => {
    const open = mode === 'all' || (mode === 'auto' && (files.length <= 2 || i === 0));
    add(root, fileView(f, opts, open));
  });
  if (!files.length) add(root, h('p', { class: 'diff-note' }, icon('file', 'sm'), 'No file changes yet.'));
  if (truncated) add(root, h('p', { class: 'diff-note' }, icon('info', 'sm'), 'More files changed than fit here — see them on your computer.'));
  return root;
}
