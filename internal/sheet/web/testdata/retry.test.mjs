// "Retry now" and "Ask now" start a connection while the automatic retry
// may have one that is still opening, or that has said hello and waits
// for its welcome. The page then goes on with the newer socket alone. The
// one it left is closed and says nothing, in whichever order the two
// open, and the unconfirmed edit reaches the host once.
'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { FakeServer, find, key, shownAt, startApp, type } from './server.mjs';

const sheet = () => new FakeServer({ tabs: [{ id: 1, name: 'Budget', rows: 30, cols: 6, cells: { '0,0': 'Item', '1,0': 'Venue', '2,0': 'Food' } }] });
const status = (t) => t.app.status.el.lastChild.textContent;
const live = (t) => t.server.sockets.filter((ws) => !ws.closed);

// offline leaves a page that lost its connection with one unconfirmed
// edit. The automatic retry has started a socket (earlier), and the
// person pressed "Retry now" before it opened (later).
async function offline() {
  const t = await startApp({ server: sheet() });
  t.server.socket.drop();
  key(t, 'ArrowDown');
  type(t, 'Hall');
  key(t, 'Enter');
  assert.equal(t.net.pending.length, 1);
  t.timers.advance(1000);
  const earlier = t.server.socket;
  assert.deepEqual(live(t), [earlier], 'the automatic retry has a socket that is opening');
  find(t.app.banners, 'Retry now').click();
  const later = t.server.socket;
  assert.notEqual(later, earlier);
  return { ...t, earlier, later };
}

// busy leaves a page whose edit the host answered "unavailable" four
// times. The automatic retry has started a socket (earlier), and the
// person pressed "Ask now" before it opened (later).
async function busy() {
  const t = await startApp({ server: sheet() });
  t.server.unavailable = 4;
  key(t, 'ArrowDown');
  type(t, 'Hall');
  key(t, 'Enter');
  for (const wait of [1000, 2000, 4000]) { t.timers.advance(wait); t.server.socket.open(); }
  assert.equal(t.server.busyActions.length, 4);
  assert.match(t.app.banners.textContent, /The host has been busy for a while/);
  t.timers.advance(8000);
  const earlier = t.server.socket;
  assert.deepEqual(live(t), [earlier], 'the automatic retry has a socket that is opening');
  find(t.app.banners, 'Ask now').click();
  const later = t.server.socket;
  assert.notEqual(later, earlier);
  return { ...t, earlier, later };
}

// resumed reads the hello of the connection the page goes on with: it
// names the session, the place of the stream and the unconfirmed edit.
function resumed(hello) {
  assert.deepEqual([hello.type, hello.session, hello.watermark, hello.tab, hello.pending], ['hello', 'tok1', '10', 1, 1]);
}

// saved checks the end of every case. One socket is left, which carried
// one hello and the edit once. The host applied the edit once, editing
// did not stop, and the next edit goes out on the same socket.
function saved(t, current) {
  assert.equal(t.net.stopped, null, 'editing goes on');
  assert.deepEqual(live(t), [current], 'one socket is left');
  assert.equal(current.frames('hello').length, 1);
  assert.deepEqual(current.frames('action').map((a) => [a.n, a.ops[0].text]), [['1', 'Hall']]);
  assert.deepEqual(t.server.applied, [['s1', '1']], 'the edit applied once');
  assert.equal(t.server.cell(1, 1, 0).value, 'Hall');
  assert.equal(shownAt(t, 1, 0), 'Hall');
  assert.equal(t.net.pending.length, 0);
  assert.equal(status(t), 'Saved');
  assert.equal(t.app.banners.children.length, 0);
  type(t, 'Bus');
  key(t, 'Enter');
  assert.equal(t.server.cell(1, 2, 0).value, 'Bus');
  assert.deepEqual(t.server.applied.map(([, n]) => n), ['1', '2']);
  assert.equal(current.frames('hello').length, 1);
}

// The tests play the opening of the socket that was left as well. Nothing
// may reach the host through it, or through the socket that replaced it.
for (const [button, start] of [['Retry now', offline], ['Ask now', busy]]) {
  test(`${button} during an attempt: the newer socket opens first`, async () => {
    const t = await start();
    resumed(t.later.open());
    assert.equal(t.earlier.open(), undefined, 'the socket that was left says no hello');
    assert.deepEqual(t.earlier.sent, []);
    assert.equal(t.earlier.closed, true, 'the attempt that was under way is closed');
    saved(t, t.later);
  });

  test(`${button} during an attempt: the earlier socket opens first`, async () => {
    const t = await start();
    assert.equal(t.earlier.open(), undefined, 'the socket that was left says no hello');
    assert.deepEqual(t.later.sent, [], 'and nothing goes out on the one that is still opening');
    assert.equal(t.net.open, false);
    resumed(t.later.open());
    assert.deepEqual(t.earlier.sent, []);
    assert.equal(t.earlier.closed, true, 'the attempt that was under way is closed');
    saved(t, t.later);
  });

  test(`${button} during an attempt: the earlier socket opens while the welcome is on its way`, async () => {
    const t = await start();
    t.server.slowWelcome = true;
    resumed(t.later.open());
    assert.equal(t.later.queue.length, 1, 'the welcome is on its way');
    assert.equal(t.net.open, false);
    assert.equal(t.earlier.open(), undefined, 'the socket that was left says no hello');
    assert.deepEqual(t.earlier.sent, []);
    assert.equal(t.earlier.closed, true, 'the attempt that was under way is closed');
    t.server.slowWelcome = false;
    t.server.deliver(Infinity, t.later);
    saved(t, t.later);
  });
}

test('Retry now while a welcome is on its way leaves that socket, and its welcome is not taken', async () => {
  const t = await offline();
  t.server.slowWelcome = true;
  const waiting = t.later;
  resumed(waiting.open());
  assert.equal(t.net.open, false);
  // No welcome came yet, so the banner still offers the button.
  find(t.app.banners, 'Retry now').click();
  const last = t.server.socket;
  assert.notEqual(last, waiting);
  assert.equal(waiting.closed, true, 'the socket that waited is closed');
  assert.deepEqual(live(t), [last]);
  t.server.slowWelcome = false;
  // What the socket had received before it was closed still arrives.
  waiting.onmessage({ data: JSON.stringify(waiting.queue.shift()) });
  assert.equal(t.net.open, false, 'the welcome of a socket that was left is not taken');
  assert.deepEqual(waiting.frames('action'), [], 'and no edit goes out on it');
  resumed(last.open());
  assert.deepEqual(t.server.sockets.map((ws) => ws.frames('hello').length), [1, 0, 1, 1], 'each socket that opened said hello once, on itself');
  saved(t, last);
});
