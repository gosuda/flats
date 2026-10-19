import test from 'node:test';
import assert from 'node:assert/strict';
import { installDOM } from './dom.mjs';
import { toast, popover, closePopover, h } from '../ui.js';

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
