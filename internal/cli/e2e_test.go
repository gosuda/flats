package cli

import (
	"context"
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

// TestAgainstRealAPI runs the CLI against the real API server and core, with
// the loopback network standing in for Tailscale and Portal.
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
	svc, err := core.New(context.Background(), core.Config{DataDir: filepath.Join(dir, "data"), Store: st, Private: priv,
		Public: local.NewPublic(pubNet), ConsoleURL: func() string { return "http://console.test" }, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&api.Server{Svc: svc}).Handler())
	t.Cleanup(func() { srv.Close(); svc.Close(); priv.Close(); pubNet.Close(); st.Close() })

	site := t.TempDir()
	writeTree(t, site, map[string]string{"index.html": "<h1>v1</h1>", "about.html": "about", ".DS_Store": "x"})

	r := run(t, srv.URL, "", "deploy", site, "--flat", "demo", "--message", "first")
	if r.code != 0 {
		t.Fatalf("deploy: %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, "Saved version 1 of demo: 2 files") || !strings.Contains(r.stdout, "Version 1 is live.") {
		t.Fatalf("deploy output:\n%s", r.stdout)
	}
	// The flat really serves the upload.
	f, err := svc.GetFlat(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(f.PrivateURL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "<h1>v1</h1>" {
		t.Fatalf("served %q from %s", body, f.PrivateURL)
	}

	writeTree(t, site, map[string]string{"index.html": "<h1>v2</h1>"})
	if r := run(t, srv.URL, "", "deploy", site, "--flat", "demo"); r.code != 0 || !strings.Contains(r.stdout, "Version 2 is live (was 1).") {
		t.Fatalf("second deploy: %d %s %s", r.code, r.stdout, r.stderr)
	}
	if r := run(t, srv.URL, "", "versions", "demo"); r.code != 0 || !strings.Contains(r.stdout, "first") {
		t.Fatalf("versions: %s %s", r.stdout, r.stderr)
	}
	if r := run(t, srv.URL, "", "rollback", "demo"); r.code != 0 || !strings.Contains(r.stdout, "Version 1 is live (was 2).") {
		t.Fatalf("rollback: %d %s %s", r.code, r.stdout, r.stderr)
	}

	// Validation errors come back with their fixes and exit 1.
	bad := t.TempDir()
	writeTree(t, bad, map[string]string{"flats.json": `{"knd": "static"}`, "index.html": "x"})
	r = run(t, srv.URL, "", "deploy", bad, "--flat", "demo")
	if r.code != ExitError || !strings.Contains(r.stderr, "fix:") {
		t.Fatalf("invalid manifest: %d\n%s", r.code, r.stderr)
	}

	// Widening exposure from the CLI waits for the operator.
	r = run(t, srv.URL, "", "visibility", "demo", "public-unlisted", "--reason", "demo day")
	if r.code != ExitPending || !strings.Contains(r.stdout, "http://console.test/approvals/apr-") || !strings.Contains(r.stdout, core.UnlistedNotice) {
		t.Fatalf("visibility: %d %s %s", r.code, r.stdout, r.stderr)
	}
	if r := run(t, srv.URL, "", "approvals", "--status", "pending"); r.code != 0 || !strings.Contains(r.stdout, "set_visibility public-unlisted") {
		t.Fatalf("approvals: %s %s", r.stdout, r.stderr)
	}
	if r := run(t, srv.URL, "", "delete", "demo"); r.code != ExitPending {
		t.Fatalf("delete: %d %s", r.code, r.stderr)
	}

	// The test server is on loopback, so the CLI may set secrets.
	if r := run(t, srv.URL, "hunter2\n", "secret", "set", "demo", "API_KEY"); r.code != 0 {
		t.Fatalf("secret set: %d %s", r.code, r.stderr)
	}
	if r := run(t, srv.URL, "", "secret", "ls", "demo"); !strings.Contains(r.stdout, "API_KEY") || strings.Contains(r.stdout, "hunter2") {
		t.Fatalf("secret ls: %s", r.stdout)
	}

	if r := run(t, srv.URL, "", "logs", "demo", "--kind", "deploy"); r.code != 0 || !strings.Contains(r.stdout, "rollback: version 1 is live") {
		t.Fatalf("logs: %s %s", r.stdout, r.stderr)
	}
	if r := run(t, srv.URL, "", "preview", "demo"); r.code != 0 || !strings.Contains(r.stdout, "Preview of demo version 2") {
		t.Fatalf("preview: %d %s %s", r.code, r.stdout, r.stderr)
	}
	if r := run(t, srv.URL, "", "rename", "demo", "demo2"); r.code != 0 || !strings.Contains(r.stdout, "Renamed demo to demo2") {
		t.Fatalf("rename: %d %s %s", r.code, r.stdout, r.stderr)
	}
	if r := run(t, srv.URL, "", "list"); r.code != 0 || !strings.Contains(r.stdout, "demo2") {
		t.Fatalf("list: %s %s", r.stdout, r.stderr)
	}
}
