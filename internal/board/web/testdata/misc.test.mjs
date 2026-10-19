import test from 'node:test';
import assert from 'node:assert/strict';
import { THEMES, contrast, frameFill } from '../theme.js';
import { STICKY_COLORS, INK_COLORS } from '../schema.js';
import { keyBetween, keysBetween, newId } from '../ids.js';
import { findMatches } from '../find.js';
import { BoardState } from '../state.js';
import { buildRecovery, recoveryZip } from '../recovery.js';
import { readZipIndex, readZipEntry, crc32 } from '../zip.js';

const AA = 4.5;

for (const [name, t] of Object.entries(THEMES)) {
  test(`${name} theme: text on every sticky color and shape fill reaches 4.5:1`, () => {
    for (const c of STICKY_COLORS) {
      assert.ok(contrast(t.ink, t.sticky[c]) >= AA, `${name} ink on ${c}: ${contrast(t.ink, t.sticky[c]).toFixed(2)}`);
      assert.ok(contrast(t.by, t.sticky[c]) >= AA, `${name} author on ${c}: ${contrast(t.by, t.sticky[c]).toFixed(2)}`);
    }
    for (const [f, bg] of Object.entries(t.fill)) assert.ok(contrast(t.ink, bg) >= AA, `${name} ink on fill ${f}`);
  });

  test(`${name} theme: every ink color reaches 4.5:1 on the canvas, paper and frame fills`, () => {
    const grounds = { canvas: t.canvas, paper: t.paper, frameWhite: frameFill(t, 'white'), frameGray: frameFill(t, 'gray'), white: t.fill.white };
    for (const ink of INK_COLORS) {
      for (const [g, bg] of Object.entries(grounds)) {
        const c = contrast(t.inks[ink], bg);
        assert.ok(c >= AA, `${name} ${ink} on ${g}: ${c.toFixed(2)}`);
      }
    }
    assert.ok(contrast(t.muted, t.paper) >= AA && contrast(t.muted, t.canvas) >= AA, `${name} muted UI text`);
  });
}

test('fractional keys stay ordered and never end in 0', () => {
  let keys = [keyBetween(null, null)];
  for (let i = 0; i < 200; i++) {
    const j = Math.floor((i * 7919) % (keys.length + 1));
    const k = keyBetween(j ? keys[j - 1] : null, j < keys.length ? keys[j] : null);
    keys.splice(j, 0, k);
  }
  for (let i = 1; i < keys.length; i++) assert.ok(keys[i - 1] < keys[i], `${keys[i - 1]} < ${keys[i]}`);
  for (const k of keys) assert.ok(!k.endsWith('0'));
  const after = keysBetween('V', null, 100);
  assert.ok(after.every((k, i) => k > (i ? after[i - 1] : 'V')));
  assert.ok(/^e_[0-9A-Za-z]{10}$/.test(newId()));
});

test('find matches text, labels, frame names and alt text in reading order', () => {
  const els = [
    { id: 'a', type: 'sticky', x: 300, y: 0, w: 10, h: 10, text: 'Review queue' },
    { id: 'b', type: 'frame', x: 0, y: 500, w: 10, h: 10, text: 'reviews' },
    { id: 'c', type: 'image', x: 0, y: 0, w: 10, h: 10, alt: 'Chart of REVIEW time' },
    { id: 'd', type: 'connector', text: 'no match' },
    { id: 'e', type: 'pen', x: 0, y: 0, w: 1, h: 1 },
  ];
  const centerOf = (el) => [el.x ?? 0, el.y ?? 0];
  assert.deepEqual(findMatches(els, 'review', centerOf), ['c', 'a', 'b']);
  assert.deepEqual(findMatches(els, '   ', centerOf), []);
  assert.deepEqual(findMatches(els, 'missing', centerOf), []);
});

test('recovery file holds created and changed elements, deleted ones as a list, and image bytes', async () => {
  const s = new BoardState();
  s.session = 'S';
  s.loadSnapshot({ seq: 1, elements: [
    { id: 'e_keep000001', type: 'sticky', x: 0, y: 0, w: 100, h: 100, rot: 0, z: 'a', text: 'before', inc: 1, v: {} },
    { id: 'e_gone000001', type: 'sticky', x: 0, y: 0, w: 100, h: 100, rot: 0, z: 'b', text: 'bye', inc: 1, v: {} },
    { id: 'e_same000001', type: 'text', x: 0, y: 0, w: 100, h: 100, rot: 0, z: 'c', text: 'same', inc: 1, v: {} },
  ] });
  s.push([
    { op: 'set', id: 'e_keep000001', inc: 1, g: 'text', val: 'after' },
    { op: 'delete', ids: ['e_gone000001'], inc_of: { e_gone000001: 1 } },
    { op: 'add', el: { id: 'e_new0000001', type: 'image', x: 5, y: 5, w: 10, h: 10, rot: 0, z: 'd', asset: 'aa', alt: 'chart', natural: [10, 10] } },
    { op: 'add', el: { id: 'e_new0000002', type: 'image', x: 5, y: 5, w: 10, h: 10, rot: 0, z: 'e', asset: 'bb', alt: 'lost', natural: [10, 10] } },
  ]);
  const rec = buildRecovery(s);
  assert.deepEqual(rec.elements.map((e) => e.id), ['e_keep000001', 'e_new0000001', 'e_new0000002']);
  assert.equal(rec.elements[0].recovered_from, 'e_keep000001');
  assert.equal(rec.elements[0].text, 'after');
  assert.equal(rec.elements[0].inc, undefined, 'runtime fields are left out');
  assert.deepEqual(rec.deleted, [{ id: 'e_gone000001', text: 'bye' }]);
  const png = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 1, 2, 3]);
  const zip = await recoveryZip(rec, { title: 'Retro', grid: true }, async (h) => (h === 'aa' ? png : null), new Date(2026, 9, 19, 15, 40));
  const index = readZipIndex(zip);
  assert.deepEqual([...index.keys()].sort(), ['assets/aa.png', 'board.json', 'elements.json', 'recovery.json']);
  const text = async (n) => new TextDecoder().decode(await readZipEntry(zip, index.get(n)));
  const recovery = JSON.parse(await text('recovery.json'));
  assert.deepEqual(recovery.missing_assets, ['bb']);
  assert.deepEqual(recovery.deleted, [{ id: 'e_gone000001', text: 'bye' }]);
  assert.equal(JSON.parse(await text('board.json')).format, 1);
  assert.deepEqual(await readZipEntry(zip, index.get('assets/aa.png')), png);
});

test('crc32 matches the reference value', () => {
  assert.equal(crc32(new TextEncoder().encode('123456789')), 0xcbf43926);
});
