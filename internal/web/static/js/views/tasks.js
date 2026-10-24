// Tasks (F7; workstream I): list, new task, one task with its diff.
// Placeholders until the workstream lands.
import { h } from '../dom.js';

const page = (title) => ({ el: h('div', { class: 'view' }, h('div', { class: 'page-head' }, h('h1', { class: 'page-title' }, title))) });

export function tasksView() { return page('Tasks'); }
export function newTaskView() { return page('New task'); }
export function taskView(id) { return page('Task'); }
