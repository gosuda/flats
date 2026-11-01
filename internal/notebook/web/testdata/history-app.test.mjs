'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { startApp, page, block, para, pid, bid, lastAction } from './page.mjs';

const button = (host, label) => host.querySelectorAll('button').find((b) => b.textContent === label);
const settled = () => new Promise((resolve) => setImmediate(resolve));

for (const exists of [false, true]) test(`History ${exists ? 'copies an existing' : 'restores a deleted'} page through the live App session`, async () => {
  const requests = [];
  const historical = page('old', { title: 'Earlier' });
  const root = { id: exists ? pid('copy') : historical.ref.id, inc: '20' };
  const t = await startApp({
    pages: [page('a'), ...(exists ? [historical] : [])],
    fetch: async (url, init) => {
      requests.push({ url, init });
      const data = init.method === 'POST' ? { root, revision: 0, revision_pending: true }
        : url.includes('?limit=') ? { revisions: [] }
          : { tree: [historical], complete: true };
      return { ok: true, json: async () => data };
    },
  });
  t.app.historyBtn.click();
  await settled();
  await t.app.history._openRevision(2);
  assert.ok(button(t.app.historyEl, exists ? 'Restore as copy' : 'Restore page'));
  button(t.app.historyEl, exists ? 'Restore as copy' : 'Restore page').click();
  assert.match(t.app.historyEl.textContent, exists ? /as a new page/ : /under its original parent/);
  // Read the session at submission, not the token present at construction.
  t.app.net.session = 'current-token';
  await t.app.history._confirmRestore();
  const post = requests.find((r) => r.init.method === 'POST');
  assert.ok(post, t.app.historyEl.textContent);
  assert.equal(post.url, '/_notebook/api/recover');
  assert.deepEqual(JSON.parse(post.init.body), { revision: 2, page: historical.ref.id, session: 'current-token' });
  assert.equal(location.hash, '#/' + root.id);
  t.socket().recv({ type: 'delta', generation: 0, seq: '11', tree: [{ ...historical, ref: root, ver: '20' }], changes: [] });
  assert.equal(t.app.page, root.id);
  assert.equal(t.app.titleEl.textContent, 'Earlier');
  assert.ok(t.app.history._existsNow(root.id), 'lookup follows canonical recovery');
});

test('Private overwrite attribution recovers retained text into a new block', async () => {
  const t = await startApp({ pages: [page('a')], blocks: [block('x', 'a', para('base'))] });
  const source = t.app.queue([{ op: 'set_block', block: { id: bid('x'), inc: '5' }, base_v: '5', content: para('mine') }]);
  t.socket().recv({ type: 'delta', seq: '11', changes: [block('x', 'a', para('mine'))], from: { session_id: 'me', n: source.n } });
  t.socket().recv({ type: 'ack', n: source.n, seq: '11', outcomes: [] });
  t.socket().recv({ type: 'delta', seq: '12', changes: [block('x', 'a', para('theirs'))], overwrote: [{ block: { id: bid('x'), inc: '5' }, version: '11', base: '5', source: { session_id: 'other', n: source.n } }] });
  assert.equal(t.app.notices.length, 0);
  t.socket().recv({ type: 'delta', seq: '13', changes: [], overwrote: [{ block: { id: bid('x'), inc: '5' }, version: '11', base: '5', source: { session_id: 'me', n: source.n } }] });
  assert.equal(t.app.notices.length, 1);
  assert.equal(t.app.editableOf(bid('x')).textContent, 'theirs');
  button(t.app.banners, 'Keep my text').click();
  const ops = lastAction(t.socket).ops;
  assert.equal(ops.length, 1);
  assert.equal(ops[0].op, 'create_block');
  assert.deepEqual(ops[0].content, para('mine'));
});
