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
  querySelector: (selector) => document.body.querySelector(selector),
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
  private_state: 'ready', connection_state: 'ready',
  providers: ['local', 'portal'],
  draft: { revision: 9, hash: 'abc123draft', base_version: 2, dirty: true, updated_at: '2026-10-03T00:00:00Z', role: 'draft', number: 0 },
  live: { number: 2, kind: 'server' }, created_at: '2026-10-01T00:00:00Z', updated_at: '2026-10-02T00:00:00Z', disk_bytes: 10,
};
// An older server may omit the notice; the console must still show one.
const shop = { ...blog, slug: 'shop', name: 'Shop', visibility: 'public-listed', public_url: 'https://shop.portal.example', public_notice: undefined, providers: ['local', 'tailscale-funnel'] };
const notes = { ...blog, slug: 'notes', name: 'Notes', visibility: 'private', publication: 'unpublished', live_version: 0, public_url: undefined, public_notice: undefined, providers: ['local'], draft: null, connection_state: 'starting' };
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
  calls.push({ key, body: opts.body, headers: opts.headers });
  let body = routes[key] ?? (key.includes('/logs') ? { events: [] } : key.includes('/stats') ? { page_views: [] } : {});
  if (typeof body === 'function') body = await body(); // a deferred response
  const status = body && body.__status ? body.__status : 200;
  return { ok: status < 400, status, json: async () => body };
};

const { publicNoticeOf, PUBLIC_URL_NOTICE, LISTED_NOTICE, pendingNoticeFor } = await import('./dom.js');
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
assert.equal(all(dbMain, (e) => e.tagName === 'A' && e.textContent === 'Manage versions and rollback')[0].getAttribute('href'), '/flats/blog#history');
for (const label of ['Access', 'Analytics', 'Settings']) assert.equal(byText(rows[0], label).length, 1);

const { connectionState, connectionDetail, statusLine, publishDraft, changeVisibility, saveProvider, CHECK_COPY, currentTarget, failureMessage, visibilitySettled, draftEditor, activateVersion, endpointAudienceLabel, endpointConnectionLabel, endpointStatusLine, providerRemovalCopy } = await import('./lifecycle.js');
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
assert.equal(all(lifeMain, (e) => e.getAttribute('id') === 'draft')[0].getAttribute('tabindex'), '-1');
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
routes['POST /console/api/approvals/ap2/approve'] = { __status: 409, category: 'public_stop_unconfirmed', error: 'public route unconfirmed', approval: { id: 'ap2', status: 'failed', result: 'public route unconfirmed', result_data: { status: 'failed', failure_code: 'public_stop_unconfirmed', data_impact: 'none', health_data: 'not_run', live_data: 'untouched' } } };
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

const { mount: approvalMount, describeApproval, RESTORE_COPY, RUNTIME_COPY } = await import('./approval.js');
routes['GET /console/api/approvals/stale1'] = { id: 'stale1', flat: 'blog', action: 'publish', status: 'failed', via: 'mcp', params: { revision: 9, hash: 'abc123draft' }, result: 'stale candidate', requested_at: '2026-10-03T00:00:00Z' };
const approvalMain = new Element('main');
const stopApproval = approvalMount(approvalMain, ['stale1'], ctx);
await tick();
assert.ok(approvalMain.textContent.includes('The content or access settings changed. Review again.'));
assert.equal(byText(approvalMain, 'Approve…').length, 0);
if (stopApproval) stopApproval();

// Actual endpoint readiness gates Current links even when a URL/state exists.
const currentEndpoint = { provider: 'portal', audience: 'current', host: 'blog', url: 'https://example.invalid', state: 'starting', ready: false, configured: true, permitted: true };
assert.equal(currentTarget({ ...blog, visibility: 'public', endpoints: [currentEndpoint] }).ready, false);
assert.equal(currentTarget({ ...blog, visibility: 'public', endpoints: [{ ...currentEndpoint, state: 'ready' }] }).ready, false);
assert.equal(currentTarget({ ...blog, visibility: 'public', endpoints: [{ ...currentEndpoint, state: 'ready', ready: true }] }).ready, true);
assert.equal(currentTarget({ ...blog, visibility: 'public', endpoints: [{ ...currentEndpoint, state: 'ready', ready: true, permitted: false }] }).ready, false);

// Every decision 409 uses the server's DecisionError envelope, not a synthetic 200.
for (const [code, expected] of [
  ['provider_not_permitted', 'not permitted'], ['provider_not_ready', 'still connecting'],
  ['provider_unavailable', 'unavailable'], ['runtime_unavailable', 'Server flats are disabled'],
  ['unavailable', 'feature is unavailable'], ['provider_in_use', 'registered routes'], ['not_deployed', 'No current runtime'], ['public_stop_unconfirmed', 'not shown as Private'],
  ['unchanged_content', 'matches the current'], ['stale_approval', 'Review again'],
]) {
  const response = { __status: 409, error: 'decision failed', category: code,
    approval: { id: 'typed-' + code, status: 'failed', result: 'decision failed', result_data: {
      status: 'failed', failure_code: code, data_impact: 'none', health_data: 'not_run', live_data: 'untouched',
    } } };
  assert.ok(failureMessage(response).includes(expected));
  assert.ok(failureMessage({ approval: { result_data: JSON.stringify(response.approval.result_data) } }).includes(expected));
  routes['POST /console/api/flats/blog/visibility'] = { status: 'pending_approval', approval: { id: 'typed' } };
  routes['POST /console/api/approvals/typed/approve'] = response;
  const pending = changeVisibility(blog, 'private');
  await tick();
  all(document.body, (e) => e.tagName === 'DIALOG').at(-1).close('ok');
  await tick();
  const resultDialog = all(document.body, (e) => e.tagName === 'DIALOG').at(-1);
  assert.ok(resultDialog.textContent.includes(expected), code + ': ' + resultDialog.textContent);
  if (code !== 'stale_approval') assert.equal(resultDialog.textContent.includes('content or access settings changed'), false);
  resultDialog.close('ok');
  const outcome = await pending;
  assert.equal(outcome.stale, code === 'stale_approval');
  assert.ok(visibilitySettled(blog, 'private', outcome).text.includes(expected));
}

// The approval decision surface exposes canonical Public and frozen restore identity.
for (const [id, action, params, expected] of [
  ['public', 'set_visibility', { visibility: 'public' }, 'anyone on the internet'],
  ['restore', 'rollback', { version: 1, restore_data: true, snapshot: 'before-v2.sqlite', snapshot_hash: 'snapshot-sha256' }, RESTORE_COPY],
  ['data', 'restore_data', { version: 1, snapshot: 'before-v2.sqlite', snapshot_hash: 'snapshot-sha256' }, RESTORE_COPY],
  ['activate', 'activate', { version: 2 }, RUNTIME_COPY],
  ['publish', 'publish', { revision: 9, hash: blog.draft.hash }, RUNTIME_COPY],
]) {
  routes['GET /console/api/approvals/' + id] = { id, flat: 'blog', action, params, status: 'pending', via: 'mcp' };
  const main = new Element('main');
  approvalMount(main, [id], ctx);
  await tick();
  assert.ok(main.textContent.includes(expected), id + ': ' + main.textContent);
  if (id === 'public') {
    const badge = all(main, (e) => e.className === 'vis vis-public')[0];
    assert.equal(badge.textContent, 'Public');
    const globe = (await import('./dom.js')).visibilityBadge('public');
    assert.equal(badge.childNodes[0].childNodes[0].getAttribute('d'), globe.childNodes[0].childNodes[0].getAttribute('d'));
    assert.ok(main.textContent.includes(pendingNoticeFor('public')));
    assert.equal(main.textContent.includes('This flat is public:'), false);
  }
  if (params.snapshot) assert.ok(main.textContent.includes('snapshot-sha256') && main.textContent.includes('before-v2.sqlite'));
  byText(main, 'Approve…')[0].dispatch('click');
  await tick();
  const dlg = all(document.body, (e) => e.tagName === 'DIALOG').at(-1);
  assert.ok(dlg.textContent.includes(expected));
  if (id === 'public') {
    assert.ok(dlg.textContent.includes(pendingNoticeFor('public')));
    assert.equal(dlg.textContent.includes('This flat is public:'), false);
  }
  if (['public', 'restore', 'data'].includes(id)) assert.equal(byText(dlg, 'Approve')[0].className, 'btn btn-danger');
  dlg.close('cancel');
  await tick();
  assert.equal(calls.some((c) => c.key === 'POST /console/api/approvals/' + id + '/approve'), false);
}
assert.equal(describeApproval({ flat: 'blog', action: 'activate', params: { version: 1 } }), 'Make v1 current on blog');

// Rejection never attempts approval and renders the persisted rejection.
routes['GET /console/api/approvals/reject'] = { id: 'reject', flat: 'blog', action: 'publish', params: { revision: 9 }, status: 'pending', via: 'mcp' };
const rejectMain = new Element('main');
approvalMount(rejectMain, ['reject'], ctx);
await tick();
byText(rejectMain, 'Reject…')[0].dispatch('click');
await tick();
routes['POST /console/api/approvals/reject/reject'] = { id: 'reject', status: 'rejected' };
routes['GET /console/api/approvals/reject'] = { ...routes['GET /console/api/approvals/reject'], status: 'rejected', decided_by: 'local operator' };
all(document.body, (e) => e.tagName === 'DIALOG').at(-1).close('ok');
await tick();
assert.ok(rejectMain.textContent.includes('request was rejected'));
assert.equal(byText(rejectMain, 'Approve…').length, 0);
assert.equal(calls.some((c) => c.key === 'POST /console/api/approvals/reject/approve'), false);

// Provider-missing requests remain pending and disclose permission policy before decision.
routes['GET /console/api/flats/blog'] = { ...blog, providers: ['local'], endpoints: [], connection_state: 'unavailable' };
const rawPolicyHash = 'raw-policy-sha256-must-stay-hidden';
routes['GET /console/api/approvals/missing'] = { id: 'missing', flat: 'blog', action: 'set_visibility', status: 'pending', via: 'mcp', params: { visibility: 'public', providers: `local,portal\n${rawPolicyHash}` } };
const missingMain = new Element('main');
approvalMount(missingMain, ['missing'], ctx);
await tick();
assert.ok(missingMain.textContent.includes('Public') && missingMain.textContent.includes('pending'));
assert.equal(byText(missingMain, 'Approve…').length, 1);

assert.ok(missingMain.textContent.includes('If approved, this flat becomes Public:'));
assert.equal(missingMain.textContent.includes('This flat is public:'), false);
assert.ok(missingMain.textContent.includes('Provider permissions at requestlocal, portal'));
assert.equal(missingMain.textContent.includes(rawPolicyHash), false);
assert.equal(endpointAudienceLabel({ audience: 'draft', host: 'preview-v1', state: 'ready' }, [{ host: 'preview-v1', target: 'version', version: 1 }]), 'v1 preview (Private)');
assert.equal(endpointAudienceLabel({ audience: 'draft', host: 'preview-draft', state: 'ready' }, [{ host: 'preview-draft', target: 'draft', version: 0 }]), 'Draft preview (Private)');
const stoppedFunnel = { provider: 'tailscale-funnel', audience: 'current', host: 'blog', state: 'stopped', detail: 'route stopped', configured: true, permitted: true, ready: false };
const refusedPortal = { provider: 'portal', audience: 'current', host: 'blog', state: 'unavailable', detail: 'portal is permitted but not configured', configured: false, permitted: true, ready: false };
const startingPortal = { ...refusedPortal, state: 'starting', detail: 'waiting for relay', configured: true };
assert.equal(endpointAudienceLabel({ audience: 'draft', host: 'closed', state: 'stopped', detail: 'route stopped' }, []), 'Stopped preview route');
assert.equal(endpointAudienceLabel(stoppedFunnel, []), 'Stopped current route');
assert.equal(endpointConnectionLabel(stoppedFunnel), 'Stopped');
assert.equal(endpointConnectionLabel(refusedPortal), 'Not configured');
assert.equal(endpointConnectionLabel({ ...refusedPortal, configured: true, permitted: false, detail: 'host permission absent' }), 'Not permitted');
assert.equal(endpointStatusLine(refusedPortal), 'Not configured · Route permitted');
assert.equal(endpointStatusLine(stoppedFunnel), 'Configured · Route permitted · Stopped');
assert.equal(endpointConnectionLabel({ state: 'unavailable', detail: 'route stopped' }), 'Stopped'); // legacy DTO
assert.equal(connectionState({ ...blog, connection_state: 'stopped', endpoints: [stoppedFunnel, refusedPortal] }), 'unavailable');
assert.equal(connectionDetail({ ...blog, connection_state: 'stopped', endpoints: [stoppedFunnel, refusedPortal] }), 'portal is permitted but not configured');
assert.equal(connectionState({ ...blog, connection_state: 'stopped', endpoints: [stoppedFunnel, startingPortal] }), 'starting');
assert.equal(connectionState({ ...blog, connection_state: 'stopped', endpoints: [stoppedFunnel] }), 'stopped');
assert.equal(currentTarget({ ...blog, connection_state: 'stopped', endpoints: [stoppedFunnel, refusedPortal] }).state, 'unavailable');
assert.ok(providerRemovalCopy({ visibility: 'public' }, 'tailscale').includes('Private Tailscale') && providerRemovalCopy({ visibility: 'public' }, 'tailscale').includes('Funnel route stays up'));
assert.ok(providerRemovalCopy({ visibility: 'private' }, 'portal').includes('removes permission'));
assert.equal(providerRemovalCopy({ visibility: 'private' }, 'portal').includes('changing this flat to Private'), false);
for (const state of ['starting', 'error']) {
  assert.equal(connectionDetail({ slug: 'blog', visibility: 'public', connection_state: state,
    endpoints: [
      { audience: 'draft', host: 'blog', provider: 'portal', state, detail: 'wrong draft' },
      { audience: 'current', host: 'old', provider: 'portal', state, detail: 'wrong alias' },
      { audience: 'current', host: 'blog', provider: 'tailscale', state, detail: 'wrong private' },
      { audience: 'current', host: 'blog', provider: 'portal', url: '', state, detail: 'Public connection detail' },
    ] }), 'Public connection detail');
}

// Clean Drafts have no Publish action. First saves guard revision zero.
routes['GET /console/api/flats/blog'] = { ...blog, draft: { ...blog.draft, dirty: false } };
const cleanMain = new Element('main');
const stopClean = flat.mount(cleanMain, ['blog'], ctx);
await tick();
assert.equal(byText(cleanMain, 'Publish the next version after v2').length, 0);
stopClean();
const firstEditor = draftEditor(notes, () => {});
const firstInput = all(firstEditor.editor, (e) => e.getAttribute('type') === 'file')[0];
firstInput.files = [file];
routes['POST /console/api/flats/notes/draft?expected_revision=0'] = { draft: { revision: 1 } };
firstInput.dispatch('change');
await tick();
assert.ok(calls.some((c) => c.key === 'POST /console/api/flats/notes/draft?expected_revision=0'));

// Retry refreshes revision from the server instead of reusing the conflicting one.
const retryEditor = draftEditor(blog, () => {});
const retryInput = all(retryEditor.editor, (e) => e.getAttribute('type') === 'file')[0];
retryInput.files = [file];
routes['POST /console/api/flats/blog/draft?expected_revision=9'] = { __status: 409, category: 'conflict', error: 'draft revision conflict' };
retryInput.dispatch('change');
await tick();
routes['GET /console/api/flats/blog'] = { ...blog, draft: { ...blog.draft, revision: 12 } };
routes['POST /console/api/flats/blog/draft?expected_revision=12'] = { draft: { revision: 13 } };
byText(retryEditor.editor, 'Retry draft save')[0].dispatch('click');
await tick();
assert.ok(calls.some((c) => c.key === 'POST /console/api/flats/blog/draft?expected_revision=12'));

// Console rollback can explicitly request the existing API's restore_data path,
// then requires a danger confirmation for the frozen snapshot.
const rollback = activateVersion(blog, 1, { rollback: true });
await tick();
let restoreDlg = all(document.body, (e) => e.tagName === 'DIALOG').at(-1);
all(restoreDlg, (e) => e.getAttribute('id') === 'rollback-restore-data')[0].checked = true;
routes['POST /console/api/flats/blog/rollback'] = { status: 'pending_approval', approval: { id: 'rollback-restore', params: { version: 1, restore_data: true, snapshot: 'before-v2.sqlite', snapshot_hash: 'frozen-hash' } } };
restoreDlg.close('ok');
await tick();
assert.deepEqual(JSON.parse(calls.find((c) => c.key === 'POST /console/api/flats/blog/rollback').body), { version: 1, restore_data: true });
restoreDlg = all(document.body, (e) => e.tagName === 'DIALOG').at(-1);
assert.ok(restoreDlg.textContent.includes('frozen-hash'));
assert.equal(byText(restoreDlg, 'Approve and restore live data')[0].className, 'btn btn-danger');
restoreDlg.close('cancel');
assert.equal((await rollback).pending, true);
assert.equal(calls.some((c) => c.key === 'POST /console/api/approvals/rollback-restore/approve'), false);

assert.equal(typeof publishDraft, 'function');
assert.equal(typeof changeVisibility, 'function');
assert.equal(typeof saveProvider, 'function');
// A failed session read must not recursively trigger its own resynchronization.
const { api } = await import('./api.js');
let statusEvents = 0;
globalThis.CustomEvent = class { constructor(type) { this.type = type; } };
document.dispatchEvent = () => { statusEvents++; };
routes['GET /console/api/operator/session'] = { __status: 403, category: 'operator_secure_transport_required', error: 'HTTPS required' };
await assert.rejects(api.operatorStatus());
assert.equal(statusEvents, 0);
routes['POST /console/api/flats/blog/publish'] = { __status: 403, category: 'operator_required', error: 'Unlock required' };
await assert.rejects(api.publish('blog', {}));
assert.equal(statusEvents, 1);

// --- settings page ---
// The console never asks through window.confirm.
globalThis.confirm = () => { throw new Error('window.confirm used'); };
globalThis.location = { origin: 'http://127.0.0.1:7878' };
const settingsPage = await import('./settings.js');
const byId = (root, id) => all(root, (e) => e.getAttribute('id') === id)[0];
const SETTINGS = { upload_max_bytes: '20971520', keep_versions: '10', disk_quota_bytes: '32212254720', preview_ttl_seconds: '86400',
  rate_limit_rps: '50', redirect_days: '7', events_keep: '5000', portal_relays: 'https://relay.example.com', portal_discovery: 'true', portal_max_relays: '3' };
const CONFIG = {
  mode: 'config', schema_version: 1, etag: 'etag-1', changed_on_disk: false,
  host: { management_addr: '127.0.0.1:7878', local_addr: '127.0.0.1:7879', console_host: 'flats', server_runtime: true },
  network: { permitted: ['portal'], private_backend: 'local' },
  credentials: { operator_file: true, tailscale_authkey_file: false },
  keys: { upload_max_bytes: 'system.upload_max_bytes', keep_versions: 'system.keep_versions', disk_quota_bytes: 'system.disk_quota_bytes',
    preview_ttl_seconds: 'system.preview_ttl_seconds', rate_limit_rps: 'system.rate_limit_rps', redirect_days: 'system.redirect_days',
    events_keep: 'system.events_keep', portal_relays: 'portal.relays', portal_discovery: 'portal.discovery', portal_max_relays: 'portal.max_active_relays' },
  sources: { 'host.management_addr': 'flag', 'host.local_addr': 'default', 'host.console_host': 'default', 'host.server_runtime': 'default',
    'network.permitted': 'file', 'network.private_backend': 'default', 'credentials.operator_file': 'file', 'credentials.tailscale_authkey_file': 'default',
    'system.keep_versions': 'file', 'system.events_keep': 'default', 'portal.relays': 'file' },
  pinned: { portal_relays: "the service's --relays flag" },
};
const settingsRoute = (config) => ({ settings: SETTINGS, defaults: SETTINGS, apply_on_restart: ['portal_relays'], config });
routes['GET /console/api/settings'] = settingsRoute(CONFIG);
const mountSettings = async () => {
  const main = new Element('main');
  settingsPage.mount(main, [], ctx);
  await tick();
  return main;
};
const puts = () => calls.filter((c) => c.key === 'PUT /console/api/settings');

// Host, network and credentials are read only, with their sources.
let page = await mountSettings();
const configCard = byId(page, 'configuration');
for (const text of ['Management address', '127.0.0.1:7878', 'flag for this run', 'Network', 'portal', 'Credentials',
  'Operator credential file', 'not configured', 'flats config set', 'apply after Flats restarts']) {
  assert.ok(configCard.textContent.includes(text), `configuration lacks ${text}: ${configCard.textContent}`);
}
assert.equal(all(configCard, (e) => ['INPUT', 'TEXTAREA', 'SELECT', 'BUTTON'].includes(e.tagName)).length, 0);
assert.equal(page.textContent.includes('config.json changed on disk'), false);
assert.equal(configCard.textContent.includes('flats install'), false);
// The system and Portal settings stay editable and show their source; the
// pinned relays cannot be changed and say why.
const legends = all(page, (e) => e.tagName === 'LEGEND').map((e) => e.textContent);
assert.deepEqual(legends, ['System', 'Portal']);
assert.ok(byId(page, 'set-keep_versions-help').textContent.includes('Source: config.json.'));
assert.ok(byId(page, 'set-events_keep-help').textContent.includes('Source: default.'));
const relaysInput = byId(page, 'set-portal_relays');
assert.equal(relaysInput.getAttribute('disabled'), '');
assert.ok(relaysInput.getAttribute('aria-describedby').split(' ').includes('set-portal_relays-pin'));
assert.ok(byId(page, 'set-portal_relays-pin').textContent.includes("Set by the service's --relays flag"));
assert.equal(byId(page, 'set-keep_versions').getAttribute('disabled'), null);

// A file changed on disk and legacy mode are both explained.
routes['GET /console/api/settings'] = settingsRoute({ ...CONFIG, mode: 'legacy', changed_on_disk: true });
page = await mountSettings();
assert.ok(page.textContent.includes('config.json changed on disk'));
assert.ok(page.textContent.includes('Restart Flats to load it'));
assert.ok(byId(page, 'configuration').textContent.includes('flats install'));

// A refused save keeps the operator's input and says why; the page then
// learns that the file changed.
routes['GET /console/api/settings'] = settingsRoute(CONFIG);
page = await mountSettings();
let form = all(page, (e) => e.tagName === 'FORM')[0];
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

// Lowering a retention setting first shows what the next pruning removes
// and saves only after a second, explicit confirmation.
routes['GET /console/api/settings'] = settingsRoute(CONFIG);
page = await mountSettings();
form = all(page, (e) => e.tagName === 'FORM')[0];
routes['POST /console/api/settings/impact'] = { impact: { keep_versions: { current: '10', candidate: '2', total: 3, flats: [{ slug: 'blog', count: 3, versions: [3, 2, 1] }] } } };
routes['PUT /console/api/settings'] = { settings: SETTINGS, applied: ['keep_versions'], restart_required: ['portal_max_relays'], etag: 'etag-2' };
byId(page, 'set-keep_versions').value = '2';
byId(page, 'set-portal_max_relays').value = '5';
let putCount = puts().length;
form.dispatch('submit');
await tick();
const impactCall = calls.filter((c) => c.key === 'POST /console/api/settings/impact').at(-1);
assert.deepEqual(JSON.parse(impactCall.body), { keep_versions: '2', portal_max_relays: '5' });
assert.equal(puts().length, putCount, 'a decrease must not save before the confirmation');
let panel = all(page, (e) => e.className === 'impact')[0];
assert.ok(panel.textContent.includes('Saving removes data'));
assert.ok(panel.textContent.includes('Versions to keep: 10 → 2 versions.'));
assert.ok(panel.textContent.includes('blog: 3 versions (3, 2, 1)'));
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
put = puts().at(-1);
assert.equal(puts().length, putCount + 1);
assert.equal(put.headers['If-Match'], '"etag-1"');
assert.deepEqual(JSON.parse(put.body), { keep_versions: '2', portal_max_relays: '5' });
const limits = byId(page, 'limits');
assert.ok(limits.textContent.includes('Versions to keep: saved, applied now.'), limits.textContent);
assert.ok(limits.textContent.includes('Active Portal relays: saved, applies after Flats restarts.'));

// A decrease that removes nothing still asks once, with a plain Save.
routes['POST /console/api/settings/impact'] = { impact: { events_keep: { current: '5000', candidate: '100', total: 0, flats: [] } } };
page = await mountSettings();
form = all(page, (e) => e.tagName === 'FORM')[0];
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
const IMPACT_2 = { impact: { keep_versions: { current: '10', candidate: '2', total: 3, flats: [{ slug: 'blog', count: 3, versions: [3, 2, 1] }] } } };
let release;
routes['POST /console/api/settings/impact'] = () => new Promise((resolve) => { release = () => resolve(IMPACT_2); });
page = await mountSettings();
form = all(page, (e) => e.tagName === 'FORM')[0];
byId(page, 'set-keep_versions').value = '2';
putCount = puts().length;
form.dispatch('submit');
await tick();
assert.equal(typeof release, 'function', 'the impact request must be pending');
byId(page, 'set-keep_versions').value = '9';
form.dispatch('input');
release();
await tick();
assert.equal(all(page, (e) => e.className === 'impact').length, 0, 'a late impact answer must not show');
assert.equal(puts().length, putCount);
assert.equal(byId(page, 'set-keep_versions').value, '9');

// The confirmation saves only the values it described.
routes['POST /console/api/settings/impact'] = IMPACT_2;
page = await mountSettings();
form = all(page, (e) => e.tagName === 'FORM')[0];
byId(page, 'set-keep_versions').value = '2';
form.dispatch('submit');
await tick();
panel = all(page, (e) => e.className === 'impact')[0];
byId(page, 'set-keep_versions').value = '9'; // changed without an input event
byText(panel, 'Save and remove')[0].dispatch('click');
await tick();
assert.equal(puts().length, putCount);
assert.equal(all(page, (e) => e.className === 'impact').length, 0);
assert.ok(page.textContent.includes('The form changed after the preview.'));
assert.equal(byId(page, 'set-keep_versions').value, '9');

// Raising a limit saves at once.
page = await mountSettings();
form = all(page, (e) => e.tagName === 'FORM')[0];
byId(page, 'set-keep_versions').value = '20';
const impactCount = calls.filter((c) => c.key === 'POST /console/api/settings/impact').length;
putCount = puts().length;
form.dispatch('submit');
await tick();
assert.equal(calls.filter((c) => c.key === 'POST /console/api/settings/impact').length, impactCount);
assert.equal(puts().length, putCount + 1);
assert.equal(settingsPage.isDecrease('keep_versions', '0', '5'), true);
assert.equal(settingsPage.isDecrease('keep_versions', '5', '0'), false);
console.log('ok');
