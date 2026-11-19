//go:build unix

package sheet

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// muteListener is the listener of one route of the browser test. Its
// connections can lose what the server writes, and can all be dropped, so
// a test can leave an action applied and unconfirmed.
type muteListener struct {
	net.Listener
	mute  atomic.Bool
	mu    sync.Mutex
	conns map[*muteConn]struct{}
}

type muteConn struct {
	net.Conn
	l *muteListener
}

func (l *muteListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	mc := &muteConn{Conn: c, l: l}
	l.mu.Lock()
	l.conns[mc] = struct{}{}
	l.mu.Unlock()
	return mc, nil
}

func (c *muteConn) Write(p []byte) (int, error) {
	if c.l.mute.Load() {
		return len(p), nil
	}
	return c.Conn.Write(p)
}

func (c *muteConn) Close() error {
	c.l.mu.Lock()
	delete(c.l.conns, c)
	c.l.mu.Unlock()
	return c.Conn.Close()
}

// drop closes every connection of the route and lets new ones through.
func (l *muteListener) drop() {
	l.mu.Lock()
	conns := make([]*muteConn, 0, len(l.conns))
	for c := range l.conns {
		conns = append(conns, c)
	}
	l.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
	l.mute.Store(false)
}

// route is one listener in front of the instance. It makes the decision
// core makes for its route: it strips what a client must not send and sets
// the trusted access headers.
type route struct {
	url       string
	listener  *muteListener
	withdrawn atomic.Bool
}

func (e *env) route(private bool) *route {
	e.t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		e.t.Fatal(err)
	}
	rt := &route{listener: &muteListener{Listener: ln, conns: map[*muteConn]struct{}{}}}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rt.withdrawn.Load() {
			http.NotFound(w, r)
			return
		}
		for _, name := range []string{"X-Flats-Access", "X-Flats-Access-Epoch", "X-Flats-Public-Access", "Tailscale-User-Login", "Tailscale-User-Name"} {
			r.Header.Del(name)
		}
		if private {
			r.Header.Set("X-Flats-Access", "private")
		}
		r.Header.Set("X-Flats-Access-Epoch", "1")
		e.in.ServeHTTP(w, r)
	}))
	server.Listener = rt.listener
	server.Start()
	e.t.Cleanup(server.Close)
	rt.url = server.URL
	return rt
}

func (rt *route) get(t *testing.T, path string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(rt.url + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

// wantShown compares what the cells of a range show, row by row with "|"
// between cells. A read ends at the last filled cell of the tab.
func (e *env) wantShown(tab, rng string, want ...string) {
	e.t.Helper()
	var got []string
	for _, row := range e.readSheet(ReadQuery{Tab: tab, Range: rng, Include: []string{"shown"}}).Shown {
		got = append(got, strings.Join(row, "|"))
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		e.t.Errorf("%s!%s shows\n%s\nwant\n%s", tab, rng, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// content is the content of one cell as a read returns it: a JSON value,
// or a formula object.
func (e *env) content(tab, addr string) string {
	e.t.Helper()
	rows := e.readSheet(ReadQuery{Tab: tab, Range: addr}).Rows
	if len(rows) == 0 || len(rows[0]) == 0 {
		return "null"
	}
	return string(rows[0][0])
}

func (e *env) wantFormula(tab, addr, formula string) {
	e.t.Helper()
	var cell struct {
		Formula string `json:"formula"`
	}
	got := e.content(tab, addr)
	if json.Unmarshal([]byte(got), &cell) != nil || cell.Formula != formula {
		e.t.Errorf("%s!%s holds %s, want the formula %s", tab, addr, got, formula)
	}
}

func (e *env) tabList() string {
	e.t.Helper()
	var names []string
	for _, t := range e.readSheet(ReadQuery{}).Tabs {
		names = append(names, t.Name)
	}
	return strings.Join(names, ",")
}

// TestBrowser drives the real client in a headless Chrome against a real
// instance: a Private route for each of two people and a Public route. It
// runs only with FLATS_SHEET_E2E=1 and skips when no browser is found.
// FLATS_SHEET_E2E_SHOTS names a directory for screenshots.
func TestBrowser(t *testing.T) {
	if os.Getenv("FLATS_SHEET_E2E") != "1" {
		t.Skip("set FLATS_SHEET_E2E=1 to run the browser test")
	}
	path := findChrome()
	if path == "" {
		t.Skip("no Chrome found; set CHROME to its executable")
	}
	e := start(t)
	e.mustAgent(`{"message":"Offsite budget","ops":[
		{"op":"rename_tab","tab":"Sheet1","name":"Budget"},
		{"op":"write","tab":"Budget","at":"A1","rows":[
			["Item","Qty","Price","Total"],
			["Venue",2,1800,"=B2*C2"],
			["Lunch",24,32,"=B3*C3"],
			["Bus",2,420,"=B4*C4"],
			["Total",null,null,"=SUM(D2:D4)"]]},
		{"op":"format","tab":"Budget","ranges":["A1:D1"],"format":{"bold":true}},
		{"op":"format","tab":"Budget","ranges":["C2:D5"],"format":{"number":{"kind":"currency","symbol":"$","decimals":2}}},
		{"op":"add_tab","name":"Notes"},
		{"op":"write","tab":"Notes","at":"A1","rows":[["Bring a projector"]]}]}`)
	b := launchChrome(t, path)
	s := &browserTest{T: t, top: t, e: e, b: b, alice: e.route(true), bob: e.route(true), public: e.route(false), shots: os.Getenv("FLATS_SHEET_E2E_SHOTS")}
	wide := viewport{width: 1280, height: 800}
	s.a = b.open("Alice", s.alice.url+"/", wide)
	s.enter(s.a, "Alice")

	for _, c := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"load", s.load},
		{"typing", s.typing},
		{"navigation", s.navigation},
		{"drag scroll", s.dragScroll},
		{"second page", s.secondPage},
		{"rows", s.rows},
		{"sort and undo", s.sortUndo},
		{"format", s.format},
		{"tabs", s.tabs},
		{"find", s.find},
		{"copy and paste", s.copyPaste},
		{"formula in flight", s.formulaInFlight},
		{"agent write", s.agentWrite},
		{"refusal", s.refusal},
		{"history", s.history},
		{"reconnect", s.reconnect},
		{"public", s.publicRoute},
		{"phone", s.phone},
		{"screenshots", s.screenshots},
		{"withdrawn", s.withdrawn},
	} {
		ok := t.Run(c.name, func(t *testing.T) {
			// A failure belongs to the case that found it.
			s.T, b.t, e.t = t, t, t
			defer func() { s.T, b.t, e.t = s.top, s.top, s.top }()
			c.run(t)
		})
		if !ok {
			// Every case builds on the sheet the earlier ones left.
			break
		}
		for _, p := range []*page{s.a, s.bo, s.pub, s.ph} {
			if p != nil {
				p.noErrors()
			}
		}
	}
}

type browserTest struct {
	*testing.T
	top                *testing.T
	e                  *env
	b                  *chrome
	alice, bob, public *route
	a, bo, pub, ph     *page
	shots              string
}

// enter gives the name the Private route asks for on first opening.
func (s *browserTest) enter(p *page, name string) {
	s.Helper()
	p.wait("the name dialog", `document.querySelector('#me-name')`)
	p.typeText(name)
	p.key("Enter", 0)
	p.wait("the welcome with the name", fmt.Sprintf(`globalThis.sheet && sheet.net.me && sheet.net.me.label === %q && sheet.model.tab && !sheet.model.tab.loading`, name))
}

func (s *browserTest) load(t *testing.T) {
	a := s.a
	resp, shell := s.alice.get(t, "/")
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "style-src 'self'") {
		t.Fatalf("shell CSP = %q", csp)
	}
	if !strings.Contains(string(shell), `id="app"`) {
		t.Fatalf("shell = %s", shell)
	}
	a.waitText("A1", `T.shown('A1')`, "Item")
	for addr, want := range map[string]string{"B2": "2", "C2": "$1,800.00", "D2": "$3,600.00", "D3": "$768.00", "D4": "$840.00", "D5": "$5,208.00", "A5": "Total"} {
		a.waitText(addr, fmt.Sprintf(`T.shown(%q)`, addr), want)
	}
	if got := a.str(`T.tabs()`); got != "Budget,Notes" {
		t.Errorf("tab bar = %q", got)
	}
	if !strings.Contains(a.str(`T.classes('A1')`), " b") {
		t.Errorf("A1 is not drawn bold: %q", a.str(`T.classes('A1')`))
	}
	// The styles arrived and positions set through the CSSOM hold under
	// the shell's policy.
	if got := a.str(`getComputedStyle(document.querySelector('.gridwrap')).overflow`); !strings.Contains(got, "auto") && !strings.Contains(got, "scroll") {
		t.Errorf("the grid's overflow is %q: the stylesheet did not apply", got)
	}
	if got := a.str(`T.cellEl('B2').style.left`); got == "" {
		t.Error("a cell has no position: the CSSOM write did not apply")
	}
	if got := a.str(`sheet.editor.functions.length`); got != "104" {
		t.Errorf("the client loaded %s functions, want 104", got)
	}
	a.waitText("the save status", `T.status()`, "Saved")
	a.noErrors()
}

func (s *browserTest) pick(p *page, addr string) {
	s.Helper()
	p.click(p.at(fmt.Sprintf("%q", addr)))
	p.waitText("the active cell", `T.active()`, addr)
}

func (s *browserTest) shows(p *page, addr, want string) {
	s.Helper()
	p.waitText(addr, fmt.Sprintf(`T.shown(%q)`, addr), want)
}

// saved waits until the server has confirmed every edit of the page.
func (s *browserTest) saved(p *page) {
	s.Helper()
	p.wait("every edit confirmed", `sheet.net.pending.length === 0 && T.status() === 'Saved'`)
}

// menuItem clicks a command of the open menu.
func (s *browserTest) menuItem(p *page, label string) {
	s.Helper()
	item := fmt.Sprintf(`T.byText('.menu button', %q)`, label)
	p.wait("the menu item "+label, item)
	p.click(p.at(item))
}

func (s *browserTest) typing(t *testing.T) {
	a, e := s.a, s.e
	s.pick(a, "B2")
	a.typeText("3")
	a.key("Enter", 0)
	s.shows(a, "B2", "3")
	s.shows(a, "D2", "$5,400.00")
	s.shows(a, "D5", "$7,008.00")
	a.waitText("the active cell after Enter", `T.active()`, "B3")
	s.saved(a)
	e.wantShown("Budget", "B2:D2", "3|$1,800.00|$5,400.00")
	e.wantShown("Budget", "D5", "$7,008.00")

	s.pick(a, "E2")
	a.typeText("=B2*2")
	a.wait("the function hint or the editor", `sheet.editor.open && sheet.editor.text === '=B2*2'`)
	a.key("Tab", 0)
	s.shows(a, "E2", "6")
	a.waitText("the active cell after Tab", `T.active()`, "F2")
	s.saved(a)
	e.wantFormula("Budget", "E2", "=B2*2")

	// A changed input updates every formula that reads it, also E2.
	s.pick(a, "B2")
	a.typeText("5")
	a.key("Enter", 0)
	s.shows(a, "E2", "10")
	s.shows(a, "D2", "$9,000.00")
	s.shows(a, "D5", "$10,608.00")
	s.saved(a)
	e.wantShown("Budget", "A2:E2", "Venue|5|$1,800.00|$9,000.00|10")
}

func (s *browserTest) navigation(t *testing.T) {
	a, e := s.a, s.e
	s.pick(a, "B2")
	for _, step := range []struct {
		key  string
		mods int
		want string
	}{
		{"ArrowDown", 0, "B3"}, {"ArrowRight", 0, "C3"}, {"ArrowUp", 0, "C2"}, {"ArrowLeft", 0, "B2"},
		{"Tab", 0, "C2"}, {"Tab", modShift, "B2"}, {"ArrowDown", modShift, "B2:B3"}, {"ArrowRight", 0, "C2"},
	} {
		a.key(step.key, step.mods)
		a.waitText("the selection after "+step.key, `T.active()`, step.want)
	}
	before := e.seq()

	// Enter opens the cell in place and Escape leaves it as it was.
	s.pick(a, "B2")
	a.key("Enter", 0)
	a.wait("the editor on B2", `sheet.editor.open && sheet.editor.text === '5'`)
	a.typeText("99")
	a.key("Escape", 0)
	a.wait("the editor closed", `!sheet.editor.ed`)
	s.shows(a, "B2", "5")
	a.waitText("the active cell after Escape", `T.active()`, "B2")

	// F2 on a formula asks for its text and edits it in place.
	s.pick(a, "E2")
	a.waitText("the formula bar", `T.bar()`, "=B2*2")
	a.key("F2", 0)
	a.wait("the editor with the formula", `sheet.editor.open && sheet.editor.text === '=B2*2'`)
	a.key("Escape", 0)
	a.wait("the editor closed", `!sheet.editor.ed`)
	if got := e.seq(); got != before {
		t.Errorf("moving and cancelling changed the sheet: sequence %d, was %d", got, before)
	}

	// F2 edits in place: the typed character joins the content.
	s.pick(a, "B2")
	a.key("F2", 0)
	a.wait("the editor on B2", `sheet.editor.open && sheet.editor.text === '5'`)
	a.typeText("0")
	a.key("Enter", 0)
	s.shows(a, "B2", "50")
	s.shows(a, "E2", "100")
	s.saved(a)
	e.wantShown("Budget", "B2", "50")
}

// dragScroll holds a selection drag under the grid: the grid scrolls on
// and the selection grows with it until the button is let go.
func (s *browserTest) dragScroll(t *testing.T) {
	a := s.a
	from := a.at(`'B3'`)
	var under point
	a.eval(`(() => { const r = document.querySelector('.gridwrap').getBoundingClientRect(); return { X: r.left + 200, Y: r.bottom + 30 }; })()`, &under)
	a.mouse("mouseMoved", from, "none", 0)
	a.mouse("mousePressed", from, "left", 1)
	a.mouse("mouseMoved", point{from.X, from.Y + 40}, "left", 0)
	a.mouse("mouseMoved", under, "left", 0)
	a.wait("the grid to scroll under the resting pointer", `document.querySelector('.gridwrap').scrollTop > 600`)
	a.mouse("mouseReleased", under, "left", 1)
	var top, end int
	a.eval(`document.querySelector('.gridwrap').scrollTop`, &top)
	a.eval(`Number(/^B3:B([0-9]+)$/.exec(T.active())[1])`, &end)
	if end < 40 {
		t.Errorf("the selection ends at row %d after scrolling %d pixels", end, top)
	}
	if !a.truthy(fmt.Sprintf(`T.cellEl('B%d') && T.classes('B%d').split(' ').includes('sel')`, end, end)) {
		t.Errorf("row %d, the end of the selection, is not drawn as selected", end)
	}
	// It stops with the button: no step is left waiting.
	if !a.truthy(`sheet.grid.dragTimer === null`) {
		t.Error("the grid goes on scrolling after the button was let go")
	}
	a.key("Home", modCtrl)
	a.waitText("the first cell", `T.active()`, "A1")
	a.wait("the top of the grid", `document.querySelector('.gridwrap').scrollTop === 0`)
}

func (s *browserTest) secondPage(t *testing.T) {
	a, e := s.a, s.e
	s.bo = s.b.open("Bob", s.bob.url+"/", viewport{width: 1280, height: 800})
	bo := s.bo
	s.enter(bo, "Bob")
	s.shows(bo, "B2", "50")
	s.shows(bo, "E2", "100")

	// Each sees the other's cell with the name, marked as not verified.
	s.pick(bo, "C3")
	a.wait("Bob's cell on Alice's screen", `T.cellEl('C3').querySelector('.who') && T.cellEl('C3').querySelector('.who').textContent === 'Bob'`)
	a.wait("Bob among the people", `document.querySelector('.faces').getAttribute('aria-label') === 'People here: Alice (you), Bob'`)
	s.pick(a, "A4")
	bo.wait("Alice's cell on Bob's screen", `T.cellEl('A4').querySelector('.who') && T.cellEl('A4').querySelector('.who').textContent === 'Alice'`)

	// Bob's edit reaches Alice with the values that depend on it.
	s.pick(bo, "B3")
	bo.typeText("10")
	a.wait("Bob marked as editing", `T.cellEl('B3').querySelector('.who') && T.cellEl('B3').querySelector('.who').textContent === 'Bob is editing'`)
	bo.key("Enter", 0)
	s.shows(a, "B3", "10")
	s.shows(a, "D3", "$320.00")
	s.shows(a, "D5", "$91,160.00")
	s.saved(bo)
	e.wantShown("Budget", "B3:D3", "10|$32.00|$320.00")
}

func (s *browserTest) rows(t *testing.T) {
	a, bo, e := s.a, s.bo, s.e
	e.wantFormula("Budget", "D5", "=SUM(D2:D4)")
	a.rightClick(a.at(`T.rowHead(2)`))
	s.menuItem(a, "Insert 1 row above")
	s.shows(a, "A3", "Venue")
	s.shows(a, "A2", "")
	s.shows(bo, "A3", "Venue")
	s.shows(a, "D6", "$91,160.00")
	s.saved(a)
	e.wantFormula("Budget", "D6", "=SUM(D3:D5)")
	e.wantFormula("Budget", "E3", "=B3*2")
	if got := a.str(`document.querySelector('.gridwrap').getAttribute('aria-rowcount')`); got != "1002" {
		t.Errorf("the grid states %s rows with its header, want 1002", got)
	}

	a.rightClick(a.at(`T.rowHead(2)`))
	s.menuItem(a, "Delete 1 row")
	s.shows(a, "A2", "Venue")
	s.shows(bo, "A2", "Venue")
	s.saved(a)
	e.wantFormula("Budget", "D5", "=SUM(D2:D4)")
	e.wantShown("Budget", "A2:E2", "Venue|50|$1,800.00|$90,000.00|100")

	// Columns go through the menu of a column letter.
	a.rightClick(a.at(`T.colHead('F')`))
	a.rightClick(a.at(`T.colHead('C')`))
	s.menuItem(a, "Insert 1 column left")
	s.shows(a, "D1", "Price")
	s.saved(a)
	e.wantFormula("Budget", "E2", "=B2*D2")
	e.wantFormula("Budget", "F2", "=B2*2")
	a.rightClick(a.at(`T.colHead('C')`))
	s.menuItem(a, "Delete 1 column")
	s.shows(a, "C1", "Price")
	s.saved(a)
	e.wantFormula("Budget", "D2", "=B2*C2")
	e.wantFormula("Budget", "E2", "=B2*2")
}

func (s *browserTest) sortUndo(t *testing.T) {
	a, bo, e := s.a, s.bo, s.e
	// Rows 2 to 4 by Qty: Venue 50, Lunch 10, Bus 2.
	a.drag(a.at(`'B2'`), a.at(`'B4'`))
	a.waitText("the selection", `T.active()`, "B2:B4")
	a.click(a.at(`T.byText('.tools button', 'Sort…')`))
	a.wait("the sort dialog", `document.querySelector('#sort-col')`)
	if got := a.str(`document.querySelector('#sort-col').value + ' ' + document.querySelector('#sort-dir').value`); got != "1 asc" {
		t.Fatalf("the sort dialog offers %q, want column B ascending", got)
	}
	a.click(a.at(`T.byText('.pop button', 'Sort')`))
	s.shows(a, "A2", "Bus")
	s.shows(a, "A3", "Lunch")
	s.shows(a, "A4", "Venue")
	s.shows(bo, "A2", "Bus")
	s.saved(a)
	e.wantShown("Budget", "A2:D4", "Bus|2|$420.00|$840.00", "Lunch|10|$32.00|$320.00", "Venue|50|$1,800.00|$90,000.00")
	// The formulas of the moved rows shifted with them.
	e.wantFormula("Budget", "D2", "=B2*C2")
	e.wantFormula("Budget", "D4", "=B4*C4")

	a.key("z", modCtrl)
	s.shows(a, "A2", "Venue")
	s.shows(bo, "A2", "Venue")
	s.saved(a)
	e.wantShown("Budget", "A2:B4", "Venue|50", "Lunch|10", "Bus|2")
	a.key("z", modCtrl|modShift)
	s.shows(a, "A2", "Bus")
	s.saved(a)
	a.key("z", modCtrl)
	s.shows(a, "A2", "Venue")
	s.saved(a)
	e.wantShown("Budget", "A2:B4", "Venue|50", "Lunch|10", "Bus|2")

	// An undo leaves a value someone else wrote since, and says so.
	s.pick(a, "B4")
	a.typeText("3")
	a.key("Enter", 0)
	s.shows(bo, "B4", "3")
	s.pick(bo, "B4")
	bo.typeText("2")
	bo.key("Enter", 0)
	s.shows(a, "B4", "2")
	s.saved(bo)
	s.pick(a, "A8")
	a.key("z", modCtrl)
	a.wait("the notice of the skipped undo", `T.notices().includes('Your undo skipped 1 change: someone else changed the same value after you. Their value stays.')`)
	s.saved(a)
	e.wantShown("Budget", "B4", "2")
}

func (s *browserTest) format(t *testing.T) {
	a, bo, e := s.a, s.bo, s.e
	s.pick(a, "A2")
	a.key("b", modCtrl)
	a.wait("A2 drawn bold", `T.classes('A2').split(' ').includes('b')`)
	bo.wait("A2 drawn bold for Bob", `T.classes('A2').split(' ').includes('b')`)
	s.saved(a)
	if got := e.formatOf("Budget", "A2"); got != `{"bold":true}` {
		t.Errorf("A2 has the format %s", got)
	}
	a.wait("the Bold button pressed", `T.byLabel('Bold').getAttribute('aria-pressed') === 'true'`)

	s.pick(a, "A3")
	a.click(a.at(`T.byLabel('Italic')`))
	a.wait("A3 drawn italic", `T.classes('A3').split(' ').includes('it')`)
	s.saved(a)
	if got := e.formatOf("Budget", "A3"); got != `{"italic":true}` {
		t.Errorf("A3 has the format %s", got)
	}

	// A fill from the palette, and a number format from the list.
	s.pick(a, "A4")
	a.click(a.at(`T.byLabel('Fill color')`))
	a.wait("the palette", `T.byLabel('yellow 100')`)
	a.click(a.at(`T.byLabel('yellow 100')`))
	a.wait("A4 filled", `T.cellEl('A4').style.backgroundColor.includes('yellow-100')`)
	s.saved(a)
	if got := e.formatOf("Budget", "A4"); got != `{"fill":"yellow-100"}` {
		t.Errorf("A4 has the format %s", got)
	}

	s.pick(a, "B3")
	a.eval(`(() => { const el = T.byLabel('Number format'); el.value = 'percent'; el.dispatchEvent(new Event('change')); })()`, nil)
	s.shows(a, "B3", "1000.00%")
	s.saved(a)
	if got := e.formatOf("Budget", "B3"); got != `{"number":{"kind":"percent","decimals":2}}` {
		t.Errorf("B3 has the format %s", got)
	}

	// A date format goes out with its pattern, and comes back with it.
	s.pick(a, "G1")
	a.typeText("2026-03-01")
	a.key("Enter", 0)
	s.shows(a, "G1", "2026-03-01")
	s.pick(a, "G1")
	a.click(a.at(`T.byText('.tools button', 'Options…')`))
	a.wait("the format options", `document.querySelector('#nf-kind')`)
	a.eval(`(() => {
		const kind = document.querySelector('#nf-kind'); kind.value = 'date'; kind.dispatchEvent(new Event('change'));
		document.querySelector('#nf-date').value = 'mmm d, yyyy';
	})()`, nil)
	a.click(a.at(`T.byText('.pop button', 'Apply')`))
	s.shows(a, "G1", "Mar 1, 2026")
	s.shows(bo, "G1", "Mar 1, 2026")
	s.saved(a)
	if got := e.formatOf("Budget", "G1"); got != `{"number":{"kind":"date","pattern":"mmm d, yyyy"}}` {
		t.Errorf("G1 has the format %s", got)
	}
	a.click(a.at(`T.byText('.tools button', 'Options…')`))
	a.wait("the format options", `document.querySelector('#nf-kind')`)
	if got := a.str(`document.querySelector('#nf-kind').value + ' ' + document.querySelector('#nf-date').value`); got != "date mmm d, yyyy" {
		t.Errorf("the options show %q for a date cell", got)
	}
	a.key("Escape", 0)
	a.wait("the dialog closed", `!document.querySelector('#nf-kind')`)

	// Back to how the later cases expect the sheet.
	s.pick(a, "B3")
	a.eval(`(() => { const el = T.byLabel('Number format'); el.value = 'general'; el.dispatchEvent(new Event('change')); })()`, nil)
	s.shows(a, "B3", "10")
	s.pick(a, "G1")
	a.key("Delete", 0)
	s.shows(a, "G1", "")
	s.saved(a)
}

func (s *browserTest) tabs(t *testing.T) {
	a, bo, e := s.a, s.bo, s.e
	a.click(a.at(`T.byLabel('Add a tab')`))
	a.waitText("the tab bar", `T.tabs()`, "Budget,Notes,Sheet1")
	bo.waitText("Bob's tab bar", `T.tabs()`, "Budget,Notes,Sheet1")
	s.saved(a)

	tab := `T.byText('.tabbar .tab', 'Sheet1')`
	a.doubleClick(a.at(tab))
	a.wait("the rename field", `document.querySelector('.tabbar .tab input')`)
	a.key("a", modMeta)
	a.eval(`document.querySelector('.tabbar .tab input').select()`, nil)
	a.typeText("Travel")
	a.key("Enter", 0)
	a.waitText("the tab bar", `T.tabs()`, "Budget,Notes,Travel")
	bo.waitText("Bob's tab bar", `T.tabs()`, "Budget,Notes,Travel")
	s.saved(a)
	if got := e.tabList(); got != "Budget,Notes,Travel" {
		t.Errorf("the sheet has the tabs %s", got)
	}

	// Open it, type, and come back: the other tab keeps its cells.
	a.click(a.at(`T.byText('.tabbar .tab', 'Travel')`))
	a.wait("the Travel tab", `sheet.model.tab && sheet.model.tab.name === 'Travel' && !sheet.model.tab.loading`)
	s.shows(a, "A1", "")
	a.typeText("Train")
	a.key("Enter", 0)
	s.shows(a, "A1", "Train")
	s.saved(a)
	e.wantShown("Travel", "A1", "Train")
	a.click(a.at(`T.byText('.tabbar .tab', 'Budget')`))
	a.wait("the Budget tab", `sheet.model.tab.name === 'Budget' && !sheet.model.tab.loading`)
	s.shows(a, "A2", "Venue")

	// Deleting a tab with content asks in the page first.
	a.rightClick(a.at(`T.byText('.tabbar .tab', 'Travel')`))
	s.menuItem(a, "Delete…Del")
	a.wait("the confirmation", `T.byText('.tabbar .confirm button', 'Delete')`)
	a.click(a.at(`T.byText('.tabbar .confirm button', 'Delete')`))
	a.waitText("the tab bar", `T.tabs()`, "Budget,Notes")
	bo.waitText("Bob's tab bar", `T.tabs()`, "Budget,Notes")
	s.saved(a)
	if got := e.tabList(); got != "Budget,Notes" {
		t.Errorf("the sheet has the tabs %s", got)
	}
}

func (s *browserTest) find(t *testing.T) {
	a := s.a
	s.pick(a, "A1")
	a.key("f", modCtrl)
	a.wait("the find field", `document.activeElement === document.querySelector('.find input')`)
	a.typeText("LUNCH")
	a.waitText("the match count", `document.querySelector('.find-count').textContent`, "1 of 1")
	a.waitText("the selection", `T.active()`, "A3")
	a.wait("the match marked", `T.classes('A3').split(' ').includes('hit')`)
	a.key("Escape", 0)
	a.wait("find closed and focus in the grid", `document.querySelector('.find').hidden && document.activeElement === sheet.grid.input`)

	// The anchor is the active cell when the field opens, A3. Opening the
	// field leaves the selection there, the text selects the first match
	// at or after it, and Next the match after that (rule 20a).
	matches := s.e.findMatches("Budget", "0.00")
	first := slices.IndexFunc(matches, func(m findMatch) bool { return m.row >= 2 })
	if first < 1 || len(matches)-first < 2 {
		t.Fatalf("the case needs a match of 0.00 before A3 and two at or after it; the tab has %v", matches)
	}
	count := func(i int) string { return fmt.Sprintf("%d of %d", i+1, len(matches)) }
	search := func(text string) {
		a.key("f", modCtrl)
		a.wait("the find field", `document.activeElement === document.querySelector('.find input')`)
		a.eval(`document.querySelector('.find input').select()`, nil)
		a.typeText(text)
	}
	a.key("f", modCtrl)
	a.wait("the find field", `document.activeElement === document.querySelector('.find input')`)
	a.waitText("the selection after the field opens", `T.active()`, "A3")
	a.eval(`document.querySelector('.find input').select()`, nil)
	a.typeText("0.00")
	a.waitText("the match count", `document.querySelector('.find-count').textContent`, count(first))
	a.waitText("the first match at or after A3", `T.active()`, matches[first].addr)
	a.key("Enter", 0)
	a.waitText("the match count after Next", `document.querySelector('.find-count').textContent`, count(first+1))
	a.waitText("the next match", `T.active()`, matches[first+1].addr)
	a.key("Escape", 0)
	a.wait("find closed", `document.querySelector('.find').hidden`)

	// A person's move takes the anchor along: from A1 the same text selects
	// the first match of the tab.
	s.pick(a, "A1")
	search("0.00")
	a.waitText("the match count from A1", `document.querySelector('.find-count').textContent`, count(0))
	a.waitText("the first match of the tab", `T.active()`, matches[0].addr)
	a.key("Escape", 0)
	a.wait("find closed", `document.querySelector('.find').hidden`)
}

// findMatch is a cell that Find matches, and its 0-based row.
type findMatch struct {
	addr string
	row  int
}

// findMatches lists the cells of a tab whose shown text holds text,
// capitals aside, in reading order, as Find matches them (rule 20a). The
// texts of this test have no letter whose browser lower case differs.
func (e *env) findMatches(tab, text string) []findMatch {
	e.t.Helper()
	var out []findMatch
	for r, row := range e.readSheet(ReadQuery{Tab: tab, Include: []string{"shown"}}).Shown {
		for c, shown := range row {
			if strings.Contains(strings.ToLower(shown), strings.ToLower(text)) {
				out = append(out, findMatch{fmt.Sprintf("%s%d", formula.ColumnName(int32(c)), r+1), r})
			}
		}
	}
	return out
}

// clipboardEvent sends a copy, cut or paste event with a clipboard of its
// own to the page and returns the text the page put on it. The system
// clipboard of this machine is not touched.
func (p *page) clipboardEvent(kind, text string) string {
	p.b.t.Helper()
	return p.str(fmt.Sprintf(`(() => {
		const data = new DataTransfer();
		if (%q) data.setData('text/plain', %q);
		document.dispatchEvent(new ClipboardEvent(%q, { clipboardData: data, bubbles: true, cancelable: true }));
		return data.getData('text/plain');
	})()`, text, text, kind))
}

func (s *browserTest) copyPaste(t *testing.T) {
	a, bo, e := s.a, s.bo, s.e
	// A copy inside the sheet carries the formula and its format.
	s.pick(a, "D2")
	text := a.clipboardEvent("copy", "")
	if text != "$90,000.00" {
		t.Fatalf("the copy put %q on the clipboard", text)
	}
	s.pick(a, "H2")
	a.clipboardEvent("paste", text)
	s.shows(a, "H2", "$0.00")
	s.shows(bo, "H2", "$0.00")
	s.saved(a)
	e.wantFormula("Budget", "H2", "=F2*G2")

	// A range copies as tab-separated shown text and pastes as formulas.
	a.drag(a.at(`'B2'`), a.at(`'D3'`))
	a.waitText("the selection", `T.active()`, "B2:D3")
	text = a.clipboardEvent("copy", "")
	if text != "50\t$1,800.00\t$90,000.00\n10\t$32.00\t$320.00" {
		t.Fatalf("the copy put %q on the clipboard", text)
	}
	s.pick(a, "J2")
	a.clipboardEvent("paste", text)
	s.shows(a, "L2", "$90,000.00")
	s.shows(a, "L3", "$320.00")
	s.saved(a)
	e.wantFormula("Budget", "L3", "=J3*K3")
	e.wantShown("Budget", "J2:L3", "50|$1,800.00|$90,000.00", "10|$32.00|$320.00")

	// Text from elsewhere is typed into the cells.
	s.pick(a, "J5")
	a.clipboardEvent("paste", "7\t=J5*2\nseven\t")
	s.shows(a, "J5", "7")
	s.shows(a, "K5", "14")
	s.shows(a, "J6", "seven")
	s.saved(a)
	e.wantFormula("Budget", "K5", "=J5*2")

	// A formula whose result is empty text copies as empty text and
	// pastes as its formula.
	s.pick(a, "J7")
	a.typeText(`=""`)
	a.key("Enter", 0)
	s.saved(a)
	s.pick(a, "J7")
	if text = a.clipboardEvent("copy", ""); text != "" {
		t.Fatalf("the copy of an empty result put %q on the clipboard", text)
	}
	s.pick(a, "J5")
	a.clipboardEvent("paste", "")
	s.shows(a, "J5", "")
	s.saved(a)
	e.wantFormula("Budget", "J5", `=""`)

	// Clear what was pasted: Delete over a selected range.
	a.drag(a.at(`'H2'`), a.at(`'L7'`))
	a.waitText("the selection", `T.active()`, "H2:L7")
	a.key("Delete", 0)
	s.shows(a, "L2", "")
	s.shows(a, "J6", "")
	s.saved(a)
	if got := e.content("Budget", "K5"); got != "null" {
		t.Errorf("K5 holds %s after the clear", got)
	}
}

func (s *browserTest) formulaInFlight(t *testing.T) {
	a, bo, e := s.a, s.bo, s.e
	// Alice types a formula that reads row 2. Bob inserts a row above it
	// while her editor is open.
	a.eval(`(() => { const net = sheet.net, send = net.rebase; globalThis.rebases = 0; net.rebase = function (...args) { globalThis.rebases++; return send.apply(this, args); }; })()`, nil)
	s.pick(a, "F4")
	a.typeText("=B2+")
	a.wait("the formula in the editor", `sheet.editor.isFormula && sheet.editor.text === '=B2+'`)
	bo.rightClick(bo.at(`T.rowHead(2)`))
	s.menuItem(bo, "Insert 1 row above")
	s.shows(bo, "A3", "Venue")
	// Her text and her grid move together: the row arrives with the
	// adjusted reference, and the editor stays on its cell, now F5.
	a.wait("the adjusted text", `sheet.editor.open && sheet.editor.text === '=B3+'`)
	s.shows(a, "A3", "Venue")
	if got := a.str(`globalThis.rebases`); got == "0" {
		t.Error("the editor's text changed without a rebase exchange")
	}
	a.waitText("the edited cell", `T.active()`, "F5")
	a.typeText("C3")
	a.key("Enter", 0)
	s.shows(a, "F5", "1850")
	s.shows(bo, "F5", "1850")
	s.saved(a)
	e.wantFormula("Budget", "F5", "=B3+C3")
	e.wantShown("Budget", "A3:C3", "Venue|50|$1,800.00")

	// Take the row out again, and the formula.
	bo.rightClick(bo.at(`T.rowHead(2)`))
	s.menuItem(bo, "Delete 1 row")
	s.shows(a, "A2", "Venue")
	s.saved(bo)
	e.wantFormula("Budget", "F4", "=B2+C2")
	s.pick(a, "F4")
	a.key("Delete", 0)
	s.shows(a, "F4", "")
	s.saved(a)
}

func (s *browserTest) agentWrite(t *testing.T) {
	a, bo, e := s.a, s.bo, s.e
	e.mustAgent(`{"message":"Add the snacks","ops":[{"op":"write","tab":"Budget","at":"A7","rows":[["Snacks",4,25,"=B7*C7"]]}]}`)
	s.shows(a, "A7", "Snacks")
	s.shows(a, "D7", "100")
	s.shows(bo, "D7", "100")
	a.wait("the agent notice", `T.notices().includes('The agent wrote 4 cells in Budget.')`)
	a.wait("the written cells highlighted", `T.classes('A7').split(' ').includes('flash')`)
	e.wantShown("Budget", "A7:D7", "Snacks|4|25|100")

	// An agent write over what a person typed tells that person.
	s.pick(a, "A9")
	a.typeText("mine")
	a.key("Enter", 0)
	s.shows(a, "A9", "mine")
	s.saved(a)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Budget","at":"A9","rows":[["theirs"]]}]}`)
	s.shows(a, "A9", "theirs")
	a.wait("the overwrite notice with the typed text", `T.notices().includes('Someone changed A9 after you typed in it.') && T.notices().includes('mine')`)
	if bo.truthy(`T.notices().includes('Someone changed')`) {
		t.Error("Bob was told about an overwrite of Alice's cell")
	}
	e.mustAgent(`{"ops":[{"op":"clear","tab":"Budget","ranges":["A7:D9"]}]}`)
	s.shows(a, "A9", "")
}

// eventually polls a condition of the server's sheet.
func (s *browserTest) eventually(what string, ok func() bool) {
	s.Helper()
	deadline := time.Now().Add(e2eWait)
	for !ok() {
		if time.Now().After(deadline) {
			s.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(e2ePoll)
	}
}

func (s *browserTest) refusal(t *testing.T) {
	a, e := s.a, s.e
	before := e.seq()
	s.pick(a, "H1")
	a.clipboardEvent("paste", strings.Repeat("x", 40000))
	a.wait("the refusal with its reason", `T.notices().includes('The host refused this paste: a text cell holds at most 32,767 characters. Nothing was changed')`)
	// The grid is back at the host's state without any delta: a refused
	// action takes no sequence.
	s.shows(a, "H1", "")
	s.saved(a)
	if got := e.seq(); got != before {
		t.Errorf("the refused paste took a sequence: %d, was %d", got, before)
	}
	a.eval(`document.querySelectorAll('.toasts .toast button[aria-label="Dismiss this notice"]').forEach((b) => b.click())`, nil)
}

func (s *browserTest) history(t *testing.T) {
	a, bo, e := s.a, s.bo, s.e
	e.b.importAll(t)
	const seed = 2 // revision 1 is the creation, 2 the agent's first write
	a.click(a.at(`T.byLabel('History')`))
	rev := fmt.Sprintf(`document.querySelector('.hist .rev[data-key="rev-%d"]')`, seed)
	a.wait("the revision list", rev)
	if got := a.str(rev + `.textContent`); !strings.Contains(got, "Agent write") || !strings.Contains(got, "Offsite budget") {
		t.Errorf("revision %d is listed as %q", seed, got)
	}
	a.click(a.at(rev))
	a.wait("the revision banner", fmt.Sprintf(`document.querySelector('.banners').textContent.includes('Viewing revision %d')`, seed))
	// The revision shows its own values, calculated and formatted.
	s.shows(a, "B2", "2")
	s.shows(a, "D2", "$3,600.00")
	s.shows(a, "D5", "$5,208.00")
	if !strings.Contains(a.str(`T.classes('A1')`), " b") {
		t.Errorf("the revision's A1 is not drawn bold: %q", a.str(`T.classes('A1')`))
	}
	if a.truthy(`sheet.canWrite()`) {
		t.Error("a revision can be edited")
	}

	// Pin it.
	pin := fmt.Sprintf(`T.byText('.hist button', 'Pin revision %d')`, seed)
	a.wait("the pin button", pin)
	a.click(a.at(pin))
	a.wait("the pin confirmed", fmt.Sprintf(`document.querySelector('.hist').textContent.includes('Pinned revision %d.')`, seed))
	pinned := e.b.isPinned(seed)
	if !pinned {
		t.Errorf("revision %d is not pinned on the host", seed)
	}

	// Recover its Notes tab: the tab exists, so it comes back as a copy.
	a.click(a.at(`T.byText('.tabbar .tab', 'Notes')`))
	s.shows(a, "A1", "Bring a projector")
	restore := `T.byText('.hist button', 'Restore tab “Notes” as “Notes (restored)”')`
	a.wait("the restore button", restore)
	a.click(a.at(restore))
	a.waitText("the tab bar", `T.tabs()`, "Budget,Notes,Notes (restored)")
	bo.waitText("Bob's tab bar", `T.tabs()`, "Budget,Notes,Notes (restored)")
	if got := e.tabList(); got != "Budget,Notes,Notes (restored)" {
		t.Errorf("the sheet has the tabs %s", got)
	}
	e.wantShown("Notes (restored)", "A1", "Bring a projector")
	// The recovery is Alice's own edit: her undo takes the tab out again.
	a.click(a.at(`'C8'`))
	a.key("z", modCtrl)
	a.waitText("the tab bar after undo", `T.tabs()`, "Budget,Notes")
	s.saved(a)
	if got := e.tabList(); got != "Budget,Notes" {
		t.Errorf("after the undo the sheet has the tabs %s", got)
	}

	// Roll the whole sheet back.
	a.wait("the revision list", rev)
	a.click(a.at(rev))
	back := `T.byText('.hist button', 'Roll back to this revision…')`
	a.wait("the rollback button", back)
	a.click(a.at(back))
	a.wait("the confirmation in the panel", `document.querySelector('.hist .confirm-acts')`)
	a.click(a.at(`T.byText('.hist .confirm-acts button', 'Roll back')`))
	a.wait("the rollback notice", `T.notices().includes('The sheet was rolled back to an earlier revision.')`)
	bo.wait("Bob's rollback notice", `T.notices().includes('The sheet was rolled back to an earlier revision.')`)
	for _, p := range []*page{a, bo} {
		p.wait("the sheet again", `sheet.net.open && sheet.model.tab && !sheet.model.tab.loading && !sheet.view`)
		s.shows(p, "B2", "2")
		s.shows(p, "D5", "$5,208.00")
		s.shows(p, "E2", "")
	}
	e.wantShown("Budget", "A1:D5", "Item|Qty|Price|Total", "Venue|2|$1,800.00|$3,600.00", "Lunch|24|$32.00|$768.00", "Bus|2|$420.00|$840.00", "Total|||$5,208.00")
	if got := e.formatOf("Budget", "A2"); got != "{}" {
		t.Errorf("after the rollback A2 has the format %s", got)
	}
	a.click(a.at(`T.byLabel('Close History')`))
	a.wait("History closed", `document.querySelector('.hist').hidden`)

	// Undo history is gone for everyone, and editing goes on.
	a.click(a.at(`'B3'`))
	a.key("z", modCtrl)
	a.wait("the notice that nothing can be undone", `T.notices().includes('No earlier undo is available.')`)
	s.pick(a, "B2")
	a.typeText("4")
	a.key("Enter", 0)
	s.shows(a, "D2", "$7,200.00")
	s.shows(bo, "D2", "$7,200.00")
	s.saved(a)
	// The reconnects after the rollback are expected.
	a.errors()
	bo.errors()
}

func (s *browserTest) reconnect(t *testing.T) {
	a, bo, e := s.a, s.bo, s.e
	// From here on nothing the server sends reaches Alice.
	s.alice.listener.mute.Store(true)
	bo.rightClick(bo.at(`T.rowHead(2)`))
	s.menuItem(bo, "Insert 1 row above")
	s.shows(bo, "A3", "Venue")
	s.saved(bo)

	// Alice still sees Venue in row 2 and writes a formula against that
	// screen. The server receives it, adjusts it, and its answer is lost.
	if got := a.str(`T.shown('A2')`); got != "Venue" {
		t.Fatalf("Alice's screen moved without a delta: A2 shows %q", got)
	}
	s.pick(a, "F2")
	a.typeText("=B2*10")
	a.key("Enter", 0)
	s.eventually("the formula on the server", func() bool { return strings.Contains(e.content("Budget", "F3"), `"=B3*10"`) })
	// A pasted formula is read against her screen in the same way.
	s.pick(a, "G2")
	a.clipboardEvent("paste", "=C2+1")
	s.eventually("the pasted formula on the server", func() bool { return strings.Contains(e.content("Budget", "G3"), `"=C3+1"`) })
	applied := e.seq()
	a.wait("the edits unconfirmed", `sheet.net.pending.length === 2 && T.status() === 'Saving…' && T.classes('F2').split(' ').includes('pend')`)

	// The connection drops. The page reconnects, takes what it missed and
	// sends the edit again; the server answers with its stored outcome.
	s.alice.listener.drop()
	a.wait("the reconnect", `sheet.net.open && sheet.net.pending.length === 0 && T.status() === 'Saved'`)
	s.shows(a, "A3", "Venue")
	s.shows(a, "F3", "40")
	s.shows(a, "G3", "1801")
	s.shows(a, "F2", "")
	if got := e.seq(); got != applied {
		t.Errorf("the resent edit changed the sheet again: sequence %d, was %d", got, applied)
	}
	e.wantFormula("Budget", "F3", "=B3*10")
	e.wantFormula("Budget", "G3", "=C3+1")
	if e.content("Budget", "F2") != "null" {
		t.Errorf("F2 holds %s", e.content("Budget", "F2"))
	}
	if errs := a.errors(); len(errs) > 1 {
		t.Errorf("the dropped connection reported more than itself: %v", errs)
	}

	bo.rightClick(bo.at(`T.rowHead(2)`))
	s.menuItem(bo, "Delete 1 row")
	s.shows(a, "A2", "Venue")
	s.saved(bo)
	a.drag(a.at(`'F2'`), a.at(`'G2'`))
	a.waitText("the selection", `T.active()`, "F2:G2")
	a.key("Delete", 0)
	s.shows(a, "G2", "")
	s.saved(a)
}

// wantCSV is the CSV of the Budget tab at the public case: each field is
// the cell's value, not the text its currency format shows (rule 60).
const wantCSV = "\ufeffItem,Qty,Price,Total\r\nVenue,4,1800,7200\r\nLunch,24,32,768\r\nBus,2,420,840\r\nTotal,,,8808\r\n"

func (s *browserTest) publicRoute(t *testing.T) {
	a, e := s.a, s.e
	s.pub = s.b.open("Public", s.public.url+"/", viewport{width: 1280, height: 800})
	pub := s.pub
	pub.wait("the Public sheet", `globalThis.sheet && sheet.net.access === 'public' && sheet.model.tab && !sheet.model.tab.loading`)
	s.shows(pub, "A2", "Venue")
	s.shows(pub, "D5", "$8,808.00")
	for what, expr := range map[string]string{
		"the read-only badge shows":    `!document.querySelector('.badge').hidden && document.querySelector('.badge').textContent === 'Public · read-only'`,
		"the toolbar is hidden":        `document.querySelector('.tools').hidden`,
		"History is not offered":       `T.byLabel('History') === null`,
		"no name is asked":             `!document.querySelector('#me-name')`,
		"no session reaches the page":  `sheet.net.session === null && sheet.net.sessionId === null`,
		"no presence reaches the page": `sheet.presence.people.length === 0 && !document.querySelector('.gridwrap .who')`,
	} {
		if !pub.truthy(expr) {
			t.Errorf("on the Public route it is not true that %s", what)
		}
	}
	// It reads the formula of a cell, and follows edits made elsewhere.
	s.pick(pub, "D2")
	pub.waitText("the formula bar", `T.bar()`, "=B2*C2")
	before := e.seq()
	pub.typeText("9")
	pub.key("Enter", 0)
	pub.wait("the notice that the sheet is read-only", `T.notices().includes('This sheet is Public here')`)
	pub.key("Delete", 0)
	pub.clipboardEvent("paste", "9")
	if pub.truthy(`sheet.editor.ed`) || e.seq() != before {
		t.Errorf("the Public page edited the sheet: sequence %d, was %d", e.seq(), before)
	}
	if got := pub.clipboardEvent("copy", ""); got != "$7,200.00" {
		t.Errorf("a copy on the Public page gave %q", got)
	}
	s.pick(a, "B4")
	a.typeText("3")
	a.key("Enter", 0)
	s.shows(pub, "D4", "$1,260.00")
	s.shows(pub, "D5", "$9,228.00")
	s.pick(a, "B4")
	a.typeText("2")
	a.key("Enter", 0)
	s.shows(pub, "D5", "$8,808.00")
	s.saved(a)
	if resp, _ := s.public.get(t, "/_sheet/api/revisions"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("History on the Public route: %d", resp.StatusCode)
	}

	// The CSV of the open tab: the same bytes on both routes and from the
	// button of the page.
	tab := e.readSheet(ReadQuery{}).Tabs[0].ID
	for name, rt := range map[string]*route{"Private": s.alice, "Public": s.public} {
		resp, body := rt.get(t, fmt.Sprintf("/_sheet/csv?tab=%d", tab))
		if resp.StatusCode != http.StatusOK || string(body) != wantCSV {
			t.Errorf("the CSV of the %s route: %d %q", name, resp.StatusCode, body)
		}
		if got := resp.Header.Get("Content-Disposition"); !strings.Contains(got, "attachment") || !strings.Contains(got, "Budget.csv") {
			t.Errorf("the CSV of the %s route has the disposition %q", name, got)
		}
	}
	dir := t.TempDir()
	s.b.must("", "Browser.setDownloadBehavior", map[string]any{"behavior": "allow", "browserContextId": pub.context, "downloadPath": dir})
	pub.click(pub.at(`T.byLabel('Download this tab as CSV')`))
	var got []byte
	s.eventually("the downloaded file", func() bool {
		data, err := os.ReadFile(filepath.Join(dir, "Budget.csv"))
		got = data
		return err == nil && len(data) == len(wantCSV)
	})
	if string(got) != wantCSV {
		t.Errorf("the page downloaded %q", got)
	}
}

func (p *page) touch(kind string, points ...point) {
	p.b.t.Helper()
	active := make([]map[string]float64, len(points))
	for i, at := range points {
		active[i] = map[string]float64{"x": at.X, "y": at.Y}
	}
	p.cdp("Input.dispatchTouchEvent", map[string]any{"type": kind, "touchPoints": active})
}

// tap uses touch events so the browser generates a phone's compatibility
// mouse events without depending on the platform's synthetic gesture input.
func (p *page) tap(at point) {
	p.b.t.Helper()
	p.touch("touchStart", at)
	p.touch("touchEnd")
}

// swipeLeft moves the finger once per frame and slows it before release.
func (p *page) swipeLeft(from point, distance float64) {
	p.b.t.Helper()
	p.touch("touchStart", from)
	const steps = 16
	for i := 1; i <= steps; i++ {
		progress := float64(i) / steps
		x := from.X - distance*progress*(2-progress)
		p.touch("touchMove", point{x, from.Y})
		p.eval(`new Promise(resolve => requestAnimationFrame(() => resolve(true)))`, nil)
	}
	p.touch("touchEnd")
}

// phone opens the sheet at a phone's width: a person scrolls, selects a
// cell, reads the formula bar, edits one cell and switches tabs (rule 20).
func (s *browserTest) phone(t *testing.T) {
	e := s.e
	ph := s.b.open("Phone", s.bob.url+"/", viewport{width: 390, height: 844, scale: 2, mobile: true})
	s.ph = ph
	s.enter(ph, "Dana")
	ph.wait("the phone layout", `document.documentElement.dataset.layout === 'phone'`)
	s.shows(ph, "A2", "Venue")
	for what, expr := range map[string]string{
		"the page does not scroll sideways": `document.documentElement.scrollWidth <= innerWidth && document.body.scrollWidth <= innerWidth`,
		"the tab bar is on screen":          `(() => { const r = document.querySelector('.tabbar').getBoundingClientRect(); return r.height > 0 && r.top >= 0 && r.bottom <= innerHeight + 1; })()`,
		"the formula bar is on screen":      `(() => { const r = document.querySelector('.fbar').getBoundingClientRect(); return r.height > 0 && r.right <= innerWidth + 1; })()`,
		"the grid has room":                 `document.querySelector('.gridwrap').getBoundingClientRect().height > 300`,
	} {
		if !ph.truthy(expr) {
			t.Errorf("at phone width it is not true that %s", what)
		}
	}
	// Column D lies beyond the screen: a finger scrolls the grid to it.
	if ph.truthy(`T.cellEl('D2').getBoundingClientRect().right <= innerWidth`) {
		t.Fatal("column D fits a phone screen; the scroll below shows nothing")
	}
	// A tap during momentum scrolling stops the scroll instead of selecting a cell.
	ph.eval(`document.querySelector('.gridwrap').addEventListener('scrollend', () => { window.phoneScrollEnded = true; }, { once: true })`, nil)
	ph.swipeLeft(point{200, 400}, 160)
	ph.wait("the end of the swipe", `window.phoneScrollEnded`)
	ph.wait("column D on screen", `document.querySelector('.gridwrap').scrollLeft > 100 && T.cellEl('D2').getBoundingClientRect().right <= innerWidth`)
	ph.tap(ph.at(`'D2'`))
	ph.waitText("the tapped cell", `T.active()`, "D2")
	ph.waitText("the formula bar", `T.bar()`, "=B2*C2")
	ph.tap(ph.at(`'C3'`))
	ph.waitText("the tapped cell", `T.active()`, "C3")
	ph.waitText("the formula bar", `T.bar()`, "32")
	ph.tap(ph.at(`document.querySelector('.fbar .finput')`))
	ph.wait("the formula bar as the editor", `sheet.editor.open && document.activeElement === document.querySelector('.fbar .finput')`)
	ph.key("a", modMeta)
	ph.eval(`document.querySelector('.fbar .finput').select()`, nil)
	ph.typeText("30")
	ph.key("Enter", 0)
	s.shows(ph, "C3", "$30.00")
	s.shows(ph, "D3", "$720.00")
	s.saved(ph)
	e.wantShown("Budget", "C3:D3", "$30.00|$720.00")
	ph.tap(ph.at(`T.byText('.tabbar .tab', 'Notes')`))
	ph.wait("the Notes tab", `sheet.model.tab.name === 'Notes' && !sheet.model.tab.loading`)
	s.shows(ph, "A1", "Bring a projector")
	ph.tap(ph.at(`T.byText('.tabbar .tab', 'Budget')`))
	ph.wait("the Budget tab", `sheet.model.tab.name === 'Budget' && !sheet.model.tab.loading`)
	ph.noErrors()
}

// screenshots saves what a person sees, for a look by eye.
func (s *browserTest) screenshots(t *testing.T) {
	if s.shots == "" {
		t.Skip("set FLATS_SHEET_E2E_SHOTS to a directory to save screenshots")
	}
	a, bo := s.a, s.bo
	settle := func(p *page) {
		p.eval(`new Promise((done) => requestAnimationFrame(() => requestAnimationFrame(done)))`, nil)
	}
	a.drag(a.at(`'B2'`), a.at(`'D4'`))
	a.waitText("the selection", `T.active()`, "B2:D4")
	s.pick(bo, "A3")
	a.wait("Bob's cell", `T.cellEl('A3').querySelector('.who')`)
	settle(a)
	a.screenshot(s.shots, "wide-light.png")
	a.view(viewport{width: 1280, height: 800, dark: true})
	a.wait("the dark scheme", `document.documentElement.dataset.scheme === 'dark'`)
	settle(a)
	a.screenshot(s.shots, "wide-dark.png")
	a.click(a.at(`T.byLabel('History')`))
	a.wait("the revision list", `document.querySelector('.hist .rev')`)
	settle(a)
	a.screenshot(s.shots, "history-dark.png")
	a.click(a.at(`T.byLabel('Close History')`))
	a.view(viewport{width: 1280, height: 800})
	a.wait("the light scheme", `document.documentElement.dataset.scheme === 'light'`)
	s.pick(a, "E3")
	a.typeText("=su")
	a.wait("the function hints", `document.querySelector('.hint .fn')`)
	settle(a)
	a.screenshot(s.shots, "editing-light.png")
	a.key("Escape", 0)
	a.rightClick(a.at(`T.rowHead(3)`))
	a.wait("the menu", `document.querySelector('.menu')`)
	settle(a)
	a.screenshot(s.shots, "menu-light.png")
	a.key("Escape", 0)
	settle(s.pub)
	s.pub.screenshot(s.shots, "public-light.png")
	ph := s.ph
	settle(ph)
	ph.screenshot(s.shots, "phone-light.png")
	ph.view(viewport{width: 390, height: 844, scale: 2, mobile: true, dark: true})
	ph.wait("the dark scheme", `document.documentElement.dataset.scheme === 'dark'`)
	ph.tap(ph.at(`T.byText('.tools button', 'Format')`))
	ph.wait("the format menu", `document.querySelector('.menu')`)
	settle(ph)
	ph.screenshot(s.shots, "phone-menu-dark.png")
	ph.key("Escape", 0)
}

// withdrawn takes the Public route away, as core does when the sheet
// returns to Private: the socket closes and the content leaves the screen.
func (s *browserTest) withdrawn(t *testing.T) {
	pub, e := s.pub, s.e
	s.public.withdrawn.Store(true)
	e.in.WithdrawPublic()
	pub.wait("the closed socket", `!sheet.net.open`)
	s.eventually("no Public connection on the server", func() bool {
		e.in.hub.mu.Lock()
		defer e.in.hub.mu.Unlock()
		return e.in.hub.public == 0
	})
	pub.wait("the cover in place of the sheet", `!document.querySelector('.cover').hidden && sheet.model.tab === null && document.querySelector('.gridwrap').hidden`)
	if got := pub.str(`document.querySelector('.cover').textContent`); !strings.Contains(got, "This sheet is not available here") {
		t.Errorf("the cover says %q", got)
	}
	if pub.truthy(`document.querySelector('.gridwrap').textContent.includes('Venue')`) {
		t.Error("the withdrawn sheet's content is still in the page")
	}
	if s.shots != "" {
		pub.screenshot(s.shots, "public-withdrawn.png")
	}
	// The failed connections are the withdrawal itself.
	for _, msg := range pub.errors() {
		if !strings.Contains(msg, "WebSocket") && !strings.Contains(msg, "404") {
			t.Errorf("the Public page reported %s", msg)
		}
	}
	// The Private pages go on.
	s.pick(s.a, "B4")
	s.a.typeText("5")
	s.a.key("Enter", 0)
	s.shows(s.bo, "D4", "$2,100.00")
	s.saved(s.a)
}
