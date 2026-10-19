import test from 'node:test';
import assert from 'node:assert/strict';
import { SpatialIndex } from '../index.js';

test('grid query finds intersecting elements and forgets removed ones', () => {
  const ix = new SpatialIndex();
  ix.set('a', { x: 10, y: 10, w: 100, h: 100 });
  ix.set('b', { x: 2000, y: 2000, w: 100, h: 100 });
  ix.set('c', { x: 500, y: 500, w: 30, h: 30 }); // spans 4 cells
  assert.deepEqual([...ix.query({ x: 0, y: 0, w: 600, h: 600 })].sort(), ['a', 'c']);
  assert.deepEqual([...ix.query({ x: 1990, y: 1990, w: 5, h: 5 })], []);
  assert.deepEqual([...ix.query({ x: 2050, y: 2050, w: 5, h: 5 })], ['b']);
  const old = ix.set('a', { x: 3000, y: 0, w: 10, h: 10 });
  assert.deepEqual(old, { x: 10, y: 10, w: 100, h: 100 });
  assert.deepEqual([...ix.query({ x: 0, y: 0, w: 200, h: 200 })], []);
  ix.set('b', null);
  assert.equal(ix.bounds('b'), null);
  assert.deepEqual([...ix.query({ x: 0, y: 0, w: 1e6, h: 1e6 })].sort(), ['a', 'c']);
});

test('elements spanning more than 4 cells go to the large list', () => {
  const ix = new SpatialIndex();
  ix.set('frame', { x: 0, y: 0, w: 5000, h: 3000 });
  assert.ok(ix.large.has('frame'));
  assert.equal(ix.cells.size, 0);
  assert.deepEqual([...ix.query({ x: 4000, y: 2000, w: 1, h: 1 })], ['frame']);
  assert.deepEqual([...ix.query({ x: 6000, y: 0, w: 1, h: 1 })], []);
  ix.set('frame', { x: 0, y: 0, w: 100, h: 100 });
  assert.ok(!ix.large.has('frame'));
});

test('connector reverse index follows reattachment', () => {
  const ix = new SpatialIndex();
  ix.setConnector('c1', ['a', 'b']);
  ix.setConnector('c2', ['a']);
  assert.deepEqual([...ix.connectorsOf('a')].sort(), ['c1', 'c2']);
  ix.setConnector('c1', ['b']);
  assert.deepEqual([...ix.connectorsOf('a')], ['c2']);
  ix.setConnector('c2', null);
  assert.equal(ix.connectorsOf('a').size, 0);
  assert.deepEqual([...ix.connectorsOf('b')], ['c1']);
});
