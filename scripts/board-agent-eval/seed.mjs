#!/usr/bin/env node
// seed.mjs creates the evaluation board: 30 stickies from three topics,
// scattered and overlapping, with mixed colors that carry no meaning, plus
// connectors between specific stickies. It writes the baseline the judge
// compares against: every seeded element, each sticky's topic and the
// intended end targets of each connector.
//
// usage: node seed.mjs --url http://127.0.0.1:17940 --slug feedback-1 --out baseline.json [--seed 1]

import { writeFileSync } from 'node:fs';
import { args, post, readBoard } from './lib.mjs';

const opt = args(process.argv.slice(2), { url: null, slug: null, out: null, seed: '1' });

const TOPICS = {
  onboarding: [
    'Sign-up form asks for too many fields',
    'Welcome tour skips the invite step',
    'Email verification link expires too fast',
    'First project template is confusing',
    'No progress indicator during account setup',
    'Onboarding checklist disappears after one day',
    'New users miss the import option on first login',
    'Password rules are shown only after an error',
    'Team invite during sign-up fails silently',
    'Sample data should be optional during setup',
  ],
  performance: [
    'Dashboard takes 8 seconds to load',
    'Search results lag while typing',
    'Exports time out on large projects',
    'Mobile app drains battery during background sync',
    'Image uploads freeze the editor',
    'Report page memory grows until the tab crashes',
    'API p95 latency doubled since the last release',
    'Calendar view stutters when scrolling',
    'Desktop app cold start takes too long',
    'Push notifications arrive minutes late',
  ],
  billing: [
    'Invoice PDF is missing the VAT number',
    'Card update fails for 3-D Secure cards',
    'Annual plan discount is not shown at checkout',
    'Refund requests have no status page',
    'Seat count does not drop after removing users',
    'Switching currency resets the cart',
    'Trial-end email arrives after the first charge',
    'Receipts are sent to the wrong email address',
    'Proration on plan change is confusing',
    'No way to download past invoices in bulk',
  ],
};

// Connectors between specific stickies: [topic, index] pairs, some across
// topics, some with a label or an explicit attachment point.
const LINKS = [
  { from: ['onboarding', 0], to: ['onboarding', 2] },
  { from: ['onboarding', 2], to: ['onboarding', 4], label: 'then' },
  { from: ['performance', 0], to: ['performance', 6], label: 'same root cause' },
  { from: ['billing', 1], to: ['billing', 3] },
  { from: ['onboarding', 8], to: ['billing', 4], label: 'seats' },
  { from: ['performance', 2], to: ['billing', 9], at: [[1, 0.5], [0, 0.5]] },
  { from: ['performance', 4], to: ['onboarding', 6] },
];

// Deterministic pseudo-random numbers (LCG), so a seed gives the same board.
let state = (Number(opt.seed) * 2654435761) >>> 0 || 1;
const rnd = () => ((state = (Math.imul(state, 1664525) + 1013904223) >>> 0) / 2 ** 32);

const COLORS = ['yellow', 'orange', 'pink', 'blue', 'green', 'gray'];
const order = [];
for (const [topic, texts] of Object.entries(TOPICS)) texts.forEach((t, i) => order.push({ topic, i, text: t }));
// Shuffle so topics are interleaved on the canvas.
for (let i = order.length - 1; i > 0; i--) {
  const j = Math.floor(rnd() * (i + 1));
  [order[i], order[j]] = [order[j], order[i]];
}

const ops = [];
order.forEach((s, k) => {
  const el = {
    type: 'sticky',
    text: s.text,
    x: Math.round(rnd() * 1100),
    y: Math.round(rnd() * 700),
    style: { color: COLORS[Math.floor(rnd() * COLORS.length)] },
  };
  // A few smaller stickies and two with a 10 px font, so readability is
  // not already satisfied by the seed.
  if (k % 7 === 3) { el.w = 150; el.h = 110; }
  if (k % 11 === 5) { el.w = 120; el.h = 90; }
  if (k === 8 || k === 21) el.style.fontSize = 10;
  ops.push({ op: 'add', ref: `$${s.topic}${s.i}`, el });
});
for (const l of LINKS) {
  const ends = [{ el: `$${l.from[0]}${l.from[1]}` }, { el: `$${l.to[0]}${l.to[1]}` }];
  if (l.at) { ends[0].at = l.at[0]; ends[1].at = l.at[1]; }
  const el = { type: 'connector', ends };
  if (l.label) el.text = l.label;
  ops.push({ op: 'add', ref: `$link_${l.from.join('')}_${l.to.join('')}`, el });
}

const res = await post(opt.url, `/api/flats/${encodeURIComponent(opt.slug)}/board`, {
  op_id: `seed-${opt.slug}-${opt.seed}`,
  ops,
  message: 'Seed: 30 unsorted feedback stickies',
});
const ids = res.ids;
const board = await readBoard(opt.url, opt.slug);

const stickies = {};
for (const [topic, texts] of Object.entries(TOPICS)) {
  texts.forEach((t, i) => { stickies[ids[`$${topic}${i}`]] = { topic, text: t }; });
}
const connectors = {};
for (const l of LINKS) {
  const id = ids[`$link_${l.from.join('')}_${l.to.join('')}`];
  connectors[id] = {
    ends: [ids[`$${l.from[0]}${l.from[1]}`], ids[`$${l.to[0]}${l.to[1]}`]],
    text: l.label || '',
  };
}

const baseline = {
  slug: opt.slug,
  seed: Number(opt.seed),
  seeded: { seq: board.seq, revision: board.revision },
  topics: Object.keys(TOPICS),
  stickies,
  connectors,
  elements: board.elements,
};
writeFileSync(opt.out, JSON.stringify(baseline, null, 2) + '\n');
console.log(`seeded ${opt.slug}: ${Object.keys(stickies).length} stickies, ${Object.keys(connectors).length} connectors, seq ${board.seq}, revision ${board.revision}`);
