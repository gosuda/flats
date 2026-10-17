package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/store"
)

type contentRuntime struct {
	specs       []RuntimeSpec
	fail        bool
	afterEdit   func()           // runs after the fake app answers a live edit
	budgets     []map[string]any // disk budgets the host told the fake app
	noBudget    bool             // the fake app refuses budgets
	checkpoints int              // write-ahead log truncations the host asked for
	outbox      *fakeOutbox      // when set, the fake app serves revision capture
}

// fakeOutbox is a docs worker's unacknowledged revision entries.
type fakeOutbox struct {
	entries []docsEntry
	texts   map[string]string
	acks    []map[string]any
}

func (o *fakeOutbox) serve(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/_docs/host/revisions/sync":
		_ = json.NewEncoder(w).Encode(map[string]any{"store": "fake-store", "ready": len(o.entries), "intent": nil})
	case "/_docs/host/revisions/outbox":
		_ = json.NewEncoder(w).Encode(map[string]any{"store": "fake-store", "entries": o.entries, "more": false})
	case "/_docs/host/revisions/text":
		text, ok := o.texts[r.URL.Query().Get("hash")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"hash": r.URL.Query().Get("hash"), "text": text})
	case "/_docs/host/revisions/ack":
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		o.acks = append(o.acks, b)
		_, _ = io.WriteString(w, `{"ok":true}`)
	default:
		return false
	}
	return true
}

type contentInstance struct{ http.Handler }

func (contentInstance) Stop() {}
func (rt *contentRuntime) Start(_ context.Context, spec RuntimeSpec) (Instance, error) {
	rt.specs = append(rt.specs, spec)
	if rt.fail {
		return nil, errors.New("start failed")
	}
	return contentInstance{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_docs/api/receipt" {
			receiptResponse(w, r)
			return
		}
		if rt.outbox != nil && rt.outbox.serve(w, r) {
			return
		}
		if r.URL.Path == "/_docs/host/revisions/checkpoint" {
			rt.checkpoints++
			_, _ = io.WriteString(w, `{"busy":false}`)
			return
		}
		if r.URL.Path == "/_docs/host/revisions/budget" {
			if rt.noBudget {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			rt.budgets = append(rt.budgets, b)
			_, _ = io.WriteString(w, `{"ok":true}`)
			return
		}
		if r.URL.Path == "/_docs/api/edit" {
			editResponse(w, r)
			if rt.afterEdit != nil {
				rt.afterEdit()
			}
			return
		}
		if r.URL.Path == "/_docs/api/document" {
			_ = json.NewEncoder(w).Encode(Document{Format: 1, Doc: "index.md", Markdown: "live people's edits", Source: "live", Epoch: "epoch", Seq: 7})
			return
		}
		fmt.Fprint(w, r.Header.Get("X-Flats-Access"))
	})}, nil
}
func testDocsApp() DocsApp {
	return DocsApp{FS: fstest.MapFS{"server.js": &fstest.MapFile{Data: []byte("import c from './content.js'; export default {fetch(){ return new Response(c.entry); }}")}, "client/x.js": &fstest.MapFile{Data: []byte("client")}}, Hash: strings.Repeat("a", 64), Entry: "server.js", ContentModule: "content.js"}
}
func saveDocs(t *testing.T, s *Service, slug string) store.Version {
	t.Helper()
	v, err := s.SaveVersion(context.Background(), slug, []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs","name":"Title"}`)}, {Path: "index.md", Data: []byte("# main\n한글 😀")}, {Path: "a.md", Data: []byte("# second")}, {Path: "images/a.txt", Data: []byte("asset")}}, SaveMeta{}, ViaAPI)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// saveDocsDraft saves the saveDocs bundle as a Draft without starting it,
// for tests of the docs runtime itself.
func saveDocsDraft(t *testing.T, s *Service, slug string) store.Version {
	t.Helper()
	files, err := bundle.FromFiles([]bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs","name":"Title"}`)}, {Path: "index.md", Data: []byte("# main\n한글 😀")}, {Path: "a.md", Data: []byte("# second")}, {Path: "images/a.txt", Data: []byte("asset")}}, bundle.Limits{MaxBytes: s.UploadLimit()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.st.GetFlat(t.Context(), slug); errors.Is(err, store.ErrNotFound) {
		if _, err := s.CreateFlat(t.Context(), slug, "", ViaAPI); err != nil {
			t.Fatal(err)
		}
	}
	unlock := s.lock(slug)
	defer unlock()
	v, err := s.saveDraftLocked(t.Context(), slug, files, SaveMeta{}, ViaAPI)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestDocsRuntimeLayoutReuseAndCleanup(t *testing.T) {
	s, dir := newTestService(t)
	rt := &contentRuntime{}
	s.cfg.Runtime = rt
	s.cfg.DocsApp = testDocsApp()
	v := saveDocsDraft(t, s, "notes")
	d, err := s.build(context.Background(), "notes", v, filepath.Join(dir, "isolated"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.inst.Stop()
	spec := rt.specs[0]
	expected := filepath.Join(dir, "runtime", "docs", strings.Repeat("a", 12)+"-"+v.Hash[:16])
	if spec.Dir != expected || spec.Entry != "server.js" || spec.DataDir != filepath.Join(dir, "isolated") {
		t.Fatalf("%+v", spec)
	}
	b, err := os.ReadFile(filepath.Join(expected, "content.js"))
	if err != nil {
		t.Fatal(err)
	}
	var c docsContent
	if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(string(b), "export default "), ";\n")), &c); err != nil {
		t.Fatal(err)
	}
	if c.Format != 1 || c.Hash != v.Hash || c.Entry != "index.md" || c.Title != "Title" || len(c.Documents) != 2 || c.Documents[0].Path != "a.md" || c.Documents[1].Hash != fmt.Sprintf("%x", sha256.Sum256([]byte("# main\n한글 😀"))) || len(c.Assets) != 1 || c.Assets[0] != "images/a.txt" {
		t.Fatalf("%+v", c)
	}
	err = filepath.WalkDir(expected, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			i, _ := d.Info()
			if i.Mode().Perm() != 0444 {
				t.Errorf("mode %s: %v", p, i.Mode())
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(filepath.Join(expected, "content.js"))
	d2, err := s.build(context.Background(), "notes", v, filepath.Join(dir, "isolated2"), nil)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(filepath.Join(expected, "content.js"))
	if !os.SameFile(before, after) {
		t.Fatal("runtime was rewritten instead of reused")
	}
	s.cleanupDocs()
	if _, err := os.Stat(expected); err != nil {
		t.Fatal("cleanup removed in-use directory")
	}
	d.inst.Stop()
	d.inst.Stop()
	if _, err := os.Stat(expected); err != nil {
		t.Fatal("first release removed shared directory")
	}
	d2.inst.Stop()
	if _, err := os.Stat(expected); !os.IsNotExist(err) {
		t.Fatal("last release retained unused directory")
	}
	rt.fail = true
	if _, err := s.build(context.Background(), "notes", v, filepath.Join(dir, "fail"), nil); err == nil {
		t.Fatal("expected start failure")
	}
	if _, err := os.Stat(expected); !os.IsNotExist(err) {
		t.Fatal("failed Start leaked directory")
	}
	stale := filepath.Join(dir, "runtime", "docs", "stale")
	_ = os.MkdirAll(stale, 0700)
	s.cleanupDocs()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale directory retained")
	}
}

func TestDocsHostAssetRouting(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "image.txt"), []byte("asset"), 0444)
	h := docsAssets(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "worker") }), dir, []string{"image.txt"})
	for _, tt := range []struct{ name, method, path, body string }{{"get asset", "GET", "/image.txt", "asset"}, {"head asset", "HEAD", "/image.txt", ""}, {"post asset", "POST", "/image.txt", "worker"}, {"markdown worker", "GET", "/index.md", "worker"}, {"internal worker", "GET", "/_docs/healthz", "worker"}, {"not listed", "GET", "/other.txt", "worker"}} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRecorder()
			h.ServeHTTP(r, httptest.NewRequest(tt.method, tt.path, nil))
			if r.Code != 200 || r.Body.String() != tt.body {
				t.Fatalf("%d %q", r.Code, r.Body.String())
			}
		})
	}
	r := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/image.txt", nil)
	req.Header.Set("Range", "bytes=1-3")
	h.ServeHTTP(r, req)
	if r.Code != 206 || r.Body.String() != "sse" || r.Header().Get("ETag") == "" {
		t.Fatalf("asset semantics: %d %s", r.Code, r.Body.String())
	}
}

func TestTrustedAccessLiveAndPreview(t *testing.T) {
	s, _ := newTestService(t)
	rt := &contentRuntime{}
	s.cfg.Runtime = rt
	s.cfg.DocsApp = testDocsApp()
	saveDocs(t, s, "access")
	if _, err := s.OpenPreview(context.Background(), "access", 0); !errors.Is(err, ErrDocsUnversioned) {
		t.Fatal("document preview:", err)
	}
	flat, _ := s.st.GetFlat(context.Background(), "access")
	flat.Visibility = store.Public
	_ = s.st.UpdateFlat(context.Background(), flat)
	for _, tt := range []struct {
		name string
		h    http.Handler
		want string
	}{{"private", s.siteHandler("access", false), "private"}, {"public", s.siteHandler("access", true), "public"}} {
		t.Run(tt.name, func(t *testing.T) {
			for _, forged := range []string{"public", "private"} {
				r := httptest.NewRequest("GET", "/", nil)
				r.Header.Set("X-Flats-Access", forged)
				r.Header["x-flats-access"] = []string{"forged"}
				rec := httptest.NewRecorder()
				tt.h.ServeHTTP(rec, r)
				if rec.Code != 200 || rec.Body.String() != tt.want {
					t.Fatalf("forged %s -> %d %q", forged, rec.Code, rec.Body.String())
				}
			}
		})
	}
}

func TestGetDocumentAndFlatType(t *testing.T) {
	s, _ := newTestService(t)
	s.cfg.Runtime = &contentRuntime{}
	s.cfg.DocsApp = testDocsApp()
	ctx := context.Background()
	if _, err := s.CreateFlat(ctx, "empty", "", ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDocument(ctx, "empty", ""); !errors.Is(err, ErrNotDeployed) {
		t.Fatal(err)
	}
	if _, err := s.SaveVersion(ctx, "website", []bundle.File{{Path: "index.html", Data: []byte("web")}}, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDocument(ctx, "website", ""); !errors.Is(err, ErrNotDocs) {
		t.Fatal(err)
	}
	if _, err := approvedInternalDeploy(t, s, ctx, "website", 0); err != nil {
		t.Fatal(err)
	}
	// A running website cannot become a document.
	if _, err := s.SaveVersion(ctx, "website", []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte("# doc")}}, SaveMeta{}, ViaAPI); !errors.Is(err, ErrTypeChange) {
		t.Fatal("website to document:", err)
	}
	// A docs Draft left over a running website by an older release does not
	// make it a document: the running version decides the type.
	saveDocsDraft(t, s, "website")
	if f, _ := s.GetFlat(ctx, "website"); f.Type != "flat" {
		t.Fatalf("legacy docs Draft over a running website: %+v", f.Type)
	}
	if _, err := s.GetDraft(ctx, "website"); err != nil {
		t.Fatal("website Draft:", err)
	}
	// Saving a document runs it at once, Private, without approval.
	saveDocs(t, s, "notes")
	f, _ := s.GetFlat(ctx, "notes")
	if f.Type != "docs" || f.LiveVersion == 0 || f.Visibility.Public() {
		t.Fatalf("saved document is not running privately: %+v", f)
	}
	if as, _ := s.st.ListApprovals(ctx, "pending"); len(as) != 0 {
		t.Fatalf("document save requested approval: %+v", as)
	}
	out, err := s.GetDocument(ctx, "notes", "")
	if err != nil || out.Source != "live" || out.Markdown != "live people's edits" || out.Seq != 7 {
		t.Fatalf("%+v %v", out, err)
	}
	if _, err := s.GetDocument(ctx, "notes", "../index.md"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	// Saving the same content again keeps the running document.
	live := f.LiveVersion
	saveDocs(t, s, "notes")
	if f, _ := s.GetFlat(ctx, "notes"); f.LiveVersion != live {
		t.Fatalf("unchanged save restarted the document: %d -> %d", live, f.LiveVersion)
	}
	// A document cannot become a website, and has no versions or publishing.
	if _, err := s.SaveVersion(ctx, "notes", []bundle.File{{Path: "index.html", Data: []byte("website draft")}}, SaveMeta{}, ViaAPI); !errors.Is(err, ErrTypeChange) {
		t.Fatal("document to website:", err)
	}
	if _, err := s.GetDraft(ctx, "notes"); !errors.Is(err, ErrDocsUnversioned) {
		t.Fatal(err)
	}
	if _, err := s.ListVersions(ctx, "notes"); !errors.Is(err, ErrDocsUnversioned) {
		t.Fatal(err)
	}
	if _, err := s.Publish(ctx, "notes", 0, "", ViaAPI); !errors.Is(err, ErrDocsUnversioned) {
		t.Fatal(err)
	}
	if _, err := s.Deploy(ctx, "notes", 0, ViaAPI); !errors.Is(err, ErrDocsUnversioned) {
		t.Fatal(err)
	}
	if _, err := s.Rollback(ctx, "notes", 0, ViaAPI); !errors.Is(err, ErrDocsUnversioned) {
		t.Fatal(err)
	}
	if _, err := s.ListVersions(ctx, "website"); err != nil {
		t.Fatal("website versions:", err)
	}
}

func TestSavedDocumentDraftStartsOnRestart(t *testing.T) {
	s, _ := newTestService(t)
	s.cfg.Runtime = &contentRuntime{}
	s.cfg.DocsApp = testDocsApp()
	ctx := t.Context()
	saveDocsDraft(t, s, "legacy-doc")
	if out, err := s.GetDocument(ctx, "legacy-doc", ""); err != nil || out.Source != "draft" {
		t.Fatalf("%+v %v", out, err)
	}
	s.startSavedDocs(ctx)
	f, _ := s.GetFlat(ctx, "legacy-doc")
	if f.LiveVersion == 0 {
		t.Fatal("saved document did not start")
	}
	if out, err := s.GetDocument(ctx, "legacy-doc", ""); err != nil || out.Source != "live" {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestDocsRestoreTrialAndRestart(t *testing.T) {
	s, dir := newTestService(t)
	s.cfg.Runtime = &contentRuntime{}
	s.cfg.DocsApp = testDocsApp()
	ctx := context.Background()
	saveDocs(t, s, "restored-doc")
	v, err := s.st.GetVersion(ctx, "restored-doc", 1)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := s.st.GetFlat(ctx, "restored-doc")
	env, err := s.captureEnvironment(ctx, f.Slug, v)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.trialRun(ctx, f, v, filepath.Join(dir, "unused-snapshot"), env); err != nil {
		t.Fatal(err)
	}
	rt := s.cfg.Runtime.(*contentRuntime)
	last := rt.specs[len(rt.specs)-1]
	if !strings.Contains(last.Dir, filepath.Join("runtime", "docs")) || last.Entry != "server.js" || !strings.HasSuffix(last.DataDir, "restore-trial") {
		t.Fatalf("restore trial: %+v", last)
	}
	lf := s.state("restored-doc")
	old := lf.cur.Swap(nil)
	old.inst.Stop()
	if err := s.restore(ctx); err != nil {
		t.Fatal(err)
	}
	restored, err := s.GetDocument(ctx, "restored-doc", "")
	if err != nil || restored.Source != "live" {
		t.Fatalf("restore: %+v %v", restored, err)
	}
}

func TestDocsShutdownStopsRetiringInstances(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err := New(context.Background(), Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Runtime: &contentRuntime{}, DocsApp: testDocsApp(), Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = s.Close()
		}
	})
	ctx := context.Background()
	saveDocs(t, s, "shutdown-doc")
	if _, err := s.SaveVersion(ctx, "shutdown-doc", []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte("# second")}}, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	entries, err := os.ReadDir(filepath.Join(dir, "runtime", "docs"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("shutdown retained retiring runtime directories: %v %v", entries, err)
	}
}

func TestRuntimeStartsUseGenerationInsteadOfVersion(t *testing.T) {
	s, dir := newTestService(t)
	rt := &contentRuntime{}
	s.cfg.Runtime, s.cfg.DocsApp = rt, testDocsApp()
	v := saveDocsDraft(t, s, "generations")
	for _, version := range []int{0, 1, 3, 1, 0} {
		v.Number = version
		// Draft content remains at its immutable revision; only the log number varies.
		d, err := s.build(t.Context(), "generations", v, filepath.Join(dir, "data"), nil)
		if err != nil {
			t.Fatal(err)
		}
		d.inst.Stop()
	}
	var previous int64
	for _, spec := range rt.specs {
		if spec.Generation <= previous || spec.NextGeneration == nil {
			t.Fatal("unordered runtime start", spec)
		}
		previous = spec.Generation
	}
}

func TestDrainingWorkerCannotAcquireNewGeneration(t *testing.T) {
	s, dir := newTestService(t)
	rt := &contentRuntime{}
	s.cfg.Runtime, s.cfg.DocsApp = rt, testDocsApp()
	v := saveDocsDraft(t, s, "recovering")
	data := filepath.Join(dir, "live-data")
	old, err := s.build(t.Context(), "recovering", v, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer old.inst.Stop()
	previous := rt.specs[0]
	if _, err := previous.NextGeneration(t.Context()); err != nil {
		t.Fatal(err)
	}
	newer, err := s.build(t.Context(), "recovering", v, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer newer.inst.Stop()
	if _, err := previous.NextGeneration(t.Context()); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatal("obsolete instance could recover with newer source authority", err)
	}
	current := rt.specs[1]
	if generation, err := current.NextGeneration(t.Context()); err != nil || generation <= current.Generation {
		t.Fatal("current instance cannot recover", generation, err)
	}
	rt.fail = true
	if _, err := s.build(t.Context(), "recovering", v, data, nil); err == nil {
		t.Fatal("expected failure")
	}
	if _, err := current.NextGeneration(t.Context()); err != nil {
		t.Fatal("failed startup revoked current recovery", err)
	}
}

func TestStoppedCandidateRestoresLiveRecovery(t *testing.T) {
	s, dir := newTestService(t)
	rt := &contentRuntime{}
	s.cfg.Runtime, s.cfg.DocsApp = rt, testDocsApp()
	v := saveDocsDraft(t, s, "candidate")
	data := filepath.Join(dir, "live-data")
	live, err := s.build(t.Context(), "candidate", v, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer live.inst.Stop()
	candidate, err := s.build(t.Context(), "candidate", v, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate.inst.Stop()
	candidate.inst.Stop()
	if gen, err := rt.specs[0].NextGeneration(t.Context()); err != nil || gen <= rt.specs[1].Generation {
		t.Fatal("stopped candidate revoked live recovery", gen, err)
	}
}

func TestHostHealthSignalIsTrusted(t *testing.T) {
	for _, public := range []bool{false, true} {
		r := httptest.NewRequest("GET", "/_docs/healthz", nil)
		r.Header.Set("X-Flats-Health", "1")
		r.Header["x-flats-health"] = []string{"1"}
		r.Header.Set("X-Flats-Host-Op", "edit")
		r.Header["x-flats-host-op"] = []string{"edit"}
		clean := trustedAccess(r, public)
		for key := range clean.Header {
			if strings.EqualFold(key, "X-Flats-Health") || strings.EqualFold(key, "X-Flats-Host-Op") {
				t.Fatal("client host signal retained", key)
			}
		}
	}
	result := healthCheck(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Flats-Health") != "1" || r.Header.Get("X-Flats-Access") != "private" {
			t.Error("host health missing trusted headers")
		}
		w.WriteHeader(200)
	}), "/_docs/healthz", func(s string) string { return s })
	if result.Status != 200 || result.Error != "" {
		t.Fatal(result)
	}
}

func TestDocsEnvironmentSnapshotAndContentIsolation(t *testing.T) {
	s, dir := newTestService(t)
	rt := &contentRuntime{}
	s.cfg.Runtime, s.cfg.DocsApp = rt, testDocsApp()
	ctx := t.Context()
	v := saveDocsDraft(t, s, "configured-doc")
	for _, err := range []error{
		s.SetEnv(ctx, "configured-doc", "MODE", "ordinary-doc-setting", ViaAPI),
		s.SetNetworkPolicy(ctx, "configured-doc", []string{"https://api.example.com"}, ViaCLI),
		s.SetSecret(ctx, "configured-doc", "TOKEN", "secret-doc-setting", ViaCLI),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := s.captureEnvironment(ctx, "configured-doc", v)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnv(ctx, "configured-doc", "MODE", "next-doc-setting", ViaAPI); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "isolated")
	d, err := s.buildWithEnvironment(ctx, "configured-doc", v, data, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.inst.Stop()
	spec := rt.specs[0]
	if len(spec.NetworkOrigins) != 0 {
		t.Fatal("docs runtime received outbound grants")
	}
	if spec.Env["MODE"] != "ordinary-doc-setting" || spec.Env["TOKEN"] != "secret-doc-setting" || d.environment != snapshot {
		t.Fatal("docs did not use captured environment")
	}
	if got := d.redact("ordinary-doc-setting secret-doc-setting"); got != "ordinary-doc-setting [redacted]" {
		t.Fatalf("redaction: %s", got)
	}
	spec.Env["MODE"] = "worker mutation"
	if snapshot.values["MODE"] != "ordinary-doc-setting" {
		t.Fatal("worker mutated frozen environment")
	}
	b, err := os.ReadFile(filepath.Join(spec.Dir, "content.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"ordinary-doc-setting", "next-doc-setting", "secret-doc-setting"} {
		if strings.Contains(string(b), value) {
			t.Fatal("environment leaked into content module")
		}
	}
	rt.fail = true
	if _, err := s.buildWithEnvironment(ctx, "configured-doc", v, data, snapshot, nil); err == nil {
		t.Fatal("expected startup failure")
	}
	if err := s.restart(ctx, "configured-doc", d); err == nil {
		t.Fatal("expected restart failure")
	}
	if rt.specs[len(rt.specs)-1].Env["MODE"] != "ordinary-doc-setting" {
		t.Fatal("restart recaptured settings")
	}
}

// A document keeps no data snapshots: saving it again leaves none behind.
func TestDocumentSavesKeepNoSnapshots(t *testing.T) {
	s, _ := newTestService(t)
	s.cfg.Runtime = &contentRuntime{}
	s.cfg.DocsApp = testDocsApp()
	ctx := t.Context()
	saveDocs(t, s, "unsnapshotted")
	db := filepath.Join(s.dataDirOf("unsnapshotted"), dbName)
	if err := os.MkdirAll(filepath.Dir(db), 0o700); err != nil {
		t.Fatal(err)
	}
	conn, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	for _, text := range []string{"# second", "# third"} {
		if _, err := s.SaveVersion(ctx, "unsnapshotted", []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte(text)}}, SaveMeta{}, ViaAPI); err != nil {
			t.Fatal(err)
		}
	}
	if snaps, err := s.Snapshots("unsnapshotted"); err != nil || len(snaps) != 0 {
		t.Fatalf("document saves took snapshots: %v %v", snaps, err)
	}
}

// Running follows the active runtime, not the stored live version: a
// document whose runtime is gone (say, it failed to start after a restart)
// is not running.
func TestDocumentRunningFollowsRuntime(t *testing.T) {
	s, _ := newTestService(t)
	s.cfg.Runtime = &contentRuntime{}
	s.cfg.DocsApp = testDocsApp()
	saveDocs(t, s, "fragile")
	if f, _ := s.GetFlat(t.Context(), "fragile"); !f.Running {
		t.Fatal("saved document is not running")
	}
	if old := s.state("fragile").cur.Swap(nil); old != nil && old.inst != nil {
		old.inst.Stop()
	}
	if f, _ := s.GetFlat(t.Context(), "fragile"); f.Running || f.LiveVersion == 0 {
		t.Fatalf("document without a runtime: running=%t live=%d", f.Running, f.LiveVersion)
	}
}

// Document saves keep the running and previous internal bundles independently
// of the user-facing Version quota.
func TestDocumentSavesPruneOldBundles(t *testing.T) {
	s, _ := newTestService(t)
	s.cfg.Runtime = &contentRuntime{}
	s.cfg.DocsApp = testDocsApp()
	ctx := t.Context()
	if _, err := s.UpdateSettings(ctx, map[string]string{SetVersionQuota: "1"}); err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		if _, err := s.SaveVersion(ctx, "pruned-doc", []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte(fmt.Sprintf("# save %d", i))}}, SaveMeta{}, ViaAPI); err != nil {
			t.Fatal(err)
		}
	}
	f, _ := s.st.GetFlat(ctx, "pruned-doc")
	vs, err := s.st.ListVersions(ctx, "pruned-doc")
	if err != nil {
		t.Fatal(err)
	}
	var kept []int
	for _, v := range vs {
		_, statErr := os.Stat(s.versionDir("pruned-doc", v.Number))
		if !v.Pruned && statErr == nil {
			kept = append(kept, v.Number)
		} else if statErr == nil {
			t.Fatalf("pruned bundle %d is still on disk", v.Number)
		}
	}
	if len(vs) != 4 || len(kept) != 2 || kept[0] != f.LiveVersion || kept[1] != f.LiveVersion-1 {
		t.Fatalf("kept bundles %v of %d (live %d)", kept, len(vs), f.LiveVersion)
	}
	revisions, err := s.st.ListDraftRevisions(ctx, "pruned-doc")
	if err != nil {
		t.Fatal(err)
	}
	retainedDrafts := 0
	for _, r := range revisions {
		_, statErr := os.Stat(s.draftRevDir("pruned-doc", r.Revision))
		if !r.Pruned && statErr == nil {
			retainedDrafts++
		} else if statErr == nil {
			t.Fatalf("pruned draft snapshot %d remains on disk", r.Revision)
		}
	}
	if retainedDrafts != 1 {
		t.Fatalf("retained %d draft snapshots, want only the latest", retainedDrafts)
	}
	events, err := s.st.ListEvents(ctx, "pruned-doc", "retention", 0, 20)
	if err != nil || len(events) != 2 {
		t.Fatalf("backing cleanup events: %+v %v", events, err)
	}
	for _, e := range events {
		if !strings.Contains(e.Message, "internal backing bundle") || strings.Contains(e.Message, "version") {
			t.Fatalf("backing cleanup described as user Version deletion: %s", e.Message)
		}
	}
}

// A docs asset must not be executable in the browser's docs origin, including
// for bundles written before this protection was introduced.
func TestDocsAssetContentSecurity(t *testing.T) {
	dir := t.TempDir()
	body := "<script>fetch('/_docs/api/document')</script>"
	tests := []struct {
		name, contentType string
		attachment        bool
	}{
		{"picture.PNG", "image/png", false},
		{"picture.jpeg", "image/jpeg", false},
		{"clip.mp4", "video/mp4", false},
		{"tone.MP3", "audio/mpeg", false},
		{"font.woff2", "font/woff2", false},
		{"notes.txt", "text/plain; charset=utf-8", false},
		{"table.csv", "text/plain; charset=utf-8", false},
		{"active.html", "application/octet-stream", true},
		{"active.HTM", "application/octet-stream", true},
		{"active.xhtml", "application/octet-stream", true},
		{"diagram.svg", "image/svg+xml", false},
		{"figure.SVG", "image/svg+xml", false},
		{"active.svgz", "application/octet-stream", true},
		{"active.js", "application/octet-stream", true},
		{"active.mjs", "application/octet-stream", true},
		{"active.css", "application/octet-stream", true},
		{"active.xml", "application/octet-stream", true},
		{"active.pdf", "application/octet-stream", true},
		{"active.wasm", "application/octet-stream", true},
		{"active.unknown", "application/octet-stream", true},
	}
	assets := make([]string, 0, len(tests))
	for _, tt := range tests {
		if err := os.WriteFile(filepath.Join(dir, tt.name), []byte(body), 0o444); err != nil {
			t.Fatal(err)
		}
		assets = append(assets, tt.name)
	}
	h := docsAssets(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "worker")
	}), dir, assets)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRecorder()
			h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/"+tt.name, nil))
			if r.Code != http.StatusOK || r.Body.String() != body {
				t.Fatalf("asset response: %d %q", r.Code, r.Body.String())
			}
			if got := r.Header().Get("Content-Type"); got != tt.contentType {
				t.Errorf("content type = %q, want %q", got, tt.contentType)
			}
			wantDisposition := ""
			if tt.attachment {
				wantDisposition = "attachment"
			}
			if got := r.Header().Get("Content-Disposition"); got != wantDisposition {
				t.Errorf("content disposition = %q, want %q", got, wantDisposition)
			}
			wantCSP := "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'; sandbox"
			if strings.HasPrefix(tt.contentType, "audio/") || strings.HasPrefix(tt.contentType, "video/") {
				wantCSP = "default-src 'none'; media-src 'self'; frame-ancestors 'none'"
			}
			if got := r.Header().Get("Content-Security-Policy"); got != wantCSP {
				t.Errorf("CSP = %q, want %q", got, wantCSP)
			}
			if got := r.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q", got)
			}
		})
	}
	// Partial responses must also keep the download policy.
	req := httptest.NewRequest(http.MethodGet, "/active.html", nil)
	req.Header.Set("Range", "bytes=0-5")
	r := httptest.NewRecorder()
	h.ServeHTTP(r, req)
	if r.Code != http.StatusPartialContent || r.Header().Get("Content-Disposition") != "attachment" ||
		r.Header().Get("Content-Type") != "application/octet-stream" ||
		r.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("partial active asset response: %d %v", r.Code, r.Header())
	}
}
