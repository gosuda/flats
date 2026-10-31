package notebook

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func liveHTTPRequest(method, target, body, access string) *http.Request {
	r := httptest.NewRequest(method, "https://notes.example"+target, strings.NewReader(body))
	if access != "" {
		r.Header.Set("X-Flats-Access", access)
	}
	r.Header.Set("X-Flats-Notebook", "1")
	r.Header.Set("Origin", "https://notes.example")
	return r
}

func TestLiveHTTPAccess(t *testing.T) {
	f := newWriterFixture(t)
	for _, tc := range []struct {
		name   string
		access string
		status int
	}{
		{"missing", "", http.StatusForbidden},
		{"public", "public", http.StatusForbidden},
		{"unknown", "owner", http.StatusForbidden},
		{"case sensitive", "Private", http.StatusForbidden},
		{"list", "private, public", http.StatusForbidden},
		{"private", "private", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := liveHTTPRequest("POST", "/_notebook/api/paste", `{"markdown":"# hello"}`, tc.access)
			r.Header.Set("X-Flats-Public-Access", "edit")
			w := httptest.NewRecorder()
			f.in.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal(w.Header())
			}
		})
	}
	r := liveHTTPRequest("POST", "/_notebook/api/paste", `{"markdown":"x"}`, "private")
	r.Header.Add("X-Flats-Access", "public")
	w := httptest.NewRecorder()
	f.in.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("duplicate access headers: %d", w.Code)
	}
}

func TestLiveHTTPOrigin(t *testing.T) {
	f := newWriterFixture(t)
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
		status int
	}{
		{"same origin", func(*http.Request) {}, http.StatusOK},
		{"cross origin", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, http.StatusForbidden},
		{"null origin", func(r *http.Request) { r.Header.Set("Origin", "null") }, http.StatusForbidden},
		{"missing origin", func(r *http.Request) { r.Header.Del("Origin") }, http.StatusForbidden},
		{"fetch metadata", func(r *http.Request) { r.Header.Del("Origin"); r.Header.Set("Sec-Fetch-Site", "same-origin") }, http.StatusOK},
		{"conflicting origin", func(r *http.Request) {
			r.Header.Set("Origin", "https://evil.example")
			r.Header.Set("Sec-Fetch-Site", "same-origin")
		}, http.StatusForbidden},
		{"cross site metadata", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, http.StatusForbidden},
		{"missing notebook header", func(r *http.Request) { r.Header.Del("X-Flats-Notebook") }, http.StatusForbidden},
		{"trusted proxy", func(r *http.Request) { r.TLS = nil; r.Header.Set("X-Forwarded-Proto", "https") }, http.StatusOK},
		{"wrong scheme", func(r *http.Request) { r.TLS = nil }, http.StatusForbidden},
		{"tls", func(r *http.Request) { r.TLS = &tls.ConnectionState{} }, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := liveHTTPRequest("POST", "/_notebook/api/paste", `{"markdown":"x"}`, "private")
			tc.change(r)
			w := httptest.NewRecorder()
			f.in.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestLiveHTTPRoutes(t *testing.T) {
	f := newWriterFixture(t)
	for _, route := range []string{"revisions", "revisions/1", "recover", "pin", "rollback"} {
		t.Run(route, func(t *testing.T) {
			method := "POST"
			if strings.HasPrefix(route, "revisions") {
				method = "GET"
			}
			for _, access := range []string{"public", "private"} {
				w := httptest.NewRecorder()
				f.in.ServeHTTP(w, liveHTTPRequest(method, "/_notebook/api/"+route, `{}`, access))
				refused := w.Code == http.StatusForbidden || w.Code == http.StatusNotFound
				if (access == "public") != refused {
					t.Fatalf("%s: %d %s", access, w.Code, w.Body.String())
				}
			}
		})
	}
	for _, route := range []string{"assets", "paste", "conversion-preview"} {
		w := httptest.NewRecorder()
		f.in.ServeHTTP(w, liveHTTPRequest("GET", "/_notebook/api/"+route, "", "private"))
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "POST" {
			t.Fatalf("%s: %d", route, w.Code)
		}
	}
	w := httptest.NewRecorder()
	f.in.ServeHTTP(w, liveHTTPRequest("GET", "/unknown", "", "private"))
	if w.Code != http.StatusNotFound {
		t.Fatal(w.Code)
	}
	r := liveHTTPRequest("GET", "/_notebook/ws", "", "public")
	r.Header.Set("Origin", "https://evil.example")
	w = httptest.NewRecorder()
	f.in.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal(w.Code)
	}
}

func TestLiveHTTPApplication(t *testing.T) {
	source := fstest.MapFS{
		"web/index.html":         &fstest.MapFile{Data: []byte(`<link rel="stylesheet" href="/_notebook/app/app.css"><script type="module" src="/_notebook/app/app.js"></script>`)},
		"web/app.js":             &fstest.MapFile{Data: []byte(`import './other.js'; fetch('/_notebook/app/other.js');`)},
		"web/other.js":           &fstest.MapFile{Data: []byte(`export const n = 1;`)},
		"web/app.css":            &fstest.MapFile{Data: []byte(`body { color: black; }`)},
		"web/testdata/secret.js": &fstest.MapFile{Data: []byte(`not served`)},
	}
	app, err := loadNotebookApp(source)
	if err != nil {
		t.Fatal(err)
	}
	if len(app.files) != 3 || bytes.Contains(app.shell, []byte(`src="/_notebook/app/app.js"`)) || !bytes.Contains(app.shell, []byte(app.version)) {
		t.Fatalf("bundle: %+v", app)
	}
	if !bytes.Contains(app.files["app.js"], []byte(notebookAppPrefix+app.version+"/other.js")) {
		t.Fatal("absolute module references are not versioned")
	}
	source["web/other.js"].Data = []byte(`export const n = 2;`)
	changed, err := loadNotebookApp(source)
	if err != nil || changed.version == app.version {
		t.Fatalf("bundle version did not change: %v", err)
	}
	for _, tc := range []struct {
		name   string
		status int
		cache  string
	}{
		{"/", 200, "no-store"},
		{notebookAppPrefix + app.version + "/app.js", 200, "public, max-age=31536000, immutable"},
		{notebookAppPrefix + app.version + "/app.css", 200, "public, max-age=31536000, immutable"},
		{notebookAppPrefix + "old/app.js", 404, "no-store"},
		{notebookAppPrefix + "app.js", 404, "no-store"},
		{notebookAppPrefix + app.version + "/testdata/secret.js", 404, "no-store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := liveHTTPRequest("GET", tc.name, "", "public")
			name := strings.TrimPrefix(tc.name, notebookAppPrefix)
			if tc.name == "/" {
				name = ""
			}
			app.serve(w, r, name)
			if w.Code != tc.status || w.Header().Get("Cache-Control") != tc.cache {
				t.Fatalf("status/cache: %d %s", w.Code, w.Header().Get("Cache-Control"))
			}
			if tc.name == "/" {
				csp := w.Header().Get("Content-Security-Policy")
				if !strings.Contains(csp, "img-src 'self' blob:") || strings.Contains(csp, "unsafe") || !strings.Contains(csp, "frame-src 'none'") {
					t.Fatal(csp)
				}
			}
		})
	}
	delete(source, "web/index.html")
	if _, err := loadNotebookApp(source); err == nil {
		t.Fatal("missing shell accepted")
	}
}

func TestLiveHTTPPaste(t *testing.T) {
	f := newWriterFixture(t)
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"markdown", `{"markdown":"# Hi\n\n**bold**"}`, 200},
		{"html", `{"html":"<h2>Hi</h2><script>unsafe()</script><p><b>bold</b></p>"}`, 200},
		{"unknown field", `{"markdown":"x","unexpected":1}`, 400},
		{"both", `{"html":"x","markdown":"x"}`, 400},
		{"missing", `{}`, 400},
		{"trailing JSON", `{"markdown":"x"}{}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			f.in.ServeHTTP(w, liveHTTPRequest("POST", "/_notebook/api/paste", tc.body, "private"))
			if w.Code != tc.want {
				t.Fatalf("%d: %s", w.Code, w.Body.String())
			}
			if tc.want != 200 {
				return
			}
			var result struct {
				Markdown string `json:"markdown"`
				Refs     []struct {
					Ref  string `json:"ref"`
					Kind Kind   `json:"kind"`
				} `json:"refs"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			ops, _, err := ParseMarkdown(result.Markdown, Placement{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, op := range ops {
				creates, err := operationCreates(op)
				if err != nil {
					t.Fatal(err)
				}
				for _, create := range creates {
					names = append(names, create.Name)
				}
			}
			if len(names) != len(result.Refs) || len(names) != 2 {
				t.Fatalf("creation names: %+v %+v", names, result.Refs)
			}
			for i, ref := range result.Refs {
				if ref.Ref != names[i] || ref.Kind != BlockKind {
					t.Fatal(result.Refs)
				}
			}
			if tc.name == "html" && strings.Contains(result.Markdown, "unsafe") {
				t.Fatal("script content survived")
			}
		})
	}
}

func TestLiveHTTPPreviewToken(t *testing.T) {
	f := newWriterFixture(t)
	text := "not a number"
	u := Update{Slug: "notes", OpID: "preview-fixture", Ops: []Operation{
		CreateDatabaseOp{CreateFields: CreateFields{Ref: "db"}, BlockRef: "owner", Place: Placement{Page: Target{Ref: &f.page}}, Title: "Data"},
		CreatePropertyOp{CreateFields: CreateFields{Ref: "property"}, Database: Target{Tmp: "db"}, Definition: PropertyDef{Name: "Value", Type: TextType}},
		CreateRowOp{CreateFields: CreateFields{Ref: "row"}, Database: Target{Tmp: "db"}, Cells: []CellInput{{Property: Target{Tmp: "property"}, Value: CellInputValue{Text: &text}}}},
	}}
	result, err := f.in.Update(t.Context(), u, 0)
	if err != nil {
		t.Fatal(err)
	}
	property := result.Refs["property"].Ref
	definition := PropertyDef{Name: "Value", Type: NumberType}
	body, err := json.Marshal(map[string]any{"property": property, "definition": definition})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	f.in.ServeHTTP(w, liveHTTPRequest("POST", "/_notebook/api/conversion-preview", string(body), "private"))
	if w.Code != 200 {
		t.Fatalf("%d: %s", w.Code, w.Body.String())
	}
	var preview struct {
		Preview string `json:"preview"`
		Emptied int    `json:"emptied"`
		Options int    `json:"options_created"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if len(preview.Preview) != 64 || preview.Emptied != 1 || preview.Options != 0 {
		t.Fatalf("preview: %+v", preview)
	}
	ack, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, Action{N: 1, ActionID: "convert", Ops: []Operation{SetPropertyOp{Property: Target{Ref: &property}, Definition: definition, ConfirmedPreview: &preview.Preview}}})
	if err != nil || ack.Summary.Applied != 1 {
		t.Fatalf("confirm preview: %+v %v", ack, err)
	}
}
