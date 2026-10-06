package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/store"
)

type draftOnlyRuntime struct{}

func (draftOnlyRuntime) Start(context.Context, core.RuntimeSpec) (core.Instance, error) {
	panic("reading or saving Draft must not start a runtime")
}

func TestDocumentAPIAndTypeViews(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	net, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer net.Close()
	svc, err := core.New(context.Background(), core.Config{DataDir: dir, Store: st, Private: net, Runtime: draftOnlyRuntime{}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	ctx := context.Background()
	if _, err := svc.SaveVersion(ctx, "document", []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte("# 한국어 😀")}}, core.SaveMeta{}, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveVersion(ctx, "website", []bundle.File{{Path: "index.html", Data: []byte("website")}}, core.SaveMeta{}, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateFlat(ctx, "empty", "", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	h := (&api.Server{Svc: svc}).Handler()
	for _, prefix := range []string{"/api", "/console/api"} {
		t.Run(prefix, func(t *testing.T) {
			for _, tt := range []struct {
				name, path, category string
				status               int
			}{{"draft", "document/document", "", 200}, {"missing doc", "document/document?doc=missing.md", "document_not_found", 404}, {"not docs", "website/document", "not_docs", 400}, {"empty", "empty/document", "not_deployed", 409}, {"missing flat", "absent/document", "not_found", 404}, {"bad path", "document/document?doc=../index.md", "invalid", 400}, {"missing conflict", "document/document?conflict=4", "document_not_found", 404}, {"negative conflict", "document/document?conflict=-1", "invalid", 400}, {"bad conflict", "document/document?conflict=no", "invalid", 400}} {
				t.Run(tt.name, func(t *testing.T) {
					r := httptest.NewRequest("GET", prefix+"/flats/"+tt.path, nil)
					r.Header.Set("X-Flats-Console", "1")
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					if w.Code != tt.status {
						t.Fatalf("%d %s", w.Code, w.Body.String())
					}
					if tt.status == 200 {
						var out core.Document
						err := json.Unmarshal(w.Body.Bytes(), &out)
						if err != nil || out.Source != "draft" || out.Markdown != "# 한국어 😀" {
							t.Fatalf("%s %v", w.Body.String(), err)
						}
					} else {
						var out api.ErrorBody
						_ = json.Unmarshal(w.Body.Bytes(), &out)
						if out.Category != tt.category {
							t.Fatalf("%+v", out)
						}
					}
				})
			}
			for _, path := range []string{"/flats/document", "/flats/document/draft", "/flats"} {
				r := httptest.NewRequest("GET", prefix+path, nil)
				r.Header.Set("X-Flats-Console", "1")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != http.StatusOK {
					t.Fatalf("view %s: %s", path, w.Body.String())
				}
				var out map[string]any
				_ = json.Unmarshal(w.Body.Bytes(), &out)
				if path == "/flats" {
					list := out["flats"].([]any)
					found := false
					for _, f := range list {
						v := f.(map[string]any)
						if v["slug"] == "document" {
							found = v["type"] == "docs"
						}
					}
					if !found {
						t.Fatal("list missing docs type")
					}
				} else if out["type"] != "docs" {
					t.Fatalf("view missing type: %s", w.Body.String())
				}
			}
		})
	}
}
