package sheet

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// TestFillOpsF06 is case F-06 of section 22.2: a fill in auto and in series
// of each source of F-01 and F-02 that holds dates or times, down and up,
// then a stop and a load of the sheet. The sheet loads again, and each
// filled cell shows what it showed before the stop. The grid, get_sheet,
// the formula bar, the CSV, the revision and an undo read from its scratch
// file give for each filled cell what they give for a cell where the text
// it shows was typed, and its content is that typed content, bit for bit.
// So a filled time equals, and sorts with, the typed time it shows.
func TestFillOpsF06(t *testing.T) {
	sources := []struct {
		texts []string
		n     int // the target cells down, and again up
	}{
		{[]string{"22:00"}, 3},
		{[]string{"09:00", "09:20"}, 4},
		{[]string{"1900-01-01"}, 2}, // no series up: it is copied
		{[]string{"2026-03-15 08:00", "2026-03-15 08:20"}, 4},
		{[]string{"1900-01-25 19:29:37", "1900-01-25 19:49:37"}, 4},
		{[]string{"23:00", "23:30"}, 4}, // a series of times past midnight
		{[]string{"00:30", "01:00"}, 4}, // and up past midnight
		{[]string{"2026-03-15 23:20", "2026-03-15 23:40"}, 4},
		{[]string{"2026-03-15"}, 3},
		{[]string{"9999-12-30"}, 2}, // F-02: no series past 9999-12-31
	}
	// Each source fills one column down from its top and one column up to
	// row 1. The typed copy of a filled cell is typedRows below it, and a
	// formula that compares the two is formulaRows below it.
	const typedRows, formulaRows = 10, 20
	type cellRef struct{ col, row int } // 0-based column, 1-based row
	addr := func(c cellRef) string { return fmt.Sprintf("%s%d", formula.ColumnName(int32(c.col)), c.row) }
	below := func(c cellRef, rows int) cellRef { return cellRef{c.col, c.row + rows} }
	for _, mode := range []string{"auto", "series"} {
		t.Run(mode, func(t *testing.T) {
			e := start(t, unhurried)
			spillAll := func() {
				e.in.mu.Lock()
				e.in.undoPool().entryMax = -1
				e.in.mu.Unlock()
			}
			spillAll()
			alice := e.person("Alice")
			var filled []cellRef
			var timeCols []int
			lastRow := 0
			for i, src := range sources {
				m, n := len(src.texts), src.n
				down, up := 2*i, 2*i+1
				if typed, _ := formula.ParseTyped(src.texts[0]); typed.Kind == formula.ContentTime {
					timeCols = append(timeCols, down, up)
				}
				for j, text := range src.texts {
					alice.set(addr(cellRef{down, j + 1}), text)
					alice.set(addr(cellRef{up, n + j + 1}), text)
				}
				fills := []struct{ source, target string }{
					{addr(cellRef{down, 1}) + ":" + addr(cellRef{down, m}), addr(cellRef{down, m + 1}) + ":" + addr(cellRef{down, m + n})},
					{addr(cellRef{up, n + 1}) + ":" + addr(cellRef{up, n + m}), addr(cellRef{up, 1}) + ":" + addr(cellRef{up, n})},
				}
				for _, f := range fills {
					if ack := alice.do(e.fillRange(f.source, f.target, mode)); status(ack) != "applied" || ack.Results[0].Applied != n {
						t.Fatalf("the fill of %s over %s: %+v", f.source, f.target, ack.Results)
					}
				}
				for k := 1; k <= n; k++ {
					filled = append(filled, cellRef{down, m + k}, cellRef{up, k})
				}
				lastRow = max(lastRow, m+n)
			}
			cols := 2 * len(sources)
			wantTyped := func(when string) {
				t.Helper()
				for _, c := range filled {
					shown, got := e.shown(addr(c)), e.contentOf(addr(c))
					if typed, _ := formula.ParseTyped(shown); !sameContent(got, typed) {
						t.Errorf("%s: %s holds %+v, which shows %q, and typed entry gives %+v for it", when, addr(c), got, shown, typed)
					}
				}
			}
			wantTyped("after the fills")
			shown := map[cellRef]string{}
			contents := map[cellRef]formula.Content{}
			for _, c := range filled {
				shown[c], contents[c] = e.shown(addr(c)), e.contentOf(addr(c))
			}
			for _, c := range filled {
				alice.set(addr(below(c, typedRows)), shown[c])
				alice.set(addr(below(c, formulaRows)), "="+addr(c)+"="+addr(below(c, typedRows)))
			}
			e.clock.Add(groupIdle + time.Second)
			e.tick()
			rev := e.b.importAll(t)

			e.restart()
			spillAll()
			wantTyped("after the load")
			s := serve(e)
			client := s.dial("private", helloV1)
			client.next("welcome")
			_, grid := client.watch(1, e.tabID("Sheet1"))
			gridCell := func(c cellRef) string {
				arr := grid[cellKey(e, addr(c))]
				if len(arr) < 3 {
					t.Fatalf("the grid has %s as %v", addr(c), arr)
				}
				if len(arr) > 5 {
					arr = arr[:5] // the style is the format's place in the head's list
				}
				return fmt.Sprint(arr[1:])
			}
			value := func(out map[string]any) string {
				data, _ := json.Marshal(firstCell(out))
				return string(data)
			}
			for _, c := range filled {
				a, typed := addr(c), addr(below(c, typedRows))
				if got := e.shown(a); got != shown[c] {
					t.Errorf("after the load %s shows %q, and before the stop %q", a, got, shown[c])
				}
				if got := e.contentOf(a); !sameContent(got, contents[c]) {
					t.Errorf("after the load %s holds %+v, and before the stop %+v", a, got, contents[c])
				}
				if got, want := e.contentOf(a), e.contentOf(typed); !sameContent(got, want) {
					t.Errorf("%s holds %+v, and %s where %q was typed %+v", a, got, typed, shown[c], want)
				}
				if got := e.shown(addr(below(c, formulaRows))); got != "TRUE" {
					t.Errorf("=%s=%s is %s", a, typed, got)
				}
				if got, want := gridCell(c), gridCell(below(c, typedRows)); got != want {
					t.Errorf("the grid has %s as %s, and the typed %s as %s", a, got, typed, want)
				}
				if got, want := e.text(a), e.text(typed); got != want {
					t.Errorf("the formula bar shows %q for %s and %q for the typed %s", got, a, want, typed)
				}
				live := value(e.read(ReadQuery{Tab: "Sheet1", Range: a}))
				if want := value(e.read(ReadQuery{Tab: "Sheet1", Range: typed})); live != want || live == "null" {
					t.Errorf("get_sheet reads %s as %s and the typed %s as %s", a, live, typed, want)
				}
				csv := func(at string) any { return e.read(ReadQuery{Tab: "Sheet1", Range: at, Format: "csv"})["csv"] }
				if got, want := csv(a), csv(typed); got != want || got == "" {
					t.Errorf("the CSV writes %s as %q and the typed %s as %q", a, got, typed, want)
				}
				revision := value(e.readRevision(rev, ReadQuery{Tab: "Sheet1", Range: a}))
				if want := value(e.readRevision(rev, ReadQuery{Tab: "Sheet1", Range: typed})); revision != want || revision != live {
					t.Errorf("revision %d reads %s as %s and the typed %s as %s; the sheet reads it as %s", rev, a, revision, typed, want, live)
				}
			}
			// A clear of the cells and its undo, whose payload is in a scratch
			// file, give each filled cell its content again.
			bob := e.person("Bob")
			if ack := bob.do(e.clearOp("Sheet1", 1, lastRow, 1, cols)); status(ack) != "applied" {
				t.Fatalf("the clear: %s", status(ack))
			}
			if depth, _ := bob.undoDepth(); bob.spilled(depth-1) == nil {
				t.Fatal("the payload of the clear is not in a scratch file")
			}
			if u := undo(t, bob); u.Applied == 0 || u.Skipped != 0 {
				t.Fatalf("the undo of the clear: %+v", u)
			}
			for _, c := range filled {
				if got := e.contentOf(addr(c)); !sameContent(got, contents[c]) {
					t.Errorf("the undo read %s back from its scratch file as %+v, want %+v", addr(c), got, contents[c])
				}
			}

			// A sort of a column with the filled cells and their typed copies
			// puts each filled time with the typed time it shows.
			for _, col := range timeCols {
				if ack := bob.do(e.sortOp("Sheet1", 1, typedRows+lastRow, col+1, false)); status(ack) != "applied" {
					t.Fatalf("the sort by column %s: %s", formula.ColumnName(int32(col)), status(ack))
				}
				prev := math.Inf(-1)
				for row := 1; row <= typedRows+lastRow; row++ {
					at := addr(cellRef{col, row})
					if e.kindOf(at) == formula.ContentBlank {
						continue
					}
					typed, _ := formula.ParseTyped(e.shown(at))
					if typed.Num < prev {
						t.Errorf("sorted by column %s, %s shows %s after %v", formula.ColumnName(int32(col)), at, e.shown(at), prev)
					}
					prev = typed.Num
				}
			}
		})
	}
}

// sameContent reports whether two contents are the same, numbers bit for
// bit.
func sameContent(a, b formula.Content) bool {
	return a.Kind == b.Kind && math.Float64bits(a.Num) == math.Float64bits(b.Num) && a.Text == b.Text
}
