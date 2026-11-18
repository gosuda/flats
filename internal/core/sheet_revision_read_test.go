package core

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet"
	"github.com/gosuda/flats/internal/store"
)

// The generic revision reads of a sheet go through the sheet's readers: a
// file comes back as the blob holds it, a manifest of other bytes is a
// damaged revision, and a request that ended before the read answers with
// its own error, not with a damaged revision.
func TestGenericSheetRevisionReadsUseTheSheetReaders(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	newSheet(t, s, "budget")
	writeSheet(t, s, "budget", "one", sheetBudgetRows)
	files, manifest := latestManifest(t, s, "budget")
	revs, err := s.st.ListRevisions(ctx, "budget", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	n := revs[0].Number
	_, listed, err := s.GetRevision(ctx, "budget", n)
	if err != nil || len(listed) != len(files) {
		t.Fatalf("the list holds %d files, the sheet's %d: %v", len(listed), len(files), err)
	}
	if !slices.IsSortedFunc(listed, func(a, b store.RevisionFile) int { return strings.Compare(a.Path, b.Path) }) {
		t.Fatalf("the list is not in path order: %+v", listed)
	}
	original, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	data, f, err := s.RevisionFileContent(ctx, "budget", n, sheetManifest)
	if err != nil || string(data) != string(original) || f.Path != sheetManifest {
		t.Fatalf("the manifest read %q: %v", data, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := s.RevisionFileContent(cancelled, "budget", n, sheetManifest); !errors.Is(err, context.Canceled) {
		t.Fatalf("a read after its request ended answered %v", err)
	}
	if err := os.Chmod(manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	damaged := slices.Clone(original)
	damaged[0] ^= 1
	if err := os.WriteFile(manifest, damaged, 0o600); err != nil {
		t.Fatal(err)
	}
	var problem *sheet.WriteError
	if _, _, err := s.RevisionFileContent(ctx, "budget", n, sheetManifest); !errors.As(err, &problem) || problem.Reason != "revision" {
		t.Fatalf("a manifest of other bytes answered %v", err)
	}
	if _, _, err := s.RevisionFileContent(ctx, "budget", n, "tabs/1/other.json"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a path the revision lacks answered %v", err)
	}
}
