package sheet

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

// TestCancelledReopenLeavesItsAnswerToNoOtherEditor commits =B5+1 in A1
// and =D5+1 in C1 in the real client, and another person inserts a row
// above row 5 before either is answered. The person reopens A1, which asks
// the real engine to adjust its pending text, and presses Escape before
// the answer. Then they reopen C1 and type +1, or open nothing. The
// confirmations of both actions come before the late answer or after
// every answer. The late answer opens no editor, C1's editor shows its own
// adjusted text, and the engine stores =D6+1+1 in C1.
func TestCancelledReopenLeavesItsAnswerToNoOtherEditor(t *testing.T) {
	node := nodeOrSkip(t)
	for _, next := range []string{"C1", "nothing"} {
		for _, confirmed := range []string{"before", "after"} {
			t.Run(next+"/"+confirmed, func(t *testing.T) {
				e := start(t)
				e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"B5","rows":[[10]]},{"op":"write","tab":"Sheet1","at":"D5","rows":[[20]]}]}`)
				tab, rowA, colA := e.ids("Sheet1!A1")
				_, rowC, colC := e.ids("Sheet1!C1")
				s := serve(e)
				r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
				r.watch(tab)
				steps, _ := r.taken()
				base := e.seq()
				first := fmt.Sprintf(`{"type":"action","n":"1","base":"%d","ops":[{"op":"set","tab":%d,"row":%d,"col":%d,"text":"=B5+1","at":[0,0]}]}`, base, tab, rowA, colA)
				second := fmt.Sprintf(`{"type":"action","n":"2","base":"%d","ops":[{"op":"set","tab":%d,"row":%d,"col":%d,"text":"=D5+1","at":[0,2]}]}`, base, tab, rowC, colC)
				e.person("Bob").do(e.insertRowsOp("Sheet1", 5, 1))
				got, _ := r.taken()
				changed := got[len(steps):]
				through := e.seq()
				rebaseA := fmt.Sprintf(`{"type":"rebase","id":1,"tab":%d,"row":%d,"col":%d,"text":"=B5+1","at":[0,0],"base":"%d","through":"%d"}`, tab, rowA, colA, base, through)
				r.ask(rebaseA, "rebased", 1)
				got, _ = r.taken()
				rebasedA := got[len(steps)+len(changed):]
				rebaseC := fmt.Sprintf(`{"type":"rebase","id":2,"tab":%d,"row":%d,"col":%d,"text":"=D5+1","at":[0,2],"base":"%d","through":"%d"}`, tab, rowC, colC, base, through)
				r.ask(rebaseC, "rebased", 2)
				got, _ = r.taken()
				rebasedC := got[len(steps)+len(changed)+len(rebasedA):]
				for _, f := range []string{first, second} {
					if ack := r.answer(json.RawMessage(f)); len(ack.Results) != 1 || ack.Results[0].Status != "applied" {
						t.Fatalf("a first edit was answered %+v", ack.Results)
					}
				}
				got, _ = r.taken()
				confirms := got[len(steps)+len(changed)+len(rebasedA)+len(rebasedC):]

				gs := []any{
					map[string]any{"select": []int{0, 0, 0, 0}}, map[string]any{"type": "=B5+1"}, map[string]any{"key": "Enter"},
					map[string]any{"select": []int{0, 2, 0, 2}}, map[string]any{"type": "=D5+1"}, map[string]any{"key": "Enter"},
					map[string]any{"steps": changed},
					map[string]any{"select": []int{0, 0, 0, 0}}, map[string]any{"key": "F2"},
					map[string]any{"edit": true}, map[string]any{"key": "Escape"},
				}
				if next == "C1" {
					gs = append(gs, map[string]any{"select": []int{0, 2, 0, 2}}, map[string]any{"key": "F2"}, map[string]any{"type": "+1"})
				}
				if confirmed == "before" {
					gs = append(gs, map[string]any{"steps": confirms})
				}
				gs = append(gs, map[string]any{"steps": rebasedA}, map[string]any{"edit": true})
				if next == "C1" {
					gs = append(gs, map[string]any{"steps": rebasedC}, map[string]any{"edit": true})
				}
				if confirmed == "after" {
					gs = append(gs, map[string]any{"steps": confirms})
				}
				gs = append(gs, map[string]any{"key": "Enter"})
				run := play(t, node, steps, gs...)

				want := []any{nil, nil}
				if next == "C1" {
					want = append(want, "=D6+1+1")
				}
				if !reflect.DeepEqual(edited(run), want) {
					t.Fatalf("the editor held %v, want %v: the late answer of A1 opens no editor", edited(run), want)
				}
				if !sameFrame(t, sentRequest(t, run, "rebase", 1), rebaseA) {
					t.Fatalf("the client's exchange for A1 differs from the recorded one: %v", run.Requests)
				}
				if len(run.Notices) != 0 {
					t.Fatalf("notices %q", run.Notices)
				}
				if next == "nothing" {
					if len(run.Frames) != 2 {
						t.Fatalf("the client sent %d actions, want the two first edits", len(run.Frames))
					}
					e.wantText("Sheet1!A1", "=B6+1")
					e.want("Sheet1!A1", "11")
					e.wantText("Sheet1!C1", "=D6+1")
					e.want("Sheet1!C1", "21")
					return
				}
				if !sameFrame(t, sentRequest(t, run, "rebase", 2), rebaseC) {
					t.Fatalf("the client's exchange for C1 differs from the recorded one: %v", run.Requests)
				}
				if len(run.Frames) != 3 {
					t.Fatalf("the client sent %d actions, want 3: %s", len(run.Frames), run.Frames)
				}
				if ack := r.answer(run.Frames[2]); len(ack.Results) != 1 || ack.Results[0].Status != "applied" {
					t.Fatalf("the edit of C1 was answered %+v", ack.Results)
				}
				e.wantText("Sheet1!C1", "=D6+1+1")
				e.want("Sheet1!C1", "22")
				e.wantText("Sheet1!A1", "=B6+1")
				e.want("Sheet1!A1", "11")
			})
		}
	}
}

// widthAt is the width the engine keeps for the column of an address.
func (e *env) widthAt(addr string) int {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	t, _, col := e.at(addr)
	if w, ok := t.widths[col]; ok {
		return w.px
	}
	return defaultWidth
}

// recordedSince runs change and returns the frames it brought the
// recording connection.
func recordedSince(r *recorder, change func()) []replayStep {
	before, _ := r.taken()
	change()
	after, _ := r.taken()
	return after[len(before):]
}

// kept is the text the notices of a run offer back.
func kept(run clientRun) []string {
	var out []string
	for _, k := range run.Kept {
		if k != nil {
			out = append(out, *k)
		}
	}
	return out
}

// TestFillDragKeepsItsSourceAndTargets drags the fill handle of A1 to A5
// in the real client while another person changes the rows, and gives
// what the client sends to the real engine. A1:A5 hold 10, 99, 30, 40 and
// 50. A source deleted before the button is let go, deleted and brought
// back by an undo, or deleted by a change that settling the open edit
// applies, sends no fill, so 99 is never repeated. A row inserted above
// moves the cells, and the fill, in mode auto as a drag sends it (spec
// rule 21a), repeats the one plain number 10 over the four cells it was
// aimed at.
func TestFillDragKeepsItsSourceAndTargets(t *testing.T) {
	node := nodeOrSkip(t)
	for _, how := range []string{"inserted above", "deleted", "deleted and undone", "deleted by the settle"} {
		t.Run(how, func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[10],[99],[30],[40],[50]]}]}`)
			tab, source, col := e.ids("Sheet1!A1")
			var targets []RowID
			for _, a := range []string{"A2", "A3", "A4", "A5"} {
				_, row, _ := e.ids("Sheet1!" + a)
				targets = append(targets, row)
			}
			s := serve(e)
			r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
			r.watch(tab)
			steps, _ := r.taken()
			base := e.seq()
			bob := e.person("Bob")
			changed := recordedSince(r, func() {
				if how == "inserted above" {
					bob.do(e.insertRowsOp("Sheet1", 1, 1))
					return
				}
				bob.do(e.deleteRowsOp("Sheet1", 1, 1))
				if how == "deleted and undone" {
					if ack := bob.do(`{"op":"undo"}`); status(ack) != "applied" {
						t.Fatalf("the undo was answered %s", status(ack))
					}
				}
			})
			gs := []any{
				map[string]any{"select": []int{0, 0, 0, 0}},
				map[string]any{"down": map[string]any{"zone": "handle", "r": 0, "c": 0}},
				map[string]any{"move": map[string]any{"zone": "cell", "r": 4, "c": 0}},
			}
			if how == "deleted by the settle" {
				// A key typed during the drag opens an edit of A1, which
				// holds the deletion back until the release commits it.
				gs = append(gs, map[string]any{"type": "=1"})
			}
			gs = append(gs, map[string]any{"steps": changed}, map[string]any{"up": true})
			run := play(t, node, steps, gs...)

			var fills []json.RawMessage
			for _, f := range run.Frames {
				var a struct {
					Base string
					Ops  []struct{ Op string }
				}
				_ = json.Unmarshal(f, &a)
				if a.Ops[0].Op == "fill" {
					fills = append(fills, f)
				}
			}
			if how != "inserted above" {
				if len(fills) != 0 {
					t.Fatalf("the client sent a fill without its source: %s", fills)
				}
				for _, f := range run.Frames {
					r.answer(f)
				}
				want := []string{"99", "30", "40", "50"}
				if how == "deleted and undone" {
					want = []string{"10", "99", "30", "40", "50"}
				}
				if got := e.column("Sheet1", "A", 1, len(want)); !reflect.DeepEqual(got, want) {
					t.Fatalf("column A shows %v, want %v", got, want)
				}
				return
			}
			if len(fills) != 1 {
				t.Fatalf("the client sent %d fills: %s", len(fills), run.Frames)
			}
			want := fmt.Sprintf(`{"type":"action","n":"1","base":"%d","ops":[{"op":"fill","tab":%d,"source":{"rows":[[%d,1]],"cols":[[%d,1]]},"target":{"rows":[[%d,4]],"cols":[[%d,1]]},"mode":"auto"}]}`, base, tab, source, col, targets[0], col)
			if !sameFrame(t, string(fills[0]), want) {
				t.Fatalf("the client sent %s, want %s", fills[0], want)
			}
			if ack := r.answer(fills[0]); len(ack.Results) != 1 || ack.Results[0].Status != "applied" {
				t.Fatalf("the fill was answered %+v", ack.Results)
			}
			if got, want := e.column("Sheet1", "A", 1, 6), []string{"", "10", "10", "10", "10", "10"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("column A shows %v, want %v", got, want)
			}
		})
	}
}

// TestColumnResizeFollowsItsColumn drags the right edge of column B in
// the real client towards 200 pixels while another person inserts a
// column before A, deletes column B and undoes that, or deletes the tab so
// that the client shows the other tab. The engine sets the width of the
// column that was B, and nothing else, or nothing at all.
func TestColumnResizeFollowsItsColumn(t *testing.T) {
	node := nodeOrSkip(t)
	for _, how := range []string{"inserted before", "deleted and undone", "tab replaced"} {
		t.Run(how, func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[{"op":"add_tab","name":"Notes"}]}`)
			tab, _, col := e.ids("Sheet1!B1")
			s := serve(e)
			r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
			r.watch(tab)
			steps, _ := r.taken()
			base := e.seq()
			bob := e.person("Bob")
			changed := recordedSince(r, func() {
				switch how {
				case "inserted before":
					bob.do(e.insertColsOp("Sheet1", 1, 1))
				case "deleted and undone":
					bob.do(e.deleteColsOp("Sheet1", 2, 1))
					if ack := bob.do(`{"op":"undo"}`); status(ack) != "applied" {
						t.Fatalf("the undo was answered %s", status(ack))
					}
				case "tab replaced":
					bob.do(fmt.Sprintf(`{"op":"delete_tab","tab":%d}`, tab))
					// The client asks for the first tab left, as its own
					// second watch.
					r.sync()
					r.generation++
					gen := r.generation
					if err := r.ws.Send(fmt.Sprintf(`{"type":"watch","generation":%d,"tab":%d}`, gen, e.tabID("Notes"))); err != nil {
						t.Fatal(err)
					}
					r.until("the snapshot of Notes", func() bool { return r.done == gen })
				}
			})
			run := play(t, node, steps,
				map[string]any{"down": map[string]any{"zone": "resize", "r": 0, "c": 1, "x": 300}},
				map[string]any{"move": map[string]any{"zone": "col", "r": 0, "c": 1, "x": 340}},
				map[string]any{"steps": changed},
				map[string]any{"move": map[string]any{"zone": "col", "r": 0, "c": 1, "x": 396}},
				map[string]any{"up": true},
			)
			if how != "inserted before" {
				if len(run.Frames) != 0 {
					t.Fatalf("the client sent %s", run.Frames)
				}
				if len(run.Notices) == 0 {
					t.Fatal("the end of the resize was not told")
				}
				tab := "Sheet1"
				if how == "tab replaced" {
					tab = "Notes"
				}
				for _, a := range []string{"A1", "B1", "C1"} {
					if w := e.widthAt(tab + "!" + a); w != defaultWidth {
						t.Fatalf("%s!%s is %d pixels wide", tab, a, w)
					}
				}
				return
			}
			want := fmt.Sprintf(`{"type":"action","n":"1","base":"%d","ops":[{"op":"width","tab":%d,"cols":[[%d,1]],"width":200}]}`, base, tab, col)
			if len(run.Frames) != 1 || !sameFrame(t, string(run.Frames[0]), want) {
				t.Fatalf("the client sent %s, want %s", run.Frames, want)
			}
			if ack := r.answer(run.Frames[0]); status(ack) != "applied" {
				t.Fatalf("the width was answered %s", status(ack))
			}
			for a, w := range map[string]int{"A1": defaultWidth, "B1": defaultWidth, "C1": 200} {
				if got := e.widthAt("Sheet1!" + a); got != w {
					t.Errorf("%s is %d pixels wide, want %d", a, got, w)
				}
			}
		})
	}
}

// TestDialogsLeaveRestoredTargetsUnchanged opens the number format, the
// column width and the sort dialog in the real client, enters settings,
// and then another person deletes the row or the column the dialog acts
// on and undoes that. The undo brings back the same ids with a newer born
// stamp. The client sends what the dialog prepared with the base of the
// screen it was opened on, so the engine ignores it: the content, the
// formats, the widths and the order of the rows stay as they were, and
// the client offers the settings back.
func TestDialogsLeaveRestoredTargetsUnchanged(t *testing.T) {
	node := nodeOrSkip(t)
	cases := []struct {
		name    string
		open    []any
		submit  string
		deleted func(e *env) string
		kept    string
	}{
		{"format", []any{
			map[string]any{"select": []int{1, 1, 1, 1}},
			map[string]any{"click": "Format options: decimals, separator, currency symbol, date and time forms"},
			map[string]any{"field": []string{"nf-kind", "currency"}},
			map[string]any{"field": []string{"nf-sym", "CHF"}},
		}, "Apply", func(e *env) string { return e.deleteRowsOp("Sheet1", 2, 1) }, "Number format: Currency, symbol CHF, 2 decimals"},
		{"width", []any{
			map[string]any{"menu": []int{1, 1}},
			map[string]any{"click": "Column width"},
			map[string]any{"field": []string{"col-width", "237"}},
		}, "Set width", func(e *env) string { return e.deleteColsOp("Sheet1", 2, 1) }, "237"},
		{"sort", []any{
			map[string]any{"select": []int{1, 1, 3, 1}},
			map[string]any{"click": "Sort…"},
			map[string]any{"field": []string{"sort-dir", "desc"}},
		}, "Sort", func(e *env) string { return e.deleteColsOp("Sheet1", 2, 1) }, "Sort by column B, descending"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A2","rows":[["x",10],["y",30],["z",20]]},{"op":"format","tab":"Sheet1","ranges":["B2"],"format":{"bold":true}}]}`)
			tab := e.tabID("Sheet1")
			format := e.formatOf("Sheet1", "B2")
			s := serve(e)
			r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
			r.watch(tab)
			steps, _ := r.taken()
			base := e.seq()
			bob := e.person("Bob")
			changed := recordedSince(r, func() {
				bob.do(c.deleted(e))
				if ack := bob.do(`{"op":"undo"}`); status(ack) != "applied" {
					t.Fatalf("the undo was answered %s", status(ack))
				}
			})
			gs := append(append([]any{}, c.open...), map[string]any{"steps": changed}, map[string]any{"click": c.submit})
			run := play(t, node, steps, gs...)
			if len(run.Frames) != 1 {
				t.Fatalf("the client sent %d actions: %s", len(run.Frames), run.Frames)
			}
			var a struct{ Base string }
			_ = json.Unmarshal(run.Frames[0], &a)
			if a.Base != fmt.Sprint(base) {
				t.Fatalf("the dialog was sent with base %s, want %d, the screen it was opened on", a.Base, base)
			}
			// A sort is ignored as a whole; a format or a width counts the
			// cells it left alone.
			before := e.seq()
			ack := r.answer(run.Frames[0])
			if res := ack.Results; len(res) != 1 || res[0].Applied != 0 || (res[0].Status != "ignored" && res[0].Ignored == 0) {
				t.Fatalf("the engine answered %+v, want nothing applied", ack.Results)
			}
			if e.seq() != before {
				t.Fatalf("the ignored action took sequence %d", e.seq())
			}
			e.want("Sheet1!B2", "10")
			if got := e.formatOf("Sheet1", "B2"); got != format {
				t.Fatalf("B2 has format %s, want %s", got, format)
			}
			if w := e.widthAt("Sheet1!B1"); w != defaultWidth {
				t.Fatalf("B is %d pixels wide", w)
			}
			if got := e.column("Sheet1", "B", 2, 4); !reflect.DeepEqual(got, []string{"10", "30", "20"}) {
				t.Fatalf("column B shows %v", got)
			}
			if got := e.column("Sheet1", "A", 2, 4); !reflect.DeepEqual(got, []string{"x", "y", "z"}) {
				t.Fatalf("column A shows %v", got)
			}

			// The ack reaches the client, which offers the settings back.
			got, _ := r.taken()
			answered := play(t, node, steps, append(gs, map[string]any{"steps": got[len(steps)+len(changed):]})...)
			if k := kept(answered); len(k) == 0 || k[len(k)-1] != c.kept {
				t.Fatalf("the notices offer back %q, want %q", k, c.kept)
			}
		})
	}
}

// TestMenuChildrenKeepTheMenusBase opens the rows and columns menu on B2
// in the real client, and another person deletes column B and undoes that
// before the person chooses Column width, Sort rows or Paste values only.
// The undo brings back the same ids with a newer born stamp. The dialog or
// the clipboard read the item starts sends with the base of the screen the
// menu opened on, so the engine ignores it: the content, the widths and
// the order of the rows stay as they were, and the client offers the
// settings or the clipboard text back.
func TestMenuChildrenKeepTheMenusBase(t *testing.T) {
	node := nodeOrSkip(t)
	cases := []struct {
		name  string
		child []any
		kept  string
	}{
		{"width", []any{
			map[string]any{"click": "Column width"},
			map[string]any{"field": []string{"col-width", "237"}},
			map[string]any{"click": "Set width"},
		}, "237"},
		{"sort", []any{
			map[string]any{"click": "Sort rows"},
			map[string]any{"field": []string{"sort-dir", "desc"}},
			map[string]any{"click": "Sort"},
		}, "Sort by column B, descending"},
		{"paste values", []any{
			map[string]any{"click": "Paste values only"},
			map[string]any{"read": true},
		}, "pasted"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A2","rows":[["x",10],["y",30],["z",20]]}]}`)
			tab := e.tabID("Sheet1")
			s := serve(e)
			r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
			r.watch(tab)
			steps, _ := r.taken()
			base := e.seq()
			bob := e.person("Bob")
			changed := recordedSince(r, func() {
				bob.do(e.deleteColsOp("Sheet1", 2, 1))
				if ack := bob.do(`{"op":"undo"}`); status(ack) != "applied" {
					t.Fatalf("the undo was answered %s", status(ack))
				}
			})
			gs := []any{
				map[string]any{"select": []int{1, 1, 3, 1}},
				map[string]any{"clipboard": "pasted"},
				map[string]any{"menu": []int{1, 1}},
				map[string]any{"steps": changed},
			}
			gs = append(gs, c.child...)
			run := play(t, node, steps, gs...)
			if len(run.Frames) != 1 {
				t.Fatalf("the client sent %d actions: %s", len(run.Frames), run.Frames)
			}
			var a struct{ Base string }
			_ = json.Unmarshal(run.Frames[0], &a)
			if a.Base != fmt.Sprint(base) {
				t.Fatalf("the %s was sent with base %s, want %d, the screen the menu was opened on", c.name, a.Base, base)
			}
			before := e.seq()
			ack := r.answer(run.Frames[0])
			if res := ack.Results; len(res) != 1 || res[0].Applied != 0 || (res[0].Status != "ignored" && res[0].Ignored == 0) {
				t.Fatalf("the engine answered %+v, want nothing applied", ack.Results)
			}
			if e.seq() != before {
				t.Fatalf("the ignored action took sequence %d", e.seq())
			}
			if w := e.widthAt("Sheet1!B1"); w != defaultWidth {
				t.Fatalf("B is %d pixels wide", w)
			}
			if got := e.column("Sheet1", "B", 2, 4); !reflect.DeepEqual(got, []string{"10", "30", "20"}) {
				t.Fatalf("column B shows %v", got)
			}
			if got := e.column("Sheet1", "A", 2, 4); !reflect.DeepEqual(got, []string{"x", "y", "z"}) {
				t.Fatalf("column A shows %v", got)
			}

			got, _ := r.taken()
			answered := play(t, node, steps, append(gs, map[string]any{"steps": got[len(steps)+len(changed):]})...)
			if k := kept(answered); len(k) == 0 || k[len(k)-1] != c.kept {
				t.Fatalf("the notices offer back %q, want %q", k, c.kept)
			}
		})
	}
}

// TestDelayedPasteKeepsItsDestination chooses Paste values only on A1 in
// the real client while the clipboard read of a two by two text is still
// waiting. Another person then deletes, inserts or sorts rows or columns
// before the text arrives. The client writes the text to the rows and
// columns that lay under it when the item was chosen, wherever they are
// now, and the engine ignores the part whose row or column was deleted:
// no neighbour takes its place.
func TestDelayedPasteKeepsItsDestination(t *testing.T) {
	node := nodeOrSkip(t)
	cases := []struct {
		name    string
		change  func(e *env) string
		ignored int
		want    map[string][]string // column letter: rows 1 to 4 after the paste
	}{
		{"nothing", nil, 0, map[string][]string{"A": {"X", "Y", "30", ""}, "B": {"P", "Q", "31", ""}, "C": {"12", "22", "32", ""}}},
		{"row deleted", func(e *env) string { return e.deleteRowsOp("Sheet1", 2, 1) }, 2,
			map[string][]string{"A": {"X", "30", "", ""}, "B": {"P", "31", "", ""}, "C": {"12", "32", "", ""}}},
		{"column deleted", func(e *env) string { return e.deleteColsOp("Sheet1", 2, 1) }, 2,
			map[string][]string{"A": {"X", "Y", "30", ""}, "B": {"12", "22", "32", ""}, "C": {"", "", "", ""}}},
		{"row inserted", func(e *env) string { return e.insertRowsOp("Sheet1", 2, 1) }, 0,
			map[string][]string{"A": {"X", "", "Y", "30"}, "B": {"P", "", "Q", "31"}, "C": {"12", "", "22", "32"}}},
		{"column inserted", func(e *env) string { return e.insertColsOp("Sheet1", 2, 1) }, 0,
			map[string][]string{"A": {"X", "Y", "30", ""}, "B": {"", "", "", ""}, "C": {"P", "Q", "31", ""}, "D": {"12", "22", "32", ""}}},
		{"sorted", func(e *env) string { return e.sortOp("Sheet1", 1, 3, 1, true) }, 0,
			map[string][]string{"A": {"30", "Y", "X", ""}, "B": {"31", "Q", "P", ""}, "C": {"32", "22", "12", ""}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[10,11,12],[20,21,22],[30,31,32]]}]}`)
			tab, row1, colA := e.ids("Sheet1!A1")
			_, row2, colB := e.ids("Sheet1!B2")
			s := serve(e)
			r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
			r.watch(tab)
			steps, _ := r.taken()
			base := e.seq()
			var changed []replayStep
			if c.change != nil {
				bob := e.person("Bob")
				changed = recordedSince(r, func() {
					if ack := bob.do(c.change(e)); status(ack) != "applied" {
						t.Fatalf("the change was answered %s", status(ack))
					}
				})
			}
			gs := []any{
				map[string]any{"select": []int{0, 0, 0, 0}},
				map[string]any{"clipboard": "X\tP\nY\tQ"},
				map[string]any{"menu": []int{0, 0}},
				map[string]any{"click": "Paste values only"},
				map[string]any{"steps": changed},
				map[string]any{"read": true},
			}
			run := play(t, node, steps, gs...)
			if len(run.Frames) != 1 {
				t.Fatalf("the client sent %d actions: %s", len(run.Frames), run.Frames)
			}
			want := fmt.Sprintf(`{"type":"action","n":"1","base":"%d","ops":[{"op":"write","tab":%d,"rows":[%d,%d],"cols":[%d,%d],"values":[["X","P"],["Y","Q"]]}]}`, base, tab, row1, row2, colA, colB)
			if !sameFrame(t, string(run.Frames[0]), want) {
				t.Fatalf("the client sent %s, want %s", run.Frames[0], want)
			}
			ack := r.answer(run.Frames[0])
			if res := ack.Results; len(res) != 1 || res[0].Status != "applied" || res[0].Applied != 4-c.ignored || res[0].Ignored != c.ignored {
				t.Fatalf("the engine answered %+v, want %d cells applied and %d ignored", ack.Results, 4-c.ignored, c.ignored)
			}
			for col, list := range c.want {
				if got := e.column("Sheet1", col, 1, 4); !reflect.DeepEqual(got, list) {
					t.Errorf("column %s shows %v, want %v", col, got, list)
				}
			}
			// The notice of the ack counts the ignored cells and offers
			// the text back.
			got, _ := r.taken()
			answered := play(t, node, steps, append(gs, map[string]any{"steps": got[len(steps)+len(changed):]})...)
			k := kept(answered)
			if c.ignored > 0 && (len(k) == 0 || k[len(k)-1] != "X\tP\nY\tQ") {
				t.Errorf("the notices offer back %q, want the pasted text", k)
			}
			if c.ignored == 0 && len(answered.Notices) != 0 {
				t.Errorf("notices %q", answered.Notices)
			}
		})
	}
}
