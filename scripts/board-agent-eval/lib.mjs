// Shared helpers for the board agent evaluation: argument parsing and
// reading a board through the host's HTTP API (every page of one read).

export function args(argv, spec) {
  const out = { ...spec };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (!a.startsWith('--')) throw new Error(`unexpected argument ${a}`);
    const k = a.slice(2);
    if (!(k in spec)) throw new Error(`unknown flag --${k}`);
    out[k] = argv[++i];
    if (out[k] === undefined) throw new Error(`--${k} needs a value`);
  }
  for (const [k, v] of Object.entries(out)) {
    if (v === null) throw new Error(`--${k} is required`);
  }
  return out;
}

async function call(base, method, path, body) {
  const res = await fetch(base.replace(/\/$/, '') + path, {
    method,
    headers: body ? { 'content-type': 'application/json' } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  let data;
  try { data = JSON.parse(text); } catch { data = { raw: text }; }
  if (!res.ok) throw new Error(`${method} ${path}: HTTP ${res.status}: ${text.slice(0, 500)}`);
  return data;
}

export const post = (base, path, body) => call(base, 'POST', path, body);

// readBoard returns {seq, revision, pending, elements} for the current state,
// following `next` cursors so every page comes from the same snapshot.
export async function readBoard(base, slug) {
  const path = `/api/flats/${encodeURIComponent(slug)}/board`;
  let first = null;
  const elements = [];
  let cursor = '';
  for (;;) {
    const q = new URLSearchParams({ limit: '2000' });
    if (cursor) q.set('cursor', cursor);
    const page = await call(base, 'GET', `${path}?${q}`);
    if (!first) first = page;
    elements.push(...(page.elements || []));
    if (!page.next) break;
    cursor = page.next;
  }
  return { seq: first.seq, revision: first.revision, pending: first.pending, elements };
}
