package sheet

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// rollbackFixture is a dense sheet with a revision to roll back to and a
// later change that the rollback would take back.
type rollbackFixture struct {
	e     *env
	files []File
	dump  string
	seq   uint64
}

func newRollbackFixture(t *testing.T, cols int, slug string) *rollbackFixture {
	t.Helper()
	e := denseSheetNamed(t, cols, slug)
	n := e.b.importAll(t)
	files, _, release, err := e.b.RevisionFiles(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["after"]]}]}`)
	return &rollbackFixture{e: e, files: files, dump: e.dump(), seq: e.seq()}
}

// rollBack starts a rollback of the fixture under ctx and returns where
// its error comes.
func (f *rollbackFixture) rollBack(ctx context.Context, i int) <-chan error {
	done := make(chan error, 1)
	in := f.e.in
	go func() { done <- in.Rollback(ctx, f.files, "Alice", fmt.Sprintf("turns-%d", i)) }()
	return done
}

// wantUnchanged fails when the sheet of the fixture is not as it was before
// the rollback, or still calculates.
func (f *rollbackFixture) wantUnchanged(t *testing.T, i int, when string) {
	t.Helper()
	if got := f.e.seq(); got != f.seq {
		t.Fatalf("sheet %d is at %d %s, was at %d", i, got, when, f.seq)
	}
	if after := f.e.dump(); after != f.dump {
		t.Fatalf("sheet %d changed %s:\n%s", i, when, dumpDiff(f.dump, after))
	}
	if f.e.in.calculating() {
		t.Fatalf("sheet %d still calculates %s", i, when)
	}
}

// rollbackPhases are the phases of a rollback at the gate a test holds it
// in, with the phases after each: the decode of a row file of 256 full
// rows, and the adoption of the calculated target, which goes through
// its styles, ids, widths and cells (Workbook.adoptFrom).
var rollbackPhases = []struct {
	phase string
	later []string
}{
	{"rows", []string{"styles", "cells", "formats", "adopt"}},
	{"adopt", nil},
}

// wantReadable fails when a reader of the sheet of each env would wait
// for the workbook lock.
func wantReadable(t *testing.T, envs []*env, phase string) {
	t.Helper()
	for i, e := range envs {
		if !e.in.mu.TryRLock() {
			t.Fatalf("the sheet %d cannot be read while its rollback is held in the %s", i, phase)
		}
		e.in.mu.RUnlock()
	}
}

// A rollback builds its target at the gate and takes turns there while it
// decodes a row file of 256 full rows and while it makes the calculated
// target the next state of the live sheet: with every slot taken by
// rollbacks held in one of those phases, a small sheet loads twice, and
// their sheets stay readable. A stop of their sheets ends the rollbacks
// there, before any later phase, and each sheet opens again as it was
// before its rollback. It runs with a gate of one slot and with several.
func TestRollbackTakesTurnsWhileItBuilds(t *testing.T) {
	slow(t, "rolls back to revisions of 256 full rows while others wait")
	ctx := context.Background()
	cols := sized(formula.MaxCols, 96)
	for _, p := range rollbackPhases {
		for _, slots := range gateSizes {
			t.Run(fmt.Sprintf("%s, %d slots", p.phase, slots), func(t *testing.T) {
				gateSlots(t, slots)
				fixtures := make([]*rollbackFixture, slots)
				held := make([]*env, slots)
				for i := range fixtures {
					fixtures[i] = newRollbackFixture(t, cols, heavySlug)
					held[i] = fixtures[i].e
				}
				w := watchLoads(t)
				w.holdAt(p.phase)
				done := make([]<-chan error, slots)
				for i, f := range fixtures {
					done[i] = f.rollBack(ctx, i)
				}
				w.heldAt(t, slots, p.phase)
				wantReadable(t, held, p.phase)
				handOverAndStop(t, w, slots, p.phase, p.later, held)
				for i, f := range fixtures {
					if err := <-done[i]; refusal(err).Code != "unavailable" {
						t.Fatalf("the rollback %d stopped with its sheet answered %v", i, err)
					}
					f.e.open()
					f.wantUnchanged(t, i, "when it opens after its stopped rollback")
				}
			})
		}
	}
}

// A rollback whose request ends while it builds its target at the gate,
// or while it makes the calculated target the next state of the live
// sheet, ends there, before any later phase, and installs nothing: the
// live sheet, its sequence and its state are as before, the sheet no
// longer calculates and every slot of the gate is free again. The same
// rollback asked again then applies. It runs with a gate of one slot and
// with several, each slot taken by such a rollback.
func TestRollbackEndsWithItsRequestWhileItBuilds(t *testing.T) {
	slow(t, "rolls back to revisions of 256 full rows and gives the requests up")
	cols := sized(formula.MaxCols, 96)
	for _, p := range rollbackPhases {
		for _, slots := range gateSizes {
			t.Run(fmt.Sprintf("%s, %d slots", p.phase, slots), func(t *testing.T) {
				gateSlots(t, slots)
				fixtures := make([]*rollbackFixture, slots)
				for i := range fixtures {
					fixtures[i] = newRollbackFixture(t, cols, heavySlug)
				}
				w := watchLoads(t)
				w.holdAt(p.phase)
				cancels := make([]context.CancelFunc, slots)
				done := make([]<-chan error, slots)
				for i, f := range fixtures {
					var ctx context.Context
					ctx, cancels[i] = context.WithCancel(context.Background())
					done[i] = f.rollBack(ctx, i)
				}
				w.heldAt(t, slots, p.phase)
				// The phase it is held in counts too: a rollback that went on
				// after its request ended would reach more of its points.
				phases := append([]string{p.phase}, p.later...)
				before := make([]map[string]int, slots)
				for i, f := range fixtures {
					before[i] = map[string]int{}
					for _, phase := range phases {
						before[i][phase] = w.count(f.e.in, phase)
					}
				}
				for i := range fixtures {
					cancels[i]()
					if err := <-done[i]; !errors.Is(err, context.Canceled) {
						t.Fatalf("the rollback %d whose request ended answered %v", i, err)
					}
				}
				gateSettles(t, "after the rollbacks whose requests ended", slots, 0)
				for i, f := range fixtures {
					for _, phase := range phases {
						if got := w.count(f.e.in, phase); got != before[i][phase] {
							t.Fatalf("the rollback %d reached %d check points of the %s after its request ended", i, got-before[i][phase], phase)
						}
					}
					f.wantUnchanged(t, i, "after its rollback ended with the request")
				}
				w.holdAt("")
				f := fixtures[0]
				if err := <-f.rollBack(context.Background(), slots); err != nil {
					t.Fatalf("the rollback asked again: %v", err)
				}
				f.e.want("A1", "0")
			})
		}
	}
}

// A rollback that the instance's stop or the end of its request meets
// after its calculation and its adoption, once it has left the gate and
// before its transaction, stores nothing: the sequence, the content and
// the outbox of the sheet stay as they were, and so does the sheet that
// opens again. That holds while the forward of the stop to the rollback's
// pass has not run yet. A rollback asked again after the end of its
// request applies.
func TestRollbackStopsBeforeItStores(t *testing.T) {
	ctx := context.Background()
	for _, stop := range []bool{true, false} {
		name := "the end of its request"
		if stop {
			name = "a stop"
		}
		t.Run(name, func(t *testing.T) {
			var armed atomic.Bool
			reached, release := make(chan struct{}), make(chan struct{})
			e := start(t, func(o *Options) {
				// The rollback's pass reports its calculation once it has
				// left the gate; the hook keeps it there.
				o.OnRecalc = func(edit bool, _ time.Duration) {
					if !edit && armed.CompareAndSwap(true, false) {
						close(reached)
						<-release
					}
				}
			})
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["before"]]}]}`)
			n := e.b.importAll(t)
			files, _, done, err := e.b.RevisionFiles(ctx, n)
			if err != nil {
				t.Fatal(err)
			}
			defer done()
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["after"]]}]}`)
			seq, dump, outbox := e.seq(), e.dump(), e.outboxAfter(0)
			rctx, cancel := context.WithCancel(ctx)
			defer cancel()
			armed.Store(true)
			answer := make(chan error, 1)
			in := e.in
			go func() { answer <- in.Rollback(rctx, files, "Alice", "stopped") }()
			<-reached
			stopped := make(chan struct{})
			// The forward of the stop to the rollback's pass, when it runs
			// before the rollback is done, waits until the rollback has
			// answered, so only a look at the stop itself can end it.
			forward := make(chan struct{})
			if stop {
				hold := func() { <-forward }
				passForward.Store(&hold)
				defer passForward.Store(nil)
				go func() {
					in.Stop()
					close(stopped)
				}()
				gateSettlesWith(t, "stop of the instance", func() bool { return in.ctx.Err() != nil })
			} else {
				cancel()
				close(stopped)
			}
			close(release)
			err = <-answer
			close(forward)
			<-stopped
			if stop && refusal(err).Reason != "stopped" || !stop && !errors.Is(err, context.Canceled) {
				t.Fatalf("the rollback answered %v", err)
			}
			if !stop {
				if got, after := e.seq(), e.dump(); got != seq || after != dump {
					t.Fatalf("the rollback whose request ended changed the sheet from %d to %d:\n%s", seq, got, dumpDiff(dump, after))
				}
				if got := e.outboxAfter(0); !slices.Equal(got, outbox) {
					t.Fatalf("the outbox holds %v after the rollback, held %v", got, outbox)
				}
				e.in.Stop()
			}
			e.open()
			if got, after := e.seq(), e.dump(); got != seq || after != dump {
				t.Fatalf("the sheet opens at %d after the rollback, was at %d:\n%s", got, seq, dumpDiff(dump, after))
			}
			if got := e.outboxAfter(0); !slices.Equal(got, outbox) {
				t.Fatalf("the outbox holds %v after the rollback, held %v", got, outbox)
			}
			e.want("A1", "after")
			if !stop {
				if err := e.in.Rollback(ctx, files, "Alice", "again"); err != nil {
					t.Fatalf("the rollback asked again: %v", err)
				}
				e.want("A1", "before")
			}
		})
	}
}

// A rollback that finds every slot of the gate taken waits for one before
// it reads its target, and builds it at the gate: it gets its slot from
// the revision builds that hold the slots, which stay in their decode,
// and reaches the check points of each phase of its build. It runs with a
// gate of one slot and with several.
func TestRollbackBuildsAtAFullGate(t *testing.T) {
	slow(t, "rolls back while builds of 256 full rows hold every slot")
	ctx := context.Background()
	cols := sized(formula.MaxCols, 96)
	for _, slots := range gateSizes {
		t.Run(fmt.Sprintf("%d slots", slots), func(t *testing.T) {
			gateSlots(t, slots)
			dense := make([]*env, slots)
			for i := range dense {
				dense[i] = denseSheet(t, cols)
			}
			f := newRollbackFixture(t, cols, "rollback")
			w := watchLoads(t)
			w.holdAt("rows")
			for _, e := range dense {
				n := e.b.importAll(t)
				files, date, release, err := e.b.RevisionFiles(ctx, n)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				if _, err := e.in.ReadRevision(ctx, n, files, date, ReadQuery{Tab: "Sheet1", Range: "A1"}); refusal(err).Reason != "calculating" {
					t.Fatalf("the view of revision %d did not start a build: %v", n, err)
				}
			}
			w.heldAt(t, slots, "rows")
			gateSettles(t, "with the builds held in their row files", 0, 0)
			parked := calcSlots.parks.Load()
			if err := <-f.rollBack(ctx, 0); err != nil {
				t.Fatalf("the rollback: %v", err)
			}
			f.e.want("A1", "0")
			if got := calcSlots.parks.Load() - parked; got < 1 {
				t.Fatalf("the held builds handed %d slots on for the rollback", got)
			}
			for i, e := range dense {
				if got := w.phase(e.in); got != "rows" {
					t.Fatalf("the build %d left its row file for the %s while it was held", i, got)
				}
			}
			cells := chunkRows * cols
			// The adoption visits every cell of the target, besides its
			// styles, ids and widths.
			for phase, least := range map[string]int{"rows": cells / loadCheckCells, "cells": cells / loadCheckCells, "adopt": cells / loadCheckCells} {
				if got := w.count(f.e.in, phase); got < least {
					t.Fatalf("the rollback reached %d check points in the %s of its %d cells, want at least %d", got, phase, cells, least)
				}
			}
			for _, e := range dense {
				e.in.Stop()
			}
			gateSettles(t, "after the stop of the held builds", slots, 0)
		})
	}
}

// A tab recovery builds the revision it copies from at the gate, as a
// view and a rollback do, and takes turns there while it decodes a row
// file of 256 full rows: with every slot taken by recoveries held in that
// decode, a small sheet loads twice. A stop of their sheets ends them
// there, and each sheet opens again without a recovered tab. It runs with
// a gate of one slot and with several.
func TestRecoveryTakesTurnsWhileItBuilds(t *testing.T) {
	slow(t, "recovers tabs of 256 full rows while others wait")
	cols := sized(formula.MaxCols, 96)
	for _, slots := range gateSizes {
		t.Run(fmt.Sprintf("%d slots", slots), func(t *testing.T) {
			gateSlots(t, slots)
			fixtures := make([]*rollbackFixture, slots)
			held := make([]*env, slots)
			people := make([]*person, slots)
			for i := range fixtures {
				fixtures[i] = newRollbackFixture(t, cols, heavySlug)
				held[i] = fixtures[i].e
				people[i] = held[i].person("Alice")
			}
			w := watchLoads(t)
			w.holdAt("rows")
			done := make([]chan error, slots)
			for i, f := range fixtures {
				done[i] = make(chan error, 1)
				go func() {
					_, err := f.e.in.recoverTab(context.Background(), people[i].s, "", f.files, 1)
					done[i] <- err
				}()
			}
			handOverAndStop(t, w, slots, "rows", []string{"styles", "cells", "formats"}, held)
			for i, f := range fixtures {
				if err := <-done[i]; refusal(err).Code != "unavailable" {
					t.Fatalf("the recovery %d stopped with its sheet answered %v", i, err)
				}
				f.e.open()
				f.wantUnchanged(t, i, "when it opens after its stopped recovery")
			}
		})
	}
}
