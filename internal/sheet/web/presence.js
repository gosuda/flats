// Presence and agent activity (rules 25 to 27, TECH 15). Presence is
// Private only: who else has the sheet open, on which tab, and each
// person's active cell or range, always with their name so color is never
// the only signal. A cell someone else is editing is marked; the mark is
// not a lock.
'use strict';

import { button, h, plural } from './dom.js';

const RANGE_RUNS_MAX = 64; // a selection in more id runs than this sends its active cell only

// work counts the steps taken to place other people's ranges, so a test can
// hold them to what is on screen.
export const work = { steps: 0 };

// cleanRuns reads the rows or the columns of a range that another client
// sent, "all" or id runs [[first, count], ...], into sorted spans
// [first, end) that do not overlap, or returns null when they are neither.
// "all" is one span that holds every id, so a whole row or column is
// drawn wherever the grid draws one of its cells. The runs are never
// expanded: a run may name a billion ids, and what a range costs must not
// depend on that.
export function cleanRuns(runs) {
  if (runs === 'all') return [[0, Infinity]];
  if (!Array.isArray(runs) || runs.length > RANGE_RUNS_MAX) return null;
  const spans = [];
  for (const run of runs) {
    if (!Array.isArray(run) || run.length !== 2) return null;
    const [first, count] = run;
    if (!Number.isSafeInteger(first) || !Number.isSafeInteger(count) || first < 0 || count < 1) return null;
    spans.push([first, first + count]);
  }
  spans.sort((a, b) => a[0] - b[0]);
  const out = [];
  for (const span of spans) {
    const last = out[out.length - 1];
    if (last && span[0] <= last[1]) last[1] = Math.max(last[1], span[1]);
    else out.push(span);
  }
  return out;
}

// inRuns says whether id lies in the spans cleanRuns made, by halving.
export function inRuns(spans, id) {
  let lo = 0;
  let hi = spans.length - 1;
  while (lo <= hi) {
    work.steps++;
    const mid = (lo + hi) >> 1;
    if (id < spans[mid][0]) hi = mid - 1;
    else if (id >= spans[mid][1]) lo = mid + 1;
    else return true;
  }
  return false;
}

// presenceMessage is what this tab tells the others about its selection,
// or null when there is nothing it can say. The host ends the connection
// on a presence frame it cannot read, so the message holds only what the
// host takes: ids of the open tab's own axes, each once, as runs that do
// not overlap, at most RANGE_RUNS_MAX of them. A selection that needs more
// runs, as one over rows a sort has shuffled, tells the active cell only.
export function presenceMessage(model, selection, editing) {
  const t = model.tab;
  const a = selection.active();
  const active = [model.rowAt(a.r), model.colAt(a.c)];
  if (!active.every((id) => Number.isSafeInteger(id) && id > 0)) return null;
  const m = { tab: t.id, active, editing: !!editing };
  if (!selection.single) {
    const range = selection.ids();
    if (range.rows.length + range.cols.length <= RANGE_RUNS_MAX) m.range = range;
  }
  return m;
}

export class Presence {
  constructor() { this.people = []; }

  update(msg, selfId) { this.people = (msg.people || []).filter((p) => p.session_id !== selfId); }
  clear() { this.people = []; }

  // tabs lists the tab ids other people have open.
  tabs() { return new Set(this.people.map((p) => p.tab)); }

  // outlines lists the people on the open tab with what the grid needs to
  // draw them: the place of the active cell and the rows and columns of the
  // range as spans of ids. Which drawn cells lie in a range is worked out
  // by the grid for the cells it draws, so a range costs what is on screen
  // whatever it names: ids that are not in the tab, or no longer, match no
  // cell. A person whose active cell is not on this tab's axes is left out.
  outlines(model) {
    const out = [];
    const t = model.tab;
    if (!t) return out;
    for (const p of this.people) {
      if (p.tab !== t.id || !Array.isArray(p.active)) continue;
      const ar = model.rowIndex(p.active[0]);
      const ac = model.colIndex(p.active[1]);
      if (ar < 0 || ac < 0) continue;
      let rows = p.range ? cleanRuns(p.range.rows) : null;
      let cols = p.range ? cleanRuns(p.range.cols) : null;
      // Without a usable range the person's active cell is what shows.
      if (!rows || !cols) { rows = [[p.active[0], p.active[0] + 1]]; cols = [[p.active[1], p.active[1] + 1]]; }
      out.push({ rows, cols, ar, ac, label: p.label, color: p.color, editing: !!p.editing });
    }
    return out;
  }

  // faces renders the top bar's people button.
  faces(me, onOpen) {
    const names = [`${me?.label || 'You'} (you)`, ...this.people.map((p) => p.label)];
    return button('', onOpen, { class: 'faces', 'aria-haspopup': 'dialog', 'aria-label': `People here: ${names.join(', ')}`, title: 'People here' },
      h('span', { class: `face me${me?.verified ? '' : ' unv'}`, 'aria-hidden': 'true', text: initial(me?.label) }),
      this.people.slice(0, 4).map((p) => h('span', { class: `face pcb${(p.color % 6) + 1}${p.verified ? '' : ' unv'}`, 'aria-hidden': 'true', text: initial(p.label) })));
  }

  // list renders the people dialog's content.
  list(me, tabName) {
    const row = (p, self) => h('div', { class: 'p' },
      h('span', { class: `face ${self ? 'me' : `pcb${(p.color % 6) + 1}`}${p.verified ? '' : ' unv'}`, 'aria-hidden': 'true', text: initial(p.label) }),
      h('span', {}, self ? `${p.label || 'You'} (you)` : p.label, ' · ',
        h('span', { class: p.verified ? 'verified' : 'unverified', text: p.verified ? 'Verified' : 'Unverified name' }),
        self ? null : h('small', { text: `On ${tabName(p.tab)}${p.editing ? ', editing a cell' : ''}` })));
    return [h('h2', { text: `${plural(this.people.length + 1, 'person', 'people')} here` }), row(me || {}, true), this.people.map((p) => row(p, false))];
  }
}

const initial = (label) => ([...(label || '?')][0] || '?').toUpperCase();

// agentNotice words the notice of rule 27 from what the delta of an
// agent's call tells: the cells it wrote (note is {tab, cells}) and the
// structural operations it made (edits, as structEdits reads them). before
// and after are the tab lists around the delta, for the names. The notice
// is one line: at most three things are named, and it never says that
// zero cells were written.
export function agentNotice(note, edits, before, after) {
  const name = (id) => (after.find((t) => t.id === id) || before.find((t) => t.id === id))?.name ?? null;
  const within = (tabs) => (tabs.size === 1 && name([...tabs][0]) != null ? ` in ${name([...tabs][0])}` : '');
  const parts = [];
  if (note.cells > 0) parts.push(`wrote ${plural(note.cells, 'cell')}${within(new Set([note.tab]))}`);
  // Rows and columns: one count for each kind of change, over all tabs.
  const axis = new Map(); // op -> {count, tabs}
  const tabs = { add_tab: [], delete_tab: [], rename_tab: [], move_tab: [] };
  for (const e of edits) {
    if (e.op in tabs) { tabs[e.op].push(e); continue; }
    let g = axis.get(e.op);
    if (!g) { g = { count: 0, tabs: new Set() }; axis.set(e.op, g); }
    g.tabs.add(e.tab);
    if (e.op !== 'sort') for (const [, count] of e.ids) g.count += count;
  }
  const one = (list, verb) => {
    if (!list.length) return;
    const known = list.length === 1 ? name(list[0].tab) : null;
    parts.push(known != null ? `${verb} the tab “${known}”` : `${verb} ${plural(list.length, 'tab')}`);
  };
  one(tabs.add_tab, 'added');
  if (tabs.rename_tab.length === 1) {
    const e = tabs.rename_tab[0];
    const old = before.find((t) => t.id === e.tab)?.name;
    parts.push(old != null ? `renamed the tab “${old}” to “${e.name}”` : `renamed a tab to “${e.name}”`);
  } else if (tabs.rename_tab.length) parts.push(`renamed ${plural(tabs.rename_tab.length, 'tab')}`);
  one(tabs.move_tab, 'moved');
  one(tabs.delete_tab, 'deleted');
  for (const [op, verb, unit] of [['insert_rows', 'inserted', 'row'], ['delete_rows', 'deleted', 'row'], ['insert_cols', 'inserted', 'column'], ['delete_cols', 'deleted', 'column']]) {
    const g = axis.get(op);
    if (g) parts.push(`${verb} ${plural(g.count, unit)}${within(g.tabs)}`);
  }
  if (axis.has('sort')) parts.push(`sorted rows${within(axis.get('sort').tabs)}`);
  // A call that wrote no cell and changed no structure changed formats,
  // widths or settings, which the delta does not count.
  if (!parts.length) parts.push(name(note.tab) != null ? `changed ${name(note.tab)}` : 'changed the sheet');
  const shown = parts.slice(0, 3);
  if (parts.length > 3) shown.push('made other changes');
  const text = shown.length === 1 ? shown[0] : `${shown.slice(0, -1).join(', ')} and ${shown.at(-1)}`;
  return `The agent ${text}.`;
}
