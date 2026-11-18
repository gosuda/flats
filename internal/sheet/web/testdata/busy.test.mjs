// A host that cannot take an action now (it is calculating, it stopped the
// sheet after idle time, or it could not write scratch storage for an
// undo) ends the connection with fatal "unavailable". Nothing was committed
// and the action's number was not used. The client sends the action again
// with the same number a bounded number of times, and then treats it as
// refused. A limit refuses every operation of an action, which is one
// refusal to the person.
'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { ops } from '../protocol.js';
import { fire } from './dom.mjs';
import { FakeServer, find, key, notices, shownAt, startApp, type } from './server.mjs';

const sheet = () => new FakeServer({ tabs: [{ id: 1, name: 'Budget', rows: 30, cols: 6, cells: { '0,0': 'Item', '1,0': 'Venue', '1,1': '1200', '2,0': 'Food', '2,1': '900' } }] });
const status = (t) => t.app.status.el.lastChild.textContent;
const actions = (t) => t.server.sockets.flatMap((s) => s.frames('action'));
const clip = (text = '') => { const data = { 'text/plain': text }; return { clipboardData: { setData: (k, v) => { data[k] = v; }, getData: (k) => data[k] }, data }; };
// again waits out the pause before the next connection and opens it.
const again = (t, ms) => { t.timers.advance(ms); t.server.socket.open(); };

test('an action the host cannot take now is sent again with its number and applies once', async () => {
  const t = await startApp({ server: sheet() });
  t.server.unavailable = 2;
  key(t, 'ArrowDown');
  type(t, 'Hall');
  key(t, 'Enter');
  assert.equal(shownAt(t, 1, 0), 'Hall', 'the typed text stays on screen');
  assert.notEqual(status(t), 'Saved');
  assert.match(t.app.banners.textContent, /The host is busy\. Your 1 unsaved edit stays on this screen and is sent again in a moment\. Each applies once\./);
  t.timers.advance(999);
  assert.equal(actions(t).length, 1, 'the next try waits a second');
  again(t, 1);
  assert.notEqual(status(t), 'Saved', 'Saved stays off while the action is tried again');
  again(t, 2000);
  assert.deepEqual(actions(t).map((a) => [a.n, a.base, a.ops[0].text]), [['1', '10', 'Hall'], ['1', '10', 'Hall'], ['1', '10', 'Hall']], 'the same action each time');
  assert.equal(t.server.cell(1, 1, 0).value, 'Hall');
  assert.equal(t.server.applied.length, 1);
  assert.equal(status(t), 'Saved');
  assert.deepEqual(notices(t), []);
  assert.equal(t.app.banners.children.length, 0);
  // the next action goes out at once again
  type(t, 'Bus');
  key(t, 'Enter');
  assert.equal(actions(t).at(-1).n, '2');
  assert.equal(t.server.cell(1, 2, 0).value, 'Bus');
});

test('an action the host keeps answering "unavailable" is never given up or called refused', async () => {
  const t = await startApp({ server: sheet() });
  t.server.unavailable = 9;
  key(t, 'ArrowDown');
  type(t, 'Hall');
  key(t, 'Enter');
  // the quick tries, then slower and slower ones
  const waits = [1000, 2000, 4000, 8000, 16000, 30000, 30000, 30000];
  for (const [i, wait] of waits.entries()) {
    assert.equal(shownAt(t, 1, 0), 'Hall', 'the typed text stays on screen');
    assert.equal(t.net.pending.length, 1, 'the action stays in the queue');
    assert.notEqual(status(t), 'Saved');
    assert.deepEqual(notices(t), [], 'no outcome is made up');
    t.timers.advance(wait - 1);
    assert.equal(t.server.busyActions.length, i + 1, `the next try waits ${wait} ms`);
    again(t, 1);
    if (i >= 3) {
      assert.match(t.app.banners.textContent, /The host has been busy for a while, so your 1 unsaved edit is not saved yet\. It stays on this screen, and this page keeps asking the host until it answers\. Each applies once\./);
      assert.equal(t.app.banners.children[0].getAttribute('aria-label'), 'Host busy');
    }
  }
  assert.deepEqual(t.server.busyActions, Array(9).fill('1'), 'the same number every time');
  assert.equal(t.server.applied.length, 0);
  // more can be typed meanwhile; it waits its turn behind the unanswered action
  type(t, 'Bus');
  key(t, 'Enter');
  assert.equal(shownAt(t, 2, 0), 'Bus');
  assert.deepEqual(t.net.pending.map((a) => a.n), ['1', '2']);
  // the host answers at last: both apply once, in order, and the page is Saved
  again(t, 30000);
  assert.deepEqual(actions(t).slice(-2).map((a) => [a.n, a.ops[0].text]), [['1', 'Hall'], ['2', 'Bus']]);
  assert.deepEqual(t.server.applied.map(([, n]) => n), ['1', '2']);
  assert.deepEqual([t.server.cell(1, 1, 0).value, t.server.cell(1, 2, 0).value], ['Hall', 'Bus']);
  assert.equal(status(t), 'Saved');
  assert.equal(t.app.banners.children.length, 0);
  assert.deepEqual(notices(t), []);
  // and the pace is the usual one again
  t.server.unavailable = 1;
  type(t, 'Later');
  key(t, 'Enter');
  again(t, 1000);
  assert.equal(t.server.cell(1, 3, 0).value, 'Later');
});

test('an action that was committed with its ack lost is not doubled, whatever the host answers to asking again', async () => {
  const t = await startApp({ server: sheet() });
  // The insert of a row is committed, but its ack never arrives: the
  // connection is lost first. Asking again meets a busy host six times.
  t.sel.set(2, 0);
  t.app.changed();
  t.server.holdAcks = true;
  t.app.insertRows('before');
  t.server.socket.drop();
  t.server.holdAcks = false;
  assert.equal(t.server.tab(1).rows.length, 31, 'the host has the row');
  assert.equal(t.server.applied.length, 1);
  assert.equal(t.net.pending.length, 1, 'the client does not know');
  t.server.unavailable = 6;
  for (const wait of [1000, 2000, 4000, 8000, 16000, 30000]) {
    again(t, wait);
    assert.equal(t.net.pending.length, 1);
    assert.notEqual(status(t), 'Saved');
    assert.equal(notices(t).some(([kind]) => kind === 'bad'), false, 'nothing says the insert was refused');
  }
  assert.equal(t.model.rowCount, 31, 'the replayed stream brought the row the host committed');
  // the next try reaches the stored receipt
  again(t, 30000);
  assert.equal(t.net.pending.length, 0);
  assert.equal(status(t), 'Saved');
  assert.equal(t.server.tab(1).rows.length, 31, 'one row, not two');
  assert.equal(t.server.applied.length, 1, 'the action applied once');
  assert.deepEqual(notices(t), []);
});

test('a session the host ends while an action is unanswered stops editing and offers the text for copying', async () => {
  const t = await startApp({ server: sheet() });
  t.server.unavailable = 2;
  key(t, 'ArrowDown');
  type(t, 'Hall');
  key(t, 'Enter');
  again(t, 1000);
  // the host forgets the session: the outcome can no longer be learned
  t.server.sessions.clear();
  again(t, 2000);
  assert.equal(status(t), 'Editing stopped · 1 unsaved');
  assert.match(t.app.banners.textContent, /Editing stopped\. The host no longer knows this browser tab’s session.*Your 1 unsaved edit was not confirmed by the host and may not be saved\. Copy it before you reload:/);
  assert.equal(t.net.pending[0].ops[0].text, 'Hall');
  assert.equal(notices(t).some(([, text]) => /refused/.test(text)), false);
});

test('an action answered before whose outcome the host no longer keeps stops editing without calling it applied or refused', async () => {
  const t = await startApp({ server: sheet() });
  // The action was committed long ago and its ack lost; the host has since
  // dropped the receipt.
  t.server.holdAcks = true;
  key(t, 'ArrowDown');
  type(t, 'Hall');
  key(t, 'Enter');
  t.server.socket.drop();
  t.server.manual.add('action');
  t.timers.advance(1000);
  t.server.socket.open();
  t.server.socket.recv({ type: 'fatal', code: 'receipt_expired' });
  assert.equal(status(t), 'Editing stopped · 1 unsaved');
  assert.match(t.app.banners.textContent, /Editing stopped\. The host no longer keeps the outcome of an unsaved edit, so it cannot tell whether it was applied\. Your 1 unsaved edit was not confirmed by the host and may not be saved\. Copy it before you reload:/);
  assert.ok(find(t.app.banners, 'Copy edits'), 'the text is offered for copying');
  assert.ok(find(t.app.banners, 'Reload'));
  assert.equal(t.net.pending[0].ops[0].text, 'Hall', 'the action is not dropped');
  assert.equal(notices(t).some(([, text]) => /refused|Nothing was changed/.test(text)), false);
  // nothing more is sent, and no edit starts
  t.timers.advance(60000);
  assert.equal(t.server.sockets.at(-1).closed, true);
  type(t, 'more');
  assert.equal(t.editor.open, false);
});

test('an action the host calls malformed stops editing; its text is offered, not called saved', async () => {
  const t = await startApp({ server: sheet() });
  t.server.manual.add('action');
  key(t, 'ArrowDown');
  type(t, 'Hall');
  key(t, 'Enter');
  t.server.socket.recv({ type: 'fatal', code: 'protocol' });
  assert.equal(status(t), 'Editing stopped · 1 unsaved');
  assert.match(t.app.banners.textContent, /The host could not read what this page sent\..*was not confirmed by the host and may not be saved/);
  assert.equal(t.net.pending.length, 1);
});

test('an undo the host cannot take now is sent again until it is answered', async () => {
  const t = await startApp({ server: sheet() });
  t.server.unavailable = 5;
  key(t, 'z', { ctrlKey: true });
  for (const wait of [1000, 2000, 4000, 8000, 16000]) again(t, wait);
  assert.deepEqual(actions(t).map((a) => [a.n, a.ops[0].op]), Array(6).fill(['1', 'undo']));
  assert.equal(t.server.applied.length, 1);
  assert.equal(status(t), 'Saved');
  assert.deepEqual(notices(t), []);
});

test('a fatal frame that overtakes the ack of a committed action: both actions end applied once', async () => {
  const t = await startApp({ server: sheet() });
  // Two actions are in flight. The host commits the first, and its ack
  // waits in the send queue. The second cannot be taken: its fatal frame
  // is sent ahead of the queue and ends the connection, so the ack of the
  // first never arrives.
  t.server.holdAcks = true;
  key(t, 'ArrowDown');
  type(t, 'One');
  key(t, 'Enter');
  assert.equal(t.server.cell(1, 1, 0).value, 'One', 'the first action is committed');
  t.server.unavailable = 1;
  type(t, 'Two');
  key(t, 'Enter');
  t.server.holdAcks = false;
  assert.equal(t.server.socket.closed, true);
  assert.deepEqual(t.net.pending.map((a) => a.n), ['1', '2'], 'both are unanswered for the client');
  assert.deepEqual(t.net.pending.map((a) => a.refusals || 0), [0, 0], 'the refusal names neither of them');
  // Asking again, one at a time: the busy host answers the first with
  // "unavailable" once more, although it was committed, then from its
  // receipt. The second goes out only after that.
  t.server.unavailable = 1;
  again(t, 1000);
  assert.deepEqual(t.server.socket.frames('action').map((a) => a.n), ['1'], 'one action at a time');
  assert.deepEqual(t.net.pending.map((a) => a.n), ['1', '2']);
  assert.deepEqual(notices(t), [], 'the first action is not called refused');
  again(t, 2000);
  assert.deepEqual(t.server.socket.frames('action').map((a) => a.n), ['1', '2'], 'the second goes out when the first was answered');
  assert.deepEqual([t.server.cell(1, 1, 0).value, t.server.cell(1, 2, 0).value], ['One', 'Two']);
  assert.deepEqual(t.server.applied.map(([, n]) => n), ['1', '2'], 'each applied once');
  assert.equal(status(t), 'Saved');
  assert.deepEqual(notices(t), []);
});

test('a host that stopped the sheet after idle time takes the action on the next connection', async () => {
  const t = await startApp({ server: sheet() });
  // The instance was stopped: the action in flight ends the connection, and
  // the host opens the sheet again when the client connects.
  t.server.unavailable = 1;
  key(t, 'ArrowDown');
  key(t, 'Delete');
  again(t, 1000);
  assert.equal(t.server.cell(1, 1, 0), null);
  assert.equal(status(t), 'Saved');
  assert.deepEqual(actions(t).map((a) => a.n), ['1', '1']);
});

test('a limit refuses every operation of an action: one notice, and the screen returns to the host\'s state', async () => {
  // An action of several operations gets one result for each. No gesture
  // of this page sends one, so the test queues it.
  let t = await startApp({ server: sheet() });
  const m = t.model;
  t.server.refuse = { status: 'refused', reason: 'cells' };
  t.net.queue([ops.set(1, m.rowAt(3), m.colAt(0), 'one', [3, 0]), ops.set(1, m.rowAt(4), m.colAt(0), 'two', [4, 0])], m.seq, { tab: 1, label: 'paste', texts: ['one', 'two'] });
  t.net.flush();
  assert.equal(actions(t).at(-1).ops.length, 2);
  assert.deepEqual(notices(t), [['bad', 'The host refused this paste: a sheet holds at most 500,000 cells with content or a format. Nothing was changed. Your text:', 'one\ntwo']], 'one refusal, said once');
  assert.equal(status(t), 'Saved');

  // a paste inside the sheet over a range is one copy
  t = await startApp({ server: sheet() });
  key(t, 'ArrowDown');
  const e = clip();
  fire(t.doc, 'copy', e);
  t.sel.range(3, 0, 6, 1);
  t.app.changed();
  t.server.refuse = { status: 'refused', reason: 'cells' };
  fire(t.doc, 'paste', clip(e.data['text/plain']));
  assert.deepEqual(actions(t).at(-1).ops.map((op) => op.op), ['copy']);
  assert.deepEqual(notices(t), [['bad', 'The host refused this paste: a sheet holds at most 500,000 cells with content or a format. Nothing was changed. Your text:', 'Venue']]);
  assert.equal(shownAt(t, 3, 0), '');
  assert.equal(status(t), 'Saved');

  // typed strings pasted over several cells are one operation with pending text in every cell
  t = await startApp({ server: sheet() });
  t.sel.set(4, 0);
  t.app.changed();
  t.server.refuse = { status: 'refused', reason: 'sheet_text' };
  fire(t.doc, 'paste', clip('a\tb\nc\td'));
  assert.equal(notices(t).length, 1);
  assert.match(notices(t)[0][1], /The host refused this paste: the cells of a sheet hold at most 50,000,000 characters of typed text\. Nothing was changed/);
  assert.equal(notices(t)[0][2], 'a\tb\nc\td');
  assert.deepEqual([shownAt(t, 4, 0), shownAt(t, 4, 1), shownAt(t, 5, 0), shownAt(t, 5, 1)], ['', '', '', ''], 'no pasted text stays on screen');
  assert.equal(t.model.pending.size, 0);

  // a fill and a deletion of several runs of rows are one operation each
  t = await startApp({ server: sheet() });
  t.sel.range(1, 0, 4, 0);
  t.app.changed();
  t.server.refuse = { status: 'refused', reason: 'formulas' };
  t.app.fill('down');
  assert.equal(notices(t).length, 1);
  assert.match(notices(t)[0][1], /The host refused this fill down: a sheet holds at most 100,000 formulas\. Nothing was changed\./);
  assert.equal(shownAt(t, 2, 0), 'Food');
  t.server.refuse = { status: 'refused', reason: 'disk_quota' };
  t.app.deleteRows();
  assert.equal(notices(t).length, 2);
  assert.equal(t.model.rowCount, 30);
  assert.equal(shownAt(t, 1, 0), 'Venue');
});

test('an action with one refused operation among applied ones says which part was not applied', async () => {
  // No gesture of this page sends an action of several operations, so the
  // test queues one.
  const t = await startApp({ server: sheet() });
  const m = t.model;
  t.server.manual.add('action');
  const a = t.net.queue([ops.set(1, m.rowAt(3), m.colAt(0), 'one', [3, 0]), ops.set(1, m.rowAt(4), m.colAt(0), '=A1', [4, 0])], m.seq, { tab: 1, label: 'paste' });
  t.net.flush();
  t.server.socket.recv({ type: 'ack', n: a.n, seq: '10', results: [{ status: 'applied', applied: 1 }, { status: 'refused', reason: 'stale_formula' }] });
  assert.deepEqual(notices(t).map((n) => n[1]), ['The host refused a part of this paste: the grid changed since the formula was typed and the host can no longer adjust its references. That part was not applied.']);
});

test('an undo whose saved step the host lost says so, and undo goes on to the steps before it', async () => {
  const t = await startApp({ server: sheet() });
  t.server.undoResult = { applied: 0, skipped: 1, reason: 'lost' };
  key(t, 'z', { ctrlKey: true });
  assert.deepEqual(notices(t), [['warn', 'This step could not be undone: the host no longer has what it had saved for it. Earlier steps are still available.', null]]);
  assert.equal(status(t), 'Saved');
  // the next undo is sent as usual and reaches the earlier step
  t.server.undoResult = { applied: 3, skipped: 0 };
  key(t, 'z', { ctrlKey: true });
  assert.deepEqual(actions(t).map((a) => [a.n, a.ops[0].op]), [['1', 'undo'], ['2', 'undo']]);
  assert.equal(t.server.applied.length, 2);
  t.server.undoResult = { applied: 0, skipped: 1, reason: 'lost' };
  key(t, 'z', { ctrlKey: true, shiftKey: true });
  assert.equal(notices(t).at(-1)[1], 'This step could not be redone: the host no longer has what it had saved for it. Earlier steps are still available.');
  assert.equal(notices(t).length, 1, 'the notice of the earlier undo was replaced');
});
