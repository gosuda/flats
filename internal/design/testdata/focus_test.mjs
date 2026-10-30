// Runs under Node from TestFocusFrameLoading, which puts web/app.js and
// web/wheel.js beside this file. It boots the real canvas app against a small
// fake DOM, host API and event stream on a phone-sized viewport, and checks
// which artboard frames it creates.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

const appSource = fs.readFileSync(new URL('./app.js', import.meta.url), 'utf8');
const wheelSource = fs.readFileSync(new URL('./wheel.js', import.meta.url), 'utf8').replace('export function', 'function');
assert.match(appSource, /^import \{ createWheelPan \} from '\.\/wheel\.js';$/m);
assert.match(appSource, /\nboot\(\);\n?$/);
const source = wheelSource + '\n' + appSource.replace(/^import .*$/m, '').replace(/\nboot\(\);\n?$/, '\nglobalThis.__S = S;\nglobalThis.__boot = boot();\n');

const VIEWPORT = { width: 390, height: 664 };
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// ---- fake DOM ----
class El {
  constructor(tag) {
    this.tagName = tag.toUpperCase(); this.children = []; this.parentNode = null; this.attrs = {};
    this.style = {}; this.listeners = {}; this._text = ''; this.className = '';
    const el = this;
    this.classList = {
      add: (c) => { if (!el.classes().includes(c)) el.className = (el.className + ' ' + c).trim(); },
      remove: (c) => { el.className = el.classes().filter((x) => x !== c).join(' '); },
      toggle: (c, on) => { if (on === undefined) on = !el.classes().includes(c); on ? el.classList.add(c) : el.classList.remove(c); },
      contains: (c) => el.classes().includes(c),
    };
    if (this.tagName === 'IFRAME') this.contentWindow = { received: [], postMessage(m) { this.received.push(m); } };
  }
  classes() { return this.className.split(/\s+/).filter(Boolean); }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  addEventListener(k, f) { (this.listeners[k] ||= []).push(f); }
  dispatch(k, e = {}) { for (const f of this.listeners[k] || []) f({ preventDefault() {}, stopPropagation() {}, ...e }); }
  appendChild(c) { if (c.parentNode) c.remove(); c.parentNode = this; this.children.push(c); return c; }
  append(...cs) { for (const c of cs) this.appendChild(typeof c === 'string' ? new El('#text') : c); }
  replaceChildren(...cs) { for (const c of this.children) c.parentNode = null; this.children = []; this.append(...cs); }
  remove() { if (this.parentNode) { this.parentNode.children = this.parentNode.children.filter((c) => c !== this); this.parentNode = null; } }
  replaceWith(n) { const p = this.parentNode; if (!p) return; const i = p.children.indexOf(this); this.remove(); n.parentNode = p; p.children.splice(i, 0, n); }
  contains(n) { for (; n; n = n.parentNode) if (n === this) return true; return false; }
  set textContent(v) { this._text = String(v); this.children = []; }
  get textContent() { return this._text + this.children.map((c) => c.textContent).join(''); }
  setPointerCapture() {}
  matches(part) { return part.startsWith('.') ? this.classes().includes(part.slice(1)) : this.tagName === part.toUpperCase(); }
  all(out = []) { for (const c of this.children) { out.push(c); c.all(out); } return out; }
  querySelectorAll(sel) {
    const parts = sel.trim().split(/\s+/);
    return this.all().filter((e) => {
      if (!e.matches(parts[parts.length - 1])) return false;
      let i = parts.length - 2;
      for (let a = e.parentNode; a && i >= 0; a = a.parentNode) if (a.matches(parts[i])) i--;
      return i < 0;
    });
  }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  getBoundingClientRect() {
    if (this.classList.contains('viewport')) return { left: 0, top: 0, right: VIEWPORT.width, bottom: VIEWPORT.height, ...VIEWPORT };
    // A board: its frame on the world, through the world's transform.
    const world = this.parentNode;
    const m = /translate\((-?[\d.e+-]+)px, (-?[\d.e+-]+)px\) scale\(([\d.e+-]+)\)/.exec((world && world.style.transform) || '');
    const [tx, ty, z] = m ? [+m[1], +m[2], +m[3]] : [0, 0, 1];
    const left = parseFloat(this.style.left) * z + tx, top = parseFloat(this.style.top) * z + ty;
    const width = parseFloat(this.style.width) * z, height = parseFloat(this.style.height) * z;
    return { left, top, right: left + width, bottom: top + height, width, height };
  }
}

// ---- one app instance against a fake host ----
async function start({ launch = true, hash = '' } = {}) {
  const boards = {}, order = [];
  for (let i = 0; i < 12; i++) {
    const p = `b${String(i).padStart(2, '0')}.html`;
    boards[p] = { x: (i % 3) * 1520, y: Math.floor(i / 3) * 1020, w: 1440, h: 900, title: `Board ${i}`, fill: true };
    order.push(p);
  }
  const host = {
    seq: 1, cap: 'cap1', created: [], streams: [],
    canvas: { title: 'T', boards, order, notes: {}, ...(launch ? { launch: { view: 'focused', board: 'b00.html' } } : {}) },
    files: [...order.map((p) => ({ path: p, hash: 'h0' + p.padEnd(12, '0'), gen: 1 })), { path: 'canvas.json', hash: 'c'.repeat(12), gen: 1 }, { path: 'assets/app.js', hash: 'a'.repeat(12), gen: 1 }],
  };
  const state = () => JSON.parse(JSON.stringify({ slug: 's', access: 'private', seq: host.seq, revision: host.seq, pending: false, visibility: 'private', canvas: host.canvas, files: host.files, artboards: {} }));
  const res = (data) => ({ ok: true, status: 200, statusText: 'OK', json: async () => data });
  const fetch = async (url, init = {}) => {
    const method = init.method || 'GET';
    if (url === '/_design/api/state') return res(state());
    if (url === '/_design/api/session' && method === 'POST') return res({ cap: JSON.parse(init.body || '{}').revision ? 'revcap' : host.cap });
    if (/^\/_design\/api\/revisions\/\d+$/.test(url)) return res({ revision: {}, files: host.files.map((f) => ({ ...f, hash: 'r' + f.hash.slice(1) })) });
    if (url === '/_design/c/revcap/canvas.json') return res(JSON.parse(JSON.stringify(host.canvas)));
    throw new Error('unexpected fetch ' + method + ' ' + url);
  };
  class EventSource {
    constructor() { this.l = {}; host.streams.push(this); setTimeout(() => this.emit('hello', { seq: host.seq }), 0); }
    addEventListener(k, f) { this.l[k] = f; }
    emit(k, data) { this.l[k] && this.l[k]({ data: JSON.stringify(data || {}) }); }
    close() {}
  }
  const document = new El('#document');
  document.body = document.appendChild(new El('body'));
  document.createElement = (t) => { const e = new El(t); if (e.tagName === 'IFRAME') host.created.push(e); return e; };
  document.createElementNS = (_ns, t) => new El(t);
  document.createTextNode = () => new El('#text');
  document.activeElement = null;
  const location = { hash, pathname: '/' };
  const windowListeners = {};
  const ctx = {
    document, location, fetch, EventSource, console, setTimeout, clearTimeout, Promise,
    window: { addEventListener: (k, f) => { (windowListeners[k] ||= []).push(f); } },
    history: { replaceState: (_s, _t, url) => { location.hash = url.startsWith('#') ? url : ''; } },
    sessionStorage: { getItem: () => 'e1', setItem() {} },
    crypto: globalThis.crypto, CSS: { escape: (s) => s }, performance: { now: () => 0 },
  };
  vm.createContext(ctx);
  vm.runInContext(source, ctx);
  await ctx.__boot;
  const S = ctx.__S;
  const app = {
    host, S, document, location, ctx, run: (code) => vm.runInContext(code, ctx),
    // Boards that have a canvas frame.
    loadedBoards: () => [...S.frames].filter(([, f]) => f.iframe).map(([p]) => p),
    focusIframe: () => document.querySelector('.focus iframe'),
    focusPath: () => S.focus && S.focus.path,
    window: (k, e) => { for (const f of windowListeners[k] || []) f(e); },
    // An agent write: new hashes for some files, then a change event.
    write: async (paths) => {
      host.seq++;
      host.files = host.files.map((f) => (paths.includes(f.path) ? { ...f, hash: 'h' + host.seq + f.hash.slice(2), gen: f.gen + 1 } : f));
      host.streams[host.streams.length - 1].emit('change', { seq: host.seq });
      await sleep(150);
    },
    // The event stream reconnects after the host restarted and dropped tokens.
    reconnect: async (cap) => { host.cap = cap; host.streams[host.streams.length - 1].emit('hello', { seq: host.seq }); await sleep(150); },
    button: (label) => document.all().find((e) => e.tagName === 'BUTTON' && e.getAttribute('aria-label') === label),
    message: async (source, data) => { for (const f of windowListeners.message || []) await f({ source, data: { flats: 'design', ...data } }); },
  };
  return app;
}

const boardOf = (app, p) => app.S.frames.get(p);
const errorText = (app, p) => { const f = boardOf(app, p); return f.error && f.error.parentNode ? f.error.textContent : null; };

// ---- defects: no first loads behind the focus view ----
for (const opts of [{ launch: true }, { launch: false, hash: '#b04.html' }]) {
  const app = await start(opts);
  const focused = opts.launch ? 'b00.html' : 'b04.html';
  assert.equal(app.focusPath(), focused, 'opens focused');
  assert.deepEqual(app.loadedBoards(), [], 'a focused launch loads no canvas board');
  assert.equal(app.host.created.length, 1, 'only the focus frame is created');
  assert.match(app.focusIframe().getAttribute('src'), new RegExp(`^/_design/c/cap1/${focused}\\?v=`));

  app.window('resize', {});
  assert.deepEqual(app.loadedBoards(), [], 'resize while focused');

  await app.write(['b01.html']);
  await app.write(['assets/app.js']);
  assert.deepEqual(app.loadedBoards(), [], 'agent writes while focused');

  await app.reconnect('cap2');
  assert.deepEqual(app.loadedBoards(), [], 'capability renewal while focused');
  assert.match(app.focusIframe().getAttribute('src'), /^\/_design\/c\/cap2\//, 'the focus frame takes the new capability');

  app.button('Next artboard').dispatch('click');
  assert.equal(app.focusPath(), opts.launch ? 'b01.html' : 'b05.html');
  app.button('Previous artboard').dispatch('click');
  assert.equal(app.focusPath(), focused);
  await app.message(app.focusIframe().contentWindow, { type: 'navigate', href: 'b07.html' });
  assert.equal(app.focusPath(), 'b07.html', 'prototype link navigation');
  const before = app.focusIframe();
  await app.write(['b07.html']);
  assert.notEqual(app.focusIframe(), before, 'the focus frame reopened after its file changed');
  assert.deepEqual(app.loadedBoards(), [], 'moving between artboards inside focus');

  await app.run('viewRevision(1)');
  assert.equal(app.S.revision, 1);
  assert.deepEqual(app.loadedBoards(), [], 'entering a revision while focused');
  app.run('leaveRevision()');
  assert.equal(app.S.revision, null);
  assert.deepEqual(app.loadedBoards(), [], 'leaving a revision while focused');

  // ---- regression: closing focus loads the boards in view, current ----
  app.button('Close (Esc)').dispatch('click');
  assert.equal(app.S.focus, null);
  const loaded = app.loadedBoards();
  assert.ok(loaded.length > 0, 'closing focus loads the visible boards');
  for (const p of loaded) {
    const src = boardOf(app, p).iframe.getAttribute('src');
    const file = app.host.files.find((f) => f.path === p);
    assert.equal(src, `/_design/c/cap2/${p}?v=${file.hash.slice(0, 12)}g${file.gen}`, 'current bytes and capability');
  }
  assert.equal(app.location.hash, '', 'closing focus clears the hash');
}

// ---- regression: a normal canvas launch loads the boards visible after fit ----
{
  const app = await start({ launch: false });
  assert.equal(app.S.focus, null);
  const vp = { left: -400, top: -400, right: VIEWPORT.width + 400, bottom: VIEWPORT.height + 400 };
  const near = [...app.S.frames].filter(([, f]) => { const r = f.el.getBoundingClientRect(); return r.right > vp.left && r.left < vp.right && r.bottom > vp.top && r.top < vp.bottom; }).map(([p]) => p);
  assert.ok(near.length > 0);
  assert.deepEqual(app.loadedBoards().sort(), near.sort(), 'loads exactly the boards near the fitted viewport');

  // ---- regression: loaded boards get replacements while focused ----
  const loaded = app.loadedBoards();
  app.S.editing = true; // edit mode messages must still reach replaced frames
  app.run("openFocus('b03.html')");
  const old = boardOf(app, 'b02.html').iframe;
  await app.write(['b02.html']);
  const replaced = boardOf(app, 'b02.html').iframe;
  assert.notEqual(replaced, old, 'a loaded board reloads on its file change while focused');
  const before = new Map(loaded.map((p) => [p, boardOf(app, p).iframe]));
  await app.write(['assets/app.js']);
  for (const p of loaded) assert.notEqual(boardOf(app, p).iframe, before.get(p), 'and on a shared asset change');
  await app.reconnect('cap3');
  for (const p of loaded) assert.match(boardOf(app, p).iframe.getAttribute('src'), /^\/_design\/c\/cap3\//, 'and on a capability renewal');
  assert.deepEqual(app.loadedBoards().sort(), loaded.sort(), 'no other board loads');
  const win = boardOf(app, 'b02.html').iframe.contentWindow;
  win.received = [];
  await app.message(win, { type: 'ready' });
  assert.deepEqual(win.received.map((m) => m.type + ':' + (m.mode || '')), ['boards:', 'mode:edit'], 'a replaced frame gets the edit mode');
  app.S.editing = false;

  // ---- regression: deleting the focused board closes focus and loads in view ----
  app.run("closeFocus()");
  app.run("openFocus('b11.html')");
  app.host.canvas.boards = Object.fromEntries(Object.entries(app.host.canvas.boards).filter(([p]) => p !== 'b11.html'));
  app.host.canvas.order = app.host.canvas.order.filter((p) => p !== 'b11.html');
  app.host.files = app.host.files.filter((f) => f.path !== 'b11.html');
  await app.write(['canvas.json']);
  assert.equal(app.S.focus, null, 'focus closes when its board is gone');
  assert.equal(app.document.querySelector('.focus'), null);
}
{
  const app = await start();
  app.host.canvas.boards = Object.fromEntries(Object.entries(app.host.canvas.boards).filter(([p]) => p !== 'b00.html'));
  app.host.canvas.order = app.host.canvas.order.filter((p) => p !== 'b00.html');
  app.host.files = app.host.files.filter((f) => f.path !== 'b00.html');
  await app.write(['canvas.json']);
  assert.equal(app.S.focus, null, 'a focused launch closes when its board is deleted');
  assert.ok(app.loadedBoards().length > 0, 'and the canvas loads the boards in view');
}
{
  // Opening a board that no longer exists (a stale link) returns to the canvas.
  const app = await start();
  app.run("openFocus('gone.html')");
  assert.equal(app.S.focus, null);
  assert.equal(app.document.querySelector('.focus'), null);
  assert.ok(app.loadedBoards().length > 0);
}

// ---- defect: an error reported in focus marks the canvas board ----
{
  const app = await start();
  await app.message(app.focusIframe().contentWindow, { type: 'error', message: 'boom' });
  assert.equal(errorText(app, 'b00.html'), 'Error: boom', 'marks the unloaded board');
  app.run('closeFocus()');
  assert.ok(app.loadedBoards().includes('b00.html'));
  assert.equal(errorText(app, 'b00.html'), 'Error: boom', 'survives the deferred first load');
  await app.reconnect('cap9');
  assert.equal(errorText(app, 'b00.html'), 'Error: boom', 'survives a capability renewal');
  app.run("openFocus('b00.html')");
  app.run('closeFocus()');
  assert.equal(errorText(app, 'b00.html'), 'Error: boom', 'survives reopening focus');
  await app.write(['assets/app.js']);
  assert.equal(errorText(app, 'b00.html'), null, 'a shared asset change clears it');

  app.run("openFocus('b01.html')");
  await app.message(app.focusIframe().contentWindow, { type: 'error', message: 'one' });
  await app.message(app.focusIframe().contentWindow, { type: 'error', message: 'two' });
  assert.equal(errorText(app, 'b01.html'), 'Error: two', 'a new error replaces the marker');
  await app.write(['b01.html']);
  app.run('closeFocus()');
  assert.equal(errorText(app, 'b01.html'), null, 'a change of the board file clears it');

  // A canvas frame's own error still marks its board.
  await app.message(boardOf(app, 'b00.html').iframe.contentWindow, { type: 'error', message: 'own' });
  assert.equal(errorText(app, 'b00.html'), 'Error: own');
}

// ---- replacements touch only boards loaded before focus opened ----
{
  // Close the focused launch at 100% zoom: only the boards near the top-left
  // corner load, so focus reopens with loaded and unloaded boards.
  const app = await start();
  app.run('S.view = { x: 0, y: 0, z: 1 }; applyView(); closeFocus();');
  const loaded = app.loadedBoards().sort();
  assert.deepEqual(loaded, ['b00.html', 'b03.html'], 'a mixed set: two boards loaded, ten not');
  app.run("openFocus('b05.html')");
  const iframes = () => new Map(loaded.map((p) => [p, boardOf(app, p).iframe]));
  let before = iframes();
  await app.write(['b00.html', 'b01.html']);
  assert.notEqual(boardOf(app, 'b00.html').iframe, before.get('b00.html'), 'a loaded board reloads on its file change');
  assert.equal(boardOf(app, 'b03.html').iframe, before.get('b03.html'), 'an unchanged board does not');
  assert.deepEqual(app.loadedBoards().sort(), loaded, 'an unloaded board does not load on its file change');
  before = iframes();
  await app.write(['assets/app.js']);
  for (const p of loaded) assert.notEqual(boardOf(app, p).iframe, before.get(p), 'loaded boards reload on a shared asset change');
  assert.deepEqual(app.loadedBoards().sort(), loaded, 'unloaded boards do not load on a shared asset change');
  await app.reconnect('cap4');
  for (const p of loaded) assert.match(boardOf(app, p).iframe.getAttribute('src'), /^\/_design\/c\/cap4\//, 'loaded boards take the new capability');
  assert.deepEqual(app.loadedBoards().sort(), loaded, 'unloaded boards do not load on a capability renewal');
}

// ---- regression: a malformed fragment still boots the canvas ----
{
  const app = await start({ launch: false, hash: '#%' });
  assert.equal(app.S.focus, null);
  assert.ok(app.loadedBoards().length > 0, 'a malformed fragment does not stop frame loading');
  const launched = await start({ launch: true, hash: '#%E0%A4%A' });
  assert.equal(launched.focusPath(), 'b00.html', 'and the configured launch still applies');
}

// ---- an error keeps the identity of the document that reported it ----
{
  // Live focus, then a revision view opens behind it (a History request that
  // completes after focus opened). The focus frame still shows live content.
  const app = await start();
  const live = app.focusIframe();
  await app.run('viewRevision(1)');
  assert.equal(app.S.revision, 1);
  await app.message(live.contentWindow, { type: 'error', message: 'live error' });
  app.run('closeFocus()');
  assert.ok(app.loadedBoards().includes('b00.html'));
  assert.match(boardOf(app, 'b00.html').iframe.getAttribute('src'), /^\/_design\/c\/revcap\//, 'the canvas shows the revision');
  assert.equal(errorText(app, 'b00.html'), null, 'a live error does not mark revision bytes');
  app.run('leaveRevision()');
  app.run("openFocus('b01.html')");
  app.run('closeFocus()');
  assert.equal(errorText(app, 'b00.html'), null);
}
{
  // The reverse: an error seen while viewing a revision does not mark live bytes.
  const app = await start({ launch: false });
  await app.run('viewRevision(1)');
  app.run("openFocus('b00.html')");
  await app.message(app.focusIframe().contentWindow, { type: 'error', message: 'old error' });
  app.run('closeFocus()');
  assert.equal(errorText(app, 'b00.html'), 'Error: old error', 'marks the revision board it came from');
}

console.log('ok');
