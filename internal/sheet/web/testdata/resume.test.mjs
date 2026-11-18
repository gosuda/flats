// A resumed stream (TECH 12.5): the welcome names the watermark and the
// replayed deltas follow, so the page keeps the tab list and the date of
// its grid until those deltas change them. A formula typed in between is
// read against the tab names on screen, which are those of its base.
'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { FakeServer, key, startApp, type } from './server.mjs';

const twoTabs = () => new FakeServer({ tabs: [{ id: 1, name: 'Alpha', rows: 6, cols: 3, cells: { '0,0': '111' } }, { id: 2, name: 'Beta', rows: 6, cols: 3, cells: { '0,0': '222' } }] });

test('a replayed welcome keeps the tab list and the date of the grid, and the replayed deltas change them in order', async () => {
  const t = await startApp({ server: twoTabs() });
  const { server, model } = t;
  server.socket.drop();
  // While the page is away the two tabs swap their names and the date moves.
  const renames = [server.renameTab(1, 'Temp'), server.renameTab(2, 'Alpha'), server.renameTab(1, 'Beta')];
  const day = server.push({ date: '2026-11-14' });
  // The replay is late: the welcome comes first, on its own.
  server.manual.add('hello');
  t.timers.advance(1000);
  const ws = server.socket;
  const hello = ws.open();
  assert.deepEqual([hello.watermark, hello.tab], ['10', 1]);
  ws.session = server.sessions.get('tok1');
  ws.recv({ type: 'welcome', access: 'private', session: 'tok1', session_id: 's1', me: server.me, seq: '10' });
  assert.deepEqual(model.tabs.map((x) => [x.id, x.name]), [[1, 'Alpha'], [2, 'Beta']], 'the names are those of sequence 10');
  assert.deepEqual([model.date, model.seq, model.stale, t.app.canWrite()], ['2026-11-13', '10', false, true]);
  assert.deepEqual(t.app.tabs.el.querySelectorAll('[role="tab"]').map((el) => el.textContent), ['Alpha', 'Beta'], 'and the tab bar shows them');

  // A formula typed now names the tab the screen calls Beta, tab 2, and
  // goes out with base 10, where Beta is that tab.
  server.manual.add('action');
  t.sel.set(0, 1);
  type(t, '=Beta!A1');
  key(t, 'Enter');
  const [action] = ws.frames('action');
  assert.deepEqual([action.base, action.ops[0].text], ['10', '=Beta!A1']);

  // The replay brings the renames and the date in their places.
  ws.recv({ type: 'delta', seq: renames[0].seq, struct: renames[0].struct, tabs: renames[0].tabs });
  assert.deepEqual(model.tabs.map((x) => x.name), ['Temp', 'Beta']);
  for (const d of renames.slice(1)) ws.recv({ type: 'delta', seq: d.seq, struct: d.struct, tabs: d.tabs });
  ws.recv({ type: 'delta', seq: day.seq, date: day.date });
  assert.deepEqual(model.tabs.map((x) => [x.id, x.name]), [[1, 'Beta'], [2, 'Alpha']]);
  assert.deepEqual([model.date, model.seq, t.net.lastSeq], ['2026-11-14', day.seq, day.seq]);
});

test('a welcome of a resume the host could not replay brings the tab list and the date of its own sequence', async () => {
  const t = await startApp({ server: twoTabs() });
  const { server, model } = t;
  server.socket.drop();
  server.renameTab(1, 'Renamed');
  server.date = '2026-11-14';
  server.resume = 'snapshot';
  t.timers.advance(1000);
  server.socket.open();
  assert.deepEqual([model.tabs[0].name, model.date, model.seq], ['Renamed', '2026-11-14', String(server.seq)]);
});
