// Image tool (I), paste and drop: upload through POST /_board/api/assets,
// then add an image element (rule 19, TECH 5.5, 7). The tab keeps the
// uploaded bytes until a batch that references them is acknowledged, for
// recovery files (9.3).

import { h, toast } from '../ui.js';
import * as ops from '../ops.js';

const ALLOWED = ['image/png', 'image/jpeg', 'image/gif', 'image/webp'];

export class ImageTool {
  constructor(app) { this.app = app; }

  activate() {
    const app = this.app;
    const input = h('input', { type: 'file', accept: ALLOWED.join(','), multiple: true, class: 'sr' });
    input.addEventListener('change', () => {
      const files = [...input.files];
      input.remove();
      if (files.length) uploadImages(app, files, app.pasteAt());
    });
    document.body.appendChild(input);
    input.click();
    setTimeout(() => app.setTool('select'));
  }

  down() {}
  move() {}
  up() {}
}

function upload(app, file, at) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    const [sx, sy] = app.toScreen(at[0], at[1]);
    const label = h('span', { class: 'note', text: `Uploading ${file.name} 0%` });
    const bar = h('span', { class: 'bar', role: 'progressbar', 'aria-valuemin': '0', 'aria-valuemax': '100', 'aria-valuenow': '0', 'aria-label': `Uploading ${file.name}` }, h('span'));
    const card = h('div', { class: 'upload-card' }, label, bar,
      h('button', { type: 'button', class: 'btn ghost', text: 'Cancel upload', onclick: () => xhr.abort() }));
    card.style.left = sx - 150 + 'px';
    card.style.top = sy - 50 + 'px';
    document.querySelector('.board-ui').appendChild(card);
    xhr.upload.onprogress = (e) => {
      if (!e.lengthComputable) return;
      const pct = Math.round((e.loaded / e.total) * 100);
      label.textContent = `Uploading ${file.name} ${pct}%`;
      bar.setAttribute('aria-valuenow', String(pct));
      bar.firstChild.style.width = pct + '%';
    };
    xhr.onload = () => {
      card.remove();
      let body = null;
      try { body = JSON.parse(xhr.responseText); } catch { body = null; }
      if (xhr.status >= 200 && xhr.status < 300 && body && body.hash) resolve(body);
      else reject(Object.assign(new Error((body && (body.message || body.error)) || ''), { status: xhr.status }));
    };
    xhr.onerror = () => { card.remove(); reject(new Error('the connection failed')); };
    xhr.onabort = () => { card.remove(); reject(Object.assign(new Error('cancelled'), { cancelled: true })); };
    xhr.open('POST', '/_board/api/assets');
    xhr.setRequestHeader('X-Flats-Board', '1');
    xhr.setRequestHeader('Content-Type', file.type || 'application/octet-stream');
    xhr.send(file);
  });
}

function refusal(file, err) {
  if (err.status === 413) return `${file.name} is too large to upload.`;
  if (err.message) return `${file.name} was not uploaded: ${err.message}`;
  return `${file.name} could not be read. The file may be truncated or damaged.`;
}

export async function uploadImages(app, files, at) {
  let i = 0;
  for (const file of files) {
    const p = [at[0] + i * 24, at[1] + i * 24];
    i++;
    if (!ALLOWED.includes(file.type)) {
      toast(`${file.name} was not uploaded. Only PNG, JPEG, GIF and still WebP images can be uploaded.`, { error: true });
      continue;
    }
    let r;
    try {
      r = await upload(app, file, p);
    } catch (err) {
      if (!err.cancelled) toast(refusal(file, err), { error: true, actions: [['Upload again', () => uploadImages(app, [file], at)]] });
      continue;
    }
    app.uploads.set(r.hash, new Uint8Array(await file.arrayBuffer()));
    const w = r.w || 320, hh = r.h || 240;
    const k = Math.min(1, 480 / Math.max(w, hh));
    const el = ops.newElement(app.state, 'image', { x: p[0] - (w * k) / 2, y: p[1] - (hh * k) / 2, w: w * k, h: hh * k, rot: 0 },
      { asset: r.hash, alt: '', natural: [w, hh] });
    if (app.commit([ops.addOp(app.state, el)])) app.select([el.id]);
  }
}
