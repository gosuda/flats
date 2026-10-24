// Delete from the flat list (docs/spec/console-row-delete.md): the "…" menu
// item that turns into a slide control in place, the pending lifecycle and
// every outcome of the request, against a stubbed API. Reuses the fake DOM of
// ui_test.mjs, whose suite runs first. The Go runner copies both scripts.
import './ui_test.mjs';
import assert from 'node:assert/strict';

const all = (root, pred, out = []) => {
  if (root && root.tagName && pred(root)) out.push(root);
  for (const c of root.childNodes || []) all(c, pred, out);
  return out;
};
const hasClass = (e, c) => (e.getAttribute('class') || '').split(/\s+/).includes(c);

// The fake DOM answers only a few selectors; teach it the ones the menu uses
// for its items and keyboard navigation, and give elements a dataset.
const Element = document.createElement('div').constructor;
const baseQSA = Element.prototype.querySelectorAll;
Element.prototype.querySelectorAll = function (selector) {
  if (selector === '.menu-item') return all(this, (e) => hasClass(e, 'menu-item'));
  if (selector.startsWith('.menu-item:not([disabled])')) {
    return all(this, (e) => (hasClass(e, 'menu-item') && !e.disabled && e.getAttribute('aria-disabled') !== 'true')
      || (e.getAttribute('role') === 'slider' && e.getAttribute('aria-disabled') !== 'true'));
  }
  return baseQSA.call(this, selector);
};
Element.prototype.getClientRects = function () { return [{}]; };
// A node counts as connected while it is inside a mounted page or the body.
Object.defineProperty(Element.prototype, 'isConnected', {
  get() { for (let x = this; x; x = x.parentNode) if (x.tagName === 'MAIN' || x.tagName === 'BODY') return true; return false; },
});
Element.prototype.closest = function (selector) {
  const m = selector.match(/^\[role="(\w+)"\]$/);
  for (let x = this; x && x.getAttribute; x = x.parentNode) if (m && x.getAttribute('role') === m[1]) return x;
  return null;
};
Object.defineProperty(Element.prototype, 'dataset', { get() { return (this._dataset ||= {}); } });
// The hidden attribute set at creation reads back as the hidden property.
Object.defineProperty(Element.prototype, 'hidden', {
  get() { return this._hidden ?? this.getAttribute('hidden') !== null; },
  set(v) { this._hidden = !!v; },
});

// The delete deadlines (30 s and 10 s) run twenty times faster (1.5 s and 0.5 s).
const realSetTimeout = globalThis.setTimeout;
globalThis.setTimeout = (fn, ms, ...args) => realSetTimeout(fn, ms === 30000 || ms === 10000 ? ms / 20 : ms, ...args);
const tick = () => new Promise((r) => realSetTimeout(r, 20));

const list = await import('./list.js');
const ui = await import('./ui.js');

const toasts = document.createElement('div');
document.getElementById = (id) => (id === 'toasts' ? toasts : null);
const lastToast = () => toasts.childNodes.at(-1)?.textContent || '';
const windowListeners = new Map();
globalThis.window = {
  addEventListener(type, fn) { windowListeners.set(type, fn); },
  removeEventListener(type, fn) { if (windowListeners.get(type) === fn) windowListeners.delete(type); },
};
// Document listeners: the menu's outside press and the list's visibility.
const docListeners = new Map();
document.addEventListener = (type, fn) => { (docListeners.get(type) || docListeners.set(type, new Set()).get(type)).add(fn); };
document.removeEventListener = (type, fn) => { docListeners.get(type)?.delete(fn); };
// One event object goes to every listener, as in a browser.
const pressOutside = (pointerId) => {
  const ev = { target: document.createElement('div'), pointerId, preventDefault() {} };
  for (const fn of [...(docListeners.get('pointerdown') || [])]) fn(ev);
};
const fire = (type, ev = {}) => { for (const fn of [...(docListeners.get(type) || [])]) fn(ev); };
const becomeVisible = () => { document.visibilityState = 'visible'; for (const fn of [...(docListeners.get('visibilitychange') || [])]) fn(); };

const flatsOf = (...slugs) => ({ flats: slugs.map((slug) => ({
  slug, name: slug === 'notes' ? 'Team notes' : slug, type: slug === 'notes' ? 'docs' : 'flat', visibility: 'private',
  running: true, latest_version: 1, live_version: 1, latest_deployed: true, updated_at: '2026-10-02T00:00:00Z',
  private_url: `http://${slug}.localhost:7879`,
})) });

// Each test sets the replies: listed is the flat list (or a function), del
// answers DELETE. A reply may wait for the request's abort signal.
let listed;
let del;
let decide = () => ({ ok: true, status: 200, json: async () => ({ id: 'a1', status: 'approved' }) });
let calls;
const hang = (opts) => new Promise((_, reject) => opts.signal?.addEventListener('abort', () => reject(new Error('aborted'))));
globalThis.fetch = async (url, opts = {}) => {
  const method = opts.method || 'GET';
  calls.push(method + ' ' + url);
  if (method === 'DELETE') return del(opts);
  if (method === 'POST' && url.includes('/approvals/')) return decide(opts);
  const one = method === 'GET' && url.match(/^\/console\/api\/flats\/([a-z0-9-]+)$/);
  if (one) {
    const f = (listed.flats || []).find((x) => x.slug === one[1]);
    return f ? { ok: true, status: 200, json: async () => ({ ...f, endpoints: [], providers: [] }) } : { ok: false, status: 404, json: async () => ({ error: 'not found', category: 'not_found' }) };
  }
  if (url === '/console/api/flats') {
    if (typeof listed === 'function') return listed(opts);
    return { ok: true, status: 200, json: async () => listed };
  }
  return { ok: true, status: 200, json: async () => (url.includes('/approvals') ? { approvals: [] } : {}) };
};
const answer = (status, body) => () => ({ ok: status >= 200 && status < 300, status, json: async () => { if (body === undefined) throw new Error('not JSON'); return body; } });
const deletes = () => calls.filter((c) => c.startsWith('DELETE')).length;
const listReads = () => calls.filter((c) => c === 'GET /console/api/flats').length;

let alive = true;
const ctx = { setTitle() {}, alive: () => alive, navigate() {} };
// Each page polls approvals; stopping them all lets Node exit.
const stops = [];
async function page(slugs) {
  listed = flatsOf(...slugs);
  calls = [];
  alive = true;
  const main = document.createElement('main');
  stops.push(list.mount(main, [], ctx));
  await tick();
  return main;
}
const rowOf = (main, slug) => all(main, (e) => e.tagName === 'LI' && e.className === 'flat')
  .find((li) => all(li, (e) => e.tagName === 'A' && e.className === 'flat-name')[0].getAttribute('href') === '/flats/' + slug);
const moreButton = (main, slug) => all(rowOf(main, slug), (e) => e.getAttribute('aria-haspopup') === 'menu')[0];
const menuOf = (main, slug) => all(rowOf(main, slug), (e) => e.getAttribute('role') === 'menu')[0];
const entries = (menu) => menu.childNodes.map((e) => (e.getAttribute('role') || e.className) + ':' + e.textContent);
const press = (el, k, { repeat = false } = {}) => {
  const ev = { key: k, repeat, prevented: false, stopped: false, preventDefault() { this.prevented = true; }, stopPropagation() { this.stopped = true; } };
  for (const fn of el.listeners.keydown || []) fn(ev);
  return ev;
};
// openMenu opens a row's menu as a click on "…" would.
function openMenu(main, slug) {
  moreButton(main, slug).dispatch('click');
  return menuOf(main, slug);
}
// arm opens the menu and chooses Delete.
function arm(main, slug) {
  const menu = openMenu(main, slug);
  menu.childNodes.find((e) => e.textContent === 'Delete').dispatch('click');
  const slide = menu.childNodes.find((e) => e.className === 'slide');
  const handle = all(slide, (e) => e.getAttribute('role') === 'slider')[0];
  return { menu, slide, handle };
}
async function slideToEnd(handle) {
  for (let i = 0; i < 10; i++) press(handle, 'ArrowRight');
  await tick();
}

// The menu ends with a divider and a red Delete, for every content type.
let main = await page(['blog', 'notes']);
for (const slug of ['blog', 'notes']) {
  const menu = openMenu(main, slug);
  assert.deepEqual(entries(menu).slice(-3), ['menuitem:Settings', 'separator:', 'menuitem:Delete']);
  assert.ok(hasClass(menu.childNodes.at(-1), 'danger'));
  moreButton(main, slug).dispatch('click');
}

// Choosing Delete keeps the menu open and puts the slide control exactly
// where the item was; every other entry stays. Focus moves to the handle,
// announced with the flat's name. There is no other text.
let d = arm(main, 'notes');
assert.equal(d.menu.hidden, false);
assert.deepEqual(entries(d.menu).slice(-6, -1), ['menuitem:Share', 'menuitem:Overview', 'menuitem:Analytics', 'menuitem:Settings', 'separator:']);
assert.equal(entries(d.menu)[0].startsWith('menuitem:Open privately'), true, 'the address link stays first');
assert.equal(d.menu.childNodes.at(-1), d.slide);
assert.equal(d.slide.textContent, 'Slide to delete');
assert.equal(document.activeElement, d.handle);
assert.equal(d.handle.getAttribute('aria-label'), 'Slide to delete Team notes');
assert.equal(d.handle.getAttribute('aria-valuenow'), '0');
// Escape closes the menu without deleting, focus goes back to "…", and the
// next opening shows the plain Delete item again.
let ev = press(d.menu, 'Escape');
assert.ok(ev.prevented);
assert.equal(d.menu.hidden, true);
assert.equal(document.activeElement, moreButton(main, 'notes'));
assert.equal(deletes(), 0);
let menu = openMenu(main, 'notes');
assert.deepEqual(entries(menu).slice(-1), ['menuitem:Delete']);
// A press outside and Tab close it too, before the end.
menu.childNodes.find((e) => e.textContent === 'Delete').dispatch('click');
pressOutside();
assert.equal(menu.hidden, true);
d = arm(main, 'notes');
press(d.menu, 'Tab');
assert.equal(d.menu.hidden, true);
assert.equal(deletes(), 0);

// Keyboard: Right and Left move a tenth per press and are kept from the
// menu; held keys move once; Home resets; End and Page Up do nothing; Up and
// Down still belong to the menu and moving focus away resets the handle.
d = arm(main, 'blog');
ev = press(d.handle, 'ArrowRight');
assert.ok(ev.prevented && ev.stopped);
for (let i = 0; i < 20; i++) press(d.handle, 'ArrowRight', { repeat: true });
for (const k of ['End', 'PageUp', 'PageDown']) {
  ev = press(d.handle, k);
  assert.ok(ev.prevented && ev.stopped, k);
}
assert.equal(d.handle.getAttribute('aria-valuenow'), '10');
ev = press(d.handle, 'Home');
assert.ok(ev.stopped);
assert.equal(d.handle.getAttribute('aria-valuenow'), '0');
for (let i = 0; i < 9; i++) press(d.handle, 'ArrowRight');
assert.equal(d.handle.getAttribute('aria-valuenow'), '90');
press(d.handle, 'ArrowLeft');
assert.equal(d.handle.getAttribute('aria-valuenow'), '80');
ev = press(d.handle, 'ArrowDown');
assert.ok(!ev.stopped, 'Down is left to the menu');
document.activeElement = d.handle;
press(d.menu, 'ArrowDown');
assert.ok(document.activeElement.textContent.startsWith('Open privately'), 'Down from the handle wraps to the first item');
d.handle.dispatch('blur');
assert.equal(d.handle.getAttribute('aria-valuenow'), '0');
press(d.menu, 'ArrowUp');
assert.equal(document.activeElement, d.handle, 'Up from the first item reaches the handle in Delete\'s place');
assert.equal(deletes(), 0);

// The tenth press deletes. While pending: one DELETE, other items disabled,
// Escape, Tab, arrows, outside presses and "…" do not close the menu or move
// focus, another row's menu does not open, and the list is not redrawn.
let release;
del = () => new Promise((r) => { release = () => r({ ok: true, status: 200, json: async () => ({ status: 'done' }) }); });
await slideToEnd(d.handle);
assert.equal(deletes(), 1);
assert.deepEqual(calls.filter((c) => c.startsWith('DELETE')), ['DELETE /console/api/flats/blog?reason=deleted%20in%20the%20console']);
assert.equal(d.slide.getAttribute('data-state'), 'busy');
assert.equal(d.handle.getAttribute('aria-disabled'), 'true');
assert.ok(d.slide.textContent.includes('Deleting…') && d.slide.textContent.includes('Deleting blog…'));
assert.ok(d.menu.childNodes.filter((e) => hasClass(e, 'menu-item')).every((e) => e.disabled || e.getAttribute('aria-disabled') === 'true'));
for (const k of ['Escape', 'Tab', 'ArrowDown', 'ArrowUp', 'Home', 'End']) {
  document.activeElement = d.handle;
  assert.ok(press(d.menu, k).prevented, k);
  assert.equal(document.activeElement, d.handle, k);
}
press(d.handle, 'Home');
pressOutside();
// The click that follows an outside press (on a link, say) is dropped, however
// long the press lasts (no timer ends the guard), and the guard ends with the
// press. The wait stays under the sped-up 1.5 s delete deadline.
// detail is 1 for a pointer click and 0 for one a key makes.
const clickEvent = (detail = 1) => ({ detail, prevented: false, stopped: false, preventDefault() { this.prevented = true; }, stopPropagation() { this.stopped = true; } });
await new Promise((r) => realSetTimeout(r, 200));
let click = clickEvent();
fire('pointerup');
fire('click', click);
assert.ok(click.prevented && click.stopped, 'the click after an outside press is dropped while deleting');
await tick();
click = clickEvent();
fire('click', click);
assert.ok(!click.prevented, 'a later click is not dropped');
// Links in the menu (the flat's addresses) are disabled too and do not
// navigate; pressing this menu's own "…" button keeps focus on the handle.
const link = d.menu.childNodes.find((e) => e.tagName === 'A');
assert.equal(link.getAttribute('aria-disabled'), 'true');
click = clickEvent();
for (const fn of link.listeners.click || []) fn(click);
assert.ok(click.prevented, 'a held link does not navigate');
assert.equal(d.menu.hidden, false);
// An interrupted press (the window loses focus before any pointerup) does
// not leave clicks blocked, and neither does a key or a new press.
pressOutside();
windowListeners.get('blur')();
click = clickEvent();
fire('click', click);
assert.ok(!click.prevented, 'window blur ends the click guard');
// A key during the blocked press does not let its click through; a click a
// key makes is never dropped; a new press ends the guard before its click.
pressOutside();
fire('keydown', { key: 'Shift' });
click = clickEvent(0);
fire('click', click);
assert.ok(!click.prevented, 'a keyboard click is not dropped');
click = clickEvent();
fire('click', click);
assert.ok(click.prevented, 'a key does not end the blocked press');
windowListeners.get('blur')();
// Overlapping pointers: a second pointer's press and release cannot lift
// the first press's guard; the first press's own release ends it.
pressOutside(1);
pressOutside(2);
fire('pointerup', { pointerId: 2 });
await tick();
click = { ...clickEvent(), pointerId: 1 };
fire('click', click);
assert.ok(click.prevented, 'another pointer does not lift the guard');
fire('pointerup', { pointerId: 1 });
click = { ...clickEvent(), pointerId: 1 };
fire('click', click);
assert.ok(click.prevented, 'the press\'s own click is still dropped');
await tick();
click = { ...clickEvent(), pointerId: 1 };
fire('click', click);
assert.ok(!click.prevented, 'its release ended the guard');
const down = clickEvent();
for (const fn of moreButton(main, 'blog').listeners.pointerdown || []) fn(down);
assert.ok(down.prevented, 'pressing "…" while held keeps focus');
const itemDown = { ...clickEvent(), target: link };
for (const fn of d.menu.listeners.pointerdown || []) fn(itemDown);
assert.ok(itemDown.prevented, 'pressing a held item keeps focus');
const handleDown = { ...clickEvent(), target: d.handle };
for (const fn of d.menu.listeners.pointerdown || []) fn(handleDown);
assert.ok(!handleDown.prevented, 'the handle itself is not blocked by the menu');
moreButton(main, 'blog').dispatch('click');
assert.equal(d.menu.hidden, false);
moreButton(main, 'notes').dispatch('click');
assert.equal(menuOf(main, 'notes').hidden, true, 'another row\'s menu does not open');
await slideToEnd(d.handle);
assert.equal(deletes(), 1, 'one DELETE per completed slide');
listed = () => { throw new Error('offline'); };
becomeVisible();
await tick();
assert.ok(rowOf(main, 'blog'), 'a failed refresh while pending keeps the row');
assert.ok(!main.textContent.includes('Cannot load flats'));
listed = flatsOf('notes');
// A press outside with no release yet: its click stays dropped even if the
// deletion ends first.
pressOutside();
const readsBefore = listReads();
becomeVisible();
await tick();
assert.ok(rowOf(main, 'blog'), 'no redraw while pending');
assert.equal(d.menu.hidden, false);
// Confirmed: the menu closes, a toast confirms, the row goes and focus moves
// to the row that took its place.
release();
await tick();
await tick();
assert.equal(lastToast(), 'blog was deleted.');
assert.equal(rowOf(main, 'blog'), undefined);
assert.ok(listReads() > readsBefore);
assert.equal(document.activeElement, moreButton(main, 'notes'));
click = clickEvent();
fire('pointerup');
fire('click', click);
assert.ok(click.prevented, 'the blocked press still cannot click after the deletion ended');
await tick();
click = clickEvent();
fire('click', click);
assert.ok(!click.prevented, 'a later click works');
// A press whose release never arrives: the next press ends its guard, so that
// press's own click works once nothing is held.
fire('pointerdown', { target: document.createElement('div') });
click = clickEvent();
fire('click', click);
assert.ok(!click.prevented, 'no blocker is left behind');

// Deleting the last row focuses the row above; the only row, the heading.
main = await page(['blog', 'notes']);
del = answer(200, { status: 'done' });
d = arm(main, 'notes');
listed = flatsOf('blog');
await slideToEnd(d.handle);
await tick();
assert.equal(document.activeElement, moreButton(main, 'blog'));
d = arm(main, 'blog');
listed = flatsOf();
await slideToEnd(d.handle);
await tick();
assert.equal(document.activeElement.tagName, 'H1');

// Not found (404 with not_found): already deleted.
main = await page(['blog']);
del = answer(404, { error: 'flat "blog" not found', category: 'not_found' });
d = arm(main, 'blog');
listed = flatsOf();
await slideToEnd(d.handle);
await tick();
assert.equal(d.menu.hidden, true);
assert.equal(lastToast(), 'blog was already deleted.');

// A host error with the flat still listed (including a 404 without
// not_found): the menu stays, the track asks for a retry, the handle is back
// and focused, items work again, the toast has the host's message, and a
// second slide can delete.
for (const failure of [answer(409, { error: 'another change to this flat is running', category: 'conflict' }), answer(404, { error: 'no route' })]) {
  main = await page(['blog']);
  del = failure;
  d = arm(main, 'blog');
  await slideToEnd(d.handle);
  await tick();
  assert.equal(d.menu.hidden, false);
  assert.equal(listReads(), 2);
  assert.equal(d.slide.textContent.startsWith('Failed. Slide to retry'), true);
  assert.equal(d.handle.getAttribute('aria-valuenow'), '0');
  assert.equal(d.handle.getAttribute('aria-disabled'), null);
  assert.equal(document.activeElement, d.handle);
  assert.ok(d.menu.childNodes.filter((e) => hasClass(e, 'menu-item')).every((e) => !e.disabled && e.getAttribute('aria-disabled') !== 'true'));
  assert.ok(lastToast().startsWith('Delete failed: '));
  del = answer(200, { status: 'done' });
  listed = flatsOf();
  await slideToEnd(d.handle);
  await tick();
  assert.equal(d.menu.hidden, true);
  assert.equal(lastToast(), 'blog was deleted.');
}

// A press begun on a held item (the address link) cannot activate it when
// its release comes after a retryable error re-enabled the items; a fresh
// press afterwards works.
{
  main = await page(['blog']);
  let answerDelete;
  del = () => new Promise((r) => { answerDelete = () => r({ ok: false, status: 409, json: async () => ({ error: 'busy', category: 'conflict' }) }); });
  d = arm(main, 'blog');
  await slideToEnd(d.handle);
  const link = d.menu.childNodes.find((e) => e.tagName === 'A');
  const ev = (extra) => ({ detail: 1, pointerId: 3, target: link, prevented: false, stopped: false, preventDefault() { this.prevented = true; }, stopPropagation() { this.stopped = true; }, ...extra });
  // A click reaches the link only when the document did not stop it.
  const clickLink = () => {
    const c = ev();
    fire('click', c);
    if (!c.stopped) for (const fn of link.listeners.click || []) fn(c);
    return c;
  };
  const down = ev();
  fire('pointerdown', down);
  for (const fn of d.menu.listeners.pointerdown || []) fn(down);
  assert.ok(down.prevented);
  answerDelete();
  await tick();
  assert.ok(d.slide.textContent.startsWith('Failed. Slide to retry'));
  assert.equal(link.getAttribute('aria-disabled'), null, 'items work again');
  fire('pointerup', ev());
  let c = clickLink();
  assert.ok(c.prevented, 'the press begun while held does not activate the link');
  assert.equal(d.menu.hidden, false);
  await tick();
  const fresh = ev();
  fire('pointerdown', fresh);
  for (const fn of d.menu.listeners.pointerdown || []) fn(fresh);
  assert.ok(!fresh.prevented, 'a fresh press is not blocked');
  fire('pointerup', ev());
  c = clickLink();
  assert.ok(!c.prevented, 'and its click works');
  assert.equal(d.menu.hidden, true, 'choosing the link closes the menu');
}

// A host error after the record is gone: deleted, with the problem kept.
main = await page(['blog']);
del = answer(500, { error: 'flat "blog" metadata deleted but storage cleanup is incomplete: disk busy' });
d = arm(main, 'blog');
listed = flatsOf();
await slideToEnd(d.handle);
await tick();
assert.equal(d.menu.hidden, true);
assert.equal(lastToast(), 'blog was deleted, but the host reported a problem: flat "blog" metadata deleted but storage cleanup is incomplete: disk busy');
assert.equal(rowOf(main, 'blog'), undefined);
// flats: null is a valid empty list.
main = await page(['blog']);
del = answer(500, { error: 'internal error' });
d = arm(main, 'blog');
listed = { flats: null };
await slideToEnd(d.handle);
await tick();
assert.equal(d.menu.hidden, true);
assert.ok(lastToast().startsWith('blog was deleted, but the host reported a problem:'));

// unknownShown checks the unknown result: locked control, items working,
// the toast.
function unknownShown(dd, why) {
  assert.equal(dd.menu.hidden, false, why);
  assert.equal(dd.slide.getAttribute('data-state'), 'locked', why);
  assert.ok(dd.slide.textContent.startsWith('Not confirmed'), why);
  assert.equal(dd.handle.getAttribute('aria-disabled'), 'true', why);
  assert.ok(dd.menu.childNodes.filter((e) => hasClass(e, 'menu-item')).every((e) => !e.disabled && e.getAttribute('aria-disabled') !== 'true'), why);
  assert.ok(lastToast().startsWith('Could not confirm whether blog was deleted; the deletion may still finish.'), why);
  const before = dd.handle.getAttribute('aria-valuenow');
  press(dd.handle, 'Home');
  assert.equal(dd.handle.getAttribute('aria-valuenow'), before, why);
}

// A host error and an unreadable list (failure, malformed, no answer in 10 s).
for (const [why, reply] of [
  ['list fails', () => { throw new Error('offline'); }],
  ['list malformed', () => ({ ok: true, status: 200, json: async () => ({}) })],
  ['list entry without a slug', () => ({ ok: true, status: 200, json: async () => ({ flats: [{}] }) })],
  ['list entry null', () => ({ ok: true, status: 200, json: async () => ({ flats: [null] }) })],
  ['list hangs', (opts) => hang(opts)],
]) {
  main = await page(['blog']);
  del = answer(500, { error: 'internal error' });
  d = arm(main, 'blog');
  listed = reply;
  await slideToEnd(d.handle);
  await new Promise((r) => realSetTimeout(r, why === 'list hangs' ? 650 : 20));
  unknownShown(d, why);
}

// No Flats answer: unknown even though the flat is still listed, and the
// list is not read to decide. Closing refreshes the list, which then shows
// the deletion that finished meanwhile.
for (const [why, reply] of [
  ['connection reset', () => { throw new Error('connection reset'); }],
  ['proxy page', answer(502, undefined)],
  ['proxy JSON without error', answer(503, { message: 'upstream down' })],
  ['2xx not a result', answer(200, undefined)],
  ['2xx empty object', answer(200, {})],
  ['2xx pending approval', answer(202, { status: 'pending_approval' })],
  ['approval never decided', answer(202, { status: 'pending_approval', approval: { id: 'a1', status: 'pending' } })],
  ['no answer in 30 s', (opts) => hang(opts)],
]) {
  main = await page(['blog', 'notes']);
  del = reply;
  decide = why === 'approval never decided' ? (opts) => hang(opts) : decide;
  d = arm(main, 'blog');
  const reads = listReads();
  await slideToEnd(d.handle);
  await new Promise((r) => realSetTimeout(r, why === 'no answer in 30 s' || why === 'approval never decided' ? 1700 : 20));
  unknownShown(d, why);
  assert.equal(listReads(), reads, why);
  listed = flatsOf('notes');
  press(d.menu, 'Escape');
  await tick();
  assert.equal(d.menu.hidden, true, why);
  assert.equal(rowOf(main, 'blog'), undefined, why);
  assert.equal(document.activeElement, moreButton(main, 'notes'), why);
}
// A late answer after the deadline is ignored: no second outcome, the
// unknown result and the menu stay as they are.
main = await page(['blog', 'notes']);
del = () => new Promise((r) => realSetTimeout(() => r({ ok: true, status: 200, json: async () => ({ status: 'done' }) }), 2000));
d = arm(main, 'blog');
await slideToEnd(d.handle);
await new Promise((r) => realSetTimeout(r, 1700));
unknownShown(d, 'before the late answer');
const toastsBefore = toasts.childNodes.length;
listed = flatsOf('notes');
await new Promise((r) => realSetTimeout(r, 600));
unknownShown(d, 'after the late answer');
assert.equal(toasts.childNodes.length, toastsBefore, 'no second outcome toast');
assert.ok(rowOf(main, 'blog'), 'the late answer does not remove the row');
press(d.menu, 'Escape');
await tick();

// After an unknown result, a flat still listed keeps focus on its own "…".
main = await page(['blog']);
del = answer(504, undefined);
d = arm(main, 'blog');
await slideToEnd(d.handle);
await tick();
pressOutside();
await tick();
assert.equal(document.activeElement, moreButton(main, 'blog'));

// A redraw asked for while pending is held back through a retry and through
// an unknown result: the row, the open menu, the label, the enabled state
// and focus stay until the menu closes, and then the list is redrawn.
for (const [why, reply, state] of [
  ['retry', answer(409, { error: 'busy', category: 'conflict' }), 'retry'],
  ['unknown', answer(502, undefined), 'unknown'],
]) {
  main = await page(['blog', 'notes']);
  del = () => new Promise((r) => { release = () => r(reply()); });
  d = arm(main, 'blog');
  await slideToEnd(d.handle);
  const row = rowOf(main, 'blog');
  becomeVisible();
  await tick();
  release();
  await tick();
  await tick();
  // retained checks the row, the open menu, the label, the enabled states
  // and focus that the held list must keep.
  const retained = (when) => {
    const w = `${why} (${when})`;
    assert.equal(rowOf(main, 'blog'), row, w);
    assert.equal(d.menu.hidden, false, w);
    assert.equal(document.activeElement, d.handle, w);
    assert.ok(d.menu.childNodes.filter((e) => hasClass(e, 'menu-item')).every((e) => !e.disabled && e.getAttribute('aria-disabled') !== 'true'), w + ': items work again');
    if (state === 'retry') {
      assert.ok(d.slide.textContent.startsWith('Failed. Slide to retry'), w);
      assert.equal(d.handle.getAttribute('aria-disabled'), null, w);
      assert.equal(d.slide.getAttribute('data-state') === 'busy' || d.slide.getAttribute('data-state') === 'locked', false, w);
    } else {
      assert.ok(d.slide.textContent.startsWith('Not confirmed'), w);
      assert.equal(d.handle.getAttribute('aria-disabled'), 'true', w);
      assert.equal(d.slide.getAttribute('data-state'), 'locked', w);
    }
  };
  retained('after the outcome');
  becomeVisible();
  await tick();
  retained('after another refresh');
  press(d.menu, 'Escape');
  await tick();
  assert.notEqual(rowOf(main, 'blog'), row, why + ': redrawn after the menu closed');
  assert.ok(rowOf(main, 'blog'), why);
  assert.equal(document.activeElement, moreButton(main, 'blog'), why + ': focus on the mounted "…" button');
}

// Consecutive deletes: a slow refresh after the first cannot take focus
// from the second while it is pending, and focus does not wait for it.
main = await page(['blog', 'notes', 'shop']);
del = answer(200, { status: 'done' });
let releaseList;
listed = () => new Promise((r) => { releaseList = () => r({ ok: true, status: 200, json: async () => flatsOf('shop') }); });
d = arm(main, 'blog');
await slideToEnd(d.handle);
await tick();
assert.equal(rowOf(main, 'blog'), undefined);
assert.equal(document.activeElement, moreButton(main, 'notes'), 'focus moves before the refresh answers');
del = () => new Promise((r) => { release = () => r({ ok: true, status: 200, json: async () => ({ status: 'done' }) }); });
const second = arm(main, 'notes');
await slideToEnd(second.handle);
assert.equal(second.slide.getAttribute('data-state'), 'busy');
releaseList();
await tick();
assert.equal(document.activeElement, second.handle, 'an earlier refresh does not take focus from a pending delete');
listed = flatsOf('shop');
release();
await tick();
await tick();
assert.equal(document.activeElement, moreButton(main, 'shop'));

// An older answer cannot bring back a deleted flat: a refresh that started
// while the delete was pending and answers last is ignored.
main = await page(['blog', 'notes']);
del = () => new Promise((r) => { release = () => r({ ok: true, status: 200, json: async () => ({ status: 'done' }) }); });
d = arm(main, 'blog');
await slideToEnd(d.handle);
let releaseOld;
listed = () => new Promise((r) => { releaseOld = () => r({ ok: true, status: 200, json: async () => flatsOf('blog', 'notes') }); });
becomeVisible();
await tick();
listed = flatsOf('notes');
release();
await tick();
await tick();
assert.equal(rowOf(main, 'blog'), undefined);
releaseOld();
await tick();
assert.equal(rowOf(main, 'blog'), undefined, 'an older answer is ignored');

// A refresh that never answers does not hold focus back.
main = await page(['blog', 'notes']);
del = answer(200, { status: 'done' });
d = arm(main, 'blog');
listed = (opts) => hang(opts);
await slideToEnd(d.handle);
await tick();
assert.equal(rowOf(main, 'blog'), undefined);
assert.equal(document.activeElement, moreButton(main, 'notes'));

// A press during deletion that never releases: once the deletion is over,
// the next press ends its guard, so later clicks work.
main = await page(['blog']);
del = () => new Promise((r) => { release = () => r({ ok: true, status: 200, json: async () => ({ status: 'done' }) }); });
d = arm(main, 'blog');
await slideToEnd(d.handle);
pressOutside();
listed = flatsOf();
release();
await tick();
assert.equal(d.menu.hidden, true);
fire('pointerdown', { target: document.createElement('div') });
let later = { detail: 1, prevented: false, preventDefault() { this.prevented = true; }, stopPropagation() {} };
fire('click', later);
assert.ok(!later.prevented, 'no blocker is left after an unreleased press');

// Leaving the page while pending: the request is not cancelled, the outcome
// still appears as a toast, and focus is not moved.
main = await page(['blog']);
del = () => new Promise((r) => { release = () => r({ ok: true, status: 200, json: async () => ({ status: 'done' }) }); });
d = arm(main, 'blog');
await slideToEnd(d.handle);
ui.closeMenus();
alive = false;
const focusBefore = document.activeElement;
release();
await tick();
assert.equal(lastToast(), 'blog was deleted.');
assert.equal(document.activeElement, focusBefore);
alive = true;

// Pointer drags: only the handle's own drag moves it, a release before the
// end returns it, focus or window loss and a cancelled pointer cancel the
// drag so its later moves cannot delete; one pixel short does not delete; a
// drag to the end does.
const pointer = (el, type, clientX) => { for (const fn of el.listeners[type] || []) fn({ type, pointerId: 7, button: 0, clientX, preventDefault() {} }); };
main = await page(['blog']);
del = answer(200, { status: 'done' });
d = arm(main, 'blog');
Object.assign(d.slide, { clientWidth: 196 });
Object.assign(d.handle, { offsetWidth: 30.5, offsetLeft: 3 }); // travel 159.5 px
assert.equal(['pointerdown', 'pointermove', 'click'].some((t) => (d.slide.listeners[t] || []).length), false, 'the track has no pointer handlers');
pointer(d.handle, 'pointermove', 100);
assert.equal(d.handle.getAttribute('aria-valuenow'), '0');
pointer(d.handle, 'pointerdown', 0);
assert.equal(document.activeElement, d.handle);
pointer(d.handle, 'pointermove', 79.75);
assert.equal(d.handle.getAttribute('aria-valuenow'), '50');
pointer(d.handle, 'pointerup', 79.75);
assert.equal(d.handle.getAttribute('aria-valuenow'), '0');
for (const end of ['blur', 'window', 'pointercancel']) {
  pointer(d.handle, 'pointerdown', 0);
  pointer(d.handle, 'pointermove', 79.75);
  if (end === 'blur') d.handle.dispatch('blur');
  else if (end === 'window') windowListeners.get('blur')();
  else pointer(d.handle, 'pointercancel', 79.75);
  assert.equal(d.handle.getAttribute('aria-valuenow'), '0', end);
  pointer(d.handle, 'pointermove', 400);
  assert.equal(d.handle.getAttribute('aria-valuenow'), '0', end);
}
await tick();
assert.equal(deletes(), 0);
pointer(d.handle, 'pointerdown', 0);
pointer(d.handle, 'pointermove', 159);
await tick();
assert.equal(d.handle.getAttribute('aria-valuenow'), '99');
assert.equal(deletes(), 0);
pointer(d.handle, 'pointermove', 159.5);
await tick();
assert.equal(deletes(), 1);
assert.equal(d.menu.hidden, true);

// The list keeps its grid view and search after a delete; deleting the only
// match while others remain focuses the heading.
main = await page(['blog', 'notes', 'shop']);
all(main, (e) => e.getAttribute('aria-label') === 'Grid view')[0].dispatch('click');
const search = all(main, (e) => e.tagName === 'INPUT' && e.getAttribute('type') === 'search')[0];
search.value = 'sho';
search.dispatch('input');
del = answer(200, { status: 'done' });
d = arm(main, 'shop');
listed = flatsOf('blog', 'notes');
await slideToEnd(d.handle);
await tick();
assert.equal(search.value, 'sho');
assert.equal(all(main, (e) => e.getAttribute('aria-label') === 'Grid view')[0].getAttribute('aria-pressed'), 'true');
assert.equal(document.activeElement.tagName, 'H1');
assert.ok(main.textContent.includes('No flats match'));

// The Settings page keeps its typed-slug deletion: empty or wrong text keeps
// the button disabled, Cancel sends nothing, the exact slug enables it.
const flatPage = await import('./flat.js');
listed = flatsOf('notes');
calls = [];
const settingsMain = document.createElement('main');
stops.push(flatPage.mountSettings(settingsMain, ['notes'], ctx));
await tick();
const byText = (root, tag, text) => all(root, (e) => e.tagName === tag && e.textContent === text);
const openTyped = () => {
  byText(settingsMain, 'BUTTON', 'Delete flat…')[0].dispatch('click');
  const dlg = all(document.body, (e) => e.tagName === 'DIALOG').at(-1);
  return { dlg, input: all(dlg, (e) => e.tagName === 'INPUT')[0], confirm: byText(dlg, 'BUTTON', 'Delete permanently')[0], cancel: byText(dlg, 'BUTTON', 'Cancel')[0] };
};
let typed = openTyped();
assert.ok(typed.confirm.disabled);
for (const text of ['', 'note', 'Notes', 'notes ']) {
  typed.input.value = text;
  typed.input.dispatch('input');
  assert.ok(typed.confirm.disabled, `"${text}" enables deletion`);
}
typed.cancel.dispatch('click');
await tick();
assert.equal(deletes(), 0);
typed = openTyped();
typed.input.value = 'notes';
typed.input.dispatch('input');
assert.ok(!typed.confirm.disabled);
typed.dlg.close('ok');
await tick();
assert.deepEqual(calls.filter((c) => c.startsWith('DELETE')), ['DELETE /console/api/flats/notes?reason=deleted%20in%20the%20console']);

for (const stop of stops) stop();
console.log('list delete ok');
