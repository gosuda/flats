package sheet

import (
	"context"
	"fmt"
	"math/bits"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// A numbersKind is a formula whose work reads no cell and goes through no
// text: all of it is numbers written in the formula. The work starts the
// day after the test's first day, so a sheet of such formulas is written
// cheaply and a change of the date starts the long pass.
type numbersKind struct {
	name    string
	formula string
	first   string // the value on the first day
	shown   string // the value from the second day on
	// units is the least work of one formula from the second day on, in
	// units of the budget, counted by hand from the formula.
	units int64
}

// medianOfZeros is a median of 4,000 zeros written in the formula. A
// median goes through its numbers once to count them and eight times to
// find the middle ones: 36,000 numbers for a formula.
func medianOfZeros() numbersKind {
	const zeros = 4000
	text := "=IF(TODAY()>DATE(2026,11,13),MEDIAN(" + strings.TrimSuffix(strings.Repeat("0,", zeros), ",") + "),-1)"
	return numbersKind{name: "medians of 4,000 zeros", formula: text, first: "-1", shown: "0", units: 9 * zeros}
}

// mediansOfOne is as many medians of one number as a formula holds.
// Finding a number by its rank goes through two tables of 256 counts eight
// times, however few the numbers are: 64 units for a median.
func mediansOfOne() numbersKind {
	const prefix, suffix, piece = "=IF(TODAY()>DATE(2026,11,13),", ",-1)", "MEDIAN(1)+"
	calls := (formula.MaxFormulaChars - len(prefix) - len(suffix) + 1) / len(piece)
	text := prefix + strings.TrimSuffix(strings.Repeat(piece, calls), "+") + suffix
	return numbersKind{name: "medians of one number", formula: text, first: "-1", shown: fmt.Sprint(calls), units: 64 * int64(calls)}
}

// numbersSheet is a sheet of count formulas of a kind, with the reports of
// its recalculations.
type numbersSheet struct {
	*env
	kind  numbersKind
	count int
	// passes counts the recalculations that no edit started: the load,
	// and then each change of the date.
	passes atomic.Int64
	// reads are the reads that started a change of the date.
	reads sync.WaitGroup
}

const numbersCols = 10

func newNumbersSheet(t *testing.T, kind numbersKind, count int) *numbersSheet {
	t.Helper()
	if count%numbersCols != 0 || count > numbersCols*newTabRows {
		t.Fatalf("%d formulas do not fill rows of %d columns of a new tab", count, numbersCols)
	}
	s := &numbersSheet{kind: kind, count: count}
	s.env = start(t, unhurried, func(o *Options) {
		o.OnRecalc = func(edit bool, _ time.Duration) {
			if !edit {
				s.passes.Add(1)
			}
		}
	})
	row := `["` + strings.TrimSuffix(strings.Repeat(kind.formula+`","`, numbersCols), `","`) + `"]`
	// A call holds at most 8 MiB.
	const rowsPerCall = 40
	for first := 0; first < count/numbersCols; first += rowsPerCall {
		rows := min(rowsPerCall, count/numbersCols-first)
		s.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[%s]}]}`,
			first+1, strings.TrimSuffix(strings.Repeat(row+",", rows), ",")))
	}
	s.want("A1", kind.first)
	s.want(fmt.Sprintf("J%d", count/numbersCols), kind.first)
	// A read ends with its pass, or with the stop of the sheet at the
	// latest, before the test is over.
	t.Cleanup(func() {
		s.in.Stop()
		s.reads.Wait()
	})
	return s
}

// full is the least work of a pass that calculates every formula.
func (s *numbersSheet) full() int64 { return int64(s.count) * s.kind.units }

// day returns the calculation date of the sheet.
func (s *numbersSheet) day() string {
	s.in.mu.RLock()
	defer s.in.mu.RUnlock()
	return s.in.wb.date.String()
}

// nextDay moves the clock of the sheet to the next day and asks the sheet
// for a cell, which starts the change of the date. It does not wait for
// the pass: a reader may be answered when the pass is done, or be told
// that the sheet is calculating.
func (s *numbersSheet) nextDay() {
	s.clock.Add(24 * time.Hour)
	in := s.in
	s.reads.Add(1)
	go func() {
		defer s.reads.Done()
		_, err := in.Read(context.Background(), ReadQuery{Tab: "Sheet1", Range: "A1"})
		if err != nil && refusal(err).Code != "unavailable" {
			s.t.Errorf("the read that starts the date change: %v", err)
		}
	}()
}

// wantCalculated waits until the sheet calculates with the given day and
// checks that the newest pass calculated every formula: it counted the
// work of all of them, and the sheet shows their values on that day. The
// writer reports a pass to OnRecalc before it installs the pass's date and
// values together under the write lock, so it is the date that tells that
// the values are there, not the count of passes.
func (s *numbersSheet) wantCalculated(t *testing.T, day string) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Minute); s.day() != day; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("after %d passes the sheet calculates with %s, want %s", s.passes.Load(), s.day(), day)
		}
	}
	if work := s.in.lastWork.Load(); work < s.full() {
		t.Fatalf("the pass counted %d units of work, for %d formulas of at least %d each", work, s.count, s.kind.units)
	}
	s.want("A1", s.kind.shown)
	s.want(fmt.Sprintf("J%d", s.count/numbersCols), s.kind.shown)
}

// numbersHeld is how many formulas the sheets of the tests that hold a
// pass at a check point have. A loop of the pass looks at its deadline
// every passStride formulas, and the marking of a date change counts each
// formula twice, once as it reads the date and once as it is marked, so
// these sheets have fewer than half of passStride. The loops before the
// evaluation count less than the budget's 4,096 units over so few formulas
// and the rows of a new tab, so the first check point of a pass comes in
// the evaluation of its first formula, from the numeric work alone.
// TestStopEndsANumericPass checks that: the pass it stops there has
// counted the budget's interval at least.
const numbersHeld = numbersCols * (passStride / 2 / numbersCols)

// readSoon reads a range of a sheet whose date may be changing: a reader
// that is told that the sheet is calculating asks again.
func readSoon(t *testing.T, e *env, q ReadQuery) string {
	t.Helper()
	for deadline := time.Now().Add(shortBound); ; time.Sleep(time.Millisecond) {
		out, err := e.in.Read(context.Background(), q)
		if err == nil {
			return string(out)
		}
		if w := refusal(err); w.Code != "unavailable" || w.Reason != "calculating" || time.Now().After(deadline) {
			t.Fatalf("the read: %v", err)
		}
	}
}

// takeSlot stands in line at the gate as a pass does and returns once it
// has a slot. While every slot is held by a pass at work, it gets one only
// when such a pass reaches a check point and hands its slot on, or ends.
// The caller gives the slot back with leave; the end of the test does
// otherwise.
func takeSlot(t *testing.T) *calcTurn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	turn, ok := calcSlots.enter(nil, ctx)
	t.Cleanup(turn.leave)
	if !ok {
		t.Fatal("no pass gave a slot up within a minute")
	}
	return turn
}

// TestNumericPassesTakeTurns is TestLongPassesTakeTurns with passes that
// reach their check points by their numeric work. As many sheets as the
// gate has slots change their date, and each starts a pass that only goes
// through numbers written in its formulas: it reads no cell and no text,
// and its sheet has too few formulas for a loop of the pass to look at the
// deadline by their count, so only the count of its own work brings it to
// a check point (numbersHeld). Each pass is held at the first check point
// it reaches, as a pass with minutes of numeric work left would be.
//
// While those passes are held a slot is asked for, three times, and comes
// each time: a pass handed it on from the middle of its work. An unrelated
// small sheet then loads, takes a change of the date and serves a read,
// each within a short bound. Let go, the long passes end with every
// formula calculated.
func TestNumericPassesTakeTurns(t *testing.T) {
	for _, kind := range []numbersKind{medianOfZeros(), mediansOfOne()} {
		for _, slots := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s, %d slots", kind.name, slots), func(t *testing.T) {
				gateSlots(t, slots)
				small := start(t, unhurried)
				small.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,"=SUM(A1:A3)","=TODAY()"],[2],[3]]}]}`)
				small.in.Stop()
				held := holdPasses(t)
				long := make([]*numbersSheet, slots)
				for i := range long {
					long[i] = newNumbersSheet(t, kind, numbersHeld)
					held.hold(long[i].in)
				}
				ended := func() (n int64) {
					for _, s := range long {
						n += s.passes.Load() - 1 // without the load
					}
					return n
				}
				for _, s := range long {
					s.nextDay()
				}
				held.reached(t, slots)
				// Should the gate never hand a slot on, the passes are let go
				// after a minute, so that the test ends and reports it.
				guard := time.AfterFunc(time.Minute, held.release)
				defer guard.Stop()
				gateSettles(t, "with the long passes held at work", 0, 0)
				parked := calcSlots.parks.Load()

				// A slot comes from a pass that is still at work. The pass
				// that gave the last one up takes it back before the next
				// is asked for, so that each slot asked for is one a pass
				// hands on.
				for i := 0; i < 3; i++ {
					gateSettles(t, "with the long passes held at work again", 0, 0)
					turn := takeSlot(t)
					if n := ended(); n != 0 {
						t.Fatalf("%d long passes had ended when a slot came: none was handed on from a pass at work", n)
					}
					turn.leave()
				}
				if got := calcSlots.parks.Load() - parked; got < 3 {
					t.Fatalf("the long passes handed %d slots on for three passes that asked", got)
				}

				// The small sheet.
				timed(t, "a load", small.open)
				small.want("B1", "6")
				small.clock.Add(24 * time.Hour)
				timed(t, "a date change and a read", func() {
					out := readSoon(t, small, ReadQuery{Tab: "Sheet1", Range: "B1:C1", Include: []string{"shown"}})
					if !strings.Contains(out, `"date":"2026-11-14"`) || !strings.Contains(out, `[["6","2026-11-14"]]`) {
						t.Fatalf("after the date change the small sheet reads %s", out)
					}
				})
				if !guard.Stop() || ended() != 0 {
					t.Fatalf("%d long passes ended before they were let go", ended())
				}

				// The long passes end, each with all of its work done.
				held.release()
				for _, s := range long {
					s.wantCalculated(t, "2026-11-14")
				}
				gateSettles(t, "after every pass", slots, 0)
			})
		}
	}
}

// TestStopEndsANumericPass stops a sheet in the middle of such a pass,
// held at a check point of its numeric work. The stop does not wait for
// the pass to finish: the pass ends at that check point with part of its
// work done, and the sheet keeps its previous state, date included. A stop
// that waited would wait until the pass is let go, and the pass would then
// count all of its work.
func TestStopEndsANumericPass(t *testing.T) {
	for _, kind := range []numbersKind{medianOfZeros(), mediansOfOne()} {
		t.Run(kind.name, func(t *testing.T) {
			gateSlots(t, 1)
			held := holdPasses(t)
			s := newNumbersSheet(t, kind, numbersHeld)
			held.hold(s.in)
			s.nextDay()
			held.reached(t, 1)
			guard := time.AfterFunc(time.Minute, held.release)
			defer guard.Stop()
			// The pass hands its slot on: it is at work.
			takeSlot(t).leave()
			if n := s.passes.Load(); n != 1 {
				t.Fatalf("the pass had ended before the stop: %d passes", n)
			}
			s.in.Stop()
			if !guard.Stop() {
				t.Fatal("the stop waited until the pass was let go")
			}
			if n := s.passes.Load(); n != 2 {
				t.Fatalf("%d passes ended, want the load and the stopped pass", n)
			}
			work := s.in.lastWork.Load()
			t.Logf("the stopped pass had counted %d of at least %d units", work, s.full())
			if work >= s.full() {
				t.Fatalf("the stopped pass counted %d units of work; a whole pass counts at least %d", work, s.full())
			}
			// The budget looks at the deadline once its work reaches its
			// interval: a pass held with less work was brought to its check
			// point by the count of its formulas, not by its numeric work.
			if work < budgetCheck {
				t.Fatalf("the stopped pass counted %d units of work: it was held at a check point before its numeric work", work)
			}
			gateSettles(t, "after the stop", 1, 0)
			// Nothing of the pass was stored.
			s.clock.Add(-24 * time.Hour)
			s.open()
			if got := s.day(); got != "2026-11-13" {
				t.Fatalf("after the stop the sheet calculates with %s", got)
			}
			s.want("A1", kind.first)
		})
	}
}

// TestEditMeetsItsDeadlineInNumericWork makes an edit start a full pass
// over such formulas, with its deadline on the units its budget counts
// (workDeadline). The edit without a deadline counts the work of every
// formula. A like edit whose deadline passes at a quarter of those units
// is refused within an eighth of them after it, in the middle of the
// evaluation, and the sheet is as before. The numeric work of a formula
// looks at the deadline as it goes, not once at its end.
func TestEditMeetsItsDeadlineInNumericWork(t *testing.T) {
	slow(t, "calculates a sheet of long formulas")
	sizes := map[string]int{medianOfZeros().name: raced(1000, 200), mediansOfOne().name: raced(100, 50)}
	for _, kind := range []numbersKind{medianOfZeros(), mediansOfOne()} {
		t.Run(kind.name, func(t *testing.T) {
			s := newNumbersSheet(t, kind, sizes[kind.name])
			s.nextDay()
			s.wantCalculated(t, "2026-11-14")
			// An inserted row makes every formula dirty.
			insert := func() string {
				return agentStatus(s.env, fmt.Sprintf(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":%d,"count":1}]}`, newTabRows+1))
			}
			deadline := s.workDeadline()
			got, whole := deadline.run(never, insert)
			if got != "applied" {
				t.Fatalf("the edit without a deadline: %s", got)
			}
			if whole.used < s.full() {
				t.Fatalf("the edit counted %d units of work, for at least %d", whole.used, s.full())
			}
			// A formula counts tens of thousands of units. Its work comes
			// to the budget a step at a time, so two looks at the deadline
			// are at most the budget's interval and one step apart.
			if gap := whole.longest(); gap > budgetCheck+1024 {
				t.Errorf("the edit counted %d units between two looks at its deadline; a formula counts %d", gap, kind.units)
			}
			before := s.dump()
			w := deadline.stopsAtDeadline(t, whole, insert)
			if after := s.dump(); after != before {
				t.Fatalf("the refused edit changed the sheet: %s", dumpDiff(before, after))
			}
			if w.used >= s.full() {
				t.Errorf("the refused edit counted %d units of work, a whole pass of at least %d", w.used, s.full())
			}
			// Without a deadline the same edit is applied.
			s.mustAgent(fmt.Sprintf(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":%d,"count":1}]}`, newTabRows+1))
			if work := s.in.lastWork.Load(); work < s.full() {
				t.Errorf("the applied edit counted %d units of work, for at least %d", work, s.full())
			}
			s.want("A1", kind.shown)
		})
	}
}

// TestPassLooksAtItsDeadline calculates a sheet of each kind with a budget
// that records every look at the deadline. However the formulas count
// their work, two looks are never more than a bounded number of formulas
// apart, in every phase of the pass, nor more than the budget's interval
// and a step of the work apart, also inside the numeric work of one
// formula. A look that finds the deadline passed ends the pass before it
// has calculated the formulas up to the next look.
func TestPassLooksAtItsDeadline(t *testing.T) {
	cheap := numbersKind{name: "numbers", formula: "=1", first: "1", shown: "1", units: 1}
	for _, kind := range []numbersKind{medianOfZeros(), mediansOfOne(), cheap} {
		t.Run(kind.name, func(t *testing.T) {
			count := raced(500, 200)
			switch kind.name {
			case mediansOfOne().name:
				count = raced(100, 50)
			case cheap.name:
				count = numbersCols * newTabRows
			}
			s := newNumbersSheet(t, kind, count)
			s.in.mu.Lock()
			defer s.in.mu.Unlock()
			wb := s.in.wb
			day := wb.date + 1
			pass := func(b *formula.Budget) (calcOut, *WriteError) {
				return wb.recalc(calcReq{all: true, date: &day, budget: b})
			}
			looks := 0
			var b *formula.Budget
			var last, widest int64
			b = formula.NewBudget(func() bool {
				looks++
				widest, last = max(widest, b.Used()-last), b.Used()
				return false
			})
			out, err := pass(b)
			if err != nil || len(out.nodes) != count {
				t.Fatalf("the pass: %v, %d formulas", err, len(out.nodes))
			}
			// A formula of the numeric kinds counts tens of thousands of
			// units, which come to the budget a step at a time.
			if widest > budgetCheck+1024 {
				t.Fatalf("the pass counted %d units between two looks at its deadline; a formula counts at least %d", widest, kind.units)
			}
			// The pass has four loops over the formulas: it marks them,
			// orders them, takes their old text results from the total
			// and calculates them.
			if least := 4 * (count / passStride); looks < least {
				t.Fatalf("the pass looked at its deadline %d times, want at least %d for %d formulas", looks, least, count)
			}
			// The deadline passes at a look: the pass ends there.
			for _, at := range []int{1, looks / 2} {
				seen := 0
				b := formula.NewBudget(func() bool {
					seen++
					return seen >= at
				})
				_, err := pass(b)
				if err == nil || err.Limit != "recalculation_time" {
					t.Fatalf("a pass whose deadline passed at look %d of %d answered %v", at, looks, err)
				}
				if seen != at {
					t.Errorf("the pass looked at its deadline %d times after it had passed at look %d", seen-at, at)
				}
			}
		})
	}
}

// TestMarkingLooksAtItsDeadline recalculates from one edited cell that
// 10,000 formulas read. Marking them dirty looks each of them up in the
// index, which counts a unit or so for each, and the loop looks at the
// deadline every 64 formulas as the other loops of the pass do. A deadline
// that passes while the formulas are marked ends the pass at that look.
func TestMarkingLooksAtItsDeadline(t *testing.T) {
	const count = numbersCols * newTabRows
	e := start(t)
	row := `["` + strings.TrimSuffix(strings.Repeat(`=$L$1","`, numbersCols), `","`) + `"]`
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + strings.TrimSuffix(strings.Repeat(row+",", newTabRows), ",") + `]},
		{"op":"write","tab":"Sheet1","at":"L1","rows":[[7]]}]}`)
	e.want("J1000", "7")
	e.in.mu.Lock()
	defer e.in.mu.Unlock()
	wb := e.in.wb
	pass := func(b *formula.Budget) (calcOut, *WriteError) {
		return wb.recalc(calcReq{cells: []cellAt{{wb.tabs[0], formula.Pos{Col: numbersCols + 1}}}, budget: b})
	}
	looks := 0
	out, err := pass(formula.NewBudget(func() bool {
		looks++
		return false
	}))
	if err != nil || len(out.nodes) != count {
		t.Fatalf("the pass: %v, %d formulas", err, len(out.nodes))
	}
	// Four loops go over the formulas: marking, ordering, taking their old
	// results from the total and evaluation.
	if least := 4 * (count / passStride); looks < least {
		t.Fatalf("the pass looked at its deadline %d times, want at least %d for %d formulas", looks, least, count)
	}
	// The deadline passes at the tenth look, while the formulas are marked.
	seen := 0
	b := formula.NewBudget(func() bool {
		seen++
		return seen >= 10
	})
	if _, err := pass(b); err == nil || err.Limit != "recalculation_time" || seen != 10 {
		t.Fatalf("a pass whose deadline passed at look 10 answered %v after %d looks", err, seen)
	}
}

// TestOrderingReachesTheDeadlineInsideARange orders a formula whose range
// holds no formula in 40,000 cells, in rows that hold formulas beside it.
// The cells count on the budget a block at a time, so the pass looks at
// its deadline inside the range and not only after it.
func TestOrderingReachesTheDeadlineInsideARange(t *testing.T) {
	const rows = 4000
	e := start(t)
	e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":%d}]}`, rows-newTabRows))
	row := `[1,2,3,4,5,6,7,8,9,10,"=1"]`
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + strings.TrimSuffix(strings.Repeat(row+",", rows), ",") + `]},
		{"op":"write","tab":"Sheet1","at":"M1","rows":[["=SUM(A1:J4000)"]]}]}`)
	e.want("M1", fmt.Sprint(55*rows))
	e.in.mu.Lock()
	defer e.in.mu.Unlock()
	var b *formula.Budget
	var last, widest int64
	b = formula.NewBudget(func() bool {
		widest, last = max(widest, b.Used()-last), b.Used()
		return false
	})
	if _, err := e.in.wb.recalc(calcReq{all: true, budget: b}); err != nil {
		t.Fatal(err)
	}
	// Ordering goes through each row of the range and its ten cells, and
	// evaluation reads the cells.
	if least := int64(rows*11 + rows*10); b.Used() < least {
		t.Fatalf("the pass counted %d units of work, for at least %d", b.Used(), least)
	}
	// Two looks are 4,096 units apart, and work reaches the budget a block
	// of about a thousand cells at a time.
	if widest > 4096+2*1024 {
		t.Fatalf("the pass counted %d units of work between two looks at its deadline", widest)
	}
}

// TestOrderingCountsTheCellsOfAWideRange orders a formula whose range is
// as wide as the tab, over rows that each hold 700 numbers and a formula.
// Ordering goes through every cell of such a row to find the formulas in
// the range, and each of those cells counts, as the cells of a narrow
// range do.
func TestOrderingCountsTheCellsOfAWideRange(t *testing.T) {
	const rows, numbers = 200, 700
	e := start(t, unhurried)
	e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"insert_cols","tab":"Sheet1","at":"A","count":%d}]}`, formula.MaxCols-newTabCols))
	row := "[" + strings.Repeat("1,", numbers) + `"=1"]`
	// A call holds at most 50,000 cells.
	const rowsPerCall = 50
	for first := 1; first <= rows; first += rowsPerCall {
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[%s]}]}`,
			first, strings.TrimSuffix(strings.Repeat(row+",", rowsPerCall), ",")))
	}
	e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[["=SUM(A1:ZZ%d)"]]}]}`, rows+1, rows))
	e.want(fmt.Sprintf("A%d", rows+1), fmt.Sprint((numbers+1)*rows))
	e.in.mu.Lock()
	defer e.in.mu.Unlock()
	var b *formula.Budget
	var last, widest int64
	b = formula.NewBudget(func() bool {
		widest, last = max(widest, b.Used()-last), b.Used()
		return false
	})
	if _, err := e.in.wb.recalc(calcReq{all: true, budget: b}); err != nil {
		t.Fatal(err)
	}
	// Evaluation reads every cell of the range, and ordering goes through
	// the 701 cells of each row.
	if least := int64(rows*formula.MaxCols + rows*(numbers+1)); b.Used() < least {
		t.Fatalf("the pass counted %d units of work, for at least %d", b.Used(), least)
	}
	// A row is at most 702 cells, which reach the budget with the block
	// they end.
	if widest > 4096+1024+formula.MaxCols {
		t.Fatalf("the pass counted %d units of work between two looks at its deadline", widest)
	}
}

// TestIndexBuildReachesTheDeadlineInsideAFormula builds the index of a
// sheet whose one formula names 1,169 ranges of 702 columns: 820,638
// entries. They count on the budget a block at a time, so the build looks
// at its deadline while it enters the formula, and a deadline that passes
// there ends it.
func TestIndexBuildReachesTheDeadlineInsideAFormula(t *testing.T) {
	const ranges, entries = 1169, 1169 * formula.MaxCols
	e := start(t)
	e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"insert_cols","tab":"Sheet1","at":"A","count":%d}]}`, formula.MaxCols-newTabCols))
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A2","rows":[["=SUM(` + strings.TrimSuffix(strings.Repeat("A1:ZZ1,", ranges), ",") + `)"]]}]}`)
	e.in.mu.Lock()
	defer e.in.mu.Unlock()
	wb := e.in.wb
	var b *formula.Budget
	var last, widest int64
	looks := 0
	b = formula.NewBudget(func() bool {
		looks++
		widest, last = max(widest, b.Used()-last), b.Used()
		return false
	})
	if err := wb.buildDeps(b); err != nil || wb.deps.size() != entries {
		t.Fatalf("the build: %v, %d entries, want %d", err, wb.deps.size(), entries)
	}
	if b.Used() < entries || b.Used() > entries+64 || looks < entries/(4096+1024) || widest > 4096+1024 {
		t.Fatalf("the build counted %d units of work for %d entries and looked at its deadline %d times, at most %d units apart",
			b.Used(), entries, looks, widest)
	}
	seen := 0
	passed := formula.NewBudget(func() bool {
		seen++
		return seen >= looks/2
	})
	if err := wb.buildDeps(passed); err == nil || err.Limit != "recalculation_time" || !wb.deps.stale || wb.deps.size() != 0 {
		t.Fatalf("a build past its deadline answered %v, with %d entries", err, wb.deps.size())
	}
	if passed.Used() > entries/2+2*4096 {
		t.Fatalf("the stopped build counted %d units of work; its deadline passed at about %d", passed.Used(), entries/2)
	}
	if err := wb.buildDeps(nil); err != nil || wb.deps.size() != entries {
		t.Fatalf("the build without a deadline: %v, %d entries", err, wb.deps.size())
	}
}

// TestFirstLookupMergesUnderTheBudget looks a cell up in a column of the
// index that a full build left loose, with 240,000 ranges in no order. The
// lookup has to put them in order first. That work counts on the budget of
// the recalculation, which looks at its deadline while the column is
// merged, and a deadline that passes there ends the merge with every range
// still in the column.
func TestFirstLookupMergesUnderTheBudget(t *testing.T) {
	const formulas, ranges = 300, 800
	e := start(t, unhurried)
	// Formula i names ranges of two rows of column A that start at rows
	// spread over the tab. An action of more than 1,000 cells builds the
	// index in full.
	reads := 0 // the formulas that read A5
	var rows strings.Builder
	for i := 0; i < formulas; i++ {
		var f strings.Builder
		hit := false
		for j := 0; j < ranges; j++ {
			first := (i*7919+j*104729)%997 + 1
			fmt.Fprintf(&f, "A%d:A%d,", first, first+1)
			hit = hit || first == 4 || first == 5
		}
		if hit {
			reads++
		}
		fmt.Fprintf(&rows, `["=SUM(%s)",1,2,3],`, strings.TrimSuffix(f.String(), ","))
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[` + strings.TrimSuffix(rows.String(), ",") + `]}]}`)
	if reads == 0 || reads == formulas {
		t.Fatalf("%d of %d formulas read A5", reads, formulas)
	}
	e.in.mu.Lock()
	defer e.in.mu.Unlock()
	wb := e.in.wb
	column := wb.deps.ranges[colKey{wb.tabs[0].id, 0}]
	const entries = formulas * ranges
	held := func() int { return column.sorted.len() + len(column.tail) }
	if column == nil || !column.loose || held() != entries {
		t.Fatalf("the column of the index is not loose with %d ranges", entries)
	}
	lookup := func(b *formula.Budget) (calcOut, *WriteError) {
		return wb.recalc(calcReq{cells: []cellAt{{wb.tabs[0], formula.Pos{Row: 4}}}, budget: b})
	}

	// The deadline passes at the second look, in the merge.
	seen := 0
	passed := formula.NewBudget(func() bool {
		seen++
		return seen >= 2
	})
	if _, err := lookup(passed); err == nil || err.Limit != "recalculation_time" {
		t.Fatalf("a lookup past its deadline answered %v", err)
	}
	if seen != 2 || passed.Used() > 3*4096 || !column.loose || held() != entries {
		t.Fatalf("the stopped lookup: %d looks at the deadline, %d units of work, the column loose %v with %d of %d ranges",
			seen, passed.Used(), column.loose, held(), entries)
	}

	var b *formula.Budget
	var last, widest int64
	looks := 0
	b = formula.NewBudget(func() bool {
		looks++
		widest, last = max(widest, b.Used()-last), b.Used()
		return false
	})
	out, err := lookup(b)
	if err != nil || len(out.nodes) != reads || column.loose || held() != entries {
		t.Fatalf("the lookup: %v, %d formulas, want %d; the column loose %v with %d ranges", err, len(out.nodes), reads, column.loose, held())
	}
	// Going through the ranges, comparing each at least once and building
	// the search tree over them is three steps for a range at the least,
	// 64 steps to a unit.
	if least := int64(3 * entries / 64); b.Used() < least || looks < int(least/(4096+64)) || widest > 4096+2*1024 {
		t.Fatalf("the lookup counted %d units of work, for at least %d, and looked at its deadline %d times, at most %d units apart",
			b.Used(), least, looks, widest)
	}
}

// TestLookupCountsItsEntriesInBlocks looks up a cell that 194,550 entries
// of the index name: ranges of one column and single references. The
// entries count on the budget a block at a time, so the recalculation
// looks at its deadline while it goes through them, and stops there once
// the deadline has passed.
func TestLookupCountsItsEntriesInBlocks(t *testing.T) {
	const each, ranges, singles = 50, 1161, 2730
	sum := `"=SUM(` + strings.TrimSuffix(strings.Repeat("A1:A2,", ranges), ",") + `)"`
	plus := `"=` + strings.TrimSuffix(strings.Repeat("A1+", singles), "+") + `"`
	e := start(t, unhurried)
	// More than 1,000 cells in one action, so the index is built in full.
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"C1","rows":[` +
		strings.Repeat("["+sum+","+plus+"],", each) + strings.TrimSuffix(strings.Repeat("[1,2],", 500), ",") + `]}]}`)
	e.in.mu.Lock()
	defer e.in.mu.Unlock()
	wb := e.in.wb
	const entries = each * (ranges + singles)
	if wb.deps.size() != entries {
		t.Fatalf("the index holds %d entries, want %d", wb.deps.size(), entries)
	}
	lookup := func(b *formula.Budget) (calcOut, *WriteError) {
		return wb.recalc(calcReq{cells: []cellAt{{wb.tabs[0], formula.Pos{}}}, budget: b})
	}
	var b *formula.Budget
	var last, widest int64
	b = formula.NewBudget(func() bool {
		widest, last = max(widest, b.Used()-last), b.Used()
		return false
	})
	out, err := lookup(b)
	if err != nil || len(out.nodes) != 2*each {
		t.Fatalf("the lookup: %v, %d formulas, want %d", err, len(out.nodes), 2*each)
	}
	if b.Used() < entries || widest > 4096+2*1024 {
		t.Fatalf("the pass counted %d units of work for %d entries, at most %d units apart", b.Used(), entries, widest)
	}
	// The deadline passes at the first look: the lookup ends in the
	// entries of the cell, before it has gone through them all.
	passed := formula.NewBudget(func() bool { return true })
	if _, err := lookup(passed); err == nil || err.Limit != "recalculation_time" {
		t.Fatalf("a lookup past its deadline answered %v", err)
	}
	if passed.Used() > 4096+2*1024 {
		t.Fatalf("the stopped lookup counted %d units of work", passed.Used())
	}
}

// TestEditEntersManyRangesOfOneColumn writes 500 formulas that name
// 580,500 ranges of one column in one call. The index takes the ranges as
// they come and puts the column in order once, at the next lookup, so the
// call and the edit after it sort the column once: about n log n steps of
// the sort for n ranges (workSteps), where putting it in order at every
// full tail would take n log n steps for each 64 ranges. An edit of a cell
// of the column then finds every formula.
func TestEditEntersManyRangesOfOneColumn(t *testing.T) {
	slow(t, "enters 580,500 ranges into the index")
	const formulas, ranges = 500, 1161
	row := `["=SUM(` + strings.TrimSuffix(strings.Repeat("A1:A2,", ranges), ",") + `)"]`
	e := start(t, unhurried)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1],[2]]}]}`)
	call := `{"ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[` + strings.TrimSuffix(strings.Repeat(row+",", formulas), ",") + `]}]}`
	var err error
	steps := countWork(func() { _, err = e.agent(call) })
	if err != nil {
		t.Fatalf("the call: %v", err)
	}
	e.in.mu.RLock()
	entries := e.in.wb.deps.size()
	e.in.mu.RUnlock()
	if entries != formulas*ranges {
		t.Fatalf("the index holds %d entries, want %d", entries, formulas*ranges)
	}
	e.want("B1", fmt.Sprint(3*ranges))
	e.want(fmt.Sprintf("B%d", formulas), fmt.Sprint(3*ranges))
	edit := countWork(func() { e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A2","rows":[[4]]}]}`) })
	e.want("B1", fmt.Sprint(5*ranges))
	n := int64(formulas * ranges)
	sorted := steps.sorted.Load() + edit.sorted.Load()
	t.Logf("the call took %d steps of the sort of the index, the edit %d, for %d ranges", steps.sorted.Load(), edit.sorted.Load(), n)
	if most := 4 * n * int64(bits.Len64(uint64(n))); sorted > most {
		t.Errorf("the index took %d steps to sort %d ranges, over %d", sorted, n, most)
	}
	e.want(fmt.Sprintf("B%d", formulas), fmt.Sprint(5*ranges))
	// The lookup went through the ranges to put them in order, a step
	// each, and then looked at each of them, a unit each.
	if work, least := e.in.lastWork.Load(), int64(entries+entries/64); work < least {
		t.Fatalf("the edit of a cell of the column counted %d units of work, for at least %d", work, least)
	}
}
