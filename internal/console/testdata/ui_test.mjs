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
const PUBLIC = 'This flat is public: anyone on the internet can open it. A domain or URL is not what makes it public.';
const blog = {
  type: 'docs', slug: 'blog', name: 'Blog', visibility: 'public', live_version: 2, versions: 2, providers: ['portal'],
  portal_listing: 'default', portal_hidden: false,
  private_url: 'https://blog.tail.ts.net', public_url: 'https://blog.portal.example', public_notice: PUBLIC,
  draft: { revision: 3, hash: 'c', dirty: true, size: 1, files: 1, kind: 'server', updated_at: '2026-10-03T00:00:00Z' },
  live: { number: 2, kind: 'server' }, created_at: '2026-10-01T00:00:00Z', updated_at: '2026-10-02T00:00:00Z', disk_bytes: 10,
};
// An older server may omit the notice or report a legacy public value, and a
// Funnel address comes from the flat's endpoints; the console still shows the notice.
const shop = { ...blog, type: 'flat', slug: 'shop', name: 'Shop', visibility: 'public-listed', public_url: undefined, public_notice: undefined,
  endpoints: [{ provider: 'tailscale-funnel', audience: 'current', url: 'https://shop.tail.ts.net', state: 'ready' }] };
const notes = { ...blog, slug: 'notes', name: 'Notes', visibility: 'private', public_url: undefined, public_notice: undefined, draft: null };
const pending = (id) => ({ status: 'pending_approval', approval: { id, status: 'pending' } });
const calls = [];
const routes = {
  'GET /console/api/flats': { flats: [blog, shop, notes] },
  'GET /console/api/approvals?status=pending': { approvals: [] },
  'GET /console/api/flats/blog': blog,
  'GET /console/api/flats/blog/network': { origins: ['https://api.example.com'] },
  'PUT /console/api/flats/blog/network': { origins: ['https://api.example.com'] },
  'PUT /console/api/flats/blog/listing': { ...blog, portal_listing: 'hidden', portal_hidden: true },
  'GET /console/api/flats/blog/env': { env: [{ name: 'APP_MODE', value: '<script>demo</script>\nsecond line', updated_at: '2026-10-03T00:00:00Z' }, { name: 'EMPTY', value: '', updated_at: '2026-10-03T00:00:00Z' }] },
  'GET /console/api/flats/blog/secrets': { secrets: [{ name: 'API_KEY', updated_at: '2026-10-03T00:00:00Z', value: 'SECRET_MUST_NEVER_RENDER' }] },
  'GET /console/api/flats/blog/versions': { versions: [
    { number: 2, kind: 'server', hash: 'b', size: 1, files: 1, created_at: '2026-10-02T00:00:00Z' },
    { number: 1, kind: 'server', hash: 'a', size: 1, files: 1, created_at: '2026-10-01T00:00:00Z' }] },
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
assert.equal(rows.length, 3);
// The type shows as an icon left of the name, labelled for hover and screen
// readers; there is no initials tile or text badge.
const typeLabel = (root) => all(root, (e) => e.className.startsWith('type-icon'))
  .map((e) => [e.getAttribute('role'), e.getAttribute('aria-label'), e.getAttribute('data-tip'), e.getAttribute('tabindex')]);
assert.deepEqual(typeLabel(rows[0]), [['img', 'Document', 'Document', '0']]);
assert.deepEqual(typeLabel(rows[1]), [['img', 'Flat', 'Flat', '0']]);
for (const row of rows) {
  assert.equal(all(row, (e) => /\b(thumb|content-type)\b/.test(e.className)).length, 0);
}
for (const row of rows) {
  assert.equal(all(row, (e) => e.className.includes('notice-text')).length, 0);
}
for (const [row, site] of rows.map((row, i) => [row, [blog, shop, notes][i]])) {
 const name = all(row, (e) => e.tagName === 'A' && e.className === 'flat-name')[0];
 assert.equal(name.getAttribute('href'), new URL(site.public_url || publicURL(site) || site.private_url).href);
 assert.equal(name.getAttribute('target'), '_blank');
 assert.equal(name.getAttribute('data-nav'), null);
}
// A flat with draft changes offers Publish; one without does not.
assert.equal(byText(rows[0], 'Publish').length, 1);
assert.equal(byText(rows[2], 'Publish').length, 0);
stopList();

// The flat's own address is its Deployments tab: status, versions, previews
// and logs under the same tabs as Settings, with no settings panels of its own.
// It offers a redeploy of the live version (and only of it), and the deploy
// result shows the notice.
const flat = await import('./flat.js');
const flatMain = new Element('main');
const stopFlat = flat.mount(flatMain, ['blog'], ctx);
await tick();
assert.deepEqual(typeLabel(flatMain), [['img', 'Document', 'Document', '0']]);
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

// The unpublished draft is a row above the published versions and publishes
// the exact revision shown.
const draftRow = all(flatMain, (e) => e.tagName === 'TR' && e.className === 'is-draft')[0];
assert.ok(draftRow, 'want a draft row');
routes['POST /console/api/flats/blog/publish'] = pending('apr-publish');
routes['POST /console/api/approvals/apr-publish/approve'] = { id: 'apr-publish', status: 'approved', result: 'version 3 is live' };
byText(draftRow, 'Publish')[0].dispatch('click');
await tick();
dialogs = all(document.body, (e) => e.tagName === 'DIALOG');
assert.ok(dialogs[0].textContent.includes('Publish Blog?'));
dialogs[0].close('ok');
await tick();
assert.deepEqual(JSON.parse(calls.find((c) => c.key === 'POST /console/api/flats/blog/publish').body), { revision: 3, hash: 'c' });
assert.ok(calls.some((c) => c.key === 'POST /console/api/approvals/apr-publish/approve'));
all(document.body, (e) => e.tagName === 'DIALOG').forEach((d) => d.close('ok'));
await tick();

stopFlat();
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
const funnel = all(management, (e) => e.tagName === 'INPUT' && e.getAttribute('id') === 'net-tailscale-funnel')[0];
funnel.checked = true;
funnel.dispatch('change');
await tick();
assert.deepEqual(JSON.parse(calls.find((c) => c.key === 'POST /console/api/flats/blog/providers').body), { provider: 'tailscale-funnel', permitted: true });
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
assert.ok(listingSelect.parentNode.textContent.includes('Hidden from relay listings now.'));
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
const access = all(share, (e) => e.tagName === 'SELECT')[0];
access.value = 'private';
access.dispatch('change');
await tick();
const permissionConfirm = all(document.body, (e) => e.tagName === 'DIALOG' && e !== share)[0];
assert.ok(permissionConfirm.textContent.includes('Make Blog private?'));
routes['POST /console/api/flats/blog/visibility'] = pending('apr-vis');
routes['POST /console/api/approvals/apr-vis/approve'] = { id: 'apr-vis', status: 'approved', result: 'blog is now private' };
routes['GET /console/api/flats/blog'] = { ...blog, visibility: 'private', public_url: undefined };
permissionConfirm.close('ok');
await tick();
assert.deepEqual(JSON.parse(calls.find((c) => c.key === 'POST /console/api/flats/blog/visibility').body), { visibility: 'private', reason: '' });
assert.ok(share.textContent.includes('devices allowed by your tailnet'));
assert.equal(all(share, (e) => e.tagName === 'A' && e.textContent === 'Visit')[0].getAttribute('href'), new URL(blog.private_url).href);
share.close();
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
// The console never asks through window.confirm.
globalThis.confirm = () => { throw new Error('window.confirm used'); };
globalThis.location = { origin: 'http://127.0.0.1:7878' };
const settingsPage = await import('./settings.js');
const byId = (root, id) => all(root, (e) => e.getAttribute('id') === id)[0];
const SETTINGS = { upload_max_bytes: '20971520', keep_versions: '10', disk_quota_bytes: '32212254720', preview_ttl_seconds: '86400',
  rate_limit_rps: '50', redirect_days: '7', events_keep: '5000', portal_relays: 'https://relay.example.com', portal_discovery: 'true', portal_max_relays: '3', portal_hide: 'false' };
const CONFIG = {
  mode: 'config', schema_version: 1, etag: 'etag-1', changed_on_disk: false,
  host: { management_addr: '127.0.0.1:7878', local_addr: '127.0.0.1:7879', console_host: 'flats', server_runtime: true },
  network: { permitted: ['portal'], private_backend: 'local' },
  credentials: { operator_file: true, tailscale_authkey_file: false },
  keys: { upload_max_bytes: 'system.upload_max_bytes', keep_versions: 'system.keep_versions', disk_quota_bytes: 'system.disk_quota_bytes',
    preview_ttl_seconds: 'system.preview_ttl_seconds', rate_limit_rps: 'system.rate_limit_rps', redirect_days: 'system.redirect_days',
    events_keep: 'system.events_keep', portal_relays: 'portal.relays', portal_discovery: 'portal.discovery', portal_max_relays: 'portal.max_active_relays', portal_hide: 'portal.hide' },
  sources: { 'host.management_addr': 'flag', 'host.local_addr': 'default', 'host.console_host': 'default', 'host.server_runtime': 'default',
    'network.permitted': 'file', 'network.private_backend': 'default', 'credentials.operator_file': 'file', 'credentials.tailscale_authkey_file': 'default',
    'system.keep_versions': 'file', 'system.events_keep': 'default', 'portal.relays': 'file' },
  pinned: { portal_relays: "the service's --relays flag" },
};
const settingsRoute = (config) => ({ settings: SETTINGS, defaults: SETTINGS, apply_on_restart: ['portal_relays'], config });
routes['GET /console/api/settings'] = settingsRoute(CONFIG);
const provider = (id, scope, enabled, flats = []) => ({ id, scope, enabled, configured: enabled, flats });
routes['GET /console/api/providers'] = { providers: [provider('local', 'private', true), provider('tailscale', 'private', false),
  provider('tailscale-funnel', 'public', false), provider('portal', 'public', true, ['blog'])] };
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
for (const text of ['Management address', '127.0.0.1:7878', 'flag for this run', 'Network', 'Credentials',
  'Tailscale auth key file', 'not configured', 'flats config set', 'apply after Flats restarts']) {
  assert.ok(configCard.textContent.includes(text), `configuration lacks ${text}: ${configCard.textContent}`);
}
assert.equal(all(configCard, (e) => ['INPUT', 'TEXTAREA', 'SELECT', 'BUTTON'].includes(e.tagName)).length, 0);
assert.equal(page.textContent.includes('config.json changed on disk'), false);
assert.equal(configCard.textContent.includes('flats install'), false);
// The system and Portal settings stay editable and show their source; the
// pinned relays cannot be changed and say why.
const legends = all(page, (e) => e.tagName === 'LEGEND').map((e) => e.textContent);
assert.deepEqual(legends, ['Portal settings', 'System']);
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
let form = all(byId(page, 'limits'), (e) => e.tagName === 'FORM')[0];
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
form = all(byId(page, 'limits'), (e) => e.tagName === 'FORM')[0];
routes['POST /console/api/settings/impact'] = { impact: { keep_versions: { current: '10', candidate: '2', total: 3, flats: [{ slug: 'blog', count: 3, versions: [3, 2, 1] }] } } };
routes['PUT /console/api/settings'] = { settings: SETTINGS, applied: ['keep_versions'], restart_required: ['portal_max_relays'], etag: 'etag-2' };
byId(page, 'set-keep_versions').value = '2';
let putCount = puts().length;
form.dispatch('submit');
await tick();
const impactCall = calls.filter((c) => c.key === 'POST /console/api/settings/impact').at(-1);
assert.deepEqual(JSON.parse(impactCall.body), { keep_versions: '2' });
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
assert.deepEqual(JSON.parse(put.body), { keep_versions: '2' });
const limits = byId(page, 'limits');
assert.ok(limits.textContent.includes('Versions to keep: saved, applied now.'), limits.textContent);
assert.ok(limits.textContent.includes('Active Portal relays: saved, applies after Flats restarts.'));

// A decrease that removes nothing still asks once, with a plain Save.
routes['POST /console/api/settings/impact'] = { impact: { events_keep: { current: '5000', candidate: '100', total: 0, flats: [] } } };
page = await mountSettings();
form = all(byId(page, 'limits'), (e) => e.tagName === 'FORM')[0];
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
form = all(byId(page, 'limits'), (e) => e.tagName === 'FORM')[0];
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
form = all(byId(page, 'limits'), (e) => e.tagName === 'FORM')[0];
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
form = all(byId(page, 'limits'), (e) => e.tagName === 'FORM')[0];
byId(page, 'set-keep_versions').value = '20';
const impactCount = calls.filter((c) => c.key === 'POST /console/api/settings/impact').length;
putCount = puts().length;
form.dispatch('submit');
await tick();
assert.equal(calls.filter((c) => c.key === 'POST /console/api/settings/impact').length, impactCount);
assert.equal(puts().length, putCount + 1);
assert.equal(settingsPage.isDecrease('keep_versions', '0', '5'), true);
assert.equal(settingsPage.isDecrease('keep_versions', '5', '0'), false);

// The Portal settings save from their own form in the Portal provider panel.
routes['GET /console/api/settings'] = settingsRoute({ ...CONFIG, pinned: {} });
routes['PUT /console/api/settings'] = { settings: SETTINGS, applied: [], restart_required: ['portal_max_relays'], etag: 'etag-3' };
page = await mountSettings();
const portalForm = all(byId(page, 'provider-portal'), (e) => e.tagName === 'FORM')[0];
assert.ok(portalForm, 'the Portal panel holds the Portal settings');
assert.equal(all(byId(page, 'limits'), (e) => e.getAttribute('id') === 'set-portal_relays').length, 0);
byId(page, 'set-portal_max_relays').value = '5';
putCount = puts().length;
portalForm.dispatch('submit');
await tick();
assert.equal(puts().length, putCount + 1);
assert.deepEqual(JSON.parse(puts().at(-1).body), { portal_max_relays: '5' });
page = await mountSettings();
const hideBox = byId(page, 'set-portal_hide');
assert.ok(hideBox && byId(page, 'set-portal_hide-help').textContent.includes('not access control'));
hideBox.checked = true;
putCount = puts().length;
all(byId(page, 'provider-portal'), (e) => e.tagName === 'FORM')[0].dispatch('submit');
await tick();
assert.equal(puts().length, putCount + 1);
assert.deepEqual(JSON.parse(puts().at(-1).body), { portal_hide: 'true' });

// Settings group providers into Private and Public; only Local is always on,
// and turning a provider on is an explicit host request.
routes['GET /console/api/settings'] = settingsRoute({ ...CONFIG, etag: 'etag-9', pinned: {} });
routes['PUT /console/api/providers/tailscale'] = { providers: [] };
const settingsMain = new Element('main');
settingsPage.mount(settingsMain, [], ctx);
await tick();
const groups = all(settingsMain, (e) => e.className === 'provider-group');
assert.deepEqual(groups.map((g) => all(g, (e) => e.tagName === 'H3')[0].textContent), ['Private', 'Public']);
assert.deepEqual(all(groups[0], (e) => e.tagName === 'ARTICLE').map((e) => e.getAttribute('id')), ['provider-local', 'provider-tailscale']);
assert.deepEqual(all(groups[1], (e) => e.tagName === 'ARTICLE').map((e) => e.getAttribute('id')), ['provider-tailscale-funnel', 'provider-portal']);
assert.equal(all(groups[0], (e) => e.tagName === 'BUTTON' && /^Turn (on|off)$/.test(e.textContent)).length, 1, 'Local has no switch');
assert.ok(groups[1].textContent.includes('Portal relays'), 'Portal settings belong to the Portal provider');
byText(groups[0], 'Turn on')[0].dispatch('click');
await tick();
const turnOn = all(document.body, (e) => e.tagName === 'DIALOG').pop();
assert.ok(turnOn.textContent.includes('Turn on Tailscale?'));
turnOn.close('ok');
await tick();
const turnOnCall = calls.find((c) => c.key === 'PUT /console/api/providers/tailscale');
assert.deepEqual(JSON.parse(turnOnCall.body), { enabled: true });
assert.equal(turnOnCall.headers['If-Match'], '"etag-9"', 'a provider change is guarded by the settings ETag');

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
assert.equal(all(remoteShare, (e) => e.tagName === 'A' && e.textContent === 'Visit').length, 0);
remoteShare.close();
shareDialog({ ...blog, visibility: 'private', private_url: 'https://blog.tail1234.ts.net', live_version: 1 });
const tailShare = all(document.body, (e) => e.tagName === 'DIALOG').pop();
assert.equal(all(tailShare, (e) => e.tagName === 'BUTTON' && e.textContent.includes('Copy link'))[0].getAttribute('disabled'), null);
tailShare.close();
globalThis.location = { origin: 'http://127.0.0.1:7878' };
assert.equal(ui.extLink('http://blog.localhost:7879/').tagName, 'A', 'on the host a loopback link works');
globalThis.location = savedLocation;
console.log('ok');
