package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet"
)

// sheetClock gives a service a clock the test can move forward.
func sheetClock(s *Service) *atomic.Int64 {
	var offset atomic.Int64
	s.now = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	return &offset
}

// unloadSheetNow moves the clock past the idle time until the sheet is
// unloaded. One step is not always enough: the flat lock may be taken, and
// an import that was still due counts as a use. The host's own pass may be
// the one that unloads the sheet.
func unloadSheetNow(t *testing.T, s *Service, offset *atomic.Int64, slugName string) {
	t.Helper()
	for deadline := time.Now().Add(sheetTestWait); ; time.Sleep(10 * time.Millisecond) {
		offset.Add(int64(sheetIdleUnload + time.Minute))
		if h := s.sheetHandle(slugName); h == nil || s.unloadSheet(h) || h.loaded() == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the idle sheet stayed loaded")
		}
	}
}

func awaitSheetIdle(t *testing.T, in *sheet.Instance) {
	t.Helper()
	deadline := time.Now().Add(sheetTestWait)
	for in.Stats().IdleSince.IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("the sheet never reported itself idle")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestSheetUnloadKeepsAFreshCursor covers a paged read of a sheet nobody
// used for the idle time: the unload pass that runs before the sheet notes
// the read on its own tick keeps the sheet, and the next page reads.
func TestSheetUnloadKeepsAFreshCursor(t *testing.T) {
	for _, rev := range []int{0, 2} {
		t.Run(fmt.Sprint("revision=", rev), func(t *testing.T) {
			ctx := context.Background()
			s, _ := newTestService(t)
			offset := sheetClock(s)
			in := newSheet(t, s, "budget")
			writeSheet(t, s, "budget", "rows", sheetBudgetRows)
			awaitSheetIdle(t, in)
			offset.Add(int64(sheetIdleUnload + time.Minute))

			first := sheetRead(t, s, "budget", rev, `{"tab":"Sheet1","limit":4}`)
			next, _ := first["next"].(map[string]any)
			if next == nil || len(first["rows"].([]any)) != 2 {
				t.Fatalf("first page = %+v", first)
			}
			// The host's own pass may have unloaded the idle sheet before
			// the read. The instance that issued the cursor is the one the
			// pass below must keep.
			in = s.liveSheet("budget")
			if in == nil {
				t.Fatal("no instance holds the cursor")
			}
			s.unloadIdleSheets()
			page, err := s.GetSheet(ctx, "budget", rev, []byte(fmt.Sprintf(`{"cursor":%q}`, next["cursor"])))
			if err != nil {
				t.Fatalf("the page after the unload pass: %v", err)
			}
			if s.liveSheet("budget") != in {
				t.Fatal("the unload pass stopped a sheet with a fresh cursor")
			}
			if got := string(page); !strings.Contains(got, `"start":"A3"`) {
				t.Fatalf("second page = %s", got)
			}

			// Once the cursor has expired and the idle time has passed since
			// its last page, the sheet unloads. The pass waits for the sheet
			// to note on its tick that it is idle again.
			deadline := time.Now().Add(sheetTestWait)
			for s.liveSheet("budget") != nil {
				if time.Now().After(deadline) {
					t.Fatal("the sheet stayed loaded after its cursor expired")
				}
				offset.Add(int64(sheetIdleUnload + time.Minute))
				s.unloadIdleSheets()
				time.Sleep(100 * time.Millisecond)
			}
			if got := sheetRead(t, s, "budget", rev, `{"tab":"Sheet1","range":"B2"}`)["rows"].([]any)[0].([]any)[0]; got != float64(1200) {
				t.Fatalf("B2 after the reload = %v", got)
			}
		})
	}
}

// TestSheetRequestsRaceTheIdleUnload reads a sheet through the agent path
// and the handler of the browser path while it is unloaded again and again:
// every request is served, by the instance it found or by a new one.
func TestSheetRequestsRaceTheIdleUnload(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	offset := sheetClock(s)
	newSheet(t, s, "budget")
	writeSheet(t, s, "budget", "rows", sheetBudgetRows)
	h := s.sheetHandle("budget")

	// A read of a sheet that is still loading is told to retry; a read may
	// never be told that its instance stopped.
	read := func(rev int, query string) error {
		for {
			_, err := s.GetSheet(ctx, "budget", rev, []byte(query))
			var problem *sheet.WriteError
			if errors.As(err, &problem) && problem.Reason == "calculating" {
				time.Sleep(time.Millisecond)
				continue
			}
			return err
		}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	var requests atomic.Int64
	fail := make(chan error, 16)
	for worker := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				var err error
				switch worker {
				case 0:
					err = read(0, `{"tab":"Sheet1","range":"A1:B4"}`)
				case 1:
					err = read(2, `{"tab":"Sheet1","range":"B2"}`)
				default:
					w := httptest.NewRecorder()
					h.ServeHTTP(w, httptest.NewRequest("GET", "http://budget.test/", nil))
					if w.Code != http.StatusOK {
						err = fmt.Errorf("page = %d %s", w.Code, w.Body.String())
					}
				}
				if err != nil {
					select {
					case fail <- err:
					default:
					}
					return
				}
				requests.Add(1)
				// A pause between requests leaves the unload its openings,
				// also on a machine where a request takes milliseconds.
				time.Sleep(time.Duration(worker+1) * 2 * time.Millisecond)
			}
		}()
	}
	unloads := 0
	// The race runs for 25 unloads or five seconds. A slow machine gets
	// longer to reach its first unload.
	for start := time.Now(); unloads < 25 && len(fail) == 0; {
		if elapsed := time.Since(start); elapsed > sheetTestWait || (unloads > 0 && elapsed > 5*time.Second) {
			break
		}
		// Every use so far is older than the idle time.
		offset.Add(int64(sheetIdleUnload + time.Minute))
		if s.unloadSheet(h) {
			unloads++
		}
		time.Sleep(time.Millisecond)
	}
	close(done)
	wg.Wait()
	select {
	case err := <-fail:
		t.Fatalf("a request that raced the unload failed: %v", err)
	default:
	}
	if unloads == 0 || requests.Load() == 0 {
		t.Fatalf("%d unloads raced %d requests", unloads, requests.Load())
	}
	t.Logf("%d unloads raced %d requests", unloads, requests.Load())
}

// TestSheetReadReopensAStoppedInstance covers a read that finds its
// instance stopped: it opens a new one and answers.
func TestSheetReadReopensAStoppedInstance(t *testing.T) {
	s, _ := newTestService(t)
	in := newSheet(t, s, "budget")
	writeSheet(t, s, "budget", "rows", sheetBudgetRows)
	in.Stop()
	if got := sheetRead(t, s, "budget", 0, `{"tab":"Sheet1","range":"B2"}`)["rows"].([]any)[0].([]any)[0]; got != float64(1200) {
		t.Fatalf("B2 from the new instance = %v", got)
	}
	if now := s.liveSheet("budget"); now == nil || now == in {
		t.Fatal("the read did not open a new instance")
	}
	if got := sheetRead(t, s, "budget", 2, `{"tab":"Sheet1","range":"B2"}`)["rows"].([]any)[0].([]any)[0]; got != float64(1200) {
		t.Fatalf("B2 of revision 2 = %v", got)
	}
}
