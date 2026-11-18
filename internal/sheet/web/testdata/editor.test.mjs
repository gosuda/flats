'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { caretByToken, tokens } from '../editor.js';
import { fire } from './dom.mjs';
import { FakeServer, key, notices, shownAt, startApp, type } from './server.mjs';

const FUNCTIONS = [
  { name: 'SUM', group: 'Math', args: ['number1', 'number2…'] },
  { name: 'SUMIF', group: 'Conditional totals', args: ['range', 'criterion', 'sum_range'] },
  { name: 'ROUND', group: 'Math', args: ['number', 'digits'] },
];
const sheet = () => new FakeServer({ tabs: [{ id: 1, name: 'Budget', rows: 30, cols: 6, cells: { '0,0': 'Item', '1,0': 'Venue', '1,1': '1200', '2,1': '900', '3,1': '=B2+B3', '4,0': { kind: 'n', value: 0.5, shown: '50%', edit: '50%' } } }] });
const lastOp = (server) => server.socket.frames('action').at(-1)?.ops[0];

test('typing replaces the content, Enter commits and moves down', async () => {
  const t = await startApp({ server: sheet() });
  key(t, 'ArrowDown');
  type(t, 'H');
  assert.equal(t.editor.open, true);
  assert.equal(t.grid.input.classList.contains('on'), true, 'the text area shows once the person types');
  type(t, 'all');
  assert.equal(t.app.bar.value, 'Hall', 'the formula bar mirrors the editor');
  key(t, 'Enter');
  assert.equal(t.editor.open, false);
  assert.equal(lastOp(t.server).text, 'Hall');
  assert.deepEqual(t.sel.active(), { r: 2, c: 0 });
  assert.equal(t.grid.input.value, '', 'the text area is empty again');
  assert.equal(t.doc.activeElement, t.grid.input);
});

test('Tab commits and moves right, Shift reverses both, Escape cancels', async () => {
  const t = await startApp({ server: sheet() });
  key(t, 'ArrowDown');
  key(t, 'ArrowRight');
  type(t, '1');
  key(t, 'Tab');
  assert.deepEqual(t.sel.active(), { r: 1, c: 2 });
  type(t, '2');
  key(t, 'Tab', { shiftKey: true });
  assert.deepEqual(t.sel.active(), { r: 1, c: 1 });
  type(t, '3');
  key(t, 'Enter', { shiftKey: true });
  assert.deepEqual(t.sel.active(), { r: 0, c: 1 });
  const sent = t.server.socket.frames('action').length;
  type(t, 'never');
  key(t, 'Escape');
  assert.equal(t.editor.open, false);
  assert.equal(t.server.socket.frames('action').length, sent, 'a cancelled edit writes nothing');
  assert.equal(shownAt(t, 0, 1), '');
});

test('Enter, F2 and a double click edit in place with the content as it would be typed', async () => {
  const t = await startApp({ server: sheet() });
  key(t, 'ArrowDown');
  key(t, 'Enter');
  assert.equal(t.editor.text, 'Venue');
  assert.equal(t.grid.input.selectionStart, 5, 'the caret is at the end');
  key(t, 'Escape');
  // a cell whose shown text differs from its edit text opens with the edit text
  t.sel.set(4, 0);
  t.app.changed();
  assert.equal(shownAt(t, 4, 0), '50%');
  key(t, 'F2');
  assert.equal(t.editor.text, '50%');
  // unchanged text commits nothing
  const sent = t.server.socket.frames('action').length;
  key(t, 'Enter');
  assert.equal(t.server.socket.frames('action').length, sent);
  t.app.grid.h.down({ zone: 'cell', r: 1, c: 1 }, { preventDefault() {} });
  t.app.grid.h.up({});
  t.app.grid.h.dbl({ zone: 'cell', r: 1, c: 1 }, {});
  assert.equal(t.editor.text, '1200');
});

test('a formula opens with its text once the peek has arrived, and keeps keys typed meanwhile', async () => {
  const server = sheet();
  const t = await startApp({ server });
  server.manual.add('peek');
  t.sel.set(3, 1);
  t.app.changed();
  key(t, 'F2');
  const peek = server.socket.frames('peek')[0];
  assert.deepEqual([peek.tab, peek.row, peek.col], [1, t.model.rowAt(3), t.model.colAt(1)]);
  assert.equal(t.editor.open, false, 'the editor waits for the formula text');
  t.grid.input.value = '+1';
  fire(t.grid.input, 'input');
  server.socket.recv({ type: 'peeked', id: peek.id, seq: '10', edit: '=B2+B3', formula: true });
  assert.equal(t.editor.open, true);
  assert.equal(t.editor.text, '=B2+B3+1');
  key(t, 'Enter');
  assert.equal(lastOp(server).text, '=B2+B3+1');
  assert.deepEqual(lastOp(server).at, [3, 1]);
});

test('an edit of a formula cell whose own edit is still pending opens with that text, as the formula bar shows it', async () => {
  const server = sheet();
  const t = await startApp({ server });
  server.manual.add('action');
  server.manual.add('peek');
  t.sel.set(3, 1);
  t.app.changed();
  type(t, '7');
  key(t, 'Enter');
  key(t, 'ArrowUp');
  assert.equal(t.app.bar.value, '7', 'the bar shows the pending text');
  key(t, 'F2');
  assert.equal(t.editor.open, true, 'nothing waits for the old formula');
  assert.equal(t.editor.text, '7');
  assert.equal(server.socket.frames('peek').length, 0);
  key(t, 'Escape');
});

test('the formula bar shows a formula text fetched on demand, and caches it until the cell changes', async () => {
  const server = sheet();
  const t = await startApp({ server });
  t.sel.set(3, 1);
  t.app.changed();
  assert.equal(t.app.bar.value, '', 'nothing is guessed before the peek');
  t.timers.advance(60);
  assert.equal(t.app.bar.value, '=B2+B3');
  key(t, 'ArrowUp');
  key(t, 'ArrowDown');
  t.timers.advance(60);
  assert.equal(server.socket.frames('peek').length, 1, 'the answer is cached');
  server.setCell(1, 3, 1, '=B2*2');
  t.timers.advance(60);
  assert.equal(server.socket.frames('peek').length, 2);
  assert.equal(t.app.bar.value, '=B2*2');
});

test('keys of a composition never commit, cancel or move', async () => {
  const t = await startApp({ server: sheet() });
  key(t, 'ArrowDown');
  fire(t.grid.input, 'compositionstart');
  assert.equal(t.editor.open, true, 'a composition starts the edit');
  t.grid.input.value = '한';
  fire(t.grid.input, 'input', { isComposing: true });
  for (const k of ['Enter', 'Tab', 'Escape', 'ArrowDown']) {
    fire(t.grid.input, 'keydown', { key: k, isComposing: true });
    fire(t.grid.input, 'keydown', { key: 'Process', keyCode: 229 });
  }
  assert.equal(t.editor.open, true);
  assert.equal(t.server.socket.frames('action').length, 0);
  assert.deepEqual(t.sel.active(), { r: 1, c: 0 });
  t.grid.input.value = '한글';
  fire(t.grid.input, 'compositionend');
  assert.equal(t.app.bar.value, '한글');
  key(t, 'Enter');
  assert.equal(lastOp(t.server).text, '한글');
});

test('Alt or Option+Enter adds a line break instead of committing', async () => {
  const t = await startApp({ server: sheet() });
  type(t, 'one');
  key(t, 'Enter', { altKey: true });
  type(t, 'two');
  assert.equal(t.editor.text, 'one\ntwo');
  assert.equal(t.editor.open, true);
  key(t, 'Enter');
  assert.equal(lastOp(t.server).text, 'one\ntwo');
});

test('after "=" a click inserts a reference, a second click replaces it, and a drag gives a range', async () => {
  const t = await startApp({ server: sheet() });
  const down = (r, c) => t.grid.h.down({ zone: 'cell', r, c }, { preventDefault() {} });
  t.sel.set(5, 3);
  t.app.changed();
  type(t, '=');
  assert.equal(t.editor.pointable(), true);
  down(1, 1);
  t.grid.h.up({});
  assert.equal(t.editor.text, '=B2');
  assert.deepEqual(t.sel.active(), { r: 5, c: 3 }, 'the selection stays on the edited cell');
  down(2, 1);
  t.grid.h.up({});
  assert.equal(t.editor.text, '=B3', 'a second click replaces the reference just inserted');
  type(t, '+');
  down(1, 1);
  t.grid.h.drag({ zone: 'cell', r: 2, c: 1 }, {});
  t.grid.h.up({});
  assert.equal(t.editor.text, '=B3+B2:B3');
  assert.equal(t.editor.open, true);
  // after a complete reference a click is an ordinary click: it commits the edit
  type(t, ' ');
  type(t, 'x');
  assert.equal(t.editor.pointable(), false);
  down(0, 0);
  t.grid.h.up({});
  assert.equal(t.editor.open, false);
  assert.equal(lastOp(t.server).text, '=B3+B2:B3 x');
  assert.deepEqual(t.sel.active(), { r: 0, c: 0 });
});

test('a reference pasted after the one a click inserted is the person\'s text, which a click does not replace', async () => {
  const t = await startApp({ server: sheet() });
  const down = (r, c) => { t.grid.h.down({ zone: 'cell', r, c }, { preventDefault() {} }); t.grid.h.up({}); };
  const input = (value, caret) => {
    const ta = t.grid.input;
    ta.value = value;
    ta.selectionStart = ta.selectionEnd = caret;
    fire(ta, 'input');
  };
  t.sel.set(5, 3);
  t.app.changed();
  type(t, '=');
  down(1, 1);
  assert.equal(t.editor.text, '=B2');
  input('=B2+B2', 6);
  assert.equal(t.editor.pointable(), false, 'the text before the caret ends with a reference the person pasted');
  down(2, 1);
  assert.equal(t.editor.open, false, 'the click commits the edit');
  assert.equal(lastOp(t.server).text, '=B2+B2');

  t.sel.set(5, 3);
  t.app.changed();
  type(t, '=+1');
  const ta = t.grid.input;
  ta.selectionStart = ta.selectionEnd = 1;
  down(1, 1);
  assert.equal(t.editor.text, '=B2+1');
  input('=B21', 3);
  assert.equal(t.editor.pointable(), true, 'text removed after the caret leaves the reference the click inserted');
  down(2, 1);
  assert.equal(t.editor.text, '=B31');
});

test('typing a function name offers the matching functions with their arguments', async () => {
  const t = await startApp({ server: sheet(), functions: FUNCTIONS });
  type(t, '=su');
  const list = t.doc.querySelector('.hint');
  assert.equal(list.getAttribute('role'), 'listbox');
  assert.deepEqual(list.querySelectorAll('.fn').map((el) => el.textContent), ['SUM(number1, number2…)', 'SUMIF(range, criterion, sum_range)']);
  assert.match(t.doc.querySelector('#announce').textContent, /2 functions match\. SUM is first\./);
  // Down chooses, Enter accepts a chosen one instead of committing
  key(t, 'ArrowDown');
  assert.equal(t.grid.input.getAttribute('aria-activedescendant'), 'fn-opt-1');
  key(t, 'Enter');
  assert.equal(t.editor.text, '=SUMIF(');
  assert.equal(t.editor.open, true);
  // inside the call the hint names the argument the caret is in
  type(t, 'A1:A9,');
  const sig = t.doc.querySelector('.hint .sig');
  assert.equal(sig.textContent, 'SUMIF(range, criterion, sum_range)');
  assert.equal(sig.querySelector('em').textContent, 'criterion');
  // Tab accepts the first suggestion
  type(t, 'rou');
  key(t, 'Tab');
  assert.equal(t.editor.text, '=SUMIF(A1:A9,ROUND(');
  // text inside quotation marks is never a function name
  type(t, '"su');
  assert.equal(t.doc.querySelector('.hint'), null);
  key(t, 'Escape');
  assert.equal(t.doc.querySelector('.hint'), null);
});

test('the formula bar edits the active cell: Enter there starts, and Enter again commits', async () => {
  const t = await startApp({ server: sheet() });
  key(t, 'ArrowDown');
  t.app.bar.focus();
  key(t, 'Enter');
  assert.equal(t.editor.open, true);
  assert.equal(t.editor.area, t.app.bar);
  t.app.bar.value = 'Venue hall';
  fire(t.app.bar, 'input');
  assert.equal(t.grid.input.value, 'Venue hall', 'the cell mirrors the bar');
  key(t, 'Enter');
  assert.equal(lastOp(t.server).text, 'Venue hall');
  assert.equal(t.doc.activeElement, t.grid.input, 'focus returns to the grid');
});

test('an edit in a row that someone deletes closes with the text offered back', async () => {
  const server = sheet();
  const t = await startApp({ server });
  key(t, 'ArrowDown');
  type(t, 'Lodging');
  server.deleteRows(1, 1, 1);
  assert.equal(t.editor.open, false);
  assert.equal(server.socket.frames('action').length, 0);
  const [[kind, text, kept]] = notices(t);
  assert.equal(kind, 'warn');
  assert.match(text, /The row, column or tab of the cell you were editing was deleted\. Nothing was written\./);
  assert.equal(kept, 'Lodging');
});

test('a plain edit follows its cell when rows are inserted above while typing', async () => {
  const server = sheet();
  const t = await startApp({ server });
  key(t, 'ArrowDown');
  type(t, 'Hall');
  const row = t.model.rowAt(1);
  server.insertRows(1, 0, 2);
  assert.equal(t.editor.open, true, 'content that is no formula holds nothing back');
  assert.equal(t.model.rowCount, 32);
  key(t, 'Enter');
  const op = lastOp(server);
  assert.equal(op.row, row, 'the edit names its cell by identity');
  assert.deepEqual(op.at, [3, 0]);
  assert.equal(server.cell(1, 3, 0).value, 'Hall');
});

test('the caret keeps its piece when a rebase changes the text', () => {
  assert.deepEqual(tokens('=B5+SUM(A1:A9,"x y")+\'My Tab\'!C3'), ['=', 'B5', '+', 'SUM', '(', 'A1', ':', 'A9', ',', '"x y"', ')', '+', "'My Tab'", '!', 'C3']);
  assert.equal(caretByToken('=B5+B6', 6, '=B6+B7'), 6);
  assert.equal(caretByToken('=B5+', 4, '=B6+'), 4);
  assert.equal(caretByToken('=B9+B10', 3, '=B10+B11'), 4, 'the caret stays at the end of the reference that grew');
  assert.equal(caretByToken('=B9+B10', 1, '=B10+B11'), 1);
  assert.equal(caretByToken('=B9+B10', 0, '=B10+B11'), 0);
  assert.equal(caretByToken('=B10+B11', 3, '=B9+B10'), 3, 'inside a reference that shrank the caret stays inside it');
  assert.equal(caretByToken('=B10+B11', 5, '=B9+B10'), 4);
  // a rename that quotes or unquotes the tab name leaves as many pieces
  assert.equal(caretByToken('=Old!A1', 1, "='New Name'!A1"), 1);
  assert.equal(caretByToken('=Old!A1', 7, "='New Name'!A1"), 14);
  assert.equal(caretByToken("='Old Name'!A1+'Old Name'!B1", 14, '=New!A1+New!B1'), 7);
  // texts of another structure, as with a reference that is gone, put the
  // caret at the end
  assert.equal(caretByToken('=A1+', 4, '=#REF!+'), 7);
  assert.equal(caretByToken('=A1+A2', 2, '=#REF!+A1'), 9);
  assert.equal(caretByToken('=A1+A2', 1, '=#REF!+A1'), 9);
});
