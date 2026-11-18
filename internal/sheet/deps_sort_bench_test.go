package sheet

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// sortCase returns a loose list of n intervals: in random order, or all of
// one row but for nine whose first rows fall at the quarter points of the
// list, which makes the sort take the list for one in reverse order and
// reverse it whole first.
func sortCase(n int, reversed bool) parted[interval] {
	node := &fnode{gen: 1}
	r := rand.New(rand.NewPCG(1, 2))
	var l parted[interval]
	for range n {
		first := int32(r.IntN(1 << 20))
		if reversed {
			first = 0
		}
		l.push(interval{first, first + 1, node, 1})
	}
	if reversed {
		for k, at := range []int{n / 4, n / 4 * 2, n / 4 * 3} {
			for d := -1; d <= 1; d++ {
				first := int32(9 - 3*k - (d + 1))
				*l.at(at + d) = interval{first, first + 1, node, 1}
			}
		}
	}
	return l
}

// BenchmarkSortIntervals sorts the 8,331,000 ranges the column of a sheet
// can hold at its limits, in random order and in the order that the sort
// reverses whole first. It reports the longest time between two looks at
// the deadline beside the mean.
func BenchmarkSortIntervals(b *testing.B) {
	for _, c := range []struct {
		name     string
		reversed bool
	}{{"random", false}, {"reversed", true}} {
		b.Run(c.name, func(b *testing.B) {
			base := sortCase(mostIntervals, c.reversed)
			l := base.clone()
			var longest time.Duration
			for b.Loop() {
				b.StopTimer()
				for i := range base.len() {
					*l.at(i) = *base.at(i)
				}
				last := time.Now()
				budget := formula.NewBudget(func() bool {
					now := time.Now()
					longest = max(longest, now.Sub(last))
					last = now
					return false
				})
				b.StartTimer()
				if !sortIntervals(&l, budget) {
					b.Fatal("the sort stopped")
				}
				longest = max(longest, time.Since(last))
			}
			b.ReportMetric(float64(longest.Microseconds())/1000, "longest-ms")
		})
	}
}
