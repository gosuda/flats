// Landing page: every flat, with search, a list/grid toggle and row actions.

import { h, clear, icon, timeEl, visibilityBadge, slugHue, initials, publicURL } from './dom.js';
import { api, thumbnail } from './api.js';
import { menu, errorPanel, loading, extLink, busy } from './ui.js';
import { publishDraft } from './actions.js';
import { shareDialog } from './share.js';
import { describeApproval } from './approval.js';

const VIEW_KEY = 'flats.view';

function storedView() {
  try { return localStorage.getItem(VIEW_KEY) === 'grid' ? 'grid' : 'list'; } catch { return 'list'; }
}
function storeView(v) {
  try { localStorage.setItem(VIEW_KEY, v); } catch { /* private mode */ }
}

export function mount(main, _params, ctx) {
  ctx.setTitle('');
  let flats = [];
  let view = storedView();

  const banner = h('div', { class: 'banner-slot' });
  const searchId = 'flat-search';
  const search = h('input', {
    id: searchId, type: 'search', placeholder: 'Search flats', autocomplete: 'off', spellcheck: 'false',
  });
  const listBtn = h('button', { type: 'button', class: 'seg', 'aria-label': 'List view', title: 'List view' }, icon('list'));
  const gridBtn = h('button', { type: 'button', class: 'seg', 'aria-label': 'Grid view', title: 'Grid view' }, icon('grid'));
  const results = h('div', { class: 'results' }, loading());
  const count = h('p', { class: 'sr-only', role: 'status', 'aria-live': 'polite' });

  main.appendChild(h('section', { class: 'page' },
    h('div', { class: 'page-head' }, h('h1', { text: 'Flats' })),
    banner,
    h('div', { class: 'toolbar' },
      h('div', { class: 'search' },
        h('label', { for: searchId, class: 'sr-only', text: 'Search flats' }),
        icon('search'), search),
      h('div', { class: 'segmented', role: 'group', 'aria-label': 'Layout' }, listBtn, gridBtn)),
    count,
    results));

  function setView(v) {
    view = v;
    storeView(v);
    listBtn.setAttribute('aria-pressed', String(v === 'list'));
    gridBtn.setAttribute('aria-pressed', String(v === 'grid'));
    draw();
  }
  listBtn.addEventListener('click', () => setView('list'));
  gridBtn.addEventListener('click', () => setView('grid'));
  search.addEventListener('input', draw);

  async function load() {
    try {
      const [fl, ap] = await Promise.all([api.flats(), api.approvals('pending').catch(() => ({ approvals: [] }))]);
      if (!ctx.alive()) return;
      flats = fl.flats || [];
      drawBanner(ap.approvals || []);
      draw();
    } catch (err) {
      if (!ctx.alive()) return;
      clear(results).appendChild(errorPanel(err, 'Cannot load flats'));
    }
  }

  function drawBanner(pending) {
    clear(banner);
    if (!pending.length) return;
    banner.appendChild(h('div', { class: 'banner', role: 'region', 'aria-label': 'Pending approvals' },
      icon('alert'),
      h('div', null,
        h('strong', { text: pending.length === 1 ? 'An agent is waiting for your approval' : `${pending.length} requests are waiting for your approval` }),
        h('ul', null, pending.map((a) => h('li', null,
          h('a', { href: `/approvals/${encodeURIComponent(a.id)}`, 'data-nav': true, text: describeApproval(a) }),
          h('span', { class: 'muted', text: ` · via ${a.via} · ` }), timeEl(a.requested_at)))))));
  }

  function draw() {
    const q = search.value.trim().toLowerCase();
    const shown = flats.filter((f) => !q || f.slug.includes(q) || (f.name || '').toLowerCase().includes(q));
    clear(results);
    count.textContent = q ? `${shown.length} of ${flats.length} flats match` : '';
    if (!flats.length) {
      results.appendChild(emptyState());
      return;
    }
    if (!shown.length) {
      results.appendChild(h('p', { class: 'empty-small', text: `No flats match “${search.value.trim()}”.` }));
      return;
    }
    const list = h('ul', { class: 'flats flats-' + view, role: 'list' });
    if (view === 'list') {
      results.appendChild(h('div', { class: 'list-head', 'aria-hidden': 'true' },
        h('span', { text: 'Name' }), h('span', { text: 'Visibility' }), h('span')));
    }
    for (const f of shown) list.appendChild(row(f));
    results.appendChild(list);
  }

  function row(f) {
    const href = `/flats/${encodeURIComponent(f.slug)}`;
    const draft = f.draft && f.draft.dirty;
    const status = (f.live_version ? `Live v${f.live_version}` : 'Not published') + (f.live_version && draft ? ' · draft changes' : '');
    const actions = h('div', { class: 'row-actions' });
    if (!f.live_version || draft) {
      const pub = h('button', {
        type: 'button', class: 'btn btn-pill', text: 'Publish', disabled: !draft,
        title: draft ? 'Publish the draft' : 'No draft saved yet',
        'aria-label': `Publish ${f.name || f.slug}`,
      });
      pub.addEventListener('click', () => busy(pub, async () => { if (await publishDraft(f)) load(); }));
      actions.appendChild(pub);
    }
    if (f.live_version) {
      actions.appendChild(extLink(f.private_url, icon('external', `Open ${f.name || f.slug} (private URL)`), 'icon-btn'));
      if (publicURL(f)) {
        actions.appendChild(extLink(publicURL(f), icon('globe', `Open ${f.name || f.slug} (public URL: anyone with it can open the flat)`), 'icon-btn'));
      }
    }
    actions.appendChild(menu(`More actions for ${f.name || f.slug}`, [
      { label: 'Share', onSelect: () => shareDialog(f, load) },
      { label: 'Analytics', onSelect: () => ctx.navigate(href + '/analytics') },
      { label: 'Settings', onSelect: () => ctx.navigate(href + '/settings') },
    ]));
    return h('li', { class: 'flat' },
      thumb(f),
      h('div', { class: 'flat-main' },
        extLink(publicURL(f) || f.private_url, f.name || f.slug, 'flat-name'),
        typeBadge(f),
        h('div', { class: 'flat-sub' }, timeEl(f.updated_at), ` · ${status}`)),
      h('div', { class: 'flat-vis' }, visibilityBadge(f.visibility)),
      actions);
  }

  const onVisible = () => { if (document.visibilityState === 'visible') load(); };
  document.addEventListener('visibilitychange', onVisible);
  setView(view);
  load();
  return () => document.removeEventListener('visibilitychange', onVisible);
}

export function thumb(f, large) {
  const tile = h('div', { class: 'thumb' + (large ? ' thumb-large' : ''), 'aria-hidden': 'true' },
    h('span', { text: initials(f.name, f.slug) }));
  tile.style.setProperty('--hue', String(slugHue(f.slug)));
  if (f.thumbnail) {
    thumbnail(f.thumbnail).then((src) => {
      if (!src) return;
      clear(tile).appendChild(h('img', { src, alt: '', loading: 'lazy', decoding: 'async' }));
      tile.classList.add('has-img');
    });
  }
  return tile;
}

function emptyState() {
  return h('div', { class: 'empty' },
    h('h2', { text: 'No flats yet' }),
    h('p', { text: 'Agents create flats for you. Connect Claude Code, Codex, Cursor or any MCP client to this host, then ask it to build and publish a site; or use the flats CLI to deploy a folder.' }),
    h('p', null, h('a', { class: 'btn btn-primary', href: '/settings#connect', 'data-nav': true, text: 'Connect an agent' })));
}

export function typeBadge(flat) {
  return h('span', { class: 'badge content-type', text: flat.type === 'docs' ? 'Document' : 'Website' });
}
