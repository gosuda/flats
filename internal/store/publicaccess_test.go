package store

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// Migration 19 starts every Public flat at view and leaves others without a
// level.
func TestMigrationPublicAccess(t *testing.T) {
	path := loadFixture(t, "v5-80b206b")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for slug, want := range map[string]string{"blog": AccessView, "notes": "", "docs": ""} {
		f, err := s.GetFlat(ctx, slug)
		if err != nil || f.PublicAccess != want || f.AccessSeq != 0 {
			t.Fatalf("%s: %q seq %d %v", slug, f.PublicAccess, f.AccessSeq, err)
		}
	}
}

// CommitVisibility is a compare-and-set on visibility, level, access_seq and
// live version, and keeps the level invariant.
func TestCommitVisibilityAccess(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/flats.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	if err := s.CreateFlat(ctx, Flat{Slug: "plan", Name: "plan", Visibility: Private, LiveVersion: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	approve := func(id string) {
		t.Helper()
		if err := s.InsertApproval(ctx, Approval{ID: id, Flat: "plan", Action: "set_visibility", Params: []byte(`{}`), Status: "applying", RequestedAt: now, IdempotencyKey: id}); err != nil {
			t.Fatal(err)
		}
	}
	from, _ := s.GetFlat(ctx, "plan")
	next := from
	next.Visibility, next.PublicAccess = Public, AccessEdit
	approve("apr-a")
	if err := s.CommitVisibility(ctx, next, from, "apr-a", []byte(`{}`)); err == nil {
		t.Fatal("edit committed for a flat that is not a board")
	}
	if err := s.ContentTx(ctx, "plan", func(tx *ContentTx) error { return tx.InitState(ctx, "board") }); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Flat{{Visibility: Private, PublicAccess: AccessView}, {Visibility: Public}} {
		f := from
		f.Visibility, f.PublicAccess = bad.Visibility, bad.PublicAccess
		if err := s.CommitVisibility(ctx, f, from, "apr-a", []byte(`{}`)); err == nil {
			t.Fatalf("invalid pair %s %q committed", bad.Visibility, bad.PublicAccess)
		}
	}
	if err := s.CommitVisibility(ctx, next, from, "apr-a", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetFlat(ctx, "plan")
	if got.Visibility != Public || got.PublicAccess != AccessEdit || got.AccessSeq != 1 {
		t.Fatalf("after commit: %+v", got)
	}
	// A commit read before that change no longer applies.
	approve("apr-b")
	back := from
	back.Visibility, back.PublicAccess = Public, AccessView
	if err := s.CommitVisibility(ctx, back, from, "apr-b", []byte(`{}`)); err == nil {
		t.Fatal("stale commit applied")
	}
	if err := s.SetPublicAccess(ctx, "plan", AccessEdit, AccessView, Event{Level: "warn", Kind: "visibility", Message: "stopped"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetFlat(ctx, "plan"); got.PublicAccess != AccessView || got.AccessSeq != 2 {
		t.Fatalf("after SetPublicAccess: %+v", got)
	}
	if slugs, err := s.FlatsWithAccess(ctx, AccessView); err != nil || len(slugs) != 1 {
		t.Fatalf("flats at view = %v %v", slugs, err)
	}
}

// A document can be edited by anyone like a board: its type comes from its
// content state, or from its live version's manifest before the first
// revision import creates that row. Other types stay refused.
func TestCommitVisibilityEditDocuments(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/flats.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	n := 0
	try := func(slug, manifest string) error {
		t.Helper()
		if err := s.CreateFlat(ctx, Flat{Slug: slug, Name: slug, Visibility: Private, LiveVersion: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertVersion(ctx, Version{Flat: slug, Number: 1, Hash: slug, Kind: "static", Manifest: json.RawMessage(manifest), CreatedAt: now, Published: true}); err != nil {
			t.Fatal(err)
		}
		n++
		id := fmt.Sprintf("apr-%d", n)
		if err := s.InsertApproval(ctx, Approval{ID: id, Flat: slug, Action: "set_visibility", Params: []byte(`{}`), Status: "applying", RequestedAt: now, IdempotencyKey: id}); err != nil {
			t.Fatal(err)
		}
		from, _ := s.GetFlat(ctx, slug)
		next := from
		next.Visibility, next.PublicAccess = Public, AccessEdit
		return s.CommitVisibility(ctx, next, from, id, []byte(`{}`))
	}
	if err := try("notes", `{"type":"docs"}`); err != nil {
		t.Fatalf("document without content state: %v", err)
	}
	for slug, manifest := range map[string]string{"site": `{}`, "page": `{"type":"page"}`, "art": `{"type":"design"}`} {
		if err := try(slug, manifest); err == nil {
			t.Fatalf("%s committed edit", slug)
		}
	}
	// The content state wins over the manifest.
	if err := s.CreateFlat(ctx, Flat{Slug: "mixed", Name: "mixed", Visibility: Private, LiveVersion: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.ContentTx(ctx, "mixed", func(tx *ContentTx) error { return tx.InitState(ctx, "docs") }); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.publicEditable(ctx, "mixed"); err != nil || !ok {
		t.Fatalf("document content state: %v %v", ok, err)
	}
}

func (s *Store) publicEditable(ctx context.Context, flat string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	return publicEditableTx(ctx, tx, flat)
}

// Guest line ranges are stored with a revision's document files, read back
// with them, and go when the revision is deleted (migration 20).
func TestRevisionGuestLines(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/flats.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	if err := s.CreateFlat(ctx, Flat{Slug: "notes", Name: "notes", Visibility: Private, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	files := []RevisionFile{
		{Path: "a.md", Hash: "ha", Size: 1, GuestLines: &[][2]int{{1, 2}, {5, 5}}},
		{Path: "b.md", Hash: "hb", Size: 1},
	}
	err = s.ContentTx(ctx, "notes", func(tx *ContentTx) error {
		if err := tx.InitReadyState(ctx, "docs"); err != nil {
			return err
		}
		for _, n := range []int{1, 2} {
			if err := tx.InsertRevision(ctx, Revision{Number: n, CreatedAt: now, Kind: "person"}, files); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.RevisionFiles(ctx, "notes", 1)
	if err != nil || len(got) != 2 || len(got[0].Lines()) != 2 || got[0].Lines()[1] != [2]int{5, 5} || got[1].GuestLines != nil {
		t.Fatalf("files %+v %v", got, err)
	}
	if err := s.ContentTx(ctx, "notes", func(tx *ContentTx) error { return tx.DeleteRevision(ctx, 1) }); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM revision_guest_lines WHERE flat='notes'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("guest line rows after delete: %d %v", rows, err)
	}
	// Deleting the flat removes the rest.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM flats WHERE slug='notes'`); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM revision_guest_lines`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("guest line rows after flat delete: %d %v", rows, err)
	}
}
