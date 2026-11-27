package sheet

import (
	"encoding/json"
	"runtime"
	"strconv"
	"testing"
)

// TestSmallResultsOfLongTextsStaySmall writes many formulas that cut a
// short piece from a long text they build, through agent calls, and checks
// that the sheet keeps the memory of the pieces it counts and not of the
// long texts: 2,100 results of 1 to 3 characters, each cut from a text of
// 98,301 bytes.
func TestSmallResultsOfLongTextsStaySmall(t *testing.T) {
	e := start(t)
	live := func() uint64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	row := []string{`=LEFT(REPT("가",32767),1)`, `=RIGHT(REPT("가",32767),2)`, `=MID(REPT("가",32767),7,3)`}
	const batches, rows = 7, 100
	before := live()
	for b := 0; b < batches; b++ {
		block := make([][]string, rows)
		for i := range block {
			block[i] = row
		}
		call, err := json.Marshal(map[string]any{"ops": []any{map[string]any{
			"op": "write", "tab": "Sheet1", "at": "A" + strconv.Itoa(b*rows+1), "rows": block,
		}}})
		if err != nil {
			t.Fatal(err)
		}
		e.mustAgent(string(call))
	}
	grown := int64(live()) - int64(before)
	e.in.mu.RLock()
	counted, formulas := e.in.wb.count.calcText, len(e.in.wb.formulas)
	e.in.mu.RUnlock()
	if want := int64(batches * rows * 6); counted != want {
		t.Fatalf("the sheet counts %d characters of calculated text, want %d", counted, want)
	}
	// The long texts together are 206 MB. The cells, their formulas and
	// what the calls leave behind stay far under this bound.
	if grown > 16<<20 {
		t.Errorf("%d results of %d characters keep %d bytes alive (%d formulas)", batches*rows*len(row), counted, grown, formulas)
	}
}
