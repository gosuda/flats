// Flats server runtime API v1: TypeScript declarations.
//
// The JavaScript server-flat runtime (QuickJS on WebAssembly) provides exactly
// the globals and bindings declared here. These declarations are the
// authoritative contract; where prose and this file disagree, this file wins.
// Host tests check them against the real runtime.
//
// Use as an ambient script without the DOM library. Its Request, Response,
// Headers, URL and fetch are smaller than the browser ones:
//   tsconfig: "lib": ["ES2022"], "types": [], with this file in "include".
// Upload compiled JavaScript ES modules, never TypeScript source.
// All environment bindings are synchronous; none of them returns a Promise.
//
// Not in v1 and absent at runtime (`typeof name === "undefined"`):
// @absent TextEncoder TextDecoder structuredClone Blob File FormData ReadableStream WritableStream TransformStream AbortController AbortSignal Event EventTarget WebAssembly WebSocket XMLHttpRequest EventSource require process Buffer module exports window document self setImmediate
// crypto.subtle is absent too.
//
// Engine extras that exist at runtime but have no Flats contract (do not use):
// @unsupported os std bjson print scriptArgs gc performance navigator DOMException QJS_PROXY_VALUE
// Built-in app internals outside v1: runtimeGeneration, ws.setSendLimits,
// __flats_docsCodec and other __flats_* names.

declare namespace Flats {
  /** The default export of a server flat's entry module. */
  interface ServerModule<E extends Env = Env> {
    /**
     * Handles one HTTP request. May be sync or async. Runs in one of four
     * pooled request VMs (by default) within a 10-second deadline; module
     * globals are neither durable nor shared by every request.
     */
    fetch(request: IncomingRequest, env: E): HandlerResult | Promise<HandlerResult>;
    /** Optional WebSocket callbacks; they run serially in a separate VM. */
    websocket?: WebSocketHandlers<E>;
  }

  /**
   * What a handler may return. A string is a 200 text/plain;charset=UTF-8
   * body. A plain object is serialized like a Response.
   */
  type HandlerResult = Response | string | PlainResponse;

  /** A plain-object response. */
  interface PlainResponse {
    /** Defaults to 200; coerced with `| 0`, and 0 becomes 200. Final statuses must be 200–599. */
    status?: number;
    /** Arrays give repeated values, e.g. several Set-Cookie headers. */
    headers?: HeadersInit;
    /**
     * Objects and arrays (other than binary or URLSearchParams) are
     * JSON-serialized and default to application/json. Binary bodies keep
     * their bytes; null or undefined is an empty body.
     */
    body?: BodyInit | object | number | boolean | null;
  }

  /** Buffered body types. Binary bodies are ArrayBuffer or typed-array views. */
  type BodyInit = string | ArrayBuffer | ArrayBufferView | URLSearchParams;

  /** Object values that are arrays give repeated values; undefined values are skipped. */
  type HeadersInit =
    | Headers
    | Iterable<readonly [string, string]>
    | Record<string, string | readonly string[] | undefined>;

  /**
   * The request passed to `fetch(request, env)`. It is a Request instance, but
   * its headers and body differ from an outbound Request.
   */
  interface IncomingRequest {
    /** The method as the client sent it (normally uppercase). */
    readonly method: string;
    /** Absolute URL, including the scheme and host the visitor used. */
    readonly url: string;
    readonly headers: IncomingHeaders;
    /**
     * The buffered body as text, or null when there is none. At most 10 MiB
     * (larger requests get 413). Not a stream and not lossless for binary.
     */
    readonly body: string | null;
    readonly redirect: "error";
    /** The body, or "" when there is none. */
    text(): Promise<string>;
    /** JSON.parse of the body; rejects for invalid or empty JSON. */
    json(): Promise<unknown>;
    /** UTF-8 encoding of the buffered text, not the original request bytes. */
    arrayBuffer(): Promise<ArrayBuffer>;
  }

  /**
   * Incoming request and WebSocket headers. Header names are lowercase own
   * properties; duplicate values are joined with ", " (Cookie with "; ").
   * Read them with get(); there is no append/set/delete/values.
   */
  interface IncomingHeaders {
    /** The value, or null when absent. Names are case-insensitive. */
    get(name: string): string | null;
    has(name: string): boolean;
    forEach(callback: (value: string, name: string, headers: IncomingHeaders) => void, thisArg?: unknown): void;
    /** [lowercase name, value] pairs in arrival order. */
    entries(): IterableIterator<[string, string]>;
    keys(): IterableIterator<string>;
    [Symbol.iterator](): IterableIterator<[string, string]>;
  }

  /**
   * The handler's `env`: a frozen object. DB and FILES are host bindings. Every
   * other property is an ordinary environment variable or a secret, as a string,
   * captured when the worker starts. Missing names are undefined.
   * Declare your names: `Flats.Env<"API_KEY" | "MODE">`.
   */
  type Env<Names extends string = never> = {
    readonly DB: Database;
    readonly FILES: Files;
  } & { readonly [K in Names]?: string };

  /** A SQLite value read back from `query`. */
  type SqlValue = string | number | null;

  /**
   * A query parameter. Booleans become 1/0. Objects and arrays become JSON
   * text (keys sorted; `<`, `>`, `&`, U+2028 and U+2029 escaped). NaN and
   * Infinity become NULL; BigInt and cyclic objects throw. Not a BLOB.
   */
  type SqlParam = string | number | boolean | null | object;

  /** One result row, keyed by column name. Alias duplicate column names. */
  type Row = Record<string, SqlValue>;

  /** The result of `exec`, from SQLite RowsAffected and LastInsertId. */
  interface ExecResult {
    changes: number;
    /** Not a meaningful new ID for every statement. */
    last_insert_id: number;
  }

  /**
   * `env.DB`: the flat's own SQLite database. Synchronous; throws on SQL,
   * parameter, storage or limit errors. One connection per VM; a transaction
   * left open at the end of an invocation is rolled back. WAL, foreign keys
   * and a 5-second busy timeout are on. ATTACH, VACUUM INTO and
   * directory-changing pragmas are blocked.
   */
  interface Database {
    /**
     * Runs a statement and returns its rows; no rows is []. Pass positional `?`
     * parameters as separate arguments or as one array (a single array argument
     * is the parameter list, so nest an array to bind one as JSON text).
     *
     * Values: NULL is null; integers and reals are numbers (beyond 2^53 they lose
     * precision); text is a string; BLOBs come back as text through UTF-8.
     * Values of columns declared DATE, DATETIME or TIMESTAMP that parse as times
     * come back reformatted as RFC 3339 strings (e.g. "2024-01-02T03:04:05Z").
     * The serialized result is capped at 16 MiB (add LIMIT); a value or row at
     * 32 MiB. `R` is an unchecked convenience type.
     */
    query<R = Row>(sql: string, params: readonly SqlParam[]): R[];
    query<R = Row>(sql: string, ...params: SqlParam[]): R[];
    /** Runs a statement and returns {changes, last_insert_id}. Same parameters as query. */
    exec(sql: string, params: readonly SqlParam[]): ExecResult;
    exec(sql: string, ...params: SqlParam[]): ExecResult;
  }

  /**
   * `env.FILES`: the flat's local-disk string key/value store. Synchronous;
   * errors (invalid key, size or quota, disk, file/directory conflicts) throw.
   *
   * Keys for get/put/delete: nonempty UTF-8, at most 512 bytes, relative
   * slash-separated paths; no leading or trailing slash, empty, "." or ".."
   * segments, backslash or control characters; no segment starting
   * ".flats-tmp-". Key identity follows the host filesystem (case and Unicode
   * normalization may be folded). Values are text, at most 10 MiB each; the
   * flat's total is 1 GiB. Calls are not transactions.
   */
  interface Files {
    /** The stored text, or null for an absent key, absent store or directory key. */
    get(key: string): string | null;
    /**
     * Replaces the whole value atomically, creating parent directories.
     * Store text: other values are coerced (null/undefined to "", objects to
     * "[object Object]"; binary is decoded as UTF-8, not stored as bytes).
     * Encode binary as base64 yourself.
     */
    put(key: string, value: string): void;
    /** true when a file was removed; false when absent or not a regular file. */
    delete(key: string): boolean;
    /**
     * Sorted full keys that start with the literal prefix (default ""). Regular
     * files only; at most 10,000 keys, cut off in walk order before sorting,
     * with no cursor or truncation flag. Prefix: at most 512 bytes, no
     * backslash or NUL.
     */
    list(prefix?: string): string[];
  }

  /** Optional WebSocket callbacks. Each may be async. */
  interface WebSocketHandlers<E extends Env = Env> {
    open?(ws: WebSocket, env: E): void | Promise<void>;
    /** Text and binary messages both arrive as strings, at most 1 MiB. */
    message?(ws: WebSocket, data: string, env: E): void | Promise<void>;
    /** ws.closeCode and ws.closeReason are set before this runs. */
    close?(ws: WebSocket, env: E): void | Promise<void>;
  }

  /** An accepted incoming WebSocket. No extensions or subprotocols. */
  interface WebSocket {
    readonly id: number;
    /** Absolute URL of the upgrade request. */
    readonly url: string;
    readonly headers: IncomingHeaders;
    /** 1 open, 2 closing (after close()), 3 closed. */
    readonly readyState: number;
    /**
     * Set before the close callback runs: the close code, 1005 when the peer sent
     * none, 1006 when the connection dropped. Undefined before close.
     */
    readonly closeCode?: number;
    /** Set before the close callback runs; undefined when the reason was empty. */
    readonly closeReason?: string;
    /**
     * Sends a text message; other values are converted to text. Throws when
     * not open. At most 256 queued messages; overflow closes the connection.
     */
    send(data: string): void;
    /** Starts closing; does nothing when already closing or closed. */
    close(code?: number, reason?: string): void;
  }

  /** Outbound fetch options. Any other option (including signal) throws. */
  interface RequestInit {
    /** GET, HEAD, POST, PUT, PATCH, DELETE or OPTIONS (case-insensitive). */
    method?: string;
    headers?: HeadersInit;
    /** At most 1 MiB. GET and HEAD cannot have a body. */
    body?: BodyInit | null;
    /** "error" (default) fails on a redirect; "manual" returns it unfollowed. */
    redirect?: "error" | "manual";
  }

  interface ResponseInit {
    /** Defaults to 200; coerced with `| 0`. */
    status?: number;
    statusText?: string;
    headers?: HeadersInit;
  }

  /** A setTimeout or setInterval handle. */
  type TimerId = number;

  /** Integer typed arrays accepted by crypto.getRandomValues. */
  type IntegerTypedArray =
    | Int8Array | Uint8Array | Uint8ClampedArray | Int16Array | Uint16Array
    | Int32Array | Uint32Array | BigInt64Array | BigUint64Array;

  /** Logs to the flat's runtime log (get_logs). Lines truncate at 8 KiB; 20/s, burst 100. */
  interface Console {
    log(...data: unknown[]): void;
    info(...data: unknown[]): void;
    /** Logged at info level. */
    debug(...data: unknown[]): void;
    /** Logged at info level. */
    trace(...data: unknown[]): void;
    warn(...data: unknown[]): void;
    error(...data: unknown[]): void;
  }

  /** Host randomness only; there is no crypto.subtle. */
  interface Crypto {
    /** Fills an integer typed array (at most 65,536 bytes per call) and returns it. */
    getRandomValues<T extends IntegerTypedArray>(array: T): T;
    /** A random version 4 UUID in lowercase hex. */
    randomUUID(): string;
  }
}

/**
 * A minimal header map. Names are lowercased; get() joins repeated values with
 * ", "; iteration is sorted by name.
 */
declare class Headers {
  constructor(init?: Flats.HeadersInit);
  append(name: string, value: string): void;
  set(name: string, value: string): void;
  get(name: string): string | null;
  /** Every Set-Cookie value, unjoined. */
  getSetCookie(): string[];
  has(name: string): boolean;
  delete(name: string): void;
  forEach(callback: (value: string, name: string, headers: Headers) => void, thisArg?: unknown): void;
  entries(): IterableIterator<[string, string]>;
  keys(): IterableIterator<string>;
  values(): IterableIterator<string>;
  [Symbol.iterator](): IterableIterator<[string, string]>;
}

/**
 * A buffered response. Not a stream: no clone(), blob(), formData() or
 * bodyUsed, and body readers may be called repeatedly. HEAD responses send no
 * body; the decoded response is capped at 32 MiB.
 */
declare class Response {
  /** A string body defaults to text/plain;charset=UTF-8, URLSearchParams to form encoding. */
  constructor(body?: Flats.BodyInit | null, init?: Flats.ResponseInit);
  status: number;
  /** "" unless set; responses from fetch() always have "". */
  statusText: string;
  /** "" unless the response came from fetch(). */
  url: string;
  redirected: boolean;
  headers: Headers;
  /** The body as given; a Uint8Array for responses from fetch(). */
  body: Flats.BodyInit | null;
  /** status is 200–299. */
  readonly ok: boolean;
  /** Binary bodies are decoded as UTF-8 with replacement characters. */
  text(): Promise<string>;
  json(): Promise<unknown>;
  arrayBuffer(): Promise<ArrayBuffer>;
  /** JSON.stringify(data) with content-type application/json unless set. */
  static json(data: unknown, init?: Flats.ResponseInit): Response;
  static redirect(url: string | URL, status?: number): Response;
}

/**
 * An outbound request for fetch(). Passing a Request or the incoming request
 * copies its method, URL, headers and body; init overrides them.
 */
declare class Request {
  constructor(input: string | URL | Request | Flats.IncomingRequest, init?: Flats.RequestInit);
  url: string;
  /** Uppercased. */
  method: string;
  headers: Headers;
  body: Flats.BodyInit | null;
  redirect: "error" | "manual";
  text(): Promise<string>;
  json(): Promise<unknown>;
  arrayBuffer(): Promise<ArrayBuffer>;
}

/**
 * Buffered outbound HTTP(S), denied unless the operator granted the exact
 * origin (ports 80 and 443 only, public addresses only). Only inside a request
 * or WebSocket callback. It blocks this VM while it runs, so Promise.all does
 * not parallelize calls. Limits: 8 KiB URL, 1 MiB request body, 4 MiB response
 * body, 5 seconds per call, 16 calls per invocation. No cookie jar, streams or
 * automatic decompression.
 */
declare function fetch(input: string | URL | Request | Flats.IncomingRequest, init?: Flats.RequestInit): Promise<Response>;

/**
 * A minimal URL parser: lowercases scheme and host, resolves "." and ".."
 * segments, drops credentials, and re-serializes the query through
 * URLSearchParams (spaces as "+"). No IDNA or percent normalization.
 */
declare class URL {
  constructor(url: string | URL, base?: string | URL);
  /** With the trailing colon, e.g. "https:". */
  protocol: string;
  hostname: string;
  /** "" when the URL has no explicit port. */
  port: string;
  pathname: string;
  /** With the leading "#", or "". */
  hash: string;
  /** Always "": credentials are dropped. */
  username: string;
  /** Always "": credentials are dropped. */
  password: string;
  readonly searchParams: URLSearchParams;
  /** With the leading "?", or "". */
  readonly search: string;
  readonly host: string;
  readonly origin: string;
  readonly href: string;
  toString(): string;
  toJSON(): string;
}

declare class URLSearchParams {
  constructor(init?: string | URLSearchParams | readonly (readonly [string, string])[] | Record<string, string>);
  get(name: string): string | null;
  getAll(name: string): string[];
  has(name: string): boolean;
  append(name: string, value: string): void;
  set(name: string, value: string): void;
  delete(name: string): void;
  forEach(callback: (value: string, name: string, params: URLSearchParams) => void, thisArg?: unknown): void;
  entries(): IterableIterator<[string, string]>;
  keys(): IterableIterator<string>;
  values(): IterableIterator<string>;
  [Symbol.iterator](): IterableIterator<[string, string]>;
  readonly size: number;
  toString(): string;
}

/** Base64-encodes a string of characters U+0000–U+00FF; throws otherwise. */
declare function btoa(data: string): string;
/** Decodes base64 to a string of characters U+0000–U+00FF; throws on invalid input. */
declare function atob(data: string): string;

declare var console: Flats.Console;
declare var crypto: Flats.Crypto;

/**
 * Engine timers. They fire only while the current invocation is awaiting and
 * within its deadline; they are not background jobs. Extra arguments are not
 * passed to the callback. Clear timers before returning: one left pending may
 * run during a later invocation on the same VM, or never.
 */
declare function setTimeout(callback: () => void, delay?: number): Flats.TimerId;
declare function clearTimeout(id: Flats.TimerId | undefined): void;
declare function setInterval(callback: () => void, delay?: number): Flats.TimerId;
declare function clearInterval(id: Flats.TimerId | undefined): void;
declare function queueMicrotask(callback: () => void): void;
