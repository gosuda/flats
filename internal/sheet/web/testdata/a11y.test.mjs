'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { cellLabel } from '../a11y.js';
import { fire } from './dom.mjs';
import { FakeServer, find, key, startApp, type } from './server.mjs';

const sheet = () => new FakeServer({ tabs: [
  { id: 1, name: 'Budget', rows: 1000, cols: 26, cells: { '0,0': 'Item', '0,1': 'Amount', '1,0': 'Venue', '1,1': { kind: 'n', value: 1200, shown: '$1,200.00', edit: '1200' }, '2,1': '=B2*2', '3,1': { kind: 'ef', value: '#DIV/0!', formula: '=1/0' } } },
  { id: 2, name: 'Notes', rows: 5, cols: 3 },
] });
const label = (t) => t.grid.input.getAttribute('aria-label');

test('the grid is exposed as a grid with the counts of the whole tab, although few rows are drawn', async () => {
  const t = await startApp({ server: sheet() });
  const g = t.grid.el;
  assert.equal(g.getAttribute('role'), 'grid');
  assert.equal(g.getAttribute('aria-rowcount'), '1001', '1,000 rows and the row of column letters');
  assert.equal(g.getAttribute('aria-colcount'), '27', '26 columns and the column of row numbers');
  assert.equal(g.getAttribute('aria-label'), 'Tab Budget, 1000 rows, 26 columns');
  assert.ok(t.grid.rowEls.size < 40);
  // its description names the keys that lead out of it, because Tab stays inside
  const desc = t.doc.querySelector(`#${g.getAttribute('aria-describedby')}`);
  assert.match(desc.textContent, /Tab and Shift\+Tab move between cells/);
  assert.match(desc.textContent, /F6 and Shift\+F6 move between top bar, toolbar, formula bar, grid and tabs; Escape returns to the grid/);
});

test('rows, cells and headers carry their roles and their indices in the whole tab', async () => {
  const t = await startApp({ server: sheet() });
  const head = t.grid.headRow;
  assert.equal(head.getAttribute('role'), 'row');
  assert.equal(head.getAttribute('aria-rowindex'), '1');
  assert.equal(t.grid.corner.getAttribute('role'), 'columnheader');
  assert.deepEqual(head._c.slice(0, 3).map((el) => [el.getAttribute('role'), el.getAttribute('aria-colindex'), el.textContent]), [['columnheader', '2', 'A'], ['columnheader', '3', 'B'], ['columnheader', '4', 'C']]);
  const row = t.grid.rowEls.get(t.model.rowAt(4));
  assert.equal(row.getAttribute('role'), 'row');
  assert.equal(row.getAttribute('aria-rowindex'), '6', 'row 5 of the tab, after the header row');
  assert.equal(row._rh.getAttribute('role'), 'rowheader');
  assert.equal(row._rh.textContent, '5');
  assert.deepEqual(row._c.slice(0, 2).map((el) => [el.getAttribute('role'), el.getAttribute('aria-colindex')]), [['gridcell', '2'], ['gridcell', '3']]);
  for (const group of [t.grid.top, t.grid.body]) assert.equal(group.getAttribute('role'), 'rowgroup');
  // far down the tab the indices are still those of the whole tab
  t.grid.el.scrollTop = 800 * 28;
  t.grid.paint();
  const far = t.grid.rowEls.get(t.model.rowAt(805));
  assert.equal(far.getAttribute('aria-rowindex'), '807');
});

test('one text area owns focus, sits in the active cell and names the cell on every move', async () => {
  const t = await startApp({ server: sheet() });
  assert.equal(t.doc.activeElement, t.grid.input);
  assert.equal(t.grid.input.parentNode, t.grid.activeEl, 'the text area is inside the active cell\'s element');
  assert.equal(t.grid.activeEl.getAttribute('role'), 'gridcell');
  assert.equal(t.grid.input.value, '', 'it is empty until the person types');
  assert.equal(label(t), 'A1, Item');
  key(t, 'ArrowDown');
  key(t, 'ArrowRight');
  assert.equal(label(t), 'B2, $1,200.00', 'address and what the cell shows');
  assert.equal(t.doc.querySelector('#announce-cell').textContent, 'B2, $1,200.00', 'each move is also spoken through a live region');
  assert.equal(t.grid.activeEl.getAttribute('aria-colindex'), '3');
  assert.equal(t.grid.activeEl.parentNode.getAttribute('aria-rowindex'), '3');
  key(t, 'ArrowDown');
  assert.equal(label(t), 'B3, 0, formula');
  key(t, 'ArrowDown');
  assert.equal(label(t), 'B4, #DIV/0!, formula, error');
  key(t, 'ArrowRight');
  assert.equal(label(t), 'C4, blank');
  key(t, 'ArrowUp', { shiftKey: true });
  assert.equal(label(t), 'C4, blank, selected', 'part of a range');
  assert.equal(t.doc.activeElement, t.grid.input, 'focus never left the text area');
  // the active cell stays drawn when it is scrolled far out of view
  t.grid.el.scrollTop = 900 * 28;
  t.grid.paint();
  assert.ok(t.grid.activeEl.isConnected);
  assert.equal(t.doc.activeElement, t.grid.input);
  assert.equal(cellLabel({ r: 0, c: 27, shown: 'x', pending: true }), 'AB1, x, not saved yet');
});

test('cells of a selected range are marked selected for assistive technology', async () => {
  const t = await startApp({ server: sheet() });
  t.sel.range(0, 0, 1, 1);
  t.app.changed();
  const row = t.grid.rowEls.get(t.model.rowAt(1));
  assert.equal(row._c[1].getAttribute('aria-selected'), 'true');
  assert.equal(row._c[2].getAttribute('aria-selected'), null);
  assert.equal(t.grid.el.getAttribute('aria-multiselectable'), 'true');
});

test('the formula bar is a labeled field that reads the content of the active cell', async () => {
  const t = await startApp({ server: sheet() });
  assert.equal(t.app.bar.tagName, 'TEXTAREA');
  assert.equal(t.app.bar.getAttribute('aria-label'), 'Formula bar: content of the active cell');
  key(t, 'ArrowDown');
  key(t, 'ArrowRight');
  assert.equal(t.app.bar.value, '1200', 'the content as it would be typed, not what the cell shows');
});

test('F6 and Shift+F6 move through top bar, toolbar, formula bar, grid and tab bar, and Escape returns', async () => {
  const t = await startApp({ server: sheet() });
  const said = () => t.doc.querySelector('#announce').textContent;
  const where = () => {
    const a = t.doc.activeElement;
    for (const [name, root] of [['top', t.app.top], ['tools', t.app.toolbar.el], ['bar', t.app.fbar], ['grid', t.grid.el], ['tabs', t.app.tabs.el]]) if (root.contains(a)) return name;
    return 'elsewhere';
  };
  assert.equal(where(), 'grid');
  const route = [];
  for (let i = 0; i < 5; i++) { assert.equal(key(t, 'F6').defaultPrevented, true); route.push(where()); }
  assert.deepEqual(route, ['tabs', 'top', 'tools', 'bar', 'grid']);
  assert.equal(said(), 'Grid', 'the region is named when focus arrives');
  const back = [];
  for (let i = 0; i < 5; i++) { key(t, 'F6', { shiftKey: true }); back.push(where()); }
  assert.deepEqual(back, ['bar', 'tools', 'top', 'tabs', 'grid']);
  // in the tab bar focus lands on the open tab
  key(t, 'F6');
  assert.equal(t.doc.activeElement.getAttribute('aria-selected'), 'true');
  // Escape in any other region returns to the grid
  for (const steps of [1, 2, 3, 4]) {
    for (let i = 0; i < steps; i++) key(t, 'F6');
    assert.notEqual(where(), 'grid');
    assert.equal(key(t, 'Escape').defaultPrevented, true);
    assert.equal(where(), 'grid');
  }
});

test('F6 commits an open edit before it moves on', async () => {
  const t = await startApp({ server: sheet() });
  t.sel.set(5, 0);
  type(t, 'done');
  key(t, 'F6');
  assert.equal(t.editor.open, false);
  assert.equal(t.server.cell(1, 5, 0).value, 'done');
  assert.ok(t.app.tabs.el.contains(t.doc.activeElement));
});

test('the tab bar is a tab list with the open tab marked, and arrows move inside it', async () => {
  const t = await startApp({ server: sheet() });
  const list = t.app.tabs.list;
  assert.equal(list.getAttribute('role'), 'tablist');
  assert.deepEqual(list.children.map((el) => [el.getAttribute('role'), el.getAttribute('aria-selected'), el.getAttribute('tabindex')]), [['tab', 'true', '0'], ['tab', 'false', '-1']]);
  t.app.tabs.focus();
  key(t, 'ArrowRight');
  assert.equal(t.doc.activeElement, list.children[1]);
  key(t, 'ArrowRight');
  assert.equal(t.doc.activeElement, list.children[0], 'it wraps');
  list.children[1].click();
  assert.equal(list.children[1].getAttribute('aria-selected'), 'true');
});

test('a menu is named, takes focus, keeps Tab inside and returns focus to the grid when it closes', async () => {
  const t = await startApp({ server: sheet() });
  t.grid.el.offsetWidth = 1000;
  key(t, 'F10', { shiftKey: true });
  const menu = t.doc.querySelector('.menu');
  assert.equal(menu.getAttribute('role'), 'menu');
  assert.equal(menu.getAttribute('aria-label'), 'Rows and columns');
  const items = menu.querySelectorAll('[role="menuitem"]');
  assert.equal(t.doc.activeElement, items[0], 'the menu takes focus');
  assert.equal(items[0].textContent, 'Insert 1 row above');
  key(t, 'ArrowDown');
  assert.equal(t.doc.activeElement, items[1]);
  key(t, 'ArrowUp');
  key(t, 'ArrowUp');
  assert.equal(t.doc.activeElement, items.filter((b) => !b.disabled).at(-1), 'arrows wrap');
  key(t, 'Tab');
  assert.ok(menu.contains(t.doc.activeElement), 'Tab stays inside');
  // keys inside a menu do not reach the grid
  const before = t.sel.active();
  key(t, 'ArrowDown');
  assert.deepEqual(t.sel.active(), before);
  assert.equal(key(t, 'Escape').defaultPrevented, true);
  assert.equal(t.doc.querySelector('.menu'), null);
  assert.equal(t.doc.activeElement, t.grid.input, 'focus is back in the grid');
  // choosing a command closes the menu and returns focus too
  key(t, 'ContextMenu');
  find(t.doc.querySelector('.menu'), 'Insert 1 row below').click();
  assert.equal(t.doc.querySelector('.menu'), null);
  assert.equal(t.doc.activeElement, t.grid.input);
  assert.equal(t.model.rowCount, 1001);
  // a press outside closes it
  key(t, 'F10', { shiftKey: true });
  fire(t.app.top, 'mousedown');
  assert.equal(t.doc.querySelector('.menu'), null);
});

test('a dialog is named, takes focus and gives it back to the grid', async () => {
  const t = await startApp({ server: sheet() });
  find(t.app.toolbar.el, 'Sort…').click();
  const dialog = t.doc.querySelector('.pop:not([hidden])');
  assert.equal(dialog.getAttribute('role'), 'dialog');
  assert.equal(dialog.getAttribute('aria-label'), 'Sort rows');
  assert.equal(t.doc.activeElement, dialog.querySelector('select'), 'the first field takes focus');
  for (const el of dialog.querySelectorAll('select, input')) assert.ok(dialog.querySelectorAll('label').some((l) => l.getAttribute('for') === el.id), 'every field has a label');
  key(t, 'Escape');
  assert.equal(t.doc.querySelector('.pop:not([hidden])'), null);
  assert.equal(t.doc.activeElement, t.grid.input);
  // the keyboard paths dialog
  find(t.app.top, t.app.keysBtn.getAttribute('aria-label')).click();
  assert.equal(t.doc.querySelector('.pop.keys').getAttribute('aria-label'), 'Keyboard paths');
  find(t.doc.querySelector('.pop.keys'), 'Close').click();
  assert.equal(t.doc.activeElement, t.grid.input);
});

test('save status and notices are announced without moving focus', async () => {
  const t = await startApp({ server: sheet() });
  const status = t.doc.querySelector('#announce');
  t.server.manual.add('action');
  type(t, 'x');
  key(t, 'Enter');
  assert.equal(status.textContent, 'Saving…');
  t.server.socket.recv({ type: 'ack', n: '1', seq: '11', results: [{ status: 'refused', reason: 'text_length' }] });
  assert.equal(status.textContent, 'Saved');
  // the refusal is added to a live region of its own, so the status does not cut it short
  assert.equal(t.app.toasts.getAttribute('aria-live'), 'polite');
  assert.equal(t.app.toasts.getAttribute('role'), 'region');
  assert.equal(t.app.toasts.getAttribute('aria-label'), 'Notices');
  assert.match(t.app.toasts.textContent, /a text cell holds at most 32,767 characters/);
  assert.equal(t.doc.activeElement, t.grid.input);
  t.server.socket.drop();
  assert.equal(status.textContent, 'Offline');
});

test('icon buttons have names, and pressed states are exposed', async () => {
  const t = await startApp({ server: sheet() });
  for (const b of t.doc.querySelectorAll('button')) {
    assert.ok(b.getAttribute('aria-label') || b.textContent.trim(), 'every button has a name');
  }
  const bold = find(t.app.toolbar.el, 'Bold (Ctrl or Cmd+B)');
  assert.equal(bold.getAttribute('aria-pressed'), 'false');
  t.server.tabs[0].styles.push({ bold: true });
  t.server.push({ tab: 1, styles: [{ bold: true }], cells: [[t.model.rowAt(0), [[t.model.colAt(0), 't', 'Item', null, null, 1]]]] });
  assert.equal(find(t.app.toolbar.el, 'Bold (Ctrl or Cmd+B)').getAttribute('aria-pressed'), 'true');
  assert.equal(t.app.toolbar.el.getAttribute('role'), 'toolbar');
  assert.ok(t.grid.activeEl.classList.contains('b'));
});
