// Insights page (F6; workstream I): usage, cost, what the rules did.
// Placeholder until the workstream lands.
import { h } from '../dom.js';

export function insightsView() {
  return { el: h('div', { class: 'view' }, h('div', { class: 'page-head' }, h('h1', { class: 'page-title' }, 'Insights'))) };
}
