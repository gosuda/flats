package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestPortalListing(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.UnixMilli(1_700_000_000_000)
	if err := s.CreateFlat(ctx, Flat{Slug: "blog", Name: "Blog", Visibility: Public, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if m, err := s.PortalListing(ctx, "blog"); err != nil || m != ListingDefault {
		t.Fatalf("initial = %q %v", m, err)
	}
	for _, mode := range []string{ListingHidden, ListingListed, ListingDefault, ListingHidden} {
		if err := s.SetPortalListing(ctx, "blog", mode, now); err != nil {
			t.Fatal(err)
		}
		if m, err := s.PortalListing(ctx, "blog"); err != nil || m != mode {
			t.Fatalf("after %s = %q %v", mode, m, err)
		}
	}
	if err := s.SetPortalListing(ctx, "blog", "unlisted", now); err == nil {
		t.Fatal("invalid mode accepted")
	}
	// The override follows a rename and goes with the flat.
	if err := s.RenameFlat(ctx, "blog", "journal", time.Time{}, now); err != nil {
		t.Fatal(err)
	}
	if m, err := s.PortalListing(ctx, "journal"); err != nil || m != ListingHidden {
		t.Fatalf("after rename = %q %v", m, err)
	}
	if err := s.DeleteFlat(ctx, "journal"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateFlat(ctx, Flat{Slug: "journal", Name: "Journal", Visibility: Private, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if m, err := s.PortalListing(ctx, "journal"); err != nil || m != ListingDefault {
		t.Fatalf("recreated flat inherited %q %v", m, err)
	}
}
