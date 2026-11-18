// The grid and the stream (app.js #grid). The stream is what the connection
// has received; the grid is the live tab as of model.seq. A grid can fall
// behind the stream for good (stale), and then only the head of a new
// snapshot brings it back. This file walks through what can interrupt that
// snapshot: every cause of a stale grid, a connection lost at every point
// of the snapshot's arrival, up to three times in a row, with a host that
// can replay the stream on the next connection or cannot. A delta the page
// fails to take is one of the causes and one of the events.
//
// At every step:
//   - when an edit can start, the grid is the host's tab as of model.seq
//     (the guard never lifts over a stale grid);
// and at the end, with no more interruptions:
//   - the page shows the host's current tab and can be edited, without the
//     person doing anything and without waiting for the page to ask again;
//   - the action that was unanswered all the while applied exactly once.
'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { FakeServer, key, startApp, type } from './server.mjs';

const HEAD_WAIT = 15000;

// tabState writes a tab as rows in order with their cells.
const hostTab = (t) => JSON.stringify(t.rows.map((row) => [row, t.cols.map((col) => t.cells.get(`${row}:${col}`)?.value ?? null)]));
const gridTab = (model) => JSON.stringify(model.tab.rows.map((row) => [row, model.tab.cols.map((col) => model.cell(row, col)?.value ?? null)]));

async function begin() {
  const server = new FakeServer({ tabs: [{ id: 1, name: 'Data', rows: 12, cols: 4, cells: { '0,0': 'Item', '1,0': 'Venue', '2,1': '7' } }, { id: 2, name: 'Other', rows: 5, cols: 3 }] });
  // Rows in two runs and one run a head frame, so the head arrives in parts.
  server.tab(1).rows.splice(5, 1);
  server.headRuns = 1;
  // A delta the page fails to take is reported and not thrown (failed).
  const failed = [];
  const t = await startApp({ server, report: (err) => failed.push(err) });
  // What the host's tab held at each sequence.
  const history = new Map([[String(server.seq), hostTab(server.tab(1))]]);
  const push = server.push.bind(server);
  server.push = (fields) => { const d = push(fields); history.set(d.seq, hostTab(server.tab(1))); return d; };
  let n = 0;
  // change is another writer's edit: a cell, and now and then a row.
  const change = () => {
    n++;
    if (n % 3 === 0) server.insertRows(1, 1); else server.setCell(1, 3 + (n % 4), 2, `w${n}`);
  };
  // check holds the invariant of every step.
  const check = (where) => {
    const { model, app } = t;
    if (!app.canWrite()) return;
    assert.equal(model.stale, false, `${where}: an edit can start on a stale grid`);
    const then = history.get(model.seq);
    assert.ok(then != null, `${where}: the grid names sequence ${model.seq}, which the host never had`);
    if (model.tab.loading) assert.equal(JSON.stringify(model.tab.rows), JSON.stringify(JSON.parse(then).map(([row]) => row)), `${where}: the rows of a loading grid are those of sequence ${model.seq}`);
    else assert.equal(gridTab(model), then, `${where}: an edit can start, but the grid is not the host's tab of sequence ${model.seq}`);
  };
  // One action stays unanswered through everything that follows.
  server.manual.add('action');
  key(t, 'ArrowDown');
  key(t, 'ArrowRight');
  const kept = [t.model.rowAt(1), t.model.colAt(1)];
  type(t, 'Kept');
  key(t, 'Enter');
  assert.equal(t.net.pending.length, 1);
  server.paced = true;
  const reconnect = (mode) => {
    server.resume = mode;
    t.timers.advance(1000);
    server.socket.open();
  };
  // unread is another writer's change that the grid fails to take when it
  // reaches the grid at once: the formats of the delta find no room. The
  // connection then ends, and the next one opens after its wait. A stale
  // grid takes no delta, and a paced one gets it later, when it fits.
  const unread = () => {
    const styles = t.model.tab.styles;
    styles.push = () => { throw new RangeError('no room for the formats'); };
    server.styles.push({ wrap: true });
    const before = failed.length;
    server.setCell(1, 4, 3, `u${++n}`, { styles: [{ wrap: true }] });
    delete styles.push;
    if (failed.length === before) return;
    assert.equal(t.model.stale, true, 'a grid that failed to take a delta is stale at once');
    assert.equal(t.net.open, false);
    reconnect('replay');
  };
  return { ...t, change, check, reconnect, kept, unread, failed };
}

// What makes the grid stale. Each leaves a snapshot on its way.
const CAUSES = {
  'a resync': (t) => {
    // The connection lagged: three changes never arrived.
    const ws = t.server.socket;
    const recv = ws.recv;
    ws.recv = () => {};
    for (let i = 0; i < 3; i++) t.change();
    ws.recv = recv;
    const generation = t.net.generation + 1;
    ws.recv({ type: 'resync', generation, seq: String(t.server.seq), date: t.server.date, tabs: t.server.tabInfo() });
    t.server.snapshot(ws, generation, 1);
  },
  'a resume the host cannot replay': (t) => {
    t.server.socket.drop();
    t.change();
    t.change();
    t.reconnect('snapshot');
  },
  'a connection that starts over after a gap': (t) => {
    t.change();
    t.server.socket.recv({ type: 'delta', seq: String(t.server.seq + 7) });
    t.change();
    t.server.socket.open();
  },
  'a rollback': (t) => {
    t.change();
    t.server.socket.recv({ type: 'reset', seq: String(t.server.seq) });
    t.change();
    t.server.socket.open();
  },
  'a delta the page fails to take': (t) => {
    t.change();
    t.unread();
    assert.equal(t.failed.length, 1, 'the delta reached a current grid and failed');
    t.change();
  },
};

// How far the snapshot got when the connection was lost.
const POINTS = {
  'before its head': () => {},
  'while the host waits for room to take it': (t) => t.server.socket.recv({ type: 'status', calculating: true }),
  'in the middle of its head': (t) => t.server.deliver(1),
  'after its head': (t) => { const q = t.server.socket.queue; while (q.length && (q[0].type === 'tab_head' || q[0].type === 'tab_more')) t.server.deliver(1); },
  'before its last frame': (t) => { const q = t.server.socket.queue; while (q.length && q[0].type !== 'tab_done') t.server.deliver(1); },
};
const MODES = ['replay', 'snapshot'];

async function scenario(cause, steps) {
  const t = await begin();
  const name = `${cause}; ${steps.map(([point, mode]) => `lost ${point}, then ${mode}`).join('; ')}`;
  CAUSES[cause](t);
  t.check(`${name} [after the cause]`);
  for (const [i, [point, mode]] of steps.entries()) {
    POINTS[point](t);
    t.check(`${name} [step ${i + 1}, ${point}]`);
    t.change();
    t.check(`${name} [step ${i + 1}, after a change]`);
    t.server.socket.drop();
    t.change();
    t.check(`${name} [step ${i + 1}, disconnected]`);
    t.reconnect(mode);
    t.check(`${name} [step ${i + 1}, reconnected]`);
  }
  // No more interruptions: everything the host sent arrives.
  t.server.paced = false;
  t.server.deliver();
  t.check(`${name} [settled]`);
  assert.equal(t.app.canWrite(), true, `${name}: the page is read-only after everything arrived (grid ${t.model.stale ? 'stale' : t.model.tab ? 'shown' : 'none'}, calculating ${t.app.calculating})`);
  assert.equal(t.model.tab.loading, false, name);
  assert.equal(t.model.seq, String(t.server.seq), `${name}: the grid is at the host's sequence`);
  assert.equal(gridTab(t.model), hostTab(t.server.tab(1)), `${name}: the grid is the host's tab`);
  // The unanswered action is still there and applies once.
  assert.equal(t.net.pending.length, 1, `${name}: the pending action was lost`);
  assert.equal(t.model.shown(...t.kept), 'Kept', `${name}: its text is still on screen`);
  t.server.manual.delete('action');
  t.server.socket.drop();
  t.reconnect('replay');
  t.server.deliver();
  assert.equal(t.net.pending.length, 0, name);
  assert.equal(t.server.applied.length, 1, `${name}: the action applied ${t.server.applied.length} times`);
  assert.equal(t.app.canWrite(), true, name);
  assert.equal(gridTab(t.model), hostTab(t.server.tab(1)), name);
  return t;
}

// sequences lists every run of up to depth losses.
function sequences(depth) {
  const one = Object.keys(POINTS).flatMap((point) => MODES.map((mode) => [point, mode]));
  let out = one.map((s) => [s]);
  let last = out;
  for (let d = 1; d < depth; d++) {
    last = last.flatMap((seq) => one.map((s) => [...seq, s]));
    out = out.concat(last);
  }
  return out;
}

for (const cause of Object.keys(CAUSES)) {
  test(`${cause}: the connection lost once or twice at every point of the snapshot`, async () => {
    for (const steps of sequences(2)) await scenario(cause, steps);
  });
}

test('the connection lost three times in a row at every point of the snapshot', async () => {
  // A resync leaves the watermark ahead of the grid on an open connection,
  // which is where a resumed hello could mislead: every run of three.
  const three = sequences(3).filter((s) => s.length === 3);
  for (const steps of three) await scenario('a resync', steps);
  // For a resume the host cannot replay: every third loss after two
  // losses that came before the head.
  const early = new Set(['before its head', 'while the host waits for room to take it']);
  for (const steps of three.filter((s) => early.has(s[0][0]) && early.has(s[1][0]))) await scenario('a resume the host cannot replay', steps);
});

// The states a grid can be in when something arrives, and how to get there.
const STATES = {
  'current': () => {},
  'stale, snapshot announced': (t) => CAUSES['a resync'](t),
  'stale, after a welcome that was not replayed': (t) => CAUSES['a resume the host cannot replay'](t),
  'stale, tab asked for': (t) => CAUSES['a connection that starts over after a gap'](t),
  'loading': (t) => { CAUSES['a resync'](t); POINTS['after its head'](t); },
  'loading, rows here': (t) => { CAUSES['a resync'](t); POINTS['before its last frame'](t); },
};

// Everything that can arrive, or happen to the connection, in each of them.
const EVENTS = {
  'a resync': (t) => CAUSES['a resync'](t),
  'a rollback': (t) => CAUSES['a rollback'](t),
  'a gap in the stream': (t) => CAUSES['a connection that starts over after a gap'](t),
  'a delta with a cell': (t) => t.server.setCell(1, 2, 2, 'cell'),
  'a delta that inserts a row': (t) => t.server.insertRows(1, 0),
  'a delta the page fails to take': (t) => t.unread(),
  'a delta that renames another tab': (t) => { if (t.server.tab(2)) t.server.renameTab(2, 'Renamed'); },
  'a delta that deletes another tab': (t) => { if (t.server.tab(2)) t.server.deleteTab(2); },
  'status: calculating, then not': (t) => { t.server.socket.recv({ type: 'status', calculating: true }); t.server.socket.recv({ type: 'status', calculating: false }); },
  // Generations start at 0, and below that there is no earlier one.
  'a head of an earlier generation': (t) => { if (t.net.generation < 1) return; const old = []; t.server.snapshot({ recv: (m) => old.push(m) }, t.net.generation - 1, 1); t.server.socket.recv(old[0]); },
  'rows and the end of an earlier generation': (t) => { if (t.net.generation < 1) return; const old = []; t.server.snapshot({ recv: (m) => old.push(m) }, t.net.generation - 1, 1); for (const m of old.filter((x) => x.type === 'rows' || x.type === 'tab_done')) t.server.socket.recv(m); },
  // A tab_more that continues no head the page has belongs to a snapshot
  // that would never end, so the connection starts over.
  'a tab_more without its head': (t) => {
    try {
      t.server.socket.recv({ type: 'tab_more', generation: t.net.generation, tab: 1, styles: [{}] });
    } catch (err) {
      if (!(err instanceof TypeError)) throw err;
      assert.deepEqual([t.model.stale, t.net.open], [true, false], 'the grid is stale and the connection ended at once');
      t.reconnect('replay');
    }
  },
  'the connection lost, then replayed': (t) => { t.server.socket.drop(); t.change(); t.reconnect('replay'); },
  'the connection lost, then not replayed': (t) => { t.server.socket.drop(); t.change(); t.reconnect('snapshot'); },
  'the connection lost twice before a welcome': (t) => { t.server.socket.drop(); t.timers.advance(1000); t.server.socket.drop(); t.change(); t.timers.advance(2000); t.server.socket.open(); },
};

test('every event in every state of the grid ends with the host\'s tab on screen, editable', async () => {
  // How often a delta failed on the grid: it reaches a grid at once only
  // when no snapshot waits in the queue, and must do so in some run.
  let failures = 0;
  for (const [state, enter] of Object.entries(STATES)) {
    for (const [event, happen] of Object.entries(EVENTS)) {
      for (const [second, again] of [['', null], ...Object.entries(EVENTS)]) {
        // Pairs of events only from the stale and loading states, where one
        // event leaves something for the next to meet.
        if (again && state === 'current') continue;
        const t = await begin();
        const name = `${state}; ${event}${second ? `; ${second}` : ''}`;
        enter(t);
        t.check(`${name} [state]`);
        happen(t);
        t.check(`${name} [event]`);
        if (again) { again(t); t.check(`${name} [second event]`); }
        t.server.paced = false;
        t.server.deliver();
        t.check(`${name} [settled]`);
        assert.equal(t.app.canWrite(), true, `${name}: the page is read-only after everything arrived (stale ${t.model.stale}, calculating ${t.app.calculating}, switching ${t.app.switching})`);
        assert.equal(t.model.tab.loading, false, name);
        assert.equal(t.model.seq, String(t.server.seq), `${name}: the grid is at the host's sequence`);
        assert.equal(gridTab(t.model), hostTab(t.server.tab(1)), `${name}: the grid is the host's tab`);
        assert.equal(t.net.pending.length, 1, `${name}: the pending action was lost`);
        failures += t.failed.length;
      }
    }
  }
  assert.ok(failures > 0, 'no delta failed on a grid in any run');
});

test('the grid waits for a head that does not come, and the page asks for the tab again', async () => {
  const t = await begin();
  CAUSES['a resync'](t);
  // The host drops the snapshot it announced and sends nothing.
  t.server.socket.queue.length = 0;
  t.server.paced = false;
  const watches = t.server.socket.frames('watch').length;
  assert.equal(t.app.canWrite(), false);
  t.timers.advance(HEAD_WAIT - 1);
  assert.equal(t.server.socket.frames('watch').length, watches, 'not before the wait is over');
  t.timers.advance(1);
  assert.equal(t.server.socket.frames('watch').length, watches + 1);
  assert.equal(t.app.canWrite(), true);
  assert.equal(gridTab(t.model), hostTab(t.server.tab(1)));
  // and nothing more is asked once the grid is current
  t.timers.advance(10 * HEAD_WAIT);
  assert.equal(t.server.socket.frames('watch').length, watches + 1);
});

test('a snapshot older than the stream does not lift the guard: the tab is asked for again', async () => {
  const t = await begin();
  t.server.paced = false;
  // The host sends a head taken at sequence 10 after the client received 11 and 12.
  const old = [];
  t.server.snapshot({ recv: (m) => old.push(m) }, t.net.generation, 1);
  t.change();
  t.change();
  t.server.manual.add('watch');
  for (const m of old) t.server.socket.recv(m);
  assert.equal(t.model.stale, true, 'the grid of sequence 10 cannot take the changes that went by');
  assert.equal(t.app.canWrite(), false);
  assert.equal(t.server.socket.frames('watch').at(-1).tab, 1);
  t.server.manual.delete('watch');
  t.server.snapshot(t.server.socket, t.net.generation, 1);
  t.check('after the new snapshot');
  assert.equal(t.app.canWrite(), true);
  assert.equal(gridTab(t.model), hostTab(t.server.tab(1)));
});

test('a tab the person asked for that is deleted before it arrives leaves the page on its tab, editable', async () => {
  const t = await begin();
  t.server.paced = false;
  t.server.manual.add('watch');
  t.app.openTab(2);
  assert.equal(t.app.canWrite(), false, 'the person left this tab');
  t.server.manual.delete('watch');
  t.server.deleteTab(2);
  assert.equal(t.model.tab.id, 1);
  assert.equal(t.app.canWrite(), true);
  assert.equal(t.server.socket.frames('watch').at(-1).tab, 1);
});
