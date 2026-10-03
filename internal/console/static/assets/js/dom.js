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

// Visibility labels and widening order, mirroring internal/store.
export const VISIBILITY = {
  private: { label: 'Private', icon: 'lock', rank: 0 },
  public: { label: 'Public', icon: 'globe', rank: 1 },
  'public-unlisted': { label: 'Public · unlisted', icon: 'globe', rank: 1 },
  'public-listed': { label: 'Public · listed', icon: 'globe', rank: 2 },
};

// Notices shown before widening; the API returns the same text afterwards.
// console_test.go checks they match internal/core.
export const UNLISTED_NOTICE = 'Unlisted only hides this flat from Portal relay listings. It is NOT access control: anyone with the URL can open it.';
export const LISTED_NOTICE = 'This flat is public: anyone can open it, and it appears in Portal relay listings.';

export const PUBLIC_ACCESS_NOTICE = 'This flat is public: anyone on the internet can open it. A domain or URL is not what makes it public.';

export function noticeFor(vis) {
  if (vis === 'public') return PUBLIC_ACCESS_NOTICE;
  if (vis === 'public-unlisted') return UNLISTED_NOTICE;
  if (vis === 'public-listed') return LISTED_NOTICE;
  return '';
}

// PUBLIC_URL_NOTICE covers a public URL that came without a notice.
export const PUBLIC_URL_NOTICE = 'Anyone with the public URL can open it. Unlisted only hides a flat from Portal relay listings; it is NOT access control.';

// publicNoticeOf returns the warning to show next to a flat's public URL
// ('' when it has none). Every public URL in the console carries one.
export function publicNoticeOf(f) {
  if (!f || !f.public_url) return '';
  return f.public_notice || noticeFor(f.visibility) || PUBLIC_URL_NOTICE;
}

export function visibilityBadge(vis) {
  const v = VISIBILITY[vis] || { label: vis, icon: 'lock' };
  return h('span', { class: 'vis vis-' + vis }, icon(v.icon), h('span', { text: v.label }));
}

// slugColor derives a stable hue from the slug for initials tiles.
export function slugHue(slug) {
  let x = 0;
  for (let i = 0; i < slug.length; i++) x = (x * 31 + slug.charCodeAt(i)) >>> 0;
  return x % 360;
}

export function initials(name, slug) {
  const words = (name || slug || '?').split(/[\s\-_]+/).filter(Boolean);
  const s = words.length > 1 ? words[0][0] + words[1][0] : (words[0] || '?').slice(0, 2);
  return s.toUpperCase();
}

export function shortHash(s, n = 12) {
  return s ? s.replace(/^sha256:/, '').slice(0, n) : '';
}
