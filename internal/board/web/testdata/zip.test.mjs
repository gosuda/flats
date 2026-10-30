import test from 'node:test';
import assert from 'node:assert/strict';
import { writeZip, readZipIndex, readZipEntry, crc32 } from '../zip.js';

// craftedZip builds a minimal single-entry archive with caller-chosen
// csize/size header fields, so a test can lie the way a malicious archive
// would: small compressed bytes, huge declared size.
function craftedZip(name, method, csize, size, data) {
  const nb = new TextEncoder().encode(name);
  const buf = new ArrayBuffer(30 + data.length + nb.length + 46 + nb.length + 22);
  const v = new DataView(buf);
  const u = new Uint8Array(buf);
  v.setUint32(0, 0x04034b50, true);
  v.setUint16(4, 20, true);
  v.setUint16(8, method, true);
  v.setUint32(18, csize, true);
  v.setUint32(22, size, true);
  v.setUint16(26, nb.length, true);
  u.set(nb, 30);
  u.set(data, 30 + nb.length);
  const p = 30 + data.length + nb.length;
  v.setUint32(p, 0x02014b50, true);
  v.setUint16(p + 4, 20, true);
  v.setUint16(p + 6, 20, true);
  v.setUint16(p + 10, method, true);
  v.setUint32(p + 20, csize, true);
  v.setUint32(p + 24, size, true);
  v.setUint16(p + 28, nb.length, true);
  v.setUint32(p + 42, 0, true);
  u.set(nb, p + 46);
  const e = p + 46 + nb.length;
  v.setUint32(e, 0x06054b50, true);
  v.setUint16(e + 8, 1, true);
  v.setUint16(e + 10, 1, true);
  v.setUint32(e + 12, 46 + nb.length, true);
  v.setUint32(e + 16, p, true);
  return u;
}

function deflateRaw(bytes) {
  return new Response(new Blob([bytes]).stream().pipeThrough(new CompressionStream('deflate-raw'))).arrayBuffer();
}

test('stored entry with huge declared size returns the csize bytes uninflated', async () => {
  const data = new TextEncoder().encode('tiny payload');
  const bytes = craftedZip('bomb.bin', 0, data.length, 0x7fffffff, data);
  const entry = readZipIndex(bytes).get('bomb.bin');
  assert.equal(entry.size, 0x7fffffff);
  assert.deepEqual(await readZipEntry(bytes, entry), data);
});

test('deflate entry inflating past its declared size is rejected', async () => {
  const raw = new Uint8Array(256).fill(7);
  const packed = new Uint8Array(await deflateRaw(raw));
  const bytes = craftedZip('bomb.bin', 8, packed.length, 4, packed);
  const entry = readZipIndex(bytes).get('bomb.bin');
  await assert.rejects(readZipEntry(bytes, entry), { message: 'zip entry exceeds declared size' });
});

test('small legit deflate entry round-trips', async () => {
  const raw = new TextEncoder().encode('hello board data');
  const packed = new Uint8Array(await deflateRaw(raw));
  const bytes = craftedZip('note.txt', 8, packed.length, raw.length, packed);
  const entry = readZipIndex(bytes).get('note.txt');
  const out = await readZipEntry(bytes, entry);
  assert.deepEqual(out, raw);
  assert.equal(crc32(out), crc32(raw));
});

test('writeZip archives round-trip through readZipIndex/readZipEntry', async () => {
  const files = [
    { name: 'board.json', data: '{"format":1}' },
    { name: 'assets/a.bin', data: new Uint8Array([0, 1, 2, 255]) },
  ];
  const zip = writeZip(files, new Date('2026-10-19T15:40:00Z'));
  const index = readZipIndex(zip);
  assert.deepEqual([...index.keys()].sort(), ['assets/a.bin', 'board.json']);
  for (const f of files) {
    const want = typeof f.data === 'string' ? new TextEncoder().encode(f.data) : f.data;
    assert.deepEqual(await readZipEntry(zip, index.get(f.name)), want);
  }
});
