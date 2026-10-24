// Shared UI pieces: confirm dialogs, popup menus, toasts, error panels and
// copyable snippets.

import { h, clear, icon, linkTargets } from './dom.js';

// confirmDialog opens a modal <dialog> and resolves true when confirmed.
// opts: title, body (string | Node | array), confirmLabel, danger,
// requireText (the user must type this exactly to enable the confirm button).
export function confirmDialog(opts) {
  return new Promise((resolve) => {
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

// slideConfirm builds a "slide to unlock" control for an action that cannot
// be undone (docs/spec/console-row-delete.md). Only the handle drags, a
// release before the end returns it, and each Right or Left press moves it a
// tenth (held-key repeats are ignored); Up and Down are left to the menu
// around it. onEnd runs once the handle reaches the end; the caller then
// calls reset(text) to allow another try or lock(text) to leave it disabled.
export function slideConfirm({ text, name, busyText, busyAnnouncement, onEnd }) {
  const fill = h('div', { class: 'slide-fill' });
  const label = h('div', { class: 'slide-label', 'aria-hidden': 'true' });
  const status = h('p', { class: 'sr-only', role: 'status', 'aria-live': 'polite' });
  const handle = h('div', {
    class: 'slide-handle', role: 'slider', tabindex: '0', 'aria-label': `${text} ${name}`,
    'aria-valuemin': '0', 'aria-valuemax': '100', 'aria-valuenow': '0', 'aria-valuetext': '0%',
  });
  const el = h('div', { class: 'slide', 'data-state': 'idle' }, fill, label, handle, status);
  let p = 0;
  let pointer = null;
  let startX = 0;
  let startP = 0;
  let returning;
  const state = () => el.getAttribute('data-state');
  const setState = (s) => el.setAttribute('data-state', s);
  const settled = () => state() === 'busy' || state() === 'locked';
  // travel is how far the handle moves, in pixels, from start to end.
  const travel = () => {
    const t = el.clientWidth - handle.offsetWidth - 2 * handle.offsetLeft;
    return t > 0 ? t : 0;
  };
  function show(value) {
    p = Math.min(1, Math.max(0, value));
    el.style.setProperty('--p', String(p));
    el.style.setProperty('--travel', travel() + 'px');
    // Floor, so 100% is announced only at the end, where deletion starts.
    const pct = Math.floor(p * 100);
    handle.setAttribute('aria-valuenow', String(pct));
    handle.setAttribute('aria-valuetext', pct + '%');
  }
  function idleLabel(failed) {
    clear(label).appendChild(failed ? h('span', { class: 'slide-failed', text: failed }) : h('span', { class: 'slide-shine', text }));
  }
  function reached() {
    if (p < 1 || settled()) return false;
    pointer = null;
    window.removeEventListener('blur', cancel);
    setState('busy');
    clear(label).appendChild(h('span', { text: busyText }));
    clear(handle).appendChild(icon('spinner'));
    handle.setAttribute('aria-disabled', 'true');
    status.textContent = busyAnnouncement;
    onEnd();
    return true;
  }
  // cancel ends an unfinished drag: a release, a lost pointer, or focus
  // leaving the handle or the window. Later events of that drag do nothing.
  function cancel() {
    if (pointer !== null) {
      try { handle.releasePointerCapture(pointer); } catch { /* already released */ }
      pointer = null;
    }
    window.removeEventListener('blur', cancel);
    if (!settled() && (p > 0 || state() === 'dragging')) back(true);
  }
  function back(animate) {
    clearTimeout(returning);
    setState(animate ? 'returning' : 'idle');
    show(0);
    if (animate) returning = setTimeout(() => { if (state() === 'returning') setState('idle'); }, 240);
  }
  handle.addEventListener('pointerdown', (e) => {
    if (settled() || (e.button !== undefined && e.button !== 0)) return;
    e.preventDefault();
    handle.focus();
    pointer = e.pointerId;
    try { handle.setPointerCapture(e.pointerId); } catch { /* the pointer is already gone */ }
    startX = e.clientX;
    startP = p;
    clearTimeout(returning);
    setState('dragging');
    window.addEventListener('blur', cancel);
  });
  handle.addEventListener('pointermove', (e) => {
    if (e.pointerId !== pointer) return;
    const t = travel();
    if (!t) return;
    show(startP + (e.clientX - startX) / t);
    reached();
  });
  const release = (e) => { if (e.pointerId === pointer) cancel(); };
  handle.addEventListener('pointerup', release);
  handle.addEventListener('pointercancel', release);
  handle.addEventListener('lostpointercapture', release);
  handle.addEventListener('keydown', (e) => {
    if (settled()) return;
    const step = { ArrowRight: 1, ArrowLeft: -1 }[e.key];
    // These keys belong to the slider, not to a menu around it.
    if (step || e.key === 'Home' || e.key === 'End' || e.key === 'PageUp' || e.key === 'PageDown') {
      e.preventDefault();
      e.stopPropagation();
    }
    if (e.repeat) return;
    if (step) {
      clearTimeout(returning);
      setState('idle');
      show(Math.round(p * 10 + step) / 10);
      reached();
    } else if (e.key === 'Home') back(false);
  });
  handle.addEventListener('blur', cancel);
  idleLabel();
  clear(handle).appendChild(icon('chevrons'));
  show(0);
  return {
    el,
    handle,
    // reset returns the handle for another try; failed replaces the label.
    reset(failed) {
      handle.removeAttribute('aria-disabled');
      status.textContent = failed || '';
      idleLabel(failed);
      clear(handle).appendChild(icon('chevrons'));
      back(true);
    },
    lock(lockedText) {
      setState('locked');
      handle.setAttribute('aria-disabled', 'true');
      status.textContent = lockedText;
      clear(label).appendChild(h('span', { text: lockedText }));
      clear(handle).appendChild(icon('more'));
    },
  };
}

// menu builds a "…" button with a popup menu.
// items: [{label, onSelect, danger, disabled, className}] or {separator: true};
// className lets CSS show an item at some widths only (a hidden item is
// skipped by the keyboard). An item with inPlace(ctl) instead keeps the menu
// open when chosen and works inside it:
//   ctl.replace(node)  puts node where the item is, until the menu closes;
//   ctl.hold(on)       keeps the menu open and its other items disabled;
//   ctl.close(refocus) closes the menu;
//   ctl.onClose(fn)    runs fn once when the menu next closes.
let openMenu = null;

export function menu(label, items, iconName) {
  const id = 'menu-' + Math.random().toString(36).slice(2);
  const btn = h('button', {
    type: 'button', class: 'icon-btn', 'aria-haspopup': 'menu', 'aria-expanded': 'false',
    'aria-controls': id, 'aria-label': label, title: label,
  }, icon(iconName || 'more'));
  const list = h('div', { class: 'menu', role: 'menu', id, hidden: true });
  let held = false;
  let swaps = [];
  let onClose = [];
  const ctl = {
    replace(item, node) {
      item.replaceWith(node);
      swaps.push([node, item]);
    },
    // Links cannot be disabled natively: aria-disabled marks them, and their
    // clicks are dropped while held.
    hold(on) {
      held = on;
      for (const el of list.querySelectorAll('.menu-item')) {
        if (el.tagName === 'A') {
          if (on) el.setAttribute('aria-disabled', 'true');
          else el.removeAttribute('aria-disabled');
        } else if (on) { el.dataset.wasDisabled = String(!!el.disabled); el.disabled = true; }
        else if (el.dataset.wasDisabled !== undefined) { el.disabled = el.dataset.wasDisabled === 'true'; delete el.dataset.wasDisabled; }
      }
    },
    close: (refocus) => close(refocus),
    onClose(fn) { onClose.push(fn); },
  };
  for (const it of items) {
    if (it.separator) { list.appendChild(h('div', { class: 'menu-sep', role: 'separator' })); continue; }
    // An item with href is an external link (opened like extLink); one the
    // console cannot open from this device is shown disabled.
    const href = it.href ? safeHref(it.href) : '';
    const blocked = it.href && (!href || serverOnly(href));
    const content = it.detail ? [h('span', { class: 'menu-label', text: it.label }), h('span', { class: 'menu-detail muted small', text: it.detail })] : null;
    const mi = href && !blocked
      ? h('a', { role: 'menuitem', tabindex: '-1', class: 'menu-item' + (it.className ? ' ' + it.className : ''), href, target: '_blank', rel: 'noopener noreferrer' }, content || it.label)
      : h('button', {
        type: 'button', role: 'menuitem', tabindex: '-1', title: blocked ? SERVER_ONLY : undefined,
        class: 'menu-item' + (it.danger ? ' danger' : '') + (it.className ? ' ' + it.className : ''), disabled: !!it.disabled || !!blocked,
      }, content || it.label);
    if (it.inPlace) {
      mi.addEventListener('click', () => it.inPlace({ ...ctl, replace: (node) => ctl.replace(mi, node) }));
    } else {
      mi.addEventListener('click', (e) => {
        if (held) { e.preventDefault(); return; }
        close(!href);
        if (it.onSelect) it.onSelect();
      });
    }
    list.appendChild(mi);
  }
  const wrap = h('div', { class: 'menu-wrap' }, btn, list);
  // Items and an enabled in-place slider take part in arrow-key navigation.
  const enabled = () => [...list.querySelectorAll('.menu-item:not([disabled]):not([aria-disabled="true"]), [role="slider"]:not([aria-disabled="true"])')]
    .filter((el) => el.getClientRects().length);

  function open() {
    if (openMenu && openMenu !== close && openMenu(false) === false) return;
    list.hidden = false;
    btn.setAttribute('aria-expanded', 'true');
    openMenu = close;
    const first = enabled()[0];
    if (first) first.focus();
    document.addEventListener('pointerdown', outside, true);
  }
  // close resolves false, leaving the menu open, while it is held, unless
  // forced (leaving the page).
  function close(refocus, force) {
    if (held && !force) return false;
    list.hidden = true;
    btn.setAttribute('aria-expanded', 'false');
    document.removeEventListener('pointerdown', outside, true);
    if (openMenu === close) openMenu = null;
    for (const [node, item] of swaps.reverse()) node.replaceWith(item);
    swaps = [];
    held = false;
    const fns = onClose;
    onClose = [];
    if (refocus) btn.focus();
    for (const fn of fns) fn();
    return true;
  }
  // A press outside closes the menu. While held it does nothing at all:
  // focus stays where it is and the click that press makes (on a link, say)
  // is dropped, even if the deletion ends before the press does. Guards are
  // kept per pointer: each ends with its own pointer's release or cancel, a
  // new press of that pointer, or the window losing focus, so other pointers
  // and keys cannot lift it, and keyboard clicks (detail 0) are never dropped.
  const guards = new Map();
  let guardEvent = null;
  const pointerOf = (e) => (e.pointerId === undefined || e.pointerId < 0 ? null : e.pointerId);
  function onClick(c) {
    if (!c.detail) return;
    const id = pointerOf(c);
    if (id === null ? guards.size : guards.has(id)) { c.preventDefault(); c.stopPropagation(); }
  }
  function endGuard(id) {
    guards.delete(id);
    if (guards.size) return;
    document.removeEventListener('click', onClick, true);
    document.removeEventListener('pointerup', onRelease, true);
    document.removeEventListener('pointercancel', onRelease, true);
    document.removeEventListener('pointerdown', onPress, true);
    window.removeEventListener('blur', endAll);
  }
  function endAll() { for (const id of [...guards.keys()]) endGuard(id); }
  // The press's click comes right after its pointerup, in the same task.
  function onRelease(e) {
    const id = pointerOf(e);
    const g = guards.get(id);
    if (g) setTimeout(() => { if (guards.get(id) === g) endGuard(id); }, 0);
  }
  function onPress(e) { if (e !== guardEvent && guards.has(pointerOf(e))) endGuard(pointerOf(e)); }
  // guardPress drops the click of press e and keeps focus where it is.
  function guardPress(e) {
    e.preventDefault?.();
    if (!guards.size) {
      document.addEventListener('click', onClick, true);
      document.addEventListener('pointerup', onRelease, true);
      document.addEventListener('pointercancel', onRelease, true);
      document.addEventListener('pointerdown', onPress, true);
      window.addEventListener('blur', endAll);
    }
    guardEvent = e;
    guards.set(pointerOf(e), {});
  }
  function outside(e) {
    if (wrap.contains(e.target)) return;
    if (!held) close(false);
    else guardPress(e);
  }

  btn.addEventListener('click', () => (list.hidden ? open() : close(false)));
  // While held, a press on this menu's own button or on a held item does
  // nothing either, even if the hold ends before the press does.
  btn.addEventListener('pointerdown', (e) => { if (held) guardPress(e); });
  list.addEventListener('pointerdown', (e) => { if (held && !e.target.closest?.('[role="slider"]')) guardPress(e); });
  btn.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowDown') { e.preventDefault(); open(); }
  });
  list.addEventListener('keydown', (e) => {
    // While held, focus stays where it is and the menu stays open.
    if (held && ['Escape', 'Tab', 'ArrowDown', 'ArrowUp', 'Home', 'End'].includes(e.key)) { e.preventDefault(); return; }
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

export function closeMenus() { if (openMenu) openMenu(false, true); }

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

// loopbackHost reports whether a host name reaches only the machine it is
// opened on: localhost and *.localhost (with or without a trailing dot),
// 127.0.0.0/8, ::1, the unspecified 0.0.0.0 and ::, and their IPv4-mapped
// IPv6 forms. It expects a URL hostname, which the URL parser has already
// lowercased and normalized (127.1 and 2130706433 become 127.0.0.1, and
// ::ffff:127.0.0.1 becomes ::ffff:7f00:1).
export function loopbackHost(host) {
  const name = String(host || '').toLowerCase().replace(/^\[|\]$/g, '').replace(/\.+$/, '');
  if (name === 'localhost' || name.endsWith('.localhost')) return true;
  if (name === '0.0.0.0' || /^127\.\d+\.\d+\.\d+$/.test(name)) return true;
  if (name === '::1' || name === '::') return true;
  const mapped = /^(?:0{0,4}:){0,5}:?ffff:(.+)$/.exec(name);
  if (!mapped) return false;
  const v4 = mapped[1];
  if (v4.includes('.')) return v4 === '0.0.0.0' || /^127\./.test(v4);
  const [hi = '', lo = ''] = v4.split(':');
  const high = parseInt(hi, 16), low = parseInt(lo, 16);
  return (high >> 8) === 127 || (high === 0 && low === 0);
}

// serverOnly reports whether href opens only on the Flats host and this
// console is being viewed from another device, where it would open that
// device instead.
export function serverOnly(href) {
  const safe = safeHref(href);
  return Boolean(safe) && !consoleOnServer() && loopbackHost(new URL(safe).hostname);
}

// consoleOnServer is true when this console was opened through a loopback
// address, i.e. on the Flats host itself.
export function consoleOnServer() {
  const loc = globalThis.location;
  let host = loc?.hostname;
  if (!host && loc?.origin) {
    try { host = new URL(loc.origin).hostname; } catch { host = ''; }
  }
  return !host || loopbackHost(host);
}

// SERVER_ONLY explains a Local link that opens only on the Flats host. While
// a reverse proxy provider is on for the host, it names the proxy as another
// way to open the flat from other devices (setHostProviders).
const SERVER_ONLY_TEXT = {
  '': 'Opens only on the Flats host machine. To open it from other devices, allow Tailscale or NetBird in this flat’s Settings → Networks → Private (turning one on in host Settings is not enough).',
  proxy: 'Opens only on the Flats host machine. To open it from other devices, allow Tailscale, NetBird or Reverse proxy in this flat’s Settings → Networks → Private (turning one on in host Settings is not enough).',
  'proxy-public': 'Opens only on the Flats host machine. To open it from other devices, allow Tailscale or NetBird in this flat’s Settings → Networks → Private, or, while the flat is public, Reverse proxy (public) under Public (turning one on in host Settings is not enough).',
};
export let SERVER_ONLY = SERVER_ONLY_TEXT[''];

// setHostProviders records which providers are on for the host, from
// GET /console/api/providers, for the server-only explanation. Links drawn
// before it changed get the new explanation.
export function setHostProviders(providers) {
  const proxy = (providers || []).find((p) => (p.id === 'proxy' || p.id === 'proxy-public') && p.enabled);
  const next = SERVER_ONLY_TEXT[proxy ? proxy.id : ''];
  if (next === SERVER_ONLY) return;
  const prev = SERVER_ONLY;
  SERVER_ONLY = next;
  replaceNote(globalThis.document && document.body, prev, next);
}

// replaceNote swaps the server-only explanation in the titles and
// accessible names under node.
function replaceNote(node, prev, next) {
  if (!node) return;
  if (node.getAttribute) {
    if (node.getAttribute('title') === prev) node.setAttribute('title', next);
    const label = node.getAttribute('aria-label');
    if (label && label.includes(prev)) node.setAttribute('aria-label', label.replace(prev, next));
  }
  for (const c of node.childNodes || []) replaceNote(c, prev, next);
}

// extLink is an external link that opens in a new tab without an opener. A
// URL that is not http(s) is shown as text instead, and so is a loopback URL
// when the console is viewed from another device: it would open that device,
// not the Flats host. An icon-only link (class icon-btn) keeps its fixed
// size: instead of a text badge that would overflow into its neighbours it
// becomes a disabled-looking button that explains itself in its tooltip, its
// accessible name and, on click or tap, a toast.
export function extLink(href, text, cls) {
  const safe = safeHref(href);
  if (!safe) return h('span', { class: cls ? cls + ' is-disabled' : undefined }, text || String(href ?? ''));
  if (serverOnly(safe)) {
    if ((cls || '').split(/\s+/).includes('icon-btn')) {
      const label = text?.getAttribute?.('aria-label');
      if (label) text.setAttribute('aria-label', `${label}, server only. ${SERVER_ONLY}`);
      const btn = h('button', { type: 'button', class: cls + ' is-disabled server-only', title: SERVER_ONLY, 'aria-disabled': 'true' }, text || href);
      btn.addEventListener('click', () => toast(SERVER_ONLY));
      return btn;
    }
    // Without its own text the link names the network instead of the
    // address: a Local address means nothing on this device.
    return h('span', { class: (cls ? cls + ' ' : '') + 'is-disabled server-only', title: SERVER_ONLY, 'aria-disabled': 'true' },
      text || LOCAL_ADDRESS, h('span', { class: 'badge badge-warn', text: 'server only' }));
  }
  return h('a', { href: safe, target: '_blank', rel: 'noopener noreferrer', class: cls }, text || href);
}

// LOCAL_ADDRESS stands in for a Local address the console does not show
// because this device cannot open it.
export const LOCAL_ADDRESS = 'Local address';

// addressText is a URL to show as plain text: a Local address seen from
// another device becomes LOCAL_ADDRESS.
export function addressText(url) {
  return url && serverOnly(url) ? LOCAL_ADDRESS : url;
}

// openTargets are a flat's addresses (linkTargets) that open from this
// device: seen from another device, Local addresses are left out. local is
// the left-out private Local target, for a disabled button that explains it.
export function openTargets(f) {
  const targets = linkTargets(f);
  const local = targets.private.find((t) => serverOnly(t.url)) || null;
  return {
    private: targets.private.filter((t) => !serverOnly(t.url)),
    public: targets.public.filter((t) => !serverOnly(t.url)),
    local,
  };
}

// privateURL is the flat's private_url, or, when that is a Local address
// this device cannot open, the first private address that it can.
export function privateURL(f) {
  if (!f?.private_url || !serverOnly(f.private_url)) return f?.private_url;
  return openTargets(f).private[0]?.url || f.private_url;
}

// busy disables a button while fn runs.
export async function busy(btn, fn) {
  const was = btn.disabled;
  btn.disabled = true;
  btn.setAttribute('aria-busy', 'true');
  try { return await fn(); } finally {
    btn.disabled = was;
    btn.removeAttribute('aria-busy');
  }
}

// openLinks is a flat's open shortcuts: a private button (lock icon) and,
// while the flat is public, a public one (globe icon) to its left. Without a
// public button an empty slot of the same size takes its place, so the
// private button keeps its position whatever the visibility, also in the
// left-aligned narrow layout. Each is a plain icon link with one address and
// a list of its networks with several. Seen from another device, Local
// addresses are not offered, so the lock opens the one other private address
// directly; with none the private button stays, disabled, and explains how to
// open the flat from other devices.
export function openLinks(f) {
  const name = f.name || f.slug;
  const targets = openTargets(f);
  const button = (isPublic) => {
    let ts = isPublic ? targets.public : targets.private;
    const kind = isPublic ? 'public' : 'private';
    const glyph = isPublic ? 'globe' : 'lock';
    const note = isPublic ? ' (anyone with the URL can open the flat)' : '';
    if (!ts.length) {
      if (isPublic || !targets.local) return null;
      ts = [targets.local];
    }
    if (ts.length === 1) {
      return extLink(ts[0].url, icon(glyph, `Open ${name} ${kind}ly through ${ts[0].label}${note}`), 'icon-btn open-' + kind);
    }
    return menu(`Open ${name} ${kind}ly: choose a network${note}`, ts.map((t) => ({
      label: t.label, detail: t.url.replace(/^https?:\/\//, ''), href: t.url,
    })), glyph);
  };
  const priv = button(false);
  const pub = button(true) || (priv ? h('span', { class: 'icon-btn open-spacer', 'aria-hidden': 'true' }) : null);
  return pub || priv ? h('div', { class: 'open-links' }, pub, priv) : null;
}

// openPrimary is the single open shortcut of the narrow layout: one link to
// the flat's first public address while it is public, else its first private
// one, skipping addresses that only open on the Flats host when the console
// is viewed from another device (a phone on the tailnet or NetBird would
// otherwise get the Local address). Its glyph is the external-link arrow, so
// the globe and lock stay the visibility's own icons; openMenuItems lists
// every address for the menu.
export function openPrimary(f) {
  const name = f.name || f.slug;
  const targets = openTargets(f);
  const t = [...targets.public, ...targets.private][0] || targets.local;
  if (!t) return null;
  const note = t.public ? ' (anyone with the URL can open the flat)' : '';
  return extLink(t.url, icon('external', `Open ${name} ${t.public ? 'public' : 'private'}ly through ${t.label}${note}`), 'icon-btn open-primary');
}

// openMenuItems are menu entries for each address of a flat that opens from
// this device, public ones first, shown only where the narrow layout
// replaces openLinks.
export function openMenuItems(f) {
  const targets = openTargets(f);
  return [...targets.public, ...targets.private].map((t) => ({
    label: `Open ${t.public ? 'publicly' : 'privately'} · ${t.label}`,
    detail: t.url.replace(/^https?:\/\//, ''), href: t.url, className: 'menu-narrow',
  }));
}
