package sheet

import (
	"fmt"
	"strings"
	"testing"
)

// symbol returns the i-th of many currency symbols of three letters of the
// Deseret alphabet: 12 bytes of UTF-8, which a format holds in a text of
// its own.
func symbol(i int) string {
	const first, letters = 0x10400, 80
	return string([]rune{first + rune(i%letters), first + rune(i/letters%letters), first + rune(i/(letters*letters)%letters)})
}

// TestUndoCountsTheSymbolsOfFormats has an agent give cells currency
// formats with symbols of their own, decoded from its calls, and three
// people change the number format of their cells to General, four times
// each. Their entries are the last holders of the earlier formats: the
// cells, the style table and the formats sent to clients hold the new
// ones. What the heap lets go when their history is dropped is no more
// than undo counted.
func TestUndoCountsTheSymbolsOfFormats(t *testing.T) {
	slow(t, "formats 36,000 cells with symbols of their own")
	const people, rounds, cols = 3, 4, 3
	rows := raced(newTabRows, 60)
	cells := rows * cols
	e := start(t)
	var list []*person
	var general []string
	for k := range people {
		list = append(list, e.person(fmt.Sprint("P", k)))
		general = append(general, jsonf(`{"op":"format","tab":%d,"rows":%s,"cols":%s,"set":{"number":{"kind":"general"}}}`,
			e.tabID("Sheet1"), e.rowsOf("Sheet1", 1, rows), e.colsOf("Sheet1", k*cols+1, cols)))
	}
	for round := range rounds {
		ops := make([]string, people*cells)
		for i := range ops {
			ops[i] = jsonf(`{"op":"format","tab":"Sheet1","ranges":["%c%d"],"format":{"number":{"kind":"currency","symbol":%q,"decimals":2}}}`,
				'A'+i/rows, i%rows+1, symbol(round*len(ops)+i))
		}
		e.mustAgent(`{"ops":[` + strings.Join(ops, ",") + `]}`)
		for k, p := range list {
			if ack := p.do(general[k]); status(ack) != "applied" {
				t.Fatalf("round %d: the format of %s: %s", round, p.s.label, status(ack))
			}
		}
	}
	e.in.mu.Lock()
	for _, p := range list {
		for _, entry := range p.s.undo.undo {
			if entry.file != nil || len(entry.cells) != cells {
				e.in.mu.Unlock()
				t.Fatalf("an entry holds %d cells, in a scratch file %v; want %d in memory", len(entry.cells), entry.file != nil, cells)
			}
		}
	}
	e.in.wb.compactWire()
	e.in.mu.Unlock()
	wantCharges(t, e)
	wantUndoHeap(t, e, list...)
}

// TestUndoCountsFollowedFormulas has Alice clear long formulas that read
// another tab, and an agent then insert a row into that tab, which gives
// each formula her entries hold another text. The formulas are long text
// written in them and one reference, so their text is most of what they
// take. The entries keep the formulas they follow and nothing of the texts
// before. What the heap lets go when her history is dropped is no more
// than undo counted, and her undo brings back the formula the insert made.
func TestUndoCountsFollowedFormulas(t *testing.T) {
	slow(t, "rewrites 200 formulas of 7,000 characters")
	const formulas = 4
	entries := raced(maxUndoEntries/2, 4)
	text := func(row, ref int) string {
		return fmt.Sprintf(`="%s"&D!A%d`, strings.Repeat(string(rune('a'+row%26)), 7000), ref)
	}
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"D"}]}`)
	e.fillColumn("Sheet1", "A", entries*formulas, func(row int) string { return text(row, 5) })
	alice := e.person("Alice")
	for k := range entries {
		if ack := alice.do(e.clearOp("Sheet1", k*formulas+1, formulas, 1, 1)); status(ack) != "applied" {
			t.Fatalf("clear %d: %s", k, status(ack))
		}
	}
	e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"D","at":1,"count":1}]}`)
	e.in.mu.RLock()
	for _, entry := range alice.s.undo.undo {
		if entry.file != nil {
			e.in.mu.RUnlock()
			t.Fatal("an entry went to a scratch file")
		}
	}
	e.in.mu.RUnlock()
	wantCharges(t, e)
	if u := undo(t, alice); u.Applied != formulas {
		t.Fatalf("the undo of the last clear = %+v", u)
	}
	last := (entries-1)*formulas + 1
	if got, want := e.text(fmt.Sprintf("Sheet1!A%d", last)), text(last, 6); got != want {
		t.Fatalf("the undo brought back %.40q...%q, want ...%q", got, got[max(0, len(got)-8):], want[len(want)-8:])
	}
	wantUndoHeap(t, e, alice)
}
