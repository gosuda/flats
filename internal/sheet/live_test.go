package sheet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
	"github.com/gosuda/flats/internal/wsconn"
)

// site is a host in front of an instance: it makes the route decision core
// makes, by path prefix, and strips what a client must not send.
type site struct {
	e      *env
	server *httptest.Server
	epoch  atomic.Value
}

// serve starts the host of a test. wrap, when given, goes around the
// handler, for a test that watches what the handler does.
func serve(e *env, wrap ...func(http.Handler) http.Handler) *site {
	s := &site{e: e}
	s.epoch.Store("1")
	var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range []string{"X-Flats-Access", "X-Flats-Access-Epoch", "Tailscale-User-Login", "Tailscale-User-Name"} {
			r.Header.Del(name)
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/private"):
			r.URL.Path = strings.TrimPrefix(r.URL.Path, "/private")
			r.Header.Set("X-Flats-Access", "private")
		case strings.HasPrefix(r.URL.Path, "/tailscale"):
			r.URL.Path = strings.TrimPrefix(r.URL.Path, "/tailscale")
			r.Header.Set("X-Flats-Access", "private")
			r.Header.Set("Tailscale-User-Login", "ada@example.com")
			r.Header.Set("Tailscale-User-Name", "Ada")
		case strings.HasPrefix(r.URL.Path, "/public"):
			r.URL.Path = strings.TrimPrefix(r.URL.Path, "/public")
		}
		r.Header.Set("X-Flats-Access-Epoch", s.epoch.Load().(string))
		if r.Header.Get("Origin") == "" {
			r.Header.Set("Sec-Fetch-Site", "same-origin")
		}
		e.in.ServeHTTP(w, r)
	})
	for _, w := range wrap {
		handler = w(handler)
	}
	s.server = httptest.NewServer(handler)
	e.t.Cleanup(s.server.Close)
	return s
}

type client struct {
	t       testing.TB
	ws      *wsconn.Conn
	frames  chan map[string]any
	seen    []map[string]any
	parts   map[string][]map[string]any
	largest atomic.Int64 // bytes of the largest frame received
}

func (s *site) dial(route, hello string) *client {
	s.e.t.Helper()
	host := strings.TrimPrefix(s.server.URL, "http://")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, err := wsconn.Dial(ctx, func(ctx context.Context) (net.Conn, error) { return new(net.Dialer).DialContext(ctx, "tcp", host) }, host, "/"+route+"/_sheet/ws")
	if err != nil {
		s.e.t.Fatalf("dial %s: %v", route, err)
	}
	c := &client{t: s.e.t, ws: ws, frames: make(chan map[string]any, 4096), parts: map[string][]map[string]any{}}
	go func() {
		defer close(c.frames)
		for {
			raw, err := ws.ReadMessage(64 << 20)
			if err != nil {
				return
			}
			if n := int64(len(raw)); n > c.largest.Load() {
				c.largest.Store(n)
			}
			var f map[string]any
			if json.Unmarshal([]byte(raw), &f) != nil {
				f = map[string]any{"type": "garbage", "raw": raw}
			}
			c.frames <- f
		}
	}()
	s.e.t.Cleanup(func() { ws.Close(1000, "") })
	if hello != "" {
		c.send(hello)
	}
	return c
}

func (c *client) send(frame string) {
	c.t.Helper()
	if err := c.ws.Send(frame); err != nil {
		c.t.Fatalf("send: %v", err)
	}
}

// next returns the next frame of one of the types and passes over others.
// It fails when the connection ends or nothing arrives in time.
func (c *client) next(types ...string) map[string]any {
	c.t.Helper()
	f, ok := c.wait(types...)
	if !ok {
		c.t.Fatalf("no %v frame; saw %v", types, c.kinds())
	}
	return f
}

func (c *client) wait(types ...string) (map[string]any, bool) {
	timeout := time.After(10 * time.Second)
	for {
		select {
		case f, ok := <-c.frames:
			if !ok {
				return nil, false
			}
			if f["type"] == "delta_part" {
				// Parts are numbered from 0 and join to the delta.
				seq := f["seq"].(string)
				c.parts[seq] = append(c.parts[seq], f)
				if len(c.parts[seq]) < int(f["parts"].(float64)) {
					continue
				}
				var err error
				if f, err = joinDeltaParts(c.parts[seq]); err != nil {
					c.t.Fatalf("joined delta parts: %v", err)
				}
				delete(c.parts, seq)
			}
			c.seen = append(c.seen, f)
			if slices.Contains(types, f["type"].(string)) {
				return f, true
			}
		case <-timeout:
			return nil, false
		}
	}
}

func (c *client) kinds() []string {
	out := make([]string, len(c.seen))
	for i, f := range c.seen {
		out[i] = fmt.Sprint(f["type"])
	}
	return out
}

// closed waits for the connection to end.
func (c *client) closed() bool {
	_, ok := c.wait("never")
	return !ok
}

// watch asks for a tab and returns its head and its cells by row and column
// id after tab_done.
func (c *client) watch(generation int, tab TabID) (map[string]any, map[string][]any) {
	c.t.Helper()
	c.send(fmt.Sprintf(`{"type":"watch","generation":%d,"tab":%d}`, generation, tab))
	return c.tab()
}

func (c *client) tab() (map[string]any, map[string][]any) {
	c.t.Helper()
	head := c.next("tab_head")
	// A head larger than one frame goes on in tab_more frames, which
	// nothing may come between.
	for head["more"] == true {
		more := c.next("tab_more", "rows", "tab_done", "delta", "tab_head")
		if more["type"] != "tab_more" || more["generation"] != head["generation"] || more["tab"] != head["tab"] {
			c.t.Fatalf("an unfinished head was followed by %v", more["type"])
		}
		for _, list := range []string{"rows", "styles"} {
			if add, ok := more[list].([]any); ok {
				had, _ := head[list].([]any)
				head[list] = append(had, add...)
			}
		}
		head["more"] = more["more"]
	}
	delete(head, "more")
	cells := map[string][]any{}
	for {
		f := c.next("rows", "tab_done")
		if f["type"] == "tab_done" {
			return head, cells
		}
		for _, row := range f["rows"].([]any) {
			pair := row.([]any)
			for _, cell := range pair[1].([]any) {
				arr := cell.([]any)
				key := fmt.Sprint(pair[0], "/", arr[0])
				if _, twice := cells[key]; twice {
					c.t.Fatalf("the snapshot carries the cell %s twice", key)
				}
				cells[key] = arr
			}
		}
	}
}

func cellKey(e *env, addr string) string {
	_, row, col := e.ids(addr)
	return fmt.Sprint(float64(row), "/", float64(col))
}

const helloV1 = `{"type":"hello","version":1,"pending":0}`

func TestLiveSnapshotAndDeltas(t *testing.T) {
	e := start(t)
	e.mustAgent(offsite)
	s := serve(e)
	tab := e.tabID("Budget")
	alice := s.dial("private", `{"type":"hello","version":1,"name":"Alice","pending":0}`)
	welcome := alice.next("welcome")
	if welcome["access"] != "private" || welcome["session"] == "" || welcome["seq"] != fmt.Sprint(e.seq()) || welcome["date"] != "2026-11-13" {
		t.Fatalf("welcome = %v", welcome)
	}
	me := welcome["me"].(map[string]any)
	if me["label"] != "Alice" || me["verified"] != false {
		t.Fatalf("me = %v", me)
	}
	tabs := welcome["tabs"].([]any)[0].(map[string]any)
	if tabs["name"] != "Budget" || tabs["rows"] != 1000.0 || tabs["cols"] != 26.0 {
		t.Fatalf("tabs = %v", tabs)
	}
	head, cells := alice.watch(1, tab)
	if head["generation"] != 1.0 || head["seq"] != welcome["seq"] || fmt.Sprint(head["rows"]) != "[[1 1000]]" || fmt.Sprint(head["cols"]) != "[[1 26]]" {
		t.Fatalf("tab_head = %v", head)
	}
	styles := head["styles"].([]any)
	if len(styles) < 3 || len(styles[0].(map[string]any)) != 0 {
		t.Fatalf("styles = %v, want the default format at index 0", styles)
	}
	// CellOut: [col, kind, value, shown, edit, style] with trailing
	// defaults left out, and never the text of a formula.
	b2, c2, a1 := cells[cellKey(e, "Budget!B2")], cells[cellKey(e, "Budget!C2")], cells[cellKey(e, "Budget!A2")]
	if len(b2) != 6 || b2[1] != "n" || b2[2] != 1200.0 || b2[3] != "$1,200.00" || b2[4] != "1200" {
		t.Fatalf("B2 = %v", b2)
	}
	if len(c2) != 6 || c2[1] != "nf" || c2[3] != "57.14%" || c2[4] != nil {
		t.Fatalf("C2 = %v", c2)
	}
	if len(a1) != 3 || a1[1] != "t" || a1[2] != "Venue" {
		t.Fatalf("A2 = %v", a1)
	}
	if _, blank := cells[cellKey(e, "Budget!D5")]; blank {
		t.Fatal("a blank cell without a format is in the snapshot")
	}

	// A second editor and a Public viewer see an edit and the values that
	// follow from it in one delta; the author's ack comes after its delta.
	bob := s.dial("private", `{"type":"hello","version":1,"name":"Bob","pending":0}`)
	bob.next("welcome")
	bob.watch(1, tab)
	viewer := s.dial("public", helloV1)
	if w := viewer.next("welcome"); w["access"] != "public" || w["session"] != nil || w["me"] != nil {
		t.Fatalf("public welcome = %v", w)
	}
	viewer.watch(1, tab)
	base := e.seq()
	_, row, col := e.ids("Budget!B2")
	alice.send(fmt.Sprintf(`{"type":"action","n":"1","base":"%d","ops":[{"op":"set","tab":%d,"row":%d,"col":%d,"text":"1500","at":[1,1]}]}`, base, tab, row, col))
	ack := alice.next("delta", "ack")
	if ack["type"] != "delta" {
		t.Fatalf("the ack came before the delta: %v", ack)
	}
	ack = alice.next("ack")
	if ack["n"] != "1" || ack["seq"] != fmt.Sprint(base+1) || fmt.Sprint(ack["results"]) != "[map[applied:1 status:applied]]" {
		t.Fatalf("ack = %v", ack)
	}
	for name, c := range map[string]*client{"bob": bob, "viewer": viewer} {
		d := c.next("delta")
		got := map[string][]any{}
		for _, r := range d["cells"].([]any) {
			pair := r.([]any)
			for _, cell := range pair[1].([]any) {
				got[fmt.Sprint(pair[0], "/", cell.([]any)[0])] = cell.([]any)
			}
		}
		if d["seq"] != fmt.Sprint(base+1) || got[cellKey(e, "Budget!B2")][2] != 1500.0 || got[cellKey(e, "Budget!B9")][3] != "$2,400.00" || got[cellKey(e, "Budget!C3")][3] != "37.50%" {
			t.Fatalf("%s delta = %v", name, d)
		}
		if name == "viewer" && (d["from"] != nil || d["note"] != nil) {
			t.Fatalf("a Public delta names its author: %v", d)
		}
		if name == "bob" && d["from"].(map[string]any)["n"] != "1" {
			t.Fatalf("delta from = %v", d["from"])
		}
	}

	// peek returns the A1 text of a formula; a Public viewer may peek.
	_, frow, fcol := e.ids("Budget!C3")
	viewer.send(fmt.Sprintf(`{"type":"peek","id":7,"tab":%d,"row":%d,"col":%d}`, tab, frow, fcol))
	if p := viewer.next("peeked"); p["id"] != 7.0 || p["edit"] != "=B3/$B$9" || p["formula"] != true {
		t.Fatalf("peeked = %v", p)
	}

	// An agent write reaches Private connections with a note, and a person
	// whose content it replaced gets an overwritten frame.
	e.mustAgent(`{"ops":[{"op":"write","tab":"Budget","at":"B2","rows":[[1300]]}]}`)
	if note := bob.next("delta")["note"].(map[string]any); note["agent"] != true || note["cells"] != 1.0 {
		t.Fatalf("note = %v", note)
	}
	if o := alice.next("overwritten"); o["row"] != float64(row) || o["replaced"] != fmt.Sprint(base+1) {
		t.Fatalf("overwritten = %v", o)
	}
	if d := viewer.next("delta"); d["note"] != nil {
		t.Fatalf("a Public delta carries the agent note: %v", d)
	}

	// A resent action returns its stored ack and applies nothing.
	seq := e.seq()
	alice.send(fmt.Sprintf(`{"type":"action","n":"1","base":"%d","ops":[{"op":"set","tab":%d,"row":%d,"col":%d,"text":"1500","at":[1,1]}]}`, base, tab, row, col))
	if again := alice.next("ack"); again["seq"] != fmt.Sprint(base+1) || e.seq() != seq {
		t.Fatalf("resent action: %v at sequence %d", again, e.seq())
	}
	e.want("Budget!B2", "$1,300.00")
}

// TestLiveStructureAndSettings is C-09 with section 12.3: every value group
// travels in a delta, to Private and Public connections alike, through
// undo and redo, without a new snapshot.
func TestLiveStructureAndSettings(t *testing.T) {
	e := start(t)
	e.mustAgent(numbers)
	s := serve(e)
	tab := e.tabID("Sheet1")
	alice := s.dial("private", `{"type":"hello","version":1,"name":"Alice","pending":0}`)
	alice.next("welcome")
	alice.watch(1, tab)
	bob := s.dial("private", `{"type":"hello","version":1,"name":"Bob","pending":0}`)
	bob.next("welcome")
	bob.watch(1, tab)
	viewer := s.dial("public", helloV1)
	viewer.next("welcome")
	viewer.watch(1, tab)
	n := 0
	act := func(op string) []map[string]any {
		t.Helper()
		n++
		alice.send(fmt.Sprintf(`{"type":"action","n":"%d","base":"%d","ops":[%s]}`, n, e.seq(), op))
		if ack := alice.next("ack"); !strings.Contains(fmt.Sprint(ack["results"]), "applied") {
			t.Fatalf("%s: %v", op, ack)
		}
		return []map[string]any{bob.next("delta"), viewer.next("delta")}
	}
	_, _, colB := e.ids("B1")
	steps := []struct {
		op    string
		check func(d map[string]any) bool
	}{
		{fmt.Sprintf(`{"op":"width","tab":%d,"cols":[[%d,1]],"width":222}`, tab, colB), func(d map[string]any) bool {
			return fmt.Sprint(d["widths"]) == fmt.Sprintf("[[%d 222]]", colB)
		}},
		{fmt.Sprintf(`{"op":"settings","tab":%d,"frozen_rows":1,"frozen_cols":2}`, tab), func(d map[string]any) bool {
			info := d["tabs"].([]any)[0].(map[string]any)
			return info["frozen_rows"] == 1.0 && info["frozen_cols"] == 2.0
		}},
		{fmt.Sprintf(`{"op":"rename_tab","tab":%d,"name":"Plan"}`, tab), func(d map[string]any) bool {
			return d["tabs"].([]any)[0].(map[string]any)["name"] == "Plan"
		}},
		{`{"op":"add_tab","name":"Second"}`, func(d map[string]any) bool { return len(d["tabs"].([]any)) == 2 }},
		{fmt.Sprintf(`{"op":"move_tab","tab":%d,"before":null}`, tab), func(d map[string]any) bool {
			return d["tabs"].([]any)[1].(map[string]any)["name"] == "Plan"
		}},
	}
	undone := []func(d map[string]any) bool{
		func(d map[string]any) bool { return d["tabs"].([]any)[0].(map[string]any)["name"] == "Plan" },
		func(d map[string]any) bool { return len(d["tabs"].([]any)) == 1 },
		func(d map[string]any) bool { return d["tabs"].([]any)[0].(map[string]any)["name"] == "Sheet1" },
		func(d map[string]any) bool { return d["tabs"].([]any)[0].(map[string]any)["frozen_rows"] == 0.0 },
		func(d map[string]any) bool { return fmt.Sprint(d["widths"]) == fmt.Sprintf("[[%d 104]]", colB) },
	}
	for _, step := range steps {
		for i, d := range act(step.op) {
			if !step.check(d) {
				t.Fatalf("%s reached connection %d as %v", step.op, i, d)
			}
		}
	}
	for i, check := range undone {
		for k, d := range act(`{"op":"undo"}`) {
			if !check(d) {
				t.Fatalf("undo %d reached connection %d as %v", i, k, d)
			}
		}
	}
	for i := range steps {
		for k, d := range act(`{"op":"redo"}`) {
			if !steps[i].check(d) {
				t.Fatalf("redo %d reached connection %d as %v", i, k, d)
			}
		}
	}
	// Structure: an insert, a delete and a sort arrive as struct edits.
	plan := e.tabID("Plan")
	d := act(e.insertRowsOp("Plan", 2, 2))[0]
	edit := d["struct"].([]any)[0].(map[string]any)
	e.in.mu.RLock()
	rows := e.in.wb.byID[plan].rows.ids
	want := fmt.Sprintf("[[%d 2]] %d", rows[1], rows[0])
	e.in.mu.RUnlock()
	if edit["op"] != "insert_rows" || fmt.Sprint(edit["ids"], " ", edit["after"]) != want {
		t.Fatalf("insert = %v, want %s", edit, want)
	}
	d = act(e.deleteRowsOp("Plan", 2, 2))[1]
	if edit := d["struct"].([]any)[0].(map[string]any); edit["op"] != "delete_rows" || d["from"] != nil {
		t.Fatalf("delete on the Public connection = %v", d)
	}
	// Row 1 is frozen, so the sort of rows 1 to 3 leaves it out.
	d = act(e.sortOp("Plan", 1, 3, 1, true))[0]
	edit = d["struct"].([]any)[0].(map[string]any)
	if edit["op"] != "sort" || fmt.Sprint(edit["positions"]) != "[[1 2]]" || len(edit["ids"].([]any)) != 2 {
		t.Fatalf("sort = %v", edit)
	}
	// A change of another tab arrives as touched only.
	e.mustAgent(`{"ops":[{"op":"write","tab":"Second","at":"A1","rows":[[1]]}]}`)
	if d := bob.next("delta"); d["cells"] != nil || fmt.Sprint(d["touched"]) != fmt.Sprintf("[%d]", e.tabID("Second")) {
		t.Fatalf("delta of another tab = %v", d)
	}
	for _, c := range []*client{bob, viewer} {
		heads := 0
		for _, kind := range c.kinds() {
			if kind == "tab_head" || kind == "resync" {
				heads++
			}
		}
		if heads != 1 {
			t.Fatalf("a connection took a new snapshot: %v", c.kinds())
		}
	}
}

// TestLiveRebase is the server side of C-05: the exchange that adjusts the
// text an editor holds, and its refusal.
func TestLiveRebase(t *testing.T) {
	e := start(t)
	e.mustAgent(numbers)
	s := serve(e)
	tab := e.tabID("Sheet1")
	alice := s.dial("private", `{"type":"hello","version":1,"name":"Alice","pending":0}`)
	alice.next("welcome")
	alice.watch(1, tab)
	base := e.seq()
	_, row, col := e.ids("D5")
	bob := e.person("Bob")
	bob.do(e.insertRowsOp("Sheet1", 1, 1))
	first := e.seq()
	rebase := func(text string, at string, from, through uint64) map[string]any {
		alice.send(fmt.Sprintf(`{"type":"rebase","id":3,"tab":%d,"row":%d,"col":%d,"text":%q,"at":%s,"base":"%d","through":"%d"}`, tab, row, col, text, at, from, through))
		return alice.next("rebased")
	}
	// (a), (b): the text read against the old screen turns with the grid.
	if r := rebase("=B5+B6", "[4,3]", base, first); r["text"] != "=B6+B7" || fmt.Sprint(r["at"]) != "[5 3]" || r["base"] != fmt.Sprint(first) {
		t.Fatalf("rebased = %v", r)
	}
	// (d): a second structural change before the first answer is applied
	// in order.
	bob.do(e.deleteRowsOp("Sheet1", 7, 1))
	if r := rebase("=B5+B6", "[4,3]", base, e.seq()); r["text"] != "=B6+#REF!" {
		t.Fatalf("rebased through two changes = %v", r)
	}
	// An unfinished formula is adjusted too.
	if r := rebase("=B5+", "[4,3]", base, first); r["text"] != "=B6+" {
		t.Fatalf("rebased unfinished text = %v", r)
	}
	// (f): Enter before the answer commits with the old base, and the
	// server stores the adjusted formula.
	alice.send(fmt.Sprintf(`{"type":"action","n":"1","base":"%d","ops":[{"op":"set","tab":%d,"row":%d,"col":%d,"text":"=B5+A5","at":[4,3]}]}`, base, tab, row, col))
	alice.next("ack")
	e.wantText("D6", "=B6+A6")
	// (h): a refused rebase, once the log no longer reaches back.
	e.in.mu.Lock()
	e.in.structs.clear(e.in.wb.seq)
	e.in.mu.Unlock()
	if r := rebase("=B5+B6", "[4,3]", base, e.seq()); r["refused"] != "stale_formula" || r["text"] != nil {
		t.Fatalf("rebase after the log was trimmed = %v", r)
	}
}

// TestLiveReconnect covers section 12.5: resume with a watermark replays
// the deltas, pending actions apply exactly once, and a rollback sends
// reset, after which the client reconnects and gets its stored receipts.
func TestLiveReconnect(t *testing.T) {
	e := start(t)
	e.mustAgent(numbers)
	rev := e.b.importAll(t)
	s := serve(e)
	tab := e.tabID("Sheet1")
	alice := s.dial("private", `{"type":"hello","version":1,"name":"Alice","pending":0}`)
	token := alice.next("welcome")["session"].(string)
	alice.watch(1, tab)
	_, row, col := e.ids("C1")
	action := fmt.Sprintf(`{"type":"action","n":"1","base":"%d","ops":[{"op":"set","tab":%d,"row":%d,"col":%d,"text":"once","at":[0,2]}]}`, e.seq(), tab, row, col)
	alice.send(action)
	alice.next("ack")
	mark := e.seq()
	alice.ws.Close(1000, "")
	alice.closed()
	e.person("Bob").set("C2", "meanwhile")

	// The log reaches back: the deltas after the watermark are replayed,
	// and the resent action returns its receipt.
	again := s.dial("private", fmt.Sprintf(`{"type":"hello","version":1,"session":%q,"watermark":"%d","tab":%d,"pending":1}`, token, mark, tab))
	// Its welcome leaves out the tab list and the date, which the host
	// has only as they are now, ahead of the watermark.
	if w := again.next("welcome"); w["seq"] != fmt.Sprint(mark) || w["session"] != token || w["tabs"] != nil || w["date"] != nil {
		t.Fatalf("welcome on resume = %v", w)
	}
	if d := again.next("delta"); d["seq"] != fmt.Sprint(mark+1) || d["cells"] == nil {
		t.Fatalf("replayed delta = %v", d)
	}
	seq := e.seq()
	again.send(action)
	if ack := again.next("ack"); ack["seq"] != fmt.Sprint(mark) || e.seq() != seq {
		t.Fatalf("resent action after reconnect: %v", ack)
	}
	e.want("C1", "once")

	// A rollback reaches every connection as reset.
	if _, err := e.b.Rollback(context.Background(), rev, "Alice"); err != nil {
		t.Fatal(err)
	}
	if r := again.next("reset"); r["seq"] != fmt.Sprint(e.seq()) {
		t.Fatalf("reset = %v", r)
	}
	again.ws.Close(1000, "")
	again.closed()
	// The client reconnects with its session and no watermark, watches
	// again and resends: the receipt is still there.
	third := s.dial("private", fmt.Sprintf(`{"type":"hello","version":1,"session":%q,"pending":1}`, token))
	if w := third.next("welcome"); w["seq"] != fmt.Sprint(e.seq()) {
		t.Fatalf("welcome after the rollback = %v", w)
	}
	_, cells := third.watch(1, tab)
	if _, there := cells[cellKey(e, "C1")]; there {
		t.Fatal("the rolled back cell is in the snapshot")
	}
	third.send(action)
	if ack := third.next("ack"); ack["seq"] != fmt.Sprint(mark) {
		t.Fatalf("receipt after the rollback = %v", ack)
	}
	e.want("C1", "")

	// A watermark the log no longer reaches gets the tab as a snapshot,
	// unasked.
	third.ws.Close(1000, "")
	third.closed()
	e.in.hub.start(e.seq())
	fourth := s.dial("private", fmt.Sprintf(`{"type":"hello","version":1,"session":%q,"watermark":"%d","tab":%d,"pending":0}`, token, mark, tab))
	if w := fourth.next("welcome"); w["seq"] != fmt.Sprint(e.seq()) || w["tabs"] == nil || w["date"] == nil {
		t.Fatalf("welcome with a lost watermark = %v", w)
	}
	if head, _ := fourth.tab(); head["generation"] != 0.0 || head["seq"] != fmt.Sprint(e.seq()) {
		t.Fatalf("unasked snapshot = %v", head)
	}

	// An unknown session with pending edits stops editing.
	lost := s.dial("private", `{"type":"hello","version":1,"session":"0123456789abcdef0123456789abcdef","pending":2}`)
	if f := lost.next("fatal"); f["code"] != "session_expired" {
		t.Fatalf("fatal = %v", f)
	}
}

// TestLivePresenceAndIdentity covers section 15 and rule 26.
func TestLivePresenceAndIdentity(t *testing.T) {
	e := start(t)
	s := serve(e)
	tab := e.tabID("Sheet1")
	ada := s.dial("tailscale", `{"type":"hello","version":1,"name":"Mallory","pending":0}`)
	if me := ada.next("welcome")["me"].(map[string]any); me["label"] != "Ada" || me["verified"] != true {
		t.Fatalf("a Tailscale identity = %v", me)
	}
	anon := s.dial("private", helloV1)
	if me := anon.next("welcome")["me"].(map[string]any); me["label"] != nil || me["verified"] != false {
		t.Fatalf("a session without a name = %v", me)
	}
	viewer := s.dial("public", helloV1)
	viewer.next("welcome")
	_, row, col := e.ids("B2")
	ada.send(fmt.Sprintf(`{"type":"presence","tab":%d,"active":[%d,%d],"editing":true}`, tab, row, col))
	var people []any
	for len(people) == 0 {
		people = anon.next("presence")["people"].([]any)
	}
	p := people[0].(map[string]any)
	if p["label"] != "Ada" || p["verified"] != true || p["editing"] != true || fmt.Sprint(p["active"]) != fmt.Sprintf("[%d %d]", row, col) {
		t.Fatalf("presence = %v", p)
	}
	// A dropped connection loses its presence at once.
	ada.ws.Close(1000, "")
	for len(people) != 0 {
		people = anon.next("presence")["people"].([]any)
	}
	// Presence never reaches a Public connection, and a Public connection
	// that sends it is closed.
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1]]}]}`)
	viewer.next("delta")
	if slices.Contains(viewer.kinds(), "presence") {
		t.Fatal("a Public connection received presence")
	}
	viewer.send(fmt.Sprintf(`{"type":"presence","tab":%d}`, tab))
	if f := viewer.next("fatal"); f["code"] != "forbidden" {
		t.Fatalf("fatal = %v", f)
	}
}

// TestPublicBoundary covers rule 56 and section 17 over raw HTTP and
// WebSocket: forged headers, wrong origins and every refused request.
func TestPublicBoundary(t *testing.T) {
	e := start(t)
	e.mustAgent(numbers)
	e.b.importAll(t)
	s := serve(e)
	tab := e.tabID("Sheet1")
	for _, frame := range []string{
		fmt.Sprintf(`{"type":"action","n":"1","base":"%d","ops":[{"op":"append_rows","tab":%d,"count":1}]}`, e.seq(), tab),
		fmt.Sprintf(`{"type":"rebase","id":1,"tab":%d,"row":1,"col":1,"text":"=A1","at":[0,0],"base":"1","through":"1"}`, tab),
		`{"type":"action_part","n":"1","part":0,"parts":2,"data":"{"}`,
	} {
		viewer := s.dial("public", helloV1)
		viewer.next("welcome")
		seq := e.seq()
		viewer.send(frame)
		if f := viewer.next("fatal"); f["code"] != "forbidden" || e.seq() != seq {
			t.Fatalf("%s on a Public connection: %v", frame, f)
		}
	}
	// A Public hello cannot carry a session or pending edits.
	bad := s.dial("public", `{"type":"hello","version":1,"session":"0123456789abcdef0123456789abcdef","pending":0}`)
	if f := bad.next("fatal"); f["code"] != "forbidden" {
		t.Fatalf("fatal = %v", f)
	}
	get := func(path string, header map[string]string, method string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, s.server.URL+path, strings.NewReader(`{"revision":1}`))
		for name, value := range header {
			req.Header.Set(name, value)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	// History is not reachable on a Public route, whatever the client sends.
	forged := map[string]string{"X-Flats-Access": "private", "X-Flats-Sheet": "1"}
	for _, path := range []string{"/_sheet/api/revisions", "/_sheet/api/revisions/1", "/_sheet/api/revisions/1/tabs/1"} {
		if resp := get("/public"+path, forged, http.MethodGet); resp.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s on a Public route: %d", path, resp.StatusCode)
		}
	}
	for _, path := range []string{"/_sheet/api/rollback", "/_sheet/api/pin", "/_sheet/api/recover"} {
		if resp := get("/public"+path, forged, http.MethodPost); resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s on a Public route: %d", path, resp.StatusCode)
		}
		// A Private POST needs the same origin and the sheet header.
		if resp := get("/private"+path, map[string]string{"X-Flats-Sheet": "1", "Origin": "https://evil.example"}, http.MethodPost); resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s from another origin: %d", path, resp.StatusCode)
		}
		if resp := get("/private"+path, nil, http.MethodPost); resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s without the sheet header: %d", path, resp.StatusCode)
		}
	}
	// The instance trusts only core's exact header: a second value, or
	// another value, is Public.
	for _, values := range [][]string{{"private", "private"}, {"Private"}, {"public"}, {}} {
		r := httptest.NewRequest(http.MethodGet, "/_sheet/api/revisions", nil)
		for _, v := range values {
			r.Header.Add("X-Flats-Access", v)
		}
		w := httptest.NewRecorder()
		e.in.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("X-Flats-Access %v reads history: %d", values, w.Code)
		}
	}
	// An upgrade from another origin is refused.
	r := httptest.NewRequest(http.MethodGet, "/_sheet/ws", nil)
	r.Header.Set("X-Flats-Access", "private")
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	e.in.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("upgrade from another origin: %d", w.Code)
	}
	// Private history works, and the shell carries its policy.
	resp := get("/private/_sheet/api/revisions?limit=50", nil, http.MethodGet)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"number":2`) || !strings.Contains(string(body), `"created":"`) {
		t.Fatalf("history list: %d %s", resp.StatusCode, body)
	}
	shell := get("/public/", nil, http.MethodGet)
	if csp := shell.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "img-src 'none'") || shell.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("shell headers: %v", shell.Header)
	}
}

// TestPublicAdmission is C-01.
func TestPublicAdmission(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["keep","secret"]]}]}`)
	s := serve(e)
	tab := e.tabID("Sheet1")
	holds := func(cells map[string][]any, text string) bool {
		for _, cell := range cells {
			if len(cell) > 2 && cell[2] == text {
				return true
			}
		}
		return false
	}
	// A snapshot cache is built while the sheet is Private.
	editor := s.dial("private", `{"type":"hello","version":1,"name":"Ed","pending":0}`)
	editor.next("welcome")
	if _, cells := editor.watch(1, tab); !holds(cells, "secret") {
		t.Fatal("the editor does not see the cell")
	}
	cached := func() int {
		e.in.hub.mu.Lock()
		defer e.in.hub.mu.Unlock()
		return len(e.in.hub.snaps)
	}
	if cached() != 1 {
		t.Fatalf("%d snapshots cached", cached())
	}
	// The cell is deleted and the sheet becomes Public.
	e.mustAgent(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["B1"]}]}`)
	s.epoch.Store("2")
	admitted := e.seq()
	viewer := s.dial("public", helloV1)
	viewer.next("welcome")
	head, cells := viewer.watch(1, tab)
	if holds(cells, "secret") || head["seq"] != fmt.Sprint(admitted) {
		t.Fatalf("a Public viewer got a frame older than its admission: seq %v", head["seq"])
	}
	// With no edit between the cache and the publication, the cache of the
	// Private epoch is still not reused.
	editor.watch(2, tab)
	s.epoch.Store("3")
	epochs := func() []string {
		e.in.hub.mu.Lock()
		defer e.in.hub.mu.Unlock()
		var out []string
		for key := range e.in.hub.snaps {
			out = append(out, fmt.Sprint(key.public, " ", key.epoch, "/", key.shut))
		}
		slices.Sort(out)
		return out
	}
	before := epochs()
	second := s.dial("public", helloV1)
	second.next("welcome")
	if head, cells := second.watch(1, tab); holds(cells, "secret") || head["seq"] != fmt.Sprint(e.seq()) {
		t.Fatal("the second viewer got older content")
	}
	// The viewer of the new epoch was served from a capture of its own.
	if after := epochs(); slices.Contains(before, "true 3/0") || !slices.Contains(after, "true 3/0") {
		t.Fatalf("cache entries before %v and after %v the second viewer", before, after)
	}
	// Withdrawal closes Public connections at once and drops the cache.
	e.in.WithdrawPublic()
	if !viewer.closed() || !second.closed() || cached() != 0 {
		t.Fatalf("after the withdrawal: %d cached snapshots", cached())
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"C1","rows":[["private note"]]},{"op":"clear","tab":"Sheet1","ranges":["C1"]}]}`)
	s.epoch.Store("4")
	third := s.dial("public", helloV1)
	third.next("welcome")
	if head, cells := third.watch(1, tab); holds(cells, "private note") || holds(cells, "secret") || head["seq"] != fmt.Sprint(e.seq()) {
		t.Fatal("the viewer after the republication got older content")
	}
	if !slices.Contains(editor.kinds(), "delta") {
		editor.next("delta")
	}
}

// TestHistoryRoutes covers section 16.4 and the Recovery criterion: every
// tab of a retained revision can be viewed and restored.
func TestHistoryRoutes(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Sheet1","name":"One"},{"op":"add_tab","name":"Two"},
		{"op":"write","tab":"One","at":"A1","rows":[[1,"=A1+TODAY()*0"]]},{"op":"write","tab":"Two","at":"A1","rows":[["two"]]}]}`)
	rev := e.b.importAll(t)
	e.mustAgent(`{"ops":[{"op":"delete_tab","tab":"Two"},{"op":"write","tab":"One","at":"A1","rows":[[5]]}]}`)
	s := serve(e)
	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		for {
			req, _ := http.NewRequest(method, s.server.URL+"/private"+path, strings.NewReader(body))
			req.Header.Set("X-Flats-Sheet", "1")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var out map[string]any
			_ = json.Unmarshal(data, &out)
			if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/x-ndjson") {
				frames, err := ndjsonFrames(data)
				if err != nil {
					t.Fatal(err)
				}
				out = map[string]any{"frames": frames}
			}
			if resp.StatusCode == http.StatusServiceUnavailable && out["reason"] == "calculating" {
				continue // the revision workbook is still being calculated
			}
			return resp.StatusCode, out
		}
	}
	code, out := call(http.MethodGet, fmt.Sprintf("/_sheet/api/revisions/%d", rev), "")
	tabs, _ := out["tabs"].([]any)
	if code != http.StatusOK || len(tabs) != 2 {
		t.Fatalf("revision tabs: %d %v", code, out)
	}
	for _, info := range tabs {
		id := uint64(info.(map[string]any)["id"].(float64))
		code, out := call(http.MethodGet, fmt.Sprintf("/_sheet/api/revisions/%d/tabs/%d", rev, id), "")
		frames, _ := out["frames"].([]any)
		if code != http.StatusOK || len(frames) < 3 || frames[0].(map[string]any)["type"] != "tab_head" || frames[len(frames)-1].(map[string]any)["type"] != "tab_done" {
			t.Fatalf("revision tab %d: %d %v", id, code, out)
		}
		code, out = call(http.MethodPost, "/_sheet/api/recover", fmt.Sprintf(`{"revision":%d,"tab":%d}`, rev, id))
		if code != http.StatusOK || out["tab"] == nil {
			t.Fatalf("recover tab %d: %d %v", id, code, out)
		}
	}
	if got := e.tabNames(); !slices.Equal(got, []string{"One", "One (restored)", "Two"}) {
		t.Fatalf("tabs after recovering every tab = %v", got)
	}
	e.want("'One (restored)'!A1", "1")
	e.want("Two!A1", "two")
	if code, out := call(http.MethodPost, "/_sheet/api/pin", fmt.Sprintf(`{"revision":%d,"pinned":true}`, rev)); code != http.StatusOK || !e.b.isPinned(rev) {
		t.Fatalf("pin: %d %v", code, out)
	}
	if code, out := call(http.MethodPost, "/_sheet/api/rollback", fmt.Sprintf(`{"revision":%d}`, rev)); code != http.StatusOK || out["revision"] == nil {
		t.Fatalf("rollback: %d %v", code, out)
	}
	if got := e.tabNames(); !slices.Equal(got, []string{"One", "Two"}) {
		t.Fatalf("tabs after the rollback = %v", got)
	}
	if code, out := call(http.MethodPost, "/_sheet/api/recover", `{"revision":99,"tab":1}`); code != http.StatusNotFound || out["code"] != "not_found" {
		t.Fatalf("recover from a missing revision: %d %v", code, out)
	}
}

// TestWebEmbed is C-14: the shell, every module and functions.json are
// served under one versioned prefix. TestFunctionsJSONMatchesRegistry
// checks the function list.
func TestWebEmbed(t *testing.T) {
	e := start(t)
	s := serve(e)
	app, err := sheetApp()
	if err != nil {
		t.Fatal(err)
	}
	fetch := func(path string) (*http.Response, []byte) {
		t.Helper()
		resp, err := http.Get(s.server.URL + "/public" + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, body
	}
	resp, shell := fetch("/")
	prefix := appPrefix + app.version + "/"
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(shell), prefix+"boot.js") || strings.Contains(string(shell), `"`+appPrefix+`boot.js`) {
		t.Fatalf("shell: %d %s", resp.StatusCode, shell)
	}
	names := []string{"functions.json"}
	for name := range app.files {
		if name != "functions.json" {
			names = append(names, name)
		}
	}
	if len(names) < 3 {
		t.Fatalf("embedded files: %v", names)
	}
	for _, name := range names {
		resp, body := fetch(prefix + name)
		if resp.StatusCode != http.StatusOK || len(body) == 0 || !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
			t.Errorf("%s: %d, cache %q", name, resp.StatusCode, resp.Header.Get("Cache-Control"))
		}
	}
	if resp, _ := fetch(appPrefix + "0000/boot.js"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("another version: %d", resp.StatusCode)
	}
}

// TestFunctionsJSONMatchesRegistry reads functions.json as a browser gets
// it and compares it with the formula registry: the same 104 functions in
// the same order, each with its group and its argument names.
func TestFunctionsJSONMatchesRegistry(t *testing.T) {
	e := start(t)
	s := serve(e)
	app, err := sheetApp()
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(s.server.URL + "/public" + appPrefix + app.version + "/functions.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("functions.json: %d %v", resp.StatusCode, err)
	}
	var list []struct {
		Name  string   `json:"name"`
		Group string   `json:"group"`
		Args  []string `json:"args"`
	}
	if err := strictJSON(body, &list); err != nil {
		t.Fatal(err)
	}
	registry := formula.Functions()
	if len(list) != len(registry) || len(list) != 104 {
		t.Fatalf("functions.json lists %d functions, the registry %d", len(list), len(registry))
	}
	for i, f := range registry {
		if list[i].Name != f.Name || list[i].Group != f.Group || !slices.Equal(list[i].Args, f.Args) {
			t.Fatalf("function %d = %+v, registry %+v", i, list[i], f)
		}
	}
}

// TestLiveLargeFrames covers the part frames of section 12: an action
// larger than one frame is assembled, and a large delta arrives in parts.
func TestLiveLargeFrames(t *testing.T) {
	e := start(t)
	s := serve(e)
	tab := e.tabID("Sheet1")
	alice := s.dial("private", `{"type":"hello","version":1,"name":"Alice","pending":0}`)
	alice.next("welcome")
	alice.watch(1, tab)
	bob := s.dial("private", `{"type":"hello","version":1,"name":"Bob","pending":0}`)
	bob.next("welcome")
	bob.watch(1, tab)
	values := make([]string, 60)
	for i := range values {
		values[i] = `["` + strings.Repeat(string(rune('a'+i%26)), 12000) + `"]`
	}
	action := fmt.Sprintf(`{"type":"action","n":"1","base":"%d","ops":[{"op":"write","tab":%d,"rows":%s,"cols":%s,"values":[%s]}]}`,
		e.seq(), tab, e.rowsOf("Sheet1", 1, 60), e.colsOf("Sheet1", 1, 1), strings.Join(values, ","))
	const chunk = 200 << 10
	parts := (len(action) + chunk - 1) / chunk
	for i := 0; i < parts; i++ {
		data, _ := json.Marshal(action[i*chunk : min(len(action), (i+1)*chunk)])
		alice.send(fmt.Sprintf(`{"type":"action_part","n":"1","part":%d,"parts":%d,"data":%s}`, i, parts, data))
	}
	if ack := alice.next("ack"); !strings.Contains(fmt.Sprint(ack["results"]), "applied:60") {
		t.Fatalf("assembled action: %v", ack)
	}
	d := bob.next("delta")
	if len(d["cells"].([]any)) != 60 {
		t.Fatalf("the delta in parts holds %d rows", len(d["cells"].([]any)))
	}
	if got := e.shown("A60"); len(got) != 12000 {
		t.Fatalf("A60 holds %d characters", len(got))
	}
}

// TestAgentNoteTab repeats agent calls and reads the tab their note names:
// the one tab the call wrote cells in, the one tab a call without cells
// changed, and no tab when there are several. The same call names the same
// tab every time, and it is one of the tabs the call's revision lists as
// changed.
func TestAgentNoteTab(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Costs"},{"op":"add_tab","name":"Notes"},{"op":"write","tab":"Costs","at":"A1","rows":[["=Sheet1!A1+Notes!A1"]]}]}`)
	s := serve(e)
	sheet1, costs, notes := float64(e.tabID("Sheet1")), float64(e.tabID("Costs")), float64(e.tabID("Notes"))
	bob := s.dial("private", `{"type":"hello","version":1,"name":"Bob","pending":0}`)
	bob.next("welcome")
	bob.watch(1, e.tabID("Sheet1"))
	for _, c := range []struct {
		name, ops  string
		tab, cells float64
	}{
		{"one tab", `{"op":"write","tab":"Costs","at":"B1","rows":[[1,2]]}`, costs, 2},
		{"several tabs", `{"op":"write","tab":"Sheet1","at":"B1","rows":[[1]]},{"op":"write","tab":"Costs","at":"B2","rows":[[2]]},{"op":"write","tab":"Notes","at":"B3","rows":[[3]]}`, 0, 3},
		{"cells in one tab, rows in another", `{"op":"write","tab":"Notes","at":"C1","rows":[[1]]},{"op":"insert_rows","tab":"Sheet1","at":1,"count":1}`, notes, 1},
		{"a format in one tab", `{"op":"format","tab":"Notes","ranges":["A1:B2"],"format":{"bold":true}}`, notes, 0},
		{"formats in two tabs", `{"op":"format","tab":"Notes","ranges":["A1"],"format":{"italic":true}},{"op":"format","tab":"Costs","ranges":["A1"],"format":{"italic":true}}`, 0, 0},
		{"a width", `{"op":"widths","tab":"Sheet1","cols":"B:B","width":160}`, sheet1, 0},
	} {
		for round := 0; round < 12; round++ {
			e.mustAgent(`{"ops":[` + c.ops + `]}`)
			note := bob.next("delta")["note"].(map[string]any)
			if note["tab"] != c.tab || note["cells"] != c.cells {
				t.Fatalf("%s, call %d: note = %v, want tab %v and %v cells", c.name, round+1, note, c.tab, c.cells)
			}
			rev := e.b.importAll(t)
			changed := e.b.entry(rev).Tabs
			if c.tab != 0 && !slices.Contains(changed, uint64(c.tab)) {
				t.Fatalf("%s, call %d: the note names tab %v and the revision lists %v as changed", c.name, round+1, c.tab, changed)
			}
		}
	}
}

// TestIdentityHeadersByAccessClass sends Tailscale identity headers to the
// instance as each route class would hand them on. The host removes the
// ones a client sends and only its Tailscale route sets them, so the
// instance takes them as the host's word on a Private request. On a Public
// request it reads none, also if one got that far: there is no session, no
// label and no History.
func TestIdentityHeadersByAccessClass(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1]]}]}`)
	rev := e.b.importAll(t)
	// This host hands the instance what the first path segment names, as
	// the routes do after they have removed what the client sent: an
	// access class, with or without an identity.
	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		class, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		r.URL.Path = "/" + rest
		for _, name := range []string{"X-Flats-Access", "Tailscale-User-Login", "Tailscale-User-Name"} {
			r.Header.Del(name)
		}
		access, identity, _ := strings.Cut(class, "+")
		if access != "none" {
			r.Header.Set("X-Flats-Access", access)
		}
		if identity != "" {
			r.Header.Set("Tailscale-User-Login", "ada@example.com")
			r.Header.Set("Tailscale-User-Name", "Ada")
		}
		r.Header.Set("X-Flats-Access-Epoch", "1")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		e.in.ServeHTTP(w, r)
	}))
	t.Cleanup(raw.Close)
	host := strings.TrimPrefix(raw.URL, "http://")
	hello := func(class string) map[string]any {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ws, err := wsconn.Dial(ctx, func(ctx context.Context) (net.Conn, error) { return new(net.Dialer).DialContext(ctx, "tcp", host) }, host, "/"+class+"/_sheet/ws")
		if err != nil {
			t.Fatalf("dial %s: %v", class, err)
		}
		defer ws.Close(1000, "")
		if err := ws.Send(`{"type":"hello","version":1,"pending":0}`); err != nil {
			t.Fatal(err)
		}
		frame, err := ws.ReadMessage(1 << 20)
		if err != nil {
			t.Fatal(err)
		}
		var welcome map[string]any
		if err := json.Unmarshal([]byte(frame), &welcome); err != nil {
			t.Fatal(err)
		}
		return welcome
	}
	// Tailscale: a Private request with the identity the route set.
	if w := hello("private+identity"); fmt.Sprint(w["me"]) != "map[label:Ada verified:true]" {
		t.Errorf("a Private request with an identity: %v", w["me"])
	}
	// Local, NetBird and a reverse proxy: a Private request without one.
	if w := hello("private"); w["access"] != "private" || fmt.Sprint(w["me"]) != "map[verified:false]" {
		t.Errorf("a Private request without an identity: %v %v", w["access"], w["me"])
	}
	for _, class := range []string{"public", "public+identity", "none+identity"} {
		if w := hello(class); w["access"] != "public" || w["me"] != nil || w["session"] != nil || w["session_id"] != nil {
			t.Errorf("a Public request (%s): %v", class, w)
		}
		for _, call := range []struct{ method, path, body string }{
			{http.MethodGet, "/_sheet/api/revisions", ""},
			{http.MethodPost, "/_sheet/api/rollback", fmt.Sprintf(`{"revision":%d}`, rev)},
			{http.MethodPost, "/_sheet/api/recover", fmt.Sprintf(`{"revision":%d,"tab":1}`, rev)},
		} {
			req, _ := http.NewRequest(call.method, raw.URL+"/"+class+call.path, strings.NewReader(call.body))
			req.Header.Set("X-Flats-Sheet", "1")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s %s on a Public request (%s): %d", call.method, call.path, class, resp.StatusCode)
			}
		}
	}
	if n := e.b.importAll(t); n != rev {
		t.Fatalf("the refused requests recorded %d revisions", n-rev)
	}
}

// TestOverwriteNotice covers rule 30 between two people: when the later
// writer had not seen the earlier one's content, the later write stays and
// the earlier writer's session is told which cell and which of its writes
// was replaced. A writer who had seen the content replaces it without a
// notice.
func TestOverwriteNotice(t *testing.T) {
	e := start(t)
	s := serve(e)
	tab := e.tabID("Sheet1")
	alice := s.dial("private", `{"type":"hello","version":1,"name":"Alice","pending":0}`)
	alice.next("welcome")
	alice.watch(1, tab)
	bob := s.dial("private", `{"type":"hello","version":1,"name":"Bob","pending":0}`)
	bob.next("welcome")
	bob.watch(1, tab)
	_, row, col := e.ids("C1")
	set := func(c *client, n int, base uint64, text string) {
		t.Helper()
		c.send(fmt.Sprintf(`{"type":"action","n":"%d","base":"%d","ops":[{"op":"set","tab":%d,"row":%d,"col":%d,"text":%q,"at":[0,2]}]}`, n, base, tab, row, col, text))
		if ack := c.next("ack"); fmt.Sprint(ack["results"]) != "[map[applied:1 status:applied]]" {
			t.Fatalf("ack = %v", ack)
		}
	}
	base := e.seq()
	set(alice, 1, base, "alice")
	set(bob, 1, base, "bob")
	e.want("C1", "bob")
	if o := alice.next("overwritten"); o["tab"] != float64(tab) || o["row"] != float64(row) || o["col"] != float64(col) || o["replaced"] != fmt.Sprint(base+1) {
		t.Fatalf("overwritten = %v", o)
	}
	// Alice has seen Bob's text when she writes over it.
	set(alice, 2, e.seq(), "alice again")
	e.want("C1", "alice again")
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Later"}]}`)
	for last := fmt.Sprint(e.seq()); bob.next("delta")["seq"] != last; {
	}
	for _, f := range bob.seen {
		if f["type"] == "overwritten" {
			t.Fatalf("a writer who had seen the content was reported: %v", f)
		}
	}
}
