// Approval page: what an agent asked for and the operator's decision.

import { h, clear, dateTime, timeEl, VISIBILITY, visibilityBadge, noticeFor } from './dom.js';
import { api } from './api.js';
import { confirmDialog, errorPanel, loading, toast, busy } from './ui.js';

function params(a) {
  if (!a.params) return {};
  if (typeof a.params === 'object') return a.params;
  try { return JSON.parse(a.params); } catch { return {}; }
}

const visLabel = (v) => (VISIBILITY[v] ? VISIBILITY[v].label : v);

// describeApproval is a one-line summary for lists.
export function describeApproval(a) {
  const p = params(a);
  if (a.action === 'set_visibility') return `Make ${a.flat} ${visLabel(p.visibility).toLowerCase()}`;
  if (a.action === 'delete') return `Delete ${a.flat}`;
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

    const notice = a.action === 'set_visibility' ? noticeFor(p.visibility) : '';
    if (notice && a.status === 'pending') body.appendChild(h('p', { class: 'alert alert-warn', text: notice }));
    if (a.action === 'delete' && a.status === 'pending') {
      body.appendChild(h('p', { class: 'alert alert-warn', text: 'Approving permanently deletes the flat, its versions, data, secrets and logs.' }));
    }

    if (a.status !== 'pending') {
      body.appendChild(h('p', { class: 'muted', text: `This request was ${a.status}. Nothing else to do.` }));
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
    const notice = yes && a.action === 'set_visibility' ? noticeFor(p.visibility) : '';
    const ok = await confirmDialog({
      title: yes ? `Approve: ${describeApproval(a)}?` : `Reject: ${describeApproval(a)}?`,
      body: [
        h('p', { text: yes ? 'The change applies immediately.' : 'Nothing changes; the agent sees the request as rejected.' }),
        notice ? h('p', { class: 'alert alert-warn', text: notice }) : null,
      ],
      confirmLabel: yes ? 'Approve' : 'Reject',
      danger: yes && (a.action === 'delete' || !!notice),
      requireText: yes && a.action === 'delete' ? a.flat : undefined,
    });
    if (!ok) return;
    clear(errSlot);
    try {
      const res = await api.decide(a.id, yes);
      toast(`Request ${res.status}.`, res.status === 'failed' ? 'error' : 'success');
    } catch (err) {
      if (ctx.alive()) errSlot.appendChild(errorPanel(err, yes ? 'Approval failed' : 'Rejection failed'));
    }
    if (ctx.alive()) load();
  }

  load();
}
