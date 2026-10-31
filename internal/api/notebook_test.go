package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/notebook"
)

const notebookPageID = "p_aaaaaaaaaaaaaaaaaaaaaaaaaa"
const notebookDatabaseID = "d_aaaaaaaaaaaaaaaaaaaaaaaaaa"
const notebookViewID = "v_aaaaaaaaaaaaaaaaaaaaaaaaaa"
const notebookPropertyID = "pr_aaaaaaaaaaaaaaaaaaaaaaaaaa"
const notebookUpdateBody = `{"op_id":"api-write-1","ops":[{"op":"create_page","ref":"page","parent":null,"before":null,"title":"Notes"}]}`

type notebookFakeService struct {
	get    func(context.Context, string, int, []byte) (json.RawMessage, error)
	update func(context.Context, string, []byte, core.Via) (core.NotebookWriteResult, error)
	upload int64
}

func (f notebookFakeService) GetNotebook(ctx context.Context, slug string, rev int, q []byte) (json.RawMessage, error) {
	return f.get(ctx, slug, rev, q)
}
func (f notebookFakeService) UpdateNotebook(ctx context.Context, slug string, raw []byte, via core.Via) (core.NotebookWriteResult, error) {
	return f.update(ctx, slug, raw, via)
}
func (f notebookFakeService) UploadLimit() int64 { return f.upload }

func notebookRequest(method, query, body string) *http.Request {
	r := httptest.NewRequest(method, "/api/flats/notes/notebook"+query, strings.NewReader(body))
	r.SetPathValue("slug", "notes")
	return r
}

func TestNotebookQuery(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, query string
		rev         int
		want        string
	}{
		{"tree", "", 0, `{}`},
		{"page", "page=" + notebookPageID + "&format=markdown&limit=1&revision=7", 7, `"format":"markdown"`},
		{"reference", "page=" + url.QueryEscape(`{"id":"`+notebookPageID+`","inc":"7"}`), 0, `"inc":"7"`},
		{"database", "database=" + notebookDatabaseID + "&view=" + notebookViewID, 0, `"view":"` + notebookViewID + `"`},
		{"ad hoc", "database=" + notebookDatabaseID + "&sort=" + url.QueryEscape(`[{"property":{"id":"`+notebookPropertyID+`","inc":"1"},"direction":"asc"}]`) + "&filter=[]", 0, `"direction":"asc"`},
		{"content cursor", "cursor=opaque", 0, `{"cursor":"opaque"}`},
		{"mapping cursor", "op_id=write-1&cursor=opaque", 0, `"op_id":"write-1"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			q, _ := url.ParseQuery(tt.query)
			rev, raw, err := NotebookQuery(q)
			if err != nil || rev != tt.rev || !strings.Contains(string(raw), tt.want) {
				t.Fatalf("query = %d %s %v", rev, raw, err)
			}
		})
	}
}

func TestNotebookReadAdmission(t *testing.T) {
	t.Parallel()
	badQueries := []struct{ name, query string }{
		{"unknown", "bogus=1"}, {"repeat", "limit=1&limit=2"}, {"empty", "page="},
		{"bad encoding", "page=%zz"}, {"semicolon", "limit=1;page=x"},
		{"page type", "page=" + notebookDatabaseID}, {"two scopes", "page=" + notebookPageID + "&database=" + notebookDatabaseID},
		{"invalid ref", "page=" + url.QueryEscape(`{"id":"`+notebookPageID+`","inc":"0"}`)},
		{"extra ref field", "page=" + url.QueryEscape(`{"id":"`+notebookPageID+`","inc":"1","extra":true}`)},
		{"limit zero", "limit=0"}, {"limit large", "limit=2001"}, {"limit decimal", "limit=1.1"},
		{"revision zero", "revision=0"}, {"revision negative", "revision=-1"},
		{"format", "format=html"}, {"markdown tree", "format=markdown"},
		{"view without database", "view=" + notebookViewID},
		{"view and sort", "database=" + notebookDatabaseID + "&view=" + notebookViewID + "&sort=[]"},
		{"cursor and limit", "cursor=x&limit=1"}, {"cursor and revision", "cursor=x&revision=1"},
		{"op without cursor", "op_id=x"}, {"invalid op", "cursor=x&op_id=bad%20id"},
		{"sort null", "database=" + notebookDatabaseID + "&sort=null"},
		{"sort temporary", "database=" + notebookDatabaseID + "&sort=" + url.QueryEscape(`[{"property":{"tmp":"p"},"direction":"asc"}]`)},
		{"filter unknown", "database=" + notebookDatabaseID + "&filter=" + url.QueryEscape(`[{"property":{"id":"`+notebookPropertyID+`","inc":"1"},"op":"empty","unknown":1}]`)},
	}
	for _, tt := range badQueries {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			getNotebook(w, notebookRequest("GET", "?"+tt.query, ""), notebookFakeService{})
			if w.Code != 400 || !strings.Contains(w.Body.String(), `"category": "invalid"`) {
				t.Fatalf("read = %d %s", w.Code, w.Body.String())
			}
		})
	}
	w := httptest.NewRecorder()
	getNotebook(w, notebookRequest("GET", "", "{}"), notebookFakeService{})
	if w.Code != 400 {
		t.Fatalf("GET body = %d", w.Code)
	}
}

func TestNotebookReadRoundTrip(t *testing.T) {
	t.Parallel()
	calls := 0
	fake := notebookFakeService{get: func(ctx context.Context, slug string, rev int, q []byte) (json.RawMessage, error) {
		calls++
		if slug != "notes" || ctx == nil || rev != 4 || strings.Contains(string(q), "revision") || !strings.Contains(string(q), notebookPageID) {
			t.Fatalf("core read = %s %d %s", slug, rev, q)
		}
		return json.RawMessage(`{"state":{"store":"s","seq":"1"},"page":{"title":"Notes"},"complete":true,"next":null}`), nil
	}}
	w := httptest.NewRecorder()
	getNotebook(w, notebookRequest("GET", "?revision=4&page="+notebookPageID, ""), fake)
	if w.Code != 200 || calls != 1 || !strings.Contains(w.Body.String(), `"title": "Notes"`) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("read = %d %s", w.Code, w.Body.String())
	}
}

func TestNotebookUpdateAdmission(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, body string }{
		{"null", "null"}, {"empty", ""}, {"array", "[]"}, {"missing", `{}`},
		{"slug", `{"slug":"notes","op_id":"x","ops":[]}`},
		{"unknown", `{"op_id":"x","ops":[],"extra":1}`},
		{"trailing", notebookUpdateBody + ` {}`},
		{"duplicate", `{"op_id":"x","op_id":"y","ops":[]}`},
		{"browser field", strings.Replace(notebookUpdateBody, `"title":"Notes"`, `"title":"Notes","proposed_id":"`+notebookPageID+`"`, 1)},
		{"bare mutation id", `{"op_id":"x","ops":[{"op":"delete_page","page":"` + notebookPageID + `"}]}`},
		{"unknown operation", `{"op_id":"x","ops":[{"op":"erase"}]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			updateNotebook(w, notebookRequest("POST", "", tt.body), core.ViaAPI, notebookFakeService{})
			if w.Code != 400 {
				t.Fatalf("update = %d %s", w.Code, w.Body.String())
			}
		})
	}
	w := httptest.NewRecorder()
	updateNotebook(w, notebookRequest("POST", "?page=x", notebookUpdateBody), core.ViaAPI, notebookFakeService{})
	if w.Code != 400 {
		t.Fatalf("POST query = %d", w.Code)
	}
	w = httptest.NewRecorder()
	r := notebookRequest("POST", "", "")
	r.Body = io.NopCloser(bytes.NewReader(bytes.Repeat([]byte(" "), notebook.MaxAgentOperationBytes+1)))
	updateNotebook(w, r, core.ViaAPI, notebookFakeService{})
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize = %d %s", w.Code, w.Body.String())
	}
}

func TestNotebookUpdateReplay(t *testing.T) {
	t.Parallel()
	calls, effects := 0, 0
	var original string
	fake := notebookFakeService{update: func(ctx context.Context, slug string, raw []byte, via core.Via) (core.NotebookWriteResult, error) {
		calls++
		if slug != "notes" || via != core.ViaCLI || strings.Contains(string(raw), `"slug"`) {
			t.Fatalf("write = %s %s %s", slug, via, raw)
		}
		var object any
		if err := json.Unmarshal(raw, &object); err != nil {
			t.Fatal(err)
		}
		canonical, _ := json.Marshal(object)
		replay := original != ""
		if replay && original != string(canonical) {
			return core.NotebookWriteResult{}, &notebook.WriteError{Code: "conflict", Reason: "op_id_reused"}
		}
		if !replay {
			effects++
			original = string(canonical)
		}
		return core.NotebookWriteResult{Result: json.RawMessage(fmt.Sprintf(`{"replay":%t,"revision":null,"revision_pending":true,"applied":1}`, replay)), Pending: true}, nil
	}}
	for i, body := range []string{notebookUpdateBody, "\n " + notebookUpdateBody, strings.Replace(notebookUpdateBody, "Notes", "Changed", 1)} {
		w := httptest.NewRecorder()
		updateNotebook(w, notebookRequest("POST", "", body), core.ViaCLI, fake)
		if i == 2 {
			if w.Code != 409 {
				t.Fatalf("conflicting replay = %d %s", w.Code, w.Body.String())
			}
		} else if w.Code != 200 || !strings.Contains(w.Body.String(), `"pending": true`) || !strings.Contains(w.Body.String(), fmt.Sprintf(`"replay": %t`, i == 1)) {
			t.Fatalf("write = %d %s", w.Code, w.Body.String())
		}
	}
	if calls != 3 || effects != 1 {
		t.Fatalf("calls/effects = %d/%d", calls, effects)
	}
}

func TestNotebookStatus(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		err      error
		status   int
		category string
	}{
		{"wrong type", core.ErrNotNotebook, 400, "not_notebook"},
		{"invalid", &notebook.WriteError{Code: "invalid"}, 400, "invalid"},
		{"conflict", &notebook.WriteError{Code: "conflict", Reason: "read_expired"}, 409, "conflict"},
		{"capacity", &notebook.WriteError{Code: "notebook_capacity", Reason: "disk_quota"}, 422, "notebook_capacity"},
		{"unavailable", &notebook.WriteError{Code: "unavailable", Retry: true}, 503, "unavailable"},
		{"missing", &notebook.WriteError{Code: "not_found"}, 404, "not_found"},
		{"forbidden", &notebook.WriteError{Code: "forbidden"}, 403, "forbidden"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			failNotebook(w, fmt.Errorf("host: %w", tt.err))
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != tt.status || body["category"] != tt.category {
				t.Fatalf("refusal = %d %s", w.Code, w.Body.String())
			}
			var refusal *notebook.WriteError
			if errors.As(tt.err, &refusal) && body["retry"] != refusal.Retry {
				t.Fatalf("retry = %s", w.Body.String())
			}
		})
	}
}

func TestNotebookRegistration(t *testing.T) {
	t.Parallel()
	h := (&Server{}).Handler()
	for _, tt := range []struct {
		name, method, path string
		status             int
	}{
		{"read", "GET", "/api/flats/notes/notebook?unknown=x", 400},
		{"write", "POST", "/api/flats/notes/notebook?unknown=x", 400},
		{"console", "GET", "/console/api/flats/notes/notebook", 404},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, tt.path, nil)
			r.RemoteAddr = "127.0.0.1:1234"
			r.Header.Set("X-Flats-Client", "cli")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.status {
				t.Fatalf("route = %d %s", w.Code, w.Body.String())
			}
		})
	}
}
