// Entry point: a small history-API router over four pages.

import { h, clear } from './dom.js';
import { closeMenus } from './ui.js';
import * as listPage from './list.js';
import * as flatPage from './flat.js';
import * as approvalPage from './approval.js';
import * as settingsPage from './settings.js';

const routes = [
  { re: /^\/$/, page: listPage, section: 'flats' },
  { re: /^\/flats\/([^/]+)$/, page: flatPage, section: 'flats' },
  { re: /^\/approvals\/([^/]+)$/, page: approvalPage, section: 'flats' },
  { re: /^\/settings$/, page: settingsPage, section: 'settings' },
];

let cleanup = null;
let generation = 0;
let rendered = false; // false until the first page is drawn

function match(pathname) {
  for (const r of routes) {
    const m = pathname.match(r.re);
    if (!m) continue;
    try {
      return { route: r, params: m.slice(1).map(decodeURIComponent) };
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

render();
