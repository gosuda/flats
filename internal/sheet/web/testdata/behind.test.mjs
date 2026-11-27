// A grid that can no longer be trusted is read-only from the moment the
// page knows it (TECH 8.4 and 12.5): after a rollback's reset, after a
// frame of the grid the page refused, and after a delta that does not
// follow the one before it. Each is played through the whole client: the
// open editor closes and offers its text back, no new edit is queued,
// however long the next connection takes, and the actions that were
// unconfirmed before stay queued and go out again.
'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { fire } from './dom.mjs';
import { FakeServer, key, notices, startApp, type } from './server.mjs';

const sheet = () => new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 6, cols: 3, cells: { '0,0': 'top', '5,0': 'bottom', '1,1': '5' } }] });
const clip = (text) => ({ clipboardData: { getData: () => text, setData() {} } });

const EDITS = 8;

// tryEdits makes the eight kinds of change a person can make from the
// grid and the tab bar, and returns how many of them reached the queue.
function tryEdits(t) {
  const before = t.net.pending.length;
  t.sel.set(0, 0);
  type(t, 'new');
  key(t, 'Enter');
  fire(t.doc, 'paste', clip('pasted'));
  key(t, 'Delete');
  key(t, 'b', { ctrlKey: true });
  t.sel.range(0, 0, 2, 0);
  key(t, 'd', { ctrlKey: true });
  key(t, 'z', { ctrlKey: true });
  t.app.insertRows('before');
  t.app.tabs.cb.add('Added');
  return t.net.pending.length - before;
}

test('on a current grid every one of those edits is queued', async () => {
  const t = await startApp({ server: sheet() });
  t.server.manual.add('action');
  assert.equal(tryEdits(t), EDITS);
});

test('a rollback makes the grid read-only at once, until the restored sheet arrives, however late its welcome is', async () => {
  const server = sheet();
  const t = await startApp({ server });
  const tab = server.tab(1);
  // One action is unconfirmed when the rollback comes.
  server.manual.add('action');
  t.sel.set(2, 2);
  type(t, 'kept');
  key(t, 'Enter');
  const unconfirmed = t.net.pending.map((a) => a.n);
  assert.equal(unconfirmed.length, 1);
  server.manual.delete('action');
  // The person is typing in A1 when it comes.
  t.sel.set(0, 0);
  type(t, 'typing');
  assert.equal(t.editor.open, true);

  // The rollback restores the rows in another order, under the same ids.
  // The page never saw the calculation it ran in.
  tab.rows.reverse();
  server.seq++;
  const before = server.socket;
  before.recv({ type: 'reset', seq: String(server.seq) });
  assert.equal(t.app.calculating, false);
  assert.equal(t.model.stale, true, 'the grid is behind at once');
  assert.equal(t.app.canWrite(), false);
  assert.equal(t.editor.open, false, 'the editor closed');
  assert.ok(notices(t).some(([kind, text, kept]) => kind === 'warn' && /rolled back while you were typing in A1/.test(text) && kept === 'typing'), 'and gave its text back');

  // The new connection is open but its welcome has not come.
  assert.notEqual(server.socket, before);
  assert.equal(tryEdits(t), 0, 'nothing is queued against the grid before the rollback');
  t.timers.advance(60000);
  assert.equal(tryEdits(t), 0, 'nor after a long wait');
  assert.deepEqual(t.net.pending.map((a) => a.n), unconfirmed, 'the unconfirmed action stays queued');

  // The welcome arrives; the snapshot of the restored sheet has not.
  server.manual.add('watch');
  const hello = server.socket.open();
  assert.equal('watermark' in hello, false, 'the connection starts from a new snapshot');
  assert.equal(t.model.stale, true);
  assert.equal(tryEdits(t), 0, 'nor between the welcome and the snapshot');
  server.manual.delete('watch');
  server.snapshot(server.socket, t.net.generation, 1);
  assert.deepEqual([t.model.stale, t.model.tab.loading], [false, false]);
  assert.deepEqual(t.model.tab.rows, tab.rows, 'the grid is the restored sheet');

  // The unconfirmed action went out again with its number, and applied once.
  assert.deepEqual(server.socket.frames('action').map((a) => a.n), unconfirmed);
  assert.equal(t.net.pending.length, 0);
  // An edit of A1 now goes to the row the restored sheet has there.
  t.sel.set(0, 0);
  type(t, 'after');
  key(t, 'Enter');
  assert.equal(server.cell(1, 0, 0).value, 'after');
  assert.equal(tab.cells.get(`${tab.rows[0]}:${tab.cols[0]}`).value, 'after');
  assert.equal(server.cell(1, 5, 0).value, 'top', 'the row that was first is now last and kept its text');
});

test('a session the host no longer knows makes the grid read-only at once, until the snapshot for the new session arrives', async () => {
  const server = sheet();
  const t = await startApp({ server });
  const tab = server.tab(1);
  // The connection ends with nothing pending. While it is away the host
  // lets the session go, and a rollback restores the rows in another
  // order, under the same ids. The page hears of neither.
  server.socket.drop();
  server.sessions.clear();
  tab.rows.reverse();
  server.seq++;
  // A current grid can be edited while the connection is away.
  t.sel.set(0, 0);
  type(t, 'typing');
  assert.equal(t.editor.open, true);

  // The next connection names the old session and is told it expired.
  t.timers.advance(1000);
  const refused = server.socket;
  assert.equal(refused.open().session, 'tok1');
  assert.equal(refused.closed, true, 'the host ended that connection');
  assert.equal(t.net.stopped, null, 'with nothing pending, editing goes on');
  assert.equal(t.model.stale, true, 'the grid is behind at once');
  assert.equal(t.app.canWrite(), false);
  assert.equal(t.editor.open, false, 'the editor closed');
  assert.ok(notices(t).some(([kind, , kept]) => kind === 'warn' && kept === 'typing'), 'and gave its text back');

  // The new connection says hello; its welcome is late.
  const ws = server.socket;
  assert.notEqual(ws, refused);
  const held = [];
  const deliver = ws.recv.bind(ws);
  ws.recv = (m) => held.push(m);
  const hello = ws.open();
  assert.deepEqual([hello.session, hello.pending, 'watermark' in hello], [undefined, 0, false]);
  assert.equal(tryEdits(t), 0, 'nothing is queued against the grid of the old session');
  t.timers.advance(10000);
  assert.equal(tryEdits(t), 0, 'nor after a wait');
  ws.recv = deliver;
  for (const m of held) ws.recv(m);
  assert.deepEqual(server.socket.frames('action'), [], 'nothing went out under the new session');
  assert.deepEqual([t.model.stale, t.model.tab.loading], [false, false]);
  assert.deepEqual(t.model.tab.rows, tab.rows, 'the grid is the restored sheet');
  assert.equal(t.net.session, 'tok2');

  // An edit of A1 now goes to the row the restored sheet has there.
  t.sel.set(0, 0);
  type(t, 'after');
  key(t, 'Enter');
  assert.equal(server.cell(1, 0, 0).value, 'after');
  assert.equal(server.cell(1, 5, 0).value, 'top', 'the row that was first is now last and kept its text');
});

// Frames of the grid the page refuses before any handler takes them. None
// comes from the host on a normal path; each stands for a frame that came
// out wrong.
const REFUSED = {
  'a delta part counted as no part': () => ({ type: 'delta_part', seq: '11', part: 0, parts: 0 }),
  'a delta part that starts in the middle': () => ({ type: 'delta_part', seq: '11', part: 1, parts: 2, cells: [] }),
  'a delta inside the parts of another': (ws) => { ws.onmessage({ data: JSON.stringify({ type: 'delta_part', seq: '11', part: 0, parts: 2 }) }); return { type: 'delta', seq: '11' }; },
  'a resync whose generation is a text': (ws, t) => ({ type: 'resync', generation: String(t.net.generation + 1), seq: '11', date: t.server.date, tabs: t.server.tabInfo() }),
  'a resync without a sequence': (ws, t) => ({ type: 'resync', generation: t.net.generation + 1, date: t.server.date, tabs: t.server.tabInfo() }),
  'a resync whose sequence is no number': (ws, t) => ({ type: 'resync', generation: t.net.generation + 1, seq: 'eleven', date: t.server.date, tabs: t.server.tabInfo() }),
  'a resync whose sequence is empty': (ws, t) => ({ type: 'resync', generation: t.net.generation + 1, seq: '', date: t.server.date, tabs: t.server.tabInfo() }),
  'a resync whose tab list is no list': (ws, t) => ({ type: 'resync', generation: t.net.generation + 1, seq: '11', date: t.server.date, tabs: 3 }),
  'a resync without a tab list or a date': (ws, t) => ({ type: 'resync', generation: t.net.generation + 1, seq: '11' }),
  'a resync whose tab list is null': (ws, t) => ({ type: 'resync', generation: t.net.generation + 1, seq: '11', date: t.server.date, tabs: null }),
  'a resync whose date is null': (ws, t) => ({ type: 'resync', generation: t.net.generation + 1, seq: '11', date: null, tabs: t.server.tabInfo() }),
  'a resync whose tab list names a tab twice': (ws, t) => ({ type: 'resync', generation: t.net.generation + 1, seq: '11', date: t.server.date, tabs: [...t.server.tabInfo(), ...t.server.tabInfo()] }),
  'a resync with a tab list and no date': (ws, t) => ({ type: 'resync', generation: t.net.generation + 1, seq: '11', tabs: t.server.tabInfo() }),
  'a resync with a date and no tab list': (ws, t) => ({ type: 'resync', generation: t.net.generation + 1, seq: '11', date: t.server.date }),
  'a delta whose tab list is null': () => ({ type: 'delta', seq: '11', tabs: null }),
  'a delta whose date is a number': () => ({ type: 'delta', seq: '11', date: 20261114 }),
  'a delta part whose date is null': () => ({ type: 'delta_part', seq: '11', part: 0, parts: 1, date: null }),
  'a snapshot head whose more is null': (ws, t) => ({ type: 'tab_head', generation: t.net.generation, tab: 1, seq: '10', date: t.server.date, tabs: t.server.tabInfo(), rows: [], cols: [], widths: {}, frozen_rows: 0, frozen_cols: 0, styles: [{}], more: null }),
  'an object without a type': () => ({}),
  'an object whose type is null': () => ({ type: null }),
  'an object whose type is a number': () => ({ type: 7 }),
};

for (const [name, frame] of Object.entries(REFUSED)) {
  test(`${name} makes the grid read-only at once and gives the editor's text back`, async () => {
    const server = sheet();
    const t = await startApp({ server });
    t.sel.set(1, 1);
    type(t, 'draft');
    assert.equal(t.editor.open, true);
    const ws = server.socket;
    const m = frame(ws, t);
    assert.throws(() => ws.onmessage({ data: JSON.stringify(m) }), TypeError);
    assert.equal(t.model.stale, true, 'the grid is behind');
    assert.equal(t.editor.open, false, 'the editor closed');
    assert.ok(notices(t).some(([kind, , kept]) => kind === 'warn' && kept === 'draft'), 'and gave its text back');
    assert.equal(t.app.canWrite(), false);
    assert.equal(t.net.open, false, 'the connection ended');
    assert.equal(tryEdits(t), 0, 'no edit is queued while the page waits');
    // The next connection starts from a new snapshot.
    t.timers.advance(1000);
    assert.equal(tryEdits(t), 0, 'nor while the next one opens');
    assert.equal('watermark' in server.socket.open(), false);
    assert.deepEqual([t.model.stale, t.model.tab.loading], [false, false]);
    assert.equal(t.app.canWrite(), true);
  });
}

// Frames of a snapshot whose generation, tab or numbering is no value of
// Appendix A, and heads whose tab list or date does not fit, after the
// head of that snapshot arrived. Each could belong
// to the snapshot under way, so it is not passed over as a frame of an
// older one: the page starts over at once, though no frame follows it.
const BAD_SNAPSHOT_FRAMES = {
  'a tab_head whose generation is a text': (f) => ({ ...f.head, generation: String(f.head.generation) }),
  'a tab_head whose sequence is a number': (f) => ({ ...f.head, seq: Number(f.head.seq) }),
  'a tab_head whose sequence has a leading zero': (f) => ({ ...f.head, seq: `0${f.head.seq}` }),
  'a tab_head without its tab list': (f) => ({ ...f.head, tabs: undefined }),
  'a tab_head without its date': (f) => ({ ...f.head, date: undefined }),
  'a tab_head whose date is a number': (f) => ({ ...f.head, date: 20261113 }),
  'a tab_head whose tab list does not name its tab': (f) => ({ ...f.head, tabs: f.head.tabs.filter((x) => x.id !== f.head.tab) }),
  'a tab_head whose tab list names a tab twice': (f) => ({ ...f.head, tabs: [...f.head.tabs, f.head.tabs[0]] }),
  'rows whose generation is a text': (f) => ({ ...f.rows, generation: String(f.rows.generation) }),
  'rows whose generation is no whole number': (f) => ({ ...f.rows, generation: f.rows.generation + 0.5 }),
  'rows whose tab is a text': (f) => ({ ...f.rows, tab: String(f.rows.tab) }),
  'a tab_done whose generation is a text': (f) => ({ ...f.done, generation: String(f.done.generation) }),
  'a tab_done whose generation is negative': (f) => ({ ...f.done, generation: -1 }),
  'a tab_done whose count is a text': (f) => ({ ...f.done, parts: String(f.done.parts) }),
  'a tab_more whose generation is a text': (f) => ({ type: 'tab_more', generation: String(f.head.generation), tab: f.head.tab, styles: [] }),
  'a tab_more whose more is false': (f) => ({ type: 'tab_more', generation: f.head.generation, tab: f.head.tab, styles: [], more: false }),
  'a tab_more of another tab': (f) => ({ type: 'tab_more', generation: f.head.generation, tab: 1, styles: [] }),
};

// snapshotUnderWay opens a second tab with its snapshot held back, and
// lets the head and the first rows frame of that snapshot arrive. With
// more, only the head arrives, and it says that tab_more frames follow.
async function snapshotUnderWay({ more = false } = {}) {
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 6, cols: 3, cells: { '0,0': 'top' } }, { id: 2, name: 'Other', rows: 4, cols: 2, cells: { '1,1': 'x' } }] });
  const t = await startApp({ server });
  server.manual.add('watch');
  t.app.openTab(2);
  const watch = server.socket.frames('watch').at(-1);
  const frames = [];
  server.snapshot({ recv: (m) => frames.push(m) }, watch.generation, watch.tab);
  const f = { head: frames[0], rows: frames[1], done: frames.at(-1) };
  const head = more ? { ...f.head, more: true } : f.head;
  server.socket.onmessage({ data: JSON.stringify(head) });
  if (!more) {
    server.socket.onmessage({ data: JSON.stringify(f.rows) });
    assert.deepEqual([t.model.tab.id, t.model.tab.loading, t.app.canWrite()], [2, true, true], 'the tab is interactive while it loads');
  }
  return { t, server, f };
}

for (const [name, make] of Object.entries(BAD_SNAPSHOT_FRAMES)) {
  test(`${name} starts the grid over from a new snapshot`, async () => {
    const { t, server, f } = await snapshotUnderWay({ more: name.includes('tab_more') });
    const ws = server.socket;
    const bad = make(f);
    assert.throws(() => ws.onmessage({ data: JSON.stringify(bad) }), TypeError);
    // Nothing else arrives, and the page has started over all the same.
    assert.equal(t.model.stale, true, 'the grid is behind');
    assert.equal(t.app.canWrite(), false, 'no edit starts on it');
    assert.equal(t.net.open, false, 'the connection ended');
    server.manual.delete('watch');
    t.timers.advance(1000);
    assert.equal('watermark' in server.socket.open(), false, 'the next connection resumes nothing');
    assert.deepEqual([t.model.tab.id, t.model.stale, t.model.tab.loading, t.app.canWrite()], [2, false, false, true]);
  });
}

test('frames of an older snapshot are passed over, and the snapshot under way completes', async () => {
  const { t, server, f } = await snapshotUnderWay();
  const ws = server.socket;
  const older = f.head.generation - 1;
  ws.onmessage({ data: JSON.stringify({ ...f.rows, generation: older, part: 7 }) });
  ws.onmessage({ data: JSON.stringify({ ...f.done, generation: older, parts: 9 }) });
  ws.onmessage({ data: JSON.stringify({ type: 'tab_more', generation: older, tab: f.head.tab, styles: [] }) });
  assert.deepEqual([t.net.open, t.model.stale, t.model.tab.loading], [true, false, true]);
  ws.onmessage({ data: JSON.stringify(f.done) });
  assert.deepEqual([t.net.open, t.model.stale, t.model.tab.loading, t.app.canWrite()], [true, false, false, true]);
});

// Snapshot frames that continue no snapshot the page has: of a newer
// generation than any it asked for, or of the newest one before that
// snapshot's head. The snapshot they would belong to never ends, so the
// page starts over at once, though no frame follows. Frames of older
// generations are still passed over (above).
const UNMATCHED = {
  'rows of a newer generation': ({ f }) => ({ ...f.rows, generation: f.head.generation + 1, part: 1 }),
  'a tab_done of a newer generation': ({ f }) => ({ ...f.done, generation: f.head.generation + 1 }),
  'a tab_more of a newer generation': ({ f }) => ({ type: 'tab_more', generation: f.head.generation + 1, tab: f.head.tab, styles: [] }),
  'a tab_more after a whole head': ({ f }) => ({ type: 'tab_more', generation: f.head.generation, tab: f.head.tab, styles: [] }),
  'rows of the newest generation before its head': ({ t, f }) => {
    t.app.openTab(1);
    return { ...f.rows, generation: t.net.generation, tab: 1, part: 1 };
  },
};

for (const [name, make] of Object.entries(UNMATCHED)) {
  test(`${name} starts the grid over from a new snapshot`, async () => {
    const { t, server, f } = await snapshotUnderWay();
    const ws = server.socket;
    const bad = make({ t, f });
    assert.throws(() => ws.onmessage({ data: JSON.stringify(bad) }), TypeError);
    assert.equal(t.model.stale, true, 'the grid is behind');
    assert.equal(t.app.canWrite(), false, 'no edit starts on it');
    assert.equal(t.net.open, false, 'the connection ended');
    server.manual.delete('watch');
    t.timers.advance(1000);
    assert.equal('watermark' in server.socket.open(), false, 'the next connection resumes nothing');
    assert.deepEqual([t.model.stale, t.model.tab.loading, t.app.canWrite()], [false, false, true]);
  });
}

// Deltas whose sequence is no decimal text. The page compares sequences,
// and sends them back as the base of its next action and as the watermark
// of its next hello, so such a delta is refused before anything of it is
// taken or held. The unconfirmed action stays queued and goes out again
// with its own base.
const BAD_SEQUENCES = {
  'a delta whose sequence is a number': { type: 'delta', seq: 11 },
  'a delta whose sequence has a leading zero': { type: 'delta', seq: '011' },
  'a delta whose sequence has a sign': { type: 'delta', seq: '+11' },
  'a delta whose sequence is past what the host reads': { type: 'delta', seq: '18446744073709551616' },
  'a delta in one part whose sequence is a number': { type: 'delta_part', seq: 11, part: 0, parts: 1 },
  'a delta in one part whose sequence has a leading zero': { type: 'delta_part', seq: '011', part: 0, parts: 1 },
  'a first delta part whose sequence is a number': { type: 'delta_part', seq: 11, part: 0, parts: 2 },
};

for (const [name, bad] of Object.entries(BAD_SEQUENCES)) {
  test(`${name} is refused, and the grid starts over keeping the unconfirmed action`, async () => {
    const server = sheet();
    const t = await startApp({ server });
    server.manual.add('action');
    t.sel.set(2, 2);
    type(t, 'kept');
    key(t, 'Enter');
    const unconfirmed = t.net.pending.map((a) => [a.n, a.base]);
    assert.deepEqual(unconfirmed, [['1', '10']]);
    server.manual.delete('action');
    t.sel.set(1, 1);
    type(t, 'draft');
    const ws = server.socket;
    assert.throws(() => ws.onmessage({ data: JSON.stringify(bad) }), TypeError);
    // Nothing else arrives.
    assert.equal(t.model.stale, true, 'the grid is behind');
    assert.equal(t.model.seq, '10', 'the grid keeps its sequence');
    assert.equal(t.net.lastSeq, null, 'and the stream is not resumed from it');
    assert.equal(t.net.open, false, 'the connection ended');
    assert.ok(notices(t).some(([kind, , kept]) => kind === 'warn' && kept === 'draft'), 'the editor gave its text back');
    assert.equal(tryEdits(t), 0);
    assert.deepEqual(t.net.pending.map((a) => [a.n, a.base]), unconfirmed, 'the unconfirmed action stays queued');
    t.timers.advance(1000);
    assert.equal('watermark' in server.socket.open(), false, 'the next connection resumes nothing');
    assert.deepEqual(server.socket.frames('action').map((a) => [a.n, a.base]), unconfirmed, 'the action went out again as it was');
    assert.deepEqual([t.model.stale, t.model.tab.loading, t.app.canWrite()], [false, false, true]);
    assert.equal(server.cell(1, 2, 2).value, 'kept');
    // The next edit names the snapshot's sequence as text.
    server.manual.add('action');
    t.sel.set(0, 0);
    type(t, 'next');
    key(t, 'Enter');
    assert.equal(t.net.pending.at(-1).base, t.model.seq);
    assert.equal(typeof t.model.seq, 'string');
  });
}

// Welcomes that do not fit Appendix A, laid over a good one. An empty
// session token or session id is no value the page could tell from a
// missing one.
const BAD_WELCOMES = {
  'a welcome without a tab list': { tabs: 3 },
  'a welcome whose sequence is no number': { seq: 'eleven' },
  'a welcome whose sequence is empty': { seq: '' },
  'a welcome whose session token is empty': { session: '' },
  'a welcome whose session id is empty': { session_id: '' },
  'a welcome whose session token is no text': { session: 7 },
  'a welcome whose session token is null': { session: null },
  'a welcome whose identity is null': { me: null },
  'a welcome without a date': { date: undefined },
  'a welcome whose access is no access': { access: 'owner' },
  'a welcome whose label is null': { me: { label: null, verified: false } },
  'a welcome whose label is empty': { me: { label: '', verified: false } },
  'a welcome whose identity has no verified mark': { me: { label: 'Ann' } },
  'a welcome whose tab list names a tab as null': { tabs: [{ id: 1, name: null, rows: 6, cols: 3, frozen_rows: 0, frozen_cols: 0 }] },
  'a welcome with a date and no tab list': { tabs: undefined },
  // Only the welcome of a stream that goes on from the watermark leaves
  // its tab list and date out.
  'a welcome without a tab list and a date that names another sequence': { tabs: undefined, date: undefined, seq: '11' },
};

for (const [name, bad] of Object.entries(BAD_WELCOMES)) {
  test(`${name} is refused as a frame of the grid`, async () => {
    const server = sheet();
    const t = await startApp({ server });
    server.socket.drop();
    server.manual.add('hello');
    t.timers.advance(1000);
    const ws = server.socket;
    ws.open();
    t.sel.set(1, 1);
    type(t, 'offline');
    assert.equal(t.editor.open, true, 'a current grid can be edited while the connection is away');
    const session = t.net.session;
    assert.throws(() => ws.onmessage({ data: JSON.stringify({ type: 'welcome', access: 'private', session: 'tok1', session_id: 's1', seq: '10', date: server.date, tabs: [], ...bad }) }), TypeError);
    assert.equal(t.model.stale, true);
    assert.equal(t.editor.open, false);
    assert.deepEqual(t.model.tabs.map((x) => x.id), [1], 'the tab list stays the one the page had');
    assert.equal(t.net.session, session, 'the session stays the one the page had');
    assert.equal(t.net.open, false);
    assert.equal(tryEdits(t), 0);
    server.manual.delete('hello');
    t.timers.advance(2000);
    server.socket.open();
    assert.equal(t.app.canWrite(), true);
  });
}

// A resync that skipped deltas carries the tab list and the date those
// deltas changed. One whose tab list is null carries neither in a form the
// page can install, so it is refused: the watermark stays where it was,
// the next connection resumes nothing, and its welcome and snapshot bring
// the tab list and the date as they are.
test('a resync whose tab list is null is refused, and a new snapshot brings the tab list and the date', async () => {
  const server = sheet();
  const t = await startApp({ server });
  t.sel.set(1, 1);
  type(t, 'draft');
  // The deltas the resync skips renamed the watched tab and moved the date.
  server.tab(1).name = 'Renamed';
  server.date = '2026-11-14';
  server.seq = 11;
  const ws = server.socket;
  assert.equal(t.net.lastSeq, '10');
  assert.throws(() => ws.onmessage({ data: JSON.stringify({ type: 'resync', generation: t.net.generation + 1, seq: '11', date: server.date, tabs: null }) }), TypeError);
  assert.equal(t.net.lastSeq, null, 'the stream moved neither to the resync nor stays resumable');
  assert.equal(t.model.stale, true, 'the grid is behind');
  assert.equal(t.net.open, false, 'the connection ended');
  assert.ok(notices(t).some(([kind, , kept]) => kind === 'warn' && kept === 'draft'), 'the editor gave its text back');
  assert.deepEqual([t.model.tabs[0].name, t.model.date], ['Data', '2026-11-13'], 'the page installed nothing of it');
  assert.equal(tryEdits(t), 0);
  t.timers.advance(1000);
  assert.equal('watermark' in server.socket.open(), false, 'the next connection resumes nothing');
  assert.deepEqual([t.model.stale, t.model.tab.loading], [false, false]);
  assert.deepEqual([t.model.tabs[0].name, t.model.date], ['Renamed', '2026-11-14'], 'the tab list and the date are current');
  assert.equal(t.model.seq, '11');
  assert.equal(t.app.canWrite(), true);
});

test('a delta that does not follow makes the grid read-only before the next connection opens', async () => {
  const server = sheet();
  const t = await startApp({ server });
  t.sel.set(1, 1);
  type(t, 'draft');
  const before = server.socket;
  before.recv({ type: 'delta', seq: '15' });
  assert.notEqual(server.socket, before, 'the connection starts over');
  assert.equal(t.model.stale, true);
  assert.equal(t.editor.open, false);
  assert.ok(notices(t).some(([, , kept]) => kept === 'draft'));
  assert.equal(tryEdits(t), 0);
  server.seq = 15;
  assert.equal('watermark' in server.socket.open(), false);
  assert.equal(t.app.canWrite(), true);
});

test('a snapshot that misses one of its rows frames is not taken as whole, whatever came in its place', async () => {
  for (const instead of [null, { type: 'later_kind' }, { type: 'rows', generation: 0, tab: 2, part: 1, rows: [] }]) {
    const server = new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 30, cols: 3, cells: { '0,0': 'a' } }, { id: 2, name: 'Other', rows: 30, cols: 3, cells: { '0,0': 'two', '20,1': 'end' } }] });
    const t = await startApp({ server });
    const { model, net } = t;
    const ws = server.socket;
    server.manual.add('watch');
    t.app.openTab(2);
    const frames = [];
    server.snapshot({ recv: (m) => frames.push(m) }, net.generation, 2, 1);
    assert.deepEqual(frames.map((f) => f.type), ['tab_head', 'rows', 'rows', 'tab_done']);
    ws.recv(frames[0]);
    ws.recv(frames[1]);
    // The second rows frame never comes; a frame of a kind this client
    // does not know is left alone, and so is one of an older snapshot.
    if (instead) ws.recv(instead);
    assert.equal(net.open, true, 'nothing so far ends the connection');
    assert.throws(() => ws.recv(frames[3]), TypeError);
    assert.equal(model.tab.loading, true, 'the snapshot is not whole');
    assert.equal(model.stale, true);
    assert.equal(t.app.canWrite(), false);
    assert.equal(net.open, false);
    server.manual.delete('watch');
    t.timers.advance(1000);
    assert.equal('watermark' in server.socket.open(), false);
    assert.deepEqual([model.tab.id, model.stale, model.tab.loading], [2, false, false]);
    assert.equal(model.shown(model.rowAt(20), model.colAt(1)), 'end');
  }
});
