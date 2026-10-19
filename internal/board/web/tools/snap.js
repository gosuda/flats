// Snapping (TECH 11.2): to other elements' edges and center lines within 6
// screen pixels, otherwise to the 8px grid while the grid shows. Alt turns
// it off.

export function snapMove(app, box, dx, dy, exclude, off) {
  if (off || !box) return { dx, dy, guides: [] };
  const tol = 6 / app.cam.z;
  const m = { x: box.x + dx, y: box.y + dy, w: box.w, h: box.h };
  const mx = [m.x, m.x + m.w / 2, m.x + m.w];
  const my = [m.y, m.y + m.h / 2, m.y + m.h];
  let bx = null, by = null;
  const doc = app.doc;
  for (const id of doc.index.query(app.renderer.viewRect())) {
    if (exclude.has(id)) continue;
    const el = doc.get(id);
    if (!el || el.type === 'connector' || el.type === 'pen') continue;
    const b = doc.index.bounds(id);
    const cx = [b.x, b.x + b.w / 2, b.x + b.w], cy = [b.y, b.y + b.h / 2, b.y + b.h];
    for (const a of mx) for (const c of cx) {
      const d = c - a;
      if (Math.abs(d) <= tol && (!bx || Math.abs(d) < Math.abs(bx.d))) bx = { d, at: c, b };
    }
    for (const a of my) for (const c of cy) {
      const d = c - a;
      if (Math.abs(d) <= tol && (!by || Math.abs(d) < Math.abs(by.d))) by = { d, at: c, b };
    }
  }
  const guides = [];
  const grid = doc.meta().grid;
  if (bx) dx += bx.d;
  else if (grid) dx += Math.round(m.x / 8) * 8 - m.x;
  if (by) dy += by.d;
  else if (grid) dy += Math.round(m.y / 8) * 8 - m.y;
  if (bx) {
    const y0 = Math.min(bx.b.y, box.y + dy), y1 = Math.max(bx.b.y + bx.b.h, box.y + dy + box.h);
    guides.push([bx.at, y0, bx.at, y1]);
  }
  if (by) {
    const x0 = Math.min(by.b.x, box.x + dx), x1 = Math.max(by.b.x + by.b.w, box.x + dx + box.w);
    guides.push([x0, by.at, x1, by.at]);
  }
  return { dx, dy, guides };
}

export function drawGuides(ctx, r, guides) {
  if (!guides || !guides.length) return;
  ctx.strokeStyle = r.theme.guide;
  ctx.lineWidth = 1;
  ctx.setLineDash([4, 3]);
  for (const [x0, y0, x1, y1] of guides) {
    const a = r.toScreen(x0, y0), b = r.toScreen(x1, y1);
    ctx.beginPath();
    ctx.moveTo(a[0], a[1]);
    ctx.lineTo(b[0], b[1]);
    ctx.stroke();
  }
  ctx.setLineDash([]);
}
