package store

import (
	"context"
	"testing"
	"time"
)

// Board import rows and element counts follow their revision through
// retention, rename and delete.
func TestBoardRevisionRows(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now().UTC()
	if err := s.CreateFlat(ctx, Flat{Slug: "plan", Name: "Plan", Visibility: Private, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	err := s.ContentTx(ctx, "plan", func(tx *ContentTx) error {
		if err := tx.InitReadyState(ctx, "board"); err != nil {
			return err
		}
		for n := 1; n <= 2; n++ {
			files := []RevisionFile{{Path: "assets/" + string(rune('a'+n)) + ".png", Hash: string(rune('a'+n)) + "h", Size: 1}}
			if err := tx.InsertRevision(ctx, Revision{Number: n, CreatedAt: now, Kind: "agent"}, files); err != nil {
				return err
			}
			if err := tx.PutBoardRevStats(ctx, n, ElementCounts{Added: n}); err != nil {
				return err
			}
			if err := tx.PutBoardImport(ctx, "store1", int64(10+n), "op", n); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, ok, err := s.BoardImportRevision(ctx, "plan", "store1", 12); err != nil || !ok || n != 2 {
		t.Fatalf("import revision = %d %v %v", n, ok, err)
	}
	if p, ok, err := s.RevisionAssetPath(ctx, "plan", "bh"); err != nil || !ok || p != "assets/b.png" {
		t.Fatalf("asset path = %q %v %v", p, ok, err)
	}
	if n, _ := s.CountRevisions(ctx, "plan"); n != 2 {
		t.Fatalf("count = %d", n)
	}
	if err := s.ContentTx(ctx, "plan", func(tx *ContentTx) error { return tx.DeleteRevision(ctx, 1) }); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.BoardImportRevision(ctx, "plan", "store1", 11); ok {
		t.Fatal("the import row of a deleted revision stayed")
	}
	if stats, _ := s.BoardRevStats(ctx, "plan", 1, 2); len(stats) != 1 || stats[2].Added != 2 {
		t.Fatalf("stats after retention = %v", stats)
	}
	if err := s.RenameFlat(ctx, "plan", "roadmap", time.Time{}, now); err != nil {
		t.Fatal(err)
	}
	if n, ok, _ := s.BoardImportRevision(ctx, "roadmap", "store1", 12); !ok || n != 2 {
		t.Fatal("the import row did not follow the rename")
	}
	if stats, _ := s.BoardRevStats(ctx, "roadmap", 1, 2); stats[2].Added != 2 {
		t.Fatalf("stats after rename = %v", stats)
	}
	if err := s.DeleteFlat(ctx, "roadmap"); err != nil {
		t.Fatal(err)
	}
	var rows int
	s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM board_imports) + (SELECT COUNT(*) FROM board_rev_stats)`).Scan(&rows)
	if rows != 0 {
		t.Fatalf("%d board rows outlived their flat", rows)
	}
}
