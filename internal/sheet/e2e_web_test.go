//go:build unix

package sheet

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// TestBrowserScreenState drives the real client in a headless Chrome
// against a real instance, for the cases where what the screen shows must
// stay bound to one state: row heights under sideways scrolling, a formula
// that names another tab while that tab changes, a refused width, and the
// live sheet moving under an open revision. Like TestBrowser it runs only
// with FLATS_SHEET_E2E=1 and skips when no browser is found.
func TestBrowserScreenState(t *testing.T) {
	if os.Getenv("FLATS_SHEET_E2E") != "1" {
		t.Skip("set FLATS_SHEET_E2E=1 to run the browser test")
	}
	path := findChrome()
	if path == "" {
		t.Skip("no Chrome found; set CHROME to its executable")
	}
	e := start(t)
	e.mustAgent(`{"ops":[
		{"op":"rename_tab","tab":"Sheet1","name":"Summary"},
		{"op":"add_tab","name":"Data"},
		{"op":"write","tab":"Data","at":"A1","rows":[["h",1],["a",2],["b",3],["c",4],["d",5]]},
		{"op":"write","tab":"Summary","at":"A2","rows":[["two\nlines","plain"]]},
		{"op":"write","tab":"Summary","at":"Z4","rows":[["far\nto the\nright"]]},
		{"op":"write","tab":"Summary","at":"Y6","rows":[["a wrapped text that needs several lines in a narrow column"]]},
		{"op":"format","tab":"Summary","ranges":["Y6"],"format":{"wrap":true}},
		{"op":"write","tab":"Summary","at":"A8","rows":[[5,null,null,"=A8*3"]]}]}`)
	b := launchChrome(t, path)
	s := &browserTest{T: t, top: t, e: e, b: b, alice: e.route(true)}
	a := b.open("Alice", s.alice.url+"/", viewport{width: 1280, height: 800})
	s.a = a
	s.enter(a, "Alice")
	s.shows(a, "B2", "plain")

	// drawn waits until the page has drawn everything it scheduled.
	drawn := func() {
		t.Helper()
		a.wait("the screen drawn", `!sheet.rendering && !sheet.grid.scheduled`)
	}
	heights := func() string {
		return a.str(`JSON.stringify([0, 1, 2, 3, 4, 5, 6, 7].map((r) => sheet.grid.heights.get(r)))`)
	}
	rowTop := func(n int) string {
		return a.str(fmt.Sprintf(`String(T.rowHead(%d).getBoundingClientRect().top)`, n))
	}

	t.Run("row heights", func(t *testing.T) {
		// Z4 and Y6 lie right of the drawn columns: their rows have their
		// heights before those cells are ever drawn.
		drawn()
		if a.truthy(`T.cellEl('Z4')`) {
			t.Fatal("Z4 is drawn at the start; the case needs it out of view")
		}
		first := heights()
		var h []float64
		a.eval(`[0, 1, 2, 3, 4, 5, 6, 7].map((r) => sheet.grid.heights.get(r))`, &h)
		if !(h[0] == 28 && h[1] > 28 && h[3] > h[1] && h[5] > h[1] && h[2] == 28) {
			t.Fatalf("row heights at the start = %s", first)
		}
		top7 := rowTop(7)
		// Scroll to the far cells: they are drawn and measured where they
		// are, and no row moves.
		a.eval(`sheet.grid.el.scrollLeft = 5000`, nil)
		a.wait("the far cells drawn", `T.cellEl('Z4') && T.cellEl('Y6') && !T.cellEl('A2')`)
		drawn()
		if got := heights(); got != first {
			t.Errorf("row heights changed with the far cells in view: %s, were %s", got, first)
		}
		if got := rowTop(7); got != top7 {
			t.Errorf("row 7 moved from %s to %s", top7, got)
		}
		if got := a.str(`String(T.cellEl('Y6').querySelector('.tx').offsetHeight + 1 <= sheet.grid.heights.get(5))`); got != "true" {
			t.Errorf("the wrapped cell does not fit its row: %s", a.str(`T.cellEl('Y6').querySelector('.tx').offsetHeight + ' in ' + sheet.grid.heights.get(5)`))
		}
		a.eval(`sheet.grid.el.scrollLeft = 0`, nil)
		a.wait("the first columns drawn", `T.cellEl('A2') && !T.cellEl('Z4')`)
		drawn()
		if got := heights(); got != first {
			t.Errorf("row heights changed after scrolling back: %s, were %s", got, first)
		}
		// Content changes a height while its column is out of view.
		e.mustAgent(`{"ops":[{"op":"write","tab":"Summary","at":"Z4","rows":[["short"]]}]}`)
		a.wait("row 4 at one line", `sheet.grid.heights.get(3) === 28`)
		a.noErrors()
	})

	t.Run("formula that names another tab", func(t *testing.T) {
		a.eval(`(() => { const net = sheet.net, send = net.rebase; globalThis.rebases = 0; net.rebase = function (...args) { globalThis.rebases++; return send.apply(this, args); }; })()`, nil)
		s.pick(a, "A1")
		a.typeText("=Data!B5+1")
		a.wait("the formula in the editor", `sheet.editor.isFormula && sheet.editor.text === '=Data!B5+1'`)
		// Another writer inserts a row above Data!B5.
		e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"Data","at":2,"count":1}]}`)
		a.wait("the adjusted text", `sheet.editor.open && sheet.editor.text === '=Data!B6+1'`)
		if got := a.str(`globalThis.rebases`); got == "0" {
			t.Error("the editor's text changed without a rebase exchange")
		}
		a.key("Enter", 0)
		s.shows(a, "A1", "6")
		s.saved(a)
		e.wantFormula("Summary", "A1", "=Data!B6+1")
		// And when the person commits before the exchange ends, the server
		// adjusts from the screen the text was typed on.
		s.pick(a, "B1")
		a.typeText("=Data!B6+2")
		a.wait("the formula in the editor", `sheet.editor.text === '=Data!B6+2'`)
		a.eval(`sheet.net.rebase = () => true`, nil) // the answer never comes
		e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"Data","at":2,"count":1}]}`)
		a.wait("the insert held", `sheet.rebaser.holding`)
		a.key("Enter", 0)
		s.shows(a, "B1", "7")
		s.saved(a)
		e.wantFormula("Summary", "B1", "=Data!B7+2")
		e.wantFormula("Summary", "A1", "=Data!B7+1")
		a.noErrors()
	})

	t.Run("refused width", func(t *testing.T) {
		before := a.str(`JSON.stringify([T.colHead('B').getBoundingClientRect().width, T.cellEl('C2').getBoundingClientRect().left, sheet.grid.size.style.width])`)
		e.b.setQuota(1)
		edge := a.at(`T.colHead('B')`)
		edge.X += 51
		a.drag(edge, point{edge.X + 200, edge.Y})
		a.wait("the refusal", `T.notices().includes('The host refused this column width')`)
		e.b.setQuota(0)
		a.wait("every edit answered", `sheet.net.pending.length === 0`)
		drawn()
		if got := a.str(`JSON.stringify([T.colHead('B').getBoundingClientRect().width, T.cellEl('C2').getBoundingClientRect().left, sheet.grid.size.style.width])`); got != before {
			t.Errorf("after the refusal the grid is at %s, the confirmed state is %s", got, before)
		}
		// A click where column C lies by the confirmed widths lands on C.
		a.click(a.at(`'C3'`))
		a.waitText("the active cell", `T.active()`, "C3")
		a.eval(`document.querySelectorAll('.toasts .toast button[aria-label="Dismiss this notice"]').forEach((b) => b.click())`, nil)
		a.noErrors()
	})

	t.Run("tab bar while a tab loads", func(t *testing.T) {
		// Every frame from the host arrives 400 ms late, as on a slow line.
		slow := `(() => { const ws = sheet.net.ws, on = ws.onmessage; globalThis.fast = () => { ws.onmessage = on; }; ws.onmessage = (ev) => setTimeout(() => on(ev), 400); })()`
		a.eval(slow, nil)
		// A double click on a tab that is not open asks for the tab and
		// opens the rename field. The tab arrives while the name is typed.
		a.doubleClick(a.at(`T.byText('.tabbar .tab', 'Data')`))
		a.wait("the rename field", `document.querySelector('.tabbar .tab input')`)
		a.eval(`document.querySelector('.tabbar .tab input').select()`, nil)
		a.typeText("Fa")
		a.wait("the tab loaded under the field", `sheet.model.tab.name === 'Data' && !sheet.model.tab.loading`)
		drawn()
		if got := a.str(`(() => { const el = document.querySelector('.tabbar .tab input'); return el ? el.value + '|' + (document.activeElement === el) : 'no field'; })()`); got != "Fa|true" {
			t.Fatalf("after the tab loaded the rename field holds %q, want the typed text with focus", got)
		}
		a.typeText("cts")
		a.key("Enter", 0)
		a.waitText("the tab bar", `T.tabs()`, "Summary,Facts")
		s.saved(a)
		if got := e.tabList(); got != "Summary,Facts" {
			t.Errorf("the sheet has the tabs %s", got)
		}
		// A click on another tab, and typing before that tab arrives: the
		// tab still on screen is not written.
		s.shows(a, "A1", "h")
		a.click(a.at(`T.byText('.tabbar .tab', 'Summary')`))
		a.typeText("zz")
		a.key("Enter", 0)
		a.wait("the Summary tab", `sheet.model.tab.name === 'Summary' && !sheet.model.tab.loading`)
		s.saved(a)
		e.wantShown("Facts", "A1", "h")
		a.eval(`fast()`, nil)
		// A click on the tab that is open asks for nothing, so what is typed
		// right after it is kept whole.
		a.click(a.at(`T.byText('.tabbar .tab', 'Summary')`))
		s.pick(a, "F1")
		a.click(a.at(`T.byText('.tabbar .tab', 'Summary')`))
		a.typeText("Train")
		a.key("Enter", 0)
		s.shows(a, "F1", "Train")
		s.saved(a)
		e.wantShown("Summary", "F1", "Train")
		a.eval(`document.querySelectorAll('.toasts .toast button[aria-label="Dismiss this notice"]').forEach((b) => b.click())`, nil)
		a.noErrors()
	})

	t.Run("revision over a moving live sheet", func(t *testing.T) {
		rev := e.b.importAll(t)
		// The live sheet moves on: the formula of D8 lies in D9 now.
		e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"Summary","at":7,"count":1}]}`)
		s.shows(a, "D9", "15")
		a.click(a.at(`T.byLabel('History')`))
		row := fmt.Sprintf(`document.querySelector('.hist .rev[data-key="rev-%d"]')`, rev)
		a.wait("the revision list", row)
		a.click(a.at(row))
		a.wait("the revision banner", fmt.Sprintf(`document.querySelector('.banners').textContent.includes('Viewing revision %d')`, rev))
		s.shows(a, "D8", "15")
		// The formula bar shows the revision's text for the revision's cell.
		a.click(a.at(`'D8'`))
		a.waitText("the formula bar", `T.bar()`, "=A8*3")
		if got := a.str(`String(sheet.net.pending.length)`); got != "0" {
			t.Errorf("reading a revision left %s actions pending", got)
		}
		// The connection starts over and the live tab arrives as a new
		// snapshot. The revision stays on screen.
		a.eval(`globalThis.heads = 0; (() => { const h = sheet.net.h, was = h.tabHead; h.tabHead = (m) => { globalThis.heads++; was(m); }; })(); sheet.net.restart({ fresh: true })`, nil)
		a.wait("the live snapshot", `globalThis.heads > 0 && sheet.net.open && sheet.model.tab && !sheet.model.tab.loading`)
		e.mustAgent(`{"ops":[{"op":"write","tab":"Summary","at":"E1","rows":[["written under the revision"]]}]}`)
		a.wait("the live delta", `sheet.model.cellAt(0, 4) && sheet.model.cellAt(0, 4).shown === 'written under the revision'`)
		drawn()
		if got := a.str(`T.shown('D8') + '|' + T.shown('D9') + '|' + T.shown('E1') + '|' + T.bar()`); got != "15|||=A8*3" {
			t.Errorf("under the revision banner the grid shows %q", got)
		}
		if !strings.Contains(a.str(`document.querySelector('.banners').textContent`), fmt.Sprintf("Viewing revision %d", rev)) {
			t.Error("the revision banner is gone")
		}
		a.click(a.at(`T.byText('.banners button', 'Back to current')`))
		s.shows(a, "D9", "15")
		s.shows(a, "D8", "")
		s.shows(a, "E1", "written under the revision")
		a.click(a.at(`'D9'`))
		a.waitText("the live formula bar", `T.bar()`, "=A9*3")
		// The restart is an expected reconnect.
		a.errors()
	})
}

// TestBrowserLargeFormatDelta sends the real client in a headless Chrome a
// delta with more new formats than a call takes arguments: every cell of
// 4,000 rows and 50 columns has a format of its own, and one write makes
// all of them bold. A second write of that size makes the host rebuild
// its format list, so a resync and a new snapshot replace the client's
// list. Like TestBrowser it runs only with FLATS_SHEET_E2E=1 and skips
// when no browser is found.
func TestBrowserLargeFormatDelta(t *testing.T) {
	if os.Getenv("FLATS_SHEET_E2E") != "1" {
		t.Skip("set FLATS_SHEET_E2E=1 to run the browser test")
	}
	path := findChrome()
	if path == "" {
		t.Skip("no Chrome found; set CHROME to its executable")
	}
	const rows, cols = 4000, 50
	e := start(t)
	last := formula.ColumnName(cols - 1)
	e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["first"]]},{"op":"write","tab":"Sheet1","at":"%s%d","rows":[["last"]]}]}`, last, rows))
	// A fill per column and a currency symbol per row.
	hues := []string{"gray", "red", "orange", "yellow", "green", "teal", "blue", "purple", "magenta"}
	shades := []int{100, 200, 300, 400, 600, 800}
	var ops []string
	for c := 0; c < cols; c++ {
		name := formula.ColumnName(int32(c))
		ops = append(ops, fmt.Sprintf(`{"op":"format","tab":"Sheet1","ranges":["%s1:%s%d"],"format":{"fill":"%s-%d"}}`, name, name, rows, hues[c%9], shades[c/9]))
	}
	e.mustAgent(`{"ops":[` + strings.Join(ops, ",") + `]}`)
	for from := 0; from < rows; from += 500 {
		ops = ops[:0]
		for r := from; r < from+500; r++ {
			symbol := string([]byte{'A' + byte(r%26), 'A' + byte(r/26%26), 'A' + byte(r/676%26)})
			ops = append(ops, fmt.Sprintf(`{"op":"format","tab":"Sheet1","ranges":["A%d:%s%d"],"format":{"number":{"kind":"currency","symbol":"%s","decimals":2}}}`, r+1, last, r+1, symbol))
		}
		e.mustAgent(`{"ops":[` + strings.Join(ops, ",") + `]}`)
	}
	b := launchChrome(t, path)
	s := &browserTest{T: t, top: t, e: e, b: b, alice: e.route(true)}
	a := b.open("Alice", s.alice.url+"/", viewport{width: 1280, height: 800})
	s.a = a
	s.enter(a, "Alice")
	s.shows(a, "A1", "first")
	const cells = rows * cols
	styles := func() int {
		var n int
		a.eval(`sheet.model.tab.styles.length`, &n)
		return n
	}
	a.wait("the tab loaded", `sheet.model.tab && !sheet.model.tab.loading`)
	if n := styles(); n <= cells {
		t.Fatalf("the client holds %d formats, and every one of %d cells has its own", n, cells)
	}
	before := styles()

	// One write gives every cell a format the list has not held.
	all := fmt.Sprintf("A1:%s%d", last, rows)
	e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"format","tab":"Sheet1","ranges":["%s"],"format":{"bold":true}}]}`, all))
	a.wait("the delta applied", fmt.Sprintf(`sheet.model.seq === "%d"`, e.seq()))
	if n := styles(); n != before+cells {
		t.Errorf("the client holds %d formats after a delta with %d new ones, and held %d before", n, cells, before)
	}
	if !a.truthy(fmt.Sprintf(`!sheet.model.stale && sheet.net.lastSeq === "%d" && sheet.model.format(sheet.model.cellAt(%d, %d)).bold === true`, e.seq(), rows-1, cols-1)) {
		t.Error("the grid is not at the delta, or its last cell is not bold")
	}
	a.wait("A1 drawn bold", `T.classes('A1').split(' ').includes('b')`)
	a.noErrors()

	// A second write of that size leaves more formats out of use than the
	// list may hold, so the host rebuilds it: the client gets a resync and
	// a snapshot, and its list is the new one.
	e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"format","tab":"Sheet1","ranges":["%s"],"format":{"italic":true}}]}`, all))
	a.wait("the rebuilt list", fmt.Sprintf(`!sheet.model.stale && !sheet.model.tab.loading && sheet.model.seq === "%d" && sheet.model.tab.styles.length <= %d`, e.seq(), cells+2))
	if !a.truthy(fmt.Sprintf(`(() => { const f = sheet.model.format(sheet.model.cellAt(%d, %d)); return f.bold === true && f.italic === true && f.number.kind === 'currency'; })()`, rows-1, cols-1)) {
		t.Error("the last cell lost its format with the list")
	}
	a.wait("A1 drawn bold and italic", `T.classes('A1').split(' ').includes('b') && T.classes('A1').split(' ').includes('it')`)
	// The stream goes on after it.
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["after"]]}]}`)
	s.shows(a, "A1", "after")
	a.noErrors()
}

// TestBrowserEscapedTextTab loads in a headless Chrome a tab whose frames
// hold more characters than one JavaScript string can: 1,600 cells of an
// apostrophe, an equals sign and 30,000 U+0001, which a frame carries as
// value and edit text at six characters a control. The live snapshot, a
// bold format over the whole range and the tab in History all reach the
// real client, which parses them a frame at a time. Like TestBrowser it
// runs only with FLATS_SHEET_E2E=1 and skips when no browser is found.
func TestBrowserEscapedTextTab(t *testing.T) {
	if os.Getenv("FLATS_SHEET_E2E") != "1" {
		t.Skip("set FLATS_SHEET_E2E=1 to run the browser test")
	}
	path := findChrome()
	if path == "" {
		t.Skip("no Chrome found; set CHROME to its executable")
	}
	const cells, chars = 1600, 30000
	e := start(t)
	text := strings.Repeat(`\u0001`, chars)
	for row := 1; row <= cells; row += 25 {
		list := make([]string, 25)
		for i := range list {
			list[i] = `["'=` + text + `"]`
		}
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[%s]}]}`, row, strings.Join(list, ",")))
	}
	rev := e.b.importAll(t)
	b := launchChrome(t, path)
	s := &browserTest{T: t, top: t, e: e, b: b, alice: e.route(true)}
	a := b.open("Alice", s.alice.url+"/", viewport{width: 1280, height: 800})
	s.a = a
	s.enter(a, "Alice")
	// Loading this much takes longer than the usual wait.
	long := func(what, expr string) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Minute)
		for !a.truthy(expr) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s (%s)\nerrors: %v", what, expr, a.errors())
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	whole := fmt.Sprintf(`((m) => m && m.tab && !m.tab.loading && m.rowCount >= %d && [0, %d].every((r) => m.cellAt(r, 0) && m.cellAt(r, 0).value.length === %d && m.cellAt(r, 0).edit.length === %d))`, cells, cells-1, chars+1, chars+2)
	long("the live tab", whole+`(sheet.model)`)

	// The frames the page parses, counted as they arrive.
	a.eval(`globalThis.parts = 0; globalThis.chars = 0; globalThis.longest = 0; (() => {
		const net = sheet.net, was = net.receive.bind(net);
		net.receive = (m, text) => {
			if (m.type === 'delta_part') { globalThis.parts++; globalThis.chars += text.length; globalThis.longest = Math.max(globalThis.longest, text.length); }
			return was(m, text);
		};
	})()`, nil)
	e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"format","tab":"Sheet1","ranges":["A1:A%d"],"format":{"bold":true}}]}`, cells))
	long("the format delta", fmt.Sprintf(`sheet.model.seq === "%d"`, e.seq()))
	var parts struct{ Parts, Chars, Longest int }
	a.eval(`({parts: globalThis.parts, chars: globalThis.chars, longest: globalThis.longest})`, &parts)
	if parts.Chars <= 1<<29 || parts.Longest > serverFrameMax {
		t.Errorf("the delta came in %d parts of %d characters, the longest %d", parts.Parts, parts.Chars, parts.Longest)
	}
	if !a.truthy(fmt.Sprintf(`!sheet.model.stale && sheet.net.lastSeq === "%d" && [0, %d].every((r) => sheet.model.format(sheet.model.cellAt(r, 0)).bold === true)`, e.seq(), cells-1)) {
		t.Error("the grid is not at the format delta, or its cells are not bold")
	}
	if got := a.str(`sheet.net.failures`); got != "0" {
		t.Errorf("the connection ended %s times", got)
	}
	a.noErrors()

	a.click(a.at(`T.byLabel('History')`))
	row := fmt.Sprintf(`document.querySelector('.hist .rev[data-key="rev-%d"]')`, rev)
	a.wait("the revision list", row)
	a.click(a.at(row))
	long("the revision tab", fmt.Sprintf(`sheet.view && sheet.view.revision === %d && %s(sheet.view.model)`, rev, whole))
	if a.truthy(`document.querySelector('.hist [role="alert"]')`) {
		t.Errorf("History shows %q", a.str(`document.querySelector('.hist [role="alert"]').textContent`))
	}
	if a.truthy(`sheet.view.model.format(sheet.view.model.cellAt(0, 0)).bold`) {
		t.Error("the revision shows the format made after it")
	}
	// The host answers that it calculates the revision until it has.
	for _, msg := range a.errors() {
		if !strings.Contains(msg, "503 (Service Unavailable)") || !strings.Contains(msg, fmt.Sprintf("/_sheet/api/revisions/%d", rev)) {
			t.Errorf("Alice reported %s", msg)
		}
	}
}
