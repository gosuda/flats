package sheet

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// controlColumn fills A1:A40 with texts of 32,767 control characters,
// which take six bytes each as JSON: a page holds about twenty of them,
// or ten with their shown text.
func controlColumn(e *env) {
	e.t.Helper()
	text := strings.Repeat(`\u0001`, 32767)
	for row := 1; row <= 40; row += 20 {
		list := make([]string, 20)
		for i := range list {
			list[i] = `["` + text + `"]`
		}
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[%s]}]}`, row, strings.Join(list, ",")))
	}
}

// pageStart returns the start of a page and the cursor of the next one.
func pageStart(t *testing.T, data json.RawMessage) (start, cursor string) {
	t.Helper()
	var out struct {
		Start string `json:"start"`
		Next  struct {
			Cursor string `json:"cursor"`
		} `json:"next"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out.Start, out.Next.Cursor
}

// TestJSONPageOfAKeptCaptureNeedsRoom reads the second page of a kept
// JSON capture while other captures hold the whole budget. Building the
// page is refused with nothing charged and the cursor kept, and the same
// page comes once the budget has room.
func TestJSONPageOfAKeptCaptureNeedsRoom(t *testing.T) {
	e := start(t, unhurried)
	controlColumn(e)
	ctx := context.Background()
	for _, include := range [][]string{nil, {"shown", "formats"}} {
		data, err := e.in.Read(ctx, ReadQuery{Tab: "Sheet1", Include: include})
		if err != nil {
			t.Fatal(err)
		}
		_, cursor := pageStart(t, data)
		if cursor == "" {
			t.Fatal("the read fits one page")
		}
		release := fillCaptures(t, 0)
		full := captureBytes.Load()
		_, err = e.in.Read(ctx, ReadQuery{Cursor: cursor})
		e.wantRefusal(err, "unavailable", "capture_budget")
		if got := captureBytes.Load(); got != full {
			t.Fatalf("include %v: the refused page leaves %d bytes charged", include, got-full)
		}
		release()
		data, err = e.in.Read(ctx, ReadQuery{Cursor: cursor})
		if err != nil {
			t.Fatalf("include %v: the page after the budget had room: %v", include, err)
		}
		if start, _ := pageStart(t, data); start == "A1" || start == "" {
			t.Fatalf("include %v: the second page starts at %q", include, start)
		}
		e.in.captures.clear()
	}
}

// TestOverlappingJSONPagesShareTheBudget builds a page of one capture and
// holds it while it is built. What the page holds of the heap then is
// charged to the capture budget, and a page of another capture that
// would need more than the room left is refused, until the first page is
// done.
func TestOverlappingJSONPagesShareTheBudget(t *testing.T) {
	e := start(t, unhurried)
	controlColumn(e)
	ctx := context.Background()
	var cursors [2]string
	for i := range cursors {
		data, err := e.in.Read(ctx, ReadQuery{Tab: "Sheet1", Include: []string{"shown"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, cursors[i] = pageStart(t, data); cursors[i] == "" {
			t.Fatal("the read fits one page")
		}
	}
	kept := captureBytes.Load()
	base := steadyHeap()
	entered, proceed := make(chan struct{}), make(chan struct{})
	hookOnce(e, map[string]func(){"page_built": func() {
		close(entered)
		<-proceed
	}})
	type result struct {
		page json.RawMessage
		err  error
	}
	done := make(chan result, 1)
	go func() {
		page, err := e.in.Read(ctx, ReadQuery{Cursor: cursors[0]})
		done <- result{page, err}
	}()
	select {
	case <-entered:
	case <-time.After(time.Minute):
		t.Fatal("the page was never held while it was built")
	}
	charged := captureBytes.Load() - kept
	// A page is about 4 MB of rows and as much of shown text.
	if charged < 4<<20 {
		close(proceed)
		t.Fatalf("a page under construction is charged %d bytes", charged)
	}
	if measures {
		held := liveHeap() - base
		t.Logf("the page under construction holds %d KiB of heap and is charged %d KiB", held>>10, charged>>10)
		if held > charged+256<<10 {
			close(proceed)
			t.Fatalf("the page under construction holds %d KiB of heap, more than the %d KiB it is charged", held>>10, charged>>10)
		}
	}
	release := fillCaptures(t, charged/2)
	_, err := e.in.Read(ctx, ReadQuery{Cursor: cursors[1]})
	e.wantRefusal(err, "unavailable", "capture_budget")
	release()
	close(proceed)
	res := <-done
	if res.err != nil {
		t.Fatal(res.err)
	}
	if got := captureBytes.Load(); got != kept {
		t.Fatalf("after the page %d bytes are charged beside the kept captures", got-kept)
	}
	data, err := e.in.Read(ctx, ReadQuery{Cursor: cursors[1]})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := pageStart(t, res.page)
	if second, _ := pageStart(t, data); first != second || first == "A1" {
		t.Fatalf("the pages of the two captures start at %q and %q", first, second)
	}
	e.in.captures.clear()
}

// TestPageFormatMapCharge fills a map of the lengths of formats as a page
// does, up to an entry for every cell of a page, and after each entry
// compares what the map allocated, old groups included, with what a page
// counts for it.
func TestPageFormatMapCharge(t *testing.T) {
	if !measures {
		t.Skip("the race detector allocates beside the map")
	}
	runtime.GC()
	before := allocated()
	m := map[int32]int{}
	for i := 0; i < maxPageCells; i++ {
		m[int32(i)] = i
		if spent, charge := allocated()-before, int64(formatLenBase+len(m)*formatLenEntry); spent > charge {
			t.Fatalf("a map of %d formats allocated %d bytes and is counted %d", len(m), spent, charge)
		}
	}
}

// TestAgentCellMax encodes cells of every kind at the ends of what they
// may hold, texts of characters that JSON writes in six bytes or that take
// four bytes themselves, and checks that none passes the room a page
// makes for it before it encodes it.
func TestAgentCellMax(t *testing.T) {
	control, wide := strings.Repeat("\x01", formula.MaxTextLen), strings.Repeat("\U0001F600", formula.MaxTextLen)
	var cells []capCell
	for _, num := range []float64{0, -1.7976931348623157e308, 5e-324, -1e20 - 0.5, -123456789012345.6, -9.87654321e-7, 2958465.99999} {
		for _, kind := range []formula.ContentKind{formula.ContentNumber, formula.ContentDate, formula.ContentTime, formula.ContentDateTime} {
			cells = append(cells, capCell{content: formula.Content{Kind: kind, Num: num}, val: formula.NumberValue(num)})
		}
	}
	cells = append(cells, capCell{content: formula.Content{Kind: formula.ContentBool, Num: 1}, val: formula.BoolValue(true)})
	for _, text := range []string{control, wide, "=" + control[1:], "'" + wide[4:], `"\`} {
		cells = append(cells, capCell{content: formula.Content{Kind: formula.ContentText, Text: text}, val: formula.TextValue(text)})
	}
	f, err := formula.Parse(`="`+strings.Repeat("\x01", formula.MaxFormulaChars-3)+`"`, formula.Pos{}, formula.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	values := []formula.Value{formula.TextValue(control), formula.TextValue(wide), formula.NumberValue(-1.7976931348623157e308), formula.BoolValue(false)}
	for e := formula.ErrSyntax; e <= formula.ErrLimit; e++ {
		values = append(values, formula.ErrorValue(e))
	}
	for _, v := range values {
		for _, kind := range []formula.ContentKind{formula.ContentFormula, formula.ContentUnreadable} {
			cells = append(cells, capCell{content: formula.Content{Kind: kind, Text: f.Stored()}, f: f.TextForm(), val: v})
		}
	}
	at := formula.Pos{Row: 1048575, Col: 16383}
	if got := len(appendAgentCell(nil, nil, "")); got > agentCellMax(nil, "") {
		t.Errorf("a blank cell takes %d bytes and is given %d", got, agentCellMax(nil, ""))
	}
	for i := range cells {
		cell := &cells[i]
		text := agentText(cell, at)
		if got, room := len(appendAgentCell(nil, cell, text)), agentCellMax(cell, text); got > room {
			t.Errorf("a cell of kind %d with a value of kind %d takes %d bytes and is given %d", cell.content.Kind, cell.val.Kind(), got, room)
		}
	}
}

// TestCaptureFormulaMapCharge fills a map of the formulas a capture keeps
// as a capture does while it is built, up to the formulas a sheet may
// hold, and after each entry compares what the map allocated, old groups
// included, with what the capture counts for it.
func TestCaptureFormulaMapCharge(t *testing.T) {
	if !measures {
		t.Skip("the race detector allocates beside the map")
	}
	keys := make([]formula.Formula, maxFormulas)
	runtime.GC()
	before := allocated()
	forms := map[*formula.Formula]*formula.Formula{}
	for i := range keys {
		forms[&keys[i]] = &keys[i]
		if spent, charge := allocated()-before, int64(capFormsBytes+len(forms)*capFormulaBytes); spent > charge {
			t.Fatalf("a map of %d formulas allocated %d bytes and is counted %d", len(forms), spent, charge)
		}
	}
}
