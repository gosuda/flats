import test from 'node:test';
import assert from 'node:assert/strict';
import { BoardState, activityCounts } from '../state.js';
import { textOp, moveOps } from '../ops.js';

const sticky = (id, inc, extra = {}) => ({
  id, type: 'sticky', x: 0, y: 0, w: 200, h: 200, rot: 0, z: 'V', locked: false, text: '', inc, v: { geom: inc, text: inc }, ...extra,
});

function seeded() {
  const s = new BoardState();
  s.session = 'S';
  s.loadSnapshot({ seq: 10, elements: [sticky('e_aaaaaaaaaa', 3), sticky('e_bbbbbbbbbb', 4)] });
  return s;
}

test('deltas apply in seq order; gaps and old seqs are not applied', () => {
  const s = seeded();
  assert.equal(s.applyDelta({ seq: 11, changes: [{ id: 'e_aaaaaaaaaa', inc: 3, set: { text: ['a', 11] } }] }).status, 'applied');
  assert.equal(s.server.get('e_aaaaaaaaaa').text, 'a');
  assert.equal(s.server.get('e_aaaaaaaaaa').v.text, 11);
  assert.equal(s.applyDelta({ seq: 13, changes: [{ id: 'e_aaaaaaaaaa', inc: 3, set: { text: ['c', 13] } }] }).status, 'gap');
  assert.equal(s.server.get('e_aaaaaaaaaa').text, 'a');
  assert.equal(s.applyDelta({ seq: 11, changes: [{ id: 'e_aaaaaaaaaa', inc: 3, set: { text: ['x', 11] } }] }).status, 'old');
  assert.equal(s.watermark, 11);
});

test('an ack removes the pending batch and never changes the server copy', () => {
  const s = seeded();
  const op = { op: 'set', id: 'e_aaaaaaaaaa', inc: 3, g: 'geom', val: { x: 50, y: 0, w: 200, h: 200, rot: 0 } };
  const { batch } = s.push([op]);
  assert.equal(s.get('e_aaaaaaaaaa').x, 50, 'optimistic display');
  assert.equal(s.server.get('e_aaaaaaaaaa').x, 0);
  s.ack(batch.n);
  assert.equal(s.pending.length, 0);
  assert.equal(s.server.get('e_aaaaaaaaaa').x, 0, 'ack leaves the server copy alone');
  assert.equal(s.get('e_aaaaaaaaaa').x, 0, 'screen equals the server copy once pending is empty');
});

test('a lost ack and a later overwrite do not drift from the delta order', () => {
  const s = seeded();
  const { batch } = s.push([{ op: 'set', id: 'e_aaaaaaaaaa', inc: 3, g: 'text', val: 'mine' }]);
  s.applyDelta({ seq: 11, from: `S:${batch.n}`, changes: [{ id: 'e_aaaaaaaaaa', inc: 3, set: { text: ['mine', 11] } }] });
  assert.equal(s.pending.length, 0, 'own delta removes the batch');
  s.applyDelta({ seq: 12, changes: [{ id: 'e_aaaaaaaaaa', inc: 3, set: { text: ['theirs', 12] } }] });
  assert.equal(s.get('e_aaaaaaaaaa').text, 'theirs');
});

test('incarnation 0 for elements this session created, real incarnation after the delta', () => {
  const s = seeded();
  const el = sticky('e_cccccccccc', undefined);
  delete el.inc; delete el.v;
  const { batch } = s.push([{ op: 'add', el }]);
  assert.equal(s.incOf('e_cccccccccc'), 0);
  assert.equal(textOp(s, s.get('e_cccccccccc'), 'hi').inc, 0);
  s.applyDelta({ seq: 11, from: `S:${batch.n}`, changes: [{ id: 'e_cccccccccc', inc: 11, add: { ...el, inc: 11, v: { geom: 11 } } }] });
  assert.equal(s.incOf('e_cccccccccc'), 11);
  assert.equal(textOp(s, s.get('e_cccccccccc'), 'hi').inc, 11);
  assert.equal(s.incOf('e_dddddddddd'), null);
});

test('a permanent reject drops the batch; a transient one keeps it', () => {
  const s = seeded();
  const a = s.push([{ op: 'set', id: 'e_aaaaaaaaaa', inc: 3, g: 'text', val: 'x' }]).batch;
  s.reject(a.n, true);
  assert.equal(s.pending.length, 1);
  assert.equal(s.get('e_aaaaaaaaaa').text, 'x');
  s.reject(a.n, false);
  assert.equal(s.pending.length, 0);
  assert.equal(s.get('e_aaaaaaaaaa').text, '', 'optimistic display undone');
});

test('a reset snapshot replaces the server copy and keeps pending batches', () => {
  const s = seeded();
  s.push([{ op: 'set', id: 'e_bbbbbbbbbb', inc: 4, g: 'text', val: 'pending' }]);
  s.myText.set('e_bbbbbbbbbb', { v: 9, text: 'old' });
  s.loadSnapshot({ seq: 40, reset: true, elements: [sticky('e_bbbbbbbbbb', 4, { text: 'rolled back' })] });
  assert.equal(s.server.has('e_aaaaaaaaaa'), false);
  assert.equal(s.watermark, 40);
  assert.equal(s.pending.length, 1);
  assert.equal(s.get('e_bbbbbbbbbb').text, 'pending');
  assert.equal(s.myText.size, 0);
});

test('a reset delta clears remembered text writes', () => {
  const s = seeded();
  s.myText.set('e_aaaaaaaaaa', { v: 9, text: 'x' });
  const r = s.applyDelta({ seq: 11, reset: { kind: 'rollback', of: 3 }, changes: [{ id: 'e_bbbbbbbbbb', del: 11 }] });
  assert.deepEqual(r.reset, { kind: 'rollback', of: 3 });
  assert.equal(s.myText.size, 0);
  assert.equal(s.get('e_bbbbbbbbbb'), null);
});

test('overwritten text: concurrent overwrite reports, sequential edit does not', () => {
  const s = seeded();
  const { batch } = s.push([{ op: 'set', id: 'e_aaaaaaaaaa', inc: 3, g: 'text', val: 'my words', base_v: 3 }]);
  s.applyDelta({ seq: 11, from: `S:${batch.n}`, changes: [{ id: 'e_aaaaaaaaaa', inc: 3, set: { text: ['my words', 11] } }] });
  const concurrent = s.applyDelta({ seq: 12, from: 'T:4', changes: [{ id: 'e_aaaaaaaaaa', inc: 3, set: { text: ['their words', 12] }, overwrote: 11 }] });
  assert.deepEqual(concurrent.overwritten, [{ id: 'e_aaaaaaaaaa', text: 'my words' }]);
  const sequential = s.applyDelta({ seq: 13, from: 'T:5', changes: [{ id: 'e_aaaaaaaaaa', inc: 3, set: { text: ['later', 13] } }] });
  assert.deepEqual(sequential.overwritten, []);
});

test('board metadata ignores unknown keys such as __proto__', () => {
  const s = seeded();
  const d = JSON.parse('{"seq":11,"changes":[{"id":"_board","set":{"__proto__":[{"polluted":true},11],"title":["T",11]}}]}');
  s.applyDelta(d);
  assert.equal(s.meta.title, 'T');
  assert.equal(Object.getPrototypeOf(s.meta), Object.prototype);
  assert.equal(s.meta.polluted, undefined);
  assert.ok(!Object.prototype.hasOwnProperty.call(s.metaV, '__proto__'));
  s.push([JSON.parse('{"op":"meta","g":"__proto__","val":{"polluted":true}}')]);
  assert.equal(s.viewMeta().polluted, undefined);
});

test('public deltas carry values without versions', () => {
  const s = new BoardState();
  s.public = true;
  s.loadSnapshot({ seq: 5, elements: [{ id: 'e_aaaaaaaaaa', type: 'text', x: 0, y: 0, w: 10, h: 10, rot: 0, z: 'V', text: 'a' }] });
  s.applyDelta({ seq: 6, changes: [{ id: 'e_aaaaaaaaaa', set: { text: 'b' } }, { id: '_board', set: { title: 'T' } }] });
  assert.equal(s.get('e_aaaaaaaaaa').text, 'b');
  assert.equal(s.meta.title, 'T');
  s.applyDelta({ seq: 7, changes: [] });
  assert.equal(s.watermark, 7, 'empty public deltas still advance the watermark');
});

test('optimistic frame move carries members, locked ones too', () => {
  const s = new BoardState();
  s.session = 'S';
  s.loadSnapshot({ seq: 1, elements: [
    { id: 'e_frame00001', type: 'frame', x: 0, y: 0, w: 500, h: 500, rot: 0, z: '1', text: 'F', inc: 1, v: {} },
    sticky('e_member0001', 1, { frame: 'e_frame00001', x: 10, y: 10 }),
    sticky('e_member0002', 1, { frame: 'e_frame00001', x: 20, y: 20, locked: true }),
  ] });
  s.push(moveOps(s, ['e_frame00001'], 100, 0));
  assert.equal(s.get('e_frame00001').x, 100);
  assert.equal(s.get('e_member0001').x, 110);
  assert.equal(s.get('e_member0002').x, 120);
});

test('optimistic delete detaches connector ends at their last position', () => {
  const s = new BoardState();
  s.loadSnapshot({ seq: 1, elements: [
    sticky('e_aaaaaaaaaa', 1, { x: 100, y: 100, w: 100, h: 100 }),
    { id: 'e_conn000001', type: 'connector', z: 'W', ends: [{ el: 'e_aaaaaaaaaa', at: [1, 0.5] }, { pt: [500, 500] }], inc: 1, v: {} },
  ] });
  s.push([{ op: 'delete', ids: ['e_aaaaaaaaaa'], inc_of: { e_aaaaaaaaaa: 1 } }]);
  assert.equal(s.get('e_aaaaaaaaaa'), null);
  assert.deepEqual(s.get('e_conn000001').ends[0], { pt: [200, 150] });
});

test('an ack that follows its own delta still returns the batch for notices', () => {
  const s = seeded();
  const { batch } = s.push([{ op: 'undo' }]);
  batch.undo = 'undo';
  s.applyDelta({ seq: 11, from: `S:${batch.n}`, changes: [{ id: 'e_aaaaaaaaaa', inc: 3, set: { text: ['x', 11] } }] });
  assert.equal(s.pending.length, 0);
  const r = s.ack(batch.n);
  assert.equal(r.batch, batch);
  assert.equal(s.ack(batch.n).batch, null);
});

test('a same-incarnation revive replaces the element and reports it as an update', () => {
  const s = seeded();
  const { batch } = s.push([{ op: 'set', id: 'e_aaaaaaaaaa', inc: 3, g: 'text', val: 'mine' }]);
  s.applyDelta({ seq: 11, from: `S:${batch.n}`, changes: [{ id: 'e_aaaaaaaaaa', inc: 3, set: { text: ['mine', 11] } }] });
  assert.ok(s.myText.has('e_aaaaaaaaaa'));
  // A reset that rewrote only author fields keeps e_a at incarnation 3; e_c
  // comes back at a new incarnation the client never had.
  const d = {
    seq: 12, reset: { kind: 'rollback', of: 2 }, by: { kind: 'agent', label: 'Agent' },
    changes: [
      { id: 'e_aaaaaaaaaa', revive: sticky('e_aaaaaaaaaa', 3, { text: 'mine', by: 'agent' }) },
      { id: 'e_cccccccccc', revive: sticky('e_cccccccccc', 12) },
    ],
  };
  const r = s.applyDelta(d);
  assert.equal(r.status, 'applied');
  assert.equal(s.server.get('e_aaaaaaaaaa').by, 'agent', 'the revive replaced the element');
  assert.ok(r.changed.has('e_aaaaaaaaaa'));
  assert.ok(r.changed.has('e_cccccccccc'));
  assert.deepEqual([...r.replaced], ['e_aaaaaaaaaa']);
  assert.equal(s.myText.size, 0, 'a reset clears remembered text writes');
  assert.deepEqual(activityCounts(d, r), { added: 1, changed: 1, removed: 0 });
});

test('a revive at a new incarnation of an element the client has is an addition', () => {
  const s = seeded();
  const d = { seq: 11, reset: { kind: 'rollback', of: 2 }, changes: [{ id: 'e_aaaaaaaaaa', revive: sticky('e_aaaaaaaaaa', 11) }] };
  const r = s.applyDelta(d);
  assert.equal(s.server.get('e_aaaaaaaaaa').inc, 11);
  assert.equal(r.replaced.size, 0);
  assert.deepEqual(activityCounts(d, r), { added: 1, changed: 0, removed: 0 });
});

test('optimistic delete clamps a detached end to the coordinate range, as the server does', () => {
  const MAX = 10_000_000;
  const s = new BoardState();
  s.loadSnapshot({ seq: 1, elements: [
    sticky('e_aaaaaaaaaa', 1, { x: MAX - 50, y: 100, w: 100, h: 100 }),
    sticky('e_bbbbbbbbbb', 1, { x: 0, y: 0, w: 100, h: 100, rot: 30 }),
    { id: 'e_conn000001', type: 'connector', z: 'W', ends: [{ el: 'e_aaaaaaaaaa', at: [1, 0.5] }, { el: 'e_bbbbbbbbbb', at: [1, 0] }], inc: 1, v: {} },
  ] });
  s.push([{ op: 'delete', ids: ['e_aaaaaaaaaa', 'e_bbbbbbbbbb'], inc_of: { e_aaaaaaaaaa: 1, e_bbbbbbbbbb: 1 } }]);
  const ends = s.get('e_conn000001').ends;
  assert.deepEqual(ends[0], { pt: [MAX, 150] });
  // The rotated corner is stored rounded to one decimal place (Appendix A).
  const r = (v) => Math.round(v * 10) / 10;
  const c = 50, rad = (30 * Math.PI) / 180;
  assert.deepEqual(ends[1], { pt: [r(c + 50 * Math.cos(rad) + 50 * Math.sin(rad)), r(c + 50 * Math.sin(rad) - 50 * Math.cos(rad))] });
});

test('a group move clamps a member connector free end in the optimistic copy', () => {
  const MAX = 10_000_000;
  const s = new BoardState();
  s.session = 'S';
  s.loadSnapshot({ seq: 1, elements: [
    sticky('e_aaaaaaaaaa', 1, { group: 'g_aaaaaaaaaa', x: 0, y: 0 }),
    { id: 'e_conn000001', type: 'connector', z: 'W', group: 'g_aaaaaaaaaa', ends: [{ pt: [MAX - 10, 0] }, { pt: [0, 0] }], inc: 1, v: {} },
  ] });
  s.push(moveOps(s, ['e_aaaaaaaaaa'], 30, 0));
  assert.deepEqual(s.get('e_conn000001').ends, [{ pt: [MAX, 0] }, { pt: [30, 0] }]);
});
