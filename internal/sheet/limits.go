package sheet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// The stored limits of spec rule 9, by their names of the technical design,
// section 11.
const (
	maxTabs         = 50
	maxCells        = 500_000
	maxFormulas     = 100_000
	maxFormulaReads = 1_000_000
	maxSheetReads   = 20_000_000
	maxSheetText    = 50_000_000
	maxCallBytes    = 8 << 20
	maxCallCells    = 50_000
	maxPageCells    = 10_000
	maxPageBytes    = 4 << 20
	maxBlankRecords = 100_000
	maxOutcomeBytes = 64 << 10

	// The bounds of a column's width and of a tab's settings (spec rules 7
	// and 8).
	maxFrozenRows = 20
	maxFrozenCols = 10
	minWidth      = 40
	maxWidth      = 1000

	// maxTabName is the longest tab name in code points (spec rule 2).
	maxTabName = formula.MaxTabNameChars

	defaultRecalcLimit = 10 * time.Second
)

// maxSlugMemberBytes is room beside the bytes of a call for what is no part
// of it: the slug member of an MCP call, which names the sheet as the path
// of the management API does, or white space around a body. A slug member
// of any valid slug, its characters written as escapes, takes 354 bytes.
const maxSlugMemberBytes = 4 << 10

// MaxUpdateBodyBytes is the largest agent write call. The slug member of an
// MCP call does not count (DecodeUpdate).
func MaxUpdateBodyBytes() int64 { return maxCallBytes }

// MaxUpdateReadBytes is the most an interface reads of one agent write call:
// the call and room for its slug member or white space around it. A longer
// body is over the bound of a call whatever it holds.
func MaxUpdateReadBytes() int64 { return maxCallBytes + maxSlugMemberBytes }

// WriteError is a refusal: its category of section 18.3, and what the
// category needs to be acted on.
type WriteError struct {
	Code   string `json:"code"`
	Reason string `json:"reason,omitempty"`
	Limit  string `json:"limit,omitempty"` // the limit's name, for sheet_capacity
	Max    int64  `json:"max,omitempty"`   // the limit
	Value  int64  `json:"value,omitempty"` // what the write would reach
	Name   string `json:"name,omitempty"`  // the tab or address the refusal is about
	Retry  bool   `json:"retry"`
	Detail string `json:"detail,omitempty"`
	cause  error
}

func (e *WriteError) Error() string {
	s := e.Code
	for _, part := range []string{e.Reason, e.Limit, e.Name, e.Detail} {
		if part != "" {
			s += ":" + part
		}
	}
	return s
}

func (e *WriteError) Unwrap() error { return e.cause }

// reason is the short reason a person's result or a fatal frame carries:
// a limit's name, disk_quota, stale_formula, gone, unavailable, invalid, or
// the reason of a conflict.
func (e *WriteError) reason() string {
	switch {
	case e.Limit != "":
		return e.Limit
	case e.Code == "invalid", e.Code == "unavailable":
		return e.Code
	case e.Reason != "":
		return e.Reason
	}
	return e.Code
}

func unavailable(reason string) *WriteError {
	return &WriteError{Code: "unavailable", Reason: reason, Retry: true}
}

func capacity(limit string, max, value int64) *WriteError {
	return &WriteError{Code: "sheet_capacity", Limit: limit, Max: max, Value: value}
}

func conflict(reason, name string) *WriteError {
	return &WriteError{Code: "conflict", Reason: reason, Name: name}
}

func invalidf(reason, format string, args ...any) *WriteError {
	return &WriteError{Code: "invalid", Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

func limitError(err *formula.LimitError) *WriteError {
	return capacity(err.Name, int64(err.Limit), int64(err.Value))
}

// errGone marks a target that no longer exists: the operation is ignored
// (spec rule 32).
var errGone = errors.New("sheet: the target is gone")

// strictJSON decodes one JSON value and refuses unknown fields and anything
// after the value.
func strictJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("unexpected data after the JSON value")
	}
	return nil
}

// checkLimits checks the stored limits on the state an action left. scan
// asks for a look at every formula, after structure or a tab changed. A
// look at every formula counts on the budget b of the action, and its
// deadline refuses the action there as in its recalculations.
func (wb *Workbook) checkLimits(written []*fnode, scan bool, b *formula.Budget) *WriteError {
	if len(wb.tabs) > maxTabs {
		return capacity("tabs", maxTabs, int64(len(wb.tabs)))
	}
	for _, t := range wb.tabs {
		if n := len(t.rows.ids); n > formula.MaxRows {
			return capacity("rows", formula.MaxRows, int64(n))
		}
		if n := len(t.cols.ids); n > formula.MaxCols {
			return capacity("columns", formula.MaxCols, int64(n))
		}
	}
	c := wb.count
	if scan {
		wb.formulaTextOK = false
	}
	switch {
	case c.cells > maxCells:
		return capacity("cells", maxCells, int64(c.cells))
	case c.formulas > maxFormulas:
		return capacity("formulas", maxFormulas, int64(c.formulas))
	}
	typed, ok := wb.typedText(b)
	if !ok {
		return calcTimeout()
	}
	if typed > maxSheetText {
		return capacity("sheet_text", maxSheetText, typed)
	}
	if scan {
		var over *fnode
		done := wb.eachFormulaPaced(b, func(_ *Tab, _ RowID, _ ColID, _ formula.Pos, c *Cell) bool {
			if c.node != nil && c.node.reads > maxFormulaReads {
				over = c.node
			}
			return over == nil
		})
		if over != nil {
			return capacity("formula_reads", maxFormulaReads, over.reads)
		}
		if !done {
			return calcTimeout()
		}
	}
	for _, n := range written {
		if n.reads > maxFormulaReads {
			return capacity("formula_reads", maxFormulaReads, n.reads)
		}
	}
	if c.reads > maxSheetReads {
		return capacity("sheet_reads", maxSheetReads, c.reads)
	}
	return nil
}
