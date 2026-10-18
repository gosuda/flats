package api_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
)

// The stats response keeps page views and top pages and adds usage. A website
// runs inside the Flats process: CPU and memory are not measured separately
// and their fields are omitted.
func TestStatsIncludesUsage(t *testing.T) {
	srv, svc := setup(t)
	ctx := context.Background()
	if _, err := svc.SaveVersion(ctx, "site", []bundle.File{{Path: "index.html", Data: []byte("<p>hi")}}, core.SaveMeta{}, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	code, out := req(t, "GET", srv.URL+"/api/flats/site/stats?days=7", nil, nil)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	for _, key := range []string{"page_views", "top_pages", "note", "usage"} {
		if _, ok := out[key]; !ok {
			t.Fatalf("missing %s in %v", key, out)
		}
	}
	usage := out["usage"].(map[string]any)
	days, ok := usage["days"].([]any)
	if !ok || len(days) != 0 {
		t.Fatalf("days %v", usage["days"])
	}
	totals := usage["totals"].(map[string]any)
	for _, key := range []string{"error_rate", "p50_ms", "p95_ms"} {
		if v, ok := totals[key]; !ok || v != nil {
			t.Fatalf("%s must be null without requests: %v", key, totals)
		}
	}
	if totals["requests"] != float64(0) || totals["errors"] != float64(0) {
		t.Fatalf("totals %v", totals)
	}
	if _, ok := totals["cpu_seconds"]; ok {
		t.Fatalf("cpu for a website %v", totals)
	}
	now := usage["now"].(map[string]any)
	if now["cpu"] != "not_separate" || now["memory"] != "not_separate" {
		t.Fatalf("now %v", now)
	}
	for _, key := range []string{"state", "workers", "memory_bytes", "memory_sampled_at", "memory_stale"} {
		if _, ok := now[key]; ok {
			t.Fatalf("%s present for a website: %v", key, now)
		}
	}
	if code, _ := req(t, "GET", srv.URL+"/api/flats/missing/stats", nil, nil); code != 404 {
		t.Fatalf("missing flat %d", code)
	}
}

// When usage cannot be read, stats still returns page views and top pages and
// omits usage, which the console then hides.
func TestStatsWithoutUsage(t *testing.T) {
	dir := t.TempDir()
	srv, svc := setupWith(t, dir, func(*core.Config) {})
	ctx := context.Background()
	if _, err := svc.SaveVersion(ctx, "site", []bundle.File{{Path: "index.html", Data: []byte("<p>hi")}}, core.SaveMeta{}, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `DROP TABLE usagedays`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Usage(ctx, "site", 7); err == nil {
		t.Fatal("usage read without its table")
	}
	code, out := req(t, "GET", srv.URL+"/api/flats/site/stats?days=7", nil, nil)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	for _, key := range []string{"page_views", "top_pages", "note"} {
		if _, ok := out[key]; !ok {
			t.Fatalf("missing %s in %v", key, out)
		}
	}
	if _, ok := out["usage"]; ok {
		t.Fatalf("usage present although it failed: %v", out["usage"])
	}
}
