package cli

import (
	"context"
	"encoding/json"
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

// TestAgainstRealAPI runs CLI against actual core/API using disposable Local
// listeners. Decisions go through the console API, which the CLI cannot use.
func TestAgainstRealAPI(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	priv, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pubNet, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := core.New(context.Background(), core.Config{DataDir: filepath.Join(dir, "data"), Store: st, Private: priv, Public: local.NewPublic(pubNet), ConsoleURL: func() string { return "http://console.test" }, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&api.Server{Svc: svc}).Handler())
	t.Cleanup(func() { srv.Close(); svc.Close(); priv.Close(); pubNet.Close(); st.Close() })
	operatorCall := func(path, body string) map[string]any {
		t.Helper()
		request, _ := http.NewRequest("POST", srv.URL+"/console/api"+path, strings.NewReader(body))
		request.Header.Set("X-Flats-Console", "1")
		request.Header.Set("Origin", srv.URL)
		request.Header.Set("Sec-Fetch-Site", "same-origin")
		request.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("operator %s: %d %v", path, resp.StatusCode, out)
		}
		return out
	}
	approvePending := func() {
		t.Helper()
		as, err := svc.ListApprovals(context.Background(), "pending")
		if err != nil || len(as) != 1 {
			t.Fatalf("pending request: %v %v", as, err)
		}
		out := operatorCall("/approvals/"+as[0].ID+"/approve", "")
		if out["status"] != "approved" {
			t.Fatalf("decision: %v", out)
		}
	}
	assertState := func(version int, visibility, content string) {
		t.Helper()
		f, err := svc.GetFlat(context.Background(), "demo")
		if err != nil || f.LiveVersion != version || string(f.Visibility) != visibility {
			t.Fatalf("state: %+v %v", f, err)
		}
		if version > 0 {
			resp, err := http.Get(f.PrivateURL)
			if err != nil {
				t.Fatal(err)
			}
			b, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != 200 || string(b) != content {
				t.Fatalf("traffic: %d %q %v", resp.StatusCode, b, err)
			}
		}
	}
	site := t.TempDir()
	writeTree(t, site, map[string]string{"index.html": "<h1>v1</h1>", "about.html": "about", ".DS_Store": "x"})
	r := run(t, srv.URL, "", "deploy", site, "--flat", "demo", "--message", "first")
	if r.code != ExitPending || !strings.Contains(r.stdout, "pending_approval") || strings.Contains(r.stdout, " is live") {
		t.Fatalf("first pending: %+v", r)
	}
	assertState(0, "private", "")
	vs, _ := svc.ListVersions(context.Background(), "demo")
	if len(vs) != 0 {
		t.Fatal("CLI save allocated vN")
	}
	approvePending()
	assertState(1, "private", "<h1>v1</h1>")
	writeTree(t, site, map[string]string{"index.html": "<h1>v2</h1>"})
	if r := run(t, srv.URL, "", "deploy", site, "--flat", "demo", "--expected-revision", "1"); r.code != ExitPending {
		t.Fatalf("second pending: %+v", r)
	}
	assertState(1, "private", "<h1>v1</h1>")
	approvePending()
	assertState(2, "private", "<h1>v2</h1>")
	if r := run(t, srv.URL, "", "versions", "demo"); r.code != 0 || !strings.Contains(r.stdout, "first") {
		t.Fatalf("history: %+v", r)
	}
	writeTree(t, site, map[string]string{"index.html": "DRAFT-3"})
	if r := run(t, srv.URL, "", "draft", "demo", site, "--expected-revision", "2"); r.code != 0 || !strings.Contains(r.stdout, "Private Draft revision 3") {
		t.Fatalf("Draft save: %+v", r)
	}
	assertState(2, "private", "<h1>v2</h1>")
	if r := run(t, srv.URL, "", "draft", "demo", site, "--expected-revision", "2"); r.code != ExitError || !strings.Contains(r.stderr, "revision") {
		t.Fatalf("stale save: %+v", r)
	}
	if r := run(t, srv.URL, "", "draft", "demo"); r.code != 0 || !strings.Contains(r.stdout, "revision 3") {
		t.Fatalf("Draft read: %+v", r)
	}
	if r := run(t, srv.URL, "", "rollback", "demo"); r.code != ExitPending {
		t.Fatalf("rollback pending: %+v", r)
	}
	assertState(2, "private", "<h1>v2</h1>")
	approvePending()
	assertState(1, "private", "<h1>v1</h1>")
	bad := t.TempDir()
	writeTree(t, bad, map[string]string{"flats.json": `{"knd":"static"}`, "index.html": "x"})
	if r := run(t, srv.URL, "", "deploy", bad, "--flat", "demo"); r.code != ExitError || !strings.Contains(r.stderr, "fix:") {
		t.Fatalf("validation: %+v", r)
	}
	if r := run(t, srv.URL, "", "visibility", "demo", "public"); r.code != ExitPending || !strings.Contains(r.stdout, "http://console.test/approvals/apr-") {
		t.Fatalf("unpermitted provider must still request approval: %+v", r)
	}
	assertState(1, "private", "<h1>v1</h1>")
	unpermitted, err := svc.ListApprovals(context.Background(), "pending")
	if err != nil || len(unpermitted) != 1 {
		t.Fatalf("unpermitted pending: %v %v", unpermitted, err)
	}
	operatorCall("/approvals/"+unpermitted[0].ID+"/reject", "")
	operatorCall("/flats/demo/providers", `{"provider":"portal","permitted":true}`)
	if r := run(t, srv.URL, "", "visibility", "demo", "public-unlisted", "--reason", "demo day"); r.code != ExitPending || !strings.Contains(r.stdout, "http://console.test/approvals/apr-") {
		t.Fatalf("public pending: %+v", r)
	}
	assertState(1, "private", "<h1>v1</h1>")
	before, _ := svc.ListApprovals(context.Background(), "pending")
	if r := run(t, srv.URL, "", "approvals", "--status", "pending"); r.code != 0 || !strings.Contains(r.stdout, "set_visibility public") {
		t.Fatalf("approval read: %+v", r)
	}
	after, _ := svc.ListApprovals(context.Background(), "pending")
	if len(before) != len(after) || after[0].Status != "pending" {
		t.Fatal("CLI approval read changed status")
	}
	approvePending()
	assertState(1, "public", "<h1>v1</h1>")
	if r := run(t, srv.URL, "", "visibility", "demo", "private"); r.code != ExitPending {
		t.Fatalf("private pending: %+v", r)
	}
	assertState(1, "public", "<h1>v1</h1>")
	approvePending()
	assertState(1, "private", "<h1>v1</h1>")
	if r := run(t, srv.URL, "", "network", "set", "demo", "https://api.example.com"); r.code != 0 {
		t.Fatalf("network set: %+v", r)
	}
	if r := run(t, srv.URL, "", "network", "ls", "demo"); r.code != 0 || !strings.Contains(r.stdout, "https://api.example.com") || !strings.Contains(r.stdout, "redeploy to revoke") {
		t.Fatalf("network ls: %+v", r)
	}
	if r := run(t, srv.URL, "", "network", "set", "demo"); r.code != 2 {
		t.Fatalf("empty set must require clear: %+v", r)
	}
	if r := run(t, srv.URL, "", "network", "clear", "demo"); r.code != 0 || !strings.Contains(r.stdout, "No server HTTP(S) origins") {
		t.Fatalf("network clear: %+v", r)
	}
	if r := run(t, srv.URL, "hunter2\n", "secret", "set", "demo", "API_KEY"); r.code != 0 {
		t.Fatalf("secret set: %+v", r)
	}
	if r := run(t, srv.URL, "", "secret", "ls", "demo"); !strings.Contains(r.stdout, "API_KEY") || strings.Contains(r.stdout, "hunter2") {
		t.Fatalf("secret read: %+v", r)
	}
	if r := run(t, srv.URL, "", "logs", "demo", "--kind", "deploy"); r.code != 0 || !strings.Contains(r.stdout, "rollback: version 1 is live") {
		t.Fatalf("logs: %+v", r)
	}
	if r := run(t, srv.URL, "", "preview", "demo"); r.code != 0 || !strings.Contains(r.stdout, "Private Draft revision 3") {
		t.Fatalf("Draft preview: %+v", r)
	}
	if r := run(t, srv.URL, "", "preview", "demo", "--version", "2"); r.code != 0 || !strings.Contains(r.stdout, "version 2") {
		t.Fatalf("published preview: %+v", r)
	}
	if r := run(t, srv.URL, "", "--json", "publish", "demo", "--revision", "3"); r.code != ExitPending || !strings.Contains(r.stdout, `"pending_approval"`) {
		t.Fatalf("explicit publish: %+v", r)
	}
	assertState(1, "private", "<h1>v1</h1>")
	approvePending()
	assertState(3, "private", "DRAFT-3")
	if r := run(t, srv.URL, "", "deploy", "--flat", "demo", "--version", "2"); r.code != ExitPending {
		t.Fatalf("existing activation: %+v", r)
	}
	assertState(3, "private", "DRAFT-3")
	approvePending()
	assertState(2, "private", "<h1>v2</h1>")
	if r := run(t, srv.URL, "", "delete", "demo"); r.code != ExitPending {
		t.Fatalf("delete: %+v", r)
	}
	assertState(2, "private", "<h1>v2</h1>")
	if r := run(t, srv.URL, "", "rename", "demo", "demo2"); r.code != 0 || !strings.Contains(r.stdout, "Renamed demo to demo2") {
		t.Fatalf("rename: %+v", r)
	}
	if r := run(t, srv.URL, "", "list"); r.code != 0 || !strings.Contains(r.stdout, "demo2") {
		t.Fatalf("list: %+v", r)
	}
}
