// A tiny DOM stand-in for tests of the UI modules under Node: just enough
// elements, events, focus and selectors for ui.js, textedit.js and, with
// page.mjs, the whole app.

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
  get classList() {
    const names = () => this.className.split(/\s+/).filter(Boolean);
    const set = (list) => { this.className = list.join(' '); };
    return {
      contains: (c) => names().includes(c),
      add: (...cs) => set([...new Set([...names(), ...cs])]),
      remove: (...cs) => set(names().filter((n) => !cs.includes(n))),
      toggle: (c, on = !names().includes(c)) => { if (on) set([...new Set([...names(), c])]); else set(names().filter((n) => n !== c)); return on; },
    };
  }
  append(...kids) { for (const k of kids) this.appendChild(typeof k === 'string' ? doc.createTextNode(k) : k); }
  click() { this.dispatch('click'); }
  replaceChildren(...kids) {
    for (const c of [...this.children]) c.remove();
    for (const k of kids) this.appendChild(typeof k === 'string' ? doc.createTextNode(k) : k);
  }
  replaceWith(n) {
    const p = this.parentNode;
    if (!p) return;
    if (n.parentNode) n.remove();
    p.children[p.children.indexOf(this)] = n;
    n.parentNode = p;
    this.parentNode = null;
  }
  get isConnected() { let n = this; while (n.parentNode) n = n.parentNode; return n === doc.root; }
  appendChild(c) { if (c.parentNode) c.remove(); c.parentNode = this; this.children.push(c); return c; }
  remove() {
    if (!this.parentNode) return;
    const p = this.parentNode;
    p.children = p.children.filter((c) => c !== this);
    this.parentNode = null;
    if (doc.activeElement === this) doc.activeElement = doc.body;
  }
  setAttribute(k, v) {
    this.attrs.set(k, String(v));
    if (k === 'class') this.className = String(v);
    if (k === 'value') this.value = String(v); // the initial value of an input
    if (k.startsWith('data-')) this.dataset[k.slice(5).replace(/-(.)/g, (_, c) => c.toUpperCase())] = String(v);
  }
  // A dialog opens and closes as in a browser: close fires 'close'.
  showModal() { this.open = true; }
  close() { if (!this.open) return; this.open = false; this.dispatch('close'); }
  getAttribute(k) {
    if (k === 'class') return this.className;
    return this.attrs.has(k) ? this.attrs.get(k) : null;
  }
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

// matches understands only the selectors the tested modules use: comma
// lists, descendant combinators and compounds of tag, .class, #id,
// [attr] or [attr=value] and :checked.
function matches(n, sel) {
  return sel.split(',').some((alt) => {
    const parts = alt.trim().split(/\s+/);
    if (!one(n, parts.at(-1))) return false;
    let i = parts.length - 2;
    for (let p = n.parentNode; p && i >= 0; p = p.parentNode) if (one(p, parts[i])) i--;
    return i < 0;
  });
}

function one(n, sel) {
  if (!n.tagName) return false;
  const re = /([.#]?)([\w-]+)|\[([\w-]+)(?:=([^\]]+))?\]|:([\w-]+)/g;
  for (const m of sel.matchAll(re)) {
    if (m[2] != null) {
      if (m[1] === '.' && !n.className.split(/\s+/).includes(m[2])) return false;
      if (m[1] === '#' && n.getAttribute('id') !== m[2]) return false;
      if (!m[1] && n.tagName !== m[2].toUpperCase()) return false;
    } else if (m[3] != null) {
      const v = n.getAttribute(m[3]);
      if (v == null || (m[4] != null && v !== m[4].replace(/^["']|["']$/g, ''))) return false;
    } else if (m[5] === 'checked' && !n.checked) return false;
  }
  return true;
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
