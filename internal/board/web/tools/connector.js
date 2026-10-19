// Connector tool (L): drag from one point to another. An end dropped on an
// element attaches to it (rule 4); Alt keeps it free.

import { attachAt } from '../geom.js';
import { round1 } from '../schema.js';
import * as ops from '../ops.js';

export class ConnectorTool {
  constructor(app) { this.app = app; this.drag = null; }

  end(e, p) {
    const t = e.altKey ? null : this.app.hitAt(p[0], p[1], { filter: (el) => el.type !== 'connector' && el.type !== 'pen' });
    return t ? { el: t.id, at: attachAt(t, p[0], p[1]) } : { pt: [round1(p[0]), round1(p[1])] };
  }

  down(e, p) { this.drag = { a: this.end(e, p), p0: p, p1: p }; }

  move(e, p) {
    if (!this.drag) return;
    this.drag.p1 = p;
    this.drag.b = this.end(e, p);
    this.app.renderer.dynamic();
  }

  up(e, p) {
    const d = this.drag;
    this.drag = null;
    const app = this.app;
    app.renderer.dynamic();
    if (!d || Math.hypot(p[0] - d.p0[0], p[1] - d.p0[1]) * app.cam.z < 6) return;
    const b = this.end(e, p);
    if (d.a.el && d.a.el === b.el) return;
    const el = ops.newElement(app.state, 'connector', null, { ends: [d.a, b], text: '' });
    if (app.commit([ops.addOp(app.state, el)])) {
      app.setTool('select');
      app.select([el.id]);
    }
  }

  cancel() { this.drag = null; this.app.renderer.dynamic(); }

  draw(ctx, r) {
    const d = this.drag;
    if (!d) return;
    const a = r.toScreen(...d.p0), b = r.toScreen(...d.p1);
    ctx.strokeStyle = r.theme.ink;
    ctx.lineWidth = 2;
    ctx.beginPath(); ctx.moveTo(...a); ctx.lineTo(...b); ctx.stroke();
    if (d.b && d.b.el) {
      const t = this.app.doc.get(d.b.el);
      if (t) { ctx.strokeStyle = r.theme.sel; r.outline(ctx, t, 4); ctx.stroke(); }
    }
  }
}
