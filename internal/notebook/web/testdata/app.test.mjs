// App-level tests: the real editor on the fake DOM, with the test playing
// the server over the fake socket.
'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { startApp, page, block, para, run, pid, bid, caretAt, lastAction, queued } from './page.mjs';
import { fire, htmlWrites, textNodes } from './dom.mjs';

const twoPages = {
  pages: [page('a', { title: 'Home', position: 'A' }), page('b', { title: 'Plans', position: 'B' }), page('c', { title: 'Child', parent: 'b' })],
  blocks: [block('x', 'a', { kind: 'paragraph', text: [run('hello', 'bold'), run(' world')] }, { position: 'A' }), block('y', 'a', para('second'), { position: 'B' })],
};

const beforeinput = (el, inputType, data, extra = {}) => fire(el, 'beforeinput', { inputType, data, getTargetRanges: () => [], ...extra });

test('opening without a page id shows the first top-level page', async () => {
  const { app } = await startApp(twoPages);
  assert.equal(location.hash, '#/' + pid('a'));
  assert.equal(app.page, pid('a'));
  assert.equal(app.titleEl.textContent, 'Home');
});

test('a hash route opens that page, with breadcrumb and watch', async () => {
  const { app, socket } = await startApp({ ...twoPages, hash: '#/' + pid('c') });
  assert.equal(app.page, pid('c'));
  assert.equal(app.crumbs.textContent, 'Plans/Child');
  assert.equal(socket().sent[0].page, pid('c'), 'hello names the selected page');
  location.hash = '#/' + pid('a');
  assert.equal(app.page, pid('a'));
  assert.deepEqual(socket().frames('watch').at(-1), { type: 'watch', generation: 1, page: pid('a'), visible_databases: [], pending_scopes: [] });
});

test('an unknown page id says so and links to the first page', async () => {
  const { app } = await startApp({ ...twoPages, hash: '#/' + pid('zz') });
  assert.equal(app.titleEl.textContent, 'This page does not exist');
  const link = app.subpages.querySelector('a');
  assert.equal(link.getAttribute('href'), '#/' + pid('a'));
  assert.ok(app.subpages.textContent.includes('History'), 'writers get a History link');
});

test('typing goes through beforeinput as code point edits and flushes within 200 ms', async () => {
  const { app, socket, timers } = await startApp(twoPages);
  const el = caretAt(app, 'x', 5);
  beforeinput(el, 'insertText', '😀');
  beforeinput(el, 'insertText', '!');
  assert.equal(socket().frames('action').length, 0, 'held for the flush window');
  assert.equal(el.textContent, 'hello😀! world', 'shown at once');
  timers.advance(200);
  const a = lastAction(socket);
  assert.equal(a.ops.length, 1);
  assert.deepEqual(a.ops[0], { op: 'set_block', block: { id: bid('x'), inc: '5' }, content: { kind: 'paragraph', text: [run('hello😀!', 'bold'), run(' world')] }, base_v: '5' });
  assert.deepEqual(a.typing_block, { id: bid('x'), inc: '5' });
  assert.equal(app.statusEl.textContent, 'Saving…');
  const s = getSelection();
  assert.equal(s.anchorNode.data, 'hello😀!');
  assert.equal(s.anchorOffset, 8, 'caret after the emoji pair and !');
});

test('Saved shows only after every action is acknowledged', async () => {
  const { app, socket, timers } = await startApp(twoPages);
  beforeinput(caretAt(app, 'x', 0), 'insertText', 'A');
  timers.advance(200);
  assert.equal(app.statusEl.textContent, 'Saving…');
  socket().recv({ t: 'ack', n: '1', seq: '11', outcomes: [{ indexes: [0], status: 'applied' }], summary: { applied: 1, ignored: 0, refused: 0, copied_roots: [], complete: true }, results: [], results_sample: { total: 0, complete: true } });
  assert.equal(app.statusEl.textContent, 'Saved');
  socket().drop();
  assert.equal(app.statusEl.textContent, 'Disconnected');
});

test('Enter splits the block as one unit and focuses the new block', async () => {
  const { app, socket } = await startApp(twoPages);
  const el = caretAt(app, 'x', 2);
  fire(el, 'keydown', { key: 'Enter' });
  const a = lastAction(socket);
  assert.deepEqual(a.ops.map((o) => o.op), ['set_block', 'create_block']);
  assert.equal(a.ops[0].unit_id, a.ops[1].unit_id);
  assert.deepEqual(a.ops[0].content.text, [run('he', 'bold')]);
  assert.deepEqual(a.ops[1].content.text, [run('llo', 'bold'), run(' world')]);
  assert.deepEqual(a.ops[1].place.after, { id: bid('x'), inc: '5' });
  const order = [...app.pageEl.querySelectorAll('.blk')].map((b) => b.textContent);
  assert.deepEqual(order, ['he', 'llo world', 'second']);
  assert.equal(document.activeElement, app.editableOf(a.ops[1].proposed_id));
});

test('Backspace at a block start merges into the previous block', async () => {
  const { app, socket } = await startApp(twoPages);
  const el = caretAt(app, 'y', 0);
  fire(el, 'keydown', { key: 'Backspace' });
  const a = lastAction(socket);
  assert.deepEqual(a.ops.map((o) => o.op), ['set_block', 'delete_blocks']);
  assert.deepEqual(a.ops[0].content.text, [run('hello', 'bold'), run(' worldsecond')]);
  assert.deepEqual(a.ops[0].structure, [{ block: { id: bid('y'), inc: '5' }, page: { id: pid('a'), inc: '5' }, parent: null, after: { id: bid('x'), inc: '5' } }]);
  const s = getSelection();
  assert.equal(document.activeElement, app.editableOf(bid('x')));
  assert.equal(s.anchorNode.data, ' worldsecond');
  assert.equal(s.anchorOffset, 6);
});

test('a failed unit discards its optimistic change and offers Keep my text', async () => {
  const { app, socket } = await startApp(twoPages);
  fire(caretAt(app, 'x', 5), 'keydown', { key: 'Enter' });
  const a = lastAction(socket);
  assert.equal(app.pageEl.querySelectorAll('.blk').length, 3);
  socket().recv({ t: 'ack', n: a.n, seq: '11', outcomes: [{ indexes: [0, 1], status: 'refused', reason: 'structure', failed_index: 0 }], summary: { applied: 0, ignored: 0, refused: 2, copied_roots: [], complete: true }, results: [], results_sample: { total: 0, complete: true } });
  assert.deepEqual([...app.pageEl.querySelectorAll('.blk')].map((b) => b.textContent), ['hello world', 'second']);
  const notices = app.banners.querySelectorAll('[data-notice]');
  assert.equal(notices.length, 2, 'both halves of the split are offered');
  assert.ok(notices[0].textContent.includes('hello') && notices[1].textContent.includes(' world'));
  const keep = [...notices[1].querySelectorAll('button')].find((b) => b.textContent === 'Keep my text');
  keep.click();
  const k = lastAction(socket);
  assert.equal(k.ops[0].op, 'create_block');
  assert.deepEqual(k.ops[0].place.after, { id: bid('x'), inc: '5' }, 'after the original, never into the contested group');
  assert.deepEqual(k.ops[0].content.text, [run(' world')]);
  assert.equal(app.banners.querySelectorAll('[data-notice]').length, 1);
});

test('an overwrite of this tab\'s text names the other writer and keeps the text', async () => {
  const { app, socket, timers } = await startApp(twoPages);
  beforeinput(caretAt(app, 'y', 6), 'insertText', '!');
  timers.advance(200);
  socket().recv({ t: 'delta', generation: 0, seq: '11', tree: [], changes: [block('y', 'a', para('second!'), { position: 'B', v: { content: '11' } })], from: { session_id: 'me', n: '1' } });
  socket().recv({ t: 'ack', n: '1', seq: '11', outcomes: [{ indexes: [0], status: 'applied' }], summary: { applied: 1, ignored: 0, refused: 0, copied_roots: [], complete: true }, results: [], results_sample: { total: 0, complete: true } });
  socket().recv({ t: 'delta', generation: 0, seq: '12', tree: [], changes: [block('y', 'a', para('theirs'), { position: 'B', v: { content: '12' } })], overwrote: [{ block: { id: bid('y'), inc: '5' }, version: '11', source: { session_id: 'me', n: '1' }, by: { label: 'Minji' } }] });
  assert.equal(app.editableOf(bid('y')).textContent, 'theirs');
  const notice = app.banners.querySelector('[data-notice]');
  assert.ok(notice.textContent.includes('Minji changed this text after you.'), notice.textContent);
  assert.ok(notice.textContent.includes('second!'));
});

test('composition defers remote changes to the composing block and submits the result once', async () => {
  const { app, socket, timers } = await startApp(twoPages);
  const el = caretAt(app, 'y', 6);
  fire(el, 'compositionstart');
  // the browser composes into the DOM
  el.firstChild.data += '한';
  fire(el, 'beforeinput', { inputType: 'insertCompositionText', data: '한', isComposing: true, getTargetRanges: () => [] });
  fire(el, 'input', { isComposing: true });
  const node = app.pageView.entryOf(bid('y')).el;
  // a remote content change, then a move of the composing block
  socket().recv({ t: 'delta', generation: 0, seq: '11', tree: [], changes: [block('y', 'a', para('remote'), { position: 'B', v: { content: '11' } })] });
  socket().recv({ t: 'delta', generation: 0, seq: '12', tree: [], changes: [block('y', 'a', para('remote'), { position: '0', v: { content: '11', place: '12' } })] });
  assert.equal(el.textContent, 'second한', 'the composing node is not patched');
  assert.equal(app.pageView.entryOf(bid('y')).el, node);
  assert.equal(node.nextSibling, null, 'nor moved');
  timers.advance(1000);
  assert.equal(socket().frames('action').length, 0, 'nothing is sent while composing');
  fire(el, 'compositionend', { data: '한' });
  fire(el, 'input', {});
  timers.advance(200);
  const actions = socket().frames('action');
  assert.equal(actions.length, 1, 'the duplicate final input is not submitted again');
  assert.deepEqual(actions[0].ops[0].content, para('second한'), 'the complete local content with the captured kind');
  assert.equal(actions[0].ops[0].base_v, '5', 'the base version captured at compositionstart');
  const order = [...app.pageEl.querySelectorAll('.blk')].map((b) => b.dataset.id);
  assert.deepEqual(order, [bid('y'), bid('x')], 'the deferred move applies after composition');
});

test('a block deleted during composition keeps the composed text copyable', async () => {
  const { app, socket } = await startApp(twoPages);
  const el = caretAt(app, 'y', 6);
  fire(el, 'compositionstart');
  el.firstChild.data += '글';
  socket().recv({ t: 'delta', generation: 0, seq: '11', tree: [], changes: [{ deleted: { id: bid('y'), inc: '5' } }] });
  assert.ok(el.isConnected, 'the composing node stays');
  fire(el, 'compositionend', { data: '글' });
  assert.equal(socket().frames('action').length, 0, 'no stale write');
  const notice = app.banners.querySelector('[data-notice]');
  assert.ok(notice.textContent.includes('second글'));
  assert.ok(!app.editableOf(bid('y')));
});

test('Markdown shortcuts change the kind of an empty block and inline pairs add marks', async () => {
  const { app, socket, timers } = await startApp({ pages: twoPages.pages, blocks: [block('e', 'a', para(''))] });
  const el = caretAt(app, 'e', 0);
  beforeinput(el, 'insertText', '#');
  beforeinput(app.editableOf(bid('e')), 'insertText', ' ');
  assert.deepEqual(queued(app).ops.at(-1).content, { kind: 'heading1', text: [] });
  const h = app.editableOf(bid('e'));
  assert.equal(h.tagName, 'H2', 'a heading 1 block is a real heading under the page title');
  for (const c of '**hi**') beforeinput(app.editableOf(bid('e')), 'insertText', c);
  timers.advance(200);
  assert.deepEqual(queued(app).ops[0].content, { kind: 'heading1', text: [run('hi', 'bold')] });
  assert.equal(socket().frames('action').length, 1, 'one action in flight at a time');
});

test('the slash menu filters as the person types and turns the block', async () => {
  const { app } = await startApp({ pages: twoPages.pages, blocks: [block('e', 'a', para(''))] });
  const el = caretAt(app, 'e', 0);
  beforeinput(el, 'insertText', '/');
  let menu = app.pageEl.querySelector('[role="listbox"]');
  assert.ok(menu, 'opened');
  assert.equal(el.getAttribute('aria-controls'), menu.id);
  for (const c of 'to') beforeinput(app.editableOf(bid('e')), 'insertText', c);
  menu = app.pageEl.querySelector('[role="listbox"]');
  const options = [...menu.querySelectorAll('[role="option"]')].map((o) => o.dataset.key);
  assert.deepEqual(options, ['todo', 'toggle']);
  fire(app.editableOf(bid('e')), 'keydown', { key: 'ArrowDown' });
  assert.ok(app.editableOf(bid('e')).getAttribute('aria-activedescendant').endsWith('-toggle'));
  fire(app.editableOf(bid('e')), 'keydown', { key: 'Enter' });
  assert.equal(app.pageEl.querySelector('[role="listbox"]'), null);
  assert.deepEqual(queued(app).ops.at(-1).content, { kind: 'toggle', text: [], open_default: false }, 'the slash query is removed');
  assert.equal(app.pageView.entryOf(bid('e')).el.dataset.k, 'toggle');
});

test('Tab nests a list item under the previous one', async () => {
  const { app, socket } = await startApp({ pages: twoPages.pages, blocks: [block('l', 'a', para('one', 'bullet'), { position: 'A' }), block('m', 'a', para('two', 'bullet'), { position: 'B' })] });
  fire(caretAt(app, 'm', 1), 'keydown', { key: 'Tab' });
  const a = lastAction(socket);
  assert.equal(a.ops[0].op, 'move_blocks');
  assert.deepEqual(a.ops[0].place.parent, { id: bid('l'), inc: '5' });
  const kids = app.pageView.entryOf(bid('l')).kids;
  assert.equal(kids.querySelector('.blk').dataset.id, bid('m'));
  assert.equal(document.activeElement, app.editableOf(bid('m')));
});

test('Shift+Down at a block end selects across blocks; Delete removes them as one action', async () => {
  const { app, socket } = await startApp(twoPages);
  const el = caretAt(app, 'x', 11);
  fire(el, 'keydown', { key: 'ArrowDown', shiftKey: true });
  assert.deepEqual([...app.pageEl.querySelectorAll('.blk.sel')].map((b) => b.dataset.id), [bid('x'), bid('y')]);
  fire(app.pageEl, 'keydown', { key: 'Delete' });
  assert.deepEqual(lastAction(socket).ops, [{ op: 'delete_blocks', blocks: [{ id: bid('x'), inc: '5' }, { id: bid('y'), inc: '5' }] }]);
});

test('copying a cross-block selection reads the model', async () => {
  const { app } = await startApp(twoPages);
  app.selection.set({ block: bid('x'), offset: 6 }, { block: bid('y'), offset: 3 });
  const data = {};
  fire(document, 'copy', { clipboardData: { setData: (k, v) => { data[k] = v; } } });
  assert.equal(data['text/plain'], 'world\nsec');
});

test('the sidebar is a keyboard tree: arrows move, Right expands, Enter opens', async () => {
  const { app } = await startApp(twoPages);
  const tree = app.side.querySelector('[role="tree"]');
  const item = (id) => tree.querySelector(`[data-id="${pid(id)}"]`);
  assert.equal(item('a').getAttribute('aria-selected'), 'true');
  assert.equal(item('a').getAttribute('tabindex'), '0');
  item('a').focus();
  fire(item('a'), 'keydown', { key: 'ArrowDown' });
  assert.equal(document.activeElement, item('b'));
  assert.equal(item('b').getAttribute('aria-expanded'), 'false');
  assert.equal(item('c'), null);
  fire(item('b'), 'keydown', { key: 'ArrowRight' });
  assert.equal(item('b').getAttribute('aria-expanded'), 'true');
  fire(item('b'), 'keydown', { key: 'ArrowRight' });
  assert.equal(document.activeElement, item('c'));
  assert.equal(item('c').getAttribute('aria-level'), '2');
  fire(item('c'), 'keydown', { key: 'Enter' });
  assert.equal(app.page, pid('c'));
  fire(item('c'), 'keydown', { key: 'ArrowLeft' });
  assert.equal(document.activeElement, item('b'));
});

test('the tree, breadcrumb and title stay live with remote page changes', async () => {
  const { app, socket } = await startApp(twoPages);
  socket().recv({ t: 'delta', generation: 0, seq: '11', tree: [page('d', { title: 'New from Joon', position: 'C' })], changes: [] });
  socket().recv({ t: 'delta', generation: 0, seq: '12', tree: [page('a', { title: 'Home renamed', position: 'A' })], changes: [] });
  const labels = [...app.side.querySelectorAll('[role="treeitem"] .row a')].map((a) => a.textContent);
  assert.deepEqual(labels, ['Home renamed', 'Plans', 'New from Joon']);
  assert.equal(app.titleEl.textContent, 'Home renamed');
});

test('a page link to a deleted page shows Missing page and works again on restore', async () => {
  const { app, socket } = await startApp({ pages: twoPages.pages, blocks: [block('k', 'a', { kind: 'page_link', target: pid('b') })] });
  const link = () => app.pageView.entryOf(bid('k')).link;
  assert.equal(link().getAttribute('href'), '#/' + pid('b'));
  assert.equal(link().textContent, 'Plans');
  socket().recv({ t: 'delta', generation: 0, seq: '11', tree: [{ deleted: { id: pid('b'), inc: '5' } }], changes: [] });
  assert.equal(link().textContent, 'Missing page');
  assert.equal(link().getAttribute('href'), null);
  socket().recv({ t: 'delta', generation: 0, seq: '12', tree: [page('b', { title: 'Plans', position: 'B', inc: '12' })], changes: [] });
  assert.equal(link().textContent, 'Plans');
});

test('an unrelated delta patches only its block and keeps the focused editor and caret', async () => {
  const { app, socket } = await startApp(twoPages);
  const el = caretAt(app, 'x', 3);
  const yNode = app.pageView.entryOf(bid('y')).el;
  socket().recv({ t: 'delta', generation: 0, seq: '11', tree: [], changes: [block('y', 'a', para('changed'), { position: 'B', v: { content: '11' } })] });
  assert.equal(app.editableOf(bid('x')), el);
  assert.equal(document.activeElement, el);
  assert.equal(app.pageView.entryOf(bid('y')).el, yNode, 'patched in place');
  assert.equal(yNode.textContent, 'changed');
  const s = getSelection();
  assert.equal(s.anchorNode.parentNode.closest('[data-field]'), el);
});

test('a remote replacement of the focused block maps the caret by prefix and suffix', async () => {
  const { app, socket } = await startApp({ pages: twoPages.pages, blocks: [block('x', 'a', para('hello world'))] });
  caretAt(app, 'x', 8);
  socket().recv({ t: 'delta', generation: 0, seq: '11', tree: [], changes: [block('x', 'a', para('hello big world'), { v: { content: '11' } })] });
  const s = getSelection();
  assert.equal(s.anchorNode.data, 'hello big world');
  assert.equal(s.anchorOffset, 12);
});

test('a deleted focused block moves focus to the nearest survivor', async () => {
  const { app, socket, doc } = await startApp(twoPages);
  caretAt(app, 'x', 1);
  socket().recv({ t: 'delta', generation: 0, seq: '11', tree: [], changes: [{ deleted: { id: bid('x'), inc: '5' } }] });
  assert.equal(document.activeElement, app.editableOf(bid('y')));
  assert.equal(doc.querySelector('#announce').textContent, 'The block you were editing was removed.');
});

test('a Public route renders read-only without editing controls', async () => {
  const { app } = await startApp({ ...twoPages, access: 'public' });
  assert.equal(app.editableOf(bid('x')).getAttribute('contenteditable'), null);
  assert.equal(app.pageEl.querySelector('.h'), null, 'no block handles');
  assert.equal(app.titleEl.getAttribute('contenteditable'), null);
  assert.equal(app.statusEl.textContent, 'Public · read only');
  assert.equal(app.historyBtn.hidden, true);
});

test('disconnection keeps unsaved edits visible and warns before unload', async () => {
  const { app, socket, timers } = await startApp(twoPages);
  beforeinput(caretAt(app, 'x', 0), 'insertText', 'Z');
  timers.advance(200);
  socket().drop();
  assert.equal(app.statusEl.textContent, 'Disconnected · 1 unsaved edit');
  assert.equal(app.editableOf(bid('x')).textContent, 'Zhello world');
  assert.ok(app.banners.textContent.includes('Reconnecting.'));
  const e = fire(window, 'beforeunload', {});
  assert.equal(e.defaultPrevented, true);
});

test('stopped editing offers the unconfirmed text to copy', async () => {
  const { app, socket, timers } = await startApp(twoPages);
  beforeinput(caretAt(app, 'x', 0), 'insertText', 'Z');
  timers.advance(200);
  socket().recv({ t: 'reject', n: '1', code: 'expired', retry: false, detail: {} });
  assert.equal(app.statusEl.textContent, 'Editing stopped');
  const ta = app.banners.querySelector('textarea');
  assert.equal(ta.textContent, 'Zhello world');
  assert.equal(app.editableOf(bid('x')).getAttribute('contenteditable'), null, 'editing is off');
});

test('presence names who edits a block and is not a lock', async () => {
  const { app, socket, timers } = await startApp(twoPages);
  socket().recv({ t: 'presence', editors: [{ label: 'Minji', verified: true, page: pid('a'), edit: { block: { id: bid('y'), inc: '5' } } }] });
  const y = app.pageView.entryOf(bid('y')).el;
  assert.equal(y.querySelector('.editing-tag').textContent, 'Minji is editing');
  assert.equal(app.editableOf(bid('y')).getAttribute('contenteditable'), 'true');
  assert.ok(app.side.querySelector(`[data-id="${pid('a')}"]`).getAttribute('aria-label').includes('Minji here'));
  caretAt(app, 'x', 0);
  timers.advance(50);
  assert.deepEqual(socket().frames('presence').at(-1), { type: 'presence', page: pid('a'), edit: { block: { id: bid('x'), inc: '5' } } });
});

test('user text is never parsed as HTML anywhere in the shell', async () => {
  const evil = '<img src=x onerror=alert(1)><script>alert(2)</script>';
  const { app, doc } = await startApp({
    pages: [page('a', { title: evil, position: 'A' }), page('b', { title: evil, parent: 'a' })],
    blocks: [block('x', 'a', para(evil)), block('k', 'a', { kind: 'page_link', target: pid('b') }), block('c', 'a', { kind: 'code', text: evil, language: evil }),
      block('i', 'a', { kind: 'image', asset: 'a'.repeat(64), alt: evil, caption: [run(evil)] })],
    hash: '#/' + pid('a'),
  });
  app.sidebar.toggle(pid('a'), true);
  app.notice(evil, para(evil), bid('x'));
  app.render();
  assert.deepEqual(htmlWrites, []);
  for (const tag of ['SCRIPT', 'IFRAME']) assert.equal(doc.querySelectorAll(tag.toLowerCase()).length, 0);
  const imgs = doc.querySelectorAll('img');
  assert.equal(imgs.length, 1, 'only the image block');
  assert.equal(imgs[0].getAttribute('src'), '/_notebook/a/' + 'a'.repeat(64) + '.png');
  assert.equal(imgs[0].getAttribute('alt'), evil);
  assert.ok(textNodes(doc.body).filter((t) => t.data.includes('<img')).length >= 6);
});

test('the shell has no inline script or style and loads only versioned app files', () => {
  const html = readFileSync(new URL('../index.html', import.meta.url), 'utf8');
  assert.doesNotMatch(html, /<script(?![^>]*\bsrc=)[^>]*>/, 'every script has a src');
  assert.doesNotMatch(html, /\sstyle=|\son[a-z]+=|<style/i);
  for (const src of html.matchAll(/(?:src|href)="([^"]+)"/g)) assert.match(src[1], /^\/_notebook\/app\//);
});

test('no module sets inline styles or HTML', () => {
  for (const f of ['app.js', 'blocks.js', 'dom.js', 'editor.js', 'menu.js', 'sidebar.js', 'presence.js', 'history.js', 'richtext.js', 'selection.js', 'paste.js', 'a11y.js', 'theme.js', 'actions.js', 'state.js', 'net.js', 'undo.js']) {
    const src = readFileSync(new URL('../' + f, import.meta.url), 'utf8');
    assert.doesNotMatch(src, /innerHTML|outerHTML|insertAdjacentHTML|document\.write|\.style\s*=|setAttribute\('style'|eval\(|new Function/, f);
  }
});

test('at phone width the sidebar is a menu and the page cannot scroll sideways', async () => {
  const css = readFileSync(new URL('../app.css', import.meta.url), 'utf8');
  const phone = css.slice(css.indexOf('@media (max-width: 720px)'));
  assert.match(phone, /\.side \{ display: none; \}/);
  assert.match(phone, /\.menu-btn \{ display: inline-flex; \}/);
  assert.match(css, /\.scroll \{[^}]*overflow-x: hidden/);
  assert.match(css, /\.code pre \{[^}]*overflow-x: auto/);
  globalThis.__phone = true;
  try {
    const { app } = await startApp(twoPages);
    assert.equal(document.documentElement.dataset.layout, 'phone');
    assert.equal(app.toggleSide.getAttribute('aria-expanded'), 'false');
    app.toggleSide.click();
    assert.ok(app.side.classList.contains('open'));
    assert.equal(app.toggleSide.getAttribute('aria-expanded'), 'true');
    const link = app.side.querySelector(`[data-id="${pid('b')}"] a`);
    fire(link, 'click', {});
    assert.equal(app.page, pid('b'));
    assert.ok(!app.side.classList.contains('open'), 'opening a page closes the menu');
  } finally {
    globalThis.__phone = false;
  }
});

// A page with one inline database: a title and a number property, one
// table view, and rows that arrive in database windows.
const did = 'd_' + 'd'.repeat(26), tid = 'pr_' + 't'.repeat(26), nid = 'pr_' + 'n'.repeat(26), vid = 'v_' + 'v'.repeat(26);
const rid = (n) => 'r_' + String(n).padStart(26, 'r');
const dbRow = (n, title, { inc = '5', ver = inc } = {}) => ({ kind: 'row', ref: { id: rid(n), inc }, ver, v: {}, database: did, position: 'V' + n, cells: { [tid]: { type: 'title', value: title }, [nid]: { type: 'number', value: String(n) } } });
const dbItems = [
  block('db', 'a', { kind: 'database', database: did }),
  { kind: 'database', ref: { id: did, inc: '5' }, ver: '5', v: {}, block: bid('db'), title: 'Tasks', property_order: [tid, nid], view_order: [vid] },
  { kind: 'property', ref: { id: tid, inc: '5' }, ver: '5', v: {}, database: did, definition: { name: 'Name', type: 'title' }, option_order: [] },
  { kind: 'property', ref: { id: nid, inc: '5' }, ver: '5', v: {}, database: did, definition: { name: 'Score', type: 'number', format: 'plain' }, option_order: [] },
  { kind: 'view', ref: { id: vid, inc: '5' }, ver: '5', v: {}, database: did, definition: { name: 'Table', display: { layout: 'table', visible: [tid, nid], widths: [] }, sort: [], filter: [] } },
].map((i) => ({ ...i, page: i.page || pid('a') }));

async function startDatabase({ access = 'private', rows = [dbRow(1, 'One'), dbRow(2, 'Two'), dbRow(3, 'Three')] } = {}) {
  const t = await startApp({ access, pages: twoPages.pages, blocks: dbItems });
  t.window = (window, items, final) => {
    const watch = t.socket().frames('watch').at(-1);
    t.socket().recv({ t: 'snapshot', generation: watch ? watch.generation : 0, seq: '10', scope: { kind: 'database', id: did }, spill: 's', window, rows: items.filter((i) => i.kind === 'row').length, final_window: final, spill_part: 2 + window, part: 0, parts: 1, data: { items } });
  };
  t.el = () => t.app.pageView.entryOf(bid('db')).db;
  t.cell = (n, prop = tid) => t.el().querySelector(`tr[data-row="${rid(n)}"] td[data-prop="${prop}"]`);
  t.window(0, dbItems.slice(1), false);
  if (rows) t.window(1, rows, true);
  return t;
}

test('a database block shows its title, and loading ends with the final window', async () => {
  const t = await startDatabase({ rows: null });
  assert.deepEqual(t.socket().frames('watch').at(-1).visible_databases, [did]);
  assert.match(t.el().querySelector('.db-title').textContent, /^Tasks$/);
  assert.match(t.el().querySelector('.db-loading').textContent, /Loading rows/);
  t.window(1, [dbRow(1, 'One')], false);
  assert.ok(t.el().querySelector('.db-loading'), 'a later window is still coming');
  assert.equal(t.cell(1).textContent, 'One', 'rows already arrived are usable');
  t.window(2, [dbRow(2, 'Two')], true);
  assert.equal(t.el().querySelector('.db-loading'), null);
  assert.equal(t.el().querySelector('table').getAttribute('aria-rowcount'), '3');
  assert.deepEqual(t.el().querySelectorAll('tbody tr[aria-rowindex]').map((r) => r.getAttribute('aria-rowindex')), ['2', '3']);
});

test('a remote change to another row keeps an open cell editor focused', async () => {
  const t = await startDatabase();
  const root = t.el().firstChild;
  const cell = t.cell(1);
  cell.focus();
  fire(cell, 'keydown', { key: 'Enter' });
  const input = cell.querySelector('input');
  assert.equal(document.activeElement, input, 'the cell editor is open');
  input.value = 'Renamed';
  t.socket().recv({ t: 'delta', generation: 0, seq: '11', tree: [], changes: [dbRow(3, 'Remote', { ver: '11' })] });
  assert.equal(t.cell(3).textContent, 'Remote', 'the remote change is shown');
  assert.equal(t.el().firstChild, root, 'the view was updated, not mounted again');
  assert.equal(document.activeElement, input, 'the editor kept focus');
  assert.equal(input.value, 'Renamed', 'and what was typed');
  fire(input, 'keydown', { key: 'Enter' });
  assert.deepEqual(lastAction(t.socket).ops, [{ op: 'set_cell', row: { id: rid(1), inc: '5' }, cell: { property: { id: tid, inc: '5' }, value: 'Renamed' } }]);
  assert.equal(t.cell(1).textContent, 'Renamed', 'the pending edit shows at once');
  assert.equal(document.activeElement, t.cell(1), 'focus returns to the cell');
});

test('typing elsewhere on the page does not repaint the database view', async () => {
  const t = await startDatabase({ rows: [dbRow(1, 'One')] });
  const row = t.el().querySelector(`tr[data-row="${rid(1)}"]`);
  const tab = t.el().querySelector('[role=tab]');
  tab.focus();
  t.socket().recv({ t: 'delta', generation: 0, seq: '11', tree: [], changes: [dbRow(1, 'One', { ver: '11' })] });
  assert.equal(t.el().querySelector(`tr[data-row="${rid(1)}"]`), row, 'an unchanged row keeps its element');
  t.socket().recv({ t: 'delta', generation: 0, seq: '12', tree: [], changes: [dbRow(2, 'Two', { ver: '12' })] });
  assert.equal(document.activeElement, tab, 'the view tab keeps focus through a row change');
});

test('a database view stops when its block leaves the page', async () => {
  const t = await startDatabase();
  const listeners = () => (document.listeners.get('pointerdown') || []).length;
  const before = listeners();
  t.el().querySelectorAll('button').find((b) => b.textContent === 'Add filter').click();
  assert.ok(t.el().querySelector('.db-pop'), 'the filter form is open');
  assert.equal(listeners(), before + 1);
  t.socket().recv({ t: 'delta', generation: 0, seq: '11', tree: [], changes: [{ deleted: { id: bid('db'), inc: '5' } }] });
  assert.equal(t.app.pageView.entryOf(bid('db')), null);
  assert.equal(t.app.databases.size, 0);
  assert.equal(listeners(), before, 'stopping closed the open form');
});

test('focusing a database cell sends cell presence, and others show by name', async () => {
  const t = await startDatabase();
  t.cell(2, nid).focus();
  t.timers.advance(50);
  assert.deepEqual(t.socket().frames('presence').at(-1), { type: 'presence', page: pid('a'), edit: { database: { id: did, inc: '5' }, row: { id: rid(2), inc: '5' }, property: { id: nid, inc: '5' } } });
  t.socket().recv({ t: 'presence', editors: [{ label: 'Minji', verified: true, page: pid('a'), edit: { database: { id: did, inc: '5' }, row: { id: rid(3), inc: '5' }, property: { id: tid, inc: '5' } } }] });
  assert.match(t.cell(3).textContent, /Minji editing/);
});

test('a view repair notice names what the schema change removed', async () => {
  const t = await startDatabase();
  t.socket().recv({ t: 'view_adjustment', view: vid, notice_seq: '11', removed: ['sort:' + nid, 'filter:pr_' + 'x'.repeat(26)] });
  assert.equal(t.el().querySelector('.banner.info').textContent, 'A schema change removed the sort by Score and the filter on a deleted property from this view.');
});

test('a persisted view repair arriving after database hydration is displayed', async () => {
  const t = await startDatabase({ rows: null });
  assert.equal(t.el().querySelector('.banner.info'), null);
  t.window(1, [dbRow(1, 'One')], true);
  t.socket().recv({ type: 'view_adjustment', view: vid, notice_seq: '9', removed: ['visible:' + nid, 'filter:' + nid] });
  assert.equal(t.el().querySelector('.banner.info').textContent, 'A schema change removed Score from the shown properties and the filter on Score from this view.');
  for (const m of t.app.databases.values()) m.handle.stop();
  t.app.net.stop('test');
});

test('a Public reader sees the database without editing controls', async () => {
  const t = await startDatabase({ access: 'public' });
  assert.equal(t.cell(1).textContent, 'One');
  assert.equal(t.el().querySelectorAll('button').find((b) => b.textContent === 'New row'), undefined);
  assert.equal(t.el().querySelector('.col-resize'), null);
  assert.equal(t.el().querySelector('.row-handle'), null);
});

test('the stylesheet styles every class the database views use', () => {
  const css = readFileSync(new URL('../app.css', import.meta.url), 'utf8');
  const used = new Set();
  for (const f of ['table.js', 'kanban.js', 'dbschema.js']) {
    const src = readFileSync(new URL('../' + f, import.meta.url), 'utf8');
    for (const m of src.matchAll(/class: '([^']+)'/g)) for (const c of m[1].split(/\s+/)) if (c) used.add(c);
    for (const m of src.matchAll(/'(hidden-by-filter|opt)\b/g)) used.add(m[1]);
  }
  const styled = new Set([...css.matchAll(/\.([a-z][\w-]*)/g)].map((m) => m[1]));
  assert.deepEqual([...used].filter((c) => !styled.has(c)).sort(), []);
  const phone = css.slice(css.indexOf('@media (max-width: 720px)'));
  assert.match(phone, /\.kanban \{[^}]*flex-direction: column/);
  assert.match(css, /\.tablewrap \{[^}]*overflow-x: auto/);
});
