// Client state machine (TECH 4.7). The server copy changes only through
// canonical deltas with consecutive seqs; acks only remove pending batches.
// The screen is the server copy with the pending batches applied again on top
// (the overlay), plus a transient preview while a drag is in progress.

import { clampPoint, withBundle } from './schema.js';
import { endpointWorld } from './geom.js';
import { keyBetween } from './ids.js';

// META_KEYS are the bundles of the _board pseudo-element (TECH 3.3). Other
// keys in a delta or op are ignored, so they can never reach object
// internals such as __proto__.
const META_KEYS = new Set(['title', 'grid']);

// activityCounts counts a delta's element changes for a notice. A revive
// that replaced an element the client already had at the same incarnation
// (r.replaced from applyDelta) is a change, not an addition.
export function activityCounts(d, r) {
  let added = 0, changed = 0, removed = 0;
  for (const c of d.changes || []) {
    if (c.id === '_board') continue;
    if (c.add || (c.revive && !(r && r.replaced && r.replaced.has(c.id)))) added++;
    else if (c.del !== undefined) removed++;
    else changed++;
  }
  return { added, changed, removed };
}

export class BoardState {
  constructor() {
    this.server = new Map();    // id -> PrivateElement (content + inc + v)
    this.meta = { title: '', grid: true };
    this.metaV = {};
    this.watermark = 0;
    this.session = null;
    this.public = false;
    this.pending = [];          // [{n, ops}] in send order
    this.nextN = 1;
    this.created = new Set();   // ids this session added (expected incarnation 0)
    this.overlay = new Map();   // id -> element, or null for deleted
    this.overlayMeta = {};
    this.preview = null;        // id -> element while dragging
    this.myText = new Map();    // id -> {v, text} of this tab's last committed text
    this.unacked = new Map();   // n -> batch whose delta arrived before its ack
  }

  // ---- reading the screen state ----

  get(id) {
    if (this.preview && this.preview.has(id)) return this.preview.get(id);
    return this.base(id);
  }

  base(id) {
    return this.overlay.has(id) ? this.overlay.get(id) : this.server.get(id) || null;
  }

  *all() {
    const pv = this.preview;
    for (const [id, el] of this.server) {
      if (this.overlay.has(id)) continue;
      yield pv && pv.has(id) ? pv.get(id) : el;
    }
    for (const [id, el] of this.overlay) {
      if (el) yield pv && pv.has(id) ? pv.get(id) : el;
    }
  }

  viewMeta() { return { ...this.meta, ...this.overlayMeta }; }

  // incOf is the expected incarnation to send for id: the server copy's, or 0
  // for an element this session created that no delta has shown yet (TECH 4.1).
  incOf(id) {
    const el = this.server.get(id);
    if (el) return el.inc;
    return this.created.has(id) ? 0 : null;
  }

  // ---- local edits ----

  // push queues a batch and returns it with the ids whose display changed.
  push(ops) {
    for (const o of ops) if (o.op === 'add') this.created.add(o.el.id);
    const batch = { n: this.nextN++, ops };
    this.pending.push(batch);
    return { batch, changed: this.#rebuild() };
  }

  setPreview(map) {
    const changed = new Set(this.preview ? this.preview.keys() : []);
    this.preview = map && map.size ? map : null;
    if (this.preview) for (const id of this.preview.keys()) changed.add(id);
    return changed;
  }

  // ---- server messages ----

  loadSnapshot(snap) {
    const old = new Set(this.server.keys());
    this.server = new Map();
    for (const el of snap.elements) this.server.set(el.id, el);
    if (snap.meta) this.#setMeta(snap.meta);
    this.watermark = snap.seq;
    if (snap.reset) this.myText.clear();
    for (const id of this.server.keys()) old.add(id);
    for (const id of this.#rebuild()) old.add(id);
    return old;
  }

  #setMeta(m) {
    for (const k of META_KEYS) {
      if (m[k] === undefined) continue;
      this.meta[k] = m[k];
    }
    if (m.v) this.metaV = { ...m.v };
  }

  // applyDelta applies one canonical delta (4.6) in seq order.
  applyDelta(d) {
    if (d.seq <= this.watermark) return { status: 'old' };
    if (d.seq > this.watermark + 1) return { status: 'gap' };
    const n = this.#ownBatch(d.from);
    const mine = n != null;
    const changed = new Set();
    const replaced = new Set();
    const overwritten = [];
    for (const c of d.changes || []) {
      changed.add(c.id);
      if (c.id === '_board') {
        for (const [g, e] of Object.entries(c.set || {})) {
          if (!META_KEYS.has(g)) continue;
          const [val, v] = this.public ? [e, null] : e;
          this.meta[g] = val;
          if (v != null) this.metaV[g] = v;
        }
        continue;
      }
      if (c.add) this.server.set(c.id, c.add);
      else if (c.revive) {
        // A reset that changes only author fields or immutable content of a
        // surviving element revives it at the same incarnation: the element
        // is replaced, not new. Public clients see no incarnations, so for
        // them any revive of an element they already have is a replacement.
        const prev = this.server.get(c.id);
        if (prev && prev.inc === c.revive.inc) replaced.add(c.id);
        this.server.set(c.id, c.revive);
      }
      else if (c.del !== undefined) this.server.delete(c.id);
      else if (c.set) {
        const prev = this.server.get(c.id);
        if (!prev) continue;
        let el = { ...prev, v: { ...prev.v } };
        if (c.inc != null) el.inc = c.inc;
        for (const [g, e] of Object.entries(c.set)) {
          const [val, v] = this.public ? [e, null] : e;
          el = withBundle(el, g, val);
          if (v != null) el.v[g] = v;
        }
        this.server.set(c.id, el);
        const t = c.set.text;
        if (t && !this.public) {
          if (mine) this.myText.set(c.id, { v: t[1], text: t[0] });
          else if (c.overwrote != null) {
            const m = this.myText.get(c.id);
            if (m && m.v === c.overwrote) overwritten.push({ id: c.id, text: m.text });
          }
        }
      }
    }
    this.watermark = d.seq;
    if (mine) {
      const b = this.pending.find((p) => p.n === n);
      if (b) {
        this.pending = this.pending.filter((p) => p !== b);
        this.unacked.set(n, b);
        if (this.unacked.size > 200) this.unacked.delete(this.unacked.keys().next().value);
      }
    }
    if (d.reset) this.myText.clear();
    for (const id of this.#rebuild()) changed.add(id);
    return { status: 'applied', changed, replaced, mine, overwritten, reset: d.reset || null };
  }

  #ownBatch(from) {
    if (!from || !this.session) return null;
    const i = from.lastIndexOf(':');
    if (from.slice(0, i) !== this.session) return null;
    return Number(from.slice(i + 1));
  }

  // ack removes the batch from pending and returns it, also when its delta
  // already did. It never changes the server copy.
  ack(n) {
    const early = this.unacked.get(n);
    if (early) { this.unacked.delete(n); return { batch: early, changed: new Set() }; }
    const batch = this.pending.find((b) => b.n === n) || null;
    if (!batch) return { batch: null, changed: new Set() };
    this.pending = this.pending.filter((b) => b !== batch);
    return { batch, changed: this.#rebuild() };
  }

  // reject drops a permanently refused batch; a transient one stays queued.
  reject(n, retry) {
    const batch = this.pending.find((b) => b.n === n) || null;
    if (!batch || retry) return { batch, changed: new Set() };
    this.pending = this.pending.filter((b) => b !== batch);
    return { batch, changed: this.#rebuild() };
  }

  // discard drops a batch that was never sent because it cannot be sent at
  // all (over 8 MiB or 32 parts). The server never saw its number, so the
  // later pending batches, also unsent, move down by one: the next batch the
  // server receives is still max_n + 1 (TECH 4.8).
  discard(n) {
    const i = this.pending.findIndex((b) => b.n === n);
    if (i < 0) return { batch: null, changed: new Set() };
    const batch = this.pending[i];
    this.pending.splice(i, 1);
    for (const o of batch.ops) if (o.op === 'add') this.created.delete(o.el.id);
    for (const b of this.pending) if (b.n > n) b.n--;
    this.nextN--;
    return { batch, changed: this.#rebuild() };
  }

  #rebuild() {
    const old = this.overlay;
    this.overlay = new Map();
    this.overlayMeta = {};
    const ctx = {
      get: (id) => this.base(id),
      put: (el) => this.overlay.set(el.id, el),
      del: (id) => this.overlay.set(id, null),
      all: () => this.#baseAll(),
      meta: (g, val) => { if (META_KEYS.has(g)) this.overlayMeta[g] = val; },
    };
    for (const b of this.pending) for (const op of b.ops) applyOp(op, ctx);
    const changed = new Set(old.keys());
    for (const id of this.overlay.keys()) changed.add(id);
    return changed;
  }

  *#baseAll() {
    for (const [id, el] of this.server) if (!this.overlay.has(id)) yield el;
    for (const el of this.overlay.values()) if (el) yield el;
  }
}

// applyOp is the optimistic, approximate effect of one client operation. The
// canonical delta replaces it when it arrives (TECH 4.3).
export function applyOp(op, c) {
  switch (op.op) {
    case 'add':
      if (!c.get(op.el.id)) c.put({ ...op.el, inc: 0, v: {} });
      return;
    case 'set': {
      const el = c.get(op.id);
      if (!el || (el.locked && op.g !== 'locked')) return;
      c.put(withBundle(el, op.g, op.val));
      if (op.g === 'geom' && op.carry) {
        const dx = op.val.x - el.x, dy = op.val.y - el.y;
        const follows = op.carry === 'members'
          ? (m) => m.frame === el.id
          : (m) => el.group && m.group === el.group && m.id !== el.id;
        for (const m of [...c.all()]) if (follows(m)) shiftElement(m, dx, dy, c);
      }
      return;
    }
    case 'reparent':
      for (const m of targetsWithGroups(op.ids, c)) c.put({ ...m, frame: op.frame });
      return;
    case 'group':
      for (const id of op.ids) { const m = c.get(id); if (m) c.put({ ...m, group: op.group }); }
      return;
    case 'ungroup':
      for (const m of [...c.all()]) if (m.group === op.group) c.put({ ...m, group: null });
      return;
    case 'delete':
      deleteLocal(op.ids.filter((id) => { const m = c.get(id); return m && !m.locked; }), c);
      return;
    case 'delete_frame_contents': {
      const ids = [op.frame];
      for (const m of c.all()) if (m.frame === op.frame && !m.locked) ids.push(m.id);
      deleteLocal(ids, c);
      for (const m of [...c.all()]) if (m.frame === op.frame) c.put({ ...m, frame: null });
      return;
    }
    case 'zorder':
      zorderLocal(op, c);
      return;
    case 'meta':
      c.meta(op.g, op.val);
      return;
    default:
  }
}

function shiftElement(m, dx, dy, c) {
  if (m.type === 'connector') {
    const ends = m.ends.map((e) => (e.pt ? { pt: clampPoint([e.pt[0] + dx, e.pt[1] + dy]) } : e));
    c.put({ ...m, ends });
  } else {
    const [x, y] = clampPoint([m.x + dx, m.y + dy]);
    c.put({ ...m, x, y });
  }
}

function targetsWithGroups(ids, c) {
  const out = new Map();
  const groups = new Set();
  for (const id of ids) {
    const m = c.get(id);
    if (!m) continue;
    out.set(id, m);
    if (m.group) groups.add(m.group);
  }
  if (groups.size) for (const m of c.all()) if (groups.has(m.group)) out.set(m.id, m);
  return out.values();
}

function deleteLocal(ids, c) {
  const gone = new Set(ids);
  const victims = new Map();
  for (const id of ids) { const m = c.get(id); if (m) victims.set(id, m); }
  for (const m of [...c.all()]) {
    if (gone.has(m.id)) continue;
    let n = m;
    if (m.frame && gone.has(m.frame)) n = { ...n, frame: null };
    if (m.type === 'connector' && m.ends.some((e) => e.el && gone.has(e.el))) {
      n = {
        ...n,
        ends: m.ends.map((e) => (e.el && gone.has(e.el) ? { pt: clampPoint(endpointWorld(victims.get(e.el), e.at)) } : e)),
      };
    }
    if (n !== m) c.put(n);
  }
  for (const id of ids) c.del(id);
}

function zorderLocal(op, c) {
  const targets = op.ids.map((id) => c.get(id)).filter((m) => m && !m.locked);
  if (!targets.length) return;
  const set = new Set(targets.map((m) => m.id));
  const rest = [...c.all()].filter((m) => !set.has(m.id)).map((m) => m.z).sort();
  let lo = null, hi = null;
  if (op.to === 'front') lo = rest[rest.length - 1] ?? null;
  else if (op.to === 'back') hi = rest[0] ?? null;
  else {
    const ref = c.get(op.to.after || op.to.before);
    if (!ref) return;
    const i = rest.indexOf(ref.z);
    if (op.to.after) { lo = ref.z; hi = rest[i + 1] ?? null; } else { hi = ref.z; lo = i > 0 ? rest[i - 1] : null; }
  }
  targets.sort((a, b) => (a.z < b.z ? -1 : a.z > b.z ? 1 : 0));
  for (const m of targets) {
    const z = lo != null && hi != null && lo === hi ? lo + 'V' : keyBetween(lo, hi);
    c.put({ ...m, z });
    lo = z;
  }
}
