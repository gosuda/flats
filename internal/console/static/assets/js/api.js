// Client for /console/api. Every request carries X-Flats-Console: 1; the
// browser adds Sec-Fetch-Site, which the server checks on mutations.

const BASE = '/console/api';

export class ApiError extends Error {
  constructor(status, body) {
    super((body && body.error) || `request failed (HTTP ${status})`);
    this.status = status;
    this.body = body || {};
  }
  get problems() { return this.body.problems || []; }
  get health() { return this.body.health || null; }
}

export async function request(method, path, body) {
  const headers = { 'X-Flats-Console': '1', Accept: 'application/json' };
  const opts = { method, headers, credentials: 'same-origin', cache: 'no-store' };
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(BASE + path, opts);
  } catch (e) {
    throw new ApiError(0, { error: 'cannot reach the Flats server: ' + e.message });
  }
  let data = null;
  try { data = await res.json(); } catch { /* empty or non-JSON body */ }
  if (!res.ok) throw new ApiError(res.status, data);
  return data;
}

const enc = encodeURIComponent;
const get = (p) => request('GET', p);
const post = (p, b) => request('POST', p, b === undefined ? {} : b);

export const api = {
  status: () => get('/status'),
  flats: (q) => get('/flats' + (q ? '?q=' + enc(q) : '')),
  flat: (slug) => get(`/flats/${enc(slug)}`),
  versions: (slug) => get(`/flats/${enc(slug)}/versions`),
  deploy: (slug, version) => post(`/flats/${enc(slug)}/deploy`, { version }),
  rollback: (slug, version, restoreData) => post(`/flats/${enc(slug)}/rollback`, { version: version || 0, restore_data: !!restoreData }),
  deployments: (slug) => get(`/flats/${enc(slug)}/deployments`),
  openPreview: (slug, version) => post(`/flats/${enc(slug)}/previews`, { version }),
  previews: (slug) => get(`/flats/${enc(slug)}/previews`),
  closePreview: (host) => request('DELETE', `/previews/${enc(host)}`),
  setVisibility: (slug, visibility) => post(`/flats/${enc(slug)}/visibility`, { visibility, reason: '' }),
  setName: (slug, name) => post(`/flats/${enc(slug)}/name`, { name }),
  rename: (slug, to) => post(`/flats/${enc(slug)}/rename`, { slug: to }),
  remove: (slug) => request('DELETE', `/flats/${enc(slug)}?reason=${enc('deleted in the console')}`),
  logs: (slug, after) => get(`/flats/${enc(slug)}/logs?limit=200` + (after ? '&after=' + after : '')),
  secrets: (slug) => get(`/flats/${enc(slug)}/secrets`),
  putSecret: (slug, name, value) => request('PUT', `/flats/${enc(slug)}/secrets/${enc(name)}`, { value }),
  deleteSecret: (slug, name) => request('DELETE', `/flats/${enc(slug)}/secrets/${enc(name)}`),
  snapshots: (slug) => get(`/flats/${enc(slug)}/snapshots`),
  stats: (slug, days) => get(`/flats/${enc(slug)}/stats?days=${days || 30}`),
  approvals: (status) => get('/approvals' + (status ? '?status=' + enc(status) : '')),
  approval: (id) => get(`/approvals/${enc(id)}`),
  decide: (id, approve) => post(`/approvals/${enc(id)}/${approve ? 'approve' : 'reject'}`),
  settings: () => get('/settings'),
  saveSettings: (values) => request('PUT', '/settings', values),
};

// newestVersion returns the highest saved version number, or 0.
export async function newestVersion(slug) {
  const { versions } = await api.versions(slug);
  return (versions || []).reduce((m, v) => Math.max(m, v.number), 0);
}

// Thumbnails are version files behind the console API, which needs the
// console header, so they are fetched and inlined as data: URLs (allowed by
// the CSP). Results are cached for the page's lifetime.
const thumbs = new Map();
const MAX_THUMB = 2 << 20;

export function thumbnail(url) {
  if (!thumbs.has(url)) thumbs.set(url, loadThumb(url));
  return thumbs.get(url);
}

// thumbPath maps core's thumbnail URL (/api/flats/{slug}/versions/{n}/files/
// {path}) to a console API path. core inserts the manifest's screenshot path
// unescaped, so each segment after /files/ is escaped here: a name with "#",
// "?" or "%" would otherwise be cut off or misread.
export function thumbPath(url) {
  const m = /^\/(?:console\/)?api(\/flats\/[^/?#]+\/versions\/\d+\/files\/)(.+)$/.exec(String(url));
  if (!m) return null;
  return m[1] + m[2].split('/').map(enc).join('/');
}

async function loadThumb(url) {
  // core reports thumbnails on the agent API; read them through the console API.
  const path = thumbPath(url);
  if (!path) return null;
  try {
    const res = await fetch(BASE + path, { headers: { 'X-Flats-Console': '1' }, credentials: 'same-origin' });
    if (!res.ok) return null;
    const blob = await res.blob();
    if (!blob.type.startsWith('image/') || blob.size > MAX_THUMB) return null;
    return await new Promise((resolve) => {
      const r = new FileReader();
      r.onload = () => resolve(typeof r.result === 'string' ? r.result : null);
      r.onerror = () => resolve(null);
      r.readAsDataURL(blob);
    });
  } catch {
    return null;
  }
}
