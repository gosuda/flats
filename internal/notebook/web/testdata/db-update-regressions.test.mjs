'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { setup, schema, V, P, O, row, id, cellOf, renderedRows } from './db-fixture.mjs';
import { orderRows } from '../query-worker.js';

const button = (el, text) => el.querySelectorAll('button').find((b) => b.textContent === text || b.getAttribute('aria-label') === text);
const labelled = (el, tag, label) => el.querySelectorAll(tag).find((n) => n.getAttribute('aria-label') === label);
const ref = (id) => ({ id, inc: '7' });
const finished = { property: ref(P.done), op: 'is not checked' };
const scoreSort = { property: ref(P.score), direction: 'desc' };
const titleSort = { property: ref(P.title), direction: 'asc' };
const width = (property, px) => ({ property: ref(property), px });
const definition = (ctx, view = V.table) => ctx.views.find((v) => v.ref.id === view).definition;

function remote(f, change, view = V.table) {
  change(definition(f.ctx, view));
  f.db.update(f.ctx);
}

function accept(f) {
  const op = f.sent.at(-1)[0];
  f.ctx.views.find((v) => v.ref.id === op.view.id).definition = structuredClone(op.definition);
  f.db.update(f.ctx);
}

function mounted(t, options) {
  const f = setup(options);
  t.after(() => f.db.stop());
  return f;
}

function expectWrite(f, expected, view = V.table) {
  assert.deepEqual(f.sent.at(-1), [{ op: 'set_view', view: ref(view), definition: expected }]);
}

test('keyboard resize preserves a filter saved after the header was mounted', (t) => {
  const f = mounted(t);
  const grip = f.el.querySelector(`th[data-prop=${P.score}] .col-resize`);
  remote(f, (def) => { def.filter = [finished]; });
  assert.equal(f.el.querySelector(`th[data-prop=${P.score}] .col-resize`), grip);
  const expected = structuredClone(definition(f.ctx));
  expected.display.widths = [width(P.score, 190)];
  grip.dispatch('keydown', { key: 'ArrowRight' });
  assert.equal(f.sent.length, 0);
  grip.dispatch('keydown', { key: 'Enter' });
  expectWrite(f, expected);
});

test('pointer resize commits against intervening remote settings and other widths', (t) => {
  const f = mounted(t);
  const grip = f.el.querySelector(`th[data-prop=${P.score}] .col-resize`);
  grip.dispatch('pointerdown', { clientX: 100 });
  document.dispatch('pointermove', { clientX: 140 });
  remote(f, (def) => {
    def.name = 'Remote name';
    def.filter = [finished];
    def.sort = [scoreSort];
    def.display.widths = [width(P.due, 300)];
  });
  const expected = structuredClone(definition(f.ctx));
  expected.display.widths = [width(P.score, 220), width(P.due, 300)];
  assert.equal(f.sent.length, 0);
  document.dispatch('pointerup');
  assert.equal(f.sent.length, 1);
  expectWrite(f, expected);
});

test('view settings retain consecutive visibility changes when renaming', (t) => {
  const f = mounted(t);
  button(f.el, 'View settings').click();
  const pop = f.el.querySelector('.db-pop');
  const score = pop.querySelectorAll('.db-visible label').find((n) => n.textContent === 'Score').querySelector('input');
  score.checked = false;
  score.dispatch('change');
  accept(f);
  remote(f, (def) => { def.filter = [finished]; def.display.widths = [width(P.due, 310)]; });
  const expected = structuredClone(definition(f.ctx));
  expected.name = 'My tasks';
  const name = labelled(pop, 'input', 'View name');
  name.value = expected.name;
  name.dispatch('change');
  expectWrite(f, expected);
});

test('a visibility toggle changes only its property in the latest saved view', (t) => {
  const f = mounted(t);
  button(f.el, 'View settings').click();
  const pop = f.el.querySelector('.db-pop');
  remote(f, (def) => {
    def.name = 'Remote';
    def.filter = [finished];
    def.display.visible = def.display.visible.filter((p) => p.id !== P.due);
  });
  const expected = structuredClone(definition(f.ctx));
  expected.display.visible = expected.display.visible.filter((p) => p.id !== P.score);
  const score = pop.querySelectorAll('.db-visible label').find((n) => n.textContent === 'Score').querySelector('input');
  score.checked = false;
  score.dispatch('change');
  expectWrite(f, expected);
});

test('kanban empty-column settings preserve a remote name, filter and visible properties', (t) => {
  const f = mounted(t, { view: V.board });
  button(f.el, 'View settings').click();
  const pop = f.el.querySelector('.db-pop');
  remote(f, (def) => {
    def.name = 'Remote board';
    def.filter = [finished];
    def.display.visible = [ref(P.title), ref(P.due)];
  }, V.board);
  const expected = structuredClone(definition(f.ctx, V.board));
  expected.display.show_empty = false;
  const toggle = pop.querySelectorAll('label').find((n) => n.textContent === 'Show empty columns').querySelector('input');
  toggle.checked = false;
  toggle.dispatch('change');
  expectWrite(f, expected, V.board);
});

test('kanban grouping preserves consecutive and remote settings', (t) => {
  const extra = id('pr_', 9);
  const s = schema();
  s.properties.push({ ref: ref(extra), definition: { name: 'Priority', type: 'select' }, options: [] });
  const f = mounted(t, { view: V.board, schema: s });
  button(f.el, 'View settings').click();
  const pop = f.el.querySelector('.db-pop');
  const toggle = pop.querySelectorAll('label').find((n) => n.textContent === 'Show empty columns').querySelector('input');
  toggle.checked = false;
  toggle.dispatch('change');
  accept(f);
  remote(f, (def) => { def.name = 'Remote board'; def.filter = [finished]; }, V.board);
  const expected = structuredClone(definition(f.ctx, V.board));
  expected.display.group_by = ref(extra);
  const group = labelled(pop, 'select', 'Group by');
  group.value = extra;
  group.dispatch('change');
  expectWrite(f, expected, V.board);
});

test('an open filter form appends to the latest filters without reverting other controls', (t) => {
  const f = mounted(t);
  button(f.el, 'Add filter').click();
  const form = f.el.querySelector('.db-pop form');
  const [prop, op] = form.querySelectorAll('select');
  prop.value = P.title;
  prop.dispatch('change');
  op.value = 'contains';
  op.dispatch('change');
  form.querySelector('.db-value').value = 'Task';
  remote(f, (def) => {
    def.name = 'Remote'; def.filter = [finished]; def.sort = [scoreSort];
    def.display.widths = [width(P.score, 240)];
  });
  const expected = structuredClone(definition(f.ctx));
  expected.filter.push({ property: ref(P.title), op: 'contains', value: 'Task' });
  form.dispatch('submit');
  expectWrite(f, expected);
});

test('an open add-sort form preserves intervening sort keys, filters and widths', (t) => {
  const f = mounted(t);
  button(f.el, 'Sort').click();
  const form = f.el.querySelector('.db-pop form');
  labelled(form, 'select', 'Sort by').value = P.due;
  remote(f, (def) => {
    def.name = 'Remote'; def.filter = [finished]; def.sort = [scoreSort];
    def.display.widths = [width(P.score, 260)];
  });
  const expected = structuredClone(definition(f.ctx));
  expected.sort.push({ property: ref(P.due), direction: 'asc' });
  form.dispatch('submit');
  expectWrite(f, expected);
});

for (const remove of [false, true]) {
  test(`an open sort form ${remove ? 'removes' : 'edits'} its original key after remote reordering`, (t) => {
    const f = mounted(t, { view: V.sorted });
    const chip = f.el.querySelectorAll('button').find((n) => n.getAttribute('aria-label')?.startsWith('Sorted by Score'));
    chip.click();
    const form = f.el.querySelector('.db-pop form');
    remote(f, (def) => { def.name = 'Remote'; def.filter = [finished]; def.sort = [titleSort, scoreSort]; }, V.sorted);
    const expected = structuredClone(definition(f.ctx, V.sorted));
    if (remove) {
      expected.sort = [titleSort];
      button(form, 'Remove sort').click();
    } else {
      labelled(form, 'select', 'Direction').value = 'asc';
      expected.sort[1].direction = 'asc';
      form.dispatch('submit');
    }
    expectWrite(f, expected, V.sorted);
  });
}

test('a combined row and option deletion repaints only surviving rows and stays stable', (t) => {
  const f = mounted(t);
  f.data.rows = f.data.rows.filter((r) => r.ref.id !== row(0));
  for (const r of f.data.rows) if (r.cells[P.status].value === O.todo) r.cells[P.status].value = null;
  const status = f.ctx.schema.properties.find((p) => p.ref.id === P.status);
  status.options = status.options.filter((o) => o.ref.id !== O.todo);
  assert.doesNotThrow(() => f.db.update(f.ctx));
  assert.deepEqual(renderedRows(f.el).map((n) => n.dataset.row), [1, 2, 3, 4, 5].map(row));
  assert.equal(cellOf(f.el, row(4), P.status).textContent, 'Empty');
  assert.equal(f.el.querySelector('table').getAttribute('aria-rowcount'), '6');
  assert.doesNotThrow(() => f.db.update(f.ctx));
  assert.deepEqual(renderedRows(f.el).map((n) => n.dataset.row), [1, 2, 3, 4, 5].map(row));
  f.data.rows = [];
  f.ctx.presence = [{ label: 'Another editor', edit: null }];
  assert.doesNotThrow(() => f.db.update(f.ctx));
  assert.deepEqual(renderedRows(f.el), []);
  assert.equal(f.el.querySelector('table').getAttribute('aria-rowcount'), '1');
});

class QueryWorker {
  static current;
  constructor() { QueryWorker.current = this; }
  postMessage(request) { this.request = request; }
  flush() {
    const { id, schema, view, rows, keep } = this.request;
    this.onmessage({ data: { id, order: orderRows(schema, view, rows, keep) } });
  }
  terminate() {}
}

for (const blurOnRemoval of [false, true]) {
  test(`deleted rows leave the virtual list before a delayed query or partial repaint${blurOnRemoval ? ', including synchronous blur' : ''}`, (t) => {
    const previous = globalThis.Worker;
    globalThis.Worker = QueryWorker;
    t.after(() => { if (previous === undefined) delete globalThis.Worker; else globalThis.Worker = previous; });
    const f = mounted(t);
    const worker = QueryWorker.current;
    worker.flush();
    const cell = cellOf(f.el, row(0), P.title);
    cell.focus();
    cell.dispatch('keydown', { key: 'Enter' });
    const input = cell.querySelector('input');
    if (blurOnRemoval) {
      const tr = cell.parentNode;
      const remove = tr.remove.bind(tr);
      tr.remove = () => { remove(); input.dispatch('blur'); };
    }
    f.data.rows = f.data.rows.filter((r) => r.ref.id !== row(0));
    assert.doesNotThrow(() => f.db.update(f.ctx));
    assert.ok(!cellOf(f.el, row(0), P.title), 'deletion is visible before the worker responds');
    assert.doesNotThrow(() => input.dispatch('keydown', { key: 'Escape' }));
    assert.doesNotThrow(() => f.el.querySelector('.tablewrap').dispatch('scroll'));
    assert.doesNotThrow(() => worker.flush());
    assert.deepEqual(renderedRows(f.el).map((n) => n.dataset.row), [1, 2, 3, 4, 5].map(row));
    f.db.update(f.ctx);
    worker.flush();
    assert.equal(f.el.querySelector('table').getAttribute('aria-rowcount'), '6');
  });
}
