package sheet

import "testing"

// BenchmarkFormatRange measures a format of all nine properties over
// 500,000 cells, and the undo of such a format, inside the write lock: the
// work that the deadline of an action covers. Building the undo entry,
// staging the rows in sheet.db and the commit come after it and are not
// measured. Each run starts from the same sheet, since the work is taken
// back after it.
//
//	format  one format operation over the range
//	undo    the undo of a committed format, with its payload read from the
//	        scratch file
func BenchmarkFormatRange(b *testing.B) {
	const rows, cols = 5000, 100
	formats := [2]string{
		`{"bold":true,"italic":true,"underline":true,"strikethrough":true,"text_color":"blue-600","fill":"teal-200","align":"right","wrap":true,"number":{"kind":"percent","decimals":1}}`,
		`{"bold":null,"italic":null,"underline":null,"strikethrough":null,"text_color":"gray-100","fill":"red-100","align":"center","wrap":null,"number":{"kind":"number","decimals":1,"thousands":false}}`,
	}
	e := start(b)
	fillNumbers(e, "Sheet1", rows, cols)
	p := e.person("Alice")
	if ack := p.do(e.formatRange("Sheet1", 1, rows, 1, cols, formats[0])); status(ack) != "applied" {
		b.Fatal(status(ack))
	}
	var ops []personOp
	if err := strictJSON([]byte("["+e.formatRange("Sheet1", 1, rows, 1, cols, formats[1])+"]"), &ops); err != nil {
		b.Fatal(err)
	}
	in := e.in
	in.mu.Lock()
	defer in.mu.Unlock()
	wb := in.wb
	run := func(b *testing.B, work func(x *txn)) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			x := newTxn(wb, wb.seq+1, wb.seq, p.s.ord)
			in.begin(x)
			work(x)
			b.StopTimer()
			wb.revert(&x.j)
			wb.settle()
			b.StartTimer()
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/(rows*cols), "ns/cell")
	}
	b.Run("format", func(b *testing.B) {
		run(b, func(x *txn) {
			if applied, _, err := x.applyPerson(&ops[0]); err != nil || applied != rows*cols {
				b.Fatalf("the format applied to %d cells: %v", applied, err)
			}
		})
	})
	b.Run("undo", func(b *testing.B) {
		u := &p.s.undo
		entry := u.undo[len(u.undo)-1]
		if entry.file == nil {
			b.Fatal("the payload of the format is in memory")
		}
		run(b, func(x *txn) {
			if applied, skipped, _, err := x.applyUndo(u, entry); err != nil || applied != 9*rows*cols || skipped != 0 {
				b.Fatalf("the undo restored %d groups and skipped %d: %v", applied, skipped, err)
			}
		})
	})
}

// BenchmarkClearRange measures a clear of 500,000 numbers, and the undo of
// such a clear after the tick collected the blank records it left, inside
// the write lock: the work that the deadline of an action covers, as in
// BenchmarkFormatRange.
//
//	clear  one clear operation over the range
//	undo   the undo of a committed clear, with its payload read from the
//	       scratch file and the stamps of its cells from the store of
//	       collected blank records
func BenchmarkClearRange(b *testing.B) {
	const rows, cols = 5000, 100
	e := start(b)
	fillNumbers(e, "Sheet1", rows, cols)
	p := e.person("Alice")
	var ops []personOp
	if err := strictJSON([]byte("["+e.clearOp("Sheet1", 1, rows, 1, cols)+"]"), &ops); err != nil {
		b.Fatal(err)
	}
	if ack := p.do(e.clearOp("Sheet1", 1, rows, 1, cols)); status(ack) != "applied" {
		b.Fatal(status(ack))
	}
	e.tick()
	if left := e.blanks(); left > rows*cols/2 {
		b.Fatalf("%d of %d blank records are left after the tick", left, rows*cols)
	}
	in := e.in
	in.mu.Lock()
	defer in.mu.Unlock()
	wb := in.wb
	u := &p.s.undo
	entry := u.undo[len(u.undo)-1]
	if entry.file == nil {
		b.Fatal("the payload of the clear is in memory")
	}
	run := func(b *testing.B, work func(x *txn)) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			x := newTxn(wb, wb.seq+1, wb.seq, p.s.ord)
			in.begin(x)
			work(x)
			b.StopTimer()
			wb.revert(&x.j)
			wb.settle()
			b.StartTimer()
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/(rows*cols), "ns/cell")
	}
	b.Run("clear", func(b *testing.B) {
		// The cells the tick collected are filled again first, so that the
		// clear finds what it found before the tick.
		fill := newTxn(wb, wb.seq+1, wb.seq, p.s.ord)
		in.begin(fill)
		if applied, skipped, _, err := fill.applyUndo(u, entry); err != nil || applied != rows*cols || skipped != 0 {
			b.Fatalf("the undo restored %d cells and skipped %d: %v", applied, skipped, err)
		}
		defer func() {
			wb.revert(&fill.j)
			wb.settle()
		}()
		run(b, func(x *txn) {
			if applied, _, err := x.applyPerson(&ops[0]); err != nil || applied != rows*cols {
				b.Fatalf("the clear applied to %d cells: %v", applied, err)
			}
		})
	})
	b.Run("undo", func(b *testing.B) {
		run(b, func(x *txn) {
			if applied, skipped, _, err := x.applyUndo(u, entry); err != nil || applied != rows*cols || skipped != 0 {
				b.Fatalf("the undo restored %d cells and skipped %d: %v", applied, skipped, err)
			}
		})
	})
}
