// Flat management page. One page per flat with tabs (site.js): Deployments
// (status, versions, previews and logs) and Settings (name, slug, sharing,
// networks, environment variables, secrets, server permissions and delete).
// Analytics and Database are their own modules under the same tabs.

import { h, icon, timeEl, dateTime, bytes, plural, shortHash, VISIBILITY, visibilityOf, publicURL, publicNoticeOf } from './dom.js';
import { api } from './api.js';
import { confirmDialog, errorPanel, loading, toast, extLink, busy, fill } from './ui.js';
import { deployVersion, publishDraft, previewVersion, deleteFlat, setProvider, redeployLive, PROVIDERS } from './actions.js';
import { siteHeader } from './site.js';
import { shareDialog } from './share.js';

const LOG_POLL_MS = 5000;
const LOG_KEEP = 500;

function section(title, id, ...children) {
  const hid = id + '-title';
  return h('section', { class: 'settings-section', id, 'aria-labelledby': hid },
    h('h2', { id: hid, text: title }), ...children);
}

export function mountSettings(main, params, ctx) { return mount(main, params, ctx, 'settings'); }

export function mount(main, [slug], ctx, tab = 'deployments') {
  const settings = tab === 'settings';
  ctx.setTitle(slug);
  let flat = null;
  const timers = [];
  let hostProviders = null; // provider id -> turned on for the host; null until loaded

  const headSlot = h('div', null, loading());
  const netSlot = h('div');
  const versionsSlot = h('div', null, loading());
  const previewsSlot = h('div', null, loading());
  const envSlot = h('div', null, loading());
  const networkSlot = h('div', null, loading());
  const secretsSlot = h('div', null, loading());
  const logs = settings ? { poll() {}, start() {}, stop() {} } : logsPanel(slug, ctx, timers);

  const page = h('section', { class: 'page site-page' },
    h('a', { class: 'back', href: '/', 'data-nav': true }, icon('back'), 'All flats'),
    headSlot);
  main.appendChild(page);

  async function loadFlat() {
    try {
      flat = await api.flat(slug);
    } catch (err) {
      if (!ctx.alive()) return;
      fill(headSlot, errorPanel(err, err.status === 404 ? `There is no flat called “${slug}”` : 'Cannot load this flat'));
      if (err.status === 404) {
        headSlot.appendChild(h('p', { class: 'muted', text: 'It may have been renamed or deleted. Renamed flats keep redirecting their old addresses, but not this console page.' }));
      }
      return false;
    }
    if (!ctx.alive()) return false;
    ctx.setTitle(`${flat.name || flat.slug} · ${settings ? 'Settings' : 'Deployments'}`);
    drawHead();
    if (settings) drawNetworks();
    return true;
  }

  // Everything that a deploy, rollback or visibility change can affect.
  async function refresh() {
    const ok = await loadFlat();
    if (ok) {
      if (settings) { loadEnv(); loadSecrets(); loadNetwork(); loadHostProviders(); }
      else { loadVersions(); loadPreviews(); logs.poll(); }
    }
    return ok;
  }

  // --- header ---

  function drawHead() {
    if (settings) { drawSettings(); return; }
    const urls = h('dl', { class: 'facts' });
    const add = (k, v) => urls.append(h('dt', { text: k }), h('dd', null, v));
    const pending = flat.private_state && !['ready', 'key-expiring'].includes(flat.private_state);
    add('Private URL', flat.live_version
      ? h('div', null, extLink(flat.private_url),
          pending ? h('div', { class: 'muted small' },
            h('span', { class: 'badge badge-' + flat.private_state, text: flat.private_state }), ' ',
            flat.private_detail || 'not answering yet') : null)
      : h('span', { class: 'muted' }, h('code', { text: flat.private_url }), ' (online after the first deploy)'));
    const pub = publicURL(flat);
    if (pub) {
      add('Public URL', h('div', null,
        flat.live_version ? extLink(pub) : h('code', { text: pub }),
        h('p', { class: 'notice-text', text: publicNoticeOf(flat) })));
    }
    add('Live', flat.live ? `Version ${flat.live.number} · ${flat.live.kind}` : 'Not published');
    add('Created', dateTime(flat.created_at));
    if (flat.old_slug && flat.old_slug_until) {
      add('Redirect', `Old slug ${flat.old_slug} redirects here until ${dateTime(flat.old_slug_until)}`);
    }

    fill(headSlot, siteHeader(flat, 'deployments'),
      section('Status', 'status', urls),
      section('Versions', 'versions', versionsSlot),
      section('Open previews', 'previews', previewsSlot),
      section('Logs', 'logs', logs.el));
  }

  function drawSettings() {
    const name = h('input', { id: 'site-name', value: flat.name || '', maxlength: '200', required: true });
    const save = h('button', { class: 'btn btn-small', type: 'submit', text: 'Save' });
    const nameForm = h('form', { class: 'site-name-form' }, name, save);
    nameForm.addEventListener('submit', (e) => {
      e.preventDefault();
      busy(save, async () => {
        try { await api.setName(slug, name.value.trim()); await loadFlat(); toast('Name saved.', 'success'); }
        catch (err) { toast(err.message, 'error'); }
      });
    });
    const change = h('button', { class: 'btn btn-small', type: 'button', text: 'Change' });
    const rename = h('div', { class: 'site-rename', hidden: true }, renameForm());
    change.addEventListener('click', () => { rename.hidden = !rename.hidden; if (!rename.hidden) rename.querySelector('input').focus(); });
    const manage = h('button', { class: 'btn btn-small', type: 'button', text: 'Manage' });
    manage.addEventListener('click', () => shareDialog(flat, refresh));
    const row = (title, hint, control) => h('div', { class: 'setting-row' },
      h('div', null, h('label', { for: title === 'Name' ? 'site-name' : undefined, text: title }),
        hint ? h('div', { class: 'muted small', text: hint }) : null), control);
    fill(headSlot, siteHeader(flat, 'settings'),
      h('section', { class: 'settings-section', 'aria-labelledby': 'general-title' },
        h('h2', { id: 'general-title', text: 'General' }),
        row('Name', 'Name for your site', nameForm),
        row('URL', 'Web address', h('div', { class: 'setting-control' }, extLink(flat.public_url || flat.private_url), change)), rename,
        row('Custom domain', 'Custom domains are not supported on this host yet.', h('span', { class: 'muted small', text: 'Unavailable' })),
        row('Sharing', 'Who can view your site', h('div', { class: 'setting-control' }, h('span', { class: 'muted', text: VISIBILITY[visibilityOf(flat.visibility)].label }), manage))),
      netSlot,
      h('section', { class: 'settings-section', id: 'env', 'aria-labelledby': 'env-title' }, h('h2', { id: 'env-title', text: 'Ordinary environment variables' }), envSlot),
      h('section', { class: 'settings-section', id: 'secrets', 'aria-labelledby': 'secrets-title' }, h('h2', { id: 'secrets-title', text: 'Secrets' }), secretsSlot),
      h('section', { class: 'settings-section', id: 'network', 'aria-labelledby': 'network-title' }, h('h2', { id: 'network-title', text: 'Server HTTP(S) permissions' }), networkSlot),
      h('section', { class: 'settings-section', id: 'delete' }, h('h2', { text: 'Danger zone' }), deleteBlock()));
  }

  // --- networks ---

  // Who can open the flat is changed in the sharing dialog; this section
  // lists the networks that serve it.
  function drawNetworks() {
    fill(netSlot, section('Networks', 'networks',
      h('p', { class: 'muted small' }, 'Private flats are reachable on this device and, when Tailscale is allowed, on your tailnet. Public flats are served through Tailscale Funnel or Portal. A network must first be turned on in ',
        h('a', { href: '/settings#providers', 'data-nav': true, text: 'Settings' }), '.'),
      h('div', { class: 'networks-group' }, h('h4', { text: 'Private' }),
        h('ul', { class: 'networks' },
          h('li', null, h('input', { type: 'checkbox', id: 'net-local', checked: true, disabled: true }),
            h('label', { for: 'net-local' }, 'Local', h('span', { class: 'muted small', text: 'This device, always on' }))),
          PROVIDERS.filter((p) => p.scope === 'private').map(networkRow))),
      h('div', { class: 'networks-group' }, h('h4', { text: 'Public' }),
        h('ul', { class: 'networks' }, PROVIDERS.filter((p) => p.scope === 'public').map(networkRow)))));
  }

  function networkRow(p) {
    const id = 'net-' + p.id;
    const allowed = (flat.providers || []).includes(p.id);
    // A provider that is off for the host can only be unchecked here.
    const off = hostProviders !== null && !hostProviders[p.id];
    const box = h('input', { type: 'checkbox', id, checked: allowed, disabled: off && !allowed });
    box.addEventListener('change', async () => {
      box.disabled = true;
      const ok = await setProvider(flat, p.id, box.checked);
      box.disabled = false;
      // A grant whose route failed to open is still recorded, so redraw
      // from the server after a failure as well.
      if (!(await refresh()) && !ok) box.checked = allowed;
    });
    return h('li', null, box, h('label', { for: id }, p.label,
      h('span', { class: 'muted small', text: off ? 'Off in Settings' : p.hint })));
  }

  async function loadHostProviders() {
    try {
      const { providers } = await api.providers();
      hostProviders = Object.fromEntries((providers || []).map((p) => [p.id, p.enabled]));
      if (ctx.alive() && flat) drawNetworks();
    } catch { /* keep every provider selectable; the server still enforces host grants */ }
  }

  // --- versions ---

  async function loadVersions() {
    try {
      const { versions } = await api.versions(slug);
      if (ctx.alive()) drawVersions((versions || []).slice().sort((a, b) => b.number - a.number));
    } catch (err) {
      if (ctx.alive()) fill(versionsSlot, errorPanel(err, 'Cannot load versions'));
    }
  }

  function drawVersions(vs) {
    const draft = flat.draft && flat.draft.dirty ? flat.draft : null;
    if (!vs.length && !draft) {
      fill(versionsSlot, h('p', { class: 'muted', text: 'No saved versions yet. Agents save versions with save_version (MCP) or `flats deploy`.' }));
      return;
    }
    const live = flat.live_version;
    const rows = vs.map((v) => {
      const actions = h('div', { class: 'cell-actions' });
      const prev = h('button', { type: 'button', class: 'btn btn-small', text: 'Preview', disabled: v.pruned, 'aria-label': `Preview version ${v.number}` });
      prev.addEventListener('click', () => busy(prev, async () => { if (await previewVersion(flat, v.number)) loadPreviews(); }));
      actions.appendChild(prev);
      if (v.number === live) {
        actions.appendChild(redeployButton());
      } else {
        const back = live && v.number < live;
        const btn = h('button', {
          type: 'button', class: 'btn btn-small' + (back ? '' : ' btn-primary'), disabled: v.pruned,
          text: back ? 'Roll back' : 'Deploy', 'aria-label': `${back ? 'Roll back to' : 'Deploy'} version ${v.number}`,
        });
        btn.addEventListener('click', () => busy(btn, async () => {
          if (await deployVersion(flat, v.number, back ? 'rollback' : 'deploy')) refresh();
        }));
        actions.appendChild(btn);
      }
      return h('tr', { class: v.number === live ? 'is-live' : '' },
        h('th', { scope: 'row' }, `v${v.number}`, v.number === live ? h('span', { class: 'badge badge-live', text: 'Live' }) : null),
        h('td', null, timeEl(v.created_at)),
        h('td', null, h('code', { title: v.hash, text: shortHash(v.hash) })),
        h('td', null, v.git_sha ? h('code', { title: v.git_sha, text: v.git_sha.slice(0, 7) }) : h('span', { class: 'muted', text: '—' }),
          v.git_dirty ? h('span', { class: 'badge badge-warn', text: 'dirty', title: 'Uncommitted changes when saved' }) : null),
        h('td', { class: 'msg' }, v.message || h('span', { class: 'muted', text: '—' })),
        h('td', null, bytes(v.size), h('div', { class: 'muted small', text: `${plural(v.files, 'file')} · ${v.kind}` }),
          v.pruned ? h('span', { class: 'badge', text: 'files pruned', title: 'Removed by retention; cannot be deployed or previewed' }) : null),
        h('td', null, actions));
    });
    fill(versionsSlot, h('div', { class: 'table-wrap' },
      h('table', { class: 'table table-versions' },
        h('caption', { class: 'sr-only', text: 'Draft and published versions, newest first' }),
        h('thead', null, h('tr', null, ['Version', 'Created', 'Hash', 'Git', 'Message', 'Size', 'Actions'].map((t) => h('th', { scope: 'col', text: t })))),
        h('tbody', null, draft ? draftRow(draft) : null, rows))));
  }

  // draftRow is the agent's latest save that is not published yet.
  function draftRow(d) {
    const prev = h('button', { type: 'button', class: 'btn btn-small', text: 'Preview', 'aria-label': 'Preview the draft' });
    prev.addEventListener('click', () => busy(prev, async () => { if (await previewVersion(flat, 0)) loadPreviews(); }));
    const pub = h('button', { type: 'button', class: 'btn btn-small btn-primary', text: 'Publish', 'aria-label': 'Publish the draft' });
    pub.addEventListener('click', () => busy(pub, async () => { if (await publishDraft(flat)) refresh(); }));
    return h('tr', { class: 'is-draft' },
      h('th', { scope: 'row' }, 'Draft', h('span', { class: 'badge badge-draft', text: 'New' })),
      h('td', null, timeEl(d.updated_at)),
      h('td', null, h('code', { title: d.hash, text: shortHash(d.hash) })),
      h('td', null, d.git_sha ? h('code', { title: d.git_sha, text: d.git_sha.slice(0, 7) }) : h('span', { class: 'muted', text: '—' }),
        d.git_dirty ? h('span', { class: 'badge badge-warn', text: 'dirty', title: 'Uncommitted changes when saved' }) : null),
      h('td', { class: 'msg' }, d.message || h('span', { class: 'muted', text: '—' })),
      h('td', null, bytes(d.size), h('div', { class: 'muted small', text: `${plural(d.files, 'file')} · ${d.kind}` })),
      h('td', null, h('div', { class: 'cell-actions' }, prev, pub)));
  }

  // redeployButton restarts the live version, e.g. after environment settings changed.
  function redeployButton() {
    const btn = h('button', {
      type: 'button', class: 'btn btn-small', text: 'Redeploy (apply environment)',
      'aria-label': `Redeploy live version ${flat.live_version} to apply environment`,
    });
    btn.addEventListener('click', () => busy(btn, async () => { if (await redeployLive(flat)) refresh(); }));
    return btn;
  }

  // --- previews ---

  async function loadPreviews() {
    try {
      const { previews } = await api.previews(slug);
      if (!ctx.alive()) return;
      if (!previews || !previews.length) {
        fill(previewsSlot, h('p', { class: 'muted', text: 'No open previews. Previews close when the flat is deployed or after a day without visits.' }));
        return;
      }
      fill(previewsSlot, h('ul', { class: 'plain-list' }, previews.map((p) => {
        const close = h('button', { type: 'button', class: 'btn btn-small', text: 'Close', 'aria-label': `Close preview ${p.host}` });
        close.addEventListener('click', () => busy(close, async () => {
          try { await api.closePreview(p.host); toast('Preview closed.', 'success'); } catch (err) { toast(err.message, 'error'); }
          loadPreviews();
        }));
        return h('li', { class: 'plain-row' },
          h('div', null, h('strong', { text: p.version ? `v${p.version}` : 'Draft' }), ' ', extLink(p.url),
            h('div', { class: 'muted small' }, 'Opened ', timeEl(p.created_at), ` · closes if unvisited until ${dateTime(p.expires_at)}`)),
          close);
      })));
    } catch (err) {
      if (ctx.alive()) fill(previewsSlot, errorPanel(err, 'Cannot load previews'));
    }
  }

  // --- operator-managed outbound HTTP(S) origins ---

  async function loadNetwork() {
    try {
      const policy = await api.network(slug);
      if (!ctx.alive()) return;
      const origins = h('textarea', { id: 'network-origins', rows: '5', spellcheck: 'false', placeholder: 'https://api.example.com', value: (policy.origins || []).join('\n') });
      const save = h('button', { type: 'submit', class: 'btn btn-primary btn-small', text: 'Save server origins' });
      const clear = h('button', { type: 'button', class: 'btn btn-small btn-danger-text', text: 'Clear server permissions' });
      const form = h('form', { class: 'form-grid' }, h('label', { for: 'network-origins', text: 'Allowed origins (one per line)' }), origins, h('div', { class: 'form-actions' }, save, clear));
      form.addEventListener('submit', (e) => {
        e.preventDefault();
        busy(save, async () => {
          const list = origins.value.split(/\r?\n/).map((v) => v.trim()).filter(Boolean);
          if (!list.length) { toast('Use Clear server permissions to remove every origin.', 'error'); return; }
          try { await api.setNetwork(slug, list); toast('Server origins saved. Redeploy to apply the changed permissions.', 'success'); loadNetwork(); }
          catch (err) { toast(err.message, 'error'); }
        });
      });
      clear.addEventListener('click', () => busy(clear, async () => {
        if (!await confirmDialog({ title: 'Clear server permissions?', body: 'Running workers and automatic restarts retain their captured grants. Redeploy the live version after clearing to revoke its server access.', confirmLabel: 'Clear permissions', danger: true })) return;
        try { await api.setNetwork(slug, []); toast('Server permissions cleared. Redeploy to revoke live access.', 'success'); loadNetwork(); }
        catch (err) { toast(err.message, 'error'); }
      }));
      fill(networkSlot,
        h('p', { class: 'muted', text: 'Only the operator grants JavaScript server fetch access. WASI receives no outbound network capability. Permit at most 32 exact public HTTP:80 or HTTPS:443 origins, one per line. No wildcards, paths, credentials or private targets. An empty list denies server fetch.' }),
        form,
        h('p', { class: 'muted small', text: 'Changes apply on the next deploy/redeploy, rollback or data restoration after any required approval, Flats host restart, or new preview. Running workers and automatic restarts retain captured grants; after clearing, redeploy to revoke live access. Browser fetch follows browser CORS/CSP independently and receives no injected secrets.' }),
        flat.live_version ? redeployButton() : h('p', { class: 'muted small', text: 'Applied when this flat is first published.' }));
    } catch (err) { if (ctx.alive()) fill(networkSlot, errorPanel(err, 'Cannot load server network permissions')); }
  }

  // --- ordinary environment variables ---

  async function loadEnv() {
    try {
      const res = await api.env(slug);
      if (ctx.alive()) drawEnv(res.env || []);
    } catch (err) {
      if (ctx.alive()) fill(envSlot, errorPanel(err, 'Cannot load environment variables'));
    }
  }

  function drawEnv(list) {
    const name = h('input', {
      id: 'env-name', type: 'text', required: true, pattern: '[A-Z_][A-Z0-9_]*', maxlength: '64',
      autocomplete: 'off', spellcheck: 'false', autocapitalize: 'characters', class: 'mono', placeholder: 'APP_MODE',
    });
    const value = h('textarea', { id: 'env-value', rows: '2', autocomplete: 'off', spellcheck: 'false', class: 'mono' });
    const save = h('button', { type: 'submit', class: 'btn btn-primary btn-small', text: 'Save variable' });
    name.addEventListener('input', () => { name.value = name.value.toUpperCase(); });
    const form = h('form', { class: 'form-grid', id: 'env-form' },
      h('div', { class: 'field' }, h('label', { for: 'env-name', text: 'Name' }), name,
        h('span', { class: 'hint', text: 'Capital letters, digits and _. Reserved names and secret names cannot be used.' })),
      h('div', { class: 'field' }, h('label', { for: 'env-value', text: 'Value' }), value,
        h('span', { class: 'hint', text: 'Plain text; an empty value is allowed' })),
      h('div', { class: 'field field-end' }, save));
    form.addEventListener('submit', (e) => {
      e.preventDefault();
      busy(save, async () => {
        try {
          await api.putEnv(slug, name.value, value.value);
          toast(`Variable ${name.value} saved. It applies on the next approved activation or Flats host restart.`, 'success');
          loadEnv(); logs.poll();
        } catch (err) { toast(err.message, 'error'); }
      });
    });
    const add = h('button', { type: 'button', class: 'btn btn-small', text: 'Add variable', 'aria-expanded': 'false', 'aria-controls': 'env-form' });
    form.hidden = true;
    add.addEventListener('click', () => {
      form.hidden = !form.hidden;
      add.setAttribute('aria-expanded', String(!form.hidden));
      if (!form.hidden) name.focus();
    });
    const items = list.length ? h('ul', { class: 'plain-list' }, list.map((v) => {
      const edit = h('button', { type: 'button', class: 'btn btn-small', text: 'Edit', 'aria-label': `Edit variable ${v.name}` });
      edit.addEventListener('click', () => {
        form.hidden = false; add.setAttribute('aria-expanded', 'true');
        name.value = v.name; value.value = v.value; value.focus();
      });
      const del = h('button', { type: 'button', class: 'btn btn-small btn-danger-text', text: 'Delete', 'aria-label': `Delete variable ${v.name}` });
      del.addEventListener('click', () => busy(del, async () => {
        const ok = await confirmDialog({
          title: `Delete variable ${v.name}?`,
          body: 'The running version keeps its current environment until the next approved activation or Flats host restart.',
          confirmLabel: 'Delete', danger: true,
        });
        if (!ok) return;
        try {
          await api.deleteEnv(slug, v.name);
          toast(`Variable ${v.name} deleted. It applies on the next approved activation or Flats host restart.`, 'success');
          loadEnv();
        } catch (err) { toast(err.message, 'error'); }
      }));
      return h('li', { class: 'plain-row' },
        h('div', null, h('code', { text: v.name }),
          h('pre', { class: 'env-value', text: v.value === '' ? '(empty)' : v.value }),
          h('div', { class: 'muted small' }, 'Updated ', timeEl(v.updated_at))),
        h('div', { class: 'cell-actions' }, edit, del));
    })) : h('p', { class: 'muted', text: 'No ordinary environment variables.' });
    const apply = flat.live_version
      ? h('div', { class: 'inline-form' },
        h('span', { class: 'muted', text: `Changes apply on the next approved activation or Flats host restart: redeploy the live version ${flat.live_version} to use them now.` }), redeployButton())
      : h('p', { class: 'muted', text: 'Changes apply when a deployment is approved.' });
    fill(envSlot, add,
      h('p', { class: 'muted', text: 'Values are stored as plain text and readable by authorized agents. Use Secrets for credentials. Server-side JavaScript reads env.NAME; WASI reads environment variables. Static files and browser bundles receive no injected values.' }),
      items, form, apply,
      h('p', { class: 'muted small', text: 'Approved deployment, redeployment, rollback and standalone data snapshot restoration capture current settings. Automatic worker restarts reuse the captured settings. New previews load current settings.' }));
  }

  // --- secrets ---

  async function loadSecrets() {
    try {
      const res = await api.secrets(slug);
      if (ctx.alive()) drawSecrets(res.secrets || [], res.note);
    } catch (err) {
      if (ctx.alive()) fill(secretsSlot, errorPanel(err, 'Cannot load secrets'));
    }
  }

  function drawSecrets(list, note) {
    const nameId = 'secret-name';
    const valueId = 'secret-value';
    const name = h('input', {
      id: nameId, type: 'text', required: true, pattern: '[A-Z_][A-Z0-9_]*', maxlength: '64',
      autocomplete: 'off', spellcheck: 'false', autocapitalize: 'characters', class: 'mono', placeholder: 'API_KEY',
      'data-1p-ignore': true, 'data-lpignore': 'true', 'data-bwignore': true,
    });
    // Not "new-password": that asks the browser to generate and save the value
    // in its password manager. The ignore attributes do the same for the
    // common password-manager extensions.
    const value = h('input', {
      id: valueId, type: 'password', autocomplete: 'off', spellcheck: 'false',
      'data-1p-ignore': true, 'data-lpignore': 'true', 'data-bwignore': true,
    });
    const save = h('button', { type: 'submit', class: 'btn btn-primary btn-small', text: 'Save secret' });
    name.addEventListener('input', () => {
      const up = name.value.toUpperCase();
      if (up !== name.value) name.value = up;
    });
    const form = h('form', { class: 'form-grid', id: 'secret-form' },
      h('div', { class: 'field' }, h('label', { for: nameId, text: 'Name' }), name,
        h('span', { class: 'hint', text: 'Capital letters, digits and _' })),
      h('div', { class: 'field' }, h('label', { for: valueId, text: 'Value' }), value,
        h('span', { class: 'hint', text: 'Stored encrypted; never shown again' })),
      h('div', { class: 'field field-end' }, save));
    form.addEventListener('submit', (e) => {
      e.preventDefault();
      busy(save, async () => {
        try {
          await api.putSecret(slug, name.value, value.value);
          toast(`Secret ${name.value} saved. It applies after Redeploy (apply environment).`, 'success');
          loadSecrets();
          logs.poll();
        } catch (err) {
          toast(err.message, 'error');
        } finally {
          value.value = '';
        }
      });
    });
    const items = list.length
      ? h('ul', { class: 'plain-list' }, list.map((s) => {
        const replace = h('button', { type: 'button', class: 'btn btn-small', text: 'Replace', 'aria-label': `Replace value of ${s.name}` });
        const del = h('button', { type: 'button', class: 'btn btn-small btn-danger-text', text: 'Delete', 'aria-label': `Delete secret ${s.name}` });
        replace.addEventListener('click', () => { form.hidden = false; addSecret.setAttribute('aria-expanded', 'true'); name.value = s.name; value.value = ''; value.focus(); });
        del.addEventListener('click', () => busy(del, async () => {
          const ok = await confirmDialog({
            title: `Delete secret ${s.name}?`,
            body: 'The running version keeps its current environment until it is redeployed (Redeploy (apply environment) on the live version).',
            confirmLabel: 'Delete', danger: true,
          });
          if (!ok) return;
          try { await api.deleteSecret(slug, s.name); toast(`Secret ${s.name} deleted.`, 'success'); } catch (err) { toast(err.message, 'error'); }
          loadSecrets();
        }));
        return h('li', { class: 'plain-row' },
          h('div', null, h('code', { text: s.name }), h('div', { class: 'muted small' }, 'Updated ', timeEl(s.updated_at))),
          h('div', { class: 'cell-actions' }, replace, del));
      }))
      : h('p', { class: 'muted', text: 'No secrets.' });
    const apply = flat.live_version
      ? h('div', { class: 'inline-form' },
        h('span', { class: 'muted', text: `Changes apply on the next approved activation or Flats host restart: redeploy the live version ${flat.live_version} to use them now.` }),
        redeployButton())
      : h('p', { class: 'muted', text: 'Secrets apply when a deployment is approved.' });
    const addSecret = h('button', { type: 'button', class: 'btn btn-small', text: 'Add secret', 'aria-expanded': 'false', 'aria-controls': 'secret-form' });
    form.hidden = true;
    addSecret.addEventListener('click', () => {
      form.hidden = !form.hidden;
      addSecret.setAttribute('aria-expanded', String(!form.hidden));
      if (!form.hidden) name.focus();
    });
    fill(secretsSlot, addSecret,
      h('p', { class: 'muted', text: (note ? note[0].toUpperCase() + note.slice(1) : 'Values are never returned') + '. Server-only: JavaScript reads env.NAME; WASI reads environment variables. Static files and browser bundles receive no injected values.' }),
      items, form, apply,
      h('p', { class: 'muted small', text: 'Approved deployment, redeployment, rollback and standalone data snapshot restoration capture current settings. Automatic worker restarts reuse the captured settings. New previews load current settings.' }));
  }

  // --- rename & delete ---

  function renameForm() {
    const id = 'slug-input';
    const input = h('input', {
      id, type: 'text', value: flat.slug, required: true, class: 'mono', minlength: '3', maxlength: '54',
      pattern: '[a-z][a-z0-9\\-]*[a-z0-9]', autocomplete: 'off', spellcheck: 'false', autocapitalize: 'off',
    });
    const btn = h('button', { type: 'submit', class: 'btn btn-small', text: 'Rename…' });
    const form = h('form', { class: 'inline-form' },
      h('label', { for: id, text: 'New slug' }), input, btn);
    form.addEventListener('submit', (e) => {
      e.preventDefault();
      const to = input.value.trim();
      if (to === flat.slug) return;
      busy(btn, async () => {
        let days = 7;
        try { days = Number((await api.settings()).settings.redirect_days) || days; } catch { /* default */ }
        const ok = await confirmDialog({
          title: `Rename ${flat.slug} to ${to}?`,
          body: [
            h('p', { text: `The flat moves to new addresses. The old private and public addresses redirect to the new ones for ${plural(days, 'day')}; after that they stop working and the old slug can be taken by another flat.` }),
            h('p', { class: 'muted', text: 'Open previews close. Agents and scripts that use the old slug must be updated.' }),
          ],
          confirmLabel: 'Rename',
        });
        if (!ok) return;
        try {
          const f = await api.rename(flat.slug, to);
          toast(`Renamed to ${f.slug}.`, 'success');
          ctx.navigate(`/flats/${encodeURIComponent(f.slug)}/settings`, { replace: true });
        } catch (err) {
          toast(err.message, 'error');
        }
      });
    });
    return h('div', null,
      h('p', { class: 'muted', text: 'The slug is the flat’s name in its addresses.' }), form);
  }

  function deleteBlock() {
    const btn = h('button', { type: 'button', class: 'btn btn-danger', text: 'Delete flat…' });
    btn.addEventListener('click', () => busy(btn, async () => {
      if (await deleteFlat(flat)) ctx.navigate('/', { replace: true });
    }));
    return h('div', { class: 'danger-zone' },
      h('p', { text: 'Permanently deletes every version, the flat’s data, environment variables, secrets and logs, and takes its addresses offline.' }),
      btn);
  }

  refresh().then((ok) => { if (ok) logs.start(); });
  return () => { for (const t of timers) clearInterval(t); logs.stop(); };
}

// logsPanel shows events newest first and polls for newer ones with ?after=.
function logsPanel(slug, ctx, timers) {
  let lastId = 0;
  let timer = null;
  let inflight = false;
  const list = h('ol', { class: 'log', 'aria-label': 'Events, newest first' });
  const status = h('span', { class: 'muted small' });
  const autoId = 'logs-auto';
  const auto = h('input', { id: autoId, type: 'checkbox', checked: true });
  const el = h('div', null,
    h('div', { class: 'log-tools' },
      h('label', { for: autoId, class: 'check' }, auto, ' Auto-refresh every 5 s'), status),
    list);

  let again = false; // a poll was requested while one was in flight
  async function poll() {
    if (!ctx.alive()) return;
    if (inflight) { again = true; return; }
    inflight = true;
    again = false;
    try {
      const { events } = await api.logs(slug, lastId);
      if (!ctx.alive()) return;
      for (const e of events || []) {
        lastId = Math.max(lastId, e.id);
        list.prepend(eventRow(e));
      }
      while (list.childElementCount > LOG_KEEP) list.lastElementChild.remove();
      if (!list.childElementCount) list.appendChild(h('li', { class: 'muted empty-log', text: 'No events yet.' }));
      else list.querySelector('.empty-log')?.remove();
      status.textContent = 'Updated ' + new Date().toLocaleTimeString();
    } catch (err) {
      status.textContent = 'Refresh failed: ' + err.message;
    } finally {
      inflight = false;
      if (again) poll();
    }
  }
  function start() {
    stop();
    if (auto.checked) {
      timer = setInterval(() => { if (document.visibilityState === 'visible') poll(); }, LOG_POLL_MS);
      timers.push(timer);
    }
  }
  function stop() { if (timer) clearInterval(timer); timer = null; }
  auto.addEventListener('change', () => (auto.checked ? (poll(), start()) : stop()));
  return { el, poll, start, stop };
}

function eventRow(e) {
  let data = null;
  if (e.data !== undefined && e.data !== null) {
    data = h('details', null, h('summary', { text: 'Details' }), h('pre', { text: JSON.stringify(e.data, null, 2) }));
  }
  return h('li', { class: 'log-row level-' + e.level },
    h('time', { datetime: e.time, title: dateTime(e.time), text: new Date(e.time).toLocaleString() }),
    h('span', { class: 'badge badge-' + e.level, text: e.level }),
    h('span', { class: 'log-kind', text: e.kind }),
    h('span', { class: 'log-msg', text: e.message }),
    data);
}
