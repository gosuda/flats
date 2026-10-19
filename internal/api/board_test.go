package api_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/core"
)

func TestBoardAPI(t *testing.T) {
	srv, svc := setup(t)
	body := `{"op_id":"api-write-1","ops":[{"op":"add","ref":"$a","el":{"type":"sticky","text":"alpha"}},{"op":"add","el":{"type":"sticky","text":"beta"}}],"message":"two notes"}`
	code, out := req(t, "POST", srv.URL+"/api/flats/plan/board", strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
	if code != 200 || out["revision"] != float64(2) || out["created"] != true || out["ids"].(map[string]any)["$a"] == nil {
		t.Fatalf("update = %d %v", code, out)
	}
	code, out = req(t, "GET", srv.URL+"/api/flats/plan/board?format=outline&limit=1", nil, nil)
	if code != 200 || len(out["elements"].([]any)) != 1 || out["next"] == "" || out["revision"] != float64(2) {
		t.Fatalf("read = %d %v", code, out)
	}
	code, out = req(t, "GET", srv.URL+"/api/flats/plan/board?revision=1", nil, nil)
	if code != 200 || len(out["elements"].([]any)) != 0 {
		t.Fatalf("revision read = %d %v", code, out)
	}
	for _, tt := range []struct {
		method, path, body, category string
		status                       int
	}{
		{"GET", "/api/flats/plan/board?rect=1,2", "", "invalid", 400},
		{"GET", "/api/flats/plan/board?cursor=0123456789abcdef0123456789abcdef", "", "conflict", 409},
		{"POST", "/api/flats/plan/board", `{"op_id":"api-write-1","ops":[]}`, "conflict", 409},
		{"POST", "/api/flats/plan/board", `{"op_id":"short"}`, "invalid", 400},
		{"POST", "/api/flats/plan/board", `{"op_id":"unknown-field","bogus":1}`, "", 400},
		{"GET", "/api/flats/absent/board", "", "not_found", 404},
	} {
		code, out := req(t, tt.method, srv.URL+tt.path, strings.NewReader(tt.body), map[string]string{"Content-Type": "application/json"})
		if code != tt.status || (tt.category != "" && out["category"] != tt.category) {
			t.Errorf("%s %s = %d %v", tt.method, tt.path, code, out)
		}
	}
	// A website is not a board.
	saveAndPublish(t, srv, "site", "hello")
	if code, out := req(t, "GET", srv.URL+"/api/flats/site/board", nil, nil); code != 400 || out["category"] != "not_board" {
		t.Fatalf("board read of a website = %d %v", code, out)
	}
	// The board routes are agent routes only.
	if code, _ := req(t, "GET", srv.URL+"/console/api/flats/plan/board", nil, consoleHdr(t, srv)); code != 404 && code != 405 {
		t.Fatalf("console board route = %d", code)
	}

	// Import: the exported data file into a new board.
	f, _ := svc.GetFlat(context.Background(), "plan")
	resp, err := http.Get(f.PrivateURL + "/_board/export.zip")
	if err != nil {
		t.Fatal(err)
	}
	zipped, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	code, out = req(t, "POST", srv.URL+"/api/flats/copy/board/import?mode=add&op_id=api-import-1", bytes.NewReader(zipped), map[string]string{"Content-Type": "application/zip"})
	if code != 200 || len(out["ids"].([]any)) != 2 || out["revision"] != float64(2) {
		t.Fatalf("import = %d %v", code, out)
	}
	code, out = req(t, "POST", srv.URL+"/api/flats/copy/board/import?mode=sideways", bytes.NewReader(zipped), map[string]string{"Content-Type": "application/zip"})
	if code != 400 || out["category"] != "invalid" {
		t.Fatalf("bad mode = %d %v", code, out)
	}
	// An import body above upload_max_bytes × 4 is refused before parsing.
	if _, err := svc.UpdateSettings(context.Background(), map[string]string{core.SetUploadMaxBytes: "1024"}); err != nil {
		t.Fatal(err)
	}
	code, _ = req(t, "POST", srv.URL+"/api/flats/copy/board/import", bytes.NewReader(make([]byte, 5000)), map[string]string{"Content-Type": "application/zip"})
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized import = %d", code)
	}
}
