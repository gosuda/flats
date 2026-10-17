// Analytics Usage section (docs/spec/metrics.md): tiles, the "now" line, the
// stacked requests chart and the notes for flats without their own worker.
// Reuses the console's DOM harness and runs its regression suite first; the
// Go runner (console_usage_test.go) copies both scripts.
import './ui_test.mjs';
import assert from 'node:assert/strict';
import { mount as analytics, duration, percent, cpuTime, usageSeries } from './analytics.js';

const ctx = { alive: () => true, setTitle() {} };
const tick = () => new Promise((resolve) => setTimeout(resolve, 20));
const all = (root, pred) => [root, ...(root.childNodes || []).flatMap((n) => all(n, () => true))].filter(pred);
const tileValue = (root, label) => {
  const dt = all(root, (n) => n.tagName === 'DT' && n.textContent === label)[0];
  if (!dt) return undefined;
  return all(dt.parentNode, (n) => n.tagName === 'DD')[0].textContent;
};
const section = (root) => all(root, (n) => n.tagName === 'SECTION' && (n.className || '').includes('usage-panel'))[0];
const today = new Date().toISOString().slice(0, 10);
const NOTE = 'CPU and memory are not measured separately for this type; it runs inside the Flats process.';
const UNSUPPORTED = 'Memory is not measured on this system.';

let flat, stats;
globalThis.fetch = async (url) => {
  let body = {};
  if (url.includes('/stats?')) body = stats;
  else if (url === '/console/api/flats/sample') body = flat;
  return { ok: true, status: 200, json: async () => body };
};
async function render(type, usage) {
  flat = { slug: 'sample', name: 'Sample', type, disk_bytes: 2048, versions: 3, visibility: 'private', endpoints: [] };
  stats = { page_views: [{ day: today, count: 5 }], top_pages: [], ...(usage === undefined ? {} : { usage }) };
  const main = document.createElement('main');
  analytics(main, ['sample'], ctx);
  await tick();
  return main;
}

// Formatting.
assert.equal(duration(null), '—');
assert.equal(duration(0.5), '0.5 ms');
assert.equal(duration(2.25), '2.3 ms');
assert.equal(duration(25), '25 ms');
assert.equal(duration(1500), '1.5 s');
assert.equal(duration(30000), '30 s');
assert.equal(percent(null), '—');
assert.equal(percent(0), '0%');
assert.equal(percent(0.0167), '1.67%');
assert.equal(percent(0.5), '50%');
assert.equal(percent(0.00001), '<0.01%');
assert.equal(cpuTime(0), '0 s');
assert.equal(cpuTime(0.42), '420 ms');
assert.equal(cpuTime(4.2), '4.2 s');
assert.equal(cpuTime(125), '2 min 5 s');
assert.equal(cpuTime(7260), '2 h 1 min');
const series = usageSeries([{ day: '2026-10-02', requests: 3, status: { '2xx': 2, closed: 1 } }], 2, new Date('2026-10-03T12:00:00Z'));
assert.deepEqual(series.map((d) => [d.day, d.requests, d.classes['2xx'], d.classes.closed]), [['2026-10-02', 3, 2, 1], ['2026-10-03', 0, 0, 0]]);

// A server flat with measured usage.
const server = {
  days: [{ day: today, requests: 120, status: { '2xx': 110, '4xx': 7, '5xx': 2, closed: 1 }, rate_limited: 3, cpu_seconds: 4.2, memory_peak_bytes: 52428800 }],
  totals: { requests: 120, errors: 2, error_rate: 0.0167, p50_ms: 25, p95_ms: 250, cpu_seconds: 4.2, memory_peak_bytes: 52428800 },
  now: { cpu: 'measured', memory: 'measured', state: 'running', workers: 1, memory_bytes: 50331648, memory_sampled_at: new Date().toISOString(), memory_stale: false },
};
let main = await render('flat', server);
let usage = section(main);
assert.ok(usage, 'a server flat shows Usage');
assert.ok(main.textContent.indexOf('Traffic over time') < main.textContent.indexOf('Usage'), 'Usage comes after Page Views');
assert.ok(main.textContent.indexOf('Usage') < main.textContent.indexOf('Storage'), 'Storage stays after Usage');
assert.equal(tileValue(usage, 'Requests'), (120).toLocaleString());
assert.equal(tileValue(usage, 'Error rate'), '1.67%');
assert.equal(tileValue(usage, 'Median response time'), '25 ms');
assert.equal(tileValue(usage, 'p95 response time'), '250 ms');
assert.equal(tileValue(usage, 'CPU time'), '4.2 s');
assert.equal(tileValue(usage, 'Peak memory'), '50 MB');
assert.ok(usage.textContent.includes('Worker: Running'));
assert.ok(usage.textContent.includes('Memory: 48 MB'));
assert.ok(!usage.textContent.includes('last sampled'), 'a fresh sample shows no age');
assert.ok(!usage.textContent.includes(NOTE));
const chart = all(usage, (n) => n.tagName === 'SVG' && (n.getAttribute('class') || '').includes('usage-chart'))[0];
assert.ok(chart, 'the daily requests chart is drawn');
assert.equal(chart.getAttribute('role'), 'img');
assert.ok(chart.getAttribute('aria-label').includes(`${today}: 120 requests (2xx 110, 4xx 7, 5xx 2, Closed 1, 3 rate limited)`), chart.getAttribute('aria-label'));
assert.deepEqual(all(chart, (n) => n.tagName === 'RECT').map((n) => n.getAttribute('class')),
  ['usage-bar status-2xx', 'usage-bar status-4xx', 'usage-bar status-5xx', 'usage-bar status-closed']);
const legend = all(usage, (n) => (n.className || '') === 'legend-item').map((n) => n.textContent);
assert.deepEqual(legend, ['2xx', '3xx', '4xx', '5xx', 'Closed'], 'no 1xx in the legend without upgrades');
assert.equal(all(usage, (n) => n.tagName === 'H2')[0].getAttribute('id'), 'usage-heading');
assert.equal(usage.getAttribute('aria-labelledby'), 'usage-heading');
assert.equal(all(main, (n) => n.tagName === 'DT' && n.textContent === 'Disk').length, 1, 'the Storage tile is unchanged');

// WebSocket upgrades add a 1xx series; the worker states use console labels.
for (const [state, label] of [['starting', 'Starting'], ['quiescing', 'Going to sleep'], ['sleeping', 'Sleeping'], ['backoff', 'Restarting'], ['stopped', 'Stopped']]) {
  main = await render('docs', { ...server, days: [{ day: today, requests: 4, status: { '1xx': 1, '2xx': 3 } }], now: { ...server.now, state, workers: 0, memory_bytes: 0 } });
  usage = section(main);
  assert.ok(usage.textContent.includes(`Worker: ${label}`), `${state} shows ${label}`);
  assert.ok(all(usage, (n) => (n.className || '') === 'legend-item').some((n) => n.textContent === '1xx'));
}

// A website runs inside the host process: one note replaces CPU and memory.
main = await render('page', {
  days: [], totals: { requests: 0, errors: 0, error_rate: null, p50_ms: null, p95_ms: null },
  now: { cpu: 'not_separate', memory: 'not_separate' },
});
usage = section(main);
assert.ok(usage.textContent.includes(NOTE));
assert.equal(tileValue(usage, 'CPU time'), undefined);
assert.equal(tileValue(usage, 'Peak memory'), undefined);
assert.ok(!usage.textContent.includes('Worker:'), 'no worker state for a website');
assert.ok(!usage.textContent.includes('Memory:'));
// Null percentiles and error rate show an em dash.
assert.equal(tileValue(usage, 'Error rate'), '—');
assert.equal(tileValue(usage, 'Median response time'), '—');
assert.equal(tileValue(usage, 'p95 response time'), '—');
assert.ok(usage.textContent.includes('No requests in this period.'));
assert.equal(all(usage, (n) => n.tagName === 'RECT').length, 0);

// Memory is not measured on this system (non-Linux host).
main = await render('docs', { ...server, now: { cpu: 'measured', memory: 'unsupported', state: 'running', workers: 1 } });
usage = section(main);
assert.equal(tileValue(usage, 'Peak memory'), UNSUPPORTED);
assert.equal(tileValue(usage, 'CPU time'), '4.2 s');
assert.ok(usage.textContent.includes('Worker: Running'));
assert.ok(!usage.textContent.includes('Memory:'));
assert.ok(!usage.textContent.includes(NOTE));

// A stale memory sample shows its age.
const old = new Date(Date.now() - 5 * 60 * 1000).toISOString();
main = await render('flat', { ...server, now: { ...server.now, memory_stale: true, memory_sampled_at: old } });
usage = section(main);
const now = all(usage, (n) => (n.className || '').includes('usage-now'))[0];
assert.ok(/Memory: 48 MB \(last sampled .+\)/.test(now.textContent), now.textContent);
assert.ok(now.getAttribute('title').startsWith('Memory sampled '));

// An older server without usage: the section is hidden.
main = await render('flat', undefined);
assert.equal(section(main), undefined);
assert.ok(!main.textContent.includes('Usage'));
assert.ok(main.textContent.includes('Traffic over time'));
assert.ok(main.textContent.includes('Storage'));

// Server data stays text: a hostile state name is not parsed as markup.
main = await render('flat', { ...server, now: { ...server.now, state: '<img src=x onerror=alert(1)>' } });
usage = section(main);
assert.ok(usage.textContent.includes('Worker: <img src=x onerror=alert(1)>'));
assert.equal(all(usage, (n) => n.tagName === 'IMG').length, 0);
console.log('usage ok');
