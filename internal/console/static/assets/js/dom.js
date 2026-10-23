// DOM building and formatting helpers. Everything is built with
// createElement/textContent so server data is never parsed as HTML.

const SVG_NS = 'http://www.w3.org/2000/svg';

// h creates an element. attrs: class, text, on<Event> handlers, dataset via
// "data-*", boolean attributes as true/false, anything else as attribute.
export function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (v === undefined || v === null || v === false) continue;
      if (k === 'class') el.className = v;
      else if (k === 'text') el.textContent = v;
      else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2).toLowerCase(), v);
      else if (v === true) el.setAttribute(k, '');
      else el.setAttribute(k, String(v));
    }
  }
  append(el, children);
  return el;
}

function append(el, children) {
  for (const c of children) {
    if (c === undefined || c === null || c === false) continue;
    if (Array.isArray(c)) append(el, c);
    else if (c instanceof Node) el.appendChild(c);
    else el.appendChild(document.createTextNode(String(c)));
  }
}

export function clear(el) {
  while (el.firstChild) el.removeChild(el.firstChild);
  return el;
}

// Icon paths on a 24x24 grid, stroked.
const ICONS = {
  lock: 'M7 11V8a5 5 0 0 1 10 0v3M6 11h12v9H6z',
  globe: 'M12 3a9 9 0 1 0 0 18a9 9 0 1 0 0-18M3 12h18M12 3c2.5 2.7 3.8 5.7 3.8 9s-1.3 6.3-3.8 9M12 3c-2.5 2.7-3.8 5.7-3.8 9s1.3 6.3 3.8 9',
  external: 'M14 4h6v6M20 4l-9 9M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5',
  more: 'M5 12h.01M12 12h.01M19 12h.01',
  list: 'M8 6h13M8 12h13M8 18h13M3.5 6h.01M3.5 12h.01M3.5 18h.01',
  grid: 'M4 4h7v7H4zM13 4h7v7h-7zM4 13h7v7H4zM13 13h7v7h-7z',
  search: 'M11 4a7 7 0 1 0 0 14a7 7 0 1 0 0-14M20 20l-4-4',
  copy: 'M9 9h11v11H9zM5 15H4V4h11v1',
  alert: 'M12 3l10 18H2zM12 10v5M12 18h.01',
  back: 'M15 18l-6-6l6-6',
  close: 'M6 6l12 12M18 6L6 18',
  home: 'M3.5 11.5L12 4l8.5 7.5M6 9.5V19a1 1 0 0 0 1 1h10a1 1 0 0 0 1-1V9.5M10 20v-4.5a2 2 0 0 1 4 0V20',
  doc: 'M14 3H7a1 1 0 0 0-1 1v16a1 1 0 0 0 1 1h10a1 1 0 0 0 1-1V7zM14 3v4h4M9 12h6M9 16h4',
  code: 'M8 7l-5 5l5 5M16 7l5 5l-5 5M14 4l-4 16',
  layout: 'M4 4h9v7H4zM15 4h5v16h-5zM4 13h9v7H4z',
  board: 'M3 4h18v16H3zM6 7h5v5H6zM13 16l5-5M15 11h3v3',
  chevrons: 'M6 7l5 5l-5 5M13 7l5 5l-5 5',
  spinner: 'M12 3a9 9 0 1 0 9 9',
};

export function icon(name, label) {
  const svg = document.createElementNS(SVG_NS, 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('class', 'icon');
  svg.setAttribute('focusable', 'false');
  if (label) {
    svg.setAttribute('role', 'img');
    svg.setAttribute('aria-label', label);
  } else {
    svg.setAttribute('aria-hidden', 'true');
  }
  const p = document.createElementNS(SVG_NS, 'path');
  p.setAttribute('d', ICONS[name] || '');
  svg.appendChild(p);
  return svg;
}

// --- formatting ---

const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto', style: 'short' });
const UNITS = [
  ['year', 365 * 24 * 3600], ['month', 30 * 24 * 3600], ['week', 7 * 24 * 3600],
  ['day', 24 * 3600], ['hour', 3600], ['minute', 60], ['second', 1],
];

export function relTime(iso) {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '';
  const diff = (t - Date.now()) / 1000;
  for (const [unit, secs] of UNITS) {
    if (Math.abs(diff) >= secs || unit === 'second') {
      return rtf.format(Math.round(diff / secs), unit);
    }
  }
  return '';
}

const dtf = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' });
export function dateTime(iso) {
  const t = Date.parse(iso);
  return Number.isNaN(t) ? '' : dtf.format(t);
}

// timeEl renders a relative time with the absolute time on hover.
export function timeEl(iso) {
  return h('time', { datetime: iso, title: dateTime(iso), text: relTime(iso) });
}

export function bytes(n) {
  n = Number(n) || 0;
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n : n.toFixed(n < 10 ? 1 : 0)) + ' ' + units[i];
}

export function plural(n, word) {
  return `${n} ${word}${n === 1 ? '' : 's'}`;
}

// Visibility labels and widening order, mirroring internal/store. Legacy
// public-listed and public-unlisted values are shown as Public.
export const VISIBILITY = {
  private: { label: 'Private', icon: 'lock', rank: 0 },
  public: { label: 'Public', icon: 'globe', rank: 1 },
};

export const visibilityOf = (vis) => (!vis || vis === 'private' ? 'private' : 'public');

// A Public board also has a public access level, view or edit
// (docs/spec/board-public-edit.md). accessKey combines both into one of
// 'private', 'public' (any Public flat that is not a board), 'view' or
// 'edit'. Only a board reports public_access.
export function accessKey(vis, access) {
  if (visibilityOf(vis) === 'private') return 'private';
  if (access === 'edit') return 'edit';
  return access === 'view' ? 'view' : 'public';
}
// accessOf reads a flat; a Public board without a reported level is view only.
export function accessOf(f) {
  const key = accessKey(f.visibility, f.public_access);
  return key === 'public' && f.type === 'board' ? 'view' : key;
}

export const ACCESS = {
  private: { label: 'Private', icon: 'lock', desc: 'Only people on your networks' },
  public: { label: 'Public', icon: 'globe', desc: 'Anyone on the internet can open it' },
  view: { label: 'Public, view only', icon: 'globe', desc: 'Anyone with the link can watch live. Only your networks edit.' },
  edit: { label: 'Public, anyone can edit', icon: 'globe', desc: 'Anyone on the internet can edit and delete. They type a name first.' },
};
export const accessLabel = (vis, access) => ACCESS[accessKey(vis, access)].label;

// Notice shown before making a flat public; the API returns the same text
// afterwards. console_test.go checks it matches internal/core.
export const PUBLIC_ACCESS_NOTICE = 'This flat is public: anyone on the internet can open it. A domain or URL is not what makes it public.';

// BECOMING_PUBLIC_NOTICE warns before a flat is made public; the flat is not
// public yet, so it must not say that it is.
export const BECOMING_PUBLIC_NOTICE = 'Once public, anyone on the internet can open this flat. A domain or URL is not what makes it public.';
export function becomingNoticeFor(vis) {
  return visibilityOf(vis) === 'public' ? BECOMING_PUBLIC_NOTICE : '';
}

// NETWORK_LABELS names every provider ID for people.
export const NETWORK_LABELS = { local: 'Local', tailscale: 'Tailscale', netbird: 'NetBird', proxy: 'Reverse proxy', 'tailscale-funnel': 'Tailscale Funnel', portal: 'Portal', zrok: 'zrok', 'cloudflare-quick': 'Cloudflare Quick Tunnel', 'netbird-expose': 'NetBird Expose', 'proxy-public': 'Reverse proxy (public)' };
export const PUBLIC_NETWORKS = ['tailscale-funnel', 'portal', 'zrok', 'cloudflare-quick', 'netbird-expose', 'proxy-public'];

// publicURL is the current public address: the server's public_url, or the
// URL of a current public route.
export function publicURL(f) {
  if (!f) return '';
  if (f.public_url) return f.public_url;
  const ep = (f.endpoints || []).find((e) => e.audience === 'current' && e.url && PUBLIC_NETWORKS.includes(e.provider));
  return ep ? ep.url : '';
}

// proxyRoute is the flat's current route on a reverse proxy provider (proxy
// or proxy-public), or null. A served route's detail says that Flats cannot
// check the proxy; a route in error has no address and its detail says why.
export function proxyRoute(f, provider) {
  return ((f && f.endpoints) || []).find((e) => e.provider === provider && e.audience === 'current' &&
    (!e.host || e.host === f.slug) && e.state !== 'stopped') || null;
}

// proxyDetail is the detail of the reverse proxy route serving url, or ''.
export function proxyDetail(f, url) {
  const ep = ['proxy', 'proxy-public'].map((p) => proxyRoute(f, p)).find((e) => e && e.url && e.url === url);
  return ep ? ep.detail || '' : '';
}

// linkTargets lists the addresses that open a flat, split into private and
// public ones: one per allowed network that has a route. Without a private
// route it falls back to the private URL, and without a public one to the
// server's public_url, which is already limited to this flat's permitted
// routes. Public addresses are listed only while the flat is public.
export function linkTargets(f) {
  const out = { private: [], public: [] };
  if (!f) return out;
  const add = (list, provider, url, label) => {
    if (url && !list.some((t) => t.url === url)) list.push({ provider, url, public: list === out.public, label: label || NETWORK_LABELS[provider] || provider });
  };
  const isPublic = visibilityOf(f.visibility) === 'public';
  for (const e of f.endpoints || []) {
    if (e.audience !== 'current' || !e.url || !e.permitted || (e.host && e.host !== f.slug) || e.state === 'stopped') continue;
    if (!PUBLIC_NETWORKS.includes(e.provider)) add(out.private, e.provider, e.url);
    else if (isPublic) add(out.public, e.provider, e.url);
  }
  if (!out.private.length) add(out.private, '', f.private_url, 'private URL');
  if (isPublic && !out.public.length) add(out.public, '', f.public_url, 'public URL');
  return out;
}

// publicNoticeOf returns the warning to show next to a flat's public URL
// ('' when it has none). Every public URL in the console carries one.
export function publicNoticeOf(f) {
  if (!publicURL(f)) return '';
  return f.public_notice || PUBLIC_ACCESS_NOTICE;
}

// visibilityBadge never shows an editable board as plain Public (rule 34).
export function visibilityBadge(vis, access) {
  const key = accessKey(vis, access);
  const v = ACCESS[key];
  return h('span', { class: 'vis vis-' + visibilityOf(vis) + (key === 'edit' ? ' vis-edit' : '') }, icon(v.icon), h('span', { text: v.label }));
}


export function shortHash(s, n = 12) {
  return s ? s.replace(/^sha256:/, '').slice(0, n) : '';
}
