// Keyboard and screen reader access (rule 13, TECH 11.5): a visually hidden
// element list (role="tree") that shows itself while it has focus. Frames are
// branches, elements leaves, in reading order per frame.

import { $, h, announce } from './ui.js';
import { KIND_NAMES, SHAPE_NAMES, style, authorOf } from './schema.js';
import * as ops from './ops.js';

const byReading = (a, b) => Math.round(a.y / 16) - Math.round(b.y / 16) || a.x - b.x;

export class A11yTree {
  constructor(app) {
    this.app = app;
    this.panel = $('#a11y');
    this.tree = $('#element-tree');
    this.stale = true;
    this.timer = 0;
    this.current = null;     // focused element id
    this.collapsed = new Set();
    this.tree.addEventListener('keydown', (e) => this.key(e));
    this.tree.addEventListener('focusin', (e) => {
      const li = e.target.closest('[role=treeitem]');
      const id = li && li.dataset.id;
      if (this.stale) {
        if (id) this.current = id;
        this.build(true);
        return;
      }
      if (id) this.focus(id, false);
    });
    this.panel.addEventListener('focusout', (e) => {
      if (!this.panel.contains(e.relatedTarget)) { this.app.focusId = null; this.app.renderer.dynamic(); }
    });
    this.build();
  }

  active() { return this.panel.contains(document.activeElement); }

  dirty() {
    this.stale = true;
    if (!this.active() && this.tree.childElementCount) return;
    clearTimeout(this.timer);
    this.timer = setTimeout(() => this.build(), 400);
  }

  selection() {
    for (const li of this.tree.querySelectorAll('[aria-selected=true]')) li.setAttribute('aria-selected', 'false');
    for (const id of this.app.sel) {
      const li = this.item(id);
      if (li) li.setAttribute('aria-selected', 'true');
    }
  }

  item(id) { return this.tree.querySelector(`[data-id="${CSS.escape(id)}"]`); }

  name(el) {
    if (!el) return 'a removed element';
    const text = (el.text || el.alt || '').split('\n')[0].slice(0, 60);
    return text ? `"${text}"` : KIND_NAMES[el.type].toLowerCase();
  }

  label(el) {
    const doc = this.app.doc;
    let kind = KIND_NAMES[el.type];
    if (el.type === 'sticky') kind += ', ' + style(el, 'color');
    if (el.type === 'shape') kind += ', ' + SHAPE_NAMES[style(el, 'shape')].toLowerCase();
    if (el.locked) kind += ', locked';
    const parts = [];
    if (el.type === 'connector') {
      const [a, b] = el.ends.map((e) => (e.el ? this.name(doc.get(e.el)) : 'a free point'));
      parts.push(`${kind} from ${a} to ${b}${el.text ? ': ' + el.text : ''}`);
    } else {
      const text = el.type === 'image' ? el.alt || 'no description' : el.text || '';
      parts.push(text ? `${kind}: ${text}` : kind);
    }
    const f = el.frame && doc.get(el.frame);
    if (f) parts.push(`Frame ${f.text || 'untitled'}`);
    // Screen readers hear the guest mark the canvas draws next to the name.
    const author = this.app.isMember ? authorOf(el, this.app.names) : null;
    if (author && author.guest) parts.push(`Created by ${author.name} (guest)`);
    if (el.type !== 'connector') {
      const to = [];
      for (const cid of doc.index.connectorsOf(el.id)) {
        const c = doc.get(cid);
        if (!c) continue;
        for (const e of c.ends) if (e.el && e.el !== el.id) to.push(this.name(doc.get(e.el)));
      }
      if (to.length) parts.push('Connected to: ' + to.join(', '));
    }
    return parts.join('. ');
  }

  build(focus = this.active()) {
    clearTimeout(this.timer);
    this.stale = false;
    const doc = this.app.doc;
    const all = [...doc.all()];
    const members = new Map();
    const top = [];
    for (const el of all) {
      if (el.frame && doc.get(el.frame)) {
        if (!members.has(el.frame)) members.set(el.frame, []);
        members.get(el.frame).push(el);
      } else top.push(el);
    }
    const geo = (el) => (el.type === 'connector' ? { x: 0, y: 0, ...(doc.index.bounds(el.id) || {}) } : el);
    const sortEls = (list) => list.map((el) => ({ el, g: geo(el) })).sort((a, b) => byReading(a.g, b.g)).map((p) => p.el);
    const leaf = (el, level, pos, size) => h('li', {
      role: 'treeitem', 'aria-level': String(level), 'aria-posinset': String(pos), 'aria-setsize': String(size),
      'aria-selected': String(this.app.sel.has(el.id)), tabindex: '-1', 'data-id': el.id, 'aria-label': this.label(el),
    }, h('span', { class: 'note', text: KIND_NAMES[el.type] }), h('span', { text: (el.text || el.alt || '').split('\n')[0] }));
    const roots = sortEls(top);
    const items = roots.map((el, i) => {
      if (el.type !== 'frame') return leaf(el, 1, i + 1, roots.length);
      const kids = sortEls(members.get(el.id) || []);
      const open = !this.collapsed.has(el.id);
      const li = h('li', {
        role: 'treeitem', 'aria-level': '1', 'aria-posinset': String(i + 1), 'aria-setsize': String(roots.length),
        'aria-expanded': String(open), 'aria-selected': String(this.app.sel.has(el.id)), tabindex: '-1', 'data-id': el.id,
        'aria-label': `Frame ${el.text || 'untitled'}, ${kids.length} element${kids.length === 1 ? '' : 's'}`,
      }, h('span', { class: 'frame-name', text: el.text || 'Frame' }));
      if (open && kids.length) li.appendChild(h('ul', { role: 'group' }, kids.map((k, j) => leaf(k, 2, j + 1, kids.length))));
      return li;
    });
    this.tree.replaceChildren(...items);
    const cur = (this.current && this.item(this.current)) || this.tree.querySelector('[role=treeitem]');
    if (cur) cur.tabIndex = 0;
    else this.tree.appendChild(h('li', { role: 'treeitem', tabindex: '0', 'aria-label': 'The board is empty', text: 'The board is empty' }));
    if (focus && cur) this.focus(cur.dataset.id || '', true);
  }

  // relabel refreshes the labels of the elements editor key created, after
  // the name presence gives for it changed. Frame labels name no creator.
  relabel(key) {
    for (const li of this.visible()) {
      const el = this.app.doc.get(li.dataset.id);
      if (el && el.by === key && el.type !== 'frame') li.setAttribute('aria-label', this.label(el));
    }
  }

  visible() { return [...this.tree.querySelectorAll('[role=treeitem]')].filter((li) => li.dataset.id); }

  focus(id, move = true) {
    const li = id ? this.item(id) : this.tree.querySelector('[role=treeitem]');
    if (!li) return;
    if (!id) { li.focus({ preventScroll: true }); return; }
    for (const other of this.tree.querySelectorAll('[tabindex="0"]')) other.tabIndex = -1;
    li.tabIndex = 0;
    if (move) li.focus({ preventScroll: true });
    this.current = id;
    const app = this.app;
    app.focusId = id;
    const el = app.doc.get(id);
    if (el && !el.locked) app.select([id]);
    app.reveal(id);
    app.renderer.dynamic();
  }

  key(e) {
    const li = e.target.closest('[role=treeitem]');
    if (!li || !li.dataset.id) return;
    const app = this.app;
    const id = li.dataset.id;
    const list = this.visible();
    const i = list.indexOf(li);
    const k = e.key;
    let handled = true;
    if (e.shiftKey && k.startsWith('Arrow')) {
      const dx = k === 'ArrowLeft' ? -10 : k === 'ArrowRight' ? 10 : 0;
      const dy = k === 'ArrowUp' ? -10 : k === 'ArrowDown' ? 10 : 0;
      if (app.canEdit) app.commit(ops.moveOps(app.state, [id], dx, dy));
    } else if (k === 'ArrowDown') { if (list[i + 1]) this.focus(list[i + 1].dataset.id); }
    else if (k === 'ArrowUp') { if (list[i - 1]) this.focus(list[i - 1].dataset.id); }
    else if (k === 'Home') { if (list[0]) this.focus(list[0].dataset.id); }
    else if (k === 'End') { if (list.length) this.focus(list[list.length - 1].dataset.id); }
    else if (k === 'ArrowRight') {
      if (li.getAttribute('aria-expanded') === 'false') { this.collapsed.delete(id); this.build(); }
      else if (li.getAttribute('aria-expanded') === 'true' && list[i + 1]) this.focus(list[i + 1].dataset.id);
    } else if (k === 'ArrowLeft') {
      if (li.getAttribute('aria-expanded') === 'true') { this.collapsed.add(id); this.build(); }
      else { const parent = li.parentElement.closest('[role=treeitem]'); if (parent) this.focus(parent.dataset.id); }
    } else if (k === 'Enter') {
      app.editText(id);
      announce('Editing text. Press Escape to return to the list.');
    } else if (k === 'Delete' || k === 'Backspace') {
      if (app.canEdit) {
        const next = list[i + 1] || list[i - 1];
        app.commit(ops.deleteOps(app.state, [id]));
        if (next) this.current = next.dataset.id;
      }
    } else if (k === 'Escape') {
      app.select([]);
    } else handled = false;
    if (handled) { e.preventDefault(); e.stopPropagation(); }
  }
}
