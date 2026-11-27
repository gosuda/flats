// What the client takes of the stream, and what it does when it cannot
// (TECH 12.2, 12.3 and 19): a delta as large as the limits of a sheet
// allow is applied, and neither the grid's sequence nor the stream's moves
// past a delta that was not. A frame the page fails to take is a frame
// that did not arrive: the grid is stale at once, the connection starts
// over without resuming, and a snapshot replaces the grid.
'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { Model } from '../model.js';
import { Net } from '../net.js';
import { decodeTSV } from '../clipboard.js';
import { SERVER_FRAME_MAX } from '../protocol.js';
import { FakeServer, Timers, deltaFrames, drawn, key, startApp, type } from './server.mjs';

const HUES = ['gray', 'red', 'orange', 'yellow', 'green', 'teal', 'blue', 'purple', 'magenta'];
const SHADES = [100, 200, 300, 400, 600, 800];
const letters = (n) => String.fromCharCode(65 + (n % 26), 65 + (Math.floor(n / 26) % 26), 65 + (Math.floor(n / 676) % 26));

// distinctFormats gives every cell of rows x cols a format of its own, the
// way a sheet can hold them: a currency symbol per row, a fill per column.
// bold says whether each is bold.
function distinctFormats(rows, cols, bold) {
  const out = [];
  for (let r = 0; r < rows; r++) {
    for (let c = 0; c < cols; c++) {
      const f = { fill: `${HUES[c % 9]}-${SHADES[Math.floor(c / 9) % 6]}`, number: { kind: 'currency', symbol: letters(r), decimals: c % 11 } };
      if (bold) f.bold = true;
      out.push(f);
    }
  }
  return out;
}

// sendInParts sends a delta the way the host sends a large one, in
// delta_part frames that each hold some of its fields, and returns how
// many frames it took.
function sendInParts(ws, d) {
  let parts = 0;
  for (const data of deltaFrames({ type: 'delta', ...d })) { parts++; ws.onmessage({ data }); }
  return parts;
}

// The text of a cell that the sheet's limits allow 1,600 of in one tab: an
// apostrophe, an equals sign and 30,000 U+0001. A cell frame carries it as
// its value and as its edit text, each control as six characters of JSON.
const VALUE = `=${'\u0001'.repeat(30000)}`;
const EDIT = `'${VALUE}`;

// escapedTab fills a column of the fake server's tab with that text.
function escapedTab(server, rows) {
  const tab = server.tab(1);
  for (const row of tab.rows.slice(0, rows)) tab.cells.set(`${row}:${tab.cols[0]}`, { kind: 't', value: VALUE, edit: EDIT });
  return tab;
}

// watchFrames records, for each frame the socket delivers, its type and
// the sequences of the stream and the grid after it.
function watchFrames(t) {
  const ws = t.server.socket;
  const deliver = ws.onmessage;
  const seen = [];
  ws.onmessage = (ev) => {
    try {
      deliver(ev);
    } finally {
      seen.push({ type: /^\{"type":"([a-z_]+)"/.exec(ev.data)[1], chars: ev.data.length, bytes: Buffer.byteLength(ev.data), stream: t.net.lastSeq, grid: t.model.seq });
    }
  };
  return seen;
}

const hostTab = (t) => JSON.stringify(t.rows.map((row) => [row, t.cols.map((col) => t.cells.get(`${row}:${col}`)?.value ?? null)]));
const gridTab = (model) => JSON.stringify(model.tab.rows.map((row) => [row, model.tab.cols.map((col) => model.cell(row, col)?.value ?? null)]));

// failOnce makes the grid fail to take the next formats it is given, as it
// does when it runs out of room: the delta itself is one the host may send.
function failOnce(model) {
  const styles = model.tab.styles;
  styles.push = () => { delete styles.push; throw new RangeError('no room for the formats'); };
  return () => { delete styles.push; };
}

test('a delta with 500,000 new formats applies, and the stream goes on through a reconnect and a new format list', async () => {
  const ROWS = 10000;
  const COLS = 50;
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Wide', rows: ROWS, cols: COLS }] });
  const t = await startApp({ server });
  const tab = server.tab(1);
  assert.equal(t.model.rowCount, ROWS);

  // Every cell has a format of its own, none bold. One edit makes all of
  // them bold: 500,000 formats the list has not held, in one delta.
  const plain = distinctFormats(ROWS, COLS, false);
  for (const f of plain) server.styles.push(f);
  tab.rows.forEach((row, r) => tab.cols.forEach((col, c) => tab.cells.set(`${row}:${col}`, { kind: '', style: 1 + r * COLS + c })));
  server.snapshot(server.socket, t.net.generation, 1);
  assert.equal(t.model.tab.styles.length, 1 + ROWS * COLS);

  const bold = distinctFormats(ROWS, COLS, true);
  const first = server.styles.length;
  for (const f of bold) server.styles.push(f);
  const cells = tab.rows.map((row, r) => [row, tab.cols.map((col, c) => {
    const cell = { kind: 't', value: 'x', style: first + r * COLS + c };
    tab.cells.set(`${row}:${col}`, cell);
    return [col, 't', 'x', null, null, cell.style];
  })]);
  const d = { seq: String(++server.seq), tab: 1, styles: bold, cells };
  server.log.push(d);
  const parts = sendInParts(server.socket, { seq: d.seq, styles: bold, cells });
  assert.ok(parts > 10, `the delta went out in ${parts} parts`);

  const { model, net } = t;
  assert.equal(model.tab.styles.length, 1 + 2 * ROWS * COLS, 'every new format is in the list');
  assert.equal(model.seq, d.seq);
  assert.equal(net.lastSeq, d.seq);
  assert.equal(model.stale, false);
  for (const [r, c] of [[0, 0], [1, 7], [ROWS - 1, COLS - 1]]) {
    const cell = model.cellAt(r, c);
    assert.equal(cell.shown, 'x');
    assert.deepEqual(model.format(cell), bold[r * COLS + c], `the format of row ${r}, column ${c}`);
  }
  assert.ok(drawn(t, 0, 1).classList.contains('b'), 'the cells on screen are drawn bold');
  assert.equal(t.app.canWrite(), true);

  // The connection is lost and resumes from the delta: nothing is skipped
  // and nothing loaded again, and the next change applies.
  server.socket.drop();
  t.timers.advance(1000);
  const hello = server.socket.open();
  assert.equal(hello.watermark, d.seq);
  assert.equal(server.socket.frames('watch').length, 0, 'the grid went on from the stream');
  server.setCell(1, 0, 0, 'after');
  assert.equal(model.shown(model.rowAt(0), model.colAt(0)), 'after');
  assert.equal(model.seq, String(server.seq));

  // The host rebuilds its format list (a new wire epoch): a resync and a
  // snapshot whose head carries the new list. The client's list is that
  // one, and a later delta adds to it.
  server.styles.length = 0;
  server.styles.push({}, { bold: true });
  for (const cell of tab.cells.values()) cell.style = 1;
  const generation = net.generation + 1;
  server.socket.recv({ type: 'resync', generation, seq: String(server.seq), date: server.date, tabs: server.tabInfo() });
  server.snapshot(server.socket, generation, 1);
  assert.deepEqual(model.tab.styles, [{}, { bold: true }]);
  assert.deepEqual(model.format(model.cellAt(ROWS - 1, COLS - 1)), { bold: true });
  server.styles.push({ italic: true });
  server.push({ tab: 1, styles: [{ italic: true }], cells: [[model.rowAt(2), [[model.colAt(2), 't', 'new', null, null, 2]]]] });
  assert.deepEqual(model.format(model.cellAt(2, 2)), { italic: true });
  assert.equal(model.tab.styles.length, 3);
  assert.equal(model.seq, String(server.seq));
  assert.equal(t.app.canWrite(), true);
});

test('the model takes 500,000 formats in one delta and keeps its sequence when a delta fails', () => {
  const m = new Model();
  m.setTabs([{ id: 1, name: 'T', rows: 2, cols: 2, frozen_rows: 0, frozen_cols: 0 }]);
  m.openHead({ generation: 1, tab: 1, seq: '10', rows: [[100, 2]], cols: [[200, 2]], widths: {}, frozen_rows: 0, frozen_cols: 0, styles: [{}] });
  m.done();
  const styles = Array.from({ length: 500000 }, (_, i) => ({ number: { kind: 'number', decimals: i % 11, thousands: true }, text_color: `red-${i}` }));
  m.apply({ seq: '11', styles, cells: [[100, [[200, 't', 'a', null, null, 500000]]]] });
  assert.equal(m.tab.styles.length, 500001);
  assert.equal(m.format(m.cell(100, 200)), styles[499999]);
  assert.equal(m.seq, '11');

  // A delta that fails half way: its first row is in the grid, its second is not.
  assert.throws(() => m.apply({ seq: '12', cells: [[100, [[201, 't', 'half']]], [101, [null]]] }), TypeError);
  assert.equal(m.seq, '11', 'the grid is not at a sequence it does not hold');
  assert.equal(m.stale, true, 'and it is no state the host had');
  // A stale grid takes nothing more until a snapshot replaces it.
  m.apply({ seq: '13', cells: [[101, [[200, 't', 'later']]]] });
  assert.equal(m.seq, '11');
  assert.equal(m.cell(101, 200), null);
  m.openHead({ generation: 2, tab: 1, seq: '13', rows: [[100, 2]], cols: [[200, 2]], widths: {}, frozen_rows: 0, frozen_cols: 0, styles: [{}] });
  assert.equal(m.stale, false);
  assert.equal(m.seq, '13');
});

test('a structural delta that meets a loading snapshot leaves the grid stale at its sequence', () => {
  const m = new Model();
  m.setTabs([{ id: 1, name: 'T', rows: 2, cols: 2, frozen_rows: 0, frozen_cols: 0 }]);
  m.openHead({ generation: 1, tab: 1, seq: '10', rows: [[100, 2]], cols: [[200, 2]], widths: {}, frozen_rows: 0, frozen_cols: 0, styles: [{}] });
  const out = m.apply({ seq: '11', styles: [{ bold: true }], struct: [{ op: 'insert_rows', tab: 1, ids: [[300, 1]], after: null }] });
  assert.equal(out.rewatch, true);
  assert.equal(m.seq, '10', 'the delta was not applied');
  assert.equal(m.stale, true);
  assert.equal(m.tab.styles.length, 1, 'no format is added to a list a head replaces');
});

test('the stream does not move past a delta its handler could not take, and the next connection resumes nothing', () => {
  const server = new FakeServer();
  const timers = new Timers();
  const taken = [];
  let fail = false;
  const net = new Net({ delta: (d) => { if (fail) throw new Error('not taken'); taken.push(d.seq); } }, { connect: () => server.connect(), timers, now: () => timers.now });
  net.start();
  server.socket.open();
  net.watch(1);
  server.setCell(1, 0, 0, 'one');
  assert.equal(net.lastSeq, '11');
  fail = true;
  assert.throws(() => server.setCell(1, 0, 0, 'two'), /not taken/);
  assert.notEqual(net.lastSeq, '12', 'the stream is not at a delta nobody holds');
  assert.equal(net.open, false, 'the connection ended');
  assert.equal(net.status, 'offline');
  fail = false;
  // Not at once: a frame that fails every time is asked for at the pace of a lost connection.
  assert.equal(server.sockets.length, 1);
  timers.advance(999);
  assert.equal(server.sockets.length, 1);
  timers.advance(1);
  assert.deepEqual(server.socket.open(), { type: 'hello', version: 1, session: 'tok1', pending: 0 }, 'no watermark: the host sends a snapshot');
  assert.equal(net.lastSeq, '12');
  server.setCell(1, 0, 0, 'three');
  assert.deepEqual(taken, ['11', '13']);
});

test('a handler that fails on a frame of a snapshot ends the connection the same way', () => {
  for (const failing of ['tabHead', 'rows', 'tabDone']) {
    const server = new FakeServer({ tabs: [{ id: 1, name: 'Sheet1', rows: 4, cols: 2, cells: { '0,0': 'a' } }] });
    const timers = new Timers();
    const net = new Net({ [failing]: () => { throw new Error(`no ${failing}`); } }, { connect: () => server.connect(), timers, now: () => timers.now });
    net.start();
    server.socket.open();
    assert.throws(() => net.watch(1), new RegExp(`no ${failing}`));
    assert.equal(net.open, false, failing);
    assert.equal(net.lastSeq, null, failing);
    timers.advance(1000);
    assert.equal('watermark' in server.socket.open(), false, failing);
  }
});

test('a delta the page fails to take moves neither sequence, and the page loads the tab again', async () => {
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 8, cols: 4, cells: { '0,0': 'Item', '1,0': 'Venue' } }] });
  const failed = [];
  const t = await startApp({ server, report: (err) => failed.push(err) });
  const { model, net } = t;
  // One action stays unanswered through the failure.
  server.manual.add('action');
  key(t, 'ArrowDown');
  key(t, 'ArrowRight');
  type(t, 'Kept');
  key(t, 'Enter');
  assert.equal(net.pending.length, 1);
  const before = model.seq;
  assert.equal(net.lastSeq, before);

  // The delta restyles two rows and changes a cell in each. The grid fails
  // on its formats, before any of its cells.
  server.styles.push({ bold: true });
  failOnce(model);
  const row = model.rowAt(3);
  const d = server.setCell(1, 3, 2, 'changed', { styles: [{ bold: true }] });
  assert.equal(failed.length, 1, 'the failure is reported once');
  assert.match(failed[0].message, /no room/);
  assert.equal(model.seq, before, 'the grid keeps the sequence of what it holds');
  assert.notEqual(net.lastSeq, d.seq, 'the stream is not past the delta');
  assert.equal(model.cell(row, model.colAt(2)), null);
  assert.equal(model.stale, true, 'the grid is marked at once');
  assert.equal(t.app.canWrite(), false, 'no edit starts on it');
  assert.equal(net.open, false);

  // The next connection resumes nothing and gets a snapshot.
  server.resume = 'replay';
  t.timers.advance(1000);
  const hello = server.socket.open();
  assert.equal('watermark' in hello, false, 'the hello asks for no replay');
  assert.equal(hello.pending, 1);
  assert.equal(model.stale, false);
  assert.equal(model.seq, String(server.seq));
  assert.equal(net.lastSeq, String(server.seq));
  assert.equal(gridTab(model), hostTab(server.tab(1)), 'the grid is the host\'s tab, with the change it had missed');
  assert.equal(model.shown(row, model.colAt(2)), 'changed');
  assert.equal(t.app.canWrite(), true);
  assert.equal(failed.length, 1);

  // The unanswered action is still there and applies once.
  assert.equal(net.pending.length, 1);
  server.manual.delete('action');
  server.socket.drop();
  t.timers.advance(1000);
  server.socket.open();
  assert.equal(net.pending.length, 0);
  assert.equal(server.applied.length, 1);
  assert.equal(gridTab(model), hostTab(server.tab(1)));
});

test('a delta that fails half way leaves no half of it on a grid that can be edited', async () => {
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 8, cols: 4, cells: { '0,0': 'Item' } }] });
  const failed = [];
  const t = await startApp({ server, report: (err) => failed.push(err) });
  const { model, net } = t;
  const before = model.seq;
  // The first row of the delta is read, the second cannot be: its cell's
  // kind is a number.
  const seq = String(++server.seq);
  server.socket.recv({ type: 'delta', seq, cells: [[model.rowAt(2), [[model.colAt(1), 't', 'half']]], [model.rowAt(3), [[model.colAt(1), 5]]]] });
  assert.equal(failed.length, 1);
  assert.equal(model.seq, before);
  assert.notEqual(net.lastSeq, seq);
  assert.equal(t.app.canWrite(), false, 'the half that was installed is on a grid nobody edits');
  t.timers.advance(1000);
  server.socket.open();
  assert.equal(gridTab(model), hostTab(server.tab(1)), 'the snapshot replaced the half');
  assert.equal(model.shown(model.rowAt(2), model.colAt(1)), '');
  assert.equal(t.app.canWrite(), true);
});

test('a delta the editor held and the page then fails to take ends the same way', async () => {
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 8, cols: 4, cells: { '0,0': 'Item' } }] });
  const failed = [];
  const t = await startApp({ server, report: (err) => failed.push(err) });
  const { model, net } = t;
  server.manual.add('rebase');
  t.sel.set(2, 2);
  t.app.changed();
  type(t, '=A1+');
  // A row is inserted above, with a new format: the delta is held.
  server.styles.push({ bold: true });
  const d = server.insertRows(1, 0, 1, { styles: [{ bold: true }] });
  assert.equal(model.seq, '10');
  assert.equal(net.lastSeq, d.seq, 'the stream holds the delta for the editor');
  failOnce(model);
  // Escape closes the editor, and what it held applies.
  key(t, 'Escape');
  assert.equal(failed.length, 1);
  assert.equal(t.editor.open, false);
  assert.equal(model.seq, '10', 'the grid is not at the held delta');
  assert.equal(model.stale, true);
  assert.equal(net.open, false);
  t.timers.advance(1000);
  assert.equal('watermark' in server.socket.open(), false, 'the hello does not resume behind the held delta');
  assert.equal(model.rowCount, 9);
  assert.equal(gridTab(model), hostTab(server.tab(1)));
  assert.equal(t.app.canWrite(), true);
});

test('a snapshot the page fails to take is asked for again on a new connection', async () => {
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 8, cols: 4, cells: { '0,0': 'Item', '1,0': 'Venue' } }, { id: 2, name: 'Other', rows: 3, cols: 2, cells: { '0,0': 'there' } }] });
  const failed = [];
  const t = await startApp({ server, report: (err) => failed.push(err) });
  const { model } = t;
  // The rows of the other tab cannot be read.
  server.manual.add('watch');
  t.app.openTab(2);
  const generation = t.net.generation;
  const frames = [];
  server.snapshot({ recv: (m) => frames.push(m) }, generation, 2);
  server.socket.recv(frames[0]);
  assert.equal(model.tab.id, 2);
  server.socket.recv({ type: 'rows', generation, tab: 2, part: 0, rows: [[model.rowAt(0), [[model.colAt(0), 5]]]] });
  assert.equal(failed.length, 1);
  assert.equal(model.stale, true);
  assert.equal(t.app.canWrite(), false);
  assert.equal(t.net.open, false);
  server.manual.delete('watch');
  t.timers.advance(1000);
  server.socket.open();
  assert.equal(model.tab.id, 2);
  assert.equal(gridTab(model), hostTab(server.tab(2)));
  assert.equal(t.app.canWrite(), true);
});

test('a pasted text of more rows than a call takes arguments is read', () => {
  const rows = decodeTSV('a\tb\n'.repeat(300000));
  assert.equal(rows.length, 300000);
  assert.deepEqual(rows[299999], ['a', 'b']);
});

test('a socket the connection has left delivers nothing more, and text that is no frame ends the connection', () => {
  const server = new FakeServer();
  const timers = new Timers();
  const taken = [];
  const net = new Net({ delta: (d) => taken.push(d.seq) }, { connect: () => server.connect(), timers, now: () => timers.now });
  net.start();
  const old = server.socket;
  old.open();
  net.watch(1);
  server.setCell(1, 0, 0, 'one');
  // A gap starts the connection over. The socket it left still holds a delta.
  old.recv({ type: 'delta', seq: '15' });
  assert.notEqual(server.socket, old);
  old.closed = false;
  old.recv({ type: 'delta', seq: '12' });
  assert.deepEqual(taken, ['11']);
  assert.equal(net.lastSeq, null, 'the stream took nothing from it');
  server.socket.open();
  assert.equal(net.lastSeq, '11');
  assert.throws(() => server.socket.onmessage({ data: '{"type":"delta","seq":"12"' }), SyntaxError);
  assert.equal(net.open, false);
  assert.equal(net.lastSeq, null);
  timers.advance(1000);
  assert.equal('watermark' in server.socket.open(), false);
});

test('valid JSON that is no frame object ends the connection as text that is no JSON does, and the snapshot it interrupts is not taken', async () => {
  // An object is a frame only with a text type.
  for (const text of ['null', '7', '[]', '"rows"', 'true', '{"type":"rows"', '{}', '{"type":null}', '{"type":2}', '{"rows":[]}']) {
    const server = new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 30, cols: 3, cells: { '0,0': 'a' } }, { id: 2, name: 'Other', rows: 30, cols: 3, cells: { '0,0': 'two', '20,1': 'end' } }] });
    const t = await startApp({ server });
    const { model, net } = t;
    const ws = server.socket;
    server.manual.add('watch');
    t.app.openTab(2);
    const frames = [];
    server.snapshot({ recv: (m) => frames.push(m) }, net.generation, 2, 1);
    assert.deepEqual(frames.map((f) => f.type), ['tab_head', 'rows', 'rows', 'tab_done'], text);
    ws.onmessage({ data: JSON.stringify(frames[0]) });
    ws.onmessage({ data: JSON.stringify(frames[1]) });
    assert.equal(model.tab.id, 2, text);
    // The text arrives in place of the second rows frame.
    assert.throws(() => ws.onmessage({ data: text }), text === '{"type":"rows"' ? SyntaxError : TypeError, text);
    assert.equal(net.open, false, `${text}: the connection ended`);
    assert.equal(net.lastSeq, null, `${text}: nothing is resumed`);
    assert.equal(model.stale, true, `${text}: the grid is behind at once`);
    assert.equal(t.app.canWrite(), false, text);
    // The end of the snapshot on the socket that was left is not taken.
    ws.onmessage({ data: JSON.stringify(frames[3]) });
    assert.equal(model.tab.loading, true, `${text}: the snapshot is not whole`);
    assert.equal(model.stale, true, text);
    assert.equal(t.app.canWrite(), false, text);
    // The next connection starts over and gets the tab whole.
    server.manual.delete('watch');
    t.timers.advance(1000);
    assert.equal('watermark' in server.socket.open(), false, text);
    assert.deepEqual([model.tab.id, model.stale, model.tab.loading], [2, false, false], text);
    assert.equal(model.shown(model.rowAt(20), model.colAt(1)), 'end', text);
    assert.equal(t.app.canWrite(), true, text);
  }
});

test('structural deltas over as many rows as a tab holds apply', () => {
  const ROWS = 100000;
  const m = new Model();
  m.setTabs([{ id: 1, name: 'T', rows: 1, cols: 1, frozen_rows: 0, frozen_cols: 0 }]);
  m.openHead({ generation: 1, tab: 1, seq: '10', rows: [[1, 1]], cols: [[5, 1]], widths: {}, frozen_rows: 0, frozen_cols: 0, styles: [{}] });
  m.done();
  // The inserted ids as runs of one, the longest form the list has.
  const ids = Array.from({ length: ROWS - 1 }, (_, i) => [10 + 2 * i, 1]);
  m.apply({ seq: '11', struct: [{ op: 'insert_rows', tab: 1, ids, after: 1 }] });
  assert.equal(m.rowCount, ROWS);
  assert.equal(m.rowAt(ROWS - 1), 10 + 2 * (ROWS - 2));
  const order = m.tab.rows.slice().reverse();
  m.apply({ seq: '12', struct: [{ op: 'sort', tab: 1, positions: [[0, ROWS]], ids: order }] });
  assert.equal(m.rowAt(ROWS - 1), 1);
  assert.equal(m.rowIndex(1), ROWS - 1);
  m.apply({ seq: '13', struct: [{ op: 'delete_rows', tab: 1, ids }] });
  assert.deepEqual(m.tab.rows, [1]);
  assert.equal(m.seq, '13');
});

test('a whole-range format delta longer than one string can hold applies, and the sequence moves with its last part', async () => {
  const ROWS = 1600;
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Escaped', rows: ROWS, cols: 1 }] });
  const t = await startApp({ server });
  const { model, net } = t;
  const tab = escapedTab(server, ROWS);
  // The snapshot of the tab arrives in rows frames inside the frame limit.
  const seen = watchFrames(t);
  server.snapshot(server.socket, net.generation, 1, 1);
  assert.ok(seen.reduce((n, f) => n + f.chars, 0) > 2 ** 29, 'the snapshot holds more characters than one string can');
  assert.ok(seen.every((f) => f.bytes <= SERVER_FRAME_MAX));
  assert.equal(model.cellAt(ROWS - 1, 0).value, VALUE);
  const before = model.seq;

  // Bold over the whole range: every cell again, with a new format.
  seen.length = 0;
  server.styles.push({ bold: true });
  const style = server.styles.length - 1;
  const cells = tab.rows.map((row) => {
    const cell = tab.cells.get(`${row}:${tab.cols[0]}`);
    cell.style = style;
    return [row, [[tab.cols[0], 't', VALUE, null, EDIT, style]]];
  });
  const d = server.push({ tab: 1, styles: [{ bold: true }], cells });
  const parts = seen.filter((f) => f.type === 'delta_part');
  assert.equal(parts.length, seen.length);
  assert.ok(parts.length > 1000, `${parts.length} parts`);
  assert.ok(seen.reduce((n, f) => n + f.chars, 0) > 2 ** 29, 'the delta holds more characters than one string can');
  assert.ok(seen.every((f) => f.bytes <= SERVER_FRAME_MAX), 'every part is inside the server frame limit');
  assert.ok(parts.slice(0, -1).every((f) => f.stream === before && f.grid === before), 'neither sequence moves before the last part');
  assert.equal(parts.at(-1).stream, d.seq);
  assert.equal(parts.at(-1).grid, d.seq);
  assert.equal(model.stale, false);
  for (const r of [0, 800, ROWS - 1]) {
    const cell = model.cellAt(r, 0);
    assert.equal(cell.value, VALUE, `row ${r}`);
    assert.equal(cell.edit, EDIT, `row ${r}`);
    assert.equal(model.format(cell).bold, true, `row ${r}`);
  }
  assert.ok(drawn(t, 0, 0).classList.contains('b'), 'the cells on screen are drawn bold');
  assert.equal(t.app.canWrite(), true);
  // The stream goes on from it.
  server.setCell(1, 0, 0, 'after');
  assert.equal(model.shown(model.rowAt(0), model.colAt(0)), 'after');
  assert.equal(net.lastSeq, String(server.seq));
});

test('a delta in parts that breaks off or fails half way moves neither sequence, and a snapshot replaces the grid', async () => {
  const ROWS = 40;
  for (const how of ['a part that is no JSON', 'a cell the grid cannot take']) {
    const server = new FakeServer({ tabs: [{ id: 1, name: 'Escaped', rows: ROWS, cols: 1 }] });
    const failed = [];
    const t = await startApp({ server, report: (err) => failed.push(err) });
    const { model, net } = t;
    const tab = escapedTab(server, ROWS);
    server.snapshot(server.socket, net.generation, 1, 1);
    const before = model.seq;
    server.styles.push({ bold: true });
    const style = server.styles.length - 1;
    const cells = tab.rows.map((row, r) => {
      tab.cells.get(`${row}:${tab.cols[0]}`).style = style;
      // A cell whose kind is a number, half way through the delta.
      return [row, [r === ROWS / 2 && how !== 'a part that is no JSON' ? [tab.cols[0], 5] : [tab.cols[0], 't', VALUE, null, EDIT, style]]];
    });
    const seq = String(++server.seq);
    server.log.push({ seq, tab: 1, styles: [{ bold: true }], cells });
    const frames = [...deltaFrames({ type: 'delta', seq, styles: [{ bold: true }], cells })];
    assert.ok(frames.length > 10, how);
    const cut = frames.length >> 1;
    for (let i = 0; i < frames.length && net.open; i++) {
      const data = how === 'a part that is no JSON' && i === cut ? frames[i].slice(0, -1) : frames[i];
      if (how === 'a part that is no JSON' && i === cut) {
        assert.throws(() => server.socket.onmessage({ data }), SyntaxError, how);
        continue;
      }
      server.socket.onmessage({ data });
      if (net.open) assert.equal(net.lastSeq, before, `${how}: part ${i}`);
    }
    assert.equal(net.open, false, how);
    assert.equal(model.seq, before, `${how}: the grid keeps its sequence`);
    assert.notEqual(net.lastSeq, seq, `${how}: the stream is not past the delta`);
    if (how === 'a cell the grid cannot take') {
      assert.equal(failed.length, 1, how);
      assert.equal(model.stale, true, `${how}: the half that was installed is on a stale grid`);
      assert.equal(t.app.canWrite(), false, how);
    } else {
      assert.equal(model.cellAt(0, 0).style, 0, `${how}: no part of the delta reached the grid`);
    }
    // The next connection resumes nothing and gets a snapshot of the host's tab.
    tab.cells.get(`${tab.rows[ROWS / 2]}:${tab.cols[0]}`).style = style;
    t.timers.advance(1000);
    assert.equal('watermark' in server.socket.open(), false, how);
    assert.equal(model.stale, false, how);
    assert.equal(model.seq, seq, how);
    assert.equal(model.format(model.cellAt(ROWS / 2, 0)).bold, true, how);
    assert.equal(t.app.canWrite(), true, how);
  }
});
