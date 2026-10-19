// Connector tool (L): drag from one point to another. An end dropped on an
// element attaches to it (rule 4), on its outline (TECH 4.4); Alt keeps it
// free.

import { outlineAt, defaultAt, endpointWorld } from '../geom.js';
import { clampPoint } from '../schema.js';
import * as ops from '../ops.js';

export class ConnectorTool {
  constructor(app) { this.app = app; this.drag = null; }

  target(e, p) {
    return e.altKey ? null : this.app.hitAt(p[0], p[1], { filter: (el) => el.type !== 'connector' && el.type !== 'pen' });
  }

  // ends turns the two drop points into connector ends. An end not dropped
  // on its target's outline takes the side facing the other end, which
  // counts at its target's center when it has no point on the outline
  // either, as the server does for an agent's end without at (TECH 4.4).
  ends(a, b) {
    const tol = 8 / this.app.cam.z;
    const drops = [a, b].map(({ t, p }) => {
      if (!t) return { end: { pt: clampPoint(p) }, toward: p };
      const at = outlineAt(t, p[0], p[1], tol);
      return { t, at, toward: at ? endpointWorld(t, at) : [t.x + t.w / 2, t.y + t.h / 2] };
    });
    return drops.map((d, k) => d.end || { el: d.t.id, at: d.at || defaultAt(d.t, drops[1 - k].toward) });
  }

  down(e, p) { this.drag = { a: { t: this.target(e, p), p }, p0: p, p1: p }; }

  move(e, p) {
    if (!this.drag) return;
    this.drag.p1 = p;
    this.drag.over = this.target(e, p);
    this.app.renderer.dynamic();
  }

  up(e, p) {
    const d = this.drag;
    this.drag = null;
    const app = this.app;
    app.renderer.dynamic();
    if (!d || Math.hypot(p[0] - d.p0[0], p[1] - d.p0[1]) * app.cam.z < 6) return;
    const b = { t: this.target(e, p), p };
    if (d.a.t && b.t && d.a.t.id === b.t.id) return;
    const a = { t: d.a.t && app.doc.get(d.a.t.id), p: d.a.p };
    const el = ops.newElement(app.state, 'connector', null, { ends: this.ends(a, b), text: '' });
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
    if (d.over) { ctx.strokeStyle = r.theme.sel; r.outline(ctx, d.over, 4); ctx.stroke(); }
  }
}
