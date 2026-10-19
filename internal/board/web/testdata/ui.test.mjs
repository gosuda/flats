import test from 'node:test';
import assert from 'node:assert/strict';
import { installDOM } from './dom.mjs';
import { toast, popover, closePopover, h, TOAST_KEEP_MAX } from '../ui.js';

test('toast eviction keeps toasts with actions and persistent toasts', (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const doc = installDOM([{ cls: 'toasts' }]);
  const host = doc.querySelector('.toasts');
  const keep = toast('The text you wrote was overwritten.', { actions: [['Keep my text', () => {}]], timeout: 0 });
  const sticky = toast('Import failed.', { error: true, timeout: 0 });
  const plain = Array.from({ length: 6 }, (_, i) => toast(`notice ${i}`));
  assert.ok(host.children.includes(keep), 'the action toast stays');
  assert.ok(host.children.includes(sticky), 'the persistent toast stays');
  assert.deepEqual(host.children.filter((c) => c !== keep && c !== sticky), plain.slice(2), 'the oldest plain toasts go');
  t.mock.timers.tick(60000);
  assert.deepEqual(host.children, [keep, sticky], 'plain toasts time out; the others wait for the person');
});

test('kept toasts are capped, and the newest recovery toast always stays', (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const doc = installDOM([{ cls: 'toasts' }]);
  const host = doc.querySelector('.toasts');
  let kept = [];
  for (let i = 0; i < 20; i++) kept.push(toast(`Ann changed "note ${i}", so the text you wrote was overwritten.`, { actions: [['Keep my text', () => {}]], timeout: 0 }));
  assert.equal(host.children.length, TOAST_KEEP_MAX, 'bounded');
  assert.deepEqual(host.children, kept.slice(-TOAST_KEEP_MAX), 'the oldest kept toasts go first');
  // Identical recovery toasts are not merged: each may hold different text.
  const a = toast('The element you were editing was deleted or rolled back.', { actions: [['Keep my text', () => {}]], timeout: 0 });
  const b = toast('The element you were editing was deleted or rolled back.', { actions: [['Keep my text', () => {}]], timeout: 0 });
  assert.ok(host.children.includes(a) && host.children.includes(b));
  assert.equal(host.children.at(-1), b, 'the newest Keep my text toast is there');
  // Plain toasts never push out kept ones.
  for (let i = 0; i < 10; i++) toast(`notice ${i}`);
  assert.equal(host.children.filter((c) => 'keep' in c.dataset).length, TOAST_KEEP_MAX);
  assert.ok(host.children.includes(b));
});

test('an identical notice without actions replaces its older copy', (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const doc = installDOM([{ cls: 'toasts' }]);
  const host = doc.querySelector('.toasts');
  const fail = Array.from({ length: 12 }, () => toast('Could not import: bad file. The board did not change.', { error: true, timeout: 0 }));
  assert.deepEqual(host.children, [fail.at(-1)], 'one persistent copy');
  const info = toast('Could not import: bad file. The board did not change.');
  assert.deepEqual(host.children, [fail.at(-1), info], 'an error and a plain notice are different notices');
  const again = toast('Nothing to undo.');
  toast('Nothing to undo.');
  assert.ok(!again.isConnected, 'the older plain copy is gone');
});

test('a popover closed in the tick it opened leaves no outside-press listener', (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const doc = installDOM();
  popover(h('div'), 10, 10);
  closePopover();
  t.mock.timers.tick(10);
  assert.equal(doc.listenerCount('pointerdown'), 0);

  // An open popover still closes on an outside press, and only itself.
  const a = h('div');
  popover(a, 10, 10);
  t.mock.timers.tick(10);
  assert.equal(doc.listenerCount('pointerdown'), 1);
  popover(h('div'), 10, 10); // replaces a before its successor's listener arms
  const b = h('div');
  popover(b, 10, 10);
  t.mock.timers.tick(10);
  assert.equal(doc.listenerCount('pointerdown'), 1, 'only the open popover listens');
  doc.dispatch('pointerdown', { target: b });
  assert.ok(b.isConnected, 'a press inside the open popover keeps it');
  doc.dispatch('pointerdown', { target: doc.body });
  assert.ok(!b.isConnected);
  assert.equal(doc.listenerCount('pointerdown'), 0);
});
