// Personal undo and redo (TECH 10, rule 25). The server owns the inverse
// stacks of this tab's session; the client sends undo and redo as their
// own actions and explains the result from the ack's undo summary.
'use strict';


export const undoOp = (redo) => ({ op: redo ? 'redo' : 'undo' });

// undoMessage turns an UndoSummary into the notice the person sees, or
// null when everything was undone.
export function undoMessage(summary, redo) {
  if (!summary) return null;
  const word = redo ? 'redo' : 'undo';
  if (!summary.applied && !summary.skipped_later && !summary.skipped_gone && !summary.skipped_structure) return `Nothing to ${word}.`;
  const parts = [];
  if (summary.skipped_later) parts.push('Changes other people made later were not undone.');
  if (summary.skipped_gone) parts.push(`Some items no longer exist, so that part was not ${redo ? 'redone' : 'undone'}.`);
  if (summary.skipped_structure) parts.push(`Part of it would break the page structure, so it was not ${redo ? 'redone' : 'undone'}.`);
  return parts.length ? parts.join(' ') : null;
}
