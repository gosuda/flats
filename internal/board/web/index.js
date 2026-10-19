// Spatial index (TECH 11.1): a uniform 512px grid, a separate list for
// elements that span more than 4 cells, and a reverse index from a target
// element to the connectors attached to it.

const CELL = 512;
const LARGE_CELLS = 4;

export class SpatialIndex {
  constructor() {
    this.cells = new Map();   // "cx,cy" -> Set<id>
    this.large = new Map();   // id -> rect
    this.boxes = new Map();   // id -> rect
    this.attached = new Map(); // target id -> Set<connector id>
    this.targets = new Map();  // connector id -> [target ids]
  }

  clear() {
    this.cells.clear(); this.large.clear(); this.boxes.clear();
    this.attached.clear(); this.targets.clear();
  }

  bounds(id) { return this.boxes.get(id) || null; }

  *#cellKeys(r) {
    const x0 = Math.floor(r.x / CELL), x1 = Math.floor((r.x + r.w) / CELL);
    const y0 = Math.floor(r.y / CELL), y1 = Math.floor((r.y + r.h) / CELL);
    for (let cx = x0; cx <= x1; cx++) for (let cy = y0; cy <= y1; cy++) yield cx + ',' + cy;
  }

  #isLarge(r) {
    const nx = Math.floor((r.x + r.w) / CELL) - Math.floor(r.x / CELL) + 1;
    const ny = Math.floor((r.y + r.h) / CELL) - Math.floor(r.y / CELL) + 1;
    return nx * ny > LARGE_CELLS;
  }

  // set places id at rect r, or removes it when r is null. It returns the
  // previous rect.
  set(id, r) {
    const old = this.boxes.get(id) || null;
    if (old) {
      if (this.large.has(id)) this.large.delete(id);
      else for (const k of this.#cellKeys(old)) {
        const s = this.cells.get(k);
        if (s) { s.delete(id); if (!s.size) this.cells.delete(k); }
      }
      this.boxes.delete(id);
    }
    if (r) {
      this.boxes.set(id, r);
      if (this.#isLarge(r)) this.large.set(id, r);
      else for (const k of this.#cellKeys(r)) {
        let s = this.cells.get(k);
        if (!s) this.cells.set(k, (s = new Set()));
        s.add(id);
      }
    }
    return old;
  }

  // query returns the ids whose rect intersects r.
  query(r) {
    const out = new Set();
    const hitRect = (b) => b.x <= r.x + r.w && r.x <= b.x + b.w && b.y <= r.y + r.h && r.y <= b.y + b.h;
    const nx = Math.floor((r.x + r.w) / CELL) - Math.floor(r.x / CELL) + 1;
    const ny = Math.floor((r.y + r.h) / CELL) - Math.floor(r.y / CELL) + 1;
    if (nx * ny > this.cells.size) {
      // A query wider than the occupied cells is cheaper as a scan.
      for (const s of this.cells.values()) for (const id of s) if (!out.has(id) && hitRect(this.boxes.get(id))) out.add(id);
    } else {
      for (const k of this.#cellKeys(r)) {
        const s = this.cells.get(k);
        if (s) for (const id of s) if (!out.has(id) && hitRect(this.boxes.get(id))) out.add(id);
      }
    }
    for (const [id, b] of this.large) if (hitRect(b)) out.add(id);
    return out;
  }

  // setConnector records which targets a connector's ends are attached to.
  setConnector(id, targetIds) {
    for (const t of this.targets.get(id) || []) {
      const s = this.attached.get(t);
      if (s) { s.delete(id); if (!s.size) this.attached.delete(t); }
    }
    if (!targetIds || !targetIds.length) { this.targets.delete(id); return; }
    this.targets.set(id, targetIds);
    for (const t of targetIds) {
      let s = this.attached.get(t);
      if (!s) this.attached.set(t, (s = new Set()));
      s.add(id);
    }
  }

  connectorsOf(targetId) { return this.attached.get(targetId) || EMPTY; }
}

const EMPTY = new Set();
