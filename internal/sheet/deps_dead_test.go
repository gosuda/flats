package sheet

import (
	"runtime"
	"strings"
	"testing"
)

// TestDeadEntriesKeepNoValues writes 1,000 formulas that read one cell and
// give 131,068 bytes of text each, and clears them. The default run has a
// fifth of them. The index keeps their entries, dead, until a compaction,
// which so few do not call for, and a dead entry names the node of its
// formula. The node lets go of the value of its last pass, so the dead
// entries keep nothing of the text: a compaction that drops them lets go
// of little of what the calculations allocated.
//
// The memory profile records every allocation while the formulas are
// written, and the test counts only what was allocated inside a
// calculation (calculatedInUse). The rest of the heap, which other tests'
// goroutines and the runtime change meanwhile, is not in it.
func TestDeadEntriesKeepNoValues(t *testing.T) {
	slow(t, "calculates hundreds of long texts")
	n := sized(1000, 200)
	e := start(t)
	defer func(rate int) { runtime.MemProfileRate = rate }(runtime.MemProfileRate)
	runtime.MemProfileRate = 1
	writeColumn(e, "A", n, func(int) string { return `=REPT("😀",32767)&B1` })
	runtime.MemProfileRate = 0
	// The profile must see the texts for the test to see them kept.
	if made := calculatedInUse(); made < int64(n)*131068 {
		t.Fatalf("the profile has %d bytes of %d calculated texts in use", made, n)
	}
	e.mustAgent(jsonf(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["A1:A%d"]}]}`, n))
	e.want("A2", "")
	e.in.mu.RLock()
	dead := e.in.wb.deps.total - e.in.wb.deps.live
	e.in.mu.RUnlock()
	if dead != int64(n) {
		t.Fatalf("the index holds %d dead entries, want %d", dead, n)
	}
	held := calculatedInUse()
	e.in.mu.Lock()
	e.in.wb.deps.compact(nil)
	total := e.in.wb.deps.total
	e.in.mu.Unlock()
	if total != 0 {
		t.Fatalf("after the compaction the index holds %d entries", total)
	}
	kept := held - calculatedInUse()
	t.Logf("%d dead entries kept %d KiB of what the calculations allocated", dead, kept>>10)
	if most := int64(n) << 10; kept > most {
		t.Errorf("%d dead entries kept %d KiB of what the calculations allocated, more than %d KiB", n, kept>>10, most>>10)
	}
}

// calculatedInUse returns the bytes in use that the memory profile has
// from allocations inside a calculation of a formula (formula.Eval), once
// the profile holds every collection up to now. The profile has only the
// allocations it sampled, all of them while MemProfileRate is 1.
func calculatedInUse() int64 {
	// The frees of a collection reach the profile with the next one.
	for range 3 {
		runtime.GC()
	}
	var records []runtime.MemProfileRecord
	for {
		n, ok := runtime.MemProfile(records, true)
		if ok {
			records = records[:n]
			break
		}
		records = make([]runtime.MemProfileRecord, n+n/4+16)
	}
	var bytes int64
	for i := range records {
		if inCalculation(records[i].Stack()) {
			bytes += records[i].InUseBytes()
		}
	}
	return bytes
}

func inCalculation(stack []uintptr) bool {
	frames := runtime.CallersFrames(stack)
	for {
		f, more := frames.Next()
		if strings.HasSuffix(f.Function, "/formula.(*Formula).Eval") {
			return true
		}
		if !more {
			return false
		}
	}
}
