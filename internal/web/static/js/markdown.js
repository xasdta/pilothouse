// A small, safe Markdown renderer for agent plans and messages. It builds
// DOM nodes from text — never innerHTML — so nothing an agent writes can
// become markup, script or a live link.
//
// Supported: ATX headings, paragraphs, - * + and 1. lists (nested by
// indent, [ ] / [x] task items), > quotes, ``` / ~~~ code blocks with a
// language label, --- rules, pipe tables, **strong**, *em* / _em_,
// ~~del~~, `code`, and links/autolinks shown as text: "label (url)".
//
// Exported API (stable; workstream I may use it for agent text):
//   renderMarkdown(text, opts?) → <div class="md">
//     opts.baseLevel      the HTML level of the text's top heading level (default 3,
//                         for text inside a card under a page h1 and a section h2)
//     opts.dropTitle      drop a leading level-1 heading (shown elsewhere)
//   inlineMarkdown(text) → nodes[] (inline formatting only)
//   markdownTitle(text) → the leading "# Title", or ''

import { h, icon, add } from './dom.js';

const MAX_INPUT = 200000;

/** The text of a leading level-1 heading, or ''. */
export function markdownTitle(text) {
  const m = /^\s*#\s+(.+?)\s*#*\s*$/m.exec(String(text || '').split('\n').find((l) => l.trim()) || '');
  return m ? m[1] : '';
}

// Inline tokens, tried left to right; the earliest match wins. No
// lookbehind: older Safari can't parse it and would refuse the module.
// [kind, regex, group of the content, group of a kept prefix]
const INLINE = [
  ['code', /`([^`\n]+)`/, 1],
  ['link', /\[([^\]\n]+)\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"]*")?\s*\)/, 1],
  ['auto', /<((?:https?|mailto):[^>\s]+)>/, 1],
  ['strong', /\*\*(?=\S)([\s\S]*?\S)\*\*/, 1],
  ['strong', /(^|[\s(])__(?=\S)([\s\S]*?\S)__(?=$|[\s.,;:!?)])/, 2, 1],
  ['del', /~~(?=\S)([\s\S]*?\S)~~/, 1],
  ['em', /\*(?=[^\s*])([\s\S]*?[^\s*])\*/, 1],
  ['em', /(^|[\s(])_(?=\S)([\s\S]*?\S)_(?=$|[\s.,;:!?)])/, 2, 1],
];

/** Inline formatting as nodes. */
export function inlineMarkdown(text, depth = 0) {
  const out = [];
  let s = String(text || '');
  while (s) {
    let best = null;
    for (const [kind, re, g, pg] of INLINE) {
      const m = re.exec(s);
      if (!m) continue;
      const pre = pg ? m[pg].length : 0;
      if (!best || m.index + pre < best.at) best = { kind, m, g, at: m.index + pre };
    }
    if (!best || depth > 6) {
      out.push(s);
      break;
    }
    const { kind, m, g, at } = best;
    if (at > 0) out.push(s.slice(0, at));
    const inner = m[g];
    switch (kind) {
      case 'code':
        out.push(h('code', null, inner));
        break;
      case 'link':
        out.push(h('span', { class: 'md-link' }, inlineMarkdown(inner, depth + 1)));
        if (m[2] !== inner) out.push(' ', h('span', { class: 'md-url' }, '(' + m[2] + ')'));
        break;
      case 'auto':
        out.push(h('span', { class: 'md-url' }, inner));
        break;
      case 'strong':
        out.push(h('strong', null, inlineMarkdown(inner, depth + 1)));
        break;
      case 'del':
        out.push(h('del', null, inlineMarkdown(inner, depth + 1)));
        break;
      case 'em':
        out.push(h('em', null, inlineMarkdown(inner, depth + 1)));
        break;
    }
    s = s.slice(m.index + m[0].length);
  }
  return out;
}

const LIST_RE = /^(\s*)([-*+]|\d{1,9}[.)])\s+(.*)$/;
const FENCE_RE = /^\s{0,3}(`{3,}|~{3,})\s*([\w+#.-]*)/;
const HR_RE = /^\s{0,3}([-*_])(\s*\1){2,}\s*$/;
const HEAD_RE = /^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$/;
const TABLE_SEP_RE = /^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$/;

function splitRow(line) {
  let l = line.trim();
  if (l.startsWith('|')) l = l.slice(1);
  if (l.endsWith('|')) l = l.slice(0, -1);
  return l.split('|').map((c) => c.trim());
}

function indentOf(s) {
  let n = 0;
  for (const c of s) {
    if (c === ' ') n++;
    else if (c === '\t') n += 4;
    else break;
  }
  return n;
}

function blocks(lines, opts, depth) {
  const out = [];
  let i = 0;
  const offset = opts.offset;
  while (i < lines.length) {
    const line = lines[i];
    if (!line.trim()) { i++; continue; }

    const fence = FENCE_RE.exec(line);
    if (fence) {
      const marker = fence[1];
      const body = [];
      i++;
      while (i < lines.length && !lines[i].trim().startsWith(marker)) body.push(lines[i++]);
      i++; // closing fence (or end)
      const pre = h('pre', { tabindex: '0' });
      if (fence[2]) add(pre, h('span', { class: 'lang' }, fence[2]));
      add(pre, h('code', null, body.join('\n')));
      out.push(pre);
      continue;
    }

    const head = HEAD_RE.exec(line);
    if (head) {
      const level = Math.min(6, head[1].length + offset);
      out.push(h('h' + level, null, inlineMarkdown(head[2])));
      i++;
      continue;
    }

    if (HR_RE.test(line)) {
      out.push(h('hr'));
      i++;
      continue;
    }

    if (/^\s{0,3}>/.test(line)) {
      const body = [];
      while (i < lines.length && /^\s{0,3}>/.test(lines[i])) body.push(lines[i++].replace(/^\s{0,3}>\s?/, ''));
      out.push(h('blockquote', null, depth < 4 ? blocks(body, opts, depth + 1) : body.join(' ')));
      continue;
    }

    if (line.includes('|') && i + 1 < lines.length && TABLE_SEP_RE.test(lines[i + 1])) {
      const headCells = splitRow(line);
      i += 2;
      const rows = [];
      while (i < lines.length && lines[i].includes('|') && lines[i].trim()) rows.push(splitRow(lines[i++]));
      out.push(h('table', null,
        h('thead', null, h('tr', null, headCells.map((c) => h('th', { scope: 'col' }, inlineMarkdown(c))))),
        h('tbody', null, rows.map((r) => h('tr', null, headCells.map((_, j) => h('td', null, inlineMarkdown(r[j] || ''))))))));
      continue;
    }

    if (LIST_RE.test(line)) {
      const [el, next] = list(lines, i, opts, depth);
      out.push(el);
      i = next;
      continue;
    }

    // Paragraph: until a blank line or another block starts.
    const para = [];
    while (i < lines.length && lines[i].trim() && !FENCE_RE.test(lines[i]) && !HEAD_RE.test(lines[i]) &&
      !/^\s{0,3}>/.test(lines[i]) && !(para.length && LIST_RE.test(lines[i])) && !(para.length && HR_RE.test(lines[i]))) {
      para.push(lines[i++].trim());
    }
    if (!para.length) { para.push(lines[i++].trim()); }
    out.push(h('p', null, inlineMarkdown(para.join(' '))));
  }
  return out;
}

function list(lines, start, opts, depth) {
  const first = LIST_RE.exec(lines[start]);
  const base = indentOf(first[1]);
  const ordered = /\d/.test(first[2]);
  const el = h(ordered ? 'ol' : 'ul');
  if (ordered && parseInt(first[2], 10) !== 1) el.setAttribute('start', String(parseInt(first[2], 10)));
  let i = start;
  while (i < lines.length) {
    const m = LIST_RE.exec(lines[i]);
    if (!m || indentOf(m[1]) !== base || /\d/.test(m[2]) !== ordered) break;
    const body = [m[3]];
    i++;
    // Continuation and nested lines: indented deeper than the marker.
    while (i < lines.length) {
      const l = lines[i];
      if (!l.trim()) {
        const nextIdx = i + 1;
        if (nextIdx < lines.length && indentOf(lines[nextIdx]) > base && lines[nextIdx].trim()) { body.push(''); i++; continue; }
        break;
      }
      const lm = LIST_RE.exec(l);
      if (indentOf(l) <= base && (lm || !body.length)) break;
      if (indentOf(l) <= base && !lm) {
        body[body.length - 1] += ' ' + l.trim();
        i++;
        continue;
      }
      body.push(l.slice(Math.min(indentOf(l), base + 2)));
      i++;
    }
    let text = body[0];
    let task = null;
    const t = /^\[( |x|X)\]\s+(.*)$/.exec(text);
    if (t) { task = t[1] !== ' '; text = t[2]; }
    const li = h('li', task === null ? null : { class: 'task' + (task ? ' done' : '') });
    if (task !== null) add(li, h('span', { class: 'box', 'aria-hidden': 'true' }, task ? icon('check') : null), h('span', { class: 'sr-only' }, task ? 'Done: ' : 'To do: '));
    const rest = body.slice(1);
    if (rest.some((l) => l.trim())) {
      const nested = depth < 6 ? blocks(rest, opts, depth + 1) : [rest.join(' ')];
      add(li, task !== null ? h('span', null, inlineMarkdown(text), nested) : [inlineMarkdown(text), nested]);
    } else {
      add(li, task !== null ? h('span', null, inlineMarkdown(text)) : inlineMarkdown(text));
    }
    add(el, li);
  }
  return [el, i];
}

/** Renders Markdown into a <div class="md">. */
export function renderMarkdown(text, opts = {}) {
  let src = String(text || '');
  if (src.length > MAX_INPUT) src = src.slice(0, MAX_INPUT) + '\n\n…';
  let lines = src.replace(/\r\n?/g, '\n').split('\n');
  if (opts.dropTitle) {
    const k = lines.findIndex((l) => l.trim());
    if (k >= 0 && /^\s*#\s+/.test(lines[k])) lines = lines.slice(k + 1);
  }
  // Map the text's own top level to baseLevel, so headings stay in order
  // within the page (a plan's "##" under the card is not an h2).
  let top = 6;
  let fenced = false;
  for (const l of lines) {
    if (FENCE_RE.test(l)) fenced = !fenced;
    const m = !fenced && HEAD_RE.exec(l);
    if (m) top = Math.min(top, m[1].length);
  }
  const o = { ...opts, offset: (opts.baseLevel || 3) - top };
  return h('div', { class: 'md' }, blocks(lines, o, 0));
}
