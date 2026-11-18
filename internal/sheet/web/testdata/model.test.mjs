'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { Model } from '../model.js';

const TABS = [{ id: 1, name: 'Budget', rows: 5, cols: 3, frozen_rows: 0, frozen_cols: 0 }];
const head = (extra = {}) => ({ generation: 1, tab: 1, seq: '10', rows: [[100, 5]], cols: [[200, 3]], widths: { 201: 180 }, frozen_rows: 1, frozen_cols: 0, styles: [{}, { bold: true }], ...extra });

function loaded() {
  const m = new Model();
  m.setTabs(TABS);
  m.openHead(head());
  m.addRows({ generation: 1, tab: 1, part: 0, rows: [[100, [[200, 't', 'Item', null, null, 1], [201, 't', 'Amount']]], [101, [[200, 't', 'Venue'], [201, 'n', 1200, '$1,200.00', '1200']]]] });
  m.addRows({ generation: 1, tab: 1, part: 1, rows: [[104, [[201, 'nf', 1200, '$1,200.00']]]] });
  m.done();
  return m;
}

test('a snapshot arrives in chunks and gives rows, columns, widths, styles and cells by id', () => {
  const m = loaded();
  assert.equal(m.rowCount, 5);
  assert.equal(m.colCount, 3);
  assert.equal(m.rowAt(1), 101);
  assert.equal(m.rowIndex(104), 4);
  assert.equal(m.colIndex(999), -1);
  assert.equal(m.widthOf(201), 180);
  assert.equal(m.widthOf(200), 104, 'a column without a width has the default');
  assert.equal(m.tab.frozenRows, 1);
  assert.equal(m.shown(101, 201), '$1,200.00');
  assert.equal(m.cell(101, 201).edit, '1200');
  assert.equal(m.cell(104, 201).formula, true);
  assert.deepEqual(m.format(m.cell(100, 200)), { bold: true });
  assert.deepEqual(m.format(m.cell(101, 200)), {});
  assert.deepEqual(m.usedRange(), { r: 4, c: 1 });
  assert.equal(m.seq, '10');
});

test('a row that takes several rows frames keeps the cells of each', () => {
  const m = new Model();
  m.setTabs(TABS);
  m.openHead(head());
  m.addRows({ generation: 1, tab: 1, part: 0, rows: [[100, [[200, 't', 'a']]], [101, [[200, 't', 'b']]]] });
  m.addRows({ generation: 1, tab: 1, part: 1, rows: [[101, [[201, 't', 'c']]]] });
  m.addRows({ generation: 1, tab: 1, part: 2, rows: [[101, [[202, 't', 'd']]], [102, [[200, 't', 'e']]]] });
  m.done();
  assert.deepEqual([200, 201, 202].map((col) => m.shown(101, col)), ['b', 'c', 'd']);
  assert.equal(m.shown(100, 200), 'a');
  assert.equal(m.shown(102, 200), 'e');
});

test('a delta that arrives during the transfer is not replaced by a later rows frame', () => {
  const m = new Model();
  m.setTabs(TABS);
  m.openHead(head());
  const out = m.apply({ seq: '11', cells: [[101, [[201, 'n', 999]]]] });
  assert.deepEqual([...out.rows], [101]);
  m.addRows({ generation: 1, tab: 1, part: 0, rows: [[101, [[200, 't', 'Venue'], [201, 'n', 1200]]]] });
  m.done();
  assert.equal(m.shown(101, 201), '999');
  assert.equal(m.shown(101, 200), 'Venue');
  assert.equal(m.seq, '11');
});

test('a structural delta during the transfer asks for a new watch', () => {
  const m = new Model();
  m.setTabs(TABS);
  m.openHead(head());
  const out = m.apply({ seq: '11', struct: [{ op: 'insert_rows', tab: 1, ids: [[300, 1]], after: null }] });
  assert.equal(out.rewatch, true);
  assert.equal(m.rowCount, 5, 'nothing is applied to the half-loaded tab');
});

test('cell deltas set, restyle and clear cells', () => {
  const m = loaded();
  m.apply({ seq: '11', styles: [{ fill: 'yellow-100' }], cells: [[102, [[200, 't', 'Food'], [202, '', null, null, null, 2]]], [101, [[201]]]] });
  assert.equal(m.shown(102, 200), 'Food');
  assert.deepEqual(m.format(m.cell(102, 202)), { fill: 'yellow-100' });
  assert.equal(m.cell(102, 202).kind, '', 'a blank cell keeps its format');
  assert.equal(m.cell(101, 201), null, 'a cell frame with nothing removes the cell');
  assert.equal(m.filledAt(2, 2), false);
});

test('inserted rows go after their anchor, deleted rows take their cells with them', () => {
  const m = loaded();
  let out = m.apply({ seq: '11', struct: [{ op: 'insert_rows', tab: 1, ids: [[300, 2]], after: 100 }] });
  assert.equal(out.struct, true);
  assert.deepEqual(m.tab.rows, [100, 300, 301, 101, 102, 103, 104]);
  assert.equal(m.rowIndex(101), 3, 'positions follow the new order');
  m.apply({ seq: '12', struct: [{ op: 'insert_rows', tab: 1, ids: [[310, 1]], after: null }] });
  assert.equal(m.rowAt(0), 310);
  out = m.apply({ seq: '13', struct: [{ op: 'delete_rows', tab: 1, ids: [[101, 1], [310, 1]] }] });
  assert.deepEqual(m.tab.rows, [100, 300, 301, 102, 103, 104]);
  assert.equal(m.cell(101, 200), null);
  assert.equal(m.shown(104, 201), '$1,200.00', 'other rows keep their cells: they are held by id');
});

test('columns insert and delete the same way', () => {
  const m = loaded();
  m.apply({ seq: '11', struct: [{ op: 'insert_cols', tab: 1, ids: [[400, 1]], after: 200 }] });
  assert.deepEqual(m.tab.cols, [200, 400, 201, 202]);
  m.apply({ seq: '12', struct: [{ op: 'delete_cols', tab: 1, ids: [[201, 1]] }] });
  assert.deepEqual(m.tab.cols, [200, 400, 202]);
  assert.equal(m.cell(101, 201), null);
  assert.equal(m.rowCells(104), null, 'a row whose only cell was in the column is empty');
  assert.equal(m.widthOf(201), 104);
});

test('a sort names the new ids of the positions it reordered', () => {
  const m = loaded();
  m.apply({ seq: '11', struct: [{ op: 'sort', tab: 1, positions: [[1, 2], [4, 1]], ids: [104, 101, 102] }] });
  assert.deepEqual(m.tab.rows, [100, 104, 101, 103, 102]);
  assert.equal(m.shown(m.rowAt(1), 201), '$1,200.00', 'each row moved with its cells');
});

test('widths, the tab list and the date travel in deltas', () => {
  const m = loaded();
  const out = m.apply({ seq: '11', date: '2026-11-14', widths: [[200, 240]], tabs: [{ id: 1, name: 'Costs', rows: 5, cols: 3, frozen_rows: 2, frozen_cols: 1 }, { id: 2, name: 'Notes', rows: 9, cols: 3, frozen_rows: 0, frozen_cols: 0 }] });
  assert.equal(out.widths, true);
  assert.equal(out.tabs, true);
  assert.equal(m.widthOf(200), 240);
  assert.equal(m.date, '2026-11-14');
  assert.equal(m.tab.name, 'Costs');
  assert.equal(m.tab.frozenRows, 2);
  assert.equal(m.tab.frozenCols, 1);
  assert.equal(m.tabs.length, 2);
  assert.equal(m.apply({ seq: '12', tabs: [{ id: 2, name: 'Notes', rows: 9, cols: 3, frozen_rows: 0, frozen_cols: 0 }] }).gone, true);
});

test('pending text lies over the server value and leaves with its action', () => {
  const m = loaded();
  m.addPending('1', [{ row: 101, col: 201, text: '1500' }]);
  m.addPending('2', [{ row: 102, col: 200, text: 'Food' }]);
  assert.equal(m.shown(101, 201), '1500');
  assert.equal(m.isPending(101, 201), true);
  assert.equal(m.cell(101, 201).shown, '$1,200.00', 'the server value is untouched');
  // a later edit of the same cell keeps its own pending text when the first is confirmed
  m.addPending('3', [{ row: 101, col: 201, text: '1600' }]);
  assert.equal(m.dropPending('1'), false);
  assert.equal(m.shown(101, 201), '1600');
  m.dropPending('3');
  assert.equal(m.shown(101, 201), '$1,200.00');
  m.addPendingWidth('4', [200], 300);
  assert.equal(m.widthOf(200), 300);
  m.dropPending('4');
  assert.equal(m.widthOf(200), 104);
});

test('peeked formula text is kept until its cell, the structure or a tab name changes', () => {
  const m = loaded();
  m.setPeek(104, 201, { edit: '=B2', formula: true });
  m.setPeek(101, 201, { edit: '1200', formula: false });
  m.apply({ seq: '11', cells: [[102, [[200, 't', 'x']]]] });
  assert.equal(m.peeked(104, 201).edit, '=B2');
  m.apply({ seq: '12', cells: [[104, [[201, 'nf', 5]]]] });
  assert.equal(m.peeked(104, 201), null);
  assert.equal(m.peeked(101, 201).edit, '1200');
  m.apply({ seq: '13', struct: [{ op: 'insert_rows', tab: 1, ids: [[300, 1]], after: null }] });
  assert.equal(m.peeked(101, 201), null);
});

test('eachCell walks the filled cells of a range without walking the tab', () => {
  const m = loaded();
  const seen = [];
  m.eachCell({ r1: 0, c1: 1, r2: 4, c2: 1 }, (r, c, cell) => seen.push([r, c, cell.shown]));
  assert.deepEqual(seen.sort(), [[0, 1, 'Amount'], [1, 1, '$1,200.00'], [4, 1, '$1,200.00']]);
});

test('a new snapshot replaces the style list, and a grid that waits for one adds nothing to it', () => {
  const m = loaded();
  m.apply({ seq: '11', styles: [{ italic: true }], cells: [[101, [[200, 't', 'Venue', null, null, 2]]]] });
  assert.equal(m.tab.styles.length, 3);
  assert.deepEqual(m.format(m.cell(101, 200)), { italic: true });
  // The server rebuilt its list: a resync, deltas that the stale grid does
  // not take, then a head with the new list, where the indices mean
  // something else.
  m.lag();
  const old = m.tab.styles;
  m.apply({ seq: '12', styles: [{ wrap: true }, { underline: true }] });
  assert.equal(old.length, 3, 'a stale grid appends no style');
  m.openHead(head({ generation: 2, seq: '12', styles: [{}, { italic: true }] }));
  m.addRows({ generation: 2, tab: 1, part: 0, rows: [[101, [[200, 't', 'Venue', null, null, 1]]]] });
  m.done();
  assert.deepEqual(m.tab.styles, [{}, { italic: true }], 'the list is the one of the head, not the old one with more');
  assert.notEqual(m.tab.styles, old);
  assert.deepEqual(m.format(m.cell(101, 200)), { italic: true });
  // A delta after the snapshot adds to the new list.
  m.apply({ seq: '13', styles: [{ bold: true }], cells: [[100, [[200, 't', 'Item', null, null, 2]]]] });
  assert.deepEqual(m.format(m.cell(100, 200)), { bold: true });
  assert.equal(m.tab.styles.length, 3);
});
