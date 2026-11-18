package formula

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// keptBy returns the bytes of the heap that the objects run allocated
// keep alive after it returned, as the allocator takes them. It records
// every allocation of run in the memory profile and counts only run's
// own. A reading of the whole heap also has what the runtime allocates
// for itself meanwhile, for the collections that run's garbage starts and
// for its goroutines, timers and threads: kilobytes more or less, which
// over a few hundred formulas is several bytes each.
func keptBy(run func()) int64 {
	defer func(rate int) { runtime.MemProfileRate = rate }(runtime.MemProfileRate)
	before := profiledBy()
	runtime.MemProfileRate = 1
	inProfile(run)
	runtime.MemProfileRate = 0
	return profiledBy() - before
}

// inProfile runs run under a frame of its own, which marks the stacks of
// what run allocates in the memory profile.
//
//go:noinline
func inProfile(run func()) { run() }

// profiledBy returns the bytes in use that the memory profile has from
// stacks through inProfile, once the profile holds every collection up
// to now.
func profiledBy() int64 {
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
		if fromRun(records[i].Stack()) {
			bytes += records[i].InUseBytes()
		}
	}
	return bytes
}

// fromRun reports whether a stack of the memory profile is one of an
// allocation that inProfile's run made: one that leads out of the
// allocator into run's code, through nothing else of the runtime, and
// down to inProfile. A recursive parse can be deeper than the profile
// keeps of a stack; such a stack is cut off at its outer end, and it is
// run's when its inner frames are run's code and not the runtime's.
func fromRun(stack []uintptr) bool {
	frames := runtime.CallersFrames(stack)
	inAllocator := true
	for {
		f, more := frames.Next()
		name := f.Function
		switch {
		case strings.HasSuffix(name, "formula.inProfile"):
			return true
		case inAllocator && allocatorFrame(name):
		case strings.HasPrefix(name, "runtime."):
			// The runtime allocating for itself, such as the
			// collection a malloc starts.
			return false
		default:
			inAllocator = false
		}
		if !more {
			return len(stack) >= profileDepth && !inAllocator
		}
	}
}

// profileDepth is the most frames runtime.MemProfile gives of a stack.
var profileDepth = len(runtime.MemProfileRecord{}.Stack0)

// allocatorFrame reports whether a frame is of the allocator that the
// code it serves calls: new, make, append, a string conversion or join.
func allocatorFrame(name string) bool {
	for _, prefix := range []string{"runtime.mallocgc", "runtime.newobject", "runtime.makeslice", "runtime.growslice",
		"runtime.makemap", "runtime.rawstring", "runtime.rawbyteslice", "runtime.slicebytetostring",
		"runtime.concatstring", "runtime.stringtoslicebyte", "runtime.newarray", "runtime.mapassign"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return name == "internal/stringslite.Clone" || name == "strings.Clone" || strings.HasPrefix(name, "internal/runtime/maps.")
}

// sizeShapes are formulas of every kind the parser builds, each near the
// largest it accepts where that is the costly case.
func sizeShapes() []struct {
	name, text string
} {
	repeat := func(n int, f func(i int) string, sep string) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = f(i)
		}
		return strings.Join(parts, sep)
	}
	return []struct{ name, text string }{
		{"a number", "=1"},
		{"one reference", "=A1"},
		{"dense arithmetic", "=1" + strings.Repeat("+0", 4000)},
		{"products and powers", "=2" + strings.Repeat("*3^1", 2000)},
		{"signs and percents", "=" + strings.Repeat("-1%+", 1500) + "1"},
		{"many references", "=" + repeat(1500, func(i int) string { return fmt.Sprintf("B%d", i+1) }, "+")},
		{"fixed references", "=" + repeat(900, func(i int) string { return fmt.Sprintf("$C$%d", i+1) }, "+")},
		{"references to another tab", "=" + repeat(400, func(i int) string { return fmt.Sprintf("'Other tab'!D%d", i+1) }, "+")},
		{"ranges in one call", "=SUM(" + repeat(800, func(i int) string { return fmt.Sprintf("A%d:B%d", i+1, i+2) }, ",") + ")"},
		{"whole columns and rows", "=SUM(" + repeat(600, func(i int) string { return fmt.Sprintf("A:B,%d:%d", i+1, i+1) }, ",") + ")"},
		{"many calls", "=" + repeat(700, func(i int) string { return fmt.Sprintf("ABS(%d)", i) }, "+")},
		{"empty arguments", "=CHOOSE(1" + strings.Repeat(",", 2000) + ")"},
		{"nested brackets", "=" + strings.Repeat("(", 64) + "1" + strings.Repeat(")", 64)},
		{"nested calls", "=" + strings.Repeat("ABS(", 64) + "1" + strings.Repeat(")", 64)},
		{"a long text", `="` + strings.Repeat("a", 8000) + `"`},
		{"texts with quotation marks", "=" + repeat(400, func(int) string { return `"a""b"` }, "&")},
		{"names, errors and booleans", "=" + repeat(1000, func(i int) string { return []string{"TRUE", "#N/A", "x", "FALSE"}[i%4] }, "+")},
		{"comparisons and joins", "=" + repeat(1000, func(i int) string { return fmt.Sprint(i) }, "&") + "=" + "1"},
		{"a syntax error", "=1" + strings.Repeat("+0", 3000) + "+"},
		{"unreadable", "=1" + strings.Repeat("+0", 3000) + "+{"},
	}
}

// TestSizeCoversTheHeap parses copies of each shape and compares Size
// with what the heap keeps for them. Each copy is parsed from a text of
// its own, so a formula that kept a part of its typed text would show it.
// Size must cover the heap and stay close to it.
func TestSizeCoversTheHeap(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector changes what the allocator takes")
	}
	if testing.Short() {
		t.Skip("parses hundreds of formulas of thousands of nodes")
	}
	for _, c := range sizeShapes() {
		t.Run(c.name, func(t *testing.T) {
			check := func(t *testing.T, kind string, build func() *Formula) {
				t.Helper()
				const copies = 8
				kept := make([]*Formula, copies)
				total := keptBy(func() {
					for i := range kept {
						kept[i] = build()
					}
				})
				size, measured := kept[0].Size(), total/copies
				runtime.KeepAlive(kept)
				t.Logf("%s: %d bytes of text, Size %d, the heap keeps %d", kind, len(c.text), size, measured)
				if size*copies < total {
					t.Errorf("%s: Size is %d, the heap keeps %d", kind, size, measured)
				}
				if size > measured+measured/8+64 {
					t.Errorf("%s: Size is %d, far above the %d the heap keeps", kind, size, measured)
				}
			}
			at := Pos{Row: 7, Col: 3}
			parsed, err := Parse(c.text, at, Limits{})
			if err != nil {
				t.Fatal(err)
			}
			check(t, "typed", func() *Formula {
				f, err := Parse(strings.Clone(c.text), at, Limits{})
				if err != nil {
					t.Fatal(err)
				}
				return f
			})
			if parsed.Unreadable() {
				return
			}
			check(t, "stored", func() *Formula {
				f, err := ParseStored(strings.Clone(parsed.Stored()))
				if err != nil {
					t.Fatal(err)
				}
				return f
			})
			check(t, "text form", func() *Formula {
				f, err := ParseStored(strings.Clone(parsed.Stored()))
				if err != nil {
					t.Fatal(err)
				}
				return f.TextForm()
			})
			if parsed.NamesTab() {
				check(t, "renamed", func() *Formula {
					f, _, _ := parsed.Apply("Sheet1", at, Transform{Kind: RenameTab, Tab: "Other tab", Name: "Renamed tab"})
					return f
				})
			}
		})
	}
}

// Size is worked out when a formula is parsed, so asking for it costs
// nothing. A text form shows what its formula shows and has a size of its
// own.
func TestSizeIsKept(t *testing.T) {
	f, err := Parse("=1"+strings.Repeat("+0", 4000), Pos{}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if allocs := testing.AllocsPerRun(100, func() { _ = f.Size() }); allocs != 0 {
		t.Fatalf("Size allocates %v times", allocs)
	}
	if form := f.TextForm(); form.Size() >= f.Size()/20 {
		t.Fatalf("the text form of a long sum takes %d bytes of %d", form.Size(), f.Size())
	}
	shapes := append(sizeShapes(), struct{ name, text string }{"a date", "=DATE(2026,1,TODAY())"})
	for _, c := range shapes {
		for _, at := range []Pos{{}, {Row: 7, Col: 3}, {Row: 999_999, Col: 701}} {
			f, err := Parse(c.text, Pos{Row: 7, Col: 3}, Limits{})
			if err != nil {
				t.Fatal(err)
			}
			form := f.TextForm()
			if form.Size() != f.TextFormSize() || form.TextForm() != form {
				t.Fatalf("%s: the text form takes %d bytes, TextFormSize says %d", c.name, form.Size(), f.TextFormSize())
			}
			if form.Text(at) != f.Text(at) || form.TextLen(at) != f.TextLen(at) || form.Stored() != f.Stored() ||
				form.Unreadable() != f.Unreadable() || form.DateResult() != f.DateResult() || form.ReadsToday() != f.ReadsToday() {
				t.Fatalf("%s at %v: the text form shows %.40q, %d code points, the formula %.40q, %d", c.name, at,
					form.Text(at), form.TextLen(at), f.Text(at), f.TextLen(at))
			}
		}
	}
}

// allocatedBy returns the bytes that run allocates, as the allocator
// takes them, garbage included, from the memory profile as keptBy does.
func allocatedBy(run func()) int64 {
	defer func(rate int) { runtime.MemProfileRate = rate }(runtime.MemProfileRate)
	before := profiledAllocs()
	runtime.MemProfileRate = 1
	inProfile(run)
	runtime.MemProfileRate = 0
	return profiledAllocs() - before
}

// profiledAllocs returns the bytes the memory profile has allocated from
// stacks through inProfile.
func profiledAllocs() int64 {
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
		if fromRun(records[i].Stack()) {
			bytes += records[i].AllocBytes
		}
	}
	return bytes
}

// TestTextWorkCoversWhatTextAllocates writes the A1 text of each shape,
// of its text form and of shapes whose references write their longest
// text, at cells from the first to the far corner, and compares what
// writing it allocates with TextWork, which a caller charges before.
func TestTextWorkCoversWhatTextAllocates(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector changes what the allocator takes")
	}
	shapes := append(sizeShapes(),
		struct{ name, text string }{"far fixed cells", "=" + strings.Repeat("$ZZZ$999999999+", 500) + "1"},
		struct{ name, text string }{"far fixed ranges", "=SUM(" + strings.Repeat("$ZZZ$999999998:$ZZZ$999999999,", 250) + "1)"},
		struct{ name, text string }{"far whole rows", "=SUM(" + strings.Repeat("$999999998:$999999999,", 300) + "1)"},
		struct{ name, text string }{"whole columns", "=SUM(" + strings.Repeat("$ZZY:$ZZZ,", 300) + "1)"},
		struct{ name, text string }{"the shortest slots", "=" + strings.Repeat("$A$1+", 1600) + "1"},
		struct{ name, text string }{"references that leave the grid", "=" + strings.Repeat("A1+", 1600) + "1"},
		struct{ name, text string }{"long tab names", "=" + strings.Repeat("'"+strings.Repeat("ä", 60)+"'!B2+", 80) + "1"},
	)
	cells := []Pos{{}, {Row: 7, Col: 3}, {Row: 1_048_575, Col: 16_383}, {Row: rowCap - 2, Col: colCap - 1}}
	for _, c := range shapes {
		parsed, err := Parse(c.text, Pos{Row: 1_048_575, Col: 16_383}, Limits{})
		if err != nil {
			t.Fatal(err)
		}
		forms := []*Formula{parsed, parsed.TextForm()}
		if !parsed.Unreadable() {
			stored, err := ParseStored(parsed.Stored())
			if err != nil {
				t.Fatal(err)
			}
			forms = append(forms, stored, stored.TextForm())
		}
		for _, f := range forms {
			for _, at := range cells {
				var text string
				spent := allocatedBy(func() { text = f.Text(at) })
				if len(text) > maxTextRatio*len(f.Stored()) {
					t.Errorf("%s at %v: the text has %d bytes, the stored text %d", c.name, at, len(text), len(f.Stored()))
				}
				// A text form works its references out each time, so the
				// profile must see that work.
				if f.form && text != f.Stored() && spent == 0 {
					t.Fatalf("%s at %v: the profile saw nothing of writing the text", c.name, at)
				}
				// The text is written into one allocation of its length.
				if !f.form && text != f.Stored() && spent > textBytes(len(text)) {
					t.Errorf("%s at %v: writing %d bytes of text allocated %d", c.name, at, len(text), spent)
				}
				if work := f.TextWork(); spent > work {
					t.Errorf("%s at %v, text form %v: writing the text allocated %d bytes, TextWork is %d", c.name, at, f.form, spent, work)
				}
			}
		}
	}
}
