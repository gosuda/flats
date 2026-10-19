// Canvas 2D renderer (TECH 11.1): a static layer for elements and a dynamic
// layer for selection, presence and tool feedback, under a DOM overlay.
// The static layer redraws only dirty regions, shifts its bitmap while
// panning and shows a scaled bitmap while a zoom gesture runs.

import { currentTheme, stickyColor, inkColor, shapeFill, frameFill } from './theme.js';
import { style, authorName } from './schema.js';
import { connectorPoints, pointAlong, penWorldPoints, penWidth, labelBox, boxCorners } from './geom.js';
import { zCompare } from './ids.js';

export const FONT = '-apple-system, BlinkMacSystemFont, "Segoe UI", "Apple SD Gothic Neo", "Noto Sans KR", sans-serif';
const LINE = 1.38;
const MIN_FONT = 10;

// ---- text layout ----

const widths = new Map();
function measure(ctx, font, s) {
  const k = font + '|' + s;
  let w = widths.get(k);
  if (w === undefined) {
    if (widths.size > 60000) widths.clear();
    w = ctx.measureText(s).width;
    widths.set(k, w);
  }
  return w;
}

export function wrap(ctx, font, text, maxW) {
  ctx.font = font;
  const out = [];
  const pushChars = (s, line) => {
    for (const ch of s) {
      if (line && measure(ctx, font, line + ch) > maxW) { out.push(line); line = ch; } else line += ch;
    }
    return line;
  };
  for (const para of String(text).split('\n')) {
    let line = '';
    for (const tok of para.split(/(\s+)/)) {
      if (!tok) continue;
      const cand = line + tok;
      if (measure(ctx, font, cand) <= maxW) { line = cand; continue; }
      if (/^\s+$/.test(tok)) { out.push(line); line = ''; continue; }
      if (line) { out.push(line.trimEnd()); line = ''; }
      line = measure(ctx, font, tok) > maxW ? pushChars(tok, '') : tok;
    }
    out.push(line);
  }
  return out;
}

// fitText wraps text into a box, shrinking from size down to minSize (when
// shrink is set) and marking overflow when it still does not fit.
export function fitText(ctx, text, maxW, maxH, size, shrink, weight = 400) {
  for (let s = size; ; s--) {
    const font = `${weight} ${s}px ${FONT}`;
    const lines = wrap(ctx, font, text, maxW);
    const fit = lines.length * s * LINE <= maxH;
    if (fit || !shrink || s <= MIN_FONT) {
      const max = Math.max(1, Math.floor(maxH / (s * LINE)));
      return { font, size: s, lines: fit ? lines : lines.slice(0, max), overflow: !fit };
    }
  }
}

// ---- image cache ----

export class ImageCache {
  constructor(onLoad, budget) {
    this.onLoad = onLoad;
    this.budget = budget;
    this.entries = new Map(); // key -> {bmp, bytes, used}
    this.loading = new Set();
    this.failed = new Set();
    this.bytes = 0;
  }

  // get returns a decoded copy no smaller than needW pixels wide when one is
  // cached, starting the decode otherwise and returning any cached level.
  get(hash, natural, needW) {
    const nw = (natural && natural[0]) || 1024;
    let level = 0;
    for (let w = nw; w / 2 >= needW && level < 6; w /= 2) level++;
    const key = hash + '@' + level;
    const e = this.entries.get(key);
    if (e) { e.used = performance.now(); return e.bmp; }
    if (!this.loading.has(key) && !this.failed.has(hash)) this.#load(hash, level, key);
    // Until it arrives, show the nearest level already decoded.
    for (let d = 1; d <= 6; d++) {
      const other = this.entries.get(hash + '@' + (level + d)) || this.entries.get(hash + '@' + (level - d));
      if (other) return other.bmp;
    }
    return null;
  }

  #decode(hash) {
    return new Promise((resolve, reject) => {
      const img = new Image();
      img.onload = () => resolve(img);
      img.onerror = () => reject(new Error('image ' + hash + ' did not load'));
      img.src = '/_board/a/' + hash;
    });
  }

  async #load(hash, level, key) {
    this.loading.add(key);
    try {
      const img = await this.#decode(hash);
      const w = Math.max(1, Math.round(img.naturalWidth / 2 ** level));
      const h = Math.max(1, Math.round(img.naturalHeight / 2 ** level));
      const bmp = await scaled(img, w, h);
      this.entries.set(key, { bmp, bytes: w * h * 4, used: performance.now() });
      this.bytes += w * h * 4;
      this.#evict();
      this.onLoad(hash);
    } catch {
      this.failed.add(hash);
    } finally {
      this.loading.delete(key);
    }
  }

  #evict() {
    if (this.bytes <= this.budget) return;
    const list = [...this.entries.entries()].sort((a, b) => a[1].used - b[1].used);
    for (const [k, e] of list) {
      if (this.bytes <= this.budget * 0.8) break;
      this.entries.delete(k);
      this.bytes -= e.bytes;
      if (e.bmp.close) e.bmp.close();
    }
  }

  // full decodes an image at its natural size, for PNG export.
  full(hash) { return this.#decode(hash); }
}

async function scaled(img, w, h) {
  if (w === img.naturalWidth && h === img.naturalHeight) return createImageBitmap(img);
  try {
    return await createImageBitmap(img, { resizeWidth: w, resizeHeight: h, resizeQuality: 'high' });
  } catch {
    const c = document.createElement('canvas');
    c.width = w; c.height = h;
    c.getContext('2d').drawImage(img, 0, 0, w, h);
    return c;
  }
}

// ---- painting elements ----

function rounded(ctx, x, y, w, h, r) {
  ctx.beginPath();
  if (ctx.roundRect) ctx.roundRect(x, y, w, h, Math.min(r, w / 2, h / 2));
  else ctx.rect(x, y, w, h);
}

function dashOf(el, sw) {
  const d = style(el, 'dash');
  if (d === 'dashed') return [sw * 4, sw * 3];
  if (d === 'dotted') return [sw, sw * 2];
  return [];
}

// Painter draws elements in canvas coordinates. The renderer and the PNG
// export share it; opts.marks is false for export (no editing state).
export class Painter {
  constructor(opts) {
    this.theme = opts.theme;
    this.images = opts.images;
    this.get = opts.get;
    this.names = opts.names || null;
    this.showAuthors = !!opts.showAuthors;
    this.hidden = opts.hidden || null; // id whose text an editor overlay covers
    this.layouts = new Map();
    this.dpr = opts.dpr || 1;
  }

  layout(el, key, make) {
    const c = this.layouts.get(el.id);
    if (c && c.key === key) return c.value;
    const value = make();
    if (this.layouts.size > 20000) this.layouts.clear();
    this.layouts.set(el.id, { key, value });
    return value;
  }

  draw(ctx, el, z) {
    const lod = z <= 0.1 ? 2 : z < 0.25 ? 1 : 0;
    switch (el.type) {
      case 'connector': return this.connector(ctx, el, z, lod);
      case 'pen': return this.pen(ctx, el, z);
      default:
    }
    ctx.save();
    if (el.rot) {
      ctx.translate(el.x + el.w / 2, el.y + el.h / 2);
      ctx.rotate((el.rot * Math.PI) / 180);
      ctx.translate(-(el.x + el.w / 2), -(el.y + el.h / 2));
    }
    if (el.type === 'sticky') this.sticky(ctx, el, z, lod);
    else if (el.type === 'shape') this.shape(ctx, el, z, lod);
    else if (el.type === 'text') this.text(ctx, el, z, lod);
    else if (el.type === 'image') this.image(ctx, el, z);
    else if (el.type === 'frame') this.frame(ctx, el, z);
    ctx.restore();
  }

  lines(ctx, lay, x, y, maxW, align, color, lod) {
    const lh = lay.size * LINE;
    if (lod === 1) {
      ctx.fillStyle = this.theme.line;
      lay.lines.forEach((line, i) => {
        if (!line) return;
        const w = Math.min(maxW, line.length * lay.size * 0.5);
        const lx = align === 'center' ? x + (maxW - w) / 2 : align === 'right' ? x + maxW - w : x;
        ctx.fillRect(lx, y + i * lh + lh * 0.3, w, lh * 0.4);
      });
      return;
    }
    ctx.font = lay.font;
    ctx.fillStyle = color;
    ctx.textBaseline = 'top';
    ctx.textAlign = align;
    const ax = align === 'center' ? x + maxW / 2 : align === 'right' ? x + maxW : x;
    lay.lines.forEach((line, i) => ctx.fillText(line, ax, y + i * lh + lay.size * 0.15));
    ctx.textAlign = 'left';
    if (lay.overflow) {
      ctx.font = `600 ${lay.size}px ${FONT}`;
      ctx.textAlign = 'right';
      ctx.fillText('…', x + maxW, y + lay.lines.length * lh - lh * 0.2);
      ctx.textAlign = 'left';
    }
  }

  sticky(ctx, el, z, lod) {
    const t = this.theme;
    if (lod < 2) { ctx.fillStyle = t.shadow; rounded(ctx, el.x, el.y + 2, el.w, el.h, 4); ctx.fill(); }
    ctx.fillStyle = stickyColor(t, style(el, 'color'));
    rounded(ctx, el.x, el.y, el.w, el.h, 4);
    ctx.fill();
    if (lod === 2) return;
    const author = this.showAuthors ? authorName(el.by, this.names) : '';
    const pad = 12, foot = author ? 26 : 12;
    if (this.hidden !== el.id && el.text) {
      const fs = style(el, 'fontSize');
      const auto = fs === 'auto';
      const lay = this.layout(el, `${el.text}|${el.w}|${el.h}|${fs}|${foot}`, () =>
        fitText(ctx, el.text, el.w - pad * 2, el.h - pad - foot, auto ? 16 : fs, auto));
      this.lines(ctx, lay, el.x + pad, el.y + pad, el.w - pad * 2, 'left', t.ink, lod);
    }
    if (author && lod === 0 && z >= 0.45) {
      ctx.font = `11px ${FONT}`;
      ctx.fillStyle = t.by;
      ctx.textBaseline = 'alphabetic';
      ctx.fillText(author, el.x + pad, el.y + el.h - 10, el.w - pad * 2);
    }
  }

  shapePath(ctx, el) {
    const k = style(el, 'shape');
    const { x, y, w, h } = el;
    if (k === 'ellipse') { ctx.beginPath(); ctx.ellipse(x + w / 2, y + h / 2, w / 2, h / 2, 0, 0, Math.PI * 2); }
    else if (k === 'diamond') {
      ctx.beginPath();
      ctx.moveTo(x + w / 2, y); ctx.lineTo(x + w, y + h / 2); ctx.lineTo(x + w / 2, y + h); ctx.lineTo(x, y + h / 2);
      ctx.closePath();
    } else rounded(ctx, x, y, w, h, k === 'round' ? 14 : 2);
  }

  shape(ctx, el, z, lod) {
    const t = this.theme;
    const fill = shapeFill(t, style(el, 'fill'));
    const sw = style(el, 'strokeWidth');
    this.shapePath(ctx, el);
    if (fill) { ctx.fillStyle = fill; ctx.fill(); }
    ctx.lineWidth = Math.max(sw, 1 / z);
    ctx.strokeStyle = inkColor(t, style(el, 'stroke'));
    ctx.setLineDash(dashOf(el, sw));
    ctx.stroke();
    ctx.setLineDash([]);
    if (lod === 2 || !el.text || this.hidden === el.id) return;
    const inset = style(el, 'shape') === 'diamond' ? 0.25 : style(el, 'shape') === 'ellipse' ? 0.15 : 0;
    const pad = 8;
    const bw = el.w * (1 - inset * 2) - pad * 2, bh = el.h * (1 - inset * 2) - pad * 2;
    const fs = style(el, 'fontSize');
    const lay = this.layout(el, `${el.text}|${bw}|${bh}|${fs}`, () => fitText(ctx, el.text, bw, bh, fs, true));
    const th = lay.lines.length * lay.size * LINE;
    this.lines(ctx, lay, el.x + el.w * inset + pad, el.y + (el.h - th) / 2, bw, 'center', t.ink, lod);
  }

  text(ctx, el, z, lod) {
    if (this.hidden === el.id || !el.text) return;
    const fs = style(el, 'fontSize');
    const lay = this.layout(el, `${el.text}|${el.w}|${fs}`, () => fitText(ctx, el.text, el.w - 8, Infinity, fs, false));
    this.lines(ctx, lay, el.x + 4, el.y + 4, el.w - 8, style(el, 'align'), inkColor(this.theme, style(el, 'color')), lod);
  }

  image(ctx, el, z) {
    const bmp = this.images ? this.images.get(el.asset, el.natural, el.w * z * this.dpr) : null;
    if (bmp) { ctx.drawImage(bmp, el.x, el.y, el.w, el.h); return; }
    ctx.fillStyle = this.theme.line;
    ctx.fillRect(el.x, el.y, el.w, el.h);
  }

  frame(ctx, el, z) {
    const t = this.theme;
    const fill = frameFill(t, style(el, 'fill'));
    rounded(ctx, el.x, el.y, el.w, el.h, 8);
    if (fill) { ctx.fillStyle = fill; ctx.fill(); }
    ctx.lineWidth = Math.max(1.5, 1 / z);
    ctx.strokeStyle = t.frameLine;
    ctx.stroke();
    if (this.hidden === el.id) return;
    // The name keeps a readable screen size at every zoom level.
    const fs = 13 / z;
    ctx.font = `600 ${fs}px ${FONT}`;
    ctx.fillStyle = t.muted;
    ctx.textBaseline = 'alphabetic';
    ctx.fillText(el.text || 'Frame', el.x, el.y - 8 / z, Math.max(el.w, 40 / z));
  }

  connector(ctx, el, z, lod) {
    const pts = connectorPoints(el, this.get);
    if (!pts) return;
    const t = this.theme;
    const sw = style(el, 'strokeWidth');
    const color = inkColor(t, style(el, 'stroke'));
    ctx.lineWidth = Math.max(sw, 1 / z);
    ctx.strokeStyle = color;
    ctx.fillStyle = color;
    ctx.lineJoin = 'round';
    ctx.setLineDash(dashOf(el, sw));
    ctx.beginPath();
    ctx.moveTo(pts[0][0], pts[0][1]);
    for (let i = 1; i < pts.length; i++) ctx.lineTo(pts[i][0], pts[i][1]);
    ctx.stroke();
    ctx.setLineDash([]);
    const head = (tip, from) => {
      const a = Math.atan2(tip[1] - from[1], tip[0] - from[0]);
      const s = 8 + sw * 2;
      ctx.beginPath();
      ctx.moveTo(tip[0], tip[1]);
      ctx.lineTo(tip[0] - s * Math.cos(a - 0.45), tip[1] - s * Math.sin(a - 0.45));
      ctx.lineTo(tip[0] - s * Math.cos(a + 0.45), tip[1] - s * Math.sin(a + 0.45));
      ctx.closePath();
      ctx.fill();
    };
    if (style(el, 'head1') === 'arrow') head(pts[pts.length - 1], pts[pts.length - 2]);
    if (style(el, 'head0') === 'arrow') head(pts[0], pts[1]);
    if (!el.text || lod === 2 || this.hidden === el.id) return;
    const mid = pointAlong(pts, 0.5);
    const b = labelBox(el, mid);
    ctx.fillStyle = t.labelBg;
    rounded(ctx, b.x, b.y, b.w, b.h, 3);
    ctx.fill();
    const fs = style(el, 'fontSize');
    const lay = this.layout(el, `${el.text}|${fs}`, () => fitText(ctx, el.text, b.w, Infinity, fs, false));
    this.lines(ctx, lay, b.x, b.y + 3, b.w, 'center', t.ink, lod);
  }

  pen(ctx, el, z) {
    const pts = penWorldPoints(el);
    if (!pts.length) return;
    const marker = style(el, 'width') === 'marker';
    ctx.lineWidth = Math.max(penWidth(el), 1 / z);
    ctx.lineCap = 'round';
    ctx.lineJoin = 'round';
    ctx.strokeStyle = inkColor(this.theme, marker && style(el, 'stroke') === 'ink' ? 'highlight' : style(el, 'stroke'));
    strokeSmooth(ctx, pts);
  }
}

// strokeSmooth draws a polyline as quadratic curves through midpoints.
export function strokeSmooth(ctx, pts) {
  ctx.beginPath();
  ctx.moveTo(pts[0][0], pts[0][1]);
  if (pts.length < 3) {
    for (const p of pts.slice(1)) ctx.lineTo(p[0], p[1]);
  } else {
    for (let i = 1; i < pts.length - 1; i++) {
      const mx = (pts[i][0] + pts[i + 1][0]) / 2, my = (pts[i][1] + pts[i + 1][1]) / 2;
      ctx.quadraticCurveTo(pts[i][0], pts[i][1], mx, my);
    }
    const last = pts[pts.length - 1];
    ctx.lineTo(last[0], last[1]);
  }
  ctx.stroke();
}

// ---- renderer ----

export class Renderer {
  constructor(app, host) {
    this.app = app;
    this.host = host;
    this.stat = host.querySelector('.layer-static');
    this.dyn = host.querySelector('.layer-dynamic');
    this.sctx = this.stat.getContext('2d');
    this.dctx = this.dyn.getContext('2d');
    this.scratch = document.createElement('canvas');
    this.w = 0; this.h = 0; this.dpr = 1;
    this.full = true;
    this.world = [];       // dirty canvas-coordinate rects
    this.screen = [];      // dirty screen rects (exposed strips)
    this.drawn = null;     // camera of the static bitmap
    this.zoomBase = null;  // {cam} while a zoom gesture shows the scaled bitmap
    this.zoomTimer = 0;
    this.raf = 0;
    this.dynDirty = true;
    this.overlays = [];    // functions drawing on the dynamic layer
    const ipad = navigator.maxTouchPoints > 1 && /Macintosh|iPad/.test(navigator.userAgent);
    this.images = new ImageCache((hash) => app.assetLoaded(hash), (ipad ? 128 : 256) * 1024 * 1024);
    this.painter = null;
    this.#painter();
    new ResizeObserver(() => this.resize()).observe(host);
    matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => { this.#painter(); this.invalidateAll(); });
    this.resize();
  }

  #painter() {
    const old = this.painter;
    this.painter = new Painter({
      theme: currentTheme(), images: this.images, get: (id) => this.app.doc.get(id),
      names: this.app.names, showAuthors: !this.app.isPublic, dpr: this.dpr,
    });
    if (old) this.painter.hidden = old.hidden;
  }

  get theme() { return this.painter.theme; }

  resize() {
    const r = this.host.getBoundingClientRect();
    this.dpr = window.devicePixelRatio || 1;
    this.w = Math.max(1, Math.round(r.width));
    this.h = Math.max(1, Math.round(r.height));
    for (const c of [this.stat, this.dyn]) {
      c.width = Math.round(this.w * this.dpr);
      c.height = Math.round(this.h * this.dpr);
    }
    this.painter.dpr = this.dpr;
    this.app.viewportChanged();
    this.invalidateAll();
  }

  schedule() {
    if (!this.raf) this.raf = requestAnimationFrame(() => { this.raf = 0; this.#frame(); });
  }

  invalidate(r) {
    if (!r || this.full) return;
    this.world.push(r);
    this.schedule();
  }

  invalidateAll() { this.full = true; this.dynDirty = true; this.schedule(); }

  dynamic() { this.dynDirty = true; this.schedule(); }

  setHidden(id) { this.painter.hidden = id; this.invalidateAll(); }

  // zoomGesture shows the scaled bitmap until the gesture stops.
  zoomGesture() {
    if (!this.zoomBase && this.drawn && !this.full) {
      this.scratch.width = this.stat.width;
      this.scratch.height = this.stat.height;
      this.scratch.getContext('2d').drawImage(this.stat, 0, 0);
      this.zoomBase = { ...this.drawn };
    }
    clearTimeout(this.zoomTimer);
    this.zoomTimer = setTimeout(() => { this.zoomBase = null; this.invalidateAll(); }, 160);
  }

  viewRect() {
    const c = this.app.cam;
    return { x: c.x, y: c.y, w: this.w / c.z, h: this.h / c.z };
  }

  #frame() {
    const cam = this.app.cam;
    const prev = this.drawn;
    if (!this.full && prev && (prev.x !== cam.x || prev.y !== cam.y || prev.z !== cam.z)) {
      if (prev.z !== cam.z) {
        if (this.zoomBase) this.#drawScaled(cam);
        else this.full = true;
      } else this.#shift(prev, cam);
    }
    if (this.full) {
      this.full = false;
      this.world = [];
      this.screen = [];
      if (!this.zoomBase) this.#region(0, 0, this.w, this.h);
    } else if (!this.zoomBase || prev.z === cam.z) {
      const rects = this.screen;
      // Frame names keep a fixed screen size above the frame, so every
      // region reaches 26px higher than its canvas rect.
      for (const r of this.world) {
        rects.push({ x: (r.x - cam.x) * cam.z - 2, y: (r.y - cam.y) * cam.z - 28, w: r.w * cam.z + 4, h: r.h * cam.z + 30 });
      }
      this.world = [];
      this.screen = [];
      const clipped = rects.map((r) => clip(r, this.w, this.h)).filter(Boolean);
      if (clipped.length > 24) this.#region(0, 0, this.w, this.h);
      else for (const r of merge(clipped)) this.#region(r.x, r.y, r.w, r.h);
    }
    this.drawn = { ...cam };
    if (this.dynDirty) { this.dynDirty = false; this.#dynamicLayer(); }
  }

  #shift(prev, cam) {
    const dx = (prev.x - cam.x) * cam.z, dy = (prev.y - cam.y) * cam.z;
    const ddx = dx * this.dpr, ddy = dy * this.dpr;
    if (Math.abs(ddx - Math.round(ddx)) > 0.01 || Math.abs(ddy - Math.round(ddy)) > 0.01 || Math.abs(dx) >= this.w || Math.abs(dy) >= this.h) {
      this.full = true;
      return;
    }
    const s = this.scratch;
    s.width = this.stat.width; s.height = this.stat.height;
    s.getContext('2d').drawImage(this.stat, 0, 0);
    const ctx = this.sctx;
    ctx.save();
    ctx.setTransform(1, 0, 0, 1, 0, 0);
    ctx.clearRect(0, 0, this.stat.width, this.stat.height);
    ctx.drawImage(s, Math.round(ddx), Math.round(ddy));
    ctx.restore();
    if (dx > 0) this.screen.push({ x: 0, y: 0, w: dx, h: this.h });
    if (dx < 0) this.screen.push({ x: this.w + dx, y: 0, w: -dx, h: this.h });
    if (dy > 0) this.screen.push({ x: 0, y: 0, w: this.w, h: dy });
    if (dy < 0) this.screen.push({ x: 0, y: this.h + dy, w: this.w, h: -dy });
  }

  #drawScaled(cam) {
    const b = this.zoomBase;
    const s = cam.z / b.z;
    const ox = (b.x - cam.x) * cam.z, oy = (b.y - cam.y) * cam.z;
    const ctx = this.sctx;
    ctx.save();
    ctx.setTransform(1, 0, 0, 1, 0, 0);
    ctx.fillStyle = this.theme.canvas;
    ctx.fillRect(0, 0, this.stat.width, this.stat.height);
    ctx.drawImage(this.scratch, ox * this.dpr, oy * this.dpr, this.scratch.width * s, this.scratch.height * s);
    ctx.restore();
  }

  #region(sx, sy, sw, sh) {
    const ctx = this.sctx, cam = this.app.cam, d = this.dpr, t = this.theme;
    ctx.save();
    ctx.setTransform(d, 0, 0, d, 0, 0);
    ctx.beginPath();
    ctx.rect(sx, sy, sw, sh);
    ctx.clip();
    ctx.fillStyle = t.canvas;
    ctx.fillRect(sx, sy, sw, sh);
    if (this.app.doc.meta().grid) this.#dots(ctx, sx, sy, sw, sh);
    const wr = { x: cam.x + sx / cam.z, y: cam.y + sy / cam.z, w: sw / cam.z, h: sh / cam.z + 40 / cam.z };
    const doc = this.app.doc;
    const els = [];
    for (const id of doc.index.query(wr)) { const el = doc.get(id); if (el) els.push(el); }
    els.sort(zCompare);
    ctx.setTransform(d * cam.z, 0, 0, d * cam.z, -cam.x * cam.z * d, -cam.y * cam.z * d);
    for (const el of els) this.painter.draw(ctx, el, cam.z);
    ctx.restore();
  }

  #dots(ctx, sx, sy, sw, sh) {
    const cam = this.app.cam;
    let step = 24;
    while (step * cam.z < 12) step *= 2;
    const x0 = Math.floor((cam.x + sx / cam.z) / step) * step;
    const y0 = Math.floor((cam.y + sy / cam.z) / step) * step;
    ctx.fillStyle = this.theme.dot;
    for (let wx = x0; (wx - cam.x) * cam.z <= sx + sw; wx += step) {
      const px = (wx - cam.x) * cam.z;
      for (let wy = y0; (wy - cam.y) * cam.z <= sy + sh; wy += step) ctx.fillRect(px - 1, (wy - cam.y) * cam.z - 1, 2.2, 2.2);
    }
  }

  // ---- dynamic layer ----

  toScreen(x, y) { const c = this.app.cam; return [(x - c.x) * c.z, (y - c.y) * c.z]; }

  #dynamicLayer() {
    const ctx = this.dctx;
    ctx.setTransform(1, 0, 0, 1, 0, 0);
    ctx.clearRect(0, 0, this.dyn.width, this.dyn.height);
    ctx.setTransform(this.dpr, 0, 0, this.dpr, 0, 0);
    for (const fn of this.overlays) {
      ctx.save();
      fn(ctx, this);
      ctx.restore();
    }
  }

  // ---- drawing helpers for overlays (screen coordinates) ----

  outline(ctx, el, pad = 0) {
    if (el.type === 'connector' || el.type === 'pen') {
      const r = this.app.doc.index.bounds(el.id);
      if (!r) return;
      const [x, y] = this.toScreen(r.x, r.y);
      ctx.beginPath();
      ctx.rect(x - pad, y - pad, r.w * this.app.cam.z + pad * 2, r.h * this.app.cam.z + pad * 2);
      return;
    }
    const pts = boxCorners(el).map(([x, y]) => this.toScreen(x, y));
    const [cx, cy] = this.toScreen(el.x + el.w / 2, el.y + el.h / 2);
    ctx.beginPath();
    pts.forEach(([x, y], i) => {
      const dx = x - cx, dy = y - cy, l = Math.hypot(dx, dy) || 1;
      const px = x + (dx / l) * pad * 1.414, py = y + (dy / l) * pad * 1.414;
      if (i) ctx.lineTo(px, py); else ctx.moveTo(px, py);
    });
    ctx.closePath();
  }

  tag(ctx, text, x, y, color) {
    ctx.font = `600 11px ${FONT}`;
    const w = ctx.measureText(text).width + 12;
    ctx.fillStyle = color;
    rounded(ctx, x, y - 18, w, 18, 4);
    ctx.fill();
    ctx.fillStyle = '#ffffff';
    ctx.textBaseline = 'middle';
    ctx.fillText(text, x + 6, y - 9);
  }
}

function clip(r, w, h) {
  const x0 = Math.max(0, Math.floor(r.x)), y0 = Math.max(0, Math.floor(r.y));
  const x1 = Math.min(w, Math.ceil(r.x + r.w)), y1 = Math.min(h, Math.ceil(r.y + r.h));
  return x1 > x0 && y1 > y0 ? { x: x0, y: y0, w: x1 - x0, h: y1 - y0 } : null;
}

// merge joins overlapping rects so a region is never painted twice.
function merge(rects) {
  const out = [];
  for (let r of rects) {
    for (let i = 0; i < out.length; i++) {
      const o = out[i];
      if (r.x <= o.x + o.w && o.x <= r.x + r.w && r.y <= o.y + o.h && o.y <= r.y + r.h) {
        const x = Math.min(r.x, o.x), y = Math.min(r.y, o.y);
        r = { x, y, w: Math.max(r.x + r.w, o.x + o.w) - x, h: Math.max(r.y + r.h, o.y + o.h) - y };
        out.splice(i, 1);
        i = -1;
      }
    }
    out.push(r);
  }
  return out;
}
