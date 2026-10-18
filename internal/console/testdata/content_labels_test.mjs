// Reuse the console's DOM harness and run its regression suite before these
// focused cross-content-model checks. The Go runner copies both scripts.
import './ui_test.mjs';
import assert from 'node:assert/strict';
import { mount as analytics } from './analytics.js';
import { mount as settings } from './settings.js';

const ctx = { alive: () => true, setTitle() {} };
const tick = () => new Promise((resolve) => setTimeout(resolve, 20));
const all = (root, pred) => [root, ...(root.childNodes || []).flatMap((n) => all(n, () => true))].filter(pred);
const element = (root, tag, text) => all(root, (n) => n.tagName === tag && n.textContent === text)[0];
const calls = [];
let flat, views = 0;
const values = { upload_max_bytes: '20971520', version_quota: '20', disk_quota_bytes: '32212254720', preview_ttl_seconds: '86400', rate_limit_rps: '50', redirect_days: '7', events_keep: '5000', revision_quota: '20', pin_quota: '5' };
globalThis.fetch = async (url, opts = {}) => {
  calls.push([url, opts]);
  let body;
  if (url.includes('/stats?')) body = { page_views: [{ day: new Date().toISOString().slice(0, 10), count: views }] };
  else if (url === '/console/api/flats/sample') body = flat;
  else if (url === '/console/api/settings' && opts.method === 'PUT') {
    Object.assign(values, JSON.parse(opts.body));
    body = { applied: Object.keys(JSON.parse(opts.body)) };
  } else if (url === '/console/api/settings') body = { settings: values, defaults: values };
  else if (url === '/console/api/console-domains') body = { providers: {} };
  else body = {};
  return { ok: true, json: async () => body };
};
for (const type of ['flat', 'page', 'docs', 'design', undefined, 'future']) {
  for (views of [0, 7]) {
    flat = { slug: 'sample', name: 'Sample', type, disk_bytes: 1024, versions: 9, visibility: 'private', endpoints: [] };
    const main = document.createElement('main');
    analytics(main, ['sample'], ctx);
    await tick();
    assert.equal(element(main, 'DT', 'Unique Visitors'), undefined);
    assert.ok(element(main, 'DT', 'Page Views'));
    assert.ok(main.textContent.includes('Unique visitors are not tracked.'));
    assert.equal(!!element(main, 'DT', 'Versions'), !['docs', 'design'].includes(type));
    assert.equal(main.textContent.includes('(versions, data and previews)'), !['docs', 'design'].includes(type));
    if (['docs', 'design'].includes(type)) assert.ok(element(main, 'DD', '1.0 KB'));
    assert.equal(main.textContent.includes('No page views in this period.'), views === 0);
    element(main, 'BUTTON', '7d').dispatch('click');
    await tick();
    assert.ok(calls.at(-1)[0].endsWith('/stats?days=7'));
    element(main, 'BUTTON', '30d').dispatch('click');
    await tick();
    assert.ok(calls.at(-1)[0].endsWith('/stats?days=30'));
  }
}
const main = document.createElement('main');
settings(main, ['limits'], ctx);
await tick();
assert.ok(element(main, 'LABEL', 'Revisions'));
assert.ok(element(main, 'LABEL', 'Pinned revisions'));
assert.ok(main.textContent.includes('each document or design keeps'));
assert.ok(main.textContent.includes('Pinned revisions are never deleted automatically'));
assert.ok(main.textContent.includes('For websites and pages.'));
assert.ok(element(main, 'LABEL', 'Versions'));
assert.equal(element(main, 'LABEL', 'Versions to keep'), undefined);
assert.ok(main.textContent.includes('Never deleted automatically'));
assert.ok(main.textContent.includes('0 is unlimited.'));
const input = (id) => all(main, (n) => n.getAttribute?.('id') === id)[0];
const form = input('limits-form');
input('set-pin_quota').value = '-1';
form.dispatch('submit');
assert.ok(main.textContent.includes('Pinned revisions must be a whole number of 0 or more.'));
input('set-pin_quota').value = '6';
input('set-revision_quota').value = '21';
form.dispatch('submit');
await tick();
await tick();
assert.ok(main.textContent.includes('Revisions: saved, applied now.'));
assert.ok(main.textContent.includes('Pinned revisions: saved, applied now.'));
console.log('content labels ok');
