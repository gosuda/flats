// Minimal Chrome DevTools Protocol client: launches headless Chrome with a
// temporary profile, speaks CDP over the global WebSocket of Node 22+ and
// drives pages through real input events (Input.dispatch*). No dependencies.

import { spawn } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

export async function launchChrome({ chrome, headless = true, size = [1440, 900] }) {
  const profile = mkdtempSync(join(tmpdir(), 'board-e2e-chrome-'));
  const args = [
    `--user-data-dir=${profile}`, '--remote-debugging-port=0', '--no-first-run', '--no-default-browser-check',
    '--disable-background-networking', '--disable-component-update', '--disable-sync', '--disable-extensions',
    '--disable-default-apps', '--metrics-recording-only', '--password-store=basic', '--use-mock-keychain',
    '--no-proxy-server', '--force-color-profile=srgb', '--hide-scrollbars', '--mute-audio',
    '--disable-renderer-backgrounding', '--disable-background-timer-throttling', '--disable-backgrounding-occluded-windows',
    `--window-size=${size[0]},${size[1]}`,
  ];
  if (headless) args.push('--headless=new');
  args.push('about:blank');
  const proc = spawn(chrome, args, { stdio: ['ignore', 'ignore', 'pipe'] });
  const wsURL = await new Promise((resolve, reject) => {
    let buf = '';
    const t = setTimeout(() => reject(new Error('Chrome did not report a DevTools endpoint in 30s')), 30000);
    proc.stderr.on('data', (d) => {
      buf += d.toString();
      const m = buf.match(/DevTools listening on (ws:\/\/\S+)/);
      if (m) { clearTimeout(t); resolve(m[1]); }
    });
    proc.on('exit', (code) => { clearTimeout(t); reject(new Error(`Chrome exited early (${code}): ${buf.slice(-500)}`)); });
  });
  const cdp = await CDP.connect(wsURL);
  const version = await cdp.send('Browser.getVersion');
  return {
    cdp, proc, profile, version,
    async close() {
      try { await cdp.send('Browser.close'); } catch { /* already gone */ }
      await Promise.race([new Promise((r) => proc.once('exit', r)), sleep(5000)]);
      if (proc.exitCode === null) proc.kill('SIGKILL');
      cdp.close();
      try { rmSync(profile, { recursive: true, force: true }); } catch { /* best effort */ }
    },
  };
}

export class CDP {
  static connect(url) {
    return new Promise((resolve, reject) => {
      const ws = new WebSocket(url);
      ws.onopen = () => resolve(new CDP(ws));
      ws.onerror = (e) => reject(new Error('CDP connection failed: ' + (e.message || 'error')));
    });
  }

  constructor(ws) {
    this.ws = ws;
    this.id = 0;
    this.calls = new Map();
    this.listeners = new Set();
    ws.onmessage = (ev) => {
      const m = JSON.parse(ev.data);
      if (m.id !== undefined) {
        const c = this.calls.get(m.id);
        if (!c) return;
        this.calls.delete(m.id);
        if (m.error) c.reject(new Error(`${c.method}: ${m.error.message}${m.error.data ? ' ' + m.error.data : ''}`));
        else c.resolve(m.result);
        return;
      }
      for (const l of [...this.listeners]) l(m);
    };
    ws.onclose = () => { for (const c of this.calls.values()) c.reject(new Error('CDP socket closed')); this.calls.clear(); };
  }

  send(method, params = {}, sessionId) {
    const id = ++this.id;
    const msg = { id, method, params };
    if (sessionId) msg.sessionId = sessionId;
    return new Promise((resolve, reject) => {
      this.calls.set(id, { resolve, reject, method });
      this.ws.send(JSON.stringify(msg));
    });
  }

  // on registers a listener for one event method (and session); it returns
  // an unsubscribe function.
  on(method, fn, sessionId) {
    const l = (m) => { if (m.method === method && (sessionId === undefined || m.sessionId === sessionId)) fn(m.params, m); };
    this.listeners.add(l);
    return () => this.listeners.delete(l);
  }

  waitEvent(method, { sessionId, pred = () => true, timeout = 15000 } = {}) {
    return new Promise((resolve, reject) => {
      let off = null;
      const t = setTimeout(() => { off(); reject(new Error(`timed out waiting for ${method}`)); }, timeout);
      off = this.on(method, (p) => { if (pred(p)) { clearTimeout(t); off(); resolve(p); } }, sessionId);
    });
  }

  close() { try { this.ws.close(); } catch { /* ignore */ } }
}

// Key definitions for Input.dispatchKeyEvent (US layout).
const SPECIAL = {
  Enter: { code: 'Enter', vk: 13, text: '\r' }, Escape: { code: 'Escape', vk: 27 }, Tab: { code: 'Tab', vk: 9 },
  Delete: { code: 'Delete', vk: 46 }, Backspace: { code: 'Backspace', vk: 8 }, Home: { code: 'Home', vk: 36 }, End: { code: 'End', vk: 35 },
  ArrowLeft: { code: 'ArrowLeft', vk: 37 }, ArrowUp: { code: 'ArrowUp', vk: 38 }, ArrowRight: { code: 'ArrowRight', vk: 39 }, ArrowDown: { code: 'ArrowDown', vk: 40 },
  ' ': { code: 'Space', vk: 32, text: ' ' }, ']': { code: 'BracketRight', vk: 221, text: ']' }, '[': { code: 'BracketLeft', vk: 219, text: '[' },
  '=': { code: 'Equal', vk: 187, text: '=' }, '-': { code: 'Minus', vk: 189, text: '-' }, '!': { code: 'Digit1', vk: 49, text: '!' }, '@': { code: 'Digit2', vk: 50, text: '@' },
};
const MOD = { alt: 1, ctrl: 2, meta: 4, shift: 8 };

function keyDef(key) {
  if (SPECIAL[key]) return { key, ...SPECIAL[key] };
  if (/^[a-zA-Z]$/.test(key)) return { key, code: 'Key' + key.toUpperCase(), vk: key.toUpperCase().charCodeAt(0), text: key };
  if (/^[0-9]$/.test(key)) return { key, code: 'Digit' + key, vk: key.charCodeAt(0), text: key };
  throw new Error('unknown key ' + key);
}

// Tab is one page target attached through a flattened session.
export class Tab {
  constructor(cdp, sessionId, targetId, name) {
    this.cdp = cdp;
    this.sessionId = sessionId;
    this.targetId = targetId;
    this.name = name;
    this.dialogs = [];
    cdp.on('Page.javascriptDialogOpening', (p) => {
      this.dialogs.push(p);
      this.send('Page.handleJavaScriptDialog', { accept: true }).catch(() => {});
    }, sessionId);
  }

  static async open(cdp, { browserContextId, name }) {
    const { targetId } = await cdp.send('Target.createTarget', { url: 'about:blank', browserContextId });
    const { sessionId } = await cdp.send('Target.attachToTarget', { targetId, flatten: true });
    const tab = new Tab(cdp, sessionId, targetId, name);
    await tab.send('Page.enable');
    await tab.send('Runtime.enable');
    await tab.send('Emulation.setFocusEmulationEnabled', { enabled: true });
    await tab.send('Emulation.setDeviceMetricsOverride', { width: 1440, height: 900, deviceScaleFactor: 1, mobile: false });
    await tab.send('Page.setInterceptFileChooserDialog', { enabled: true });
    return tab;
  }

  send(method, params = {}) { return this.cdp.send(method, params, this.sessionId); }
  waitEvent(method, opts = {}) { return this.cdp.waitEvent(method, { ...opts, sessionId: this.sessionId }); }

  async goto(url, { timeout = 20000 } = {}) {
    const loaded = this.waitEvent('Page.loadEventFired', { timeout });
    const r = await this.send('Page.navigate', { url });
    if (r.errorText) throw new Error(`navigation to ${url} failed: ${r.errorText}`);
    await loaded;
  }

  // ev evaluates an expression in the page and returns its JSON value.
  async ev(expression, { timeout = 15000 } = {}) {
    const r = await Promise.race([
      this.send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true, userGesture: true }),
      sleep(timeout).then(() => { throw new Error(`evaluate timed out: ${expression.slice(0, 80)}`); }),
    ]);
    if (r.exceptionDetails) {
      const d = r.exceptionDetails;
      throw new Error(`page error: ${(d.exception && d.exception.description) || d.text} in ${expression.slice(0, 120)}`);
    }
    return r.result.value;
  }

  // call runs a function in the page with JSON arguments.
  call(fn, ...args) { return this.ev(`(${fn.toString()})(...${JSON.stringify(args)})`); }

  // callWith is call with evaluation options such as a longer timeout.
  callWith(opts, fn, ...args) { return this.ev(`(${fn.toString()})(...${JSON.stringify(args)})`, opts); }

  // until polls a page function until it returns a truthy value.
  until(fn, args = [], opts = {}) {
    return this.waitFor(`(${fn.toString()})(...${JSON.stringify(args)})`, { desc: opts.desc || fn.toString().slice(0, 100), ...opts });
  }

  // waitFor polls an expression until it is truthy and returns its value.
  async waitFor(expression, { timeout = 10000, interval = 60, desc } = {}) {
    const end = Date.now() + timeout;
    let last;
    while (Date.now() < end) {
      try {
        last = await this.ev(expression);
        if (last) return last;
      } catch (err) { last = err.message; }
      await sleep(interval);
    }
    throw new Error(`timed out after ${timeout} ms waiting for ${desc || expression.slice(0, 120)} (last: ${JSON.stringify(last)})`);
  }

  // ---- input ----

  mods(m) { return (m || []).reduce((a, k) => a | MOD[k], 0); }

  async mouse(type, x, y, { button = 'left', modifiers, clickCount = 1, buttons } = {}) {
    const b = buttons ?? (type === 'mouseReleased' || type === 'mouseMoved' ? 0 : 1);
    await this.send('Input.dispatchMouseEvent', {
      type, x, y, button: type === 'mouseMoved' && !b ? 'none' : button, buttons: b, clickCount, modifiers: this.mods(modifiers), pointerType: 'mouse',
    });
  }

  async click(x, y, { modifiers, button = 'left', clickCount = 1 } = {}) {
    await this.mouse('mouseMoved', x, y, { modifiers, buttons: 0 });
    await this.mouse('mousePressed', x, y, { modifiers, button, clickCount, buttons: button === 'right' ? 2 : 1 });
    await this.mouse('mouseReleased', x, y, { modifiers, button, clickCount, buttons: 0 });
  }

  async dblclick(x, y, opts = {}) {
    await this.click(x, y, { ...opts, clickCount: 1 });
    await this.mouse('mousePressed', x, y, { ...opts, clickCount: 2, buttons: 1 });
    await this.mouse('mouseReleased', x, y, { ...opts, clickCount: 2, buttons: 0 });
  }

  async drag(from, to, { steps = 12, modifiers, path } = {}) {
    await this.mouse('mouseMoved', from[0], from[1], { modifiers, buttons: 0 });
    await this.mouse('mousePressed', from[0], from[1], { modifiers, buttons: 1 });
    const pts = path || Array.from({ length: steps }, (_, i) => {
      const t = (i + 1) / steps;
      return [from[0] + (to[0] - from[0]) * t, from[1] + (to[1] - from[1]) * t];
    });
    for (const [x, y] of pts) {
      await this.mouse('mouseMoved', x, y, { modifiers, buttons: 1 });
      await sleep(8);
    }
    const last = pts[pts.length - 1];
    await this.mouse('mouseReleased', last[0], last[1], { modifiers, buttons: 0 });
  }

  async key(key, { modifiers } = {}) {
    const d = keyDef(key);
    const m = this.mods(modifiers);
    const withText = d.text && !(m & (MOD.ctrl | MOD.meta | MOD.alt));
    let text = d.text;
    if (withText && m & MOD.shift && /^[a-z]$/.test(text)) text = text.toUpperCase();
    const base = { key: d.key, code: d.code, windowsVirtualKeyCode: d.vk, nativeVirtualKeyCode: d.vk, modifiers: m };
    await this.send('Input.dispatchKeyEvent', withText ? { type: 'keyDown', text, unmodifiedText: text, ...base } : { type: 'rawKeyDown', ...base });
    await this.send('Input.dispatchKeyEvent', { type: 'keyUp', ...base });
  }

  insertText(text) { return this.send('Input.insertText', { text }); }

  // clickSel clicks the center of the first element matching a selector
  // (optionally with exact or partial text) through real mouse events.
  async clickSel(selector, { text, modifiers } = {}) {
    const box = await this.ev(`(() => {
      const els = [...document.querySelectorAll(${JSON.stringify(selector)})];
      const t = ${JSON.stringify(text ?? null)};
      const el = els.find((e) => t == null || (e.textContent || '').includes(t) || (e.getAttribute('aria-label') || '').includes(t));
      if (!el) return null;
      el.scrollIntoView({ block: 'nearest' });
      const r = el.getBoundingClientRect();
      return { x: r.left + r.width / 2, y: r.top + r.height / 2, w: r.width, h: r.height };
    })()`);
    if (!box) throw new Error(`no element ${selector}${text ? ` with text "${text}"` : ''}`);
    if (!box.w || !box.h) throw new Error(`element ${selector} is not visible`);
    await this.click(box.x, box.y, { modifiers });
    return box;
  }

  // chooseFiles answers the next file chooser that `trigger` opens.
  async chooseFiles(files, trigger) {
    const opened = this.waitEvent('Page.fileChooserOpened', { timeout: 10000 });
    await trigger();
    const ev = await opened;
    await this.send('DOM.setFileInputFiles', { files, backendNodeId: ev.backendNodeId });
  }

  async screenshot(file) {
    const r = await this.send('Page.captureScreenshot', { format: 'png' });
    writeFileSync(file, Buffer.from(r.data, 'base64'));
    return file;
  }

  async close() { try { await this.cdp.send('Target.closeTarget', { targetId: this.targetId }); } catch { /* ignore */ } }
}
