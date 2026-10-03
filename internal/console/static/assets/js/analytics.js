import { h, icon } from './dom.js';
import { api } from './api.js';
import { fill, loading, errorPanel } from './ui.js';
import { siteHeader } from './site.js';

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
      fill(content,
        h('div', { class: 'analytics-summary' },
          h('dl', { class: 'analytics-metrics' },
            h('div', null, h('dt', { text: 'Unique Visitors' }), h('dd', { text: '—', title: 'Unique visitors are not tracked.' })),
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
        h('p', { class: 'muted small', text: stats.note || 'Page views count page requests. Unique visitors are not tracked.' }));
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

function trafficChart(data) {
  const ns = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(ns, 'svg');
  svg.setAttribute('viewBox', '0 0 720 290');
  svg.setAttribute('class', 'traffic-chart');
  svg.setAttribute('role', 'img');
  svg.setAttribute('aria-label', `Daily page views: ${data.map((d) => `${d.day}: ${d.count}`).join(', ')}`);
  const el = (tag, attrs, text) => {
    const node = document.createElementNS(ns, tag);
    for (const [key, value] of Object.entries(attrs)) node.setAttribute(key, value);
    if (text !== undefined) node.textContent = text;
    svg.appendChild(node);
    return node;
  };
  const max = Math.max(4, Math.ceil(Math.max(...data.map((d) => d.count)) / 4) * 4);
  for (let i = 0; i <= 4; i++) {
    const y = 250 - i * 56;
    el('line', { x1: 46, x2: 704, y1: y, y2: y, class: 'traffic-grid' });
    el('text', { x: 34, y: y + 4, 'text-anchor': 'end', class: 'traffic-label' }, String(max * i / 4));
  }
  const points = data.map((d, i) => `${46 + i * 658 / (data.length - 1)},${250 - d.count / max * 224}`).join(' ');
  el('polyline', { points, class: 'traffic-line' });
  const ticks = new Set(Array.from({ length: 5 }, (_, i) => Math.round(i * (data.length - 1) / 4)));
  data.forEach((d, i) => {
    if (!ticks.has(i)) return;
    el('text', { x: 46 + i * 658 / (data.length - 1), y: 278, 'text-anchor': i === 0 ? 'start' : i === data.length - 1 ? 'end' : 'middle', class: 'traffic-label' },
      new Date(d.day + 'T00:00:00Z').toLocaleDateString(undefined, { month: 'short', day: 'numeric', timeZone: 'UTC' }));
  });
  return svg;
}
