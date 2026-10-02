package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestFlatLifecycleAndCAS(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now().UTC()
	if err := s.CreateFlat(ctx, Flat{Slug: "blog", Name: "Blog", Visibility: Private, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateFlat(ctx, Flat{Slug: "blog", Name: "x", Visibility: Private, CreatedAt: now, UpdatedAt: now}); err == nil {
		t.Fatal("duplicate slug accepted")
	}
	s.InsertVersion(ctx, Version{Flat: "blog", Number: 1, Hash: "h", Kind: "static", Manifest: []byte("{}"), CreatedAt: now})
	if err := s.SetLive(ctx, "blog", 1, 0, "deploy", now); err != nil {
		t.Fatal(err)
	}
	// Compare-and-swap: a stale previous version must fail.
	if err := s.SetLive(ctx, "blog", 1, 0, "deploy", now); err == nil {
		t.Fatal("stale SetLive succeeded")
	}
	f, _ := s.GetFlat(ctx, "blog")
	if f.LiveVersion != 1 {
		t.Fatalf("live %d", f.LiveVersion)
	}
	ds, _ := s.ListDeployments(ctx, "blog", 10)
	if len(ds) != 1 || ds[0].Kind != "deploy" {
		t.Fatalf("deployments %+v", ds)
	}
}

func TestRenameCascadesAndRedirectWindow(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now().UTC()
	s.CreateFlat(ctx, Flat{Slug: "old", Name: "o", Visibility: Private, CreatedAt: now, UpdatedAt: now})
	s.InsertVersion(ctx, Version{Flat: "old", Number: 1, Hash: "h", Kind: "static", Manifest: []byte("{}"), CreatedAt: now})
	s.AddEvent(ctx, Event{Flat: "old", Time: now, Level: "info", Kind: "x", Message: "m"})
	s.PutSecret(ctx, "old", SealedSecret{Name: "K", Nonce: []byte{1}, Ciphertext: []byte{2}, UpdatedAt: now})
	if err := s.RenameFlat(ctx, "old", "new", now.Add(7*24*time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetVersion(ctx, "new", 1); err != nil {
		t.Fatalf("version did not follow rename: %v", err)
	}
	if evs, _ := s.ListEvents(ctx, "new", "", 0, 10); len(evs) != 1 {
		t.Fatalf("events did not follow: %d", len(evs))
	}
	if secs, _ := s.ListSecrets(ctx, "new"); len(secs) != 1 {
		t.Fatal("secrets did not follow rename")
	}
	if f, err := s.FlatByOldSlug(ctx, "old", now); err != nil || f.Slug != "new" {
		t.Fatalf("redirect lookup: %v", err)
	}
	if _, err := s.FlatByOldSlug(ctx, "old", now.Add(8*24*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatal("redirect must expire")
	}
}

func TestApprovalsDecideOnce(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now().UTC()
	s.InsertApproval(ctx, Approval{ID: "a1", Flat: "f", Action: "delete", Params: []byte("{}"), Status: "pending", Via: "mcp", RequestedAt: now})
	if err := s.DecideApproval(ctx, "a1", "approved", "ok", now); err != nil {
		t.Fatal(err)
	}
	if err := s.DecideApproval(ctx, "a1", "rejected", "x", now); err == nil {
		t.Fatal("approval decided twice")
	}
	if as, _ := s.ListApprovals(ctx, "pending"); len(as) != 0 {
		t.Fatal("decided approval still pending")
	}
}

func TestEventsPagingAndPageViews(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now().UTC()
	var last int64
	for i := 0; i < 5; i++ {
		last, _ = s.AddEvent(ctx, Event{Flat: "f", Time: now, Level: "info", Kind: "deploy", Message: "m"})
	}
	if evs, _ := s.ListEvents(ctx, "f", "", last-2, 10); len(evs) != 2 {
		t.Fatalf("after paging: %d", len(evs))
	}
	if evs, _ := s.ListEvents(ctx, "f", "", 0, 3); len(evs) != 3 || evs[2].ID != last {
		t.Fatalf("tail: %+v", evs)
	}
	s.AddPageViews(ctx, "f", "2026-10-01", 3)
	s.AddPageViews(ctx, "f", "2026-10-01", 2)
	pv, _ := s.PageViews(ctx, "f", 7)
	if len(pv) != 1 || pv[0].Count != 5 {
		t.Fatalf("pageviews %+v", pv)
	}
}
