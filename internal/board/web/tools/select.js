// Select tool (rule 15, TECH 4.2, 11.2): click and Shift-click selection,
// area selection, moving with snapping and frame membership, resizing,
// rotating and dragging connector ends. Each drag sends one batch on release.

import { endWorld, attachAt, rotateAround, toLocal, boxCorners, union, elementBounds, containsRect } from '../geom.js';
import { clampGeom, clampPoint, geomOf, round1 } from '../schema.js';
import * as ops from '../ops.js';
import { snapMove, drawGuides } from './snap.js';
import { toast } from '../ui.js';

const HANDLE = 9;
const CURSORS = { nw: 'nwse-resize', se: 'nwse-resize', ne: 'nesw-resize', sw: 'nesw-resize', n: 'ns-resize', s: 'ns-resize', e: 'ew-resize', w: 'ew-resize', rot: 'grab', end0: 'crosshair', end1: 'crosshair' };

export class SelectTool {
  constructor(app) {
    this.app = app;
    this.drag = null;
    this.guides = null;
    this.lockedHint = 0;
  }

  // handles lists the manipulation handles of the selection in screen
  // coordinates.
  handles() {
    const app = this.app;
    if (!app.canEdit || app.editor) return [];
    const els = app.selected().filter((el) => !el.locked);
    if (!els.length || els.length !== app.sel.size) return [];
    const S = (x, y) => app.toScreen(x, y);
    if (els.length === 1 && els[0].type === 'connector') {
      return els[0].ends.map((e, i) => {
        const p = endWorld(e, (id) => app.doc.get(id));
        return p ? { kind: 'end' + i, at: S(...p), attached: !!e.el } : null;
      }).filter(Boolean);
    }
    const geo = els.filter((el) => el.type !== 'connector');
    if (!geo.length) return [];
    if (geo.length === 1) {
      const el = geo[0];
      const [nw, ne, se, sw] = boxCorners(el).map(([x, y]) => S(x, y));
      const mid = (a, b) => [(a[0] + b[0]) / 2, (a[1] + b[1]) / 2];
      const out = [
        { kind: 'nw', at: nw }, { kind: 'ne', at: ne }, { kind: 'se', at: se }, { kind: 'sw', at: sw },
        { kind: 'n', at: mid(nw, ne) }, { kind: 'e', at: mid(ne, se) }, { kind: 's', at: mid(se, sw) }, { kind: 'w', at: mid(sw, nw) },
      ];
      if (el.type !== 'frame') {
        // The rotation handle sits off the top-right corner, clear of the
        // quick-create handles beside the sides.
        const c = S(el.x + el.w / 2, el.y + el.h / 2);
        const d = Math.hypot(ne[0] - c[0], ne[1] - c[1]) || 1;
        out.push({ kind: 'rot', at: [ne[0] + ((ne[0] - c[0]) / d) * 22, ne[1] + ((ne[1] - c[1]) / d) * 22], from: ne });
      }
      return out;
    }
    let b = null;
    for (const el of geo) b = union(b, elementBounds(el, (id) => app.doc.get(id)));
    const [x0, y0] = S(b.x, b.y), [x1, y1] = S(b.x + b.w, b.y + b.h);
    return [{ kind: 'nw', at: [x0, y0] }, { kind: 'ne', at: [x1, y0] }, { kind: 'se', at: [x1, y1] }, { kind: 'sw', at: [x0, y1] }];
  }

  handleAt(s) {
    for (const hd of this.handles()) if (Math.abs(hd.at[0] - s[0]) <= 8 && Math.abs(hd.at[1] - s[1]) <= 8) return hd;
    return null;
  }

  hover(e, p, s) {
    const hd = this.handleAt(s);
    this.app.renderer.dyn.style.cursor = hd ? CURSORS[hd.kind] : '';
  }

  down(e, p, s) {
    const app = this.app;
    const hd = this.handleAt(s);
    if (hd) { this.startHandle(hd, p, e); return; }
    const el = app.hitAt(p[0], p[1]);
    if (!el) {
      const locked = app.hitAt(p[0], p[1], { locked: true });
      if (locked && locked.locked && performance.now() - this.lockedHint > 8000) {
        this.lockedHint = performance.now();
        toast('Locked elements are not selected by clicking. Right-click one or use the Locked elements list to unlock it.', { actions: [['Locked elements', () => app.locked.open()]] });
      }
      if (!e.shiftKey) app.select([]);
      this.drag = { mode: 'marquee', p0: p, p1: p, base: e.shiftKey ? new Set(app.sel) : new Set() };
      return;
    }
    const ids = app.expandGroups([el.id]);
    if (e.shiftKey) {
      const has = app.sel.has(el.id);
      for (const id of ids) has ? app.sel.delete(id) : app.sel.add(id);
      app.selectionChanged();
    } else if (!app.sel.has(el.id)) app.select(ids);
    this.drag = { mode: 'press', p0: p, el, shift: e.shiftKey, alt: e.altKey };
  }

  startHandle(hd, p, e) {
    const app = this.app;
    const els = app.selected();
    if (hd.kind.startsWith('end')) {
      this.drag = { mode: 'end', i: Number(hd.kind[3]), conn: els[0], p0: p };
      return;
    }
    if (hd.kind === 'rot') { this.drag = { mode: 'rotate', el: els[0], p0: p }; return; }
    const geo = els.filter((el) => el.type !== 'connector');
    if (geo.length === 1) { this.drag = { mode: 'resize', kind: hd.kind, el: geo[0], p0: p }; return; }
    // Several elements scale about the opposite corner; members of selected
    // frames scale with them because resizing a frame never carries members.
    const set = new Map();
    for (const el of geo) {
      set.set(el.id, el);
      if (el.type === 'frame') for (const m of ops.membersOf(app.state, el.id)) if (!m.locked) set.set(m.id, m);
    }
    for (const el of els) if (el.type === 'connector') set.set(el.id, el);
    let b = null;
    for (const el of set.values()) if (el.type !== 'connector') b = union(b, { x: el.x, y: el.y, w: el.w, h: el.h });
    this.drag = { mode: 'scale', kind: hd.kind, els: [...set.values()], box: b, p0: p, shift: e.shiftKey };
  }

  move(e, p) {
    const app = this.app;
    const d = this.drag;
    if (!d) return;
    if (d.mode === 'press') {
      const far = Math.hypot(p[0] - d.p0[0], p[1] - d.p0[1]) * app.cam.z > 3;
      if (!far || !app.canEdit) return;
      const units = ops.moveUnits(app.state, [...app.sel]);
      if (!units.length) { this.drag = null; return; }
      let box = null;
      const moving = new Set();
      for (const u of units) {
        box = union(box, ops.unitBounds(app.state, u));
        moving.add(u.el.id);
        for (const m of u.members) moving.add(m.id);
      }
      Object.assign(d, { mode: 'move', units, box, moving });
      app.styleBar.busy = true;
      app.styleBar.hide();
    }
    if (d.mode === 'move') {
      const snap = snapMove(app, d.box, p[0] - d.p0[0], p[1] - d.p0[1], d.moving, e.altKey);
      d.dx = snap.dx; d.dy = snap.dy;
      this.guides = snap.guides;
      app.setPreview(ops.movePreview(app.state, d.units, d.dx, d.dy));
      const drag = {};
      for (const u of d.units.slice(0, 30)) {
        if (u.kind === 'conn') continue;
        const g = app.state.get(u.el.id);
        drag[u.el.id] = [round1(g.x), round1(g.y), g.w, g.h, g.rot || 0];
      }
      app.presence.send({ drag });
    } else if (d.mode === 'marquee') {
      d.p1 = p;
      const r = rectOf(d.p0, p);
      const ids = new Set(d.base);
      for (const id of app.doc.index.query(r)) {
        const el = app.doc.get(id);
        if (!el || el.locked) continue;
        const b = app.doc.index.bounds(id);
        if (b && containsRect(r, el.type === 'frame' ? { x: el.x, y: el.y, w: el.w, h: el.h } : b)) ids.add(id);
      }
      app.sel = ids;
      app.selectionChanged();
    } else if (d.mode === 'resize') {
      d.geom = resizeGeom(d.el, d.kind, p, e.shiftKey !== (d.el.type === 'image'));
      app.setPreview(new Map([[d.el.id, { ...d.el, ...d.geom }]]));
    } else if (d.mode === 'scale') {
      d.map = scaleMap(d, p);
      app.setPreview(d.map);
    } else if (d.mode === 'rotate') {
      const c = [d.el.x + d.el.w / 2, d.el.y + d.el.h / 2];
      let deg = (Math.atan2(p[1] - c[1], p[0] - c[0]) * 180) / Math.PI + 90;
      if (e.shiftKey) deg = Math.round(deg / 15) * 15;
      d.rot = round1(((deg % 360) + 360) % 360);
      app.setPreview(new Map([[d.el.id, { ...d.el, rot: d.rot }]]));
    } else if (d.mode === 'end') {
      const target = e.altKey ? null : app.hitAt(p[0], p[1], { filter: (el) => el.type !== 'connector' && el.type !== 'pen' });
      const toward = endWorld(d.conn.ends[1 - d.i], (id) => app.doc.get(id)) || p;
      d.val = target ? { el: target.id, at: attachAt(target, p[0], p[1], 8 / app.cam.z, toward) } : { pt: clampPoint(p) };
      d.target = target;
      const ends = d.conn.ends.slice();
      ends[d.i] = d.val;
      app.setPreview(new Map([[d.conn.id, { ...d.conn, ends }]]));
    }
    app.renderer.dynamic();
  }

  up(e, p) {
    const app = this.app;
    const d = this.drag;
    this.drag = null;
    this.guides = null;
    app.styleBar.busy = false;
    if (!d) return;
    if (d.mode !== 'press' && d.mode !== 'marquee') app.setPreview(null);
    if (d.mode === 'press') {
      if (!d.shift && app.sel.size > 1) app.select(app.expandGroups([d.el.id]));
    } else if (d.mode === 'move') {
      app.presence.send({ drag: null });
      app.commit(ops.moveOps(app.state, [...app.sel], d.dx, d.dy, { reparent: true }));
    } else if (d.mode === 'resize' && d.geom) {
      app.commit([{ op: 'set', id: d.el.id, inc: app.state.incOf(d.el.id), g: 'geom', val: clampGeom({ ...geomOf(d.el), ...d.geom }) }]);
    } else if (d.mode === 'scale' && d.map) {
      const list = [];
      for (const [id, n] of d.map) {
        if (n.type === 'connector') {
          n.ends.forEach((end, i) => { if (end.pt) list.push({ op: 'set', id, inc: app.state.incOf(id), g: 'end' + i, val: { pt: clampPoint(end.pt) } }); });
        } else list.push({ op: 'set', id, inc: app.state.incOf(id), g: 'geom', val: clampGeom(geomOf(n)) });
      }
      app.commit(list);
    } else if (d.mode === 'rotate' && d.rot != null) {
      app.commit([{ op: 'set', id: d.el.id, inc: app.state.incOf(d.el.id), g: 'geom', val: clampGeom({ ...geomOf(d.el), rot: d.rot }) }]);
    } else if (d.mode === 'end' && d.val) {
      const op = { op: 'set', id: d.conn.id, inc: app.state.incOf(d.conn.id), g: 'end' + d.i, val: d.val };
      if (d.val.el) op.refs = { [d.val.el]: app.state.incOf(d.val.el) };
      app.commit([op]);
    }
    app.styleBar.update();
    app.renderer.dynamic();
  }

  cancel() {
    if (this.drag && this.drag.mode !== 'press' && this.drag.mode !== 'marquee') this.app.setPreview(null);
    this.drag = null;
    this.guides = null;
    this.app.styleBar.busy = false;
    this.app.renderer.dynamic();
  }

  dblclick(e, p) {
    const app = this.app;
    if (!app.canEdit) return;
    const el = app.hitAt(p[0], p[1]);
    if (el) { if (el.type !== 'pen' && el.type !== 'image') { app.select([el.id]); app.editText(el.id); } return; }
    app.tools.text.createAt(p);
  }

  draw(ctx, r) {
    const t = r.theme;
    const d = this.drag;
    if (d && d.mode === 'marquee') {
      const a = r.toScreen(...d.p0), b = r.toScreen(...d.p1);
      ctx.fillStyle = t.sel;
      ctx.globalAlpha = 0.08;
      ctx.fillRect(Math.min(a[0], b[0]), Math.min(a[1], b[1]), Math.abs(a[0] - b[0]), Math.abs(a[1] - b[1]));
      ctx.globalAlpha = 1;
      ctx.strokeStyle = t.sel;
      ctx.lineWidth = 1;
      ctx.strokeRect(Math.min(a[0], b[0]) + 0.5, Math.min(a[1], b[1]) + 0.5, Math.abs(a[0] - b[0]), Math.abs(a[1] - b[1]));
    }
    drawGuides(ctx, r, this.guides);
    if (d && d.mode !== 'press') return;
    for (const hd of this.handles()) {
      const [x, y] = hd.at;
      ctx.lineWidth = 1.5;
      ctx.strokeStyle = t.sel;
      if (hd.kind === 'rot') {
        ctx.beginPath(); ctx.moveTo(...hd.from); ctx.lineTo(x, y); ctx.stroke();
        ctx.beginPath(); ctx.arc(x, y, 7, 0, Math.PI * 2);
        ctx.fillStyle = t.paper; ctx.fill(); ctx.stroke();
      } else if (hd.kind.startsWith('end')) {
        ctx.beginPath(); ctx.arc(x, y, 5.5, 0, Math.PI * 2);
        ctx.fillStyle = hd.attached ? t.sel : t.paper;
        ctx.fill();
        ctx.strokeStyle = hd.attached ? t.paper : t.sel;
        ctx.lineWidth = 2;
        ctx.stroke();
      } else {
        ctx.fillStyle = t.paper;
        ctx.fillRect(x - HANDLE / 2, y - HANDLE / 2, HANDLE, HANDLE);
        ctx.strokeRect(x - HANDLE / 2, y - HANDLE / 2, HANDLE, HANDLE);
      }
    }
  }
}

function rectOf(a, b) {
  return { x: Math.min(a[0], b[0]), y: Math.min(a[1], b[1]), w: Math.abs(a[0] - b[0]), h: Math.abs(a[1] - b[1]) };
}

// resizeGeom moves the dragged edges in the element's own (unrotated)
// coordinates and keeps the opposite edges in place.
function resizeGeom(el, kind, p, keepRatio) {
  const [lx, ly] = toLocal(el, p[0], p[1]);
  let l = el.x, t = el.y, r = el.x + el.w, b = el.y + el.h;
  const MIN = 8;
  if (kind.includes('e')) r = Math.max(lx, l + MIN);
  if (kind.includes('w')) l = Math.min(lx, r - MIN);
  if (kind.includes('s')) b = Math.max(ly, t + MIN);
  if (kind.includes('n')) t = Math.min(ly, b - MIN);
  if (keepRatio && kind.length === 2) {
    const ratio = el.w / el.h;
    const w = r - l, hh = b - t;
    if (w / hh > ratio) {
      const nw = hh * ratio;
      if (kind.includes('w')) l = r - nw; else r = l + nw;
    } else {
      const nh = w / ratio;
      if (kind.includes('n')) t = b - nh; else b = t + nh;
    }
  }
  const cx0 = el.x + el.w / 2, cy0 = el.y + el.h / 2;
  const [cx, cy] = rotateAround((l + r) / 2, (t + b) / 2, cx0, cy0, el.rot || 0);
  return { x: cx - (r - l) / 2, y: cy - (b - t) / 2, w: r - l, h: b - t };
}

function scaleMap(d, p) {
  const b = d.box;
  const ax = d.kind.includes('w') ? b.x + b.w : b.x;
  const ay = d.kind.includes('n') ? b.y + b.h : b.y;
  let sx = Math.max(0.05, Math.abs(p[0] - ax) / b.w);
  let sy = Math.max(0.05, Math.abs(p[1] - ay) / b.h);
  if (!d.shift) sx = sy = Math.max(sx, sy);
  const out = new Map();
  for (const el of d.els) {
    if (el.type === 'connector') {
      out.set(el.id, { ...el, ends: el.ends.map((e) => (e.pt ? { pt: clampPoint([ax + (e.pt[0] - ax) * sx, ay + (e.pt[1] - ay) * sy]) } : e)) });
    } else {
      out.set(el.id, { ...el, x: ax + (el.x - ax) * sx, y: ay + (el.y - ay) * sy, w: el.w * sx, h: el.h * sy });
    }
  }
  return out;
}
