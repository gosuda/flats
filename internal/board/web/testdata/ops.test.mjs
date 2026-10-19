import test from 'node:test';
import assert from 'node:assert/strict';
import { BoardState } from '../state.js';
import { alignOps, distributeOps, moveOps, copyPayload, pasteOps, quickCreateOps, deleteOps, zorderOps } from '../ops.js';

const base = (id, extra) => ({ id, type: 'sticky', x: 0, y: 0, w: 100, h: 100, rot: 0, z: 'V', locked: false, text: '', inc: 2, v: {}, ...extra });

function board(...els) {
  const s = new BoardState();
  s.session = 'S';
  s.loadSnapshot({ seq: 1, elements: els });
  return s;
}

test('aligning a frame and one of its members moves the member once', () => {
  const s = board(
    base('e_frame00001', { type: 'frame', x: 100, y: 100, w: 600, h: 400, z: '1' }),
    base('e_member0001', { frame: 'e_frame00001', x: 300, y: 200 }),
    base('e_single0001', { x: 50, y: 700 }),
  );
  const ops = alignOps(s, ['e_frame00001', 'e_member0001', 'e_single0001'], 'left');
  assert.deepEqual(ops.map((o) => o.id), ['e_frame00001'], 'only the frame moves; the member follows it');
  assert.equal(ops[0].carry, 'members');
  assert.equal(ops[0].val.x, 50);
});

test('moving a group sends one set with carry group', () => {
  const s = board(
    base('e_grp0000001', { group: 'g_aaaaaaaaaa', x: 0 }),
    base('e_grp0000002', { group: 'g_aaaaaaaaaa', x: 200 }),
    base('e_alone00001', { x: 400 }),
  );
  const ops = moveOps(s, ['e_grp0000001', 'e_grp0000002', 'e_alone00001'], 10, 5);
  assert.equal(ops.length, 2);
  assert.equal(ops[0].carry, 'group');
  assert.deepEqual(ops[0].val, { x: 10, y: 5, w: 100, h: 100, rot: 0 });
  assert.equal(ops[1].id, 'e_alone00001');
  assert.equal(ops[1].carry, undefined);
});

test('locked elements are not moved directly', () => {
  const s = board(base('e_locked0001', { locked: true }));
  assert.deepEqual(moveOps(s, ['e_locked0001'], 10, 0), []);
  assert.deepEqual(deleteOps(s, ['e_locked0001']), []);
});

test('a drag ending inside another frame adds a reparent to the batch', () => {
  const s = board(
    base('e_frame00001', { type: 'frame', x: 0, y: 0, w: 300, h: 300, z: '1', inc: 5 }),
    base('e_frame00002', { type: 'frame', x: 1000, y: 0, w: 300, h: 300, z: '2', inc: 6 }),
    base('e_member0001', { frame: 'e_frame00001', x: 50, y: 50 }),
  );
  const ops = moveOps(s, ['e_member0001'], 1000, 0, { reparent: true });
  const rp = ops.find((o) => o.op === 'reparent');
  assert.deepEqual(rp, { op: 'reparent', ids: ['e_member0001'], inc_of: { e_member0001: 2 }, frame: 'e_frame00002', frame_inc: 6 });
  const out = moveOps(s, ['e_member0001'], 0, 2000, { reparent: true }).find((o) => o.op === 'reparent');
  assert.equal(out.frame, null);
  assert.equal(out.frame_inc, undefined);
});

test('distribute spaces three elements evenly', () => {
  const s = board(base('e_a000000001', { x: 0 }), base('e_b000000001', { x: 150 }), base('e_c000000001', { x: 500 }));
  const ops = distributeOps(s, ['e_a000000001', 'e_b000000001', 'e_c000000001'], 'h');
  assert.deepEqual(ops.map((o) => [o.id, o.val.x]), [['e_b000000001', 250]]);
});

test('paste relinks internal connectors and frees external ends', () => {
  const s = board(
    base('e_a000000001', { x: 0, y: 0 }),
    base('e_b000000001', { x: 300, y: 0 }),
    base('e_out0000001', { x: 600, y: 0 }),
    { id: 'e_conn000001', type: 'connector', z: 'W', locked: false, ends: [{ el: 'e_a000000001', at: [1, 0.5] }, { el: 'e_b000000001', at: [0, 0.5] }], inc: 2, v: {} },
    { id: 'e_conn000002', type: 'connector', z: 'X', locked: false, ends: [{ el: 'e_b000000001', at: [1, 0.5] }, { el: 'e_out0000001', at: [0, 0.5] }], inc: 2, v: {} },
  );
  const payload = copyPayload(s, ['e_a000000001', 'e_b000000001', 'e_conn000001', 'e_conn000002']);
  const { ops, ids } = pasteOps(s, payload, [200, 500]);
  assert.equal(ops.length, 4);
  assert.ok(ids.every((id) => /^e_[0-9A-Za-z]{10}$/.test(id) && !s.get(id)));
  const added = new Map(ops.map((o) => [o.el.id, o]));
  const conns = ops.filter((o) => o.el.type === 'connector');
  const inside = conns.find((o) => o.el.ends.every((e) => e.el));
  assert.ok(added.has(inside.el.ends[0].el) && added.has(inside.el.ends[1].el), 'internal connection joins the copies');
  assert.deepEqual(inside.refs, { [inside.el.ends[0].el]: 0, [inside.el.ends[1].el]: 0 });
  const outside = conns.find((o) => o !== inside);
  assert.ok(outside.el.ends[1].pt, 'connection to an element outside the range becomes a free end');
  assert.ok(added.has(outside.el.ends[0].el));
});

test('quick create adds a connected element of the same type 80px away', () => {
  const s = board(base('e_src0000001', { type: 'shape', x: 0, y: 0, w: 160, h: 100, style: { shape: 'round' } }));
  const { ops, el, conn } = quickCreateOps(s, s.get('e_src0000001'), 'right');
  assert.equal(el.type, 'shape');
  assert.equal(el.x, 240);
  assert.deepEqual(el.style, { shape: 'round' });
  assert.deepEqual(conn.ends, [{ el: 'e_src0000001', at: [1, 0.5] }, { el: el.id, at: [0, 0.5] }]);
  assert.deepEqual(ops[1].refs, { e_src0000001: 2, [el.id]: 0 });
});

test('bring forward goes just above the next overlapping element', () => {
  const s = board(base('e_a000000001', { z: 'a' }), base('e_b000000001', { z: 'b', x: 5000 }), base('e_c000000001', { z: 'c', x: 50 }));
  const [op] = zorderOps(s, ['e_a000000001'], 'forward');
  assert.deepEqual(op.to, { after: 'e_c000000001', inc: 2 });
});
