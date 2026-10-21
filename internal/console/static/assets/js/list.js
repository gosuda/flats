// Landing page: every flat, with search, a list/grid toggle and row actions.

import { h, clear, icon, timeEl, relTime, visibilityBadge } from './dom.js';
import { api, thumbnail } from './api.js';
import { menu, errorPanel, loading, busy, openLinks, openPrimary, openMenuItems } from './ui.js';
import { saveVersion, publishVersion, nextVersion } from './actions.js';
import { shareDialog } from './share.js';
import { describeApproval } from './approval.js';

const VIEW_KEY = 'flats.view';
const APPROVAL_REFRESH_MS = 15000;

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

  let disposed = false;
  let approvalFlight = null;
  let approvalController = null;
  let approvalTimer;
  let trailingApproval = false;
  let pending = [];
  let pendingKey = null;
  let approvalError = false;
  let hadApprovalError = false;
  const alive = () => !disposed && ctx.alive();
  const approvalAnnouncement = h('p', { class: 'sr-only', role: 'status', 'aria-live': 'polite' });
  const banner = h('div', { class: 'banner-slot' });
  const approvalMessage = h('p', { class: 'muted small' });
  const retry = h('button', { type: 'button', class: 'btn btn-small', text: 'Retry' });
  const approvalFeedback = h('div', { class: 'field-group', hidden: true }, approvalMessage, retry);
  retry.addEventListener('click', () => refreshApprovals());
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
    approvalAnnouncement, approvalFeedback, banner,
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

  function load(afterAction = true) {
    loadFlats();
    return refreshApprovals(afterAction);
  }

  async function loadFlats() {
    try {
      const fl = await api.flats();
      if (!alive()) return;
      flats = fl.flats || [];
      draw();
    } catch (err) {
      if (!alive()) return;
      clear(results).appendChild(errorPanel(err, 'Cannot load flats'));
    }
  }

  function scheduleApprovals() {
    clearTimeout(approvalTimer);
    if (alive() && document.visibilityState === 'visible') {
      approvalTimer = setTimeout(() => refreshApprovals(), APPROVAL_REFRESH_MS);
    }
  }

  function showApprovalFeedback() {
    if (!hadApprovalError) return;
    approvalFeedback.hidden = false;
    approvalMessage.textContent = approvalFlight
      ? 'Checking approval requests…' + (approvalError && pending.length ? ' Requests below may be out of date.' : '')
      : approvalError
      ? 'Cannot check approval requests.' + (pending.length ? ' Requests below are from the last successful check and may be out of date.' : '')
      : 'Approval requests are up to date.';
    retry.textContent = approvalError ? 'Retry' : 'Check again';
    // Native disabled would discard keyboard focus while the request runs.
    retry.setAttribute('aria-disabled', String(!!approvalFlight));
    retry.setAttribute('aria-busy', String(!!approvalFlight));
  }

  function refreshApprovals(afterAction = false) {
    if (!alive()) return Promise.resolve();
    clearTimeout(approvalTimer);
    if (approvalFlight) {
      if (afterAction) trailingApproval = true;
      return approvalFlight;
    }
    approvalController = new AbortController();
    const controller = approvalController;
    const timeout = setTimeout(() => controller.abort(), APPROVAL_REFRESH_MS);
    approvalFlight = (async () => {
      try {
        const ap = await api.approvals('pending', controller.signal);
        if (!alive()) return;
        if (!ap || !(Array.isArray(ap.approvals) || ap.approvals === null)) throw new Error('Invalid approval response');
        const next = ap.approvals || [];
        const key = JSON.stringify(next.map((a) => [a.id, describeApproval(a), a.via, a.requested_at]));
        if (approvalError || (pendingKey !== null && key !== pendingKey)) {
          approvalAnnouncement.textContent = `${next.length} approval ${next.length === 1 ? 'request' : 'requests'} pending.` + (approvalError ? ' Approval requests are up to date.' : '');
        }
        approvalError = false;
        pending = next;
        if (key !== pendingKey) {
          pendingKey = key;
          drawBanner(pending);
        } else {
          // Keep the link nodes (and focus) while updating relative timestamps.
          for (const time of banner.querySelectorAll('time')) time.textContent = relTime(time.getAttribute('datetime'));
        }
      } catch {
        if (!alive()) return;
        if (!approvalError) approvalAnnouncement.textContent = 'Cannot check approval requests. Try again.';
        approvalError = true;
        hadApprovalError = true;
      } finally {
        clearTimeout(timeout);
        approvalFlight = null;
        approvalController = null;
        if (alive()) {
          showApprovalFeedback();
          if (trailingApproval) {
            trailingApproval = false;
            refreshApprovals();
          } else scheduleApprovals();
        }
      }
    })();
    showApprovalFeedback();
    return approvalFlight;
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
    // A document, design or board has no versions or publishing: it runs
    // once it is saved.
    const docs = isLive(f);
    const draft = !docs && f.draft && f.draft.dirty;
    const latest = f.latest_version || 0;
    // Save makes the draft a new version; Publish puts a saved version live.
    const unpublished = !docs && latest > 0 && !f.latest_deployed;
    const status = docs ? (f.running ? 'Running' : 'Not running')
      : (f.live_version
        ? `Live v${f.live_version}` + (unpublished ? ` · v${latest} saved, not published` : '')
        : 'Not published' + (unpublished ? ` · v${latest} saved` : ''))
        + (draft ? ' · unsaved draft changes' : '');
    const actions = h('div', { class: 'row-actions' });
    const name = f.name || f.slug;
    if (!docs && draft) {
      const n = nextVersion(f);
      const save = h('button', {
        type: 'button', class: 'btn btn-pill btn-quiet', text: `Save as v${n}`,
        title: `Save the draft as version ${n}. Nothing goes live.`,
        'aria-label': `Save the draft of ${name} as version ${n}`,
      });
      save.addEventListener('click', () => busy(save, async () => { if (await saveVersion(f)) load(); }));
      actions.appendChild(save);
    } else if (unpublished) {
      const pub = h('button', {
        type: 'button', class: 'btn btn-pill', text: `Publish v${latest}`,
        title: `Put saved version ${latest} live`,
        'aria-label': `Publish version ${latest} of ${name}`,
      });
      pub.addEventListener('click', () => busy(pub, async () => { if (await publishVersion(f, latest)) load(); }));
      actions.appendChild(pub);
    } else if (!docs && !f.live_version) {
      const save = h('button', {
        type: 'button', class: 'btn btn-pill btn-quiet', text: 'Save as v1', disabled: true,
        title: 'No draft saved yet',
        'aria-label': `Save the draft of ${name} as version 1`,
      });
      actions.appendChild(save);
    }
    // A document is open once it runs; a flat once a version is live.
    // Wide rows show both open shortcuts; narrow ones show one and move the
    // full list into the menu (CSS picks which).
    const openable = docs ? f.running : f.live_version;
    if (openable) {
      const open = openLinks(f);
      if (open) actions.appendChild(open);
      const primary = openPrimary(f);
      if (primary) actions.appendChild(primary);
    }
    actions.appendChild(menu(`More actions for ${f.name || f.slug}`, [
      ...(openable ? openMenuItems(f) : []),
      { label: 'Share', onSelect: () => shareDialog(f, () => load()) },
      { label: docs ? 'Overview' : 'Deployments', onSelect: () => ctx.navigate(href) },
      { label: 'Analytics', onSelect: () => ctx.navigate(href + '/analytics') },
      { label: 'Settings', onSelect: () => ctx.navigate(href + '/settings') },
    ]));
    return h('li', { class: 'flat' },
      view === 'grid' ? preview(f) : null,
      typeIcon(f),
      h('div', { class: 'flat-main' },
        h('a', { class: 'flat-name', href, 'data-nav': true, text: f.name || f.slug }),
        h('div', { class: 'flat-sub' }, h('span', { class: 'flat-sub-vis' }, visibilityBadge(f.visibility, f.public_access)), h('span', null, timeEl(f.updated_at), ` · ${status}`))),
      h('div', { class: 'flat-vis' }, visibilityBadge(f.visibility, f.public_access)),
      actions);
  }

  const onVisible = () => {
    clearTimeout(approvalTimer);
    if (document.visibilityState === 'visible') load(false);
  };
  document.addEventListener('visibilitychange', onVisible);
  setView(view);
  load(false);
  return () => {
    disposed = true;
    clearTimeout(approvalTimer);
    approvalController?.abort();
    document.removeEventListener('visibilitychange', onVisible);
  };
}

// preview is the grid card's picture: the flat's screenshot when it has one,
// otherwise a quiet placeholder with its type glyph.
function preview(f) {
  const tile = h('div', { class: 'preview preview-' + typeOf(f).key, 'aria-hidden': 'true' }, icon(typeOf(f).icon));
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
    h('p', { text: 'Connect Claude Code, Codex, Cursor or any MCP client, then ask for what you want to make. You can also deploy a folder with the flats CLI.' }),
    h('p', null, h('strong', { text: 'Website: ' }), '“Build a portfolio website.”', h('br'),
      h('strong', { text: 'Document: ' }), '“Create a Markdown project brief.”', h('br'),
      h('strong', { text: 'Page: ' }), '“Make a single HTML event page.”', h('br'),
      h('strong', { text: 'Design: ' }), '“Create a design canvas with two screens.”', h('br'),
      h('strong', { text: 'Board: ' }), '“Put this discussion on a board as sticky notes.”'),
    h('p', { text: 'Agents wait for your approval to save or publish a website or page version. Document, design and board changes take effect as soon as they are saved; who can open them is set separately in Share.' }),
    h('p', null, h('a', { class: 'btn btn-primary', href: '/settings/agents', 'data-nav': true, text: 'Connect an agent' })));
}

// Content types as the console names them. Website flats are just "Flat".
const TYPES = {
  flat: { key: 'flat', label: 'Flat', icon: 'home' },
  docs: { key: 'docs', label: 'Document', icon: 'doc' },
  page: { key: 'page', label: 'Page', icon: 'code' },
  design: { key: 'design', label: 'Design', icon: 'layout' },
  board: { key: 'board', label: 'Board', icon: 'board' },
};

// isLive reports a Revision-model flat (document, design or board): no
// versions or publishing; it runs once it is saved.
export const isLive = (flat) => flat.type === 'docs' || flat.type === 'design' || flat.type === 'board';

export const typeOf = (flat) => (Object.hasOwn(TYPES, flat.type) ? TYPES[flat.type] : TYPES.flat);

// typeNoun names a flat's type in running text: "document", "design", "board".
export const typeNoun = (flat) => typeOf(flat).label.toLowerCase();

// hasServer reports whether a flat can run server code, so its database,
// environment, secrets and server HTTP(S) permissions apply: documents run
// the built-in editor, designs and boards run inside the host, and pages are
// always static.
export const hasServer = (flat) => !isLive(flat) && flat.type !== 'page';

// typeIcon is the tile left of a flat's name. Its label shows on hover, and on
// focus for keyboard users and touch (a tap focuses it); screen readers
// announce the same label.
export function typeIcon(flat, large) {
  const t = typeOf(flat);
  return h('span', {
    class: `type-icon type-${t.key}` + (large ? ' type-icon-large' : ''),
    role: 'img', tabindex: '0', 'aria-label': t.label, 'data-tip': t.label,
  }, icon(t.icon));
}
