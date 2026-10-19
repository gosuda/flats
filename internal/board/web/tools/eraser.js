// Eraser (E): drag across pen strokes to delete them; the B1 eraser deletes
// whole strokes and never other elements (TECH 3.1).

import * as ops from '../ops.js';

export class EraserTool {
  constructor(app) { this.app = app; this.hits = null; }

  down(e, p) { this.hits = new Set(); this.collect(p); }

  move(e, p) { if (this.hits) this.collect(p); }

  collect(p) {
    const el = this.app.hitAt(p[0], p[1], { filter: (m) => m.type === 'pen' });
    if (el && !this.hits.has(el.id)) { this.hits.add(el.id); this.app.renderer.dynamic(); }
  }

  up() {
    const ids = this.hits ? [...this.hits] : [];
    this.hits = null;
    this.app.renderer.dynamic();
    if (ids.length) this.app.commit(ops.deleteOps(this.app.state, ids));
  }

  cancel() { this.hits = null; this.app.renderer.dynamic(); }

  draw(ctx, r) {
    if (!this.hits) return;
    ctx.strokeStyle = r.theme.guide;
    ctx.lineWidth = 2;
    ctx.setLineDash([3, 3]);
    for (const id of this.hits) {
      const el = this.app.doc.get(id);
      if (el) { r.outline(ctx, el, 3); ctx.stroke(); }
    }
    ctx.setLineDash([]);
  }
}
