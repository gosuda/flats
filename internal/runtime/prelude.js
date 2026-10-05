// Flats server prelude: evaluated once per QuickJS runtime before the flat's
// module. Provides fetch, Response, Headers, Request, URL, URLSearchParams,
// btoa/atob, console, the env object and the dispatch entry points the Go
// side calls with JSON strings.
(function () {
  "use strict";
  const host = globalThis.__flats_host;
  delete globalThis.__flats_host;
  const docsCodec = globalThis.__flats_docs_codec;
  delete globalThis.__flats_docs_codec;
  Object.defineProperty(globalThis, "__flats_docsCodec", {value: Object.freeze({
    encode(bytes) {
      if (!(bytes instanceof Uint8Array) || bytes.byteLength > 1536 * 1024) throw new Error("docs codec binary limit");
      const buffer = bytes.byteOffset === 0 && bytes.byteLength === bytes.buffer.byteLength
        ? bytes.buffer : bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength);
      return docsCodec("encode", buffer);
    },
    decode(s) { return new Uint8Array(docsCodec("decode", JSON.stringify(s))); },
    digest(s) { return docsCodec("digest", JSON.stringify(s)); },
    length(s) { return docsCodec("length", JSON.stringify(s)); },
    textEncoder: Object.freeze({
      encode(s) { return new Uint8Array(docsCodec("textEncode", JSON.stringify(s))); },
      encodeInto(s, destination) {
        const bytes = this.encode(s);
        if (bytes.length > destination.length) throw new Error("docs codec destination too small");
        destination.set(bytes);
        return {read: s.length, written: bytes.length};
      },
    }),
    textDecoder: Object.freeze({
      decode(bytes) {
        if (!(bytes instanceof Uint8Array) || bytes.byteLength > 1536 * 1024) throw new Error("docs codec binary limit");
        return JSON.parse(docsCodec("textDecode", bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength)));
      },
    }),
  })});
  const call = (op, args) => {
    const r = host(op, JSON.stringify(args));
    return r === "" ? undefined : JSON.parse(r);
  };

  // --- console ---
  const fmt = (a) => {
    if (typeof a === "string") return a;
    if (a instanceof Error) return a.stack ? `${a.name}: ${a.message}\n${a.stack}` : String(a);
    if (typeof a === "function" || typeof a === "symbol" || typeof a === "bigint") return String(a);
    try {
      const s = JSON.stringify(a);
      return s === undefined ? String(a) : s;
    } catch (_) {
      return String(a);
    }
  };
  const logger = (level) => (...args) => {
    call("log", [level, args.map(fmt).join(" ")]);
  };
  globalThis.console = Object.freeze({
    log: logger("info"), info: logger("info"), debug: logger("info"),
    warn: logger("warn"), error: logger("error"), trace: logger("info"),
  });

  // --- crypto (host randomness from crypto/rand) ---
  const randomBytes = (n) => {
    const hex = call("crypto.random", [n]) || "";
    const out = new Uint8Array(n);
    for (let i = 0; i < n; i++) out[i] = parseInt(hex.substr(i * 2, 2), 16);
    return out;
  };
  globalThis.crypto = Object.freeze({
    getRandomValues(arr) {
      if (!ArrayBuffer.isView(arr) || arr instanceof Float32Array || arr instanceof Float64Array) {
        throw new TypeError("crypto.getRandomValues expects an integer typed array");
      }
      const bytes = randomBytes(arr.byteLength);
      new Uint8Array(arr.buffer, arr.byteOffset, arr.byteLength).set(bytes);
      return arr;
    },
    randomUUID() {
      const b = randomBytes(16);
      b[6] = (b[6] & 0x0f) | 0x40;
      b[8] = (b[8] & 0x3f) | 0x80;
      const h = Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
      return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
    },
  });

  // --- base64 ---
  const B64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
  const bytesToB64 = (u8) => {
    let out = "";
    let i = 0;
    for (; i + 2 < u8.length; i += 3) {
      const n = (u8[i] << 16) | (u8[i + 1] << 8) | u8[i + 2];
      out += B64[n >> 18] + B64[(n >> 12) & 63] + B64[(n >> 6) & 63] + B64[n & 63];
    }
    if (i < u8.length) {
      const n = (u8[i] << 16) | ((i + 1 < u8.length ? u8[i + 1] : 0) << 8);
      out += B64[n >> 18] + B64[(n >> 12) & 63] + (i + 1 < u8.length ? B64[(n >> 6) & 63] : "=") + "=";
    }
    return out;
  };
  if (typeof globalThis.btoa !== "function") {
    globalThis.btoa = (s) => {
      s = String(s);
      const u8 = new Uint8Array(s.length);
      for (let i = 0; i < s.length; i++) {
        const c = s.charCodeAt(i);
        if (c > 255) throw new Error("btoa: character out of range");
        u8[i] = c;
      }
      return bytesToB64(u8);
    };
  }
  if (typeof globalThis.atob !== "function") {
    globalThis.atob = (s) => {
      s = String(s).replace(/[\s=]/g, "");
      let out = "";
      let buf = 0, bits = 0;
      for (const ch of s) {
        const v = B64.indexOf(ch);
        if (v < 0) throw new Error("atob: invalid character");
        buf = (buf << 6) | v;
        bits += 6;
        if (bits >= 8) {
          bits -= 8;
          out += String.fromCharCode((buf >> bits) & 255);
        }
      }
      return out;
    };
  }

  // --- Headers ---
  const norm = (n) => String(n).toLowerCase();
  class Headers {
    #m = new Map();
    constructor(init) {
      if (init == null) return;
      if (init instanceof Headers) {
        const raw = init.__raw();
        for (const k of Object.keys(raw)) for (const v of raw[k]) this.append(k, v);
      } else if (Array.isArray(init) || typeof init[Symbol.iterator] === "function") {
        for (const [k, v] of init) this.append(k, v);
      } else if (typeof init === "object") {
        for (const k of Object.keys(init)) {
          const v = init[k];
          if (Array.isArray(v)) for (const x of v) this.append(k, x);
          else if (v !== undefined) this.append(k, v);
        }
      }
    }
    append(k, v) {
      k = norm(k);
      const cur = this.#m.get(k);
      if (cur) cur.push(String(v));
      else this.#m.set(k, [String(v)]);
    }
    set(k, v) { this.#m.set(norm(k), [String(v)]); }
    get(k) {
      const v = this.#m.get(norm(k));
      return v ? v.join(", ") : null;
    }
    getSetCookie() { return [...(this.#m.get("set-cookie") || [])]; }
    has(k) { return this.#m.has(norm(k)); }
    delete(k) { this.#m.delete(norm(k)); }
    forEach(cb, thisArg) {
      for (const [k, v] of [...this.#m].sort()) cb.call(thisArg, v.join(", "), k, this);
    }
    *entries() { for (const [k, v] of [...this.#m].sort()) yield [k, v.join(", ")]; }
    *keys() { for (const [k] of this.entries()) yield k; }
    *values() { for (const [, v] of this.entries()) yield v; }
    [Symbol.iterator]() { return this.entries(); }
    __raw() {
      const o = {};
      for (const [k, v] of this.#m) o[k] = v;
      return o;
    }
  }
  globalThis.Headers = Headers;

  // --- URLSearchParams / URL (minimal) ---
  const dec = (s) => {
    try { return decodeURIComponent(s.replace(/\+/g, " ")); } catch (_) { return s; }
  };
  const enc = (s) => encodeURIComponent(s).replace(/%20/g, "+");
  if (typeof globalThis.URLSearchParams !== "function") {
    class URLSearchParams {
      #l = [];
      constructor(init) {
        if (init == null || init === "") return;
        if (typeof init === "string") {
          for (const part of init.replace(/^\?/, "").split("&")) {
            if (!part) continue;
            const i = part.indexOf("=");
            this.#l.push(i < 0 ? [dec(part), ""] : [dec(part.slice(0, i)), dec(part.slice(i + 1))]);
          }
        } else if (Array.isArray(init) || init instanceof URLSearchParams) {
          for (const [k, v] of init) this.#l.push([String(k), String(v)]);
        } else if (typeof init === "object") {
          for (const k of Object.keys(init)) this.#l.push([k, String(init[k])]);
        }
      }
      get(k) { const e = this.#l.find((x) => x[0] === k); return e ? e[1] : null; }
      getAll(k) { return this.#l.filter((x) => x[0] === k).map((x) => x[1]); }
      has(k) { return this.#l.some((x) => x[0] === k); }
      append(k, v) { this.#l.push([String(k), String(v)]); }
      set(k, v) {
        const i = this.#l.findIndex((x) => x[0] === k);
        this.#l = this.#l.filter((x, j) => x[0] !== k || j === i);
        if (i < 0) this.#l.push([String(k), String(v)]);
        else this.#l[this.#l.findIndex((x) => x[0] === k)][1] = String(v);
      }
      delete(k) { this.#l = this.#l.filter((x) => x[0] !== k); }
      forEach(cb, thisArg) { for (const [k, v] of this.#l) cb.call(thisArg, v, k, this); }
      *entries() { for (const e of this.#l) yield [e[0], e[1]]; }
      *keys() { for (const e of this.#l) yield e[0]; }
      *values() { for (const e of this.#l) yield e[1]; }
      [Symbol.iterator]() { return this.entries(); }
      get size() { return this.#l.length; }
      toString() { return this.#l.map(([k, v]) => enc(k) + "=" + enc(v)).join("&"); }
    }
    globalThis.URLSearchParams = URLSearchParams;
  }
  if (typeof globalThis.URL !== "function") {
    const RE = /^([a-zA-Z][a-zA-Z0-9+.-]*:)\/\/(?:[^@/?#]*@)?([^/?#:]*|\[[^\]]*\])(?::(\d*))?([^?#]*)(\?[^#]*)?(#.*)?$/;
    class URL {
      constructor(url, base) {
        url = String(url);
        let m = RE.exec(url);
        if (!m && base !== undefined) {
          const b = new URL(base);
          if (url.startsWith("//")) m = RE.exec(b.protocol + url);
          else if (url.startsWith("/")) m = RE.exec(b.origin + url);
          else if (url.startsWith("?")) m = RE.exec(b.origin + b.pathname + url);
          else if (url.startsWith("#")) m = RE.exec(b.origin + b.pathname + b.search + url);
          else m = RE.exec(b.origin + b.pathname.replace(/[^/]*$/, "") + url);
        }
        if (!m) throw new TypeError("Invalid URL: " + url);
        this.protocol = m[1].toLowerCase();
        this.hostname = m[2].toLowerCase();
        this.port = m[3] || "";
        let p = m[4] || "/";
        const segs = [];
        for (const s of p.split("/").slice(1)) {
          if (s === "..") segs.pop();
          else if (s !== ".") segs.push(s);
        }
        this.pathname = "/" + segs.join("/");
        this.searchParams = new URLSearchParams(m[5] || "");
        this.hash = m[6] && m[6] !== "#" ? m[6] : "";
        this.username = "";
        this.password = "";
      }
      get search() { const s = this.searchParams.toString(); return s ? "?" + s : ""; }
      get host() { return this.hostname + (this.port ? ":" + this.port : ""); }
      get origin() { return this.protocol + "//" + this.host; }
      get href() { return this.origin + this.pathname + this.search + this.hash; }
      toString() { return this.href; }
      toJSON() { return this.href; }
    }
    globalThis.URL = URL;
  }

  // --- body helpers ---
  const isBinary = (b) => b instanceof ArrayBuffer || ArrayBuffer.isView(b);
  const toBytes = (b) => b instanceof ArrayBuffer ? new Uint8Array(b) : new Uint8Array(b.buffer, b.byteOffset, b.byteLength);
  const bodyText = (b) => {
    if (b == null) return "";
    if (typeof b === "string") return b;
    if (isBinary(b)) {
      const u8 = toBytes(b);
      let s = "";
      for (let i = 0; i < u8.length; i++) s += String.fromCharCode(u8[i]);
      try { return decodeURIComponent(escape(s)); } catch (_) { return s; }
    }
    return String(b);
  };

  // Trusted, read-only deployment metadata for apps coordinating overlapping
  // workers. The host generation orders activations, including previews and rollbacks.
  const runtimeGeneration = call("runtime.generation", []);
  const utf8Bytes = (value) => {
    const s = String(value), out = new Uint8Array(s.length * 3);
    let used = 0;
    const put = (...bytes) => { for (const b of bytes) out[used++] = b; };
    for (let i = 0; i < s.length; i++) {
      let cp = s.charCodeAt(i);
      if (cp >= 0xd800 && cp <= 0xdbff) {
        const lo = s.charCodeAt(i + 1);
        if (lo >= 0xdc00 && lo <= 0xdfff) { cp = 0x10000 + ((cp - 0xd800) << 10) + lo - 0xdc00; i++; }
        else cp = 0xfffd;
      } else if (cp >= 0xdc00 && cp <= 0xdfff) cp = 0xfffd;
      if (cp < 0x80) put(cp);
      else if (cp < 0x800) put(0xc0 | cp >> 6, 0x80 | cp & 63);
      else if (cp < 0x10000) put(0xe0 | cp >> 12, 0x80 | cp >> 6 & 63, 0x80 | cp & 63);
      else put(0xf0 | cp >> 18, 0x80 | cp >> 12 & 63, 0x80 | cp >> 6 & 63, 0x80 | cp & 63);
    }
    return out.subarray(0, used);
  };
  const bufferedBytes = (b) => b == null ? new Uint8Array(0) : isBinary(b) ? toBytes(b) : utf8Bytes(bodyText(b));
  const decodeB64 = (s) => new Uint8Array(docsCodec("fetch.decode", JSON.stringify(s || "")));
  // UTF-8 decoding for buffered HTTP bodies, replacing malformed input.
  const utf8Text = (bytes) => {
    let out = "";
    for (let i = 0; i < bytes.length;) {
      const a = bytes[i];
      if (a < 0x80) { out += String.fromCharCode(a); i++; continue; }
      const n = a >= 0xc2 && a <= 0xdf ? 2 : a >= 0xe0 && a <= 0xef ? 3 : a >= 0xf0 && a <= 0xf4 ? 4 : 0;
      let cp = n === 2 ? a & 31 : n === 3 ? a & 15 : a & 7;
      let valid = n > 0 && i + n <= bytes.length;
      for (let j = 1; valid && j < n; j++) {
        const b = bytes[i + j];
        valid = b >= 0x80 && b <= 0xbf;
        cp = cp << 6 | b & 63;
      }
      if (!valid || (n === 3 && cp < 0x800) || (n === 4 && cp < 0x10000) || cp > 0x10ffff || (cp >= 0xd800 && cp <= 0xdfff)) {
        out += "\ufffd"; i++; continue;
      }
      out += String.fromCodePoint(cp); i += n;
    }
    return out;
  };
  const requestInit = (init) => {
    if (init == null) return {};
    if (typeof init !== "object") throw new TypeError("fetch options must be an object");
    for (const k of Object.keys(init)) {
      if (!["method", "headers", "body", "redirect"].includes(k)) throw new TypeError("unsupported fetch option");
    }
    return init;
  };

  // --- Request ---
  const headerHelpers = (o) => {
    const h = (k) => norm(k);
    Object.defineProperties(o, {
      get: { value: (k) => (o[h(k)] === undefined ? null : o[h(k)]) },
      has: { value: (k) => o[h(k)] !== undefined },
      forEach: { value: (cb, t) => { for (const k of Object.keys(o)) cb.call(t, o[k], k, o); } },
      entries: { value: () => Object.entries(o)[Symbol.iterator]() },
      keys: { value: () => Object.keys(o)[Symbol.iterator]() },
      [Symbol.iterator]: { value: () => Object.entries(o)[Symbol.iterator]() },
    });
    return o;
  };
  class Request {
    constructor(r, init) {
      Object.defineProperty(this, "runtimeGeneration", { value: runtimeGeneration });
      // Preserve the existing inbound request object's headers and body API.
      if (init === undefined && r && typeof r === "object" && !(r instanceof Request) && !(r instanceof URL) && typeof r.url === "string") {
        this.method = r.method;
        this.url = r.url;
        this.headers = headerHelpers(r.headers || {});
        this.body = r.body ?? null;
        this.redirect = "error";
        return;
      }
      init = requestInit(init);
      const source = r instanceof Request ? r : null;
      this.url = source ? source.url : String(r);
      this.method = String(init.method ?? (source && source.method) ?? "GET").toUpperCase();
      this.headers = new Headers(init.headers ?? (source && source.headers));
      this.body = init.body !== undefined ? init.body : source ? source.body : null;
      this.redirect = init.redirect ?? (source && source.redirect) ?? "error";
      if (this.redirect !== "error" && this.redirect !== "manual") throw new TypeError("fetch supports redirect error or manual only");
      if ((this.method === "GET" || this.method === "HEAD") && this.body != null) throw new TypeError("GET and HEAD requests cannot have a body");
      if (this.body != null && typeof this.body !== "string" && !isBinary(this.body) && !(this.body instanceof URLSearchParams)) throw new TypeError("fetch requires a buffered string, URLSearchParams or binary body");
      if (!this.headers.has("content-type") && typeof this.body === "string") this.headers.set("content-type", "text/plain;charset=UTF-8");
      if (!this.headers.has("content-type") && this.body instanceof URLSearchParams) this.headers.set("content-type", "application/x-www-form-urlencoded;charset=UTF-8");
    }
    text() { return Promise.resolve(isBinary(this.body) ? utf8Text(toBytes(this.body)) : bodyText(this.body)); }
    json() { return this.text().then(JSON.parse); }
    arrayBuffer() { return Promise.resolve(bufferedBytes(this.body).slice().buffer); }
  }
  globalThis.Request = Request;

  // --- Response ---
  class Response {
    constructor(body = null, init = {}) {
      init = init || {};
      this.status = init.status === undefined ? 200 : init.status | 0;
      this.statusText = init.statusText ?? "";
      this.url = "";
      this.redirected = false;
      this.headers = new Headers(init.headers);
      this.body = body;
      if (typeof body === "string" && !this.headers.has("content-type")) {
        this.headers.set("content-type", "text/plain;charset=UTF-8");
      } else if (body instanceof URLSearchParams && !this.headers.has("content-type")) {
        this.headers.set("content-type", "application/x-www-form-urlencoded;charset=UTF-8");
      }
    }
    get ok() { return this.status >= 200 && this.status < 300; }
    text() { return Promise.resolve(isBinary(this.body) ? utf8Text(toBytes(this.body)) : bodyText(this.body)); }
    arrayBuffer() { return Promise.resolve(bufferedBytes(this.body).slice().buffer); }
    json() { return this.text().then(JSON.parse); }
    static json(data, init = {}) {
      const h = new Headers(init && init.headers);
      if (!h.has("content-type")) h.set("content-type", "application/json");
      return new Response(JSON.stringify(data), Object.assign({}, init, { headers: h }));
    }
    static redirect(url, status = 302) {
      return new Response(null, { status, headers: { location: String(url) } });
    }
  }
  globalThis.Response = Response;

  // Host-mediated, buffered HTTP(S). Calls block this VM while the host I/O
  // runs, but return a Promise and respect the enclosing handler's deadline.
  globalThis.fetch = async (input, init) => {
    const req = new Request(input, init === undefined ? {} : init);
    if (req.url.length > 8192 || (typeof req.body === "string" && req.body.length > 1048576) || (isBinary(req.body) && toBytes(req.body).byteLength > 1048576)) throw new TypeError("outbound request limit exceeded");
    const bytes = bufferedBytes(req.body);
    if (bytes.byteLength > 1048576) throw new TypeError("outbound request limit exceeded");
    const result = call("fetch", [{ url: req.url, method: req.method, headers: req.headers.__raw(), body: docsCodec("fetch.encode", bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength)), redirect: req.redirect }]);
    const response = new Response(decodeB64(result.body), { status: result.status, headers: result.headers });
    response.url = result.url;
    response.redirected = result.redirected || false;
    return response;
  };

  const serialize = (res) => {
    if (res == null) throw new Error("fetch() returned nothing; return a Response or {status, headers, body}");
    let status = 200, headers, body;
    if (res instanceof Response) {
      status = res.status; headers = res.headers; body = res.body;
    } else if (typeof res === "string") {
      headers = new Headers({ "content-type": "text/plain;charset=UTF-8" }); body = res;
    } else if (typeof res === "object") {
      status = res.status === undefined ? 200 : res.status | 0;
      headers = new Headers(res.headers);
      body = res.body;
      if (body != null && typeof body === "object" && !isBinary(body) && !(body instanceof URLSearchParams)) {
        body = JSON.stringify(body);
        if (!headers.has("content-type")) headers.set("content-type", "application/json");
      }
    } else {
      throw new Error("fetch() returned a " + typeof res + "; return a Response or {status, headers, body}");
    }
    const out = { status, headers: headers.__raw() };
    if (body == null) out.body = "";
    else if (isBinary(body)) { out.body = bytesToB64(toBytes(body)); out.b64 = true; }
    else out.body = String(body);
    return out;
  };

  const errInfo = (e) => {
    if (e instanceof Error) return { name: e.name, message: String(e.message), stack: String(e.stack || "") };
    return { name: "Error", message: "uncaught: " + fmt(e), stack: "" };
  };

  // --- env ---
  const DB = Object.freeze({
    query(sql, ...params) {
      if (params.length === 1 && Array.isArray(params[0])) params = params[0];
      return call("db.query", [String(sql), params]);
    },
    exec(sql, ...params) {
      if (params.length === 1 && Array.isArray(params[0])) params = params[0];
      return call("db.exec", [String(sql), params]);
    },
  });
  const FILES = Object.freeze({
    get(key) { return call("files.get", [String(key)]); },
    put(key, data) { call("files.put", [String(key), bodyText(data)]); },
    delete(key) { return call("files.delete", [String(key)]); },
    list(prefix = "") { return call("files.list", [String(prefix)]); },
  });
  const env = Object.freeze(Object.assign({}, call("env", []), { DB, FILES }));

  // --- dispatch ---
  let mod = null;
  globalThis.__flats_setModule = (m) => { mod = m; };
  globalThis.__flats_info = () => JSON.stringify({
    fetch: !!mod && typeof mod.fetch === "function",
    websocket: !!mod && !!mod.websocket && typeof mod.websocket === "object",
  });
  globalThis.__flats_dispatch = async (s) => {
    try {
      const req = new Request(JSON.parse(s));
      if (!mod || typeof mod.fetch !== "function") {
        throw new Error("the module has no default export with fetch(request, env)");
      }
      const res = await mod.fetch(req, env);
      return JSON.stringify(serialize(res));
    } catch (e) {
      return JSON.stringify({ __error: errInfo(e) });
    }
  };

  // --- WebSocket ---
  const sockets = new Map();
  class FlatsWebSocket {
    constructor(id, url, headers) {
      Object.defineProperty(this, "runtimeGeneration", { value: runtimeGeneration });
      this.id = id;
      this.url = url;
      this.headers = headerHelpers(headers || {});
      this.readyState = 1;
    }
    send(data) {
      if (this.readyState !== 1) throw new Error("WebSocket is not open");
      call("ws.send", [this.id, bodyText(data)]);
    }
    // Optional byte budgets include queued and currently writing frames.
    // Repeated calls may only lower either budget; existing apps opt out.
    setSendLimits(connectionBytes, flatBytes) {
      call("ws.limits", [this.id, connectionBytes, flatBytes]);
    }
    close(code = 1000, reason = "") {
      if (this.readyState >= 2) return;
      this.readyState = 2;
      call("ws.close", [this.id, code | 0, String(reason)]);
    }
  }
  globalThis.__flats_ws = async (s) => {
    const ev = JSON.parse(s);
    try {
      const h = mod && mod.websocket;
      let ws = sockets.get(ev.id);
      if (ev.type === "open") {
        ws = new FlatsWebSocket(ev.id, ev.url, ev.headers);
        sockets.set(ev.id, ws);
        if (h && typeof h.open === "function") await h.open(ws, env);
      } else if (ev.type === "message") {
        if (ws && h && typeof h.message === "function") await h.message(ws, ev.data, env);
      } else if (ev.type === "close") {
        sockets.delete(ev.id);
        if (ws) {
          ws.readyState = 3;
          ws.closeCode = ev.code;
          ws.closeReason = ev.reason;
          if (h && typeof h.close === "function") await h.close(ws, env);
        }
      }
      return "";
    } catch (e) {
      return JSON.stringify({ __error: errInfo(e) });
    }
  };
})();
