// Dialogs and menus act on what they were opened for (rule 21, TECH
// 19.5): the tab and the row and column ids of that moment. Inserts and
// sorts elsewhere move those rows and columns, and the dialog follows
// them. When its tab or all of its rows or columns are gone, it changes
// nothing and keeps what was typed in it.
'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { fire } from './dom.mjs';
import { FakeServer, find, notices, settle, startApp, type } from './server.mjs';

const sheet = () => new FakeServer({ tabs: [
  { id: 1, name: 'Budget', rows: 30, cols: 8, cells: { '0,0': 'Item', '1,1': '1200', '2,1': '900', '3,1': '300' } },
  { id: 2, name: 'Notes', rows: 30, cols: 8 },
] });
const actions = (t) => t.server.socket.frames('action');
const popup = (t) => t.doc.querySelector('.menu') || t.doc.querySelector('.pop:not([hidden])');
const alertOf = (d) => d.querySelector('[role="alert"]').textContent;
const field = (d, id) => d.querySelectorAll('select, input').find((el) => el.id === id);
// item finds a menu item by the start of its text, which ends with its key.
const item = (menu, text) => menu.querySelectorAll('button').find((b) => b.textContent.startsWith(text));
const OPTIONS = 'Format options: decimals, separator, currency symbol, date and time forms';

// insertCols adds a column before position `at`, as another writer does.
function insertCols(server, tabId, at) {
  const t = server.tab(tabId);
  const id = server.nextId++;
  const after = at > 0 ? t.cols[at - 1] : null;
  t.cols.splice(at, 0, id);
  return server.push({ tab: tabId, struct: [{ op: 'insert_cols', ids: [[id, 1]], after }], tabs: server.tabInfo() });
}

// deleteCol deletes one column by id, as another writer does.
function deleteCol(server, tabId, id) {
  const t = server.tab(tabId);
  t.cols.splice(t.cols.indexOf(id), 1);
  return server.push({ tab: tabId, struct: [{ op: 'delete_cols', ids: [[id, 1]] }], tabs: server.tabInfo() });
}

// The three dialogs, each opened on tab 1 with something typed or chosen.
const dialogs = {
  sort: {
    open(t) {
      t.sel.range(1, 0, 3, 1);
      t.app.changed();
      find(t.app.toolbar.el, 'Sort…').click();
      const d = popup(t);
      field(d, 'sort-col').value = '1';
      field(d, 'sort-dir').value = 'desc';
      return { d, rows: [t.model.rowAt(1), t.model.rowAt(2), t.model.rowAt(3)], col: t.model.colAt(1) };
    },
    submit: (d) => find(d, 'Sort').click(),
    kept: (d) => [field(d, 'sort-col').value, field(d, 'sort-dir').value],
    want: ['1', 'desc'],
  },
  width: {
    open(t) {
      t.sel.set(0, 3);
      t.app.rowsMenu({ x: 0, y: 0 });
      find(popup(t), 'Column width…').click();
      const d = popup(t);
      d.querySelector('input').value = '237';
      return { d, rows: [], col: t.model.colAt(3) };
    },
    submit: (d) => find(d, 'Set width').click(),
    kept: (d) => [d.querySelector('input').value],
    want: ['237'],
  },
  format: {
    open(t) {
      t.sel.set(1, 1);
      t.app.changed();
      find(t.app.toolbar.el, OPTIONS).click();
      const d = popup(t);
      field(d, 'nf-kind').value = 'currency';
      fire(field(d, 'nf-kind'), 'change');
      field(d, 'nf-sym').value = 'CHF';
      field(d, 'nf-dec').value = '3';
      return { d, rows: [t.model.rowAt(1)], col: t.model.colAt(1) };
    },
    submit: (d) => find(d, 'Apply').click(),
    kept: (d) => [field(d, 'nf-kind').value, field(d, 'nf-sym').value, field(d, 'nf-dec').value],
    want: ['currency', 'CHF', '3'],
  },
};

for (const [name, dlg] of Object.entries(dialogs)) {
  test(`the ${name} dialog changes nothing in the tab that replaces its deleted tab, and keeps its input`, async () => {
    const t = await startApp({ server: sheet() });
    const { d } = dlg.open(t);
    const sent = actions(t).length;
    t.server.deleteTab(1);
    assert.equal(t.model.tab.id, 2, 'the page now shows the other tab');
    dlg.submit(d);
    assert.equal(actions(t).length, sent, 'nothing is sent to the other tab');
    assert.equal(popup(t), d, 'the dialog stays open');
    assert.deepEqual(dlg.kept(d), dlg.want, 'with what was typed and chosen');
    assert.match(notices(t).at(-1)[1], /was opened for/, 'the refusal is told');
  });

  test(`the ${name} dialog acts on the rows and columns it was opened for after inserts move them`, async () => {
    const t = await startApp({ server: sheet() });
    const { d, rows, col } = dlg.open(t);
    t.server.insertRows(1, 0, 2);
    insertCols(t.server, 1, 0);
    dlg.submit(d);
    assert.equal(popup(t), null, 'a dialog that was sent closes');
    const op = actions(t).at(-1).ops[0];
    assert.equal(op.tab, 1);
    if (name === 'sort') {
      assert.deepEqual([op.rows, op.by, op.desc], [[[rows[0], 3]], col, true]);
    } else if (name === 'width') {
      assert.deepEqual([op.cols, op.width], [[[col, 1]], 237]);
    } else {
      assert.deepEqual([op.rows, op.cols, op.set], [[[rows[0], 1]], [[col, 1]], { number: { kind: 'currency', symbol: 'CHF', decimals: 3 } }]);
    }
  });
}

test('a dialog whose rows or columns were all deleted changes nothing and keeps its input', async () => {
  const t = await startApp({ server: sheet() });
  const { d } = dialogs.width.open(t);
  deleteCol(t.server, 1, t.model.colAt(3));
  const sent = actions(t).length;
  dialogs.width.submit(d);
  assert.equal(actions(t).length, sent);
  assert.equal(popup(t), d);
  assert.deepEqual(dialogs.width.kept(d), ['237']);
  assert.match(alertOf(d), /The width was not set/);
  // The sort's key column is part of what it was opened for.
  const s = dialogs.sort.open(t);
  deleteCol(t.server, 1, s.col);
  dialogs.sort.submit(s.d);
  assert.equal(actions(t).length, sent);
  assert.equal(popup(t), s.d);
  assert.deepEqual(dialogs.sort.kept(s.d), ['1', 'desc']);
  assert.match(alertOf(s.d), /The rows were not sorted/);
});

test('a host-refused number format offers its settings back', async () => {
  const t = await startApp({ server: sheet() });
  const { d } = dialogs.format.open(t);
  t.server.manual.add('action');
  dialogs.format.submit(d);
  assert.equal(popup(t), null, 'a format that was sent closes the dialog');
  const a = actions(t).at(-1);
  t.server.socket.recv({ type: 'ack', n: a.n, seq: '10', results: [{ status: 'refused', reason: 'disk_quota' }] });
  const refusal = notices(t).find(([, text]) => /The host refused this format/.test(text));
  assert.ok(refusal, 'the refusal is told');
  assert.equal(refusal[2], 'Number format: Currency, symbol CHF, 3 decimals', 'with the settings that were chosen');
});

test('a menu changes nothing when the tab it was opened for is gone or its rows moved', async () => {
  const t = await startApp({ server: sheet() });
  t.sel.range(1, 0, 2, 0);
  t.app.rowsMenu({ x: 0, y: 0 });
  const menu = popup(t);
  const sent = actions(t).length;
  t.server.deleteTab(1);
  find(menu, 'Delete 2 rows').click();
  assert.equal(actions(t).length, sent, 'no row of the other tab is deleted');
  assert.match(notices(t).at(-1)[1], /was opened for/);
  // A row inserted inside the selection changes what the menu would act on.
  const t2 = await startApp({ server: sheet() });
  t2.sel.range(1, 0, 2, 0);
  t2.app.rowsMenu({ x: 0, y: 0 });
  const menu2 = popup(t2);
  t2.server.insertRows(1, 2);
  find(menu2, 'Delete 2 rows').click();
  assert.equal(actions(t2).length, 0, 'the inserted row is not deleted');
  // Rows inserted above leave the selected rows as they were: the menu acts on them.
  const t3 = await startApp({ server: sheet() });
  t3.sel.range(1, 0, 2, 0);
  const ids = [t3.model.rowAt(1), t3.model.rowAt(2)];
  t3.app.rowsMenu({ x: 0, y: 0 });
  const menu3 = popup(t3);
  t3.server.insertRows(1, 0);
  find(menu3, 'Delete 2 rows').click();
  assert.deepEqual(actions(t3).at(-1).ops[0], { op: 'delete_rows', tab: 1, ids: [[ids[0], 2]] });
});

test('the palette applies its color to the cells it was opened for', async () => {
  const t = await startApp({ server: sheet() });
  t.sel.set(1, 1);
  t.app.changed();
  const row = t.model.rowAt(1);
  const col = t.model.colAt(1);
  find(t.app.toolbar.el, 'Fill color').click();
  const d = popup(t);
  t.server.insertRows(1, 0);
  find(d, 'blue 300').click();
  assert.deepEqual(actions(t).at(-1).ops[0], { op: 'format', tab: 1, rows: [[row, 1]], cols: [[col, 1]], set: { fill: 'blue-300' } });
  find(t.app.toolbar.el, 'Fill color').click();
  const d2 = popup(t);
  const sent = actions(t).length;
  t.server.deleteTab(1);
  find(d2, 'blue 300').click();
  assert.equal(actions(t).length, sent);
});

test('the format dialog leaves a row inserted inside its range unformatted', async () => {
  const t = await startApp({ server: sheet() });
  t.sel.range(1, 1, 2, 1);
  t.app.changed();
  const rows = [t.model.rowAt(1), t.model.rowAt(2)];
  find(t.app.toolbar.el, OPTIONS).click();
  const d = popup(t);
  field(d, 'nf-kind').value = 'percent';
  fire(field(d, 'nf-kind'), 'change');
  t.server.insertRows(1, 2);
  find(d, 'Apply').click();
  const op = actions(t).at(-1).ops[0];
  assert.deepEqual(op.rows, [[rows[0], 2]], 'the two rows it was opened for, not the one between them');
  assert.deepEqual(op.set, { number: { kind: 'percent', decimals: 2 } });
});

test('a right click that commits an edit opens the menu for the cell clicked, after the edit releases a held insert', async () => {
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 30, cols: 6, cells: { '5,1': '6' } }] });
  const t = await startApp({ server });
  server.manual.add('rebase');
  const clicked = [t.model.rowAt(1), t.model.colAt(1)];
  t.sel.set(2, 3);
  t.app.changed();
  type(t, '=B6+1');
  server.insertRows(1, 0);
  assert.equal(t.grid.input.readOnly, false);
  t.grid.h.context({ zone: 'cell', r: 1, c: 1 }, { preventDefault() {}, clientX: 0, clientY: 0, button: 2 });
  assert.equal(t.editor.ed, null, 'the edit was committed');
  const a = t.sel.active();
  assert.deepEqual([t.model.rowAt(a.r), t.model.colAt(a.c)], clicked, 'the cell clicked is selected where it moved');
  assert.match(popup(t).textContent, /Rows 3/, 'and the menu is for its row');
});

test('the freeze menu and a paste of values change nothing once the tab they were opened for is gone', async () => {
  const t = await startApp({ server: sheet() });
  t.sel.set(2, 1);
  t.app.changed();
  find(t.app.toolbar.el, 'Freeze rows and columns').click();
  const menu = popup(t);
  t.server.deleteTab(1);
  find(menu, 'Freeze rows down to row 3').click();
  assert.equal(actions(t).length, 0, 'the other tab is not frozen');
  assert.match(notices(t).at(-1)[1], /was opened for/);
  // The clipboard is read after the menu closes; its text goes nowhere else.
  const t2 = await startApp({ server: sheet() });
  globalThis.__clipboard = 'pasted';
  t2.sel.set(1, 1);
  t2.app.rowsMenu({ x: 0, y: 0 });
  item(popup(t2), 'Paste values only').click();
  t2.server.deleteTab(1);
  await settle();
  assert.equal(actions(t2).length, 0);
  assert.match(notices(t2).at(-1)[1], /was opened for/);
  const t3 = await startApp({ server: sheet() });
  globalThis.__clipboard = 'pasted';
  t3.sel.set(1, 1);
  t3.app.rowsMenu({ x: 0, y: 0 });
  item(popup(t3), 'Paste values only').click();
  await settle();
  assert.deepEqual([actions(t3).at(-1).ops[0].tab, actions(t3).at(-1).ops[0].values], [1, [['pasted']]], 'with nothing changed it pastes');
});

test('a paste of values whose text arrives when the page cannot edit offers the text back', async () => {
  const t = await startApp({ server: sheet() });
  globalThis.__clipboard = 'pasted';
  t.sel.set(1, 1);
  t.app.rowsMenu({ x: 0, y: 0 });
  item(popup(t), 'Paste values only').click();
  t.server.socket.recv({ type: 'status', calculating: true });
  await settle();
  assert.equal(actions(t).length, 0);
  assert.deepEqual(notices(t).at(-1).slice(1), ['The sheet is calculating. Editing is available again when it ends. Nothing was written. The text:', 'pasted']);
  // A later refusal of the same kind leaves the offered text on screen.
  type(t, 'x');
  assert.equal(notices(t).filter((n) => n[2] === 'pasted').length, 1);
});

test('the phone format menu changes nothing once the tab it was opened for is gone', async () => {
  globalThis.__phone = true;
  try {
    const t = await startApp({ server: sheet(), width: 390, height: 500 });
    find(t.app.toolbar.el, 'Format').click();
    const menu = popup(t);
    t.server.deleteTab(1);
    find(menu, 'Bold').click();
    assert.equal(actions(t).length, 0);
    assert.match(notices(t).at(-1)[1], /was opened for/);
  } finally {
    globalThis.__phone = false;
  }
});
