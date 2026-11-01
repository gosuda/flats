// Kanban layout: one column per option of the grouping select property in
// option order, plus "No value" (rule 10). Columns render a bounded number
// of cards and always include the focused one.
'use strict';


import { h, ops, newUnit, propertyOf, optionsOf, cellDisplay, titleProperty } from './dbschema.js';
import { matchesView } from './query-worker.js';

export const COLUMN_PAGE = 100;

// cardMoveOps is a card drop or keyboard move. Without a sort it moves the
// row before `before` (null is the end) and, when the column changes, sets
// the grouping cell in the same unit. With a sort only the cell changes.
export function cardMoveOps(schema, view, row, column, before, sorted) {
  const prop = propertyOf(schema, view.groupBy);
  const changes = (row.cells[prop.id] ?? null) !== column;
  const out = [];
  if (!sorted && before?.id !== row.id) out.push(ops.moveRows([row], before));
  if (changes) out.push(ops.setCell(schema, row, prop, column));
  if (out.length > 1) {
    const unit = newUnit();
    for (const op of out) op.unit_id = unit;
  }
  return out;
}

export function columnsOf(schema, view) {
  const prop = propertyOf(schema, view.groupBy);
  return [...optionsOf(schema, prop).map((o) => ({ id: o.id, name: o.name, color: o.color })), { id: null, name: 'No value', color: 'gray' }];
}

// renderKanban fills host. m: {schema, view, rows (Map), order, kept,
// readOnly, onOps, announce, touched(rowId), limits (Map), focus, setFocus}.
export function renderKanban(host, m) {
  const { schema, view } = m;
  const def = view.def;
  const prop = propertyOf(schema, def.groupBy);
  const sorted = def.sort.length > 0;
  const cols = columnsOf(schema, def);
  const byCol = new Map(cols.map((c) => [c.id, []]));
  for (const id of m.order) {
    const r = m.rows.get(id);
    if (!r) continue;
    const v = r.cells[prop.id] ?? null;
    (byCol.get(v) ?? byCol.get(null)).push(r);
  }
  const title = titleProperty(schema);
  const shown = def.visible.map((id) => propertyOf(schema, id)).filter((p) => p && p.id !== prop.id && p.id !== title.id);
  const emit = (list, row) => {
    if (!list.length) return;
    m.touched(row.id);
    m.onOps(list);
  };
  const move = (row, column, before) => {
    const list = cardMoveOps(schema, def, row, column, before, sorted);
    emit(list, row);
    if ((row.cells[prop.id] ?? null) !== column) {
      m.announce(`Moved “${row.cells[title.id] || 'Untitled'}” to ${cols.find((c) => c.id === column).name}. ${prop.name} changed for everyone.`);
    }
  };
  const dragged = (e) => m.rows.get(e.dataTransfer?.getData('text/x-notebook-row'));

  // One card is the board's tab stop: the focused one, else the first.
  const first = cols.map((c) => byCol.get(c.id)).find((list) => list.length)?.[0];
  const stop = m.focus && m.rows.has(m.focus) && m.order.includes(m.focus) ? m.focus : first?.id;
  const board = h('div', { class: 'kanban', role: 'list', 'aria-label': `Cards grouped by ${prop.name}` });
  cols.forEach((col, ci) => {
    const cards = byCol.get(col.id);
    if (!def.showEmpty && !cards.length) return;
    const limit = m.limits.get(col.id) ?? COLUMN_PAGE;
    const visible = cards.slice(0, limit);
    const pinned = cards.findIndex((r) => r.id === m.focus);
    if (pinned >= limit) visible.push(cards[pinned]);
    const column = h('div', { class: 'col', role: 'listitem', 'data-col': col.id ?? '', 'aria-label': `${col.name}, ${cards.length} ${cards.length === 1 ? 'card' : 'cards'}` },
      h('div', { class: 'col-h' }, h('span', { class: 'opt ' + col.color }, col.name), h('span', { class: 'count' }, String(cards.length))));
    const list = h('div', { class: 'cards', role: 'list', 'aria-label': col.name });
    visible.forEach((r) => {
      const at = cards.indexOf(r);
      const hidden = m.kept.has(r.id) && !matchesView(schema, def, r);
      const card = h('div', {
        class: 'card' + (hidden ? ' hidden-by-filter' : ''), role: 'listitem', tabindex: r.id === stop ? '0' : '-1', 'data-row': r.id,
        draggable: m.readOnly ? null : 'true',
        'aria-label': `${r.cells[title.id] || 'Untitled'}. ${m.readOnly ? '' : `Left and right arrows change ${prop.name}; ${sorted ? 'the sort sets the order' : 'Alt and up or down arrows reorder'}.`}`,
      },
      h('span', { class: 'card-title' }, r.cells[title.id] || 'Untitled'),
      hidden ? h('span', { class: 'note' }, 'Hidden by this view’s filter. It stays until you leave the view.') : null,
      h('div', { class: 'meta' }, shown.map((p) => {
        const d = cellDisplay(schema, p, r.cells[p.id]);
        return d == null ? null : h('span', { 'aria-label': p.name }, d);
      })));
      card.addEventListener('focus', () => m.setFocus(r.id));
      if (!m.readOnly) {
        card.addEventListener('dragstart', (e) => { e.stopPropagation(); e.dataTransfer?.setData('text/x-notebook-row', r.id); });
        card.addEventListener('dragover', (e) => { e.preventDefault(); e.stopPropagation(); });
        card.addEventListener('drop', (e) => {
          e.preventDefault();
          e.stopPropagation();
          const d = dragged(e);
          if (d && d.id !== r.id) move(d, col.id, r);
        });
        card.addEventListener('keydown', (e) => {
          if (e.key === 'ArrowLeft' || e.key === 'ArrowRight') {
            e.preventDefault();
            const next = cols[ci + (e.key === 'ArrowRight' ? 1 : -1)];
            if (next) {
              m.setFocus(r.id);
              emit(cardMoveOps(schema, def, r, next.id, null, true), r);
              m.announce(`Moved to ${next.name}.`);
            }
          } else if ((e.key === 'ArrowUp' || e.key === 'ArrowDown') && e.altKey) {
            e.preventDefault();
            if (sorted) { m.announce('Cards follow this view’s sort, so reordering is off here.'); return; }
            if (e.key === 'ArrowUp' && at === 0) return;
            if (e.key === 'ArrowDown' && at === cards.length - 1) return;
            m.setFocus(r.id);
            emit([ops.moveRows([r], e.key === 'ArrowUp' ? cards[at - 1] : cards[at + 2] ?? null)], r);
          } else if (e.key === 'ArrowUp' || e.key === 'ArrowDown') {
            e.preventDefault();
            const n = cards[at + (e.key === 'ArrowUp' ? -1 : 1)];
            if (n) m.focusCard(n.id);
          }
        });
      }
      list.append(card);
    });
    column.append(list);
    if (cards.length > visible.length) {
      column.append(h('button', {
        type: 'button', class: 'btn more',
        onclick: () => { m.limits.set(col.id, limit + COLUMN_PAGE); m.repaint(); },
      }, `Show ${Math.min(COLUMN_PAGE, cards.length - limit)} more`));
    }
    if (!m.readOnly) {
      column.addEventListener('dragover', (e) => e.preventDefault());
      column.addEventListener('drop', (e) => {
        e.preventDefault();
        const d = dragged(e);
        if (d) move(d, col.id, null);
      });
      column.append(h('button', {
        type: 'button', class: 'addrow', 'aria-label': `New card in ${col.name}`,
        onclick: () => m.createRow(col.id == null ? {} : { [prop.id]: col.id }),
      }, 'New'));
    }
    board.append(column);
  });
  host.replaceChildren(board);
}
