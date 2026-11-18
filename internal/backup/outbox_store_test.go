package backup

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sqliteuri"
)

// storeDB writes a sheet database of the outbox tables whose store id is
// the SQL expression store.
func storeDB(t *testing.T, store string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sheet.db")
	db, err := sql.Open("sqlite", sqliteuri.File(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		`CREATE TABLE meta (singleton INTEGER PRIMARY KEY, store TEXT NOT NULL)`,
		`CREATE TABLE outbox (seq INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT)`,
		`INSERT INTO meta(singleton,store) VALUES(1,` + store + `)`,
		`INSERT INTO outbox(kind) VALUES('person')`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// The store id of an outbox database is read only when it is no longer
// than any store id: one of 8 MiB is refused, and the read allocates far
// less than the field. A store id of the writer's reads.
func TestOversizedStoreIDIsNotRead(t *testing.T) {
	const n = 8 << 20
	ctx := context.Background()
	st, err := readOutbox(ctx, storeDB(t, `'`+strings.Repeat("f", 32)+`'`), typeSheet)
	if err != nil || st.store != strings.Repeat("f", 32) || st.last != 1 {
		t.Fatalf("the store id of the writer: %+v, %v", st, err)
	}
	path := storeDB(t, fmt.Sprintf(`CAST(zeroblob(%d) AS TEXT)`, n))
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err = readOutbox(ctx, path, typeSheet)
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Fatal("a store id of 8 MiB was read")
	}
	if got := after.TotalAlloc - before.TotalAlloc; !raceEnabled && got > n/16 {
		t.Errorf("the refused read allocated %d KiB for a field of %d KiB", got>>10, n>>10)
	}
}
