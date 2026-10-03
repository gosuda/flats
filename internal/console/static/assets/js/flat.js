// Flat page: addresses, visibility, versions, previews, logs, secrets,
// usage, rename and delete.

import { h, timeEl, dateTime, bytes, plural, shortHash, VISIBILITY, publicNoticeOf } from './dom.js';
import { api } from './api.js';
import { confirmDialog, errorPanel, loading, toast, extLink, busy, fill } from './ui.js';
import { deployVersion, previewVersion, deleteFlat, setVisibility, redeployLive } from './actions.js';
import { thumb } from './list.js';

const LOG_POLL_MS = 5000;
const LOG_KEEP = 500;

function card(title, id, ...children) {
  const hid = id + '-title';
  return h('section', { class: 'card', id, 'aria-labelledby': hid },
    h('h2', { id: hid, text: title }), ...children);
}

export function mount(main, [slug], ctx) {
  ctx.setTitle(slug);
  let flat = null;
  const timers = [];

  const headSlot = h('div', null, loading());
  const visSlot = h('div');
  const versionsSlot = h('div', null, loading());
  const previewsSlot = h('div', null, loading());
  const secretsSlot = h('div', null, loading());
  const usageSlot = h('div', null, loading());
  const logs = logsPanel(slug, ctx, timers);

  const page = h('section', { class: 'page' },
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
    drawVisibility();
    return true;
  }

  // Everything that a deploy, rollback or visibility change can affect.
  async function refresh() {
    const ok = await loadFlat();
    if (ok) {
      loadVersions();
      loadPreviews();
      loadSecrets();
      loadUsage();
      logs.poll();
    }
    return ok;
  }

  // --- header ---

  function drawHead() {
    const nameEl = h('h1', { class: 'flat-title', text: flat.name || flat.slug });
    const edit = h('button', { type: 'button', class: 'btn btn-small', text: 'Edit name' });
    const titleRow = h('div', { class: 'title-row' }, nameEl, edit);
    edit.addEventListener('click', () => editName(titleRow));

    const urls = h('dl', { class: 'facts' });
    const add = (k, v) => urls.append(h('dt', { text: k }), h('dd', null, v));
    add('Slug', h('code', { text: flat.slug }));
    const pending = flat.private_state && !['ready', 'key-expiring'].includes(flat.private_state);
    add('Private URL', flat.live_version
      ? h('div', null, extLink(flat.private_url),
          pending ? h('div', { class: 'muted small' },
            h('span', { class: 'badge badge-' + flat.private_state, text: flat.private_state }), ' ',
            flat.private_detail || 'not answering yet') : null)
      : h('span', { class: 'muted' }, h('code', { text: flat.private_url }), ' (online after the first deploy)'));
    if (flat.public_url) {
      add('Public URL', h('div', null,
        flat.live_version ? extLink(flat.public_url) : h('code', { text: flat.public_url }),
        h('p', { class: 'notice-text', text: publicNoticeOf(flat) })));
    }
    add('Live', flat.live ? `Version ${flat.live.number} · ${flat.live.kind}` : 'Not published');
    add('Created', dateTime(flat.created_at));
    if (flat.old_slug && flat.old_slug_until) {
      add('Redirect', `Old slug ${flat.old_slug} redirects here until ${dateTime(flat.old_slug_until)}`);
    }

    fill(headSlot,
      h('header', { class: 'flat-head' }, thumb(flat, true), h('div', { class: 'flat-head-main' }, titleRow, urls)),
      visSlot,
      card('Versions', 'versions', versionsSlot),
      card('Open previews', 'previews', previewsSlot),
      card('Logs', 'logs', logs.el),
      card('Secrets', 'secrets', secretsSlot),
      card('Usage', 'usage', usageSlot),
      card('Rename slug', 'rename', renameForm()),
      card('Delete flat', 'delete', deleteBlock()));
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
          loadVersions(); loadPreviews(); loadUsage();
        } catch (err) {
          toast(err.message, 'error');
        }
      });
    });
    row.replaceWith(form);
    input.focus();
    input.select();
  }

  // --- visibility ---

  function drawVisibility() {
    const id = 'vis-select';
    const select = h('select', { id }, Object.entries(VISIBILITY).map(([v, d]) =>
      h('option', { value: v, selected: v === flat.visibility, text: d.label })));
    const apply = h('button', { type: 'submit', class: 'btn btn-small', text: 'Apply' });
    const form = h('form', { class: 'inline-form' },
      h('label', { for: id, text: 'Who can open it' }), select, apply);
    form.addEventListener('submit', (e) => {
      e.preventDefault();
      if (select.value === flat.visibility) return;
      busy(apply, async () => {
        const res = await setVisibility(flat, select.value);
        if (res) refresh(); else select.value = flat.visibility;
      });
    });
    fill(visSlot, card('Visibility', 'visibility',
      h('p', { class: 'muted', text: 'Private flats are reachable only on your tailnet. Public flats are served through Portal relays.' }),
      form));
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
    if (!vs.length) {
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
        h('caption', { class: 'sr-only', text: 'Saved versions, newest first' }),
        h('thead', null, h('tr', null, ['Version', 'Created', 'Hash', 'Git', 'Message', 'Size', 'Actions'].map((t) => h('th', { scope: 'col', text: t })))),
        h('tbody', null, rows))));
  }

  // redeployButton restarts the live version, e.g. after a secret changed.
  function redeployButton() {
    const btn = h('button', {
      type: 'button', class: 'btn btn-small', text: 'Redeploy (apply secrets)',
      'aria-label': `Redeploy live version ${flat.live_version} to apply secrets`,
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
          h('div', null, h('strong', { text: `v${p.version}` }), ' ', extLink(p.url),
            h('div', { class: 'muted small' }, 'Opened ', timeEl(p.created_at), ` · closes if unvisited until ${dateTime(p.expires_at)}`)),
          close);
      })));
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
        replace.addEventListener('click', () => { name.value = s.name; value.value = ''; value.focus(); });
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
    fill(secretsSlot,
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
          ctx.navigate(`/flats/${encodeURIComponent(f.slug)}`, { replace: true });
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

