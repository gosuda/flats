// Pen tool (P): freehand strokes, simplified with RDP at 0.5 screen pixels
// and stored as normalized points (TECH 3.1, 11.1). A stylus draws with any
// tool (rule 20).

import { penFromPoints } from '../geom.js';
import { inkColor } from '../theme.js';
import { strokeSmooth } from '../render.js';
import * as ops from '../ops.js';

export class PenTool {
  constructor(app) {
    this.app = app;
    this.pts = null;
    this.mode = 'pen';
    this.color = 'ink';
  }

  activate() { this.app.styleBar.update(); }

  down(e, p) { this.pts = [p]; }

  move(e, p) {
    if (!this.pts) return;
    const evs = e.getCoalescedEvents ? e.getCoalescedEvents() : [];
    if (evs.length > 1) for (const ce of evs) this.pts.push(this.app.toWorld(...this.app.local(ce)));
    else this.pts.push(p);
    this.app.renderer.dynamic();
  }

  up() {
    const pts = this.pts;
    this.pts = null;
    const app = this.app;
    app.renderer.dynamic();
    if (!pts || !pts.length) return;
    const g = penFromPoints(pts, 0.5 / app.cam.z);
    const marker = this.mode === 'marker';
    const style = { width: marker ? 'marker' : 'pen', stroke: marker && this.color === 'ink' ? 'highlight' : this.color };
    const el = ops.newElement(app.state, 'pen', { x: g.x, y: g.y, w: g.w, h: g.h, rot: 0 }, { pts: g.pts, style });
    app.commit([ops.addOp(app.state, el)]);
  }

  cancel() { this.pts = null; this.app.renderer.dynamic(); }

  draw(ctx, r) {
    if (!this.pts || !this.pts.length) return;
    const marker = this.mode === 'marker';
    ctx.strokeStyle = inkColor(r.theme, marker && this.color === 'ink' ? 'highlight' : this.color);
    ctx.lineWidth = (marker ? 16 : 3) * this.app.cam.z;
    ctx.lineCap = 'round';
    ctx.lineJoin = 'round';
    strokeSmooth(ctx, this.pts.map(([x, y]) => r.toScreen(x, y)));
  }
}
