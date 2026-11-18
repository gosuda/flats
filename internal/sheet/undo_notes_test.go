package sheet

import (
	"cmp"
	"context"
	"fmt"
	"math/rand/v2"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"weak"
)

// noteLink is one stand-in note by itself.
type noteLink struct {
	g     groupKey
	canon uint64
}

// sortedNoteSets builds the note sets of an undo the plain way: it sorts
// the links, drops the ones that come twice and cuts the rest into sets,
// columns and runs.
func sortedNoteSets(u *undoState, stamp uint64, redo bool, links []noteLink) []*noteSet {
	slices.SortFunc(links, func(a, b noteLink) int {
		if d := cmp.Compare(a.canon, b.canon); d != 0 {
			return d
		}
		if x, y := isAux(a.g.prop), isAux(b.g.prop); x != y {
			if y {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(a.g.tab, b.g.tab), cmp.Compare(a.g.col, b.g.col), cmp.Compare(a.g.prop, b.g.prop), cmp.Compare(a.g.row, b.g.row))
	})
	var sets []*noteSet
	var s *noteSet
	var c *noteCol
	for i, l := range links {
		if i > 0 && links[i-1] == l {
			continue
		}
		aux := isAux(l.g.prop)
		if s == nil || s.canon != l.canon || s.aux != aux {
			s = &noteSet{owner: u, stamp: stamp, canon: l.canon, redo: redo, aux: aux}
			sets, c = append(sets, s), nil
		}
		if c == nil || c.compare(l.g.tab, l.g.col, l.g.prop) != 0 {
			s.cols = append(s.cols, noteCol{tab: l.g.tab, col: l.g.col, prop: l.g.prop})
			c = &s.cols[len(s.cols)-1]
		}
		if last := len(c.runs) - 1; last >= 0 && c.runs[last].from+RowID(c.runs[last].n) == l.g.row {
			c.runs[last].n++
		} else {
			c.runs = append(c.runs, noteRun{l.g.row, 1})
		}
		s.n++
	}
	for _, s := range sets {
		for i := range s.cols {
			s.cols[i].runs = slices.Clone(s.cols[i].runs)
		}
		s.cols = slices.Clone(s.cols)
		s.bytes = s.measure()
	}
	return sets
}

// The notes an undo collects as runs while it restores its groups make
// the sets that sorting the same notes one by one makes, with the same
// count of bytes: for cells in the order of the grid, in any other order,
// for notes that come twice, and for the groups that are no cells.
func TestUndoLinksCollectIntoTheSameSets(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 23))
	u := &undoState{}
	for round := 0; round < 400; round++ {
		rows, cols := 1+rng.IntN(40), 1+rng.IntN(6)
		var links []noteLink
		cell := func(row, col int) {
			g := groupKey{tab: TabID(1 + (col+row/30)%2), row: RowID(10 + row), col: ColID(100 + col)}
			if round%5 == 4 {
				// Row ids that leave gaps.
				g.row += RowID(row / 3)
			}
			for prop := groupContent; prop < propCount; prop++ {
				if rng.IntN(8) == 0 {
					continue
				}
				g.prop = int8(prop)
				links = append(links, noteLink{g, uint64(1 + rng.IntN(1+round%3))})
			}
		}
		for row := 0; row < rows; row++ {
			for col := 0; col < cols; col++ {
				cell(row, col)
			}
		}
		for i := rng.IntN(4); i > 0; i-- {
			links = append(links, noteLink{groupKey{tab: 1, col: ColID(100 + rng.IntN(cols)), prop: groupWidth}, 2})
			links = append(links, noteLink{groupKey{tab: 1, row: RowID(10 + rng.IntN(rows)), prop: groupSort}, uint64(1 + rng.IntN(3))})
			links = append(links, noteLink{groupKey{prop: groupOrder}, 1})
		}
		switch round % 4 {
		case 1:
			rng.Shuffle(len(links), func(i, k int) { links[i], links[k] = links[k], links[i] })
		case 2:
			// Some notes twice, and the rows from the bottom up.
			links = append(links, links[:len(links)/3]...)
			slices.Reverse(links)
		}
		// Entry 3 is not on the stack: its notes are not kept.
		onStack := map[uint64]struct{}{1: {}, 2: {}}
		var l linkSet
		for _, link := range links {
			l.add(link.g, link.canon)
		}
		got := l.sets(u, 77, round%2 == 0, onStack)
		want := sortedNoteSets(u, 77, round%2 == 0, slices.DeleteFunc(links, func(link noteLink) bool { return link.canon == 3 }))
		if len(got) != len(want) {
			t.Fatalf("round %d: %d sets, want %d", round, len(got), len(want))
		}
		for i := range want {
			if !reflect.DeepEqual(got[i], want[i]) {
				t.Fatalf("round %d, set %d:\ngot  %+v\nwant %+v", round, i, *got[i], *want[i])
			}
			for k := range want[i].cols {
				if g, w := cap(got[i].cols[k].runs), cap(want[i].cols[k].runs); g != w {
					t.Fatalf("round %d, set %d: column %d keeps room for %d runs, want %d", round, i, k, g, w)
				}
			}
			if g, w := cap(got[i].cols), cap(want[i].cols); g != w {
				t.Fatalf("round %d, set %d: room for %d columns, want %d", round, i, g, w)
			}
		}
	}
}

// noteCounts is what the note budgets of a sheet read: the counters the
// sheet keeps for one session and for all, and the sets of every session
// measured anew, with what an unfinished sweep has built beside its set.
type noteCounts struct{ session, sheet, measured int64 }

func (p *person) noteCounts() noteCounts {
	in := p.e.in
	in.mu.RLock()
	defer in.mu.RUnlock()
	in.smu.Lock()
	defer in.smu.Unlock()
	c := noteCounts{session: p.s.undo.noteBytes, sheet: in.scratch.notes.bytes}
	for _, s := range in.sessions {
		if s.undo.notes != nil {
			c.measured += noteIndexBytes
		}
		for _, sets := range s.undo.notes {
			for _, set := range sets {
				c.measured += set.measure()
			}
		}
	}
	if w := in.scratch.notes.sweep; w != nil {
		c.measured += w.held()
	}
	return c
}

// The notes of an undo over a range are a few runs for each column. When
// another writer then stamps every other row, the notes that are left are
// one run for each row: built again without the others they would take
// many times the bytes, past what a session and the sheet may hold. The
// tick leaves such a set as it is. The notes of the rows the other writer
// stamped match nothing, and the undo they stand for applies to the rows
// that are still the session's and leaves the others.
func TestUndoNotesDoNotGrowWhenRowsBetweenThemAreWritten(t *testing.T) {
	slow(t, "formats every cell of a large sheet several times")
	// 5,000 rows of 100 columns at full size: 4.5 million notes in 900
	// runs, which would be 2.25 million runs of 16 bytes after the other
	// writer's format. By default the sheet has 2,000 rows of 50 columns,
	// whose 450,000 runs would still pass a session's 4 MiB.
	rows, cols := raced(sized(5000, 2000), 80), raced(sized(100, 50), 20)
	cells := rows * cols
	e := start(t)
	holdSweeps(e)
	fillNumbers(e, "Sheet1", rows, cols)
	alice, bob := e.person("Alice"), e.person("Bob")
	for k := 0; k < 2; k++ {
		if ack := alice.do(e.formatRange("Sheet1", 1, rows, 1, cols, nineFormats[k])); status(ack) != "applied" {
			t.Fatalf("format %d: %s", k, status(ack))
		}
	}
	wantUndo(t, "undo of the second format", undo(t, alice), undoAck{Applied: 9 * cells})
	compact := alice.noteCounts()
	if n := alice.stampLinks(); n != 9*cells || compact.session == 0 || compact.session > int64(9*cols)*(noteColBytes+2*noteRunBytes)+2*noteSetBytes+noteIndexBytes {
		t.Fatalf("%d notes in %d bytes, want %d notes in a run for each column and property", n, compact.session, 9*cells)
	}
	if apart := int64(9*cells/2) * noteRunBytes; measures && apart <= undoSessionBytes {
		t.Fatalf("a run for every other row takes %d bytes, inside a session's budget: the case does not reach the bound", apart)
	}
	tab := e.tabID("Sheet1")
	e.in.mu.RLock()
	var runs []string
	for pos := 1; pos < rows; pos += 2 {
		runs = append(runs, fmt.Sprintf("[%d,1]", e.in.wb.tabs[0].rows.ids[pos]))
	}
	e.in.mu.RUnlock()
	if ack := bob.do(jsonf(`{"op":"format","tab":%d,"rows":[%s],"cols":%s,"set":%s}`, tab, strings.Join(runs, ","), e.colsOf("Sheet1", 1, cols), nineFormats[2])); status(ack) != "applied" {
		t.Fatalf("the other writer's format: %s", status(ack))
	}
	if n := alice.stampLinks(); n != 9*cells/2 {
		t.Fatalf("%d notes after another writer stamped every other row, want %d", n, 9*cells/2)
	}
	before := int64(heapInUse())
	looked := sweepTick(e, noteSweepWork)
	grown := int64(heapInUse()) - before
	after := alice.noteCounts()
	t.Logf("%d notes: %d bytes before the tick and %d after it; the tick looked at %d notes and the heap grew by %d KiB", 9*cells/2, compact.session, after.session, looked, grown>>10)
	if after.session > compact.session || after.session != after.measured || after.sheet != after.measured {
		t.Fatalf("after the tick the session counts %d bytes of notes and the sheet %d, the sets take %d; before it they took %d", after.session, after.sheet, after.measured, compact.session)
	}
	if measures && grown > 1<<20 {
		t.Fatalf("the heap grew by %d KiB over a tick that had %d bytes of notes to look at", grown>>10, compact.session)
	}
	// The set is not looked through again before more of its notes go.
	e.in.mu.RLock()
	for _, sets := range alice.s.undo.notes {
		for _, s := range sets {
			if s.due() || s.tried != s.gone || s.gone != 9*cells/2 {
				t.Errorf("a set of %d notes with %d gone, %d when the tick looked: due %v", s.n, s.gone, s.tried, s.due())
			}
		}
	}
	e.in.mu.RUnlock()
	sweepTick(e, noteSweepWork)
	if again := alice.noteCounts(); again != after {
		t.Fatalf("a second tick changed the notes: %+v, then %+v", after, again)
	}
	if alice.lost(0) {
		t.Fatal("the entry the notes stand for expired")
	}
	wantUndo(t, "undo of the first format", undo(t, alice), undoAck{Applied: 9 * cells / 2, Skipped: 9 * cells / 2, Reason: "changed"})
	if e.bold("A1") || !e.bold("A2") {
		t.Fatalf("bold: A1 %v, A2 %v; want the first format undone in row 1 and the other writer's kept in row 2", e.bold("A1"), e.bold("A2"))
	}
	if c := alice.noteCounts(); alice.stampLinks() != 0 || c.session != 0 || c.sheet != c.measured {
		t.Fatalf("after the undo: %d notes, %+v", alice.stampLinks(), c)
	}
	wantUndo(t, "redo of the first format", redo(t, alice), undoAck{Applied: 9 * cells / 2})
	if !e.bold("A1") {
		t.Fatal("the redo did not bring the first format back")
	}
}

// Building a set again without some of its notes gives the set of the
// notes that are left, and never a larger one: where those would take more
// room than the set has, it stays exactly as it was.
func TestNoteSetNeverGrowsWhenBuiltAgain(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 3))
	u := &undoState{}
	kept, left := 0, 0
	for round := 0; round < 600; round++ {
		rows, cols := 1+rng.IntN(60), 1+rng.IntN(5)
		// Rows in stretches, so that the sets hold long runs and single
		// rows.
		var links []noteLink
		for col := 0; col < cols; col++ {
			for row := 0; row < rows; row++ {
				if round%3 == 0 && rng.IntN(3) == 0 {
					continue
				}
				for prop := groupContent; prop < 2; prop++ {
					links = append(links, noteLink{groupKey{tab: 1, row: RowID(10 + row), col: ColID(100 + col), prop: int8(prop)}, 1})
				}
			}
		}
		if len(links) == 0 {
			continue
		}
		s := sortedNoteSets(u, 77, false, slices.Clone(links))[0]
		// Every other row goes, or a random share, or a stretch.
		from, share := rng.IntN(rows), 1+rng.IntN(6)
		keep := func(g groupKey) bool {
			switch round % 4 {
			case 0:
				return g.row%2 == 0
			case 1:
				return int(g.row)%share != 0
			case 2:
				return int(g.row) < 10+from
			}
			return (uint64(g.row)*7+uint64(g.col)*13+uint64(g.prop+1))%uint64(share+1) != 0
		}
		was := *s
		was.cols = slices.Clone(s.cols)
		for i := range was.cols {
			was.cols[i].runs = slices.Clone(s.cols[i].runs)
		}
		var rest []noteLink
		for _, l := range links {
			if keep(l.g) {
				rest = append(rest, l)
			}
		}
		var want *noteSet
		if sets := sortedNoteSets(u, 77, false, rest); len(sets) > 0 {
			want = sets[0]
		}
		s.gone, s.tried = len(links)-len(rest), 1
		was.gone, was.tried = s.gone, s.tried
		// The same set swept a few notes at a time, as ticks do, ends as
		// one filter does. It stays as it is until the sweep ends, and
		// what the sweep holds beside it is the notes kept so far.
		stepped := was
		stepped.cols = slices.Clone(was.cols)
		for i := range stepped.cols {
			stepped.cols[i].runs = slices.Clone(was.cols[i].runs)
		}
		sweep, looked := stepped.sweep(), 0
		for {
			used, done, _ := sweep.step(keep, 1+rng.IntN(40))
			if looked += used; done {
				break
			}
			if !reflect.DeepEqual(stepped, was) {
				t.Fatalf("round %d: the set changed while it was swept", round)
			}
			held := 0
			for _, l := range links {
				if sweep.holds(l.g) {
					if !keep(l.g) {
						t.Fatalf("round %d: the sweep holds the note of %+v, which goes", round, l.g)
					}
					held++
				}
			}
			if held != sweep.n || sweep.held() > 2*was.bytes {
				t.Fatalf("round %d: the sweep holds %d notes in %d bytes, counts %d; the set takes %d bytes", round, held, sweep.held(), sweep.n, was.bytes)
			}
		}
		if looked > len(links) {
			t.Fatalf("round %d: the sweep looked at %d notes of %d", round, looked, len(links))
		}
		if !s.filter(keep) {
			left++
			if !reflect.DeepEqual(*s, was) || !reflect.DeepEqual(stepped, was) {
				t.Fatalf("round %d: a set that was not built again changed:\ngot  %+v\nwant %+v", round, *s, was)
			}
			if want == nil {
				t.Fatalf("round %d: a set with no note left was not built again", round)
			}
			if want.bytes <= s.bytes {
				t.Fatalf("round %d: the set of %d bytes was not built again, and the notes that are left take %d", round, s.bytes, want.bytes)
			}
			// Every note is still found, also those that match nothing.
			for _, l := range links {
				if !s.has(l.g) {
					t.Fatalf("round %d: the set lost the note of %+v", round, l.g)
				}
			}
			continue
		}
		kept++
		if !reflect.DeepEqual(stepped, *s) {
			t.Fatalf("round %d: swept in steps\ngot  %+v\nwant %+v", round, stepped, *s)
		}
		if got := s.measure(); got > s.bytes {
			t.Fatalf("round %d: the set takes %d bytes after it was built again, %d before", round, got, s.bytes)
		}
		if want == nil {
			if s.n != 0 || len(s.cols) != 0 {
				t.Fatalf("round %d: %d notes are left in %d columns, want none", round, s.n, len(s.cols))
			}
			continue
		}
		if s.n != want.n || s.gone != 0 || s.tried != 0 || !reflect.DeepEqual(s.cols, want.cols) {
			t.Fatalf("round %d:\ngot  %d notes, %d gone, %+v\nwant %d notes, %+v", round, s.n, s.gone, s.cols, want.n, want.cols)
		}
	}
	if kept < 100 || left < 100 {
		t.Fatalf("%d sets were built again and %d were left as they were: the rounds do not cover both", kept, left)
	}
	// A set that was left as it was is due again only after another
	// share of its notes has gone.
	s := &noteSet{n: 800}
	for _, step := range []struct {
		gone, tried int
		due         bool
	}{{99, 0, false}, {100, 0, true}, {100, 100, false}, {199, 100, false}, {200, 100, true}, {800, 700, true}} {
		if s.gone, s.tried = step.gone, step.tried; s.due() != step.due {
			t.Fatalf("a set of 800 notes with %d gone, %d of them when it was last looked through: due %v, want %v", step.gone, step.tried, s.due(), step.due)
		}
	}
}

// The tick looks through a bounded number of notes under the sheet's lock.
// Sets it does not reach stay as they are until a later tick, and each is
// built again in time.
func TestUndoNoteSweepIsBoundedPerTick(t *testing.T) {
	const rows, cols = 400, 3
	e := start(t)
	holdSweeps(e)
	fillNumbers(e, "Sheet1", rows, 3*cols)
	people := []*person{e.person("Alice"), e.person("Bob"), e.person("Carol")}
	for i, p := range people {
		for _, set := range []string{`{"bold":true}`, `{"bold":null}`} {
			if ack := p.do(e.formatRange("Sheet1", 1, rows, i*cols+1, cols, set)); status(ack) != "applied" {
				t.Fatalf("format: %s", status(ack))
			}
		}
		wantUndo(t, "undo of the second format", undo(t, p), undoAck{Applied: rows * cols})
	}
	// An agent writes the first quarter of the rows: each set loses a
	// stretch of its notes, and what is left is as compact as before.
	e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"format","tab":"Sheet1","ranges":["A1:I%d"],"format":{"bold":null}}]}`, rows/4))
	// built returns how many of the three sets were built again.
	built := func() int {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		n := 0
		for _, p := range people {
			for _, sets := range p.s.undo.notes {
				for _, s := range sets {
					if s.gone == 0 && s.n == rows*cols*3/4 {
						n++
					}
				}
			}
		}
		return n
	}
	// A tick looks through three quarters of a set. It goes on where the
	// tick before stopped, in the same set, and finishes one set a tick
	// after the first.
	for tick := 1; tick <= 4; tick++ {
		sweepTick(e, rows*cols*3/4)
		if got := built(); got != tick-1 {
			t.Fatalf("after tick %d %d sets were built again, want %d", tick, got, tick-1)
		}
		if c := people[0].noteCounts(); c.sheet != c.measured {
			t.Fatalf("after tick %d the sheet counts %d bytes of notes, and %d are held", tick, c.sheet, c.measured)
		}
	}
	for i, p := range people {
		if c := p.noteCounts(); c.session == 0 || c.sheet != c.measured {
			t.Fatalf("the notes of session %d: %+v", i, c)
		}
		wantUndo(t, "undo of the first format", undo(t, p), undoAck{Applied: rows * cols * 3 / 4, Skipped: rows * cols / 4, Reason: "changed"})
	}
}

// nineFormats give every cell four different values of all nine format
// properties.
var nineFormats = [4]string{
	`{"bold":true,"italic":true,"underline":true,"strikethrough":true,"text_color":"blue-600","fill":"teal-200","align":"right","wrap":true,"number":{"kind":"percent","decimals":1}}`,
	`{"bold":null,"italic":null,"underline":null,"strikethrough":null,"text_color":"gray-100","fill":"red-100","align":"center","wrap":null,"number":{"kind":"number","decimals":1,"thousands":false}}`,
	`{"bold":true,"italic":null,"underline":true,"strikethrough":null,"text_color":"red-600","fill":"blue-100","align":"left","wrap":true,"number":{"kind":"number","decimals":2,"thousands":true}}`,
	`{"bold":null,"italic":true,"underline":null,"strikethrough":true,"text_color":"green-600","fill":"gray-100","align":"right","wrap":null,"number":{"kind":"percent","decimals":2}}`,
}

// One set can hold millions of notes in a few runs: the notes of an undo
// of a format of nine properties over a large range. A tick looks through
// a bounded number of notes and goes on with the same set at the next
// tick, so how long a tick holds the sheet's lock does not grow with the
// set, and the set changes only when it has been looked through whole.
func TestUndoNoteSweepOfOneLargeSetIsBoundedPerTick(t *testing.T) {
	slow(t, "formats every cell of a large range several times")
	// At full size 5,000 rows of 100 columns, 4.5 million notes, and the
	// tick's own bound. Otherwise 1,000 rows of 50 columns and an eighth
	// of the notes a tick.
	rows, cols := raced(sized(5000, 1000), 100), raced(sized(100, 50), 20)
	cells := rows * cols
	notes := 9 * cells
	work := raced(sized(noteSweepWork, notes/8), notes/8)
	ticks := (notes + work - 1) / work
	if ticks < 4 {
		t.Fatalf("%d notes take %d ticks of %d: the case does not reach the bound", notes, ticks, work)
	}
	e := start(t)
	holdSweeps(e)
	fillNumbers(e, "Sheet1", rows, cols)
	alice, bob := e.person("Alice"), e.person("Bob")
	for k := 0; k < 2; k++ {
		if ack := alice.do(e.formatRange("Sheet1", 1, rows, 1, cols, nineFormats[k])); status(ack) != "applied" {
			t.Fatalf("format %d: %s", k, status(ack))
		}
	}
	wantUndo(t, "undo of the second format", undo(t, alice), undoAck{Applied: notes})
	set := func() (n, gone int) {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		for _, sets := range alice.s.undo.notes {
			for _, s := range sets {
				n, gone = n+s.n, gone+s.gone
			}
		}
		return n, gone
	}
	if n, _ := set(); n != notes {
		t.Fatalf("%d notes, want %d", n, notes)
	}
	// Bob formats a quarter of the rows. What is left of each column is
	// one run, so the set is built again in less room once it has been
	// looked through.
	stamp := func(k, from int) {
		t.Helper()
		if ack := bob.do(e.formatRange("Sheet1", from, rows/4, 1, cols, nineFormats[k])); status(ack) != "applied" {
			t.Fatalf("the other writer's format: %s", status(ack))
		}
	}
	stamp(2, 1)
	n, gone := set()
	if n != notes || gone != notes/4 {
		t.Fatalf("%d notes, %d gone; want %d and %d", n, gone, notes, notes/4)
	}
	// The notes each tick looked at.
	var bounded []int64
	for {
		bounded = append(bounded, sweepTick(e, work))
		c := alice.noteCounts()
		if c.sheet != c.measured || c.session > c.sheet {
			t.Fatalf("after tick %d the session counts %d bytes of notes and the sheet %d; %d are held", len(bounded), c.session, c.sheet, c.measured)
		}
		if m := alice.noteMemory(); m.sheet > m.sheetMax || m.session > m.sessionMax {
			t.Fatalf("after tick %d the sheet holds %d bytes of %d, the session %d of %d", len(bounded), m.sheet, m.sheetMax, m.session, m.sessionMax)
		}
		if len(bounded) == 1 {
			// While the set is swept, Bob writes a note the sweep has kept
			// already, in the first column, and one it has not looked at
			// yet, in the last. The first is a gone note of the set built
			// again; the sweep leaves the second out.
			if ack := bob.do(e.formatRange("Sheet1", rows, 1, 1, 1, `{"bold":null}`)); status(ack) != "applied" {
				t.Fatal(status(ack))
			}
			if ack := bob.do(e.formatRange("Sheet1", rows, 1, cols, 1, `{"bold":null}`)); status(ack) != "applied" {
				t.Fatal(status(ack))
			}
			gone += 2
		}
		now, left := set()
		if now == notes-notes/4-1 && left == 1 {
			break
		}
		// The set stays as it was until the last of its notes is looked at.
		if now != n || left != gone {
			t.Fatalf("after tick %d: %d notes, %d gone; want them unchanged at %d and %d", len(bounded), now, left, n, gone)
		}
		if len(bounded) == ticks {
			t.Fatalf("the set is not built again after %d ticks of %d notes", ticks, work)
		}
	}
	if len(bounded) == 1 {
		t.Fatalf("a tick of at most %d notes built a set of %d again", work, notes)
	}
	// The same set again, swept in one tick that has no bound. It looks
	// through three quarters of the notes, against work in a bounded tick.
	stamp(3, rows/4+1)
	held, _ := set()
	whole := sweepTick(e, notes)
	if now, left := set(); now != notes/2-2 || left != 0 {
		t.Fatalf("after the unbounded tick: %d notes, %d gone; want %d and none", now, left, notes/2-2)
	}
	t.Logf("%d notes, %d a tick: the ticks looked at %v notes; one tick without a bound looked at %d", notes, work, bounded, whole)
	// Each bounded tick looks at no more notes than its bound, and the
	// ticks look at each note of the set once. A tick that also looked
	// through the whole set would look at more.
	var sum int64
	for i, looked := range bounded {
		sum += looked
		if looked > int64(work) {
			t.Errorf("tick %d of at most %d notes looked at %d", i+1, work, looked)
		}
	}
	if sum != int64(notes) {
		t.Errorf("the bounded ticks looked at %d notes of a set of %d", sum, notes)
	}
	if whole != int64(held) {
		t.Errorf("the tick without a bound looked at %d notes of a set of %d", whole, held)
	}
	wantUndo(t, "undo of the first format", undo(t, alice), undoAck{Applied: notes/2 - 2, Skipped: notes/2 + 2, Reason: "changed"})
}

// noteMemory is what the limits of undo memory read for a session and for
// the sheet: payloads in memory, what payloads in scratch files leave
// there and stand-in notes, with what an unfinished sweep holds.
type noteMemory struct {
	session, sheet       int64
	sessionMax, sheetMax int64
	swept                *noteSweep
}

func (p *person) noteMemory() noteMemory {
	in := p.e.in
	in.mu.RLock()
	defer in.mu.RUnlock()
	in.smu.Lock()
	defer in.smu.Unlock()
	pool := &in.scratch
	return noteMemory{p.s.undo.resident(), pool.resident + pool.notes.bytes, pool.sessionMax, pool.instanceMax, pool.notes.sweep}
}

// holdSweeps gives the sweep of the instance's own maintenance ticks no
// work. Those ticks come every few seconds, and on a busy machine one of
// them can come in the middle of a test and sweep for it. After this only
// sweepOnce sweeps notes.
func holdSweeps(e *env) {
	e.in.mu.Lock()
	e.in.undoPool().noteWork = 0
	e.in.mu.Unlock()
}

// onWriter runs fn on the writer as one piece of its work, as e.tick runs
// a tick. The instance's own ticks wait until fn returns.
func onWriter(e *env, fn func()) {
	e.t.Helper()
	if err := e.in.execute(context.Background(), &work{run: func() error { fn(); return nil }}); err != nil {
		e.t.Fatal(err)
	}
}

// sweepOnce runs a maintenance tick on the writer whose sweep looks
// through at most work notes, and leaves the sweep without work again
// (holdSweeps). It returns the notes the tick looked at (workSteps). Call
// it on the writer (onWriter).
func sweepOnce(e *env, work int) int64 {
	e.in.mu.Lock()
	e.in.undoPool().noteWork = work
	e.in.mu.Unlock()
	steps := countWork(e.in.maintain)
	holdSweeps(e)
	return steps.notes.Load()
}

// sweepTick runs sweepOnce as one piece of the writer's work.
func sweepTick(e *env, work int) int64 {
	e.t.Helper()
	var looked int64
	onWriter(e, func() { looked = sweepOnce(e, work) })
	return looked
}

// sweepFixture is a set of notes of Alice's undo that the tick builds
// again a stretch at a time: one of millions of notes in a few runs, or
// one of a run for every other row, which takes megabytes. Its sweeps
// run only in sweepTick and sweepOnce.
type sweepFixture struct {
	e          *env
	alice, bob *person
	rows, cols int
	notes      int // of the set, the gone ones included
	// rest formats the rows Bob has not stamped yet.
	rest func()
}

func newSweepFixture(t *testing.T, fragmented bool) *sweepFixture {
	f := &sweepFixture{e: start(t)}
	e := f.e
	holdSweeps(e)
	f.alice, f.bob = e.person("Alice"), e.person("Bob")
	if !fragmented {
		f.rows, f.cols = raced(sized(5000, 1000), 100), raced(sized(100, 50), 20)
		fillNumbers(e, "Sheet1", f.rows, f.cols)
		for k := 0; k < 2; k++ {
			if ack := f.alice.do(e.formatRange("Sheet1", 1, f.rows, 1, f.cols, nineFormats[k])); status(ack) != "applied" {
				t.Fatalf("format %d: %s", k, status(ack))
			}
		}
		f.notes = 9 * f.rows * f.cols
		wantUndo(t, "undo of the second format", undo(t, f.alice), undoAck{Applied: f.notes})
		if ack := f.bob.do(e.formatRange("Sheet1", 1, f.rows/4, 1, f.cols, nineFormats[2])); status(ack) != "applied" {
			t.Fatalf("Bob's format: %s", status(ack))
		}
		f.rest = func() {
			if ack := f.bob.do(e.formatRange("Sheet1", f.rows/4+1, f.rows-f.rows/4, 1, f.cols, nineFormats[3])); status(ack) != "applied" {
				t.Fatalf("Bob's second format: %s", status(ack))
			}
		}
		return f
	}
	f.rows, f.cols = raced(sized(8000, 2000), 400), 20
	fillNumbers(e, "Sheet1", f.rows, f.cols)
	e.in.mu.RLock()
	ids := e.in.wb.tab("Sheet1").rows.ids
	var every []string
	for i := 0; i < f.rows; i += 2 {
		every = append(every, fmt.Sprint(ids[i]))
	}
	e.in.mu.RUnlock()
	alternate := func(set string) string {
		return jsonf(`{"op":"format","tab":%d,"rows":[%s],"cols":%s,"set":%s}`, e.tabID("Sheet1"), strings.Join(every, ","), e.colsOf("Sheet1", 1, f.cols), set)
	}
	for _, set := range []string{`{"bold":true}`, `{"bold":null}`} {
		if ack := f.alice.do(alternate(set)); status(ack) != "applied" {
			t.Fatalf("format %s: %s", set, status(ack))
		}
	}
	f.notes = f.rows / 2 * f.cols
	wantUndo(t, "undo of the second format", undo(t, f.alice), undoAck{Applied: f.notes})
	if ack := f.bob.do(e.formatRange("Sheet1", 1, f.rows/4, 1, f.cols, `{"bold":true}`)); status(ack) != "applied" {
		t.Fatalf("Bob's format: %s", status(ack))
	}
	f.rest = func() {
		if ack := f.bob.do(e.formatRange("Sheet1", f.rows/4+1, f.rows-f.rows/4, 1, f.cols, `{"bold":true}`)); status(ack) != "applied" {
			t.Fatalf("Bob's second format: %s", status(ack))
		}
	}
	return f
}

// set returns Alice's set of notes.
func (f *sweepFixture) set() *noteSet {
	f.e.t.Helper()
	f.e.in.mu.RLock()
	defer f.e.in.mu.RUnlock()
	var found *noteSet
	for _, sets := range f.alice.s.undo.notes {
		for _, s := range sets {
			if found != nil {
				f.e.t.Fatal("Alice has more than one set of notes")
			}
			found = s
		}
	}
	if found == nil {
		f.e.t.Fatal("Alice has no set of notes")
	}
	return found
}

// An unfinished sweep holds the notes it has built beside its set. They
// count in the session's and the sheet's undo memory from the moment they
// are built, and the sweep builds only as much as those limits leave room
// for. With the sheet nearly full it stops, and leaves the set as it is
// until another share of its notes is gone; with room for it, it finishes.
// What the counters say the sweep holds is what the heap holds.
func TestUndoNoteSweepStaysInsideTheMemoryLimits(t *testing.T) {
	slow(t, "formats every cell of a large range several times")
	for _, fragmented := range []bool{false, true} {
		for _, room := range []string{"nearly full", "room for the sweep"} {
			name := "notes in long runs, "
			if fragmented {
				name = "a run for every other row, "
			}
			t.Run(name+room, func(t *testing.T) {
				f := newSweepFixture(t, fragmented)
				e, s := f.e, f.set()
				if !s.due() {
					t.Fatalf("the set is not due: %d notes, %d gone", s.n, s.gone)
				}
				n, gone, bytes := s.n, s.gone, s.bytes
				before := f.alice.noteMemory()
				// The sheet is nearly full, with what other sessions hold.
				// A sweep needs at most the set's bytes beside it here.
				free := int64(4 << 10)
				if room != "nearly full" {
					free = 2 * bytes
				}
				e.in.mu.Lock()
				e.in.undoPool().instanceMax = before.sheet + free
				// Drop write marks that no entry needs before measuring
				// the room available for the sweep.
				e.in.pruneMarks()
				e.in.mu.Unlock()
				// Run the note sweeps and readings together on the writer.
				// The rest of a maintenance tick allocates unrelated memory,
				// including scratch directory read buffers.
				type reading struct {
					sheet, sheetMax     int64
					session, sessionMax int64
					sweeping            bool
					counts              noteCounts
					heap                int64
					noise               uint64
				}
				readings := make([]reading, 0, 10)
				var base int64
				var baseNoise uint64
				work := max(1, f.notes/6)
				onWriter(e, func() {
					if measures {
						base, baseNoise = heapWithin(64<<10, 15*time.Second)
					}
					for range 10 {
						e.in.mu.Lock()
						pool := e.in.undoPool()
						pool.noteWork = work
						e.in.sweepNotes()
						pool.noteWork = 0
						e.in.mu.Unlock()
						m := f.alice.noteMemory()
						r := reading{sheet: m.sheet, sheetMax: m.sheetMax, session: m.session, sessionMax: m.sessionMax,
							sweeping: m.swept != nil, counts: f.alice.noteCounts()}
						if measures {
							r.heap, r.noise = heapWithin(64<<10, 15*time.Second)
						}
						readings = append(readings, r)
						if !r.sweeping {
							return
						}
					}
				})
				if baseNoise > 64<<10 {
					t.Fatalf("something still allocated %d bytes during every collection for 15 s", baseNoise)
				}
				counted := before.sheet
				for i, r := range readings {
					tick := i + 1
					if r.sheet > r.sheetMax || r.session > r.sessionMax {
						t.Fatalf("after tick %d the sheet holds %d bytes of %d, the session %d of %d", tick, r.sheet, r.sheetMax, r.session, r.sessionMax)
					}
					if r.counts.sheet != r.counts.measured {
						t.Fatalf("after tick %d the sheet counts %d bytes of notes, and %d are held", tick, r.counts.sheet, r.counts.measured)
					}
					if measures {
						if r.noise > 64<<10 {
							t.Fatalf("something still allocated %d bytes during every collection for 15 s", r.noise)
						}
						// The heap grows or shrinks by what the counters do, within
						// the size classes of its lists and what a collection reads
						// beside it.
						grown, said := r.heap-base, r.sheet-counted
						t.Logf("tick %d: the heap grew by %d bytes, the counters by %d (GC allocations: baseline %d, current %d)",
							tick, grown, said, baseNoise, r.noise)
						if d, slack := grown-said, 16<<10+max(said, -said)/8; d > slack || d < -slack {
							t.Fatalf("after tick %d the heap grew by %d bytes and the counters by %d", tick, grown, said)
						}
					}
				}
				if readings[len(readings)-1].sweeping {
					t.Fatal("the sweep did not end in 10 ticks")
				}
				now := f.set()
				if room == "nearly full" {
					if now.n != n || now.gone != gone || now.bytes != bytes || now.due() {
						t.Fatalf("with no room: %d notes, %d gone, %d bytes, due %v; want %d, %d and %d, not due", now.n, now.gone, now.bytes, now.due(), n, gone, bytes)
					}
				} else if now.gone != 0 || now.n != n-gone || now.bytes > bytes {
					t.Fatalf("with room: %d notes, %d gone, %d bytes; want %d, none and at most %d", now.n, now.gone, now.bytes, n-gone, bytes)
				}
				// No entry expired for it.
				e.in.mu.RLock()
				for _, en := range f.alice.s.undo.undo {
					if en.lost {
						t.Errorf("Alice's entry %d expired", en.seq)
					}
				}
				e.in.mu.RUnlock()
				wantUndo(t, "undo of the first format", undo(t, f.alice), undoAck{Applied: f.notes - f.notes/4, Skipped: f.notes / 4, Reason: "changed"})
			})
		}
	}
}

// When the set a sweep builds again leaves its session, what the sweep has
// built goes with it at once. The counters say so before the next tick,
// and the storage of the set and of the sweep is unreachable.
func TestUndoNoteSweepEndsWithItsSet(t *testing.T) {
	slow(t, "formats every other row of a large range")
	f := newSweepFixture(t, true)
	e := f.e
	sweepTick(e, max(1, f.notes/3))
	e.in.mu.RLock()
	w := e.in.scratch.notes.sweep
	if w == nil || len(w.cols) == 0 || cap(w.runs) == 0 {
		e.in.mu.RUnlock()
		t.Fatal("the sweep ended in one tick, or holds no columns yet")
	}
	storage := []weak.Pointer[noteCol]{weak.Make(&w.set.cols[0]), weak.Make(&w.cols[0])}
	runs := weak.Make(&w.runs[:1][0])
	e.in.mu.RUnlock()
	w = nil
	// Bob stamps the rest of the rows. No note of the set is left, and the
	// set goes as his action commits.
	f.rest()
	m, c := f.alice.noteMemory(), f.alice.noteCounts()
	if m.swept != nil || c.session != 0 || c.sheet != 0 || c.measured != 0 {
		t.Fatalf("after the set went: sweep %v, the session counts %d bytes of notes, the sheet %d, %d are held", m.swept != nil, c.session, c.sheet, c.measured)
	}
	runtime.GC()
	for i, p := range storage {
		if p.Value() != nil {
			t.Fatalf("the columns of the %s are still reachable", []string{"set", "sweep"}[i])
		}
	}
	if runs.Value() != nil {
		t.Fatal("the runs of the sweep are still reachable")
	}
}

// An unfinished sweep holds room that a person's action can need. The
// sweep gives it back before the action's payload goes to a scratch file
// or any entry expires, and the set stays as it was.
func TestUndoNoteSweepGivesWayToActions(t *testing.T) {
	slow(t, "formats every other row of a large range")
	f := newSweepFixture(t, true)
	e := f.e
	n, gone := f.set().n, f.set().gone
	sweepTick(e, max(1, f.notes/3))
	m := f.alice.noteMemory()
	if m.swept == nil || m.swept.held() < 4<<10 {
		t.Fatal("the sweep holds too little to give way")
	}
	// spilled counts the entries of every session whose payload is in a
	// scratch file, and those that expired.
	spilled := func() (files, lost int) {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		for _, s := range e.in.sessions {
			for _, stack := range [2][]*undoEntry{s.undo.undo, s.undo.redo} {
				for _, en := range stack {
					if en.file != nil {
						files++
					}
					if en.lost {
						lost++
					}
				}
			}
		}
		return files, lost
	}
	files, lost := spilled()
	// The sheet has no room left beside the sweep.
	e.in.mu.Lock()
	e.in.undoPool().instanceMax = m.sheet
	e.in.mu.Unlock()
	carol := e.person("Carol")
	carol.set("A1", "Carol's")
	after := f.alice.noteMemory()
	if after.swept != nil || after.sheet > after.sheetMax {
		t.Fatalf("after Carol's action: sweep %v, the sheet holds %d bytes of %d", after.swept != nil, after.sheet, after.sheetMax)
	}
	if f, l := spilled(); f != files || l != lost {
		t.Fatalf("%d payloads in scratch files and %d expired entries, before %d and %d", f, l, files, lost)
	}
	if s := f.set(); s.n != n || s.gone != gone || s.due() {
		t.Fatalf("the set: %d notes, %d gone, due %v; want it as it was, %d and %d, and not due", s.n, s.gone, s.due(), n, gone)
	}
	wantUndo(t, "Carol's undo", undo(t, carol), undoAck{Applied: 1})
}
