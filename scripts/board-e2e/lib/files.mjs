// PNG and zip helpers for fixtures and for checking downloaded results.
// Synthetic data only.

import zlib from 'node:zlib';

const SIG = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);

function chunk(type, data) {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length);
  const td = Buffer.concat([Buffer.from(type, 'ascii'), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(zlib.crc32(td) >>> 0);
  return Buffer.concat([len, td, crc]);
}

// makePNG builds an RGBA gradient image of w x h pixels.
export function makePNG(w, h, seed = 0) {
  const raw = Buffer.alloc(h * (1 + w * 4));
  for (let y = 0; y < h; y++) {
    const row = y * (1 + w * 4);
    raw[row] = 0;
    for (let x = 0; x < w; x++) {
      const o = row + 1 + x * 4;
      raw[o] = (x * 255 / Math.max(1, w - 1) + seed) & 255;
      raw[o + 1] = (y * 255 / Math.max(1, h - 1)) & 255;
      raw[o + 2] = (128 + seed * 7) & 255;
      raw[o + 3] = 255;
    }
  }
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(w, 0);
  ihdr.writeUInt32BE(h, 4);
  ihdr[8] = 8; ihdr[9] = 6; ihdr[10] = 0; ihdr[11] = 0; ihdr[12] = 0;
  return Buffer.concat([SIG, chunk('IHDR', ihdr), chunk('IDAT', zlib.deflateSync(raw)), chunk('IEND', Buffer.alloc(0))]);
}

// readPNG validates the signature, chunk CRCs and IHDR, inflates the image
// data, and checks that its length matches the declared size. It returns
// the size and simple pixel statistics.
export function readPNG(buf) {
  if (buf.length < 8 || !buf.subarray(0, 8).equals(SIG)) throw new Error('not a PNG (bad signature)');
  let off = 8, ihdr = null, idat = [], ended = false;
  while (off + 12 <= buf.length) {
    const len = buf.readUInt32BE(off);
    const type = buf.toString('ascii', off + 4, off + 8);
    const data = buf.subarray(off + 8, off + 8 + len);
    const crc = buf.readUInt32BE(off + 8 + len);
    if ((zlib.crc32(buf.subarray(off + 4, off + 8 + len)) >>> 0) !== crc) throw new Error(`bad CRC in ${type}`);
    if (type === 'IHDR') ihdr = { w: data.readUInt32BE(0), h: data.readUInt32BE(4), depth: data[8], color: data[9], interlace: data[12] };
    if (type === 'IDAT') idat.push(data);
    off += 12 + len;
    if (type === 'IEND') { ended = true; break; }
  }
  if (!ihdr || !ended) throw new Error('PNG is missing IHDR or IEND');
  const bpp = { 6: 4, 2: 3, 0: 1, 4: 2 }[ihdr.color];
  const raw = zlib.inflateSync(Buffer.concat(idat));
  const expected = ihdr.interlace ? null : ihdr.h * (1 + ihdr.w * bpp * (ihdr.depth / 8));
  if (expected != null && raw.length !== expected) throw new Error(`PNG data is ${raw.length} bytes, expected ${expected}`);
  // Distinct filtered byte values: a blank image has very few.
  const seen = new Set();
  for (let i = 0; i < raw.length && seen.size < 64; i += 997) seen.add(raw[i]);
  return { ...ihdr, distinct: seen.size };
}

// readZip lists and extracts the entries of a zip file.
export function readZip(buf) {
  let eocd = -1;
  for (let i = buf.length - 22; i >= Math.max(0, buf.length - 65557); i--) {
    if (buf.readUInt32LE(i) === 0x06054b50) { eocd = i; break; }
  }
  if (eocd < 0) throw new Error('not a zip file (no end of central directory)');
  const count = buf.readUInt16LE(eocd + 10);
  let p = buf.readUInt32LE(eocd + 16);
  const out = new Map();
  for (let i = 0; i < count; i++) {
    if (buf.readUInt32LE(p) !== 0x02014b50) throw new Error('bad central directory entry');
    const method = buf.readUInt16LE(p + 10);
    const csize = buf.readUInt32LE(p + 20);
    const nlen = buf.readUInt16LE(p + 28), xlen = buf.readUInt16LE(p + 30), clen = buf.readUInt16LE(p + 32);
    const local = buf.readUInt32LE(p + 42);
    const name = buf.toString('utf8', p + 46, p + 46 + nlen);
    const lnlen = buf.readUInt16LE(local + 26), lxlen = buf.readUInt16LE(local + 28);
    const start = local + 30 + lnlen + lxlen;
    const data = buf.subarray(start, start + csize);
    out.set(name, method === 8 ? zlib.inflateRawSync(data) : Buffer.from(data));
    p += 46 + nlen + xlen + clen;
  }
  return out;
}
