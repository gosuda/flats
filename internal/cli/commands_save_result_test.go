package cli

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/store"
)

func publishedSaveServer(t *testing.T) (*httptest.Server, *core.Service) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	priv, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { priv.Close() })
	pub, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pub.Close() })
	svc, err := core.New(t.Context(), core.Config{DataDir: dir, Store: st, Private: priv, Public: local.NewPublic(pub), Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })
	if _, err := svc.SetPermittedNetworks([]string{"portal"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateSettings(t.Context(), map[string]string{core.SetDefaultPublic: "portal"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetPublicInstance(t.Context(), true, "portal", "test"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&api.Server{Svc: svc}).Handler())
	t.Cleanup(srv.Close)
	return srv, svc
}

func TestPublishedSaveCLI(t *testing.T) {
	for _, kind := range []string{"website", "page"} {
		t.Run(kind, func(t *testing.T) {
			srv, svc := publishedSaveServer(t)
			dir := t.TempDir()
			files := map[string]string{"index.html": "<title>Published</title>"}
			if kind == "page" {
				files["flats.json"] = `{"type":"page"}`
			}
			writeTree(t, dir, files)
			r := run(t, srv.URL, "", "save-draft", "published", dir)
			f, err := svc.GetFlat(t.Context(), "published")
			if err != nil || r.code != ExitOK || !strings.Contains(r.stdout, "published as version 1") || f.PublicURL == "" || !strings.Contains(r.stdout, f.PublicURL) || !strings.Contains(r.stdout, f.PublicNotice) || strings.Contains(r.stdout, "approval") || strings.Contains(r.stdout, "Current version is unchanged") {
				t.Fatalf("published save: %+v %+v %v", r, f, err)
			}
			r = run(t, srv.URL, "", "--json", "save-draft", "published", dir)
			if r.code != ExitOK || !strings.Contains(r.stdout, `"version": 2`) || !strings.Contains(r.stdout, `"public_notice"`) {
				t.Fatalf("published JSON: %+v", r)
			}
			quota := 2
			if err := svc.SetVersionQuotaOverride(t.Context(), "published", &quota); err != nil {
				t.Fatal(err)
			}
			for _, jsonOut := range []bool{false, true} {
				args := []string{"save-draft", "published", dir}
				if jsonOut {
					args = append(args, "--json")
				}
				r = run(t, srv.URL, "", args...)
				text := r.stderr
				if jsonOut {
					text = r.stdout
				}
				if r.code != ExitError || !strings.Contains(text, "was saved but not published") || !strings.Contains(text, "Draft was kept") || !strings.Contains(text, "version quota") || strings.Contains(r.stdout, "published as version") {
					t.Fatalf("failed save: %+v", r)
				}
			}
			if _, err := svc.SetPublicInstance(t.Context(), false, "", "test"); err != nil {
				t.Fatal(err)
			}
			r = run(t, srv.URL, "", "save-draft", "published", dir)
			if r.code != ExitOK || !strings.Contains(r.stdout, "Saved Private Draft revision 5 of published:") || !strings.Contains(r.stdout, "Current version is unchanged. Request approval for this saved content with `flats create-version published --revision 5 --hash ") || strings.Contains(r.stdout, "published as version") {
				t.Fatalf("off save changed: %+v", r)
			}
			f, err = svc.GetFlat(t.Context(), "published")
			if err != nil || f.LiveVersion != 2 || f.Versions != 2 || f.Draft.Revision != 5 {
				t.Fatalf("failed/off saves changed live version or lost Draft: %+v %v", f, err)
			}
		})
	}
}

func TestPublishedSaveCLINotOpen(t *testing.T) {
	for _, tc := range []struct {
		name string
		flat string
		want string
	}{
		{"no route", `{"connection_state":"unavailable","public_notice":"Public notice"}`, "the public link is not open yet (unavailable)"},
		{"pending URL", `{"public_url":"https://pending.example","connection_state":"starting","public_notice":"Public notice","endpoints":[{"provider":"portal","audience":"current","host":"site","state":"starting","detail":"waiting for relay"}]}`, "the public link is not open yet (starting: waiting for relay)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := newFakeAPI(t)
			f.handle("POST /api/flats/site/draft", 201, `{"draft":{"revision":1,"hash":"abc"},"version":1,"flat":`+tc.flat+`}`)
			dir := t.TempDir()
			writeTree(t, dir, map[string]string{"index.html": "<title>Site</title>"})
			r := run(t, srv.URL, "", "save-draft", "site", dir)
			if r.code != ExitOK || !strings.Contains(r.stdout, "published as version 1") || !strings.Contains(r.stdout, tc.want) || !strings.Contains(r.stdout, "Public notice") || strings.Contains(r.stdout, "approval") {
				t.Fatalf("not-open publication: %+v", r)
			}
		})
	}
}

func TestPublishedSaveCLIDeploymentFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause *core.DeployError
		want  string
	}{
		{"health", &core.DeployError{Version: 2, Previous: 1, Health: core.HealthResult{Path: "/healthz", Status: 500, BodyHead: "not ready"}}, "GET /healthz -> 500"},
		{"start", &core.DeployError{Version: 2, Previous: 1, Cause: errors.New("worker could not start"), Data: "Live runtime startup may have changed data."}, "Live runtime startup may have changed data."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := newFakeAPI(t)
			response := map[string]any{"error": (&core.SavedDraftError{Revision: 2, Cause: tc.cause}).Error()}
			if tc.cause.Cause == nil {
				response["health"] = tc.cause.Health
			}
			body, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			f.handle("POST /api/flats/site/draft", 422, string(body))
			dir := t.TempDir()
			writeTree(t, dir, map[string]string{"index.html": "<title>Site</title>"})
			r := run(t, srv.URL, "", "save-draft", "site", dir)
			if r.code != ExitError || !strings.Contains(r.stderr, "Draft revision 2 was saved but not published") || !strings.Contains(r.stderr, tc.want) || strings.Contains(r.stdout, "published as version") {
				t.Fatalf("failed publication: %+v", r)
			}
		})
	}
}
