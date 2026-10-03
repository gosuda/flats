// Flat actions shared by the list and the flat page. Each asks for
// confirmation first and reports the outcome; they resolve true when the
// server state changed.

import { h, dateTime, publicNoticeOf } from './dom.js';
import { api } from './api.js';
import { confirmDialog, infoDialog, errorPanel, healthBlock, toast, extLink, announce } from './ui.js';
import { CHECK_COPY, approvePending, impactText } from './lifecycle.js';

const label = (f) => f.name || f.slug;

async function report(title, err) {
  await infoDialog(title, errorPanel(err, title));
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
