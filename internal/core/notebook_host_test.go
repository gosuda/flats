package core

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/contenttype"
	"github.com/gosuda/flats/internal/store"
)

func TestNotebookHostVersionRefusals(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	if _, err := s.CreateFlat(ctx, "notes", "Notes", ViaMCP); err != nil {
		t.Fatal(err)
	}
	if err := s.st.ContentTx(ctx, "notes", func(tx *store.ContentTx) error {
		return tx.InitState(ctx, contenttype.Notebook)
	}); err != nil {
		t.Fatal(err)
	}

	// Initialization has committed the type before any shell version exists.
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"list versions", func() error { _, err := s.ListVersions(ctx, "notes"); return err }},
		{"get version", func() error { _, err := s.GetVersion(ctx, "notes", 1); return err }},
		{"version file", func() error { _, err := s.VersionFile("notes", 1, "index.html"); return err }},
		{"deployment history", func() error { _, err := s.Deployments(ctx, "notes"); return err }},
		{"list previews", func() error { _, err := s.ListPreviews(ctx, "notes"); return err }},
		{"save draft", func() error {
			_, err := s.SaveDraft(ctx, "notes", []bundle.File{{Path: bundle.ManifestName, Data: []byte(`{"type":"notebook"}`)}}, SaveMeta{}, 0, ViaMCP)
			return err
		}},
		{"request create version", func() error { _, err := s.RequestCreateVersion(ctx, "notes", 0, "", ViaMCP); return err }},
		{"request publish", func() error { _, err := s.RequestPublish(ctx, "notes", 0, "", ViaMCP); return err }},
		{"rollback with data", func() error { _, err := s.RollbackWithData(ctx, "notes", 1, true, ViaMCP); return err }},
		{"get draft", func() error { _, err := s.GetDraft(ctx, "notes"); return err }},
		{"preview", func() error { _, err := s.OpenPreview(ctx, "notes", 0); return err }},
		{"create version", func() error { _, err := s.CreateVersion(ctx, "notes", 0, "", ViaMCP); return err }},
		{"publish", func() error { _, err := s.Publish(ctx, "notes", 0, "", ViaMCP); return err }},
		{"deploy", func() error { _, err := s.Deploy(ctx, "notes", 1, ViaMCP); return err }},
		{"version rollback", func() error { _, err := s.Rollback(ctx, "notes", 1, ViaMCP); return err }},
		{"version quota", func() error { return s.SetVersionQuotaOverride(ctx, "notes", nil) }},
		{"delete version", func() error { _, err := s.DeleteVersion(ctx, "notes", 1, ViaMCP); return err }},
		{"request delete version", func() error {
			_, err := s.RequestDeleteVersion(ctx, "notes", 1, ViaMCP)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if !errors.Is(err, ErrNotebookUnversioned) || ErrorCategory(err) != "invalid" {
				t.Fatalf("refusal = %v (%s)", err, ErrorCategory(err))
			}
			if !strings.Contains(err.Error(), "update_notebook") || !strings.Contains(err.Error(), "revision") {
				t.Fatalf("refusal lacks notebook and history guidance: %v", err)
			}
		})
	}
	if _, err := s.SaveVersion(ctx, "fresh", []bundle.File{
		{Path: bundle.ManifestName, Data: []byte(`{"type":"notebook"}`)},
	}, SaveMeta{}, ViaMCP); !errors.Is(err, ErrNotebookUnversioned) {
		t.Fatalf("notebook manifest on bundle path: %v", err)
	}
	if _, err := s.st.GetFlat(ctx, "fresh"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("refused bundle created a flat: %v", err)
	}
	if _, err := s.SaveVersion(ctx, "notes", []bundle.File{
		{Path: "index.html", Data: []byte("website")},
	}, SaveMeta{}, ViaMCP); !errors.Is(err, ErrTypeChange) || !strings.Contains(err.Error(), "update_notebook") {
		t.Fatalf("website into notebook: %v", err)
	}
}

func TestNotebookHostPendingInitAndShell(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	if _, err := s.CreateFlat(ctx, "notes", "Notes", ViaMCP); err != nil {
		t.Fatal(err)
	}
	if err := s.st.ContentTx(ctx, "notes", func(tx *store.ContentTx) error {
		return tx.InitState(ctx, contenttype.Notebook)
	}); err != nil {
		t.Fatal(err)
	}
	s.startPendingDesigns(ctx)
	s.startPendingDesigns(ctx)
	st, err := s.st.ContentState(ctx, "notes")
	if err != nil || st.Phase != "ready" {
		t.Fatalf("completed initialization = %+v, %v", st, err)
	}
	in := s.liveNotebook("notes")
	if in == nil {
		t.Fatal("notebook did not start without a server runtime")
	}
	view, err := s.GetFlat(ctx, "notes")
	if err != nil || view.Type != contenttype.Notebook || view.Name != "Notes" {
		t.Fatalf("notebook view = %+v, %v", view, err)
	}
	if view.LiveVersion != 0 || view.Live != nil || view.Versions != 0 || view.VersionCount != 0 ||
		view.NextVersion != 0 || view.LatestVersion != 0 || view.LatestDeployed ||
		view.Draft != nil || view.Publication != "" || view.EffectiveVersionQuota != 0 || view.PublicAccess != "" {
		t.Fatalf("view exposes shell or guest-edit state: %+v", view)
	}
	f, err := s.st.GetFlat(ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.st.GetVersion(ctx, "notes", f.LiveVersion)
	if err != nil {
		t.Fatal(err)
	}
	if h, err := s.checkIsolated(ctx, f, v, nil); err != nil || !h.OK {
		t.Fatalf("isolated shell health = %+v, %v", h, err)
	}
	if s.liveNotebook("notes") != in {
		t.Fatal("isolated health check replaced the live notebook")
	}
	if _, err := s.GetVersion(ctx, "notes", f.LiveVersion); !errors.Is(err, ErrNotebookUnversioned) {
		t.Fatalf("low-level version read exposed internal shell: %v", err)
	}
	page, err := s.ListRevisions(ctx, "notes", 0, 100)
	if err != nil || len(page.Revisions) != 1 || page.Revisions[0].Kind != "created" {
		t.Fatalf("initial history = %+v, %v", page, err)
	}
}

func TestNotebookHostBlobStoreAndCategories(t *testing.T) {
	s := &Service{cfg: Config{DataDir: t.TempDir()}}
	if got, want := s.storeDir("notes", contenttype.Notebook), filepath.Join(s.flatDir("notes"), "revisions"); got != want {
		t.Fatalf("notebook blob root = %s, want %s", got, want)
	}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"wrong type", ErrNotNotebook, "not_notebook"},
		{"capacity", ErrNotebookCapacity, "notebook_capacity"},
		{"version model", ErrNotebookUnversioned, "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ErrorCategory(fmt.Errorf("wrapped: %w", tc.err)); got != tc.want {
				t.Fatalf("category = %s, want %s", got, tc.want)
			}
		})
	}
}
