// A task database in the wire shapes the page editor hands to
// renderDatabase, plus a context that records submitted actions.
'use strict';


import { installDOM, mount } from './db-dom.mjs';
import { renderDatabase } from '../table.js';

const A = 'abcdefghijklmnopqrstuvwxyz234567';
export const id = (prefix, n) => prefix + 'a'.repeat(23) + A[Math.floor(n / 32) % 32] + A[n % 32] + 'a';

export const DB = id('d_', 1);
export const P = { title: id('pr_', 1), status: id('pr_', 2), score: id('pr_', 3), done: id('pr_', 4), due: id('pr_', 5) };
export const O = { todo: id('o_', 1), doing: id('o_', 2), done: id('o_', 3) };
export const V = { table: id('v_', 1), sorted: id('v_', 2), board: id('v_', 3), sortedBoard: id('v_', 4), filtered: id('v_', 5) };
export const row = (n) => id('r_', n);
const r = (x) => ({ id: x, inc: '7' });

export function schema() {
  return {
    database: r(DB),
    title: 'Tasks',
    properties: [
      { ref: r(P.title), definition: { name: 'Name', type: 'title' }, options: [] },
      { ref: r(P.status), definition: { name: 'Status', type: 'select' }, options: [
        { ref: r(O.todo), name: 'Todo', color: 'gray' },
        { ref: r(O.doing), name: 'Doing', color: 'blue' },
        { ref: r(O.done), name: 'Done', color: 'green' },
      ] },
      { ref: r(P.score), definition: { name: 'Score', type: 'number', format: 'plain' }, options: [] },
      { ref: r(P.done), definition: { name: 'Finished', type: 'checkbox' }, options: [] },
      { ref: r(P.due), definition: { name: 'Due', type: 'date' }, options: [] },
    ],
  };
}

const table = (name, extra = {}) => ({
  name,
  display: { layout: 'table', visible: Object.values(P).map(r), widths: [] },
  sort: [],
  filter: [],
  ...extra,
});
const board = (name, extra = {}) => ({
  name,
  display: { layout: 'kanban', group_by: r(P.status), visible: [r(P.title), r(P.score)], show_empty: true },
  sort: [],
  filter: [],
  ...extra,
});

export function views() {
  return [
    { ref: r(V.table), definition: table('All') },
    { ref: r(V.sorted), definition: table('By score', { sort: [{ property: r(P.score), direction: 'desc' }] }) },
    { ref: r(V.board), definition: board('Board') },
    { ref: r(V.sortedBoard), definition: board('Sorted board', { sort: [{ property: r(P.score), direction: 'asc' }] }) },
    { ref: r(V.filtered), definition: table('Open', { filter: [{ property: r(P.done), op: 'is not checked' }] }) },
  ];
}

const pos = (i) => 'a' + String(i).padStart(5, '0');

export function rows(n = 6) {
  const status = [O.todo, O.doing, O.done, null];
  return Array.from({ length: n }, (_, i) => ({
    ref: r(row(i)),
    position: pos(i),
    cells: {
      [P.title]: { type: 'title', value: `Task ${i}` },
      [P.status]: { type: 'select', value: status[i % 4] },
      [P.score]: { type: 'number', value: String((i * 7) % 10) },
      [P.done]: { type: 'checkbox', value: false },
    },
  }));
}

// setup renders a database and returns the context, the recorded actions
// and the view handle. Options select the starting view and row count.
export function setup({ view = V.table, count = 6, clientHeight = 360, ...extra } = {}) {
  installDOM();
  const el = mount(clientHeight);
  const sent = [];
  const data = { rows: rows(count) };
  const ctx = {
    schema: schema(),
    views: views(),
    rows: () => data.rows,
    onOps: (ops) => sent.push(ops),
    presence: [],
    ...extra,
  };
  const db = renderDatabase({ id: id('b_', 1) }, el, ctx);
  if (view !== V.table) db.selectView(view);
  el.sizeScrollers();
  db.update(ctx);
  return { el, ctx, db, sent, data };
}

export const cellOf = (el, rowId, prop) => el.querySelector(`tr[data-row=${rowId}] td[data-prop=${prop}]`);
export const renderedRows = (el) => el.querySelectorAll('tbody tr').filter((t) => t.dataset.row);
