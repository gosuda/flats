package board

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// outboxMark reads the highest outbox seq board.db ever assigned.
func outboxMark(t *testing.T, dir string) int64 {
	t.Helper()
	u := url.URL{Scheme: "file", Path: filepath.Join(dir, dbName)}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var mark int64
	if err := db.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = 'outbox'`).Scan(&mark); err != nil {
		t.Fatal(err)
	}
	return mark
}

// TestOpenRefusesStoreBehindImportCursor pins the boundary: the cursor may
// reach the highest entry the store assigned, even after acknowledgement
// deleted it, and one past it is a store that lacks imported entries.
func TestOpenRefusesStoreBehindImportCursor(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "board")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	core := newFakeCore(filepath.Dir(dir))
	opts := Options{Slug: "plan", Title: "Plan", tickEvery: time.Hour}
	in, created, err := Open(dir, core, opts)
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	id := in.StoreID()
	if err := in.Acknowledge(t.Context(), 1, 0); err != nil {
		t.Fatal(err)
	}
	entries, err := in.Outbox(t.Context(), 0, 10)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outbox after acknowledgement = %d entries, %v", len(entries), err)
	}
	in.Stop()
	mark := outboxMark(t, dir)
	if mark != 1 {
		t.Fatalf("mark = %d, want 1", mark)
	}

	open := func(store string, seq int64) error {
		o := opts
		o.ImportStore, o.ImportSeq = store, seq
		in, _, err := Open(dir, core, o)
		if err == nil {
			in.Stop()
		}
		return err
	}
	for _, tc := range []struct {
		name  string
		store string
		seq   int64
	}{
		{"no cursor", "", 0},
		{"cursor at the mark", id, mark},
		{"another store", "other", mark + 100},
	} {
		if err := open(tc.store, tc.seq); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
	}

	for range 3 {
		err := open(id, mark+1)
		var be *Error
		if !errors.As(err, &be) || be.Category != CategoryUnavailable {
			t.Fatalf("refusal = %v", err)
		}
		for _, want := range []string{"board.db", fmt.Sprintf("entry %d", mark), fmt.Sprintf("through %d", mark+1)} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal %q lacks %q", err, want)
			}
		}
		if got := outboxMark(t, dir); got != mark {
			t.Fatalf("a refused open moved the mark from %d to %d", mark, got)
		}
	}
}
