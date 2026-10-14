import { h, icon, linkTargets } from './dom.js';
import { extLink } from './ui.js';
import { hasServer, typeIcon } from './list.js';

// The management tabs of a flat; Deployments is the flat's own address. A
// document has no deployments, so its first tab is its Overview.
const TABS = [['deployments', ''], ['analytics', '/analytics'], ['database', '/database'], ['settings', '/settings']];
const tabLabel = (flat, tab) => (tab === 'deployments' && (flat.type === 'docs' || flat.type === 'design') ? 'Overview' : tab[0].toUpperCase() + tab.slice(1));

export function siteHeader(flat, active) {
  const base = `/flats/${encodeURIComponent(flat.slug)}`;
  return h('div', { class: 'site-heading' },
    h('header', { class: 'site-head' }, typeIcon(flat, true),
      h('div', { class: 'site-head-main' }, h('h1', { text: flat.name || flat.slug }),
        siteLinks(flat))),
    h('nav', { class: 'site-tabs', 'aria-label': 'Site management' },
      // Documents and pages have no database, so no Database tab.
      TABS.filter(([tab]) => tab !== 'database' || hasServer(flat)).map(([tab, path]) => h('a', {
        href: base + path, 'data-nav': true, 'aria-current': active === tab ? 'page' : undefined,
        text: tabLabel(flat, tab),
      }))));
}

// siteLinks shows the flat's addresses, one line per kind: private (lock)
// first, then public (globe) while the flat is public.
function siteLinks(flat) {
  const targets = linkTargets(flat);
  const line = (ts, isPublic) => (ts.length ? h('div', { class: 'site-address muted' },
    icon(isPublic ? 'globe' : 'lock', isPublic ? 'Public' : 'Private'),
    ts.map((t, i) => [i ? h('span', { 'aria-hidden': 'true', text: '·' }) : null, extLink(t.url)])) : null);
  return h('div', { class: 'site-links' }, line(targets.private, false), line(targets.public, true));
}
