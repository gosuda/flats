package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/oesni/flats/internal/core"
)

const testWorkerEnv = "FLATS_TEST_WORKER"

// TestMain doubles as the worker binary: Manager re-executes this test
// binary with FLATS_TEST_WORKER=1 (the only variable in its environment).
func TestMain(m *testing.M) {
	if os.Getenv(testWorkerEnv) == "1" {
		keys := []string{}
		for _, kv := range os.Environ() {
			k, _, _ := strings.Cut(kv, "=")
			keys = append(keys, k)
		}
		sort.Strings(keys)
		// Variable NAMES only, so the sandbox test can check the environment.
		stderrLog.send(childMsg{T: "log", Level: "info", Msg: "test-env-keys: " + strings.Join(keys, ",")})
		if err := WorkerMain(os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "worker:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

var (
	cacheOnce sync.Once
	cacheRoot string
)

func newManager(t *testing.T) *Manager {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A persistent DataDir shares the wazero compilation cache across tests
	// and runs (only the cache and sockets live there).
	cacheOnce.Do(func() {
		cacheRoot = filepath.Join(os.TempDir(), fmt.Sprintf("flats-runtime-test-%d", os.Getuid()))
	})
	return &Manager{DataDir: cacheRoot, Exe: exe, Env: []string{testWorkerEnv + "=1"}, Logf: t.Logf}
}

type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) add(level, msg string) {
	l.mu.Lock()
	l.lines = append(l.lines, level+": "+msg)
	l.mu.Unlock()
}

func (l *logs) find(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func (l *logs) waitFor(t *testing.T, sub string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !l.find(sub) {
		if time.Now().After(deadline) {
			t.Fatalf("log %q not seen; logs:\n%s", sub, l)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type flat struct {
	inst    core.Instance
	srv     *httptest.Server
	logs    *logs
	dataDir string
	root    string
	spec    core.RuntimeSpec
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func startFlat(t *testing.T, m *Manager, name string, files map[string]string, entry string, env map[string]string) (*flat, error) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "versions", "1")
	os.MkdirAll(dir, 0o755)
	writeFiles(t, dir, files)
	f := &flat{logs: &logs{}, dataDir: filepath.Join(root, "data"), root: root}
	f.spec = core.RuntimeSpec{Flat: name, Version: 1, Dir: dir, Entry: entry, DataDir: f.dataDir, Env: env, Log: f.logs.add}
	inst, err := m.Start(context.Background(), f.spec)
	if err != nil {
		return nil, err
	}
	f.inst = inst
	f.srv = httptest.NewServer(inst)
	t.Cleanup(func() {
		f.srv.Close()
		f.inst.Stop()
		if f.logs.find("DATA RACE") || f.logs.find("panic:") {
			t.Errorf("worker reported a race or panic:\n%s", f.logs)
		}
	})
	return f, nil
}

func mustStart(t *testing.T, m *Manager, name string, files map[string]string, entry string, env map[string]string) *flat {
	t.Helper()
	f, err := startFlat(t, m, name, files, entry, env)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return f
}

type result struct {
	status int
	header http.Header
	body   string
}

func (f *flat) do(t *testing.T, method, path, body string, hdr ...string) result {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, f.srv.URL+path, rd)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return result{resp.StatusCode, resp.Header, string(b)}
}

func (f *flat) get(t *testing.T, path string) result { return f.do(t, "GET", path, "") }

func (f *flat) json(t *testing.T, path string, v any) {
	t.Helper()
	r := f.get(t, path)
	if r.status != 200 {
		t.Fatalf("GET %s = %d %q; logs:\n%s", path, r.status, r.body, f.logs)
	}
	if err := json.Unmarshal([]byte(r.body), v); err != nil {
		t.Fatalf("GET %s: %v in %q", path, err, r.body)
	}
}

func TestFetchResponse(t *testing.T) {
	m := newManager(t)
	f := mustStart(t, m, "hello", map[string]string{
		"index.js": `
import { greet } from "./lib/greet.js";
export default {
  async fetch(request, env) {
    const u = new URL(request.url);
    if (u.pathname === "/plain") return { status: 202, headers: { "x-kind": "plain" }, body: "plain body" };
    if (u.pathname === "/json") return Response.json({ q: u.searchParams.get("q"), n: 1 }, { status: 201 });
    if (u.pathname === "/echo") return Response.json({ method: request.method, url: request.url,
        ct: request.headers["content-type"], viaGet: request.headers.get("X-Test"), body: request.body, text: await request.text() });
    if (u.pathname === "/bin") return new Response(new Uint8Array([0, 1, 2, 255]), { headers: { "content-type": "application/octet-stream" } });
    if (u.pathname === "/redirect") return Response.redirect("/plain", 307);
    if (u.pathname === "/throw") throw new Error("boom secret detail");
    const h = new Headers({ "Content-Type": "text/html; charset=utf-8" });
    h.append("Set-Cookie", "a=1"); h.append("Set-Cookie", "b=2");
    return new Response(greet("<b>flats</b>"), { status: 200, headers: h });
  }
}`,
		"lib/greet.js": `export const greet = (s) => "hello " + s;`,
	}, "index.js", nil)

	r := f.get(t, "/")
	if r.status != 200 || r.body != "hello <b>flats</b>" || r.header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("GET / = %d %q %v", r.status, r.body, r.header)
	}
	if c := r.header.Values("Set-Cookie"); !slices.Equal(c, []string{"a=1", "b=2"}) {
		t.Fatalf("Set-Cookie = %v", c)
	}
	r = f.get(t, "/plain")
	if r.status != 202 || r.body != "plain body" || r.header.Get("X-Kind") != "plain" {
		t.Fatalf("plain = %d %q %v", r.status, r.body, r.header)
	}
	r = f.get(t, "/json?q=a%20b")
	if r.status != 201 || r.body != `{"q":"a b","n":1}` || r.header.Get("Content-Type") != "application/json" {
		t.Fatalf("json = %d %q %v", r.status, r.body, r.header)
	}
	r = f.do(t, "POST", "/echo?x=1", `{"hi":true}`, "Content-Type", "application/json", "X-Test", "yes")
	var echo map[string]any
	json.Unmarshal([]byte(r.body), &echo)
	if echo["method"] != "POST" || echo["body"] != `{"hi":true}` || echo["text"] != `{"hi":true}` || echo["ct"] != "application/json" || echo["viaGet"] != "yes" ||
		!strings.HasSuffix(fmt.Sprint(echo["url"]), "/echo?x=1") || !strings.HasPrefix(fmt.Sprint(echo["url"]), "http://127.0.0.1:") {
		t.Fatalf("echo = %d %q", r.status, r.body)
	}
	r = f.get(t, "/bin")
	if r.body != "\x00\x01\x02\xff" {
		t.Fatalf("bin = %q", r.body)
	}
	req, _ := http.NewRequest("GET", f.srv.URL+"/redirect", nil)
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil || resp.StatusCode != 307 || resp.Header.Get("Location") != "/plain" {
		t.Fatalf("redirect = %v %v", resp, err)
	}
	resp.Body.Close()
	r = f.get(t, "/throw")
	if r.status != 500 || strings.Contains(r.body, "boom") {
		t.Fatalf("throw = %d %q (no details may reach the visitor)", r.status, r.body)
	}
	f.logs.waitFor(t, "boom secret detail")
	// Request body cap.
	r = f.do(t, "POST", "/echo", strings.Repeat("x", MaxRequestBody+1))
	if r.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("big body = %d", r.status)
	}
	if r := f.get(t, "/plain"); r.status != 202 {
		t.Fatalf("after big body = %d", r.status)
	}
}

const dbApp = `
export default {
  async fetch(request, env) {
    env.DB.exec("CREATE TABLE IF NOT EXISTS visits (id INTEGER PRIMARY KEY, path TEXT, n REAL)");
    const u = new URL(request.url);
    if (u.pathname === "/add") {
      const r = env.DB.exec("INSERT INTO visits (path, n) VALUES (?, ?)", u.pathname, 1.5);
      return Response.json(r);
    }
    if (u.pathname === "/tx") {
      env.DB.exec("BEGIN");
      env.DB.exec("INSERT INTO visits (path) VALUES ('tx')");
      throw new Error("abandon the transaction");
    }
    if (u.pathname === "/attach") {
      try { env.DB.exec("ATTACH DATABASE ? AS x", u.searchParams.get("p")); return new Response("attached"); }
      catch (e) { return new Response("refused: " + e.message); }
    }
    if (u.pathname === "/vacuum") {
      try { env.DB.exec("VACUUM INTO ?", u.searchParams.get("p")); return new Response("vacuumed"); }
      catch (e) { return new Response("refused: " + e.message); }
    }
    if (u.pathname === "/ext") {
      try { env.DB.query("SELECT load_extension('/usr/lib/libz.dylib')"); return new Response("loaded"); }
      catch (e) { return new Response("refused: " + e.message); }
    }
    return Response.json(env.DB.query("SELECT count(*) AS n, sum(n) AS s FROM visits WHERE path = ?", ["/add"])[0]);
  }
}`

func TestDBPersistsAcrossRequestsAndRestart(t *testing.T) {
	m := newManager(t)
	f := mustStart(t, m, "db", map[string]string{"index.js": dbApp}, "index.js", nil)
	for i := 0; i < 3; i++ {
		var r map[string]float64
		f.json(t, "/add", &r)
		if r["changes"] != 1 || r["last_insert_id"] != float64(i+1) {
			t.Fatalf("exec result = %v", r)
		}
	}
	var count struct{ N, S float64 }
	f.json(t, "/count", &count)
	if count.N != 3 || count.S != 4.5 {
		t.Fatalf("count = %+v", count)
	}
	// An abandoned transaction is rolled back after the request.
	if r := f.get(t, "/tx"); r.status != 500 {
		t.Fatalf("tx = %d", r.status)
	}
	f.json(t, "/count", &count)
	if count.N != 3 {
		t.Fatalf("count after abandoned tx = %+v", count)
	}
	if _, err := os.Stat(filepath.Join(f.dataDir, "db.sqlite")); err != nil {
		t.Fatalf("db file: %v", err)
	}

	// Kill the worker: the next request restarts it and the data is still there.
	in := f.inst.(*instance)
	pid := in.pid()
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	f.logs.waitFor(t, "worker exited unexpectedly")
	f.json(t, "/count", &count)
	if count.N != 3 {
		t.Fatalf("count after restart = %+v", count)
	}
	if in.pid() == pid || in.pid() == 0 {
		t.Fatalf("worker not restarted (pid %d -> %d)", pid, in.pid())
	}
	if !f.logs.find("worker restarted") {
		t.Fatalf("no restart log:\n%s", f.logs)
	}

	// After another crash, concurrent requests share one restart.
	if err := syscall.Kill(in.pid(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for in.pid() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	before := strings.Count(f.logs.String(), "worker restarted")
	var wg sync.WaitGroup
	codes := make(chan int, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(f.srv.URL + "/count")
			if err != nil {
				codes <- 0
				return
			}
			resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != 200 {
			t.Errorf("concurrent request after crash = %d; logs:\n%s", c, f.logs)
		}
	}
	if n := strings.Count(f.logs.String(), "worker restarted") - before; n != 1 {
		t.Errorf("%d restarts for one crash", n)
	}

	// A fresh Start (deploy/restart of flats) sees the same data.
	pid = in.pid()
	f.inst.Stop()
	if _, err := os.Stat(in.sock); !os.IsNotExist(err) {
		t.Fatalf("socket not removed: %v", err)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatal("worker still alive after Stop")
	}
	inst2, err := m.Start(context.Background(), f.spec)
	if err != nil {
		t.Fatal(err)
	}
	defer inst2.Stop()
	rec := httptest.NewRecorder()
	inst2.ServeHTTP(rec, httptest.NewRequest("GET", "/count", nil))
	if !strings.Contains(rec.Body.String(), `"n":3`) {
		t.Fatalf("after new Start: %d %q", rec.Code, rec.Body.String())
	}
}

func TestFiles(t *testing.T) {
	m := newManager(t)
	f := mustStart(t, m, "files", map[string]string{"index.js": `
export default {
  async fetch(request, env) {
    const u = new URL(request.url);
    const k = u.searchParams.get("k");
    const out = (v) => Response.json(v);
    const tryIt = (fn) => { try { return { ok: fn() ?? null }; } catch (e) { return { err: e.message }; } };
    switch (u.pathname) {
      case "/put": return out(tryIt(() => env.FILES.put(k, request.body)));
      case "/get": return out(tryIt(() => env.FILES.get(k)));
      case "/del": return out(tryIt(() => env.FILES.delete(k)));
      case "/list": return out(tryIt(() => env.FILES.list(k || "")));
      case "/big": return out(tryIt(() => env.FILES.put("big", "x".repeat(10 * 1024 * 1024 + 1))));
    }
    return new Response("?", { status: 404 });
  }
}`}, "index.js", nil)
	type res struct {
		OK  any    `json:"ok"`
		Err string `json:"err"`
	}
	call := func(method, path, body string) res {
		t.Helper()
		r := f.do(t, method, path, body)
		var v res
		if err := json.Unmarshal([]byte(r.body), &v); err != nil {
			t.Fatalf("%s: %d %q", path, r.status, r.body)
		}
		return v
	}
	for _, kv := range [][2]string{{"notes/a.txt", "alpha"}, {"notes/b.txt", "beta"}, {"top", "t"}, {"unicode/한글.txt", "ok"}} {
		if v := call("POST", "/put?k="+kv[0], kv[1]); v.Err != "" {
			t.Fatalf("put %s: %s", kv[0], v.Err)
		}
	}
	if v := call("GET", "/get?k=notes/a.txt", ""); v.OK != "alpha" {
		t.Fatalf("get = %+v", v)
	}
	if v := call("GET", "/get?k=missing", ""); v.OK != nil || v.Err != "" {
		t.Fatalf("get missing = %+v", v)
	}
	if v := call("GET", "/list?k=notes/", ""); fmt.Sprint(v.OK) != "[notes/a.txt notes/b.txt]" {
		t.Fatalf("list = %+v", v)
	}
	if v := call("GET", "/list", ""); fmt.Sprint(v.OK) != "[notes/a.txt notes/b.txt top unicode/한글.txt]" {
		t.Fatalf("list all = %+v", v)
	}
	if v := call("GET", "/del?k=notes/a.txt", ""); v.OK != true {
		t.Fatalf("del = %+v", v)
	}
	if v := call("GET", "/get?k=notes/a.txt", ""); v.OK != nil {
		t.Fatalf("get after del = %+v", v)
	}
	// Traversal and malformed keys are rejected.
	secret := filepath.Join(f.root, "outside.txt")
	os.WriteFile(secret, []byte("outside"), 0o644)
	for _, k := range []string{"../outside.txt", "../../outside.txt", "a/../../outside.txt", "/etc/passwd", "a//b", "a\\..\\b", ".", "..", "x/./y", "", "a\x00b"} {
		for _, op := range []string{"/get", "/put", "/del"} {
			v := call("POST", op+"?k="+urlQuery(k), "pwned")
			if v.Err == "" || !strings.Contains(v.Err, "invalid key") {
				t.Errorf("%s %q = %+v; want invalid key", op, k, v)
			}
		}
	}
	if b, _ := os.ReadFile(secret); string(b) != "outside" {
		t.Fatalf("outside file changed: %q", b)
	}
	if v := call("GET", "/big", ""); !strings.Contains(v.Err, "limit") {
		t.Fatalf("big = %+v", v)
	}
	if b, err := os.ReadFile(filepath.Join(f.dataDir, "files", "notes", "b.txt")); err != nil || string(b) != "beta" {
		t.Fatalf("on disk: %q %v", b, err)
	}
}

func urlQuery(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

func TestSecretsInEnv(t *testing.T) {
	m := newManager(t)
	f := mustStart(t, m, "env", map[string]string{"index.js": `
export default { fetch(request, env) { return Response.json({ key: env.API_KEY, mode: env.MODE, keys: Object.keys(env).sort() }); } }`},
		"index.js", map[string]string{"API_KEY": "sk-test-123", "MODE": "prod"})
	var v struct {
		Key, Mode string
		Keys      []string
	}
	f.json(t, "/", &v)
	if v.Key != "sk-test-123" || v.Mode != "prod" || !slices.Equal(v.Keys, []string{"API_KEY", "DB", "FILES", "MODE"}) {
		t.Fatalf("env = %+v", v)
	}
}

const limitsApp = `
let hits = 0;
export default {
  async fetch(request, env) {
    hits++;
    const p = new URL(request.url).pathname;
    if (p === "/loop") { for (;;) {} }
    if (p === "/spin-async") { await null; while (true) {} }
    if (p === "/bomb") { const a = []; for (;;) a.push(new Array(100000).fill(1.5)); }
    if (p === "/str") { let s = "x"; for (;;) s = s + s; }
    if (p === "/recurse") { const f = (n) => f(n + 1) + 1; return new Response(String(f(0))); }
    if (p === "/parse") { let x = JSON.parse(request.body); let k = 0; while (Array.isArray(x) && x.length) { x = x[0]; k++; } return new Response(String(k)); }
    if (p === "/depth") { const max = +new URL(request.url).searchParams.get("n"); const f = (n) => n >= max ? 0 : f(n + 1) + 1; return new Response(String(f(0))); }
    return new Response("ok " + hits);
  }
}`

func TestInfiniteLoopTimesOut(t *testing.T) {
	m := newManager(t)
	f := mustStart(t, m, "loop", map[string]string{"index.js": limitsApp}, "index.js", nil)
	if r := f.get(t, "/"); r.status != 200 {
		t.Fatalf("warmup = %d", r.status)
	}
	pid := f.inst.(*instance).pid()
	for _, path := range []string{"/loop", "/spin-async"} {
		start := time.Now()
		r := f.get(t, path)
		took := time.Since(start)
		t.Logf("%s -> %d after %v", path, r.status, took.Round(time.Millisecond))
		if r.status != http.StatusGatewayTimeout || took < 9*time.Second || took > 15*time.Second {
			t.Fatalf("%s = %d %q after %v; want 504 after ~10s", path, r.status, r.body, took)
		}
		if r := f.get(t, "/"); r.status != 200 {
			t.Fatalf("after %s = %d %q", path, r.status, r.body)
		}
	}
	if p := f.inst.(*instance).pid(); p != pid {
		t.Fatalf("the worker process was replaced (%d -> %d); only the JS runtime should be", pid, p)
	}
	f.logs.waitFor(t, "-> 504")
}

func TestMemoryBombAndRecursion(t *testing.T) {
	m := newManager(t)
	f := mustStart(t, m, "bomb", map[string]string{"index.js": limitsApp}, "index.js", nil)
	for _, path := range []string{"/bomb", "/str", "/recurse", "/bomb"} {
		start := time.Now()
		r := f.get(t, path)
		t.Logf("%s -> %d %q after %v", path, r.status, strings.TrimSpace(r.body), time.Since(start).Round(time.Millisecond))
		if r.status < 500 {
			t.Fatalf("%s = %d %q; want 5xx", path, r.status, r.body)
		}
		for i := 0; i < 3; i++ {
			if r := f.get(t, "/"); r.status != 200 {
				t.Fatalf("after %s = %d %q; logs:\n%s", path, r.status, r.body, f.logs)
			}
		}
	}
	maxOK := 0
	for _, n := range []int{100, 300, 1000, 2000, 3000, 4000, 6000, 10000, 20000} {
		if r := f.get(t, fmt.Sprintf("/depth?n=%d", n)); r.status == 200 {
			maxOK = n
		} else {
			break
		}
	}
	t.Logf("JS recursion depth that still works: %d (deeper recursion hits the stack guard -> 500, the runtime is replaced)", maxOK)
	if maxOK < 1000 {
		t.Errorf("recursion depth %d < 1000", maxOK)
	}
	// A visitor-supplied deeply nested JSON body cannot break the flat.
	for _, d := range []int{1000, 200000} {
		r := f.do(t, "POST", "/parse", strings.Repeat("[", d)+strings.Repeat("]", d))
		t.Logf("JSON.parse of %d-deep body -> %d %s", d, r.status, strings.TrimSpace(r.body))
		if d == 1000 && (r.status != 200 || r.body != "999") {
			t.Errorf("1000-deep JSON = %d %q", r.status, r.body)
		}
		if r := f.get(t, "/"); r.status != 200 {
			t.Fatalf("after %d-deep JSON = %d", d, r.status)
		}
	}
	if r := f.get(t, "/"); r.status != 200 {
		t.Fatalf("after depth probe = %d", r.status)
	}
	for _, l := range strings.Split(f.logs.String(), "\n") {
		if strings.Contains(l, "-> 5") {
			t.Logf("log: %.160s", l)
		}
	}
}

func TestSyntaxErrorFailsStart(t *testing.T) {
	m := newManager(t)
	_, err := startFlat(t, m, "bad", map[string]string{"index.js": "export default {\n  fetch(request) {\n    return new Response(\"x\"\n  }\n}\n"}, "index.js", nil)
	if err == nil || !strings.Contains(err.Error(), "SyntaxError") || !strings.Contains(err.Error(), "index.js") {
		t.Fatalf("err = %v", err)
	}
	t.Logf("syntax error: %v", err)
	_, err = startFlat(t, m, "nodefault", map[string]string{"index.js": "export const x = 1;"}, "index.js", nil)
	if err == nil || !strings.Contains(err.Error(), "default export") {
		t.Fatalf("no default: %v", err)
	}
	t.Logf("no default export: %v", err)
	_, err = startFlat(t, m, "nofetch", map[string]string{"index.js": "export default { hello() {} };"}, "index.js", nil)
	if err == nil || !strings.Contains(err.Error(), "fetch") {
		t.Fatalf("no fetch: %v", err)
	}
	_, err = startFlat(t, m, "missing", map[string]string{"index.js": "x"}, "server.js", nil)
	if err == nil || !strings.Contains(err.Error(), "server.js") {
		t.Fatalf("missing entry: %v", err)
	}
	t.Logf("missing entry: %v", err)
	_, err = startFlat(t, m, "throws", map[string]string{"index.js": "throw new Error('top-level failure');\nexport default { fetch() {} };"}, "index.js", nil)
	if err == nil || !strings.Contains(err.Error(), "top-level failure") {
		t.Fatalf("throwing module: %v", err)
	}
}

func TestSandbox(t *testing.T) {
	t.Setenv("FLATS_PARENT_SECRET", "parent-secret-value")
	m := newManager(t)
	// Flat A holds a secret; flat B must not see it.
	mustStart(t, m, "alpha", map[string]string{"index.js": `export default { fetch() { return new Response("a"); } }`},
		"index.js", map[string]string{"ALPHA_SECRET": "alpha-secret-value"})
	cwd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	f := mustStart(t, m, "beta", map[string]string{"index.js": `
import * as std from "qjs:std";
import * as os from "qjs:os";
const probe = (fn) => { try { const v = fn(); return v === undefined ? "undefined" : v; } catch (e) { return "threw: " + e.message; } };
export default {
  async fetch(request, env) {
    const q = JSON.parse(request.body);
    const out = {};
    for (const p of q.paths) out["load:" + p] = probe(() => std.loadFile(p));
    const ls = (d) => JSON.stringify(os.readdir(d)[0].filter((n) => n !== "." && n !== "..").sort());
    out.readdirRoot = probe(() => ls("/"));
    out.readdirUp = probe(() => ls("/.."));
    out.readdirDot = probe(() => ls("../.."));
    out.write = probe(() => { const f = std.open("/written.txt", "w"); if (!f) return "no file"; f.puts("x"); f.close(); return "wrote"; });
    out.getenvHome = probe(() => std.getenv("HOME"));
    out.getenvPath = probe(() => std.getenv("PATH"));
    out.getenvParent = probe(() => std.getenv("FLATS_PARENT_SECRET"));
    out.getenvAlpha = probe(() => std.getenv("ALPHA_SECRET"));
    out.envKeys = Object.keys(env).sort().join(",");
    out.globals = ["fetch", "XMLHttpRequest", "WebSocket", "Deno", "process", "require"].map((g) => g + ":" + typeof globalThis[g]).join(",");
    out.osExec = typeof os.exec;
    out.urlGet = probe(() => typeof std.urlGet === "function" ? std.urlGet("http://127.0.0.1:" + q.port + "/") : "absent");
    out.socket = typeof os.socket;
    try { await import("/etc/passwd"); out.importPasswd = "imported"; } catch (e) { out.importPasswd = "refused"; }
    try { env.DB.exec("ATTACH DATABASE ? AS x", q.attach); out.attach = "attached"; } catch (e) { out.attach = "refused: " + e.message; }
    try { env.DB.exec("VACUUM INTO ?", q.vacuum); out.vacuum = "vacuumed"; } catch (e) { out.vacuum = "refused: " + e.message; }
    try { env.DB.query("SELECT load_extension('x')"); out.ext = "loaded"; } catch (e) { out.ext = "refused: " + e.message; }
    try { out.readfile = JSON.stringify(env.DB.query("SELECT readfile('/etc/passwd') AS x")); } catch (e) { out.readfile = "refused: " + e.message; }
    return Response.json(out);
  }
}`}, "index.js", map[string]string{"BETA_SECRET": "beta"})

	parentFile := filepath.Join(f.root, "parent-file.txt")
	os.WriteFile(parentFile, []byte("parent-file-content"), 0o644)
	// A TCP listener the guest must not be able to reach.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	reached := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			reached <- struct{}{}
			c.Close()
		}
	}()
	paths := []string{"/etc/passwd", "../../../../etc/passwd", "/../etc/passwd", parentFile, "../parent-file.txt", "../../parent-file.txt",
		filepath.Join(cwd, "runtime_test.go"), filepath.Join(home, ".zshrc"), "/index.js"}
	q, _ := json.Marshal(map[string]any{"paths": paths, "port": ln.Addr().(*net.TCPAddr).Port,
		"attach": parentFile, "vacuum": filepath.Join(f.root, "stolen.db")})
	r := f.do(t, "POST", "/", string(q))
	if r.status != 200 {
		t.Fatalf("probe = %d %q; logs:\n%s", r.status, r.body, f.logs)
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(r.body), &out); err != nil {
		t.Fatalf("%v: %s", err, r.body)
	}
	for k, v := range out {
		t.Logf("%-40s %.120q", k, v)
	}
	for _, p := range paths[:len(paths)-1] {
		if v := out["load:"+p]; v != "" && !strings.HasPrefix(v, "threw") { // "" = JSON null
			t.Errorf("std.loadFile(%q) = %.80q; want null", p, v)
		}
	}
	if !strings.Contains(out["load:/index.js"], "export default") {
		t.Errorf("the flat's own files should be readable read-only: %q", out["load:/index.js"])
	}
	for _, k := range []string{"readdirRoot", "readdirUp", "readdirDot"} {
		if out[k] != `["index.js"]` && out[k] != `[]` {
			t.Errorf("%s = %s; want only the version files", k, out[k])
		}
	}
	if out["readdirRoot"] != `["index.js"]` {
		t.Errorf("readdir / = %s", out["readdirRoot"])
	}
	if out["write"] == "wrote" {
		t.Errorf("guest could write into the version dir")
	}
	if _, err := os.Stat(filepath.Join(f.spec.Dir, "written.txt")); err == nil {
		t.Errorf("written.txt appeared on the host")
	}
	for _, k := range []string{"getenvHome", "getenvPath", "getenvParent", "getenvAlpha"} {
		if out[k] != "undefined" {
			t.Errorf("%s = %q; want undefined", k, out[k])
		}
	}
	if out["envKeys"] != "BETA_SECRET,DB,FILES" {
		t.Errorf("env keys = %q", out["envKeys"])
	}
	if out["globals"] != "fetch:undefined,XMLHttpRequest:undefined,WebSocket:undefined,Deno:undefined,process:undefined,require:undefined" {
		t.Errorf("globals = %q", out["globals"])
	}
	if out["osExec"] != "undefined" || out["socket"] != "undefined" {
		t.Errorf("os.exec %q, os.socket %q", out["osExec"], out["socket"])
	}
	select {
	case <-reached:
		t.Errorf("guest reached a host TCP listener (urlGet = %q)", out["urlGet"])
	case <-time.After(200 * time.Millisecond):
	}
	if out["importPasswd"] != "refused" {
		t.Errorf("import /etc/passwd = %q", out["importPasswd"])
	}
	for _, k := range []string{"attach", "vacuum", "ext", "readfile"} {
		if !strings.HasPrefix(out[k], "refused") {
			t.Errorf("%s = %q; want refused", k, out[k])
		}
	}
	if _, err := os.Stat(filepath.Join(f.root, "stolen.db")); err == nil {
		t.Errorf("VACUUM INTO wrote a host file")
	}
	// The worker process itself has a minimal environment.
	f.logs.waitFor(t, "test-env-keys: ")
	if !f.logs.find("test-env-keys: "+testWorkerEnv+"\n") && !strings.Contains(f.logs.String(), "test-env-keys: "+testWorkerEnv) {
		t.Errorf("worker environment: %s", f.logs)
	}
	for _, l := range strings.Split(f.logs.String(), "\n") {
		if strings.Contains(l, "test-env-keys: ") && !strings.HasSuffix(l, "test-env-keys: "+testWorkerEnv) {
			t.Errorf("worker environment has more than %s: %s", testWorkerEnv, l)
		}
	}
	if cmd := m.command(); len(cmd.Env) != 1 || cmd.Env[0] != testWorkerEnv+"=1" || slices.Contains(cmd.Args, "sk") {
		t.Errorf("worker command env = %v args = %v", cmd.Env, cmd.Args)
	}
	if strings.Contains(f.logs.String(), "alpha-secret-value") || strings.Contains(f.logs.String(), "parent-secret-value") {
		t.Errorf("secret leaked into logs")
	}
}

func TestWebSocketEcho(t *testing.T) {
	m := newManager(t)
	f := mustStart(t, m, "ws", map[string]string{"index.js": `
const peers = new Set();
export default {
  fetch(request) { return new Response("http " + request.method); },
  websocket: {
    open(ws, env) { peers.add(ws); ws.send("welcome " + new URL(ws.url).pathname + " peers=" + peers.size); },
    message(ws, data) {
      if (data === "close") return ws.close(4000, "bye");
      if (data === "broadcast") { for (const p of peers) p.send("all:" + peers.size); return; }
      ws.send("echo:" + data);
    },
    close(ws) { peers.delete(ws); console.log("closed", ws.closeCode); },
  },
}`}, "index.js", nil)
	if r := f.get(t, "/"); r.body != "http GET" {
		t.Fatalf("plain GET = %q", r.body)
	}
	addr := strings.TrimPrefix(f.srv.URL, "http://")
	dial := func(ctx context.Context) (net.Conn, error) { return (&net.Dialer{}).DialContext(ctx, "tcp", addr) }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, err := dialWS(ctx, dial, addr, "/chat")
	if err != nil {
		t.Fatalf("dial: %v; logs:\n%s", err, f.logs)
	}
	defer a.c.Close()
	a.c.SetDeadline(time.Now().Add(10 * time.Second))
	expect := func(c *wsConn, want string) {
		t.Helper()
		got, err := c.readMessage()
		if err != nil || got != want {
			t.Fatalf("read = %q, %v; want %q; logs:\n%s", got, err, want, f.logs)
		}
	}
	expect(a, "welcome /chat peers=1")
	a.writeFrame(opText, []byte("hello"))
	expect(a, "echo:hello")
	big := strings.Repeat("é", 40000)
	a.writeFrame(opText, []byte(big))
	expect(a, "echo:"+big)
	b, err := dialWS(ctx, dial, addr, "/chat")
	if err != nil {
		t.Fatal(err)
	}
	defer b.c.Close()
	b.c.SetDeadline(time.Now().Add(10 * time.Second))
	expect(b, "welcome /chat peers=2")
	b.writeFrame(opText, []byte("broadcast"))
	expect(a, "all:2")
	expect(b, "all:2")
	a.writeFrame(opText, []byte("close"))
	_, err = a.readMessage()
	if ce, ok := err.(*wsCloseError); !ok || ce.Code != 4000 || ce.Reason != "bye" {
		t.Fatalf("close = %v", err)
	}
	f.logs.waitFor(t, "closed 4000")
	// Client-initiated close.
	b.writeFrame(opClose, []byte{0x03, 0xE8})
	if _, err := b.readMessage(); err == nil {
		t.Fatal("expected close")
	}
	f.logs.waitFor(t, "closed 1000")
}

func TestWASIHandler(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain to build a wasip1 guest")
	}
	src := t.TempDir()
	writeFiles(t, src, map[string]string{
		"go.mod": "module guest\n\ngo 1.24\n",
		"main.go": `package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

func main() {
	var req struct {
		Method string            ` + "`json:\"method\"`" + `
		URL    string            ` + "`json:\"url\"`" + `
		Body   *string           ` + "`json:\"body\"`" + `
	}
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		fmt.Fprintln(os.Stderr, "bad request:", err)
		os.Exit(2)
	}
	if req.Method == "PUT" {
		for {
		}
	}
	if req.Method == "DELETE" {
		time.Sleep(time.Hour)
	}
	_, ferr := os.ReadFile("/etc/passwd")
	fmt.Fprintln(os.Stderr, "guest log line")
	json.NewEncoder(os.Stdout).Encode(map[string]any{
		"status":  200,
		"headers": map[string]string{"content-type": "text/plain", "x-guest": "wasi"},
		"body":    fmt.Sprintf("wasi %s %s greeting=%s home=%q fs=%v", req.Method, req.URL, os.Getenv("GREETING"), os.Getenv("HOME"), ferr != nil),
	})
}
`,
	})
	out := filepath.Join(t.TempDir(), "handler.wasm")
	cmd := exec.Command(goBin, "build", "-o", out, ".")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0", "GOFLAGS=")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build a wasip1 guest: %v\n%s", err, b)
	}
	wasm, _ := os.ReadFile(out)
	m := newManager(t)
	t0 := time.Now()
	f := mustStart(t, m, "wasi", map[string]string{"handler.wasm": string(wasm)}, "handler.wasm", map[string]string{"GREETING": "hi"})
	t.Logf("WASI cold start (incl. compile of %d KiB): %v", len(wasm)>>10, time.Since(t0).Round(time.Millisecond))
	r := f.get(t, "/x?y=1")
	if r.status != 200 || r.header.Get("X-Guest") != "wasi" || !strings.HasPrefix(r.body, "wasi GET http://") ||
		!strings.Contains(r.body, "/x?y=1 greeting=hi home=\"\" fs=true") {
		t.Fatalf("wasi = %d %q %v; logs:\n%s", r.status, r.body, r.header, f.logs)
	}
	f.logs.waitFor(t, "guest log line")
	lat := measure(t, f, "/x", 50)
	t.Logf("WASI request latency p50 %v", lat)
	start := time.Now()
	r = f.do(t, "PUT", "/", "x")
	if r.status != 504 || time.Since(start) > 15*time.Second {
		t.Fatalf("wasi loop = %d after %v", r.status, time.Since(start))
	}
	if r := f.get(t, "/"); r.status != 200 {
		t.Fatalf("after loop = %d", r.status)
	}
	// A sleeping guest is stopped by the timeout too (wazero's host sleep
	// ignores the context).
	start = time.Now()
	r = f.do(t, "DELETE", "/", "")
	if r.status != 504 || time.Since(start) > 15*time.Second {
		t.Fatalf("wasi sleep = %d after %v", r.status, time.Since(start))
	}
	if r := f.get(t, "/"); r.status != 200 {
		t.Fatalf("after sleep = %d", r.status)
	}
}

func measure(t *testing.T, f *flat, path string, n int) time.Duration {
	t.Helper()
	d := make([]time.Duration, n)
	for i := range d {
		start := time.Now()
		if r := f.get(t, path); r.status != 200 {
			t.Fatalf("%s = %d", path, r.status)
		}
		d[i] = time.Since(start)
	}
	slices.Sort(d)
	return d[n/2]
}

func TestLatencyAndConcurrency(t *testing.T) {
	m := newManager(t)
	files := map[string]string{"index.js": `
let n = 0;
export default {
  async fetch(request, env) {
    n++;
    if (request.url.endsWith("/db")) { env.DB.exec("CREATE TABLE IF NOT EXISTS t (x)"); env.DB.exec("INSERT INTO t VALUES (?)", n); return Response.json(env.DB.query("SELECT count(*) AS c FROM t")[0]); }
    if (request.url.endsWith("/slow")) { const end = Date.now() + 300; while (Date.now() < end) {} }
    return new Response("ok");
  }
}`}
	t0 := time.Now()
	f := mustStart(t, m, "perf", files, "index.js", nil)
	cold := time.Since(t0)
	first := time.Now()
	f.get(t, "/")
	t.Logf("cold start (Start until ready): %v; first request %v", cold.Round(time.Millisecond), time.Since(first).Round(time.Microsecond))
	t.Logf("JS request latency p50 via parent proxy: %v", measure(t, f, "/", 200))
	t.Logf("JS request latency p50 with DB insert+query: %v", measure(t, f, "/db", 100))

	// Four slow requests run in parallel on the runtime pool.
	var wg sync.WaitGroup
	start := time.Now()
	errs := make(chan string, 8)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(f.srv.URL + "/slow")
			if err != nil || resp.StatusCode != 200 {
				errs <- fmt.Sprint(resp, err)
				return
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	took := time.Since(start)
	t.Logf("4 concurrent 300ms requests took %v", took.Round(time.Millisecond))
	if took > 1100*time.Millisecond {
		t.Errorf("requests did not run in parallel (%v)", took)
	}
	// Many concurrent DB writers.
	var wg2 sync.WaitGroup
	fail := make(chan string, 40)
	for i := 0; i < 40; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			resp, err := http.Get(f.srv.URL + "/db")
			if err != nil {
				fail <- err.Error()
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				fail <- string(b)
			}
		}()
	}
	wg2.Wait()
	close(fail)
	for e := range fail {
		t.Errorf("concurrent db: %s; logs:\n%s", e, f.logs)
	}
}

func TestStopWhileServing(t *testing.T) {
	m := newManager(t)
	f := mustStart(t, m, "stop", map[string]string{"index.js": `export default { fetch() { return new Response("ok"); } }`}, "index.js", nil)
	if r := f.get(t, "/"); r.status != 200 {
		t.Fatal(r.status)
	}
	in := f.inst.(*instance)
	pid := in.pid()
	in.Stop()
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("worker %d still running after Stop", pid)
	}
	if r := f.get(t, "/"); r.status != http.StatusServiceUnavailable {
		t.Fatalf("after Stop = %d", r.status)
	}
	in.Stop() // idempotent
}

const hardeningApp = `
import * as os from "qjs:os";
import * as std from "qjs:std";
export default {
  async fetch(request, env) {
    const u = new URL(request.url);
    const p = u.pathname;
    if (p === "/sleep") { os.sleep(60000); return new Response("slept"); }
    if (p === "/timer") { await new Promise((r) => os.setTimeout(r, 60000)); return new Response("timer"); }
    if (p === "/sqlloop") return Response.json(env.DB.query("WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c) SELECT count(*) AS n FROM c"));
    if (p === "/exit") std.exit(3);
    if (p === "/sql") {
      try { return new Response("ok: " + JSON.stringify(env.DB.query(request.body))); }
      catch (e) { return new Response("refused: " + e.message); }
    }
    return new Response("ok");
  }
}`

// Sleeps (os.sleep, os.setTimeout) and long SQL statements are stopped by the
// request timeout like busy loops; wazero's host sleep ignores the context.
func TestTimeoutStopsSleepsAndSQL(t *testing.T) {
	m := newManager(t)
	m.Timeout = 2 * time.Second
	f := mustStart(t, m, "sleepy", map[string]string{"index.js": hardeningApp}, "index.js", nil)
	for _, c := range []struct {
		path   string
		status int
	}{{"/sleep", 504}, {"/timer", 504}, {"/sqlloop", 504}, {"/exit", 500}} {
		start := time.Now()
		done := make(chan result, 1)
		go func() {
			req, _ := http.NewRequest("GET", f.srv.URL+c.path, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				done <- result{}
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			done <- result{resp.StatusCode, resp.Header, string(b)}
		}()
		select {
		case r := <-done:
			t.Logf("%s -> %d after %v", c.path, r.status, time.Since(start).Round(time.Millisecond))
			if r.status != c.status {
				t.Errorf("%s = %d %q; want %d", c.path, r.status, r.body, c.status)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: no answer after 10s with a 2s timeout", c.path)
		}
		if r := f.get(t, "/"); r.status != 200 {
			t.Fatalf("after %s = %d", c.path, r.status)
		}
	}
}

func TestSQLiteHardening(t *testing.T) {
	m := newManager(t)
	f := mustStart(t, m, "sqlhard", map[string]string{"index.js": hardeningApp}, "index.js", nil)
	tmp := t.TempDir()
	sql := func(q string) string {
		t.Helper()
		r := f.do(t, "POST", "/sql", q)
		if r.status != 200 {
			t.Fatalf("%s = %d %q", q, r.status, r.body)
		}
		return r.body
	}
	refused := []string{
		"PRAGMA temp_store_directory = '" + tmp + "'",
		`PRAGMA "Temp_Store_Directory" = '` + tmp + "'",
		"PRAGMA data_store_directory = '" + tmp + "'",
		"SELECT randomblob(1000000000)",
		"SELECT zeroblob(100000000)",
		"UPDATE sqlite_dbpage SET data = zeroblob(4096) WHERE pgno = 1",
	}
	sql("CREATE TABLE IF NOT EXISTS t (x)")
	for _, q := range refused {
		if r := sql(q); !strings.HasPrefix(r, "refused") {
			t.Errorf("%s = %.200s; want refused", q, r)
		}
	}
	if r := sql("PRAGMA writable_schema = ON"); strings.HasPrefix(r, "ok") {
		if r := sql("UPDATE sqlite_schema SET sql = 'x' WHERE name = 't'"); !strings.HasPrefix(r, "refused") {
			t.Errorf("schema update under writable_schema = %q; want refused (defensive mode)", r)
		}
	}
	sql("PRAGMA hard_heap_limit = 0")
	sql("PRAGMA hard_heap_limit = 100000000000")
	want := fmt.Sprintf(`ok: [{"hard_heap_limit":%d}]`, sqliteHeapLimit)
	if r := sql("PRAGMA hard_heap_limit"); r != want {
		t.Errorf("hard_heap_limit = %q; want %q (it can only be lowered)", r, want)
	}
	for q, w := range map[string]string{"PRAGMA temp_store": `ok: [{"temp_store":2}]`, "PRAGMA trusted_schema": `ok: [{"trusted_schema":0}]`} {
		if r := sql(q); r != w {
			t.Errorf("%s = %q; want %q", q, r, w)
		}
	}
	if r := sql("SELECT length(zeroblob(1000000)) AS n"); r != `ok: [{"n":1000000}]` {
		t.Errorf("1 MB blob = %q", r)
	}
}

// wazero's directory mount follows symlinks, so a symlink in the version
// files would expose a host file: the worker refuses to start.
func TestVersionSymlinkRefused(t *testing.T) {
	m := newManager(t)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	os.WriteFile(outside, []byte("OUTSIDE-SECRET"), 0o644)
	root := t.TempDir()
	dir := filepath.Join(root, "v")
	writeFiles(t, dir, map[string]string{"index.js": `import * as std from "qjs:std";
export default { fetch() { return new Response(String(std.loadFile("/link.txt"))); } }`})
	if err := os.Symlink(outside, filepath.Join(dir, "link.txt")); err != nil {
		t.Skip(err)
	}
	l := &logs{}
	inst, err := m.Start(context.Background(), core.RuntimeSpec{Flat: "sym", Version: 1, Dir: dir, Entry: "index.js", DataDir: filepath.Join(root, "data"), Log: l.add})
	if err == nil {
		defer inst.Stop()
		rec := httptest.NewRecorder()
		inst.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		t.Fatalf("started with a symlink in the version files; GET / = %d %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(err.Error(), "link.txt") {
		t.Fatalf("err = %v", err)
	}
}

// A worker that fails to start leaves no socket behind.
func TestFailedStartRemovesSocket(t *testing.T) {
	m := newManager(t)
	glob := filepath.Join(m.DataDir, "run", "w*.sock")
	before, _ := filepath.Glob(glob)
	if _, err := startFlat(t, m, "bad", map[string]string{"index.js": "syntax error here {"}, "index.js", nil); err == nil {
		t.Fatal("expected a start error")
	}
	if after, _ := filepath.Glob(glob); len(after) != len(before) {
		t.Fatalf("sockets before %v, after %v", before, after)
	}
}

// A visitor that stops reading must not stall the websocket runtime that
// serves every other connection of the flat (closing its connection used to
// wait for the blocked writer).
func TestWebSocketSlowClient(t *testing.T) {
	m := newManager(t)
	f := mustStart(t, m, "wsslow", map[string]string{"index.js": `
export default {
  fetch() { return new Response("http"); },
  websocket: {
    open(ws) { ws.send("welcome"); },
    message(ws, data) {
      if (data !== "flood") return ws.send("echo:" + data);
      const chunk = "x".repeat(65536);
      for (let i = 0; i < 2000; i++) { try { ws.send(chunk); } catch (e) { return; } }
    },
  },
}`}, "index.js", nil)
	addr := strings.TrimPrefix(f.srv.URL, "http://")
	dial := func(ctx context.Context) (net.Conn, error) { return (&net.Dialer{}).DialContext(ctx, "tcp", addr) }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	slow, err := dialWS(ctx, dial, addr, "/")
	if err != nil {
		t.Fatal(err)
	}
	defer slow.c.Close()
	if m, err := slow.readMessage(); err != nil || m != "welcome" {
		t.Fatalf("welcome = %q %v", m, err)
	}
	slow.writeFrame(opText, []byte("flood")) // and never read again
	time.Sleep(500 * time.Millisecond)
	start := time.Now()
	other, err := dialWS(ctx, dial, addr, "/")
	if err != nil {
		t.Fatal(err)
	}
	defer other.c.Close()
	other.c.SetDeadline(time.Now().Add(20 * time.Second))
	if m, err := other.readMessage(); err != nil || m != "welcome" {
		t.Fatalf("other welcome = %q %v", m, err)
	}
	other.writeFrame(opText, []byte("hi"))
	if m, err := other.readMessage(); err != nil || m != "echo:hi" {
		t.Fatalf("other echo = %q %v", m, err)
	}
	took := time.Since(start)
	t.Logf("other connection served in %v while a client stalled", took.Round(time.Millisecond))
	// Events run one at a time, so the other connection waits for the flood
	// handler itself (seconds under -race), which is fine. The defect was the
	// handler blocking until its timeout, after which the websocket runtime
	// was replaced and every connection closed with 1011 (checked above).
	if f.logs.find("websocket message handler") {
		t.Errorf("the flood handler failed (it must not block on the slow peer):\n%s", f.logs)
	}
}

func TestCryptoAndForwardedScheme(t *testing.T) {
	m := newManager(t)
	f := mustStart(t, m, "cryptoflat", map[string]string{
		"index.js": `
export default {
  async fetch(request, env) {
    const a = crypto.getRandomValues(new Uint8Array(16));
    const b = crypto.getRandomValues(new Uint32Array(4));
    return Response.json({ uuid: crypto.randomUUID(), a: Array.from(a), b: Array.from(b), url: request.url });
  }
}`,
	}, "index.js", nil)
	r := f.do(t, "GET", "/x", "", "X-Forwarded-Proto", "https")
	var out struct {
		UUID string `json:"uuid"`
		A    []int  `json:"a"`
		B    []int  `json:"b"`
		URL  string `json:"url"`
	}
	if err := json.Unmarshal([]byte(r.body), &out); err != nil {
		t.Fatalf("body %q: %v", r.body, err)
	}
	if len(out.UUID) != 36 || out.UUID[14] != '4' || len(out.A) != 16 || len(out.B) != 4 {
		t.Fatalf("crypto output %+v", out)
	}
	zero := 0
	for _, v := range out.A {
		if v == 0 {
			zero++
		}
	}
	if zero == 16 {
		t.Fatal("random bytes are all zero")
	}
	if !strings.HasPrefix(out.URL, "https://") {
		t.Fatalf("forwarded https scheme lost: %s", out.URL)
	}
	r2 := f.get(t, "/x")
	var out2 struct {
		UUID string `json:"uuid"`
	}
	json.Unmarshal([]byte(r2.body), &out2)
	if out2.UUID == out.UUID {
		t.Fatal("randomUUID repeated")
	}
}
