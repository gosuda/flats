'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { findMatches } from '../find.js';
import { fire } from './dom.mjs';
import { FakeServer, find, startApp, type } from './server.mjs';

const sheet = (access = 'private') => new FakeServer({ access, tabs: [{ id: 1, name: 'Budget', rows: 200, cols: 5, cells: {
  '0,0': 'Item', '1,0': 'Train tickets', '1,2': 'train', '4,1': { kind: 'n', value: 0.5, shown: '50% of TRAIN fare' }, '150,3': 'Night train', '3,0': 'Bus',
} }] });
const search = (t, text) => { t.app.find.input.value = text; fire(t.app.find.input, 'input'); };

test('matches are found in what cells show, capitals ignored, row by row', async () => {
  const t = await startApp({ server: sheet() });
  assert.deepEqual(findMatches(t.model, 'TRAIN'), [{ r: 1, c: 0 }, { r: 1, c: 2 }, { r: 4, c: 1 }, { r: 150, c: 3 }]);
  assert.deepEqual(findMatches(t.model, '50%'), [{ r: 4, c: 1 }], 'the shown text is searched, not the stored value');
  assert.deepEqual(findMatches(t.model, '0.5'), []);
  assert.deepEqual(findMatches(t.model, ''), []);
});

test('Ctrl or Cmd+F opens the field, counts the matches and moves the selection through them', async () => {
  const t = await startApp({ server: sheet() });
  const f = t.app.find;
  assert.equal(fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true }).defaultPrevented, true);
  assert.equal(f.open, true);
  assert.equal(f.el.getAttribute('role'), 'dialog');
  assert.equal(f.el.getAttribute('aria-label'), 'Find');
  assert.equal(t.doc.activeElement, f.input);
  search(t, 'train');
  assert.equal(f.count.textContent, '1 of 4');
  assert.match(t.doc.querySelector('#announce').textContent, /Match 1 of 4 matches/, 'the count is announced without moving focus');
  assert.equal(t.doc.activeElement, f.input);
  assert.deepEqual(t.sel.active(), { r: 1, c: 0 });
  assert.ok(t.grid.activeEl.classList.contains('hit'), 'matches are marked in the grid');
  fire(f.input, 'keydown', { key: 'Enter' });
  assert.deepEqual(t.sel.active(), { r: 1, c: 2 });
  find(f.el, 'Next match (Enter)').click();
  find(f.el, 'Next match (Enter)').click();
  assert.deepEqual(t.sel.active(), { r: 150, c: 3 });
  assert.ok(t.grid.rowEls.has(t.model.rowAt(150)), 'the grid scrolled to the match');
  assert.equal(f.count.textContent, '4 of 4');
  fire(f.input, 'keydown', { key: 'Enter' });
  assert.equal(f.count.textContent, '1 of 4', 'it wraps around');
  fire(f.input, 'keydown', { key: 'Enter', shiftKey: true });
  assert.equal(f.count.textContent, '4 of 4');
  search(t, 'plane');
  assert.equal(f.count.textContent, 'No matches');
  assert.match(t.doc.querySelector('#announce').textContent, /No matches/);
  // Escape closes, clears the marks and returns focus to the grid
  fire(f.input, 'keydown', { key: 'Escape' });
  assert.equal(f.open, false);
  assert.equal(t.grid.overlays.hits, null);
  assert.equal(t.doc.activeElement, t.grid.input);
});

test('the matches follow the tab while it changes', async () => {
  const t = await startApp({ server: sheet() });
  t.app.find.show();
  search(t, 'train');
  t.server.setCell(1, 1, 2, 'bus');
  assert.equal(t.app.find.count.textContent, '1 of 3');
  t.server.setCell(1, 2, 0, 'Train pass');
  assert.equal(t.app.find.matches.length, 4);
});

test('find works the same on a Public route', async () => {
  const t = await startApp({ server: sheet('public') });
  find(t.app.top, 'Find in this tab (Ctrl or Cmd+F)').click();
  search(t, 'train');
  assert.equal(t.app.find.count.textContent, '1 of 4');
  assert.deepEqual(t.sel.active(), { r: 1, c: 0 });
  assert.deepEqual(t.server.socket.sent.map((m) => m.type), ['hello', 'watch'], 'it sends nothing');
});

test('Find selects the match where it is after the edit it commits releases a held insert', async () => {
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 30, cols: 6, cells: { '1,1': 'needle', '5,1': '6' } }] });
  const t = await startApp({ server });
  server.manual.add('rebase');
  const needle = [t.model.rowAt(1), t.model.colAt(1)];
  t.sel.set(2, 3);
  t.app.changed();
  type(t, '=B6+1');
  // Another writer inserts a row at the top while the formula is typed:
  // the editor holds it and asks for a rebase, and stays editable.
  server.insertRows(1, 0);
  assert.equal(t.grid.input.readOnly, false);
  assert.deepEqual([t.model.rowAt(1), t.model.colAt(1)], needle, 'the insert is held');
  fire(t.doc.activeElement, 'keydown', { key: 'f', ctrlKey: true });
  search(t, 'needle');
  assert.equal(t.editor.ed, null, 'the edit was committed');
  const a = t.sel.active();
  assert.deepEqual([t.model.rowAt(a.r), t.model.colAt(a.c)], needle, 'the selection is on the match');
  assert.deepEqual(a, { r: 2, c: 1 });
  assert.equal(t.app.find.count.textContent, '1 of 1');
});
