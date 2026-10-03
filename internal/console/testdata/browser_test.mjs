// Opt-in rendered verification on a disposable production binary and loopback host.
// FLATS_PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs node this-file BIN EVIDENCE_DIR
// No live providers, installed service, operator secrets, or user data are used.
import assert from 'node:assert/strict';
import { spawn, execFileSync } from 'node:child_process';
import { mkdtemp, mkdir, writeFile, readFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { resolve, join } from 'node:path';
import { createServer } from 'node:net';
import { createHash, randomBytes } from 'node:crypto';
import { pathToFileURL } from 'node:url';

const { chromium } = await import(pathToFileURL(process.env.FLATS_PLAYWRIGHT_MODULE).href);
const binary = resolve(process.argv[2]);
const evidence = resolve(process.argv[3]);
await mkdir(evidence, { recursive: true });
const work = await mkdtemp(join(tmpdir(), 'flats-ux-render-'));
const credential = randomBytes(32).toString('base64url');
async function port() {
  const s = createServer();
  await new Promise((r) => s.listen(0, '127.0.0.1', r));
  const n = s.address().port;
  await new Promise((r) => s.close(r));
  return n;
}
const managementPort = await port(), localPort = await port();
const base = `http://127.0.0.1:${managementPort}`;
const env = { ...process.env };
for (const key of ['TS_AUTHKEY', 'TS_AUTH_KEY', 'FLATS_PORTAL_E2E', 'FLATS_URL']) delete env[key];
const server = spawn(binary, ['serve', '--data', join(work, 'data'), '--listen', `127.0.0.1:${managementPort}`, '--local-addr', `127.0.0.1:${localPort}`, '--network', 'local', '--portal=false', '--operator-credential-stdin'], { env, stdio: ['pipe', 'pipe', 'pipe'] });
server.stdin.end(credential + '\n');
let log = '';
server.stdout.on('data', (d) => { log += d; });
server.stderr.on('data', (d) => { log += d; });
const receipt = {
  head: execFileSync('git', ['rev-parse', 'HEAD'], { encoding: 'utf8' }).trim(),
  tree: execFileSync('git', ['rev-parse', 'HEAD^{tree}'], { encoding: 'utf8' }).trim(),
  dirty: execFileSync('git', ['status', '--porcelain'], { encoding: 'utf8' }).trim(),
  binary_sha256: createHash('sha256').update(await readFile(binary)).digest('hex'),
  binary_build: execFileSync('go', ['version', '-m', binary], { encoding: 'utf8' }),
  cases: [], screenshots: [], limitations: ['Local production host only; no real Portal/Funnel/Tailscale paths', 'provider_not_ready/unavailable/public_stop_unconfirmed/unchanged_content rendered with explicit server-shaped response fixtures; real provider_not_permitted and stale_approval use actual API responses'],
};
let browser;
async function agent(method, path, body, raw = false) {
  const res = await fetch(base + '/api' + path, { method, headers: { 'X-Flats-Client': 'api', 'Content-Type': raw ? 'application/gzip' : 'application/json' }, body: body === undefined ? undefined : raw ? body : JSON.stringify(body) });
  const json = await res.json();
  assert.ok(res.ok, `${res.status}: ${JSON.stringify(json)}`);
  return json;
}
async function save(slug, n, kind = 'static') {
  const source = join(work, `${slug}-${n}`);
  await mkdir(source);
  if (kind === 'static') await writeFile(join(source, 'index.html'), `<h1>Rendered ${slug} ${n}</h1>`);
  else {
    await writeFile(join(source, 'flats.json'), '{"kind":"server"}');
    await writeFile(join(source, 'server.js'), `export default { fetch(request, env) { env.DB.exec('CREATE TABLE IF NOT EXISTS hits (n INTEGER)'); env.DB.exec('INSERT INTO hits VALUES (1)'); env.FILES.put('marker', 'version-${n}'); return new Response('server-${n}'); } };`);
  }
  const archive = execFileSync('tar', ['-czf', '-', '-C', source, '.']);
  return agent('POST', `/flats/${slug}/draft?expected_revision=${n - 1}`, archive, true);
}
try {
  for (let i = 0; i < 100; i++) {
    try { if ((await fetch(base + '/api/status', { headers: { 'X-Flats-Client': 'api' } })).ok) break; } catch {}
    if (server.exitCode !== null) throw new Error('Disposable host exited: ' + log);
    if (i === 99) throw new Error('Disposable host did not start: ' + log);
    await new Promise((r) => setTimeout(r, 100));
  }
  browser = await chromium.launch({ headless: true });
  const context = await browser.newContext();
  const page = await context.newPage();
  const pageErrors = [];
  page.on('pageerror', (err) => pageErrors.push(err.message));
  await page.goto(base);
  await page.getByRole('button', { name: 'Unlock decisions', exact: true }).click();
  await page.getByLabel('Operator credential', { exact: true }).fill(credential);
  await page.getByRole('button', { name: 'Unlock', exact: true }).click();
  await page.getByText('Operator session unlocked', { exact: true }).waitFor();
  async function shot(label, mobile) {
    await page.setViewportSize(mobile ? { width: 390, height: 844 } : { width: 1280, height: 900 });
    await page.evaluate(() => window.scrollTo(0, 0));
    const size = await page.evaluate(() => ({ width: innerWidth, scroll: document.documentElement.scrollWidth }));
    assert.ok(size.scroll <= size.width, `${label}: overflow ${JSON.stringify(size)}`);
    const file = `${mobile ? 'mobile' : 'desktop'}-${label}.png`;
    await page.screenshot({ path: join(evidence, file), fullPage: true });
    receipt.screenshots.push({ file, ...size });
  }
  async function both(label) { await shot(label, false); await shot(label, true); }
  async function requestPublish(slug) { const f = await agent('GET', `/flats/${slug}`); return agent('POST', `/flats/${slug}/publish`, { revision: f.draft.revision, hash: f.draft.hash }); }
  async function decision(request, approve = true, expected = 'approved') {
    await page.goto(base + '/approvals/' + request.approval.id);
    await page.getByRole('button', { name: approve ? 'Approve…' : 'Reject…', exact: true }).click();
    const response = page.waitForResponse((r) => r.url().endsWith(`/approvals/${request.approval.id}/${approve ? 'approve' : 'reject'}`));
    await page.getByRole('button', { name: approve ? 'Approve' : 'Reject', exact: true }).click();
    const res = await response;
    const body = await res.json();
    assert.equal((body.approval || body).status, expected);
    await page.getByText(`This request was ${expected}.`, { exact: true }).waitFor();
    receipt.cases.push({ id: request.approval.id, status: expected, http: res.status(), category: body.category, failure_code: body.approval?.result_data?.failure_code });
    return body;
  }
  await save('rendered', 1);
  const first = await requestPublish('rendered');
  await page.goto(base + '/approvals/' + first.approval.id);
  await both('final-publish-pending');
  await decision(first);
  await both('final-publish-approved');
  const live = await agent('GET', '/flats/rendered');
  assert.equal(live.live_version, 1);
  assert.ok((await (await fetch(live.private_url)).text()).includes('Rendered rendered 1'));
  await page.goto(base + '/flats/rendered');
  await page.getByText('Published · v1', { exact: false }).first().waitFor();
  assert.equal(await page.getByRole('button', { name: 'Publish the next version after v1', exact: true }).count(), 0);
  const versionPreview = await agent('POST', '/flats/rendered/previews', { target: 'version', version: 1 });
  await page.reload();
  await page.getByRole('tab', { name: 'Access', exact: true }).click();
  await page.locator('strong').filter({ hasText: 'v1 preview (Private)' }).waitFor();
  receipt.cases.push({ version_preview: versionPreview.host, label: 'v1 preview (Private)' });
  const missing = await agent('POST', '/flats/rendered/visibility', { visibility: 'public' });
  await page.goto(base + '/approvals/' + missing.approval.id);
  await page.getByText('If approved, this flat becomes Public: anyone on the internet can open it.', { exact: false }).waitFor();
  await page.getByText('Provider permissions at request', { exact: true }).waitFor();
  const policyFingerprint = String(missing.approval.params.providers || '').split('\n')[1];
  if (policyFingerprint) assert.equal((await page.locator('main').innerText()).includes(policyFingerprint), false);
  assert.equal((await agent('GET', '/flats/rendered')).visibility, 'private');
  await both('provider-missing-pending');
  assert.equal((await agent('GET', '/approvals/' + missing.approval.id)).status, 'pending');
  await page.getByRole('button', { name: 'Approve…', exact: true }).click();
  assert.ok(await page.getByRole('button', { name: 'Approve', exact: true }).evaluate((b) => b.classList.contains('btn-danger')));
  const confirmation = await page.locator('dialog[open]').innerText();
  assert.ok(confirmation.includes('If approved, this flat becomes Public:'));
  assert.equal(confirmation.includes('This flat is public:'), false);
  await both('public-danger-confirmation');
  await page.getByRole('button', { name: 'Cancel', exact: true }).click();
  const missingFailure = await decision(missing, true, 'failed');
  assert.equal(missingFailure.category, 'provider_not_permitted');
  await page.getByText('This provider is not permitted.', { exact: false }).first().waitFor();
  await both('typed-provider-not-permitted');
  assert.equal((await agent('GET', '/flats/rendered')).visibility, 'private');
  for (const code of ['provider_not_ready', 'provider_unavailable', 'public_stop_unconfirmed', 'unchanged_content']) {
    const request = await agent('POST', '/flats/rendered/visibility', { visibility: 'public' });
    const fixture = { ...missingFailure, category: code, error: code, approval: { ...missingFailure.approval, id: request.approval.id, result: code, result_data: { ...missingFailure.approval.result_data, failure_code: code } } };
    await page.route(`**/console/api/approvals/${request.approval.id}/approve`, (route) => route.fulfill({ status: 409, json: fixture }));
    await page.goto(base + '/approvals/' + request.approval.id);
    await page.getByRole('button', { name: 'Approve…', exact: true }).click();
    await page.getByRole('button', { name: 'Approve', exact: true }).click();
    await page.locator('.alert-error').first().waitFor();
    assert.equal(await page.getByText('The content or access settings changed. Review again.', { exact: true }).count(), 0);
    await both('typed-' + code);
    receipt.cases.push({ category: code, http: 409, fixture: true });
    await page.unroute(`**/console/api/approvals/${request.approval.id}/approve`);
    await decision(request, false, 'rejected');
  }
  await save('rendered', 2);
  const stale = await requestPublish('rendered');
  await save('rendered', 3);
  const staleFailure = await decision(stale, true, 'failed');
  assert.equal(staleFailure.category, 'stale_approval');
  await both('typed-stale-approval');
  const rejected = await requestPublish('rendered');
  await decision(rejected, false, 'rejected');
  await both('final-rejection');
  assert.equal((await agent('GET', '/flats/rendered')).live_version, 1);
  for (let n = 1; n <= 2; n++) {
    await save('data-risk', n, 'server');
    const request = await requestPublish('data-risk');
    await page.goto(base + '/approvals/' + request.approval.id);
    await page.getByText('Starting the server runtime may write to live DB and FILES', { exact: false }).waitFor();
    if (n === 1) await both('server-runtime-risk');
    await decision(request);
    const f = await agent('GET', '/flats/data-risk');
    assert.equal(await (await fetch(f.private_url)).text(), `server-${n}`);
  }
  const restore = await agent('POST', '/flats/data-risk/rollback', { version: 1, restore_data: true });
  assert.ok(restore.approval.params.snapshot && restore.approval.params.snapshot_hash);
  await page.goto(base + '/approvals/' + restore.approval.id);
  await page.getByText(restore.approval.params.snapshot_hash, { exact: true }).waitFor();
  await page.getByText('Replaces the live database and captured FILES', { exact: false }).waitFor();
  await both('frozen-restore-pending');
  await page.getByRole('button', { name: 'Approve…', exact: true }).click();
  assert.ok(await page.getByRole('button', { name: 'Approve', exact: true }).evaluate((b) => b.classList.contains('btn-danger')));
  await both('frozen-restore-danger');
  await page.getByRole('button', { name: 'Cancel', exact: true }).click();
  assert.equal((await agent('GET', '/approvals/' + restore.approval.id)).status, 'pending');
  await decision(restore, false, 'rejected');
  // Successful actions replace the opener: focus must land on its replacement/fallback.
  await page.goto(base + '/flats/rendered');
  await page.getByRole('button', { name: 'Publish the next version after v1', exact: true }).click();
  await page.getByRole('button', { name: 'Approve and publish', exact: true }).click();
  await page.getByRole('button', { name: 'Close', exact: true }).click();
  await page.getByText('Published · v2', { exact: false }).first().waitFor();
  await page.waitForFunction(() => document.activeElement?.id === 'draft');
  receipt.cases.push({ focus_after_publish: await page.evaluate(() => document.activeElement.id) });
  // Nonsecret status resynchronizes a retained cookie after reload.
  await page.reload();
  await page.getByText('Operator session unlocked', { exact: true }).waitFor();
  receipt.cases.push({ operator_status_after_reload: 'authorized' });
  await both('operator-session-resynchronized');
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await page.getByText('Decisions require operator unlock', { exact: true }).waitFor();
  await page.reload();
  await page.getByText('Decisions require operator unlock', { exact: true }).waitFor();
  receipt.cases.push({ operator_status_after_logout_reload: 'unauthorized' });
  assert.deepEqual(pageErrors, []);
  receipt.page_errors = pageErrors;
  receipt.acceptance = true;
  console.log('PASS rendered desktop/mobile lifecycle correction matrix');
} finally {
  if (browser) await browser.close();
  server.kill('SIGTERM');
  await new Promise((r) => { if (server.exitCode !== null) r(); else { server.once('exit', r); setTimeout(() => server.kill('SIGKILL'), 7000).unref(); } });
  // Disposable startup output is kept private; scrub the in-memory test credential.
  await writeFile(join(evidence, 'server.log'), log.replaceAll(credential, '[redacted]'));
  await writeFile(join(evidence, 'receipt.json'), JSON.stringify(receipt, null, 2));
}
