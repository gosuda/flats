// Recovery files (TECH 9.3): the result of applying this tab's unconfirmed
// batches on top of the server copy, with the image bytes they reference.

import { clampPoint, contentOf } from './schema.js';
import { endWorld } from './geom.js';
import { writeZip } from './zip.js';

function stable(v) {
  if (Array.isArray(v)) return '[' + v.map(stable).join(',') + ']';
  if (v && typeof v === 'object') {
    return '{' + Object.keys(v).sort().filter((k) => v[k] !== undefined).map((k) => JSON.stringify(k) + ':' + stable(v[k])).join(',') + '}';
  }
  return JSON.stringify(v);
}

// buildRecovery lists the created and changed elements (changed ones keep
// their original id in recovered_from) and the deleted ones.
export function buildRecovery(state) {
  const elements = [], deleted = [];
  for (const [id, el] of state.overlay) {
    const base = state.server.get(id);
    if (!el) {
      if (base) deleted.push({ id, text: base.text || base.alt || '' });
      continue;
    }
    const c = contentOf(el);
    if (!base) elements.push(c);
    else if (stable(c) !== stable(contentOf(base))) elements.push({ ...c, recovered_from: id });
  }
  // References that leave the file become free ends and no membership.
  const ids = new Set(elements.map((e) => e.id));
  const get = (id) => state.base(id);
  for (const e of elements) {
    if (e.frame && !ids.has(e.frame)) delete e.frame;
    if (e.type === 'connector') {
      e.ends = e.ends.map((end) => {
        if (!end.el || ids.has(end.el)) return end;
        const p = endWorld(end, get);
        return { pt: p ? clampPoint(p) : [0, 0] };
      });
    }
  }
  const groupSizes = new Map();
  for (const e of elements) if (e.group) groupSizes.set(e.group, (groupSizes.get(e.group) || 0) + 1);
  for (const e of elements) if (e.group && groupSizes.get(e.group) < 2) delete e.group;
  elements.sort((a, b) => (a.id < b.id ? -1 : 1));
  const assets = [...new Set(elements.filter((e) => e.type === 'image').map((e) => e.asset))];
  return { elements, deleted, assets };
}

export function imageExt(bytes) {
  if (bytes[0] === 0x89 && bytes[1] === 0x50) return 'png';
  if (bytes[0] === 0xff && bytes[1] === 0xd8) return 'jpg';
  if (bytes[0] === 0x47 && bytes[1] === 0x49) return 'gif';
  if (bytes[8] === 0x57 && bytes[9] === 0x45) return 'webp';
  return 'bin';
}

// recoveryZip writes the recovery file. fetchAsset(hash) resolves to the
// image bytes or null; unavailable images are listed in missing_assets.
export async function recoveryZip(rec, meta, fetchAsset, now = new Date()) {
  const files = [
    { name: 'board.json', data: JSON.stringify({ format: 1, title: meta.title || '', grid: !!meta.grid }) },
    { name: 'elements.json', data: JSON.stringify(rec.elements) },
  ];
  const missing = [];
  for (const hash of rec.assets) {
    const bytes = await fetchAsset(hash);
    if (bytes) files.push({ name: `assets/${hash}.${imageExt(bytes)}`, data: bytes });
    else missing.push(hash);
  }
  files.push({
    name: 'recovery.json',
    data: JSON.stringify({ format: 1, created_at: now.toISOString(), deleted: rec.deleted, missing_assets: missing }),
  });
  return writeZip(files, now);
}
