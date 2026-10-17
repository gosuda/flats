package store

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func lat(b ...int64) []int64 {
	out := make([]int64, 13)
	copy(out, b)
	return out
}

func TestAddUsageMergesDays(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now().UTC()
	if err := s.CreateFlat(ctx, Flat{Slug: "blog", Name: "b", Visibility: Private, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	f, _ := s.GetFlat(ctx, "blog")
	first := []UsageDay{
		{Day: "2026-10-17", C2xx: 3, C5xx: 1, Timed: 4, DurationMS: 40, LatencyVersion: 1, Latency: lat(1, 3), CPUMS: 100, MemoryPeak: 50},
		{Day: "2026-10-18", C4xx: 2, Closed: 1, RateLimited: 2, CPUMS: 5},
	}
	if ok, err := s.AddUsage(ctx, f.ID, first); err != nil || !ok {
		t.Fatal(ok, err)
	}
	second := []UsageDay{{Day: "2026-10-17", C2xx: 1, Timed: 1, DurationMS: 7, LatencyVersion: 1, Latency: lat(0, 0, 1), CPUMS: 10, MemoryPeak: 40}}
	if ok, err := s.AddUsage(ctx, f.ID, second); err != nil || !ok {
		t.Fatal(ok, err)
	}
	got, err := s.UsageSince(ctx, f.ID, "2026-10-17")
	if err != nil || len(got) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	d := got[0]
	if d.Day != "2026-10-17" || d.C2xx != 4 || d.C5xx != 1 || d.Requests() != 5 || d.Timed != 5 || d.DurationMS != 47 ||
		!slices.Equal(d.Latency, lat(1, 3, 1)) || d.CPUMS != 110 || d.MemoryPeak != 50 {
		t.Fatalf("merged day %+v", d)
	}
	if e := got[1]; e.C4xx != 2 || e.Closed != 1 || e.RateLimited != 2 || e.LatencyVersion != 0 || e.Timed != 0 {
		t.Fatalf("second day %+v", e)
	}
	var requests int64
	s.db.QueryRow(`SELECT requests FROM usagedays WHERE flat_uid=? AND day='2026-10-17'`, f.ID).Scan(&requests)
	if requests != 5 {
		t.Fatalf("requests column %d", requests)
	}
	if later, _ := s.UsageSince(ctx, f.ID, "2026-10-18"); len(later) != 1 {
		t.Fatalf("since filter %+v", later)
	}
}

func TestAddUsageKeepsLatencyVersionsApart(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now().UTC()
	s.CreateFlat(ctx, Flat{Slug: "blog", Name: "b", Visibility: Private, CreatedAt: now, UpdatedAt: now})
	f, _ := s.GetFlat(ctx, "blog")
	// A row written with other bucket bounds keeps them; counts of another
	// version are not added into it.
	if _, err := s.db.Exec(`INSERT INTO usagedays(flat_uid,day,requests,c2xx,timed,duration_ms,latency) VALUES(?,?,2,2,2,30,'{"v":2,"b":[1,1]}')`, f.ID, "2026-10-18"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddUsage(ctx, f.ID, []UsageDay{{Day: "2026-10-18", C2xx: 1, Timed: 1, DurationMS: 5, LatencyVersion: 1, Latency: lat(1)}}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.UsageSince(ctx, f.ID, "2026-10-18")
	if len(got) != 1 || got[0].C2xx != 3 || got[0].LatencyVersion != 2 || !slices.Equal(got[0].Latency, []int64{1, 1}) || got[0].Timed != 2 || got[0].DurationMS != 30 {
		t.Fatalf("%+v", got)
	}
	// Unreadable latency counts as no buckets.
	if v, b := decodeLatency("{bad"); v != 0 || b != nil {
		t.Fatal(v, b)
	}
}

func TestUsageOfDeletedFlatIsDiscarded(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now().UTC()
	s.CreateFlat(ctx, Flat{Slug: "blog", Name: "b", Visibility: Private, CreatedAt: now, UpdatedAt: now})
	old, _ := s.GetFlat(ctx, "blog")
	if ok, err := s.AddUsage(ctx, old.ID, []UsageDay{{Day: "2026-10-18", C2xx: 9}}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := s.DeleteFlat(ctx, "blog"); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM usagedays`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d usage rows survived the delete", n)
	}
	// A recreated slug gets a new ID and starts empty; a late flush under the
	// old ID writes nothing.
	s.CreateFlat(ctx, Flat{Slug: "blog", Name: "b", Visibility: Private, CreatedAt: now, UpdatedAt: now})
	fresh, _ := s.GetFlat(ctx, "blog")
	if fresh.ID == old.ID {
		t.Fatal("recreated flat reused the ID")
	}
	if ok, err := s.AddUsage(ctx, old.ID, []UsageDay{{Day: "2026-10-18", C2xx: 1}}); err != nil || ok {
		t.Fatalf("late flush for a deleted flat: %t %v", ok, err)
	}
	if got, _ := s.UsageSince(ctx, fresh.ID, ""); len(got) != 0 {
		t.Fatalf("recreated flat has usage %+v", got)
	}
	s.db.QueryRow(`SELECT COUNT(*) FROM usagedays`).Scan(&n)
	if n != 0 {
		t.Fatalf("late flush wrote %d rows", n)
	}
	// Usage follows a rename, which keeps the ID.
	s.AddUsage(ctx, fresh.ID, []UsageDay{{Day: "2026-10-18", C2xx: 1}})
	if err := s.RenameFlat(ctx, "blog", "journal", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	renamed, _ := s.GetFlat(ctx, "journal")
	if got, _ := s.UsageSince(ctx, renamed.ID, ""); renamed.ID != fresh.ID || len(got) != 1 {
		t.Fatalf("usage after rename %+v", got)
	}
}

func TestFlatTypesTakeTheViewsType(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now().UTC()
	for _, slug := range []string{"live", "saved", "draft", "empty"} {
		s.CreateFlat(ctx, Flat{Slug: slug, Name: slug, Visibility: Private, CreatedAt: now, UpdatedAt: now})
	}
	s.InsertVersion(ctx, Version{Flat: "live", Number: 1, Hash: "h", Kind: "server", Manifest: []byte(`{"type":"docs"}`), CreatedAt: now, Published: true})
	s.InsertVersion(ctx, Version{Flat: "live", Number: 2, Hash: "h2", Kind: "static", Manifest: []byte(`{"type":"page"}`), CreatedAt: now, Published: true})
	if err := s.SetLive(ctx, "live", 1, 0, "deploy", "", now); err != nil {
		t.Fatal(err)
	}
	s.InsertVersion(ctx, Version{Flat: "saved", Number: 1, Hash: "h", Kind: "static", Manifest: []byte(`{}`), CreatedAt: now, Published: true})
	s.InsertVersion(ctx, Version{Flat: "saved", Number: 2, Hash: "h2", Kind: "static", Manifest: []byte(`{"type":"page"}`), CreatedAt: now, Published: true})
	if err := s.PutDraft(ctx, Draft{Flat: "draft", Revision: 1, Hash: "d", Kind: "server", Manifest: []byte(`{"kind":"server"}`), UpdatedAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	s.db.Exec(`UPDATE flats SET visibility='public' WHERE slug='saved'`)
	fts, err := s.FlatTypes(ctx)
	if err != nil || len(fts) != 4 {
		t.Fatalf("%+v %v", fts, err)
	}
	byslug := map[string]FlatType{}
	for _, ft := range fts {
		byslug[ft.Slug] = ft
		if len(ft.UID) != 32 {
			t.Fatalf("%s uid %q", ft.Slug, ft.UID)
		}
	}
	for slug, want := range map[string]struct{ manifest, kind string }{
		"live":  {`{"type":"docs"}`, "server"},
		"saved": {`{"type":"page"}`, "static"},
		"draft": {`{"kind":"server"}`, "server"},
		"empty": {"", ""},
	} {
		if ft := byslug[slug]; string(ft.Manifest) != want.manifest || ft.Kind != want.kind || (slug == "saved") != (ft.Visibility == Public) {
			t.Fatalf("%s: %+v", slug, ft)
		}
	}
	if one, err := s.FlatTypeOf(ctx, "live"); err != nil || one.Kind != "server" {
		t.Fatalf("%+v %v", one, err)
	}
	if _, err := s.FlatTypeOf(ctx, "missing"); err != ErrNotFound {
		t.Fatal(err)
	}
}

func TestPendingApprovalsAndSize(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now().UTC()
	s.CreateFlat(ctx, Flat{Slug: "blog", Name: "b", Visibility: Private, CreatedAt: now, UpdatedAt: now})
	for i, status := range []string{"pending", "pending", "approved"} {
		if err := s.InsertApproval(ctx, Approval{ID: string(rune('a' + i)), Flat: "blog", Action: "delete", Params: []byte("{}"), Status: status, Via: "api", RequestedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.PendingApprovals(ctx); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	fi, err := os.Stat(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if size := s.Size(); size < fi.Size() {
		t.Fatalf("size %d below the file's %d", size, fi.Size())
	}
}

// A database at the previous schema gains the usage table; its flats keep
// their IDs and start with no usage.
func TestMigrationAddsUsageDays(t *testing.T) {
	ctx := context.Background()
	full := migrations
	path := filepath.Join(t.TempDir(), "flats.db")
	withMigrations(t, full[:len(full)-1])
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.CreateFlat(ctx, Flat{Slug: "blog", Name: "b", Visibility: Private, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetFlat(ctx, "blog")
	var tables int
	s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='usagedays'`).Scan(&tables)
	s.Close()
	if tables != 0 {
		t.Fatal("previous schema already has usagedays")
	}
	migrations = full
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if info, _ := Inspect(path); info.Version != 17 {
		t.Fatalf("version %d", info.Version)
	}
	after, _ := s.GetFlat(ctx, "blog")
	if after.ID != before.ID {
		t.Fatal("flat ID changed")
	}
	if got, err := s.UsageSince(ctx, after.ID, ""); err != nil || len(got) != 0 {
		t.Fatalf("%+v %v", got, err)
	}
	if ok, err := s.AddUsage(ctx, after.ID, []UsageDay{{Day: "2026-10-18", C2xx: 1}}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if bs := backupFiles(t, path); len(bs) != 1 || !strings.HasPrefix(bs[0], "flats.v16.") {
		t.Fatalf("backups %v", bs)
	}
}
