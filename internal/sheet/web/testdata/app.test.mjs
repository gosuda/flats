'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { fire, htmlWrites, textNodes } from './dom.mjs';
import { FakeServer, find, key, notices, shownAt, startApp, type } from './server.mjs';

const budget = () => new FakeServer({ tabs: [
  { id: 1, name: 'Budget', rows: 30, cols: 6, cells: { '0,0': 'Item', '0,1': 'Amount', '1,0': 'Venue', '1,1': '1200', '2,0': 'Food', '2,1': '900', '3,1': '=B2+B3' } },
  { id: 2, name: 'Notes', rows: 10, cols: 3, cells: { '0,0': 'Remember' } },
] });

test('the first screen shows the first tab, Saved, and the tab list', async () => {
  const t = await startApp({ server: budget() });
  assert.equal(t.model.tab.id, 1);
  assert.equal(shownAt(t, 1, 0), 'Venue');
  assert.equal(shownAt(t, 1, 1), '1200');
  assert.equal(t.app.status.el.lastChild.textContent, 'Saved');
  assert.deepEqual(t.app.tabs.list.children.map((el) => [el.textContent, el.getAttribute('aria-selected')]), [['Budget', 'true'], ['Notes', 'false']]);
  assert.equal(location.hash, '#/1');
  assert.equal(t.app.nameBox.textContent, 'A1');
  assert.equal(t.app.bar.value, 'Item');
  assert.deepEqual(htmlWrites, []);
});

test('the fragment names the tab to open', async () => {
  const t = await startApp({ server: budget(), hash: '#/2' });
  assert.equal(t.model.tab.id, 2);
  assert.equal(shownAt(t, 0, 0), 'Remember');
});

test('a committed edit shows its typed text as pending until the server answers', async () => {
  const server = budget();
  const t = await startApp({ server });
  server.manual.add('action');
  key(t, 'ArrowDown');
  key(t, 'ArrowRight');
  type(t, '1500');
  key(t, 'Enter');
  const action = server.socket.frames('action')[0];
  assert.deepEqual(action, { type: 'action', n: '1', base: '10', ops: [{ op: 'set', tab: 1, row: t.model.rowAt(1), col: t.model.colAt(1), text: '1500', at: [1, 1] }] });
  // the screen shows the typed text, marked pending; the server value is not guessed
  assert.equal(shownAt(t, 1, 1), '1500');
  assert.ok(t.grid.rowEls.get(t.model.rowAt(1))._c[1].classList.contains('pend'));
  assert.equal(t.model.cell(t.model.rowAt(1), t.model.colAt(1)).value, 1200);
  assert.equal(t.app.status.el.lastChild.textContent, 'Saving…');
  assert.deepEqual(t.sel.active(), { r: 2, c: 1 }, 'Enter moved down');
  // the delta replaces the pending text with the server's value, and dependents arrive with it
  server.socket.recv({ type: 'delta', seq: '11', from: { session_id: 's1', n: '1' }, cells: [[t.model.rowAt(1), [[t.model.colAt(1), 'n', 1500, '$1,500']]], [t.model.rowAt(3), [[t.model.colAt(1), 'nf', 2400]]]] });
  assert.equal(shownAt(t, 1, 1), '$1,500');
  assert.equal(shownAt(t, 3, 1), '2400');
  assert.equal(t.model.pending.size, 0);
  assert.equal(t.app.status.el.lastChild.textContent, 'Saving…', 'Saved waits for the ack');
  server.socket.recv({ type: 'ack', n: '1', seq: '11', results: [{ status: 'applied', applied: 1 }] });
  assert.equal(t.app.status.el.lastChild.textContent, 'Saved');
  assert.deepEqual(notices(t), []);
});

// Acks that do not fit Appendix A. None comes from the host on a normal
// path; each stands for an answer that came out wrong.
const BAD_ACKS = {
  'results that are an object': { results: {} },
  'no results': { results: undefined },
  'null results': { results: null },
  'fewer results than operations': { results: [] },
  'more results than operations': { results: [{ status: 'applied', applied: 1 }, { status: 'applied', applied: 1 }] },
  'a result that is no object': { results: ['applied'] },
  'a result of an unknown status': { results: [{ status: 'saved' }] },
  'a count that is no whole number': { results: [{ status: 'applied', applied: '1' }] },
  'a reason that is no text': { results: [{ status: 'refused', reason: 7 }] },
  'no sequence': { seq: undefined },
  'an undo outcome that is no object': { undo: 'none' },
  'an undo outcome that is null': { undo: null },
  'a reason that is null': { results: [{ status: 'applied', applied: 1, reason: null }] },
};

for (const [name, fields] of Object.entries(BAD_ACKS)) {
  test(`an ack with ${name} keeps the edit pending, and the next connection asks again`, async () => {
    const server = budget();
    const t = await startApp({ server });
    server.manual.add('action');
    key(t, 'ArrowDown');
    key(t, 'ArrowRight');
    type(t, '1500');
    key(t, 'Enter');
    const [a] = t.net.pending;
    const ws = server.socket;
    const ack = { type: 'ack', n: a.n, seq: '11', results: [{ status: 'applied', applied: 1 }], ...fields };
    assert.throws(() => ws.recv(ack), TypeError);
    assert.deepEqual(t.net.pending.map((x) => x.n), [a.n], 'the action stays queued');
    assert.equal(shownAt(t, 1, 1), '1500', 'and its text stays pending on screen');
    assert.notEqual(t.app.status.el.lastChild.textContent, 'Saved');
    assert.deepEqual(notices(t), [], 'nothing is said of an outcome');
    assert.equal(t.net.open, false, 'the connection ended');
    // The next connection sends the action again with its number.
    server.manual.delete('action');
    t.timers.advance(1000);
    server.socket.open();
    assert.deepEqual(server.socket.frames('action').map((x) => x.n), [a.n]);
    assert.equal(t.net.pending.length, 0);
    assert.equal(t.app.status.el.lastChild.textContent, 'Saved');
    assert.equal(server.cell(1, 1, 1).value, 1500);
  });
}

test('a refused edit returns the grid to the server state and offers the text back', async () => {
  const server = budget();
  const t = await startApp({ server });
  server.refuse = { status: 'refused', reason: 'disk_quota' };
  key(t, 'ArrowDown');
  type(t, 'Second shuttle');
  key(t, 'Enter');
  assert.equal(shownAt(t, 1, 0), 'Venue', 'the optimistic text is gone');
  assert.equal(t.model.pending.size, 0);
  const [[kind, text, kept]] = notices(t);
  assert.equal(kind, 'bad');
  assert.match(text, /The host refused this edit of A2: the sheet is over its disk quota/);
  assert.match(text, /Nothing was changed\. Your text:/);
  assert.equal(kept, 'Second shuttle');
  assert.equal(t.app.status.el.lastChild.textContent, 'Saved');
  // the notice is spoken without moving focus: it is added to a polite live region
  assert.equal(t.app.toasts.getAttribute('aria-live'), 'polite');
  assert.equal(t.doc.activeElement, t.grid.input);
});

test('an ignored edit and a paste that partly applied say so with the text', async () => {
  const server = budget();
  const t = await startApp({ server });
  server.refuse = { status: 'ignored', reason: 'gone' };
  type(t, 'Lodging');
  key(t, 'Enter');
  assert.match(notices(t)[0][1], /Your edit of A1 was ignored: its row, column or tab was deleted before the host received it/);
  assert.equal(notices(t)[0][2], 'Lodging');
  server.refuse = { status: 'applied', applied: 4, ignored: 4 };
  t.app.paste('a\tb\nc\td\ne\tf\ng\th');
  assert.match(notices(t)[1][1], /Your paste applied to 4 of its 8 cells/);
  assert.equal(notices(t)[1][2], 'a\tb\nc\td\ne\tf\ng\th');
});

test('a pasted formula the host can no longer adjust is refused with the text offered back', async () => {
  const server = budget();
  const t = await startApp({ server });
  key(t, 'ArrowRight');
  key(t, 'ArrowRight');
  // the host reads a pasted formula against the screen of the action's base, as it does a typed one
  server.refuse = { status: 'refused', reason: 'stale_formula' };
  t.app.paste('=B2+B3\t7');
  assert.equal(server.socket.frames('action').at(-1).base, '10');
  const [[kind, text, kept]] = notices(t);
  assert.equal(kind, 'bad');
  assert.match(text, /The host refused this paste: the grid changed since the formula was typed and the host can no longer adjust its references\. Nothing was changed\. Your text:/);
  assert.equal(kept, '=B2+B3\t7');
  assert.equal(t.model.pending.size, 0, 'no pasted text stays on screen');
  assert.equal(shownAt(t, 0, 2), '');
});

test('undo and redo are requests to the server, and the answer says what was skipped', async () => {
  const server = budget();
  const t = await startApp({ server });
  key(t, 'z', { ctrlKey: true });
  assert.deepEqual(server.socket.frames('action').at(-1).ops, [{ op: 'undo' }]);
  assert.deepEqual(notices(t), [], 'a full undo needs no notice');
  key(t, 'z', { metaKey: true, shiftKey: true });
  assert.deepEqual(server.socket.frames('action').at(-1).ops, [{ op: 'redo' }]);
  key(t, 'y', { ctrlKey: true });
  assert.deepEqual(server.socket.frames('action').at(-1).ops, [{ op: 'redo' }]);
  server.undoResult = { applied: 1, skipped: 2, reason: 'changed' };
  find(t.app.toolbar.el, 'Undo (Ctrl or Cmd+Z)').click();
  assert.match(notices(t).at(-1)[1], /Your undo skipped 2 changes: someone else changed the same value after you\. Their value stays\./);
  server.undoResult = { applied: 0, skipped: 1, reason: 'written' };
  key(t, 'z', { ctrlKey: true });
  assert.match(notices(t).at(-1)[1], /someone else has written in the rows, columns or tab it would remove/);
  server.undoResult = { applied: 0, skipped: 1, reason: 'sorted' };
  key(t, 'z', { ctrlKey: true });
  assert.match(notices(t).at(-1)[1], /someone else sorted these rows after you/);
  // C-07: when the stack is empty the client says so
  server.undoResult = { applied: 0, skipped: 0 };
  key(t, 'z', { ctrlKey: true });
  assert.equal(notices(t).at(-1)[1], 'No earlier undo is available.');
});

test('Delete clears content optimistically and Ctrl+B asks for bold on the selection', async () => {
  const server = budget();
  const t = await startApp({ server });
  server.manual.add('action');
  key(t, 'ArrowDown');
  key(t, 'ArrowDown', { shiftKey: true });
  key(t, 'Delete');
  const m = t.model;
  assert.deepEqual(server.socket.frames('action')[0].ops, [{ op: 'clear', tab: 1, rows: [[m.rowAt(1), 2]], cols: [[m.colAt(0), 1]] }]);
  assert.equal(shownAt(t, 1, 0), '');
  assert.equal(shownAt(t, 2, 0), '');
  key(t, 'b', { ctrlKey: true });
  assert.deepEqual(server.socket.frames('action')[1].ops, [{ op: 'format', tab: 1, rows: [[m.rowAt(1), 2]], cols: [[m.colAt(0), 1]], set: { bold: true } }]);
});

test('overwritten content brings a notice with the text this tab typed', async () => {
  const server = budget();
  const t = await startApp({ server });
  key(t, 'ArrowDown');
  key(t, 'ArrowRight');
  type(t, '104');
  key(t, 'Enter');
  server.setCell(1, 1, 1, '96');
  server.socket.recv({ type: 'overwritten', tab: 1, row: t.model.rowAt(1), col: t.model.colAt(1), replaced: '11' });
  const [[kind, text, kept]] = notices(t);
  assert.equal(kind, 'warn');
  assert.equal(text, 'Someone changed B2 after you typed in it. The value the host received later stays. Your text:');
  assert.equal(kept, '104');
  assert.equal(shownAt(t, 1, 1), '96');
});

// commitAt types text into B2 and commits it. With the server answering
// by itself the edit applies at once.
function commitAt(t, text, r = 1, c = 1) {
  t.sel.set(r, c);
  type(t, text);
  key(t, 'Enter');
}

test('a late overwrite notice offers the text of the commit it names, and no later edit of the cell', async () => {
  const server = budget();
  const t = await startApp({ server });
  const row = t.model.rowAt(1);
  const col = t.model.colAt(1);
  const overwritten = (replaced) => {
    server.socket.recv({ type: 'overwritten', tab: 1, row, col, replaced });
    return notices(t).at(-1)[2];
  };
  commitAt(t, 'first');
  assert.equal(server.seq, 11, 'the first edit applied at 11');
  // Another writer replaces it at 12. Before the notice arrives, this tab
  // types again in the same cell, and the host has not answered yet.
  server.setCell(1, 1, 1, '96');
  server.manual.add('action');
  commitAt(t, 'second');
  assert.equal(overwritten('11'), 'first', 'the notice names the first commit');
  // The second edit is refused. A notice can name only a commit of this tab.
  server.socket.recv({ type: 'ack', n: '2', seq: '12', results: [{ status: 'refused', reason: 'quota' }] });
  assert.equal(overwritten('12'), null, 'the refused edit committed nothing that was replaced');
  // A third edit is waiting for its answer when a notice names the
  // sequence it will have: nothing is known of it yet.
  commitAt(t, 'third');
  assert.equal(overwritten('13'), null, 'an edit without a known sequence is offered by no notice');
  server.seq = 13;
  server.socket.recv({ type: 'delta', seq: '13', from: { session_id: 's1', n: '3' }, cells: [[row, [[col, 't', 'third']]]] });
  assert.equal(overwritten('13'), 'third', 'its delta tells the sequence before its ack does');
  server.socket.recv({ type: 'ack', n: '3', seq: '13', results: [{ status: 'applied', applied: 1 }] });
  assert.equal(overwritten('11'), 'first', 'the first commit is still offered under its own sequence');
});

test('an overwrite notice whose sequence or cell does not fit offers no text', async () => {
  const server = budget();
  const t = await startApp({ server });
  commitAt(t, 'first');
  const row = t.model.rowAt(1);
  const col = t.model.colAt(1);
  for (const bad of [{ replaced: 11 }, { replaced: '011' }, { replaced: '' }, { row: String(row) }, { col: 0 }]) {
    assert.throws(() => server.socket.recv({ type: 'overwritten', tab: 1, row, col, replaced: '11', ...bad }), TypeError, JSON.stringify(bad));
  }
  assert.deepEqual(notices(t), [], 'none is shown');
  server.socket.recv({ type: 'overwritten', tab: 1, row, col, replaced: '11' });
  assert.equal(notices(t).at(-1)[2], 'first');
});

test('the text kept for overwrite notices has a bound in memory', async () => {
  const server = budget();
  const t = await startApp({ server });
  // Each text is as long as a cell's content may be.
  const long = (i) => String(i).padStart(32767, 'x');
  for (let i = 0; i < 60; i++) commitAt(t, long(i), i % 30, 2 + Math.floor(i / 30));
  let held = 0;
  for (const text of t.app.commits.values()) held += 2 * text.length;
  assert.ok(held <= 2 << 20, `the notices keep ${held} bytes of text`);
  const offered = (i) => {
    server.socket.recv({ type: 'overwritten', tab: 1, row: t.model.rowAt(i % 30), col: t.model.colAt(2 + Math.floor(i / 30)), replaced: String(11 + i) });
    return notices(t).at(-1)[2];
  };
  assert.equal(offered(59), long(59), 'the newest commit is kept');
  assert.equal(offered(0), null, 'the oldest gave way');
});

// pasteText pastes text from another program over the selection.
function pasteText(t, text) {
  const data = { 'text/plain': text };
  fire(t.doc, 'paste', { clipboardData: { getData: (k) => data[k], setData() {} }, preventDefault() {} });
}

test('a paste files the text of each of its cells at the sequence of its delta or of an ack that applied it, and none of a refused one', async () => {
  const server = budget();
  const t = await startApp({ server });
  const row = t.model.rowAt(1);
  const [b, c, d] = [1, 2, 3].map((i) => t.model.colAt(i));
  const overwritten = (col, replaced) => {
    server.socket.recv({ type: 'overwritten', tab: 1, row, col, replaced });
    return notices(t).at(-1)[2];
  };
  // The host writes B2 and D2 and ignores C2, whose column it no longer has.
  server.manual.add('action');
  t.sel.range(1, 1, 1, 3);
  pasteText(t, 'one\ttwo\tthree');
  server.seq = 11;
  server.socket.recv({ type: 'delta', seq: '11', from: { session_id: 's1', n: '1' }, cells: [[row, [[b, 't', 'one'], [d, 't', 'three']]]] });
  server.socket.recv({ type: 'ack', n: '1', seq: '11', results: [{ status: 'applied', applied: 2, ignored: 1 }] });
  assert.equal(overwritten(b, '11'), 'one');
  assert.equal(overwritten(d, '11'), 'three');
  // The delta does not say which cell the host ignored: C2 is filed too,
  // though a host never names it, because this session did not write it.
  assert.equal(overwritten(c, '11'), 'two');
  // With no delta, the ack files every cell.
  pasteText(t, 'four\tfive\tsix');
  server.seq = 12;
  server.socket.recv({ type: 'ack', n: '2', seq: '12', results: [{ status: 'applied', applied: 2, ignored: 1 }] });
  assert.equal(overwritten(b, '12'), 'four');
  assert.equal(overwritten(d, '12'), 'six');
  // A refused paste committed nothing, and one the ack confirms whole files every cell.
  t.sel.range(2, 1, 2, 3);
  pasteText(t, 'no\tno\tno');
  server.socket.recv({ type: 'ack', n: '3', seq: '12', results: [{ status: 'refused', reason: 'quota' }] });
  server.socket.recv({ type: 'overwritten', tab: 1, row: t.model.rowAt(2), col: b, replaced: '12' });
  assert.equal(notices(t).at(-1)[2], null);
  t.sel.range(1, 1, 1, 3);
  pasteText(t, 'x\ty\tz');
  server.seq = 13;
  server.socket.recv({ type: 'ack', n: '4', seq: '13', results: [{ status: 'applied', applied: 3 }] });
  assert.deepEqual([b, c, d].map((col) => overwritten(col, '13')), ['x', 'y', 'z']);
});

test('the pasted text kept for overwrite notices has the same bound as typed text', async () => {
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Big', rows: 300, cols: 3 }] });
  const t = await startApp({ server });
  t.sel.range(0, 0, 299, 0);
  pasteText(t, Array.from({ length: 300 }, (_, i) => `v${i}`).join('\n'));
  assert.equal(t.app.commits.size, 200, 'the newest 200 cells are kept');
  const offered = (r) => {
    server.socket.recv({ type: 'overwritten', tab: 1, row: t.model.rowAt(r), col: t.model.colAt(0), replaced: '11' });
    return notices(t).at(-1)[2];
  };
  assert.equal(offered(299), 'v299');
  assert.equal(offered(0), null);
  const long = 'x'.repeat(32767);
  t.sel.range(0, 1, 59, 1);
  pasteText(t, Array.from({ length: 60 }, () => long).join('\n'));
  let held = 0;
  for (const text of t.app.commits.values()) held += 2 * text.length;
  assert.ok(held <= 2 << 20, `the notices keep ${held} bytes of text`);
});

test('an agent write shows a notice and highlights the cells it changed for a while', async () => {
  const server = budget();
  const t = await startApp({ server });
  server.setCell(1, 5, 0, 'Train', { note: { agent: true, tab: 1, cells: 42 } });
  assert.deepEqual(notices(t)[0].slice(0, 2), ['ok', 'The agent wrote 42 cells in Budget.']);
  const cell = t.grid.rowEls.get(t.model.rowAt(5))._c[0];
  assert.ok(cell.classList.contains('flash'));
  t.timers.advance(5000);
  assert.equal(cell.classList.contains('flash'), false);
});

test('an agent call that wrote in several tabs flashes its cells in the open tab', async () => {
  const server = budget();
  const t = await startApp({ server });
  server.setCell(1, 5, 0, 'Train', { note: { agent: true, tab: 0, cells: 9 } });
  const cell = t.grid.rowEls.get(t.model.rowAt(5))._c[0];
  assert.ok(cell.classList.contains('flash'));
});

test('width, freeze, rename and move arrive as deltas and need no new snapshot (C-09)', async () => {
  const server = budget();
  const t = await startApp({ server });
  const second = server.connect();
  second.onmessage = () => {};
  const heads = () => server.sockets[0].sent.filter((m) => m.type === 'watch').length;
  const m = t.model;
  t.app.setWidth(1, 1, 240);
  assert.equal(m.widthOf(m.colAt(1)), 240);
  assert.equal(t.grid.cols.size(1), 240);
  t.app.freeze(1, null);
  assert.equal(m.tab.frozenRows, 1);
  assert.equal(t.grid.top.children.length, 2, 'the frozen row is drawn in the sticky block');
  t.app.tabs.cb.rename(1, 'Costs');
  assert.equal(t.app.tabs.list.children[0].textContent, 'Costs');
  t.app.tabs.cb.move(1, null);
  assert.deepEqual(t.app.tabs.list.children.map((el) => el.textContent), ['Notes', 'Costs']);
  // undo and redo of each are ordinary actions whose effect travels the same way
  key(t, 'z', { ctrlKey: true });
  server.push({ tabs: [{ id: 1, name: 'Budget', rows: 30, cols: 6, frozen_rows: 0, frozen_cols: 0 }, { id: 2, name: 'Notes', rows: 10, cols: 3, frozen_rows: 0, frozen_cols: 0 }], tab: 1, widths: [[m.colAt(1), 104]] });
  assert.deepEqual(t.app.tabs.list.children.map((el) => el.textContent), ['Budget', 'Notes']);
  assert.equal(m.tab.frozenRows, 0);
  assert.equal(t.grid.cols.size(1), 104);
  assert.equal(heads(), 1, 'one watch for the whole session');
});

test('switching tabs releases the old tab and watches the new one', async () => {
  const server = budget();
  const t = await startApp({ server });
  key(t, 'ArrowDown');
  t.app.tabs.list.children[1].click();
  assert.equal(t.model.tab.id, 2);
  assert.equal(shownAt(t, 0, 0), 'Remember');
  assert.deepEqual(server.socket.frames('watch').map((w) => [w.generation, w.tab]), [[1, 1], [2, 2]]);
  assert.equal(location.hash, '#/2');
  // coming back restores where the person was on that tab; it is theirs alone and not saved
  t.app.tabs.list.children[0].click();
  assert.deepEqual(t.sel.active(), { r: 1, c: 0 });
});

test('the tab on screen is deleted by someone else: another tab opens with a notice', async () => {
  const server = budget();
  const t = await startApp({ server });
  server.tabs.shift();
  server.push({ tabs: server.tabInfo() });
  assert.equal(t.model.tab.id, 2);
  assert.match(notices(t)[0][1], /The tab “Budget” was deleted/);
});

test('the calculating state stops editing and says why', async () => {
  const server = budget();
  const t = await startApp({ server });
  server.socket.recv({ type: 'status', calculating: true });
  assert.equal(t.app.status.el.lastChild.textContent, 'Calculating…');
  assert.match(t.app.banners.textContent, /The sheet is calculating/);
  type(t, 'x');
  assert.equal(t.editor.open, false);
  assert.match(notices(t)[0][1], /The sheet is calculating\. Editing is available again when it ends\./);
  assert.equal(server.socket.frames('action').length, 0);
  server.socket.recv({ type: 'status', calculating: false });
  assert.equal(t.app.status.el.lastChild.textContent, 'Saved');
  type(t, 'x');
  assert.equal(t.editor.open, true);
});

test('offline keeps unconfirmed edits on screen, resends them once and warns before unload', async () => {
  const server = budget();
  const t = await startApp({ server });
  const unload = () => fire(globalThis.window, 'beforeunload', { returnValue: undefined });
  assert.equal(unload().defaultPrevented, false, 'nothing unsaved, no warning');
  server.socket.drop();
  assert.equal(t.app.status.el.lastChild.textContent, 'Offline');
  key(t, 'ArrowDown');
  type(t, 'Hall');
  key(t, 'Enter');
  assert.equal(shownAt(t, 1, 0), 'Hall', 'the edit stays on screen');
  assert.equal(t.app.status.el.lastChild.textContent, 'Offline · 1 unsaved');
  assert.match(t.app.banners.textContent, /Connection lost\. Your 1 unsaved edit stays on this screen and is sent again when the connection returns\. Each applies once\./);
  assert.equal(unload().defaultPrevented, true);
  find(t.app.banners, 'Retry now').click();
  server.socket.open();
  assert.equal(server.cell(1, 1, 0).value, 'Hall');
  assert.equal(server.applied.length, 1);
  assert.equal(t.app.status.el.lastChild.textContent, 'Saved');
  assert.equal(t.app.banners.children.length, 0);
  assert.equal(unload().defaultPrevented, false);
});

test('a rollback leaves the page editable: the calculating state ends with the connection that saw it', async () => {
  const server = budget();
  const t = await startApp({ server });
  // the host says it calculates, swaps the sheet, and tells the old connection that it ended
  const before = server.socket;
  before.recv({ type: 'status', calculating: true });
  assert.equal(t.app.readOnly, true);
  before.recv({ type: 'reset', seq: '11' });
  before.recv({ type: 'status', calculating: false });
  server.seq = 11;
  server.socket.open();
  assert.notEqual(server.socket, before);
  assert.equal(t.app.status.el.lastChild.textContent, 'Saved');
  assert.equal(t.app.banners.textContent.includes('calculating'), false);
  type(t, 'after');
  key(t, 'Enter');
  assert.equal(server.cell(1, 0, 0).value, 'after');
});

test('a tab whose snapshot the connection cut short is watched again after the reconnect', async () => {
  const server = budget();
  server.manual.add('watch');
  const t = await startApp({ server });
  // the head and nothing else arrives before the connection is lost
  const tab = server.tab(1);
  server.socket.recv({ type: 'tab_head', generation: 1, tab: 1, seq: String(server.seq), date: server.date, tabs: server.tabInfo(), rows: [[tab.rows[0], tab.rows.length]], cols: [[tab.cols[0], tab.cols.length]], widths: {}, frozen_rows: 0, frozen_cols: 0, styles: [{}] });
  assert.equal(t.model.tab.loading, true);
  server.socket.drop();
  server.manual.delete('watch');
  t.timers.advance(1000);
  const hello = server.socket.open();
  // A stream that is resumed carries deltas, not the rows that never came:
  // the hello does not ask to go on from a grid that is not whole, and the
  // tab is asked for again.
  assert.deepEqual([hello.tab, hello.watermark], [undefined, undefined]);
  assert.equal(server.socket.frames('watch').length, 1);
  assert.equal(t.model.tab.loading, false);
  assert.equal(shownAt(t, 1, 0), 'Venue');
});

test('a sheet that was deleted or renamed says so', async () => {
  for (const [code, words] of [['deleted', /This sheet was deleted\./], ['renamed', /This sheet was renamed and has a new address\./]]) {
    const server = budget();
    const t = await startApp({ server });
    server.socket.recv({ type: 'fatal', code });
    assert.match(t.app.banners.textContent, words);
    assert.equal(t.app.status.el.lastChild.textContent, 'Editing stopped · 0 unsaved');
  }
});

test('a stopped session shows the unconfirmed text for copying and never renumbers it', async () => {
  const server = budget();
  const t = await startApp({ server });
  server.manual.add('action');
  type(t, 'Not saved');
  key(t, 'Enter');
  server.socket.drop();
  server.sessions.clear();
  t.timers.advance(1000);
  server.socket.open();
  assert.equal(t.app.status.el.lastChild.textContent, 'Editing stopped · 1 unsaved');
  assert.match(t.app.banners.textContent, /Editing stopped\. The host no longer knows this browser tab’s session/);
  find(t.app.banners, 'Copy edits').click();
  await Promise.resolve();
  assert.equal(globalThis.__clipboard, 'edit of A1: Not saved');
  // editing is off, and the person is told why
  type(t, 'more');
  assert.equal(t.editor.open, false);
  assert.match(notices(t).at(-1)[1], /Editing has stopped in this browser tab/);
  // reload asks first, inline
  find(t.app.banners, 'Reload').click();
  assert.ok(find(t.app.banners, 'Reload and discard'));
  find(t.app.banners, 'Cancel').click();
  assert.equal(location.reloads, 0);
});

test('a route without a verified identity asks for a name before editing', async () => {
  const server = budget();
  server.me = null;
  const t = await startApp({ server });
  const dialog = t.doc.querySelector('.dialog');
  assert.equal(dialog.getAttribute('role'), 'dialog');
  assert.equal(dialog.getAttribute('aria-label'), 'Your name on this sheet');
  const input = dialog.querySelector('input');
  assert.equal(t.doc.activeElement, input, 'the dialog takes focus');
  // without a name the sheet can be read, not edited
  find(dialog, 'Read only').click();
  assert.equal(t.doc.activeElement, t.grid.input, 'focus returns to the grid');
  type(t, 'x');
  assert.equal(t.editor.open, false);
  assert.equal(server.presence.length, 0, 'no presence before a name');
  // the refusal offered the dialog again
  find(t.doc.querySelector('.dialog'), 'Read only').click();
  // a reconnect does not ask again; the banner keeps the offer
  server.socket.drop();
  t.timers.advance(1000);
  server.socket.open();
  assert.equal(t.doc.querySelector('.dialog'), null);
  find(t.app.banners, 'Choose a name').click();
  const again = t.doc.querySelector('.dialog');
  again.querySelector('input').value = '  Dana ';
  find(again, 'Continue').click();
  // The name opens a new session: the server keeps the label a session was opened with.
  const hello = server.socket.open();
  assert.equal(hello.name, 'Dana');
  assert.equal(hello.session, undefined);
  assert.deepEqual(t.net.me, { label: 'Dana', verified: false });
  assert.equal(globalThis.localStorage.getItem('flats-sheet-name'), 'Dana');
  assert.equal(t.app.needName, false);
  type(t, 'x');
  assert.equal(t.editor.open, true);
});

// named opens a sheet on a route without a verified identity and gives a
// name in the dialog, as a person does on first opening. The connection
// that carries the name is not opened yet. The page draws when the test
// says so, as a browser draws between tasks. frame draws what is due and
// returns the banners and the status a person then sees.
async function named() {
  const server = budget();
  server.me = null;
  const due = [];
  const t = await startApp({ server, raf: (fn) => due.push(fn) });
  const frame = () => {
    for (const fn of due.splice(0)) fn();
    return [t.app.banners.children.map((el) => el.getAttribute('aria-label')), t.app.status.el.lastChild.textContent];
  };
  assert.deepEqual(frame(), [[], 'Saved'], 'the dialog is open over a sheet that can be read');
  t.doc.querySelector('.dialog input').value = 'Dana';
  key(t, 'Enter');
  assert.equal(t.doc.querySelector('.dialog'), null, 'the dialog closed');
  return { t, server, frame };
}

test('a name just given is not asked for again while its connection opens', async () => {
  const { t, server, frame } = await named();
  // Nothing appears above the grid, so no cell moves under the pointer,
  // and the connection the page opened itself is not called lost.
  assert.deepEqual(frame(), [[], 'Connecting…']);
  // A key pressed meanwhile does not bring the dialog back.
  type(t, 'x');
  assert.equal(t.editor.open, false);
  assert.equal(t.doc.querySelector('.dialog'), null);
  assert.doesNotMatch(notices(t).at(-1)[1], /name/);
  assert.deepEqual(frame(), [[], 'Connecting…']);
  assert.equal(server.socket.open().name, 'Dana');
  assert.deepEqual(frame(), [[], 'Saved']);
  type(t, 'x');
  assert.equal(t.editor.open, true);
});

test('a name the host did not take is offered again', async () => {
  const { t, server, frame } = await named();
  // The welcome of the new session carries no label.
  server.manual.add('hello');
  server.socket.open();
  server.manual.delete('hello');
  server.socket.recv({ type: 'welcome', access: 'private', seq: String(server.seq), date: server.date, tabs: server.tabInfo(), session: 'tok9', session_id: 's9', me: { verified: false } });
  assert.deepEqual(frame(), [['Name'], 'Saved']);
  assert.equal(t.doc.querySelector('.dialog'), null, 'the name is asked once; the banner keeps the offer');
  type(t, 'x');
  assert.equal(t.editor.open, false);
  assert.ok(t.doc.querySelector('.dialog'), 'an edit offers the dialog');
});

test('a connection that fails after the name was given is told, and the name is offered', async () => {
  const { t, server, frame } = await named();
  server.socket.drop();
  assert.deepEqual(frame(), [['Connection lost', 'Name'], 'Offline']);
  // The next connection carries the name all the same.
  t.timers.advance(1000);
  assert.equal(server.socket.open().name, 'Dana');
  assert.deepEqual(frame(), [[], 'Saved']);
  assert.equal(t.app.needName, false);
});

test('CSV download goes through the route of the open tab', async () => {
  const t = await startApp({ server: budget() });
  const made = [];
  const create = t.doc.createElement.bind(t.doc);
  t.doc.createElement = (tag) => { const el = create(tag); if (tag === 'a') made.push(el); return el; };
  find(t.app.top, 'Download this tab as CSV').click();
  assert.equal(made[0].getAttribute('href'), '/_sheet/csv?tab=1');
  assert.equal(made[0].getAttribute('download'), 'Budget.csv');
});

test('cell text only ever becomes text, whatever it holds', async () => {
  const server = new FakeServer({ tabs: [{ id: 1, name: 'T', rows: 5, cols: 3, cells: { '0,0': '<img src=x onerror=alert(1)>', '1,0': 'https://example.com', '2,0': '<b>bold</b>' } }] });
  const t = await startApp({ server });
  assert.equal(shownAt(t, 0, 0), '<img src=x onerror=alert(1)>');
  assert.deepEqual(htmlWrites, []);
  assert.equal(t.grid.el.querySelectorAll('img, b, a, script').length, 0);
  assert.ok(textNodes(t.grid.el).some((n) => n.data === '<b>bold</b>'));
  for (const el of t.doc.querySelectorAll('button, div, span, textarea, input')) {
    for (const k of el.attrs.keys()) assert.equal(/^on/.test(k), false, `no inline handler attribute (${k})`);
  }
});
