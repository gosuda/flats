// Opt-in: FLATS_PLAYWRIGHT_MODULE=/absolute/path/index.mjs node this-file BIN EVIDENCE
// Disposable real flats serve, MCP authoring, operator approvals, two browsers.
import assert from 'node:assert/strict';
import { spawn, execFileSync } from 'node:child_process';
import { mkdtemp, mkdir, writeFile, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { resolve, join } from 'node:path';
import { createServer } from 'node:net';
import { createHash } from 'node:crypto';
import { pathToFileURL } from 'node:url';
const { chromium } = await import(pathToFileURL(process.env.FLATS_PLAYWRIGHT_MODULE).href);
const binary = resolve(process.argv[2]), evidence = resolve(process.argv[3]);
await mkdir(evidence, { recursive: true });
const work = await mkdtemp(join(tmpdir(), 'flats-docs-product-data-'));
async function port() {
  const s = createServer();
  await new Promise(r => s.listen(0, '127.0.0.1', r));
  const n = s.address().port;
  await new Promise(r => s.close(r));
  assert.ok(n < 7878 || n > 7891);
  return n;
}
const managementPort = await port(), localPort = await port();
const base = `http://127.0.0.1:${managementPort}`;
const env = { ...process.env };
for (const k of Object.keys(env)) {
  if (k.startsWith('TS_') || ['FLATS_CONFIG', 'FLATS_DATA', 'FLATS_PORTAL_E2E', 'FLATS_URL'].includes(k)) delete env[k];
}
let server, browser, log = '', rpcID = 0, initialized = false;
const config = join(work, 'data', 'config.json');
const receipt = {
  head: execFileSync('git', ['rev-parse', 'HEAD'], { encoding: 'utf8' }).trim(),
  dirty: execFileSync('git', ['status', '--porcelain'], { encoding: 'utf8' }).trim(),
  binary_sha256: createHash('sha256').update(await readFile(binary)).digest('hex'),
  browser: '', cases: [], screenshots: [], acceptance: false,
  limitations: ['Loopback only; no Portal, Funnel or Tailscale', 'Browser text insertion includes Korean and emoji, not physical OS IME'],
};
async function wait(check, message, timeout = 30000) {
  const end = Date.now() + timeout;
  while (Date.now() < end) {
    if (await check()) return;
    await new Promise(r => setTimeout(r, 100));
  }
  throw new Error(message);
}
async function start() {
  if (!initialized) {
    execFileSync(binary, ['config', 'init', '--data', join(work, 'data'), '--listen', `127.0.0.1:${managementPort}`, '--local-addr', `127.0.0.1:${localPort}`], { env });
    initialized = true;
  }
  server = spawn(binary, ['serve', '--config', config], { env, stdio: ['pipe', 'pipe', 'pipe'] });
  server.stdin.end();
  server.stdout.on('data', d => { log += d; });
  server.stderr.on('data', d => { log += d; });
  await wait(async () => {
    if (server.exitCode !== null) throw new Error('Disposable host exited');
    try { return (await fetch(base + '/api/status', { headers: { 'X-Flats-Client': 'api' } })).ok; } catch { return false; }
  }, 'Host startup');
  await rpc('initialize', { protocolVersion: '2025-03-26', capabilities: {}, clientInfo: { name: 'docs-product-test', version: '1' } });
}
async function stop() {
  if (!server || server.exitCode !== null) return;
  server.kill('SIGTERM');
  await new Promise(r => {
    const timer = setTimeout(() => server.kill('SIGKILL'), 10000);
    server.once('exit', () => { clearTimeout(timer); r(); });
  });
}
async function rpc(method, params) {
  const res = await fetch(base + '/mcp', {
    method: 'POST', headers: { 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream' },
    body: JSON.stringify({ jsonrpc: '2.0', id: ++rpcID, method, params }),
  });
  const json = await res.json();
  assert.ok(res.ok && !json.error, JSON.stringify(json));
  return json.result;
}
async function tool(name, args, allowError = false) {
  const result = await rpc('tools/call', { name, arguments: args });
  assert.ok(allowError || !result.isError, JSON.stringify(result));
  if (result.structuredContent) return result.structuredContent;
  const text = result.content.find(c => c.type === 'text')?.text;
  // rollback reports pending approval through the tool's error JSON.
  try { return JSON.parse(text); } catch { throw new Error(`Missing structured result for ${name}: ${text}`); }
}
async function api(path) {
  const res = await fetch(base + '/api' + path, { headers: { 'X-Flats-Client': 'api' } });
  assert.ok(res.ok);
  return res.json();
}
async function operator(page, path, body) {
  return page.evaluate(async ({ path, body }) => {
    const res = await fetch('/console/api' + path, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Flats-Console': '1' }, body: JSON.stringify(body) });
    return { status: res.status, body: await res.json() };
  }, { path, body });
}
async function approve(page, request, label) {
  const approval = request.approval || request.action?.approval;
  assert.ok(approval?.id, JSON.stringify(request));
  assert.equal((await api('/flats/shared-doc')).live_version, label === 'v1' ? 0 : label === 'v2' ? 1 : 2);
  const res = await operator(page, `/approvals/${approval.id}/approve`, {});
  assert.equal(res.status, 200, JSON.stringify(res));
  assert.equal((res.body.approval || res.body).status, 'approved', JSON.stringify(res));
  receipt.cases.push({ approval: label, id: approval.id, status: 'approved' });
}
async function editorText(page) {
  return page.locator('.cm-content').evaluate(el => {
    const copy = el.cloneNode(true);
    for (const c of copy.querySelectorAll('.cm-ySelectionCaret')) c.remove();
    return [...copy.querySelectorAll('.cm-line')].map(l => l.textContent).join('\n');
  });
}
async function open(name, url) {
  const context = await browser.newContext({ viewport: { width: 1440, height: 960 } });
  await context.addInitScript(name => localStorage.setItem('flats-docs-name', name), name);
  const page = await context.newPage();
  page.on('pageerror', e => receipt.cases.push({ page_error: e.message }));
  await page.goto(url);
  await page.locator('.cm-content').waitFor();
  await wait(async () => await page.locator('#status').innerText() === 'Saved', 'Initial sync');
  return page;
}
async function converged(a, b, includes, excludes = []) {
  await wait(async () => {
    const [x, y] = await Promise.all([editorText(a), editorText(b)]);
    return x === y && includes.every(s => x.includes(s)) && excludes.every(s => !x.includes(s)) &&
      await a.locator('#status').innerText() === 'Saved' && await b.locator('#status').innerText() === 'Saved';
  }, 'Browser convergence');
  return editorText(a);
}
async function shot(page, name) {
  await page.screenshot({ path: join(evidence, name + '.png'), fullPage: true });
  receipt.screenshots.push(name + '.png');
}
const v1 = '# Shared\n\nAgent paragraph v1\n\nHuman paragraph\n';
try {
  await start();
  browser = await chromium.launch({ headless: true });
  receipt.browser = browser.version();
  const operatorContext = await browser.newContext();
  const operatorPage = await operatorContext.newPage();
  await operatorPage.goto(base);
  const saved = await tool('save_document', { slug: 'shared-doc', markdown: v1, title: 'Shared document' });
  assert.equal(saved.draft.type, 'docs');
  assert.equal((await tool('get_document', { slug: 'shared-doc' })).source, 'draft');
  // A real Draft preview must join and load source before the first publication.
  const preview = await tool('open_preview', { slug: 'shared-doc', version: 0 });
  const previewPage = await open('Preview', preview.url);
  assert.ok((await editorText(previewPage)).includes('Agent paragraph v1'));
  await shot(previewPage, 'draft-preview');
  await previewPage.context().close();
  receipt.cases.push({ draft_preview: 'pass' });
  await approve(operatorPage, await tool('publish', { slug: 'shared-doc', revision: saved.draft.revision, hash: saved.draft.hash }), 'v1');
  await operatorPage.goto(base);
  await operatorPage.locator('.flat .content-type').filter({ hasText: 'Document' }).waitFor();
  await shot(operatorPage, 'console-document-list');
  await operatorPage.goto(base + '/flats/shared-doc');
  await operatorPage.locator('.flat-head .content-type').filter({ hasText: 'Document' }).waitFor();
  await operatorPage.getByRole('heading', { name: 'Versions', exact: true }).waitFor();
  await shot(operatorPage, 'console-document-flat');
  receipt.cases.push({ restored_console_document_badges: 'pass' });
  const flat = await api('/flats/shared-doc');
  const a = await open('Alice', flat.private_url), b = await open('Bora', flat.private_url);
  assert.notEqual(a.context(), b.context());
  await Promise.all([a.locator('.cm-content').click(), b.locator('.cm-content').click()]);
  await Promise.all([a.keyboard.press('ControlOrMeta+End'), b.keyboard.press('ControlOrMeta+End')]);
  await Promise.all([a.keyboard.insertText('\nAlice 한국어 🙂'), b.keyboard.insertText('\nBora 한글 🌍')]);
  const human = ['Alice 한국어 🙂', 'Bora 한글 🌍'];
  await converged(a, b, human);
  await wait(async () => await a.locator('.cm-ySelectionCaret').count() > 0 && await b.locator('.cm-ySelectionCaret').count() > 0, 'Remote cursors');
  const live = await tool('get_document', { slug: 'shared-doc' });
  assert.ok(human.every(s => live.markdown.includes(s)));
  assert.equal(live.source, 'live');
  assert.ok(live.chain);
  await shot(a, 'concurrent-alice'); await shot(b, 'concurrent-bora');
  receipt.cases.push({ concurrent_unicode_edits: 'pass', remote_cursors: 'pass', durable_saved: 'pass', mcp_live: live });
  const second = await tool('save_document', { slug: 'shared-doc', markdown: v1.replace('paragraph v1', 'paragraph v2'), title: 'Shared document' });
  await approve(operatorPage, await tool('publish', { slug: 'shared-doc', revision: second.draft.revision, hash: second.draft.hash }), 'v2');
  const v2text = await converged(a, b, [...human, 'Agent paragraph v2'], ['Agent paragraph v1']);
  assert.equal((await tool('get_document', { slug: 'shared-doc' })).markdown.trimEnd(), v2text.trimEnd());
  await shot(a, 'published-v2');
  receipt.cases.push({ merged_v2: 'pass', markdown: v2text });
  // Use the real API's normal approval request; authoring and all reads use MCP.
  const rollbackResponse = await fetch(base + '/api/flats/shared-doc/rollback', { method: 'POST', headers: { 'X-Flats-Client': 'api', 'Content-Type': 'application/json' }, body: JSON.stringify({ version: 1, restore_data: false }) });
  assert.equal(rollbackResponse.status, 202);
  await approve(operatorPage, await rollbackResponse.json(), 'rollback-v1');
  const rollbackText = await converged(a, b, [...human, 'Agent paragraph v1'], ['Agent paragraph v2']);
  const rollback = await tool('get_document', { slug: 'shared-doc' });
  assert.equal(rollback.markdown.trimEnd(), rollbackText.trimEnd());
  await shot(a, 'rollback-v1');
  receipt.cases.push({ rollback_revisited_hash: 'pass', document: rollback });
  await stop();
  await start();
  const restored = await tool('get_document', { slug: 'shared-doc' });
  assert.equal(restored.markdown, rollback.markdown);
  assert.equal(restored.epoch, rollback.epoch);
  assert.equal(restored.seq, rollback.seq);
  await a.reload(); await b.reload();
  await converged(a, b, [...human, 'Agent paragraph v1'], ['Agent paragraph v2']);
  await shot(a, 'restart-persisted');
  receipt.cases.push({ real_server_restart_persistence: 'pass', document: restored });
  assert.ok(!receipt.cases.some(c => c.page_error));
  receipt.acceptance = true;
  console.log('PASS real-binary MCP / operator approvals / two-context collaboration / v2 / rollback / restart');
} catch (e) {
  receipt.failure = String(e.stack);
  throw new Error(receipt.failure);
} finally {
  if (browser) await browser.close();
  await stop();
  await writeFile(join(evidence, 'server.log'), log);
  await writeFile(join(evidence, 'receipt.json'), JSON.stringify(receipt, null, 2));
  await rm(work, { recursive: true, force: true });
}
