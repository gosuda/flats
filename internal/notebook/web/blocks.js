// Page rendering (TECH 18, rules 3, 12, 18): keyed blocks with semantic
// headings, lists, to-dos, toggles, code, images, page links and inline
// databases. Unchanged blocks keep their DOM; a changed block is patched in
// place, so an unrelated delta never replaces the page or the focused
// editor. While an input method composes, structure is left alone and only
// other blocks' content is patched (TECH 14).
'use strict';


import { h, icon, replace } from './dom.js';
import { renderRuns, sameRuns, plainText, mapOffset, domText } from './richtext.js';
import { isText, PARENT_KINDS } from './state.js';

export const KIND_NAMES = {
  paragraph: 'Text', heading1: 'Heading 1', heading2: 'Heading 2', heading3: 'Heading 3', bullet: 'Bulleted list',
  numbered: 'Numbered list', todo: 'To-do', toggle: 'Toggle', quote: 'Quote', callout: 'Callout', code: 'Code',
  divider: 'Divider', image: 'Image', page_link: 'Page link', database: 'Database',
};

const PLACEHOLDER = {
  paragraph: "Type '/' for blocks", heading1: 'Heading 1', heading2: 'Heading 2', heading3: 'Heading 3', bullet: 'List',
  numbered: 'List', todo: 'To-do', toggle: 'Toggle', quote: 'Quote', callout: 'Callout',
};

const TONE_LABEL = { neutral: '', info: 'Note', success: 'Done', warning: 'Warning', danger: 'Caution' };
const TONE_ICON = { neutral: 'info', info: 'info', success: 'ok', warning: 'warn', danger: 'bad' };
const LIST = ['bullet', 'numbered', 'todo'];

// editableTag gives headings their heading element so the page outline is
// real document structure; the page title is the h1.
const editableTag = (kind) => ({ heading1: 'h2', heading2: 'h3', heading3: 'h4' })[kind] || 'div';

function editable(kind, field, writable, label) {
  const el = h(editableTag(kind), { class: 't', 'data-field': field, 'data-ph': label });
  if (writable) {
    el.setAttribute('contenteditable', 'true');
    el.setAttribute('spellcheck', kind === 'code' ? 'false' : 'true');
    if (!kind.startsWith('heading')) {
      el.setAttribute('role', 'textbox');
      el.setAttribute('aria-multiline', 'true');
      el.setAttribute('aria-label', label);
    }
  }
  return el;
}

// assetURL is the checked image route for an asset hash.
export const assetURL = (hash, ext) => `/_notebook/a/${hash}.${ext || 'png'}`;

export class PageView {
  // ctx: {writable(), view(), composing(), presenceFor(id), assetExt(hash),
  //       renderDatabase(block, el), toggleOpen(id), isOpen(block), selected(id),
  //       onCheck(block, checked), onLanguage(block, text), handle(block, el)}
  constructor(ctx) {
    this.ctx = ctx;
    this.entries = new Map(); // block id -> {el, t, kids, sig, runs, kind}
    this.page = null;
    this.root = null;
  }

  editableOf(id, field = 'text') {
    const e = this.entries.get(id);
    if (!e) return null;
    return field === 'text' ? e.t : e.el.querySelector(`[data-field="${field}"]`);
  }

  entryOf(id) { return this.entries.get(id) || null; }

  confirmCreation(ref) {
    const e = this.entries.get(ref.id);
    if (e && e.inc === '0') e.inc = ref.inc;
  }

  // render brings container up to date with page's blocks in view.
  render(view, pageId, container) {
    if (this.page !== pageId) {
      this.entries.clear();
      container.replaceChildren();
      this.page = pageId;
    }
    this.root = container;
    const composing = this.ctx.composing();
    const seen = new Set();
    const fill = (parentEl, parent, depth) => {
      const nodes = [];
      let group = null, n = 0, groupIndex = 0;
      for (const b of view.blocks(pageId, parent)) {
        n = b.content.kind === 'numbered' ? n + 1 : 0;
        const e = this.#entry(b, depth, n, composing);
        seen.add(b.ref.id);
        if (PARENT_KINDS.includes(b.content.kind) && e.kids) {
          const open = b.content.kind !== 'toggle' || this.ctx.isOpen(b);
          if (!composing) e.kids.hidden = !open;
          if (open) fill(e.kids, b.ref.id, depth + 1);
          else for (const id of this.#descendants(view, pageId, b.ref.id)) seen.add(id);
        }
        if (LIST.includes(b.content.kind)) {
          if (!group || group.dataset.kind !== b.content.kind) {
            group = this.#group(parentEl, groupIndex++, b.content.kind);
            group.items = [];
            nodes.push(group);
          }
          group.items.push(e.el);
        } else {
          group = null;
          nodes.push(e.el);
        }
      }
      // While an input method composes, structure stays as it is so the
      // composing node is never moved or removed; content still updates.
      if (composing) return;
      for (const node of nodes) if (node.items) sync(node, node.items);
      sync(parentEl, nodes);
    };
    fill(container, null, 0);
    if (!composing) for (const id of [...this.entries.keys()]) if (!seen.has(id)) this.entries.delete(id);
  }

  #descendants(view, page, id) {
    const out = [];
    const walk = (p) => { for (const b of view.blocks(page, p)) { out.push(b.ref.id); walk(b.ref.id); } };
    walk(id);
    return out;
  }

  #group(parentEl, index, kind) {
    parentEl.groups ||= [];
    let g = parentEl.groups[index];
    if (!g || g.dataset.kind !== kind) {
      g = h(kind === 'numbered' ? 'ol' : 'ul', { class: 'lgrp', role: 'list', 'data-kind': kind });
      parentEl.groups[index] = g;
    }
    return g;
  }

  #entry(b, depth, num, composing) {
    const id = b.ref.id;
    const ctx = this.ctx;
    const sig = JSON.stringify([b.ref.inc, b.content, depth, num, ctx.writable(), ctx.isOpen(b), ctx.selected(id),
      ctx.presenceFor(id), b.content.kind === 'page_link' ? this.#linkTarget(b) : null, b.content.kind === 'image' ? ctx.assetExt(b.content.asset) : null]);
    let e = this.entries.get(id);
    if (e && e.sig === sig) {
      // A database's rows and schema are not in the block's signature, so
      // its renderer runs on every render and keeps its own keyed DOM.
      if (b.content.kind === 'database') ctx.renderDatabase(b, e.db);
      return e;
    }
    const active = composing && this.entries.get(composing.block);
    // Replacing an ancestor also detaches its composing descendant.
    if (e && active && e.el.contains(active.el)) return e;
    if (!e || e.kind !== b.content.kind || e.inc !== b.ref.inc || e.writable !== ctx.writable()) {
      const fresh = this.#build(b);
      if (e && e.el.parentNode) e.el.parentNode.replaceChild(fresh.el, e.el);
      if (e && e.kids && fresh.kids) fresh.kids.append(...Array.from(e.kids.childNodes));
      e = fresh;
      this.entries.set(id, e);
    }
    this.#update(e, b, depth, num);
    e.sig = sig;
    return e;
  }

  #linkTarget(b) {
    const p = this.ctx.view().get(b.content.target);
    return p ? p.title : null;
  }

  #build(b) {
    const kind = b.content.kind;
    const writable = this.ctx.writable();
    const el = h(LIST.includes(kind) ? 'li' : 'div', { class: 'blk', 'data-id': b.ref.id, 'data-k': kind });
    const e = { el, kind, inc: b.ref.inc, writable, t: null, kids: null, runs: null, mark: null };
    if (writable) {
      el.appendChild(h('div', { class: 'h' },
        h('button', { type: 'button', class: 'add', 'aria-label': 'Add block below', tabindex: '-1' }, icon('plus')),
        h('button', { type: 'button', class: 'grip', 'aria-label': 'Block actions', 'aria-haspopup': 'menu', draggable: 'true' }, icon('grip'))));
    }
    const body = h('div', { class: 'body' });
    if (isText(kind)) {
      if (kind === 'bullet' || kind === 'numbered') e.mark = el.appendChild(h('div', { class: 'mark', 'aria-hidden': 'true' }));
      if (kind === 'todo') {
        const cb = h('input', { type: 'checkbox', 'aria-label': 'Done' });
        if (!writable) cb.setAttribute('disabled', '');
        cb.addEventListener('change', () => this.ctx.onCheck(this.ctx.view().get(b.ref.id), cb.checked));
        e.mark = el.appendChild(h('div', { class: 'mark' }, cb));
      }
      if (kind === 'toggle') {
        const btn = h('button', { type: 'button', class: 'toggle-btn' }, icon('chev'));
        btn.addEventListener('click', () => this.ctx.toggleOpen(b.ref.id));
        e.mark = el.appendChild(btn);
      }
      e.t = editable(kind, 'text', writable, PLACEHOLDER[kind]);
      if (kind === 'callout') {
        e.tone = h('span', { class: 'tone' });
        e.toneIcon = h('span', { class: 'tone-icon' });
        body.appendChild(h('div', { class: 'callout' }, e.toneIcon, h('div', { class: 'callout-body' }, e.tone, e.t)));
      } else if (kind === 'quote') {
        body.appendChild(h('blockquote', { class: 'quote' }, e.t));
      } else {
        body.appendChild(e.t);
      }
    } else if (kind === 'code') {
      e.t = editable('code', 'text', writable, 'Code');
      e.t.classList.add('code-text');
      e.lang = h('input', { class: 'lang', type: 'text', 'aria-label': 'Code language', placeholder: 'Language', maxlength: '200' });
      if (!writable) e.lang.setAttribute('readonly', '');
      e.lang.addEventListener('change', () => this.ctx.onLanguage(this.ctx.view().get(b.ref.id), e.lang.value));
      body.appendChild(h('div', { class: 'code' }, e.lang, h('pre', {}, e.t)));
    } else if (kind === 'divider') {
      body.appendChild(h('hr', { class: 'div' }));
    } else if (kind === 'image') {
      e.img = h('img', { alt: '', loading: 'lazy', decoding: 'async' });
      e.caption = editable('paragraph', 'caption', writable, 'Caption');
      e.caption.classList.add('caption');
      body.appendChild(h('figure', { class: 'image' }, e.img, h('figcaption', {}, e.caption)));
    } else if (kind === 'page_link') {
      e.link = body.appendChild(h('a', { class: 'plink' }));
    } else if (kind === 'database') {
      e.db = body.appendChild(h('div', { class: 'db', 'data-database': b.content.database }));
    }
    if (PARENT_KINDS.includes(kind)) e.kids = body.appendChild(h('div', { class: 'kids' }));
    el.appendChild(body);
    if (writable) this.ctx.handle(b, el);
    return e;
  }

  #update(e, b, depth, num) {
    const c = b.content, ctx = this.ctx;
    e.el.dataset.depth = String(depth);
    e.el.classList.toggle('sel', ctx.selected(b.ref.id));
    const who = ctx.presenceFor(b.ref.id);
    e.el.classList.toggle('remote', !!who);
    let tag = e.el.querySelector('.editing-tag');
    if (who && !tag) tag = e.el.appendChild(h('span', { class: 'editing-tag' }));
    if (tag) { if (who) tag.textContent = `${who} is editing`; else tag.remove(); }
    if (e.t && (isText(c.kind) || c.kind === 'code')) {
      const runs = c.kind === 'code' ? [{ text: c.text, marks: [] }] : c.text;
      this.setRuns(e, runs);
    }
    if (c.kind === 'bullet') e.mark.textContent = depth % 2 ? '◦' : '•';
    if (c.kind === 'numbered') e.mark.textContent = num + '.';
    if (c.kind === 'todo') {
      const cb = e.mark.firstChild;
      cb.checked = !!c.checked;
      e.el.classList.toggle('done', !!c.checked);
    }
    if (c.kind === 'toggle') {
      const open = ctx.isOpen(b);
      e.mark.setAttribute('aria-expanded', String(open));
      e.mark.setAttribute('aria-label', (open ? 'Collapse ' : 'Expand ') + (plainText(c.text) || 'toggle'));
    }
    if (c.kind === 'callout') {
      e.el.dataset.tone = c.tone;
      e.tone.textContent = TONE_LABEL[c.tone] ? TONE_LABEL[c.tone] + '. ' : '';
      e.tone.setAttribute('contenteditable', 'false');
      replace(e.toneIcon, icon(TONE_ICON[c.tone]));
    }
    if (c.kind === 'code' && e.lang.value !== c.language && document.activeElement !== e.lang) e.lang.value = c.language;
    if (c.kind === 'image') {
      e.img.setAttribute('src', assetURL(c.asset, ctx.assetExt(c.asset)));
      e.img.setAttribute('alt', c.alt);
      this.setRuns(e, c.caption, 'caption');
    }
    if (c.kind === 'page_link') {
      const title = this.#linkTarget(b);
      e.link.replaceChildren(icon('page'));
      if (title == null) {
        e.link.removeAttribute('href');
        e.link.className = 'plink missing';
        e.link.setAttribute('title', 'The linked page was deleted. Restoring it from History brings the link back.');
        e.link.append('Missing page');
      } else {
        e.link.className = 'plink';
        e.link.removeAttribute('title');
        e.link.setAttribute('href', '#/' + c.target);
        e.link.append(title || 'Untitled');
      }
    }
    if (c.kind === 'database') ctx.renderDatabase(b, e.db);
  }

  // setRuns shows runs in an editable unless it already shows them. The
  // focused editable keeps its caret through a bounded prefix and suffix
  // mapping (TECH 18); other editables are simply replaced.
  setRuns(e, runs, field = 'text') {
    const el = field === 'text' ? e.t : e.caption;
    const key = field === 'text' ? 'runs' : 'captionRuns';
    const old = e[key];
    if (old && sameRuns(old, runs) && el.firstChild) return;
    e[key] = runs;
    const caret = document.activeElement === el && this.ctx.caret ? this.ctx.caret.read(el) : null;
    const from = caret ? domText(el) : '';
    el.replaceChildren(...renderRuns(runs));
    if (caret) {
      const to = plainText(runs);
      this.ctx.caret.place(el, { start: mapOffset(from, to, caret.start), end: mapOffset(from, to, caret.end) });
    }
  }
}

// sync makes parent's children exactly nodes, moving only nodes out of
// place, so a focused editor keeps its DOM node.
function sync(parent, nodes) {
  const want = new Set(nodes);
  for (let i = 0; i < nodes.length; i++) {
    const at = parent.childNodes[i];
    if (at !== nodes[i]) parent.insertBefore(nodes[i], at || null);
  }
  for (const c of Array.from(parent.childNodes).slice(nodes.length)) if (!want.has(c)) c.remove();
}
