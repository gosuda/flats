import { h, icon } from './dom.js';
import { api } from './api.js';
import { fill, loading, errorPanel } from './ui.js';
import { siteHeader } from './site.js';

export function mount(main, [slug], ctx) {
  const slot = h('div', null, loading('Loading database…'));
  main.appendChild(h('section', { class: 'page site-page' },
    h('a', { class: 'back', href: '/', 'data-nav': true }, icon('back'), 'All flats'), slot));
  async function load() {
    try {
      const [flat, result] = await Promise.all([api.flat(slug), api.snapshots(slug)]);
      if (!ctx.alive()) return;
      ctx.setTitle(`${flat.name || slug} · Database`);
      const snapshots = result.snapshots || [];
      fill(slot, siteHeader(flat, 'database'),
        h('section', { class: 'settings-section' }, h('h2', { text: 'Database snapshots' }),
          h('p', { class: 'muted', text: 'Server flats use a SQLite database. Snapshots are saved automatically before deployment.' }),
          snapshots.length ? h('div', { class: 'table-wrap' }, h('table', { class: 'table' },
            h('thead', null, h('tr', null, ['Snapshot'].map((text) => h('th', { scope: 'col', text })))),
            h('tbody', null, snapshots.map((s) => h('tr', null, h('th', { scope: 'row', text: s }))))))
            : h('p', { class: 'empty-small', text: 'No database snapshots yet. Static flats do not use a database.' }),
          h('p', { class: 'muted small', text: result.note || 'Use rollback with restore_data to restore a snapshot from before the current version.' }),
          h('a', { class: 'btn btn-small', href: `/flats/${encodeURIComponent(slug)}#versions`, 'data-nav': true, text: 'Open deployments' })));
    } catch (err) { if (ctx.alive()) fill(slot, errorPanel(err, 'Cannot load database')); }
  }
  load();
}
