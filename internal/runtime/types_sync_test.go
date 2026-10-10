package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	runtimeref "github.com/gosuda/flats/docs"
	"github.com/gosuda/flats/internal/egress"
)

// declarations is what flats-runtime-v1.d.ts declares, parsed just enough to
// compare it with the real runtime.
type declarations struct {
	members     map[string][]string // interface, class or object type -> members; statics as "static name"
	globals     []string            // declare function/var/class names
	absent      []string            // "@absent" comment
	unsupported []string            // "@unsupported" comment
}

var (
	dtsBlockStart = regexp.MustCompile(`^\s*(?:declare\s+)?(?:interface|class|type)\s+(\w+)`)
	dtsMember     = regexp.MustCompile(`^\s*(static\s+)?(?:readonly\s+)?(\[Symbol\.iterator\]|[A-Za-z_$][\w$]*)\??\s*(?:<[^>]*>)?\s*[(:]`)
	dtsGlobal     = regexp.MustCompile(`^declare\s+(?:function|var|const|let|class)\s+(\w+)`)
)

// parseDeclarations reads the declaration file line by line. It relies on the
// file's layout: one member per line and no braces inside comments or strings.
func parseDeclarations(t *testing.T, src string) declarations {
	t.Helper()
	d := declarations{members: map[string][]string{}}
	type block struct {
		name  string
		depth int
	}
	var stack []block
	depth, parens, inDoc := 0, 0, false
	for _, line := range strings.Split(src, "\n") {
		trim := strings.TrimSpace(line)
		if inDoc {
			inDoc = !strings.Contains(trim, "*/")
			continue
		}
		if strings.HasPrefix(trim, "/*") {
			inDoc = !strings.Contains(trim, "*/")
			continue
		}
		if names, ok := strings.CutPrefix(trim, "// @absent "); ok {
			d.absent = append(d.absent, strings.Fields(names)...)
		}
		if names, ok := strings.CutPrefix(trim, "// @unsupported "); ok {
			d.unsupported = append(d.unsupported, strings.Fields(names)...)
		}
		if strings.HasPrefix(trim, "//") {
			continue
		}
		if m := dtsGlobal.FindStringSubmatch(line); m != nil {
			d.globals = append(d.globals, m[1])
		}
		if n := len(stack); n > 0 && depth == stack[n-1].depth+1 && parens == 0 {
			if m := dtsMember.FindStringSubmatch(line); m != nil && m[2] != "constructor" {
				name := m[2]
				if m[1] != "" {
					name = "static " + name
				}
				if !slices.Contains(d.members[stack[n-1].name], name) {
					d.members[stack[n-1].name] = append(d.members[stack[n-1].name], name)
				}
			}
		}
		start := dtsBlockStart.FindStringSubmatch(line)
		for _, c := range line {
			switch c {
			case '(':
				parens++
			case ')':
				parens--
			case '{':
				if start != nil {
					stack = append(stack, block{start[1], depth})
					start = nil
				}
				depth++
			case '}':
				depth--
				if n := len(stack); n > 0 && stack[n-1].depth == depth {
					stack = stack[:n-1]
				}
			}
		}
	}
	if depth != 0 || parens != 0 || inDoc || len(stack) != 0 {
		t.Fatalf("declaration parse ended unbalanced: depth=%d parens=%d doc=%v", depth, parens, inDoc)
	}
	return d
}

// ECMAScript and QuickJS language globals: they come from the engine and are
// typed by the ES lib, not by the Flats declarations.
var languageGlobals = []string{
	"AggregateError", "Array", "ArrayBuffer", "BigInt", "BigInt64Array", "BigUint64Array", "Boolean",
	"DataView", "Date", "Error", "EvalError", "FinalizationRegistry", "Float16Array", "Float32Array",
	"Float64Array", "Function", "Infinity", "Int16Array", "Int32Array", "Int8Array",
	"InternalError", // QuickJS: out of memory, stack overflow
	"Iterator", "JSON", "Map", "Math", "NaN", "Number", "Object", "Promise", "Proxy", "RangeError",
	"ReferenceError", "Reflect", "RegExp", "Set", "SharedArrayBuffer", "String", "Symbol", "SyntaxError",
	"TypeError", "URIError", "Uint16Array", "Uint32Array", "Uint8Array", "Uint8ClampedArray", "WeakMap",
	"WeakRef", "WeakSet", "decodeURI", "decodeURIComponent", "encodeURI", "encodeURIComponent", "escape",
	"eval", "globalThis", "isFinite", "isNaN", "parseFloat", "parseInt", "undefined", "unescape",
}

// Declared types that describe values the flat supplies (or option bags),
// so the runtime has no object to list members from. They are checked by
// behavior below instead.
var shapeOnlyTypes = []string{"ServerModule", "PlainResponse", "WebSocketHandlers", "RequestInit", "ResponseInit"}

// typesProbe lists the members of every runtime object that a declared
// interface or class describes, and checks the declared behavior of the
// synchronous bindings. Built-in internals outside v1 are excluded by name.
const typesProbe = `
const names = (o, skip = []) => Object.getOwnPropertyNames(o).filter(n => n !== "constructor" && !skip.includes(n));
const iter = (o) => Object.getOwnPropertySymbols(o).includes(Symbol.iterator) ? ["[Symbol.iterator]"] : [];
const members = (o, skip = []) => {
  const p = Object.getPrototypeOf(o);
  return [...names(o, skip), ...iter(o), ...(p === Object.prototype ? [] : [...names(p, skip), ...iter(p)])];
};
const statics = (C) => names(C, ["length", "name", "prototype"]).map(n => "static " + n);
const thenable = (v) => v !== null && typeof v === "object" && typeof v.then === "function";
export default { async fetch(request, env) {
  if (new URL(request.url).pathname === "/plain") return {status: 202, headers: {"x-a": ["1", "2"]}, body: {ok: true}};
  const hidden = (o) => Object.getOwnPropertyNames(o).filter(n => !Object.getOwnPropertyDescriptor(o, n).enumerable);
  const out = {members: {
    Database: names(env.DB), Files: names(env.FILES),
    Env: names(env).filter(n => n === "DB" || n === "FILES"),
    ExecResult: Object.keys(env.DB.exec("SELECT 1")),
    IncomingRequest: members(request, ["runtimeGeneration"]),
    IncomingHeaderHelpers: [...hidden(request.headers), ...iter(request.headers)],
    Headers: members(new Headers(), ["__raw"]),
    Request: members(new Request("https://a.example/"), ["runtimeGeneration"]),
    Response: [...members(new Response()), ...statics(Response)],
    URL: members(new URL("https://a.example/")),
    URLSearchParams: members(new URLSearchParams()),
    Console: names(console), Crypto: names(crypto),
  }, globals: {}, checks: {}};
  for (const n of Object.getOwnPropertyNames(globalThis)) out.globals[n] = typeof globalThis[n];
  const c = out.checks;
  c.envFrozen = Object.isFrozen(env);
  c.envStrings = [env.API_KEY, env.EMPTY, typeof env.MISSING];
  c.subtle = typeof crypto.subtle;

  const created = env.DB.exec("CREATE TABLE t (n INTEGER, s TEXT, d DATETIME, j TEXT)");
  const inserted = env.DB.exec("INSERT INTO t VALUES (?, ?, ?, ?)", [true, "x", "2024-01-02 03:04:05", {b: 1, a: 2}]);
  const spread = env.DB.exec("INSERT INTO t VALUES (?, ?, ?, ?)", 2, null, "not a time", [1]);
  const rows = env.DB.query("SELECT n, s, d, j FROM t ORDER BY rowid");
  c.db = {syncResults: ![created, inserted, spread, rows].some(thenable), rows,
    exec: inserted, array: Array.isArray(rows), none: env.DB.query("SELECT 1 WHERE 0"),
    valueTypes: [...new Set(rows.flatMap(r => Object.values(r).map(v => v === null ? "null" : typeof v)))].sort()};

  const get0 = env.FILES.get("a/b"), put = env.FILES.put("a/b", "v"), get1 = env.FILES.get("a/b");
  const dir = env.FILES.get("a"), list = env.FILES.list(), listA = env.FILES.list("a/");
  const del1 = env.FILES.delete("a/b"), del2 = env.FILES.delete("a/b");
  c.files = {sync: ![get0, put, get1, dir, list, del1, del2].some(thenable),
    results: [get0, put === undefined, get1, dir, list, listA, del1, del2]};

  c.response = await (async () => {
    const r = new Response(null, {status: 201, statusText: "made", headers: {a: ["1", "2"]}});
    const j = Response.json({x: 1}), rd = Response.redirect("https://a.example/x");
    return [r.status, r.statusText, r.headers.get("a"), r.ok, r.url, r.redirected, r.body,
      j.headers.get("content-type"), await j.json(), rd.status, rd.headers.get("location")];
  })();
  c.requestInit = ["method", "headers", "body", "redirect"].map(k => {
    try { new Request("https://a.example/", {method: "POST", [k]: {method: "PUT", headers: {}, body: "x", redirect: "manual"}[k]}); return k; } catch (e) { return "throws " + k; }
  });
  try { new Request("https://a.example/", {signal: null}); c.requestInit.push("signal accepted"); } catch (_) {}
  c.copied = (() => {
    const r = new Request(request, {headers: {"x-copy": "1"}}), s = new Request(request);
    return [r.method, r.url === request.url, r.body, r.headers.get("x-copy"), s.headers.get("x-probe"), s.body, s instanceof Request];
  })();
  c.incoming = [request.method, request.url, request.body, request.redirect, await request.text(),
    request.headers.get("X-Probe"), request.headers.has("x-probe"), request.headers["x-probe"],
    Object.keys(request.headers).every(k => k === k.toLowerCase() && typeof request.headers[k] === "string")];
  c.url = (() => {
    const u = new URL("../b?q=a b#h", "https://User:pw@A.Example:8080/x/y/z");
    return [u.href, u.protocol, u.hostname, u.port, u.pathname, u.search, u.hash, u.username, u.password, u.origin,
      ["search", "host", "origin", "href"].every(n => Object.getOwnPropertyDescriptor(URL.prototype, n).set === undefined)];
  })();
  c.crypto = (() => {
    const a = new Uint32Array(2);
    let floatThrows = false;
    try { crypto.getRandomValues(new Float64Array(1)); } catch (_) { floatThrows = true; }
    return [crypto.getRandomValues(a) === a, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(crypto.randomUUID()), floatThrows];
  })();
  c.timers = await (async () => {
    let args = null, micro = false;
    const id = setTimeout((...a) => { args = a; }, 0, "extra");
    queueMicrotask(() => { micro = true; });
    await new Promise(r => setTimeout(r, 5));
    const iv = setInterval(() => {}, 1000);
    clearInterval(iv); clearTimeout(undefined);
    return [typeof id, typeof iv, args, micro];
  })();
  const throws = (f) => { try { f(); return false; } catch (_) { return true; } };
  c.base64 = [btoa("\u00ff"), atob("/w=="), throws(() => btoa("\u0100")), atob("Y=Q=="), atob(" YQ "), throws(() => atob("Y!Q"))];
  return Response.json(out);
},
websocket: {
  open(ws) {
    ws.send(JSON.stringify({members: members(ws, ["runtimeGeneration", "setSendLimits"]),
      headers: [...Object.getOwnPropertyNames(ws.headers).filter(n => !Object.getOwnPropertyDescriptor(ws.headers, n).enumerable), ...iter(ws.headers)],
      readyState: ws.readyState, id: typeof ws.id}));
  },
  message(ws, data) { ws.send(typeof data); },
  close(ws) { console.log("close-probe " + JSON.stringify([typeof ws.closeCode, ws.closeCode, typeof ws.closeReason, ws.readyState])); },
} };`

// The declarations are the typed contract of runtime API v1: every declared
// member must exist in the real runtime and every runtime member must be
// declared, the synchronous bindings must return the declared shapes, and
// every global must be declared, documented as unsupported or a language global.
func TestTypesMatchRuntime(t *testing.T) {
	d := parseDeclarations(t, runtimeref.Types)
	f := mustStart(t, newManager(t), "types", map[string]string{"index.js": typesProbe}, "index.js",
		map[string]string{"API_KEY": "k", "EMPTY": ""})
	r := f.do(t, "POST", "/x?y=1", "hello", "X-Probe", "p")
	var out struct {
		Members map[string][]string
		Globals map[string]string
		Checks  map[string]json.RawMessage
	}
	if r.status != 200 || json.Unmarshal([]byte(r.body), &out) != nil {
		t.Fatalf("probe: %d %s\nlogs:\n%s", r.status, r.body, f.logs)
	}

	for name, declared := range d.members {
		if slices.Contains(shapeOnlyTypes, name) || name == "WebSocket" {
			continue
		}
		actual, ok := out.Members[name]
		if !ok {
			t.Errorf("declared type %s has no runtime counterpart in the probe; add one or list it as shape-only", name)
			continue
		}
		sameSet(t, name, declared, actual)
	}
	for name := range out.Members {
		if _, ok := d.members[name]; !ok {
			t.Errorf("runtime object %s is not declared", name)
		}
	}
	for _, name := range shapeOnlyTypes {
		if len(d.members[name]) == 0 {
			t.Errorf("shape-only type %s is not declared", name)
		}
	}
	sameSet(t, "ServerModule", d.members["ServerModule"], []string{"fetch", "websocket"})
	sameSet(t, "WebSocketHandlers", d.members["WebSocketHandlers"], []string{"open", "message", "close"})
	sameSet(t, "PlainResponse", d.members["PlainResponse"], []string{"status", "headers", "body"})
	sameSet(t, "ResponseInit", d.members["ResponseInit"], []string{"status", "statusText", "headers"})
	sameSet(t, "RequestInit", d.members["RequestInit"], []string{"method", "headers", "body", "redirect"})

	for _, g := range d.globals {
		if typ := out.Globals[g]; typ != "function" && typ != "object" {
			t.Errorf("declared global %s is %q at runtime", g, typ)
		}
	}
	for _, g := range d.absent {
		if typ, ok := out.Globals[g]; ok {
			t.Errorf("global %s is declared absent but exists (%s)", g, typ)
		}
	}
	for _, g := range d.unsupported {
		if _, ok := out.Globals[g]; !ok {
			t.Errorf("unsupported engine extra %s no longer exists; drop it from the declarations", g)
		}
	}
	for g := range out.Globals {
		if !slices.Contains(d.globals, g) && !slices.Contains(d.unsupported, g) &&
			!slices.Contains(languageGlobals, g) && !strings.HasPrefix(g, "__flats_") {
			t.Errorf("runtime global %s is neither declared, unsupported nor a language global", g)
		}
	}

	want := map[string]string{
		"envFrozen":   `true`,
		"envStrings":  `["k","","undefined"]`,
		"subtle":      `"undefined"`,
		"db":          `{"syncResults":true,"rows":[{"n":1,"s":"x","d":"2024-01-02T03:04:05Z","j":"{\"a\":2,\"b\":1}"},{"n":2,"s":null,"d":"not a time","j":"[1]"}],"exec":{"changes":1,"last_insert_id":1},"array":true,"none":[],"valueTypes":["null","number","string"]}`,
		"files":       `{"sync":true,"results":[null,true,"v",null,["a/b"],["a/b"],true,false]}`,
		"response":    `[201,"made","1, 2",true,"",false,null,"application/json",{"x":1},302,"https://a.example/x"]`,
		"requestInit": `["method","headers","body","redirect"]`,
		"copied":      `["POST",true,"hello","1","p","hello",true]`,
		"incoming":    `["POST","` + f.srv.URL + `/x?y=1","hello","error","hello","p",true,"p",true]`,
		"url":         `["https://a.example:8080/x/b?q=a+b#h","https:","a.example","8080","/x/b","?q=a+b","#h","","","https://a.example:8080",true]`,
		"crypto":      `[true,true,true]`,
		"timers":      `["number","number",[],true]`,
		"base64":      `["/w==","ÿ",true,"a","a",true]`,
	}
	for k, v := range want {
		if got := string(out.Checks[k]); got != v {
			t.Errorf("declared behavior %s:\n got %s\nwant %s", k, got, v)
		}
	}
	if len(out.Checks) != len(want) {
		t.Errorf("probe returned %d checks, the test expects %d", len(out.Checks), len(want))
	}

	// A plain-object result: objects are JSON, header arrays repeat.
	p := f.do(t, "GET", "/plain", "")
	if p.status != 202 || p.body != `{"ok":true}` || p.header.Get("Content-Type") != "application/json" ||
		strings.Join(p.header.Values("X-A"), ",") != "1,2" {
		t.Errorf("PlainResponse: %d %v %s", p.status, p.header, p.body)
	}

	// WebSocket members, message type and close fields.
	addr := strings.TrimPrefix(f.srv.URL, "http://")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, err := dialWS(ctx, func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}, addr, "/ws")
	if err != nil {
		t.Fatalf("dial: %v; logs:\n%s", err, f.logs)
	}
	defer ws.c.Close()
	ws.c.SetDeadline(time.Now().Add(10 * time.Second))
	msg, err := ws.readMessage()
	if err != nil {
		t.Fatal(err)
	}
	var open struct {
		Members, Headers []string
		ReadyState       int
		ID               string
	}
	if err := json.Unmarshal([]byte(msg), &open); err != nil {
		t.Fatal(err)
	}
	// closeCode and closeReason are set only when the connection closes.
	sameSet(t, "WebSocket", d.members["WebSocket"], append(open.Members, "closeCode", "closeReason"))
	sameSet(t, "WebSocket headers", d.members["IncomingHeaderHelpers"], open.Headers)
	if open.ReadyState != 1 || open.ID != "number" {
		t.Errorf("WebSocket at open: %+v", open)
	}
	ws.writeFrame(opBinary, []byte{0x68, 0x69})
	if msg, err := ws.readMessage(); err != nil || msg != "string" {
		t.Errorf("binary message type = %q, %v", msg, err)
	}
	ws.writeFrame(opClose, []byte{0x03, 0xE8}) // 1000, empty reason
	ws.readMessage()
	f.logs.waitFor(t, `close-probe ["number",1000,"undefined",3]`)
}

// Every env.DB/env.FILES member the prose topics mention is declared, and every
// declared member is documented in its topic.
func TestTypesMatchTopics(t *testing.T) {
	d := parseDeclarations(t, runtimeref.Types)
	for _, c := range []struct{ binding, typ, topic string }{
		{"env.DB", "Database", "server.db"},
		{"env.FILES", "Files", "server.files"},
	} {
		re := regexp.MustCompile(regexp.QuoteMeta(c.binding) + `\.(\w+)`)
		for _, name := range runtimeref.TopicOrder {
			md, _ := runtimeref.Topic(name)
			for _, m := range re.FindAllStringSubmatch(md, -1) {
				if !slices.Contains(d.members[c.typ], m[1]) {
					t.Errorf("topic.%s mentions %s.%s, which %s does not declare", name, c.binding, m[1], c.typ)
				}
			}
		}
		md, _ := runtimeref.Topic(c.topic)
		for _, m := range d.members[c.typ] {
			if !strings.Contains(md, c.binding+"."+m) {
				t.Errorf("topic.%s does not document declared %s.%s", c.topic, c.binding, m)
			}
		}
	}
}

func sameSet(t *testing.T, what string, declared, actual []string) {
	t.Helper()
	a, b := slices.Sorted(slices.Values(declared)), slices.Sorted(slices.Values(actual))
	if !slices.Equal(slices.Compact(a), slices.Compact(b)) {
		t.Errorf("%s: declared %v, runtime %v", what, a, b)
	}
}

type recordingFetch struct{ got []egress.Request }

func (f *recordingFetch) Fetch(_ context.Context, req egress.Request) (egress.Response, error) {
	f.got = append(f.got, req)
	return egress.Response{Status: 200, URL: req.URL}, nil
}
func (*recordingFetch) Close() {}

// The declarations accept the incoming request as a fetch input: it is a
// Request instance, so fetch copies its method, URL, headers and body.
func TestTypesIncomingRequestAsFetchInput(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.js": `export default { async fetch(request) {
 const a = await fetch(request), b = await fetch(request, {method: "PUT", headers: {"x-b": "1"}});
 return new Response(String(request instanceof Request) + a.status + b.status);
} };`})
	network := &recordingFetch{}
	w := &worker{spec: workerSpec{Dir: dir, Entry: "index.js", DataDir: t.TempDir(), TimeoutMS: 10000}, network: network, log: newChildLog(io.Discard)}
	vm, err := newJSVM(w, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer vm.close()
	out, err := vm.invoke(t.Context(), "__flats_dispatch",
		`{"method":"POST","url":"https://api.example.test/in?q=1","headers":{"x-a":"v"},"body":"hi"}`)
	var res respPayload
	if err != nil || json.Unmarshal([]byte(out), &res) != nil || res.Error != nil || res.Body != "true200200" {
		t.Fatalf("dispatch: %s %v", out, err)
	}
	if len(network.got) != 2 {
		t.Fatalf("fetch calls: %+v", network.got)
	}
	for i, want := range []struct{ method, header string }{{"POST", "x-a"}, {"PUT", "x-b"}} {
		g := network.got[i]
		if g.URL != "https://api.example.test/in?q=1" || g.Method != want.method || len(g.Headers[want.header]) != 1 || string(g.Body) != "hi" {
			t.Errorf("call %d: %+v", i, g)
		}
	}
}
