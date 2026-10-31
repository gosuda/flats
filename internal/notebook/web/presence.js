// Presence and agent activity (TECH 14, rules 26-28). Presence is Private
// only: who else has the notebook open, on which page, editing which block
// or cell, always named so color is never the only signal. It is not a
// lock. Agent writes show a short notice and briefly highlight what changed.
'use strict';


import { h, icon, toast } from './dom.js';

export class Presence {
  constructor() { this.editors = []; }

  update(msg) { this.editors = msg.editors || []; }

  // onPage lists the labels of other people on page.
  onPage(page) { return this.editors.filter((e) => e.page === page).map((e) => e.label); }

  // editing returns the label of someone editing block id, or null.
  editing(id) {
    const e = this.editors.find((x) => x.edit && x.edit.block && x.edit.block.id === id);
    return e ? e.label : null;
  }

  // faces renders the header's people button and its list.
  faces(onToggle, open, pageTitle) {
    if (!this.editors.length) return null;
    const btn = h('button', { type: 'button', class: 'faces', 'aria-expanded': String(open), 'aria-label': `${this.editors.length} ${this.editors.length === 1 ? 'other person' : 'others'} here, show who`, onclick: onToggle },
      this.editors.slice(0, 3).map((e) => h('span', { class: 'face', 'aria-hidden': 'true' }, [...e.label][0] || '?')));
    const list = open ? h('div', { class: 'menu people', role: 'dialog', 'aria-label': 'People here' },
      this.editors.map((e) => h('div', { class: 'mi' },
        h('span', { class: 'face', 'aria-hidden': 'true' }, [...e.label][0] || '?'),
        h('span', { class: 'lbl' }, e.label, ' · ', h('span', { class: e.verified ? 'verified' : 'unverified' }, e.verified ? 'Verified' : 'Unverified name'),
          h('small', {}, `On ${pageTitle(e.page)}${e.edit ? ', editing' : ''}`))))) : null;
    return h('div', { class: 'people-wrap' }, btn, list);
  }
}

// activity shows an agent's write notice and flashes the changed items that
// are visible (1.5 s, TECH 14).
export function activity(msg, root) {
  toast([icon('bot'), ' ', msg.message]);
  for (const id of msg.changed || []) {
    const el = root.querySelector(`[data-id="${id}"]`);
    if (!el) continue;
    el.classList.remove('flash');
    el.classList.add('flash');
    setTimeout(() => el.classList.remove('flash'), 1500);
  }
}
