// Flats board canvas app. Plain ES modules, no build step, no third-party
// code. It renders the board on Canvas 2D and edits it through the live
// WebSocket protocol of board-TECH.md.

import { BoardState, activityCounts } from './state.js';
import { Net, queueBatch } from './net.js';
import { SpatialIndex } from './index.js';
import { Renderer } from './render.js';
import { elementBounds, hit, union, center, connectorPoints } from './geom.js';
import { zCompare } from './ids.js';
import { LIMITS } from './schema.js';
import * as ops from './ops.js';
import { $, h, icon, toast, dialog, plural, closePopover } from './ui.js';
import { Presence } from './presence.js';
import { StyleBar, contextMenu } from './stylebar.js';
import { TextEditor } from './textedit.js';
import { ZoomBar, FrameList, LockedList, FindBar, ShareBox } from './panels.js';
import { History } from './history.js';
import { Exporter } from './export.js';
import { A11yTree } from './a11y.js';
import { Undo } from './undo.js';
import { SelectTool } from './tools/select.js';
import { HandTool } from './tools/hand.js';
import { StickyTool } from './tools/sticky.js';
import { TextTool } from './tools/text.js';
import { ShapeTool } from './tools/shape.js';
import { FrameTool } from './tools/frame.js';
import { ConnectorTool } from './tools/connector.js';
import { PenTool } from './tools/pen.js';
import { EraserTool } from './tools/eraser.js';
import { ImageTool, uploadImages } from './tools/image.js';

const MIN_Z = 0.1, MAX_Z = 4;
const TOO_LARGE = 'This change is too large to send at once. Try a smaller selection.';

const TOOLBAR = [
  ['select', 'Select', 'V', 'select'], ['hand', 'Hand', 'H', 'hand'], '-',
  ['sticky', 'Sticky note', 'S', 'sticky'], ['text', 'Text', 'T', 'text'], ['shape', 'Shape', 'R', 'rect'],
  ['connector', 'Connector', 'L', 'connector'], ['pen', 'Pen', 'P', 'pen'], ['eraser', 'Eraser', 'E', 'eraser'], '-',
  ['image', 'Image', 'I', 'image'], ['frame', 'Frame', 'F', 'frame'],
];

// editorId identifies this browser to the server (hello.editor). Storage can
// be unavailable in private windows; then each tab is its own editor.
function editorId() {
  const fresh = [...crypto.getRandomValues(new Uint8Array(12))].map((b) => b.toString(16).padStart(2, '0')).join('');
  try {
    const id = localStorage.getItem('flats-board-editor');
    if (id) return id;
    localStorage.setItem('flats-board-editor', fresh);
  } catch { /* storage blocked */ }
  return fresh;
}

function storedName() {
  try { return localStorage.getItem('flats-board-name') || ''; } catch { return ''; }
}

class App {
  constructor() {
    this.state = new BoardState();
    this.index = new SpatialIndex();
    this.cam = { x: -80, y: -80, z: 1 };
    this.sel = new Set();
    this.names = new Map();
    this.me = null;
    this.access = null;
    this.welcomed = false;
    this.readOnlyChoice = false; // the person chose "Read only" in the name prompt
    this.stopped = null;         // fatal code
    this.quota = false;
    this.uploads = new Map();    // hash -> bytes of images this tab uploaded, until acked
    this.highlights = new Map(); // id -> expiry time (agent writes)
    this.focusId = null;         // keyboard focus on the canvas
    this.pointer = null;         // last pointer position in canvas coordinates
    this.editor = null;
    this.firstFit = false;
    this.fitPending = false;
    this.gridOverride = null;    // a viewer's own grid toggle when they cannot edit
    this.clip = null;            // last board clipboard payload of this tab
    this.space = false;
    this.bannerKind = null;
    this.live = {
      get: (id) => this.state.get(id), all: () => this.state.all(), index: this.index,
      meta: () => (this.gridOverride == null ? this.state.viewMeta() : { ...this.state.viewMeta(), grid: this.gridOverride }), live: true,
    };
    this.doc = this.live;
    this.editorKey = editorId();
    this.name = storedName();
    this.dom();
    this.renderer = new Renderer(this, $('.board'));
    this.presence = new Presence(this);
    this.styleBar = new StyleBar(this);
    this.zoomBar = new ZoomBar(this);
    this.frames = new FrameList(this);
    this.locked = new LockedList(this);
    this.find = new FindBar(this);
    this.share = new ShareBox(this);
    this.history = new History(this);
    this.exporter = new Exporter(this);
    this.a11y = new A11yTree(this);
    this.undo = new Undo(this);
    this.tools = {
      select: new SelectTool(this), hand: new HandTool(this), sticky: new StickyTool(this), text: new TextTool(this),
      shape: new ShapeTool(this), frame: new FrameTool(this), connector: new ConnectorTool(this),
      pen: new PenTool(this), eraser: new EraserTool(this), image: new ImageTool(this),
    };
    this.tool = this.tools.select;
    this.renderer.overlays.push((ctx, r) => this.drawMarks(ctx, r), (ctx, r) => this.presence.draw(ctx, r), (ctx, r) => this.tool.draw && this.tool.draw(ctx, r));
    this.net = new Net(this.state, this.handlers());
    this.input();
    this.status();
    this.net.start();
    addEventListener('beforeunload', (e) => {
      if (!this.state.pending.length) return;
      e.preventDefault();
      e.returnValue = '';
      setTimeout(() => this.banner('unload'), 0);
    });
  }

  get isPublic() { return this.access !== 'private'; }
  get canEdit() { return this.welcomed && !this.isPublic && !this.stopped && !this.readOnlyChoice && this.doc.live && !!this.me && (this.me.verified || !!this.name); }

  // ---- layout ----

  dom() {
    for (const s of document.querySelectorAll('[data-icon]')) s.replaceWith(icon(s.dataset.icon));
    $('#find-btn').addEventListener('click', () => this.find.open());
    $('.mobile-nav [data-act=frames]').addEventListener('click', () => this.frames.toggle());
    $('.mobile-nav [data-act=find]').addEventListener('click', () => this.find.open());
    $('.mobile-nav [data-act=fit]').addEventListener('click', () => this.fitAll());
    this.titleEl = $('#board-title');
    this.statusEl = $('.save');
    this.toolsEl = $('.tools');
    for (const t of TOOLBAR) {
      if (t === '-') { this.toolsEl.appendChild(h('span', { class: 'sep', 'aria-hidden': 'true' })); continue; }
      const [name, label, key, ic] = t;
      this.toolsEl.appendChild(h('button', {
        type: 'button', class: 't', 'data-tool': name, 'aria-label': `${label} (${key})`, title: `${label} (${key})`,
        'aria-pressed': name === 'select' ? 'true' : 'false', onclick: () => this.setTool(name),
      }, icon(ic)));
    }
    $('.undo [data-act=undo]').addEventListener('click', () => this.undo.undo());
    $('.undo [data-act=redo]').addEventListener('click', () => this.undo.redo());
    $('#frames-btn').addEventListener('click', () => this.frames.toggle());
    $('#name-btn').addEventListener('click', () => this.namePrompt());
  }

  // ---- connection handlers ----

  handlers() {
    return {
      hello: () => {
        const st = this.state;
        const m = { t: 'hello', v: 1, pending: { count: st.pending.length }, editor: this.editorKey };
        if (st.session) { m.session = st.session; m.watermark = st.watermark; }
        if (st.pending.length) m.pending.first_n = st.pending[0].n;
        if (this.name) m.name = this.name;
        return m;
      },
      welcome: (m) => this.welcome(m),
      snapshot: (changed, snap) => {
        this.rebuild();
        if (snap.reset && this.firstFit) toast('The board was rolled back or replaced while you were away. Showing the current board.');
        if (!this.firstFit) { this.firstFit = true; this.fitPending = true; this.viewportChanged(); }
      },
      delta: (r, d) => this.onDelta(r, d),
      ack: (m, batch, changed) => { this.changed(changed); this.onAck(m, batch); },
      reject: (m, batch, changed) => { this.changed(changed); this.onReject(m, batch); },
      fatal: (code) => this.onFatal(code),
      pres: (m) => this.presence.receive(m),
      shared: (m) => this.share.shared(m),
      status: () => this.status(),
      tooLarge: (b, changed) => {
        this.changed(changed);
        toast(TOO_LARGE, { error: true });
      },
    };
  }

  welcome(m) {
    this.access = m.access === 'private' ? 'private' : 'public';
    this.me = m.me || null;
    this.welcomed = true;
    if (this.me && this.me.key) this.names.set(this.me.key, this.me.name);
    document.body.classList.toggle('public', this.isPublic);
    const chip = $('#access-chip');
    chip.hidden = false;
    chip.textContent = this.isPublic ? 'Public · read only' : 'Private';
    chip.classList.toggle('priv', !this.isPublic);
    if (this.isPublic) {
      $('#export-btn .lbl').textContent = 'Download PNG';
      $('#export-btn').setAttribute('aria-label', 'Download PNG');
      $('#export-btn').title = 'Download PNG';
    }
    this.presence.avatars();
    this.renderer.painter.showAuthors = !this.isPublic;
    this.renderer.invalidateAll();
    if (!this.isPublic && this.me && !this.me.verified && !this.name && !this.readOnlyChoice) this.namePrompt();
    this.applyMode();
    this.presence.sendAll();
  }

  applyMode() {
    document.body.classList.toggle('readonly', !this.canEdit);
    $('#name-btn').hidden = !(this.welcomed && !this.isPublic && this.readOnlyChoice);
    if (!this.canEdit && this.tool !== this.tools.select && this.tool !== this.tools.hand) this.setTool('select');
    this.styleBar.update();
    this.share.update();
    this.renderer.dynamic();
    this.status();
  }

  onDelta(r, d) {
    this.changed(r.changed);
    for (const o of r.overwritten) this.overwritten(o, d.by);
    if (r.reset) this.undo.cleared(r.reset, d.by);
    if (d.by && d.by.kind === 'agent') this.agentActivity(d, r);
    if (this.editor) this.editor.remote();
    this.history.deltaArrived();
  }

  onAck(m, batch) {
    if (batch) this.releaseUploads(batch);
    this.undo.ack(m, batch);
    if (batch && !batch.undo) {
      const gone = countVoid(m.results, 'gone');
      if (gone) toast(`Edits to ${plural(gone, 'element')} someone else deleted were not applied.`);
      const group = countVoid(m.results, 'invalid_group');
      if (group) toast('Grouping did not apply: an element changed frame or group meanwhile. Group them again.');
    }
    if (this.quota) {
      this.quota = false;
      if (this.bannerKind === 'quota') this.banner(null);
    }
  }

  onReject(m, batch) {
    if (m.retry) return;
    if (batch) this.releaseUploads(batch);
    if (m.code === 'disk_quota') { this.quota = true; this.banner('quota'); return; }
    const reasons = {
      limit: 'This edit would go over a board limit, so it was not saved.',
      format: 'This edit could not be saved because it was malformed.',
      permission: 'This connection can no longer edit the board.',
    };
    toast(m.detail || reasons[m.code] || `The edit was refused (${m.code}).`, { error: true });
  }

  onFatal(code) {
    this.stopped = code;
    this.applyMode();
    const msgs = {
      session_expired: 'Editing cannot continue. More than 24 hours passed since the connection dropped, so your unsent edits cannot be applied to the board.',
      receipt_expired: 'Editing cannot continue. The board no longer remembers part of your unsent edits, so they cannot be applied safely.',
      deleted: 'This board was deleted.',
      renamed: 'This board was renamed. Open it at its new address.',
      type_changed: 'This board is no longer a board.',
      protocol: 'Editing stopped after a protocol error.',
    };
    this.banner('fatal', msgs[code] || 'Editing stopped.');
  }

  releaseUploads(batch) {
    for (const o of batch.ops) if (o.op === 'add' && o.el.type === 'image') this.uploads.delete(o.el.asset);
  }

  // ---- state changes ----

  rebuild() {
    this.index.clear();
    const get = (id) => this.state.get(id);
    for (const el of this.state.all()) this.indexElement(el, get);
    this.pruneSelection();
    this.renderer.invalidateAll();
    this.afterChange(null);
  }

  indexElement(el, get) {
    this.index.set(el.id, elementBounds(el, get));
    if (el.type === 'connector') this.index.setConnector(el.id, el.ends.filter((e) => e.el).map((e) => e.el));
  }

  // changed updates the index and redraws what a set of ids touched,
  // including connectors attached to them.
  changed(ids) {
    if (!ids || !ids.size || !this.doc.live) return;
    const get = (id) => this.state.get(id);
    const conns = new Set();
    const r = this.renderer;
    const touch = (id) => {
      const old = this.index.bounds(id);
      if (old) r.invalidate(old);
      const el = this.state.get(id);
      if (!el) { this.index.set(id, null); this.index.setConnector(id, null); return; }
      this.indexElement(el, get);
      r.invalidate(this.index.bounds(id));
    };
    for (const id of ids) {
      touch(id);
      for (const c of this.index.connectorsOf(id)) if (!ids.has(c)) conns.add(c);
    }
    for (const c of conns) touch(c);
    this.pruneSelection();
    this.afterChange(ids);
  }

  afterChange(ids) {
    this.renderer.dynamic();
    this.styleBar.update();
    this.a11y.dirty();
    this.find.refresh();
    this.frames.refresh();
    this.locked.refresh();
    if (!ids || ids.has('_board')) this.title();
    this.status();
  }

  title() {
    const t = this.doc.meta().title || document.title || 'Board';
    this.titleEl.textContent = t;
  }

  pruneSelection() {
    let pruned = false;
    for (const id of this.sel) if (!this.doc.get(id)) { this.sel.delete(id); pruned = true; }
    if (pruned) this.selectionChanged();
  }

  // commit sends one person action as one batch (TECH 4.5).
  commit(list) {
    if (!list.length || !this.canEdit) return null;
    if (list.length > LIMITS.batchOps) {
      toast(`One action can change at most ${LIMITS.batchOps} elements. Try a smaller selection.`, { error: true });
      return null;
    }
    // queueBatch refuses an unsendable batch before it takes a number
    // (TECH 4.8, n == max_n + 1) and serializes the batch only once.
    const queued = queueBatch(this.state, list);
    if (!queued) {
      toast(TOO_LARGE, { error: true });
      return null;
    }
    const { batch, changed } = queued;
    this.changed(changed);
    this.net.flush();
    this.status();
    return batch;
  }

  setPreview(map) {
    const changed = this.state.setPreview(map);
    this.changed(changed);
  }

  // focusBoard moves keyboard focus back to the board. The canvases are
  // aria-hidden, so focus goes to the labelled board region around them.
  focusBoard() { $('.board').focus({ preventScroll: true }); }

  // ---- selection ----

  select(ids) {
    this.sel.clear();
    for (const id of ids) if (this.doc.get(id)) this.sel.add(id);
    this.selectionChanged();
  }

  selectionChanged() {
    this.renderer.dynamic();
    this.styleBar.update();
    this.presence.send({ sel: [...this.sel].slice(0, 200) });
    this.a11y.selection();
    this.share.update();
    this.history.selectionChanged();
  }

  selected() { return [...this.sel].map((id) => this.doc.get(id)).filter(Boolean); }

  // expand adds the other members of groups the ids belong to.
  expandGroups(ids) {
    const groups = new Set(ids.map((id) => this.doc.get(id)).filter((el) => el && el.group).map((el) => el.group));
    if (!groups.size) return ids;
    const out = new Set(ids);
    for (const el of this.doc.all()) if (el.group && groups.has(el.group) && !el.locked) out.add(el.id);
    return [...out];
  }

  // ---- hit testing and coordinates ----

  toWorld(sx, sy) { return [this.cam.x + sx / this.cam.z, this.cam.y + sy / this.cam.z]; }
  toScreen(x, y) { return [(x - this.cam.x) * this.cam.z, (y - this.cam.y) * this.cam.z]; }

  local(e) {
    const r = this.renderer.dyn.getBoundingClientRect();
    return [e.clientX - r.left, e.clientY - r.top];
  }

  // hitAt returns the topmost element at a canvas point.
  hitAt(x, y, { locked = false, filter } = {}) {
    const tol = 8 / this.cam.z;
    const get = (id) => this.doc.get(id);
    const cands = [];
    for (const id of this.doc.index.query({ x: x - tol, y: y - tol, w: tol * 2, h: tol * 2 })) {
      const el = get(id);
      if (!el || (!locked && el.locked) || (filter && !filter(el))) continue;
      if (hit(el, x, y, this.cam.z, get)) cands.push(el);
    }
    cands.sort(zCompare);
    return cands.length ? cands[cands.length - 1] : null;
  }

  // ---- camera ----

  setCam(x, y, z, { gesture = false } = {}) {
    z = Math.max(MIN_Z, Math.min(MAX_Z, z));
    if (z !== this.cam.z && gesture) this.renderer.zoomGesture();
    this.cam = { x, y, z };
    this.renderer.schedule();
    this.renderer.dynamic();
    this.viewportChanged();
  }

  zoomAt(sx, sy, z, opts) {
    z = Math.max(MIN_Z, Math.min(MAX_Z, z));
    const [wx, wy] = this.toWorld(sx, sy);
    this.setCam(wx - sx / z, wy - sy / z, z, opts);
  }

  zoomBy(f) { this.zoomAt(this.renderer.w / 2, this.renderer.h / 2, this.cam.z * f); }

  panBy(dx, dy) { this.setCam(this.cam.x - dx / this.cam.z, this.cam.y - dy / this.cam.z, this.cam.z); }

  fitRect(r, { max = 1, pad = 48 } = {}) {
    if (!r) return;
    const w = this.renderer.w, hh = this.renderer.h;
    const z = Math.max(MIN_Z, Math.min(max, (w - pad * 2) / Math.max(r.w, 1), (hh - pad * 2) / Math.max(r.h, 1)));
    const [cx, cy] = center(r);
    this.setCam(cx - w / 2 / z, cy - hh / 2 / z, z);
  }

  contentBounds(ids) {
    let r = null;
    const get = (id) => this.doc.get(id);
    for (const el of ids ? ids.map(get).filter(Boolean) : this.doc.all()) r = union(r, elementBounds(el, get));
    return r;
  }

  fitAll(initial) {
    const r = this.contentBounds();
    if (r) this.fitRect(r);
    else if (!initial) this.setCam(-this.renderer.w / 2, -this.renderer.h / 2, 1);
  }

  fitSelection() { if (this.sel.size) this.fitRect(this.contentBounds([...this.sel]), { max: 2 }); }

  // reveal moves the view so the element is visible, without zooming.
  reveal(id) {
    const b = this.doc.index.bounds(id);
    if (!b) return;
    const v = this.renderer.viewRect();
    if (b.x >= v.x && b.y >= v.y && b.x + b.w <= v.x + v.w && b.y + b.h <= v.y + v.h) return;
    if (b.w > v.w * 0.9 || b.h > v.h * 0.9) { this.fitRect(b, { max: this.cam.z }); return; }
    const [cx, cy] = center(b);
    this.setCam(cx - v.w / 2, cy - v.h / 2, this.cam.z);
  }

  viewportChanged() {
    if (!this.renderer) return;
    // The first fit waits until the canvas has a real size.
    if (this.fitPending && this.renderer.w > 50 && this.renderer.h > 50) { this.fitPending = false; this.fitAll(true); return; }
    if (this.editor) this.editor.place();
    this.styleBar.place();
    this.zoomBar.update();
    const v = this.renderer.viewRect();
    this.presence.send({ vp: [v.x, v.y, v.w, v.h, this.cam.z] });
  }

  assetLoaded(hash) {
    for (const el of this.doc.all()) if (el.type === 'image' && el.asset === hash) this.renderer.invalidate(this.doc.index.bounds(el.id));
  }

  // ---- tools ----

  setTool(name, opts) {
    if (!this.canEdit && name !== 'select' && name !== 'hand') return;
    if (this.tool.cancel) this.tool.cancel();
    this.tool = this.tools[name];
    if (this.tool.activate) this.tool.activate(opts);
    for (const b of this.toolsEl.querySelectorAll('[data-tool]')) b.setAttribute('aria-pressed', String(b.dataset.tool === name));
    this.renderer.dyn.dataset.tool = name;
    this.styleBar.update();
    this.renderer.dynamic();
  }

  editText(id, opts = {}) {
    if (!this.canEdit) return;
    const el = this.state.get(id);
    if (!el || el.locked || el.type === 'pen' || el.type === 'image') return;
    if (this.editor) this.editor.commit();
    this.editor = new TextEditor(this, el, opts);
  }

  // ---- input ----

  input() {
    const c = $('.layer-dynamic');
    const pointers = new Map();
    let pan = null, pinch = null, active = null;
    const toolFor = (e) => {
      if (e.pointerType === 'pen' && (this.tool === this.tools.select || this.tool === this.tools.hand) && this.canEdit) return this.tools.pen;
      return this.tool;
    };
    c.addEventListener('pointerdown', (e) => {
      closePopover();
      c.setPointerCapture(e.pointerId);
      const s = this.local(e);
      pointers.set(e.pointerId, s);
      if (pointers.size === 2) {
        if (active && active.cancel) active.cancel();
        active = null;
        const [a, b] = [...pointers.values()];
        pinch = { d: Math.hypot(a[0] - b[0], a[1] - b[1]), m: [(a[0] + b[0]) / 2, (a[1] + b[1]) / 2], z: this.cam.z };
        pan = null;
        return;
      }
      if (this.editor && !this.editor.contains(e.target)) this.editor.commit();
      // A finger pans; a short tap acts like a click and a long press opens
      // the context menu (rule 3, rule 20).
      if (e.pointerType === 'touch' && this.tool !== this.tools.pen) {
        pan = { s, cam: { ...this.cam }, touch: true };
        pan.timer = setTimeout(() => {
          if (!pan || !pan.touch || pan.moved) return;
          pan = null;
          this.openMenu(this.toWorld(...s), e.clientX, e.clientY);
        }, 550);
        return;
      }
      const handPan = e.button === 1 || this.space || this.tool === this.tools.hand;
      if (handPan) { pan = { s, cam: { ...this.cam } }; c.classList.add('panning'); return; }
      if (e.button === 2) return;
      active = toolFor(e);
      active.down(e, this.toWorld(...s), s);
    });
    c.addEventListener('pointermove', (e) => {
      const s = this.local(e);
      const w = this.toWorld(...s);
      this.pointer = w;
      if (pointers.has(e.pointerId)) pointers.set(e.pointerId, s);
      if (!this.isPublic && this.canEdit) this.presence.send({ c: [Math.round(w[0]), Math.round(w[1])] });
      if (pinch && pointers.size === 2) {
        const [a, b] = [...pointers.values()];
        const d = Math.hypot(a[0] - b[0], a[1] - b[1]);
        const m = [(a[0] + b[0]) / 2, (a[1] + b[1]) / 2];
        const z = Math.max(MIN_Z, Math.min(MAX_Z, pinch.z * (d / pinch.d)));
        const [wx, wy] = this.toWorld(...pinch.m);
        this.setCam(wx - m[0] / z, wy - m[1] / z, z, { gesture: true });
        pinch.m = m;
        pinch.z = z; pinch.d = d;
        return;
      }
      if (pan) {
        if (pan.touch && !pan.moved && Math.hypot(s[0] - pan.s[0], s[1] - pan.s[1]) > 8) { pan.moved = true; clearTimeout(pan.timer); }
        this.setCam(pan.cam.x - (s[0] - pan.s[0]) / this.cam.z, pan.cam.y - (s[1] - pan.s[1]) / this.cam.z, this.cam.z);
        return;
      }
      if (active) active.move(e, w, s);
      else if (this.tool.hover) this.tool.hover(e, w, s);
    });
    const end = (e) => {
      pointers.delete(e.pointerId);
      if (pinch) { if (pointers.size < 2) pinch = null; return; }
      if (pan) {
        const tap = pan.touch && !pan.moved && e.type === 'pointerup';
        clearTimeout(pan.timer);
        pan = null;
        c.classList.remove('panning');
        if (tap && this.tool !== this.tools.hand) {
          const s = this.local(e), w = this.toWorld(...s);
          this.tool.down(e, w, s);
          this.tool.up(e, w, s);
        }
        return;
      }
      if (active) {
        const t = active;
        active = null;
        if (e.type === 'pointercancel') { if (t.cancel) t.cancel(); } else t.up(e, this.toWorld(...this.local(e)), this.local(e));
      }
    };
    c.addEventListener('pointerup', end);
    c.addEventListener('pointercancel', end);
    c.addEventListener('dblclick', (e) => { if (this.tool.dblclick) this.tool.dblclick(e, this.toWorld(...this.local(e))); });
    c.addEventListener('contextmenu', (e) => {
      e.preventDefault();
      if (e.pointerType !== 'touch') this.openMenu(this.toWorld(...this.local(e)), e.clientX, e.clientY);
    });
    c.addEventListener('pointerleave', () => this.presence.send({ c: null }));
    $('.board').addEventListener('wheel', (e) => {
      e.preventDefault();
      if (e.ctrlKey || e.metaKey) {
        const s = this.local(e);
        this.zoomAt(s[0], s[1], this.cam.z * Math.exp(-e.deltaY * (e.deltaMode ? 0.05 : 0.01)), { gesture: true });
      } else {
        const k = e.deltaMode ? 16 : 1;
        this.panBy(-(e.shiftKey && !e.deltaX ? e.deltaY : e.deltaX) * k, -(e.shiftKey && !e.deltaX ? 0 : e.deltaY) * k);
      }
    }, { passive: false });
    addEventListener('keydown', (e) => this.key(e));
    addEventListener('keyup', (e) => { if (e.key === ' ') { this.space = false; c.classList.remove('space'); } });
    document.addEventListener('copy', (e) => this.copy(e, false));
    document.addEventListener('cut', (e) => this.copy(e, true));
    document.addEventListener('paste', (e) => this.paste(e));
    const board = $('.board');
    board.addEventListener('dragover', (e) => { if (this.canEdit) e.preventDefault(); });
    board.addEventListener('drop', (e) => {
      if (!this.canEdit) return;
      e.preventDefault();
      const files = [...e.dataTransfer.files].filter((f) => f.type.startsWith('image/') || /\.(png|jpe?g|gif|webp|svg)$/i.test(f.name));
      if (files.length) uploadImages(this, files, this.toWorld(...this.local(e)));
    });
  }

  openMenu(w, x, y) {
    if (this.isPublic) return;
    const el = this.hitAt(w[0], w[1], { locked: true });
    if (el && !el.locked && !this.sel.has(el.id)) this.select(this.expandGroups([el.id]));
    if (!el) this.select([]);
    contextMenu(this, x, y, el);
  }

  typing(e) {
    const t = e.target;
    return t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName));
  }

  key(e) {
    const mod = e.metaKey || e.ctrlKey;
    const k = e.key;
    if (mod && k.toLowerCase() === 'f') { e.preventDefault(); this.find.open(); return; }
    if (this.typing(e) || document.querySelector('dialog[open]')) return;
    if (k === ' ' && !mod) { this.space = true; $('.layer-dynamic').classList.add('space'); e.preventDefault(); return; }
    if (mod && (k === '=' || k === '+')) { e.preventDefault(); this.zoomBy(1.25); return; }
    if (mod && k === '-') { e.preventDefault(); this.zoomBy(0.8); return; }
    if (mod && k === '0') { e.preventDefault(); this.zoomAt(this.renderer.w / 2, this.renderer.h / 2, 1); return; }
    if (e.shiftKey && (e.code === 'Digit1' || k === '!')) { e.preventDefault(); this.fitAll(); return; }
    if (e.shiftKey && (e.code === 'Digit2' || k === '@')) { e.preventDefault(); this.fitSelection(); return; }
    if (k === 'Escape') {
      if (this.tool !== this.tools.select) this.setTool('select');
      else if (this.sel.size) this.select([]);
      return;
    }
    if (!this.canEdit) return;
    const ids = [...this.sel];
    const lower = k.toLowerCase();
    if (mod && lower === 'z') { e.preventDefault(); if (e.shiftKey) this.undo.redo(); else this.undo.undo(); return; }
    if (mod && lower === 'y') { e.preventDefault(); this.undo.redo(); return; }
    if (mod && lower === 'a') { e.preventDefault(); this.select([...this.doc.all()].filter((el) => !el.locked).map((el) => el.id)); return; }
    if (mod && lower === 'd') { e.preventDefault(); this.duplicate(); return; }
    if (mod && lower === 'g') { e.preventDefault(); this.commit(e.shiftKey ? ops.ungroupOps(this.state, ids) : ops.groupOps(this.state, ids)); return; }
    if (mod && e.shiftKey && lower === 'l') { e.preventDefault(); this.toggleLock(this.selected()); return; }
    if (mod && (k === ']' || k === '[')) {
      e.preventDefault();
      const dir = k === ']' ? (e.altKey ? 'forward' : 'front') : (e.altKey ? 'backward' : 'back');
      this.commit(ops.zorderOps(this.state, ids, dir));
      return;
    }
    if ((k === 'Delete' || k === 'Backspace') && ids.length) { e.preventDefault(); this.commit(ops.deleteOps(this.state, ids)); return; }
    if (k === 'Enter' && ids.length === 1) { e.preventDefault(); this.editText(ids[0]); return; }
    if (k.startsWith('Arrow') && ids.length) {
      e.preventDefault();
      const d = e.shiftKey ? 10 : 1;
      const dx = k === 'ArrowLeft' ? -d : k === 'ArrowRight' ? d : 0;
      const dy = k === 'ArrowUp' ? -d : k === 'ArrowDown' ? d : 0;
      this.commit(ops.moveOps(this.state, ids, dx, dy));
      return;
    }
    if (mod || e.altKey) return;
    const map = { v: 'select', h: 'hand', s: 'sticky', t: 'text', r: ['shape', 'rect'], o: ['shape', 'ellipse'], d: ['shape', 'diamond'], l: 'connector', p: 'pen', e: 'eraser', f: 'frame', i: 'image' };
    const t = map[lower];
    if (t) { e.preventDefault(); if (Array.isArray(t)) this.setTool(t[0], { shape: t[1] }); else this.setTool(t); }
  }

  toggleLock(els) {
    if (!els.length) return;
    const lock = els.some((el) => !el.locked);
    this.commit(ops.lockOps(this.state, els.map((el) => el.id), lock));
    if (lock) this.select([]);
  }

  // ---- clipboard (rule 19, 11.2) ----

  copy(e, cut) {
    if (this.typing(e) || !this.sel.size) return;
    const payload = ops.copyPayload(this.doc, [...this.sel]);
    this.clip = payload;
    e.clipboardData.setData('application/x-flats-board+json', JSON.stringify(payload));
    e.clipboardData.setData('text/plain', payload.elements.map((el) => el.text || el.alt || '').filter(Boolean).join('\n'));
    e.preventDefault();
    if (cut && this.canEdit) this.commit(ops.deleteOps(this.state, [...this.sel]));
  }

  pasteAt() {
    if (this.pointer && this.pointerInside()) return this.pointer;
    const v = this.renderer.viewRect();
    return center(v);
  }

  pointerInside() {
    const v = this.renderer.viewRect();
    return this.pointer[0] >= v.x && this.pointer[0] <= v.x + v.w && this.pointer[1] >= v.y && this.pointer[1] <= v.y + v.h;
  }

  paste(e) {
    if (this.typing(e) || !this.canEdit) return;
    const data = e.clipboardData;
    e.preventDefault();
    const files = [...data.files].filter((f) => f.type.startsWith('image/'));
    if (files.length) { uploadImages(this, files, this.pasteAt()); return; }
    const raw = data.getData('application/x-flats-board+json');
    let payload = null;
    if (raw) { try { payload = JSON.parse(raw); } catch { payload = null; } }
    const text = data.getData('text/plain');
    if (!payload && this.clip && text === this.clip.elements.map((el) => el.text || el.alt || '').filter(Boolean).join('\n')) payload = this.clip;
    if (payload && Array.isArray(payload.elements)) { this.pasteElements(payload, this.pasteAt()); return; }
    if (text && text.trim()) this.pasteText(text);
  }

  pasteElements(payload, at) {
    const { ops: list, ids } = ops.pasteOps(this.state, payload, at);
    if (this.commit(list)) this.select(ids.filter((id) => !this.state.get(id) || this.state.get(id).type !== 'frame' || ids.length === 1));
  }

  duplicate() {
    if (!this.sel.size) return;
    const payload = ops.copyPayload(this.doc, [...this.sel]);
    const b = this.contentBounds([...this.sel]);
    const [cx, cy] = center(b);
    this.pasteElements(payload, [cx + 24, cy + 24]);
  }

  async pasteText(text) {
    const lines = text.split(/\r?\n/).filter((l) => l.trim());
    let asOne = lines.length === 1 && text.includes('\n');
    if (lines.length > 20) {
      asOne = await askPasteMode(lines.length);
      if (asOne == null) return;
    }
    const { ops: list, ids } = ops.pasteTextOps(this.state, text, this.pasteAt(), asOne);
    if (this.commit(list)) this.select(ids);
  }

  // ---- notices ----

  overwritten(o, by) {
    const el = this.state.get(o.id);
    if (!el) return;
    const who = (by && by.label) || 'Someone';
    const what = (el.text || '').split('\n')[0].slice(0, 30);
    toast(`${who} just changed ${what ? `"${what}"` : 'this element'}, so the text you wrote was overwritten.`, {
      actions: [['Keep my text', () => this.keepText(el, o.text)]],
      timeout: 0,
    });
  }

  keepText(el, text) {
    const x = el.type === 'connector' ? (this.pointer ? this.pointer[0] : 0) : el.x + el.w + 24;
    const y = el.type === 'connector' ? (this.pointer ? this.pointer[1] : 0) : el.y;
    const n = ops.newElement(this.state, 'text', { x, y, w: el.w || 240, h: el.h || 60 }, { text });
    if (this.commit([ops.addOp(this.state, n)])) this.select([n.id]);
  }

  agentActivity(d, r) {
    const { added, changed, removed } = activityCounts(d, r);
    const until = performance.now() + 2000;
    for (const c of d.changes) {
      if (c.id !== '_board' && c.del === undefined) this.highlights.set(c.id, until);
    }
    const parts = [];
    if (added) parts.push(`added ${plural(added, 'element')}`);
    if (changed) parts.push(`changed ${plural(changed, 'element')}`);
    if (removed) parts.push(`removed ${plural(removed, 'element')}`);
    if (!parts.length) return;
    const who = (d.by && d.by.label) || 'The agent';
    const said = parts.length > 1 ? `${parts.slice(0, -1).join(', ')} and ${parts[parts.length - 1]}` : parts[0];
    toast(`${who} ${said}.`, { actions: [['Roll back to before this…', () => this.history.open()]] });
    this.renderer.dynamic();
    setTimeout(() => this.renderer.dynamic(), 2050);
  }

  // drawMarks draws search hits, agent highlights, my selection and the
  // keyboard focus ring on the dynamic layer.
  drawMarks(ctx, r) {
    const t = r.theme;
    const now = performance.now();
    for (const [id, until] of this.highlights) {
      if (until < now) { this.highlights.delete(id); continue; }
      const el = this.doc.get(id);
      if (!el) continue;
      ctx.strokeStyle = t.accent;
      ctx.globalAlpha = 0.55;
      ctx.lineWidth = 3;
      r.outline(ctx, el, 5);
      ctx.stroke();
      ctx.globalAlpha = 1;
    }
    this.find.draw(ctx, r);
    for (const el of this.selected()) {
      ctx.strokeStyle = t.sel;
      if (el.type === 'connector') {
        const pts = connectorPoints(el, (id) => this.doc.get(id));
        if (!pts) continue;
        ctx.globalAlpha = 0.45;
        ctx.lineWidth = 5;
        ctx.beginPath();
        pts.forEach(([x, y], i) => (i ? ctx.lineTo(...r.toScreen(x, y)) : ctx.moveTo(...r.toScreen(x, y))));
        ctx.stroke();
        ctx.globalAlpha = 1;
        continue;
      }
      ctx.lineWidth = 1.5;
      r.outline(ctx, el, 4);
      ctx.stroke();
    }
    if (this.focusId) {
      const el = this.doc.get(this.focusId);
      if (el) { ctx.strokeStyle = t.accent; ctx.lineWidth = 3; r.outline(ctx, el, 6); ctx.stroke(); }
    }
  }

  // ---- status and banners (11.3) ----

  status() {
    const el = this.statusEl;
    el.replaceChildren();
    const pend = this.state.pending.length;
    let text, cls;
    if (this.isPublic && this.welcomed && this.net.connected) {
      if (this.bannerKind === 'withdrawn') this.banner(null);
      el.hidden = true;
      return;
    }
    el.hidden = false;
    if (this.stopped) { cls = 'stopped'; el.appendChild(icon('warn')); text = 'Editing stopped'; }
    else if (!this.net.connected) {
      cls = 'offline'; el.appendChild(icon('wifi-off'));
      text = this.net.connecting ? 'Connecting…' : `Disconnected · Reconnecting${pend ? ` (${plural(pend, 'unsent edit')})` : ''}`;
    } else if (pend) { cls = 'saving'; el.appendChild(h('span', { class: 'dot' })); text = 'Saving…'; }
    else { cls = 'saved'; el.appendChild(h('span', { class: 'dot' })); text = 'Saved'; }
    el.className = 'save ' + cls;
    el.appendChild(h('span', { text }));
    if (this.welcomed && !this.net.connected && !this.net.connecting && pend && !this.stopped && !this.bannerKind) this.banner('offline');
    if (this.bannerKind === 'offline' && (this.net.connected || !pend)) this.banner(null);
    if (this.welcomed && this.isPublic && !this.net.connected && this.net.failures >= 2) this.checkPublic();
  }

  async checkPublic() {
    if (this.publicChecking || this.bannerKind === 'withdrawn') return;
    this.publicChecking = true;
    try {
      if (await this.net.withdrawn(() => fetch(location.pathname, { method: 'HEAD', cache: 'no-store' }))) this.banner('withdrawn');
    } finally {
      this.publicChecking = false;
    }
  }

  banner(kind, message) {
    const host = $('.banners');
    host.replaceChildren();
    this.bannerKind = kind;
    document.body.classList.toggle('has-banner', !!kind);
    if (!kind) return;
    const dl = (label) => h('button', { type: 'button', class: 'btn ghost', onclick: () => this.exporter.downloadRecovery() }, icon('download'), h('span', { text: label }));
    let b;
    if (kind === 'offline') {
      b = h('div', { class: 'banner info', role: 'status' }, icon('wifi-off'),
        h('span', { text: `${plural(this.state.pending.length, 'unsent edit is', 'unsent edits are')} in this tab. Closing or reloading the tab loses ${this.state.pending.length === 1 ? 'it' : 'them'}.` }),
        h('span', { class: 'spacer' }), dl('Download now'));
    } else if (kind === 'fatal') {
      b = h('div', { class: 'banner err', role: 'alert' }, icon('warn'), h('span', { text: message }), h('span', { class: 'spacer' }),
        this.state.pending.length ? dl('Download unsent edits') : null,
        h('button', { type: 'button', class: 'btn', onclick: () => location.reload() }, this.state.pending.length ? 'Reopen (discard edits)' : 'Reopen'));
      if (this.state.pending.length) this.exporter.prepareRecovery();
    } else if (kind === 'unload') {
      b = h('div', { class: 'banner warn', role: 'status' }, icon('warn'), h('span', { text: 'You stayed on this page because some edits are not sent yet.' }),
        h('span', { class: 'spacer' }), dl('Download unsent edits'));
    } else if (kind === 'quota') {
      b = h('div', { class: 'banner warn', role: 'alert' }, icon('warn'),
        h('span', { text: 'The disk quota is full, so your last edit was not saved. Remove images or old boards, or ask the operator to raise the quota. Further edits may be refused too.' }),
        h('span', { class: 'spacer' }), h('button', { type: 'button', class: 'btn', 'aria-label': 'Dismiss', onclick: () => this.banner(null) }, icon('close')));
    } else if (kind === 'withdrawn') {
      b = h('div', { class: 'banner err', role: 'alert' }, icon('warn'), h('span', { text: 'This board is no longer public. The view is not updating.' }));
    } else if (kind === 'revision') {
      b = message;
    }
    host.appendChild(b);
  }

  // ---- identity (rule 23) ----

  namePrompt() {
    const input = h('input', { id: 'name-input', autocomplete: 'nickname', maxlength: '80', value: this.name || '' });
    let chosen = false;
    const acts = h('div', { class: 'acts' }, h('span', { class: 'spacer' }),
      h('button', { type: 'button', class: 'btn ghost', text: 'Read only', onclick: () => dlg.close() }),
      h('button', { type: 'submit', class: 'btn primary', text: 'Open board' }));
    const form = h('form', { onsubmit: (e) => {
      e.preventDefault();
      const name = input.value.trim().slice(0, 80);
      if (!name) { input.focus(); return; }
      chosen = true;
      this.name = name;
      try { localStorage.setItem('flats-board-name', name); } catch { /* private mode: keep it for this tab */ }
      dlg.close();
    } },
    h('div', { class: 'field' }, h('label', { for: 'name-input', text: 'Name' }), input),
    h('p', { class: 'note', text: 'Until you choose a name you can only read the board, and others do not see your cursor. Close this with Esc or Read only, and reopen it from "Choose a name to edit" at the top.' }),
    acts);
    const dlg = dialog({
      title: 'Your name on this board',
      describe: 'This connection cannot verify names, so others see yours as "unverified". This browser remembers it.',
      body: form,
      className: 'narrow',
      onClose: () => {
        this.readOnlyChoice = !chosen;
        if (chosen) this.net.restart();
        this.applyMode();
      },
    });
    input.focus();
  }
}

function countVoid(results, reason) {
  if (!Array.isArray(results)) {
    return results && results.void ? results.void[reason] || 0 : 0;
  }
  let n = 0;
  for (const r of results) {
    if (r === 'void:' + reason) n++;
    else if (r && typeof r === 'object' && r.void) n += Object.values(r.void).filter((v) => v === reason).length;
  }
  return n;
}

function askPasteMode(count) {
  return new Promise((resolve) => {
    let result = null;
    const opt = (value, label, checked) => h('label', { class: 'opt' + (checked ? ' on' : '') },
      h('input', { type: 'radio', name: 'paste-mode', value, checked }), ' ', label);
    const opts = h('div', { class: 'opts', onchange: (e) => {
      for (const l of opts.children) l.classList.toggle('on', l.contains(e.target));
    } }, opt('lines', `One sticky per line (${count})`, true), opt('one', 'One text element', false));
    const acts = h('div', { class: 'acts' }, h('span', { class: 'spacer' }),
      h('button', { type: 'button', class: 'btn ghost', text: 'Cancel', onclick: () => dlg.close() }),
      h('button', { type: 'button', class: 'btn primary', text: 'Paste', onclick: () => {
        result = opts.querySelector('input:checked').value === 'one';
        dlg.close();
      } }));
    const dlg = dialog({
      title: `The pasted text has ${count} lines`,
      describe: 'Choose how to add it. Pasting while you edit a text puts it into that text.',
      body: opts, actions: acts, onClose: () => resolve(result),
    });
  });
}

window.board = new App();
