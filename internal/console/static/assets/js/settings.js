// Settings: network providers, console domains, operator keys, config.json,
// limits, the approvals queue and agent setup.

import { h, timeEl, dateTime, relTime, plural } from './dom.js';
import { api } from './api.js';
import { errorPanel, loading, toast, extLink, busy, fill, copyable, confirmDialog, infoDialog } from './ui.js';
import { describeApproval, statusBadge, cleanupPending } from './approval.js';

// Limits shown in human units; values are stored as integers in base units.
const FIELDS = [
  { key: 'upload_max_bytes', label: 'Upload size limit', unit: 'MB', factor: 1 << 20, min: 0.001, help: 'Total uncompressed size of one saved version.' },
  { key: 'version_quota', label: 'Version quota', unit: 'versions', factor: 1, step: 1, help: 'For websites and pages. Default version count per flat (20 on a new host). Each flat can override it. Versions are never deleted automatically; a full quota refuses new versions. Lowering it keeps existing versions. 0 is unlimited.' },
  { key: 'disk_quota_bytes', label: 'Disk quota per flat', unit: 'GB', factor: 1 << 30, help: '0 means no quota.' },
  { key: 'preview_ttl_seconds', label: 'Preview idle timeout', unit: 'hours', factor: 3600, min: 0.01, help: 'A preview closes after this long without visits.' },
  { key: 'rate_limit_rps', label: 'Rate limit per flat', unit: 'requests/s', factor: 1, step: 1, min: 1, help: 'Bursts up to twice this rate are allowed.' },
  { key: 'redirect_days', label: 'Redirect after a slug rename', unit: 'days', factor: 1, step: 1, help: 'How long old addresses redirect to the new ones.' },
  { key: 'events_keep', label: 'Log events kept per flat', unit: 'events', factor: 1, step: 1, min: 1, help: 'Older events are deleted.' },
  { key: 'revision_quota', label: 'Revisions to keep', unit: 'revisions', factor: 1, step: 1, help: 'Unpinned revisions each document or design keeps; older ones are deleted at once when lowered. Each document or design can override it. 0 keeps all.' },
  { key: 'pin_quota', label: 'Pinned revisions allowed', unit: 'pins', factor: 1, step: 1, help: 'Maximum pinned revisions per document or design. Pinned revisions are never deleted automatically. Each document or design can override it. 0 is unlimited.' },
  { key: 'portal_max_relays', label: 'Active Portal relays', unit: 'relays', factor: 1, step: 1, min: 1, portal: true, help: 'How many discovered relays a public flat uses at once. Used when Portal starts.' },
];

// LABELS names every setting in messages.
const LABELS = Object.fromEntries([...FIELDS.map((f) => [f.key, f.label]),
  ['portal_relays', 'Portal relays'], ['portal_discovery', 'Discover Portal relays'], ['portal_hide', 'Hide public flats from relay listings']]);

// Lower values of these settings remove data at the next pruning.
const RETENTION = ['events_keep', 'preview_ttl_seconds'];

const SOURCES = { default: 'default', file: 'config.json', flag: 'flag for this run' };

// Read-only config.json values: [key, label, value(config)].
const CONFIG_GROUPS = [
  ['Host', [
    ['host.management_addr', 'Management address', (c) => c.host.management_addr],
    ['host.local_addr', 'Local network address', (c) => c.host.local_addr],
    ['host.server_runtime', 'Server flats', (c) => (c.host.server_runtime ? 'on' : 'off')],
  ]],
  ['Network', [
    ['network.private_backend', 'Private routes', (c) => c.network.private_backend],
  ]],
];

export function mount(main, _params, ctx) {
  ctx.setTitle('Settings');
  const providersSlot = h('div', null, loading());
  const domainsSlot = h('div', null, loading());
  const keysSlot = h('div', null, loading());
  const noticeSlot = h('div');
  const configSlot = h('div', null, loading());
  const limitsSlot = h('div', null, loading());
  const approvalsSlot = h('div', null, loading());

  const card = (title, id, ...children) => h('section', { class: 'card', id, 'aria-labelledby': id + '-title' },
    h('h2', { id: id + '-title', text: title }), ...children);

  main.appendChild(h('section', { class: 'page' },
    h('div', { class: 'page-head' }, h('h1', { text: 'Settings' })),
    noticeSlot,
    card('Network providers', 'providers', providersSlot),
    card('Console domains', 'console-domains', domainsSlot),
    card('Keys', 'keys', keysSlot),
    card('Host configuration', 'configuration', configSlot),
    card('Limits', 'limits', limitsSlot),
    card('Approvals', 'approvals', approvalsSlot),
    card('Connect an agent', 'connect', connectAgent())));

  loadSettings();
  api.approvals().then((r) => ctx.alive() && fill(approvalsSlot, approvals(r.approvals || [])))
    .catch((err) => ctx.alive() && fill(approvalsSlot, errorPanel(err, 'Cannot load approvals')));

  // loadSettings renders the providers, config.json and the limits form;
  // saved is the result of the save that triggered the reload, if any.
  async function loadSettings(saved) {
    let data;
    try {
      data = await api.settings();
    } catch (err) {
      if (!ctx.alive()) return;
      fill(configSlot, errorPanel(err, 'Cannot load settings'));
      fill(limitsSlot);
      fill(providersSlot);
      fill(domainsSlot);
      fill(keysSlot);
      return;
    }
    if (!ctx.alive()) return;
    const showNotice = (cfg) => fill(noticeSlot, changedNotice(cfg));
    const opts = { reload: loadSettings, saved, showNotice };
    showNotice(data.config);
    fill(configSlot, configView(data.config));
    fill(limitsSlot, limitsForm(data, { ...opts, group: 'system' }));
    const [prov, domains, keys] = await Promise.allSettled([api.providers(), api.consoleDomains(), api.keys()]);
    if (!ctx.alive()) return;
    const providers = prov.status === 'fulfilled' ? (prov.value.providers || []) : [];
    lastProviders = providers;
    fill(providersSlot, prov.status === 'fulfilled' ? providerGroups(providers, data, opts) : errorPanel(prov.reason, 'Cannot load network providers'));
    fill(domainsSlot, domains.status === 'fulfilled' ? consoleDomains(domains.value, opts) : errorPanel(domains.reason, 'Cannot load the console domains'));
    fill(keysSlot, keys.status === 'fulfilled' ? keysCard(keys.value.keys || [], providers, opts) : errorPanel(keys.reason, 'Cannot load the keys'));
    scheduleRefresh(domains.value, keys.value && keys.value.keys, opts);
  }

  // Tailnet nodes and enrollments finish in the background: while one is
  // pending, the domain and key cards refresh every few seconds, except
  // while the operator is typing in them.
  let lastProviders = [];
  let refreshTimer = null;
  function scheduleRefresh(domains, keys, opts) {
    clearTimeout(refreshTimer);
    if (!pendingWork(domains, keys)) return;
    refreshTimer = setTimeout(async () => {
      if (!ctx.alive()) return;
      if ([domainsSlot, keysSlot].some(editing)) {
        scheduleRefresh(domains, keys, opts);
        return;
      }
      const [d, k] = await Promise.allSettled([api.consoleDomains(), api.keys()]);
      if (!ctx.alive()) return;
      // The operator may have started typing while the requests ran.
      if (d.status === 'fulfilled' && !editing(domainsSlot)) fill(domainsSlot, consoleDomains(d.value, opts));
      if (k.status === 'fulfilled' && !editing(keysSlot)) fill(keysSlot, keysCard(k.value.keys || [], lastProviders, opts));
      scheduleRefresh(d.value, k.value && k.value.keys, opts);
    }, 5000);
    // Never keep a test process alive (Node timers have unref; browsers ignore it).
    if (refreshTimer && typeof refreshTimer.unref === 'function') refreshTimer.unref();
  }
}

// pendingWork reports a console node or an enrollment still in progress.
export function pendingWork(domains, keys) {
  const all = Object.keys(NETWORKS).flatMap((prov) => (domains ? networkOf(domains, prov).domains : []) || []);
  const nodes = all.some((d) => !d.conflict && d.state !== 'ready');
  return nodes || (keys || []).some((k) => k.waiting);
}

// editing reports whether the operator is typing in a card.
function editing(slot) {
  const active = globalThis.document && document.activeElement;
  if (active && typeof slot.contains === 'function' && slot.contains(active)) return true;
  return typeof slot.querySelectorAll === 'function' && [...slot.querySelectorAll('input')].some((i) => i.value);
}

// --- network providers ---

// PROVIDER_INFO describes each provider for the operator. Local is always on;
// the others are off until turned on here, and a flat uses one only after it
// is also allowed on that flat.
export const PROVIDER_INFO = {
  local: { label: 'Local', what: 'This device, through localhost.' },
  tailscale: { label: 'Tailscale', what: 'Devices your tailnet ACL allows. Flats and previews get their own tailnet nodes.' },
  'tailscale-funnel': { label: 'Tailscale Funnel', what: 'Anyone on the internet, through Tailscale Funnel on the flat’s tailnet node. Visitors do not need Tailscale.' },
  portal: { label: 'Portal', what: 'Anyone on the internet, through Portal relays.' },
  zrok: { label: 'zrok', what: 'Anyone on the internet, through zrok public shares on your zrok account.' },
  'cloudflare-quick': { label: 'Cloudflare Quick Tunnel', what: 'Anyone on the internet, through an anonymous Cloudflare quick tunnel on a random trycloudflare.com address. No Cloudflare account or domain needed.' },
  netbird: { label: 'NetBird', what: 'Peers your NetBird access policies allow. Each flat runs on its own NetBird peer, at http://SLUG.<NetBird domain>/.' },
  'netbird-expose': { label: 'NetBird Expose', what: 'Anyone on the internet, through NetBird’s reverse proxy, from the flat’s own NetBird peer.' },
};

// providerGroups lists the providers as Private and Public groups. data is
// the settings response: its ETag guards a change, and the Portal panel
// holds the Portal settings.
export function providerGroups(list, data, opts) {
  const group = (scope, title, hint) => h('section', { class: 'provider-group', 'aria-labelledby': 'providers-' + scope },
    h('h3', { id: 'providers-' + scope, text: title }),
    h('p', { class: 'muted small', text: hint }),
    defaultField(scope, list, data, opts),
    list.filter((p) => p.scope === scope).map((p) => providerPanel(p, data, opts)));
  return [
    h('p', { class: 'muted', text: 'Local is always on. Turn on another provider to make it available, then allow it on each flat that should use it. Each mode has a default: a new flat starts allowed on the defaults only, and is always created Private. Turning a provider on does not publish a flat or make it public.' }),
    group('private', 'Private', 'Reachable from this device or your tailnet.'),
    group('public', 'Public', 'Reachable from the internet. A flat goes public only after you approve it.'),
  ];
}

// DEFAULT_KEYS are the settings naming the provider new flats inherit.
const DEFAULT_KEYS = { private: 'default_private_provider', public: 'default_public_provider' };

// effectiveDefault is the provider new flats inherit in scope ('' = none).
export function effectiveDefault(scope, data) {
  const v = (data.settings || {})[DEFAULT_KEYS[scope]] || '';
  if (scope === 'private' && !v) return (data.config && data.config.network.private_backend) || 'local';
  return v;
}

// defaultField picks the provider every new flat is allowed on for scope.
// Only providers turned on for the host can be the default.
function defaultField(scope, list, data, opts) {
  const key = DEFAULT_KEYS[scope];
  const id = 'default-' + scope;
  const current = effectiveDefault(scope, data);
  const choices = list.filter((p) => p.scope === scope && (p.id === 'local' || p.enabled));
  const options = scope === 'public' ? [['', 'None (new flats get no public provider)']] : [];
  for (const p of choices) options.push([p.id, (PROVIDER_INFO[p.id] || { label: p.id }).label]);
  const select = h('select', { id, 'aria-describedby': id + '-help' },
    options.map(([value, text]) => h('option', { value, text, selected: value === current })));
  select.value = current;
  select.addEventListener('change', async () => {
    const next = select.value;
    select.disabled = true;
    try {
      await api.saveSettings({ [key]: next }, data.config ? data.config.etag : '');
      const label = next ? (PROVIDER_INFO[next] || { label: next }).label : 'none';
      toast(`New flats now start with ${label} as the ${scope} provider.`, 'success');
      await opts.reload();
    } catch (err) {
      select.value = current;
      select.disabled = false;
      await infoDialog('Cannot change the default provider', errorPanel(err));
    }
  });
  const help = scope === 'public'
    ? 'New flats are allowed on this provider only, and stay Private until you approve making them public. Turn more public providers on to choose among them.'
    : 'New flats are allowed on this provider. Local is always allowed as well.';
  return h('div', { class: 'field provider-default' },
    h('label', { for: id, text: 'Default for new flats' }), select,
    h('span', { class: 'muted small', id: id + '-help', text: help }));
}

function providerPanel(p, data, opts) {
  const info = PROVIDER_INFO[p.id] || { label: p.id, what: '' };
  const local = p.id === 'local';
  const state = local ? h('span', { class: 'badge badge-ready', text: 'Always on' })
    : h('span', { class: 'badge' + (p.enabled ? ' badge-ready' : ''), text: p.enabled ? 'On' : 'Off' });
  const isDefault = effectiveDefault(p.scope, data) === p.id;
  const defBadge = isDefault ? h('span', { class: 'badge badge-default', text: 'Default' }) : null;
  let toggle = null;
  if (!local) {
    // zrok can be turned on only once an account is connected.
    // A build without NetBird cannot turn it on.
    const needsAccount = (p.id === 'zrok' && !p.enabled && !p.connected) || (!!p.unsupported && !p.enabled);
    toggle = needsAccount ? null : h('button', { type: 'button', class: 'btn btn-small' + (p.enabled ? '' : ' btn-primary'), text: p.enabled ? 'Turn off' : 'Turn on',
      'aria-label': `${p.enabled ? 'Turn off' : 'Turn on'} ${info.label}` });
    if (toggle) {
      toggle.addEventListener('click', () => busy(toggle, async () => {
        if (await setHostProvider(p, info, !p.enabled, data.config ? data.config.etag : '')) await opts.reload();
      }));
      toggle = withReason(toggle, p.locked || '');
    }
  }
  const used = p.flats.length
    ? h('p', { class: 'small' }, 'Allowed on ', p.flats.map((slug, i) => [i ? ', ' : '', h('a', { href: `/flats/${encodeURIComponent(slug)}`, 'data-nav': true, text: slug })]))
    : (local ? null : h('p', { class: 'muted small', text: 'Not allowed on any flat yet.' }));
  return h('article', { class: 'provider' + (p.enabled ? ' is-on' : ''), id: 'provider-' + p.id },
    h('div', { class: 'provider-head' },
      h('div', null, h('h4', null, info.label, state, defBadge), h('p', { class: 'muted small', text: info.what })),
      toggle),
    p.locked ? h('p', { class: 'muted small', text: p.locked }) : null,
    used,
    providerDetails(p, data, opts));
}

function providerDetails(p, data, opts) {
  if (p.id === 'zrok') return zrokDetails(p);
  if (p.id === 'netbird' || p.id === 'netbird-expose') return netbirdDetails(p);
  if (p.id === 'local') return p.address ? h('dl', { class: 'facts' }, h('dt', { text: 'Address' }), h('dd', null, h('code', { text: p.address }))) : null;
  if (p.id === 'portal') return [limitsForm(data, { ...opts, group: 'portal' }), p.enabled && p.status ? hostsTable(p.status) : null];
  if (!p.enabled) return null;
  if (p.id === 'tailscale') {
    return [
      h('dl', { class: 'facts' }, h('dt', { text: 'Auth key' }),
        h('dd', null, p.auth_key ? 'Stored. ' : 'None: each new node shows a login link below. ', keysLink('Manage it under Keys.'))),
      p.status ? hostsTable(p.status) : null,
    ];
  }
  if (p.id === 'tailscale-funnel') {
    return h('p', { class: 'muted small', text: 'Uses the Tailscale nodes. Your tailnet policy must allow Funnel and HTTPS certificates for them.' });
  }
  if (p.id === 'cloudflare-quick') {
    return [
      h('p', { class: 'muted small', text: 'Cloudflare offers quick tunnels for testing and development: no uptime guarantee, up to 200 concurrent requests per flat and no server-sent events. Each flat gets a random address. Flats keeps it across restarts while Cloudflare keeps the tunnel; if Cloudflare removes the tunnel, the flat gets a new address.' }),
      p.status ? hostsTable(p.status) : null,
    ];
  }
  return null;
}

// zrokDetails shows the zrok account the provider uses. The account token is
// managed under Keys; with zrok.environment set in config.json the account
// is managed outside Flats, with the zrok CLI.
function zrokDetails(p) {
  const facts = h('dl', { class: 'facts' },
    p.connected ? [
      h('dt', { text: 'Account' }), h('dd', { text: 'Connected' }),
      h('dt', { text: 'Environment ID' }), h('dd', null, h('code', { text: p.environment_id || '' })),
      h('dt', { text: 'zrok service' }), h('dd', null, h('code', { text: p.api_endpoint || '' })),
    ] : [h('dt', { text: 'Account' }), h('dd', { text: 'Not connected' })],
    h('dt', { text: 'Namespace' }), h('dd', null, h('code', { text: p.namespace || 'public' })));
  const hosts = p.enabled && p.status ? hostsTable(p.status) : null;
  if (!p.managed) {
    return [facts,
      h('p', { class: 'muted small', text: 'Uses the zrok environment set as zrok.environment in config.json; manage its account with the zrok CLI. Each public flat reserves its slug as a name in this namespace.' }),
      hosts];
  }
  return [facts,
    h('p', { class: 'muted small' }, p.connected
      ? 'Each public flat reserves its slug as a name in this namespace, so its URL stays the same. '
      : 'Connect a zrok account first, then turn zrok on. ', keysLink('Manage the account token under Keys.')),
    hosts];
}

// netbirdDetails shows what both NetBird providers share: the setup key every
// flat's peer registers with, managed under Keys, and the running peers.
function netbirdDetails(p) {
  // A binary built without NetBird cannot run a peer or turn it on.
  if (p.unsupported) return h('p', { class: 'alert alert-warn', text: p.unsupported });
  const facts = h('dl', { class: 'facts' },
    h('dt', { text: 'Setup key' }), h('dd', null, p.key_set ? 'Stored' : 'Not stored. ', p.key_set ? null : keysLink('Register one under Keys.')),
    h('dt', { text: 'Management server' }), h('dd', null, h('code', { text: p.management_url || '' })),
    p.id === 'netbird' ? [h('dt', { text: 'Peers running' }), h('dd', { text: String(p.peers || 0) })] : null);
  const hosts = p.enabled && p.status ? hostsTable(p.status) : null;
  if (p.id === 'netbird-expose') {
    return [facts, h('p', { class: 'muted small', text: 'A public flat is exposed through NetBird’s reverse proxy, which needs the proxy feature on your NetBird account or server. Anyone can open the URL.' }), hosts];
  }
  return [facts,
    h('p', { class: 'muted small', text: 'NetBird routes are plain HTTP: traffic between peers is encrypted by WireGuard, but browsers treat the page as not secure, so features such as clipboard writes, crypto.subtle, service workers and Secure cookies do not work there.' }),
    hosts];
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

// keysLink points to the Keys card.
function keysLink(text) {
  return h('a', { href: '#keys', text });
}


// zrokTokenForm sends an account token to connect or replace the zrok account.
function zrokTokenForm(opts, o) {
  const id = 'zrok-token';
  const input = h('input', { type: 'password', id, name: 'token', autocomplete: 'off', spellcheck: 'false', required: true,
    placeholder: 'zrok account token' });
  const submit = h('button', { type: 'submit', class: 'btn btn-primary btn-small', text: o.submit });
  const form = h('form', { class: 'settings-form provider-form', novalidate: true },
    h('label', { for: id, text: o.label }), input,
    h('p', { class: 'muted small', text: o.help }),
    h('div', { class: 'form-actions' }, submit));
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
      await opts.reload();
    });
  });
  return form;
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

// --- keys ---

const KEY_INFO = {
  tailscale: { label: 'Tailscale auth key', what: 'Logs in each new tailnet node: flats, previews and console domains. Nodes already on the tailnet keep their identity.' },
  netbird: { label: 'NetBird setup key', what: 'Registers each flat and NetBird console domain as its own NetBird peer. Peers already registered keep their identity.' },
  zrok: { label: 'zrok account token', what: 'Registers this machine as an environment of your zrok account.' },
};

const KEY_SOURCES = { console: 'the console', cli: 'flats key set', flag: 'a flats serve flag', env: 'an environment variable',
  config: 'config.json (moved to flats.db)', unknown: 'unknown (connected before Flats recorded it)' };

// keysCard lists every operator key the same way: its state, where it came
// from and when, and the actions to set, replace or remove it. Values are
// never shown.
export function keysCard(keys, providers, opts) {
  const byId = Object.fromEntries(providers.map((p) => [p.id, p]));
  return [
    h('p', { class: 'muted' }, 'Keys are stored in flats.db, encrypted with secret.key, and never shown again. They can also be given before the first start with ',
      h('code', { text: 'flats key set' }), ' or a ', h('code', { text: 'flats serve' }),
      ' flag or environment variable; those only fill in a key that is not stored yet.'),
    ...keys.map((k) => keyRow(k, byId, opts)),
  ];
}

function keyRow(k, providers, opts) {
  const info = KEY_INFO[k.id] || { label: k.id, what: '' };
  let state = k.set ? (k.id === 'zrok' ? 'Connected' : 'Stored') : (k.waiting ? 'Waiting for the next start' : 'Not set');
  if (k.unsupported) state = 'Not available';
  const facts = [];
  if (k.source) facts.push(h('dt', { text: 'Source' }), h('dd', { text: KEY_SOURCES[k.source] || k.source }));
  if (k.updated_at) facts.push(h('dt', { text: 'Set' }), h('dd', null, h('time', { datetime: k.updated_at, title: dateTime(k.updated_at), text: relTime(k.updated_at) })));
  if (k.environment_id) facts.push(h('dt', { text: 'Environment ID' }), h('dd', null, h('code', { text: k.environment_id })));
  if (k.api_endpoint) facts.push(h('dt', { text: 'zrok service' }), h('dd', null, h('code', { text: k.api_endpoint })));
  if (k.id === 'netbird' && k.management_url) facts.push(h('dt', { text: 'Management server' }), h('dd', null, h('code', { text: k.management_url })));
  if (k.id === 'netbird' && k.peers) facts.push(h('dt', { text: 'Peers running' }), h('dd', { text: String(k.peers) }));
  return h('article', { class: 'provider key' + (k.set ? ' is-on' : ''), id: 'key-' + k.id },
    h('div', { class: 'provider-head' },
      h('div', null, h('h4', null, info.label, h('span', { class: 'badge' + (k.set ? ' badge-ready' : ''), text: state })),
        h('p', { class: 'muted small', text: info.what }))),
    facts.length ? h('dl', { class: 'facts' }, facts) : null,
    k.external_differs ? h('p', { class: 'alert alert-warn', text: 'The key given to this run by a flag or environment variable differs from the stored key. Flats uses the stored key; register a new one here to replace it.' }) : null,
    k.last_error ? h('p', { class: 'alert alert-warn', text: `The last attempt to use this key failed: ${k.last_error}` }) : null,
    k.legacy_peer ? legacyPeerNotice(k.legacy_peer, opts) : null,
    keyActions(k, providers, opts));
}

function keyActions(k, providers, opts) {
  if (k.id === 'tailscale') return storedKeyActions(k, opts, TAILSCALE_KEY);
  if (k.id === 'netbird') {
    if (k.unsupported) return h('p', { class: 'alert alert-warn', text: k.unsupported });
    return storedKeyActions(k, opts, NETBIRD_KEY);
  }
  if (k.id === 'zrok') {
    if (!k.managed) return h('p', { class: 'muted small', text: 'zrok.environment in config.json names an environment managed with the zrok CLI; manage its account there.' });
    const enabled = !!(providers.zrok && providers.zrok.enabled);
    if (!k.set) {
      return zrokTokenForm(opts, {
        label: 'Account token', submit: 'Connect',
        help: 'Sign in at myzrok.io, open the zrok API console and copy the account token from its getting-started wizard. Flats registers this machine as an environment of that account and keeps the token in its data directory; it is never shown again. Turn zrok on once the account is connected.',
        done: 'zrok account connected. Turn zrok on to use it.',
      });
    }
    return [
      zrokTokenForm(opts, {
        label: 'Replace with a new account token', submit: 'Replace account',
        help: 'Connects the account of the new token in place of this one, for example after regenerating the token. The current account stays connected if the new token is refused. Refused while a flat is public through zrok.',
        done: 'zrok account replaced.',
        confirm: { title: 'Replace the zrok account?', body: 'Flats registers this machine with the account of the new token, then unregisters the current environment and deletes its token.' },
      }),
      zrokDisconnect(k, enabled, opts),
    ];
  }
  return null;
}

// TAILSCALE_KEY and NETBIRD_KEY describe the keys Flats stores and uses for
// every new node or peer.
const TAILSCALE_KEY = {
  id: 'tailscale-key', name: 'Tailscale auth key', placeholder: 'tskey-auth-…', label: 'Auth key', replace: 'Replace with a new auth key',
  help: 'In the Tailscale admin console, open Settings › Keys and generate a reusable auth key. Leave it untagged so console decisions record who made them. New nodes use it from now on; nodes already on the tailnet are not affected.',
  removeBody: 'New tailnet nodes then wait for an interactive login and show its link. Nodes already on the tailnet keep working.',
  set: (key) => api.setTailscaleKey(key), remove: () => api.deleteTailscaleKey(),
};
const NETBIRD_KEY = {
  id: 'netbird-key', name: 'NetBird setup key', placeholder: 'xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx', label: 'Setup key', replace: 'Replace with a new setup key',
  help: 'In the NetBird dashboard, open Setup Keys and create a key with “Allow extra DNS labels” on (required: each flat is reached by its own name). Make it reusable and ephemeral, with an auto-assigned group your access policies use, and without an expiry or usage limit: Flats registers a peer with it for every flat and console domain, and again when NetBird has removed an offline ephemeral peer. Peers already registered keep working when the key changes.',
  removeBody: 'Running peers keep working, but a new flat, a new NetBird console domain or a peer NetBird removed cannot register until you register a key again.',
  set: (key) => api.setNetbirdKey(key), remove: () => api.deleteNetbirdKey(),
};

// storedKeyActions stores a key Flats keeps (Tailscale, NetBird); a stored
// one can be replaced or removed.
function storedKeyActions(k, opts, cfg) {
  const input = h('input', { type: 'password', id: cfg.id, name: 'key', autocomplete: 'off', spellcheck: 'false', required: true,
    placeholder: cfg.placeholder });
  const submit = h('button', { type: 'submit', class: 'btn btn-primary btn-small', text: k.set ? 'Replace key' : 'Save key' });
  const form = h('form', { class: 'settings-form provider-form', novalidate: true },
    h('label', { for: cfg.id, text: k.set ? cfg.replace : cfg.label }), input,
    h('p', { class: 'muted small', text: cfg.help }),
    h('div', { class: 'form-actions' }, submit));
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
      await opts.reload();
    });
  });
  if (!k.set) return form;
  const remove = h('button', { type: 'button', class: 'btn btn-small', text: 'Remove key' });
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
    await opts.reload();
  }));
  return [form, h('div', { class: 'form-actions' }, remove)];
}

// legacyPeerNotice asks the operator to delete the shared NetBird peer an
// earlier version registered for the whole host.
function legacyPeerNotice(p, opts) {
  const done = h('button', { type: 'button', class: 'btn btn-small', text: 'Done' });
  done.addEventListener('click', () => busy(done, async () => {
    try {
      await api.dismissNetbirdLegacyPeer();
    } catch (err) {
      await infoDialog('Cannot dismiss the notice', errorPanel(err));
      return;
    }
    await opts.reload();
  }));
  return h('div', { class: 'alert alert-warn', role: 'status' },
    h('p', null, 'Flats no longer uses the single NetBird peer ', h('code', { text: p.device_name || '(unnamed)' }),
      ' this host registered before: each flat now runs on its own peer. Its local identity was removed. Delete that peer in the NetBird dashboard (Peers), then choose Done.'),
    h('div', { class: 'form-actions' }, done));
}

function zrokDisconnect(k, enabled, opts) {
  const disconnect = h('button', { type: 'button', class: 'btn btn-small', text: 'Disconnect account' });
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
        body: [errorPanel(err), h('p', { text: `Forget the account on this machine anyway? Environment ${k.environment_id || ''} may stay registered in your zrok account; remove it in the zrok console.` })],
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
    await opts.reload();
  }));
  return h('div', { class: 'form-actions' }, withReason(disconnect, enabled ? 'zrok is on. Turn it off under Network providers before disconnecting its account.' : ''));
}

// --- console domains ---

// NETWORKS describes the networks console domains run on, in the order the
// card lists them.
const NETWORKS = {
  tailscale: {
    heading: 'Tailnet (Tailscale)', noun: 'tailnet domain', node: 'tailnet node',
    what: 'Each name is its own tailnet node at https://NAME.<tailnet>.ts.net, open to the devices your tailnet ACL allows.',
    off: 'Turn Tailscale on under Network providers to add a tailnet domain.',
    add: 'A lowercase name: letters, digits and hyphens. It cannot be a flat’s name. The new tailnet node logs in with the Tailscale auth key under Keys, or shows a login link here.',
    added: 'Its node joins the tailnet in a minute or two.',
    removed: 'These names were deleted, but their tailnet nodes are not logged out yet. They stay reserved until that succeeds; Flats retries at the next start.',
    assigned: (d) => `The tailnet named this node ${d.assigned}: another machine already uses ${d.name}. Rename or remove that machine in the Tailscale admin console, or use this address.`,
  },
  netbird: {
    heading: 'NetBird', noun: 'NetBird domain', node: 'NetBird peer',
    what: 'Each name is its own NetBird peer at http://NAME.<NetBird domain>/, open to the peers your NetBird access policies allow. NetBird gives peers no certificates, so the console is plain HTTP there; traffic between peers is still encrypted by WireGuard.',
    off: 'Turn NetBird on under Network providers to add a NetBird domain.',
    add: 'A lowercase name: letters, digits and hyphens. It cannot be a flat’s name, but may match a tailnet domain. The new peer registers with the NetBird setup key under Keys.',
    added: 'Its peer joins NetBird in a minute.',
    removed: 'These names were deleted, but their NetBird peers are not stopped yet. They stay reserved until that succeeds; Flats retries at the next start.',
    assigned: (d) => `NetBird answers this peer as ${d.assigned} as well.`,
  },
};

// consoleDomains lists the console's names on each network. Each is its own
// node; names are only added and deleted, so a change of name is an addition
// followed by a deletion.
export function consoleDomains(view, opts) {
  return [
    h('p', { class: 'muted' }, 'The addresses where this console opens, one list per network. To change an address, add the new one, move your agents to it (',
      h('code', { text: 'flats connect' }), '), then delete the old one. Several can be used together.'),
    ...Object.keys(NETWORKS).map((prov) => networkDomains(view, prov, opts)),
  ];
}

// networkOf returns one network's part of the view.
function networkOf(view, prov) {
  return (view.providers && view.providers[prov]) || { domains: [], retiring: [], on: false };
}

// readyDomains lists every ready domain, on any network, as provider/name.
function readyDomains(view) {
  return Object.keys(NETWORKS).flatMap((prov) => (networkOf(view, prov).domains || [])
    .filter((d) => !d.conflict && d.state === 'ready').map((d) => prov + '/' + d.name));
}

function networkDomains(view, prov, opts) {
  const net = NETWORKS[prov];
  const pv = networkOf(view, prov);
  const domains = pv.domains || [];
  const id = 'console-domains-' + prov;
  const rows = domains.map((d) => domainRow(d, prov, view, opts));
  const retiring = (pv.retiring || []).map((d) => retiringRow(d, prov, view, opts));
  const parts = [h('h3', { id, text: net.heading }), h('p', { class: 'muted small', text: net.what })];
  if (pv.pinned) parts.push(h('p', { class: 'muted small', text: `Set by ${pv.pinned}. Reinstall the service with flats install to manage the ${net.noun}s here.` }));
  if (!domains.length) {
    parts.push(h('p', { class: 'muted small', text: `No ${net.noun}.` }));
  } else {
    parts.push(h('div', { class: 'table-wrap' }, h('table', { class: 'table', 'aria-labelledby': id },
      h('thead', null, h('tr', null, ['Name', 'State', 'Address', ''].map((t) => h('th', { scope: 'col', text: t })))),
      h('tbody', null, rows))));
  }
  if (retiring.length) {
    parts.push(h('h4', { text: 'Being removed' }), h('p', { class: 'muted small', text: net.removed }),
      h('ul', { class: 'plain-list' }, retiring));
  }
  if (!pv.pinned) parts.push(pv.on ? domainAddForm(prov, view, opts) : h('p', { class: 'muted small', text: net.off }));
  return h('section', { class: 'domain-network', 'aria-labelledby': id }, parts);
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

function domainRow(d, prov, view, opts) {
  const net = NETWORKS[prov];
  const login = d.state === 'needs-login' && (d.detail || '').match(/https?:\/\/\S+/)?.[0];
  const state = d.conflict ? h('span', { class: 'badge badge-error', text: 'not served' })
    : h('span', { class: 'badge badge-' + (d.state || 'starting'), text: d.state || 'starting' });
  const del = h('button', { type: 'button', class: 'btn btn-small', text: 'Delete', 'aria-label': `Delete ${net.noun} ${d.name}` });
  del.addEventListener('click', () => busy(del, () => deleteDomain(d, prov, view, opts)));
  const pinned = networkOf(view, prov).pinned;
  return h('tr', null,
    h('th', { scope: 'row' }, h('code', { text: d.name }), d.current ? h('span', { class: 'badge', text: 'this page' }) : null),
    h('td', null, state),
    h('td', null,
      login ? h('div', null, extLink(login, 'Log in', 'btn btn-small btn-primary')) : null,
      d.url ? extLink(d.url) : null,
      d.conflict ? h('div', { class: 'muted small', text: `Not served: ${d.conflict}. Delete this domain or rename that flat.` }) : null,
      d.assigned && prov === 'tailscale' ? h('div', { class: 'warn-text small', text: net.assigned(d) }) : null,
      d.detail && !login && !d.conflict ? h('div', { class: 'muted small' }, linkify(d.detail)) : null),
    h('td', null, pinned ? null : withReason(del, deleteBlocker(d, view, totalDomains(view), readyDomains(view), prov))));
}

function retiringRow(d, prov, view, opts) {
  const retry = h('button', { type: 'button', class: 'btn btn-small', text: 'Retry', 'aria-label': `Retry removing ${d.name}` });
  retry.addEventListener('click', () => busy(retry, async () => {
    try {
      await api.deleteConsoleDomain(prov, d.name, view.etag);
    } catch (err) {
      await infoDialog(`Cannot remove ${d.name}`, errorPanel(err));
      return;
    }
    toast(`${d.name} removed.`, 'success');
    await opts.reload();
  }));
  return h('li', { class: 'plain-row' },
    h('div', null, h('code', { text: d.name }), d.error ? h('div', { class: 'muted small', text: d.error }) : null), retry);
}

async function deleteDomain(d, prov, view, opts) {
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
    await opts.reload();
    return;
  }
  toast(`${d.name} deleted.`, 'success');
  if (d.current && res && res.redirect) {
    window.location.href = res.redirect.replace(/\/$/, '') + '/settings#console-domains';
    return;
  }
  await opts.reload();
}

function domainAddForm(prov, view, opts) {
  const net = NETWORKS[prov];
  const id = 'console-domain-name-' + prov;
  const input = h('input', { type: 'text', id, name: 'name', autocomplete: 'off', spellcheck: 'false', required: true,
    pattern: '[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?', placeholder: 'flats' });
  const submit = h('button', { type: 'submit', class: 'btn btn-primary btn-small', text: 'Add' });
  const form = h('form', { class: 'settings-form provider-form', novalidate: true },
    h('label', { for: id, text: `Add a ${net.noun}` }), input,
    h('p', { class: 'muted small', text: net.add }),
    h('div', { class: 'form-actions' }, submit));
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    const name = input.value.trim();
    if (!name) { input.focus(); return; }
    busy(submit, async () => {
      try {
        await api.addConsoleDomain(prov, name, view.etag);
      } catch (err) {
        await infoDialog(`Cannot add ${name}`, errorPanel(err));
        return;
      }
      input.value = '';
      toast(`${name} added. ${net.added}`, 'success');
      await opts.reload();
    });
  });
  return form;
}

function humanKey(k) {
  const s = k.replace(/_/g, ' ');
  return s.charAt(0).toUpperCase() + s.slice(1);
}

// linkify turns http(s) URLs in text into links (for login URLs in details).
function linkify(text) {
  const parts = [];
  let last = 0;
  for (const m of text.matchAll(/https?:\/\/[^\s"'<>]+/g)) {
    parts.push(text.slice(last, m.index), extLink(m[0]));
    last = m.index + m[0].length;
  }
  parts.push(text.slice(last));
  return h('span', null, parts);
}

// hostsTable lists the hosts a provider backend serves (core.NetStatus).
function hostsTable(n) {
  const hosts = n.hosts || [];
  const rows = hosts.map((x) => {
    // tsnet reports the login URL as the detail of a needs-login host.
    const login = x.login_url || (x.state === 'needs-login' && (x.detail || '').match(/https?:\/\/\S+/)?.[0]);
    const expiry = x.key_expiry ? h('span', { class: x.state === 'key-expiring' ? 'warn-text' : '' },
      h('time', { datetime: x.key_expiry, title: dateTime(x.key_expiry), text: relTime(x.key_expiry) })) : h('span', { class: 'muted', text: '—' });
    return h('tr', null,
      h('th', { scope: 'row' }, h('code', { text: x.host }), x.ephemeral ? h('span', { class: 'badge', text: 'preview' }) : null),
      h('td', null, h('span', { class: 'badge badge-' + x.state, text: x.state })),
      h('td', null, expiry),
      h('td', null,
        login ? h('div', null, extLink(login, 'Log in', 'btn btn-small btn-primary')) : null,
        x.url ? extLink(x.url) : null,
        x.detail ? h('div', { class: 'muted small' }, linkify(x.detail)) : null));
  });
  return h('div', { class: 'net' },
    n.detail ? h('p', { class: 'muted small' }, linkify(n.detail)) : null,
    hosts.length
      ? h('div', { class: 'table-wrap' }, h('table', { class: 'table' },
        h('thead', null, h('tr', null, ['Host', 'State', 'Key expiry', 'Address'].map((t) => h('th', { scope: 'col', text: t })))),
        h('tbody', null, rows)))
      : h('p', { class: 'muted small', text: 'Nothing is served on this provider yet.' }));
}

// --- config.json ---

function changedNotice(cfg) {
  if (!cfg || !cfg.changed_on_disk) return null;
  return h('div', { class: 'alert alert-warn', role: 'status' },
    h('strong', { text: 'config.json changed on disk' }),
    h('p', { text: 'Restart Flats to load it. Until then Flats keeps the settings it loaded, and saving here is refused.' }));
}

function sourceBadge(src) {
  if (!src) return null;
  return h('span', { class: 'badge' }, h('span', { class: 'sr-only', text: 'source: ' }), SOURCES[src] || src);
}

// configView shows the host, network and credentials keys of config.json,
// which the console only reads.
export function configView(cfg) {
  if (!cfg) return h('p', { class: 'muted', text: 'This host keeps its settings in memory, not in config.json.' });
  const sources = cfg.sources || {};
  return [
    cfg.mode === 'legacy' ? h('p', { class: 'notice-text' },
      'This service starts without ', h('code', { text: '--config' }), '. Run ', h('code', { text: 'flats install' }),
      ' to switch it to config.json.') : null,
    h('p', { class: 'muted' }, 'Read only. Change these values in config.json, or with ', h('code', { text: 'flats config set' }),
      ' while Flats is stopped. They apply after Flats restarts. Turn network providers on or off above.'),
    ...CONFIG_GROUPS.flatMap(([title, rows]) => [
      h('h3', { text: title }),
      h('dl', { class: 'facts' }, rows.map(([key, label, value]) => [
        h('dt', { text: label }),
        h('dd', null, h('span', { class: 'mono', text: String(value(cfg)) }), ' ', sourceBadge(sources[key]))])),
    ]),
  ];
}

// --- limits ---

function display(base, factor) {
  const n = Number(base) / factor;
  return Number.isFinite(n) ? String(Math.round(n * 1000) / 1000) : '';
}

// Only destructive retention settings use isDecrease. Version quotas keep all existing versions.
export function isDecrease(key, current, next) {
  const cur = Number(current);
  const n = Number(next);
  return n < cur;
}

// limitsForm edits the system settings (opts.group 'system') or the Portal
// settings ('portal'). opts: reload(saved), saved (the last save response),
// showNotice(config).
function limitsForm(data, opts) {
  const portal = opts.group === 'portal';
  const settings = data.settings || {};
  const defaults = data.defaults || {};
  const cfg = data.config || null;
  const etag = cfg ? cfg.etag : '';
  const sources = (cfg && cfg.sources) || {};
  const keys = (cfg && cfg.keys) || {};
  const pinned = (cfg && cfg.pinned) || {};
  const controls = {}; // setting key -> { input, error, describedBy }

  // control wires a field's hint, source, pin reason and error message.
  const control = (key, input, help) => {
    const id = input.getAttribute('id');
    const src = sources[keys[key]];
    const hint = [help, src ? `Source: ${SOURCES[src] || src}.` : ''].filter(Boolean).join(' ');
    const describedBy = [id + '-help'];
    const pin = pinned[key] ? h('span', { class: 'hint', id: id + '-pin', text: `Set by ${pinned[key]}. Reinstall the service with \`flats install\` to change it here.` }) : null;
    if (pin) {
      input.setAttribute('disabled', '');
      input.disabled = true;
      describedBy.push(id + '-pin');
    }
    const error = h('span', { class: 'field-error', id: id + '-error', hidden: true });
    input.setAttribute('aria-describedby', describedBy.join(' '));
    controls[key] = { input, error, describedBy };
    return [h('span', { class: 'hint', id: id + '-help', text: hint }), pin, error];
  };

  const inputs = [];
  const numberField = (f) => {
    const id = 'set-' + f.key;
    const initial = display(settings[f.key], f.factor);
    const input = h('input', {
      id, type: 'number', inputmode: 'decimal', min: String(f.min ?? 0), step: f.step ? String(f.step) : 'any',
      value: initial, required: true,
    });
    inputs.push({ f, input, initial });
    const def = defaults[f.key] !== undefined ? ` Default: ${display(defaults[f.key], f.factor)} ${f.unit}.` : '';
    return h('div', { class: 'field' },
      h('label', { for: id, text: f.label }),
      h('div', { class: 'with-unit' }, input, h('span', { class: 'unit', text: f.unit })),
      control(f.key, input, f.help + def));
  };

  const relaysId = 'set-portal_relays';
  const relaysInitial = (settings.portal_relays || '').split(',').map((s) => s.trim()).filter(Boolean).join('\n');
  const relays = h('textarea', { id: relaysId, rows: '3', spellcheck: 'false', class: 'mono', placeholder: 'https://relay.example.com' });
  relays.value = relaysInitial;
  const discoverId = 'set-portal_discovery';
  const discoverInitial = String(settings.portal_discovery ?? 'true') !== 'false';
  const discover = h('input', { id: discoverId, type: 'checkbox', checked: discoverInitial });
  const hideId = 'set-portal_hide';
  const hideInitial = String(settings.portal_hide ?? 'false') === 'true';
  const hide = h('input', { id: hideId, type: 'checkbox', checked: hideInitial });

  const save = h('button', { type: 'submit', id: portal ? 'portal-save' : 'settings-save', class: 'btn btn-primary' + (portal ? ' btn-small' : ''),
    text: portal ? 'Save Portal settings' : 'Save settings' });
  const result = h('div', { class: 'muted small', role: 'status' }, savedSummary(opts.saved));
  const errorSlot = h('div');
  const impactSlot = h('div');
  const form = h('form', { class: 'settings-form' + (portal ? ' provider-form' : ''), novalidate: true },
    portal ? null : h('fieldset', { class: 'field-group' }, h('legend', { text: 'System' }),
      h('div', { class: 'form-grid' }, FIELDS.filter((f) => !f.portal).map(numberField))),
    !portal ? null : h('fieldset', { class: 'field-group' }, h('legend', { text: 'Portal settings' }),
      h('div', { class: 'form-grid' },
        h('div', { class: 'field field-wide' },
          h('label', { for: relaysId, text: 'Portal relays' }), relays,
          control('portal_relays', relays, 'One relay per line. Leave empty to use Portal’s defaults (relay discovery, up to 3 active relays). Used when Portal starts; restart Flats to apply a change while Portal runs.')),
        h('div', { class: 'field field-wide field-check' },
          discover, h('label', { for: discoverId, text: 'Discover Portal relays' }),
          control('portal_discovery', discover, 'Find further relays automatically (Portal’s default). Used when Portal starts.')),
        h('div', { class: 'field field-wide field-check' },
          hide, h('label', { for: hideId, text: 'Hide public flats from relay listings' }),
          control('portal_hide', hide, 'Keeps public URLs out of the relays’ lists of sites. It is not access control: anyone with a URL can still open the flat. Saving updates public flats at once; relays apply it at their next lease renewal (up to about 90 seconds). A flat can override it under its Settings → Networks.')),
        FIELDS.filter((f) => f.portal).map(numberField))),
    impactSlot,
    errorSlot,
    h('div', { class: 'actions' }, result, save));

  const setError = (key, message) => {
    const c = controls[key];
    if (!c) return;
    c.error.textContent = message;
    c.error.hidden = false;
    c.error.removeAttribute('hidden');
    c.input.setAttribute('aria-invalid', 'true');
    c.input.setAttribute('aria-describedby', [...c.describedBy, c.error.getAttribute('id')].join(' '));
  };
  const clearErrors = () => {
    fill(errorSlot);
    for (const c of Object.values(controls)) {
      c.error.textContent = '';
      c.error.hidden = true;
      c.error.setAttribute('hidden', '');
      c.input.removeAttribute('aria-invalid');
      c.input.setAttribute('aria-describedby', c.describedBy.join(' '));
    }
  };
  const showError = (err) => {
    // Mark the field the server names, by setting or config.json key.
    const msg = err.message || String(err);
    const key = Object.keys(controls).find((k) => msg.includes(k) || (keys[k] && msg.includes(keys[k])));
    if (key) setError(key, msg);
    fill(errorSlot, h('div', { class: 'alert alert-error', role: 'alert' },
      h('strong', { text: 'Settings not saved' }), h('p', { text: msg }),
      h('p', { class: 'small', text: 'Your changes are still in the form.' })));
    if (key) controls[key].input.focus();
  };

  // changes returns the edited settings, or null after marking an invalid
  // field.
  const changes = () => {
    const out = {};
    for (const { f, input, initial } of inputs) {
      if (pinned[f.key] || input.value === initial) continue;
      const n = Number(input.value);
      if (input.value.trim() === '' || !Number.isFinite(n) || n < 0 || (f.key === 'version_quota' && !Number.isSafeInteger(n))) {
        setError(f.key, `${f.label} must be ${f.key === 'version_quota' ? 'a whole number' : 'a number'} of 0 or more.`);
        input.focus();
        return null;
      }
      out[f.key] = String(Math.round(n * f.factor));
    }
    if (!portal) return out;
    const relayList = relays.value.split(/[\n,]/).map((s) => s.trim()).filter(Boolean);
    if (!pinned.portal_relays && relayList.join('\n') !== relaysInitial) out.portal_relays = relayList.join(',');
    if (!pinned.portal_discovery && discover.checked !== discoverInitial) out.portal_discovery = String(discover.checked);
    if (!pinned.portal_hide && !!hide.checked !== hideInitial) out.portal_hide = String(!!hide.checked);
    return out;
  };

  const store = async (out) => {
    try {
      const res = await api.saveSettings(out, etag);
      toast('Settings saved.', 'success');
      await opts.reload(res);
    } catch (err) {
      fill(result);
      showError(err);
      // The file may have changed on disk; show that without discarding input.
      if (err.status === 412 || err.status === 409) {
        api.settings().then((d) => opts.showNotice(d.config)).catch(() => {});
      }
    }
  };

  // Any edit invalidates a shown or pending impact; saving checks it again.
  // generation counts edits and submits, so a late impact response for
  // older values is dropped.
  let generation = 0;
  const edited = () => { generation++; fill(impactSlot); };
  form.addEventListener('input', edited);
  form.addEventListener('change', edited);
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    const gen = ++generation;
    clearErrors();
    fill(impactSlot);
    const out = changes();
    if (!out) return;
    if (!Object.keys(out).length) { fill(result, h('p', { text: 'Nothing changed.' })); return; }
    busy(save, async () => {
      const lower = RETENTION.filter((k) => k in out && isDecrease(k, settings[k], out[k]));
      if (!lower.length) return store(out);
      let impact;
      try {
        impact = (await api.settingsImpact(out)).impact || {};
      } catch (err) {
        if (gen === generation) showError(err);
        return;
      }
      if (gen !== generation) return;
      if (!Object.keys(impact).length) return store(out);
      // The confirmation saves only the values it described.
      const confirm = () => {
        const now = changes();
        if (gen !== generation || !now || !sameChanges(now, out)) {
          fill(impactSlot);
          if (now) fill(errorSlot, h('div', { class: 'alert alert-warn', role: 'alert' },
            h('p', { text: 'The form changed after the preview. Save again to check the new values.' })));
          return undefined;
        }
        return store(out);
      };
      fill(impactSlot, impactPanel(impact, confirm, () => {
        fill(impactSlot);
        controls[lower[0]].input.focus();
      }));
    });
  });
  return form;
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
  if (!applied.length && !later.length) return h('p', { text: 'Saved. No value changed.' });
  return h('ul', { class: 'compact saved-list' },
    applied.map((k) => h('li', { text: `${LABELS[k] || k}: saved, applied now.` })),
    later.map((k) => h('li', { text: `${LABELS[k] || k}: saved, applies after Flats restarts.` })));
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
  const removes = Object.values(impact).some((r) => r.total > 0);
  const heading = h('h3', { id: titleId, tabindex: '-1', text: removes ? 'Saving removes data' : 'Check the lower retention values' });
  const confirm = h('button', { type: 'button', class: removes ? 'btn btn-danger' : 'btn btn-primary', text: removes ? 'Save and remove' : 'Save' });
  const cancel = h('button', { type: 'button', class: 'btn', text: 'Keep editing' });
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
    h('div', { class: 'actions' }, cancel, confirm));
  queueMicrotask(() => heading.focus());
  return panel;
}

// --- approvals ---

function approvals(list) {
  if (!list.length) return h('p', { class: 'muted', text: 'No approval requests. Agents ask for approval when they try to make a flat public or delete one.' });
  return h('div', { class: 'table-wrap' }, h('table', { class: 'table' },
    h('thead', null, h('tr', null, ['Request', 'Via', 'Requested', 'Status'].map((t) => h('th', { scope: 'col', text: t })))),
    h('tbody', null, list.map((a) => h('tr', null,
      h('th', { scope: 'row' }, h('a', { href: `/approvals/${encodeURIComponent(a.id)}`, 'data-nav': true, text: describeApproval(a) }),
        a.reason ? h('div', { class: 'muted small', text: a.reason }) : null),
      h('td', { text: a.via }),
      h('td', null, timeEl(a.requested_at)),
      h('td', null, statusBadge(a.status), cleanupPending(a) ? h('span', { class: 'badge badge-warn', text: 'Storage cleanup pending' }) : null))))));
}

// --- connect an agent ---

function connectAgent() {
  const url = location.origin + '/mcp';
  return [
    h('p', null, 'Flats speaks MCP over Streamable HTTP at ', h('code', { text: url }),
      '. Agents can create flats, save Drafts, request numbered versions and deploy them, open previews and read logs. Version creation, deployment and deletion, visibility changes and deleting a flat need your approval here.'),
    copyable(`claude mcp add --transport http flats ${url}`, 'Claude Code'),
    copyable(`# ~/.codex/config.toml\n[mcp_servers.flats]\nurl = "${url}"`, 'Codex'),
    copyable(JSON.stringify({ mcpServers: { flats: { url } } }, null, 2), 'Cursor (~/.cursor/mcp.json or .cursor/mcp.json)'),
    h('h3', { text: 'Command line' }),
    h('p', { class: 'muted', text: 'Save a Private Draft, request a numbered version, then deploy that version. Version creation and deployment each need their own approval:' }),
    copyable('flats save-draft <slug> ./dist\nflats create-version <slug> --revision <revision> --hash <hash>\nflats deploy --flat <slug> --version <version>', 'CLI'),
    h('p', { class: 'muted small', text: 'Secret values can only be set in this console or with “flats secret set” on the Flats host.' }),
  ];
}

