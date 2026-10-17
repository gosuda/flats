package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gosuda/flats/internal/bundle"
)

func TestSaveDocumentPreservesBundle(t *testing.T) {
	t.Parallel()
	s, _ := newTestService(t)
	rt := &contentRuntime{}
	s.cfg.Runtime, s.cfg.DocsApp = rt, testDocsApp()
	ctx := t.Context()
	manifest := []byte("{\n  \"type\": \"docs\", \"name\": \"Handbook\", \"entry\": \"guide.markdown\", \"screenshot\": \"images/chart.png\", \"health\": \"/_docs/healthz\"\n}\n")
	asset := []byte{0, 255, 1, 128}
	initial := []bundle.File{{Path: "flats.json", Data: manifest}, {Path: "guide.markdown", Data: []byte("guide")}, {Path: "notes.md", Data: []byte("notes")}, {Path: "images/chart.png", Data: asset}}
	if _, err := s.SaveVersion(ctx, "handbook", initial, SaveMeta{}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	read := func(path string) []byte {
		t.Helper()
		f, err := s.st.GetFlat(ctx, "handbook")
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(s.versionDir("handbook", f.LiveVersion), path))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, path := range []string{"notes.md", "새 문서/chapter.markdown", "index.md"} {
		if _, err := s.SaveDocument(ctx, "handbook", path, "", "", SaveMeta{Message: "one document"}, ViaMCP); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(read("flats.json"), manifest) || !bytes.Equal(read("images/chart.png"), asset) || string(read("guide.markdown")) != "guide" {
			t.Fatal("sibling or configuration changed")
		}
		if string(read(path)) != "" {
			t.Fatal("empty document was not saved")
		}
	}
	if _, err := s.SaveDocument(ctx, "handbook", "notes.md", "renamed", "New title", SaveMeta{}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	json.Unmarshal(read("flats.json"), &got)
	json.Unmarshal(manifest, &want)
	want["name"] = "New title"
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("title changed settings: %v", got)
	}
	// A rejected candidate never changes the active source. A startup failure
	// may leave a newer internal Draft; the next partial save must ignore it.
	rt.fail = true
	if _, err := s.SaveDocument(ctx, "handbook", "failed.md", "not active", "", SaveMeta{}, ViaMCP); err == nil {
		t.Fatal("startup failure accepted")
	}
	rt.fail = false
	if _, err := s.SaveDocument(ctx, "handbook", "notes.md", "recovered", "", SaveMeta{}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	f, _ := s.st.GetFlat(ctx, "handbook")
	if _, err := os.Stat(filepath.Join(s.versionDir("handbook", f.LiveVersion), "failed.md")); !os.IsNotExist(err) {
		t.Fatal("failed candidate adopted", err)
	}
	// An explicit whole-bundle save still deletes omissions.
	if _, err := s.SaveVersion(ctx, "handbook", []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte("replacement")}}, SaveMeta{}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	f, _ = s.st.GetFlat(ctx, "handbook")
	if _, err := os.Stat(filepath.Join(s.versionDir("handbook", f.LiveVersion), "guide.markdown")); !os.IsNotExist(err) {
		t.Fatal("whole-bundle save retained omitted file", err)
	}
}

func TestSaveDocumentKeepsImplicitEntry(t *testing.T) {
	t.Parallel()
	s, _ := newTestService(t)
	s.cfg.Runtime, s.cfg.DocsApp = &contentRuntime{}, testDocsApp()
	ctx := t.Context()
	if _, err := s.SaveDocument(ctx, "implicit", "z.md", "entry", "", SaveMeta{}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	v, err := s.SaveDocument(ctx, "implicit", "a.md", "sibling", "", SaveMeta{}, ViaMCP)
	if err != nil {
		t.Fatal(err)
	}
	var m bundle.Manifest
	if err := json.Unmarshal(v.Manifest, &m); err != nil || m.Entry != "z.md" {
		t.Fatalf("entry changed: %+v %v", m, err)
	}
}

func TestSaveDocumentConcurrentSiblings(t *testing.T) {
	t.Parallel()
	s, _ := newTestService(t)
	s.cfg.Runtime, s.cfg.DocsApp = &contentRuntime{}, testDocsApp()
	ctx := t.Context()
	if _, err := s.SaveDocument(ctx, "concurrent", "", "entry", "", SaveMeta{}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	const n = 8
	start := make(chan struct{})
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			_, err := s.SaveDocument(ctx, "concurrent", fmt.Sprintf("notes/%d.md", i), fmt.Sprint(i), "", SaveMeta{}, ViaMCP)
			errs <- err
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	f, _ := s.st.GetFlat(ctx, "concurrent")
	for i := range n {
		b, err := os.ReadFile(filepath.Join(s.versionDir("concurrent", f.LiveVersion), fmt.Sprintf("notes/%d.md", i)))
		if err != nil || string(b) != fmt.Sprint(i) {
			t.Fatalf("lost sibling %d: %q %v", i, b, err)
		}
	}
}

func TestSaveDocumentRejectsCombinedLimits(t *testing.T) {
	t.Parallel()
	s, _ := newTestService(t)
	s.cfg.Runtime, s.cfg.DocsApp = &contentRuntime{}, testDocsApp()
	ctx := t.Context()
	files := []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs","entry":"doc-000.md"}`)}}
	for i := range 128 {
		files = append(files, bundle.File{Path: fmt.Sprintf("doc-%03d.md", i), Data: []byte("old")})
	}
	v, err := s.SaveVersion(ctx, "capacity", files, SaveMeta{}, ViaMCP)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, path, text string }{{"document count", "one-more.md", "new"}, {"per-document size", "doc-000.md", strings.Repeat("x", (1<<20)+1)}} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.SaveDocument(ctx, "capacity", tc.path, tc.text, "", SaveMeta{}, ViaMCP); err == nil {
				t.Fatal("combined limit accepted")
			}
			d, _ := s.st.GetDraft(ctx, "capacity")
			if d.Hash != v.Hash {
				t.Fatal("rejected save changed source")
			}
		})
	}
	large := []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs","entry":"doc-000.md"}`)}}
	for i := range 4 {
		large = append(large, bundle.File{Path: fmt.Sprintf("doc-%03d.md", i), Data: []byte(strings.Repeat("x", 1<<20))})
	}
	if _, err := s.SaveVersion(ctx, "total-limit", large, SaveMeta{}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveDocument(ctx, "total-limit", "new.md", "x", "", SaveMeta{}, ViaMCP); err == nil || !strings.Contains(err.Error(), "Markdown total exceeds") {
		t.Fatalf("aggregate limit: %v", err)
	}

	// Wrong types and invalid paths cannot change an existing flat.
	if _, err := s.SaveVersion(ctx, "website", []bundle.File{{Path: "index.html", Data: []byte("old")}}, SaveMeta{}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveDocument(ctx, "website", "index.md", "bad", "", SaveMeta{}, ViaMCP); err == nil {
		t.Fatal("website converted")
	}
	if _, err := s.SaveDocument(ctx, "capacity", "_docs/new.md", "bad", "", SaveMeta{}, ViaMCP); err == nil {
		t.Fatal("reserved path accepted")
	}
}

func TestSaveDocumentLegacyManifests(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`null`, `{"name":"Notes"}`} {
		for _, tc := range []struct{ name, path, title string }{{"title", "README.md", "New title"}, {"default path", "", ""}, {"second document", "notes.md", ""}} {
			t.Run(raw+"/"+tc.name, func(t *testing.T) {
				s, _ := newTestService(t)
				s.cfg.Runtime, s.cfg.DocsApp = &contentRuntime{}, testDocsApp()
				ctx := t.Context()
				if _, err := s.SaveVersion(ctx, "legacy", []bundle.File{{Path: "flats.json", Data: []byte(raw)}, {Path: "README.md", Data: []byte("entry")}}, SaveMeta{}, ViaMCP); err != nil {
					t.Fatal(err)
				}
				v, err := s.SaveDocument(ctx, "legacy", tc.path, "new", tc.title, SaveMeta{}, ViaMCP)
				if err != nil {
					t.Fatal(err)
				}
				var m bundle.Manifest
				if err := json.Unmarshal(v.Manifest, &m); err != nil || m.Type != "docs" || m.Entry != "README.md" {
					t.Fatalf("type/entry: %+v %v", m, err)
				}
			})
		}
	}
}

func TestSaveDocumentUsesUnactivatedDraft(t *testing.T) {
	t.Parallel()
	s, _ := newTestService(t)
	rt := &contentRuntime{fail: true}
	s.cfg.Runtime, s.cfg.DocsApp = rt, testDocsApp()
	ctx := t.Context()
	if _, err := s.SaveDocument(ctx, "not-started", "README.md", "saved before start", "", SaveMeta{}, ViaMCP); err == nil {
		t.Fatal("startup failure accepted")
	}
	f, _ := s.st.GetFlat(ctx, "not-started")
	if f.LiveVersion != 0 {
		t.Fatal("unexpected live version")
	}
	rt.fail = false
	if _, err := s.SaveDocument(ctx, "not-started", "notes.md", "new", "", SaveMeta{}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	f, _ = s.st.GetFlat(ctx, "not-started")
	b, err := os.ReadFile(filepath.Join(s.versionDir("not-started", f.LiveVersion), "README.md"))
	if err != nil || string(b) != "saved before start" {
		t.Fatalf("Draft sibling lost: %q %v", b, err)
	}
}

func TestSaveDocumentRefusalsPreserveSource(t *testing.T) {
	t.Parallel()
	s, _ := newTestService(t)
	s.cfg.Runtime, s.cfg.DocsApp = &contentRuntime{}, testDocsApp()
	ctx := t.Context()
	v, err := s.SaveVersion(ctx, "refusals", []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte("entry")}, {Path: "a/b.md", Data: []byte("nested")}, {Path: "asset.txt", Data: []byte("asset")}, {Path: "folder.md/child.md", Data: []byte("nested folder")}}, SaveMeta{}, ViaMCP)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"._hidden.md", ".git/hidden.md", "node_modules/hidden.md", "__MACOSX/hidden.md", `a\b.md`, "asset.txt/child.md", "folder.md", "_docs/hidden.md"} {
		t.Run(path, func(t *testing.T) {
			_, err := s.SaveDocument(ctx, "refusals", path, "bad", "", SaveMeta{}, ViaMCP)
			_, validation := bundle.IsValidation(err)
			if err == nil || !validation && ErrorCategory(err) != "invalid" || strings.Contains(err.Error(), "upload_max_bytes") {
				t.Fatalf("invalid save refusal: %v", err)
			}
			if path == "asset.txt/child.md" || path == "folder.md" {
				if _, ok := bundle.IsValidation(err); !ok {
					t.Fatalf("collision is not a validation refusal: %v", err)
				}
			}
			d, _ := s.st.GetDraft(ctx, "refusals")
			if d.Hash != v.Hash {
				t.Fatal("failed save changed Draft")
			}
		})
	}
	if _, err := s.UpdateSettings(ctx, map[string]string{SetDiskQuotaBytes: "1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveDocument(ctx, "refusals", "index.md", "new", "", SaveMeta{}, ViaMCP); err == nil || !strings.Contains(err.Error(), "per-flat disk quota") || strings.Contains(err.Error(), "upload_max_bytes") {
		t.Fatalf("disk quota refusal: %v", err)
	}
	d, _ := s.st.GetDraft(ctx, "refusals")
	if d.Hash != v.Hash {
		t.Fatal("quota failure changed source")
	}
}

// The console sends the per-flat disk quota as an exact byte count: a
// small quota is stored as given, and 0 turns the quota off.
func TestDiskQuotaSettingExactAndZeroIsOff(t *testing.T) {
	s, _ := newTestService(t)
	s.cfg.Runtime, s.cfg.DocsApp = &contentRuntime{}, testDocsApp()
	ctx := t.Context()
	if _, err := s.SaveDocument(ctx, "quota-off", "index.md", "saved", "", SaveMeta{}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	all, err := s.UpdateSettings(ctx, map[string]string{SetDiskQuotaBytes: "107374"})
	if err != nil {
		t.Fatal(err)
	}
	if got := all[SetDiskQuotaBytes]; got != "107374" {
		t.Fatalf("stored quota %q, want the exact byte count", got)
	}
	if _, err := s.UpdateSettings(ctx, map[string]string{SetDiskQuotaBytes: "1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveDocument(ctx, "quota-off", "index.md", "over quota", "", SaveMeta{}, ViaMCP); err == nil || !strings.Contains(err.Error(), "disk quota") {
		t.Fatalf("1-byte quota must refuse the save: %v", err)
	}
	if all, err = s.UpdateSettings(ctx, map[string]string{SetDiskQuotaBytes: "0"}); err != nil || all[SetDiskQuotaBytes] != "0" {
		t.Fatalf("quota off: %q %v", all[SetDiskQuotaBytes], err)
	}
	if _, err := s.SaveDocument(ctx, "quota-off", "index.md", strings.Repeat("no quota ", 1000), "", SaveMeta{}, ViaMCP); err != nil {
		t.Fatalf("0 must mean no quota: %v", err)
	}
}

func TestSaveDocumentDiskQuotaGuidance(t *testing.T) {
	s, _ := newTestService(t)
	s.cfg.Runtime, s.cfg.DocsApp = &contentRuntime{}, testDocsApp()
	ctx := t.Context()
	if _, err := s.SaveDocument(ctx, "quota-guidance", "index.md", "saved", "", SaveMeta{}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	before, err := s.st.GetDraft(ctx, "quota-guidance")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSettings(ctx, map[string]string{SetDiskQuotaBytes: "1"}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"single-document", "complete-bundle"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "single-document" {
				_, err = s.SaveDocument(ctx, "quota-guidance", "index.md", "replacement", "", SaveMeta{}, ViaMCP)
			} else {
				_, err = s.SaveVersion(ctx, "quota-guidance", []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte("replacement")}}, SaveMeta{}, ViaMCP)
			}
			validation, ok := bundle.IsValidation(err)
			if !ok || len(validation.Problems) != 1 || !strings.Contains(validation.Problems[0].Message, "disk quota") {
				t.Fatalf("disk quota must remain a structured upload validation refusal: %T %v", err, err)
			}
			fix := validation.Problems[0].Fix
			if fix != "raise the per-flat disk quota in system settings to provide room for this save" {
				t.Fatalf("already-over-quota refusal must offer a usable remedy: %s", fix)
			}
			after, err := s.st.GetDraft(ctx, "quota-guidance")
			if err != nil || after.Hash != before.Hash || after.Revision != before.Revision {
				t.Fatalf("quota refusal changed saved source: %+v %v", after, err)
			}
		})
	}
}

func TestSaveDocumentCanShrinkOverLimitSource(t *testing.T) {
	t.Parallel()
	t.Run("lowered upload limit", func(t *testing.T) {
		s, _ := newTestService(t)
		s.cfg.Runtime, s.cfg.DocsApp = &contentRuntime{}, testDocsApp()
		ctx := t.Context()
		if _, err := s.SaveVersion(ctx, "shrink", []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte(strings.Repeat("x", 5000))}, {Path: "notes.md", Data: []byte("sibling")}}, SaveMeta{}, ViaMCP); err != nil {
			t.Fatal(err)
		}
		if _, err := s.UpdateSettings(ctx, map[string]string{SetUploadMaxBytes: "4096"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SaveDocument(ctx, "shrink", "new.md", "new", "", SaveMeta{}, ViaMCP); err == nil || !strings.Contains(err.Error(), "retains the other files") {
			t.Fatalf("over-limit recovery guidance: %v", err)
		}
		if _, err := s.SaveDocument(ctx, "shrink", "index.md", "small", "", SaveMeta{}, ViaMCP); err != nil {
			t.Fatal("shrinking save blocked", err)
		}
	})
	t.Run("historical Markdown total", func(t *testing.T) {
		s, _ := newTestService(t)
		s.cfg.Runtime, s.cfg.DocsApp = &contentRuntime{}, testDocsApp()
		ctx := t.Context()
		if _, err := s.SaveDocument(ctx, "historical", "", "initial", "", SaveMeta{}, ViaMCP); err != nil {
			t.Fatal(err)
		}
		files := []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs","entry":"doc-0.md"}`)}}
		for i := range 5 {
			files = append(files, bundle.File{Path: fmt.Sprintf("doc-%d.md", i), Data: []byte(strings.Repeat("x", 900<<10))})
		}
		slices.SortFunc(files, func(a, b bundle.File) int { return strings.Compare(a.Path, b.Path) })
		// This is the same historical bundle path used by a cross-shape rollback.
		unlock, err := s.lockStorage("historical")
		if err != nil {
			t.Fatal(err)
		}
		v, err := s.saveDraftLocked(ctx, "historical", files, SaveMeta{historical: true}, ViaMCP)
		if err == nil {
			err = s.applyDocsLocked(ctx, "historical", v.Revision, ViaMCP)
		}
		unlock()
		if err != nil {
			t.Fatal(err)
		}
		before, _ := s.st.GetDraft(ctx, "historical")
		if _, err := s.SaveDocument(ctx, "historical", "doc-0.md", strings.Repeat("x", 899<<10), "", SaveMeta{}, ViaMCP); err == nil || !strings.Contains(err.Error(), "within 4 MiB") || strings.Contains(err.Error(), "upload_max_bytes") {
			t.Fatalf("insufficient historical shrink refusal: %v", err)
		}
		after, _ := s.st.GetDraft(ctx, "historical")
		if after.Hash != before.Hash {
			t.Fatal("refused shrink changed source")
		}
		if _, err := s.SaveDocument(ctx, "historical", "doc-0.md", "small", "", SaveMeta{}, ViaMCP); err != nil {
			t.Fatal("historical shrinking save blocked", err)
		}
		f, _ := s.st.GetFlat(ctx, "historical")
		raw, err := os.ReadFile(filepath.Join(s.versionDir("historical", f.LiveVersion), "flats.json"))
		if err != nil || string(raw) != `{"type":"docs","entry":"doc-0.md"}` {
			t.Fatalf("size-only edit rewrote manifest: %q %v", raw, err)
		}
	})
}

func TestSaveDocumentKeepsTypeInferredFromSingleMarkdown(t *testing.T) {
	t.Parallel()
	s, _ := newTestService(t)
	s.cfg.Runtime, s.cfg.DocsApp = &contentRuntime{}, testDocsApp()
	ctx := t.Context()
	if _, err := s.SaveVersion(ctx, "inferred", []bundle.File{{Path: "flats.json", Data: []byte(`{"name":"Notes"}`)}, {Path: "z.md", Data: []byte("entry")}}, SaveMeta{}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	v, err := s.SaveDocument(ctx, "inferred", "a.md", "new", "", SaveMeta{}, ViaMCP)
	if err != nil {
		t.Fatal(err)
	}
	var m bundle.Manifest
	if err := json.Unmarshal(v.Manifest, &m); err != nil || m.Type != "docs" || m.Entry != "z.md" {
		t.Fatalf("lost inferred type/entry: %+v %v", m, err)
	}
}

func TestSaveDocumentManifestDirectoryCollision(t *testing.T) {
	t.Parallel()
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			s, _ := newTestService(t)
			s.cfg.Runtime, s.cfg.DocsApp = &contentRuntime{}, testDocsApp()
			ctx := t.Context()
			var original string
			if existing {
				v, err := s.SaveVersion(ctx, "manifest-collision", []bundle.File{{Path: "README.md", Data: []byte("entry without manifest")}}, SaveMeta{}, ViaMCP)
				if err != nil {
					t.Fatal(err)
				}
				original = v.Hash
			}
			_, err := s.SaveDocument(ctx, "manifest-collision", "flats.json/x.md", "new", "", SaveMeta{}, ViaMCP)
			if _, ok := bundle.IsValidation(err); !ok {
				t.Fatalf("manifest collision is not a validation refusal: %v", err)
			}
			if existing {
				d, _ := s.st.GetDraft(ctx, "manifest-collision")
				if d.Hash != original {
					t.Fatal("collision changed saved source")
				}
			} else {
				if _, err := s.st.GetFlat(ctx, "manifest-collision"); err == nil {
					t.Fatal("invalid first save created a flat")
				}
			}
		})
	}
}
