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

// decodePNG returns the RGBA pixels of a non-interlaced 8-bit RGB or RGBA
// PNG (what canvas.toBlob writes): { w, h, px } with 4 bytes per pixel.
export function decodePNG(buf) {
  const info = readPNG(buf);
  if (info.depth !== 8 || info.interlace || (info.color !== 6 && info.color !== 2)) {
    throw new Error(`decodePNG reads 8-bit RGB or RGBA without interlace, not color type ${info.color} depth ${info.depth}`);
  }
  const idat = [];
  for (let off = 8; off + 12 <= buf.length;) {
    const len = buf.readUInt32BE(off);
    if (buf.toString('ascii', off + 4, off + 8) === 'IDAT') idat.push(buf.subarray(off + 8, off + 8 + len));
    off += 12 + len;
  }
  const raw = zlib.inflateSync(Buffer.concat(idat));
  const bpp = info.color === 6 ? 4 : 3;
  const stride = info.w * bpp;
  const cur = Buffer.alloc(stride), prev = Buffer.alloc(stride);
  const px = new Uint8Array(info.w * info.h * 4);
  for (let y = 0; y < info.h; y++) {
    const f = raw[y * (stride + 1)];
    const line = raw.subarray(y * (stride + 1) + 1, (y + 1) * (stride + 1));
    for (let i = 0; i < stride; i++) {
      const a = i >= bpp ? cur[i - bpp] : 0, b = prev[i], c = i >= bpp ? prev[i - bpp] : 0;
      let p;
      if (f === 0) p = 0;
      else if (f === 1) p = a;
      else if (f === 2) p = b;
      else if (f === 3) p = (a + b) >> 1;
      else if (f === 4) {
        const q = a + b - c, pa = Math.abs(q - a), pb = Math.abs(q - b), pc = Math.abs(q - c);
        p = pa <= pb && pa <= pc ? a : pb <= pc ? b : c;
      } else throw new Error(`bad PNG filter ${f} on row ${y}`);
      cur[i] = (line[i] + p) & 255;
    }
    for (let x = 0; x < info.w; x++) {
      const o = (y * info.w + x) * 4;
      px[o] = cur[x * bpp]; px[o + 1] = cur[x * bpp + 1]; px[o + 2] = cur[x * bpp + 2];
      px[o + 3] = bpp === 4 ? cur[x * bpp + 3] : 255;
    }
    cur.copy(prev);
  }
  return { w: info.w, h: info.h, px };
}
