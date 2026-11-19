'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { decodeTSV, encodeTSV, measureTSV, targets } from '../clipboard.js';
import { ACTION_MAX } from '../protocol.js';
import { dragTarget, keyFill } from '../fill.js';
import { fire } from './dom.mjs';
import { FakeServer, key, notices, settle, shownAt, startApp, type } from './server.mjs';

const sheet = () => new FakeServer({ tabs: [
  { id: 1, name: 'Budget', rows: 6, cols: 4, cells: { '0,0': 'Item', '0,1': 'Amount', '1,0': 'Venue', '1,1': { kind: 'n', value: 1200, shown: '$1,200.00', edit: '1200' }, '2,0': 'Food', '2,1': '=B2*2' } },
  { id: 2, name: 'Notes', rows: 5, cols: 3 },
] });
const clip = (text = '') => { const data = { 'text/plain': text }; return { clipboardData: { setData: (k, v) => { data[k] = v; }, getData: (k) => data[k] }, data }; };
const lastOps = (server) => server.socket.frames('action').at(-1).ops;

test('tab-separated text quotes fields that hold tabs, line breaks or quotation marks', () => {
  assert.equal(encodeTSV([['a', 'b'], ['1', '2']]), 'a\tb\n1\t2');
  assert.equal(encodeTSV([['two\nlines', 'say "hi"', 'tab\there', '']]), '"two\nlines"\t"say ""hi"""\t"tab\there"\t');
  assert.deepEqual(decodeTSV('a\tb\n1\t2'), [['a', 'b'], ['1', '2']]);
  assert.deepEqual(decodeTSV('a\tb\r\n1\t2\r\n'), [['a', 'b'], ['1', '2']], 'a trailing line break adds no row');
  assert.deepEqual(decodeTSV('"two\nlines"\t"say ""hi"""\t"tab\there"\t'), [['two\nlines', 'say "hi"', 'tab\there', '']]);
  assert.deepEqual(decodeTSV('a\n\nb'), [['a'], [''], ['b']], 'a blank row inside stays');
  assert.deepEqual(decodeTSV('a\tb\tc\nd'), [['a', 'b', 'c'], ['d', '', '']], 'short rows are padded to a rectangle');
  assert.deepEqual(decodeTSV('x'), [['x']]);
  const rows = [['=SUM(A1)', "'007", '한글'], ['', '"', '\t']];
  assert.deepEqual(decodeTSV(encodeTSV(rows)), rows);
});

test('targets name ids, and rows or columns the write adds past the end', () => {
  assert.deepEqual(targets([10, 11, 12], 1, 2), [11, 12]);
  assert.deepEqual(targets([10, 11, 12], 2, 3), [12, { new: 1 }, { new: 2 }]);
});

test('copy writes the shown text of the selection to the system clipboard', async () => {
  const t = await startApp({ server: sheet() });
  t.sel.range(0, 0, 2, 1);
  const e = clip();
  assert.equal(fire(t.doc, 'copy', e).defaultPrevented, true);
  assert.equal(e.data['text/plain'], 'Item\tAmount\nVenue\t$1,200.00\nFood\t0', 'shown values, as another program reads them');
  assert.match(t.doc.querySelector('#announce').textContent, /Copied A1:B3\./);
  assert.equal(t.server.socket.frames('action').length, 0, 'copying sends nothing');
  // a whole column copies up to the last filled cell
  t.sel.cols(1, 1);
  const col = clip();
  fire(t.doc, 'copy', col);
  assert.equal(col.data['text/plain'], 'Amount\n$1,200.00\n0');
});

test('a paste of what this tab copied sends copy, so formulas and formats travel', async () => {
  const t = await startApp({ server: sheet() });
  const m = t.model;
  t.sel.range(1, 0, 2, 1);
  const e = clip();
  fire(t.doc, 'copy', e);
  t.sel.set(4, 2);
  fire(t.doc, 'paste', clip(e.data['text/plain']));
  assert.deepEqual(lastOps(t.server), [{
    op: 'copy', mode: 'all', cut: false,
    from: { tab: 1, rows: [[m.rowAt(1), 2]], cols: [[m.colAt(0), 2]] },
    to: { tab: 1, rows: [m.rowAt(4), m.rowAt(5)], cols: [m.colAt(2), m.colAt(3)] },
  }]);
  // pasting again repeats the copy; a paste past the end names the rows it adds
  t.sel.set(5, 3);
  fire(t.doc, 'paste', clip(e.data['text/plain']));
  assert.deepEqual(lastOps(t.server)[0].to, { tab: 1, rows: [m.rowAt(5), { new: 1 }], cols: [m.colAt(3), { new: 1 }] });
});

test('one copied cell pasted over a range is one copy over the whole range', async () => {
  // The host repeats the source over the larger target and takes every
  // cell from the source (rule 15): a fill from the first pasted cell
  // would pass on what that cell became, or a cell that was there when
  // the source is gone.
  const t = await startApp({ server: sheet() });
  const m = t.model;
  t.sel.set(2, 1);
  const e = clip();
  fire(t.doc, 'copy', e);
  t.sel.range(3, 1, 5, 2);
  fire(t.doc, 'paste', clip(e.data['text/plain']));
  assert.deepEqual(lastOps(t.server), [{
    op: 'copy', from: { tab: 1, rows: [[m.rowAt(2), 1]], cols: [[m.colAt(1), 1]] },
    to: { tab: 1, rows: [[m.rowAt(3), 3]], cols: [[m.colAt(1), 2]] }, mode: 'all', cut: false,
  }]);
});

test('a copy whose shown text is empty is pasted as a copy, and empty text from elsewhere writes nothing', async () => {
  const server = sheet();
  server.tab(1).cells.set(`${server.tab(1).rows[4]}:${server.tab(1).cols[0]}`, { kind: 'tf', value: '', formula: '=""' });
  server.tab(1).cells.set(`${server.tab(1).rows[5]}:${server.tab(1).cols[0]}`, { kind: '', style: 1 });
  server.styles.push({ fill: 'green-100' });
  const t = await startApp({ server });
  const m = t.model;
  t.server.manual.add('action');
  for (const [r, cut] of [[4, false], [5, false], [4, true]]) {
    t.sel.set(r, 0);
    const e = clip('unchanged');
    fire(t.doc, cut ? 'cut' : 'copy', e);
    assert.equal(e.data['text/plain'], '', `row ${r + 1} shows nothing`);
    t.sel.set(1, 1);
    fire(t.doc, 'paste', clip(''));
    assert.deepEqual(lastOps(t.server), [{
      op: 'copy', from: { tab: 1, rows: [[m.rowAt(r), 1]], cols: [[m.colAt(0), 1]] },
      to: { tab: 1, rows: [m.rowAt(1)], cols: [m.colAt(1)] }, mode: 'all', cut,
    }]);
  }
  // As values over a range, it is one copy of values over the range.
  t.sel.set(4, 0);
  fire(t.doc, 'copy', clip());
  t.sel.range(0, 2, 2, 3);
  key(t, 'v', { ctrlKey: true, shiftKey: true });
  fire(t.doc, 'paste', clip(''));
  assert.equal(lastOps(t.server)[0].mode, 'values');
  assert.deepEqual(lastOps(t.server)[0].to, { tab: 1, rows: [[m.rowAt(0), 3]], cols: [[m.colAt(2), 2]] });
  // A copy of another range is no copy of an empty cell.
  const sent = t.server.socket.frames('action').length;
  t.sel.set(0, 0);
  fire(t.doc, 'copy', clip());
  fire(t.doc, 'paste', clip(''));
  assert.equal(t.server.socket.frames('action').length, sent);
});

test('paste values only sends copy with mode values', async () => {
  const t = await startApp({ server: sheet() });
  t.sel.set(2, 1);
  const e = clip();
  fire(t.doc, 'copy', e);
  t.sel.set(4, 1);
  // Ctrl or Cmd+Shift+V: the key marks the paste event that follows
  assert.equal(key(t, 'v', { ctrlKey: true, shiftKey: true }).defaultPrevented, false, 'the browser still fires the paste');
  fire(t.doc, 'paste', clip(e.data['text/plain']));
  assert.equal(lastOps(t.server)[0].mode, 'values');
  // the mark does not outlive the gesture
  key(t, 'v', { metaKey: true, shiftKey: true });
  t.timers.advance(600);
  fire(t.doc, 'paste', clip(e.data['text/plain']));
  assert.equal(lastOps(t.server)[0].mode, 'all');
});

test('one copied cell pasted as values over a range is one copy over the whole range, with no fill', async () => {
  // A fill copies formats as well, so values only goes without one: the
  // host repeats the source over the larger target itself (rule 15).
  const t = await startApp({ server: sheet() });
  const m = t.model;
  t.sel.set(2, 1);
  const e = clip();
  fire(t.doc, 'copy', e);
  t.sel.range(3, 1, 5, 2);
  key(t, 'v', { ctrlKey: true, shiftKey: true });
  fire(t.doc, 'paste', clip(e.data['text/plain']));
  assert.deepEqual(lastOps(t.server), [{
    op: 'copy', from: { tab: 1, rows: [[m.rowAt(2), 1]], cols: [[m.colAt(1), 1]] },
    to: { tab: 1, rows: [[m.rowAt(3), 3]], cols: [[m.colAt(1), 2]] }, mode: 'values', cut: false,
  }]);
  // A copy of several cells goes once, as for a paste of everything.
  t.sel.range(1, 0, 2, 1);
  fire(t.doc, 'copy', e);
  t.sel.range(3, 2, 5, 3);
  key(t, 'v', { ctrlKey: true, shiftKey: true });
  fire(t.doc, 'paste', clip(e.data['text/plain']));
  assert.deepEqual(lastOps(t.server), [{
    op: 'copy', from: { tab: 1, rows: [[m.rowAt(1), 2]], cols: [[m.colAt(0), 2]] },
    to: { tab: 1, rows: [m.rowAt(3), m.rowAt(4)], cols: [m.colAt(2), m.colAt(3)] }, mode: 'values', cut: false,
  }]);
});

test('cut moves once: the paste carries cut, and a second paste is plain text', async () => {
  const t = await startApp({ server: sheet() });
  t.sel.set(1, 0);
  const e = clip();
  fire(t.doc, 'cut', e);
  assert.equal(e.data['text/plain'], 'Venue');
  assert.equal(t.server.socket.frames('action').length, 0, 'the cells leave when they are pasted');
  t.sel.set(4, 0);
  fire(t.doc, 'paste', clip('Venue'));
  assert.equal(lastOps(t.server)[0].cut, true);
  t.sel.set(5, 0);
  fire(t.doc, 'paste', clip('Venue'));
  assert.equal(lastOps(t.server)[0].op, 'write');
});

test('a cut whose paste moved nothing can be pasted again; one that moved a part cannot', async () => {
  const answers = {
    'every destination deleted': [{ status: 'applied', ignored: 1 }, true, /^Your paste moved 0 of its 1 cell\. Rows or columns were deleted before the host received it, so the other 1 did not move\. A cut cell that did not move keeps its content, unless another edit deleted its own row or column and the cell with it\. The text:$/],
    'the destination tab deleted': [{ status: 'ignored', reason: 'gone' }, true, /was ignored/],
    'the paste refused': [{ status: 'refused', reason: 'formula_length' }, true, /refused/],
    'a part moved': [{ status: 'applied', applied: 1, ignored: 1 }, false, /^Your paste moved 1 of its 2 cells\. Rows or columns were deleted before the host received it, so the other 1 did not move\. A cut cell that did not move keeps its content, unless another edit deleted its own row or column and the cell with it\. The text:$/],
  };
  for (const [name, [answer, again, notice]] of Object.entries(answers)) {
    const t = await startApp({ server: sheet() });
    t.sel.range(1, 0, 2, 0);
    const e = clip();
    fire(t.doc, 'cut', e);
    t.sel.set(4, 2);
    t.server.refuse = answer;
    fire(t.doc, 'paste', clip(e.data['text/plain']));
    assert.equal(lastOps(t.server)[0].cut, true, name);
    assert.match(notices(t).at(-1)[1], notice, name);
    t.sel.set(4, 3);
    fire(t.doc, 'paste', clip(e.data['text/plain']));
    const ops = lastOps(t.server);
    if (again) {
      assert.equal(ops[0].op, 'copy', `${name}: the cut cells are where they were and move with the next paste`);
      assert.equal(ops[0].cut, true, name);
    } else {
      assert.equal(ops[0].op, 'write', `${name}: the cells that moved are gone from their place, so the next paste is text`);
    }
  }
  // A copy made while the paste was on its way is the one a later paste uses.
  const t = await startApp({ server: sheet() });
  t.server.manual.add('action');
  t.sel.set(1, 0);
  const cut = clip();
  fire(t.doc, 'cut', cut);
  t.sel.set(4, 2);
  fire(t.doc, 'paste', clip('Venue'));
  const a = t.server.socket.frames('action').at(-1);
  t.sel.set(2, 0);
  const copy = clip();
  fire(t.doc, 'copy', copy);
  t.server.socket.recv({ type: 'ack', n: a.n, seq: '10', results: [{ status: 'applied', ignored: 1 }] });
  t.sel.set(4, 3);
  fire(t.doc, 'paste', clip(copy.data['text/plain']));
  assert.deepEqual(lastOps(t.server)[0].from.rows, [[t.model.rowAt(2), 1]]);
  assert.equal(lastOps(t.server)[0].cut, false);
});

test('a paste from another program sends the typed strings for the server to read', async () => {
  const t = await startApp({ server: sheet() });
  const m = t.model;
  t.server.manual.add('action');
  t.sel.set(4, 2);
  fire(t.doc, 'paste', clip('1,200.50\t=A1\n$5\t"multi\nline"\n3/15/2026\tTRUE\n'));
  assert.deepEqual(lastOps(t.server), [{
    op: 'write', tab: 1, rows: [m.rowAt(4), m.rowAt(5), { new: 1 }], cols: [m.colAt(2), m.colAt(3)],
    values: [['1,200.50', '=A1'], ['$5', 'multi\nline'], ['3/15/2026', 'TRUE']],
  }], 'the client reads none of it: no number, date or formula is guessed');
  assert.equal(shownAt(t, 4, 2), '1,200.50', 'the pasted text shows as pending');
  assert.equal(shownAt(t, 5, 2), '$5');
  // one value pasted over a range fills the range
  t.sel.range(0, 2, 1, 3);
  fire(t.doc, 'paste', clip('x'));
  assert.deepEqual(lastOps(t.server)[0].values, [['x', 'x'], ['x', 'x']]);
});

test('copy while editing is left to the text area, and paste is refused where editing is off', async () => {
  const t = await startApp({ server: sheet() });
  t.grid.input.value = 'ab';
  fire(t.grid.input, 'input');
  assert.equal(fire(t.doc, 'copy', clip()).defaultPrevented, false);
  assert.equal(fire(t.doc, 'paste', clip('x')).defaultPrevented, false);
  key(t, 'Escape');
  t.server.socket.recv({ type: 'status', calculating: true });
  fire(t.doc, 'paste', clip('x'));
  assert.equal(t.server.socket.frames('action').length, 0);
  assert.match(notices(t)[0][1], /calculating/);
});

test('the fill handle names the source and the cells the drag adds', async () => {
  const sel = { r1: 2, c1: 1, r2: 3, c2: 2 };
  assert.equal(dragTarget(sel, 3, 2), null, 'inside the selection nothing is added');
  assert.deepEqual(dragTarget(sel, 6, 2), { r1: 4, r2: 6, c1: 1, c2: 2 }, 'down');
  assert.deepEqual(dragTarget(sel, 0, 1), { r1: 0, r2: 1, c1: 1, c2: 2 }, 'up');
  assert.deepEqual(dragTarget(sel, 3, 5), { r1: 2, r2: 3, c1: 3, c2: 5 }, 'right');
  assert.deepEqual(dragTarget(sel, 2, 0), { r1: 2, r2: 3, c1: 0, c2: 0 }, 'left');
  assert.deepEqual(dragTarget(sel, 9, 4), { r1: 4, r2: 9, c1: 1, c2: 2 }, 'the axis the pointer left by more');

  const t = await startApp({ server: sheet() });
  const m = t.model;
  t.sel.set(2, 1);
  const ev = { preventDefault() {} };
  t.grid.h.down({ zone: 'handle', r: 2, c: 1 }, ev);
  t.grid.h.drag({ zone: 'cell', r: 5, c: 1 }, ev);
  assert.deepEqual(t.grid.overlays.fill, { r1: 3, r2: 5, c1: 1, c2: 1 }, 'the target is previewed');
  assert.equal(t.server.socket.frames('action').length, 0);
  t.grid.h.up(ev);
  // A drag fills by rule 21a: mode auto.
  assert.deepEqual(lastOps(t.server), [{ op: 'fill', tab: 1, source: { rows: [[m.rowAt(2), 1]], cols: [[m.colAt(1), 1]] }, target: { rows: [[m.rowAt(3), 3]], cols: [[m.colAt(1), 1]] }, mode: 'auto' }]);
  assert.deepEqual(t.sel.box(), { r1: 2, c1: 1, r2: 5, c2: 1 }, 'the filled range is selected');
  assert.equal(t.grid.overlays.fill, null);
});

test('Ctrl+D and Ctrl+R fill the selection from its first row or column', async () => {
  assert.deepEqual(keyFill({ r1: 2, c1: 1, r2: 5, c2: 2 }, 'down'), { source: { r1: 2, c1: 1, r2: 2, c2: 2 }, target: { r1: 3, c1: 1, r2: 5, c2: 2 } });
  assert.deepEqual(keyFill({ r1: 2, c1: 1, r2: 5, c2: 2 }, 'right'), { source: { r1: 2, c1: 1, r2: 5, c2: 1 }, target: { r1: 2, c1: 2, r2: 5, c2: 2 } });
  assert.deepEqual(keyFill({ r1: 2, c1: 1, r2: 2, c2: 1 }, 'down'), { source: { r1: 1, c1: 1, r2: 1, c2: 1 }, target: { r1: 2, c1: 1, r2: 2, c2: 1 } }, 'one row is filled from the row above');
  assert.equal(keyFill({ r1: 0, c1: 0, r2: 0, c2: 0 }, 'down'), null);
  assert.equal(keyFill({ r1: 0, c1: 0, r2: 0, c2: 0 }, 'right'), null);

  const t = await startApp({ server: sheet() });
  const m = t.model;
  t.sel.range(2, 1, 4, 1);
  assert.equal(key(t, 'd', { ctrlKey: true }).defaultPrevented, true);
  assert.deepEqual(lastOps(t.server)[0].target, { rows: [[m.rowAt(3), 2]], cols: [[m.colAt(1), 1]] });
  t.sel.range(1, 0, 1, 3);
  key(t, 'r', { metaKey: true });
  assert.deepEqual(lastOps(t.server)[0], { op: 'fill', tab: 1, source: { rows: [[m.rowAt(1), 1]], cols: [[m.colAt(0), 1]] }, target: { rows: [[m.rowAt(1), 1]], cols: [[m.colAt(1), 3]] } });
  // Ctrl+D and Ctrl+R copy: their fill names no mode, which is a copy,
  // and leaves no button to change it.
  for (const a of t.server.socket.frames('action')) assert.equal(Object.hasOwn(a.ops[0], 'mode'), false);
  assert.equal(t.doc.querySelector('.fillopts').hidden, true);
});

// built counts the array entries added while fn runs, and ends fn once
// they pass max. A paste that builds its rectangle first would add
// billions and never return.
function built(max, fn) {
  const { push, fill } = Array.prototype;
  const { from } = Array;
  let n = 0;
  const add = (k) => { n += k; if (n > max) throw new Error(`built more than ${max} array entries`); };
  Array.prototype.push = function (...items) { add(items.length); return push.apply(this, items); };
  Array.prototype.fill = function (...args) { add(this.length); return fill.apply(this, args); };
  Array.from = function (...args) { const out = from.apply(this, args); add(out.length); return out; };
  try {
    fn();
  } finally {
    Object.assign(Array.prototype, { push, fill });
    Array.from = from;
  }
  return n;
}

// Texts of 6 to 300 kilobytes whose widest row times their rows is more
// cells than an action can hold.
const ragged = {
  'one wide row, then many short ones': '\t'.repeat(99999) + '\n' + 'x\n'.repeat(100000),
  'empty rows after a wide one': '\t'.repeat(2999) + '\n'.repeat(3000),
  'the wide row last': 'x\n'.repeat(5000) + '\t'.repeat(5000),
  'quoted fields that hold tabs and line breaks': '"q\t"\t'.repeat(3000) + '\n' + '"two\nlines"\r\n'.repeat(3000),
};

test('a paste whose rows differ so much that its rectangle passes an action is refused before the rectangle is built', async () => {
  const t = await startApp({ server: sheet() });
  // An edit elsewhere is still on its way.
  t.server.manual.add('action');
  type(t, 'held');
  key(t, 'Enter');
  assert.equal(t.model.pending.size, 1);
  t.sel.set(4, 2);
  t.app.changed();
  const sent = () => t.server.socket.sent.filter((m) => m.type === 'action' || m.type === 'action_part').length;
  for (const [name, text] of Object.entries(ragged)) {
    const n = built(1000, () => fire(t.doc, 'paste', clip(text)));
    assert.ok(n < 1000, `${name}: ${n} array entries`);
    assert.equal(sent(), 1, `${name}: nothing was sent`);
    const [kind, said, kept] = notices(t).at(-1);
    assert.equal(kind, 'bad', name);
    assert.equal(said, 'This paste holds more cells or text than one edit can carry. Nothing was changed; paste it in smaller parts. The text:', name);
    assert.equal(kept, text, `${name}: the text is offered back`);
    assert.equal(t.net.stopped, null, `${name}: editing goes on`);
    assert.equal(t.net.pending.length, 1, `${name}: the earlier edit is still on its way`);
    assert.equal(t.model.pending.size, 1, name);
    assert.equal(shownAt(t, 0, 0), 'held', name);
    assert.equal(shownAt(t, 4, 2), '', `${name}: no pasted text shows`);
  }
  // The page takes the next paste.
  fire(t.doc, 'paste', clip('a\tb\tc\nd'));
  assert.deepEqual(lastOps(t.server)[0].values, [['a', 'b', 'c'], ['d', '', '']]);
  assert.equal(t.net.pending.length, 2);
});

test('a refused paste of values whose text arrives later leaves an open editor as it is', async () => {
  const t = await startApp({ server: sheet() });
  t.server.manual.add('action');
  const text = ragged['empty rows after a wide one'];
  globalThis.__clipboard = text;
  t.sel.set(1, 1);
  t.app.rowsMenu({ x: 0, y: 0 });
  t.doc.querySelector('.menu').querySelectorAll('button').find((b) => b.textContent.startsWith('Paste values only')).click();
  type(t, 'draft');
  assert.notEqual(t.editor.ed, null);
  await settle();
  assert.deepEqual(notices(t).at(-1), ['bad', 'This paste holds more cells or text than one edit can carry. Nothing was changed; paste it in smaller parts. The text:', text]);
  assert.equal(t.server.socket.frames('action').length, 0);
  assert.notEqual(t.editor.ed, null, 'the editor is still open');
  assert.equal(t.editor.text, 'draft');
  key(t, 'Enter');
  assert.equal(lastOps(t.server)[0].text, 'draft', 'and its text commits');
});

test('one pasted value that would fill the selection past what an action holds is refused', async () => {
  const t = await startApp({ server: new FakeServer({ tabs: [{ id: 1, name: 'Wide', rows: 100, cols: 50 }] }) });
  t.server.manual.add('action');
  t.sel.range(0, 0, 99, 49);
  // 5,000 cells of 2,000 characters are 10,000,000 bytes of text.
  const text = 'y'.repeat(2000);
  const n = built(1000, () => fire(t.doc, 'paste', clip(text)));
  assert.ok(n < 1000, `${n} array entries`);
  assert.equal(t.server.socket.sent.filter((m) => m.type === 'action' || m.type === 'action_part').length, 0);
  assert.equal(t.net.stopped, null);
  assert.deepEqual(notices(t).at(-1).slice(1), ['This paste holds more cells or text than one edit can carry. Nothing was changed; paste it in smaller parts. The text:', text]);
  // A shorter value fills the same selection.
  fire(t.doc, 'paste', clip('y'.repeat(20)));
  const { values } = t.net.pending.at(-1).ops[0];
  assert.deepEqual([values.length, values[0].length, values[99][49]], [100, 50, 'y'.repeat(20)]);
});

test('pastes inside what an action holds are written whole: quoted, ragged, and as wide as a tab can be', async () => {
  const t = await startApp({ server: sheet() });
  t.server.manual.add('action');
  t.sel.set(1, 1);
  fire(t.doc, 'paste', clip('"a\tb"\t"c""d"\tz\r\n"two\nlines"\n'));
  assert.deepEqual(lastOps(t.server)[0].values, [['a\tb', 'c"d', 'z'], ['two\nlines', '', '']]);
  // One row of 702 fields, the columns a tab holds, over 199 short rows.
  t.sel.set(0, 0);
  fire(t.doc, 'paste', clip('\t'.repeat(701) + '\n' + 'x\n'.repeat(199)));
  const op = t.net.pending.at(-1).ops[0];
  assert.deepEqual([op.rows.length, op.cols.length, op.values.length], [200, 702, 200]);
  assert.ok(op.values.every((row) => row.length === 702));
  assert.deepEqual([op.values[0][701], op.values[199][0], op.values[199][701]], ['', 'x', '']);
  assert.ok(t.server.socket.frames('action_part').length > 1, 'it went out in parts');
  assert.equal(t.net.stopped, null);
});

test('a text is measured as it is read: the same rows, widest row and characters', () => {
  const texts = ['', 'x', 'a\tb\n1\t2', 'a\tb\r\n1\t2\r\n', 'a\n\nb', '\n', '\r\n\r\n', '\t', '\t\n', 'a\tb\tc\nd', 'd\na\tb\tc', 'a\r\rb',
    '"two\nlines"\t"say ""hi"""\t"tab\there"\t', '""', '""""', '"', '"open\tto\nthe end', '"a"b"c"', 'a"b"\t"c', '""x\t""\n""', '"a""', '😀\t한글\n\ud83d',
    encodeTSV([['=SUM(A1)', "'007", '한글'], ['', '"', '\t']]), '\t\t\t\n\n\nx', 'x\n\n"a\tb"\t\t\t\r\n'];
  for (const text of texts) {
    const rows = decodeTSV(text);
    const units = rows.reduce((n, row) => n + row.reduce((k, f) => k + f.length, 0), 0);
    assert.deepEqual(measureTSV(text), { height: rows.length, width: rows[0].length, units }, JSON.stringify(text.slice(0, 40)));
    assert.ok(rows.every((row) => row.length === rows[0].length), JSON.stringify(text.slice(0, 40)));
  }
  // The measure itself builds no row and no field.
  const big = ragged['one wide row, then many short ones'];
  let size;
  assert.equal(built(0, () => { size = measureTSV(big); }), 0);
  assert.deepEqual(size, { height: 100001, width: 100000, units: 100000 });
});

test('a paste is refused from the first rectangle whose cells and text cannot fit an action', async () => {
  const t = await startApp({ server: sheet() });
  const cb = t.app.clipboard;
  // One value over two cells: each cell takes its text and three bytes.
  t.sel.range(0, 0, 1, 0);
  const most = ACTION_MAX / 2 - 3;
  const p = cb.pasteOps('z'.repeat(most), false);
  assert.deepEqual([p.ops[0].values.length, p.ops[0].values[0].length, p.ops[0].values[1][0].length], [2, 1, most]);
  assert.equal(cb.pasteOps('z'.repeat(most + 1), false), null);
  // Cells with no text: 1,024 columns by 2,731 rows are 8,389,632 bytes.
  t.sel.set(0, 0);
  const empty = '\t'.repeat(1023) + '\n'.repeat(2731);
  assert.deepEqual(measureTSV(empty), { height: 2731, width: 1024, units: 0 });
  assert.ok(3 * 1024 * 2730 <= ACTION_MAX && 3 * 1024 * 2731 > ACTION_MAX);
  assert.equal(built(1000, () => assert.equal(cb.pasteOps(empty, false), null)), 0);
});

test('a paste that passes the measure and still cannot fit an action is refused the same way, and editing goes on', async () => {
  const t = await startApp({ server: sheet() });
  t.server.manual.add('action');
  type(t, 'held');
  key(t, 'Enter');
  t.sel.set(4, 2);
  t.app.changed();
  const sent = () => t.server.socket.sent.filter((m) => m.type === 'action' || m.type === 'action_part').length;
  const pastes = {
    // 1,024 by 2,730 empty cells: their brackets and the ids of the rows they add pass the limit.
    'four kilobytes of tabs and line breaks': '\t'.repeat(1023) + '\n'.repeat(2730),
    'one cell of text that takes three bytes a character': '한'.repeat(2800000),
    'one cell of characters an action writes as two': '\\'.repeat(4200000),
  };
  for (const [name, text] of Object.entries(pastes)) {
    const size = measureTSV(text);
    assert.ok(3 * size.height * size.width + size.units <= ACTION_MAX, `${name}: the measure passes it`);
    fire(t.doc, 'paste', clip(text));
    assert.equal(t.net.stopped, null, `${name}: editing goes on`);
    assert.equal(sent(), 1, `${name}: nothing was sent`);
    assert.deepEqual(notices(t).at(-1).slice(0, 2), ['bad', 'This paste holds more cells or text than one edit can carry. Nothing was changed; paste it in smaller parts. The text:'], name);
    assert.equal(notices(t).at(-1)[2], text, `${name}: the text is offered back`);
    assert.equal(t.net.pending.length, 1, `${name}: the earlier edit is still on its way`);
    assert.equal(t.model.pending.size, 1, name);
    assert.equal(shownAt(t, 0, 0), 'held', name);
  }
  // A typed edit and a paste go out after them.
  type(t, 'next');
  key(t, 'Enter');
  assert.equal(lastOps(t.server)[0].text, 'next');
  fire(t.doc, 'paste', clip('a\tb'));
  assert.deepEqual(lastOps(t.server)[0].values, [['a', 'b']]);
  assert.equal(t.net.pending.length, 3);
});

// A paste carries at most ACTION_MAX / 3 cells, 2,796,202. A tab as wide
// as a tab can be passes that with its row 3,984.
const wide = (rows, cells = {}) => new FakeServer({ tabs: [{ id: 1, name: 'Wide', rows, cols: 702, cells }] });
const tooLarge = (what) => ['bad', `This selection is too large to ${what}. Select fewer cells.`, null];

test('a copy or a cut of more cells than a paste can carry is refused before a cell is read', async () => {
  const t = await startApp({ server: wide(3984, { '0,0': 'first', '3982,701': 'last' }) });
  assert.ok(3 * 3983 * 702 <= ACTION_MAX && 3 * 3984 * 702 > ACTION_MAX);
  // An earlier copy stays the one a paste copies from.
  t.sel.set(0, 0);
  fire(t.doc, 'copy', clip());
  for (const what of ['copy', 'cut']) {
    t.sel.range(0, 0, 3983, 701);
    const e = clip('unchanged');
    let fired;
    const n = built(1000, () => { fired = fire(t.doc, what, e); });
    assert.ok(n < 1000, `${what}: ${n} array entries`);
    assert.equal(fired.defaultPrevented, false, what);
    assert.equal(e.data['text/plain'], 'unchanged', `${what}: the clipboard keeps what it held`);
    assert.deepEqual(notices(t).at(-1), tooLarge(what));
  }
  t.sel.set(5, 5);
  fire(t.doc, 'paste', clip('first'));
  assert.equal(lastOps(t.server)[0].op, 'copy', 'the earlier copy is still remembered');
  // One row fewer is copied whole, blank cells and all.
  t.sel.range(0, 0, 3982, 701);
  const e = clip();
  assert.equal(fire(t.doc, 'copy', e).defaultPrevented, true);
  const text = e.data['text/plain'];
  assert.equal(text.length, 3983 * 701 + 3982 + 'first'.length + 'last'.length);
  assert.ok(text.startsWith('first\t\t') && text.endsWith('\t\tlast'));
  assert.equal(t.app.clipboard.kept.rows.length, 3983);
  assert.equal(t.app.clipboard.kept.cols.length, 702);
});

test('a whole tab of 100,000 rows and 702 columns is not walked for a copy', async () => {
  const t = await startApp({ server: wide(100000, { '99999,701': 'end' }) });
  assert.equal(t.model.tab.loading, false);
  // As cells, and as the whole tab up to its last filled cell.
  for (const select of [() => t.sel.range(0, 0, 99999, 701), () => t.sel.all()]) {
    select();
    const e = clip('unchanged');
    const n = built(1000, () => fire(t.doc, 'copy', e));
    assert.ok(n < 1000, `${n} array entries`);
    assert.equal(e.data['text/plain'], 'unchanged');
    assert.deepEqual(notices(t).at(-1), tooLarge('copy'));
  }
  assert.equal(t.server.socket.frames('action').length, 0);
});

test('a whole tab of ordinary size is copied as before, as cells and as the whole tab', async () => {
  const t = await startApp({ server: sheet() });
  t.sel.range(0, 0, 5, 3);
  const cells = clip();
  fire(t.doc, 'copy', cells);
  assert.equal(cells.data['text/plain'], 'Item\tAmount\t\t\nVenue\t$1,200.00\t\t\nFood\t0\t\t\n\t\t\t\n\t\t\t\n\t\t\t', 'blank cells at the end are part of it');
  t.sel.all();
  const all = clip();
  fire(t.doc, 'copy', all);
  assert.equal(all.data['text/plain'], 'Item\tAmount\nVenue\t$1,200.00\nFood\t0', 'the whole tab ends at its last filled cell');
  assert.equal(notices(t).length, 0);
  // The copy is remembered, and the CSV download is a link the host answers.
  t.sel.set(4, 2);
  fire(t.doc, 'paste', clip(all.data['text/plain']));
  assert.equal(lastOps(t.server)[0].op, 'copy');
  t.app.downloadCSV();
  assert.match(notices(t).at(-1)[1], /^Downloading “Budget\.csv”/);
});

test('a paste of a cut that cannot be sent leaves the cut to be pasted again', async () => {
  const t = await startApp({ server: sheet() });
  const cb = t.app.clipboard;
  t.sel.set(1, 0);
  fire(t.doc, 'cut', clip());
  t.sel.set(4, 0);
  const asked = [];
  assert.equal(cb.pasteOps('Venue', false, undefined, (list) => { asked.push(list[0].op); return false; }), null);
  assert.deepEqual(asked, ['copy']);
  assert.equal(cb.remembers('Venue'), true);
  assert.equal(cb.pasteOps('Venue', false).ops[0].cut, true);
  // Text from elsewhere is asked about once its write exists.
  assert.equal(cb.pasteOps('a\tb', false, undefined, (list) => { asked.push(list[0].op); return false; }), null);
  assert.deepEqual(asked, ['copy', 'write']);
});

// pasteInto is what a browser does with a paste while an edit is open or
// the formula bar has focus: the page leaves the event to the text area,
// which takes the text at the caret, whatever its length.
function pasteInto(ta, text) {
  assert.equal(fire(ta, 'paste', clip(text)).defaultPrevented, false, 'the paste is left to the text area');
  const at = ta.selectionStart;
  ta.value = ta.value.slice(0, at) + text + ta.value.slice(ta.selectionEnd);
  ta.selectionStart = ta.selectionEnd = at + text.length;
  fire(ta, 'input');
}

test('text that no edit can carry, pasted into the cell editor or the formula bar, is refused at Enter and editing goes on', async () => {
  const text = '한'.repeat(2800000);
  const editors = {
    'the cell editor': (t) => { key(t, 'F2'); return t.grid.input; },
    'the formula bar': (t) => { t.app.bar.focus(); return t.app.bar; },
  };
  for (const [name, open] of Object.entries(editors)) {
    const t = await startApp({ server: sheet() });
    // An edit elsewhere is still on its way.
    t.server.manual.add('action');
    type(t, 'held');
    key(t, 'Enter');
    t.sel.set(4, 2);
    t.app.changed();
    const sent = () => t.server.socket.sent.filter((m) => m.type === 'action' || m.type === 'action_part').length;
    const ta = open(t);
    pasteInto(ta, text);
    assert.equal(t.editor.open, true, name);
    assert.equal(t.editor.area, ta, name);
    assert.equal(t.editor.text, text, `${name}: the text area took the whole paste`);
    key(t, 'Enter');
    assert.equal(t.net.stopped, null, `${name}: editing goes on`);
    assert.equal(t.net.open, true, `${name}: the connection stays`);
    assert.equal(t.editor.ed, null, `${name}: the edit ended`);
    assert.equal(sent(), 1, `${name}: nothing was sent`);
    assert.deepEqual(notices(t).at(-1).slice(0, 2), ['bad', 'This edit of C5 is larger than one edit can carry. Nothing was changed. Your text:'], name);
    assert.equal(notices(t).at(-1)[2], text, `${name}: the text is offered back`);
    assert.equal(t.net.pending.length, 1, `${name}: the earlier edit is still on its way`);
    assert.equal(t.model.pending.size, 1, name);
    assert.equal(shownAt(t, 0, 0), 'held', name);
    assert.equal(t.model.isPending(t.model.rowAt(4), t.model.colAt(2)), false, `${name}: no pending text shows`);
    // A typed edit goes out after it.
    t.server.manual.delete('action');
    type(t, 'next');
    key(t, 'Enter');
    assert.equal(lastOps(t.server)[0].text, 'next', name);
    assert.equal(t.net.stopped, null, name);
  }
});
