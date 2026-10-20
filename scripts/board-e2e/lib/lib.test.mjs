// Tests of the e2e helpers that need no browser or host:
// node --test scripts/board-e2e/lib/lib.test.mjs

import test from 'node:test';
import assert from 'node:assert/strict';
import zlib from 'node:zlib';
import { ownedProcesses } from './cdp.mjs';
import { makePNG, decodePNG } from './files.mjs';

test('ownedProcesses picks only this browser with exactly this profile', () => {
  const chrome = '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
  const helper = '/Applications/Google Chrome.app/Contents/Frameworks/Google Chrome Framework.framework/Helpers/Google Chrome Helper.app/Contents/MacOS/Google Chrome Helper';
  const p = '/tmp/board-e2e-chrome-abc';
  const listing = [
    `  101   101 ${chrome} --user-data-dir=${p} --remote-debugging-port=0`,
    `  102   101 ${helper} --type=gpu-process`, // in the browser's group
    `  103     7 ${helper} --type=utility --user-data-dir=${p}`, // outside the group
    `  104     7 ${helper} --type=gpu-process --user-data-dir=${p}-other`,
    `  105     7 ${chrome} --user-data-dir=${p}x about:blank`,
    `  106     7 /usr/bin/node -e x -- --user-data-dir=${p}`,
    `  107     7 /bin/grep --user-data-dir=${p}`,
    `  108     7 ${chrome} --user-data-dir=/tmp/another`,
    `  109     7 ${chrome} --note=--user-data-dir=${p}`,
  ].join('\n');
  assert.deepEqual(ownedProcesses(listing, chrome, p, 101).map((x) => x.pid), [101, 102, 103]);
  // Elsewhere only the executable itself counts, not its directory.
  const linux = '/usr/bin/chromium';
  const other = [
    `7 9 /usr/bin/chromium --type=renderer --user-data-dir=${p}`,
    `8 9 /usr/bin/node /tmp/probe.mjs --user-data-dir=${p}`,
    `9 9 /usr/bin/chromium-wrapper --user-data-dir=${p}`,
    `10 9 /opt/google/chrome/chrome --note=--user-data-dir=${p}`,
  ].join('\n');
  assert.deepEqual(ownedProcesses(other, linux, p, 0).map((x) => x.pid), [7]);
});

// encodePNG writes RGBA rows with the given filter per row, so the decoder
// meets every filter type, as browsers write them.
function encodePNG(w, h, px, filters) {
  const raw = Buffer.alloc(h * (1 + w * 4));
  for (let y = 0; y < h; y++) {
    const f = filters[y % filters.length];
    raw[y * (1 + w * 4)] = f;
    for (let i = 0; i < w * 4; i++) {
      const cur = px[y * w * 4 + i];
      const a = i >= 4 ? px[y * w * 4 + i - 4] : 0;
      const b = y > 0 ? px[(y - 1) * w * 4 + i] : 0;
      const c = y > 0 && i >= 4 ? px[(y - 1) * w * 4 + i - 4] : 0;
      const paeth = () => { const q = a + b - c, pa = Math.abs(q - a), pb = Math.abs(q - b), pc = Math.abs(q - c); return pa <= pb && pa <= pc ? a : pb <= pc ? b : c; };
      const pred = [0, a, b, (a + b) >> 1, paeth()][f];
      raw[y * (1 + w * 4) + 1 + i] = (cur - pred) & 255;
    }
  }
  const chunk = (type, data) => {
    const len = Buffer.alloc(4); len.writeUInt32BE(data.length);
    const td = Buffer.concat([Buffer.from(type), data]);
    const crc = Buffer.alloc(4); crc.writeUInt32BE(zlib.crc32(td) >>> 0);
    return Buffer.concat([len, td, crc]);
  };
  const ihdr = Buffer.alloc(13); ihdr.writeUInt32BE(w, 0); ihdr.writeUInt32BE(h, 4); ihdr[8] = 8; ihdr[9] = 6;
  return Buffer.concat([Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]), chunk('IHDR', ihdr), chunk('IDAT', zlib.deflateSync(raw)), chunk('IEND', Buffer.alloc(0))]);
}

test('decodePNG returns the pixels written with every filter type', () => {
  const w = 13, h = 10;
  const px = new Uint8Array(w * h * 4).map((_, i) => (i * 37 + (i >> 2) * 11) & 255);
  const img = decodePNG(encodePNG(w, h, px, [0, 1, 2, 3, 4]));
  assert.equal(img.w, w);
  assert.equal(img.h, h);
  assert.deepEqual([...img.px], [...px]);
  const plain = decodePNG(makePNG(5, 4, 3));
  assert.deepEqual([...plain.px.slice(0, 4)], [3, 0, (128 + 21) & 255, 255]);
});
