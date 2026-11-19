package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet"
	"github.com/gosuda/flats/internal/store"
)

// storeMark reads the highest outbox seq a board.db, notebook.db or sheet.db
// ever assigned.
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
	}, {
		name: "sheet",
		slug: "budget",
		rel:  filepath.Join("flats", "budget", "sheet", "sheet.db"),
		create: func(t *testing.T, s *Service) {
			newSheet(t, s, "budget")
		},
		write: func(s *Service, n int) error {
			ctx := context.Background()
			data, err := s.GetSheet(ctx, "budget", 0, []byte(`{}`))
			if err != nil {
				return err
			}
			var read struct {
				State string `json:"state"`
			}
			if err := json.Unmarshal(data, &read); err != nil {
				return err
			}
			body := fmt.Sprintf(`{"state":%q,"op_id":"older-store-%d","ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[[%d]]}]}`, read.State, n, n, n)
			_, err = s.UpdateSheet(ctx, "budget", []byte(body), ViaMCP)
			return err
		},
		live: func(s *Service) bool { return s.sheetHandle("budget") != nil },
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
// earlier moment than flats.db, without a -wal file. The flat stays down with
// its reason in the log, a sibling starts, nothing is written to the store,
// more restarts change nothing, and the matching store brings the flat back
// with no revision lost.
func TestStoreBehindCursorDoesNotStart(t *testing.T) {
	ctx := context.Background()
	for _, tc := range olderStoreCases() {
		t.Run(tc.name, func(t *testing.T) {
			older, current, top := olderStoreImages(t, tc)
			image := crashImage(t, current)
			swapStore(t, older, image, tc.rel)
			mark := storeMark(t, filepath.Join(image, tc.rel))
			// Reading the mark checkpointed the log into the store, so the
			// open refuses it, and the failed build records the event.
			if _, err := os.Stat(filepath.Join(image, tc.rel+"-wal")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the store has a -wal file at the restart: %v", err)
			}

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
			if !strings.HasPrefix(events[0].Message, "could not start live version after restart: ") {
				t.Errorf("event %q is not the failed build", events[0].Message)
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

// sheetStoreCase is the sheet of olderStoreCases.
func sheetStoreCase(t *testing.T) olderStoreCase {
	t.Helper()
	for _, c := range olderStoreCases() {
		if c.name == "sheet" {
			return c
		}
	}
	t.Fatal("no sheet case")
	return olderStoreCase{}
}

// longLog is the length past which the writer truncates the log of a sheet
// after its commits and a stop leaves the log as it is (4 MiB).
const longLog = 4 << 20

// fileLength is the length of the file at path, 0 when there is none.
func fileLength(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// loggedStoreImages is olderStoreImages for the sheet, whose older image
// keeps the -wal file a crash leaves. With long, a reader holds the log
// while long texts are written, as the snapshot of a backup does, so that no
// commit truncates it, and the log of the older image is longer than
// longLog.
func loggedStoreImages(t *testing.T, tc olderStoreCase, long bool) (older, current string, top int) {
	t.Helper()
	ctx := context.Background()
	s, dir := newTestService(t)
	tc.create(t, s)
	for n := 1; n <= 2; n++ {
		if err := tc.write(s, n); err != nil {
			t.Fatal(err)
		}
	}
	db := filepath.Join(dir, tc.rel)
	if long {
		reader, err := sql.Open("sqlite", db)
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		tx, err := reader.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM meta`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		row := `["` + strings.Repeat("a", 32000) + `"],`
		rows := strings.TrimSuffix(strings.Repeat(row, 40), ",")
		for i := 0; fileLength(t, db+"-wal") <= longLog; i++ {
			body := fmt.Sprintf(`{"state":%q,"op_id":"long-%d","ops":[{"op":"write","tab":"Sheet1","at":"C1","rows":[%s]}]}`, sheetState(t, s, tc.slug), i, rows)
			if _, err := s.UpdateSheet(ctx, tc.slug, []byte(body), ViaMCP); err != nil {
				t.Fatal(err)
			}
		}
		older = crashImage(t, dir)
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	} else {
		older = crashImage(t, dir)
	}
	for n := 3; n <= 5; n++ {
		if err := tc.write(s, n); err != nil {
			t.Fatal(err)
		}
	}
	current = crashImage(t, dir)
	return older, current, storedRevisions(t, s, tc.slug)[0]
}

// logMark is storeMark of the store at path and its -wal file, read from a
// copy, so that the store keeps its log.
func logMark(t *testing.T, path string) int64 {
	t.Helper()
	copied := filepath.Join(t.TempDir(), filepath.Base(path))
	for _, suffix := range []string{"", "-wal"} {
		data, err := os.ReadFile(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(copied+suffix, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return storeMark(t, copied)
}

// storeHashes are the hashes of the store at path and of its -wal file.
func storeHashes(t *testing.T, path string) [2][sha256.Size]byte {
	t.Helper()
	var out [2][sha256.Size]byte
	for i, suffix := range []string{"", "-wal"} {
		data, err := os.ReadFile(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = sha256.Sum256(data)
	}
	return out
}

// TestStoreBehindCursorWithALog restarts a host whose sheet.db is behind the
// cursor with the -wal file a crash left, shorter and longer than the log a
// stop checkpoints. Open does not read a log, so the flat starts, and the
// load of the sheet refuses the store: the flat gets one restore event with
// the reason, every read and write is refused, and sheet.db and its -wal
// keep their bytes through the refusal and the stop of the sheet.
func TestStoreBehindCursorWithALog(t *testing.T) {
	ctx := context.Background()
	tc := sheetStoreCase(t)
	for _, long := range []bool{false, true} {
		t.Run(fmt.Sprintf("long log %v", long), func(t *testing.T) {
			older, current, top := loggedStoreImages(t, tc, long)
			image := crashImage(t, current)
			swapStore(t, older, image, tc.rel)
			db := filepath.Join(image, tc.rel)
			if n := fileLength(t, db+"-wal"); n == 0 || n > longLog != long {
				t.Fatalf("the store has a log of %d bytes", n)
			}
			mark := logMark(t, db)
			before := storeHashes(t, db)

			r := restartFrom(t, image)
			if !tc.live(r) {
				t.Fatal("the flat of a store with a log did not start")
			}
			var events []store.Event
			for deadline := time.Now().Add(sheetTestWait); len(events) == 0; time.Sleep(10 * time.Millisecond) {
				var err error
				if events, err = r.st.ListEvents(ctx, tc.slug, "restore", 0, 10); err != nil {
					t.Fatal(err)
				}
				if time.Now().After(deadline) {
					t.Fatal("the refused load recorded no restore event")
				}
			}
			st, err := r.st.ContentState(ctx, tc.slug)
			if err != nil {
				t.Fatal(err)
			}
			message := events[0].Message
			if events[0].Level != "error" || !strings.HasPrefix(message, "could not load the sheet: sheet.db is behind flats.db") {
				t.Errorf("the event is %s %q", events[0].Level, message)
			}
			for _, want := range []string{fmt.Sprintf("entry %d", mark), fmt.Sprintf("through %d", st.ImportSeq)} {
				if !strings.Contains(message, want) {
					t.Errorf("event %q lacks %q", message, want)
				}
			}
			for _, body := range []string{
				`{"state":"s_000000000000.1","ops":[{"op":"write","tab":"Sheet1","at":"A6","rows":[[6]]}]}`,
				`{"state":"s_000000000000.1","op_id":"refused-write","ops":[{"op":"write","tab":"Sheet1","at":"A6","rows":[[6]]}]}`,
			} {
				if _, err := r.UpdateSheet(ctx, tc.slug, []byte(body), ViaMCP); !errors.Is(err, ErrUnavailable) || refusalReason(err) != "load" {
					t.Errorf("a write to the refused sheet: %v", err)
				}
			}
			if _, err := r.GetSheet(ctx, tc.slug, 0, []byte(`{}`)); !errors.Is(err, ErrUnavailable) || refusalReason(err) != "load" {
				t.Errorf("a read of the refused sheet: %v", err)
			}
			if got := storeHashes(t, db); got != before {
				t.Fatal("the refusal changed sheet.db or its log")
			}
			r.sheetHandle(tc.slug).Stop()
			if got := storeHashes(t, db); got != before {
				t.Fatal("the stop of the refused sheet changed sheet.db or its log")
			}
			if got := storedRevisions(t, r, tc.slug); got[0] != top {
				t.Fatalf("revisions = %v, want newest %d", got, top)
			}
			if events, err := r.st.ListEvents(ctx, tc.slug, "restore", 0, 10); err != nil || len(events) != 1 {
				t.Fatalf("restore events = %v, %v", events, err)
			}
		})
	}
}

// refusalReason is the reason of the sheet refusal err carries.
func refusalReason(err error) string {
	var problem *sheet.WriteError
	if errors.As(err, &problem) {
		return problem.Reason
	}
	return ""
}

// TestStoreBehindCursorRefusesAnUnloadedSheet puts a sheet.db behind the
// cursor, without a -wal file, under a sheet that the idle time unloaded.
// The next request opens the sheet again, the open refuses the store, and
// the request gets the unavailable error with the reason. The flat records
// no event for it.
func TestStoreBehindCursorRefusesAnUnloadedSheet(t *testing.T) {
	ctx := context.Background()
	tc := sheetStoreCase(t)
	s, _ := newTestService(t)
	offset := sheetClock(s)
	tc.create(t, s)
	for n := 1; n <= 2; n++ {
		if err := tc.write(s, n); err != nil {
			t.Fatal(err)
		}
	}
	db := s.sheetDB(tc.slug)
	unloaded := func() {
		t.Helper()
		unloadSheetNow(t, s, offset, tc.slug)
		if s.liveSheet(tc.slug) != nil {
			t.Fatal("the sheet is loaded")
		}
		if _, err := os.Stat(db + "-wal"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the unloaded sheet has a -wal file: %v", err)
		}
	}
	unloaded()
	older, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	for n := 3; n <= 5; n++ {
		if err := tc.write(s, n); err != nil {
			t.Fatal(err)
		}
	}
	unloaded()
	if err := os.WriteFile(db, older, 0o600); err != nil {
		t.Fatal(err)
	}
	for n := 6; n <= 7; n++ {
		if err := tc.write(s, n); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "open the sheet: sheet.db is behind flats.db") {
			t.Fatalf("a request to the unloaded sheet: %v", err)
		}
	}
	if s.liveSheet(tc.slug) != nil {
		t.Fatal("the refused store is loaded")
	}
	if events, err := s.st.ListEvents(ctx, tc.slug, "restore", 0, 10); err != nil || len(events) != 0 {
		t.Fatalf("restore events = %v, %v", events, err)
	}
	if got, err := os.ReadFile(db); err != nil || !bytes.Equal(got, older) {
		t.Fatalf("the refused open changed sheet.db: %v", err)
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
