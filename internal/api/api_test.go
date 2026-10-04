package api_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/store"
)

func setup(t *testing.T) (*httptest.Server, *core.Service) {
	srv, svc, _ := setupWithAuthority(t)
	return srv, svc
}

func setupWithAuthority(t *testing.T) (*httptest.Server, *core.Service, *api.OperatorAuthority) {
	t.Helper()
	return setupWithLifecycle(t, nil)
}

func setupWithLifecycle(t *testing.T, lifecycle core.LifecycleNet) (*httptest.Server, *core.Service, *api.OperatorAuthority) {
	t.Helper()
	return setupWith(t, t.TempDir(), func(c *core.Config) { c.Lifecycle = lifecycle })
}

// setupWith serves a Service on data directory dir; edit adjusts its config.
func setupWith(t *testing.T, dir string, edit func(*core.Config)) (*httptest.Server, *core.Service, *api.OperatorAuthority) {
	t.Helper()
	authority := operatorAuthority(t)
	st, err := store.Open(filepath.Join(dir, "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	priv, _ := local.Listen("127.0.0.1:0")
	pubNet, _ := local.Listen("127.0.0.1:0")
	cfg := core.Config{OperatorIdentity: authority.DecisionIdentity, ValidateOperatorDecision: authority.ValidateDecision, DataDir: dir, Store: st, Private: priv, Public: local.NewPublic(pubNet),
		ConsoleURL: func() string { return "http://console" }, Logf: t.Logf}
	edit(&cfg)
	svc, err := core.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&api.Server{Svc: svc, Operator: authority}).Handler())
	t.Cleanup(func() { srv.Close(); svc.Close(); priv.Close(); pubNet.Close(); st.Close() })
	return srv, svc, authority
}

const operatorTestCredential = "independent-fixture-credential-32-bytes"

func operatorAuthority(t *testing.T) *api.OperatorAuthority {
	t.Helper()
	a, err := api.NewOperatorAuthority(operatorTestCredential)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// consoleHdr explicitly authenticates a disposable operator, separately from
// the agent API. Browser headers alone never authorize a decision.
func consoleHdr(t *testing.T, srv *httptest.Server) map[string]string {
	t.Helper()
	h := map[string]string{"X-Flats-Console": "1", "Sec-Fetch-Site": "same-origin", "Origin": srv.URL, "Content-Type": "application/json"}
	r, _ := http.NewRequest("POST", srv.URL+"/console/api/operator/session", strings.NewReader(`{"credential":"`+operatorTestCredential+`"}`))
	for k, v := range h {
		r.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || len(resp.Cookies()) != 1 {
		t.Fatalf("operator fixture login: %d", resp.StatusCode)
	}
	h["Cookie"] = resp.Cookies()[0].String()
	return h
}

func archive(files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for n, b := range files {
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg})
		tw.Write([]byte(b))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func req(t *testing.T, method, url string, body io.Reader, hdr map[string]string) (int, map[string]any) {
	t.Helper()
	r, _ := http.NewRequest(method, url, body)
	if !strings.Contains(url, "/console/api/") {
		// Agent API mutations need a client header (CSRF protection).
		r.Header.Set("X-Flats-Client", "test")
	}
	for k, v := range hdr {
		if v == "" {
			r.Header.Del(k)
			continue
		}
		r.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestUploadValidationAndDeploy(t *testing.T) {
	srv, _ := setup(t)
	code, out := req(t, "POST", srv.URL+"/api/flats/site/versions", bytes.NewReader(archive(map[string]string{"app.js": "x"})), nil)
	if code != 422 || out["problems"] == nil {
		t.Fatalf("want 422 with problems, got %d %v", code, out)
	}
	code, out = req(t, "POST", srv.URL+"/api/flats/site/versions?deploy=1&git_sha=abc&git_dirty=true", bytes.NewReader(archive(map[string]string{"index.html": "ok"})), nil)
	if code != 202 || out["status"] != "pending_approval" || out["deploy"] == nil {
		t.Fatalf("upload+deploy: %d %v", code, out)
	}
	v := out["version"].(map[string]any)
	if v["git_sha"] != "abc" || v["git_dirty"] != true {
		t.Fatalf("git metadata lost: %v", v)
	}
	code, _ = req(t, "POST", srv.URL+"/api/flats/site/deploy", strings.NewReader(`{"version":99}`), nil)
	if code != 404 {
		t.Fatalf("deploy of missing version = %d", code)
	}
	code, _ = req(t, "POST", srv.URL+"/api/flats/site/deploy", strings.NewReader(`{"bogus":1}`), nil)
	if code != 400 {
		t.Fatalf("unknown field = %d", code)
	}
}

func saveAndPublish(t *testing.T, srv *httptest.Server, slug, content string) {
	t.Helper()
	code, out := req(t, "POST", srv.URL+"/api/flats/"+slug+"/versions?deploy=1", bytes.NewReader(archive(map[string]string{"index.html": content})), nil)
	if code != 202 || out["status"] != "pending_approval" {
		t.Fatalf("publish request: %d %v", code, out)
	}
	approveRequest(t, srv, out)
}

func approveRequest(t *testing.T, srv *httptest.Server, pending map[string]any) map[string]any {
	t.Helper()
	id := pending["approval"].(map[string]any)["id"].(string)
	code, out := req(t, "POST", srv.URL+"/console/api/approvals/"+id+"/approve", nil, consoleHdr(t, srv))
	if code != 200 || out["status"] != "approved" {
		t.Fatalf("authorized decision: %d %v", code, out)
	}
	return out
}

func TestApprovalFlowAndConsoleGuard(t *testing.T) {
	srv, _ := setup(t)
	saveAndPublish(t, srv, "site", "ok")
	if code, out := req(t, "POST", srv.URL+"/console/api/flats/site/providers", strings.NewReader(`{"provider":"portal","permitted":true}`), consoleHdr(t, srv)); code != 200 {
		t.Fatalf("operator provider grant: %d %v", code, out)
	}
	code, out := req(t, "POST", srv.URL+"/api/flats/site/visibility", strings.NewReader(`{"visibility":"public-listed","reason":"launch"}`), nil)
	if code != 202 || out["status"] != "pending_approval" || !strings.HasPrefix(out["approval_url"].(string), "http://console/approvals/") {
		t.Fatalf("pending approval: %d %v", code, out)
	}
	id := out["approval"].(map[string]any)["id"].(string)
	// Agents cannot reach the approve endpoint on the agent API ...
	if code, _ := req(t, "POST", srv.URL+"/api/approvals/"+id+"/approve", nil, nil); code != 404 && code != 405 {
		t.Fatalf("agent approve endpoint exists: %d", code)
	}
	// ... nor on the console API without browser headers.
	if code, _ := req(t, "POST", srv.URL+"/console/api/approvals/"+id+"/approve", nil, map[string]string{"X-Flats-Console": "1"}); code != 403 {
		t.Fatalf("console approve without Sec-Fetch-Site = %d", code)
	}
	code, out = req(t, "POST", srv.URL+"/console/api/approvals/"+id+"/approve", nil, consoleHdr(t, srv))
	if code != 200 || out["status"] != "approved" {
		t.Fatalf("console approve: %d %v", code, out)
	}
	code, out = req(t, "GET", srv.URL+"/api/flats/site", nil, nil)
	if code != 200 || out["visibility"] != "public" || out["public_notice"] != core.PublicAccessNotice {
		t.Fatalf("after approval: %v", out)
	}
	// Agents cannot create flats from the console.
	if code, _ := req(t, "POST", srv.URL+"/console/api/flats", strings.NewReader(`{"slug":"abc"}`), consoleHdr(t, srv)); code != 405 && code != 404 {
		t.Fatalf("console create = %d", code)
	}
}

func TestSecretsOperatorOnlyAndNeverReturned(t *testing.T) {
	srv, _ := setup(t)
	req(t, "POST", srv.URL+"/api/flats/site/versions", bytes.NewReader(archive(map[string]string{"index.html": "ok"})), nil)
	if code, _ := req(t, "PUT", srv.URL+"/api/flats/site/secrets/API_KEY", strings.NewReader(`{"value":"s3cr3t"}`), nil); code != 403 {
		t.Fatalf("agent set secret = %d", code)
	}
	// The CLI on the host (loopback) may set it.
	if code, out := req(t, "PUT", srv.URL+"/api/flats/site/secrets/API_KEY", strings.NewReader(`{"value":"s3cr3t"}`), map[string]string{"X-Flats-Client": "cli"}); code != 200 {
		t.Fatalf("cli set secret = %d %v", code, out)
	}
	resp, _ := http.Get(srv.URL + "/api/flats/site/secrets")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "API_KEY") || strings.Contains(string(b), "s3cr3t") {
		t.Fatalf("secret listing leaked or missing: %s", b)
	}
	resp, _ = http.Get(srv.URL + "/api/flats/site/logs")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(b), "s3cr3t") {
		t.Fatal("secret value leaked into logs")
	}
}

// A web page in the operator's browser must not drive the agent API: it can
// only send "simple" requests (text/plain, no custom headers) without a
// preflight, and the browser marks them with Sec-Fetch-Site and Origin.
func TestAgentAPIRefusesCrossSiteRequests(t *testing.T) {
	srv, _ := setup(t)
	if code, out := req(t, "POST", srv.URL+"/api/flats/site/versions?deploy=1", bytes.NewReader(archive(map[string]string{"index.html": "legit"})), nil); code != 202 {
		t.Fatalf("setup upload: %d %v", code, out)
	}
	deface := func() io.Reader { return bytes.NewReader(archive(map[string]string{"index.html": "defaced"})) }
	noClient := map[string]string{"X-Flats-Client": "", "Content-Type": "text/plain"}
	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"cross-site", map[string]string{"X-Flats-Client": "", "Content-Type": "text/plain", "Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, 403},
		{"same-site flat page", map[string]string{"X-Flats-Client": "", "Content-Type": "text/plain", "Sec-Fetch-Site": "same-site"}, 403},
		{"foreign origin, old browser", map[string]string{"X-Flats-Client": "", "Content-Type": "text/plain", "Origin": "https://evil.example"}, 403},
		{"simple request without client header", noClient, 415},
		{"form post", map[string]string{"X-Flats-Client": "", "Content-Type": "application/x-www-form-urlencoded"}, 415},
	}
	for _, c := range cases {
		if code, out := req(t, "POST", srv.URL+"/api/flats/site/versions?deploy=1", deface(), c.hdr); code != c.want {
			t.Errorf("%s: upload = %d %v, want %d", c.name, code, out, c.want)
		}
	}
	if code, _ := req(t, "POST", srv.URL+"/api/flats/site/rename", strings.NewReader(`{"slug":"pwned"}`), noClient); code != 415 {
		t.Errorf("text/plain rename = %d", code)
	}
	if code, _ := req(t, "POST", srv.URL+"/api/flats", strings.NewReader(`{"slug":"spam"}`), noClient); code != 415 {
		t.Errorf("text/plain create = %d", code)
	}
	if code, out := req(t, "GET", srv.URL+"/api/flats/site", nil, nil); code != 200 || out["slug"] != "site" {
		t.Fatalf("flat changed by a refused request: %d %v", code, out)
	}
	if code, out := req(t, "GET", srv.URL+"/api/flats/site/versions", nil, map[string]string{"Sec-Fetch-Site": "same-site"}); code != 403 {
		t.Errorf("same-site read = %d %v", code, out)
	}

	// Real clients: an archive or JSON Content-Type works without a client
	// header, and the same-origin console origin is fine.
	if code, out := req(t, "POST", srv.URL+"/api/flats/site/versions", deface(), map[string]string{"X-Flats-Client": "", "Content-Type": "application/gzip"}); code != 201 {
		t.Errorf("archive upload without client header = %d %v", code, out)
	}
	if code, out := req(t, "POST", srv.URL+"/api/flats", strings.NewReader(`{"slug":"other"}`), map[string]string{"X-Flats-Client": "", "Content-Type": "application/json; charset=utf-8", "Origin": srv.URL}); code != 201 {
		t.Errorf("JSON create = %d %v", code, out)
	}
	if code, out := req(t, "DELETE", srv.URL+"/api/flats/other?reason=x", nil, map[string]string{"X-Flats-Client": "cli"}); code != 202 {
		t.Errorf("CLI delete request = %d %v", code, out)
	}
}

// The console API refuses requests that identify as the CLI or another
// client, and mutations without a same-origin browser Origin.
func TestConsoleRefusesClientsAndForeignOrigins(t *testing.T) {
	srv, _ := setup(t)
	req(t, "POST", srv.URL+"/api/flats/site/versions?deploy=1", bytes.NewReader(archive(map[string]string{"index.html": "ok"})), nil)
	_, out := req(t, "DELETE", srv.URL+"/api/flats/site?reason=x", nil, nil)
	id := out["approval"].(map[string]any)["id"].(string)
	approve := srv.URL + "/console/api/approvals/" + id + "/approve"
	with := func(k, v string) map[string]string {
		h := consoleHdr(t, srv)
		h[k] = v
		return h
	}
	for name, h := range map[string]map[string]string{
		"cli header":     with("X-Flats-Client", "cli"),
		"any client":     with("X-Flats-Client", "agent"),
		"no origin":      with("Origin", ""),
		"foreign origin": with("Origin", "http://attacker.example:7878"),
		"no fetch site":  with("Sec-Fetch-Site", ""),
	} {
		if code, out := req(t, "POST", approve, nil, h); code != 403 {
			t.Errorf("%s: approve = %d %v", name, code, out)
		}
	}
	if code, _ := req(t, "GET", srv.URL+"/console/api/settings", nil, map[string]string{"X-Flats-Console": "1", "X-Flats-Client": "cli"}); code != 403 {
		t.Errorf("console read with client header = %d", code)
	}
	if code, out := req(t, "GET", srv.URL+"/api/approvals/"+id, nil, nil); code != 200 || out["status"] != "pending" {
		t.Fatalf("approval decided by a refused request: %d %v", code, out)
	}
}

// Decisions made through the console node carry the WhoIs login that the
// tsnet middleware set; the loopback listener has none.
func TestDecisionRecordsTailnetLogin(t *testing.T) {
	srv, svc, authority := setupWithAuthority(t)
	tail := httptest.NewServer(api.TailnetIdentity((&api.Server{Svc: svc, Operator: authority}).Handler()))
	t.Cleanup(tail.Close)
	req(t, "POST", srv.URL+"/api/flats/site/versions?deploy=1", bytes.NewReader(archive(map[string]string{"index.html": "ok"})), nil)
	pending := func() string {
		_, out := req(t, "DELETE", srv.URL+"/api/flats/site", nil, nil)
		return out["approval"].(map[string]any)["id"].(string)
	}

	id := pending()
	h := consoleHdr(t, tail)
	h["Tailscale-User-Login"] = "=?utf-8?q?j=C3=BCrgen@example.com?="
	code, out := req(t, "POST", tail.URL+"/console/api/approvals/"+id+"/reject", nil, h)
	if code != 200 || out["decided_by"] != "jürgen@example.com" || out["status"] != "rejected" {
		t.Fatalf("reject via console node: %d %v", code, out)
	}
	_, stored := req(t, "GET", srv.URL+"/api/approvals/"+id, nil, nil)
	if stored["decided_by"] != "jürgen@example.com" || stored["authorized_at"] == nil {
		t.Fatalf("rejection actor not persisted: %v", stored)
	}
	_, logs := req(t, "GET", srv.URL+"/api/flats/site/logs?kind=approval", nil, nil)
	b, _ := json.Marshal(logs)
	if !strings.Contains(string(b), "tailnet user jürgen@example.com") || !strings.Contains(string(b), id) {
		t.Fatalf("approval event lacks the approver: %s", b)
	}

	id = pending()
	code, out = req(t, "POST", srv.URL+"/console/api/approvals/"+id+"/approve", nil, consoleHdr(t, srv))
	if code != 200 || out["decided_by"] != "local operator" {
		t.Fatalf("loopback approve: %d %v", code, out)
	}
}

func TestSettingsReportRestartRequired(t *testing.T) {
	srv, _ := setup(t)
	code, out := req(t, "GET", srv.URL+"/console/api/settings", nil, map[string]string{"X-Flats-Console": "1"})
	if code != 200 || !strings.Contains(fmt.Sprint(out["apply_on_restart"]), "portal_relays") {
		t.Fatalf("get settings: %d %v", code, out)
	}
	code, out = req(t, "PUT", srv.URL+"/console/api/settings", strings.NewReader(`{"portal_relays":"https://relay.example"}`), consoleHdr(t, srv))
	if code != 200 || fmt.Sprint(out["restart_required"]) != "[portal_relays]" || out["note"] == nil {
		t.Fatalf("relay change: %d %v", code, out)
	}
	code, out = req(t, "PUT", srv.URL+"/console/api/settings", strings.NewReader(`{"keep_versions":"5","portal_relays":"https://relay.example"}`), consoleHdr(t, srv))
	if code != 200 || fmt.Sprint(out["restart_required"]) != "[]" {
		t.Fatalf("unchanged relays: %d %v", code, out)
	}
}

func TestHostGuard(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	loop := &api.HostGuard{Hosts: func() []string { return []string{"127.0.0.1", "localhost", "::1"} }, Ports: []string{"7878"}, Next: next}
	tailnet := "flats.tail1234.ts.net"
	node := &api.HostGuard{Hosts: func() []string { return []string{"flats", tailnet} }, Ports: []string{"", "80", "443"}, Next: next}
	cases := []struct {
		g            *api.HostGuard
		method, host string
		origin       string
		want         int
	}{
		{loop, "GET", "127.0.0.1:7878", "", 204},
		{loop, "GET", "localhost:7878", "", 204},
		{loop, "GET", "LOCALHOST:7878", "", 204},
		{loop, "GET", "[::1]:7878", "", 204},
		{loop, "POST", "127.0.0.1:7878", "http://localhost:7878", 204},
		{loop, "GET", "attacker.example:7878", "", 403}, // DNS rebinding
		{loop, "POST", "attacker.example:7878", "http://attacker.example:7878", 403},
		{loop, "GET", "127.0.0.1:7879", "", 403},
		{loop, "GET", "127.0.0.1", "", 403},
		{loop, "GET", "", "", 403},
		{loop, "POST", "127.0.0.1:7878", "https://evil.example", 403},
		{loop, "POST", "127.0.0.1:7878", "null", 403},
		{loop, "GET", "127.0.0.1:7878", "https://evil.example", 403},
		{node, "GET", tailnet, "", 204},
		{node, "GET", tailnet + ".", "", 204},
		{node, "GET", "flats", "", 204},
		{node, "GET", "flats:80", "", 204},
		{node, "POST", tailnet, "https://" + tailnet, 204},
		{node, "POST", tailnet, "https://blog.tail1234.ts.net", 403},
		{node, "GET", "rebind.evil.example", "", 403},
		{node, "GET", "100.64.0.1", "", 403},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "/console/api/approvals", nil)
		r.Host = c.host
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		w := httptest.NewRecorder()
		c.g.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s Host=%q Origin=%q: %d %s, want %d", c.method, c.host, c.origin, w.Code, w.Body, c.want)
		}
	}
}
