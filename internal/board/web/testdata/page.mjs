// page.mjs starts the real board app under Node: it builds the body of
// index.html with dom.mjs, stubs the browser APIs the app touches (canvas,
// storage, WebSocket, media queries) and gives the test the socket so it can
// play the server.

import { readFileSync } from 'node:fs';
import { installDOM } from './dom.mjs';

// buildBody adds the elements of index.html's body: tags with their
// attributes and text, no scripts.
function buildBody(doc) {
  const html = readFileSync(new URL('../index.html', import.meta.url), 'utf8');
  const body = html.slice(html.indexOf('<body>') + 6, html.indexOf('</body>'));
  const voids = new Set(['input', 'br', 'img']);
  const stack = [doc.body];
  const re = /<(\/?)([a-z][\w-]*)([^>]*)>|([^<]+)/g;
  for (const m of body.matchAll(re)) {
    const top = stack.at(-1);
    if (m[4] != null) {
      if (m[4].trim()) top.textContent += m[4].trim();
      continue;
    }
    if (m[1]) { stack.pop(); continue; }
    const el = doc.createElement(m[2]);
    for (const a of m[3].matchAll(/([\w-]+)(?:="([^"]*)")?/g)) {
      el.setAttribute(a[1], a[2] ?? '');
      if (a[1] === 'hidden') el.hidden = true;
      if (a[1] === 'disabled') el.disabled = true;
    }
    top.appendChild(el);
    if (!voids.has(m[2])) stack.push(el);
  }
}

function storage() {
  const m = new Map();
  return {
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => m.set(k, String(v)),
    removeItem: (k) => m.delete(k),
    clear: () => m.clear(),
  };
}

// FakeSocket records what the app sends; the test delivers server frames.
export class FakeSocket {
  static all = [];
  constructor(url) {
    this.url = url;
    this.sent = [];
    this.closed = false;
    FakeSocket.all.push(this);
  }
  send(s) { this.sent.push(JSON.parse(s)); }
  close() { this.closed = true; }
  // open runs the app's onopen, which sends hello; it returns that hello.
  open() { this.onopen(); return this.sent.at(-1); }
  recv(m) { this.onmessage({ data: JSON.stringify(m) }); }
  drop() { this.closed = true; this.onclose && this.onclose(); }
}

const ctx2d = new Proxy({}, {
  get: (t, k) => (k in t ? t[k] : () => ({ width: 0 })),
  set: (t, k, v) => { t[k] = v; return true; },
});

// startApp installs a fresh page and returns { app, doc, socket(),
// downloads }, where socket() is the app's latest WebSocket and downloads
// the blobs the app offered as files. Timers run on t.mock.timers when
// the caller enabled them.
export async function startApp({ local = {}, session = {} } = {}) {
  const doc = installDOM();
  buildBody(doc);
  const g = globalThis;
  g.localStorage = storage();
  g.sessionStorage = storage();
  for (const [k, v] of Object.entries(local)) g.localStorage.setItem(k, v);
  for (const [k, v] of Object.entries(session)) g.sessionStorage.setItem(k, v);
  g.window = g;
  g.devicePixelRatio = 1;
  g.location = { protocol: 'https:', host: 'board.test', pathname: '/', reload() {} };
  g.addEventListener = () => {};
  g.matchMedia = () => ({ matches: false, addEventListener() {} });
  g.requestAnimationFrame = () => 0;
  g.cancelAnimationFrame = () => {};
  g.ResizeObserver = class { observe() {} disconnect() {} };
  g.WebSocket = FakeSocket;
  const downloads = [];
  g.URL.createObjectURL = (blob) => { downloads.push(blob); return 'blob:board.test/' + downloads.length; };
  g.URL.revokeObjectURL = () => {};
  g.CSS = { escape: (s) => String(s).replace(/["\\]/g, '\\$&') };
  Object.defineProperty(g, 'navigator', { value: { maxTouchPoints: 0, userAgent: 'node' }, configurable: true });
  doc.createElement = ((make) => (tag) => {
    const el = make(tag);
    if (tag === 'canvas') el.getContext = () => ctx2d;
    return el;
  })(doc.createElement);
  for (const c of doc.querySelectorAll('canvas')) c.getContext = () => ctx2d;
  g.boardTest = true;
  FakeSocket.all = [];
  const { App } = await import('../app.js');
  const app = new App();
  return { app, doc, socket: () => FakeSocket.all.at(-1), downloads };
}
