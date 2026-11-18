'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { Net } from '../net.js';
import { ops } from '../protocol.js';
import { FakeServer, FakeSocket, Timers } from './server.mjs';

// setup connects a Net to a fake server and records what its handlers get.
function setup(opts = {}) {
  const server = new FakeServer(opts);
  const timers = new Timers();
  const events = [];
  const rec = (name) => (...args) => { events.push([name, ...args]); };
  const net = new Net({
    welcome: rec('welcome'), tabHead: rec('tabHead'), rows: rec('rows'), tabDone: rec('tabDone'), delta: rec('delta'),
    resync: rec('resync'), reset: rec('reset'), ack: rec('ack'), peeked: rec('peeked'), rebased: rec('rebased'),
    presence: rec('presence'), overwritten: rec('overwritten'), calculating: rec('calculating'), stopped: rec('stopped'),
  }, { connect: () => server.connect(), timers, now: () => timers.now });
  net.start();
  const names = () => events.map((e) => e[0]);
  const of = (name) => events.filter((e) => e[0] === name).map((e) => e.slice(1));
  return { server, net, timers, events, names, of };
}

const setA1 = (text) => ops.set(1, 1000, 1020, text, [0, 0]);

test('hello names the version and the pending count, and the welcome gives access, session and tabs', () => {
  const { server, net, of } = setup();
  assert.deepEqual(server.socket.open(), { type: 'hello', version: 1, pending: 0 });
  assert.equal(net.access, 'private');
  assert.equal(net.session, 'tok1');
  assert.equal(net.sessionId, 's1');
  assert.deepEqual(net.me, { label: 'Ann', verified: true });
  assert.equal(net.lastSeq, '10');
  assert.equal(net.status, 'saved');
  const [welcome, info] = of('welcome')[0];
  assert.equal(welcome.tabs[0].name, 'Sheet1');
  assert.equal(info.resumed, false);
});

test('watch asks for a snapshot under a new generation and drops frames of older ones', () => {
  const { server, net, names, of } = setup();
  server.socket.open();
  server.manual.add('watch');
  net.watch(1);
  net.watch(1);
  assert.deepEqual(server.socket.frames('watch'), [{ type: 'watch', generation: 1, tab: 1 }, { type: 'watch', generation: 2, tab: 1 }]);
  server.snapshot(server.socket, 1, 1);
  assert.equal(names().includes('tabHead'), false, 'the first generation is stale');
  server.snapshot(server.socket, 2, 1);
  assert.deepEqual(names().filter((n) => n !== 'welcome'), ['tabHead', 'rows', 'tabDone']);
  assert.equal(of('tabHead')[0][0].seq, '10');
});

test('a head in several frames counts once, when its last tab_more arrived', () => {
  const { server, net, names, of } = setup();
  server.socket.open();
  const t = server.tab(1);
  t.rows.reverse(); // every row is a run of its own
  t.styles.push({ bold: true }, { italic: true });
  server.headRuns = 8;
  net.watch(1);
  assert.deepEqual(names().filter((n) => n !== 'welcome'), ['tabHead', 'rows', 'tabDone']);
  const [head] = of('tabHead')[0];
  assert.deepEqual(head.rows, t.rows.map((id) => [id, 1]), 'the runs of all frames, in order');
  assert.deepEqual(head.styles, [{}, { bold: true }, { italic: true }]);
  assert.equal('more' in head, false);
  assert.equal(net.generation, 1);
  assert.equal(net.lastSeq, '10');
});

test('an unfinished head is dropped by a newer watch and by a lost connection', () => {
  const { server, net, names, of } = setup();
  server.socket.open();
  server.manual.add('watch');
  const more = (generation, fields) => server.socket.recv({ type: 'tab_more', generation, tab: 1, ...fields });
  const first = (generation) => server.socket.recv({ type: 'tab_head', generation, tab: 1, seq: '10', date: server.date, tabs: server.tabInfo(), rows: [[1000, 2]], cols: [[1020, 6]], widths: {}, frozen_rows: 0, frozen_cols: 0, styles: [], more: true });
  net.watch(1);
  first(1);
  assert.equal(names().includes('tabHead'), false, 'a head with more to come is not shown');
  net.watch(1);
  more(1, { rows: [[1002, 18]], styles: [{}] });
  assert.equal(names().includes('tabHead'), false, 'the older generation is stale');
  assert.equal(net.head, null, 'and its head, now whole, is let go');
  // A tab_more of an older generation is passed over, whatever head is under way.
  more(1, { rows: [[1, 1]] });
  first(2);
  more(1, { rows: [[1, 1]] });
  more(2, { rows: [[1002, 18]], more: true });
  more(2, { styles: [{}] });
  assert.deepEqual(of('tabHead').map(([m]) => [m.generation, m.rows]), [[2, [[1000, 2], [1002, 18]]]]);
  // A connection that ends inside a head leaves nothing behind.
  net.watch(1);
  first(3);
  server.socket.drop();
  assert.equal(net.head, null);
});

test('deltas arrive in sequence: a duplicate is dropped and a gap starts over from a new snapshot', () => {
  const { server, net, of } = setup();
  server.socket.open();
  net.watch(1);
  server.setCell(1, 0, 0, 'a');
  assert.equal(net.lastSeq, '11');
  server.socket.recv({ type: 'delta', seq: '11', cells: [] });
  assert.equal(of('delta').length, 1, 'the duplicate is not delivered');
  const first = server.socket;
  first.recv({ type: 'delta', seq: '14' });
  assert.equal(of('delta').length, 1, 'the delta after a gap is not delivered');
  assert.notEqual(server.socket, first, 'the client reconnected');
  const hello = server.socket.open();
  assert.equal(hello.watermark, undefined, 'without a watermark the server starts over');
  assert.equal(hello.session, 'tok1');
  assert.equal(of('welcome').at(-1)[1].resumed, false);
});

test('a large delta arrives as delta_part frames and is applied once, joined as values', () => {
  const { server, net, of } = setup();
  server.socket.open();
  let bytes = 0;
  const part = (i, fields) => {
    const m = { type: 'delta_part', seq: '11', part: i, parts: 3, ...fields };
    bytes += Buffer.byteLength(JSON.stringify(m));
    server.socket.recv(m);
  };
  part(0, { from: { session_id: 's2', n: '4' }, cells: [[1000, [[1020, 't', 'a']]], [1001, [[1020, 't', 'b']]]], struct: [{ op: 'sort', tab: 1, positions: [[0, 2]], ids: [1001], more: true }] });
  part(1, { struct: [{ op: 'sort', tab: 1, ids: [1000] }, { op: 'rename_tab', tab: 2, name: 'B' }], cells: [[1001, [[1021, 't', 'c']]], [1002, [[1020, 't', 'd']]]] });
  assert.equal(of('delta').length, 0, 'nothing is taken before the last part');
  assert.equal(net.lastSeq, '10');
  part(2, { styles: [{ bold: true }], cells: [[1002, [[1021, 't', 'e']]]] });
  assert.equal(of('delta').length, 1);
  const [d, size] = of('delta')[0];
  assert.equal(net.lastSeq, '11');
  assert.deepEqual(d, {
    type: 'delta', seq: '11', from: { session_id: 's2', n: '4' },
    cells: [[1000, [[1020, 't', 'a']]], [1001, [[1020, 't', 'b'], [1021, 't', 'c']]], [1002, [[1020, 't', 'd'], [1021, 't', 'e']]]],
    struct: [{ op: 'sort', tab: 1, positions: [[0, 2]], ids: [1001, 1000] }, { op: 'rename_tab', tab: 2, name: 'B' }],
    styles: [{ bold: true }],
  });
  assert.equal(size(), bytes, 'the size of a delta in parts is that of its parts');
});

test('a resync drops a delta whose parts are not all in', () => {
  const { server, net, of } = setup();
  server.socket.open();
  server.socket.recv({ type: 'delta_part', seq: '11', part: 0, parts: 3, cells: [[1000, [[1020, 't', 'a']]]] });
  server.socket.recv({ type: 'resync', generation: 5, seq: '12', date: server.date, tabs: server.tabInfo() });
  assert.equal(of('delta').length, 0);
  assert.equal(net.lastSeq, '12');
  assert.equal(net.open, true);
  server.socket.recv({ type: 'delta', seq: '13', cells: [[1000, [[1020, 't', 'b']]]] });
  assert.deepEqual(of('delta').map(([d]) => d.seq), ['13']);
});

test('delta parts that do not follow one another end the connection without taking anything', () => {
  const cells = [[1000, [[1020, 't', 'x']]]];
  for (const [what, frames] of [
    ['a part missing', [{ part: 0, parts: 3 }, { part: 2, parts: 3 }]],
    ['another count', [{ part: 0, parts: 3 }, { part: 1, parts: 4 }]],
    ['a part of another delta', [{ part: 0, parts: 2 }, { part: 1, parts: 2, seq: '12' }]],
    ['a new delta before the last part', [{ part: 0, parts: 2 }, { part: 0, parts: 2 }]],
    ['a field twice', [{ part: 0, parts: 2, from: { session_id: 's2', n: '4' } }, { part: 1, parts: 2, from: { session_id: 's2', n: '4' } }]],
    ['an edit going on into another', [{ part: 0, parts: 2, struct: [{ op: 'sort', tab: 1, ids: [1], more: true }] }, { part: 1, parts: 2, struct: [{ op: 'sort', tab: 2, ids: [2] }] }]],
    ['an edit going on after the last part', [{ part: 0, parts: 1, struct: [{ op: 'sort', tab: 1, ids: [1], more: true }] }]],
    ['a count of zero', [{ part: 0, parts: 0, cells }]],
    ['a negative count', [{ part: 0, parts: -1, cells }]],
    ['no count', [{ part: 0, cells }]],
    ['a count that is no whole number', [{ part: 0, parts: 1.5, cells }]],
    ['a count as text', [{ part: 0, parts: '1', cells }]],
    ['a count that changes after a whole first part', [{ part: 0, parts: 2, cells }, { part: 1, parts: 1.5, cells }]],
    ['a negative number', [{ part: -1, parts: 1, cells }]],
    ['no number', [{ parts: 1, cells }]],
    ['a number that is no whole number', [{ part: 0, parts: 2, cells }, { part: 0.5, parts: 2, cells }]],
    ['a number as text', [{ part: '0', parts: 1, cells }]],
    ['a number past the count', [{ part: 0, parts: 2, cells }, { part: 2, parts: 2, cells }]],
  ]) {
    const { server, net, of } = setup();
    server.socket.open();
    assert.throws(() => { for (const f of frames) server.socket.recv({ type: 'delta_part', seq: '11', ...f }); }, TypeError, what);
    assert.equal(of('delta').length, 0, what);
    assert.equal(net.open, false, what);
    assert.equal(net.lastSeq, null, `${what}: the next connection resumes nothing`);
  }
});

test('a resync that names no generation, sequence or tab list ends the connection without taking anything', () => {
  for (const [what, f] of [
    ['no generation', { seq: '12' }],
    ['a generation as text', { generation: '5', seq: '12' }],
    ['a generation that is no whole number', { generation: 5.5, seq: '12' }],
    ['no sequence', { generation: 5 }],
    ['a sequence as a number', { generation: 5, seq: 12 }],
    ['a tab list that is no list', { generation: 5, seq: '12', date: '2026-11-14', tabs: { id: 1 } }],
    ['a tab list that is null', { generation: 5, seq: '12', date: '2026-11-14', tabs: null }],
    ['a date that is null', { generation: 5, seq: '12', date: null, tabs: [] }],
    ['a date that is no text', { generation: 5, seq: '12', date: 20261114, tabs: [] }],
    ['a tab list without its date', { generation: 5, seq: '12', tabs: [] }],
    ['a date without its tab list', { generation: 5, seq: '12', date: '2026-11-14' }],
    ['neither a tab list nor a date', { generation: 5, seq: '12' }],
  ]) {
    const { server, net, of } = setup();
    server.socket.open();
    const generation = net.generation;
    assert.throws(() => server.socket.recv({ type: 'resync', ...f }), TypeError, what);
    assert.equal(of('resync').length, 0, what);
    assert.equal(net.generation, generation, what);
    assert.equal(net.open, false, what);
    assert.equal(net.lastSeq, null, `${what}: the next connection resumes nothing`);
  }
});

test('actions carry increasing n and their base, and the ack removes them from the queue', () => {
  const { server, net, of } = setup();
  server.socket.open();
  net.watch(1);
  const a = net.queue([setA1('one')], '10', { label: 'x' });
  const b = net.queue([setA1('two')], '10');
  assert.equal(net.status, 'saving');
  net.flush();
  assert.deepEqual(server.socket.frames('action').map((f) => [f.n, f.base]), [['1', '10'], ['2', '10']]);
  assert.equal(a.n, '1');
  assert.equal(b.n, '2');
  assert.equal(net.pending.length, 0);
  assert.equal(net.status, 'saved');
  const acks = of('ack');
  assert.deepEqual(acks.map(([action, m]) => [action.n, m.results[0].status]), [['1', 'applied'], ['2', 'applied']]);
  assert.equal(acks[0][0].meta.label, 'x');
  // the delta of an action comes before its ack and names its author
  assert.deepEqual(of('delta')[0][0].from, { session_id: 's1', n: '1' });
});

test('after a reconnect every unconfirmed action is sent again as it was, and applies once', () => {
  const { server, net, timers, of } = setup();
  server.socket.open();
  net.watch(1);
  // the first action reaches the server, but its delta and ack are lost
  const lost = server.socket;
  lost.recv = () => {};
  net.queue([setA1('kept')], '10');
  net.flush();
  net.queue([setA1('later')], '10');
  net.flush();
  assert.equal(server.applied.length, 2);
  assert.equal(net.pending.length, 2);
  lost.drop();
  assert.equal(net.status, 'offline');
  // an edit made while offline waits in the queue
  net.queue([ops.set(1, 1001, 1020, 'offline', [1, 0])], '10');
  net.flush();
  timers.advance(1000);
  const hello = server.socket.open();
  assert.deepEqual([hello.session, hello.watermark, hello.tab, hello.pending], ['tok1', '10', 1, 3]);
  assert.equal(of('welcome').at(-1)[1].resumed, true);
  assert.deepEqual(server.socket.frames('action').map((f) => [f.n, f.base, f.ops[0].text]), [['1', '10', 'kept'], ['2', '10', 'later'], ['3', '10', 'offline']]);
  assert.deepEqual(server.applied, [['s1', '1'], ['s1', '2'], ['s1', '3']], 'no action applied twice');
  assert.equal(net.pending.length, 0);
  assert.equal(net.status, 'saved');
  // the replay brought the two missed deltas, then the new one
  assert.deepEqual(of('delta').map(([d]) => d.seq), ['11', '12', '13']);
  assert.equal(server.cell(1, 0, 0).value, 'later');
});

test('a resume the server cannot replay brings a snapshot, and deltas continue after its sequence', () => {
  const { server, net, timers, of } = setup();
  server.socket.open();
  net.watch(1);
  server.setCell(1, 0, 0, 'a');
  const old = server.socket;
  old.closed = true; // the next two changes never arrive
  server.setCell(1, 0, 1, 'b');
  server.setCell(1, 0, 2, 'c');
  old.onclose();
  server.resume = 'snapshot';
  timers.advance(1000);
  server.socket.open();
  assert.equal(of('tabHead').at(-1)[0].seq, '13');
  assert.equal(net.lastSeq, '13');
  server.setCell(1, 1, 1, 'd');
  assert.equal(of('delta').at(-1)[0].seq, '14', 'the stream goes on without a gap');
});

test('deltas between the welcome and the snapshot of a resume that was not replayed are no gap', () => {
  const { server, net, timers, of } = setup();
  server.socket.open();
  net.watch(1);
  server.setCell(1, 0, 0, 'a');
  server.socket.drop();
  server.seq = 20; // the log no longer reaches back to 11
  server.manual.add('hello');
  timers.advance(1000);
  const ws = server.socket;
  assert.equal(ws.open().watermark, '11');
  // The server names its current sequence, sends what was committed before
  // the snapshot was taken, then the snapshot.
  ws.recv({ type: 'welcome', access: 'private', session: 'tok1', session_id: 's1', me: server.me, seq: '20', date: server.date, tabs: server.tabInfo() });
  ws.recv({ type: 'delta', seq: '21', touched: [1] });
  server.seq = 21;
  server.snapshot(ws, 0, 1);
  assert.equal(server.socket, ws, 'the connection was not started over');
  assert.deepEqual(of('delta').map(([d]) => d.seq), ['11', '21']);
  assert.equal(of('tabHead').at(-1)[0].seq, '21');
  assert.equal(net.lastSeq, '21');
});

test('a session the server no longer knows stops editing when edits are unconfirmed', () => {
  const { server, net, timers, of } = setup();
  server.socket.open();
  net.watch(1);
  server.manual.add('action');
  net.queue([setA1('unsaved')], '10');
  net.flush();
  server.socket.drop();
  server.sessions.clear();
  timers.advance(1000);
  server.socket.open();
  assert.deepEqual(of('stopped')[0], ['session_expired', {}]);
  assert.equal(net.status, 'stopped');
  assert.equal(net.pending[0].ops[0].text, 'unsaved', 'the text stays for copying');
  assert.equal(server.socket.frames('action').length, 0, 'nothing is renumbered or resent');
});

test('with nothing unconfirmed an unknown session simply becomes a new one', () => {
  const { server, net, timers } = setup();
  server.socket.open();
  server.socket.drop();
  server.sessions.clear();
  timers.advance(1000);
  // The server ends the connection that names the unknown session; the
  // client then connects again without one.
  const refused = server.socket;
  refused.open();
  assert.deepEqual(refused.closed, true);
  assert.notEqual(server.socket, refused);
  assert.deepEqual(server.socket.open(), { type: 'hello', version: 1, pending: 0 });
  assert.equal(net.stopped, null);
  assert.equal(net.session, 'tok2');
  assert.equal(net.queue([setA1('x')], '10').n, '1');
});

test('a temporary refusal reconnects with backoff, and other fatal codes stop', () => {
  const { server, net, timers, of } = setup();
  server.socket.open();
  const first = server.socket;
  first.recv({ type: 'fatal', code: 'unavailable' });
  assert.equal(net.stopped, null);
  assert.equal(net.status, 'offline');
  timers.advance(999);
  assert.equal(server.socket, first);
  timers.advance(1);
  assert.notEqual(server.socket, first);
  // the socket fails again before a welcome: the delay doubles
  const second = server.socket;
  second.drop();
  timers.advance(1999);
  assert.equal(server.socket, second);
  timers.advance(1);
  server.socket.open();
  server.socket.recv({ type: 'fatal', code: 'version', message: 'reload' });
  assert.deepEqual(of('stopped')[0], ['version', { message: 'reload' }]);
  timers.advance(60000);
  assert.equal(server.socket.closed, true, 'a stopped connection does not reconnect');
});

test('resync announces a new snapshot generation and reset starts the connection over', () => {
  const { server, net, names, of } = setup();
  server.socket.open();
  net.watch(1);
  server.socket.recv({ type: 'resync', generation: 5, seq: '30', date: server.date, tabs: server.tabInfo() });
  assert.equal(net.lastSeq, '30');
  server.snapshot(server.socket, 1, 1);
  assert.equal(of('tabHead').length, 1, 'frames of the generation before the resync are stale');
  server.seq = 30;
  server.snapshot(server.socket, 5, 1);
  assert.equal(of('tabHead').length, 2);
  const before = server.socket;
  before.recv({ type: 'reset', seq: '31' });
  assert.ok(names().includes('reset'));
  assert.notEqual(server.socket, before);
  assert.equal(server.socket.open().watermark, undefined);
});

test('peek, rebase and presence are sent as the schema names them', () => {
  const { server, net, timers, of } = setup();
  server.socket.open();
  net.watch(1);
  server.manual.add('rebase');
  assert.equal(net.peek(net.requestId(), 1, 1000, 1020), true);
  assert.deepEqual(server.socket.frames('peek')[0], { type: 'peek', id: 1, tab: 1, row: 1000, col: 1020 });
  assert.equal(of('peeked')[0][0].id, 1);
  net.rebase(net.requestId(), { tab: 1, row: 1000, col: 1020, text: '=B5+', at: [0, 0], base: '10', through: '11' });
  assert.deepEqual(server.socket.frames('rebase')[0], { type: 'rebase', id: 2, tab: 1, row: 1000, col: 1020, text: '=B5+', at: [0, 0], base: '10', through: '11' });
  // presence stays under the 20 updates a second the server takes, and the newest one wins
  net.presence({ tab: 1, active: [1000, 1020], editing: false });
  net.presence({ tab: 1, active: [1001, 1020], editing: false });
  net.presence({ tab: 1, active: [1002, 1020], editing: false });
  assert.equal(server.presence.length, 1);
  timers.advance(59);
  assert.equal(server.presence.length, 1);
  timers.advance(1);
  assert.deepEqual(server.presence.map((p) => p.active[0]), [1000, 1002]);
  for (let i = 0; i < 100; i++) { net.presence({ tab: 1, active: [1000 + (i % 20), 1020], editing: false }); timers.advance(10); }
  assert.ok(server.presence.length <= 2 + 17, `a second of moves sent ${server.presence.length - 2} updates`);
});

test('a Public connection sends no action, presence or rebase whatever is asked', () => {
  const { server, net } = setup({ access: 'public' });
  server.socket.open();
  net.watch(1);
  assert.equal(net.session, null);
  assert.equal(net.queue([setA1('x')], '10'), null);
  net.presence({ tab: 1, active: [1000, 1020] });
  assert.equal(net.rebase(net.requestId(), { tab: 1, row: 1, col: 1, text: '=A1', at: [0, 0], base: '10', through: '11' }), false);
  assert.deepEqual(server.socket.sent.map((m) => m.type), ['hello', 'watch']);
  assert.equal(net.peek(net.requestId(), 1, 1000, 1020), true, 'a Public reader may peek');
});

test('the fake socket rejects frames and fields outside Appendix A', () => {
  const ws = new FakeSocket(null);
  assert.throws(() => ws.send(JSON.stringify({ type: 'hello', version: 1, pending: 0, editor: 'x' })), /unknown field editor/);
  assert.throws(() => ws.send(JSON.stringify({ type: 'subscribe' })), /the frame is of the unknown kind subscribe/);
  // The text of a frame is read, not a parsed copy of it.
  assert.throws(() => ws.send('{"type":"hello","version":1,"version":1,"pending":0}'), /the field version twice/);
  assert.throws(() => ws.send('{"type":"hello","VERSION":1,"pending":0}'), /unknown field VERSION/);
  assert.throws(() => ws.send('{"type":"hello","version":1.0,"pending":0}'), /version is a fraction/);
  assert.throws(() => ws.send('{"type":"action","n":"1","base":"1","ops":[{"op":"write","tab":1,"rows":[1],"cols":[1],"values":[[null]]}]}'), /values\[0\]\[0\] is null/);
  ws.send('{"type":"hello","v\\u0065rsion":1,"pending":0}');
  assert.deepEqual(ws.sent, [{ type: 'hello', version: 1, pending: 0 }], 'a name written with an escape is the name');
  assert.throws(() => ws.send(JSON.stringify({ type: 'action', n: '1', base: '1', ops: [{ op: 'set', tab: 1, row: 1, col: 1, text: 'x', at: [0, 0], value: 3 }] })), /unknown field value/);
});

test('the fake socket holds a presence range and the values of a write to their form without a server', () => {
  const ws = new FakeSocket(null);
  const range = (rows, cols = 'all') => JSON.stringify({ type: 'presence', tab: 1, range: { rows, cols } });
  const odd = (n) => Array.from({ length: n }, (_, i) => [2 * i + 1, 1]);
  assert.throws(() => ws.send(range([5])), /range\.rows\[0\] is a int, not a list/);
  assert.throws(() => ws.send(range([{ new: 1 }])), /range\.rows\[0\] is a object, not a list/);
  assert.throws(() => ws.send(range([[5, 1, 1]])), /a run that is not \[first, count\]/);
  assert.throws(() => ws.send(range([[5]])), /a run that is not \[first, count\]/);
  assert.throws(() => ws.send(range([[5, 0]])), /a run that names no ids/);
  assert.throws(() => ws.send(range([[0, 1]])), /a run that names no ids/);
  assert.throws(() => ws.send(range('all', 'some')), /neither all nor runs/);
  assert.throws(() => ws.send(range([[5, 10], [9, 3]])), /runs that overlap/);
  assert.throws(() => ws.send(range([[1, 100001]])), /more ids than a tab can hold/);
  assert.throws(() => ws.send(range('all', [[1, 703]])), /more ids than a tab can hold/);
  assert.throws(() => ws.send(range(odd(33), odd(32))), /more than 64 runs/);
  ws.send(range(odd(32), odd(32)));
  ws.send(range([[5, 4], [9, 3]], [[7, 1]]));
  ws.send(range([]));

  const write = (rows, cols, values) => JSON.stringify({ type: 'action', n: '1', base: '1', ops: [{ op: 'write', tab: 1, rows, cols, values }] });
  ws.send(write([[5, 2], 9, { new: 1 }], [[7, 2]], [['a', 'b'], ['c', 'd'], ['e', 'f'], ['g', 'h']]));
  assert.throws(() => ws.send(write([[5, 2]], [7], [['a']])), /values of another size/);
  assert.throws(() => ws.send(write([5], [[7, 2]], [['a']])), /values of another size/);
  assert.throws(() => ws.send(write('all', [[7, 2]], [['a']])), /values of another size/);
  // Only a server knows how many rows "all" is.
  ws.send(write('all', [7], [['a'], ['b']]));
});
