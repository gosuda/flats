import { h, icon, VISIBILITY, visibilityOf, PUBLIC_ACCESS_NOTICE } from './dom.js';
import { api } from './api.js';
import { setVisibility } from './actions.js';
import { busy, fill, toast, extLink, serverOnly, loopbackHost, SERVER_ONLY } from './ui.js';

export function shareDialog(initial, onChange = () => {}) {
  let flat = initial;
  let changed = false;
  let pending = false;
  let refreshError = false;
  let visitKey = '';
  const previousFocus = document.activeElement;
  const id = 'share-' + Math.random().toString(36).slice(2);
  const close = h('button', { type: 'button', class: 'icon-btn', 'aria-label': 'Close share dialog' }, icon('close'));
  const access = h('select', { id: id + '-access' }, Object.entries(VISIBILITY).map(([value, desc]) =>
    h('option', { value, selected: value === visibilityOf(flat.visibility), text: desc.label })));
  const notice = h('p', { class: 'muted small share-notice' });
  const explanation = h('p', { class: 'muted small' });
  const link = h('div', { class: 'share-link muted small' });
  const status = h('p', { class: 'muted small', role: 'status', 'aria-live': 'polite' });
  const visit = h('span');
  const copy = h('button', { type: 'button', class: 'btn btn-small' }, icon('copy'), 'Copy link');
  const refresh = h('button', { type: 'button', class: 'btn btn-small', text: 'Refresh' });
  const done = h('button', { type: 'button', class: 'btn btn-primary', text: 'Done' });
  const footer = h('div', { class: 'share-footer' }, status, link,
    h('div', { class: 'share-footer-actions' }, h('div', { class: 'cell-actions' }, visit, copy, refresh), done));
  const dlg = h('dialog', { class: 'dialog share-dialog', 'aria-labelledby': id },
    h('div', { class: 'share-content' },
      h('div', { class: 'share-heading' }, h('h2', { id, text: `Share ${flat.name || flat.slug}` }), close),
      h('p', { class: 'muted small', text: 'Access depends on the selected network and link visibility. Email invitations are not supported.' }),
      h('div', { class: 'field share-access' }, h('label', { for: id + '-access', text: 'Link access' }), access, notice),
      h('section', { class: 'share-people', 'aria-labelledby': id + '-people' },
        h('h3', { id: id + '-people', text: 'Who has access' }),
        h('div', { class: 'share-person' }, h('span', { class: 'share-avatar' }, icon('lock')),
          h('div', null, h('div', { text: 'Host operator' }), h('div', { class: 'muted small', text: 'Manages this flat' })),
          h('span', { class: 'muted small', text: 'Owner' })),
        explanation),
      footer));
  function draw() {
    const route = shareRoute(flat);
    const vis = visibilityOf(flat.visibility);
    access.value = vis;
    access.disabled = pending;
    refresh.setAttribute('aria-disabled', String(pending));
    refresh.setAttribute('aria-busy', String(pending));
    refresh.className = 'btn btn-small' + (pending ? ' is-disabled' : '');
    notice.textContent = vis === 'private' ? privateAccess(route) : (flat.public_notice || PUBLIC_ACCESS_NOTICE);
    explanation.textContent = vis === 'private' ? 'Other permitted networks may provide additional private links.' : 'Public links can be opened by anyone.';
    status.textContent = refreshError ? 'Could not refresh. Showing the last known state: ' + route.message + ' Try Refresh again.' : route.message;
    link.textContent = route.url || '';
    copy.disabled = !route.usable || serverOnly(route.url) || pending;
    if (copy.disabled) copy.setAttribute('disabled', '');
    else copy.removeAttribute('disabled');
    copy.title = serverOnly(route.url) ? SERVER_ONLY : '';
    const nextVisitKey = route.usable ? route.url : '';
    if (nextVisitKey !== visitKey) {
      const hadFocus = visit.firstChild && document.activeElement === visit.firstChild;
      fill(visit, route.usable ? extLink(route.url, [icon('external'), 'Visit'], 'btn btn-small') : null);
      visitKey = nextVisitKey;
      if (hadFocus) {
        if (visit.firstChild?.tagName === 'A') visit.firstChild.focus();
        else refresh.focus();
      }
    }
  }
  copy.addEventListener('click', () => busy(copy, async () => {
    const route = shareRoute(flat);
    if (!route.usable || serverOnly(route.url)) return;
    try { await navigator.clipboard.writeText(route.url); toast('Link copied.', 'success'); }
    catch { toast('Could not copy the link. Select and copy the address above.', 'error'); }
  }).finally(() => { if (dlg.open) draw(); }));
  done.addEventListener('click', () => dlg.close());
  refresh.addEventListener('click', async () => {
    if (pending) return;
    if (document.activeElement === access) refresh.focus();
    pending = true;
    refresh.textContent = 'Refreshing…';
    draw();
    try {
      const current = await api.flat(flat.slug);
      if (!dlg.open) return;
      changed ||= JSON.stringify(current) !== JSON.stringify(flat);
      flat = current;
      refreshError = false;
    } catch {
      if (!dlg.open) return;
      refreshError = true;
    } finally {
      pending = false;
      if (dlg.open) { refresh.textContent = 'Refresh'; draw(); }
    }
  });
  access.addEventListener('change', async () => {
    if (pending) return;
    const next = access.value;
    pending = true;
    access.disabled = true;
    refresh.setAttribute('aria-disabled', 'true');
    refresh.className = 'btn btn-small is-disabled';
    try {
      const result = await setVisibility(flat, next);
      if (result) {
        changed = true;
        flat = await api.flat(flat.slug);
        refreshError = false;
        if (!dlg.open) { onChange(); return; }
      }
      draw();
    } catch (err) { refreshError = true; access.value = visibilityOf(flat.visibility); toast(err.message, 'error'); }
    finally { pending = false; if (dlg.open) draw(); }
  });
  close.addEventListener('click', () => dlg.close());
  dlg.addEventListener('close', () => { dlg.remove(); previousFocus?.focus(); if (changed) onChange(); });
  draw();
  document.body.appendChild(dlg);
  dlg.showModal();
  access.focus();
}

const publicProviders = new Set(['tailscale-funnel', 'portal', 'zrok', 'cloudflare-quick', 'netbird-expose']);
const privateRank = { tailscale: 3, netbird: 2, local: 1 };
const failedStates = new Set(['error', 'needs-login', 'unavailable']);

// Share requires a usable current route, rather than an address alone.
export function shareRoute(flat) {
  const isPublic = visibilityOf(flat.visibility) === 'public';
  const current = (flat.endpoints || []).filter(e => e.audience === 'current' &&
    (!e.host || e.host === flat.slug) && publicProviders.has(e.provider) === isPublic);
  const candidates = current.filter(e => e.permitted && e.state !== 'stopped');
  const remoteCandidates = candidates.filter(e => !serverOnly(e.url));
  const rank = e => e.ready === true && e.url ? 3 : e.state === 'starting' ? 2 : 1;
  const route = [...(remoteCandidates.length ? remoteCandidates : candidates)].sort((a, b) =>
    rank(b) - rank(a) || (privateRank[b.provider] || 0) - (privateRank[a.provider] || 0))[0];
  const url = route ? route.url || '' : (current.length ? '' : (isPublic ? flat.public_url : flat.private_url) || '');
  const state = route ? route.state : (isPublic ? flat.connection_state : flat.private_state);
  const ready = route ? route.ready === true : !state || state === 'ready' || state === 'key-expiring';
  const out = { url, provider: route?.provider || '', usable: false, message: '' };
  if (!flat.live_version) {
    out.message = ['docs', 'design'].includes(flat.type) ? 'This flat is not running.' : 'This flat has not been published yet.';
  } else if (flat.running === false) out.message = 'This flat is not running.';
  else if (url && ready) {
    out.usable = true;
    out.message = serverOnly(url) ? SERVER_ONLY : state ? 'Link is ready.' : 'Link available.';
  } else if (state === 'starting') out.message = 'Link is being prepared. Refresh to check its status.';
  else if (failedStates.has(state)) out.message = 'Link is unavailable. Check this flat’s Networks settings, then refresh.';
  else out.message = 'No link is available. Check this flat’s Networks settings, then refresh.';
  return out;
}

function privateAccess(route) {
  if (route.provider === 'local' || isLoopback(route.url)) return 'This private link opens only on the Flats host machine.';
  if (route.provider === 'tailscale') return 'This private link follows your Tailscale access rules.';
  if (route.provider === 'netbird') return 'This private link follows your NetBird access policies.';
  return 'Private access follows the access rules of the network serving this link.';
}

function isLoopback(url) {
  try {
    const host = new URL(url).hostname;
    return loopbackHost(host);
  } catch { return false; }
}
