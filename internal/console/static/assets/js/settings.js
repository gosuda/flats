// Settings: network status, config.json, limits, the approvals queue and
// agent setup.

import { h, timeEl, dateTime, relTime, plural } from './dom.js';
import { api } from './api.js';
import { errorPanel, loading, toast, extLink, busy, fill, copyable } from './ui.js';
import { describeApproval, statusBadge } from './approval.js';

// Limits shown in human units; values are stored as integers in base units.
const FIELDS = [
  { key: 'upload_max_bytes', label: 'Upload size limit', unit: 'MB', factor: 1 << 20, min: 0.001, help: 'Total uncompressed size of one saved version.' },
  { key: 'keep_versions', label: 'Versions to keep', unit: 'versions', factor: 1, step: 1, help: 'Files of older versions are pruned. The live and previewed versions are always kept. 0 keeps every version.' },
  { key: 'disk_quota_bytes', label: 'Disk quota per flat', unit: 'GB', factor: 1 << 30, help: '0 means no quota.' },
  { key: 'preview_ttl_seconds', label: 'Preview idle timeout', unit: 'hours', factor: 3600, min: 0.01, help: 'A preview closes after this long without visits.' },
  { key: 'rate_limit_rps', label: 'Rate limit per flat', unit: 'requests/s', factor: 1, step: 1, min: 1, help: 'Bursts up to twice this rate are allowed.' },
  { key: 'redirect_days', label: 'Redirect after a slug rename', unit: 'days', factor: 1, step: 1, help: 'How long old addresses redirect to the new ones.' },
  { key: 'events_keep', label: 'Log events kept per flat', unit: 'events', factor: 1, step: 1, min: 1, help: 'Older events are deleted.' },
  { key: 'portal_max_relays', label: 'Active Portal relays', unit: 'relays', factor: 1, step: 1, min: 1, portal: true, help: 'How many discovered relays a public flat uses at once. Applies after Flats restarts.' },
];

// LABELS names every setting in messages.
const LABELS = Object.fromEntries([...FIELDS.map((f) => [f.key, f.label]),
  ['portal_relays', 'Portal relays'], ['portal_discovery', 'Discover Portal relays']]);

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
    ['network.permitted', 'Permitted providers', (c) => ((c.network.permitted || []).length ? c.network.permitted.join(', ') : 'none (Local only)')],
    ['network.private_backend', 'Private network', (c) => c.network.private_backend],
  ]],
  ['Credentials', [
    ['credentials.operator_file', 'Operator credential file', (c) => (c.credentials.operator_file ? 'configured' : 'not configured')],
    ['credentials.tailscale_authkey_file', 'Tailscale auth key file', (c) => (c.credentials.tailscale_authkey_file ? 'configured' : 'not configured')],
  ]],
];

const NET_NAMES = { tsnet: 'Tailscale', tailscale: 'Tailscale', portal: 'Portal', local: 'Local (loopback, no Tailscale)', 'local-public': 'Local public stand-in' };

export function mount(main, _params, ctx) {
  ctx.setTitle('Settings');
  const netSlot = h('div', null, loading());
  const noticeSlot = h('div');
  const configSlot = h('div', null, loading());
  const limitsSlot = h('div', null, loading());
  const approvalsSlot = h('div', null, loading());

  const card = (title, id, ...children) => h('section', { class: 'card', id, 'aria-labelledby': id + '-title' },
    h('h2', { id: id + '-title', text: title }), ...children);

  main.appendChild(h('section', { class: 'page' },
    h('div', { class: 'page-head' }, h('h1', { text: 'Settings' })),
    noticeSlot,
    card('Networks', 'networks', netSlot),
    card('Host configuration', 'configuration', configSlot),
    card('Limits and Portal', 'limits', limitsSlot),
    card('Approvals', 'approvals', approvalsSlot),
    card('Connect an agent', 'connect', connectAgent())));

  api.status().then((s) => ctx.alive() && fill(netSlot, networks(s.system)))
    .catch((err) => ctx.alive() && fill(netSlot, errorPanel(err, 'Cannot load network status')));
  loadSettings();
  api.approvals().then((r) => ctx.alive() && fill(approvalsSlot, approvals(r.approvals || [])))
    .catch((err) => ctx.alive() && fill(approvalsSlot, errorPanel(err, 'Cannot load approvals')));

  // loadSettings renders config.json and the limits form; saved is the
  // result of the save that triggered the reload, if any.
  async function loadSettings(saved) {
    try {
      const data = await api.settings();
      if (!ctx.alive()) return;
      const showNotice = (cfg) => fill(noticeSlot, changedNotice(cfg));
      showNotice(data.config);
      fill(configSlot, configView(data.config));
      fill(limitsSlot, limitsForm(data, { reload: loadSettings, saved, showNotice }));
    } catch (err) {
      if (!ctx.alive()) return;
      fill(configSlot, errorPanel(err, 'Cannot load settings'));
      fill(limitsSlot);
    }
  }
}

// --- networks ---

// isNet matches core.NetStatus. Go encodes an empty host list as null, so a
// network that serves nothing yet has "hosts": null.
export function isNet(v) {
  if (!v || typeof v !== 'object' || Array.isArray(v) || !('hosts' in v)) return false;
  return Array.isArray(v.hosts) || (v.hosts === null && typeof v.kind === 'string');
}

// networks renders the server's system status. Objects with a hosts list
// are drawn as networks; other fields as facts, so new fields still show.
export function networks(sys) {
  if (!sys || typeof sys !== 'object') return h('p', { class: 'muted', text: 'The server did not report network status.' });
  const nets = [];
  const facts = [];
  for (const [k, v] of Object.entries(sys)) {
    if (isNet(v)) nets.push([k, v]);
    else if (Array.isArray(v) && v.length && v.every(isNet)) v.forEach((n, i) => nets.push([`${k} ${i + 1}`, n]));
    else if (v !== null && v !== undefined && v !== '') facts.push([k, v]);
  }
  return [
    facts.length ? h('dl', { class: 'facts' }, facts.map(([k, v]) => [h('dt', { text: humanKey(k) }), h('dd', null, factValue(v))])) : null,
    ...nets.map(([k, n]) => network(k, n)),
  ];
}

function humanKey(k) {
  const s = k.replace(/_/g, ' ');
  return s.charAt(0).toUpperCase() + s.slice(1);
}

function factValue(v) {
  if (Array.isArray(v)) return v.length ? h('ul', { class: 'compact' }, v.map((x) => h('li', null, factValue(x)))) : h('span', { class: 'muted', text: 'none' });
  if (v && typeof v === 'object') return h('dl', { class: 'facts nested' }, Object.entries(v).map(([k, x]) => [h('dt', { text: humanKey(k) }), h('dd', null, factValue(x))]));
  if (typeof v === 'boolean') return v ? 'yes' : 'no';
  return linkify(String(v));
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

function network(key, n) {
  const title = NET_NAMES[n.kind] || NET_NAMES[key] || humanKey(key);
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
        login ? h('div', null, extLink(login, 'Log in again', 'btn btn-small btn-primary')) : null,
        x.url ? extLink(x.url) : null,
        x.detail ? h('div', { class: 'muted small' }, linkify(x.detail)) : null));
  });
  return h('div', { class: 'net' },
    h('h3', null, title, ' ', h('span', { class: 'badge ' + (n.enabled ? 'badge-ready' : ''), text: n.enabled ? 'enabled' : 'disabled' })),
    n.detail ? h('p', { class: 'muted' }, linkify(n.detail)) : null,
    hosts.length
      ? h('div', { class: 'table-wrap' }, h('table', { class: 'table' },
        h('thead', null, h('tr', null, ['Host', 'State', 'Key expiry', 'Address'].map((t) => h('th', { scope: 'col', text: t })))),
        h('tbody', null, rows)))
      : h('p', { class: 'muted', text: 'No hosts are served on this network.' }));
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
      ' while Flats is stopped. They apply after Flats restarts.'),
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

// limitsForm edits the system and Portal settings. opts: reload(saved),
// saved (the last save response), showNotice(config).
function limitsForm(data, opts) {
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

  const save = h('button', { type: 'submit', id: 'settings-save', class: 'btn btn-primary', text: 'Save settings' });
  const result = h('div', { class: 'muted small', role: 'status' }, savedSummary(opts.saved));
  const errorSlot = h('div');
  const impactSlot = h('div');
  const form = h('form', { class: 'settings-form', novalidate: true },
    h('fieldset', { class: 'field-group' }, h('legend', { text: 'System' }),
      h('div', { class: 'form-grid' }, FIELDS.filter((f) => !f.portal).map(numberField))),
    h('fieldset', { class: 'field-group' }, h('legend', { text: 'Portal' }),
      h('div', { class: 'form-grid' },
        h('div', { class: 'field field-wide' },
          h('label', { for: relaysId, text: 'Portal relays' }), relays,
          control('portal_relays', relays, 'One relay per line. Leave empty to use Portal’s defaults (relay discovery, up to 3 active relays). Applies after Flats restarts.')),
        h('div', { class: 'field field-wide field-check' },
          discover, h('label', { for: discoverId, text: 'Discover Portal relays' }),
          control('portal_discovery', discover, 'Find further relays automatically (Portal’s default). Applies after Flats restarts.')),
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
    const relayList = relays.value.split(/[\n,]/).map((s) => s.trim()).filter(Boolean);
    if (!pinned.portal_relays && relayList.join('\n') !== relaysInitial) out.portal_relays = relayList.join(',');
    if (!pinned.portal_discovery && discover.checked !== discoverInitial) out.portal_discovery = String(discover.checked);
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

  // Any edit invalidates a shown impact; saving checks it again.
  form.addEventListener('input', () => fill(impactSlot));
  form.addEventListener('change', () => fill(impactSlot));
  form.addEventListener('submit', (e) => {
    e.preventDefault();
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
        showError(err);
        return;
      }
      if (!Object.keys(impact).length) return store(out);
      fill(impactSlot, impactPanel(impact, () => store(out), () => {
        fill(impactSlot);
        controls[lower[0]].input.focus();
      }));
    });
  });
  return form;
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
  keep_versions: (n) => `The next deploy of each flat prunes the files of ${plural(n, 'version')}.`,
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

