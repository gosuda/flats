// Moving the selection while an edit is open (rule 21, TECH 19.5): the
// text area is drawn at the active cell and the formula bar names it, so
// every path that moves the active cell settles the edit first. The edit
// commits, or, when it cannot be committed now, stays on its cell with its
// text and the selection does not move.
'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { address, packIds } from '../protocol.js';
import { fire } from './dom.mjs';
import { FakeServer, key, startApp, type } from './server.mjs';

const ev = (props = {}) => ({ preventDefault() {}, clientX: 0, clientY: 0, button: 0, ...props });

// B2 holds the text Find looks for, A1 a formula, B6 a number.
async function sheet() {
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 30, cols: 6, cells: { '0,0': '=B6', '1,1': 'needle', '5,1': '6' } }] });
  const t = await startApp({ server });
  const actions = () => server.socket.frames('action');
  const written = () => actions().map((a) => [a.ops[0].row, a.ops[0].col, a.ops[0].text, a.ops[0].at]);
  const ids = (r, c) => [t.model.rowAt(r), t.model.colAt(c)];
  const find = (text) => {
    fire(t.doc.activeElement, 'keydown', { key: 'f', ctrlKey: true });
    t.app.find.input.value = text;
    fire(t.app.find.input, 'input');
  };
  const escape = () => fire(t.app.find.input, 'keydown', { key: 'Escape' });
  const menu = (r, c) => t.grid.h.context({ zone: 'cell', r, c }, ev({ button: 2 }));
  const at = () => [t.sel.active().r, t.sel.active().c];
  return { ...t, actions, written, ids, find, escape, menu, at };
}

// stays checks that the edit is still on (r, c), where the active cell,
// the address the formula bar shows and the cell the editor writes agree.
function stays(t, r, c, text, what) {
  assert.ok(t.editor.ed, `${what}: the edit is still open`);
  assert.deepEqual(t.at(), [r, c], `${what}: the selection stays on the edited cell`);
  assert.deepEqual([t.editor.ed.row, t.editor.ed.col], t.ids(r, c), `${what}: the editor writes the cell it is drawn on`);
  assert.equal(t.app.nameBox.textContent, address(r, c), `${what}: the formula bar names that cell`);
  assert.equal(t.editor.text, text, `${what}: the typed text is kept`);
}

test('Find commits an ordinary edit before it moves to a match, and the next edit writes the match', async () => {
  const t = await sheet();
  t.sel.set(0, 2);
  t.app.changed();
  type(t, 'draft');
  assert.equal(t.editor.open, true);
  t.find('needle');
  assert.equal(t.editor.ed, null, 'the edit was committed before the selection moved');
  assert.deepEqual(t.written(), [[...t.ids(0, 2), 'draft', [0, 2]]], 'the text went to the cell it was typed in');
  assert.deepEqual(t.at(), [1, 1]);
  assert.equal(t.app.nameBox.textContent, 'B2');
  assert.equal(t.doc.activeElement, t.app.find.input, 'focus stays in the find field');
  t.escape();
  type(t, 'more');
  key(t, 'Enter');
  assert.deepEqual(t.written()[1], [...t.ids(1, 1), 'more', [1, 1]], 'the next edit writes the match that is shown');
});

test('the context menu commits an ordinary edit before it selects another cell, and keeps it on its own cell', async () => {
  const t = await sheet();
  t.sel.set(0, 2);
  t.app.changed();
  type(t, 'draft');
  // A right click on the edited cell moves nothing: the edit stays.
  t.menu(0, 2);
  stays(t, 0, 2, 'draft', 'a menu on the edited cell');
  assert.equal(t.app.popups.open, true);
  t.app.popups.close();
  t.menu(1, 1);
  assert.equal(t.editor.ed, null);
  assert.deepEqual(t.written(), [[...t.ids(0, 2), 'draft', [0, 2]]]);
  assert.deepEqual(t.at(), [1, 1]);
  assert.equal(t.app.popups.open, true, 'the menu opens for the cell clicked');
  t.app.popups.close();
  type(t, 'x');
  key(t, 'Enter');
  assert.deepEqual(t.written()[1], [...t.ids(1, 1), 'x', [1, 1]]);
});

test('Find, the context menu and the keyboard leave an edit that waits for its formula on its own cell', async () => {
  const t = await sheet();
  t.server.manual.add('peek');
  key(t, 'F2');
  const peek = t.server.socket.frames('peek')[0];
  type(t, '+1');
  t.find('needle');
  stays(t, 0, 0, '+1', 'Find');
  assert.equal(t.app.find.count.textContent, '1 match', 'the count does not name a match the selection is not on');
  t.escape();
  t.menu(1, 1);
  stays(t, 0, 0, '+1', 'the context menu');
  assert.equal(t.app.popups.open, false, 'no menu opens for a cell the edit cannot leave');
  assert.equal(t.doc.activeElement, t.editor.area, 'focus is back in the edit');
  for (const k of ['ArrowDown', 'PageDown', 'Tab', 'Enter']) {
    key(t, k);
    stays(t, 0, 0, '+1', k);
  }
  key(t, 'Home', { ctrlKey: true });
  stays(t, 0, 0, '+1', 'Ctrl+Home');
  t.server.socket.recv({ type: 'peeked', id: peek.id, seq: '10', edit: '=B6', formula: true });
  stays(t, 0, 0, '=B6+1', 'the answer');
  key(t, 'Enter');
  assert.deepEqual(t.written(), [[...t.ids(0, 0), '=B6+1', [0, 0]]], 'the edit writes the cell it stayed on');
});

test('Find and the context menu leave a read-only edit on its own cell, and it commits there once editable', async () => {
  const t = await sheet();
  t.server.manual.add('rebase');
  const shift = (text) => text.replace(/([A-Z]+)(\d+)/g, (_, c, r) => c + (Number(r) + 1));
  const answer = (i) => {
    const m = t.server.rebases[i];
    t.server.socket.recv({ type: 'rebased', id: m.id, text: shift(m.text), at: [m.at[0] + 1, m.at[1]], base: m.through });
  };
  t.sel.set(2, 3);
  t.app.changed();
  type(t, '=B6+');
  const edited = t.ids(2, 3);
  t.server.insertRows(1, 0);
  type(t, '1');
  t.timers.advance(500);
  answer(0);
  assert.equal(t.grid.input.readOnly, true);
  t.find('needle');
  stays(t, 2, 3, '=B6+1', 'Find');
  t.escape();
  t.menu(5, 1);
  stays(t, 2, 3, '=B6+1', 'the context menu');
  assert.equal(t.app.popups.open, false);
  for (const k of ['ArrowDown', 'Tab', 'Enter']) {
    key(t, k);
    stays(t, 2, 3, '=B6+1', k);
  }
  assert.equal(t.actions().length, 0, 'nothing is written while the edit is read-only');
  answer(1);
  assert.equal(t.grid.input.readOnly, false);
  stays(t, 3, 3, '=B7+1', 'the answer');
  key(t, 'Enter');
  assert.deepEqual(t.written(), [[...edited, '=B7+1', [3, 3]]], 'the edit writes the cell it was typed in');
});

test('a drag that an edit started under settles that edit before it moves the selection', async () => {
  const t = await sheet();
  // A key typed while a fill handle is held opens an edit of the active cell.
  t.sel.set(0, 2);
  t.app.changed();
  t.grid.h.down({ zone: 'handle', r: 0, c: 2 }, ev());
  type(t, 'held');
  t.grid.h.drag({ zone: 'cell', r: 3, c: 2 }, ev());
  assert.equal(t.editor.ed, null, 'the edit committed when the drag went on');
  assert.deepEqual(t.written(), [[...t.ids(0, 2), 'held', [0, 2]]]);
  t.grid.h.up(ev());
  // A formula that waits for its text keeps the drag from moving the selection.
  t.server.manual.add('peek');
  t.sel.set(0, 0);
  t.app.changed();
  t.grid.h.down({ zone: 'cell', r: 0, c: 0 }, ev());
  key(t, 'F2');
  type(t, '+2');
  t.grid.h.drag({ zone: 'cell', r: 4, c: 4 }, ev());
  t.grid.h.up(ev());
  stays(t, 0, 0, '+2', 'a drag');
  assert.equal(t.sel.single, true, 'the range did not grow under the edit');
  // A fill whose edit opened after the last move fills nothing past it.
  const t2 = await sheet();
  t2.server.manual.add('peek');
  t2.grid.h.down({ zone: 'handle', r: 0, c: 0 }, ev());
  t2.grid.h.drag({ zone: 'cell', r: 3, c: 0 }, ev());
  key(t2, 'F2');
  t2.grid.h.up(ev());
  assert.ok(t2.editor.ed?.waiting);
  assert.deepEqual(t2.at(), [0, 0]);
  assert.equal(t2.actions().length, 0, 'the fill is not sent while the edit stays');
  assert.equal(t2.sel.single, true);
});

test('a fill the page cannot send leaves the selection as it was', async () => {
  const t = await sheet();
  t.grid.h.down({ zone: 'handle', r: 0, c: 0 }, ev());
  t.grid.h.drag({ zone: 'cell', r: 3, c: 0 }, ev());
  t.server.socket.recv({ type: 'status', calculating: true });
  t.grid.h.up(ev());
  assert.equal(t.actions().length, 0);
  assert.equal(t.sel.single, true, 'the selection does not claim cells that were not filled');
});

test('a reference drag ends when its edit closes under it', async () => {
  const t = await sheet();
  t.sel.set(0, 3);
  t.app.changed();
  type(t, '=');
  t.grid.h.down({ zone: 'cell', r: 1, c: 1 }, ev());
  assert.equal(t.editor.text, '=B2');
  t.server.socket.recv({ type: 'status', calculating: true });
  assert.equal(t.editor.ed, null);
  t.grid.h.drag({ zone: 'cell', r: 3, c: 1 }, ev());
  t.grid.h.up(ev());
  assert.equal(t.editor.ed, null);
  assert.equal(t.actions().length, 0);
});

// held opens a formula edit in D3 and has another writer insert a row
// above and a column before everything. The editor holds both back until
// it settles (TECH 8.4).
async function held() {
  const t = await sheet();
  t.server.manual.add('rebase');
  t.sel.set(2, 3);
  t.app.changed();
  type(t, '=1');
  t.server.insertRows(1, 0);
  const tab = t.server.tab(1);
  const col = t.server.nextId++;
  tab.cols.unshift(col);
  t.server.push({ tab: 1, struct: [{ op: 'insert_cols', ids: [[col, 1]], after: null }], tabs: t.server.tabInfo() });
  assert.deepEqual([t.model.rowCount, t.model.colCount], [30, 6], 'the editor holds the inserts');
  return t;
}

test('a click that settles an edit selects the cell, row or column it was on, wherever the released changes moved it', async () => {
  for (const [what, hit, want] of [
    ['a cell', { zone: 'cell', r: 4, c: 1 }, (t, b) => assert.deepEqual([t.at(), t.sel.single], [[5, 2], true])],
    ['a row header', { zone: 'row', r: 4, c: 0 }, (t, b) => assert.deepEqual([b.r1, b.r2, t.sel.kind], [5, 5, 'rows'])],
    ['a column header', { zone: 'col', r: 0, c: 1 }, (t, b) => assert.deepEqual([b.c1, b.c2, t.sel.kind], [2, 2, 'cols'])],
  ]) {
    const t = await held();
    const ids = t.ids(4, 1);
    t.grid.h.down(hit, ev());
    t.grid.h.up(ev());
    assert.equal(t.editor.ed, null, `${what}: the edit committed`);
    assert.deepEqual([t.model.rowCount, t.model.colCount], [31, 7]);
    assert.deepEqual(t.written(), [[...t.ids(3, 4), '=1', [2, 3]]], `${what}: the edit wrote its own cell`);
    want(t, t.sel.box());
    if (what === 'a cell') assert.deepEqual(t.ids(...t.at()), ids, 'the cell the person clicked');
  }
});

test('a click on a cell that the released changes deleted selects nothing new', async () => {
  const t = await sheet();
  t.server.manual.add('rebase');
  t.sel.set(2, 3);
  t.app.changed();
  type(t, '=1');
  t.server.deleteRows(1, 4);
  t.grid.h.down({ zone: 'cell', r: 4, c: 1 }, ev());
  t.grid.h.up(ev());
  assert.equal(t.editor.ed, null);
  assert.deepEqual(t.at(), [2, 3], 'the selection stays on the cell that was edited');
  assert.equal(t.sel.single, true);
});

test('a drag that settles an edit extends to the cell the pointer is on after the released changes', async () => {
  const t = await sheet();
  t.server.manual.add('rebase');
  t.sel.set(0, 2);
  t.app.changed();
  t.grid.h.down({ zone: 'cell', r: 0, c: 2 }, ev());
  type(t, '=1');
  t.server.insertRows(1, 0);
  const ids = t.ids(3, 4);
  t.grid.h.drag({ zone: 'cell', r: 3, c: 4 }, ev());
  t.grid.h.up(ev());
  assert.equal(t.editor.ed, null);
  assert.equal(t.model.rowCount, 31);
  const b = t.sel.box();
  assert.deepEqual([b.r1, b.c1, b.r2, b.c2], [1, 2, 4, 4]);
  assert.deepEqual([t.model.rowAt(b.r2), t.model.colAt(b.c2)], ids, 'the range ends on the cell the pointer was on');
});

test('a fill released after an edit opened is aimed at the cells the pointer was on after the edit settles', async () => {
  const t = await sheet();
  t.server.manual.add('rebase');
  t.sel.set(0, 2);
  t.app.changed();
  t.grid.h.down({ zone: 'handle', r: 0, c: 2 }, ev());
  t.grid.h.drag({ zone: 'cell', r: 3, c: 2 }, ev());
  const to = t.model.rowAt(3);
  type(t, '=1');
  t.server.insertRows(1, 0);
  t.grid.h.up(ev());
  assert.equal(t.editor.ed, null);
  assert.equal(t.model.rowCount, 31);
  const fill = t.actions().at(-1).ops[0];
  assert.equal(fill.op, 'fill');
  const b = t.sel.box();
  assert.deepEqual([b.r1, b.r2, b.c1, b.c2], [1, 4, 2, 2], 'the source and the target as they lie now');
  assert.equal(t.model.rowAt(b.r2), to, 'the fill ends on the row the pointer was on');
  const rows = (r1, r2) => packIds(t.model.tab.rows.slice(r1, r2 + 1));
  assert.deepEqual([fill.source.rows, fill.target.rows], [rows(1, 1), rows(2, 4)], 'neither the inserted row nor the source is a target');
});
