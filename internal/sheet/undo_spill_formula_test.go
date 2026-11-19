package sheet

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// A payload holds an unreadable formula as its typed text with a flag. In
// a scratch file whose checksum fits, text under that flag that the lexer
// can split, as =A1+B1 is, would be a readable formula with references
// read from A1 instead of from its own cell: the decoder refuses it, the
// undo is skipped as lost and changes nothing, and the entry before it
// still applies. An unreadable formula that is really one comes back from
// its file as it was.
func TestSpilledUnreadableFormulaIsChecked(t *testing.T) {
	const unreadable, readable = "=A1;B1", "=A1+B1"
	setup := func(t *testing.T, e *env) *person {
		t.Helper()
		e.in.mu.Lock()
		e.in.undoPool().entryMax = -1
		e.in.mu.Unlock()
		alice := e.person("Alice")
		alice.set("C1", "early")
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,2],["` + unreadable + `"]]}]}`)
		alice.set("A2", "kept")
		if alice.spilled(1) == nil {
			t.Fatal("the payload was not spilled")
		}
		return alice
	}
	t.Run("a readable text under the flag", func(t *testing.T) {
		sink := &logSink{}
		e := start(t, func(o *Options) { o.Log = slog.New(slog.NewTextHandler(sink, nil)) })
		alice := setup(t, e)
		file := alice.spilled(1)
		data, err := os.ReadFile(file.path)
		if err != nil {
			t.Fatal(err)
		}
		body := data[:len(data)-4]
		if bytes.Count(body, []byte(unreadable)) != 1 {
			t.Fatalf("the payload holds the formula %d times", bytes.Count(body, []byte(unreadable)))
		}
		body = bytes.Replace(body, []byte(unreadable), []byte(readable), 1)
		data = binary.LittleEndian.AppendUint32(body, crc32.ChecksumIEEE(body))
		if err := os.WriteFile(file.path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		seq, before := e.seq(), e.dump()
		wantUndo(t, "undo of the damaged payload", undo(t, alice), undoAck{Skipped: 1, Reason: "lost"})
		if e.seq() != seq || e.dump() != before {
			t.Fatal("an undo that was lost changed the sheet")
		}
		if lines := sink.lines("sheet undo payload lost"); len(lines) != 1 || !strings.Contains(lines[0], "an unreadable formula that can be read") {
			t.Fatalf("the log lines about the payload are %q", lines)
		}
		wantUndo(t, "undo of the entry before", undo(t, alice), undoAck{Applied: 1})
		e.want("C1", "")
		e.wantText("A2", "kept")
	})
	t.Run("an unreadable formula", func(t *testing.T) {
		e := start(t)
		alice := setup(t, e)
		wantUndo(t, "undo of the spilled entry", undo(t, alice), undoAck{Applied: 1})
		e.wantText("A2", unreadable)
		e.want("A2", "#ERROR!")
		_, row, col := e.ids("A2")
		e.in.mu.RLock()
		f := e.in.wb.tabs[0].cell(row, col).f
		e.in.mu.RUnlock()
		if f == nil || !f.Unreadable() {
			t.Fatal("the formula that came back is not the unreadable one")
		}
		e.restart()
		e.wantText("A2", unreadable)
	})
}

// retainedForm is a stored form of n bytes made as the longest form of
// formula.MaxRetainedBytes is: as many references as a formula of
// formula.MaxFormulaChars holds with room for a text, each the longest slot
// a transform makes, 174 bytes with a tab name of 31 code points of four
// bytes, and the text to fill.
func retainedForm(t *testing.T, n int) string {
	t.Helper()
	corner := "R[-999999999]C[-18277]"
	slot := "{'" + strings.Repeat("\U0001F600", maxTabName) + "'!" + corner + ":" + corner + "}"
	if len(slot) != 174 {
		t.Fatalf("the slot has %d bytes", len(slot))
	}
	refs := "=" + slot + strings.Repeat("+"+slot, formula.MaxFormulaChars/2-2)
	return refs + `+"` + strings.Repeat("x", n-len(refs)-len(`+""`)) + `"`
}

// swapSpilledFormula puts the stored form to in place of the form from,
// which the payload of file holds once, with its length, and writes the
// checksum of the new payload.
func swapSpilledFormula(t *testing.T, file *undoFile, from, to string) {
	t.Helper()
	data, err := os.ReadFile(file.path)
	if err != nil {
		t.Fatal(err)
	}
	body := data[:len(data)-4]
	at := bytes.Index(body, []byte(from))
	if bytes.Count(body, []byte(from)) != 1 || at < 1 || body[at-1] != byte(len(from)) {
		t.Fatalf("the payload does not hold the form %q once with its length", from)
	}
	swapped := append(binary.AppendUvarint(slices.Clone(body[:at-1]), uint64(len(to))), to...)
	swapped = append(swapped, body[at+len(from):]...)
	data = binary.LittleEndian.AppendUint32(swapped, crc32.ChecksumIEEE(swapped))
	if err := os.WriteFile(file.path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// L-20 of the technical design, section 22.2: the decoder of a scratch
// file reads a stored form with formula.ParseRetained, which holds it to
// formula.MaxRetainedBytes, 745,472. A form of 745,473 bytes is damaged:
// its undo is skipped as lost and changes nothing, and the entry before it
// still applies. A form of 745,472 bytes is read, and its undo is refused
// only because its text at the cell is longer than a formula may be.
func TestUndoOfARetainedFormAtItsBound(t *testing.T) {
	const spilled = `="spilled"`
	at, over := retainedForm(t, formula.MaxRetainedBytes), retainedForm(t, formula.MaxRetainedBytes+1)
	if len(at) != 745472 || len(over) != 745473 {
		t.Fatalf("the forms have %d and %d bytes", len(at), len(over))
	}
	if _, err := formula.ParseRetained(at); err != nil {
		t.Fatalf("ParseRetained of %d bytes: %v", len(at), err)
	}
	if _, err := formula.ParseRetained(over); !errors.Is(err, formula.ErrStored) {
		t.Fatalf("ParseRetained of %d bytes: %v", len(over), err)
	}
	setup := func(t *testing.T, form string) (*env, *person, *logSink) {
		t.Helper()
		sink := &logSink{}
		e := start(t, func(o *Options) { o.Log = slog.New(slog.NewTextHandler(sink, nil)) })
		e.in.mu.Lock()
		e.in.undoPool().entryMax = -1
		e.in.mu.Unlock()
		alice := e.person("Alice")
		alice.set("C1", "early")
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A2","rows":[["` + strings.ReplaceAll(spilled, `"`, `\"`) + `"]]}]}`)
		alice.set("A2", "kept")
		file := alice.spilled(1)
		if file == nil {
			t.Fatal("the payload was not spilled")
		}
		swapSpilledFormula(t, file, spilled, form)
		return e, alice, sink
	}
	t.Run("745,473 bytes", func(t *testing.T) {
		e, alice, sink := setup(t, over)
		seq, before := e.seq(), e.dump()
		wantUndo(t, "undo of the form past the bound", undo(t, alice), undoAck{Skipped: 1, Reason: "lost"})
		if e.seq() != seq || e.dump() != before {
			t.Fatal("an undo that was lost changed the sheet")
		}
		if lines := sink.lines("sheet undo payload lost"); len(lines) != 1 || !strings.Contains(lines[0], "is damaged") || !strings.Contains(lines[0], formula.ErrStored.Error()) {
			t.Fatalf("the log lines about the payload are %q", lines)
		}
		wantUndo(t, "undo of the entry before", undo(t, alice), undoAck{Applied: 1})
		e.want("C1", "")
		e.wantText("A2", "kept")
	})
	t.Run("745,472 bytes", func(t *testing.T) {
		e, alice, sink := setup(t, at)
		seq, before := e.seq(), e.dump()
		if ack := alice.do(`{"op":"undo"}`); status(ack) != "refused:formula_length" {
			t.Fatalf("undo of the form at the bound = %s %+v", status(ack), ack.Undo)
		}
		if e.seq() != seq || e.dump() != before {
			t.Fatal("a refused undo changed the sheet")
		}
		if lines := sink.lines("sheet undo payload lost"); len(lines) != 0 {
			t.Fatalf("the payload of the form at the bound was lost: %q", lines)
		}
	})
}
