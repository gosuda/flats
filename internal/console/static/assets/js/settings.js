// Settings: network providers, config.json, limits, the approvals queue
// and agent setup.

import { h, timeEl, dateTime, relTime, plural } from './dom.js';
import { api } from './api.js';
import { errorPanel, loading, toast, extLink, busy, fill, copyable, confirmDialog, infoDialog } from './ui.js';
import { describeApproval, statusBadge } from './approval.js';

// Limits shown in human units; values are stored as integers in base units.
const FIELDS = [
  { key: 'upload_max_bytes', label: 'Upload size limit', unit: 'MB', factor: 1 << 20, min: 0.001, help: 'Total uncompressed size of one saved version.' },
  { key: 'keep_versions', label: 'Versions to keep', unit: 'versions', factor: 1, step: 1, help: 'Files of older versions are pruned after each deploy, which also closes the flat’s previews. The live version is always kept. 0 keeps every version.' },
  { key: 'disk_quota_bytes', label: 'Disk quota per flat', unit: 'GB', factor: 1 << 30, help: '0 means no quota.' },
  { key: 'preview_ttl_seconds', label: 'Preview idle timeout', unit: 'hours', factor: 3600, min: 0.01, help: 'A preview closes after this long without visits.' },
  { key: 'rate_limit_rps', label: 'Rate limit per flat', unit: 'requests/s', factor: 1, step: 1, min: 1, help: 'Bursts up to twice this rate are allowed.' },
  { key: 'redirect_days', label: 'Redirect after a slug rename', unit: 'days', factor: 1, step: 1, help: 'How long old addresses redirect to the new ones.' },
  { key: 'events_keep', label: 'Log events kept per flat', unit: 'events', factor: 1, step: 1, min: 1, help: 'Older events are deleted.' },
  { key: 'portal_max_relays', label: 'Active Portal relays', unit: 'relays', factor: 1, step: 1, min: 1, portal: true, help: 'How many discovered relays a public flat uses at once. Used when Portal starts.' },
];

// LABELS names every setting in messages.
const LABELS = Object.fromEntries([...FIELDS.map((f) => [f.key, f.label]),
  ['portal_relays', 'Portal relays'], ['portal_discovery', 'Discover Portal relays'], ['portal_hide', 'Hide public flats from relay listings']]);

// Lower values of these settings remove data at the next pruning.
const RETENTION = ['keep_versions', 'events_keep', 'preview_ttl_seconds'];

const SOURCES = { default: 'default', file: 'config.json', flag: 'flag for this run' };

// Read-only config.json values: [key, label, value(config)].
const CONFIG_GROUPS = [
  ['Host', [
    ['host.management_addr', 'Management address', (c) => c.host.management_addr],
    ['host.local_addr', 'Local network address', (c) => c.host.local_addr],
    ['host.console_host', 'Console host name', (c) => c.host.console_host],
    ['host.server_runtime', 'Server flats', (c) => (c.host.server_runtime ? 'on' : 'off')],
  ]],
  ['Network', [
    ['network.private_backend', 'Console and private routes', (c) => c.network.private_backend],
  ]],
  ['Credentials', [
    ['credentials.tailscale_authkey_file', 'Tailscale auth key file', (c) => (c.credentials.tailscale_authkey_file ? 'configured' : 'not configured')],
  ]],
];

export function mount(main, _params, ctx) {
  ctx.setTitle('Settings');
  const providersSlot = h('div', null, loading());
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
      return;
    }
    if (!ctx.alive()) return;
    const showNotice = (cfg) => fill(noticeSlot, changedNotice(cfg));
    const opts = { reload: loadSettings, saved, showNotice };
    showNotice(data.config);
    fill(configSlot, configView(data.config));
    fill(limitsSlot, limitsForm(data, { ...opts, group: 'system' }));
    try {
      const { providers } = await api.providers();
      if (ctx.alive()) fill(providersSlot, providerGroups(providers || [], data, opts));
    } catch (err) {
      if (ctx.alive()) fill(providersSlot, errorPanel(err, 'Cannot load network providers'));
    }
  }
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
};

// providerGroups lists the providers as Private and Public groups. data is
// the settings response: its ETag guards a change, and the Portal panel
// holds the Portal settings.
export function providerGroups(list, data, opts) {
  const group = (scope, title, hint) => h('section', { class: 'provider-group', 'aria-labelledby': 'providers-' + scope },
    h('h3', { id: 'providers-' + scope, text: title }),
    h('p', { class: 'muted small', text: hint }),
    list.filter((p) => p.scope === scope).map((p) => providerPanel(p, data, opts)));
  return [
    h('p', { class: 'muted', text: 'Local is always on. Turn on another provider to make it available, then allow it on each flat that should use it. Turning a provider on does not publish a flat or make it public.' }),
    group('private', 'Private', 'Reachable from this device or your tailnet.'),
    group('public', 'Public', 'Reachable from the internet. A flat goes public only after you approve it.'),
  ];
}

function providerPanel(p, data, opts) {
  const info = PROVIDER_INFO[p.id] || { label: p.id, what: '' };
  const local = p.id === 'local';
  const state = local ? h('span', { class: 'badge badge-ready', text: 'Always on' })
    : h('span', { class: 'badge' + (p.enabled ? ' badge-ready' : ''), text: p.enabled ? 'On' : 'Off' });
  let toggle = null;
  if (!local) {
    toggle = h('button', { type: 'button', class: 'btn btn-small' + (p.enabled ? '' : ' btn-primary'), text: p.enabled ? 'Turn off' : 'Turn on',
      disabled: !!p.locked, 'aria-label': `${p.enabled ? 'Turn off' : 'Turn on'} ${info.label}` });
    toggle.addEventListener('click', () => busy(toggle, async () => {
      if (await setHostProvider(p, info, !p.enabled, data.config ? data.config.etag : '')) await opts.reload();
    }));
  }
  const used = p.flats.length
    ? h('p', { class: 'small' }, 'Allowed on ', p.flats.map((slug, i) => [i ? ', ' : '', h('a', { href: `/flats/${encodeURIComponent(slug)}`, 'data-nav': true, text: slug })]))
    : (local ? null : h('p', { class: 'muted small', text: 'Not allowed on any flat yet.' }));
  return h('article', { class: 'provider' + (p.enabled ? ' is-on' : ''), id: 'provider-' + p.id },
    h('div', { class: 'provider-head' },
      h('div', null, h('h4', null, info.label, state), h('p', { class: 'muted small', text: info.what })),
      toggle),
    p.locked ? h('p', { class: 'muted small', text: p.locked }) : null,
    used,
    providerDetails(p, data, opts));
}

function providerDetails(p, data, opts) {
  if (p.id === 'local') return p.address ? h('dl', { class: 'facts' }, h('dt', { text: 'Address' }), h('dd', null, h('code', { text: p.address }))) : null;
  if (p.id === 'portal') return [limitsForm(data, { ...opts, group: 'portal' }), p.enabled && p.status ? hostsTable(p.status) : null];
  if (!p.enabled) return null;
  if (p.id === 'tailscale') {
    return [
      h('dl', { class: 'facts' }, h('dt', { text: 'Auth key' }),
        h('dd', { text: p.auth_key ? 'From credentials.tailscale_authkey_file.' : 'None. Each new node shows a login link below.' })),
      p.status ? hostsTable(p.status) : null,
    ];
  }
  if (p.id === 'tailscale-funnel') {
    return h('p', { class: 'muted small', text: 'Uses the Tailscale nodes. Your tailnet policy must allow Funnel and HTTPS certificates for them.' });
  }
  return null;
}

async function setHostProvider(p, info, enabled, etag) {
  const ok = await confirmDialog({
    title: `${enabled ? 'Turn on' : 'Turn off'} ${info.label}?`,
    body: enabled ? [
      h('p', { text: info.what }),
      p.scope === 'public' ? h('p', { class: 'alert alert-warn', text: 'A flat made public through this provider can be opened by anyone on the internet. Making a flat public still needs your approval.' }) : null,
      h('p', { class: 'muted', text: 'Saved to config.json and applied now. Nothing is served until you allow it on a flat.' }),
    ] : [h('p', { text: `${info.label} stops being available. Flats that allow it must stop allowing it first.` })],
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

// isDecrease mirrors the server: keep_versions 0 keeps every version.
export function isDecrease(key, current, next) {
  const cur = Number(current);
  const n = Number(next);
  if (key === 'keep_versions') return n !== 0 && (cur === 0 || n < cur);
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
      if (input.value.trim() === '' || !Number.isFinite(n) || n < 0) {
        setError(f.key, `${f.label} must be a number of 0 or more.`);
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
  keep_versions: (n) => `The next deploy of each flat prunes the files of ${plural(n, 'version')}, if the live versions stay live.`,
  events_keep: (n) => `The next cleanup deletes ${plural(n, 'log event')}.`,
  preview_ttl_seconds: (n) => `The next cleanup closes ${plural(n, 'preview')}.`,
};

function impactItem(key, f) {
  const shown = (f.versions || f.previews || []).length;
  const more = f.count > shown && shown ? ` and ${f.count - shown} more` : '';
  if (key === 'keep_versions') return `${f.slug}: ${plural(f.count, 'version')} (${f.versions.join(', ')}${more})`;
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
      h('td', null, statusBadge(a.status)))))));
}

// --- connect an agent ---

function connectAgent() {
  const url = location.origin + '/mcp';
  return [
    h('p', null, 'Flats speaks MCP over Streamable HTTP at ', h('code', { text: url }),
      '. Agents can create flats, save and deploy versions, open previews and read logs. Making a flat public or deleting one always needs your approval here.'),
    copyable(`claude mcp add --transport http flats ${url}`, 'Claude Code'),
    copyable(`# ~/.codex/config.toml\n[mcp_servers.flats]\nurl = "${url}"`, 'Codex'),
    copyable(JSON.stringify({ mcpServers: { flats: { url } } }, null, 2), 'Cursor (~/.cursor/mcp.json or .cursor/mcp.json)'),
    h('h3', { text: 'Command line' }),
    h('p', { class: 'muted', text: 'On the Flats host, the flats CLI uploads a built folder as a new version and deploys it:' }),
    copyable('flats deploy ./dist --flat <slug>', 'CLI'),
    h('p', { class: 'muted small', text: 'Secret values can only be set in this console or with “flats secret set” on the Flats host.' }),
  ];
}

