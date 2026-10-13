package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bypassPragmas are spellings of limit-changing pragmas checkSQL must refuse.
var bypassPragmas = []string{
	"PRAGMA max_page_count = 1000000000",
	`PRAGMA "MAX_PAGE_COUNT" = 1000000000`,
	"PRAGMA main.max_page_count(1000000000)",
	`pragma "main" . [max_page_count]=1`,
	"PRAGMA /* comment */ page_size = 65536",
	"PRAGMA journal_mode = DELETE",
	"PRAGMA `journal_mode`=MEMORY",
	"SELECT 1; PRAGMA journal_mode = DELETE",
	"PRAGMA temp_store = FILE",
	`PRAGMA "Temp_Store" = 1`,
	"PRAGMA temp_store_directory",
	"PRAGMA data_store_directory = '/tmp'",
	"-- note\nPRAGMA\tmain.temp_store\n=\n1",
	"PRAGMA 'max_page_count' = 1000000000",
	"PRAGMA 'main'.'max_page_count'(1000000000)",
	"PRAGMA main.'page_size' = 65536",
	"PRAGMA 'temp_store' = FILE",
	"PRAGMA 'temp_store_directory' = '/tmp'",
	"PRAGMA 'journal_mode' = 'delete'",
	"\uFEFFPRAGMA max_page_count = 1000000000",
	"PRAGMA \uFEFFmax_page_count = 1000000000",
	"SELECT 1;\uFEFFPRAGMA \uFEFF'temp_store' = FILE",
}

func TestCheckSQLPragmas(t *testing.T) {
	t.Parallel()
	for _, q := range bypassPragmas {
		if err := checkSQL(q); err == nil {
			t.Errorf("checkSQL(%q) allowed a storage pragma", q)
		}
	}
	for _, q := range []string{
		"PRAGMA max_page_count",
		"PRAGMA main.page_size",
		"PRAGMA journal_mode",
		"PRAGMA temp_store;",
		"PRAGMA page_count",
		"PRAGMA wal_checkpoint(TRUNCATE)",
		"PRAGMA foreign_keys = ON",
		"CREATE TABLE settings (page_size INTEGER, journal_mode TEXT, temp_store TEXT)",
		"UPDATE settings SET page_size = 50, journal_mode = 'x'",
		"INSERT INTO notes VALUES ('PRAGMA page_size = 1')",
		`SELECT "pragma" AS max_page_count FROM t -- PRAGMA page_size = 1`,
		"SELECT * FROM pragma_page_size()",
		"PRAGMA 'page_size'",
		"SELECT 'pragma', 'page_size', '=', 1",
	} {
		if err := checkSQL(q); err != nil {
			t.Errorf("checkSQL(%q) = %v; want allowed", q, err)
		}
	}
}

func TestSQLitePageBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	d := newDataStore(dir)
	d.maxDBBytes = 64 * 4096
	c, err := d.conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var pageSize, maxPages int64
	var mode string
	if err := c.QueryRowContext(ctx, "PRAGMA main.page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if err := c.QueryRowContext(ctx, "PRAGMA main.max_page_count").Scan(&maxPages); err != nil || maxPages != d.maxDBBytes/pageSize {
		t.Fatalf("max_page_count = %d, %v; want %d", maxPages, err, d.maxDBBytes/pageSize)
	}
	if err := c.QueryRowContext(ctx, "PRAGMA main.journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v; want wal", mode, err)
	}
	if got, err := dbQuery(ctx, c, "PRAGMA temp_store", nil); err != nil || got != `[{"temp_store":2}]` {
		t.Fatalf("read temp_store: %s, %v", got, err)
	}
	// Run the bypass spellings through SQLite itself: whatever checkSQL
	// misses must not change the limits.
	for _, q := range bypassPragmas {
		dbExec(ctx, c, q, nil)
		dbQuery(ctx, c, q, nil)
	}
	var tempStore int64
	if err := c.QueryRowContext(ctx, "PRAGMA main.max_page_count").Scan(&maxPages); err != nil || maxPages != d.maxDBBytes/pageSize {
		t.Fatalf("max_page_count after bypass attempts = %d, %v", maxPages, err)
	}
	if err := c.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode after bypass attempts = %q, %v", mode, err)
	}
	if err := c.QueryRowContext(ctx, "PRAGMA temp_store").Scan(&tempStore); err != nil || tempStore != 2 {
		t.Fatalf("temp_store after bypass attempts = %d, %v", tempStore, err)
	}
	if _, err := dbExec(ctx, c, "CREATE TABLE settings (page_size INTEGER)", nil); err != nil {
		t.Fatalf("column named page_size: %v", err)
	}
	if _, err := dbExec(ctx, c, "CREATE TABLE entries (value BLOB)", nil); err != nil {
		t.Fatal(err)
	}
	_, err = dbExec(ctx, c, "INSERT INTO entries VALUES (zeroblob(1048576))", nil)
	if err == nil || !strings.Contains(err.Error(), "database is full") {
		t.Fatalf("growth past the budget: %v", err)
	}
	var count int64
	if err := c.QueryRowContext(ctx, "PRAGMA main.page_count").Scan(&count); err != nil || count > maxPages {
		t.Fatalf("page count = %d, %v; want at most %d", count, err, maxPages)
	}
	if _, err := dbExec(ctx, c, "INSERT INTO entries VALUES (zeroblob(1000))", nil); err != nil {
		t.Fatalf("small insert within the budget: %v", err)
	}
	c.Close()
	d.Close()

	// The limit is per connection, so it must hold again after a restart.
	restarted := newDataStore(dir)
	restarted.maxDBBytes = d.maxDBBytes
	defer restarted.Close()
	c, err = restarted.conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.QueryRowContext(ctx, "PRAGMA main.max_page_count").Scan(&maxPages); err != nil || maxPages != restarted.maxDBBytes/pageSize {
		t.Fatalf("restarted page budget = %d, %v", maxPages, err)
	}
}

// A database that grew before the budget existed keeps working: it can be
// read, rows can be deleted and the freed pages reused, but it cannot grow.
func TestSQLiteOverBudgetDatabaseStaysUsable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	raw, err := sql.Open("sqlite", filepath.Join(dir, "db.sqlite")+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("CREATE TABLE big (id INTEGER PRIMARY KEY, value BLOB)"); err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		if _, err := raw.Exec("INSERT INTO big VALUES (?, zeroblob(131072))", i); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	d := newDataStore(dir)
	d.maxDBBytes = 64 * 4096 // a quarter of the existing 1 MiB
	defer d.Close()
	c, err := d.conn(ctx)
	if err != nil {
		t.Fatalf("over-budget database refused: %v", err)
	}
	defer c.Close()
	if got, err := dbQuery(ctx, c, "SELECT count(*) AS n FROM big", nil); err != nil || got != `[{"n":8}]` {
		t.Fatalf("read over-budget database: %s, %v", got, err)
	}
	if _, err := dbExec(ctx, c, "INSERT INTO big VALUES (100, zeroblob(1048576))", nil); err == nil || !strings.Contains(err.Error(), "database is full") {
		t.Fatalf("over-budget database grew: %v", err)
	}
	if _, err := dbExec(ctx, c, "DELETE FROM big WHERE id < 2", nil); err != nil {
		t.Fatalf("delete from over-budget database: %v", err)
	}
	if _, err := dbExec(ctx, c, "INSERT INTO big VALUES (200, zeroblob(131072))", nil); err != nil {
		t.Fatalf("reuse freed pages: %v", err)
	}
}

// A reader holding an old snapshot keeps the write-ahead log from
// restarting. Commits are refused once the log passes the budget, and a
// refused commit truncates the log once the reader is gone.
func TestSQLiteWALBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	d := newDataStore(dir)
	d.maxDBBytes = 256 * 4096
	defer d.Close()
	writer, err := d.conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	reader, err := d.conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := dbExec(ctx, writer, "CREATE TABLE t (id INTEGER PRIMARY KEY, value BLOB)", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := dbExec(ctx, writer, "INSERT INTO t VALUES (1, zeroblob(65536))", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := dbExec(ctx, reader, "BEGIN", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := dbQuery(ctx, reader, "SELECT count(*) AS n FROM t", nil); err != nil {
		t.Fatal(err)
	}
	wal := filepath.Join(dir, "db.sqlite-wal")
	refused := false
	for i := range 200 {
		_, err := dbExec(ctx, writer, "UPDATE t SET value = randomblob(65536) WHERE id = 1", nil)
		if err != nil {
			if !strings.Contains(err.Error(), "write-ahead log") {
				t.Fatalf("update %d: %v", i, err)
			}
			refused = true
			break
		}
	}
	if !refused {
		t.Fatal("write-ahead log grew without limit under a long-lived reader")
	}
	if info, err := os.Stat(wal); err != nil || info.Size() > 2*d.maxDBBytes {
		t.Fatalf("write-ahead log size = %v, %v; want at most %d", info.Size(), err, 2*d.maxDBBytes)
	}
	if _, err := dbExec(ctx, reader, "COMMIT", nil); err != nil {
		t.Fatal(err)
	}
	// The first refusal after the reader ends checkpoints and truncates.
	if _, err := dbExec(ctx, writer, "UPDATE t SET value = zeroblob(10) WHERE id = 1", nil); err != nil {
		if !strings.Contains(err.Error(), "write-ahead log") {
			t.Fatal(err)
		}
		if _, err := dbExec(ctx, writer, "UPDATE t SET value = zeroblob(10) WHERE id = 1", nil); err != nil {
			t.Fatalf("write after checkpoint: %v", err)
		}
	}
	if info, err := os.Stat(wal); err != nil || info.Size() > d.maxDBBytes {
		t.Fatalf("write-ahead log after recovery = %v, %v", info.Size(), err)
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
	if got := d.filesEntries.Load(); got != 4 {
		t.Fatalf("file and directory count = %d, want 4", got)
	}
	if removed, err := d.fileDelete("a/b/empty"); err != nil || !removed {
		t.Fatalf("delete: %v, %v", removed, err)
	}
	if got := d.filesEntries.Load(); got != 3 {
		t.Fatalf("count after delete = %d, want 3", got)
	}
	if removed, err := d.fileDelete("a/b/other"); err != nil || !removed {
		t.Fatalf("delete: %v, %v", removed, err)
	}
	if got := d.filesEntries.Load(); got != 0 {
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
	if got := d.filesEntries.Load(); got != 2 {
		t.Fatalf("object count after retry = %d, want 2", got)
	}
}

func TestFilesZeroByteObjectQuotaAcrossRestart(t *testing.T) {
	t.Parallel()
	const limit = 12
	dir := t.TempDir()
	seed := filepath.Join(dir, "files", "seed")
	if err := os.MkdirAll(seed, 0o700); err != nil {
		t.Fatal(err)
	}
	// Pre-existing empty files consume inodes but zero bytes. Count them
	// before the first put, including the seed directory.
	for i := range limit - 2 {
		if err := os.WriteFile(filepath.Join(seed, fmt.Sprintf("%05d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	d := newDataStore(dir)
	d.maxFilesObjects = limit
	if err := d.filePut("new", ""); err != nil {
		t.Fatal(err)
	}
	if err := d.filePut("other", ""); err == nil || !strings.Contains(err.Error(), "object quota") {
		t.Fatalf("empty put past quota: %v", err)
	}
	// Overwriting existing keys adds no objects and keeps working when full.
	if err := d.filePut("new", "replaced"); err != nil {
		t.Fatalf("overwrite at the object limit: %v", err)
	}
	if err := d.filePut("seed/00000", "replaced"); err != nil {
		t.Fatalf("overwrite in a subdirectory at the object limit: %v", err)
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
	restarted.maxFilesObjects = limit
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

// Stores that already exceed the object limit (written before it existed)
// keep reading, overwriting and deleting; only new objects are refused.
func TestFilesOverObjectLimitStoreStaysUsable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for i := range 10 {
		p := filepath.Join(dir, "files", fmt.Sprintf("k%d", i))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("v"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	d := newDataStore(dir)
	d.maxFilesObjects = 5
	defer d.Close()
	if v, err := d.fileGet("k1"); err != nil || v == nil || *v != "v" {
		t.Fatalf("get: %v, %v", v, err)
	}
	if err := d.filePut("k1", "updated"); err != nil {
		t.Fatalf("overwrite above the limit: %v", err)
	}
	if err := d.filePut("k99", ""); err == nil || !strings.Contains(err.Error(), "object quota") {
		t.Fatalf("new object above the limit: %v", err)
	}
	if removed, err := d.fileDelete("k2"); err != nil || !removed {
		t.Fatalf("delete above the limit: %v, %v", removed, err)
	}
}
