// Live terminal for wrapped sessions (F3; workstream I). Placeholder
// until the workstream lands.
import { h } from '../dom.js';

export function terminalView(id) {
  return { el: h('div', { class: 'view' }, h('div', { class: 'page-head' }, h('h1', { class: 'page-title' }, 'Terminal'))) };
}
