package sheet

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// longTexts returns an agent write of count distinct texts of size bytes
// each into one column from at.
func longTexts(at string, count, size int, salt string) string {
	var b strings.Builder
	b.WriteString(`{"ops":[{"op":"write","tab":"Sheet1","at":"` + at + `","rows":[`)
	for i := 0; i < count; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		seed := fmt.Sprintf("%s-%d ", salt, i)
		b.WriteString(`["` + strings.Repeat(seed, size/len(seed)+1)[:size] + `"]`)
	}
	b.WriteString(`]}]}`)
	return b.String()
}

// TestExpiredCapturesAreCollected keeps four paged reads of four states of
// a sheet whose texts are replaced between them, so that each capture is
// the only owner of the texts of its state. When the captures expire while
// a connection keeps the instance running, their records and those texts
// leave the heap, not only the budget.
func TestExpiredCapturesAreCollected(t *testing.T) {
	// Under the race detector the captures still expire and their charge
	// still goes; the heap is not measured.
	const cells, size = 100, 30000
	e := start(t)
	s := serve(e)
	open := s.dial("private", helloV1)
	open.next("welcome")
	base := captureBytes.Load()
	var cursors []string
	for state := 0; state < maxCaptures; state++ {
		e.mustAgent(longTexts("A1", cells, size, fmt.Sprint("state", state)))
		cursors = append(cursors, cursorOf(t, e.read(ReadQuery{Tab: "Sheet1", Limit: 10})))
	}
	// The workbook lets go of the texts of the last state as well.
	e.mustAgent(longTexts("A1", cells, 8, "short"))
	charged := captureBytes.Load() - base
	if want := int64(maxCaptures * cells * size); charged < want {
		t.Fatalf("four kept captures count %d bytes, less than their %d bytes of text", charged, want)
	}
	held := liveHeap()

	e.clock.Add(2 * captureTTL)
	e.tick()
	if got := captureBytes.Load() - base; got != 0 {
		t.Fatalf("expired captures leave %d bytes charged", got)
	}
	for _, cursor := range cursors {
		_, err := e.in.Read(context.Background(), ReadQuery{Cursor: cursor})
		e.wantRefusal(err, "conflict", "read_expired")
	}
	if e.in.hub.count() != 1 {
		t.Fatal("the connection that keeps the instance running is gone")
	}
	freed := held - liveHeap()
	// The texts alone are 12 MB; the records and the rest of the test's
	// heap are small beside them.
	if want := int64(maxCaptures * cells * size); measures && freed < want*8/10 {
		t.Fatalf("the heap gave back %d bytes when captures of %d bytes of text expired; the budget counts none of them any more", freed, want)
	}
	runtime.KeepAlive(open)
}

// TestEvictedDeltaIsCollected appends a few small deltas, one valid delta
// larger than the whole bound of the delta log, and a small one. The large
// one is the newest entry first and stays; the next entry pushes it out,
// and then the log holds its bytes no longer, in its count or on the heap.
func TestEvictedDeltaIsCollected(t *testing.T) {
	// Under the race detector the delta still leaves the log and its
	// count; the heap is not measured.
	const cells, copies = 64, 9
	e := start(t)
	e.mustAgent(longTexts("A1", cells, 32767, "text"))
	for i := 0; i < 3; i++ {
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"L%d","rows":[[%d]]}]}`, i+1, i))
	}
	// A copy of the texts into nine columns is one delta of about 19 MB.
	alice := e.person("Alice")
	tab := e.tabID("Sheet1")
	op := jsonf(`{"op":"copy","from":{"tab":%d,"rows":%s,"cols":%s},"to":{"tab":%d,"rows":%s,"cols":%s},"mode":"all","cut":false}`,
		tab, e.rowsOf("Sheet1", 1, cells), e.colsOf("Sheet1", 1, 1), tab, e.rowsOf("Sheet1", 1, cells), e.colsOf("Sheet1", 2, copies))
	if ack := alice.do(op); status(ack) != "applied" {
		t.Fatalf("the copy: %s", status(ack))
	}
	large := cells * copies * 32767
	source, _ := logRetained(e.in.hub)
	e.in.hub.mu.Lock()
	entries := len(e.in.hub.log)
	e.in.hub.mu.Unlock()
	if source < large || source <= deltaLogBytes || entries != 1 {
		t.Fatalf("after the copy the log holds %d entries of %d bytes; the test needs one entry above the bound of %d", entries, source, deltaLogBytes)
	}
	held := liveHeap()

	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"L9","rows":[[9]]}]}`)
	source, _ = logRetained(e.in.hub)
	e.in.hub.mu.Lock()
	entries, counted := len(e.in.hub.log), e.in.hub.bytes
	e.in.hub.mu.Unlock()
	if entries != 1 || source > 4096 || counted != source {
		t.Fatalf("after the next delta the log holds %d entries of %d bytes and counts %d", entries, source, counted)
	}
	if freed := held - liveHeap(); measures && freed < int64(large)*8/10 {
		t.Fatalf("the heap gave back %d bytes when a delta of %d bytes left the log", freed, large)
	}
}

// TestDroppedStateIsCollected removes parts of a sheet that hold long
// texts, each time through a list or a cache that is compacted in place,
// and checks that the heap gives the texts back: no slice keeps a pointer
// to what it dropped in the memory behind its length.
func TestDroppedStateIsCollected(t *testing.T) {
	if !measures {
		t.Skip("every assertion measures the heap; not run under the race detector")
	}
	const cells, size = 100, 30000
	text := int64(cells * size)
	check := func(t *testing.T, what string, held int64) {
		t.Helper()
		if freed := held - liveHeap(); freed < text*8/10 {
			t.Fatalf("the heap gave back %d bytes when %s; it held %d bytes of text", freed, what, text)
		}
	}
	t.Run("the last tab is deleted", func(t *testing.T) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"Wide"}]}`)
		e.mustAgent(strings.Replace(longTexts("A1", cells, size, "tab"), `"Sheet1"`, `"Wide"`, 1))
		held := liveHeap()
		e.mustAgent(`{"ops":[{"op":"delete_tab","tab":"Wide"}]}`)
		check(t, "the tab was deleted", held)
	})
	t.Run("the last rows are deleted", func(t *testing.T) {
		e := start(t)
		e.mustAgent(longTexts("A901", cells, size, "rows"))
		held := liveHeap()
		e.mustAgent(`{"ops":[{"op":"delete_rows","tab":"Sheet1","at":901,"count":100}]}`)
		check(t, "the rows were deleted", held)
	})
	t.Run("a revision workbook expires", func(t *testing.T) {
		e := start(t)
		e.mustAgent(longTexts("A1", cells, size, "revision"))
		rev := e.b.importAll(t)
		e.mustAgent(longTexts("A1", cells, 8, "short"))
		// The blobs of the test's host stay; the workbook built from them
		// is what expires.
		empty := liveHeap()
		if out := e.readRevision(rev, ReadQuery{Tab: "Sheet1", Range: "A1"}); firstCell(out) == nil {
			t.Fatalf("revision %d has no A1", rev)
		}
		e.in.captures.clear()
		held := liveHeap()
		if held-empty < text*8/10 {
			t.Fatalf("a cached revision workbook of %d bytes of text takes %d bytes", text, held-empty)
		}
		e.clock.Add(2 * captureTTL)
		e.tick()
		check(t, "the revision workbook expired", held)
	})
}
