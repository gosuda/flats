// Announcements and focus recovery (TECH 18, rule 18).

// announce speaks text through the polite live region.
'use strict';

export function announce(text) {
  const live = document.querySelector('#announce');
  if (!live) return;
  live.textContent = '';
  const set = () => { live.textContent = text; };
  if (globalThis.requestAnimationFrame) requestAnimationFrame(set); else set();
}

// nearestSurvivor picks where focus goes when the focused block disappears:
// the next block in the old document order that still exists, else the
// previous one, else null for the page title.
export function nearestSurvivor(oldOrder, id, exists) {
  const i = oldOrder.indexOf(id);
  if (i < 0) return null;
  for (let j = i + 1; j < oldOrder.length; j++) if (exists(oldOrder[j])) return oldOrder[j];
  for (let j = i - 1; j >= 0; j--) if (exists(oldOrder[j])) return oldOrder[j];
  return null;
}
