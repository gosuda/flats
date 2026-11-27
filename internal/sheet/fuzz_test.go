package sheet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// The fuzz targets of this file feed bytes from outside the engine to its
// decoders through the entry point those bytes take in a running host: a
// WebSocket frame, an update_sheet call, the files of a revision and the
// rows of sheet.db. Each target checks that an input is either refused with
// nothing changed or accepted into a state that the engine can store, load
// and send again.

// fuzzContent is the sheet the targets start from: two tabs with every
// kind of content, a formula in each direction, formats, a width and a
// frozen row.
const fuzzContent = `{"ops":[
	{"op":"add_tab","name":"Data"},
	{"op":"write","tab":"Sheet1","at":"A1","rows":[
		["name","amount","when","ok"],
		["tea",12.5,{"date":"2026-11-13"},true],
		["rice",-3,{"time":"09:30:00"},false],
		["=A2&\"-\"&A3","=SUM(B2:B3)","=Data!A1*2","=IF(D2,\"y\",\"n\")"],
		["'=text",{"datetime":"2026-11-13T09:30:00"},"=B4/0","=\"open"]]},
	{"op":"write","tab":"Data","at":"A1","rows":[[21,"=Sheet1!B4+A1"],["x","=A1:A2"]]},
	{"op":"format","tab":"Sheet1","ranges":["A1:D1"],"format":{"bold":true,"fill":"yellow-200","align":"center"}},
	{"op":"format","tab":"Sheet1","ranges":["B2:B4"],"format":{"number":{"kind":"currency","decimals":2,"symbol":"$"}}},
	{"op":"widths","tab":"Sheet1","cols":"A:A","width":180},
	{"op":"tab_settings","tab":"Sheet1","frozen_rows":1}]}`

// fuzzBook is the sheet of a fuzz target. Every input meets the same
// stored state: after an input that changed the sheet the book opens a
// fresh copy, so a failure does not depend on the inputs before it.
type fuzzBook struct {
	e     *env
	files map[string][]byte // the files of the sheet's directory
	back  *fakeBackend
	root  string
	n     int
	seq   uint64
	base  string // dump of the stored state
}

func newFuzzBook(f *testing.F) *fuzzBook {
	f.Helper()
	e := start(f)
	e.mustAgent(fuzzContent)
	e.b.importAll(f)
	e.in.Stop()
	b := &fuzzBook{e: e, files: map[string][]byte{}, back: e.b.clone(), root: f.TempDir()}
	entries, err := os.ReadDir(e.dir)
	if err != nil {
		f.Fatal(err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(e.dir, entry.Name()))
		if err != nil {
			f.Fatal(err)
		}
		b.files[entry.Name()] = data
	}
	b.reset(f)
	return b
}

// reset opens a fresh copy of the stored state in place of the instance.
func (b *fuzzBook) reset(tb testing.TB) {
	tb.Helper()
	b.e.in.Stop()
	if b.n > 0 {
		_ = os.RemoveAll(b.e.dir)
	}
	b.n++
	dir := filepath.Join(b.root, strconv.Itoa(b.n))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		tb.Fatal(err)
	}
	for name, data := range b.files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			tb.Fatal(err)
		}
	}
	b.e.t, b.e.dir, b.e.b = tb, dir, b.back.clone()
	b.e.open()
	b.seq, b.base = b.e.seq(), b.e.dump()
}

// use hands the book to one input.
func (b *fuzzBook) use(t *testing.T) *env {
	b.e.t = t
	return b.e
}

// unchanged fails when a refused input left a trace in the sheet.
func (b *fuzzBook) unchanged(t *testing.T, what string) {
	t.Helper()
	if got := b.e.dump(); got != b.base {
		t.Fatalf("%s changed the sheet:\n%s", what, dumpDiff(b.base, got))
	}
}

// accepted checks the state an accepted input left: the sequence moved by
// at most one, the state is whole, and a restart finds the same state. It
// then opens a fresh copy for the next input.
func (b *fuzzBook) accepted(t *testing.T, what string) {
	t.Helper()
	e := b.e
	after := e.dump()
	if seq := e.seq(); seq == b.seq {
		if after != b.base {
			t.Fatalf("%s changed the sheet without a sequence:\n%s", what, dumpDiff(b.base, after))
		}
		return
	} else if seq != b.seq+1 {
		t.Fatalf("%s moved the sequence from %d to %d", what, b.seq, seq)
	}
	checkWhole(t, e, what)
	e.restart()
	if got := e.dump(); got != after {
		t.Fatalf("%s left a state that a restart reads differently:\n%s", what, dumpDiff(after, got))
	}
	b.reset(t)
}

// checkWhole checks the stored limits and the invariants every reader of
// the workbook relies on.
func checkWhole(t *testing.T, e *env, what string) {
	t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	if err := wholeWorkbook(e.in.wb); err != nil {
		t.Fatalf("%s left a broken state: %v", what, err)
	}
}

// wholeWorkbook returns what is wrong with a workbook: a stored limit of
// spec rule 9 it passes, or an invariant between its parts that does not
// hold.
func wholeWorkbook(wb *Workbook) error {
	if len(wb.tabs) == 0 || len(wb.tabs) > maxTabs {
		return fmt.Errorf("%d tabs", len(wb.tabs))
	}
	names := map[string]bool{}
	cells, formulas := 0, 0
	for _, t := range wb.tabs {
		folded := formula.FoldName(t.name)
		if !validTabName(t.name) || names[folded] || wb.byID[t.id] != t || wb.byName[folded] != t {
			return fmt.Errorf("tab %d has the name %q, which is invalid, taken or not indexed", t.id, t.name)
		}
		names[folded] = true
		rows, cols := t.rows.len(), t.cols.len()
		if rows < 1 || rows > formula.MaxRows || cols < 1 || cols > formula.MaxCols {
			return fmt.Errorf("tab %q is %d by %d", t.name, rows, cols)
		}
		if t.frozenRows < 0 || t.frozenRows > 20 || t.frozenCols < 0 || t.frozenCols > 10 {
			return fmt.Errorf("tab %q freezes %d rows and %d columns", t.name, t.frozenRows, t.frozenCols)
		}
		for _, a := range []*axis{&t.rows, &t.cols} {
			if len(a.pos) != len(a.ids) {
				return fmt.Errorf("tab %q has %d ids and %d positions", t.name, len(a.ids), len(a.pos))
			}
			for i, id := range a.ids {
				if a.pos[id] != int32(i) || id == 0 || id >= maxID {
					return fmt.Errorf("tab %q has id %d at %d, indexed at %d", t.name, id, i, a.pos[id])
				}
			}
		}
		for col, w := range t.widths {
			if _, live := t.cols.pos[uint64(col)]; !live || w.px < 40 || w.px > 1000 {
				return fmt.Errorf("tab %q has the width %d on column %d", t.name, w.px, col)
			}
		}
		for row, rd := range t.data {
			if _, live := t.rows.pos[uint64(row)]; !live {
				return fmt.Errorf("tab %q holds cells of row %d, which is not on its axis", t.name, row)
			}
			for i, s := range rd.cells {
				if _, live := t.cols.pos[uint64(s.col)]; !live || (i > 0 && rd.cells[i-1].col >= s.col) {
					return fmt.Errorf("tab %q row %d holds column %d out of order or off its axis", t.name, row, s.col)
				}
				c := s.c
				if c.style != 0 && wb.styles.byID[c.style] == nil {
					return fmt.Errorf("tab %q cell %d/%d names style %d, which is gone", t.name, row, s.col, c.style)
				}
				if (c.f != nil) != c.content.IsFormula() {
					return fmt.Errorf("tab %q cell %d/%d has content kind %d and formula %v", t.name, row, s.col, c.content.Kind, c.f != nil)
				}
				if wb.counted(c) {
					cells++
				}
				if c.f != nil {
					formulas++
				}
				if c.content.Kind == formula.ContentText && utf8.RuneCountInString(c.content.Text) > formula.MaxTextLen {
					return fmt.Errorf("tab %q cell %d/%d holds %d characters", t.name, row, s.col, utf8.RuneCountInString(c.content.Text))
				}
			}
		}
	}
	if cells > maxCells || formulas > maxFormulas {
		return fmt.Errorf("%d cells and %d formulas", cells, formulas)
	}
	if wb.count.cells != cells || wb.count.formulas != formulas {
		return fmt.Errorf("the counters say %d cells and %d formulas, the tabs hold %d and %d", wb.count.cells, wb.count.formulas, cells, formulas)
	}
	return nil
}

// frameSeeds are client frames beside the table of frames.json: one of
// every operation of an action, the frames of an exchange, and frames at
// the edges of their fields. "$tab", "$row", "$col" and "$seq" stand for
// ids and the sequence of the fuzz sheet.
var frameSeeds = []string{
	`{"type":"watch","generation":2147483647,"tab":"$tab"}`,
	`{"type":"watch","generation":-1,"tab":9007199254740993}`,
	`{"type":"peek","id":-9223372036854775808,"tab":"$tab","row":3,"col":2}`,
	`{"type":"action","n":"1","base":"$seq","ops":[{"op":"set","tab":"$tab","row":"$row","col":"$col","text":"=SUM(B2:B3)*2","at":[0,0]}]}`,
	`{"type":"action","n":"1","base":"$seq","ops":[{"op":"set","tab":"$tab","row":"$row","col":"$col","text":""}]}`,
	`{"type":"action","n":"1","base":"$seq","ops":[{"op":"set","tab":"$tab","row":"$row","col":"$col","text":"x"},{"op":"insert_rows","tab":"$tab","beside":"$row","side":"after","count":1}]}`,
	`{"type":"action","n":"1","base":"0","ops":[{"op":"set","tab":"$tab","row":5,"col":5,"text":"=Data!A1","at":[4,4]}]}`,
	`{"type":"action","n":"1","base":"$seq","ops":[{"op":"set","tab":"$tab","row":5,"col":5,"text":"=A1","at":[2147483647,-1]}]}`,
	`{"type":"action","n":"1","base":"$seq","ops":[{"op":"write","tab":"$tab","rows":[[1,2],{"new":1}],"cols":[1,{"new":1}],"values":[["a","=A1",""],["b","'c","1e3"],["","",""]]}]}`,
	`{"type":"action","n":"1","base":"$seq","ops":[{"op":"clear","tab":"$tab","rows":"all","cols":[[1,2]]}]}`,
	`{"type":"action","n":"1","base":"$seq","ops":[{"op":"format","tab":"$tab","rows":[[1,3]],"cols":"all","set":{"bold":null,"italic":true,"underline":true,"strikethrough":true,"text_color":"red-600","fill":null,"align":"right","wrap":true,"number":{"kind":"date","pattern":"yyyy-mm-dd"}}}]}`,
	`{"type":"action","n":"1","base":"$seq","ops":[{"op":"width","tab":"$tab","cols":[1,[2,2]],"width":40}]}`,
	`{"type":"action","n":"1","base":"$seq","ops":[{"op":"fill","tab":"$tab","source":{"rows":[[4,1]],"cols":[[1,4]]},"target":{"rows":[[5,3]],"cols":[[1,4]]}}]}`,
	`{"type":"action","n":"1","base":"$seq","ops":[{"op":"copy","from":{"tab":"$tab","rows":[[1,4]],"cols":[[1,2]]},"to":{"tab":2,"rows":[[1001,4]],"cols":[[27,2]]},"mode":"values","cut":false}]}`,
	`{"type":"action","n":"1","base":"$seq","ops":[{"op":"copy","from":{"tab":"$tab","rows":[[4,1]],"cols":[[1,4]]},"to":{"tab":"$tab","rows":[[6,1]],"cols":[[1,4]]},"mode":"all","cut":true}]}`,
	`{"type":"action","n":"1","base":"$seq","ops":[{"op":"insert_rows","tab":"$tab","beside":4,"side":"after","count":3},{"op":"insert_cols","tab":"$tab","beside":2,"side":"before","count":1}]}`,
	`{"type":"action","n":"1","base":"$seq","ops":[{"op":"append_rows","tab":"$tab","count":5},{"op":"append_cols","tab":"$tab","count":2}]}`,
	`{"type":"action","n":"1","base":"$seq","ops":[{"op":"delete_rows","tab":"$tab","ids":[[2,2]]},{"op":"delete_cols","tab":"$tab","ids":[2]}]}`,
	`{"type":"action","n":"1","base":"$seq","ops":[{"op":"delete_rows","tab":"$tab","ids":"all"}]}`,
	`{"type":"action","n":"1","base":"$seq","ops":[{"op":"sort","tab":"$tab","rows":"all","by":2,"desc":true}]}`,
	`{"type":"action","n":"1","base":"$seq","ops":[{"op":"sort","tab":"$tab","rows":[[1,5]],"by":1}]}`,
	`{"type":"action","n":"1","base":"$seq","ops":[{"op":"settings","tab":"$tab","frozen_rows":20,"frozen_cols":10}]}`,
	`{"type":"action","n":"1","base":"$seq","ops":[{"op":"add_tab","name":"New","before":1},{"op":"rename_tab","tab":2,"name":"Renamed"},{"op":"move_tab","tab":1},{"op":"delete_tab","tab":2}]}`,
	`{"type":"action","n":"1","base":"$seq","ops":[{"op":"add_tab"}]}`,
	`{"type":"action","n":"1","base":"$seq","ops":[{"op":"redo"}]}`,
	`{"type":"action","n":"18446744073709551615","base":"18446744073709551615","ops":[{"op":"undo"}]}`,
	`{"type":"action","n":"1","base":"$seq","ops":[]}`,
	`{"type":"action_part","n":"1","part":0,"parts":1,"data":"{\"type\":\"action\",\"n\":\"1\",\"base\":\"$seq\",\"ops\":[{\"op\":\"set\",\"tab\":1,\"row\":9,\"col\":1,\"text\":\"whole\"}]}"}`,
	`{"type":"action_part","n":"2","part":0,"parts":1,"data":"{\"type\":\"action\",\"n\":\"1\",\"base\":\"$seq\",\"ops\":[{\"op\":\"undo\"}]}"}`,
	`{"type":"action_part","n":"1","part":1,"parts":32,"data":"x"}`,
	`{"type":"rebase","id":1,"tab":"$tab","row":4,"col":2,"text":"=SUM(B2:B3)+Data!A1","at":[3,1],"base":"1","through":"$seq"}`,
	`{"type":"rebase","id":1,"tab":77,"row":4,"col":2,"text":"plain","at":[0,0],"base":"$seq","through":"$seq"}`,
	`{"type":"rebase","id":1,"tab":"$tab","row":4,"col":2,"text":"=","at":[0,0],"base":"$seq","through":"18446744073709551615"}`,
	`{"type":"presence","tab":"$tab"}`,
	`{"type":"presence","tab":"$tab","active":[1,1],"range":{"rows":"all","cols":"all"},"editing":false}`,
	`{"type":"presence","tab":"$tab","range":{"rows":[[1,100000]],"cols":[[1,702]]}}`,
	`[]`, `null`, `{}`, `{"type":null}`, `{"type":"action"`, "{\"type\":\"watch\",\"generation\":1,\"tab\":1}\x00",
}

// wireFrame is a client frame as it is read and as it is written again.
type wireFrame interface {
	UnmarshalJSON([]byte) error
	appendJSON([]byte) []byte
}

func newWireFrame(kind string) wireFrame {
	switch kind {
	case "hello":
		return new(helloFrame)
	case "watch":
		return new(watchFrame)
	case "action":
		return new(actionFrame)
	case "action_part":
		return new(actionPartFrame)
	case "peek":
		return new(peekFrame)
	case "rebase":
		return new(rebaseFrame)
	case "presence":
		return new(presenceFrame)
	}
	return nil
}

// frameReadsBack reads the text of a frame as the server does, and reports
// whether it is a frame of Appendix A. The value built from such a frame
// writes back to a text that is a frame too and reads as the same value:
// nothing of the frame is kept that its own schema would not read, and
// two spellings of one frame are one value.
func frameReadsBack(t *testing.T, frame string) bool {
	t.Helper()
	kind, fault := walk([]byte(frame), frameShape, nil)
	f := newWireFrame(kind)
	if fault != nil || f == nil {
		for _, kind := range []string{"hello", "watch", "action", "action_part", "peek", "rebase", "presence"} {
			if newWireFrame(kind).UnmarshalJSON([]byte(frame)) == nil {
				t.Fatalf("the text passes no walk of its shape and is read as a %s\n%q", kind, frame)
			}
		}
		return false
	}
	if err := f.UnmarshalJSON([]byte(frame)); err != nil {
		// Its fields are well formed and hold a value no frame takes.
		return false
	}
	written := f.appendJSON(nil)
	if _, fault := walk(written, frameShape, nil); fault != nil {
		t.Fatalf("the frame is written back as %s, which is no frame: %v\n%q", written, fault, frame)
	}
	again := newWireFrame(kind)
	if err := again.UnmarshalJSON(written); err != nil {
		t.Fatalf("the frame is written back as %s, which is not read: %v\n%q", written, err, frame)
	}
	if !reflect.DeepEqual(f, again) {
		t.Fatalf("the frame reads as %+v and its written form %s as %+v\n%q", f, written, again, frame)
	}
	if twice := again.appendJSON(nil); !bytes.Equal(written, twice) {
		t.Fatalf("the frame is written back as %s, and that as %s\n%q", written, twice, frame)
	}
	return true
}

// FuzzClientFrame sends one frame to a sheet on a new WebSocket connection
// and reads how the server took it: as the first frame, where hello is
// read, or after a hello. A frame is read, or it ends the connection with
// a fatal frame that names a code. A text that is no frame of Appendix A
// ends it as a protocol error, and a text that is one reads back as the
// same value from its written form (frameReadsBack). A frame that is not
// read changes nothing, and neither does an action whose every operation
// is refused or ignored. A frame that is read changes the sheet by at most
// one sequence, into a state that is whole and that a restart reads back
// the same. The handler never panics.
func FuzzClientFrame(f *testing.F) {
	book := newFuzzBook(f)
	// net/http recovers a panic of a handler and only ends its connection,
	// so the test records it.
	panics := make(chan string, 16)
	s := serve(book.e, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if p := recover(); p != nil {
					select {
					case panics <- fmt.Sprintf("%v\n%s", p, debug.Stack()):
					default:
					}
					panic(p)
				}
			}()
			next.ServeHTTP(w, r)
		})
	})
	tab, row, col := book.e.ids("A1")
	// The seeds name ids of the sheet: quoted in the seeds of this file,
	// bare in the rows of the frame table.
	_, row2, col2 := book.e.ids("B2")
	var pairs []string
	for _, id := range []struct {
		name  string
		value uint64
	}{{"$other", uint64(book.e.tabID("Data"))}, {"$row2", uint64(row2)}, {"$col2", uint64(col2)}, {"$tab", uint64(tab)}, {"$row", uint64(row)}, {"$col", uint64(col)}} {
		pairs = append(pairs, `"`+id.name+`"`, fmt.Sprint(id.value), id.name, fmt.Sprint(id.value))
	}
	fill := strings.NewReplacer(append(pairs, "$seq", fmt.Sprint(book.seq))...).Replace
	for _, row := range loadFrameTable(f) {
		if row.Kind != "call" {
			f.Add(row.Kind == "hello", fill(row.Raw))
		}
	}
	for _, seed := range frameSeeds {
		f.Add(false, fill(seed))
	}
	for _, seed := range []string{
		`{"type":"hello","version":1,"pending":0}`,
		`{"type":"hello","version":1,"name":"Ann","pending":0}`,
		`{"type":"hello","version":1,"session":"no-such-token","watermark":"3","tab":1,"pending":2}`,
		`{"type":"hello","version":2,"pending":0}`,
		`{"type":"hello","version":1,"pending":-1}`,
		`{"type":"hello","version":1,"pending":0,"extra":true}`,
		`{"type":"hello","version":1,"name":"\u0000","watermark":"18446744073709551616","tab":9007199254740993,"pending":2147483648}`,
		`{"type":"watch","generation":1,"tab":1}`,
		`{"version":1,"pending":0}`, `[]`, ``,
	} {
		f.Add(true, seed)
	}
	const probe = `{"type":"peek","id":424242,"tab":1,"row":1,"col":1}`
	// exchange sends the frame on a new connection, with the probe behind
	// it when one is given, and returns how the server answered: "read"
	// for the answer to the probe, "fatal <code>", or "closed". applied
	// counts the operations an ack reports as applied, and is -1 without
	// an ack.
	applied := -1
	exchange := func(t *testing.T, first bool, frame, probe string) string {
		applied = -1
		c := s.dial("private", "")
		defer c.ws.Close(1000, "")
		if !first {
			c.send(`{"type":"hello","version":1,"name":"Fuzz","pending":0}`)
			c.next("welcome")
		}
		_ = c.ws.Send(frame)
		if probe != "" {
			_ = c.ws.Send(probe)
		}
		timeout := time.After(30 * time.Second)
		for {
			select {
			case got, open := <-c.frames:
				switch {
				case !open:
					return "closed"
				case got["type"] == "fatal":
					return fmt.Sprint("fatal ", got["code"])
				case got["type"] == "ack":
					applied = 0
					results, _ := got["results"].([]any)
					for _, raw := range results {
						if res, _ := raw.(map[string]any); res["status"] == "applied" {
							applied++
						}
					}
				case got["type"] == "peeked" && got["id"] == float64(424242):
					return "read"
				}
			case <-timeout:
				return "silent"
			}
		}
	}
	f.Fuzz(func(t *testing.T, first bool, frame string) {
		e := book.use(t)
		decoded := frameReadsBack(t, frame)
		// Frames are read in order, so the answer to the probe follows the
		// frame under test unless that frame ended the connection.
		verdict := exchange(t, first, frame, probe)
		if verdict == "closed" && len(frame) <= clientFrameMax && e.seq() == book.seq {
			// The server ended the connection while the probe was on its
			// way. Closing a socket with unread bytes can reset it, and a
			// reset can take the fatal frame with it. Sent alone, the
			// frame gets its answer.
			if verdict = exchange(t, first, frame, ""); verdict == "silent" && !first {
				t.Fatalf("the frame ended one connection and was read on another\n%q", frame)
			}
			if verdict == "silent" {
				verdict = "read" // a hello that is read is answered by a welcome alone
			}
		}
		select {
		case p := <-panics:
			t.Fatalf("the handler panicked: %s\n%q", p, frame)
		default:
		}
		if !decoded && verdict != "fatal protocol" && len(frame) <= clientFrameMax {
			t.Fatalf("a frame that is none of Appendix A was answered with %q\n%q", verdict, frame)
		}
		switch {
		case verdict == "silent":
			t.Fatalf("no answer to the frame within 30 seconds\n%q", frame)
		case verdict == "read" && applied == 0:
			if e.seq() != book.seq {
				t.Fatalf("an action without an applied operation moved the sequence to %d\n%q", e.seq(), frame)
			}
			book.unchanged(t, "an action whose operations were all refused or ignored")
		case verdict == "read":
			book.accepted(t, fmt.Sprintf("the frame %q", frame))
		case verdict == "closed" && len(frame) <= clientFrameMax:
			// Only a message over the frame limit ends a connection
			// without a fatal frame.
			t.Fatalf("the connection ended without a fatal frame\n%q", frame)
		default:
			if verdict == "fatal <nil>" || verdict == "fatal " {
				t.Fatalf("a fatal frame without a code\n%q", frame)
			}
			if e.seq() != book.seq {
				t.Fatalf("a frame answered with %q moved the sequence to %d\n%q", verdict, e.seq(), frame)
			}
			book.unchanged(t, fmt.Sprintf("a frame answered with %q", verdict))
		}
	})
}

// callSeeds are update_sheet calls: every operation, the forms of a cell,
// guards, an op_id and calls at the edges of their fields. "$state" stands
// for the state of the fuzz sheet.
var callSeeds = []string{
	fuzzContent,
	`{"state":"$state","ops":[{"op":"write","tab":"Sheet1","at":"B7","rows":[[1,"two",true,null,{"formula":"=B7*2","value":2},{"date":"2026-01-31"},{"time":"23:59"},{"datetime":"2026-01-31T00:00:00"}]]}]}`,
	`{"state":"$state","op_id":"fuzz-1","message":"seed","guard":{"unchanged":true,"ranges":["Sheet1!A1:D5","Data!A:A"]},"ops":[{"op":"clear","tab":"Sheet1","ranges":["A2:B3","4:4"],"formats":true}]}`,
	`{"state":"$state","ops":[{"op":"format","tab":"Data","ranges":["A:B"],"format":{"italic":true,"number":{"kind":"percent","decimals":1},"text_color":null}}]}`,
	`{"state":"$state","ops":[{"op":"widths","tab":"Data","cols":"B:D","width":1000},{"op":"tab_settings","tab":"Data","frozen_rows":0,"frozen_cols":10}]}`,
	`{"state":"$state","ops":[{"op":"insert_rows","tab":"Sheet1","at":2,"count":2},{"op":"delete_rows","tab":"Sheet1","at":1,"count":1},{"op":"insert_cols","tab":"Sheet1","at":"B","count":1},{"op":"delete_cols","tab":"Sheet1","at":"A","count":1}]}`,
	`{"state":"$state","ops":[{"op":"sort","tab":"Sheet1","rows":"2:5","by":"B","desc":true}]}`,
	`{"state":"$state","ops":[{"op":"rename_tab","tab":"Sheet1","name":"Budget"},{"op":"write","tab":"Budget","at":"A1","rows":[["x"]]},{"op":"add_tab","name":"Notes","at":1},{"op":"move_tab","tab":"Notes","to":3},{"op":"delete_tab","tab":"Data"}]}`,
	`{"state":"$state","ops":[{"op":"import_csv","tab":"Data","at":"C3","csv":"a,\"b \"\"q\"\"\",=A1\r\n1,2,\n\n'x,TRUE,2026-11-13"}]}`,
	`{"ops":[{"op":"add_tab","name":"NoState"}]}`,
	`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1]]}]}`,
	`{"state":"s_000000000000.1","ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1]]}]}`,
	`{"state":"$state","ops":[{"op":"write","tab":"Sheet1","at":"ZZ100000","rows":[[1,2],[3,4]]}]}`,
	`{"state":"$state","ops":[{"op":"insert_rows","tab":"Sheet1","at":100001,"count":100000}]}`,
	`{"state":"$state","ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1e309,-0,1E-400,"=1/0","'","''"]]}]}`,
	`{"state":"$state","ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[{"formula":"=A1"}],[{"formula":"=B1","error":"#REF!"}]]}]}`,
	`{"state":"$state","ops":[]}`, `{}`, `[]`, `{"ops":[{"op":"write"`,
}

// FuzzUpdateSheet decodes one update_sheet call and applies it to a sheet.
// A call is refused with a category, and then nothing of it remains, or it
// is read. A call that is read writes back to a call that reads as the
// same value, field for field. An applied call answers with the new
// state, changes the sheet by at most one sequence into a state that is
// whole and that a restart reads back the same, and is not applied again
// when the agent repeats it with its op_id.
func FuzzUpdateSheet(f *testing.F) {
	book := newFuzzBook(f)
	for _, seed := range callSeeds {
		f.Add(seed)
	}
	known := []string{"invalid", "conflict", "sheet_capacity", "unavailable"}
	f.Fuzz(func(t *testing.T, call string) {
		e := book.use(t)
		raw := []byte(strings.ReplaceAll(call, "$state", e.state()))
		refused := func(stage string, err error) {
			var werr *WriteError
			if !errors.As(err, &werr) || !slices.Contains(known, werr.Code) {
				t.Fatalf("%s failed with %v, which is no refusal an agent can read\n%s", stage, err, raw)
			}
			if e.seq() != book.seq {
				t.Fatalf("a call refused as %s %s moved the sequence\n%s", werr.Code, werr.reason(), raw)
			}
			book.unchanged(t, fmt.Sprintf("a call refused as %s %s", werr.Code, werr.reason()))
		}
		u, err := DecodeUpdate(raw)
		if err != nil {
			refused("decoding", err)
			return
		}
		canonical := u.appendJSON(nil)
		again, err := DecodeUpdate(canonical)
		if err != nil || again.hash != u.hash || again.cells != u.cells {
			t.Fatalf("an accepted call encodes to %s, which decodes as %v with hash %s, not %s", canonical, err, again.hash, u.hash)
		}
		if !reflect.DeepEqual(u, again) || !bytes.Equal(again.appendJSON(nil), canonical) {
			t.Fatalf("an accepted call reads as %+v and its written form %s as %+v\n%s", u, canonical, again, raw)
		}
		res, err := e.in.Update(context.Background(), u, e.b.other)
		if err != nil {
			refused("the call", err)
			return
		}
		if res.State != e.state() {
			t.Fatalf("the call answered with state %s, the sheet is at %s\n%s", res.State, e.state(), raw)
		}
		if u.OpID != "" {
			seq := e.seq()
			replay, err := e.in.Update(context.Background(), again, e.b.other)
			if err != nil || replay.State != res.State || e.seq() != seq {
				t.Fatalf("the repeated call answered %v with state %s at sequence %d, the first one state %s at %d\n%s", err, replay.State, e.seq(), res.State, seq, raw)
			}
		}
		book.accepted(t, fmt.Sprintf("the call %s", raw))
	})
}

// revisionSet is the files of a revision with their bytes.
type revisionSet struct {
	files []File
	data  map[string][]byte // by hash
}

func (rs *revisionSet) read(hash string) (io.ReadCloser, error) {
	data, ok := rs.data[hash]
	if !ok {
		return nil, fmt.Errorf("no blob %s", hash)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// with returns the set with the bytes of one file replaced.
func (rs *revisionSet) with(path string, data []byte) *revisionSet {
	out := &revisionSet{data: map[string][]byte{}}
	for hash, d := range rs.data {
		out.data[hash] = d
	}
	replaced := false
	for _, f := range rs.files {
		if f.Path == path {
			f.Hash, f.Size, replaced = contentHash(data), int64(len(data)), true
			out.data[f.Hash] = data
		}
		out.files = append(out.files, f)
	}
	if !replaced {
		f := File{Path: path, Hash: contentHash(data), Size: int64(len(data))}
		out.files, out.data[f.Hash] = append(out.files, f), data
	}
	return out
}

// encodeRevision writes a workbook as revision files: the manifest, the
// axes of each tab and one row file per tab.
func encodeRevision(t *testing.T, wb *Workbook) *revisionSet {
	t.Helper()
	rs := &revisionSet{data: map[string][]byte{}}
	rs = rs.with(manifestPath, encodeManifest(wb))
	for _, tab := range wb.tabs {
		rs = rs.with(axesPath(tab.id), encodeAxes(tab))
		parts, err := wb.encodeChunk(tab, wb.filledRows(tab))
		if err != nil {
			t.Fatalf("a loaded revision does not encode: %v", err)
		}
		for n, part := range parts {
			rs = rs.with(chunkPrefix(tab.id, 0)+strconv.Itoa(n)+".json", part)
		}
	}
	return rs
}

// calculate brings a workbook built from storage to the state a reader
// gets, as a load and a revision view do, and encodes every tab as the
// snapshot frames a browser receives.
func calculate(t *testing.T, wb *Workbook) {
	t.Helper()
	if wb.date == 0 {
		wb.date = formula.DateOf(2026, 11, 13)
	}
	deadline := time.Now().Add(20 * time.Second)
	budget := formula.NewBudget(func() bool { return time.Now().After(deadline) })
	if err := wb.buildDeps(budget); err != nil {
		return // over a read limit: the engine refuses to calculate this state
	}
	out, err := wb.recalc(calcReq{all: true, budget: budget})
	if err != nil {
		return
	}
	wb.apply(out, nil, 0)
	wb.settle()
	for _, tab := range wb.tabs {
		c, head, err := wb.captureTab(tab)
		if err != nil {
			continue // the capture budget of the host is full
		}
		enc, err := newSnapEncoder(c, head)
		if err != nil {
			c.release()
			continue
		}
		i := 0
		enc.run(func(kind string, frame []byte) bool {
			if len(frame)+frameSlack > serverFrameMax || !json.Valid([]byte(`{"type":"x",`+string(frame))) {
				t.Fatalf("snapshot frame %d (%s) of tab %d has %d bytes or is no JSON", i, kind, tab.id, len(frame))
			}
			i++
			return true
		})
		c.release()
	}
}

// FuzzRevisionFiles loads a revision in which one file, the manifest, the
// axes of a tab or a row file, holds arbitrary bytes. The loader refuses
// the revision or builds a workbook that is whole: it meets the stored
// limits, calculates, encodes as snapshot frames, and encodes as revision
// files that load to the same workbook.
func FuzzRevisionFiles(f *testing.F) {
	book := newFuzzBook(f)
	files, _, release, err := book.e.b.RevisionFiles(context.Background(), len(book.e.b.revs))
	if err != nil {
		f.Fatal(err)
	}
	release()
	base := &revisionSet{files: files, data: book.e.b.blobs}
	var rows string
	for _, file := range files {
		if strings.HasPrefix(file.Path, "tabs/1/rows/") {
			rows = file.Path
		}
	}
	if rows == "" {
		f.Fatal("the fuzz sheet has no row file")
	}
	paths := []string{manifestPath, axesPath(1), rows}
	for kind, path := range paths {
		for _, file := range files {
			if file.Path == path {
				f.Add(uint8(kind), base.data[file.Hash])
			}
		}
	}
	for _, seed := range []string{
		`{"format":1,"tabs":[{"frozen_cols":0,"frozen_rows":0,"id":1,"name":"Sheet1"},{"frozen_cols":0,"frozen_rows":0,"id":1,"name":"Data"}]}`,
		`{"format":1,"tabs":[{"frozen_cols":-1,"frozen_rows":21,"id":1,"name":"Sheet1"},{"frozen_cols":0,"frozen_rows":0,"id":2,"name":"sheet1"}]}`,
		`{"format":2,"tabs":[]}`,
		// A tab that freezes more rows than a tab may.
		`{"format":1,"tabs":[{"frozen_rows":21,"id":1,"name":"0"}]}`,
		`{"format":1,"tabs":[{"frozen_cols":11,"frozen_rows":-1,"id":1,"name":"Sheet1"},{"id":2,"name":"Data"}]}`,
		`{"format":1,"tabs":[{"frozen_cols":0,"frozen_rows":0,"id":1,"name":"Before","\u006eame":"Sheet1"}]}`,
	} {
		f.Add(uint8(0), []byte(seed))
	}
	for _, seed := range []string{
		`{"cols":[[1,26]],"rows":[[1,100000]],"widths":{"1":40,"26":1000}}`,
		`{"cols":[[1,702],[1,1]],"rows":[[5,2],[1,3]],"widths":{"0":104}}`,
		`{"cols":[[9007199254740991,2]],"rows":[[1,0]],"widths":{}}`,
		`{"cols":[],"rows":[],"widths":null}`,
		// Runs whose first id plus count wraps, and runs that end at the
		// last id.
		`{"cols":[[1,26]],"rows":[[1,1000],[18446744073709551615,2]],"widths":{}}`,
		`{"cols":[[1,26],[18446744073709551615,2]],"rows":[[1,1000]],"widths":{}}`,
		`{"cols":[[1,26],[9007199254740991,1]],"rows":[[1,1000],[9007199254740991,1]],"widths":{"9007199254740991":60}}`,
		`{"cols":[[1,26,3]],"rows":[[1,1000]],"widths":{}}`,
	} {
		f.Add(uint8(1), []byte(seed))
	}
	for _, seed := range []string{
		`{"rows":[{"cells":{"1":{"v":1},"2":{"f":"=R[0]C[-1]*2","s":0},"3":{"x":"=SUM(","s":1}},"id":1}],"styles":[{"bold":true},{"number":{"kind":"percent","decimals":0}}]}`,
		`{"rows":[{"cells":{"1":{"d":"2026-02-30"},"2":{"t":"24:00:00"},"3":{"dt":"2026-11-13"},"4":{"v":"text","s":7}},"id":2}],"styles":[]}`,
		`{"rows":[{"cells":{"27":{"v":true}},"id":1},{"cells":{"1":{}},"id":1001},{"cells":{"1":{"v":null,"f":null}},"id":1}],"styles":[{}]}`,
		`{"rows":[{"cells":{"1":{"v":1e400},"01":{"v":"dup"}},"id":3}],"styles":[{"bold":null}]}`,
		`{"rows":[{"cells":{"1":{"t":"+1:+2:+3"},"2":{"dt":"2026-11-13T-0:00"},"3":{"t":"09:05"}},"id":4}],"styles":[]}`,
		`{"rows":[{"cells":{"1":{"v":1,"\u0076":2,"s":0}},"id":1}],"styles":[{"bold":true,"bold":false,"number":{"kind":"percent","decimals":0,"decimals":1}}]}`,
	} {
		f.Add(uint8(2), []byte(seed))
	}
	f.Fuzz(func(t *testing.T, kind uint8, data []byte) {
		set := base.with(paths[int(kind)%len(paths)], data)
		wb, err := loadRevision(set.files, set.read, nil)
		if err != nil {
			return
		}
		if err := wholeWorkbook(wb); err != nil {
			t.Fatalf("a revision was loaded into a broken workbook: %v\n%s", err, data)
		}
		calculate(t, wb)
		first := dumpWorkbook(wb, false)
		encoded := encodeRevision(t, wb)
		again, err := loadRevision(encoded.files, encoded.read, nil)
		if err != nil {
			t.Fatalf("the files of a loaded revision do not load: %v\n%s", err, data)
		}
		calculate(t, again)
		if second := dumpWorkbook(again, false); second != first {
			t.Fatalf("a loaded revision encodes to files that load differently:\n%s\n%s", dumpDiff(first, second), data)
		}
	})
}

// storedColumns are the columns of sheet.db whose bytes the loader parses:
// id runs, born stamps, the chunk list, the tab order, a format with its
// stamps, the stored text of a formula and the calculation date.
var storedColumns = []struct{ name, read, write string }{
	{"row ids", `SELECT row_ids FROM tabs WHERE id=1`, `UPDATE tabs SET row_ids=? WHERE id=1`},
	{"row stamps", `SELECT row_born FROM tabs WHERE id=1`, `UPDATE tabs SET row_born=? WHERE id=1`},
	{"column ids", `SELECT col_ids FROM tabs WHERE id=1`, `UPDATE tabs SET col_ids=? WHERE id=1`},
	{"column stamps", `SELECT col_born FROM tabs WHERE id=1`, `UPDATE tabs SET col_born=? WHERE id=1`},
	{"chunk list", `SELECT chunks FROM tabs WHERE id=1`, `UPDATE tabs SET chunks=? WHERE id=1`},
	{"tab order", `SELECT CAST(tab_order AS BLOB) FROM meta`, `UPDATE meta SET tab_order=CAST(? AS TEXT)`},
	{"format", `SELECT CAST(format AS BLOB) FROM styles ORDER BY id LIMIT 1`, `UPDATE styles SET format=CAST(? AS TEXT) WHERE id=(SELECT min(id) FROM styles)`},
	{"format stamps", `SELECT stamps FROM styles ORDER BY id LIMIT 1`, `UPDATE styles SET stamps=? WHERE id=(SELECT min(id) FROM styles)`},
	{"formula", `SELECT CAST(str AS BLOB) FROM cells WHERE kind=7 ORDER BY tab,row,col LIMIT 1`, `UPDATE cells SET str=CAST(? AS TEXT) WHERE kind=7`},
	{"unreadable formula", `SELECT CAST(str AS BLOB) FROM cells WHERE kind=8 LIMIT 1`, `UPDATE cells SET str=CAST(? AS TEXT) WHERE kind=8`},
	{"calculation date", `SELECT CAST(calc_date AS BLOB) FROM meta`, `UPDATE meta SET calc_date=CAST(? AS TEXT)`},
}

// FuzzStoredState reads sheet.db after one of the columns the loader
// parses was replaced by arbitrary bytes. The loader refuses the database
// or builds a workbook that is whole: it calculates, encodes as snapshot
// frames, and is written back and read again as the same workbook.
func FuzzStoredState(f *testing.F) {
	if formula.ContentFormula != 7 || formula.ContentUnreadable != 8 {
		f.Fatal("the content kinds in the queries of storedColumns are out of date")
	}
	book := newFuzzBook(f)
	book.e.in.Stop()
	db, _, _, err := openDB(filepath.Join(book.e.dir, dbName))
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { db.Close() })
	ctx := context.Background()
	for i, col := range storedColumns {
		var value []byte
		if err := db.QueryRowContext(ctx, col.read).Scan(&value); err != nil {
			f.Fatalf("%s: %v", col.name, err)
		}
		f.Add(uint8(i), value)
		f.Add(uint8(i), []byte{})
		if len(value) > 1 {
			f.Add(uint8(i), value[:len(value)-1])
			f.Add(uint8(i), append(slices.Clone(value), value...))
		}
	}
	for _, seed := range []string{"\x01\xa0\x8d\x06", "\x05\x01\x01\x05", "\xff\xff\xff\xff\xff\xff\xff\xff\xff\x01\x01", "\x01\x03\x02\x01"} {
		f.Add(uint8(0), []byte(seed))
		f.Add(uint8(2), []byte(seed))
		f.Add(uint8(1), []byte(seed+"\x01"))
		f.Add(uint8(4), []byte(seed))
	}
	for _, seed := range []string{`[1,2]`, `[2,1]`, `[1,1]`, `[1]`, `[1,2,3]`, `{}`, `[1.5,2]`} {
		f.Add(uint8(5), []byte(seed))
	}
	for _, seed := range []string{`{}`, `{"bold":null}`, `{"number":{"kind":"general"}}`, `{"fill":"nope"}`, `{"bold":true,"bold":false}`} {
		f.Add(uint8(6), []byte(seed))
	}
	for _, seed := range []string{"=R[0]C[0]", "=R[-9]C[-9]", "=SUM(R1C1:R[1048576]C[1])", "=Gone!R1C1", "1", ""} {
		f.Add(uint8(8), []byte(seed))
		f.Add(uint8(9), []byte(seed))
	}
	f.Fuzz(func(t *testing.T, column uint8, value []byte) {
		col := storedColumns[int(column)%len(storedColumns)]
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, col.write, value); err != nil {
			t.Fatal(err)
		}
		wb, err := loadWorkbook(ctx, tx)
		if err != nil {
			return
		}
		if err := wholeWorkbook(wb); err != nil {
			t.Fatalf("sheet.db with other %s was loaded into a broken workbook: %v\n%q", col.name, err, value)
		}
		calculate(t, wb)
		first := dumpWorkbook(wb, true)
		if err := writeAll(ctx, tx, wb); err != nil {
			t.Fatalf("a loaded workbook is not written back: %v\n%s %q", err, col.name, value)
		}
		again, err := loadWorkbook(ctx, tx)
		if err != nil {
			t.Fatalf("a loaded workbook, written back, does not load: %v\n%s %q", err, col.name, value)
		}
		calculate(t, again)
		if second := dumpWorkbook(again, true); second != first {
			t.Fatalf("a loaded workbook is read differently after it was written back:\n%s\n%s %q", dumpDiff(first, second), col.name, value)
		}
	})
}
