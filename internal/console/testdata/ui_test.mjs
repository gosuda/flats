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
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  querySelectorAll(sel) {
    const out = [];
    const walk = (node) => {
      if (!(node instanceof Element)) return;
      if (sel.startsWith('.') ? (node.className || '').split(/\s+/).includes(sel.slice(1)) : node.tagName === String(sel).toUpperCase()) out.push(node);
      for (const c of node.childNodes) walk(c);
    };
    for (const c of this.childNodes) walk(c);
    return out;
  }
  get childElementCount() { return this.childNodes.filter((c) => c instanceof Element).length; }
  get lastElementChild() { return [...this.childNodes].reverse().find((c) => c instanceof Element) || null; }
  focus() {}
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
const tick = () => new Promise((r) => setTimeout(r, 20));

// --- stubbed API ---
const UNLISTED = 'Unlisted only hides this flat from Portal relay listings. It is NOT access control: anyone with the URL can open it.';
const blog = {
  slug: 'blog', name: 'Blog', visibility: 'public', publication: 'published', live_version: 2, versions: 2,
  private_url: 'https://blog.tail.ts.net', public_url: 'https://blog.portal.example', public_notice: UNLISTED,
  private_state: 'ready', connection: { state: 'ready', detail: '' },
  providers: ['local', 'portal'],
  draft: { revision: 9, hash: 'abc123draft', base_version: 2, dirty: true, updated_at: '2026-10-03T00:00:00Z', role: 'draft', number: 0 },
  live: { number: 2, kind: 'server' }, created_at: '2026-10-01T00:00:00Z', updated_at: '2026-10-02T00:00:00Z', disk_bytes: 10,
};
// An older server may omit the notice; the console must still show one.
const shop = { ...blog, slug: 'shop', name: 'Shop', visibility: 'public-listed', public_url: 'https://shop.portal.example', public_notice: undefined, providers: ['local', 'tailscale-funnel'] };
const notes = { ...blog, slug: 'notes', name: 'Notes', visibility: 'private', publication: 'unpublished', live_version: 0, public_url: undefined, public_notice: undefined, providers: ['local'], draft: null, connection: 'starting' };
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
  const status = body && body.__status ? body.__status : 200;
  return { ok: status < 400, status, json: async () => body };
};

const { publicNoticeOf, PUBLIC_URL_NOTICE, LISTED_NOTICE } = await import('./dom.js');
assert.equal(publicNoticeOf(blog), UNLISTED);
assert.equal(publicNoticeOf(shop), LISTED_NOTICE);
assert.equal(publicNoticeOf({ public_url: 'https://x.example' }), PUBLIC_URL_NOTICE);
assert.equal(publicNoticeOf(notes), '');

// List rows show visibility badges without repeated public-access banners.
const ctx = { setTitle() {}, alive: () => true, navigate() {} };
const list = await import('./list.js');
const listMain = new Element('main');
const stopList = list.mount(listMain, [], ctx);
await tick();
const rows = all(listMain, (e) => e.tagName === 'LI' && e.className === 'flat');
assert.equal(rows.length, 3);
for (const row of rows) {
  assert.equal(all(row, (e) => e.className.includes('notice-text')).length, 0);
}
for (const [row, site] of rows.map((row, i) => [row, [blog, shop, notes][i]])) {
 const name = all(row, (e) => e.tagName === 'A' && e.className === 'flat-name')[0];
 assert.equal(name.getAttribute('href'), `/flats/${site.slug}`);
 assert.equal(name.getAttribute('data-nav'), '');
 assert.equal(name.getAttribute('target'), null);
 assert.equal(row.textContent.includes('This flat is public'), false);
 assert.equal(row.textContent.toLowerCase().includes('only me'), false);
 assert.equal(row.textContent.toLowerCase().includes('only you'), false);
}
stopList();

// The flat page offers a redeploy of the live version (and only of it), also
// from the secrets panel, and the deploy result shows the notice.
const flat = await import('./flat.js');
const flatMain = new Element('main');
const stopFlat = flat.mount(flatMain, ['blog'], ctx);
await tick();
byText(flatMain, 'Operations')[0].dispatch('click');
await tick();
const redeploys = byText(flatMain, 'Redeploy (apply secrets)');
assert.ok(redeploys.length >= 2, 'want a redeploy button on the current version and in the secrets panel');
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
// Management pages use real API data and preserve the deployment tools.
const management = new Element('main');
const stopSettings = flat.mountSettings(management, ['blog'], ctx);
await tick();
assert.equal(all(management, (e) => e.tagName === 'A' && e.textContent === 'Scheduled').length, 0);
for (const tab of ['Settings', 'Analytics', 'Database']) {
 assert.equal(all(management, (e) => e.tagName === 'A' && e.textContent === tab).length, 1);
}
assert.equal(byText(management, 'Add variable').length, 1);
byText(management, 'Add variable')[0].dispatch('click');
assert.ok(all(management, (e) => e.tagName === 'FORM' && e.className === 'form-grid').some((e) => !e.hidden));
stopSettings();
const analytics = await import('./analytics.js');
assert.deepEqual(analytics.dailySeries([{ day: '2026-10-02', count: 4 }, { day: '2026-01-01', count: 99 }], 2, new Date('2026-10-03T12:00:00Z')),
 [{ day: '2026-10-02', count: 4 }, { day: '2026-10-03', count: 0 }]);
const analyticsMain = new Element('main');
analytics.mount(analyticsMain, ['blog'], ctx);
await tick();
assert.ok(analyticsMain.textContent.includes('Traffic over time'));
byText(analyticsMain, '7d')[0].dispatch('click');
await tick();
assert.ok(calls.some((c) => c.key.endsWith('/stats?days=7')));
const database = await import('./database.js');
routes['GET /console/api/flats/blog/snapshots'] = { snapshots: ['before-v2-20261002.sqlite'] };
const dbMain = new Element('main');
database.mount(dbMain, ['blog'], ctx);
await tick();
assert.ok(dbMain.textContent.includes('before-v2-20261002.sqlite'));
for (const label of ['Access', 'Analytics', 'Settings']) assert.equal(byText(rows[0], label).length, 1);

const { statusLine, publishDraft, changeVisibility, saveProvider, CHECK_COPY } = await import('./lifecycle.js');
assert.equal(statusLine(blog).startsWith('Published · v2 · Public'), true, statusLine(blog));
assert.equal(statusLine({ ...notes, connection: 'error', publication: 'published', live_version: 2 }).includes('Published · v2'), true);
assert.equal(statusLine({ ...notes, connection: 'error', publication: 'published', live_version: 2 }).includes('Unpublished'), false);
assert.equal(statusLine(notes).includes('Unpublished'), true);
assert.ok(statusLine(blog).includes('Tailscale') === false || statusLine(blog).includes('Portal'));
assert.equal(CHECK_COPY.includes('copy of this flat'), true);

const lifeMain = new Element('main');
const stopLife = flat.mount(lifeMain, ['blog'], ctx);
await tick();
assert.ok(lifeMain.textContent.includes('Current version'));
assert.ok(lifeMain.textContent.includes('Draft'));
assert.ok(lifeMain.textContent.includes('revision 9'));
assert.equal(lifeMain.textContent.toLowerCase().includes('only me'), false);
assert.equal(lifeMain.textContent.includes('Tailscale Serve'), false);
const publishBtn = byText(lifeMain, 'Publish the next version after v2')[0];
assert.ok(publishBtn, lifeMain.textContent);
publishBtn.dispatch('click');
await tick();
let pubDlg = all(document.body, (e) => e.tagName === 'DIALOG').at(-1);
assert.ok(pubDlg.textContent.includes('revision 9'), pubDlg.textContent);
assert.ok(pubDlg.textContent.includes('abc123draft'.slice(0, 12)) || pubDlg.textContent.includes('abc123d'));
assert.ok(pubDlg.textContent.includes('Access stays Public'));
assert.ok(pubDlg.textContent.includes(CHECK_COPY));
pubDlg.close('cancel');
await tick();
assert.equal(calls.some((c) => c.key === 'POST /console/api/flats/blog/publish'), false);

publishBtn.dispatch('click');
await tick();
pubDlg = all(document.body, (e) => e.tagName === 'DIALOG').at(-1);
routes['POST /console/api/flats/blog/publish'] = { status: 'pending_approval', approval: { id: 'ap1', status: 'pending' } };
routes['POST /console/api/approvals/ap1/approve'] = { id: 'ap1', status: 'approved', result: 'published v3', data_impact: 'none' };
pubDlg.close('ok');
await tick();
const pubCall = calls.find((c) => c.key === 'POST /console/api/flats/blog/publish');
assert.deepEqual(JSON.parse(pubCall.body), { revision: 9, hash: 'abc123draft' });
assert.ok(calls.some((c) => c.key === 'POST /console/api/approvals/ap1/approve'));
all(document.body, (e) => e.tagName === 'DIALOG').at(-1)?.close('ok');
await tick();
stopLife();

const accessMain = new Element('main');
const stopAccess = flat.mount(accessMain, ['blog'], ctx);
await tick();
byText(accessMain, 'Access')[0].dispatch('click');
await tick();
const vis = all(accessMain, (e) => e.tagName === 'SELECT')[0];
assert.ok(vis, accessMain.textContent);
vis.value = 'private';
vis.dispatch('change');
await tick();
assert.equal(calls.some((c) => c.key === 'POST /console/api/flats/blog/visibility'), false);
const accessForm = all(accessMain, (e) => e.tagName === 'FORM' && e.textContent.includes('Apply access'))[0];
accessForm.dispatch('submit');
await tick();
const visDlg = all(document.body, (e) => e.tagName === 'DIALOG').at(-1);
assert.ok(visDlg.textContent.includes('Make Blog private?'), visDlg.textContent);
assert.ok(visDlg.textContent.includes('tailnet ACL'));
visDlg.close('cancel');
await tick();
assert.equal(calls.filter((c) => c.key === 'POST /console/api/flats/blog/visibility').length, 0);

vis.value = 'private';
accessForm.dispatch('submit');
await tick();
routes['POST /console/api/flats/blog/visibility'] = { status: 'pending_approval', approval: { id: 'ap2', status: 'pending' } };
routes['POST /console/api/approvals/ap2/approve'] = { id: 'ap2', status: 'failed', result: 'public route unconfirmed' };
routes['GET /console/api/flats/blog'] = { ...blog, visibility: 'public' };
all(document.body, (e) => e.tagName === 'DIALOG').at(-1).close('ok');
await tick();
all(document.body, (e) => e.tagName === 'DIALOG').at(-1)?.close('ok');
await tick();
assert.equal(accessMain.textContent.includes('Access is Private'), false);
stopAccess();

const draftMain = new Element('main');
routes['GET /console/api/flats/blog'] = blog;
const stopDraft = flat.mount(draftMain, ['blog'], ctx);
await tick();
const fileInput = all(draftMain, (e) => e.tagName === 'INPUT' && e.getAttribute('type') === 'file')[0];
assert.ok(fileInput, 'draft upload control');
const file = new File(['flat'], 'site.zip', { type: 'application/zip' });
fileInput.files = [file];
routes['POST /console/api/flats/blog/draft?expected_revision=9'] = { draft: { revision: 10, hash: 'fff', dirty: false }, version: { revision: 10, number: 0, role: 'draft', published: false } };
fileInput.dispatch('change');
await tick();
const draftCall = calls.find((c) => c.key.startsWith('POST /console/api/flats/blog/draft'));
assert.ok(draftCall, calls.map((c) => c.key).join('\n'));
assert.equal(draftCall.key.includes('expected_revision=9'), true);
assert.equal(draftCall.body, file, 'draft autosave sends the source archive');
routes['POST /console/api/flats/blog/draft?expected_revision=10'] = { draft: { revision: 11, hash: 'ggg', dirty: true }, version: { revision: 11, number: 0, role: 'draft', published: false } };
const nextFile = new File(['updated flat'], 'site.zip', { type: 'application/zip' });
fileInput.files = [nextFile];
fileInput.dispatch('change');
await tick();
const nextDraftCall = calls.find((c) => c.key === 'POST /console/api/flats/blog/draft?expected_revision=10');
assert.ok(nextDraftCall, 'second autosave uses the server-returned revision');
assert.equal(nextDraftCall.body, nextFile);
assert.ok(draftMain.textContent.includes('Saved draft revision 11'));
assert.equal(calls.filter((c) => c.key === 'POST /console/api/flats/blog/publish').length, 1);
stopDraft();

routes['GET /console/api/flats/notes'] = notes;
routes['GET /console/api/flats/notes/versions'] = { versions: [] };
routes['GET /console/api/flats/notes/previews'] = { previews: [] };
routes['GET /console/api/approvals'] = { approvals: [] };
const bare = new Element('main');
const stopBare = flat.mount(bare, ['notes'], ctx);
await tick();
byText(bare, 'Access')[0].dispatch('click');
await tick();
const publicOpt = all(bare, (e) => e.tagName === 'OPTION' && e.getAttribute('value') === 'public')[0];
assert.equal(publicOpt.getAttribute('disabled'), '');
assert.ok(bare.textContent.includes('Publish v1 before making this flat Public'));
assert.ok(bare.textContent.includes('Connecting'));
assert.equal(bare.textContent.toLowerCase().includes('reachable'), false);
stopBare();

const permitMain = new Element('main');
const stopPermit = flat.mount(permitMain, ['blog'], ctx);
await tick();
byText(permitMain, 'Access')[0].dispatch('click');
await tick();
const funnel = all(permitMain, (e) => e.getAttribute('id') === 'permit-tailscale-funnel')[0];
const intent = all(permitMain, (e) => e.getAttribute('id') === 'intent-tailscale-funnel')[0];
const saveFunnel = byText(permitMain, 'Save Tailscale Funnel')[0];
funnel.checked = true;
funnel.dispatch('change');
assert.equal(saveFunnel.disabled, true);
intent.checked = true;
intent.dispatch('change');
assert.equal(saveFunnel.disabled, false);
routes['POST /console/api/flats/blog/providers'] = { provider: 'tailscale-funnel', permitted: true };
const beforePublish = calls.filter((c) => c.key === 'POST /console/api/flats/blog/publish').length;
saveFunnel.dispatch('click');
await tick();
const permitDlg = all(document.body, (e) => e.tagName === 'DIALOG').at(-1);
assert.ok(permitDlg.textContent.includes('does not publish'));
assert.ok(permitDlg.textContent.includes('Funnel'));
permitDlg.close('ok');
await tick();
const permitCall = calls.find((c) => c.key === 'POST /console/api/flats/blog/providers');
assert.deepEqual(JSON.parse(permitCall.body), { provider: 'tailscale-funnel', permitted: true });
assert.equal(calls.filter((c) => c.key === 'POST /console/api/flats/blog/publish').length, beforePublish);
all(document.body, (e) => e.tagName === 'DIALOG').forEach((d) => { try { d.close('ok'); } catch { /* already closed */ } });
stopPermit();

routes['POST /console/api/flats/blog/draft?expected_revision=9'] = { __status: 409, error: 'draft revision conflict' };
const conflictMain = new Element('main');
const stopConflict = flat.mount(conflictMain, ['blog'], ctx);
await tick();
const conflictInput = all(conflictMain, (e) => e.getAttribute('type') === 'file')[0];
conflictInput.files = [file];
conflictInput.dispatch('change');
await tick();
assert.ok(conflictMain.textContent.includes('The draft changed somewhere else') || document.body.textContent.includes('The draft changed somewhere else'));
stopConflict();

const { mount: approvalMount } = await import('./approval.js');
routes['GET /console/api/approvals/stale1'] = { id: 'stale1', flat: 'blog', action: 'publish', status: 'failed', via: 'mcp', params: { revision: 9, hash: 'abc123draft' }, result: 'stale candidate', requested_at: '2026-10-03T00:00:00Z' };
const approvalMain = new Element('main');
const stopApproval = approvalMount(approvalMain, ['stale1'], ctx);
await tick();
assert.ok(approvalMain.textContent.includes('The content or access settings changed. Review again.'));
assert.equal(byText(approvalMain, 'Approve…').length, 0);
if (stopApproval) stopApproval();

assert.equal(typeof publishDraft, 'function');
assert.equal(typeof changeVisibility, 'function');
assert.equal(typeof saveProvider, 'function');
console.log('ok');
