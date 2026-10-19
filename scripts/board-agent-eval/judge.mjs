#!/usr/bin/env node
// judge.mjs checks a board an agent sorted against the seed baseline
// (seed.mjs) and the board spec's agent success criterion:
//
//   kept        every seeded element still exists with the same id, type and text
//   visible     no element is drawn under an opaque frame (the client draws in
//               (z, id) order, and a frame's fill hides what lies below it)
//   overlap     no two non-frame elements overlap; frames do not overlap each
//               other; an element over a frame belongs to it and lies inside it;
//               no element covers a frame's name
//   text        every text is drawn at 12 px or more at 100% zoom and is not cut off
//   connectors  both ends of every seeded connector are attached to the intended
//               elements
//   sorted      (the task itself) each topic's stickies share one named frame
//               that holds no other topic
//
// usage: node judge.mjs --url http://127.0.0.1:17940 --slug feedback-1 \
//          --baseline baseline.json [--measure auto|chrome|approx] [--json out.json]
// Exit status: 0 when every check passes, 1 when one fails, 2 on errors.

import { readFileSync, writeFileSync } from 'node:fs';
import { args, readBoard } from './lib.mjs';
import { measure } from './measure.mjs';
import { connectorPoints } from '../../internal/board/web/geom.js';

const MIN_TEXT = 12;     // px at 100% zoom (board.md success criteria)
const EPS = 0.5;         // px; the host stores coordinates to 0.1 px
const LINE = 1.38;       // render.js line height factor
const FRAME_LABEL = 13;  // render.js draws frame names at 13 / zoom px
const FONT = '-apple-system, BlinkMacSystemFont, "Segoe UI", "Apple SD Gothic Neo", "Noto Sans KR", sans-serif';

// Style defaults, as internal/board/web/schema.js STYLE_DEFAULTS.
const DEFAULTS = {
  frame: { fill: 'white' }, sticky: { fontSize: 'auto' }, text: { fontSize: 20 }, shape: { shape: 'rect', fontSize: 14 },
  connector: { fontSize: 12 },
};
const style = (el, k) => (el.style && el.style[k] !== undefined ? el.style[k] : (DEFAULTS[el.type] || {})[k]);

// box returns the axis-aligned bounds of an element, rotation included.
function box(el) {
  const { x, y, w, h } = el;
  const r = ((el.rot || 0) * Math.PI) / 180;
  if (!r) return { x0: x, y0: y, x1: x + w, y1: y + h };
  const cx = x + w / 2, cy = y + h / 2;
  const ew = (Math.abs(w * Math.cos(r)) + Math.abs(h * Math.sin(r))) / 2;
  const eh = (Math.abs(w * Math.sin(r)) + Math.abs(h * Math.cos(r))) / 2;
  return { x0: cx - ew, y0: cy - eh, x1: cx + ew, y1: cy + eh };
}
const overlapArea = (a, b) => {
  const w = Math.min(a.x1, b.x1) - Math.max(a.x0, b.x0);
  const h = Math.min(a.y1, b.y1) - Math.max(a.y0, b.y0);
  return w > EPS && h > EPS ? { w: +w.toFixed(1), h: +h.toFixed(1) } : null;
};
// segmentInBox reports whether segment p-q passes through the interior of b.
function segmentInBox(p, q, b) {
  let t0 = 0, t1 = 1;
  const d = [q[0] - p[0], q[1] - p[1]];
  for (const [pc, dc, lo, hi] of [[p[0], d[0], b.x0 + EPS, b.x1 - EPS], [p[1], d[1], b.y0 + EPS, b.y1 - EPS]]) {
    if (Math.abs(dc) < 1e-9) { if (pc <= lo || pc >= hi) return false; continue; }
    let a = (lo - pc) / dc, c = (hi - pc) / dc;
    if (a > c) [a, c] = [c, a];
    t0 = Math.max(t0, a); t1 = Math.min(t1, c);
    if (t0 >= t1) return false;
  }
  return true;
}
const inside = (a, f) => a.x0 >= f.x0 - EPS && a.y0 >= f.y0 - EPS && a.x1 <= f.x1 + EPS && a.y1 <= f.y1 + EPS;

async function main() {
  const opt = args(process.argv.slice(2), { url: null, slug: null, baseline: null, measure: 'auto', json: '' });
  const base = JSON.parse(readFileSync(opt.baseline, 'utf8'));
  const board = await readBoard(opt.url, opt.slug);
  const byId = new Map(board.elements.map((e) => [e.id, e]));
  const short = (id) => {
    const e = byId.get(id);
    const t = e && e.text ? ` "${e.text.slice(0, 40)}"` : '';
    return `${id}${t}`;
  };
  const checks = {};

  // kept
  {
    const problems = [];
    for (const b of base.elements) {
      const e = byId.get(b.id);
      if (!e) { problems.push(`${b.type} ${b.id} "${(b.text || '').slice(0, 40)}" is missing`); continue; }
      if (e.type !== b.type) problems.push(`${b.id} changed type ${b.type} -> ${e.type}`);
      if ((e.text || '') !== (b.text || '')) problems.push(`${b.id} text changed "${b.text}" -> "${e.text}"`);
    }
    checks.kept = { pass: problems.length === 0, problems, seeded: base.elements.length };
  }

  const frames = board.elements.filter((e) => e.type === 'frame');
  const solids = board.elements.filter((e) => e.type !== 'frame' && e.type !== 'connector');

  // visible: render.js draws every element in (z, id) order (ids.js
  // zCompare), and a frame whose fill is not "none" is opaque, so an element
  // under it, or a connector line crossing it, is hidden there.
  {
    const problems = [];
    const below = (a, b) => (a.z !== b.z ? a.z < b.z : a.id < b.id);
    const opaque = frames.filter((f) => style(f, 'fill') !== 'none');
    for (const e of board.elements) {
      if (e.type === 'frame') continue;
      for (const f of opaque) {
        if (!below(e, f)) continue;
        const fb = box(f);
        let hidden = false;
        if (e.type === 'connector') {
          const pts = connectorPoints(e, (id) => byId.get(id));
          hidden = !!pts && pts.slice(1).some((q, i) => segmentInBox(pts[i], q, fb));
        } else hidden = !!overlapArea(box(e), fb);
        if (hidden) problems.push(`${e.type} ${short(e.id)} (z ${e.z}) is drawn under frame ${short(f.id)} (z ${f.z}, fill ${style(f, 'fill')})`);
      }
    }
    checks.visible = { pass: problems.length === 0, problems };
  }

  // text: font size the client draws at 100% zoom
  const items = [];
  const meta = [];
  for (const e of board.elements) {
    if (e.type === 'frame') {
      items.push({ text: e.text || 'Frame', maxW: 1e9, maxH: null, size: FRAME_LABEL, shrink: false, weight: 600,
        labelFont: `600 ${FRAME_LABEL}px ${FONT}` });
      meta.push({ e, kind: 'frame-label' });
      continue;
    }
    if (!e.text) continue;
    const fs = style(e, 'fontSize');
    if (e.type === 'sticky') {
      // render.js sticky(): pad 12, foot 26 when an author name shows (a
      // tailnet editor key), else 12. Agent and API keys show no name.
      const foot = typeof e.by === 'string' && e.by.startsWith('ts:') ? 26 : 12;
      items.push({ text: e.text, maxW: e.w - 24, maxH: e.h - 12 - foot, size: fs === 'auto' ? 16 : fs, shrink: fs === 'auto' });
    } else if (e.type === 'shape') {
      const k = style(e, 'shape');
      const inset = k === 'diamond' ? 0.25 : k === 'ellipse' ? 0.15 : 0;
      items.push({ text: e.text, maxW: e.w * (1 - inset * 2) - 16, maxH: e.h * (1 - inset * 2) - 16, size: fs, shrink: true });
    } else if (e.type === 'text') {
      items.push({ text: e.text, maxW: e.w - 8, maxH: null, size: fs, shrink: false });
    } else if (e.type === 'connector') {
      items.push({ text: e.text, maxW: 1e9, maxH: null, size: fs, shrink: false });
    } else continue;
    meta.push({ e, kind: e.type });
  }
  const m = await measure(items, opt.measure);
  {
    const problems = [];
    let min = Infinity;
    m.values.forEach((r, i) => {
      const { e, kind } = meta[i];
      if (kind === 'frame-label') return;
      min = Math.min(min, r.size);
      if (r.size < MIN_TEXT) problems.push(`${kind} ${short(e.id)} drawn at ${r.size} px (fontSize ${JSON.stringify(style(e, 'fontSize'))}, ${e.w}x${e.h})`);
      if (r.overflow) problems.push(`${kind} ${short(e.id)} is cut off: text does not fit at ${r.size} px in ${e.w}x${e.h}`);
    });
    checks.text = { pass: problems.length === 0, problems, min_px: min === Infinity ? null : min, engine: m.engine,
      measured: meta.filter((x) => x.kind !== 'frame-label').length };
  }

  // overlap
  {
    const problems = [];
    const boxes = solids.map((e) => ({ e, b: box(e) }));
    for (let i = 0; i < boxes.length; i++) {
      for (let j = i + 1; j < boxes.length; j++) {
        const o = overlapArea(boxes[i].b, boxes[j].b);
        if (o) problems.push(`${boxes[i].e.type} ${short(boxes[i].e.id)} overlaps ${boxes[j].e.type} ${short(boxes[j].e.id)} by ${o.w}x${o.h}`);
      }
    }
    const fboxes = frames.map((f) => ({ f, b: box(f) }));
    for (let i = 0; i < fboxes.length; i++) {
      for (let j = i + 1; j < fboxes.length; j++) {
        const o = overlapArea(fboxes[i].b, fboxes[j].b);
        if (o) problems.push(`frame ${short(fboxes[i].f.id)} overlaps frame ${short(fboxes[j].f.id)} by ${o.w}x${o.h}`);
      }
    }
    for (const { e, b } of boxes) {
      for (const { f, b: fb } of fboxes) {
        if (e.frame === f.id) {
          if (!inside(b, fb)) problems.push(`${e.type} ${short(e.id)} sticks out of its frame ${short(f.id)}`);
        } else if (overlapArea(b, fb)) {
          problems.push(`${e.type} ${short(e.id)} lies over frame ${short(f.id)} without belonging to it`);
        }
      }
    }
    // Frame names: render.js draws them on the baseline 8 px above the frame
    // at 13 px, as wide as the text (at least 40 px, at most the frame).
    m.values.forEach((r, i) => {
      const { e: f, kind } = meta[i];
      if (kind !== 'frame-label') return;
      const lb = { x0: f.x, y0: f.y - 8 - FRAME_LABEL, x1: f.x + Math.min(Math.max(r.width, 1), Math.max(f.w, 40)), y1: f.y - 8 + 3 };
      for (const { e, b } of boxes) {
        if (overlapArea(lb, b)) problems.push(`${e.type} ${short(e.id)} covers the name of frame ${short(f.id)}`);
      }
    });
    checks.overlap = { pass: problems.length === 0, problems, elements: boxes.length, frames: frames.length };
  }

  // connectors
  {
    const problems = [];
    const notes = [];
    for (const [id, want] of Object.entries(base.connectors)) {
      const c = byId.get(id);
      if (!c) { problems.push(`connector ${id} (${want.ends.join(' -> ')}) is missing`); continue; }
      const got = (c.ends || []).map((x) => (x && x.el) || null);
      if (got.length !== 2 || got.some((g) => !g)) {
        problems.push(`connector ${id} has a free end: ${JSON.stringify(c.ends)}`);
        continue;
      }
      const same = got[0] === want.ends[0] && got[1] === want.ends[1];
      const reversed = got[0] === want.ends[1] && got[1] === want.ends[0];
      if (!same && !reversed) problems.push(`connector ${id} joins ${short(got[0])} -> ${short(got[1])}, intended ${short(want.ends[0])} -> ${short(want.ends[1])}`);
      else if (reversed) notes.push(`connector ${id} is attached to the intended elements but reversed`);
      for (const g of got) if (!byId.has(g)) problems.push(`connector ${id} is attached to ${g}, which does not exist`);
    }
    for (const c of board.elements.filter((e) => e.type === 'connector' && !base.connectors[e.id])) {
      const free = (c.ends || []).filter((x) => !x || !x.el).length;
      notes.push(`agent added connector ${c.id}${free ? ` with ${free} free end(s)` : ''}`);
    }
    checks.connectors = { pass: problems.length === 0, problems, notes, intended: Object.keys(base.connectors).length };
  }

  // sorted (the task, beyond the spec criterion)
  {
    const problems = [];
    const frameOf = {};
    for (const topic of base.topics) {
      const ids = Object.entries(base.stickies).filter(([, s]) => s.topic === topic).map(([id]) => id);
      const fs = new Set(ids.map((id) => (byId.get(id) || {}).frame || null));
      if (fs.size !== 1 || fs.has(null)) {
        problems.push(`topic ${topic}: stickies are in frames ${JSON.stringify([...fs])}`);
        continue;
      }
      const f = [...fs][0];
      frameOf[topic] = f;
      const fe = byId.get(f);
      if (!fe || !(fe.text || '').trim()) problems.push(`topic ${topic}: frame ${f} has no name`);
    }
    const used = Object.entries(frameOf);
    for (let i = 0; i < used.length; i++) {
      for (let j = i + 1; j < used.length; j++) {
        if (used[i][1] === used[j][1]) problems.push(`topics ${used[i][0]} and ${used[j][0]} share frame ${used[i][1]}`);
      }
    }
    const names = Object.fromEntries(Object.entries(frameOf).map(([t, f]) => [t, (byId.get(f) || {}).text || '']));
    checks.sorted = { pass: problems.length === 0, problems, frames: names };
  }

  const added = board.elements.filter((e) => !base.elements.some((b) => b.id === e.id));
  const criterion = ['kept', 'visible', 'overlap', 'text', 'connectors'].every((k) => checks[k].pass);
  const result = {
    slug: opt.slug,
    board: { seq: board.seq, revision: board.revision, pending: board.pending, elements: board.elements.length },
    added: added.map((e) => ({ id: e.id, type: e.type, text: e.text || '' })),
    criterion_pass: criterion,
    task_pass: checks.sorted.pass,
    pass: criterion && checks.sorted.pass,
    checks,
  };
  if (opt.json) writeFileSync(opt.json, JSON.stringify(result, null, 2) + '\n');

  console.log(`board ${opt.slug}: seq ${board.seq}, revision ${board.revision}, ${board.elements.length} elements (${added.length} added)`);
  for (const [k, c] of Object.entries(checks)) {
    console.log(`${c.pass ? 'PASS' : 'FAIL'} ${k}${k === 'text' ? ` (min ${c.min_px} px; ${c.engine})` : ''}${k === 'sorted' ? ` ${JSON.stringify(c.frames)}` : ''}`);
    for (const p of c.problems.slice(0, 25)) console.log(`  - ${p}`);
    if (c.problems.length > 25) console.log(`  ... ${c.problems.length - 25} more`);
    for (const n of c.notes || []) console.log(`  note: ${n}`);
  }
  console.log(`criterion: ${criterion ? 'PASS' : 'FAIL'}; task (sorted): ${checks.sorted.pass ? 'PASS' : 'FAIL'}`);
  process.exit(result.pass ? 0 : 1);
}

main().catch((err) => { console.error(`judge: ${err.message}`); process.exit(2); });
