package store

import (
	"context"
	"testing"
	"time"
)

func TestAnalyticsPeriodAndLifecycle(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now().UTC()
	if err := s.CreateFlat(ctx, Flat{Slug: "blog", Name: "Blog", Visibility: Private, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for day, n := range map[string]int64{"2026-08-01": 20, "2026-10-01": 2, "2026-10-02": 3} {
		if err := s.AddPageViews(ctx, "blog", day, n); err != nil {
			t.Fatal(err)
		}
	}
	counts := []PathCount{
		{Day: "2026-08-01", Path: "/old", Count: 20},
		{Day: "2026-10-01", Path: "/", Count: 2},
		{Day: "2026-10-02", Path: "/", Count: 1},
		{Day: "2026-10-02", Path: "/notes", Count: 2},
	}
	if err := s.AddPathCounts(ctx, "blog", counts); err != nil {
		t.Fatal(err)
	}
	views, err := s.PageViewsSince(ctx, "blog", "2026-09-27")
	if err != nil || len(views) != 2 || views[0].Count != 2 || views[1].Count != 3 {
		t.Fatalf("period views: %+v, %v", views, err)
	}
	if err := s.RenameFlat(ctx, "blog", "notes", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	pages, err := s.TopPages(ctx, "notes", "2026-09-27")
	if err != nil || len(pages) != 2 || pages[0].Path != "/" || pages[0].Count != 3 || pages[1].Count != 2 {
		t.Fatalf("renamed page totals: %+v, %v", pages, err)
	}
	if err := s.DeleteFlat(ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	pages, err = s.TopPages(ctx, "notes", "2026-01-01")
	if err != nil || len(pages) != 0 {
		t.Fatalf("deleted page totals: %+v, %v", pages, err)
	}
}
