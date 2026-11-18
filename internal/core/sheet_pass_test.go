package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet"
	"github.com/gosuda/flats/internal/store"
)

// sheetPassBound is what a call of the host may take while a sheet
// calculates for minutes. None of them waits for the calculation, so the
// bound is room for a busy machine.
const sheetPassBound = 10 * time.Second

const slowSheetFormulas, slowSheetTexts = 30, 20

// sheetPassService is newTestService for a test that closes the service
// itself. stop closes it once.
func sheetPassService(t *testing.T) (s *Service, stop func()) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	s, err = New(context.Background(), Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	stop = sync.OnceFunc(func() { s.Close() })
	t.Cleanup(func() { stop(); st.Close() })
	return s, stop
}

// newSlowSheet creates a sheet whose calculation from the day after
// tomorrow on is long real work: formulas that each match a wildcard
// pattern against 20 texts of 32,000 characters, so a pass reaches a check
// point in its first formula. Until that day every formula is 0 and costs
// nothing. The tests hold such a pass there (holdSheetPass), as a pass of
// minutes would be.
func newSlowSheet(t *testing.T, s *Service, slugName string) *sheet.Instance {
	t.Helper()
	in := newSheet(t, s, slugName)
	writeSheet(t, s, slugName, "rows", fmt.Sprintf(`{"op":"insert_rows","tab":"Sheet1","at":1,"count":%d}`, slowSheetFormulas))
	text := `["` + strings.Repeat("a", 32000) + `"],`
	writeSheet(t, s, slugName, "texts", `{"op":"write","tab":"Sheet1","at":"A1","rows":[`+strings.TrimSuffix(strings.Repeat(text, slowSheetTexts), ",")+`]}`)
	y, m, d := s.now().In(time.Local).AddDate(0, 0, 1).Date()
	formula := fmt.Sprintf(`["=IF(TODAY()>DATE(%d,%d,%d),COUNTIF(A$1:A$%d,\"*a?b*\"),0)"],`, y, int(m), d, slowSheetTexts)
	writeSheet(t, s, slugName, "formulas", `{"op":"write","tab":"Sheet1","at":"B1","rows":[`+strings.TrimSuffix(strings.Repeat(formula, slowSheetFormulas), ",")+`]}`)
	return in
}

// inTime runs a call of the host and fails when it takes longer than
// sheetPassBound.
func inTime(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	began := time.Now()
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		t.Logf("%s took %v", what, time.Since(began))
	case <-time.After(sheetPassBound):
		t.Fatalf("%s did not return in %v while a sheet calculated", what, sheetPassBound)
	}
}

// holdSheetPass keeps the next pass of in that reaches a check point there
// for the rest of the test, while every other pass goes on. The held pass
// still hands its slot on and ends with its stop or its request. The
// returned function waits until the pass is held.
func holdSheetPass(t *testing.T, in *sheet.Instance) (held func()) {
	t.Helper()
	reached, release, err := sheet.HoldPass(in)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return func() {
		t.Helper()
		select {
		case <-reached:
		case <-time.After(sheetTestWait):
			t.Fatal("the sheet's pass reached no check point")
		}
	}
}

// holdSheetRollback holds, as holdSheetPass does, the pass of the rollback
// of the sheet slugName that the test starts next. It holds the instance
// that is loaded now and keeps it loaded, as a user of the sheet, until
// that pass is held. So the host's idle unload, which may run on any of
// its ticks, cannot replace the instance before the rollback takes it.
// The returned function waits until the pass is held and lets the sheet
// go; the rollback keeps it loaded from then on.
func holdSheetRollback(t *testing.T, s *Service, slugName string) (in *sheet.Instance, held func()) {
	t.Helper()
	h := s.sheetHandle(slugName)
	if h == nil {
		t.Fatalf("the sheet %s is not running", slugName)
	}
	in, done, err := h.acquire()
	if err != nil {
		t.Fatal(err)
	}
	let := sync.OnceFunc(done)
	t.Cleanup(let)
	reached := holdSheetPass(t, in)
	return in, func() {
		t.Helper()
		reached()
		if s.liveSheet(slugName) != in {
			t.Fatal("the held pass is not of the loaded instance of the sheet")
		}
		let()
	}
}

// heavySheetDate moves the clock past the date the formulas of the slow
// sheet wait for and reads the sheet, which changes its date. With reload
// the host's idle unload replaces the instance before the read, as the
// host's own pass may on any tick after the clock moved.
func heavySheetDate(t *testing.T, s *Service, offset *atomic.Int64, reload bool) {
	t.Helper()
	offset.Add(int64(48 * time.Hour))
	if reload {
		unloadSheetNow(t, s, offset, "slow")
	}
	sheetRead(t, s, "slow", 0, `{"tab":"Sheet1","range":"B1"}`)
}

// sheetReloads are the cases of the rollback tests: the instance that
// wrote the sheet calculates the rollback, or one the host loaded after
// an idle unload.
var sheetReloads = []struct {
	name   string
	reload bool
}{{"loaded", false}, {"reloaded", true}}

// TestSheetDateChangeLeavesTheHostFree changes the date of a sheet whose
// recalculation is held at a check point, as minutes of work would be,
// while that sheet and another one hold revisions the host has not imported. The host's passes return
// without waiting for the calculation, the other sheet's revision is
// imported, a read of the calculating sheet is refused as calculating, and
// the host closes while the calculation is still at work.
func TestSheetDateChangeLeavesTheHostFree(t *testing.T) {
	ctx := context.Background()
	s, stop := sheetPassService(t)
	offset := sheetClock(s)
	slow := newSlowSheet(t, s, "slow")
	other := newSheet(t, s, "other")
	// The flat locks keep the host's own tick from importing the two
	// writes before the calculation runs.
	unlock := sync.OnceFunc(func() {
		for _, name := range []string{"slow", "other"} {
			if m, ok := s.locks.Load(name); ok {
				m.(*sync.Mutex).Unlock()
			}
		}
	})
	s.lock("slow")
	s.lock("other")
	defer unlock()
	directSheetWrite(t, slow, sheetState(t, s, "slow"), "pending-slow", `{"op":"write","tab":"Sheet1","at":"D1","rows":[["pending"]]}`)
	directSheetWrite(t, other, sheetState(t, s, "other"), "pending-other", sheetBudgetRows)
	before, beforeSlow := storedRevisions(t, s, "other"), storedRevisions(t, s, "slow")

	offset.Add(int64(48 * time.Hour))
	read := func(what string) {
		t.Helper()
		inTime(t, what, func() {
			if _, err := s.GetSheet(ctx, "slow", 0, []byte(`{"tab":"Sheet1","range":"B1"}`)); !sheetCalculating(err) {
				t.Errorf("%s: %v, want the refusal calculating", what, err)
			}
		})
	}
	held := holdSheetPass(t, slow)
	read("the read that meets the new date")
	held()
	unlock()

	inTime(t, "a tick of the sheets", func() { s.TickSheets(ctx) })
	// A request records the revision the calculating sheet holds, and
	// leaves the acknowledgement for the time after the calculation.
	inTime(t, "the list of the calculating sheet's revisions", func() {
		page, err := s.ListRevisions(ctx, "slow", 0, 100)
		if err != nil || len(page.Revisions) != len(beforeSlow)+1 {
			t.Errorf("the revisions of the calculating sheet: %d, %v, want %d", len(page.Revisions), err, len(beforeSlow)+1)
		}
	})
	if entries, err := slow.Outbox(ctx, 0, 10); err != nil || len(entries) != 1 {
		t.Fatalf("the calculating sheet's outbox holds %d entries, %v, want the one it could not be told of", len(entries), err)
	}
	inTime(t, "the periodic blob sweep", func() { s.SweepDesignBlobs(ctx) })
	for deadline := time.Now().Add(sheetPassBound); len(storedRevisions(t, s, "other")) != len(before)+1; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the other sheet's revision was not imported while the first calculated: %v", storedRevisions(t, s, "other"))
		}
		s.TickSheets(ctx)
	}
	inTime(t, "a write to the calculating sheet", func() {
		_, err := s.UpdateSheet(ctx, "slow", []byte(`{"ops":[{"op":"write","tab":"Sheet1","at":"E1","rows":[["late"]]}]}`), ViaMCP)
		if !sheetCalculating(err) {
			t.Errorf("a write during the calculation: %v, want the refusal calculating", err)
		}
	})
	read("a later read")
	if !slow.Stats().Calculating {
		t.Fatal("the held calculation ended")
	}
	inTime(t, "closing the host", stop)
}

// TestSheetRollbackLeavesTheHostFree rolls a sheet back to a revision
// whose calculation at the current date is held at a check point, as
// minutes of work would be. The host's passes do not wait for it, the live sheet and its History stay
// readable, a write is refused as calculating, and the host closes while
// the rollback is still at work, which ends it.
func TestSheetRollbackLeavesTheHostFree(t *testing.T) {
	for _, c := range sheetReloads {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			s, stop := sheetPassService(t)
			offset := sheetClock(s)
			first := newSlowSheet(t, s, "slow")
			revs := storedRevisions(t, s, "slow")
			heavy := slices.Max(revs)
			// Without its formulas the live sheet changes its date in no time.
			writeSheet(t, s, "slow", "light", fmt.Sprintf(`{"op":"clear","tab":"Sheet1","ranges":["B1:B%d"]}`, slowSheetFormulas))
			heavySheetDate(t, s, offset, c.reload)

			slow, held := holdSheetRollback(t, s, "slow")
			if c.reload && slow == first {
				t.Fatal("the idle unload left the first instance loaded")
			}
			rolled := make(chan error, 1)
			go func() {
				_, err := s.RollbackRevision(ctx, "slow", heavy, "Ann", ViaConsole)
				rolled <- err
			}()
			held()
			inTime(t, "a tick of the sheets", func() { s.TickSheets(ctx) })
			inTime(t, "the periodic blob sweep", func() { s.SweepDesignBlobs(ctx) })
			inTime(t, "a read of the live sheet", func() {
				// The cleared formulas left column A as the tab's used range.
				data, err := s.GetSheet(ctx, "slow", 0, []byte(`{"tab":"Sheet1","range":"A1:B1"}`))
				if err != nil || !strings.Contains(string(data), `"range":"A1","start":"A1","rows":[["aaaa`) {
					t.Errorf("a read during the rollback: %v, %.100s, want the live row without its formula", err, data)
				}
			})
			inTime(t, "a write during the rollback", func() {
				_, err := s.UpdateSheet(ctx, "slow", []byte(`{"ops":[{"op":"write","tab":"Sheet1","at":"E1","rows":[["late"]]}]}`), ViaMCP)
				if !sheetCalculating(err) {
					t.Errorf("a write during the rollback: %v, want the refusal calculating", err)
				}
			})
			// History comes from the host's store, which the rollback leaves alone
			// until it has applied.
			inTime(t, "the list of revisions during the rollback", func() {
				if page, err := s.ListRevisions(ctx, "slow", 0, 10); err != nil || len(page.Revisions) != len(revs)+1 {
					t.Errorf("the list of revisions during the rollback: %d, %v, want %d", len(page.Revisions), err, len(revs)+1)
				}
			})
			inTime(t, "a read of a revision during the rollback", func() {
				// The revision closed before the date its formulas wait for. Its
				// view calculates at the gate beside the rollback, and the read is
				// refused as calculating until it has.
				data, err := s.GetSheet(ctx, "slow", heavy, []byte(`{"tab":"Sheet1","range":"B1"}`))
				for deadline := time.Now().Add(sheetPassBound / 2); sheetCalculating(err) && time.Now().Before(deadline); {
					time.Sleep(10 * time.Millisecond)
					data, err = s.GetSheet(ctx, "slow", heavy, []byte(`{"tab":"Sheet1","range":"B1"}`))
				}
				if err != nil || !strings.Contains(string(data), `,"value":0}]]`) {
					t.Errorf("a read of a revision during the rollback: %v, %.200s, want its formula's value 0", err, data)
				}
			})
			if !slow.Stats().Calculating {
				t.Fatal("the held rollback ended")
			}
			inTime(t, "closing the host", stop)
			select {
			case err := <-rolled:
				if err == nil {
					t.Fatal("the rollback reported success after the host stopped it")
				}
			case <-time.After(sheetPassBound):
				t.Fatal("the rollback did not end with the host")
			}
		})
	}
}

// TestSheetRenameAndDeleteEndARollback renames a sheet, and deletes one,
// while it rolls back to a revision whose calculation at the current date
// is held at a check point, as minutes of work would be. Neither waits for
// the calculation. Each stops the sheet, which ends the
// rollback, and the rollback reports that it was not applied. The renamed
// sheet reads as it was before the rollback, and the deleted one is gone.
func TestSheetRenameAndDeleteEndARollback(t *testing.T) {
	for _, step := range []string{"rename", "delete"} {
		for _, c := range sheetReloads {
			t.Run(step+"-"+c.name, func(t *testing.T) {
				ctx := context.Background()
				s, _ := sheetPassService(t)
				offset := sheetClock(s)
				first := newSlowSheet(t, s, "slow")
				heavy := slices.Max(storedRevisions(t, s, "slow"))
				writeSheet(t, s, "slow", "light", fmt.Sprintf(`{"op":"clear","tab":"Sheet1","ranges":["B1:B%d"]}`, slowSheetFormulas))
				heavySheetDate(t, s, offset, c.reload)

				slow, held := holdSheetRollback(t, s, "slow")
				if c.reload && slow == first {
					t.Fatal("the idle unload left the first instance loaded")
				}
				rolled := make(chan error, 1)
				go func() {
					_, err := s.RollbackRevision(ctx, "slow", heavy, "Ann", ViaConsole)
					rolled <- err
				}()
				held()
				inTime(t, "the "+step+" during the rollback", func() {
					var err error
					if step == "rename" {
						_, err = s.RenameSlug(ctx, "slow", "renamed", ViaConsole)
					} else {
						_, err = s.applyDelete(ctx, "slow", ViaConsole, "")
					}
					if err != nil {
						t.Errorf("the %s during the rollback: %v", step, err)
					}
				})
				select {
				case err := <-rolled:
					if err == nil {
						t.Fatalf("the rollback reported success after the %s stopped it", step)
					}
				case <-time.After(sheetPassBound):
					t.Fatalf("the rollback did not end with the %s", step)
				}
				if step == "delete" {
					if _, err := s.st.GetFlat(ctx, "slow"); !errors.Is(err, store.ErrNotFound) {
						t.Fatalf("the deleted sheet: %v", err)
					}
					return
				}
				data := sheetRead(t, s, "renamed", 0, `{"tab":"Sheet1","range":"A1:B1"}`)
				if got := fmt.Sprint(data["rows"]); !strings.HasPrefix(got, "[[aaaa") || strings.Contains(got, "COUNTIF") {
					t.Fatalf("the renamed sheet reads %.80s, want the live row without its formula", got)
				}
			})
		}
	}
}

// TestSheetRollbackLeavesItsFlatFree rolls a sheet back to a revision whose
// calculation at the current date is held at a check point, as minutes of
// work would be. Meanwhile the
// host's passes over every flat return and also serve the flat after the
// sheet, a provider permission and a visibility approval of the sheet get
// their answer, and an import that a request starts keeps the rollback's
// hold. Then the request is cancelled, which ends the rollback: its hold
// is dropped, the sheet reads as before and takes the next write.
func TestSheetRollbackLeavesItsFlatFree(t *testing.T) {
	for _, c := range sheetReloads {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			s, _ := sheetPassService(t)
			offset := sheetClock(s)
			first := newSlowSheet(t, s, "slow")
			// ListFlats returns the flats by slug, so the loops reach this one
			// after the sheet.
			newSheet(t, s, "zeta")
			heavy := slices.Max(storedRevisions(t, s, "slow"))
			writeSheet(t, s, "slow", "light", fmt.Sprintf(`{"op":"clear","tab":"Sheet1","ranges":["B1:B%d"]}`, slowSheetFormulas))
			heavySheetDate(t, s, offset, c.reload)
			network := newLifecycleRouteNet()
			s.cfg.Lifecycle = network
			for _, name := range []string{"slow", "zeta"} {
				if _, err := s.st.SetProviderPermission(ctx, name, store.ProviderTailscale, true, store.Audit{}); err != nil {
					t.Fatal(err)
				}
			}

			rctx, cancel := context.WithCancel(ctx)
			defer cancel()
			slow, held := holdSheetRollback(t, s, "slow")
			if c.reload && slow == first {
				t.Fatal("the idle unload left the first instance loaded")
			}
			rolled := make(chan error, 1)
			go func() {
				_, err := s.RollbackRevision(rctx, "slow", heavy, "Ann", ViaConsole)
				rolled <- err
			}()
			held()
			inTime(t, "the host's relay listing", func() { s.applyHostListing(ctx) })
			inTime(t, "the retry of provider routes", func() { s.RetryProviderRoutes(ctx, ProviderTailscale, ProviderPortal) })
			for _, name := range []string{"slow", "zeta"} {
				if !network.serves(name, ProviderTailscale) {
					t.Errorf("the retry of provider routes did not serve %s", name)
				}
			}
			inTime(t, "a provider permission of the sheet", func() {
				if err := s.SetProviderPermission(ctx, "slow", store.ProviderPortal, true, ViaConsole); err != nil {
					t.Errorf("a provider permission during the rollback: %v", err)
				}
			})
			inTime(t, "a visibility approval of the sheet", func() {
				req, err := s.SetVisibility(ctx, "slow", store.Public, ViaAPI, "")
				if err != nil || req.Approval == nil {
					t.Errorf("a visibility request during the rollback: %+v, %v", req, err)
					return
				}
				if _, err := s.Decide(ctx, req.Approval.ID, true); err != nil {
					t.Errorf("a visibility approval during the rollback: %v", err)
				}
			})
			if f, err := s.st.GetFlat(ctx, "slow"); err != nil || f.Visibility != store.Public {
				t.Errorf("the approved visibility: %+v, %v, want Public", f.Visibility, err)
			}
			inTime(t, "the list of revisions", func() {
				if _, err := s.ListRevisions(ctx, "slow", 0, 10); err != nil {
					t.Errorf("the list of revisions during the rollback: %v", err)
				}
			})
			if holds, err := s.st.Holds(ctx, "slow"); err != nil || len(holds) != 1 || holds[0].Number != heavy {
				t.Fatalf("an import during the rollback left the holds %+v, %v, want the rollback's", holds, err)
			}
			if !slow.Stats().Calculating {
				t.Fatal("the held rollback ended")
			}

			cancel()
			select {
			case err := <-rolled:
				if err == nil {
					t.Fatal("the rollback reported success after its request ended")
				}
			case <-time.After(sheetPassBound):
				t.Fatal("the rollback did not end with its request")
			}
			if holds, err := s.st.Holds(ctx, "slow"); err != nil || len(holds) != 0 {
				t.Fatalf("the ended rollback left the holds %+v, %v", holds, err)
			}
			data := sheetRead(t, s, "slow", 0, `{"tab":"Sheet1","range":"A1:B1"}`)
			if got := fmt.Sprint(data["rows"]); !strings.HasPrefix(got, "[[aaaa") || strings.Contains(got, "COUNTIF") {
				t.Fatalf("after the ended rollback the sheet reads %.80s, want the live row without its formula", got)
			}
			writeSheet(t, s, "slow", "after", `{"op":"write","tab":"Sheet1","at":"E1","rows":[["after"]]}`)
		})
	}
}

// TestSheetRollbackFindsItsRevisionImportedByAnother rolls a sheet back
// and holds the rollback once it has calculated without the flat lock,
// before it takes the lock back for its own import. Meanwhile the test
// imports the rollback's entry, as the host's tick or another request can
// in that window; the tick may have imported it first. The rollback then
// finds nothing left to import and reports the revision the other import
// recorded.
func TestSheetRollbackFindsItsRevisionImportedByAnother(t *testing.T) {
	ctx := context.Background()
	s, _ := sheetPassService(t)
	in := newSheet(t, s, "slow")
	writeSheet(t, s, "slow", "first", sheetBudgetRows)
	first := slices.Max(storedRevisions(t, s, "slow"))
	writeSheet(t, s, "slow", "second", `{"op":"clear","tab":"Sheet1","ranges":["B2:B3"]}`)
	before := storedRevisions(t, s, "slow")

	reached, resume := make(chan struct{}), make(chan struct{})
	s.sheetRollbackHook = func() {
		close(reached)
		<-resume
	}
	release := sync.OnceFunc(func() { close(resume) })
	defer release()
	type outcome struct {
		r   RevisionRollback
		err error
	}
	rolled := make(chan outcome, 1)
	go func() {
		r, err := s.RollbackRevisionResult(ctx, "slow", first, "Ann", ViaConsole)
		rolled <- outcome{r, err}
	}()
	select {
	case <-reached:
	case o := <-rolled:
		t.Fatalf("the rollback returned %+v, %v without reaching its import", o.r, o.err)
	case <-time.After(sheetTestWait):
		t.Fatal("the rollback did not finish its calculation")
	}
	// The rollback takes the flat lock only after the hook returns.
	lctx, cancel := context.WithTimeout(ctx, sheetTestWait)
	unlock, err := s.lockStorageCtx(lctx, "slow")
	cancel()
	if err != nil {
		t.Fatalf("the flat lock while the rollback waits for it: %v", err)
	}
	res, err := s.importSheetLocked(ctx, "slow", in)
	var recorded []int
	for _, n := range storedRevisions(t, s, "slow") {
		if !slices.Contains(before, n) {
			recorded = append(recorded, n)
		}
	}
	unlock()
	if err != nil || len(recorded) != 1 || len(res.revisions) > 1 || len(res.revisions) == 1 && res.revisions[0] != recorded[0] {
		t.Fatalf("before the rollback's import the test's import recorded %v, %v, and the stored new revisions are %v; want the rollback's one", res.revisions, err, recorded)
	}
	release()
	select {
	case o := <-rolled:
		if o.err != nil || o.r.Revision != recorded[0] || o.r.Warning != "" {
			t.Fatalf("the rollback reported %+v, %v, want revision %d", o.r, o.err, recorded[0])
		}
	case <-time.After(sheetPassBound):
		t.Fatal("the rollback did not return")
	}
	if got := storedRevisions(t, s, "slow"); len(got) != len(before)+1 {
		t.Fatalf("after the rollback the sheet has revisions %v, want one more than %v", got, before)
	}
}
