package sheet

import (
	"log/slog"
	"strings"
	"testing"
)

// undoBudgets is what the undo budgets of a sheet read at one moment, with
// what the test counts itself beside the counters the sheet keeps.
type undoBudgets struct {
	// sessionNotes and sheetNotes are the counters of note bytes of
	// Alice's session and of the sheet. recounted is the size of every
	// note set of the sheet, measured here, and naming the number of sets
	// that name the entry that expires.
	sessionNotes, sheetNotes, recounted int64
	naming                              int
	// resident is the pool's count of payload bytes in memory, and stacks
	// the payloads on the stacks, measured here.
	resident, stacks int64
	files            int // scratch files
	segments, open   int // of the edit log
}

// An entry that passes a limit of undo history gives up the same state
// whichever limit it is, and at once: its payload, and the stand-in notes
// that name it with their bytes in the budgets of the session and of the
// sheet. Each case brings the same history to the same end and expires
// Alice's oldest entry by another limit. A session that fills its own
// budget is ordinary use and is not logged. The other limits are, by the
// same line.
func TestUndoExpiryReleasesTheSameStateOnEveryPath(t *testing.T) {
	const line = "sheet undo payload lost"
	cases := []struct {
		name string
		// lower sets the budgets for Alice's last undo. one is the size of
		// one of her note sets, and rest what the session and the sheet
		// hold beside the sets: the index of her notes, what her entries
		// leave in memory with every payload in a scratch file, the write
		// marks, and room for the record of the entry the undo adds. A set
		// is larger than rest, so that the second set passes a bound that
		// the first fits.
		lower func(p *undoScratch, one, rest int64)
		// atUndo says that the entry expires at that undo, by the notes
		// the undo adds. Otherwise both sets fit, and the entry expires at
		// Bob's deletion, by the edits its payload waits for.
		atUndo bool
		// logged is part of the cause the log line names. It is empty
		// when the expiry is not logged.
		logged string
	}{
		{"the edits a payload may wait for", func(p *undoScratch, one, rest int64) { p.sessionMax = 2*one + rest }, false, "waits for more structural edits"},
		{"the session's budget of notes", func(p *undoScratch, one, rest int64) { p.sessionMax = one + rest }, true, ""},
		{"the sheet's budget of notes", func(p *undoScratch, one, rest int64) { p.instanceMax = one + rest }, true, "stand-in notes"},
	}
	ends := make([]undoBudgets, len(cases))
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sink := &logSink{}
			e := start(t, func(o *Options) {
				o.Slug = "ledger"
				o.Log = slog.New(slog.NewTextHandler(sink, nil))
			})
			e.mustAgent(`{"ops":[{"op":"add_tab","name":"D"},{"op":"write","tab":"D","at":"A1","rows":[[1],[2],[3],[4],[5]]},
				{"op":"write","tab":"Sheet1","at":"A1","rows":[["=D!A5"]]}]}`)
			// Every payload goes to a scratch file, so that the bounds
			// below meet what payloads in scratch files leave in memory.
			e.in.mu.Lock()
			e.in.undoPool().entryMax = -1
			e.in.mu.Unlock()
			alice, bob := e.person("Alice"), e.person("Bob")
			// Each action sets a cell and formats its row over width
			// columns with nine properties, so that the notes of its undo
			// take more than the session holds beside them. The first
			// entry holds the formula, which names D. The undo of the
			// second leaves a note that names the first.
			const width = newTabCols - 2
			groups := 1 + 9*width
			edit := func(addr string, row int, text string, format int) {
				t.Helper()
				if ack := alice.do(e.setOp("Sheet1!"+addr, text), e.formatRange("Sheet1", row, 1, 1, width, nineFormats[format])); status(ack) != "applied,applied" {
					t.Fatalf("the edit of %s: %s", addr, status(ack))
				}
			}
			edit("A1", 1, "x", 0)
			edit("A1", 1, "y", 1)
			wantUndo(t, "undo of the second edit", undo(t, alice), undoAck{Applied: groups})
			edit("B1", 2, "p", 2)
			edit("B1", 2, "q", 3)
			one := alice.noteBytes() - noteIndexBytes
			e.in.mu.RLock()
			oldest := alice.s.undo.undo[0]
			seq := oldest.seq
			rest := alice.s.undo.resident() - one + e.in.wb.markBytes() + 1<<10
			e.in.mu.RUnlock()
			if one <= rest {
				t.Fatalf("a set of notes takes %d bytes and the rest %d: the case does not reach the bounds", one, rest)
			}
			read := func() undoBudgets {
				t.Helper()
				var b undoBudgets
				b.files, _ = e.scratch()
				b.segments, b.open = e.editLog()
				b.resident, b.stacks = e.residentUndo()
				e.in.mu.RLock()
				defer e.in.mu.RUnlock()
				e.in.smu.Lock()
				defer e.in.smu.Unlock()
				for _, s := range e.in.sessions {
					for _, sets := range s.undo.notes {
						for _, set := range sets {
							b.recounted += set.measure()
							if set.canon == seq {
								b.naming++
							}
						}
					}
				}
				b.sessionNotes, b.sheetNotes = alice.s.undo.noteBytes, e.in.scratch.notes.bytes
				return b
			}
			if b := read(); one == 0 || b.naming != 1 || b.recounted != one || b.sheetNotes != one+noteIndexBytes {
				t.Fatalf("before the last undo: one set of %d bytes, %+v", one, b)
			}
			// expired checks the budgets right after the entry expired,
			// with no tick between. The note of the second undo stays.
			expired := func(when string) {
				t.Helper()
				if !alice.lost(0) {
					t.Fatalf("%s: the entry did not expire", when)
				}
				b := read()
				if b.naming != 0 {
					t.Errorf("%s: %d note sets still name the entry that expired", when, b.naming)
				}
				if b.recounted != one || b.sessionNotes != one+noteIndexBytes || b.sheetNotes != one+noteIndexBytes {
					t.Errorf("%s: the session counts %d bytes of notes and the sheet %d, the sets take %d; want the %d of the one set that is left", when, b.sessionNotes, b.sheetNotes, b.recounted, one)
				}
				if b.resident != b.stacks {
					t.Errorf("%s: the pool counts %d bytes of payloads in memory and the stacks hold %d", when, b.resident, b.stacks)
				}
				got := sink.lines(line)
				if c.logged == "" {
					if len(got) != 0 {
						t.Errorf("%s: an expiry by the session's own budget was logged: %q", when, got)
					}
					return
				}
				if len(got) != 1 || !strings.Contains(got[0], c.logged) || !strings.Contains(got[0], "slug=ledger") || !strings.Contains(got[0], "level=WARN") || !strings.Contains(got[0], "suppressed=0") {
					t.Errorf("%s: the log lines about the expiry are %q, want one that names the sheet and the cause %q", when, got, c.logged)
				}
			}
			e.in.mu.Lock()
			p := e.in.undoPool()
			c.lower(p, one, rest)
			e.in.mu.Unlock()
			wantUndo(t, "undo of the newest edit", undo(t, alice), undoAck{Applied: groups})
			if c.atUndo {
				expired("after the undo")
			} else if b := read(); alice.lost(0) || b.naming != 1 || b.sessionNotes != 2*one+noteIndexBytes {
				t.Fatalf("after the undo: the entry expired: %v, %+v; want both note sets kept", alice.lost(0), b)
			}
			// Every case ends with the same deletion under the same
			// bounds: one byte of edits for a payload to wait for.
			e.in.mu.Lock()
			p.sessionMax, p.instanceMax, p.edits.entryMax = undoSessionBytes, undoInstanceBytes, 1
			e.in.mu.Unlock()
			if ack := bob.do(e.deleteRowsOp("D", 1, 1)); status(ack) != "applied" {
				t.Fatalf("the deletion: %s", status(ack))
			}
			expired("after the deletion")
			ends[i] = read()
			wantUndo(t, "undo of the entry that stayed", undo(t, alice), undoAck{Applied: groups})
			wantUndo(t, "undo of the entry that expired", undo(t, alice), undoAck{Skipped: 1, Reason: "lost"})
			e.want("A1", "x")
			e.want("B1", "")
		})
	}
	for i := range cases {
		if ends[i] != ends[0] {
			t.Errorf("the budgets after an expiry by %s read %+v, and after one by %s %+v", cases[i].name, ends[i], cases[0].name, ends[0])
		}
	}
}
