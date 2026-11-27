// Notices (rules 24, 27, 30, 32 and 40). A notice says what happened
// without moving focus, and offers typed text back so it can be copied:
// after a refused or ignored edit, an overwrite, a rollback under an open
// editor. The host element is a polite live region, so each notice is
// spoken as it is added.
'use strict';

import { announce } from './a11y.js';
import { button, h, icon } from './dom.js';

const MAX = 8;       // notices kept on screen (TECH 7.2)
const SHORT = 6000;  // a notice with nothing to copy leaves by itself

export class Notices {
  constructor(host, { timers = globalThis } = {}) {
    this.host = host;
    this.timers = timers;
    this.seq = 0;
  }

  // show adds a notice. kind is info, ok, warn or bad. kept is text offered
  // back; a notice with kept text or actions stays until it is dismissed.
  // A notice with an id replaces the earlier one of that id.
  show({ id, kind = 'info', text, kept = null, actions = [], sticky = false }) {
    if (id) this.dismiss(id);
    const el = h('div', { class: `toast ${kind}`, 'data-notice': id || `n${++this.seq}` },
      h('p', { text }));
    const close = () => el.remove();
    if (kept != null) {
      const keptEl = h('div', { class: 'kept', tabindex: '0', 'aria-label': 'Your text', text: kept });
      el.appendChild(keptEl);
    }
    const row = h('div', { class: 'rowb' });
    if (kept != null) row.appendChild(button('Copy', (e) => copyText(kept, e.currentTarget), { class: 'btn' }));
    for (const [label, fn] of actions) row.appendChild(button(label, () => { close(); fn(); }, { class: 'btn' }));
    row.appendChild(button('Dismiss', close, { class: 'btn icon', 'aria-label': 'Dismiss this notice' }, icon('close')));
    el.appendChild(row);
    this.host.appendChild(el);
    while (this.host.children.length > MAX) this.host.firstChild.remove();
    if (kept == null && !actions.length && !sticky) this.timers.setTimeout(close, SHORT);
    return el;
  }

  dismiss(id) {
    for (const el of [...this.host.children]) if (el.getAttribute('data-notice') === id) el.remove();
  }
}

// copyText copies text to the system clipboard and says so on the button.
// Where the clipboard is not available the kept text stays selectable.
export function copyText(text, btn) {
  const done = () => { if (btn) btn.textContent = 'Copied'; announce('Copied.'); };
  const failed = () => announce('The clipboard is not available here. Select the text and copy it.');
  const cb = globalThis.navigator?.clipboard;
  if (!cb?.writeText) { failed(); return; }
  cb.writeText(text).then(done, failed);
}
