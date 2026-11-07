package core

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/gosuda/flats/internal/bundle"
)

func docsImagePNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// A new version that cannot start refuses the image and leaves the running
// document as it was; a later paste of the same image then works.
func TestAddDocumentImageKeepsRunningDocumentWhenStartFails(t *testing.T) {
	t.Parallel()
	s, _ := newTestService(t)
	rt := &contentRuntime{}
	s.cfg.Runtime, s.cfg.DocsApp = rt, testDocsApp()
	saveDocs(t, s, "notes")
	ctx := t.Context()
	before, err := s.st.GetFlat(ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	data := docsImagePNG(t)
	rt.fail = true
	_, err = s.AddDocumentImage(ctx, "notes", "index.md", data, "Kim")
	var deploy *DeployError
	if !errors.As(err, &deploy) {
		t.Fatalf("got %v", err)
	}
	after, err := s.st.GetFlat(ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	if after.LiveVersion != before.LiveVersion || s.state("notes").cur.Load() == nil {
		t.Fatalf("live version %d→%d", before.LiveVersion, after.LiveVersion)
	}
	rt.fail = false
	image, err := s.AddDocumentImage(ctx, "notes", "index.md", data, "Kim")
	if err != nil || !image.Added {
		t.Fatal(image, err)
	}
	saved, err := os.ReadFile(filepath.Join(s.versionDir("notes", s.state("notes").cur.Load().version.Number), image.Path))
	if err != nil || !bytes.Equal(saved, data) {
		t.Fatalf("stored image: %v", err)
	}
}

func TestAddDocumentImageNeedsARunningDocument(t *testing.T) {
	t.Parallel()
	s, _ := newTestService(t)
	rt := &contentRuntime{}
	s.cfg.Runtime, s.cfg.DocsApp = rt, testDocsApp()
	ctx := t.Context()
	data := docsImagePNG(t)
	if _, err := s.AddDocumentImage(ctx, "absent", "index.md", data, "Kim"); err == nil {
		t.Fatal("added to a flat that does not exist")
	}
	if _, err := s.SaveVersion(ctx, "site", []bundle.File{{Path: "index.html", Data: []byte("<p>")}}, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddDocumentImage(ctx, "site", "index.md", data, "Kim"); !errors.Is(err, ErrNotDeployed) && !errors.Is(err, ErrNotDocs) {
		t.Fatalf("website: %v", err)
	}
	saveDocsDraft(t, s, "draft")
	if _, err := s.AddDocumentImage(ctx, "draft", "index.md", data, "Kim"); !errors.Is(err, ErrNotDeployed) {
		t.Fatalf("not running: %v", err)
	}
}

// A failure after the new version took over still added the image: it is
// stored, and the answer must say so rather than report a refusal.
func TestAddDocumentImageReportsACommittedFailureAsAdded(t *testing.T) {
	t.Parallel()
	s, _ := newTestService(t)
	s.cfg.DocsApp = testDocsApp()
	s.cfg.Runtime = lifecycleRuntime(func(spec RuntimeSpec) (Instance, error) {
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
		if !spec.ManagedDocs {
			return lifecycleInstance{h}, nil
		}
		m := &idleTestInstance{Handler: h}
		m.wake = func(context.Context, func(context.Context, http.Handler) error) (RuntimeLease, error) {
			return nil, errors.New("cannot initialize child")
		}
		return m, nil
	})
	ctx := t.Context()
	var deploy *DeployError
	_, err := s.SaveVersion(ctx, "notes", []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte("# notes")}}, SaveMeta{}, ViaAPI)
	if !errors.As(err, &deploy) || !deploy.Committed {
		t.Fatalf("setup: %v", err)
	}
	data := docsImagePNG(t)
	image, err := s.AddDocumentImage(ctx, "notes", "index.md", data, "Kim")
	if err != nil || !image.Added {
		t.Fatal(image, err)
	}
	f, err := s.st.GetFlat(ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.st.GetVersion(ctx, "notes", f.LiveVersion)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(s.contentDir("notes", v), image.Path))
	if err != nil || !bytes.Equal(stored, data) {
		t.Fatalf("stored image: %v", err)
	}
}
