package benchdata_test

import (
	"testing"

	"github.com/gosuda/flats/internal/sheet"
	"github.com/gosuda/flats/internal/sheet/benchdata"
)

// TestReferenceIsDeterministic generates the reference sheet twice and
// checks its shape: every call decodes as an agent call within the call
// bounds, and the manifest states what the spec's success criteria name.
func TestReferenceIsDeterministic(t *testing.T) {
	var first benchdata.Manifest
	for round := 0; round < 2; round++ {
		calls := 0
		m, err := benchdata.Reference(func(call []byte) error {
			calls++
			_, err := sheet.DecodeUpdate(call)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if round == 0 {
			first = m
			continue
		}
		if m.Digest != first.Digest || m.Calls != calls {
			t.Fatalf("two runs gave digests %s and %s", first.Digest, m.Digest)
		}
	}
	m := first
	if m.Seed != benchdata.Seed || m.Tabs != 10 || len(m.Totals) != 20 {
		t.Fatalf("manifest = %+v", m)
	}
	// 20,000 filled rows with two formulas each, 20 totals, and the small
	// tabs' own formulas.
	if want := 40000 + 20 + benchdata.SmallTabs*41; m.Formulas != want {
		t.Fatalf("%d formulas, want %d", m.Formulas, want)
	}
	if want := benchdata.LargeRows * benchdata.LargeCols; m.Cells < want {
		t.Fatalf("%d cells, want at least the large tab's %d", m.Cells, want)
	}
	// Order numbers 1 to 20,000 sum to a known value.
	if m.Totals[0] != 20000*20001/2 {
		t.Fatalf("total of the first column = %v", m.Totals[0])
	}
	for _, col := range []int{1, 8, 9, 19} {
		if m.Totals[col] != 0 {
			t.Fatalf("the text column %d has total %v", col, m.Totals[col])
		}
	}
}
