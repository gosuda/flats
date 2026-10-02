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

// Regression (correctness C7): redirects are kept per old slug and follow
// later renames of their flat.
func TestRedirectsFollowRenames(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now().UTC()
	till := now.Add(7 * 24 * time.Hour)
	s.CreateFlat(ctx, Flat{Slug: "aaa", Name: "a", Visibility: Private, CreatedAt: now, UpdatedAt: now})
	if err := s.RenameFlat(ctx, "aaa", "bbb", till, now); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameFlat(ctx, "bbb", "ccc", till, now); err != nil {
		t.Fatal(err)
	}
	for _, old := range []string{"aaa", "bbb"} {
		if r, err := s.RedirectFor(ctx, old, now); err != nil || r.Flat != "ccc" {
			t.Fatalf("redirect from %s: %+v %v", old, r, err)
		}
	}
	// Taking an old slug back ends its redirect; a rename without a window
	// records none.
	if err := s.RenameFlat(ctx, "ccc", "aaa", time.Time{}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedirectFor(ctx, "aaa", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("aaa is a flat again, not a redirect: %v", err)
	}
	if _, err := s.RedirectFor(ctx, "ccc", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename without window recorded a redirect: %v", err)
	}
	rs, _ := s.ActiveRedirects(ctx, now)
	if len(rs) != 1 || rs[0].Old != "bbb" || rs[0].Flat != "aaa" {
		t.Fatalf("active redirects %+v", rs)
	}
	if err := s.DeleteExpiredRedirects(ctx, till.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if rs, _ := s.ActiveRedirects(ctx, now); len(rs) != 0 {
		t.Fatalf("expired redirects kept: %+v", rs)
	}
}

func TestDuplicateSlugIsErrExists(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now().UTC()
	for _, slug := range []string{"one", "two"} {
		s.CreateFlat(ctx, Flat{Slug: slug, Name: slug, Visibility: Private, CreatedAt: now, UpdatedAt: now})
	}
	if err := s.CreateFlat(ctx, Flat{Slug: "one", Name: "x", Visibility: Private, CreatedAt: now, UpdatedAt: now}); !errors.Is(err, ErrExists) {
		t.Fatalf("create: %v", err)
	}
	if err := s.RenameFlat(ctx, "two", "one", time.Time{}, now); !errors.Is(err, ErrExists) {
		t.Fatalf("rename: %v", err)
	}
}

// Regression (interfaces F7): a claimed approval cannot be decided again.
func TestClaimApproval(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now().UTC()
	s.InsertApproval(ctx, Approval{ID: "a1", Flat: "f", Action: "delete", Params: []byte("{}"), Status: "pending", Via: "mcp", RequestedAt: now})
	if err := s.ClaimApproval(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimApproval(ctx, "a1"); err == nil {
		t.Fatal("claimed twice")
	}
	if err := s.DecideApproval(ctx, "a1", "rejected", "x", now); err == nil {
		t.Fatal("claimed approval rejected")
	}
	if err := s.FinishApproval(ctx, "a1", "approved", "ok", now); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.GetApproval(ctx, "a1"); a.Status != "approved" || a.DecidedAt == nil {
		t.Fatalf("approval %+v", a)
	}
}

// Regression (spec F7): PruneEvents keeps the newest events of one flat.
func TestPruneEvents(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now().UTC()
	for i := 0; i < 20; i++ {
		s.AddEvent(ctx, Event{Flat: "f", Time: now, Level: "info", Kind: "x", Message: "m"})
		s.AddEvent(ctx, Event{Flat: "g", Time: now, Level: "info", Kind: "x", Message: "m"})
	}
	n, err := s.PruneEvents(ctx, "f", 5)
	if err != nil || n != 15 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	if evs, _ := s.ListEvents(ctx, "f", "", 0, 100); len(evs) != 5 {
		t.Fatalf("f has %d events", len(evs))
	}
	if evs, _ := s.ListEvents(ctx, "g", "", 0, 100); len(evs) != 20 {
		t.Fatalf("other flat pruned: %d", len(evs))
	}
	if n, _ := s.PruneEvents(ctx, "f", 5); n != 0 {
		t.Fatalf("second prune removed %d", n)
	}
}

// Rename redirects recorded on the flat row before the redirects table
// existed are carried over, unless a flat uses the old slug again.
func TestMigrateOldSlugRedirects(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "f.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	till := unix(now.Add(time.Hour))
	s.CreateFlat(ctx, Flat{Slug: "new", Name: "n", Visibility: Private, CreatedAt: now, UpdatedAt: now})
	s.CreateFlat(ctx, Flat{Slug: "taken", Name: "t", Visibility: Private, CreatedAt: now, UpdatedAt: now})
	s.CreateFlat(ctx, Flat{Slug: "other", Name: "o", Visibility: Private, CreatedAt: now, UpdatedAt: now})
	s.db.Exec(`UPDATE flats SET old_slug='old', old_slug_until=? WHERE slug='new'`, till)
	s.db.Exec(`UPDATE flats SET old_slug='taken', old_slug_until=? WHERE slug='other'`, till)
	s.db.Exec(`DELETE FROM redirects`)
	s.db.Exec(`PRAGMA user_version = 0`)
	s.Close()
	s = open2(t, path)
	if r, err := s.RedirectFor(ctx, "old", now); err != nil || r.Flat != "new" {
		t.Fatalf("migrated redirect: %+v %v", r, err)
	}
	if _, err := s.RedirectFor(ctx, "taken", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("redirect over an existing flat migrated: %v", err)
	}
}

func open2(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
