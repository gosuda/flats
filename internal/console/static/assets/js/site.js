import { h } from './dom.js';
import { thumb } from './list.js';

export function siteHeader(flat, active) {
  const base = `/flats/${encodeURIComponent(flat.slug)}`;
  return h('div', { class: 'site-heading' },
    h('header', { class: 'site-head' }, thumb(flat, true),
      h('div', { class: 'site-head-main' }, h('h1', { text: flat.name || flat.slug }),
        h('div', { class: 'site-address muted' },
          h('a', { href: base + '#access', 'data-nav': true, text: 'Current version and draft addresses' })))),
    h('nav', { class: 'site-tabs', 'aria-label': 'Site management' },
      ['settings', 'analytics', 'database'].map((tab) => h('a', {
        href: `${base}/${tab}`, 'data-nav': true, 'aria-current': active === tab ? 'page' : undefined,
        text: tab[0].toUpperCase() + tab.slice(1),
      }))));
}
