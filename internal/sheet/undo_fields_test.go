package sheet

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// rewriteNewest decodes the payload of a person's newest entry, lets change
// alter what it holds and writes it back through the encoder: the file is
// intact, its checksum fits, and its fields say what change made them say.
func (p *person) rewriteNewest(change func(e *undoEntry)) {
	e := p.e
	e.t.Helper()
	e.in.mu.Lock()
	defer e.in.mu.Unlock()
	entry := p.s.undo.undo[len(p.s.undo.undo)-1]
	if entry.file == nil {
		e.t.Fatal("the newest entry was not spilled")
	}
	pool := e.in.undoPool()
	loaded, err := pool.read(e.in.wb, entry, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	change(loaded)
	file, err := pool.write(loaded)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := os.Rename(file.path, entry.file.path); err != nil {
		e.t.Fatal(err)
	}
	entry.file.bytes = file.bytes
}

// TestUndoChecksTheFieldsOfAPayload writes payloads whose counts fit and
// whose fields do not fit each other: a sort with fewer stamps than rows,
// lists that hold an id twice, a deletion that saved what it did not
// delete or saved at the place of another of its rows, lists of saved
// records that hold a cell, a width, a last sort or a formula twice, a structural step
// of no ids, a deleted tab without its tab, one that holds a row twice,
// with rows that are not in it or without the chunks its files need,
// names, widths and frozen panes no tab can have, and places before the
// start. The undo of each is skipped as lost, with
// one line in the host's log, and changes nothing; the person's earlier
// entry still applies.
func TestUndoChecksTheFieldsOfAPayload(t *testing.T) {
	setup := `{"ops":[{"op":"add_tab","name":"T"},{"op":"write","tab":"Sheet1","at":"A1","rows":[[3,"x","=A2"],[1,"y"],[2,"z"]]},{"op":"write","tab":"T","at":"A1","rows":[[1,2],[3,4]]},{"op":"widths","tab":"Sheet1","cols":"B:B","width":60}]}`
	cases := []struct {
		name   string
		op     func(e *env) string
		change func(e *undoEntry)
	}{
		{"a sort with fewer stamps than rows", func(e *env) string { return e.sortOp("Sheet1", 1, 3, 1, false) }, func(u *undoEntry) {
			u.steps[0].stamps = nil
		}},
		{"a sort of a row twice", func(e *env) string { return e.sortOp("Sheet1", 1, 3, 1, false) }, func(u *undoEntry) {
			s := &u.steps[0]
			s.ids[1] = s.ids[0]
		}},
		{"a deletion of a row twice", func(e *env) string { return e.deleteRowsOp("Sheet1", 1, 2) }, func(u *undoEntry) {
			s := &u.steps[0]
			s.ids[1] = s.ids[0]
		}},
		{"a deletion that saved a row it did not delete", func(e *env) string { return e.deleteRowsOp("Sheet1", 1, 1) }, func(u *undoEntry) {
			u.steps[0].saved[0].id++
		}},
		{"a deletion that saved a cell of a column it did not delete", func(e *env) string { return e.deleteColsOp("Sheet1", 1, 1) }, func(u *undoEntry) {
			u.steps[0].cells[0].col++
		}},
		{"a saved row with its cells out of order", func(e *env) string { return e.deleteRowsOp("Sheet1", 1, 1) }, func(u *undoEntry) {
			cells := u.steps[0].saved[0].rd.cells
			cells[0], cells[1] = cells[1], cells[0]
		}},
		{"a deletion before the first row", func(e *env) string { return e.deleteRowsOp("Sheet1", 1, 1) }, func(u *undoEntry) {
			u.steps[0].at = -1
		}},
		{"a deleted tab without its tab", func(e *env) string { return jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("T")) }, func(u *undoEntry) {
			u.steps[0].savedTab = nil
		}},
		{"a deleted tab with a row twice", func(e *env) string { return jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("T")) }, func(u *undoEntry) {
			a := &u.steps[0].savedTab.rows
			a.ids[1] = a.ids[0]
		}},
		{"a deleted tab with an id without a born stamp", func(e *env) string { return jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("T")) }, func(u *undoEntry) {
			a := &u.steps[0].savedTab.cols
			a.born = a.born[:0]
		}},
		{"a deleted tab with cells of a row outside it", func(e *env) string { return jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("T")) }, func(u *undoEntry) {
			tab := u.steps[0].savedTab
			tab.data[RowID(maxID-1)] = tab.data[RowID(tab.rows.ids[0])]
		}},
		{"a deleted tab with frozen rows below none", func(e *env) string { return jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("T")) }, func(u *undoEntry) {
			u.steps[0].savedTab.frozenRows = -1
		}},
		// A deletion of an empty row saves nothing but its id. Without the
		// id the step says nothing to restore, and its undo would put no
		// rows back.
		{"a deletion of no rows", func(e *env) string { return e.deleteRowsOp("Sheet1", 4, 1) }, func(u *undoEntry) {
			s := &u.steps[0]
			s.ids, s.saved, s.cells, s.widths, s.sorted, s.colAt, s.rowAt = nil, nil, nil, nil, nil, nil, nil
		}},
		{"a deletion of no columns", func(e *env) string { return e.deleteColsOp("Sheet1", 4, 1) }, func(u *undoEntry) {
			s := &u.steps[0]
			s.ids, s.cells, s.widths, s.rowAt = nil, nil, nil, nil
		}},
		{"an insert of no rows", func(e *env) string { return e.insertRowsOp("Sheet1", 1, 1) }, func(u *undoEntry) {
			u.steps[0].ids = nil
		}},
		{"a sort of no rows", func(e *env) string { return e.sortOp("Sheet1", 1, 3, 1, false) }, func(u *undoEntry) {
			s := &u.steps[0]
			s.ids, s.stamps = nil, nil
		}},
		{"a saved row at a place past its deletion", func(e *env) string { return e.deleteRowsOp("Sheet1", 1, 1) }, func(u *undoEntry) {
			u.steps[0].saved[0].pos = 1
		}},
		{"a saved row at the place of another row of its deletion", func(e *env) string { return e.deleteRowsOp("Sheet1", 3, 2) }, func(u *undoEntry) {
			u.steps[0].saved[0].pos = 1
		}},
		{"a saved formula at a place before the first column", func(e *env) string { return e.deleteRowsOp("Sheet1", 1, 1) }, func(u *undoEntry) {
			s := &u.steps[0]
			for i := range s.colAt[0] {
				s.colAt[0][i] = -2
			}
		}},
		{"a saved width outside the widths a column takes", func(e *env) string { return e.deleteColsOp("Sheet1", 2, 1) }, func(u *undoEntry) {
			u.steps[0].widths[0].w.px = maxWidth + 1
		}},
		{"a restored cell with a property past the format", func(e *env) string { return e.formatOp("Sheet1!A1", `{"bold":true}`) }, func(u *undoEntry) {
			u.cells[0].props |= 1 << propCount
		}},
		{"a deleted tab without its chunks", func(e *env) string { return jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("T")) }, func(u *undoEntry) {
			u.steps[0].savedTab.chunks = nil
		}},
		{"a deleted tab whose chunks start past its first row", func(e *env) string { return jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("T")) }, func(u *undoEntry) {
			u.steps[0].savedTab.chunks[0] = RowID(u.steps[0].savedTab.rows.ids[0] + 1)
		}},
		{"a deleted tab with an empty name", func(e *env) string { return jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("T")) }, func(u *undoEntry) {
			u.steps[0].savedTab.name = ""
		}},
		{"a deleted tab with frozen rows past the bound", func(e *env) string { return jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("T")) }, func(u *undoEntry) {
			u.steps[0].savedTab.frozenRows = maxFrozenRows + 1
		}},
		{"a rename of a tab back to an empty name", func(e *env) string { return jsonf(`{"op":"rename_tab","tab":%d,"name":"U"}`, e.tabID("T")) }, func(u *undoEntry) {
			u.steps[0].name = ""
		}},
		{"a deletion kept apart whose runs are not its rows", func(e *env) string { return e.deleteRowsOp("Sheet1", 1, 1) }, func(u *undoEntry) {
			u.gaps = append(u.gaps, undoGap{tab: u.steps[0].tab, rows: true, name: "Sheet1", at: 2, n: 2, runs: [][2]uint64{{90, 1}}})
		}},
		{"a deletion that saved a cell twice", func(e *env) string { return e.deleteColsOp("Sheet1", 1, 1) }, func(u *undoEntry) {
			cells := u.steps[0].cells
			cells[1].row = cells[0].row
		}},
		{"a restored cell twice", func(e *env) string { return e.formatOp("Sheet1!A1", `{"bold":true}`) }, func(u *undoEntry) {
			u.cells = append(u.cells, u.cells[0])
		}},
		{"a deletion that saved a width twice", func(e *env) string { return e.deleteColsOp("Sheet1", 2, 1) }, func(u *undoEntry) {
			s := &u.steps[0]
			s.widths = append(s.widths, s.widths[0])
		}},
		{"a deletion that saved the last sort of a row twice", func(e *env) string { return e.deleteRowsOp("Sheet1", 1, 1) }, func(u *undoEntry) {
			s := &u.steps[0]
			row := RowID(s.ids[0])
			s.sorted = []savedSort{{row, sortStamp{1, 0}}, {row, sortStamp{2, 0}}}
		}},
		{"a deletion that saved the formula of a cell twice", func(e *env) string { return e.deleteRowsOp("Sheet1", 2, 1) }, func(u *undoEntry) {
			s := &u.steps[0]
			s.repairs = append(s.repairs, s.repairs[0])
		}},
		{"a tab order that names a tab twice", func(e *env) string {
			return jsonf(`{"op":"move_tab","tab":%d,"before":%d}`, e.tabID("T"), e.tabID("Sheet1"))
		}, func(u *undoEntry) {
			s := &u.steps[0]
			s.order = append(s.order, s.order[0])
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sink := &logSink{}
			e := start(t, func(o *Options) { o.Log = slog.New(slog.NewTextHandler(sink, nil)) })
			e.mustAgent(setup)
			e.in.mu.Lock()
			e.in.undoPool().entryMax = -1
			e.in.mu.Unlock()
			alice := e.person("Alice")
			// The earlier entry writes into the tab the action leaves alone.
			first := "T!D1"
			if strings.Contains(c.name, "tab") {
				first = "Sheet1!Z9"
			}
			alice.set(first, "first")
			if ack := alice.do(c.op(e)); status(ack) != "applied" {
				t.Fatalf("the action: %s", status(ack))
			}
			alice.rewriteNewest(c.change)
			seq, before := e.seq(), e.dump()
			wantUndo(t, "undo of the payload", undo(t, alice), undoAck{Skipped: 1, Reason: "lost"})
			if e.seq() != seq || e.dump() != before {
				t.Fatal("an undo that was lost changed the sheet")
			}
			if lines := sink.lines("sheet undo payload lost"); len(lines) != 1 || !strings.Contains(lines[0], "is damaged") {
				t.Fatalf("the log lines about the payload are %q", lines)
			}
			wantUndo(t, "undo of the earlier entry", undo(t, alice), undoAck{Applied: 1})
			e.want(first, "")
		})
	}
}

// A saved row's place among the rows its deletion took is where following
// reads its formulas from, and its id where the undo puts it back. A file
// whose place names another deleted row is damaged: with it, following
// would shift A1's reference to row 10 for an insert below that row, and
// the undo would bring the formula back to its own row as =A11.
func TestUndoChecksTheRowAtASavedPlace(t *testing.T) {
	sink := &logSink{}
	e := start(t, func(o *Options) { o.Log = slog.New(slog.NewTextHandler(sink, nil)) })
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"T"},{"op":"write","tab":"Sheet1","at":"A1","rows":[["=A10"]]}]}`)
	e.in.mu.Lock()
	e.in.undoPool().entryMax = -1
	e.in.mu.Unlock()
	alice, bob := e.person("Alice"), e.person("Bob")
	alice.set("T!A1", "first")
	if ack := alice.do(e.deleteRowsOp("Sheet1", 1, 2)); status(ack) != "applied" {
		t.Fatalf("the deletion: %s", status(ack))
	}
	alice.rewriteNewest(func(u *undoEntry) {
		s := &u.steps[0]
		if len(s.saved) != 1 || s.saved[0].pos != 0 {
			t.Fatalf("the deletion saved %d rows", len(s.saved))
		}
		s.saved[0].pos = 1
	})
	// Row 9 now is row 11 of the sheet before the deletion.
	if ack := bob.do(e.insertRowsOp("Sheet1", 9, 1)); status(ack) != "applied" {
		t.Fatalf("Bob's insert: %s", status(ack))
	}
	seq, before := e.seq(), e.dump()
	wantUndo(t, "undo of the deletion", undo(t, alice), undoAck{Skipped: 1, Reason: "lost"})
	if e.seq() != seq || e.dump() != before {
		t.Fatal("an undo that was lost changed the sheet")
	}
	if lines := sink.lines("sheet undo payload lost"); len(lines) != 1 || !strings.Contains(lines[0], "is damaged") || !strings.Contains(lines[0], "did not delete there") {
		t.Fatalf("the log lines about the payload are %q", lines)
	}
	wantUndo(t, "undo of the earlier entry", undo(t, alice), undoAck{Applied: 1})
	e.want("T!A1", "")
}

// rewriteNewestStyles writes a person's newest entry back with one more
// style state under the id of the first it holds, after it: the file is
// intact and names the id twice, with two formats.
func (p *person) rewriteNewestStyles(other formula.Format) {
	e := p.e
	e.t.Helper()
	e.in.mu.Lock()
	defer e.in.mu.Unlock()
	entry := p.s.undo.undo[len(p.s.undo.undo)-1]
	pool := e.in.undoPool()
	loaded, err := pool.read(e.in.wb, entry, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	if len(loaded.styles) != 1 {
		e.t.Fatalf("the entry holds %d style states", len(loaded.styles))
	}
	styles := loaded.styles
	loaded.styles = nil
	file, err := pool.write(loaded)
	if err != nil {
		e.t.Fatal(err)
	}
	plain, err := os.ReadFile(file.path)
	if err != nil {
		e.t.Fatal(err)
	}
	os.Remove(file.path)
	// Without states the file is the magic, a count of zero, the rest and
	// the checksum.
	rest := plain[len(spillMagic)+1 : len(plain)-4]
	data := checksummed(spillMagic, func(enc *spillEnc) {
		enc.uv(2)
		for id, state := range styles {
			for _, f := range []formula.Format{state.f, other} {
				enc.uv(uint64(id))
				enc.format(f)
				enc.stamps(&state.stamps)
			}
		}
		enc.w.Write(rest)
	})
	if err := os.WriteFile(entry.file.path, data, 0o600); err != nil {
		e.t.Fatal(err)
	}
	entry.file.bytes = int64(len(data))
}

// A payload that names a style state twice is damaged: the saved cells of
// the id would get one of two formats. Alice's deletion saved a bold cell.
func TestUndoChecksEachStyleOfAPayloadOnce(t *testing.T) {
	sink := &logSink{}
	e := start(t, func(o *Options) { o.Log = slog.New(slog.NewTextHandler(sink, nil)) })
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"T"},{"op":"write","tab":"Sheet1","at":"A1","rows":[["x"],["y"]]},{"op":"format","tab":"Sheet1","ranges":["A1"],"format":{"bold":true}}]}`)
	e.in.mu.Lock()
	e.in.undoPool().entryMax = -1
	e.in.mu.Unlock()
	alice := e.person("Alice")
	alice.set("T!A1", "first")
	if ack := alice.do(e.deleteRowsOp("Sheet1", 1, 1)); status(ack) != "applied" {
		t.Fatalf("the deletion: %s", status(ack))
	}
	alice.rewriteNewestStyles(formula.Format{Italic: true})
	seq, before := e.seq(), e.dump()
	wantUndo(t, "undo of the deletion", undo(t, alice), undoAck{Skipped: 1, Reason: "lost"})
	if e.seq() != seq || e.dump() != before {
		t.Fatal("an undo that was lost changed the sheet")
	}
	if lines := sink.lines("sheet undo payload lost"); len(lines) != 1 || !strings.Contains(lines[0], "is damaged") || !strings.Contains(lines[0], "twice") {
		t.Fatalf("the log lines about the payload are %q", lines)
	}
	wantUndo(t, "undo of the earlier entry", undo(t, alice), undoAck{Applied: 1})
	e.want("T!A1", "")
}

// Every style a saved cell names is in its payload: a file without the
// state of a saved cell's style is damaged, or the undo would bring the
// cell back without its format. The bold cells lie in a deleted row, a
// deleted column and a deleted tab.
func TestUndoChecksTheStylesOfSavedCells(t *testing.T) {
	setup := `{"ops":[{"op":"add_tab","name":"T"},{"op":"write","tab":"Sheet1","at":"A1","rows":[["x","y"],["z"]]},{"op":"write","tab":"T","at":"A1","rows":[["w"]]},
		{"op":"format","tab":"Sheet1","ranges":["A1:B1"],"format":{"bold":true}},{"op":"format","tab":"T","ranges":["A1"],"format":{"bold":true}}]}`
	for _, c := range []struct {
		name string
		op   func(e *env) string
	}{
		{"a saved row", func(e *env) string { return e.deleteRowsOp("Sheet1", 1, 1) }},
		{"a saved column", func(e *env) string { return e.deleteColsOp("Sheet1", 2, 1) }},
		{"a deleted tab", func(e *env) string { return jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("T")) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			sink := &logSink{}
			e := start(t, func(o *Options) { o.Log = slog.New(slog.NewTextHandler(sink, nil)) })
			e.mustAgent(setup)
			e.in.mu.Lock()
			e.in.undoPool().entryMax = -1
			e.in.mu.Unlock()
			alice := e.person("Alice")
			// The earlier entry writes into the tab the action leaves alone.
			first := "T!D1"
			if strings.Contains(c.name, "tab") {
				first = "Sheet1!Z9"
			}
			alice.set(first, "first")
			if ack := alice.do(c.op(e)); status(ack) != "applied" {
				t.Fatalf("the action: %s", status(ack))
			}
			alice.rewriteNewest(func(u *undoEntry) {
				if len(u.styles) != 1 {
					t.Fatalf("the entry holds %d style states", len(u.styles))
				}
				u.styles = nil
			})
			seq, before := e.seq(), e.dump()
			wantUndo(t, "undo of the payload", undo(t, alice), undoAck{Skipped: 1, Reason: "lost"})
			if e.seq() != seq || e.dump() != before {
				t.Fatal("an undo that was lost changed the sheet")
			}
			if lines := sink.lines("sheet undo payload lost"); len(lines) != 1 || !strings.Contains(lines[0], "is damaged") || !strings.Contains(lines[0], "style") {
				t.Fatalf("the log lines about the payload are %q", lines)
			}
			wantUndo(t, "undo of the earlier entry", undo(t, alice), undoAck{Applied: 1})
			e.want(first, "")
		})
	}
}
