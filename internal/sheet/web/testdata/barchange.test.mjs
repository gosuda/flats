// The formula bar's text belongs to the cell it was read from (TECH 19.5).
// A change can apply before the bar is drawn again: a rename, an insertion
// or a sort rewrites the text of a formula that names what it moved or
// whose row it moved, another writer can replace the text, and the
// selection can move. An edit of text that is no longer the active cell's
// text is refused with its text offered back, and no key of it goes into
// the new text. With nothing typed the edit opens on the text as it is,
// and text the change left as it was is the edit's text as typed. Each case
// holds the drawing of the screen between the change and the keys.
'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { fire } from './dom.mjs';
import { FakeServer, key, notices, startApp, type } from './server.mjs';

const sheet = () => new FakeServer({ tabs: [
  { id: 1, name: 'Data', rows: 6, cols: 3, cells: { '0,0': '=Old!A1', '0,1': '=B5', '0,2': '=Other!A2', '1,0': 'abc', '1,2': '11', '2,0': '=Old!A1+Old!B1', '2,1': '=AB!A1', '2,2': 'abab', '3,0': "='Old Name'!A1", '4,0': '5', '4,1': '10' } },
  { id: 2, name: 'Old', rows: 5, cols: 3, cells: { '0,0': '10' } },
  { id: 3, name: 'Other', rows: 5, cols: 3, cells: { '1,0': '20' } },
  { id: 4, name: 'Old Name', rows: 5, cols: 3, cells: { '0,0': '10' } },
  { id: 5, name: 'AB', rows: 5, cols: 3, cells: { '0,0': '10' } },
  { id: 6, name: 'ABABAB', rows: 5, cols: 3, cells: { '0,0': '99' } },
] });

// held starts the app with its frames held until draw().
async function held() {
  const frames = [];
  const t = await startApp({ server: sheet(), raf: (fn) => frames.push(fn) });
  t.draw = () => { while (frames.length) frames.shift()(); };
  t.draw();
  t.server.manual.add('peek');
  return t;
}

// peeked answers the newest peek of the cell with text.
function peeked(t, text) {
  const p = t.server.socket.frames('peek').at(-1);
  t.server.socket.recv({ type: 'peeked', id: p.id, seq: String(t.server.seq), edit: text, formula: true });
}

// shows selects (r, c), draws the bar and answers its peek with text.
function shows(t, r, c, text) {
  t.sel.set(r, c);
  t.app.changed();
  t.draw();
  t.timers.advance(100);
  if (text.startsWith('=')) peeked(t, text);
  t.draw();
  assert.equal(t.app.bar.value, text);
}

// barInput puts value into the formula bar with the caret at caret, as
// typing does, and lets the bar read it.
function barInput(t, value, caret = value.length) {
  const bar = t.app.bar;
  bar.focus();
  bar.value = value;
  bar.selectionStart = bar.selectionEnd = caret;
  fire(bar, 'input');
}

// reach starts an edit in the formula bar with nothing typed: by a press
// on it, or by F2 once focus is there.
function reach(t, how) {
  const bar = t.app.bar;
  if (how === 'pointer') fire(bar, 'mousedown', { button: 0 });
  bar.focus();
  if (how === 'F2') key(t, 'F2');
}

const actions = (t) => t.server.socket.frames('action');
const sent = (t) => actions(t).at(-1);

// refused checks that no edit is open, that only `written` actions went
// out, and that the newest notice offers `typed` back for the reason why.
function refused(t, typed, why, written = 0) {
  assert.equal(t.editor.ed, null, 'no edit is open');
  assert.equal(actions(t).length, written, 'nothing is written');
  const [kind, text, kept] = notices(t).at(-1);
  assert.equal(kind, 'warn');
  assert.match(text, why === 'moved' ? /selection moved before the formula bar showed the new cell/ : /changed before your edit in the formula bar opened/);
  assert.equal(kept, typed);
}

// What another writer does to a formula of tab 1 before the bar is drawn
// again: [what, change, cell, text shown, text now].
const rewrites = [
  ['a rename of the tab it names', (s) => s.renameTab(2, 'New'), [0, 0], '=Old!A1', '=New!A1'],
  ['an insertion above the row it names', (s) => s.insertRows(1, 2), [0, 1], '=B5', '=B6'],
  // The sort moves the cell's row, and its reference by as many rows.
  ['a sort that moves its row', (s) => s.sortRows(1), [0, 2], '=Other!A2', '=Other!A7'],
  ['a rename that quotes the tab it names', (s) => s.renameTab(2, 'New Name'), [0, 0], '=Old!A1', "='New Name'!A1"],
  ['a rename that unquotes the tab it names', (s) => s.renameTab(4, 'New'), [3, 0], "='Old Name'!A1", '=New!A1'],
];
// What the person makes of the text the bar shows: [what, text, caret].
const edits = [
  ['added at its end', (s) => `${s}+1`, (s) => s.length + 2],
  ['inserted after its =', (s) => `=-${s.slice(1)}`, () => 2],
  ['that removed its end', (s) => s.slice(0, -1), (s) => s.length - 1],
];

for (const [name, change, at, before, now] of rewrites) {
  for (const [how, edit, caret] of edits) {
    test(`keys ${how} in a bar not drawn since ${name} are refused, and the text offered back`, async () => {
      const t = await held();
      shows(t, ...at, before);
      change(t.server);
      assert.equal(t.app.bar.value, before, 'the bar is not drawn again yet');
      const typed = edit(before);
      barInput(t, typed, caret(before));
      assert.deepEqual([!!t.editor.ed, t.editor.open], [true, false], 'the edit waits for the text of the grid on show');
      peeked(t, now);
      refused(t, typed, 'changed');
      // The bar shows the formula as it is, and an edit of that goes on.
      assert.equal(t.app.bar.value, now);
      barInput(t, `${now}+1`);
      assert.deepEqual([t.editor.open, t.editor.text], [true, `${now}+1`]);
      const base = String(t.server.seq);
      key(t, 'Enter');
      assert.deepEqual([sent(t).ops[0].text, sent(t).base], [`${now}+1`, base], 'the text is of the grid the action is based on');
    });
  }
}

// BA pasted between A and B of =AB!A1 gives =ABAB!A1, which is the text the
// formula has once AB is renamed ABAB. It was still made from text the cell
// no longer holds.
test('an edit that makes the text a rename gave the formula is refused as well', async () => {
  const t = await held();
  shows(t, 2, 1, '=AB!A1');
  t.server.renameTab(5, 'ABAB');
  barInput(t, '=ABAB!A1', 4);
  peeked(t, '=ABAB!A1');
  refused(t, '=ABAB!A1', 'changed');
  assert.equal(t.app.bar.value, '=ABAB!A1');
});

test('keys typed in a bar not drawn since another writer replaced the text are refused at once', async () => {
  for (const typed of ['abcd', 'ab', 'xabc']) {
    const t = await held();
    shows(t, 1, 0, 'abc');
    t.server.setCell(1, 1, 0, 'xyz');
    barInput(t, typed);
    refused(t, typed, 'changed');
    assert.equal(t.app.bar.value, 'xyz', 'the bar shows the text as it is');
    barInput(t, 'xyzd');
    key(t, 'Enter');
    assert.equal(sent(t).ops[0].text, 'xyzd');
  }
});

// A change that leaves the cell's text as the bar shows it: [what, change,
// cell, text].
const elsewhere = [
  ['a rename of a tab it does not name', (s) => s.renameTab(3, 'Else'), [0, 0], '=Old!A1'],
  ['an insertion below the row it names', (s) => s.insertRows(1, 5), [0, 1], '=B5'],
  ["another writer's text in another cell", (s) => s.setCell(1, 1, 0, 'xyz'), [0, 0], '=Old!A1'],
  ["another writer's text in another cell", (s) => s.setCell(1, 2, 0, 'other'), [1, 0], 'abc'],
];

for (const [name, change, at, text] of elsewhere) {
  for (const [how, edit, caret] of edits) {
    test(`keys ${how} in a bar not drawn since ${name} stand: the ${text.startsWith('=') ? 'formula' : 'text'} is as shown`, async () => {
      const t = await held();
      shows(t, ...at, text);
      change(t.server);
      const typed = edit(text);
      barInput(t, typed, caret(text));
      // A structural change drops the formula text, which is asked again.
      if (!t.editor.open) peeked(t, text);
      assert.deepEqual([t.editor.open, t.editor.text], [true, typed]);
      const base = String(t.server.seq);
      key(t, 'Enter');
      assert.deepEqual([sent(t).ops[0].text, sent(t).base], [typed, base]);
      assert.deepEqual(notices(t), []);
    });
  }
}

for (const how of ['pointer', 'F2']) {
  test(`an edit started in a bar not drawn since a rename with nothing typed opens on the formula as it is (${how})`, async () => {
    const t = await held();
    shows(t, 0, 0, '=Old!A1');
    t.server.renameTab(2, 'New');
    reach(t, how);
    assert.deepEqual([!!t.editor.ed, t.editor.open], [true, false]);
    peeked(t, '=New!A1');
    assert.deepEqual([t.editor.open, t.editor.text, t.app.bar.value], [true, '=New!A1', '=New!A1']);
    barInput(t, '=New!A1+1');
    key(t, 'Enter');
    assert.equal(sent(t).ops[0].text, '=New!A1+1');
    assert.deepEqual(notices(t), []);
  });

  test(`an edit started in a bar not drawn since another writer replaced the text with nothing typed opens on that text (${how})`, async () => {
    const t = await held();
    shows(t, 1, 0, 'abc');
    t.server.setCell(1, 1, 0, 'xyz');
    reach(t, how);
    assert.deepEqual([t.editor.open, t.editor.text, t.app.bar.value], [true, 'xyz', 'xyz']);
  });
}

test('keys typed while an edit of text a rename rewrote waits for the formula are refused', async () => {
  const t = await held();
  shows(t, 0, 0, '=Old!A1');
  t.server.renameTab(2, 'New');
  reach(t, 'F2');
  barInput(t, '=Old!A1+1');
  assert.equal(t.editor.open, false);
  peeked(t, '=New!A1');
  refused(t, '=Old!A1+1', 'changed');
  assert.equal(t.app.bar.value, '=New!A1');
});

// The answer names a grid after a change the waiting edit holds back. With
// nothing typed the edit binds nothing to the screen, also when it holds
// the text the bar showed.
test('an edit of shown text with nothing typed opens on the formula of a later grid', async () => {
  const t = await held();
  shows(t, 0, 0, '=Old!A1');
  t.server.renameTab(2, 'New');
  reach(t, 'F2');
  t.server.renameTab(2, 'Newer');
  assert.equal(t.model.tabInfo(2).name, 'New', 'the waiting edit holds the second rename');
  peeked(t, '=Newer!A1');
  assert.deepEqual([t.editor.open, t.editor.text], [true, '=Newer!A1']);
  assert.equal(t.model.tabInfo(2).name, 'Newer');
  assert.deepEqual(notices(t), []);
});

// pending commits text into A2 of tab 1 and shows it pending in the bar.
async function pending(text) {
  const t = await held();
  t.server.manual.add('action');
  t.server.manual.add('rebase');
  t.sel.set(1, 0);
  t.app.changed();
  t.draw();
  type(t, text);
  key(t, 'Enter');
  t.sel.set(1, 0);
  t.app.changed();
  t.draw();
  assert.equal(t.app.bar.value, text, 'the bar shows the pending text');
  return t;
}

// adjusted answers the exchange the edit asked for with the pending text
// as it is on the grid on show.
function adjusted(t, was, now) {
  const r = t.server.rebases.at(-1);
  assert.deepEqual([r.text, r.base, r.through], [was, '10', String(t.server.seq)], 'the pending text is adjusted from its base');
  t.server.socket.recv({ type: 'rebased', id: r.id, text: now, at: [1, 0], base: r.through });
}

// What another writer does before the bar that shows a pending formula is
// drawn again: [what, text, change, text as the exchange adjusts it].
const adjustments = [
  ['a rename of the tab it names', '=Old!A1', (s) => s.renameTab(2, 'New'), '=New!A1'],
  ['an insertion above the row of the cell it names', '=Old!A1', (s) => s.insertRows(2, 0), '=Old!A2'],
  ['a rename that quotes the tab it names', '=Old!A1', (s) => s.renameTab(2, 'New Name'), "='New Name'!A1"],
  ['a rename that unquotes the tab it names', "='Old Name'!A1", (s) => s.renameTab(4, 'New'), '=New!A1'],
  ['a rename that repeats the name of the tab it names', '=AB!A1', (s) => s.renameTab(5, 'ABAB'), '=ABAB!A1'],
];

for (const [name, text, change, now] of adjustments) {
  for (const [how, edit, caret] of [...edits, ['that repeat its name', (s) => `=${s.slice(1, 3)}${s.slice(1)}`, () => 3]]) {
    test(`keys ${how} in a bar not drawn since ${name} are refused when the exchange adjusts the pending formula`, async () => {
      const t = await pending(text);
      change(t.server);
      const typed = edit(text);
      barInput(t, typed, caret(text));
      assert.equal(t.editor.open, false);
      adjusted(t, text, now);
      refused(t, typed, 'changed', 1);
      // The bar shows nothing of text typed on another screen, and keys
      // typed there join the adjusted text.
      assert.equal(t.app.bar.value, '');
      barInput(t, '+1');
      adjusted(t, text, now);
      assert.deepEqual([t.editor.open, t.editor.text], [true, `${now}+1`]);
      const base = String(t.server.seq);
      key(t, 'Enter');
      assert.deepEqual([sent(t).ops[0].text, sent(t).base], [`${now}+1`, base]);
    });
  }
}

test('keys typed in a bar not drawn since a change the pending formula does not name stand', async () => {
  const t = await pending('=Old!A1');
  t.server.renameTab(3, 'Else');
  barInput(t, '=Old!A1+1');
  adjusted(t, '=Old!A1', '=Old!A1');
  assert.deepEqual([t.editor.open, t.editor.text], [true, '=Old!A1+1']);
  const base = String(t.server.seq);
  key(t, 'Enter');
  assert.deepEqual([sent(t).ops[0].text, sent(t).base], ['=Old!A1+1', base]);
  assert.deepEqual(notices(t), []);
});

test('an edit started in a bar that shows a pending formula with nothing typed opens on the adjusted text', async () => {
  const t = await pending('=Old!A1');
  t.server.renameTab(2, 'New Name');
  reach(t, 'F2');
  adjusted(t, '=Old!A1', "='New Name'!A1");
  assert.deepEqual([t.editor.open, t.editor.text], [true, "='New Name'!A1"]);
  assert.deepEqual(notices(t), []);
});

// The bar can still show another cell when the selection moved and the
// screen was not drawn since. Whatever was typed there was typed into the
// text of the cell the selection left: at its end, inside it, or where
// repeated text hides the place.
for (const [name, at, shown, typed, caret] of [
  ['added at the end', [1, 0], 'abc', 'abcX', 4],
  ['inserted inside', [1, 0], 'abc', 'aXbc', 2],
  ['that removed text', [1, 0], 'abc', 'ab', 2],
  ['1 inserted at the start of 11', [1, 2], '11', '111', 1],
  ['1 added at the end of 11', [1, 2], '11', '111', 3],
  ['2 added at the end of 11', [1, 2], '11', '112', 3],
  ['ab inserted inside abab', [2, 2], 'abab', 'ababab', 4],
  ['a added at the end of abab', [2, 2], 'abab', 'ababa', 5],
]) {
  test(`keys ${name} of a bar that still shows the cell the selection left are refused`, async () => {
    const t = await held();
    shows(t, ...at, shown);
    t.sel.set(4, 0);
    t.app.changed();
    assert.equal(t.app.bar.value, shown, 'the bar is not drawn again yet');
    barInput(t, typed, caret);
    refused(t, typed, 'moved');
    // The bar shows the active cell, and an edit of that goes on.
    assert.equal(t.app.bar.value, '5');
    barInput(t, '5X');
    key(t, 'Enter');
    assert.deepEqual([sent(t).ops[0].text, sent(t).ops[0].row], ['5X', t.model.rowAt(4)]);
    assert.equal(actions(t).length, 1, 'the cell the selection left is not written');
  });
}

for (const how of ['pointer', 'F2']) {
  test(`an edit started in a bar that still shows the cell the selection left with nothing typed opens on the active cell (${how})`, async () => {
    const t = await held();
    shows(t, 1, 0, 'abc');
    t.sel.set(4, 1);
    t.app.changed();
    reach(t, how);
    assert.deepEqual([t.editor.open, t.editor.text, t.app.bar.value], [true, '10', '10']);
    assert.deepEqual(t.editor.cell, { row: t.model.rowAt(4), col: t.model.colAt(1) });
    assert.deepEqual(notices(t), []);
  });
}
