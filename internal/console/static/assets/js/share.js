import { h, icon, VISIBILITY, publicNoticeOf } from './dom.js';
import { api } from './api.js';
import { setVisibility } from './actions.js';
import { busy, fill, toast, extLink } from './ui.js';

export function shareDialog(initial, onChange = () => {}) {
  let flat = initial;
  let changed = false;
  const previousFocus = document.activeElement;
  const id = 'share-' + Math.random().toString(36).slice(2);
  const close = h('button', { type: 'button', class: 'icon-btn', 'aria-label': 'Close share dialog' }, icon('close'));
  const access = h('select', { id: id + '-access' }, Object.entries(VISIBILITY).map(([value, desc]) =>
    h('option', { value, selected: value === flat.visibility, text: desc.label })));
  const notice = h('p', { class: 'muted small share-notice' });
  const footer = h('div', { class: 'share-footer' });
  const dlg = h('dialog', { class: 'dialog share-dialog', 'aria-labelledby': id },
    h('div', { class: 'share-content' },
      h('div', { class: 'share-heading' }, h('h2', { id, text: `Share ${flat.name || flat.slug}` }), close),
      h('p', { class: 'muted small', text: 'Access is managed through your tailnet and public links. Email invitations are not supported.' }),
      h('div', { class: 'field share-access' }, h('label', { for: id + '-access', text: 'Link access' }), access, notice),
      h('section', { class: 'share-people', 'aria-labelledby': id + '-people' },
        h('h3', { id: id + '-people', text: 'Who has access' }),
        h('div', { class: 'share-person' }, h('span', { class: 'share-avatar' }, icon('lock')),
          h('div', null, h('div', { text: 'Host operator' }), h('div', { class: 'muted small', text: 'Manages this flat' })),
          h('span', { class: 'muted small', text: 'Owner' })),
        h('p', { class: 'muted small', text: 'Private access follows your Tailscale ACLs. Public links can be opened by anyone.' })),
      footer));
  function draw() {
    access.value = flat.visibility;
    notice.textContent = flat.visibility === 'private'
      ? 'Only devices allowed by your tailnet can open the private URL.' : publicNoticeOf(flat);
    const url = flat.visibility === 'private' ? flat.private_url : flat.public_url;
    const copy = h('button', { type: 'button', class: 'btn btn-small', disabled: !url || !flat.live_version }, icon('copy'), 'Copy link');
    copy.addEventListener('click', () => busy(copy, async () => {
      try { await navigator.clipboard.writeText(url); toast('Link copied.', 'success'); }
      catch { toast('Could not copy the link. Select and copy the address below.', 'error'); }
    }));
    const done = h('button', { type: 'button', class: 'btn btn-primary', text: 'Done' });
    done.addEventListener('click', () => dlg.close());
    fill(footer, h('div', { class: 'share-link muted small', text: url || 'Public link is being prepared. Reopen Share to check.' }),
      h('div', { class: 'share-footer-actions' }, h('div', { class: 'cell-actions' },
        flat.live_version && url ? extLink(url, [icon('external'), 'Visit'], 'btn btn-small') : null, copy), done));
  }
  access.addEventListener('change', async () => {
    const next = access.value;
    access.disabled = true;
    try {
      const result = await setVisibility(flat, next);
      if (result) {
        changed = true;
        flat = await api.flat(flat.slug);
        if (!dlg.open) { onChange(); return; }
      }
      draw();
    } catch (err) { access.value = flat.visibility; toast(err.message, 'error'); }
    finally { access.disabled = false; }
  });
  close.addEventListener('click', () => dlg.close());
  dlg.addEventListener('close', () => { dlg.remove(); previousFocus?.focus(); if (changed) onChange(); });
  draw();
  document.body.appendChild(dlg);
  dlg.showModal();
  access.focus();
}
