package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLitePageQuota(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	d := newDataStore(dir)
	ctx := context.Background()
	c, err := d.conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var pageSize, maxPages int64
	if err := c.QueryRowContext(ctx, "PRAGMA main.page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if err := c.QueryRowContext(ctx, "PRAGMA main.max_page_count").Scan(&maxPages); err != nil {
		t.Fatal(err)
	}
	if maxPages != int64(MaxDBBytes)/pageSize {
		t.Fatalf("max_page_count = %d, want %d", maxPages, int64(MaxDBBytes)/pageSize)
	}
	var mode string
	if err := c.QueryRowContext(ctx, "PRAGMA main.journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "delete" {
		t.Fatalf("journal_mode = %q, want delete", mode)
	}
	for _, query := range []string{
		"PRAGMA max_page_count = 1000000000",
		"PRAGMA \"MAX_PAGE_COUNT\" = 1000000000",
		"PRAGMA page_size = 65536",
		"PRAGMA journal_mode = WAL",
		"PRAGMA temp_store = FILE",
		"PRAGMA \"Temp_Store\" = FILE",
	} {
		if _, err := dbExec(ctx, c, query, nil); err == nil {
			t.Errorf("%q changed a storage limit", query)
		}
	}
	if got, err := dbQuery(ctx, c, "PRAGMA temp_store", nil); err != nil || got != "[{\"temp_store\":2}]" {
		t.Fatalf("read temp_store: %s, %v", got, err)
	}
	if _, err := c.ExecContext(ctx, "CREATE TABLE entries(value BLOB)"); err != nil {
		t.Fatal(err)
	}
	var applied int64
	if err := c.QueryRowContext(ctx, "PRAGMA main.max_page_count = 64").Scan(&applied); err != nil || applied != 64 {
		t.Fatalf("tight test page limit = %d, %v", applied, err)
	}
	if _, err := c.ExecContext(ctx, "INSERT INTO entries(value) VALUES (zeroblob(1048576))"); err == nil {
		t.Fatal("SQLite grew past the page budget")
	}
	var count int64
	if err := c.QueryRowContext(ctx, "PRAGMA main.page_count").Scan(&count); err != nil || count > 64 {
		t.Fatalf("page count = %d, %v", count, err)
	}
	c.Close()
	d.Close()
	restarted := newDataStore(dir)
	defer restarted.Close()
	c, err = restarted.conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.QueryRowContext(ctx, "PRAGMA main.max_page_count").Scan(&maxPages); err != nil || maxPages != int64(MaxDBBytes)/pageSize {
		t.Fatalf("restarted page budget = %d, %v", maxPages, err)
	}
}

func TestFilesObjectAccounting(t *testing.T) {
	t.Parallel()
	d := newDataStore(t.TempDir())
	defer d.Close()
	for _, entry := range []struct{ key, value string }{
		{"a/b/empty", ""}, {"a/b/empty", "updated"}, {"a/b/other", ""},
	} {
		if err := d.filePut(entry.key, entry.value); err != nil {
			t.Fatal(err)
		}
	}
	if got := d.filesEntries; got != 4 {
		t.Fatalf("file and directory count = %d, want 4", got)
	}
	if removed, err := d.fileDelete("a/b/empty"); err != nil || !removed {
		t.Fatalf("delete: %v, %v", removed, err)
	}
	if got := d.filesEntries; got != 3 {
		t.Fatalf("count after delete = %d, want 3", got)
	}
	if removed, err := d.fileDelete("a/b/other"); err != nil || !removed {
		t.Fatalf("delete: %v, %v", removed, err)
	}
	if got := d.filesEntries; got != 0 {
		t.Fatalf("count after directory pruning = %d, want 0", got)
	}
	if got := d.filesUsed.Load(); got != 0 {
		t.Fatalf("bytes after delete = %d, want 0", got)
	}
	// A failed write can leave parents behind; it must invalidate the cache.
	if err := d.filePut("created/"+strings.Repeat("x", 256), "x"); err == nil {
		t.Skip("filesystem permits 256-byte path segments")
	}
	if got := d.filesUsed.Load(); got != -1 {
		t.Fatalf("usage cache after failed write = %d, want invalidated", got)
	}
	if err := d.filePut("next", ""); err != nil {
		t.Fatal(err)
	}
	if got := d.filesEntries; got != 3 {
		t.Fatalf("object count after retry = %d, want 3", got)
	}
}

func TestFilesZeroByteObjectQuotaAcrossRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seed := filepath.Join(dir, "files", "seed")
	if err := os.MkdirAll(seed, 0o700); err != nil {
		t.Fatal(err)
	}
	// Pre-existing empty files consume inodes but zero bytes. Count them
	// before the first put, including the seed directory.
	for i := range MaxFilesObjects - 2 {
		if err := os.WriteFile(filepath.Join(seed, fmt.Sprintf("%05d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	d := newDataStore(dir)
	if err := d.filePut("new", ""); err != nil {
		t.Fatal(err)
	}
	if err := d.filePut("other", ""); err == nil || !strings.Contains(err.Error(), "object quota") {
		t.Fatalf("empty put past quota: %v", err)
	}
	if removed, err := d.fileDelete("new"); err != nil || !removed {
		t.Fatalf("delete new: %v, %v", removed, err)
	}
	if err := d.filePut("deep/key", ""); err == nil || !strings.Contains(err.Error(), "object quota") {
		t.Fatalf("put requiring directory and file: %v", err)
	}
	if removed, err := d.fileDelete("seed/00000"); err != nil || !removed {
		t.Fatalf("delete seed: %v, %v", removed, err)
	}
	if err := d.filePut("deep/key", ""); err != nil {
		t.Fatal(err)
	}
	d.Close()
	restarted := newDataStore(dir)
	defer restarted.Close()
	if err := restarted.filePut("outside", ""); err == nil || !strings.Contains(err.Error(), "object quota") {
		t.Fatalf("quota bypass after restart: %v", err)
	}
	if removed, err := restarted.fileDelete("deep/key"); err != nil || !removed {
		t.Fatalf("delete deep: %v, %v", removed, err)
	}
	if err := restarted.filePut("outside", ""); err != nil {
		t.Fatalf("quota not released after deleting file and directory: %v", err)
	}
}
