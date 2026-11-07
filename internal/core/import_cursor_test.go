package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// storeMark reads the highest outbox seq a board.db or notebook.db ever
// assigned.
func storeMark(t *testing.T, path string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", path)
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

// swapStore puts the store at rel of image from into image to, with the -wal
// it had and no file of the store it replaces.
func swapStore(t *testing.T, from, to, rel string) {
	t.Helper()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(filepath.Join(to, rel+suffix)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(from, rel+suffix))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(to, rel+suffix), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// olderStoreCase is one store type under test.
type olderStoreCase struct {
	name   string
	slug   string
	rel    string
	create func(t *testing.T, s *Service)
	write  func(s *Service, n int) error
	live   func(s *Service) bool
}

func olderStoreCases() []olderStoreCase {
	return []olderStoreCase{{
		name: "board",
		slug: "plan",
		rel:  filepath.Join("flats", "plan", "board", "board.db"),
		create: func(t *testing.T, s *Service) {
			newBoard(t, s, "plan")
		},
		write: func(s *Service, n int) error {
			ops := fmt.Sprintf(`[{"op":"add","el":{"type":"sticky","text":"note %d"}}]`, n)
			u := BoardUpdate{OpID: fmt.Sprintf("older-store-%d", n), Ops: boardOpsRaw(ops)}
			_, err := s.UpdateBoard(context.Background(), "plan", u, ViaMCP)
			return err
		},
		live: func(s *Service) bool { return s.liveBoard("plan") != nil },
	}, {
		name: "notebook",
		slug: "notes",
		rel:  filepath.Join("flats", "notes", "notebook", "notebook.db"),
		create: func(t *testing.T, s *Service) {
			newNotebook(t, s, "notes")
		},
		write: func(s *Service, n int) error {
			op := fmt.Sprintf(`{"op":"create_page","ref":"page","parent":null,"before":null,"title":"Page %d"}`, n)
			_, err := s.UpdateNotebook(context.Background(), "notes", notebookPayload(fmt.Sprintf("older-store-%d", n), op), ViaMCP)
			return err
		},
		live: func(s *Service) bool { return s.liveNotebook("notes") != nil },
	}}
}

// olderStoreImages builds a host with two imported writes, then three more:
// older holds the state after the first two and current the final state.
func olderStoreImages(t *testing.T, tc olderStoreCase) (older, current string, top int) {
	t.Helper()
	s, dir := newTestService(t)
	tc.create(t, s)
	newBoard(t, s, "other")
	for n := 1; n <= 2; n++ {
		if err := tc.write(s, n); err != nil {
			t.Fatal(err)
		}
	}
	older = crashImage(t, dir)
	for n := 3; n <= 5; n++ {
		if err := tc.write(s, n); err != nil {
			t.Fatal(err)
		}
	}
	current = crashImage(t, dir)
	return older, current, storedRevisions(t, s, tc.slug)[0]
}

// TestStoreBehindCursorDoesNotStart restarts a host with one store from an
// earlier moment than flats.db. The flat stays down with its reason in the log,
// a sibling starts, nothing is written to the store, more restarts change
// nothing, and the matching store brings the flat back with no revision lost.
func TestStoreBehindCursorDoesNotStart(t *testing.T) {
	ctx := context.Background()
	for _, tc := range olderStoreCases() {
		t.Run(tc.name, func(t *testing.T) {
			older, current, top := olderStoreImages(t, tc)
			image := crashImage(t, current)
			swapStore(t, older, image, tc.rel)
			mark := storeMark(t, filepath.Join(image, tc.rel))

			r := restartFrom(t, image)
			if tc.live(r) {
				t.Fatal("a store behind the cursor started")
			}
			if r.liveBoard("other") == nil {
				t.Fatal("the sibling board did not start")
			}
			events, err := r.st.ListEvents(ctx, tc.slug, "restore", 0, 10)
			if err != nil || len(events) != 1 {
				t.Fatalf("restore events = %v, %v", events, err)
			}
			st, err := r.st.ContentState(ctx, tc.slug)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"behind flats.db", fmt.Sprintf("entry %d", mark), fmt.Sprintf("through %d", st.ImportSeq)} {
				if !strings.Contains(events[0].Message, want) {
					t.Errorf("event %q lacks %q", events[0].Message, want)
				}
			}
			if err := tc.write(r, 6); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "not running") {
				t.Fatalf("write on the refused flat: %v", err)
			}
			if got := storeMark(t, filepath.Join(image, tc.rel)); got != mark {
				t.Fatalf("the refused flat moved the mark from %d to %d", mark, got)
			}
			if got := storedRevisions(t, r, tc.slug); got[0] != top {
				t.Fatalf("revisions = %v, want newest %d", got, top)
			}

			again := crashImage(t, image)
			r2 := restartFrom(t, again)
			if tc.live(r2) {
				t.Fatal("a second restart started the store")
			}
			if got := storeMark(t, filepath.Join(again, tc.rel)); got != mark {
				t.Fatalf("a second restart moved the mark from %d to %d", mark, got)
			}

			restored := crashImage(t, again)
			swapStore(t, current, restored, tc.rel)
			r3 := restartFrom(t, restored)
			if !tc.live(r3) {
				t.Fatal("the matching store did not start")
			}
			if err := tc.write(r3, 7); err != nil {
				t.Fatal(err)
			}
			if got := storedRevisions(t, r3, tc.slug); got[0] != top+1 {
				t.Fatalf("revisions after the restore = %v, want newest %d", got, top+1)
			}
		})
	}
}

// TestStoreBehindCursorDuringInit covers a notebook whose creation stopped in
// phase init: writes reach it through a second open path, which must refuse as
// well, or each write would raise the mark until the store passes the check.
func TestStoreBehindCursorDuringInit(t *testing.T) {
	var tc olderStoreCase
	for _, c := range olderStoreCases() {
		if c.name == "notebook" {
			tc = c
		}
	}
	older, current, _ := olderStoreImages(t, tc)
	image := crashImage(t, current)
	swapStore(t, older, image, tc.rel)
	boardExecDB(t, filepath.Join(image, "flats.db"), `UPDATE content_state SET phase='init' WHERE flat='notes'`)
	mark := storeMark(t, filepath.Join(image, tc.rel))

	r := restartFrom(t, image)
	if err := tc.write(r, 6); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "behind flats.db") {
		t.Fatalf("write in phase init: %v", err)
	}
	if got := storeMark(t, filepath.Join(image, tc.rel)); got != mark {
		t.Fatalf("the refused write moved the mark from %d to %d", mark, got)
	}
}

// TestFlatWithoutContentStateStillStarts keeps the behaviour for a store whose
// flat has no content state: there is no cursor to compare.
func TestFlatWithoutContentStateStillStarts(t *testing.T) {
	for _, tc := range olderStoreCases() {
		t.Run(tc.name, func(t *testing.T) {
			_, current, _ := olderStoreImages(t, tc)
			image := crashImage(t, current)
			boardExecDB(t, filepath.Join(image, "flats.db"), fmt.Sprintf(`DELETE FROM content_state WHERE flat=%q`, tc.slug))
			r := restartFrom(t, image)
			if !tc.live(r) {
				t.Fatal("the flat did not start")
			}
		})
	}
}
