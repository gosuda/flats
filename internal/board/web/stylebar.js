// The small toolbar over the selection (rule 18, Appendix A style keys),
// quick-create handles (rule 16), alignment (rule 15), the alt text editor
// and the context menu. There is no properties panel.

import { $, h, icon, menu, popover, closePopover } from './ui.js';
import { STICKY_COLORS, INK_COLORS, SHAPES, SHAPE_NAMES, FONT_SIZES, KIND_NAMES, style, LIMITS } from './schema.js';
import { stickyColor, inkColor, shapeFill, frameFill } from './theme.js';
import { union, elementBounds } from './geom.js';
import * as ops from './ops.js';

const cap = (s) => s[0].toUpperCase() + s.slice(1);
const COLOR_NAMES = { ink: 'Black', highlight: 'Highlighter' };
const colorName = (c) => COLOR_NAMES[c] || cap(c);

function same(els, k) {
  const v = style(els[0], k);
  return els.every((el) => style(el, k) === v) ? v : undefined;
}

export class StyleBar {
  constructor(app) {
    this.app = app;
    this.el = h('div', { class: 'pop stylebar', role: 'toolbar', hidden: true });
    this.handles = h('div', { class: 'quick', hidden: true });
    const ui = $('.board-ui');
    ui.append(this.el, this.handles);
    this.raf = 0;
    this.busy = false; // a drag is in progress
  }

  update() {
    if (!this.raf) this.raf = requestAnimationFrame(() => { this.raf = 0; this.build(); });
  }

  hide() { this.el.hidden = true; this.handles.hidden = true; }

  build() {
    const app = this.app;
    const els = app.selected();
    if (!app.canEdit || !els.length || this.busy || app.editor || app.tool !== app.tools.select) {
      if (app.canEdit && app.tool === app.tools.pen && !this.busy) { this.penBar(); return; }
      this.hide();
      return;
    }
    const unlocked = els.filter((el) => !el.locked);
    const ids = unlocked.map((el) => el.id);
    const items = [];
    const types = new Set(unlocked.map((el) => el.type));
    if (unlocked.length && types.size === 1) items.push(...this.typeControls([...types][0], unlocked, ids));
    if (unlocked.length >= 2) {
      if (items.length) items.push(sep());
      items.push(...this.alignControls(ids, unlocked.length));
    }
    if (items.length) items.push(sep());
    const anyGroup = unlocked.some((el) => el.group);
    if (unlocked.length >= 2 && !anyGroup) items.push(this.button('group', 'Group', 'Group (⌘G)', () => app.commit(ops.groupOps(app.state, ids)), '⌘G'));
    if (anyGroup) items.push(this.button('group', 'Ungroup', 'Ungroup (⇧⌘G)', () => app.commit(ops.ungroupOps(app.state, ids))));
    const allLocked = els.every((el) => el.locked);
    items.push(this.button(allLocked ? 'unlock' : 'lock', null, allLocked ? 'Unlock (⇧⌘L)' : 'Lock (⇧⌘L)', () => app.toggleLock(els)));
    items.push(this.button('more', null, 'More', (e) => {
      const r = e.currentTarget.getBoundingClientRect();
      contextMenu(app, r.left, r.bottom + 4, els.length === 1 ? els[0] : null, e.currentTarget);
    }));
    this.el.setAttribute('aria-label', `Style for ${els.length} selected`);
    this.el.replaceChildren(...items);
    this.el.hidden = false;
    this.quick(els);
    this.place();
  }

  button(ic, text, label, onclick, kbd) {
    return h('button', { type: 'button', class: 'btn sm', 'aria-label': text ? null : label, title: label, onclick },
      ic ? icon(ic) : null, text ? h('span', { text }) : null, kbd ? h('span', { class: 'kbd', text: kbd }) : null);
  }

  swatch(color, label, pressed, onclick, cls = '') {
    const b = h('button', { type: 'button', class: 'sw ' + cls, 'aria-label': label, title: label, 'aria-pressed': String(pressed), onclick });
    if (color) b.style.background = color; else b.classList.add('none');
    return b;
  }

  // menuButton opens a menu of choices and shows the current one.
  menuButton(text, label, choices, current, apply) {
    const b = h('button', { type: 'button', class: 'btn sm', 'aria-haspopup': 'menu', 'aria-expanded': 'false', 'aria-label': label, title: label,
      onclick: () => {
        const r = b.getBoundingClientRect();
        popover(menu(choices.map(([v, l, ic]) => ({ label: l, icon: ic, checked: v === current, run: () => apply(v) })), label), r.left, r.bottom + 4, { opener: b, menu: true });
      } }, h('span', { text }), icon('down'));
    return b;
  }

  typeControls(type, els, ids) {
    const app = this.app;
    const t = app.renderer.theme;
    const set = (k) => (v) => app.commit(ops.styleOps(app.state, ids, k, v));
    const out = [];
    const font = (sizes) => {
      const cur = same(els, 'fontSize');
      return this.menuButton(cur === 'auto' ? 'Aa Auto' : cur ? `Aa ${cur}` : 'Aa', `Font size: ${cur === undefined ? 'mixed' : cur}`,
        sizes.map((s) => [s, s === 'auto' ? 'Auto (fit)' : `${s} px`]), cur, set('fontSize'));
    };
    const inks = (k, extra = []) => {
      const cur = same(els, k);
      return this.menuButton(colorName(cur || 'ink'), `${k === 'stroke' ? 'Line color' : 'Text color'}: ${cur ? colorName(cur) : 'mixed'}`,
        [...INK_COLORS, ...extra].map((c) => [c, colorName(c)]), cur, set(k));
    };
    if (type === 'sticky') {
      const cur = same(els, 'color');
      if (cur === undefined) out.push(h('span', { class: 'badge', title: 'The selected stickies have different colors', text: 'Mixed colors' }));
      for (const c of STICKY_COLORS) out.push(this.swatch(stickyColor(t, c), cur === undefined ? `Make all ${c}` : cap(c), cur === c, () => set('color')(c)));
      out.push(sep(), font(FONT_SIZES.sticky));
    } else if (type === 'shape') {
      const k = same(els, 'shape');
      out.push(this.menuButton(k ? SHAPE_NAMES[k] : 'Shape', `Shape: ${k ? SHAPE_NAMES[k] : 'mixed'}`,
        SHAPES.map((s) => [s, SHAPE_NAMES[s], s === 'round' || s === 'rect' ? 'rect' : s]), k, set('shape')), sep());
      const fill = same(els, 'fill');
      for (const c of ['none', 'white', ...STICKY_COLORS]) out.push(this.swatch(shapeFill(t, c), `Fill: ${c === 'none' ? 'none' : colorName(c)}`, fill === c, () => set('fill')(c)));
      out.push(sep(), inks('stroke'), ...this.strokeControls(els, set), font(FONT_SIZES.shape));
    } else if (type === 'text') {
      const cur = same(els, 'color');
      for (const c of INK_COLORS) out.push(this.swatch(inkColor(t, c), `Text color: ${colorName(c)}`, cur === c, () => set('color')(c)));
      out.push(sep(), font(FONT_SIZES.text));
      const al = same(els, 'align');
      for (const a of ['left', 'center', 'right']) {
        out.push(h('button', { type: 'button', class: 'btn sm', 'aria-label': `Align text ${a}`, title: `Align text ${a}`, 'aria-pressed': String(al === a), onclick: () => set('align')(a) }, icon('al-' + a)));
      }
    } else if (type === 'connector') {
      const route = same(els, 'route');
      out.push(
        h('button', { type: 'button', class: 'btn sm', 'aria-pressed': String(route === 'straight'), onclick: () => set('route')('straight') }, icon('straight'), h('span', { text: 'Straight' })),
        h('button', { type: 'button', class: 'btn sm', 'aria-pressed': String(route === 'elbow'), onclick: () => set('route')('elbow') }, icon('elbow'), h('span', { text: 'Elbow' })),
        sep());
      const h0 = same(els, 'head0'), h1 = same(els, 'head1');
      out.push(
        h('button', { type: 'button', class: 'btn sm', 'aria-label': 'Arrow at start', title: 'Arrow at start', 'aria-pressed': String(h0 === 'arrow'), onclick: () => set('head0')(h0 === 'arrow' ? 'none' : 'arrow') }, icon('arrow-start')),
        h('button', { type: 'button', class: 'btn sm', 'aria-label': 'Arrow at end', title: 'Arrow at end', 'aria-pressed': String(h1 === 'arrow'), onclick: () => set('head1')(h1 === 'arrow' ? 'none' : 'arrow') }, icon('arrow-end')),
        inks('stroke'), ...this.strokeControls(els, set), font(FONT_SIZES.connector));
      if (els.length === 1) out.push(this.button('label', 'Label', 'Edit the label', () => app.editText(els[0].id)));
    } else if (type === 'pen') {
      const cur = same(els, 'stroke');
      for (const c of [...INK_COLORS, 'highlight']) out.push(this.swatch(inkColor(t, c), `Line color: ${colorName(c)}`, cur === c, () => set('stroke')(c)));
      const w = same(els, 'width');
      out.push(sep(),
        h('button', { type: 'button', class: 'btn sm', 'aria-pressed': String(w === 'pen'), onclick: () => set('width')('pen') }, icon('pen'), h('span', { text: 'Pen' })),
        h('button', { type: 'button', class: 'btn sm', 'aria-pressed': String(w === 'marker'), onclick: () => set('width')('marker') }, icon('marker'), h('span', { text: 'Marker' })));
    } else if (type === 'frame') {
      const fill = same(els, 'fill');
      for (const c of ['white', 'gray', 'none']) out.push(this.swatch(frameFill(t, c), `Frame fill: ${c}`, fill === c, () => set('fill')(c)));
      if (els.length === 1) out.push(sep(), this.button(null, 'Rename', 'Rename the frame', () => app.editText(els[0].id)));
    } else if (type === 'image' && els.length === 1) {
      out.push(this.button(null, els[0].alt ? 'Alt text' : 'Add alt text', 'Alternative text', () => this.altEditor(els[0])));
    }
    return out;
  }

  strokeControls(els, set) {
    const sw = same(els, 'strokeWidth');
    const dash = same(els, 'dash');
    return [
      this.menuButton(`Line ${sw ?? ''}`.trim(), `Line width: ${sw ?? 'mixed'}`, [[1, 'Thin (1)'], [2, 'Regular (2)'], [4, 'Thick (4)']], sw, set('strokeWidth')),
      this.menuButton(dash ? cap(dash) : 'Line style', `Line style: ${dash || 'mixed'}`, [['solid', 'Solid'], ['dashed', 'Dashed'], ['dotted', 'Dotted']], dash, set('dash')),
    ];
  }

  alignControls(ids, n) {
    const app = this.app;
    const a = (mode, label) => this.button('al-' + mode, null, label, () => app.commit(ops.alignOps(app.state, ids, mode)));
    const out = [a('left', 'Align left'), a('center', 'Align centers horizontally'), a('right', 'Align right'),
      a('top', 'Align top'), a('middle', 'Align middles vertically'), a('bottom', 'Align bottom')];
    if (n >= 3) {
      out.push(sep(),
        this.button('dist-h', null, 'Distribute horizontally', () => app.commit(ops.distributeOps(app.state, ids, 'h'))),
        this.button('dist-v', null, 'Distribute vertically', () => app.commit(ops.distributeOps(app.state, ids, 'v'))));
    }
    return out;
  }

  penBar() {
    const app = this.app, pen = app.tools.pen, t = app.renderer.theme;
    const items = [
      h('button', { type: 'button', class: 'btn sm', 'aria-pressed': String(pen.mode === 'pen'), onclick: () => { pen.mode = 'pen'; this.update(); } }, icon('pen'), h('span', { text: 'Pen' })),
      h('button', { type: 'button', class: 'btn sm', 'aria-pressed': String(pen.mode === 'marker'), onclick: () => { pen.mode = 'marker'; this.update(); } }, icon('marker'), h('span', { text: 'Highlighter' })),
      sep(),
    ];
    for (const c of INK_COLORS) items.push(this.swatch(inkColor(t, c), `Pen color: ${colorName(c)}`, pen.color === c, () => { pen.color = c; this.update(); }));
    this.el.setAttribute('aria-label', 'Pen');
    this.el.replaceChildren(...items);
    this.el.hidden = false;
    this.handles.hidden = true;
    this.el.classList.add('docked');
    this.place();
  }

  // quick shows the + handles beside a single sticky or shape.
  quick(els) {
    const el = els.length === 1 ? els[0] : null;
    if (!el || el.locked || (el.type !== 'sticky' && el.type !== 'shape') || el.rot) { this.handles.hidden = true; return; }
    const app = this.app;
    this.handles.replaceChildren(...['up', 'right', 'down', 'left'].map((dir) => h('button', {
      type: 'button', class: 'qh', 'data-dir': dir, 'aria-label': `Create a connected ${KIND_NAMES[el.type].toLowerCase()} ${dir === 'up' ? 'above' : dir === 'down' ? 'below' : 'to the ' + dir}`,
      onclick: () => {
        const cur = app.state.get(el.id);
        if (!cur) return;
        const r = ops.quickCreateOps(app.state, cur, dir);
        if (app.commit(r.ops)) { app.select([r.el.id]); app.editText(r.el.id, { created: true }); }
      },
    }, icon('plus'))));
    this.handles.hidden = false;
  }

  place() {
    const app = this.app;
    if (this.el.hidden) return;
    if (this.el.classList.contains('docked') && app.tool === app.tools.pen) {
      this.el.style.left = ''; this.el.style.top = '';
      return;
    }
    this.el.classList.remove('docked');
    const els = app.selected();
    if (!els.length) return;
    let b = null;
    for (const el of els) b = union(b, app.doc.index.bounds(el.id) || elementBounds(el, (id) => app.doc.get(id)));
    const [x0, y0] = app.toScreen(b.x, b.y);
    const [x1, y1] = app.toScreen(b.x + b.w, b.y + b.h);
    const w = this.el.offsetWidth, hh = this.el.offsetHeight;
    const vw = app.renderer.w, vh = app.renderer.h;
    let top = y1 + 16;
    if (top + hh > vh - 8) top = y0 - hh - 44;
    this.el.style.left = Math.max(8, Math.min(vw - w - 8, (x0 + x1) / 2 - w / 2)) + 'px';
    this.el.style.top = Math.max(8, Math.min(vh - hh - 8, top)) + 'px';
    if (!this.handles.hidden && els.length === 1) {
      const pos = { up: [(x0 + x1) / 2, y0 - 26], down: [(x0 + x1) / 2, y1 + 26], left: [x0 - 26, (y0 + y1) / 2], right: [x1 + 26, (y0 + y1) / 2] };
      for (const b2 of this.handles.children) {
        const [px, py] = pos[b2.dataset.dir];
        b2.style.left = px - 12 + 'px';
        b2.style.top = py - 12 + 'px';
      }
    }
  }

  altEditor(el) {
    const app = this.app;
    const ta = h('textarea', { id: 'alt-text', rows: '3', maxlength: String(LIMITS.alt) });
    ta.value = el.alt || '';
    const box = h('div', { class: 'pop alt-pop', role: 'group', 'aria-labelledby': 'alt-l' },
      h('div', { class: 'field' }, h('label', { id: 'alt-l', for: 'alt-text' }, h('strong', { text: 'Alternative text' }), ' ', h('span', { class: 'note', text: 'Used by screen readers and Find' })), ta),
      h('div', { class: 'acts' }, h('span', { class: 'spacer' }),
        h('button', { type: 'button', class: 'btn ghost', text: 'Cancel', onclick: () => closePopover() }),
        h('button', { type: 'button', class: 'btn primary', text: 'Save', onclick: () => {
          closePopover();
          const cur = app.state.get(el.id);
          if (cur && ta.value !== (cur.alt || '')) app.commit(ops.setOps(app.state, [el.id], 'alt', ta.value));
        } })));
    const r = this.el.getBoundingClientRect();
    popover(box, r.left, r.bottom + 6);
    ta.focus();
  }
}

const sep = () => h('span', { class: 'vsep', 'aria-hidden': 'true' });

// contextMenu opens the right-click or long-press menu for el, the
// selection or the empty canvas.
export function contextMenu(app, x, y, el, opener) {
  if (!app.canEdit) return;
  const items = [];
  const kind = el ? KIND_NAMES[el.type] : '';
  const copy = () => { document.execCommand('copy'); };
  if (el && el.locked) {
    items.push({ note: `Locked ${kind.toLowerCase()}${el.text || el.alt ? ': ' + (el.text || el.alt).split('\n')[0].slice(0, 40) : ''}` },
      { label: 'Unlock', icon: 'unlock', kbd: '⇧⌘L', current: true, run: () => app.commit(ops.lockOps(app.state, [el.id], false)) },
      { label: 'Delete (unlock first)', icon: 'trash', disabled: true, run() {} });
  } else if (app.sel.size) {
    const ids = [...app.sel];
    const els = app.selected();
    const grouped = els.some((m) => m.group);
    items.push(
      { label: 'Copy', icon: 'copy', kbd: '⌘C', run: copy },
      { label: 'Duplicate', icon: 'dup', kbd: '⌘D', run: () => app.duplicate() },
      { label: 'Bring to front', icon: 'front', kbd: '⌘]', run: () => app.commit(ops.zorderOps(app.state, ids, 'front')) },
      { label: 'Bring forward', icon: 'front', kbd: '⌥⌘]', run: () => app.commit(ops.zorderOps(app.state, ids, 'forward')) },
      { label: 'Send backward', icon: 'back', kbd: '⌥⌘[', run: () => app.commit(ops.zorderOps(app.state, ids, 'backward')) },
      { label: 'Send to back', icon: 'back', kbd: '⌘[', run: () => app.commit(ops.zorderOps(app.state, ids, 'back')) },
      { label: 'Lock', icon: 'lock', kbd: '⇧⌘L', run: () => app.toggleLock(els) },
      grouped ? { label: 'Ungroup', icon: 'group', kbd: '⇧⌘G', run: () => app.commit(ops.ungroupOps(app.state, ids)) }
        : { label: 'Group', icon: 'group', kbd: '⌘G', disabled: els.length < 2, run: () => app.commit(ops.groupOps(app.state, ids)) },
      '-',
      ...(app.isMember ? [{ label: 'Send to agent', icon: 'send', run: () => app.share.send() }, '-'] : []));
    const frames = els.filter((m) => m.type === 'frame');
    if (frames.length) items.push({ label: frames.length > 1 ? 'Delete frames and contents' : 'Delete frame and contents', icon: 'trash', danger: true, run: () => app.commit(ops.deleteFrameContentsOps(app.state, frames.map((f) => f.id))) });
    items.push({ label: 'Delete', icon: 'trash', kbd: '⌫', danger: true, run: () => app.commit(ops.deleteOps(app.state, ids)) });
  } else {
    const locked = [...app.doc.all()].filter((m) => m.locked).length;
    items.push(
      { label: 'Paste', icon: 'copy', kbd: '⌘V', disabled: !app.clip, run: () => app.pasteElements(app.clip, app.pointer || app.pasteAt()) },
      { label: 'Select all', icon: 'select', kbd: '⌘A', run: () => app.select([...app.doc.all()].filter((m) => !m.locked).map((m) => m.id)) },
      { label: `Locked elements (${locked})`, icon: 'lock', disabled: !locked, run: () => app.locked.open() },
      '-',
      { label: 'Fit all', icon: 'fit', kbd: '⇧1', run: () => app.fitAll() });
  }
  closePopover();
  popover(menu(items, el ? `${kind} menu` : 'Board menu'), x, y, { opener, menu: true });
}
