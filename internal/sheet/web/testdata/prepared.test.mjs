// Actions prepared before they are sent (TECH 7.1 and 19.5): a fill drag,
// a column resize, a dialog and a menu name their tab, rows and columns
// by id and keep the sequence of the screen they were prepared on as the
// base of what they send. Another writer's changes in between move them;
// a deletion ends them visibly, and one that was deleted and came back
// since is never changed. The host ignores ids born after the base.
'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { packIds } from '../protocol.js';
import { fire } from './dom.mjs';
import { FakeServer, find, notices, startApp, type } from './server.mjs';

const ev = (props = {}) => ({ preventDefault() {}, clientX: 0, clientY: 0, button: 0, ...props });
const sheet = () => new FakeServer({ tabs: [
  { id: 1, name: 'Data', rows: 30, cols: 8, cells: { '0,0': '10', '1,0': '99', '2,0': '30', '3,0': '40', '4,0': '50', '1,1': '1200' } },
  { id: 2, name: 'Notes', rows: 30, cols: 8 },
] });
const actions = (t) => t.server.socket.frames('action');
const popup = (t) => t.doc.querySelector('.menu') || t.doc.querySelector('.pop:not([hidden])');
const field = (d, id) => d.querySelectorAll('select, input').find((el) => el.id === id);

// The other writer's changes, by ids. restore puts deleted ids back where
// they were, as an undo of the deletion does.
function deleteAxis(server, tabId, rows, id) {
  const t = server.tab(tabId);
  const axis = rows ? t.rows : t.cols;
  const at = axis.indexOf(id);
  axis.splice(at, 1);
  server.push({ tab: tabId, struct: [{ op: rows ? 'delete_rows' : 'delete_cols', ids: [[id, 1]] }], tabs: server.tabInfo() });
  return at;
}
function restoreAxis(server, tabId, rows, id, at) {
  const t = server.tab(tabId);
  const axis = rows ? t.rows : t.cols;
  const after = at > 0 ? axis[at - 1] : null;
  axis.splice(at, 0, id);
  return server.push({ tab: tabId, struct: [{ op: rows ? 'insert_rows' : 'insert_cols', ids: [[id, 1]], after }], tabs: server.tabInfo() });
}
function insertCol(server, tabId, at) {
  const t = server.tab(tabId);
  const id = server.nextId++;
  const after = at > 0 ? t.cols[at - 1] : null;
  t.cols.splice(at, 0, id);
  return server.push({ tab: tabId, struct: [{ op: 'insert_cols', ids: [[id, 1]], after }], tabs: server.tabInfo() });
}

// fillDrag selects A1 and drags its fill handle to A5, keeping the button down.
async function fillDrag() {
  const t = await startApp({ server: sheet() });
  t.sel.set(0, 0);
  t.app.changed();
  const source = t.model.rowAt(0);
  const targets = [1, 2, 3, 4].map((r) => t.model.rowAt(r));
  const col = t.model.colAt(0);
  t.grid.h.down({ zone: 'handle', r: 0, c: 0 }, ev());
  t.grid.h.drag({ zone: 'cell', r: 4, c: 0 }, ev());
  return { ...t, source, targets, col };
}

test('a fill whose source is deleted before the button is let go sends nothing and says so', async () => {
  for (const how of ['deleted', 'deleted and restored', 'deleted by a delta the settle applies']) {
    const t = await fillDrag();
    if (how === 'deleted by a delta the settle applies') {
      // A key typed during the drag opens an edit of A1 that holds the deletion.
      t.server.manual.add('rebase');
      type(t, '=1');
      deleteAxis(t.server, 1, true, t.source);
      assert.equal(t.model.rowAt(0), t.source, 'the deletion is held while the edit is open');
    } else {
      const at = deleteAxis(t.server, 1, true, t.source);
      if (how === 'deleted and restored') restoreAxis(t.server, 1, true, t.source, at);
    }
    t.grid.h.up(ev());
    assert.deepEqual(actions(t).filter((a) => a.ops[0].op === 'fill'), [], `${how}: no other cell is used as the source`);
    assert.match(notices(t).at(-1)[1], /fill ended because the cells it started from were deleted/, how);
    assert.equal(t.grid.overlays.fill, null, how);
  }
});

test('a fill keeps the source and the targets it was prepared with, and the base of the screen they were chosen on', async () => {
  const t = await fillDrag();
  const base = t.model.seq;
  // Rows inserted above and inside the target move them; neither is filled.
  t.server.insertRows(1, 0);
  t.server.insertRows(1, 3);
  t.grid.h.up(ev());
  const a = actions(t).at(-1);
  assert.deepEqual(a.ops[0], { op: 'fill', tab: 1, source: { rows: [[t.source, 1]], cols: [[t.col, 1]] }, target: { rows: packIds(t.targets), cols: [[t.col, 1]] }, mode: 'auto' });
  assert.equal(a.base, base);
  const b = t.sel.box();
  assert.deepEqual([b.r1, b.r2], [1, 6], 'the selection covers the source and the targets where they are');
});

test('a fill aimed at a cell that is gone after the settle sends nothing and says so', async () => {
  const t = await fillDrag();
  t.server.manual.add('rebase');
  type(t, '=1');
  deleteAxis(t.server, 1, true, t.targets[3]);
  t.grid.h.up(ev());
  assert.deepEqual(actions(t).map((a) => a.ops[0].op), ['set']);
  assert.match(notices(t).at(-1)[1], /this fill was aimed at were deleted/);
});

// resizeB starts dragging the right edge of column B towards 200 pixels.
async function resizeB() {
  const t = await startApp({ server: sheet() });
  const col = t.model.colAt(1);
  const base = t.model.seq;
  t.grid.h.down({ zone: 'resize', r: 0, c: 1 }, ev({ clientX: 300 }));
  t.grid.h.drag({ zone: 'col', r: 0, c: 1 }, ev({ clientX: 340 }));
  return { ...t, col, base };
}

test('a column resize follows its column when another column is inserted, and keeps its base', async () => {
  const t = await resizeB();
  const a = t.model.colAt(0);
  insertCol(t.server, 1, 0);
  t.grid.h.drag({ zone: 'col', r: 0, c: 2 }, ev({ clientX: 396 }));
  assert.equal(t.model.widthOf(t.col), 200, 'the preview is on the column being resized');
  assert.equal(t.model.widthOf(a), 104, 'and not on the one now in its place');
  t.grid.h.up(ev());
  const sent = actions(t).at(-1);
  assert.deepEqual(sent.ops, [{ op: 'width', tab: 1, cols: [[t.col, 1]], width: 200 }]);
  assert.equal(sent.base, t.base);
});

test('a column resize ends visibly when its column or its tab is gone', async () => {
  for (const how of ['column deleted', 'column deleted and restored', 'tab deleted and another shown']) {
    const t = await resizeB();
    if (how === 'tab deleted and another shown') {
      t.server.deleteTab(1);
      assert.equal(t.model.tab.id, 2, 'the replacement tab is on screen');
    } else {
      const at = deleteAxis(t.server, 1, false, t.col);
      if (how === 'column deleted and restored') restoreAxis(t.server, 1, false, t.col, at);
    }
    t.grid.h.drag({ zone: 'col', r: 0, c: 1 }, ev({ clientX: 396 }));
    t.grid.h.up(ev());
    assert.deepEqual(actions(t), [], `${how}: no width is sent`);
    assert.ok(notices(t).some(([, text]) => /resize ended because its column was deleted or its tab closed/.test(text)), how);
    for (const c of t.model.tab.cols) assert.equal(t.model.widthOf(c), 104, `${how}: no column keeps a preview`);
  }
});

// The dialogs of dialogs.test.mjs, opened on Data!B2.
const dialogs = {
  format: {
    open(t) {
      t.sel.set(1, 1);
      t.app.changed();
      find(t.app.toolbar.el, 'Format options: decimals, separator, currency symbol, date and time forms').click();
      const d = popup(t);
      field(d, 'nf-kind').value = 'currency';
      fire(field(d, 'nf-kind'), 'change');
      field(d, 'nf-sym').value = 'CHF';
      return d;
    },
    submit: (d) => find(d, 'Apply').click(),
    kept: 'Number format: Currency, symbol CHF, 2 decimals',
  },
  width: {
    open(t) {
      t.sel.set(1, 1);
      t.app.rowsMenu({ x: 0, y: 0 });
      find(popup(t), 'Column width…').click();
      const d = popup(t);
      d.querySelector('input').value = '237';
      return d;
    },
    submit: (d) => find(d, 'Set width').click(),
    kept: '237',
  },
  sort: {
    open(t) {
      t.sel.range(1, 1, 3, 1);
      t.app.changed();
      find(t.app.toolbar.el, 'Sort…').click();
      const d = popup(t);
      field(d, 'sort-dir').value = 'desc';
      return d;
    },
    submit: (d) => find(d, 'Sort').click(),
    kept: 'Sort by column B, descending',
  },
};

for (const [name, dlg] of Object.entries(dialogs)) {
  test(`the ${name} dialog is sent with the base it was opened on, and a host that ignores it gets its input offered back`, async () => {
    const t = await startApp({ server: sheet() });
    const base = t.model.seq;
    const d = dlg.open(t);
    const row = t.model.rowAt(1);
    const col = t.model.colAt(1);
    // Another writer deletes row 2 or column B and undoes that.
    const rows = name !== 'width';
    const id = rows ? row : col;
    restoreAxis(t.server, 1, rows, id, deleteAxis(t.server, 1, rows, id));
    assert.notEqual(t.model.seq, base);
    t.server.manual.add('action');
    dlg.submit(d);
    const a = actions(t).at(-1);
    assert.equal(a.base, base, 'the host compares the ids with the screen the dialog was opened on');
    t.server.socket.recv({ type: 'ack', n: a.n, seq: t.server.seq.toString(), results: [{ status: 'ignored', reason: 'gone' }] });
    const told = notices(t).find(([, text]) => /was ignored/.test(text));
    assert.ok(told, 'the person is told');
    assert.equal(told[2], dlg.kept, 'with what was entered');
  });
}

test('a menu item and a tab bar action are sent with the base of the screen they were opened on', async () => {
  const t = await startApp({ server: sheet() });
  const base = t.model.seq;
  t.sel.range(1, 0, 1, 0);
  t.app.rowsMenu({ x: 0, y: 0 });
  const menu = popup(t);
  t.server.setCell(1, 9, 3, 'elsewhere');
  menu.querySelectorAll('button').find((b) => b.textContent.startsWith('Delete 1 row')).click();
  assert.equal(actions(t).at(-1).base, base);
  // A tab rename field started before a change.
  const before = t.model.seq;
  t.app.tabs.rename(2);
  t.server.setCell(1, 9, 4, 'elsewhere');
  const input = t.app.tabs.list.querySelector('input');
  input.value = 'Ideas';
  fire(input, 'keydown', { key: 'Enter' });
  const rename = actions(t).at(-1);
  assert.deepEqual([rename.ops[0].op, rename.base], ['rename_tab', before]);
});
