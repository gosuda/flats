package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"
)

func sheetRevisionFixture(t *testing.T, s *Store, flat string) {
	t.Helper()
	ctx := context.Background()
	if err := s.CreateFlat(ctx, Flat{Slug: flat, Name: flat, Visibility: Private, CreatedAt: fixtureNow, UpdatedAt: fixtureNow}); err != nil {
		t.Fatal(err)
	}
	if err := s.ContentTx(ctx, flat, func(tx *ContentTx) error {
		if err := tx.InitReadyState(ctx, "sheet"); err != nil {
			return err
		}
		for n := 1; n <= 2; n++ {
			if err := tx.InsertRevision(ctx, Revision{Number: n, Kind: "agent", CreatedAt: fixtureNow}, nil); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSheetRevisionRows(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	tabs := []SheetTabChange{
		{Tab: 9, Change: "added", NameAfter: "Notes"},
		{Tab: 1, Change: "changed", NameBefore: "Sheet1", NameAfter: "예산 📊"},
		{Tab: 4, Change: "removed", NameBefore: "Old"},
	}
	for _, flat := range []string{"budget", "other"} {
		sheetRevisionFixture(t, s, flat)
		if err := s.ContentTx(ctx, flat, func(tx *ContentTx) error {
			for n := 1; n <= 2; n++ {
				if err := tx.PutSheetImport(ctx, "store1", int64(n), "op", n, "2026-11-1"+string(rune('0'+n))); err != nil {
					return err
				}
				if err := tx.PutSheetRevTabs(ctx, n, tabs); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	want := []SheetTabChange{tabs[1], tabs[2], tabs[0]}
	if got, err := s.SheetRevTabs(ctx, "budget", 1, 1); err != nil || !reflect.DeepEqual(got, map[int][]SheetTabChange{1: want}) {
		t.Fatalf("tab summaries = %+v, %v", got, err)
	}
	if got, err := s.SheetRevTabs(ctx, "budget", 3, 4); err != nil || len(got) != 0 {
		t.Fatalf("absent summaries = %+v, %v", got, err)
	}
	if date, ok, err := s.SheetRevisionDate(ctx, "budget", 2); err != nil || !ok || date != "2026-11-12" {
		t.Fatalf("revision date = %q %t %v", date, ok, err)
	}
	if date, ok, err := s.SheetRevisionDate(ctx, "budget", 3); err != nil || ok || date != "" {
		t.Fatalf("missing revision date = %q %t %v", date, ok, err)
	}
	if n, ok, err := s.SheetImportRevision(ctx, "budget", "missing", 1); err != nil || ok || n != 0 {
		t.Fatalf("missing import = %d %t %v", n, ok, err)
	}
	// A revision has exactly one import row.
	if err := s.ContentTx(ctx, "budget", func(tx *ContentTx) error { return tx.PutSheetImport(ctx, "store2", 1, "", 2, "2026-11-13") }); err == nil {
		t.Fatal("second import row of one revision was stored")
	}
	if err := s.ContentTx(ctx, "budget", func(tx *ContentTx) error {
		return tx.PutSheetRevTabs(ctx, 2, []SheetTabChange{{Tab: 1, Change: "renamed"}})
	}); err == nil {
		t.Fatal("unknown change kind was stored")
	}
	if err := s.ContentTx(ctx, "budget", func(tx *ContentTx) error { return tx.PutSheetRevTabs(ctx, 7, tabs[:1]) }); err == nil {
		t.Fatal("summary of a missing revision was stored")
	}
	if err := s.ContentTx(ctx, "budget", func(tx *ContentTx) error { return tx.DeleteRevision(ctx, 1) }); err != nil {
		t.Fatal(err)
	}
	if n, ok, err := s.SheetImportRevision(ctx, "budget", "store1", 1); err != nil || ok || n != 0 {
		t.Fatalf("pruned import = %d %t %v", n, ok, err)
	}
	if got, err := s.SheetRevTabs(ctx, "budget", 1, 2); err != nil || !reflect.DeepEqual(got, map[int][]SheetTabChange{2: want}) {
		t.Fatalf("summaries after retention = %+v, %v", got, err)
	}
	if err := s.RenameFlat(ctx, "budget", "renamed", time.Time{}, fixtureNow); err != nil {
		t.Fatal(err)
	}
	if n, ok, err := s.SheetImportRevision(ctx, "renamed", "store1", 2); err != nil || !ok || n != 2 {
		t.Fatalf("renamed import = %d %t %v", n, ok, err)
	}
	if date, ok, err := s.SheetRevisionDate(ctx, "renamed", 2); err != nil || !ok || date != "2026-11-12" {
		t.Fatalf("renamed date = %q %t %v", date, ok, err)
	}
	if got, err := s.SheetRevTabs(ctx, "budget", 1, 2); err != nil || len(got) != 0 {
		t.Fatalf("old slug summaries = %+v, %v", got, err)
	}
	if err := s.DeleteFlat(ctx, "renamed", Audit{}); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM sheet_imports WHERE flat='renamed') + (SELECT COUNT(*) FROM sheet_rev_tabs WHERE flat='renamed')`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("rows after flat delete = %d, %v", remaining, err)
	}
	if got, err := s.SheetRevTabs(ctx, "other", 1, 2); err != nil || !reflect.DeepEqual(got, map[int][]SheetTabChange{1: want, 2: want}) {
		t.Fatalf("other flat summaries = %+v, %v", got, err)
	}
}

func TestSheetHostDerivedType(t *testing.T) {
	for _, value := range []any{Version{Manifest: json.RawMessage(`{"type":"sheet"}`)}, Draft{Manifest: json.RawMessage(`{"type":"sheet"}`)}} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var got struct{ Type string }
		if err := json.Unmarshal(raw, &got); err != nil || got.Type != "sheet" {
			t.Fatalf("derived type: %s, %v", raw, err)
		}
	}
	tabs := []SheetTabChange{{Tab: 3, Change: "changed", NameBefore: "Sheet1", NameAfter: "Budget"}}
	raw, err := json.Marshal(Revision{Number: 2, SheetTabs: tabs})
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Tabs []SheetTabChange }
	if err := json.Unmarshal(raw, &got); err != nil || len(got.Tabs) != 1 || got.Tabs[0] != tabs[0] {
		t.Fatalf("revision summaries: %s, %v", raw, err)
	}
}

// A sheet is never editable by anyone: a Public approval is view only.
func TestSheetHostPublicViewOnly(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.CreateFlat(ctx, Flat{Slug: "budget", Name: "Budget", Visibility: Private, LiveVersion: 1, CreatedAt: fixtureNow, UpdatedAt: fixtureNow}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertVersion(ctx, Version{Flat: "budget", Number: 1, Hash: "shell", Kind: "static", Manifest: json.RawMessage(`{"type":"sheet"}`), CreatedAt: fixtureNow, Published: true}); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"manifest", "content state"} {
		if phase == "content state" {
			if err := s.ContentTx(ctx, "budget", func(tx *ContentTx) error { return tx.InitReadyState(ctx, "sheet") }); err != nil {
				t.Fatal(err)
			}
		}
		if ok, err := s.publicEditable(ctx, "budget"); err != nil || ok {
			t.Fatalf("%s: sheet editable: %v, %v", phase, ok, err)
		}
	}
	id := "approval-edit"
	if err := s.InsertApproval(ctx, Approval{ID: id, Flat: "budget", Action: "set_visibility", Params: json.RawMessage(`{}`), Status: "applying", RequestedAt: fixtureNow, IdempotencyKey: id}); err != nil {
		t.Fatal(err)
	}
	from, err := s.GetFlat(ctx, "budget")
	if err != nil {
		t.Fatal(err)
	}
	next := from
	next.Visibility, next.PublicAccess = Public, AccessEdit
	if err := s.CommitVisibility(ctx, next, from, id, json.RawMessage(`{}`), Audit{}); err == nil {
		t.Fatal("sheet Public edit committed")
	}
}

func TestSheetMigrationFromSchema27(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "flats.db")
	s := openNotebookSchema(t, path, 27)
	notebookRevisionFixture(t, s, "notes")
	if err := s.ContentTx(ctx, "notes", func(tx *ContentTx) error { return tx.PutNotebookImport(ctx, "store", 1, "op", 1) }); err != nil {
		t.Fatal(err)
	}
	pendingApproval(t, s, "keep")
	var tables int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'sheet_%'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("schema 27 holds %d sheet objects, %v", tables, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("sheet migration failed")
	t.Run("failed migration", func(t *testing.T) {
		broken := slices.Clone(migrations)
		broken[slices.IndexFunc(broken, func(m migration) bool { return m.version == 28 })].up = func(ctx context.Context, x executor) error {
			if err := addSheetRevisions(ctx, x); err != nil {
				return err
			}
			return boom
		}
		withMigrations(t, broken)
		if _, err := Open(path); !errors.Is(err, boom) {
			t.Fatalf("migration error = %v", err)
		}
	})
	if info, err := Inspect(path); err != nil || info.Version != 27 {
		t.Fatalf("schema after failure = %+v, %v", info, err)
	}
	queryRO(t, path, `SELECT COUNT(*) FROM sqlite_master WHERE name IN ('sheet_imports','sheet_imports_revision','sheet_rev_tabs')`, &tables)
	if tables != 0 {
		t.Fatalf("failed migration left %d schema objects", tables)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if info, err := Inspect(path); err != nil || info.Version != 28 {
		t.Fatalf("migrated schema = %+v, %v", info, err)
	}
	if n, ok, err := s.NotebookImportRevision(ctx, "notes", "store", 1); err != nil || !ok || n != 1 {
		t.Fatalf("notebook import after migration = %d %t %v", n, ok, err)
	}
	if a, err := s.GetApproval(ctx, "keep"); err != nil || a.Status != "pending" {
		t.Fatalf("approval after migration = %+v, %v", a, err)
	}
	sheetRevisionFixture(t, s, "budget")
	if err := s.ContentTx(ctx, "budget", func(tx *ContentTx) error {
		if err := tx.PutSheetImport(ctx, "store", 1, "", 1, "2026-11-13"); err != nil {
			return err
		}
		return tx.PutSheetRevTabs(ctx, 1, []SheetTabChange{{Tab: 1, Change: "added", NameAfter: "Sheet1"}})
	}); err != nil {
		t.Fatal(err)
	}
}
