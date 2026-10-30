// Geometry shared by the renderer, hit testing and operation builders.
// endpointWorld and defaultAt must match the Go implementation (TECH 4.4);
// both read the vectors in internal/board/model/testdata/geometry.json.

import { style } from './schema.js';

const RAD = Math.PI / 180;

// Scalar helpers below write results into these module slots instead of
// allocating a [x, y] per point; the caller reads the slots before any other
// geometry call can overwrite them. Exported functions keep returning fresh
// arrays wherever the value escapes to callers.
const sc = { sin: 0, cos: 1 };  // sinCos
const loc = { x: 0, y: 0 };     // toLocalScalars
const nrm = { x: 0, y: 0 };     // normalized
const dir = { axis: null, sign: 0 }; // sideDir
const at1 = [0, 0];             // defaultAtInto scratch, read immediately
const MIDS = [[0.5, 0], [1, 0.5], [0.5, 1], [0, 0.5]];
const GAP = 24;

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
  toLocalScalars(el, wx, wy);
  return [loc.x, loc.y];
}

// toLocalScalars is toLocal without the array: it writes into loc.
function toLocalScalars(el, wx, wy) {
  const rot = el.rot || 0;
  if (!rot) { loc.x = wx; loc.y = wy; return; }
  const cx = el.x + el.w / 2, cy = el.y + el.h / 2;
  const r = -rot * RAD, c = Math.cos(r), s = Math.sin(r);
  const dx = wx - cx, dy = wy - cy;
  loc.x = cx + dx * c - dy * s;
  loc.y = cy + dx * s + dy * c;
}

export function endWorld(end, get) {
  if (end.pt) return end.pt;
  const t = get(end.el);
  return t ? endpointWorld(t, end.at) : null;
}

// sinCos is exact at multiples of 90 degrees, as in Go; it writes into sc.
function sinCos(deg) {
  switch (deg % 360) {
    case 0: sc.sin = 0; sc.cos = 1; return;
    case 90: sc.sin = 1; sc.cos = 0; return;
    case 180: sc.sin = 0; sc.cos = -1; return;
    case 270: sc.sin = -1; sc.cos = 0; return;
  }
  sc.sin = Math.sin(deg * RAD);
  sc.cos = Math.cos(deg * RAD);
}

// defaultAt is the attachment point of an end attached to el without a
// position: the middle of the side, in el's rotated frame, through which the
// ray from el's center toward the canvas point toward leaves the box. A tie
// picks the left or right side.
export function defaultAt(el, toward) {
  const out = [0, 0];
  defaultAtInto(el, toward[0], toward[1], out);
  return out;
}

// defaultAtInto writes defaultAt into out without allocating.
function defaultAtInto(el, tx, ty, out) {
  const dx = tx - (el.x + el.w / 2), dy = ty - (el.y + el.h / 2);
  sinCos(el.rot || 0);
  const lx = dx * sc.cos + dy * sc.sin, ly = dy * sc.cos - dx * sc.sin;
  if (Math.abs(lx) * el.h >= Math.abs(ly) * el.w) { out[0] = lx >= 0 ? 1 : 0; out[1] = 0.5; }
  else { out[0] = 0.5; out[1] = ly >= 0 ? 1 : 0; }
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
  toLocalScalars(el, wx, wy);
  nrm.x = (loc.x - el.x) / el.w - 0.5;
  nrm.y = (loc.y - el.y) / el.h - 0.5;
}

const inside = (el, at) => outlineNorm(el, at[0] - 0.5, at[1] - 0.5) < 0.99;

// clipToOutline moves an end inside its target out to where the segment
// toward to crosses the target's outline. An end whose segment stays inside
// is kept.
function clipToOutline(el, from, to) {
  normalized(el, to[0], to[1]);
  if (outlineNorm(el, nrm.x, nrm.y) <= 1) return from;
  const fx = from[0], fy = from[1], tx = to[0], ty = to[1];
  let lo = 0, hi = 1;
  for (let i = 0; i < 30; i++) {
    const mid = (lo + hi) / 2;
    normalized(el, fx + (tx - fx) * mid, fy + (ty - fy) * mid);
    if (outlineNorm(el, nrm.x, nrm.y) < 1) lo = mid; else hi = mid;
  }
  return [fx + (tx - fx) * hi, fy + (ty - fy) * hi];
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
  const x = el.x, y = el.y, w = el.w, h = el.h, rot = el.rot || 0;
  if (!rot) return [[x, y], [x + w, y], [x + w, y + h], [x, y + h]];
  const cx = x + w / 2, cy = y + h / 2;
  const r = rot * RAD, c = Math.cos(r), s = Math.sin(r);
  const rot2 = (px, py) => {
    const dx = px - cx, dy = py - cy;
    return [cx + dx * c - dy * s, cy + dx * s + dy * c];
  };
  return [rot2(x, y), rot2(x + w, y), rot2(x + w, y + h), rot2(x, y + h)];
}

// ---- connectors ----

// sideDir is the outward direction of an attached end: the end leaves from
// the nearest side of its target, written into dir (axis 'h'/'v' plus sign).
// It returns false for a free end, which has no direction.
function sideDir(elId, at, get) {
  const t = elId ? get(elId) : null;
  if (!t) { dir.axis = null; return false; }
  const ax = at[0], ay = at[1];
  // Distances to the left, right, top and bottom side; the earliest wins a
  // tie, as the sorted candidate list did before.
  let d = ax, dx = -1, dy = 0;
  if (1 - ax < d) { d = 1 - ax; dx = 1; dy = 0; }
  if (ay < d) { d = ay; dx = 0; dy = -1; }
  if (1 - ay < d) { dx = 0; dy = 1; }
  const rot = t.rot || 0;
  let rx = dx, ry = dy;
  if (rot) {
    const r = rot * RAD, c = Math.cos(r), s = Math.sin(r);
    rx = dx * c - dy * s;
    ry = dx * s + dy * c;
  }
  if (Math.abs(rx) >= Math.abs(ry)) { dir.axis = 'h'; dir.sign = Math.sign(rx); }
  else { dir.axis = 'v'; dir.sign = Math.sign(ry); }
  return true;
}

// endWorldOf is the end's canvas point with its attachment replaced by the
// middle of the side of w facing toward, for ends attached inside w.
function endWorldOf(e, w, toward, get) {
  if (w) {
    defaultAtInto(w, toward[0], toward[1], at1);
    return endpointWorld(w, at1);
  }
  if (e.pt) return e.pt;
  const t = get(e.el);
  return t ? endpointWorld(t, e.at) : null;
}

// within is the target when end attaches inside it, else null.
const within = (e, get) => {
  const t = e.el && get(e.el);
  return t && inside(t, e.at) ? t : null;
};

// connectorPoints is the path of a connector: a straight segment, or an
// orthogonal elbow path chosen from the directions of both ends (no
// obstacle avoidance, TECH 11.1). Attached ends start on the target's
// outline: an end attached inside its target (such as an older center
// attachment) is cut where a straight line leaves the target, and leaves an
// elbow path from the side that faces the other end.
export function connectorPoints(conn, get) {
  const ends = conn.ends;
  const a0 = endWorld(ends[0], get), b0 = endWorld(ends[1], get);
  if (!a0 || !b0) return null;
  const w0 = within(ends[0], get), w1 = within(ends[1], get);
  if (style(conn, 'route') !== 'elbow') {
    return [w0 ? clipToOutline(w0, a0, b0) : a0, w1 ? clipToOutline(w1, b0, a0) : b0];
  }
  const a = endWorldOf(ends[0], w0, b0, get), b = endWorldOf(ends[1], w1, a0, get);
  // sideDir sees the replaced attachment of an end that sits inside its
  // target, as when the ends were rebuilt before.
  const atOf = (e, w, toward) => {
    if (!w) return e.at;
    defaultAtInto(w, toward[0], toward[1], at1);
    return at1;
  };
  sideDir(ends[0].el, atOf(ends[0], w0, b0), get);
  const da = dir.axis || (Math.abs(b[0] - a[0]) >= Math.abs(b[1] - a[1]) ? 'h' : 'v');
  const sa = dir.axis ? dir.sign : 0;
  sideDir(ends[1].el, atOf(ends[1], w1, a0), get);
  const db = dir.axis || da;
  const sb = dir.axis ? dir.sign : 0;
  // The path skips points that repeat their predecessor.
  const pts = [a];
  const add = (x, y) => {
    const q = pts[pts.length - 1];
    if (q[0] !== x || q[1] !== y) pts.push([x, y]);
  };
  if (da === db) {
    const i = da === 'h' ? 0 : 1;
    let m = (a[i] + b[i]) / 2;
    if (sa === sb && sa !== 0) m = sa > 0 ? Math.max(a[i], b[i]) + GAP : Math.min(a[i], b[i]) - GAP;
    if (da === 'h') { add(m, a[1]); add(m, b[1]); }
    else { add(a[0], m); add(b[0], m); }
  } else if (da === 'h') add(b[0], a[1]);
  else add(a[0], b[1]);
  add(b[0], b[1]);
  return pts;
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
  let max = 0;
  for (let i = 0; i < lines.length; i++) if (lines[i].length > max) max = lines[i].length;
  const w = max * fs * 0.62 + 10;
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
  const rot = el.rot || 0;
  if (!rot) {
    for (let i = 0; i + 1 < p.length; i += 2) {
      out.push([el.x + (p[i] / 10000) * el.w, el.y + (p[i + 1] / 10000) * el.h]);
    }
    return out;
  }
  const cx = el.x + el.w / 2, cy = el.y + el.h / 2;
  const r = rot * RAD, c = Math.cos(r), s = Math.sin(r);
  for (let i = 0; i + 1 < p.length; i += 2) {
    const dx = el.x + (p[i] / 10000) * el.w - cx, dy = el.y + (p[i + 1] / 10000) * el.h - cy;
    out.push([cx + dx * c - dy * s, cy + dx * s + dy * c]);
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
  const stack = [0, pts.length - 1];
  while (stack.length) {
    const e = stack.pop(), s = stack.pop();
    let best = -1, bi = -1;
    for (let i = s + 1; i < e; i++) {
      const d = distToSegment(pts[i][0], pts[i][1], pts[s], pts[e]);
      if (d > best) { best = d; bi = i; }
    }
    if (best > eps) {
      keep[bi] = 1;
      stack.push(s, bi, bi, e);
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

// rotatedRect is the axis-aligned box of a rotated element, without
// building its corner points.
function rotatedRect(el) {
  const cx = el.x + el.w / 2, cy = el.y + el.h / 2;
  const r = (el.rot || 0) * RAD, c = Math.cos(r), s = Math.sin(r);
  let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
  const see = (px, py) => {
    const dx = px - cx, dy = py - cy;
    const x = cx + dx * c - dy * s, y = cy + dx * s + dy * c;
    if (x < x0) x0 = x; if (x > x1) x1 = x;
    if (y < y0) y0 = y; if (y > y1) y1 = y;
  };
  see(el.x, el.y); see(el.x + el.w, el.y); see(el.x + el.w, el.y + el.h); see(el.x, el.y + el.h);
  return { x: x0, y: y0, w: x1 - x0, h: y1 - y0 };
}

// elementBounds is the axis-aligned box of an element, including the
// derived path of a connector.
export function elementBounds(el, get) {
  if (el.type === 'connector') return connectorBounds(el, get);
  const r = el.rot ? rotatedRect(el) : { x: el.x, y: el.y, w: el.w, h: el.h };
  if (el.type === 'pen') return inflate(r, penWidth(el) / 2 + 1);
  if (el.type === 'frame') return { x: r.x, y: r.y - 28, w: r.w, h: r.h + 28 };
  return r;
}

export function insideBox(el, wx, wy) {
  toLocalScalars(el, wx, wy);
  return loc.x >= el.x && loc.x <= el.x + el.w && loc.y >= el.y && loc.y <= el.y + el.h;
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
    toLocalScalars(el, wx, wy);
    const lx = loc.x, ly = loc.y;
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
  normalized(el, wx, wy);
  const ux = nrm.x, uy = nrm.y;
  const r = outlineNorm(el, ux, uy);
  if (r === 0 || (Math.abs(r - 1) * Math.min(el.w, el.h)) / 2 > tol) return null;
  let ax = 0.5 + ux / r, ay = 0.5 + uy / r;
  const shape = el.type === 'shape' ? style(el, 'shape') : 'rect';
  if (shape === 'rect' || shape === 'round') {
    // A box side: the nearest point on it, not the one toward the center.
    ax = 0.5 + ux; ay = 0.5 + uy;
    if ((0.5 - Math.abs(ux)) * el.w < (0.5 - Math.abs(uy)) * el.h) ax = ux < 0 ? 0 : 1;
    else ay = uy < 0 ? 0 : 1;
  }
  ax = Math.max(0, Math.min(1, ax)); ay = Math.max(0, Math.min(1, ay));
  for (const m of MIDS) if (Math.abs(m[0] - ax) < 0.2 && Math.abs(m[1] - ay) < 0.2) return m.slice();
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
