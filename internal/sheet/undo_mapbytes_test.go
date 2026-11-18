package sheet

import (
	"runtime"
	"strconv"
	"testing"
)

// mapSizes are numbers of entries around every size at which a map's
// tables double, where they are emptiest, and a few between.
func mapSizes() []int {
	out := []int{0, 1, 2, 7, 8, 9, 100, 1000, 3000}
	for c := 16; c <= 1<<18; c *= 2 {
		out = append(out, c*7/8, c*7/8+1)
	}
	return out
}

// heapOf returns what the heap grows by while one result of build is
// alive, and the last result: the least of three readings, since the
// runtime and other goroutines can allocate between two of them. Nothing
// but the reading keeps a result alive.
func heapOf(build func() any) (int64, any) {
	least := int64(-1)
	var kept any
	for range 3 {
		kept = nil
		var m runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m)
		before := m.HeapAlloc
		kept = build()
		runtime.GC()
		runtime.ReadMemStats(&m)
		if grown := int64(m.HeapAlloc) - int64(before); least < 0 || grown < least {
			least = grown
		}
	}
	return least, kept
}

func checkMapBytes[K comparable, V any](t *testing.T, name string, key func(i int) K) {
	t.Helper()
	for _, n := range mapSizes() {
		for _, made := range []bool{true, false} {
			grown, kept := heapOf(func() any {
				m := map[K]V{}
				if made {
					m = make(map[K]V, n)
				}
				var v V
				for i := 0; i < n; i++ {
					m[key(i)] = v
				}
				return m
			})
			if got := mapBytes(kept.(map[K]V)); grown > got {
				t.Errorf("a map of %s with %d entries, made for them %v, takes %d bytes of the heap and counts %d", name, n, made, grown, got)
			}
		}
	}
}

// TestMapBytesCoverTheHeap holds mapBytes against the heap that maps of
// every type a saved payload keeps take, made for their entries or grown
// to them.
func TestMapBytesCoverTheHeap(t *testing.T) {
	if !measures {
		t.Skip("the race detector changes what the heap holds")
	}
	ids := func(i int) uint64 { return uint64(i)*7 + 1 }
	checkMapBytes[uint64, int32](t, "positions", ids)
	checkMapBytes[ColID, width](t, "widths", func(i int) ColID { return ColID(ids(i)) })
	checkMapBytes[RowID, *rowData](t, "rows", func(i int) RowID { return RowID(ids(i)) })
	checkMapBytes[uint64, writeMark](t, "marks", ids)
	checkMapBytes[RowID, sortStamp](t, "sort stamps", func(i int) RowID { return RowID(ids(i)) })
	checkMapBytes[uint32, styleState](t, "styles", func(i int) uint32 { return uint32(i) + 1 })
	checkMapBytes[TabID, struct{}](t, "tabs an entry lies in", func(i int) TabID { return TabID(ids(i)) })
	checkMapBytes[TabID, uint64](t, "tabs a payload waits for", func(i int) TabID { return TabID(ids(i)) })
	// The texts of names are counted on their own, so the keys are made
	// before the heap is read.
	names := make([]string, 1<<18)
	for i := range names {
		names[i] = strconv.Itoa(i)
	}
	checkMapBytes[string, struct{}](t, "names", func(i int) string { return names[i] })
}

// TestNoteIndexCoversTheHeap holds what a session counts for its stand-in
// notes against the heap they take with its index: sets that join the
// index, each with the place it counts there, and then leave it until a
// part of them is left, as they leave when the entries they name go. The
// index is made again once it holds less than half of the most it held
// (removeNotes). The session counts the index's smallest table with its
// notes while it holds any.
func TestNoteIndexCoversTheHeap(t *testing.T) {
	if !measures {
		t.Skip("the race detector changes what the heap holds")
	}
	for _, n := range mapSizes() {
		for _, kept := range []int{n, n/2 + 1, n / 2, n / 5, 1} {
			if kept > n || kept == 0 {
				continue
			}
			var counted int64
			grown, _ := heapOf(func() any {
				u := &undoState{}
				sets := make([]*noteSet, n)
				for i := range sets {
					sets[i] = &noteSet{owner: u, stamp: uint64(i)*3 + 1, bytes: noteSetBytes}
					u.addNotes(sets[i])
				}
				for _, s := range sets[kept:] {
					u.removeNotes(s)
				}
				counted = u.noteBytes
				return u
			})
			if want := counted; grown > want {
				t.Errorf("an index that held %d sets and keeps %d takes %d bytes of the heap; the session counts %d for them", n, kept, grown, counted)
			}
		}
	}
}
