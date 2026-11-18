package sheet

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// screen is what a browser holds of one tab: the row and column order, the
// widths, the format list and the cells, built from a snapshot and kept
// current by deltas alone. A cell is kept with the format its style index
// names in the list at the time the cell arrives, as a browser draws it.
type screen struct {
	tab        any // the id of the watched tab, as the frames carry it
	rows, cols []float64
	widths     map[string]float64
	styles     []string
	cells      map[string]string
}

// put stores a cell as it arrived, with its format in place of the index.
func (sc *screen) put(key string, cell []any) {
	cell = slices.Clone(cell)
	if len(cell) > 5 {
		index := int(cell[5].(float64))
		if index <= 0 || index >= len(sc.styles) {
			cell[5] = fmt.Sprintf("style %d of %d", index, len(sc.styles))
		} else {
			cell[5] = sc.styles[index]
		}
	}
	data, _ := json.Marshal(cell)
	sc.cells[key] = string(data)
}

func (sc *screen) addStyles(list any) {
	formats, _ := list.([]any)
	for _, f := range formats {
		data, _ := json.Marshal(f)
		sc.styles = append(sc.styles, string(data))
	}
}

// brief returns the kind, the value and the format of a cell.
func (sc *screen) brief(key string) string {
	var cell []any
	if json.Unmarshal([]byte(sc.cells[key]), &cell) != nil || len(cell) < 2 {
		return ""
	}
	parts := []string{"blank"}
	if kind := cell[1].(string); kind != "" {
		parts[0] = kind
	}
	if len(cell) > 2 && cell[2] != nil {
		parts = append(parts, fmt.Sprint(cell[2]))
	}
	if len(cell) > 5 {
		parts = append(parts, cell[5].(string))
	}
	return strings.Join(parts, " ")
}

// diff says how the screen differs from another, or nothing.
func (sc *screen) diff(want *screen) string {
	switch {
	case !slices.Equal(sc.rows, want.rows):
		return "the row order differs from a fresh snapshot"
	case !slices.Equal(sc.cols, want.cols):
		return "the column order differs from a fresh snapshot"
	case fmt.Sprint(sc.widths) != fmt.Sprint(want.widths):
		return fmt.Sprintf("the widths are %v, a fresh snapshot has %v", sc.widths, want.widths)
	case !slices.Equal(sc.styles, want.styles):
		return fmt.Sprintf("the format list is %v, a fresh snapshot has %v", sc.styles, want.styles)
	case len(sc.cells) != len(want.cells):
		return fmt.Sprintf("it holds %d cells, a fresh snapshot %d", len(sc.cells), len(want.cells))
	}
	for key, cell := range want.cells {
		if sc.cells[key] != cell {
			return fmt.Sprintf("cell %s is %s, a fresh snapshot has %s", key, sc.cells[key], cell)
		}
	}
	return ""
}

func expandRuns(runs any) []float64 {
	var out []float64
	for _, run := range runs.([]any) {
		pair := run.([]any)
		for i := 0.0; i < pair[1].(float64); i++ {
			out = append(out, pair[0].(float64)+i)
		}
	}
	return out
}

func newScreen(head map[string]any, cells map[string][]any) *screen {
	sc := &screen{tab: head["tab"], rows: expandRuns(head["rows"]), cols: expandRuns(head["cols"]), widths: map[string]float64{}, cells: map[string]string{}}
	widths, _ := head["widths"].(map[string]any)
	for col, px := range widths {
		sc.widths[col] = px.(float64)
	}
	sc.addStyles(head["styles"])
	for key, cell := range cells {
		sc.put(key, cell)
	}
	return sc
}

func (sc *screen) apply(d map[string]any) {
	sc.addStyles(d["styles"])
	edits, _ := d["struct"].([]any)
	for _, raw := range edits {
		edit := raw.(map[string]any)
		// The list holds the edits of every tab. Those of other tabs
		// change nothing on this screen.
		if edit["tab"] != sc.tab {
			continue
		}
		// drop forgets the cells of rows or columns that are gone.
		drop := func(gone []float64, part int) {
			for key := range sc.cells {
				id, _ := strconv.ParseFloat(strings.Split(key, "/")[part], 64)
				if slices.Contains(gone, id) {
					delete(sc.cells, key)
				}
			}
		}
		switch edit["op"] {
		case "insert_rows", "insert_cols":
			axis := &sc.rows
			if edit["op"] == "insert_cols" {
				axis = &sc.cols
			}
			at := 0
			if after, ok := edit["after"].(float64); ok {
				at = slices.Index(*axis, after) + 1
			}
			*axis = slices.Insert(*axis, at, expandRuns(edit["ids"])...)
		case "delete_rows":
			gone := expandRuns(edit["ids"])
			sc.rows = slices.DeleteFunc(sc.rows, func(id float64) bool { return slices.Contains(gone, id) })
			drop(gone, 0)
		case "delete_cols":
			gone := expandRuns(edit["ids"])
			sc.cols = slices.DeleteFunc(sc.cols, func(id float64) bool { return slices.Contains(gone, id) })
			drop(gone, 1)
			for _, id := range gone {
				delete(sc.widths, fmt.Sprint(id))
			}
		case "sort":
			for i, pos := range expandRuns(edit["positions"]) {
				sc.rows[int(pos)] = edit["ids"].([]any)[i].(float64)
			}
		}
	}
	widths, _ := d["widths"].([]any)
	for _, raw := range widths {
		pair := raw.([]any)
		// A snapshot leaves the default width out.
		if sc.widths[fmt.Sprint(pair[0])] = pair[1].(float64); pair[1] == float64(defaultWidth) {
			delete(sc.widths, fmt.Sprint(pair[0]))
		}
	}
	rows, _ := d["cells"].([]any)
	for _, raw := range rows {
		pair := raw.([]any)
		for _, c := range pair[1].([]any) {
			cell := c.([]any)
			key := fmt.Sprint(pair[0], "/", cell[0])
			if len(cell) == 1 {
				delete(sc.cells, key)
				continue
			}
			sc.put(key, cell)
		}
	}
}

// TestConcurrentEditorsWithStructure is the Concurrent editing criterion on
// the engine: five editors type while one inserts rows and one sorts. Every
// edit that applied stays in the cell it was made in, formulas follow their
// rows, and every screen that only applied deltas equals a fresh snapshot.
func TestConcurrentEditorsWithStructure(t *testing.T) {
	e := start(t)
	rows := make([]string, 40)
	for i := range rows {
		rows[i] = fmt.Sprintf(`[%d,"=A%d*2"]`, (i*37)%40, i+1)
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + strings.Join(rows, ",") + `]}]}`)
	s := serve(e)
	tab := e.tabID("Sheet1")
	watchers := map[string]*client{"private": s.dial("private", `{"type":"hello","version":1,"name":"W","pending":0}`), "public": s.dial("public", helloV1)}
	screens := map[string]*screen{}
	for name, c := range watchers {
		c.next("welcome")
		screens[name] = newScreen(c.watch(1, tab))
	}

	const editors, rounds = 5, 30
	type key struct {
		row RowID
		col ColID
	}
	last := make([]map[key]string, editors)
	var wg sync.WaitGroup
	e.in.mu.RLock()
	cols := slices.Clone(e.in.wb.byID[tab].cols.ids)
	e.in.mu.RUnlock()
	for i := 0; i < editors; i++ {
		last[i] = map[key]string{}
		p := e.person(fmt.Sprint("Editor", i))
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(i)))
			for k := 0; k < rounds; k++ {
				// The editor picks a row on the screen it has and types
				// into its own column of that row.
				e.in.mu.RLock()
				t := e.in.wb.byID[tab]
				base := e.in.wb.seq
				row := RowID(t.rows.ids[rng.Intn(40)])
				e.in.mu.RUnlock()
				col := ColID(cols[2+i])
				text := fmt.Sprintf("e%d-%d", i, k)
				// Each edit also gives its cell a format, most of them
				// new to the sheet, so the format list grows under
				// concurrent actions.
				format := fmt.Sprintf(`{"bold":%v,"italic":%v,"underline":%v,"wrap":%v,"align":%q}`, k&1 == 1, k&2 == 2, k&4 == 4, k&8 == 8, alignNames[i%len(alignNames)])
				ack, err := p.try(base, fmt.Sprintf(`{"op":"set","tab":%d,"row":%d,"col":%d,"text":%q}`, tab, row, col, text),
					fmt.Sprintf(`{"op":"format","tab":%d,"rows":[[%d,1]],"cols":[[%d,1]],"set":%s}`, tab, row, col, format))
				if err != nil || status(ack) != "applied,applied" {
					panic(fmt.Sprintf("editor %d: %v %s", i, err, status(ack)))
				}
				last[i][key{row, col}] = text
			}
		}(i)
	}
	inserter, sorter := e.person("Inserter"), e.person("Sorter")
	wg.Add(2)
	go func() {
		defer wg.Done()
		for k := 0; k < 8; k++ {
			if ack := inserter.do(e.insertRowsOp("Sheet1", 1, 1)); status(ack) != "applied" {
				panic("insert: " + status(ack))
			}
		}
	}()
	go func() {
		defer wg.Done()
		for k := 0; k < 8; k++ {
			e.in.mu.RLock()
			base := e.in.wb.seq
			e.in.mu.RUnlock()
			op := fmt.Sprintf(`{"op":"sort","tab":%d,"rows":"all","by":%d,"desc":%v}`, tab, cols[0], k%2 == 0)
			if ack := sorter.act(base, op); status(ack) != "applied" {
				panic("sort: " + status(ack))
			}
		}
	}()
	wg.Wait()

	final := e.seq()
	if want := uint64(2 + editors*rounds + 16); final != want {
		t.Fatalf("sequence %d after %d actions that must all apply, want %d", final, editors*rounds+16, want)
	}
	// Every edit is in the cell it was made in.
	e.in.mu.RLock()
	wb := e.in.wb
	live := wb.byID[tab]
	for i := range last {
		for k, text := range last[i] {
			if c := live.cell(k.row, k.col); c == nil || c.content.Text != text {
				t.Errorf("editor %d: row %d holds %+v, want %q", i, k.row, c, text)
			}
		}
	}
	// Formulas moved with their rows: every row still doubles its own A.
	doubled := 0
	for _, id := range live.rows.ids {
		a, b := live.cell(RowID(id), ColID(cols[0])), live.cell(RowID(id), ColID(cols[1]))
		if a == nil {
			continue
		}
		doubled++
		if b == nil || b.val.Number() != 2*a.val.Number() {
			t.Errorf("row %d: A = %v and its formula gives %+v", id, a.val.Number(), b)
		}
	}
	e.in.mu.RUnlock()
	if doubled != 40 || live.rows.len() != 1008 {
		t.Fatalf("%d rows with data in a tab of %d rows", doubled, live.rows.len())
	}
	// Every screen that applied only deltas equals a fresh snapshot.
	fresh := s.dial("private", `{"type":"hello","version":1,"name":"F","pending":0}`)
	fresh.next("welcome")
	want := newScreen(fresh.watch(1, tab))
	for name, c := range watchers {
		sc := screens[name]
		seq := uint64(2)
		for seq < final {
			d := c.next("delta")
			next, _ := strconv.ParseUint(d["seq"].(string), 10, 64)
			if next != seq+1 {
				t.Fatalf("%s screen: delta %d after %d", name, next, seq)
			}
			seq = next
			sc.apply(d)
		}
		if diff := sc.diff(want); diff != "" {
			t.Errorf("%s screen: %s", name, diff)
		}
	}
	if len(want.styles) < 20 {
		t.Errorf("the editors left %d formats, too few to check the format list", len(want.styles))
	}
}
