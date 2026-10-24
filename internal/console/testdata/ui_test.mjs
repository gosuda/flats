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
  querySelectorAll(selector) {
    if (selector === '[data-dirty="true"]') return all(this, (e) => e.getAttribute('data-dirty') === 'true');
    if (selector === '.savebar') return all(this, (e) => e.className === 'savebar');
    return [];
  }
  get childElementCount() { return this.childNodes.filter((c) => c instanceof Element).length; }
  get lastElementChild() { return [...this.childNodes].reverse().find((c) => c instanceof Element) || null; }
  focus() { document.activeElement = this; this.focusedWhileDisabled = !!this.disabled; this.focusCount = (this.focusCount || 0) + 1; }
  select() {}
  showModal() { this.open = true; }
  close(v) { this.open = false; this.returnValue = v; this.dispatch('close'); }
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
const radios = (root) => all(root, (e) => e.tagName === 'INPUT' && e.getAttribute('type') === 'radio');
// pick chooses a link access radio, as a click on it would.
const pick = (root, value) => {
  const r = radios(root).find((e) => e.value === value);
  for (const o of radios(root)) o.checked = o === r;
  r.dispatch('change');
};
const tick = () => new Promise((r) => setTimeout(r, 20));

// --- stubbed API ---
const PUBLIC = 'This flat is public: anyone on the internet can open it. A domain or URL is not what makes it public.';
const blog = {
  version_quota: null, effective_version_quota: 20, version_count: 2, type: 'flat', slug: 'blog', name: 'Blog', visibility: 'public', live_version: 2, versions: 2, latest_version: 2, latest_deployed: true, providers: ['portal'],
  portal_listing: 'default', portal_hidden: false,
  private_url: 'https://blog.tail.ts.net', public_url: 'https://blog.portal.example', public_notice: PUBLIC,
  draft: { revision: 3, hash: 'c', dirty: true, size: 1, files: 1, kind: 'server', updated_at: '2026-10-03T00:00:00Z' },
  live: { number: 2, kind: 'server' }, created_at: '2026-10-01T00:00:00Z', updated_at: '2026-10-02T00:00:00Z', disk_bytes: 10,
};
// An older server may omit the notice or report a legacy public value, and a
// Funnel address comes from the flat's endpoints; the console still shows the notice.
const shop = { ...blog, type: 'flat', slug: 'shop', name: 'Shop', visibility: 'public-listed', public_url: undefined, public_notice: undefined,
  endpoints: [{ provider: 'tailscale-funnel', audience: 'current', url: 'https://shop.tail.ts.net', state: 'ready' }] };
// A document runs once saved: it has no versions, Draft or publishing.
const notes = { ...blog, type: 'docs', running: true, slug: 'notes', name: 'Notes', visibility: 'private', public_url: undefined, public_notice: undefined, draft: null, versions: 0, providers: [] };
// A page has the website lifecycle but is always static: no database,
// environment, secrets or server permissions.
const report = { ...blog, type: 'page', slug: 'report', name: 'Report', visibility: 'private', public_url: undefined, public_notice: undefined, providers: [],
  draft: { revision: 2, hash: 'p', dirty: false, size: 1, files: 1, kind: 'static', updated_at: '2026-10-03T00:00:00Z' }, live: { number: 1, kind: 'static' }, live_version: 1, versions: 1, latest_version: 1 };
const pending = (id) => ({ status: 'pending_approval', approval: { id, status: 'pending' } });
const calls = [];
const routes = {
  'GET /console/api/flats': { flats: [blog, shop, notes, report] },
  'GET /console/api/approvals?status=pending': { approvals: [] },
  'GET /console/api/flats/blog': blog,
  'GET /console/api/flats/notes': notes,
  'GET /console/api/flats/report': report,
  'GET /console/api/flats/report/versions': { versions: [{ number: 1, kind: 'static', hash: 'p', size: 1, files: 1, created_at: '2026-10-02T00:00:00Z' }] },
  'GET /console/api/flats/blog/network': { origins: ['https://api.example.com'] },
  'PUT /console/api/flats/blog/network': { origins: ['https://api.example.com'] },
  'PUT /console/api/flats/blog/listing': { ...blog, portal_listing: 'hidden', portal_hidden: true },
  'GET /console/api/flats/blog/env': { env: [{ name: 'APP_MODE', value: '<script>demo</script>\nsecond line', updated_at: '2026-10-03T00:00:00Z' }, { name: 'EMPTY', value: '', updated_at: '2026-10-03T00:00:00Z' }] },
  'GET /console/api/flats/blog/secrets': { secrets: [{ name: 'API_KEY', updated_at: '2026-10-03T00:00:00Z', value: 'SECRET_MUST_NEVER_RENDER' }] },
  'GET /console/api/flats/blog/versions': { versions: [
    { number: 2, kind: 'server', hash: 'b', size: 1, files: 1, deployed: true, created_at: '2026-10-02T00:00:00Z' },
    { number: 1, kind: 'server', hash: 'a', size: 1, files: 1, deployed: true, created_at: '2026-10-01T00:00:00Z' }] },
  'POST /console/api/flats/blog/deploy': pending('apr-deploy'),
  'POST /console/api/approvals/apr-deploy/approve': { id: 'apr-deploy', status: 'approved', result: 'version 2 is live' },
};
globalThis.fetch = async (url, opts) => {
  const key = (opts.method || 'GET') + ' ' + url;
  calls.push({ key, body: opts.body, headers: opts.headers });
  let body = routes[key] ?? (key.includes('/logs') ? { events: [] } : key.includes('/stats') ? { page_views: [] } : {});
  if (typeof body === 'function') body = await body(); // a deferred response
  const status = body && body.__status ? body.__status : 200;
  return { ok: status < 400, status, json: async () => body };
};

const { publicNoticeOf, publicURL } = await import('./dom.js');
assert.equal(publicNoticeOf(blog), PUBLIC);
assert.equal(publicNoticeOf(shop), PUBLIC);
assert.equal(publicURL(shop), 'https://shop.tail.ts.net');
assert.equal(publicNoticeOf(notes), '');

// List rows show visibility badges without repeated public-access banners.
const ctx = { setTitle() {}, alive: () => true, navigate() {} };
const list = await import('./list.js');
const listMain = new Element('main');
const stopList = list.mount(listMain, [], ctx);
await tick();
const rows = all(listMain, (e) => e.tagName === 'LI' && e.className === 'flat');
assert.equal(rows.length, 4);
// The type shows as an icon left of the name, labelled for hover and screen
// readers; there is no initials tile or text badge.
const typeLabel = (root) => all(root, (e) => e.className.startsWith('type-icon'))
  .map((e) => [e.getAttribute('role'), e.getAttribute('aria-label'), e.getAttribute('data-tip'), e.getAttribute('tabindex')]);
assert.deepEqual(typeLabel(rows[0]), [['img', 'Flat', 'Flat', '0']]);
assert.deepEqual(typeLabel(rows[1]), [['img', 'Flat', 'Flat', '0']]);
assert.deepEqual(typeLabel(rows[2]), [['img', 'Document', 'Document', '0']]);
assert.deepEqual(typeLabel(rows[3]), [['img', 'Page', 'Page', '0']]);
// The page tile carries its own class and the </> code glyph.
const pageIcon = all(rows[3], (e) => e.className.startsWith('type-icon'))[0];
assert.ok(pageIcon.className.includes('type-page'), pageIcon.className);
assert.equal(all(pageIcon, (e) => e.tagName === 'PATH')[0]?.getAttribute('d'), 'M8 7l-5 5l5 5M16 7l5 5l-5 5M14 4l-4 16');
for (const row of rows) {
  assert.equal(all(row, (e) => /\b(thumb|content-type)\b/.test(e.className)).length, 0);
}
for (const row of rows) {
  assert.equal(all(row, (e) => e.className.includes('notice-text')).length, 0);
}
// The name opens the flat's first management tab in the console, not the site.
for (const [row, site] of rows.map((row, i) => [row, [blog, shop, notes, report][i]])) {
 const name = all(row, (e) => e.tagName === 'A' && e.className === 'flat-name')[0];
 assert.equal(name.getAttribute('href'), `/flats/${site.slug}`);
 assert.equal(name.getAttribute('target'), null);
 assert.equal(name.getAttribute('data-nav'), '');
}
// A running document has its private open button.
const openCount = (row) => all(row, (e) => (e.getAttribute('class') || '').includes('open-links')).length;
assert.equal(all(rows[2], (e) => (e.getAttribute('class') || '').includes('open-private')).length, 1);
assert.equal(openCount(rows[0]), 1);
// Narrow rows also get one open link, the visibility inline under the name
// and every address in the menu; CSS shows them only on small screens.
const classCount = (row, c) => all(row, (e) => (e.getAttribute('class') || '').split(/\s+/).includes(c)).length;
assert.equal(classCount(rows[0], 'open-primary'), 1);
assert.equal(classCount(rows[0], 'flat-sub-vis'), 1);
assert.ok(classCount(rows[0], 'menu-narrow') >= 1);
// A flat with draft changes offers Save as the next version number; a
// document never offers Save or Publish.
const startsWith = (root, prefix) => all(root, (e) => e.tagName === 'BUTTON' && e.textContent.startsWith(prefix));
assert.equal(byText(rows[0], 'Save as v3').length, 1);
assert.ok(rows[0].textContent.includes('Live v2 · unsaved draft changes'), rows[0].textContent);
assert.equal(startsWith(rows[2], 'Save as').length + startsWith(rows[2], 'Publish').length, 0);
assert.ok(rows[2].textContent.includes('Running') && !rows[2].textContent.includes('Live v'), 'a document row shows no version: ' + rows[2].textContent);
assert.equal(byText(rows[2], 'Overview').length, 1);
stopList();
// A stopped document and an unpublished flat have nothing to open.
const savedFlats = routes['GET /console/api/flats'];
routes['GET /console/api/flats'] = { flats: [
  { ...notes, running: false },
  { ...blog, live_version: 0, live: null },
  { ...blog, slug: 'saved-not-live', name: 'Saved Not Live', live_version: 0, live: null, versions: 1, latest_version: 1, latest_deployed: false, draft: { ...blog.draft, dirty: false } },
] };
const idleMain = new Element('main');
const stopIdle = list.mount(idleMain, [], ctx);
await tick();
const idleRows = all(idleMain, (e) => e.tagName === 'LI' && e.className === 'flat');
assert.equal(idleRows.length, 3);
for (const row of idleRows) {
  assert.equal(openCount(row), 0, row.textContent);
  assert.equal(classCount(row, 'open-primary') + classCount(row, 'menu-narrow'), 0, row.textContent);
}
assert.ok(idleRows[2].textContent.includes('Not published · v1 saved'), idleRows[2].textContent);
assert.equal(byText(idleRows[2], 'Publish v1').length, 1, 'a saved version that was never live offers Publish v1');
assert.equal(startsWith(idleRows[2], 'Save as').length, 0, 'no draft changes: nothing to save');
stopIdle();
// A live flat whose newest saved version was never published offers Publish
// for it; one that was live before (a rollback) offers nothing.
routes['GET /console/api/flats'] = { flats: [
  { ...blog, slug: 'next', name: 'Next', latest_version: 3, latest_deployed: false, draft: { ...blog.draft, dirty: false } },
  { ...blog, slug: 'back', name: 'Back', live_version: 1, live: { number: 1, kind: 'server' }, latest_version: 2, latest_deployed: true, draft: { ...blog.draft, dirty: false } },
] };
const nextMain = new Element('main');
const stopNext = list.mount(nextMain, [], ctx);
await tick();
const nextRows = all(nextMain, (e) => e.tagName === 'LI' && e.className === 'flat');
assert.ok(nextRows[0].textContent.includes('Live v2 · v3 saved, not published'), nextRows[0].textContent);
assert.equal(byText(nextRows[0], 'Publish v3').length, 1);
assert.ok(!nextRows[1].textContent.includes('not published'), nextRows[1].textContent);
assert.equal(startsWith(nextRows[1], 'Publish').length + startsWith(nextRows[1], 'Save as').length, 0);
stopNext();
routes['GET /console/api/flats'] = savedFlats;

// The flat's own address is its Deployments tab: status, versions, previews
// and logs under the same tabs as Settings, with no settings panels of its own.
// It offers a redeploy of the live version (and only of it), and the deploy
// result shows the notice.
const flat = await import('./flat.js');
const flatMain = new Element('main');
const stopFlat = flat.mount(flatMain, ['blog'], ctx);
await tick();
assert.deepEqual(typeLabel(flatMain), [['img', 'Flat', 'Flat', '0']]);
const tabLinks = (root) => all(root, (e) => e.tagName === 'A' && e.parentNode?.className === 'site-tabs');
assert.deepEqual(tabLinks(flatMain).map((a) => [a.textContent, a.getAttribute('href'), a.getAttribute('aria-current')]), [
  ['Deployments', '/flats/blog', 'page'], ['Analytics', '/flats/blog/analytics', null],
  ['Database', '/flats/blog/database', null], ['Settings', '/flats/blog/settings', null]]);
for (const id of ['status', 'versions', 'previews', 'logs']) {
  assert.equal(all(flatMain, (e) => e.tagName === 'SECTION' && e.getAttribute('id') === id).length, 1, `deployments tab lacks ${id}`);
}
for (const id of ['env', 'secrets', 'network', 'networks', 'delete', 'visibility', 'usage', 'rename']) {
  assert.equal(all(flatMain, (e) => e.getAttribute('id') === id).length, 0, `deployments tab repeats settings section ${id}`);
}
assert.equal(all(flatMain, (e) => e.tagName === 'SELECT').length, 0, 'visibility changes only through the sharing dialog');
const redeploys = byText(flatMain, 'Redeploy (apply environment)');
assert.equal(redeploys.length, 1, 'want a redeploy button on the live version row only');
const liveRow = all(flatMain, (e) => e.tagName === 'TR' && e.className === 'is-live')[0];
// Version rows name their state: the live one, and an earlier one that was
// live before and is offered as a rollback, never as a new Publish.
const v1Row = all(flatMain, (e) => e.tagName === 'TR' && e.className === '' && e.textContent.startsWith('v1'))[0];
assert.ok(v1Row && v1Row.textContent.includes('Previously live'), v1Row?.textContent);
assert.equal(byText(v1Row, 'Roll back to v1').length, 1);
assert.ok(liveRow && byText(liveRow, 'Redeploy (apply environment)').length === 1, 'live version row has no redeploy action');

redeploys[0].dispatch('click');
await tick();
let dialogs = all(document.body, (e) => e.tagName === 'DIALOG');
assert.equal(dialogs.length, 1);
assert.ok(dialogs[0].textContent.includes('Redeploy version 2?'));
assert.ok(dialogs[0].textContent.includes('environment variables and secrets'));
assert.ok(dialogs[0].textContent.includes('when approved activation begins'));
assert.ok(dialogs[0].textContent.includes('health check and live worker use the same settings snapshot'));
assert.ok(dialogs[0].textContent.includes('after capture apply at the next activation'));
dialogs[0].close('ok');
await tick();
const deploy = calls.find((c) => c.key === 'POST /console/api/flats/blog/deploy');
assert.ok(deploy, 'redeploy must deploy the live version');
assert.deepEqual(JSON.parse(deploy.body), { version: 2 });
assert.ok(calls.some((c) => c.key === 'POST /console/api/approvals/apr-deploy/approve'), 'the console approves its own confirmed request');
dialogs = all(document.body, (e) => e.tagName === 'DIALOG');
assert.equal(dialogs.length, 1);
assert.ok(dialogs[0].textContent.includes('https://blog.portal.example'));
assert.ok(dialogs[0].textContent.includes(PUBLIC), 'deploy result must show the public notice: ' + dialogs[0].textContent);
dialogs[0].close('ok');
await tick();

// The unpublished draft is a row above the published versions and saves
// the exact revision shown as a new version.
const draftRow = all(flatMain, (e) => e.tagName === 'TR' && e.className === 'is-draft')[0];
assert.ok(draftRow, 'want a draft row');
routes['POST /console/api/flats/blog/versions'] = pending('apr-publish');
routes['POST /console/api/approvals/apr-publish/approve'] = { id: 'apr-publish', status: 'approved', result: 'saved version 3', result_data: { version: 3 } };
const pubToastBox = new Element('div');
pubToastBox.setAttribute('id', 'toasts');
const origGetElementById = document.getElementById;
document.getElementById = (id) => (id === 'toasts' ? pubToastBox : origGetElementById?.(id));
byText(draftRow, 'Save as v3')[0].dispatch('click');
await tick();
dialogs = all(document.body, (e) => e.tagName === 'DIALOG');
assert.ok(dialogs[0].textContent.includes('Save the draft of Blog as version 3?'), dialogs[0].textContent);
assert.ok(dialogs[0].textContent.includes('Saving does not publish: version 2 stays live'), dialogs[0].textContent);
dialogs[0].close('ok');
await tick();
document.getElementById = origGetElementById;
assert.deepEqual(JSON.parse(calls.find((c) => c.key === 'POST /console/api/flats/blog/versions').body), { revision: 3, hash: 'c' });
assert.ok(calls.some((c) => c.key === 'POST /console/api/approvals/apr-publish/approve'));
assert.ok(pubToastBox.textContent.includes('Saved version 3. It is not live yet: publish it when ready.'), pubToastBox.textContent);
all(document.body, (e) => e.tagName === 'DIALOG').forEach((d) => d.close('ok'));
await tick();

// The operator deletes one non-live version directly, after an explicit
// destructive confirmation. The live version and old pruned records cannot
// be selected for deletion.
assert.equal(byText(liveRow, 'Delete').length, 0);
let oldRow = all(flatMain, (e) => e.tagName === 'TR' && e.textContent.startsWith('v1'))[0];
assert.equal(byText(oldRow, 'Delete').length, 1);
const deletedBefore = calls.filter((c) => c.key === 'DELETE /console/api/flats/blog/versions/1').length;
byText(oldRow, 'Delete')[0].dispatch('click');
await tick();
dialogs = all(document.body, (e) => e.tagName === 'DIALOG');
assert.ok(dialogs[0].textContent.includes('closes its open previews'));
assert.ok(dialogs[0].textContent.includes('Server data and data snapshots are kept'));
dialogs[0].close('cancel');
await tick();
assert.equal(document.activeElement, byText(oldRow, 'Delete')[0], 'cancelling restores the invoking Delete button');
assert.equal(document.activeElement.focusedWhileDisabled, false, 'focus is restored after the invoker is enabled');
assert.equal(calls.filter((c) => c.key === 'DELETE /console/api/flats/blog/versions/1').length, deletedBefore);
// If the live version changed while the confirmation was open, the server
// refuses deletion and the page reports the error rather than claiming success.
byText(oldRow, 'Delete')[0].dispatch('click');
await tick();
routes['DELETE /console/api/flats/blog/versions/1'] = { __status: 409, error: 'the live version cannot be deleted' };
all(document.body, (e) => e.tagName === 'DIALOG')[0].close('ok');
await tick();
dialogs = all(document.body, (e) => e.tagName === 'DIALOG');
assert.ok(dialogs[0].textContent.includes('Version deletion failed'));
assert.ok(dialogs[0].textContent.includes('the live version cannot be deleted'));
dialogs[0].close('ok'); await tick();
byText(oldRow, 'Delete')[0].dispatch('click');
await tick();
const beforeDeletionVersions = routes['GET /console/api/flats/blog/versions'];
routes['DELETE /console/api/flats/blog/versions/1'] = () => {
  routes['GET /console/api/flats/blog'] = { ...blog, version_count: 1 };
  routes['GET /console/api/flats/blog/versions'] = { versions: beforeDeletionVersions.versions.filter((v) => v.number !== 1) };
  return { status: 'deleted' };
};
const approvalsBeforeDelete = calls.filter((c) => c.key.includes('/approvals/') && c.key.endsWith('/approve')).length;
all(document.body, (e) => e.tagName === 'DIALOG')[0].close('ok');
await tick();
assert.equal(calls.filter((c) => c.key === 'DELETE /console/api/flats/blog/versions/1').length, deletedBefore + 2);
assert.equal(calls.filter((c) => c.key.includes('/approvals/') && c.key.endsWith('/approve')).length, approvalsBeforeDelete);
assert.ok(flatMain.textContent.includes('1 version stored · Quota: 20 (host default).'));
assert.equal(all(flatMain, (e) => e.tagName === 'TR' && e.textContent.startsWith('v1')).length, 0);
routes['GET /console/api/flats/blog'] = blog;
routes['GET /console/api/flats/blog/versions'] = beforeDeletionVersions;

// Per-flat quota supports a host default, explicit unlimited and safe integer
// validation. A refusal preserves the input and exposes an accessible error.
const quotaMain = new Element('main');
const stopQuota = flat.mountSettings(quotaMain, ['blog'], ctx);
await tick();
const quotaInput = all(quotaMain, (e) => e.getAttribute('id') === 'version-quota')[0];
const quotaForm = quotaInput.parentNode.parentNode;
assert.equal(quotaInput.value, '');
assert.ok(quotaMain.textContent.includes('Lowering this quota keeps every existing version'));
quotaInput.value = '1.5'; quotaForm.dispatch('submit'); await tick();
assert.equal(quotaInput.getAttribute('aria-invalid'), 'true');
assert.equal(calls.some((c) => c.key === 'PUT /console/api/flats/blog/version-quota'), false);
routes['PUT /console/api/flats/blog/version-quota'] = { __status: 409, error: 'quota changed; try again' };
quotaInput.value = '0'; quotaForm.dispatch('submit'); await tick();
assert.deepEqual(JSON.parse(calls.filter((c) => c.key === 'PUT /console/api/flats/blog/version-quota').at(-1).body), { version_quota: 0 });
assert.equal(quotaInput.value, '0');
assert.ok(quotaForm.textContent.includes('quota changed; try again'));
routes['PUT /console/api/flats/blog/version-quota'] = blog;
quotaInput.value = ''; quotaForm.dispatch('submit'); await tick();
assert.deepEqual(JSON.parse(calls.filter((c) => c.key === 'PUT /console/api/flats/blog/version-quota').at(-1).body), { version_quota: null });
stopQuota();

// A migrated flat above its quota keeps its history and explains recovery;
// pruned historical records offer no actions and don't count as stored.
routes['GET /console/api/flats/blog'] = { ...blog, latest_version: 3, effective_version_quota: 1 };
routes['GET /console/api/flats/blog/versions'] = { versions: [
  ...beforeDeletionVersions.versions.map((v) => v.number === 1 ? { ...v, pruned: true } : v),
  { number: 3, kind: 'server', hash: 'new', size: 1, files: 1, created_at: '2026-10-03T00:00:00Z' }] };
const fullMain = new Element('main');
const stopFull = flat.mount(fullMain, ['blog'], ctx); await tick();
assert.ok(fullMain.textContent.includes('The quota is full. Delete older versions or raise the quota'));
const prunedRow = all(fullMain, (e) => e.tagName === 'TR' && e.textContent.startsWith('v1'))[0];
assert.ok(prunedRow.textContent.includes('Unavailable'));
assert.equal(all(prunedRow, (e) => e.tagName === 'BUTTON').length, 0);
stopFull();
routes['GET /console/api/flats/blog'] = { ...blog, effective_version_quota: 0, version_quota: 0 };
const unlimitedMain = new Element('main');
const stopUnlimited = flat.mount(unlimitedMain, ['blog'], ctx); await tick();
assert.ok(unlimitedMain.textContent.includes('Unlimited quota (override for this flat).'));
assert.equal(unlimitedMain.textContent.includes('The quota is full'), false);
stopUnlimited();
routes['GET /console/api/flats/blog'] = blog;
routes['GET /console/api/flats/blog/versions'] = beforeDeletionVersions;

// Deleting the highest stored version, or every version, never reuses its
// number in either the management page or list Save action.
routes['GET /console/api/flats/blog'] = { ...blog, latest_version: 2, next_version: 5 };
const highwaterMain = new Element('main');
const stopHighwater = flat.mount(highwaterMain, ['blog'], ctx); await tick();
assert.equal(byText(highwaterMain, 'Save as v5').length, 1);
assert.equal(byText(highwaterMain, 'Save as v3').length, 0);
stopHighwater();
const versionActions = await import('./actions.js');
assert.equal(versionActions.nextVersion({ latest_version: 0, next_version: 5 }), 5);
routes['GET /console/api/flats/blog'] = blog;

// A refresh can remove the invoker while its confirmation is open. Closing
// it must never focus a detached button from the earlier version table.
const detachedMain = new Element('main');
const stopDetached = flat.mount(detachedMain, ['blog'], ctx); await tick();
const detachedHistory = all(detachedMain, (e) => e.getAttribute('aria-label') === 'Version history')[0];
const detachedDelete = all(detachedMain, (e) => e.getAttribute('aria-label') === 'Delete version 1')[0];
const detachedFocusCount = detachedDelete.focusCount || 0;
detachedDelete.dispatch('click'); await tick();
let detachedRow = detachedDelete;
while (detachedRow.tagName !== 'TR') detachedRow = detachedRow.parentNode;
detachedRow.remove();
all(document.body, (e) => e.tagName === 'DIALOG')[0].close('cancel'); await tick();
assert.equal(detachedDelete.focusCount || 0, detachedFocusCount);
assert.equal(document.activeElement, detachedHistory, 'a removed invoker falls back to this page’s history region');
stopDetached();

const approvalsPage = await import('./approval.js');
assert.equal(approvalsPage.describeApproval({ action: 'create_version', flat: 'blog' }), 'Save the draft of blog as a new version (does not go live)');
assert.equal(approvalsPage.describeApproval({ action: 'delete_version', flat: 'blog', params: { version: 1 } }), 'Delete version 1 of blog');
routes['GET /console/api/approvals/delete-one'] = { id: 'delete-one', flat: 'blog', action: 'delete_version', params: { version: 1 }, status: 'pending', via: 'mcp' };
const deleteApprovalMain = new Element('main');
approvalsPage.mount(deleteApprovalMain, ['delete-one'], ctx); await tick();
assert.ok(deleteApprovalMain.textContent.includes('closes its open previews'));
assert.ok(deleteApprovalMain.textContent.includes('The live version cannot be deleted'));
// Durable deletion can apply metadata while private storage cleanup is
// pending. The console acknowledges that outcome without retrying or showing
// a fully completed success toast; approved receipts keep the warning too.
const cleanupMessage = 'Version 1 was deleted from metadata; restart Flats before saving, creating, deploying, or previewing this flat.';
routes['DELETE /console/api/flats/blog/versions/1'] = { status: 'cleanup_pending', cleanup_pending: true, version: 1, message: cleanupMessage };
const cleanupToasts = new Element('div');
document.getElementById = (id) => id === 'toasts' ? cleanupToasts : origGetElementById?.(id);
const cleanupCallsBefore = calls.filter((c) => c.key === 'DELETE /console/api/flats/blog/versions/1').length;
const cleanupPromise = versionActions.deleteVersion(blog, 1);
await tick();
all(document.body, (e) => e.tagName === 'DIALOG')[0].close('ok'); await tick();
const cleanupDialog = all(document.body, (e) => e.tagName === 'DIALOG')[0];
assert.ok(cleanupDialog.textContent.includes('Version removed; cleanup pending'));
assert.ok(cleanupDialog.textContent.includes(cleanupMessage));
assert.equal(cleanupToasts.textContent.includes('Version 1 deleted.'), false);
cleanupDialog.close('ok');
assert.equal(await cleanupPromise, true, 'metadata change still refreshes the list');
assert.equal(calls.filter((c) => c.key === 'DELETE /console/api/flats/blog/versions/1').length, cleanupCallsBefore + 1);
document.getElementById = origGetElementById;
routes['GET /console/api/approvals/delete-pending-cleanup'] = { id: 'delete-pending-cleanup', flat: 'blog', action: 'delete_version', params: { version: 1 }, status: 'approved', via: 'mcp', result: cleanupMessage, result_data: JSON.stringify({ cleanup_pending: true }) };
const cleanupApprovalMain = new Element('main');
approvalsPage.mount(cleanupApprovalMain, ['delete-pending-cleanup'], ctx); await tick();
assert.ok(cleanupApprovalMain.textContent.includes('Restart Flats to finish storage cleanup'));
assert.equal(cleanupApprovalMain.textContent.includes('Nothing else to do'), false);
assert.equal(all(cleanupApprovalMain, (e) => e.getAttribute('role') === 'alert' && e.textContent.includes(cleanupMessage)).length, 1);


stopFlat();
// A document's page has an Overview with status and logs but no versions,
// previews, Draft or publishing, and its settings no environment panels.
const docMain = new Element('main');
const stopDoc = flat.mount(docMain, ['notes'], ctx);
await tick();
assert.deepEqual(tabLinks(docMain).map((a) => a.textContent), ['Overview', 'Analytics', 'Settings']);
for (const id of ['versions', 'previews']) {
  assert.equal(all(docMain, (e) => e.getAttribute('id') === id).length, 0, `document page shows ${id}`);
}
for (const id of ['status', 'logs']) {
  assert.equal(all(docMain, (e) => e.tagName === 'SECTION' && e.getAttribute('id') === id).length, 1, `document page lacks ${id}`);
}
for (const text of ['Redeploy (apply environment)', 'Preview']) {
  assert.equal(byText(docMain, text).length, 0, `document page offers ${text}`);
}
for (const prefix of ['Save as', 'Publish', 'Roll back']) {
  assert.equal(startsWith(docMain, prefix).length, 0, `document page offers ${prefix}`);
}
assert.ok(docMain.textContent.includes('Running') && !docMain.textContent.includes('Not published'));
assert.equal(calls.some((c) => c.key.startsWith('GET /console/api/flats/notes/versions') || c.key.startsWith('GET /console/api/flats/notes/previews')), false, 'a document loads no versions or previews');
stopDoc();
const docSettings = new Element('main');
const stopDocSettings = flat.mountSettings(docSettings, ['notes'], ctx);
await tick();
for (const id of ['env', 'secrets', 'network']) {
  assert.equal(all(docSettings, (e) => e.getAttribute('id') === id).length, 0, `document settings show ${id}`);
}
assert.equal(all(docSettings, (e) => e.getAttribute('id') === 'delete').length, 1);
stopDocSettings();
// A page keeps deployments, versions and previews, but has no Database tab,
// no environment redeploy and no server settings panels.
const pageMain = new Element('main');
const stopPage = flat.mount(pageMain, ['report'], ctx);
await tick();
assert.deepEqual(typeLabel(pageMain), [['img', 'Page', 'Page', '0']]);
assert.deepEqual(tabLinks(pageMain).map((a) => a.textContent), ['Deployments', 'Analytics', 'Settings']);
for (const id of ['status', 'versions', 'previews', 'logs']) {
  assert.equal(all(pageMain, (e) => e.tagName === 'SECTION' && e.getAttribute('id') === id).length, 1, `page lacks ${id}`);
}
assert.equal(byText(pageMain, 'Redeploy (apply environment)').length, 0, 'a page has no environment to apply');
stopPage();
const pageSettings = new Element('main');
const stopPageSettings = flat.mountSettings(pageSettings, ['report'], ctx);
await tick();
for (const id of ['env', 'secrets', 'network']) {
  assert.equal(all(pageSettings, (e) => e.getAttribute('id') === id).length, 0, `page settings show ${id}`);
}
assert.equal(all(pageSettings, (e) => e.getAttribute('id') === 'delete').length, 1);
assert.equal(calls.some((c) => /\/flats\/report\/(env|secrets|network)$/.test(c.key)), false, 'a page loads no server settings');
stopPageSettings();
// A board runs once saved like a document or design: its own label and
// icon, an open link to the canvas, and no versions, previews, Database or
// server settings. Its settings hold the revision quotas.
const sketch = { ...notes, type: 'board', slug: 'sketch', name: 'Sketch', private_url: 'https://sketch.tail.ts.net' };
routes['GET /console/api/flats/sketch'] = sketch;
routes['GET /console/api/flats/sketch/revision-quota'] = { revision_quota: null, pin_quota: null, default_revision_quota: 100, default_pin_quota: 20, pins: 0 };
const listedFlats = routes['GET /console/api/flats'];
routes['GET /console/api/flats'] = { flats: [sketch] };
const boardList = new Element('main');
const stopBoardList = list.mount(boardList, [], ctx);
await tick();
const boardRow = all(boardList, (e) => e.tagName === 'LI' && e.className === 'flat')[0];
assert.deepEqual(typeLabel(boardRow), [['img', 'Board', 'Board', '0']]);
const boardIcon = all(boardRow, (e) => e.className.startsWith('type-icon'))[0];
assert.ok(boardIcon.className.includes('type-board'), boardIcon.className);
assert.equal(all(boardIcon, (e) => e.tagName === 'PATH')[0]?.getAttribute('d'), 'M3 4h18v16H3zM6 7h5v5H6zM13 16l5-5M15 11h3v3');
assert.ok(boardRow.textContent.includes('Running') && !boardRow.textContent.includes('Live v'), 'a board row shows no version: ' + boardRow.textContent);
assert.equal(startsWith(boardRow, 'Save as').length + startsWith(boardRow, 'Publish').length, 0);
assert.equal(all(boardRow, (e) => (e.getAttribute('class') || '').includes('open-private')).length, 1, 'a running board links to its canvas');
assert.equal(byText(boardRow, 'Overview').length, 1);
stopBoardList();
routes['GET /console/api/flats'] = listedFlats;
const boardMain = new Element('main');
const stopBoard = flat.mount(boardMain, ['sketch'], ctx);
await tick();
assert.deepEqual(typeLabel(boardMain), [['img', 'Board', 'Board', '0']]);
assert.deepEqual(tabLinks(boardMain).map((a) => a.textContent), ['Overview', 'Analytics', 'Settings']);
for (const id of ['versions', 'previews']) {
  assert.equal(all(boardMain, (e) => e.getAttribute('id') === id).length, 0, `board page shows ${id}`);
}
assert.ok(all(boardMain, (e) => e.tagName === 'DT' && e.textContent === 'Board').length === 1, 'board status names the type');
assert.ok(boardMain.textContent.includes('Running') && !boardMain.textContent.includes('Not published'));
for (const prefix of ['Save as', 'Publish', 'Roll back']) {
  assert.equal(startsWith(boardMain, prefix).length, 0, `board page offers ${prefix}`);
}
assert.equal(calls.some((c) => /\/flats\/sketch\/(versions|previews)/.test(c.key)), false, 'a board loads no versions or previews');
stopBoard();
const boardSettings = new Element('main');
const stopBoardSettings = flat.mountSettings(boardSettings, ['sketch'], ctx);
await tick();
for (const id of ['env', 'secrets', 'network', 'version-history']) {
  assert.equal(all(boardSettings, (e) => e.getAttribute('id') === id).length, 0, `board settings show ${id}`);
}
assert.equal(all(boardSettings, (e) => e.getAttribute('id') === 'revisions').length, 1, 'board settings lack revision quotas');
assert.ok(boardSettings.textContent.includes('People and agents manage revisions on the board itself'));
assert.equal(calls.some((c) => /\/flats\/sketch\/(env|secrets|network)$/.test(c.key)), false, 'a board loads no server settings');
stopBoardSettings();
// The Settings tab holds every setting once, and still offers the redeploy
// that applies environment changes.
const management = new Element('main');
const stopSettings = flat.mountSettings(management, ['blog'], ctx);
await tick();
assert.equal(all(management, (e) => e.tagName === 'A' && e.textContent === 'Scheduled').length, 0);
assert.deepEqual(tabLinks(management).map((a) => [a.textContent, a.getAttribute('aria-current')]),
  [['Deployments', null], ['Analytics', null], ['Database', null], ['Settings', 'page']]);
for (const id of ['versions', 'previews', 'logs']) {
  assert.equal(all(management, (e) => e.getAttribute('id') === id).length, 0, `settings tab repeats ${id}`);
}
assert.equal(byText(management, 'Redeploy (apply environment)').length, 3, 'want a redeploy button in each environment panel');
const secrets = all(management, (e) => e.tagName === 'SECTION' && e.getAttribute('id') === 'secrets')[0];
assert.ok(secrets.textContent.includes('redeploy the live version 2'), 'secrets panel must mention redeploying: ' + secrets.textContent);
// Networks are per-flat permissions; Local is always on.
const portal = all(management, (e) => e.tagName === 'INPUT' && e.getAttribute('id') === 'net-portal')[0];
assert.equal(portal.checked, true);
const zrokBox = all(management, (e) => e.tagName === 'INPUT' && e.getAttribute('id') === 'net-zrok')[0];
assert.ok(zrokBox, 'zrok is listed among the public networks');
assert.ok(!zrokBox.checked);
const funnel = all(management, (e) => e.tagName === 'INPUT' && e.getAttribute('id') === 'net-tailscale-funnel')[0];
funnel.checked = true;
funnel.dispatch('change');
await tick();
assert.deepEqual(JSON.parse(calls.find((c) => c.key === 'POST /console/api/flats/blog/providers').body), { provider: 'tailscale-funnel', permitted: true });
// A grant whose route did not open is still recorded by the server, so the
// page reloads the flat instead of only unticking the box.
routes['POST /console/api/flats/blog/providers'] = { __status: 409, category: 'provider_not_ready', error: 'provider not ready: tailscale is permitted, but its route did not open' };
const tailscaleBox = all(management, (e) => e.tagName === 'INPUT' && e.getAttribute('id') === 'net-tailscale')[0];
const flatLoads = calls.filter((c) => c.key === 'GET /console/api/flats/blog').length;
routes['GET /console/api/flats/blog'] = { ...blog, providers: ['portal', 'tailscale'] };
tailscaleBox.checked = true;
tailscaleBox.dispatch('change');
await tick();
const grantError = all(document.body, (e) => e.tagName === 'DIALOG').pop();
assert.ok(grantError.textContent.includes('Tailscale allowed, but its route did not open'), 'a saved grant is not reported as unchanged: ' + grantError.textContent);
assert.ok(!grantError.textContent.includes('Cannot change'));
grantError.close();
await tick();
assert.ok(calls.filter((c) => c.key === 'GET /console/api/flats/blog').length > flatLoads, 'a failed grant must reload the flat');
assert.equal(all(management, (e) => e.tagName === 'INPUT' && e.getAttribute('id') === 'net-tailscale')[0].checked, true, 'the redrawn box shows the saved grant');
routes['GET /console/api/flats/blog'] = blog;
delete routes['POST /console/api/flats/blog/providers'];
const envPanel = all(management, (e) => e.getAttribute('id') === 'env')[0];
const secretPanel = all(management, (e) => e.getAttribute('id') === 'secrets')[0];
assert.ok(envPanel.textContent.includes('Ordinary environment variables'));
for (const disclosure of ['plain text', 'readable by authorized agents', 'env.NAME', 'WASI', 'browser bundles', 'next approved activation or Flats host restart']) {
  assert.ok(envPanel.textContent.includes(disclosure), `environment panel lacks ${disclosure}`);
}
assert.ok(envPanel.textContent.includes('<script>demo</script>\nsecond line'));
assert.equal(all(envPanel, (e) => e.tagName === 'SCRIPT').length, 0, 'values must be text, never HTML');
assert.ok(envPanel.textContent.includes('(empty)'));
for (const panel of [envPanel, secretPanel]) {
  assert.ok(panel.textContent.includes('next approved activation or Flats host restart'));
  assert.ok(panel.textContent.includes('standalone data snapshot restoration capture current settings'));
  assert.ok(panel.textContent.includes('Automatic worker restarts reuse the captured settings'));
  assert.ok(panel.textContent.includes('New previews load current settings'));
}
assert.equal(secretPanel.textContent.includes('SECRET_MUST_NEVER_RENDER'), false);
assert.equal(all(secretPanel, (e) => e.getAttribute('id') === 'secret-value')[0].getAttribute('type'), 'password');
const field = (id) => all(envPanel, (e) => e.getAttribute('id') === id)[0];
byText(envPanel, 'Edit')[0].dispatch('click');
assert.equal(field('env-name').value, 'APP_MODE');
assert.equal(field('env-value').value, '<script>demo</script>\nsecond line');
assert.equal(field('env-form').hidden, false);
assert.equal(byText(envPanel, 'Add variable')[0].getAttribute('aria-expanded'), 'true');
// A refused save preserves both fields; ordinary errors never include values.
routes['PUT /console/api/flats/blog/env/APP_MODE'] = { __status: 400, error: 'environment variable name is reserved' };
field('env-value').value = 'keep this value';
field('env-form').dispatch('submit');
await tick();
assert.equal(field('env-value').value, 'keep this value');
assert.deepEqual(JSON.parse(calls.filter((c) => c.key === 'PUT /console/api/flats/blog/env/APP_MODE').at(-1).body), { value: 'keep this value' });
routes['PUT /console/api/flats/blog/env/APP_MODE'] = {};
field('env-value').value = '';
field('env-form').dispatch('submit');
await tick();
const envPut = calls.filter((c) => c.key === 'PUT /console/api/flats/blog/env/APP_MODE').at(-1);
assert.deepEqual(JSON.parse(envPut.body), { value: '' }, 'saving an empty value must not delete it');
// Deleting is confirmed, and does not send the value or mutate secrets.
byText(envPanel, 'Delete')[0].dispatch('click');
await tick();
let envDelete = all(document.body, (e) => e.tagName === 'DIALOG')[0];
assert.ok(envDelete.textContent.includes('Delete variable APP_MODE?'));
assert.ok(envDelete.textContent.includes('next approved activation or Flats host restart'));
assert.equal(calls.some((c) => c.key === 'DELETE /console/api/flats/blog/env/APP_MODE'), false);
envDelete.close('cancel');
await tick();
assert.equal(calls.some((c) => c.key === 'DELETE /console/api/flats/blog/env/APP_MODE'), false);
byText(envPanel, 'Delete')[0].dispatch('click');
await tick();
all(document.body, (e) => e.tagName === 'DIALOG')[0].close('ok');
await tick();
assert.ok(calls.some((c) => c.key === 'DELETE /console/api/flats/blog/env/APP_MODE'));
assert.equal(calls.some((c) => /^(PUT|DELETE).*\/secrets\//.test(c.key)), false);
assert.equal(byText(management, 'Add secret').length, 1);
assert.equal(byText(management, 'Add variable').length, 1);
byText(management, 'Add variable')[0].dispatch('click');
assert.ok(all(management, (e) => e.tagName === 'FORM' && e.className === 'form-grid').some((e) => !e.hidden));
const networkPanel = all(management, (e) => e.tagName === 'SECTION' && e.getAttribute('id') === 'network')[0];
assert.ok(networkPanel.textContent.includes('automatic restarts retain captured grants'));
assert.ok(networkPanel.textContent.includes('Browser fetch follows browser CORS/CSP'));
const originInput = all(networkPanel, (e) => e.tagName === 'TEXTAREA')[0];
assert.equal(originInput.value, 'https://api.example.com');
originInput.value = 'https://api.example.com\nhttps://other.example.com';
all(networkPanel, (e) => e.tagName === 'FORM')[0].dispatch('submit');
await tick();
assert.deepEqual(JSON.parse(calls.filter((c) => c.key === 'PUT /console/api/flats/blog/network').at(-1).body), { origins: ['https://api.example.com', 'https://other.example.com'] });
byText(networkPanel, 'Clear server permissions')[0].dispatch('click');
await tick();
const clearDialog = all(document.body, (e) => e.tagName === 'DIALOG')[0];
assert.ok(clearDialog.textContent.includes('Redeploy the live version after clearing'));
clearDialog.close('ok');
await tick();
assert.deepEqual(JSON.parse(calls.filter((c) => c.key === 'PUT /console/api/flats/blog/network').at(-1).body), { origins: [] });
// A Portal flat chooses its relay listing; hiding says it is not access control.
let listingSelect = all(management, (e) => e.getAttribute('id') === 'portal-listing')[0];
assert.ok(listingSelect, 'a Portal flat shows its relay listing');
assert.equal(listingSelect.value, 'default');
assert.ok(listingSelect.parentNode.textContent.includes('anyone with the URL can still open it'));
listingSelect.value = 'hidden';
listingSelect.dispatch('change');
await tick();
assert.deepEqual(JSON.parse(calls.filter((c) => c.key === 'PUT /console/api/flats/blog/listing').at(-1).body), { listing: 'hidden' });
listingSelect = all(management, (e) => e.getAttribute('id') === 'portal-listing')[0];
assert.equal(listingSelect.value, 'hidden');
assert.ok(listingSelect.parentNode.textContent.includes('Relays are asked to hide this flat'));
assert.ok(!listingSelect.parentNode.textContent.includes('now.'), 'the console must not claim relays already changed');
stopSettings();
const analytics = await import('./analytics.js');
assert.deepEqual(analytics.dailySeries([{ day: '2026-10-02', count: 4 }, { day: '2026-01-01', count: 99 }], 2, new Date('2026-10-03T12:00:00Z')),
 [{ day: '2026-10-02', count: 4 }, { day: '2026-10-03', count: 0 }]);
const analyticsMain = new Element('main');
analytics.mount(analyticsMain, ['blog'], ctx);
await tick();
assert.ok(analyticsMain.textContent.includes('Traffic over time'));
assert.ok(analyticsMain.textContent.includes('(versions, data and previews)'), 'analytics shows the storage the flat page used to');
byText(analyticsMain, '7d')[0].dispatch('click');
await tick();
assert.ok(calls.some((c) => c.key.endsWith('/stats?days=7')));
const database = await import('./database.js');
routes['GET /console/api/flats/blog/snapshots'] = { snapshots: ['before-v2-20261002.sqlite'] };
const dbMain = new Element('main');
database.mount(dbMain, ['blog'], ctx);
await tick();
assert.ok(dbMain.textContent.includes('before-v2-20261002.sqlite'));
const { shareDialog } = await import('./share.js');
shareDialog(blog);
const share = all(document.body, (e) => e.tagName === 'DIALOG')[0];
assert.ok(share.textContent.includes('Share Blog'));
assert.ok(share.textContent.includes(PUBLIC));
assert.deepEqual(radios(share).map((r) => r.value), ['private', 'public'], 'a website has two choices');
assert.equal(radios(share).find((r) => r.checked).value, 'public');
assert.equal(byText(share, 'Stop guest editing')[0].parentNode.hidden, true, 'no Stop guest editing for a website');
pick(share, 'private');
await tick();
const refreshDuringVisibility = byText(share, 'Refresh')[0];
assert.equal(refreshDuringVisibility.getAttribute('aria-disabled'), 'true');
const visibilityCalls = calls.length;
refreshDuringVisibility.dispatch('click');
assert.equal(calls.length, visibilityCalls, 'refresh waits for visibility changes');
const permissionConfirm = all(document.body, (e) => e.tagName === 'DIALOG' && e !== share)[0];
assert.ok(permissionConfirm.textContent.includes('Make Blog private?'));
routes['POST /console/api/flats/blog/visibility'] = pending('apr-vis');
routes['POST /console/api/approvals/apr-vis/approve'] = { id: 'apr-vis', status: 'approved', result: 'blog is now private' };
routes['GET /console/api/flats/blog'] = { ...blog, visibility: 'private', public_url: undefined };
permissionConfirm.close('ok');
await tick();
assert.deepEqual(JSON.parse(calls.find((c) => c.key === 'POST /console/api/flats/blog/visibility').body), { visibility: 'private', reason: '' });
assert.ok(share.textContent.includes('access rules of the network serving this link'));
assert.equal(all(share, (e) => e.tagName === 'A' && e.textContent === 'Visit')[0].getAttribute('href'), new URL(blog.private_url).href);
share.close();
// Before a private flat goes public, the warning must not say it already is.
const quiet = { ...blog, slug: 'quiet', name: 'Quiet', visibility: 'private', public_url: undefined, public_notice: undefined };
shareDialog(quiet);
const quietShare = all(document.body, (e) => e.tagName === 'DIALOG' && e.open !== false && e.textContent.includes('Share Quiet'))[0];
pick(quietShare, 'public');
await tick();
const publicConfirm = all(document.body, (e) => e.tagName === 'DIALOG' && e.textContent.includes('Make Quiet public?'))[0];
assert.ok(publicConfirm, 'making a private flat public asks first');
assert.ok(publicConfirm.textContent.includes('Once public, anyone on the internet can open this flat.'), publicConfirm.textContent);
assert.ok(!publicConfirm.textContent.includes(PUBLIC), 'the confirm must not claim the flat is public yet');
publicConfirm.close('cancel');
await tick();
quietShare.close();
// Designs and boards run without published versions (live_version 0), so
// widening them reaches the confirm; a versioned flat still publishes first.
for (const type of ['design', 'board']) {
  const live = { ...quiet, type, slug: 'live-' + type, name: 'Live ' + type, running: true, live_version: 0, versions: 0, draft: null, providers: ['portal'] };
  const widened = versionActions.setVisibility(live, 'public');
  await tick();
  const confirm = all(document.body, (e) => e.tagName === 'DIALOG' && e.textContent.includes(`Make Live ${type} public?`))[0];
  assert.ok(confirm, type + ' widening reaches the confirm dialog');
  assert.ok(!all(document.body, (e) => e.tagName === 'DIALOG' && e.open !== false && e.textContent.includes('Publish first')).length, type + ' is not asked to publish');
  confirm.close('cancel');
  assert.equal(await widened, null);
}
const unpublished = versionActions.setVisibility({ ...quiet, slug: 'unpub', name: 'Unpub', live_version: 0, providers: ['portal'] }, 'public');
await tick();
const publishFirst = all(document.body, (e) => e.tagName === 'DIALOG' && e.textContent.includes('Unpub has no live version yet'))[0];
assert.ok(publishFirst, 'a versioned flat without a live version must publish first');
publishFirst.close('ok');
assert.equal(await unpublished, null);
for (const label of ['Share', 'Deployments', 'Analytics', 'Settings']) assert.equal(byText(rows[0], label).length, 1);

// The build label next to the brand: a release links to its release page,
// a clean development build to its commit, a modified one to nothing.
const { buildOf, buildLabel } = await import('./build.js');
const C = 'dba5279b25052e8b31a4fd15c6c3765de68fc497';
let label = buildLabel({ version: 'v0.2.0', release: true, commit: C, dirty: false, go: 'go1.27.1', os: 'linux', arch: 'arm64' });
assert.equal(label.tagName, 'A');
assert.equal(label.textContent, 'v0.2.0');
assert.equal(label.getAttribute('href'), 'https://github.com/gosuda/flats/releases/tag/v0.2.0');
assert.equal(label.getAttribute('target'), '_blank');
assert.ok(label.getAttribute('title').includes(`commit ${C}`) && label.getAttribute('title').includes('go1.27.1 · linux/arm64'));
assert.equal(label.getAttribute('aria-label'), 'Flats version v0.2.0');
label = buildLabel({ version: 'dba5279', release: false, commit: C, dirty: false, go: 'go1.27.1', os: 'darwin', arch: 'arm64' });
assert.equal(label.getAttribute('href'), `https://github.com/gosuda/flats/commit/${C}`);
assert.ok(label.className.includes('build-dev'));
label = buildLabel({ version: 'dba5279-dirty', release: false, commit: C, dirty: true, go: 'go1.27.1', os: 'darwin', arch: 'arm64' });
assert.equal(label.tagName, 'SPAN', 'a modified build has no commit on GitHub to link');
assert.equal(label.textContent, 'dba5279-dirty');
assert.equal(all(label, (e) => e.className === 'build-dirty').length, 1);
assert.ok(label.getAttribute('title').includes('with uncommitted changes'));
label = buildLabel({ version: 'dba5279', release: false, commit: '', dirty: false, go: 'go1.27.1' });
assert.equal(label.tagName, 'SPAN', 'go install @main knows only the short commit');
assert.equal(buildLabel(undefined), null, 'an older host without build info shows nothing');
// /api/status nests the build under system, next to the older system.version.
const reported = { version: 'dba5279', release: false, commit: C, dirty: false };
assert.equal(buildOf({ ok: true, system: { version: 'dba5279', build: reported } }), reported);
assert.equal(buildOf({ ok: true, system: { version: 'dev' } }), null);
assert.equal(buildOf({ build: reported }), null, 'the build is read from system.build, not the top level');
assert.equal(buildOf(null), null);
assert.equal(buildLabel({ version: '' }), null);

// --- settings page ---
// Settings has four sections behind a left nav. Each page stages its field
// edits for one save bar; buttons such as Turn on, Add and Remove act at once.
// The console never asks through window.confirm.
globalThis.confirm = () => { throw new Error('window.confirm used'); };
globalThis.location = { origin: 'http://127.0.0.1:7878' };
const settingsPage = await import('./settings.js');
const byId = (root, id) => all(root, (e) => e.getAttribute('id') === id)[0];
const SETTINGS = { upload_max_bytes: '20971520', version_quota: '20', disk_quota_bytes: '32212254720', preview_ttl_seconds: '86400',
  rate_limit_rps: '50', redirect_days: '7', events_keep: '5000', portal_relays: 'https://relay.example.com', portal_discovery: 'true', portal_max_relays: '3', portal_hide: 'false' };
const CONFIG = {
  mode: 'config', schema_version: 5, etag: 'etag-1', changed_on_disk: false,
  host: { console_addr: '127.0.0.1:7878', local_addr: '127.0.0.1:7879', server_runtime: true },
  network: { permitted: ['portal'], private_backend: 'local' },
  tailscale: { console_hosts: [] },
  credentials: { operator_file: true, tailscale_authkey_file: false },
  keys: { upload_max_bytes: 'system.upload_max_bytes', version_quota: 'system.version_quota', disk_quota_bytes: 'system.disk_quota_bytes',
    preview_ttl_seconds: 'system.preview_ttl_seconds', rate_limit_rps: 'system.rate_limit_rps', redirect_days: 'system.redirect_days',
    events_keep: 'system.events_keep', portal_relays: 'portal.relays', portal_discovery: 'portal.discovery', portal_max_relays: 'portal.max_active_relays', portal_hide: 'portal.hide' },
  sources: { 'host.console_addr': 'flag', 'host.local_addr': 'default', 'tailscale.console_hosts': 'default', 'host.server_runtime': 'default',
    'network.permitted': 'file', 'network.private_backend': 'default', 'credentials.operator_file': 'file', 'credentials.tailscale_authkey_file': 'default',
    'system.version_quota': 'file', 'system.events_keep': 'default', 'portal.relays': 'file' },
  pinned: { portal_relays: "the service's --relays flag" },
};
const settingsRoute = (config) => ({ settings: SETTINGS, defaults: SETTINGS, apply_on_restart: ['portal_relays'], config });
routes['GET /console/api/settings'] = settingsRoute(CONFIG);
const provider = (id, scope, enabled, flats = []) => ({ id, scope, enabled, configured: enabled, flats });
const PROVIDERS = { providers: [provider('local', 'private', true), provider('tailscale', 'private', false),
  provider('tailscale-funnel', 'public', false), provider('portal', 'public', true, ['blog']),
  { ...provider('zrok', 'public', true), namespace: 'flats-ns', managed: true, connected: true, environment_id: 'zenv-123', api_endpoint: 'https://api-v2.zrok.io', status: { kind: 'zrok', enabled: true, hosts: [] } },
  { ...provider('cloudflare-quick', 'public', true, ['demo']), status: { kind: 'cloudflare-quick', enabled: true, hosts: [{ host: 'demo', url: 'https://quick-demo.trycloudflare.com', state: 'ready', detail: 'ready at https://quick-demo.trycloudflare.com' }] } }] };
routes['GET /console/api/providers'] = PROVIDERS;
const KEYS = [
  { id: 'tailscale', set: false, managed: true },
  { id: 'netbird', set: false, managed: true, management_url: 'https://api.netbird.io:443' },
  { id: 'zrok', set: true, managed: true, source: 'console', updated_at: '2026-10-01T00:00:00Z', environment_id: 'zenv-123', api_endpoint: 'https://api-v2.zrok.io' },
];
routes['GET /console/api/keys'] = { keys: KEYS };
const DOMAINS = { providers: { tailscale: { domains: [], retiring: [], on: false }, netbird: { domains: [], retiring: [], on: false } },
  loopback: true, pending_approvals: 0, etag: 'etag-1' };
// dview builds a console domains view from per-network parts.
const dview = (ts, nb, extra) => ({ ...DOMAINS, ...extra, providers: {
  tailscale: { domains: [], retiring: [], on: false, ...ts }, netbird: { domains: [], retiring: [], on: false, ...nb } } });
routes['GET /console/api/console-domains'] = DOMAINS;
// Autopilot is off (Settings › Experiments; experiments_test.mjs covers it).
routes['GET /console/api/experiments/autopilot'] = { autopilot: { enabled: false, allowed: [], guide: '', guide_max: 4000 } };
// mountSettings opens a settings page: [] is Networks, ['limits'], ['networks', 'portal'] and so on.
const mountSettings = async (params = []) => {
  const main = new Element('main');
  settingsPage.mount(main, params, ctx);
  await tick();
  return main;
};
const puts = () => calls.filter((c) => c.key === 'PUT /console/api/settings');
const savebarOf = (root) => all(root, (e) => ['savebar', 's-inline-actions'].includes(e.getAttribute('class') || ''))[0];

// The left nav names the five sections; Approvals is not a settings section.
let page = await mountSettings();
const navLinks = all(all(page, (e) => e.tagName === 'NAV')[0], (e) => e.tagName === 'A');
assert.deepEqual(navLinks.map((a) => [a.textContent, a.getAttribute('href')]),
  [['Networks', '/settings/networks'], ['Console', '/settings/console'], ['Limits', '/settings/limits'], ['Agents', '/settings/agents'], ['Experiments', '/settings/experiments']]);
assert.equal(navLinks[0].getAttribute('aria-current'), 'page');
assert.equal((await mountSettings(['nowhere'])).textContent.includes('No such settings page'), true);

// Agents explains how to connect and what agents cannot do.
page = await mountSettings(['agents']);
assert.ok(page.textContent.includes('flats save-draft <slug> ./dist'));
assert.ok(page.textContent.includes('flats create-version <slug> --revision <revision> --hash <hash>'));
assert.ok(page.textContent.includes('flats deploy --flat <slug> --version <version>'));
assert.equal(page.textContent.includes('flats deploy ./dist'), false);
assert.ok(page.textContent.includes('claude mcp add --transport http flats http://127.0.0.1:7878/mcp'));
assert.ok(page.textContent.includes('Read or set keys') && page.textContent.includes('Approve requests'));
// The limits are those of the agent tools: the console has no login yet (spec rule 10).
assert.ok(page.textContent.includes('What the agent tools can’t do') && page.textContent.includes('The console itself has no login yet'));
const agentTabs = all(page, (e) => e.getAttribute('role') === 'tab');
assert.deepEqual(agentTabs.map((t) => t.textContent), ['Claude Code', 'Codex', 'Cursor', 'CLI']);
agentTabs[3].dispatch('click');
assert.ok(byId(page, 'agent-snippet').textContent.includes('flats save-draft'));
assert.equal(agentTabs[3].getAttribute('aria-selected'), 'true');
// Arrow keys, Home and End move between clients and take focus.
const tablist = all(page, (e) => e.getAttribute('role') === 'tablist')[0];
const key = (k) => tablist.listeners.keydown[0]({ key: k, preventDefault() {} });
key('ArrowRight');
assert.equal(agentTabs[0].getAttribute('aria-selected'), 'true', 'ArrowRight wraps to the first tab');
assert.equal(document.activeElement, agentTabs[0]);
key('ArrowLeft');
assert.equal(agentTabs[3].getAttribute('aria-selected'), 'true');
key('Home');
assert.ok(byId(page, 'agent-snippet').textContent.includes('claude mcp add'));
key('End');
assert.equal(agentTabs[3].getAttribute('tabindex'), '0');
assert.equal(agentTabs[0].getAttribute('tabindex'), '-1');

// Each client tab leads with the plugin and keeps registering the MCP address
// directly. Flats and its CLI are on this machine, so no tab saves a host.
const snippetOf = (tabs, i) => { tabs[i].dispatch('click'); return byId(page, 'agent-snippet').textContent; };
const claudeSnippet = snippetOf(agentTabs, 0);
assert.ok(claudeSnippet.includes('claude plugin marketplace add gosuda/flats && claude plugin install flats@flats'));
assert.ok(claudeSnippet.indexOf('claude plugin install') < claudeSnippet.indexOf('claude mcp add'));
assert.ok(claudeSnippet.includes('the flats CLI must be on the agent’s PATH'));
const codexSnippet = snippetOf(agentTabs, 1);
assert.ok(codexSnippet.includes('codex plugin marketplace add gosuda/flats && codex plugin add flats@flats'));
assert.ok(codexSnippet.includes('url = "http://127.0.0.1:7878/mcp"') && codexSnippet.includes('default_tools_approval_mode = "approve"'));
// Auto-approval is for trusted agents only, as flats mcp-config says.
assert.ok(page.textContent.includes('It does not make a version live') && page.textContent.includes('Use it only with trusted agents.'));
const cursorSnippet = snippetOf(agentTabs, 2);
assert.ok(cursorSnippet.includes('cursor-agent plugin marketplace add https://github.com/gosuda/flats'));
assert.ok(cursorSnippet.includes('Then install Flats from the Cursor plugin list.') && cursorSnippet.includes('"mcpServers"'));
// Interactive zsh treats a pasted # line as a command, not a comment: it prints
// "command not found: #", and an apostrophe in it leaves the paste at quote>.
// So no copied block holds an apostrophe, and the shell blocks (plugin and CLI)
// hold no # line. The Codex TOML block is a config file and keeps its comments.
for (const i of [0, 1, 2, 3]) {
  agentTabs[i].dispatch('click');
  const blocks = all(byId(page, 'agent-snippet'), (e) => e.tagName === 'PRE').map((pre) => pre.textContent);
  blocks.forEach((text, b) => assert.ok(!text.includes("'"), `tab ${i} block ${b} has no apostrophe`));
  const shell = i === 3 ? blocks : blocks.slice(0, 1);
  shell.forEach((text) => assert.ok(!text.split('\n').some((line) => line.trim().startsWith('#')), `tab ${i} shell block has no # line`));
}
const cliSnippet = snippetOf(agentTabs, 3);
// The plugin and the direct registration are alternatives, so each client tab
// has two blocks with a Copy button each, and neither block holds the other's text.
for (const i of [0, 1, 2]) {
  agentTabs[i].dispatch('click');
  const panel = byId(page, 'agent-snippet');
  const blocks = all(panel, (e) => e.tagName === 'PRE');
  assert.equal(blocks.length, 2, `tab ${i} has a plugin block and a direct block`);
  assert.equal(all(panel, (e) => e.tagName === 'BUTTON').length, 2, `tab ${i} has a Copy button per block`);
  assert.ok(blocks[0].textContent.includes('plugin') && !/mcp add|mcp_servers|mcpServers/.test(blocks[0].textContent), `tab ${i} plugin block`);
  assert.ok(/mcp add|mcp_servers|mcpServers/.test(blocks[1].textContent) && !blocks[1].textContent.includes('plugin'), `tab ${i} direct block`);
  // Each focusable block is named by its title.
  blocks.forEach((pre) => {
    const name = pre.getAttribute('aria-labelledby');
    assert.ok(name && name.startsWith('agent-block-'), `tab ${i} block has a name`);
    assert.ok(byId(panel, name).textContent.length > 0, `tab ${i} block name resolves`);
  });
  // Each block has its own name, so a screen reader can tell the two apart.
  const [pluginName, directName] = blocks.map((pre) => pre.getAttribute('aria-labelledby'));
  assert.notEqual(pluginName, directName, `tab ${i} blocks share a name`);
  assert.ok(byId(panel, pluginName).textContent.startsWith('Plugin'), `tab ${i} plugin block name`);
  assert.ok(byId(panel, directName).textContent.startsWith('Without the plugin'), `tab ${i} direct block name`);
}
agentTabs[3].dispatch('click');
assert.equal(all(byId(page, 'agent-snippet'), (e) => e.tagName === 'PRE').length, 1);
assert.ok(cliSnippet.startsWith('flats status\nflats save-draft'));
for (const text of [claudeSnippet, codexSnippet, cursorSnippet, cliSnippet]) assert.equal(text.includes('flats connect'), false, 'on the host, no tab saves a host');
assert.equal(page.textContent.includes('Use it only with trusted agents.'), false, 'the caveat belongs to the Codex tab');
// The check row says how to tell that the connection works.
assert.ok(page.textContent.includes('Check the connection') && page.textContent.includes('check_client'));
assert.ok(page.textContent.includes('flats status') && page.textContent.includes('flats mcp-config'));

// From another device the plugin and the CLI would still use the host saved on
// their own machine, so every tab saves this page's address first.
const agentsLocation = globalThis.location;
globalThis.location = { origin: 'https://flats.example.org', hostname: 'flats.example.org' };
page = await mountSettings(['agents']);
const remoteTabs = all(page, (e) => e.getAttribute('role') === 'tab');
for (let i = 0; i < remoteTabs.length; i++) {
  assert.ok(snippetOf(remoteTabs, i).includes('flats connect https://flats.example.org\n'), `tab ${i} saves the page address`);
}
assert.ok(snippetOf(remoteTabs, 0).includes('claude mcp add --transport http flats https://flats.example.org/mcp'));
assert.ok(snippetOf(remoteTabs, 3).startsWith('flats connect https://flats.example.org\nflats status\n'));
// With nothing saved the plugin and the CLI use http://127.0.0.1:7878, so a
// loopback console on another port or name is saved first too.
for (const origin of ['http://127.0.0.1:17878', 'http://localhost:7878']) {
  globalThis.location = { origin, hostname: new URL(origin).hostname };
  page = await mountSettings(['agents']);
  const tabs = all(page, (e) => e.getAttribute('role') === 'tab');
  for (let i = 0; i < tabs.length; i++) {
    assert.ok(snippetOf(tabs, i).includes(`flats connect ${origin}\n`), `${origin}: tab ${i} saves the page address`);
  }
  assert.ok(snippetOf(tabs, 0).includes(`claude mcp add --transport http flats ${origin}/mcp`));
  snippetOf(tabs, 3);
  assert.ok(page.textContent.includes('The CLI uses the host saved with flats connect, or http://127.0.0.1:7878 when none is saved.'), `${origin}: CLI note names the default`);
}
globalThis.location = agentsLocation;

// Host values are read only, with their sources, on the Console page.
page = await mountSettings(['console']);
const configCard = byId(page, 'configuration');
for (const text of ['Console address', '127.0.0.1:7878', 'flag for this run', 'Private routes',
  'flats config set', 'apply after Flats restarts']) {
  assert.ok(configCard.textContent.includes(text), `configuration lacks ${text}: ${configCard.textContent}`);
}
assert.equal(all(configCard, (e) => ['INPUT', 'TEXTAREA', 'SELECT', 'BUTTON'].includes(e.tagName)).length, 0);
assert.equal(page.textContent.includes('config.json changed on disk'), false);
assert.equal(configCard.textContent.includes('flats install'), false);

// A file changed on disk and legacy mode are both explained.
routes['GET /console/api/settings'] = settingsRoute({ ...CONFIG, mode: 'legacy', changed_on_disk: true });
page = await mountSettings(['console']);
assert.ok(page.textContent.includes('config.json changed on disk'));
assert.ok(page.textContent.includes('Restart Flats to load it'));
assert.ok(byId(page, 'configuration').textContent.includes('flats install'));

// Limits stay editable and say where a non-default value is set; the
// Portal relays pinned by a flag cannot be changed and say why.
routes['GET /console/api/settings'] = settingsRoute(CONFIG);
page = await mountSettings(['limits']);
assert.deepEqual(all(page, (e) => e.tagName === 'H3').map((e) => e.textContent), ['Storage', 'History', 'Traffic and addresses']);
assert.ok(byId(page, 'set-version_quota-help').textContent.includes('Set in config.json.'));
assert.equal(byId(page, 'set-events_keep-help').textContent.includes('Set in'), false, 'a default value names no source');
assert.equal(byId(page, 'set-version_quota').getAttribute('disabled'), null);
assert.equal(byId(page, 'set-portal_relays'), undefined, 'Portal settings live on the Portal page');
assert.equal(savebarOf(page).getAttribute('hidden'), '', 'the save bar waits for a change');
// Staging a change shows the save bar with a count and what changes.
byId(page, 'set-redirect_days').value = '9';
byId(page, 'limits-form').dispatch('input');
assert.equal(savebarOf(page).getAttribute('hidden'), null);
assert.ok(savebarOf(page).textContent.includes('1 unsaved change') && savebarOf(page).textContent.includes('Rename redirect: 7 days → 9 days'), savebarOf(page).textContent);
// An invalid value blocks saving and says which field to fix.
byId(page, 'set-version_quota').value = '2.5';
byId(page, 'limits-form').dispatch('input');
assert.ok(savebarOf(page).textContent.includes('2 unsaved changes') && savebarOf(page).textContent.includes('Fix Versions before saving.'));
assert.equal(byId(page, 'limits-save').disabled, true);
assert.equal(byId(page, 'set-version_quota').getAttribute('aria-invalid'), 'true');
// Discard restores every staged value.
byText(savebarOf(page), 'Discard')[0].dispatch('click');
assert.equal(byId(page, 'set-redirect_days').value, '7');
assert.equal(byId(page, 'set-version_quota').value, '20');
assert.equal(byId(page, 'set-version_quota').getAttribute('aria-invalid'), null);
assert.equal(savebarOf(page).getAttribute('hidden'), '');
page = await mountSettings(['networks', 'portal']);
const relaysInput = byId(page, 'set-portal_relays');
assert.equal(relaysInput.getAttribute('disabled'), '');
assert.ok(relaysInput.getAttribute('aria-describedby').split(' ').includes('set-portal_relays-pin'));
assert.ok(byId(page, 'set-portal_relays-pin').textContent.includes("Set by the service's --relays flag"));

// A refused save keeps the operator's input and says why; the page then
// learns that the file changed.
page = await mountSettings(['limits']);
let form = byId(page, 'limits-form');
byId(page, 'set-upload_max_bytes').value = '30';
routes['GET /console/api/settings'] = settingsRoute({ ...CONFIG, changed_on_disk: true });
routes['PUT /console/api/settings'] = { __status: 412, category: 'config_changed', error: 'config.json changed on disk since Flats loaded it; restart Flats to load the file, then retry' };
form.dispatch('submit');
await tick();
let put = puts().at(-1);
assert.equal(put.headers['If-Match'], '"etag-1"');
assert.deepEqual(JSON.parse(put.body), { upload_max_bytes: String(30 * 1048576) });
assert.equal(byId(page, 'set-upload_max_bytes').value, '30');
let alert = all(page, (e) => e.getAttribute('role') === 'alert' && e.textContent.includes('Settings not saved'))[0];
assert.ok(alert && alert.textContent.includes('restart Flats') && alert.textContent.includes('still in the form'));
assert.ok(page.textContent.includes('config.json changed on disk'), 'the notice must appear after a 412');

// A server error that names a setting is tied to its field.
routes['PUT /console/api/settings'] = { __status: 400, category: 'invalid', error: 'config: system.upload_max_bytes: 0 is out of range (use an integer from 1 to 9007199254740991)' };
byId(page, 'set-upload_max_bytes').value = '0';
form.dispatch('submit');
await tick();
const upload = byId(page, 'set-upload_max_bytes');
assert.equal(upload.getAttribute('aria-invalid'), 'true');
assert.ok(upload.getAttribute('aria-describedby').split(' ').includes('set-upload_max_bytes-error'));
assert.ok(byId(page, 'set-upload_max_bytes-error').textContent.includes('out of range'));

// Lowering version quota never enters destructive retention confirmation.
routes['GET /console/api/settings'] = settingsRoute(CONFIG);
routes['PUT /console/api/settings'] = { settings: SETTINGS, applied: ['version_quota'], restart_required: [] };
page = await mountSettings(['limits']);
form = byId(page, 'limits-form');
byId(page, 'set-version_quota').value = '1.5';
let quotaPutCount = puts().length;
form.dispatch('submit'); await tick();
assert.equal(puts().length, quotaPutCount);
assert.equal(byId(page, 'set-version_quota').getAttribute('aria-invalid'), 'true');
byId(page, 'set-version_quota').value = '1';
let quotaImpactCount = calls.filter((c) => c.key === 'POST /console/api/settings/impact').length;
form.dispatch('submit'); await tick();
assert.equal(puts().length, quotaPutCount + 1);
assert.deepEqual(JSON.parse(puts().at(-1).body), { version_quota: '1' });
assert.equal(calls.filter((c) => c.key === 'POST /console/api/settings/impact').length, quotaImpactCount);
assert.equal(all(page, (e) => e.className === 'impact').length, 0);

// Lowering a retention setting first shows what the next pruning removes
// and saves only after a second, explicit confirmation in the save bar.
routes['GET /console/api/settings'] = settingsRoute(CONFIG);
page = await mountSettings(['limits']);
form = byId(page, 'limits-form');
routes['POST /console/api/settings/impact'] = { impact: { events_keep: { current: '5000', candidate: '2', total: 3, flats: [{ slug: 'blog', count: 3,  }] } } };
routes['PUT /console/api/settings'] = { settings: SETTINGS, applied: ['events_keep'], restart_required: ['portal_max_relays'], etag: 'etag-2' };
byId(page, 'set-events_keep').value = '2';
let putCount = puts().length;
form.dispatch('submit');
await tick();
const impactCall = calls.filter((c) => c.key === 'POST /console/api/settings/impact').at(-1);
assert.deepEqual(JSON.parse(impactCall.body), { events_keep: '2' });
assert.equal(puts().length, putCount, 'a decrease must not save before the confirmation');
let panel = all(page, (e) => e.className === 'impact')[0];
assert.ok(all(savebarOf(page), (e) => e === panel).length === 1, 'the confirmation sits in the save bar');
assert.ok(panel.textContent.includes('Saving removes data'));
assert.ok(panel.textContent.includes('Log events: 5000 → 2 events.'));
assert.ok(panel.textContent.includes('blog: 3 events'));
// Keep editing closes the panel without saving.
byText(panel, 'Keep editing')[0].dispatch('click');
await tick();
assert.equal(all(page, (e) => e.className === 'impact').length, 0);
assert.equal(puts().length, putCount);
// An edit while the panel is open withdraws it.
form.dispatch('submit');
await tick();
assert.equal(all(page, (e) => e.className === 'impact').length, 1);
form.dispatch('input');
assert.equal(all(page, (e) => e.className === 'impact').length, 0);
form.dispatch('submit');
await tick();
panel = all(page, (e) => e.className === 'impact')[0];
byText(panel, 'Save and remove')[0].dispatch('click');
await tick();
await tick();
put = puts().at(-1);
assert.equal(puts().length, putCount + 1);
assert.equal(put.headers['If-Match'], '"etag-1"');
assert.deepEqual(JSON.parse(put.body), { events_keep: '2' });
const limits = byId(page, 'limits-form');
assert.ok(limits.textContent.includes('Log events: saved, applied now.'), limits.textContent);
assert.ok(limits.textContent.includes('Active relays: saved, applies after Flats restarts.'));

// A decrease that removes nothing still asks once, with a plain Save.
routes['POST /console/api/settings/impact'] = { impact: { events_keep: { current: '5000', candidate: '100', total: 0, flats: [] } } };
page = await mountSettings(['limits']);
form = byId(page, 'limits-form');
byId(page, 'set-events_keep').value = '100';
putCount = puts().length;
form.dispatch('submit');
await tick();
panel = all(page, (e) => e.className === 'impact')[0];
assert.ok(panel.textContent.includes('Nothing is removed now.'));
assert.equal(puts().length, putCount);
byText(panel, 'Save')[0].dispatch('click');
await tick();
assert.equal(puts().length, putCount + 1);

// An edit while the impact request is in flight drops its late answer, so
// the older value can never be confirmed and saved.
const IMPACT_2 = { impact: { events_keep: { current: '5000', candidate: '2', total: 3, flats: [{ slug: 'blog', count: 3,  }] } } };
let release;
routes['POST /console/api/settings/impact'] = () => new Promise((resolve) => { release = () => resolve(IMPACT_2); });
page = await mountSettings(['limits']);
form = byId(page, 'limits-form');
byId(page, 'set-events_keep').value = '2';
putCount = puts().length;
form.dispatch('submit');
await tick();
assert.equal(typeof release, 'function', 'the impact request must be pending');
byId(page, 'set-events_keep').value = '9';
form.dispatch('input');
release();
await tick();
assert.equal(all(page, (e) => e.className === 'impact').length, 0, 'a late impact answer must not show');
assert.equal(puts().length, putCount);
assert.equal(byId(page, 'set-events_keep').value, '9');

// The confirmation saves only the values it described.
routes['POST /console/api/settings/impact'] = IMPACT_2;
page = await mountSettings(['limits']);
form = byId(page, 'limits-form');
byId(page, 'set-events_keep').value = '2';
form.dispatch('submit');
await tick();
panel = all(page, (e) => e.className === 'impact')[0];
byId(page, 'set-events_keep').value = '9'; // changed without an input event
byText(panel, 'Save and remove')[0].dispatch('click');
await tick();
assert.equal(puts().length, putCount);
assert.equal(all(page, (e) => e.className === 'impact').length, 0);
assert.ok(page.textContent.includes('The form changed after the preview.'));
assert.equal(byId(page, 'set-events_keep').value, '9');

// Raising a limit saves at once.
page = await mountSettings(['limits']);
form = byId(page, 'limits-form');
byId(page, 'set-version_quota').value = '30';
const impactCount = calls.filter((c) => c.key === 'POST /console/api/settings/impact').length;
putCount = puts().length;
form.dispatch('submit');
await tick();
assert.equal(calls.filter((c) => c.key === 'POST /console/api/settings/impact').length, impactCount);
assert.equal(puts().length, putCount + 1);
assert.equal(settingsPage.isDecrease('events_keep', '100', '5'), true);
assert.equal(settingsPage.isDecrease('events_keep', '5', '100'), false);
// Revision and pin quotas are counts: a fraction is refused, not rounded.
page = await mountSettings(['limits']);
byId(page, 'set-revision_quota').value = '40.5';
putCount = puts().length;
byId(page, 'limits-form').dispatch('submit');
await tick();
assert.equal(puts().length, putCount);
assert.ok(byId(page, 'set-revision_quota-error').textContent.includes('Revisions must be a whole number of 0 or more.'));
const withRevisions = (n) => ({ ...settingsRoute(CONFIG), settings: { ...SETTINGS, revision_quota: n } });
// Lowering the revision quota deletes revisions at once, so it asks first,
// in words: the server cannot count them in advance.
assert.equal(settingsPage.revisionsDrop('0', '40'), true, '0 keeps all revisions');
assert.equal(settingsPage.revisionsDrop(undefined, '40'), true);
assert.equal(settingsPage.revisionsDrop('100', '40'), true);
assert.equal(settingsPage.revisionsDrop('40', '100'), false);
assert.equal(settingsPage.revisionsDrop('40', '0'), false, 'keeping all deletes nothing');
routes['PUT /console/api/settings'] = { settings: SETTINGS, applied: ['revision_quota'], restart_required: [] };
page = await mountSettings(['limits']);
byId(page, 'set-revision_quota').value = '40';
putCount = puts().length;
let impactCalls = calls.filter((c) => c.key === 'POST /console/api/settings/impact').length;
byId(page, 'limits-form').dispatch('submit');
await tick();
assert.equal(puts().length, putCount, 'a revision quota drop must not save before the confirmation');
assert.equal(calls.filter((c) => c.key === 'POST /console/api/settings/impact').length, impactCalls, 'the server has no revision count to ask for');
panel = all(page, (e) => e.className === 'impact')[0];
assert.ok(panel.textContent.includes('Saving removes data') && panel.textContent.includes('Revisions: all → 40 revisions.'));
assert.ok(panel.textContent.includes('Pinned revisions and flats with their own limit are kept'));
byText(panel, 'Save and remove')[0].dispatch('click');
await tick();
assert.deepEqual(JSON.parse(puts().at(-1).body), { revision_quota: '40' });
// Raising it, or going back to keeping all, saves at once.
routes['GET /console/api/settings'] = withRevisions('100');
page = await mountSettings(['limits']);
byId(page, 'set-revision_quota').value = '0';
putCount = puts().length;
byId(page, 'limits-form').dispatch('submit');
await tick();
assert.equal(puts().length, putCount + 1);
assert.equal(all(page, (e) => e.className === 'impact').length, 0);
// With log events too, one confirmation covers both.
routes['POST /console/api/settings/impact'] = { impact: { events_keep: { current: '5000', candidate: '2', total: 3, flats: [{ slug: 'blog', count: 3 }] } } };
page = await mountSettings(['limits']);
byId(page, 'set-revision_quota').value = '10';
byId(page, 'set-events_keep').value = '2';
byId(page, 'limits-form').dispatch('submit');
await tick();
panel = all(page, (e) => e.className === 'impact')[0];
assert.ok(panel.textContent.includes('Log events: 5000 → 2 events.') && panel.textContent.includes('Revisions: 100 → 10 revisions.'));
putCount = puts().length;
byText(panel, 'Save and remove')[0].dispatch('click');
await tick();
assert.equal(puts().length, putCount + 1);
assert.deepEqual(JSON.parse(puts().at(-1).body), { events_keep: '2', revision_quota: '10' }, 'the one confirmation saves both');
// Raising it saves at once.
page = await mountSettings(['limits']);
byId(page, 'set-revision_quota').value = '200';
putCount = puts().length;
byId(page, 'limits-form').dispatch('submit');
await tick();
assert.equal(puts().length, putCount + 1);
assert.deepEqual(JSON.parse(puts().at(-1).body), { revision_quota: '200' });
routes['GET /console/api/settings'] = settingsRoute(CONFIG);

// Byte sizes show the stored count exactly, in the largest unit that
// converts back to it, with a unit menu and the exact byte count.
assert.deepEqual(settingsPage.byteValue('32212254720', 'GB'), { unit: 'GB', value: '30' });
assert.deepEqual(settingsPage.byteValue('1610612736', 'GB'), { unit: 'GB', value: '1.5' });
assert.deepEqual(settingsPage.byteValue('107374', 'GB'), { unit: 'bytes', value: '107374' });
assert.deepEqual(settingsPage.byteValue('1073742', 'GB'), { unit: 'bytes', value: '1073742' });
assert.deepEqual(settingsPage.byteValue('1395864371', 'GB'), { unit: 'bytes', value: '1395864371' });
assert.deepEqual(settingsPage.byteValue('3221225472000', 'GB'), { unit: 'GB', value: '3000' });
assert.deepEqual(settingsPage.byteValue('9007199254740991', 'GB'), { unit: 'bytes', value: '9007199254740991' });
assert.deepEqual(settingsPage.byteValue('9007199254740992', 'GB'), { unit: 'GB', value: '' });
assert.deepEqual(settingsPage.byteValue('1536', 'GB'), { unit: 'KB', value: '1.5' });
assert.deepEqual(settingsPage.byteValue('1', 'GB'), { unit: 'bytes', value: '1' });
assert.deepEqual(settingsPage.byteValue('0', 'GB'), { unit: 'GB', value: '0' });
assert.deepEqual(settingsPage.byteValue('', 'MB'), { unit: 'MB', value: '' });
assert.equal(settingsPage.toBase('104.857421875', 1024, false), 107374);
assert.equal(settingsPage.toBase('0.0', 1 << 30, false), 0);
assert.equal(settingsPage.toBase('1.5', 1, true), null);
assert.equal(settingsPage.toBase('-1', 1, false), null);
assert.equal(settingsPage.toBase('', 1, false), null);
assert.equal(settingsPage.toBase('1e300', 1 << 30, false), null);
assert.equal(settingsPage.toBase('1.0000000000000001', 1, true), null, 'a fraction of a byte is refused even below float precision');
assert.equal(settingsPage.toBase('9007199254740991.1', 1, true), null);
assert.equal(settingsPage.toBase('9007199254740991', 1, true), 9007199254740991);
assert.equal(settingsPage.toBase('9007199254740992', 1, true), null);
assert.equal(settingsPage.toBase('8388607.999999999', 1 << 30, false), 9007199254740991);
assert.equal(settingsPage.toBase('8388608', 1 << 30, false), null);
assert.equal(settingsPage.toBase('1.5e3', 1, true), 1500);
assert.equal(settingsPage.toBase('15e-1', 1, true), null);
assert.equal(settingsPage.toBase('1e-500', 1, false), null);
assert.equal(settingsPage.toBase('.5', 1 << 20, false), 524288);
assert.equal(settingsPage.toBase('0.0000001', 1 << 30, false), 107, 'rounds half up to whole bytes');
assert.equal(settingsPage.toBase('1.', 1, true), 1);
assert.equal(settingsPage.toBase('.', 1, true), null);
assert.equal(settingsPage.toBase('-0', 1, true), null);
assert.equal(settingsPage.toBase('abc', 1, true), null);
assert.equal(settingsPage.toBase('1.5', 1, true), null);
assert.equal(settingsPage.toBase('2.5', 3, false), 8);

const withSettings = (extra, config = CONFIG) => ({ ...settingsRoute(config), settings: { ...SETTINGS, ...extra } });
const submitLimits = async (p) => { byId(p, 'limits-form').dispatch('submit'); await tick(); };
routes['PUT /console/api/settings'] = { settings: SETTINGS, applied: ['disk_quota_bytes'], restart_required: [] };

// External origins are staged, validated and saved with the config ETag.
routes['PUT /console/api/settings'] = () => {
  const settings = { external_origins: JSON.parse(puts().at(-1).body).external_origins };
  routes['GET /console/api/settings'] = withSettings(settings);
  return { applied: ['external_origins'], restart_required: [] };
};
routes['GET /console/api/settings'] = withSettings({ external_origins: 'https://flats.example.org' });
page = await mountSettings(['console']);
let originsForm = byId(page, 'external-origins-form');
let originsInput = byId(page, 'set-external_origins');
assert.equal(originsInput.value, 'https://flats.example.org');
assert.ok(page.textContent.includes('Flats has no built-in login yet'));
assert.ok(!savebarOf(originsForm).hidden);
assert.equal(byId(page, 'external-origins-save').disabled, true);
assert.equal(byText(originsForm, 'Discard')[0].disabled, true);
for (const invalid of ['https://flats.example.org/', 'https://UPPER.example.org', 'https://127.0.0.1', 'https://127.1', 'https://0x7f000001', 'https://localhost', 'https://app.localhost', 'https://[::1]', 'https://user@example.org', 'https://example.org?a=1', 'https://example.org#x', 'https://example.org:65536', 'https://example.org:080', 'https://flats.example.org\nhttp://flats.example.org', 'https://flats.example.org\nhttps://flats.example.org:443']) {
  originsInput.value = invalid;
  originsForm.dispatch('input');
  assert.equal(originsInput.getAttribute('aria-invalid'), 'true', invalid);
  assert.equal(byId(page, 'external-origins-save').disabled, true, invalid);
}
byId(page, 'console-preferred').value = 'local';
byId(page, 'links-form').dispatch('change');
const beforeOrigins = puts().length;
originsForm.dispatch('submit');
await tick();
assert.equal(puts().length, beforeOrigins, 'invalid origins never reach the API');
originsInput.value = 'https://flats.example.org\nhttps://other.example.org:8443';
originsForm.dispatch('input');
assert.equal(originsInput.getAttribute('aria-invalid'), null);
assert.equal(byId(page, 'external-origins-save').disabled, false);
assert.equal(puts().length, beforeOrigins, 'typing only stages the changes');
assert.equal(all(page, (e) => e.className === 's-inline-actions' && !e.hidden).length, 2, 'both dirty console forms have visible inline actions');
assert.equal(byId(page, 'links-save').disabled, false);
byText(originsForm, 'Discard')[0].dispatch('click');
assert.equal(originsInput.value, 'https://flats.example.org');
assert.ok(!savebarOf(originsForm).hidden);
assert.equal(byId(page, 'external-origins-save').disabled, true);
assert.equal(byText(originsForm, 'Discard')[0].disabled, true);
originsInput.value = 'https://flats.example.org\nhttps://other.example.org:8443';
originsForm.dispatch('input');
originsForm.dispatch('submit');
await tick();
assert.equal(JSON.parse(puts().at(-1).body).external_origins, 'https://flats.example.org,https://other.example.org:8443');
assert.equal(puts().at(-1).headers['If-Match'], '"etag-1"');
assert.equal(byId(page, 'set-external_origins').value, 'https://flats.example.org\nhttps://other.example.org:8443', 'reload renders saved origins on separate lines');
assert.equal(byId(page, 'console-preferred').value, 'local', 'saving origins retains unsaved preferred-link edits');
assert.ok(byId(page, 'external-origins-form').textContent.includes('External origins: saved, applied now.'));
assert.ok(!byId(page, 'links-form').textContent.includes('saved, applied now.'), 'the links form does not duplicate the origins success notice');
byId(page, 'links-form').dispatch('submit');
await tick();
assert.ok(byId(page, 'links-form').textContent.includes('Address for links: saved, applied now.'));
assert.ok(!byId(page, 'external-origins-form').textContent.includes('saved, applied now.'), 'the origins form does not duplicate the links success notice');
// Clearing the field removes the configured entries; an empty list is valid.
page = await mountSettings(['console']);
originsForm = byId(page, 'external-origins-form');
originsInput = byId(page, 'set-external_origins');
originsInput.value = '';
originsForm.dispatch('input');
originsForm.dispatch('submit');
await tick();
assert.equal(JSON.parse(puts().at(-1).body).external_origins, '');
assert.equal(byId(page, 'set-external_origins').value, '');
// Removing the origin serving this page requires confirmation, including
// default-port aliases. Cancel and edits during confirmation never submit.
const originTestLocation = globalThis.location;
globalThis.location = { origin: 'https://flats.example.org' };
routes['GET /console/api/settings'] = withSettings({ external_origins: 'https://flats.example.org:443,https://other.example.org' });
page = await mountSettings(['console']);
originsForm = byId(page, 'external-origins-form');
originsInput = byId(page, 'set-external_origins');
originsInput.value = 'https://other.example.org';
originsForm.dispatch('input');
const beforeRemoval = puts().length;
originsForm.dispatch('submit');
await tick();
let removalDialog = all(document.body, (e) => e.tagName === 'DIALOG')[0];
assert.ok(removalDialog.textContent.includes('will stop working'));
assert.ok(removalDialog.textContent.includes('has not verified'));
removalDialog.close('cancel');
await tick();
assert.equal(puts().length, beforeRemoval);
assert.equal(originsInput.value, 'https://other.example.org');
originsForm.dispatch('submit');
await tick();
removalDialog = all(document.body, (e) => e.tagName === 'DIALOG')[0];
originsInput.value = '';
originsForm.dispatch('input');
removalDialog.close('ok');
await tick();
assert.equal(puts().length, beforeRemoval, 'stale confirmation cannot save');
assert.ok(originsForm.textContent.includes('save again'));
originsForm.dispatch('submit');
await tick();
removalDialog = all(document.body, (e) => e.tagName === 'DIALOG')[0];
assert.ok(removalDialog.textContent.includes('local console'));
removalDialog.close('ok');
await tick();
assert.equal(puts().length, beforeRemoval + 1);
assert.equal(JSON.parse(puts().at(-1).body).external_origins, '');
// Comma-separated paste is accepted and shown one per line after saving.
originsForm = byId(page, 'external-origins-form');
originsInput = byId(page, 'set-external_origins');
originsInput.value = 'https://first.example.org, https://second.example.org';
originsForm.dispatch('input');
originsForm.dispatch('submit');
await tick();
assert.equal(byId(page, 'set-external_origins').value, 'https://first.example.org\nhttps://second.example.org');
globalThis.location = originTestLocation;
routes['PUT /console/api/settings'] = { settings: SETTINGS, applied: ['disk_quota_bytes'], restart_required: [] };
routes['GET /console/api/settings'] = settingsRoute(CONFIG);

// Pending network refreshes must preserve dirty inline forms even when
// focus is outside a field; clean inline actions must allow refreshes.
const originalSetTimeout = globalThis.setTimeout;
const originalClearTimeout = globalThis.clearTimeout;
let scheduledRefresh;
globalThis.setTimeout = (fn, delay, ...args) => {
  if (delay !== 5000) return originalSetTimeout(fn, delay, ...args);
  const timer = { fn, unref() {} };
  scheduledRefresh = timer;
  return timer;
};
globalThis.clearTimeout = (timer) => {
  if (timer?.fn) { if (scheduledRefresh === timer) scheduledRefresh = null; }
  else originalClearTimeout(timer);
};
let stopRefresh;
try {
  routes['GET /console/api/console-domains'] = dview({ on: true, domains: [{ name: 'pending', state: 'starting' }] });
  const refreshPage = new Element('main');
  stopRefresh = settingsPage.mount(refreshPage, ['console'], ctx);
  await tick();
  document.activeElement = document.body;
  const fireRefresh = async () => { const timer = scheduledRefresh; assert.ok(timer); scheduledRefresh = null; timer.fn(); await tick(); };
  const reads = () => calls.filter((c) => c.key === 'GET /console/api/settings').length;
  let beforeReads = reads();
  await fireRefresh();
  assert.equal(reads(), beforeReads + 1, 'clean inline forms still refresh');
  const dirtyInput = byId(refreshPage, 'set-external_origins');
  dirtyInput.value = 'https://staged.example.org';
  byId(refreshPage, 'external-origins-form').dispatch('input');
  routes['GET /console/api/settings'] = { __status: 503, error: 'temporary failure' };
  beforeReads = reads();
  await fireRefresh();
  assert.equal(reads(), beforeReads, 'dirty origins suppress the refresh request');
  assert.equal(byId(refreshPage, 'set-external_origins'), dirtyInput, 'dirty form is not redrawn');
  byText(byId(refreshPage, 'external-origins-form'), 'Discard')[0].dispatch('click');
  byId(refreshPage, 'console-preferred').value = 'local';
  byId(refreshPage, 'links-form').dispatch('change');
  await fireRefresh();
  assert.equal(reads(), beforeReads, 'dirty preferred links also suppress refresh');
  byText(byId(refreshPage, 'links-form'), 'Discard')[0].dispatch('click');
  // If editing starts after the request, even a failed response must not
  // replace the form or erase its validation warning.
  let finishSettings;
  routes['GET /console/api/settings'] = () => new Promise((resolve) => { finishSettings = resolve; });
  await fireRefresh();
  assert.equal(reads(), beforeReads + 1);
  dirtyInput.value = 'bad-origin';
  byId(refreshPage, 'external-origins-form').dispatch('input');
  finishSettings({ __status: 503, error: 'temporary failure' });
  await tick();
  assert.equal(byId(refreshPage, 'set-external_origins'), dirtyInput);
  assert.equal(dirtyInput.getAttribute('aria-invalid'), 'true');
  assert.ok(byId(refreshPage, 'external-origins-form').textContent.includes('Address 1:'));
} finally {
  stopRefresh?.();
  globalThis.setTimeout = originalSetTimeout;
  globalThis.clearTimeout = originalClearTimeout;
  routes['GET /console/api/settings'] = settingsRoute(CONFIG);
  routes['GET /console/api/console-domains'] = DOMAINS;
}

// A small quota is shown exactly instead of rounding to "0", and typing 0
// turns the quota off.
routes['GET /console/api/settings'] = withSettings({ disk_quota_bytes: '107374' });
page = await mountSettings(['limits']);
let disk = byId(page, 'set-disk_quota_bytes');
let diskUnit = byId(page, 'set-disk_quota_bytes-unit');
assert.equal(disk.value, '107374');
assert.equal(diskUnit.tagName, 'SELECT');
assert.equal(diskUnit.value, 'bytes');
assert.deepEqual(all(diskUnit, (e) => e.tagName === 'OPTION').map((o) => o.getAttribute('value')), ['GB', 'MB', 'KB', 'bytes']);
assert.equal(byId(page, 'set-disk_quota_bytes-exact').textContent, 'Stores 107,374 bytes.');
assert.ok(disk.getAttribute('aria-describedby').split(' ').includes('set-disk_quota_bytes-exact'));
assert.ok(byId(page, 'set-disk_quota_bytes-help').textContent.includes('0 means no quota. Default: 30 GB.'));
putCount = puts().length;
disk.value = '0';
disk.dispatch('input');
assert.equal(byId(page, 'set-disk_quota_bytes-exact').textContent, 'Stores 0 bytes.');
await submitLimits(page);
assert.equal(puts().length, putCount + 1);
assert.deepEqual(JSON.parse(puts().at(-1).body), { disk_quota_bytes: '0' });

// An untouched small quota is not resent when another setting changes.
page = await mountSettings(['limits']);
byId(page, 'set-redirect_days').value = '8';
await submitLimits(page);
assert.deepEqual(JSON.parse(puts().at(-1).body), { redirect_days: '8' });

// Any spelling of 0 over a quota that is already off changes nothing.
routes['GET /console/api/settings'] = withSettings({ disk_quota_bytes: '0' });
for (const typed of ['0', '0.0', '0.000']) {
  page = await mountSettings(['limits']);
  assert.equal(byId(page, 'set-disk_quota_bytes').value, '0');
  assert.equal(byId(page, 'set-disk_quota_bytes-unit').value, 'GB');
  byId(page, 'set-disk_quota_bytes').value = typed;
  putCount = puts().length;
  await submitLimits(page);
  assert.equal(puts().length, putCount, `${typed} must not save`);
  assert.ok(byId(page, 'limits-form').textContent.includes('Nothing changed.'));
}

// The unit menu sets what the number means; bytes take whole numbers.
page = await mountSettings(['limits']);
disk = byId(page, 'set-disk_quota_bytes');
diskUnit = byId(page, 'set-disk_quota_bytes-unit');
disk.value = '500';
diskUnit.value = 'bytes';
diskUnit.dispatch('change');
assert.equal(byId(page, 'set-disk_quota_bytes-exact').textContent, 'Stores 500 bytes.');
putCount = puts().length;
await submitLimits(page);
assert.deepEqual(JSON.parse(puts().at(-1).body), { disk_quota_bytes: '500' });
page = await mountSettings(['limits']);
disk = byId(page, 'set-disk_quota_bytes');
diskUnit = byId(page, 'set-disk_quota_bytes-unit');
diskUnit.value = 'bytes';
disk.value = '1.5';
await submitLimits(page);
assert.equal(puts().length, putCount + 1, 'a fraction of a byte must not save');
assert.equal(disk.getAttribute('aria-invalid'), 'true');
assert.ok(byId(page, 'set-disk_quota_bytes-error').textContent.includes('a whole number of 0 or more'));
diskUnit.value = 'MB';
await submitLimits(page);
assert.deepEqual(JSON.parse(puts().at(-1).body), { disk_quota_bytes: String(1.5 * 1048576) });
// Changing only the unit is an edit too.
routes['GET /console/api/settings'] = withSettings({ disk_quota_bytes: '1024' });
page = await mountSettings(['limits']);
assert.equal(byId(page, 'set-disk_quota_bytes').value, '1');
assert.equal(byId(page, 'set-disk_quota_bytes-unit').value, 'KB');
putCount = puts().length;
byId(page, 'set-disk_quota_bytes-unit').value = 'MB';
await submitLimits(page);
assert.equal(puts().length, putCount + 1);
assert.deepEqual(JSON.parse(puts().at(-1).body), { disk_quota_bytes: '1048576' });

// A pinned byte size cannot change its unit either.
routes['GET /console/api/settings'] = withSettings({}, { ...CONFIG, pinned: { ...CONFIG.pinned, disk_quota_bytes: 'a test flag' } });
page = await mountSettings(['limits']);
assert.equal(byId(page, 'set-disk_quota_bytes-unit').disabled, true);
assert.equal(byId(page, 'set-disk_quota_bytes').getAttribute('disabled'), '');
routes['GET /console/api/settings'] = settingsRoute(CONFIG);

// The Portal settings save with the Portal page's save bar.
routes['GET /console/api/settings'] = settingsRoute({ ...CONFIG, pinned: {} });
routes['PUT /console/api/settings'] = { settings: SETTINGS, applied: [], restart_required: ['portal_max_relays'], etag: 'etag-3' };
page = await mountSettings(['networks', 'portal']);
const portalForm = byId(page, 'portal-form');
assert.ok(portalForm, 'the Portal page holds the Portal settings');
assert.ok(byId(page, 'row-portal_max_relays').textContent.includes('Applies after restart'), 'a restart-only setting is tagged');
byId(page, 'set-portal_max_relays').value = '5';
putCount = puts().length;
portalForm.dispatch('submit');
await tick();
assert.equal(puts().length, putCount + 1);
assert.deepEqual(JSON.parse(puts().at(-1).body), { portal_max_relays: '5' });
page = await mountSettings(['networks', 'portal']);
const hideBox = byId(page, 'set-portal_hide');
assert.equal(hideBox.getAttribute('role'), 'switch');
assert.ok(hideBox && byId(page, 'set-portal_hide-help').textContent.includes('not access control'));
hideBox.checked = true;
byId(page, 'portal-form').dispatch('change');
assert.ok(savebarOf(page).textContent.includes('Hide public flats from relay listings: Off → On'));
putCount = puts().length;
byId(page, 'portal-form').dispatch('submit');
await tick();
assert.equal(puts().length, putCount + 1);
assert.deepEqual(JSON.parse(puts().at(-1).body), { portal_hide: 'true' });

// Networks lists new-flat defaults, then Private and Public providers, each
// a row that opens its own page.
routes['GET /console/api/settings'] = settingsRoute({ ...CONFIG, etag: 'etag-9', pinned: {} });
routes['PUT /console/api/providers/tailscale'] = { providers: [] };
const settingsMain = await mountSettings();
assert.deepEqual(all(settingsMain, (e) => e.tagName === 'H3').map((e) => e.textContent), ['New flats', 'Private', 'Public']);
assert.deepEqual(all(byId(settingsMain, 'providers-private'), (e) => e.tagName === 'A').map((e) => e.getAttribute('id')), ['provider-local', 'provider-tailscale']);
assert.deepEqual(all(byId(settingsMain, 'providers-public'), (e) => e.tagName === 'A').map((e) => e.getAttribute('id')),
  ['provider-tailscale-funnel', 'provider-portal', 'provider-zrok', 'provider-cloudflare-quick']);
assert.equal(byId(settingsMain, 'provider-portal').getAttribute('href'), '/settings/networks/portal');
assert.ok(byId(settingsMain, 'provider-local').textContent.includes('Always on') && byId(settingsMain, 'provider-local').textContent.includes('Default'));
assert.ok(byId(settingsMain, 'provider-portal').textContent.includes('Allowed on 1 flat'));
assert.ok(byId(settingsMain, 'provider-tailscale').textContent.includes('Off'));
assert.equal(all(settingsMain, (e) => e.tagName === 'BUTTON' && /^Turn (on|off)$/.test(e.textContent)).length, 0, 'switches live on each network’s page');
// Only providers that are on can be a default; choosing one saves with the bar.
const pubDefault = byId(settingsMain, 'set-default_public_provider');
assert.deepEqual(all(pubDefault, (e) => e.tagName === 'OPTION').map((o) => o.getAttribute('value')), ['', 'portal', 'zrok', 'cloudflare-quick']);
pubDefault.value = 'portal';
putCount = puts().length;
byId(settingsMain, 'defaults-form').dispatch('submit');
await tick();
assert.deepEqual(JSON.parse(puts().at(-1).body), { default_public_provider: 'portal' });
assert.equal(puts().at(-1).headers['If-Match'], '"etag-9"');
// A provider page turns it on, as an explicit host request.
let tsPage = await mountSettings(['networks', 'tailscale']);
assert.ok(tsPage.textContent.includes('Each flat and preview gets its own tailnet node'));
byText(tsPage, 'Turn on')[0].dispatch('click');
await tick();
const turnOn = all(document.body, (e) => e.tagName === 'DIALOG').pop();
assert.ok(turnOn.textContent.includes('Turn on Tailscale?'));
turnOn.close('ok');
await tick();
const turnOnCall = calls.find((c) => c.key === 'PUT /console/api/providers/tailscale');
assert.deepEqual(JSON.parse(turnOnCall.body), { enabled: true });
assert.equal(turnOnCall.headers['If-Match'], '"etag-9"', 'a provider change is guarded by the settings ETag');
// Local has no switch.
assert.equal(all(await mountSettings(['networks', 'local']), (e) => e.tagName === 'BUTTON' && /^Turn (on|off)$/.test(e.textContent)).length, 0);
// A blocked switch stays visible and says why next to it.
routes['GET /console/api/providers'] = { providers: [provider('local', 'private', true), { ...provider('tailscale', 'private', true, ['blog']), locked: 'Can’t turn off while the console runs on Tailscale.' }] };
tsPage = await mountSettings(['networks', 'tailscale']);
assert.equal(byText(tsPage, 'Turn off')[0].getAttribute('disabled'), '');
assert.ok(tsPage.textContent.includes('Can’t turn off while the console runs on Tailscale.'));
assert.deepEqual(all(byId(tsPage, 'allowed'), (e) => e.tagName === 'A').map((a) => a.getAttribute('href')), ['/flats/blog']);
routes['GET /console/api/providers'] = PROVIDERS;

const quickPanel = await mountSettings(['networks', 'cloudflare-quick']);
assert.ok(quickPanel.textContent.includes('Cloudflare Quick Tunnel'), 'the quick tunnel page is labeled');
assert.ok(quickPanel.textContent.includes('No Cloudflare account or domain needed'), 'the quick tunnel page says it needs no account');
assert.ok(quickPanel.textContent.includes('no uptime guarantee'), 'the quick tunnel page states its limits');
const quickLinks = all(quickPanel, (e) => e.tagName === 'A').map((e) => e.getAttribute('href')).filter((href) => /^https?:/.test(href));
assert.deepEqual([...new Set(quickLinks)], ['https://quick-demo.trycloudflare.com/'], 'the quick tunnel page links the served flat');
assert.equal(byText(quickPanel, 'Turn off').length, 1, 'quick tunnels can be turned off without an account step');
// The zrok page shows the connected account and its actions.
const zrokPanel = await mountSettings(['networks', 'zrok']);
assert.ok(zrokPanel.textContent.includes('flats-ns'), 'the zrok page shows its namespace');
assert.ok(zrokPanel.textContent.includes('Connected') && zrokPanel.textContent.includes('from the console'), 'state and source are shown');
const zrokFacts = all(byId(zrokPanel, 'key-zrok'), (e) => e.tagName === 'CODE').map((e) => e.textContent);
assert.deepEqual(zrokFacts, ['zenv-123', 'https://api-v2.zrok.io', 'flats-ns'], 'the connected environment is identified');
assert.equal(byText(zrokPanel, 'Turn off').length, 1, 'a connected zrok can be turned off');
assert.equal(byText(zrokPanel, 'Replace account').length, 1, 'a connected account can be replaced with a new token');
const zrokDisconnectBtn = byText(zrokPanel, 'Disconnect')[0];
assert.ok(zrokDisconnectBtn, 'a connected zrok account can be disconnected');
assert.equal(zrokDisconnectBtn.getAttribute('disabled'), '', 'disconnecting waits until zrok is off');
assert.ok(all(zrokPanel, (e) => e.className === 'disabled-reason')[0].getAttribute('data-reason').includes('Turn it off'), 'and says so on hover');

// zrok without a connected account offers the account token form, and its
// switch says why it waits; connecting does not turn zrok on by itself.
routes['GET /console/api/providers'] = { providers: [provider('local', 'private', true),
  { ...provider('zrok', 'public', false), namespace: 'public', managed: true, connected: false }] };
routes['PUT /console/api/providers/zrok/account'] = { providers: [] };
routes['PUT /console/api/providers/zrok'] = { providers: [], etag: 'etag-10' };
routes['GET /console/api/keys'] = { keys: [KEYS[0], KEYS[1], { id: 'zrok', set: false, managed: true }] };
const zrokMain = await mountSettings(['networks', 'zrok']);
assert.equal(byText(zrokMain, 'Turn on')[0].getAttribute('disabled'), '', 'zrok without an account cannot be turned on');
assert.ok(zrokMain.textContent.includes('Connect a zrok account below first.'));
const tokenInput = byId(zrokMain, 'zrok-token');
assert.equal(tokenInput.getAttribute('type'), 'password', 'the account token is typed into a password field');
tokenInput.value = '  test-token  ';
all(zrokMain, (e) => e.tagName === 'FORM' && e.className === 's-inline-form')[0].dispatch('submit');
await tick();
await tick();
const connectCall = calls.find((c) => c.key === 'PUT /console/api/providers/zrok/account');
assert.deepEqual(JSON.parse(connectCall.body), { token: 'test-token' });
assert.ok(!calls.some((c) => c.key === 'PUT /console/api/providers/zrok'), 'connecting does not turn zrok on');
assert.equal(tokenInput.value, '', 'the token is cleared from the form');
routes['GET /console/api/keys'] = { keys: KEYS };

// NetBird without a setup key shows numbered steps: add a key, then turn it
// on (allowed before a key is stored).
routes['GET /console/api/providers'] = { providers: [provider('local', 'private', true),
  { ...provider('netbird', 'private', false), management_url: 'https://api.netbird.io:443' },
  { ...provider('netbird-expose', 'public', false), management_url: 'https://api.netbird.io:443' }] };
routes['PUT /console/api/keys/netbird'] = { keys: KEYS };
const nbMain = await mountSettings(['networks', 'netbird']);
const steps = byId(nbMain, 'setup');
assert.ok(steps && steps.textContent.includes('Add a setup key') && steps.textContent.includes('Turn on NetBird'));
assert.equal(byText(nbMain, 'Turn on').length, 1, 'NetBird can be turned on before a key is stored');
assert.ok(nbMain.textContent.includes('plain HTTP') || nbMain.textContent.includes('Plain HTTP'), 'the page explains the HTTP limits');
assert.ok(steps.textContent.includes('Allow extra DNS labels') && steps.textContent.includes('ephemeral'), 'the recommended key options are named');
const keyInput = byId(nbMain, 'netbird-key');
assert.equal(keyInput.getAttribute('type'), 'password', 'the setup key is typed into a password field');
keyInput.value = '  8f3c2a1e-4b5d-4c6e-9f70-0123456789ab  ';
all(steps, (e) => e.tagName === 'FORM')[0].dispatch('submit');
await tick();
await tick();
const nbCall = calls.find((c) => c.key === 'PUT /console/api/keys/netbird');
assert.deepEqual(JSON.parse(nbCall.body), { key: '8f3c2a1e-4b5d-4c6e-9f70-0123456789ab' });
assert.equal(keyInput.value, '', 'the setup key is cleared from the form');
const nbExpose = await mountSettings(['networks', 'netbird-expose']);
assert.equal(byText(nbExpose, 'Turn on').length, 1, 'NetBird Expose can be turned on before a key is stored');
assert.ok(nbExpose.textContent.includes('reverse proxy'));

// A binary built without NetBird says so and offers neither a key form nor a switch.
routes['GET /console/api/providers'] = { providers: [provider('local', 'private', true),
  { ...provider('netbird', 'private', false), unsupported: 'This Flats build does not include NetBird; build ./netbird/cmd/flats from the source tree' }] };
routes['GET /console/api/keys'] = { keys: [KEYS[0], { id: 'netbird', set: false, managed: false, unsupported: 'This Flats build does not include NetBird; build ./netbird/cmd/flats from the source tree' }, KEYS[2]] };
const nbOff = await mountSettings(['networks', 'netbird']);
assert.ok(nbOff.textContent.includes('does not include NetBird'));
assert.equal(byText(nbOff, 'Turn on').length, 0, 'no switch without NetBird');
assert.equal(all(nbOff, (e) => e.tagName === 'FORM').length, 0, 'no key form without NetBird');
assert.ok((await mountSettings()).textContent.includes('Not in this build'), 'the list says so too');
routes['GET /console/api/providers'] = { providers: [provider('local', 'private', true),
  { ...provider('netbird-expose', 'public', false), unsupported: 'This Flats build does not include NetBird; build ./netbird/cmd/flats from the source tree' }] };
const nbExposeOff = await mountSettings(['networks', 'netbird-expose']);
assert.ok(nbExposeOff.textContent.includes('does not include NetBird'), 'NetBird Expose says why too');
assert.equal(byText(nbExposeOff, 'Turn on').length, 0);
assert.equal(nbExposeOff.textContent.includes('reverse proxy, which needs'), false, 'no setup guidance for a build without it');

// A stored key can be replaced or removed; the old shared peer is named until
// the operator dismisses the notice.
routes['GET /console/api/providers'] = { providers: [provider('local', 'private', true),
  { ...provider('netbird', 'private', true), management_url: 'https://nb.example', key_set: true, peers: 3 },
  { ...provider('netbird-expose', 'public', false), management_url: 'https://nb.example', key_set: true }] };
routes['GET /console/api/keys'] = { keys: [KEYS[0], { id: 'netbird', set: true, managed: true, peers: 3, management_url: 'https://nb.example',
  legacy_peer: { device_name: 'flats-box', management_url: 'https://nb.example' } }, KEYS[2]] };
routes['DELETE /console/api/keys/netbird/legacy-peer'] = { keys: KEYS };
const nbMain2 = await mountSettings(['networks', 'netbird']);
const nbKey = byId(nbMain2, 'key');
assert.ok(nbKey.textContent.includes('Stored'), 'the page shows the stored key');
assert.ok(nbKey.textContent.includes('3 peers running'), 'the page counts running peers');
assert.equal(byText(nbKey, 'Replace').length, 1, 'a stored key can be replaced');
assert.equal(byText(nbKey, 'Replace key').length, 1, 'with its own form');
assert.equal(byText(nbKey, 'Remove').length, 1, 'a stored key can be removed');
assert.ok(nbKey.textContent.includes('flats-box') && nbKey.textContent.includes('NetBird dashboard'), 'the old shared peer is named');
byText(nbKey, 'Done')[0].dispatch('click');
await tick();
await tick();
assert.ok(calls.some((c) => c.key === 'DELETE /console/api/keys/netbird/legacy-peer'), 'the notice can be dismissed');
routes['GET /console/api/keys'] = { keys: KEYS };
routes['GET /console/api/providers'] = PROVIDERS;

// --- Tailscale auth key ---
// The key is typed into a password field, sent once and cleared; a stored
// key can be replaced or removed, and a differing start value is explained.
routes['GET /console/api/keys'] = { keys: KEYS };
routes['PUT /console/api/keys/tailscale'] = { keys: KEYS };
const tsMain = await mountSettings(['networks', 'tailscale']);
const tsKey = byId(tsMain, 'key');
assert.ok(tsKey.textContent.includes('Not set'));
const tsInput = byId(tsKey, 'tailscale-key');
// The first key gets the same guidance as a replacement (spec rule 11).
assert.ok(byId(tsKey, 'tailscale-key-help').textContent.includes('reusable') && byId(tsKey, 'tailscale-key-help').textContent.includes('that is up to you'));
assert.ok(tsInput.getAttribute('aria-describedby').split(' ').includes('tailscale-key-help'));
assert.equal(tsInput.getAttribute('type'), 'password', 'the auth key is typed into a password field');
tsInput.value = '  tskey-auth-abc  ';
all(tsKey, (e) => e.tagName === 'FORM')[0].dispatch('submit');
await tick();
await tick();
assert.deepEqual(JSON.parse(calls.find((c) => c.key === 'PUT /console/api/keys/tailscale').body), { key: 'tskey-auth-abc' });
assert.equal(tsInput.value, '', 'the key is cleared from the form');
routes['GET /console/api/keys'] = { keys: [{ id: 'tailscale', set: true, managed: true, source: 'env', updated_at: '2026-10-01T00:00:00Z', external_differs: true, external_given: true }, KEYS[1], KEYS[2]] };
routes['DELETE /console/api/keys/tailscale'] = { keys: KEYS };
const tsMain2 = await mountSettings(['networks', 'tailscale']);
const tsKey2 = byId(tsMain2, 'key');
assert.ok(tsKey2.textContent.includes('Stored') && tsKey2.textContent.includes('an environment variable'), 'state and source');
assert.ok(tsKey2.textContent.includes('differs from the stored key'), 'a differing start value is explained');
assert.equal(byText(tsKey2, 'Replace').length, 1);
byText(tsKey2, 'Remove')[0].dispatch('click');
await tick();
const removeDialog = all(document.body, (e) => e.tagName === 'DIALOG').pop();
assert.ok(removeDialog.textContent.includes('interactive login'));
assert.ok(removeDialog.textContent.includes('the next start stores the key again'), 'removing a key given at start warns that it comes back');
removeDialog.close('ok');
await tick();
assert.ok(calls.some((c) => c.key === 'DELETE /console/api/keys/tailscale'), 'the key is removed after confirmation');
assert.equal(JSON.stringify(calls.filter((c) => c.key.startsWith('GET /console/api/keys'))).includes('tskey'), false);
routes['GET /console/api/keys'] = { keys: KEYS };

// --- console addresses ---
// Keys that fail to load are an error, not "not set": no key action is offered.
routes['GET /console/api/keys'] = { __status: 500, error: 'boom' };
routes['GET /console/api/providers'] = { providers: [...PROVIDERS.providers, { ...provider('netbird', 'private', false), management_url: 'https://nb.example' }] };
for (const id of ['tailscale', 'netbird', 'zrok']) {
  const failed = await mountSettings(['networks', id]);
  assert.ok(failed.textContent.includes('Cannot load the keys'), `${id} explains the failure`);
  assert.equal(all(failed, (e) => e.tagName === 'INPUT' && e.getAttribute('type') === 'password').length, 0, `${id} offers no key form`);
  assert.equal(byText(failed, 'Save key').length + byText(failed, 'Remove').length + byText(failed, 'Disconnect').length, 0);
}
// A failed zrok enrollment keeps its error and where the token came from.
routes['GET /console/api/providers'] = { providers: [provider('local', 'private', true),
  { ...provider('zrok', 'public', false), namespace: 'public', managed: true, connected: false }] };
routes['GET /console/api/keys'] = { keys: [KEYS[0], KEYS[1], { id: 'zrok', set: false, waiting: true, managed: true, source: 'env', updated_at: '2026-10-01T00:00:00Z', last_error: 'token refused' }] };
const zrokWait = await mountSettings(['networks', 'zrok']);
assert.ok(zrokWait.textContent.includes('Waiting for the next start') && zrokWait.textContent.includes('from an environment variable'));
assert.ok(zrokWait.textContent.includes('The last attempt to use this token failed: token refused'));
assert.equal(all(byId(zrokWait, 'key-zrok'), (e) => e.tagName === 'TIME' && e.getAttribute('datetime') === '2026-10-01T00:00:00Z').length, 1, 'when the token was set is shown');
routes['GET /console/api/providers'] = PROVIDERS;
routes['GET /console/api/keys'] = { keys: KEYS };

// --- console addresses ---
// Without Tailscale or NetBird there is nothing to add; with them, names are
// listed, added and deleted, and the last one cannot be deleted through itself.
let domMain = await mountSettings(['console']);
let domCard = byId(domMain, 'console-domains');
assert.ok(domCard.textContent.includes('127.0.0.1:7878') && domCard.textContent.includes('you are here'), 'the loopback address is listed');
assert.ok(domCard.textContent.includes('Turn Tailscale or NetBird on'), 'adding needs a network');
assert.equal(all(domCard, (e) => e.tagName === 'FORM').length, 0);
routes['GET /console/api/console-domains'] = dview({ on: true,
  domains: [{ name: 'flats', url: 'https://flats.tail1234.ts.net', state: 'ready', current: true },
    { name: 'blog', conflict: 'used by the flat "blog"' },
    { name: 'admin', assigned: 'admin-1', url: 'https://admin-1.tail1234.ts.net', state: 'ready' }],
  retiring: [{ name: 'old', error: 'logout refused' }] }, {},
{ loopback: false, via: { provider: 'tailscale', name: 'flats' }, pending_approvals: 2, etag: 'etag-7' });
routes['POST /console/api/console-domains'] = { etag: 'etag-8' };
routes['DELETE /console/api/console-domains/tailscale/blog'] = { etag: 'etag-8', redirect: '' };
domMain = await mountSettings(['console']);
domCard = byId(domMain, 'console-domains');
assert.ok(domCard.textContent.includes('You’re on flats (Tailscale).'), 'the address in use is named');
assert.ok(byId(domCard, 'domain-tailscale-flats').textContent.includes('you are here'), 'the domain in use is marked');
assert.ok(domCard.textContent.includes('used by the flat'), 'a conflict is explained');
assert.ok(domCard.textContent.includes('The tailnet named this node admin-1'), 'a renamed node is explained');
assert.ok(domCard.textContent.includes('Being removed') && domCard.textContent.includes('logout refused'), 'a failed logout is listed');
assert.equal(all(domCard, (e) => e.tagName === 'A').some((e) => e.getAttribute('href') === 'https://flats.tail1234.ts.net/'), true, 'a domain links to its console');
const netSelect = byId(domCard, 'console-domain-network');
assert.deepEqual(all(netSelect, (e) => e.tagName === 'OPTION').map((o) => [o.getAttribute('value'), o.getAttribute('disabled')]), [['tailscale', null], ['netbird', '']], 'a network that is off cannot be chosen');
const nameInput = byId(domCard, 'console-domain-name');
nameInput.value = ' admin ';
all(domCard, (e) => e.tagName === 'FORM')[0].dispatch('submit');
await tick();
await tick();
const addCall = calls.find((c) => c.key === 'POST /console/api/console-domains');
assert.deepEqual(JSON.parse(addCall.body), { provider: 'tailscale', name: 'admin' });
assert.equal(addCall.headers['If-Match'], '"etag-7"', 'adding is guarded by the settings ETag');
const delButtons = all(domCard, (e) => e.tagName === 'BUTTON' && e.textContent === 'Delete');
assert.equal(delButtons.length, 3);
delButtons[1].dispatch('click');
await tick();
const delDialog = all(document.body, (e) => e.tagName === 'DIALOG').pop();
assert.ok(delDialog.textContent.includes('Remaining: flats, admin.'), 'the remaining names are listed');
assert.ok(delDialog.textContent.includes('2 approvals waiting'), 'pending approvals are mentioned');
delDialog.close('ok');
await tick();
await tick();
const delCall = calls.find((c) => c.key === 'DELETE /console/api/console-domains/tailscale/blog');
assert.ok(delCall && delCall.headers['If-Match'] === '"etag-7"', 'deleting is guarded by the settings ETag');
// The last domain, seen through itself, cannot be deleted here.
routes['GET /console/api/console-domains'] = dview({ on: true,
  domains: [{ name: 'flats', url: 'https://flats.tail1234.ts.net', state: 'ready', current: true }] }, {},
{ loopback: false, via: { provider: 'tailscale', name: 'flats' }, etag: 'etag-7' });
domMain = await mountSettings(['console']);
const lastDel = all(byId(domMain, 'console-domains'), (e) => e.tagName === 'BUTTON' && e.textContent === 'Delete')[0];
assert.equal(lastDel.getAttribute('disabled'), '', 'the last domain is deleted only from the loopback console');
// A disabled button gets no hover events: its wrapper carries the reason,
// and screen readers get it as the button's description.
const lastWrap = all(byId(domMain, 'console-domains'), (e) => e.className === 'disabled-reason')[0];
assert.ok(lastWrap && lastWrap.getAttribute('data-reason').includes('last domain') && lastWrap.getAttribute('data-reason').includes('loopback'), 'the reason shows on hover');
assert.equal(lastWrap.getAttribute('tabindex'), '0', 'keyboard users can reach the reason');
assert.ok(byId(domMain, lastDel.getAttribute('aria-describedby')).textContent.includes('last domain'), 'the reason describes the button');
assert.deepEqual(all(domMain, (e) => e.tagName === 'H3').map((e) => e.textContent), ['Addresses', 'Links', 'Reverse proxy', 'Host']);
// The domain in use cannot be deleted while no other one is ready.
routes['GET /console/api/console-domains'] = dview({ on: true,
  domains: [{ name: 'flats', url: 'https://flats.tail1234.ts.net', state: 'ready', current: true }, { name: 'next', state: 'starting' }] }, {},
{ loopback: false, via: { provider: 'tailscale', name: 'flats' }, etag: 'etag-7' });
domMain = await mountSettings(['console']);
const curWrap = all(byId(domMain, 'console-domains'), (e) => e.className === 'disabled-reason');
assert.equal(curWrap.length, 1, 'only the domain in use is blocked');
assert.ok(curWrap[0].getAttribute('data-reason').includes('no other console domain is ready'));
assert.ok(byId(domMain, 'domain-tailscale-next').textContent.includes('Starting'));
// A ready NetBird domain counts: the last tailnet domain may go, and the
// page can move there. NetBird domains are added with their network.
routes['GET /console/api/console-domains'] = dview({ on: true,
  domains: [{ name: 'flats', url: 'https://flats.tail1234.ts.net', state: 'ready', current: true }] },
{ on: true, domains: [{ name: 'flats', url: 'http://flats.netbird.cloud/', state: 'ready' }] },
{ loopback: false, via: { provider: 'tailscale', name: 'flats' }, etag: 'etag-9' });
domMain = await mountSettings(['console']);
assert.equal(all(byId(domMain, 'console-domains'), (e) => e.className === 'disabled-reason').length, 0, 'a ready NetBird domain lets the tailnet one go');
assert.ok(domMain.textContent.includes('plain HTTP'), 'the page explains HTTP for NetBird addresses');
const nbNet = byId(domMain, 'console-domain-network');
nbNet.value = 'netbird';
nbNet.dispatch('change');
byId(domMain, 'console-domain-name').value = 'ops';
all(byId(domMain, 'console-domains'), (e) => e.tagName === 'FORM')[0].dispatch('submit');
await tick();
await tick();
assert.deepEqual(JSON.parse(calls.filter((c) => c.key === 'POST /console/api/console-domains').pop().body), { provider: 'netbird', name: 'ops' });
assert.equal(settingsPage.deleteBlocker({ name: 'a' }, { loopback: true }, 1, ['tailscale/a']), '', 'the loopback console may delete the last domain');
routes['GET /console/api/console-domains'] = DOMAINS;

// --- preferred console ---
// The choice is Automatic, the loopback address or a listed console domain;
// it saves with the save bar, guarded by the settings ETag.
const optionValues = (sel) => all(sel, (e) => e.tagName === 'OPTION').map((o) => o.getAttribute('value'));
routes['GET /console/api/console-domains'] = dview({ on: true,
  domains: [{ name: 'flats', url: 'https://flats.tail1234.ts.net', state: 'ready' }, { name: 'next', state: 'starting' }] },
{ on: true, domains: [{ name: 'ops', url: 'http://ops.netbird.cloud/', state: 'ready' }] },
{ preferred: '', link_url: 'https://flats.tail1234.ts.net', etag: 'etag-p1' });
routes['PUT /console/api/console-domains/preferred'] = { etag: 'etag-p2' };
domMain = await mountSettings(['console']);
// hasURL reports whether text names exactly url as one of its URLs, not as a
// substring of a longer one.
const hasURL = (text, url) => (text.match(/https?:\/\/[^\s,;)]+/g) || []).some((u) => u.replace(/\.$/, '') === url);
let prefSel = byId(domMain, 'console-preferred');
assert.deepEqual(optionValues(prefSel), ['', 'local', 'tailscale:flats', 'tailscale:next', 'netbird:ops'], 'Automatic, loopback and every listed domain');
assert.equal(prefSel.value, '', 'no preference is chosen');
assert.ok(all(byId(domMain, 'console-links'), (e) => e.tagName === 'CODE' && e.textContent === 'https://flats.tail1234.ts.net').length === 1, 'where links go now is shown');
assert.equal(all(domMain, (e) => e.className === 'alert alert-warn').length, 0, 'no warning without a preference');
prefSel.value = 'tailscale:next';
byId(domMain, 'links-form').dispatch('change');
assert.ok(savebarOf(domMain).textContent.includes('Address for links: Automatic → next (Tailscale)'));
byId(domMain, 'links-form').dispatch('submit');
await tick();
await tick();
const prefCall = calls.filter((c) => c.key === 'PUT /console/api/console-domains/preferred').pop();
assert.deepEqual(JSON.parse(prefCall.body), { preferred: 'tailscale:next' });
assert.equal(prefCall.headers['If-Match'], '"etag-p1"', 'choosing is guarded by the settings ETag');
// Choosing Automatic sends an empty preference.
routes['GET /console/api/console-domains'] = dview({ on: true,
  domains: [{ name: 'flats', url: 'https://flats.tail1234.ts.net', state: 'ready' }, { name: 'next', url: 'https://next.tail1234.ts.net', state: 'ready' }] }, {},
{ preferred: 'tailscale:next', link_url: 'https://next.tail1234.ts.net', etag: 'etag-p2' });
domMain = await mountSettings(['console']);
assert.ok(byId(domMain, 'domain-tailscale-next').textContent.includes('Preferred'), 'the preferred address is tagged');
prefSel = byId(domMain, 'console-preferred');
prefSel.value = '';
byId(domMain, 'links-form').dispatch('submit');
await tick();
await tick();
assert.deepEqual(JSON.parse(calls.filter((c) => c.key === 'PUT /console/api/console-domains/preferred').pop().body), { preferred: '' });
// A staged choice survives an immediate action that reloads the page.
routes['GET /console/api/console-domains'] = dview({ on: true,
  domains: [{ name: 'flats', url: 'https://flats.tail1234.ts.net', state: 'ready' }, { name: 'next', url: 'https://next.tail1234.ts.net', state: 'ready' }] }, {},
{ preferred: '', link_url: 'https://flats.tail1234.ts.net', etag: 'etag-p5' });
routes['POST /console/api/console-domains'] = { etag: 'etag-p6' };
domMain = await mountSettings(['console']);
byId(domMain, 'console-preferred').value = 'tailscale:next';
byId(domMain, 'links-form').dispatch('change');
byId(domMain, 'console-domain-name').value = 'third';
all(byId(domMain, 'console-domains'), (e) => e.tagName === 'FORM')[0].dispatch('submit');
await tick();
await tick();
assert.equal(byId(domMain, 'console-preferred').value, 'tailscale:next', 'the staged choice is kept');
assert.equal(savebarOf(domMain).getAttribute('hidden'), null, 'and still waits for Save');
assert.ok(savebarOf(domMain).textContent.includes('Address for links: Automatic → next (Tailscale)'));
// After a save the page starts clean.
byId(domMain, 'links-form').dispatch('submit');
await tick();
await tick();
assert.equal(byId(domMain, 'console-preferred').value, '', 'the reload shows the stored value');
assert.equal(byId(domMain, 'links-save').disabled, true);
// What the operator does while the reload runs wins over the earlier
// staged value: a new choice, or Discard.
const VIEW_P5 = routes['GET /console/api/console-domains'];
let releaseDomains;
const slowDomains = (view) => () => new Promise((resolve) => { releaseDomains = () => resolve(view); });
for (const [during, want] of [['change', ''], ['discard', '']]) {
  routes['GET /console/api/console-domains'] = VIEW_P5;
  domMain = await mountSettings(['console']);
  byId(domMain, 'console-preferred').value = 'tailscale:next';
  byId(domMain, 'links-form').dispatch('change');
  routes['GET /console/api/console-domains'] = slowDomains(VIEW_P5);
  byId(domMain, 'console-domain-name').value = 'fourth';
  all(byId(domMain, 'console-domains'), (e) => e.tagName === 'FORM')[0].dispatch('submit');
  await tick();
  assert.equal(typeof releaseDomains, 'function', 'the reload must be waiting');
  if (during === 'change') {
    byId(domMain, 'console-preferred').value = '';
    byId(domMain, 'links-form').dispatch('change');
  } else {
    byText(savebarOf(domMain), 'Discard')[0].dispatch('click');
  }
  releaseDomains();
  await tick();
  await tick();
  assert.equal(byId(domMain, 'console-preferred').value, want, `${during} during the reload is kept`);
  assert.equal(byId(domMain, 'links-save').disabled, true, `${during}: nothing is left staged`);
  releaseDomains = undefined;
}
// A staged choice that no longer exists after the reload is dropped: the
// stored value stays and nothing is staged.
routes['GET /console/api/console-domains'] = VIEW_P5;
domMain = await mountSettings(['console']);
byId(domMain, 'console-preferred').value = 'tailscale:next';
byId(domMain, 'links-form').dispatch('change');
routes['GET /console/api/console-domains'] = dview({ on: true, domains: [{ name: 'flats', url: 'https://flats.tail1234.ts.net', state: 'ready' }] }, {},
  { preferred: '', link_url: 'https://flats.tail1234.ts.net', etag: 'etag-p7' });
byId(domMain, 'console-domain-name').value = 'fifth';
all(byId(domMain, 'console-domains'), (e) => e.tagName === 'FORM')[0].dispatch('submit');
await tick();
await tick();
assert.equal(byId(domMain, 'console-preferred').value, '', 'a vanished choice is not restored');
assert.equal(byId(domMain, 'links-save').disabled, true);
// A preferred domain that is not served is warned about, with where links go instead.
routes['GET /console/api/console-domains'] = dview({ on: true,
  domains: [{ name: 'flats', url: 'https://flats.tail1234.ts.net', state: 'ready' }, { name: 'next', state: 'starting' }] }, {},
{ preferred: 'tailscale:next', preferred_state: 'not_served', link_url: 'https://flats.tail1234.ts.net', etag: 'etag-p3' });
domMain = await mountSettings(['console']);
prefSel = byId(domMain, 'console-preferred');
assert.equal(prefSel.value, 'tailscale:next', 'the preference is shown');
let prefWarn = all(domMain, (e) => e.className === 'alert alert-warn');
assert.equal(prefWarn.length, 1);
assert.ok(prefWarn[0].textContent.includes('tailnet domain next') && prefWarn[0].textContent.includes('not served')
  && hasURL(prefWarn[0].textContent, 'https://flats.tail1234.ts.net'), prefWarn[0].textContent);
// A preferred domain whose node is still starting is warned about too.
routes['GET /console/api/console-domains'] = dview({ on: true,
  domains: [{ name: 'flats', url: 'https://flats.tail1234.ts.net', state: 'ready' }, { name: 'next', url: 'https://next.tail1234.ts.net', state: 'starting' }] }, {},
{ preferred: 'tailscale:next', preferred_state: 'starting', link_url: 'https://flats.tail1234.ts.net', etag: 'etag-p3' });
domMain = await mountSettings(['console']);
prefWarn = all(domMain, (e) => e.className === 'alert alert-warn');
assert.ok(prefWarn.length === 1 && prefWarn[0].textContent.includes('still starting') && hasURL(prefWarn[0].textContent, 'https://flats.tail1234.ts.net'),
  'a starting preferred domain is warned about');
// A file edit that names an unlisted domain is shown as chosen, and ignored.
routes['GET /console/api/console-domains'] = dview({ on: true, domains: [{ name: 'flats', url: 'https://flats.tail1234.ts.net', state: 'ready' }] }, {},
  { preferred: 'netbird:gone', preferred_state: 'unlisted', link_url: 'https://flats.tail1234.ts.net', etag: 'etag-p4' });
domMain = await mountSettings(['console']);
prefSel = byId(domMain, 'console-preferred');
assert.deepEqual(optionValues(prefSel), ['', 'local', 'tailscale:flats', 'netbird:gone']);
assert.equal(prefSel.value, 'netbird:gone');
prefWarn = all(domMain, (e) => e.className === 'alert alert-warn');
assert.ok(prefWarn.length === 1 && prefWarn[0].textContent.includes('not a console domain') && prefWarn[0].textContent.includes('ignored'), 'an unlisted preference is warned about');
// A refused change says why and keeps the staged choice to fix or discard.
routes['PUT /console/api/console-domains/preferred'] = { __status: 400, error: 'gone is not a netbird console domain' };
prefSel.value = 'local';
byId(domMain, 'links-form').dispatch('submit');
await tick();
await tick();
const prefAlert = all(domMain, (e) => e.getAttribute('role') === 'alert')[0];
assert.ok(prefAlert && prefAlert.textContent.includes('Cannot change the preferred console') && prefAlert.textContent.includes('not a netbird console domain'));
assert.equal(prefSel.value, 'local', 'the staged choice stays until discarded');
byText(savebarOf(domMain), 'Discard')[0].dispatch('click');
assert.equal(prefSel.value, 'netbird:gone', 'Discard restores the stored choice');
routes['PUT /console/api/console-domains/preferred'] = { etag: 'etag-p2' };
routes['GET /console/api/console-domains'] = DOMAINS;
// The pages refresh only while a node or an enrollment is still pending.
assert.equal(settingsPage.pendingWork(dview({ domains: [{ name: 'a', state: 'ready' }, { name: 'b', conflict: 'x' }] }), [{ id: 'zrok' }]), false);
assert.equal(settingsPage.pendingWork(dview({}, { domains: [{ name: 'a', state: 'starting' }] }), []), true);
assert.equal(settingsPage.pendingWork(dview({}), [{ id: 'zrok', waiting: true }]), true);
// A node on an expiring Tailscale key serves: it is not pending work, and it
// lets the domain in use be deleted, as the server counts it.
assert.equal(settingsPage.pendingWork(dview({ domains: [{ name: 'a', state: 'key-expiring' }] }), []), false);
routes['GET /console/api/console-domains'] = dview({ on: true,
  domains: [{ name: 'flats', url: 'https://flats.tail1234.ts.net', state: 'ready', current: true }, { name: 'old', url: 'https://old.tail1234.ts.net', state: 'key-expiring' }] }, {},
{ loopback: false, via: { provider: 'tailscale', name: 'flats' }, etag: 'etag-7' });
domMain = await mountSettings(['console']);
assert.equal(all(byId(domMain, 'console-domains'), (e) => e.className === 'disabled-reason').length, 0, 'a key-expiring domain counts as ready');
assert.ok(byId(domMain, 'domain-tailscale-old').textContent.includes('Key expiring'));
routes['GET /console/api/console-domains'] = DOMAINS;

// --- approvals ---
// Approvals are a queue in the top bar, not a settings card.
const approvalModule = await import('./approval.js');
routes['GET /console/api/approvals'] = { approvals: [{ id: 'apr-1', action: 'delete', flat: 'blog', via: 'mcp', status: 'pending', requested_at: '2026-10-03T00:00:00Z' }] };
const apprMain = new Element('main');
approvalModule.mountList(apprMain, [], ctx);
await tick();
const apprLink = all(apprMain, (e) => e.tagName === 'A' && e.getAttribute('href') === '/approvals/apr-1')[0];
assert.ok(apprLink && apprLink.textContent === 'Delete blog');
assert.ok(apprMain.textContent.includes('pending'));

// --- loopback links seen from another device ---
// A loopback URL opens the viewer's own machine, so a console opened over
// the tailnet shows it as server-only text; on the host it stays a link.
const ui = await import('./ui.js');
const savedLocation = globalThis.location;
const hostOf = (url) => new URL(url).hostname;
for (const url of ['http://localhost/', 'http://blog.localhost:7879/', 'http://BLOG.LOCALHOST/', 'http://blog.localhost.:7879/',
  'http://127.0.0.1/', 'http://127.1/', 'http://2130706433/', 'http://[::1]/', 'http://[0:0:0:0:0:0:0:1]/', 'http://0.0.0.0:7879/',
  'http://[::]/', 'http://[::ffff:127.0.0.1]/', 'http://[::ffff:0.0.0.0]/']) assert.ok(ui.loopbackHost(hostOf(url)), url);
for (const url of ['https://flats.tail1234.ts.net/', 'https://example.com/', 'http://10.0.0.1/', 'http://localhost.example.com/',
  'http://[::ffff:10.0.0.1]/', 'http://[2001:db8::1]/', 'http://128.0.0.1/']) assert.ok(!ui.loopbackHost(hostOf(url)), url);
globalThis.location = { origin: 'https://flats.tail1234.ts.net', hostname: 'flats.tail1234.ts.net' };
const remote = ui.extLink('http://blog.localhost:7879/', 'Open');
assert.equal(remote.tagName, 'SPAN');
assert.equal(remote.getAttribute('aria-disabled'), 'true');
assert.equal(remote.getAttribute('title'), ui.SERVER_ONLY);
assert.ok(remote.textContent.includes('Open') && remote.textContent.includes('server only'));
// Without its own text a server-only link names the network, never the address.
const bare = ui.extLink('http://blog.localhost:7879/');
assert.equal(bare.tagName, 'SPAN');
assert.ok(bare.textContent.includes(ui.LOCAL_ADDRESS) && bare.textContent.includes('server only'), bare.textContent);
assert.ok(!bare.textContent.includes('localhost'), 'the Local address is not shown');
assert.equal(ui.addressText('http://blog.localhost:7879/'), ui.LOCAL_ADDRESS);
assert.equal(ui.addressText('https://blog.tail1234.ts.net/'), 'https://blog.tail1234.ts.net/');
assert.ok(ui.SERVER_ONLY.includes('Settings → Networks → Private'), 'the tooltip must say where to allow Tailscale');
// An icon-only row button keeps its size: no text badge, the explanation is
// in the tooltip and the icon's accessible name.
const { icon } = await import('./dom.js');
const remoteIcon = ui.extLink('http://blog.localhost:7879/', icon('external', 'Open Blog (private URL)'), 'icon-btn');
assert.equal(remoteIcon.tagName, 'BUTTON', 'reachable by keyboard and touch');
assert.equal(remoteIcon.getAttribute('aria-disabled'), 'true');
assert.equal(remoteIcon.getAttribute('href'), null);
assert.equal(remoteIcon.getAttribute('title'), ui.SERVER_ONLY);
assert.ok(!remoteIcon.textContent.includes('server only'), 'icon button must not carry a text badge');
assert.equal(all(remoteIcon, (e) => (e.getAttribute('class') || '').includes('badge')).length, 0);
assert.equal(remoteIcon.childElementCount, 1);
assert.ok(remoteIcon.childNodes[0].getAttribute('aria-label').startsWith('Open Blog (private URL), server only.'));
const toastBox = new Element('div');
toastBox.setAttribute('id', 'toasts');
const savedGetById = document.getElementById;
document.getElementById = (id) => (id === 'toasts' ? toastBox : savedGetById?.(id));
remoteIcon.dispatch('click');
assert.ok(toastBox.textContent.includes(ui.SERVER_ONLY), 'tapping the icon explains why it is disabled');
document.getElementById = savedGetById;
assert.equal(ui.extLink('https://blog.tail1234.ts.net/').tagName, 'A', 'tailnet links stay links');
for (const url of ['http://blog.localhost.:7879/', 'http://[::ffff:127.0.0.1]:7879/', 'http://0.0.0.0:7879/']) {
  assert.equal(ui.extLink(url).tagName, 'SPAN', url);
}
// The share dialog does not offer a server-only private link for copying.
shareDialog({ ...blog, visibility: 'private', private_url: 'http://blog.localhost:7879', live_version: 1 });
const remoteShare = all(document.body, (e) => e.tagName === 'DIALOG').pop();
const copyLink = all(remoteShare, (e) => e.tagName === 'BUTTON' && e.textContent.includes('Copy link'))[0];
assert.equal(copyLink.getAttribute('disabled'), '', 'copying a server-only link is disabled');
assert.ok(remoteShare.textContent.includes(ui.SERVER_ONLY));
assert.ok(!remoteShare.textContent.includes('localhost'), 'the share dialog does not show the Local address');
assert.equal(all(remoteShare, (e) => (e.getAttribute('class') || '').includes('share-link'))[0].textContent, '', 'no Local link text');
assert.equal(all(remoteShare, (e) => e.textContent.includes('Visit')).length, 0, 'no Visit for a server-only link');
remoteShare.close();
shareDialog({ ...blog, visibility: 'private', private_url: 'https://blog.tail1234.ts.net', live_version: 1 });
const tailShare = all(document.body, (e) => e.tagName === 'DIALOG').pop();
assert.equal(all(tailShare, (e) => e.tagName === 'BUTTON' && e.textContent.includes('Copy link'))[0].getAttribute('disabled'), null);
tailShare.close();
globalThis.location = { origin: 'http://127.0.0.1:7878' };
assert.equal(ui.extLink('http://blog.localhost:7879/').tagName, 'A', 'on the host a loopback link works');
// --- separate private and public open buttons ---
// The private button (lock) is always last, so it keeps its place; the
// public one (globe) sits to its left only while the flat is public. A kind
// with several networks becomes a menu of them.
const glyphOf = (el) => all(el, (e) => e.tagName === 'SVG')[0];
const pathOf = (svg) => svg.childNodes[0].getAttribute('d');
const ep = (provider, url) => ({ provider, url, audience: 'current', permitted: true, state: 'ready' });
const openBoth = ui.openLinks({ ...blog, visibility: 'public', live_version: 1, endpoints: [ep('local', 'http://blog.localhost:7879/'), ep('portal', 'https://blog.rly.best/')] });
const [pubBtn, privBtn] = openBoth.childNodes;
assert.equal(openBoth.childNodes.length, 2, 'one public and one private button');
assert.equal(pubBtn.getAttribute('href'), 'https://blog.rly.best/');
assert.ok(pubBtn.getAttribute('class').includes('open-public'));
assert.equal(privBtn.getAttribute('href'), 'http://blog.localhost:7879/');
assert.ok(privBtn.getAttribute('class').includes('open-private'));
assert.ok(glyphOf(pubBtn).getAttribute('aria-label').includes('anyone with the URL'), 'the public button warns');
assert.equal(pathOf(glyphOf(privBtn)), pathOf(icon('lock')), 'private uses the lock icon');
assert.equal(pathOf(glyphOf(pubBtn)), pathOf(icon('globe')), 'public uses the globe icon');
// A private flat has no public button; an empty slot keeps the private one in place.
const isSpacer = (el) => el.getAttribute('class') === 'icon-btn open-spacer' && el.getAttribute('aria-hidden') === 'true' && el.childNodes.length === 0;
const openPrivate = ui.openLinks({ ...blog, visibility: 'private', live_version: 1, endpoints: [ep('local', 'http://blog.localhost:7879/'), ep('tailscale', 'https://blog.tail1234.ts.net/'), ep('portal', 'https://blog.rly.best/')] });
assert.equal(openPrivate.childNodes.length, 2);
assert.ok(isSpacer(openPrivate.childNodes[0]), 'a private flat has no public button');
assert.ok(openPrivate.childNodes[1].getAttribute('class').includes('menu-wrap'), 'on the host, Local and one more network stay a menu');
assert.equal(all(openPrivate, (e) => (e.getAttribute('class') || '').includes('menu-item')).length, 2);
// A private flat with a leftover public URL still shows only the private button.
assert.ok(isSpacer(ui.openLinks({ ...blog, visibility: 'private' }).childNodes[0]));
const onlyLocal = ui.openLinks({ ...blog, visibility: 'private', live_version: 1, endpoints: [ep('local', 'http://blog.localhost:7879/')] });
assert.equal(onlyLocal.childNodes[1].getAttribute('href'), 'http://blog.localhost:7879/', 'on the host a Local-only flat opens Local');
// The narrow list's single open link uses the external-link arrow, so globe
// and lock only mean visibility. It opens the first public address while the
// flat is public, else the first private one; the menu lists every address.
const twoNets = { ...blog, visibility: 'public', live_version: 1, endpoints: [ep('local', 'http://blog.localhost:7879/'), ep('portal', 'https://blog.rly.best/')] };
const primary = ui.openPrimary(twoNets);
assert.equal(primary.getAttribute('href'), 'https://blog.rly.best/');
assert.ok(primary.getAttribute('class').includes('open-primary'));
assert.equal(pathOf(glyphOf(primary)), pathOf(icon('external')));
assert.ok(glyphOf(primary).getAttribute('aria-label').includes('anyone with the URL'), 'the public link warns');
const privPrimary = ui.openPrimary({ ...twoNets, visibility: 'private' });
assert.equal(privPrimary.getAttribute('href'), 'http://blog.localhost:7879/');
assert.ok(!glyphOf(privPrimary).getAttribute('aria-label').includes('anyone with the URL'));
assert.deepEqual(ui.openMenuItems(twoNets).map((i) => [i.label, i.detail, i.href, i.className]), [
  ['Open publicly · Portal', 'blog.rly.best/', 'https://blog.rly.best/', 'menu-narrow'],
  ['Open privately · Local', 'blog.localhost:7879/', 'http://blog.localhost:7879/', 'menu-narrow']]);
assert.deepEqual(ui.openMenuItems({ ...twoNets, visibility: 'private' }).map((i) => i.label), ['Open privately · Local']);
// Seen from another device (a phone on NetBird), the narrow link skips the
// Local address, which only opens on the host, for one that opens there.
const meshNets = { ...blog, visibility: 'private', live_version: 1,
  endpoints: [ep('local', 'http://blog.localhost:7879/'), ep('netbird', 'http://blog.netbird.cloud/')] };
globalThis.location = { origin: 'http://flats.netbird.cloud', hostname: 'flats.netbird.cloud' };
assert.equal(ui.openPrimary(meshNets).getAttribute('href'), 'http://blog.netbird.cloud/', 'a remote viewer gets the NetBird address');
assert.equal(ui.openPrimary({ ...meshNets, endpoints: [meshNets.endpoints[0]] }).tagName, 'BUTTON', 'with only Local it stays server only');
assert.ok(ui.SERVER_ONLY.includes('NetBird'), 'the server-only note names NetBird too');
// Seen from another device, Local addresses are not offered at all: the
// lock opens the one remaining address, menus leave Local out, and a flat
// with only Local keeps a disabled lock that explains itself.
{
  const meshTs = { ...meshNets, endpoints: [...meshNets.endpoints, ep('tailscale', 'https://blog.tail1234.ts.net/')] };
  const remoteMenu = ui.openLinks(meshTs).childNodes[1];
  assert.ok(remoteMenu.getAttribute('class').includes('menu-wrap'));
  const items = all(remoteMenu, (e) => (e.getAttribute('class') || '').includes('menu-item'));
  assert.equal(items.length, 2, 'the remote menu leaves Local out');
  assert.ok(!remoteMenu.textContent.includes('localhost') && !remoteMenu.textContent.includes('Local'), remoteMenu.textContent);
  const lock = ui.openLinks(meshNets).childNodes[1];
  assert.equal(lock.tagName, 'A', 'Local plus NetBird opens NetBird directly, without a menu');
  assert.equal(lock.getAttribute('href'), 'http://blog.netbird.cloud/');
  const disabledLock = ui.openLinks({ ...meshNets, endpoints: [meshNets.endpoints[0]] }).childNodes[1];
  assert.equal(disabledLock.tagName, 'BUTTON');
  assert.equal(disabledLock.getAttribute('href'), null);
  assert.equal(disabledLock.getAttribute('title'), ui.SERVER_ONLY);
  assert.deepEqual(ui.openMenuItems(meshNets).map((i) => i.label), ['Open privately · NetBird'], 'the narrow menu leaves Local out');
  // A private_url on Local (the host's private default) gives way to an
  // address this device can open; with none it stays.
  assert.equal(ui.privateURL({ ...meshNets, private_url: 'http://blog.localhost:7879' }), 'http://blog.netbird.cloud/');
  assert.equal(ui.privateURL({ ...meshNets, endpoints: [meshNets.endpoints[0]], private_url: 'http://blog.localhost:7879' }), 'http://blog.localhost:7879');
  assert.equal(ui.privateURL({ ...meshNets, private_url: 'https://blog.tail1234.ts.net' }), 'https://blog.tail1234.ts.net');
  // The Status row shows that address with its own route's state.
  {
    const flatMod = await import('./flat.js');
    const starting = { ...blog, visibility: 'private', type: 'docs', running: true, private_url: 'http://blog.localhost:7879', private_state: 'ready',
      endpoints: [ep('local', 'http://blog.localhost:7879/'), { ...ep('tailscale', 'https://blog.tail1234.ts.net/'), state: 'starting', ready: false, detail: 'joining the tailnet' }] };
    const savedRoute = routes['GET /console/api/flats/blog'];
    routes['GET /console/api/flats/blog'] = starting;
    const statusMain = new Element('main');
    const stopStatus = flatMod.mount(statusMain, ['blog'], ctx);
    await tick();
    const statusBox = byId(statusMain, 'status') || statusMain;
    const statusText = statusBox.textContent;
    assert.equal(all(statusBox, (e) => e.tagName === 'A')[0]?.getAttribute('href'), 'https://blog.tail1234.ts.net/', 'the Private URL links the Tailscale address');
    assert.ok(!statusText.includes('localhost'), statusText);
    assert.ok(statusText.includes('starting') && statusText.includes('joining the tailnet'), statusText);
    stopStatus();
    routes['GET /console/api/flats/blog'] = savedRoute;
  }
  assert.deepEqual(ui.openMenuItems({ ...meshNets, endpoints: [meshNets.endpoints[0]] }), []);
  // The fallback private_url is a Local address too.
  assert.deepEqual(ui.openMenuItems({ ...blog, visibility: 'private', live_version: 1, endpoints: [], private_url: 'http://blog.localhost:7879' }), []);
  const { siteHeader: remoteHeader } = await import('./site.js');
  const remoteLines = (f) => all(remoteHeader(f, 'deployments'), (e) => e.className === 'site-address muted');
  const withNb = remoteLines(meshNets);
  assert.equal(withNb.length, 1);
  assert.ok(withNb[0].textContent.includes('netbird') && !withNb[0].textContent.includes('localhost'), withNb[0].textContent);
  const onlyLocalLine = remoteLines({ ...meshNets, endpoints: [meshNets.endpoints[0]] });
  assert.equal(onlyLocalLine.length, 1, 'a Local-only flat keeps its private line');
  assert.ok(onlyLocalLine[0].textContent.includes(ui.LOCAL_ADDRESS) && !onlyLocalLine[0].textContent.includes('localhost'), onlyLocalLine[0].textContent);
}
globalThis.location = { origin: 'http://127.0.0.1:7878' };
assert.equal(ui.openPrimary(meshNets).getAttribute('href'), 'http://blog.localhost:7879/', 'on the host the first address is kept');
assert.equal(ui.privateURL({ ...meshNets, private_url: 'http://blog.localhost:7879' }), 'http://blog.localhost:7879', 'on the host private_url is kept');
// Public routes the flat may not use (unpermitted, another flat's, stopped)
// never become its public button; only the server's public_url backs it.
const local = ep('local', 'http://blog.localhost:7879/');
for (const bad of [{ ...ep('tailscale-funnel', 'https://blog.ts.net/'), permitted: false }, { ...ep('portal', 'https://other.rly.best/'), host: 'other' },
  { ...ep('zrok', 'https://blog.share.zrok.io/'), state: 'stopped' }]) {
  const t = (await import('./dom.js')).linkTargets({ ...blog, public_url: undefined, endpoints: [local, bad] });
  assert.deepEqual(t.public, [], bad.url);
  assert.deepEqual(t.private.map((x) => x.url), [local.url]);
  assert.ok(isSpacer(ui.openLinks({ ...blog, public_url: undefined, endpoints: [local, bad] }).childNodes[0]), bad.url);
}
const fromServer = (await import('./dom.js')).linkTargets({ ...blog, endpoints: [local, { ...ep('portal', 'https://other.rly.best/'), host: 'other' }] });
assert.deepEqual(fromServer.public.map((x) => x.url), [blog.public_url], 'the server public_url is the fallback');
const openNb = ui.openLinks({ ...blog, visibility: 'public', live_version: 1, endpoints: [ep('netbird-expose', 'https://blog.nb.example/')] });
assert.equal(openNb.childNodes.length, 2, 'the private URL backs the private button');
assert.equal(openNb.childNodes[0].getAttribute('href'), 'https://blog.nb.example/', 'NetBird Expose is public');
assert.equal(openNb.childNodes[1].getAttribute('href'), new URL(blog.private_url).href);
// The flat page shows one line per kind: private (lock) first, then public.
const { siteHeader } = await import('./site.js');
const lines = (f) => all(siteHeader(f, 'deployments'), (e) => e.className === 'site-address muted');
const pubLines = lines({ ...blog, visibility: 'public' });
assert.equal(pubLines.length, 2);
assert.equal(pathOf(glyphOf(pubLines[0])), pathOf(icon('lock')));
assert.equal(all(pubLines[0], (e) => e.tagName === 'A')[0].getAttribute('href'), new URL(blog.private_url).href);
assert.equal(pathOf(glyphOf(pubLines[1])), pathOf(icon('globe')));
assert.equal(all(pubLines[1], (e) => e.tagName === 'A')[0].getAttribute('href'), new URL(blog.public_url).href);
assert.equal(lines({ ...blog, visibility: 'private' }).length, 1, 'no public line while private');
globalThis.location = savedLocation;
// Share readiness and refresh use current routes, never draft or stopped links.
const { shareRoute } = await import('./share.js');
const shareFixture = { ...blog, visibility: 'private', running: true, endpoints: [], private_state: 'ready' };
const routeFixture = (provider, state, ready = false) => ({ provider, state, ready, permitted: true,
  audience: 'current', host: blog.slug, url: 'https://guide.example.com' });
for (const provider of ['tailscale', 'netbird']) {
  for (const [state, ready, usable] of [['ready', true, true], ['key-expiring', true, true],
    ['starting', false, false], ['error', false, false], ['needs-login', false, false], ['unknown', false, false]]) {
    const stateFlat = { ...shareFixture, endpoints: [routeFixture(provider, state, ready)] };
    assert.equal(shareRoute(stateFlat).usable, usable, provider + ':' + state);
    if (state === 'unknown') assert.ok(!shareRoute(stateFlat).message.includes('being prepared'));
    shareDialog(stateFlat);
    const dialog = all(document.body, e => e.tagName === 'DIALOG').pop();
    assert.ok(dialog.textContent.includes(provider === 'netbird' ? 'NetBird access policies' : 'Tailscale access rules'));
    dialog.close();
  }
}
for (const patch of [{ permitted: false }, { state: 'stopped' }]) {
  assert.equal(shareRoute({ ...shareFixture, endpoints: [{ ...routeFixture('netbird', 'ready', true), ...patch }] }).usable, false);
}
for (const patch of [{ audience: 'draft' }, { host: 'another-flat' }, { provider: 'portal' }]) {
  assert.equal(shareRoute({ ...shareFixture, endpoints: [{ ...routeFixture('netbird', 'ready', true), ...patch }] }).url, shareFixture.private_url, 'unrelated route does not mask legacy selected URL');
}
assert.equal(shareRoute({ ...shareFixture, endpoints: [{ ...routeFixture('tailscale', 'ready', true), url: '' }, routeFixture('netbird', 'ready', true)] }).provider, 'netbird');
for (const state of ['error', 'needs-login', 'unavailable']) {
  assert.ok(shareRoute({ ...shareFixture, endpoints: [routeFixture('netbird', state)] }).message.includes('unavailable'));
}
assert.ok(shareRoute({ ...shareFixture, running: false }).message.includes('not running'));
for (const provider of ['portal', 'tailscale-funnel', 'zrok', 'cloudflare-quick', 'netbird-expose']) {
  const publicRoute = routeFixture(provider, 'ready', true);
  assert.equal(shareRoute({ ...shareFixture, visibility: 'public', endpoints: [routeFixture('netbird', 'ready', true), publicRoute] }).provider, provider);
}
const shareLocation = globalThis.location;
globalThis.location = { origin: 'https://console.example.com' };
const localRoute = { ...routeFixture('local', 'ready', true), url: 'http://blog.localhost:7879' };
assert.equal(shareRoute({ ...shareFixture, endpoints: [localRoute, routeFixture('netbird', 'starting')] }).provider, 'netbird');
shareDialog({ ...shareFixture, endpoints: [localRoute] });
const localShare = all(document.body, e => e.tagName === 'DIALOG').pop();
assert.ok(localShare.textContent.includes('This private link opens only on the Flats host machine.'));
localShare.close();
globalThis.location = shareLocation;
assert.ok(shareRoute({ ...shareFixture, private_url: '', private_state: '' }).message.includes('No link'));
assert.ok(shareRoute({ ...shareFixture, live_version: 0 }).message.includes('not been published'));
for (const type of ['docs', 'design', 'board']) {
  const message = shareRoute({ ...shareFixture, type, running: false }).message;
  assert.ok(message.includes('not running') && !message.includes('publish'));
}
assert.equal(shareRoute({ ...shareFixture, private_state: 'starting' }).usable, false);
assert.equal(shareRoute({ ...shareFixture, private_state: undefined }).message, 'Link available.');
assert.equal(shareRoute({ ...shareFixture, endpoints: [routeFixture('tailscale', 'starting'), routeFixture('netbird', 'ready', true)] }).provider, 'netbird');
let observedChanges = 0;
shareDialog({ ...shareFixture, private_state: 'starting' }, () => observedChanges++);
const refreshDialog = all(document.body, e => e.tagName === 'DIALOG').pop();
const refreshButton = byText(refreshDialog, 'Refresh')[0];
let finishRefresh;
routes['GET /console/api/flats/blog'] = () => new Promise(resolve => { finishRefresh = resolve; });
refreshButton.focus = () => { document.activeElement = refreshButton; };
refreshButton.focus();
refreshButton.dispatch('click');
assert.ok(radios(refreshDialog).every((r) => r.disabled === true));
assert.notEqual(refreshButton.disabled, true, 'refresh remains focusable while pending');
assert.equal(refreshButton.getAttribute('aria-disabled'), 'true');
const requestsBeforeDuplicate = calls.length;
refreshButton.dispatch('click');
assert.equal(calls.length, requestsBeforeDuplicate, 'refresh is single-flight');
finishRefresh(shareFixture);
await tick();
assert.ok(refreshDialog.textContent.includes('Link is ready.'));
assert.equal(byText(refreshDialog, 'Refresh')[0], refreshButton, 'refresh keeps the same focused element');
assert.equal(document.activeElement, refreshButton);
const focusedVisit = all(refreshDialog, e => e.tagName === 'A' && e.textContent === 'Visit')[0];
assert.ok(focusedVisit);
assert.equal(all(refreshDialog, e => e.tagName === 'BUTTON' && e.textContent.includes('Copy link'))[0].disabled, false);
document.activeElement = focusedVisit;
routes['GET /console/api/flats/blog'] = { __status: 503, error: 'unavailable' };
refreshButton.dispatch('click');
await tick();
assert.ok(refreshDialog.textContent.includes('last known state'));
assert.equal(all(refreshDialog, e => e.tagName === 'A' && e.textContent === 'Visit')[0], focusedVisit, 'unchanged refresh keeps Visit element');
assert.equal(document.activeElement, focusedVisit, 'unchanged Visit retains focus');
assert.ok(refreshDialog.textContent.includes(shareFixture.private_url));
routes['GET /console/api/flats/blog'] = () => new Promise(resolve => { finishRefresh = resolve; });
refreshButton.dispatch('click');
refreshDialog.close();
const closedText = refreshDialog.textContent;
finishRefresh({ ...shareFixture, private_url: 'https://changed.example.com' });
await tick();
assert.equal(refreshDialog.textContent, closedText, 'late response does not redraw a closed dialog');
assert.equal(observedChanges, 1, 'parent refreshes after an observed change');
// --- anonymous public editing of boards (board-public-edit.md) ---
// Labels: an editable board is never plain "Public" (rule 34).
{
  const dom = await import('./dom.js');
  const label = (vis, access) => dom.visibilityBadge(vis, access).textContent;
  assert.equal(label('private'), 'Private');
  assert.equal(label('public'), 'Public', 'a website stays Public');
  assert.equal(label('public', 'view'), 'Public, view only');
  assert.equal(label('public', 'edit'), 'Public, anyone can edit');
  assert.equal(label('private', 'edit'), 'Private', 'a Private flat has no level');
  assert.equal(dom.accessOf({ type: 'board', visibility: 'public' }), 'view', 'a Public board without a level is view only');
  assert.equal(dom.accessOf({ type: 'flat', visibility: 'public' }), 'public');
  assert.equal(dom.accessOf({ type: 'board', visibility: 'public', public_access: 'edit' }), 'edit');
}
const retro = { ...notes, type: 'board', slug: 'retro', name: 'Retro', visibility: 'public', public_access: 'view', running: true, live_version: 0,
  providers: ['portal'], private_url: 'https://retro.tail.ts.net', public_url: 'https://retro.portal.example', public_notice: PUBLIC,
  public_presence: { guests: 0, viewers: 9 } };
const retroEdit = { ...retro, public_access: 'edit', public_presence: { guests: 4, viewers: 9 } };
{
  // List rows use the new labels.
  routes['GET /console/api/flats'] = { flats: [retroEdit, retro] };
  const main = new Element('main');
  const stop = list.mount(main, [], ctx);
  await tick();
  const rowsNow = all(main, (e) => e.tagName === 'LI' && e.className === 'flat');
  assert.ok(rowsNow[0].textContent.includes('Public, anyone can edit'));
  assert.ok(rowsNow[1].textContent.includes('Public, view only'));
  stop();
  routes['GET /console/api/flats'] = listedFlats;
}
const visCalls = () => calls.filter((c) => c.key === 'POST /console/api/flats/retro/visibility').map((c) => JSON.parse(c.body));
const openDialogs = () => all(document.body, (e) => e.tagName === 'DIALOG' && e.open);
{
  // A board at view only: three choices, viewer count, no Stop guest editing.
  routes['GET /console/api/flats/retro'] = retro;
  shareDialog(retro);
  const dlg = openDialogs().pop();
  assert.deepEqual(radios(dlg).map((r) => r.value), ['private', 'view', 'edit']);
  assert.equal(radios(dlg).find((r) => r.checked).value, 'view');
  assert.ok(dlg.textContent.includes('Public, anyone can edit') && dlg.textContent.includes('Anyone on the internet can edit and delete'));
  assert.ok(dlg.textContent.includes('People on your networks'));
  assert.ok(dlg.textContent.includes('Anyone on the internet') && dlg.textContent.includes('Can view. 9 viewers on the board now.'));
  assert.equal(byText(dlg, 'Stop guest editing')[0].parentNode.hidden, true);

  // Choosing "anyone can edit" with problems shows the blocked dialog with
  // one action per problem and changes nothing.
  routes['GET /console/api/flats/retro/public-edit'] = { problems: [{ code: 'disk_quota_off', message: 'disk quota is off' }, { code: 'pin_quota_full', message: 'pin quota is full' }],
    disk_quota: 0, revision_quota: 100, pin_quota: 20, pins: 20, safe_point: null, listed: false };
  pick(dlg, 'edit');
  await tick();
  const blocked = openDialogs().pop();
  assert.ok(blocked.textContent.includes('Set these limits before anyone can edit'));
  assert.ok(blocked.textContent.includes('Disk quota is off.') && blocked.textContent.includes('Pin quota is full (20 of 20).'));
  assert.ok(blocked.textContent.includes('Revision quota is 100.'), 'a satisfied requirement is listed as met');
  const limits = all(blocked, (e) => e.tagName === 'A' && e.textContent === 'Open Limits')[0];
  assert.equal(limits.getAttribute('href'), '/settings/limits');
  assert.equal(all(blocked, (e) => e.tagName === 'A' && e.textContent === 'Open History')[0].getAttribute('href'), new URL(retro.private_url).href);
  const allow = byText(blocked, 'Allow anyone to edit')[0];
  assert.ok(allow.getAttribute('disabled') !== null && allow.getAttribute('aria-disabled') === 'true');
  byText(blocked, 'Cancel')[0].dispatch('click');
  await tick();
  assert.equal(visCalls().length, 0, 'a blocked change sends nothing');
  assert.equal(radios(dlg).find((r) => r.checked).value, 'view', 'the radios show the stored level again');

  // Without problems the confirmation states what it grants, then the
  // console self-approves an edit request.
  routes['GET /console/api/flats/retro/public-edit'] = { problems: null, disk_quota: 2 * 1024 * 1024 * 1024, revision_quota: 100, pin_quota: 20, pins: 3, safe_point: null, listed: true };
  routes['POST /console/api/flats/retro/visibility'] = pending('apr-retro-edit');
  routes['POST /console/api/approvals/apr-retro-edit/approve'] = { id: 'apr-retro-edit', status: 'approved', result: 'retro is now public, anyone can edit' };
  pick(dlg, 'edit');
  await tick();
  const confirm = openDialogs().pop();
  assert.ok(confirm.textContent.includes('Let anyone on the internet edit Retro?'));
  assert.ok(confirm.textContent.includes('safe point') && confirm.textContent.includes("can't be unpinned while guests can edit"));
  assert.ok(confirm.textContent.includes('disk quota (2.0 GB)') && confirm.textContent.includes('revision quota (100)'), confirm.textContent);
  assert.ok(confirm.textContent.includes('listed publicly by a relay'), 'a listed flat says so');
  routes['GET /console/api/flats/retro'] = retroEdit;
  confirm.close('ok');
  await tick();
  assert.deepEqual(visCalls().at(-1), { visibility: 'public', access: 'edit', reason: '' });
  assert.ok(calls.some((c) => c.key === 'POST /console/api/approvals/apr-retro-edit/approve'), 'the console approves its own request');

  // At edit: counts, the guest note and Stop guest editing, which applies at
  // once without a confirmation.
  assert.equal(radios(dlg).find((r) => r.checked).value, 'edit');
  assert.ok(dlg.textContent.includes('Can edit. 4 guests and 9 viewers on the board now.'));
  assert.equal(byText(dlg, 'Stop guest editing')[0].parentNode.hidden, false);
  assert.ok(dlg.textContent.includes("Guests can't see history."));
  routes['POST /console/api/flats/retro/visibility'] = pending('apr-retro-stop');
  routes['POST /console/api/approvals/apr-retro-stop/approve'] = { id: 'apr-retro-stop', status: 'approved', result: 'retro is now public, view only' };
  routes['GET /console/api/flats/retro'] = retro;
  const before = openDialogs().length;
  byText(dlg, 'Stop guest editing')[0].dispatch('click');
  await tick();
  assert.equal(openDialogs().length, before, 'Stop guest editing asks nothing');
  assert.deepEqual(visCalls().at(-1), { visibility: 'public', access: 'view', reason: '' });
  assert.ok(calls.some((c) => c.key === 'POST /console/api/approvals/apr-retro-stop/approve'));
  assert.equal(radios(dlg).find((r) => r.checked).value, 'view');
  dlg.close();
}
{
  // Choosing view only from edit is the same Stop, with no confirmation;
  // going Private keeps the existing confirmation.
  routes['GET /console/api/flats/retro'] = retroEdit;
  shareDialog(retroEdit);
  const dlg = openDialogs().pop();
  const n = visCalls().length;
  routes['GET /console/api/flats/retro'] = retro;
  pick(dlg, 'view');
  await tick();
  assert.equal(openDialogs().pop(), dlg, 'no confirmation for view only');
  assert.equal(visCalls().length, n + 1);
  assert.deepEqual(visCalls().at(-1), { visibility: 'public', access: 'view', reason: '' });
  pick(dlg, 'private');
  await tick();
  const priv = openDialogs().pop();
  assert.ok(priv.textContent.includes('Make Retro private?'), 'Public to Private keeps its confirmation');
  assert.ok(priv.textContent.includes('Visibility changes from Public, view only to Private.'));
  priv.close('cancel');
  await tick();
  assert.equal(visCalls().length, n + 1);
  dlg.close();
}
{
  // A private board asked for "anyone can edit" still checks its public
  // network first.
  const privRetro = { ...retro, visibility: 'private', public_access: undefined, providers: [] };
  const r = versionActions.setVisibility(privRetro, 'public', 'edit');
  await tick();
  const need = openDialogs().pop();
  assert.ok(need.textContent.includes('Allow a public network first'));
  need.close('ok');
  assert.equal(await r, null);
}
{
  // A refusal from the server shows the blocked dialog with its problems.
  routes['GET /console/api/flats/retro/public-edit'] = { problems: [], disk_quota: 1024, revision_quota: 10, pin_quota: 20, pins: 3, safe_point: 7, safe_point_pinned: true, listed: false };
  routes['POST /console/api/flats/retro/visibility'] = { __status: 409, category: 'public_edit_blocked', error: 'blocked', problems: [{ code: 'revision_quota_unlimited', message: 'unlimited' }] };
  const r = versionActions.allowGuestEditing(retro);
  await tick();
  openDialogs().pop().close('ok');
  await tick();
  const blocked = openDialogs().pop();
  assert.ok(blocked.textContent.includes('Revision quota is unlimited.'), blocked.textContent);
  assert.equal(all(blocked, (e) => e.tagName === 'A' && e.textContent === 'Open revision settings')[0].getAttribute('href'), '/flats/retro/settings#revisions');
  assert.ok(blocked.textContent.includes('The safe point r7 is kept pinned.'));
  byText(blocked, 'Cancel')[0].dispatch('click');
  assert.equal(await r, null);
}
{
  // The flat page's Sharing row: level, counts, Stop guest editing, the
  // pinned safe point and a pending visibility change.
  routes['GET /console/api/flats/retro'] = retroEdit;
  routes['GET /console/api/flats/retro/revision-quota'] = { revision_quota: 50, pin_quota: null, default_revision_quota: 100, default_pin_quota: 20, pins: 1 };
  routes['GET /console/api/flats/retro/public-edit'] = { problems: [], disk_quota: 1024, revision_quota: 50, pin_quota: 20, pins: 1, safe_point: 12, safe_point_pinned: true, listed: false };
  routes['GET /console/api/approvals?status=pending'] = { approvals: [{ id: 'apr-wait', action: 'set_visibility', flat: 'retro', status: 'pending', params: { visibility: 'private', from: 'public', from_access: 'edit' } }] };
  const main = new Element('main');
  const stop = flat.mountSettings(main, ['retro'], ctx);
  await tick();
  assert.ok(main.textContent.includes('Public, anyone can edit'));
  assert.ok(main.textContent.includes('Right now: 4 guests editing, 9 viewers watching.'));
  assert.ok(main.textContent.includes("Guests can't see history, import, upload images or send selections to an agent."));
  assert.ok(main.textContent.includes('Revision r12, before guest editing') && main.textContent.includes("It can't be unpinned until guest editing stops."));
  assert.ok(main.textContent.includes('Waiting for approval') && main.textContent.includes('Make retro private'));
  assert.equal(all(main, (e) => e.tagName === 'A' && e.textContent === 'View request')[0].getAttribute('href'), '/approvals/apr-wait');
  assert.ok(all(main, (e) => e.getAttribute('class') === 'badge badge-live').some((e) => e.textContent === 'Pinned'));
  // Stop is announced only when the host confirmed it: a refused or an
  // unconfirmed decision is reported as a failure and changes nothing shown.
  const stopToasts = new Element('div');
  const savedGet = document.getElementById;
  document.getElementById = (id) => (id === 'toasts' ? stopToasts : savedGet?.(id));
  for (const [i, answer] of [
    { __status: 500, error: 'guest editing could not be confirmed as stopped', category: 'unavailable', approval: { id: 'x', status: 'failed' } },
    { id: 'apr-retro-fail', status: 'failed', result: 'guest editing could not be confirmed as stopped' },
    { id: 'apr-retro-fail', status: 'pending' },
  ].entries()) {
    routes['POST /console/api/flats/retro/visibility'] = pending('apr-retro-fail');
    routes['POST /console/api/approvals/apr-retro-fail/approve'] = answer;
    const res = versionActions.stopGuestEditing(retroEdit);
    await tick();
    const failed = openDialogs().pop();
    assert.ok(failed && failed.textContent.includes('Stop guest editing failed'), `case ${i}: the failure is reported`);
    if (answer.result || answer.error) assert.ok(failed.textContent.includes('could not be confirmed as stopped'), `case ${i}: ${failed.textContent}`);
    failed.close('ok');
    assert.equal(await res, null, `case ${i}`);
    assert.ok(!stopToasts.textContent.includes('Guest editing stopped'), `case ${i}: no success toast`);
  }
  routes['POST /console/api/flats/retro/visibility'] = pending('apr-retro-stop2');
  routes['POST /console/api/approvals/apr-retro-stop2/approve'] = { id: 'apr-retro-stop2', status: 'approved' };
  routes['GET /console/api/flats/retro'] = retro;
  byText(main, 'Stop guest editing')[0].dispatch('click');
  await tick();
  assert.deepEqual(visCalls().at(-1), { visibility: 'public', access: 'view', reason: '' });
  assert.equal(openDialogs().length, 0, 'no confirmation from the flat page either');
  assert.ok(main.textContent.includes('Public, view only'));
  assert.equal(byText(main, 'Stop guest editing').length, 0);
  assert.ok(stopToasts.textContent.includes('Guest editing stopped'), 'a confirmed Stop is announced');
  document.getElementById = savedGet;
  stop();
  // After Stop the safe point can be unpinned in History; reloading the
  // settings shows its real state.
  routes['GET /console/api/flats/retro/public-edit'] = { problems: [], disk_quota: 1024, revision_quota: 50, pin_quota: 20, pins: 0, safe_point: 12, safe_point_pinned: false, listed: false };
  const again = new Element('main');
  const stopAgain = flat.mountSettings(again, ['retro'], ctx);
  await tick();
  const badgeTexts = (root) => all(root, (e) => (e.getAttribute('class') || '').split(' ').includes('badge')).map((e) => e.textContent);
  assert.ok(badgeTexts(again).includes('Not pinned') && !badgeTexts(again).includes('Pinned'), badgeTexts(again).join('|'));
  assert.ok(again.textContent.includes('Retention can delete it like any unpinned revision.'));
  stopAgain();
  // A safe point that no longer exists is not shown.
  routes['GET /console/api/flats/retro/public-edit'] = { problems: [], disk_quota: 1024, revision_quota: 50, pin_quota: 20, pins: 0, safe_point: null, safe_point_pinned: false, listed: false };
  const gone = new Element('main');
  const stopGone = flat.mountSettings(gone, ['retro'], ctx);
  await tick();
  assert.ok(!gone.textContent.includes('Safe point'));
  stopGone();
  routes['GET /console/api/approvals?status=pending'] = { approvals: [] };
}
{
  // The approval page shows the requested and the earlier level and the
  // safe point note; a request that no longer matches has expired.
  assert.equal(approvalsPage.describeApproval({ action: 'set_visibility', flat: 'retro', params: { visibility: 'public', access: 'edit' } }), 'Make retro public, anyone can edit');
  assert.equal(approvalsPage.describeApproval({ action: 'set_visibility', flat: 'blog', params: { visibility: 'public' } }), 'Make blog public');
  routes['GET /console/api/flats/retro'] = retro;
  routes['GET /console/api/approvals/apr-edit'] = { id: 'apr-edit', flat: 'retro', action: 'set_visibility', status: 'pending', via: 'mcp', requested_at: '2026-10-03T00:00:00Z',
    params: { visibility: 'public', access: 'edit', from: 'public', from_access: 'view', access_seq: 3 } };
  let main = new Element('main');
  approvalsPage.mount(main, ['apr-edit'], ctx); await tick();
  const dd = all(main, (e) => e.tagName === 'DD').map((e) => e.textContent);
  assert.ok(dd.includes('Public, view only') && dd.includes('Public, anyone can edit'), dd.join(' | '));
  assert.ok(main.textContent.includes('A safe point of the board will be pinned first.'));
  assert.ok(main.textContent.includes('If the visibility changes before you approve, it expires.'));
  assert.equal(byText(main, 'Approve…').length, 1);
  routes['GET /console/api/flats/retro'] = { ...retro, visibility: 'private', public_access: undefined };
  main = new Element('main');
  approvalsPage.mount(main, ['apr-edit'], ctx); await tick();
  assert.ok(main.textContent.includes("This request can't be approved."));
  assert.equal(byText(main, 'Approve…').length, 0, 'an expired request cannot be approved');
  assert.equal(byText(main, 'Dismiss…').length, 1);
  assert.ok(all(main, (e) => e.tagName === 'DD').some((e) => e.textContent === 'Private'), 'the current state is shown');
  // A legacy request without from_access still matches a view-only board.
  routes['GET /console/api/flats/retro'] = retro;
  routes['GET /console/api/approvals/apr-legacy'] = { id: 'apr-legacy', flat: 'retro', action: 'set_visibility', status: 'pending', via: 'mcp', requested_at: '2026-10-03T00:00:00Z',
    params: { visibility: 'private', from: 'public' } };
  main = new Element('main');
  approvalsPage.mount(main, ['apr-legacy'], ctx); await tick();
  assert.equal(byText(main, 'Approve…').length, 1);
}

// --- anonymous public editing of documents (docs-public-edit.md) ---
// The same components as for a board, with the documents' nouns (rule 36).
{
  const dom = await import('./dom.js');
  assert.equal(dom.accessOf({ type: 'docs', visibility: 'public' }), 'view', 'a Public document without a level is view only');
  assert.equal(dom.accessOf({ type: 'docs', visibility: 'public', public_access: 'edit' }), 'edit');
  assert.equal(dom.accessOf({ type: 'design', visibility: 'public' }), 'public', 'a design has no level');
}
const minutes = { ...notes, slug: 'minutes', name: 'Minutes', visibility: 'public', public_access: 'view', providers: ['portal'],
  private_url: 'https://minutes.tail.ts.net', public_url: 'https://minutes.portal.example', public_notice: PUBLIC, public_presence: { guests: 0, viewers: 2 } };
const minutesEdit = { ...minutes, public_access: 'edit', public_presence: { guests: 3, viewers: 2 } };
{
  const docVis = () => calls.filter((c) => c.key === 'POST /console/api/flats/minutes/visibility').map((c) => JSON.parse(c.body));
  const openDialogs = () => all(document.body, (e) => e.tagName === 'DIALOG' && e.open);
  const toasts = new Element('div');
  const savedGet = document.getElementById;
  document.getElementById = (id) => (id === 'toasts' ? toasts : savedGet?.(id));
  // At view only: three choices with the documents' description, networks
  // can edit, and the viewer count.
  routes['GET /console/api/flats/minutes'] = minutes;
  shareDialog(minutes);
  const dlg = openDialogs().pop();
  assert.deepEqual(radios(dlg).map((r) => r.value), ['private', 'view', 'edit']);
  assert.equal(radios(dlg).find((r) => r.checked).value, 'view');
  assert.ok(dlg.textContent.includes('Anyone with the link can read live. Only your networks edit.'), dlg.textContent);
  assert.ok(!dlg.textContent.includes('can watch live'));
  const networksRow = all(dlg, (e) => e.className === 'share-person').find((e) => e.textContent.includes('People on your networks'));
  assert.ok(networksRow.textContent.includes('Can edit'), networksRow.textContent);
  assert.ok(dlg.textContent.includes('Can view. 2 viewers on the documents now.'));
  assert.equal(byText(dlg, 'Stop guest editing')[0].parentNode.hidden, true);

  // The blocked dialog is the board's.
  routes['GET /console/api/flats/minutes/public-edit'] = { problems: [{ code: 'revision_quota_unlimited', message: 'unlimited' }], disk_quota: 1024, revision_quota: 0, pin_quota: 20, pins: 0, safe_point: null, listed: false };
  pick(dlg, 'edit');
  await tick();
  const blocked = openDialogs().pop();
  assert.ok(blocked.textContent.includes('Set these limits before anyone can edit') && blocked.textContent.includes('Revision quota is unlimited.'));
  assert.equal(all(blocked, (e) => e.tagName === 'A' && e.textContent === 'Open revision settings')[0].getAttribute('href'), '/flats/minutes/settings#revisions');
  byText(blocked, 'Cancel')[0].dispatch('click');
  await tick();
  assert.equal(docVis().length, 0);

  // The confirmation names the documents and what guests cannot do.
  routes['GET /console/api/flats/minutes/public-edit'] = { problems: [], disk_quota: 1024 * 1024 * 1024, revision_quota: 100, pin_quota: 20, pins: 0, safe_point: null, listed: false };
  routes['POST /console/api/flats/minutes/visibility'] = pending('apr-min-edit');
  routes['POST /console/api/approvals/apr-min-edit/approve'] = { id: 'apr-min-edit', status: 'approved' };
  pick(dlg, 'edit');
  await tick();
  const confirm = openDialogs().pop();
  assert.ok(confirm.textContent.includes('Let anyone on the internet edit Minutes?'));
  assert.ok(confirm.textContent.includes('Anyone who opens the link can add, change and delete all the text of every document in this flat.'), confirm.textContent);
  assert.ok(confirm.textContent.includes('safe point of the documents as it is now'));
  assert.ok(confirm.textContent.includes("Guests can't see history, restore earlier text or add files."));
  assert.ok(!confirm.textContent.includes('board'), 'no board wording for documents');
  routes['GET /console/api/flats/minutes'] = minutesEdit;
  confirm.close('ok');
  await tick();
  assert.deepEqual(docVis().at(-1), { visibility: 'public', access: 'edit', reason: '' });
  assert.ok(toasts.textContent.includes('Anyone with the link can now edit these documents.'), toasts.textContent);

  // At edit: counts, the documents' guest note and Stop guest editing.
  assert.equal(radios(dlg).find((r) => r.checked).value, 'edit');
  assert.ok(dlg.textContent.includes('Can edit. 3 guests and 2 viewers on the documents now.'));
  assert.ok(dlg.textContent.includes("Guests can't see history. They can't restore earlier text or add files. You can restore any earlier state from History."), dlg.textContent);
  assert.ok(!dlg.textContent.includes('board'));
  routes['POST /console/api/flats/minutes/visibility'] = pending('apr-min-stop');
  routes['POST /console/api/approvals/apr-min-stop/approve'] = { id: 'apr-min-stop', status: 'approved' };
  routes['GET /console/api/flats/minutes'] = minutes;
  byText(dlg, 'Stop guest editing')[0].dispatch('click');
  await tick();
  assert.deepEqual(docVis().at(-1), { visibility: 'public', access: 'view', reason: '' });
  assert.ok(toasts.textContent.includes('Guest editing stopped. Viewers can still read.'));
  dlg.close();
  document.getElementById = savedGet;

  // The flat page's Sharing row and safe point slot.
  routes['GET /console/api/flats/minutes'] = minutesEdit;
  routes['GET /console/api/flats/minutes/revision-quota'] = { revision_quota: 50, pin_quota: null, default_revision_quota: 100, default_pin_quota: 20, pins: 1 };
  routes['GET /console/api/flats/minutes/public-edit'] = { problems: [], disk_quota: 1024, revision_quota: 50, pin_quota: 20, pins: 1, safe_point: 5, safe_point_pinned: true, listed: false };
  const main = new Element('main');
  const stop = flat.mountSettings(main, ['minutes'], ctx);
  await tick();
  assert.ok(main.textContent.includes('Who can open these documents'));
  assert.ok(main.textContent.includes('Public, anyone can edit'));
  assert.ok(main.textContent.includes('Right now: 3 guests editing, 2 viewers reading.'), main.textContent);
  assert.ok(main.textContent.includes("Guests can't see history, restore earlier text or add files."));
  assert.ok(main.textContent.includes('Revision r5, before guest editing'));
  assert.equal(byText(main, 'Stop guest editing').length, 1);
  stop();

  // The approval page.
  routes['GET /console/api/approvals/apr-doc-edit'] = { id: 'apr-doc-edit', flat: 'minutes', action: 'set_visibility', status: 'pending', via: 'mcp', requested_at: '2026-10-03T00:00:00Z',
    params: { visibility: 'public', access: 'edit', from: 'public', from_access: 'view', access_seq: 2 } };
  routes['GET /console/api/flats/minutes'] = minutes;
  const page = new Element('main');
  approvalsPage.mount(page, ['apr-doc-edit'], ctx); await tick();
  const dd = all(page, (e) => e.tagName === 'DD').map((e) => e.textContent);
  assert.ok(dd.includes('Public, view only') && dd.includes('Public, anyone can edit'), dd.join(' | '));
  assert.ok(page.textContent.includes('can add, change and delete all the text of every document in this flat.'));
  assert.ok(page.textContent.includes('A safe point of the documents will be pinned first.'));
  assert.equal(byText(page, 'Approve…').length, 1);
  byText(page, 'Approve…')[0].dispatch('click');
  await tick();
  const sure = openDialogs().pop();
  assert.ok(sure.textContent.includes('will be able to add, change and delete all the text of every document in this flat.'), sure.textContent);
  sure.close('cancel');
  await tick();
}

// --- reverse proxy ---
// The reverse proxy is the proxy (Private) and proxy-public (Public)
// providers on one listener and one address template.
{
  const DETAIL = 'Served through your reverse proxy. Flats cannot check that the proxy reaches it.';
  const openDialogs = () => all(document.body, (e) => e.tagName === 'DIALOG' && e.open !== false);
  const proxyOff = (id, scope) => ({ ...provider(id, scope, false), address: 'http://127.0.0.1:7880', template: 'https://{host}.site.example.org' });
  const proxyOn = { ...proxyOff('proxy', 'private'), enabled: true, configured: true, flats: ['blog'],
    status: { kind: 'proxy', enabled: true, hosts: [{ host: 'blog', url: 'https://blog.site.example.org', state: 'ready', detail: DETAIL }] } };
  const withProxy = (...ps) => ({ providers: [provider('local', 'private', true), provider('tailscale', 'private', false), ...ps] });
  const proxySettings = () => settingsRoute({ ...CONFIG, etag: 'etag-px', pinned: {},
    keys: { ...CONFIG.keys, proxy_url_template: 'proxy.url_template' } });
  const withTemplate = (template) => ({ ...proxySettings(), settings: { ...SETTINGS, proxy_url_template: template } });

  // Networks lists both kinds under their own groups with readable names.
  routes['GET /console/api/settings'] = withTemplate('https://{host}.site.example.org');
  routes['GET /console/api/providers'] = withProxy(proxyOn, proxyOff('proxy-public', 'public'));
  let net = await mountSettings();
  assert.ok(byId(net, 'providers-private').textContent.includes('Reverse proxy'), byId(net, 'providers-private').textContent);
  assert.ok(byId(net, 'providers-public').textContent.includes('Reverse proxy (public)'));
  assert.equal(byId(net, 'provider-proxy').getAttribute('href'), '/settings/networks/proxy');
  assert.ok(byId(net, 'provider-proxy').textContent.includes('On'));
  // A kind that is on can be the default for new flats, under its name.
  const privDefault = byId(net, 'set-default_private_provider');
  assert.deepEqual(all(privDefault, (e) => e.tagName === 'OPTION').map((o) => [o.getAttribute('value'), o.textContent]), [['local', 'Local only'], ['proxy', 'Reverse proxy']]);

  // The page of either kind holds the template, the listener address and
  // the kind, with the warning for the Private kind and no separate switch.
  let px = await mountSettings(['networks', 'proxy']);
  assert.equal(byId(px, 'set-proxy_url_template').value, 'https://{host}.site.example.org');
  assert.ok(byId(px, 'proxy-listener').textContent.includes('http://127.0.0.1:7880'));
  assert.ok(byId(px, 'proxy-listener').textContent.includes('flats config set proxy.addr'));
  assert.equal(byId(px, 'proxy-kind').value, 'proxy');
  assert.equal(all(px, (e) => e.tagName === 'BUTTON' && /^Turn (on|off)$/.test(e.textContent)).length, 0, 'the kind replaces the switch');
  assert.ok(px.textContent.includes('Flats does not check who they are') && px.textContent.includes('only access control'), 'the Private kind warning is shown');
  assert.ok(!byId(px, 'proxy-kind').disabled, 'a kind can be chosen while a template is saved');

  // Changing the template while a kind is on asks first and names the
  // approvals it makes stale; cancelling saves nothing.
  putCount = puts().length;
  byId(px, 'set-proxy_url_template').value = 'https://{host}.flats.example.org';
  byId(px, 'proxy-template-form').dispatch('input');
  byId(px, 'proxy-template-form').dispatch('submit');
  await tick();
  let confirmTemplate = openDialogs().pop();
  assert.ok(confirmTemplate.textContent.includes('Change the address template?'));
  assert.ok(confirmTemplate.textContent.includes('Every pending approval becomes stale'), confirmTemplate.textContent);
  assert.ok(confirmTemplate.textContent.includes('version deployments and visibility changes'));
  confirmTemplate.close('cancel');
  await tick();
  assert.equal(puts().length, putCount, 'a cancelled template change saves nothing');
  byId(px, 'proxy-template-form').dispatch('submit');
  await tick();
  openDialogs().pop().close('ok');
  await tick();
  assert.equal(puts().length, putCount + 1);
  assert.deepEqual(JSON.parse(puts().at(-1).body), { proxy_url_template: 'https://{host}.flats.example.org' });
  assert.equal(puts().at(-1).headers['If-Match'], '"etag-px"');

  // Turning the kind off asks first and also names the stale approvals.
  px = await mountSettings(['networks', 'proxy']);
  routes['PUT /console/api/providers/proxy'] = { providers: [] };
  byId(px, 'proxy-kind').value = '';
  byId(px, 'proxy-kind').dispatch('change');
  await tick();
  const turnOff = openDialogs().pop();
  assert.ok(turnOff.textContent.includes('Turn off Reverse proxy?'), turnOff.textContent);
  assert.ok(turnOff.textContent.includes('Every pending approval becomes stale'));
  turnOff.close('ok');
  await tick();
  assert.deepEqual(JSON.parse(calls.filter((c) => c.key === 'PUT /console/api/providers/proxy').at(-1).body), { enabled: false });

  // Picking the other kind while one is on sends nothing and asks for no
  // confirmation: it explains the switch and keeps the current kind.
  px = await mountSettings(['networks', 'proxy']);
  const switchCalls = calls.filter((c) => c.key.startsWith('PUT /console/api/providers/')).length;
  byId(px, 'proxy-kind').value = 'proxy-public';
  byId(px, 'proxy-kind').dispatch('change');
  await tick();
  const switchKind = openDialogs().pop();
  assert.ok(switchKind.textContent.includes('Turn off Reverse proxy first'), switchKind.textContent);
  assert.ok(switchKind.textContent.includes('stop allowing Reverse proxy on every flat, choose Off here, then choose Public'), switchKind.textContent);
  assert.ok(!switchKind.textContent.includes('Saved to config.json'), 'no change is offered');
  assert.equal(byText(switchKind, 'Cancel').length, 0, 'it is an explanation, not a confirmation');
  switchKind.close('ok');
  await tick();
  assert.equal(calls.filter((c) => c.key.startsWith('PUT /console/api/providers/')).length, switchCalls, 'no request is sent');
  assert.equal(byId(px, 'proxy-kind').value, 'proxy', 'the current kind stays chosen');
  assert.ok(!byId(px, 'proxy-kind').disabled);

  // While the Public kind is on, the Private kind's warning is not shown.
  routes['GET /console/api/providers'] = withProxy(proxyOff('proxy', 'private'), { ...proxyOff('proxy-public', 'public'), enabled: true, configured: true });
  px = await mountSettings(['networks', 'proxy-public']);
  assert.equal(byId(px, 'proxy-kind').value, 'proxy-public');
  assert.ok(!px.textContent.includes('Flats does not check who they are'), 'no Private kind warning while Public is on');

  // Off with a template: choosing Private warns about Private access and the
  // stale approvals before turning proxy on.
  routes['GET /console/api/providers'] = withProxy(proxyOff('proxy', 'private'), proxyOff('proxy-public', 'public'));
  px = await mountSettings(['networks', 'proxy-public']);
  assert.equal(byId(px, 'proxy-kind').value, '');
  assert.ok(px.textContent.includes('Flats does not check who they are'), 'the Private kind warning is shown while it can be chosen');
  // Choosing Public asks first, with the public warning and not the Private one.
  byId(px, 'proxy-kind').value = 'proxy-public';
  byId(px, 'proxy-kind').dispatch('change');
  await tick();
  const turnOnPublic = openDialogs().pop();
  assert.ok(turnOnPublic.textContent.includes('Turn on Reverse proxy (public)?'), turnOnPublic.textContent);
  assert.ok(turnOnPublic.textContent.includes('Every pending approval becomes stale'));
  assert.ok(!turnOnPublic.textContent.includes('Flats does not check who they are'));
  turnOnPublic.close('cancel');
  await tick();
  assert.equal(byId(px, 'proxy-kind').value, '');
  byId(px, 'proxy-kind').value = 'proxy';
  byId(px, 'proxy-kind').dispatch('change');
  await tick();
  const turnOn = openDialogs().pop();
  assert.ok(turnOn.textContent.includes('Turn on Reverse proxy?'));
  assert.ok(turnOn.textContent.includes('Flats does not check who they are'), 'the confirmation repeats the Private kind warning');
  assert.ok(turnOn.textContent.includes('Every pending approval becomes stale'));
  turnOn.close('ok');
  await tick();
  assert.deepEqual(JSON.parse(calls.filter((c) => c.key === 'PUT /console/api/providers/proxy').at(-1).body), { enabled: true });
  // A template changed while both kinds are off needs no confirmation.
  px = await mountSettings(['networks', 'proxy']);
  putCount = puts().length;
  byId(px, 'set-proxy_url_template').value = 'https://{host}.other.example.org';
  byId(px, 'proxy-template-form').dispatch('input');
  byId(px, 'proxy-template-form').dispatch('submit');
  await tick();
  assert.equal(puts().length, putCount + 1, 'no approval depends on the template while the proxy is off');

  // Without a template no kind can be turned on, and the page says why.
  routes['GET /console/api/settings'] = withTemplate('');
  routes['GET /console/api/providers'] = withProxy({ ...proxyOff('proxy', 'private'), template: undefined }, { ...proxyOff('proxy-public', 'public'), template: undefined });
  px = await mountSettings(['networks', 'proxy']);
  assert.equal(byId(px, 'proxy-kind').disabled, true);
  assert.ok(byId(px, 'proxy-listener').textContent.includes('Save an address template first.'));
  net = await mountSettings();
  assert.ok(byId(net, 'provider-proxy').textContent.includes('Needs an address template'));

  // A listener that did not bind at start is shown with its error.
  routes['GET /console/api/settings'] = withTemplate('https://{host}.site.example.org');
  routes['GET /console/api/providers'] = withProxy({ ...proxyOn, bind_error: 'the reverse proxy listener cannot bind proxy.addr 127.0.0.1:7880: address already in use' }, proxyOff('proxy-public', 'public'));
  px = await mountSettings(['networks', 'proxy']);
  assert.ok(px.textContent.includes('The listener did not start: the reverse proxy listener cannot bind proxy.addr 127.0.0.1:7880: address already in use'), px.textContent);
  assert.ok(px.textContent.includes('Error'));
  // The Status does not claim that nothing is served yet for want of flats.
  routes['GET /console/api/providers'] = withProxy({ ...proxyOn, bind_error: 'the reverse proxy listener cannot bind proxy.addr 127.0.0.1:7880: address already in use',
    status: { kind: 'proxy', enabled: true, detail: 'the reverse proxy listener cannot bind proxy.addr 127.0.0.1:7880: address already in use', hosts: [] } }, proxyOff('proxy-public', 'public'));
  px = await mountSettings(['networks', 'proxy']);
  assert.ok(!byId(px, 'status').textContent.includes('Nothing served yet'), byId(px, 'status').textContent);
  assert.ok(byId(px, 'status').textContent.includes('The listener did not start'));
  // A flat's Networks row shows its proxy route in error with the reason.
  const failing = { ...blog, visibility: 'private', providers: ['proxy'],
    endpoints: [ep('local', 'http://blog.localhost:7879/'), { provider: 'proxy', audience: 'current', host: 'blog', permitted: true, configured: true, state: 'error',
      detail: 'the reverse proxy listener cannot bind proxy.addr 127.0.0.1:7880: address already in use' }] };
  routes['GET /console/api/flats/blog'] = failing;
  const failingFlat = new Element('main');
  const stopFailingFlat = flat.mountSettings(failingFlat, ['blog'], ctx);
  await tick();
  const failingRow = byId(failingFlat, 'net-proxy').parentNode;
  assert.ok(failingRow.textContent.includes('error') && failingRow.textContent.includes('cannot bind proxy.addr'), failingRow.textContent);
  assert.equal(all(failingRow, (e) => (e.getAttribute('class') || '').includes('badge-error')).length, 1);
  stopFailingFlat();
  routes['GET /console/api/flats/blog'] = blog;

  // A flat's Networks list names both kinds and shows the proxy address with
  // the note that Flats cannot check the proxy.
  const proxied = { ...blog, visibility: 'private', providers: ['proxy'], private_url: 'https://blog.site.example.org', private_state: 'ready', private_detail: DETAIL,
    endpoints: [ep('local', 'http://blog.localhost:7879/'), { ...ep('proxy', 'https://blog.site.example.org'), host: 'blog', ready: true, detail: DETAIL }] };
  routes['GET /console/api/flats/blog'] = proxied;
  const proxyFlat = new Element('main');
  const stopProxyFlat = flat.mountSettings(proxyFlat, ['blog'], ctx);
  await tick();
  const proxyRow = byId(proxyFlat, 'net-proxy').parentNode;
  assert.ok(proxyRow.textContent.includes('Reverse proxy') && proxyRow.textContent.includes(DETAIL), proxyRow.textContent);
  assert.equal(all(proxyRow, (e) => e.tagName === 'A')[0].getAttribute('href'), 'https://blog.site.example.org/');
  assert.ok(byId(proxyFlat, 'net-proxy-public').parentNode.textContent.includes('Reverse proxy (public)'));
  stopProxyFlat();
  // The Status section shows the same note next to the address.
  const proxyStatus = new Element('main');
  const stopProxyStatus = flat.mount(proxyStatus, ['blog'], ctx);
  await tick();
  assert.ok(byId(proxyStatus, 'status').textContent.includes(DETAIL));
  stopProxyStatus();
  routes['GET /console/api/flats/blog'] = blog;

  // Open menus and links name the networks; proxy-public is a public route.
  const both = { ...blog, visibility: 'public', live_version: 1, endpoints: [ep('local', 'http://blog.localhost:7879/'),
    ep('proxy', 'https://blog.site.example.org/'), ep('proxy-public', 'https://blog.public.example.org/')] };
  assert.deepEqual(ui.openMenuItems(both).map((i) => i.label),
    ['Open publicly · Reverse proxy (public)', 'Open privately · Local', 'Open privately · Reverse proxy']);

  // Seen from another device, a proxy address is an ordinary link while a
  // Local one stays server only. The server-only note names the reverse
  // proxy only while a proxy kind is on for the host.
  globalThis.location = { origin: 'https://flats.example.org', hostname: 'flats.example.org' };
  ui.setHostProviders(withProxy(proxyOff('proxy', 'private'), proxyOff('proxy-public', 'public')).providers);
  assert.ok(!ui.SERVER_ONLY.includes('Reverse proxy'), ui.SERVER_ONLY);
  assert.equal(ui.extLink('https://blog.site.example.org/').tagName, 'A', 'a proxy address opens from another device');
  const localOnly = ui.extLink('http://blog.localhost:7879/', 'Open');
  assert.equal(localOnly.tagName, 'SPAN');
  assert.ok(localOnly.textContent.includes('server only'));
  ui.setHostProviders(withProxy(proxyOn, proxyOff('proxy-public', 'public')).providers);
  assert.ok(ui.SERVER_ONLY.includes('Reverse proxy') && ui.SERVER_ONLY.includes('Settings → Networks → Private'), ui.SERVER_ONLY);
  assert.equal(ui.extLink('http://blog.localhost:7879/', 'Open').getAttribute('title'), ui.SERVER_ONLY);
  ui.setHostProviders(withProxy(proxyOff('proxy', 'private'), { ...proxyOff('proxy-public', 'public'), enabled: true }).providers);
  assert.ok(ui.SERVER_ONLY.includes('Reverse proxy (public) under Public'), ui.SERVER_ONLY);
  // The share dialog names the proxy for a private proxy link.
  ui.setHostProviders(withProxy(proxyOn).providers);
  shareDialog({ ...proxied, live_version: 1 });
  const proxyShare = all(document.body, (e) => e.tagName === 'DIALOG').pop();
  assert.ok(proxyShare.textContent.includes('whoever your reverse proxy lets through'), proxyShare.textContent);
  assert.equal(all(proxyShare, (e) => e.tagName === 'BUTTON' && e.textContent.includes('Copy link'))[0].getAttribute('disabled'), null);
  proxyShare.close();
  ui.setHostProviders([]);
  globalThis.location = { origin: 'http://127.0.0.1:7878' };
  routes['GET /console/api/settings'] = settingsRoute(CONFIG);
  routes['GET /console/api/providers'] = PROVIDERS;
  delete routes['PUT /console/api/providers/proxy'];
}

// --- first paint does not wait for the host's providers ---
// app.js draws the page at once and reads /console/api/providers in the
// background: a request that never answers leaves the console working with
// the note without the proxy, and one that answers later updates the note
// on links already drawn.
{
  const main = new Element('main');
  document.body.appendChild(main);
  const savedGet = document.getElementById;
  const savedLocation = globalThis.location;
  document.getElementById = (id) => (id === 'main' ? main : null);
  document.querySelectorAll = () => [];
  globalThis.window = { addEventListener() {}, scrollTo() {} };
  globalThis.history = { pushState(_s, _t, path) { globalThis.location = { ...globalThis.location, pathname: path }; }, replaceState() {} };
  globalThis.location = { origin: 'https://flats.example.org', hostname: 'flats.example.org', pathname: '/', search: '', hash: '', href: 'https://flats.example.org/' };
  const savedFlats = routes['GET /console/api/flats'];
  routes['GET /console/api/flats'] = { flats: [{ ...blog, visibility: 'private', live_version: 1, endpoints: [ep('local', 'http://blog.localhost:7879/')] }] };
  const DETAIL = 'Served through your reverse proxy. Flats cannot check that the proxy reaches it.';
  const proxyOnList = [{ id: 'proxy', scope: 'private', enabled: true, configured: true, flats: ['blog'], status: { kind: 'proxy', enabled: true, hosts: [{ host: 'blog', state: 'ready', detail: DETAIL }] } }];
  ui.setHostProviders([]);
  const noProxyNote = ui.SERVER_ONLY;
  const serverOnlyButtons = () => all(main, (e) => e.tagName === 'BUTTON' && (e.getAttribute('class') || '').includes('server-only'));

  // A providers request that never answers.
  routes['GET /console/api/providers'] = () => new Promise(() => {});
  const hung = await import('./app.js?hung');
  await tick();
  assert.equal(all(main, (e) => e.tagName === 'LI' && e.className === 'flat').length, 1, 'the list draws while providers hang');
  assert.ok(serverOnlyButtons().length > 0, 'the Local link is server only');
  assert.equal(serverOnlyButtons()[0].getAttribute('title'), noProxyNote);
  assert.equal(ui.SERVER_ONLY, noProxyNote);
  hung.navigate('/nowhere'); // stops the list page's timers

  // A providers request that answers after the first page.
  let answer;
  routes['GET /console/api/providers'] = () => new Promise((resolve) => { answer = resolve; });
  globalThis.location = { ...globalThis.location, pathname: '/' };
  const late = await import('./app.js?late');
  await tick();
  assert.equal(all(main, (e) => e.tagName === 'LI' && e.className === 'flat').length, 1, 'the list draws before providers answer');
  assert.equal(serverOnlyButtons()[0].getAttribute('title'), noProxyNote);
  answer({ providers: proxyOnList });
  await tick();
  assert.ok(ui.SERVER_ONLY.includes('Reverse proxy'), ui.SERVER_ONLY);
  const button = serverOnlyButtons()[0];
  assert.equal(button.getAttribute('title'), ui.SERVER_ONLY, 'a link drawn before the answer gets the proxy note');
  assert.ok(button.childNodes[0].getAttribute('aria-label').includes(ui.SERVER_ONLY));
  late.navigate('/nowhere');

  ui.setHostProviders([]);
  main.remove();
  routes['GET /console/api/flats'] = savedFlats;
  routes['GET /console/api/providers'] = PROVIDERS;
  document.getElementById = savedGet;
  globalThis.location = savedLocation;
}

console.log('ok');
