'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { View } from '../state.js';
import * as act from '../actions.js';
import { page, block, para, run, pid, bid } from './page.mjs';

const view = (...items) => new View(new Map(items.map((i) => [i.ref.id, i])));
const R = (tag, inc = '5') => ({ id: tag.startsWith('p_') ? tag : bid(tag), inc });

test('proposed ids use the kind prefix and canonical base32', () => {
  const seen = new Set();
  for (let i = 0; i < 200; i++) {
    const id = act.newId('block');
    assert.match(id, /^b_[a-z2-7]{25}[aeimquy4]$/, 'the last character carries two zero padding bits');
    seen.add(id);
  }
  assert.equal(seen.size, 200);
  assert.match(act.newId('property'), /^pr_[a-z2-7]{26}$/);
});

test('Enter in the middle splits into one unit: set the head, create the tail after the block', () => {
  const v = view(page('a'), block('x', 'a', { kind: 'paragraph', text: [run('hello', 'bold'), run(' world')] }, { v: { content: '8' }, inc: '5' }));
  const plan = act.enter(v, v.get(bid('x')), v.get(bid('x')).content.text, 3);
  const [set, create] = plan.ops;
  assert.equal(plan.ops.length, 2);
  assert.ok(set.unit_id && set.unit_id === create.unit_id, 'one unit_id on every operation');
  assert.deepEqual(set.structure, [{ block: R('x'), page: R(pid('a')), parent: null }]);
  assert.equal(create.structure, undefined, 'structure only on the first operation');
  assert.deepEqual(set, { op: 'set_block', block: R('x'), content: { kind: 'paragraph', text: [run('hel', 'bold')] }, base_v: '8', unit_id: set.unit_id, structure: set.structure });
  assert.equal(create.op, 'create_block');
  assert.deepEqual(create.place, { page: R(pid('a')), parent: null, after: R('x') });
  assert.deepEqual(create.content, { kind: 'paragraph', text: [run('lo', 'bold'), run(' world')] });
  assert.equal(plan.focus.block, create.proposed_id);
});

test('Enter continues a list and keeps the split item its own fields', () => {
  const v = view(page('a'), block('t', 'a', { kind: 'todo', text: [run('ab')], checked: true }));
  const { ops } = act.enter(v, v.get(bid('t')), v.get(bid('t')).content.text, 2);
  assert.deepEqual(ops[0].content, { kind: 'todo', text: [run('ab')], checked: true });
  assert.deepEqual(ops[1].content, { kind: 'todo', text: [], checked: false });
});

test('Enter in an empty list item ends the list', () => {
  const v = view(page('a'), block('l', 'a', para('', 'bullet')));
  const { ops } = act.enter(v, v.get(bid('l')), [], 0);
  assert.deepEqual(ops, [{ op: 'turn_blocks', blocks: [R('l')], kind: 'paragraph' }]);
});

test('Enter in a heading creates a paragraph below', () => {
  const v = view(page('a'), block('h', 'a', para('Title', 'heading1')));
  const { ops } = act.enter(v, v.get(bid('h')), v.get(bid('h')).content.text, 5);
  assert.equal(ops[1].content.kind, 'paragraph');
});

test('Backspace at the start merges into the previous block as one guarded unit', () => {
  const v = view(page('a'),
    block('p', 'a', { kind: 'paragraph', text: [run('ab', 'italic')] }, { position: 'A', v: { content: '7' } }),
    block('q', 'a', para('cd', 'bullet'), { position: 'B' }),
    block('k', 'a', para('kid'), { parent: 'q', position: 'V' }));
  const q = v.get(bid('q'));
  const plan = act.backspace(v, q, q.content.text);
  const [set, move, del] = plan.ops;
  assert.deepEqual(plan.ops.map((o) => o.op), ['set_block', 'move_blocks', 'delete_blocks']);
  assert.ok(new Set(plan.ops.map((o) => o.unit_id)).size === 1 && set.unit_id);
  assert.deepEqual(set.content, { kind: 'paragraph', text: [run('ab', 'italic'), run('cd')] });
  assert.equal(set.base_v, '7');
  assert.deepEqual(set.structure, [{ block: R('q'), page: R(pid('a')), parent: null, after: R('p') }], 'merge guards the expected preceding sibling');
  assert.deepEqual(move, { op: 'move_blocks', blocks: [R('k')], place: { page: R(pid('a')), parent: null, after: R('p') }, unit_id: set.unit_id }, 'children move after a paragraph that cannot hold them');
  assert.deepEqual(del.blocks, [R('q')]);
  assert.deepEqual(plan.focus, { block: bid('p'), offset: 2 });
});

test('Backspace into the last child of a list item merges with that child', () => {
  const v = view(page('a'),
    block('l', 'a', para('item', 'bullet'), { position: 'A' }),
    block('c', 'a', para('child'), { parent: 'l' }),
    block('n', 'a', para('next'), { position: 'B' }));
  const n = v.get(bid('n'));
  const { ops } = act.backspace(v, n, n.content.text);
  assert.equal(ops[0].block.id, bid('c'));
  assert.deepEqual(ops[0].structure, [
    { block: R('n'), page: R(pid('a')), parent: null, after: R('l') },
    { block: R('c'), page: R(pid('a')), parent: R('l') },
  ]);
});

test('Backspace after a block without rich text selects it instead', () => {
  const v = view(page('a'), block('d', 'a', { kind: 'divider' }, { position: 'A' }), block('x', 'a', para('x'), { position: 'B' }));
  assert.deepEqual(act.backspace(v, v.get(bid('x')), v.get(bid('x')).content.text), { select: [bid('d')] });
  assert.equal(act.backspace(v, v.get(bid('d')), []), null, 'nothing before the first block');
});

test('Tab nests under the preceding list sibling; Shift+Tab moves out after the parent', () => {
  const v = view(page('a'), block('l', 'a', para('list', 'bullet'), { position: 'A' }), block('x', 'a', para('x', 'bullet'), { position: 'B' }));
  const tab = act.indent(v, [v.get(bid('x'))]);
  assert.equal(tab.ops.length, 1);
  assert.deepEqual(tab.ops[0].place, { page: R(pid('a')), parent: R('l'), before: null });
  assert.deepEqual(tab.ops[0].structure, [{ block: R('x'), page: R(pid('a')), parent: null, after: R('l') }]);
  assert.ok(tab.ops[0].unit_id);
  const nested = view(page('a'), block('l', 'a', para('list', 'bullet')), block('x', 'a', para('x', 'bullet'), { parent: 'l' }));
  const out = act.outdent(nested, [nested.get(bid('x'))]);
  assert.deepEqual(out.ops[0].place, { page: R(pid('a')), parent: null, after: R('l') });
  assert.equal(act.outdent(v, [v.get(bid('l'))]), null, 'a top-level block cannot unnest');
  const para0 = view(page('a'), block('p', 'a', para('p'), { position: 'A' }), block('x', 'a', para('x'), { position: 'B' }));
  assert.equal(act.indent(para0, [para0.get(bid('x'))]), null, 'a paragraph holds no children');
});

test('Tab refuses to nest beyond six levels', () => {
  const items = [page('a')];
  let parent = null;
  for (let d = 0; d < 6; d++) { items.push(block('l' + d, 'a', para('', 'bullet'), { parent, position: 'A' })); parent = 'l' + d; }
  items.push(block('x', 'a', para('', 'bullet'), { parent: 'l4', position: 'B' }));
  const v = view(...items);
  assert.equal(v.depth(v.get(bid('l5'))), 6);
  assert.deepEqual(act.indent(v, [v.get(bid('x'))]), { refused: 'depth' });
});

test('turn targets are the intersection of what every selected block can take', () => {
  const v = view(page('a'), block('p', 'a', para('p')), block('c', 'a', { kind: 'code', text: '', language: '' }), block('d', 'a', { kind: 'divider' }));
  assert.ok(act.turnTargets([v.get(bid('p'))]).includes('code'));
  assert.ok(!act.turnTargets([v.get(bid('p'))]).includes('paragraph'));
  assert.ok(!act.turnTargets([v.get(bid('p')), v.get(bid('c'))]).includes('code'));
  assert.deepEqual(act.turnTargets([v.get(bid('p')), v.get(bid('d'))]), []);
});

test('a divider shortcut creates the divider before the emptied block in one unit', () => {
  const v = view(page('a'), block('x', 'a', para('---')));
  const { ops } = act.shortcut(v, v.get(bid('x')), 'divider');
  assert.deepEqual(ops.map((o) => o.op), ['create_block', 'set_block']);
  assert.equal(ops[0].unit_id, ops[1].unit_id);
  assert.deepEqual(ops[0].place.before, R('x'));
  assert.deepEqual(ops[1].content, { kind: 'paragraph', text: [] });
  const h = act.shortcut(v, v.get(bid('x')), 'heading2');
  assert.deepEqual(h.ops[0].content, { kind: 'heading2', text: [] });
});

test('roots keep only selected ancestors, in document order', () => {
  const v = view(page('a'), block('l', 'a', para('', 'bullet'), { position: 'A' }), block('c', 'a', para('c'), { parent: 'l' }), block('z', 'a', para('z'), { position: 'B' }));
  assert.deepEqual(act.roots(v, [bid('z'), bid('c'), bid('l')]).map((b) => b.ref.id), [bid('l'), bid('z')]);
  assert.deepEqual(act.duplicate(act.roots(v, [bid('l'), bid('z')])), [{ op: 'duplicate_blocks', blocks: [R('l'), R('z')], after: R('z') }]);
});

test('moving a selection by one uses semantic anchors', () => {
  const v = view(page('a'), block('a1', 'a', para('1'), { position: 'A' }), block('a2', 'a', para('2'), { position: 'B' }), block('a3', 'a', para('3'), { position: 'C' }));
  assert.deepEqual(act.moveBy(v, [v.get(bid('a2'))], -1)[0].place, { page: R(pid('a')), parent: null, before: R('a1') });
  assert.deepEqual(act.moveBy(v, [v.get(bid('a2'))], 1)[0].place, { page: R(pid('a')), parent: null, after: R('a3') });
  assert.equal(act.moveBy(v, [v.get(bid('a1'))], -1), null);
});

test('Keep my text inserts a new block after the original, or on a surviving page', () => {
  const v = view(page('a'), block('x', 'a', para('theirs')));
  const kept = act.keepMyText(v, bid('x'), para('mine'), pid('a'));
  assert.equal(kept.ops[0].op, 'create_block');
  assert.deepEqual(kept.ops[0].place.after, R('x'));
  assert.deepEqual(kept.ops[0].content, para('mine'));
  const gone = act.keepMyText(v, bid('gone'), para('mine'), pid('a'));
  assert.deepEqual(gone.ops[0].place, { page: R(pid('a')), parent: null, before: null });
});

test('a new database proposes all four identities', () => {
  const v = view(page('a'), block('x', 'a', para('')));
  const { ops, ids } = act.newDatabase(v, v.get(bid('x')));
  assert.equal(new Set(Object.values(ids)).size, 4);
  assert.equal(ops[0].proposed_id, ids.database);
  assert.equal(ops[0].block_proposed_id, ids.block);
  assert.equal(ops[0].title_property_proposed_id, ids.title);
  assert.equal(ops[0].view_proposed_id, ids.view);
});
