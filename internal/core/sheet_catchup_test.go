package core

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet"
)

// A history read after a crash restart imports the write the crash left
// unimported, also while the sheet's load still opens its sheet.db: with
// the load held where SQLite recovers the log the crash left, a list of
// revisions waits without the flat lock, a cancelled one returns its
// context's error, and once the load goes on the waiting list includes the
// revision of the write.
func TestSheetCatchUpWaitsForTheLoad(t *testing.T) {
	ctx := context.Background()
	s, dir := newTestService(t)
	in := newSheet(t, s, "budget")
	state := sheetState(t, s, "budget")
	unlock := sync.OnceFunc(s.lock("budget"))
	defer unlock()
	directSheetWrite(t, in, state, "lost-import", sheetBudgetRows)
	image := crashImage(t, dir)
	unlock()

	reached, release, err := sheet.HoldLogs()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	s = restartFrom(t, image)
	select {
	case <-reached:
	case <-time.After(sheetTestWait):
		t.Fatal("the restarted sheet's load opened no log")
	}

	cancelled, cancel := context.WithCancel(ctx)
	gone := make(chan error, 1)
	go func() {
		_, err := s.ListRevisions(cancelled, "budget", 0, 10)
		gone <- err
	}()
	type listed struct {
		kind string
		n    int
		err  error
	}
	waiting := make(chan listed, 1)
	go func() {
		page, err := s.ListRevisions(ctx, "budget", 0, 10)
		r := listed{n: len(page.Revisions), err: err}
		if len(page.Revisions) > 0 {
			r.kind = page.Revisions[0].Kind
		}
		waiting <- r
	}()
	inTime(t, "taking the flat lock while the lists wait", func() {
		lctx, done := context.WithTimeout(ctx, sheetPassBound)
		defer done()
		unlockFlat, err := s.lockStorageCtx(lctx, "budget")
		if err != nil {
			t.Errorf("the flat lock: %v", err)
			return
		}
		unlockFlat()
	})
	cancel()
	select {
	case err := <-gone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the cancelled list answered %v", err)
		}
	case <-time.After(sheetPassBound):
		t.Fatal("the cancelled list did not return")
	}
	select {
	case r := <-waiting:
		t.Fatalf("the list answered while the load was held: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case r := <-waiting:
		if r.err != nil || r.n != 2 || r.kind != "agent" {
			t.Fatalf("the list after the load: %+v", r)
		}
	case <-time.After(sheetTestWait):
		t.Fatal("the list did not return after the load")
	}
}
