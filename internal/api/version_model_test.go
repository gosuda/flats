package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/bundle"
	"net/http"
	"strings"
	"testing"
)

func TestVersionQuotaAndExplicitDeletion(t *testing.T) {
	srv, _ := setup(t)
	saveAndPublish(t, srv, "versions", "one")
	code, f := req(t, "GET", srv.URL+"/api/flats/versions", nil, nil)
	if code != 200 || f["version_count"] != float64(1) || f["effective_version_quota"] != float64(20) || f["version_quota"] != nil {
		t.Fatalf("default quota: %d %v", code, f)
	}
	quotaURL := srv.URL + "/console/api/flats/versions/version-quota"
	code, f = req(t, "PUT", quotaURL, strings.NewReader(`{"version_quota":1}`), consoleHdr(t, srv))
	if code != 200 || f["version_quota"] != float64(1) {
		t.Fatalf("override: %d %v", code, f)
	}
	req(t, "POST", srv.URL+"/api/flats/versions/draft", bytes.NewReader(archive(map[string]string{"index.html": "two"})), nil)
	code, out := req(t, "POST", srv.URL+"/api/flats/versions/versions", strings.NewReader(`{"revision":2}`), nil)
	if code != 409 || !strings.Contains(fmt.Sprint(out), "quota") {
		t.Fatalf("full quota: %d %v", code, out)
	}
	code, out = req(t, "DELETE", srv.URL+"/api/flats/versions/versions/1", nil, nil)
	if code < 400 {
		t.Fatalf("deleted live version: %d %v", code, out)
	}
	code, _ = req(t, "PUT", quotaURL, strings.NewReader(`{"version_quota":0}`), consoleHdr(t, srv))
	if code != 200 {
		t.Fatal(code)
	}
	code, pending := req(t, "POST", srv.URL+"/api/flats/versions/versions", strings.NewReader(`{"revision":2}`), nil)
	if code != 202 || pending["approval"].(map[string]any)["action"] != "create_version" {
		t.Fatalf("create: %d %v", code, pending)
	}
	approveRequest(t, srv, pending)
	code, pending = req(t, "DELETE", srv.URL+"/api/flats/versions/versions/2", nil, nil)
	if code != 202 || pending["approval"].(map[string]any)["action"] != "delete_version" {
		t.Fatalf("delete approval: %d %v", code, pending)
	}
	_, before := req(t, "GET", srv.URL+"/api/flats/versions", nil, nil)
	if before["version_count"] != float64(2) {
		t.Fatal(before)
	}
	approveRequest(t, srv, pending)
	_, after := req(t, "GET", srv.URL+"/api/flats/versions", nil, nil)
	if after["version_count"] != float64(1) {
		t.Fatal(after)
	}
	for _, body := range []string{`{}`, `{"version_quota":-1}`, `{"version_quota":"2"}`, `{"version_quota":1.5}`} {
		code, _ := req(t, "PUT", quotaURL, strings.NewReader(body), consoleHdr(t, srv))
		if code != 400 {
			t.Fatalf("invalid quota %s: %d", body, code)
		}
	}
	code, f = req(t, "PUT", quotaURL, strings.NewReader(`{"version_quota":null}`), consoleHdr(t, srv))
	if code != 200 || f["version_quota"] != nil || f["effective_version_quota"] != float64(20) {
		t.Fatalf("inherit: %d %v", code, f)
	}
	code, _ = req(t, "PUT", srv.URL+"/api/flats/versions/version-quota", strings.NewReader(`{"version_quota":0}`), nil)
	if code < 400 {
		t.Fatal("agent changed operator quota")
	}
}

func TestConcurrentDraftSaveReceiptsKeepOwnContent(t *testing.T) {
	srv, _ := setup(t)
	req(t, "POST", srv.URL+"/api/flats/receipts/draft", bytes.NewReader(archive(map[string]string{"index.html": "seed"})), nil)
	type receipt struct {
		want  string
		code  int
		draft api.SavedDraft
		err   error
	}
	start := make(chan struct{})
	results := make(chan receipt, 8)
	for i := range 8 {
		content := fmt.Sprintf("content-%d", i)
		go func() {
			<-start
			want := bundle.Hash([]bundle.File{{Path: "index.html", Data: []byte(content)}})
			request, err := http.NewRequest("POST", srv.URL+"/api/flats/receipts/draft", bytes.NewReader(archive(map[string]string{"index.html": content})))
			if err != nil {
				results <- receipt{err: err}
				return
			}
			request.Header.Set("X-Flats-Client", "test")
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				results <- receipt{err: err}
				return
			}
			defer response.Body.Close()
			var out api.SaveResponse
			err = json.NewDecoder(response.Body).Decode(&out)
			result := receipt{want: want, code: response.StatusCode, err: err}
			if out.Draft != nil {
				result.draft = *out.Draft
			}
			results <- result
		}()
	}
	close(start)
	seen := map[int]bool{}
	for range 8 {
		got := <-results
		if got.err != nil || got.code != 201 || got.draft.Hash != got.want || got.draft.Revision < 2 || got.draft.SavedAt.IsZero() {
			t.Fatalf("save receipt mismatch: %+v", got)
		}
		if seen[got.draft.Revision] {
			t.Fatalf("two saves returned revision %d", got.draft.Revision)
		}
		seen[got.draft.Revision] = true
	}
}

// This sends the real legacy request, bypassing req's save-and-create fixture.
func TestDraftRejectsDeployQueryWithoutSaving(t *testing.T) {
	srv, svc := setup(t)
	request, err := http.NewRequest("POST", srv.URL+"/api/flats/hidden/draft?deploy=1", bytes.NewReader(archive(map[string]string{"index.html": "must not save"})))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Flats-Client", "test")
	response, err := srv.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatalf("hidden deploy accepted: %d", response.StatusCode)
	}
	flats, err := svc.ListFlats(context.Background())
	if err != nil || len(flats) != 0 {
		t.Fatalf("refused request saved content: %+v %v", flats, err)
	}
	approvals, err := svc.ListApprovals(context.Background(), "pending")
	if err != nil || len(approvals) != 0 {
		t.Fatalf("refused request created approval: %+v %v", approvals, err)
	}
}
