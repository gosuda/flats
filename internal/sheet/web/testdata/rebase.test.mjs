// A formula in flight (TECH 8.4): the browser parts of cases C-05, S-01,
// S-05 and S-46. The editor holds `=B5+` in D3 and another writer inserts a
// row above. The fake server adjusts text the way the real one does for
// this one insert: every row number at or after the insert grows by one.
'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { HOLD_MAX } from '../protocol.js';
import { fire } from './dom.mjs';
import { FakeServer, key, notices, shownAt, startApp, structField, type } from './server.mjs';

const shift = (text, by = 1) => text.replace(/([A-Z]+)(\d+)/g, (_, c, r) => c + (Number(r) + by));

async function editing() {
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 30, cols: 6, cells: { '4,1': '5', '5,1': '6' } }, { id: 2, name: 'Old', rows: 5, cols: 3 }] });
  const t = await startApp({ server });
  server.manual.add('rebase');
  t.sel.set(2, 3);
  t.app.changed();
  type(t, '=B5+');
  const answer = (i, extra = {}) => {
    const m = server.rebases[i];
    server.socket.recv({ type: 'rebased', id: m.id, text: shift(m.text), at: [m.at[0] + 1, m.at[1]], base: m.through, ...extra });
  };
  return { ...t, answer, rows: () => t.model.rowCount, click: (r, c) => { t.grid.h.down({ zone: 'cell', r, c }, { preventDefault() {} }); t.grid.h.up({}); } };
}

test('a structural delta is held while the editor holds a formula, and a rebase is asked', async () => {
  const t = await editing();
  const d = t.server.insertRows(1, 0);
  assert.equal(t.rows(), 30, 'the grid does not change yet');
  assert.equal(shownAt(t, 4, 1), '5');
  assert.equal(t.model.seq, '10', 'the screen is still the one at the base');
  assert.deepEqual(t.server.rebases[0], { type: 'rebase', id: t.server.rebases[0].id, tab: 1, row: t.model.rowAt(2), col: t.model.colAt(3), text: '=B5+', at: [2, 3], base: '10', through: d.seq });
  // every later delta is held too, in order, whatever it carries
  t.server.setCell(1, 0, 0, 'later');
  assert.equal(t.model.cellAt(0, 0), null);
  t.answer(0);
  assert.equal(t.rows(), 31, 'text and grid move together');
  assert.equal(t.editor.text, '=B6+');
  assert.equal(t.model.seq, '12');
  assert.equal(t.model.shown(t.model.rowAt(0), t.model.colAt(0)), 'later', 'the cell delta held behind it applied too');
  assert.deepEqual(t.editor.position(), { r: 3, c: 3 });
  assert.equal(t.editor.open, true);
});

test('C-05 a and b: a click or typed characters during the exchange are read against the old screen', async () => {
  for (const how of ['click', 'type']) {
    const t = await editing();
    t.server.insertRows(1, 0);
    if (how === 'click') t.click(5, 1); else type(t, 'B6');
    assert.equal(t.editor.text, '=B5+B6', 'the screen still shows the old grid, so B6 names the cell under 5');
    t.answer(0);
    assert.equal(t.rows(), 30, 'the answer is for text the editor no longer has');
    assert.equal(t.editor.text, '=B5+B6');
    assert.deepEqual([t.server.rebases[1].text, t.server.rebases[1].base, t.server.rebases[1].through], ['=B5+B6', '10', '11'], 'the new text goes out with the same base');
    t.answer(1);
    assert.equal(t.rows(), 31);
    assert.equal(t.editor.text, '=B6+B7');
    assert.equal(shownAt(t, 6, 1), '6', 'the cell the person clicked is the one the formula names');
  }
});

test('C-05 c: typing through three exchanges ends with a read-only editor for one round trip', async () => {
  const t = await editing();
  t.server.insertRows(1, 0);
  for (let i = 0; i < 3; i++) {
    assert.equal(t.grid.input.readOnly, false);
    type(t, String(i));
    t.answer(i);
  }
  assert.equal(t.server.rebases.length, 4);
  assert.equal(t.grid.input.readOnly, true);
  assert.equal(t.app.bar.readOnly, true);
  assert.equal(t.rows(), 30);
  // Enter during the last round trip is ignored: the text is not committed against a grid it is not bound to
  key(t, 'Enter');
  assert.equal(t.editor.open, true);
  t.answer(3);
  assert.equal(t.rows(), 31);
  assert.equal(t.editor.text, '=B6+012');
  assert.equal(t.grid.input.readOnly, false, 'the editor is editable again');
});

test('a read-only editor that a tab switch left open keeps its text through every commit once the answer makes it editable', async () => {
  const t = await editing();
  t.server.insertRows(1, 0);
  type(t, '1');
  t.timers.advance(500);
  t.answer(0);
  assert.equal(t.grid.input.readOnly, true);
  t.server.manual.add('watch');
  t.app.tabs.list.children[1].click();
  assert.equal(t.editor.open, true, 'the read-only editor commits nothing on the switch');
  t.answer(1);
  assert.deepEqual([t.grid.input.readOnly, t.editor.text, t.rows()], [false, '=B6+1', 31]);
  const commits = [
    ['Enter', () => key(t, 'Enter')],
    ['Tab', () => key(t, 'Tab')],
    ['a press outside the grid', () => fire(t.doc.body, 'mousedown', { button: 0 })],
    ['a click on another cell', () => t.click(8, 1)],
  ];
  const active = t.app.sel.active();
  for (const [what, commit] of commits) {
    commit();
    assert.equal(t.editor.open, true, `${what} leaves the editor open`);
    assert.equal(t.editor.text, '=B6+1', `${what} keeps the text`);
    assert.deepEqual(t.app.sel.active(), active, `${what} keeps the edited cell active`);
  }
  assert.equal(t.server.socket.frames('action').length, 0, 'nothing is written into the tab the person left');
  t.server.manual.delete('watch');
  t.server.snapshot(t.server.socket, t.net.generation, 2);
  assert.equal(t.editor.ed, null);
  assert.ok(notices(t).some(([, , kept]) => kept === '=B6+1'), 'the snapshot offers the text back');
  assert.equal(t.server.socket.frames('action').length, 0);
});

test('the editor also turns read-only after 500 ms of exchanges', async () => {
  const t = await editing();
  t.server.insertRows(1, 0);
  type(t, '1');
  t.timers.advance(500);
  t.answer(0);
  assert.equal(t.grid.input.readOnly, true);
  t.answer(1);
  assert.equal(t.editor.text, '=B6+1');
  assert.equal(t.grid.input.readOnly, false);
});

test('C-05 d: a second structural change before the first answer is applied in order', async () => {
  const t = await editing();
  t.server.insertRows(1, 0);
  t.server.insertRows(1, 0);
  assert.equal(t.server.rebases.length, 1, 'one exchange at a time');
  assert.equal(t.server.rebases[0].through, '11');
  t.answer(0);
  assert.equal(t.rows(), 31, 'the held deltas apply up to the answered sequence');
  assert.equal(t.editor.text, '=B6+');
  assert.deepEqual([t.server.rebases[1].base, t.server.rebases[1].through, t.server.rebases[1].text], ['11', '12', '=B6+']);
  assert.deepEqual(t.server.rebases[1].at, [3, 3]);
  t.answer(1);
  assert.equal(t.rows(), 32);
  assert.equal(t.editor.text, '=B7+');
  assert.equal(t.model.seq, '12');
});

test('C-05 e: a composition in progress waits for its end before the exchange', async () => {
  const t = await editing();
  fire(t.grid.input, 'compositionstart');
  t.server.insertRows(1, 0);
  assert.equal(t.server.rebases.length, 0, 'nothing is asked while the composition runs');
  assert.equal(t.rows(), 30);
  t.grid.input.value = '=B5+B6';
  fire(t.grid.input, 'compositionend');
  assert.equal(t.server.rebases[0].text, '=B5+B6');
  t.answer(0);
  assert.equal(t.editor.text, '=B6+B7');
  assert.equal(t.rows(), 31);
});

test('C-05 f: Enter before the answer commits with the old base and applies the held deltas', async () => {
  const t = await editing();
  const row = t.model.rowAt(2);
  t.server.insertRows(1, 0);
  type(t, 'B6');
  key(t, 'Enter');
  const action = t.server.socket.frames('action')[0];
  assert.equal(action.base, '10', 'the server adjusts from the screen the person saw');
  assert.deepEqual(action.ops[0], { op: 'set', tab: 1, row, col: t.model.colAt(3), text: '=B5+B6', at: [2, 3] });
  assert.equal(t.editor.open, false);
  assert.equal(t.rows(), 31, 'the held insert shows once the editor is closed');
  assert.deepEqual(t.sel.active(), { r: 4, c: 3 }, 'the selection moved down from the cell, where it lies now');
  t.answer(0);
  assert.equal(t.rows(), 31, 'a late answer changes nothing');
  assert.deepEqual(notices(t), []);
});

test('C-05 g: nothing changes before a delayed answer arrives', async () => {
  const t = await editing();
  t.server.insertRows(1, 0);
  t.timers.advance(5000);
  assert.equal(t.rows(), 30);
  assert.equal(t.editor.text, '=B5+');
  assert.equal(t.grid.input.readOnly, false);
  t.answer(0);
  assert.equal(t.rows(), 31);
});

test('C-05 h and S-05: a refused rebase closes the editor and offers the text back before the grid changes', async () => {
  const t = await editing();
  t.server.insertRows(1, 0);
  let rowsAtNotice = null;
  const show = t.app.notices.show.bind(t.app.notices);
  t.app.notices.show = (n) => { rowsAtNotice = t.rows(); return show(n); };
  t.server.socket.recv({ type: 'rebased', id: t.server.rebases[0].id, refused: 'stale_formula' });
  assert.equal(t.editor.open, false);
  assert.equal(rowsAtNotice, 30, 'the text is offered before the grid changes');
  assert.equal(t.rows(), 31);
  assert.equal(t.server.socket.frames('action').length, 0, 'nothing was committed');
  const [[kind, text, kept]] = notices(t);
  assert.equal(kind, 'warn');
  assert.match(text, /could not adjust the formula you were typing to the changed grid/);
  assert.equal(kept, '=B5+');
  assert.equal(t.doc.activeElement, t.grid.input);
});

test('a rebase answer that does not fit Appendix A closes the editor and offers the text back', async () => {
  // Each is laid over a whole answer, or sent alone with its id.
  const cases = [
    [{ base: 'twelve' }], [{ text: 5 }], [{ at: 'B6' }], [{ at: [1] }], [{ refused: true }],
    // An empty reason is no refusal the consumer can tell from success.
    [{ refused: '' }], [{ refused: '' }, true], [{ refused: '', base: 'eleven' }, true],
    // A refusal carries nothing else: an answer with a reason and a text
    // is neither.
    [{ refused: 'stale_formula' }],
    // A null is no field left out, and an answer whose id does not fit
    // can be the answer to the exchange in flight.
    [{ text: null }], [{ at: [1, null] }], [{ id: null }], [{ id: null }, true], [{ id: '1' }], [{ id: 0, refused: 'gone' }, true],
    // The new base is the sequence the exchange asked for, and no other.
    [{ base: '99' }], [{ base: '10' }],
  ];
  for (const [bad, alone] of cases) {
    const name = `${JSON.stringify(bad)}${alone ? ' alone' : ''}`;
    const t = await editing();
    t.server.insertRows(1, 0);
    const m = t.server.rebases[0];
    let rowsAtNotice = null;
    const show = t.app.notices.show.bind(t.app.notices);
    t.app.notices.show = (n) => { rowsAtNotice ??= t.rows(); return show(n); };
    const frame = alone ? { type: 'rebased', id: m.id, ...bad } : { type: 'rebased', id: m.id, text: shift(m.text), at: [m.at[0] + 1, m.at[1]], base: m.through, ...bad };
    assert.throws(() => t.server.socket.recv(frame), TypeError, name);
    assert.equal(t.editor.open, false, `${name}: the editor closed`);
    assert.ok(notices(t).some(([, , kept]) => kept === '=B5+'), `${name}: and offered its text back`);
    assert.equal(rowsAtNotice, 30, `${name}: the text is offered before the grid changes`);
    assert.equal(t.rows(), 31, `${name}: the held insert applied`);
    assert.equal(t.server.socket.frames('action').length, 0, `${name}: nothing was committed`);
  }
});

test('C-05 i and S-46: a rollback closes the editor without committing, for a formula and for a plain value', async () => {
  for (const text of ['=B5+', 'plain']) {
    const t = await editing();
    key(t, 'Escape');
    type(t, text);
    if (text.startsWith('=')) t.server.insertRows(1, 0);
    const before = t.server.socket;
    before.recv({ type: 'reset', seq: '12' });
    assert.equal(t.editor.open, false);
    assert.equal(t.server.sockets.flatMap((s) => s.frames('action')).length, 0, 'nothing is written');
    const [[kind, said, kept]] = notices(t);
    assert.equal(kind, 'warn');
    assert.match(said, /The sheet was rolled back while you were typing in D3\. Nothing was written/);
    assert.equal(kept, text);
    assert.equal(t.app.rebaser.holding, false, 'the held deltas are dropped');
    // the connection starts over and the restored grid arrives as a snapshot
    assert.notEqual(t.server.socket, before);
    t.server.socket.open();
    assert.equal(t.rows(), text.startsWith('=') ? 31 : 30);
    assert.match(notices(t)[1][1], /Undo history was cleared/);
  }
});

test('C-05 j: held deltas over 8 MiB close the editor, and the next insert shows without one', async () => {
  const t = await editing();
  fire(t.grid.input, 'compositionstart');
  t.server.insertRows(1, 0);
  // Each delta holds one cell of as many characters as a cell may hold.
  const big = 'x'.repeat(32767 - 8);
  let n = 0;
  while (t.app.rebaser.bytes <= HOLD_MAX - 40000) { t.server.setCell(1, 9, 0, big + n++); assert.equal(t.editor.open, true); }
  assert.equal(t.rows(), 30);
  t.server.setCell(1, 9, 0, `${big}last`);
  assert.equal(t.editor.open, false, 'the editor closed without committing');
  assert.equal(notices(t)[0][2], '=B5+');
  assert.match(notices(t)[0][1], /Too many changes arrived while you were typing a formula/);
  assert.equal(t.rows(), 31, 'the held deltas applied in order');
  assert.equal(t.app.rebaser.holding, false);
  assert.equal(t.server.socket.frames('action').length, 0);
  t.server.insertRows(1, 0);
  assert.equal(t.rows(), 32, 'no editor is open when the later insert shows');
  assert.equal(t.editor.open, false);
});

test('a tab rename is held like a row insert, and a cell edit elsewhere is not', async () => {
  const t = await editing();
  t.server.setCell(1, 0, 0, 'plain change');
  assert.equal(t.model.shown(t.model.rowAt(0), t.model.colAt(0)), 'plain change', 'a cell delta applies at once');
  assert.equal(t.app.rebaser.holding, false);
  t.server.renameTab(2, 'New name');
  assert.equal(t.app.rebaser.holding, true);
  assert.equal(t.model.tabs[1].name, 'Old', 'the tab list does not change under the text');
  t.server.socket.recv({ type: 'rebased', id: t.server.rebases[0].id, text: '=B5+', at: [2, 3], base: '12' });
  assert.equal(t.model.tabs[1].name, 'New name');
  assert.equal(t.editor.text, '=B5+');
});

test('S-01: a formula committed with a base before an insert names the screen it was typed on', async () => {
  const t = await editing();
  t.server.manual.add('action');
  // the insert is still on its way when the person presses Enter
  type(t, 'B6');
  key(t, 'Enter');
  const a = t.server.socket.frames('action')[0];
  assert.deepEqual([a.base, a.ops[0].text, a.ops[0].at], ['10', '=B5+B6', [2, 3]]);
  assert.equal(shownAt(t, 2, 3), '=B5+B6', 'pending shows the typed text; the client does not rewrite it');
  // the server stores the adjusted formula and answers with its value
  t.server.insertRows(1, 0);
  t.server.socket.recv({ type: 'delta', seq: '12', from: { session_id: 's1', n: '1' }, cells: [[a.ops[0].row, [[a.ops[0].col, 'nf', 11]]]] });
  t.server.socket.recv({ type: 'ack', n: '1', seq: '12', results: [{ status: 'applied', applied: 1 }] });
  assert.equal(shownAt(t, 3, 3), '11');
});

test('a resync under a formula editor closes it with the text offered back', async () => {
  const t = await editing();
  t.server.insertRows(1, 0);
  t.server.socket.recv({ type: 'resync', generation: 7, seq: '11', date: t.server.date, tabs: t.server.tabInfo() });
  assert.equal(t.editor.open, false);
  assert.equal(notices(t)[0][2], '=B5+');
  assert.equal(t.app.rebaser.holding, false);
  t.server.snapshot(t.server.socket, 7, 1);
  assert.equal(t.rows(), 31, 'the new snapshot replaces the held deltas');
});

// An ordinary snapshot that arrives while the editor holds deltas drops
// them, as a resync does. Its head carries the tab list and the date of
// its sequence, so what the dropped deltas changed of both is on screen
// with the new grid. A head older than the stream installs neither: the
// stream's own stay, and the tab is asked for again.
test('a snapshot under a formula editor drops the held deltas and installs the tab list and the date of its head', async () => {
  const t = await editing();
  t.server.renameTab(2, 'New name');
  t.server.date = '2026-11-14';
  assert.equal(t.app.rebaser.holding, true);
  assert.equal(t.model.tabs[1].name, 'Old');
  t.net.watch(1);
  t.server.snapshot(t.server.socket, t.net.generation, 1);
  assert.equal(t.editor.open, false);
  assert.equal(notices(t)[0][2], '=B5+');
  assert.equal(t.app.rebaser.holding, false);
  assert.deepEqual([t.model.tabs[1].name, t.model.date, t.model.seq], ['New name', '2026-11-14', '11']);
  assert.deepEqual([t.model.stale, t.app.canWrite()], [false, true]);

  t.server.manual.add('watch');
  const watches = t.server.socket.frames('watch').length;
  const head = { type: 'tab_head', generation: t.net.generation, tab: 1, seq: '10', date: '2026-11-13', tabs: [{ ...t.server.tabInfo()[0] }, { ...t.server.tabInfo()[1], name: 'Old' }], rows: [], cols: [], widths: {}, frozen_rows: 0, frozen_cols: 0, styles: [{}] };
  t.server.socket.recv(head);
  assert.deepEqual([t.model.tabs[1].name, t.model.date], ['New name', '2026-11-14'], 'an older head leaves the tab list and the date of the stream');
  assert.deepEqual([t.model.seq, t.model.stale], ['10', true]);
  assert.equal(t.server.socket.frames('watch').length, watches + 1, 'and the tab is asked for again');
});

// ---- a formula that names another tab (rule 14) ----

// The editor in Summary!A1 holds =Data!B5+1. frames collects what the
// connection receives.
async function crossTab(text = '=Data!B5+1') {
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Summary', rows: 10, cols: 4 }, { id: 2, name: 'Data', rows: 30, cols: 6, cells: { '4,1': '5' } }, { id: 3, name: 'Other', rows: 5, cols: 3 }] });
  const t = await startApp({ server });
  server.manual.add('rebase');
  const frames = [];
  const recv = server.socket.recv.bind(server.socket);
  server.socket.recv = (m) => { frames.push(m); recv(m); };
  type(t, text);
  const answer = (i, adjusted) => {
    const m = server.rebases[i];
    server.socket.recv({ type: 'rebased', id: m.id, text: adjusted, at: m.at, base: m.through });
  };
  return { ...t, frames, answer, dataRows: () => t.model.tabInfo(2).rows };
}

test('a row inserted on another tab is held and the formula that names that tab is rebased', async () => {
  const t = await crossTab();
  const d = t.server.insertRows(2, 0);
  const frame = t.frames.at(-1);
  assert.deepEqual(frame.struct, structField([{ tab: 2, op: 'insert_rows', ids: d.struct[0].ids, after: null }], 1), 'the delta names the operation and its tab');
  assert.deepEqual(frame.touched, [2]);
  assert.equal(frame.cells, undefined, 'and carries no cell of a tab that is not watched');
  assert.equal(t.app.rebaser.holding, true);
  assert.equal(t.model.seq, '10', 'the screen stays the one the text was typed on');
  assert.equal(t.dataRows(), 30);
  assert.deepEqual(t.server.rebases[0], { type: 'rebase', id: t.server.rebases[0].id, tab: 1, row: t.model.rowAt(0), col: t.model.colAt(0), text: '=Data!B5+1', at: [0, 0], base: '10', through: '11' });
  t.answer(0, '=Data!B6+1');
  assert.equal(t.editor.text, '=Data!B6+1', 'the reference follows the cell it named');
  assert.equal(t.model.seq, '11');
  assert.equal(t.dataRows(), 31);
  assert.equal(t.model.rowCount, 10, 'an operation on another tab leaves the watched grid alone');
  key(t, 'Enter');
  const a = t.server.socket.frames('action')[0];
  assert.deepEqual([a.base, a.ops[0].text], ['11', '=Data!B6+1']);
});

test('structural operations on another tab are held in every form a delta can name them', async () => {
  const insert = { tab: 2, op: 'insert_rows', ids: [[9000, 1]], after: null };
  const tabs = (t, rows) => t.server.tabInfo().map((x) => (x.id === 2 ? { ...x, rows } : x));
  const forms = {
    'the operation alone': (t) => ({ struct: structField([insert], 1), touched: [2] }),
    'the operation and the tab list': (t) => ({ struct: structField([insert], 1), touched: [2], tabs: tabs(t, 31) }),
    'the tab list alone, with a changed row count': (t) => ({ touched: [2], tabs: tabs(t, 31) }),
    'a deleted column': () => ({ struct: structField([{ tab: 2, op: 'delete_cols', ids: [[9001, 1]] }], 1), touched: [2] }),
    'a sort': () => ({ struct: structField([{ tab: 2, op: 'sort', positions: [[0, 2]], ids: [9003, 9002] }], 1), touched: [2] }),
    'a rename': () => ({ struct: structField([{ tab: 2, op: 'rename_tab', name: 'Facts' }], 1) }),
    'a deleted tab': (t) => ({ struct: structField([{ tab: 3, op: 'delete_tab' }], 1), tabs: t.server.tabInfo().filter((x) => x.id !== 3) }),
  };
  for (const [name, make] of Object.entries(forms)) {
    const t = await crossTab();
    t.server.socket.recv({ type: 'delta', seq: '11', ...make(t) });
    assert.equal(t.app.rebaser.holding, true, name);
    assert.equal(t.model.seq, '10', name);
    assert.deepEqual([t.server.rebases[0].base, t.server.rebases[0].through], ['10', '11'], name);
    t.answer(0, '=Data!B6+1');
    assert.equal(t.model.seq, '11', name);
    assert.equal(t.editor.text, '=Data!B6+1', name);
    assert.deepEqual([t.model.rowCount, t.model.colCount], [10, 4], `${name}: the watched grid keeps its rows and columns`);
  }
});

test('a delta that names the watched tab on its own operations moves the watched grid', async () => {
  const t = await crossTab('=A5+1');
  t.sel.set(0, 0);
  t.server.socket.recv({ type: 'delta', seq: '11', struct: [{ tab: 1, op: 'insert_rows', ids: [[9000, 1]], after: null }, { tab: 2, op: 'insert_rows', ids: [[9001, 1]], after: null }] });
  assert.equal(t.model.rowCount, 10, 'held');
  t.answer(0, '=A6+1');
  assert.equal(t.model.rowCount, 11, 'only the operation of the watched tab changed its rows');
  assert.equal(t.model.rowAt(0), 9000);
});

test('an added or a moved tab is not held, and a plain value holds nothing back', async () => {
  const t = await crossTab();
  const added = [...t.server.tabInfo(), { id: 4, name: 'New', rows: 20, cols: 6, frozen_rows: 0, frozen_cols: 0 }];
  t.server.socket.recv({ type: 'delta', seq: '11', struct: structField([{ tab: 4, op: 'add_tab' }], 1), tabs: added });
  t.server.socket.recv({ type: 'delta', seq: '12', struct: structField([{ tab: 3, op: 'move_tab' }], 1), tabs: [...added].reverse() });
  assert.equal(t.app.rebaser.holding, false);
  assert.equal(t.model.seq, '12');
  assert.equal(t.server.rebases.length, 0);
  const plain = await crossTab('plain text');
  plain.server.insertRows(2, 0);
  assert.equal(plain.app.rebaser.holding, false, 'only a formula is bound to a screen');
  assert.equal(plain.dataRows(), 31);
});

test('the exchange for another tab keeps its rules: typing, composition, Enter, refusal and rollback', async () => {
  // typed characters during the exchange go out again with the same base
  let t = await crossTab();
  t.server.insertRows(2, 0);
  type(t, '0');
  t.answer(0, '=Data!B6+1');
  assert.equal(t.editor.text, '=Data!B5+10', 'an answer for text the editor no longer has changes nothing');
  assert.equal(t.dataRows(), 30);
  assert.deepEqual([t.server.rebases[1].text, t.server.rebases[1].base, t.server.rebases[1].through], ['=Data!B5+10', '10', '11']);
  t.answer(1, '=Data!B6+10');
  assert.equal(t.editor.text, '=Data!B6+10');
  assert.equal(t.dataRows(), 31);

  // a composition ends before the exchange
  t = await crossTab();
  fire(t.grid.input, 'compositionstart');
  t.server.insertRows(2, 0);
  assert.equal(t.server.rebases.length, 0);
  assert.equal(t.dataRows(), 30);
  t.grid.input.value = '=Data!B5+12';
  fire(t.grid.input, 'compositionend');
  assert.equal(t.server.rebases[0].text, '=Data!B5+12');
  t.answer(0, '=Data!B6+12');
  assert.equal(t.editor.text, '=Data!B6+12');

  // Enter before the answer commits with the base of the screen it was typed on
  t = await crossTab();
  t.server.insertRows(2, 0);
  key(t, 'Enter');
  const a = t.server.socket.frames('action')[0];
  assert.deepEqual([a.base, a.ops[0].text, a.ops[0].at], ['10', '=Data!B5+1', [0, 0]], 'the server adjusts it from that screen');
  assert.equal(t.dataRows(), 31, 'the held delta shows once the editor is closed');

  // a refused rebase closes the editor with the text offered back
  t = await crossTab();
  t.server.deleteTab(2);
  assert.equal(t.model.tabs.length, 3, 'the tab list does not change under the text');
  t.server.socket.recv({ type: 'rebased', id: t.server.rebases[0].id, refused: 'stale_formula' });
  assert.equal(t.editor.open, false);
  assert.equal(notices(t)[0][2], '=Data!B5+1');
  assert.equal(t.model.tabs.length, 2);
  assert.equal(t.server.socket.frames('action').length, 0);

  // a rollback closes it and drops what was held
  t = await crossTab();
  t.server.insertRows(2, 0);
  t.server.socket.recv({ type: 'reset', seq: '12' });
  assert.equal(t.editor.open, false);
  assert.equal(t.app.rebaser.holding, false);
  assert.equal(notices(t)[0][2], '=Data!B5+1');
  assert.equal(t.server.sockets.flatMap((s) => s.frames('action')).length, 0);
});

test('the watched tab deleted under a formula closes the editor with the text offered back', async () => {
  const t = await crossTab();
  t.server.deleteTab(1);
  assert.equal(t.app.rebaser.holding, true);
  assert.equal(t.model.tab.id, 1, 'the grid stays until the exchange ends');
  t.server.socket.recv({ type: 'rebased', id: t.server.rebases[0].id, refused: 'gone' });
  assert.equal(t.editor.open, false);
  assert.equal(notices(t)[0][2], '=Data!B5+1');
  assert.match(notices(t).at(-1)[1], /The tab “Summary” was deleted/);
  assert.equal(t.server.socket.frames('action').length, 0);
  assert.equal(t.server.socket.frames('watch').at(-1).tab, 2, 'another tab is shown');
});

test('held deltas are counted in encoded bytes: text of valid cells outside ASCII closes the editor at 8 MiB', async () => {
  const t = await crossTab();
  fire(t.grid.input, 'compositionstart');
  let bytes = 0;
  const recv = t.server.socket.recv;
  t.server.socket.recv = (m) => { if (m.type === 'delta') bytes += Buffer.byteLength(JSON.stringify(m)); recv(m); };
  t.server.insertRows(2, 0);
  // A cell holds at most 32,767 characters; each of these takes three bytes.
  const cell = '\u6f22'.repeat(32767);
  let sent = 1;
  while (bytes + 3 * 32767 + 200 <= HOLD_MAX) {
    t.server.setCell(1, 3, 0, cell);
    sent++;
    assert.equal(t.editor.open, true, `still open after ${sent} held deltas`);
    assert.equal(t.app.rebaser.bytes, bytes, 'the count is the bytes of the frames as they arrived');
  }
  assert.ok(sent > 80 && sent < 90, `8 MiB is ${sent} such deltas, not three times as many`);
  assert.ok(t.app.rebaser.held.reduce((n, d) => n + JSON.stringify(d).length, 0) < HOLD_MAX / 2, 'in UTF-16 units they are far under the limit');
  t.server.setCell(1, 3, 0, cell);
  assert.equal(t.editor.open, false, 'the editor closed without committing');
  assert.match(notices(t)[0][1], /Too many changes arrived while you were typing a formula/);
  assert.equal(notices(t)[0][2], '=Data!B5+1', 'with the typed text offered back');
  assert.equal(t.app.rebaser.holding, false);
  assert.equal(t.app.rebaser.bytes, 0);
  assert.equal(t.dataRows(), 31, 'the held deltas applied in order');
  assert.equal(t.model.shown(t.model.rowAt(3), t.model.colAt(0)), cell);
  assert.equal(t.server.socket.frames('action').length, 0);
});

// Pending formula text reopened (TECH 8.4). A1 holds =B5 and B5 holds 10.
// The person commits =B5+1 in A1, and another writer changes the grid
// before that action is answered. The pending text names cells of the
// screen it was typed on.
async function pendingFormula() {
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 30, cols: 6, cells: { '0,0': '=B5', '4,1': '10' } }, { id: 2, name: 'Old', rows: 5, cols: 3 }] });
  const t = await startApp({ server });
  for (const kind of ['action', 'rebase', 'peek']) server.manual.add(kind);
  t.sel.set(0, 0);
  t.app.changed();
  type(t, '=B5+1');
  key(t, 'Enter');
  key(t, 'ArrowUp');
  const first = server.socket.frames('action')[0];
  const row = t.model.rowAt(0);
  const col = t.model.colAt(0);
  // confirm delivers the first action's delta and ack, as the host sends
  // them once it has adjusted that action from its own base.
  const confirm = () => {
    const d = server.push({ tab: 1, from: { session_id: 's1', n: first.n }, cells: [[row, [[col, 'nf', 11]]]] });
    server.socket.recv({ type: 'ack', n: first.n, seq: d.seq, results: [{ status: 'applied', applied: 1 }] });
  };
  return { ...t, first, row, col, confirm };
}

const CHANGES = {
  'an insert above row 5': { change: (s) => s.insertRows(1, 4), text: '=B6+1', at: [0, 0] },
  'a delete of row 2': { change: (s) => s.deleteRows(1, 1), text: '=B4+1', at: [0, 0] },
  'a sort of the rows': { change: (s) => s.sortRows(1), text: '=B26+1', at: [29, 0] },
  'a rename of a tab': { change: (s) => s.renameTab(2, 'New'), text: '=B5+1', at: [0, 0] },
};

test('pending formula text reopened after a structural change is adjusted from its own base before it opens', async () => {
  for (const [what, c] of Object.entries(CHANGES)) {
    for (const when of ['after the second commit', 'while the text is adjusted', 'after the editor opened']) {
      const t = await pendingFormula();
      assert.deepEqual([t.first.base, t.first.ops[0].text], ['10', '=B5+1']);
      c.change(t.server);
      assert.equal(t.model.seq, '11');
      t.sel.set(c.at[0], c.at[1]);
      t.app.changed();
      assert.equal(t.app.bar.value, '', `${what}: the bar does not show text of a screen the grid has left`);
      key(t, 'F2');
      assert.equal(t.editor.open, false, `${what}: the editor waits for the adjusted text`);
      const r = t.server.rebases[0];
      assert.deepEqual([r.tab, r.row, r.col, r.text, r.at, r.base, r.through], [1, t.row, t.col, '=B5+1', [0, 0], '10', '11'], `${what}: the text goes out with the base and place it was typed at`);
      type(t, '+1');
      if (when === 'while the text is adjusted') t.confirm();
      t.server.socket.recv({ type: 'rebased', id: r.id, text: c.text, at: c.at, base: '11' });
      assert.equal(t.editor.open, true);
      assert.equal(t.editor.text, `${c.text}+1`, `${what}: the keys follow the adjusted text`);
      if (when === 'after the editor opened') t.confirm();
      key(t, 'Enter');
      const second = t.server.socket.frames('action')[1];
      const base = when === 'after the second commit' ? '11' : '12';
      assert.deepEqual([second.base, second.ops[0].text, second.ops[0].at], [base, `${c.text}+1`, c.at], `${what}, confirmed ${when}`);
      if (when === 'after the second commit') t.confirm();
      assert.equal(t.server.rebases.length, 1);
      assert.deepEqual(notices(t), []);
    }
  }
});

test('pending formula text with no structural change since opens at once', async () => {
  const t = await pendingFormula();
  t.server.setCell(1, 8, 3, 'elsewhere');
  assert.equal(t.app.bar.value, '=B5+1');
  key(t, 'F2');
  assert.equal(t.editor.text, '=B5+1');
  assert.equal(t.server.rebases.length, 0);
});

test('a structural change while reopened text is adjusted is held and exchanged once the editor opens', async () => {
  const t = await pendingFormula();
  t.server.insertRows(1, 4);
  key(t, 'F2');
  t.server.insertRows(1, 4);
  assert.equal(t.model.rowCount, 31, 'the screen the text is adjusted to stays');
  type(t, '+B6');
  t.server.socket.recv({ type: 'rebased', id: t.server.rebases[0].id, text: '=B6+1', at: [0, 0], base: '11' });
  assert.equal(t.editor.text, '=B6+1+B6');
  const r = t.server.rebases[1];
  assert.deepEqual([r.text, r.base, r.through], ['=B6+1+B6', '11', '12']);
  t.server.socket.recv({ type: 'rebased', id: r.id, text: '=B7+1+B7', at: [0, 0], base: '12' });
  assert.equal(t.model.rowCount, 32);
  assert.equal(t.editor.text, '=B7+1+B7');
  key(t, 'Enter');
  const a = t.server.socket.frames('action')[1];
  assert.deepEqual([a.base, a.ops[0].text], ['12', '=B7+1+B7']);
});

test('reopened text the host cannot adjust closes the editor and offers the text back', async () => {
  const t = await pendingFormula();
  t.server.insertRows(1, 4);
  key(t, 'F2');
  type(t, '+1');
  t.server.socket.recv({ type: 'rebased', id: t.server.rebases[0].id, refused: 'stale_formula' });
  assert.equal(t.editor.ed, null);
  assert.ok(notices(t).some(([, text, kept]) => /could not adjust the formula/.test(text) && kept === '=B5+1+1'));
  assert.equal(t.server.socket.frames('action').length, 1, 'nothing more is written');
});

test('reopened text whose exchange the lost connection carried is asked again after the reconnect', async () => {
  const t = await pendingFormula();
  t.server.insertRows(1, 4);
  key(t, 'F2');
  assert.equal(t.server.rebases.length, 1);
  t.server.socket.drop();
  t.timers.advance(1000);
  t.server.socket.open();
  assert.equal(t.server.rebases.length, 2);
  assert.deepEqual([t.server.rebases[1].text, t.server.rebases[1].base, t.server.rebases[1].through], ['=B5+1', '10', '11']);
  t.server.socket.recv({ type: 'rebased', id: t.server.rebases[1].id, text: '=B6+1', at: [0, 0], base: '11' });
  assert.equal(t.editor.text, '=B6+1');
});

// The exchange of reopened text belongs to the editor that asked for it.
// A1 holds pending =B5+1 and C1 pending =D5+1 when another writer inserts
// a row above row 5. The person reopens A1, cancels before its answer and
// reopens C1, or opens nothing; the confirmations of both actions arrive
// before or after the late answer.
test('a cancelled reopen leaves its late answer to no other editor', async () => {
  for (const next of ['C1', 'nothing']) {
    for (const confirmed of ['before the late answer', 'after the answers']) {
      const what = `${next} reopened, confirmations ${confirmed}`;
      const t = await pendingFormula();
      t.sel.set(0, 2);
      t.app.changed();
      type(t, '=D5+1');
      key(t, 'Enter');
      key(t, 'ArrowUp');
      const second = t.server.socket.frames('action')[1];
      const c1 = [t.model.rowAt(0), t.model.colAt(2)];
      const confirm = () => {
        t.confirm();
        const d = t.server.push({ tab: 1, from: { session_id: 's1', n: second.n }, cells: [[c1[0], [[c1[1], 'nf', 21]]]] });
        t.server.socket.recv({ type: 'ack', n: second.n, seq: d.seq, results: [{ status: 'applied', applied: 1 }] });
      };
      t.server.insertRows(1, 4);
      t.sel.set(0, 0);
      t.app.changed();
      key(t, 'F2');
      const r = t.server.rebases[0];
      assert.deepEqual([r.col, r.text], [t.col, '=B5+1']);
      key(t, 'Escape');
      assert.equal(t.editor.ed, null);
      if (next === 'C1') {
        t.sel.set(0, 2);
        t.app.changed();
        key(t, 'F2');
        assert.equal(t.server.rebases.length, 2, `${what}: C1 asks for its own text`);
        const r2 = t.server.rebases[1];
        assert.deepEqual([r2.col, r2.text, r2.base, r2.at], [c1[1], '=D5+1', second.base, [0, 2]], what);
        type(t, '+1');
      }
      if (confirmed === 'before the late answer') confirm();
      t.server.socket.recv({ type: 'rebased', id: r.id, text: '=B6+1', at: [0, 0], base: r.through });
      assert.equal(t.editor.open, false, `${what}: the late answer opens no editor`);
      if (next === 'C1') {
        t.server.socket.recv({ type: 'rebased', id: t.server.rebases[1].id, text: '=D6+1', at: [0, 2], base: t.server.rebases[1].through });
        assert.equal(t.editor.text, '=D6+1+1', what);
        if (confirmed === 'after the answers') confirm();
        key(t, 'Enter');
        const a = t.server.socket.frames('action')[2];
        assert.deepEqual([a.ops[0].col, a.ops[0].text, a.ops[0].at], [c1[1], '=D6+1+1', [0, 2]], what);
      } else {
        if (confirmed === 'after the answers') confirm();
        assert.equal(t.editor.ed, null, what);
        assert.equal(t.server.socket.frames('action').length, 2, `${what}: nothing more is written`);
      }
      assert.deepEqual(notices(t), [], what);
    }
  }
});

test('a reference drag changes nothing once the editor is read-only for the last exchange', async () => {
  const t = await editing();
  t.grid.h.down({ zone: 'cell', r: 5, c: 1 }, { preventDefault() {} });
  assert.equal(t.editor.text, '=B5+B6');
  t.server.insertRows(1, 0);
  assert.equal(t.server.rebases[0].text, '=B5+B6');
  t.timers.advance(500);
  t.grid.h.drag({ zone: 'cell', r: 6, c: 1 }, {});
  assert.equal(t.editor.text, '=B5+B6:B7', 'the drag goes on while the editor is editable');
  t.answer(0);
  assert.equal(t.grid.input.readOnly, true);
  assert.equal(t.server.rebases[1].text, '=B5+B6:B7');
  t.grid.h.drag({ zone: 'cell', r: 8, c: 1 }, {});
  t.grid.h.drag({ zone: 'cell', r: 9, c: 1 }, {});
  t.grid.h.up({});
  assert.equal(t.editor.text, '=B5+B6:B7', 'pointer moves leave the text that waits for the last answer');
  assert.equal(t.app.bar.value, '=B5+B6:B7');
  t.answer(1);
  assert.equal(t.editor.text, '=B6+B7:B8');
  assert.equal(t.grid.input.readOnly, false);
  assert.equal(t.server.rebases.length, 2, 'one last round trip');
  t.grid.h.drag({ zone: 'cell', r: 2, c: 1 }, {});
  assert.equal(t.editor.text, '=B6+B7:B8', 'the drag ended with the read-only interval');
});

test('a function hint is not accepted while the editor is read-only for the last exchange', async () => {
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 30, cols: 6 }] });
  const t = await startApp({ server, functions: [{ name: 'SUM', group: 'Math', args: ['number1'] }] });
  server.manual.add('rebase');
  t.sel.set(2, 3);
  t.app.changed();
  type(t, '=S');
  server.insertRows(1, 0);
  t.timers.advance(500);
  type(t, 'U');
  server.socket.recv({ type: 'rebased', id: server.rebases[0].id, text: '=S', at: [3, 3], base: server.rebases[0].through });
  assert.equal(t.grid.input.readOnly, true);
  assert.equal(t.editor.text, '=SU');
  key(t, 'Tab');
  assert.equal(t.editor.text, '=SU', 'Tab leaves the text that waits for the last answer');
  const hint = t.app.root.querySelector('.fn');
  assert.ok(hint, 'the hint is on screen');
  fire(hint, 'mousedown');
  assert.equal(t.editor.text, '=SU', 'a press on the hint leaves it too');
  assert.equal(server.socket.frames('action').length, 0);
});

test('a reference drag ends when an exchange moves the screen its start was on', async () => {
  const t = await editing();
  t.grid.h.down({ zone: 'cell', r: 5, c: 1 }, { preventDefault() {} });
  t.server.insertRows(1, 0);
  assert.equal(t.server.rebases[0].text, '=B5+B6');
  t.answer(0);
  assert.equal(t.editor.text, '=B6+B7', 'text and grid moved together');
  t.grid.h.drag({ zone: 'cell', r: 8, c: 1 }, {});
  t.grid.h.up({});
  assert.equal(t.editor.text, '=B6+B7', 'a range from a start on the earlier screen is not inserted');
  assert.equal(t.editor.open, true);
});

test('a formula too long for one frame closes the editor when the grid changes under it, and the connection stays', async () => {
  const t = await editing();
  key(t, 'Escape');
  // An edit elsewhere is still on its way.
  t.server.manual.add('action');
  t.sel.set(0, 0);
  t.app.changed();
  type(t, 'held');
  key(t, 'Enter');
  t.sel.set(2, 3);
  t.app.changed();
  // 400,000 characters: more than a client frame, less than an action.
  const text = `=B5+${'1+'.repeat(200000)}1`;
  type(t, text);
  assert.equal(t.editor.text, text);
  t.server.insertRows(1, 0);
  assert.equal(t.server.rebases.length, 0, 'no rebase is sent');
  assert.equal(t.server.sockets.length, 1, 'the connection stays');
  assert.equal(t.net.open, true);
  assert.equal(t.editor.ed, null, 'the editor closed');
  assert.deepEqual(notices(t).at(-1), ['warn', 'The grid changed while you were typing a formula that is too long for the host to adjust, so the editor closed. Nothing was written. Your text:', text]);
  assert.equal(t.rows(), 31, 'the held delta applied');
  assert.equal(t.net.pending.length, 1, 'the earlier edit is still on its way');
  // The page takes the next edit.
  t.server.manual.delete('action');
  type(t, 'next');
  key(t, 'Enter');
  assert.equal(t.server.socket.frames('action').at(-1).ops[0].text, 'next');
});
