// A small DOM stand-in for the database view tests under Node: elements,
// attributes, text, events, focus, a scrolling box and the selectors the
// database modules use. Every rendered row is ROW_HEIGHT pixels tall.
'use strict';


export const ROW_HEIGHT = 36;

class El {
  constructor(tag) {
    this.tagName = tag.toUpperCase();
    this.children = [];
    this.parentNode = null;
    this.attrs = new Map();
    this.dataset = {};
    this.style = {};
    this.listeners = new Map();
    this.value = '';
    this.checked = false;
    this.disabled = false;
    this.scrollTop = 0;
    this.clientHeight = 0;
    this._text = '';
  }
  get className() { return this.attrs.get('class') ?? ''; }
  set className(v) { this.attrs.set('class', String(v)); }
  get offsetHeight() { return this.tagName === 'TR' && !this.className.includes('spacer') ? ROW_HEIGHT : 0; }
  get textContent() { return this.tagName === '#TEXT' ? this._text : this.children.map((c) => c.textContent).join(''); }
  set textContent(v) { if (this.tagName === '#TEXT') this._text = String(v); else this.replaceChildren(...(v === '' ? [] : [String(v)])); }
  get isConnected() { let n = this; while (n.parentNode) n = n.parentNode; return n === doc.root; }
  get firstChild() { return this.children[0] ?? null; }
  get nextSibling() { const p = this.parentNode; return p ? p.children[p.children.indexOf(this) + 1] ?? null : null; }
  insertBefore(c, ref) {
    if (!ref) return this.appendChild(c);
    if (c === ref) return c;
    const wasActive = doc.activeElement === c || c.contains(doc.activeElement);
    if (c.parentNode) c.remove();
    c.parentNode = this;
    this.children.splice(this.children.indexOf(ref), 0, c);
    // A browser blurs a focused element that is moved; record it.
    if (wasActive) doc.moved = (doc.moved || 0) + 1;
    return c;
  }
  append(...kids) { for (const k of kids) this.appendChild(typeof k === 'string' ? doc.createTextNode(k) : k); }
  appendChild(c) {
    if (c.parentNode && (doc.activeElement === c || c.contains(doc.activeElement))) doc.moved = (doc.moved || 0) + 1;
    if (c.parentNode) c.remove();
    c.parentNode = this;
    this.children.push(c);
    return c;
  }
  replaceChildren(...kids) { for (const c of [...this.children]) c.remove(); this.append(...kids); }
  remove() {
    if (!this.parentNode) return;
    this.parentNode.children = this.parentNode.children.filter((c) => c !== this);
    this.parentNode = null;
    if (doc.activeElement && !doc.activeElement.isConnected) doc.activeElement = doc.body;
  }
  contains(n) { for (; n; n = n.parentNode) if (n === this) return true; return false; }
  setAttribute(k, v) {
    this.attrs.set(k, String(v));
    if (k.startsWith('data-')) this.dataset[k.slice(5).replace(/-(.)/g, (_, c) => c.toUpperCase())] = String(v);
  }
  getAttribute(k) {
    if (k === 'data-row' && this.dataset.row !== undefined) return this.dataset.row;
    return this.attrs.has(k) ? this.attrs.get(k) : null;
  }
  hasAttribute(k) { return this.getAttribute(k) !== null; }
  removeAttribute(k) { this.attrs.delete(k); }
  addEventListener(t, fn) { if (!this.listeners.has(t)) this.listeners.set(t, new Set()); this.listeners.get(t).add(fn); }
  removeEventListener(t, fn) { this.listeners.get(t)?.delete(fn); }
  // dispatch bubbles to ancestors unless a handler stops propagation.
  dispatch(type, init = {}) {
    let stopped = false;
    const e = { type, target: this, defaultPrevented: false, preventDefault() { this.defaultPrevented = true; }, stopPropagation() { stopped = true; }, ...init };
    for (let n = this; n && !stopped; n = n.parentNode) {
      e.currentTarget = n;
      for (const fn of [...(n.listeners.get(type) || [])]) fn(e);
    }
    return e;
  }
  click() { this.dispatch('click'); }
  focus() {
    if (!this.isConnected) return;
    const prev = doc.activeElement;
    if (prev === this) return;
    doc.activeElement = this;
    if (prev && prev !== doc.body) prev.dispatch('blur');
    this.dispatch('focus');
  }
  closest(sel) { for (let n = this; n; n = n.parentNode) if (n.tagName && matches(n, sel)) return n; return null; }
  querySelector(sel) { return this.querySelectorAll(sel)[0] ?? null; }
  querySelectorAll(sel) {
    const out = [];
    const walk = (n) => { for (const c of n.children) { if (c.tagName !== '#TEXT' && matches(c, sel)) out.push(c); walk(c); } };
    walk(this);
    return out;
  }
}

function matches(n, sel) {
  return sel.split(',').some((alt) => {
    const parts = alt.trim().split(/\s+/);
    if (!one(n, parts.at(-1))) return false;
    let i = parts.length - 2;
    for (let p = n.parentNode; p && i >= 0; p = p.parentNode) if (p.tagName && one(p, parts[i])) i--;
    return i < 0;
  });
}

function one(n, sel) {
  const re = /([.#]?)([\w-]+)|\[([\w-]+)(?:=([^\]]+))?\]/g;
  for (const m of sel.matchAll(re)) {
    if (m[2] != null) {
      if (m[1] === '.' && !n.className.split(/\s+/).includes(m[2])) return false;
      if (m[1] === '#' && n.getAttribute('id') !== m[2]) return false;
      if (!m[1] && n.tagName !== m[2].toUpperCase()) return false;
    } else {
      const v = n.getAttribute(m[3]);
      if (v == null || (m[4] != null && v !== m[4].replace(/^["']|["']$/g, ''))) return false;
    }
  }
  return true;
}

const doc = {};

export function installDOM() {
  doc.root = new El('html');
  doc.body = doc.root.appendChild(new El('body'));
  doc.activeElement = doc.body;
  doc.moved = 0;
  doc.createElement = (tag) => new El(tag);
  doc.createTextNode = (text) => { const t = new El('#text'); t._text = String(text); return t; };
  const listeners = new El('document');
  doc.addEventListener = (t, fn) => listeners.addEventListener(t, fn);
  doc.removeEventListener = (t, fn) => listeners.removeEventListener(t, fn);
  doc.dispatch = (t, init) => listeners.dispatch(t, init);
  doc.querySelector = (s) => doc.root.querySelector(s);
  doc.querySelectorAll = (s) => doc.root.querySelectorAll(s);
  globalThis.document = doc;
  return doc;
}

export function mount(clientHeight = 360) {
  const el = doc.createElement('div');
  doc.body.appendChild(el);
  el.sizeScrollers = () => { for (const s of el.querySelectorAll('.tablewrap')) s.clientHeight = clientHeight; };
  return el;
}

// dataTransfer is the drag payload shared by one dragstart/drop pair.
export function dataTransfer() {
  const data = new Map();
  return { setData: (k, v) => data.set(k, v), getData: (k) => data.get(k) ?? '' };
}
