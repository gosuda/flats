package store

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"
)

// allocated returns the bytes of the Go heap fn allocates.
func allocated(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// A bounded revision file list is refused after one file more than its
// bound, with little of the Go heap allocated where the whole list takes
// far more, and a path, a hash or a size its reader does not take is
// refused and not returned. A list at its bounds is returned whole. This
// measures the Go heap only; flats.db is trusted host state, and what
// SQLite reads of a row edited outside Flats is not bounded here.
func TestBoundedRevisionFilesRefuseBeforeTheListIsHeld(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/flats.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	if err := s.CreateFlat(ctx, Flat{Slug: "sheet", Name: "sheet", Visibility: Private, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	hash := fmt.Sprintf("%064x", 1)
	files := []RevisionFile{{Path: "sheet.json", Hash: hash, Size: 10}, {Path: "tabs/1/axes.json", Hash: hash, Size: 20}}
	err = s.ContentTx(ctx, "sheet", func(tx *ContentTx) error {
		if err := tx.InitReadyState(ctx, "sheet"); err != nil {
			return err
		}
		for _, n := range []int{1, 2, 3, 4, 5} {
			if err := tx.InsertRevision(ctx, Revision{Number: n, CreatedAt: now, Kind: "person"}, files); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	bounds := RevisionFileBounds{Files: 4711, PathBytes: 66, HashBytes: 64}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	many := func(n, count int) {
		exec(`WITH RECURSIVE i(k) AS (SELECT 1 UNION ALL SELECT k+1 FROM i WHERE k<?)
			INSERT INTO revision_files(flat,number,path,hash,size) SELECT 'sheet',?,'tabs/1/rows/0-'||k||'.json',?,4194304 FROM i`, count, n, hash)
	}
	many(1, 200_000)
	many(2, bounds.Files-len(files))
	const long = 8 << 20
	exec(`UPDATE revision_files SET path=printf('%.*c',?,'x') WHERE flat='sheet' AND number=3 AND path='sheet.json'`, long)
	exec(`UPDATE revision_files SET hash=printf('%.*c',?,'x') WHERE flat='sheet' AND number=4 AND path='sheet.json'`, long)
	exec(`UPDATE revision_files SET size=printf('%.*c',?,'x') WHERE flat='sheet' AND number=5 AND path='sheet.json'`, long)

	var whole uint64
	whole = allocated(func() {
		if got, err := s.RevisionFiles(ctx, "sheet", 1); err != nil || len(got) != 200_000+len(files) {
			t.Fatalf("the whole list has %d files: %v", len(got), err)
		}
	})
	for _, c := range []struct {
		name string
		n    int
	}{{"a file more", 1}, {"a long path", 3}, {"a long hash", 4}, {"a long size", 5}} {
		var got []RevisionFile
		var err error
		used := allocated(func() { got, err = s.BoundedRevisionFiles(ctx, "sheet", c.n, bounds) })
		if !errors.Is(err, ErrRevisionFiles) || got != nil {
			t.Errorf("%s: %d files: %v", c.name, len(got), err)
		}
		t.Logf("%s: refused after %d KiB, the whole list of 200,002 files takes %d KiB", c.name, used>>10, whole>>10)
		if used > 4<<20 {
			t.Errorf("%s: the refusal allocated %d KiB of the Go heap", c.name, used>>10)
		}
	}
	got, err := s.BoundedRevisionFiles(ctx, "sheet", 2, bounds)
	if err != nil || len(got) != bounds.Files {
		t.Fatalf("a list at its bounds gave %d files: %v", len(got), err)
	}
}
