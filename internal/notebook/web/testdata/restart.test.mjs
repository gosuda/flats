'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { startApp, FakeSocket, page, block, para, caretAt } from './page.mjs';
import { fire } from './dom.mjs';

const content = { pages: [page('a')], blocks: [block('x', 'a', para('original'))] };
const welcome = (session = 'tok') => ({ type: 'welcome', access: 'private', session, session_id: session + '-editor' });

function type(app, timers) {
  fire(caretAt(app, 'x', 0), 'beforeinput', { inputType: 'insertText', data: 'unsaved ', getTargetRanges: () => [] });
  timers.advance(200);
}

function close(socket) {
  socket().onclose({ code: 1001, reason: '', wasClean: true });
}

test('an ordinary host close reconnects and resends a pending action exactly once', async () => {
  const { app, socket, timers } = await startApp(content);
  type(app, timers);
  const original = socket().frames('action')[0];
  close(socket);
  assert.equal(app.net.stopped, null);
  assert.equal(app.statusEl.textContent, 'Disconnected · 1 unsaved edit');
  timers.advance(999);
  assert.equal(FakeSocket.all.length, 1);
  timers.advance(1);
  assert.equal(FakeSocket.all.length, 2);
  const hello = socket().open();
  assert.equal(hello.session, 'tok');
  assert.deepEqual(hello.pending, { count: 1, first_n: original.n });
  socket().recv(welcome());
  app.net.flush();
  timers.advance(30000);
  assert.deepEqual(socket().frames('action'), [original]);
  socket().recv({ type: 'ack', n: original.n, seq: '11' });
  timers.advance(30000);
  assert.deepEqual(socket().frames('action'), [original]);
  assert.equal(app.store.pending.size, 0);
  assert.equal(app.statusEl.textContent, 'Saved');
});

test('a new session after a host close continues when all edits were confirmed', async () => {
  const { app, socket, timers } = await startApp(content);
  type(app, timers);
  socket().recv({ type: 'ack', n: '1', seq: '11' });
  close(socket);
  assert.equal(app.statusEl.textContent, 'Disconnected');
  timers.advance(1000);
  socket().open();
  socket().recv(welcome('new-token'));
  assert.equal(app.net.stopped, null);
  assert.equal(app.net.sessionId, 'new-token-editor');
  assert.equal(app.statusEl.textContent, 'Saved');
  type(app, timers);
  assert.equal(socket().frames('action')[0].n, '1');
});

test('an unknown session without pending edits reconnects without the expired token', async () => {
  const { app, socket, timers } = await startApp(content);
  type(app, timers);
  socket().recv({ type: 'ack', n: '1', seq: '11' });
  close(socket);
  timers.advance(1000);
  assert.equal(socket().open().session, 'tok');
  const expired = socket();
  expired.recv({ type: 'fatal', code: 'session_expired' });
  expired.drop();
  assert.equal(app.net.stopped, null);
  assert.equal(app.statusEl.textContent, 'Disconnected');
  assert.equal(app.banners.querySelector('textarea'), null);
  timers.advance(2000);
  assert.equal(FakeSocket.all.length, 3);
  const hello = socket().open();
  assert.equal(hello.session, undefined);
  assert.deepEqual(hello.pending, { count: 0 });
  socket().recv(welcome('new-token'));
  assert.equal(app.statusEl.textContent, 'Saved');
  type(app, timers);
  assert.equal(socket().frames('action')[0].n, '1');
});

for (const response of ['new session', 'session_expired']) {
  test(`${response} after a host close stops pending edits with a copy-out notice`, async () => {
    const { app, socket, timers } = await startApp(content);
    type(app, timers);
    const pending = socket().frames('action')[0];
    close(socket);
    timers.advance(1000);
    socket().open();
    socket().recv(response === 'new session' ? welcome('new-token') : { type: 'fatal', code: 'session_expired' });
    timers.advance(120000);
    assert.equal(FakeSocket.all.length, 2);
    assert.equal(app.net.stopped.code, 'session_expired');
    assert.equal(app.statusEl.textContent, 'Editing stopped');
    assert.equal(app.banners.querySelector('textarea').textContent, 'unsaved original');
    assert.match(app.banners.textContent, /server no longer knows this tab/);
    assert.deepEqual(app.store.pending.actions.map((a) => a.n), [pending.n]);
    assert.deepEqual(socket().frames('action'), []);
  });
}

for (const code of ['deleted', 'renamed']) {
  test(`a ${code} notebook still stops the tab without reconnecting`, async () => {
    const { app, socket, timers } = await startApp(content);
    socket().recv({ type: 'fatal', code });
    timers.advance(120000);
    assert.equal(FakeSocket.all.length, 1);
    assert.equal(app.statusEl.textContent, 'Editing stopped');
    assert.match(app.banners.textContent, new RegExp(code));
  });
}
