// Geometry shared by the renderer, hit testing and operation builders.
// endpointWorld and defaultAt must match the Go implementation (TECH 4.4);
// both read the vectors in internal/board/model/testdata/geometry.json.

import { style } from './schema.js';

const RAD = Math.PI / 180;

export function rotateAround(px, py, cx, cy, deg) {
  if (!deg) return [px, py];
  const r = deg * RAD, c = Math.cos(r), s = Math.sin(r);
  const dx = px - cx, dy = py - cy;
  return [cx + dx * c - dy * s, cy + dx * s + dy * c];
}

// endpointWorld is the canvas coordinate of a connector end attached to el at
// the normalized position at ([0..1, 0..1] inside the unrotated box),
// rotated with the element about its center.
export function endpointWorld(el, at) {
  const lx = el.x + at[0] * el.w, ly = el.y + at[1] * el.h;
  return rotateAround(lx, ly, el.x + el.w / 2, el.y + el.h / 2, el.rot || 0);
}

// toLocal maps a canvas point into el's unrotated box coordinates.
export function toLocal(el, wx, wy) {
  return rotateAround(wx, wy, el.x + el.w / 2, el.y + el.h / 2, -(el.rot || 0));
}

export function endWorld(end, get) {
  if (end.pt) return end.pt;
  const t = get(end.el);
  return t ? endpointWorld(t, end.at) : null;
}

// sinCos is exact at multiples of 90 degrees, as in Go.
function sinCos(deg) {
  switch (deg % 360) {
    case 0: return [0, 1];
    case 90: return [1, 0];
    case 180: return [0, -1];
    case 270: return [-1, 0];
  }
  return [Math.sin(deg * RAD), Math.cos(deg * RAD)];
}

// defaultAt is the attachment point of an end attached to el without a
// position: the middle of the side, in el's rotated frame, through which the
// ray from el's center toward the canvas point toward leaves the box. A tie
// picks the left or right side.
export function defaultAt(el, toward) {
  const dx = toward[0] - (el.x + el.w / 2), dy = toward[1] - (el.y + el.h / 2);
  const [sin, cos] = sinCos(el.rot || 0);
  const lx = dx * cos + dy * sin, ly = dy * cos - dx * sin;
  if (Math.abs(lx) * el.h >= Math.abs(ly) * el.w) return lx >= 0 ? [1, 0.5] : [0, 0.5];
  return ly >= 0 ? [0.5, 1] : [0.5, 0];
}

// outlineNorm measures a point of el's box, in coordinates normalized to
// [-0.5, 0.5] around its center, against el's outline: 1 on the outline,
// less inside.
function outlineNorm(el, ux, uy) {
  const shape = el.type === 'shape' ? style(el, 'shape') : 'rect';
  if (shape === 'ellipse') return 2 * Math.hypot(ux, uy);
  if (shape === 'diamond') return 2 * (Math.abs(ux) + Math.abs(uy));
  return 2 * Math.max(Math.abs(ux), Math.abs(uy));
}

function normalized(el, wx, wy) {
  const [lx, ly] = toLocal(el, wx, wy);
  return [(lx - el.x) / el.w - 0.5, (ly - el.y) / el.h - 0.5];
}

const inside = (el, at) => outlineNorm(el, at[0] - 0.5, at[1] - 0.5) < 0.99;

// clipToOutline moves an end inside its target out to where the segment
// toward to crosses the target's outline. An end whose segment stays inside
// is kept.
function clipToOutline(el, from, to) {
  const [tx, ty] = normalized(el, to[0], to[1]);
  if (outlineNorm(el, tx, ty) <= 1) return from;
  let lo = 0, hi = 1;
  const at = (t) => [from[0] + (to[0] - from[0]) * t, from[1] + (to[1] - from[1]) * t];
  for (let i = 0; i < 30; i++) {
    const mid = (lo + hi) / 2;
    const [x, y] = at(mid);
    if (outlineNorm(el, ...normalized(el, x, y)) < 1) lo = mid; else hi = mid;
  }
  return at(hi);
}

// ---- rectangles: {x, y, w, h} ----

export function rectFromPoints(pts) {
  let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
  for (const [x, y] of pts) {
    if (x < x0) x0 = x; if (x > x1) x1 = x;
    if (y < y0) y0 = y; if (y > y1) y1 = y;
  }
  return { x: x0, y: y0, w: x1 - x0, h: y1 - y0 };
}
export function union(a, b) {
  if (!a) return b;
  if (!b) return a;
  const x = Math.min(a.x, b.x), y = Math.min(a.y, b.y);
  return { x, y, w: Math.max(a.x + a.w, b.x + b.w) - x, h: Math.max(a.y + a.h, b.y + b.h) - y };
}
export const contains = (r, x, y) => x >= r.x && x <= r.x + r.w && y >= r.y && y <= r.y + r.h;
export const containsRect = (a, b) => b.x >= a.x && b.y >= a.y && b.x + b.w <= a.x + a.w && b.y + b.h <= a.y + a.h;
export const inflate = (r, d) => ({ x: r.x - d, y: r.y - d, w: r.w + 2 * d, h: r.h + 2 * d });
export const center = (r) => [r.x + r.w / 2, r.y + r.h / 2];

export function boxCorners(el) {
  const cx = el.x + el.w / 2, cy = el.y + el.h / 2, r = el.rot || 0;
  return [[el.x, el.y], [el.x + el.w, el.y], [el.x + el.w, el.y + el.h], [el.x, el.y + el.h]]
    .map(([x, y]) => rotateAround(x, y, cx, cy, r));
}

// ---- connectors ----

// sideDir is the outward direction of an attached end, [axis, sign]: the
// end leaves from the nearest side of its target. A free end has none.
function sideDir(end, get) {
  const t = end.el ? get(end.el) : null;
  if (!t) return null;
  const [ax, ay] = end.at;
  const d = [[ax, -1, 0], [1 - ax, 1, 0], [ay, 0, -1], [1 - ay, 0, 1]].sort((a, b) => a[0] - b[0])[0];
  const [dx, dy] = rotateAround(d[1], d[2], 0, 0, t.rot || 0);
  return Math.abs(dx) >= Math.abs(dy) ? ['h', Math.sign(dx)] : ['v', Math.sign(dy)];
}

// connectorPoints is the path of a connector: a straight segment, or an
// orthogonal elbow path chosen from the directions of both ends (no
// obstacle avoidance, TECH 11.1). Attached ends start on the target's
// outline: an end attached inside its target (such as an older center
// attachment) is cut where a straight line leaves the target, and leaves an
// elbow path from the side that faces the other end.
export function connectorPoints(conn, get) {
  let ends = conn.ends;
  const a0 = endWorld(ends[0], get), b0 = endWorld(ends[1], get);
  if (!a0 || !b0) return null;
  const within = ends.map((e) => {
    const t = e.el && get(e.el);
    return t && inside(t, e.at) ? t : null;
  });
  if (style(conn, 'route') !== 'elbow') {
    return [within[0] ? clipToOutline(within[0], a0, b0) : a0, within[1] ? clipToOutline(within[1], b0, a0) : b0];
  }
  const toward = [b0, a0];
  ends = ends.map((e, k) => (within[k] ? { el: e.el, at: defaultAt(within[k], toward[k]) } : e));
  const a = endWorld(ends[0], get), b = endWorld(ends[1], get);
  const free = Math.abs(b[0] - a[0]) >= Math.abs(b[1] - a[1]) ? ['h', 0] : ['v', 0];
  const [da, sa] = sideDir(ends[0], get) || free;
  const [db, sb] = sideDir(ends[1], get) || [da, 0];
  const GAP = 24;
  let pts;
  if (da === db) {
    const i = da === 'h' ? 0 : 1;
    let m = (a[i] + b[i]) / 2;
    if (sa === sb && sa !== 0) m = sa > 0 ? Math.max(a[i], b[i]) + GAP : Math.min(a[i], b[i]) - GAP;
    pts = da === 'h' ? [a, [m, a[1]], [m, b[1]], b] : [a, [a[0], m], [b[0], m], b];
  } else if (da === 'h') pts = [a, [b[0], a[1]], b];
  else pts = [a, [a[0], b[1]], b];
  return pts.filter((p, i) => i === 0 || p[0] !== pts[i - 1][0] || p[1] !== pts[i - 1][1]);
}

export function polylineLength(pts) {
  let len = 0;
  for (let i = 1; i < pts.length; i++) len += Math.hypot(pts[i][0] - pts[i - 1][0], pts[i][1] - pts[i - 1][1]);
  return len;
}

// pointAlong returns the point at fraction f of the polyline's length.
export function pointAlong(pts, f) {
  let rest = polylineLength(pts) * f;
  for (let i = 1; i < pts.length; i++) {
    const [x0, y0] = pts[i - 1], [x1, y1] = pts[i];
    const seg = Math.hypot(x1 - x0, y1 - y0);
    if (rest <= seg && seg > 0) return [x0 + ((x1 - x0) * rest) / seg, y0 + ((y1 - y0) * rest) / seg];
    rest -= seg;
  }
  return pts[pts.length - 1];
}

export function labelBox(conn, mid) {
  const fs = style(conn, 'fontSize');
  const lines = String(conn.text || '').split('\n');
  const w = Math.max(...lines.map((l) => l.length)) * fs * 0.62 + 10;
  const h = lines.length * fs * 1.3 + 6;
  return { x: mid[0] - w / 2, y: mid[1] - h / 2, w, h };
}

export function connectorBounds(conn, get) {
  const pts = connectorPoints(conn, get);
  if (!pts) return null;
  let r = inflate(rectFromPoints(pts), 12 + style(conn, 'strokeWidth'));
  if (conn.text) r = union(r, labelBox(conn, pointAlong(pts, 0.5)));
  return r;
}

// ---- pens ----

export function penWorldPoints(el) {
  const out = [];
  const p = el.pts || [];
  const cx = el.x + el.w / 2, cy = el.y + el.h / 2, r = el.rot || 0;
  for (let i = 0; i + 1 < p.length; i += 2) {
    out.push(rotateAround(el.x + (p[i] / 10000) * el.w, el.y + (p[i + 1] / 10000) * el.h, cx, cy, r));
  }
  return out;
}

export const penWidth = (el) => (style(el, 'width') === 'marker' ? 16 : 3);

// rdp simplifies a polyline ([[x, y], ...]) with the Ramer-Douglas-Peucker
// algorithm and tolerance eps.
export function rdp(pts, eps) {
  if (pts.length < 3) return pts.slice();
  const keep = new Uint8Array(pts.length);
  keep[0] = keep[pts.length - 1] = 1;
  const stack = [[0, pts.length - 1]];
  while (stack.length) {
    const [s, e] = stack.pop();
    let best = -1, bi = -1;
    for (let i = s + 1; i < e; i++) {
      const d = distToSegment(pts[i][0], pts[i][1], pts[s], pts[e]);
      if (d > best) { best = d; bi = i; }
    }
    if (best > eps) {
      keep[bi] = 1;
      stack.push([s, bi], [bi, e]);
    }
  }
  return pts.filter((_, i) => keep[i]);
}

// penFromPoints turns a drawn stroke into an element box and points
// normalized to integers 0..10000 inside it (TECH 3.1), at most max points.
export function penFromPoints(world, eps, max = 2000) {
  let pts = rdp(world, eps);
  for (let e = eps; pts.length > max;) {
    e *= 2;
    pts = rdp(world, e);
  }
  const r = rectFromPoints(pts);
  const w = Math.max(r.w, 1), h = Math.max(r.h, 1);
  const out = [];
  for (const [x, y] of pts) {
    out.push(Math.round(((x - r.x) / w) * 10000), Math.round(((y - r.y) / h) * 10000));
  }
  if (out.length === 2) out.push(out[0], out[1]);
  return { x: r.x, y: r.y, w, h, rot: 0, pts: out };
}

// ---- bounds and hit testing ----

export function distToSegment(px, py, a, b) {
  const dx = b[0] - a[0], dy = b[1] - a[1];
  const l2 = dx * dx + dy * dy;
  let t = l2 ? ((px - a[0]) * dx + (py - a[1]) * dy) / l2 : 0;
  t = Math.max(0, Math.min(1, t));
  return Math.hypot(px - (a[0] + t * dx), py - (a[1] + t * dy));
}

export function distToPolyline(px, py, pts) {
  let d = Infinity;
  for (let i = 1; i < pts.length; i++) d = Math.min(d, distToSegment(px, py, pts[i - 1], pts[i]));
  if (pts.length === 1) d = Math.hypot(px - pts[0][0], py - pts[0][1]);
  return d;
}

// elementBounds is the axis-aligned box of an element, including the
// derived path of a connector.
export function elementBounds(el, get) {
  if (el.type === 'connector') return connectorBounds(el, get);
  const r = el.rot ? rectFromPoints(boxCorners(el)) : { x: el.x, y: el.y, w: el.w, h: el.h };
  if (el.type === 'pen') return inflate(r, penWidth(el) / 2 + 1);
  if (el.type === 'frame') return { x: r.x, y: r.y - 28, w: r.w, h: r.h + 28 };
  return r;
}

export function insideBox(el, wx, wy) {
  const [lx, ly] = toLocal(el, wx, wy);
  return lx >= el.x && lx <= el.x + el.w && ly >= el.y && ly <= el.y + el.h;
}

// hit reports whether a canvas point touches el at zoom level zoom. Frames
// are hit on their title and border, so a drag inside a frame selects its
// members instead of moving the frame.
export function hit(el, wx, wy, zoom, get) {
  const tol = 6 / zoom;
  if (el.type === 'connector') {
    const pts = connectorPoints(el, get);
    if (!pts) return false;
    if (el.text && contains(labelBox(el, pointAlong(pts, 0.5)), wx, wy)) return true;
    return distToPolyline(wx, wy, pts) <= tol + style(el, 'strokeWidth');
  }
  if (el.type === 'pen') return distToPolyline(wx, wy, penWorldPoints(el)) <= tol + penWidth(el) / 2;
  if (el.type === 'frame') {
    if (wy < el.y && wy >= el.y - 24 / zoom && wx >= el.x && wx <= el.x + el.w) return true;
    const [lx, ly] = toLocal(el, wx, wy);
    const inOuter = lx >= el.x - tol && lx <= el.x + el.w + tol && ly >= el.y - tol && ly <= el.y + el.h + tol;
    const inInner = lx > el.x + tol && lx < el.x + el.w - tol && ly > el.y + tol && ly < el.y + el.h - tol;
    return inOuter && !inInner;
  }
  return insideBox(el, wx, wy);
}

// outlineAt is the normalized attachment point for a connector end dropped
// at a canvas point within tol of el's outline: the point on the outline,
// snapped to a side's middle nearby. It is null for a point farther inside.
export function outlineAt(el, wx, wy, tol) {
  const [ux, uy] = normalized(el, wx, wy);
  const r = outlineNorm(el, ux, uy);
  if (r === 0 || (Math.abs(r - 1) * Math.min(el.w, el.h)) / 2 > tol) return null;
  let ax = 0.5 + ux / r, ay = 0.5 + uy / r;
  if (el.type !== 'shape' || ['rect', 'round'].includes(style(el, 'shape'))) {
    // A box side: the nearest point on it, not the one toward the center.
    ax = 0.5 + ux; ay = 0.5 + uy;
    if ((0.5 - Math.abs(ux)) * el.w < (0.5 - Math.abs(uy)) * el.h) ax = ux < 0 ? 0 : 1;
    else ay = uy < 0 ? 0 : 1;
  }
  ax = Math.max(0, Math.min(1, ax)); ay = Math.max(0, Math.min(1, ay));
  const mids = [[0.5, 0], [1, 0.5], [0.5, 1], [0, 0.5]];
  for (const m of mids) if (Math.abs(m[0] - ax) < 0.2 && Math.abs(m[1] - ay) < 0.2) return m.slice();
  return [Math.round(ax * 1000) / 1000, Math.round(ay * 1000) / 1000];
}

// attachAt is the attachment point for an end dropped on el: on the outline
// under the cursor, or else the middle of the side facing toward, so lines
// never start inside el.
export const attachAt = (el, wx, wy, tol, toward) => outlineAt(el, wx, wy, tol) || defaultAt(el, toward);

// sideAnchor is the attachment point in the middle of the side facing (dx, dy).
export function sideAnchor(dx, dy) {
  if (Math.abs(dx) >= Math.abs(dy)) return dx >= 0 ? [1, 0.5] : [0, 0.5];
  return dy >= 0 ? [0.5, 1] : [0.5, 0];
}

export function elementCenter(el, get) {
  if (el.type === 'connector') {
    const pts = connectorPoints(el, get);
    return pts ? pointAlong(pts, 0.5) : [0, 0];
  }
  return [el.x + el.w / 2, el.y + el.h / 2];
}
