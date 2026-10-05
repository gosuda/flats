package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/egress"
	"github.com/gosuda/flats/internal/qjs"
)

// Exercise the real embedded QuickJS prelude with a test-only host stub. This
// verifies binary/HTTP bridging without bypassing the production egress client
// or making external requests; transport security is tested in internal/egress.
func TestOutboundFetchPrelude(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rt, err := qjs.New(qjs.Option{Context: ctx, CloseOnContextDone: true})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	c := rt.Context()
	var requests []egress.Request
	c.SetFunc("__flats_host", func(this *qjs.This) (*qjs.Value, error) {
		args := this.Args()
		defer func() {
			for _, arg := range args {
				arg.Free()
			}
		}()
		if args[0].String() == "env" {
			return this.Context().NewString("{}"), nil
		}
		if args[0].String() != "fetch" {
			return nil, errors.New("unexpected test operation")
		}
		var a []egress.Request
		if err := json.Unmarshal([]byte(args[1].String()), &a); err != nil {
			return nil, err
		}
		requests = append(requests, a[0])
		body := []byte("{\"message\":\"안녕🌍\"}")
		if strings.HasSuffix(a[0].URL, "/binary") {
			body = []byte{0, 1, 255, 0xc3, 0xa9}
		}
		b, _ := json.Marshal(egress.Response{Status: 201, Headers: map[string][]string{"x-fixture": {"first", "second"}}, Body: body, URL: a[0].URL})
		return this.Context().NewString(string(b)), nil
	})
	v, err := c.Eval("prelude.js", qjs.Code(preludeJS))
	if err != nil {
		t.Fatal(err)
	}
	v.Free()
	v, err = c.Eval("fetch-test.js", qjs.Code(`(async () => {
  const req = new Request("https://api.example.test/json", {method: "post", headers: {Authorization: "Bearer fixture-only"}, body: "안녕🌍"});
  const response = await fetch(req);
  const data = await response.json();
  const binary = await fetch("https://api.example.test/binary", {method: "PUT", body: new Uint8Array([0, 255, 1]), redirect: "manual"});
  return JSON.stringify({message: data.message, status: response.status, ok: response.ok, url: response.url, redirected: response.redirected, header: response.headers.get("x-fixture"), bytes: Array.from(new Uint8Array(await binary.arrayBuffer())), text: await binary.text(), contentType: req.headers.get("content-type")});
 })()`))
	if err != nil {
		t.Fatal(err)
	}
	result := v
	defer result.Free()
	var got struct {
		Message     string
		Status      int
		OK          bool
		URL         string
		Redirected  bool
		Header      string
		Bytes       []int
		Text        string
		ContentType string
	}
	if err := json.Unmarshal([]byte(result.String()), &got); err != nil {
		t.Fatal(err)
	}
	if got.Message != "안녕🌍" || got.Status != 201 || !got.OK || got.URL != "https://api.example.test/json" || got.Redirected || got.Header != "first, second" || got.ContentType != "text/plain;charset=UTF-8" {
		t.Fatalf("fetch result: %+v", got)
	}
	if len(got.Bytes) != 5 || got.Bytes[2] != 255 || got.Text != "\x00\x01\ufffdé" {
		t.Fatalf("binary result: %+v", got)
	}
	if len(requests) != 2 || string(requests[0].Body) != "안녕🌍" || requests[0].Method != "POST" || requests[0].Headers["authorization"][0] != "Bearer fixture-only" || requests[1].Redirect != "manual" || len(requests[1].Body) != 3 || requests[1].Body[1] != 255 {
		t.Fatalf("host requests: %+v", requests)
	}
	example, err := os.ReadFile("../../examples/external-api/server.js")
	if err != nil {
		t.Fatal(err)
	}
	v, err = c.Eval("server-example.js", qjs.Code(strings.Replace(string(example), "export default", "globalThis.example =", 1)+`
 example.fetch(new Request({method: "GET", url: "https://flat.test/api/message", headers: {}}), {API_BASE: "https://api.example.test", API_TOKEN: "example-fixture-only"}).then(async (r) => JSON.stringify({status: r.status, body: await r.json()}));`))
	if err != nil {
		t.Fatal(err)
	}
	exampleResult := v
	defer exampleResult.Free()
	if exampleResult.String() != `{"status":200,"body":{"message":"안녕🌍"}}` {
		t.Fatalf("checked-in example: %s", exampleResult.String())
	}
	if len(requests) != 3 || requests[2].URL != "https://api.example.test/message" || requests[2].Headers["authorization"][0] != "Bearer example-fixture-only" {
		t.Fatalf("example request: %+v", requests)
	}

}

func TestOutboundFetchDeniedAndLimits(t *testing.T) {
	code := `export default { async fetch(request) {
 const results = [];
 for (let i = 0; i < 17; i++) {
  try { await fetch("https://api.example.test/path?token=fixture-secret", {headers: {Authorization: "fixture-secret"}}); }
  catch (e) { results.push(e.message); }
 }
 return Response.json(results);
} };`
	f := mustStart(t, newManager(t), "fetch-denied", map[string]string{"index.js": code}, "index.js", nil)
	for repeat := 0; repeat < 2; repeat++ {
		r := f.do(t, "GET", "/", "")
		var got []string
		if r.status != 200 || json.Unmarshal([]byte(r.body), &got) != nil || len(got) != 17 {
			t.Fatalf("response: %d %s", r.status, r.body)
		}
		for i, message := range got {
			want := egress.ErrDenied.Error()
			if i == 16 {
				want = egress.ErrLimit.Error()
			}
			if message != want || strings.Contains(message, "fixture-secret") || strings.Contains(message, "api.example") {
				t.Fatalf("call %d: %q", i, message)
			}
		}
	}
	if f.logs.find("fixture-secret") {
		t.Fatalf("sensitive request leaked: %s", f.logs)
	}
}

func TestOutboundFetchUnsupportedOptions(t *testing.T) {
	code := `export default { async fetch() {
 const cases = [
  {redirect: "follow"}, {signal: {}}, {cache: "no-store"}, {body: {}, method: "POST"},
  {body: "x"}, {body: new Uint8Array(1048577), method: "POST"}, false,
 ];
 const messages = [];
 for (const options of cases) {
  try { await fetch("https://api.example.test", options); messages.push("allowed"); }
  catch (e) { messages.push(e.name + ": " + e.message); }
 }
 return Response.json(messages);
} };`
	f := mustStart(t, newManager(t), "fetch-options", map[string]string{"index.js": code}, "index.js", nil)
	r := f.do(t, "GET", "/", "")
	var got []string
	if r.status != 200 || json.Unmarshal([]byte(r.body), &got) != nil || len(got) != 7 {
		t.Fatalf("response: %d %s", r.status, r.body)
	}
	for _, message := range got {
		if !strings.HasPrefix(message, "TypeError:") {
			t.Fatalf("unsupported option: %q", message)
		}
	}
}

func TestOutboundFetchDeniedDuringModuleLoad(t *testing.T) {
	_, err := startFlat(t, newManager(t), "fetch-startup", map[string]string{"index.js": `await fetch("https://api.example.test"); export default { fetch() { return new Response("ok"); } };`}, "index.js", nil)
	if err == nil || !strings.Contains(err.Error(), "only available inside") {
		t.Fatalf("startup fetch: %v", err)
	}
}

func TestOutboundFetchHostLifecycle(t *testing.T) {
	client, err := egress.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	vm := &jsVM{w: &worker{network: client}}
	raw := json.RawMessage(`[{"url":"https://api.example.test"}]`)
	if _, err := vm.host(context.Background(), "fetch", raw); err == nil || !strings.Contains(err.Error(), "only available inside") {
		t.Fatalf("inactive VM: %v", err)
	}
	if _, err := vm.host(context.Background(), "fetch", json.RawMessage(strings.Repeat(" ", (2<<20)+1))); !errors.Is(err, egress.ErrLimit) {
		t.Fatalf("oversized ABI: %v", err)
	}
	vm.networkActive = true
	if _, err := vm.host(context.Background(), "fetch", raw); !errors.Is(err, egress.ErrDenied) {
		t.Fatalf("active deny: %v", err)
	}
	if _, err := vm.host(context.Background(), "fetch", json.RawMessage(`[{} , {}]`)); !errors.Is(err, egress.ErrInvalid) {
		t.Fatalf("bad arguments: %v", err)
	}
	vm.networkCalls = 16
	if _, err := vm.host(context.Background(), "fetch", raw); !errors.Is(err, egress.ErrLimit) {
		t.Fatalf("limit: %v", err)
	}
}

type contextEngine struct{ vmCanceled, outboundCanceled bool }

func (e *contextEngine) handle(ctx context.Context, _ []byte) ([]byte, error) {
	e.vmCanceled = ctx.Err() != nil
	fetchCtx, cancel := outboundContext(ctx)
	defer cancel()
	e.outboundCanceled = fetchCtx.Err() != nil
	return []byte(`{"status":200,"body":"ok"}`), nil
}
func (*contextEngine) close() {}

func TestWorkerSeparatesRequestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	eng := &contextEngine{}
	w := &worker{eng: eng, log: newChildLog(io.Discard)}
	r := httptest.NewRequest(http.MethodGet, "http://flat.test/", nil).WithContext(ctx)
	w.ServeHTTP(httptest.NewRecorder(), r)
	if eng.vmCanceled || !eng.outboundCanceled {
		t.Fatalf("disconnect cancellation: VM=%v outbound=%v", eng.vmCanceled, eng.outboundCanceled)
	}
}

func TestOutboundFetchServerExample(t *testing.T) {
	example, err := os.ReadFile("../../examples/external-api/server.js")
	if err != nil {
		t.Fatal(err)
	}
	manifestJSON, err := os.ReadFile("../../examples/external-api/flats.json")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := bundle.ParseManifest([]bundle.File{{Path: "server.js", Data: example}, {Path: "flats.json", Data: manifestJSON}})
	if err != nil || manifest.Kind != "server" || manifest.Entry != "server.js" || manifest.Health != "/healthz" {
		t.Fatalf("example manifest: %+v %v", manifest, err)
	}
	m := newManager(t)
	f := mustStart(t, m, "api-example", map[string]string{manifest.Entry: string(example)}, manifest.Entry, nil)
	for _, test := range []struct {
		path   string
		status int
		body   string
	}{
		{manifest.Health, 200, "ok"}, {"/api/message", 503, "API is not configured"}, {"/", 404, "Not found"},
	} {
		r := f.do(t, "GET", test.path, "")
		if r.status != test.status || r.body != test.body {
			t.Fatalf("%s: %d %s", test.path, r.status, r.body)
		}
	}
	f = mustStart(t, m, "api-example-denied", map[string]string{manifest.Entry: string(example)}, manifest.Entry, map[string]string{"API_BASE": "https://api.example.test", "API_TOKEN": "fixture-secret-only"})
	r := f.do(t, "GET", "/api/message", "")
	if r.status != 502 || r.body != "Upstream unavailable" || f.logs.find("fixture-secret-only") || f.logs.find("api.example.test") {
		t.Fatalf("sanitized denied example: %d %s logs %s", r.status, r.body, f.logs)
	}
}

// Invalid requests to a granted public origin stop before DNS/dial. Distinct
// errors prove real Manager->worker policy propagation without external I/O.
func TestOutboundFetchWorkerPermissions(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.js": `export default { async fetch() {
  const cases = [
   ["http://8.8.8.8/", {method: "CONNECT"}],
   ["http://8.8.8.8/", {headers: {Host: "override.example.test"}}],
   ["http://1.1.1.1/", {}],
   ["http://127.0.0.1/", {}],
  ];
  const messages = [];
  for (const [url, init] of cases) {
   try { await fetch(url, init); messages.push("allowed"); }
   catch (e) { messages.push(e.message); }
  }
  return Response.json(messages);
} };`})
	m := newManager(t)
	log := &logs{}
	inst, err := m.Start(context.Background(), core.RuntimeSpec{
		Flat: "fetch-permissions", Version: 1, Dir: dir, Entry: "index.js",
		DataDir: filepath.Join(t.TempDir(), "data"), NetworkOrigins: []string{"http://8.8.8.8"}, Log: log.add,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Stop()
	server := httptest.NewServer(inst)
	defer server.Close()
	check := func() {
		t.Helper()
		response, err := server.Client().Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var messages []string
		if err := json.NewDecoder(response.Body).Decode(&messages); err != nil {
			t.Fatal(err)
		}
		want := []string{egress.ErrInvalid.Error(), egress.ErrInvalid.Error(), egress.ErrDenied.Error(), egress.ErrDenied.Error()}
		if response.StatusCode != 200 || len(messages) != len(want) {
			t.Fatalf("response: %d %#v logs %s", response.StatusCode, messages, log)
		}
		for i, message := range messages {
			if message != want[i] {
				t.Fatalf("request %d: got %q want %q", i, message, want[i])
			}
		}
	}
	check()
	in := inst.(*instance)
	pid := in.pid()
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	log.waitFor(t, "worker exited unexpectedly")
	check()
	if in.pid() == 0 || in.pid() == pid {
		t.Fatalf("worker did not respawn: %d -> %d", pid, in.pid())
	}

}

func TestOutboundFetchCancellationKeepsVMAndWrites(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.js": `let hits = 0;
 export default { async fetch(request, env) {
  let message = "";
  if (new URL(request.url).pathname === "/cancel") {
   try { await fetch("http://8.8.8.8/"); }
   catch (e) { message = e.message; }
  }
  env.DB.exec("CREATE TABLE IF NOT EXISTS writes (id INTEGER)");
  env.DB.exec("INSERT INTO writes VALUES (1)");
  return Response.json({message, count: env.DB.query("SELECT count(*) AS n FROM writes")[0].n, hits: ++hits});
 } };`})
	w := &worker{spec: workerSpec{Dir: dir, Entry: "index.js", DataDir: filepath.Join(t.TempDir(), "data"), NetworkOrigins: []string{"http://8.8.8.8"}}, log: newChildLog(io.Discard)}
	w.data = newDataStore(w.spec.DataDir)
	if err := w.init(); err != nil {
		t.Fatal(err)
	}
	defer func() { w.network.Close(); w.eng.close(); w.data.Close() }()
	original, disconnect := context.WithCancel(context.Background())
	disconnect()
	deadline, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	canceledRequest := context.WithValue(deadline, outboundRequestContextKey{}, original)
	check := func(ctx context.Context, path, wantMessage string, wantCount int) {
		t.Helper()
		input, _ := json.Marshal(reqPayload{Method: "GET", URL: "http://flat.test" + path})
		output, err := w.eng.handle(ctx, input)
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		var response respPayload
		if err := json.Unmarshal(output, &response); err != nil || response.Error != nil {
			t.Fatalf("response: %s %v", output, err)
		}
		var got struct {
			Message     string
			Count, Hits int
		}
		if err := json.Unmarshal([]byte(response.Body), &got); err != nil {
			t.Fatal(err)
		}
		if got.Message != wantMessage || got.Count != wantCount || got.Hits != wantCount {
			t.Fatalf("retained VM/writes: %+v", got)
		}
	}
	check(canceledRequest, "/cancel", egress.ErrCanceled.Error(), 1)
	check(deadline, "/count", "", 2)
	if deadline.Err() != nil {
		t.Fatalf("visitor cancellation destroyed VM deadline: %v", deadline.Err())
	}
}

func TestOutboundContextDisconnectAfterCallStarts(t *testing.T) {
	vmCtx, stopVM := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopVM()
	visitor, disconnect := context.WithCancel(context.Background())
	defer disconnect()
	fetchCtx, stopFetch := outboundContext(context.WithValue(vmCtx, outboundRequestContextKey{}, visitor))
	defer stopFetch()
	if fetchCtx.Err() != nil {
		t.Fatalf("active outbound context: %v", fetchCtx.Err())
	}
	disconnect()
	select {
	case <-fetchCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("disconnect did not cancel outbound context")
	}
	if vmCtx.Err() != nil {
		t.Fatalf("disconnect canceled VM: %v", vmCtx.Err())
	}
}
