package sheet

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// BenchmarkSortRows measures the sort of one column inside the write lock:
// the comparisons and the reordering, without the commit. Each run sorts
// the same rows, since the sort is taken back after it.
//
//	numbers      20,000 numbers in a scattered order
//	short texts  20,000 texts of ten characters in a scattered order
//	long texts   2,000 texts of 3,900 equal characters and a number
func BenchmarkSortRows(b *testing.B) {
	scattered := func(i, n int) int { return i * 7919 % n }
	long := strings.Repeat("a", 3900)
	cases := []struct {
		name string
		rows int
		cell func(i, n int) string
	}{
		{"numbers", 20000, func(i, n int) string { return fmt.Sprint(scattered(i, n)) }},
		{"short texts", 20000, func(i, n int) string { return fmt.Sprintf(`"item-%05d"`, scattered(i, n)) }},
		{"long texts", 2000, func(i, n int) string { return fmt.Sprintf(`"%s%d"`, long, 10000+scattered(i, n)) }},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			e := start(b)
			if c.rows > newTabRows {
				e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":%d}]}`, c.rows-newTabRows))
			}
			var rows strings.Builder
			for i := 0; i < c.rows; i++ {
				if i > 0 {
					rows.WriteByte(',')
				}
				rows.WriteString("[" + c.cell(i, c.rows) + "]")
			}
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + rows.String() + `]}]}`)
			in := e.in
			in.mu.Lock()
			defer in.mu.Unlock()
			wb := in.wb
			t := wb.tab("Sheet1")
			positions := make([]int32, c.rows)
			for i := range positions {
				positions[i] = int32(i)
			}
			col := ColID(t.cols.ids[0])
			unsorted := slices.Clone(t.rows.ids)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				x := newTxn(wb, wb.seq+1, wb.seq, 0)
				in.begin(x)
				if err := x.sortRows(t, positions, col, false); err != nil {
					b.Fatal(err)
				}
				if slices.Equal(t.rows.ids, unsorted) {
					b.Fatal("the sort moved nothing")
				}
				wb.revert(&x.j)
				wb.settle()
			}
		})
	}
}
