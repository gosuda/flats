// Formula text on demand (TECH 12.4): a peek answer names the sequence it
// was read at, and the server may send it before or after deltas of other
// sequences. The client installs text only from a state its screen is in.
'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { fire } from './dom.mjs';
import { FakeServer, key, notices, startApp, type } from './server.mjs';

const PEEK_DELAY = 60;

// A1 holds =B5. The test answers every peek itself. ask selects A1 and
// returns the peek the client sends for it.
async function formulaInA1() {
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 30, cols: 6, cells: { '0,0': '=B5', '4,1': '5', '8,3': 'far' } }, { id: 2, name: 'Other', rows: 5, cols: 3 }] });
  const t = await startApp({ server });
  server.manual.add('peek');
  const row = t.model.rowAt(0);
  const col = t.model.colAt(0);
  const peeks = () => server.sockets.flatMap((s) => s.frames('peek'));
  const answer = (peek, seq, edit) => server.socket.recv({ type: 'peeked', id: peek.id, seq, edit, formula: true });
  // insertAbove is what another writer's insert of a row above A1 sends:
  // the row, and A1 with the value of its adjusted formula.
  const insertAbove = () => server.insertRows(1, 0, 1, { cells: [[row, [[col, 'nf', 5]]]] });
  const cached = () => t.model.peeked(row, col)?.edit ?? null;
  return { ...t, row, col, peeks, answer, insertAbove, cached };
}

test('a peek answer read before a structural change is dropped and asked for again', async () => {
  const t = await formulaInA1();
  t.timers.advance(PEEK_DELAY);
  const first = t.peeks()[0];
  assert.deepEqual([first.row, first.col], [t.row, t.col]);
  // The server read the text at 10; the insert of sequence 11 is sent first.
  t.insertAbove();
  t.answer(first, '10', '=B5');
  assert.equal(t.cached(), null, 'text of the state before the insert is not installed');
  assert.equal(t.app.bar.value, '');
  assert.equal(t.peeks().length, 1);
  t.timers.advance(PEEK_DELAY);
  assert.equal(t.peeks().length, 2, 'the text is asked for again');
  t.answer(t.peeks()[1], '11', '=B6');
  assert.equal(t.cached(), '=B6');
  assert.equal(t.app.bar.value, '=B6');
  // F2 and +1 commit the reference the screen shows
  key(t, 'F2');
  type(t, '+1');
  key(t, 'Enter');
  const a = t.server.socket.frames('action')[0];
  assert.deepEqual([a.base, a.ops[0].text, a.ops[0].at], ['11', '=B6+1', [1, 0]]);
});

test('an editor that waits for its formula opens only with text of the state on screen', async () => {
  const t = await formulaInA1();
  t.server.manual.add('rebase');
  key(t, 'F2');
  const first = t.peeks()[0];
  t.grid.input.value = '+1';
  fire(t.grid.input, 'input');
  t.insertAbove();
  assert.equal(t.model.seq, '10', 'the keys hold the screen they were typed on');
  t.answer(first, '10', '=B5');
  assert.equal(t.editor.open, true, 'the text of that screen opens the editor');
  assert.equal(t.editor.text, '=B5+1');
  const r = t.server.rebases[0];
  assert.deepEqual([r.text, r.base, r.through, r.at], ['=B5+1', '10', '11', [0, 0]]);
  t.server.socket.recv({ type: 'rebased', id: r.id, text: '=B6+1', at: [1, 0], base: '11' });
  assert.equal(t.editor.text, '=B6+1', 'the text moves with the grid');
  assert.equal(t.model.seq, '11');
  key(t, 'Enter');
  const a = t.server.socket.frames('action')[0];
  assert.deepEqual([a.base, a.ops[0].text, a.ops[0].at], ['11', '=B6+1', [1, 0]]);
});

test('references typed while the formula text is on its way keep the cells they named when rows are inserted', async () => {
  for (const surface of ['cell', 'bar']) {
    const t = await formulaInA1();
    t.server.manual.add('rebase');
    if (surface === 'cell') { key(t, 'F2'); type(t, '+B5'); } else barEdit(t, '+B5');
    // A row above row 5 makes the cell the person typed as B5 into B6.
    t.server.insertRows(1, 4);
    assert.equal(t.model.rowCount, 30, `${surface}: the grid the keys were typed on stays`);
    assert.equal(t.model.seq, '10');
    t.answer(t.peeks()[0], '10', '=B5');
    assert.equal(t.editor.text, '=B5+B5', `${surface}: both references are of the screen at 10`);
    assert.deepEqual(t.server.rebases.map((r) => [r.text, r.base, r.through, r.at]), [['=B5+B5', '10', '11', [0, 0]]]);
    t.server.socket.recv({ type: 'rebased', id: t.server.rebases[0].id, text: '=B6+B6', at: [0, 0], base: '11' });
    assert.equal(t.model.rowCount, 31);
    assert.deepEqual([t.editor.text, t.app.bar.value, t.grid.input.value], ['=B6+B6', '=B6+B6', '=B6+B6'], `${surface}: text and grid move together`);
    key(t, 'Enter');
    const a = t.server.socket.frames('action');
    assert.equal(a.length, 1);
    assert.deepEqual([a[0].base, a[0].ops[0].text, a[0].ops[0].at], ['11', '=B6+B6', [0, 0]], `${surface}: the commit names the screen it was adjusted to`);
  }
});

test('keys typed while the formula text is on its way are offered back when only text of a later grid arrives', async () => {
  for (const order of ['delta first', 'answer first']) {
    const t = await formulaInA1();
    t.server.manual.add('rebase');
    key(t, 'F2');
    type(t, '+B5');
    // The server read the text after the insert, so =B6 names the cell
    // the person typed as B5, and +B5 a cell of the screen before it.
    if (order === 'delta first') t.server.insertRows(1, 4);
    t.answer(t.peeks()[0], '11', '=B6');
    if (order === 'answer first') t.server.insertRows(1, 4);
    assert.equal(t.editor.ed, null, `${order}: no text mixes the two grids`);
    assert.ok(notices(t).some(([, text, kept]) => /before the formula you were editing arrived/.test(text) && kept === '+B5'), `${order}: the keys are offered back`);
    assert.equal(t.model.rowCount, 31, `${order}: the held insert shows once the editor is closed`);
    assert.equal(t.server.rebases.length, 0);
    key(t, 'Enter');
    assert.equal(t.server.socket.frames('action').length, 0, `${order}: nothing is written`);
  }
});

test('an editor that waits for its formula with no keys typed opens on the text of a later grid', async () => {
  const t = await formulaInA1();
  t.server.manual.add('rebase');
  key(t, 'F2');
  t.server.insertRows(1, 4);
  t.server.setCell(1, 8, 3, 'later');
  assert.equal(t.model.rowCount, 30);
  t.answer(t.peeks()[0], '11', '=B6');
  assert.deepEqual([t.editor.open, t.editor.text, t.model.rowCount], [true, '=B6', 31], 'the held deltas apply up to the answer');
  assert.equal(t.model.seq, '12', 'a cell change held after it needs no exchange');
  assert.equal(t.model.cellAt(8, 3)?.shown, 'later');
  assert.equal(t.server.rebases.length, 0);
  // a structural change after that is held and exchanged from the screen the text opened on
  t.server.insertRows(1, 4);
  assert.deepEqual([t.server.rebases[0].text, t.server.rebases[0].base, t.server.rebases[0].through], ['=B6', '12', '13']);
});

test('a peek answer read at a later sequence than the screen waits for the deltas up to it', async () => {
  const t = await formulaInA1();
  key(t, 'F2');
  const first = t.peeks()[0];
  // The server already applied the insert when it read the text; its
  // answer overtakes the delta.
  t.answer(first, '11', '=B6');
  assert.equal(t.editor.open, false, 'the screen still shows the grid of sequence 10');
  assert.equal(t.cached(), null);
  t.insertAbove();
  assert.equal(t.editor.open, true, 'text and grid are of the same sequence now');
  assert.equal(t.editor.text, '=B6');
  assert.equal(t.model.seq, '11');
  assert.equal(t.peeks().length, 1, 'the answer was read after the change, so it stands');
  // a later insert is held and rebased from that same sequence
  t.server.manual.add('rebase');
  t.insertAbove();
  assert.deepEqual([t.server.rebases[0].text, t.server.rebases[0].base, t.server.rebases[0].through], ['=B6', '11', '12']);
});

test('a change of the cell itself outdates a peek answer, and a change elsewhere does not', async () => {
  let t = await formulaInA1();
  t.timers.advance(PEEK_DELAY);
  t.server.setCell(1, 0, 0, '=B5*2');
  t.answer(t.peeks()[0], '10', '=B5');
  assert.equal(t.cached(), null);
  t.timers.advance(PEEK_DELAY);
  t.answer(t.peeks()[1], '11', '=B5*2');
  assert.equal(t.app.bar.value, '=B5*2');

  t = await formulaInA1();
  t.timers.advance(PEEK_DELAY);
  t.server.setCell(1, 8, 3, 'changed elsewhere');
  t.answer(t.peeks()[0], '10', '=B5');
  assert.equal(t.app.bar.value, '=B5', 'the text of sequence 10 still holds at 11');
  t.timers.advance(PEEK_DELAY);
  assert.equal(t.peeks().length, 1);
});

test('a change on another tab outdates a peek answer: the formula may name that tab', async () => {
  const t = await formulaInA1();
  t.timers.advance(PEEK_DELAY);
  t.server.insertRows(2, 0);
  t.answer(t.peeks()[0], '10', '=Other!B5');
  assert.equal(t.cached(), null);
  t.timers.advance(PEEK_DELAY);
  t.answer(t.peeks()[1], '11', '=Other!B6');
  assert.equal(t.app.bar.value, '=Other!B6');
});

test('a snapshot outdates a peek answer read before it', async () => {
  const t = await formulaInA1();
  t.timers.advance(PEEK_DELAY);
  const first = t.peeks()[0];
  // The connection is resynced: deltas up to 12 never arrive one by one.
  t.server.seq = 12;
  t.server.socket.recv({ type: 'resync', generation: 5, seq: '12', date: t.server.date, tabs: t.server.tabInfo() });
  t.server.snapshot(t.server.socket, 5, 1);
  t.answer(first, '11', '=B5');
  assert.equal(t.cached(), null);
  t.timers.advance(PEEK_DELAY);
  t.answer(t.peeks()[1], '12', '=B7');
  assert.equal(t.app.bar.value, '=B7');
});

test('an editor a late peek answer opens on the tab being left keeps its text through every commit until the snapshot offers it back', async () => {
  const t = await formulaInA1();
  key(t, 'F2');
  const first = t.peeks()[0];
  type(t, '+2');
  t.server.manual.add('watch');
  t.app.tabs.list.children[1].click();
  assert.equal(t.model.tab.id, 1, 'the other tab has not arrived');
  t.answer(first, '10', '=B5');
  assert.equal(t.editor.text, '=B5+2', 'the answer opens the editor with the keys typed meanwhile');
  const commits = [
    ['Enter', () => key(t, 'Enter')],
    ['Tab', () => key(t, 'Tab')],
    ['a press outside the grid', () => fire(t.doc.body, 'mousedown', { button: 0 })],
    ['a click on another cell', () => t.grid.h.down({ zone: 'cell', r: 3, c: 3 }, { preventDefault() {} })],
  ];
  const active = t.app.sel.active();
  for (const [what, commit] of commits) {
    commit();
    assert.equal(t.editor.open, true, `${what} leaves the editor open`);
    assert.equal(t.editor.text, '=B5+2', `${what} keeps the text`);
    assert.deepEqual(t.app.sel.active(), active, `${what} keeps the edited cell active`);
  }
  assert.equal(t.server.socket.frames('action').length, 0, 'nothing is written into the tab the person left');
  assert.match(notices(t).at(-1)[1], /The tab you opened is still loading/);
  t.server.manual.delete('watch');
  t.server.snapshot(t.server.socket, t.net.generation, 2);
  assert.equal(t.model.tab.id, 2);
  assert.equal(t.editor.ed, null);
  assert.ok(notices(t).some(([, , kept]) => kept === '=B5+2'), 'the snapshot offers the text back');
  assert.equal(t.server.socket.frames('action').length, 0);
});

// barEdit clicks the formula bar of the active cell, as a person does, and
// types there while the editor waits for the formula text.
function barEdit(t, text) {
  fire(t.app.bar, 'mousedown');
  t.app.bar.focus();
  assert.equal(t.editor.ed?.waiting, true, 'the editor waits for the formula text');
  assert.equal(t.editor.area, t.app.bar);
  type(t, text);
  assert.equal(t.app.bar.value, text, 'the bar shows what was typed');
}

test('keys typed in the formula bar while the formula text is on its way follow the formula', async () => {
  const t = await formulaInA1();
  barEdit(t, '+2');
  t.answer(t.peeks()[0], '10', '=B5');
  assert.equal(t.editor.open, true);
  assert.deepEqual([t.editor.text, t.app.bar.value, t.grid.input.value], ['=B5+2', '=B5+2', '=B5+2']);
  assert.equal(t.server.socket.frames('action').length, 0, 'nothing is written before the person commits');
  key(t, 'Enter');
  const a = t.server.socket.frames('action');
  assert.equal(a.length, 1);
  assert.deepEqual([a[0].ops[0].row, a[0].ops[0].col, a[0].ops[0].text, a[0].ops[0].at], [t.row, t.col, '=B5+2', [0, 0]]);
});

test('a snapshot offers back the keys typed in the formula bar while the formula text was on its way', async () => {
  const t = await formulaInA1();
  barEdit(t, '+2');
  t.server.manual.add('watch');
  t.app.tabs.list.children[1].click();
  assert.equal(t.editor.ed?.waiting, true);
  t.server.manual.delete('watch');
  t.server.snapshot(t.server.socket, t.net.generation, 2);
  assert.equal(t.model.tab.id, 2);
  assert.equal(t.editor.ed, null);
  assert.ok(notices(t).some(([, text, kept]) => /The tab was loaded again while you were typing/.test(text) && kept === '+2'), 'the snapshot offers the text back');
  assert.equal(t.server.socket.frames('action').length, 0);
});

test('a click on another cell while the editor waits for its formula leaves the edit on its own cell', async () => {
  const t = await formulaInA1();
  key(t, 'F2');
  const first = t.peeks()[0];
  type(t, '+1');
  t.grid.h.down({ zone: 'cell', r: 3, c: 3 }, { preventDefault() {} });
  t.grid.h.up({});
  assert.deepEqual([t.app.sel.active().r, t.app.sel.active().c], [0, 0], 'the text area stays on the cell being edited');
  t.answer(first, '10', '=B5');
  assert.equal(t.editor.text, '=B5+1');
  key(t, 'Enter');
  const a = t.server.socket.frames('action')[0];
  assert.deepEqual([a.ops[0].row, a.ops[0].col, a.ops[0].text, a.ops[0].at], [t.row, t.col, '=B5+1', [0, 0]]);
});

test('a peek answer for a tab that is no longer open is dropped', async () => {
  const t = await formulaInA1();
  t.timers.advance(PEEK_DELAY);
  const first = t.peeks()[0];
  t.app.openTab(2);
  t.answer(first, '10', '=B5');
  assert.equal(t.model.tab.id, 2);
  assert.equal(t.cached(), null);
  assert.equal(t.app.bar.value, '');
});

test('a peek the lost connection carried is asked again after the reconnect', async () => {
  const t = await formulaInA1();
  key(t, 'F2');
  assert.equal(t.peeks().length, 1);
  t.server.socket.drop();
  t.timers.advance(1000);
  t.server.socket.open();
  assert.equal(t.peeks().length, 2, 'the waiting editor asks on the new connection');
  t.answer(t.peeks()[1], '10', '=B5');
  assert.equal(t.editor.text, '=B5');
  // the formula bar asks again too, when no editor waits
  const b = await formulaInA1();
  b.timers.advance(PEEK_DELAY);
  b.server.socket.drop();
  b.timers.advance(1000);
  b.server.socket.open();
  b.timers.advance(PEEK_DELAY);
  assert.equal(b.peeks().length, 2);
  b.answer(b.peeks()[1], '10', '=B5');
  assert.equal(b.app.bar.value, '=B5');
});

test('a peek answer that does not fit Appendix A is not installed, the stream goes on, and the text is asked for again', async () => {
  // An answer whose id does not fit could answer any request out, so every
  // one of them is asked for again.
  for (const bad of [{ seq: 'ten' }, { seq: 10 }, { edit: 5 }, { formula: 'yes' }, { edit: null }, { id: null }, { id: 'one' }, { id: 0 }]) {
    const t = await formulaInA1();
    t.timers.advance(PEEK_DELAY);
    const first = t.peeks()[0];
    assert.throws(() => t.server.socket.recv({ type: 'peeked', id: first.id, seq: '10', edit: '=B5', formula: true, ...bad }), TypeError);
    assert.equal(t.cached(), null, `${JSON.stringify(bad)}: nothing is installed`);
    // Later deltas apply as before.
    t.server.setCell(1, 2, 2, 'later');
    assert.equal(t.model.stale, false);
    assert.equal(t.model.cellAt(2, 2).value, 'later');
    t.timers.advance(PEEK_DELAY);
    assert.equal(t.peeks().length, 2, `${JSON.stringify(bad)}: the text is asked for again`);
    t.answer(t.peeks()[1], '11', '=B5');
    assert.equal(t.cached(), '=B5');
  }
});
