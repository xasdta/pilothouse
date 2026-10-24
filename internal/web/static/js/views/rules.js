// Rules page (F5; workstream H): every rule with its source, presets,
// "would this be allowed?". Placeholder until the workstream lands.
import { h } from '../dom.js';

export function rulesView() {
  return { el: h('div', { class: 'view' }, h('div', { class: 'page-head' }, h('h1', { class: 'page-title' }, 'Rules'))) };
}
