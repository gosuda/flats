package store

import (
	"context"
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
