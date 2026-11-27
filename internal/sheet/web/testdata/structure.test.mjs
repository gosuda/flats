'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { freeName, tabNameError } from '../tabs.js';
import { fire } from './dom.mjs';
import { FakeServer, find, key, notices, shownAt, startApp, type } from './server.mjs';

const sheet = () => new FakeServer({ tabs: [
  { id: 1, name: 'Budget', rows: 30, cols: 8, cells: { '0,0': 'Item', '0,1': 'Amount', '1,0': 'Venue', '1,1': '1200', '2,0': 'A much longer item name', '2,1': '900', '3,0': 'Bus', '3,1': '=B2+B3' } },
  { id: 2, name: 'Notes', rows: 5, cols: 3 },
] });
const lastOps = (t) => t.server.socket.frames('action').at(-1).ops;
const ev = (props = {}) => ({ preventDefault() {}, ...props });
const colsDrawnIn = (t) => t.grid.rowEls.get(t.model.rowAt(1))._c.map((el) => Number(el.getAttribute('aria-colindex')) - 2);
const popup = (t) => t.doc.querySelector('.menu') || t.doc.querySelector('.pop:not([hidden])');

test('the toolbar sets every mark, alignment and wrap on the selection by name', async () => {
  const t = await startApp({ server: sheet() });
  const m = t.model;
  t.server.manual.add('action');
  t.sel.range(1, 0, 2, 1);
  t.app.changed();
  const range = { rows: [[m.rowAt(1), 2]], cols: [[m.colAt(0), 2]] };
  const press = (label) => { find(t.app.toolbar.el, label).click(); return lastOps(t)[0]; };
  assert.deepEqual(press('Bold (Ctrl or Cmd+B)'), { op: 'format', tab: 1, ...range, set: { bold: true } });
  assert.deepEqual(press('Italic (Ctrl or Cmd+I)').set, { italic: true });
  assert.deepEqual(press('Underline (Ctrl or Cmd+U)').set, { underline: true });
  assert.deepEqual(press('Strikethrough').set, { strikethrough: true });
  assert.deepEqual(press('Align left').set, { align: 'left' });
  assert.deepEqual(press('Align center').set, { align: 'center' });
  assert.deepEqual(press('Align right').set, { align: 'right' });
  assert.deepEqual(press('Wrap text').set, { wrap: true });
  key(t, 'i', { metaKey: true });
  assert.deepEqual(lastOps(t)[0].set, { italic: true });
  key(t, 'u', { ctrlKey: true });
  assert.deepEqual(lastOps(t)[0].set, { underline: true });
  assert.equal(t.doc.activeElement, t.grid.input);
});

test('a mark that is on is taken off, and the toolbar shows the active cell\'s format', async () => {
  const server = sheet();
  server.tabs[0].styles.push({ bold: true, align: 'center', wrap: true, text_color: 'red-600', fill: 'yellow-100', number: { kind: 'currency', symbol: '$', decimals: 2 } });
  server.tabs[0].cells.get(`${server.tabs[0].rows[1]}:${server.tabs[0].cols[1]}`).style = 1;
  const t = await startApp({ server });
  t.sel.set(1, 1);
  t.app.changed();
  const bar = t.app.toolbar.el;
  assert.equal(find(bar, 'Bold (Ctrl or Cmd+B)').getAttribute('aria-pressed'), 'true');
  assert.equal(find(bar, 'Align center').getAttribute('aria-pressed'), 'true');
  assert.equal(find(bar, 'Wrap text').getAttribute('aria-pressed'), 'true');
  assert.equal(bar.querySelector('select').value, 'currency');
  assert.ok(find(bar, 'Text color').querySelector('.sw-red-600'), 'the chip shows the color by its token class');
  assert.ok(find(bar, 'Fill color').querySelector('.sw-yellow-100'));
  // the cell is drawn with its format through classes and palette tokens
  const cell = t.grid.activeEl;
  assert.ok(['b', 'ctr', 'wrap'].every((c) => cell.classList.contains(c)));
  assert.equal(cell.style.color, 'var(--c-red-600)');
  assert.equal(cell.style.backgroundColor, 'var(--c-yellow-100)');
  find(bar, 'Bold (Ctrl or Cmd+B)').click();
  assert.deepEqual(lastOps(t)[0].set, { bold: false });
  find(bar, 'Align center').click();
  assert.deepEqual(lastOps(t)[0].set, { align: null }, 'back to general alignment');
});

test('number formats: the kind list, the options dialog and the decimals buttons', async () => {
  const t = await startApp({ server: sheet() });
  t.sel.set(1, 1);
  t.app.changed();
  const bar = t.app.toolbar.el;
  const select = bar.querySelector('select');
  assert.deepEqual(select.children.map((o) => o.textContent), ['General', 'Number', 'Percent', 'Currency', 'Date', 'Time', 'Date and time', 'Text']);
  select.value = 'percent';
  fire(select, 'change');
  assert.deepEqual(lastOps(t)[0].set, { number: { kind: 'percent', decimals: 2 } });
  find(bar, 'More decimals').click();
  assert.deepEqual(lastOps(t)[0].set, { number: { kind: 'percent', decimals: 3 } });
  find(bar, 'Fewer decimals').click();
  find(bar, 'Fewer decimals').click();
  assert.deepEqual(lastOps(t)[0].set, { number: { kind: 'percent', decimals: 1 } });
  t.sel.set(2, 1);
  t.app.changed();
  find(bar, 'More decimals').click();
  assert.deepEqual(lastOps(t)[0].set, { number: { kind: 'number', decimals: 2, thousands: true } }, 'a cell without decimals to change becomes a number');
  t.sel.set(1, 1);
  t.app.changed();
  // the options dialog names every setting of rule 6
  find(bar, 'Format options: decimals, separator, currency symbol, date and time forms').click();
  const d = popup(t);
  assert.equal(d.getAttribute('aria-label'), 'Format options');
  const field = (id) => d.querySelectorAll('select, input').find((el) => el.id === id);
  const rowOf = (id) => d.querySelectorAll('label').find((l) => l.getAttribute('for') === id);
  field('nf-kind').value = 'currency';
  fire(field('nf-kind'), 'change');
  assert.equal(rowOf('nf-sym').hidden, false);
  assert.equal(rowOf('nf-date').hidden, true, 'only the settings of the kind are shown');
  field('nf-sym').value = 'CHF';
  field('nf-dec').value = '0';
  find(d, 'Apply').click();
  assert.deepEqual(lastOps(t)[0].set, { number: { kind: 'currency', symbol: 'CHF', decimals: 0 } });
  // a setting out of range is refused in the dialog
  find(bar, 'Format options: decimals, separator, currency symbol, date and time forms').click();
  const again = popup(t);
  const kind = again.querySelectorAll('select').find((el) => el.id === 'nf-kind');
  kind.value = 'number';
  fire(kind, 'change');
  again.querySelectorAll('input').find((el) => el.id === 'nf-dec').value = '11';
  const sent = t.server.socket.frames('action').length;
  find(again, 'Apply').click();
  assert.equal(t.server.socket.frames('action').length, sent);
  assert.match(again.querySelector('[role="alert"]').textContent, /Decimals go from 0 to 10/);
  // date and time forms
  kind.value = 'datetime';
  fire(kind, 'change');
  again.querySelectorAll('select').find((el) => el.id === 'nf-date').value = 'mmm d, yyyy';
  again.querySelectorAll('select').find((el) => el.id === 'nf-time').value = 'h:mm AM/PM';
  find(again, 'Apply').click();
  assert.deepEqual(lastOps(t)[0].set, { number: { kind: 'datetime', date: 'mmm d, yyyy', time: 'h:mm AM/PM' } });
});

test('the palette offers the 54 colors of rule 6 and none', async () => {
  const t = await startApp({ server: sheet() });
  find(t.app.toolbar.el, 'Fill color').click();
  const pal = popup(t);
  assert.equal(pal.getAttribute('aria-label'), 'Fill color');
  const swatches = pal.querySelectorAll('.sw');
  assert.equal(swatches.length, 54);
  assert.ok(swatches.every((s) => s.getAttribute('aria-label')), 'each color has a name, so color is not the only signal');
  find(pal, 'teal 300').click();
  assert.deepEqual(lastOps(t)[0].set, { fill: 'teal-300' });
  find(t.app.toolbar.el, 'Text color').click();
  find(popup(t), 'None').click();
  assert.deepEqual(lastOps(t)[0].set, { text_color: null });
});

test('rows and columns are inserted as many as are selected, beside the selection, and deleted by id', async () => {
  const t = await startApp({ server: sheet() });
  const m = t.model;
  t.sel.range(1, 1, 2, 2);
  const [r1, r2, c1, c2] = [m.rowAt(1), m.rowAt(2), m.colAt(1), m.colAt(2)];
  const pick = (label) => { t.app.rowsMenu({ x: 0, y: 0 }); find(popup(t), label).click(); return lastOps(t)[0]; };
  t.server.manual.add('action');
  assert.deepEqual(pick('Insert 2 rows above'), { op: 'insert_rows', tab: 1, beside: r1, side: 'before', count: 2 });
  assert.deepEqual(pick('Insert 2 rows below'), { op: 'insert_rows', tab: 1, beside: r2, side: 'after', count: 2 });
  assert.deepEqual(pick('Insert 2 columns left'), { op: 'insert_cols', tab: 1, beside: c1, side: 'before', count: 2 });
  assert.deepEqual(pick('Insert 2 columns right'), { op: 'insert_cols', tab: 1, beside: c2, side: 'after', count: 2 });
  assert.deepEqual(pick('Delete 2 rows'), { op: 'delete_rows', tab: 1, ids: [[r1, 2]] });
  assert.deepEqual(pick('Delete 2 columns'), { op: 'delete_cols', tab: 1, ids: [[c1, 2]] });
  assert.deepEqual(pick('Add 100 rows at the end'), { op: 'append_rows', tab: 1, count: 100 });
  assert.deepEqual(pick('Add a column at the end'), { op: 'append_cols', tab: 1, count: 1 });
});

test('the menu of a row number or a column letter opens for that line', async () => {
  const t = await startApp({ server: sheet() });
  t.grid.h.context({ zone: 'row', r: 4, c: 0 }, ev({ clientX: 10, clientY: 150 }));
  assert.deepEqual(t.sel.box(), { r1: 4, c1: 0, r2: 4, c2: 7 });
  const labels = popup(t).querySelectorAll('[role="menuitem"]').map((b) => b.textContent);
  assert.ok(labels.includes('Insert 1 row above'));
  assert.equal(labels.some((l) => l.startsWith('Insert 1 column')), false, 'a row menu has no column commands');
  t.app.popups.close();
  // a right click inside a selection keeps it
  t.sel.range(1, 1, 3, 2);
  t.grid.h.context({ zone: 'cell', r: 2, c: 2 }, ev({ clientX: 10, clientY: 10 }));
  assert.deepEqual(t.sel.box(), { r1: 1, c1: 1, r2: 3, c2: 2 });
  assert.ok(find(popup(t), 'Insert 3 rows above'));
});

test('the whole tab cannot lose its last row or column', async () => {
  const t = await startApp({ server: sheet() });
  t.sel.all();
  t.app.deleteRows();
  t.app.deleteCols();
  assert.equal(t.server.socket.frames('action').length, 0);
  assert.deepEqual(notices(t).map((n) => n[1]), ['A tab keeps at least one row.', 'A tab keeps at least one column.']);
});

test('sort: one selected cell sorts every row under the frozen rows, several rows sort those', async () => {
  const t = await startApp({ server: sheet() });
  const m = t.model;
  t.server.manual.add('action');
  t.sel.set(2, 1);
  t.app.changed();
  find(t.app.toolbar.el, 'Sort ascending by the active column').click();
  assert.deepEqual(lastOps(t)[0], { op: 'sort', tab: 1, rows: 'all', by: m.colAt(1), desc: false });
  find(t.app.toolbar.el, 'Sort descending by the active column').click();
  assert.equal(lastOps(t)[0].desc, true);
  t.sel.range(1, 0, 3, 1);
  t.app.changed();
  find(t.app.toolbar.el, 'Sort…').click();
  const d = popup(t);
  assert.match(d.textContent, /Reorders rows 2 to 4\. Whole rows move; frozen rows stay\./);
  const [col, dir] = d.querySelectorAll('select');
  assert.deepEqual(col.children.map((o) => o.textContent), ['Column A', 'Column B'], 'the columns of the selection');
  col.value = '1';
  dir.value = 'desc';
  find(d, 'Sort').click();
  assert.deepEqual(lastOps(t)[0], { op: 'sort', tab: 1, rows: [[m.rowAt(1), 3]], by: m.colAt(1), desc: true });
});

test('a sort moves rows with their cells on every screen', async () => {
  const t = await startApp({ server: sheet() });
  t.sel.range(1, 0, 3, 0);
  t.app.sort(0, false, { r1: 1, r2: 3 });
  // the fake reverses the rows it is given; the real order is the server's
  assert.deepEqual([1, 2, 3].map((r) => shownAt(t, r, 0)), ['Bus', 'A much longer item name', 'Venue']);
  assert.equal(shownAt(t, 1, 1), '0', 'each row moved whole');
});

test('freeze rows down to, or columns up to, the selection, and unfreeze', async () => {
  const t = await startApp({ server: sheet() });
  t.sel.set(0, 1);
  t.app.changed();
  find(t.app.toolbar.el, 'Freeze rows and columns').click();
  const menu = popup(t);
  assert.equal(menu.querySelector('.mhead').textContent, 'Frozen now: 0 rows, 0 columns');
  assert.equal(find(menu, 'Unfreeze rows').disabled, true);
  find(menu, 'Freeze rows down to row 1').click();
  assert.deepEqual(lastOps(t)[0], { op: 'settings', tab: 1, frozen_rows: 1, frozen_cols: 0 });
  assert.equal(t.model.tab.frozenRows, 1);
  t.app.rowsMenu({ x: 0, y: 0 });
  find(popup(t), 'Freeze columns up to column B').click();
  assert.deepEqual(lastOps(t)[0], { op: 'settings', tab: 1, frozen_rows: 1, frozen_cols: 2 }, 'the other count is kept');
  assert.equal(find(t.app.toolbar.el, 'Freeze rows and columns').getAttribute('aria-pressed'), 'true');
  find(t.app.toolbar.el, 'Freeze rows and columns').click();
  find(popup(t), 'Unfreeze rows').click();
  assert.deepEqual(lastOps(t)[0], { op: 'settings', tab: 1, frozen_rows: 0, frozen_cols: 2 });
});

test('dragging the edge of a letter previews the width and sends one width on release', async () => {
  const t = await startApp({ server: sheet() });
  const m = t.model;
  t.server.manual.add('action');
  t.grid.h.down({ zone: 'resize', r: 0, c: 1 }, ev({ clientX: 300 }));
  t.grid.h.drag({ zone: 'col', r: 0, c: 1 }, ev({ clientX: 340 }));
  t.grid.h.drag({ zone: 'col', r: 0, c: 2 }, ev({ clientX: 396 }));
  assert.equal(t.grid.cols.size(1), 200, 'the column follows the pointer');
  assert.equal(t.server.socket.frames('action').length, 0, 'nothing is sent while dragging');
  t.grid.h.up(ev());
  assert.deepEqual(t.server.socket.frames('action').map((a) => a.ops), [[{ op: 'width', tab: 1, cols: [[m.colAt(1), 1]], width: 200 }]]);
  assert.equal(t.grid.cols.size(1), 200, 'the width stays, pending, until the server answers');
  // widths stay within 40 to 1,000 pixels
  t.grid.h.down({ zone: 'resize', r: 0, c: 0 }, ev({ clientX: 100 }));
  t.grid.h.drag({ zone: 'col', r: 0, c: 0 }, ev({ clientX: -900 }));
  assert.equal(t.grid.cols.size(0), 40);
  t.grid.h.drag({ zone: 'col', r: 0, c: 0 }, ev({ clientX: 5000 }));
  assert.equal(t.grid.cols.size(0), 1000);
  t.grid.h.up(ev());
  // a refusal returns the column to the server's width
  t.server.socket.recv({ type: 'ack', n: '2', seq: '10', results: [{ status: 'refused', reason: 'disk_quota' }] });
  assert.equal(t.grid.cols.size(0), 104);
});

test('a refused width returns the columns, their offsets, the drawn cells and hit testing to the confirmed state', async () => {
  const t = await startApp({ server: sheet(), width: 46 + 4 * 104, height: 400 });
  const m = t.model;
  t.server.manual.add('action');
  const hitCol = (x) => t.grid.hit(46 + x, 28 + 5).c;
  const leftOf = (c) => t.grid.rowEls.get(m.rowAt(1))._c.find((el) => el.getAttribute('aria-colindex') === String(c + 2)).style.left;
  const confirmed = { total: t.grid.size.style.width, c2: leftOf(2), cols: colsDrawnIn(t) };
  // the resize is sent and shows at once
  t.grid.h.down({ zone: 'resize', r: 0, c: 1 }, ev({ clientX: 208 }));
  t.grid.h.drag({ zone: 'col', r: 0, c: 3 }, ev({ clientX: 504 }));
  t.grid.h.up(ev());
  t.grid.paint();
  assert.equal(t.grid.cols.size(1), 400);
  assert.equal(hitCol(250), 1, 'the pending width covers this point');
  assert.equal(leftOf(2), `${46 + 104 + 400}px`);
  // the host refuses it; no delta follows a refused action
  t.server.socket.recv({ type: 'ack', n: '1', seq: '10', results: [{ status: 'refused', reason: 'disk_quota' }] });
  assert.equal(m.widthOf(m.colAt(1)), 104);
  assert.equal(t.grid.cols.size(1), 104, 'the column is 104 pixels again without another event');
  assert.equal(t.grid.cols.offset(2), 208);
  assert.equal(hitCol(250), 2, 'a click lands on the column the confirmed widths put there');
  assert.equal(leftOf(2), confirmed.c2, 'the drawn cells moved back');
  assert.equal(t.grid.size.style.width, confirmed.total, 'and the scroll width is the confirmed one');
  assert.deepEqual(colsDrawnIn(t), confirmed.cols, 'the columns in view are those of the confirmed widths');
  assert.match(notices(t)[0][1], /The host refused this column width/);
  // moving to a column scrolls by the confirmed offsets
  t.sel.set(0, 7);
  t.grid.reveal(0, 7);
  assert.equal(t.grid.el.scrollLeft, 8 * 104 - 4 * 104);
});

test('a drag preview leaves no width behind when the resize cannot be sent', async () => {
  const t = await startApp({ server: sheet() });
  t.grid.h.down({ zone: 'resize', r: 0, c: 1 }, ev({ clientX: 208 }));
  t.grid.h.drag({ zone: 'col', r: 0, c: 3 }, ev({ clientX: 504 }));
  assert.equal(t.grid.cols.size(1), 400);
  // the sheet starts calculating before the button is let go
  t.server.socket.recv({ type: 'status', calculating: true });
  t.grid.h.up(ev());
  assert.equal(t.server.socket.frames('action').length, 0);
  assert.equal(t.model.widthOf(t.model.colAt(1)), 104);
  assert.equal(t.grid.cols.size(1), 104);
});

test('a double click on a column edge fits the column to its loaded cells', async () => {
  const t = await startApp({ server: sheet() });
  t.grid.h.dbl({ zone: 'resize', r: 0, c: 0 }, ev());
  // the widest line is "A much longer item name": 23 characters of 7 px, plus padding
  assert.deepEqual(lastOps(t)[0], { op: 'width', tab: 1, cols: [[t.model.colAt(0), 1]], width: 23 * 7 + 16 });
  t.sel.set(0, 3);
  t.app.rowsMenu({ x: 0, y: 0 });
  find(popup(t), 'Column width…').click();
  const d = popup(t);
  d.querySelector('input').value = '39';
  find(d, 'Set width').click();
  assert.match(d.querySelector('[role="alert"]').textContent, /Enter a width from 40 to 1000 pixels/);
  d.querySelector('input').value = '250';
  find(d, 'Set width').click();
  assert.equal(lastOps(t)[0].width, 250);
});

test('a typed width the page cannot send keeps the dialog with the number, and one the host refuses is offered back', async () => {
  const t = await startApp({ server: sheet() });
  t.sel.set(0, 3);
  t.app.rowsMenu({ x: 0, y: 0 });
  find(popup(t), 'Column width…').click();
  const d = popup(t);
  d.querySelector('input').value = '237';
  // the sheet starts calculating before the number is submitted
  t.server.socket.recv({ type: 'status', calculating: true });
  find(d, 'Set width').click();
  assert.equal(t.server.socket.frames('action').length, 0);
  assert.equal(popup(t), d, 'the dialog stays open');
  assert.equal(d.querySelector('input').value, '237', 'with the typed number');
  assert.match(d.querySelector('[role="alert"]').textContent, /The width was not set/);
  assert.match(notices(t).at(-1)[1], /The sheet is calculating/);
  t.server.socket.recv({ type: 'status', calculating: false });
  t.server.manual.add('action');
  find(d, 'Set width').click();
  assert.equal(popup(t), null, 'a width that was sent closes the dialog');
  const a = t.server.socket.frames('action').at(-1);
  assert.equal(a.ops[0].width, 237);
  t.server.socket.recv({ type: 'ack', n: a.n, seq: '10', results: [{ status: 'refused', reason: 'disk_quota' }] });
  const refusal = notices(t).find(([, text]) => /The host refused this column width/.test(text));
  assert.ok(refusal, 'the refusal is told');
  assert.equal(refusal[2], '237', 'and the typed number is offered back');
  // a width dragged or fitted was not typed: its refusal offers no text
  t.grid.h.dbl({ zone: 'resize', r: 0, c: 0 }, ev());
  const fit = t.server.socket.frames('action').at(-1);
  t.server.socket.recv({ type: 'ack', n: fit.n, seq: '10', results: [{ status: 'refused', reason: 'disk_quota' }] });
  assert.equal(notices(t).filter(([, text]) => /The host refused this column width/.test(text)).at(-1)[2], null);
});

test('the format options and sort dialogs stay open with what was chosen when the page cannot send them', async () => {
  const t = await startApp({ server: sheet() });
  t.sel.set(1, 1);
  t.app.changed();
  find(t.app.toolbar.el, 'Format options: decimals, separator, currency symbol, date and time forms').click();
  const d = popup(t);
  const field = (id) => d.querySelectorAll('select, input').find((el) => el.id === id);
  field('nf-kind').value = 'currency';
  fire(field('nf-kind'), 'change');
  field('nf-sym').value = 'CHF';
  field('nf-dec').value = '3';
  t.server.socket.recv({ type: 'status', calculating: true });
  find(d, 'Apply').click();
  assert.equal(t.server.socket.frames('action').length, 0);
  assert.equal(popup(t), d);
  assert.deepEqual([field('nf-kind').value, field('nf-sym').value, field('nf-dec').value], ['currency', 'CHF', '3']);
  assert.match(d.querySelector('[role="alert"]').textContent, /The format was not applied/);
  t.server.socket.recv({ type: 'status', calculating: false });
  find(d, 'Apply').click();
  assert.equal(popup(t), null);
  assert.deepEqual(lastOps(t)[0].set, { number: { kind: 'currency', symbol: 'CHF', decimals: 3 } });

  t.sel.range(1, 0, 3, 1);
  t.app.changed();
  t.app.rowsMenu({ x: 0, y: 0 });
  find(popup(t), 'Sort rows…').click();
  const s = popup(t);
  s.querySelector('#sort-dir').value = 'desc';
  t.server.socket.recv({ type: 'status', calculating: true });
  const sent = t.server.socket.frames('action').length;
  find(s, 'Sort').click();
  assert.equal(t.server.socket.frames('action').length, sent);
  assert.equal(popup(t), s);
  assert.equal(s.querySelector('#sort-dir').value, 'desc');
  t.server.socket.recv({ type: 'status', calculating: false });
  find(s, 'Sort').click();
  assert.equal(popup(t), null);
  assert.equal(lastOps(t)[0].desc, true);
});

test('tab names follow rule 2', () => {
  const tabs = [{ id: 1, name: 'Budget' }, { id: 2, name: 'Notes' }];
  assert.equal(tabNameError('Costs', tabs, 1), null);
  assert.equal(tabNameError('Budget', tabs, 1), null, 'a tab may keep its own name');
  assert.match(tabNameError('budget', tabs, 2), /already has this name/, 'names are unique apart from capitals');
  assert.match(tabNameError('', tabs, 1), /1 to 100 characters/);
  assert.match(tabNameError('x'.repeat(101), tabs, 1), /1 to 100 characters/);
  assert.equal(tabNameError('가'.repeat(100), tabs, 1), null, 'characters are code points');
  for (const bad of ['a[b', 'a]b', 'a:b', 'a*b', 'a?b', 'a/b', 'a\\b', 'a\tb']) assert.match(tabNameError(bad, tabs, 1), /cannot hold/);
  for (const bad of [' a', 'a ', "'a", "a'"]) assert.match(tabNameError(bad, tabs, 1), /start or end/);
  assert.equal(tabNameError("Q1 costs, o'clock", tabs, 1), null);
  assert.equal(freeName([{ name: 'Sheet1' }, { name: 'sheet2' }, { name: 'Budget' }]), 'Sheet3');
});

test('tabs are added, renamed in place and reordered from the menu and by dragging', async () => {
  const t = await startApp({ server: sheet() });
  const list = t.app.tabs.list;
  find(t.app.tabs.el, 'Add a tab').click();
  assert.deepEqual(lastOps(t)[0], { op: 'add_tab', name: 'Sheet1' });
  assert.deepEqual(list.children.map((el) => el.textContent), ['Budget', 'Notes', 'Sheet1']);
  // rename: a double click or F2 puts a field in the tab bar
  fire(list.children[1], 'dblclick');
  const input = list.querySelector('input');
  assert.equal(t.doc.activeElement, input);
  assert.equal(input.getAttribute('aria-label'), 'New name for tab Notes');
  input.value = 'budget';
  fire(input, 'keydown', { key: 'Enter' });
  assert.match(notices(t)[0][1], /The name was not changed\. Another tab already has this name\./);
  assert.equal(notices(t)[0][2], 'budget', 'the typed name is offered back');
  list.children[1].focus();
  key(t, 'F2');
  list.querySelector('input').value = 'Q1 costs';
  fire(list.querySelector('input'), 'keydown', { key: 'Enter' });
  assert.deepEqual(lastOps(t)[0], { op: 'rename_tab', tab: 2, name: 'Q1 costs' });
  assert.equal(list.children[1].textContent, 'Q1 costs');
  assert.equal(t.doc.activeElement, t.grid.input, 'focus returns to the grid');
  // Escape leaves the name as it was
  fire(list.children[0], 'dblclick');
  list.querySelector('input').value = 'Other';
  const sent = t.server.socket.frames('action').length;
  fire(list.querySelector('input'), 'keydown', { key: 'Escape' });
  assert.equal(t.server.socket.frames('action').length, sent);
  // reorder from the menu: the keyboard path
  list.children[0].focus();
  key(t, 'F10', { shiftKey: true });
  assert.equal(popup(t).getAttribute('aria-label'), 'Tab Budget');
  assert.equal(find(popup(t), 'Move left').disabled, true);
  find(popup(t), 'Move right').click();
  assert.deepEqual(lastOps(t)[0], { op: 'move_tab', tab: 1, before: 3 });
  assert.deepEqual(list.children.map((el) => el.textContent), ['Q1 costs', 'Budget', 'Sheet1']);
  // reorder by dragging: drop Sheet1 before Q1 costs
  list.children.forEach((el, i) => { el.rect = { left: i * 100, top: 0, width: 100, height: 30 }; });
  fire(list.children[2], 'mousedown', { clientX: 250 });
  fire(t.doc, 'mousemove', { clientX: 20 });
  assert.ok(list.children[0].classList.contains('drop-before'), 'the drop place is shown');
  fire(t.doc, 'mouseup', {});
  assert.deepEqual(lastOps(t)[0], { op: 'move_tab', tab: 3, before: 2 });
  assert.deepEqual(list.children.map((el) => el.textContent), ['Sheet1', 'Q1 costs', 'Budget']);
});

test('opening a tab from the tab bar puts focus in its grid', async () => {
  const t = await startApp({ server: sheet() });
  // Frames run when the test says, as in a browser: the bar is drawn again after the click.
  const frames = [];
  t.app.raf = (fn) => frames.push(fn);
  const tab = t.app.tabs.list.children[1];
  tab.focus();
  fire(tab, 'click');
  assert.equal(t.doc.activeElement, t.grid.input, 'typing goes to the opened tab');
  while (frames.length) frames.shift()();
  assert.equal(t.model.tab.id, 2);
  assert.equal(t.doc.activeElement, t.grid.input);
  assert.equal(t.app.tabs.list.children[1].getAttribute('aria-selected'), 'true');
});

test('deleting a tab asks inline when it may hold anything, and never deletes the last tab', async () => {
  const t = await startApp({ server: sheet() });
  const list = t.app.tabs.list;
  list.children[0].focus();
  key(t, 'Delete');
  const ask = t.app.tabs.confirmEl;
  assert.equal(ask.hidden, false);
  assert.equal(ask.getAttribute('role'), 'alertdialog');
  assert.match(ask.textContent, /Delete tab “Budget” with its cells\?/);
  assert.equal(t.doc.activeElement, find(ask, 'Cancel'), 'focus goes to the safe answer');
  key(t, 'Escape');
  assert.equal(ask.hidden, true);
  assert.equal(t.server.socket.frames('action').length, 0);
  assert.equal(t.doc.activeElement, t.grid.input);
  list.children[0].focus();
  key(t, 'Delete');
  find(ask, 'Delete').click();
  assert.deepEqual(lastOps(t)[0], { op: 'delete_tab', tab: 1 });
  assert.equal(t.model.tab.id, 2, 'the other tab opens');
  // the last tab stays
  t.app.tabs.list.children[0].focus();
  key(t, 'Delete');
  assert.match(notices(t).at(-1)[1], /A sheet keeps at least one tab/);
  assert.equal(t.server.socket.frames('action').at(-1).ops[0].tab, 1);
});

test('S-37: after a rollback every formula is read again from the restored sheet', async () => {
  const t = await startApp({ server: sheet() });
  t.sel.set(3, 1);
  t.app.changed();
  t.timers.advance(60);
  assert.equal(t.app.bar.value, '=B2+B3');
  // the sheet is rolled back to a revision in which the formula was another one
  const tab = t.server.tabs[0];
  tab.cells.get(`${tab.rows[3]}:${tab.cols[1]}`).formula = '=SUM(B2:B3)';
  const before = t.server.socket;
  before.recv({ type: 'reset', seq: '11' });
  t.server.seq = 11;
  t.server.socket.open();
  t.timers.advance(60);
  assert.equal(t.server.socket.frames('peek').length, 1, 'nothing cached survives the rollback');
  assert.equal(t.app.bar.value, '=SUM(B2:B3)');
  assert.deepEqual(t.sel.active(), { r: 3, c: 1 }, 'the selection stays where it was');
});

test('on a phone-width screen the toolbar is a menu and the formula bar is the editor', async () => {
  globalThis.__phone = true;
  try {
    const t = await startApp({ server: sheet(), width: 390, height: 500 });
    assert.equal(t.doc.documentElement.dataset.layout, 'phone');
    const bar = t.app.toolbar.el;
    assert.deepEqual(bar.querySelectorAll('button').map((b) => b.getAttribute('aria-label') || b.textContent), ['Undo (Ctrl or Cmd+Z)', 'Redo (Shift+Ctrl or Cmd+Z)', 'Format']);
    find(bar, 'Format').click();
    find(popup(t), 'Bold').click();
    assert.deepEqual(lastOps(t)[0].set, { bold: true });
    // a tap selects a cell, and editing happens in the formula bar
    t.grid.h.down({ zone: 'cell', r: 1, c: 0 }, ev());
    t.grid.h.up(ev());
    assert.deepEqual(t.sel.active(), { r: 1, c: 0 });
    assert.equal(t.app.bar.value, 'Venue');
    t.grid.h.dbl({ zone: 'cell', r: 1, c: 0 }, ev());
    assert.equal(t.editor.area, t.app.bar);
    assert.equal(t.doc.activeElement, t.app.bar);
    t.app.bar.value = 'Hall';
    fire(t.app.bar, 'input');
    key(t, 'Enter');
    assert.equal(lastOps(t)[0].text, 'Hall');
    assert.equal(t.grid.el.querySelector('.handle'), null, 'fill needs a wider screen');
    // tabs switch as on a wide screen
    t.app.tabs.list.children[1].click();
    assert.equal(t.model.tab.id, 2);
  } finally {
    globalThis.__phone = false;
  }
});

test('on a phone the app follows the visual viewport, so the on-screen keyboard covers nothing', async () => {
  globalThis.__phone = true;
  const listeners = {};
  globalThis.visualViewport = { height: 700, offsetTop: 0, scale: 1, addEventListener: (type, fn) => { listeners[type] = fn; } };
  try {
    const t = await startApp({ server: new FakeServer({ tabs: [{ id: 1, name: 'T', rows: 200, cols: 6 }] }), width: 390, height: 500 });
    const root = t.doc.documentElement;
    assert.deepEqual([root.style['--view-height'], root.style['--view-top']], ['700px', '0px']);
    // a cell near the bottom is edited; the keyboard opens and the grid gets shorter
    t.sel.set(15, 1);
    t.grid.reveal(15, 1);
    t.app.changed();
    key(t, 'F2');
    assert.equal(t.editor.open, true);
    const before = t.grid.el.scrollTop;
    Object.assign(globalThis.visualViewport, { height: 380.4, offsetTop: 12 });
    t.grid.el.clientHeight = 180;
    listeners.resize();
    assert.deepEqual([root.style['--view-height'], root.style['--view-top']], ['380px', '12px']);
    assert.ok(t.grid.el.scrollTop > before, 'the edited cell is scrolled back into view');
    const r = t.grid.cellRect(15, 1);
    assert.ok(r.bottom <= 180, `the edited cell ends above the keyboard: ${r.bottom}`);
    // a zoomed page keeps its layout
    Object.assign(globalThis.visualViewport, { height: 190, offsetTop: 300, scale: 2 });
    listeners.scroll();
    assert.deepEqual([root.style['--view-height'], root.style['--view-top']], ['100%', '0px']);
  } finally {
    globalThis.__phone = false;
    delete globalThis.visualViewport;
  }
  // a wide screen keeps its full height
  const wide = await startApp({ server: sheet() });
  assert.equal(wide.doc.documentElement.style['--view-height'], '100%');
});

test('a rename field keeps its text and focus while its tab loads and the bar is drawn again', async () => {
  const t = await startApp({ server: sheet() });
  const list = t.app.tabs.list;
  t.server.manual.add('watch');
  // A double click on a tab that is not open: its first click asks for the tab.
  list.children[1].click();
  fire(list.children[1], 'dblclick');
  const input = list.querySelector('input');
  assert.equal(t.doc.activeElement, input);
  input.value = 'Tr';
  // While the person types, the tab's snapshot arrives, someone else opens a
  // tab, and a third tab is added.
  t.server.manual.delete('watch');
  t.server.snapshot(t.server.socket, t.net.generation, 2);
  assert.equal(t.model.tab.id, 2);
  t.server.socket.recv({ type: 'presence', people: [{ session_id: 's9', label: 'Bob', verified: true, color: 1, tab: 1, active: [t.server.tab(1).rows[0], t.server.tab(1).cols[0]] }] });
  t.server.push({ struct: [{ op: 'add_tab', tab: 3 }], tabs: [...t.server.tabInfo(), { id: 3, name: 'Sheet1', rows: 20, cols: 6, frozen_rows: 0, frozen_cols: 0 }] });
  assert.equal(list.children.length, 3, 'the bar was drawn again');
  assert.ok(list.children[0].querySelector('.pdot'), 'with the other person\'s dot');
  assert.equal(list.querySelector('input'), input, 'the field is the same element');
  assert.equal(input.value, 'Tr', 'with what was typed so far');
  assert.equal(t.doc.activeElement, input, 'and it still has focus');
  assert.equal(t.server.socket.frames('action').length, 0, 'nothing was renamed early');
  input.value = 'Travel';
  fire(input, 'keydown', { key: 'Enter' });
  assert.deepEqual(lastOps(t)[0], { op: 'rename_tab', tab: 2, name: 'Travel' });
  assert.equal(list.children[1].textContent, 'Travel');
  assert.equal(t.doc.activeElement, t.grid.input);
});

test('a tab keeps its button when another tab opens, so a click under way lands', async () => {
  const t = await startApp({ server: sheet() });
  const list = t.app.tabs.list;
  const before = [...list.children];
  list.children[1].click();
  assert.equal(t.model.tab.id, 2);
  assert.deepEqual([...list.children], before, 'the same elements');
  assert.deepEqual(list.children.map((el) => [el.getAttribute('aria-selected'), el.getAttribute('tabindex')]), [['false', '-1'], ['true', '0']]);
  // the second click of a double click arrives on the element of the first
  fire(before[1], 'dblclick');
  assert.ok(list.querySelector('input'));
  fire(list.querySelector('input'), 'keydown', { key: 'Escape' });
  // a tab that is deleted while it is being renamed ends the field without a rename
  fire(list.children[0], 'dblclick');
  list.querySelector('input').value = 'Gone';
  const sent = t.server.socket.frames('action').length;
  t.server.deleteTab(1);
  assert.equal(list.querySelector('input'), null);
  assert.deepEqual(list.children.map((el) => el.textContent), ['Notes']);
  assert.equal(t.server.socket.frames('action').length, sent);
});

test('a click on the open tab asks for no new snapshot, and what is typed next is kept', async () => {
  const t = await startApp({ server: sheet() });
  const watches = t.server.socket.frames('watch').length;
  t.app.tabs.list.children[0].click();
  assert.equal(t.server.socket.frames('watch').length, watches);
  type(t, 'Train');
  key(t, 'Enter');
  assert.equal(t.server.cell(1, 0, 0).value, 'Train');
  assert.deepEqual(notices(t), []);
});

test('after a click on another tab nothing is typed into the tab that is still on screen', async () => {
  const t = await startApp({ server: sheet() });
  t.server.manual.add('watch');
  t.app.tabs.list.children[1].click();
  assert.equal(t.model.tab.id, 1, 'the new tab has not arrived');
  type(t, 'for Notes');
  assert.equal(t.editor.open, false);
  assert.match(notices(t).at(-1)[1], /The tab you opened is still loading/);
  key(t, 'Delete');
  assert.equal(t.server.socket.frames('action').length, 0, 'the tab the person left is not written');
  // the tab bar is not the grid: a rename goes out while the tab loads
  const list = t.app.tabs.list;
  fire(list.children[1], 'dblclick');
  list.querySelector('input').value = 'Travel';
  fire(list.querySelector('input'), 'keydown', { key: 'Enter' });
  assert.deepEqual(lastOps(t)[0], { op: 'rename_tab', tab: 2, name: 'Travel' });
  t.server.manual.delete('watch');
  t.server.snapshot(t.server.socket, t.net.generation, 2);
  type(t, 'for Notes');
  key(t, 'Enter');
  assert.deepEqual([lastOps(t)[0].tab, lastOps(t)[0].text], [2, 'for Notes']);
});

test('a rename that cannot be sent when it ends gives the typed name back', async () => {
  const t = await startApp({ server: sheet() });
  const list = t.app.tabs.list;
  fire(list.children[1], 'dblclick');
  t.server.socket.recv({ type: 'status', calculating: true });
  const input = list.querySelector('input');
  assert.ok(input, 'the rename field stays while the sheet calculates');
  input.value = 'Travel';
  fire(input, 'keydown', { key: 'Enter' });
  assert.equal(t.server.socket.frames('action').length, 0);
  assert.ok(notices(t).some(([, text, kept]) => /The tab was not renamed/.test(text) && kept === 'Travel'), 'the typed name is offered back');
});

test('no number that is not one reaches a frame: a sort without a column and a drop on a rename field send nothing wrong', async () => {
  const t = await startApp({ server: sheet() });
  const sent = () => t.server.socket.frames('action').length;
  const before = sent();
  t.app.sort(Number.NaN, false, 'all');
  t.app.sort(99, false, 'all');
  assert.equal(sent(), before, 'a column that is none sorts nothing');
  // every frame the page sent so far names real numbers only
  assert.equal(JSON.stringify(t.server.socket.sent).includes('null'), false);
  // A tab dragged over a tab that is being renamed: the field names no place.
  const list = t.app.tabs.list;
  fire(list.children[1], 'dblclick');
  list.children.forEach((el, i) => { el.rect = { left: i * 100, top: 0, width: 100, height: 30 }; });
  fire(list.children[0], 'mousedown', { button: 0, clientX: 10 });
  fire(t.doc, 'mousemove', { clientX: 130 });
  fire(t.doc, 'mouseup', {});
  // A move is only sent to a real place: before a tab, or null for the end.
  for (const a of t.server.socket.frames('action').slice(before)) for (const op of a.ops) if (op.op === 'move_tab') assert.ok(op.before === null || Number.isInteger(op.before));
});
