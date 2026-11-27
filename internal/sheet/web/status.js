// Save status and selection totals (rules 19 and 38). The sheet says Saved
// only when the server has confirmed every edit. Selecting two or more
// cells that hold numbers shows their sum, average and count, read from
// the values the server sent.
'use strict';

import { announce } from './a11y.js';
import { h, plural, replace } from './dom.js';

// statusText names the connection state for the top bar.
export function statusText(state, unsaved, calculating, connecting = false) {
  if (state === 'stopped') return `Editing stopped · ${unsaved} unsaved`;
  if (state === 'offline' && connecting) return 'Connecting…';
  if (state === 'offline') return unsaved ? `Offline · ${unsaved} unsaved` : 'Offline';
  if (calculating) return 'Calculating…';
  return state === 'saving' ? 'Saving…' : 'Saved';
}

// aggregate totals the numbers inside box, or returns null when fewer than
// two cells hold one.
export function aggregate(model, box) {
  let sum = 0;
  let count = 0;
  model.eachCell(box, (_r, _c, cell) => {
    if (cell.kind !== 'n' || typeof cell.value !== 'number') return;
    sum += cell.value;
    count++;
  });
  return count < 2 ? null : { sum, average: sum / count, count };
}

// plain prints a total without a number format: the status line states the
// numbers, it does not show a cell.
const plain = (v) => String(Math.round(v * 1e4) / 1e4);

export class Status {
  constructor() {
    this.el = h('span', { class: 'status' }, h('span', { class: 'dot' }), h('span', { class: 'status-text' }));
    this.agg = h('div', { class: 'agg' });
    this.last = null;
  }

  set(state, unsaved, calculating, connecting) {
    const text = statusText(state, unsaved, calculating, connecting);
    this.el.dataset.state = calculating && state !== 'offline' && state !== 'stopped' ? 'calculating' : state;
    this.el.lastChild.textContent = text;
    if (this.last !== text) { this.last = text; announce(text); }
  }

  totals(a) {
    if (!a) { replace(this.agg); return; }
    const part = (label, value) => h('span', {}, `${label} `, h('b', { text: value }));
    replace(this.agg, part('Sum', plain(a.sum)), part('Average', plain(a.average)), part('Count', String(a.count)));
    this.agg.setAttribute('aria-label', `Sum ${plain(a.sum)}, average ${plain(a.average)}, ${plural(a.count, 'number')}`);
  }
}
