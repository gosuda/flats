'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { startApp, page, block, para, run, bid, caretAt } from './page.mjs';
import { fire, textNodes } from './dom.mjs';

const removed = (tag) => ({ deleted: { id: bid(tag), inc: '5' } });

const structuralCases = {
  'ancestor conversion': () => [block('parent', 'a', para('Parent'), { position: 'A' }), block('child', 'a', para('remote'), { position: 'B' })],
  'ancestor deletion': () => [removed('parent'), removed('child')],
  'ancestor reparenting': () => [block('parent', 'a', para('Parent', 'bullet'), { parent: 'other', position: 'A' }), block('child', 'a', para('remote'), { parent: 'parent' })],
  'target reparenting': () => [block('child', 'a', para('remote'), { parent: 'other' })],
  'target deletion': () => [removed('child')],
  'target revival': () => [block('child', 'a', para('revived'), { parent: 'parent', inc: '11' })],
  'ancestor toggle closure': () => [block('parent', 'a', { ...para('Parent', 'toggle'), open_default: false }, { position: 'A' })],
};

for (const [name, changes] of Object.entries(structuralCases)) {
  test(`composition stays attached and focused through ${name}`, async (t) => {
    const toggle = name === 'ancestor toggle closure';
    const parentContent = toggle ? { ...para('Parent', 'toggle'), open_default: true } : para('Parent', 'bullet');
    const { app, doc, socket, timers } = await startApp({
      pages: [page('a')],
      blocks: [block('parent', 'a', parentContent, { position: 'A' }), block('child', 'a', para('base'), { parent: 'parent' }), block('other', 'a', para('Other', 'bullet'), { position: 'C' })],
    });
    const el = caretAt(app, 'child', 4);
    const wrapper = app.pageView.entryOf(bid('child')).el;
    const parent = app.pageView.entryOf(bid('parent')).el;
    const nodePrototype = Object.getPrototypeOf(Object.getPrototypeOf(el));
    const removeChild = nodePrototype.removeChild;
    let detachments = 0;
    nodePrototype.removeChild = function (child) {
      if (app.store.composing && child.isConnected && child.contains(el)) detachments++;
      return removeChild.call(this, child);
    };
    t.after(() => { nodePrototype.removeChild = removeChild; });
    fire(el, 'compositionstart');
    el.textContent = 'base한😀';
    getSelection().collapse(el.firstChild, el.firstChild.length);
    socket().recv({ t: 'delta', generation: app.net.generation, seq: '11', tree: [], scopes: [], changes: changes() });
    assert.equal(el.isConnected, true);
    assert.equal(doc.activeElement, el);
    assert.equal(detachments, 0, 'no transient detach and refocus may hide a broken composition');
    assert.equal(app.pageView.entryOf(bid('child')).el, wrapper);
    assert.equal(el.textContent, 'base한😀');
    assert.equal(socket().frames('action').length, 0);
    assert.equal(app.pageView.entryOf(bid('parent')).el, parent);

    fire(el, 'compositionend');
    fire(el, 'input', { inputType: 'insertCompositionText' });
    timers.advance(200);
    assert.equal(app.store.composing, null);
    const orphan = ['ancestor deletion', 'target deletion', 'target revival'].includes(name);
    if (orphan) {
      assert.equal(socket().frames('action').length, 0, 'never write deleted or revived targets');
      assert.equal(app.notices.length, 1);
      assert.equal(app.notices[0].content.text.map((r) => r.text).join(''), 'base한😀');
      assert.ok(app.banners.textContent.includes('Keep my text'));
      if (name !== 'target revival') assert.equal(el.isConnected, false);
    } else {
      const actions = socket().frames('action');
      assert.equal(actions.length, 1);
      assert.equal(actions[0].ops.length, 1);
      assert.deepEqual(actions[0].ops[0].block, { id: bid('child'), inc: '5' });
      assert.equal(actions[0].ops[0].content.text.map((r) => r.text).join(''), 'base한😀');
      if (!toggle) assert.equal(doc.activeElement, app.editableOf(bid('child')), 'reconciliation restores the composing caret');
      if (name === 'ancestor conversion') assert.equal(app.pageView.entryOf(bid('parent')).kind, 'paragraph');
      if (name === 'ancestor reparenting') assert.ok(app.pageView.entryOf(bid('other')).el.contains(parent));
      if (name === 'target reparenting') assert.ok(app.pageView.entryOf(bid('other')).el.contains(wrapper));
      if (toggle) assert.equal(app.pageView.entryOf(bid('parent')).kids.hidden, true);
    }
  });
}

test('reset leaves composition attached until its text can be recovered', async () => {
  const { app, doc, socket } = await startApp({ pages: [page('a')], blocks: [block('child', 'a', para('base'))] });
  const el = caretAt(app, 'child', 4);
  fire(el, 'compositionstart');
  el.textContent = 'base한';
  socket().recv({ t: 'delta', generation: app.net.generation, seq: '11', tree: [], scopes: [], changes: [], reset: true });
  assert.equal(el.isConnected, true);
  assert.equal(doc.activeElement, el);
  fire(el, 'compositionend');
  assert.equal(app.notices[0].content.text.map((r) => r.text).join(''), 'base한');
  assert.equal(socket().frames('action').length, 0);
});

const pointerCases = [
  { name: 'plain forward', first: [run('abcdef')], last: [run('uvwxyz')], a: [0, 2], f: [0, 3], offsets: [2, 3], want: 'cdef\nuvw' },
  { name: 'plain reversed', first: [run('abcdef')], last: [run('uvwxyz')], a: [0, 2], f: [0, 3], offsets: [2, 3], want: 'cdef\nuvw', reverse: true },
  { name: 'marked surrogate pairs', first: [run('ab', 'bold'), run('😀cd', 'italic'), run('ef')], last: [run('uv😀', 'bold', 'italic'), run('wxyz', 'code')], a: [1, 2], f: [0, 4], offsets: [3, 3], want: 'cdef\nuv😀' },
  { name: 'reversed marked surrogate pairs', first: [run('ab', 'bold'), run('😀cd', 'italic'), run('ef')], last: [run('uv😀', 'bold', 'italic'), run('wxyz', 'code')], a: [1, 2], f: [0, 4], offsets: [3, 3], want: 'cdef\nuv😀', reverse: true },
];

for (const c of pointerCases) {
  test(`pointer selection and copy preserve partial endpoints: ${c.name}`, async () => {
    const { app, doc } = await startApp({ pages: [page('a')], blocks: [block('first', 'a', { kind: 'paragraph', text: c.first }, { position: 'A' }), block('last', 'a', { kind: 'paragraph', text: c.last }, { position: 'B' })] });
    const first = app.editableOf(bid('first')), last = app.editableOf(bid('last'));
    first.focus();
    const a = textNodes(first)[c.a[0]], f = textNodes(last)[c.f[0]];
    if (c.reverse) getSelection().setBaseAndExtent(f, c.f[1], a, c.a[1]);
    else getSelection().setBaseAndExtent(a, c.a[1], f, c.f[1]);
    fire(doc, 'mouseup');
    assert.deepEqual(app.selection.anchor, { block: bid(c.reverse ? 'last' : 'first'), offset: c.offsets[c.reverse ? 1 : 0] });
    assert.deepEqual(app.selection.focus, { block: bid(c.reverse ? 'first' : 'last'), offset: c.offsets[c.reverse ? 0 : 1] });
    let copied;
    const event = fire(doc, 'copy', { clipboardData: { setData: (type, value) => { assert.equal(type, 'text/plain'); copied = value; } } });
    assert.equal(event.defaultPrevented, true);
    assert.equal(copied, c.want);
    assert.equal(doc.activeElement, app.pageEl);
  });
}

test('pointer selection copies partial code blocks in code points', async () => {
  const { app, doc } = await startApp({ pages: [page('a')], blocks: [block('first', 'a', { kind: 'code', text: 'a😀bcd', language: '' }, { position: 'A' }), block('last', 'a', { kind: 'code', text: 'xy😀z', language: '' }, { position: 'B' })] });
  const first = app.editableOf(bid('first')), last = app.editableOf(bid('last'));
  getSelection().setBaseAndExtent(first.firstChild, 3, last.firstChild, 4);
  fire(doc, 'mouseup');
  let copied;
  fire(doc, 'copy', { clipboardData: { setData: (_, value) => { copied = value; } } });
  assert.equal(copied, 'bcd\nxy😀');
});

test('pointer endpoints on marked element boundaries count preceding runs', async () => {
  const { app, doc } = await startApp({ pages: [page('a')], blocks: [block('first', 'a', { kind: 'paragraph', text: [run('a😀', 'bold'), run('bc')] }, { position: 'A' }), block('last', 'a', { kind: 'paragraph', text: [run('xy', 'italic'), run('z')] }, { position: 'B' })] });
  const first = app.editableOf(bid('first')), last = app.editableOf(bid('last'));
  getSelection().setBaseAndExtent(first, 1, last.firstChild, 1);
  fire(doc, 'mouseup');
  assert.deepEqual(app.selection.anchor, { block: bid('first'), offset: 2 });
  assert.deepEqual(app.selection.focus, { block: bid('last'), offset: 2 });
  let copied;
  fire(doc, 'copy', { clipboardData: { setData: (_, value) => { copied = value; } } });
  assert.equal(copied, 'bc\nxy');
});
