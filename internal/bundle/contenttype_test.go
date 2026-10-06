package bundle

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContentTypeManifest(t *testing.T) {
	tests := []struct {
		name, manifest      string
		paths               []string
		typ, entry, problem string
	}{
		{"explicit", `{"type":"docs"}`, []string{"index.md"}, "docs", "index.md", ""},
		{"readme", `{"type":"docs"}`, []string{"README.md", "z.md"}, "docs", "README.md", ""},
		{"only nested", `{"type":"docs"}`, []string{"chapter/one.markdown"}, "docs", "chapter/one.markdown", ""},
		{"index preferred", `{"type":"docs"}`, []string{"README.md", "index.md"}, "docs", "index.md", ""},
		{"explicit entry", `{"type":"docs","entry":"b.md"}`, []string{"a.md", "b.md"}, "docs", "b.md", ""},
		{"inferred", ``, []string{"index.md", "image.png"}, "docs", "index.md", ""},
		{"inferred nested", `{}`, []string{"notes/a.md"}, "docs", "notes/a.md", ""},
		{"website remains", `{}`, []string{"index.html", "index.md"}, "flat", "index.html", ""},
		{"custom static alias remains", `{"entry":"./notes.md"}`, []string{"notes.md"}, "flat", "./notes.md", ""},
		{"custom static remains", `{"entry":"notes.md"}`, []string{"notes.md"}, "flat", "notes.md", ""},
		{"server remains", `{"kind":"server"}`, []string{"server.js", "index.md"}, "flat", "server.js", ""},
		{"explicit flat", `{"type":"flat"}`, []string{"index.md"}, "", "", "no index.html"},
		{"no server inference", `{}`, []string{"server.js", "index.md"}, "", "", "no index.html"},
		{"no kind inference", `{"kind":""}`, []string{"index.md"}, "", "", "no index.html"},
		{"ambiguous", `{"type":"docs"}`, []string{"a.md", "b.md"}, "", "", "entry"},
		{"kind rejected", `{"type":"docs","kind":"server"}`, []string{"index.md"}, "", "", "kind is not allowed"},
		{"spa false rejected", `{"type":"docs","spa":false}`, []string{"index.md"}, "", "", "spa is not allowed"},
		{"notfound empty rejected", `{"type":"docs","not_found":""}`, []string{"index.md"}, "", "", "not_found is not allowed"},
		{"health default", `{"type":"docs","health":"/_docs/healthz"}`, []string{"index.md"}, "docs", "index.md", ""},
		{"health override", `{"type":"docs","health":"/"}`, []string{"index.md"}, "", "", "docs health"},
		{"entry not markdown", `{"type":"docs","entry":"x.txt"}`, []string{"x.txt"}, "", "", "entry"},
		{"missing entry", `{"type":"docs","entry":"missing.md"}`, []string{"index.md"}, "", "", "entry"},
		{"reserved", `{"type":"docs"}`, []string{"index.md", "_docs/asset.png"}, "", "", "reserved"},
		{"unknown type", `{"type":"slides"}`, []string{"index.html"}, "", "", "unknown type"},
		{"bad type", `{"type":true}`, []string{"index.html"}, "", "", "must be a string"},
		{"empty type", `{"type":""}`, []string{"index.html"}, "", "", "type must"},
		{"null type", `{"type":null}`, []string{"index.html"}, "", "", "type must"},
		{"screenshot", `{"type":"docs","screenshot":"shot.png"}`, []string{"index.md"}, "", "", "screenshot file is missing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := []File{}
			if tt.manifest != "" {
				files = append(files, File{Path: ManifestName, Data: []byte(tt.manifest)})
			}
			for _, p := range tt.paths {
				files = append(files, File{Path: p, Data: []byte("# title")})
			}
			m, err := ParseManifest(files)
			if tt.problem != "" {
				v, ok := IsValidation(err)
				if !ok || !strings.Contains(err.Error(), tt.problem) {
					t.Fatalf("manifest %+v: %v", m, err)
				}
				for _, p := range v.Problems {
					if p.Fix == "" {
						t.Fatalf("no fix: %+v", p)
					}
				}
				return
			}
			if err != nil || m.Type != tt.typ || m.Entry != tt.entry {
				t.Fatalf("manifest %+v: %v", m, err)
			}
			if tt.typ == "docs" && (m.Kind != "server" || m.Health != "/_docs/healthz") {
				t.Fatalf("docs defaults: %+v", m)
			}
		})
	}
}

func TestDocsMarkdownLimits(t *testing.T) {
	tests := []struct {
		name    string
		files   []File
		problem string
	}{
		{"invalid UTF8", []File{{Path: "index.md", Data: []byte{0xff}}}, "UTF-8"},
		{"per document", []File{{Path: "index.md", Data: []byte(strings.Repeat("x", 1<<20+1))}}, "1 MiB"},
		{"total", nil, "4 MiB"},
		{"at limits", nil, ""},
	}
	for i := 0; i < 4; i++ {
		tests[2].files = append(tests[2].files, File{Path: fmt.Sprintf("%d.md", i), Data: []byte(strings.Repeat("x", 1<<20))})
		tests[3].files = append(tests[3].files, tests[2].files[i])
	}
	tests[2].files = append(tests[2].files, File{Path: "extra.md", Data: []byte("x")})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := append([]File{{Path: ManifestName, Data: []byte(`{"type":"docs","entry":"0.md"}`)}}, tt.files...)
			if len(tt.files) == 1 {
				files[0].Data = []byte(`{"type":"docs"}`)
			}
			_, err := ParseManifest(files)
			if tt.problem == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			v, ok := IsValidation(err)
			if !ok || !strings.Contains(err.Error(), tt.problem) {
				t.Fatal(err)
			}
			for _, p := range v.Problems {
				if p.Fix == "" {
					t.Fatal("no fix")
				}
			}
		})
	}
}

func TestDocsWriteRetainsUpload(t *testing.T) {
	files := []File{{Path: ManifestName, Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte("# exact\n한글 😀\n")}}
	dir := filepath.Join(t.TempDir(), "version")
	res, err := Write(files, dir)
	if err != nil || res.Manifest.Kind != "server" {
		t.Fatalf("%+v %v", res, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("host modules leaked into version: %v", entries)
	}
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(dir, f.Path))
		if err != nil || string(b) != string(f.Data) {
			t.Fatalf("file changed: %s %v", b, err)
		}
	}
}

func TestDocsDocumentCountLimit(t *testing.T) {
	for _, count := range []int{128, 129, 131} {
		files := []File{{Path: ManifestName, Data: []byte(`{"type":"docs","entry":"0.md"}`)}}
		for i := 0; i < count; i++ {
			files = append(files, File{Path: fmt.Sprintf("%d.md", i), Data: []byte("x")})
		}
		_, err := ParseManifest(files)
		if count == 128 {
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		v, ok := IsValidation(err)
		if !ok || !strings.Contains(err.Error(), "128 documents") {
			t.Fatal(err)
		}
		for _, problem := range v.Problems {
			if problem.Fix == "" {
				t.Fatal("missing fix hint")
			}
		}
	}
}
