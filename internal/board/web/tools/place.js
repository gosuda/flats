// Shared behavior of the tools that place one box element: click for the
// default size centered on the pointer, or drag to size it. The new element
// joins the topmost frame that contains its center (rule 3).

import * as ops from '../ops.js';

export class PlaceTool {
  constructor(app, type, size) {
    this.app = app;
    this.type = type;
    this.size = size;
    this.drag = null;
  }

  extra() { return {}; }

  down(e, p) { this.drag = { p0: p, p1: p }; }

  move(e, p) {
    if (!this.drag) return;
    this.drag.p1 = p;
    this.app.renderer.dynamic();
  }

  up(e, p) {
    const d = this.drag;
    this.drag = null;
    if (!d) return;
    const app = this.app;
    const [w0, h0] = this.size;
    const dragged = Math.abs(p[0] - d.p0[0]) * app.cam.z > 8 && Math.abs(p[1] - d.p0[1]) * app.cam.z > 8;
    const geom = dragged
      ? { x: Math.min(p[0], d.p0[0]), y: Math.min(p[1], d.p0[1]), w: Math.abs(p[0] - d.p0[0]), h: Math.abs(p[1] - d.p0[1]) }
      : { x: d.p0[0] - w0 / 2, y: d.p0[1] - h0 / 2, w: w0, h: h0 };
    this.create(geom);
    app.renderer.dynamic();
  }

  create(geom) {
    const app = this.app;
    const el = ops.newElement(app.state, this.type, geom, this.extra());
    if (!app.commit([ops.addOp(app.state, el)])) return null;
    app.setTool('select');
    app.select([el.id]);
    if (this.type !== 'frame') app.editText(el.id, { created: true });
    return el;
  }

  cancel() { this.drag = null; this.app.renderer.dynamic(); }

  draw(ctx, r) {
    const d = this.drag;
    if (!d) return;
    const a = r.toScreen(...d.p0), b = r.toScreen(...d.p1);
    ctx.strokeStyle = r.theme.sel;
    ctx.setLineDash([5, 4]);
    ctx.lineWidth = 1.5;
    ctx.strokeRect(Math.min(a[0], b[0]), Math.min(a[1], b[1]), Math.abs(a[0] - b[0]), Math.abs(a[1] - b[1]));
    ctx.setLineDash([]);
  }
}
