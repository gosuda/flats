package api_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oesni/flats/internal/api"
	"github.com/oesni/flats/internal/core"
	"github.com/oesni/flats/internal/expose/local"
	"github.com/oesni/flats/internal/store"
)

func setup(t *testing.T) (*httptest.Server, *core.Service) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	priv, _ := local.Listen("127.0.0.1:0")
	pubNet, _ := local.Listen("127.0.0.1:0")
	svc, err := core.New(context.Background(), core.Config{DataDir: dir, Store: st, Private: priv, Public: local.NewPublic(pubNet),
		ConsoleURL: func() string { return "http://console" }, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&api.Server{Svc: svc}).Handler())
	t.Cleanup(func() { srv.Close(); svc.Close(); priv.Close(); pubNet.Close(); st.Close() })
	return srv, svc
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
	for k, v := range hdr {
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
	if code != 201 || out["deploy"] == nil {
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

func TestApprovalFlowAndConsoleGuard(t *testing.T) {
	srv, _ := setup(t)
	req(t, "POST", srv.URL+"/api/flats/site/versions?deploy=1", bytes.NewReader(archive(map[string]string{"index.html": "ok"})), nil)
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
	code, out = req(t, "POST", srv.URL+"/console/api/approvals/"+id+"/approve", nil, map[string]string{"X-Flats-Console": "1", "Sec-Fetch-Site": "same-origin"})
	if code != 200 || out["status"] != "approved" {
		t.Fatalf("console approve: %d %v", code, out)
	}
	code, out = req(t, "GET", srv.URL+"/api/flats/site", nil, nil)
	if code != 200 || out["visibility"] != "public-listed" || out["public_notice"] != core.ListedNotice {
		t.Fatalf("after approval: %v", out)
	}
	// Agents cannot create flats from the console.
	if code, _ := req(t, "POST", srv.URL+"/console/api/flats", strings.NewReader(`{"slug":"abc"}`), map[string]string{"X-Flats-Console": "1", "Sec-Fetch-Site": "same-origin"}); code != 405 && code != 404 {
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
