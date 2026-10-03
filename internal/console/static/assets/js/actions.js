// Flat actions shared by the list and the flat page. Each asks for
// confirmation first and reports the outcome; they resolve true when the
// server state changed.

import { h, dateTime, publicNoticeOf } from './dom.js';
import { api, newestVersion } from './api.js';
import { confirmDialog, infoDialog, errorPanel, healthBlock, toast, extLink, announce } from './ui.js';
import { CHECK_COPY, approvePending, changeVisibility, impactText, publishDraft } from './lifecycle.js';

const label = (f) => f.name || f.slug;

async function report(title, err) {
  await infoDialog(title, errorPanel(err, title));
}

// restoreBox is the opt-in "restore data" checkbox shown on rollbacks.
function restoreBox(flat) {
  const id = 'restore-' + flat.slug;
  const input = h('input', { type: 'checkbox', id });
  const row = h('div', { class: 'field field-check' }, input,
    h('label', { for: id, text: `Also restore the database as it was before version ${flat.live_version} was deployed (server flats only)` }));
  return { input, row };
}

export async function deployVersion(flat, n, kind = 'deploy') {
  const rollback = kind === 'rollback';
  const box = rollback ? restoreBox(flat) : null;
  const ok = await confirmDialog({
    title: rollback ? `Roll back to version ${n}?` : `Deploy version ${n}?`,
    body: [
      h('p', { text: `${label(flat)} will serve version ${n}` + (flat.live_version ? ` instead of version ${flat.live_version}.` : '.') }),
      h('p', { class: 'muted', text: CHECK_COPY + ' The version number does not change when you serve an existing published version again.' }),
      box ? box.row : null,
    ],
    confirmLabel: rollback ? 'Roll back' : 'Deploy',
  });
  if (!ok) return false;
  try {
    const res = rollback ? await api.rollback(flat.slug, n, box.input.checked) : await api.deploy(flat.slug, n);
    const decision = await approvePending(res);
    await infoDialog(decision?.status === 'approved' ? `Version ${n} approval completed` : 'Approval recorded', deployDone(res, decision, flat));
    announce(decision?.result || 'Approval completed.');
    return true;
  } catch (err) {
    await report(rollback ? 'Roll back failed' : 'The version change failed', err);
    return false;
  }
}

function deployDone(res, decision, flat) {
  const f = (res && res.flat) || flat || {};
  const reported = decision || res || {};
  return [
    res && res.health ? healthBlock(res.health) : null,
    h('p', { text: impactText(reported) }),
    res && res.previous ? h('p', { class: 'muted', text: `Previously live: version ${res.previous}.` }) : null,
    f.private_url ? h('p', null, 'Private URL: ', extLink(f.private_url)) : null,
    f.public_url ? h('p', null, 'Public URL: ', extLink(f.public_url)) : null,
    f.public_url ? h('p', { class: 'notice-text', text: publicNoticeOf(f) }) : null,
  ];
}

// redeployLive restarts the live version so it picks up changed secrets
// (a worker reads them only when it starts).
export async function redeployLive(flat) {
  const n = flat.live_version;
  if (!n) return false;
  const server = !flat.live || flat.live.kind === 'server';
  const ok = await confirmDialog({
    title: `Redeploy version ${n}?`,
    body: [
      h('p', { text: `${label(flat)} restarts version ${n} with its current secrets.` }),
      h('p', { class: 'muted', text: server
        ? CHECK_COPY + ' This restarts the current published version so it reads secrets. It does not create a new version.'
        : 'This is a static flat: secrets are not used. Restarting does not create a new version. ' + CHECK_COPY }),
    ],
    confirmLabel: 'Redeploy',
  });
  if (!ok) return false;
  try {
    const res = await api.deploy(flat.slug, n);
    const decision = await approvePending(res);
    await infoDialog(`Version ${n} restart requested`, deployDone(res, decision, flat));
    announce(decision?.result || `Restart of version ${n} was submitted.`);
    return decision?.status !== 'failed' && decision?.status !== 'rejected';
  } catch (err) {
    await report('Redeploy failed', err);
    return false;
  }
}

// publishLatest publishes the mutable draft after the same confirmation as the flat page.
export function publishLatest(flat) {
  return publishDraft(flat);
}

export async function rollbackPrevious(flat) {
  const box = restoreBox(flat);
  const ok = await confirmDialog({
    title: 'Roll back to the previous version?',
    body: [
      h('p', { text: `${label(flat)} will serve the version that was live before version ${flat.live_version}.` }),
      h('p', { class: 'muted', text: CHECK_COPY + ' Only code changes unless you also restore the data. The version number does not increase.' }),
      box.row,
    ],
    confirmLabel: 'Roll back',
  });
  if (!ok) return false;
  try {
    const res = await api.rollback(flat.slug, 0, box.input.checked);
    const decision = await approvePending(res);
    if (decision?.status === 'failed' || decision?.status === 'rejected') {
      await report('Roll back failed', decision.error || decision.reason || 'The approval was not applied.');
      return false;
    }
    const n = decision?.version || res.version;
    toast(n ? `Rolled back: version ${n} is current.` : 'Roll back requested.', 'success');
    return true;
  } catch (err) {
    await report('Roll back failed', err);
    return false;
  }
}

export async function previewVersion(flat, n) {
  try {
    const p = await api.openPreview(flat.slug, n);
    await infoDialog(`Preview of version ${p.version}`, [
      h('p', null, extLink(p.url)),
      h('p', { class: 'muted', text: `Private, on its own address. It closes when ${label(flat)} is deployed, or if nobody opens it before ${dateTime(p.expires_at)}.` }),
    ]);
    return true;
  } catch (err) {
    await report('Preview failed', err);
    return false;
  }
}

export async function previewLatest(flat) {
  let n;
  try { n = await newestVersion(flat.slug); } catch (err) { await report('Cannot list versions', err); return false; }
  if (!n) {
    await infoDialog('Nothing to preview', 'This flat has no saved versions yet.');
    return false;
  }
  return previewVersion(flat, n);
}

// deleteFlat resolves true when the flat is gone.
export async function deleteFlat(flat) {
  const ok = await confirmDialog({
    title: `Delete ${label(flat)}?`,
    body: [
      h('p', { text: 'This permanently deletes the flat, every version, its data, secrets and logs, and takes its private and public addresses offline.' }),
      h('p', { class: 'muted', text: 'This cannot be undone.' }),
    ],
    confirmLabel: 'Delete permanently',
    danger: true,
    requireText: flat.slug,
  });
  if (!ok) return false;
  try {
    const res = await api.remove(flat.slug);
    if (res.status === 'pending_approval') {
      toast('Deletion is waiting for approval.');
      return false;
    }
    toast(res.message || `${flat.slug} was deleted.`, 'success');
    return true;
  } catch (err) {
    await report('Delete failed', err);
    return false;
  }
}

// setVisibility uses the Access confirmation. Publishing stays a separate action.
export function setVisibility(flat, vis) {
  return changeVisibility(flat, vis);
}
