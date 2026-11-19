// The fill handle and the button it leaves (rule 21a, TECH 19): a drag
// fills with mode auto, and once its ack shows that it applied, a button
// by the filled cells offers the same fill again as a copy or as a
// series, each an action of its own with the sequence of its own screen.
// The button closes on the next edit, selection change or scroll, and on
// Escape, and F6 reaches it. Ctrl+D and Ctrl+R copy and leave no button.
// The values each mode gives are the engine's (F-01 to F-05); these tests
// hold what the client sends.
'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { fire } from './dom.mjs';
import { FakeServer, find, key, startApp, type } from './server.mjs';

const ev = () => ({ preventDefault() {}, clientX: 0, clientY: 0, button: 0 });
// F-03: A1:A2 hold 1 and 2, B1 holds Mon and C1 holds =A1.
const sheet = () => new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 30, cols: 6, cells: { '0,0': '1', '1,0': '2', '0,1': 'Mon', '0,2': '=A1' } }] });
const actions = (t) => t.server.socket.frames('action');
const fills = (t) => actions(t).filter((a) => a.ops[0].op === 'fill');
const button = (t) => t.doc.querySelector('.fillopts');
const shown = (t) => !button(t).hidden;
const menuItem = (t, label) => [...(t.doc.querySelector('.menu')?.querySelectorAll('button') || [])].find((b) => b.textContent === label) || null;

// drag selects A1:C2 and drags its fill handle down to row 4.
async function drag(server = sheet()) {
  const t = await startApp({ server });
  t.sel.range(0, 0, 1, 2);
  t.app.changed();
  t.grid.h.down({ zone: 'handle', r: 1, c: 2 }, ev());
  t.grid.h.drag({ zone: 'cell', r: 3, c: 2 }, ev());
  t.grid.h.up(ev());
  return t;
}

test('F-03: a drag fills with mode auto, and its button sends the same fill as a copy with the sequence of its own screen', async () => {
  const t = await drag();
  const m = t.model;
  const cols = [[m.colAt(0), 3]];
  const first = fills(t)[0];
  assert.deepEqual(first.ops, [{ op: 'fill', tab: 1, source: { rows: [[m.rowAt(0), 2]], cols }, target: { rows: [[m.rowAt(2), 2]], cols }, mode: 'auto' }]);
  assert.equal(shown(t), true, 'the ack applied, so the button shows');
  const btn = find(button(t), 'Fill options: copy cells or fill series');
  assert.ok(btn, 'the button is named');
  assert.equal(btn.getAttribute('aria-haspopup'), 'menu');
  const corner = t.grid.cellRect(3, 2);
  assert.deepEqual([button(t).style.left, button(t).style.top], [`${Math.round(corner.right + 2)}px`, `${Math.round(corner.bottom + 2)}px`], 'it is placed by the corner of the filled cells');
  const seq = m.seq;
  assert.notEqual(seq, first.base, 'the drag fill moved the screen on');
  btn.click();
  assert.ok(menuItem(t, 'Copy cells') && menuItem(t, 'Fill series'), 'the button offers a copy and a series');
  menuItem(t, 'Copy cells').click();
  const again = fills(t)[1];
  assert.deepEqual(again.ops, [{ ...first.ops[0], mode: 'copy' }], 'the same source and target ids');
  assert.equal(again.base, seq, 'the sequence of the screen it was chosen on');
  assert.notEqual(again.n, first.n, 'an action of its own, which undo takes back alone');
  assert.equal(shown(t), false, 'the button closes with the fill it sent');
  assert.equal(t.doc.activeElement, t.grid.input);
  key(t, 'z', { ctrlKey: true });
  key(t, 'z', { ctrlKey: true });
  assert.deepEqual(actions(t).slice(-2).map((a) => a.ops), [[{ op: 'undo' }], [{ op: 'undo' }]]);
  assert.equal(fills(t).length, 2, 'no button sends anything after the undo');
  // Ctrl+D over the same cells copies the first row and shows no button.
  t.sel.range(0, 0, 3, 2);
  key(t, 'd', { ctrlKey: true });
  const down = fills(t)[2].ops[0];
  assert.deepEqual(down, { op: 'fill', tab: 1, source: { rows: [[m.rowAt(0), 1]], cols }, target: { rows: [[m.rowAt(1), 3]], cols } });
  assert.equal(shown(t), false);
});

test('F6 reaches the button from the grid and the keys choose a series', async () => {
  const t = await drag();
  key(t, 'F6');
  const btn = find(button(t), 'Fill options: copy cells or fill series');
  assert.equal(t.doc.activeElement, btn, 'F6 goes from the grid to the button');
  btn.click(); // Enter or Space on a focused button
  assert.equal(t.doc.activeElement, menuItem(t, 'Copy cells'));
  key(t, 'ArrowDown');
  assert.equal(t.doc.activeElement, menuItem(t, 'Fill series'));
  t.doc.activeElement.click();
  assert.equal(fills(t).at(-1).ops[0].mode, 'series');
  assert.deepEqual({ ...fills(t).at(-1).ops[0], mode: 'auto' }, fills(t)[0].ops[0]);
  assert.equal(shown(t), false);
});

test('the button closes on the next edit, selection change or scroll, and on Escape', async () => {
  const cases = {
    'an edit': (t) => type(t, 'x'),
    'another action': (t) => key(t, 'b', { ctrlKey: true }),
    'a move of the selection': (t) => key(t, 'ArrowDown'),
    'a click on a cell': (t) => { t.grid.h.down({ zone: 'cell', r: 8, c: 4 }, ev()); t.grid.h.up(ev()); },
    'a scroll': (t) => { t.grid.el.scrollTop = 120; fire(t.grid.el, 'scroll'); },
    'Escape in the grid': (t) => key(t, 'Escape'),
    'Escape on the button': (t) => {
      key(t, 'F6');
      key(t, 'Escape');
      assert.equal(t.doc.activeElement, t.grid.input, 'focus returns to the grid');
    },
  };
  for (const [how, act] of Object.entries(cases)) {
    const t = await drag();
    assert.equal(shown(t), true, how);
    act(t);
    assert.equal(shown(t), false, how);
    assert.equal(fills(t).length, 1, `${how}: nothing more is filled`);
  }
  // A scroll event that leaves the grid where it was, as one that comes
  // after the drag stopped scrolling it, keeps the button.
  const t = await drag();
  fire(t.grid.el, 'scroll');
  assert.equal(shown(t), true);
});

test('the button waits for the ack, and shows only for a fill that applied to a cell with nothing done since', async () => {
  for (const [how, result, moved, want] of [
    ['applied', { status: 'applied', applied: 4 }, false, true],
    ['applied to some of its cells', { status: 'applied', applied: 1, ignored: 3 }, false, true],
    ['applied after the selection moved', { status: 'applied', applied: 4 }, true, false],
    // Another person deleted the source rows first: the host leaves out a
    // count of 0.
    ['applied to no cell', { status: 'applied', ignored: 4 }, false, false],
    ['applied to 0 cells', { status: 'applied', applied: 0, ignored: 4 }, false, false],
    ['refused', { status: 'refused', reason: 'cell_limit' }, false, false],
    ['ignored', { status: 'ignored', reason: 'gone' }, false, false],
  ]) {
    const server = sheet();
    server.manual.add('action');
    const t = await drag(server);
    assert.equal(fills(t).length, 1, how);
    assert.equal(shown(t), false, `${how}: no button before the ack`);
    if (moved) key(t, 'ArrowRight');
    server.socket.recv({ type: 'ack', n: fills(t)[0].n, seq: '11', results: [result] });
    assert.equal(shown(t), want, how);
  }
});
