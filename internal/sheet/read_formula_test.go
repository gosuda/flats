package sheet

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// longA1Sum is a formula of 8,190 characters that reads A1 2,730 times.
// Writing its A1 text works 2,730 references out of the stored text of a
// capture's text form, in arrays of hundreds of KiB.
var longA1Sum = "=" + strings.TrimSuffix(strings.Repeat("A1+", 2730), "+")

// formulaCursors writes longA1Sum into A2 under a number in A1 and takes n
// captures of A1:A2 a cell a page. Each returns the cursor of its second
// page, which starts at the formula.
func formulaCursors(t *testing.T, e *env, n int) []string {
	t.Helper()
	e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1],[%q]]}]}`, longA1Sum))
	cursors := make([]string, n)
	for i := range cursors {
		data, err := e.in.Read(context.Background(), ReadQuery{Tab: "Sheet1", Range: "A1:A2", Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		var start string
		if start, cursors[i] = pageStart(t, data); start != "A1" || cursors[i] == "" {
			t.Fatalf("the first page starts at %q and has the cursor %q", start, cursors[i])
		}
	}
	return cursors
}

// textWork returns the bytes a page reserves for writing the A1 text of
// longA1Sum, and the bytes writing it allocates.
func textWork(t *testing.T) (reserved, allocates int64) {
	t.Helper()
	f, err := formula.Parse(longA1Sum, formula.Pos{Row: 1}, formula.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	form := f.TextForm()
	before := allocated()
	text := form.Text(formula.Pos{Row: 1})
	allocates = allocated() - before
	if text != longA1Sum {
		t.Fatalf("the text form writes %.40q", text)
	}
	return form.TextWork(), allocates
}

// TestFormulaTextOfAPageNeedsRoom reads the page of a kept capture that
// starts at a long formula while other captures leave no room, or room
// for the page's buffers but not for the formula's A1 text. The page is
// refused before the text is worked out: it allocates a small part of
// what writing the text does, and it leaves nothing charged. Once the
// budget has room, the page carries the formula as it was typed.
func TestFormulaTextOfAPageNeedsRoom(t *testing.T) {
	e := start(t, unhurried)
	cursors := formulaCursors(t, e, 1)
	kept := captureBytes.Load()
	reserved, allocates := textWork(t)
	t.Logf("writing the formula's text allocates %d KiB, and a page reserves %d KiB for it", allocates>>10, reserved>>10)
	if measures && (allocates < 256<<10 || reserved < allocates) {
		t.Fatalf("writing the formula's text allocates %d bytes, and a page reserves %d for it", allocates, reserved)
	}
	ctx := context.Background()
	for _, free := range []int64{0, reserved / 2, reserved - 1} {
		release := fillCaptures(t, free)
		full := captureBytes.Load()
		before := allocated()
		_, err := e.in.Read(ctx, ReadQuery{Cursor: cursors[0]})
		spent := allocated() - before
		e.wantRefusal(err, "unavailable", "capture_budget")
		if got := captureBytes.Load(); got != full {
			t.Fatalf("with %d bytes free: the refused page leaves %d bytes charged", free, got-full)
		}
		t.Logf("with %d KiB free: the refused page allocated %d KiB", free>>10, spent>>10)
		if measures && spent > 64<<10 {
			t.Errorf("with %d KiB free: the refused page allocated %d KiB", free>>10, spent>>10)
		}
		release()
	}
	data, err := e.in.Read(ctx, ReadQuery{Cursor: cursors[0]})
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Start string `json:"start"`
		Rows  [][]struct {
			Formula string `json:"formula"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &page); err != nil {
		t.Fatal(err)
	}
	if page.Start != "A2" || len(page.Rows) != 1 || len(page.Rows[0]) != 1 || page.Rows[0][0].Formula != longA1Sum {
		t.Fatalf("the page starts at %q and holds %d rows", page.Start, len(page.Rows))
	}
	if got := captureBytes.Load(); got != kept {
		t.Fatalf("after the page %d bytes are charged beside the kept capture", got-kept)
	}
	e.in.captures.clear()
}

// TestOverlappingFormulaTextsShareTheBudget holds a page of one capture
// once it has written the A1 text of a long formula. What the text took
// to write stays charged until the page has encoded the cell, so a page
// of another capture that would need more than the room left is refused
// before it works its text out, until the first page is done.
func TestOverlappingFormulaTextsShareTheBudget(t *testing.T) {
	e := start(t, unhurried)
	cursors := formulaCursors(t, e, 2)
	reserved, _ := textWork(t)
	ctx := context.Background()
	kept := captureBytes.Load()
	base := steadyHeap()
	entered, proceed := make(chan struct{}), make(chan struct{})
	hookOnce(e, map[string]func(){"formula_text": func() {
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
		t.Fatal("the page was never held while it wrote the formula")
	}
	charged := captureBytes.Load() - kept
	if charged < reserved {
		close(proceed)
		t.Fatalf("a page that wrote the formula's text is charged %d bytes, %d for the text", charged, reserved)
	}
	if measures {
		held := liveHeap() - base
		t.Logf("the page that wrote the formula's text holds %d KiB of heap and is charged %d KiB", held>>10, charged>>10)
		if held > charged+256<<10 {
			close(proceed)
			t.Fatalf("the page holds %d KiB of heap, more than the %d KiB it is charged", held>>10, charged>>10)
		}
	}
	release := fillCaptures(t, reserved-1)
	before := allocated()
	_, err := e.in.Read(ctx, ReadQuery{Cursor: cursors[1]})
	spent := allocated() - before
	e.wantRefusal(err, "unavailable", "capture_budget")
	if measures && spent > 64<<10 {
		t.Errorf("the refused page of the other capture allocated %d KiB", spent>>10)
	}
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
	if second, _ := pageStart(t, data); first != "A2" || second != "A2" {
		t.Fatalf("the pages of the two captures start at %q and %q", first, second)
	}
	e.in.captures.clear()
}

// TestFullPageLeavesTheNextFormulaAlone reads A1:B1 a cell a page, with a
// number in A1 and a long formula in B1. The first page is full once it
// holds A1, so it ends there without working out B1's A1 text: with room
// for the page but not for that text it still returns A1 and a cursor, and
// with room it never writes the text. The page from the cursor needs the
// room and then carries the formula.
func TestFullPageLeavesTheNextFormulaAlone(t *testing.T) {
	e := start(t, unhurried)
	e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,%q]]}]}`, longA1Sum))
	reserved, _ := textWork(t)
	ctx := context.Background()
	for _, pressed := range []bool{true, false} {
		var wrote atomic.Bool
		hookOnce(e, map[string]func(){"formula_text": func() { wrote.Store(true) }})
		release := func() {}
		if pressed {
			release = fillCaptures(t, reserved-1)
		}
		data, err := e.in.Read(ctx, ReadQuery{Tab: "Sheet1", Range: "A1:B1", Limit: 1})
		if err != nil {
			release()
			t.Fatalf("pressed %v: %v", pressed, err)
		}
		var page struct {
			Rows [][]json.RawMessage `json:"rows"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			t.Fatal(err)
		}
		start, cursor := pageStart(t, data)
		if start != "A1" || cursor == "" || len(page.Rows) != 1 || len(page.Rows[0]) != 1 || string(page.Rows[0][0]) != "1" {
			release()
			t.Fatalf("pressed %v: the first page is %s", pressed, data)
		}
		if wrote.Load() {
			t.Errorf("pressed %v: the first page worked out the A1 text of B1", pressed)
		}
		if pressed {
			_, err := e.in.Read(ctx, ReadQuery{Cursor: cursor})
			e.wantRefusal(err, "unavailable", "capture_budget")
		}
		release()
		data, err = e.in.Read(ctx, ReadQuery{Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		var next struct {
			Start string `json:"start"`
			Rows  [][]struct {
				Formula string `json:"formula"`
			} `json:"rows"`
		}
		if err := json.Unmarshal(data, &next); err != nil {
			t.Fatal(err)
		}
		if next.Start != "B1" || len(next.Rows) != 1 || len(next.Rows[0]) != 1 || next.Rows[0][0].Formula != longA1Sum {
			t.Fatalf("pressed %v: the page from the cursor starts at %q and holds %d rows", pressed, next.Start, len(next.Rows))
		}
		e.in.captures.clear()
	}
}
