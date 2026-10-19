// A tiny DOM stand-in for tests of the UI modules under Node: just enough
// elements, events, focus and selectors for ui.js and textedit.js.

class Node {
  constructor(tag) {
    this.tagName = tag.toUpperCase();
    this.children = [];
    this.parentNode = null;
    this.attrs = new Map();
    this.dataset = {};
    this.style = {};
    this.listeners = new Map();
    this.className = '';
    this.textContent = '';
    this.value = '';
    this.scrollHeight = 0;
  }
  get firstChild() { return this.children[0] || null; }
  get isConnected() { let n = this; while (n.parentNode) n = n.parentNode; return n === doc.root; }
  appendChild(c) { if (c.parentNode) c.remove(); c.parentNode = this; this.children.push(c); return c; }
  remove() {
    if (!this.parentNode) return;
    const p = this.parentNode;
    p.children = p.children.filter((c) => c !== this);
    this.parentNode = null;
    if (doc.activeElement === this) doc.activeElement = doc.body;
  }
  setAttribute(k, v) { this.attrs.set(k, String(v)); if (k === 'class') this.className = String(v); }
  getAttribute(k) { return this.attrs.has(k) ? this.attrs.get(k) : null; }
  addEventListener(type, fn) {
    if (!this.listeners.has(type)) this.listeners.set(type, new Set());
    this.listeners.get(type).add(fn);
  }
  removeEventListener(type, fn) { this.listeners.get(type)?.delete(fn); }
  dispatch(type, ev = {}) {
    const e = { type, target: this, preventDefault() {}, stopPropagation() {}, ...ev };
    for (const fn of [...(this.listeners.get(type) || [])]) fn(e);
    return e;
  }
  focus() { doc.activeElement = this; }
  select() {}
  setSelectionRange() {}
  contains(n) { for (; n; n = n.parentNode) if (n === this) return true; return false; }
  closest(sel) { for (let n = this; n; n = n.parentNode) if (matches(n, sel)) return n; return null; }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  querySelectorAll(sel) {
    const out = [];
    const walk = (n) => { for (const c of n.children) { if (matches(c, sel)) out.push(c); walk(c); } };
    walk(this);
    return out;
  }
  getBoundingClientRect() { return { left: 0, top: 0, right: 100, bottom: 40, width: 100, height: 40 }; }
}

// matches understands only the selectors the tested modules use: .class,
// #id and tag.
function matches(n, sel) {
  if (!n.tagName) return false;
  if (sel.startsWith('.')) return n.className.split(/\s+/).includes(sel.slice(1));
  if (sel.startsWith('#')) return n.getAttribute('id') === sel.slice(1);
  return n.tagName === sel.toUpperCase();
}

const doc = {};

// installDOM sets up globalThis.document with a body and the named hosts
// (class names) under it, and returns the document.
export function installDOM(hosts = []) {
  doc.root = new Node('html');
  doc.body = doc.root.appendChild(new Node('body'));
  doc.activeElement = doc.body;
  doc.createElement = (tag) => new Node(tag);
  doc.createElementNS = (_ns, tag) => new Node(tag);
  doc.createTextNode = (text) => Object.assign(new Node('#text'), { textContent: text, tagName: '' });
  doc.querySelector = (sel) => doc.root.querySelector(sel);
  doc.querySelectorAll = (sel) => doc.root.querySelectorAll(sel);
  const listeners = new Node('document');
  doc.addEventListener = (t, fn) => listeners.addEventListener(t, fn);
  doc.removeEventListener = (t, fn) => listeners.removeEventListener(t, fn);
  doc.dispatch = (t, ev) => listeners.dispatch(t, ev);
  doc.listenerCount = (t) => listeners.listeners.get(t)?.size || 0;
  for (const h of hosts) {
    const n = new Node(h.tag || 'div');
    n.className = h.cls;
    if (h.tabindex != null) n.setAttribute('tabindex', h.tabindex);
    doc.body.appendChild(n);
  }
  globalThis.document = doc;
  globalThis.innerWidth = 1200;
  globalThis.innerHeight = 800;
  return doc;
}
