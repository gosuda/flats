// Personal undo and redo (rule 21, TECH 4.10). The stack lives on the server
// per session; the client sends {op:"undo"} or {op:"redo"} and reports what
// the server skipped.

import { toast, plural } from './ui.js';
import { who } from './guest.js';

export class Undo {
  constructor(app) { this.app = app; }

  undo() { this.#send('undo'); }
  redo() { this.#send('redo'); }

  #send(op) {
    const app = this.app;
    if (!app.canEdit) return;
    if (app.editor) app.editor.commit();
    const batch = app.commit([{ op }]);
    if (batch) batch.undo = op;
  }

  ack(m, batch) {
    if (!batch || !batch.undo) return;
    const r = Array.isArray(m.results) ? m.results[0] : m.results;
    if (!r || typeof r !== 'object') return;
    const sk = r.skipped || {};
    const others = (sk.stale || 0) + (sk.gone || 0) + (sk.invalid_group || 0);
    const locked = sk.locked || 0;
    const verb = batch.undo === 'undo' ? 'Undone' : 'Redone';
    if (!r.undone && !others && !locked) {
      toast(batch.undo === 'undo' ? 'Nothing to undo.' : 'Nothing to redo.');
      return;
    }
    if (!others && !locked) return;
    const parts = [];
    if (locked) parts.push(plural(locked, 'locked element'));
    if (others) parts.push(`${plural(others, 'change')} other people made later`);
    toast(`${r.undone ? verb : 'Nothing changed'}. ${parts.join(' and ')} ${others + locked === 1 ? 'was' : 'were'} not ${batch.undo === 'undo' ? 'undone' : 'redone'}.`);
  }

  // cleared reports a whole-board rollback or replacement, which clears
  // every tab's undo history.
  cleared(reset, by) {
    const name = who(by).text;
    const what = reset.kind === 'rollback' ? `rolled the board back${reset.of ? ` to r${reset.of}` : ''}` : 'replaced the whole board';
    toast(`${name} ${what}. Everyone's undo history was cleared.`);
  }
}
