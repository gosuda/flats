// Draft, published version, visibility, and provider presentation.
// Publish, rollback, and visibility stay pending until the console approve call.
// A save message is metadata on a file upload. It is not the flat content.

import { h, timeEl, dateTime, bytes, shortHash } from './dom.js';
import { api } from './api.js';
import { announce, confirmDialog, infoDialog, extLink, toast } from './ui.js';

export const CHECK_COPY = 'After you approve, the check runs on a copy of this flat’s data. Live data is not used for that check. A failed check does not create a published version.';

export const PROVIDERS = [
  { id: 'local', label: 'Local', scope: 'private', audience: 'This device, through localhost.' },
  { id: 'tailscale', label: 'Tailscale', scope: 'private', audience: 'People and devices allowed by your tailnet ACL.' },
  { id: 'tailscale-funnel', label: 'Tailscale Funnel', scope: 'public', audience: 'Anyone on the internet. Visitors do not need Tailscale.' },
  { id: 'portal', label: 'Portal', scope: 'public', audience: 'Anyone on the internet.' },
];

const OPEN_STATES = new Set(['ready', 'key-expiring']);

export function providerById(id) {
  return PROVIDERS.find((p) => p.id === id) || null;
}

export function providerLabel(id) {
  return providerById(id)?.label || id || '';
}

export function permittedProviders(flat) {
  const ids = new Set(Array.isArray(flat?.providers) ? flat.providers : []);
  ids.add('local');
  return PROVIDERS.filter((p) => ids.has(p.id));
}

export function connectionState(flat) {
  if (flat?.connection_state) return flat.connection_state;
  return '';
}

export function connectionDetail(flat) {
  const publicRoute = flat?.visibility === 'public';
  if (!publicRoute) return flat?.private_detail || '';
  return flat?.endpoints?.find((ep) => ep.audience === 'current' && ep.host === flat.slug &&
    ['tailscale-funnel', 'portal'].includes(ep.provider) && ep.url === flat.public_url && ep.state === connectionState(flat))?.detail || '';
}

export function connectionLabel(state) {
  switch (state) {
    case 'ready': return 'Connected';
    case 'key-expiring': return 'Connected, certificate expiring';
    case 'starting': return 'Connecting';
    case 'needs-login': return 'Needs setup';
    case 'unavailable': return 'Needs setup';
    case 'error': return 'Connection failed';
    default: return state ? state : 'Connection not reported';
  }
}

export function isOpenable(state) {
  return OPEN_STATES.has(state);
}

export function publicationLabel(flat) {
  if (flat?.publication === 'published') return `Published · v${flat.live_version || 0}`;
  if (flat?.publication === 'unpublished') return 'Unpublished';
  return 'Publication not reported';
}

export function visibilityWord(vis) {
  if (vis === 'public' || vis === 'public-listed' || vis === 'public-unlisted') return 'Public';
  if (vis === 'private') return 'Private';
  return 'Access not reported';
}

export function statusLine(flat) {
  const draft = !flat?.draft ? 'No draft' : flat.draft.dirty ? 'Draft changes' : 'Draft saved';
  const names = permittedProviders(flat).map((p) => p.label).join(', ');
  return [publicationLabel(flat), visibilityWord(flat?.visibility), draft, names, connectionLabel(connectionState(flat))].join(' · ');
}

export function endpointState(flat, which) {
  if (which === 'public') return flat?.public_state || connectionState(flat);
  return flat?.private_state || connectionState(flat);
}

export function currentTarget(flat) {
  const visibility = flat?.visibility === 'public' ? 'public' : 'private';
  const endpoint = flat?.endpoints?.find((ep) => ep.audience === 'current' &&
    (visibility === 'public' ? ['tailscale-funnel', 'portal'].includes(ep.provider) : ['local', 'tailscale'].includes(ep.provider)) && ep.ready)
    || flat?.endpoints?.find((ep) => ep.audience === 'current' &&
      (visibility === 'public' ? ['tailscale-funnel', 'portal'].includes(ep.provider) : ['local', 'tailscale'].includes(ep.provider)));
  const url = endpoint?.url || (visibility === 'public' ? flat?.public_url : flat?.private_url);
  const state = endpoint?.state || endpointState(flat, visibility);
  const ready = !!(url && isOpenable(state) && (!endpoint || (endpoint.ready && endpoint.permitted && endpoint.configured))
    && flat?.publication === 'published' && flat?.live_version);
  return {
    url: ready ? url : (url || ''),
    ready,
    target: 'current',
    visibility,
    state,
    version: flat?.live_version || 0,
  };
}

export function draftTarget(preview) {
  if (!preview) return { url: '', ready: false, target: 'draft', visibility: 'private', state: '' };
  const state = preview.state || '';
  const ready = !!(preview.url && (state === '' || isOpenable(state)));
  return { url: preview.url || '', ready, target: 'draft', visibility: 'private', state, revision: preview.revision || 0 };
}

export function findDraftPreview(previews, revision) {
  return (previews || []).find((p) => (p.target === 'draft' || (p.version === 0 && p.revision))
    && (!revision || p.revision === revision)) || null;
}

const nameOf = (flat) => flat?.name || flat?.slug || 'this flat';

export function impactText(source) {
  let execution = source?.result_data || source?.approval?.result_data || source;
  if (typeof execution === 'string') { try { execution = JSON.parse(execution); } catch { execution = {}; } }
  const impact = execution?.data_impact || source?.data_impact || source?.approval?.data_impact || '';
  if (impact === 'none') return 'The check reported data impact: none. Live data was not used for the check.';
  if (impact === 'runtime_start') return 'The runtime started against live data and may have written to it. Health checks use an isolated copy.';
  if (impact === 'restore_data') return 'The approved data restore ran. The previous live data was backed up.';
  if (impact === 'unknown') return 'The result could not confirm the live data impact. Inspect the operation logs before retrying.';
  if (impact) return String(impact);
  const result = source?.result || source?.error || '';
  if (/ran against the live data/i.test(result)) return result;
  return 'This result did not report data impact. A failed approval does not create a published version. The console will not claim the data was untouched.';
}

export function failureCode(source) {
  let execution = source?.approval?.result_data || source?.result_data;
  if (typeof execution === 'string') { try { execution = JSON.parse(execution); } catch { execution = {}; } }
  return execution?.failure_code || source?.category || '';
}

export function failureMessage(source) {
  const messages = {
    provider_not_permitted: 'This provider is not permitted. Review host and per-flat provider grants before requesting approval again.',
    provider_not_ready: 'The provider is still connecting or needs setup. Check its connection before requesting approval again.',
    provider_unavailable: 'The provider is unavailable. Check host configuration before requesting approval again.',
    runtime_unavailable: 'Server flats are disabled on this host. Enable the runtime before requesting approval again.',
    unavailable: 'This feature is unavailable on this host.',
    provider_in_use: 'This provider still has registered routes. Approve Private access before removing a Public provider. If stopping Tailscale failed, its permission stays allowed; retry after checking the connection.',
    not_deployed: 'No current runtime is serving this flat. Activate a published version before requesting Public access.',
    public_stop_unconfirmed: 'Could not confirm the public route is blocked. Access is not shown as Private.',
    unchanged_content: 'The draft matches the current published content. No new version was published.',
    stale_approval: 'The content or access settings changed. Review again.',
  };
  return messages[failureCode(source)] || '';
}

function failedKind(source) {
  const code = failureCode(source);
  return code ? code === 'stale_approval' : /stale/i.test(`${source?.result || ''} ${source?.error || ''}`);
}

export async function approvePending(res) {
  if (res?.status === 'pending_approval' && res.approval?.id) {
    return api.decide(res.approval.id, true);
  }
  return res;
}

function policyLines(flat, draft) {
  const current = flat.publication === 'published' && flat.live_version
    ? `Current published version: v${flat.live_version}.`
    : 'No published version yet. The first successful approval creates v1.';
  const providers = permittedProviders(flat).map((p) => p.label).join(', ');
  const link = currentTarget(flat);
  return [
    h('p', { text: `Candidate draft revision ${draft.revision}, hash ${shortHash(draft.hash, 16) || 'not reported'}.` }),
    h('p', { text: current }),
    h('p', { text: `Access stays ${visibilityWord(flat.visibility)}. Publishing does not change who can open the flat.` }),
    h('p', { text: `Permitted providers: ${providers}.` }),
    h('p', { text: link.ready
      ? `Current version link is openable (${connectionLabel(link.state)}).`
      : `Current connection: ${connectionLabel(link.state)}. An address is not treated as reachable until the connection is ready.` }),
    h('p', { text: CHECK_COPY }),
    draft.kind !== 'static' ? h('p', { text: 'Starting a server runtime may write to live DB and FILES, even if activation fails.' }) : null,
    h('p', { class: 'muted', text: 'You are approving this in the console. The draft revision above is the one that will be published.' }),
  ];
}

export async function publishDraft(flat) {
  const draft = flat?.draft;
  if (!draft?.revision || !draft.hash) {
    await infoDialog('Nothing to publish', 'Upload draft files first. Saving a draft does not create a version.');
    return null;
  }
  const ok = await confirmDialog({
    title: `Publish the next version of ${nameOf(flat)}?`,
    body: policyLines(flat, draft),
    confirmLabel: 'Approve and publish',
  });
  if (!ok) return null;
  let created;
  try {
    created = await api.publish(flat.slug, { revision: draft.revision, hash: draft.hash });
  } catch (err) {
    await infoDialog('Publish did not complete', [h('p', { text: err.message }), h('p', { text: impactText(err.body) })]);
    announce('Publish did not complete. ' + err.message);
    return null;
  }
  if (created?.status !== 'pending_approval') {
    await infoDialog('Publish was not approved', 'The server did not return a pending approval, so this console did not mark a version published.');
    announce('Publish was not approved.');
    return null;
  }
  return settle('publish', created, flat);
}

async function settle(kind, created, flat) {
  let decision;
  try {
    decision = await approvePending(created);
  } catch (err) {
    const stale = failedKind(err.body);
    const title = failureMessage(err.body) || (stale ? 'The content or access settings changed. Review again.' : 'The approval failed');
    await infoDialog(title, [h('p', { text: err.message }), h('p', { text: impactText(err.body) })]);
    announce(title);
    return { created, error: err, stale, message: title };
  }
  const status = decision?.status || '';
  if (status === 'failed' || status === 'rejected') {
    const stale = failedKind(decision);
    const title = failureMessage(decision) || (stale
      ? 'The content or access settings changed. Review again.'
      : status === 'rejected' ? 'The request was rejected. The draft is unchanged.' : 'Publish did not complete');
    await infoDialog(title, [h('p', { text: decision.result || title }), h('p', { text: impactText(decision) })]);
    announce(title);
    return { created, decision, stale };
  }
  const version = decision?.live_version || flat.live_version;
  const msg = status === 'approved'
    ? `Approval ${decision.id || ''} completed. The current published version stays in place until you reload it.`
    : 'The approval finished.';
  await infoDialog(kind === 'publish' ? 'Publish approval recorded' : 'Approval recorded', [
    h('p', { text: decision?.result || msg }),
    h('p', { text: impactText(decision) }),
    version ? h('p', { class: 'muted', text: `Reload to see the current version. Last known current version: v${version}.` }) : null,
  ]);
  announce(decision?.result || 'Approval completed.');
  return { created, decision };
}

export async function changeVisibility(flat, next) {
  if (!next || next === flat.visibility) return null;
  if (next === 'public' && flat.publication !== 'published') {
    await infoDialog('Publish a version first', 'Public access applies to a published version. Publish v1 before making this flat Public.');
    announce('Publish a version before making this flat Public.');
    return null;
  }
  const toPublic = next === 'public';
  const version = flat.live_version ? `v${flat.live_version}` : 'the current published version';
  const ok = await confirmDialog({
    title: toPublic ? `Make ${nameOf(flat)} public?` : `Make ${nameOf(flat)} private?`,
    body: toPublic ? [
      h('p', { text: `${version} would be reachable on the internet through a permitted public provider (Tailscale Funnel or Portal).` }),
      h('p', { text: 'The version number does not change. Drafts stay on a private path.' }),
      h('p', { text: 'Funnel visitors do not need Tailscale. Portal is also an internet path.' }),
    ] : [
      h('p', { text: `Public routes for ${version} would be blocked. The version number does not change.` }),
      h('p', { text: 'Private Tailscale access still follows your tailnet ACL, including other people and devices that ACL allows. Localhost still works on this device.' }),
      h('p', { text: 'Private is shown only after the public routes are confirmed blocked.' }),
    ],
    confirmLabel: toPublic ? 'Approve making it public' : 'Approve making it private',
    danger: toPublic,
  });
  if (!ok) return null;
  let created;
  try {
    created = await api.setVisibility(flat.slug, next);
  } catch (err) {
    await infoDialog('Access change did not complete', err.message);
    announce(err.message);
    return null;
  }
  if (created?.status !== 'pending_approval') {
    return { created, unchanged: true };
  }
  return settle(toPublic ? 'public' : 'private', created, flat);
}

export function visibilitySettled(flat, requested, outcome) {
  if (!outcome || outcome.stale || outcome.error) return { ok: false, text: outcome?.message || (outcome?.stale ? 'The content or access settings changed. Review again.' : 'The access change did not complete.') };
  if (outcome.decision?.status === 'rejected') return { ok: false, text: 'The access request was rejected. The previous access remains.' };
  if (outcome.decision?.status === 'failed') {
    return { ok: false, text: failureMessage(outcome.decision) || (/block|unconfirmed|public route/i.test(outcome.decision.result || '')
      ? 'Could not confirm the public route is blocked. Access is not shown as Private.'
      : (outcome.decision.result || 'The access change failed.')) };
  }
  if (requested === 'private' && flat?.visibility === 'public') {
    return { ok: false, text: 'Could not confirm the public route is blocked. Access is not shown as Private.' };
  }
  return { ok: true, text: `Access is ${visibilityWord(flat?.visibility)}.` };
}

export async function saveProvider(flat, provider, permitted) {
  const desc = providerById(provider);
  if (!desc) return null;
  if (provider === 'local' && !permitted) {
    await infoDialog('Local stays available', 'Local loopback access stays permitted. It does not publish a version or change Private or Public.');
    return null;
  }
  const ok = await confirmDialog({
    title: permitted ? `Allow ${desc.label} for ${nameOf(flat)}?` : `Stop allowing ${desc.label}?`,
    body: [
      h('p', { text: permitted
        ? `This allows ${desc.label} for this flat. ${desc.audience}`
        : provider === 'tailscale'
          ? 'This stops this flat’s Tailscale current, preview and redirect routes before removing permission. Local stays available. Permission stays allowed if stopping cannot be confirmed.'
          : `Public routes must first be stopped by an approved change to Private before removing permission for ${desc.label}.` }),
      h('p', { text: 'Provider permission does not publish a version and does not change Private or Public.' }),
      h('p', { class: 'muted', text: 'A connected provider is not the same as a published or reachable flat. A failure does not switch this flat to another provider.' }),
    ],
    confirmLabel: permitted ? 'Allow for this flat' : 'Remove permission',
  });
  if (!ok) return null;
  try {
    const res = await api.setProvider(flat.slug, provider, permitted);
    announce(permitted ? `${desc.label} is allowed for this flat.` : `${desc.label} is no longer allowed for this flat.`);
    toast(permitted ? `${desc.label} allowed for this flat.` : `${desc.label} permission removed.`, 'success');
    return res;
  } catch (err) {
    await infoDialog('Provider permission did not save', failureMessage(err.body) || err.message);
    announce('Provider permission did not save. ' + err.message);
    return null;
  }
}

export async function activateVersion(flat, version, opts = {}) {
  const rollback = !!opts.rollback;
  const restore = !!opts.restoreData;
  const restoreInput = rollback ? h('input', { type: 'checkbox', id: 'rollback-restore-data', checked: restore }) : null;
  const ok = await confirmDialog({
    title: rollback ? `Roll back ${nameOf(flat)} to v${version}?` : `Make v${version} the current version of ${nameOf(flat)}?`,
    body: [
      h('p', { text: `This serves published v${version} again. It does not create a new version number.` }),
      h('p', { text: restore
        ? 'This approval also restores data from the snapshot taken before the current version. New snapshots include the database and FILES; historical DB-only snapshots preserve current FILES.'
        : 'Without the restore option below, live DB and FILES stay in place. Server runtime start may still write to them.' }),
      h('p', { text: CHECK_COPY }),
      h('p', { text: 'For a server candidate, starting the runtime may write to live DB and FILES, even if activation fails.' }),
      restoreInput ? h('div', { class: 'field field-check' }, restoreInput, h('label', { for: 'rollback-restore-data', text: 'Also restore live data from before the current version (current live data is backed up first; captured DB and FILES are replaced, historical DB-only snapshots preserve FILES).' })) : null,
    ],
    confirmLabel: rollback ? 'Approve rollback' : 'Approve and make current',
    danger: restore,
  });
  if (!ok) return null;
  let created;
  try {
    created = rollback
      ? await api.rollback(flat.slug, version, !!restoreInput?.checked)
      : await api.deploy(flat.slug, version);
  } catch (err) {
    await infoDialog('The version change did not complete', [h('p', { text: err.message }), h('p', { text: impactText(err.body) })]);
    return null;
  }
  if (created?.status !== 'pending_approval') {
    await infoDialog('Approval was not created', 'The server did not return a pending approval, so the current version was not changed from this console.');
    return null;
  }
  if (restoreInput?.checked || created.approval?.params?.restore_data) {
    let p = created.approval?.params || {};
    if (typeof p === 'string') { try { p = JSON.parse(p); } catch { p = {}; } }
    if (!p.snapshot || !p.snapshot_hash) {
      await infoDialog('Review the pending restore request', 'Snapshot identity was not returned. Open its approval page to review before deciding.');
      return { created, pending: true };
    }
    if (p.restore_data) {
      const confirmed = await confirmDialog({
        title: 'Approve rollback with live data restore?',
        body: [h('p', { text: `Snapshot: ${p.snapshot || 'not reported'} · hash ${p.snapshot_hash || 'not reported'}` }),
          h('p', { text: 'Replaces live DB and captured FILES after backing up current live data. Historical DB-only snapshots preserve current FILES.' })],
        confirmLabel: 'Approve and restore live data', danger: true,
      });
      if (!confirmed) return { created, pending: true };
    }
  }
  return settle('activate', created, flat);
}

export function openControl(target, label) {
  if (!target?.ready || !target.url) {
    const why = target?.url
      ? `${connectionLabel(target.state)}. The address is not opened while the connection is still ${connectionLabel(target.state).toLowerCase()}.`
      : 'No address is ready.';
    return h('span', { class: 'muted small', text: `${label}: ${why}` });
  }
  return extLink(target.url, label);
}

export function renderCurrent(flat, onRestart) {
  const link = currentTarget(flat);
  const published = flat.publication === 'published' && flat.live_version;
  const body = published ? [
    h('p', { text: `v${flat.live_version} · ${visibilityWord(flat.visibility)} · ${connectionLabel(link.state)}` }),
    connectionDetail(flat) ? h('p', { class: 'muted small', text: connectionDetail(flat) }) : null,
    h('p', { class: 'muted small', text: link.visibility === 'public'
      ? 'Anyone on the internet can open the current version when a public provider is connected.'
      : (link.state && flat.private_state === undefined && permittedProviders(flat).some((p) => p.id === 'tailscale')
        ? 'Private Tailscale access follows your tailnet ACL. Localhost works on this device.'
        : 'Private access is the current device through localhost, or people and devices allowed by your tailnet ACL when Tailscale is permitted.') }),
    h('div', { class: 'life-actions' },
      openControl(link, `Open current version v${flat.live_version}`),
      onRestart ? restartButton(flat, onRestart) : null),
  ] : [
    h('p', { text: 'No published version yet.' }),
    h('p', { class: 'muted', text: 'Publishing the draft creates v1 after you approve it. Until then there is no current version to open.' }),
  ];
  return h('section', { class: 'card life-card', id: 'current-version' },
    h('h2', { text: 'Current version' }), ...body);
}

function restartButton(flat, onRestart) {
  const btn = h('button', {
    type: 'button', class: 'btn btn-small', text: 'Redeploy (apply secrets)',
    'aria-label': `Redeploy current version ${flat.live_version} to apply secrets`,
  });
  btn.addEventListener('click', () => onRestart(btn));
  return btn;
}

export function renderDraft(flat, preview, handlers) {
  const draft = flat.draft;
  const target = draftTarget(preview);
  const base = draft?.base_version ? `Based on v${draft.base_version}` : 'Not based on a published version';
  const dirty = draft?.dirty ? 'Draft changes' : draft ? 'No unpublished file changes reported' : 'No draft';
  const save = handlers.saveState || (draft ? 'Saved' : '');
  return h('section', { class: 'card life-card', id: 'draft', tabindex: '-1' },
    h('h2', { text: 'Draft' }),
    h('p', { text: draft
      ? `${base} · revision ${draft.revision} · ${dirty} · Private`
      : 'No draft yet · Private' }),
    h('p', { class: 'muted small', text: 'A draft is always private. Visitors of a public flat keep seeing the current version. Tailscale draft access follows your tailnet ACL. Localhost works on this device.' }),
    h('p', { class: 'draft-save', role: 'status', 'aria-live': 'polite', text: save }),
    handlers.editor,
    h('div', { class: 'life-actions' },
      target.ready ? openControl(target, 'Open draft (private)') : handlers.previewButton,
      handlers.publishButton));
}

export function draftEditor(flat, onUploaded) {
  let lastFile = null;
  let message = '';
  let revision = flat.draft?.revision ?? 0;
  let conflicted = false;
  const status = { text: '' };
  const fileId = 'draft-archive';
  const msgId = 'draft-message';
  const input = h('input', { id: fileId, type: 'file', accept: '.tar,.tar.gz,.tgz,.gz,.zip,application/gzip,application/x-tar,application/zip' });
  const note = h('input', {
    id: msgId, type: 'text', maxlength: '200', autocomplete: 'off',
    placeholder: 'Optional note stored with the next upload',
  });
  const retry = h('button', { type: 'button', class: 'btn btn-small', text: 'Retry draft save', hidden: true });
  note.addEventListener('input', () => { message = note.value; });
  async function upload(file) {
    status.text = 'Saving draft…';
    onUploaded({ pending: true, status: status.text });
    retry.hidden = true;
    try {
      if (conflicted) {
        const latest = await api.flat(flat.slug);
        revision = latest.draft?.revision ?? 0;
        conflicted = false;
      }
      const res = await api.saveDraft(flat.slug, file, {
        expectedRevision: revision,
        message: message.trim(),
      });
      revision = res.draft?.revision ?? res.version?.revision ?? revision;
      status.text = `Saved draft revision ${res.draft?.revision ?? res.version?.revision ?? ''}. This did not publish a version.`;
      announce(status.text);
      onUploaded({ pending: false, status: status.text, response: res });
    } catch (err) {
      const conflict = err.status === 409;
      conflicted = conflict;
      status.text = conflict
        ? 'The draft changed somewhere else. Review it before uploading again. Your selected file was kept.'
        : 'Could not save the draft. Your selected file was kept.';
      retry.hidden = false;
      announce(status.text);
      onUploaded({ pending: false, status: status.text, error: err, conflict });
    }
  }
  input.addEventListener('change', () => {
    const file = input.files && input.files[0];
    if (!file) return;
    lastFile = file;
    upload(file);
  });
  retry.addEventListener('click', () => { if (lastFile) upload(lastFile); });
  const editor = h('div', { class: 'draft-editor' },
    h('div', { class: 'field' },
      h('label', { for: fileId, text: 'Draft files' }),
      input,
      h('span', { class: 'hint', text: 'Upload a tar, tar.gz, or zip of the flat. The upload saves when you choose a file. It does not publish.' })),
    h('div', { class: 'field' },
      h('label', { for: msgId, text: 'Save message' }),
      note,
      h('span', { class: 'hint', text: 'Metadata recorded with the next file upload. It is not the flat content and it does not save on its own.' })),
    retry);
  return { editor, status: () => status.text };
}

export function renderAccess(flat, controls) {
  const state = connectionState(flat);
  return h('div', { class: 'access-panel', id: 'access' },
    h('section', { class: 'card', 'aria-labelledby': 'access-range-title' },
      h('h2', { id: 'access-range-title', text: 'Access' }),
      h('p', { class: 'muted', text: 'Private and Public describe who may open the current published version. They are separate from publishing and from which provider is allowed.' }),
      controls.visibility),
    h('section', { class: 'card', 'aria-labelledby': 'provider-title' },
      h('h2', { id: 'provider-title', text: 'Provider' }),
      h('p', { class: 'muted', text: 'Allow a provider here after you mean this flat to use it. Connecting Tailscale does not allow Funnel, and allowing a provider does not publish or change access.' }),
      controls.providers),
    h('section', { class: 'card', 'aria-labelledby': 'address-title' },
      h('h2', { id: 'address-title', text: 'Addresses' }),
      h('p', { text: `Connection: ${connectionLabel(state)}. ${state === 'ready' ? 'Connected describes the provider path. Publication is shown separately.' : 'The address stays closed until the connection is ready.'}` }),
      connectionDetail(flat) ? h('p', { class: 'muted small', text: connectionDetail(flat) }) : null,
      Array.isArray(flat.endpoints) && flat.endpoints.length ? h('ul', { class: 'plain-list' }, flat.endpoints.map((ep) =>
        h('li', { class: 'plain-row' },
          h('div', null,
            h('strong', { text: `${providerLabel(ep.provider)} · ${ep.audience === 'draft' ? 'Draft (Private)' : 'Current version'}` }),
            h('p', { class: 'muted small', text: `${ep.configured ? 'Configured' : 'Needs setup'} · ${ep.permitted ? 'Route permitted' : 'Route not permitted'} · ${connectionLabel(ep.state)}` }),
            ep.detail ? h('p', { class: 'muted small', text: ep.detail }) : null,
            openControl({url:ep.url, ready:ep.ready, state:ep.state}, `Open ${providerLabel(ep.provider)} ${ep.audience === 'draft' ? 'draft' : 'current version'}`)))) ) : null,
      controls.addresses));
}

export function renderHistory(flat, versions, approvals, actions) {
  const rows = (versions || []).map((v) => {
    const current = v.number === flat.live_version;
    return h('tr', { class: current ? 'is-current' : '' },
      h('th', { scope: 'row' }, `v${v.number}`, current ? h('span', { class: 'badge badge-live', text: 'Current' }) : null),
      h('td', null, timeEl(v.created_at)),
      h('td', null, h('code', { title: v.hash, text: shortHash(v.hash) })),
      h('td', { text: `${bytes(v.size)} · ${v.kind || ''}` }),
      h('td', null, actions.versionActions(v, current)));
  });
  const attempts = (approvals || []).filter((a) => a.flat === flat.slug && a.status !== 'approved');
  return h('div', { id: 'history' },
    h('section', { class: 'card' },
      h('h2', { text: 'Published versions' }),
      rows.length ? h('div', { class: 'table-wrap' }, h('table', { class: 'table' },
        h('caption', { class: 'sr-only', text: 'Published versions' }),
        h('thead', null, h('tr', null, ['Version', 'Published', 'Hash', 'Size', 'Actions'].map((t) => h('th', { scope: 'col', text: t })))),
        h('tbody', null, rows)))
        : h('p', { class: 'muted', text: 'No published version yet. Draft uploads are not listed here.' })),
    h('section', { class: 'card' },
      h('h2', { text: 'Approvals that did not publish' }),
      attempts.length ? h('ul', { class: 'plain-list' }, attempts.map(attemptItem))
        : h('p', { class: 'muted', text: 'No rejected or failed approvals in the recent list.' })));
}

function attemptItem(a) {
  const when = a.requested_at ? dateTime(a.requested_at) : '';
  const label = a.status === 'rejected'
    ? 'The request was rejected. The draft is unchanged.'
    : a.status === 'pending'
      ? 'Waiting for approval. The current version is unchanged.'
      : /stale/i.test(a.result || '')
        ? 'The content or access settings changed. Review again.'
        : 'This attempt did not publish a version.';
  return h('li', { class: 'plain-row' },
    h('div', null,
      h('strong', { text: `${a.action || 'request'} · ${a.status}` }),
      h('div', { class: 'muted small', text: `${label} ${a.result || ''} ${when}` })));
}

export function pendingItems(flat, approvals) {
  return (approvals || []).filter((a) => a.flat === flat.slug && a.status === 'pending');
}
