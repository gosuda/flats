package sheet

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestTypedEntry covers spec rule 5 on the engine: what a person types is
// read once, a recognized form sets a number format only on a cell without
// one, and import csv reads fields the same way while an agent write never
// turns a string into a number or a date.
func TestTypedEntry(t *testing.T) {
	e := start(t)
	alice := e.person("Alice")
	for _, c := range []struct{ addr, typed, shown, text string }{
		{"A1", "1,200.50", "1200.5", "1200.5"},
		{"A2", "12.5%", "12.50%", "0.125"},
		{"A3", "$1,200", "$1,200.00", "1200"},
		{"A4", "3/15/2026", "3/15/2026", "2026-03-15"},
		{"A5", "2:30 PM", "2:30 PM", "14:30"},
		{"A6", "true", "TRUE", "TRUE"},
		{"A7", "'007", "007", "'007"},
		{"A8", "=", "=", "="},
		{"A9", "=1+1", "2", "=1+1"},
		{"A10", "Mar 15, 2026", "Mar 15, 2026", "2026-03-15"},
		{"A11", "line one\nline two", "line one\nline two", "line one\nline two"},
	} {
		if ack := alice.set(c.addr, c.typed); status(ack) != "applied" {
			t.Fatalf("%s %q: %s", c.addr, c.typed, status(ack))
		}
		if e.shown(c.addr) != c.shown || e.text(c.addr) != c.text {
			t.Errorf("%s typed %q shows %q and holds %q, want %q and %q", c.addr, c.typed, e.shown(c.addr), e.text(c.addr), c.shown, c.text)
		}
	}
	// A cell that already has a number format keeps it: the entry sets none.
	e.mustAgent(`{"ops":[{"op":"format","tab":"Sheet1","ranges":["B1:B3"],"format":{"number":{"kind":"number","decimals":1,"thousands":true}}}]}`)
	alice.set("B1", "50%")
	alice.set("B2", "$1,234.56")
	alice.set("B3", "2026-03-15")
	e.want("B1", "0.5")
	e.want("B2", "1,234.6")
	e.want("B3", "46,096.0")
	// The format an entry set stays when plain content replaces it, and a
	// later entry of another form does not replace it either.
	alice.set("A2", "0.5")
	e.want("A2", "50.00%")
	alice.set("A2", "$3")
	e.want("A2", "300.00%")
	// Clearing keeps the format; an undo gives content and format back.
	alice.do(jsonf(`{"op":"clear","tab":%d,"rows":%s,"cols":%s}`, e.tabID("Sheet1"), e.rowsOf("Sheet1", 2, 1), e.colsOf("Sheet1", 1, 1)))
	e.want("A2", "")
	alice.set("A2", "0.25")
	e.want("A2", "25.00%")
	alice.set("C1", "75%")
	undo(t, alice)
	e.want("C1", "")
	alice.set("C1", "0.75")
	e.want("C1", "0.75")

	// import csv reads every field as typed; an agent write never does.
	e.mustAgent(`{"ops":[{"op":"import_csv","tab":"Sheet1","at":"E1","csv":"Item,Amount,Due\r\n\"Venue, hall\",\"1,200.50\",2026-03-15\r\n'007,50%,\r\n=E2&\"!\",TRUE,\"say \"\"hi\"\"\"\r\n"},
		{"op":"write","tab":"Sheet1","at":"E6","rows":[["1,200.50","50%","2026-03-15","TRUE"]]}]}`)
	for addr, want := range map[string]string{
		"E2": "Venue, hall", "F2": "1200.5", "G2": "2026-03-15", "E3": "007", "F3": "50%", "G3": "",
		"E4": "Venue, hall!", "F4": "TRUE", "G4": `say "hi"`,
		"E6": "1,200.50", "F6": "50%", "G6": "2026-03-15", "H6": "TRUE",
	} {
		e.want(addr, want)
	}
	out := e.read(ReadQuery{Tab: "Sheet1", Range: "E6:H6"})
	if got := firstCell(out); got != "1,200.50" {
		t.Errorf("an agent's string reads back as %v", got)
	}
	out = e.read(ReadQuery{Tab: "Sheet1", Range: "G2:G2"})
	if got, ok := firstCell(out).(map[string]any); !ok || got["date"] != "2026-03-15" {
		t.Errorf("an imported date reads back as %v", firstCell(out))
	}
}

// TestClockShapes gives an agent's write and a revision file a time in every
// shape but hh:mm and hh:mm:ss of two digits each. The write is refused and
// the file does not load.
func TestClockShapes(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[{"time":"09:05"},{"datetime":"2026-03-15T23:59:59"}]]}]}`)
	e.want("A1", "09:05:00")
	rev := e.b.importAll(t)
	files, _, release, err := e.b.RevisionFiles(context.Background(), rev)
	if err != nil {
		t.Fatal(err)
	}
	release()
	stored := &revisionSet{files: files, data: e.b.blobs}
	var rows string
	for _, f := range files {
		if strings.Contains(f.Path, "/rows/") {
			rows = f.Path
		}
	}
	if _, err := loadRevision(stored.files, stored.read, nil); err != nil || rows == "" {
		t.Fatalf("the stored revision: %v, row file %q", err, rows)
	}
	for _, clock := range []string{"+1:+2:+3", "-0:00", "+9:05", "09:+5", "9:05", "09:5", "09:05:5", "009:05", "09:05:00:00", "09", "24:00", "09:60", "09:05:60", " 9:05", "09:05 ", "0x:05", "٠٩:٠٥", ""} {
		for _, cell := range []string{`{"time":%q}`, `{"datetime":"2026-03-15T%s"}`} {
			cell = fmt.Sprintf(cell, clock)
			if cell == `{"datetime":"2026-03-15T"}` {
				continue
			}
			_, err := e.agent(`{"ops":[{"op":"write","tab":"Sheet1","at":"C1","rows":[[` + cell + `]]}]}`)
			e.wantRefusal(err, "invalid", "cell")
		}
		for _, cell := range []string{`{"t":%q}`, `{"dt":"2026-03-15T%s"}`} {
			file := fmt.Sprintf(`{"rows":[{"cells":{"1":`+cell+`},"id":1}],"styles":[]}`, clock)
			set := stored.with(rows, []byte(file))
			if _, err := loadRevision(set.files, set.read, nil); err == nil {
				t.Errorf("a row file with the time %q loaded", clock)
			}
		}
	}
	e.wantText("C1", "")
}
