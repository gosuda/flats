package sheet

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// cutRegion is every cell a repeated-destination case can touch, on the
// given tabs.
func (e *env) cutRegion(tabs ...string) map[string]cutCell {
	e.t.Helper()
	out := map[string]cutCell{}
	for _, tab := range tabs {
		for _, col := range "ABCD" {
			for row := 1; row <= 5; row++ {
				addr := fmt.Sprintf("%s!%c%d", tab, col, row)
				out[addr] = e.cutCell(addr)
			}
		}
	}
	return out
}

// idsJSON writes ids one by one, as a client may list them.
func idsJSON(ids ...any) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprint(id)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// TestCutRefusesARepeatedDestination cuts into a destination list that
// names one row or one column twice, in the same tab and in another, next
// to a deleted destination, as a deleted row named twice and as one added
// row named twice. The second write to the same cell would replace the
// first while the cut clears both sources, so the cut is refused as
// invalid and changes nothing. A plain paste into the same list applies,
// the last write winning, and clears no source.
func TestCutRefusesARepeatedDestination(t *testing.T) {
	type ids struct {
		tab, other   TabID
		r, c, o, oc  [6]uint64 // rows and columns of Sheet1 and Other, from 1
		deletedRow   uint64    // row 5 of Sheet1, deleted before the cut
		deletedOther uint64    // row 5 of Other, deleted before the cut
	}
	read := func(e *env) ids {
		var v ids
		v.tab, v.other = e.tabID("Sheet1"), e.tabID("Other")
		for i := 1; i <= 5; i++ {
			_, row, col := e.ids(fmt.Sprintf("%c%d", 'A'+i-1, i))
			_, orow, ocol := e.ids(fmt.Sprintf("Other!%c%d", 'A'+i-1, i))
			v.r[i], v.c[i], v.o[i], v.oc[i] = uint64(row), uint64(col), uint64(orow), uint64(ocol)
		}
		v.deletedRow, v.deletedOther = v.r[5], v.o[5]
		return v
	}
	copyOp := func(cut bool, fromRows, fromCols string, tab TabID, toRows, toCols string, v ids) string {
		return jsonf(`{"op":"copy","from":{"tab":%d,"rows":%s,"cols":%s},"to":{"tab":%d,"rows":%s,"cols":%s},"mode":"all","cut":%t}`,
			v.tab, fromRows, fromCols, tab, toRows, toCols, cut)
	}
	cases := []struct {
		name   string
		delete bool // Bob deletes row 5 of both tabs before the cut
		op     func(v ids, cut bool) string
		// written maps, for a plain paste of the same lists, each address
		// the paste writes to the address whose content it then holds.
		written map[string]string
	}{{
		name: "a row twice in the tab",
		op: func(v ids, cut bool) string {
			return copyOp(cut, idsJSON(v.r[1], v.r[2]), idsJSON(v.c[1]), v.tab, idsJSON(v.r[3], v.r[3]), idsJSON(v.c[1]), v)
		},
		written: map[string]string{"Sheet1!A3": "Sheet1!A2"},
	}, {
		name: "the source row twice over itself",
		op: func(v ids, cut bool) string {
			return copyOp(cut, idsJSON(v.r[1], v.r[2]), idsJSON(v.c[1]), v.tab, idsJSON(v.r[2], v.r[2]), idsJSON(v.c[1]), v)
		},
		written: map[string]string{"Sheet1!A2": "Sheet1!A2"},
	}, {
		name: "a column twice in the tab",
		op: func(v ids, cut bool) string {
			return copyOp(cut, idsJSON(v.r[1]), idsJSON(v.c[1], v.c[2]), v.tab, idsJSON(v.r[3]), idsJSON(v.c[3], v.c[3]), v)
		},
		written: map[string]string{"Sheet1!C3": "Sheet1!B1"},
	}, {
		name: "a row twice in another tab",
		op: func(v ids, cut bool) string {
			return copyOp(cut, idsJSON(v.r[1], v.r[2]), idsJSON(v.c[1]), v.other, idsJSON(v.o[2], v.o[2]), idsJSON(v.oc[1]), v)
		},
		written: map[string]string{"Other!A2": "Sheet1!A2"},
	}, {
		name: "a column twice in another tab",
		op: func(v ids, cut bool) string {
			return copyOp(cut, idsJSON(v.r[1]), idsJSON(v.c[1], v.c[2]), v.other, idsJSON(v.o[1]), idsJSON(v.oc[2], v.oc[2]), v)
		},
		written: map[string]string{"Other!B1": "Sheet1!B1"},
	}, {
		name: "a row twice next to a deleted one", delete: true,
		op: func(v ids, cut bool) string {
			return copyOp(cut, idsJSON(v.r[1], v.r[2]), idsJSON(v.c[1]), v.tab, idsJSON(v.deletedRow, v.r[4], v.r[4]), idsJSON(v.c[1]), v)
		},
		// The source repeats over the larger target: A1, A2, A1.
		written: map[string]string{"Sheet1!A4": "Sheet1!A1"},
	}, {
		name: "a row twice next to a deleted one in another tab", delete: true,
		op: func(v ids, cut bool) string {
			return copyOp(cut, idsJSON(v.r[1], v.r[2]), idsJSON(v.c[1]), v.other, idsJSON(v.deletedOther, v.o[2], v.o[2]), idsJSON(v.oc[1]), v)
		},
		written: map[string]string{"Other!A2": "Sheet1!A1"},
	}, {
		name: "a deleted row twice", delete: true,
		op: func(v ids, cut bool) string {
			return copyOp(cut, idsJSON(v.r[1], v.r[2]), idsJSON(v.c[1]), v.tab, idsJSON(v.deletedRow, v.deletedRow), idsJSON(v.c[1]), v)
		},
	}, {
		name: "an added row twice",
		op: func(v ids, cut bool) string {
			return copyOp(cut, idsJSON(v.r[1], v.r[2]), idsJSON(v.c[1]), v.other, `[{"new":1},{"new":1}]`, idsJSON(v.oc[1]), v)
		},
	}}
	setup := func(t *testing.T, del bool) (*env, ids) {
		e := start(t)
		e.mustAgent(cutSheet)
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"Other"},{"op":"write","tab":"Other","at":"A1","rows":[["o1","p1"],["o2","p2"]]},
			{"op":"format","tab":"Other","ranges":["A2"],"format":{"italic":true}}]}`)
		v := read(e)
		if del {
			bob := e.person("Bob")
			for _, tab := range []string{"Sheet1", "Other"} {
				if ack := bob.do(e.deleteRowsOp(tab, 5, 1)); status(ack) != "applied" {
					t.Fatalf("Bob's deletion was answered %s", status(ack))
				}
			}
		}
		return e, v
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, v := setup(t, c.delete)
			before, seq := e.cutRegion("Sheet1", "Other"), e.seq()
			rows, otherRows := e.rowCount("Sheet1"), e.rowCount("Other")
			ack := e.person("Ann").do(c.op(v, true))
			if got := status(ack); got != "refused:invalid" {
				t.Fatalf("the cut was answered %s %+v, want it refused as invalid", got, ack.Results)
			}
			for addr, was := range before {
				if got := e.cutCell(addr); got != was {
					t.Errorf("%s is %+v after the refused cut, want %+v", addr, got, was)
				}
			}
			if e.seq() != seq || e.rowCount("Sheet1") != rows || e.rowCount("Other") != otherRows {
				t.Errorf("after the refused cut the sheet is at %d with %d and %d rows, want %d with %d and %d",
					e.seq(), e.rowCount("Sheet1"), e.rowCount("Other"), seq, rows, otherRows)
			}
		})
		if c.written == nil {
			continue
		}
		t.Run(c.name+", paste", func(t *testing.T) {
			e, v := setup(t, c.delete)
			before := e.cutRegion("Sheet1", "Other")
			ack := e.person("Ann").do(c.op(v, false))
			if got := status(ack); got != "applied" {
				t.Fatalf("the paste was answered %s %+v", got, ack.Results)
			}
			for addr, was := range before {
				want := was
				if from, ok := c.written[addr]; ok {
					want = before[from]
				}
				got := e.cutCell(addr)
				if strings.HasPrefix(want.text, "=") && strings.HasPrefix(got.text, "=") {
					// A copied formula moves with its cell. Each source has
					// a format of its own, which tells which one the cell
					// holds.
					got, want = cutCell{format: got.format}, cutCell{format: want.format}
				}
				if got != want {
					t.Errorf("%s is %+v after the paste, want %+v", addr, got, want)
				}
			}
		})
	}
}

// TestRefusedRepeatedCutFromTheClient sends the real client's cut with its
// destination row named twice, as a client the host cannot trust might.
// The host refuses it, and the client, reading that ack, says that
// nothing changed, offers the text back and keeps the cut, which a second
// paste sends again.
func TestRefusedRepeatedCutFromTheClient(t *testing.T) {
	node := nodeOrSkip(t)
	e := start(t)
	e.mustAgent(cutSheet)
	s := serve(e)
	r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
	r.watch(e.tabID("Sheet1"))
	steps, _ := r.taken()
	cut := []any{
		map[string]any{"select": []int{0, 0, 1, 0}}, map[string]any{"cut": true},
		map[string]any{"select": []int{0, 2, 0, 2}}, map[string]any{"paste": "all"},
	}
	sent := play(t, node, steps, cut...).Frames
	if len(sent) != 1 || !strings.Contains(string(sent[0]), `"cut":true`) {
		t.Fatalf("the client sent %s, want one cut", sent)
	}
	var frame map[string]any
	if err := json.Unmarshal(sent[0], &frame); err != nil {
		t.Fatal(err)
	}
	_, row, _ := e.ids("A1")
	frame["ops"].([]any)[0].(map[string]any)["to"].(map[string]any)["rows"] = []any{row, row}
	repeated, _ := json.Marshal(frame)

	before := e.cutRegion("Sheet1")
	ack := r.answer(repeated)
	if len(ack.Results) != 1 || ack.Results[0].Status != "refused" {
		t.Fatalf("the repeated cut was answered %+v", ack.Results)
	}
	for addr, was := range before {
		if got := e.cutCell(addr); got != was {
			t.Errorf("%s is %+v after the refused cut, want %+v", addr, got, was)
		}
	}

	later, _ := r.taken()
	run := play(t, node, steps, append(cut, map[string]any{"steps": later[len(steps):]}, map[string]any{"paste": "all"})...)
	if len(run.Notices) == 0 || !strings.HasPrefix(run.Notices[len(run.Notices)-1], "The host refused this paste: ") || !strings.Contains(run.Notices[len(run.Notices)-1], "Nothing was changed") {
		t.Errorf("the client said %q, want that the host refused the paste and nothing changed", run.Notices)
	}
	if len(run.Frames) != 2 || !strings.Contains(string(run.Frames[1]), `"cut":true`) {
		t.Errorf("after the refusal the client sent %s, want the cut again", run.Frames)
	}
}
