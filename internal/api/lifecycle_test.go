package api_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func flatState(t *testing.T, srv *httptest.Server, slug string, version int, visibility string, revision int) map[string]any {
	t.Helper()
	code, out := req(t, "GET", srv.URL+"/api/flats/"+slug, nil, nil)
	if code != 200 || out["live_version"] != float64(version) || out["visibility"] != visibility {
		t.Fatalf("state: %d %v; want v%d %s", code, out, version, visibility)
	}
	if revision > 0 && out["draft"].(map[string]any)["revision"] != float64(revision) {
		t.Fatalf("Draft revision: %v", out)
	}
	publication := "published"
	if version == 0 {
		publication = "unpublished"
	}
	if out["publication"] != publication {
		t.Fatalf("publication: %v", out)
	}
	return out
}

func serving(t *testing.T, url, content string) {
	t.Helper()
	r, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil || r.StatusCode != 200 || string(b) != content {
		t.Fatalf("traffic: %d %q %v", r.StatusCode, b, err)
	}
}

func TestDraftRevisionConflictPublishAndServingIsolation(t *testing.T) {
	srv, _ := setup(t)
	for revision := 1; revision <= 3; revision++ {
		code, out := req(t, "POST", fmt.Sprintf("%s/api/flats/drafts/draft?expected_revision=%d", srv.URL, revision-1), bytes.NewReader(archive(map[string]string{"index.html": fmt.Sprintf("DRAFT-%d", revision)})), nil)
		if code != 201 {
			t.Fatalf("save: %d %v", code, out)
		}
		v := out["version"].(map[string]any)
		if v["number"] != float64(0) || v["published"] != false || v["role"] != "draft" || v["revision"] != float64(revision) {
			t.Fatalf("compatibility Draft: %v", v)
		}
		flatState(t, srv, "drafts", 0, "private", revision)
	}
	for _, query := range []string{"expected_revision=-1", "expected_revision=unknown", "expected_revision=1&expected_revision=2"} {
		code, denied := req(t, "POST", srv.URL+"/api/flats/drafts/draft?"+query, bytes.NewReader(archive(map[string]string{"index.html": "OVERWRITE"})), nil)
		if code != 400 {
			t.Fatalf("invalid revision query: %d %v", code, denied)
		}
		flatState(t, srv, "drafts", 0, "private", 3)
	}
	code, denied := req(t, "POST", srv.URL+"/api/flats/drafts/draft?expected_revision=3", bytes.NewReader(archive(map[string]string{"app.js": "no entry"})), nil)
	if code != 422 || denied["problems"] == nil {
		t.Fatalf("invalid Draft validation: %d %v", code, denied)
	}
	flatState(t, srv, "drafts", 0, "private", 3)
	code, out := req(t, "GET", srv.URL+"/api/flats/drafts/versions", nil, nil)
	if code != 200 || len(out["versions"].([]any)) != 0 {
		t.Fatalf("save allocated published versions: %v", out)
	}
	for _, expected := range []string{"1", "99"} {
		code, out = req(t, "POST", srv.URL+"/api/flats/drafts/draft?expected_revision="+expected, bytes.NewReader(archive(map[string]string{"index.html": "OVERWRITE"})), nil)
		if code != 409 || out["category"] != "conflict" {
			t.Fatalf("stale/unknown revision: %d %v", code, out)
		}
		flatState(t, srv, "drafts", 0, "private", 3)
	}
	code, out = req(t, "POST", srv.URL+"/api/flats/drafts/visibility", strings.NewReader(`{"visibility":"public"}`), nil)
	if code != 409 {
		t.Fatalf("unpublished Public: %d %v", code, out)
	}
	code, out = req(t, "POST", srv.URL+"/api/flats/drafts/previews", strings.NewReader(`{"target":"draft","version":0}`), nil)
	if code != 201 || out["target"] != "draft" || out["revision"] != float64(3) {
		t.Fatalf("Draft preview: %d %v", code, out)
	}
	serving(t, out["url"].(string), "DRAFT-3")
	_, draft := req(t, "GET", srv.URL+"/api/flats/drafts/draft", nil, nil)
	code, out = req(t, "POST", srv.URL+"/api/flats/drafts/publish", strings.NewReader(fmt.Sprintf(`{"revision":3,"hash":%q}`, draft["hash"])), nil)
	if code != 202 || out["status"] != "pending_approval" {
		t.Fatalf("publish pending: %d %v", code, out)
	}
	flatState(t, srv, "drafts", 0, "private", 3)
	id := out["approval"].(map[string]any)["id"].(string)
	approveRequest(t, srv, out)
	f := flatState(t, srv, "drafts", 1, "private", 3)
	serving(t, f["private_url"].(string), "DRAFT-3")
	code, out = req(t, "POST", srv.URL+"/api/flats/drafts/draft?expected_revision=3", bytes.NewReader(archive(map[string]string{"index.html": "DRAFT-4"})), nil)
	if code != 201 {
		t.Fatalf("new Draft: %d %v", code, out)
	}
	flatState(t, srv, "drafts", 1, "private", 4)
	serving(t, f["private_url"].(string), "DRAFT-3")
	code, pending := req(t, "POST", srv.URL+"/api/flats/drafts/deploy", strings.NewReader(`{"version":0}`), nil)
	if code != 202 {
		t.Fatalf("compat deploy Draft: %d %v", code, pending)
	}
	_, out = req(t, "POST", srv.URL+"/api/flats/drafts/draft?expected_revision=4", bytes.NewReader(archive(map[string]string{"index.html": "DRAFT-5"})), nil)
	id = pending["approval"].(map[string]any)["id"].(string)
	code, out = req(t, "POST", srv.URL+"/console/api/approvals/"+id+"/approve", nil, consoleHdr(t, srv))
	if code != 409 || !strings.Contains(fmt.Sprint(out["error"]), "draft") || out["approval"].(map[string]any)["result_data"].(map[string]any)["failure_code"] != "stale_approval" {
		t.Fatalf("frozen Draft drift: %d %v", code, out)
	}
	flatState(t, srv, "drafts", 1, "private", 5)
	serving(t, f["private_url"].(string), "DRAFT-3")
	code, out = req(t, "POST", srv.URL+"/api/flats/drafts/publish", strings.NewReader(`{"revision":5}`), nil)
	if code != 202 {
		t.Fatalf("fresh request: %d %v", code, out)
	}
	approveRequest(t, srv, out)
	flatState(t, srv, "drafts", 2, "private", 5)
	serving(t, f["private_url"].(string), "DRAFT-5")
}

func TestProviderGrantAndBothVisibilityDirectionsRequireConsole(t *testing.T) {
	srv, _ := setup(t)
	saveAndPublish(t, srv, "access", "CURRENT")
	grant := `{"provider":"portal","permitted":true}`
	code, out := req(t, "POST", srv.URL+"/api/flats/access/providers", strings.NewReader(grant), nil)
	if code != 403 {
		t.Fatalf("agent grant: %d %v", code, out)
	}
	code, out = req(t, "POST", srv.URL+"/console/api/flats/access/providers", strings.NewReader(grant), consoleHdr(t, srv))
	if code != 200 {
		t.Fatalf("console grant: %d %v", code, out)
	}
	flatState(t, srv, "access", 1, "private", 1)
	for _, visibility := range []string{"public-unlisted", "private"} {
		prefix, h := "/api", map[string]string{"X-Flats-Client": "cli"}
		before, after := "private", "public"
		if visibility == "private" {
			prefix, h = "/console/api", consoleHdr(t, srv)
			before, after = "public", "private"
		}
		code, pending := req(t, "POST", srv.URL+prefix+"/flats/access/visibility", strings.NewReader(`{"visibility":"`+visibility+`"}`), h)
		if code != 202 || pending["status"] != "pending_approval" {
			t.Fatalf("visibility pending: %d %v", code, pending)
		}
		flatState(t, srv, "access", 1, before, 1)
		approveRequest(t, srv, pending)
		flatState(t, srv, "access", 1, after, 1)
	}
	code, out = req(t, "POST", srv.URL+"/api/flats/access/visibility", strings.NewReader(`{"visibility":"private"}`), nil)
	if code != 200 || out["status"] != "done" || out["message"] != "visibility unchanged" {
		t.Fatalf("same visibility: %d %v", code, out)
	}
}

// This exercises real HTTP/core transitions through disposable Local listeners
// behind the historical PublicNet adapter. The production provider-manager lane
// and real internet/tailnet behavior are verified separately by integration.
func TestPublicPublishRollbackKeepsBothCurrentEndpointsOnApprovedVersion(t *testing.T) {
	srv, _ := setup(t)
	saveAndPublish(t, srv, "dual", "VERSION-1")
	code, out := req(t, "POST", srv.URL+"/console/api/flats/dual/providers", strings.NewReader(`{"provider":"portal","permitted":true}`), consoleHdr(t, srv))
	if code != 200 {
		t.Fatalf("grant: %d %v", code, out)
	}
	_, pending := req(t, "POST", srv.URL+"/api/flats/dual/visibility", strings.NewReader(`{"visibility":"public"}`), nil)
	approveRequest(t, srv, pending)
	f := flatState(t, srv, "dual", 1, "public", 1)
	privateURL, publicURL := f["private_url"].(string), f["public_url"].(string)
	for _, url := range []string{privateURL, publicURL} {
		serving(t, url, "VERSION-1")
	}
	code, out = req(t, "POST", srv.URL+"/api/flats/dual/draft?expected_revision=1", bytes.NewReader(archive(map[string]string{"index.html": "VERSION-2"})), nil)
	if code != 201 {
		t.Fatalf("save: %d %v", code, out)
	}
	for _, url := range []string{privateURL, publicURL} {
		serving(t, url, "VERSION-1")
	}
	code, preview := req(t, "POST", srv.URL+"/api/flats/dual/previews", strings.NewReader(`{"target":"draft","version":0}`), nil)
	if code != 201 {
		t.Fatalf("Draft preview: %d %v", code, preview)
	}
	serving(t, preview["url"].(string), "VERSION-2")
	code, pending = req(t, "POST", srv.URL+"/api/flats/dual/publish", strings.NewReader(`{"revision":2}`), nil)
	if code != 202 {
		t.Fatalf("publish: %d %v", code, pending)
	}
	for _, url := range []string{privateURL, publicURL} {
		serving(t, url, "VERSION-1")
	}
	approveRequest(t, srv, pending)
	flatState(t, srv, "dual", 2, "public", 2)
	for _, url := range []string{privateURL, publicURL} {
		serving(t, url, "VERSION-2")
	}
	code, pending = req(t, "POST", srv.URL+"/api/flats/dual/rollback", strings.NewReader(`{"version":1,"restore_data":false}`), nil)
	if code != 202 {
		t.Fatalf("rollback: %d %v", code, pending)
	}
	for _, url := range []string{privateURL, publicURL} {
		serving(t, url, "VERSION-2")
	}
	approveRequest(t, srv, pending)
	flatState(t, srv, "dual", 1, "public", 2)
	for _, url := range []string{privateURL, publicURL} {
		serving(t, url, "VERSION-1")
	}
	_, versions := req(t, "GET", srv.URL+"/api/flats/dual/versions", nil, nil)
	if len(versions["versions"].([]any)) != 2 {
		t.Fatalf("rollback allocated version: %v", versions)
	}
	code, preview = req(t, "POST", srv.URL+"/api/flats/dual/previews", strings.NewReader(`{"target":"draft","version":0}`), nil)
	if code != 201 {
		t.Fatalf("retained Draft preview: %d %v", code, preview)
	}
	serving(t, preview["url"].(string), "VERSION-2")
}
