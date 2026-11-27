// The grid on screen and the stream can part: a reconnect the server
// cannot replay, a resync, a sequence gap, or a structural change that
// meets a snapshot still arriving. Until the new snapshot starts, the
// model's sequence is not the state the person sees, so nothing typed
// against that grid is sent with it as its base (rule 14, TECH 8.4).
'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { FakeServer, key, notices, shownAt, startApp, type } from './server.mjs';

const sheet = () => new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 30, cols: 6, cells: { '4,1': '5' } }, { id: 2, name: 'Other', rows: 5, cols: 3 }] });
const actions = (t) => t.server.sockets.flatMap((s) => s.frames('action'));
const welcome = (server, seq) => ({ type: 'welcome', access: 'private', session: 'tok1', session_id: 's1', me: server.me, seq, date: server.date, tabs: server.tabInfo() });

test('a formula in the editor is not committed when the connection starts over', async () => {
  const t = await startApp({ server: sheet() });
  type(t, '=B5+1');
  // A delta was lost: the client connects again without a watermark.
  const before = t.server.socket;
  before.recv({ type: 'delta', seq: '15' });
  assert.notEqual(t.server.socket, before);
  t.server.seq = 15;
  t.server.manual.add('watch');
  t.server.socket.open();
  assert.equal(t.editor.open, false, 'the editor closed');
  assert.equal(actions(t).length, 0, 'and its text was not sent against a grid the new sequence does not describe');
  assert.deepEqual(notices(t).map((n) => n[2]), ['=B5+1'], 'the text is offered back');
  // nothing can be typed until the new snapshot starts
  type(t, '=B5');
  assert.equal(t.editor.open, false);
  assert.match(notices(t).at(-1)[1], /The sheet is loading again/);
  assert.equal(t.app.root.classList.contains('ro'), true);
  t.server.snapshot(t.server.socket, 1, 1);
  type(t, '=B5');
  key(t, 'Enter');
  assert.deepEqual([actions(t)[0].base, actions(t)[0].ops[0].text], ['15', '=B5']);
});

test('between a welcome that could not replay and its snapshot nothing is typed against the old grid', async () => {
  const t = await startApp({ server: sheet() });
  t.server.socket.drop();
  t.server.seq = 20; // the log no longer reaches back
  t.server.manual.add('hello');
  t.timers.advance(1000);
  const ws = t.server.socket;
  assert.equal(ws.open().watermark, '10');
  // The server names its current sequence, sends a delta committed before
  // the snapshot was taken, then the snapshot, unasked.
  ws.session = t.server.sessions.get('tok1');
  ws.recv(welcome(t.server, '20'));
  ws.recv({ type: 'delta', seq: '21', touched: [2] });
  assert.deepEqual([t.model.seq, t.net.lastSeq, t.model.stale], ['10', '21', true], 'the stream is at 21; the grid stays the one of sequence 10');
  assert.equal(ws.frames('watch').length, 0, 'the snapshot comes unasked');
  type(t, '=B5');
  assert.equal(t.editor.open, false, 'the grid on screen is the one of sequence 10, not 21');
  assert.equal(actions(t).length, 0);
  t.server.seq = 21;
  t.server.snapshot(ws, 0, 1);
  assert.equal(shownAt(t, 4, 1), '5');
  type(t, '=B5');
  assert.equal(t.editor.open, true);
  key(t, 'Enter');
  assert.equal(actions(t)[0].base, '21');
});

test('a reconnect that is replayed keeps the editor and its base', async () => {
  const t = await startApp({ server: sheet() });
  type(t, '=B5+1');
  t.server.socket.drop();
  t.timers.advance(1000);
  t.server.socket.open();
  assert.equal(t.editor.open, true);
  assert.equal(t.model.stale, false);
  key(t, 'Enter');
  assert.deepEqual([actions(t)[0].base, actions(t)[0].ops[0].text], ['10', '=B5+1']);
});

test('after a resync nothing is typed until the snapshot starts', async () => {
  const t = await startApp({ server: sheet() });
  t.server.seq = 14;
  t.server.socket.recv({ type: 'resync', generation: 5, seq: '14', date: t.server.date, tabs: t.server.tabInfo() });
  t.server.socket.recv({ type: 'delta', seq: '15', touched: [2] });
  type(t, '=B5');
  assert.equal(t.editor.open, false);
  t.server.seq = 15;
  t.server.snapshot(t.server.socket, 5, 1);
  type(t, '=B5');
  assert.equal(t.editor.open, true);
});

test('a structural change that meets a snapshot still arriving stops editing until the tab is loaded again', async () => {
  const server = sheet();
  const t = await startApp({ server });
  server.manual.add('watch');
  t.app.openTab(2);
  const tab = server.tab(2);
  const head = (generation, seq) => ({ type: 'tab_head', generation, tab: 2, seq, date: server.date, tabs: server.tabInfo(), rows: [[tab.rows[0], tab.rows.length]], cols: [[tab.cols[0], tab.cols.length]], widths: {}, frozen_rows: 0, frozen_cols: 0, styles: [{}] });
  const rows = (generation) => ({ type: 'rows', generation, tab: 2, part: 0, rows: [] });
  server.socket.recv(head(2, '10'));
  type(t, 'plain');
  assert.equal(t.editor.open, false, 'a tab is not interactive from its head alone');
  assert.match(notices(t).at(-1)[1], /The tab is still loading/);
  server.socket.recv(rows(2));
  type(t, 'plain');
  assert.equal(t.editor.open, true, 'a tab is interactive from its first rows on');
  // a row is inserted before the snapshot is complete
  const watches = server.socket.frames('watch').length;
  server.insertRows(2, 0);
  assert.equal(server.socket.frames('watch').length, watches + 1, 'the tab is watched again');
  assert.equal(t.editor.open, false);
  assert.equal(notices(t).at(-1)[2], 'plain');
  type(t, '=A1');
  assert.equal(t.editor.open, false, 'the rows on screen are those before the insert');
  assert.equal(actions(t).length, 0);
  server.socket.recv(head(3, '11'));
  server.socket.recv(rows(3));
  type(t, '=A1');
  assert.equal(t.editor.open, true);
});

test('a resync of a generation above the client\'s own is taken, and the next watch goes above it', async () => {
  const t = await startApp({ server: sheet() });
  const own = t.net.generation;
  t.server.setCell(1, 0, 0, 'newer');
  t.server.socket.recv({ type: 'resync', generation: own + 5, seq: String(t.server.seq), date: t.server.date, tabs: t.server.tabInfo() });
  assert.equal(t.app.canWrite(), false);
  t.server.snapshot(t.server.socket, own + 5, 1);
  assert.equal(t.app.canWrite(), true);
  assert.equal(shownAt(t, 0, 0), 'newer');
  t.app.openTab(2);
  assert.equal(t.server.socket.frames('watch').at(-1).generation, own + 6);
  assert.equal(t.model.tab.id, 2);
});

test('a resync that meets a pending watch brings the tab the person asked for', async () => {
  const t = await startApp({ server: sheet() });
  // The watch of the other tab waits on the host; a resync arrives first,
  // and its snapshot is of the wanted tab, under the resync's generation.
  t.server.manual.add('watch');
  t.app.openTab(2);
  const generation = t.net.generation + 1;
  t.server.socket.recv({ type: 'resync', generation, seq: String(t.server.seq), date: t.server.date, tabs: t.server.tabInfo() });
  assert.equal(t.model.tab.id, 1);
  assert.equal(t.app.canWrite(), false);
  t.server.manual.delete('watch');
  t.server.snapshot(t.server.socket, generation, 2);
  assert.equal(t.model.tab.id, 2, 'the wanted tab is on screen');
  assert.equal(t.app.switching, false);
  assert.equal(t.app.canWrite(), true);
  type(t, 'in Other');
  key(t, 'Enter');
  assert.equal(actions(t)[0].ops[0].tab, 2);
});

// resync is the frame of a connection that skipped deltas: it names the
// tab list and the date of the sequence it goes on from.
const resync = (server, generation) => server.socket.recv({ type: 'resync', generation, seq: String(server.seq), date: server.date, tabs: server.tabInfo() });

for (const access of ['private', 'public']) {
  test(`${access}: a resync that skipped the deletion of the tab on screen brings the tab list, its order and names, the date and the first tab`, async () => {
    const server = new FakeServer({ access, tabs: [{ id: 1, name: 'Data', rows: 30, cols: 6, cells: { '4,1': '5' } }, { id: 2, name: 'Other', rows: 5, cols: 3 }, { id: 3, name: 'Third', rows: 5, cols: 3, cells: { '0,0': 'third' } }] });
    const t = await startApp({ server });
    assert.equal(t.model.tab.id, 1);
    // What the deltas the connection skipped did: Data deleted, Third
    // moved to the front, Other renamed, and the next day.
    server.tabs = [server.tab(3), server.tab(2)];
    server.tab(2).name = 'Renamed';
    server.date = '2026-11-14';
    server.seq += 4;
    const generation = t.net.generation + 1;
    resync(server, generation);
    assert.deepEqual(t.model.tabs, server.tabInfo());
    assert.equal(t.model.date, '2026-11-14');
    assert.equal(t.model.tab, null, 'the deleted tab is not on screen');
    assert.equal(notices(t).at(-1)[1], 'The tab “Data” was deleted. Showing another tab.');
    assert.equal(t.app.canWrite(), false);
    // The host's snapshot after the resync is of the first tab.
    server.snapshot(server.socket, generation, 1);
    assert.deepEqual([t.model.tab.id, t.model.tab.name, t.model.stale, t.model.tab.loading], [3, 'Third', false, false]);
    assert.equal(shownAt(t, 0, 0), 'third');
    assert.equal(t.app.canWrite(), access === 'private');
    // Nothing more is asked for.
    t.timers.advance(30000);
    assert.equal(server.socket.frames('watch').length, 1);
    assert.equal(t.server.sockets.length, 1);
  });
}

test('a resync that skipped the deletion of the tab the person asked for goes back to the tab on screen', async () => {
  const t = await startApp({ server: sheet() });
  t.server.manual.add('watch');
  t.app.openTab(2);
  t.server.tabs = [t.server.tab(1)];
  t.server.seq += 1;
  const generation = t.net.generation + 1;
  resync(t.server, generation);
  assert.deepEqual(t.model.tabs, t.server.tabInfo());
  const watch = t.server.socket.frames('watch').at(-1);
  assert.equal(watch.tab, 1);
  assert.ok(watch.generation > generation, 'the watch goes above the resync');
  // The host answers the watch it held with the first tab, under the
  // resync's generation, and then the new watch.
  t.server.manual.delete('watch');
  t.server.snapshot(t.server.socket, generation, 2);
  t.server.snapshot(t.server.socket, watch.generation, 1);
  assert.deepEqual([t.model.tab.id, t.model.stale, t.app.switching], [1, false, false]);
  assert.equal(shownAt(t, 4, 1), '5');
  assert.equal(t.app.canWrite(), true);
});

test('a resync that skipped nothing brings the names the formula editor held back', async () => {
  const t = await startApp({ server: sheet() });
  t.server.manual.add('rebase');
  type(t, '=B5+');
  // Another writer swaps the names of the two tabs. The editor holds the
  // renames, so the tab bar keeps the names of the grid.
  t.server.renameTab(1, 'Temp');
  t.server.renameTab(2, 'Data');
  t.server.renameTab(1, 'Other');
  const names = () => t.app.tabs.list.children.map((el) => el.textContent);
  assert.equal(t.app.rebaser.holding, true);
  assert.deepEqual(names(), ['Data', 'Other']);
  // The host rebuilt its format list: the connection skipped nothing, and
  // the resync carries the tab list and the date of its sequence.
  t.server.date = '2026-11-14';
  t.server.socket.recv({ type: 'resync', generation: t.net.generation + 1, seq: String(t.server.seq), date: t.server.date, tabs: t.server.tabInfo() });
  assert.equal(t.app.rebaser.holding, false, 'the held renames are dropped');
  assert.equal(notices(t).at(-1)[2], '=B5+');
  t.server.snapshot(t.server.socket, t.net.generation, 1);
  assert.deepEqual([t.model.stale, t.model.seq, t.model.date], [false, String(t.server.seq), '2026-11-14']);
  assert.deepEqual(names(), ['Other', 'Data'], 'the tab bar names the tabs as the snapshot has them');
  const base = t.model.seq;
  t.sel.set(0, 2);
  type(t, '=Data!B5');
  key(t, 'Enter');
  assert.deepEqual([actions(t)[0].base, actions(t)[0].ops[0].text], [base, '=Data!B5']);
});

test('a resync the host sends on its own closes an open editor and offers the text back, with no blame', async () => {
  const t = await startApp({ server: sheet() });
  // The host rebuilt its format list: every connection gets one resync at
  // its next delta, and the new snapshot numbers the formats anew.
  t.server.styles.push({ bold: true });
  t.server.socket.recv({ type: 'delta', seq: String(++t.server.seq), styles: [{ bold: true }], cells: [[t.model.rowAt(4), [[t.model.colAt(1), 'n', 5, null, null, 1]]]] });
  assert.equal(t.model.format(t.model.cellAt(4, 1)).bold, true);
  key(t, 'ArrowDown');
  key(t, 'ArrowRight');
  type(t, 'half typed');
  t.server.styles.splice(0, t.server.styles.length, {}, { italic: true }, { bold: true });
  t.server.tab(1).cells.set(`${t.model.rowAt(4)}:${t.model.colAt(1)}`, { kind: 'n', value: 5, style: 2 });
  t.server.socket.recv({ type: 'delta', seq: String(++t.server.seq), touched: [2] });
  t.server.socket.recv({ type: 'resync', generation: t.net.generation + 1, seq: String(t.server.seq), date: t.server.date, tabs: t.server.tabInfo() });
  assert.equal(t.editor.open, false);
  assert.deepEqual(notices(t), [['warn', 'The sheet was loaded again while you were typing in B2. Nothing was written. Your text:', 'half typed']]);
  assert.equal(actions(t).length, 0);
  t.server.snapshot(t.server.socket, t.net.generation, 1);
  assert.equal(t.model.format(t.model.cellAt(4, 1)).bold, true, 'the cell keeps its format under the new numbers');
  assert.equal(t.model.format(t.model.cellAt(4, 1)).italic, undefined);
  // the text can be typed again at once
  type(t, 'half typed');
  key(t, 'Enter');
  assert.equal(actions(t)[0].ops[0].text, 'half typed');
});
