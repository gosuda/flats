package sheet

import (
	"log/slog"
	"math"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// contentOf returns the content of a cell under the read lock.
func (e *env) contentOf(addr string) formula.Content {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	t, row, col := e.at(addr)
	if c := t.cell(row, col); c != nil {
		return c.content
	}
	return formula.Content{}
}

// A spilled payload saves the content a person's write replaced. Content
// that no cell holds, or that sheet.db and a row file would not read back
// as the same, is damage: a date that is no whole day of the years a date
// reads, a time outside a day or between two seconds, a date with time
// outside those days, a boolean other than 0 or 1, text beside a number or
// a blank, a number beside a text and a text past the text limit. Its undo
// is skipped as lost and changes nothing, and the person's earlier entry
// still applies. Content a writer stores comes back as it was.
func TestUndoChecksTheContentOfAPayload(t *testing.T) {
	setup := `{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[{"date":"2026-03-15"},{"time":"14:30:05"},{"datetime":"2026-03-15T14:30:05"},true,"text",3]]}]}`
	cases := []struct {
		name, addr string
		change     func(c *formula.Content)
	}{
		{"day 0", "A1", func(c *formula.Content) { c.Num = 0 }},
		{"a day after 9999-12-31", "A1", func(c *formula.Content) { c.Num = lastDay + 1 }},
		{"half a day", "A1", func(c *formula.Content) { c.Num += 0.5 }},
		{"a time of a whole day", "B1", func(c *formula.Content) { c.Num = 1 }},
		{"a negative time", "B1", func(c *formula.Content) { c.Num = -0.25 }},
		{"half a second", "B1", func(c *formula.Content) { c.Num = 0.5 / 86400 }},
		{"a date with time on day 1", "C1", func(c *formula.Content) { c.Num = 1.5 }},
		{"a date with time after 9999-12-31", "C1", func(c *formula.Content) { c.Num = lastDay + 1.25 }},
		{"a date with time and half a second", "C1", func(c *formula.Content) { c.Num += 0.5 / 86400 }},
		{"a boolean of one half", "D1", func(c *formula.Content) { c.Num = 0.5 }},
		{"a number with a text", "F1", func(c *formula.Content) { c.Text = "x" }},
		{"a text with a number", "E1", func(c *formula.Content) { c.Num = 2 }},
		{"a text past the text limit", "E1", func(c *formula.Content) { c.Text = strings.Repeat("x", formula.MaxTextLen+1) }},
		{"a blank with a number", "G1", func(c *formula.Content) { c.Num = 1 }},
		{"a blank with a text", "G1", func(c *formula.Content) { c.Text = "x" }},
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
			alice.set("Sheet1!Z9", "first")
			if ack := alice.set(c.addr, "new"); status(ack) != "applied" {
				t.Fatalf("the write: %s", status(ack))
			}
			alice.rewriteNewest(func(u *undoEntry) {
				if len(u.cells) != 1 {
					t.Fatalf("the write saved %d cells", len(u.cells))
				}
				c.change(&u.cells[0].before)
			})
			seq, before := e.seq(), e.dump()
			wantUndo(t, "undo of the payload", undo(t, alice), undoAck{Skipped: 1, Reason: "lost"})
			if e.seq() != seq || e.dump() != before {
				t.Fatal("an undo that was lost changed the sheet")
			}
			if lines := sink.lines("sheet undo payload lost"); len(lines) != 1 || !strings.Contains(lines[0], "is damaged") {
				t.Fatalf("the log lines about the payload are %q", lines)
			}
			wantUndo(t, "undo of the earlier entry", undo(t, alice), undoAck{Applied: 1})
			e.want("Z9", "")
		})
	}

	// The legitimate values go through a scratch file and back, at the
	// ends of their ranges too, and the sheet opens again with them.
	t.Run("legitimate content", func(t *testing.T) {
		e := start(t)
		e.mustAgent(setup)
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A2","rows":[[{"date":"1900-01-01"},{"date":"9999-12-31"},{"time":"00:00:00"},{"time":"23:59:59"},{"datetime":"1900-01-01T00:00:00"},{"datetime":"9999-12-31T23:59:59"},false]]}]}`)
		e.in.mu.Lock()
		e.in.undoPool().entryMax = -1
		e.in.mu.Unlock()
		alice := e.person("Alice")
		addrs := []string{"A1", "B1", "C1", "D1", "E1", "F1", "G1", "A2", "B2", "C2", "D2", "E2", "F2", "G2"}
		saved := make([]formula.Content, len(addrs))
		for i, addr := range addrs {
			saved[i] = e.contentOf(addr)
			if ack := alice.set(addr, "new"); status(ack) != "applied" {
				t.Fatalf("the write of %s: %s", addr, status(ack))
			}
		}
		for range addrs {
			wantUndo(t, "undo", undo(t, alice), undoAck{Applied: 1})
		}
		e.restart()
		for i, addr := range addrs {
			got := e.contentOf(addr)
			if got.Kind != saved[i].Kind || math.Float64bits(got.Num) != math.Float64bits(saved[i].Num) || got.Text != saved[i].Text {
				t.Errorf("%s came back as %+v, want %+v", addr, got, saved[i])
			}
		}
		for addr, kind := range map[string]formula.ContentKind{"A1": formula.ContentDate, "B1": formula.ContentTime, "C1": formula.ContentDateTime, "D1": formula.ContentBool, "E1": formula.ContentText, "F1": formula.ContentNumber, "G1": formula.ContentBlank, "F2": formula.ContentDateTime} {
			if k := e.contentOf(addr).Kind; k != kind {
				t.Errorf("%s holds content of kind %d, want %d", addr, k, kind)
			}
		}
	})
}
