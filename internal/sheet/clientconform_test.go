package sheet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
	"github.com/gosuda/flats/internal/wsconn"
)

// replayStep is one step of web/testdata/replay.mjs: a frame the server
// sent, as text, or the tab the recording connection asked for next.
type replayStep struct {
	Recv string `json:"recv,omitempty"`
	Open uint64 `json:"open,omitempty"`
}

// recorder is a connection to a real instance that keeps every frame as it
// arrived, in order, for the real client to read later.
type recorder struct {
	t          *testing.T
	e          *env
	route      string
	ws         *wsconn.Conn
	mu         sync.Mutex
	cond       *sync.Cond
	steps      []replayStep
	seq        uint64 // the newest sequence a frame brought
	done       int    // generation of the newest tab_done
	closed     bool
	generation int
}

func (s *site) record(route, hello string) *recorder {
	s.e.t.Helper()
	host := strings.TrimPrefix(s.server.URL, "http://")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, err := wsconn.Dial(ctx, func(ctx context.Context) (net.Conn, error) { return new(net.Dialer).DialContext(ctx, "tcp", host) }, host, "/"+route+"/_sheet/ws")
	if err != nil {
		s.e.t.Fatalf("dial %s: %v", route, err)
	}
	r := &recorder{t: s.e.t.(*testing.T), e: s.e, route: route, ws: ws}
	r.cond = sync.NewCond(&r.mu)
	go func() {
		for {
			raw, err := ws.ReadMessage(64 << 20)
			r.mu.Lock()
			if err != nil {
				r.closed = true
				r.cond.Broadcast()
				r.mu.Unlock()
				return
			}
			var f struct {
				Type       string
				Seq        string
				Generation int
				Part       int
				Parts      int
			}
			_ = json.Unmarshal([]byte(raw), &f)
			r.steps = append(r.steps, replayStep{Recv: raw})
			seq, _ := strconv.ParseUint(f.Seq, 10, 64)
			switch {
			case f.Type == "delta_part" && f.Part != f.Parts-1:
			case f.Type == "tab_done":
				r.done = f.Generation
			case seq > r.seq:
				r.seq = seq
			}
			r.cond.Broadcast()
			r.mu.Unlock()
		}
	}()
	s.e.t.Cleanup(func() { ws.Close(1000, "") })
	if err := ws.Send(hello); err != nil {
		s.e.t.Fatal(err)
	}
	r.until("the welcome", func() bool { return len(r.steps) > 0 })
	return r
}

// until waits for the frames that make reached true. The caller's wait is
// for frames the server has already decided to send.
func (r *recorder) until(what string, reached func() bool) {
	r.t.Helper()
	timer := time.AfterFunc(30*time.Second, func() {
		r.mu.Lock()
		r.closed = true
		r.cond.Broadcast()
		r.mu.Unlock()
	})
	defer timer.Stop()
	r.mu.Lock()
	defer r.mu.Unlock()
	for !reached() {
		if r.closed {
			r.t.Fatalf("%s: the connection ended or stalled before %s", r.route, what)
		}
		r.cond.Wait()
	}
}

// sync waits until the connection has every delta of the sheet as it is.
func (r *recorder) sync() {
	r.t.Helper()
	seq := r.e.seq()
	r.until(fmt.Sprintf("sequence %d", seq), func() bool { return r.seq >= seq })
}

// watch asks for a tab, as the client does when a person opens it, and
// waits for the whole snapshot. The first watch is the one the client
// sends by itself after the welcome.
func (r *recorder) watch(tab TabID) {
	r.t.Helper()
	r.sync()
	r.generation++
	gen := r.generation
	if gen > 1 {
		r.mu.Lock()
		r.steps = append(r.steps, replayStep{Open: uint64(tab)})
		r.mu.Unlock()
	}
	if err := r.ws.Send(fmt.Sprintf(`{"type":"watch","generation":%d,"tab":%d}`, gen, tab)); err != nil {
		r.t.Fatal(err)
	}
	r.until("the snapshot", func() bool { return r.done == gen })
}

// taken returns the steps so far and the kinds of frames among them.
func (r *recorder) taken() ([]replayStep, map[string]int) {
	r.sync()
	r.mu.Lock()
	defer r.mu.Unlock()
	kinds := map[string]int{}
	for _, s := range r.steps {
		var f struct {
			Type string
			More bool
		}
		_ = json.Unmarshal([]byte(s.Recv), &f)
		kinds[f.Type]++
	}
	return append([]replayStep(nil), r.steps...), kinds
}

// clientView is what the client must hold of the sheet while it watches a
// tab, read from the engine's own state: no frame encoder takes part.
func (e *env) clientView(tab string) map[string]any {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	wb := e.in.wb
	var tabs []any
	for _, t := range wb.tabs {
		tabs = append(tabs, map[string]any{"id": float64(t.id), "name": t.name, "rows": float64(t.rows.len()), "cols": float64(t.cols.len()),
			"frozen_rows": float64(t.frozenRows), "frozen_cols": float64(t.frozenCols)})
	}
	t := wb.tab(tab)
	if t == nil {
		e.t.Fatalf("no tab %q", tab)
	}
	ids := func(list []uint64) []any {
		out := make([]any, len(list))
		for i, id := range list {
			out[i] = float64(id)
		}
		return out
	}
	widths := map[string]any{}
	for col, w := range t.widths {
		if w.px != defaultWidth {
			widths[fmt.Sprint(col)] = float64(w.px)
		}
	}
	cells := map[string]any{}
	for row, rd := range t.data {
		for _, s := range rd.cells {
			c := s.c
			f := wb.styles.format(c.style)
			kind := kindNames[c.val.Kind()]
			if kind == "" && c.f == nil && f == (formula.Format{}) {
				continue // the client keeps no blank cell without a format
			}
			var value any
			switch c.val.Kind() {
			case formula.KindNumber:
				value = c.val.Number()
			case formula.KindText:
				value = c.val.Text()
			case formula.KindBool:
				value = c.val.Bool()
			case formula.KindError:
				value = c.val.Err().String()
			}
			shown := wb.shown(c)
			edit := shown // the text of a formula is never sent
			if c.f == nil && !c.val.IsBlank() {
				edit = formula.EditText(c.content, formula.Pos{})
			}
			encoded, err := appendFormat(nil, f)
			if err != nil {
				e.t.Fatal(err)
			}
			var format any
			_ = json.Unmarshal(encoded, &format)
			cells[fmt.Sprint(row, "/", s.col)] = map[string]any{"kind": kind, "formula": c.f != nil, "value": value, "shown": shown, "edit": edit, "format": format}
		}
	}
	// What the first rows and columns show on screen.
	var drawn [][]string
	for r := 0; r < min(8, t.rows.len()); r++ {
		var line []string
		for col := 0; col < min(6, t.cols.len()); col++ {
			text := ""
			if c := t.cellAt(formula.Pos{Row: int32(r), Col: int32(col)}); c != nil {
				text = wb.shown(c)
			}
			line = append(line, text)
		}
		drawn = append(drawn, line)
	}
	return map[string]any{
		"seq": fmt.Sprint(wb.seq), "date": wb.date.String(), "tabs": tabs, "drawn": drawn,
		"tab": map[string]any{"id": float64(t.id), "name": t.name, "loading": false, "rows": ids(t.rows.ids), "cols": ids(t.cols.ids),
			"frozen_rows": float64(t.frozenRows), "frozen_cols": float64(t.frozenCols), "widths": widths, "cells": cells},
	}
}

// replay runs the real client over the recorded frames and returns what it
// holds.
func replay(t *testing.T, node string, steps []replayStep) map[string]any {
	t.Helper()
	input, err := json.Marshal(steps)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "web/testdata/replay.mjs")
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node replay.mjs: %v\n%s", err, stderr.String())
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("replay.mjs output: %v\n%.2000s", err, out)
	}
	return got
}

// compareClient lists how what the client holds differs from the engine.
func compareClient(got, want map[string]any) []string {
	var diffs []string
	add := func(format string, args ...any) {
		if len(diffs) < 12 {
			diffs = append(diffs, fmt.Sprintf(format, args...))
		}
	}
	for _, field := range []string{"seq", "date", "tabs"} {
		if !reflect.DeepEqual(got[field], want[field]) {
			add("%s is %v, the engine has %v", field, got[field], want[field])
		}
	}
	gt, _ := got["tab"].(map[string]any)
	wt := want["tab"].(map[string]any)
	if gt == nil {
		return append(diffs, "the client holds no tab")
	}
	for _, field := range []string{"id", "name", "loading", "frozen_rows", "frozen_cols", "widths"} {
		if !reflect.DeepEqual(gt[field], wt[field]) {
			add("%s of the tab is %v, the engine has %v", field, gt[field], wt[field])
		}
	}
	for _, axis := range []string{"rows", "cols"} {
		if !reflect.DeepEqual(gt[axis], wt[axis]) {
			g, _ := gt[axis].([]any)
			w := wt[axis].([]any)
			at := 0
			for at < len(g) && at < len(w) && g[at] == w[at] {
				at++
			}
			add("the %s differ from position %d on: the client has %d ids, the engine %d", axis, at, len(g), len(w))
		}
	}
	gc, _ := gt["cells"].(map[string]any)
	wc := wt["cells"].(map[string]any)
	keys := make([]string, 0, len(wc))
	for key := range wc {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		g, w := gc[key], wc[key].(map[string]any)
		if g == nil {
			add("the client lacks cell %s, the engine has %v", key, w)
			continue
		}
		for field, value := range w {
			have := g.(map[string]any)[field]
			if fw, ok := value.(float64); ok {
				if fg, ok := have.(float64); ok && strconv.FormatFloat(fg, 'g', 15, 64) == strconv.FormatFloat(fw, 'g', 15, 64) {
					continue
				}
			}
			if !reflect.DeepEqual(have, value) {
				add("%s of cell %s is %#v, the engine has %#v", field, key, have, value)
			}
		}
	}
	for key, g := range gc {
		if wc[key] == nil {
			add("the client holds cell %s = %v, which the engine does not have", key, g)
		}
	}
	// A cell outside the drawn part of the grid has no text to compare.
	gd, _ := got["drawn"].([]any)
	wd := want["drawn"].([][]string)
	for r := range wd {
		if r >= len(gd) {
			add("the client drew %d rows, the engine has %d", len(gd), len(wd))
			break
		}
		line, _ := gd[r].([]any)
		for c, text := range wd[r] {
			if c < len(line) && line[c] != nil && line[c] != text {
				add("the screen shows %q at row %d, column %d, the engine has %q", line[c], r+1, c+1, text)
			}
		}
	}
	return diffs
}

// TestClientAppliesEngineFrames is the conformance check for the frames
// the server sends. A Private and a Public connection record every frame
// of a scripted session on a real instance: a snapshot, edits and formats
// by several writers, two actions of one transaction that each add a
// format, structural edits of the watched tab and of another one, tab
// operations, undo and redo, deltas that arrive in parts, a snapshot whose
// head needs tab_more frames and whose widest row needs several rows
// frames, and a return to the first tab. The real client then reads the
// recorded frames under Node, and what it holds and draws must be the
// engine's own state of the tab at three points of the session.
//
// This proves that the client's handling of welcome, tab_head, tab_more,
// rows, tab_done, delta and delta_part gives the engine's cells, formats,
// axes, widths, settings and tab list for this session. It does not cover
// frames the session does not produce (resync, reset, the answers to peek
// and rebase, presence), what the client sends, or how a browser lays the
// grid out; the client's own tests and the browser suite hold those.
func TestClientAppliesEngineFrames(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is not installed, so the client cannot be checked in CI")
		}
		t.Skip("node is not installed")
	}
	e := start(t)
	e.mustAgent(`{"ops":[
		{"op":"add_tab","name":"Other"},{"op":"add_tab","name":"Big"},
		{"op":"write","tab":"Other","at":"A1","rows":[[10],[20],[30]]},
		{"op":"write","tab":"Sheet1","at":"A1","rows":[
			["text \"é\" 표",1e21,1.5e-7,123456789012345680000,true,{"date":"2026-11-13"}],
			["=Other!A1+Other!A2","=0.1+0.2","=1/0","=\"open","'007",{"time":"09:30:00"}],
			["z",3,-0.5,1e-7,100000000000000000000,0.000001],["y",1],["x",2]]},
		{"op":"format","tab":"Sheet1","ranges":["A1:F1"],"format":{"bold":true}},
		{"op":"format","tab":"Sheet1","ranges":["B2"],"format":{"number":{"kind":"percent","decimals":1}}},
		{"op":"format","tab":"Sheet1","ranges":["D9"],"format":{"fill":"teal-200"}},
		{"op":"widths","tab":"Sheet1","cols":"B:B","width":220},
		{"op":"tab_settings","tab":"Sheet1","frozen_rows":1}]}`)
	s := serve(e)
	first, big := e.tabID("Sheet1"), e.tabID("Big")
	conns := []*recorder{
		s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`),
		s.record("public", helloV1),
	}
	for _, r := range conns {
		r.watch(first)
	}
	type point struct {
		name  string
		want  map[string]any
		steps [][]replayStep
		kinds []map[string]int
	}
	var points []point
	mark := func(name, tab string) {
		p := point{name: name, want: e.clientView(tab)}
		for _, r := range conns {
			steps, kinds := r.taken()
			p.steps, p.kinds = append(p.steps, steps), append(p.kinds, kinds)
		}
		points = append(points, p)
	}
	alice, bob := e.person("Alice"), e.person("Bob")
	applied := func(ack ackBody) {
		t.Helper()
		for _, r := range ack.Results {
			if r.Status != "applied" {
				t.Fatalf("an action of the session was answered with %s", status(ack))
			}
		}
	}

	// Content, and values a recalculation changes.
	applied(alice.set("Sheet1!C3", "41"))
	applied(alice.set("Sheet1!D3", "=C3+B3"))
	applied(bob.set("Sheet1!B3", "4"))
	// Two actions of one transaction, each with a format that is new to
	// the sheet, then cells that name those formats and a third one.
	// The operations are built before the writer is held: building one
	// reads the sheet.
	base, here, there := e.seq(), e.formatOp("Sheet1!A3", `{"italic":true,"fill":"red-100"}`), e.formatOp("Sheet1!A4", `{"underline":true,"align":"right"}`)
	if n := e.group(
		func() { applied(alice.act(base, here)) },
		func() { applied(bob.act(base, there)) },
	); n != 1 {
		t.Fatalf("the two formats took %d transactions, want one", n)
	}
	applied(alice.do(e.formatOp("Sheet1!A5", `{"strikethrough":true,"wrap":true}`), e.setOp("Sheet1!A6", "after")))
	applied(bob.do(e.formatOp("Sheet1!B4", `{"italic":true,"fill":"red-100"}`)))
	// Structure of the watched tab.
	applied(alice.do(e.insertRowsOp("Sheet1", 3, 2)))
	applied(bob.do(e.deleteColsOp("Sheet1", 5, 1)))
	e.mustAgent(`{"ops":[{"op":"sort","tab":"Sheet1","rows":"5:7","by":"B"}]}`)
	applied(alice.do(e.insertColsOp("Sheet1", 2, 1)))
	// Structure and the name of a tab the connections do not watch: the
	// formula in A2 reads it.
	applied(bob.do(e.insertRowsOp("Other", 1, 1)))
	e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Other","name":"Data 2"}]}`)
	applied(bob.do(e.deleteRowsOp("Data 2", 3, 1)))
	// The tab list, the settings and a width.
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Temp","at":1}]}`)
	applied(alice.do(fmt.Sprintf(`{"op":"move_tab","tab":%d}`, e.tabID("Temp"))))
	e.mustAgent(`{"ops":[{"op":"delete_tab","tab":"Temp"}]}`)
	applied(alice.do(fmt.Sprintf(`{"op":"settings","tab":%d,"frozen_rows":0,"frozen_cols":1}`, first)))
	applied(bob.do(fmt.Sprintf(`{"op":"width","tab":%d,"cols":%s,"width":300}`, first, e.colsOf("Sheet1", 1, 1))))
	// One transaction that changes the watched tab and another one.
	base, here, there = e.seq(), e.setOp("Sheet1!D4", "both"), e.setOp("'Data 2'!C1", "elsewhere")
	e.group(
		func() { applied(alice.act(base, here)) },
		func() { applied(bob.act(base, there)) },
	)
	// Undo and redo travel as ordinary deltas.
	applied(alice.do(`{"op":"undo"}`))
	applied(alice.do(`{"op":"undo"}`))
	applied(alice.do(`{"op":"redo"}`))
	mark("after deltas alone", "Sheet1")

	// 6,000 formats in two calls: each delta is larger than a frame and
	// arrives in parts, and the format list no longer fits a tab_head.
	// 6,000 formats are about 800 KiB of JSON, more than one frame holds.
	const formats = 6000
	e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"insert_rows","tab":"Big","at":1,"count":%d}]}`, formats-newTabRows+10))
	for from := 0; from < formats; from += 3000 {
		ops := make([]string, 0, 3000)
		for i := from; i < from+3000; i++ {
			symbol := string([]rune{rune('A' + i%26), rune('A' + i/26%26), rune('A' + i/676%26)})
			ops = append(ops, fmt.Sprintf(`{"op":"format","tab":"Big","ranges":["A%d"],"format":{"bold":true,"italic":true,"underline":true,"strikethrough":true,"wrap":true,"number":{"kind":"currency","symbol":"%s","decimals":2}}}`, i+11, symbol))
		}
		e.mustAgent(`{"ops":[` + strings.Join(ops, ",") + `]}`)
	}
	// A row of 20 cells of 32,767 characters is larger than a frame.
	wide := make([]string, 20)
	for i := range wide {
		wide[i] = `"` + strings.Repeat(string(rune('a'+i)), formula.MaxTextLen) + `"`
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"Big","at":"A2","rows":[[` + strings.Join(wide, ",") + `]]},{"op":"write","tab":"Big","at":"A12","rows":[[5],[6.5]]}]}`)
	for _, r := range conns {
		r.watch(big)
	}
	// Edits of the large tab: two new formats in one transaction, a cell
	// of the wide row, and a row inserted above it.
	base, here, there = e.seq(), e.formatOp("Big!B12", `{"fill":"blue-600","text_color":"gray-100"}`), e.formatOp("Big!B13", `{"number":{"kind":"number","decimals":3,"thousands":true}}`)
	e.group(
		func() { applied(alice.act(base, here)) },
		func() { applied(bob.act(base, there)) },
	)
	// A delta larger than a frame arrives in parts.
	for i := range wide {
		wide[i] = `"` + strings.Repeat(string(rune('A'+i)), formula.MaxTextLen) + `"`
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"Big","at":"A3","rows":[[` + strings.Join(wide, ",") + `]]}]}`)
	applied(alice.set("Big!C2", "short"))
	applied(bob.set("Big!B13", "1234.5"))
	applied(alice.do(e.insertRowsOp("Big", 1, 1)))
	mark("on the large tab", "Big")

	// Back to the first tab, whose head now needs tab_more as well.
	for _, r := range conns {
		r.watch(first)
	}
	applied(alice.do(e.formatOp("Sheet1!C5", `{"number":{"kind":"date","pattern":"m/d/yyyy"}}`), e.setOp("Sheet1!C5", "2026-12-24")))
	applied(bob.do(e.deleteRowsOp("Sheet1", 3, 1)))
	mark("back on the first tab", "Sheet1")

	// The session produced the frames this test is about.
	_, kinds := conns[0].taken()
	for _, kind := range []string{"welcome", "tab_head", "tab_more", "rows", "tab_done", "delta", "delta_part"} {
		if kinds[kind] == 0 {
			t.Errorf("the session produced no %s frame", kind)
		}
	}
	if kinds["rows"] < 4 || kinds["tab_done"] != 3 {
		t.Errorf("the session produced %d rows frames and %d snapshots: %v", kinds["rows"], kinds["tab_done"], kinds)
	}

	for _, p := range points {
		for i, r := range conns {
			got := replay(t, node, p.steps[i])
			// The client took every frame as the next one of its stream:
			// it neither connected again nor asked for a tab again. The
			// notices that remain tell of the agent's calls.
			if got["sockets"] != float64(1) || strings.Contains(fmt.Sprint(got["notices"]), "loaded again") {
				t.Errorf("%s, %s: the client opened %v sockets and showed the notices %v", p.name, r.route, got["sockets"], got["notices"])
			}
			var watched []string
			for _, w := range got["watches"].([]any) {
				m := w.(map[string]any)
				watched = append(watched, fmt.Sprint(m["generation"], ":", m["tab"]))
			}
			if want := []string{fmt.Sprint("1:", first), fmt.Sprint("2:", big), fmt.Sprint("3:", first)}[:p.kinds[i]["tab_done"]]; !reflect.DeepEqual(watched, want) {
				t.Errorf("%s, %s: the client watched %v, the recording %v", p.name, r.route, watched, want)
			}
			for _, diff := range compareClient(got, p.want) {
				t.Errorf("%s, %s: %s", p.name, r.route, diff)
			}
		}
	}
}
