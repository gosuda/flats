package sheet

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// checksummed returns a scratch file with the given magic and what body
// writes after it, closed by the checksum of all of it: a file whose
// bytes are intact and whose structure is whatever body makes it. Zeros
// follow the body, so that the lists body opens have the bytes for their
// first thing and the decoder comes to the length body ends on.
func checksummed(magic string, body func(enc *spillEnc)) []byte {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	w.WriteString(magic)
	body(&spillEnc{w: w, formulas: map[*formula.Formula]uint64{}})
	w.Write(make([]byte, 128))
	w.Flush()
	var tail [4]byte
	binary.LittleEndian.PutUint32(tail[:], crc32.ChecksumIEEE(buf.Bytes()))
	return append(buf.Bytes(), tail[:]...)
}

// tooLong is a length no payload has.
const tooLong = 1 << 31

// manySteps is a number of steps or edits that each name as many ids as a
// tab has rows: 51 MiB of ids from a file of a few KiB.
const manySteps = 64

// plainStep writes the fields of a step after its ids, all of them empty,
// and no deleted tab.
func plainStep(enc *spillEnc) {
	enc.uv(0) // above
	enc.uv(0) // below
	enc.flag(false)
	enc.iv(0)       // at
	enc.uv(0)       // saved rows
	enc.uv(0)       // saved cells
	enc.flag(false) // placed
	enc.uv(0)       // widths
	enc.uv(0)       // repairs
	enc.uv(0)       // col
	enc.iv(0)
	enc.uv(0)
	enc.flag(false) // had
	enc.iv(0)       // frozen
	enc.iv(0)
	enc.str("")
	enc.uv(0) // order
	enc.uv(0) // seq
	enc.uv(0) // prev
	enc.uv(0) // stamps
	enc.uv(0) // sorted
	enc.flag(false)
}

// A scratch file can hold lengths that its bytes do not back: after a
// fault of the disk or of the host that the checksum does not show. The
// decoder checks each length against the bytes that are left and against
// what a sheet can hold before it makes room for it, so such a file costs
// its entry and no memory. Each case writes a file with a checksum that
// fits and one length that does not.
func TestUndoScratchLengthsAreChecked(t *testing.T) {
	// zeros pads a file, so that a length is refused for what it means
	// and not for the bytes behind it.
	zeros := func(enc *spillEnc, n int) { enc.w.Write(make([]byte, n)) }
	// step writes a step up to its ids, and rest the fields between its
	// ids and its saved rows.
	step := func(enc *spillEnc) {
		enc.uv(0) // styles
		enc.uv(0) // cells
		enc.uv(1) // steps
		enc.uv(stepDeleted)
		enc.uv(1) // tab
		enc.flag(true)
	}
	rest := func(enc *spillEnc) {
		enc.uv(0) // ids
		enc.uv(0)
		enc.uv(0)
		enc.flag(false)
		enc.iv(0)
	}
	// tab writes a step with a deleted tab up to the tab's last sorts.
	tab := func(enc *spillEnc) {
		step(enc)
		rest(enc)
		for i := 0; i < 2; i++ { // saved rows, cells
			enc.uv(0)
		}
		enc.flag(false)
		for i := 0; i < 2; i++ { // widths, repairs
			enc.uv(0)
		}
		enc.uv(0) // col
		enc.iv(0)
		enc.uv(0)
		enc.flag(false)
		enc.iv(0)
		enc.iv(0)
		enc.str("")
		enc.uv(0) // order
		enc.uv(0)
		enc.uv(0)
		enc.uv(0) // stamps
		enc.uv(0) // sorted
		enc.flag(true)
		enc.uv(7)
		enc.str("Gone")
		for i := 0; i < 4; i++ {
			enc.uv(0)
		}
		for i := 0; i < 2; i++ { // the axes: ids, born
			enc.uv(0)
			enc.uv(0)
		}
		enc.uv(0) // widths
		enc.iv(0)
		enc.iv(0)
		enc.uv(0) // chunks
	}
	// cell writes one cell of the entry up to its content.
	cell := func(enc *spillEnc) {
		enc.uv(0) // styles
		enc.uv(1) // cells
		enc.uv(1)
		enc.uv(1)
		enc.uv(1)
		enc.flag(true)
	}
	// cause is what the decoder says of the length, in the host's log.
	const long = "a count of 2147483648 with"
	payloads := []struct {
		name, cause string
		body        func(enc *spillEnc)
	}{
		{"styles", long, func(enc *spillEnc) { enc.uv(tooLong) }},
		{"more styles than the file has bytes for", "a count of 1048576 with", func(enc *spillEnc) {
			enc.uv(1 << 20)
			zeros(enc, 4096)
		}},
		{"the text of a format", long, func(enc *spillEnc) {
			enc.uv(1)
			enc.uv(5)
			enc.uv(0)
			enc.uv(tooLong)
		}},
		{"cells", long, func(enc *spillEnc) {
			enc.uv(0)
			enc.uv(tooLong)
		}},
		{"more cells than an action changes", "a count of 2000001 with", func(enc *spillEnc) {
			enc.uv(0)
			enc.uv(journalMax/journalEntryBytes + 1)
			zeros(enc, 1<<10)
		}},
		{"the text of a cell", long, func(enc *spillEnc) {
			cell(enc)
			enc.uv(0)
			enc.uv(uint64(formula.ContentText))
			zeros(enc, 8)
			enc.uv(tooLong)
			enc.w.WriteString("text")
		}},
		{"the text of a formula", long, func(enc *spillEnc) {
			cell(enc)
			enc.uv(1)
			enc.flag(false)
			enc.uv(tooLong)
			enc.w.WriteString("A1")
		}},
		{"steps", long, func(enc *spillEnc) {
			enc.uv(0)
			enc.uv(0)
			enc.uv(tooLong)
		}},
		{"ids", "a list of 2147483648 ids", func(enc *spillEnc) {
			step(enc)
			enc.uv(tooLong)
			enc.uv(1)
			enc.uv(tooLong)
		}},
		{"more ids than a tab has rows", "a list of 100001 ids", func(enc *spillEnc) {
			step(enc)
			enc.uv(formula.MaxRows + 1)
			enc.uv(1)
			enc.uv(formula.MaxRows + 1)
			zeros(enc, 1<<10)
		}},
		{"a run of ids past its list", "an id run of 2147483648", func(enc *spillEnc) {
			step(enc)
			enc.uv(8)
			enc.uv(1)
			enc.uv(tooLong)
		}},
		{"saved rows", long, func(enc *spillEnc) {
			step(enc)
			rest(enc)
			enc.uv(tooLong)
		}},
		{"the cells of a saved row", long, func(enc *spillEnc) {
			step(enc)
			rest(enc)
			enc.uv(1)
			enc.uv(9)
			enc.iv(0)
			enc.uv(tooLong)
		}},
		{"more cells in a row than a tab has columns", "a count of 703 with", func(enc *spillEnc) {
			step(enc)
			rest(enc)
			enc.uv(1)
			enc.uv(9)
			enc.iv(0)
			enc.uv(formula.MaxCols + 1)
			zeros(enc, 64<<10)
		}},
		{"saved cells", long, func(enc *spillEnc) {
			step(enc)
			rest(enc)
			enc.uv(0)
			enc.uv(tooLong)
		}},
		{"the name of a step", long, func(enc *spillEnc) {
			step(enc)
			rest(enc)
			for i := 0; i < 2; i++ {
				enc.uv(0)
			}
			enc.flag(false)
			for i := 0; i < 2; i++ {
				enc.uv(0)
			}
			enc.uv(0)
			enc.iv(0)
			enc.uv(0)
			enc.flag(false)
			enc.iv(0)
			enc.iv(0)
			enc.uv(tooLong)
		}},
		{"a name longer than a tab's", "a count of 401 with", func(enc *spillEnc) {
			step(enc)
			rest(enc)
			for i := 0; i < 2; i++ {
				enc.uv(0)
			}
			enc.flag(false)
			for i := 0; i < 2; i++ {
				enc.uv(0)
			}
			enc.uv(0)
			enc.iv(0)
			enc.uv(0)
			enc.flag(false)
			enc.iv(0)
			enc.iv(0)
			enc.str(strings.Repeat("n", 4*maxTabName+1))
			zeros(enc, 64)
		}},
		{"the last sorts of a deleted tab", long, func(enc *spillEnc) {
			tab(enc)
			enc.uv(tooLong)
		}},
		{"the last sort of a row of a deleted tab twice", "holds 5 twice", func(enc *spillEnc) {
			tab(enc)
			enc.uv(2)
			for i := 0; i < 2; i++ {
				enc.uv(5) // the row, its sort and the sorter
				enc.uv(1)
				enc.uv(0)
			}
		}},
		{"the rows of a deleted tab", long, func(enc *spillEnc) {
			tab(enc)
			enc.uv(0)
			enc.uv(tooLong)
		}},
		{"the marks of a deleted tab", long, func(enc *spillEnc) {
			tab(enc)
			enc.uv(0)
			enc.uv(0)
			enc.uv(0) // recovered
			for i := 0; i < 3; i++ {
				enc.uv(0)
			}
			enc.uv(tooLong)
		}},
		{"deletions kept apart", long, func(enc *spillEnc) {
			for i := 0; i < 3; i++ {
				enc.uv(0)
			}
			enc.uv(tooLong)
		}},
		// A run of two values stands for as many ids as a tab has rows, so
		// a file of a few KiB could name many times as many ids as any
		// payload of its size held.
		{"more ids than the payload was written with", "more ids than", func(enc *spillEnc) {
			enc.uv(0) // styles
			enc.uv(0) // cells
			enc.uv(manySteps)
			for i := 0; i < manySteps; i++ {
				enc.uv(stepDeleted)
				enc.uv(1) // tab
				enc.flag(true)
				enc.uv(formula.MaxRows)
				enc.uv(1)
				enc.uv(formula.MaxRows)
				plainStep(enc)
			}
		}},
	}
	// lost undoes the entry whose file was replaced and checks that the
	// undo is skipped as lost, changes nothing and allocates little.
	lost := func(t *testing.T, e *env, alice *person, sink *logSink, cause string) {
		t.Helper()
		seq := e.seq()
		before := allocated()
		u := undo(t, alice)
		grown := allocated() - before
		wantUndo(t, "undo of the damaged payload", u, undoAck{Skipped: 1, Reason: "lost"})
		if e.seq() != seq {
			t.Fatal("an undo that was lost changed the sheet")
		}
		if measures && grown > 2<<20 {
			t.Fatalf("the undo of a damaged payload allocated %d KiB", grown>>10)
		}
		if lines := sink.lines("sheet undo payload lost"); len(lines) != 1 || !strings.Contains(lines[0], "is damaged") || !strings.Contains(lines[0], cause) {
			t.Fatalf("the log lines about the payload are %q, want one that says it is damaged by %q", lines, cause)
		}
	}
	for _, c := range payloads {
		t.Run("a payload, "+c.name, func(t *testing.T) {
			sink := &logSink{}
			e := start(t, func(o *Options) { o.Log = slog.New(slog.NewTextHandler(sink, nil)) })
			e.in.mu.Lock()
			e.in.undoPool().entryMax = -1
			e.in.mu.Unlock()
			alice := e.person("Alice")
			alice.set("A1", "kept")
			file := alice.spilled(0)
			if file == nil {
				t.Fatal("the payload was not spilled")
			}
			if err := os.WriteFile(file.path, checksummed(spillMagic, c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			lost(t, e, alice, sink, c.cause)
			e.want("A1", "kept")
		})
	}
	// edit writes one edit of a segment of the edit log up to the ids it
	// took.
	edit := func(enc *spillEnc) {
		enc.uv(1) // edits
		enc.uv(1) // its ordinal
		enc.uv(9)
		enc.uv(2)
		enc.uv(uint64(formula.DeleteRows))
		enc.str("D")
		enc.str("")
		enc.iv(0)
		enc.iv(1)
		enc.flag(true)
	}
	segments := []struct {
		name, cause string
		body        func(enc *spillEnc)
	}{
		{"edits", long, func(enc *spillEnc) { enc.uv(tooLong) }},
		{"more edits than the file has bytes for", "a count of 65536 with", func(enc *spillEnc) {
			enc.uv(1 << 16)
			zeros(enc, 1<<10)
		}},
		{"the name of an edit's tab", long, func(enc *spillEnc) {
			enc.uv(1)
			enc.uv(1)
			enc.uv(9)
			enc.uv(2)
			enc.uv(uint64(formula.DeleteRows))
			enc.uv(tooLong)
		}},
		{"the ids an edit took", "a list of 2147483648 ids", func(enc *spillEnc) {
			edit(enc)
			enc.uv(tooLong)
			enc.uv(1)
			enc.uv(tooLong)
		}},
		{"the rows a sort moved", long, func(enc *spillEnc) {
			edit(enc)
			enc.uv(0)
			enc.uv(0)
			enc.uv(0)
			enc.uv(0)
			enc.uv(tooLong)
		}},
		{"a deletion of more rows than the ids it took", "at 0 of 2 with 1 ids", func(enc *spillEnc) {
			enc.uv(1) // edits
			enc.uv(1)
			enc.uv(9)
			enc.uv(2)
			enc.uv(uint64(formula.DeleteRows))
			enc.str("D")
			enc.str("")
			enc.iv(0)
			enc.iv(2)
			enc.flag(true)
			enc.uv(1) // the ids it took
			enc.uv(5)
			enc.uv(1)
			for k := 0; k < 4; k++ { // the ids it brought, above, below, moved
				enc.uv(0)
			}
		}},
		{"a deletion of rows on the axis of columns", "at 0 of 1 with 1 ids", func(enc *spillEnc) {
			enc.uv(1) // edits
			enc.uv(1)
			enc.uv(9)
			enc.uv(2)
			enc.uv(uint64(formula.DeleteRows))
			enc.str("D")
			enc.str("")
			enc.iv(0)
			enc.iv(1)
			enc.flag(false) // columns
			enc.uv(1)
			enc.uv(5)
			enc.uv(1)
			for k := 0; k < 4; k++ {
				enc.uv(0)
			}
		}},
		{"more ids than the segment was written with", "more ids than", func(enc *spillEnc) {
			enc.uv(manySteps)
			for i := 0; i < manySteps; i++ {
				enc.uv(uint64(i + 1))
				enc.uv(9)
				enc.uv(2)
				enc.uv(uint64(formula.DeleteRows))
				enc.str("D")
				enc.str("")
				enc.iv(0)
				enc.iv(1)
				enc.flag(true)
				enc.uv(formula.MaxRows) // the ids it took
				enc.uv(1)
				enc.uv(formula.MaxRows)
				for k := 0; k < 4; k++ { // the ids it brought, above, below, moved
					enc.uv(0)
				}
			}
		}},
	}
	for _, c := range segments {
		t.Run("a segment of the edit log, "+c.name, func(t *testing.T) {
			sink := &logSink{}
			e := start(t, func(o *Options) { o.Log = slog.New(slog.NewTextHandler(sink, nil)) })
			e.in.mu.Lock()
			e.in.undoPool().entryMax = -1
			e.in.mu.Unlock()
			e.tinySegments()
			e.mustAgent(`{"ops":[{"op":"add_tab","name":"D"},{"op":"write","tab":"Sheet1","at":"A1","rows":[["=D!A5"]]}]}`)
			alice, bob := e.person("Alice"), e.person("Bob")
			alice.set("A1", "kept")
			bob.do(e.insertRowsOp("D", 1, 1))
			e.in.mu.RLock()
			dir := e.in.scratch.dir
			e.in.mu.RUnlock()
			names, err := filepath.Glob(filepath.Join(dir, "*.edits"))
			if err != nil || len(names) != 1 {
				t.Fatalf("the segments of the edit log are %v, %v; want one", names, err)
			}
			if err := os.WriteFile(names[0], checksummed(editMagic, c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			lost(t, e, alice, sink, c.cause)
			e.want("A1", "kept")
		})
	}
}
