import test from 'node:test';
import assert from 'node:assert/strict';
import { installDOM } from './dom.mjs';
import {
  cleanGuestName, guestNameError, runeCount, viewerLabel, viewersOf, helloFields, byKind, who, badge, nameWithBadge,
  rejectMessage, guestBatchFits, stored, GUEST_NAME_MAX, GUEST_BATCH_OPS, GUEST_FATALS,
} from '../guest.js';
import { guestCredit, creditText } from '../history.js';
import { Net } from '../net.js';
import { BoardState } from '../state.js';

test('guest names lose control and format characters and are 1 to 40 characters', () => {
  assert.equal(cleanGuestName('  Mina  '), 'Mina');
  assert.equal(cleanGuestName('Mi\u0000na\u0007'), 'Mina', 'C0 controls');
  assert.equal(cleanGuestName('Mi\u0085na\u009f'), 'Mina', 'C1 controls');
  assert.equal(cleanGuestName('‮Mina​⁦'), 'Mina', 'bidi and zero-width format characters');
  assert.equal(cleanGuestName('​ ​'), '');
  assert.equal(guestNameError(''), 'Type a name to join.');
  assert.equal(guestNameError(' ​\n'), 'Type a name to join.', 'a name that cleans to nothing is refused');
  assert.equal(guestNameError('a'.repeat(GUEST_NAME_MAX)), '');
  assert.match(guestNameError('a'.repeat(GUEST_NAME_MAX + 1)), /at most 40/);
  // Characters, not UTF-16 code units: 40 emoji fit.
  const emoji = '🙂'.repeat(GUEST_NAME_MAX);
  assert.equal(runeCount(emoji), 40);
  assert.equal(guestNameError(emoji), '');
  assert.equal(guestNameError(`  ${'b'.repeat(GUEST_NAME_MAX)}​  `), '', 'counted after cleaning');
});

test('viewer labels and the viewers frame', () => {
  assert.equal(viewerLabel({ label: 'Viewer 3', viewer: 3 }), 'Viewer 3');
  assert.equal(viewerLabel({ viewer: 7 }), 'Viewer 7');
  assert.equal(viewerLabel(null), 'Viewer');
  assert.deepEqual(viewersOf({ t: 'viewers', n: [3, 1, 2] }), { count: 3, labels: ['Viewer 1', 'Viewer 2', 'Viewer 3'] });
  assert.deepEqual(viewersOf({ t: 'viewers', n: [] }), { count: 0, labels: [] });
  assert.deepEqual(viewersOf({ t: 'viewers', count: 27 }), { count: 27, labels: [] }, 'above 20 only a count');
  const many = Array.from({ length: 21 }, (_, i) => i + 1);
  assert.deepEqual(viewersOf({ t: 'viewers', n: many }), { count: 21, labels: [] }, 'never more than 20 entries');
  assert.deepEqual(viewersOf({ t: 'viewers', n: [1, 'x', -2, 2.5] }), { count: 1, labels: ['Viewer 1'] }, 'malformed entries are ignored');
});

test('hello asks to join only when the visitor chose to, and carries the tab tokens', () => {
  assert.deepEqual(helloFields({ join: false, name: 'Mina', guestToken: 'aa', viewerToken: '' }), {});
  assert.deepEqual(helloFields({ join: true, name: 'Mina', guestToken: '', viewerToken: '' }), { guest: { name: 'Mina' } });
  assert.deepEqual(helloFields({ join: true, name: 'Mina', guestToken: 'aa', viewerToken: 'bb' }), { guest: { name: 'Mina', token: 'aa' }, viewer_token: 'bb' });
  assert.deepEqual(helloFields({ join: true, name: '', guestToken: 'aa', viewerToken: 'bb' }), { viewer_token: 'bb' }, 'no guest without a name');
});

test('names and tokens live in browser storage, and blocked storage is harmless', () => {
  const mem = () => { const m = new Map(); return { getItem: (k) => m.get(k) ?? null, setItem: (k, v) => m.set(k, String(v)), removeItem: (k) => m.delete(k), m }; };
  globalThis.localStorage = mem();
  globalThis.sessionStorage = mem();
  stored.setName('Mina');
  stored.setGuestToken('aa');
  stored.setViewerToken('bb');
  assert.equal(localStorage.m.get('flats-board-guest-name'), 'Mina');
  assert.equal(sessionStorage.m.get('flats-board-guest-token'), 'aa');
  assert.equal(sessionStorage.m.get('flats-board-viewer-token'), 'bb');
  assert.equal(stored.name(), 'Mina');
  stored.setName('');
  assert.equal(stored.name(), '');
  const blocked = { getItem() { throw new Error('denied'); }, setItem() { throw new Error('denied'); }, removeItem() { throw new Error('denied'); } };
  globalThis.localStorage = blocked;
  globalThis.sessionStorage = blocked;
  assert.equal(stored.name(), '');
  assert.doesNotThrow(() => stored.setGuestToken('cc'));
  delete globalThis.localStorage;
  delete globalThis.sessionStorage;
});

test('authors: a guest socket gets kinds, a member socket objects', () => {
  assert.equal(byKind('guest'), 'guest');
  assert.equal(byKind({ kind: 'agent', label: 'Claude' }), 'agent');
  assert.equal(byKind(null), '');
  assert.deepEqual(who('member'), { text: 'A member', guest: false });
  assert.deepEqual(who('guest'), { text: 'A guest', guest: false }, 'no name, so no badge after a generic word');
  assert.deepEqual(who({ kind: 'guest', label: 'Mina' }), { text: 'Mina', guest: true });
  assert.deepEqual(who({ kind: 'person', label: 'Ann' }), { text: 'Ann', guest: false });
  assert.deepEqual(who(undefined), { text: 'Someone', guest: false });
});

test('the guest badge is a separate element next to the name text', () => {
  installDOM();
  const b = badge('guest');
  assert.equal(b.tagName, 'SPAN');
  assert.equal(b.className, 'badge guest');
  assert.equal(b.textContent, 'guest');
  // A name that imitates the badge stays plain text before the real badge.
  const parts = nameWithBadge('Mina guest ✓ verified', true);
  assert.equal(parts[0], 'Mina guest ✓ verified');
  assert.equal(parts[2].className, 'badge guest');
  assert.deepEqual(nameWithBadge('Ann', false), ['Ann']);
});

test('revision credits list guests with the badge and "and N more guests"', () => {
  installDOM();
  assert.equal(guestCredit({ authors: ['ann'] }), null);
  const c = guestCredit({ guest_authors: ['Mina', 'Rui', 'Kai', 'Sol', 'Jo'], guests_more: 3 });
  const text = (n) => (n.tagName ? n.children.map(text).join('') || n.textContent : n.textContent);
  assert.equal(text(c), 'Mina, Rui, Kai, Sol, Jo guest and 3 more guests');
  assert.ok(c.children.some((k) => k.className === 'badge guest'));
  assert.equal(text(guestCredit({ guest_authors: ['Mina'] })), 'Mina guest');
  assert.equal(creditText({ guest_authors: ['Mina'], guests_more: 0 }), 'Mina (guest)');
  assert.equal(creditText({ guest_authors: ['Mina', 'Rui'], guests_more: 1 }), 'Mina, Rui (guests) and 1 more guest');
  assert.equal(creditText({}), '');
});

test('guest refusals have messages; other codes keep the generic path', () => {
  for (const code of ['guest_rate', 'guest_size', 'guest_room', 'guest_asset', 'guest_revive', 'permission']) {
    assert.ok(rejectMessage(code), code);
  }
  assert.match(rejectMessage('guest_rate'), /too fast/);
  assert.equal(rejectMessage('limit'), null);
  assert.ok(GUEST_FATALS.has('editing_ended') && GUEST_FATALS.has('full') && GUEST_FATALS.has('name_invalid'));
  assert.ok(!GUEST_FATALS.has('protocol'));
});

test('a guest batch is one ops frame of at most 64 KiB and 100 operations', () => {
  const op = (i) => ({ op: 'set', id: 'e_' + String(i).padStart(10, '0'), inc: 1, g: 'text', val: 'x' });
  assert.ok(guestBatchFits(1, Array.from({ length: GUEST_BATCH_OPS }, (_, i) => op(i))));
  assert.ok(!guestBatchFits(1, Array.from({ length: GUEST_BATCH_OPS + 1 }, (_, i) => op(i))));
  assert.ok(!guestBatchFits(1, [{ ...op(0), val: 'y'.repeat(64 * 1024) }]), 'over 64 KiB never becomes ops_part');
  assert.ok(guestBatchFits(1, [{ ...op(0), val: 'y'.repeat(60 * 1024) }]));
});

class FakeSocket {
  static last = null;
  constructor() { this.sent = []; FakeSocket.last = this; }
  send(s) { this.sent.push(JSON.parse(s)); }
  close() {}
  recv(m) { this.onmessage({ data: JSON.stringify(m) }); }
}

function harness(t) {
  globalThis.WebSocket = FakeSocket;
  globalThis.location = { protocol: 'http:', host: 'board.test' };
  t.mock.timers.enable({ apis: ['setTimeout', 'Date'] });
  const state = new BoardState();
  const seen = { viewers: [], levels: [], fatal: null, welcomes: 0 };
  const net = new Net(state, {
    hello: () => ({ t: 'hello', v: 1, pending: { count: 0 }, editor: 'ed' }),
    welcome() { seen.welcomes++; }, status() {}, pres() {}, shared() {}, tooLarge() {}, snapshot() {}, delta() {}, ack() {}, reject() {},
    viewers(m) { seen.viewers.push(m); },
    level(m) { seen.levels.push(m.access); },
    fatal(code, m) { seen.fatal = { code, m }; },
  });
  net.start();
  FakeSocket.last.onopen();
  return { state, net, seen };
}

test('a guest uses the private wire form and sends presence at most 10 times a second', (t) => {
  const { state, net } = harness(t);
  const ws = FakeSocket.last;
  ws.recv({ t: 'welcome', session: 'S1', access: 'guest', level: 'edit', me: { key: 'p:1', label: 'Mina', guest: true } });
  assert.equal(state.public, false, 'guests merge with versions like members');
  for (let i = 0; i < 20; i++) { net.presence({ c: [i, i] }); t.mock.timers.tick(10); }
  const pres = ws.sent.filter((m) => m.t === 'pres');
  assert.ok(pres.length <= 3, `${pres.length} presence frames in 200 ms`);
  assert.ok(pres.length >= 1);
});

test('a viewer gets the public form and sends no presence', (t) => {
  const { state, net } = harness(t);
  const ws = FakeSocket.last;
  ws.recv({ t: 'welcome', access: 'public', level: 'view', me: { label: 'Viewer 2', viewer: 2 } });
  assert.equal(state.public, true);
  net.presence({ c: [1, 1] });
  t.mock.timers.tick(200);
  assert.equal(ws.sent.filter((m) => m.t === 'pres').length, 0);
});

test('viewers and level frames reach the app; a guest fatal code keeps its message and can resume', (t) => {
  const { net, seen } = harness(t);
  const ws = FakeSocket.last;
  ws.recv({ t: 'welcome', access: 'public', level: 'view', me: { label: 'Viewer 1', viewer: 1 } });
  ws.recv({ t: 'viewers', n: [1, 2] });
  ws.recv({ t: 'level', access: 'edit' });
  assert.deepEqual(seen.viewers, [{ t: 'viewers', n: [1, 2] }]);
  assert.deepEqual(seen.levels, ['edit']);
  ws.recv({ t: 'fatal', code: 'name_invalid', detail: 'Use a shorter name.' });
  assert.equal(seen.fatal.code, 'name_invalid');
  assert.equal(seen.fatal.m.detail, 'Use a shorter name.');
  assert.equal(net.stopped, true);
  t.mock.timers.tick(60000);
  assert.equal(FakeSocket.last, ws, 'a fatal code does not reconnect by itself');
  net.resume();
  assert.notEqual(FakeSocket.last, ws, 'resume opens a new socket');
  assert.equal(net.stopped, false);
  FakeSocket.last.onopen();
  assert.equal(FakeSocket.last.sent[0].t, 'hello');
  FakeSocket.last.recv({ t: 'welcome', access: 'public', level: 'edit', me: { label: 'Viewer 1', viewer: 1 } });
  assert.equal(seen.welcomes, 2);
});
