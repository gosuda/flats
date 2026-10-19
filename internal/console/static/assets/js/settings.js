// Settings: four sections behind a left nav (Networks, Console, Limits,
// Agents). Networks lists the providers, each with a detail page that also
// holds its key. Every setting, provider, key, address and fact is the same
// row; fields stage changes for one save bar per page, while buttons such as
// Turn on, Add, Replace and Delete act at once.

import { h, dateTime, relTime, plural } from './dom.js';
import { api } from './api.js';
import { errorPanel, loading, toast, extLink, busy, fill, confirmDialog, infoDialog } from './ui.js';

export const SECTIONS = [['networks', 'Networks'], ['console', 'Console'], ['limits', 'Limits'], ['agents', 'Agents']];

// Limits shown in human units; values are stored as integers in base units.
// Byte sizes (bytes: true) have a unit menu; unit and factor name the unit
// used for 0 and for a new value. group places a field on its page.
const FIELDS = [
  { key: 'upload_max_bytes', group: 'storage', label: 'Upload size', unit: 'MB', factor: 1 << 20, bytes: true, help: 'Total uncompressed size of one saved version.' },
  { key: 'disk_quota_bytes', group: 'storage', label: 'Disk per flat', perFlat: true, unit: 'GB', factor: 1 << 30, bytes: true, help: 'A change that would exceed it is refused; nothing is deleted. 0 means no quota.' },
  { key: 'version_quota', group: 'history', label: 'Versions', perFlat: true, unit: 'versions', factor: 1, step: 1, whole: true, help: 'For websites and pages. Never deleted automatically; a full quota refuses new versions. Lowering it keeps existing versions. 0 is unlimited.' },
  { key: 'revision_quota', group: 'history', label: 'Revisions', perFlat: true, unit: 'revisions', factor: 1, step: 1, whole: true, help: 'Unpinned revisions each document, design or board keeps. Older ones are deleted at once when lowered. 0 keeps all.' },
  { key: 'pin_quota', group: 'history', label: 'Pinned revisions', perFlat: true, unit: 'pins', factor: 1, step: 1, whole: true, help: 'Pinned revisions are never deleted automatically. A full quota refuses new pins. 0 is unlimited.' },
  { key: 'events_keep', group: 'history', label: 'Log events', unit: 'events', factor: 1, step: 1, min: 1, whole: true, help: 'Kept per flat. Older events are deleted.' },
  { key: 'rate_limit_rps', group: 'traffic', label: 'Rate limit', unit: 'req/s', factor: 1, step: 1, min: 1, help: 'Requests per second per flat. Bursts up to twice this are allowed.' },
  { key: 'preview_ttl_seconds', group: 'traffic', label: 'Preview idle timeout', unit: 'hours', factor: 3600, min: 0.01, help: 'A preview closes after this long without visits.' },
  { key: 'redirect_days', group: 'traffic', label: 'Rename redirect', unit: 'days', factor: 1, step: 1, help: 'How long old addresses redirect after a flat is renamed.' },
  { key: 'portal_max_relays', group: 'portal', label: 'Active relays', unit: 'relays', factor: 1, step: 1, min: 1, restart: true, help: 'How many discovered relays a public flat uses at once.' },
];

const LIMIT_GROUPS = [
  ['storage', 'Storage', ''],
  ['history', 'History', 'Versions for websites and pages; revisions for documents, designs and boards.'],
  ['traffic', 'Traffic and addresses', ''],
];

// LABELS names every setting in messages.
const LABELS = Object.fromEntries([...FIELDS.map((f) => [f.key, f.label]),
  ['portal_relays', 'Relay list'], ['portal_discovery', 'Discover more relays'], ['portal_hide', 'Hide public flats from relay listings'],
  ['default_private_provider', 'Private network for new flats'], ['default_public_provider', 'Public network for new flats'],
  ['external_origins', 'External origins'], ['preferred', 'Address for links']]);

// Lower values of these settings remove data at the next pruning.
const RETENTION = ['events_keep', 'preview_ttl_seconds'];

// revisionsDrop reports a revision quota change that deletes revisions at
// once: a lower limit, or any limit where 0 kept all of them. The server
// cannot count them in advance, so the confirmation says so in words.
export function revisionsDrop(current, next) {
  const cur = Number(current || 0);
  const n = Number(next);
  return n !== 0 && (cur === 0 || n < cur);
}

const SOURCES = { default: 'default', file: 'config.json', flag: 'a flag for this run' };
// SOURCE_TAGS label a read-only value's source.
const SOURCE_TAGS = { default: 'default', file: 'config.json', flag: 'flag for this run' };

// Read-only config.json values: [key, label, value(config), description].
const CONFIG_ROWS = [
  ['host.management_addr', 'Management address', (c) => c.host.management_addr, ''],
  ['host.local_addr', 'Local network address', (c) => c.host.local_addr, ''],
  ['host.server_runtime', 'Server flats', (c) => (c.host.server_runtime ? 'on' : 'off'), 'Run JavaScript server apps.'],
  ['network.private_backend', 'Private routes', (c) => c.network.private_backend, 'The network new flats use when no private default is chosen.'],
];

// --- page shell ---

export function mount(main, [section, sub], ctx) {
  const active = section || 'networks';
  const known = SECTIONS.some(([id]) => id === active) && (!sub || active === 'networks');
  const notice = h('div');
  const content = h('div', { class: 's-content' }, loading());
  main.appendChild(h('div', { class: 's-shell' },
    h('aside', { class: 's-side' },
      h('h1', { text: 'Settings' }),
      h('nav', { 'aria-label': 'Settings' }, SECTIONS.map(([id, label]) =>
        h('a', { href: '/settings/' + id, 'data-nav': true, 'aria-current': id === active ? 'page' : null, text: label })))),
    h('div', { class: 's-main' }, notice, content)));
  if (!known) {
    ctx.setTitle('Not found');
    fill(content, h('p', null, 'No such settings page. ', h('a', { href: '/settings', 'data-nav': true, text: 'Back to Settings' })));
    return null;
  }

  // Tailnet nodes and enrollments finish in the background: while one is
  // pending, the page reloads every few seconds, except while the operator
  // is typing or has staged a change.
  let refreshTimer = null;
  // page.token identifies the latest load: an older one, or a background
  // refresh that finds the operator editing, draws nothing.
  // page.forms are the staged forms on screen; page.collect gathers those a
  // load draws, and replaces page.forms only if that load draws.
  const page = { ctx, content, notice, refresh, token: null, forms: [], collect: [] };
  let seq = 0;
  function refresh(pending) {
    clearTimeout(refreshTimer);
    if (!pending) return;
    refreshTimer = setTimeout(() => {
      if (!ctx.alive()) return;
      if (editing(content)) { refresh(pending); return; }
      load(undefined, true);
    }, 5000);
    // Never keep a test process alive (Node timers have unref; browsers ignore it).
    if (refreshTimer && typeof refreshTimer.unref === 'function') refreshTimer.unref();
  }
  const load = (saved, background) => {
    page.token = { id: ++seq, background: !!background };
    // An immediate action (Add, Delete, Turn on, a key) reloads the page:
    // keep what the operator staged but has not saved. The old forms are
    // read when the new page has drawn, so edits and Discard made while the
    // reload ran count. A save reloads with its result and starts clean.
    const token = page.token;
    const drawn = [];
    page.collect = drawn;
    const done = () => {
      if (page.token !== token || !token.drawn) return;
      const staged = Object.assign({}, ...page.forms.map((f) => f.snapshot()));
      // Console has independently saved link and proxy settings. A save
      // clears its own fields while retaining edits in the other form.
      if (saved) for (const key of [...(saved.applied || []), ...(saved.restart_required || [])]) delete staged[key];
      page.forms = drawn;
      for (const f of drawn) f.restore(staged);
    };
    let run;
    if (active === 'networks' && sub) run = providerPage(page, sub, saved, load);
    else if (active === 'networks') run = networksPage(page, saved, load);
    else if (active === 'console') run = consolePage(page, saved, load);
    else if (active === 'limits') run = limitsPage(page, saved, load);
    else run = agentsPage(page);
    return Promise.resolve(run).then(done);
  };
  load();
  return () => clearTimeout(refreshTimer);
}

// editing reports whether the operator is typing in the page or has staged
// a change there.
function editing(root) {
  const active = globalThis.document && document.activeElement;
  if (active && typeof root.contains === 'function' && root.contains(active) && ['INPUT', 'TEXTAREA', 'SELECT'].includes(active.tagName)) return true;
  if (typeof root.querySelectorAll !== 'function') return false;
  if ([...root.querySelectorAll('input[type="password"], input[type="text"]')].some((i) => i.value)) return true;
  return root.querySelectorAll('[data-dirty="true"]').length > 0 || [...root.querySelectorAll('.savebar')].some((b) => !b.hidden);
}

// pageHead is a page's title, optional crumb, status and action, and intro.
function pageHead({ title, crumb, status, action, intro, after }) {
  return h('div', { class: 's-head' },
    crumb ? h('p', { class: 's-crumb' }, h('a', { href: crumb[1], 'data-nav': true, text: crumb[0] }), h('span', { 'aria-hidden': 'true', text: '/' }), h('span', { text: title })) : null,
    h('div', { class: 's-title-row' }, h('div', { class: 's-title' }, h('h2', { text: title }), status), action),
    intro ? h('p', { class: 's-intro' }, intro) : null,
    after);
}

// group is a heading over a bordered list of rows (or other content).
function group(id, title, hint, ...body) {
  return h('section', { class: 's-group', id, 'aria-labelledby': id + '-title' },
    h('div', { class: 's-group-head' }, h('h3', { id: id + '-title', text: title }), hint ? h('p', null, hint) : null),
    ...body);
}

const list = (...rows) => h('div', { class: 's-list' }, ...rows);

// row is the one row anatomy: title, description and meta on the left; a
// status and at most two controls on the right. With href it opens a page.
function row({ id, title, labelFor, desc, meta, ctl, cls, href, stack }) {
  const titleEl = labelFor ? h('label', { class: 's-row-title', for: labelFor }, title) : h('span', { class: 's-row-title' }, title);
  const metaEl = meta && [].concat(meta).filter(Boolean).length
    ? h('span', { class: 's-row-meta' }, [].concat(meta).filter(Boolean).map((m, i) => [i ? h('span', { class: 'sep', 'aria-hidden': 'true', text: '·' }) : null, typeof m === 'string' ? h('span', { text: m }) : m]))
    : null;
  const main = h('div', { class: 's-row-main' }, titleEl, desc ? h('p', { class: 's-row-desc' }, desc) : null, metaEl);
  const ctlEl = ctl ? h('div', { class: 's-row-ctl' }, ctl, href ? chevron() : null) : (href ? h('div', { class: 's-row-ctl' }, chevron()) : null);
  const classes = ['s-row', href ? 'is-link' : '', stack ? 'stack' : '', cls || ''].filter(Boolean).join(' ');
  if (href) return h('a', { class: classes, id, href, 'data-nav': true }, main, ctlEl);
  return h('div', { class: classes, id }, main, ctlEl);
}

function chevron() {
  return h('span', { class: 's-chev', 'aria-hidden': 'true', text: '›' });
}

// pill reports live state; tag describes a setting's role. They never swap.
export function pill(text, kind) {
  return h('span', { class: 'pill pill-' + (kind || 'off'), text });
}
function tag(text, kind) {
  return h('span', { class: 'tag' + (kind ? ' tag-' + kind : ''), text });
}

// stateKind maps a node or relay state to a pill kind.
export function stateKind(state) {
  if (state === 'ready') return 'on';
  if (state === 'starting' || state === 'key-expiring' || state === 'connecting') return 'busy';
  if (state === 'needs-login' || state === 'error' || state === 'failed') return 'err';
  return 'off';
}
const STATE_TEXT = { ready: 'Ready', starting: 'Starting', 'key-expiring': 'Key expiring', 'needs-login': 'Needs login', error: 'Error', failed: 'Failed' };
const statePill = (state) => pill(STATE_TEXT[state] || state || 'Starting', stateKind(state || 'starting'));

function callout(kind, ...body) {
  return h('div', { class: 'callout callout-' + kind, role: kind === 'warn' || kind === 'err' ? 'status' : null }, h('div', null, ...body));
}

// changedNotice tells the operator that config.json changed on disk.
function changedNotice(cfg) {
  if (!cfg || !cfg.changed_on_disk) return null;
  return h('div', { class: 'alert alert-warn', role: 'status' },
    h('strong', { text: 'config.json changed on disk' }),
    h('p', { text: 'Restart Flats to load it. Until then Flats keeps the settings it loaded, and saving here is refused.' }));
}

// withReason disables button when reason is set and shows why. A disabled
// button gets no mouse events and is skipped by Tab, so the reason sits on a
// wrapper that takes focus and shows it as a tooltip on hover and on
// keyboard focus; screen readers read it as the wrapper's description.
let reasonSeq = 0;
export function withReason(button, reason) {
  if (!reason) return button;
  const id = 'reason-' + (++reasonSeq);
  button.disabled = true;
  button.setAttribute('disabled', '');
  button.setAttribute('aria-describedby', id);
  return h('span', { class: 'disabled-reason', tabindex: '0', 'data-reason': reason,
    'aria-label': `${button.textContent} is unavailable`, 'aria-describedby': id },
  button, h('span', { id, class: 'sr-only', text: reason }));
}

// locked explains a blocked action in place.
function locked(text) {
  return h('p', { class: 's-locked' }, h('span', { 'aria-hidden': 'true', class: 's-lock' }), h('span', null, text));
}

// linkify turns http(s) URLs in text into links (for login URLs in details).
function linkify(text) {
  const parts = [];
  let last = 0;
  for (const m of String(text).matchAll(/https?:\/\/[^\s"'<>]+/g)) {
    parts.push(text.slice(last, m.index), extLink(m[0]));
    last = m.index + m[0].length;
  }
  parts.push(text.slice(last));
  return h('span', null, parts);
}

// --- network providers ---

// PROVIDER_INFO describes each provider for the operator. Local is always on;
// the others are off until turned on here, and a flat uses one only after it
// is also allowed on that flat.
export const PROVIDER_INFO = {
  local: { label: 'Local', short: 'This device', what: 'This device, through localhost.' },
  tailscale: { label: 'Tailscale', short: 'Devices your tailnet ACL allows', noun: 'node', what: 'Each flat and preview gets its own tailnet node, open to the devices your tailnet ACL allows.' },
  netbird: { label: 'NetBird', short: 'Peers your NetBird policies allow', noun: 'peer', what: 'Each flat runs on its own NetBird peer at http://SLUG.<NetBird domain>/, open to the peers your NetBird access policies allow.' },
  portal: { label: 'Portal', short: 'Through Portal relays', what: 'Public flats are served through Portal relays. Anyone on the internet can open them.' },
  'cloudflare-quick': { label: 'Cloudflare Quick Tunnel', short: 'Random trycloudflare.com address', what: 'Anyone on the internet, through an anonymous Cloudflare quick tunnel on a random trycloudflare.com address. No Cloudflare account or domain needed.' },
  'tailscale-funnel': { label: 'Tailscale Funnel', short: 'Through the flat’s tailnet node', what: 'Anyone on the internet, through Tailscale Funnel on the flat’s tailnet node. Visitors do not need Tailscale.' },
  zrok: { label: 'zrok', short: 'Public shares on your zrok account', what: 'Anyone on the internet, through zrok public shares on your zrok account.' },
  'netbird-expose': { label: 'NetBird Expose', short: 'NetBird’s reverse proxy', what: 'Anyone on the internet, through NetBird’s reverse proxy, from the flat’s own NetBird peer.' },
};
const infoOf = (id) => PROVIDER_INFO[id] || { label: id, short: '', what: '' };

// DEFAULT_KEYS are the settings naming the provider new flats inherit.
const DEFAULT_KEYS = { private: 'default_private_provider', public: 'default_public_provider' };

// effectiveDefault is the provider new flats inherit in scope ('' = none).
export function effectiveDefault(scope, data) {
  const v = (data.settings || {})[DEFAULT_KEYS[scope]] || '';
  if (scope === 'private' && !v) return (data.config && data.config.network.private_backend) || 'local';
  return v;
}

// loadAll fetches what a page needs; settings failing is fatal, the others
// are shown as errors where they are used.
async function loadAll(page, want) {
  const token = page.token;
  const calls = { settings: api.settings, providers: api.providers, keys: api.keys, domains: api.consoleDomains };
  const names = ['settings', ...want];
  const res = await Promise.allSettled(names.map((n) => calls[n]()));
  if (!page.ctx.alive() || page.token !== token) return null;
  // The operator may have started typing while a background refresh ran.
  if (token && token.background && editing(page.content)) {
    page.refresh(true);
    return null;
  }
  if (token) token.drawn = true;
  const out = {};
  names.forEach((n, i) => { out[n] = res[i].status === 'fulfilled' ? res[i].value : null; out[n + 'Error'] = res[i].reason; });
  if (!out.settings) {
    fill(page.content, errorPanel(out.settingsError, 'Cannot load settings'));
    return null;
  }
  fill(page.notice, changedNotice(out.settings.config));
  return out;
}

// providerState is the pill for a provider's host state.
function providerState(p) {
  if (p.id === 'local') return pill('Always on', 'on');
  return p.enabled ? pill('On', 'on') : pill('Off', 'off');
}

// providerMeta is the one-line summary under a provider in the list.
function providerMeta(p, keys) {
  const info = infoOf(p.id);
  const meta = [info.short];
  if (p.id === 'local') {
    if (p.address) meta.push(h('code', { text: p.address }));
    return meta;
  }
  const key = (keys || []).find((k) => k.id === (p.id === 'netbird-expose' ? 'netbird' : p.id));
  if (p.unsupported) meta.push('Not in this build');
  else if (p.id === 'zrok' && !p.connected) meta.push('Needs an account token');
  else if ((p.id === 'netbird' || p.id === 'netbird-expose') && !(p.key_set || (key && key.set))) meta.push('Needs a setup key');
  const hosts = p.enabled && p.status ? (p.status.hosts || []).length : 0;
  if (hosts && info.noun) meta.push(plural(hosts, info.noun));
  meta.push(p.flats.length ? `Allowed on ${plural(p.flats.length, 'flat')}` : 'Not allowed on any flat');
  return meta;
}

// networksPage: defaults for new flats, then every provider as a row that
// opens its detail page.
async function networksPage(page, saved, reload) {
  page.ctx.setTitle('Networks · Settings');
  const d = await loadAll(page, ['providers', 'keys']);
  if (!d) return;
  const providers = d.providers ? (d.providers.providers || []) : [];
  const keys = d.keys ? (d.keys.keys || []) : [];
  const byScope = (scope) => providers.filter((p) => p.scope === scope);
  const providerRows = (scope) => byScope(scope).map((p) => {
    const isDefault = effectiveDefault(scope, d.settings) === p.id;
    return row({ id: 'provider-' + p.id, href: '/settings/networks/' + encodeURIComponent(p.id),
      title: [infoOf(p.id).label, isDefault ? tag('Default') : null], meta: providerMeta(p, keys), ctl: providerState(p) });
  });
  fill(page.content,
    pageHead({ title: 'Networks', intro: 'Where flats can be reached. Turn a network on here, then allow it on each flat that should use it. Turning a network on never makes a flat public.' }),
    d.providers ? defaultsForm(d.settings, providers, saved, reload, page.notice, page) : errorPanel(d.providersError, 'Cannot load network providers'),
    d.providers ? group('providers-private', 'Private', 'Reachable from this device or your own network.', list(...providerRows('private'))) : null,
    d.providers ? group('providers-public', 'Public', 'Reachable from the internet, only after you approve it per flat.', list(...providerRows('public'))) : null);
}

// defaultsForm picks the provider every new flat is allowed on for each
// scope. Only providers turned on for the host can be a default.
function defaultsForm(data, providers, saved, reload, notice, page) {
  const choices = (scope) => {
    const out = scope === 'public' ? [['', 'None']] : [];
    for (const p of providers) if (p.scope === scope && (p.id === 'local' || p.enabled)) out.push([p.id, p.id === 'local' ? 'Local only' : infoOf(p.id).label]);
    return out;
  };
  return stagedForm({
    id: 'defaults', data, saved, reload, notice, page,
    groups: [['defaults', 'New flats', 'Applies to flats created from now on.', [
      { key: DEFAULT_KEYS.private, kind: 'select', label: 'Private network', options: choices('private'), initial: effectiveDefault('private', data),
        help: 'New flats are reachable here. Local is always included.' },
      { key: DEFAULT_KEYS.public, kind: 'select', label: 'Public network', options: choices('public'), initial: effectiveDefault('public', data),
        help: 'Used once you approve making a flat public. New flats always start Private.' },
    ]]],
  });
}

// providerPage shows one provider: header with state and the immediate
// action, then its key, then status, then where it is allowed.
async function providerPage(page, id, saved, reload) {
  const d = await loadAll(page, ['providers', 'keys']);
  if (!d) return;
  if (!d.providers) { fill(page.content, errorPanel(d.providersError, 'Cannot load network providers')); return; }
  const providers = d.providers.providers || [];
  const p = providers.find((x) => x.id === id);
  const info = infoOf(id);
  page.ctx.setTitle(`${info.label} · Settings`);
  if (!p) {
    fill(page.content, pageHead({ title: info.label, crumb: ['Networks', '/settings/networks'] }), h('p', { class: 'muted', text: 'No such network on this host.' }));
    return;
  }
  const keys = d.keys ? (d.keys.keys || []) : [];
  const keyOf = (kid) => keys.find((k) => k.id === kid);
  // Without the key state the page offers no key action: an unknown key
  // must not look unset.
  const keysFailed = !d.keys && ['tailscale', 'netbird', 'zrok'].includes(p.id) && !p.unsupported;
  const keyError = () => errorPanel(d.keysError, 'Cannot load the keys');
  const data = d.settings;
  const etag = data.config ? data.config.etag : '';
  const after = () => reload();
  const isDefault = effectiveDefault(p.scope, data) === p.id;
  // NetBird without a setup key shows numbered steps; the switch is step 2.
  const setup = p.id === 'netbird' && !keysFailed && !p.unsupported && !p.enabled && !p.key_set && !(keyOf('netbird') || {}).set;
  const action = setup ? null : providerToggle(p, info, etag, after);
  const head = pageHead({
    title: info.label, crumb: ['Networks', '/settings/networks'],
    status: [providerState(p), isDefault ? tag(p.scope === 'public' ? 'Default public' : 'Default private') : null],
    action: action && action.button,
    intro: info.what,
    after: action && action.reason ? locked(action.reason) : null,
  });
  const parts = [head];
  const keyParts = [];
  if (p.id === 'tailscale') keyParts.push(group('key', 'Auth key', 'Stored encrypted in flats.db. Never shown again.', keysFailed ? keyError() : keySection(keyOf('tailscale'), TAILSCALE_KEY, after)));
  // A build without a provider (NetBird) says why on both of its pages.
  if (p.unsupported) parts.push(callout('warn', p.unsupported));
  if (p.id === 'netbird' && !p.unsupported) {
    if (setup) parts.push(netbirdSteps(p, keyOf('netbird'), etag, after));
    else keyParts.push(group('key', 'Setup key', 'Stored encrypted in flats.db. Never shown again.', keysFailed ? keyError() : keySection(keyOf('netbird'), NETBIRD_KEY, after)));
  }
  if (p.id === 'netbird-expose' && !p.unsupported) {
    parts.push(callout('info', 'A public flat is exposed through NetBird’s reverse proxy, which needs the proxy feature on your NetBird account or server. Its peer registers with the setup key under ',
      h('a', { href: '/settings/networks/netbird', 'data-nav': true, text: 'NetBird' }), '.'));
  }
  if (p.id === 'zrok') keyParts.push(group('key', 'Account', p.managed ? 'The token is kept in Flats’ zrok environment. Never shown again.' : '', keysFailed ? keyError() : zrokSection(p, keyOf('zrok'), after)));
  parts.push(...keyParts);
  if (p.id === 'portal') parts.push(portalForm(data, saved, reload, page.notice, page));
  if (p.id === 'local') parts.push(group('address', 'Address', '', list(row({ title: 'Local address', desc: 'Every flat is reachable here from this device.', ctl: p.address ? h('code', { text: p.address }) : null }))));
  if (p.id === 'tailscale-funnel') parts.push(callout('info', 'Uses the Tailscale nodes. Your tailnet policy must allow Funnel and HTTPS certificates for them.'));
  if (p.id === 'cloudflare-quick') parts.push(callout('info', 'Cloudflare offers quick tunnels for testing and development: no uptime guarantee, up to 200 concurrent requests per flat and no server-sent events. Each flat gets a random address. Flats keeps it across restarts while Cloudflare keeps the tunnel; if Cloudflare removes the tunnel, the flat gets a new address.'));
  if (p.enabled && p.status) parts.push(statusGroup(p));
  if (p.id !== 'local') parts.push(allowedGroup(p));
  if (p.id === 'tailscale') parts.push(callout('info', 'Console addresses on Tailscale are managed under ', h('a', { href: '/settings/console', 'data-nav': true, text: 'Console' }), ', next to the other places the console opens.'));
  if (p.id === 'netbird' && !p.unsupported) parts.push(callout('warn', h('strong', { text: 'Plain HTTP. ' }), 'Traffic between peers is encrypted by WireGuard, but browsers treat the page as not secure: clipboard writes, crypto.subtle, service workers and Secure cookies don’t work there.'));
  fill(page.content, parts);
  const pendingKeys = keys.some((k) => k.waiting);
  const pendingHosts = p.enabled && p.status && (p.status.hosts || []).some((x) => x.state === 'starting');
  page.refresh(pendingKeys || pendingHosts);
}

// providerToggle is the Turn on / Turn off button and, when blocked, why.
function providerToggle(p, info, etag, after) {
  if (p.id === 'local') return null;
  // zrok can be turned on only once an account is connected. A build
  // without NetBird cannot turn it on.
  if (!p.enabled && p.unsupported) return null;
  const button = h('button', { type: 'button', class: 'btn btn-small' + (p.enabled ? ' btn-danger-text' : ' btn-primary'), text: p.enabled ? 'Turn off' : 'Turn on',
    'aria-label': `${p.enabled ? 'Turn off' : 'Turn on'} ${info.label}` });
  let reason = p.locked || '';
  if (!p.enabled && p.id === 'zrok' && !p.connected) reason = 'Connect a zrok account below first.';
  if (reason) {
    button.disabled = true;
    button.setAttribute('disabled', '');
    return { button, reason };
  }
  button.addEventListener('click', () => busy(button, async () => {
    if (await setHostProvider(p, info, !p.enabled, etag)) await after();
  }));
  return { button, reason: '' };
}

async function setHostProvider(p, info, enabled, etag) {
  const ok = await confirmDialog({
    title: `${enabled ? 'Turn on' : 'Turn off'} ${info.label}?`,
    body: enabled ? [
      h('p', { text: info.what }),
      p.scope === 'public' ? h('p', { class: 'alert alert-warn', text: 'A flat made public through this provider can be opened by anyone on the internet. Making a flat public still needs your approval.' }) : null,
      h('p', { class: 'muted', text: 'Saved to config.json and applied now. Nothing is served until you allow it on a flat.' }),
    ] : [h('p', { text: `${info.label} stops being available. Flats that allow it must stop allowing it first. If it is the default for new flats, the default is cleared.` })],
    confirmLabel: enabled ? 'Turn on' : 'Turn off',
    danger: !enabled,
  });
  if (!ok) return false;
  try {
    await api.setProvider(p.id, enabled, etag);
    toast(`${info.label} is ${enabled ? 'on' : 'off'}.`, 'success');
    return true;
  } catch (err) {
    await infoDialog(`Cannot turn ${enabled ? 'on' : 'off'} ${info.label}`, errorPanel(err));
    return false;
  }
}

// netbirdSteps replaces the switch while NetBird has no setup key: add a
// key, then turn it on.
function netbirdSteps(p, key, etag, after) {
  const form = keyForm(NETBIRD_KEY, false, after, 'netbird-key-steps-help');
  const toggle = providerToggle(p, infoOf('netbird'), etag, after);
  return group('setup', 'Set up', 'Two steps. Nothing is reachable until you allow NetBird on a flat.',
    h('div', { class: 's-list s-steps' },
      h('div', { class: 's-row s-step', id: 'step-key' }, h('span', { class: 's-num', 'aria-hidden': 'true', text: '1' }),
        h('div', { class: 's-row-main' },
          h('span', { class: 's-row-title', text: 'Add a setup key' }),
          h('p', { class: 's-row-desc', id: 'netbird-key-steps-help' }, NETBIRD_KEY.help),
          form)),
      h('div', { class: 's-row s-step' }, h('span', { class: 's-num', 'aria-hidden': 'true', text: '2' }),
        h('div', { class: 's-row-main' },
          h('span', { class: 's-row-title', text: 'Turn on NetBird' }),
          h('p', { class: 's-row-desc', text: 'Makes NetBird available to flats. Then allow it on each flat that should use it. A new peer cannot register until a setup key is saved.' })),
        toggle ? h('div', { class: 's-row-ctl' }, withReason(toggle.button, toggle.reason)) : null)),
    key && key.legacy_peer ? legacyPeerNotice(key.legacy_peer, after) : null);
}

// statusGroup lists the hosts a provider serves (core.NetStatus).
function statusGroup(p) {
  const n = p.status;
  const hosts = n.hosts || [];
  const noun = infoOf(p.id).noun;
  const title = p.id === 'tailscale' ? 'Nodes' : p.id === 'netbird' ? 'Peers' : 'Status';
  const attention = hosts.filter((x) => x.state !== 'ready').length;
  const hint = hosts.length ? [noun ? plural(hosts.length, noun) : plural(hosts.length, 'route'), attention ? `${attention} need${attention === 1 ? 's' : ''} attention` : ''].filter(Boolean).join(' · ') : '';
  const expiry = hosts.some((x) => x.key_expiry);
  const rows = hosts.map((x) => {
    // tsnet reports the login URL as the detail of a needs-login host.
    const login = x.login_url || (x.state === 'needs-login' && (x.detail || '').match(/https?:\/\/\S+/)?.[0]);
    return h('tr', null,
      h('th', { scope: 'row' }, h('code', { text: x.host }), x.ephemeral ? tag('Preview', 'solid') : null),
      h('td', null, statePill(x.state)),
      expiry ? h('td', { class: 'r' }, x.key_expiry ? h('span', { class: x.state === 'key-expiring' ? 'warn-text' : '' },
        h('time', { datetime: x.key_expiry, title: dateTime(x.key_expiry), text: relTime(x.key_expiry) })) : h('span', { class: 'muted', text: '—' })) : null,
      h('td', null,
        login ? h('div', null, extLink(login, 'Log in', 'btn btn-small btn-primary')) : null,
        x.url ? extLink(x.url) : null,
        x.detail && !login ? h('div', { class: 'muted small' }, linkify(x.detail)) : null));
  });
  return group('status', title, hint,
    n.detail ? h('p', { class: 'muted small' }, linkify(n.detail)) : null,
    hosts.length
      ? h('div', { class: 's-list s-table-wrap' }, h('table', { class: 's-table' },
        h('thead', null, h('tr', null, ['Name', 'Status', expiry ? 'Key expires' : null, 'Address'].filter(Boolean).map((t) => h('th', { scope: 'col', class: t === 'Key expires' ? 'r' : null, text: t })))),
        h('tbody', null, rows)))
      : list(row({ title: 'Nothing served yet', desc: 'Routes appear here once a flat that allows this network is running.' })));
}

// allowedGroup lists the flats that allow a provider.
function allowedGroup(p) {
  return group('allowed', 'Allowed on', 'Change this on each flat under Settings → Networks.',
    list(p.flats.length
      ? h('div', { class: 's-row stack' }, h('div', { class: 's-chips' },
        p.flats.map((slug) => h('a', { class: 's-chip', href: `/flats/${encodeURIComponent(slug)}`, 'data-nav': true, text: slug }))))
      : row({ title: 'Not allowed on any flat yet', desc: 'Turning a network on does not serve anything until a flat allows it.' })));
}

// --- keys ---

const KEY_SOURCES = { console: 'from the console', cli: 'with flats key set', flag: 'from a flats serve flag', env: 'from an environment variable',
  config: 'from config.json (moved to flats.db)', unknown: 'source unknown (connected before Flats recorded it)' };

// TAILSCALE_KEY and NETBIRD_KEY describe the keys Flats stores and uses for
// every new node or peer.
const TAILSCALE_KEY = {
  id: 'tailscale-key', name: 'Tailscale auth key', placeholder: 'tskey-auth-…',
  what: 'Logs in each new node: flats, previews and console addresses. Nodes already on the tailnet keep their identity.',
  help: 'In the Tailscale admin console, open Settings › Keys and generate a reusable auth key (tskey-auth-…). Leave it untagged so console decisions record who made them. Flats cannot tell from the key whether it is reusable and untagged; that is up to you. New nodes use it from now on; nodes already on the tailnet are not affected.',
  removeBody: 'New tailnet nodes then wait for an interactive login and show its link. Nodes already on the tailnet keep working.',
  set: (key) => api.setTailscaleKey(key), remove: () => api.deleteTailscaleKey(),
};
const NETBIRD_KEY = {
  id: 'netbird-key', name: 'NetBird setup key', placeholder: 'xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx',
  what: 'Registers each flat and NetBird console address as its own NetBird peer. Peers already registered keep their identity.',
  help: 'In the NetBird dashboard, open Setup Keys and create a key with “Allow extra DNS labels” on (required: each flat is reached by its own name). Make it reusable and ephemeral, with an auto-assigned group your access policies use, and without an expiry or usage limit.',
  removeBody: 'Running peers keep working, but a new flat, a new NetBird console address or a peer NetBird removed cannot register until you register a key again.',
  set: (key) => api.setNetbirdKey(key), remove: () => api.deleteNetbirdKey(),
};

// keyFacts is where a key came from and when, plus its non-secret identity.
function keyFacts(k) {
  const meta = [];
  if (k.updated_at) meta.push(h('span', null, 'Set ', h('time', { datetime: k.updated_at, title: dateTime(k.updated_at), text: relTime(k.updated_at) })));
  if (k.source) meta.push(KEY_SOURCES[k.source] || k.source);
  if (k.management_url) meta.push(h('span', null, 'Server ', h('code', { text: k.management_url })));
  if (k.peers) meta.push(plural(k.peers, 'peer') + ' running');
  return meta;
}

// keySection is a stored key's row (Replace, Remove) or the form to store one.
function keySection(k, cfg, after) {
  k = k || { set: false };
  if (k.unsupported) return callout('warn', k.unsupported);
  const notices = [
    k.external_differs ? callout('warn', 'The key given to this run by a flag or environment variable differs from the stored key. Flats uses the stored key; register a new one here to replace it.') : null,
    k.last_error ? callout('warn', `The last attempt to use this key failed: ${k.last_error}`) : null,
    k.legacy_peer ? legacyPeerNotice(k.legacy_peer, after) : null,
  ];
  if (!k.set) {
    return [list(
      row({ id: 'key-' + cfg.id, title: [cfg.name, k.waiting ? pill('Waiting for the next start', 'busy') : pill('Not set', 'off')], desc: cfg.what }),
      h('div', { class: 's-row stack' }, keyForm(cfg, false, after))), notices];
  }
  const formRow = h('div', { class: 's-row stack', hidden: true }, keyForm(cfg, true, after));
  const replace = h('button', { type: 'button', class: 'btn btn-small', text: 'Replace', 'aria-label': `Replace the ${cfg.name}`, 'aria-expanded': 'false' });
  replace.addEventListener('click', () => {
    const open = formRow.hidden;
    formRow.hidden = !open;
    if (open) formRow.removeAttribute('hidden'); else formRow.setAttribute('hidden', '');
    replace.setAttribute('aria-expanded', String(open));
    if (open) formRow.querySelector?.('input')?.focus();
  });
  const remove = h('button', { type: 'button', class: 'btn btn-small btn-danger-text', text: 'Remove', 'aria-label': `Remove the ${cfg.name}` });
  remove.addEventListener('click', () => busy(remove, async () => {
    const ok = await confirmDialog({
      title: `Remove the ${cfg.name}?`,
      body: [h('p', { text: cfg.removeBody }),
        k.external_given ? h('p', { class: 'alert alert-warn', text: 'This run got the key from a flats serve flag or environment variable. Remove that too, or the next start stores the key again.' }) : null],
      confirmLabel: 'Remove', danger: true,
    });
    if (!ok) return;
    try {
      await cfg.remove();
    } catch (err) {
      await infoDialog(`Cannot remove the ${cfg.name}`, errorPanel(err));
      return;
    }
    toast(`${cfg.name} removed.`, 'success');
    await after();
  }));
  return [list(
    row({ id: 'key-' + cfg.id, title: [cfg.name, pill('Stored', 'on')], meta: keyFacts(k), desc: cfg.what, ctl: [replace, remove] }),
    formRow), notices];
}

// keyForm sends a key once; the value is cleared and never shown again.
// helpId names guidance shown elsewhere (the NetBird setup steps);
// otherwise the form shows cfg.help itself.
function keyForm(cfg, replacing, after, helpId) {
  const input = h('input', { type: 'password', id: cfg.id, name: 'key', autocomplete: 'off', spellcheck: 'false', required: true,
    placeholder: cfg.placeholder, 'aria-label': replacing ? `New ${cfg.name}` : cfg.name, 'aria-describedby': helpId || cfg.id + '-help' });
  const submit = h('button', { type: 'submit', class: 'btn btn-primary btn-small', text: replacing ? 'Replace key' : 'Save key' });
  const form = h('form', { class: 's-inline-form', novalidate: true }, input, submit);
  if (!helpId) form.appendChild(h('p', { class: 's-row-desc s-form-note', id: cfg.id + '-help', text: cfg.help }));
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    const key = input.value.trim();
    if (!key) { input.focus(); return; }
    busy(submit, async () => {
      try {
        await cfg.set(key);
      } catch (err) {
        await infoDialog(`Cannot save the ${cfg.name}`, errorPanel(err));
        return;
      }
      input.value = '';
      toast(`${cfg.name} saved.`, 'success');
      await after();
    });
  });
  return form;
}

// legacyPeerNotice asks the operator to delete the shared NetBird peer an
// earlier version registered for the whole host.
function legacyPeerNotice(p, after) {
  const done = h('button', { type: 'button', class: 'btn btn-small', text: 'Done' });
  done.addEventListener('click', () => busy(done, async () => {
    try {
      await api.dismissNetbirdLegacyPeer();
    } catch (err) {
      await infoDialog('Cannot dismiss the notice', errorPanel(err));
      return;
    }
    await after();
  }));
  return h('div', { class: 'alert alert-warn', role: 'status' },
    h('p', null, 'Flats no longer uses the single NetBird peer ', h('code', { text: p.device_name || '(unnamed)' }),
      ' this host registered before: each flat now runs on its own peer. Its local identity was removed. Delete that peer in the NetBird dashboard (Peers), then choose Done.'),
    h('div', { class: 'form-actions' }, done));
}

// zrokSection shows the zrok account the provider uses. With
// zrok.environment set in config.json the account is managed outside Flats,
// with the zrok CLI.
function zrokSection(p, k, after) {
  const facts = [];
  if (p.environment_id) facts.push(h('span', null, 'Environment ', h('code', { text: p.environment_id })));
  if (p.api_endpoint) facts.push(h('span', null, 'Service ', h('code', { text: p.api_endpoint })));
  facts.push(h('span', null, 'Namespace ', h('code', { text: p.namespace || 'public' })));
  const k2 = k || { set: false };
  const notices = [
    k2.external_differs ? callout('warn', 'The token given to this run by a flag or environment variable differs from the connected account. Flats keeps the connected account; replace it here to change it.') : null,
    k2.last_error ? callout('warn', `The last attempt to use this token failed: ${k2.last_error}`) : null,
  ];
  const state = p.connected ? pill('Connected', 'on') : (k2.waiting ? pill('Waiting for the next start', 'busy') : pill('Not connected', 'off'));
  if (!p.managed) {
    return [list(row({ id: 'key-zrok', title: ['zrok account', state], meta: [...keyFacts(k2), ...facts],
      desc: 'Uses the zrok environment set as zrok.environment in config.json; manage its account with the zrok CLI. Each public flat reserves its slug as a name in this namespace.' })), notices];
  }
  if (!p.connected && !k2.set) {
    return [list(
      row({ id: 'key-zrok', title: ['zrok account token', state], meta: [...keyFacts(k2), ...facts], desc: 'Registers this machine as an environment of your zrok account. Turn zrok on once the account is connected.' }),
      h('div', { class: 's-row stack' }, zrokTokenForm(after, {
        submit: 'Connect',
        help: 'Sign in at myzrok.io, open the zrok API console and copy the account token from its getting-started wizard.',
        done: 'zrok account connected. Turn zrok on to use it.',
      }))), notices];
  }
  const formRow = h('div', { class: 's-row stack', hidden: true }, zrokTokenForm(after, {
    submit: 'Replace account',
    help: 'Connects the account of the new token in place of this one, for example after regenerating the token. The current account stays connected if the new token is refused. Refused while a flat is public through zrok.',
    done: 'zrok account replaced.',
    confirm: { title: 'Replace the zrok account?', body: 'Flats registers this machine with the account of the new token, then unregisters the current environment and deletes its token.' },
  }));
  const replace = h('button', { type: 'button', class: 'btn btn-small', text: 'Replace', 'aria-expanded': 'false' });
  replace.addEventListener('click', () => {
    const open = formRow.hidden;
    formRow.hidden = !open;
    if (open) formRow.removeAttribute('hidden'); else formRow.setAttribute('hidden', '');
    replace.setAttribute('aria-expanded', String(open));
  });
  return [list(
    row({ id: 'key-zrok', title: ['zrok account', state], meta: [...keyFacts(k2), ...facts],
      desc: 'Each public flat reserves its slug as a name in this namespace, so its URL stays the same.',
      ctl: [replace, zrokDisconnect(p, k2, after)] }),
    formRow), notices];
}

// zrokTokenForm sends an account token to connect or replace the account.
function zrokTokenForm(after, o) {
  const input = h('input', { type: 'password', id: 'zrok-token', name: 'token', autocomplete: 'off', spellcheck: 'false', required: true,
    placeholder: 'zrok account token', 'aria-label': 'zrok account token' });
  const submit = h('button', { type: 'submit', class: 'btn btn-primary btn-small', text: o.submit });
  const form = h('form', { class: 's-inline-form', novalidate: true }, input, submit, h('p', { class: 's-row-desc s-form-note', text: o.help }));
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    const token = input.value.trim();
    if (!token) { input.focus(); return; }
    busy(submit, async () => {
      if (o.confirm && !(await confirmDialog({ title: o.confirm.title, body: [h('p', { text: o.confirm.body })], confirmLabel: o.submit }))) return;
      try {
        await api.connectZrok(token);
      } catch (err) {
        await infoDialog('Cannot connect the zrok account', errorPanel(err));
        return;
      }
      input.value = '';
      toast(o.done, 'success');
      await after();
    });
  });
  return form;
}

function zrokDisconnect(p, k, after) {
  const disconnect = h('button', { type: 'button', class: 'btn btn-small btn-danger-text', text: 'Disconnect' });
  disconnect.addEventListener('click', () => busy(disconnect, async () => {
    const ok = await confirmDialog({
      title: 'Disconnect the zrok account?',
      body: [h('p', { text: 'Flats unregisters this machine from the zrok account and deletes the stored account token. Names this host reserved stay in the account; delete flats first to release them.' })],
      confirmLabel: 'Disconnect',
      danger: true,
    });
    if (!ok) return;
    try {
      await api.disconnectZrok(false);
    } catch (err) {
      // zrok may refuse the stored token (regenerated elsewhere): offer to
      // forget the account here, leaving its removal to the zrok console.
      const force = await confirmDialog({
        title: 'zrok did not unregister this environment',
        body: [errorPanel(err), h('p', { text: `Forget the account on this machine anyway? Environment ${k.environment_id || p.environment_id || ''} may stay registered in your zrok account; remove it in the zrok console.` })],
        confirmLabel: 'Forget it here',
        danger: true,
      });
      if (!force) return;
      try {
        await api.disconnectZrok(true);
      } catch (err2) {
        await infoDialog('Cannot disconnect the zrok account', errorPanel(err2));
        return;
      }
    }
    toast('zrok account disconnected.', 'success');
    await after();
  }));
  return withReason(disconnect, p.enabled ? 'zrok is on. Turn it off above before disconnecting its account.' : '');
}

// portalForm edits the Portal settings; they save with the page's save bar.
function portalForm(data, saved, reload, notice, page) {
  return stagedForm({
    id: 'portal', data, saved, reload, notice, page,
    groups: [['relays', 'Relays', 'Saved together with the save bar.', [
      { key: 'portal_relays', kind: 'relays', label: 'Relay list', restart: true, help: 'One per line. Leave empty to use Portal’s defaults (relay discovery, up to 3 active relays).' },
      { key: 'portal_discovery', kind: 'toggle', label: 'Discover more relays', restart: true, initial: String((data.settings || {}).portal_discovery ?? 'true') !== 'false',
        help: 'Find further relays automatically, beyond the list above.' },
      FIELDS.find((f) => f.key === 'portal_max_relays'),
      { key: 'portal_hide', kind: 'toggle', label: 'Hide public flats from relay listings', initial: String((data.settings || {}).portal_hide ?? 'false') === 'true',
        help: 'Keeps addresses out of the relays’ public lists. It is not access control: anyone with a URL can still open a flat.',
        meta: ['Applies within about 90 seconds', 'Each flat can override it'] },
    ]]],
  });
}

// --- console ---

// NETWORKS describes the networks console addresses run on.
const NETWORKS = {
  tailscale: {
    heading: 'Tailscale', noun: 'tailnet domain', node: 'tailnet node', suffix: '.<tailnet>.ts.net',
    what: 'Devices your tailnet ACL allows',
    added: 'Its node joins the tailnet in a minute or two.',
    removed: 'Deleted, but its tailnet node is not logged out yet. It stays reserved until that succeeds; Flats retries at the next start.',
    assigned: (d) => `The tailnet named this node ${d.assigned}: another machine already uses ${d.name}. Rename or remove that machine in the Tailscale admin console, or use this address.`,
  },
  netbird: {
    heading: 'NetBird', noun: 'NetBird domain', node: 'NetBird peer', suffix: '.<NetBird domain>',
    what: 'Peers your NetBird access policies allow · plain HTTP',
    added: 'Its peer joins NetBird in a minute.',
    removed: 'Deleted, but its NetBird peer is not stopped yet. It stays reserved until that succeeds; Flats retries at the next start.',
    assigned: (d) => `NetBird answers this peer as ${d.assigned} as well.`,
  },
};

// networkOf returns one network's part of the view.
function networkOf(view, prov) {
  return (view.providers && view.providers[prov]) || { domains: [], retiring: [], on: false };
}

// serving reports a node state that serves the console: ready, or ready with
// a Tailscale key in its last days (key-expiring), as the server counts it.
const serving = (state) => state === 'ready' || state === 'key-expiring';

// pendingWork reports a console node or an enrollment still in progress.
export function pendingWork(domains, keys) {
  const all = Object.keys(NETWORKS).flatMap((prov) => (domains ? networkOf(domains, prov).domains : []) || []);
  const nodes = all.some((d) => !d.conflict && !serving(d.state));
  return nodes || (keys || []).some((k) => k.waiting);
}

// readyDomains lists every ready domain, on any network, as provider/name.
function readyDomains(view) {
  return Object.keys(NETWORKS).flatMap((prov) => (networkOf(view, prov).domains || [])
    .filter((d) => !d.conflict && serving(d.state)).map((d) => prov + '/' + d.name));
}

// totalDomains counts the console domains of every network.
function totalDomains(view) {
  return Object.keys(NETWORKS).reduce((n, prov) => n + (networkOf(view, prov).domains || []).length, 0);
}

// deleteBlocker is why a console domain cannot be deleted from this page,
// or '' when it can. count is the number of console domains on every
// network, ready the ready ones as provider/name. The server checks the same
// rules.
export function deleteBlocker(d, view, count, ready, prov = 'tailscale') {
  if (count === 1 && !view.loopback) {
    return 'This is the console’s last domain. Add another one first, or delete it from the console on this machine’s loopback address.';
  }
  if (d.current && !ready.some((n) => n !== prov + '/' + d.name && n !== d.name)) {
    return 'You are using this domain and no other console domain is ready yet. Wait until another one shows ready, then delete this one.';
  }
  return '';
}

// preferredLabel names a console.preferred_host value for the operator.
function preferredLabel(value) {
  if (value === 'local') return 'this machine’s loopback address';
  const [prov, name] = value.split(':');
  return NETWORKS[prov] ? `${NETWORKS[prov].noun} ${name}` : value;
}

async function consolePage(page, saved, reload) {
  page.ctx.setTitle('Console · Settings');
  const d = await loadAll(page, ['domains', 'keys']);
  if (!d) return;
  const cfg = d.settings.config;
  const view = d.domains;
  const here = view && view.via ? h('span', null, 'You’re on ', h('code', { text: view.via.name }), ` (${(NETWORKS[view.via.provider] || { heading: view.via.provider }).heading}).`)
    : (view && view.loopback ? 'You’re on this machine’s loopback address.' : '');
  fill(page.content,
    pageHead({ title: 'Console', intro: ['Where this console opens and which address goes into the links Flats hands out. To move to a new address, add it, run ', h('code', { text: 'flats connect' }), ' on your agents, then delete the old one.'] }),
    view ? group('console-domains', 'Addresses', here, addressList(view, cfg, reload), retiringList(view, reload)) : group('console-domains', 'Addresses', '', errorPanel(d.domainsError, 'Cannot load the console addresses')),
    view && networkOf(view, 'netbird').domains.length ? callout('warn', h('strong', { text: 'NetBird addresses are plain HTTP. ' }), 'NetBird gives peers no certificates; traffic between peers is still encrypted by WireGuard.') : null,
    view ? preferredForm(view, d.settings, saved, reload, page) : null,
    externalOriginsForm(d.settings, saved, reload, page),
    group('configuration', 'Host', ['Set in config.json · change with ', h('code', { text: 'flats config set' }), ' while Flats is stopped; values apply after Flats restarts'], configView(cfg)));
  page.refresh(view && pendingWork(view, d.keys && d.keys.keys));
}

// External origins declare reverse-proxy addresses; they do not create a
// listener, configure a proxy or authenticate visitors.
function externalOriginsForm(data, saved, reload, page) {
  return stagedForm({
    id: 'external-origins', data, saved, reload, page, notice: page.notice, scopeSaved: true, inlineActions: true,
    confirmSave: async (out) => {
      const here = globalThis.location?.origin;
      const includesHere = (value) => (value || '').split(',').filter(Boolean).some((origin) => new URL(origin).origin === here);
      if (!here || !includesHere(data.settings?.external_origins) || includesHere(out.external_origins)) return true;
      const remaining = (out.external_origins || '').split(',').filter(Boolean);
      return confirmDialog({ title: 'Remove the address you’re using?', confirmLabel: 'Save and remove address', danger: true,
        body: [h('p', null, 'This console address, ', h('code', { text: here }), ', will stop working after saving.'),
          h('p', { text: remaining.length ? 'Other declared addresses remain, but Flats has not verified that they are reachable. Make sure you can open another console address before continuing.' : 'No external origins will remain. Make sure you can reach the local console before continuing.' }),
          h('p', null, 'You can also restore access with ', h('code', { text: 'flats config set console.external_origins' }), ' on the host while Flats is stopped.')],
      });
    },
    groups: [['reverse-proxy', 'Reverse proxy', 'Use your own domain for the console and agent connections.', [
      { key: 'external_origins', kind: 'origins', label: 'External origins',
        help: 'One address per line or separated by commas, including http:// or https:// and an optional port. Use lowercase domain names without a trailing slash or path. Leave empty to accept only existing console addresses.' },
    ]]],
    after: h('p', { class: 's-row-desc' }, 'Configure your proxy to preserve the Host header. Flats uses these addresses for links when accessed through the proxy. ',
      'Flats has no built-in login yet; protect remote access with proxy authentication or a private network.'),
  });
}

// Validate before saving, while retaining the exact declared scheme and port.
// The server remains authoritative for config validation.
function parseOrigins(text) {
  const values = text.split(/[\n,]/).map((s) => s.trim()).filter(Boolean);
  const authorities = new Set();
  for (let i = 0; i < values.length; i++) {
    const origin = values[i];
    const match = origin.match(/^https?:\/\/([a-z0-9.-]+)(?::([1-9][0-9]*))?$/);
    if (!match) return { error: `Address ${i + 1}: use an http:// or https:// origin with a lowercase domain and no path, query or fragment.` };
    const [, host, port] = match;
    const labels = host.split('.');
    if (host.length > 253 || labels.some((label) => label.length > 63 || !/^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/.test(label))) {
      return { error: `Address ${i + 1}: enter a valid lowercase domain name, not an IP address.` };
    }
    const last = labels.at(-1);
    if (host === 'localhost' || host.endsWith('.localhost') || /^(?:[0-9]+|0x[0-9a-f]+)$/.test(last) || !/[a-z]/.test(last)) {
      return { error: `Address ${i + 1}: use a non-local domain with a letter in its final label.` };
    }
    if (port && Number(port) > 65535) return { error: `Address ${i + 1}: use a port from 1 to 65535 without leading zeros.` };
    const authority = host + (port ? ':' + port : '');
    const defaultPort = origin.startsWith('https:') ? '443' : '80';
    const keys = !port || port === defaultPort ? [host, host + ':' + defaultPort] : [authority];
    if (keys.some((key) => authorities.has(key))) return { error: `Address ${i + 1}: this host and port are already listed. Use one scheme per address.` };
    keys.forEach((key) => authorities.add(key));
  }
  return { values };
}

// addressList is every place the console opens: the loopback address, then
// each network's domains, then a row to add one.
function addressList(view, cfg, reload) {
  const rows = [];
  if (cfg && cfg.host) {
    rows.push(row({ title: [h('code', { text: cfg.host.management_addr }), tag('Local', 'solid'), view.preferred === 'local' ? tag('Preferred') : null],
      meta: ['This device only', 'always available', view.loopback ? 'you are here' : null], ctl: pill('Ready', 'on') }));
  }
  for (const prov of Object.keys(NETWORKS)) {
    for (const dom of networkOf(view, prov).domains || []) rows.push(domainRow(dom, prov, view, reload));
  }
  rows.push(addRow(view, reload));
  return list(...rows);
}

function domainRow(d, prov, view, reload) {
  const net = NETWORKS[prov];
  const pinned = networkOf(view, prov).pinned;
  const login = d.state === 'needs-login' && (d.detail || '').match(/https?:\/\/\S+/)?.[0];
  const host = d.url ? d.url.replace(/^https?:\/\//, '').replace(/\/$/, '') : d.name;
  const state = d.conflict ? pill('Not served', 'err') : statePill(d.state);
  const del = h('button', { type: 'button', class: 'btn btn-small', text: 'Delete', 'aria-label': `Delete ${net.noun} ${d.name}` });
  del.addEventListener('click', () => busy(del, () => deleteDomain(d, prov, view, reload)));
  const notes = [
    d.conflict ? h('span', { class: 'warn-text', text: `Not served: ${d.conflict}. Delete this address or rename that flat.` }) : null,
    d.assigned && prov === 'tailscale' ? h('span', { class: 'warn-text', text: net.assigned(d) }) : null,
    d.detail && !login && !d.conflict ? h('span', null, linkify(d.detail)) : null,
    pinned ? `Set by ${pinned}. Reinstall the service with flats install to manage it here.` : null,
  ].filter(Boolean);
  return row({
    id: `domain-${prov}-${d.name}`,
    title: [d.url ? extLink(d.url, h('code', { text: host })) : h('code', { text: host }), tag(net.heading, 'solid'),
      view.preferred === `${prov}:${d.name}` ? tag('Preferred') : null],
    meta: [net.what, d.current ? 'you are here' : null, d.state === 'starting' ? 'joining, usually a minute or two' : null],
    desc: notes.length ? notes.map((n, i) => [i ? ' ' : '', n]) : null,
    ctl: [state, login ? extLink(login, 'Log in', 'btn btn-small btn-primary') : null,
      pinned ? null : withReason(del, deleteBlocker(d, view, totalDomains(view), readyDomains(view), prov))],
  });
}

// addRow adds a console address on a network that is on.
function addRow(view, reload) {
  const open = Object.keys(NETWORKS).filter((prov) => networkOf(view, prov).on && !networkOf(view, prov).pinned);
  if (!open.length) {
    return row({ title: 'Add an address', desc: ['Turn Tailscale or NetBird on under ', h('a', { href: '/settings/networks', 'data-nav': true, text: 'Networks' }), ' to add a console address there.'] });
  }
  const select = h('select', { id: 'console-domain-network', 'aria-label': 'Network' },
    Object.keys(NETWORKS).map((prov) => h('option', { value: prov, text: NETWORKS[prov].heading + (open.includes(prov) ? '' : ' (off)'), disabled: !open.includes(prov) })));
  select.value = open[0];
  const suffix = h('span', { class: 'unit', text: NETWORKS[open[0]].suffix });
  select.addEventListener('change', () => { suffix.textContent = NETWORKS[select.value].suffix; });
  const input = h('input', { type: 'text', id: 'console-domain-name', name: 'name', autocomplete: 'off', spellcheck: 'false', required: true,
    pattern: '[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?', placeholder: 'name' });
  const submit = h('button', { type: 'submit', class: 'btn btn-small', text: 'Add' });
  const form = h('form', { class: 's-add-form', novalidate: true },
    h('label', { for: 'console-domain-name', class: 's-row-title', text: 'Add an address' }),
    select, h('span', { class: 's-with-unit s-grow' }, input, suffix), submit);
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    const name = input.value.trim();
    const prov = select.value;
    if (!name) { input.focus(); return; }
    busy(submit, async () => {
      try {
        await api.addConsoleDomain(prov, name, view.etag);
      } catch (err) {
        await infoDialog(`Cannot add ${name}`, errorPanel(err));
        return;
      }
      input.value = '';
      toast(`${name} added. ${NETWORKS[prov].added}`, 'success');
      await reload();
    });
  });
  return h('div', { class: 's-row stack' }, form,
    h('p', { class: 's-row-desc', text: 'Lowercase letters, digits and hyphens. It can’t be a flat’s name. A new tailnet node logs in with the Tailscale auth key, or shows a login link here; a NetBird peer registers with the setup key.' }));
}

// retiringList shows deleted addresses whose node is not retired yet.
function retiringList(view, reload) {
  const rows = Object.keys(NETWORKS).flatMap((prov) => (networkOf(view, prov).retiring || []).map((d) => {
    const retry = h('button', { type: 'button', class: 'btn btn-small', text: 'Retry', 'aria-label': `Retry removing ${d.name}` });
    retry.addEventListener('click', () => busy(retry, async () => {
      try {
        await api.deleteConsoleDomain(prov, d.name, view.etag);
      } catch (err) {
        await infoDialog(`Cannot remove ${d.name}`, errorPanel(err));
        return;
      }
      toast(`${d.name} removed.`, 'success');
      await reload();
    }));
    return row({ title: [h('code', { text: d.name }), tag(NETWORKS[prov].heading, 'solid')], desc: NETWORKS[prov].removed, meta: d.error ? [d.error] : null, ctl: [pill('Being removed', 'busy'), retry] });
  }));
  if (!rows.length) return null;
  return h('div', { class: 's-sub' }, h('h4', { text: 'Being removed' }), list(...rows));
}

async function deleteDomain(d, prov, view, reload) {
  const net = NETWORKS[prov];
  const others = Object.keys(NETWORKS).flatMap((p) => (networkOf(view, p).domains || [])
    .filter((x) => !(p === prov && x.name === d.name)).map((x) => x.name));
  const body = [h('p', { text: `Flats removes the name from the ${net.noun}s and retires its ${net.node}. Agents connected through it must reconnect to another console address.` })];
  if (d.current) body.push(h('p', { class: 'alert alert-warn', text: `You are using this domain. The page moves to ${others[0] || 'another domain'}.` }));
  if (others.length) body.push(h('p', { class: 'muted small', text: `Remaining: ${others.join(', ')}.` }));
  if (view.pending_approvals) body.push(h('p', { class: 'muted small', text: `${plural(view.pending_approvals, 'approval')} waiting; links that name this domain stop working. Open them from Approvals on another domain.` }));
  const ok = await confirmDialog({ title: `Delete ${net.noun} ${d.name}?`, body, confirmLabel: 'Delete', danger: true });
  if (!ok) return;
  let res;
  try {
    res = await api.deleteConsoleDomain(prov, d.name, view.etag);
  } catch (err) {
    await infoDialog(`Cannot delete ${d.name}`, errorPanel(err));
    await reload();
    return;
  }
  toast(`${d.name} deleted.`, 'success');
  if (d.current && res && res.redirect) {
    window.location.href = res.redirect.replace(/\/$/, '') + '/settings/console';
    return;
  }
  await reload();
}

// preferredForm chooses where links go when they cannot name the console
// domain they were requested through: requests on the loopback address, or
// through a domain being deleted. The choice is the loopback address or a
// listed console domain, never a free address.
function preferredForm(view, data, saved, reload, page) {
  const current = view.preferred || '';
  const options = [['', 'Automatic'], ['local', 'Local']];
  for (const prov of Object.keys(NETWORKS)) {
    for (const d of networkOf(view, prov).domains || []) options.push([prov + ':' + d.name, `${d.name} (${NETWORKS[prov].heading})`]);
  }
  // A file edit can leave a name that is no longer listed: show it as chosen
  // so the warning below matches the control.
  if (current && !options.some(([v]) => v === current)) options.push([current, `${current} (not a console domain)`]);
  let warning = null;
  if (view.preferred_state === 'starting') {
    warning = `The preferred console, ${preferredLabel(current)}, is still starting, so links use ${view.link_url || 'the next served address'} until it is ready.`;
  } else if (view.preferred_state === 'not_served') {
    warning = `The preferred console, ${preferredLabel(current)}, is not served right now, so links use ${view.link_url || 'the next served address'} until it is.`;
  } else if (view.preferred_state === 'unlisted') {
    warning = `config.json prefers ${current}, which is not a console domain, so the preference is ignored. Choose a listed domain or Automatic.`;
  }
  return stagedForm({
    id: 'links', data, saved, reload, page, scopeSaved: true, inlineActions: true,
    save: async (out) => {
      await api.setPreferredConsole(out.preferred, view.etag);
      return { applied: ['preferred'], restart_required: [] };
    },
    errorTitle: 'Cannot change the preferred console',
    groups: [['console-links', 'Links', '', [
      { key: 'preferred', kind: 'select', label: 'Address for links', options, initial: current, inputId: 'console-preferred',
        help: 'Approval links and agent setup use the address a request came in on. When they can’t (a request over the loopback address, or through an address being deleted), they use this one. Automatic is the first served tailnet address, then NetBird, then loopback.',
        meta: [h('span', null, 'Links that can’t follow the request now go to ', view.link_url ? h('code', { text: view.link_url }) : 'the loopback address', '.')] },
    ]]],
    after: warning ? h('p', { class: 'alert alert-warn', role: 'status', text: warning }) : null,
  });
}

// configView shows the host keys of config.json, which the console only reads.
export function configView(cfg) {
  if (!cfg) return list(row({ title: 'In memory', desc: 'This host keeps its settings in memory, not in config.json.' }));
  const sources = cfg.sources || {};
  return [
    cfg.mode === 'legacy' ? h('p', { class: 'notice-text' },
      'This service starts without ', h('code', { text: '--config' }), '. Run ', h('code', { text: 'flats install' }),
      ' to switch it to config.json.') : null,
    list(...CONFIG_ROWS.map(([key, label, value, desc]) => {
      const v = String(value(cfg));
      const shown = v === 'on' || v === 'off' ? pill(v === 'on' ? 'On' : 'Off', v === 'on' ? 'on' : 'off') : h('code', { text: v });
      return row({ title: label, desc, ctl: [shown, sources[key] ? h('span', { class: 'tag tag-solid' }, h('span', { class: 'sr-only', text: 'source: ' }), SOURCE_TAGS[sources[key]] || sources[key]) : null] });
    })),
  ];
}

// --- limits ---

async function limitsPage(page, saved, reload) {
  page.ctx.setTitle('Limits · Settings');
  const d = await loadAll(page, []);
  if (!d) return;
  fill(page.content,
    pageHead({ title: 'Limits', intro: 'Defaults for every flat. A flat can override the ones marked “per flat” in its own settings. 0 means no limit.' }),
    stagedForm({
      id: 'limits', data: d.settings, saved, reload, notice: page.notice, page,
      groups: LIMIT_GROUPS.map(([gid, title, hint]) => ['limits-' + gid, title, hint, FIELDS.filter((f) => f.group === gid)]),
    }));
}

function display(base, factor) {
  const n = Number(base) / factor;
  return Number.isFinite(n) ? String(Math.round(n * 1000) / 1000) : '';
}

// Byte units, largest first. GB is 2^30 bytes, as before the unit menu.
const BYTE_UNITS = [['GB', 1 << 30], ['MB', 1 << 20], ['KB', 1 << 10], ['bytes', 1]];
const BYTE_FACTOR = Object.fromEntries(BYTE_UNITS);

// byteValue shows a byte count exactly, in the largest unit where it is a
// number with at most three decimals: 1.5 GB, but 107374 bytes rather than
// a rounded 0 GB. 0 and values it cannot read use the preferred unit.
export function byteValue(base, preferred) {
  const n = Number(base);
  if (base === '' || base == null || !Number.isSafeInteger(n) || n < 0) return { unit: preferred, value: '' };
  if (n === 0) return { unit: preferred, value: '0' };
  for (const [unit, factor] of BYTE_UNITS) {
    // n / factor is exact (factor is a power of two); a multiple of 1/8 has
    // at most three decimals.
    const v = n / factor;
    if (Number.isInteger(v * 8)) return { unit, value: String(v) };
  }
  return { unit: 'bytes', value: String(n) };
}

// byteText formats a byte count for hints, such as "30 GB".
function byteText(base, preferred) {
  const { unit, value } = byteValue(base, preferred);
  return value === '' ? '' : `${value} ${unit}`;
}

// toBase converts an entered decimal number in a unit (factor base units)
// to an integer of base units, rounding half up, or returns null for text
// that is not a number of 0 or more, is not whole when whole is set, or is
// too large to store exactly. It computes on the decimal digits, so no
// fraction is lost to floating point before the checks.
export function toBase(text, factor, whole) {
  const m = /^\s*\+?(\d*)(?:\.(\d*))?(?:[eE]([+-]?\d+))?\s*$/.exec(String(text ?? ''));
  if (!m || !(m[1] || m[2])) return null;
  const frac = m[2] || '';
  const scale = frac.length - Number(m[3] || 0); // value = digits / 10^scale
  if (Math.abs(scale) > 400) return null;
  let num = BigInt((m[1] || '') + frac || '0') * BigInt(factor);
  let den = 1n;
  if (scale > 0) den = 10n ** BigInt(scale);
  else num *= 10n ** BigInt(-scale);
  if (whole && num % den !== 0n) return null;
  const base = (num * 2n + den) / (2n * den);
  return base <= BigInt(Number.MAX_SAFE_INTEGER) ? Number(base) : null;
}

// Only destructive retention settings use isDecrease. Version quotas keep all existing versions.
export function isDecrease(key, current, next) {
  const cur = Number(current);
  const n = Number(next);
  return n < cur;
}

// --- staged forms and the save bar ---

// stagedForm builds a page form whose fields stage changes; one save bar
// shows the count and a summary, blocks saving while a field is invalid and
// asks again before a save that removes data. spec: id, data (the settings
// response), groups [[id, title, hint, fields]], saved (the last save
// response), reload(saved), and optionally save(out) for a custom endpoint,
// errorTitle and after (a node under the groups).
function stagedForm(spec) {
  const data = spec.data || {};
  const settings = data.settings || {};
  const defaults = data.defaults || {};
  const cfg = data.config || null;
  const etag = cfg ? cfg.etag : '';
  const sources = (cfg && cfg.sources) || {};
  const cfgKeys = (cfg && cfg.keys) || {};
  const pinned = (cfg && cfg.pinned) || {};
  const custom = !!spec.save;
  const controls = {}; // key -> { f, input, unit, error, describedBy, row, read(), reset() }

  // A Console save reloads both forms; only the form owning a saved field
  // should announce it, leaving the other form's staged changes separate.
  const ownedKeys = new Set(spec.groups.flatMap(([, , , fields]) => fields.map((f) => f.key)));
  const applied = (spec.saved?.applied || []).filter((key) => ownedKeys.has(key));
  const later = (spec.saved?.restart_required || []).filter((key) => ownedKeys.has(key));
  const saved = applied.length || later.length ? { applied, restart_required: later } : null;
  const result = h('div', { class: 's-result', role: 'status' }, savedSummary(spec.scopeSaved ? saved : spec.saved));
  const errorSlot = h('div');

  // wire adds the hint, source, pin reason and error to a field.
  const wire = (f, input, id) => {
    const src = custom ? '' : sources[cfgKeys[f.key]];
    const def = custom || defaults[f.key] === undefined || f.kind ? ''
      : ` Default: ${f.bytes ? byteText(defaults[f.key], f.unit) : `${display(defaults[f.key], f.factor)} ${f.unit}`}.`;
    const hint = [f.help + def, src && src !== 'default' ? `Set in ${SOURCES[src] || src}.` : ''].filter(Boolean).join(' ');
    const describedBy = [id + '-help'];
    const pin = pinned[f.key] ? h('span', { class: 's-pin', id: id + '-pin', text: `Set by ${pinned[f.key]}. Reinstall the service with \`flats install\` to change it here.` }) : null;
    if (pin) {
      input.setAttribute('disabled', '');
      input.disabled = true;
      describedBy.push(id + '-pin');
    }
    const error = h('span', { class: 'field-error', id: id + '-error', hidden: true });
    input.setAttribute('aria-describedby', describedBy.join(' '));
    return { hint: h('p', { class: 's-row-desc', id: id + '-help', text: hint }), pin, error, describedBy };
  };

  const field = (f) => {
    const kind = f.kind || (f.bytes ? 'bytes' : 'number');
    const id = f.inputId || 'set-' + f.key;
    let input;
    let unit = null;
    let exact = null;
    let update = null;
    let ctl;
    let read; // returns { value } for a changed value, { error } or null when unchanged
    let reset;
    if (kind === 'number' || kind === 'bytes') {
      const shown = kind === 'bytes' ? byteValue(settings[f.key], f.unit) : { unit: f.unit, value: display(settings[f.key], f.factor) };
      input = h('input', { id, type: 'number', inputmode: 'decimal', min: String(f.min ?? 0), step: f.step ? String(f.step) : 'any', value: shown.value, required: true });
      if (kind === 'bytes') {
        // The unit menu changes what the number means; the hint below it
        // always shows the exact byte count that saving stores.
        unit = h('select', { id: id + '-unit', class: 'unit', 'aria-label': `${f.label} unit` }, BYTE_UNITS.map(([u]) => h('option', { value: u, text: u })));
        unit.value = shown.unit;
        exact = h('span', { class: 's-row-meta', id: id + '-exact', 'aria-live': 'polite' });
        update = () => {
          const base = toBase(input.value, BYTE_FACTOR[unit.value], unit.value === 'bytes');
          exact.textContent = base == null ? '' : base === 0 ? 'Stores 0 bytes.' : `Stores ${base.toLocaleString('en-US')} ${base === 1 ? 'byte' : 'bytes'}.`;
        };
        input.addEventListener('input', update);
        unit.addEventListener('change', update);
        update();
        if (pinned[f.key]) unit.disabled = true;
      } else {
        unit = h('span', { class: 'unit', text: f.unit });
      }
      ctl = h('span', { class: 's-with-unit' }, input, unit);
      read = () => {
        if (input.value === shown.value && (kind !== 'bytes' || unit.value === shown.unit)) return null;
        const inBytes = kind === 'bytes' && unit.value === 'bytes';
        const whole = f.whole || inBytes;
        const base = toBase(input.value, kind === 'bytes' ? BYTE_FACTOR[unit.value] : f.factor, whole);
        if (base == null) return { error: `${f.label} must be ${whole ? 'a whole number' : 'a number'} of 0 or more.` };
        // Compare stored values, not text, so "0.0" over 0 changes nothing.
        if (String(base) === String(settings[f.key])) return null;
        return { value: String(base), from: describeValue(f, settings[f.key]), to: describeValue(f, base) };
      };
      reset = () => { input.value = shown.value; if (kind === 'bytes') { unit.value = shown.unit; update(); } };
    } else if (kind === 'origins') {
      const initial = (settings[f.key] || '').split(',').filter(Boolean).join('\n');
      input = h('textarea', { id, rows: '3', spellcheck: 'false', autocapitalize: 'none', autocomplete: 'off', placeholder: 'https://flats.example.org' });
      input.value = initial;
      ctl = null;
      read = () => {
        if (input.value === initial) return null;
        const parsed = parseOrigins(input.value);
        if (parsed.error) return parsed;
        const v = parsed.values;
        if (v.join('\n') === initial) return null;
        return { value: v.join(','), from: initial ? plural(initial.split('\n').length, 'origin') : 'none', to: v.length ? plural(v.length, 'origin') : 'none' };
      };
      reset = () => { input.value = initial; };
    } else if (kind === 'relays') {
      const initial = (settings.portal_relays || '').split(',').map((s) => s.trim()).filter(Boolean).join('\n');
      input = h('textarea', { id, rows: '3', spellcheck: 'false', class: 'mono', placeholder: 'https://relay.example.com' });
      input.value = initial;
      ctl = null;
      read = () => {
        const v = input.value.split(/[\n,]/).map((s) => s.trim()).filter(Boolean);
        if (v.join('\n') === initial) return null;
        return { value: v.join(','), from: initial ? plural(initial.split('\n').length, 'relay') : 'defaults', to: v.length ? plural(v.length, 'relay') : 'defaults' };
      };
      reset = () => { input.value = initial; };
    } else if (kind === 'toggle') {
      input = h('input', { id, type: 'checkbox', role: 'switch', class: 'switch', checked: !!f.initial });
      input.checked = !!f.initial;
      ctl = input;
      read = () => (!!input.checked === !!f.initial ? null : { value: String(!!input.checked), from: f.initial ? 'On' : 'Off', to: input.checked ? 'On' : 'Off' });
      reset = () => { input.checked = !!f.initial; };
    } else {
      input = h('select', { id }, f.options.map(([value, text]) => h('option', { value, text, selected: value === f.initial })));
      input.value = f.initial;
      ctl = input;
      const textOf = (v) => (f.options.find(([o]) => o === v) || [v, v])[1];
      read = () => (input.value === f.initial ? null : { value: input.value, from: textOf(f.initial), to: textOf(input.value) });
      reset = () => { input.value = f.initial; };
    }
    const w = wire(f, input, id);
    if (exact) w.describedBy.push(exact.getAttribute('id'));
    input.setAttribute('aria-describedby', w.describedBy.join(' '));
    const tags = [f.perFlat ? tag('per flat') : null, f.restart ? tag('Applies after restart', 'restart') : null];
    const r = h('div', { class: 's-row' + (['relays', 'origins'].includes(kind) ? ' stack' : ''), id: 'row-' + f.key },
      h('div', { class: 's-row-main' },
        h('div', { class: 's-row-title' }, h('label', { for: id, text: f.label }), tags),
        w.hint, exact, f.meta ? h('span', { class: 's-row-meta' }, f.meta.map((m, i) => [i ? h('span', { class: 'sep', 'aria-hidden': 'true', text: '·' }) : null, typeof m === 'string' ? h('span', { text: m }) : m])) : null,
        w.pin, w.error),
      ['relays', 'origins'].includes(kind) ? input : h('div', { class: 's-row-ctl' }, ctl));
    controls[f.key] = { f, input, unit, error: w.error, describedBy: w.describedBy, row: r, read, reset,
      // raw and setRaw carry a staged value across a page reload.
      raw: () => (kind === 'toggle' ? { checked: !!input.checked } : { value: input.value, unit: kind === 'bytes' ? unit.value : undefined }),
      setRaw: (v) => {
        if (kind === 'toggle') { input.checked = v.checked; return; }
        // A choice that no longer exists (a deleted address) is dropped and
        // the freshly loaded value stays.
        if (kind === 'select' && !hasOption(input, v.value)) return;
        input.value = v.value;
        if (kind === 'bytes' && v.unit) { unit.value = v.unit; update(); }
      } };
    return r;
  };

  const groups = spec.groups.map(([gid, title, hint, fields]) => group(gid, title, hint, list(...fields.map(field))));

  // The save bar: count, summary, Discard and Save; the impact preview of a
  // save that removes data replaces the summary.
  const count = h('strong');
  const summary = h('span', { class: 's-summary' });
  const impactSlot = h('div');
  const discard = h('button', { type: 'button', class: 'btn btn-small btn-ghost', text: 'Discard' });
  const save = h('button', { type: 'submit', id: spec.id + '-save', class: 'btn btn-primary btn-small', text: 'Save changes' });
  const barRow = h('div', { class: 'savebar-row' }, h('div', { class: 'savebar-msg', role: spec.inlineActions ? 'status' : null }, count, summary), h('div', { class: 'savebar-acts' }, discard, save));
  const bar = h('div', { class: spec.inlineActions ? 's-inline-actions' : 'savebar', role: 'region', 'aria-label': spec.inlineActions ? spec.id === 'links' ? 'Links changes' : 'External origins changes' : 'Unsaved changes', hidden: !spec.inlineActions }, impactSlot, barRow);

  const form = h('form', { class: 'settings-form s-form', id: spec.id + '-form', novalidate: true }, result, groups, spec.after || null, errorSlot, bar);

  const setError = (key, message) => {
    const c = controls[key];
    if (!c) return;
    c.error.textContent = message;
    c.error.hidden = false;
    c.error.removeAttribute('hidden');
    c.input.setAttribute('aria-invalid', 'true');
    c.input.setAttribute('aria-describedby', [...c.describedBy, c.error.getAttribute('id')].join(' '));
    c.row.classList?.add('invalid');
  };
  const clearError = (c) => {
    c.error.textContent = '';
    c.error.hidden = true;
    c.error.setAttribute('hidden', '');
    c.input.removeAttribute('aria-invalid');
    c.input.setAttribute('aria-describedby', c.describedBy.join(' '));
    c.row.classList?.remove('invalid');
  };
  const clearErrors = () => {
    fill(errorSlot);
    for (const c of Object.values(controls)) clearError(c);
  };

  // collect reads every staged field: out maps keys to new values, invalid
  // lists the fields that cannot be saved, notes describe each change.
  const collect = () => {
    const out = {};
    const invalid = [];
    const notes = [];
    for (const [key, c] of Object.entries(controls)) {
      if (pinned[key]) continue;
      const r = c.read();
      if (!r) continue;
      if (r.error) { invalid.push([key, r.error]); continue; }
      out[key] = r.value;
      notes.push(`${LABELS[key] || key}: ${r.from} → ${r.to}`);
    }
    return { out, invalid, notes };
  };

  // refreshBar marks staged and invalid rows and updates the save bar.
  // keep leaves errors the server reported in place.
  const refreshBar = (keep) => {
    const { out, invalid, notes } = collect();
    for (const [key, c] of Object.entries(controls)) {
      const r = pinned[key] ? null : c.read();
      if (r) c.row.classList?.add('changed'); else c.row.classList?.remove('changed');
      // A field the operator fixed loses its error; one still invalid shows
      // it while typing.
      if (r && r.error) setError(key, r.error);
      else if (!keep) clearError(c);
    }
    const n = Object.keys(out).length + invalid.length;
    form.setAttribute('data-dirty', String(n > 0));
    count.textContent = n ? plural(n, 'unsaved change') : spec.inlineActions ? 'No unsaved changes' : '';
    if (invalid.length) {
      summary.className = 's-summary s-summary-err';
      summary.textContent = `Fix ${invalid.map(([k]) => LABELS[k] || k).join(', ')} before saving.`;
    } else {
      summary.className = 's-summary';
      summary.textContent = notes.length <= 2 ? notes.join(' · ') : `${notes.slice(0, 2).join(' · ')} and ${notes.length - 2} more`;
    }
    save.disabled = invalid.length > 0 || (!!spec.inlineActions && n === 0);
    discard.disabled = !!spec.inlineActions && n === 0;
    // While a save waits for its confirmation, the confirmation is the
    // only way to save.
    const confirming = !!impactSlot.firstChild;
    barRow.hidden = confirming;
    if (confirming) barRow.setAttribute('hidden', ''); else barRow.removeAttribute('hidden');
    const show = spec.inlineActions || n > 0 || confirming;
    bar.hidden = !show;
    if (show) bar.removeAttribute('hidden'); else bar.setAttribute('hidden', '');
    return { out, invalid };
  };

  const showError = (err) => {
    // Mark the field the server names, by setting or config.json key.
    const msg = err.message || String(err);
    const key = Object.keys(controls).find((k) => msg.includes(k) || (cfgKeys[k] && msg.includes(cfgKeys[k])));
    if (key) setError(key, msg);
    fill(errorSlot, h('div', { class: 'alert alert-error', role: 'alert' },
      h('strong', { text: spec.errorTitle || 'Settings not saved' }), h('p', { text: msg }),
      h('p', { class: 'small', text: 'Your changes are still in the form.' })));
    if (key) controls[key].input.focus();
  };

  // changes returns the edited settings, or null after marking an invalid
  // field.
  const changes = () => {
    const { out, invalid } = collect();
    if (invalid.length) {
      for (const [key, msg] of invalid) setError(key, msg);
      controls[invalid[0][0]].input.focus();
      return null;
    }
    return out;
  };

  const store = async (out) => {
    try {
      const res = custom ? await spec.save(out) : await api.saveSettings(out, etag);
      toast('Settings saved.', 'success');
      await spec.reload(res);
    } catch (err) {
      fill(result);
      showError(err);
      // The file may have changed on disk; show that without discarding input.
      if (!custom && spec.notice && (err.status === 412 || err.status === 409)) {
        api.settings().then((d) => fill(spec.notice, changedNotice(d.config))).catch(() => {});
      }
      refreshBar(true);
    }
  };

  // Any edit invalidates a shown or pending impact; saving checks it again.
  // generation counts edits and submits, so a late impact response for
  // older values is dropped.
  let generation = 0;
  const edited = () => { generation++; fill(impactSlot); refreshBar(false); };
  form.addEventListener('input', edited);
  form.addEventListener('change', edited);
  discard.addEventListener('click', () => {
    for (const c of Object.values(controls)) c.reset();
    generation++;
    fill(impactSlot);
    clearErrors();
    refreshBar(false);
  });
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    const gen = ++generation;
    clearErrors();
    fill(impactSlot);
    const out = changes();
    if (!out) { refreshBar(true); return; }
    if (!Object.keys(out).length) { fill(result, h('p', { class: 'muted', text: 'Nothing changed.' })); refreshBar(false); return; }
    busy(save, async () => {
      if (spec.confirmSave) {
        const confirmed = await spec.confirmSave(out);
        if (!confirmed) return;
        const now = changes();
        if (gen !== generation || !now || !sameChanges(now, out)) {
          fill(errorSlot, h('p', { class: 'alert alert-warn', role: 'status', text: 'The form changed while confirming. Review your changes and save again.' }));
          refreshBar(false);
          return;
        }
      }
      const lower = RETENTION.filter((k) => k in out && isDecrease(k, settings[k], out[k]));
      if ('revision_quota' in out && revisionsDrop(settings.revision_quota, out.revision_quota)) lower.push('revision_quota');
      if (!lower.length) return store(out);
      let impact = {};
      if (lower.some((k) => RETENTION.includes(k))) {
        try {
          impact = (await api.settingsImpact(out)).impact || {};
        } catch (err) {
          if (gen === generation) showError(err);
          return;
        }
        if (gen !== generation) return;
      }
      if (lower.includes('revision_quota')) {
        impact = { ...impact, revision_quota: { current: String(settings.revision_quota ?? '0'), candidate: out.revision_quota, revisions: true } };
      }
      if (!Object.keys(impact).length) return store(out);
      // The confirmation saves only the values it described.
      const confirm = () => {
        const now = changes();
        if (gen !== generation || !now || !sameChanges(now, out)) {
          fill(impactSlot);
          if (now) fill(errorSlot, h('div', { class: 'alert alert-warn', role: 'alert' },
            h('p', { text: 'The form changed after the preview. Save again to check the new values.' })));
          refreshBar(false);
          return undefined;
        }
        return store(out);
      };
      fill(impactSlot, impactPanel(impact, confirm, () => {
        fill(impactSlot);
        refreshBar(false);
        controls[lower[0]].input.focus();
      }));
      refreshBar(false);
    }).finally(() => { if (spec.inlineActions) refreshBar(true); });
  });
  refreshBar(false);
  if (spec.page) {
    spec.page.collect.push({
      snapshot: () => Object.fromEntries(Object.entries(controls).filter(([key, c]) => !pinned[key] && c.read()).map(([key, c]) => [key, c.raw()])),
      restore: (staged) => {
        for (const [key, v] of Object.entries(staged)) {
          const c = controls[key];
          if (c && !pinned[key] && !c.input.disabled) c.setRaw(v);
        }
        refreshBar(false);
      },
    });
  }
  return form;
}

// hasOption reports whether a select offers value.
function hasOption(select, value) {
  const options = select.options ? [...select.options] : (select.childNodes || []);
  return options.some((o) => o.value === value);
}

// describeValue shows a stored value in a field's unit for the save bar.
function describeValue(f, base) {
  if (f.bytes) return byteText(base, f.unit) || String(base);
  return `${display(base, f.factor)} ${f.unit}`;
}

function sameChanges(a, b) {
  const ka = Object.keys(a);
  return ka.length === Object.keys(b).length && ka.every((k) => k in b && a[k] === b[k]);
}

// savedSummary lists what the last save changed: in effect now, or after
// a restart.
function savedSummary(res) {
  if (!res) return null;
  const applied = res.applied || [];
  const later = res.restart_required || [];
  if (!applied.length && !later.length) return h('p', { class: 'muted', text: 'Saved. No value changed.' });
  return callout('ok', h('ul', { class: 'compact saved-list' },
    applied.map((k) => h('li', { text: `${LABELS[k] || k}: saved, applied now.` })),
    later.map((k) => h('li', { text: `${LABELS[k] || k}: saved, applies after Flats restarts.` }))));
}

const IMPACT_TEXT = {
  events_keep: (n) => `The next cleanup deletes ${plural(n, 'log event')}.`,
  preview_ttl_seconds: (n) => `The next cleanup closes ${plural(n, 'preview')}.`,
};

function impactItem(key, f) {
  const shown = (f.versions || f.previews || []).length;
  const more = f.count > shown && shown ? ` and ${f.count - shown} more` : '';
  if (key === 'preview_ttl_seconds') return `${f.slug}: ${f.previews.join(', ')}${more}`;
  return `${f.slug}: ${plural(f.count, 'event')}`;
}

// impactPanel shows what lower retention values remove and asks for a
// second confirmation before saving.
export function impactPanel(impact, onConfirm, onCancel) {
  const titleId = 'impact-title';
  const field = Object.fromEntries(FIELDS.map((f) => [f.key, f]));
  const removes = Object.values(impact).some((r) => r.total > 0 || r.revisions);
  const heading = h('h3', { id: titleId, tabindex: '-1', text: removes ? 'Saving removes data' : 'Check the lower retention values' });
  const confirm = h('button', { type: 'button', class: removes ? 'btn btn-small btn-danger' : 'btn btn-small btn-primary', text: removes ? 'Save and remove' : 'Save' });
  const cancel = h('button', { type: 'button', class: 'btn btn-small btn-ghost', text: 'Keep editing' });
  confirm.addEventListener('click', () => busy(confirm, onConfirm));
  cancel.addEventListener('click', onCancel);
  const panel = h('section', { class: 'impact', role: 'group', 'aria-labelledby': titleId },
    heading,
    RETENTION.filter((k) => impact[k]).map((k) => {
      const r = impact[k];
      const f = field[k];
      const from = display(r.current, f.factor);
      const to = display(r.candidate, f.factor);
      return h('div', null,
        h('p', null, h('strong', { text: `${f.label}: ${from} → ${to} ${f.unit}.` }), ' ',
          r.total > 0 ? IMPACT_TEXT[k](r.total) : 'Nothing is removed now.'),
        r.flats && r.flats.length ? h('ul', { class: 'compact' },
          r.flats.map((x) => h('li', { text: impactItem(k, x) })),
          r.more ? h('li', { class: 'muted', text: `and ${plural(r.more, 'more flat')}` }) : null) : null);
    }),
    impact.revision_quota ? h('div', null, h('p', null,
      h('strong', { text: `Revisions: ${impact.revision_quota.current === '0' ? 'all' : impact.revision_quota.current} → ${impact.revision_quota.candidate} revisions.` }), ' ',
      'Every document, design and board that uses this default deletes its older unpinned revisions beyond the new limit at once. Pinned revisions and flats with their own limit are kept. The number deleted cannot be shown in advance.')) : null,
    h('div', { class: 'actions' }, cancel, confirm));
  queueMicrotask(() => heading.focus());
  return panel;
}

// --- agents ---

function agentsPage(page) {
  if (page.token) page.token.drawn = true;
  page.ctx.setTitle('Agents · Settings');
  const url = location.origin + '/mcp';
  const snippets = [
    ['Claude Code', `claude mcp add --transport http flats ${url}`],
    ['Codex', `# ~/.codex/config.toml\n[mcp_servers.flats]\nurl = "${url}"`],
    ['Cursor', `// ~/.cursor/mcp.json or .cursor/mcp.json\n${JSON.stringify({ mcpServers: { flats: { url } } }, null, 2)}`],
    ['CLI', 'flats save-draft <slug> ./dist\nflats create-version <slug> --revision <revision> --hash <hash>\nflats deploy --flat <slug> --version <version>'],
  ];
  const code = h('pre', { class: 's-code', tabindex: '0' }, h('code'));
  const panel = h('div', { class: 's-tabpanel', role: 'tabpanel', id: 'agent-snippet' }, code, copyButton(() => code.textContent, 'snippet'));
  const tabs = snippets.map(([label, text], i) => {
    const t = h('button', { type: 'button', role: 'tab', class: 's-tab', id: 'agent-tab-' + i, 'aria-controls': 'agent-snippet', 'aria-selected': 'false', text: label });
    t.addEventListener('click', () => select(i));
    return t;
  });
  const note = h('p', { class: 's-row-desc' });
  function select(i) {
    tabs.forEach((t, j) => { t.setAttribute('aria-selected', String(i === j)); t.setAttribute('tabindex', i === j ? '0' : '-1'); });
    code.firstChild.textContent = snippets[i][1];
    panel.setAttribute('aria-labelledby', 'agent-tab-' + i);
    note.textContent = snippets[i][0] === 'CLI'
      ? 'Save a Private Draft, request a numbered version, then deploy that version. Version creation and deployment each need their own approval.'
      : 'Agents can create flats, save Drafts, request numbered versions and deploy them, open previews and read logs.';
  }
  select(0);
  const tablist = h('div', { class: 's-tabs', role: 'tablist', 'aria-label': 'Client' }, tabs);
  tablist.addEventListener('keydown', (e) => {
    const at = tabs.findIndex((t) => t.getAttribute('aria-selected') === 'true');
    const next = { ArrowRight: at + 1, ArrowLeft: at - 1, Home: 0, End: tabs.length - 1 }[e.key];
    if (next === undefined) return;
    e.preventDefault();
    const i = (next + tabs.length) % tabs.length;
    select(i);
    tabs[i].focus();
  });
  // The snippets for every client stay in the page for search and copy.
  const all = h('div', { class: 'sr-only' }, snippets.map(([label, text]) => h('pre', null, `${label}: ${text}`)));
  fill(page.content,
    pageHead({ title: 'Agents', intro: ['Agents can create flats, save Drafts, open previews and read logs. Saving versions, deploying, visibility changes and deleting wait for your approval under ', h('a', { href: '/approvals', 'data-nav': true, text: 'Approvals' }), '.'] }),
    group('connect', 'Connect an agent', 'MCP over Streamable HTTP', list(
      row({ title: 'MCP address', meta: ['Uses the address you’re on now'], ctl: [h('code', { text: url }), copyButton(() => url, 'MCP address')] }),
      h('div', { class: 's-row stack' }, tablist, panel, note))),
    group('agent-limits', 'What the agent tools can’t do', 'MCP tools and the agent API', list(
      row({ title: 'Read or set keys', desc: ['No agent tool reads or sets a network key. Keys are set here, under each network, or with ', h('code', { text: 'flats key set' }), ' on the host.'],
        ctl: h('a', { class: 'btn btn-small', href: '/settings/networks', 'data-nav': true, text: 'Networks' }) }),
      row({ title: 'Set secret values', desc: ['No agent tool sets a secret. Set them in a flat’s settings, or with ', h('code', { text: 'flats secret set' }), ' on the Flats host.'] }),
      row({ title: 'Approve requests', desc: 'No agent tool decides an approval; every approval link opens in this console.',
        ctl: h('a', { class: 'btn btn-small', href: '/approvals', 'data-nav': true, text: 'Approvals' }) })),
      callout('info', 'These limits apply to the agent tools. The console itself has no login yet: it only refuses requests from other websites, so a program on a machine that can reach its management address can call it as this page does. Keep that address private.')),
    all);
}

// copyButton copies text() and says so.
function copyButton(text, label) {
  const btn = h('button', { type: 'button', class: 'btn btn-small', 'aria-label': 'Copy ' + label, text: 'Copy' });
  btn.addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(text());
      btn.textContent = 'Copied';
      setTimeout(() => { btn.textContent = 'Copy'; }, 1500);
    } catch {
      toast('Copy is unavailable here; select the text and press Ctrl/Cmd+C.');
    }
  });
  return btn;
}
