// Approval page: what an agent asked for and the operator's decision.

import { h, clear, dateTime, timeEl, VISIBILITY, visibilityOf, visibilityBadge, becomingNoticeFor } from './dom.js';
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

const visLabel = (v) => VISIBILITY[visibilityOf(v)].label;

// describeApproval is a one-line summary for lists.
export function describeApproval(a) {
  const p = params(a);
  if (a.action === 'set_visibility') return `Make ${a.flat} ${visLabel(p.visibility).toLowerCase()}`;
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
    if (a.action === 'set_visibility') {
      if (a.status === 'pending') {
        rows.push(['Current visibility', flat ? visibilityBadge(flat.visibility) : h('span', { text: p.from ? visLabel(p.from) : 'unknown' })]);
      } else {
        if (p.from) rows.push(['Visibility when requested', visibilityBadge(p.from)]);
        if (flat) rows.push(['Visibility now', visibilityBadge(flat.visibility)]);
      }
      rows.push(['Requested visibility', visibilityBadge(p.visibility)]);
    }
    rows.push(['Status', statusBadge(a.status)]);
    if (a.decided_at) rows.push(['Decided', dateTime(a.decided_at)]);
    if (a.result) rows.push(['Result', a.result]);
    body.appendChild(h('dl', { class: 'facts' }, rows.map(([k, v]) => [h('dt', { text: k }), h('dd', null, v)])));

    if (cleanupPending(a)) {
      body.appendChild(h('p', { class: 'alert alert-warn', role: 'alert', text: a.result || 'Version metadata was deleted, but storage cleanup is pending. Restart Flats before saving, creating, deploying, or previewing this flat.' }));
    }
    const notice = a.action === 'set_visibility' ? becomingNoticeFor(p.visibility) : '';
    if (notice && a.status === 'pending') body.appendChild(h('p', { class: 'alert alert-warn', text: notice }));
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
    approve.addEventListener('click', () => busy(approve, () => decide(a, true)));
    reject.addEventListener('click', () => busy(reject, () => decide(a, false)));
    body.appendChild(h('div', { class: 'actions' }, reject, approve));
  }

  async function decide(a, yes) {
    const p = params(a);
    const notice = yes && a.action === 'set_visibility' ? becomingNoticeFor(p.visibility) : '';
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
