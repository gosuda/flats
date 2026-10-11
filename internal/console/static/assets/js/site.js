import { h, icon } from './dom.js';
import { extLink, openLinks } from './ui.js';
import { typeIcon } from './list.js';

// The management tabs of a flat; Deployments is the flat's own address. A
// document has no deployments, so its first tab is its Overview.
const TABS = [['deployments', ''], ['analytics', '/analytics'], ['database', '/database'], ['settings', '/settings']];
const tabLabel = (flat, tab) => (tab === 'deployments' && flat.type === 'docs' ? 'Overview' : tab[0].toUpperCase() + tab.slice(1));

export function siteHeader(flat, active) {
  const base = `/flats/${encodeURIComponent(flat.slug)}`;
  return h('div', { class: 'site-heading' },
    h('header', { class: 'site-head' }, typeIcon(flat, true),
      h('div', { class: 'site-head-main' }, h('h1', { text: flat.name || flat.slug }),
        h('div', { class: 'site-address muted' }, icon(flat.visibility === 'private' ? 'lock' : 'globe'),
          extLink(flat.public_url || flat.private_url),
          flat.live_version ? openLinks(flat) : null))),
    h('nav', { class: 'site-tabs', 'aria-label': 'Site management' },
      // A document has no database snapshots, so no Database tab.
      TABS.filter(([tab]) => !(tab === 'database' && flat.type === 'docs')).map(([tab, path]) => h('a', {
        href: base + path, 'data-nav': true, 'aria-current': active === tab ? 'page' : undefined,
        text: tabLabel(flat, tab),
      }))));
}
