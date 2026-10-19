#!/usr/bin/env node
// selftest.mjs checks the judge itself on a disposable host: an unsorted
// seed must fail, a scripted correct sort must pass, and each single
// violation applied to that correct board must fail exactly its check.
//
// usage: node selftest.mjs --url http://127.0.0.1:17940 --dir <scratch dir>

import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { args, post, readBoard } from './lib.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const opt = args(process.argv.slice(2), { url: null, dir: null });
let n = 0;
const fails = [];

function judge(slug, baseline) {
  const out = join(opt.dir, `${slug}-judge.json`);
  try {
    execFileSync('node', [join(here, 'judge.mjs'), '--url', opt.url, '--slug', slug, '--baseline', baseline, '--json', out], { stdio: 'ignore' });
  } catch (e) { if (e.status !== 1) throw e; }
  return JSON.parse(readFileSync(out, 'utf8'));
}
function expect(name, r, failing) {
  const got = Object.entries(r.checks).filter(([, c]) => !c.pass).map(([k]) => k).sort();
  const want = [...failing].sort();
  const ok = JSON.stringify(got) === JSON.stringify(want);
  console.log(`${ok ? 'ok  ' : 'FAIL'} ${name}: failing checks ${JSON.stringify(got)}${ok ? '' : `, want ${JSON.stringify(want)}`}`);
  if (!ok) fails.push(name);
}
const write = (slug, body) => post(opt.url, `/api/flats/${slug}/board`, { op_id: `selftest-${slug}-${++n}`, ...body });

// sortCorrectly frames each topic, resizes small stickies, resets fonts and
// lays the frames out side by side.
async function sortCorrectly(slug, base) {
  const ops = [];
  const layout = [];
  base.topics.forEach((t, i) => {
    ops.push({ op: 'add', ref: `$f${i}`, el: { type: 'frame', text: t, x: i * 800, y: 2000, w: 700, h: 400 } });
    const ids = Object.entries(base.stickies).filter(([, s]) => s.topic === t).map(([id]) => id);
    for (const id of ids) ops.push({ op: 'set', id, fields: { w: 200, h: 200, style: { fontSize: 'auto' } } });
    ops.push({ op: 'reparent', ids, frame: `$f${i}` });
    layout.push({ kind: 'grid', ids, in: `$f${i}`, columns: 3, gap: 24 });
  });
  // Frames go to the back, as the browser draws a new frame (ops.js).
  ops.push({ op: 'zorder', ids: base.topics.map((_, i) => `$f${i}`), to: 'back' });
  return write(slug, { ops, layout });
}

async function fresh(slug) {
  const baseline = join(opt.dir, `${slug}-baseline.json`);
  execFileSync('node', [join(here, 'seed.mjs'), '--url', opt.url, '--slug', slug, '--out', baseline], { stdio: 'ignore' });
  const base = JSON.parse(readFileSync(baseline, 'utf8'));
  return { baseline, base };
}

const tag = Date.now().toString(36);
{
  const slug = `selftest-${tag}-a`;
  const { baseline, base } = await fresh(slug);
  expect('unsorted seed', judge(slug, baseline), ['overlap', 'sorted', 'text']);
  const res = await sortCorrectly(slug, base);
  expect('scripted correct sort', judge(slug, baseline), []);

  const sticky = Object.keys(base.stickies)[0];
  const conn = Object.keys(base.connectors)[0];
  const board = await readBoard(opt.url, slug);
  const el = board.elements.find((e) => e.id === sticky);
  const frame = board.elements.find((e) => e.id === el.frame);
  const other = board.elements.find((e) => e.type === 'sticky' && e.frame === el.frame && e.id !== sticky);
  const restore = { op: 'set', id: sticky, fields: { x: el.x, y: el.y, w: el.w, h: el.h, style: { fontSize: 'auto' } }, carry: false };

  await write(slug, { ops: [{ op: 'set', id: sticky, fields: { x: other.x + 50, y: other.y + 50 } }] });
  expect('one sticky moved onto another', judge(slug, baseline), ['overlap']);
  await write(slug, { ops: [restore] });

  await write(slug, { ops: [{ op: 'set', id: sticky, fields: { x: frame.x + frame.w - 50 } }] });
  expect('one sticky sticks out of its frame', judge(slug, baseline), ['overlap']);
  await write(slug, { ops: [restore] });

  await write(slug, { ops: [{ op: 'set', id: sticky, fields: { x: frame.x + 4, y: frame.y - 40, w: 60, h: 30 } }, { op: 'reparent', ids: [sticky], frame: null }] });
  const r = judge(slug, baseline);
  const coversName = r.checks.overlap.problems.some((p) => p.includes('covers the name'));
  console.log(`${coversName ? 'ok  ' : 'FAIL'} small sticky over a frame name is reported as covering it`);
  if (!coversName) fails.push('frame name');
  await write(slug, { ops: [{ op: 'reparent', ids: [sticky], frame: frame.id }, restore] });
  expect('restored', judge(slug, baseline), []);

  await write(slug, { ops: [{ op: 'zorder', ids: [frame.id], to: 'front' }] });
  expect('frame drawn above its members', judge(slug, baseline), ['visible']);
  await write(slug, { ops: [{ op: 'zorder', ids: [frame.id], to: 'back' }] });
  expect('frame back behind its members', judge(slug, baseline), []);

  await write(slug, { ops: [{ op: 'set', id: sticky, fields: { style: { fontSize: 10 } } }] });
  expect('10 px sticky', judge(slug, baseline), ['text']);
  await write(slug, { ops: [restore] });

  await write(slug, { ops: [{ op: 'set', id: sticky, fields: { text: el.text + '!' } }] });
  expect('changed text', judge(slug, baseline), ['kept']);
  await write(slug, { ops: [{ op: 'set', id: sticky, fields: { text: el.text } }] });

  const c = board.elements.find((e) => e.id === conn);
  await write(slug, { ops: [{ op: 'set', id: conn, fields: { ends: [c.ends[0], { pt: [frame.x - 300, frame.y - 300] }] } }] });
  expect('connector end freed', judge(slug, baseline), ['connectors']);
  const wrong = Object.keys(base.stickies).find((id) => !base.connectors[conn].ends.includes(id));
  await write(slug, { ops: [{ op: 'set', id: conn, fields: { ends: [c.ends[0], { el: wrong }] } }] });
  expect('connector attached to the wrong sticky', judge(slug, baseline), ['connectors']);
  await write(slug, { ops: [{ op: 'set', id: conn, fields: { ends: c.ends } }] });
  expect('connector restored', judge(slug, baseline), []);

  await write(slug, { ops: [{ op: 'delete', ids: [sticky] }] });
  expect('deleted sticky', judge(slug, baseline), ['connectors', 'kept', 'sorted'].filter((k) => k !== 'connectors' || Object.values(base.connectors).some((x) => x.ends.includes(sticky))));
  void res;
}

console.log(fails.length ? `selftest FAILED: ${fails.join(', ')}` : 'selftest passed');
process.exit(fails.length ? 1 : 0);
