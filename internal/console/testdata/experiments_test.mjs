// Settings › Experiments (Autopilot), the Agents page under Autopilot and the
// approval page's "Decided by" (docs/spec/autopilot.md rules 9-13, 25, 27).
// Reuses the console's DOM harness and runs its regression suite first; the
// Go runner (console_experiments_test.go) copies both scripts and writes
// autopilot_ops.json, the server's list of operation IDs.
import './ui_test.mjs';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { mount as settings, OPERATIONS } from './settings.js';
import { mount as approvalPage, decidedBy, approvalsTable } from './approval.js';

const tick = () => new Promise((resolve) => setTimeout(resolve, 20));
const all = (root, pred) => [root, ...(root.childNodes || []).flatMap((n) => all(n, () => true))].filter((n) => n.tagName && pred(n));
const byText = (root, text) => all(root, (e) => e.tagName === 'BUTTON' && e.textContent === text);
const byId = (root, id) => all(root, (e) => e.getAttribute('id') === id)[0];
const switches = (root) => all(root, (e) => e.tagName === 'INPUT' && e.getAttribute('role') === 'switch');
const openDialogs = () => all(document.body, (e) => e.tagName === 'DIALOG' && e.open !== false);
const ctx = { setTitle() {}, alive: () => true, navigate() {} };

const toasts = document.createElement('div');
document.getElementById = (id) => (id === 'toasts' ? toasts : null);
const lastToast = () => toasts.childNodes.at(-1);

// --- fake host ---
const BASE = '/console/api/experiments/autopilot';
let stored;
const reset = (view) => { stored = { enabled: false, allowed: [], guide: '', guide_max: 4000, ...view }; };
const calls = [];
// answers maps a request to a status and body, or a function returning a
// promise for a deferred answer; the stored state is then not changed.
const answers = {};
const serverOps = JSON.parse(readFileSync('./autopilot_ops.json', 'utf8'));
const approvals = {};
const json = (status, body) => ({ ok: status < 400, status, json: async () => body });
globalThis.fetch = async (url, opts) => {
  const key = (opts.method || 'GET') + ' ' + url;
  const body = opts.body ? JSON.parse(opts.body) : undefined;
  calls.push({ key, body, headers: opts.headers });
  if (answers[key]) {
    const a = answers[key];
    delete answers[key];
    const r = typeof a === 'function' ? await a() : a;
    return json(r.status, r.body);
  }
  if (key === 'GET ' + BASE) return json(200, { autopilot: stored });
  if (key === 'PUT ' + BASE) {
    if (body.enabled && JSON.stringify(body.confirmed_allowed) !== JSON.stringify(stored.allowed)) {
      return json(409, { error: 'the allowed operations changed after the page loaded; review them and turn Autopilot on again', category: 'conflict' });
    }
    stored = { ...stored, enabled: body.enabled };
    return json(200, { autopilot: stored });
  }
  const op = url.startsWith(BASE + '/operations/') && decodeURIComponent(url.slice((BASE + '/operations/').length));
  if (op && opts.method === 'PUT') {
    const ids = serverOps.filter((id) => (id === op ? body.allowed : stored.allowed.includes(id)));
    stored = { ...stored, allowed: ids };
    return json(200, { autopilot: stored });
  }
  if (key === 'PUT ' + BASE + '/guide') {
    stored = { ...stored, guide: body.guide };
    return json(200, { autopilot: stored });
  }
  const m = /^GET \/console\/api\/approvals\/(.+)$/.exec(key);
  if (m && approvals[m[1]]) return json(200, approvals[m[1]]);
  return json(404, { error: 'not found' });
};
const sent = (path) => calls.filter((c) => c.key === 'PUT ' + BASE + path);

async function open(section) {
  const main = document.createElement('main');
  const stop = settings(main, [section], ctx);
  await tick();
  return { main, stop };
}

// The page has every operation of the server (whose list is in bit order),
// shown in its own groups and order (docs/surfaces.md, Console).
assert.deepEqual(OPERATIONS.map((o) => o.id).sort(), [...serverOps].sort());
assert.equal(new Set(OPERATIONS.map((o) => o.label)).size, OPERATIONS.length);
const DISPLAY = [
  ['Versions and deployments', ['create_version', 'deploy', 'rollback', 'delete_version']],
  ['Visibility', ['make_public', 'make_private', 'public_editing']],
  ['Data and deletion', ['rollback_restore_data', 'delete_flat']],
];
const DISPLAY_IDS = DISPLAY.flatMap(([, ids]) => ids);

// --- Experiments, off ---
reset({ allowed: ['deploy', 'make_public'] });
let { main, stop } = await open('experiments');
const nav = all(main, (e) => e.tagName === 'A' && e.getAttribute('aria-current') === 'page');
assert.deepEqual(nav.map((a) => a.textContent), ['Experiments']);
// The intro and the warning say the features may change and only people
// change the page (rule 2).
const intro = (root) => all(root, (e) => (e.getAttribute('class') || '') === 's-intro')[0];
assert.equal(intro(main).textContent, 'These features may change or be removed.');
const warning = all(main, (e) => (e.getAttribute('class') || '') === 'callout callout-warn')[0];
assert.equal(warning.textContent, 'Only people change this page: agents must never change it, including through a browser or by calling the console. Flats cannot enforce that. The console has no login and only refuses requests from other websites, so a program that can reach this address can do what this page does.');
assert.ok(main.textContent.indexOf('Only people change this page') < main.textContent.indexOf('Autopilot'), 'the warning is at the top');
assert.ok(byId(main, 'autopilot-state').textContent.includes('Off'));
assert.equal(byText(main, 'Turn on').length, 1);
assert.equal(byText(main, 'Turn on')[0].getAttribute('aria-label'), 'Turn on Autopilot');
// While off, no switches and no guide (rule 9).
assert.equal(switches(main).length, 0);
assert.equal(all(main, (e) => e.tagName === 'TEXTAREA').length, 0);
assert.ok(!main.textContent.includes('Operations agents may decide'));
assert.ok(!main.textContent.includes('Operation guide'));

// Turn on asks once, listing the operations kept from before (rule 10).
// Cancel sends nothing.
byText(main, 'Turn on')[0].dispatch('click');
await tick();
assert.equal(openDialogs().length, 1);
let dialog = openDialogs()[0];
assert.ok(dialog.textContent.includes('Turn on Autopilot?'));
assert.deepEqual(all(byId(dialog, 'autopilot-confirm-allowed'), (e) => e.tagName === 'LI').map((li) => li.textContent), ['Deploy', 'Make Public']);
assert.ok(dialog.textContent.includes('Anyone on the internet can open a Public flat.'));
assert.ok(dialog.textContent.includes('Agents follow the operation guide, but Flats does not enforce it.'));
assert.ok(!dialog.textContent.includes('No operation is allowed yet'));
dialog.close('cancel');
await tick();
assert.equal(sent('').length, 0, 'a cancelled Turn on sends nothing');
assert.equal(stored.enabled, false);
// Confirming sends exactly the listed operations and shows the switches.
byText(main, 'Turn on')[0].dispatch('click');
await tick();
openDialogs()[0].close('ok');
await tick();
assert.deepEqual(sent('').at(-1).body, { enabled: true, confirmed_allowed: ['deploy', 'make_public'] });
assert.equal(sent('').at(-1).headers['X-Flats-Console'], '1');
assert.equal(stored.enabled, true);
assert.equal(lastToast().textContent, 'Autopilot is on.');
assert.equal(byText(main, 'Turn off').length, 1);
assert.ok(byId(main, 'autopilot-state').textContent.includes('On'));
// While on: nine switches under three sub-headings and the guide (rule 11).
assert.deepEqual(switches(main).map((s) => s.getAttribute('id')), DISPLAY_IDS.map((id) => 'autopilot-op-' + id));
const ops = byId(main, 'autopilot-ops');
assert.deepEqual(all(ops, (e) => e.tagName === 'DIV' && e.getAttribute('class') === 's-sub').map((sub) =>
  [all(sub, (e) => e.tagName === 'H4')[0].textContent, switches(sub).map((s) => s.getAttribute('id').slice('autopilot-op-'.length))]), DISPLAY);
assert.ok(byId(main, 'row-autopilot-op-rollback_restore_data').textContent.includes('replaces data'));
assert.ok(byId(main, 'row-autopilot-op-delete_flat').textContent.includes('permanent'));
assert.deepEqual(switches(main).filter((s) => s.checked).map((s) => s.getAttribute('id')), ['autopilot-op-deploy', 'autopilot-op-make_public']);
assert.ok(byId(main, 'autopilot-guide-text'));
stop();

// The confirmation lists in display order and sends the server's order.
reset({ allowed: ['delete_version', 'deploy', 'make_public'] });
({ main, stop } = await open('experiments'));
byText(main, 'Turn on')[0].dispatch('click');
await tick();
assert.deepEqual(all(byId(openDialogs()[0], 'autopilot-confirm-allowed'), (e) => e.tagName === 'LI').map((li) => li.textContent), ['Deploy', 'Delete versions', 'Make Public']);
openDialogs()[0].close('ok');
await tick();
assert.deepEqual(sent('').at(-1).body, { enabled: true, confirmed_allowed: ['delete_version', 'deploy', 'make_public'] });
assert.equal(stored.enabled, true);
stop();

// With nothing allowed, the confirmation says so and sends an empty list.
reset({});
({ main, stop } = await open('experiments'));
byText(main, 'Turn on')[0].dispatch('click');
await tick();
dialog = openDialogs()[0];
assert.ok(dialog.textContent.includes('No operation is allowed yet; agents can decide nothing until you turn one on.'));
assert.equal(byId(dialog, 'autopilot-confirm-allowed'), undefined);
dialog.close('ok');
await tick();
assert.deepEqual(sent('').at(-1).body, { enabled: true, confirmed_allowed: [] });
stop();

// A Turn on whose operations changed since the page loaded is refused with
// 409: the server's message is shown and the page reloads (rule 10).
reset({ allowed: ['deploy'] });
({ main, stop } = await open('experiments'));
stored = { ...stored, allowed: ['deploy', 'delete_flat'] };
byText(main, 'Turn on')[0].dispatch('click');
await tick();
openDialogs()[0].close('ok');
await tick();
dialog = openDialogs()[0];
assert.ok(dialog.textContent.includes('The allowed operations changed'));
assert.ok(dialog.textContent.includes('review them and turn Autopilot on again'));
assert.equal(stored.enabled, false, 'nothing changed');
const loads = calls.filter((c) => c.key === 'GET ' + BASE).length;
dialog.close('ok');
await tick();
assert.equal(calls.filter((c) => c.key === 'GET ' + BASE).length, loads + 1, 'the page reloads');
byText(main, 'Turn on')[0].dispatch('click');
await tick();
assert.deepEqual(all(byId(openDialogs()[0], 'autopilot-confirm-allowed'), (e) => e.tagName === 'LI').map((li) => li.textContent), ['Deploy', 'Delete flats'],
  'the reloaded page lists the stored operations');
openDialogs()[0].close('cancel');
await tick();
stop();

// --- Experiments, on ---
// Turn off acts at once, without a dialog (rule 10).
reset({ enabled: true, allowed: ['deploy'], guide: 'Deploy previews only.' });
({ main, stop } = await open('experiments'));
byText(main, 'Turn off')[0].dispatch('click');
await tick();
assert.equal(openDialogs().length, 0, 'no confirmation');
assert.deepEqual(sent('').at(-1).body, { enabled: false });
assert.equal(lastToast().textContent, 'Autopilot is off.');
assert.equal(byText(main, 'Turn on').length, 1);
assert.equal(switches(main).length, 0, 'the switches are hidden again');
assert.deepEqual(stored.allowed, ['deploy'], 'turning off keeps the operations');
stop();

// A switch sends one PUT and applies at once, disabled while it runs (rule 12).
reset({ enabled: true, allowed: ['deploy'] });
({ main, stop } = await open('experiments'));
let sw = byId(main, 'autopilot-op-make_public');
let release;
answers['PUT ' + BASE + '/operations/make_public'] = () => new Promise((resolve) => { release = resolve; });
sw.checked = true;
sw.dispatch('change');
await tick();
assert.equal(sw.disabled, true, 'disabled while the change runs');
stored = { ...stored, allowed: ['deploy', 'make_public'] };
release({ status: 200, body: { autopilot: stored } });
await tick();
assert.equal(sw.disabled, false);
assert.equal(sw.checked, true);
assert.deepEqual(sent('/operations/make_public').map((c) => c.body), [{ allowed: true }]);
assert.equal(lastToast().textContent, 'Agents may now decide Make Public.');
sw.checked = false;
sw.dispatch('change');
await tick();
assert.deepEqual(sent('/operations/make_public').at(-1).body, { allowed: false });
assert.deepEqual(stored.allowed, ['deploy']);
assert.equal(lastToast().textContent, 'Agents can no longer decide Make Public.');
assert.equal(all(main, (e) => (e.getAttribute('class') || '') === 'savebar').length, 0, 'switches have no save bar');
// A failed change returns the switch to the stored value and says why.
sw = byId(main, 'autopilot-op-rollback');
answers['PUT ' + BASE + '/operations/rollback'] = { status: 500, body: { error: 'database is locked' } };
sw.checked = true;
sw.dispatch('change');
await tick();
assert.equal(sw.checked, false);
assert.equal(sw.disabled, false);
assert.ok(lastToast().className.includes('toast-error'));
assert.equal(lastToast().textContent, 'Roll back was not changed: database is locked');
// A change that committed but answered with an error shows the stored value
// once the page reads it again.
answers['PUT ' + BASE + '/operations/rollback'] = () => {
  stored = { ...stored, allowed: ['deploy', 'rollback'] };
  return { status: 500, body: { error: 'cannot read the settings' } };
};
sw.checked = true;
sw.dispatch('change');
await tick();
assert.equal(sw.checked, true, 'the stored value after reading it again');
assert.equal(byId(main, 'autopilot-op-deploy').checked, true);
stop();

// Changes of different switches run one at a time, so an older response can
// never undo a newer committed change: enable Deploy with a slow answer, then
// Make Public; the display ends equal to the stored operations.
reset({ enabled: true, allowed: [] });
({ main, stop } = await open('experiments'));
const deploySw = byId(main, 'autopilot-op-deploy');
const publicSw = byId(main, 'autopilot-op-make_public');
let releaseDeploy;
const publicBefore = sent('/operations/make_public').length;
const publicSent = () => sent('/operations/make_public').slice(publicBefore);
answers['PUT ' + BASE + '/operations/deploy'] = () => new Promise((resolve) => { releaseDeploy = resolve; });
deploySw.checked = true;
deploySw.dispatch('change');
await tick();
publicSw.checked = true;
publicSw.dispatch('change');
await tick();
assert.equal(publicSent().length, 0, 'the second change waits for the first');
assert.equal(publicSw.checked, true, 'a waiting switch keeps the chosen value');
assert.equal(publicSw.disabled, true);
stored = { ...stored, allowed: ['deploy'] };
releaseDeploy({ status: 200, body: { autopilot: stored } });
await tick();
await tick();
assert.equal(publicSw.checked, true, 'the older response does not uncheck the newer change');
assert.deepEqual(publicSent().map((c) => c.body), [{ allowed: true }]);
assert.deepEqual(stored.allowed, ['deploy', 'make_public']);
assert.deepEqual(switches(main).filter((s) => s.checked).map((s) => s.getAttribute('id').slice('autopilot-op-'.length)).sort(), [...stored.allowed].sort());
assert.ok(switches(main).every((s) => !s.disabled));
stop();

// The guide counts code points, as the server does, and has its own Save
// button (rule 13).
reset({ enabled: true, guide: 'Deploy previews only.' });
({ main, stop } = await open('experiments'));
const area = byId(main, 'autopilot-guide-text');
const count = byId(main, 'autopilot-guide-count');
const save = byText(main, 'Save guide')[0];
const discard = byText(main, 'Discard')[0];
assert.equal(area.value, 'Deploy previews only.');
assert.equal(count.textContent, '21 / 4,000 characters');
assert.equal(save.disabled, true, 'unchanged');
assert.equal(discard.hidden, true);
const type = (text) => { area.value = text; area.dispatch('input'); };
type('a😀b');
assert.equal(count.textContent, '3 / 4,000 characters', 'an emoji counts as one');
assert.equal(save.disabled, false);
assert.equal(discard.hidden, false);
type('😀'.repeat(4000));
assert.equal(count.textContent, '4,000 / 4,000 characters');
assert.equal(save.disabled, false);
assert.equal(area.getAttribute('aria-invalid'), null);
type('😀'.repeat(4001));
assert.equal(count.textContent, '4,001 / 4,000 characters');
assert.equal(count.className, 'field-error');
assert.equal(area.getAttribute('aria-invalid'), 'true');
assert.equal(save.disabled, true, 'over the limit');
discard.dispatch('click');
assert.equal(area.value, 'Deploy previews only.');
assert.equal(save.disabled, true);
type('Deploy previews only.\n# Names\n```\npr-<number>\n```');
save.dispatch('click');
await tick();
assert.deepEqual(sent('/guide').at(-1).body, { guide: 'Deploy previews only.\n# Names\n```\npr-<number>\n```' });
assert.equal(stored.guide, 'Deploy previews only.\n# Names\n```\npr-<number>\n```');
assert.equal(lastToast().textContent, 'Operation guide saved.');
assert.equal(save.disabled, true, 'saved text is not dirty');
assert.equal(discard.hidden, true);
// A refused save keeps the text and shows why.
answers['PUT ' + BASE + '/guide'] = { status: 400, body: { error: 'the operation guide has 4001 characters; at most 4000' } };
type('changed');
save.dispatch('click');
await tick();
assert.equal(area.value, 'changed');
assert.ok(main.textContent.includes('Cannot save the operation guide') && main.textContent.includes('at most 4000'));
assert.equal(save.disabled, false);
stop();

// A page that cannot load Autopilot says so and offers no control.
reset({});
answers['GET ' + BASE] = { status: 500, body: { error: 'storage unavailable' } };
({ main, stop } = await open('experiments'));
assert.ok(main.textContent.includes('Cannot load Autopilot') && main.textContent.includes('storage unavailable'));
assert.ok(main.textContent.includes('Only people change this page'));
assert.equal(byText(main, 'Turn on').length + byText(main, 'Turn off').length, 0);
stop();

// --- Agents page (rule 25) ---
const BEGIN = 'Agents can create flats, save Drafts, open previews and read logs. ';
reset({ allowed: ['deploy'] });
({ main, stop } = await open('agents'));
assert.equal(intro(main).textContent, BEGIN + 'Saving versions, deploying, visibility changes and deleting wait for your approval under Approvals.');
assert.equal(all(intro(main), (e) => e.tagName === 'A')[0].getAttribute('href'), '/approvals');
assert.ok(!main.textContent.includes('Autopilot on'));
assert.ok(!main.textContent.includes('What agents can decide'));
assert.ok(main.textContent.includes('Approve requests') && main.textContent.includes('No agent tool decides an approval'));
assert.ok(!main.textContent.includes('Decide other requests'));
assert.ok(byId(main, 'agent-limit-experiments').textContent.includes('Change Experiments'));
assert.ok(main.textContent.includes('No agent tool changes Experiments. Agents are told never to change them, also not through a browser; Flats cannot tell them apart from you.'));
stop();

reset({ enabled: true, allowed: ['create_version', 'delete_version', 'deploy', 'make_public'] });
({ main, stop } = await open('agents'));
assert.equal(intro(main).textContent, BEGIN + 'Autopilot is on: agents decide requests of the operations under What agents can decide themselves. Every other request to save a version, deploy, change visibility or delete waits for your approval under Approvals.');
assert.equal(all(intro(main), (e) => e.tagName === 'A')[0].getAttribute('href'), '/approvals');
const pill = all(main, (e) => (e.getAttribute('class') || '').startsWith('pill '))[0];
assert.equal(pill.textContent, 'Autopilot on');
assert.equal(pill.getAttribute('class'), 'pill pill-on');
const decide = byId(main, 'agent-decide');
assert.ok(decide.textContent.includes('What agents can decide'));
assert.ok(decide.textContent.includes('Agents may approve and reject requests for Create versions, Deploy, Delete versions and Make Public. Every other request waits for you.'), decide.textContent);
assert.equal(all(decide, (e) => e.tagName === 'A')[0].getAttribute('href'), '/settings/experiments');
assert.ok(!main.textContent.includes('Approve requests'));
assert.ok(main.textContent.includes('Decide other requests') && main.textContent.includes('Agent tools decide only the operations listed above; you decide the rest under Approvals.'));
assert.ok(byId(main, 'agent-limit-experiments'));
stop();

reset({ enabled: true, allowed: null });
({ main, stop } = await open('agents'));
assert.equal(intro(main).textContent, BEGIN + 'Autopilot is on, but it allows no operation yet, so every request to save a version, deploy, change visibility or delete waits for your approval under Approvals.');
assert.ok(byId(main, 'agent-decide').textContent.includes('None yet.'));
assert.ok(main.textContent.includes('Autopilot on'));
stop();

// Without Autopilot's state the page is today's, with an error notice.
answers['GET ' + BASE] = { status: 500, body: { error: 'storage unavailable' } };
({ main, stop } = await open('agents'));
assert.equal(intro(main).textContent, BEGIN + 'Saving versions, deploying, visibility changes and deleting wait for your approval under Approvals.');
assert.ok(main.textContent.includes('Cannot load Autopilot') && main.textContent.includes('storage unavailable'));
assert.ok(main.textContent.includes('Approve requests') && byId(main, 'agent-limit-experiments'));
stop();

// --- approval page "Decided by" (rule 27) ---
const agent = (fields) => 'autopilot:' + JSON.stringify({ agent: 'pr-bot', ...fields });
const DECIDERS = [
  ['console', 'the console'],
  ['alice@example.com', 'alice@example.com'],
  ['netbird:laptop.netbird.cloud', 'NetBird peer laptop.netbird.cloud'],
  ['operator', 'operator'],
  [agent({ network: 'local' }), 'Autopilot: agent "pr-bot" via Local'],
  [agent({ network: 'listener', ip: '192.168.1.20' }), 'Autopilot: agent "pr-bot" via the console listener from 192.168.1.20'],
  [agent({ network: 'listener', ip: 'fe80::1%en0' }), 'Autopilot: agent "pr-bot" via the console listener from fe80::1%en0'],
  [agent({ network: 'tailscale', login: 'bot@example.com' }), 'Autopilot: agent "pr-bot" via Tailscale (bot@example.com)'],
  [agent({ network: 'tailscale', node: 'ci-runner' }), 'Autopilot: agent "pr-bot" via Tailscale node ci-runner'],
  [agent({ network: 'netbird', peer: 'ci.netbird.cloud' }), 'Autopilot: agent "pr-bot" via NetBird peer ci.netbird.cloud'],
  [agent({ network: 'netbird' }), 'Autopilot: agent "pr-bot" via NetBird'],
  [agent({ network: 'external', origin: 'https://flats.example.com' }), 'Autopilot: agent "pr-bot" via external origin https://flats.example.com'],
  [agent({ network: 'local', extra: 1 }), 'Autopilot: agent "pr-bot" via Local'],
];
// Malformed Autopilot values are shown as stored.
const MALFORMED = ['autopilot:', 'autopilot:{', 'autopilot:null', 'autopilot:[]', 'autopilot:"pr-bot"',
  'autopilot:{"network":"local"}', 'autopilot:{"agent":"  ","network":"local"}', 'autopilot:{"agent":7,"network":"local"}',
  'autopilot:{"agent":"pr-bot"}', 'autopilot:{"agent":"pr-bot","network":"carrier-pigeon"}', 'autopilot:{"agent":"pr-bot","network":"toString"}',
  'autopilot:{"agent":"pr-bot","network":"tailscale","login":5}', 'autopilot:{"agent":"pr-bot","network":"listener","ip":null}'];
for (const [value, text] of [...DECIDERS, ...MALFORMED.map((v) => [v, v])]) assert.equal(decidedBy(value), text, value);

const decidedRow = async (value) => {
  approvals['apr-1'] = { id: 'apr-1', flat: 'gone', action: 'deploy', status: 'approved', via: 'mcp', requested_at: '2026-10-24T09:00:00Z',
    decided_at: '2026-10-24T09:01:00Z', ...(value === undefined ? {} : { decided_by: value }), params: { version: 2 }, result: 'version 2 is live' };
  const page = document.createElement('main');
  approvalPage(page, ['apr-1'], ctx);
  await tick();
  const dts = all(page, (e) => e.tagName === 'DT').map((e) => e.textContent);
  const i = dts.indexOf('Decided by');
  return { page, dts, dd: i < 0 ? null : all(page, (e) => e.tagName === 'DD')[i] };
};
for (const [value, text] of DECIDERS) {
  const { dts, dd } = await decidedRow(value);
  assert.equal(dts[dts.indexOf('Decided by') - 1], 'Decided', 'right after Decided');
  assert.ok(dd.textContent.startsWith(text), dd.textContent);
  assert.equal(dd.textContent.includes('Flats did not verify the name'), value.startsWith('autopilot:'), value);
}
for (const value of MALFORMED) assert.equal((await decidedRow(value)).dd.textContent, value);
assert.equal((await decidedRow(undefined)).dd, null, 'no row without a decider');
// An agent name is text, never markup; a newline stays escaped.
let shown = await decidedRow(agent({ agent: '<b>bot</b>\nnext', network: 'local' }));
assert.equal(shown.dd.textContent.split('The agent')[0], 'Autopilot: agent "<b>bot</b>\\nnext" via Local');
assert.equal(all(shown.page, (e) => e.tagName === 'B').length, 0);
shown = await decidedRow('<img src=x onerror=alert(1)>');
assert.equal(all(shown.page, (e) => e.tagName === 'IMG').length, 0);

// The approvals list does not change.
const table = approvalsTable([{ id: 'apr-1', flat: 'blog', action: 'deploy', status: 'approved', via: 'mcp', requested_at: '2026-10-24T09:00:00Z',
  params: { version: 2 }, decided_by: agent({ network: 'local' }) }]);
assert.deepEqual(all(table, (e) => e.tagName === 'TH' && e.getAttribute('scope') === 'col').map((e) => e.textContent), ['Request', 'Via', 'Requested', 'Status']);
assert.ok(!table.textContent.includes('Autopilot') && !table.textContent.includes('pr-bot'));

console.log('experiments ok');
