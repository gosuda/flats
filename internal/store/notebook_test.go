package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func notebookRevisionFixture(t *testing.T, s *Store, flat string) {
	t.Helper()
	ctx := context.Background()
	now := fixtureNow
	if err := s.CreateFlat(ctx, Flat{Slug: flat, Name: flat, Visibility: Private, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.ContentTx(ctx, flat, func(tx *ContentTx) error {
		if err := tx.InitReadyState(ctx, "notebook"); err != nil {
			return err
		}
		for n := 1; n <= 2; n++ {
			if err := tx.InsertRevision(ctx, Revision{Number: n, Kind: "agent", CreatedAt: now}, nil); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNotebookRevisionRows(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	pages := []NotebookPageChange{
		{Page: "p_z", Change: "added", TitleAfter: ""},
		{Page: "p_a", Change: "changed", TitleBefore: "before", TitleAfter: "한글 📝 e\u0301"},
		{Page: "p_m", Change: "removed", TitleBefore: "removed"},
	}
	for _, flat := range []string{"notes", "other"} {
		notebookRevisionFixture(t, s, flat)
		if err := s.ContentTx(ctx, flat, func(tx *ContentTx) error {
			for n := 1; n <= 2; n++ {
				if err := tx.PutNotebookImport(ctx, "store1", int64(n), "op", n); err != nil {
					return err
				}
				if err := tx.PutNotebookRevPages(ctx, n, pages); err != nil {
					return err
				}
			}
			return tx.PutNotebookImport(ctx, "store2", 1, "another-op", 2)
		}); err != nil {
			t.Fatal(err)
		}
	}
	want := []NotebookPageChange{pages[1], pages[2], pages[0]}
	if got, err := s.NotebookRevPages(ctx, "notes", 1, 1); err != nil || !reflect.DeepEqual(got, map[int][]NotebookPageChange{1: want}) {
		t.Fatalf("page summaries = %+v, %v", got, err)
	}
	if got, err := s.NotebookRevPages(ctx, "notes", 3, 4); err != nil || len(got) != 0 {
		t.Fatalf("absent summaries = %+v, %v", got, err)
	}
	if n, ok, err := s.NotebookImportRevision(ctx, "notes", "store2", 1); err != nil || !ok || n != 2 {
		t.Fatalf("other store import = %d %t %v", n, ok, err)
	}
	if n, ok, err := s.NotebookImportRevision(ctx, "notes", "missing", 1); err != nil || ok || n != 0 {
		t.Fatalf("missing import = %d %t %v", n, ok, err)
	}
	if err := s.ContentTx(ctx, "notes", func(tx *ContentTx) error { return tx.DeleteRevision(ctx, 1) }); err != nil {
		t.Fatal(err)
	}
	if n, ok, err := s.NotebookImportRevision(ctx, "notes", "store1", 1); err != nil || ok || n != 0 {
		t.Fatalf("pruned import = %d %t %v", n, ok, err)
	}
	if got, err := s.NotebookRevPages(ctx, "notes", 1, 2); err != nil || !reflect.DeepEqual(got, map[int][]NotebookPageChange{2: want}) {
		t.Fatalf("summaries after retention = %+v, %v", got, err)
	}
	if err := s.RenameFlat(ctx, "notes", "renamed", time.Time{}, fixtureNow); err != nil {
		t.Fatal(err)
	}
	if n, ok, err := s.NotebookImportRevision(ctx, "renamed", "store1", 2); err != nil || !ok || n != 2 {
		t.Fatalf("renamed import = %d %t %v", n, ok, err)
	}
	if got, err := s.NotebookRevPages(ctx, "renamed", 1, 2); err != nil || !reflect.DeepEqual(got, map[int][]NotebookPageChange{2: want}) {
		t.Fatalf("renamed summaries = %+v, %v", got, err)
	}
	if n, ok, err := s.NotebookImportRevision(ctx, "notes", "store1", 2); err != nil || ok || n != 0 {
		t.Fatalf("old slug import = %d %t %v", n, ok, err)
	}
	if got, err := s.NotebookRevPages(ctx, "notes", 1, 2); err != nil || len(got) != 0 {
		t.Fatalf("old slug summaries = %+v, %v", got, err)
	}
	if err := s.DeleteFlat(ctx, "renamed"); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM notebook_imports WHERE flat='renamed') + (SELECT COUNT(*) FROM notebook_rev_pages WHERE flat='renamed')`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("rows after flat delete = %d, %v", remaining, err)
	}
	if n, ok, err := s.NotebookImportRevision(ctx, "other", "store1", 1); err != nil || !ok || n != 1 {
		t.Fatalf("other flat import = %d %t %v", n, ok, err)
	}
	if got, err := s.NotebookRevPages(ctx, "other", 1, 2); err != nil || !reflect.DeepEqual(got, map[int][]NotebookPageChange{1: want, 2: want}) {
		t.Fatalf("other flat summaries = %+v, %v", got, err)
	}
}

func TestNotebookMetadataWrites(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	notebookRevisionFixture(t, s, "notes")
	initial := []NotebookPageChange{{Page: "p_a", Change: "added", TitleAfter: "title"}}
	if err := s.PutNotebookRevPages(ctx, "notes", 1, initial); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		n     int
		pages []NotebookPageChange
	}{
		{"invalid change", 1, []NotebookPageChange{{Page: "p_b", Change: "added"}, {Page: "p_c", Change: "invalid"}}},
		{"duplicate page", 1, []NotebookPageChange{initial[0], initial[0]}},
		{"missing revision", 3, initial},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.PutNotebookRevPages(ctx, "notes", tc.n, tc.pages); err == nil {
				t.Fatal("invalid summaries accepted")
			}
			if got, err := s.NotebookRevPages(ctx, "notes", 1, 3); err != nil || !reflect.DeepEqual(got, map[int][]NotebookPageChange{1: initial}) {
				t.Fatalf("failed replacement changed summaries = %+v, %v", got, err)
			}
		})
	}
	for _, n := range []int{1, 1, 2} {
		if err := s.ContentTx(ctx, "notes", func(tx *ContentTx) error { return tx.PutNotebookImport(ctx, "store1", 5, "op", n) }); err != nil {
			t.Fatal(err)
		}
	}
	if n, ok, err := s.NotebookImportRevision(ctx, "notes", "store1", 5); err != nil || !ok || n != 2 {
		t.Fatalf("replaced import = %d %t %v", n, ok, err)
	}
	var op string
	if err := s.db.QueryRowContext(ctx, `SELECT op_id FROM notebook_imports WHERE flat='notes' AND store='store1' AND outbox_seq=5`).Scan(&op); err != nil || op != "op" {
		t.Fatalf("stored op id = %q, %v", op, err)
	}
	if err := s.ContentTx(ctx, "notes", func(tx *ContentTx) error { return tx.PutNotebookImport(ctx, "store1", 5, "bad", 3) }); err == nil {
		t.Fatal("import without revision accepted")
	}
	boom := errors.New("rollback import")
	if err := s.ContentTx(ctx, "notes", func(tx *ContentTx) error {
		if err := tx.InsertRevision(ctx, Revision{Number: 3, CreatedAt: fixtureNow, Kind: "agent"}, nil); err != nil {
			return err
		}
		if err := tx.PutNotebookImport(ctx, "store1", 6, "rollback-op", 3); err != nil {
			return err
		}
		if err := tx.PutNotebookRevPages(ctx, 3, initial); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("transaction error = %v", err)
	}
	if _, err := s.Revision(ctx, "notes", 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rolled-back revision = %v", err)
	}
	if n, ok, err := s.NotebookImportRevision(ctx, "notes", "store1", 6); err != nil || ok || n != 0 {
		t.Fatalf("rolled-back import = %d %t %v", n, ok, err)
	}
	if got, err := s.NotebookRevPages(ctx, "notes", 1, 3); err != nil || !reflect.DeepEqual(got, map[int][]NotebookPageChange{1: initial}) {
		t.Fatalf("rolled-back summaries = %+v, %v", got, err)
	}
	replacement := []NotebookPageChange{{Page: "p_b", Change: "changed", TitleBefore: "old", TitleAfter: "new"}}
	if err := s.PutNotebookRevPages(ctx, "notes", 1, replacement); err != nil {
		t.Fatal(err)
	}
	if got, err := s.NotebookRevPages(ctx, "notes", 1, 1); err != nil || !reflect.DeepEqual(got, map[int][]NotebookPageChange{1: replacement}) {
		t.Fatalf("replacement summaries = %+v, %v", got, err)
	}
	if err := s.PutNotebookRevPages(ctx, "notes", 1, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := s.NotebookRevPages(ctx, "notes", 1, 3); err != nil || len(got) != 0 {
		t.Fatalf("cleared summaries = %+v, %v", got, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := s.NotebookImportRevision(cancelled, "notes", "store1", 5); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled import lookup = %v", err)
	}
	if _, err := s.NotebookRevPages(cancelled, "notes", 1, 2); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled summaries lookup = %v", err)
	}
}

func openNotebookSchema21(t *testing.T, path string) *Store {
	t.Helper()
	saved := migrations
	migrations = saved[:slices.IndexFunc(saved, func(m migration) bool { return m.version == 22 })]
	s, err := Open(path)
	migrations = saved
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if info, err := Inspect(path); err != nil || info.Version != 21 {
		t.Fatalf("schema 21 fixture = %+v, %v", info, err)
	}
	return s
}

func TestNotebookMigrationPreservesSchema21(t *testing.T) {
	for _, customized := range []bool{false, true} {
		name := "default autopilot"
		if customized {
			name = "configured autopilot"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "flats.db")
			s := openNotebookSchema21(t, path)
			if err := s.CreateFlat(ctx, Flat{Slug: "board", Name: "Board", Visibility: Private, CreatedAt: fixtureNow, UpdatedAt: fixtureNow}); err != nil {
				t.Fatal(err)
			}
			files := []RevisionFile{{Path: "assets/image.png", Hash: "image-hash", Size: 12}}
			r := Revision{Number: 1, CreatedAt: fixtureNow, Kind: "agent", Message: "keep revision", Authors: []string{"author"}, Files: 1, Size: 12, Pinned: true}
			counts := ElementCounts{Added: 2, Changed: 3, Removed: 4}
			if err := s.ContentTx(ctx, "board", func(tx *ContentTx) error {
				if err := tx.InitReadyState(ctx, "board"); err != nil {
					return err
				}
				if err := tx.InsertRevision(ctx, r, files); err != nil {
					return err
				}
				if err := tx.ReplaceHead(ctx, files); err != nil {
					return err
				}
				if err := tx.PutBoardImport(ctx, "board-store", 8, "board-op", 1); err != nil {
					return err
				}
				if err := tx.PutBoardRevStats(ctx, 1, counts); err != nil {
					return err
				}
				return tx.SetImport(ctx, "board-store", 8)
			}); err != nil {
				t.Fatal(err)
			}
			pendingApproval(t, s, "approval")
			if customized {
				if _, _, err := s.UpdateAutopilot(ctx, fixtureNow, func(row AutopilotRow) (AutopilotRow, string, error) {
					row.Enabled, row.Ops, row.Guide = true, 5, "keep guide"
					return row, "keep change", nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			flat, err := s.GetFlat(ctx, "board")
			if err != nil {
				t.Fatal(err)
			}
			state, err := s.ContentState(ctx, "board")
			if err != nil {
				t.Fatal(err)
			}
			revision, err := s.Revision(ctx, "board", 1)
			if err != nil {
				t.Fatal(err)
			}
			auto, changes, err := s.AutopilotSync(ctx)
			if err != nil {
				t.Fatal(err)
			}
			approval, err := s.GetApproval(ctx, "approval")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			if info, err := Inspect(path); err != nil || info.Version != 22 {
				t.Fatalf("upgraded schema = %+v, %v", info, err)
			}
			if got, err := s.GetFlat(ctx, "board"); err != nil || !reflect.DeepEqual(got, flat) {
				t.Fatalf("flat after upgrade = %+v, %v", got, err)
			}
			if got, err := s.ContentState(ctx, "board"); err != nil || !reflect.DeepEqual(got, state) {
				t.Fatalf("state after upgrade = %+v, %v", got, err)
			}
			if got, err := s.Revision(ctx, "board", 1); err != nil || !reflect.DeepEqual(got, revision) {
				t.Fatalf("revision after upgrade = %+v, %v", got, err)
			}
			if got, err := s.RevisionFiles(ctx, "board", 1); err != nil || !reflect.DeepEqual(got, files) {
				t.Fatalf("revision files after upgrade = %+v, %v", got, err)
			}
			if got, err := s.Head(ctx, "board"); err != nil || len(got) != 1 || got[0].Hash != files[0].Hash || got[0].Path != files[0].Path || got[0].Size != files[0].Size {
				t.Fatalf("head after upgrade = %+v, %v", got, err)
			}
			if n, ok, err := s.BoardImportRevision(ctx, "board", "board-store", 8); err != nil || !ok || n != 1 {
				t.Fatalf("board import after upgrade = %d %t %v", n, ok, err)
			}
			if got, err := s.BoardRevStats(ctx, "board", 1, 1); err != nil || !reflect.DeepEqual(got, map[int]ElementCounts{1: counts}) {
				t.Fatalf("board stats after upgrade = %+v, %v", got, err)
			}
			if got, err := s.GetApproval(ctx, "approval"); err != nil || !reflect.DeepEqual(got, approval) {
				t.Fatalf("approval after upgrade = %+v, %v", got, err)
			}
			if got, gotChanges, err := s.AutopilotSync(ctx); err != nil || got != auto || !reflect.DeepEqual(gotChanges, changes) {
				t.Fatalf("autopilot after upgrade = %+v %+v, %v", got, gotChanges, err)
			}
			for _, table := range []string{"notebook_imports", "notebook_rev_pages"} {
				var count int
				if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("%s after upgrade = %d, %v", table, count, err)
				}
			}
			backups := backupFiles(t, path)
			if len(backups) != 1 || !strings.HasPrefix(backups[0], "flats.v21.") {
				t.Fatalf("pre-upgrade backups = %v", backups)
			}
			backup := filepath.Join(filepath.Dir(path), "backups", backups[0])
			if info, err := Inspect(backup); err != nil || info.Version != 21 {
				t.Fatalf("backup schema = %+v, %v", info, err)
			}
			var backupRows int
			queryRO(t, backup, `SELECT COUNT(*) FROM board_imports WHERE flat='board' AND store='board-store' AND outbox_seq=8 AND op_id='board-op' AND revision=1`, &backupRows)
			if backupRows != 1 {
				t.Fatalf("backup board receipt count = %d", backupRows)
			}
			fresh := filepath.Join(t.TempDir(), "fresh.db")
			freshStore, err := Open(fresh)
			if err != nil {
				t.Fatal(err)
			}
			if err := freshStore.Close(); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(schemaShape(t, path), schemaShape(t, fresh)) {
				t.Fatal("upgraded and fresh schemas differ")
			}
			notebookRevisionFixture(t, s, "notes")
			pages := []NotebookPageChange{{Page: "p_a", Change: "added", TitleAfter: "new page"}}
			if err := s.ContentTx(ctx, "notes", func(tx *ContentTx) error {
				if err := tx.PutNotebookImport(ctx, "notebook-store", 1, "notebook-op", 1); err != nil {
					return err
				}
				return tx.PutNotebookRevPages(ctx, 1, pages)
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if n, ok, err := s.NotebookImportRevision(ctx, "notes", "notebook-store", 1); err != nil || !ok || n != 1 {
				t.Fatalf("receipt after restart = %d %t %v", n, ok, err)
			}
			if got, err := s.NotebookRevPages(ctx, "notes", 1, 1); err != nil || !reflect.DeepEqual(got, map[int][]NotebookPageChange{1: pages}) {
				t.Fatalf("summaries after restart = %+v, %v", got, err)
			}
			if got := backupFiles(t, path); !slices.Equal(got, backups) {
				t.Fatalf("restart repeated migration backup = %v", got)
			}
		})
	}
}

func TestNotebookMigrationRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flats.db")
	s := openNotebookSchema21(t, path)
	pendingApproval(t, s, "keep")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("notebook migration failed")
	t.Run("failed migration", func(t *testing.T) {
		broken := slices.Clone(migrations)
		broken[len(broken)-1].up = func(ctx context.Context, x executor) error {
			if err := addNotebookRevisions(ctx, x); err != nil {
				return err
			}
			return boom
		}
		withMigrations(t, broken)
		if _, err := Open(path); !errors.Is(err, boom) {
			t.Fatalf("migration error = %v", err)
		}
	})
	if info, err := Inspect(path); err != nil || info.Version != 21 {
		t.Fatalf("schema after failure = %+v, %v", info, err)
	}
	var tables int
	queryRO(t, path, `SELECT COUNT(*) FROM sqlite_master WHERE name IN ('notebook_imports','notebook_imports_revision','notebook_rev_pages')`, &tables)
	if tables != 0 {
		t.Fatalf("failed migration left %d schema objects", tables)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if info, err := Inspect(path); err != nil || info.Version != 22 {
		t.Fatalf("retried migration = %+v, %v", info, err)
	}
	if a, err := s.GetApproval(context.Background(), "keep"); err != nil || a.Status != "pending" {
		t.Fatalf("approval after retry = %+v, %v", a, err)
	}
}
