'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { edgeJump, gridKey } from '../keys.js';
import { HEAD_HEIGHT, HEAD_WIDTH, ROW_HEIGHT } from '../virtual.js';
import { fire } from './dom.mjs';
import { FakeServer, drawn, key, shownAt, startApp, type } from './server.mjs';

const big = (extra = {}) => new FakeServer({ tabs: [{ id: 1, name: 'Large', rows: 2000, cols: 40, cells: { '0,0': 'Name', '0,1': 'Qty', '1,0': 'a', '1,1': '5', '2,0': 'b', '2,1': '7', '3,0': 'c', '5,0': 'd', '1999,39': 'end' }, ...extra }] });
const rowsDrawn = (t) => [...t.grid.rowEls.keys()].map((id) => t.model.rowIndex(id)).sort((a, b) => a - b);
const colsDrawn = (t, r = 0) => t.grid.rowEls.get(t.model.rowAt(r))._c.map((el) => Number(el.getAttribute('aria-colindex')) - 2);
const scroll = (t, top, left = t.grid.el.scrollLeft) => { t.grid.el.scrollTop = top; t.grid.el.scrollLeft = left; t.grid.paint(); };

test('only the rows and columns in view are drawn, plus the overscan', async () => {
  const t = await startApp({ server: big(), width: HEAD_WIDTH + 5 * 104, height: HEAD_HEIGHT + 10 * ROW_HEIGHT });
  assert.deepEqual(rowsDrawn(t), Array.from({ length: 20 }, (_, i) => i), '10 rows in view and 10 after them');
  assert.deepEqual(colsDrawn(t), [0, 1, 2, 3, 4, 5, 6, 7], '5 columns in view and 3 after them');
  // the box inside the scroll container has the full size of the tab
  assert.equal(t.grid.size.style.height, `${HEAD_HEIGHT + 2000 * ROW_HEIGHT + 48}px`);
  assert.equal(t.grid.size.style.width, `${HEAD_WIDTH + 40 * 104 + 48}px`);
  scroll(t, 1000 * ROW_HEIGHT, 20 * 104);
  const rows = rowsDrawn(t);
  assert.equal(rows[0], 0, 'the active row stays drawn when it is scrolled out');
  assert.deepEqual(rows.slice(1), Array.from({ length: 30 }, (_, i) => 990 + i));
  assert.deepEqual(colsDrawn(t, 1000), [17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27]);
  const row = t.grid.rowEls.get(t.model.rowAt(1000));
  assert.equal(row.style.top, `${HEAD_HEIGHT + 1000 * ROW_HEIGHT}px`, 'a row is placed by its offset');
  assert.equal(row._c[0].style.left, `${HEAD_WIDTH + 17 * 104}px`);
});

test('row elements are recycled while scrolling', async () => {
  const t = await startApp({ server: big(), width: 600, height: HEAD_HEIGHT + 10 * ROW_HEIGHT });
  const before = new Set(t.grid.rowEls.values());
  scroll(t, 500 * ROW_HEIGHT);
  const reused = [...t.grid.rowEls.values()].filter((el) => before.has(el));
  assert.ok(reused.length >= 19, 'the rows that left the view are drawn again as other rows');
  assert.equal(t.grid.body.children.length, t.grid.rowEls.size, 'no stale row stays in the document');
  // rows are in the document in row order, so a reader meets them in order
  const order = t.grid.body.children.map((el) => Number(el.getAttribute('aria-rowindex')));
  assert.deepEqual(order, [...order].sort((a, b) => a - b));
});

test('frozen rows and columns stay drawn in the sticky blocks while the rest scrolls', async () => {
  const t = await startApp({ server: big({ frozen_rows: 1, frozen_cols: 2 }), width: HEAD_WIDTH + 6 * 104, height: HEAD_HEIGHT + 10 * ROW_HEIGHT });
  scroll(t, 300 * ROW_HEIGHT, 10 * 104);
  // the frozen row is one DOM row inside the block that sticks to the top
  assert.deepEqual(t.grid.top.children.map((el) => el.getAttribute('aria-rowindex')), ['1', '2']);
  assert.equal(t.grid.top.style.height, `${HEAD_HEIGHT + ROW_HEIGHT}px`);
  const frozenRow = t.grid.top.children[1];
  assert.equal(frozenRow._f.length, 2, 'it holds its frozen cells in the block that sticks to the left');
  assert.equal(shownAt(t, 0, 0), 'Name');
  assert.equal(frozenRow._f[1]._tx.textContent, 'Qty');
  assert.ok(frozenRow.classList.contains('fr-last'));
  // a scrolled row is one DOM row too: its number and frozen cells, then the cells in view
  const row = t.grid.rowEls.get(t.model.rowAt(300));
  assert.equal(row._fz.children.length, 3);
  assert.equal(row._fz.style.width, `${HEAD_WIDTH + 2 * 104}px`);
  assert.ok(row._f[1].classList.contains('fc-last'));
  // the window starts under and beside the frozen panes: 4 columns fit beside 2 frozen ones
  assert.deepEqual(colsDrawn(t, 300), [9, 10, 11, 12, 13, 14, 15, 16, 17, 18]);
  assert.ok(!rowsDrawn(t).slice(1).some((r) => r > 0 && r < 290));
  // the letters of frozen columns stay in the header's sticky block
  assert.deepEqual(t.grid.headRow._f.map((el) => el._tx.textContent), ['A', 'B']);
  assert.equal(t.grid.headRow._c[0]._tx.textContent, 'J');
});

test('a point of the viewport maps to a cell through the frozen panes and the scroll offsets', async () => {
  const t = await startApp({ server: big({ frozen_rows: 1, frozen_cols: 1 }), width: 800, height: 400 });
  scroll(t, 100 * ROW_HEIGHT, 5 * 104);
  const at = (x, y) => t.grid.hit(x, y);
  assert.deepEqual(at(HEAD_WIDTH + 10, HEAD_HEIGHT + 5), { zone: 'cell', r: 0, c: 0 }, 'the frozen corner cell');
  assert.deepEqual(at(HEAD_WIDTH + 104 + 10, HEAD_HEIGHT + 5), { zone: 'cell', r: 0, c: 6 }, 'the frozen row, scrolled columns');
  assert.deepEqual(at(HEAD_WIDTH + 10, HEAD_HEIGHT + ROW_HEIGHT + 5), { zone: 'cell', r: 101, c: 0 }, 'the frozen column, scrolled rows');
  assert.deepEqual(at(HEAD_WIDTH + 104 + 10, HEAD_HEIGHT + ROW_HEIGHT + 5), { zone: 'cell', r: 101, c: 6 });
  assert.deepEqual(at(10, HEAD_HEIGHT + ROW_HEIGHT + 5), { zone: 'row', r: 101, c: 0 });
  assert.deepEqual(at(HEAD_WIDTH + 104 + 50, 5), { zone: 'col', r: 0, c: 6 });
  assert.deepEqual(at(HEAD_WIDTH + 104 - 2, 5), { zone: 'resize', r: 0, c: 0 }, 'the edge of a letter');
  assert.deepEqual(at(5, 5), { zone: 'corner', r: 0, c: 0 });
  const rect = t.grid.cellRect(101, 6);
  assert.deepEqual([rect.left, rect.top, rect.width, rect.height], [HEAD_WIDTH + 104, HEAD_HEIGHT + ROW_HEIGHT, 104, ROW_HEIGHT]);
});

test('text runs over blank cells to its right and stops at a filled one; numbers never run on', async () => {
  const cells = {
    '0,0': 'This text is longer than one column', // 35 characters, 245 px + padding: runs over B and C
    '1,0': 'This text is longer than one column', '1,1': 'x',
    '2,0': '123456789012345', // a number of 15 digits, 105 px: does not fit 104 px less its padding
    '3,0': '12345',
    '4,0': { kind: 'b', value: true }, '5,0': { kind: 'e', value: '#DIV/0!' },
  };
  const t = await startApp({ server: new FakeServer({ tabs: [{ id: 1, name: 'T', rows: 10, cols: 6, cells }] }) });
  t.sel.set(9, 5);
  t.grid.paint();
  const row0 = t.grid.rowEls.get(t.model.rowAt(0))._c;
  assert.ok(row0[0].classList.contains('spill'));
  assert.equal(row0[0]._tx.style.width, `${35 * 7 + 12}px`, 'the text gets the room it needs, over B and into C');
  assert.ok(row0[1].classList.contains('cov'), 'the gridline under the text is hidden');
  assert.equal(row0[2].classList.contains('cov'), false, 'the last covered cell keeps its right edge');
  const row1 = t.grid.rowEls.get(t.model.rowAt(1))._c;
  assert.equal(row1[0].classList.contains('spill'), false, 'a filled neighbour stops it');
  assert.equal(row1[0]._tx.textContent, 'This text is longer than one column', 'the text is clipped, not shortened');
  assert.equal(shownAt(t, 2, 0), '####', 'a number that does not fit never shows as a shorter number');
  assert.equal(t.model.shown(t.model.rowAt(2), t.model.colAt(0)), '123456789012345', 'the model keeps what the server sent');
  assert.equal(shownAt(t, 3, 0), '12345');
  // general alignment: text left, numbers right, booleans and errors centered
  assert.ok(drawn(t, 3, 0).classList.contains('num'));
  assert.ok(drawn(t, 4, 0).classList.contains('ctr'));
  assert.ok(drawn(t, 5, 0).classList.contains('ctr'));
  assert.ok(drawn(t, 5, 0).classList.contains('err'));
  assert.equal(drawn(t, 1, 1).className, 'gc');
  // a wider column shows the number again
  t.server.push({ tab: 1, widths: [[t.model.colAt(0), 200]] });
  assert.equal(shownAt(t, 2, 0), '123456789012345');
});

test('text that starts left of the drawn columns still runs into them', async () => {
  const t = await startApp({ server: new FakeServer({ tabs: [{ id: 1, name: 'T', rows: 10, cols: 40, cells: { '2,1': 'x'.repeat(400) } }] }), width: HEAD_WIDTH + 3 * 104, height: 300 });
  t.sel.set(9, 20);
  scroll(t, 0, 20 * 104);
  const cells = t.grid.rowEls.get(t.model.rowAt(2))._c;
  assert.equal(cells[0].getAttribute('aria-colindex'), '3', 'column B is drawn although it is scrolled out');
  assert.ok(cells[0].classList.contains('spill'));
  assert.ok(cells[1].classList.contains('cov'));
});

test('a row with wrapped text or line breaks is measured when drawn, and offsets follow', async () => {
  const cells = { '1,0': 'two\nlines', '3,0': { kind: 't', value: 'wrapped text of several lines', style: 1 } };
  const server = new FakeServer({ tabs: [{ id: 1, name: 'T', rows: 500, cols: 4, cells }] });
  server.tabs[0].styles.push({ wrap: true });
  const t = await startApp({ server, height: HEAD_HEIGHT + 10 * ROW_HEIGHT });
  t.grid.measure.height = (el) => (el.textContent.includes('wrapped') ? 80 : el.textContent.split('\n').length * 18 + 10);
  t.grid.measured.clear();
  t.grid.layout();
  t.grid.paint();
  const h = t.grid.heights;
  assert.equal(h.get(0), ROW_HEIGHT);
  assert.equal(h.get(1), 2 * 18 + 10 + 1);
  assert.equal(h.get(3), 81);
  assert.ok(drawn(t, 3, 0).classList.contains('wrap'));
  assert.ok(drawn(t, 1, 0).classList.contains('ml'));
  const top = (r) => t.grid.rowEls.get(t.model.rowAt(r)).style.top;
  assert.equal(top(2), `${HEAD_HEIGHT + ROW_HEIGHT + 47}px`);
  assert.equal(top(4), `${HEAD_HEIGHT + 2 * ROW_HEIGHT + 47 + 81}px`, 'rows below a tall row move down by its height');
  assert.equal(t.grid.size.style.height, `${HEAD_HEIGHT + 498 * ROW_HEIGHT + 47 + 81 + 48}px`);
  // scrolling far away keeps the measured heights, so offsets stay exact
  scroll(t, 300 * ROW_HEIGHT);
  assert.equal(top(300), `${HEAD_HEIGHT + 298 * ROW_HEIGHT + 47 + 81}px`);
  // the content loses its line break: the row is one line again
  t.server.setCell(1, 1, 0, 'one line');
  scroll(t, 0);
  assert.equal(h.get(1), ROW_HEIGHT);
  // rows inserted above keep the tall row's height on its new position
  t.server.insertRows(1, 0, 2);
  assert.equal(h.get(5), 81);
  assert.equal(h.get(3), ROW_HEIGHT);
});

test('scrolling sideways keeps row heights: a tall cell counts whether or not its column is drawn', async () => {
  // Row 2 holds two lines in A and plain text in B; row 4 holds three lines
  // far to the right, in a column the first paint does not draw; row 1 is
  // frozen and holds two lines in A. The active cell is on another row.
  const cells = { '0,0': 'frozen\ntwo', '1,0': 'two\nlines', '1,1': 'plain', '3,50': 'far\nto the\nright', '5,0': 'one line' };
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Wide', rows: 300, cols: 60, cells, frozen_rows: 1 }] });
  const t = await startApp({ server, width: HEAD_WIDTH + 5 * 104, height: HEAD_HEIGHT + 12 * ROW_HEIGHT });
  t.sel.set(8, 0);
  t.grid.paint();
  const h = t.grid.heights;
  const two = 2 * 18 + 10 + 1;
  const three = 3 * 18 + 10 + 1;
  const geometry = () => ({
    heights: [0, 1, 2, 3, 4, 5].map((r) => h.get(r)),
    tops: [1, 2, 4, 5, 9].map((r) => t.grid.rowEls.get(t.model.rowAt(r)).style.top),
    frozen: t.grid.top.style.height,
    total: t.grid.size.style.height,
  });
  const first = geometry();
  assert.deepEqual(first.heights, [two, two, ROW_HEIGHT, three, ROW_HEIGHT, ROW_HEIGHT], 'the cell far to the right gives its row its height before it is ever drawn');
  assert.equal(first.frozen, `${HEAD_HEIGHT + two}px`);
  assert.equal(t.grid.rowEls.get(t.model.rowAt(3))._c.some((el) => el.getAttribute('aria-colindex') === '52'), false, 'that column is not drawn');
  // far to the right: column A is neither drawn nor the nearest text cell on the left of row 2
  scroll(t, 0, 30 * 104);
  assert.deepEqual(colsDrawn(t, 1).includes(0), false);
  assert.deepEqual(geometry(), first, 'nothing moved');
  // to the column of the far cell, and back
  scroll(t, 0, 48 * 104);
  assert.equal(drawn(t, 3, 50)._tx.textContent, 'far\nto the\nright');
  assert.deepEqual(geometry(), first);
  scroll(t, 0, 0);
  assert.deepEqual(geometry(), first);
  // a hit lands on the same row at the same height wherever the grid is scrolled
  const yOfRow5 = HEAD_HEIGHT + two + two + ROW_HEIGHT + three + ROW_HEIGHT + 4;
  for (const left of [0, 30 * 104, 48 * 104]) {
    scroll(t, 0, left);
    assert.equal(t.grid.hit(HEAD_WIDTH + 10, yOfRow5).r, 5);
  }
  // content is what changes a height: the far cell loses its line breaks while its column is out of view
  scroll(t, 0, 0);
  t.server.setCell(1, 3, 50, 'short');
  assert.equal(h.get(3), ROW_HEIGHT, 'the row is one line again without scrolling there');
  // and gains them back, still out of view
  t.server.setCell(1, 3, 50, 'a\nb');
  assert.equal(h.get(3), two);
  // a width is the other thing: a wrapped cell out of view is measured again at its new width
  t.grid.measure.height = (el) => (el.textContent === 'wrapped' ? (el.parentNode.style.width === '300px' ? 40 : 90) : el.textContent.split('\n').length * 18 + 10);
  t.server.styles.push({ wrap: true });
  t.server.push({ tab: 1, styles: [{ wrap: true }], cells: [[t.model.rowAt(5), [[t.model.colAt(50), 't', 'wrapped', null, null, 1]]]] });
  assert.equal(h.get(5), 91);
  t.server.push({ tab: 1, widths: [[t.model.colAt(50), 300]] });
  assert.equal(h.get(5), 41);
  // the hidden cells that measured them hold no text a reader could meet
  assert.equal(t.grid.probeBox.getAttribute('aria-hidden'), 'true');
  assert.ok(t.grid.probeBox.children.every((el) => el.hidden));
});

test('a refused text edit returns its row to the confirmed height, also while its column is out of view', async () => {
  const server = new FakeServer({ tabs: [{ id: 1, name: 'T', rows: 30, cols: 60, cells: { '1,0': 'one line', '1,1': 'plain' } }] });
  const t = await startApp({ server, width: HEAD_WIDTH + 5 * 104, height: HEAD_HEIGHT + 10 * ROW_HEIGHT });
  server.manual.add('action');
  t.sel.set(1, 0);
  t.app.changed();
  type(t, 'typed');
  key(t, 'Enter', { altKey: true });
  type(t, 'with');
  key(t, 'Enter', { altKey: true });
  type(t, 'breaks');
  key(t, 'Enter');
  t.sel.set(6, 0);
  t.grid.paint();
  assert.equal(shownAt(t, 1, 0), 'typed\nwith\nbreaks');
  assert.equal(t.grid.heights.get(1), 3 * 18 + 10 + 1, 'the pending text shows with its lines');
  scroll(t, 0, 30 * 104);
  assert.equal(t.grid.heights.get(1), 3 * 18 + 10 + 1, 'and keeps them while its column is out of view');
  server.socket.recv({ type: 'ack', n: '1', seq: '10', results: [{ status: 'refused', reason: 'invalid' }] });
  assert.equal(t.grid.heights.get(1), ROW_HEIGHT, 'the refusal returns the row to one line at once');
  scroll(t, 0, 0);
  assert.equal(shownAt(t, 1, 0), 'one line');
});

test('the selection is drawn as one range with the active cell clear inside it', async () => {
  const t = await startApp({ server: big() });
  key(t, 'ArrowDown');
  key(t, 'ArrowDown', { shiftKey: true });
  key(t, 'ArrowRight', { shiftKey: true });
  assert.deepEqual(t.sel.box(), { r1: 1, c1: 0, r2: 2, c2: 1 });
  assert.deepEqual(t.sel.active(), { r: 1, c: 0 });
  const cls = (r, c) => t.grid.rowEls.get(t.model.rowAt(r))._c[c].className;
  assert.equal(cls(1, 1), 'gc num sel s-t s-r');
  assert.equal(cls(2, 0), 'gc sel s-b s-l');
  assert.equal(cls(2, 1), 'gc num sel s-b s-r');
  assert.equal(cls(3, 0), 'gc');
  assert.equal(t.grid.activeEl.className, 'gc act sel s-t s-l');
  assert.equal(t.grid.activeEl.parentNode, t.grid.rowEls.get(t.model.rowAt(1)));
  // the recycled cell under the active one is an empty placeholder
  assert.equal(cls(1, 0), 'gc ph sel');
  assert.equal(t.grid.rowEls.get(t.model.rowAt(1))._c[0].getAttribute('aria-hidden'), 'true');
  // headers of the selected rows and columns are marked, and the name box names the range
  assert.ok(t.grid.rowEls.get(t.model.rowAt(2))._rh.classList.contains('on'));
  assert.ok(t.grid.headRow._c[1].classList.contains('on'));
  assert.equal(t.grid.headRow._c[2].classList.contains('on'), false);
  assert.equal(t.app.nameBox.textContent, 'A2:B3');
  // the fill handle sits at the corner of the selection
  assert.ok(t.grid.rowEls.get(t.model.rowAt(2))._c[1].querySelector('.handle'));
});

test('pointer selection: click, Shift-click, drag, headers and the corner', async () => {
  const t = await startApp({ server: big() });
  const ev = (props = {}) => ({ preventDefault() {}, ...props });
  const h = t.grid.h;
  h.down({ zone: 'cell', r: 2, c: 1 }, ev());
  h.up(ev());
  assert.deepEqual(t.sel.box(), { r1: 2, c1: 1, r2: 2, c2: 1 });
  h.down({ zone: 'cell', r: 5, c: 3 }, ev({ shiftKey: true }));
  h.up(ev());
  assert.deepEqual(t.sel.box(), { r1: 2, c1: 1, r2: 5, c2: 3 });
  h.down({ zone: 'cell', r: 1, c: 0 }, ev());
  h.drag({ zone: 'cell', r: 3, c: 2 }, ev());
  h.up(ev());
  assert.deepEqual(t.sel.box(), { r1: 1, c1: 0, r2: 3, c2: 2 });
  h.down({ zone: 'col', r: 0, c: 2 }, ev());
  h.drag({ zone: 'col', r: 0, c: 4 }, ev());
  h.up(ev());
  assert.deepEqual(t.sel.box(), { r1: 0, c1: 2, r2: 1999, c2: 4 }, 'whole columns');
  assert.equal(t.sel.kind, 'cols');
  h.down({ zone: 'row', r: 7, c: 0 }, ev());
  h.down({ zone: 'row', r: 9, c: 0 }, ev({ shiftKey: true }));
  h.up(ev());
  assert.deepEqual(t.sel.box(), { r1: 7, c1: 0, r2: 9, c2: 39 }, 'whole rows');
  h.down({ zone: 'corner', r: 0, c: 0 }, ev());
  assert.deepEqual(t.sel.box(), { r1: 0, c1: 0, r2: 1999, c2: 39 });
  assert.ok(t.grid.corner.classList.contains('on'));
});

test('the selection stays on its cells when rows are inserted above or sorted', async () => {
  const t = await startApp({ server: big() });
  t.sel.range(1, 0, 2, 1);
  const rows = [t.model.rowAt(1), t.model.rowAt(2)];
  t.server.insertRows(1, 0, 3);
  assert.deepEqual(t.sel.box(), { r1: 4, c1: 0, r2: 5, c2: 1 });
  assert.equal(t.model.rowAt(4), rows[0]);
  // the selected row is deleted: the selection keeps the position it had
  t.server.deleteRows(1, 4, 1);
  assert.deepEqual(t.sel.active(), { r: 4, c: 0 });
});

test('keyboard movement: arrows, Tab, Home, End, Page keys and Shift to extend (rule 21)', async () => {
  const t = await startApp({ server: big(), height: HEAD_HEIGHT + 10 * ROW_HEIGHT });
  const at = () => t.sel.active();
  key(t, 'ArrowRight');
  key(t, 'ArrowDown');
  assert.deepEqual(at(), { r: 1, c: 1 });
  key(t, 'ArrowLeft');
  key(t, 'ArrowUp');
  key(t, 'ArrowUp');
  assert.deepEqual(at(), { r: 0, c: 0 }, 'movement stops at the edge of the tab');
  // Tab moves one cell right and Shift+Tab one cell left, and never leaves the grid
  assert.equal(key(t, 'Tab').defaultPrevented, true);
  assert.deepEqual(at(), { r: 0, c: 1 });
  key(t, 'Tab', { shiftKey: true });
  key(t, 'Tab', { shiftKey: true });
  assert.deepEqual(at(), { r: 0, c: 0 });
  assert.equal(t.doc.activeElement, t.grid.input);
  key(t, 'PageDown');
  assert.deepEqual(at(), { r: 9, c: 0 }, 'a page is the rows in view less one');
  key(t, 'PageUp');
  assert.deepEqual(at(), { r: 0, c: 0 });
  key(t, 'ArrowRight');
  key(t, 'ArrowRight');
  key(t, 'Home');
  assert.deepEqual(at(), { r: 0, c: 0 });
  key(t, 'End');
  assert.deepEqual(at(), { r: 0, c: 1 }, 'End goes to the last filled cell of the row');
  key(t, 'End', { ctrlKey: true });
  assert.deepEqual(at(), { r: 1999, c: 39 }, 'Ctrl+End goes to the last filled cell of the tab');
  assert.ok(t.grid.el.scrollTop > 1900 * ROW_HEIGHT, 'the grid scrolled to show it');
  key(t, 'Home', { metaKey: true });
  assert.deepEqual(at(), { r: 0, c: 0 });
  assert.equal(t.grid.el.scrollTop, 0);
  // Shift extends from the active cell
  key(t, 'ArrowDown', { shiftKey: true });
  key(t, 'PageDown', { shiftKey: true });
  assert.deepEqual(t.sel.box(), { r1: 0, c1: 0, r2: 10, c2: 0 });
  assert.deepEqual(at(), { r: 0, c: 0 });
  key(t, 'a', { ctrlKey: true });
  assert.equal(t.sel.kind, 'all');
  key(t, ' ', { shiftKey: true });
  assert.equal(t.sel.kind, 'rows');
});

test('Ctrl or Cmd with an arrow jumps to the edge of the data', async () => {
  const t = await startApp({ server: big() });
  const at = () => t.sel.active();
  // column A holds rows 0 to 3, a gap, then row 5
  key(t, 'ArrowDown', { ctrlKey: true });
  assert.deepEqual(at(), { r: 3, c: 0 }, 'to the end of the filled run');
  key(t, 'ArrowDown', { metaKey: true });
  assert.deepEqual(at(), { r: 5, c: 0 }, 'over the gap to the next filled cell');
  key(t, 'ArrowDown', { ctrlKey: true });
  assert.deepEqual(at(), { r: 1999, c: 0 }, 'to the edge of the tab when nothing follows');
  key(t, 'ArrowUp', { ctrlKey: true });
  assert.deepEqual(at(), { r: 5, c: 0 });
  key(t, 'ArrowUp', { ctrlKey: true, shiftKey: true });
  assert.deepEqual(t.sel.box(), { r1: 3, c1: 0, r2: 5, c2: 0 }, 'with Shift the range extends to the jump');
  // the pure function, on a small picture
  const filled = (r, c) => ['0,0', '0,1', '0,2', '0,5'].includes(`${r},${c}`);
  assert.deepEqual(edgeJump(filled, 3, 8, 0, 0, 0, 1), { r: 0, c: 2 });
  assert.deepEqual(edgeJump(filled, 3, 8, 0, 2, 0, 1), { r: 0, c: 5 });
  assert.deepEqual(edgeJump(filled, 3, 8, 0, 5, 0, 1), { r: 0, c: 7 });
  assert.deepEqual(edgeJump(filled, 3, 8, 0, 0, 0, -1), { r: 0, c: 0 });
});

test('keys that type or belong to the browser are left alone', () => {
  const k = (key, props = {}) => gridKey({ key, ...props });
  assert.equal(k('a'), null, 'a letter types into the text area');
  assert.equal(k('='), null);
  assert.equal(k('c', { ctrlKey: true }), null, 'copy is the browser\'s');
  assert.equal(k('Enter', { isComposing: true }), null);
  assert.equal(k('ArrowDown', { keyCode: 229 }), null);
  assert.equal(k('Tab', { ctrlKey: true }), null);
  assert.deepEqual(k('F10', { shiftKey: true }), { type: 'menu' });
  assert.equal(k('F10'), null);
  assert.deepEqual(k('v', { metaKey: true, shiftKey: true }), { type: 'pasteValues' });
  assert.equal(k('v', { metaKey: true }), null);
});

test('a drag held past the edge of the viewport scrolls the grid and extends the selection', async () => {
  const width = HEAD_WIDTH + 5 * 104;
  const height = HEAD_HEIGHT + 10 * ROW_HEIGHT;
  const t = await startApp({ server: big(), width, height });
  const el = t.grid.el;
  const at = (r, c) => ({ clientX: HEAD_WIDTH + c * 104 + 10, clientY: HEAD_HEIGHT + r * ROW_HEIGHT + 5, button: 0 });
  fire(el, 'mousedown', at(2, 1));
  fire(t.doc, 'mousemove', at(4, 2));
  assert.deepEqual(t.sel.box(), { r1: 2, c1: 1, r2: 4, c2: 2 });
  assert.equal(el.scrollTop, 0, 'a drag inside the viewport scrolls nothing');
  // the pointer rests 40 pixels under the grid: every step scrolls on
  fire(t.doc, 'mousemove', { clientX: at(4, 2).clientX, clientY: height + 40, button: 0 });
  const first = el.scrollTop;
  assert.ok(first > 0);
  t.timers.advance(300);
  assert.ok(el.scrollTop > first + 100, `the grid went on scrolling: ${el.scrollTop}`);
  const b = t.sel.box();
  assert.equal(b.r1, 2);
  assert.ok(b.r2 > 14, `the selection follows the last row in view: ${b.r2}`);
  assert.equal(el.scrollLeft, 0);
  // past the right edge it scrolls sideways as well
  fire(t.doc, 'mousemove', { clientX: width + 30, clientY: height + 40, button: 0 });
  t.timers.advance(90);
  assert.ok(el.scrollLeft > 0);
  assert.ok(t.sel.box().c2 > 4);
  // letting go stops it
  fire(t.doc, 'mouseup', { clientX: width + 30, clientY: height + 40, button: 0 });
  const rest = [el.scrollTop, el.scrollLeft];
  t.timers.advance(600);
  assert.deepEqual([el.scrollTop, el.scrollLeft], rest);
});

test('a drag on row numbers scrolls up and down only, and never past the end of the tab', async () => {
  const height = HEAD_HEIGHT + 10 * ROW_HEIGHT;
  const t = await startApp({ server: new FakeServer({ tabs: [{ id: 1, name: 'Short', rows: 30, cols: 40 }] }), width: HEAD_WIDTH + 5 * 104, height });
  const el = t.grid.el;
  fire(el, 'mousedown', { clientX: 10, clientY: HEAD_HEIGHT + 5, button: 0 });
  assert.equal(t.sel.kind, 'rows');
  fire(t.doc, 'mousemove', { clientX: 10, clientY: height + 200, button: 0 });
  t.timers.advance(5000);
  assert.equal(el.scrollLeft, 0, 'the pointer left of the cells does not scroll sideways');
  assert.equal(el.scrollTop, HEAD_HEIGHT + 30 * ROW_HEIGHT + 48 - height, 'the scroll stops at the end of the tab');
  assert.deepEqual([t.sel.box().r1, t.sel.box().r2], [0, 29]);
  assert.equal(t.grid.dragTimer, null, 'nothing is left running at the end');
  fire(t.doc, 'mouseup', { clientX: 10, clientY: height + 200, button: 0 });
});
