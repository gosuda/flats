'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { startApp, page, block, para, pid, bid, caretAt, lastAction } from './page.mjs';
import { fire } from './dom.mjs';

const welcome = (id = 'me') => ({ type: 'welcome', access: 'private', session: 'token-' + id, session_id: id });

function snapshot(ctx, items, seq = '10', scope = { kind: 'page', id: pid('a') }) {
  ctx.socket().recv({ type: 'snapshot', generation: ctx.app.net.generation, scope, seq,
    spill: 'capture-' + seq, part: 0, parts: 1, spill_part: 0, data: JSON.stringify({ items }) });
}

async function setup() {
  const ctx = await startApp({ access: null });
  ctx.socket().open();
  ctx.socket().recv(welcome());
  snapshot(ctx, [page('a')], '10', { kind: 'tree' });
  snapshot(ctx, [block('x', 'a', para('first'))]);
  return ctx;
}

function ack(ctx, action, seq, outcomes = [{ indexes: action.ops.map((_, i) => i), status: 'applied' }]) {
  const count = (status) => outcomes.filter((o) => o.status === status).reduce((n, o) => n + o.indexes.length, 0);
  ctx.socket().recv({ type: 'ack', n: action.n, seq, outcomes,
    summary: { applied: count('applied') + count('adjusted'), ignored: count('ignored'), refused: count('refused'), copied_roots: [], complete: true },
    results: [], results_sample: { total: 0, complete: true } });
}

function reconnect(ctx) {
  ctx.socket().drop();
  ctx.timers.advance(1000);
  const hello = ctx.socket().open();
  ctx.socket().recv(welcome());
  return hello;
}

function created(op, inc = '11') {
  return { kind: 'block', ref: { id: op.proposed_id, inc }, ver: inc, v: {},
    page: pid('a'), parent: null, position: 'z', content: op.content };
}

function enter(ctx, offset = 5) {
  fire(caretAt(ctx.app, 'x', offset), 'keydown', { key: 'Enter' });
  const action = lastAction(ctx.socket);
  const op = action.ops.find((op) => op.op === 'create_block');
  return { action, op, el: ctx.app.editableOf(op.proposed_id) };
}

function saveText(ctx, text, seq) {
  const b = ctx.app.view().get(bid('x'));
  const action = ctx.app.queue([{ op: 'set_block', block: b.ref, base_v: b.v.content || b.ref.inc, content: para(text) }]);
  ctx.socket().recv({ type: 'delta', generation: ctx.app.net.generation, seq,
    changes: [block('x', 'a', para(text), { v: { content: seq } })], tree: [],
    from: { session_id: ctx.app.net.sessionId, n: action.n } });
  ack(ctx, action, seq);
  return action;
}

function overwrite(ctx, source, seq) {
  ctx.socket().recv({ type: 'delta', generation: ctx.app.net.generation, seq,
    changes: [block('x', 'a', para('agent text'), { v: { content: seq } })], tree: [],
    from: { session_id: 'agent', n: '1' },
    overwrote: [{ block: block('x', 'a', para('')).ref, version: String(BigInt(seq) - 1n), base: '5', source }] });
}

test('fresh-session action numbers recover the displaced receipt text, not the previous session text', async () => {
  const ctx = await setup();
  const { app, socket, timers } = ctx;
  const old = saveText(ctx, 'old text', '11');
  assert.equal(app.store.pending.size, 0);
  socket().drop();
  timers.advance(1000);
  assert.equal(socket().open().session, 'token-me');
  socket().recv({ type: 'fatal', code: 'session_expired' });
  timers.advance(2000);
  assert.equal(socket().open().session, undefined);
  socket().recv(welcome('fresh'));
  snapshot(ctx, [page('a')], '11', { kind: 'tree' });
  snapshot(ctx, [block('x', 'a', para('old text'), { v: { content: '11' } })], '11');
  const fresh = saveText(ctx, 'new text', '12');
  assert.equal(fresh.n, old.n, 'the fresh session reuses action 1');
  overwrite(ctx, { session_id: 'fresh', n: fresh.n }, '13');
  const notice = app.banners.querySelector('[data-notice]');
  assert.equal(notice.querySelector('q').textContent, 'new text');
  notice.querySelectorAll('button').find((b) => b.textContent === 'Keep my text').click();
  assert.deepEqual(lastAction(socket).ops.find((op) => op.op === 'create_block').content, para('new text'));
  assert.equal(app.notices.length, 0);
  app.net.stop('test');
});

test('replayed text receipts retain exact text while overwrite notices stay limited to eight', async () => {
  const ctx = await setup();
  const { app, socket } = ctx;
  let seq = 10;
  for (let i = 0; i < 10; i++) {
    const b = app.view().get(bid('x'));
    const action = app.queue([{ op: 'set_block', block: b.ref, base_v: b.v.content || b.ref.inc, content: para('text ' + i) }]);
    const sent = lastAction(socket);
    const hello = reconnect(ctx);
    assert.deepEqual(hello.pending, { count: 1, first_n: action.n });
    assert.deepEqual(socket().frames('action'), [sent]);
    socket().recv({ type: 'resync', generation: 1, seq: String(++seq) });
    snapshot(ctx, [block('x', 'a', para('text ' + i), { v: { content: String(seq) } })], String(seq));
    ack(ctx, action, String(seq));
    overwrite(ctx, { session_id: 'me', n: action.n }, String(++seq));
    assert.equal(app.notices.at(-1).content.text[0].text, 'text ' + i);
  }
  assert.equal(app.notices.length, 8);
  assert.deepEqual(app.banners.querySelectorAll('[data-notice]').map((n) => n.querySelector('q').textContent),
    Array.from({ length: 8 }, (_, i) => 'text ' + (i + 2)));
  app.net.stop('test');
});

for (const delivery of ['snapshot ack', 'replayed receipt']) {
  for (const composing of [false, true]) {
    test(`${delivery} preserves the created editable and ${composing ? 'IME composition' : 'caret'} after reconnect`, async () => {
      const ctx = await setup();
      const { app, socket, timers } = ctx;
      const { action, op, el } = enter(ctx, 2);
      app.focusBlock(op.proposed_id, 1);
      if (composing) {
        fire(el, 'compositionstart');
        el.textContent = '한';
      }
      const anchor = composing ? el.firstChild : getSelection().anchorNode;
      const hello = reconnect(ctx);
      assert.equal(hello.session, 'token-me');
      assert.deepEqual(hello.pending, { count: 1, first_n: action.n });
      assert.deepEqual(socket().frames('action'), [action]);
      if (delivery === 'replayed receipt') {
        // Lose a second connection before its receipt, then resend the same action again.
        reconnect(ctx);
        assert.deepEqual(socket().frames('action'), [action]);
      }
      socket().recv({ type: 'resync', generation: 1, seq: '20' });
      snapshot(ctx, [page('a')], '20', { kind: 'tree' });
      snapshot(ctx, [block('x', 'a', para('fi')), created(op)], '20');
      assert.ok(app.editableOf(op.proposed_id) === el, 'pending hydration keeps the editable');
      assert.ok(document.activeElement === el);
      ack(ctx, action, '11');
      assert.equal(app.store.pending.size, 0);
      assert.equal(app.view().get(op.proposed_id).ref.inc, '11');
      assert.ok(app.editableOf(op.proposed_id) === el, 'the ack keeps the same editable');
      assert.ok(document.activeElement === el, 'the ack keeps activeElement');
      if (composing) {
        assert.ok(el.firstChild === anchor, 'hydration and ack keep the composition node');
        fire(el, 'compositionend', { data: '한' });
        fire(el, 'input');
      } else {
        assert.ok(getSelection().anchorNode === anchor, 'the ack keeps the caret node');
        assert.equal(getSelection().anchorOffset, 1);
        fire(el, 'beforeinput', { inputType: 'insertText', data: 'next', getTargetRanges: () => [] });
      }
      timers.advance(200);
      assert.ok(app.editableOf(op.proposed_id) === el, 'composition end and typing keep the editable');
      assert.ok(document.activeElement === el);
      assert.deepEqual(lastAction(socket).ops[0].block, { id: op.proposed_id, inc: '11' });
      assert.deepEqual(lastAction(socket).ops[0].content, para(composing ? '한' : 'rnextst'));
      assert.equal(app.notices.length, 0);
      app.net.stop('test');
    });
  }
}

for (const scenario of ['ignored', 'refused', 'revived', 'unrelated', 'absent']) {
  test(`receipt hydration does not promote a creation that is ${scenario}`, async () => {
    const ctx = await setup();
    const { app } = ctx;
    const { action, op, el } = enter(ctx);
    const promoted = [];
    const confirm = app.net.h.confirmCreation;
    app.net.h.confirmCreation = (ref) => { promoted.push(ref); confirm(ref); };
    reconnect(ctx);
    const item = created(op, scenario === 'revived' ? '19' : '5');
    if (scenario === 'unrelated') item.ref.id = bid('other');
    snapshot(ctx, [block('x', 'a', para('first')), ...(scenario === 'absent' ? [] : [item])], '20');
    const status = scenario === 'ignored' || scenario === 'refused' ? scenario : 'applied';
    ack(ctx, action, '11', [{ indexes: [0], status: 'applied' }, { indexes: [1], status }]);
    assert.deepEqual(promoted, []);
    assert.ok(app.editableOf(op.proposed_id) !== el, 'an unconfirmed identity is not aliased');
    assert.ok(document.activeElement !== el);
    app.net.stop('test');
  });
}
