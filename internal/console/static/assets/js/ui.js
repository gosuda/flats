// Shared UI pieces: confirm dialogs, popup menus, toasts, error panels and
// copyable snippets.

import { h, clear, icon } from './dom.js';

const restoreAfterBusy = new WeakSet();

// confirmDialog opens a modal <dialog> and resolves true when confirmed.
// opts: title, body (string | Node | array), confirmLabel, danger,
// requireText (the user must type this exactly to enable the confirm button).
export function confirmDialog(opts) {
  return new Promise((resolve) => {
    const active = document.activeElement;
    const previous = active && active !== document.body ? active : document.querySelector('[aria-busy="true"]');
    const titleId = 'dlg-title-' + Math.random().toString(36).slice(2);
    const confirmBtn = h('button', {
      type: 'submit', value: 'ok',
      class: opts.danger ? 'btn btn-danger' : 'btn btn-primary',
      text: opts.confirmLabel || 'Confirm',
    });
    let typed = null;
    if (opts.requireText) {
      const inputId = titleId + '-input';
      const input = h('input', { id: inputId, type: 'text', autocomplete: 'off', spellcheck: 'false', autocapitalize: 'off', class: 'mono' });
      confirmBtn.disabled = true;
      input.addEventListener('input', () => { confirmBtn.disabled = input.value !== opts.requireText; });
      typed = h('div', { class: 'field' },
        h('label', { for: inputId }, 'Type ', h('code', { text: opts.requireText }), ' to confirm'),
        input);
    }
    const cancelBtn = opts.noCancel ? null : h('button', { type: 'button', class: 'btn', value: 'cancel', text: opts.cancelLabel || 'Cancel' });
    const body = typeof opts.body === 'string' ? h('p', { text: opts.body }) : opts.body;
    const dlg = h('dialog', { class: 'dialog', 'aria-labelledby': titleId },
      h('form', { method: 'dialog' },
        h('h2', { id: titleId, text: opts.title }),
        h('div', { class: 'dialog-body' }, body, typed),
        h('div', { class: 'dialog-actions' }, cancelBtn, confirmBtn)));
    // Cancel is not a submit button, so Enter in a field never cancels.
    if (cancelBtn) cancelBtn.addEventListener('click', () => dlg.close('cancel'));
    dlg.addEventListener('close', () => {
      const ok = dlg.returnValue === 'ok';
      dlg.remove();
      resolve(ok);
      if (previous && typeof previous.focus === 'function') {
        if (previous.disabled) restoreAfterBusy.add(previous);
        else previous.focus();
      }
    });
    document.body.appendChild(dlg);
    dlg.showModal();
    const focus = dlg.querySelector('input') || cancelBtn || confirmBtn;
    if (focus) focus.focus();
  });
}

// infoDialog shows content with a single close button.
export function infoDialog(title, body) {
  return confirmDialog({ title, body, confirmLabel: 'Close', noCancel: true });
}

// menu builds a "…" button with a popup menu.
// items: [{label, onSelect, danger, disabled}]
let openMenu = null;

export function menu(label, items) {
  const id = 'menu-' + Math.random().toString(36).slice(2);
  const btn = h('button', {
    type: 'button', class: 'icon-btn', 'aria-haspopup': 'menu', 'aria-expanded': 'false',
    'aria-controls': id, 'aria-label': label, title: label,
  }, icon('more'));
  const list = h('div', { class: 'menu', role: 'menu', id, hidden: true });
  for (const it of items) {
    const mi = h('button', {
      type: 'button', role: 'menuitem', tabindex: '-1',
      class: 'menu-item' + (it.danger ? ' danger' : ''), disabled: !!it.disabled, text: it.label,
    });
    mi.addEventListener('click', () => { close(true); it.onSelect(); });
    list.appendChild(mi);
  }
  const wrap = h('div', { class: 'menu-wrap' }, btn, list);
  const enabled = () => [...list.querySelectorAll('.menu-item:not([disabled])')];

  function open() {
    if (openMenu && openMenu !== close) openMenu(false);
    list.hidden = false;
    btn.setAttribute('aria-expanded', 'true');
    openMenu = close;
    const first = enabled()[0];
    if (first) first.focus();
    document.addEventListener('pointerdown', outside, true);
  }
  function close(refocus) {
    list.hidden = true;
    btn.setAttribute('aria-expanded', 'false');
    document.removeEventListener('pointerdown', outside, true);
    if (openMenu === close) openMenu = null;
    if (refocus) btn.focus();
  }
  function outside(e) { if (!wrap.contains(e.target)) close(false); }

  btn.addEventListener('click', () => (list.hidden ? open() : close(false)));
  btn.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowDown') { e.preventDefault(); open(); }
  });
  list.addEventListener('keydown', (e) => {
    const els = enabled();
    const i = els.indexOf(document.activeElement);
    if (e.key === 'Escape') { e.preventDefault(); close(true); }
    else if (e.key === 'ArrowDown') { e.preventDefault(); els[(i + 1) % els.length]?.focus(); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); els[(i - 1 + els.length) % els.length]?.focus(); }
    else if (e.key === 'Home') { e.preventDefault(); els[0]?.focus(); }
    else if (e.key === 'End') { e.preventDefault(); els[els.length - 1]?.focus(); }
    else if (e.key === 'Tab') close(false);
  });
  return wrap;
}

export function closeMenus() { if (openMenu) openMenu(false); }

export function announce(message) {
  const el = document.getElementById('lifecycle-status');
  if (!el || !message) return;
  el.textContent = '';
  el.textContent = message;
}

export function toast(message, kind) {
  const box = document.getElementById('toasts');
  if (!box) return;
  const t = h('div', { class: 'toast' + (kind ? ' toast-' + kind : '') }, h('span', { text: message }));
  const dismiss = h('button', { type: 'button', class: 'icon-btn small', 'aria-label': 'Dismiss' }, icon('close'));
  dismiss.addEventListener('click', () => t.remove());
  t.appendChild(dismiss);
  box.appendChild(t);
  setTimeout(() => t.remove(), kind === 'error' ? 12000 : 5000);
}

// errorPanel renders an API error with validation problems and the failed
// health check, if any.
export function errorPanel(err, title) {
  const body = err && err.body ? err.body : {};
  const box = h('div', { class: 'alert alert-error', role: 'alert' },
    h('strong', { text: title || 'Something went wrong' }),
    h('p', { text: err.message || String(err) }));
  const problems = body.problems || [];
  if (problems.length) {
    box.appendChild(h('ul', { class: 'problems' }, problems.map((p) =>
      h('li', null,
        p.path ? h('code', { text: p.path }) : null, p.path ? ': ' : null,
        p.message,
        p.fix ? h('div', { class: 'fix' }, 'Fix: ', p.fix) : null))));
  }
  if (body.health) box.appendChild(healthBlock(body.health));
  return box;
}

export function healthBlock(hr) {
  const status = hr.status ? `HTTP ${hr.status}` : 'no response';
  return h('div', { class: 'health ' + (hr.ok ? 'ok' : 'bad') },
    h('div', null,
      h('strong', { text: hr.ok ? 'Health check passed' : 'Health check failed' }),
      ` · GET ${hr.path || '/'} → ${status}`, hr.millis ? ` in ${hr.millis} ms` : ''),
    hr.error ? h('div', { text: hr.error }) : null,
    hr.body_head ? h('pre', { class: 'body-head', text: hr.body_head }) : null);
}

// replaceWith swaps a section's content, keeping the element.
export function fill(el, ...children) {
  clear(el);
  for (const c of children.flat()) if (c) el.appendChild(c);
  return el;
}

export function loading(text) {
  return h('p', { class: 'muted loading', text: text || 'Loading…' });
}

// copyable renders a code block with a copy button.
export function copyable(text, label) {
  const code = h('pre', { class: 'snippet', tabindex: '0' }, h('code', { text }));
  const btn = h('button', { type: 'button', class: 'btn btn-small', 'aria-label': 'Copy ' + (label || 'snippet') }, icon('copy'), h('span', { text: 'Copy' }));
  btn.addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(text);
      btn.lastChild.textContent = 'Copied';
      setTimeout(() => { btn.lastChild.textContent = 'Copy'; }, 1500);
    } catch {
      const sel = window.getSelection();
      const range = document.createRange();
      range.selectNodeContents(code);
      sel.removeAllRanges();
      sel.addRange(range);
      toast('Copy is unavailable here; the text is selected — press Ctrl/Cmd+C.');
    }
  });
  return h('div', { class: 'copyable' }, label ? h('div', { class: 'copy-label', text: label }) : null, h('div', { class: 'copy-row' }, code, btn));
}

// safeHref returns href when it is an absolute http(s) URL, else null. URLs
// come from server data (status details, flat and preview addresses), so a
// javascript: or data: value must never become a link.
export function safeHref(href) {
  try {
    const u = new URL(String(href));
    return u.protocol === 'http:' || u.protocol === 'https:' ? u.href : null;
  } catch {
    return null;
  }
}

// extLink is an external link that opens in a new tab without an opener. A
// URL that is not http(s) is shown as text instead.
export function extLink(href, text, cls) {
  const safe = safeHref(href);
  if (!safe) return h('span', { class: cls ? cls + ' is-disabled' : undefined }, text || String(href ?? ''));
  return h('a', { href: safe, target: '_blank', rel: 'noopener noreferrer', class: cls }, text || href);
}

// busy disables a button while fn runs.
export async function busy(btn, fn) {
  const was = btn.disabled;
  const scope = btn.closest?.('.page') || document;
  const focusId = btn.id || btn.getAttribute('id');
  const focusText = btn.textContent;
  btn.disabled = true;
  btn.setAttribute('aria-busy', 'true');
  try { return await fn(); } finally {
    btn.disabled = was;
    btn.removeAttribute('aria-busy');
    if (restoreAfterBusy.has(btn)) {
      restoreAfterBusy.delete(btn);
      if (!btn.disabled && btn.isConnected !== false) btn.focus();
      else if (btn.isConnected === false) {
        const replacement = focusId ? document.getElementById(focusId) : [...scope.querySelectorAll('button')].find((el) => el.textContent === focusText);
        if (replacement && !replacement.disabled) replacement.focus();
        else {
          const draft = document.getElementById('draft');
          if (draft) { draft.setAttribute('tabindex', '-1'); draft.focus(); }
        }
      }
    }
  }
}
