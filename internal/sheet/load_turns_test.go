package sheet

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// loadPhases sees the check points of loads: how many each phase of each
// instance reached, and the phase each instance is in. While hold names a
// phase, the load of a format-heavy sheet at a check point of that phase
// stays there, pausing at the gate, as a load with much more of that phase
// left would.
type loadPhases struct {
	mu     sync.Mutex
	points map[*Instance]map[string]int
	in     map[*Instance]string
	hold   string
	held   map[*Instance]bool
}

func watchLoads(t *testing.T) *loadPhases {
	t.Helper()
	w := &loadPhases{points: map[*Instance]map[string]int{}, in: map[*Instance]string{}, held: map[*Instance]bool{}}
	see := func(in *Instance, phase string) {
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.points[in] == nil {
			w.points[in] = map[string]int{}
		}
		w.points[in][phase]++
		w.in[in] = phase
	}
	hold := func(turn *calcTurn) bool {
		in := turn.in
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.hold == "" || in == nil || in.slug != heavySlug || w.in[in] != w.hold {
			return false
		}
		w.held[in] = true
		return true
	}
	if !loadPoints.CompareAndSwap(nil, &see) {
		t.Fatal("another test watches loads")
	}
	if !calcHold.CompareAndSwap(nil, &hold) {
		t.Fatal("another test holds passes")
	}
	t.Cleanup(func() {
		w.holdAt("")
		calcHold.Store(nil)
		loadPoints.Store(nil)
	})
	return w
}

func (w *loadPhases) holdAt(phase string) {
	w.mu.Lock()
	w.hold, w.held = phase, map[*Instance]bool{}
	w.mu.Unlock()
}

func (w *loadPhases) count(in *Instance, phase string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.points[in][phase]
}

// heldAt waits until n loads are held in the phase.
func (w *loadPhases) heldAt(t *testing.T, n int, phase string) {
	t.Helper()
	gateSettlesWith(t, fmt.Sprintf("%d loads held in the %s", n, phase), func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.held) >= n
	})
}

func (w *loadPhases) phase(in *Instance) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.in[in]
}

// gateSettlesWith waits until done reports true, as gateSettles waits.
func gateSettlesWith(t *testing.T, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); !done(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			free, line := gateState()
			t.Fatalf("no %s: the gate has %d free slots and %d passes in line", what, free, line)
		}
	}
}

// formatHeavyStyles and formatHeavyRows make a sheet whose styles and
// rows each take a load past several of its check points.
const formatHeavyStyles, formatHeavyRows = 3*loadCheckCells + 100, 3*loadCheckCells + 200

// heavySlug names the format-heavy sheets.
const heavySlug = "heavy"

// formatHeavySheet returns a sheet with formatHeavyStyles distinct formats
// on as many cells, and formatHeavyRows rows.
func formatHeavySheet(t *testing.T) *env {
	t.Helper()
	e := start(t, unhurried, func(o *Options) { o.Slug = heavySlug })
	e.mustAgent(jsonf(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":%d}]}`, formatHeavyRows-newTabRows))
	ops := make([]string, formatHeavyStyles)
	for i := range ops {
		ops[i] = jsonf(`{"op":"format","tab":"Sheet1","ranges":["B%d"],"format":%s}`, i+1, symbolFormat(i))
	}
	e.mustAgent(`{"ops":[` + strings.Join(ops, ",") + `]}`)
	return e
}

// A load takes turns at the gate while it reads the styles, while it
// builds the axes and while it indexes the formats, as it does while it
// reads the cells. Each phase reaches a check point at least once for
// every loadCheckCells styles, ids or formats it goes through. With every
// slot of the gate taken by loads held in one of those phases, the load
// of a small sheet gets a slot from them and ends, while they are still
// in that phase. It runs with a gate of one slot and with several.
func TestLoadTakesTurnsInEachPhase(t *testing.T) {
	slow(t, "loads sheets of thousands of formats while others wait")
	for _, slots := range gateSizes {
		t.Run(fmt.Sprintf("%d slots", slots), func(t *testing.T) {
			gateSlots(t, slots)
			long := make([]*env, slots)
			for i := range long {
				long[i] = formatHeavySheet(t)
			}
			w := watchLoads(t)
			for _, phase := range []string{"styles", "axes", "formats"} {
				w.holdAt(phase)
				for _, e := range long {
					e.in.Stop()
					in, _, err := Open(e.dir, e.b, e.opts)
					if err != nil {
						t.Fatal(err)
					}
					e.in, e.b.in = in, in
				}
				w.heldAt(t, slots, phase)
				gateSettles(t, "with the loads held in the "+phase, 0, 0)
				parked := calcSlots.parks.Load()
				small := start(t)
				small.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,"=A1+1"]]}]}`)
				small.restart()
				small.want("B1", "2")
				if got := calcSlots.parks.Load() - parked; got < 2 {
					t.Fatalf("the loads held in the %s handed %d slots on for two small loads", phase, got)
				}
				for i, e := range long {
					if got := w.phase(e.in); got != phase {
						t.Fatalf("the long load %d left the %s for the %s while it was held", i, phase, got)
					}
				}
				small.in.Stop()
				w.holdAt("")
				for i, e := range long {
					if err := e.in.await(context.Background(), 0); err != nil {
						t.Fatalf("the long load %d: %v", i, err)
					}
					// Each check point stands for at most loadCheckCells of
					// the phase's work.
					for phase, units := range map[string]int{"styles": formatHeavyStyles, "axes": formatHeavyRows, "formats": formatHeavyStyles} {
						if got := w.count(e.in, phase); got < units/loadCheckCells {
							t.Fatalf("the long load %d reached %d check points in the %s for %d of its work", i, got, phase, units)
						}
					}
					e.in.mu.RLock()
					styles := len(e.in.wb.styles.byID)
					e.in.mu.RUnlock()
					if styles != formatHeavyStyles {
						t.Fatalf("the long load %d holds %d styles", i, styles)
					}
				}
			}
			gateSettles(t, "after the loads", slots, 0)
		})
	}
}

// The build of a revision view takes turns at the gate while it builds the
// axes and while it lists and indexes the formats, as it does before each
// file it reads. With every slot taken by builds of format-heavy
// revisions held in one of those phases, a small sheet loads twice, and
// the builds are still in that phase. Each phase reaches a check point at
// least once for every loadCheckCells ids or formats. It runs with a gate
// of one slot and with several.
func TestRevisionBuildTakesTurnsInEachPhase(t *testing.T) {
	slow(t, "builds revisions of thousands of formats while others wait")
	ctx := context.Background()
	for _, slots := range gateSizes {
		t.Run(fmt.Sprintf("%d slots", slots), func(t *testing.T) {
			gateSlots(t, slots)
			heavy := make([]*env, slots)
			for i := range heavy {
				heavy[i] = formatHeavySheet(t)
			}
			w := watchLoads(t)
			for round, phase := range []string{"axes", "formats"} {
				// Each round views a revision of its own, which no cache
				// holds yet.
				revs := make([]int, len(heavy))
				for i, e := range heavy {
					e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[%d]]}]}`, round))
					revs[i] = e.b.importAll(t)
				}
				w.holdAt(phase)
				for i, e := range heavy {
					files, date, release, err := e.b.RevisionFiles(ctx, revs[i])
					if err != nil {
						t.Fatal(err)
					}
					defer release()
					if _, err := e.in.ReadRevision(ctx, revs[i], files, date, ReadQuery{Tab: "Sheet1", Range: "A1"}); refusal(err).Reason != "calculating" {
						t.Fatalf("the view of revision %d did not start a build: %v", revs[i], err)
					}
				}
				w.heldAt(t, slots, phase)
				gateSettles(t, "with the builds held in the "+phase, 0, 0)
				parked := calcSlots.parks.Load()
				small := start(t)
				small.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,"=A1+1"]]}]}`)
				small.restart()
				small.want("B1", "2")
				if got := calcSlots.parks.Load() - parked; got < 2 {
					t.Fatalf("the builds held in the %s handed %d slots on for two small loads", phase, got)
				}
				for i, e := range heavy {
					if got := w.phase(e.in); got != phase {
						t.Fatalf("the build %d left the %s for the %s while it was held", i, phase, got)
					}
				}
				small.in.Stop()
				w.holdAt("")
				for i, e := range heavy {
					out := e.readRevision(revs[i], ReadQuery{Tab: "Sheet1", Range: "A1", Include: []string{"shown"}})
					if got := fmt.Sprint(out["shown"]); got != fmt.Sprintf("[[%d]]", round) {
						t.Fatalf("the view of revision %d reads %v", revs[i], got)
					}
					units := map[string]int{"axes": formatHeavyRows, "formats": formatHeavyStyles}[phase]
					if got := w.count(e.in, phase); got < (round+1)*units/loadCheckCells {
						t.Fatalf("the builds of sheet %d reached %d check points in the %s for %d of their work each", i, got, phase, units)
					}
				}
			}
			gateSettles(t, "after the builds", slots, 0)
		})
	}
}

// handOverAndStop checks the passes of the instances held in a phase: two
// small loads get a slot from them while they stay in that phase, and a
// stop of their instances ends them there, with no check point of a later
// phase and every slot free again.
func handOverAndStop(t *testing.T, w *loadPhases, slots int, phase string, later []string, held []*env) {
	t.Helper()
	w.heldAt(t, slots, phase)
	gateSettles(t, "with the passes held in the "+phase, 0, 0)
	parked := calcSlots.parks.Load()
	small := start(t)
	small.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,"=A1+1"]]}]}`)
	small.restart()
	small.want("B1", "2")
	small.in.Stop()
	if got := calcSlots.parks.Load() - parked; got < 2 {
		t.Fatalf("the passes held in the %s handed %d slots on for two small loads", phase, got)
	}
	before := make([]map[string]int, len(held))
	for i, e := range held {
		if got := w.phase(e.in); got != phase {
			t.Fatalf("the pass %d left the %s for the %s while it was held", i, phase, got)
		}
		before[i] = map[string]int{}
		for _, p := range later {
			before[i][p] = w.count(e.in, p)
		}
	}
	for _, e := range held {
		e.in.Stop()
	}
	gateSettles(t, "after the stop of the passes held in the "+phase, slots, 0)
	for i, e := range held {
		for _, p := range later {
			if got := w.count(e.in, p); got != before[i][p] {
				t.Fatalf("the pass %d stopped in the %s reached %d check points of the %s after its stop", i, phase, got-before[i][p], p)
			}
		}
	}
	w.holdAt("")
}

// denseSheet returns a sheet whose first 256 rows are full of small
// numbers in cols columns: one row file of 256 rows, written as one part.
func denseSheet(t *testing.T, cols int) *env {
	t.Helper()
	return denseSheetNamed(t, cols, heavySlug)
}

// denseSheetNamed is denseSheet under the slug given.
func denseSheetNamed(t *testing.T, cols int, slug string) *env {
	t.Helper()
	e := start(t, unhurried, func(o *Options) { o.Slug = slug })
	e.mustAgent(jsonf(`{"ops":[{"op":"insert_cols","tab":"Sheet1","at":"A","count":%d}]}`, cols-newTabCols))
	per := maxCallCells / cols
	for first := 0; first < chunkRows; first += per {
		var rows []string
		for r := first; r < min(first+per, chunkRows); r++ {
			cells := make([]string, cols)
			for c := range cells {
				cells[c] = fmt.Sprint((r*cols + c) % 1000)
			}
			rows = append(rows, "["+strings.Join(cells, ",")+"]")
		}
		e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[%s]}]}`, first+1, strings.Join(rows, ",")))
	}
	return e
}

// The build of a revision view takes turns at the gate while it decodes a
// row file of 256 full rows, which the engine writes as one part: a small
// sheet loads twice while every slot is taken by builds held in that
// decode. A stop of their sheets ends the builds there, before they read a
// style or a cell. It runs with a gate of one slot and with several.
func TestRevisionBuildTakesTurnsInItsRowFiles(t *testing.T) {
	slow(t, "builds revisions of 256 full rows while others wait")
	ctx := context.Background()
	cols := sized(formula.MaxCols, 96)
	for _, slots := range gateSizes {
		t.Run(fmt.Sprintf("%d slots", slots), func(t *testing.T) {
			gateSlots(t, slots)
			dense := make([]*env, slots)
			for i := range dense {
				dense[i] = denseSheet(t, cols)
			}
			w := watchLoads(t)
			w.holdAt("rows")
			for _, e := range dense {
				n := e.b.importAll(t)
				files, date, release, err := e.b.RevisionFiles(ctx, n)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				parts := 0
				for _, f := range files {
					if strings.HasPrefix(f.Path, "tabs/1/rows/") {
						parts++
					}
				}
				if parts != 1 {
					t.Fatalf("the revision holds %d row files", parts)
				}
				if _, err := e.in.ReadRevision(ctx, n, files, date, ReadQuery{Tab: "Sheet1", Range: "A1"}); refusal(err).Reason != "calculating" {
					t.Fatalf("the view of revision %d did not start a build: %v", n, err)
				}
			}
			handOverAndStop(t, w, slots, "rows", []string{"styles", "cells", "formats"}, dense)
		})
	}
}

// fragmentedSheet returns a format-heavy sheet whose rows lie in runs of
// one, after a sort, and whose styles hold the last style ids, so that a
// load decodes an id run for each row and finds the free ids again.
func fragmentedSheet(t *testing.T) *env {
	t.Helper()
	e := start(t, unhurried, func(o *Options) { o.Slug = heavySlug })
	e.mustAgent(jsonf(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":%d}]}`, formatHeavyRows-newTabRows))
	ops := make([]string, formatHeavyStyles)
	for i := range ops {
		ops[i] = jsonf(`{"op":"format","tab":"Sheet1","ranges":["B%d"],"format":%s}`, i+1, symbolFormat(i))
	}
	leaveIDs(e, formatHeavyStyles)
	e.mustAgent(`{"ops":[` + strings.Join(ops, ",") + `]}`)
	keys := make([]string, formatHeavyRows)
	for i := range keys {
		keys[i] = fmt.Sprintf("[%d]", (i*7919)%formatHeavyRows)
	}
	e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[%s]},{"op":"sort","tab":"Sheet1","rows":"1:%d","by":"A"}]}`, strings.Join(keys, ","), formatHeavyRows))
	e.in.mu.RLock()
	runs := 0
	forRuns(e.in.wb.tabs[0].rows.ids, func(uint64, uint64) { runs++ })
	e.in.mu.RUnlock()
	if runs < formatHeavyRows/2 {
		t.Fatalf("the sort left the rows in %d runs", runs)
	}
	return e
}

// A load takes turns at the gate while it decodes axes whose rows lie in
// runs of one, and while it finds the free style ids of a sheet whose
// styles hold the last ones: with every slot taken by loads held there, a
// small sheet loads twice. A stop ends them there, before any later phase.
// Each phase reaches a check point at least once for every loadCheckCells
// of its work. It runs with a gate of one slot and with several.
func TestLoadTakesTurnsWhileItDecodes(t *testing.T) {
	slow(t, "loads sheets of thousands of rows in runs of one while others wait")
	for _, slots := range gateSizes {
		t.Run(fmt.Sprintf("%d slots", slots), func(t *testing.T) {
			gateSlots(t, slots)
			long := make([]*env, slots)
			for i := range long {
				long[i] = fragmentedSheet(t)
			}
			w := watchLoads(t)
			reopen := func() {
				for _, e := range long {
					e.in.Stop()
					in, _, err := Open(e.dir, e.b, e.opts)
					if err != nil {
						t.Fatal(err)
					}
					e.in, e.b.in = in, in
				}
			}
			phases := []string{"axes", "widths", "styles", "cells", "formats", "refill", "files"}
			for _, phase := range []string{"axes", "refill"} {
				w.holdAt(phase)
				reopen()
				handOverAndStop(t, w, slots, phase, phases[slices.Index(phases, phase)+1:], long)
			}
			reopen()
			for i, e := range long {
				if err := e.in.await(context.Background(), 0); err != nil {
					t.Fatalf("the long load %d: %v", i, err)
				}
				// The axes count each id twice and the free ids once each.
				for phase, units := range map[string]int{"axes": 2 * formatHeavyRows, "refill": formatHeavyStyles} {
					if got := w.count(e.in, phase); got < units/loadCheckCells {
						t.Fatalf("the long load %d reached %d check points in the %s for %d of its work", i, got, phase, units)
					}
				}
				e.in.mu.RLock()
				styles, left := len(e.in.wb.styles.byID), e.in.wb.styles.left()
				e.in.mu.RUnlock()
				if styles != formatHeavyStyles || left < styleRefillLeft {
					t.Fatalf("the long load %d holds %d styles with %d ids left", i, styles, left)
				}
			}
			gateSettles(t, "after the loads", slots, 0)
		})
	}
}
