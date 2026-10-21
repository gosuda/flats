// Entry point: the history-API router for console and per-flat management pages.

import { h, clear } from './dom.js';
import { closeMenus, setHostProviders } from './ui.js';
import { api } from './api.js';
import { buildOf, buildLabel } from './build.js';
import * as listPage from './list.js';
import * as flatPage from './flat.js';
import * as approvalPage from './approval.js';
import * as settingsPage from './settings.js';
import * as analyticsPage from './analytics.js';
import * as databasePage from './database.js';

const routes = [
  { re: /^\/$/, page: listPage, section: 'flats' },
  { re: /^\/flats\/([^/]+)$/, page: flatPage, section: 'flats' },
  { re: /^\/flats\/([^/]+)\/settings$/, page: { mount: flatPage.mountSettings }, section: 'flats' },
  { re: /^\/flats\/([^/]+)\/analytics$/, page: analyticsPage, section: 'flats' },
  { re: /^\/flats\/([^/]+)\/database$/, page: databasePage, section: 'flats' },
  { re: /^\/approvals$/, page: { mount: approvalPage.mountList }, section: 'approvals' },
  { re: /^\/approvals\/([^/]+)$/, page: approvalPage, section: 'approvals' },
  { re: /^\/settings(?:\/([a-z]+)(?:\/([^/]+))?)?$/, page: settingsPage, section: 'settings' },
];

let cleanup = null;
let generation = 0;
let rendered = false; // false until the first page is drawn

function match(pathname) {
  for (const r of routes) {
    const m = pathname.match(r.re);
    if (!m) continue;
    try {
      return { route: r, params: m.slice(1).map((p) => (p === undefined ? undefined : decodeURIComponent(p))) };
    } catch {
      return null; // malformed escape: show "not found" instead of a blank page
    }
  }
  return null;
}

export function navigate(path, opts = {}) {
  if (opts.replace) history.replaceState(null, '', path);
  else history.pushState(null, '', path);
  render();
}

function render() {
  closeMenus();
  if (cleanup) { try { cleanup(); } catch { /* ignore */ } cleanup = null; }
  const gen = ++generation;
  const main = clear(document.getElementById('main'));
  const found = match(location.pathname);
  for (const a of document.querySelectorAll('.topnav a')) {
    if (found && a.dataset.section === found.route.section) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  }
  if (!found) {
    document.title = 'Not found · Flats';
    main.appendChild(h('section', { class: 'page' },
      h('h1', { text: 'Page not found' }),
      h('p', null, h('a', { href: '/', 'data-nav': true, text: 'Back to all flats' }))));
    return;
  }
  const ctx = {
    alive: () => gen === generation,
    navigate,
    setTitle: (t) => { document.title = t ? `${t} · Flats` : 'Flats'; },
  };
  cleanup = found.route.page.mount(main, found.params, ctx) || null;
  updateApprovalCount();
  // Move focus to the new page for keyboard and screen-reader users, except on
  // the first load where the browser keeps its own position.
  if (rendered) main.focus({ preventScroll: true });
  rendered = true;
  if (location.hash) {
    requestAnimationFrame(() => document.getElementById(location.hash.slice(1))?.scrollIntoView());
  } else {
    window.scrollTo(0, 0);
  }
}

document.addEventListener('click', (e) => {
  if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
  const a = e.target.closest('a[href]');
  if (!a || a.target || a.hasAttribute('download')) return;
  const url = new URL(a.href, location.href);
  if (url.origin !== location.origin || !match(url.pathname)) return;
  e.preventDefault();
  if (url.pathname === location.pathname && url.search === location.search && url.hash) {
    history.pushState(null, '', url.pathname + url.search + url.hash);
    document.getElementById(url.hash.slice(1))?.scrollIntoView();
    return;
  }
  navigate(url.pathname + url.search + url.hash);
});

window.addEventListener('popstate', render);

// The Approvals item in the top bar shows how many requests wait; it is
// read again on every page change.
let countSeq = 0;
function updateApprovalCount() {
  const seq = ++countSeq;
  api.approvals('pending').then((r) => {
    if (seq !== countSeq) return; // a newer request answers
    const n = (r.approvals || []).filter((a) => a.status === 'pending').length;
    const slot = document.getElementById('approval-count');
    if (!slot) return;
    slot.textContent = n ? String(n) : '';
    slot.hidden = !n;
    const link = slot.closest('a');
    if (link) link.setAttribute('aria-label', n ? `Approvals, ${n} waiting` : 'Approvals');
  }).catch(() => {});
}
window.addEventListener('flats:approvals', updateApprovalCount);

render();

// The note on links that open only on the Flats host names the reverse proxy
// while one is on. The host's providers arrive after the first page, which
// keeps the note without the proxy until then, or if the request fails.
api.providers().then((r) => setHostProviders(r.providers)).catch(() => {});

// The running build is read once per page load; without it the slot stays empty.
api.status().then((s) => {
  const label = buildLabel(buildOf(s));
  if (label) document.getElementById('build')?.appendChild(label);
}).catch(() => {});
