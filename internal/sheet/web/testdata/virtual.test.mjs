'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { Heights, Offsets, ROW_HEIGHT, revealScroll, windowOf } from '../virtual.js';

test('offsets stay exact for 100,000 rows with rows taller than a line', () => {
  const h = new Heights(100000);
  assert.equal(h.total, 100000 * ROW_HEIGHT);
  assert.equal(h.set(10, 64), true);
  assert.equal(h.set(10, 64), false, 'an equal height changes nothing');
  h.set(50000, 100);
  assert.equal(h.offset(10), 10 * ROW_HEIGHT);
  assert.equal(h.offset(11), 10 * ROW_HEIGHT + 64);
  assert.equal(h.offset(50001), 50001 * ROW_HEIGHT + (64 - ROW_HEIGHT) + (100 - ROW_HEIGHT));
  assert.equal(h.total, 100000 * ROW_HEIGHT + 36 + 72);
  // the row at an offset: the boundary belongs to the row that starts there
  assert.equal(h.indexAt(0), 0);
  assert.equal(h.indexAt(10 * ROW_HEIGHT), 10);
  assert.equal(h.indexAt(10 * ROW_HEIGHT + 63), 10);
  assert.equal(h.indexAt(10 * ROW_HEIGHT + 64), 11);
  assert.equal(h.indexAt(1e12), 99999, 'past the end clamps to the last row');
  for (const i of [0, 1, 9, 10, 11, 4095, 4096, 49999, 50000, 50001, 99999]) assert.equal(h.indexAt(h.offset(i)), i);
});

test('reset keeps the measured heights it is given', () => {
  const h = new Heights(0);
  assert.equal(h.total, 0);
  assert.equal(h.indexAt(50), 0);
  h.reset(5, ROW_HEIGHT, (i) => (i === 2 ? 90 : 0));
  assert.equal(h.get(2), 90);
  assert.equal(h.offset(5), 4 * ROW_HEIGHT + 90);
});

test('column offsets follow the widths', () => {
  const o = new Offsets([104, 40, 300]);
  assert.equal(o.total, 444);
  assert.equal(o.offset(2), 144);
  assert.equal(o.size(1), 40);
  assert.equal(o.indexAt(0), 0);
  assert.equal(o.indexAt(103), 0);
  assert.equal(o.indexAt(104), 1);
  assert.equal(o.indexAt(143), 1);
  assert.equal(o.indexAt(9999), 2);
});

test('the window covers the viewport plus overscan and starts under the frozen rows', () => {
  const h = new Heights(1000);
  // no frozen rows, scrolled to the top: rows 0 to 9 in view, 10 more drawn after
  assert.deepEqual(windowOf(h, 1000, 0, 0, 10 * ROW_HEIGHT, 10), { start: 0, end: 20 });
  // scrolled 100 rows down
  assert.deepEqual(windowOf(h, 1000, 0, 100 * ROW_HEIGHT, 10 * ROW_HEIGHT, 10), { start: 90, end: 120 });
  // 2 frozen rows: the scrolling pane starts at row 2 and never draws them again
  assert.deepEqual(windowOf(h, 1000, 2, 0, 10 * ROW_HEIGHT, 10), { start: 2, end: 22 });
  assert.deepEqual(windowOf(h, 1000, 2, 5 * ROW_HEIGHT, 10 * ROW_HEIGHT, 3), { start: 4, end: 20 });
  // the end of the tab
  assert.deepEqual(windowOf(h, 1000, 0, 995 * ROW_HEIGHT, 10 * ROW_HEIGHT, 10), { start: 985, end: 1000 });
  // every row frozen
  assert.deepEqual(windowOf(h, 2, 2, 0, 300, 10), { start: 2, end: 2 });
});

test('a tall row moves the window by its real height', () => {
  const h = new Heights(1000);
  h.set(0, 10 * ROW_HEIGHT);
  // one tall row fills the viewport
  assert.deepEqual(windowOf(h, 1000, 0, 0, 10 * ROW_HEIGHT, 0), { start: 0, end: 1 });
  assert.deepEqual(windowOf(h, 1000, 0, 10 * ROW_HEIGHT, 2 * ROW_HEIGHT, 0), { start: 1, end: 3 });
});

test('reveal scrolls only as far as needed and leaves frozen items alone', () => {
  const h = new Heights(1000);
  const view = 10 * ROW_HEIGHT;
  assert.equal(revealScroll(h, 5, 0, 0, view), 0, 'already in view');
  assert.equal(revealScroll(h, 10, 0, 0, view), ROW_HEIGHT, 'one row below the view');
  assert.equal(revealScroll(h, 3, 0, 20 * ROW_HEIGHT, view), 3 * ROW_HEIGHT, 'above the view');
  assert.equal(revealScroll(h, 1, 2, 500, view), 500, 'a frozen row is always in view');
  // under 2 frozen rows the scroll offset counts from row 2
  assert.equal(revealScroll(h, 2, 2, 5 * ROW_HEIGHT, view), 0);
});
