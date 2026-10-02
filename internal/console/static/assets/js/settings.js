// Settings: network status, limits, the approvals queue and agent setup.

import { h, timeEl, dateTime, relTime } from './dom.js';
import { api } from './api.js';
import { errorPanel, loading, toast, extLink, busy, fill, copyable } from './ui.js';
import { describeApproval, statusBadge } from './approval.js';

// Limits shown in human units; values are stored as integers in base units.
const FIELDS = [
  { key: 'upload_max_bytes', label: 'Upload size limit', unit: 'MB', factor: 1 << 20, min: 0.001, help: 'Total uncompressed size of one saved version.' },
  { key: 'keep_versions', label: 'Versions to keep', unit: 'versions', factor: 1, step: 1, help: 'Files of older versions are pruned. The live and previewed versions are always kept.' },
  { key: 'disk_quota_bytes', label: 'Disk quota per flat', unit: 'GB', factor: 1 << 30, help: '0 means no quota.' },
  { key: 'preview_ttl_seconds', label: 'Preview idle timeout', unit: 'hours', factor: 3600, min: 0.01, help: 'A preview closes after this long without visits.' },
  { key: 'rate_limit_rps', label: 'Rate limit per flat', unit: 'requests/s', factor: 1, step: 1, min: 1, help: 'Bursts up to twice this rate are allowed.' },
  { key: 'redirect_days', label: 'Redirect after a slug rename', unit: 'days', factor: 1, step: 1, help: 'How long old addresses redirect to the new ones.' },
  { key: 'events_keep', label: 'Log events kept per flat', unit: 'events', factor: 1, step: 1, min: 1, help: 'Older events are deleted.' },
  { key: 'portal_max_relays', label: 'Active Portal relays', unit: 'relays', factor: 1, step: 1, min: 1, help: 'How many discovered relays a public flat uses at once. Applies after `flats serve` restarts.' },
];

const NET_NAMES = { tsnet: 'Tailscale', tailscale: 'Tailscale', portal: 'Portal', local: 'Local (loopback, no Tailscale)', 'local-public': 'Local public stand-in' };

export function mount(main, _params, ctx) {
  ctx.setTitle('Settings');
  const netSlot = h('div', null, loading());
  const limitsSlot = h('div', null, loading());
  const approvalsSlot = h('div', null, loading());

  const card = (title, id, ...children) => h('section', { class: 'card', id, 'aria-labelledby': id + '-title' },
    h('h2', { id: id + '-title', text: title }), ...children);

  main.appendChild(h('section', { class: 'page' },
    h('div', { class: 'page-head' }, h('h1', { text: 'Settings' })),
    card('Networks', 'networks', netSlot),
    card('Limits', 'limits', limitsSlot),
    card('Approvals', 'approvals', approvalsSlot),
    card('Connect an agent', 'connect', connectAgent())));

  api.status().then((s) => ctx.alive() && fill(netSlot, networks(s.system)))
    .catch((err) => ctx.alive() && fill(netSlot, errorPanel(err, 'Cannot load network status')));
  loadLimits();
  api.approvals().then((r) => ctx.alive() && fill(approvalsSlot, approvals(r.approvals || [])))
    .catch((err) => ctx.alive() && fill(approvalsSlot, errorPanel(err, 'Cannot load approvals')));

  async function loadLimits() {
    try {
      const { settings, defaults } = await api.settings();
      if (ctx.alive()) fill(limitsSlot, limitsForm(settings, defaults || {}, loadLimits));
    } catch (err) {
      if (ctx.alive()) fill(limitsSlot, errorPanel(err, 'Cannot load settings'));
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

// --- limits ---

function display(base, factor) {
  const n = Number(base) / factor;
  return Number.isFinite(n) ? String(Math.round(n * 1000) / 1000) : '';
}

function limitsForm(settings, defaults, reload) {
  const inputs = [];
  const fields = FIELDS.map((f) => {
    const id = 'set-' + f.key;
    const initial = display(settings[f.key], f.factor);
    const input = h('input', {
      id, type: 'number', inputmode: 'decimal', min: String(f.min ?? 0), step: f.step ? String(f.step) : 'any',
      value: initial, required: true, 'aria-describedby': id + '-help',
    });
    inputs.push({ f, input, initial });
    const def = defaults[f.key] !== undefined ? ` Default: ${display(defaults[f.key], f.factor)} ${f.unit}.` : '';
    return h('div', { class: 'field' },
      h('label', { for: id, text: f.label }),
      h('div', { class: 'with-unit' }, input, h('span', { class: 'unit', text: f.unit })),
      h('span', { class: 'hint', id: id + '-help', text: f.help + def }));
  });
  const relaysId = 'set-portal_relays';
  const relaysInitial = (settings.portal_relays || '').split(',').map((s) => s.trim()).filter(Boolean).join('\n');
  const relays = h('textarea', { id: relaysId, rows: '3', spellcheck: 'false', class: 'mono', 'aria-describedby': relaysId + '-help', placeholder: 'https://relay.example.com' });
  relays.value = relaysInitial;
  const discoverId = 'set-portal_discovery';
  const discoverInitial = String(settings.portal_discovery ?? 'true') !== 'false';
  const discover = h('input', { id: discoverId, type: 'checkbox', checked: discoverInitial, 'aria-describedby': discoverId + '-help' });
  const save = h('button', { type: 'submit', class: 'btn btn-primary', text: 'Save limits' });
  const result = h('p', { class: 'muted small', role: 'status' });
  const form = h('form', { class: 'form-grid' },
    fields,
    h('div', { class: 'field field-wide' },
      h('label', { for: relaysId, text: 'Portal relays' }), relays,
      h('span', { class: 'hint', id: relaysId + '-help', text: 'One relay per line. Leave empty to use Portal’s defaults (relay discovery, up to 3 active relays). Applies after `flats serve` restarts.' })),
    h('div', { class: 'field field-wide field-check' },
      discover, h('label', { for: discoverId, text: 'Discover Portal relays' }),
      h('span', { class: 'hint', id: discoverId + '-help', text: 'Find further relays automatically (Portal’s default). Applies after `flats serve` restarts.' })),
    h('div', { class: 'field field-wide actions' }, result, save));
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    const out = {};
    for (const { f, input, initial } of inputs) {
      if (input.value === initial) continue;
      const n = Number(input.value);
      if (!Number.isFinite(n) || n < 0) {
        input.focus();
        toast(`${f.label} must be a non-negative number.`, 'error');
        return;
      }
      out[f.key] = String(Math.round(n * f.factor));
    }
    const relayList = relays.value.split(/[\n,]/).map((s) => s.trim()).filter(Boolean);
    if (relayList.join('\n') !== relaysInitial) out.portal_relays = relayList.join(',');
    if (discover.checked !== discoverInitial) out.portal_discovery = String(discover.checked);
    if (!Object.keys(out).length) { result.textContent = 'Nothing changed.'; return; }
    busy(save, async () => {
      try {
        const res = await api.saveSettings(out);
        const later = (res && res.restart_required) || [];
        toast(later.length ? `Settings saved. ${res.note || 'Restart flats serve to apply ' + later.join(', ') + '.'}` : 'Settings saved.', later.length ? 'info' : 'success');
        reload();
      } catch (err) {
        result.textContent = '';
        toast(err.message, 'error');
      }
    });
  });
  return form;
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

