'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { FRAME_MAX, TooLargeError, ackError, actionFrames, actionShapeError, address, colName, dateForm, decodeCell, expandIds, isBlankCell, isStructural, numberFormat, ops, packIds, rangeName, peekedError, rebasedError, resyncError, screenPosition, seqCmp, seqNext, timeForm, utf8Length, valueText, welcomeError, wirePosition } from '../protocol.js';
import { Net } from '../net.js';
import { checkFrame } from './server.mjs';

test('id runs expand and pack in position order', () => {
  assert.deepEqual(expandIds([[5, 3], [20, 1]]), [5, 6, 7, 20]);
  assert.deepEqual(packIds([5, 6, 7, 20, 21, 9]), [[5, 3], [20, 2], [9, 1]]);
  assert.deepEqual(packIds([]), []);
});

test('columns are named A to Z, then AA to ZZ', () => {
  assert.equal(colName(0), 'A');
  assert.equal(colName(25), 'Z');
  assert.equal(colName(26), 'AA');
  assert.equal(colName(27), 'AB');
  assert.equal(colName(701), 'ZZ');
  assert.equal(address(6, 1), 'B7');
  assert.equal(rangeName({ r1: 1, c1: 1, r2: 8, c2: 3 }), 'B2:D9');
  assert.equal(rangeName({ r1: 0, c1: 0, r2: 0, c2: 0 }), 'A1');
  assert.deepEqual(screenPosition(wirePosition(4, 2)), { r: 4, c: 2 });
});

test('sequences are decimal strings of any size', () => {
  assert.equal(seqCmp('9', '10'), -1);
  assert.equal(seqCmp('9007199254740993', '9007199254740992'), 1);
  assert.equal(seqNext('9007199254740992'), '9007199254740993');
});

test('a cell frame leaves trailing defaults out', () => {
  assert.deepEqual(decodeCell([7, 'n', 1200, '$1,200.00', '1200', 3]), { col: 7, kind: 'n', formula: false, value: 1200, shown: '$1,200.00', edit: '1200', style: 3 });
  assert.deepEqual(decodeCell([7, 't', 'Rent']), { col: 7, kind: 't', formula: false, value: 'Rent', shown: 'Rent', edit: 'Rent', style: 0 });
  // a formula: shown differs from the value, and the edit text is never sent
  const f = decodeCell([7, 'nf', 0.4, '40.00%']);
  assert.equal(f.kind, 'n');
  assert.equal(f.formula, true);
  assert.equal(f.shown, '40.00%');
  assert.equal(decodeCell([7, 'ef', '#DIV/0!']).shown, '#DIV/0!');
  assert.equal(decodeCell([7, 'b', true]).shown, 'TRUE');
  // a skipped middle field is null
  assert.equal(decodeCell([7, 't', 'x', null, null, 2]).style, 2);
  assert.equal(valueText(false), 'FALSE');
  assert.equal(valueText(1234.5), '1234.5');
});

test('a blank cell keeps its style, and a cell with nothing is gone', () => {
  assert.equal(isBlankCell(decodeCell([7])), true);
  assert.equal(isBlankCell(decodeCell([7, '', null, null, null, 4])), false);
  assert.equal(isBlankCell(decodeCell([7, 't', ''])), false);
});

test('every operation builder passes the schema of section 7.1', () => {
  const all = [
    ops.set(1, 5, 6, '=A1', [0, 0]), ops.write(1, [5, { new: 1 }], [6], [['a'], ['b']]), ops.clear(1, [[5, 1]], [[6, 1]]),
    ops.format(1, [[5, 1]], [[6, 1]], { bold: true, number: numberFormat('currency') }), ops.width(1, [[6, 1]], 180),
    ops.copy({ tab: 1, rows: [[5, 1]], cols: [[6, 1]] }, { tab: 1, rows: [7], cols: [6] }, 'all', false),
    ops.fill(1, { rows: [[5, 1]], cols: [[6, 1]] }, { rows: [[7, 2]], cols: [[6, 1]] }),
    ops.insertRows(1, 5, 'before', 2), ops.insertCols(1, 6, 'after', 1), ops.appendRows(1, 100), ops.appendCols(1, 1),
    ops.deleteRows(1, [[5, 1]]), ops.deleteCols(1, [[6, 1]]), ops.sort(1, 'all', 6, true), ops.settings(1, 1, 0),
    ops.addTab('Notes'), ops.renameTab(1, 'Budget'), ops.moveTab(1, null), ops.deleteTab(1), ops.undo(), ops.redo(),
  ];
  // each in an action of its own, as most must be (see the next test)
  for (const op of all) checkFrame({ type: 'action', n: '1', base: '10', ops: [op] });
  assert.deepEqual(numberFormat('number'), { kind: 'number', decimals: 2, thousands: true });
  assert.deepEqual(numberFormat('datetime', { time: 'h:mm AM/PM' }), { kind: 'datetime', date: 'yyyy-mm-dd', time: 'h:mm AM/PM' });
  assert.deepEqual(numberFormat('general'), { kind: 'general' });
  // a date and a time name their form as the server does
  assert.deepEqual(numberFormat('date', { date: 'm/d/yyyy' }), { kind: 'date', pattern: 'm/d/yyyy' });
  assert.deepEqual(numberFormat('time'), { kind: 'time', pattern: 'hh:mm' });
  assert.equal(dateForm({ kind: 'date', pattern: 'mmm d, yyyy' }), 'mmm d, yyyy');
  assert.equal(timeForm({ kind: 'time', pattern: 'hh:mm:ss' }), 'hh:mm:ss');
  assert.equal(dateForm({ kind: 'datetime', date: 'm/d/yyyy', time: 'hh:mm' }), 'm/d/yyyy');
  assert.equal(timeForm({ kind: 'datetime', date: 'm/d/yyyy', time: 'h:mm AM/PM' }), 'h:mm AM/PM');
  // the server refuses a number format with any other field
  for (const kind of ['general', 'number', 'percent', 'currency', 'date', 'time', 'datetime', 'text']) {
    checkFrame({ type: 'action', n: '1', base: '10', ops: [ops.format(1, [[5, 1]], [[6, 1]], { number: numberFormat(kind) })] });
  }
  assert.throws(() => checkFrame({ type: 'action', n: '1', base: '10', ops: [ops.format(1, [[5, 1]], [[6, 1]], { number: { kind: 'date', date: 'm/d/yyyy' } })] }), /set\.number of kind date has the field date/);
});

test('an action holds content and format operations, or one operation of another kind alone', () => {
  const cell = ops.set(1, 5, 6, 'x', [0, 0]);
  const together = [cell, ops.write(1, [5], [6], [['a']]), ops.clear(1, [[5, 1]], [[6, 1]]), ops.format(1, [[5, 1]], [[6, 1]], { bold: true }), ops.width(1, [[6, 1]], 180),
    ops.copy({ tab: 1, rows: [[5, 1]], cols: [[6, 1]] }, { tab: 1, rows: [7], cols: [6] }, 'all', false), ops.fill(1, { rows: [[5, 1]], cols: [[6, 1]] }, { rows: [[7, 2]], cols: [[6, 1]] }), ops.settings(1, 1, 0)];
  const alone = [ops.insertRows(1, 5, 'before', 2), ops.insertCols(1, 6, 'after', 1), ops.appendRows(1, 100), ops.appendCols(1, 1), ops.deleteRows(1, [[5, 1]]), ops.deleteCols(1, [[6, 1]]),
    ops.sort(1, 'all', 6, true), ops.addTab('Notes'), ops.renameTab(1, 'Budget'), ops.moveTab(1, null), ops.deleteTab(1), ops.undo(), ops.redo()];
  assert.equal(actionShapeError(together), null);
  checkFrame({ type: 'action', n: '1', base: '10', ops: together });
  for (const op of alone) {
    assert.equal(actionShapeError([op]), null, op.op);
    for (const mix of [[cell, op], [op, cell], [op, op]]) {
      assert.match(actionShapeError(mix), new RegExp(`^${op.op} is an action of its own`), op.op);
      assert.throws(() => checkFrame({ type: 'action', n: '1', base: '10', ops: mix }), /is an action of its own/, `the scripted server refuses ${op.op} in a mix`);
    }
  }
  assert.match(actionShapeError([]), /at least one operation/);
  // the one function that queues an action refuses a mix, whoever built it
  const net = new Net();
  net.access = 'private';
  assert.throws(() => net.queue([cell, ops.insertRows(1, 5, 'before', 1)], '10'), /not an action: insert_rows is an action of its own/);
  assert.throws(() => net.queue([], '10'), /not an action/);
  assert.equal(net.pending.length, 0, 'nothing was queued');
  assert.equal(net.queue([cell, ops.format(1, [[5, 1]], [[6, 1]], { bold: true })], '10').n, '1');
});

test('a structural delta is one that moves rows or columns, or renames or deletes a tab', () => {
  const tabs = [{ id: 1, name: 'Data' }, { id: 2, name: 'Notes' }];
  assert.equal(isStructural({ seq: '5', cells: [[9, [[3, 't', 'x']]]] }, tabs), false);
  assert.equal(isStructural({ seq: '5', struct: [{ op: 'insert_rows', tab: 1, ids: [[40, 1]], after: null }] }, tabs), true);
  assert.equal(isStructural({ seq: '5', struct: [{ op: 'sort', tab: 1, positions: [[0, 2]], ids: [8, 7] }] }, tabs), true);
  assert.equal(isStructural({ seq: '5', tabs: [{ id: 1, name: 'Data' }, { id: 2, name: 'Notes' }, { id: 3, name: 'New' }] }, tabs), false, 'an added tab changes no reference');
  assert.equal(isStructural({ seq: '5', tabs: [{ id: 1, name: 'Old data' }, { id: 2, name: 'Notes' }] }, tabs), true);
  assert.equal(isStructural({ seq: '5', tabs: [{ id: 2, name: 'Notes' }] }, tabs), true);
  assert.equal(isStructural({ seq: '5', tabs: [{ id: 2, name: 'Notes' }, { id: 1, name: 'Data' }] }, tabs), false, 'a reorder changes no reference');
});

test('an action over the frame limit travels as action_part frames that join to its JSON', () => {
  const small = actionFrames({ n: '3', base: '10', ops: [ops.undo()] });
  assert.deepEqual(small.map((s) => JSON.parse(s)), [{ type: 'action', n: '3', base: '10', ops: [{ op: 'undo' }] }]);
  const values = Array.from({ length: 300 }, (_, i) => [`한글 "quoted" ${i} ${'x'.repeat(2000)}`]);
  const action = { n: '4', base: '10', ops: [ops.write(1, values.map((_, i) => i + 1), [9], values)] };
  const frames = actionFrames(action);
  assert.ok(frames.length > 1);
  let text = '';
  frames.forEach((f, i) => {
    assert.ok(utf8Length(f) <= FRAME_MAX, `part ${i} fits a frame`);
    const m = JSON.parse(f);
    assert.equal(m.type, 'action_part');
    assert.equal(m.n, '4');
    assert.equal(m.part, i);
    assert.equal(m.parts, frames.length);
    text += m.data;
  });
  assert.deepEqual(JSON.parse(text), { type: 'action', n: '4', base: '10', ops: action.ops });
});

test('an action over 8 MiB is refused before it is sent', () => {
  const big = [['x'.repeat(9 * 1024 * 1024)]];
  assert.throws(() => actionFrames({ n: '1', base: '0', ops: [ops.write(1, [1], [2], big)] }), TooLargeError);
});

// Appendix A writes `| null` for no field of a server frame: a field that
// may be left out is absent or holds its type, and a null in its place is
// no way of leaving it out.
test('a server frame whose optional field holds null does not fit', () => {
  // Each case: the check, a frame that fits, and its fields that may be
  // left out one at a time.
  const cases = [
    [welcomeError, { type: 'welcome', access: 'private', session: 'tok', session_id: 's1', me: { label: 'Ann', verified: true }, seq: '10', date: '2026-11-13', tabs: [] }, ['session', 'session_id', 'me']],
    [resyncError, { type: 'resync', generation: 2, seq: '11', date: '2026-11-13', tabs: [] }, []],
    [(m) => ackError(m, 1), { type: 'ack', n: '1', seq: '11', results: [{ status: 'applied', applied: 1 }], undo: { applied: 1, skipped: 0 } }, ['undo']],
    [peekedError, { type: 'peeked', id: 1, seq: '10', edit: 'x', formula: false }, []],
    [rebasedError, { type: 'rebased', id: 1, text: 'x', at: [0, 0], base: '10' }, ['text', 'at', 'base']],
    [rebasedError, { type: 'rebased', id: 1, refused: 'gone' }, []],
  ];
  for (const [check, good, optional] of cases) {
    assert.equal(check(good), null, `${good.type} as it is`);
    for (const k of Object.keys(good)) {
      if (k !== 'type') assert.notEqual(check({ ...good, [k]: null }), null, `${good.type} with ${k} null`);
    }
    for (const k of optional) {
      const left = { ...good };
      delete left[k];
      assert.equal(check(left), null, `${good.type} without ${k}`);
    }
  }
  const resync = { type: 'resync', generation: 2, seq: '11' };
  assert.notEqual(resyncError(resync), null, 'a resync carries its tab list and its date');
  assert.notEqual(resyncError({ ...resync, tabs: [] }), null, 'a tab list without its date');
  assert.notEqual(resyncError({ ...resync, date: '2026-11-13' }), null, 'a date without its tab list');
});
