package sheet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// takeSlots takes every slot of the gate with passes that never look at
// it, as long passes without a check point would, and returns the function
// that gives them back.
func takeSlots(t *testing.T, slots int) (release func()) {
	t.Helper()
	turns := make([]*calcTurn, slots)
	for i := range turns {
		turn, ok := calcSlots.enter(nil, context.Background())
		if !ok {
			t.Fatal("no slot for the test")
		}
		turns[i] = turn
	}
	gateSettles(t, "with every slot taken", 0, 0)
	release = sync.OnceFunc(func() {
		for _, turn := range turns {
			turn.leave()
		}
	})
	t.Cleanup(release)
	return release
}

// A sheet.db is checked by its load at the gate, not by the open: with
// every slot taken, opening a sheet and one whose cell holds a number
// column of 16 MiB returns at once, their preflights wait in line, a read
// outside the writer waits for the preflight and is refused as
// calculating, and none of their check points is reached. Once the slots
// are free the sheet loads, and the other is refused by its preflight. It
// runs with a gate of one slot and with several.
func TestOpenLeavesThePreflightToTheGate(t *testing.T) {
	ctx := context.Background()
	for _, slots := range gateSizes {
		t.Run(fmt.Sprintf("%d slots", slots), func(t *testing.T) {
			gateSlots(t, slots)
			good := start(t)
			good.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,"=A1+1"]]}]}`)
			good.in.Stop()
			damaged := start(t)
			damaged.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1]]}]}`)
			damaged.in.Stop()
			execDB(t, damaged, func(db *sql.DB) error {
				_, err := db.Exec(`UPDATE cells SET num=zeroblob(?) WHERE row IS NOT NULL`, 16<<20)
				return err
			})
			w := watchLoads(t)
			release := takeSlots(t, slots)
			for _, e := range []*env{good, damaged} {
				in, _, err := Open(e.dir, e.b, e.opts)
				if err != nil {
					t.Fatalf("the open waited for the check of sheet.db: %v", err)
				}
				e.in, e.b.in = in, in
			}
			gateSettles(t, "with both loads in line", 0, 2)
			for _, e := range []*env{good, damaged} {
				if _, err := e.in.Outbox(ctx, 0, 1); refusal(err).Reason != "calculating" {
					t.Fatalf("the outbox was read before its preflight: %v", err)
				}
				select {
				case <-e.in.usable:
					t.Fatal("sheet.db was checked without a slot")
				default:
				}
				for _, phase := range []string{"preflight", "integrity", "records"} {
					if n := w.count(e.in, phase); n != 0 {
						t.Fatalf("the load reached %d check points of the %s without a slot", n, phase)
					}
				}
			}
			release()
			if err := good.in.await(ctx, 0); err != nil {
				t.Fatal(err)
			}
			good.want("B1", "2")
			if w.count(good.in, "integrity") == 0 {
				t.Fatal("the load reached no check point of its preflight")
			}
			err := damaged.in.await(ctx, 0)
			if err == nil || refusal(err).cause == nil || !strings.Contains(refusal(err).cause.Error(), "holds a row of") {
				t.Fatalf("the damaged sheet's load answered %v", err)
			}
			if _, err := damaged.in.Outbox(ctx, 0, 1); refusal(err).Reason != "load" {
				t.Fatalf("the outbox of a sheet whose preflight failed answered %v", err)
			}
			good.in.Stop()
			damaged.in.Stop()
			gateSettles(t, "after the loads", slots, 0)
		})
	}
}

// A load takes turns at the gate while it checks the classes of the
// numbers of sheet.db: with every slot taken by loads held there, two
// small loads get a slot from them, and a stop ends them there, before
// their quick_check and before they read any table. It runs with a gate
// of one slot and with several.
func TestPreflightTakesTurnsAndStops(t *testing.T) {
	for _, slots := range gateSizes {
		t.Run(fmt.Sprintf("%d slots", slots), func(t *testing.T) {
			gateSlots(t, slots)
			long := make([]*env, slots)
			for i := range long {
				long[i] = recordsSheet(t)
			}
			w := watchLoads(t)
			w.holdAt("preflight")
			for _, e := range long {
				in, _, err := Open(e.dir, e.b, e.opts)
				if err != nil {
					t.Fatal(err)
				}
				e.in, e.b.in = in, in
			}
			handOverAndStop(t, w, slots, "preflight", []string{"integrity", "records", "axes", "styles", "cells", "files"}, long)
		})
	}
}

// bigSheet returns a stopped sheet whose sheet.db holds rows of cells
// enough that its quick_check takes long, and that time.
func bigSheet(t *testing.T) (*env, time.Duration) {
	t.Helper()
	rows := 300_000
	if raceEnabled {
		rows = 60_000
	}
	e := start(t, unhurried, func(o *Options) { o.Slug = "big" })
	e.in.Stop()
	execDB(t, e, func(db *sql.DB) error {
		_, err := db.Exec(`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i<?)
			INSERT INTO cells(tab,row,col,kind,num,str,seq) SELECT 1,1000+i/20,1000+i%20,1,NULL,printf('%.200c','x'),1 FROM n`, rows-1)
		return err
	})
	db := readOnly(t, e)
	began := time.Now()
	var integrity string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("quick_check of the big sheet: %q, %v", integrity, err)
	}
	return e, time.Since(began)
}

// A stop while a load runs the quick_check of its preflight interrupts
// that statement: the preflight ends with the stop and never passes, and
// the stop and the load end long before a whole quick_check of the file
// would. It runs with a gate of one slot and with several.
func TestStopInterruptsTheQuickCheck(t *testing.T) {
	big, whole := bigSheet(t)
	t.Logf("a whole quick_check takes %v", whole)
	for _, slots := range gateSizes {
		t.Run(fmt.Sprintf("%d slots", slots), func(t *testing.T) {
			gateSlots(t, slots)
			dir := t.TempDir()
			copyFile(t, filepath.Join(big.dir, dbName), filepath.Join(dir, dbName))
			e := &env{t: t, dir: dir, b: newBackend(), clock: big.clock, opts: big.opts}
			var (
				mu      sync.Mutex
				target  *Instance
				checked time.Time
				ended   error
			)
			reached, done := make(chan struct{}), make(chan struct{})
			see := func(in *Instance, phase string) {
				mu.Lock()
				defer mu.Unlock()
				if in == target && phase == "integrity" && checked.IsZero() {
					checked = time.Now()
					close(reached)
				}
			}
			end := func(in *Instance, err error) {
				mu.Lock()
				defer mu.Unlock()
				if in == target {
					ended = err
					close(done)
				}
			}
			if !loadPoints.CompareAndSwap(nil, &see) {
				t.Fatal("another test watches loads")
			}
			defer loadPoints.Store(nil)
			preflightEnded.Store(&end)
			defer preflightEnded.Store(nil)
			// The load waits in line until the instance is known.
			release := takeSlots(t, slots)
			in, _, err := Open(e.dir, e.b, e.opts)
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			target = in
			mu.Unlock()
			release()
			<-reached
			// The quick_check starts right after its check point.
			time.Sleep(whole / 20)
			began := time.Now()
			in.Stop()
			stopped := time.Since(began)
			<-done
			mu.Lock()
			since, err := time.Since(checked), ended
			mu.Unlock()
			t.Logf("the stop took %v, %v after the check point: %v", stopped, since, err)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("the preflight ended with %v, not with the stop", err)
			}
			select {
			case <-in.usable:
				t.Fatal("the quick_check finished before the stop")
			default:
			}
			if since > whole/2 {
				t.Errorf("the stop ended the load %v after its quick_check began, which takes %v whole", since, whole)
			}
			gateSettles(t, "after the stop", slots, 0)
		})
	}
}

// A sheet.db before its first commit is checked in its open, which then
// writes the first commit. Such a file is a schema and a row of meta,
// well within newSheetPages, and one of more pages is refused before its
// preflight.
func TestNewSheetIsCheckedInItsOpen(t *testing.T) {
	dir := t.TempDir()
	db, fresh, _, err := openDB(filepath.Join(dir, dbName))
	if err != nil || !fresh {
		t.Fatal(fresh, err)
	}
	var pages int64
	if err := db.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	db.Close()
	t.Logf("a new sheet.db has %d pages", pages)
	if pages > newSheetPages/2 {
		t.Fatalf("a new sheet.db has %d pages, more than half of the %d an unwritten one may have", pages, newSheetPages)
	}
	e := start(t)
	select {
	case <-e.in.usable:
	default:
		t.Fatal("a new sheet was not checked in its open")
	}
	e.in.Stop()
	execDB(t, e, func(db *sql.DB) error {
		for _, table := range []string{"cells", "tabs", "styles", "cols", "files", "outbox", "snapshots", "sessions", "receipts", "commands", "groups", "sqlite_sequence"} {
			if _, err := db.Exec(`DELETE FROM ` + table); err != nil {
				return err
			}
		}
		// Free pages count as pages of the file.
		if _, err := db.Exec(`INSERT INTO snapshots(hash,data,refs) VALUES('x',zeroblob(?),0)`, (newSheetPages+1)<<16); err != nil {
			return err
		}
		if _, err := db.Exec(`DELETE FROM snapshots`); err != nil {
			return err
		}
		_, err := db.Exec(`UPDATE meta SET seq=0`)
		return err
	})
	in, _, err := Open(e.dir, e.b, e.opts)
	if err == nil {
		in.Stop()
		t.Fatal("an unwritten sheet.db of many pages opened")
	}
	if !strings.Contains(err.Error(), "pages before its first commit") {
		t.Fatalf("the open refused with %v", err)
	}
}
