package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/egress"
	"github.com/gosuda/flats/internal/ostest"
)

// The runtime checks of docs/spec/injected-fetch-secrets.md. The outbound client adds the header: its own tests prove
// that with local TLS servers. These tests prove what the worker gives to that client and to flat code.

const runtimeInjected = "Bearer injected-runtime-9c2e"

const injectProbe = `export default { async fetch(request, env) {
  let message = "";
  try { await fetch("https://8.8.8.8/v1", {headers: {authorization: "Bearer flat-code"}}); message = "allowed"; }
  catch (e) { message = e.message; }
  return Response.json({readable: env.READABLE, injected: typeof env.INJECTED, keys: Object.keys(env), dump: JSON.stringify(env), message});
} };`

type injectProbeResult struct {
	Readable, Injected, Dump, Message string
	Keys                              []string
}

// A real worker of the Manager with the product outbound client: flat code does not see the value, and the client
// has the injected list, because a call that supplies the bound header fails with the error of rule 15 before any
// connection. A replaced worker gets the same list.
func TestInjectedSecretThroughTheManager(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.js": injectProbe})
	m := newManager(t)
	log := &logs{}
	inst, err := m.Start(context.Background(), core.RuntimeSpec{
		Flat: "inject", Version: 1, Dir: dir, Entry: "index.js", DataDir: filepath.Join(t.TempDir(), "data"),
		Env:            map[string]string{"READABLE": "readable-fixture"},
		NetworkOrigins: []string{"https://8.8.8.8"},
		FetchHeaders:   []egress.Header{{Origin: "https://8.8.8.8", Name: "Authorization", Value: runtimeInjected}},
		Log:            log.add,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Stop()
	server := httptest.NewServer(inst)
	defer server.Close()
	want := (&egress.HostHeaderError{Name: "Authorization"}).Error()
	check := func() {
		t.Helper()
		response, err := server.Client().Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var got injectProbeResult
		if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.Readable != "readable-fixture" || got.Injected != "undefined" || got.Message != want {
			t.Fatalf("probe: %+v logs %s", got, log)
		}
		if strings.Contains(got.Dump, "injected-runtime") || slices.ContainsFunc(got.Keys, func(k string) bool { return strings.Contains(k, "INJECT") }) {
			t.Fatalf("env holds the injected secret: %+v", got)
		}
	}
	check()
	in := inst.(*instance)
	pid := in.pid()
	if err := ostest.Kill(pid); err != nil {
		t.Fatal(err)
	}
	log.waitFor(t, "worker exited unexpectedly")
	check()
	if in.pid() == 0 || in.pid() == pid {
		t.Fatalf("worker did not respawn: %d -> %d", pid, in.pid())
	}
	if log.find("injected-runtime") {
		t.Fatal("a log line holds the value")
	}
}

type recordedOutbound struct {
	mu       sync.Mutex
	origins  []string
	headers  []egress.Header
	requests []egress.Request
}

func (r *recordedOutbound) Fetch(_ context.Context, req egress.Request) (egress.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req)
	return egress.Response{Status: 200, Body: []byte("{}"), URL: req.URL}, nil
}

func (*recordedOutbound) Close() {}

// The worker gives the injected list of its spec, and nothing else, to the function that builds its outbound client,
// and the request of flat code does not hold the value.
func TestWorkerGivesItsInjectedListToTheOutboundClient(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.js": `export default { async fetch(request, env) {
  await fetch("https://api.example.test/v1", {headers: {"x-trace": "1"}});
  return Response.json({dump: JSON.stringify(env)});
} };`})
	list := []egress.Header{{Origin: "https://api.example.test", Name: "Authorization", Value: runtimeInjected}}
	rec := &recordedOutbound{}
	w := &worker{
		spec: workerSpec{Dir: dir, Entry: "index.js", DataDir: filepath.Join(t.TempDir(), "data"), Env: map[string]string{"READABLE": "r"},
			NetworkOrigins: []string{"https://api.example.test"}, FetchHeaders: list},
		log: newChildLog(io.Discard),
		newOutbound: func(origins []string, headers []egress.Header) (outboundClient, error) {
			rec.origins, rec.headers = slices.Clone(origins), slices.Clone(headers)
			return rec, nil
		},
	}
	w.data = newDataStore(w.spec.DataDir)
	if err := w.init(); err != nil {
		t.Fatal(err)
	}
	defer func() { w.eng.close(); w.data.Close() }()
	input, _ := json.Marshal(reqPayload{Method: "GET", URL: "http://flat.test/"})
	output, err := w.eng.handle(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rec.origins, []string{"https://api.example.test"}) || !slices.Equal(rec.headers, list) {
		t.Fatalf("outbound client built with %v %v", rec.origins, rec.headers)
	}
	if len(rec.requests) != 1 {
		t.Fatalf("requests: %+v", rec.requests)
	}
	raw, _ := json.Marshal(rec.requests[0])
	if strings.Contains(string(raw), "injected-runtime") || strings.Contains(string(output), "injected-runtime") {
		t.Fatalf("flat code held the value: request %s output %s", raw, output)
	}
}

// The spec on stdin holds the injected secrets in their own field, never in env.
func TestWorkerSpecKeepsInjectedSecretsOutOfEnv(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(workerSpec{Env: map[string]string{"READABLE": "r"},
		FetchHeaders: []egress.Header{{Origin: "https://api.example.test", Name: "Authorization", Value: runtimeInjected}}})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Env          map[string]string `json:"env"`
		FetchHeaders []egress.Header   `json:"fetch_headers"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Env) != 1 || got.Env["READABLE"] != "r" || len(got.FetchHeaders) != 1 || got.FetchHeaders[0].Value != runtimeInjected {
		t.Fatalf("spec: %s", b)
	}
}
