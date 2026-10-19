// In-place text editing (rule 17, TECH 4.9). While an input method is
// composing, remote changes to the element do not touch the editor; an
// element deleted meanwhile keeps the editor until composition ends, and
// then offers "Keep my text" instead of committing.

import { h, toast } from './ui.js';
import { style, textLimit, KIND_NAMES, clampGeom, geomOf } from './schema.js';
import { stickyColor, inkColor } from './theme.js';
import { connectorPoints, pointAlong, labelBox } from './geom.js';
import { fitText, FONT } from './render.js';
import * as ops from './ops.js';

export class TextEditor {
  constructor(app, el, { created = false } = {}) {
    this.app = app;
    this.opener = document.activeElement;
    this.id = el.id;
    this.type = el.type;
    this.created = created;
    this.last = el;
    this.start = el.text || '';
    // baseV is the text version the person sees as they start (TECH 4.9).
    this.baseV = ops.textVersion(app.state, el.id);
    this.composing = false;
    this.vanished = false;
    this.done = false;
    const single = el.type === 'frame' || el.type === 'connector';
    this.ta = h('textarea', {
      class: 'text-editor ' + el.type, 'aria-label': `Edit ${single && el.type === 'frame' ? 'frame name' : single ? 'connector label' : KIND_NAMES[el.type].toLowerCase() + ' text'}`,
      maxlength: String(textLimit(el)), spellcheck: 'true', rows: '1',
    });
    this.ta.value = this.start;
    this.ta.addEventListener('compositionstart', () => { this.composing = true; });
    this.ta.addEventListener('compositionend', () => {
      this.composing = false;
      // remote() checks the element again: one restored by a rollback or
      // revive during composition is no longer vanished.
      this.remote();
    });
    this.ta.addEventListener('input', () => this.place());
    this.ta.addEventListener('keydown', (e) => {
      if (e.isComposing || this.composing) return;
      if (e.key === 'Escape') { e.preventDefault(); this.cancel(); }
      else if (e.key === 'Enter' && (e.metaKey || e.ctrlKey || single)) { e.preventDefault(); this.commit(); }
    });
    this.ta.addEventListener('blur', () => { if (!this.composing) this.commit(); });
    document.querySelector('.board-ui').appendChild(this.ta);
    app.renderer.setHidden(this.id);
    app.presence.send({ edit: this.id });
    app.styleBar.hide();
    this.place();
    this.ta.focus();
    if (created || single) this.ta.select();
    else this.ta.setSelectionRange(this.ta.value.length, this.ta.value.length);
  }

  contains(node) { return node === this.ta; }

  // place positions the editor over the element and matches its type.
  place() {
    const app = this.app;
    const el = app.state.get(this.id) || this.last;
    this.last = el;
    const z = app.cam.z;
    const t = app.renderer.theme;
    const s = this.ta.style;
    let box, size, pad = 0, align = 'left';
    s.background = 'transparent';
    s.color = t.ink;
    s.fontWeight = '400';
    if (el.type === 'connector') {
      const pts = connectorPoints(el, (id) => app.state.get(id));
      const mid = pts ? pointAlong(pts, 0.5) : [0, 0];
      size = style(el, 'fontSize');
      const b = labelBox({ ...el, text: this.ta.value || ' ' }, mid);
      box = { x: b.x - 20, y: b.y, w: b.w + 40, h: b.h };
      align = 'center';
      s.background = t.labelBg;
    } else if (el.type === 'frame') {
      size = 13 / z;
      box = { x: el.x, y: el.y - 26 / z, w: Math.max(el.w, 160 / z), h: 22 / z };
      s.fontWeight = '600';
      s.color = t.muted;
      s.background = t.canvas;
    } else {
      box = { x: el.x, y: el.y, w: el.w, h: el.h };
      if (el.type === 'sticky') {
        pad = 12;
        const fs = style(el, 'fontSize');
        size = fs === 'auto'
          ? fitText(app.renderer.sctx, this.ta.value || ' ', el.w - 24, el.h - 38, 16, true).size
          : fs;
        s.background = stickyColor(t, style(el, 'color'));
      } else if (el.type === 'shape') {
        pad = 8;
        size = style(el, 'fontSize');
        align = 'center';
      } else {
        pad = 4;
        size = style(el, 'fontSize');
        align = style(el, 'align');
        s.color = inkColor(t, style(el, 'color'));
      }
    }
    const [x, y] = app.toScreen(box.x, box.y);
    s.left = x + 'px';
    s.top = y + 'px';
    s.width = box.w * z + 'px';
    s.minHeight = box.h * z + 'px';
    s.height = 'auto';
    s.padding = pad * z + 'px';
    s.font = `${s.fontWeight} ${size * z}px/1.38 ${FONT}`;
    s.textAlign = align;
    s.transform = el.rot ? `rotate(${el.rot}deg)` : '';
    if (el.type === 'text') s.height = Math.max(box.h * z, this.ta.scrollHeight) + 'px';
  }

  // remote reacts to a delta while the editor is open. It never changes the
  // editor during composition; an untouched editor follows remote text.
  remote() {
    if (this.done) return;
    const el = this.app.state.get(this.id);
    if (!el) {
      if (this.composing) this.vanished = true;
      else this.vanish();
      return;
    }
    this.vanished = false;
    if (this.composing) return;
    if (this.ta.value === this.start) {
      // Untouched: follow the remote text, which the person now sees.
      if ((el.text || '') !== this.start) {
        this.start = el.text || '';
        this.ta.value = this.start;
      }
      this.baseV = ops.textVersion(this.app.state, this.id);
    } else if (this.baseV == null) {
      // An element this tab created gets its first version from its own
      // add; the person started from that text if the server still has it.
      const base = this.app.state.server.get(this.id);
      if (base && (base.text || '') === this.start) this.baseV = ops.textVersion(this.app.state, this.id);
    }
    this.place();
  }

  vanish() {
    const text = this.ta.value;
    const last = this.last;
    this.close();
    if (text.trim() && text !== this.start) {
      toast('The element you were editing was deleted or rolled back.', {
        actions: [['Keep my text', () => this.app.keepText(last, text)]], timeout: 0,
      });
    }
  }

  commit() {
    if (this.done) return;
    const app = this.app;
    const el = app.state.get(this.id);
    if (!el) { this.vanish(); return; }
    const text = this.ta.value;
    const height = this.type === 'text' ? this.fittedHeight(el, text) : null;
    this.close();
    if (this.created && this.type === 'text' && !text.trim()) {
      app.commit(ops.deleteOps(app.state, [el.id]));
      return;
    }
    const list = [];
    if (text !== this.start) list.push(ops.textOp(app.state, el, text, this.baseV));
    if (height != null && Math.abs(height - el.h) >= 1) {
      list.push({ op: 'set', id: el.id, inc: app.state.incOf(el.id), g: 'geom', val: clampGeom({ ...geomOf(el), h: height }) });
    }
    app.commit(list);
  }

  fittedHeight(el, text) {
    const fs = style(el, 'fontSize');
    const lay = fitText(this.app.renderer.sctx, text || ' ', el.w - 8, Infinity, fs, false);
    return Math.ceil(lay.lines.length * fs * 1.38 + 8);
  }

  cancel() {
    const created = this.created && this.type === 'text' && !this.start;
    const id = this.id;
    this.close();
    if (created) this.app.commit(ops.deleteOps(this.app.state, [id]));
  }

  close() {
    if (this.done) return;
    this.done = true;
    this.ta.remove();
    const app = this.app;
    if (app.editor === this) app.editor = null;
    app.renderer.setHidden(null);
    app.presence.send({ edit: null });
    app.styleBar.update();
    if (this.opener && this.opener.isConnected && this.opener.closest('#a11y')) this.opener.focus({ preventScroll: true });
    else app.focusBoard();
  }
}
