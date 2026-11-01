'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { dataTransfer } from './db-dom.mjs';
import { setup, V, P, O, row } from './db-fixture.mjs';

const card = (el, id) => el.querySelector(`.card[data-row=${id}]`);
const column = (el, option) => el.querySelectorAll('.col').find((c) => c.dataset.col === (option ?? ''));

function drag(from, to) {
  const dt = dataTransfer();
  from.dispatch('dragstart', { dataTransfer: dt });
  return to.dispatch('drop', { dataTransfer: dt });
}

test('kanban shows one column per option in option order plus No value', () => {
  const { el } = setup({ view: V.board });
  const names = el.querySelectorAll('.col .col-h .opt').map((n) => n.textContent);
  assert.deepEqual(names, ['Todo', 'Doing', 'Done', 'No value']);
  assert.ok(column(el, O.todo).contains(card(el, row(0))));
  assert.ok(column(el, null).contains(card(el, row(3))), 'a row without a value is in No value');
});

test('dropping a card on another column is one unit: move plus grouping cell', () => {
  const { el, sent } = setup({ view: V.board });
  // Task 0 is Todo; drop it above Task 1, which is Doing.
  drag(card(el, row(0)), card(el, row(1)));
  assert.equal(sent.length, 1, 'one action');
  const [action] = sent;
  assert.deepEqual(action.map((o) => o.op), ['move_rows', 'set_cell']);
  assert.ok(action[0].unit_id, 'the operations carry a unit id');
  assert.equal(action[0].unit_id, action[1].unit_id, 'both operations share it');
  assert.deepEqual(action[0].rows, [{ id: row(0), inc: '7' }]);
  assert.deepEqual(action[0].before, { id: row(1), inc: '7' });
  assert.deepEqual(action[1].cell, { property: { id: P.status, inc: '7' }, value: { id: O.doing, inc: '7' } });
});

test('dropping a card on a column background moves it to the end of the order', () => {
  const { el, sent } = setup({ view: V.board });
  drag(card(el, row(0)), column(el, null));
  const [action] = sent;
  assert.deepEqual(action.map((o) => o.op), ['move_rows', 'set_cell']);
  assert.equal(action[0].before, null);
  assert.equal(action[1].cell.value, null, 'No value clears the cell');
  assert.equal(action[0].unit_id, action[1].unit_id);
});

test('a reorder within one column is a single move without a unit', () => {
  const { el, sent } = setup({ count: 9, view: V.board });
  // Tasks 0, 4 and 8 are Todo.
  drag(card(el, row(8)), card(el, row(0)));
  assert.equal(sent.length, 1);
  assert.deepEqual(sent[0].map((o) => o.op), ['move_rows']);
  assert.equal(sent[0][0].unit_id, undefined);
});

test('in a sorted kanban a drop only sets the grouping cell', () => {
  const { el, sent } = setup({ view: V.sortedBoard });
  drag(card(el, row(0)), card(el, row(1)));
  assert.deepEqual(sent.map((a) => a.map((o) => o.op)), [['set_cell']]);
  assert.ok(el.textContent.includes('reordering is off'), 'the view says why reordering is unavailable');
});

test('a sorted kanban refuses keyboard reordering and explains why', () => {
  const { el, sent } = setup({ count: 9, view: V.sortedBoard });
  const c = card(el, row(4));
  c.dispatch('keydown', { key: 'ArrowUp', altKey: true });
  assert.equal(sent.length, 0);
  assert.match(el.querySelector('[aria-live]').textContent, /reordering is off/);
});

test('arrow keys move a card across columns as a cell write', () => {
  const { el, sent } = setup({ view: V.board });
  card(el, row(0)).dispatch('keydown', { key: 'ArrowRight' });
  assert.deepEqual(sent.map((a) => a.map((o) => o.op)), [['set_cell']]);
  assert.deepEqual(sent[0][0].cell.value, { id: O.doing, inc: '7' });
});

test('a card moved out of a filtered column stays visible and marked', () => {
  const { el, ctx, data, db } = setup({ view: V.board });
  // Filter the board to Todo cards.
  ctx.views = ctx.views.map((v) => (v.ref.id === V.board
    ? { ...v, definition: { ...v.definition, filter: [{ property: { id: P.status, inc: '7' }, op: 'is', value: { id: O.todo, inc: '7' } }] } }
    : v));
  db.update(ctx);
  assert.ok(card(el, row(0)));
  assert.equal(card(el, row(1)), null, 'Doing cards are filtered out');
  card(el, row(0)).dispatch('keydown', { key: 'ArrowRight' });
  data.rows = data.rows.map((r) => (r.ref.id === row(0) ? { ...r, cells: { ...r.cells, [P.status]: { type: 'select', value: O.doing } } } : r));
  db.update(ctx);
  const moved = card(el, row(0));
  assert.ok(moved, 'this tab keeps the card it moved');
  assert.ok(moved.className.includes('hidden-by-filter'));
  assert.ok(column(el, O.doing).contains(moved));
  db.selectView(V.table);
  db.selectView(V.board);
  assert.equal(card(el, row(0)), null, 'leaving the view lets the filter apply');
});
