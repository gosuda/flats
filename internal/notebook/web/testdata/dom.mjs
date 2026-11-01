// A small DOM for the notebook client's Node tests: elements and text nodes
// with real child lists and textContent, attributes with dataset and
// classList, bubbling events, focus, one Selection, and the selectors the
// modules use. HTML parsing does not exist here: innerHTML, outerHTML and
// insertAdjacentHTML throw and are recorded, so a test fails if any module
// tries to turn text into markup.
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
  get ownerDocument() { return doc; }
  get firstChild() { return this.childNodes[0] || null; }
  get lastChild() { return this.childNodes.at(-1) || null; }
  get nextSibling() { const p = this.parentNode; return p ? p.childNodes[p.childNodes.indexOf(this) + 1] || null : null; }
  get previousSibling() { const p = this.parentNode; return p ? p.childNodes[p.childNodes.indexOf(this) - 1] || null : null; }
  get parentElement() { return this.parentNode && this.parentNode.nodeType === 1 ? this.parentNode : null; }
  get isConnected() { let n = this; while (n.parentNode) n = n.parentNode; return n === doc; }
  contains(n) { for (; n; n = n.parentNode) if (n === this) return true; return false; }
  appendChild(c) { return this.insertBefore(c, null); }
  insertBefore(c, ref) {
    if (c.contains(this)) throw new Error('HierarchyRequestError');
    if (c.parentNode) c.parentNode.removeChild(c);
    const i = ref ? this.childNodes.indexOf(ref) : -1;
    if (ref && i < 0) throw new Error('NotFoundError: reference is not a child');
    if (i < 0) this.childNodes.push(c); else this.childNodes.splice(i, 0, c);
    c.parentNode = this;
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
  replaceChild(n, old) { this.insertBefore(n, old); this.removeChild(old); return old; }
  remove() { if (this.parentNode) this.parentNode.removeChild(this); }
  replaceWith(n) { this.parentNode.replaceChild(n, this); }
  append(...kids) { for (const k of kids) this.appendChild(typeof k === 'string' ? doc.createTextNode(k) : k); }
  prepend(...kids) { const f = this.firstChild; for (const k of kids) this.insertBefore(typeof k === 'string' ? doc.createTextNode(k) : k, f); }
  replaceChildren(...kids) { for (const c of [...this.childNodes]) this.removeChild(c); this.append(...kids); }
  get textContent() { return this.childNodes.map((c) => c.textContent).join(''); }
  set textContent(v) { this.replaceChildren(); if (v != null && v !== '') this.appendChild(doc.createTextNode(String(v))); }
}

class Text extends Node {
  constructor(data) { super(3, '#text'); this.data = String(data); }
  get length() { return this.data.length; }
  get textContent() { return this.data; }
  set textContent(v) { this.data = String(v); }
  get nodeValue() { return this.data; }
}

const kebab = (k) => k.replace(/[A-Z]/g, (c) => '-' + c.toLowerCase());
const camel = (k) => k.replace(/-(.)/g, (_, c) => c.toUpperCase());

class Element extends Node {
  constructor(tag) {
    super(1, tag.toUpperCase());
    this.tagName = tag.toUpperCase();
    this.attrs = new Map();
    this.listeners = new Map();
    this.style = {};
    this.value = '';
    this.checked = false;
    this.dataset = new Proxy({}, {
      get: (_, k) => (typeof k === 'string' ? this.getAttribute('data-' + kebab(k)) ?? undefined : undefined),
      set: (_, k, v) => { this.setAttribute('data-' + kebab(k), v); return true; },
      deleteProperty: (_, k) => { this.removeAttribute('data-' + kebab(k)); return true; },
      has: (_, k) => this.hasAttribute('data-' + kebab(k)),
      ownKeys: () => [...this.attrs.keys()].filter((a) => a.startsWith('data-')).map((a) => camel(a.slice(5))),
      getOwnPropertyDescriptor: (_, k) => (this.hasAttribute('data-' + kebab(k)) ? { enumerable: true, configurable: true, value: this.getAttribute('data-' + kebab(k)) } : undefined),
    });
  }
  get className() { return this.getAttribute('class') || ''; }
  set className(v) { this.setAttribute('class', v); }
  get id() { return this.getAttribute('id') || ''; }
  set id(v) { this.setAttribute('id', v); }
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
    this.listeners.get(type).push({ fn, capture: opts === true || !!(opts && opts.capture) });
  }
  removeEventListener(type, fn) { const l = this.listeners.get(type); if (l) this.listeners.set(type, l.filter((x) => x.fn !== fn)); }
  // Like a browser: blur and focus reach only their target, then focusout
  // and focusin bubble.
  focus() {
    const prev = doc.activeElement;
    if (prev === this) return;
    doc.activeElement = this;
    if (prev && prev !== doc.body) { fireOwn(prev, 'blur', { relatedTarget: this }); fire(prev, 'focusout', { relatedTarget: this }); }
    fireOwn(this, 'focus', { relatedTarget: prev });
    fire(this, 'focusin', { relatedTarget: prev });
  }
  blur() { if (doc.activeElement === this) { doc.activeElement = doc.body; fireOwn(this, 'blur'); fire(this, 'focusout'); } }
  click() { fire(this, 'click'); }
  select() {}
  closest(sel) { for (let n = this; n && n.nodeType === 1; n = n.parentNode) if (matches(n, sel)) return n; return null; }
  matches(sel) { return matches(this, sel); }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  querySelectorAll(sel) {
    const out = [];
    const walk = (n) => { for (const c of n.childNodes) if (c.nodeType === 1) { if (matches(c, sel)) out.push(c); walk(c); } };
    walk(this);
    return out;
  }
  getBoundingClientRect() { return { left: 0, top: 0, right: 600, bottom: 24, width: 600, height: 24 }; }
  scrollIntoView() {}
  get innerHTML() { return this.textContent; }
  set innerHTML(v) { htmlWrites.push(String(v)); throw new Error('innerHTML is not allowed'); }
  set outerHTML(v) { htmlWrites.push(String(v)); throw new Error('outerHTML is not allowed'); }
  insertAdjacentHTML(_, v) { htmlWrites.push(String(v)); throw new Error('insertAdjacentHTML is not allowed'); }
}

// matches understands comma lists, descendant combinators and compounds of
// tag, #id, .class and [attr], [attr=v], [attr="v"], [attr^="v"].
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
  const re = /([.#]?)([\w-]+)|\[([\w-]+)(?:(\^?=)("[^"]*"|'[^']*'|[^\]]+))?\]/g;
  let any = false;
  for (const m of sel.matchAll(re)) {
    any = true;
    if (m[2] != null) {
      if (m[1] === '.' && !n.className.split(/\s+/).includes(m[2])) return false;
      if (m[1] === '#' && n.getAttribute('id') !== m[2]) return false;
      if (!m[1] && n.tagName !== m[2].toUpperCase()) return false;
    } else {
      const v = n.getAttribute(m[3]);
      if (v == null) return false;
      if (m[4]) {
        const want = m[5].replace(/^["']|["']$/g, '');
        if (m[4] === '=' && v !== want) return false;
        if (m[4] === '^=' && !v.startsWith(want)) return false;
      }
    }
  }
  return any;
}

// fireOwn dispatches a non-bubbling event, such as focus or blur, to the
// target's own listeners.
function fireOwn(target, type, props = {}) {
  const e = { type, target, currentTarget: target, bubbles: false, defaultPrevented: false, preventDefault() { this.defaultPrevented = true; }, stopPropagation() {}, ...props };
  for (const { fn } of [...(target.listeners.get(type) || [])]) fn(e);
  return e;
}

// fire dispatches a bubbling event at target and returns it.
export function fire(target, type, props = {}) {
  const e = {
    type, target, bubbles: true, defaultPrevented: false, propagationStopped: false, isComposing: false,
    preventDefault() { this.defaultPrevented = true; },
    stopPropagation() { this.propagationStopped = true; },
    ...props,
  };
  const path = [];
  for (let n = target; n; n = n.parentNode) path.push(n);
  if (target.isConnected) path.push(globalThis.window);
  // Capture listeners on ancestors, then every listener on the target once,
  // then bubbling listeners on ancestors.
  const phases = [[[...path].reverse().slice(0, -1), true], [[target], null], [path.slice(1), false]];
  for (const [nodes, capture] of phases) {
    for (const n of nodes) {
      const l = n.listeners && n.listeners.get(type);
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

class Selection {
  constructor() { this.removeAllRanges(); }
  get rangeCount() { return this.anchorNode ? 1 : 0; }
  get isCollapsed() { return this.anchorNode === this.focusNode && this.anchorOffset === this.focusOffset; }
  setBaseAndExtent(a, ao, f, fo) { this.anchorNode = a; this.anchorOffset = ao; this.focusNode = f; this.focusOffset = fo; }
  collapse(n, o = 0) { this.setBaseAndExtent(n, o, n, o); }
  removeAllRanges() { this.anchorNode = null; this.anchorOffset = 0; this.focusNode = null; this.focusOffset = 0; }
  collapseToEnd() { this.collapse(this.focusNode, this.focusOffset); }
}

class Document extends Node {
  constructor() {
    super(9, '#document');
    this.listeners = new Map();
    this.documentElement = new Element('html');
    this.appendChild(this.documentElement);
    this.head = this.documentElement.appendChild(new Element('head'));
    this.body = this.documentElement.appendChild(new Element('body'));
    this.activeElement = this.body;
  }
  createElement(tag) { return new Element(tag); }
  createElementNS(_ns, tag) { return new Element(tag); }
  createTextNode(t) { return new Text(t); }
  querySelector(sel) { return this.documentElement.matches(sel) ? this.documentElement : this.documentElement.querySelector(sel); }
  querySelectorAll(sel) { return this.documentElement.querySelectorAll(sel); }
  addEventListener(type, fn, opts) { Element.prototype.addEventListener.call(this, type, fn, opts); }
  removeEventListener(type, fn) { Element.prototype.removeEventListener.call(this, type, fn); }
}

// installDOM installs a fresh document, window globals and a location whose
// hash setter fires hashchange like a browser.
export function installDOM() {
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
  const sel = new Selection();
  g.getSelection = () => sel;
  let hash = '';
  g.location = {
    protocol: 'https:', host: 'nb.test', origin: 'https://nb.test', pathname: '/',
    get hash() { return hash; },
    set hash(v) { const next = v.startsWith('#') ? v : '#' + v; if (next === hash) return; hash = next; for (const { fn } of win.listeners.get('hashchange') || []) fn({ type: 'hashchange' }); },
    reload() {},
  };
  g.history = { replaceState: (_s, _t, url) => { hash = url.slice(url.indexOf('#')); } };
  g.requestAnimationFrame = (fn) => { fn(); return 0; };
  g.matchMedia = (q) => ({ matches: /max-width/.test(q) ? !!g.__phone : false, addEventListener() {} });
  g.HTMLScriptElement = class {};
  g.HTMLLinkElement = class {};
  Object.defineProperty(g, 'navigator', { value: { clipboard: { writeText: async (t) => { g.__clipboard = t; } } }, configurable: true });
  return doc;
}

// textNodes lists the text nodes under n, for assertions that user text
// only ever becomes text.
export function textNodes(n) {
  const out = [];
  const walk = (x) => { for (const c of x.childNodes) { if (c.nodeType === 3) out.push(c); else walk(c); } };
  walk(n);
  return out;
}

export function elements(n, tag) {
  const out = [];
  const walk = (x) => { for (const c of x.childNodes) if (c.nodeType === 1) { if (!tag || c.tagName === tag.toUpperCase()) out.push(c); walk(c); } };
  walk(n);
  return out;
}
