// Flat actions shared by the list and the flat page. Each asks for
// confirmation first and reports the outcome; they resolve true when the
// server state changed.

import { h, dateTime, VISIBILITY, visibilityOf, noticeFor, publicURL, publicNoticeOf } from './dom.js';
import { api } from './api.js';
import { confirmDialog, infoDialog, errorPanel, toast, extLink } from './ui.js';

const label = (f) => f.name || f.slug;

async function report(title, err) {
  await infoDialog(title, errorPanel(err, title));
}

// approve completes a request this console just made. Publishing, deploys,
// rollbacks, visibility changes and deletion wait for the operator's
// approval; the confirm dialog before each request is that decision, so the
// pending request is approved at once. Resolves to the decided approval.
async function approve(res) {
  if (res?.status !== 'pending_approval' || !res.approval?.id) return res;
  return api.decide(res.approval.id, true);
}

// restoreBox is the opt-in "restore data" checkbox shown on rollbacks.
function restoreBox(flat) {
  const id = 'restore-' + flat.slug;
  const input = h('input', { type: 'checkbox', id });
  const row = h('div', { class: 'field field-check' }, input,
    h('label', { for: id, text: `Also restore the data as it was before version ${flat.live_version} was deployed (server flats only)` }));
  return { input, row };
}

export async function deployVersion(flat, n, kind = 'deploy') {
  const rollback = kind === 'rollback';
  const box = rollback ? restoreBox(flat) : null;
  const ok = await confirmDialog({
    title: rollback ? `Roll back to version ${n}?` : `Deploy version ${n}?`,
    body: [
      h('p', { text: `${label(flat)} will serve version ${n}` + (flat.live_version ? ` instead of version ${flat.live_version}.` : '.') }),
      h('p', { class: 'muted', text: 'The health check runs first; if it fails, the current live version keeps serving. Data is not rolled back unless you ask for it.' }),
      box ? box.row : null,
    ],
    confirmLabel: rollback ? 'Roll back' : 'Deploy',
  });
  if (!ok) return false;
  try {
    await approve(rollback ? await api.rollback(flat.slug, n, box.input.checked) : await api.deploy(flat.slug, n));
    await showLive(flat.slug, 'is live');
    return true;
  } catch (err) {
    await report(rollback ? 'Roll back failed' : 'Deploy failed', err);
    return false;
  }
}

// publishDraft publishes the flat's draft as its next version.
export async function publishDraft(flat) {
  const draft = flat.draft;
  if (!draft || !draft.dirty) {
    await infoDialog('Nothing to publish', 'The draft has no changes since the live version. An agent saves one with save_version (MCP) or `flats deploy`.');
    return false;
  }
  const ok = await confirmDialog({
    title: `Publish ${label(flat)}?`,
    body: [
      h('p', { text: `The draft becomes a new version and ${label(flat)} serves it` + (flat.live_version ? ` instead of version ${flat.live_version}.` : '.') }),
      h('p', { class: 'muted', text: 'The health check runs first; if it fails, nothing is published.' }),
    ],
    confirmLabel: 'Publish',
  });
  if (!ok) return false;
  try {
    await approve(await api.publish(flat.slug, draft));
    await showLive(flat.slug, 'is live');
    return true;
  } catch (err) {
    await report('Publish failed', err);
    return false;
  }
}

async function showLive(slug, what) {
  const f = await api.flat(slug).catch(() => null);
  if (!f) { toast('Done.', 'success'); return; }
  const pub = publicURL(f);
  await infoDialog(`Version ${f.live_version} ${what}`, [
    f.private_url ? h('p', null, 'Private URL: ', extLink(f.private_url)) : null,
    pub ? h('p', null, 'Public URL: ', extLink(pub)) : null,
    pub ? h('p', { class: 'notice-text', text: publicNoticeOf(f) }) : null,
  ]);
}

// redeployLive restarts the live version so it picks up changed environment variables and secrets
// (approval captures one snapshot for health checking and live startup).
export async function redeployLive(flat) {
  const n = flat.live_version;
  if (!n) return false;
  const server = !flat.live || flat.live.kind === 'server';
  const ok = await confirmDialog({
    title: `Redeploy version ${n}?`,
    body: [
      h('p', { text: `${label(flat)} redeploys version ${n} with environment variables and secrets captured when approved activation begins.` }),
      h('p', { class: 'muted', text: server
        ? 'The health check and live worker use the same settings snapshot. Changes saved after capture apply at the next activation. If the health check fails, the running instance keeps serving. Data is kept as it is.'
        : 'This is a static flat: environment variables and secrets are not injected, so it keeps serving the same files.' }),
    ],
    confirmLabel: 'Redeploy',
  });
  if (!ok) return false;
  try {
    await approve(await api.deploy(flat.slug, n));
    await showLive(flat.slug, 'was redeployed');
    return true;
  } catch (err) {
    await report('Redeploy failed', err);
    return false;
  }
}

// previewVersion opens a private preview of version n, or of the draft when
// n is 0.
export async function previewVersion(flat, n) {
  try {
    const p = await api.openPreview(flat.slug, n);
    await infoDialog(n ? `Preview of version ${p.version}` : 'Preview of the draft', [
      h('p', null, extLink(p.url)),
      h('p', { class: 'muted', text: `Private, on its own address. It closes when ${label(flat)} is deployed, or if nobody opens it before ${dateTime(p.expires_at)}.` }),
    ]);
    return true;
  } catch (err) {
    await report('Preview failed', err);
    return false;
  }
}

// deleteFlat resolves true when the flat is gone.
export async function deleteFlat(flat) {
  const ok = await confirmDialog({
    title: `Delete ${label(flat)}?`,
    body: [
      h('p', { text: 'This permanently deletes the flat, every version, its data, environment variables, secrets and logs, and takes its private and public addresses offline.' }),
      h('p', { class: 'muted', text: 'This cannot be undone.' }),
    ],
    confirmLabel: 'Delete permanently',
    danger: true,
    requireText: flat.slug,
  });
  if (!ok) return false;
  try {
    await approve(await api.remove(flat.slug));
    toast(`${flat.slug} was deleted.`, 'success');
    return true;
  } catch (err) {
    await report('Delete failed', err);
    return false;
  }
}

// setVisibility changes visibility after a confirm; widening shows the notice.
export async function setVisibility(flat, vis) {
  const from = VISIBILITY[visibilityOf(flat.visibility)];
  const to = VISIBILITY[vis];
  const widening = to.rank > from.rank;
  if (widening && !flat.live_version) {
    await infoDialog('Publish first', `${label(flat)} has no live version yet. Publish it, then make it public.`);
    return null;
  }
  if (widening && !PUBLIC_PROVIDERS.some((p) => (flat.providers || []).includes(p))) {
    await infoDialog('Allow a public network first', `Allow Tailscale Funnel or Portal for ${label(flat)} under Networks, then make it public.`);
    return null;
  }
  const notice = noticeFor(vis);
  const ok = await confirmDialog({
    title: `Make ${label(flat)} ${to.label.toLowerCase()}?`,
    body: [
      h('p', { text: `Visibility changes from ${from.label} to ${to.label}.` }),
      widening && notice ? h('p', { class: 'alert alert-warn', text: notice }) : null,
      vis === 'private' ? h('p', { class: 'muted', text: 'The public address goes offline. The private address keeps working.' }) : null,
    ],
    confirmLabel: widening ? `Make ${to.label.toLowerCase()}` : 'Change visibility',
    danger: widening,
  });
  if (!ok) return null;
  try {
    const res = await approve(await api.setVisibility(flat.slug, vis));
    toast(res.result ? res.result[0].toUpperCase() + res.result.slice(1) + '.' : 'Visibility changed.', 'success');
    return res;
  } catch (err) {
    await report('Visibility change failed', err);
    return null;
  }
}

// Networks a flat can be served on besides this device (Local, always on).
// Each must be turned on for the host in Settings and then allowed per flat;
// neither step publishes a flat or makes it public.
export const PROVIDERS = [
  { id: 'tailscale', scope: 'private', label: 'Tailscale', hint: 'Devices your tailnet ACL allows' },
  { id: 'tailscale-funnel', scope: 'public', label: 'Tailscale Funnel', hint: 'Anyone on the internet' },
  { id: 'portal', scope: 'public', label: 'Portal', hint: 'Anyone on the internet, through Portal relays' },
];
const PUBLIC_PROVIDERS = ['tailscale-funnel', 'portal'];

// setProvider allows or stops a network for one flat; resolves true on success.
export async function setProvider(flat, provider, permitted) {
  const p = PROVIDERS.find((x) => x.id === provider);
  try {
    await api.setFlatProvider(flat.slug, provider, permitted);
    toast(`${p.label} ${permitted ? 'allowed' : 'no longer allowed'} for ${label(flat)}.`, 'success');
    return true;
  } catch (err) {
    await report(`Cannot change ${p.label}`, err);
    return false;
  }
}
