package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet"
	"github.com/gosuda/flats/internal/store"
)

// A history read of a sheet whose load failed answers at once from what
// flats.db holds, with a request that does not end. The load refused
// sheet.db for good, so there is nothing to import and nothing to wait
// for. It refused it at its preflight (a number of 16 MiB in a cell), or
// while it opened the log a crash left (a log it cannot read). Meanwhile
// another sheet takes a write.
func TestSheetHistoryAfterAFailedLoad(t *testing.T) {
	ctx := context.Background()
	// listed lists the revisions of slugName, with a request that ends
	// only after sheetPassBound, while another sheet takes a write, and
	// returns how many it got.
	listed := func(t *testing.T, s *Service, slugName string) int {
		t.Helper()
		type answer struct {
			n   int
			err error
		}
		began := time.Now()
		got := make(chan answer, 1)
		go func() {
			bounded, cancel := context.WithTimeout(ctx, 2*sheetPassBound)
			defer cancel()
			page, err := s.ListRevisions(bounded, slugName, 0, 10)
			got <- answer{len(page.Revisions), err}
		}()
		inTime(t, "a write to another sheet", func() {
			writeSheet(t, s, "other", "other-write", `{"op":"write","tab":"Sheet1","at":"A1","rows":[[2]]}`)
		})
		select {
		case a := <-got:
			t.Logf("the list of revisions after the failed load took %v", time.Since(began))
			if a.err != nil {
				t.Fatalf("the list answered %v", a.err)
			}
			return a.n
		case <-time.After(sheetPassBound):
			t.Fatalf("the list of revisions after the failed load did not return in %v", sheetPassBound)
			return 0
		}
	}
	t.Run("preflight", func(t *testing.T) {
		s, _ := newTestService(t)
		newSheet(t, s, "damaged")
		newSheet(t, s, "other")
		writeSheet(t, s, "damaged", "one", `{"op":"write","tab":"Sheet1","at":"A1","rows":[[1]]}`)
		page, err := s.ListRevisions(ctx, "damaged", 0, 10)
		if err != nil || len(page.Revisions) < 2 {
			t.Fatalf("the revisions before the damage: %+v, %v", page, err)
		}
		unloadWith(t, s, "damaged", `UPDATE cells SET num=zeroblob(?)`, 16<<20)
		if n := listed(t, s, "damaged"); n != len(page.Revisions) {
			t.Fatalf("the list after the failed load has %d revisions, want %d", n, len(page.Revisions))
		}
	})
	t.Run("log", func(t *testing.T) {
		s, dir := newTestService(t)
		in := newSheet(t, s, "budget")
		newSheet(t, s, "other")
		state := sheetState(t, s, "budget")
		unlock := sync.OnceFunc(s.lock("budget"))
		defer unlock()
		directSheetWrite(t, in, state, "lost-import", sheetBudgetRows)
		rel, err := filepath.Rel(dir, s.sheetDB("budget"))
		if err != nil {
			t.Fatal(err)
		}
		image := crashImage(t, dir)
		unlock()
		wal := filepath.Join(image, rel+"-wal")
		if err := os.Remove(wal); err != nil {
			t.Fatalf("the crash left no log: %v", err)
		}
		if err := os.Mkdir(wal, 0o700); err != nil {
			t.Fatal(err)
		}
		restarted := restartFrom(t, image)
		if err := awaitLoad(ctx, restarted.liveSheet("budget")); err == nil {
			t.Fatal("the sheet with a log it cannot read loaded")
		}
		if n := listed(t, restarted, "budget"); n < 1 {
			t.Fatalf("the list after the failed load has %d revisions", n)
		}
	})
}

// A rename and a delete of a sheet whose load was stopped while SQLite
// recovered the log a crash left wait until the opener closed what it
// opened, since a handle it holds can keep the files from moving, and
// then succeed. With the opener held, neither returns; let go, both
// complete, and the renamed sheet opens with its write.
func TestSheetRenameAndDeleteWaitForARecovery(t *testing.T) {
	ctx := context.Background()
	for _, step := range []string{"rename", "delete"} {
		t.Run(step, func(t *testing.T) {
			s, dir := newTestService(t)
			in := newSheet(t, s, "budget")
			state := sheetState(t, s, "budget")
			unlock := sync.OnceFunc(s.lock("budget"))
			defer unlock()
			directSheetWrite(t, in, state, "kept", sheetBudgetRows)
			image := crashImage(t, dir)
			unlock()
			reached, release, err := sheet.HoldLogs()
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			restarted := restartFrom(t, image)
			select {
			case <-reached:
			case <-time.After(sheetTestWait):
				t.Fatal("the restarted sheet's load opened no log")
			}
			from := restarted.flatDir("budget")
			done := make(chan error, 1)
			go func() {
				var err error
				if step == "rename" {
					_, err = restarted.RenameSlug(ctx, "budget", "renamed", ViaConsole)
				} else {
					_, err = restarted.applyDelete(ctx, "budget", ViaConsole, "")
				}
				done <- err
			}()
			select {
			case err := <-done:
				t.Fatalf("the %s returned while the recovery held sheet.db: %v", step, err)
			case <-time.After(200 * time.Millisecond):
			}
			if _, err := os.Stat(from); err != nil {
				t.Fatalf("the %s moved the files while the recovery held sheet.db: %v", step, err)
			}
			release()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("the %s: %v", step, err)
				}
			case <-time.After(sheetTestWait):
				t.Fatalf("the %s did not return after the recovery", step)
			}
			if _, err := os.Stat(from); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the files are still at the old path: %v", err)
			}
			if step == "delete" {
				if _, err := restarted.st.GetFlat(ctx, "budget"); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("the deleted sheet: %v", err)
				}
				return
			}
			data := sheetRead(t, restarted, "renamed", 0, `{"tab":"Sheet1","range":"A4:B4"}`)
			if got := fmt.Sprint(data["rows"]); got != "[[Total map[formula:=SUM(B2:B3) value:2100]]]" {
				t.Fatalf("the renamed sheet reads %s, want the total of its write", got)
			}
		})
	}
}

// A history read that reaches a restarted sheet while its load is still
// opening the log a crash left, and finds that the load has not ended,
// imports the revision the load then makes readable before it answers,
// also when the load succeeds between the reads of its readiness.
func TestSheetHistoryImportsWhenTheLoadSucceedsMeanwhile(t *testing.T) {
	ctx := context.Background()
	s, dir := newTestService(t)
	in := newSheet(t, s, "budget")
	state := sheetState(t, s, "budget")
	unlock := sync.OnceFunc(s.lock("budget"))
	defer unlock()
	directSheetWrite(t, in, state, "pending-import", sheetBudgetRows)
	if got := storedRevisions(t, s, "budget"); len(got) != 1 {
		t.Fatalf("revisions before the restart = %v", got)
	}
	image := crashImage(t, dir)
	unlock()
	reached, release, err := sheet.HoldLogs()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	restarted := restartFrom(t, image)
	var loading *sheet.Instance
	select {
	case loading = <-reached:
	case <-time.After(sheetTestWait):
		t.Fatal("the restarted sheet's load opened no log")
	}
	held, letReads, err := sheet.HoldReadiness()
	if err != nil {
		t.Fatal(err)
	}
	defer letReads()
	type answer struct {
		n   int
		err error
	}
	got := make(chan answer, 1)
	go func() {
		page, err := restarted.ListRevisions(ctx, "budget", 0, 10)
		got <- answer{len(page.Revisions), err}
	}()
	select {
	case in := <-held:
		if in != loading {
			t.Fatal("another sheet's readiness was held")
		}
	case <-time.After(sheetTestWait):
		t.Fatal("the history read did not look at the load")
	}
	release()
	for deadline := time.Now().Add(sheetTestWait); !loading.Stats().Loaded; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the load did not end")
		}
	}
	letReads()
	select {
	case a := <-got:
		if a.err != nil || a.n != 2 {
			t.Fatalf("the history read answered %d revisions, %v; want the pending one imported", a.n, a.err)
		}
	case <-time.After(sheetTestWait):
		t.Fatal("the history read did not answer")
	}
}

// A rename or a delete that stops a sheet while another stop of it, here
// the host's, has taken the instance but not yet stopped it waits for
// that same stop and the release of its storage. The load is held where
// SQLite recovers the log a crash left, so the storage stays held until
// it is let go: until then the second caller has not returned and the
// files have not moved, whichever stop ran the instance's shutdown.
func TestSheetSecondStopWaitsForTheFirst(t *testing.T) {
	ctx := context.Background()
	for _, step := range []string{"rename", "delete"} {
		t.Run(step, func(t *testing.T) {
			s, dir := newTestService(t)
			in := newSheet(t, s, "budget")
			state := sheetState(t, s, "budget")
			unlock := sync.OnceFunc(s.lock("budget"))
			defer unlock()
			directSheetWrite(t, in, state, "kept", sheetBudgetRows)
			image := crashImage(t, dir)
			unlock()
			reached, release, err := sheet.HoldLogs()
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			restarted := restartFrom(t, image)
			select {
			case <-reached:
			case <-time.After(sheetTestWait):
				t.Fatal("the restarted sheet's load opened no log")
			}
			h := restarted.sheetHandle("budget")
			if h == nil {
				t.Fatal("the sheet has no handle")
			}
			// Only the first caller is held; sync.OnceFunc would hold the
			// second one too, until the first returns.
			took, letFirst := make(chan struct{}), make(chan struct{})
			var callers atomic.Int32
			h.mu.Lock()
			h.stopHold = func() {
				if callers.Add(1) == 1 {
					close(took)
					<-letFirst
				}
			}
			h.mu.Unlock()
			goFirst := sync.OnceFunc(func() { close(letFirst) })
			defer goFirst()
			first := make(chan struct{})
			go func() {
				h.Stop()
				close(first)
			}()
			<-took
			from := restarted.flatDir("budget")
			done := make(chan error, 1)
			go func() {
				var err error
				if step == "rename" {
					_, err = restarted.RenameSlug(ctx, "budget", "renamed", ViaConsole)
				} else {
					_, err = restarted.applyDelete(ctx, "budget", ViaConsole, "")
				}
				done <- err
			}()
			// The second caller stops the instance itself and files the
			// release of its storage, which the held opener keeps pending.
			for deadline := time.Now().Add(sheetTestWait); ; time.Sleep(time.Millisecond) {
				short, cancel := context.WithTimeout(ctx, time.Millisecond)
				err := sheet.AwaitReleased(short, restarted.sheetDir("budget"))
				cancel()
				if errors.Is(err, context.DeadlineExceeded) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("no release of the sheet's storage is pending: %v", err)
				}
			}
			select {
			case err := <-done:
				t.Fatalf("the %s returned while the recovery held sheet.db: %v", step, err)
			default:
			}
			if _, err := os.Stat(from); err != nil {
				t.Fatalf("the %s moved the files while the recovery held sheet.db: %v", step, err)
			}
			release()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("the %s: %v", step, err)
				}
			case <-time.After(sheetTestWait):
				t.Fatalf("the %s did not return after the recovery", step)
			}
			if _, err := os.Stat(from); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the files are still at the old path: %v", err)
			}
			goFirst()
			select {
			case <-first:
			case <-time.After(sheetTestWait):
				t.Fatal("the first stop did not return")
			}
		})
	}
}
