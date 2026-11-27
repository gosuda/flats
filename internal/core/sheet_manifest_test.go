package core

import (
	"bytes"
	"context"
	"errors"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet"
	"github.com/gosuda/flats/internal/store"
)

// latestManifest returns the file list of the latest revision of a sheet
// and the path of its manifest's blob.
func latestManifest(t *testing.T, s *Service, slugName string) ([]store.RevisionFile, string) {
	t.Helper()
	revs, err := s.st.ListRevisions(context.Background(), slugName, 0, 1)
	if err != nil || len(revs) == 0 {
		t.Fatalf("revisions %v: %v", revs, err)
	}
	files, err := s.sheetRevisionList(context.Background(), slugName, revs[0].Number)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(files, func(f store.RevisionFile) bool { return f.Path == sheetManifest })
	p, ok := blobPath(s.docsStoreDir(slugName), files[i].Hash)
	if !ok {
		t.Fatal("no blob path")
	}
	return files, p
}

// An import reads the manifest of the latest revision as the sheet reads
// a revision file. A manifest blob that a damage made far longer than its
// entry is refused after one byte more, with little allocated, and one of
// other bytes fails its hash; the import then records nothing, the sheet
// keeps its outbox, and another sheet records its revisions meanwhile. A
// cancelled request ends the read. Once the blob is whole again the next
// import records the pending revision.
func TestSheetImportReadsTheManifestWithinItsBounds(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	in := newSheet(t, s, "budget")
	newSheet(t, s, "other")
	writeSheet(t, s, "budget", "one", sheetBudgetRows)
	files, manifest := latestManifest(t, s, "budget")
	original, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	before := storedRevisions(t, s, "budget")
	others := len(storedRevisions(t, s, "other"))
	importNow := func(ctx context.Context) error {
		t.Helper()
		unlock, err := s.lockStorageCtx(context.Background(), "budget")
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		_, err = s.importSheetLocked(ctx, "budget", in)
		return err
	}
	for i, damage := range [][]byte{
		append(slices.Clone(original), bytes.Repeat([]byte{' '}, 64<<20)...),
		bytes.ToUpper(original),
	} {
		if err := os.WriteFile(manifest, damage, 0o600); err != nil {
			t.Fatal(err)
		}
		names := []string{"Sheet1", "Plan", "Draft"}
		writeSheet(t, s, "budget", "rename"+names[i+1], `{"op":"rename_tab","tab":"`+names[i]+`","name":"`+names[i+1]+`"}`)
		var used uint64
		var mem runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&mem)
		used = mem.TotalAlloc
		err := importNow(ctx)
		runtime.ReadMemStats(&mem)
		used = mem.TotalAlloc - used
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("damage %d: the import answered %v", i, err)
		}
		if used > 8<<20 {
			t.Errorf("damage %d: the refused import allocated %d KiB", i, used>>10)
		}
		t.Logf("damage %d: refused after %d KiB: %v", i, used>>10, err)
		if got := storedRevisions(t, s, "budget"); !slices.Equal(got, before) {
			t.Fatalf("damage %d: the import recorded revisions %v, before %v", i, got, before)
		}
		writeSheet(t, s, "other", "other"+string(rune('a'+i)), sheetBudgetRows)
		if got := len(storedRevisions(t, s, "other")); got != others+i+1 {
			t.Fatalf("damage %d: the other sheet has %d revisions, want %d", i, got, others+i+1)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.sheetTabStates(cancelled, "budget", files); !errors.Is(err, context.Canceled) {
		t.Fatalf("a read of the manifest after its request ended answered %v", err)
	}
	if err := importNow(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("an import after its request ended answered %v", err)
	}
	if err := os.WriteFile(manifest, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := importNow(ctx); err != nil {
		t.Fatal(err)
	}
	if got := storedRevisions(t, s, "budget"); len(got) != len(before)+2 {
		t.Fatalf("after the repair the sheet has revisions %v, before %v", got, before)
	}
}

// The file list of a sheet revision is read within what a revision holds:
// a list of one file more than that is refused as a damaged revision, and
// a list of the most is read whole.
func TestSheetRevisionListIsBounded(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	newSheet(t, s, "budget")
	listed := func(n int) []store.RevisionFile {
		files := []store.RevisionFile{{Path: sheetManifest, Hash: strings.Repeat("a", 64), Size: 1}}
		for i := 1; len(files) < n; i++ {
			files = append(files, store.RevisionFile{Path: "tabs/1/rows/0-" + strconv.Itoa(i) + ".json", Hash: strings.Repeat("a", 64), Size: 1})
		}
		return files
	}
	err := s.st.ContentTx(ctx, "budget", func(tx *store.ContentTx) error {
		if err := tx.InsertRevision(ctx, store.Revision{Number: 90, CreatedAt: s.now(), Kind: "agent"}, listed(sheet.MaxRevisionFiles)); err != nil {
			return err
		}
		return tx.InsertRevision(ctx, store.Revision{Number: 91, CreatedAt: s.now(), Kind: "agent"}, listed(sheet.MaxRevisionFiles+1))
	})
	if err != nil {
		t.Fatal(err)
	}
	if files, err := s.sheetRevisionList(ctx, "budget", 90); err != nil || len(files) != sheet.MaxRevisionFiles {
		t.Fatalf("the list of the most files gave %d: %v", len(files), err)
	}
	var problem *sheet.WriteError
	if files, err := s.sheetRevisionList(ctx, "budget", 91); !errors.As(err, &problem) || problem.Code != "unavailable" || problem.Reason != "revision" || files != nil {
		t.Fatalf("a list of a file more gave %d files: %v", len(files), err)
	}
}
