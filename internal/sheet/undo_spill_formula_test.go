package sheet

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"log/slog"
	"os"
	"strings"
	"testing"
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
