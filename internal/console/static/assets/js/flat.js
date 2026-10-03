// Flat page: addresses, visibility, versions, previews, logs, secrets,
// usage, rename and delete.

import { h, timeEl, dateTime, bytes, plural } from './dom.js';
import { api } from './api.js';
import { confirmDialog, errorPanel, loading, toast, extLink, busy, fill } from './ui.js';
import { deleteFlat, previewVersion, redeployLive } from './actions.js';
import { thumb } from './list.js';
import { siteHeader } from './site.js';
import {
  activateVersion, changeVisibility, currentTarget, draftEditor, draftTarget, findDraftPreview,
  openControl, pendingItems, publishDraft, renderAccess, renderCurrent, renderDraft, renderHistory,
  saveProvider, statusLine, visibilitySettled, visibilityWord, PROVIDERS, permittedProviders,
} from './lifecycle.js';

const LOG_POLL_MS = 5000;

function initialTab() {
  try {
    const id = String((globalThis.location && location.hash) || '#overview').replace(/^#/, '');
    return ['overview', 'history', 'access', 'operations'].includes(id) ? id : 'overview';
  } catch {
    return 'overview';
  }
}
const LOG_KEEP = 500;

function card(title, id, ...children) {
  const hid = id + '-title';
  return h('section', { class: 'card', id, 'aria-labelledby': hid },
    h('h2', { id: hid, text: title }), ...children);
}

export function mountSettings(main, params, ctx) { return mount(main, params, ctx, true); }

export function mount(main, [slug], ctx, settings = false) {
  ctx.setTitle(slug);
  let flat = null;
  const timers = [];

  const headSlot = h('div', null, loading());
  const previewsSlot = h('div', null, loading());
  const secretsSlot = h('div', null, loading());
  const usageSlot = h('div', null, loading());
  const logs = settings ? { poll() {}, start() {}, stop() {} } : logsPanel(slug, ctx, timers);
  let previews = [];
  let versions = [];
  let approvals = [];
  let tab = initialTab();
  let draftSaveText = '';

  const page = h('section', { class: 'page' + (settings ? ' site-page' : '') },
    h('a', { class: 'back', href: '/', 'data-nav': true }, '← All flats'),
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
    ctx.setTitle(flat.name || flat.slug);
    drawHead();
    return true;
  }

  // Everything that a deploy, rollback or visibility change can affect.
  async function refresh() {
    const ok = await loadFlat();
    if (ok) {
      loadSecrets();
      if (!settings) { loadHistory(); loadPreviews(); loadUsage(); loadApprovals(); }
      logs.poll();
    }
    return ok;
  }

  // --- header ---

  function drawHead() {
    const activeId = headSlot.contains(document.activeElement) ? document.activeElement?.id : '';
    if (settings) { drawSettings(); return; }
    const nameEl = h('h1', { class: 'flat-title', text: flat.name || flat.slug });
    const edit = h('button', { type: 'button', class: 'btn btn-small', text: 'Edit name' });
    const titleRow = h('div', { class: 'title-row' }, nameEl, edit);
    edit.addEventListener('click', () => editName(titleRow));
    const waiting = pendingItems(flat, approvals);
    const draftUi = draftEditor(flat, (ev) => {
      draftSaveText = ev.status;
      const live = headSlot.querySelector('.draft-save');
      if (live) live.textContent = ev.status;
      if (ev.response?.draft) {
        flat = { ...flat, draft: ev.response.draft };
        const active = document.activeElement;
        const focusId = active && headSlot.contains(active) ? active.id : '';
        drawHead();
        if (focusId) document.getElementById(focusId)?.focus();
      }
      else if (ev.conflict) draftSaveText = ev.status;
    });
    if (draftSaveText) draftUi.status = () => draftSaveText;
    const publish = h('button', { id: 'publish-draft', type: 'button', class: 'btn btn-primary', text: flat.live_version ? `Publish the next version after v${flat.live_version}` : 'Publish v1' });
    publish.addEventListener('click', () => busy(publish, async () => { if (await publishDraft(flat)) await refresh(); }));
    const prepare = h('button', { id: 'prepare-draft', type: 'button', class: 'btn btn-small', text: 'Open draft (private)' });
    prepare.addEventListener('click', () => busy(prepare, async () => {
      try {
        await api.openDraftPreview(slug);
        toast('Draft preview is private. It is not a published version.', 'success');
        await loadPreviews();
        drawHead();
      } catch (err) { toast(err.message, 'error'); }
    }));
    fill(headSlot,
      h('header', { class: 'flat-head' }, thumb(flat, true), h('div', { class: 'flat-head-main' },
        titleRow,
        h('p', { class: 'life-status', text: statusLine(flat) }),
        flat.old_slug ? h('p', { class: 'muted small', text: `Old slug ${flat.old_slug} redirects until ${dateTime(flat.old_slug_until)}` }) : null)),
      waiting.length ? h('div', { class: 'banner', role: 'region', 'aria-label': 'Pending approvals' },
        h('div', null, h('strong', { text: 'Approval waiting' }),
          h('ul', null, waiting.map((a) => h('li', null,
            h('a', { href: `/approvals/${encodeURIComponent(a.id)}`, 'data-nav': true, text: `${a.action} · ${a.status}` })))))) : null,
      h('div', { class: 'life-grid' },
        renderCurrent(flat, (btn) => busy(btn, async () => { if (await redeployLive(flat)) await refresh(); })),
        renderDraft(flat, findDraftPreview(previews, flat.draft?.revision), {
          editor: draftUi.editor,
          saveState: draftSaveText || (flat.draft ? `Saved ${flat.draft.updated_at ? dateTime(flat.draft.updated_at) : ''}`.trim() : ''),
          publishButton: flat.draft?.dirty ? publish : null,
          previewButton: prepare,
        })),
      tabBar(),
      tabPanel());
    if (activeId) document.getElementById(activeId)?.focus();
  }

  function tabBar() {
    const tabs = [
      ['overview', 'Overview'],
      ['history', 'Version history'],
      ['access', 'Access'],
      ['operations', 'Operations'],
    ];
    return h('div', { class: 'life-tabs', role: 'tablist', 'aria-label': 'Flat sections' }, tabs.map(([id, label]) => {
      const btn = h('button', { type: 'button', role: 'tab', id: 'tab-' + id, 'aria-controls': 'panel-' + id, 'aria-selected': tab === id ? 'true' : 'false', tabindex: tab === id ? '0' : '-1', text: label });
      btn.addEventListener('keydown', (ev) => {
        const index = tabs.findIndex(([value]) => value === id);
        const next = ev.key === 'Home' ? 0 : ev.key === 'End' ? tabs.length - 1
          : ev.key === 'ArrowRight' ? (index + 1) % tabs.length
            : ev.key === 'ArrowLeft' ? (index + tabs.length - 1) % tabs.length : null;
        if (next === null) return;
        ev.preventDefault();
        document.getElementById('tab-' + tabs[next][0])?.click();
      });
      btn.addEventListener('click', () => {
        tab = id;
        try { history.replaceState(null, '', `#${id}`); } catch { /* harness without history */ }
        drawHead();
        document.getElementById('tab-' + id)?.focus();
      });
      return btn;
    }));
  }

  function tabPanel() {
    const panel = h('div', { id: 'panel-' + tab, role: 'tabpanel', 'aria-labelledby': 'tab-' + tab });
    if (tab === 'history') panel.appendChild(historyPanel());
    else if (tab === 'access') panel.appendChild(accessPanel());
    else if (tab === 'operations') panel.append(operationsPanel());
    else panel.append(overviewPanel());
    return panel;
  }

  function overviewPanel() {
    const current = currentTarget(flat);
    const draft = draftTarget(findDraftPreview(previews, flat.draft?.revision));
    return h('div', null,
      h('p', { text: `${flat.name || flat.slug} is ${publicationPhrase(flat)} and ${visibilityWord(flat.visibility)}.` }),
      h('div', { class: 'life-actions' },
        openControl(current, current.version ? `Open current version v${current.version}` : 'Open current version'),
        openControl(draft.ready ? draft : { ...draft, ready: false }, 'Open draft (private)')),
      h('p', { class: 'muted', text: 'Logs, the database, and environment variables are in Operations and in the site settings.' }));
  }

  function historyPanel() {
    return renderHistory(flat, versions, approvals, {
      versionActions(v, current) {
        const preview = h('button', { type: 'button', class: 'btn btn-small', text: `Preview v${v.number} (private)`, disabled: v.pruned });
        preview.addEventListener('click', () => busy(preview, async () => { if (await previewVersion(flat, v.number)) loadPreviews(); }));
        if (current) return h('div', { class: 'cell-actions' }, preview, redeployButton());
        const make = h('button', { type: 'button', class: 'btn btn-small', text: `Make v${v.number} current`, disabled: v.pruned });
        make.addEventListener('click', () => busy(make, async () => {
          if (await activateVersion(flat, v.number, { rollback: v.number < flat.live_version })) await refresh();
        }));
        return h('div', { class: 'cell-actions' }, preview, make);
      },
    });
  }

  function accessPanel() {
    const select = h('select', { id: 'access-visibility', 'aria-describedby': 'access-hint' },
      h('option', { value: 'private', selected: flat.visibility !== 'public', text: 'Private' }),
      h('option', { value: 'public', selected: flat.visibility === 'public', disabled: flat.publication !== 'published', text: 'Public' }));
    const apply = h('button', { id: 'apply-access', type: 'submit', class: 'btn btn-small', text: 'Apply access' });
    const cancel = h('button', { type: 'button', class: 'btn btn-small', text: 'Cancel' });
    const form = h('form', { class: 'inline-form' },
      h('label', { for: 'access-visibility', text: 'Access' }), select, apply, cancel);
    cancel.addEventListener('click', () => { select.value = flat.visibility === 'public' ? 'public' : 'private'; });
    form.addEventListener('submit', (e) => {
      e.preventDefault();
      const next = select.value;
      busy(apply, async () => {
        const outcome = await changeVisibility(flat, next);
        if (!outcome) { select.value = flat.visibility === 'public' ? 'public' : 'private'; return; }
        await refresh();
        const settled = visibilitySettled(flat, next, outcome);
        if (!settled.ok) toast(settled.text, 'error');
      });
    });
    const providerRows = PROVIDERS.map((p) => providerRow(p));
    const current = currentTarget(flat);
    const draft = draftTarget(findDraftPreview(previews, flat.draft?.revision));
    return renderAccess(flat, {
      visibility: h('div', null,
        h('p', { id: 'access-hint', class: 'muted small', text: flat.publication === 'published'
          ? `Current access is ${visibilityWord(flat.visibility)}. Changing it does not publish a version.`
          : 'Publish v1 before making this flat Public.' }),
        form),
      providers: h('div', null, providerRows),
      addresses: h('ul', { class: 'plain-list' },
        h('li', null, openControl(current, current.version ? `Open current version v${current.version}` : 'Open current version')),
        h('li', null, openControl(draft, 'Open draft (private)'))),
    }, previews);
  }

  function providerRow(p) {
    const allowed = p.id === 'local' || permittedProviders(flat).some((x) => x.id === p.id);
    const permit = h('input', { type: 'checkbox', id: 'permit-' + p.id, checked: allowed, disabled: p.id === 'local' });
    const intent = h('input', { type: 'checkbox', id: 'intent-' + p.id, disabled: p.id === 'local' });
    const save = h('button', { type: 'button', class: 'btn btn-small', text: p.id === 'local' ? 'Local stays on' : `Save ${p.label}`, disabled: p.id === 'local' });
    function sync() { save.disabled = p.id !== 'local' && permit.checked && !intent.checked; }
    permit.addEventListener('change', sync);
    intent.addEventListener('change', sync);
    sync();
    save.addEventListener('click', () => busy(save, async () => {
      if (p.id !== 'local' && permit.checked && !intent.checked) return;
      if (await saveProvider(flat, p.id, permit.checked)) await refresh();
    }));
    return h('div', { class: 'provider-row' },
      h('div', null, h('strong', { text: p.label }), h('div', { class: 'muted small', text: p.audience })),
      p.id === 'local' ? h('p', { class: 'muted small', text: 'Local is always permitted. It does not need a separate opt-in.' }) : h('div', { class: 'provider-opts' },
        h('label', { class: 'check', for: 'permit-' + p.id }, permit, ` Allow ${p.label}`),
        h('label', { class: 'check', for: 'intent-' + p.id }, intent, ' This flat should use it')),
      save);
  }

  function operationsPanel() {
    return h('div', null,
      h('p', { class: 'ops-links' },
        h('a', { href: `/flats/${encodeURIComponent(slug)}/database`, 'data-nav': true, text: 'Database' }),
        ' · ',
        h('a', { href: `/flats/${encodeURIComponent(slug)}/analytics`, 'data-nav': true, text: 'Analytics' }),
        ' · ',
        h('a', { href: `/flats/${encodeURIComponent(slug)}/settings`, 'data-nav': true, text: 'Environment variables and settings' })),
      card('Logs', 'logs', logs.el),
      card('Secrets', 'secrets', secretsSlot),
      card('Open previews', 'previews', previewsSlot),
      card('Usage', 'usage', usageSlot),
      card('Rename slug', 'rename', renameForm()),
      card('Delete flat', 'delete', deleteBlock()));
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
    const row = (title, hint, control) => h('div', { class: 'setting-row' },
      h('div', null, h('label', { for: title === 'Name' ? 'site-name' : undefined, text: title }),
        hint ? h('div', { class: 'muted small', text: hint }) : null), control);
    fill(headSlot, siteHeader(flat, 'settings'),
      h('section', { class: 'settings-section', 'aria-labelledby': 'general-title' },
        h('h2', { id: 'general-title', text: 'General' }),
        row('Name', 'Name for your site', nameForm),
        row('Addresses', 'Current version and draft links are labeled on the flat page', h('div', { class: 'setting-control' }, h('a', { href: `/flats/${encodeURIComponent(slug)}#access`, 'data-nav': true, text: 'Open access' }), change)), rename,
        row('Custom domain', 'Custom domains are not supported on this host yet.', h('span', { class: 'muted small', text: 'Unavailable' })),
        row('Access', 'Private or Public for the current version. Separate from publishing.', h('div', { class: 'setting-control' }, h('span', { text: visibilityWord(flat.visibility) }), h('a', { class: 'btn btn-small', href: `/flats/${encodeURIComponent(slug)}#access`, 'data-nav': true, text: 'Change access' })))),
      h('section', { class: 'settings-section', id: 'secrets' }, h('h2', { text: 'Environment variables' }), secretsSlot),
      h('section', { class: 'settings-section', id: 'delete' }, h('h2', { text: 'Danger zone' }), deleteBlock()),
      h('a', { class: 'back', href: `/flats/${encodeURIComponent(slug)}`, 'data-nav': true, text: 'Manage versions, previews and logs' }));
  }

  function editName(row) {
    const id = 'name-input';
    const input = h('input', { id, type: 'text', value: flat.name || '', maxlength: '200', required: true });
    const save = h('button', { type: 'submit', class: 'btn btn-primary btn-small', text: 'Save' });
    const cancel = h('button', { type: 'button', class: 'btn btn-small', text: 'Cancel' });
    const form = h('form', { class: 'inline-form' },
      h('label', { for: id, class: 'sr-only', text: 'Display name' }), input, save, cancel);
    cancel.addEventListener('click', drawHead);
    form.addEventListener('submit', (e) => {
      e.preventDefault();
      busy(save, async () => {
        try {
          await api.setName(flat.slug, input.value.trim());
          toast('Name saved.', 'success');
          await loadFlat();
          loadHistory(); loadPreviews(); loadUsage();
        } catch (err) {
          toast(err.message, 'error');
        }
      });
    });
    row.replaceWith(form);
    input.focus();
    input.select();
  }

  function publicationPhrase(f) {
    if (f.publication === 'published') return `Published, current v${f.live_version || 0}`;
    if (f.publication === 'unpublished') return 'Unpublished';
    return 'missing a publication field';
  }

  async function loadHistory() {
    try {
      const res = await api.versions(slug);
      versions = (res.versions || []).slice().sort((a, b) => b.number - a.number);
    } catch (err) {
      versions = [];
      if (ctx.alive()) toast(err.message, 'error');
    }
  }

  async function loadApprovals() {
    try {
      const res = await api.approvals();
      approvals = res.approvals || [];
      if (ctx.alive() && !settings) drawHead();
    } catch { approvals = []; }
  }

  // redeployButton restarts the live version, e.g. after a secret changed.
  function redeployButton() {
    const btn = h('button', {
      type: 'button', class: 'btn btn-small', text: 'Redeploy (apply secrets)',
      'aria-label': `Redeploy live version ${flat.live_version} to apply secrets`,
    });
    btn.addEventListener('click', () => busy(btn, async () => { if (await redeployLive(flat)) await refresh(); }));
    return btn;
  }

  // --- previews ---

  async function loadPreviews() {
    try {
      const res = await api.previews(slug);
      previews = res.previews || [];
      if (!ctx.alive()) return;
      if (!previews.length) {
        fill(previewsSlot, h('p', { class: 'muted', text: 'No open previews. Previews close when the flat is deployed or after a day without visits.' }));
        return;
      }
      fill(previewsSlot, h('ul', { class: 'plain-list' }, previews.map((p) => {
        const close = h('button', { type: 'button', class: 'btn btn-small', text: 'Close', 'aria-label': `Close preview ${p.host}` });
        close.addEventListener('click', () => busy(close, async () => {
          try { await api.closePreview(p.host); toast('Preview closed.', 'success'); } catch (err) { toast(err.message, 'error'); }
          loadPreviews();
        }));
        const label = p.target === 'draft' || p.version === 0 ? 'Draft preview (private)' : `Published v${p.version} (private preview)`;
        return h('li', { class: 'plain-row' },
          h('div', null, h('strong', { text: label }), ' ', extLink(p.url, label),
            h('div', { class: 'muted small' }, 'Opened ', timeEl(p.created_at), ` · closes if unvisited until ${dateTime(p.expires_at)}`)),
          close);
      })));
      if (!settings) drawHead();
    } catch (err) {
      if (ctx.alive()) fill(previewsSlot, errorPanel(err, 'Cannot load previews'));
    }
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
    const form = h('form', { class: 'form-grid' },
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
          toast(`Secret ${name.value} saved. It applies after Redeploy (apply secrets).`, 'success');
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
        replace.addEventListener('click', () => { form.hidden = false; addVariable?.setAttribute('aria-expanded', 'true'); name.value = s.name; value.value = ''; value.focus(); });
        del.addEventListener('click', () => busy(del, async () => {
          const ok = await confirmDialog({
            title: `Delete secret ${s.name}?`,
            body: 'The running version keeps its current environment until it is redeployed (Redeploy (apply secrets) on the live version).',
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
        h('span', { class: 'muted', text: `Changes apply when the flat restarts: redeploy the live version ${flat.live_version} to use them now.` }),
        redeployButton())
      : h('p', { class: 'muted', text: 'Secrets apply when a version is deployed.' });
    const addVariable = settings ? h('button', { type: 'button', class: 'btn btn-small', text: 'Add variable', 'aria-expanded': 'false' }) : null;
    if (settings) {
      form.hidden = true;
      addVariable.addEventListener('click', () => {
        form.hidden = !form.hidden;
        addVariable.setAttribute('aria-expanded', String(!form.hidden));
        if (!form.hidden) name.focus();
      });
    }
    fill(secretsSlot, addVariable,
      h('p', { class: 'muted', text: (note ? note[0].toUpperCase() + note.slice(1) : 'Values are never returned') + '. Server flats read them as environment variables.' }),
      items, form, apply);
  }

  // --- usage ---

  async function loadUsage() {
    let stats = null, err = null;
    try { stats = await api.stats(slug, 30); } catch (e) { err = e; }
    if (!ctx.alive() || !flat) return;
    fill(usageSlot,
      h('dl', { class: 'facts' },
        h('dt', { text: 'Disk' }), h('dd', { text: `${bytes(flat.disk_bytes)} (versions, data and previews)` }),
        h('dt', { text: 'Versions' }), h('dd', { text: String(flat.versions) })),
      err ? errorPanel(err, 'Cannot load page views') : viewsChart(stats.page_views || [], stats.note));
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
          ctx.navigate(`/flats/${encodeURIComponent(f.slug)}${settings ? '/settings' : ''}`, { replace: true });
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
      h('p', { text: 'Permanently deletes every version, the flat’s data, secrets and logs, and takes its addresses offline.' }),
      btn);
  }

  refresh().then((ok) => { if (ok) logs.start(); });
  return () => { for (const t of timers) clearInterval(t); logs.stop(); };
}

// viewsChart draws daily page views as bars, filling missing days with zero.
function viewsChart(rows, note) {
  const counts = new Map(rows.map((r) => [r.day, r.count]));
  const days = [];
  const today = new Date();
  for (let i = 29; i >= 0; i--) {
    const d = new Date(Date.UTC(today.getUTCFullYear(), today.getUTCMonth(), today.getUTCDate() - i));
    days.push(d.toISOString().slice(0, 10));
  }
  // Keep server days that do not fit the expected format.
  for (const r of rows) if (!days.includes(r.day)) days.push(r.day);
  const data = days.map((d) => ({ day: d, count: counts.get(d) || 0 }));
  const total = data.reduce((s, d) => s + d.count, 0);
  const max = Math.max(1, ...data.map((d) => d.count));
  const peak = data.reduce((p, d) => (d.count > p.count ? d : p), data[0]);
  const summary = total
    ? `Page views, last 30 days: ${total} in total, peak ${peak.count} on ${peak.day}.`
    : 'No page views in the last 30 days.';
  const bars = h('div', { class: 'bars', role: 'img', 'aria-label': summary },
    data.map((d) => {
      const bar = h('div', { class: 'bar', title: `${d.day}: ${d.count}` }, h('span'));
      bar.firstChild.style.setProperty('--h', `${(d.count / max) * 100}%`);
      return bar;
    }));
  return h('div', { class: 'chart' },
    h('div', { class: 'chart-head' }, h('strong', { text: 'Page views per day' }), h('span', { class: 'muted', text: total ? `${total} total · max ${max}/day` : 'none yet' })),
    bars,
    h('div', { class: 'chart-axis muted small' }, h('span', { text: data[0].day }), h('span', { text: data[data.length - 1].day })),
    h('p', { class: 'muted small', text: note || 'Counts HTML page requests. Unique visitors are not counted.' }));
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
