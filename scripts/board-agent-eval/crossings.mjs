#!/usr/bin/env node
// crossings.mjs reports, for information only, how connector lines run on a
// board: the drawn path (the client's connectorPoints from
// internal/board/web/geom.js) of each connector, which other elements it
// crosses, and whether it runs through one of its own end elements (an end
// attached on the side facing away from the other end). It never fails.
//
// usage: node crossings.mjs --url http://127.0.0.1:17940 --slug feedback-1

import { args, readBoard } from './lib.mjs';
import { connectorPoints } from '../../internal/board/web/geom.js';

const opt = args(process.argv.slice(2), { url: null, slug: null });
const board = await readBoard(opt.url, opt.slug);
const byId = new Map(board.elements.map((e) => [e.id, e]));
const solids = board.elements.filter((e) => e.type !== 'frame' && e.type !== 'connector');

// segmentCrosses reports whether segment p-q passes through the interior of
// el's box, shrunk by 1 px so a line ending on the outline does not count.
function segmentCrosses(p, q, el) {
  const x0 = el.x + 1, y0 = el.y + 1, x1 = el.x + el.w - 1, y1 = el.y + el.h - 1;
  let t0 = 0, t1 = 1;
  const d = [q[0] - p[0], q[1] - p[1]];
  for (const [pc, dc, lo, hi] of [[p[0], d[0], x0, x1], [p[1], d[1], y0, y1]]) {
    if (Math.abs(dc) < 1e-9) { if (pc <= lo || pc >= hi) return false; continue; }
    let a = (lo - pc) / dc, b = (hi - pc) / dc;
    if (a > b) [a, b] = [b, a];
    t0 = Math.max(t0, a); t1 = Math.min(t1, b);
    if (t0 >= t1) return false;
  }
  return t1 - t0 > 1e-6;
}

let crossingOthers = 0, throughOwn = 0;
const conns = board.elements.filter((e) => e.type === 'connector');
for (const c of conns) {
  const pts = connectorPoints(c, (id) => byId.get(id));
  if (!pts) { console.log(`${c.id}: not drawable`); continue; }
  const ends = (c.ends || []).map((e) => e.el).filter(Boolean);
  const hits = new Set(), own = new Set();
  for (let i = 0; i + 1 < pts.length; i++) {
    for (const s of solids) {
      if (!segmentCrosses(pts[i], pts[i + 1], s)) continue;
      (ends.includes(s.id) ? own : hits).add(s.id);
    }
  }
  if (hits.size) crossingOthers++;
  if (own.size) throughOwn++;
  const name = (id) => `"${(byId.get(id)?.text || id).slice(0, 30)}"`;
  console.log(`${c.id} ${ends.map(name).join(' -> ')} at=${JSON.stringify(c.ends.map((e) => e.at))}` +
    `${hits.size ? `; crosses ${hits.size}: ${[...hits].map(name).join(', ')}` : ''}${own.size ? `; runs through its own end ${[...own].map(name).join(', ')}` : ''}`);
}
console.log(`${conns.length} connectors: ${crossingOthers} cross other elements, ${throughOwn} run through one of their own ends`);
