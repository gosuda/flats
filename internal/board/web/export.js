// Export and import (rules 39, 40, 67, 68; TECH 9, 11.6): PNG of the whole
// board, a frame or the selection; the data file; importing data and
// recovery files; and creating recovery files of unsent edits.

import { $, h, icon, dialog, toast, download, plural } from './ui.js';
import { Painter } from './render.js';
import { elementBounds, union, inflate, center } from './geom.js';
import { zCompare } from './ids.js';
import { buildRecovery, recoveryZip } from './recovery.js';
import { readZipIndex, readZipEntry } from './zip.js';
import { boardAPI } from './history.js';
import { topFrameAt } from './ops.js';

const MP = 1024 * 1024;

function tablet() {
  return matchMedia('(pointer: coarse)').matches && Math.min(screen.width, screen.height) >= 600;
}

function slug(s) {
  return (s || 'board').toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '').slice(0, 40) || 'board';
}

const radio = (name, value, label, checked, extra) => h('label', { class: 'opt' + (checked ? ' on' : '') },
  h('input', { type: 'radio', name, value, checked }), ' ', label, extra || null);

function radios(legend, items) {
  const fs = h('fieldset', {}, h('legend', { text: legend }), h('div', { class: 'opts' }, items));
  fs.addEventListener('change', () => {
    for (const l of fs.querySelectorAll('.opt')) l.classList.toggle('on', l.querySelector('input').checked);
  });
  return fs;
}

export class Exporter {
  constructor(app) {
    this.app = app;
    $('#export-btn').addEventListener('click', () => this.open());
  }

  // ---- ranges ----

  rangeElements(range, frameId) {
    const doc = this.app.doc;
    const all = [...doc.all()];
    let targets;
    if (range === 'all') return all;
    if (range === 'frame') targets = all.filter((el) => el.id === frameId || el.frame === frameId);
    else {
      const ids = new Set(this.app.sel);
      for (const el of all) if (el.frame && ids.has(el.frame)) ids.add(el.id);
      targets = all.filter((el) => ids.has(el.id));
    }
    const inside = new Set(targets.map((el) => el.id));
    for (const el of all) {
      if (el.type === 'connector' && !inside.has(el.id) && el.ends.every((e) => e.el && inside.has(e.el))) targets.push(el);
    }
    return targets;
  }

  bounds(els) {
    const get = (id) => this.app.doc.get(id);
    let r = null;
    for (const el of els) r = union(r, elementBounds(el, get));
    return r ? inflate(r, 32) : null;
  }

  // ---- dialog ----

  open() {
    const app = this.app;
    const frames = [...app.doc.all()].filter((el) => el.type === 'frame').sort(zCompare);
    const selFrame = app.selected().find((el) => el.type === 'frame');
    const v = app.renderer.viewRect();
    const here = topFrameAt(app.doc, ...center(v));
    const frameSel = h('select', { 'aria-label': 'Frame to export' }, frames.map((f) => h('option', { value: f.id, text: f.text || 'Frame' })));
    if (selFrame || here) frameSel.value = (selFrame || here).id;
    const fmt = radios('Format', [radio('fmt', 'png', 'PNG image', true), radio('fmt', 'zip', 'Data file (.zip)', false)]);
    const range = radios('Range', [
      radio('range', 'all', 'Whole board', !app.sel.size && !selFrame),
      frames.length ? radio('range', 'frame', 'Frame: ', !!selFrame && app.sel.size === 1, frameSel) : null,
      app.sel.size ? radio('range', 'sel', `${plural(app.sel.size, 'selected element')}`, !selFrame || app.sel.size > 1) : null,
    ]);
    const scale = radios('Scale', [radio('scale', '1', '1x', false), radio('scale', '2', '2x', true), radio('scale', '4', '4x', false)]);
    const size = h('p', { class: 'note num' });
    const capNote = h('p', { class: 'note' });
    const dataNote = h('p', { class: 'note num' });
    const intro = h('p', { class: 'note' });
    const go = h('button', { type: 'button', class: 'btn primary' });
    const val = (name) => { const i = fmtForm.querySelector(`input[name=${name}]:checked`); return i ? i.value : null; };
    const fmtForm = h('form', { class: 'export-form', onsubmit: (e) => e.preventDefault() }, intro, fmt, range, scale, size, capNote, dataNote);
    if (!range.querySelector('input:checked')) range.querySelector('input').checked = true;
    const cap = (tablet() ? 16 : 64) * MP;
    const refresh = () => {
      const isPng = val('fmt') === 'png';
      range.hidden = !isPng;
      scale.hidden = !isPng;
      size.hidden = !isPng;
      capNote.hidden = !isPng;
      dataNote.hidden = isPng;
      intro.textContent = isPng
        ? 'The PNG holds only elements. Cursors, selections and name tags are left out.'
        : 'A data file can be imported into another Flats board. It always holds the whole board, without scale.';
      go.replaceChildren(icon('download'), h('span', { text: isPng ? 'Download PNG' : 'Download data file' }));
      if (isPng) {
        const els = this.rangeElements(val('range'), frameSel.value);
        const b = this.bounds(els);
        const s = Number(val('scale'));
        if (!b) { size.textContent = 'Nothing to export.'; go.disabled = true; return; }
        const w = Math.ceil(b.w * s), hh = Math.ceil(b.h * s);
        size.textContent = `Result ${w.toLocaleString('en-US')} × ${hh.toLocaleString('en-US')} px (${Math.round(b.w - 64)} × ${Math.round(b.h - 64)} plus a 32px margin, at ${s}x)`;
        const over = w * hh > cap;
        go.disabled = over;
        let fit = s;
        while (fit > 1 && Math.ceil(b.w * fit) * Math.ceil(b.h * fit) > cap) fit /= 2;
        capNote.textContent = over
          ? `This range is ${Math.round((w * hh) / MP)} megapixels at ${s}x, over the ${cap / MP} megapixel limit on this device. ${fit >= 1 && fit < s ? `Choose ${fit}x or lower.` : 'Choose a smaller range.'}`
          : `Results over ${cap / MP} megapixels are not possible on this device.`;
        capNote.classList.toggle('refusal', over);
      } else {
        const all = [...app.doc.all()];
        const imgs = new Set(all.filter((el) => el.type === 'image').map((el) => el.asset)).size;
        dataNote.textContent = `${plural(all.length, 'element')} · ${plural(imgs, 'image')}. Authors and times are included; History is not.`;
        go.disabled = false;
      }
    };
    fmtForm.addEventListener('change', refresh);
    go.addEventListener('click', async () => {
      if (val('fmt') === 'zip') { this.downloadData(); dlg.close(); return; }
      go.disabled = true;
      try {
        await this.png(this.rangeElements(val('range'), frameSel.value), Number(val('scale')));
        dlg.close();
      } catch (err) {
        toast(`Could not export: ${err.message}`, { error: true });
        go.disabled = false;
      }
    });
    const acts = h('div', { class: 'acts split' },
      app.canEdit ? h('button', { type: 'button', class: 'btn', onclick: () => { dlg.close(); this.importDialog(); } }, icon('upload'), h('span', { text: 'Import…' })) : null,
      h('span', { class: 'spacer' }),
      h('button', { type: 'button', class: 'btn ghost', text: 'Cancel', onclick: () => dlg.close() }), go);
    const dlg = dialog({ title: 'Export', body: fmtForm, actions: acts });
    if (app.isPublic) fmt.hidden = true;
    refresh();
  }

  async png(els, scale) {
    const app = this.app;
    const b = this.bounds(els);
    const canvas = document.createElement('canvas');
    canvas.width = Math.ceil(b.w * scale);
    canvas.height = Math.ceil(b.h * scale);
    const hashes = [...new Set(els.filter((el) => el.type === 'image').map((el) => el.asset))];
    const decoded = new Map();
    await Promise.all(hashes.map(async (hash) => {
      try { decoded.set(hash, await app.renderer.images.full(hash)); } catch { decoded.set(hash, null); }
    }));
    const painter = new Painter({
      theme: app.renderer.theme, get: (id) => app.doc.get(id), showAuthors: false,
      images: { get: (hash) => decoded.get(hash) || null },
    });
    const ctx = canvas.getContext('2d');
    ctx.fillStyle = painter.theme.canvas;
    ctx.fillRect(0, 0, canvas.width, canvas.height);
    ctx.setTransform(scale, 0, 0, scale, -b.x * scale, -b.y * scale);
    for (const el of [...els].sort(zCompare)) painter.draw(ctx, el, scale);
    const blob = await new Promise((resolve, reject) => canvas.toBlob((bl) => (bl ? resolve(bl) : reject(new Error('the image is too large for this browser'))), 'image/png'));
    download(blob, `${slug(app.doc.meta().title)}.png`);
  }

  downloadData() {
    const a = h('a', { href: '/_board/export.zip', download: `${slug(this.app.doc.meta().title)}.zip`, class: 'sr' });
    document.body.appendChild(a);
    a.click();
    a.remove();
  }

  // ---- recovery files (9.3) ----

  // prepareRecovery builds the recovery file now, while the images it needs
  // can still be fetched; editing has stopped, so it no longer changes.
  prepareRecovery() {
    this.prepared = this.buildRecovery();
    return this.prepared;
  }

  async buildRecovery() {
    const app = this.app;
    const rec = buildRecovery(app.state);
    if (!rec.elements.length && !rec.deleted.length) return null;
    const fetchAsset = async (hash) => {
      if (app.uploads.has(hash)) return app.uploads.get(hash);
      try {
        const r = await fetch('/_board/a/' + hash);
        return r.ok ? new Uint8Array(await r.arrayBuffer()) : null;
      } catch { return null; }
    };
    const now = new Date();
    const bytes = await recoveryZip(rec, app.state.viewMeta(), fetchAsset, now);
    const hm = now.toTimeString().slice(0, 5).replace(':', '');
    return { blob: new Blob([bytes], { type: 'application/zip' }), name: `board-unsent-${slug(app.state.viewMeta().title)}-${hm}.zip` };
  }

  async downloadRecovery() {
    const file = await (this.app.stopped && this.prepared ? this.prepared : this.buildRecovery());
    if (!file) { toast('There are no unsent edits.'); return; }
    download(file.blob, file.name);
  }

  // ---- import (9.2, 9.3) ----

  importDialog() {
    const input = h('input', { type: 'file', accept: '.zip,application/zip', class: 'sr', id: 'import-file' });
    input.addEventListener('change', async () => {
      const f = input.files[0];
      if (f) this.chosen(f);
    });
    document.body.appendChild(input);
    input.click();
    setTimeout(() => input.remove(), 60000);
  }

  async chosen(file) {
    let recovery = null, elements = null;
    try {
      const bytes = new Uint8Array(await file.arrayBuffer());
      const index = readZipIndex(bytes);
      if (index.has('recovery.json')) {
        recovery = JSON.parse(new TextDecoder().decode(await readZipEntry(bytes, index.get('recovery.json'))));
        if (index.has('elements.json')) elements = JSON.parse(new TextDecoder().decode(await readZipEntry(bytes, index.get('elements.json'))));
      }
    } catch {
      toast(`${file.name} is not a Flats board data file.`, { error: true });
      return;
    }
    if (recovery) this.recoveryDialog(file, recovery, elements || []);
    else this.modeDialog(file);
  }

  fileRow(file, detail) {
    return h('div', { class: 'file-row' }, icon('board'), h('span', { class: 'grow', text: `${file.name} · ${Math.max(1, Math.round(file.size / 1024))} KB · ${detail}` }),
      h('button', { type: 'button', class: 'btn ghost', text: 'Choose another file', onclick: (e) => { e.target.closest('dialog').close(); this.importDialog(); } }));
  }

  modeDialog(file) {
    const modes = h('fieldset', {}, h('legend', { text: 'How to import' }), h('div', { class: 'stack' },
      h('label', { class: 'opt on top' }, h('input', { type: 'radio', name: 'mode', value: 'add', checked: true }),
        h('span', {}, h('strong', { text: 'Add' }), h('br'), h('span', { class: 'note', text: 'Creates new elements to the right of the current board. Existing elements stay as they are.' }))),
      h('label', { class: 'opt top' }, h('input', { type: 'radio', name: 'mode', value: 'replace' }),
        h('span', {}, h('strong', { text: 'Replace everything' }), h('br'), h('span', { class: 'note', text: 'Replaces the current board with this file. The board just before is kept as a revision, and everyone\'s undo history is cleared.' })))));
    modes.addEventListener('change', () => { for (const l of modes.querySelectorAll('.opt')) l.classList.toggle('on', l.querySelector('input').checked); });
    const go = h('button', { type: 'button', class: 'btn primary' }, icon('upload'), h('span', { text: 'Import' }));
    go.addEventListener('click', () => {
      const mode = modes.querySelector('input:checked').value;
      dlg.close();
      this.upload(file, mode);
    });
    const dlg = dialog({
      title: 'Import', describe: 'Imports a Flats board data file.',
      body: [this.fileRow(file, 'board data file'), modes],
      actions: h('div', { class: 'acts' }, h('span', { class: 'spacer' }), h('button', { type: 'button', class: 'btn ghost', text: 'Cancel', onclick: () => dlg.close() }), go),
    });
  }

  recoveryDialog(file, rec, elements) {
    const changed = elements.filter((e) => e.recovered_from).length;
    const created = elements.length - changed;
    const when = rec.created_at ? new Date(rec.created_at) : null;
    const stamp = when ? when.toLocaleString('en-US', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false }) : '';
    const items = [
      h('li', { text: `Puts ${plural(created, 'new element')} and ${plural(changed, 'changed element')} into a new "Recovered edits${stamp ? ` (${stamp})` : ''}" frame.` }),
      h('li', { text: 'Frames in the file stay at the top level, without nesting.' }),
    ];
    if (rec.deleted && rec.deleted.length) items.push(h('li', { text: `${plural(rec.deleted.length, 'deleted element')} ${rec.deleted.length === 1 ? 'is' : 'are'} not deleted again; the file only lists ${rec.deleted.length === 1 ? 'it' : 'them'}.` }));
    if (rec.missing_assets && rec.missing_assets.length) items.push(h('li', { text: `${plural(rec.missing_assets.length, 'image')} could not be recovered and ${rec.missing_assets.length === 1 ? 'becomes a text' : 'become texts'} reading "Could not recover image: …".` }));
    const go = h('button', { type: 'button', class: 'btn primary' }, icon('upload'), h('span', { text: 'Import as additions' }));
    go.addEventListener('click', () => { dlg.close(); this.upload(file, 'add'); });
    const dlg = dialog({
      title: 'Import unsent edits',
      describe: 'This is a recovery file. It never changes the current board; it only adds new elements.',
      body: [this.fileRow(file, `${plural(elements.length, 'unsent edit')}${stamp ? ' · ' + stamp : ''}`), h('ul', { class: 'impact' }, items)],
      actions: h('div', { class: 'acts' }, h('span', { class: 'spacer' }), h('button', { type: 'button', class: 'btn ghost', text: 'Cancel', onclick: () => dlg.close() }), go),
    });
  }

  async upload(file, mode) {
    const app = this.app;
    const t = toast(`Importing ${file.name}…`, { timeout: 0 });
    try {
      const r = await boardAPI(`/_board/api/import?mode=${mode}`, { method: 'POST', body: file, raw: true });
      t.remove();
      const ids = r.ids;
      const imgs = r.assets ? r.assets.length : 0;
      const what = `${plural(ids.length, 'element')}${imgs ? ` and ${plural(imgs, 'image')}` : ''}`;
      toast(mode === 'replace' ? `Replaced the board with ${what}.` : `Added ${what}.`, {
        actions: ids.length ? [['Show', () => { app.select(ids); app.fitSelection(); }]] : [],
      });
    } catch (err) {
      t.remove();
      toast(`Could not import: ${err.message} The board did not change.`, { error: true, timeout: 0 });
    }
  }
}
