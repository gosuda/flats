import { h } from './dom.js';
import { api } from './api.js';
import { announce, busy, confirmDialog, infoDialog } from './ui.js';

// Unlock grants a console session. It never approves a pending candidate.
export function mountOperatorControls() {
  const slot = document.getElementById('operator-controls');
  if (!slot) return;
  const status = h('span', { class: 'muted small', role: 'status', text: 'Decisions require operator unlock' });
  const unlock = h('button', { type: 'button', class: 'btn btn-small', text: 'Unlock decisions' });
  const logout = h('button', { type: 'button', class: 'btn btn-small', text: 'Sign out' });
  unlock.addEventListener('click', () => busy(unlock, async () => {
    const input = h('input', { id: 'operator-credential', type: 'password', autocomplete: 'current-password',
      minlength: '32', maxlength: '4096', required: true });
    const ok = await confirmDialog({
      title: 'Unlock operator decisions',
      body: [
        h('p', { text: 'Enter the separate credential supplied when this server started. Unlocking does not publish a version or approve a request.' }),
        h('div', { class: 'field' }, h('label', { for: 'operator-credential', text: 'Operator credential' }), input),
        h('p', { class: 'muted small', text: 'Use HTTPS or localhost. Sessions expire after eight hours and on server restart. A server started without operator authority keeps decisions disabled.' }),
      ], confirmLabel: 'Unlock',
    });
    if (!ok) { input.value = ''; return; }
    try {
      await api.operatorSession(input.value);
      status.textContent = 'Operator session unlocked';
      announce('Operator session unlocked. Review and confirm each action separately.');
    } catch (err) {
      await infoDialog('Decisions remain locked', err.message);
      announce('Operator unlock did not complete.');
    } finally { input.value = ''; }
  }));
  logout.addEventListener('click', () => busy(logout, async () => {
    try {
      await api.operatorLogout();
      status.textContent = 'Decisions require operator unlock';
      announce('Operator session signed out.');
    } catch (err) { await infoDialog('Sign out did not complete', err.message); }
  }));
  slot.append(status, unlock, logout);
}
