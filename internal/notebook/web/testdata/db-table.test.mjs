'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { dataTransfer } from './db-dom.mjs';
import { setup, V, P, O, row, cellOf, renderedRows } from './db-fixture.mjs';
import { SORTED_TABLE_NOTE } from '../table.js';

const labelled = (el, tag, label) => el.querySelectorAll(tag).find((n) => n.getAttribute('aria-label') === label) ?? null;
const button = (el, text) => el.querySelectorAll('button').find((b) => b.textContent === text || b.getAttribute('aria-label') === text);

function drag(from, to) {
  const dt = dataTransfer();
  const start = from.dispatch('dragstart', { dataTransfer: dt });
  return { start, drop: to.dispatch('drop', { dataTransfer: dt }) };
}

test('the table exposes row count and row index to assistive technology', () => {
  const { el } = setup({ count: 500 });
  const table = el.querySelector('table');
  assert.equal(table.getAttribute('aria-rowcount'), '501', 'rows plus the header row');
  const shown = renderedRows(el);
  assert.ok(shown.length < 60, `only the viewport and overscan render, got ${shown.length}`);
  assert.equal(shown[0].getAttribute('aria-rowindex'), '2');
  assert.equal(shown[3].getAttribute('aria-rowindex'), '5');
  assert.equal(el.querySelector('thead tr').getAttribute('aria-rowindex'), '1');
});

test('keyboard navigation materializes off-screen rows', () => {
  const { el } = setup({ count: 500 });
  cellOf(el, row(0), P.title).focus();
  cellOf(el, row(0), P.title).dispatch('keydown', { key: 'End', ctrlKey: true });
  const last = cellOf(el, row(499), P.title);
  assert.ok(last, 'the last row is rendered');
  assert.equal(document.activeElement, last);
  assert.equal(last.parentNode.getAttribute('aria-rowindex'), '501');
  last.dispatch('keydown', { key: 'ArrowUp' });
  assert.equal(document.activeElement, cellOf(el, row(498), P.title));
  document.activeElement.dispatch('keydown', { key: 'ArrowRight' });
  assert.equal(document.activeElement, cellOf(el, row(498), P.status));
});

test('virtualization keeps the focused row while it scrolls out of view', () => {
  const { el } = setup({ count: 500 });
  const cell = cellOf(el, row(2), P.title);
  cell.focus();
  const scroller = el.querySelector('.tablewrap');
  scroller.scrollTop = 36 * 300;
  scroller.dispatch('scroll');
  const ids = renderedRows(el).map((r) => r.dataset.row);
  assert.ok(ids.includes(row(300)), 'the new viewport is rendered');
  assert.ok(ids.includes(row(2)), 'the focused row stays rendered');
  assert.ok(!ids.includes(row(100)), 'rows between are not');
  assert.equal(cellOf(el, row(2), P.title), cell, 'the same element, so focus survives');
  assert.equal(document.activeElement, cell);
  assert.equal(cell.parentNode.getAttribute('aria-rowindex'), '4');
});

test('an open cell editor survives a remote change to another row', () => {
  const { el, ctx, data, db, sent } = setup({ count: 50 });
  const cell = cellOf(el, row(1), P.title);
  cell.focus();
  cell.dispatch('keydown', { key: 'Enter' });
  const input = cell.querySelector('input');
  assert.equal(document.activeElement, input);
  input.value = 'Renamed';
  data.rows = data.rows.map((r) => (r.ref.id === row(5) ? { ...r, cells: { ...r.cells, [P.title]: { type: 'title', value: 'Remote' } } } : r));
  db.update(ctx);
  assert.equal(document.activeElement, input, 'the editor kept focus');
  assert.equal(document.moved, 0, 'the focused row was not moved in the DOM');
  assert.equal(cellOf(el, row(5), P.title).textContent, 'Remote');
  input.dispatch('keydown', { key: 'Enter' });
  assert.deepEqual(sent.at(-1), [{ op: 'set_cell', row: { id: row(1), inc: '7' }, cell: { property: { id: P.title, inc: '7' }, value: 'Renamed' } }]);
});

test('typed cell editors refuse invalid input and write canonical values', () => {
  const { el, sent } = setup();
  const edit = (prop, text) => {
    const cell = cellOf(el, row(0), prop);
    cell.focus();
    cell.dispatch('keydown', { key: 'Enter' });
    const input = cell.querySelector('input');
    input.value = text;
    input.dispatch('keydown', { key: 'Enter' });
    return cell;
  };
  const bad = edit(P.score, '12abc');
  assert.equal(bad.querySelector('input').getAttribute('aria-invalid'), 'true');
  assert.match(bad.querySelector('.db-error').textContent, /number/);
  assert.equal(sent.length, 0);
  bad.querySelector('input').dispatch('keydown', { key: 'Escape' });
  edit(P.score, '+001.50e1');
  assert.equal(sent.at(-1)[0].cell.value, '15');
  edit(P.due, '2023-02-29');
  assert.equal(sent.length, 1, 'an impossible date is refused');
  cellOf(el, row(0), P.due).querySelector('input').dispatch('keydown', { key: 'Escape' });
  edit(P.due, '2024-02-29T23:59:59');
  assert.equal(sent.at(-1)[0].cell.value, '2024-02-29T23:59:59');
  cellOf(el, row(0), P.done).querySelector('input').dispatch('change', { target: { checked: true } });
  assert.deepEqual(sent.at(-1)[0].cell, { property: { id: P.done, inc: '7' }, value: true });
});

test('a select cell writes an option reference; a new option and its use are one unit', () => {
  const { el, sent } = setup();
  cellOf(el, row(0), P.status).click();
  const pop = el.querySelector('.db-pop');
  pop.querySelectorAll('button').find((b) => b.textContent === 'Done').click();
  assert.deepEqual(sent.at(-1)[0].cell.value, { id: O.done, inc: '7' });
  cellOf(el, row(0), P.status).click();
  const input = el.querySelector('.db-pop input');
  input.value = 'Blocked';
  input.dispatch('keydown', { key: 'Enter' });
  const [create, set] = sent.at(-1);
  assert.equal(create.op, 'create_option');
  assert.match(create.proposed_id, /^o_[a-z2-7]{26}$/);
  assert.deepEqual(set.cell.value, { tmp: create.ref });
  assert.ok(create.unit_id && create.unit_id === set.unit_id);
});

test('the filter form refuses an empty value', () => {
  const { el, sent } = setup();
  button(el, 'Add filter').click();
  const form = el.querySelector('.db-pop form');
  const [prop, op] = form.querySelectorAll('select');
  prop.value = P.title;
  prop.dispatch('change');
  op.value = 'contains';
  op.dispatch('change');
  const value = form.querySelector('.db-value');
  value.value = '   ';
  form.dispatch('submit');
  assert.equal(sent.length, 0, 'nothing is written');
  assert.equal(value.getAttribute('aria-invalid'), 'true');
  assert.match(form.querySelector('.db-error').textContent, /Enter a value/);
  // Choosing "is empty" needs no value.
  op.value = 'is empty';
  op.dispatch('change');
  assert.equal(form.querySelector('.db-value'), null);
  form.dispatch('submit');
  assert.equal(sent.length, 1);
  assert.deepEqual(sent[0][0].definition.filter, [{ property: { id: P.title, inc: '7' }, op: 'is empty' }]);
});

test('the filter form offers only the operators of the property type', () => {
  const { el, sent } = setup();
  button(el, 'Add filter').click();
  const form = el.querySelector('.db-pop form');
  const [prop, op] = form.querySelectorAll('select');
  prop.value = P.status;
  prop.dispatch('change');
  assert.deepEqual(op.children.map((o) => o.textContent), ['is', 'is not', 'is any of', 'is empty', 'is not empty']);
  op.value = 'is any of';
  op.dispatch('change');
  form.dispatch('submit');
  assert.equal(sent.length, 0, 'an empty option set is refused');
  const boxes = form.querySelectorAll('.db-value input');
  boxes[0].checked = true;
  form.dispatch('submit');
  assert.deepEqual(sent[0][0].definition.filter[0], { property: { id: P.status, inc: '7' }, op: 'is any of', value: [{ id: O.todo, inc: '7' }] });
  prop.value = P.score;
  prop.dispatch('change');
  assert.deepEqual(op.children.map((o) => o.textContent), ['=', '≠', '<', '≤', '>', '≥', 'is empty', 'is not empty']);
  form.querySelector('.db-value').value = 'ten';
  form.dispatch('submit');
  assert.equal(sent.length, 1, 'a non-number is refused');
});

test('a sorted view blocks reorder and says why', () => {
  const { el, sent } = setup({ view: V.sorted });
  assert.ok(el.textContent.includes(SORTED_TABLE_NOTE));
  const handle = el.querySelector(`tr[data-row=${row(0)}] .row-handle`);
  assert.equal(handle.getAttribute('draggable'), null, 'rows cannot be dragged');
  const { start } = drag(handle, el.querySelector(`tr[data-row=${row(1)}]`));
  assert.ok(start.defaultPrevented, 'a drag that starts anyway is cancelled');
  const cell = cellOf(el, row(0), P.title);
  cell.focus();
  cell.dispatch('keydown', { key: 'ArrowDown', altKey: true });
  assert.equal(sent.length, 0, 'no move is written');
  assert.equal(el.querySelector('[aria-live]').textContent, SORTED_TABLE_NOTE);
});

test('an unsorted table reorders by drag and by keyboard', () => {
  const { el, sent } = setup();
  drag(el.querySelector(`tr[data-row=${row(3)}] .row-handle`), el.querySelector(`tr[data-row=${row(1)}]`));
  assert.deepEqual(sent.at(-1), [{ op: 'move_rows', rows: [{ id: row(3), inc: '7' }], before: { id: row(1), inc: '7' } }]);
  const cell = cellOf(el, row(1), P.title);
  cell.focus();
  cell.dispatch('keydown', { key: 'ArrowDown', altKey: true });
  assert.deepEqual(sent.at(-1), [{ op: 'move_rows', rows: [{ id: row(1), inc: '7' }], before: { id: row(3), inc: '7' } }]);
});

test('column resize commits one whole view write on release', () => {
  const { el, sent } = setup();
  const grip = el.querySelector(`th[data-prop=${P.score}] .col-resize`);
  grip.dispatch('pointerdown', { clientX: 100 });
  for (let x = 110; x <= 200; x += 10) document.dispatch('pointermove', { clientX: x });
  assert.equal(sent.length, 0, 'moving writes nothing');
  assert.equal(el.querySelector(`col[data-prop=${P.score}]`).style.width, '280px');
  document.dispatch('pointerup', {});
  assert.equal(sent.length, 1);
  const [op] = sent[0];
  assert.equal(op.op, 'set_view');
  assert.deepEqual(op.view, { id: V.table, inc: '7' });
  assert.equal(op.definition.name, 'All');
  assert.equal(op.definition.display.visible.length, 5, 'the whole definition is written');
  assert.deepEqual(op.definition.display.widths, [{ property: { id: P.score, inc: '7' }, px: 280 }]);
  grip.dispatch('pointerdown', { clientX: 0 });
  document.dispatch('pointermove', { clientX: -5000 });
  document.dispatch('pointerup', {});
  assert.equal(sent.at(-1)[0].definition.display.widths[0].px, 80, 'clamped to the minimum');
});

test('a row created in a filtered view gets matching defaults and stays marked when it stops matching', () => {
  const { el, ctx, data, db, sent } = setup({ view: V.filtered });
  button(el, 'New row').click();
  const [create] = sent.at(-1);
  assert.equal(create.op, 'create_row');
  assert.deepEqual(create.view, { id: V.filtered, inc: '7' });
  assert.deepEqual(create.cells, [], '“is not checked” already holds for an empty checkbox');
  // Mark Task 0 finished: it no longer matches "Open" but stays for this tab.
  cellOf(el, row(0), P.done).querySelector('input').dispatch('change', { target: { checked: true } });
  data.rows = data.rows.map((r) => (r.ref.id === row(0) ? { ...r, cells: { ...r.cells, [P.done]: { type: 'checkbox', value: true } } } : r));
  db.update(ctx);
  const tr = el.querySelector(`tr[data-row=${row(0)}]`);
  assert.ok(tr, 'the row is retained');
  assert.ok(tr.className.includes('hidden-by-filter'));
  assert.match(tr.textContent, /Hidden by this view’s filter/);
  db.selectView(V.table);
  db.selectView(V.filtered);
  assert.equal(el.querySelector(`tr[data-row=${row(0)}]`), null, 'leaving the view drops it');
});

for (const visible of [[P.title], []]) {
  test(`adding a property shows its column in only the clicked view${visible.length ? '' : ' when the title needs restoring'}`, () => {
    const { el, ctx, db, sent } = setup();
    const target = ctx.views.find((v) => v.ref.id === V.table);
    target.definition.display.visible = visible.map((id) => ({ id, inc: '7' }));
    target.definition.display.widths = visible.length ? [{ property: { id: P.title, inc: '7' }, px: 320 }] : [];
    target.definition.sort = [{ property: { id: P.score, inc: '7' }, direction: 'desc' }];
    target.definition.filter = [{ property: { id: P.done, inc: '7' }, op: 'is not checked' }];
    db.update(ctx);
    const before = structuredClone(ctx.views);

    button(el, 'Add property').click();
    assert.equal(sent.length, 1, 'creation and visibility are submitted as one action');
    assert.equal(sent[0].length, 2);
    const [create, set] = sent[0];
    assert.equal(create.op, 'create_property');
    assert.deepEqual(create.definition, { name: 'Property', type: 'text' });
    assert.ok(create.unit_id && create.unit_id === set.unit_id, 'one undo unit');
    assert.equal(set.op, 'set_view');
    assert.deepEqual(set.view, target.ref);
    assert.deepEqual(set.definition, {
      ...before[0].definition,
      display: {
        ...before[0].definition.display,
        visible: [{ id: P.title, inc: '7' }, { tmp: create.ref }],
      },
    });
    assert.deepEqual(ctx.views, before, 'creating the action does not mutate saved views');

    const fresh = { id: create.proposed_id, inc: '8' };
    ctx.schema.properties.push({ ref: fresh, definition: create.definition, options: [] });
    target.definition = {
      ...set.definition,
      display: { ...set.definition.display, visible: [{ id: P.title, inc: '7' }, fresh] },
    };
    db.update(ctx);
    assert.deepEqual(el.querySelectorAll('th[data-prop]').map((th) => th.dataset.prop), [P.title, fresh.id]);
    assert.equal(el.querySelector(`th[data-prop=${fresh.id}] .th-name`).textContent, 'Property');
    assert.deepEqual(ctx.views.slice(1), before.slice(1), 'all other saved views stay unchanged');
    db.selectView(V.sorted);
    assert.equal(el.querySelector(`th[data-prop=${fresh.id}]`), null);
    db.stop();
  });
}

test('adding a property preserves view changes received since the header rendered', () => {
  const { el, ctx, db, sent } = setup({ view: V.sorted });
  const add = button(el, 'Add property');
  const target = ctx.views.find((v) => v.ref.id === V.sorted);
  target.definition.name = 'Current tasks';
  target.definition.filter = [{ property: { id: P.done, inc: '7' }, op: 'is not checked' }];
  db.update(ctx);
  assert.equal(button(el, 'Add property'), add, 'the header is reused');
  add.click();
  const [create, set] = sent[0];
  assert.ok(set, 'the action also updates the view');
  assert.deepEqual(set.view, target.ref);
  assert.deepEqual(set.definition, {
    ...target.definition,
    display: { ...target.definition.display, visible: [...target.definition.display.visible, { tmp: create.ref }] },
  });
  db.stop();
});

test('rows, properties and views can be added and deleted', () => {
  const { el, sent } = setup();
  button(el, 'Add property').click();
  const [prop] = sent.at(-1);
  assert.equal(prop.op, 'create_property');
  assert.deepEqual(prop.definition, { name: 'Property', type: 'text' });
  assert.match(prop.proposed_id, /^pr_/);
  const handle = el.querySelector(`tr[data-row=${row(2)}] .row-handle`);
  handle.click();
  button(el, 'Delete row').click();
  assert.deepEqual(sent.at(-1), [{ op: 'delete_rows', rows: [{ id: row(2), inc: '7' }] }]);
  button(el, 'Add view').click();
  button(el, 'Kanban').click();
  const [view] = sent.at(-1);
  assert.equal(view.op, 'create_view');
  assert.equal(view.definition.display.layout, 'kanban');
  assert.deepEqual(view.definition.display.group_by, { id: P.status, inc: '7' });
  button(el, 'View settings').click();
  button(el, 'Delete view').click();
  assert.deepEqual(sent.at(-1), [{ op: 'delete_view', view: { id: V.table, inc: '7' } }]);
});

test('property settings rename, delete and manage options', () => {
  const { el, sent } = setup();
  el.querySelector(`th[data-prop=${P.status}] .th-name`).click();
  const pop = el.querySelector('.db-pop');
  const name = labelled(pop, 'input', 'Property name');
  name.value = 'Name';
  name.dispatch('change');
  assert.equal(sent.length, 0, 'a duplicate name is refused');
  name.value = 'State';
  name.dispatch('change');
  assert.deepEqual(sent.at(-1), [{ op: 'set_property', property: { id: P.status, inc: '7' }, definition: { name: 'State', type: 'select' } }]);
  button(pop, 'Add option').click();
  assert.equal(sent.at(-1)[0].op, 'create_option');
  button(pop, 'Delete option Doing').click();
  assert.deepEqual(sent.at(-1), [{ op: 'delete_option', option: { id: O.doing, inc: '7' } }]);
  button(pop, 'Delete property').click();
  assert.deepEqual(sent.at(-1), [{ op: 'delete_property', property: { id: P.status, inc: '7' } }]);
  el.querySelector(`th[data-prop=${P.title}] .th-name`).click();
  const title = el.querySelector('.db-pop');
  assert.equal(labelled(title, 'select', 'Type'), null, 'the title type cannot change');
  assert.equal(button(title, 'Delete property'), undefined, 'the title cannot be deleted');
});

const tick = () => new Promise((r) => setTimeout(r, 0));

test('a type change previews what it empties and re-confirms when that changes', async () => {
  const previews = [
    { preview: 'p1', emptied: 2, options_created: 0 },
    { preview: 'p2', emptied: 3, options_created: 0 },
    { preview: 'p2', emptied: 3, options_created: 0 },
  ];
  const asked = [];
  const { el, sent } = setup({ preview: async (property, definition) => { asked.push({ property, definition }); return previews.shift(); } });
  el.querySelector(`th[data-prop=${P.score}] .th-name`).click();
  const type = labelled(el.querySelector('.db-pop'), 'select', 'Type');
  type.value = 'checkbox';
  type.dispatch('change');
  await tick();
  assert.match(el.querySelector('.db-confirm').textContent, /2 values will be emptied/);
  assert.equal(sent.length, 0, 'nothing is written before confirmation');
  button(el, 'Change type').click();
  await tick();
  assert.equal(sent.length, 0, 'changed inputs need another confirmation');
  assert.match(el.querySelector('.db-confirm').textContent, /3 values will be emptied/);
  assert.match(el.querySelector('.db-status').textContent, /changed/);
  button(el, 'Change type').click();
  await tick();
  assert.deepEqual(sent, [[{ op: 'set_property', property: { id: P.score, inc: '7' }, definition: { name: 'Score', type: 'checkbox' }, confirmed_preview: 'p2' }]]);
  assert.deepEqual(asked[0], { property: { id: P.score, inc: '7' }, definition: { name: 'Score', type: 'checkbox' } });
});

test('cancelling a type change writes nothing and restores the type', async () => {
  const { el, sent } = setup({ preview: async () => ({ preview: 'p', emptied: 0, options_created: 0 }) });
  el.querySelector(`th[data-prop=${P.score}] .th-name`).click();
  const type = labelled(el.querySelector('.db-pop'), 'select', 'Type');
  type.value = 'text';
  type.dispatch('change');
  await tick();
  assert.match(el.querySelector('.db-confirm').textContent, /No values will be emptied/);
  button(el, 'Cancel').click();
  await tick();
  assert.equal(sent.length, 0);
  assert.equal(type.value, 'number');
});

test('presence names who is editing a cell, not only a color', () => {
  const { el } = setup({ presence: [{ label: 'Joon', edit: { database: { id: 'x', inc: '1' }, row: { id: row(1), inc: '7' }, property: { id: P.score, inc: '7' } } }] });
  assert.match(cellOf(el, row(1), P.score).textContent, /Joon editing/);
});

test('a loading database says so', () => {
  const { el } = setup({ loading: true });
  assert.match(el.querySelector('.db-loading').textContent, /Loading rows/);
});

test('read-only views render no editing controls', () => {
  const { el } = setup({ readOnly: true });
  assert.equal(button(el, 'Add filter'), undefined);
  assert.equal(button(el, 'New row'), undefined);
  assert.equal(el.querySelector('.col-resize'), null);
  assert.equal(el.querySelector('.row-handle'), null);
});

test('user text is rendered as text, never markup', () => {
  const { el, ctx, data, db } = setup();
  data.rows[0].cells[P.title] = { type: 'title', value: '<img src=x onerror=alert(1)>' };
  db.update(ctx);
  const cell = cellOf(el, row(0), P.title);
  assert.equal(cell.textContent, '<img src=x onerror=alert(1)>');
  assert.equal(cell.querySelector('img'), null);
});
