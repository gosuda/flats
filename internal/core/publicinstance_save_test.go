package core

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/store"
)

func piSaveFiles(body string) []bundle.File {
	return []bundle.File{{Path: "index.html", Data: []byte(body)}}
}

func TestPublicInstancePublishedSaves(t *testing.T) {
	for _, page := range []bool{false, true} {
		t.Run(fmt.Sprintf("page=%v", page), func(t *testing.T) {
			s, network := piCreationService(t, true)
			ctx := t.Context()
			if page {
				f, err := s.CreatePage(ctx, "saved", "Page", ViaMCP)
				if err != nil || f.LiveVersion != 0 || !f.CreatedPublicInstance {
					t.Fatalf("starter published: %+v %v", f, err)
				}
			}
			for i := 1; i <= 2; i++ {
				body := fmt.Sprintf("content %d", i)
				files := piSaveFiles(body)
				if page {
					files = append(files, bundle.File{Path: bundle.ManifestName, Data: []byte(`{"type":"page"}`)})
				}
				v, err := s.SaveVersion(ctx, "saved", files, SaveMeta{}, ViaAPI)
				if err != nil || !v.Published || v.Number != i {
					t.Fatalf("save %d: %+v %v", i, v, err)
				}
				f, err := s.GetFlat(ctx, "saved")
				if err != nil || f.LiveVersion != i || f.PublicURL == "" || f.ConnectionState != "ready" || !f.CreatedPublicInstance {
					t.Fatalf("flat: %+v %v", f, err)
				}
				d, err := s.GetDraft(ctx, "saved")
				if err != nil || d.Dirty || d.BaseVersion != i || d.Revision != v.Revision {
					t.Fatalf("draft: %+v %v", d, err)
				}
				network.mu.Lock()
				h := network.routes[lifecycleRouteKey{host: "saved", provider: ProviderPortal}]
				network.mu.Unlock()
				if h == nil {
					t.Fatal("public route missing")
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest("GET", "https://saved.example/", nil))
				if rec.Code != 200 || rec.Body.String() != body {
					t.Fatalf("public content: %d %q", rec.Code, rec.Body.String())
				}
			}
			ds, err := s.st.ListDeployments(ctx, "saved", 10)
			if err != nil || len(ds) != 2 {
				t.Fatalf("deployments: %+v %v", ds, err)
			}
			for _, d := range ds {
				if d.Kind != "public-instance" || d.ApprovalID != "" {
					t.Fatalf("deployment source: %+v", d)
				}
			}
			events, err := s.st.ListEvents(ctx, "saved", "deploy", 0, 100)
			if err != nil || len(events) != 2 || !strings.Contains(events[0].Message, "via public instance mode") {
				t.Fatalf("events: %+v %v", events, err)
			}
			as, err := s.ListApprovals(ctx, "")
			if err != nil || len(as) != 0 {
				t.Fatalf("approvals: %+v %v", as, err)
			}
		})
	}
}

func TestPublicInstanceSaveFailuresKeepDraft(t *testing.T) {
	for _, failure := range []string{"quota", "disk", "health", "start", "snapshot"} {
		t.Run(failure, func(t *testing.T) {
			s, _ := piCreationService(t, true)
			ctx := t.Context()
			server := failure == "health" || failure == "start" || failure == "snapshot"
			s.cfg.Runtime = lifecycleRuntime(func(spec RuntimeSpec) (Instance, error) {
				if err := os.MkdirAll(spec.DataDir, 0700); err != nil {
					return nil, err
				}
				body, err := os.ReadFile(filepath.Join(spec.Dir, "server.js"))
				if err != nil {
					return nil, err
				}
				if spec.DataDir == s.dataDirOf("saved") && string(body) == "bad" && failure == "start" {
					return nil, errors.New("startup failed")
				}
				return lifecycleInstance{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if string(body) == "bad" && failure == "health" {
						w.WriteHeader(503)
					}
					w.Write(body)
				})}, nil
			})
			files := func(body string) []bundle.File {
				if !server {
					return piSaveFiles(body)
				}
				return []bundle.File{{Path: bundle.ManifestName, Data: []byte(`{"kind":"server","entry":"server.js"}`)}, {Path: "server.js", Data: []byte(body)}}
			}
			if _, err := s.SaveVersion(ctx, "saved", files("one"), SaveMeta{}, ViaMCP); err != nil {
				t.Fatal(err)
			}
			var want error
			switch failure {
			case "quota":
				q := 1
				if err := s.SetVersionQuotaOverride(ctx, "saved", &q); err != nil {
					t.Fatal(err)
				}
				want = ErrVersionQuota
			case "disk":
				size := int64(0)
				for _, f := range files(strings.Repeat("bad", 100)) {
					size += int64(len(f.Data))
				}
				quota := dirSize(s.flatDir("saved")) + size
				if _, err := s.UpdateSettings(ctx, map[string]string{SetDiskQuotaBytes: fmt.Sprint(quota)}); err != nil {
					t.Fatal(err)
				}
				want = ErrDiskQuota
			case "snapshot":
				if err := os.RemoveAll(s.snapshotDir("saved")); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(s.snapshotDir("saved"), []byte("fault"), 0600); err != nil {
					t.Fatal(err)
				}
				want = ErrSnapshotFailed
			}
			body := "bad"
			if failure == "disk" {
				body = strings.Repeat(body, 100)
			}
			v, err := s.SaveVersion(ctx, "saved", files(body), SaveMeta{}, ViaMCP)
			var kept *SavedDraftError
			if !errors.As(err, &kept) || kept.Revision != 2 || v.Revision != 2 || !strings.Contains(err.Error(), "Draft was kept") {
				t.Fatalf("save error: %+v %v", v, err)
			}
			if want != nil && !errors.Is(err, want) {
				t.Fatalf("cause: %v want %v", err, want)
			}
			if failure == "health" || failure == "start" || failure == "snapshot" {
				var de *DeployError
				if !errors.As(err, &de) {
					t.Fatalf("not DeployError: %v", err)
				}
				if failure == "health" && (de.Health.Status != 503 || ErrorCategory(err) != "health_check_failed") {
					t.Fatal(err)
				}
				if failure == "start" && (!strings.Contains(err.Error(), "may have changed") || ErrorCategory(err) != "runtime_start_failed") {
					t.Fatal(err)
				}
				if failure == "snapshot" && ErrorCategory(err) != "snapshot_failed" {
					t.Fatal(err)
				}
			}
			f, e := s.GetFlat(ctx, "saved")
			if e != nil || f.LiveVersion != 1 {
				t.Fatalf("live changed: %+v %v", f, e)
			}
			vs, e := s.ListVersions(ctx, "saved")
			if e != nil || len(vs) != 1 || vs[0].Number != 1 {
				t.Fatalf("versions: %+v %v", vs, e)
			}
			d, e := s.GetDraft(ctx, "saved")
			if e != nil || d.Revision != 2 || !d.Dirty {
				t.Fatalf("draft: %+v %v", d, e)
			}
			if _, e := os.Stat(s.versionDir("saved", 2)); !errors.Is(e, os.ErrNotExist) {
				t.Fatalf("failed version directory: %v", e)
			}
			// The ordinary approved flow remains available after an automatic failure.
			if failure == "quota" {
				q := 0
				if e := s.SetVersionQuotaOverride(ctx, "saved", &q); e != nil {
					t.Fatal(e)
				}
			}
			if failure == "disk" {
				if _, e := s.UpdateSettings(ctx, map[string]string{SetDiskQuotaBytes: "0"}); e != nil {
					t.Fatal(e)
				}
			}
			ar, e := s.CreateVersion(ctx, "saved", 0, "", ViaMCP)
			if e != nil || ar.Approval == nil {
				t.Fatalf("approval unavailable: %+v %v", ar, e)
			}
		})
	}
}

func TestPublicInstanceSaveConsentAndRevision(t *testing.T) {
	s, _ := piCreationService(t, false)
	ctx := t.Context()
	for _, slug := range []string{"old-private", "old-public"} {
		if _, e := s.SaveVersion(ctx, slug, piSaveFiles("old"), SaveMeta{}, ViaAPI); e != nil {
			t.Fatal(e)
		}
	}
	f, e := s.st.GetFlat(ctx, "old-public")
	if e != nil {
		t.Fatal(e)
	}
	f.Visibility = store.Public
	if e := s.st.UpdateFlat(ctx, f); e != nil {
		t.Fatal(e)
	}
	if _, e := s.SetPublicInstance(ctx, true, "portal", "test"); e != nil {
		t.Fatal(e)
	}
	for _, slug := range []string{"old-private", "old-public"} {
		v, e := s.SaveVersion(ctx, slug, piSaveFiles("changed"), SaveMeta{}, ViaAPI)
		if e != nil || v.Published {
			t.Fatalf("legacy flat published: %+v %v", v, e)
		}
	}
	if _, e := s.SaveVersion(ctx, "new", piSaveFiles("one"), SaveMeta{}, ViaAPI); e != nil {
		t.Fatal(e)
	}
	if _, e := s.SaveDraft(ctx, "new", piSaveFiles("stale"), SaveMeta{}, 0, ViaAPI); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	d, e := s.GetDraft(ctx, "new")
	if e != nil || d.Revision != 1 {
		t.Fatalf("stale save wrote: %+v %v", d, e)
	}
	if _, e := s.SetPublicInstance(ctx, false, "", "test"); e != nil {
		t.Fatal(e)
	}
	v, e := s.SaveVersion(ctx, "new", piSaveFiles("off"), SaveMeta{}, ViaAPI)
	if e != nil || v.Published {
		t.Fatalf("off published: %+v %v", v, e)
	}
	if _, e := s.SetPublicInstance(ctx, true, "portal", "test"); e != nil {
		t.Fatal(e)
	}
	ar, e := s.SetVisibility(ctx, "new", store.Private, ViaMCP, "")
	if e != nil {
		t.Fatal(e)
	}
	if _, e := s.Decide(ctx, ar.Approval.ID, true); e != nil {
		t.Fatal(e)
	}
	v, e = s.SaveVersion(ctx, "new", piSaveFiles("private"), SaveMeta{}, ViaAPI)
	if e != nil || v.Published {
		t.Fatalf("Private published: %+v %v", v, e)
	}
	ar, e = s.SetVisibility(ctx, "new", store.Public, ViaMCP, "")
	if e != nil {
		t.Fatal(e)
	}
	if _, e := s.Decide(ctx, ar.Approval.ID, true); e != nil {
		t.Fatal(e)
	}
	v, e = s.SaveVersion(ctx, "new", piSaveFiles("again"), SaveMeta{}, ViaAPI)
	if e != nil || !v.Published || v.Number != 2 {
		t.Fatalf("renewed consent: %+v %v", v, e)
	}
}

func TestPublicInstanceSaveKeepsDecisionAcrossTurnOff(t *testing.T) {
	s, _ := piCreationService(t, true)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.cfg.Runtime = lifecycleRuntime(func(spec RuntimeSpec) (Instance, error) {
		if spec.DataDir != s.dataDirOf("ordered") {
			once.Do(func() { close(entered); <-release })
		}
		return lifecycleInstance{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })}, nil
	})
	files := []bundle.File{{Path: bundle.ManifestName, Data: []byte(`{"kind":"server","entry":"server.js"}`)}, {Path: "server.js", Data: []byte("ok")}}
	done := make(chan error, 1)
	go func() { _, err := s.SaveVersion(t.Context(), "ordered", files, SaveMeta{}, ViaMCP); done <- err }()
	<-entered
	if _, err := s.SetPublicInstance(t.Context(), false, "", "test"); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	v, err := s.SaveVersion(t.Context(), "ordered", files, SaveMeta{}, ViaMCP)
	if err != nil || v.Published {
		t.Fatalf("save after Turn off published: %+v %v", v, err)
	}
	f, err := s.GetFlat(t.Context(), "ordered")
	if err != nil || f.LiveVersion != 1 || f.Draft.Revision != 2 {
		t.Fatalf("ordered: %+v %v", f, err)
	}
}

func TestPublicInstanceConcurrentSaves(t *testing.T) {
	s, _ := piCreationService(t, true)
	type receipt struct {
		v   store.Version
		err error
	}
	results := make(chan receipt, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, body := range []string{"one", "two"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			v, e := s.SaveVersion(t.Context(), "race", piSaveFiles(body), SaveMeta{}, ViaMCP)
			results <- receipt{v, e}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	seen := map[int]bool{}
	var last store.Version
	for r := range results {
		if r.err != nil {
			t.Fatal(r.err)
		}
		seen[r.v.Number] = true
		if r.v.Number == 2 {
			last = r.v
		}
	}
	if !seen[1] || !seen[2] || last.Revision != 2 {
		t.Fatalf("receipts: %+v %+v", seen, last)
	}
	f, e := s.GetFlat(t.Context(), "race")
	if e != nil || f.LiveVersion != 2 || f.Draft.Hash != last.Hash {
		t.Fatalf("live: %+v %v", f, e)
	}
	live, e := s.GetVersion(t.Context(), "race", 2)
	if e != nil || live.Hash != last.Hash {
		t.Fatalf("wrong exact revision: %+v %v", live, e)
	}
}
