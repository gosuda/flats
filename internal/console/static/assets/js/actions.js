// Flat actions shared by the list and the flat page. Each asks for
// confirmation first and reports the outcome; they resolve true when the
// server state changed.

import { h, dateTime, bytes, VISIBILITY, visibilityOf, accessOf, accessLabel, ACCESS, becomingNoticeFor, publicURL, publicNoticeOf } from './dom.js';
import { api } from './api.js';
import { confirmDialog, infoDialog, errorPanel, toast, extLink } from './ui.js';
import { isLive } from './list.js';

const label = (f) => f.name || f.slug;

async function report(title, err) {
  await infoDialog(title, errorPanel(err, title));
}

// approve completes a request this console just made. Saving a version,
// publishing, rollbacks, visibility changes and deletion wait for the operator's
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

// Two separate steps, each approved on its own (docs/spec/content-models.md):
//   Save    — the draft becomes a new numbered version. Nothing goes live.
//   Publish — one saved version becomes the live content. Rolling back is
//             publishing an earlier version again.

// nextVersion is the number the next saved version is expected to get.
export const nextVersion = (flat) => flat.next_version || (flat.latest_version || 0) + 1;

// publishVersion makes version n live; kind 'rollback' publishes an earlier
// version again and offers to restore its data.
export async function publishVersion(flat, n, kind = 'publish') {
  const rollback = kind === 'rollback';
  const box = rollback ? restoreBox(flat) : null;
  const ok = await confirmDialog({
    title: rollback ? `Roll back ${label(flat)} to version ${n}?` : `Publish version ${n} of ${label(flat)}?`,
    body: [
      h('p', { text: `Version ${n} becomes the live content` + (flat.live_version ? `, replacing version ${flat.live_version}.` : '. This is its first publish.') }),
      h('p', { class: 'muted', text: 'Publishing does not create a version: it puts an already saved version live. The health check runs first; if it fails, the current live version keeps serving. Data is not rolled back unless you ask for it.' }),
      box ? box.row : null,
    ],
    confirmLabel: rollback ? `Roll back to v${n}` : `Publish v${n}`,
  });
  if (!ok) return false;
  try {
    await approve(rollback ? await api.rollback(flat.slug, n, box.input.checked) : await api.deploy(flat.slug, n));
    await showLive(flat.slug, 'is live');
    return true;
  } catch (err) {
    await report(rollback ? 'Roll back failed' : 'Publish failed', err);
    return false;
  }
}

// saveVersion saves the flat's draft as a new version. It does not publish.
export async function saveVersion(flat) {
  const draft = flat.draft;
  if (!draft || !draft.dirty) {
    await infoDialog('Nothing to save', 'The draft has no changes since the latest saved version. An agent updates the draft with save_draft or save_draft_from_dir (MCP), or `flats save-draft <slug> <dir>`.');
    return false;
  }
  const n = nextVersion(flat);
  const ok = await confirmDialog({
    title: `Save the draft of ${label(flat)} as version ${n}?`,
    body: [
      h('p', { text: `The current draft is stored as version ${n}, a fixed copy you can publish or roll back to later.` }),
      h('p', { class: 'muted', text: 'Saving does not publish: ' + (flat.live_version ? `version ${flat.live_version} stays live` : 'nothing goes live') + '. Publish the new version when you are ready.' }),
    ],
    confirmLabel: `Save as v${n}`,
  });
  if (!ok) return false;
  try {
    const decided = await approve(await api.createVersion(flat.slug, draft));
    const v = decided?.result_data?.version;
    toast(v ? `Saved version ${v}. It is not live yet: publish it when ready.` : 'Saved as a new version. It is not live yet.', 'success');
    return true;
  } catch (err) {
    await report('Save failed', err);
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

// Deletion is a direct operator action; agent requests have their own approval.
export async function deleteVersion(flat, n) {
  if (n === flat.live_version) {
    await infoDialog('Cannot delete the live version', 'Publish another version before deleting this one.');
    return false;
  }
  const ok = await confirmDialog({
    title: `Delete version ${n} of ${label(flat)}?`,
    body: [
      h('p', { text: `This permanently deletes version ${n} and closes its open previews. Its number will never be reused.` }),
      h('p', { class: 'muted', text: 'The live version keeps serving. Server data and data snapshots are kept. Deleting this version frees one place in the version quota.' }),
    ],
    confirmLabel: `Delete v${n}`, danger: true,
  });
  if (!ok) return false;
  try {
    const result = await api.deleteVersion(flat.slug, n);
    if (result?.status === 'cleanup_pending' || result?.cleanup_pending) {
      await infoDialog('Version removed; cleanup pending', [
        h('p', { text: `Version ${n} was removed from the version list. Private storage cleanup is still pending.` }),
        h('p', { class: 'alert alert-warn', text: result.message || 'Restart Flats before saving, creating, deploying, or previewing this flat so storage cleanup can finish.' }),
      ]);
    } else {
      toast(`Version ${n} deleted.`, 'success');
    }
    return true;
  } catch (err) {
    await report('Version deletion failed', err);
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
// access ('view' or 'edit') is the public access level of a board. Stopping
// guest editing and allowing it have their own steps.
export async function setVisibility(flat, vis, access) {
  const fromKey = accessOf(flat);
  if (vis === 'public' && access === 'edit') return allowGuestEditing(flat);
  if (vis === 'public' && fromKey === 'edit') return stopGuestEditing(flat);
  const from = VISIBILITY[visibilityOf(flat.visibility)];
  const to = VISIBILITY[vis];
  const widening = to.rank > from.rank;
  if (widening && !(await canGoPublic(flat))) return null;
  const notice = becomingNoticeFor(vis);
  const ok = await confirmDialog({
    title: `Make ${label(flat)} ${to.label.toLowerCase()}?`,
    body: [
      h('p', { text: `Visibility changes from ${accessLabel(flat.visibility, flat.public_access)} to ${vis === 'private' ? to.label : ACCESS[access === 'view' ? 'view' : 'public'].label}.` }),
      widening && notice ? h('p', { class: 'alert alert-warn', text: notice }) : null,
      vis === 'private' ? h('p', { class: 'muted', text: 'The public address goes offline. The private address keeps working.' }) : null,
    ],
    confirmLabel: widening ? `Make ${to.label.toLowerCase()}` : 'Change visibility',
    danger: widening,
  });
  if (!ok) return null;
  try {
    const res = await approve(await api.setVisibility(flat.slug, vis, vis === 'public' ? access : undefined));
    toast(res.result ? res.result[0].toUpperCase() + res.result.slice(1) + '.' : 'Visibility changed.', 'success');
    return res;
  } catch (err) {
    await report('Visibility change failed', err);
    return null;
  }
}

// canGoPublic checks what a Private flat needs before it can be Public.
// Documents, designs and boards run without published versions
// (live_version is 0 for designs and boards), so only versioned types need
// a live version first.
async function canGoPublic(flat) {
  if (!isLive(flat) && !flat.live_version) {
    await infoDialog('Publish first', `${label(flat)} has no live version yet. Save a version and publish it, then make it public.`);
    return false;
  }
  if (!PUBLIC_PROVIDERS.some((p) => (flat.providers || []).includes(p))) {
    await infoDialog('Allow a public network first', `Allow Tailscale Funnel, Portal, zrok, a Cloudflare quick tunnel or NetBird Expose for ${label(flat)} under Networks, then make it public.`);
    return false;
  }
  return true;
}

// confirmed returns a visibility result that the host confirmed as applied:
// an approved request, or a change that was already in place. Anything else
// is reported as a failure rather than announced as done.
function confirmed(res) {
  if (res?.status === 'approved' || res?.status === 'done') return res;
  throw Object.assign(new Error(res?.result || res?.error || 'Flats did not confirm the change.'), { body: {} });
}

// stopGuestEditing sets a board to Public, view only at once, with no
// confirmation (rule 5). The console's request is its own approval.
export async function stopGuestEditing(flat) {
  try {
    const res = confirmed(await approve(await api.setVisibility(flat.slug, 'public', 'view')));
    toast('Guest editing stopped. Viewers can still watch.', 'success');
    return res;
  } catch (err) {
    await report('Stop guest editing failed', err);
    return null;
  }
}

// allowGuestEditing widens a board to Public, anyone can edit: it reads the
// preflight first and shows either what must change or the confirmation of
// what the change grants (rules 3, 6, 6a).
export async function allowGuestEditing(flat) {
  if (visibilityOf(flat.visibility) === 'private' && !(await canGoPublic(flat))) return null;
  let pre;
  try {
    pre = await api.publicEdit(flat.slug);
  } catch (err) {
    await report('Cannot check the limits for guest editing', err);
    return null;
  }
  const problems = pre.problems || [];
  if (problems.length) { await blockedDialog(flat, pre, problems); return null; }
  const ok = await confirmDialog({
    title: `Let anyone on the internet edit ${label(flat)}?`,
    body: [
      h('p', { text: 'Anyone who opens the link can add, change and delete everything on this board. They type a name first, but nobody checks who they are.' }),
      h('ul', { class: 'checks' },
        check(true, 'You can restore any earlier state from History. Flats pins a ', h('strong', { text: 'safe point' }), " of the board as it is now, and it can't be unpinned while guests can edit."),
        check(true, "Guests can't see history, import, upload images or send selections to an agent."),
        check(true, `Guest activity is limited, and the disk quota (${bytes(pre.disk_quota)}) and the revision quota (${pre.revision_quota}) stay in force.`)),
      h('p', { class: 'alert alert-warn' },
        pre.listed ? 'This flat is also listed publicly by a relay, so strangers can find it. ' : '',
        h('strong', { text: 'Stop guest editing' }), ' turns editing off at once.'),
    ],
    confirmLabel: 'Allow anyone to edit',
  });
  if (!ok) return null;
  try {
    const res = confirmed(await approve(await api.setVisibility(flat.slug, 'public', 'edit')));
    toast('Anyone with the link can now edit this board.', 'success');
    return res;
  } catch (err) {
    if (err?.body?.category === 'public_edit_blocked') { await blockedDialog(flat, pre, err.problems); return null; }
    await report('Allowing guest editing failed', err);
    return null;
  }
}

const check = (ok, ...text) => h('li', { class: 'check' + (ok ? ' ok' : ' no') }, h('span', { class: 'tick', 'aria-hidden': 'true', text: ok ? '✓' : '!' }), h('span', null, ...text));

// PROBLEMS gives the text and the one action for each preflight problem.
const PROBLEMS = {
  disk_quota_off: { text: () => [h('strong', { text: 'Disk quota is off.' }), ' Set a disk quota in Settings, Limits.'], action: 'limits' },
  revision_quota_unlimited: { text: (f) => [h('strong', { text: 'Revision quota is unlimited.' }), ` Set a revision quota for ${label(f)} in Settings, Limits, or in this flat's settings.`], action: 'revisions' },
  pin_quota_full: { text: (f, pre) => [h('strong', { text: `Pin quota is full (${pre.pins} of ${pre.pin_quota}).` }), ' Flats needs one free pin for the safe point. Unpin a revision in History.'], action: 'history' },
};

// blockedDialog lists what must change before guests can edit, with one
// action per problem; the confirm button stays disabled.
function blockedDialog(flat, pre, problems) {
  return new Promise((resolve) => {
    const codes = new Set(problems.map((p) => p.code));
    const titleId = 'dlg-title-' + Math.random().toString(36).slice(2);
    const rows = problems.map((p) => {
      const known = PROBLEMS[p.code];
      return check(false, ...(known ? known.text(flat, pre) : [p.message || p.code]));
    });
    if (!codes.has('disk_quota_off')) rows.push(check(true, `Disk quota is ${bytes(pre.disk_quota)}.`));
    if (!codes.has('revision_quota_unlimited')) rows.push(check(true, `Revision quota is ${pre.revision_quota}.`));
    if (!codes.has('pin_quota_full')) rows.push(check(true, pre.safe_point && pre.safe_point_pinned ? `The safe point r${pre.safe_point} is kept pinned.` : 'A pin is free for the safe point.'));
    const base = `/flats/${encodeURIComponent(flat.slug)}`;
    const targets = {
      limits: h('a', { class: 'btn', href: '/settings/limits', 'data-nav': true, text: 'Open Limits' }),
      revisions: h('a', { class: 'btn', href: base + '/settings#revisions', 'data-nav': true, text: 'Open revision settings' }),
      history: flat.private_url ? extLink(flat.private_url, 'Open History', 'btn') : null,
    };
    const actions = [...new Set(problems.map((p) => PROBLEMS[p.code]?.action).filter(Boolean))].map((a) => targets[a]).filter(Boolean);
    const cancel = h('button', { type: 'button', class: 'btn', text: 'Cancel' });
    const allow = h('button', { type: 'button', class: 'btn btn-primary', disabled: true, 'aria-disabled': 'true', text: 'Allow anyone to edit' });
    const dlg = h('dialog', { class: 'dialog', role: 'alertdialog', 'aria-labelledby': titleId },
      h('form', { method: 'dialog' },
        h('h2', { id: titleId, text: 'Set these limits before anyone can edit' }),
        h('div', { class: 'dialog-body' },
          h('p', { text: "Open editing needs limits so a flood of changes can't fill the disk or push out your history." }),
          h('ul', { class: 'checks' }, rows)),
        h('div', { class: 'dialog-actions' }, cancel, actions, allow)));
    cancel.addEventListener('click', () => dlg.close());
    for (const a of actions) a.addEventListener('click', () => dlg.close());
    dlg.addEventListener('close', () => { dlg.remove(); resolve(); });
    document.body.appendChild(dlg);
    dlg.showModal();
    cancel.focus();
  });
}

// Networks a flat can be served on besides this device (Local, always on).
// Each must be turned on for the host in Settings and then allowed per flat;
// neither step publishes a flat or makes it public.
export const PROVIDERS = [
  { id: 'tailscale', scope: 'private', label: 'Tailscale', hint: 'Devices your tailnet ACL allows' },
  { id: 'netbird', scope: 'private', label: 'NetBird', hint: 'Peers your NetBird access policies allow' },
  { id: 'tailscale-funnel', scope: 'public', label: 'Tailscale Funnel', hint: 'Anyone on the internet' },
  { id: 'portal', scope: 'public', label: 'Portal', hint: 'Anyone on the internet, through Portal relays' },
  { id: 'zrok', scope: 'public', label: 'zrok', hint: 'Anyone on the internet, through zrok public shares' },
  { id: 'cloudflare-quick', scope: 'public', label: 'Cloudflare Quick Tunnel', hint: 'Anyone on the internet, through a random trycloudflare.com address' },
  { id: 'netbird-expose', scope: 'public', label: 'NetBird Expose', hint: 'Anyone on the internet, through NetBird’s reverse proxy' },
];
const PUBLIC_PROVIDERS = ['tailscale-funnel', 'portal', 'zrok', 'cloudflare-quick', 'netbird-expose'];

// setProvider allows or stops a network for one flat; resolves true on success.
export async function setProvider(flat, provider, permitted) {
  const p = PROVIDERS.find((x) => x.id === provider);
  try {
    await api.setFlatProvider(flat.slug, provider, permitted);
    toast(`${p.label} ${permitted ? 'allowed' : 'no longer allowed'} for ${label(flat)}.`, 'success');
    return true;
  } catch (err) {
    // provider_not_ready on a grant: the permission is saved, its route is not open yet.
    const saved = permitted && err?.body?.category === 'provider_not_ready';
    await report(saved ? `${p.label} allowed, but its route did not open` : `Cannot change ${p.label}`, err);
    return false;
  }
}
