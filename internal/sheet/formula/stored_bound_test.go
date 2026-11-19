package formula

import (
	"errors"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// farSum is a sum of n references to ZZZ1 of the tab S, typed in A1. ZZZ
// is past the columns of a tab and the last column a formula can name, so
// a copy one column to the right takes each reference past what a formula
// can name.
func farSum(t *testing.T, n int) (typed *Formula, shown string) {
	t.Helper()
	typed = mustParse(t, "=S!ZZZ1"+strings.Repeat("+S!ZZZ1", n-1), "A1")
	return typed, "=#REF!" + strings.Repeat("+#REF!", n-1)
}

// allocated returns the bytes the process allocates while work runs once.
func allocated(work func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	work()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// A copy that takes a reference past what a formula can name ends it: the
// reference becomes the literal #REF!, with no tab name and no position.
// A rename of its tab changes nothing, a copy back does not bring it back,
// and the stored form is as long as the text. A copy that keeps a
// reference within what a formula can name keeps it with its tab name,
// also past the columns of a tab, and so does a part along an axis the
// copy does not change.
func TestCopyPastWhatAFormulaCanNameEndsTheReferences(t *testing.T) {
	typed, shown := farSum(t, 1000)
	at := Pos{Col: 1}
	copied := typed.Moved(Pos{}, at)
	if copied.Text(at) != shown || copied.Stored() != shown || copied.NamesTab() || len(copied.slots) != 0 {
		t.Fatalf("the copy holds %d slots and %d stored bytes, names a tab %v", len(copied.slots), len(copied.Stored()), copied.NamesTab())
	}
	copied.Tabs(func(name string) { t.Errorf("the copy names the tab %q", name) })
	if err := copied.LimitAt(at); err != nil {
		t.Errorf("LimitAt the copy: %v", err)
	}
	renamed, _, changed := copied.Apply("Sheet1", at, Transform{Kind: RenameTab, Tab: "S", Name: strings.Repeat("😀", MaxTabNameChars)})
	if changed || renamed != copied {
		t.Errorf("the rename changed the copy to %d stored bytes", len(renamed.Stored()))
	}
	if back := copied.Moved(at, Pos{}); back.Text(Pos{}) != shown || back.Stored() != shown {
		t.Errorf("the copy back shows %.40q", back.Text(Pos{}))
	}
	if v := copied.Eval(&Env{Grid: &testSheet{}, At: at, Budget: NewBudget(nil)}); v.Err() != ErrRef {
		t.Errorf("the copy is %v", v)
	}

	for _, c := range []struct {
		name, typed, from, to, want string
	}{
		{"a range", "=SUM(S!A1:ZZZ1)", "A1", "B1", "=SUM(#REF!)"},
		{"past the last row", "=S!A999999999", "A1", "A3", "=#REF!"},
		{"a fill past the last row", "=S!A999999999+B1", "A1", "A3", "=#REF!+B3"},
		{"whole columns", "=SUM(S!ZZY:ZZZ)", "A1", "C1", "=SUM(#REF!)"},
		{"to the left within what a formula names", "=S!ZZZ1", "B1", "A1", "=S!ZZY1"},
		{"past the columns of a tab and back", "=S!ZZ1", "A1", "B1", "=#REF!"},
		{"down, a reference past the columns", "=S!ZZZ1", "A1", "A2", "=S!ZZZ2"},
		{"fixed parts", "=S!$ZZZ$1", "A1", "B2", "=S!$ZZZ$1"},
	} {
		f := mustParse(t, c.typed, c.from)
		g := f.Moved(pos(t, c.from), pos(t, c.to))
		if got := g.Text(pos(t, c.to)); got != c.want {
			t.Errorf("%s: %s moved from %s to %s is %s, want %s", c.name, c.typed, c.from, c.to, got, c.want)
		}
		if fresh := mustParse(t, c.want, c.to); fresh.Stored() != g.Stored() {
			t.Errorf("%s: stored %q, a fresh parse of %s gives %q", c.name, g.Stored(), c.want, fresh.Stored())
		}
	}
}

// Only the parts along an axis that a move changes are looked at: a sort
// moves a formula from one row to another of the same column, which a
// caller can give as column A of both rows, and a part to the left of
// column A there is no reference past what a formula can name.
func TestMovedLooksAtTheAxesItChanges(t *testing.T) {
	f := mustParse(t, "=A1+S!A2", "C5")
	g := f.Moved(Pos{Row: 4}, Pos{Row: 6})
	if want := "=A3+S!A4"; g.Text(pos(t, "C7")) != want {
		t.Errorf("the sort gives %s, want %s", g.Text(pos(t, "C7")), want)
	}
}

// A load reads no stored form with a reference past what a formula can
// name at its cell: the writer ends such a reference (Moved), and with it
// gone every byte of a stored form is bound to the text at its cell.
func TestLimitAtRefusesReferencesPastWhatAFormulaNames(t *testing.T) {
	for _, stored := range []string{
		"={S!R[0]C[-1]}", "={R[-1]C[0]}", "=1+{'x'!R[-1]C[0]}", "=SUM({R[0]C[0]:R[-1]C[0]})", "=SUM({C[-1]:C[0]})",
	} {
		f, err := ParseStored(stored)
		if err != nil {
			t.Fatalf("ParseStored(%q): %v", stored, err)
		}
		if err := f.LimitAt(Pos{}); !errors.Is(err, ErrStored) {
			t.Errorf("LimitAt A1 of %q: %v", stored, err)
		}
		if err := f.LimitAt(Pos{Row: 1, Col: 1}); err != nil {
			t.Errorf("LimitAt B2 of %q: %v", stored, err)
		}
	}
}

// A formula that a rename makes too long at its cell, as one an undo entry
// holds can become, keeps its references: its text is what it reads, and
// a rename back to a short name gives it its form again. ParseRetained
// reads its form, which is no form a cell holds.
func TestRetainedFormulaPastTheLimitKeepsItsReferences(t *testing.T) {
	typed := "=D!A1" + strings.Repeat("+D!A1", 799)
	at := pos(t, "B2")
	f := mustParse(t, typed, "B2")
	long := strings.Repeat("d", 100)
	grown, _, _ := f.Apply("Sheet1", at, Transform{Kind: RenameTab, Tab: "D", Name: long})
	if grown.Unreadable() || len(grown.Stored()) <= MaxStoredBytes {
		t.Fatalf("the grown formula is unreadable %v with %d stored bytes", grown.Unreadable(), len(grown.Stored()))
	}
	want := "=" + long + "!A1" + strings.Repeat("+"+long+"!A1", 799)
	var lim *LimitError
	if grown.Text(at) != want || !errors.As(grown.LimitAt(at), &lim) || lim.Value != len(want) {
		t.Errorf("the grown formula shows %d characters, LimitAt %v", len(grown.Text(at)), grown.LimitAt(at))
	}
	if _, err := ParseStored(grown.Stored()); !errors.Is(err, ErrStored) {
		t.Errorf("ParseStored of the grown form: %v", err)
	}
	// The grown form is what a scratch file of undo history holds.
	read, err := ParseRetained(grown.Stored())
	if err != nil {
		t.Fatalf("ParseRetained of the grown form: %v", err)
	}
	back, _, _ := read.Apply("Sheet1", at, Transform{Kind: RenameTab, Tab: long, Name: "D"})
	if back.Stored() != f.Stored() || back.Text(at) != typed || back.LimitAt(at) != nil {
		t.Errorf("the rename back shows %d characters", len(back.Text(at)))
	}
}

// The parts of the bound match what the stored lexer reads: a row part of
// at most 13 bytes, a column part of at most 9 and a quoted tab name of at
// most 126, two apostrophes and 31 code points of four bytes.
func TestStoredBoundParts(t *testing.T) {
	if n := len("R[-" + strconv.Itoa(rowCap-1) + "]"); n != maxRowPart {
		t.Errorf("the longest row part has %d bytes, maxRowPart %d", n, maxRowPart)
	}
	if n := len("C[-" + strconv.Itoa(colCap-1) + "]"); n != maxColPart {
		t.Errorf("the longest column part has %d bytes, maxColPart %d", n, maxColPart)
	}
	name := strings.Repeat("😀", MaxTabNameChars)
	if q := quoteTabName(name); len(q) != maxQuotedTab {
		t.Errorf("the quoted name has %d bytes, maxQuotedTab %d", len(q), maxQuotedTab)
	}
	if q := quoteTabName("a" + strings.Repeat("'", MaxTabNameChars-2) + "a"); len(q) > maxQuotedTab {
		t.Errorf("a name of apostrophes quotes to %d bytes", len(q))
	}
	corner := "R[-" + strconv.Itoa(rowCap-1) + "]C[-" + strconv.Itoa(colCap-1) + "]"
	slot := "{" + quoteTabName(name) + "!" + corner + ":" + corner + "}"
	if len(slot) != maxSlotBytes {
		t.Fatalf("the longest slot has %d bytes, maxSlotBytes %d", len(slot), maxSlotBytes)
	}
	f, err := ParseStored("=" + slot)
	if err != nil || len(f.slots) != 1 || f.slots[0].tabName() != name {
		t.Fatalf("ParseStored of the longest slot: %v", err)
	}
	for _, longer := range []string{
		strings.Replace(slot, "R[-", "R[-1", 1),
		strings.Replace(slot, "C[-", "C[-1", 1),
	} {
		if _, err := ParseStored("=" + longer); !errors.Is(err, ErrStored) {
			t.Errorf("ParseStored of a slot of %d bytes: %v", len(longer), err)
		}
	}
}

// MaxStoredBytes is nine bytes for each code point of the limit. The
// formula of a cell that is longest for its text is a sum of references
// to A1 typed in the last cell of the grid, each a slot of 18 bytes for
// two code points: its stored form is within the bound, is read, and
// passes LimitAt at its cell. A stored form of exactly MaxStoredBytes
// bytes is read, and one a byte longer is refused without a byte read.
func TestMaxStoredBytes(t *testing.T) {
	far := Pos{Row: MaxRows - 1, Col: MaxCols - 1}
	n := (MaxFormulaChars - 1) / len("+A1")
	f, err := Parse("=A1"+strings.Repeat("+A1", n-1), far, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	slot := "{R[-99999]C[-701]}"
	if want := 1 + len(slot) + (n-1)*len("+"+slot); len(f.Stored()) != want || len(f.Stored()) > MaxStoredBytes {
		t.Fatalf("the stored form has %d bytes, want %d", len(f.Stored()), want)
	}
	read, err := ParseStored(f.Stored())
	if err != nil || read.LimitAt(far) != nil {
		t.Fatalf("ParseStored of the longest form for its text: %v", err)
	}

	// The slots of a stored form need no operator between them.
	refs := "=" + strings.Repeat(slot, MaxFormulaChars/2-1)
	at := `+"` + strings.Repeat("x", MaxStoredBytes-len(refs)-len(`+""`)) + `"`
	if len(refs+at) != MaxStoredBytes {
		t.Fatalf("the form has %d bytes", len(refs+at))
	}
	if _, err := ParseStored(refs + at); err != nil {
		t.Errorf("ParseStored at MaxStoredBytes: %v", err)
	}
	over := refs + at[:len(at)-1] + `x"`
	if _, err := ParseStored(over); !errors.Is(err, ErrStored) {
		t.Errorf("ParseStored at %d bytes: %v", len(over), err)
	}
	if n := testing.AllocsPerRun(10, func() { _, _ = ParseStored(over) }); n != 0 {
		t.Errorf("ParseStored past MaxStoredBytes allocates %v times", n)
	}
}

// A stored form holds at most as many tokens as a formula has code points
// at the limit. A sum of ones of MaxStoredBytes bytes, and one of
// MaxRetainedBytes for ParseRetained, has far more: each is refused once
// the lexer reaches the token past the bound, so the refusal allocates no
// more than that many tokens take and builds no tree. A form of exactly
// that many tokens is read.
func TestStoredTokenBound(t *testing.T) {
	ones := func(bytes int) string { return "=1" + strings.Repeat("+1", (bytes-2)/2) }
	// What the lexer keeps for the tokens and what its list allocates as it
	// grows, twice over, and a little beside.
	const most = 4*maxStoredTokens*16 + 64<<10
	for _, c := range []struct {
		name  string
		parse func(string) (*Formula, error)
		bytes int
	}{
		{"ParseStored", ParseStored, MaxStoredBytes},
		{"ParseRetained", ParseRetained, MaxRetainedBytes},
	} {
		form := ones(c.bytes)
		if len(form) != c.bytes {
			t.Fatalf("%s: the form has %d bytes", c.name, len(form))
		}
		if _, err := c.parse(form); !errors.Is(err, ErrStored) {
			t.Errorf("%s of a sum of %d ones: %v", c.name, c.bytes/2, err)
		}
		if got := allocated(func() { _, _ = c.parse(form) }); got > most {
			t.Errorf("%s of a sum of %d ones allocates %d bytes, want at most %d", c.name, c.bytes/2, got, most)
		}
		at := "=1" + strings.Repeat("+1", maxStoredTokens/2-1) + "%"
		if f, err := c.parse(at); err != nil || f.root == nil {
			t.Errorf("%s of a form of %d tokens: %v", c.name, maxStoredTokens, err)
		}
		if _, err := c.parse(at + "%"); !errors.Is(err, ErrStored) {
			t.Errorf("%s of a form of %d tokens: %v", c.name, maxStoredTokens+1, err)
		}
	}
	// Each token of a formula Parse took writes a code point at least, and
	// a copy that ends a reference keeps the number of tokens.
	far := Pos{Row: MaxRows - 1, Col: MaxCols - 1}
	f, err := Parse("=A1"+strings.Repeat(" A1", (MaxFormulaChars-3)/3), far, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	moved := f.Moved(far, Pos{})
	for _, g := range []*Formula{f, moved} {
		if _, err := ParseStored(g.Stored()); err != nil {
			t.Errorf("ParseStored of %d stored bytes: %v", len(g.Stored()), err)
		}
	}
}

// MaxRetainedBytes is what a form takes when every two code points of a
// formula of 8,192 are a reference that became the longest slot: a form
// of that room is read, one a byte longer is not, without a byte read. The
// longest form a rename makes of a formula a cell held, a sum of as many
// references to a tab as a formula holds renamed to a tab name of 100
// code points of four bytes each, is within it and is read.
func TestMaxRetainedBytesWorstCase(t *testing.T) {
	name := strings.Repeat("😀", MaxTabNameChars)
	corner := "R[-" + strconv.Itoa(rowCap-1) + "]C[-" + strconv.Itoa(colCap-1) + "]"
	slot := "{" + quoteTabName(name) + "!" + corner + ":" + corner + "}"
	// One slot fewer than the room counts leaves room for the text in
	// quotation marks within the tokens a form holds.
	const slots = MaxFormulaChars/2 - 1
	refs := "=" + slot + strings.Repeat("+"+slot, slots-1)
	pad := MaxRetainedBytes - len(refs) - len(`+""`)
	if pad < 0 || pad > MaxTextLen {
		t.Fatalf("the padding is %d bytes", pad)
	}
	longest := refs + `+"` + strings.Repeat("x", pad) + `"`
	if len(longest) != MaxRetainedBytes {
		t.Fatalf("the longest form has %d bytes", len(longest))
	}
	f, err := ParseRetained(longest)
	if err != nil || len(f.slots) != slots {
		t.Fatalf("ParseRetained at MaxRetainedBytes: %v", err)
	}
	over := refs + `+"` + strings.Repeat("x", pad+1) + `"`
	if _, err := ParseRetained(over); !errors.Is(err, ErrStored) {
		t.Fatalf("ParseRetained at %d bytes: %v", len(over), err)
	}
	if n := testing.AllocsPerRun(10, func() { _, _ = ParseRetained(over) }); n != 0 {
		t.Errorf("ParseRetained past MaxRetainedBytes allocates %v times", n)
	}

	far := Pos{Row: MaxRows - 1, Col: MaxCols - 1}
	refsTyped := MaxFormulaChars / len("+D!A1")
	typed, err := Parse("=D!A1"+strings.Repeat("+D!A1", refsTyped-1), far, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	grown, _, _ := typed.Apply("Sheet1", far, Transform{Kind: RenameTab, Tab: "D", Name: name})
	if grown.Unreadable() || len(grown.Stored()) <= MaxStoredBytes || len(grown.Stored()) > MaxRetainedBytes {
		t.Fatalf("the renamed sum of %d references has %d stored bytes", refsTyped, len(grown.Stored()))
	}
	if _, err := ParseRetained(grown.Stored()); err != nil {
		t.Errorf("ParseRetained of the renamed sum: %v", err)
	}
}

// ParseRetained bounds the room of a form, not only its bytes: a form of
// references to the tab S with room MaxRetainedBytes is read, and so is
// what a rename of S to the longest name and a cut into the tab of the
// longest name make of it, although each is longer. A form a byte longer
// is refused, with far fewer bytes than MaxRetainedBytes.
func TestStoredRoom(t *testing.T) {
	const slots = MaxFormulaChars/2 - 1
	name := strings.Repeat("😀", MaxTabNameChars)
	for _, ref := range []string{"{S!R[0]C[0]}", "{R[0]C[0]}"} {
		refs := "=" + ref + strings.Repeat("+"+ref, slots-1)
		pad := MaxRetainedBytes - len(refs) - slots*(maxSlotBytes-len(ref)) - len(`+""`)
		form := refs + `+"` + strings.Repeat("x", pad) + `"`
		f, err := ParseRetained(form)
		if err != nil {
			t.Fatalf("%s: ParseRetained at the bound: %v", ref, err)
		}
		if _, err := ParseRetained(form + " "); !errors.Is(err, ErrStored) || len(form)+1 >= MaxRetainedBytes/2 {
			t.Errorf("%s: ParseRetained of a form of %d bytes past the room: %v", ref, len(form)+1, err)
		}
		g, _, _ := f.Apply("Sheet1", Pos{}, Transform{Kind: RenameTab, Tab: "S", Name: name})
		if ref[1] == 'R' {
			g = f.Cut(Pos{}, Pos{}, name, "Sheet1")
		}
		if len(g.Stored()) <= len(form) {
			t.Fatalf("%s: the transform left %d bytes of %d", ref, len(g.Stored()), len(form))
		}
		if _, err := ParseRetained(g.Stored()); err != nil {
			t.Errorf("%s: ParseRetained of the form of %d bytes the transform made: %v", ref, len(g.Stored()), err)
		}
	}
}

// Placed ends a reference only where it lies past what a formula can name
// at the cell, whatever tab it names. A reference to a tab that is not
// there now keeps its slot, so the tab is read again once it is back. A
// reference past the rows of a tab that a formula can still name keeps
// its slot too and shows #REF! (Appendix B, engine item 23).
func TestPlacedEndsOnlyWhatAFormulaCannotName(t *testing.T) {
	f, err := ParseStored("={Gone!R[-8]C[2]}+{'x y'!R[2]C[0]}+{R[200000]C[0]}")
	if err != nil {
		t.Fatal(err)
	}
	at := Pos{Row: 8}
	if g := f.Placed(at); g != f {
		t.Errorf("Placed at row 9 gives %q", g.Stored())
	}
	if err := f.LimitAt(at); err != nil {
		t.Errorf("LimitAt row 9: %v", err)
	}
	if got, want := f.Text(at), "=Gone!C1+'x y'!A11+A200009"; got != want {
		t.Errorf("the text at row 9 is %s, want %s", got, want)
	}
	up := Pos{Row: 5}
	g := f.Placed(up)
	if got, want := g.Stored(), "=#REF!+{'x y'!R[2]C[0]}+{R[200000]C[0]}"; got != want {
		t.Fatalf("Placed at row 6 is stored as %s, want %s", got, want)
	}
	var tabs []string
	g.Tabs(func(name string) { tabs = append(tabs, name) })
	if len(tabs) != 1 || tabs[0] != "x y" {
		t.Errorf("Placed at row 6 names the tabs %q", tabs)
	}
	if err := g.LimitAt(up); err != nil {
		t.Errorf("LimitAt row 6 of the placed formula: %v", err)
	}
	if back := g.Placed(at); back != g {
		t.Errorf("Placed back at row 9 gives %q", back.Stored())
	}
}
