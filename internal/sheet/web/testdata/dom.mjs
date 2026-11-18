// A small DOM for the sheet client's Node tests: elements and text nodes
// with real child lists and textContent, attributes with dataset and
// classList, text fields with a value and a caret, bubbling events, focus,
// and the selectors the modules use. HTML parsing does not exist here:
// innerHTML, outerHTML and insertAdjacentHTML throw and are recorded, so a
// test fails if any module tries to turn cell text into markup.
'use strict';

export const htmlWrites = [];

let doc = null;

class Node {
  constructor(nodeType, nodeName) {
    this.nodeType = nodeType;
    this.nodeName = nodeName;
    this.parentNode = null;
    this.childNodes = [];
  }
  get firstChild() { return this.childNodes[0] || null; }
  get lastChild() { return this.childNodes.at(-1) || null; }
  get nextSibling() { const p = this.parentNode; return p ? p.childNodes[p.childNodes.indexOf(this) + 1] || null : null; }
  get isConnected() { let n = this; while (n.parentNode) n = n.parentNode; return n === doc; }
  contains(n) { for (; n; n = n.parentNode) if (n === this) return true; return false; }
  appendChild(c) { return this.insertBefore(c, null); }
  insertBefore(c, ref) {
    if (c.contains(this)) throw new Error('HierarchyRequestError');
    const active = doc && doc.activeElement;
    const hadFocus = active && c.contains(active);
    if (c.parentNode) c.parentNode.childNodes.splice(c.parentNode.childNodes.indexOf(c), 1);
    const i = ref ? this.childNodes.indexOf(ref) : -1;
    if (ref && i < 0) throw new Error('NotFoundError: reference is not a child');
    if (i < 0) this.childNodes.push(c); else this.childNodes.splice(i, 0, c);
    c.parentNode = this;
    // Like a browser: moving an element that holds focus blurs it.
    if (hadFocus) doc.activeElement = doc.body;
    return c;
  }
  removeChild(c) {
    const i = this.childNodes.indexOf(c);
    if (i < 0) throw new Error('NotFoundError: not a child');
    this.childNodes.splice(i, 1);
    c.parentNode = null;
    if (doc && doc.activeElement && c.contains(doc.activeElement)) doc.activeElement = doc.body;
    return c;
  }
  remove() { if (this.parentNode) this.parentNode.removeChild(this); }
  append(...kids) { for (const k of kids) this.appendChild(typeof k === 'string' ? doc.createTextNode(k) : k); }
  replaceChildren(...kids) { for (const c of [...this.childNodes]) this.removeChild(c); this.append(...kids); }
  get textContent() { return this.childNodes.map((c) => c.textContent).join(''); }
  set textContent(v) { this.replaceChildren(); if (v != null && v !== '') this.appendChild(doc.createTextNode(String(v))); }
}

class Text extends Node {
  constructor(data) { super(3, '#text'); this.data = String(data); }
  get textContent() { return this.data; }
  set textContent(v) { this.data = String(v); }
}

const kebab = (k) => k.replace(/[A-Z]/g, (c) => `-${c.toLowerCase()}`);

class Element extends Node {
  constructor(tag) {
    super(1, tag.toUpperCase());
    this.tagName = tag.toUpperCase();
    this.attrs = new Map();
    this.listeners = new Map();
    this.style = { setProperty(k, v) { this[k] = v; } };
    this.value = '';
    this.checked = false;
    this.readOnly = false;
    this.selectionStart = 0;
    this.selectionEnd = 0;
    this.scrollTop = 0;
    this.scrollLeft = 0;
    this.clientWidth = 0;
    this.clientHeight = 0;
    this.offsetWidth = 0;
    this.offsetHeight = 0;
    this.rect = null; // tests set a viewport rectangle where geometry matters
    this.dataset = new Proxy({}, {
      get: (_, k) => (typeof k === 'string' ? this.getAttribute(`data-${kebab(k)}`) ?? undefined : undefined),
      set: (_, k, v) => { this.setAttribute(`data-${kebab(k)}`, v); return true; },
    });
  }
  get className() { return this.getAttribute('class') || ''; }
  set className(v) { this.setAttribute('class', v); }
  get id() { return this.getAttribute('id') || ''; }
  get hidden() { return this.hasAttribute('hidden'); }
  set hidden(v) { if (v) this.setAttribute('hidden', ''); else this.removeAttribute('hidden'); }
  get disabled() { return this.hasAttribute('disabled'); }
  set disabled(v) { if (v) this.setAttribute('disabled', ''); else this.removeAttribute('disabled'); }
  get children() { return this.childNodes.filter((c) => c.nodeType === 1); }
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
  setAttribute(k, v) { this.attrs.set(k, String(v)); if (k === 'value') this.value = String(v); }
  getAttribute(k) { return this.attrs.has(k) ? this.attrs.get(k) : null; }
  hasAttribute(k) { return this.attrs.has(k); }
  removeAttribute(k) { this.attrs.delete(k); }
  addEventListener(type, fn, opts) {
    if (!this.listeners.has(type)) this.listeners.set(type, []);
    this.listeners.get(type).push({ fn, capture: opts === true || !!opts?.capture });
  }
  removeEventListener(type, fn) { const l = this.listeners.get(type); if (l) this.listeners.set(type, l.filter((x) => x.fn !== fn)); }
  focus() {
    const prev = doc.activeElement;
    if (prev === this) return;
    doc.activeElement = this;
    if (prev && prev !== doc.body) fireOwn(prev, 'blur', { relatedTarget: this });
    fireOwn(this, 'focus', { relatedTarget: prev });
  }
  blur() { if (doc.activeElement === this) { doc.activeElement = doc.body; fireOwn(this, 'blur'); } }
  click() { if (!this.disabled) fire(this, 'click'); }
  select() { this.selectionStart = 0; this.selectionEnd = this.value.length; }
  setSelectionRange(a, b) { this.selectionStart = a; this.selectionEnd = b; }
  setRangeText(text, start, end, mode) {
    this.value = this.value.slice(0, start) + text + this.value.slice(end);
    if (mode === 'end') this.selectionStart = this.selectionEnd = start + text.length;
  }
  closest(sel) { for (let n = this; n && n.nodeType === 1; n = n.parentNode) if (matches(n, sel)) return n; return null; }
  matches(sel) { return matches(this, sel); }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  querySelectorAll(sel) {
    const out = [];
    const walk = (n) => { for (const c of n.childNodes) if (c.nodeType === 1) { if (matches(c, sel)) out.push(c); walk(c); } };
    walk(this);
    return out;
  }
  getBoundingClientRect() {
    const r = this.rect || { left: 0, top: 0, width: this.offsetWidth, height: this.offsetHeight };
    return { ...r, right: r.left + r.width, bottom: r.top + r.height };
  }
  set innerHTML(v) { htmlWrites.push(String(v)); throw new Error('innerHTML is not allowed'); }
  set outerHTML(v) { htmlWrites.push(String(v)); throw new Error('outerHTML is not allowed'); }
  insertAdjacentHTML(_, v) { htmlWrites.push(String(v)); throw new Error('insertAdjacentHTML is not allowed'); }
}

// matches understands comma lists, descendant combinators and compounds of
// tag, #id, .class, [attr], [attr="v"] and :not([attr]).
export function matches(n, sel) {
  return sel.split(',').some((alt) => {
    const parts = alt.trim().split(/\s+/);
    if (!one(n, parts.at(-1))) return false;
    let i = parts.length - 2;
    for (let p = n.parentNode; p && p.nodeType === 1 && i >= 0; p = p.parentNode) if (one(p, parts[i])) i--;
    return i < 0;
  });
}

function one(n, sel) {
  const re = /:not\(\[([\w-]+)\]\)|([.#]?)([\w-]+)|\[([\w-]+)(?:=("[^"]*"|[^\]]+))?\]/g;
  let any = false;
  for (const m of sel.matchAll(re)) {
    any = true;
    if (m[1] != null) {
      if (n.hasAttribute(m[1])) return false;
    } else if (m[3] != null) {
      if (m[2] === '.' && !n.className.split(/\s+/).includes(m[3])) return false;
      if (m[2] === '#' && n.getAttribute('id') !== m[3]) return false;
      if (!m[2] && n.tagName !== m[3].toUpperCase()) return false;
    } else {
      const v = n.getAttribute(m[4]);
      if (v == null) return false;
      if (m[5] != null && v !== m[5].replace(/^"|"$/g, '')) return false;
    }
  }
  return any;
}

function fireOwn(target, type, props = {}) {
  const e = { type, target, currentTarget: target, preventDefault() {}, stopPropagation() {}, ...props };
  for (const { fn } of [...(target.listeners.get(type) || [])]) fn(e);
}

// fire dispatches a bubbling event at target and returns it: capture
// listeners from the document down, then the target, then bubbling.
export function fire(target, type, props = {}) {
  const e = {
    type, target, defaultPrevented: false, propagationStopped: false, isComposing: false, key: '', button: 0,
    preventDefault() { this.defaultPrevented = true; },
    stopPropagation() { this.propagationStopped = true; },
    ...props,
  };
  const path = [];
  for (let n = target; n; n = n.parentNode) path.push(n);
  if (target.isConnected) path.push(globalThis.window);
  const phases = [[[...path].reverse().slice(0, -1), true], [[target], null], [path.slice(1), false]];
  for (const [nodes, capture] of phases) {
    for (const n of nodes) {
      const l = n.listeners?.get(type);
      if (!l) continue;
      for (const { fn, capture: c } of [...l]) {
        if (capture !== null && c !== capture) continue;
        e.currentTarget = n;
        fn(e);
      }
      if (e.propagationStopped) return e;
    }
  }
  return e;
}

class Document extends Node {
  constructor() {
    super(9, '#document');
    this.listeners = new Map();
    this.documentElement = new Element('html');
    this.appendChild(this.documentElement);
    this.body = this.documentElement.appendChild(new Element('body'));
    this.activeElement = this.body;
  }
  createElement(tag) { return new Element(tag); }
  createElementNS(_ns, tag) { return new Element(tag); }
  createTextNode(t) { return new Text(t); }
  querySelector(sel) { return this.documentElement.querySelector(sel); }
  querySelectorAll(sel) { return this.documentElement.querySelectorAll(sel); }
  addEventListener(type, fn, opts) { Element.prototype.addEventListener.call(this, type, fn, opts); }
  removeEventListener(type, fn) { Element.prototype.removeEventListener.call(this, type, fn); }
}

// installDOM installs a fresh document with the sheet's shell and the
// globals the client reads.
export function installDOM({ hash = '' } = {}) {
  doc = new Document();
  htmlWrites.length = 0;
  const g = globalThis;
  const win = { listeners: new Map() };
  win.addEventListener = (t, fn, o) => Element.prototype.addEventListener.call(win, t, fn, o);
  win.removeEventListener = (t, fn) => Element.prototype.removeEventListener.call(win, t, fn);
  g.window = win;
  g.document = doc;
  g.addEventListener = win.addEventListener;
  g.removeEventListener = win.removeEventListener;
  const loc = { protocol: 'https:', host: 'budget.test', hostname: 'budget.test', origin: 'https://budget.test', pathname: '/', hash, reloads: 0, reload() { this.reloads++; } };
  g.location = loc;
  g.history = { replaceState: (_s, _t, url) => { loc.hash = url.slice(url.indexOf('#')); } };
  g.requestAnimationFrame = (fn) => { fn(); return 0; };
  g.matchMedia = (q) => ({ matches: /max-width/.test(q) ? !!g.__phone : false, addEventListener() {} });
  g.HTMLScriptElement = class {};
  g.HTMLLinkElement = class {};
  const store = new Map();
  g.localStorage = { getItem: (k) => (store.has(k) ? store.get(k) : null), setItem: (k, v) => store.set(k, String(v)) };
  g.__clipboard = null;
  Object.defineProperty(g, 'navigator', { value: { clipboard: { writeText: async (t) => { g.__clipboard = t; }, readText: async () => g.__clipboard ?? '' } }, configurable: true });
  for (const [tag, attrs] of [['div', { id: 'app', class: 'app' }], ['div', { id: 'announce' }], ['div', { id: 'announce-cell' }]]) {
    const el = doc.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, v);
    doc.body.appendChild(el);
  }
  return doc;
}

// textNodes lists the text nodes under n, for assertions that cell text
// only ever becomes text.
export function textNodes(n) {
  const out = [];
  const walk = (x) => { for (const c of x.childNodes) { if (c.nodeType === 3) out.push(c); else walk(c); } };
  walk(n);
  return out;
}
