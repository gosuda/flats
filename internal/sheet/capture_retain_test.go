package sheet

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// wireLen returns the length and the capacity of the workbook's format list.
func (e *env) wireLen() (n, c int) {
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	return len(e.in.wb.wire.list), cap(e.in.wb.wire.list)
}

// steadyHeap returns liveHeap once two readings 20 milliseconds apart
// agree within 4 KiB, so that the work a write leaves running in the
// background does not count as what a capture holds.
func steadyHeap() int64 {
	last := liveHeap()
	for range 50 {
		time.Sleep(20 * time.Millisecond)
		now := liveHeap()
		if now-last < 4<<10 && last-now < 4<<10 {
			return now
		}
		last = now
	}
	return last
}

// symbolFormats gives count cells of Sheet1, from cell index from on, a
// currency format each with a symbol of its own: three letters of four
// bytes, twelve bytes that each format's decoded text holds in an
// allocation of sixteen.
func (e *env) symbolFormats(from, count int) {
	e.t.Helper()
	for start := from; start < from+count; start += 3000 {
		ops := make([]string, 0, 3000)
		for i := start; i < min(start+3000, from+count); i++ {
			symbol := string([]rune{rune(0x10400 + i%80), rune(0x10400 + i/80%80), rune(0x20000 + i/6400)})
			cell := formula.Pos{Row: int32(i / newTabCols), Col: int32(i % newTabCols)}
			ops = append(ops, fmt.Sprintf(`{"op":"format","tab":"Sheet1","ranges":[%q],"format":{"number":{"kind":"currency","symbol":%q,"decimals":2}}}`, cell.A1(), symbol))
		}
		e.mustAgent(`{"ops":[` + strings.Join(ops, ",") + `]}`)
	}
}

// checkCaptureRetention takes a capture of two small cells of the tab
// Other while the format list of the workbook holds at least formats
// entries, right after the list grew. It then fills the rest of the list's
// capacity, clears every formatted cell so that the workbook rebuilds its
// list, and compares what the heap gives back when the capture lets go
// with what the capture is charged.
func checkCaptureRetention(t *testing.T, take func(e *env) (charge func() int64, release func())) {
	const formats = 40000
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Other"},{"op":"write","tab":"Other","at":"A1","rows":[[1,2]]},` +
		fmt.Sprintf(`{"op":"insert_rows","tab":"Sheet1","at":1,"count":%d}]}`, 2*formats/newTabCols))
	done := 0
	for {
		n, c := e.wireLen()
		if n < c {
			e.symbolFormats(done, c-n)
			done += c - n
			continue
		}
		if n >= formats {
			e.symbolFormats(done, 1)
			done++
			break
		}
		e.symbolFormats(done, 1)
		done++
	}
	before, grown := e.wireLen()
	if grown <= before {
		t.Fatalf("the format list did not grow: %d formats in room for %d", before, grown)
	}
	charge, release := take(e)
	defer release()
	n, c := e.wireLen()
	e.symbolFormats(done, c-n)
	if n, c2 := e.wireLen(); n != c || c2 != c {
		t.Fatalf("the format list holds %d formats in room for %d, want %d in %d", n, c2, c, c)
	}
	e.mustAgent(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["A1:Z99999"],"formats":true}]}`)
	if n, _ := e.wireLen(); n > 2 {
		t.Fatalf("the workbook kept %d formats after its cells lost theirs", n)
	}
	counted := charge()
	held := steadyHeap()
	release()
	freed := held - steadyHeap()
	// The slack is for what other goroutines allocate between the two
	// readings; the old list alone takes over 2 MiB.
	if freed > counted+64<<10 {
		t.Fatalf("a capture of two cells keeps %d bytes of the heap and counts %d, with a format list of %d entries", freed, counted, c)
	}
	t.Logf("the capture gave back %d bytes and counted %d", freed, counted)
}

// TestPagedCaptureCountsItsFormats keeps a paged read of two small cells
// while another tab adds formats into the room the format list has and
// then lets go of all of them.
func TestPagedCaptureCountsItsFormats(t *testing.T) {
	checkCaptureRetention(t, func(e *env) (func() int64, func()) {
		e.read(ReadQuery{Tab: "Other", Range: "A1:B1", Limit: 1})
		e.in.captures.mu.Lock()
		defer e.in.captures.mu.Unlock()
		if len(e.in.captures.list) != 1 {
			t.Fatalf("%d captures are kept", len(e.in.captures.list))
		}
		c := e.in.captures.list[0]
		if len(c.formats) != 1 {
			t.Errorf("a capture of two unformatted cells holds %d formats", len(c.formats))
		}
		return func() int64 { return c.bytes }, e.in.captures.clear
	})
}

// TestSnapshotCaptureCountsItsFormats does the same with the capture a
// browser's snapshot of the tab is encoded from, which holds the whole
// format list.
func TestSnapshotCaptureCountsItsFormats(t *testing.T) {
	checkCaptureRetention(t, func(e *env) (func() int64, func()) {
		tab := e.tabID("Other")
		e.in.mu.RLock()
		c, _, err := e.in.wb.captureTab(e.in.wb.byID[tab])
		list := e.in.wb.wire.list
		e.in.mu.RUnlock()
		if err != nil {
			t.Fatal(err)
		}
		if len(c.formats) != len(list) || cap(c.formats) != len(c.formats) || unsafe.SliceData(c.formats) == unsafe.SliceData(list) {
			t.Errorf("a snapshot capture holds %d formats in room for %d of a list of %d", len(c.formats), cap(c.formats), len(list))
		}
		return func() int64 { return c.bytes }, c.release
	})
}

// TestCaptureRecordBytes checks that capFixedBytes covers a capture's own
// record and the texts a read gives it: a cursor of 32 hexadecimal digits
// and a state text under 64 bytes.
func TestCaptureRecordBytes(t *testing.T) {
	record := formula.HeapBytes(unsafe.Sizeof(capture{}), true)
	if texts := formula.TextBytes(32) + formula.TextBytes(64); record+texts > capFixedBytes {
		t.Fatalf("a capture takes %d bytes and its texts %d; it counts %d", record, texts, capFixedBytes)
	}
}

// TestCaptureKindsCountTheirHeap keeps many captures of each kind of read
// alive at once, then lets go of them, and compares what the heap gives
// back with what they were charged. The tab holds a cell in its first and
// last columns, so a read with widths keeps a width for every column.
func TestCaptureKindsCountTheirHeap(t *testing.T) {
	e := start(t)
	name := strings.Repeat("\U00010400", maxTabName)
	e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"add_tab","name":%q},{"op":"insert_cols","tab":%q,"at":"Z","count":%d},`+
		`{"op":"write","tab":%q,"at":"A1","rows":[["a text of its own"]]},{"op":"write","tab":%q,"at":%q,"rows":[[2]]},`+
		`{"op":"format","tab":%q,"ranges":["A1"],"format":{"bold":true,"number":{"kind":"currency","symbol":%q,"decimals":2}}}]}`,
		name, name, formula.MaxCols-newTabCols, name, name, formula.Pos{Col: formula.MaxCols - 1}.A1(), name, "\U00010401\U00010402\U00010403"))
	tab := e.in.wb.byID[e.tabID(name)]
	const kept = 64
	kinds := []struct {
		name string
		take func() (*capture, any, error)
	}{
		{"read with shown, formats and widths", func() (*capture, any, error) {
			c, err := e.in.wb.capture(ReadQuery{Tab: name, Include: []string{"shown", "formats", "widths"}})
			return c, nil, err
		}},
		{"CSV read", func() (*capture, any, error) {
			c, err := e.in.wb.capture(ReadQuery{Tab: name, Format: "csv"})
			if err == nil {
				c.fieldBuf()
			}
			return c, nil, err
		}},
		{"snapshot", func() (*capture, any, error) {
			c, head, err := e.in.wb.captureTab(tab)
			if err != nil {
				return nil, nil, err
			}
			enc, err := newSnapEncoder(c, head)
			return c, enc, err
		}},
	}
	for _, kind := range kinds {
		captures := make([]*capture, 0, kept)
		parts := make([]any, 0, kept)
		e.in.mu.RLock()
		for range kept {
			c, part, err := kind.take()
			if err != nil {
				e.in.mu.RUnlock()
				t.Fatalf("%s: %v", kind.name, err)
			}
			captures, parts = append(captures, c), append(parts, part)
		}
		e.in.mu.RUnlock()
		var counted int64
		for _, c := range captures {
			counted += c.bytes
		}
		held := steadyHeap()
		for _, c := range captures {
			c.release()
		}
		clear(captures)
		clear(parts)
		freed := held - steadyHeap()
		if freed > counted+64<<10 {
			t.Errorf("%d captures of a %s keep %d bytes of the heap and count %d", kept, kind.name, freed, counted)
		}
		t.Logf("%s: %d captures gave back %d bytes and counted %d", kind.name, kept, freed, counted)
	}
}
