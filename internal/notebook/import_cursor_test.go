package notebook

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// outboxMark reads the highest outbox seq notebook.db ever assigned, 0 before
// its first entry.
func outboxMark(t *testing.T, dir string) int64 {
	t.Helper()
	s, _, err := openStore(filepath.Join(dir, dbName))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	var mark int64
	if err := s.db.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = 'outbox'`).Scan(&mark); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	return mark
}

func refusesBehind(t *testing.T, err error, mark int64) {
	t.Helper()
	if err == nil {
		t.Fatal("a store behind the cursor opened")
	}
	for _, want := range []string{"notebook.db", fmt.Sprintf("entry %d", mark), fmt.Sprintf("through %d", mark+1)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}
}

// TestOpenRefusesStoreBehindImportCursor pins the boundary: the cursor may
// reach the highest entry the store assigned, even after acknowledgement
// deleted it, and one past it is a store that lacks imported entries.
func TestOpenRefusesStoreBehindImportCursor(t *testing.T) {
	dir := t.TempDir()
	b := &writerBackend{}
	opts := Options{Slug: "notes", Title: "First"}
	in, created, err := Open(dir, b, opts)
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
		in, _, err := Open(dir, b, o)
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
		refusesBehind(t, open(id, mark+1), mark)
		if got := outboxMark(t, dir); got != mark {
			t.Fatalf("a refused open moved the mark from %d to %d", mark, got)
		}
	}
}

// TestOpenChecksTheCursorBeforeRecordingCreated covers a store copied between
// its schema commit and its created entry. Recording that entry first would
// raise the mark to the cursor and let the store through.
func TestOpenChecksTheCursorBeforeRecordingCreated(t *testing.T) {
	dir := t.TempDir()
	s, fresh, err := openStore(filepath.Join(dir, dbName))
	if err != nil || !fresh {
		t.Fatalf("openStore: %v %v", fresh, err)
	}
	state, err := s.state(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	_, _, err = Open(dir, &writerBackend{}, Options{Slug: "notes", Title: "First", ImportStore: state.Store, ImportSeq: 1})
	refusesBehind(t, err, 0)
	if got := outboxMark(t, dir); got != 0 {
		t.Fatalf("a refused open recorded an entry: mark %d", got)
	}
}
