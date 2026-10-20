#!/usr/bin/env node
// Board canvas end-to-end checks in headless Chrome over the DevTools
// Protocol (board-TECH.md section 14, "Browser automation"). It drives a
// running Flats host: boards are created with the flats CLI, edited through
// real mouse, keyboard and IME input events, and judged against the page
// state, the server (CLI reads) and downloaded files. Synthetic data only.
//
// Usage: node scripts/board-e2e/run.mjs --console http://127.0.0.1:17930 --flats ./flats
// See README.md. Exit status: 0 when every check passed, 1 otherwise.

import { parseArgs } from 'node:util';
import { spawnSync } from 'node:child_process';
import { mkdirSync, writeFileSync, readFileSync, existsSync, rmSync, mkdtempSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { tmpdir } from 'node:os';
import http from 'node:http';
import { launchChrome, Tab, sleep } from './lib/cdp.mjs';
import { makePNG, readPNG, readZip, decodePNG } from './lib/files.mjs';
import { PIXEL_BOARD, pixelErrors } from './lib/pixels.mjs';

const { values: opt } = parseArgs({
  options: {
    console: { type: 'string', default: 'http://127.0.0.1:7878' },
    flats: { type: 'string', default: 'flats' },
    chrome: { type: 'string', default: '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome' },
    out: { type: 'string' },
    only: { type: 'string' },
    skip: { type: 'string' },
    public: { type: 'string', default: 'off' },
    headful: { type: 'boolean', default: false },
    help: { type: 'boolean', default: false },
  },
});
if (opt.help) {
  console.log(readFileSync(new URL('./README.md', import.meta.url), 'utf8'));
  process.exit(0);
}

const SECTIONS = ['setup', 'tools', 'select', 'keys', 'text', 'collab', 'a11y', 'find', 'share', 'png', 'data', 'history', 'converge', 'public'];
const only = opt.only ? new Set(['setup', ...opt.only.split(',')]) : null;
const skip = new Set(opt.skip ? opt.skip.split(',') : []);
if (opt.public === 'off') skip.add('public');
const enabled = (s) => (!only || only.has(s)) && !skip.has(s);

const OUT = resolve(opt.out || mkdtempSync(join(tmpdir(), 'board-e2e-')));
const DL = join(OUT, 'downloads');
mkdirSync(DL, { recursive: true });
const WORK = mkdtempSync(join(tmpdir(), 'board-e2e-work-'));
const RUN = Math.random().toString(36).slice(2, 7).replace(/^[0-9]/, 'x');
const slugOf = (s) => `e2e-${RUN}-${s}`;

// ---- results ----

const results = [];
const interventions = [];
let currentSection = '';

async function section(name, fn) {
  if (!enabled(name)) {
    results.push({ section: name, name: `${name} (section)`, status: 'skip', detail: opt.public === 'off' && name === 'public' ? 'Public checks need --public adapter (a host with a loopback public provider)' : 'not selected' });
    return;
  }
  currentSection = name;
  try { await fn(); } catch (err) {
    results.push({ section: name, name: `${name}: section aborted`, status: 'fail', detail: err.stack || String(err) });
  }
}

// In these sections every check edits the main board through the open
// editors, so each one ends with a server cross-check: what the editors show
// as saved must be what the server holds (agreesWithServer).
const SERVER_CHECKED = new Set(['tools', 'select', 'keys', 'text', 'collab', 'a11y', 'history']);

async function check(name, fn) {
  const t0 = Date.now();
  try {
    let detail = await fn();
    if (SERVER_CHECKED.has(currentSection)) detail = [detail, await agreesWithServer()].filter((d) => d != null && d !== '').join('; ');
    results.push({ section: currentSection, name, status: 'pass', detail: detail == null ? '' : String(detail), ms: Date.now() - t0 });
    console.log(`PASS  ${name}${detail ? ` — ${detail}` : ''}`);
    return true;
  } catch (err) {
    const shot = join(OUT, `fail-${results.length + 1}.png`);
    for (const tab of [A, B, P, C]) if (tab) { try { await tab.screenshot(shot.replace('.png', `-${tab.name}.png`)); } catch { /* ignore */ } }
    results.push({ section: currentSection, name, status: 'fail', detail: err.message, ms: Date.now() - t0 });
    console.log(`FAIL  ${name} — ${err.message}`);
    return false;
  }
}

function assert(cond, msg) { if (!cond) throw new Error(msg); }
const need = (v, what) => { if (v == null) throw new Error(`prerequisite missing: ${what}`); return v; };
const near = (a, b, tol = 1.5) => Math.abs(a - b) <= tol;

// ---- flats CLI ----

function cli(args, { json = false } = {}) {
  const r = spawnSync(opt.flats, ['--url', opt.console, ...(json ? ['--json'] : []), ...args], { encoding: 'utf8', timeout: 120000 });
  return { code: r.status, out: r.stdout || '', err: r.stderr || '' };
}

function cliJSON(args) {
  const r = cli(args, { json: true });
  if (r.code !== 0) throw new Error(`flats ${args.join(' ')} exited ${r.code}: ${r.err || r.out}`);
  return JSON.parse(r.out);
}

let opSeq = 0;
function agentApply(slug, body, message = 'e2e agent write') {
  const file = join(WORK, `ops-${++opSeq}.json`);
  writeFileSync(file, JSON.stringify(body));
  const r = cli(['board', 'apply', slug, file, '--message', message]);
  if (r.code !== 0) throw new Error(`board apply ${slug} exited ${r.code}: ${r.err || r.out}`);
  return r.out.trim();
}

const boardGet = (slug) => cliJSON(['board', 'get', slug]);

// httpStatus requests a loopback URL directly (no browser cache), sending
// the *.localhost Host header that the local network routes by.
function httpStatus(url) {
  const u = new URL(url);
  return new Promise((resolve) => {
    const req = http.request({ host: '127.0.0.1', port: u.port, path: u.pathname + u.search, method: 'GET', headers: { Host: u.host } }, (res) => { res.resume(); resolve(res.statusCode); });
    req.on('error', (e) => resolve(String(e.code || e)));
    req.end();
  });
}

// ---- page helpers ----

// HELPERS installs read-only test instrumentation: a toast recorder and
// accessors for the canvas app's state and coordinates.
const HELPERS = () => {
  if (window.__e2e) return true;
  const keys = ['id', 'type', 'x', 'y', 'w', 'h', 'rot', 'z', 'frame', 'group', 'locked', 'text', 'style', 'ends', 'alt', 'asset', 'natural', 'pts'];
  const strip = (e) => {
    if (!e) return null;
    const o = {};
    for (const k of keys) if (e[k] !== undefined) o[k] = e[k];
    return JSON.parse(JSON.stringify(o));
  };
  const toasts = [];
  new MutationObserver((ml) => {
    for (const m of ml) for (const n of m.addedNodes) {
      if (n.nodeType !== 1) continue;
      const t = n.querySelector('.toast-text');
      toasts.push({ text: (t || n).textContent, actions: [...n.querySelectorAll('button')].map((b) => b.textContent).filter(Boolean), at: Date.now() });
    }
  }).observe(document.querySelector('.toasts'), { childList: true });
  window.__e2e = {
    toasts, strip,
    els: () => [...board.state.all()].map(strip),
    server: () => [...board.state.server.values()].map(strip),
    ids: () => [...board.state.server.keys()],
    get: (id) => strip(board.state.get(id)),
    client(x, y) {
      const r = board.renderer.dyn.getBoundingClientRect();
      const [sx, sy] = board.toScreen(x, y);
      return [r.left + sx, r.top + sy];
    },
    center(id) {
      const el = board.doc.get(id);
      if (!el) return null;
      if (el.type === 'connector' || el.type === 'pen') {
        const b = board.doc.index.bounds(id);
        return window.__e2e.client(b.x + b.w / 2, b.y + b.h / 2);
      }
      return window.__e2e.client(el.x + el.w / 2, el.y + el.h / 2);
    },
    // pick returns a client point where a click reaches the canvas (no
    // toolbar, panel or popover above it) and lands on this element. It is
    // only a targeting aid; checks judge results from state.
    pick(id, locked) {
      const find = () => {
        const el = board.doc.get(id);
        if (!el) return null;
        const b = board.doc.index.bounds(id);
        const z = board.cam.z;
        const cands = [];
        if (el.type === 'frame') for (const t of [0.15, 0.5, 0.85]) cands.push([el.x + el.w * t, el.y - 10 / z]);
        if (el.type !== 'connector' && el.type !== 'pen') cands.push([el.x + el.w / 2, el.y + el.h / 2]);
        cands.push([b.x + b.w / 2, b.y + b.h / 2]);
        for (let i = 1; i < 10; i++) for (let j = 1; j < 10; j++) cands.push([b.x + (b.w * i) / 10, b.y + (b.h * j) / 10]);
        for (const [x, y] of cands) {
          const [cx, cy] = window.__e2e.client(x, y);
          const top = document.elementFromPoint(cx, cy);
          if (!top || !top.classList.contains('layer-dynamic')) continue;
          const h = board.hitAt(x, y, { locked: !!locked });
          if (h && h.id === id) return [cx, cy];
        }
        return null;
      };
      let p = find();
      if (!p && board.doc.get(id)) {
        const b = board.doc.index.bounds(id);
        board.setCam(b.x + b.w / 2 - board.renderer.w / 2 / board.cam.z, b.y + b.h / 2 - board.renderer.h / 2 / board.cam.z, board.cam.z);
        p = find();
      }
      return p;
    },
    handle(kind) {
      const r = board.renderer.dyn.getBoundingClientRect();
      const hd = board.tools.select.handles().find((x) => x.kind === kind);
      return hd ? [r.left + hd.at[0], r.top + hd.at[1]] : null;
    },
    view: () => board.renderer.viewRect(),
    settled: () => !!(board.welcomed && board.net.connected && board.state.pending.length === 0),
    status: () => document.querySelector('.save').textContent,
    sel: () => [...board.sel],
  };
  return true;
};

async function install(tab) { await tab.call(HELPERS); }

async function settle(tab, timeout = 15000) {
  await tab.until(() => window.__e2e && __e2e.settled(), [], { timeout, desc: `${tab.name}: all edits confirmed` });
}

// openBoard opens a board URL, answers the name prompt of unverified
// routes and waits until the tab can edit (or is a public viewer).
async function openBoard(tab, url, name) {
  await tab.goto(url);
  await tab.waitFor('window.board && board.welcomed', { timeout: 15000, desc: `${tab.name}: welcome` });
  await install(tab);
  const access = await tab.ev('board.access');
  if (access !== 'private') return access;
  const prompt = await tab.ev('!!document.querySelector("dialog[open] #name-input")');
  if (prompt) {
    await tab.until(() => document.activeElement && document.activeElement.id === 'name-input', [], { desc: 'name input focused' });
    await tab.insertText(name);
    await tab.key('Enter');
  }
  // The dialog is removed (and focus leaves its input) on its async close event.
  await tab.until(() => board.canEdit && board.net.connected && !document.querySelector('dialog'), [], { timeout: 15000, desc: `${tab.name}: can edit` });
  return access;
}

async function newElements(tab, before, type, { timeout = 10000, count = 1 } = {}) {
  return tab.until((before, type, count) => {
    const n = [...board.state.server.values()].filter((e) => !before.includes(e.id) && (!type || e.type === type));
    return n.length >= count ? n.map(__e2e.strip) : null;
  }, [before, type, count], { timeout, desc: `${tab.name}: new ${type || 'element'} confirmed by the server` });
}

const ids = (tab) => tab.ev('__e2e.ids()');
const el = (tab, id) => tab.call((id) => __e2e.get(id), id);
const pt = (tab, x, y) => tab.call((x, y) => __e2e.client(x, y), x, y);
const trueCenter = async (tab, id) => need(await tab.call((id) => __e2e.center(id), id), `on-screen position of ${id}`);
const centerOf = async (tab, id, locked = false) => need(await tab.call((id, l) => __e2e.pick(id, l), id, locked), `a visible, clickable point of ${id}`);
const toastSeen = (tab, text, timeout = 8000) => tab.until((t) => __e2e.toasts.find((x) => x.text.includes(t)), [text], { timeout, desc: `${tab.name}: notice "${text}"` });

async function resetView(tab) {
  if (ctx.cam) await tab.call((c) => board.setCam(c.x, c.y, c.z), ctx.cam);
}

async function selectOnly(tab, id) {
  await tab.key('Escape');
  const c = await centerOf(tab, id);
  await tab.click(c[0], c[1]);
  await tab.until((id) => board.sel.has(id), [id], { desc: `${tab.name}: ${id} selected` });
}

// createWithTool places one box element with a tool key and an optional
// text typed into the in-place editor.
async function createWithTool(tab, key, world, text) {
  const before = await ids(tab);
  await tab.key(key);
  const [x, y] = await pt(tab, world[0], world[1]);
  await tab.click(x, y);
  if (text != null) {
    await tab.until(() => !!board.editor, [], { desc: 'text editor opened' });
    await tab.insertText(text);
    await tab.key('Enter', { modifiers: ['ctrl'] });
  }
  const [n] = await newElements(tab, before);
  await settle(tab);
  return el(tab, n.id);
}

// normalized compares content as every screen and the CLI must see it.
function normalized(list) {
  const r1 = (v) => (typeof v === 'number' ? Math.round(v * 10) / 10 : v);
  return list.map((e) => {
    const o = { id: e.id, type: e.type, z: e.z, frame: e.frame || null, group: e.group || null, locked: !!e.locked, text: e.text || '', style: e.style || {}, alt: e.alt || '' };
    if (e.type !== 'connector') Object.assign(o, { x: r1(e.x), y: r1(e.y), w: r1(e.w), h: r1(e.h), rot: r1(e.rot || 0) });
    else o.ends = e.ends;
    return o;
  }).sort((a, b) => (a.id < b.id ? -1 : 1));
}

function diffStates(a, b) {
  const ma = new Map(a.map((e) => [e.id, JSON.stringify(e)]));
  const mb = new Map(b.map((e) => [e.id, JSON.stringify(e)]));
  const out = [];
  for (const [id, v] of ma) if (mb.get(id) !== v) out.push(`${id}: ${v} vs ${mb.get(id) || 'missing'}`);
  for (const id of mb.keys()) if (!ma.has(id)) out.push(`${id}: missing vs present`);
  return out;
}

const texts = (list) => list.map((e) => `${e.type}:${e.text || e.alt || ''}`).sort();

// agreesWithServer waits until every editor tab on the main board has no
// unsent edits and shows as saved exactly the elements that the server
// returns to `flats board get`: ids, types, geometry, stacking keys, frames,
// groups, locks, text and styles. The editors' state only says what they
// believe the server holds; the CLI reads the server itself.
async function agreesWithServer({ timeout = 10000 } = {}) {
  const tabs = [];
  for (const t of [A, B]) if (t && (await t.ev('location.href')).startsWith(ctx.url)) tabs.push(t);
  if (!tabs.length) throw new Error('server cross-check: no editor tab is on the main board');
  const end = Date.now() + timeout;
  for (;;) {
    for (const t of tabs) await settle(t);
    const server = normalized(boardGet(ctx.slug).elements);
    const diffs = [];
    for (const t of tabs) {
      const d = diffStates(normalized(await t.ev('__e2e.server()')), server);
      if (d.length) diffs.push(`${t.name}: ${d.slice(0, 3).join('; ')}${d.length > 3 ? ` (+${d.length - 3})` : ''}`);
    }
    if (!diffs.length) return `server agrees with ${tabs.map((t) => t.name).join(', ')} (${server.length} elements)`;
    if (Date.now() > end) throw new Error(`server cross-check: the saved screen and flats board get differ: ${diffs.join(' | ')}`);
    await sleep(300);
  }
}

// ---- downloads ----

const completed = new Map();
async function download(trigger) {
  const begin = browser.cdp.waitEvent('Browser.downloadWillBegin', { timeout: 20000 });
  await trigger();
  const b = await begin;
  const end = Date.now() + 30000;
  while (!completed.has(b.guid) && Date.now() < end) await sleep(50);
  const state = completed.get(b.guid);
  assert(state === 'completed', `download ${b.suggestedFilename} ended as ${state || 'timeout'}`);
  return { path: join(DL, b.guid), name: b.suggestedFilename };
}

// ---- main ----

let browser, A, B, P, C, ctxA, ctxB;
const ctx = {};

async function main() {
  console.log(`board-e2e: host ${opt.console}, output ${OUT}`);
  browser = await launchChrome({ chrome: opt.chrome, headless: !opt.headful });
  ctx.chromeVersion = browser.version.product;
  console.log(`browser ${browser.version.product}`);
  browser.cdp.on('Browser.downloadProgress', (p) => { if (p.state !== 'inProgress') completed.set(p.guid, p.state); });
  ctxA = (await browser.cdp.send('Target.createBrowserContext', { disposeOnDetach: true })).browserContextId;
  ctxB = (await browser.cdp.send('Target.createBrowserContext', { disposeOnDetach: true })).browserContextId;
  for (const id of [ctxA, ctxB]) {
    await browser.cdp.send('Browser.setDownloadBehavior', { behavior: 'allowAndName', downloadPath: DL, eventsEnabled: true, browserContextId: id });
  }
  A = await Tab.open(browser.cdp, { browserContextId: ctxA, name: 'A' });

  await section('setup', setup);
  if (!ctx.url) throw new Error('setup failed; nothing else can run');
  await section('tools', tools);
  await section('select', selection);
  await section('keys', keyboard);
  await section('text', textEditing);
  await section('collab', collaboration);
  await section('a11y', accessibility);
  await section('find', find);
  await section('share', share);
  await section('png', pngExport);
  await section('data', dataRoundTrip);
  await section('history', history);
  await section('converge', convergence);
  await section('public', publicBoundary);
}

// ---- setup ----

async function setup() {
  ctx.slug = slugOf('main');
  await check('setup: the CLI creates a board (update_board on a new slug)', async () => {
    const out = agentApply(ctx.slug, { ops: [{ op: 'add', el: { type: 'sticky', text: 'Seed from agent', x: -400, y: -300 } }] }, 'seed');
    const info = cliJSON(['info', ctx.slug]);
    assert(info.type === 'board', `type is ${info.type}`);
    assert(info.private_url, 'no private URL');
    ctx.url = info.private_url.replace(/\/$/, '') + '/';
    return `${out}; ${ctx.url}`;
  });
  if (!ctx.url) return;
  await check('setup: the canvas opens in headless Chrome and asks for a name on the Local route (rule 23)', async () => {
    await A.goto(ctx.url);
    await A.waitFor('window.board && board.welcomed', { timeout: 15000 });
    await install(A);
    const me = await A.ev('board.me');
    assert(me && me.verified === false, `expected an unverified identity, got ${JSON.stringify(me)}`);
    await A.waitFor('!!document.querySelector("dialog[open] #name-input")', { desc: 'name prompt' });
    assert(!(await A.ev('board.canEdit')), 'editing allowed before a name was chosen');
    await A.insertText('Tester A');
    await A.key('Enter');
    await A.until(() => board.canEdit && board.net.connected && !document.querySelector('dialog'), [], { timeout: 15000, desc: 'editing enabled' });
    const chip = await A.ev('document.querySelector("#access-chip").textContent');
    assert(chip === 'Private', `access chip says ${chip}`);
    const seed = (await A.ev('__e2e.server()')).find((e) => e.text === 'Seed from agent');
    assert(seed, 'the agent seed sticky is not on the canvas');
    ctx.seed = seed.id;
    return `editor key ${(await A.ev('board.me.key'))}, name ${(await A.ev('board.me.name'))}`;
  });
  await check('setup: save status shows Saved when connected with nothing pending (rule 35, TECH 11.3)', async () => {
    await settle(A);
    const s = await A.ev('__e2e.status()');
    assert(s === 'Saved', `status is "${s}"`);
    return s;
  });
  // Work at 50% so the whole test layout fits on screen; later checks
  // return here with resetView().
  // The agent seed sits in the bottom-right corner of the layout, clear of
  // the elements the checks create.
  const seed = await el(A, need(ctx.seed, 'seed'));
  await A.call((x, y) => board.setCam(x, y, 0.5), seed.x - 2600, seed.y - 1300);
  ctx.cam = await A.ev('({ ...board.cam })');
  ctx.v = await A.ev('__e2e.view()');
}

// ---- tools (rule 14) ----

async function tools() {
  const v = need(ctx.v, 'view');
  const W = (dx, dy) => [v.x + dx, v.y + dy];
  await check('tools: S + click creates a sticky and typed text is saved', async () => {
    const s = await createWithTool(A, 's', W(250, 200), 'Alpha sticky');
    assert(s.type === 'sticky', `type ${s.type}`);
    assert(s.text === 'Alpha sticky', `text "${s.text}"`);
    ctx.sticky = s.id;
    return `${s.id} ${s.w}x${s.h}`;
  });
  await check('tools: T + click creates a text element', async () => {
    const t = await createWithTool(A, 't', W(250, 560), 'Free text line');
    assert(t.type === 'text' && t.text === 'Free text line', JSON.stringify(t));
    ctx.text = t.id;
    return t.id;
  });
  await check('tools: R, O and D create a rectangle, an ellipse and a diamond', async () => {
    const r = await createWithTool(A, 'r', W(620, 200), 'Box');
    const o = await createWithTool(A, 'o', W(620, 400), 'Oval');
    const d = await createWithTool(A, 'd', W(620, 600), 'Gem');
    const shape = (e) => (e.style && e.style.shape) || 'rect';
    assert(r.type === 'shape' && shape(r) === 'rect', `R made ${r.type}/${shape(r)}`);
    assert(o.type === 'shape' && shape(o) === 'ellipse', `O made ${o.type}/${shape(o)}`);
    assert(d.type === 'shape' && shape(d) === 'diamond', `D made ${d.type}/${shape(d)}`);
    Object.assign(ctx, { rect: r.id, ellipse: o.id, diamond: d.id });
    return `${r.id} ${o.id} ${d.id}`;
  });
  await check('tools: L + drag from the sticky to the rectangle creates a connector attached at both ends (rule 4)', async () => {
    const before = await ids(A);
    await A.key('l');
    const a = await centerOf(A, need(ctx.sticky, 'sticky'));
    const b = await centerOf(A, need(ctx.rect, 'rectangle'));
    await A.drag(a, b);
    const [c] = await newElements(A, before, 'connector');
    await settle(A);
    assert(c.ends[0].el === ctx.sticky, `start end is ${JSON.stringify(c.ends[0])}`);
    assert(c.ends[1].el === ctx.rect, `end is ${JSON.stringify(c.ends[1])}`);
    ctx.conn = c.id;
    return `${c.id}: ${JSON.stringify(c.ends)}`;
  });
  await check('tools: P + drag draws a pen stroke', async () => {
    const before = await ids(A);
    await A.key('p');
    const path = [];
    for (let i = 0; i <= 16; i++) path.push(await pt(A, v.x + 860 + i * 10, v.y + 160 + (i % 2 ? 40 : 0)));
    await A.drag(path[0], path.at(-1), { path: path.slice(1) });
    const [p] = await newElements(A, before, 'pen');
    await settle(A);
    assert(Array.isArray(p.pts) && p.pts.length >= 4, `pts ${JSON.stringify(p.pts)}`);
    ctx.pen = p.id;
    ctx.penPath = path;
    return `${p.id} with ${p.pts.length / 2} points`;
  });
  await check('tools: F + drag creates a frame', async () => {
    const before = await ids(A);
    await A.key('f');
    await A.drag(await pt(A, ...W(860, 330)), await pt(A, ...W(1240, 700)));
    const [f] = await newElements(A, before, 'frame');
    await settle(A);
    assert(f.w > 300 && f.h > 300, `frame ${f.w}x${f.h}`);
    ctx.frame = f.id;
    ctx.frameName = f.text;
    return `${f.id} "${f.text}" ${Math.round(f.w)}x${Math.round(f.h)}`;
  });
  await check('tools: a sticky created inside a frame becomes its member (rule 3)', async () => {
    const s = await createWithTool(A, 's', W(1050, 520), 'Inside frame');
    assert(s.frame === need(ctx.frame, 'frame'), `frame is ${s.frame}`);
    ctx.member = s.id;
    return `${s.id} in ${s.frame}`;
  });
  await check('tools: I opens a file chooser and an uploaded PNG becomes an image element', async () => {
    const png = join(WORK, 'gradient.png');
    writeFileSync(png, makePNG(64, 48, 3));
    await A.mouse('mouseMoved', ...(await pt(A, ...W(420, 640))), { buttons: 0 });
    const before = await ids(A);
    await A.chooseFiles([png], () => A.key('i'));
    const [img] = await newElements(A, before, 'image', { timeout: 15000 });
    await settle(A);
    assert(/^[0-9a-f]{64}$/.test(img.asset), `asset ${img.asset}`);
    assert(JSON.stringify(img.natural) === '[64,48]', `natural ${JSON.stringify(img.natural)}`);
    const r = await A.call(async (h) => { const x = await fetch('/_board/a/' + h); return [x.status, x.headers.get('content-type')]; }, img.asset);
    assert(r[0] === 200 && r[1] === 'image/png', `asset GET ${r}`);
    ctx.image = img.id;
    ctx.asset = img.asset;
    return `${img.id} asset ${img.asset.slice(0, 12)}…`;
  });
  await check('tools: E + drag across a pen stroke erases the whole stroke', async () => {
    need(ctx.pen, 'pen');
    await A.key('e');
    const path = need(ctx.penPath, 'pen path');
    await A.drag(path[0], path.at(-1), { path: path.slice(1) });
    await A.until((id) => !board.state.server.has(id), [ctx.pen], { desc: 'pen removed' });
    await settle(A);
    return 'pen deleted';
  });
  await check('tools: undo restores the erased stroke with its original id (rule 21)', async () => {
    await A.key('v');
    await A.key('z', { modifiers: ['ctrl'] });
    await A.until((id) => board.state.server.has(id), [ctx.pen], { desc: 'pen back' });
    await settle(A);
    return ctx.pen;
  });
  await check('tools: H + drag pans the view without changing elements', async () => {
    const before = JSON.stringify(normalized(await A.ev('__e2e.server()')));
    const cam0 = await A.ev('({...board.cam})');
    await A.key('h');
    await A.drag(await pt(A, ...W(500, 760)), await pt(A, ...W(400, 700)));
    const cam1 = await A.ev('({...board.cam})');
    await A.key('v');
    assert(near(cam1.x - cam0.x, 100, 2) && near(cam1.y - cam0.y, 60, 2), `camera moved ${cam1.x - cam0.x}, ${cam1.y - cam0.y}`);
    assert(JSON.stringify(normalized(await A.ev('__e2e.server()'))) === before, 'elements changed while panning');
    await resetView(A);
    return `camera moved by (${Math.round(cam1.x - cam0.x)}, ${Math.round(cam1.y - cam0.y)})`;
  });
  await A.screenshot(join(OUT, 'tools.png'));
  await check('theme: the canvas follows the light and dark color scheme (rule 12)', async () => {
    const lum = async () => {
      await sleep(300);
      return A.ev(`(() => { const c = document.querySelector('canvas.layer-static'); const d = c.getContext('2d').getImageData(Math.floor(c.width * 0.97), Math.floor(c.height * 0.03), 1, 1).data;
        return Math.round((0.2126 * d[0] + 0.7152 * d[1] + 0.0722 * d[2]) / 2.55); })()`);
    };
    const out = {};
    for (const scheme of ['light', 'dark']) {
      await A.send('Emulation.setEmulatedMedia', { features: [{ name: 'prefers-color-scheme', value: scheme }] });
      await A.call(() => board.renderer.invalidateAll());
      out[scheme] = await lum();
      if (scheme === 'light') await A.screenshot(join(OUT, 'tools-light.png'));
    }
    await A.send('Emulation.setEmulatedMedia', { features: [] });
    assert(out.light > 80 && out.dark < 20, `background luminance light ${out.light}%, dark ${out.dark}%`);
    return `background luminance light ${out.light}%, dark ${out.dark}%`;
  });
}

// ---- selection, move, resize, rotate, group, z-order, delete (rule 15) ----

async function selection() {
  const v = need(ctx.v, 'view');
  await check('select: click selects, Shift+click adds, click on empty canvas clears', async () => {
    await selectOnly(A, need(ctx.sticky, 'sticky'));
    const c = await centerOf(A, need(ctx.rect, 'rect'));
    await A.click(c[0], c[1], { modifiers: ['shift'] });
    const sel = await A.ev('__e2e.sel()');
    assert(sel.length === 2 && sel.includes(ctx.sticky) && sel.includes(ctx.rect), `selection ${sel}`);
    await A.click(...(await pt(A, v.x + 420, v.y + 80)));
    assert((await A.ev('__e2e.sel()')).length === 0, 'selection not cleared');
    return 'ok';
  });
  await check('select: area drag selects the elements fully inside the area', async () => {
    await A.drag(await pt(A, v.x + 110, v.y + 70), await pt(A, v.x + 400, v.y + 620));
    const sel = (await A.ev('__e2e.sel()')).sort();
    const want = [ctx.sticky, ctx.text].sort();
    assert(JSON.stringify(sel) === JSON.stringify(want), `selected ${sel}, want ${want}`);
    return sel.join(', ');
  });
  await check('select: dragging moves the element; its attached connector end follows (rule 4)', async () => {
    await selectOnly(A, ctx.sticky);
    const s0 = await el(A, ctx.sticky);
    const c = await centerOf(A, ctx.sticky);
    const z = await A.ev('board.cam.z');
    await A.drag(c, [c[0] + 40, c[1] + 24], { modifiers: ['alt'] });
    await A.until((id, x) => board.state.server.get(id).x !== x, [ctx.sticky, s0.x], { desc: 'move confirmed' });
    await settle(A);
    const s1 = await el(A, ctx.sticky);
    assert(near(s1.x - s0.x, 40 / z, 2) && near(s1.y - s0.y, 24 / z, 2), `moved by (${s1.x - s0.x}, ${s1.y - s0.y}) at zoom ${z}`);
    const conn = await el(A, need(ctx.conn, 'connector'));
    assert(conn.ends[0].el === ctx.sticky, 'connector end detached by the move');
    return `(${s0.x}, ${s0.y}) → (${s1.x}, ${s1.y})`;
  });
  await check('select: dragging the corner handle resizes', async () => {
    await selectOnly(A, ctx.rect);
    const r0 = await el(A, ctx.rect);
    const hd = need(await A.call((k) => __e2e.handle(k), 'se'), 'se handle');
    const z = await A.ev('board.cam.z');
    await A.drag(hd, [hd[0] + 60, hd[1] + 40]);
    await A.until((id, w) => board.state.server.get(id).w !== w, [ctx.rect, r0.w], { desc: 'resize confirmed' });
    await settle(A);
    const r1 = await el(A, ctx.rect);
    assert(near(r1.w - r0.w, 60 / z, 3) && near(r1.h - r0.h, 40 / z, 3) && near(r1.x, r0.x) && near(r1.y, r0.y), `${JSON.stringify(r0)} → ${JSON.stringify(r1)}`);
    return `${r0.w}x${r0.h} → ${r1.w}x${r1.h}`;
  });
  await check('select: dragging the rotation handle rotates', async () => {
    await selectOnly(A, ctx.diamond);
    const hd = need(await A.call((k) => __e2e.handle(k), 'rot'), 'rotation handle');
    const c = await trueCenter(A, ctx.diamond);
    await A.drag(hd, [c[0] + 150, c[1]]);
    await A.until((id) => (board.state.server.get(id).rot || 0) !== 0, [ctx.diamond], { desc: 'rotation confirmed' });
    await settle(A);
    const d = await el(A, ctx.diamond);
    assert(near(d.rot, 90, 3), `rot ${d.rot}`);
    return `rot ${d.rot}`;
  });
  await check('select: Ctrl+G groups, a click then selects the whole group, Ctrl+Shift+G ungroups (rule 15)', async () => {
    await selectOnly(A, ctx.sticky);
    const c = await centerOf(A, ctx.text);
    await A.click(c[0], c[1], { modifiers: ['shift'] });
    await A.key('g', { modifiers: ['ctrl'] });
    await A.until((a, b) => board.state.server.get(a).group && board.state.server.get(a).group === board.state.server.get(b).group, [ctx.sticky, ctx.text], { desc: 'group confirmed' });
    await settle(A);
    const g = (await el(A, ctx.sticky)).group;
    await A.key('Escape');
    await A.key('Escape');
    const s = await centerOf(A, ctx.sticky);
    await A.click(s[0], s[1]);
    const sel = await A.ev('__e2e.sel()');
    assert(sel.length === 2 && sel.includes(ctx.text), `click on a member selected ${sel}`);
    await A.key('g', { modifiers: ['ctrl', 'shift'] });
    await A.until((a, b) => !board.state.server.get(a).group && !board.state.server.get(b).group, [ctx.sticky, ctx.text], { desc: 'ungroup confirmed' });
    await settle(A);
    return `group ${g}`;
  });
  await check('select: Ctrl+] brings to front and Ctrl+[ sends to back (stacking order)', async () => {
    const order = async () => (await A.ev('__e2e.server()')).sort((a, b) => (a.z === b.z ? (a.id < b.id ? -1 : 1) : a.z < b.z ? -1 : 1)).map((e) => e.id);
    await selectOnly(A, ctx.ellipse);
    await A.key(']', { modifiers: ['ctrl'] });
    await sleep(200);
    await settle(A);
    let o = await order();
    assert(o.at(-1) === ctx.ellipse, `after front, top is ${o.at(-1)}`);
    await A.key('[', { modifiers: ['ctrl'] });
    await sleep(200);
    await settle(A);
    o = await order();
    assert(o[0] === ctx.ellipse, `after back, bottom is ${o[0]}`);
    await A.key(']', { modifiers: ['ctrl'] });
    await sleep(200);
    await settle(A);
    return 'front, back';
  });
  await check('select: Delete removes the selection; undo restores the same id; redo deletes again', async () => {
    await selectOnly(A, ctx.ellipse);
    await A.key('Delete');
    await A.until((id) => !board.state.server.has(id), [ctx.ellipse], { desc: 'deleted' });
    await settle(A);
    await A.key('z', { modifiers: ['ctrl'] });
    await A.until((id) => board.state.server.has(id), [ctx.ellipse], { desc: 'restored' });
    await settle(A);
    await A.key('z', { modifiers: ['ctrl', 'shift'] });
    await A.until((id) => !board.state.server.has(id), [ctx.ellipse], { desc: 'redo deleted' });
    await settle(A);
    await A.key('y', { modifiers: ['ctrl'] }); // nothing to redo now
    await A.key('z', { modifiers: ['ctrl'] });
    await A.until((id) => board.state.server.has(id), [ctx.ellipse], { desc: 'restored again' });
    await settle(A);
    return ctx.ellipse;
  });
}

// ---- keyboard (rules 14, 15, 19, 21; TECH 11.2) ----

async function keyboard() {
  const v = need(ctx.v, 'view');
  await check('keys: arrows nudge 1px, Shift+arrow 10px; Ctrl+Z / Ctrl+Y / Ctrl+Shift+Z undo and redo', async () => {
    await selectOnly(A, need(ctx.rect, 'rect'));
    const r0 = await el(A, ctx.rect);
    const at = async (x, y, what) => {
      await A.until((id, x, y) => { const e = board.state.server.get(id); return Math.abs(e.x - x) < 0.01 && Math.abs(e.y - y) < 0.01; }, [ctx.rect, x, y], { desc: what });
      await settle(A);
    };
    await A.key('ArrowRight');
    await at(r0.x + 1, r0.y, 'nudge right');
    await A.key('ArrowDown', { modifiers: ['shift'] });
    await at(r0.x + 1, r0.y + 10, 'shift nudge down');
    await A.key('z', { modifiers: ['ctrl'] });
    await at(r0.x + 1, r0.y, 'undo 1');
    await A.key('z', { modifiers: ['ctrl'] });
    await at(r0.x, r0.y, 'undo 2');
    await A.key('y', { modifiers: ['ctrl'] });
    await at(r0.x + 1, r0.y, 'redo with Ctrl+Y');
    await A.key('z', { modifiers: ['ctrl', 'shift'] });
    await at(r0.x + 1, r0.y + 10, 'redo with Ctrl+Shift+Z');
    return 'ok';
  });
  await check('keys: copy and paste of elements creates copies with new ids (rule 19; synthetic clipboard events)', async () => {
    await selectOnly(A, ctx.rect);
    const payload = await A.ev(`(() => { const dt = new DataTransfer(); window.__clip = dt;
      document.dispatchEvent(new ClipboardEvent('copy', { clipboardData: dt, bubbles: true, cancelable: true }));
      return dt.getData('application/x-flats-board+json'); })()`);
    assert(payload && JSON.parse(payload).elements.length === 1, `clipboard payload ${payload && payload.slice(0, 80)}`);
    await A.mouse('mouseMoved', ...(await pt(A, v.x + 380, v.y + 760)), { buttons: 0 });
    const before = await ids(A);
    await A.ev(`document.dispatchEvent(new ClipboardEvent('paste', { clipboardData: window.__clip, bubbles: true, cancelable: true }))`);
    const [p] = await newElements(A, before, 'shape');
    await settle(A);
    assert(p.id !== ctx.rect && p.text === 'Box', JSON.stringify(p));
    ctx.pasted = p.id;
    return p.id;
  });
  await check('keys: copying a connected range pastes a connector attached to the copies (rule 4)', async () => {
    await selectOnly(A, ctx.sticky);
    for (const id of [ctx.rect, ctx.conn]) {
      const c = await centerOf(A, id);
      await A.click(c[0], c[1], { modifiers: ['shift'] });
    }
    const sel = await A.ev('__e2e.sel()');
    assert(sel.length === 3, `selection ${sel}`);
    await A.ev(`(() => { const dt = new DataTransfer(); window.__clip = dt; document.dispatchEvent(new ClipboardEvent('copy', { clipboardData: dt, bubbles: true, cancelable: true })); })()`);
    await A.mouse('mouseMoved', ...(await pt(A, v.x + 1700, v.y + 300)), { buttons: 0 });
    const before = await ids(A);
    await A.ev(`document.dispatchEvent(new ClipboardEvent('paste', { clipboardData: window.__clip, bubbles: true, cancelable: true }))`);
    const added = await newElements(A, before, null, { count: 3 });
    await settle(A);
    const conn = added.find((e) => e.type === 'connector');
    const newIds = new Set(added.map((e) => e.id));
    assert(conn && conn.ends.every((e) => e.el && newIds.has(e.el)), `pasted connector ends ${JSON.stringify(conn && conn.ends)}`);
    ctx.pastedRange = added.map((e) => e.id);
    return `${added.length} elements`;
  });
  await check('keys: pasting multi-line text creates one sticky per line (rule 19)', async () => {
    await A.key('Escape');
    const before = await ids(A);
    await A.mouse('mouseMoved', ...(await pt(A, v.x + 2300, v.y + 300)), { buttons: 0 });
    await A.ev(`(() => { const dt = new DataTransfer(); dt.setData('text/plain', 'Line one\\nLine two\\nLine three');
      document.dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true })); })()`);
    const added = await newElements(A, before, 'sticky', { count: 3 });
    await settle(A);
    const t = added.map((e) => e.text).sort();
    assert(JSON.stringify(t) === JSON.stringify(['Line one', 'Line three', 'Line two']), `texts ${t}`);
    ctx.lines = added.map((e) => e.id);
    return t.join(' / ');
  });
  await check('keys: Ctrl+D duplicates with a new id and an offset', async () => {
    await selectOnly(A, ctx.rect);
    const r = await el(A, ctx.rect);
    const before = await ids(A);
    await A.key('d', { modifiers: ['ctrl'] });
    const [d] = await newElements(A, before, 'shape');
    await settle(A);
    assert(d.text === 'Box' && near(d.x - r.x, 24, 1) && near(d.y - r.y, 24, 1), `duplicate ${JSON.stringify(d)}`);
    ctx.dup = d.id;
    return `${d.id} at +(${d.x - r.x}, ${d.y - r.y})`;
  });
  await check('keys: every tool has its shortcut (V H S T R O D L P E F I; TECH 11.2)', async () => {
    await A.key('Escape');
    const want = { v: 'select', h: 'hand', s: 'sticky', t: 'text', r: 'shape', o: 'shape', d: 'shape', l: 'connector', p: 'pen', e: 'eraser', f: 'frame' };
    const bad = [];
    for (const [k, tool] of Object.entries(want)) {
      await A.key(k);
      const st = await A.ev(`({ pressed: document.querySelector('[data-tool][aria-pressed=true]')?.dataset.tool, shape: board.tools.shape.shape, cur: Object.entries(board.tools).find(([, t]) => t === board.tool)[0] })`);
      if (st.cur !== tool || st.pressed !== tool) bad.push(`${k}→${st.cur}/${st.pressed}`);
      if (k === 'r' && st.shape !== 'rect') bad.push('r shape ' + st.shape);
      if (k === 'o' && st.shape !== 'ellipse') bad.push('o shape ' + st.shape);
      if (k === 'd' && st.shape !== 'diamond') bad.push('d shape ' + st.shape);
    }
    // I opens the image file chooser (intercepted, then left unanswered).
    const opened = A.waitEvent('Page.fileChooserOpened', { timeout: 5000 }).then(() => true, () => false);
    await A.key('i');
    if (!(await opened)) bad.push('i opened no file chooser');
    await sleep(100);
    await A.key('Escape');
    const cur = await A.ev(`Object.entries(board.tools).find(([, t]) => t === board.tool)[0]`);
    if (cur !== 'select') bad.push('Escape left tool ' + cur);
    await A.ev(`document.querySelectorAll('input[type=file].sr').forEach((i) => i.remove())`);
    assert(!bad.length, bad.join('; '));
    return 'all tool keys';
  });
  await check('keys: Ctrl+= / Ctrl+- zoom, Shift+1 fits all, wheel pans and Ctrl+wheel zooms (rule 8)', async () => {
    const z0 = await A.ev('board.cam.z');
    await A.key('=', { modifiers: ['ctrl'] });
    const z1 = await A.ev('board.cam.z');
    await A.key('-', { modifiers: ['ctrl'] });
    const z2 = await A.ev('board.cam.z');
    assert(near(z1, z0 * 1.25, 0.01) && near(z2, z0, 0.01), `zoom ${z0} → ${z1} → ${z2}`);
    await A.key('!', { modifiers: ['shift'] });
    const fit = await A.ev(`(() => { const b = board.contentBounds(), v = board.renderer.viewRect(); return b.x >= v.x - 1 && b.y >= v.y - 1 && b.x + b.w <= v.x + v.w + 1 && b.y + b.h <= v.y + v.h + 1; })()`);
    assert(fit, 'Shift+1 did not fit every element in view');
    const c0 = await A.ev('({...board.cam})');
    const [mx, my] = await A.ev('(() => { const r = board.renderer.dyn.getBoundingClientRect(); return [r.left + r.width / 2, r.top + r.height / 2]; })()');
    await A.send('Input.dispatchMouseEvent', { type: 'mouseWheel', x: mx, y: my, deltaX: 0, deltaY: 120 });
    await sleep(100);
    const c1 = await A.ev('({...board.cam})');
    assert(c1.y > c0.y && near(c1.z, c0.z, 1e-6), `wheel moved camera ${JSON.stringify(c0)} → ${JSON.stringify(c1)}`);
    await A.send('Input.dispatchMouseEvent', { type: 'mouseWheel', x: mx, y: my, deltaX: 0, deltaY: -100, modifiers: 2 });
    await sleep(100);
    const c2 = await A.ev('({...board.cam})');
    assert(c2.z > c1.z, `ctrl+wheel zoom ${c1.z} → ${c2.z}`);
    const lim = await A.ev('(() => { board.zoomBy(100); const hi = board.cam.z; board.zoomBy(0.0001); const lo = board.cam.z; return [lo, hi]; })()');
    assert(near(lim[0], 0.1, 1e-6) && near(lim[1], 4, 1e-6), `zoom range ${lim}`);
    await resetView(A);
    return `zoom ${z0}→${z1}→${z2}; range ${lim[0]}..${lim[1]}`;
  });
  await check('keys: Ctrl+Shift+L locks; a locked element ignores clicks; the Locked elements list unlocks it (rule 3)', async () => {
    await selectOnly(A, need(ctx.dup, 'duplicate'));
    await A.key('l', { modifiers: ['ctrl', 'shift'] });
    await A.until((id) => board.state.server.get(id).locked, [ctx.dup], { desc: 'locked' });
    await settle(A);
    const c = await centerOf(A, ctx.dup, true);
    await A.click(c[0], c[1]);
    const sel = await A.ev('__e2e.sel()');
    assert(!sel.includes(ctx.dup), 'a click selected the locked element');
    await A.click(...(await pt(A, v.x + 1700, v.y + 1300)), { button: 'right' });
    await A.clickSel('[role=menuitem]', { text: 'Locked elements' });
    await A.clickSel('.locked-pop button', { text: 'Unlock' });
    await A.until((id) => !board.state.server.get(id).locked, [ctx.dup], { desc: 'unlocked' });
    await settle(A);
    await A.key('Escape');
    // The duplicate covers the rectangle; remove it so later clicks reach the rectangle.
    await selectOnly(A, ctx.dup);
    await A.key('Delete');
    await A.until((id) => !board.state.server.has(id), [ctx.dup], { desc: 'duplicate removed' });
    await settle(A);
    return 'locked, click ignored, unlocked';
  });
}

// ---- text editing and IME (rule 17) ----

async function textEditing() {
  await check('text: double-click edits text in place and Ctrl+Enter commits', async () => {
    const c = await centerOf(A, need(ctx.member, 'frame member'));
    await A.dblclick(c[0], c[1]);
    await A.until((id) => board.editor && board.editor.id === id, [ctx.member], { desc: 'editor on the member' });
    await A.insertText(' (edited)');
    await A.key('Enter', { modifiers: ['ctrl'] });
    await A.until((id) => board.state.server.get(id).text === 'Inside frame (edited)', [ctx.member], { desc: 'text saved' });
    await settle(A);
    return 'Inside frame (edited)';
  });

  B = await Tab.open(browser.cdp, { browserContextId: ctxB, name: 'B' });
  await check('collab: a second browser context joins as another unverified editor and sees the same board', async () => {
    await openBoard(B, ctx.url, 'Tester B');
    await settle(B);
    const sticky = await el(B, ctx.sticky);
    assert(sticky && sticky.text === 'Alpha sticky', `B sees ${JSON.stringify(sticky)}`);
    const me = await B.ev('board.me');
    assert(me.key !== (await A.ev('board.me.key')), 'both tabs share an editor key');
    return `B is ${me.key} (${me.name}, verified ${me.verified})`;
  });
  await check('collab: participants see each other with names, live selections and cursors (rules 22, 24)', async () => {
    await B.call(() => board.select([]));
    await selectOnly(B, ctx.sticky);
    await B.mouse('mouseMoved', ...(await centerOf(B, ctx.rect)), { buttons: 0 });
    const peer = await A.until((id) => { const p = [...board.presence.peers.values()].find((x) => x.name === 'Tester B'); return p && p.sel && p.sel.includes(id) && p.c ? p : null; }, [ctx.sticky], { desc: 'A sees B with its selection and cursor' });
    const btn = await A.ev(`(() => { const b = document.querySelector('.people'); return { hidden: b.hidden, text: b.textContent, label: b.getAttribute('aria-label') }; })()`);
    assert(!btn.hidden, 'participant list button hidden');
    assert((btn.label || '').includes('Tester B (unverified)'), `participant label ${btn.label}`);
    await B.key('Escape');
    return `peer ${peer.key} sel ${peer.sel}; button "${btn.label || btn.text}"`;
  });

  await check('text: IME composition survives a remote change to the same element and both apply (rule 17)', async () => {
    const c = await centerOf(A, ctx.sticky);
    await A.dblclick(c[0], c[1]);
    await A.until((id) => board.editor && board.editor.id === id, [ctx.sticky], { desc: 'editor open' });
    const ed0 = await A.ev('board.editor.ta.value');
    for (const t of ['ㅎ', '하', '한', '한ㄱ', '한그', '한글']) await A.send('Input.imeSetComposition', { text: t, selectionStart: t.length, selectionEnd: t.length });
    assert(await A.ev('board.editor.composing'), 'compositionstart did not reach the editor');
    agentApply(ctx.slug, { ops: [{ op: 'set', id: ctx.sticky, fields: { style: { color: 'blue' } } }] }, 'recolor during IME');
    await A.until((id) => board.state.server.get(id).style.color === 'blue', [ctx.sticky], { desc: 'remote color arrived' });
    const mid = await A.ev('({ composing: board.editor && board.editor.composing, same: board.editor && board.editor.id, value: board.editor && board.editor.ta.value, focused: document.activeElement === (board.editor && board.editor.ta) })');
    assert(mid.composing && mid.same === ctx.sticky && mid.focused, `composition broken: ${JSON.stringify(mid)}`);
    assert(mid.value === ed0 + '한글', `editor value "${mid.value}"`);
    await A.insertText('한글');
    assert(!(await A.ev('board.editor.composing')), 'composition did not end');
    await A.key('Enter', { modifiers: ['ctrl'] });
    await A.until((id, t) => board.state.server.get(id).text === t, [ctx.sticky, ed0 + '한글'], { desc: 'composed text saved' });
    await settle(A);
    const s = await el(A, ctx.sticky);
    assert(s.style.color === 'blue', `color ${s.style.color}`);
    await B.until((id, t) => board.state.server.get(id).text === t, [ctx.sticky, ed0 + '한글'], { desc: 'B sees the composed text' });
    return `"${s.text}", color ${s.style.color}`;
  });
  await check('text: concurrent text commits keep the later value and the overwritten editor can Keep my text (rule 17, 29)', async () => {
    const c = await centerOf(A, need(ctx.text, 'text element'));
    await A.dblclick(c[0], c[1]);
    await A.until((id) => board.editor && board.editor.id === id, [ctx.text], { desc: 'A editor open' });
    const base = await A.ev('board.editor.ta.value');
    for (const t of ['ㄱ', '가', '간', '가나']) await A.send('Input.imeSetComposition', { text: t, selectionStart: t.length, selectionEnd: t.length });
    const bc = await centerOf(B, ctx.text);
    await B.dblclick(bc[0], bc[1]);
    await B.until((id) => board.editor && board.editor.id === id, [ctx.text], { desc: 'B editor open' });
    await B.insertText(' from B');
    await B.key('Enter', { modifiers: ['ctrl'] });
    await settle(B);
    await A.until((id, t) => board.state.server.get(id).text === t, [ctx.text, base + ' from B'], { desc: 'A received B text' });
    const mid = await A.ev('({ composing: board.editor && board.editor.composing, value: board.editor && board.editor.ta.value })');
    assert(mid.composing && mid.value === base + '가나', `A editor changed during composition: ${JSON.stringify(mid)}`);
    await A.insertText('가나');
    await A.key('Enter', { modifiers: ['ctrl'] });
    await settle(A);
    const finalA = (await el(A, ctx.text)).text;
    assert(finalA === base + '가나', `final text "${finalA}"`);
    await B.until((id, t) => board.state.server.get(id).text === t, [ctx.text, finalA], { desc: 'B converged' });
    const t = await toastSeen(B, 'so the text you wrote was overwritten');
    assert(t.actions.includes('Keep my text'), `notice actions ${t.actions}`);
    const before = await ids(B);
    await B.clickSel('.toasts .toast button', { text: 'Keep my text' });
    const [kept] = await newElements(B, before, 'text');
    await settle(B);
    assert(kept.text === base + ' from B', `kept "${kept.text}"`);
    ctx.kept = kept.id;
    return `final "${finalA}"; B kept "${kept.text}"`;
  });
}

// ---- collaboration (rules 10, 25, 28-30) ----

async function collaboration() {
  need(B, 'second tab');
  await check('collab: an edit in one tab appears in the other without reload (rule 10)', async () => {
    const v = need(ctx.v, 'view');
    const t0 = Date.now();
    const s = await createWithTool(A, 's', [v.x + 1400, v.y + 950], 'Live from A');
    await B.until((id) => board.state.server.has(id), [s.id], { timeout: 5000, desc: 'B sees it' });
    const ms = Date.now() - t0;
    ctx.live = s.id;
    await selectOnly(B, s.id);
    const x0 = s.x;
    await B.key('ArrowRight', { modifiers: ['shift'] });
    await A.until((id, x) => Math.abs(board.state.server.get(id).x - x) < 0.01, [s.id, x0 + 10], { timeout: 5000, desc: 'A sees B move' });
    await B.key('Escape');
    return `created in A, visible in B ${ms} ms after the start of the action; B's move reached A`;
  });
  await check('collab: concurrent edits to different property groups both apply (rule 28)', async () => {
    const id = need(ctx.live, 'live sticky');
    await selectOnly(A, id);
    await selectOnly(B, id);
    const e0 = await el(A, id);
    await Promise.all([A.key('ArrowDown'), B.key('[', { modifiers: ['ctrl'] })]);
    await settle(A); await settle(B);
    await sleep(300);
    const a = await el(A, id), b = await el(B, id);
    assert(JSON.stringify(a) === JSON.stringify(b), `screens differ: ${JSON.stringify(a)} vs ${JSON.stringify(b)}`);
    assert(near(a.y, e0.y + 1, 0.01) && a.z !== e0.z, `geom ${e0.y}→${a.y}, z ${e0.z}→${a.z}`);
    return `y +1 and z ${e0.z}→${a.z} on both screens`;
  });
  await check('collab: concurrent edits to the same group converge to one whole value (rule 29)', async () => {
    const id = ctx.live;
    const e0 = await el(A, id);
    await Promise.all([A.key('ArrowRight'), B.key('ArrowUp')]);
    await settle(A); await settle(B);
    await sleep(300);
    const a = await el(A, id), b = await el(B, id);
    assert(a.x === b.x && a.y === b.y, `screens differ: A (${a.x}, ${a.y}) B (${b.x}, ${b.y})`);
    const dx = Math.round(a.x - e0.x), dy = Math.round(a.y - e0.y);
    const ok = (dx === 1 && dy === 0) || (dx === 0 && dy === -1) || (dx === 1 && dy === -1);
    assert(ok, `unexpected result (${dx}, ${dy})`);
    await A.key('Escape'); await B.key('Escape');
    return `result (${dx}, ${dy}) on both screens${dx === 1 && dy === -1 ? ' (edits were sequential, not concurrent)' : ''}`;
  });
  await check('collab: an edit to an element someone else deleted is ignored and reported (rule 30)', async () => {
    const v = need(ctx.v, 'view');
    const s = await createWithTool(A, 's', [v.x + 1750, v.y + 950], 'Delete race');
    await B.until((id) => board.state.server.has(id), [s.id], { desc: 'B has it' });
    await selectOnly(A, s.id);
    await selectOnly(B, s.id);
    // B goes offline briefly so its nudge is prepared against the old state.
    await B.send('Network.enable');
    await B.send('Network.emulateNetworkConditions', { offline: true, latency: 0, downloadThroughput: -1, uploadThroughput: -1 });
    await B.call(() => board.net.ws && board.net.ws.close());
    await B.until(() => !board.net.connected, [], { desc: 'B disconnected' });
    await B.key('ArrowRight');
    await A.key('Delete');
    await A.until((id) => !board.state.server.has(id), [s.id], { desc: 'A deleted' });
    await settle(A);
    assert(!(await B.ev('board.net.connected')) && (await B.ev('board.state.pending.length')) === 1, 'harness could not hold B offline with one unsent edit');
    const offline = await B.ev('__e2e.status()');
    await B.send('Network.emulateNetworkConditions', { offline: false, latency: 0, downloadThroughput: -1, uploadThroughput: -1 });
    await settle(B, 30000);
    await B.until((id) => !board.state.server.has(id), [s.id], { desc: 'B sees the deletion' });
    const gone = await toastSeen(B, 'someone else deleted were not applied', 8000).then((t) => t.text, () => null);
    assert(!(await el(B, s.id)), 'element still on B');
    assert(gone, 'no "not applied" notice in B');
    return `B status while offline "${offline}"; ${gone}`;
  });
  await check('collab: an agent write shows a notice and a highlight (rule 25)', async () => {
    const before = await ids(A);
    agentApply(ctx.slug, { ops: [{ op: 'add', el: { type: 'sticky', text: 'Agent added this', x: need(ctx.v, 'view').x + 2300, y: ctx.v.y + 1000 } }] }, 'agent add');
    const [n] = await newElements(A, before, 'sticky');
    const t = await toastSeen(A, 'added 1 element');
    const hl = await A.call((id) => board.highlights.has(id), n.id);
    assert(hl, 'no highlight on the agent element');
    ctx.agentSticky = n.id;
    return t.text;
  });
}

// ---- accessibility (rule 13, TECH 11.5) ----

async function accessibility() {
  agentApply(ctx.slug, { ops: [{ op: 'set', id: need(ctx.image, 'image'), fields: { alt: 'Synthetic gradient' } }] }, 'alt text');
  await A.until((id) => board.state.server.get(id).alt === 'Synthetic gradient', [ctx.image], { desc: 'alt arrived' });
  await A.call(() => board.a11y.build());
  await check('a11y: the element list is a tree with frame branches and labelled leaves (TECH 11.5)', async () => {
    const t = await A.ev(`(() => {
      const tree = document.querySelector('#element-tree');
      const items = [...tree.querySelectorAll('[role=treeitem][data-id]')];
      return { role: tree.getAttribute('role'), label: tree.getAttribute('aria-label'),
        canvases: [...document.querySelectorAll('canvas')].map((c) => c.getAttribute('aria-hidden')),
        items: items.map((li) => ({ id: li.dataset.id, label: li.getAttribute('aria-label'), level: li.getAttribute('aria-level'), expanded: li.getAttribute('aria-expanded'), parent: li.parentElement.closest('[role=treeitem]')?.dataset.id || null })) };
    })()`);
    assert(t.role === 'tree', `role ${t.role}`);
    assert(t.canvases.every((h) => h === 'true'), 'a canvas is not aria-hidden');
    const by = new Map(t.items.map((i) => [i.id, i]));
    const all = await A.ev('__e2e.server()');
    const missing = all.filter((e) => !by.has(e.id)).map((e) => e.id);
    assert(!missing.length, `elements missing from the tree: ${missing}`);
    const errs = [];
    const lab = (id) => (by.get(id) || {}).label || '';
    if (!/^Sticky, blue: Alpha sticky/.test(lab(ctx.sticky))) errs.push('sticky: ' + lab(ctx.sticky));
    if (!lab(ctx.sticky).includes('Connected to: "Box"')) errs.push('sticky connections: ' + lab(ctx.sticky));
    if (!/^Connector from "Alpha sticky.*" to "Box"/.test(lab(ctx.conn))) errs.push('connector: ' + lab(ctx.conn));
    if (!/^Image.*Synthetic gradient/.test(lab(ctx.image))) errs.push('image: ' + lab(ctx.image));
    const fr = by.get(ctx.frame) || {};
    if (fr.expanded !== 'true' || !/^Frame /.test(fr.label || '')) errs.push('frame: ' + JSON.stringify(fr));
    const mem = by.get(ctx.member) || {};
    if (mem.parent !== ctx.frame || mem.level !== '2' || !(mem.label || '').includes(`Frame ${ctx.frameName}`)) errs.push('member: ' + JSON.stringify(mem));
    if (!/rectangle: Box/.test(lab(ctx.rect))) errs.push('shape: ' + lab(ctx.rect));
    assert(!errs.length, errs.join(' | '));
    ctx.treeItems = t.items;
    return `${t.items.length} items; e.g. "${lab(ctx.sticky)}"; "${lab(ctx.conn)}"`;
  });
  await check('a11y: tree order is per frame, top to bottom, then left to right (rule 13)', async () => {
    const items = need(ctx.treeItems, 'tree items');
    const all = new Map((await A.ev('__e2e.server()')).map((e) => [e.id, e]));
    const bounds = await A.ev(`Object.fromEntries([...board.state.server.keys()].map((id) => [id, board.index.bounds(id)]))`);
    const geo = (id) => { const e = all.get(id); return e.type === 'connector' ? bounds[id] : e; };
    const errs = [];
    const check = (list, where) => {
      for (let i = 1; i < list.length; i++) {
        const a = geo(list[i - 1]), b = geo(list[i]);
        const ra = Math.round(a.y / 16), rb = Math.round(b.y / 16);
        if (ra > rb || (ra === rb && a.x > b.x)) errs.push(`${where}: ${list[i - 1]} before ${list[i]}`);
      }
    };
    check(items.filter((i) => !i.parent).map((i) => i.id), 'top level');
    check(items.filter((i) => i.parent === ctx.frame).map((i) => i.id), 'frame');
    assert(!errs.length, errs.join('; '));
    return 'ordered';
  });
  await check('a11y: the accessibility tree exposes the list to assistive technology (CDP Accessibility)', async () => {
    await A.send('Accessibility.enable');
    const { nodes } = await A.send('Accessibility.getFullAXTree');
    const tree = nodes.find((n) => n.role && n.role.value === 'tree');
    const items = nodes.filter((n) => n.role && n.role.value === 'treeitem');
    assert(tree && tree.name && tree.name.value === 'Board elements', `tree node ${JSON.stringify(tree && tree.name)}`);
    const names = items.map((n) => (n.name && n.name.value) || '');
    assert(names.some((n) => n.startsWith('Sticky, blue: Alpha sticky')), `treeitem names ${names.slice(0, 5)}`);
    await A.send('Accessibility.disable');
    return `${items.length} treeitems`;
  });
  await check('a11y: Tab reaches the element list from the page (TECH 11.5 "Tab enters the canvas")', async () => {
    await A.key('Escape');
    await A.ev('document.activeElement && document.activeElement.blur(); document.body.focus()');
    let n = 0;
    for (; n < 60; n++) {
      await A.key('Tab');
      if (await A.ev(`!!(document.activeElement && document.activeElement.closest('#element-tree'))`)) break;
    }
    assert(n < 60, 'Tab never reached the element list in 60 presses');
    return `reached after ${n + 1} Tab presses; focus on "${await A.ev('document.activeElement.getAttribute("aria-label")')}"`;
  });
  await check('a11y: keyboard alone moves between, selects, moves, edits and deletes elements (rule 13)', async () => {
    const target = need(ctx.lines, 'pasted line stickies').find(Boolean);
    let found = false;
    for (let i = 0; i < 80; i++) {
      const cur = await A.ev('document.activeElement.dataset.id');
      if (cur === target) { found = true; break; }
      await A.key('ArrowDown');
    }
    assert(found, 'ArrowDown never reached the target item');
    const sel = await A.ev('__e2e.sel()');
    assert(sel.length === 1 && sel[0] === target && (await A.ev('board.focusId')) === target, `selection ${sel}`);
    const e0 = await el(A, target);
    await A.key('ArrowRight', { modifiers: ['shift'] });
    await A.until((id, x) => Math.abs(board.state.server.get(id).x - x) < 0.01, [target, e0.x + 10], { desc: 'Shift+Arrow moved 10px' });
    await A.key('Enter');
    await A.until((id) => board.editor && board.editor.id === id, [target], { desc: 'Enter opened the editor' });
    await A.key('Escape');
    await A.until(() => !board.editor && document.activeElement.closest('#element-tree'), [], { desc: 'Escape returned to the list' });
    await A.key('Delete');
    await A.until((id) => !board.state.server.has(id), [target], { desc: 'Delete removed it' });
    await settle(A);
    ctx.lines = ctx.lines.filter((id) => id !== target);
    return `moved, edited (cancelled) and deleted ${target}`;
  });
  await check('a11y: other people\'s selections carry a name, not only a color (rule 13)', async () => {
    await A.key('Escape');
    await selectOnly(B, need(ctx.rect, 'rect'));
    await A.until((id) => [...board.presence.peers.values()].some((p) => p.sel && p.sel.includes(id) && p.name), [ctx.rect], { desc: 'peer selection with name' });
    // The name tag is drawn on the canvas; check that the presence layer
    // draws text for the peer selection.
    const drew = await A.ev(`(() => { const ctx = board.renderer.dctx || board.renderer.dyn.getContext('2d'); const orig = ctx.fillText; const seen = []; ctx.fillText = function (t, ...r) { seen.push(String(t)); return orig.call(this, t, ...r); }; try { board.renderer.dynamic(); board.renderer.drawDynamic && board.renderer.drawDynamic(); } finally { setTimeout(() => { ctx.fillText = orig; }, 200); } return new Promise((res) => setTimeout(() => res(seen), 150)); })()`);
    await B.key('Escape');
    assert(drew.some((t) => t.includes('Tester B')), `dynamic layer text: ${JSON.stringify(drew.slice(0, 10))}`);
    return `name tags drawn: ${drew.filter((t) => t.includes('Tester B')).length}`;
  });
}

// ---- find (rule 9) ----

async function find() {
  await check('find: Ctrl+F searches text, labels, frame names and alt text and steps through results (rule 9)', async () => {
    await A.key('Escape');
    await A.ev('document.body.focus()');
    await A.key('f', { modifiers: ['ctrl'] });
    await A.until(() => document.activeElement && document.activeElement.id === 'find-q', [], { desc: 'find box focused' });
    const count = async (q) => {
      await A.ev(`(() => { const i = document.querySelector('#find-q'); i.select(); })()`);
      await A.insertText(q);
      return A.until(() => document.querySelector('.findbar .count').textContent, [], { desc: `count for ${q}` });
    };
    const res = {};
    res.gem = await count('gem');
    res.frame = await count(need(ctx.frameName, 'frame name'));
    res.alt = await count('synthetic grad');
    res.none = await count('zzz-no-such-text');
    res.line = await count('line');
    assert(res.gem === '1 of 1', `gem: ${res.gem}`);
    assert(/^1 of \d+$/.test(res.frame), `frame name: ${res.frame}`);
    assert(res.alt === '1 of 1', `alt: ${res.alt}`);
    assert(res.none === 'No matching text', `none: ${res.none}`);
    const n = Number(res.line.split(' of ')[1]);
    assert(n >= 3, `line: ${res.line}`);
    await A.key('Enter');
    const stepped = await A.ev('document.querySelector(".findbar .count").textContent');
    assert(stepped === `2 of ${n}`, `after Enter: ${stepped}`);
    await A.key('Escape');
    assert(!(await A.ev('!!document.querySelector(".findbar")')), 'find bar still open');
    return JSON.stringify(res);
  });
}

// ---- send to agent (rule 26) ----

async function share() {
  await check('share: Send to agent shows a share number', async () => {
    await selectOnly(A, need(ctx.rect, 'rect'));
    await A.clickSel('#share-btn');
    const n = await A.until(() => document.querySelector('.share-pop .num')?.textContent, [], { desc: 'share number' });
    assert(/^#\d+$/.test(n), `number ${n}`);
    await A.key('Escape');
    await A.key('Escape');
    return n;
  });
}

// ---- PNG export (rule 67, TECH 11.6) ----

async function openExport(tab) {
  await tab.clickSel('#export-btn');
  await tab.until(() => !!document.querySelector('dialog[open] .export-form'), [], { desc: 'export dialog' });
}

async function exportPNG(tab, scale, range) {
  await openExport(tab);
  if (range) await tab.clickSel(`dialog[open] input[name=range][value=${range}]`);
  await tab.clickSel(`dialog[open] input[name=scale][value="${scale}"]`);
  const info = await tab.ev(`(() => { const r = [...document.querySelectorAll('dialog[open] input[name=range]')].find((i) => i.checked); const els = board.exporter.rangeElements(r ? r.value : 'all', document.querySelector('dialog[open] select')?.value); const b = board.exporter.bounds(els); return { text: document.querySelector('dialog[open] .export-form p.note.num').textContent, b, n: els.length, range: r && r.value }; })()`);
  const file = await download(() => tab.clickSel('dialog[open] .acts button.primary', { text: 'Download PNG' }));
  const png = readPNG(readFileSync(file.path));
  return { ...info, png, file };
}

async function pngExport() {
  await A.key('Escape');
  await A.call(() => board.select([]));
  await check('png: whole-board export at 1x is a valid PNG of the announced size (rule 67, TECH 11.6)', async () => {
    const r = await exportPNG(A, 1);
    const w = Math.ceil(r.b.w), h = Math.ceil(r.b.h);
    assert(r.png.w === w && r.png.h === h, `PNG ${r.png.w}x${r.png.h}, bounds ${w}x${h}`);
    assert(r.text.includes(`${w.toLocaleString('en-US')} × ${h.toLocaleString('en-US')}`), `dialog says "${r.text}"`);
    assert(r.png.distinct > 8, 'PNG looks blank');
    // Independent oracle: the export box contains every element's box
    // plus the 32px margin.
    const all = (await A.ev('__e2e.server()')).filter((e) => e.type !== 'connector' && !e.rot);
    const minX = Math.min(...all.map((e) => e.x)), maxX = Math.max(...all.map((e) => e.x + e.w));
    assert(r.b.x <= minX - 31 && r.b.x + r.b.w >= maxX + 31, `bounds ${JSON.stringify(r.b)} vs elements ${minX}..${maxX}`);
    writeFileSync(join(OUT, 'export-1x.png'), readFileSync(r.file.path));
    ctx.png1 = r.png;
    return `${r.file.name}: ${r.png.w}x${r.png.h}, ${r.n} elements, color type ${r.png.color}`;
  });
  await check('png: 2x export doubles the pixel size', async () => {
    const r = await exportPNG(A, 2);
    const w = Math.ceil(r.b.w * 2), h = Math.ceil(r.b.h * 2);
    assert(r.png.w === w && r.png.h === h, `PNG ${r.png.w}x${r.png.h}, want ${w}x${h}`);
    if (ctx.png1) assert(Math.abs(r.png.w - 2 * ctx.png1.w) <= 1, `2x ${r.png.w} vs 1x ${ctx.png1.w}`);
    return `${r.png.w}x${r.png.h}`;
  });
  await check('png: a frame export covers the frame and its members only', async () => {
    await selectOnly(A, need(ctx.frame, 'frame'));
    const r = await exportPNG(A, 1, 'frame');
    const f = await el(A, ctx.frame);
    assert(r.range === 'frame', `range ${r.range}`);
    assert(r.png.w === Math.ceil(r.b.w) && r.png.h === Math.ceil(r.b.h), `PNG ${r.png.w}x${r.png.h} vs ${JSON.stringify(r.b)}`);
    // A frame's box includes its 28px name band above it (geom.elementBounds).
    assert(near(r.b.w, f.w + 64, 2) && near(r.b.h, f.h + 28 + 64, 2), `bounds ${JSON.stringify(r.b)} vs frame ${f.w}x${f.h}`);
    await A.key('Escape');
    return `${r.png.w}x${r.png.h} for a ${Math.round(f.w)}x${Math.round(f.h)} frame`;
  });
  await pixelCheck();
}

async function pixelCheck() {
  await check('png: a known board exports its colors, sizes and frame name at the expected pixels, at 1x and 2x (rule 67, TECH 11.6)', async () => {
    const slug = slugOf('pixels');
    agentApply(slug, { ops: PIXEL_BOARD.map((el) => ({ op: 'add', el })) }, 'pixel board');
    // Precondition from the server: the board holds exactly these elements.
    const got = boardGet(slug).elements.map((e) => `${e.type}@${e.x},${e.y},${e.w},${e.h}`).sort();
    const want = PIXEL_BOARD.map((e) => `${e.type}@${e.x},${e.y},${e.w},${e.h}`).sort();
    assert(JSON.stringify(got) === JSON.stringify(want), `the server holds ${got}, want ${want}`);
    const url = cliJSON(['info', slug]).private_url.replace(/\/$/, '') + '/';
    const X = await Tab.open(browser.cdp, { browserContextId: ctxA, name: 'X' });
    try {
      await openBoard(X, url, 'Tester A');
      await X.send('Emulation.setEmulatedMedia', { features: [{ name: 'prefers-color-scheme', value: 'light' }] });
      await X.call(() => board.renderer.invalidateAll());
      const out = [];
      const names = {};
      for (const s of [1, 2]) {
        const r = await exportPNG(X, s);
        const img = decodePNG(readFileSync(r.file.path));
        writeFileSync(join(OUT, `pixels-${s}x.png`), readFileSync(r.file.path));
        const res = pixelErrors(img, s);
        assert(!res.errs.length, `${s}x: ${res.errs.slice(0, 6).join('; ')}`);
        names[s] = res.name;
        out.push(`${s}x ${img.w}x${img.h}, stickies ${Object.values(res.extents).join('/')}px wide, name ${res.name.width}x${res.name.height}px ${res.name.gap}px above the frame`);
      }
      ctx.pixelNames = names;
      return out.join('; ');
    } finally {
      await X.close();
    }
  });
  // A 2x export is the board drawn twice as large, so the frame's name is
  // too: its band above the frame doubles with it. The screen keeps frame
  // names at a fixed size whatever the zoom; the export reuses that drawing.
  await check('png: the frame name doubles in height in a 2x export, like the rest of the image (rule 67)', async () => {
    const n = need(ctx.pixelNames, 'frame name measurements');
    const ratio = n[2].height / n[1].height;
    assert(ratio > 1.6 && ratio < 2.4, `the frame name is ${n[1].height}px tall at 1x and ${n[2].height}px at 2x, while its 28px band and every element doubled`);
    return `${n[1].height}px at 1x, ${n[2].height}px at 2x`;
  });
}

// ---- data file export and import (rules 64, 67, 68) ----

async function importFile(tab, file, mode) {
  await openExport(tab);
  await tab.chooseFiles([file], () => tab.clickSel('dialog[open] .acts button', { text: 'Import…' }));
  await tab.until(() => !!document.querySelector('dialog[open] input[name=mode]'), [], { desc: 'import mode dialog' });
  if (mode === 'replace') await tab.clickSel('dialog[open] input[name=mode][value=replace]');
  await tab.clickSel('dialog[open] .acts button.primary', { text: 'Import' });
}

async function dataRoundTrip() {
  let zipPath, original;
  await check('data: the data file export holds board.json, elements.json and the image asset (rule 67, TECH 9.1)', async () => {
    await settle(A);
    await A.call(() => board.select([]));
    await openExport(A);
    await A.clickSel('dialog[open] input[name=fmt][value=zip]');
    const f = await download(() => A.clickSel('dialog[open] .acts button.primary', { text: 'Download data file' }));
    const zip = readZip(readFileSync(f.path));
    zipPath = join(OUT, 'board-export.zip');
    writeFileSync(zipPath, readFileSync(f.path));
    const meta = JSON.parse(zip.get('board.json'));
    const els = JSON.parse(zip.get('elements.json'));
    original = await A.ev('__e2e.server()');
    assert(meta.format === 1, `format ${meta.format}`);
    assert(els.length === original.length, `${els.length} elements in the file, ${original.length} on the board`);
    assert(JSON.stringify(texts(els)) === JSON.stringify(texts(original)), 'element texts differ from the board');
    const asset = [...zip.keys()].find((k) => k.startsWith('assets/'));
    assert(asset === `assets/${need(ctx.asset, 'asset')}.png`, `asset entry ${asset}`);
    assert(readPNG(zip.get(asset)).w === 64, 'asset is not the uploaded PNG');
    const sorted = els.map((e) => e.id).join() === [...els.map((e) => e.id)].sort().join();
    assert(sorted, 'elements.json is not sorted by id');
    return `${f.name}: ${[...zip.keys()].length} entries, ${els.length} elements`;
  });
  ctx.slug2 = slugOf('import');
  await check('data: importing the file into another board with Add creates new ids and keeps connections (rules 64, 68)', async () => {
    need(zipPath, 'exported zip');
    agentApply(ctx.slug2, { ops: [{ op: 'add', el: { type: 'sticky', text: 'Board two seed', x: 0, y: 0 } }] }, 'seed 2');
    const url2 = cliJSON(['info', ctx.slug2]).private_url.replace(/\/$/, '') + '/';
    ctx.url2 = url2;
    await openBoard(A, url2, 'Tester A');
    await settle(A);
    const before = await ids(A);
    await importFile(A, zipPath, 'add');
    const t = await toastSeen(A, 'Added ', 20000);
    const added = await newElements(A, before, null, { count: original.length, timeout: 20000 });
    await settle(A);
    const now = await A.ev('__e2e.server()');
    assert(now.length === original.length + 1, `${now.length} elements, want ${original.length + 1}`);
    assert(JSON.stringify(texts(added)) === JSON.stringify(texts(original)), 'imported texts differ');
    const origIds = new Set(original.map((e) => e.id));
    assert(added.every((e) => !origIds.has(e.id)), 'an imported element kept its original id');
    const addedIds = new Set(added.map((e) => e.id));
    const conns = added.filter((e) => e.type === 'connector');
    const bad = conns.filter((c) => c.ends.some((e) => e.el && !addedIds.has(e.el)));
    assert(conns.length && !bad.length, `connectors not relinked: ${JSON.stringify(bad)}`);
    const mem = added.find((e) => e.text === 'Inside frame (edited)');
    const fr = added.find((e) => e.type === 'frame');
    assert(mem && fr && mem.frame === fr.id, `membership ${mem && mem.frame} vs frame ${fr && fr.id}`);
    return t.text;
  });
  await check('data: importing with Replace everything makes the board equal to the file', async () => {
    need(ctx.url2, 'second board');
    await importFile(A, zipPath, 'replace');
    await toastSeen(A, 'Replaced the board', 20000);
    await A.until((n) => board.state.server.size === n, [original.length], { timeout: 20000, desc: 'replaced' });
    await settle(A);
    const now = await A.ev('__e2e.server()');
    assert(JSON.stringify(texts(now)) === JSON.stringify(texts(original)), `texts ${texts(now)}`);
    const cli2 = boardGet(ctx.slug2);
    assert(cli2.elements.length === original.length, `CLI sees ${cli2.elements.length}`);
    return `${now.length} elements`;
  });
  await check('data: the CLI imports the browser export as a new board', async () => {
    need(zipPath, 'exported zip');
    const slug3 = slugOf('cli');
    const r = cli(['board', 'import', slug3, zipPath]);
    assert(r.code === 0, `exit ${r.code}: ${r.err || r.out}`);
    const got = boardGet(slug3);
    assert(got.elements.length === original.length, `${got.elements.length} elements`);
    assert(JSON.stringify(texts(got.elements)) === JSON.stringify(texts(original)), 'texts differ');
    return r.out.trim();
  });
  if (ctx.url2) {
    await openBoard(A, ctx.url, 'Tester A');
    await settle(A);
    await resetView(A);
  }
}

// ---- History (rules 41-47) ----

async function revisions(tab) { return tab.call(async () => (await (await fetch('/_board/api/revisions')).json())); }
async function revElements(tab, n) { return tab.call(async (n) => (await (await fetch(`/_board/api/revisions/${n}/elements`)).json()).elements, n); }

async function history() {
  const v = need(ctx.v, 'view');
  let recoverId, recoverRev;
  await check('history: an agent write closes people\'s open edit group into a revision (rule 41)', async () => {
    const s = await createWithTool(A, 's', [v.x + 2300, v.y + 800], 'Recover me');
    recoverId = s.id;
    agentApply(ctx.slug, { ops: [{ op: 'add', el: { type: 'text', text: 'Agent checkpoint 1', x: v.x + 1500, y: v.y + 1300 } }] }, 'checkpoint 1');
    await selectOnly(A, s.id);
    await A.key('Delete');
    await A.until((id) => !board.state.server.has(id), [s.id], { desc: 'deleted' });
    await settle(A);
    agentApply(ctx.slug, { ops: [{ op: 'add', el: { type: 'text', text: 'Agent checkpoint 2', x: v.x + 1500, y: v.y + 1400 } }] }, 'checkpoint 2');
    const list = (await revisions(A)).revisions;
    const kinds = new Set(list.map((r) => r.kind));
    assert(kinds.has('person') && kinds.has('agent') && kinds.has('created'), `kinds ${[...kinds]}`);
    for (const r of list) {
      const els = await revElements(A, r.number);
      if (els.some((e) => e.id === recoverId)) { recoverRev = r.number; break; }
    }
    assert(recoverRev, 'no retained revision holds the deleted sticky');
    return `${list.length} revisions; r${recoverRev} holds "Recover me"`;
  });
  await check('history: the History panel lists revisions with kind, author and changes (rule 46)', async () => {
    await A.clickSel('#history-btn');
    const rows = await A.until(() => { const r = [...document.querySelectorAll('#history .revs li')]; return r.length >= 4 ? r.map((li) => li.textContent) : null; }, [], { desc: 'revision rows' });
    assert(rows.some((r) => /People/.test(r) && /Tester A/.test(r)), `no People row by Tester A: ${rows.slice(0, 4)}`);
    assert(rows.some((r) => /checkpoint 2/.test(r) && /Agent/.test(r)), 'no agent row with its message');
    return `${rows.length} rows; first "${rows[0].replace(/\s+/g, ' ').slice(0, 80)}"`;
  });
  await check('history: pin and unpin a revision (rule 44)', async () => {
    const n = need(recoverRev, 'revision');
    await A.clickSel(`#history button.pin[aria-label="Pin r${n}"]`);
    await A.until(async (n) => (await (await fetch('/_board/api/revisions')).json()).revisions.find((r) => r.number === n).pinned, [n], { desc: 'pinned' });
    await A.until((n) => document.querySelector(`#history button.pin[aria-label="Pin r${n}"]`)?.getAttribute('aria-pressed') === 'true', [n], { desc: 'pressed' });
    await A.clickSel(`#history button.pin[aria-label="Pin r${n}"]`);
    await A.until(async (n) => !(await (await fetch('/_board/api/revisions')).json()).revisions.find((r) => r.number === n).pinned, [n], { desc: 'unpinned' });
    return `r${n} pinned then unpinned`;
  });
  await check('history: an earlier revision opens read-only (rule 46)', async () => {
    const n = need(recoverRev, 'revision');
    await A.clickSel('#history .revs button.rev', { text: `r${n}` });
    await A.until(() => !board.doc.live && document.querySelector('.banner.revision'), [], { desc: 'revision view' });
    const st = await A.ev(`({ canEdit: board.canEdit, readonly: document.body.classList.contains('readonly'), has: !!board.doc.get(${JSON.stringify(recoverId)}), banner: document.querySelector('.banner.revision').textContent })`);
    assert(!st.canEdit && st.readonly && st.has, JSON.stringify(st));
    return st.banner.slice(0, 60);
  });
  await check('history: element recovery copies a deleted element back with a new id (rule 47)', async () => {
    const c = await centerOf(A, recoverId);
    await A.click(c[0], c[1]);
    await A.until((id) => board.sel.has(id), [recoverId], { desc: 'selected in the revision' });
    const before = await A.ev('[...board.state.server.keys()]');
    await A.clickSel('.banner.revision button', { text: 'Copy 1 selected element' });
    const [n] = await newElements(A, before, 'sticky', { timeout: 15000 });
    await settle(A);
    assert(n.text === 'Recover me' && n.id !== recoverId, JSON.stringify(n));
    assert(await A.ev('board.doc.live'), 'still in the revision view');
    ctx.recovered = n.id;
    return `${recoverId} → ${n.id}`;
  });
  await check('history: whole-board rollback makes the board equal to the revision on every screen (rules 32, 45)', async () => {
    const n = need(recoverRev, 'revision');
    const want = await revElements(A, n);
    if (await A.ev('document.querySelector("#history").hidden')) await A.clickSel('#history-btn');
    await A.clickSel('#history .revs button.rev', { text: `r${n}` });
    await A.until(() => !!document.querySelector('.banner.revision'), [], { desc: 'revision view' });
    await A.clickSel('.banner.revision button', { text: 'Roll back to this revision' });
    await A.until(() => !!document.querySelector('dialog[open] button.danger'), [], { desc: 'confirm dialog' });
    await A.clickSel('dialog[open] button.danger');
    await toastSeen(A, 'Rolled the board back', 20000);
    await A.until((n) => board.state.server.size === n, [want.length], { timeout: 15000, desc: 'rolled back' });
    await settle(A);
    const now = await A.ev('__e2e.server()');
    const a = normalized(now), w = normalized(want);
    const d = diffStates(a, w);
    assert(!d.length, `differs from r${n}: ${d.slice(0, 3).join(' | ')}`);
    if (B) {
      await B.until((n) => board.state.server.size === n, [want.length], { timeout: 15000, desc: 'B rolled back' });
      const db = diffStates(normalized(await B.ev('__e2e.server()')), w);
      assert(!db.length, `B differs: ${db.slice(0, 3).join(' | ')}`);
    }
    const list = (await revisions(A)).revisions;
    assert(list.some((r) => r.kind === 'rollback'), 'no rollback revision');
    return `${now.length} elements equal to r${n}`;
  });
  await check('history: rollback clears personal undo (rule 21, 32)', async () => {
    const before = JSON.stringify(normalized(await A.ev('__e2e.server()')));
    await A.ev('document.body.focus()');
    await A.key('z', { modifiers: ['ctrl'] });
    await toastSeen(A, 'Nothing to undo', 8000);
    await settle(A);
    assert(JSON.stringify(normalized(await A.ev('__e2e.server()'))) === before, 'undo changed the board after a rollback');
    return 'Nothing to undo';
  });
  if (await A.ev('!document.querySelector("#history").hidden')) await A.clickSel('#history button', { text: '' }).catch(() => {});
  await A.ev(`document.querySelector('#history').hidden || board.history.close()`);
}

// ---- convergence ----

async function convergence() {
  await check('converge: both browser contexts and the CLI read the same board', async () => {
    await settle(A);
    if (B) await settle(B);
    await sleep(500);
    const a = normalized(await A.ev('__e2e.server()'));
    const c = normalized(boardGet(ctx.slug).elements);
    const dc = diffStates(a, c);
    assert(!dc.length, `A vs CLI: ${dc.slice(0, 3).join(' | ')}`);
    if (B) {
      const b = normalized(await B.ev('__e2e.server()'));
      const db = diffStates(a, b);
      assert(!db.length, `A vs B: ${db.slice(0, 3).join(' | ')}`);
    }
    await A.screenshot(join(OUT, 'final-A.png'));
    if (B) await B.screenshot(join(OUT, 'final-B.png'));
    return `${a.length} elements identical`;
  });
}

// ---- Public boundary (rules 50-53; TECH 7, 7.3) ----

async function operatorApprove(C, approvalURL, what) {
  await C.goto(approvalURL);
  await C.clickSel('button', { text: 'Approve…' }).catch(async () => {
    await C.until(() => [...document.querySelectorAll('button')].some((b) => b.textContent === 'Approve…'), [], { timeout: 10000, desc: 'Approve button' });
    await C.clickSel('button', { text: 'Approve…' });
  });
  await C.until(() => !!document.querySelector('dialog[open] button[value=ok]'), [], { desc: 'approval confirmation' });
  await C.clickSel('dialog[open] button[value=ok]');
  interventions.push({ at: new Date().toISOString(), actor: 'test operator (agent running board-e2e) in the disposable host console', action: what, approval: approvalURL });
}

async function publicBoundary() {
  const mode = opt.public;
  assert(mode === 'adapter', `unknown --public mode ${mode}`);
  const slug = slugOf('public');
  let pubURL, privURL;
  await check('public: operator grants a public provider and approves Public in the console (operator intervention)', async () => {
    const perm = await fetch(new URL('/__gate/host-permission', opt.console), { method: 'POST', body: JSON.stringify({ permitted: ['portal'] }) });
    assert(perm.ok, `host permission ${perm.status} (needs the loopback lifecycle adapter host)`);
    interventions.push({ at: new Date().toISOString(), actor: 'test harness', action: 'enabled the loopback Portal double for the disposable adapter host (/__gate/host-permission)' });
    agentApply(slug, { ops: [{ op: 'add', el: { type: 'sticky', text: 'Public seed', x: 0, y: 0 } }] }, 'public seed');
    privURL = cliJSON(['info', slug]).private_url.replace(/\/$/, '') + '/';
    C = await Tab.open(browser.cdp, { browserContextId: ctxA, name: 'console' });
    await C.goto(new URL(`/flats/${slug}/settings`, opt.console).href);
    await C.until(() => !!document.querySelector('#net-portal'), [], { timeout: 15000, desc: 'console Portal checkbox' });
    if (!(await C.ev('document.querySelector("#net-portal").checked'))) await C.clickSel('#net-portal');
    await C.until(() => document.querySelector('#net-portal').checked && !document.querySelector('#net-portal').disabled, [], { desc: 'Portal allowed' });
    interventions.push({ at: new Date().toISOString(), actor: 'test operator in the disposable host console', action: `allowed Portal (loopback double) for ${slug}` });
    const r = cli(['visibility', slug, 'public']);
    const m = (r.out + r.err).match(/Approval needed: (\S+)/);
    assert(r.code === 3 && m, `visibility request exit ${r.code}: ${r.out}`);
    await operatorApprove(C, m[1], `approved Public for ${slug}`);
    const end = Date.now() + 20000;
    let info;
    while (Date.now() < end) {
      info = cliJSON(['info', slug]);
      const ep = (info.endpoints || []).find((e) => e.provider === 'portal' && e.state === 'ready' && e.url);
      if (info.visibility === 'public' && ep) { pubURL = ep.url.replace(/\/$/, '') + '/'; break; }
      await sleep(300);
    }
    assert(pubURL, `not public: ${JSON.stringify(info && { visibility: info.visibility, endpoints: info.endpoints })}`);
    ctx.publicEndpoints = info.endpoints;
    if (pubURL === privURL) {
      // The loopback Portal double serves Public routes at pub-<slug>; the
      // reported endpoint URL named the private host (see the report).
      const u = new URL(privURL);
      u.hostname = 'pub-' + u.hostname;
      ctx.publicURLNote = `endpoint URL ${pubURL} equals the private URL; using the double's route ${u.href}`;
      pubURL = u.href;
    }
    return `${pubURL}; endpoints ${JSON.stringify(info.endpoints.map((e) => [e.provider, e.url, e.state, e.audience]))}`;
  });
  if (!pubURL) return;
  const ctxP = (await browser.cdp.send('Target.createBrowserContext', { disposeOnDetach: true })).browserContextId;
  await browser.cdp.send('Browser.setDownloadBehavior', { behavior: 'allowAndName', downloadPath: DL, eventsEnabled: true, browserContextId: ctxP });
  P = await Tab.open(browser.cdp, { browserContextId: ctxP, name: 'P' });
  await check('public: the Public route opens read only without tools, History, undo, Send to agent or a name prompt (rule 52)', async () => {
    const access = await openBoard(P, pubURL, 'nobody');
    assert(access === 'public', `access ${access}`);
    const ui = await P.ev(`(() => { const vis = (s) => { const e = document.querySelector(s); return !!e && !e.hidden && getComputedStyle(e).display !== 'none' && e.getBoundingClientRect().width > 0; };
      return { chip: document.querySelector('#access-chip').textContent, tools: vis('.tools'), history: vis('#history-btn'), share: vis('#share-btn'), undo: vis('.undo'), name: vis('#name-btn'), dialog: !!document.querySelector('dialog[open]'), canEdit: board.canEdit, seed: [...board.state.server.values()].some((e) => e.text === 'Public seed') }; })()`);
    assert(ui.chip === 'Public · read only', `chip ${ui.chip}`);
    assert(!ui.tools && !ui.history && !ui.share && !ui.undo && !ui.name && !ui.dialog && !ui.canEdit, JSON.stringify(ui));
    assert(ui.seed, 'board content not shown');
    await P.screenshot(join(OUT, 'public.png'));
    return JSON.stringify(ui);
  });
  await check('public: tool keys, double-click and delete change nothing (rule 52)', async () => {
    const before = boardGet(slug).elements.length;
    await P.key('s');
    await P.click(700, 500);
    await P.dblclick(720, 520);
    await P.key('a', { modifiers: ['ctrl'] });
    await P.key('Delete');
    await sleep(800);
    const after = boardGet(slug).elements.length;
    assert(after === before, `elements ${before} → ${after}`);
    assert(!(await P.ev('!!board.editor')), 'an editor opened');
    return `${after} elements unchanged`;
  });
  let lossState = null;
  await check('public: a plain network loss shows Disconnected on the Public viewer (rule 11)', async () => {
    const connected = await P.ev(`({ hidden: document.querySelector('.save').hidden, connected: board.net.connected })`);
    assert(connected.connected && connected.hidden, `while connected: ${JSON.stringify(connected)}`);
    await P.send('Network.enable');
    await P.send('Network.emulateNetworkConditions', { offline: true, latency: 0, downloadThroughput: -1, uploadThroughput: -1 });
    await P.call(() => board.net.ws && board.net.ws.close());
    const t0 = Date.now();
    const text = await P.until(() => { const e = document.querySelector('.save'); return !e.hidden && e.textContent.includes('Disconnected') && e.textContent; }, [], { timeout: 10000, desc: 'Disconnected status' });
    const ms = Date.now() - t0;
    // Stay offline past two failed reconnects (1 s, then 2 s backoff).
    await P.until(() => board.net.failures >= 3, [], { timeout: 20000, desc: 'three failed reconnects' });
    lossState = await P.ev(`({ status: document.querySelector('.save').hidden ? null : document.querySelector('.save').textContent, banner: document.querySelector('.banners').textContent, bannerKind: board.bannerKind, failures: board.net.failures })`);
    await P.send('Network.emulateNetworkConditions', { offline: false, latency: 0, downloadThroughput: -1, uploadThroughput: -1 });
    return `"${text.trim()}" ${ms} ms after the socket closed; after ${lossState.failures} failed reconnects status "${lossState.status}", banner "${lossState.banner}"`;
  });
  await check('public: after the network returns the viewer reconnects, hides the status and shows no withdrawal banner', async () => {
    need(lossState, 'network loss state');
    const st = await P.until(() => board.net.connected && ({ hidden: document.querySelector('.save').hidden, banner: document.querySelector('.banners').textContent, bannerKind: board.bannerKind }), [], { timeout: 40000, desc: 'reconnected' });
    const errs = [];
    if (lossState.bannerKind === 'withdrawn') errs.push(`while merely offline the viewer said "${lossState.banner.trim()}"`);
    if (!st.hidden) errs.push('status still shown after reconnecting');
    if (st.bannerKind) errs.push(`banner "${st.banner.trim()}" (${st.bannerKind}) stays after reconnecting`);
    assert(!errs.length, errs.join('; '));
    return `reconnected; status hidden, no banner`;
  });
  // A fresh Public viewer for the remaining checks, so that state left by
  // the network-loss checks cannot satisfy or break them.
  await openBoard(P, pubURL, 'nobody');
  await check('public: the WebSocket refuses ops, presence and shares (rule 52; TECH 7.1)', async () => {
    // One fresh socket per refused message: a refusal may end the socket.
    const out = await P.callWith({ timeout: 40000 }, async () => {
      const probe = (msg) => new Promise((resolve) => {
        const ws = new WebSocket(location.origin.replace('http', 'ws') + '/_board/ws');
        const got = [];
        let sent = false;
        const done = (why) => { try { ws.close(); } catch { /* ignore */ } resolve({ why, got: got.map((m) => (m.t === 'snapshot' ? { t: 'snapshot' } : m)) }); };
        ws.onmessage = (e) => {
          const m = JSON.parse(e.data);
          got.push(m);
          if (m.t === 'snapshot' && !sent) { sent = true; setTimeout(() => { ws.send(JSON.stringify(msg)); setTimeout(() => done('no reply in 2 s'), 2000); }, 300); }
        };
        setTimeout(() => done('no snapshot in 6 s'), 6000);
        ws.onclose = (e) => done(`close ${e.code}`);
        ws.onopen = () => ws.send(JSON.stringify({ t: 'hello', v: 1, pending: { count: 0 }, editor: 'publicprobe00', name: 'Probe' }));
      });
      return {
        ops: await probe({ t: 'ops', n: 1, ops: [{ op: 'add', el: { id: 'e_publicprb1', type: 'sticky', x: 10, y: 10, w: 100, h: 100, text: 'public write', z: 'zz' } }] }),
        pres: await probe({ t: 'pres', c: [1, 2], sel: [] }),
        share: await probe({ t: 'share', ids: [] }),
      };
    });
    const errs = [];
    for (const [k, r] of Object.entries(out)) {
      const types = r.got.map((m) => m.t);
      if (types.includes('ack') || types.includes('shared')) errs.push(`${k} was answered: ${types}`);
      if (!types.includes('welcome') || r.got.find((m) => m.t === 'welcome').access !== 'public') errs.push(`${k}: welcome not public`);
    }
    if (boardGet(slug).elements.some((e) => e.text === 'public write')) errs.push('the public write was applied');
    assert(!errs.length, `${errs.join('; ')} — ${JSON.stringify(out).slice(0, 500)}`);
    return Object.entries(out).map(([k, r]) => `${k}: ${r.got.filter((m) => m.t !== 'snapshot' && m.t !== 'welcome').map((m) => `${m.t}${m.code ? ':' + m.code : ''}`).join(',') || 'no reply'} (${r.why})`).join('; ');
  });
  await check('public: a public viewer receives live content without presence, names or author fields (rules 10, 52; TECH 7.3)', async () => {
    await openBoard(A, privURL, 'Tester A');
    await settle(A);
    await P.ev(`(() => { const ws = new WebSocket(location.origin.replace('http', 'ws') + '/_board/ws'); window.__probe = { msgs: [], close: null, ws };
      ws.onmessage = (e) => { try { window.__probe.msgs.push(JSON.parse(e.data)); } catch { window.__probe.msgs.push({ raw: String(e.data).slice(0, 200) }); } };
      ws.onclose = (e) => { window.__probe.close = { code: e.code, reason: e.reason, at: Date.now() }; };
      ws.onopen = () => ws.send(JSON.stringify({ t: 'hello', v: 1, pending: { count: 0 }, editor: 'publiclisten0' })); })()`);
    await P.until(() => window.__probe.msgs.some((m) => m.t === 'snapshot'), [], { desc: 'listener snapshot' });
    const v2 = await A.ev('__e2e.view()');
    const s = await createWithTool(A, 's', [v2.x + 300, v2.y + 300], 'Visible to public');
    await selectOnly(A, s.id);
    for (let i = 0; i < 5; i++) await A.mouse('mouseMoved', 400 + i * 20, 400, { buttons: 0 });
    await A.call(() => board.share.send());
    const t0 = Date.now();
    await P.until((id) => board.state.server.has(id), [s.id], { timeout: 8000, desc: 'public view shows the private edit live' });
    const ms = Date.now() - t0;
    await sleep(1500);
    const pr = await P.ev(`({ types: window.__probe.msgs.map((m) => m.t), deltas: window.__probe.msgs.filter((m) => m.t === 'delta' || m.t === 'delta_part'), close: window.__probe.close,
      snapshotKeys: [...new Set(window.__probe.msgs.filter((m) => m.t === 'snapshot').flatMap((m) => (m.elements || []).flatMap((e) => Object.keys(e))))],
      welcome: window.__probe.msgs.find((m) => m.t === 'welcome'), peers: board.presence.peers.size })`);
    const errs = [];
    const types = new Set(pr.types);
    if (types.has('pres')) errs.push('presence reached a public connection');
    if (types.has('shared')) errs.push('a share reached a public connection');
    if (!pr.deltas.length) errs.push('no live delta reached the listener');
    for (const d of pr.deltas) {
      for (const k of ['by', 'from', 'reset']) if (k in d) errs.push(`delta carries ${k}`);
      if (JSON.stringify(d).includes('Tester A')) errs.push('delta carries an editor name');
    }
    for (const k of ['by', 'at', 'inc', 'v', 'deleted']) if (pr.snapshotKeys.includes(k)) errs.push(`snapshot element field ${k}`);
    if (pr.peers) errs.push(`public page lists ${pr.peers} peers`);
    assert(!errs.length, `${errs.join('; ')} — ${JSON.stringify(pr).slice(0, 400)}`);
    return `edit visible on Public after ${ms} ms; listener got ${[...types].join(',')}; snapshot fields ${pr.snapshotKeys.join(',')}`;
  });
  await check('public: History, recovery, upload and import routes refuse a Public route; the data file has no authors (rule 52; TECH 7, 7.2)', async () => {
    const r = await P.call(async () => {
      const st = async (p, init) => { try { return (await fetch(p, init)).status; } catch (e) { return String(e); } };
      const post = (body, type) => ({ method: 'POST', headers: { 'X-Flats-Board': '1', 'Content-Type': type || 'application/json' }, body });
      return {
        revisions: await st('/_board/api/revisions'), revision1: await st('/_board/api/revisions/1/elements'),
        pin: await st('/_board/api/revisions/1/pin', post('{"pinned":true}')), rollback: await st('/_board/api/revisions/1/rollback', post('{}')),
        recover: await st('/_board/api/recover', post('{"revision":1,"ids":[]}')),
        assets: await st('/_board/api/assets', post(new Uint8Array([137, 80, 78, 71]), 'image/png')),
        import: await st('/_board/api/import?mode=add', post(new Uint8Array([80, 75, 5, 6]), 'application/zip')),
      };
    });
    const bad = Object.entries(r).filter(([, s]) => s === 200 || s === 201);
    assert(!bad.length, `allowed on Public: ${JSON.stringify(r)}`);
    const zipFile = await download(() => P.ev(`(() => { const a = document.createElement('a'); a.href = '/_board/export.zip'; a.download = 'public.zip'; document.body.appendChild(a); a.click(); a.remove(); })()`));
    const els = JSON.parse(readZip(readFileSync(zipFile.path)).get('elements.json'));
    const leak = els.filter((e) => 'by' in e || 'at' in e);
    assert(!leak.length, `public export has author fields: ${JSON.stringify(leak[0])}`);
    return `${JSON.stringify(r)}; public export ${els.length} elements without by/at`;
  });
  await check('public: an asset only an earlier state referenced is unreachable on Public (TECH 7.2)', async () => {
    const png = join(WORK, 'public-past.png');
    writeFileSync(png, makePNG(40, 30, 9));
    await A.mouse('mouseMoved', 500, 500, { buttons: 0 });
    const before = await ids(A);
    await A.chooseFiles([png], () => A.key('i'));
    const [img] = await newElements(A, before, 'image', { timeout: 15000 });
    await settle(A);
    await P.until((id) => board.state.server.has(id), [img.id], { desc: 'public sees the image' });
    const live = await P.call(async (h) => (await fetch('/_board/a/' + h, { cache: 'no-store' })).status, img.asset);
    await selectOnly(A, img.id);
    await A.key('Delete');
    await A.until((id) => !board.state.server.has(id), [img.id], { desc: 'image deleted' });
    await settle(A);
    await P.until((id) => !board.state.server.has(id), [img.id], { desc: 'public sees the deletion' });
    const past = await P.call(async (h) => (await fetch('/_board/a/' + h, { cache: 'no-store' })).status, img.asset);
    const priv = await A.call(async (h) => (await fetch('/_board/a/' + h, { cache: 'no-store' })).status, img.asset);
    const direct = await httpStatus(pubURL + '_board/a/' + img.asset);
    assert(live === 200, `live asset on Public ${live}`);
    assert(past === 404 && direct === 404, `past-only asset on Public: browser ${past}, direct HTTP ${direct}`);
    return `live ${live}, after deletion Public ${past} (direct HTTP ${direct}), Private ${priv}`;
  });
  await check('public: returning to Private closes open public connections at once (rule 53)', async () => {
    const r = cli(['visibility', slug, 'private']);
    const m = (r.out + r.err).match(/Approval needed: (\S+)/);
    assert(r.code === 3 && m, `visibility request exit ${r.code}: ${r.out}`);
    assert(!(await P.ev('window.__probe.close')), 'listener closed before the withdrawal');
    const pre = await P.ev(`({ bannerKind: board.bannerKind, connected: board.net.connected })`);
    assert(!pre.bannerKind && pre.connected, `viewer not in a clean connected state before the withdrawal: ${JSON.stringify(pre)}`);
    await operatorApprove(C, m[1], `approved Private for ${slug}`);
    const t0 = Date.now();
    const end = Date.now() + 20000;
    let info;
    while (Date.now() < end) { info = cliJSON(['info', slug]); if (info.visibility === 'private') break; await sleep(100); }
    const tApplied = Date.now();
    assert(info.visibility === 'private', 'withdrawal not applied');
    const closed = await P.until(() => window.__probe.close, [], { timeout: 10000, desc: 'listener socket closed' });
    const page = await P.until(() => !board.net.connected, [], { timeout: 10000, desc: 'page socket closed' }).then(() => true);
    const route = await httpStatus(pubURL);
    assert(route !== 200, `the Public route still answers ${route}`);
    return `closed (code ${closed.code}) ${Math.max(0, closed.at - tApplied)} ms after the visibility read back private (approval click to read-back ${tApplied - t0} ms); page socket closed ${page}; route now ${route}`;
  });
  await check('public: after withdrawal the public viewer shows that it is disconnected and no longer public (rule 11; app.js withdrawn banner)', async () => {
    const t0 = Date.now();
    const banner = await P.until(() => document.querySelector('.banners')?.textContent.includes('no longer public') && document.querySelector('.banners').textContent, [], { timeout: 45000, desc: 'withdrawn banner' })
      .catch(async (err) => {
        const st = await P.ev(`({ failures: board.net.failures, connected: board.net.connected, welcomed: board.welcomed, access: board.access, bannerKind: board.bannerKind, statusHidden: document.querySelector('.save').hidden, banners: document.querySelector('.banners').textContent })`);
        throw new Error(`${err.message}; page state ${JSON.stringify(st)}`);
      });
    return `"${banner.trim()}" after ${Date.now() - t0} ms more`;
  });
  await C.close();
  C = null;
}

// ---- report ----

async function finish(code) {
  if (browser) {
    // The browser and every helper it started end with the run.
    const name = 'harness: no browser process keeps the temporary profile after the run';
    try {
      const killed = await browser.close();
      const left = browser.leftovers();
      if (!killed || !left) results.push({ section: 'harness', name, status: 'fail', detail: 'the process list could not be read, so leftover processes are unknown' });
      else {
        results.push({ section: 'harness', name, status: left.length ? 'fail' : 'pass',
          detail: left.length ? left.map((p) => `${p.pid} ${p.command}`).join('; ') : `${killed.length} process(es) still ran after the browser quit and were ended` });
      }
    } catch (err) {
      results.push({ section: 'harness', name, status: 'fail', detail: err.message });
    }
  }
  try { rmSync(WORK, { recursive: true, force: true }); } catch { /* ignore */ }
  const counts = { pass: 0, fail: 0, skip: 0 };
  for (const r of results) counts[r.status]++;
  console.log('\nCheck list');
  for (const r of results) console.log(`${r.status.toUpperCase().padEnd(5)} [${r.section}] ${r.name}${r.status !== 'pass' && r.detail ? ` — ${r.detail.split('\n')[0]}` : ''}`);
  console.log(`\n${counts.pass} passed, ${counts.fail} failed, ${counts.skip} skipped`);
  if (interventions.length) console.log(`Operator interventions: ${interventions.length} (see results.json)`);
  writeFileSync(join(OUT, 'results.json'), JSON.stringify({ host: opt.console, chrome: ctx.chromeVersion, node: process.version, run: RUN, counts, results, interventions }, null, 2));
  console.log(`Results: ${join(OUT, 'results.json')}`);
  process.exit(code ?? (counts.fail ? 1 : 0));
}

main().then(() => finish(), (err) => {
  console.error(err.stack || err);
  results.push({ section: currentSection, name: 'harness', status: 'fail', detail: err.message });
  finish(1);
});
