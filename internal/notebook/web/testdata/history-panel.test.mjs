'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { installDOM, mount } from './db-dom.mjs';
import { History } from '../history.js';

installDOM();

const NOW = Date.parse('2026-10-31T12:00:00Z');

function fakeFetch(queue) {
  const calls = [];
  const fn = async (url, init = {}) => {
    calls.push({ url, init, body: init.body ? JSON.parse(init.body) : undefined });
    let next = queue.shift();
    if (!next) throw new Error(`unexpected request ${url}`);
    if (typeof next === 'function') next = await next();
    const status = next.status ?? 200;
    return { status, ok: status >= 200 && status < 300, json: async () => next.json };
  };
  fn.calls = calls;
  fn.queue = queue;
  return fn;
}

const settle = async () => { for (let i = 0; i < 10; i++) await new Promise((r) => setImmediate(r)); };
const button = (el, text) => el.querySelectorAll('button').find((b) => b.textContent === text || b.getAttribute('aria-label') === text) ?? null;
const rowOf = (el, n) => el.querySelectorAll('li').find((li) => li.getAttribute('data-revision') === String(n));
const pageRow = (el, id) => el.querySelectorAll('li').find((li) => li.getAttribute('data-page') === id).querySelector('.history-page-row');

const REV3 = { number: 3, kind: 'person', message: 'Tidy the plan', authors: ['Ada', 'Grace'], pinned: false, created: '2026-10-31T11:58:00Z', pages: [{ page: 'p_plan', title: 'Plan', change: 'changed' }, { page: 'p_old', title: 'Old notes', change: 'removed' }] };
const REV2 = { number: 2, kind: 'agent', authors: ['Helper'], pinned: true, created: '2026-10-30T09:00:00Z', pages: [{ page: 'p_plan', title: 'Plan', change: 'added' }] };
const REV1 = { number: 1, kind: 'created', pinned: false, created: '2026-10-29T09:00:00Z' };

const TREE = {
  revision: 2,
  tree: [
    { ref: { id: 'p_child', inc: '1' }, kind: 'page', title: 'Child', parent: 'p_plan', position: 'a' },
    { ref: { id: 'p_plan', inc: '1' }, kind: 'page', title: 'Plan', parent: null, position: 'a' },
    { ref: { id: 'p_gone', inc: '1' }, kind: 'page', title: 'Gone', parent: null, position: 'b' },
  ],
  next: null, complete: true, warnings: [],
};

function setup(queue, opts = {}) {
  const host = mount();
  const fetch = fakeFetch(queue);
  const events = { closed: 0, recovered: [], rolledBack: [] };
  const history = new History(host, {
    fetch,
    now: () => NOW,
    session: () => 'sess-1',
    currentPages: () => new Set(['p_plan']),
    onClose: () => events.closed++,
    onRecovered: (e) => events.recovered.push(e),
    onRolledBack: (b) => events.rolledBack.push(b),
    ...opts,
  });
  return { host, fetch, events, history };
}

test('canonical historical page objects and record continuations are assembled with image URLs', async () => {
  const hash = 'a'.repeat(64);
  const image = { kind: 'block', ref: { id: 'b_image', inc: '1' }, page: 'p_plan', parent: null, position: 'a0', content: { kind: 'image', asset: hash, alt: 'Photo', caption: [] } };
  const code = { kind: 'block', ref: { id: 'b_code', inc: '1' }, page: 'p_plan', parent: null, position: 'a1', content: { kind: 'code', text: '한글 code', language: '' } };
  const encoded = JSON.stringify(code);
  const { history, fetch } = setup([
    { json: { page: { items: [image] }, assets: { [hash]: `/_notebook/a/${hash}.png` }, complete: false, next: 'one', continuation: { record: 'b_code', part: 0, parts: 2, data: encoded.slice(0, 80) } } },
    { json: { page: { items: [] }, complete: true, next: null, continuation: { record: 'b_code', part: 1, parts: 2, data: encoded.slice(80) } } },
  ]);
  const result = await history._getAll('/_notebook/api/revisions/3?page=p_plan', 'page', () => true);
  assert.deepEqual(result.records.map((r) => r.id), ['b_image', 'b_code']);
  assert.equal(result.records[1].content.text, '한글 code');
  assert.equal(fetch.calls[1].url, '/_notebook/api/revisions/3?cursor=one');
  const rendered = history._renderBlock(result.records[0], [], new Map());
  assert.equal(rendered.querySelector('img').getAttribute('src'), `/_notebook/a/${hash}.png`);
});

test('opening lists revisions with kind, authors, message and changed pages; Load more appends', async () => {
  const { host, fetch, history } = setup([
    { json: { revisions: [REV3, REV2], next: 2 } },
    { json: { revisions: [REV1] } },
  ]);
  assert.equal(host.hidden, true);
  history.toggle(true);
  assert.equal(history.open, true);
  assert.equal(host.hidden, false);
  assert.match(host.textContent, /Loading…/);
  await settle();
  assert.equal(fetch.calls[0].url, '/_notebook/api/revisions?limit=50');
  assert.equal(fetch.calls[0].init.credentials, 'same-origin');
  assert.equal(host.querySelector('h2').textContent, 'History');
  const r3 = rowOf(host, 3).textContent;
  assert.match(r3, /Edits/);
  assert.match(r3, /Ada, Grace/);
  assert.match(r3, /Tidy the plan/);
  assert.match(r3, /Plan/);
  assert.match(r3, /Old notes/);
  assert.match(r3, /2 min ago/);
  assert.match(rowOf(host, 2).textContent, /Agent/);
  assert.match(rowOf(host, 2).textContent, /Pinned/);

  button(host, 'Load more').click();
  await settle();
  assert.equal(fetch.calls[1].url, '/_notebook/api/revisions?limit=50&before=2');
  assert.deepEqual(host.querySelectorAll('li').filter((li) => li.hasAttribute('data-revision')).map((li) => li.getAttribute('data-revision')), ['3', '2', '1']);
  assert.match(rowOf(host, 1).textContent, /Created/);
  assert.equal(button(host, 'Load more'), null, 'no next, no Load more');

  button(host, 'Close history').click();
  assert.equal(history.open, false);
  assert.equal(host.hidden, true);
  assert.equal(host.children.length, 0);
});

test('restoring a missing page confirms inline, then posts revision, page and session', async () => {
  const result = { state: 'applied', revision: 9, refs: { root: { ref: { id: 'p_gone', inc: 4 }, ver: 1 } }, copies: [] };
  const { host, fetch, events, history } = setup([
    { json: { revisions: [REV2] } },
    { json: { ...TREE, tree: TREE.tree.slice(0, 1), next: 'c1', complete: false } },
    { json: { ...TREE, tree: TREE.tree.slice(1), next: null, complete: true } },
    { json: result },
  ]);
  history.toggle(true);
  await settle();
  button(host, 'Open revision 2').click();
  await settle();
  assert.equal(fetch.calls[1].url, '/_notebook/api/revisions/2');
  assert.equal(fetch.calls[2].url, '/_notebook/api/revisions/2?cursor=c1');
  // Nested by parent: Child sits inside Plan.
  const plan = host.querySelectorAll('li').find((li) => li.getAttribute('data-page') === 'p_plan');
  assert.ok(plan.querySelectorAll('li').some((li) => li.getAttribute('data-page') === 'p_child'));

  const restore = button(pageRow(host, 'p_gone'), 'Restore page');
  assert.ok(restore, 'missing page offers Restore page');
  restore.click();
  await settle();
  assert.equal(fetch.calls.length, 3, 'nothing is sent before Confirm');
  assert.match(host.textContent, /Restore “Gone” from revision 2/);
  button(host, 'Confirm').click();
  await settle();
  const post = fetch.calls[3];
  assert.equal(post.url, '/_notebook/api/recover');
  assert.equal(post.init.method, 'POST');
  assert.equal(post.init.credentials, 'same-origin');
  assert.equal(post.init.headers['X-Flats-Notebook'], '1');
  assert.equal(post.init.headers['Content-Type'], 'application/json');
  assert.deepEqual(post.body, { revision: 2, page: 'p_gone', session: 'sess-1' });
  assert.equal(events.recovered.length, 1);
  assert.deepEqual(events.recovered[0].root, { id: 'p_gone', inc: 4 });
  assert.equal(events.recovered[0].revision, 2);
  assert.equal(events.recovered[0].page, 'p_gone');
  assert.equal(events.recovered[0].result, result);
  assert.match(host.querySelector('[role=status]').textContent, /Restored “Gone”/);
});

test('a page that exists now is restored as a copy; Cancel sends nothing; no session is refused', async () => {
  const { host, fetch, history } = setup([
    { json: { revisions: [REV2] } },
    { json: TREE },
  ], { session: () => null });
  history.toggle(true);
  await settle();
  button(host, 'Open revision 2').click();
  await settle();
  const row = pageRow(host, 'p_plan');
  assert.equal(button(row, 'Restore page'), null);
  button(row, 'Restore as copy').click();
  assert.match(host.textContent, /“Plan \(restored\)”/);
  button(host, 'Cancel').click();
  assert.equal(button(host, 'Confirm'), null);
  button(pageRow(host, 'p_plan'), 'Restore as copy').click();
  button(host, 'Confirm').click();
  await settle();
  assert.equal(fetch.calls.length, 2, 'no recover request without a session');
  assert.match(host.querySelector('[role=alert]').textContent, /no notebook session/);
});

test('opening a page previews its blocks read-only, nested and in order', async () => {
  const records = [
    { ref: { id: 'p_plan', inc: '1' }, kind: 'page', title: 'Plan', parent: null, position: 'a' },
    { id: 'b2', kind: 'block', page: 'p_plan', parent: null, position: 'b', content: { kind: 'code', text: 'const ready = true;', language: 'js' } },
    { id: 'b1', kind: 'block', page: 'p_plan', parent: null, position: 'a', content: { kind: 'paragraph', text: [{ text: 'Hello ', marks: [] }, { text: 'world', marks: ['bold'] }] } },
    { id: 'b1a', kind: 'block', page: 'p_plan', parent: 'b1', position: 'a', content: { kind: 'todo', checked: true, text: [{ text: 'nested', marks: [] }] } },
  ];
  const more = [
    { id: 'b3', kind: 'block', page: 'p_plan', parent: null, position: 'c', content: { kind: 'divider' } },
    { id: 'b4', kind: 'block', page: 'p_plan', parent: null, position: 'd', content: { kind: 'image', asset: 'a'.repeat(64), alt: 'A chart', caption: [{ text: 'Project totals', marks: [] }] } },
    { id: 'b5', kind: 'block', page: 'p_plan', parent: null, position: 'e', content: { kind: 'database', database: 'd1' } },
    { id: 'd1', kind: 'database', block: 'b5', title: 'Tasks', property_order: [], view_order: [] },
    { id: 'pr1', kind: 'property', database: 'd1', name: 'Status' },
  ];
  const { host, fetch, history } = setup([
    { json: { revisions: [REV2] } },
    { json: TREE },
    { json: { revision: 2, page: records, next: 'k', complete: false, warnings: [] } },
    { json: { revision: 2, page: more, next: null, complete: true, warnings: [] } },
  ]);
  history.toggle(true);
  await settle();
  button(host, 'Open revision 2').click();
  await settle();
  button(pageRow(host, 'p_plan'), 'Open page Plan').click();
  await settle();
  assert.equal(fetch.calls[2].url, '/_notebook/api/revisions/2?page=p_plan');
  assert.equal(fetch.calls[3].url, '/_notebook/api/revisions/2?cursor=k');
  const preview = host.querySelector('.history-preview');
  assert.equal(preview.querySelector('h3').textContent, 'Plan');
  const blocks = preview.querySelectorAll('li');
  assert.deepEqual(blocks.map((b) => b.className.split(' ')[1]), ['block-paragraph', 'block-todo', 'block-code', 'block-divider', 'block-image', 'block-database']);
  assert.match(blocks[0].textContent, /^Hello world/);
  assert.ok(blocks[0].querySelector('li'), 'child block nests under its parent');
  assert.match(blocks[1].textContent, /\[x\] nested/);
  assert.equal(preview.querySelector('code').textContent, 'const ready = true;');
  assert.ok(preview.querySelector('hr'));
  assert.match(preview.textContent, /A chart/);
  assert.equal(preview.querySelector('figcaption').textContent, 'Project totals');
  assert.match(preview.textContent, /Database: Tasks/);
  assert.equal(preview.querySelectorAll('input').length + preview.querySelectorAll('textarea').length, 0);
  assert.equal(preview.querySelectorAll('[contenteditable]').length, 0);
});

test('rollback needs inline confirmation, posts the revision and reloads the list', async () => {
  const body = { revision: 4, revision_pending: false };
  const { host, fetch, events, history } = setup([
    { json: { revisions: [REV3, REV2] } },
    { json: body },
    { json: { revisions: [{ ...REV1, number: 4, kind: 'rollback' }, REV3, REV2] } },
  ]);
  history.toggle(true);
  await settle();
  button(host, 'Roll back to revision 2').click();
  await settle();
  assert.equal(fetch.calls.length, 1, 'nothing is sent before Confirm');
  const confirm = rowOf(host, 2).querySelector('.history-confirm');
  assert.match(confirm.textContent, /whole notebook back to revision 2/);
  assert.match(confirm.textContent, /new revision/);
  assert.match(confirm.textContent, /undo/);
  button(host, 'Confirm').click();
  await settle();
  assert.equal(fetch.calls[1].url, '/_notebook/api/rollback');
  assert.equal(fetch.calls[1].init.headers['X-Flats-Notebook'], '1');
  assert.deepEqual(fetch.calls[1].body, { revision: 2 });
  assert.deepEqual(events.rolledBack, [body]);
  assert.equal(fetch.calls[2].url, '/_notebook/api/revisions?limit=50');
  assert.match(rowOf(host, 4).textContent, /Rollback/);
  assert.match(host.querySelector('[role=status]').textContent, /as revision 4/);
  assert.doesNotMatch(host.querySelector('[role=status]').textContent, /still being saved/);
});

test('pin toggles post revision and pinned and update the row', async () => {
  const { host, fetch, history } = setup([
    { json: { revisions: [REV3] } },
    { json: { status: 'saved', revision: 3, pinned: true } },
    { json: { status: 'saved', revision: 3, pinned: false } },
  ]);
  history.toggle(true);
  await settle();
  button(host, 'Pin revision 3').click();
  await settle();
  assert.equal(fetch.calls[1].url, '/_notebook/api/pin');
  assert.equal(fetch.calls[1].init.headers['X-Flats-Notebook'], '1');
  assert.deepEqual(fetch.calls[1].body, { revision: 3, pinned: true });
  assert.equal(button(host, 'Unpin revision 3').getAttribute('aria-pressed'), 'true');
  assert.match(rowOf(host, 3).textContent, /Pinned/);
  button(host, 'Unpin revision 3').click();
  await settle();
  assert.deepEqual(fetch.calls[2].body, { revision: 3, pinned: false });
  assert.ok(button(host, 'Pin revision 3'));
});

test('a pin refusal shows the quota detail in an alert', async () => {
  const { host, history } = setup([
    { json: { revisions: [REV3] } },
    { status: 413, json: { code: 'notebook_capacity', reason: 'disk_quota', retry: false, detail: 'Free storage before adding another pin.' } },
  ]);
  history.toggle(true);
  await settle();
  button(host, 'Pin revision 3').click();
  await settle();
  assert.match(host.querySelector('[role=alert]').textContent, /quota.*Free storage before adding another pin\./);
  assert.ok(button(host, 'Pin revision 3'), 'pin state is unchanged');
});

test('hostile titles and messages render as literal text', async () => {
  const evil = '<img src=x onerror=alert(1)>';
  const { host, history } = setup([
    { json: { revisions: [{ ...REV3, message: evil, authors: [evil], pages: [{ page: 'p_x', title: evil, change: 'changed' }] }] } },
    { json: { revision: 3, tree: [{ id: 'p_x', kind: 'page', title: evil, parent: null, position: 'a' }], next: null, complete: true, warnings: [] } },
    { json: { revision: 3, page: [{ id: 'p_x', kind: 'page', title: evil, parent: null, position: 'a' }, { id: 'b', kind: 'block', page: 'p_x', parent: null, position: 'a', content: { kind: 'paragraph', text: [{ text: evil, marks: [] }] } }], next: null, complete: true, warnings: [] } },
  ]);
  history.toggle(true);
  await settle();
  assert.ok(host.textContent.includes(evil));
  button(host, 'Open revision 3').click();
  await settle();
  button(pageRow(host, 'p_x'), `Open page ${evil}`).click();
  await settle();
  assert.ok(host.querySelector('.history-preview').textContent.includes(evil));
  assert.equal(host.querySelectorAll('img').length, 0);
});

test('Escape closes the panel and calls onClose', async () => {
  const { host, events, history } = setup([{ json: { revisions: [] } }]);
  history.toggle();
  await settle();
  assert.match(host.textContent, /No revisions yet/);
  host.querySelector('h2').dispatch('keydown', { key: 'Escape' });
  assert.equal(history.open, false);
  assert.equal(host.hidden, true);
  assert.equal(events.closed, 1);
});

test('restoring an existing page sends the same recover contract and reports its new root', async () => {
  const result = { refs: { root: { ref: { id: 'p_copy', inc: '7' }, ver: '7' } } };
  const { host, fetch, events, history } = setup([
    { json: { revisions: [REV2] } }, { json: TREE }, { json: result },
  ]);
  history.toggle(true);
  await settle();
  button(host, 'Open revision 2').click();
  await settle();
  button(host, 'Restore as copy: Plan').click();
  button(host, 'Confirm').click();
  await settle();
  assert.deepEqual(fetch.calls[2].body, { revision: 2, page: 'p_plan', session: 'sess-1' });
  assert.deepEqual(events.recovered[0].root, { id: 'p_copy', inc: '7' });
  assert.match(host.querySelector('[role=status]').textContent, /Restored a copy/);
});

test('an invalid successful recover response cannot announce a completed restore', async () => {
  const { host, events, history } = setup([
    { json: { revisions: [REV2] } }, { json: TREE }, { json: null },
  ]);
  history.toggle(true);
  await settle();
  button(host, 'Open revision 2').click();
  await settle();
  button(host, 'Restore page: Gone').click();
  button(host, 'Confirm').click();
  await settle();
  assert.equal(events.recovered.length, 0);
  assert.match(host.querySelector('[role=alert]').textContent, /invalid response/);
  assert.equal(host.querySelector('[role=status]').textContent, '');
});

test('null revision lists are empty, but a missing list is an error', async () => {
  const { host, history } = setup([{ json: { revisions: null } }, { json: {} }]);
  history.toggle(true);
  await settle();
  assert.match(host.textContent, /No revisions yet/);
  history.toggle(false);
  history.toggle(true);
  await settle();
  assert.match(host.querySelector('[role=alert]').textContent, /invalid revision list/);
  assert.doesNotMatch(host.textContent, /No revisions yet/);
});

function deferred() {
  let resolve;
  const promise = new Promise((r) => { resolve = r; });
  return { promise, resolve };
}

test('focus works with a browser NodeList rather than an Array', async () => {
  const { host, history } = setup([{ json: { revisions: [REV2], next: 0 } }]);
  const query = host.querySelectorAll.bind(host);
  host.querySelectorAll = (selector) => {
    const nodes = query(selector);
    return { length: nodes.length, [Symbol.iterator]: () => nodes[Symbol.iterator]() };
  };
  history.toggle(true);
  await settle();
  assert.equal(document.activeElement.getAttribute('aria-label'), 'Close history');
  assert.ok(host.textContent.includes('Revision 2'));
  assert.equal(query('button').some((b) => b.textContent === 'Load more'), false);
});

test('a failed initial list can be retried in place', async () => {
  const { host, history } = setup([
    { status: 503, json: { code: 'unavailable', reason: 'history', retry: true } },
    { json: { revisions: [REV2] } },
  ]);
  history.toggle(true);
  await settle();
  assert.match(host.querySelector('[role=alert]').textContent, /temporarily unavailable/);
  button(host, 'Retry').click();
  await settle();
  assert.ok(rowOf(host, 2));
  assert.equal(host.querySelector('[role=alert]'), null);
});

test('Escape cancels inline rollback and returns focus without sending a write', async () => {
  const { host, fetch, history } = setup([{ json: { revisions: [REV2] } }]);
  history.toggle(true);
  await settle();
  button(host, 'Roll back to revision 2').click();
  assert.equal(document.activeElement.textContent, 'Confirm');
  document.activeElement.dispatch('keydown', { key: 'Escape' });
  assert.equal(history.open, true);
  assert.equal(button(host, 'Confirm'), null);
  assert.equal(document.activeElement.getAttribute('aria-label'), 'Roll back to revision 2');
  assert.equal(fetch.calls.length, 1);
});

test('recover is sent once while pending, and failure does not announce success', async () => {
  const pending = deferred();
  const { host, fetch, events, history } = setup([
    { json: { revisions: [REV2] } }, { json: TREE }, () => pending.promise,
  ]);
  history.toggle(true);
  await settle();
  button(host, 'Open revision 2').click();
  await settle();
  button(pageRow(host, 'p_gone'), 'Restore page').click();
  const confirm = button(host, 'Confirm');
  confirm.click();
  confirm.click();
  assert.equal(fetch.calls.length, 3);
  assert.equal(button(host, 'Confirm').disabled, true);
  pending.resolve({ status: 413, json: { code: 'notebook_capacity', reason: 'disk_quota', retry: false } });
  await settle();
  assert.equal(events.recovered.length, 0);
  assert.equal(host.querySelector('[role=status]').textContent, '');
  assert.match(host.querySelector('[role=alert]').textContent, /quota exceeded/);
  assert.equal(button(pageRow(host, 'p_gone'), 'Restore page').disabled, false);
});

test('rollback failure leaves the current view intact and never invokes its success hook', async () => {
  const { host, fetch, events, history } = setup([
    { json: { revisions: [REV2] } }, { json: TREE },
    { status: 404, json: { code: 'not_found', reason: 'revision', retry: false } },
  ]);
  history.toggle(true);
  await settle();
  button(host, 'Open revision 2').click();
  await settle();
  button(host, 'Roll back to revision 2').click();
  button(host, 'Confirm').click();
  await settle();
  assert.equal(events.rolledBack.length, 0);
  assert.equal(fetch.calls.length, 3);
  assert.ok(host.querySelector('.history-revision-view'));
  assert.match(host.querySelector('[role=alert]').textContent, /no longer retained/);
  assert.equal(button(host, 'Roll back to revision 2').disabled, false);
});

test('closing stops obsolete historical pagination and reopening ignores its response', async () => {
  const pending = deferred();
  const { host, fetch, history } = setup([
    { json: { revisions: [REV2] } }, () => pending.promise,
    { json: { revisions: [REV3] } },
  ]);
  history.toggle(true);
  await settle();
  button(host, 'Open revision 2').click();
  button(host, 'Close history').click();
  history.toggle(true);
  await settle();
  pending.resolve({ json: { ...TREE, complete: false, next: 'obsolete' } });
  await settle();
  assert.equal(fetch.calls.length, 3, 'no request for the obsolete cursor');
  assert.ok(rowOf(host, 3));
  assert.equal(host.querySelector('.history-revision-view'), null);
});

test('a superseded page preview cannot overwrite the newer page or follow an old cursor', async () => {
  const pending = deferred();
  const { host, fetch, history } = setup([
    { json: { revisions: [REV2] } }, { json: TREE }, () => pending.promise,
    { json: { page: [{ ref: { id: 'b_gone', inc: '1' }, kind: 'block', page: 'p_gone', parent: null, position: 'a', content: { kind: 'paragraph', text: [{ text: 'The newer preview', marks: [] }] } }], complete: true } },
  ]);
  history.toggle(true);
  await settle();
  button(host, 'Open revision 2').click();
  await settle();
  button(host, 'Open page Plan').click();
  button(host, 'Open page Gone').click();
  await settle();
  pending.resolve({ json: { page: [], complete: false, next: 'obsolete' } });
  await settle();
  assert.equal(fetch.calls.length, 4);
  assert.match(host.querySelector('.history-preview').textContent, /The newer preview/);
});

test('a repeated content cursor reports a failed read rather than an empty page', async () => {
  const { host, fetch, history } = setup([
    { json: { revisions: [REV2] } }, { json: TREE },
    { json: { page: [], complete: false, next: 'loop' } },
    { json: { page: [], complete: false, next: 'loop' } },
  ]);
  history.toggle(true);
  await settle();
  button(host, 'Open revision 2').click();
  await settle();
  button(host, 'Open page Plan').click();
  await settle();
  assert.equal(fetch.calls.length, 4);
  assert.match(host.querySelector('[role=alert]').textContent, /could not be read to the end/);
  assert.doesNotMatch(host.querySelector('.history-preview').textContent, /This page is empty/);
});

const historical = (id, kind, fields) => ({ ref: { id, inc: '1' }, ver: '2', kind, ...fields });
const TASKS = historical('d_tasks', 'database', { block: 'b_tasks', title: 'Tasks', property_order: ['pr_title', 'pr_status', 'pr_done', 'pr_count'], view_order: ['v_tasks'] });
const DATABASE_PAGE = { page: { items: [
  historical('b_tasks', 'block', { page: 'p_plan', parent: null, position: 'a', content: { kind: 'database', database: TASKS.ref.id } }), TASKS,
] }, complete: true, warnings: [] };
const DATABASE_SCHEMA = [
  TASKS,
  historical('pr_status', 'property', { database: 'd_tasks', definition: { name: 'Old status', type: 'select' }, option_order: ['o_done'] }),
  historical('o_done', 'option', { property: 'pr_status', name: 'Previously done', color: 'green' }),
  historical('pr_title', 'property', { database: 'd_tasks', definition: { name: 'Old title', type: 'title' }, option_order: [] }),
  historical('pr_done', 'property', { database: 'd_tasks', definition: { name: 'Complete', type: 'checkbox' }, option_order: [] }),
  historical('pr_count', 'property', { database: 'd_tasks', definition: { name: 'Count', type: 'number', format: 'comma' }, option_order: [] }),
  historical('v_tasks', 'view', { database: 'd_tasks', definition: { name: 'Old board', display: { layout: 'kanban', group_by: { id: 'pr_status', inc: '1' }, visible: [] }, sort: [], filter: [] } }),
];
const oldRow = (id, text, position) => historical(id, 'row', { database: 'd_tasks', position, cells: {
  pr_title: { type: 'title', value: text }, pr_status: { type: 'select', value: 'o_done' },
  pr_done: { type: 'checkbox', value: true }, pr_count: { type: 'number', value: '1234.5' },
} });

async function openDatabasePage(history, host) {
  history.toggle(true);
  await settle();
  button(host, 'Open revision 2').click();
  await settle();
  button(host, 'Open page Plan').click();
  await settle();
}

test('historical database inspection reads every schema and row page without current data or writes', async () => {
  const deleted = oldRow('r_deleted', 'A row deleted since this revision <script>bad()</script>', 'b');
  const encoded = JSON.stringify(deleted);
  const { host, fetch, history } = setup([
    { json: { revisions: [REV2] } }, { json: TREE }, { json: DATABASE_PAGE },
    { json: { revision: 2, schema: DATABASE_SCHEMA.slice(0, 4), rows: [], complete: false, next: 'schema&rows' } },
    { json: { revision: 2, schema: DATABASE_SCHEMA.slice(4), rows: [oldRow('r_changed', 'Value before the current edit', 'a')], complete: false, next: 'row-part', continuation: { record: 'r_deleted', part: 0, parts: 2, data: encoded.slice(0, 80) } } },
    { json: { revision: 2, schema: [], rows: [], complete: true, continuation: { record: 'r_deleted', part: 1, parts: 2, data: encoded.slice(80) }, warnings: [{ code: 'retained', message: 'Retained historical values.' }] } },
  ]);
  await openDatabasePage(history, host);
  const open = button(host, 'Open database Tasks');
  assert.ok(open, 'database blocks expose an inspection control');
  open.click();
  assert.equal(document.activeElement.getAttribute('data-key'), 'database-preview');
  assert.equal(host.querySelector('.history-database').getAttribute('aria-busy'), 'true');
  assert.equal(host.querySelector('.history-database table'), null);
  await settle();
  assert.deepEqual(fetch.calls.slice(3).map((c) => c.url), [
    '/_notebook/api/revisions/2?database=d_tasks',
    '/_notebook/api/revisions/2?cursor=schema%26rows',
    '/_notebook/api/revisions/2?cursor=row-part',
  ]);
  const db = host.querySelector('.history-database');
  assert.match(db.textContent, /As of revision 2, read-only/);
  assert.match(db.textContent, /Old board/);
  assert.match(db.textContent, /Retained historical values/);
  assert.deepEqual(db.querySelectorAll('th').map((n) => n.textContent), ['Old title', 'Old status', 'Complete', 'Count']);
  assert.deepEqual(db.querySelectorAll('td').map((n) => n.textContent), [
    'Value before the current edit', 'Previously done', 'Checked', '1,234.5',
    deleted.cells.pr_title.value, 'Previously done', 'Checked', '1,234.5',
  ]);
  assert.equal(db.querySelectorAll('script').length, 0);
  assert.equal(db.querySelectorAll('input').length + db.querySelectorAll('textarea').length + db.querySelectorAll('[contenteditable]').length, 0);
  assert.ok(fetch.calls.every((c) => c.init.method === 'GET'));
  assert.equal(fetch.queue.length, 0);
});

test('an expired recovery session is explained without reporting a restored page', async () => {
  const { host, history, events, fetch } = setup([
    { json: { revisions: [REV2] } }, { json: TREE },
    { status: 409, json: { code: 'conflict', reason: 'session_expired', retry: false, detail: 'session expired; preserve pending edits' } },
  ]);
  history.toggle(true);
  await settle();
  button(host, 'Open revision 2').click();
  await settle();
  button(host, 'Restore page: Gone').click();
  button(host, 'Confirm').click();
  await settle();
  assert.match(host.querySelector('[role=alert]').textContent, /session.*Reload.*preserve pending edits/);
  assert.equal(host.querySelector('[role=status]').textContent, '');
  assert.equal(events.recovered.length, 0);
  assert.equal(fetch.calls.length, 3);
});

test('leaving a page cancels its historical database pagination', async () => {
  const pending = deferred();
  const { host, fetch, history } = setup([
    { json: { revisions: [REV2] } }, { json: TREE }, { json: DATABASE_PAGE }, () => pending.promise,
    { json: { page: { items: [] }, complete: true } },
  ]);
  await openDatabasePage(history, host);
  const open = button(host, 'Open database Tasks');
  assert.ok(open);
  open.click();
  button(host, 'Open page Gone').click();
  await settle();
  pending.resolve({ json: { schema: DATABASE_SCHEMA, rows: [], complete: false, next: 'obsolete' } });
  await settle();
  assert.equal(fetch.calls.length, 5);
  assert.equal(host.querySelector('.history-database'), null);
  assert.equal(host.querySelector('.history-preview h3').textContent, 'Gone');
});

test('failed historical database reads never present a partial capture as an empty database', async () => {
  const { host, history } = setup([
    { json: { revisions: [REV2] } }, { json: TREE }, { json: DATABASE_PAGE },
    { json: { schema: DATABASE_SCHEMA, rows: [], complete: false, next: 'expired' } },
    { status: 409, json: { code: 'conflict', reason: 'read_expired', retry: false, detail: 'cursor expired; read again' } },
  ]);
  await openDatabasePage(history, host);
  const open = button(host, 'Open database Tasks');
  assert.ok(open);
  open.click();
  await settle();
  assert.match(host.querySelector('[role=alert]').textContent, /expired.*[Oo]pen.*again/);
  assert.equal(host.querySelector('.history-database table'), null);
  assert.doesNotMatch(host.querySelector('.history-database').textContent, /no rows/i);
});

test('rollback qualifies an import still pending without inventing a retained revision', async () => {
  const body = { revision: 0, revision_pending: true };
  const { host, history, events } = setup([
    { json: { revisions: [REV2] } }, { json: body }, { json: { revisions: [REV2] } },
  ]);
  history.toggle(true);
  await settle();
  button(host, 'Roll back to revision 2').click();
  button(host, 'Confirm').click();
  await settle();
  const status = host.querySelector('[role=status]').textContent;
  assert.match(status, /Restored the notebook to revision 2/);
  assert.match(status, /new revision is still being saved/);
  assert.doesNotMatch(status, /as revision/);
  assert.deepEqual(events.rolledBack, [body]);
});

test('Go-shaped History errors explain quota, missing revisions, sessions and retry policy', async (t) => {
  for (const [name, status, json, expected] of [
    ['quota', 413, { code: 'notebook_capacity', reason: 'disk_quota', retry: false }, /quota exceeded.*[Ff]ree storage/],
    ['revision', 404, { code: 'not_found', reason: 'revision', retry: false }, /revision is no longer retained/],
    ['session', 409, { code: 'conflict', reason: 'session_expired', retry: false, detail: 'session expired; preserve pending edits' }, /session.*[Rr]eload.*preserve pending edits/],
    ['retryable', 503, { code: 'unavailable', reason: 'history', retry: true }, /temporarily unavailable.*[Tt]ry again/],
    ['not retryable', 503, { code: 'unavailable', reason: 'history', retry: false }, /temporarily unavailable/],
    ['origin', 403, { code: 'forbidden', reason: 'origin', retry: false }, /[Pp]rivate.*origin/],
    ['unknown', 409, { code: 'future_refusal', reason: 'new_reason', retry: false, detail: '<b>Useful detail</b>' }, /future_refusal.*new_reason.*<b>Useful detail<\/b>/],
  ]) {
    await t.test(name, async () => {
      const { host, history, fetch } = setup([{ status, json }]);
      history.toggle(true);
      await settle();
      assert.match(host.querySelector('[role=alert]').textContent, expected);
      if (!json.retry) assert.doesNotMatch(host.querySelector('[role=alert]').textContent, /[Tt]ry again/);
      assert.equal(host.querySelectorAll('b').length, 0);
      assert.equal(fetch.calls.length, 1, 'no automatic retries');
    });
  }
});
