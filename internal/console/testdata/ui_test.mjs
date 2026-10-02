// Renders console pages against a minimal fake DOM and a stubbed fetch.
// Run by TestConsoleUI (console_ui_test.go) next to copies of assets/js.

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
  removeEventListener() {}
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
  querySelectorAll() { return []; }
  get childElementCount() { return this.childNodes.filter((c) => c instanceof Element).length; }
  get lastElementChild() { return [...this.childNodes].reverse().find((c) => c instanceof Element) || null; }
  focus() {}
  select() {}
  showModal() {}
  close(v) { this.returnValue = v; this.dispatch('close'); }
}
globalThis.Node = Node;
globalThis.document = {
  body: new Element('body'),
  visibilityState: 'visible',
  createElement: (t) => new Element(t),
  createElementNS: (_ns, t) => new Element(t),
  createTextNode: (t) => new Text(t),
  addEventListener() {},
  removeEventListener() {},
  getElementById: () => null,
};

// all returns every element under root (inclusive) matching pred.
function all(root, pred, out = []) {
  if (root instanceof Element && pred(root)) out.push(root);
  for (const c of root.childNodes || []) all(c, pred, out);
  return out;
}
const byText = (root, text) => all(root, (e) => e.tagName === 'BUTTON' && e.textContent === text);
const tick = () => new Promise((r) => setTimeout(r, 20));

// --- stubbed API ---
const UNLISTED = 'Unlisted only hides this flat from Portal relay listings. It is NOT access control: anyone with the URL can open it.';
const blog = {
  slug: 'blog', name: 'Blog', visibility: 'public-unlisted', live_version: 2, versions: 2,
  private_url: 'https://blog.tail.ts.net', public_url: 'https://blog.portal.example', public_notice: UNLISTED,
  live: { number: 2, kind: 'server' }, created_at: '2026-10-01T00:00:00Z', updated_at: '2026-10-02T00:00:00Z', disk_bytes: 10,
};
// An older server may omit the notice; the console must still show one.
const shop = { ...blog, slug: 'shop', name: 'Shop', visibility: 'public-listed', public_url: 'https://shop.portal.example', public_notice: undefined };
const notes = { ...blog, slug: 'notes', name: 'Notes', visibility: 'private', public_url: undefined, public_notice: undefined };
const calls = [];
const routes = {
  'GET /console/api/flats': { flats: [blog, shop, notes] },
  'GET /console/api/approvals?status=pending': { approvals: [] },
  'GET /console/api/flats/blog': blog,
  'GET /console/api/flats/blog/versions': { versions: [
    { number: 2, kind: 'server', hash: 'b', size: 1, files: 1, created_at: '2026-10-02T00:00:00Z' },
    { number: 1, kind: 'server', hash: 'a', size: 1, files: 1, created_at: '2026-10-01T00:00:00Z' }] },
  'POST /console/api/flats/blog/deploy': { flat: blog, version: 2, previous: 2, health: { path: '/', status: 200, ok: true } },
};
globalThis.fetch = async (url, opts) => {
  const key = (opts.method || 'GET') + ' ' + url;
  calls.push({ key, body: opts.body });
  const body = routes[key] ?? (key.includes('/logs') ? { events: [] } : key.includes('/stats') ? { page_views: [] } : {});
  return { ok: true, status: 200, json: async () => body };
};

const { publicNoticeOf, PUBLIC_URL_NOTICE, LISTED_NOTICE } = await import('./dom.js');
assert.equal(publicNoticeOf(blog), UNLISTED);
assert.equal(publicNoticeOf(shop), LISTED_NOTICE);
assert.equal(publicNoticeOf({ public_url: 'https://x.example' }), PUBLIC_URL_NOTICE);
assert.equal(publicNoticeOf(notes), '');

// The list shows the notice next to every public URL, and only there.
const ctx = { setTitle() {}, alive: () => true, navigate() {} };
const list = await import('./list.js');
const listMain = new Element('main');
const stopList = list.mount(listMain, [], ctx);
await tick();
const rows = all(listMain, (e) => e.tagName === 'LI' && e.className === 'flat');
assert.equal(rows.length, 3);
for (const [row, want] of [[rows[0], UNLISTED], [rows[1], LISTED_NOTICE]]) {
  assert.ok(row.textContent.includes(want), 'public row lacks its notice: ' + row.textContent);
}
assert.ok(!rows[2].textContent.includes('anyone'), 'private row must not carry a public notice');
stopList();

// The flat page offers a redeploy of the live version (and only of it), also
// from the secrets panel, and the deploy result shows the notice.
const flat = await import('./flat.js');
const flatMain = new Element('main');
const stopFlat = flat.mount(flatMain, ['blog'], ctx);
await tick();
const redeploys = byText(flatMain, 'Redeploy (apply secrets)');
assert.equal(redeploys.length, 2, 'want a redeploy button on the live version row and in the secrets panel');
const liveRow = all(flatMain, (e) => e.tagName === 'TR' && e.className === 'is-live')[0];
assert.ok(liveRow && byText(liveRow, 'Redeploy (apply secrets)').length === 1, 'live version row has no redeploy action');
const secrets = all(flatMain, (e) => e.tagName === 'SECTION' && e.getAttribute('id') === 'secrets')[0];
assert.ok(secrets.textContent.includes('redeploy the live version 2'), 'secrets panel must mention redeploying: ' + secrets.textContent);

redeploys[0].dispatch('click');
await tick();
let dialogs = all(document.body, (e) => e.tagName === 'DIALOG');
assert.equal(dialogs.length, 1);
assert.ok(dialogs[0].textContent.includes('Redeploy version 2?'));
dialogs[0].close('ok');
await tick();
const deploy = calls.find((c) => c.key === 'POST /console/api/flats/blog/deploy');
assert.ok(deploy, 'redeploy must deploy the live version');
assert.deepEqual(JSON.parse(deploy.body), { version: 2 });
dialogs = all(document.body, (e) => e.tagName === 'DIALOG');
assert.equal(dialogs.length, 1);
assert.ok(dialogs[0].textContent.includes('https://blog.portal.example'));
assert.ok(dialogs[0].textContent.includes(UNLISTED), 'deploy result must show the public notice: ' + dialogs[0].textContent);
dialogs[0].close('ok');
await tick();
stopFlat();
console.log('ok');
