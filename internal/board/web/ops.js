// Operation builders: turn one person action into one batch of client
// operations (TECH 4.1 to 4.5, Appendix A). The view argument is the
// BoardState (get, incOf, all).

import { newId, newGroupId, keyBetween, keysBetween, zCompare } from './ids.js';
import { clampGeom, clampPoint, contentOf, geomOf } from './schema.js';
import { elementBounds, elementCenter, endWorld, insideBox, union, center, sideAnchor, rectFromPoints } from './geom.js';

const get = (view) => (id) => view.get(id);

export function incOf(view, ids) {
  const out = {};
  for (const id of ids) out[id] = view.incOf(id);
  return out;
}

const live = (view, ids) => {
  const out = [];
  for (const id of ids) { const el = view.get(id); if (el) out.push(el); }
  return out;
};

// ---- stacking order ----

export function zRange(view) {
  let lo = null, hi = null;
  for (const el of view.all()) {
    if (lo == null || el.z < lo) lo = el.z;
    if (hi == null || el.z > hi) hi = el.z;
  }
  return [lo, hi];
}

// ---- membership ----

// topFrameAt is the topmost frame containing a canvas point (rule 3).
export function topFrameAt(view, x, y, exclude) {
  let best = null;
  for (const el of view.all()) {
    if (el.type !== 'frame' || (exclude && exclude.has(el.id))) continue;
    if (!insideBox(el, x, y)) continue;
    if (!best || zCompare(el, best) > 0) best = el;
  }
  return best;
}

export function membersOf(view, frameId) {
  const out = [];
  for (const el of view.all()) if (el.frame === frameId) out.push(el);
  return out;
}

export function groupMembers(view, group) {
  const out = [];
  for (const el of view.all()) if (el.group === group) out.push(el);
  return out;
}

// ---- creation ----

export function newElement(view, type, geom, extra = {}) {
  const el = { id: newId(), type, locked: false, ...extra };
  if (el.z == null) {
    const [lo, hi] = zRange(view);
    el.z = type === 'frame' ? keyBetween(null, lo) : keyBetween(hi, null);
  }
  if (type !== 'connector') Object.assign(el, clampGeom(geom));
  if (type !== 'frame' && el.frame === undefined) {
    const [cx, cy] = type === 'connector' ? elementCenter(el, get(view)) : [el.x + el.w / 2, el.y + el.h / 2];
    const f = topFrameAt(view, cx, cy);
    if (f) el.frame = f.id;
  }
  return el;
}

// addOp builds an add with the expected incarnations of what el references.
export function addOp(view, el, local = new Set()) {
  const refs = {};
  const ref = (id) => { refs[id] = local.has(id) ? 0 : view.incOf(id); };
  if (el.frame) ref(el.frame);
  if (el.type === 'connector') for (const e of el.ends) if (e.el) ref(e.el);
  const op = { op: 'add', el };
  if (Object.keys(refs).length) op.refs = refs;
  return op;
}

// ---- moves (4.2) ----

// moveUnits groups a selection into what one move sends: a frame carries its
// members, a group moves through one member with carry "group", connectors
// move their free ends and everything else moves itself. Members of a moved
// frame are left out, so nothing moves twice.
export function moveUnits(view, ids) {
  const sel = [];
  for (const el of live(view, ids)) if (!el.locked) sel.push(el);
  const frames = new Set();
  for (const el of sel) if (el.type === 'frame') frames.add(el.id);
  const units = [];
  const groups = new Set();
  for (const el of sel) {
    if (el.type === 'frame') { units.push({ kind: 'frame', el, members: membersOf(view, el.id) }); continue; }
    if (el.frame && frames.has(el.frame)) continue;
    if (el.group) {
      if (groups.has(el.group)) continue;
      groups.add(el.group);
      const members = groupMembers(view, el.group);
      const rep = sel.find((m) => m.group === el.group && m.type !== 'connector')
        || members.find((m) => m.type !== 'connector' && !m.locked);
      if (rep) { units.push({ kind: 'group', el: rep, members }); continue; }
      for (const m of sel) if (m.group === el.group) units.push({ kind: 'conn', el: m, members: [m] });
      continue;
    }
    units.push({ kind: el.type === 'connector' ? 'conn' : 'single', el, members: [el] });
  }
  return units;
}

export function unitBounds(view, u) {
  if (u.kind === 'frame') return { x: u.el.x, y: u.el.y, w: u.el.w, h: u.el.h };
  let r = null;
  for (const m of u.members) r = union(r, elementBounds(m, get(view)));
  return r;
}

function shiftOps(view, u, dx, dy) {
  if (!dx && !dy) return [];
  if (u.kind === 'conn') {
    const out = [];
    u.el.ends.forEach((e, i) => {
      if (e.pt) out.push({ op: 'set', id: u.el.id, inc: view.incOf(u.el.id), g: 'end' + i, val: { pt: clampPoint([e.pt[0] + dx, e.pt[1] + dy]) } });
    });
    return out;
  }
  const el = u.el;
  const op = { op: 'set', id: el.id, inc: view.incOf(el.id), g: 'geom', val: clampGeom({ ...geomOf(el), x: el.x + dx, y: el.y + dy }) };
  if (u.kind === 'frame') op.carry = 'members';
  if (u.kind === 'group') op.carry = 'group';
  return [op];
}

// movePreview returns the elements as they look during a drag by (dx, dy),
// including every member that follows its container.
export function movePreview(view, units, dx, dy) {
  const out = new Map();
  const shift = (m) => {
    if (m.type === 'connector') {
      out.set(m.id, { ...m, ends: m.ends.map((e) => (e.pt ? { pt: clampPoint([e.pt[0] + dx, e.pt[1] + dy]) } : e)) });
    } else out.set(m.id, { ...m, x: m.x + dx, y: m.y + dy });
  };
  for (const u of units) {
    if (u.kind === 'frame') { shift(u.el); u.members.forEach(shift); } else u.members.forEach(shift);
  }
  return out;
}

export function moveOps(view, ids, dx, dy, { reparent = false } = {}) {
  const units = moveUnits(view, ids);
  const ops = [];
  for (const u of units) for (const op of shiftOps(view, u, dx, dy)) ops.push(op);
  if (reparent) for (const op of reparentOps(view, units, dx, dy)) ops.push(op);
  return ops;
}

// reparentOps adds the membership change for elements whose center ended a
// drag inside a different frame or outside their frame (4.5).
export function reparentOps(view, units, dx, dy) {
  const moving = new Set();
  for (const u of units) if (u.kind === 'frame') moving.add(u.el.id);
  const byTarget = new Map();
  for (const u of units) {
    if (u.kind === 'frame') continue;
    const b = unitBounds(view, u);
    if (!b) continue;
    const [cx, cy] = center(b);
    const f = topFrameAt(view, cx + dx, cy + dy, moving);
    const to = f ? f.id : null;
    if ((u.el.frame || null) === to) continue;
    if (!byTarget.has(to)) byTarget.set(to, []);
    byTarget.get(to).push(u.el.id);
  }
  const ops = [];
  for (const [to, ids] of byTarget) {
    const op = { op: 'reparent', ids, inc_of: incOf(view, ids), frame: to };
    if (to) op.frame_inc = view.incOf(to);
    ops.push(op);
  }
  return ops;
}

// ---- align and distribute (one batch of set geom, with carry) ----

export function alignOps(view, ids, mode) {
  const units = moveUnits(view, ids).filter((u) => u.kind !== 'conn');
  if (units.length < 2) return [];
  const boxes = units.map((u) => unitBounds(view, u));
  let minX = Infinity, maxX = -Infinity, minY = Infinity, maxY = -Infinity;
  for (const b of boxes) {
    if (b.x < minX) minX = b.x;
    if (b.x + b.w > maxX) maxX = b.x + b.w;
    if (b.y < minY) minY = b.y;
    if (b.y + b.h > maxY) maxY = b.y + b.h;
  }
  const ops = [];
  units.forEach((u, i) => {
    const b = boxes[i];
    let dx = 0, dy = 0;
    if (mode === 'left') dx = minX - b.x;
    else if (mode === 'center') dx = (minX + maxX) / 2 - (b.x + b.w / 2);
    else if (mode === 'right') dx = maxX - (b.x + b.w);
    else if (mode === 'top') dy = minY - b.y;
    else if (mode === 'middle') dy = (minY + maxY) / 2 - (b.y + b.h / 2);
    else if (mode === 'bottom') dy = maxY - (b.y + b.h);
    for (const op of shiftOps(view, u, dx, dy)) ops.push(op);
  });
  return ops;
}

export function distributeOps(view, ids, axis) {
  const units = moveUnits(view, ids).filter((u) => u.kind !== 'conn');
  if (units.length < 3) return [];
  const items = units.map((u) => ({ u, b: unitBounds(view, u) }));
  const pos = axis === 'h' ? 'x' : 'y', size = axis === 'h' ? 'w' : 'h';
  items.sort((a, b) => a.b[pos] - b.b[pos]);
  const first = items[0].b, last = items[items.length - 1].b;
  const span = last[pos] + last[size] - first[pos];
  const total = items.reduce((s, it) => s + it.b[size], 0);
  const gap = (span - total) / (items.length - 1);
  let at = first[pos];
  const ops = [];
  for (const it of items) {
    const d = at - it.b[pos];
    for (const op of shiftOps(view, it.u, axis === 'h' ? d : 0, axis === 'h' ? 0 : d)) ops.push(op);
    at += it.b[size] + gap;
  }
  return ops;
}

// ---- simple operations ----

export function deleteOps(view, ids) {
  const targets = [];
  for (const el of live(view, ids)) if (!el.locked) targets.push(el.id);
  return targets.length ? [{ op: 'delete', ids: targets, inc_of: incOf(view, targets) }] : [];
}

export function deleteFrameContentsOps(view, frameIds) {
  const ops = [];
  for (const f of live(view, frameIds)) {
    if (f.type === 'frame' && !f.locked) ops.push({ op: 'delete_frame_contents', frame: f.id, inc: view.incOf(f.id) });
  }
  return ops;
}

export function setOps(view, ids, g, val, { allowLocked = false } = {}) {
  const ops = [];
  for (const el of live(view, ids)) {
    if (allowLocked || !el.locked) ops.push({ op: 'set', id: el.id, inc: view.incOf(el.id), g, val });
  }
  return ops;
}

export const styleOps = (view, ids, key, val) => setOps(view, ids, 'style.' + key, val);
export const lockOps = (view, ids, locked) => setOps(view, ids, 'locked', locked, { allowLocked: true });

export function zorderOps(view, ids, dir) {
  const targets = [];
  for (const el of live(view, ids)) if (!el.locked) targets.push(el);
  if (!targets.length) return [];
  const tids = targets.map((el) => el.id);
  let to = dir;
  if (dir === 'forward' || dir === 'backward') {
    const box = targets.reduce((r, el) => union(r, elementBounds(el, get(view))), null);
    const sorted = [...view.all()].sort(zCompare);
    const sel = new Set(tids);
    // Only the outermost selected positions matter; find them in one pass.
    let min = -1, max = -1;
    for (let i = 0; i < sorted.length; i++) {
      if (!sel.has(sorted[i].id)) continue;
      if (min < 0) min = i;
      max = i;
    }
    if (min < 0) return [];
    const overlaps = (el) => { const b = elementBounds(el, get(view)); return b && box && b.x < box.x + box.w && box.x < b.x + b.w && b.y < box.y + box.h && box.y < b.y + b.h; };
    let ref = null;
    if (dir === 'forward') {
      for (let i = max + 1; i < sorted.length && !ref; i++) if (!sel.has(sorted[i].id) && overlaps(sorted[i])) ref = sorted[i];
      if (!ref) return [];
      to = { after: ref.id, inc: view.incOf(ref.id) };
    } else {
      for (let i = min - 1; i >= 0 && !ref; i--) if (!sel.has(sorted[i].id) && overlaps(sorted[i])) ref = sorted[i];
      if (!ref) return [];
      to = { before: ref.id, inc: view.incOf(ref.id) };
    }
  }
  return [{ op: 'zorder', ids: tids, inc_of: incOf(view, tids), to }];
}

export function groupOps(view, ids) {
  const targets = [];
  for (const el of live(view, ids)) if (!el.locked) targets.push(el.id);
  if (targets.length < 2) return [];
  return [{ op: 'group', ids: targets, inc_of: incOf(view, targets), group: newGroupId() }];
}

export function ungroupOps(view, ids) {
  const groups = new Set();
  for (const el of live(view, ids)) if (el.group) groups.add(el.group);
  const ops = [];
  for (const group of groups) ops.push({ op: 'ungroup', group });
  return ops;
}

// textVersion is the server's v.text of an element, or null when the server
// copy has none yet (an element this session created).
export function textVersion(view, id) {
  const base = view.server.get(id);
  return base && base.v && base.v.text != null ? base.v.text : null;
}

// textOp sets the text. baseV is the v.text the person saw when editing
// started (TECH 4.9), so a concurrent change in between is reported as
// overwritten; it is not read again here.
export function textOp(view, el, text, baseV) {
  const op = { op: 'set', id: el.id, inc: view.incOf(el.id), g: 'text', val: text };
  if (baseV != null) op.base_v = baseV;
  return op;
}

// ---- clipboard (rule 19, 4.4) ----

// copyPayload captures elements for the board clipboard. Selected frames and
// groups bring their members. References that leave the copied range become
// free ends and no membership, at their coordinates of this moment.
export function copyPayload(view, ids) {
  const set = new Set();
  for (const el of live(view, ids)) {
    set.add(el.id);
    if (el.type === 'frame') for (const m of membersOf(view, el.id)) set.add(m.id);
    if (el.group) for (const m of groupMembers(view, el.group)) set.add(m.id);
  }
  const groupCount = new Map();
  for (const id of set) { const g = view.get(id).group; if (g) groupCount.set(g, (groupCount.get(g) || 0) + 1); }
  const elements = [];
  for (const id of set) {
    const c = contentOf(view.get(id));
    delete c.by; delete c.at;
    c.locked = false;
    if (c.frame && !set.has(c.frame)) delete c.frame;
    if (c.group && groupCount.get(c.group) < 2) delete c.group;
    if (c.type === 'connector') {
      c.ends = c.ends.map((e) => (e.el && !set.has(e.el) ? { pt: clampPoint(endWorld(e, get(view))) } : e));
    }
    elements.push(c);
  }
  elements.sort(zCompare);
  return { format: 1, elements };
}

function payloadBounds(elements) {
  const byId = new Map();
  for (const e of elements) byId.set(e.id, e);
  const g = (id) => byId.get(id) || null;
  let r = null;
  for (const el of elements) {
    if (el.type === 'connector') {
      const pts = [];
      for (const e of el.ends) { const p = endWorld(e, g); if (p) pts.push(p); }
      if (pts.length) r = union(r, rectFromPoints(pts));
    } else r = union(r, { x: el.x, y: el.y, w: el.w, h: el.h });
  }
  return r;
}

// pasteOps places a clipboard payload centered at `at` with new ids,
// relinking references inside it. It returns the batch and the new ids.
export function pasteOps(view, payload, at) {
  const els = payload.elements;
  if (!els.length) return { ops: [], ids: [] };
  const b = payloadBounds(els);
  const [cx, cy] = center(b);
  const dx = at[0] - cx, dy = at[1] - cy;
  const ids = new Map();
  for (const e of els) ids.set(e.id, newId());
  const groups = new Map();
  for (const e of els) if (e.group && !groups.has(e.group)) groups.set(e.group, newGroupId());
  const [lo, hi] = zRange(view);
  const frames = [], others = [];
  for (const e of els) {
    if (e.type === 'frame') frames.push(e); else others.push(e);
  }
  const frameZ = keysBetween(null, lo, frames.length);
  const otherZ = keysBetween(hi, null, others.length);
  const out = [];
  frames.forEach((e, i) => out.push({ ...e, id: ids.get(e.id), z: frameZ[i] }));
  others.forEach((e, i) => out.push({ ...e, id: ids.get(e.id), z: otherZ[i] }));
  for (const n of out) {
    if (n.type === 'connector') {
      n.ends = n.ends.map((e) => (e.el ? { el: ids.get(e.el), at: e.at } : { pt: clampPoint([e.pt[0] + dx, e.pt[1] + dy]) }));
    } else Object.assign(n, clampGeom({ ...geomOf(n), x: n.x + dx, y: n.y + dy }));
    if (n.frame) n.frame = ids.get(n.frame);
    if (n.group) n.group = groups.get(n.group);
  }
  // Top-level pasted elements join the topmost existing frame at their
  // center; a group joins as a whole so its members share a frame.
  const local = new Map();
  for (const e of out) local.set(e.id, e);
  const lget = (id) => local.get(id) || view.get(id);
  const placed = new Set();
  for (const n of out) {
    if (n.type === 'frame' || n.frame || placed.has(n.id)) continue;
    const members = [n];
    if (n.group) {
      members.length = 0;
      for (const m of out) if (m.group === n.group) members.push(m);
    }
    let r = null;
    for (const m of members) r = union(r, elementBounds(m, lget));
    const [mx, my] = r ? center(r) : elementCenter(n, lget);
    const f = topFrameAt(view, mx, my);
    for (const m of members) { placed.add(m.id); if (f) m.frame = f.id; }
  }
  // Frames first, then plain elements, then connectors, so adds never
  // reference a target that does not exist yet.
  const order = [];
  for (const e of out) if (e.type === 'frame') order.push(e);
  for (const e of out) if (e.type !== 'frame' && e.type !== 'connector') order.push(e);
  for (const e of out) if (e.type === 'connector') order.push(e);
  const newIds = new Set();
  for (const e of out) newIds.add(e.id);
  const ops = [], outIds = [];
  for (const e of order) { ops.push(addOp(view, e, newIds)); outIds.push(e.id); }
  return { ops, ids: outIds };
}

// pasteTextOps creates one sticky per non-empty line in a grid, or one text
// element holding everything.
export function pasteTextOps(view, text, at, asOne) {
  if (asOne) {
    const lines = text.split('\n');
    let max = 0;
    for (const l of lines) if (l.length > max) max = l.length;
    const w = Math.min(800, Math.max(160, max * 11));
    const el = newElement(view, 'text', { x: at[0] - w / 2, y: at[1], w, h: lines.length * 28 + 8 }, { text: text.slice(0, 4000) });
    return { ops: [addOp(view, el)], ids: [el.id] };
  }
  const lines = [];
  for (const raw of text.split(/\r?\n/)) { const l = raw.trim(); if (l) lines.push(l); }
  const cols = Math.min(5, Math.ceil(Math.sqrt(lines.length)));
  const ops = [], ids = [];
  const [, hi] = zRange(view);
  const zs = keysBetween(hi, null, lines.length);
  lines.forEach((line, i) => {
    const x = at[0] - (cols * 220) / 2 + (i % cols) * 220;
    const y = at[1] + Math.floor(i / cols) * 220;
    const el = newElement(view, 'sticky', { x, y, w: 200, h: 200 }, { text: line.slice(0, 4000), z: zs[i] });
    ops.push(addOp(view, el));
    ids.push(el.id);
  });
  return { ops, ids };
}

const QUICK_DIRS = { right: [1, 0], left: [-1, 0], down: [0, 1], up: [0, -1] };

// quickCreateOps creates an element of the same type and size 80px away from
// src in direction dir, connected to it (rule 16, 11.2).
export function quickCreateOps(view, src, dir) {
  const d = QUICK_DIRS[dir];
  const geom = { x: src.x + d[0] * (src.w + 80), y: src.y + d[1] * (src.h + 80), w: src.w, h: src.h, rot: 0 };
  const extra = { text: '' };
  if (src.style) extra.style = { ...src.style };
  const el = newElement(view, src.type, geom, extra);
  const local = new Set([el.id]);
  const conn = {
    id: newId(), type: 'connector', locked: false, text: '',
    ends: [{ el: src.id, at: sideAnchor(d[0], d[1]) }, { el: el.id, at: sideAnchor(-d[0], -d[1]) }],
  };
  conn.z = keyBetween(el.z, null);
  if (src.frame && src.frame === el.frame) conn.frame = src.frame;
  local.add(conn.id);
  return { ops: [addOp(view, el), addOp(view, conn, local)], el, conn };
}
