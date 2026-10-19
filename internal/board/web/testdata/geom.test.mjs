import test from 'node:test';
import assert from 'node:assert/strict';
import { existsSync, readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { endpointWorld, connectorPoints, rdp, penFromPoints, penWorldPoints, elementBounds, attachAt } from '../geom.js';

// The server keeps numbers to one decimal place (Appendix A, Num).
const near = (got, want, tol = 0.051) =>
  assert.ok(Math.abs(got[0] - want[0]) <= tol && Math.abs(got[1] - want[1]) <= tol, `got ${got}, want ${want}`);

const OWN = [
  { el: { x: 100, y: 50, w: 200, h: 100, rot: 0 }, at: [0, 0], want: [100, 50] },
  { el: { x: 100, y: 50, w: 200, h: 100, rot: 0 }, at: [1, 0.5], want: [300, 100] },
  { el: { x: 0, y: 0, w: 100, h: 100, rot: 90 }, at: [1, 0.5], want: [50, 100] },
  { el: { x: 0, y: 0, w: 100, h: 100, rot: 180 }, at: [0, 0], want: [100, 100] },
  { el: { x: -40, y: 10, w: 80, h: 20, rot: 270 }, at: [1, 0.5], want: [0, -20] },
  { el: { x: 0, y: 0, w: 200, h: 100, rot: 45 }, at: [1, 0.5], want: [170.7, 120.7] },
];

test('endpointWorld matches our vectors', () => {
  for (const v of OWN) near(endpointWorld(v.el, v.at), v.want);
});

const shared = fileURLToPath(new URL('../../model/testdata/geometry.json', import.meta.url));
test('endpointWorld matches the Go vectors', { skip: !existsSync(shared) && 'geometry.json not written yet' }, () => {
  const { endpoints } = JSON.parse(readFileSync(shared, 'utf8'));
  assert.ok(endpoints.length > 0);
  for (const v of endpoints) near(endpointWorld(v.el, v.at), v.want);
});

test('connectors: straight and elbow routes', () => {
  const els = new Map([
    ['e_a', { id: 'e_a', type: 'shape', x: 0, y: 0, w: 100, h: 100, rot: 0 }],
    ['e_b', { id: 'e_b', type: 'shape', x: 300, y: 200, w: 100, h: 100, rot: 0 }],
  ]);
  const get = (id) => els.get(id);
  const straight = { type: 'connector', ends: [{ el: 'e_a', at: [1, 0.5] }, { el: 'e_b', at: [0, 0.5] }] };
  assert.deepEqual(connectorPoints(straight, get), [[100, 50], [300, 250]]);
  const elbow = { ...straight, style: { route: 'elbow' } };
  assert.deepEqual(connectorPoints(elbow, get), [[100, 50], [200, 50], [200, 250], [300, 250]]);
  const down = { type: 'connector', style: { route: 'elbow' }, ends: [{ el: 'e_a', at: [0.5, 1] }, { el: 'e_b', at: [0, 0.5] }] };
  assert.deepEqual(connectorPoints(down, get), [[50, 100], [50, 250], [300, 250]]);
  const dangling = { type: 'connector', ends: [{ el: 'e_gone', at: [0, 0] }, { pt: [1, 1] }] };
  assert.equal(connectorPoints(dangling, get), null);
  const b = elementBounds(straight, get);
  assert.ok(b.x <= 100 && b.x + b.w >= 300);
});

test('RDP removes points within the tolerance and keeps corners', () => {
  const line = Array.from({ length: 50 }, (_, i) => [i, i % 2 ? 0.2 : -0.2]);
  assert.equal(rdp(line, 0.5).length, 2);
  const corner = [[0, 0], [5, 0.1], [10, 0], [10, 5], [10, 10]];
  assert.deepEqual(rdp(corner, 0.5), [[0, 0], [10, 0], [10, 10]]);
});

test('pen points are normalized integers inside the element box, at most 2,000', () => {
  const pts = Array.from({ length: 5000 }, (_, i) => [i, Math.sin(i / 3) * 40]);
  const p = penFromPoints(pts, 0.5);
  assert.ok(p.pts.length / 2 <= 2000);
  for (const v of p.pts) assert.ok(Number.isInteger(v) && v >= 0 && v <= 10000);
  const world = penWorldPoints(p);
  assert.ok(Math.abs(world[0][0] - pts[0][0]) < 1);
  const dot = penFromPoints([[5, 5]], 0.5);
  assert.equal(dot.pts.length, 4, 'a dot is stored as two points');
});

test('attachAt snaps to side midpoints and clamps', () => {
  const el = { x: 0, y: 0, w: 100, h: 100, rot: 0 };
  assert.deepEqual(attachAt(el, 99, 52), [1, 0.5]);
  assert.deepEqual(attachAt(el, 10, 10), [0.1, 0.1]);
  assert.deepEqual(attachAt(el, -50, 10), [0, 0.1]);
});
