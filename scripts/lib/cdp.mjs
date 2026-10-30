// One Chrome DevTools Protocol harness for the board scripts: it launches
// Chrome with a throwaway profile, speaks CDP over the global WebSocket of
// Node 22+, and ends every browser process it started. No dependencies.
'use strict';

import { spawn, spawnSync } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

// NativeWebSocket is captured before any script replaces the global.
const NativeWebSocket = globalThis.WebSocket;

export const DEFAULT_CHROME = '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';

const CHROME_PATHS = [
  process.env.CHROME,
  DEFAULT_CHROME,
  '/usr/bin/google-chrome',
  '/usr/bin/chromium',
  '/usr/bin/chromium-browser',
].filter(Boolean);

// findChrome returns the first Chrome executable that exists on this host,
// or null when Chrome is missing.
export function findChrome() {
  return CHROME_PATHS.find((p) => existsSync(p)) || null;
}

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function signal(pid, sig) {
  try { process.kill(pid, sig); } catch { /* gone */ }
}

// signalGroup signals Chrome's process group; the group exists only while
// one of its processes runs.
function signalGroup(pid, sig) {
  if (pid > 0) signal(-pid, sig);
}

function killAll(list) {
  for (const p of list || []) signal(p.pid, 'SIGKILL');
}

// ownedProcesses picks from a `ps -o pid=,pgid=,command=` listing this
// browser's processes: those in its process group, and those of the Chrome
// executable itself (on macOS, of its .app bundle, where the helpers live)
// that have exactly this profile as a whole --user-data-dir argument. The
// profile directory is new for each launch. Another profile, even one whose
// path starts with this one, another program, and an argument that only
// contains the flag are never picked.
export function ownedProcesses(listing, chrome, profile, group) {
  const app = chrome.indexOf('.app/');
  const isChrome = (cmd) => (app >= 0 ? cmd.startsWith(chrome.slice(0, app + 5)) : cmd === chrome || cmd.startsWith(`${chrome} `));
  const flag = `--user-data-dir=${profile}`;
  const hasFlag = (cmd) => {
    for (let i = cmd.indexOf(flag); i >= 0; i = cmd.indexOf(flag, i + 1)) {
      const before = i === 0 ? ' ' : cmd[i - 1], after = cmd[i + flag.length];
      if (/\s/.test(before) && (after === undefined || /\s/.test(after))) return true;
    }
    return false;
  };
  const out = [];
  for (const line of listing.split('\n')) {
    const m = line.match(/^\s*(\d+)\s+(\d+)\s+(.*)$/);
    if (!m) continue;
    const [pid, pgid, cmd] = [Number(m[1]), Number(m[2]), m[3]];
    if ((group > 0 && pgid === group) || (isChrome(cmd) && hasFlag(cmd))) out.push({ pid, command: cmd.slice(0, 120) });
  }
  return out;
}

function profileProcesses(chrome, profile, group) {
  const r = spawnSync('ps', ['-axww', '-o', 'pid=,pgid=,command='], { encoding: 'utf8' });
  if (r.status !== 0 || r.error) return null;
  return ownedProcesses(r.stdout, chrome, profile, group).filter((p) => p.pid !== process.pid);
}

// launched holds the browsers this process started, so any exit — a crash,
// a signal or a normal exit — ends them and removes their profiles.
const launched = new Set();

// stop ends one launched browser's process group and any of its processes
// that still run (helpers such as the network service can outlive the
// browser process), removes the profile and forgets the browser.
function stop(b) {
  signalGroup(b.proc.pid, 'SIGKILL');
  killAll(b.owned());
  launched.delete(b);
  releaseSignals();
  try { rmSync(b.profile, { recursive: true, force: true }); } catch { /* best effort */ }
}

function stopAll() {
  for (const b of [...launched]) stop(b);
}

const onSignal = (sig) => { stopAll(); process.exit(sig === 'SIGINT' ? 130 : 143); };

function watchSignals() {
  process.once('SIGINT', onSignal);
  process.once('SIGTERM', onSignal);
}

function releaseSignals() {
  if (launched.size) return;
  process.removeListener('SIGINT', onSignal);
  process.removeListener('SIGTERM', onSignal);
}

process.on('exit', stopAll);

// CDP speaks the Chrome DevTools Protocol over one WebSocket: send resolves
// one call; on, waitEvent and waitFor watch events, optionally of one
// flattened session; pending calls fail when the socket or their tab closes.
export class CDP {
  static connect(url) {
    return new Promise((resolve, reject) => {
      const ws = new NativeWebSocket(url);
      ws.onopen = () => resolve(new CDP(ws));
      ws.onerror = (e) => reject(new Error(`CDP connection failed: ${e.message || 'error'}`));
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
        if (m.error) c.reject(new Error(`${c.method}: ${m.error.message}${m.error.data ? ` ${m.error.data}` : ''}`));
        else c.resolve(m.result);
        return;
      }
      // A closed tab never answers its calls.
      if (m.method === 'Target.detachedFromTarget') {
        for (const [id, c] of this.calls) {
          if (c.sessionId === m.params.sessionId) {
            this.calls.delete(id);
            c.reject(new Error(`${c.method}: the tab was closed`));
          }
        }
      }
      for (const l of [...this.listeners]) l(m);
    };
    ws.onclose = () => {
      for (const c of this.calls.values()) c.reject(new Error(`${c.method}: DevTools connection closed`));
      this.calls.clear();
    };
  }

  send(method, params = {}, sessionId) {
    const id = ++this.id;
    const msg = { id, method, params };
    if (sessionId) msg.sessionId = sessionId;
    return new Promise((resolve, reject) => {
      // A closed socket drops sends without an error, so the call would
      // never settle.
      if (this.ws.readyState !== NativeWebSocket.OPEN) { reject(new Error(`${method}: DevTools connection closed`)); return; }
      this.calls.set(id, { resolve, reject, method, sessionId });
      this.ws.send(JSON.stringify(msg));
    });
  }

  // on registers a listener and returns an unsubscribe function: on(fn)
  // sees every event message, on(method, fn, sessionId) sees the params of
  // one method, optionally only of one session.
  on(methodOrFn, fn, sessionId) {
    const l = typeof methodOrFn === 'function'
      ? methodOrFn
      : (m) => { if (m.method === methodOrFn && (sessionId === undefined || m.sessionId === sessionId)) fn(m.params, m); };
    this.listeners.add(l);
    return () => this.listeners.delete(l);
  }

  // waitEvent resolves with the params of the next event matching pred.
  waitEvent(method, { sessionId, pred = () => true, timeout = 15000 } = {}) {
    return new Promise((resolve, reject) => {
      let off = null;
      const t = setTimeout(() => { off(); reject(new Error(`timed out waiting for ${method}`)); }, timeout);
      off = this.on(method, (p) => { if (pred(p)) { clearTimeout(t); off(); resolve(p); } }, sessionId);
    });
  }

  // waitFor resolves with the first event message matching pred.
  waitFor(pred, timeout = 60000) {
    return new Promise((resolve, reject) => {
      let off = null;
      const t = setTimeout(() => { off(); reject(new Error('CDP wait timed out')); }, timeout);
      off = this.on((m) => { if (pred(m)) { clearTimeout(t); off(); resolve(m); } });
    });
  }

  close() { try { this.ws.close(); } catch { /* closed */ } }
}

// launchChrome starts Chrome with its own temporary profile and process
// group and returns {cdp, proc, profile, version, leftovers, close}.
// Options: chrome (executable; default findChrome()), headless (default
// true), width, height (window size), scale (device scale factor; 0 keeps
// the display's).
export async function launchChrome(opt = {}) {
  const chrome = opt.chrome || findChrome() || DEFAULT_CHROME;
  const profile = mkdtempSync(join(tmpdir(), 'flats-chrome-'));
  const argv = [
    `--user-data-dir=${profile}`, '--remote-debugging-port=0', '--no-first-run', '--no-default-browser-check',
    '--disable-background-networking', '--disable-component-update', '--disable-sync', '--disable-extensions',
    '--disable-default-apps', '--metrics-recording-only', '--password-store=basic', '--use-mock-keychain',
    '--no-proxy-server', '--force-color-profile=srgb', '--hide-scrollbars', '--mute-audio',
    '--disable-renderer-backgrounding', '--disable-background-timer-throttling', '--disable-backgrounding-occluded-windows',
    '--disable-features=Translate,MediaRouter,OptimizationHints',
    `--window-size=${opt.width || 1440},${opt.height || 900}`, '--window-position=40,40',
  ];
  if (opt.headless !== false) argv.push('--headless=new');
  if (opt.scale) argv.push(`--force-device-scale-factor=${opt.scale}`);
  argv.push('about:blank');
  // Chrome runs in a process group of its own (detached), so close() can end
  // it with every helper it started, and an interrupt of this script does
  // not reach it directly; the signal handlers installed below end it.
  const proc = spawn(chrome, argv, { stdio: ['ignore', 'ignore', 'pipe'], detached: true });
  const owned = () => profileProcesses(chrome, profile, proc.pid);
  const b = { proc, profile, owned };
  launched.add(b);
  watchSignals();
  let stderr = '';
  proc.stderr.on('data', (d) => { stderr = (stderr + d).slice(-4000); });

  try {
    const wsURL = await new Promise((resolve, reject) => {
      let done = false, timer, poll;
      const finish = (fn, v) => {
        if (done) return;
        done = true;
        clearTimeout(timer);
        clearInterval(poll);
        fn(v);
      };
      timer = setTimeout(() => finish(reject, new Error(`Chrome did not report a DevTools endpoint in 30s: ${stderr.slice(-400)}`)), 30000);
      // Chrome writes the endpoint to stderr and, once the socket is up, the
      // port and browser target path to DevToolsActivePort.
      poll = setInterval(() => {
        const m = stderr.match(/DevTools listening on (ws:\/\/\S+)/);
        if (m) { finish(resolve, m[1]); return; }
        try {
          const [port, path] = readFileSync(join(profile, 'DevToolsActivePort'), 'utf8').trim().split('\n');
          if (port) finish(resolve, `ws://127.0.0.1:${port}${path || ''}`);
        } catch { /* not yet */ }
      }, 100);
      proc.on('exit', (code) => finish(reject, new Error(`Chrome exited early (${code}): ${stderr.slice(-500)}`)));
      proc.on('error', (err) => finish(reject, new Error(`Chrome could not start: ${err.message}`)));
    });
    const cdp = await CDP.connect(wsURL);
    const version = await cdp.send('Browser.getVersion');
    return {
      cdp, proc, profile, version,
      // leftovers lists this browser's processes that still run, or returns
      // null when the process list cannot be read.
      leftovers: owned,
      // close asks Chrome to quit, then ends its process group and any of
      // its processes still running, and removes the profile. It returns the
      // processes that had to be ended, or null when the process list cannot
      // be read.
      async close() {
        // Chrome may close the socket before it answers Browser.close.
        await Promise.race([cdp.send('Browser.close').catch(() => { /* already gone */ }), sleep(5000)]);
        await Promise.race([new Promise((r) => (proc.exitCode !== null || proc.signalCode !== null ? r() : proc.once('exit', r))), sleep(5000)]);
        cdp.close();
        const killed = owned();
        signalGroup(proc.pid, 'SIGTERM');
        for (const p of killed || []) signal(p.pid, 'SIGTERM');
        const end = Date.now() + 5000;
        while (owned()?.length && Date.now() < end) await sleep(100);
        stop(b);
        return killed;
      },
    };
  } catch (err) {
    stop(b);
    throw err;
  }
}
