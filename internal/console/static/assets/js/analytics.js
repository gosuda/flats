import { h, icon, bytes, relTime, dateTime } from './dom.js';
import { api } from './api.js';
import { fill, loading, errorPanel } from './ui.js';
import { siteHeader } from './site.js';
import { isLive } from './list.js';

export function mount(main, [slug], ctx) {
  let days = 30, generation = 0;
  const header = h('div', null, loading());
  const content = h('div');
  const page = h('section', { class: 'page site-page' },
    h('a', { class: 'back', href: '/', 'data-nav': true }, icon('back'), 'All flats'), header, content);
  main.appendChild(page);
  async function load() {
    const gen = ++generation;
    fill(content, loading('Loading analytics…'));
    try {
      const [flat, stats] = await Promise.all([api.flat(slug), api.stats(slug, days)]);
      if (!ctx.alive() || gen !== generation) return;
      ctx.setTitle(`${flat.name || slug} · Analytics`);
      fill(header, siteHeader(flat, 'analytics'));
      const data = dailySeries(stats.page_views || [], days);
      const total = data.reduce((sum, day) => sum + day.count, 0);
      const range = h('div', { class: 'segmented', role: 'group', 'aria-label': 'Analytics date range' }, [7, 30].map((n) => {
        const btn = h('button', { type: 'button', class: 'seg range-btn', 'aria-pressed': String(days === n), text: `${n}d` });
        btn.addEventListener('click', () => { days = n; load(); });
        return btn;
      }));
      const rows = stats.top_pages || [];
      const revisionModel = isLive(flat);
      fill(content,
        h('div', { class: 'analytics-summary' },
          h('dl', { class: 'analytics-metrics' },
            h('div', null, h('dt', { text: 'Page Views' }), h('dd', { text: total.toLocaleString() }))), range),
        h('section', { class: 'analytics-panel' }, h('h2', { text: 'Traffic over time' }),
          h('p', { class: 'muted small', text: 'Daily · UTC' }), trafficChart(data),
          h('div', { class: 'chart-legend' }, h('span', { class: 'legend-dot' }), 'Page Views'),
          total === 0 ? h('p', { class: 'muted small', text: 'No page views in this period. Traffic appears after visitors open your site.' }) : null),
        h('section', { class: 'analytics-panel' }, h('div', { class: 'table-wrap' },
          h('table', { class: 'table top-pages' }, h('caption', { class: 'sr-only', text: 'Top pages in selected period' }),
            h('thead', null, h('tr', null, h('th', { scope: 'col', text: 'Top pages' }), h('th', { scope: 'col', text: 'Page Views' }))),
            h('tbody', null, rows.length ? rows.map((r) => h('tr', null, h('th', { scope: 'row', text: r.path }), h('td', { text: r.count.toLocaleString() })))
              : h('tr', null, h('td', { colspan: '2', class: 'muted', text: 'No per-page traffic recorded in this period.' })))))),
        h('p', { class: 'muted small', text: stats.note || 'Page views count page requests. Unique visitors are not tracked.' }),
        usagePanel(stats.usage, days),
        h('section', { class: 'analytics-panel' }, h('h2', { text: 'Storage' }),
          h('dl', { class: 'facts' },
            h('dt', { text: 'Disk' }), h('dd', { text: revisionModel ? bytes(flat.disk_bytes) : `${bytes(flat.disk_bytes)} (versions, data and previews)` }),
            revisionModel ? null : [h('dt', { text: 'Versions' }), h('dd', { text: String(flat.versions) })])));
    } catch (err) { if (ctx.alive() && gen === generation) fill(content, errorPanel(err, 'Cannot load analytics')); }
  }
  load();
}

export function dailySeries(rows, days, today = new Date()) {
  const counts = new Map(rows.map((r) => [r.day, r.count]));
  return Array.from({ length: days }, (_, i) => {
    const day = new Date(Date.UTC(today.getUTCFullYear(), today.getUTCMonth(), today.getUTCDate() - days + i + 1)).toISOString().slice(0, 10);
    return { day, count: counts.get(day) || 0 };
  });
}

// chartFrame draws the shared axes of the analytics charts on a 720x290
// canvas: four grid lines up to max and five date ticks. xAt maps a day's
// index to its x coordinate.
function chartFrame(className, label, data, max, xAt) {
  const ns = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(ns, 'svg');
  svg.setAttribute('viewBox', '0 0 720 290');
  svg.setAttribute('class', className);
  svg.setAttribute('role', 'img');
  svg.setAttribute('aria-label', label);
  const el = (tag, attrs, text, parent = svg) => {
    const node = document.createElementNS(ns, tag);
    for (const [key, value] of Object.entries(attrs)) node.setAttribute(key, value);
    if (text !== undefined) node.textContent = text;
    parent.appendChild(node);
    return node;
  };
  for (let i = 0; i <= 4; i++) {
    const y = 250 - i * 56;
    el('line', { x1: 46, x2: 704, y1: y, y2: y, class: 'traffic-grid' });
    el('text', { x: 34, y: y + 4, 'text-anchor': 'end', class: 'traffic-label' }, String(max * i / 4));
  }
  const ticks = new Set(Array.from({ length: 5 }, (_, i) => Math.round(i * (data.length - 1) / 4)));
  data.forEach((d, i) => {
    if (!ticks.has(i)) return;
    el('text', { x: xAt(i), y: 278, 'text-anchor': i === 0 ? 'start' : i === data.length - 1 ? 'end' : 'middle', class: 'traffic-label' },
      shortDay(d.day));
  });
  return { svg, el };
}

const chartMax = (values) => Math.max(4, Math.ceil(Math.max(0, ...values) / 4) * 4);
const shortDay = (day) => new Date(day + 'T00:00:00Z').toLocaleDateString(undefined, { month: 'short', day: 'numeric', timeZone: 'UTC' });

function trafficChart(data) {
  const max = chartMax(data.map((d) => d.count));
  const xAt = (i) => 46 + i * 658 / (data.length - 1);
  const { svg, el } = chartFrame('traffic-chart', `Daily page views: ${data.map((d) => `${d.day}: ${d.count}`).join(', ')}`, data, max, xAt);
  const points = data.map((d, i) => `${xAt(i)},${250 - d.count / max * 224}`).join(' ');
  el('polyline', { points, class: 'traffic-line' });
  return svg;
}

// --- usage ---

// Status classes in stacking order (bottom first) with their legend names.
// 1xx (WebSocket upgrades) appears only when the period has some.
export const STATUS_CLASSES = [
  ['2xx', '2xx'], ['3xx', '3xx'], ['4xx', '4xx'], ['5xx', '5xx'], ['closed', 'Closed'], ['1xx', '1xx'],
];

// Worker states and their console labels (docs/internal/metrics.md).
export const WORKER_STATES = {
  running: 'Running', starting: 'Starting', quiescing: 'Going to sleep',
  sleeping: 'Sleeping', backoff: 'Restarting', stopped: 'Stopped',
};

export const NOT_SEPARATE_NOTE = 'CPU and memory are not measured separately for this type; it runs inside the Flats process.';
export const MEMORY_UNSUPPORTED_NOTE = 'Memory is not measured on this system.';
const DASH = '—';
const num = (v) => (typeof v === 'number' && Number.isFinite(v) ? v : null);
const trim = (v, digits) => String(Number(v.toFixed(digits)));

// duration formats milliseconds; null (nothing timed) is an em dash.
export function duration(ms) {
  ms = num(ms);
  if (ms === null) return DASH;
  if (ms < 10) return `${trim(ms, ms < 1 ? 2 : 1)} ms`;
  if (ms < 1000) return `${Math.round(ms)} ms`;
  const s = ms / 1000;
  if (s < 60) return `${trim(s, s < 10 ? 2 : 1)} s`;
  return `${trim(s / 60, 1)} min`;
}

// percent formats a 0..1 ratio; null (no requests) is an em dash.
export function percent(ratio) {
  ratio = num(ratio);
  if (ratio === null) return DASH;
  const p = ratio * 100;
  if (p === 0) return '0%';
  if (p < 0.01) return '<0.01%';
  return `${trim(p, p < 10 ? 2 : 1)}%`;
}

// cpuTime formats CPU seconds.
export function cpuTime(seconds) {
  seconds = num(seconds);
  if (seconds === null) return DASH;
  if (seconds === 0) return '0 s';
  if (seconds < 1) return `${Math.max(1, Math.round(seconds * 1000))} ms`;
  if (seconds < 60) return `${trim(seconds, seconds < 10 ? 2 : 1)} s`;
  if (seconds < 3600) {
    const t = Math.round(seconds);
    return `${Math.floor(t / 60)} min ${t % 60} s`;
  }
  const m = Math.round(seconds / 60);
  return `${Math.floor(m / 60)} h ${m % 60} min`;
}

// usageSeries fills the period's days; days without usage count zero.
export function usageSeries(rows, days, today = new Date()) {
  const byDay = new Map((rows || []).map((r) => [r.day, r]));
  return dailySeries([], days, today).map(({ day }) => {
    const row = byDay.get(day) || {};
    const status = row.status || {};
    const classes = Object.fromEntries(STATUS_CLASSES.map(([key]) => [key, num(status[key]) || 0]));
    const total = Object.values(classes).reduce((a, b) => a + b, 0);
    return { day, requests: num(row.requests) ?? total, classes, rateLimited: num(row.rate_limited) || 0 };
  });
}

const tile = (label, value, cls) => h('div', null, h('dt', { text: label }), h('dd', { class: cls, text: value }));

// usagePanel renders the Usage section from stats.usage, or nothing for a
// server that does not report usage.
function usagePanel(usage, days) {
  if (!usage || typeof usage !== 'object') return null;
  const totals = usage.totals || {};
  const now = usage.now || {};
  const notSeparate = now.cpu === 'not_separate' || now.memory === 'not_separate';
  const memoryUnsupported = now.memory === 'unsupported';
  const requests = num(totals.requests) || 0;
  const tiles = [
    tile('Requests', requests.toLocaleString()),
    tile('Error rate', percent(totals.error_rate)),
    tile('Median response time', duration(totals.p50_ms)),
    tile('p95 response time', duration(totals.p95_ms)),
  ];
  if (!notSeparate) {
    tiles.push(tile('CPU time', now.cpu === 'unsupported' ? 'Not measured' : cpuTime(totals.cpu_seconds)));
    tiles.push(memoryUnsupported ? tile('Peak memory', MEMORY_UNSUPPORTED_NOTE, 'usage-note')
      : tile('Peak memory', num(totals.memory_peak_bytes) === null ? DASH : bytes(totals.memory_peak_bytes)));
  }
  const series = usageSeries(usage.days, days);
  const shown = STATUS_CLASSES.filter(([key]) => key !== '1xx' || series.some((d) => d.classes[key] > 0));
  return h('section', { class: 'analytics-panel usage-panel', 'aria-labelledby': 'usage-heading' },
    h('h2', { id: 'usage-heading', text: 'Usage' }),
    h('p', { class: 'muted small', text: 'Requests to the live route, private and public · UTC' }),
    h('dl', { class: 'analytics-metrics usage-metrics' }, tiles),
    notSeparate ? h('p', { class: 'muted small usage-note', text: NOT_SEPARATE_NOTE }) : nowLine(now),
    h('h3', { class: 'usage-chart-title', text: 'Requests by status' }),
    requestsChart(series, shown),
    h('div', { class: 'chart-legend' }, shown.map(([key, name]) =>
      h('span', { class: 'legend-item' }, h('span', { class: `legend-dot status-${key}` }), name))),
    requests === 0 ? h('p', { class: 'muted small', text: 'No requests in this period.' }) : null,
    h('p', { class: 'muted small', text: 'Errors are 5xx answers. Closed requests, whose visitor left before the answer was sent, are not errors.' }));
}

// nowLine is the current worker state and memory of a flat with a worker.
// Unsupported memory is said once, in the Peak memory tile.
function nowLine(now) {
  const parts = [];
  if (now.state) parts.push(`Worker: ${WORKER_STATES[now.state] || now.state}`);
  if (num(now.workers) > 1) parts.push(`${now.workers} worker processes`);
  if (now.memory === 'measured' && num(now.memory_bytes) !== null) {
    let mem = `Memory: ${bytes(now.memory_bytes)}`;
    if (now.memory_stale) {
      const age = now.memory_sampled_at ? relTime(now.memory_sampled_at) : '';
      mem += age ? ` (last sampled ${age})` : ' (no recent sample)';
    }
    parts.push(mem);
  }
  if (!parts.length) return null;
  const line = h('p', { class: 'usage-now small' }, h('span', { class: 'muted', text: 'Now' }), ' ', parts.join(' · '));
  if (now.memory_stale && now.memory_sampled_at) line.setAttribute('title', `Memory sampled ${dateTime(now.memory_sampled_at)}`);
  return line;
}

// requestsChart draws the daily requests as bars stacked by status class.
function requestsChart(series, shown) {
  const totals = series.map((d) => shown.reduce((sum, [key]) => sum + d.classes[key], 0));
  const max = chartMax(totals);
  const slot = 658 / series.length;
  const xAt = (i) => 46 + (i + 0.5) * slot;
  const describe = (d) => {
    const parts = shown.filter(([key]) => d.classes[key] > 0).map(([key, name]) => `${name} ${d.classes[key]}`);
    if (d.rateLimited) parts.push(`${d.rateLimited} rate limited`);
    return `${d.day}: ${d.requests} requests${parts.length ? ` (${parts.join(', ')})` : ''}`;
  };
  const { svg, el } = chartFrame('traffic-chart usage-chart', `Daily requests by status: ${series.map(describe).join('; ')}`, series, max, xAt);
  const width = Math.max(2, slot * 0.7);
  series.forEach((d, i) => {
    if (!totals[i]) return;
    const group = el('g', { class: 'usage-day' });
    el('title', {}, describe(d), group);
    let y = 250;
    for (const [key] of shown) {
      const n = d.classes[key];
      if (!n) continue;
      const height = n / max * 224;
      y -= height;
      el('rect', { x: xAt(i) - width / 2, y, width, height, class: `usage-bar status-${key}` }, undefined, group);
    }
  });
  return svg;
}
