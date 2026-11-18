package sheet

import (
	"runtime"
	"testing"
	"time"
)

// The longest lists the index can hold at the sheet's limits. A single
// reference takes three characters of a formula of 8,192 ("+A1"), so
// 6,103 formulas of the 50,000,000 characters of typed text name one cell
// 16,662,000 times, under the 20,000,000 reads of a sheet. A range of one
// column takes six ("+A1:A2", or "+T!A:A" over a tab of one row), so one
// column holds 8,331,000 ranges.
const (
	mostSingles   = 6103 * 2730
	mostIntervals = 6103 * 1365
)

// lastGrowth returns the length at which a slice of E that append grows
// one entry at a time grows for the last time on its way to n entries.
func lastGrowth[E any](n int) int {
	var list []E
	var zero E
	last := 0
	for range n {
		if len(list) == cap(list) {
			last = len(list)
		}
		list = append(list, zero)
	}
	return last
}

// benchmarkPush adds an entry to a list of n entries, b.N times, each time
// to a list as push left it, and reports the longest push beside the mean.
// A push has no check point, so the longest one bounds it.
func benchmarkPush[E any](b *testing.B, n int, e E) {
	var base parted[E]
	for range n {
		base.push(e)
	}
	var longest time.Duration
	for b.Loop() {
		b.StopTimer()
		l := base
		// Each push makes the room it needs anew, as the first push past
		// the end of a part does.
		l.head = l.head[:len(l.head):len(l.head)]
		l.rest = l.rest[:len(l.rest):len(l.rest)]
		runtime.GC()
		b.StartTimer()
		began := time.Now()
		l.push(e)
		longest = max(longest, time.Since(began))
	}
	b.ReportMetric(float64(longest.Microseconds())/1000, "longest-ms")
}

// BenchmarkListGrowsAtLimits adds an entry to the longest list of single
// references and to the longest list of the ranges of a column, where the
// list grows: at the end of the first part, which then grows for the last
// time, and at the end of a later part, where the next part is made. A
// list never grows by more than that (technical design, section 10.1).
func BenchmarkListGrowsAtLimits(b *testing.B) {
	node := &fnode{}
	b.Run("singles first part", func(b *testing.B) { benchmarkPush(b, lastGrowth[depRef](partLen), depRef{node, 1}) })
	b.Run("singles later part", func(b *testing.B) { benchmarkPush(b, mostSingles/partLen*partLen, depRef{node, 1}) })
	b.Run("ranges first part", func(b *testing.B) { benchmarkPush(b, lastGrowth[interval](partLen), interval{0, 1, node, 1}) })
	b.Run("ranges later part", func(b *testing.B) { benchmarkPush(b, mostIntervals/partLen*partLen, interval{0, 1, node, 1}) })
}
