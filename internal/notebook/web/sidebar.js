// Sidebar page tree and routing (rules 12, 13, 18, 23). The tree is a
// role=tree with one roving tab stop: arrows move, Right and Left expand
// and collapse or step into and out of children, Home and End jump, Enter
// opens. Expanded state is personal and not saved.
'use strict';


import { h, icon } from './dom.js';

// parseRoute reads a location hash: '#/<page id>', optionally followed by
// '/<block id>' for a link to one block.
export function parseRoute(hash) {
  const m = /^#\/(p_[a-z2-7]{26})(?:\/(b_[a-z2-7]{26}))?$/.exec(hash || '');
  return m ? { page: m[1], block: m[2] || null } : null;
}

export const routeOf = (pageId) => '#/' + pageId;

// resolveRoute decides what the main area shows for a hash: a page, the
// first page when there is no id, or missing for an unknown id.
export function resolveRoute(view, hash) {
  const r = parseRoute(hash);
  if (!r) {
    const first = view.firstPage();
    return first ? { page: first.ref.id, redirect: true } : { none: true };
  }
  return view.get(r.page) ? { page: r.page, block: r.block } : { missing: r.page };
}

export class Sidebar {
  // app: {view(), current(), go(id), writable(), newPage(parent), presenceOn(page)}
  constructor(app, host) {
    this.app = app;
    this.host = host;
    this.expanded = new Set();
    this.focusId = null;
    this.tree = h('ul', { class: 'tree', role: 'tree', 'aria-label': 'Pages' });
    this.tree.addEventListener('keydown', (e) => this.key(e));
    this.tree.addEventListener('click', (e) => this.click(e));
    host.appendChild(this.tree);
  }

  // reveal expands the ancestors of page so it is visible in the tree.
  reveal(view, pageId) {
    for (const a of view.ancestors(pageId)) this.expanded.add(a.ref.id);
  }

  render(view) {
    const current = this.app.current();
    const sig = JSON.stringify([current, [...this.expanded], [...view.map.values()].filter((p) => p.kind === 'page').map((p) => [p.ref.id, p.title, p.parent, p.position, this.app.presenceOn(p.ref.id)])]);
    if (sig === this.sig && this.tree.firstChild) return;
    this.sig = sig;
    if (!this.focusId || !view.get(this.focusId)) this.focusId = current && view.get(current) ? current : (view.firstPage() || { ref: {} }).ref.id;
    const build = (parent, level) => view.pages(parent).map((p, i, all) => {
      const id = p.ref.id;
      const kids = view.pages(id);
      const open = kids.length > 0 && this.expanded.has(id);
      const label = p.title || 'Untitled';
      const here = this.app.presenceOn(id);
      const li = h('li', {
        role: 'treeitem', 'data-id': id, 'aria-level': String(level), 'aria-setsize': String(all.length), 'aria-posinset': String(i + 1),
        'aria-expanded': kids.length ? String(open) : null, 'aria-selected': String(id === current),
        'aria-label': here.length ? `${label}, ${here.join(', ')} here` : label,
        tabindex: id === this.focusId ? '0' : '-1',
      },
      h('div', { class: 'row', 'aria-current': id === current ? 'page' : null },
        h('span', { class: 'twist' + (kids.length ? '' : ' leaf'), 'aria-hidden': 'true', 'data-twist': '' }, icon('chev')),
        h('a', { href: '#/' + id, tabindex: '-1' }, label),
        here.length ? h('span', { class: 'who', title: here.join(', ') + ' here' }) : null));
      if (open) li.appendChild(h('ul', { role: 'group' }, build(id, level + 1)));
      return li;
    });
    const hadFocus = this.tree.contains(document.activeElement);
    this.tree.replaceChildren(...build(null, 1));
    if (hadFocus) this.#item(this.focusId)?.focus();
  }

  #item(id) { return id ? this.tree.querySelector(`[data-id="${id}"]`) : null; }

  #visible() { return Array.from(this.tree.querySelectorAll('[role="treeitem"]')); }

  #focus(id) {
    this.focusId = id;
    for (const li of this.#visible()) li.setAttribute('tabindex', li.dataset.id === id ? '0' : '-1');
    this.#item(id)?.focus();
  }

  toggle(id, open = !this.expanded.has(id)) {
    this.sig = null;
    if (open) this.expanded.add(id); else this.expanded.delete(id);
    this.render(this.app.view());
    this.#focus(id);
  }

  click(e) {
    const li = e.target.closest && e.target.closest('[role="treeitem"]');
    if (!li) return;
    if (e.target.closest('[data-twist]')) { e.preventDefault(); this.toggle(li.dataset.id); return; }
    this.focusId = li.dataset.id;
  }

  key(e) {
    const items = this.#visible();
    const li = e.target.closest && e.target.closest('[role="treeitem"]');
    if (!li) return;
    const id = li.dataset.id, i = items.indexOf(li);
    const view = this.app.view();
    const hasKids = view.pages(id).length > 0;
    switch (e.key) {
      case 'ArrowDown': e.preventDefault(); if (items[i + 1]) this.#focus(items[i + 1].dataset.id); break;
      case 'ArrowUp': e.preventDefault(); if (items[i - 1]) this.#focus(items[i - 1].dataset.id); break;
      case 'Home': e.preventDefault(); this.#focus(items[0].dataset.id); break;
      case 'End': e.preventDefault(); this.#focus(items.at(-1).dataset.id); break;
      case 'ArrowRight':
        e.preventDefault();
        if (hasKids && !this.expanded.has(id)) this.toggle(id, true);
        else if (hasKids) this.#focus(view.pages(id)[0].ref.id);
        break;
      case 'ArrowLeft': {
        e.preventDefault();
        const p = view.get(id);
        if (hasKids && this.expanded.has(id)) this.toggle(id, false);
        else if (p && p.parent) this.#focus(p.parent);
        break;
      }
      case 'Enter':
      case ' ':
        e.preventDefault();
        this.app.go(id);
        break;
    }
  }
}

// breadcrumb builds the ancestors of a page as links, the page itself last.
export function breadcrumb(view, pageId) {
  const page = view.get(pageId);
  if (!page) return [];
  const out = [];
  for (const a of view.ancestors(pageId)) out.push(h('a', { href: '#/' + a.ref.id }, a.title || 'Untitled'), h('span', { class: 'sep', 'aria-hidden': 'true' }, '/'));
  out.push(h('span', { 'aria-current': 'page' }, page.title || 'Untitled'));
  return out;
}
