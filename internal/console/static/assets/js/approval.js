// Approval page: what an agent asked for and the operator's decision.

import { h, clear, dateTime, timeEl, VISIBILITY, visibilityBadge, noticeFor } from './dom.js';
import { api } from './api.js';
import { impactText, failureMessage, failureCode } from './lifecycle.js';
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
  if (a.action === 'set_visibility') return `Make ${a.flat} ${visLabel(p.visibility || p.to).toLowerCase()}`;
  if (a.action === 'publish') return `Publish draft revision ${p.revision || ''} of ${a.flat}`;
  if (a.action === 'delete') return `Delete ${a.flat}`;
  if (a.action === 'restore_data') return `Restore live data on ${a.flat} from ${p.snapshot || 'the frozen snapshot'}`;
  if (a.action === 'rollback' || a.action === 'deploy' || a.action === 'activate') return `Make v${p.version || ''} current on ${a.flat}${p.restore_data ? ' and restore live data' : ''}`;
  return `${a.action} on ${a.flat}`;
}

export function statusBadge(status) {
  return h('span', { class: 'badge badge-' + status, text: status });
}

export const RESTORE_COPY = 'Replaces the live database and captured FILES with this snapshot after backing up current live data. Historical DB-only snapshots preserve current FILES. Writes since the snapshot stop being live.';
export const RUNTIME_COPY = 'Starting the server runtime may write to live DB and FILES, even if activation fails. The health check uses an isolated copy.';

export function approvalRisk(a, flat) {
  const p = params(a);
  if (p.restore_data || a.action === 'restore_data') return RESTORE_COPY;
  if (['publish', 'activate', 'deploy', 'rollback'].includes(a.action)) {
    const sameCandidate = a.action === 'publish' ? p.hash === flat?.draft?.hash : p.version === flat?.live_version;
    const kind = sameCandidate ? (a.action === 'publish' ? flat?.draft?.kind : flat?.live?.kind) : '';
    return kind === 'static' ? '' : (kind === 'server' ? RUNTIME_COPY : 'If this candidate is a server flat: ' + RUNTIME_COPY);
  }
  return '';
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
    const risk = approvalRisk(a, flat);
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
        rows.push(['Current access', flat ? visibilityBadge(flat.visibility) : h('span', { text: p.from ? visLabel(p.from) : 'unknown' })]);
      } else {
        if (p.from) rows.push(['Access when requested', visibilityBadge(p.from)]);
        if (flat) rows.push(['Access now', visibilityBadge(flat.visibility)]);
      }
      rows.push(['Requested access', visibilityBadge(p.visibility || p.to)]);
    }
    if (p.revision) rows.push(['Draft revision', String(p.revision)]);
    if (p.hash) rows.push(['Candidate hash', h('code', { text: String(p.hash) })]);
    if (p.base_version || p.expected_live) rows.push(['Current version at request', `v${p.base_version || p.expected_live}`]);
    if (p.version) rows.push(['Published version', `v${p.version} (no new number)`]);
    if (p.providers !== undefined) rows.push(['Provider policy at request', Array.isArray(p.providers) ? (p.providers.join(', ') || 'local') : String(p.providers)]);
    if (p.restore_data || a.action === 'restore_data') {
      rows.push(['Restore live data', 'Yes — replaces live data after a backup']);
      rows.push(['Snapshot', h('code', { text: p.snapshot || 'Snapshot identity not reported' })]);
      rows.push(['Snapshot hash', h('code', { text: p.snapshot_hash || 'Snapshot hash not reported' })]);
    }
    if (risk) rows.push(['Data impact before approval', risk]);
    rows.push(['Status', statusBadge(a.status)]);
    if (a.decided_at) rows.push(['Decided', dateTime(a.decided_at)]);
    if (a.decided_by) rows.push(['Decision actor', a.decided_by]);
    if (a.result_data) rows.push(['Data impact', impactText(a)]);
    if (a.result) rows.push(['Result', a.result]);
    body.appendChild(h('dl', { class: 'facts' }, rows.map(([k, v]) => [h('dt', { text: k }), h('dd', null, v)])));

    const notice = a.action === 'set_visibility' ? noticeFor(p.visibility || p.to) : '';
    if (notice && a.status === 'pending') body.appendChild(h('p', { class: 'alert alert-warn', text: notice.replace('This flat is public:', 'If approved, this flat becomes Public:') }));
    if (a.action === 'delete' && a.status === 'pending') {
      body.appendChild(h('p', { class: 'alert alert-warn', text: 'Approving permanently deletes the flat, its versions, data, secrets and logs.' }));
    }

    if (failureCode(a) === 'stale_approval' || (!failureCode(a) && /stale/i.test(a.result || '')) || a.status === 'stale') {
      body.appendChild(h('p', { class: 'alert alert-warn', role: 'status', text: 'The content or access settings changed. Review again.' }));
    }
    if (a.status === 'rejected') {
      body.appendChild(h('p', { role: 'status', text: 'The request was rejected. The draft is unchanged.' }));
    }
    if (a.status === 'failed' && !/stale/i.test(a.result || '')) {
      body.appendChild(h('p', { role: 'status', text: failureMessage(a) || a.result || 'The approval failed. It did not publish a version.' }));
    }
    if (a.status !== 'pending') {
      body.appendChild(h('p', { class: 'muted', text: `This request was ${a.status}.` }));
      return;
    }
    if (!flat) {
      body.appendChild(h('p', { class: 'muted', text: 'The flat no longer exists under this name; approving will fail.' }));
    }
    const approve = h('button', { type: 'button', class: 'btn btn-primary', text: 'Approve…' });
    const reject = h('button', { type: 'button', class: 'btn', text: 'Reject…' });
    approve.addEventListener('click', () => busy(approve, () => decide(a, true, flat)));
    reject.addEventListener('click', () => busy(reject, () => decide(a, false, flat)));
    body.appendChild(h('div', { class: 'actions' }, reject, approve));
  }

  async function decide(a, yes, flat) {
    const risk = yes ? approvalRisk(a, flat) : '';
    const p = params(a);
    const notice = yes && a.action === 'set_visibility' ? noticeFor(p.visibility || p.to) : '';
    const ok = await confirmDialog({
      title: yes ? `Approve: ${describeApproval(a)}?` : `Reject: ${describeApproval(a)}?`,
      body: [
        h('p', { text: yes ? 'The change applies immediately.' : 'Nothing changes; the agent sees the request as rejected.' }),
        notice ? h('p', { class: 'alert alert-warn', text: notice }) : null,
        risk ? h('p', { class: 'alert alert-warn', text: risk }) : null,
        yes && p.snapshot ? h('p', { text: `Snapshot: ${p.snapshot} · hash ${p.snapshot_hash || 'not reported'}` }) : null,
      ],
      confirmLabel: yes ? 'Approve' : 'Reject',
      danger: yes && (a.action === 'delete' || !!notice || p.restore_data || a.action === 'restore_data'),
      requireText: yes && a.action === 'delete' ? a.flat : undefined,
    });
    if (!ok) return;
    clear(errSlot);
    try {
      const res = await api.decide(a.id, yes);
      toast(`Request ${res.status}.`, res.status === 'failed' ? 'error' : 'success');
    } catch (err) {
      if (ctx.alive()) errSlot.appendChild(errorPanel(err, yes ? (failureMessage(err.body) || 'Approval failed') : 'Rejection failed'));
    }
    if (ctx.alive()) { await load();
      body.setAttribute('tabindex', '-1');
      body.focus({ preventScroll: true });
    }
  }

  load();
}
