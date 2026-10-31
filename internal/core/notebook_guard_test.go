package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/notebook"
	"github.com/gosuda/flats/internal/store"
)

func TestNotebookContentPaths(t *testing.T) {
	page := "pages/p_" + strings.Repeat("a", 26)
	database := page + "/databases/d_" + strings.Repeat("a", 26)
	hash := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, path string
		valid      bool
	}{
		{"header", "notebook.json", true},
		{"page", page + "/page.json", true},
		{"blocks", page + "/blocks/0f.json", true},
		{"schema", database + "/schema.json", true},
		{"rows", database + "/rows/ff.json", true},
		{"asset", "assets/" + hash + ".png", true},
		{"traversal", page + "/../page.json", false},
		{"invalid id", "pages/p_bad/page.json", false},
		{"wrong kind", "pages/d_" + strings.Repeat("a", 26) + "/page.json", false},
		{"bad bucket", page + "/blocks/FF.json", false},
		{"unknown file", page + "/secret.json", false},
		{"asset mismatch", "assets/" + strings.Repeat("b", 64) + ".png", false},
		{"svg", "assets/" + hash + ".svg", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateNotebookFile(notebook.File{Path: tc.path, Hash: hash, Size: 1})
			if (err == nil) != tc.valid {
				t.Fatalf("content path %q: %v", tc.path, err)
			}
		})
	}
	for _, f := range []notebook.File{{Path: "notebook.json", Hash: "bad", Size: 1}, {Path: "notebook.json", Hash: hash, Size: -1}} {
		if err := validateNotebookFile(f); err == nil {
			t.Fatalf("invalid content file accepted: %+v", f)
		}
	}
}

func TestNotebookAdmissionAndPublicEditExclusion(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	for _, tc := range []struct{ name, raw string }{
		{"non object", `null`},
		{"unknown field", `{"op_id":"invalid-update","ops":[],"unknown":true}`},
		{"bad operation", `{"op_id":"invalid-update","ops":[{"op":"missing"}]}`},
		{"slug injection", `{"slug":"other","op_id":"invalid-update","ops":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.UpdateNotebook(ctx, "notes", []byte(tc.raw), ViaMCP); !errors.Is(err, ErrInvalid) || ErrorCategory(err) != "invalid" {
				t.Fatalf("invalid request: %v (%s)", err, ErrorCategory(err))
			}
			if _, err := s.st.GetFlat(ctx, "notes"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("invalid request created flat: %v", err)
			}
		})
	}
	if _, err := s.CreateNotebook(ctx, "notes", "Notes", ViaMCP); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetVisibilityAccess(ctx, "notes", store.Public, store.AccessEdit, ViaMCP, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("notebook admitted Public editing: %v", err)
	}
	if hasWorker("notebook", "server") || hasWorker("notebook", "static") {
		t.Fatal("notebook attributed to a worker")
	}
	metrics := s.notebookMetrics()
	if metrics.ImportCount != 1 || metrics.ImportSum < 0 || len(metrics.ImportCounts) != len(NotebookImportBuckets)+1 {
		t.Fatalf("notebook import metrics: %+v", metrics)
	}
	var count uint64
	for _, n := range metrics.ImportCounts {
		count += n
	}
	if count != metrics.ImportCount {
		t.Fatalf("histogram count=%d, imports=%d", count, metrics.ImportCount)
	}
	if metrics := s.boardMetrics(); metrics.ImportCount != 0 {
		t.Fatalf("notebook import changed board metrics: %+v", metrics)
	}
	unlock := s.lock("notes")
	directNotebookWrite(t, s.liveNotebook("notes"), "read-catchup", notebookCreatePage)
	s.notebookRetry.Store("notes", notebookBackoff{next: time.Now().Add(time.Hour), delay: time.Minute})
	unlock()
	// Transport is supplied separately; this asserts the host's foreground import.
	if _, err := s.GetNotebook(ctx, "notes", 0, []byte(`{}`)); err != nil && !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if _, exists := s.notebookRetry.Load("notes"); exists {
		t.Fatal("current read retained import backoff")
	}
	if got := storedRevisions(t, s, "notes"); len(got) != 2 || got[0] != 2 {
		t.Fatalf("current read did not import pending revision: %v", got)
	}
}
