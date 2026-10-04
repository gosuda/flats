import { h, icon } from './dom.js';
import { extLink } from './ui.js';
import { thumb, typeBadge } from './list.js';

export function siteHeader(flat, active) {
  const base = `/flats/${encodeURIComponent(flat.slug)}`;
  return h('div', { class: 'site-heading' },
    h('header', { class: 'site-head' }, thumb(flat, true),
      h('div', { class: 'site-head-main' }, h('h1', { text: flat.name || flat.slug }), typeBadge(flat),
        h('div', { class: 'site-address muted' }, icon(flat.visibility === 'private' ? 'lock' : 'globe'),
          extLink(flat.public_url || flat.private_url)))),
    h('nav', { class: 'site-tabs', 'aria-label': 'Site management' },
      ['settings', 'analytics', 'database'].map((tab) => h('a', {
        href: `${base}/${tab}`, 'data-nav': true, 'aria-current': active === tab ? 'page' : undefined,
        text: tab[0].toUpperCase() + tab.slice(1),
      }))));
}
