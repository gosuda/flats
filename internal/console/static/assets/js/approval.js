// Approval page: what an agent asked for and who decided it.

import { h, clear, dateTime, timeEl, visibilityOf, accessKey, accessOf, accessLabel, editWords, visibilityBadge, becomingNoticeFor } from './dom.js';
import { api } from './api.js';
import { confirmDialog, errorPanel, loading, toast, busy } from './ui.js';

function params(a) {
  if (!a.params) return {};
  if (typeof a.params === 'object') return a.params;
  try { return JSON.parse(a.params); } catch { return {}; }
}

// An approved deletion can still need durable storage cleanup. Its decision
// stays approved because metadata was deleted; keep the pending work visible.
export function cleanupPending(a) {
  let result = a.result_data;
  if (typeof result === 'string') {
    try { result = JSON.parse(result); } catch { return false; }
  }
  return !!result?.cleanup_pending;
}

// requested and fromState are the access a visibility approval asks for and
// the access the flat had when it was requested. An empty requested level
// means view only when Public (wire contract).
const requested = (p) => ({ vis: p.visibility, access: visibilityOf(p.visibility) === 'public' ? p.access || 'view' : '' });
const fromState = (p) => ({ vis: p.from, access: visibilityOf(p.from) === 'public' ? p.from_access || '' : '' });
const badgeOf = (s) => visibilityBadge(s.vis, s.access);

// describeApproval is a one-line summary for lists.
export function describeApproval(a) {
  const p = params(a);
  if (a.action === 'set_visibility') {
    const r = requested(p);
    return `Make ${a.flat} ${(p.access ? accessLabel(r.vis, r.access) : accessLabel(r.vis)).toLowerCase()}`;
  }
  if (a.action === 'delete') return `Delete ${a.flat}`;
  // Version creation and deployment remain separate approvals.
  if (a.action === 'create_version' || a.action === 'publish') return `Save the draft of ${a.flat} as a new version (does not go live)`;
  if (a.action === 'delete_version') return `Delete version ${p.version} of ${a.flat}`;
  if (a.action === 'activate') return `Publish version ${p.version} of ${a.flat} (put it live)`;
  if (a.action === 'rollback') return `Roll back ${a.flat} to version ${p.version}` + (p.restore_data ? ' and restore its data' : '');
  if (a.action === 'restore_data') return `Restore the data of ${a.flat}`;
  return `${a.action} on ${a.flat}`;
}

export function statusBadge(status) {
  return h('span', { class: 'badge badge-' + status, text: status });
}

const SURFACES = { mcp: 'an MCP client', cli: 'the flats CLI', api: 'the HTTP API', console: 'the console' };

// ARRIVALS describe the network an Autopilot decision arrived on
// (docs/design.md, Autopilot records).
const ARRIVALS = {
  local: () => 'Local',
  listener: (d) => (d.ip ? `the console listener from ${d.ip}` : 'the console listener'),
  tailscale: (d) => (d.login ? `Tailscale (${d.login})` : d.node ? `Tailscale node ${d.node}` : 'Tailscale'),
  netbird: (d) => (d.peer ? `NetBird peer ${d.peer}` : 'NetBird'),
  external: (d) => (d.origin ? `external origin ${d.origin}` : 'external origin'),
};
const AGENT_PREFIX = 'autopilot:';

// agentDecider parses an Autopilot decided_by value, or returns null for an
// operator value or a malformed one.
function agentDecider(decidedBy) {
  if (!decidedBy.startsWith(AGENT_PREFIX)) return null;
  let d;
  try { d = JSON.parse(decidedBy.slice(AGENT_PREFIX.length)); } catch { return null; }
  if (!d || typeof d !== 'object' || Array.isArray(d)) return null;
  if (typeof d.agent !== 'string' || !d.agent.trim() || !Object.hasOwn(ARRIVALS, d.network)) return null;
  if (['ip', 'login', 'node', 'peer', 'origin'].some((k) => k in d && typeof d[k] !== 'string')) return null;
  return d;
}

// decidedBy describes who decided an approval: the console, a tailnet login
// or NetBird peer as recorded, or Autopilot with the agent and its network.
// Anything else is shown as stored.
export function decidedBy(value) {
  if (value === 'console') return 'the console';
  if (value.startsWith('netbird:')) return 'NetBird peer ' + value.slice('netbird:'.length);
  const d = agentDecider(value);
  if (!d) return value;
  return `Autopilot: agent ${JSON.stringify(d.agent)} via ${ARRIVALS[d.network](d)}`;
}

function decidedByCell(value) {
  if (!agentDecider(value)) return decidedBy(value);
  return h('span', null, decidedBy(value),
    h('div', { class: 'muted small', text: 'The agent named itself; Flats did not verify the name.' }));
}

export function mount(main, [id], ctx) {
  ctx.setTitle('Approval');
  const body = h('div', null, loading());
  const errSlot = h('div');
  main.appendChild(h('section', { class: 'page page-narrow' },
    h('a', { class: 'back', href: '/', 'data-nav': true }, '← All flats'),
    h('h1', { text: 'Approval request' }),
    errSlot,
    body));

  async function load() {
    let a;
    try {
      a = await api.approval(id);
    } catch (err) {
      if (ctx.alive()) clear(body).appendChild(errorPanel(err, err.status === 404 ? 'No such approval' : 'Cannot load the approval'));
      return;
    }
    let flat = null;
    try { flat = await api.flat(a.flat); } catch { /* deleted or renamed */ }
    if (ctx.alive()) draw(a, flat);
  }

  function draw(a, flat) {
    const p = params(a);
    clear(body);
    const rows = [
      ['Request', h('strong', { text: describeApproval(a) })],
      ['Flat', flat ? h('a', { href: `/flats/${encodeURIComponent(a.flat)}`, 'data-nav': true, text: flat.name || a.flat }) : h('span', { text: a.flat })],
      ['Requested by', `${SURFACES[a.via] || a.via} (${a.via})`],
      ['Reason', a.reason || h('span', { class: 'muted', text: 'none given' })],
      ['Requested', h('span', null, timeEl(a.requested_at), ` · ${dateTime(a.requested_at)}`)],
    ];
    const stale = expired(a, p, flat);
    if (a.action === 'set_visibility') {
      if (p.from) rows.push(['Visibility when requested', badgeOf(fromState(p))]);
      if (flat) rows.push([a.status === 'pending' && !stale ? 'Current visibility' : 'Visibility now', visibilityBadge(flat.visibility, flat.public_access)]);
      else if (!p.from) rows.push(['Current visibility', h('span', { text: 'unknown' })]);
      rows.push(['Requested visibility', p.access ? badgeOf(requested(p)) : visibilityBadge(p.visibility)]);
    }
    rows.push(['Status', statusBadge(a.status)]);
    if (a.decided_at) rows.push(['Decided', dateTime(a.decided_at)]);
    if (a.decided_by) rows.push(['Decided by', decidedByCell(a.decided_by)]);
    if (a.result) rows.push(['Result', a.result]);
    body.appendChild(h('dl', { class: 'facts' }, rows.map(([k, v]) => [h('dt', { text: k }), h('dd', null, v)])));

    if (cleanupPending(a)) {
      body.appendChild(h('p', { class: 'alert alert-warn', role: 'alert', text: a.result || 'Version metadata was deleted, but storage cleanup is pending. Restart Flats before saving, creating, deploying, or previewing this flat.' }));
    }
    if (stale) {
      body.appendChild(h('p', { class: 'alert alert-error', role: 'alert' }, h('strong', { text: "This request can't be approved." }),
        ' The visibility changed after it was requested, so the approval no longer matches the flat. Nothing was changed.'));
      body.appendChild(h('div', { class: 'actions' }, dismissButton(a)));
      return;
    }
    const grantsEdit = a.action === 'set_visibility' && p.access === 'edit';
    const notice = a.action === 'set_visibility' && !grantsEdit ? becomingNoticeFor(p.visibility) : '';
    if (notice && a.status === 'pending') body.appendChild(h('p', { class: 'alert alert-warn', text: notice }));
    if (grantsEdit && a.status === 'pending') body.appendChild(guestEditNotes(flat));
    if (a.action === 'delete_version' && a.status === 'pending') {
      body.appendChild(h('p', { class: 'alert alert-warn', text: `Approving permanently deletes version ${p.version} and closes its open previews. Server data and data snapshots are kept. The live version cannot be deleted.` }));
    }
    if (a.action === 'delete' && a.status === 'pending') {
      body.appendChild(h('p', { class: 'alert alert-warn', text: 'Approving permanently deletes the flat, its versions, data, environment variables, secrets and logs.' }));
    }

    if (a.status !== 'pending') {
      body.appendChild(h('p', { class: 'muted', text: cleanupPending(a) ? `This request was ${a.status}. Restart Flats to finish storage cleanup.` : `This request was ${a.status}. Nothing else to do.` }));
      return;
    }
    if (!flat) {
      body.appendChild(h('p', { class: 'muted', text: 'The flat no longer exists under this name; approving will fail.' }));
    }
    const approve = h('button', { type: 'button', class: 'btn btn-primary', text: 'Approve…' });
    const reject = h('button', { type: 'button', class: 'btn', text: 'Reject…' });
    approve.addEventListener('click', () => busy(approve, () => decide(a, true, flat)));
    reject.addEventListener('click', () => busy(reject, () => decide(a, false)));
    body.appendChild(h('div', { class: 'actions' }, reject, approve));
  }

  function dismissButton(a) {
    const b = h('button', { type: 'button', class: 'btn', text: 'Dismiss…' });
    b.addEventListener('click', () => busy(b, () => decide(a, false)));
    return b;
  }

  async function decide(a, yes, flat) {
    const p = params(a);
    const notice = !yes || a.action !== 'set_visibility' ? ''
      : p.access === 'edit' ? `Anyone on the internet who has the link will be able to add, change and delete ${editWords(flat).everything}.` : becomingNoticeFor(p.visibility);
    const ok = await confirmDialog({
      title: yes ? `Approve: ${describeApproval(a)}?` : `Reject: ${describeApproval(a)}?`,
      body: [
        h('p', { text: yes ? 'The change applies immediately.' : 'Nothing changes; the agent sees the request as rejected.' }),
        notice ? h('p', { class: 'alert alert-warn', text: notice }) : null,
      ],
      confirmLabel: yes ? 'Approve' : 'Reject',
      danger: yes && (a.action === 'delete' || a.action === 'delete_version' || !!notice),
      requireText: yes && a.action === 'delete' ? a.flat : undefined,
    });
    if (!ok) return;
    clear(errSlot);
    try {
      const res = await api.decide(a.id, yes);
      if (cleanupPending(res)) toast('Request approved; storage cleanup is pending. Restart Flats before changing this flat.');
      else toast(`Request ${res.status}.`, res.status === 'failed' ? 'error' : 'success');
    } catch (err) {
      if (ctx.alive()) errSlot.appendChild(errorPanel(err, yes ? 'Approval failed' : 'Rejection failed'));
    }
    // The top bar's pending count follows the decision.
    if (typeof globalThis.dispatchEvent === 'function') globalThis.dispatchEvent(new Event('flats:approvals'));
    if (ctx.alive()) load();
  }

  load();
}

// expired reports a pending visibility request that no longer matches the
// flat: its visibility or access level changed since it was requested
// (rule 2). The server also refuses a round trip back to the same state.
export function expired(a, p, flat) {
  if (a.action !== 'set_visibility' || a.status !== 'pending' || !flat || !p.from) return false;
  const from = fromState(p);
  const now = accessOf(flat);
  const then = accessKey(from.vis, from.access);
  return now !== then && !(then === 'public' && now === 'view');
}

// guestEditNotes says what approving "anyone can edit" grants and keeps.
function guestEditNotes(flat) {
  const words = editWords(flat);
  const li = (...text) => h('li', { class: 'check ok' }, h('span', { class: 'tick', 'aria-hidden': 'true', text: '✓' }), h('span', null, ...text));
  return h('div', null,
    h('p', { class: 'alert alert-warn' }, h('strong', { text: 'If you approve,' }), ` anyone on the internet who has the link can add, change and delete ${words.everything}. Nobody checks who they are.`),
    h('ul', { class: 'checks' },
      li(`A safe point of ${words.the} will be pinned first. It can't be unpinned while guests can edit.`),
      li('You can end guest editing at any time with ', h('strong', { text: 'Stop guest editing' }), '.'),
      li('This approval is only for the state above. If the visibility changes before you approve, it expires.')));
}

// mountList is the approvals queue: every request, newest first, each
// linking to its page.
export function mountList(main, _params, ctx) {
  ctx.setTitle('Approvals');
  const body = h('div', null, loading());
  main.appendChild(h('section', { class: 'page' },
    h('div', { class: 'page-head' }, h('h1', { text: 'Approvals' })),
    h('p', { class: 'muted', text: 'Agents ask for approval before saving versions, deploying, changing visibility and deleting. Nothing changes until you decide.' }),
    body));
  api.approvals().then((r) => {
    if (!ctx.alive()) return;
    clear(body).appendChild(approvalsTable(r.approvals || []));
  }).catch((err) => ctx.alive() && clear(body).appendChild(errorPanel(err, 'Cannot load approvals')));
}

export function approvalsTable(list) {
  if (!list.length) return h('p', { class: 'muted', text: 'No approval requests yet.' });
  return h('div', { class: 'table-wrap' }, h('table', { class: 'table', id: 'approvals' },
    h('thead', null, h('tr', null, ['Request', 'Via', 'Requested', 'Status'].map((t) => h('th', { scope: 'col', text: t })))),
    h('tbody', null, list.map((a) => h('tr', null,
      h('th', { scope: 'row' }, h('a', { href: `/approvals/${encodeURIComponent(a.id)}`, 'data-nav': true, text: describeApproval(a) }),
        a.reason ? h('div', { class: 'muted small', text: a.reason }) : null),
      h('td', { text: a.via }),
      h('td', null, timeEl(a.requested_at)),
      h('td', null, statusBadge(a.status), cleanupPending(a) ? h('span', { class: 'badge badge-warn', text: 'Storage cleanup pending' }) : null))))));
}
