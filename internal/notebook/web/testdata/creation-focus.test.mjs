'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { startApp, page, block, para, run, pid, bid, caretAt, lastAction, queued, ackAll } from './page.mjs';
import { fire } from './dom.mjs';

function confirm(socket, action, changes, tree = []) {
  socket().recv({ t: 'delta', generation: 0, seq: '11', tree, changes, from: { session_id: 'me', n: action.n } });
  socket().recv({ t: 'ack', n: action.n, seq: '11', outcomes: [{ indexes: action.ops.map((_, i) => i), status: 'applied' }], summary: { applied: action.ops.length, ignored: 0, refused: 0, copied_roots: [], complete: true }, results: [], results_sample: { total: 0, complete: true } });
}

function createdBlock(op, { inc = '11', content = op.content } = {}) {
  return { kind: 'block', ref: { id: op.proposed_id, inc }, ver: inc, v: {}, page: pid('a'), parent: null, position: 'z', content };
}

function type(el, text) {
  fire(el, 'beforeinput', { inputType: 'insertText', data: text, getTargetRanges: () => [] });
}

for (const kind of ['paragraph', 'bullet', 'numbered', 'todo', 'toggle']) {
  test(`Enter at the end of a ${kind} keeps its new editable through confirmation`, async () => {
    const { app, socket, timers } = await startApp({ pages: [page('a')], blocks: [block('x', 'a', para('first', kind))] });
    fire(caretAt(app, 'x', 5), 'keydown', { key: 'Enter' });
    const action = lastAction(socket);
    const op = action.ops.find((o) => o.op === 'create_block');
    const el = app.editableOf(op.proposed_id);
    assert.equal(app.view().get(op.proposed_id).ref.inc, '0');
    assert.ok(document.activeElement === el, 'focus stays in the same editable');
    confirm(socket, action, [block('x', 'a', para('first', kind)), createdBlock(op)]);
    assert.equal(app.view().get(op.proposed_id).ref.inc, '11');
    assert.ok(app.editableOf(op.proposed_id) === el, 'the editable keeps its identity');
    assert.ok(document.activeElement === el, 'focus stays in the same editable');
    type(document.activeElement, 'next');
    timers.advance(200);
    assert.equal(el.textContent, 'next');
    assert.deepEqual(lastAction(socket).ops[0].block, { id: op.proposed_id, inc: '11' });
    assert.deepEqual(lastAction(socket).ops[0].content.text, [run('next')]);
  });
}

test('confirming a split keeps the caret between the same rich-text characters', async () => {
  const { app, socket, timers } = await startApp({ pages: [page('a')], blocks: [block('x', 'a', para('first'))] });
  fire(caretAt(app, 'x', 2), 'keydown', { key: 'Enter' });
  const action = lastAction(socket);
  const op = action.ops[1];
  const el = app.editableOf(op.proposed_id);
  app.focusBlock(op.proposed_id, 1);
  const anchor = getSelection().anchorNode;
  confirm(socket, action, [block('x', 'a', para('fi')), createdBlock(op)]);
  assert.ok(app.editableOf(op.proposed_id) === el, 'the editable keeps its identity');
  assert.ok(document.activeElement === el, 'focus stays in the same editable');
  assert.ok(getSelection().anchorNode === anchor, 'the caret keeps its text node');
  assert.equal(getSelection().anchorOffset, 1);
  type(document.activeElement, '!');
  timers.advance(200);
  assert.equal(el.textContent, 'r!st');
  assert.deepEqual(lastAction(socket).ops[0].content.text, [run('r!st')]);
});

test('Enter from an empty page title keeps the created paragraph focused when confirmed', async () => {
  const { app, socket, timers } = await startApp({ pages: [page('a')] });
  app.titleEl.focus();
  fire(app.titleEl, 'keydown', { key: 'Enter' });
  const action = lastAction(socket);
  const op = action.ops[0];
  const el = app.editableOf(op.proposed_id);
  confirm(socket, action, [createdBlock(op)]);
  assert.ok(app.editableOf(op.proposed_id) === el, 'the editable keeps its identity');
  assert.ok(document.activeElement === el, 'focus stays in the same editable');
  type(document.activeElement, 'body');
  timers.advance(200);
  assert.deepEqual(lastAction(socket).ops[0].content.text, [run('body')]);
});

test('composition in a new block survives confirmation and sends its text once', async () => {
  const { app, socket, timers } = await startApp({ pages: [page('a')], blocks: [block('x', 'a', para('first'))] });
  fire(caretAt(app, 'x', 5), 'keydown', { key: 'Enter' });
  const action = lastAction(socket);
  const op = action.ops[1];
  const el = app.editableOf(op.proposed_id);
  fire(el, 'compositionstart');
  el.textContent = '한';
  const textNode = el.firstChild;
  confirm(socket, action, [block('x', 'a', para('first')), createdBlock(op)]);
  assert.ok(document.activeElement === el, 'focus stays in the same editable');
  assert.ok(app.editableOf(op.proposed_id) === el, 'the editable keeps its identity');
  assert.ok(el.firstChild === textNode, 'composition keeps its text node');
  assert.equal(el.textContent, '한');
  assert.equal(socket().frames('action').length, 1);
  fire(el, 'compositionend', { data: '한' });
  fire(el, 'input');
  timers.advance(200);
  assert.ok(app.editableOf(op.proposed_id) === el, 'the editable keeps its identity');
  assert.ok(document.activeElement === el, 'focus stays in the same editable');
  assert.equal(app.banners.querySelector('[data-notice]'), null);
  const sent = socket().frames('action');
  assert.equal(sent.length, 2);
  assert.deepEqual(sent[1].ops[0], { op: 'set_block', block: { id: op.proposed_id, inc: '11' }, content: para('한'), base_v: '11' });
});

test('a real incarnation change still replaces a confirmed new block', async () => {
  const { app, socket } = await startApp({ pages: [page('a')], blocks: [block('x', 'a', para('first'))] });
  fire(caretAt(app, 'x', 5), 'keydown', { key: 'Enter' });
  const action = lastAction(socket);
  const op = action.ops[1];
  confirm(socket, action, [createdBlock(op)]);
  const el = app.editableOf(op.proposed_id);
  socket().recv({ t: 'delta', seq: '12', changes: [createdBlock(op, { inc: '12' })], tree: [], from: { session_id: 'other', n: '1' } });
  assert.ok(app.editableOf(op.proposed_id) !== el, 'a different kind or incarnation rebuilds');
  assert.ok(document.activeElement === document.body, 'the old editable loses focus');
});

test('a kind change in a creation confirmation still rebuilds the editable', async () => {
  const { app, socket } = await startApp({ pages: [page('a')], blocks: [block('x', 'a', para('first'))] });
  fire(caretAt(app, 'x', 5), 'keydown', { key: 'Enter' });
  const action = lastAction(socket);
  const op = action.ops[1];
  const el = app.editableOf(op.proposed_id);
  confirm(socket, action, [createdBlock(op, { content: para('', 'heading1') })]);
  assert.ok(app.editableOf(op.proposed_id) !== el, 'a different kind or incarnation rebuilds');
  assert.equal(app.editableOf(op.proposed_id).tagName, 'H2');
});

test('typing before creation confirmation keeps both the local text and its caret', async () => {
  const { app, socket, timers } = await startApp({ pages: [page('a')], blocks: [block('x', 'a', para('first'))] });
  fire(caretAt(app, 'x', 5), 'keydown', { key: 'Enter' });
  const action = lastAction(socket);
  const op = action.ops[1];
  const el = app.editableOf(op.proposed_id);
  type(el, 'draft');
  const anchor = getSelection().anchorNode;
  confirm(socket, action, [createdBlock(op)]);
  assert.ok(app.editableOf(op.proposed_id) === el);
  assert.ok(document.activeElement === el);
  assert.ok(getSelection().anchorNode === anchor);
  assert.equal(getSelection().anchorOffset, 5);
  assert.equal(el.textContent, 'draft');
  timers.advance(200);
  assert.deepEqual(lastAction(socket).ops[0].content.text, [run('draft')]);
  assert.equal(lastAction(socket).ops[0].block.inc, '0', 'already queued references keep the session alias');
});

test('a slash conversion in a pending new block survives its creation confirmation', async () => {
  const { app, socket, timers } = await startApp({ pages: [page('a')], blocks: [block('x', 'a', para('first'))] });
  fire(caretAt(app, 'x', 5), 'keydown', { key: 'Enter' });
  const action = lastAction(socket);
  const op = action.ops[1];
  type(app.editableOf(op.proposed_id), '/todo');
  fire(app.editableOf(op.proposed_id), 'keydown', { key: 'Enter' });
  const el = app.editableOf(op.proposed_id);
  assert.equal(app.view().get(op.proposed_id).content.kind, 'todo');
  confirm(socket, action, [createdBlock(op)]);
  assert.ok(app.editableOf(op.proposed_id) === el);
  assert.ok(document.activeElement === el);
  type(el, 'task');
  timers.advance(200);
  const typed = queued(app);
  assert.deepEqual(typed.ops[0].content, para('task', 'todo'));
  ackAll(app, socket);
  assert.ok(socket().frames('action').some((a) => a.n === typed.n && a.ops[0].content.text[0].text === 'task'));
});

test('reviving a confirmed creation during composition never writes to the new incarnation', async () => {
  const { app, socket, timers } = await startApp({ pages: [page('a')], blocks: [block('x', 'a', para('first'))] });
  fire(caretAt(app, 'x', 5), 'keydown', { key: 'Enter' });
  const action = lastAction(socket);
  const op = action.ops[1];
  const el = app.editableOf(op.proposed_id);
  fire(el, 'compositionstart');
  el.textContent = '한';
  confirm(socket, action, [createdBlock(op)]);
  socket().recv({ t: 'delta', seq: '12', changes: [createdBlock(op, { inc: '12' })], tree: [], from: { session_id: 'other', n: '1' } });
  fire(el, 'compositionend', { data: '한' });
  timers.advance(200);
  assert.equal(socket().frames('action').length, 1);
  assert.match(app.banners.querySelector('[data-notice]').textContent, /한/);
  assert.ok(app.editableOf(op.proposed_id) !== el);
});

test('a newly created page keeps its title editable on confirmation', async () => {
  const { app, socket } = await startApp({ pages: [page('a')] });
  app.newPageBtn.click();
  const action = lastAction(socket);
  const op = action.ops[0];
  const el = app.titleEl;
  const title = { kind: 'page', ref: { id: op.proposed_id, inc: '11' }, ver: '11', v: {}, title: op.title, parent: null, position: 'z' };
  confirm(socket, action, [], [title]);
  assert.ok(document.activeElement === el);
  assert.ok(el.isConnected);
  assert.equal(app.view().get(op.proposed_id).ref.inc, '11');
});

for (const withOverwrite of [false, true]) {
  test(`rapid Enter and typing keep both editables through deltas${withOverwrite ? ' with unrelated overwrite entries' : ''}`, async () => {
    const initial = block('x', 'a', para('first'));
    const { app, socket, timers } = await startApp({ pages: [page('a')], blocks: [initial] });
    fire(caretAt(app, 'x', 5), 'keydown', { key: 'Enter' });
    const firstId = queued(app).ops.find((op) => op.op === 'create_block').proposed_id;
    const first = app.editableOf(firstId);
    type(first, 'Line A');
    fire(first, 'keydown', { key: 'Enter' });
    const secondId = queued(app).ops.find((op) => op.op === 'create_block').proposed_id;
    const second = app.editableOf(secondId);
    type(second, 'Line B');
    const anchor = getSelection().anchorNode;
    timers.advance(200);
    const records = new Map([[initial.ref.id, initial]]);
    let seq = 10;
    while (app.store.pending.size) {
      assert.ok(seq < 20, 'all pending actions settle');
      const action = lastAction(socket);
      assert.equal(action.n, app.store.pending.actions[0].n, 'the next action was sent');
      const version = String(++seq);
      const changes = action.ops.map((op) => {
        if (op.op === 'create_block') {
          const item = { ...createdBlock(op, { inc: version }), position: 'z' + version };
          records.set(item.ref.id, item);
          return item;
        }
        assert.equal(op.op, 'set_block');
        const item = { ...records.get(op.block.id), content: op.content, ver: version, v: { content: version } };
        records.set(item.ref.id, item);
        return item;
      });
      const overwrote = withOverwrite ? [{ block: initial.ref, version: '9', base: '5', source: { session_id: 'another-tab', n: '1' } }] : [];
      socket().recv({ type: 'delta', generation: 1, seq: version, changes, from: { session_id: 'me', n: action.n }, overwrote });
      assert.ok(app.editableOf(firstId) === first, 'the first editable keeps its identity');
      assert.ok(app.editableOf(secondId) === second, 'the second editable keeps its identity');
      assert.ok(document.activeElement === second, 'typing keeps focus');
      assert.ok(getSelection().anchorNode === anchor, 'typing keeps its caret node');
      assert.equal(getSelection().anchorOffset, 6);
      socket().recv({ type: 'ack', n: action.n, seq: version, outcomes: [{ indexes: action.ops.map((_, i) => i), status: 'applied' }], summary: { applied: action.ops.length, ignored: 0, refused: 0, copied_roots: [], complete: true }, results: [], results_sample: { total: 0, complete: true } });
      socket().recv({ type: 'presence', editors: [] });
      assert.ok(document.activeElement === second, 'ack and presence keep focus');
      assert.equal(app.banners.querySelector('[data-notice]'), null);
    }
    assert.deepEqual([firstId, secondId].map((id) => records.get(id).content), [para('Line A'), para('Line B')]);
    const sent = socket().frames('action').flatMap((a) => a.ops);
    for (const [id, text] of [[firstId, 'Line A'], [secondId, 'Line B']]) {
      assert.ok(sent.some((op) => op.op === 'set_block' && op.block.id === id && op.content.text[0].text === text), 'all typed text reached the socket');
    }
    type(document.activeElement, '!');
    timers.advance(200);
    assert.deepEqual(lastAction(socket).ops[0].content, para('Line B!'));
    app.net.stop('test');
  });
}

for (const firstRender of ['presence', 'stopped', 'ack']) {
  test(`creation is confirmed before the ${firstRender} render in the delta observer`, async () => {
    const { app, socket } = await startApp({ pages: [page('a')], blocks: [block('x', 'a', para('first'))] });
    fire(caretAt(app, 'x', 5), 'keydown', { key: 'Enter' });
    const action = lastAction(socket);
    const op = action.ops.find((o) => o.op === 'create_block');
    const el = app.editableOf(op.proposed_id);
    const changed = app.net.h.changed;
    app.net.h.changed = (delta) => {
      if (delta) {
        if (firstRender === 'presence') socket().recv({ type: 'presence', editors: [] });
        else if (firstRender === 'stopped') app.net.h.stopped();
        else app.net.h.ack(action, { outcomes: [] });
        assert.ok(app.editableOf(op.proposed_id) === el, 'confirmation precedes every observer render');
        assert.ok(document.activeElement === el, 'the observer keeps focus');
      }
      changed(delta);
    };
    confirm(socket, action, [createdBlock(op)]);
    assert.ok(document.activeElement === el);
    app.net.stop('test');
  });
}

for (const source of ['paste', 'duplicate']) {
  test(`${source} creates blocks on the server without rebuilding the original editable`, async () => {
    const { app, socket } = await startApp({ pages: [page('a')], blocks: [block('x', 'a', para('first'))], fetch: async () => ({ ok: true, json: async () => ({ markdown: 'second', refs: [{ ref: 'new', kind: 'block' }] }) }) });
    const el = caretAt(app, 'x', 5);
    if (source === 'paste') {
      fire(el, 'paste', { clipboardData: { types: ['text/plain'], getData: () => '## second' } });
      await new Promise(setImmediate);
    } else {
      fire(el, 'keydown', { key: 'd', ctrlKey: true });
    }
    const action = lastAction(socket);
    assert.equal(action.ops[0].op, source === 'paste' ? 'markdown_to_blocks' : 'duplicate_blocks');
    assert.equal(app.view().order(pid('a')).length, 1, 'there is no optimistic editable to replace');
    const id = source === 'paste' ? action.ops[0].proposed_ids[0].proposed_id : bid('copy');
    const copy = { ...block('copy', 'a', para('second'), { inc: '11', position: 'z' }), ref: { id, inc: '11' } };
    confirm(socket, action, [copy]);
    assert.ok(app.editableOf(bid('x')) === el);
    assert.ok(document.activeElement === el);
    assert.equal(app.editableOf(id).textContent, 'second');
  });
}
