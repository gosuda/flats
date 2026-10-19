// Find (rule 9, TECH 11.2): case-insensitive substring search over element
// text, connector labels, frame names and image alternative text.

export function searchable(el) {
  if (el.type === 'image') return el.alt || '';
  if (el.type === 'pen') return '';
  return el.text || '';
}

// findMatches returns the ids of matching elements in reading order: top
// to bottom in 16px rows, then left to right.
export function findMatches(elements, query, centerOf) {
  const q = query.trim().toLocaleLowerCase();
  if (!q) return [];
  const hits = [];
  for (const el of elements) {
    if (!searchable(el).toLocaleLowerCase().includes(q)) continue;
    const [x, y] = centerOf(el);
    hits.push({ id: el.id, x, row: Math.round(y / 16) });
  }
  hits.sort((a, b) => a.row - b.row || a.x - b.x);
  return hits.map((h) => h.id);
}
