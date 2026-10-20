// The pixel oracle of the PNG export check: a small board with known
// content, and a comparison of an exported image with what that board must
// look like. Synthetic data only.

// The light theme colors the board below uses (web/theme.js). A frame's
// fill is its color blended over the canvas.
export const PALETTE = { canvas: '#f4f4f0', pink: '#f6cfdc', blue: '#cfe0f6', green: '#d3eacf', frameGray: '#e4e4df', frameAlpha: 0.55 };
const rgb = (hex) => [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16));
// PIXEL_BOARD is a gray frame named "Pixel frame" at (0, 0) 400x300, a pink
// sticky inside it, and a blue sticky and a green rectangle beside it. The
// expected image comes from these coordinates, the 32px margin (TECH 11.6),
// the frame's 28px name band (geom.elementBounds) and the palette, not from
// the exporter.
export const PIXEL_BOARD = [
  { type: 'frame', text: 'Pixel frame', x: 0, y: 0, w: 400, h: 300, style: { fill: 'gray' } },
  { type: 'sticky', x: 40, y: 40, w: 120, h: 120, style: { color: 'pink' } },
  { type: 'sticky', x: 480, y: 40, w: 120, h: 120, style: { color: 'blue' } },
  { type: 'shape', x: 480, y: 200, w: 120, h: 80, style: { shape: 'rect', fill: 'green' } },
];

// pixelErrors compares an exported image with the known board at scale s
// and returns what differs, plus measurements for the report.
export function pixelErrors(img, s) {
  const errs = [];
  const top = (e) => e.y - (e.type === 'frame' ? 28 : 0);
  const minX = Math.min(...PIXEL_BOARD.map((e) => e.x)), maxX = Math.max(...PIXEL_BOARD.map((e) => e.x + e.w));
  const minY = Math.min(...PIXEL_BOARD.map(top)), maxY = Math.max(...PIXEL_BOARD.map((e) => e.y + e.h));
  const ox = minX - 32, oy = minY - 32;
  const W = Math.ceil((maxX - minX + 64) * s), H = Math.ceil((maxY - minY + 64) * s);
  if (img.w !== W || img.h !== H) return { errs: [`image ${img.w}x${img.h}, want ${W}x${H}`] };
  const at = (x, y) => { const o = (y * img.w + x) * 4; return [img.px[o], img.px[o + 1], img.px[o + 2], img.px[o + 3]]; };
  const P = (wx, wy) => [Math.floor((wx - ox) * s), Math.floor((wy - oy) * s)];
  const near = (c, want, tol) => c[3] === 255 && c.slice(0, 3).every((v, i) => Math.abs(v - want[i]) <= tol);
  const canvas = rgb(PALETTE.canvas);
  const frameFill = rgb(PALETTE.frameGray).map((v, i) => Math.round(v * PALETTE.frameAlpha + canvas[i] * (1 - PALETTE.frameAlpha)));
  // Solid areas: a 5x5 patch around each point has the expected color.
  // Points come from the board: each box element's center, an empty spot
  // inside the frame and one between the elements.
  const inside = (e, x, y) => x >= e.x && x <= e.x + e.w && y >= e.y && y <= e.y + e.h;
  const frame = PIXEL_BOARD.find((e) => e.type === 'frame');
  const boxes = PIXEL_BOARD.filter((e) => e.type !== 'frame');
  const free = (x, y, inFrame) => !boxes.some((e) => inside(e, x, y)) && inside(frame, x, y) === inFrame;
  const spot = (inFrame) => {
    for (let y = minY + 30; y < maxY; y += 10) for (let x = minX + 10; x < maxX; x += 10) {
      if ([-12, 0, 12].every((d) => free(x + d, y + d, inFrame) && free(x - d, y + d, inFrame))) return [x, y];
    }
    throw new Error('no free spot on the pixel board');
  };
  const colorOf = (e) => rgb(PALETTE[e.type === 'sticky' ? e.style.color : e.style.fill]);
  const patches = [
    ['top-left corner', [3, 3], canvas],
    ['bottom-right corner', [W - 4, H - 4], canvas],
    ['canvas between the elements', P(...spot(false)), canvas],
    ['inside the frame', P(...spot(true)), frameFill],
    ...boxes.map((e) => [`${e.style.color || e.style.fill} ${e.type}`, P(e.x + e.w / 2, e.y + e.h / 2), colorOf(e)]),
  ];
  for (const [what, [x, y], want] of patches) {
    for (let dy = -2; dy <= 2; dy++) for (let dx = -2; dx <= 2; dx++) {
      const px = x + dx, py = y + dy;
      const c = at(px, py);
      if (!near(c, want, 2)) { errs.push(`${what}: pixel (${px}, ${py}) is ${c}, want ${want}`); dy = dx = 3; }
    }
  }
  // Extents: along the row through each sticky's center, its color spans
  // the sticky's width times the scale, from its left edge.
  const extents = {};
  for (const e of boxes.filter((b) => b.type === 'sticky')) {
    const want = colorOf(e);
    const [x0, y] = P(e.x + e.w / 2, e.y + e.h / 2);
    let l = x0, r = x0;
    while (l > 0 && near(at(l - 1, y), want, 2)) l--;
    while (r < W - 1 && near(at(r + 1, y), want, 2)) r++;
    const what = `${e.style.color} sticky`;
    extents[what] = r - l + 1;
    if (Math.abs(r - l + 1 - e.w * s) > 2 || Math.abs(l - P(e.x, 0)[0]) > 1) errs.push(`${what}: color spans x ${l}+${r - l + 1}, want ${P(e.x, 0)[0]}+${e.w * s}`);
  }
  // The frame's name: dark pixels above the frame's top edge, starting at
  // its left edge, inside the name band, and nowhere else above the frame.
  const ink = (c) => Math.max(...c.slice(0, 3).map((v, i) => Math.abs(v - canvas[i]))) > 60;
  const [fx, fTop] = P(frame.x, frame.y), [fRight] = P(frame.x + frame.w, 0), bandTop = P(0, frame.y - 28)[1];
  let box = null;
  for (let y = 0; y < fTop; y++) for (let x = 0; x < W; x++) {
    if (!ink(at(x, y))) continue;
    if (y < bandTop || x < fx - 1 || x > fRight) { errs.push(`ink above the frame outside its name band at (${x}, ${y})`); y = fTop; break; }
    box = box ? { l: Math.min(box.l, x), r: Math.max(box.r, x), t: Math.min(box.t, y), b: Math.max(box.b, y) } : { l: x, r: x, t: y, b: y };
  }
  if (!box) errs.push('no frame name above the frame');
  else {
    if (box.l - fx > 3 * s || box.l < fx - 1) errs.push(`the frame name starts at x ${box.l}, the frame at ${fx}`);
    if (fTop - box.b < 2 * s || fTop - box.b > 12 * s) errs.push(`the frame name ends ${fTop - box.b}px above the frame`);
  }
  return { errs, extents, name: box && { left: box.l - fx, height: box.b - box.t + 1, width: box.r - box.l + 1, gap: fTop - box.b } };
}
