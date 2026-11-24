'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { findMatches } from '../find.js';
import { fire } from './dom.mjs';
import { FakeServer, find, key, startApp, type } from './server.mjs';

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
  assert.match(t.doc.querySelector('#announce-find').textContent, /Match 1 of 4 matches/, 'the count is announced without moving focus');
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
  assert.match(t.doc.querySelector('#announce-find').textContent, /No matches/);
  // Escape closes, clears the marks and returns focus to the grid
  fire(f.input, 'keydown', { key: 'Escape' });
  assert.equal(f.open, false);
  assert.equal(t.grid.overlays.hits, null);
  assert.equal(t.doc.activeElement, t.grid.input);
});

// T-03 (TECH 22.2): B2 and D2 hold xy, B5 holds x, and the active cell
// is C2. Find starts from its anchor, steps from the active cell, and
// keeps its anchor where the person, or a reload of the tab, last put the
// selection.
const anchored = () => new FakeServer({ tabs: [{ id: 1, name: 'T', rows: 10, cols: 6, cells: { '1,1': 'xy', '1,3': 'xy', '4,1': 'x', '7,0': 'a*', '7,1': 'ab', '8,0': 'οδοσ' } }] });
const click = (t, r, c) => { const ev = { preventDefault() {}, clientX: 0, clientY: 0, button: 0 }; t.grid.h.down({ zone: 'cell', r, c }, ev); t.grid.h.up(ev); };
const at = (t) => { const a = t.sel.active(); return `${String.fromCharCode(65 + a.c)}${a.r + 1}`; };
// reload loads the tab again the way a resync and its snapshot do (apply.test.mjs).
const reload = (t) => {
  const generation = t.net.generation + 1;
  t.server.socket.recv({ type: 'resync', generation, seq: String(t.server.seq), date: t.server.date, tabs: t.server.tabInfo() });
  t.server.snapshot(t.server.socket, generation, 1);
};

test('T-03: Find starts at its anchor, steps from the active cell and keeps the anchor the person set', async () => {
  const t = await startApp({ server: anchored() });
  const f = t.app.find;
  t.sel.set(1, 2);
  t.app.changed();
  fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true });
  assert.equal(at(t), 'C2', 'opening selects nothing');
  search(t, 'x');
  assert.equal(at(t), 'D2', 'typing selects the first match at or after the anchor');
  assert.equal(f.count.textContent, '2 of 3');
  find(f.el, 'Next match (Enter)').click();
  assert.equal(at(t), 'B5');
  find(f.el, 'Previous match (Shift+Enter)').click();
  assert.equal(at(t), 'D2');
  click(t, 3, 0);
  assert.equal(at(t), 'A4');
  find(f.el, 'Next match (Enter)').click();
  assert.equal(at(t), 'B5', 'Next goes from the active cell the click set');
  search(t, 'xy');
  assert.equal(at(t), 'B2', 'no match follows the anchor A4, so the first match of the tab is selected');
  search(t, 'xyz');
  assert.equal(at(t), 'B2', 'with no match the selection stays');
  assert.equal(f.count.textContent, 'No matches');
  // clear the text, then reload the tab with the selection restored to D2:
  // the reload keeps the selection the tab has, so D2 is put back first
  search(t, '');
  t.sel.set(1, 3);
  reload(t);
  assert.equal(t.model.stale, false, 'the snapshot loaded the tab again');
  assert.equal(at(t), 'D2', 'the reload keeps the selection at D2');
  fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true });
  search(t, 'x');
  assert.equal(at(t), 'D2', 'after the reload, x selects D2, the first match at the restored anchor');
  assert.equal(f.count.textContent, '2 of 3');
  // *, ? and ~ are plain characters, and capitals are ignored by toLowerCase.
  assert.deepEqual(findMatches(t.model, 'a*'), [{ r: 7, c: 0 }]);
  search(t, 'a*');
  assert.equal(at(t), 'A8');
  assert.equal(f.count.textContent, '1 of 1');
  assert.deepEqual(findMatches(t.model, 'ΟΔΟΣ'), []);
  search(t, 'ΟΔΟΣ');
  assert.equal(f.count.textContent, 'No matches');
  assert.equal(at(t), 'A8');
});

// A click moves the selection with the focus to the grid. The count follows
// the selection however it moved, not only Find's own steps.
test('the count follows a move the person makes: a click off the matches names none', async () => {
  const t = await startApp({ server: anchored() });
  const f = t.app.find;
  t.sel.set(1, 2);
  t.app.changed();
  fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true });
  search(t, 'x');
  find(f.el, 'Next match (Enter)').click();
  find(f.el, 'Previous match (Shift+Enter)').click();
  assert.equal(at(t), 'D2');
  assert.equal(f.count.textContent, '2 of 3');
  click(t, 3, 0);
  assert.equal(at(t), 'A4');
  assert.equal(f.count.textContent, '3 matches', 'A4 is no match, so the count names none');
  // The focus is on the grid after the click, so Enter edits A4 (rule 21) and Find does not move.
  fire(t.doc.activeElement, 'keydown', { key: 'Enter' });
  assert.equal(at(t), 'A4');
  assert.equal(f.count.textContent, '3 matches');
});

test('Enter in the Find field after a click steps to the next match and the count names it', async () => {
  const t = await startApp({ server: anchored() });
  const f = t.app.find;
  t.sel.set(1, 2);
  t.app.changed();
  fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true });
  search(t, 'x');
  find(f.el, 'Next match (Enter)').click();
  find(f.el, 'Previous match (Shift+Enter)').click();
  click(t, 3, 0);
  f.input.focus();
  fire(f.input, 'keydown', { key: 'Enter' });
  assert.equal(at(t), 'B5', 'Next from A4 is B5');
  assert.equal(f.count.textContent, '3 of 3', 'the count names the match at once');
});

test('Find opened again with text in its field selects nothing and steps from the active cell', async () => {
  const t = await startApp({ server: anchored() });
  const f = t.app.find;
  t.sel.set(1, 2);
  fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true });
  search(t, 'x');
  assert.equal(at(t), 'D2');
  fire(f.input, 'keydown', { key: 'Escape' });
  assert.equal(f.open, false);
  click(t, 0, 0);
  fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true });
  assert.equal(f.input.value, 'x');
  assert.equal(at(t), 'A1', 'opening again selects nothing');
  assert.equal(f.count.textContent, '3 matches');
  assert.ok(t.grid.overlays.hits?.size === 3, 'the matches are marked');
  fire(f.input, 'keydown', { key: 'Enter' });
  assert.equal(at(t), 'B2', 'Next goes to the first match after the active cell');
  assert.equal(f.count.textContent, '1 of 3');
  fire(f.input, 'keydown', { key: 'Enter', shiftKey: true });
  assert.equal(at(t), 'B5', 'Previous before the first match goes to the last one');
  // The anchor is A1, where the field opened: Find's own moves leave it.
  search(t, 'xy');
  assert.equal(at(t), 'B2');
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

// The count is spoken in its own polite region, #announce-find, and only
// when its text changes; Find's own steps and field edits always speak. heard
// reads the region and empties it. Each expected announcement is asserted
// before it is read away, so a silent fault cannot hide in a read.
const heard = (t) => { const r = t.doc.querySelector('#announce-find'); const said = r.textContent; r.textContent = ''; return said; };

test('a click that moves the selection onto a match speaks the count in the find region, not in the status', async () => {
  const t = await startApp({ server: sheet() });
  const f = t.app.find;
  fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true });
  search(t, 'train');
  assert.equal(heard(t), 'Match 1 of 4 matches');
  click(t, 4, 1);
  assert.equal(f.count.textContent, '3 of 4');
  assert.equal(heard(t), 'Match 3 of 4 matches', 'a click onto a match is spoken');
  assert.doesNotMatch(t.doc.querySelector('#announce').textContent, /match/i, 'the status region carries no count');
  assert.equal(f.count.getAttribute('aria-hidden'), 'true', 'the visible count stays hidden from the reader');
});

test('a click on a cell that is no match speaks the count it leaves, once', async () => {
  const t = await startApp({ server: sheet() });
  const f = t.app.find;
  fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true });
  search(t, 'train');
  assert.equal(heard(t), 'Match 1 of 4 matches');
  click(t, 4, 1);
  assert.equal(heard(t), 'Match 3 of 4 matches');
  click(t, 3, 0);
  assert.equal(f.count.textContent, '4 matches');
  assert.equal(heard(t), '4 matches', 'the count changed from 3 of 4 to 4 matches');
  click(t, 3, 0);
  assert.equal(heard(t), '', 'a click on the same cell speaks nothing');
});

test('an arrow key from one cell that is no match to another speaks nothing while the count keeps its text', async () => {
  const t = await startApp({ server: sheet() });
  fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true });
  search(t, 'train');
  assert.equal(heard(t), 'Match 1 of 4 matches');
  click(t, 3, 0);
  assert.equal(heard(t), '4 matches');
  key(t, 'ArrowDown');
  assert.deepEqual(t.sel.active(), { r: 4, c: 0 }, 'the arrow key moved to another cell that is no match');
  assert.equal(t.app.find.count.textContent, '4 matches');
  assert.equal(heard(t), '', 'the text did not change, so nothing is spoken');
});

test('an own edit that adds a match speaks the new count, and the save status does not cut it short', async () => {
  const t = await startApp({ server: sheet() });
  const f = t.app.find;
  fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true });
  search(t, 'train');
  assert.equal(heard(t), 'Match 1 of 4 matches');
  click(t, 2, 0);
  assert.equal(heard(t), '4 matches', 'the click moves off the matches');
  type(t, 'Train');
  key(t, 'Enter');
  assert.equal(f.count.textContent, '5 matches');
  assert.equal(t.doc.querySelector('#announce').textContent, 'Saved', 'the save status is spoken');
  assert.equal(t.doc.querySelector('#announce-find').textContent, '5 matches', 'the save status did not replace the count');
});

test('an own edit that removes the match the selection is on speaks the new count', async () => {
  const t = await startApp({ server: sheet() });
  const f = t.app.find;
  fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true });
  search(t, 'train');
  assert.equal(heard(t), 'Match 1 of 4 matches');
  click(t, 1, 0);
  assert.equal(heard(t), '', 'a click on the match the selection is already on speaks nothing');
  key(t, 'Delete');
  assert.equal(f.count.textContent, '3 matches');
  assert.equal(heard(t), '3 matches', 'the own edit removed a match');
});

test('a remote edit speaks a count it changed, and speaks nothing for one it did not change', async () => {
  const t = await startApp({ server: sheet() });
  fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true });
  search(t, 'train');
  assert.equal(heard(t), 'Match 1 of 4 matches');
  const focus = t.doc.activeElement;
  t.server.setCell(1, 5, 0, 'train again');
  assert.equal(t.app.find.count.textContent, '1 of 5');
  assert.equal(heard(t), 'Match 1 of 5 matches', 'the count the remote edit changed is spoken');
  assert.equal(t.doc.activeElement, focus, 'the announcement does not move focus');
  t.server.setCell(1, 6, 1, 'nothing here');
  assert.equal(heard(t), '', 'a remote edit that leaves the count alone speaks nothing');
});

test('a rollback speaks the count of the sheet it restores', async () => {
  const t = await startApp({ server: sheet() });
  fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true });
  search(t, 'train');
  assert.equal(heard(t), 'Match 1 of 4 matches');
  const before = t.server.socket;
  const tab = t.server.tab(1);
  tab.cells.delete(`${tab.rows[4]}:${tab.cols[1]}`);
  before.recv({ type: 'reset', seq: String(++t.server.seq) });
  t.server.socket.open();
  assert.equal(t.app.find.count.textContent, '1 of 3');
  assert.equal(heard(t), 'Match 1 of 3 matches', 'the rollback changed the count');
});

test('a change of tab speaks the count the tab it shows has', async () => {
  const server = new FakeServer({ tabs: [
    { id: 1, name: 'Budget', rows: 20, cols: 5, cells: { '1,0': 'Train tickets', '1,2': 'train' } },
    { id: 2, name: 'Trips', rows: 20, cols: 5, cells: { '0,0': 'Train pass' } },
  ] });
  const t = await startApp({ server });
  fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true });
  search(t, 'train');
  assert.equal(heard(t), 'Match 1 of 2 matches');
  t.app.openTab(2);
  assert.equal(t.app.find.count.textContent, '1 of 1');
  assert.equal(heard(t), 'Match 1 of 1 match', 'the count of the tab shown is spoken');
});

test('a paint that changes nothing speaks nothing: a scroll, a second paint and a status change', async () => {
  const t = await startApp({ server: sheet() });
  const f = t.app.find;
  fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true });
  search(t, 'train');
  assert.equal(heard(t), 'Match 1 of 4 matches');
  const focus = t.doc.activeElement;
  t.grid.el.scrollTop = 400;
  t.app.changed();
  assert.equal(heard(t), '', 'a scroll that leaves the count as it was');
  t.app.changed();
  assert.equal(heard(t), '', 'a second paint');
  t.app.status.set('saving', 0, false, false);
  assert.equal(t.doc.querySelector('#announce').textContent, 'Saving…');
  assert.equal(heard(t), '', 'a status change does not speak the count again');
  assert.equal(t.doc.activeElement, focus, 'nothing moved focus');
  assert.equal(f.count.textContent, '1 of 4');
});

test('Next on a single match speaks it again, and the same text typed again is spoken', async () => {
  const t = await startApp({ server: sheet() });
  fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true });
  search(t, 'night');
  assert.equal(heard(t), 'Match 1 of 1 match');
  find(t.app.find.el, 'Next match (Enter)').click();
  assert.equal(heard(t), 'Match 1 of 1 match', 'a press of Next speaks the count though it did not change');
  search(t, 'night');
  assert.equal(heard(t), 'Match 1 of 1 match', 'a change of the field speaks even when the text is the same');
});

test('with Find closed nothing is spoken, and reopening speaks the count it finds', async () => {
  const t = await startApp({ server: sheet() });
  fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true });
  search(t, 'train');
  assert.equal(heard(t), 'Match 1 of 4 matches');
  fire(t.app.find.input, 'keydown', { key: 'Escape' });
  assert.equal(t.app.find.open, false);
  assert.equal(heard(t), '', 'closing speaks nothing');
  fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true });
  assert.equal(heard(t), 'Match 1 of 4 matches', 'reopening with the same text and count speaks the count afresh');
  fire(t.app.find.input, 'keydown', { key: 'Escape' });
  assert.equal(heard(t), '', 'closing again speaks nothing');
  click(t, 4, 1);
  t.server.setCell(1, 5, 0, 'train again');
  assert.equal(heard(t), '', 'nothing is spoken while Find is closed');
  fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true });
  assert.equal(t.app.find.count.textContent, '3 of 5');
  assert.equal(heard(t), 'Match 3 of 5 matches', 'reopening speaks the count at the selection');
});

test('an emptied field speaks nothing, and typing again speaks the count', async () => {
  const t = await startApp({ server: sheet() });
  const f = t.app.find;
  fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true });
  search(t, 'train');
  assert.equal(heard(t), 'Match 1 of 4 matches');
  search(t, '');
  assert.equal(f.count.textContent, '');
  assert.equal(heard(t), '', 'an empty field speaks nothing');
  t.server.setCell(1, 5, 0, 'train again');
  assert.equal(heard(t), '', 'with an empty field, a remote change speaks nothing');
  search(t, 'train');
  assert.equal(heard(t), 'Match 1 of 5 matches', 'typing again speaks the count');
});

test('the page declares the find region as a polite live region of its own', () => {
  const html = readFileSync(new URL('../index.html', import.meta.url), 'utf8');
  assert.match(html, /<div id="announce-find" class="sr" aria-live="polite" aria-atomic="true"><\/div>/);
});

// A browser frame runs the callbacks the app queued with requestAnimationFrame,
// so a test can paint between the frames of a snapshot as the browser does.
const frames = () => {
  const q = [];
  return { raf: (fn) => { q.push(fn); return 0; }, frame: () => { for (const fn of q.splice(0)) fn(); } };
};
// recordFind keeps every text put into #announce-find, in order. heard reads
// only the last one, so a test that needs each announcement reads this.
const recordFind = (t) => {
  const el = t.doc.querySelector('#announce-find');
  const log = [];
  let proto = Object.getPrototypeOf(el);
  let d;
  while (proto && !(d = Object.getOwnPropertyDescriptor(proto, 'textContent'))) proto = Object.getPrototypeOf(proto);
  Object.defineProperty(el, 'textContent', { configurable: true, get() { return d.get.call(this); }, set(v) { if (v !== '' && v != null) log.push(v); d.set.call(this, v); } });
  return { take: () => log.splice(0) };
};
const tabs = () => new FakeServer({ tabs: [
  { id: 1, name: 'Budget', rows: 20, cols: 5, cells: { '1,0': 'Train tickets', '1,2': 'train', '3,3': 'trains' } },
  { id: 2, name: 'Trips', rows: 20, cols: 5, cells: { '5,0': 'Train pass', '8,1': 'fare' } },
  { id: 3, name: 'Same', rows: 20, cols: 5, cells: { '0,0': 'train', '2,2': 'train', '4,4': 'train' } },
  { id: 4, name: 'Empty', rows: 20, cols: 5, cells: { '0,0': 'fare' } },
] });
// askTab asks the server for another tab and holds its answer: the messages of
// the snapshot come back one at a time, a row to a frame.
const askTab = (t, fr, server, tab) => {
  server.manual.add('watch');
  t.app.openTab(tab);
  fr.frame();
  const got = [];
  server.snapshot({ recv: (m) => got.push(m) }, t.net.generation, tab, 1);
  return got;
};
// step delivers one message of a snapshot and runs the frame after it.
const step = (t, fr, m) => { t.server.socket.recv(m); fr.frame(); };

test('a change of tab speaks the count of the tab shown once, at its end, with paints between the frames of its snapshot', async () => {
  const server = tabs();
  const fr = frames();
  const t = await startApp({ server, raf: fr.raf });
  fr.frame();
  const said = recordFind(t);
  fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true });
  search(t, 'train');
  fr.frame();
  assert.deepEqual(said.take(), ['Match 1 of 3 matches']);
  const got = askTab(t, fr, server, 2);
  assert.ok(got.length >= 4, 'the snapshot comes in several frames');
  const spoken = [];
  const shown = [];
  for (const m of got) { step(t, fr, m); spoken.push(...said.take()); shown.push(t.app.find.count.textContent); }
  assert.ok(shown.slice(0, -1).every((c) => c === ''), 'the count shows nothing while the tab loads');
  assert.equal(shown.at(-1), '1 match');
  assert.deepEqual(spoken, ['1 match'], 'the count of the tab shown is spoken once, at the end');
});

test('a change of tab to a tab whose count text is the same speaks that count once, at its end', async () => {
  const server = tabs();
  const fr = frames();
  const t = await startApp({ server, raf: fr.raf });
  fr.frame();
  const said = recordFind(t);
  fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true });
  search(t, 'train');
  fr.frame();
  assert.deepEqual(said.take(), ['Match 1 of 3 matches']);
  const spoken = [];
  for (const m of askTab(t, fr, server, 3)) { step(t, fr, m); spoken.push(...said.take()); }
  assert.equal(t.app.find.count.textContent, '1 of 3');
  assert.deepEqual(spoken, ['Match 1 of 3 matches'], 'the other tab has the same count text, and it is spoken once, at the end');
});

test('a change of tab to a tab where nothing matches speaks "No matches" once, at its end, after "No matches" on the tab left', async () => {
  const server = tabs();
  const fr = frames();
  const t = await startApp({ server, raf: fr.raf });
  fr.frame();
  const said = recordFind(t);
  fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true });
  search(t, 'zzz');
  fr.frame();
  assert.deepEqual(said.take(), ['No matches']);
  const spoken = [];
  for (const m of askTab(t, fr, server, 4)) { step(t, fr, m); spoken.push(...said.take()); }
  assert.equal(t.app.find.count.textContent, 'No matches');
  assert.deepEqual(spoken, ['No matches'], 'the tab shown has no match, and that is spoken once, at the end');
});

test('typing and Next while a tab loads speak nothing until its cells have arrived', async () => {
  const server = tabs();
  const fr = frames();
  const t = await startApp({ server, raf: fr.raf });
  fr.frame();
  const said = recordFind(t);
  fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true });
  search(t, 'train');
  fr.frame();
  assert.deepEqual(said.take(), ['Match 1 of 3 matches']);
  const got = askTab(t, fr, server, 2);
  step(t, fr, got[0]);
  step(t, fr, got[1]);
  search(t, 'pass');
  fr.frame();
  find(t.app.find.el, 'Next match (Enter)').click();
  fr.frame();
  click(t, 5, 0);
  fr.frame();
  assert.deepEqual(said.take(), [], 'typing, Next and a click while the tab loads speak nothing');
  assert.equal(t.app.find.count.textContent, '', 'the count shows nothing while the tab loads');
  const spoken = [];
  for (const m of got.slice(2)) { step(t, fr, m); spoken.push(...said.take()); }
  assert.deepEqual(spoken, ['Match 1 of 1 match'], 'the count of the tab shown is spoken at its end');
});

test('a reload of the same tab speaks its count only when the count changed', async () => {
  const server = tabs();
  const fr = frames();
  const t = await startApp({ server, raf: fr.raf });
  fr.frame();
  const said = recordFind(t);
  fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true });
  search(t, 'train');
  fr.frame();
  assert.deepEqual(said.take(), ['Match 1 of 3 matches']);
  reload(t);
  fr.frame();
  assert.equal(t.app.find.count.textContent, '1 of 3', 'the count is back after the reload');
  assert.deepEqual(said.take(), [], 'a reload that changes nothing speaks nothing');
  const tab = t.server.tab(1);
  tab.cells.delete(`${tab.rows[3]}:${tab.cols[3]}`);
  reload(t);
  fr.frame();
  assert.deepEqual(said.take(), ['Match 1 of 2 matches'], 'a count that the reload changed is spoken once');
});

test('Ctrl+F or the Find button while Find is open speaks the count again', async () => {
  const server = sheet();
  const fr = frames();
  const t = await startApp({ server, raf: fr.raf });
  fr.frame();
  const said = recordFind(t);
  fire(t.grid.input, 'keydown', { key: 'f', ctrlKey: true });
  search(t, 'train');
  fr.frame();
  assert.deepEqual(said.take(), ['Match 1 of 4 matches']);
  fire(t.app.find.input, 'keydown', { key: 'f', ctrlKey: true });
  fr.frame();
  assert.equal(t.doc.activeElement, t.app.find.input, 'the field keeps focus');
  assert.deepEqual(said.take(), ['Match 1 of 4 matches'], 'Ctrl+F while open speaks the count');
  find(t.app.top, 'Find in this tab (Ctrl or Cmd+F)').click();
  fr.frame();
  assert.deepEqual(said.take(), ['Match 1 of 4 matches'], 'the Find button while open speaks the count');
});
