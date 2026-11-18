package sheet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// farSum is a formula of 8,190 characters that adds A1 2,730 times. Typed
// at Z1000 each reference is the slot {R[-999]C[-25]}, so its stored form
// is more than five times as long as its text.
var farSum = "=A1" + strings.Repeat("+A1", 2729)

// writeFarSums writes farSum into column Z of n rows that end at row 1000,
// with 1 in A1.
func writeFarSums(e *env, n int) {
	e.t.Helper()
	rows := make([]string, n)
	for i := range rows {
		rows[i] = `[` + strconv.Quote(farSum) + `]`
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1]]},` +
		fmt.Sprintf(`{"op":"write","tab":"Sheet1","at":"Z%d","rows":[%s]}]}`, 1001-n, strings.Join(rows, ",")))
}

// A sheet.db cell whose stored formula or text no typed one could be is
// refused by the load: the text of the formula at its cell is longer than
// 8,192 characters, it nests deeper than 64 levels, its stored form is
// longer than formula.MaxStoredBytes or holds more tokens than a formula
// can, or the text is longer than 32,767 characters.
func TestStoredFormulaOverTheLimitsIsNotLoaded(t *testing.T) {
	cell := func(kind formula.ContentKind, str string) string {
		return fmt.Sprintf(`UPDATE cells SET kind=%d,num=NULL,str='%s' WHERE tab=1 AND row=1 AND col=1`, kind, str)
	}
	for _, c := range []struct{ name, damage string }{
		{"a formula of a long literal", cell(formula.ContentFormula, `="`+strings.Repeat("x", 32768)+`"`)},
		{"a formula nested 65 levels", cell(formula.ContentFormula, "="+strings.Repeat("(", 65)+"1"+strings.Repeat(")", 65))},
		{"a formula of a long literal sum", cell(formula.ContentFormula, "=1"+strings.Repeat("+1", formula.MaxStoredBytes/2))},
		{"a formula of more tokens than a formula holds", cell(formula.ContentFormula, "=1"+strings.Repeat("+1", (formula.MaxStoredBytes-2)/2))},
		{"a long text", cell(formula.ContentText, strings.Repeat("x", 32768))},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := storedAxisSheet(t, c.damage)
			in, _, err := Open(e.dir, e.b, e.opts)
			if err != nil {
				t.Fatal(err)
			}
			defer in.Stop()
			if err := in.await(context.Background(), 0); err == nil {
				t.Fatal("the damaged sheet.db was loaded")
			} else if werr := refusal(err); werr.Code != "unavailable" || werr.Reason != "load" {
				t.Fatalf("the load failed with %v, want unavailable load", err)
			}
		})
	}
	// At the limits the cell loads: a formula nested 64 levels and a text
	// of 32,767 characters.
	e := storedAxisSheet(t, cell(formula.ContentFormula, "="+strings.Repeat("(", 64)+"1"+strings.Repeat(")", 64)))
	e.open()
	e.want("A1", "1")
	e.in.Stop()
	e = storedAxisSheet(t, cell(formula.ContentText, strings.Repeat("x", 32767)))
	e.open()
	e.want("A1", strings.Repeat("x", 32767))
}

// A formula whose stored form is longer than its text, as one typed far
// from the cells it reads is, keeps loading: a revision view serves it, a
// rollback to it applies, and the sheet that has it opens again.
func TestStoredFormLongerThanItsTextLoads(t *testing.T) {
	ctx := context.Background()
	e := start(t)
	writeFarSums(e, 1)
	_, row, col := e.ids("Z1000")
	e.in.mu.RLock()
	stored := e.in.wb.tabs[0].cell(row, col).f.Stored()
	e.in.mu.RUnlock()
	if len(stored) < 5*len(farSum) || len(stored) > formula.MaxStoredBytes {
		t.Fatalf("the stored form has %d bytes for %d characters", len(stored), len(farSum))
	}
	n := closeRevision(e)
	e.mustAgent(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["Z1000"]}]}`)
	files, date, release, err := e.b.RevisionFiles(ctx, n)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := e.viewRevision(1000, files, date); err != nil {
		t.Fatalf("the view: %v", err)
	}
	if err := e.in.Rollback(ctx, files, "Alice", "far"); err != nil {
		t.Fatalf("the rollback: %v", err)
	}
	e.restart()
	e.want("Z1000", "2730")
	if got := e.text("Z1000"); got != farSum {
		t.Fatalf("the formula reads back as %d characters", len(got))
	}
}

// Reading long stored formulas reaches the check points of a revision
// build and of a load in their cells phase for each 256 KiB of formula
// text, so that a stop or the end of a request ends the read there.
func TestLongStoredFormulasReachCheckPoints(t *testing.T) {
	ctx := context.Background()
	const n = 40
	e := start(t)
	writeFarSums(e, n)
	e.in.mu.RLock()
	bytes := 0
	for _, rd := range e.in.wb.tabs[0].data {
		for _, s := range rd.cells {
			if s.c.f != nil {
				bytes += len(s.c.f.Stored())
			}
		}
	}
	e.in.mu.RUnlock()
	want := bytes / (64 * loadCheckCells)
	if want < 2 {
		t.Fatalf("the formulas hold %d bytes", bytes)
	}
	rev := closeRevision(e)
	files, _, release, err := e.b.RevisionFiles(ctx, rev)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	read := func(hash string) (io.ReadCloser, error) { return e.b.OpenBlob(ctx, hash) }
	points := 0
	if _, err := loadRevision(files, read, func(phase string) bool {
		if phase == "cells" {
			points++
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if points < want {
		t.Errorf("the revision build reached %d check points in its cells, want at least %d", points, want)
	}
	if _, err := loadRevision(files, read, func(phase string) bool { return phase != "cells" }); !errors.Is(err, errRevisionStopped) {
		t.Errorf("the revision build stopped in its cells answered %v", err)
	}

	w := watchLoads(t)
	e.restart()
	if got := w.count(e.in, "cells"); got < want {
		t.Errorf("the load reached %d check points in its cells, want at least %d", got, want)
	}
	e.want("Z1000", "2730")
}
