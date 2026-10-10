package core

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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
	specs []RuntimeSpec
	fail  bool
}
type contentInstance struct{ http.Handler }

func (contentInstance) Stop() {}
func (rt *contentRuntime) Start(_ context.Context, spec RuntimeSpec) (Instance, error) {
	rt.specs = append(rt.specs, spec)
	if rt.fail {
		return nil, errors.New("start failed")
	}
	return contentInstance{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_docs/api/edit" {
			editResponse(w, r)
			return
		}
		if r.URL.Path == "/_docs/api/document" {
			_ = json.NewEncoder(w).Encode(Document{Format: 1, Doc: "index.md", Markdown: "live people's edits", Source: "live", Epoch: "epoch", Seq: 7, Conflicts: []DocumentConflict{{Generation: 4, Bytes: 27}}})
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

func TestDocsRuntimeLayoutReuseAndCleanup(t *testing.T) {
	s, dir := newTestService(t)
	rt := &contentRuntime{}
	s.cfg.Runtime = rt
	s.cfg.DocsApp = testDocsApp()
	v := saveDocs(t, s, "notes")
	d, err := s.build(context.Background(), "notes", v, filepath.Join(dir, "isolated"))
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
	d2, err := s.build(context.Background(), "notes", v, filepath.Join(dir, "isolated2"))
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
	if _, err := s.build(context.Background(), "notes", v, filepath.Join(dir, "fail")); err == nil {
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
	if _, err := approvedInternalDeploy(t, s, context.Background(), "access", 0); err != nil {
		t.Fatal(err)
	}
	p, err := s.OpenPreview(context.Background(), "access", 0)
	if err != nil {
		t.Fatal(err)
	}
	flat, _ := s.st.GetFlat(context.Background(), "access")
	flat.Visibility = store.Public
	_ = s.st.UpdateFlat(context.Background(), flat)
	net := s.cfg.Private.(*memNet)
	for _, tt := range []struct {
		name string
		h    http.Handler
		want string
	}{{"private", s.siteHandler("access", false), "private"}, {"public", s.siteHandler("access", true), "public"}, {"draft preview", net.hosts[p.Host], "private"}} {
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
	saveDocs(t, s, "website")
	fallback, err := s.GetDocument(ctx, "website", "")
	if err != nil || fallback.Source != "draft" {
		t.Fatalf("docs Draft above live website: %+v %v", fallback, err)
	}
	v := saveDocs(t, s, "notes")
	out, err := s.GetDocument(ctx, "notes", "")
	if err != nil || out.Source != "draft" || out.Markdown != "# main\n한글 😀" {
		t.Fatalf("%+v %v", out, err)
	}
	out, err = s.GetDocument(ctx, "notes", "a.md")
	if err != nil || out.Markdown != "# second" {
		t.Fatalf("%+v %v", out, err)
	}
	if _, err := s.GetDocument(ctx, "notes", "missing.md"); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatal(err)
	}
	if _, err := s.GetDocument(ctx, "notes", "../index.md"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := approvedInternalDeploy(t, s, ctx, "notes", 0); err != nil {
		t.Fatal(err)
	}
	out, err = s.GetDocument(ctx, "notes", "")
	if err != nil || out.Source != "live" || out.Markdown != "live people's edits" || out.Seq != 7 || len(out.Conflicts) != 1 || out.Conflicts[0].Bytes != 27 {
		t.Fatalf("%+v %v", out, err)
	}
	// Current Draft determines the flat's type, even when the live type differs.
	if _, err := s.SaveVersion(ctx, "notes", []bundle.File{{Path: "index.html", Data: []byte("website draft")}}, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	f, _ := s.GetFlat(ctx, "notes")
	if f.Type != "flat" {
		t.Fatalf("%+v", f)
	}
	out, err = s.GetDocument(ctx, "notes", "")
	if err != nil || out.Source != "live" {
		t.Fatalf("live docs precedence %+v %v", out, err)
	}
	if _, err := s.st.GetDraftRevision(ctx, "notes", v.Revision); err != nil {
		t.Fatal(err)
	}
}

func TestDocsRestoreTrialAndRestart(t *testing.T) {
	s, dir := newTestService(t)
	s.cfg.Runtime = &contentRuntime{}
	s.cfg.DocsApp = testDocsApp()
	ctx := context.Background()
	saveDocs(t, s, "restored-doc")
	if _, err := approvedInternalDeploy(t, s, ctx, "restored-doc", 0); err != nil {
		t.Fatal(err)
	}
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
	if _, err := approvedInternalDeploy(t, s, ctx, "shutdown-doc", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveVersion(ctx, "shutdown-doc", []bundle.File{{Path: "index.md", Data: []byte("# second")}}, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := approvedInternalDeploy(t, s, ctx, "shutdown-doc", 0); err != nil {
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
	v := saveDocs(t, s, "generations")
	for _, version := range []int{0, 1, 3, 1, 0} {
		v.Number = version
		// Draft content remains at its immutable revision; only the log number varies.
		d, err := s.build(t.Context(), "generations", v, filepath.Join(dir, "data"))
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
	v := saveDocs(t, s, "recovering")
	data := filepath.Join(dir, "live-data")
	old, err := s.build(t.Context(), "recovering", v, data)
	if err != nil {
		t.Fatal(err)
	}
	defer old.inst.Stop()
	previous := rt.specs[0]
	if _, err := previous.NextGeneration(t.Context()); err != nil {
		t.Fatal(err)
	}
	newer, err := s.build(t.Context(), "recovering", v, data)
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
	if _, err := s.build(t.Context(), "recovering", v, data); err == nil {
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
	v := saveDocs(t, s, "candidate")
	data := filepath.Join(dir, "live-data")
	live, err := s.build(t.Context(), "candidate", v, data)
	if err != nil {
		t.Fatal(err)
	}
	defer live.inst.Stop()
	candidate, err := s.build(t.Context(), "candidate", v, data)
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
	v := saveDocs(t, s, "configured-doc")
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
	d, err := s.buildWithEnvironment(ctx, "configured-doc", v, data, snapshot)
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
	if _, err := s.buildWithEnvironment(ctx, "configured-doc", v, data, snapshot); err == nil {
		t.Fatal("expected startup failure")
	}
	if err := s.restart(ctx, "configured-doc", d); err == nil {
		t.Fatal("expected restart failure")
	}
	if rt.specs[len(rt.specs)-1].Env["MODE"] != "ordinary-doc-setting" {
		t.Fatal("restart recaptured settings")
	}
}
