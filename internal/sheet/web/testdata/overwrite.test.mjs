// An overwrite notice offers the text this tab committed in the cell it
// names (rule 30, TECH 7.2). The text is filed from the action's own
// cells, whatever cells its delta carries: those of rows and columns the
// action added under the ids its delta gives them, and those of an action
// whose delta the connection got for another tab.
'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { fire } from './dom.mjs';
import { FakeServer, find, key, notices, startApp, type } from './server.mjs';

function pasteText(t, text) {
  const data = { 'text/plain': text };
  fire(t.doc, 'paste', { clipboardData: { getData: (k) => data[k], setData() {} }, preventDefault() {} });
}

// overwrite has an agent replace a cell of tab `tab` and sends the notice
// the host sends the session that wrote it at `replaced`. It returns the
// text the notice offers.
function overwrite(t, tab, row, col, replaced) {
  const r = t.server.tab(tab).rows.indexOf(row);
  const c = t.server.tab(tab).cols.indexOf(col);
  t.server.setCell(tab, r, c, 'agent');
  t.server.socket.recv({ type: 'overwritten', tab, row, col, replaced });
  return notices(t).at(-1)[2];
}

for (const [name, at, text, grown] of [
  ['a row', [4, 0], 'first\nmine', ['rows']],
  ['a column', [0, 2], 'first\tmine', ['cols']],
  ['a row and a column', [4, 2], 'first\tsecond\nthird\tmine', ['rows', 'cols']],
]) {
  test(`pasted text in ${name} the paste adds is offered when an agent overwrites it`, async () => {
    const server = new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 5, cols: 3 }] });
    const t = await startApp({ server });
    const tab = server.tab(1);
    const before = { rows: tab.rows.length, cols: tab.cols.length };
    t.sel.set(...at);
    pasteText(t, text);
    const [action] = server.socket.frames('action');
    for (const axis of grown) assert.deepEqual(action.ops[0][axis].at(-1), { new: 1 }, `the paste adds ${axis}`);
    assert.deepEqual([tab.rows.length, tab.cols.length], [before.rows + (grown.includes('rows') ? 1 : 0), before.cols + (grown.includes('cols') ? 1 : 0)]);
    const row = grown.includes('rows') ? tab.rows.at(-1) : tab.rows[at[0]];
    const col = grown.includes('cols') ? tab.cols.at(-1) : tab.cols[at[1]];
    assert.equal(tab.cells.get(`${row}:${col}`).value, 'mine', 'the host wrote the added cell');
    const seq = String(server.seq);
    assert.equal(overwrite(t, 1, row, col, seq), 'mine');
    assert.equal(overwrite(t, 1, tab.rows[at[0]], tab.cols[at[1]], seq), 'first');
  });
}

test('a paste whose delta the connection did not get offers the cells it did not add', async () => {
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 5, cols: 3 }] });
  const t = await startApp({ server });
  server.manual.add('action');
  t.sel.set(4, 0);
  pasteText(t, 'first\nmine');
  const tab = server.tab(1);
  const seq = String(++server.seq);
  server.socket.recv({ type: 'ack', n: '1', seq, results: [{ status: 'applied', applied: 2 }] });
  assert.equal(overwrite(t, 1, tab.rows[4], tab.cols[0], seq), 'first');
});

// otherTab is a sheet whose Other!A1 is =Data!A1, with the page on Data.
async function otherTab() {
  const server = new FakeServer({ tabs: [
    { id: 1, name: 'Data', rows: 5, cols: 3, cells: { '0,0': 'old' } },
    { id: 2, name: 'Other', rows: 5, cols: 3, cells: { '0,0': '=Data!A1' } },
  ] });
  const data = server.tab(1);
  const other = server.tab(2);
  server.dependents.push({ from: { tab: 1, row: data.rows[0], col: data.cols[0] }, to: { tab: 2, row: other.rows[0], col: other.cols[0] } });
  const t = await startApp({ server });
  return { t, server, a1: [data.rows[0], data.cols[0]] };
}

for (const how of ['paste', 'typed edit']) {
  test(`a ${how} resent while the page watches another tab is offered when an agent overwrites it`, async () => {
    const { t, server, a1 } = await otherTab();
    server.socket.drop();
    t.sel.set(0, 0);
    if (how === 'paste') pasteText(t, 'mine');
    else { type(t, 'mine'); key(t, 'Enter'); }
    t.app.openTab(2);
    find(t.app.banners, 'Retry now').click();
    server.socket.open();
    assert.equal(t.model.tab.id, 2, 'the page watches Other');
    const sent = server.socket.sent.filter((m) => m.type === 'watch' || m.type === 'action').map((m) => m.type);
    assert.deepEqual(sent, ['watch', 'action'], 'the watch goes out before the resent action');
    const seq = String(server.seq);
    const delta = server.log.at(-1);
    assert.equal(delta.seq, seq);
    assert.deepEqual(delta.recalc.get(2)[0][1][0].slice(0, 3), [server.tab(2).cols[0], 'tf', 'mine'], 'the delta carries the recalculated Other!A1');
    assert.equal(server.tab(1).cells.get(`${a1[0]}:${a1[1]}`).value, 'mine');
    assert.equal(t.app.queued.size, 0, 'the action is filed');
    assert.equal(overwrite(t, 1, ...a1, seq), 'mine');
  });
}

test('a recalculated cell in the delta files no text, and the written one keeps its own', async () => {
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 5, cols: 3, cells: { '0,1': '=A1' } }] });
  const tab = server.tab(1);
  server.dependents.push({ from: { tab: 1, row: tab.rows[0], col: tab.cols[0] }, to: { tab: 1, row: tab.rows[0], col: tab.cols[1] } });
  const t = await startApp({ server });
  t.sel.set(0, 0);
  type(t, 'mine');
  key(t, 'Enter');
  const seq = String(server.seq);
  assert.deepEqual(server.log.at(-1).cells.flatMap(([, cells]) => cells.map((c) => c[0])), [tab.cols[0], tab.cols[1]], 'the delta carries A1 and B1');
  assert.deepEqual([...t.app.commits.values()], ['mine']);
  assert.equal(overwrite(t, 1, tab.rows[0], tab.cols[0], seq), 'mine');
});

test('a paste into an added row whose delta the page never got is offered from the ids its ack names', async () => {
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 5, cols: 3 }] });
  const t = await startApp({ server });
  const tab = server.tab(1);
  // The host commits the paste, but its delta waits behind another frame
  // and its ack is held, and the connection ends before either is sent.
  server.holdAcks = true;
  server.socket.queue.push({ type: 'presence', people: [] });
  t.sel.set(4, 0);
  pasteText(t, 'first\nmine');
  const seq = String(server.seq);
  const row = tab.rows.at(-1);
  assert.equal(tab.cells.get(`${row}:${tab.cols[0]}`).value, 'mine');
  assert.equal(t.app.queued.size, 1, 'neither the delta nor the ack arrived');
  server.socket.drop();
  server.holdAcks = false;
  server.resume = 'snapshot';
  find(t.app.banners, 'Retry now').click();
  server.socket.open();
  const ack = [...server.sessions.values()][0].receipts.get('1');
  assert.deepEqual(ack.added, { rows: row }, 'the stored ack names the added row');
  assert.equal(t.net.pending.length, 0, 'the resent paste was answered from its receipt');
  assert.equal(server.applied.length, 1, 'and applied once');
  assert.equal(overwrite(t, 1, row, tab.cols[0], seq), 'mine');
});

test('an ack whose added ids do not fit is no answer', async () => {
  const { ackError } = await import('../protocol.js');
  const ok = { n: '1', seq: '2', results: [{ status: 'applied' }] };
  assert.equal(ackError({ ...ok, added: { rows: 7 } }, 1), null);
  assert.equal(ackError({ ...ok, added: { rows: 7, cols: 9 } }, 1), null);
  for (const added of [{}, { rows: 0 }, { cols: -1 }, { rows: 1.5 }, { rows: '7' }, { rows: 2 ** 53 }, [], null, 7]) {
    assert.match(ackError({ ...ok, added }, 1) ?? '', /added ids|null/, JSON.stringify(added));
  }
});

// skipped commits the actions that act() makes with their deltas and acks
// held, and then has an agent replace cell (r, c) of Data. The connection
// is resynced past all of it, as when the agent's output pushed the log
// past the connection, and the overwrite notice of `replaced` arrives
// before the acks. It returns the acks the host stored, by n.
function skipped(t, act, r, c, replaced) {
  const { server } = t;
  server.holdAcks = true;
  server.socket.queue.push({ type: 'presence', people: [] });
  act();
  const seq = replaced();
  server.setCell(1, r, c, 'agent');
  server.socket.queue.length = 0;
  const generation = t.net.generation + 1;
  server.socket.recv({ type: 'resync', generation, seq: String(server.seq), date: server.date, tabs: server.tabInfo() });
  server.snapshot(server.socket, generation, 1);
  const tab = server.tab(1);
  server.socket.recv({ type: 'overwritten', tab: 1, row: tab.rows[r], col: tab.cols[c], replaced: seq });
  server.holdAcks = false;
  return [...server.sessions.values()][0].receipts;
}

// overwriteNotices are the overwrite notices on screen, as [text, kept].
const overwriteNotices = (t) => notices(t).filter(([, text]) => text.startsWith('Someone changed')).map(([, text, kept]) => [text, kept]);

test('an overwrite notice that arrives before the ack of its commit offers the text once the ack files it', async () => {
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 5, cols: 3 }] });
  const t = await startApp({ server });
  let seq;
  const acks = skipped(t, () => { t.sel.set(0, 0); type(t, 'mine'); key(t, 'Enter'); }, 0, 0, () => (seq = String(server.seq)));
  assert.equal(t.model.seq, String(server.seq), 'the resync skipped the edit\'s delta');
  assert.deepEqual(overwriteNotices(t), [['Someone changed A1 after you typed in it. The value the host received later stays.', null]], 'nothing is filed yet');
  assert.equal(acks.get('1').seq, seq);
  server.socket.recv(acks.get('1'));
  assert.deepEqual(overwriteNotices(t), [['Someone changed A1 after you typed in it. The value the host received later stays. Your text:', 'mine']], 'the notice offers the text the ack filed');
  assert.equal(t.app.unfiled.size, 0);
});

test('an overwrite notice of an added row that arrives before the paste\'s ack offers the pasted text', async () => {
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 5, cols: 3 }] });
  const t = await startApp({ server });
  const tab = server.tab(1);
  const pasted = () => { t.sel.set(4, 0); pasteText(t, 'first\nmine'); };
  const acks = skipped(t, pasted, 5, 0, () => String(server.seq));
  assert.equal(tab.rows.length, 6, 'the paste added a row');
  assert.deepEqual(acks.get('1').added, { rows: tab.rows[5] });
  assert.equal(overwriteNotices(t)[0][1], null);
  server.socket.recv(acks.get('1'));
  assert.deepEqual(overwriteNotices(t).map(([, kept]) => kept), ['mine']);
});

test('an overwrite notice that arrives before a refusal of its action offers nothing', async () => {
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 5, cols: 3 }] });
  const t = await startApp({ server });
  server.refuse = { status: 'refused', reason: 'quota' };
  // The notice names the sequence the refused action would have taken.
  const acks = skipped(t, () => { t.sel.set(0, 0); type(t, 'mine'); key(t, 'Enter'); }, 0, 0, () => String(server.seq + 1));
  assert.equal(acks.get('1').results[0].status, 'refused');
  server.socket.recv(acks.get('1'));
  assert.deepEqual(overwriteNotices(t).map(([, kept]) => kept), [null], 'a refused text is no commit');
  assert.equal(notices(t).at(-1)[2], 'mine', 'the refusal offers it instead');
  assert.equal(t.app.unfiled.size, 0, 'nothing waits once no action is queued');
});

test('an overwrite notice that arrives before its ack offers that commit, not a later edit of the cell', async () => {
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 5, cols: 3 }] });
  const t = await startApp({ server });
  let first;
  const both = () => {
    t.sel.set(0, 0);
    type(t, 'mine');
    key(t, 'Enter');
    first = String(server.seq);
    t.sel.set(0, 0);
    type(t, 'later');
    key(t, 'Enter');
  };
  const acks = skipped(t, both, 0, 0, () => first);
  assert.notEqual(acks.get('2').seq, first);
  server.socket.recv(acks.get('2'));
  assert.deepEqual(overwriteNotices(t).map(([, kept]) => kept), [null], 'the later edit is no text of the commit named');
  server.socket.recv(acks.get('1'));
  assert.deepEqual(overwriteNotices(t).map(([, kept]) => kept), ['mine']);
});
