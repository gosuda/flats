// Flats design bridge. The host injects this script into every artboard the
// canvas shows. The artboard runs sandboxed at an opaque origin and talks to
// the canvas app only through postMessage. Any script in the artboard can
// send the same messages, so the canvas treats them as hints: a change
// always needs input the person makes in the canvas itself.
(() => {
  'use strict';
  if (window.__flatsBridge) return;
  window.__flatsBridge = true;
  const parentWin = window.parent !== window ? window.parent : null;
  if (!parentWin) return;
  const post = (msg) => parentWin.postMessage(Object.assign({ flats: 'design' }, msg), '*');
  // The file generation the node ids of this document belong to.
  const gen = Number((document.currentScript && document.currentScript.getAttribute('data-flats-gen')) || 0);
  let mode = 'view';
  let outlined = null;
  let boards = new Set(); // artboard paths of this design, from the canvas
  // This document's path in the design: after /_design/c/<cap>/.
  const self = (location.pathname.match(/^\/_design\/c\/[^/]+\/(.*)$/) || [])[1] || location.pathname.slice(1);

  window.addEventListener('error', (e) => post({ type: 'error', message: String(e.message || 'script error').slice(0, 300) }));
  window.addEventListener('unhandledrejection', (e) => post({ type: 'error', message: String((e.reason && e.reason.message) || e.reason || 'unhandled rejection').slice(0, 300) }));

  document.addEventListener('click', (e) => {
    if (mode === 'edit') {
      e.preventDefault();
      e.stopPropagation();
      const el = e.target.closest && e.target.closest('[data-flats-node]');
      if (!el) { post({ type: 'pick', node: -1 }); return; }
      const r = el.getBoundingClientRect();
      const cs = getComputedStyle(el);
      post({
        type: 'pick', gen, node: Number(el.getAttribute('data-flats-node')),
        rect: { x: r.left, y: r.top, w: r.width, h: r.height },
        style: { font: cs.font, color: cs.color, align: cs.textAlign, lineHeight: cs.lineHeight, letterSpacing: cs.letterSpacing, padding: cs.padding, background: cs.backgroundColor },
      });
      return;
    }
    // Links to another artboard of this design move a played prototype.
    // Only links to another artboard are taken over; any other link
    // (including an HTML file that is not an artboard) navigates natively.
    const a = e.target.closest && e.target.closest('a[href]');
    if (!a) return;
    const href = a.getAttribute('href') || '';
    if (/^[a-z][a-z0-9+.-]*:/i.test(href) || href.startsWith('//') || href.startsWith('#')) return;
    const rel = href.split('#')[0].split('?')[0];
    const base = self.includes('/') ? self.slice(0, self.lastIndexOf('/') + 1) : '';
    const parts = [];
    for (const p of (rel.startsWith('/') ? rel.slice(1) : base + rel).split('/')) {
      if (p === '..') parts.pop(); else if (p && p !== '.') parts.push(decodeURIComponent(p));
    }
    const target = parts.join('/');
    if (!boards.has(target)) return;
    e.preventDefault();
    post({ type: 'navigate', href: '/' + target });
  }, true);

  function outline(node) {
    if (outlined) outlined.classList.remove('flats-picked');
    outlined = node >= 0 ? document.querySelector('[data-flats-node="' + Number(node) + '"]') : null;
    if (outlined) outlined.classList.add('flats-picked');
  }

  window.addEventListener('message', (e) => {
    if (e.source !== parentWin || !e.data || e.data.flats !== 'design') return;
    const m = e.data;
    if (m.type === 'mode') {
      mode = m.mode === 'edit' ? 'edit' : m.mode === 'play' ? 'play' : 'view';
      document.documentElement.classList.toggle('flats-edit-mode', mode === 'edit');
      if (mode !== 'edit') outline(-1);
    } else if (m.type === 'tweak' && typeof m.name === 'string' && /^[a-z][a-z0-9-]{0,39}$/.test(m.name)) {
      document.documentElement.style.setProperty('--' + m.name, String(m.css));
    } else if (m.type === 'boards' && Array.isArray(m.paths)) {
      boards = new Set(m.paths.map(String));
    } else if (m.type === 'outline') {
      outline(Number(m.node));
    }
  });

  const style = document.createElement('style');
  style.setAttribute('data-flats-host', '');
  style.textContent = '.flats-edit-mode [data-flats-node]{cursor:text;outline:1px dashed rgba(37,99,235,.5);outline-offset:2px}.flats-edit-mode [data-flats-node]:hover{outline-color:rgba(37,99,235,.95)}.flats-picked{outline:2px solid #2563eb!important;outline-offset:2px;visibility:hidden}';
  const ready = () => {
    (document.head || document.documentElement).appendChild(style);
    post({ type: 'ready' });
  };
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', ready);
  else ready();
})();
