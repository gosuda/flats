// Flat actions shared by the list and the flat page. Each asks for
// confirmation first and reports the outcome; they resolve true when the
// server state changed.

import { h, dateTime, VISIBILITY, noticeFor } from './dom.js';
import { api, newestVersion } from './api.js';
import { confirmDialog, infoDialog, errorPanel, healthBlock, toast, extLink } from './ui.js';

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
      h('p', { class: 'muted', text: 'The health check runs first; if it fails, the current live version keeps serving. Data is not rolled back unless you ask for it.' }),
      box ? box.row : null,
    ],
    confirmLabel: rollback ? 'Roll back' : 'Deploy',
  });
  if (!ok) return false;
  try {
    const res = rollback ? await api.rollback(flat.slug, n, box.input.checked) : await api.deploy(flat.slug, n);
    await infoDialog(`Version ${res.version} is live`, deployDone(res));
    return true;
  } catch (err) {
    await report(rollback ? 'Roll back failed' : 'Deploy failed', err);
    return false;
  }
}

function deployDone(res) {
  const f = res.flat || {};
  return [
    healthBlock(res.health),
    res.previous ? h('p', { class: 'muted', text: `Previously live: version ${res.previous}.` }) : null,
    f.private_url ? h('p', null, 'Private URL: ', extLink(f.private_url)) : null,
    f.public_url ? h('p', null, 'Public URL: ', extLink(f.public_url)) : null,
  ];
}

// publishLatest deploys the newest saved version.
export async function publishLatest(flat) {
  let n;
  try { n = await newestVersion(flat.slug); } catch (err) { await report('Cannot list versions', err); return false; }
  if (!n) {
    await infoDialog('Nothing to publish', 'This flat has no saved versions yet. An agent saves one with save_version (MCP) or `flats deploy`.');
    return false;
  }
  return deployVersion(flat, n);
}

export async function rollbackPrevious(flat) {
  const box = restoreBox(flat);
  const ok = await confirmDialog({
    title: 'Roll back to the previous version?',
    body: [
      h('p', { text: `${label(flat)} will serve the version that was live before version ${flat.live_version}.` }),
      h('p', { class: 'muted', text: 'The health check runs first. Only code is rolled back unless you also restore the data.' }),
      box.row,
    ],
    confirmLabel: 'Roll back',
  });
  if (!ok) return false;
  try {
    const res = await api.rollback(flat.slug, 0, box.input.checked);
    toast(`Rolled back: version ${res.version} is live.`, 'success');
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

// setVisibility changes visibility after a confirm; widening shows the notice.
export async function setVisibility(flat, vis) {
  const from = VISIBILITY[flat.visibility] || { rank: 0, label: flat.visibility };
  const to = VISIBILITY[vis];
  const widening = to.rank > from.rank;
  const notice = noticeFor(vis);
  const ok = await confirmDialog({
    title: `Make ${label(flat)} ${to.label.toLowerCase()}?`,
    body: [
      h('p', { text: `Visibility changes from ${from.label} to ${to.label}.` }),
      widening && notice ? h('p', { class: 'alert alert-warn', text: notice }) : null,
      vis === 'private' ? h('p', { class: 'muted', text: 'The public address goes offline. The private address keeps working.' }) : null,
      !flat.live_version && vis !== 'private' ? h('p', { class: 'muted', text: 'The flat goes public when its first version is deployed.' }) : null,
    ],
    confirmLabel: widening ? `Make ${to.label.toLowerCase()}` : 'Change visibility',
    danger: widening,
  });
  if (!ok) return null;
  try {
    const res = await api.setVisibility(flat.slug, vis);
    toast(res.message || 'Visibility changed.', 'success');
    return res;
  } catch (err) {
    await report('Visibility change failed', err);
    return null;
  }
}
