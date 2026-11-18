package sheet

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// Update is one agent write call (technical design, section 18.2), as
// DecodeUpdate built it from the bytes of the call.
type Update struct {
	Slug    string
	State   string
	OpID    string
	Message string
	Guard   *Guard
	Ops     []AgentOp

	// Create marks the call that creates the sheet: it needs no state
	// (spec rule 36). Core sets it; it is not part of the call's JSON.
	Create bool

	has   uint8 // the optional fields the call carries
	hash  string
	cells int
}

const (
	callSlug = 1 << iota
	callState
	callOpID
	callMessage
)

// HasSlug reports whether the call itself names a sheet. A caller that
// names the sheet some other way refuses such a call.
func (u *Update) HasSlug() bool { return u.has&callSlug != 0 }

// Guard is the guard of a call: refuse if the sheet left the state the
// agent read, or if the content of a cell of the ranges changed since.
type Guard struct {
	Unchanged bool
	Ranges    []string

	unchanged, ranges bool // the fields the guard carries
}

// AgentOp is one operation of a call. The fields an operation reads are
// those of its row in section 18.2.
type AgentOp struct {
	Op         string
	Tab        string
	Ranges     []string
	Formats    bool
	Cols       string
	Width      int
	Count      int
	By         string
	Desc       bool
	Name       string
	To         *int
	FrozenRows *int
	FrozenCols *int

	has uint32 // the fields the operation carries, by their place in agentOpMembers

	// at and rows as the call wrote them: each is a text or the other form.
	atText   string
	atNum    int64
	atIsNum  bool
	rowsText string
	rowsList bool
	csv      string

	at     formula.Pos   // write, import_csv: the top-left cell; inserts and deletes: the row or column
	place  int           // add_tab: position from 1, 0 for the end
	values [][]agentCell // write
	fields [][]string    // import_csv
	ranges []formula.Range
	patch  formatPatch
}

// agentCell is one cell of a write, read by its JSON type (spec rule 43).
type agentCell struct {
	content formula.Content
	formula string // typed formula text, when the cell is a formula
}

var opIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// Slots of an agent call.
const (
	cCall = sPropEnd + 1 + iota
	cSlug
	cState
	cOpID
	cMessage
	cGuard
	cUnchanged
	cRanges
	cRange
	cOps

	// An operation and its members, in the order of agentOpMembers: a
	// member's bit in AgentOp.has is its distance from cOpKind.
	cOp
	cOpKind
	cOpTab
	cOpAt
	cOpRows
	cOpRanges
	cOpFormats
	cOpFormat
	cOpCols
	cOpWidth
	cOpCount
	cOpBy
	cOpDesc
	cOpName
	cOpTo
	cOpFrozenRows
	cOpFrozenCols
	cOpCSV

	cOpRange
	cRow
	cCell
	cCellObject
	cDate
	cTime
	cDateTime
	cFormula
	cValue
	cError
)

// cellShape is a cell of a write: a number, a boolean, null, a string, a
// date or time object, or a formula object as a read returns it. value and
// error of a formula object are read and not written.
var cellShape = orNull(oneOf(aNumber(cCell), aFlag(cCell), aText(cCell), anObject(cCellObject,
	may("date", aText(cDate)), may("time", aText(cTime)), may("datetime", aText(cDateTime)),
	may("formula", aText(cFormula)), may("value", orNull(oneOf(aNumber(cValue), aText(cValue), aFlag(cValue)))), may("error", aText(cError)))))

// agentOpMembers are the fields the operations of a call carry between
// them, and agentOps lists per operation the ones it needs and the ones
// it may leave out. No field of an operation may be null. at is an address
// or column letters, or a row number or a position; rows is rows of cells,
// or whole rows such as "2:200". tab_settings needs one of its two counts.
var (
	agentOpMembers = []member{
		need("op", aText(cOpKind)),
		may("tab", aText(cOpTab)), may("at", oneOf(aText(cOpAt), anInt(cOpAt))),
		may("rows", oneOf(aText(cOpRows), aList(cOpRows, aList(cRow, cellShape)))),
		may("ranges", aList(cOpRanges, aText(cOpRange))), may("formats", aFlag(cOpFormats)),
		may("format", patchShape(cOpFormat)), may("cols", aText(cOpCols)),
		may("width", anInt(cOpWidth)), may("count", anInt(cOpCount)),
		may("by", aText(cOpBy)), may("desc", aFlag(cOpDesc)), may("name", aText(cOpName)),
		may("to", anInt(cOpTo)), may("frozen_rows", anInt(cOpFrozenRows)), may("frozen_cols", anInt(cOpFrozenCols)),
		may("csv", aText(cOpCSV)),
	}
	agentOps = map[string][2]string{
		"write":        {"tab at rows"},
		"clear":        {"tab ranges", "formats"},
		"format":       {"tab ranges format"},
		"widths":       {"tab cols width"},
		"insert_rows":  {"tab at count"},
		"delete_rows":  {"tab at count"},
		"insert_cols":  {"tab at count"},
		"delete_cols":  {"tab at count"},
		"sort":         {"tab rows by", "desc"},
		"add_tab":      {"name", "at"},
		"rename_tab":   {"tab name"},
		"move_tab":     {"tab to"},
		"delete_tab":   {"tab"},
		"tab_settings": {"tab", "frozen_rows frozen_cols"},
		"import_csv":   {"tab at csv"},
	}
	agentOpShape = aVariant(cOp, "op", false, agentOpMembers, agentOps)

	// callShape is an update_sheet call. Nothing in it is null but a cell
	// and a property of a format.
	callShape = anObject(cCall,
		may("slug", aText(cSlug)), may("state", aText(cState)), may("op_id", aText(cOpID)), may("message", aText(cMessage)),
		may("guard", anObject(cGuard, may("unchanged", aFlag(cUnchanged)), may("ranges", aList(cRanges, aText(cRange))))),
		need("ops", aList(cOps, agentOpShape)))
)

// agentBit is the bit of a field of an operation in AgentOp.has.
func agentBit(slot int) uint32 { return 1 << (slot - cOpKind) }

// DecodeUpdate reads and validates an agent write call. The bytes are read
// once, against callShape: it refuses unknown operations and fields, a
// field twice or in another case, values of the wrong JSON type, a null
// that is no cell, and calls over the byte or cell bound.
//
// The byte bound counts the call without its slug member, so that the
// same call meets it through MCP, which names the sheet in the call, and
// through the management API and the CLI, which name it in the path. Bytes
// longer than the bound and room for that member are not read.
func DecodeUpdate(raw []byte) (Update, error) {
	if len(raw) > maxCallBytes+maxSlugMemberBytes {
		return Update{}, capacity("call_bytes", maxCallBytes, 0)
	}
	r := callReader{}
	if _, fault := walk(raw, callShape, &r); fault != nil {
		return Update{}, r.refusal(fault)
	}
	if size := int64(len(raw) - r.slugBytes); size > maxCallBytes {
		return Update{}, capacity("call_bytes", maxCallBytes, size)
	}
	if r.err != nil {
		return Update{}, r.err
	}
	u := r.u
	if u.has&callOpID != 0 && u.OpID != "" && !opIDPattern.MatchString(u.OpID) {
		return Update{}, invalidf("op_id", "an op_id is 1 to 128 characters of letters, digits and . _ : -")
	}
	if utf8.RuneCountInString(u.Message) > 1000 {
		return Update{}, invalidf("message", "a message has at most 1,000 characters")
	}
	if u.Guard != nil {
		for _, text := range u.Guard.Ranges {
			if r, err := formula.ParseRange(text); err != nil || r.Tab == "" {
				return Update{}, invalidf("guard", "guard range %q does not name a tab and a range", text)
			}
		}
	}
	sum := sha256.Sum256(u.appendCall(nil, true))
	u.hash = hex.EncodeToString(sum[:])
	return u, nil
}

// callReader builds a call from the parts walk hands it. err is the first
// refusal of a value its shape cannot say: an address that is none, a
// count outside its bounds, a cell object of two kinds.
type callReader struct {
	noSink
	u     Update
	op    *AgentOp
	err   *WriteError
	patch patchReader
	cell  cellParts
	over  bool // the call passed its cell bound: no more cells are kept

	slugBytes int // the bytes of the slug member, which the byte bound leaves out
}

func (r *callReader) member(slot, size int) {
	if slot == cSlug {
		r.slugBytes = size
	}
}

// cellParts are the fields of a cell object.
type cellParts struct {
	date, time, datetime, formula string
	has                           uint8
}

const (
	cellDate = 1 << iota
	cellTime
	cellDateTime
	cellFormula
	cellValue
)

// refuse keeps the first refusal, with the operation it is about.
func (r *callReader) refuse(err *WriteError) {
	if r.err != nil {
		return
	}
	if r.op != nil {
		err = &WriteError{Code: err.Code, Reason: err.Reason, Limit: err.Limit, Max: err.Max, Value: err.Value, Name: err.Name,
			Detail: strings.TrimSuffix(fmt.Sprintf("operation %d (%s): %s", len(r.u.Ops), r.op.Op, err.Detail), ": ")}
	}
	r.err = err
}

// refusal is the invalid refusal of a call walk did not read. Its reason
// names the field at fault (section 18.3): the field of the operation, or
// the field of the call.
func (r *callReader) refusal(f *wireFault) *WriteError {
	inOp := f.under(0) == "ops" && len(f.path) >= 2
	reason, rest := f.under(0), f.path
	switch {
	case f.rule == faultSyntax:
		// Bytes that are no JSON are no field's fault, wherever they stand.
		reason = "json"
		if inOp {
			rest = f.path[2:]
		}
	case inOp && len(f.path) == 2:
		rest = nil
		switch f.rule {
		case faultUnknown, faultForeign:
			reason = "field"
		case faultMissing, faultTwice:
			reason = f.field
		case faultKind:
			reason = "op"
		}
	case inOp:
		reason, rest = f.under(2), f.path[2:]
		if reason == "rows" && len(f.path) >= 5 {
			reason = "cell"
		}
	case len(f.path) == 0:
		switch f.rule {
		case faultType, faultNull:
			reason = "json"
		case faultUnknown:
			reason = "field"
		default:
			reason = f.field
		}
	}
	detail := (&wireFault{rule: f.rule, path: rest, field: f.field, takes: f.takes}).Error()
	switch {
	case inOp && len(f.path) == 2 && f.rule == faultKind:
		detail = "unknown operation"
	case inOp && len(f.path) == 2 && (f.rule == faultUnknown || f.rule == faultForeign):
		detail = fmt.Sprintf("field %q does not belong to this operation", f.field)
	case inOp && len(f.path) == 2 && f.rule == faultMissing:
		detail = "the operation needs " + f.field
	case inOp && len(f.path) == 2 && f.rule == faultTwice:
		detail = "the operation has " + f.field + " twice"
	case inOp && len(f.path) == 2:
		detail = "an operation is an object"
	}
	if inOp {
		kind := ""
		if at := f.path[1].index; at < len(r.u.Ops) {
			kind = " (" + r.u.Ops[at].Op + ")"
		}
		detail = fmt.Sprintf("operation %d%s: %s", f.path[1].index+1, kind, detail)
	}
	return invalidf(reason, "%s", detail)
}

func (r *callReader) begin(slot int) {
	switch slot {
	case cGuard:
		r.u.Guard = &Guard{}
	case cRanges:
		r.u.Guard.ranges = true
	case cOps:
		r.u.Ops = []AgentOp{}
	case cOp:
		r.u.Ops = append(r.u.Ops, AgentOp{})
		r.op = &r.u.Ops[len(r.u.Ops)-1]
	case cOpRows:
		r.op.has |= agentBit(slot)
		r.op.rowsList = true
		r.op.values = [][]agentCell{}
	case cRow:
		if !r.over {
			// The row before says how long this one is, up to the cells a
			// call may still hold.
			width := 0
			if n := len(r.op.values); n > 0 {
				width = min(len(r.op.values[n-1]), maxCallCells-r.u.cells)
			}
			r.op.values = append(r.op.values, make([]agentCell, 0, max(width, 0)))
		}
	case cCellObject:
		r.cell = cellParts{}
	case cOpRanges:
		r.op.has |= agentBit(slot)
		r.op.Ranges = []string{}
	case cOpFormat:
		r.op.has |= agentBit(slot)
		r.patch = patchReader{}
	default:
		r.patch.begin(slot)
	}
}

func (r *callReader) end(slot int) {
	switch slot {
	case cOp:
		if err := r.op.check(&r.u.cells); err != nil {
			r.refuse(err)
		} else if r.u.cells > maxCallCells {
			r.refuse(capacity("call_cells", maxCallCells, int64(r.u.cells)))
		}
		r.op = nil
	case cCellObject:
		r.put(r.cell.cell())
	case cOpFormat:
		r.op.patch = r.patch.patch
		if r.patch.bad != nil {
			r.refuse(r.patch.bad)
		}
	default:
		r.patch.end(slot)
	}
}

// put adds a cell to the row being read. Past the cell bound of a call
// the cells are counted and not kept: the call is refused when its
// operation ends.
func (r *callReader) put(cell agentCell, err *WriteError) {
	r.u.cells++
	if r.over = r.over || r.u.cells > maxCallCells; r.over {
		return
	}
	row := &r.op.values[len(r.op.values)-1]
	if err != nil {
		err.Detail = fmt.Sprintf("row %d, cell %d: %s", len(r.op.values), len(*row)+1, err.Detail)
		r.refuse(err)
	}
	*row = append(*row, cell)
}

func (r *callReader) text(slot int, b []byte) {
	if slot > cOpKind && slot <= cOpCSV {
		r.op.has |= agentBit(slot)
	}
	switch slot {
	case cSlug:
		r.u.Slug, r.u.has = string(b), r.u.has|callSlug
	case cState:
		r.u.State, r.u.has = string(b), r.u.has|callState
	case cOpID:
		r.u.OpID, r.u.has = string(b), r.u.has|callOpID
	case cMessage:
		r.u.Message, r.u.has = string(b), r.u.has|callMessage
	case cRange:
		r.u.Guard.Ranges = append(r.u.Guard.Ranges, string(b))
	case cOpKind:
		r.op.Op = string(b)
	case cOpTab:
		r.op.Tab = string(b)
	case cOpAt:
		r.op.atText = string(b)
	case cOpRows:
		r.op.rowsText = string(b)
	case cOpRange:
		r.op.Ranges = append(r.op.Ranges, string(b))
	case cOpCols:
		r.op.Cols = string(b)
	case cOpBy:
		r.op.By = string(b)
	case cOpName:
		r.op.Name = string(b)
	case cOpCSV:
		r.op.csv = string(b)
	case cCell:
		r.put(textCell(string(b)))
	case cDate:
		r.cell.date, r.cell.has = string(b), r.cell.has|cellDate
	case cTime:
		r.cell.time, r.cell.has = string(b), r.cell.has|cellTime
	case cDateTime:
		r.cell.datetime, r.cell.has = string(b), r.cell.has|cellDateTime
	case cFormula:
		r.cell.formula, r.cell.has = string(b), r.cell.has|cellFormula
	case cValue, cError:
		r.cell.has |= cellValue
	default:
		r.patch.text(slot, b)
	}
}

func (r *callReader) integer(slot int, n int64) {
	if slot > cOpKind && slot <= cOpCSV {
		r.op.has |= agentBit(slot)
	}
	switch slot {
	case cOpAt:
		r.op.atNum, r.op.atIsNum = n, true
	case cOpWidth:
		r.op.Width = countOf(n)
	case cOpCount:
		r.op.Count = countOf(n)
	case cOpTo:
		to := countOf(n)
		r.op.To = &to
	case cOpFrozenRows:
		rows := countOf(n)
		r.op.FrozenRows = &rows
	case cOpFrozenCols:
		cols := countOf(n)
		r.op.FrozenCols = &cols
	case sNumDecimals:
		r.patch.num.decimals = n
	}
}

func (r *callReader) number(slot int, f float64) {
	switch slot {
	case cCell:
		r.put(agentCell{content: formula.Content{Kind: formula.ContentNumber, Num: f}}, nil)
	case cValue:
		r.cell.has |= cellValue
	}
}

func (r *callReader) flag(slot int, on bool) {
	switch slot {
	case cUnchanged:
		r.u.Guard.Unchanged, r.u.Guard.unchanged = on, true
	case cOpFormats:
		r.op.Formats, r.op.has = on, r.op.has|agentBit(slot)
	case cOpDesc:
		r.op.Desc, r.op.has = on, r.op.has|agentBit(slot)
	case cCell:
		n := 0.0
		if on {
			n = 1
		}
		r.put(agentCell{content: formula.Content{Kind: formula.ContentBool, Num: n}}, nil)
	case cValue:
		r.cell.has |= cellValue
	default:
		r.patch.flag(slot, on)
	}
}

func (r *callReader) null(slot int) {
	switch slot {
	case cCell:
		r.put(agentCell{}, nil)
	case cValue:
		r.cell.has |= cellValue
	default:
		r.patch.null(slot)
	}
}

// textCell reads a cell written as a string: a formula, text after an
// apostrophe, or text (spec rule 43).
func textCell(s string) (agentCell, *WriteError) {
	switch {
	case len(s) > 1 && s[0] == '=':
		return agentCell{formula: s}, nil
	case s != "" && s[0] == '\'':
		s = s[1:]
	}
	if s == "" {
		return agentCell{}, nil
	}
	if err := checkText(s); err != nil {
		return agentCell{}, err
	}
	return agentCell{content: formula.Content{Kind: formula.ContentText, Text: s}}, nil
}

// cell reads a cell written as an object: a date, a time, a date with
// time, or a formula object, of which only the formula is written.
func (c *cellParts) cell() (agentCell, *WriteError) {
	bad := func(format string, args ...any) (agentCell, *WriteError) {
		return agentCell{}, invalidf("cell", format, args...)
	}
	switch c.has {
	case cellFormula, cellFormula | cellValue:
		if len(c.formula) < 2 || c.formula[0] != '=' {
			return bad("a formula starts with \"=\" and has more characters")
		}
		return agentCell{formula: c.formula}, nil
	case cellDate:
		d, ok := formula.ParseDate(c.date)
		if !ok {
			return bad("%q is no date written as yyyy-mm-dd", c.date)
		}
		return agentCell{content: formula.Content{Kind: formula.ContentDate, Num: float64(d)}}, nil
	case cellTime:
		frac, ok := parseClock(c.time)
		if !ok {
			return bad("%q is no time written as hh:mm:ss", c.time)
		}
		return agentCell{content: formula.Content{Kind: formula.ContentTime, Num: frac}}, nil
	case cellDateTime:
		day, clock, found := strings.Cut(c.datetime, "T")
		d, ok := formula.ParseDate(day)
		frac, okClock := parseClock(clock)
		if !found || !ok || !okClock {
			return bad("%q is no date with time written as yyyy-mm-ddThh:mm:ss", c.datetime)
		}
		return agentCell{content: formula.Content{Kind: formula.ContentDateTime, Num: float64(d) + frac}}, nil
	}
	return bad("an object is a date, a time, a date with time or a formula object")
}

// check reads what the operation holds that its shape cannot say, and
// adds the cells it writes to cells.
func (op *AgentOp) check(cells *int) *WriteError {
	if op.Op != "add_tab" && op.Tab == "" {
		return invalidf("tab", "the operation names no tab")
	}
	switch op.Op {
	case "write", "import_csv":
		r, perr := formula.ParseRange(op.atText)
		if op.atIsNum || perr != nil || !r.Single() || r.Tab != "" {
			return invalidf("at", "at is a cell address such as \"A1\"")
		}
		op.at = r.First
		if op.Op == "import_csv" {
			op.fields = parseCSV(op.csv)
			for _, line := range op.fields {
				*cells += len(line)
			}
			return nil
		}
		if !op.rowsList {
			return invalidf("rows", "rows is a list of rows of cells")
		}
	case "clear", "format":
		if len(op.Ranges) == 0 {
			return invalidf("ranges", "the operation names no range")
		}
		for _, text := range op.Ranges {
			r, perr := formula.ParseRange(text)
			if perr != nil || r.Tab != "" {
				return invalidf("ranges", "%q is not a range of the operation's tab", text)
			}
			op.ranges = append(op.ranges, r)
		}
	case "widths":
		r, perr := formula.ParseRange(op.Cols)
		if perr != nil || !r.FullCols || r.Tab != "" {
			return invalidf("cols", "cols names whole columns such as \"B:D\"")
		}
		op.ranges = []formula.Range{r}
		if op.Width < minWidth || op.Width > maxWidth {
			return invalidf("width", "a width is 40 to 1,000 pixels")
		}
	case "insert_rows", "delete_rows":
		if !op.atIsNum || op.atNum < 1 || op.atNum > formula.MaxRows+1 {
			return invalidf("at", "at is a row number")
		}
		if err := checkCount(op, "rows", formula.MaxRows); err != nil {
			return err
		}
		op.at.Row = int32(op.atNum - 1)
	case "insert_cols", "delete_cols":
		r, perr := formula.ParseRange(op.atText + ":" + op.atText)
		if op.atIsNum || perr != nil || !r.FullCols {
			return invalidf("at", "at is the letters of a column")
		}
		if err := checkCount(op, "columns", formula.MaxCols); err != nil {
			return err
		}
		op.at.Col = r.First.Col
	case "sort":
		r, perr := formula.ParseRange(op.rowsText)
		if op.rowsList || perr != nil || !r.FullRows || r.Tab != "" {
			return invalidf("rows", "rows names whole rows such as \"2:200\"")
		}
		by, perr := formula.ParseRange(op.By + ":" + op.By)
		if perr != nil || !by.FullCols {
			return invalidf("by", "by is the letters of a column")
		}
		r.First.Col = by.First.Col
		op.ranges = []formula.Range{r}
	case "add_tab":
		if op.has&agentBit(cOpAt) != 0 {
			if !op.atIsNum || op.atNum < 1 {
				return invalidf("at", "at is a position from 1")
			}
			op.place = countOf(op.atNum)
		}
		if !validTabName(op.Name) {
			return &WriteError{Code: "invalid", Reason: "tab_name", Name: op.Name}
		}
	case "rename_tab":
		if !validTabName(op.Name) {
			return &WriteError{Code: "invalid", Reason: "tab_name", Name: op.Name}
		}
	case "move_tab":
		if *op.To < 1 {
			return invalidf("to", "to is a position from 1")
		}
	case "tab_settings":
		if op.FrozenRows == nil && op.FrozenCols == nil {
			return invalidf("settings", "tab_settings sets frozen_rows or frozen_cols")
		}
		if err := checkFrozen(op.FrozenRows, op.FrozenCols); err != nil {
			return err
		}
	}
	return nil
}

// appendJSON writes the call so that it reads back as the same call.
func (u *Update) appendJSON(dst []byte) []byte { return u.appendCall(dst, false) }

// appendCall writes the call. With request set it writes the request the
// call makes of its sheet, in the one form the hash is taken over that
// tells a repeated call from another call with its op_id. A request has
// one form however the call spells it and whichever interface carries it.
// It leaves out slug, which names the sheet for the tool and which the
// other interfaces name outside the call, and op_id, the key the hash is
// stored under. A field at the value it has when it is left out is left
// out: an empty state or message, and a field of the guard or a flag of an
// operation that is false. A guard without fields stays, because a call
// with a guard needs a state.
func (u *Update) appendCall(dst []byte, request bool) []byte {
	dst = append(dst, '{')
	for _, f := range [...]struct {
		bit  uint8
		name string
		text string
	}{{callSlug, "slug", u.Slug}, {callState, "state", u.State}, {callOpID, "op_id", u.OpID}, {callMessage, "message", u.Message}} {
		keep := u.has&f.bit != 0
		if request {
			keep = f.text != "" && f.bit&(callState|callMessage) != 0
		}
		if keep {
			dst = appendString(appendKey(dst, f.name, false), f.text)
			dst = append(dst, ',')
		}
	}
	if g := u.Guard; g != nil {
		unchanged, ranges := g.unchanged, g.ranges
		if request {
			unchanged, ranges = g.Unchanged, len(g.Ranges) > 0
		}
		dst = append(dst, `"guard":{`...)
		if unchanged {
			dst = strconv.AppendBool(append(dst, `"unchanged":`...), g.Unchanged)
		}
		if ranges {
			dst = appendStrings(appendKey(dst, "ranges", unchanged), g.Ranges)
		}
		dst = append(dst, '}', ',')
	}
	dst = append(dst, `"ops":[`...)
	for i := range u.Ops {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = u.Ops[i].appendJSON(dst, request)
	}
	return append(dst, ']', '}')
}

func appendStrings(dst []byte, list []string) []byte {
	dst = append(dst, '[')
	for i, s := range list {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = appendString(dst, s)
	}
	return append(dst, ']')
}

// appendJSON writes the operation: as it reads back, or as the request it
// makes (appendCall).
func (op *AgentOp) appendJSON(dst []byte, request bool) []byte {
	dst = appendString(append(dst, `{"op":`...), op.Op)
	for at := 1; at < len(agentOpMembers); at++ {
		slot := cOpKind + at
		if op.has&agentBit(slot) == 0 {
			continue
		}
		if request && (slot == cOpFormats && !op.Formats || slot == cOpDesc && !op.Desc) {
			continue
		}
		dst = appendKey(dst, agentOpMembers[at].name, true)
		switch slot {
		case cOpTab:
			dst = appendString(dst, op.Tab)
		case cOpAt:
			if op.atIsNum {
				dst = strconv.AppendInt(dst, op.atNum, 10)
			} else {
				dst = appendString(dst, op.atText)
			}
		case cOpRows:
			if !op.rowsList {
				dst = appendString(dst, op.rowsText)
				break
			}
			dst = append(dst, '[')
			for i, line := range op.values {
				if i > 0 {
					dst = append(dst, ',')
				}
				dst = append(dst, '[')
				for j := range line {
					if j > 0 {
						dst = append(dst, ',')
					}
					dst = line[j].appendJSON(dst)
				}
				dst = append(dst, ']')
			}
			dst = append(dst, ']')
		case cOpRanges:
			dst = appendStrings(dst, op.Ranges)
		case cOpFormats:
			dst = strconv.AppendBool(dst, op.Formats)
		case cOpFormat:
			dst = appendPatch(dst, op.patch, nil)
		case cOpCols:
			dst = appendString(dst, op.Cols)
		case cOpWidth:
			dst = strconv.AppendInt(dst, int64(op.Width), 10)
		case cOpCount:
			dst = strconv.AppendInt(dst, int64(op.Count), 10)
		case cOpBy:
			dst = appendString(dst, op.By)
		case cOpDesc:
			dst = strconv.AppendBool(dst, op.Desc)
		case cOpName:
			dst = appendString(dst, op.Name)
		case cOpTo:
			dst = strconv.AppendInt(dst, int64(*op.To), 10)
		case cOpFrozenRows:
			dst = strconv.AppendInt(dst, int64(*op.FrozenRows), 10)
		case cOpFrozenCols:
			dst = strconv.AppendInt(dst, int64(*op.FrozenCols), 10)
		case cOpCSV:
			dst = appendString(dst, op.csv)
		}
	}
	return append(dst, '}')
}

// appendJSON writes the cell in the form that reads back as the same
// cell: text that would read as a formula or lose an apostrophe gets an
// apostrophe in front, as a read returns it.
func (c *agentCell) appendJSON(dst []byte) []byte {
	k := c.content
	switch {
	case c.formula != "":
		return appendString(dst, c.formula)
	case k.Kind == formula.ContentNumber:
		return strconv.AppendFloat(dst, k.Num, 'g', -1, 64)
	case k.Kind == formula.ContentBool:
		return strconv.AppendBool(dst, k.Num != 0)
	case k.Kind == formula.ContentText:
		if strings.HasPrefix(k.Text, "=") || strings.HasPrefix(k.Text, "'") {
			dst = appendString(dst, "'"+k.Text)
		} else {
			dst = appendString(dst, k.Text)
		}
		return dst
	case k.Kind == formula.ContentDate:
		dst = append(dst, `{"date":"`...)
		dst = append(dst, formula.Date(math.Floor(k.Num)).String()...)
		return append(dst, '"', '}')
	case k.Kind == formula.ContentTime:
		dst = appendClock(append(dst, `{"time":"`...), k.Num)
		return append(dst, '"', '}')
	case k.Kind == formula.ContentDateTime:
		dst = append(dst, `{"datetime":"`...)
		dst = append(dst, formula.Date(math.Floor(k.Num)).String()...)
		dst = appendClock(append(dst, 'T'), k.Num)
		return append(dst, '"', '}')
	}
	return append(dst, "null"...)
}

// checkCount checks the count of an insert or a delete of rows or columns.
// An insert of more than a tab holds passes the limit of that name, and a
// delete of that many names rows or columns the tab does not have.
func checkCount(op *AgentOp, limit string, max int) *WriteError {
	switch {
	case op.Count < 1:
		return invalidf("count", "count is the number of %s, from 1", limit)
	case op.Count <= max:
		return nil
	case op.Op == "insert_rows" || op.Op == "insert_cols":
		return capacity(limit, int64(max), int64(op.Count))
	}
	return invalidf("count", "the %s to delete are outside the tab", limit)
}

// checkFrozen checks the frozen counts a settings operation names.
func checkFrozen(rows, cols *int) *WriteError {
	if rows != nil && (*rows < 0 || *rows > maxFrozenRows) {
		return invalidf("frozen_rows", "frozen rows are 0 to %d", maxFrozenRows)
	}
	if cols != nil && (*cols < 0 || *cols > maxFrozenCols) {
		return invalidf("frozen_cols", "frozen columns are 0 to %d", maxFrozenCols)
	}
	return nil
}

// parseClock reads hh:mm:ss or hh:mm, two digits each, as a fraction of a
// day.
func parseClock(s string) (float64, bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return 0, false
	}
	var v [3]int
	for i, p := range parts {
		if len(p) != 2 || p[0] < '0' || p[0] > '9' || p[1] < '0' || p[1] > '9' {
			return 0, false
		}
		v[i] = int(p[0]-'0')*10 + int(p[1]-'0')
	}
	if v[0] > 23 || v[1] > 59 || v[2] > 59 {
		return 0, false
	}
	return float64(v[0]*3600+v[1]*60+v[2]) / 86400, true
}

// appendClock appends the time of day of a day number as hh:mm:ss.
func appendClock(dst []byte, num float64) []byte {
	secs := int(math.Round((num - math.Floor(num)) * 86400))
	if secs >= 86400 {
		secs = 86399
	}
	for i, v := range [3]int{secs / 3600, secs / 60 % 60, secs % 60} {
		if i > 0 {
			dst = append(dst, ':')
		}
		dst = append(dst, byte('0'+v/10), byte('0'+v%10))
	}
	return dst
}

// parseCSV splits CSV text into rows of fields by RFC 4180. Lines end with
// CR LF or LF, and an empty line is a row of one empty field.
func parseCSV(text string) [][]string {
	var rows [][]string
	var row []string
	var field []byte
	text = strings.TrimPrefix(text, csvBOM)
	quoted, wasQuoted := false, false
	for i := 0; i < len(text); i++ {
		ch := text[i]
		switch {
		case quoted && ch == '"' && i+1 < len(text) && text[i+1] == '"':
			field = append(field, '"')
			i++
		case quoted && ch == '"':
			quoted = false
		case quoted:
			field = append(field, ch)
		case ch == '"' && len(field) == 0 && !wasQuoted:
			quoted, wasQuoted = true, true
		case ch == ',':
			row, field, wasQuoted = append(row, string(field)), field[:0], false
		case ch == '\n' || (ch == '\r' && i+1 < len(text) && text[i+1] == '\n'):
			if ch == '\r' {
				i++
			}
			rows, row, field, wasQuoted = append(rows, append(row, string(field))), nil, field[:0], false
		default:
			field = append(field, ch)
		}
	}
	if len(field) > 0 || len(row) > 0 || wasQuoted {
		rows = append(rows, append(row, string(field)))
	}
	return rows
}
