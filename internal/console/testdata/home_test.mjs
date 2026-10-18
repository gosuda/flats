// Renders console pages against a minimal fake DOM and a stubbed fetch.
// Run by TestConsoleHomeFeedback next to copies of assets/js.

import assert from 'node:assert/strict';

// --- fake DOM ---
class Node {
  constructor() { this.childNodes = []; this.parentNode = null; }
  appendChild(c) {
    if (c.parentNode) c.parentNode.removeChild(c);
    c.parentNode = this;
    this.childNodes.push(c);
    return c;
  }
  removeChild(c) {
    const i = this.childNodes.indexOf(c);
    if (i >= 0) this.childNodes.splice(i, 1);
    c.parentNode = null;
    return c;
  }
  get firstChild() { return this.childNodes[0] || null; }
}
class Text extends Node {
  constructor(t) { super(); this.data = t; }
  get textContent() { return this.data; }
}
class Element extends Node {
  constructor(tag) {
    super();
    this.tagName = tag.toUpperCase();
    this.attributes = {};
    this.listeners = {};
    this.style = { setProperty() {} };
    this.classList = { add() {}, remove() {}, contains() { return false; } };
    this.value = '';
  }
  get textContent() { return this.childNodes.map((c) => c.textContent).join(''); }
  set textContent(t) {
    this.childNodes = [];
    if (t !== '' && t != null) this.appendChild(new Text(String(t)));
  }
  get className() { return this.attributes.class || ''; }
  set className(v) { this.attributes.class = v; }
  setAttribute(k, v) {
    this.attributes[k] = String(v);
    if (k === 'value') this.value = String(v);
    if (k === 'checked') this.checked = true;
  }
  getAttribute(k) { return k in this.attributes ? this.attributes[k] : null; }
  removeAttribute(k) { delete this.attributes[k]; }
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  removeEventListener(type, fn) { this.listeners[type] = (this.listeners[type] || []).filter((f) => f !== fn); }
  dispatch(type) { for (const fn of this.listeners[type] || []) fn({ type, preventDefault() {}, target: this }); }
  append(...cs) { for (const c of cs) this.appendChild(typeof c === 'string' ? new Text(c) : c); }
  prepend(c) { this.appendChild(c); this.childNodes.unshift(this.childNodes.pop()); }
  replaceWith(n) {
    const p = this.parentNode;
    const i = p.childNodes.indexOf(this);
    p.childNodes[i] = n;
    n.parentNode = p;
    this.parentNode = null;
  }
  remove() { if (this.parentNode) this.parentNode.removeChild(this); }
  contains(n) { for (let x = n; x; x = x.parentNode) if (x === this) return true; return false; }
  querySelector() { return null; }
  querySelectorAll(tag) { return all(this, (e) => e.tagName === tag.toUpperCase()); }
  get childElementCount() { return this.childNodes.filter((c) => c instanceof Element).length; }
  get lastElementChild() { return [...this.childNodes].reverse().find((c) => c instanceof Element) || null; }
  focus() { document.activeElement = this; }
  select() {}
  showModal() { this.open = true; }
  close(v) { this.open = false; this.returnValue = v; this.dispatch('close'); }
}
globalThis.Node = Node;
const documentListeners = new Map();
globalThis.document = {
  body: new Element('body'),
  visibilityState: 'visible',
  createElement: (t) => new Element(t),
  createElementNS: (_ns, t) => new Element(t),
  createTextNode: (t) => new Text(t),
  addEventListener(type, fn) { documentListeners.set(type, fn); },
  removeEventListener(type, fn) { if (documentListeners.get(type) === fn) documentListeners.delete(type); },
  getElementById: () => null,
};

// all returns every element under root (inclusive) matching pred.
function all(root, pred, out = []) {
  if (root instanceof Element && pred(root)) out.push(root);
  for (const c of root.childNodes || []) all(c, pred, out);
  return out;
}
const byText = (root, text) => all(root, (e) => e.tagName === 'BUTTON' && e.textContent === text);
const tick = async () => { for (let i = 0; i < 12; i++) await Promise.resolve(); };

// A deterministic clock exercises both the request deadline and next poll.
let now = 0;
let timerID = 0;
const timers = new Map();
globalThis.setTimeout = (fn, delay) => { const id = ++timerID; timers.set(id, { fn, at: now + delay }); return id; };
globalThis.clearTimeout = (id) => timers.delete(id);
async function advance(ms) {
  now += ms;
  for (const [id, t] of [...timers]) if (t.at <= now && timers.delete(id)) t.fn();
  await tick();
}
const defer = () => { let resolve; const promise = new Promise((r) => { resolve = r; }); return { promise, resolve }; };
let calls = [];
let approvalReply = { approvals: null };
let flatsReply = { flats: [] };
let approvalWait;
let flatsWait;
let approvalFailure = false;
globalThis.fetch = async (url, opts) => {
  calls.push({ url, signal: opts.signal });
  if (opts.method === 'POST') return { ok: true, json: async () => ({ status: 'approved' }) };
  const approvals = url.includes('/approvals');
  const wait = approvals ? approvalWait : flatsWait;
  if (wait) {
    await Promise.race([wait.promise, new Promise((_, reject) => opts.signal?.addEventListener('abort', () => reject(new Error('aborted')), { once: true }))]);
  }
  if (approvals && approvalFailure) throw new Error('private server detail');
  return { ok: true, json: async () => approvals ? approvalReply : flatsReply };
};
const list = await import('./list.js');
const { relTime } = await import('./dom.js');
const ctx = { setTitle() {}, alive: () => true, navigate() {} };
const start = () => { const main = new Element('main'); const stop = list.mount(main, [], ctx); return { main, stop }; };
const visible = (state) => { document.visibilityState = state; documentListeners.get('visibilitychange')?.(); };
const pending = (id = 'request-one') => ({ id, flat: 'example', action: 'delete', via: 'mcp', requested_at: '2026-01-01T00:00:00Z' });
const countCalls = (part) => calls.filter((c) => c.url.includes(part)).length;
const approvalLink = (main) => all(main, (e) => e.getAttribute('href')?.startsWith('/approvals/'))[0];
const retryButton = (main) => byText(main, 'Retry')[0] || byText(main, 'Check again')[0];
const feedback = (main) => retryButton(main).parentNode;
const liveRegion = (main) => all(main, (e) => e.getAttribute('role') === 'status')[0];

// Flats load even while approvals are stalled. Timeout shows a generic error.
flatsReply = { flats: [{ slug: 'example', name: 'Example', visibility: 'private', latest_version: 0 }] };
approvalWait = defer();
let page = start();
await tick();
assert.ok(page.main.textContent.includes('Example'));
assert.equal(countCalls('/approvals'), 1);
await advance(15000);
assert.equal(calls[0].url.includes('/flats') || calls[1].url.includes('/flats'), true);
assert.equal(calls.find((c) => c.url.includes('/approvals')).signal.aborted, true);
assert.equal(feedback(page.main).hidden, false);
assert.ok(page.main.textContent.includes('Cannot check approval requests.'));
assert.ok(!page.main.textContent.includes('private server detail'));
assert.equal(liveRegion(page.main).textContent, 'Cannot check approval requests. Try again.');

// Retry preserves its node/focus and callers join one pending request.
approvalWait = defer();
const retry = retryButton(page.main);
retry.focus();
retry.dispatch('click');
retry.dispatch('click');
await tick();
assert.equal(countCalls('/approvals'), 2);
assert.equal(retry.getAttribute('aria-disabled'), 'true');
assert.ok(page.main.textContent.includes('Checking approval requests…'));
assert.equal(retry.disabled, undefined);
visible('hidden'); visible('visible');
assert.equal(countCalls('/approvals'), 2);
approvalWait.resolve();
await tick();
approvalWait = null;
assert.equal(document.activeElement, retry);
assert.equal(retryButton(page.main), retry);
assert.equal(retry.textContent, 'Check again');
assert.equal(liveRegion(page.main).textContent, '0 approval requests pending. Approval requests are up to date.');
assert.equal(retry.getAttribute('aria-disabled'), 'false');
assert.ok(page.main.textContent.includes('Approval requests are up to date.'));
assert.equal(timers.size, 1);

// Poll only approvals; changing banner does not redraw search or flat rows.
const search = all(page.main, (e) => e.getAttribute('type') === 'search')[0];
search.value = 'example'; search.focus(); search.dispatch('input');
const row = all(page.main, (e) => e.className === 'flat')[0];
let flatsCalls = countCalls('/flats');
approvalReply = { approvals: [pending()] };
await advance(15000);
assert.equal(countCalls('/flats'), flatsCalls);
assert.equal(search.value, 'example');
assert.equal(document.activeElement, search);
assert.equal(all(page.main, (e) => e.className === 'flat')[0], row);
const link = approvalLink(page.main);
assert.ok(link);
assert.equal(liveRegion(page.main).textContent, '1 approval request pending.');
link.focus();
const time = all(page.main, (e) => e.tagName === 'TIME')[0];
time.textContent = 'outdated relative time';
await advance(15000);
assert.equal(approvalLink(page.main), link);
assert.equal(document.activeElement, link);
assert.equal(time.textContent, relTime(time.getAttribute('datetime')));

// Failure retains stale links; repeated failures neither replace nor stack.
approvalFailure = true;
await advance(15000);
assert.equal(approvalLink(page.main), link);
assert.ok(page.main.textContent.includes('last successful check'));
const announcement = liveRegion(page.main).textContent;
await advance(15000);
assert.equal(liveRegion(page.main).textContent, announcement);
assert.equal(byText(page.main, 'Retry').length, 1);
approvalFailure = false;
approvalReply = { approvals: null };
await advance(15000);
assert.equal(approvalLink(page.main), undefined);
assert.ok(!page.main.textContent.includes('last successful check'));

// Hidden pages do not poll; returning visible refreshes flats and approvals.
visible('hidden');
let requestCount = calls.length;
await advance(60000);
assert.equal(calls.length, requestCount);
visible('visible');
await tick();
assert.equal(calls.length, requestCount + 2);

// A completed Save action queues a fresh read behind an in-flight poll.
flatsReply.flats[0].draft = { dirty: true, revision: 1, hash: 'synthetic' };
visible('visible'); await tick();
approvalWait = defer();
await advance(15000);
let beforeAction = countCalls('/approvals');
byText(page.main, 'Save as v1')[0].dispatch('click');
all(document.body, (e) => e.tagName === 'DIALOG')[0].close('ok');
await tick();
assert.equal(countCalls('/approvals'), beforeAction);
const oldPoll = approvalWait;
approvalWait = null;
oldPoll.resolve(); await tick();
assert.equal(countCalls('/approvals'), beforeAction + 1, 'post-action trailing read');

// Unmount aborts in-flight work, removes listener/timers and ignores responses.
approvalWait = defer();
await advance(15000);
const lastSignal = calls.at(-1).signal;
const beforeStop = page.main.textContent;
page.stop();
assert.equal(lastSignal.aborted, true);
await tick();
assert.equal(timers.size, 0);
assert.equal(documentListeners.size, 0);
approvalWait.resolve(); await tick();
assert.equal(page.main.textContent, beforeStop);
approvalWait = null;

// Invalid shape isn't a valid empty result; all content examples are present.
flatsReply = { flats: [] };
for (const response of [{}, { approvals: false }, { approvals: 'broken' }]) {
  approvalReply = response;
  page = start(); await tick();
  assert.equal(feedback(page.main).hidden, false);
  page.stop(); await tick();
}
approvalReply = { approvals: [] };
visible('hidden');
page = start(); await tick();
assert.equal(timers.size, 0);
assert.equal(feedback(page.main).hidden, undefined); // hidden attribute remains set
assert.equal(feedback(page.main).getAttribute('hidden'), '');
for (const example of ['Website:', 'Document:', 'Page:', 'Design:']) assert.ok(page.main.textContent.includes(example));
assert.equal(all(page.main, (e) => e.textContent === 'Connect an agent' && e.tagName === 'A')[0].getAttribute('href'), '/settings/agents');
page.stop(); await tick();
assert.equal(timers.size, 0);
console.log('home feedback ok');
