// A cell whose row of a snapshot has not arrived is not blank: its content
// may still be on its way (TECH 12.2). An edit in place of such a cell
// waits for its row, as one waits for a formula's text, and a copy of it
// gives no text. The grid is interactive from its first rows frame on.
'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { fire } from './dom.mjs';
import { FakeServer, key, notices, startApp, type } from './server.mjs';

// Every row of Data holds a cell, so a snapshot of two rows a frame sends
// rows 1 and 2, 3 and 4, and so on.
const cells = { '0,0': 'top', '7,1': 'prefix', '5,2': '=B8', '0,3': '1', '9,3': '2' };
for (let r = 1; r < 10; r++) cells[`${r},0`] = `r${r}`;
const sheet = () => new FakeServer({ tabs: [
  { id: 1, name: 'Data', rows: 10, cols: 4, cells },
  { id: 2, name: 'Other', rows: 5, cols: 3, cells: { '0,0': 'x' } },
] });
const actions = (t) => t.server.socket.frames('action');

// paced opens tab 1 again with its snapshot held in the connection's
// queue, two rows a frame, and lets the head through.
function paced(t) {
  const ws = t.server.socket;
  t.server.manual.add('watch');
  t.app.openTab(2);
  t.server.snapshot(ws, ws.frames('watch').at(-1).generation, 2);
  t.app.openTab(1);
  t.server.paced = true;
  t.server.snapshot(ws, ws.frames('watch').at(-1).generation, 1, 2);
  t.server.paced = false;
  const q = t.server.socket.queue;
  while (q[0].type === 'tab_head' || q[0].type === 'tab_more') t.server.deliver(1);
}

// rowsThrough lets the rows frames through until the one that holds a row.
function rowsThrough(t, r) {
  const row = t.server.tab(1).rows[r];
  const q = t.server.socket.queue;
  while (q.length) {
    const f = q[0];
    t.server.deliver(1);
    if (f.type === 'rows' && f.rows.some(([id]) => id === row)) return;
  }
}

const copy = (t) => {
  const data = {};
  fire(t.doc, 'copy', { clipboardData: { setData: (k, v) => { data[k] = v; }, getData: (k) => data[k] }, preventDefault() {} });
  return data['text/plain'] ?? null;
};

test('nothing is edited in a grid that has its head and no rows yet', async () => {
  const t = await startApp({ server: sheet() });
  paced(t);
  assert.deepEqual([t.model.tab.id, t.model.tab.loading, t.model.tab.started], [1, true, false]);
  t.sel.set(0, 0);
  key(t, 'F2');
  type(t, 'suffix');
  assert.equal(t.editor.ed, null, 'no editor opens on a grid with no rows');
  assert.match(notices(t).at(-1)[1], /The tab is still loading/);
  t.server.deliver(1);
  assert.equal(t.model.tab.started, true);
  key(t, 'F2');
  type(t, 'suffix');
  key(t, 'Enter');
  assert.equal(actions(t).at(-1).ops[0].text, 'topsuffix', 'the first rows bring the content the edit goes on from');
});

test('an edit in place of a cell whose row has not arrived waits for it, with the keys typed meanwhile after the content', async () => {
  const t = await startApp({ server: sheet() });
  paced(t);
  t.server.deliver(1); // rows 1 and 2
  const sent = actions(t).length;
  t.sel.set(7, 1);
  key(t, 'F2');
  assert.deepEqual([!!t.editor.ed, t.editor.open], [true, false], 'the editor waits');
  type(t, 'suffix');
  key(t, 'Enter');
  assert.equal(actions(t).length, sent, 'a waiting edit commits nothing');
  rowsThrough(t, 7);
  assert.equal(t.editor.open, false, 'row 8 is the last of its frame and may go on in the next');
  rowsThrough(t, 8);
  assert.equal(t.editor.open, true);
  assert.equal(t.editor.text, 'prefixsuffix');
  key(t, 'Enter');
  assert.equal(actions(t).at(-1).ops[0].text, 'prefixsuffix');
});

test('a formula whose row arrives is asked for, and its text opens the editor', async () => {
  const t = await startApp({ server: sheet() });
  paced(t);
  t.server.deliver(1);
  t.sel.set(5, 2);
  key(t, 'F2');
  type(t, '+1');
  assert.equal(t.server.socket.frames('peek').length, 0, 'a cell that has not arrived is no formula to ask for');
  t.server.deliver();
  assert.equal(t.editor.text, '=B8+1');
  assert.equal(t.server.socket.frames('peek').length, 1);
});

test('the end of the snapshot opens an editor that waited on the last row', async () => {
  const t = await startApp({ server: sheet() });
  paced(t);
  t.server.deliver(1);
  t.sel.set(9, 3);
  key(t, 'F2');
  type(t, 'new');
  assert.equal(t.editor.open, false);
  t.server.deliver();
  assert.deepEqual([t.model.tab.loading, t.editor.open, t.editor.text], [false, true, '2new']);
});

test('a cell a delta changed during the transfer is known at once', async () => {
  const t = await startApp({ server: sheet() });
  paced(t);
  t.server.deliver(1);
  // The delta waits behind the snapshot frames in the queue, so it is sent
  // to the client by hand, as one that arrives between two of them.
  const d = t.server.setCell(1, 7, 1, 'fresh');
  t.server.socket.queue.pop();
  t.server.socket.recv({ type: 'delta', seq: d.seq, cells: d.cells });
  t.sel.set(7, 1);
  key(t, 'F2');
  assert.equal(t.editor.text, 'fresh');
});

test('a row that goes on in the next frame is whole only once a later row or the end arrives', async () => {
  const t = await startApp({ server: sheet() });
  paced(t);
  const tab = t.server.tab(1);
  const [r0, r1] = tab.rows;
  const [a, , c] = tab.cols;
  // The queued frames are replaced by a row that takes two frames.
  t.server.socket.queue.length = 0;
  const generation = t.server.socket.frames('watch').at(-1).generation;
  const rows = (part, list) => t.server.socket.recv({ type: 'rows', generation, tab: 1, part, rows: list });
  rows(0, [[r0, [[a, 't', 'left']]]]);
  t.sel.set(0, 2);
  key(t, 'F2');
  type(t, '!');
  assert.deepEqual([t.model.known(r0, a), t.model.known(r0, c), t.editor.open], [false, false, false]);
  rows(1, [[r0, [[c, 't', 'right']]]]);
  assert.equal(t.editor.open, false, 'the row may still go on');
  rows(2, [[r1, [[a, 't', 'below']]]]);
  assert.deepEqual([t.model.known(r0, c), t.editor.open, t.editor.text], [true, true, 'right!']);
  // A copy needs every cell of the selection.
  key(t, 'Escape');
  t.sel.range(0, 0, 0, 2);
  assert.equal(copy(t), 'left\t\tright', 'rows above the last arrived are whole');
  t.sel.range(1, 0, 1, 0);
  assert.equal(copy(t), null, 'the last row of a frame is not');
});

test('a copy of cells that have not arrived gives no text, and one of all of them does', async () => {
  const t = await startApp({ server: sheet() });
  paced(t);
  t.server.deliver(1);
  t.sel.range(0, 0, 0, 0);
  assert.equal(copy(t), 'top');
  t.sel.range(1, 0, 1, 0);
  assert.equal(copy(t), null, 'the last row of the frame may go on');
  t.sel.range(6, 0, 7, 3);
  assert.equal(copy(t), null);
  assert.match(notices(t).at(-1)[1], /still loading. Nothing was copied/);
  fire(t.doc, 'cut', { clipboardData: { setData: () => assert.fail('a cut set text') }, preventDefault: () => assert.fail('a cut took the event') });
  assert.match(notices(t).at(-1)[1], /Nothing was cut/);
  t.sel.cols(0, 0);
  t.server.deliver(3);
  assert.equal(t.model.tab.loading, true);
  assert.equal(copy(t), null, 'a whole column while the tab loads');
  t.server.deliver();
  assert.equal(copy(t), 'top\nr1\nr2\nr3\nr4\nr5\nr6\nr7\nr8\nr9');
  t.sel.range(7, 1, 7, 1);
  assert.equal(copy(t), 'prefix');
});

test('keys typed in the formula bar on a cell whose content is on its way join that content', async () => {
  const t = await startApp({ server: sheet() });
  paced(t);
  t.server.deliver(1);
  t.sel.set(7, 1);
  t.app.changed();
  assert.equal(t.app.bar.value, '');
  t.app.bar.focus();
  t.app.bar.value = 'X';
  fire(t.app.bar, 'input');
  assert.deepEqual([!!t.editor.ed, t.editor.open], [true, false], 'the bar waits for the content');
  t.server.deliver();
  assert.equal(t.editor.text, 'prefixX');
  key(t, 'Enter');
  assert.equal(actions(t).at(-1).ops[0].text, 'prefixX');
});

test('keys typed in the formula bar on a formula whose text is on its way follow that text', async () => {
  const t = await startApp({ server: sheet() });
  t.server.manual.add('peek');
  t.sel.set(5, 2);
  t.app.changed();
  t.timers.advance(100);
  assert.equal(t.app.bar.value, '');
  t.app.bar.focus();
  t.app.bar.value = '+1';
  fire(t.app.bar, 'input');
  assert.equal(t.editor.open, false);
  const peek = t.server.socket.frames('peek').at(-1);
  t.server.socket.recv({ type: 'peeked', id: peek.id, seq: String(t.server.seq), edit: '=B8', formula: true });
  assert.equal(t.editor.text, '=B8+1');
});

test('a composition in the formula bar joins the content it shows, or the content on its way', async () => {
  const t = await startApp({ server: sheet() });
  t.sel.set(1, 0);
  t.app.changed();
  assert.equal(t.app.bar.value, 'r1');
  t.app.bar.focus();
  fire(t.app.bar, 'compositionstart');
  assert.equal(t.editor.text, 'r1', 'the content stays');
  t.app.bar.value = 'r1한';
  fire(t.app.bar, 'compositionend');
  key(t, 'Enter');
  assert.equal(actions(t).at(-1).ops[0].text, 'r1한');
  paced(t);
  t.server.deliver(1);
  t.sel.set(7, 1);
  t.app.changed();
  t.app.bar.focus();
  fire(t.app.bar, 'compositionstart');
  t.app.bar.value = '한';
  fire(t.app.bar, 'compositionend');
  assert.equal(t.editor.open, false);
  t.server.deliver();
  assert.equal(t.editor.text, 'prefix한');
});

test('nothing is built from the format or the cells of a part that has not arrived', async () => {
  const t = await startApp({ server: sheet() });
  paced(t);
  t.server.deliver(1);
  const bold = () => t.app.toolbar.el.querySelectorAll('button').find((b) => b.getAttribute('aria-label') === 'Bold (Ctrl or Cmd+B)');
  t.sel.set(7, 1);
  t.app.changed();
  assert.equal(bold().disabled, true, 'a toggle would start from a format that is not known');
  const sent = actions(t).length;
  key(t, 'b', { ctrlKey: true });
  assert.equal(actions(t).length, sent);
  assert.match(notices(t).at(-1)[1], /its format is not known yet/);
  t.app.fitColumn(1);
  assert.equal(actions(t).length, sent, 'a column is measured only once every cell of it arrived');
  t.sel.range(0, 3, 9, 3);
  t.app.changed();
  assert.equal(t.app.status.agg.children.length, 0, 'no sum of cells that have not all arrived');
  t.server.deliver();
  t.app.changed();
  assert.equal(t.app.status.agg.children.length > 0, true);
  t.sel.set(7, 1);
  t.app.changed();
  assert.equal(bold().disabled, false);
  key(t, 'b', { ctrlKey: true });
  assert.deepEqual(actions(t).at(-1).ops[0].set, { bold: true });
  t.app.fitColumn(1);
  assert.equal(actions(t).at(-1).ops[0].op, 'width');
});

// stored is the text the server holds in a cell of tab 1.
const stored = (t, r, c) => {
  const tab = t.server.tab(1);
  const cell = tab.cells.get(`${tab.rows[r]}:${tab.cols[c]}`);
  return cell ? cell.formula ?? String(cell.value) : '';
};

// barEdit reaches the formula bar a way a person can, on a cell of a row
// that is the last of its frame: the cell arrived, so the bar shows it, but
// the row may go on in the next frame.
async function barEdit(how) {
  const t = await startApp({ server: sheet() });
  paced(t);
  rowsThrough(t, 7);
  t.sel.set(7, 1);
  t.app.changed();
  assert.equal(t.model.known(t.model.rowAt(7), t.model.colAt(1)), false, 'the row may go on');
  assert.equal(t.app.bar.value, 'prefix', 'the bar shows the cell that arrived');
  const bar = t.app.bar;
  const end = () => { bar.selectionStart = bar.selectionEnd = bar.value.length; };
  if (how === 'keyboard') {
    key(t, 'F6', { shiftKey: true });
    assert.equal(t.doc.activeElement, bar);
    end();
    bar.value += 'suffix';
    fire(bar, 'input');
  } else if (how === 'pointer') {
    fire(bar, 'mousedown', { button: 0 });
    bar.focus();
    assert.equal(bar.value, 'prefix', 'the bar keeps what it shows');
    end();
    type(t, 'suffix');
  } else if (how === 'F2') {
    key(t, 'F6', { shiftKey: true });
    key(t, 'F2');
    end();
    type(t, 'suffix');
  } else {
    key(t, 'F6', { shiftKey: true });
    fire(bar, 'compositionstart');
    assert.equal(bar.value, 'prefix');
    bar.value = 'prefix한';
    fire(bar, 'compositionend');
  }
  return t;
}

for (const [how, want] of [['keyboard', 'prefixsuffix'], ['pointer', 'prefixsuffix'], ['F2', 'prefixsuffix'], ['composition', 'prefix한']]) {
  test(`the formula bar's text of a cell whose row goes on is the edit's text once (${how})`, async () => {
    const t = await barEdit(how);
    assert.deepEqual([!!t.editor.ed, t.editor.open], [true, false], 'the edit waits for the row');
    assert.equal(t.app.bar.value, want);
    const sent = actions(t).length;
    key(t, 'Enter');
    assert.equal(actions(t).length, sent, 'a waiting edit commits nothing');
    t.server.deliver();
    assert.deepEqual([t.editor.open, t.editor.text], [true, want]);
    key(t, 'Enter');
    assert.equal(actions(t).at(-1).ops[0].text, want);
    assert.equal(stored(t, 7, 1), want);
  });
}

test('a formula text the bar shows on a row that goes on is the edit\'s text once', async () => {
  const t = await startApp({ server: sheet() });
  paced(t);
  rowsThrough(t, 5);
  t.sel.set(5, 2);
  t.app.changed();
  t.timers.advance(100);
  assert.equal(t.app.bar.value, '=B8', 'the peeked formula text');
  key(t, 'F6', { shiftKey: true });
  t.app.bar.value += '+1';
  fire(t.app.bar, 'input');
  // A rename drops the formula text, so the row's arrival asks for it again.
  t.server.renameTab(2, 'Renamed');
  t.server.deliver();
  assert.equal(t.editor.text, '=B8+1');
  key(t, 'Enter');
  assert.equal(stored(t, 5, 2), '=B8+1');
});

test('keys typed into a bar that has not shown a cell arrived since join its content', async () => {
  const frames = [];
  const t = await startApp({ server: sheet(), raf: (fn) => frames.push(fn) });
  const draw = () => { while (frames.length) frames.shift()(); };
  paced(t);
  t.server.deliver(1);
  t.sel.set(7, 1);
  t.app.changed();
  draw();
  assert.equal(t.app.bar.value, '');
  // The row arrives whole, and the screen is not drawn before the key.
  rowsThrough(t, 8);
  assert.equal(t.model.known(t.model.rowAt(7), t.model.colAt(1)), true);
  key(t, 'F6', { shiftKey: true });
  t.app.bar.value = 'X';
  fire(t.app.bar, 'input');
  assert.equal(t.editor.text, 'prefixX');
  key(t, 'Enter');
  assert.equal(stored(t, 7, 1), 'prefixX');
});

for (const how of ['keyboard', 'pointer']) {
  test(`a bar edit after the selection moved and before the bar was drawn starts on the active cell's text (${how})`, async () => {
    const frames = [];
    const t = await startApp({ server: sheet(), raf: (fn) => frames.push(fn) });
    const draw = () => { while (frames.length) frames.shift()(); };
    t.sel.set(0, 0);
    t.app.changed();
    draw();
    assert.equal(t.app.bar.value, 'top');
    const bar = t.app.bar;
    if (how === 'keyboard') {
      key(t, 'ArrowDown');
      key(t, 'F6', { shiftKey: true });
      assert.equal(t.doc.activeElement, bar);
      key(t, 'F2');
    } else {
      const ev = { preventDefault() {}, clientX: 0, clientY: 0, button: 0 };
      t.grid.h.down({ zone: 'cell', r: 1, c: 0 }, ev);
      t.grid.h.up({});
      fire(bar, 'mousedown', { button: 0 });
      bar.focus();
    }
    assert.deepEqual(t.sel.active(), { r: 1, c: 0 });
    assert.equal(bar.value, 'r1', 'the bar holds the active cell\'s text');
    bar.value += 'X';
    bar.selectionStart = bar.selectionEnd = bar.value.length;
    fire(bar, 'input');
    assert.equal(t.editor.text, 'r1X');
    key(t, 'Enter');
    draw();
    assert.equal(stored(t, 1, 0), 'r1X');
    assert.equal(stored(t, 0, 0), 'top');
  });
}

// Keys typed before the bar showed the active cell were typed into the text
// of the cell the selection left. No key of them goes into the active cell.
test('keys typed into a bar that still shows the cell the selection left are offered back, and neither cell is written', async () => {
  const frames = [];
  const t = await startApp({ server: sheet(), raf: (fn) => frames.push(fn) });
  const draw = () => { while (frames.length) frames.shift()(); };
  t.sel.set(0, 0);
  t.app.changed();
  draw();
  key(t, 'ArrowDown');
  key(t, 'F6', { shiftKey: true });
  const bar = t.app.bar;
  bar.value += 'X';
  bar.selectionStart = bar.selectionEnd = bar.value.length;
  fire(bar, 'input');
  assert.equal(t.editor.ed, null);
  assert.equal(notices(t).at(-1)[2], 'topX');
  assert.equal(bar.value, 'r1');
  key(t, 'Enter');
  key(t, 'Enter');
  draw();
  assert.deepEqual([stored(t, 0, 0), stored(t, 1, 0)], ['top', 'r1']);
  assert.equal(actions(t).length, 0);
});
