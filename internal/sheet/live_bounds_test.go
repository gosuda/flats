package sheet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
	"github.com/gosuda/flats/internal/wsconn"
)

// manualTimers stands in for the timers of action assembly, so a test ends
// a lifetime without waiting for it.
type manualTimers struct {
	mu    sync.Mutex
	armed []*manualTimer
}

type manualTimer struct {
	d       time.Duration
	fn      func()
	stopped atomic.Bool
}

func (m *manualTimer) Stop() bool { return !m.stopped.Swap(true) }

func useManualTimers(t *testing.T) *manualTimers {
	m := &manualTimers{}
	real := assemblyTimer
	assemblyTimer = func(d time.Duration, fn func()) interface{ Stop() bool } {
		m.mu.Lock()
		defer m.mu.Unlock()
		timer := &manualTimer{d: d, fn: fn}
		m.armed = append(m.armed, timer)
		return timer
	}
	t.Cleanup(func() { assemblyTimer = real })
	return m
}

// expire runs every armed timer that was not stopped.
func (m *manualTimers) expire() (ran int) {
	m.mu.Lock()
	armed := m.armed
	m.armed = nil
	m.mu.Unlock()
	for _, timer := range armed {
		if !timer.stopped.Load() {
			timer.fn()
			ran++
		}
	}
	return ran
}

// sendParts sends an action as action_part frames of chunk bytes and stops
// after limit parts.
func (c *client) sendParts(n int, action string, chunk, limit int) {
	c.t.Helper()
	parts := (len(action) + chunk - 1) / chunk
	for i := 0; i < parts && i < limit; i++ {
		data, _ := json.Marshal(action[i*chunk : min(len(action), (i+1)*chunk)])
		c.send(fmt.Sprintf(`{"type":"action_part","n":"%d","part":%d,"parts":%d,"data":%s}`, n, i, parts, data))
	}
}

// TestActionAssemblyExpires leaves an action half sent on a connection that
// then sends nothing. When the assembly's lifetime ends its bytes return to
// the host-wide budget, and another connection assembles an action that had
// no room before.
func TestActionAssemblyExpires(t *testing.T) {
	timers := useManualTimers(t)
	e := start(t)
	s := serve(e)
	tab := e.tabID("Sheet1")
	action := func(n int) string {
		values := make([]string, 30)
		for i := range values {
			values[i] = `["` + strings.Repeat("v", 12000) + `"]`
		}
		return fmt.Sprintf(`{"type":"action","n":"%d","base":"%d","ops":[{"op":"write","tab":%d,"rows":%s,"cols":%s,"values":[%s]}]}`,
			n, e.seq(), tab, e.rowsOf("Sheet1", 1, 30), e.colsOf("Sheet1", 1, 1), strings.Join(values, ","))
	}
	const chunk = 200 << 10
	whole := int64(len(action(1)))

	// Connections of earlier tests may still be closing and give back what
	// they assembled. The budget is host-wide, so wait for it to be empty
	// before taking it as this test's base.
	for settle := time.Now().Add(5 * time.Second); assemblyBytes.Load() != 0 && time.Now().Before(settle); {
		time.Sleep(time.Millisecond)
	}
	// Other sheets' assemblies leave room for one such action and a half.
	base := assemblyBytes.Load()
	filler := assemblyBudget - base - whole*3/2
	assemblyBytes.Add(filler)
	t.Cleanup(func() { assemblyBytes.Add(-filler) })

	idle := s.dial("private", `{"type":"hello","version":1,"name":"Idle","pending":0}`)
	idle.next("welcome")
	idle.sendParts(1, action(1), chunk, 1)

	// While the first part is held, a second action does not fit.
	blocked := s.dial("private", `{"type":"hello","version":1,"name":"Blocked","pending":0}`)
	blocked.next("welcome")
	blocked.sendParts(1, action(1), chunk, 99)
	if f := blocked.next("fatal", "ack"); f["type"] != "fatal" {
		t.Fatalf("an action over the assembly budget got %v", f)
	}
	if !blocked.closed() {
		t.Fatal("the refused connection stays open")
	}
	held := func() int64 { return assemblyBytes.Load() - base - filler }
	deadline := time.Now().Add(5 * time.Second)
	for held() != chunk && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if held() != chunk {
		t.Fatalf("the idle connection holds %d bytes of the budget, want %d", held(), chunk)
	}

	// The lifetime ends with no frame from the idle connection.
	timers.mu.Lock()
	for _, timer := range timers.armed {
		if timer.d != actionPartTTL {
			t.Errorf("an assembly lives %v, want %v", timer.d, actionPartTTL)
		}
	}
	timers.mu.Unlock()
	if ran := timers.expire(); ran != 1 {
		t.Fatalf("%d assemblies expired, want the idle one", ran)
	}
	if held() != 0 {
		t.Fatalf("an expired assembly still holds %d bytes", held())
	}
	// The idle connection is told to connect again and send the action whole.
	if f := idle.next("fatal"); f["code"] != "unavailable" {
		t.Fatalf("the expired connection got %v", f)
	}

	writer := s.dial("private", `{"type":"hello","version":1,"name":"Writer","pending":0}`)
	writer.next("welcome")
	writer.sendParts(1, action(1), chunk, 99)
	if ack := writer.next("ack", "fatal"); !strings.Contains(fmt.Sprint(ack["results"]), "applied:30") {
		t.Fatalf("after the expiry the action got %v", ack)
	}
	if held() != 0 || timers.expire() != 0 {
		t.Fatalf("a finished assembly holds %d bytes or a live timer", held())
	}

	// A connection that leaves gives its assemblies back as well.
	gone := s.dial("private", `{"type":"hello","version":1,"name":"Gone","pending":0}`)
	gone.next("welcome")
	gone.sendParts(1, action(1), chunk, 1)
	for held() != chunk && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	gone.ws.Close(1000, "")
	for held() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if held() != 0 || timers.expire() != 0 {
		t.Fatalf("a closed connection leaves %d bytes or a live timer", held())
	}

	// More open assemblies than a client ever has end the connection.
	many := s.dial("private", `{"type":"hello","version":1,"name":"Many","pending":0}`)
	many.next("welcome")
	for n := 1; n <= maxAssemblies+1; n++ {
		many.send(fmt.Sprintf(`{"type":"action_part","n":"%d","part":0,"parts":2,"data":"x"}`, n))
	}
	if f := many.next("fatal"); f["code"] != "protocol" || !many.closed() {
		t.Fatalf("too many assemblies got %v", f)
	}
}

// runIDs turns id runs of a frame into the list of ids.
func runIDs(runs any) []uint64 {
	var out []uint64
	for _, id := range expandRuns(runs) {
		out = append(out, uint64(id))
	}
	return out
}

// loadsWhole watches a tab on a Private and on a Public connection. Each
// gets the whole snapshot through tab_done in frames inside the limit, with
// the tab's rows and columns in order, its style list and its cells.
func loadsWhole(t *testing.T, e *env, s *site, tab TabID, check func(route string, head map[string]any, cells map[string][]any)) {
	t.Helper()
	e.in.mu.RLock()
	tb := e.in.wb.byID[tab]
	rows, cols := slices.Clone(tb.rows.ids), slices.Clone(tb.cols.ids)
	styles := len(e.in.wb.wire.list)
	e.in.mu.RUnlock()
	for _, route := range []string{"private", "public"} {
		c := s.dial(route, helloV1)
		c.next("welcome")
		head, cells := c.watch(1, tab)
		if got := c.largest.Load(); got > serverFrameMax {
			t.Errorf("%s: a frame of %d bytes, more than %d", route, got, serverFrameMax)
		}
		if !slices.Equal(runIDs(head["rows"]), rows) || !slices.Equal(runIDs(head["cols"]), cols) {
			t.Errorf("%s: the head does not list the tab's rows and columns in order", route)
		}
		if got := len(head["styles"].([]any)); got != styles {
			t.Errorf("%s: the head lists %d styles, want %d", route, got, styles)
		}
		if head["seq"] != fmt.Sprint(e.seq()) {
			t.Errorf("%s: the head is at %v, want %d", route, head["seq"], e.seq())
		}
		check(route, head, cells)
		// The connection is still good: it gets the next delta.
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"After ` + route + `"}]}`)
		if d := c.next("delta"); d["seq"] != fmt.Sprint(e.seq()) {
			t.Errorf("%s: after the snapshot the connection got %v", route, d["seq"])
		}
		c.ws.Close(1000, "")
	}
}

// TestSnapshotWideRow loads a row that alone is larger than every send
// queue: 129 cells of 32,767 characters.
func TestSnapshotWideRow(t *testing.T) {
	heavy(t, "writes 4 MiB of text")
	e := start(t)
	text := strings.Repeat("a", 32767)
	e.mustAgent(`{"ops":[{"op":"insert_cols","tab":"Sheet1","at":"A","count":103}]}`)
	for start := 0; start < 129; start += 43 {
		cells := make([]string, 43)
		for i := range cells {
			cells[i] = `"` + text + `"`
		}
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"` + columnLetters(start) + `2","rows":[[` + strings.Join(cells, ",") + `]]}]}`)
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["above"]]},{"op":"write","tab":"Sheet1","at":"A3","rows":[["below"]]}]}`)
	loadsWhole(t, e, serve(e), e.tabID("Sheet1"), func(route string, _ map[string]any, cells map[string][]any) {
		if len(cells) != 131 {
			t.Fatalf("%s: the snapshot holds %d cells, want 131", route, len(cells))
		}
		for col := 0; col < 129; col++ {
			if cell := cells[cellKey(e, columnLetters(col)+"2")]; len(cell) < 3 || cell[2] != text {
				t.Fatalf("%s: %s2 is not the written text", route, columnLetters(col))
			}
		}
		if cells[cellKey(e, "A1")][2] != "above" || cells[cellKey(e, "A3")][2] != "below" {
			t.Fatalf("%s: the rows beside the wide row are wrong", route)
		}
	})
}

// TestSnapshotShuffledRows loads a tab of 100,000 rows in reverse id order,
// whose row list has one run for each row.
func TestSnapshotShuffledRows(t *testing.T) {
	heavy(t, "sorts 100,000 rows")
	e := start(t)
	e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":%d}]}`, formula.MaxRows-newTabRows))
	for from := 0; from < formula.MaxRows; from += 50000 {
		var b strings.Builder
		for i := 0; i < 50000; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "[%d]", from+i)
		}
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[%s]}]}`, from+1, b.String()))
	}
	e.mustAgent(`{"ops":[{"op":"sort","tab":"Sheet1","rows":"1:100000","by":"A","desc":true}]}`)
	loadsWhole(t, e, serve(e), e.tabID("Sheet1"), func(route string, head map[string]any, cells map[string][]any) {
		if runs := len(head["rows"].([]any)); runs < formula.MaxRows/2 {
			t.Fatalf("%s: the row list has %d runs; the rows are not shuffled", route, runs)
		}
		if len(cells) != formula.MaxRows {
			t.Fatalf("%s: the snapshot holds %d cells", route, len(cells))
		}
		if cell := cells[cellKey(e, "A1")]; cell[2] != float64(formula.MaxRows-1) {
			t.Fatalf("%s: A1 = %v after the sort", route, cell)
		}
	})
}

// TestSnapshotManyStyles loads a tab whose style list alone is larger than
// a frame.
func TestSnapshotManyStyles(t *testing.T) {
	e := start(t)
	const formats = 9000
	e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":%d}]}`, formats-newTabRows))
	for from := 0; from < formats; from += 3000 {
		ops := make([]string, 0, 3000)
		for i := from; i < from+3000; i++ {
			symbol := string([]rune{rune('A' + i%26), rune('A' + i/26%26), rune('A' + i/676%26)})
			ops = append(ops, fmt.Sprintf(`{"op":"format","tab":"Sheet1","ranges":["A%d"],"format":{"bold":true,"italic":true,"underline":true,"strikethrough":true,"wrap":true,"number":{"kind":"currency","symbol":"%s","decimals":2}}}`, i+1, symbol))
		}
		e.mustAgent(`{"ops":[` + strings.Join(ops, ",") + `]}`)
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[5]]}]}`)
	rev := e.b.importAll(t)
	s := serve(e)
	// History reads the same tab over HTTP in the same frames, one to a
	// line: the head goes on in tab_more frames there too.
	for {
		res, err := http.Get(fmt.Sprintf("%s/private/_sheet/api/revisions/%d/tabs/%d", s.server.URL, rev, e.tabID("Sheet1")))
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode == http.StatusServiceUnavailable && strings.Contains(string(data), `"calculating"`) {
			continue
		}
		frames, ferr := ndjsonFrames(data)
		kinds := map[string]int{}
		styles := 0
		for _, f := range frames {
			f := f.(map[string]any)
			kinds[f["type"].(string)]++
			if list, ok := f["styles"].([]any); ok {
				styles += len(list)
			}
		}
		if err != nil || ferr != nil || kinds["tab_head"] != 1 || kinds["tab_more"] == 0 || kinds["tab_done"] != 1 || styles != formats+1 {
			t.Fatalf("the History frames of the tab: %v, %d styles, %v %v", kinds, styles, err, ferr)
		}
		break
	}
	loadsWhole(t, e, s, e.tabID("Sheet1"), func(route string, head map[string]any, cells map[string][]any) {
		styles := head["styles"].([]any)
		if len(styles) != formats+1 {
			t.Fatalf("%s: %d styles", route, len(styles))
		}
		// Every cell names its own format by its place in the list.
		for _, addr := range []string{"A1", "A2", fmt.Sprintf("A%d", formats)} {
			cell := cells[cellKey(e, addr)]
			index := int(cell[len(cell)-1].(float64))
			number := styles[index].(map[string]any)["number"].(map[string]any)
			_, row, _ := e.ids(addr)
			e.in.mu.RLock()
			pos := e.in.wb.byID[e.tabID("Sheet1")].rows.pos[uint64(row)]
			e.in.mu.RUnlock()
			want := string([]rune{rune('A' + int(pos)%26), rune('A' + int(pos)/26%26), rune('A' + int(pos)/676%26)})
			if number["symbol"] != want {
				t.Fatalf("%s: %s has the style %v, want the symbol %s", route, addr, number, want)
			}
		}
		if cell := cells[cellKey(e, "A1")]; cell[3] != "AAA5.00" {
			t.Fatalf("%s: A1 shows %v", route, cell[3])
		}
	})
}

// TestLaggingConnectionResync stops reading on a connection while edits
// arrive. When its send queue is full the connection gets one resync with
// a new generation and a new snapshot of its tab, and deltas go on from
// the snapshot's sequence. A connection that falls behind again is closed.
func TestLaggingConnectionResync(t *testing.T) {
	t.Run("snapshot at once", func(t *testing.T) { laggingConnection(t, false) })
	// With the capture budget full the new snapshot waits. Until it is
	// out, the connection gets no cells of the tab it lost track of.
	t.Run("snapshot after a wait", func(t *testing.T) { laggingConnection(t, true) })
}

func laggingConnection(t *testing.T, budgetFull bool) {
	e := start(t)
	// Content that makes a snapshot larger than the room a full queue has.
	filler := make([]string, 20)
	for i := range filler {
		filler[i] = `"` + strings.Repeat("f", 30000) + `"`
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A3","rows":[[` + strings.Join(filler, ",") + `]]}]}`)
	s := serve(e)
	tab := e.tabID("Sheet1")
	host := strings.TrimPrefix(s.server.URL, "http://")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, err := wsconn.Dial(ctx, func(ctx context.Context) (net.Conn, error) { return new(net.Dialer).DialContext(ctx, "tcp", host) }, host, "/private/_sheet/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close(1000, "")
	// read returns the next frame; this test reads only when it wants to.
	read := func() (map[string]any, error) {
		raw, err := ws.ReadMessage(64 << 20)
		if err != nil {
			return nil, err
		}
		var f map[string]any
		if err := json.Unmarshal([]byte(raw), &f); err != nil {
			t.Fatal(err)
		}
		return f, nil
	}
	until := func(kind string) map[string]any {
		t.Helper()
		for {
			f, err := read()
			if err != nil {
				t.Fatalf("the connection ended before %s: %v", kind, err)
			}
			if f["type"] == kind {
				return f
			}
		}
	}
	if err := ws.Send(`{"type":"hello","version":1,"name":"Slow","pending":0}`); err != nil {
		t.Fatal(err)
	}
	until("welcome")
	if err := ws.Send(fmt.Sprintf(`{"type":"watch","generation":1,"tab":%d}`, tab)); err != nil {
		t.Fatal(err)
	}
	until("tab_done")

	e.in.hub.mu.Lock()
	var server *conn
	for c := range e.in.hub.conns {
		server = c
	}
	e.in.hub.mu.Unlock()
	queued := func() int64 { return server.ws.Budget().Used() }
	// Each edit is one delta of a little over size bytes for the watched
	// tab. edit[seq] is the number an edit wrote into A1.
	const size = 8 * 30000
	edit := map[uint64]int{}
	write := func() {
		t.Helper()
		n := len(edit) + 1
		cells := make([]string, 8)
		for i := range cells {
			cells[i] = fmt.Sprintf(`"%d %s"`, n, strings.Repeat("x", 30000-8))
		}
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[` + strings.Join(cells, ",") + `]]}]}`)
		edit[e.seq()] = n
	}
	// settled returns the queued bytes once they stop changing: the peer
	// does not read, so the queue only moves when the sender adds to it.
	settled := func() int64 {
		for last, same := queued(), 0; ; {
			time.Sleep(5 * time.Millisecond)
			if now := queued(); now != last {
				last, same = now, 0
			} else if same++; same == 4 {
				return last
			}
		}
	}
	// fallBehind writes until a delta no longer fits the unread
	// connection's queue, and stops there: that delta starts the resync.
	fallBehind := func() {
		t.Helper()
		for n := 0; settled() <= privateQueue-(64<<10)-size-(16<<10); n++ {
			if n > 400 {
				t.Fatalf("the queue holds %d bytes after %d unread edits", queued(), n)
			}
			write()
		}
		for range 2 {
			before := settled()
			write()
			for wait := time.Now().Add(2 * time.Second); queued() == before && time.Now().Before(wait); {
				time.Sleep(time.Millisecond)
			}
			if settled()-before < size {
				return // the delta was not queued
			}
		}
		t.Fatalf("two more deltas fit a queue of %d bytes", queued())
	}
	release := func() {}
	if budgetFull {
		release = fillCaptures(t, 0)
	}
	fallBehind()
	between := uint64(0)
	if budgetFull {
		// The resync is out and its snapshot waits for the budget. An edit
		// of the tab now reaches the connection as a delta without cells.
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"B2","rows":[["between"]]}]}`)
		between = e.seq()
		edit[between] = len(edit)
		settled()
		release()
	}

	// Reading again brings the queued deltas, then the resync.
	var prev uint64
	var resync map[string]any
	for resync == nil {
		f, err := read()
		if err != nil {
			t.Fatalf("the lagging connection ended without a resync: %v", err)
		}
		switch f["type"] {
		case "delta":
			seq, _ := parseDecimal(f["seq"].(string))
			if prev != 0 && seq != prev+1 {
				t.Fatalf("delta %d follows %d", seq, prev)
			}
			prev = seq
		case "resync":
			resync = f
		case "tab_head", "rows", "tab_done":
			t.Fatalf("a %v frame before the resync", f["type"])
		}
	}
	// The resync names the sequence the connection was cut off at: after
	// the deltas it got, with at least one delta lost.
	at, _ := parseDecimal(resync["seq"].(string))
	if resync["generation"] != float64(2) || at <= prev || at > e.seq() {
		t.Fatalf("resync %v after delta %d, with the sheet at %d", resync, prev, e.seq())
	}
	// The snapshot follows, with nothing of the tab's old state between.
	var head map[string]any
	shown := ""
	for done := false; !done; {
		f, err := read()
		if err != nil {
			t.Fatalf("the connection ended inside the new snapshot: %v", err)
		}
		switch f["type"] {
		case "delta":
			if f["cells"] != nil || f["struct"] != nil {
				t.Fatalf("a delta with cells between the resync and its snapshot: seq %v", f["seq"])
			}
			if f["seq"] == fmt.Sprint(between) {
				between = 0
			}
		case "tab_head":
			head = f
		case "rows":
			for _, row := range f["rows"].([]any) {
				for _, cell := range row.([]any)[1].([]any) {
					if arr := cell.([]any); fmt.Sprint(row.([]any)[0], "/", arr[0]) == cellKey(e, "A1") {
						shown = arr[2].(string)
					}
				}
			}
		case "tab_done":
			done = true
		}
	}
	if head == nil || head["generation"] != float64(2) || head["tab"] != float64(tab) {
		t.Fatalf("the head after the resync: %v", head)
	}
	if between != 0 {
		t.Fatalf("the edit at %d did not reach the waiting connection as a delta", between)
	}
	from, _ := parseDecimal(head["seq"].(string))
	if from < at || !strings.HasPrefix(shown, fmt.Sprint(edit[from], " ")) {
		t.Fatalf("the snapshot at %d after a resync at %d has A1 = %.12q, want the edit %d", from, at, shown, edit[from])
	}
	// The stream goes on from the snapshot, one delta for each sequence.
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A2","rows":[["after"]]}]}`)
	for next := from + 1; next <= e.seq(); next++ {
		if d := until("delta"); d["seq"] != fmt.Sprint(next) || d["cells"] == nil {
			t.Fatalf("after the snapshot at %d the connection got the delta %v, want %d with its cells", from, d["seq"], next)
		}
	}

	// Falling behind a second time ends the connection.
	for n := 0; ; n++ {
		if code, _ := server.ws.CloseStatus(); code != 0 {
			break
		}
		if n > 400 {
			t.Fatalf("the connection is open after %d unread edits", n)
		}
		write()
		time.Sleep(2 * time.Millisecond)
	}
	for {
		f, err := read()
		if err != nil {
			break
		}
		if f["type"] == "resync" || f["type"] == "tab_head" {
			t.Fatalf("a second %v on a connection that lags again", f["type"])
		}
	}
	if code, reason := server.ws.CloseStatus(); code != 1008 || reason != "lagging" {
		t.Fatalf("the server closed the connection with %d %q", code, reason)
	}
}

// TestDeltaPartsStayInsideFrameLimit sends a delta that is larger than one
// frame and made of characters that a JSON string writes longer than they
// are. Every part a Private and a Public connection receives is inside the
// frame limit, and the parts join to the whole delta.
func TestDeltaPartsStayInsideFrameLimit(t *testing.T) {
	for name, fill := range map[string]string{
		"html":      "<>&",
		"quotes":    `"\`,
		"control":   "\x01\x1f",
		"separator": "  ",
	} {
		t.Run(name, func(t *testing.T) {
			e := start(t)
			s := serve(e)
			tab := e.tabID("Sheet1")
			watchers := map[string]*client{}
			for _, route := range []string{"private", "public"} {
				c := s.dial(route, helloV1)
				c.next("welcome")
				c.watch(1, tab)
				watchers[route] = c
			}
			text, _ := json.Marshal(strings.Repeat(fill, 12000/len([]rune(fill))))
			rows := strings.TrimSuffix(strings.Repeat("["+string(text)+"],", 60), ",")
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + rows + `]}]}`)
			for route, c := range watchers {
				d := c.next("delta")
				if got := len(d["cells"].([]any)); got != 60 {
					t.Errorf("%s: the joined delta holds %d rows", route, got)
				}
				if got := c.largest.Load(); got > serverFrameMax {
					t.Errorf("%s: a frame of %d bytes, more than %d", route, got, serverFrameMax)
				}
				// The same cells as a snapshot, for a connection that
				// comes later.
				late := s.dial(route, helloV1)
				late.next("welcome")
				if _, cells := late.watch(1, tab); len(cells) != 60 {
					t.Errorf("%s: the snapshot holds %d cells", route, len(cells))
				}
				if got := late.largest.Load(); got > serverFrameMax {
					t.Errorf("%s: a snapshot frame of %d bytes, more than %d", route, got, serverFrameMax)
				}
			}
		})
	}
}

// TestPresenceFrameIsBounded fills a sheet's Private connections with
// people whose names and selected ranges are as large as a presence frame
// takes. The list each connection receives stays inside the frame limit. A
// range in more runs than a presence frame may carry, or in another form
// than id runs, is a protocol error and reaches nobody.
func TestPresenceFrameIsBounded(t *testing.T) {
	e := start(t)
	s := serve(e)
	tab := e.tabID("Sheet1")
	runs := func(n int) string {
		list := make([]string, n)
		for i := range list {
			list[i] = fmt.Sprintf("[%d,1]", maxID-2-2*uint64(i))
		}
		return "[" + strings.Join(list, ",") + "]"
	}
	name, _ := json.Marshal(strings.Repeat(`"`, 200))
	hello := fmt.Sprintf(`{"type":"hello","version":1,"name":%s,"pending":0}`, name)
	first := s.dial("private", hello)
	first.next("welcome")
	// A range names ids the sheet has given out: the new tab has the rows
	// 1 to 1000 and the columns 1 to 26. A selection of the whole tab is
	// read in either form.
	sender := s.dial("private", helloV1)
	sender.next("welcome")
	for _, rng := range []string{`{"rows":"all","cols":"all"}`, `{"rows":[[1,1000]],"cols":[[1,26]]}`, `{"rows":[[1000,1],[1,999]],"cols":[[26,1]]}`} {
		sender.send(fmt.Sprintf(`{"type":"presence","tab":%d,"active":[1000,26],"range":%s}`, tab, rng))
		var got map[string]any
		for got == nil {
			for _, p := range first.next("presence")["people"].([]any) {
				got, _ = p.(map[string]any)["range"].(map[string]any)
			}
		}
		want := map[string]any{}
		_ = json.Unmarshal([]byte(rng), &want)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("the range %s reached the others as %v", rng, got)
		}
		time.Sleep(60 * time.Millisecond) // under the 20 updates a second
	}
	sender.ws.Close(1000, "")
	for name, frame := range map[string]string{
		"a billion rows":       `"range":{"rows":[[1,1000000000]],"cols":[[1,1]]}`,
		"a row never given":    `"range":{"rows":[[1001,1]],"cols":[[1,1]]}`,
		"a run past the rows":  `"range":{"rows":[[999,3]],"cols":[[1,1]]}`,
		"a column never given": `"range":{"rows":[[1,1]],"cols":[[27,1]]}`,
		"overlapping runs":     `"range":{"rows":[[5,10],[9,3]],"cols":[[1,1]]}`,
		"a run twice":          `"range":{"rows":[[5,1]],"cols":[[2,2],[2,2]]}`,
		"an active row":        `"active":[1001,1]`,
		"an active column":     `"active":[1,27]`,
		"an active zero":       `"active":[0,1]`,
	} {
		c := s.dial("private", helloV1)
		c.next("welcome")
		c.send(fmt.Sprintf(`{"type":"presence","tab":%d,%s}`, tab, frame))
		if f := c.next("fatal"); f["code"] != "protocol" {
			t.Errorf("%s: fatal = %v", name, f)
		}
	}
	for _, other := range []uint64{uint64(tab) + 1, maxID} {
		c := s.dial("private", helloV1)
		c.next("welcome")
		c.send(fmt.Sprintf(`{"type":"presence","tab":%d}`, other))
		if f := c.next("fatal"); f["code"] != "protocol" {
			t.Errorf("a tab never given: fatal = %v", f)
		}
	}
	// The rest of the test is about sizes: a sheet that has given out
	// nearly every id, so that ids take their most digits.
	e.in.mu.Lock()
	e.in.wb.nextRow, e.in.wb.nextCol = maxID, maxID
	e.in.mu.Unlock()
	ids := make([]string, 12000)
	for i := range ids {
		ids[i] = fmt.Sprint(maxID - 1 - uint64(i))
	}
	for name, rng := range map[string]string{
		"too many runs":           fmt.Sprintf(`{"rows":%s,"cols":[[1,1]]}`, runs(presenceRuns)),
		"single ids":              fmt.Sprintf(`{"rows":[%s],"cols":[[1,1]]}`, strings.Join(ids, ",")),
		"added rows":              `{"rows":[{"new":1}],"cols":[[1,1]]}`,
		"empty run":               `{"rows":[[1,0]],"cols":[[1,1]]}`,
		"past the ids":            fmt.Sprintf(`{"rows":[[%d,2]],"cols":[[1,1]]}`, uint64(maxID-1)),
		"more rows than a tab":    fmt.Sprintf(`{"rows":[[1,%d]],"cols":[[1,1]]}`, formula.MaxRows+1),
		"more columns than a tab": fmt.Sprintf(`{"rows":[[1,1]],"cols":[[1,700],[800,3]]}`),
	} {
		// Three people send it, so that an echo of it would pass the limit.
		for i := 0; i < 3; i++ {
			c := s.dial("private", helloV1)
			c.next("welcome")
			c.send(fmt.Sprintf(`{"type":"presence","tab":%d,"range":%s}`, tab, rng))
			if f := c.next("fatal"); f["code"] != "protocol" {
				t.Errorf("%s: fatal = %v", name, f)
			}
		}
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		e.in.hub.mu.Lock()
		open := e.in.hub.private
		e.in.hub.mu.Unlock()
		if open == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d Private connections are still open", open)
		}
	}
	people := []*client{first}
	for len(people) < maxPrivateConns {
		c := s.dial("private", hello)
		c.next("welcome")
		people = append(people, c)
	}
	for _, c := range people {
		c.send(fmt.Sprintf(`{"type":"presence","tab":%d,"active":[%d,%d],"range":{"rows":%s,"cols":%s},"editing":true}`,
			tab, uint64(maxID-1), uint64(maxID-1), runs(presenceRuns/2), runs(presenceRuns/2)))
	}
	for i, c := range people {
		for {
			list := c.next("presence")["people"].([]any)
			if len(list) < len(people)-1 {
				continue
			}
			for _, p := range list {
				if rng, ok := p.(map[string]any)["range"].(map[string]any); !ok || len(rng["rows"].([]any)) != presenceRuns/2 {
					t.Fatalf("connection %d: a person's range arrived as %v", i, p.(map[string]any)["range"])
				}
			}
			break
		}
		if got := c.largest.Load(); got > serverFrameMax {
			t.Fatalf("connection %d: a frame of %d bytes, more than %d", i, got, serverFrameMax)
		}
	}
}

// discard is a connection that reads every frame and keeps none: only the
// newest sequence it saw.
type discard struct {
	ws  *wsconn.Conn
	seq atomic.Uint64
}

func (s *site) discard(route string, tab TabID) *discard {
	s.e.t.Helper()
	host := strings.TrimPrefix(s.server.URL, "http://")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, err := wsconn.Dial(ctx, func(ctx context.Context) (net.Conn, error) { return new(net.Dialer).DialContext(ctx, "tcp", host) }, host, "/"+route+"/_sheet/ws")
	if err != nil {
		s.e.t.Fatal(err)
	}
	d := &discard{ws: ws}
	s.e.t.Cleanup(func() { ws.Close(1000, "") })
	for _, frame := range []string{helloV1, fmt.Sprintf(`{"type":"watch","generation":1,"tab":%d}`, tab)} {
		if err := ws.Send(frame); err != nil {
			s.e.t.Fatal(err)
		}
	}
	go func() {
		for {
			raw, err := ws.ReadMessage(64 << 20)
			if err != nil {
				return
			}
			var f struct {
				Type string
				Seq  string
				Part int
			}
			// The welcome names the sequence the stream starts after. A
			// delta in parts is whole with its last part; the sequence of
			// the first tells the same.
			if json.Unmarshal([]byte(raw), &f) == nil && (f.Type == "welcome" || f.Type == "delta" || f.Type == "delta_part") {
				if seq, ok := parseDecimal(f.Seq); ok && seq > d.seq.Load() {
					d.seq.Store(seq)
				}
			}
		}
	}()
	return d
}

// logRetained adds up what the delta log holds, entry by entry.
func logRetained(h *hub) (source, frames int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, rec := range h.log {
		source += rec.size
		for _, f := range rec.frames {
			frames += len(f)
		}
	}
	return source, frames
}

// TestDeltaLogCountsProjectedFrames watches many tabs while one tab is
// sorted again and again. Every connection gets every structural edit, in
// a frame of its own tab. The frames the log keeps for reuse count toward
// its byte bound with their sources, and the memory the log retains stays
// inside that bound.
func TestDeltaLogCountsProjectedFrames(t *testing.T) {
	heavy(t, "sorts 20,000 rows forty times")
	e := start(t)
	const rows, tabs, sorts = 20000, 40, 40
	e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":%d}]}`, rows-newTabRows))
	var b strings.Builder
	for i := 0; i < rows; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "[%d]", i)
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + b.String() + `]}]}`)
	var add []string
	for i := 1; i < tabs; i++ {
		add = append(add, fmt.Sprintf(`{"op":"add_tab","name":"T%d"}`, i))
	}
	e.mustAgent(`{"ops":[` + strings.Join(add, ",") + `]}`)
	s := serve(e)
	var readers []*discard
	e.in.mu.RLock()
	ids := make([]TabID, 0, tabs)
	for _, tab := range e.in.wb.tabs {
		ids = append(ids, tab.id)
	}
	e.in.mu.RUnlock()
	for _, id := range ids {
		readers = append(readers, s.discard("private", id))
	}
	caughtUp := func() {
		t.Helper()
		want := e.seq()
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Millisecond) {
			behind := 0
			for _, r := range readers {
				if r.seq.Load() < want {
					behind++
				}
			}
			if behind == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d connections did not get the delta %d", behind, want)
			}
		}
	}
	sort := func(i int) {
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"sort","tab":"Sheet1","rows":"1:%d","by":"A","desc":%v}]}`, rows, i%2 == 0))
		caughtUp()
	}
	sort(0)
	one, _ := logRetained(e.in.hub)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	projected := 0
	for i := 1; i <= sorts; i++ {
		sort(i)
		source, frames := logRetained(e.in.hub)
		e.in.hub.mu.Lock()
		counted, kept := e.in.hub.bytes, e.in.hub.kept
		e.in.hub.mu.Unlock()
		if counted != source+frames || kept != frames {
			t.Fatalf("after sort %d the log counts %d bytes with %d of kept frames; it holds %d of entries and %d of frames", i, counted, kept, source, frames)
		}
		if counted > deltaLogBytes {
			t.Fatalf("after sort %d the log holds %d bytes, more than %d", i, counted, deltaLogBytes)
		}
		projected += tabs * (source / (i + 1))
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	// Without the bound the log would keep a frame of every sort for every
	// watched tab.
	if projected < 4*deltaLogBytes {
		t.Fatalf("the sorts project only %d bytes; the test needs more than the log bound", projected)
	}
	if grown := int64(after.HeapAlloc) - int64(before.HeapAlloc); grown > deltaLogBytes+(16<<20) {
		t.Fatalf("the heap grew by %d bytes over %d sorts of about %d bytes for %d tabs; the log may hold %d", grown, sorts, one, tabs, deltaLogBytes)
	}
	// A connection that reads an older entry still gets its frame.
	late := s.dial("private", helloV1)
	late.next("welcome")
	late.watch(1, ids[1])
	e.mustAgent(`{"ops":[{"op":"write","tab":"T1","at":"A1","rows":[["x"]]}]}`)
	if d := late.next("delta"); d["seq"] != fmt.Sprint(e.seq()) || d["cells"] == nil {
		t.Fatalf("a delta after the sorts: %v", d["seq"])
	}
}
