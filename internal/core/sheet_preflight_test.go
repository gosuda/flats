package core

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet"
	"github.com/gosuda/flats/internal/sqliteuri"
	"github.com/gosuda/flats/internal/store"
)

// unloadWith drops the loaded instance of a sheet, runs change on its
// sheet.db, and returns the handle, whose next use opens the sheet again.
func unloadWith(t *testing.T, s *Service, slugName string, change string, args ...any) *sheetInstance {
	t.Helper()
	h := s.sheetHandle(slugName)
	if h == nil {
		t.Fatalf("the sheet %s is not running", slugName)
	}
	if in := h.loaded(); in != nil {
		h.discard(in)
	}
	db, err := sql.Open("sqlite", sqliteuri.File(filepath.Join(s.sheetDir(slugName), "sheet.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(change, args...); err != nil {
		t.Fatal(err)
	}
	return h
}

// bigCells adds rows of cells enough that the check of sheet.db in a load
// lasts far longer than the steps of a test between the open and its hold.
const bigCells = `WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i<100000)
	INSERT INTO cells(tab,row,col,kind,num,str,seq) SELECT 1,1000+i/20,1000+i%20,1,NULL,printf('%.200c','x'),1 FROM n`

// Opening a sheet leaves the check of its sheet.db to the load: the open
// of a sheet whose cell holds a number of 16 MiB returns its instance, and
// the sheet is unavailable once its load refuses it. A rename, a delete
// and a close of the host while that check is held at a check point, as a
// check of a large file would be, return without waiting for it.
func TestSheetOpenLeavesTheCheckToTheLoad(t *testing.T) {
	ctx := context.Background()
	t.Run("damaged", func(t *testing.T) {
		s, _ := sheetPassService(t)
		newSheet(t, s, "damaged")
		writeSheet(t, s, "damaged", "one", `{"op":"write","tab":"Sheet1","at":"A1","rows":[[1]]}`)
		h := unloadWith(t, s, "damaged", `UPDATE cells SET num=zeroblob(?)`, 16<<20)
		in, release, err := h.acquire()
		if err != nil {
			t.Fatalf("the open waited for the check of sheet.db: %v", err)
		}
		defer release()
		err = errors.Unwrap(awaitLoad(ctx, in))
		if err == nil || !strings.Contains(err.Error(), "holds a row of") {
			t.Fatalf("the load answered %v", err)
		}
	})
	for _, step := range []string{"rename", "delete", "close"} {
		t.Run(step, func(t *testing.T) {
			s, stop := sheetPassService(t)
			newSheet(t, s, "big")
			h := unloadWith(t, s, "big", bigCells)
			in, release, err := h.acquire()
			if err != nil {
				t.Fatal(err)
			}
			held := holdSheetPass(t, in)
			held()
			release()
			inTime(t, "the "+step+" during the check of sheet.db", func() {
				var err error
				switch step {
				case "rename":
					_, err = s.RenameSlug(ctx, "big", "renamed", ViaConsole)
				case "delete":
					_, err = s.applyDelete(ctx, "big", ViaConsole, "")
				default:
					stop()
				}
				if err != nil {
					t.Errorf("the %s: %v", step, err)
				}
			})
			if step == "delete" {
				if _, err := s.st.GetFlat(ctx, "big"); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("the deleted sheet: %v", err)
				}
			}
		})
	}
}

// awaitLoad waits for the load of in and returns the cause of its failure.
func awaitLoad(ctx context.Context, in *sheet.Instance) error {
	ctx, cancel := context.WithTimeout(ctx, sheetTestWait)
	defer cancel()
	_, err := in.Read(ctx, sheet.ReadQuery{})
	for sheetCalculating(err) && ctx.Err() == nil {
		_, err = in.Read(ctx, sheet.ReadQuery{})
	}
	return err
}
