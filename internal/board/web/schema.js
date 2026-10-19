// Element schema (TECH Appendix A): style keys and defaults, bundles, and the
// limits the client checks before it sends a batch.

export const STICKY_COLORS = ['yellow', 'orange', 'pink', 'blue', 'green', 'gray'];
export const INK_COLORS = ['ink', 'gray', 'red', 'orange', 'green', 'blue', 'purple'];
export const SHAPES = ['rect', 'round', 'ellipse', 'diamond'];

export const STYLE_DEFAULTS = {
  sticky: { color: 'yellow', fontSize: 'auto' },
  text: { color: 'ink', fontSize: 20, align: 'left' },
  shape: { shape: 'rect', fill: 'white', stroke: 'ink', strokeWidth: 2, dash: 'solid', fontSize: 14 },
  connector: { stroke: 'ink', strokeWidth: 2, dash: 'solid', route: 'straight', head0: 'none', head1: 'arrow', fontSize: 12 },
  pen: { stroke: 'ink', width: 'pen' },
  image: {},
  frame: { fill: 'white' },
};

export const FONT_SIZES = {
  sticky: ['auto', 12, 14, 18, 24, 32, 48, 72],
  text: [12, 14, 16, 20, 24, 32, 48, 64, 96, 144],
  shape: [10, 12, 14, 18, 24, 32, 48, 72],
  connector: [10, 12, 14, 18, 24, 32],
};

export const LIMITS = {
  text: 4000, shortText: 200, alt: 1000, penPoints: 2000, batchOps: 500,
  coord: 10_000_000, minSize: 1, maxSize: 100_000,
};

export const KIND_NAMES = {
  sticky: 'Sticky', text: 'Text', shape: 'Shape', connector: 'Connector',
  pen: 'Drawing', image: 'Image', frame: 'Frame',
};
export const SHAPE_NAMES = { rect: 'Rectangle', round: 'Rounded rectangle', ellipse: 'Ellipse', diamond: 'Diamond' };

export const style = (el, k) => {
  const s = el.style;
  if (s && s[k] !== undefined) return s[k];
  const d = STYLE_DEFAULTS[el.type];
  return d ? d[k] : undefined;
};

export const shortText = (el) => el.type === 'frame' || el.type === 'connector';
export const textLimit = (el) => (shortText(el) ? LIMITS.shortText : LIMITS.text);

// round1 stores numbers the way the server does (Appendix A, Num).
export const round1 = (v) => Math.round(v * 10) / 10;

export function clampGeom(g) {
  const c = (v) => Math.max(-LIMITS.coord, Math.min(LIMITS.coord, v));
  const s = (v) => Math.max(LIMITS.minSize, Math.min(LIMITS.maxSize, v));
  let rot = ((g.rot || 0) % 360 + 360) % 360;
  if (rot === 360) rot = 0;
  return { x: round1(c(g.x)), y: round1(c(g.y)), w: round1(s(g.w)), h: round1(s(g.h)), rot: round1(rot) };
}

// clampPoint limits a free connector end to the coordinate range and rounds
// it as the server stores it (model.ClampPoint). An end derived from an
// element near the edge of the range, or moved past it, would otherwise be
// refused as invalid.
export function clampPoint(p) {
  const c = (v) => Math.max(-LIMITS.coord, Math.min(LIMITS.coord, v));
  return [round1(c(p[0])), round1(c(p[1]))];
}

export const geomOf = (el) => ({ x: el.x, y: el.y, w: el.w, h: el.h, rot: el.rot || 0 });

// withBundle returns a copy of el with bundle g set to val.
export function withBundle(el, g, val) {
  const n = { ...el };
  if (g === 'geom') Object.assign(n, { x: val.x, y: val.y, w: val.w, h: val.h, rot: val.rot });
  else if (g === 'end0' || g === 'end1') {
    n.ends = el.ends.slice();
    n.ends[g === 'end0' ? 0 : 1] = val;
  } else if (g.startsWith('style.')) n.style = { ...el.style, [g.slice(6)]: val };
  else n[g] = val;
  return n;
}

const RUNTIME = ['inc', 'v', 'end_inc', 'deleted', 'del_seq'];

// contentOf strips runtime fields, leaving the content fields of TECH 3.1.
export function contentOf(el) {
  const out = {};
  for (const [k, v] of Object.entries(el)) if (!RUNTIME.includes(k)) out[k] = v;
  return out;
}

// authorName turns an editor key ("ts:alice@example.com", "browser:...") into
// a short display name. Elements carry only the key.
export function authorName(by, names) {
  if (!by) return '';
  if (names && names.has(by)) return names.get(by);
  if (by.startsWith('ts:')) return by.slice(3).split('@')[0];
  return '';
}
