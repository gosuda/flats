package sheet

import (
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// The live protocol is version 1 of Appendix A of the technical design.
// Every frame is a JSON object with a "type" field that names the frame.
const (
	protocolVersion = 1
	clientFrameMax  = 256 << 10
	serverFrameMax  = 512 << 10
	partDataMax     = serverFrameMax - frameSlack // bytes of the fields of a delta_part
	actionMaxBytes  = 8 << 20
	actionMaxParts  = 32
	actionPartTTL   = 30 * time.Second
	snapshotRows    = 2000
)

// pair is a list of exactly two numbers.
type pair[T int32 | uint64] [2]T

// Slots of the client frames and of the operations of an action: what
// walk tells a sink with each value.
const (
	sFrame = iota + 1
	sType
	sVersion
	sSession
	sWatermark
	sTab
	sName
	sPending
	sGeneration
	sN
	sBase
	sOps
	sPart
	sParts
	sData
	sID
	sRow
	sCol
	sText
	sAt
	sAtItem
	sThrough
	sActive
	sActiveItem
	sRange
	sRangeRows
	sRangeCols
	sEditing

	// An operation and its members, in the order of opMembers: a member's
	// bit in personOp.has is its distance from sOpKind.
	sOp
	sOpKind
	sOpTab
	sOpRow
	sOpCol
	sOpText
	sOpAt
	sOpRows
	sOpCols
	sOpIDs
	sOpValues
	sOpSet
	sOpWidth
	sOpFrom
	sOpTo
	sOpSource
	sOpTarget
	sOpMode
	sOpCut
	sOpBeside
	sOpSide
	sOpCount
	sOpBy
	sOpDesc
	sOpFrozenRows
	sOpFrozenCols
	sOpName
	sOpBefore

	sOpAtItem
	sOpValuesRow
	sOpValue
	sRectTab
	sRectRows
	sRectCols

	// Inside Ids or Target[].
	sIDOne
	sIDRun
	sIDRunItem
	sIDNew
	sIDNewCount

	// Inside a number format.
	sNumKind
	sNumSymbol
	sNumDecimals
	sNumThousands
	sNumPattern
	sNumDate
	sNumTime

	// The properties of a format: sProp plus the property.
	sProp
	sPropEnd = sProp + propCount
)

// idsShape is Ids or Target[] of Appendix A: "all", or a list whose items
// are an id, a run [first, count] or {"new": k}. The number of items of a
// run and every value are checked where the list is used.
func idsShape(slot int) *shape {
	return oneOf(aText(slot), aList(slot, oneOf(
		anInt(sIDOne),
		aList(sIDRun, anInt(sIDRunItem)),
		anObject(sIDNew, need("new", anInt(sIDNewCount))))))
}

// numberShape is a number format: each kind takes exactly its own fields.
var numberShape = aVariant(sProp+propNumber, "kind", false, []member{
	need("kind", aText(sNumKind)), may("symbol", aText(sNumSymbol)), may("decimals", anInt(sNumDecimals)),
	may("thousands", aFlag(sNumThousands)), may("pattern", aText(sNumPattern)), may("date", aText(sNumDate)), may("time", aText(sNumTime)),
}, map[string][2]string{
	"general": {}, "text": {}, "number": {"decimals thousands"}, "percent": {"decimals"}, "currency": {"symbol decimals"},
	"date": {"pattern"}, "time": {"pattern"}, "datetime": {"date time"},
})

// patchShape is the set of a format operation and the format of an agent
// call: the named properties, each of which null resets.
func patchShape(slot int) *shape {
	members := make([]member, propCount)
	for prop, name := range propNames {
		var val *shape
		switch prop {
		case propTextColor, propFill, propAlign:
			val = aText(sProp + prop)
		case propNumber:
			val = numberShape
		default:
			val = aFlag(sProp + prop)
		}
		members[prop] = may(name, orNull(val))
	}
	return anObject(slot, members...)
}

// runsShape is Ids of Appendix A as a frame carries it: "all", or runs
// [first, count]. A run of a frame is a list of a fixed length, unlike a
// run of an operation, which refuses its operation when it is another.
func runsShape(slot int) *shape {
	return oneOf(aText(slot), aList(slot, aTuple(sIDRun, 2, anInt(sIDRunItem))))
}

// The rows and the columns of a range of a copy or a fill, and its tab. A
// copy names the tab of each range; a fill has one tab.
var rectMembers = []member{need("tab", anInt(sRectTab)), need("rows", idsShape(sRectRows)), need("cols", idsShape(sRectCols))}

func rectShape(slot int, tab bool) *shape {
	if tab {
		return anObject(slot, rectMembers...)
	}
	return anObject(slot, rectMembers[1:]...)
}

// opMembers are the members the operations of an action carry between
// them (section 7.1), and opShape gives each kind its own: the ones it
// needs and the ones it may leave out. An operation of a kind the list
// does not have is read and refused as invalid when it is applied.
var (
	opMembers = []member{
		need("op", aText(sOpKind)),
		may("tab", anInt(sOpTab)), may("row", anInt(sOpRow)), may("col", anInt(sOpCol)),
		may("text", aText(sOpText)), may("at", aTuple(sOpAt, 2, anInt(sOpAtItem))),
		may("rows", idsShape(sOpRows)), may("cols", idsShape(sOpCols)), may("ids", idsShape(sOpIDs)),
		may("values", aList(sOpValues, aList(sOpValuesRow, aText(sOpValue)))),
		may("set", patchShape(sOpSet)), may("width", anInt(sOpWidth)),
		may("from", rectShape(sOpFrom, true)), may("to", rectShape(sOpTo, true)),
		may("source", rectShape(sOpSource, false)), may("target", rectShape(sOpTarget, false)),
		may("mode", aText(sOpMode)), may("cut", aFlag(sOpCut)),
		may("beside", anInt(sOpBeside)), may("side", aText(sOpSide)), may("count", anInt(sOpCount)),
		may("by", anInt(sOpBy)), may("desc", aFlag(sOpDesc)),
		may("frozen_rows", anInt(sOpFrozenRows)), may("frozen_cols", anInt(sOpFrozenCols)),
		may("name", aText(sOpName)), may("before", orNull(anInt(sOpBefore))),
	}
	opKinds = map[string][2]string{
		"set":         {"tab row col text", "at"},
		"write":       {"tab rows cols values"},
		"clear":       {"tab rows cols"},
		"format":      {"tab rows cols set"},
		"width":       {"tab cols width"},
		"copy":        {"from to mode", "cut"},
		"fill":        {"tab source target"},
		"insert_rows": {"tab beside side count"},
		"insert_cols": {"tab beside side count"},
		"append_rows": {"tab count"},
		"append_cols": {"tab count"},
		"delete_rows": {"tab ids"},
		"delete_cols": {"tab ids"},
		"sort":        {"tab rows by", "desc"},
		"settings":    {"tab frozen_rows frozen_cols"},
		"add_tab":     {"", "name before"},
		"rename_tab":  {"tab name"},
		"move_tab":    {"tab", "before"},
		"delete_tab":  {"tab"},
		"undo":        {},
		"redo":        {},
	}
	opShape = aVariant(sOp, "op", true, opMembers, opKinds)
)

// frameShape is every client frame of Appendix A. A frame names its kind
// in type, and a kind takes its own members.
var (
	frameKinds = map[string][2]string{
		"hello":       {"version pending", "session watermark tab name"},
		"watch":       {"generation tab"},
		"action":      {"n base ops"},
		"action_part": {"n part parts data"},
		"peek":        {"id tab row col"},
		"rebase":      {"id tab row col text at base through"},
		"presence":    {"tab", "active range editing"},
	}
	frameShape = aVariant(sFrame, "type", false, []member{
		need("type", aText(sType)),
		may("version", anInt(sVersion)), may("session", aText(sSession)), may("watermark", aText(sWatermark)),
		may("tab", anInt(sTab)), may("name", aText(sName)), may("pending", anInt(sPending)),
		may("generation", anInt(sGeneration)),
		may("n", aText(sN)), may("base", aText(sBase)), may("ops", aList(sOps, opShape)),
		may("part", anInt(sPart)), may("parts", anInt(sParts)), may("data", aText(sData)),
		may("id", anInt(sID)), may("row", anInt(sRow)), may("col", anInt(sCol)),
		may("text", aText(sText)), may("at", aTuple(sAt, 2, anInt(sAtItem))), may("through", aText(sThrough)),
		may("active", aTuple(sActive, 2, anInt(sActiveItem))),
		may("range", anObject(sRange, need("rows", runsShape(sRangeRows)), need("cols", runsShape(sRangeCols)))),
		may("editing", aFlag(sEditing)),
	}, frameKinds)
)

// The bodies of the History routes that change the sheet (Appendix A),
// by route. Their slots are their own: no other shape is read with them.
const (
	sHistory = iota + 1
	sHistoryRevision
	sHistoryTab
	sHistoryPinned
	sHistorySession
)

var historyShapes = map[string]*shape{
	"pin":      anObject(sHistory, need("revision", anInt(sHistoryRevision)), need("pinned", aFlag(sHistoryPinned))),
	"rollback": anObject(sHistory, need("revision", anInt(sHistoryRevision)), may("session", aText(sHistorySession))),
	"recover":  anObject(sHistory, need("revision", anInt(sHistoryRevision)), need("tab", anInt(sHistoryTab)), may("session", aText(sHistorySession))),
}

type historyBody struct {
	frameCheck
	Revision int
	Tab      uint64
	Pinned   bool
	Session  string
}

func (b *historyBody) integer(slot int, n int64) {
	switch slot {
	case sHistoryRevision:
		b.Revision = countOf(n)
	case sHistoryTab:
		b.Tab = b.id(n)
	}
}

func (b *historyBody) flag(slot int, on bool) {
	if slot == sHistoryPinned {
		b.Pinned = on
	}
}

func (b *historyBody) text(slot int, t []byte) {
	if slot == sHistorySession {
		b.Session = string(t)
	}
}

var errFrame = errors.New("sheet: the frame is not one of Appendix A")

// validID reports whether n is an id: a positive integer below 2^53.
func validID(n uint64) bool { return n > 0 && n < maxID }

// frameCheck collects what a frame holds that its shape cannot say: a
// value outside the values of its field. In a frame any such value is a
// protocol error.
type frameCheck struct {
	noSink
	bad bool
}

// id reads an id of a frame.
func (c *frameCheck) id(n int64) uint64 {
	if n <= 0 || n >= maxID {
		c.bad = true
		return 0
	}
	return uint64(n)
}

// whole reads an integer that is no id: one a JSON number holds exactly.
func (c *frameCheck) whole(n int64) int {
	if n <= -maxID || n >= maxID {
		c.bad = true
		return 0
	}
	return int(n)
}

// index reads a row or column index: a position from 0.
func (c *frameCheck) index(n int64) int32 {
	if n < 0 || n > math.MaxInt32 {
		c.bad = true
		return 0
	}
	return int32(n)
}

// sequence reads a sequence, which is a decimal string.
func (c *frameCheck) sequence(b []byte) string {
	s := string(b)
	if _, decimal := parseDecimal(s); !decimal {
		c.bad = true
	}
	return s
}

// readFrame reads data as the client frame of the kind want into its
// sink. It is the one reading of a frame: the walk checks the bytes
// against Appendix A and hands the sink the values it checked.
func readFrame(data []byte, want string, to sink, check *frameCheck) error {
	kind, fault := walk(data, frameShape, to)
	if fault != nil || kind != want || check.bad {
		return errFrame
	}
	return nil
}

// The client frames. Each reads itself with readFrame in UnmarshalJSON, so
// that every way a frame is decoded, encoding/json included, is that one
// reading, and writes itself back with appendJSON in the form that reads
// as the same value.

type helloFrame struct {
	frameCheck
	Type      string
	Version   int
	Session   string
	Watermark string
	Tab       uint64
	Name      string
	Pending   int

	has uint8 // the optional members the frame carries
}

const (
	helloSession = 1 << iota
	helloWatermark
	helloTab
	helloName
)

func (f *helloFrame) UnmarshalJSON(data []byte) error {
	*f = helloFrame{Type: "hello"}
	return readFrame(data, f.Type, f, &f.frameCheck)
}

func (f *helloFrame) text(slot int, b []byte) {
	switch slot {
	case sSession:
		f.Session, f.has = string(b), f.has|helloSession
	case sWatermark:
		f.Watermark, f.has = f.sequence(b), f.has|helloWatermark
	case sName:
		f.Name, f.has = string(b), f.has|helloName
	}
}

func (f *helloFrame) integer(slot int, n int64) {
	switch slot {
	case sVersion:
		f.Version = f.whole(n)
	case sTab:
		f.Tab, f.has = f.id(n), f.has|helloTab
	case sPending:
		f.Pending = f.whole(n)
	}
}

func (f *helloFrame) appendJSON(dst []byte) []byte {
	dst = append(dst, `{"type":"hello","version":`...)
	dst = strconv.AppendInt(dst, int64(f.Version), 10)
	if f.has&helloSession != 0 {
		dst = appendString(append(dst, `,"session":`...), f.Session)
	}
	if f.has&helloWatermark != 0 {
		dst = appendString(append(dst, `,"watermark":`...), f.Watermark)
	}
	if f.has&helloTab != 0 {
		dst = strconv.AppendUint(append(dst, `,"tab":`...), f.Tab, 10)
	}
	if f.has&helloName != 0 {
		dst = appendString(append(dst, `,"name":`...), f.Name)
	}
	dst = strconv.AppendInt(append(dst, `,"pending":`...), int64(f.Pending), 10)
	return append(dst, '}')
}

type watchFrame struct {
	frameCheck
	Type       string
	Generation int
	Tab        uint64
}

func (f *watchFrame) UnmarshalJSON(data []byte) error {
	*f = watchFrame{Type: "watch"}
	return readFrame(data, f.Type, f, &f.frameCheck)
}

func (f *watchFrame) integer(slot int, n int64) {
	switch slot {
	case sGeneration:
		f.Generation = f.whole(n)
	case sTab:
		f.Tab = f.id(n)
	}
}

func (f *watchFrame) appendJSON(dst []byte) []byte {
	dst = strconv.AppendInt(append(dst, `{"type":"watch","generation":`...), int64(f.Generation), 10)
	dst = strconv.AppendUint(append(dst, `,"tab":`...), f.Tab, 10)
	return append(dst, '}')
}

type actionFrame struct {
	Type string
	N    string
	Base string
	Ops  []personOp
}

// actionReader builds an action from the parts of its frame: its own
// fields here, and the operations in the opReader.
type actionReader struct {
	frameCheck
	opReader
	f *actionFrame
}

func (f *actionFrame) UnmarshalJSON(data []byte) error {
	*f = actionFrame{Type: "action"}
	r := actionReader{f: f}
	err := readFrame(data, f.Type, &r, &r.frameCheck)
	f.Ops = r.ops
	return err
}

func (r *actionReader) text(slot int, b []byte) {
	switch slot {
	case sN:
		r.f.N = r.sequence(b)
	case sBase:
		r.f.Base = r.sequence(b)
	default:
		r.opReader.text(slot, b)
	}
}

func (f *actionFrame) appendJSON(dst []byte) []byte {
	dst = appendString(append(dst, `{"type":"action","n":`...), f.N)
	dst = appendString(append(dst, `,"base":`...), f.Base)
	dst = append(dst, `,"ops":[`...)
	for i := range f.Ops {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = f.Ops[i].appendJSON(dst)
	}
	return append(dst, ']', '}')
}

type actionPartFrame struct {
	frameCheck
	Type  string
	N     string
	Part  int
	Parts int
	Data  string
}

func (f *actionPartFrame) UnmarshalJSON(data []byte) error {
	*f = actionPartFrame{Type: "action_part"}
	return readFrame(data, f.Type, f, &f.frameCheck)
}

func (f *actionPartFrame) text(slot int, b []byte) {
	switch slot {
	case sN:
		f.N = f.sequence(b)
	case sData:
		f.Data = string(b)
	}
}

func (f *actionPartFrame) integer(slot int, n int64) {
	switch slot {
	case sPart:
		f.Part = f.whole(n)
	case sParts:
		f.Parts = f.whole(n)
	}
}

func (f *actionPartFrame) appendJSON(dst []byte) []byte {
	dst = appendString(append(dst, `{"type":"action_part","n":`...), f.N)
	dst = strconv.AppendInt(append(dst, `,"part":`...), int64(f.Part), 10)
	dst = strconv.AppendInt(append(dst, `,"parts":`...), int64(f.Parts), 10)
	dst = appendString(append(dst, `,"data":`...), f.Data)
	return append(dst, '}')
}

type peekFrame struct {
	frameCheck
	Type string
	ID   int64
	Tab  uint64
	Row  uint64
	Col  uint64
}

func (f *peekFrame) UnmarshalJSON(data []byte) error {
	*f = peekFrame{Type: "peek"}
	return readFrame(data, f.Type, f, &f.frameCheck)
}

func (f *peekFrame) integer(slot int, n int64) {
	switch slot {
	case sID:
		f.ID = int64(f.whole(n))
	case sTab:
		f.Tab = f.id(n)
	case sRow:
		f.Row = f.id(n)
	case sCol:
		f.Col = f.id(n)
	}
}

func (f *peekFrame) appendJSON(dst []byte) []byte {
	dst = strconv.AppendInt(append(dst, `{"type":"peek","id":`...), f.ID, 10)
	dst = strconv.AppendUint(append(dst, `,"tab":`...), f.Tab, 10)
	dst = strconv.AppendUint(append(dst, `,"row":`...), f.Row, 10)
	dst = strconv.AppendUint(append(dst, `,"col":`...), f.Col, 10)
	return append(dst, '}')
}

type rebaseFrame struct {
	frameCheck
	Type    string
	ID      int64
	Tab     uint64
	Row     uint64
	Col     uint64
	Text    string
	At      pair[int32]
	Base    string
	Through string

	at int // the items of at read so far
}

func (f *rebaseFrame) UnmarshalJSON(data []byte) error {
	*f = rebaseFrame{Type: "rebase"}
	return readFrame(data, f.Type, f, &f.frameCheck)
}

func (f *rebaseFrame) text(slot int, b []byte) {
	switch slot {
	case sText:
		f.Text = string(b)
	case sBase:
		f.Base = f.sequence(b)
	case sThrough:
		f.Through = f.sequence(b)
	}
}

func (f *rebaseFrame) integer(slot int, n int64) {
	switch slot {
	case sID:
		f.ID = int64(f.whole(n))
	case sTab:
		f.Tab = f.id(n)
	case sRow:
		f.Row = f.id(n)
	case sCol:
		f.Col = f.id(n)
	case sAtItem:
		// The shape holds at to two items.
		f.At[f.at&1], f.at = f.index(n), f.at+1
	}
}

func (f *rebaseFrame) appendJSON(dst []byte) []byte {
	dst = strconv.AppendInt(append(dst, `{"type":"rebase","id":`...), f.ID, 10)
	dst = strconv.AppendUint(append(dst, `,"tab":`...), f.Tab, 10)
	dst = strconv.AppendUint(append(dst, `,"row":`...), f.Row, 10)
	dst = strconv.AppendUint(append(dst, `,"col":`...), f.Col, 10)
	dst = appendString(append(dst, `,"text":`...), f.Text)
	dst = strconv.AppendInt(append(dst, `,"at":[`...), int64(f.At[0]), 10)
	dst = strconv.AppendInt(append(dst, ','), int64(f.At[1]), 10)
	dst = appendString(append(dst, `],"base":`...), f.Base)
	dst = appendString(append(dst, `,"through":`...), f.Through)
	return append(dst, '}')
}

type presenceFrame struct {
	Type    string
	Tab     uint64
	Active  *[2]uint64
	Range   *idRange
	Editing bool

	editing bool // the frame carries editing
}

// idRange is the range of a presence frame: the rows and the columns.
type idRange struct{ Rows, Cols idList }

type presenceReader struct {
	frameCheck
	idsReader
	f      *presenceFrame
	active int
}

func (f *presenceFrame) UnmarshalJSON(data []byte) error {
	*f = presenceFrame{Type: "presence"}
	r := presenceReader{f: f}
	return readFrame(data, f.Type, &r, &r.frameCheck)
}

// A text of a range other than "all", and a run whose first is no id or
// whose count names none, are values outside the values of their field.
// What the runs name in the sheet is checked where the range is used.
func (r *presenceReader) text(slot int, b []byte) {
	switch slot {
	case sRangeRows:
		r.f.Range.Rows.word(b)
		r.bad = r.bad || r.f.Range.Rows.bad
	case sRangeCols:
		r.f.Range.Cols.word(b)
		r.bad = r.bad || r.f.Range.Cols.bad
	}
}

func (r *presenceReader) begin(slot int) {
	switch slot {
	case sActive:
		r.f.Active = new([2]uint64)
	case sRange:
		r.f.Range = new(idRange)
	case sRangeRows:
		r.list = &r.f.Range.Rows
	case sRangeCols:
		r.list = &r.f.Range.Cols
	default:
		r.idsReader.begin(slot)
	}
}

func (r *presenceReader) end(slot int) { r.idsReader.end(slot) }

func (r *presenceReader) integer(slot int, n int64) {
	switch slot {
	case sTab:
		r.f.Tab = r.id(n)
	case sActiveItem:
		r.f.Active[r.active&1], r.active = r.id(n), r.active+1
	case sIDRunItem:
		if r.run == 0 {
			r.id(n)
		} else if r.whole(n) < 1 {
			r.bad = true
		}
		r.idsReader.integer(slot, n)
	default:
		r.idsReader.integer(slot, n)
	}
}

func (r *presenceReader) flag(slot int, on bool) {
	if slot == sEditing {
		r.f.Editing, r.f.editing = on, true
	}
}

func (f *presenceFrame) appendJSON(dst []byte) []byte {
	dst = strconv.AppendUint(append(dst, `{"type":"presence","tab":`...), f.Tab, 10)
	if f.Active != nil {
		dst = strconv.AppendUint(append(dst, `,"active":[`...), f.Active[0], 10)
		dst = strconv.AppendUint(append(dst, ','), f.Active[1], 10)
		dst = append(dst, ']')
	}
	if f.Range != nil {
		dst = f.Range.Rows.appendJSON(append(dst, `,"range":{"rows":`...))
		dst = f.Range.Cols.appendJSON(append(dst, `,"cols":`...))
		dst = append(dst, '}')
	}
	if f.editing {
		dst = strconv.AppendBool(append(dst, `,"editing":`...), f.Editing)
	}
	return append(dst, '}')
}

// connFrame is a frame of an open connection as its one reading left it:
// the kind the walk found in type, and the frame of that kind.
type connFrame struct {
	kind     string
	hello    helloFrame
	watch    watchFrame
	peek     peekFrame
	action   actionFrame
	part     actionPartFrame
	rebase   rebaseFrame
	presence presenceFrame
}

// connReader builds the frame of every kind at once. A frame names its
// kind in type, which may stand after the fields the kind decides about,
// so the walk cannot hand its parts to one reader. Each reader takes the
// parts of its own kind and passes over the others, as it does when it
// reads a frame alone. The walk refuses a frame that holds a field of
// another kind, so the reader of the kind it reports was told that
// frame's own fields and nothing else.
type connReader struct {
	f        *connFrame
	action   actionReader
	presence presenceReader
}

func (r *connReader) begin(slot int) {
	r.action.begin(slot)
	r.presence.begin(slot)
}

func (r *connReader) end(slot int) {
	r.action.end(slot)
	r.presence.end(slot)
}

func (r *connReader) text(slot int, b []byte) {
	r.f.hello.text(slot, b)
	r.f.part.text(slot, b)
	r.f.rebase.text(slot, b)
	r.action.text(slot, b)
	r.presence.text(slot, b)
}

func (r *connReader) integer(slot int, n int64) {
	r.f.hello.integer(slot, n)
	r.f.watch.integer(slot, n)
	r.f.peek.integer(slot, n)
	r.f.part.integer(slot, n)
	r.f.rebase.integer(slot, n)
	r.action.integer(slot, n)
	r.presence.integer(slot, n)
}

func (r *connReader) flag(slot int, on bool) {
	r.action.flag(slot, on)
	r.presence.flag(slot, on)
}

func (r *connReader) null(slot int) { r.action.null(slot) }

// No frame holds a number that is no integer.
func (r *connReader) number(int, float64) {}

// readConnFrame reads a frame of an open connection. It is the one
// reading of the frame. The same walk checks the bytes against Appendix A,
// finds the kind and builds the frame, so what a connection may send is
// decided from the kind the frame was read as. A hello is read here as
// the frame it is, and has no place on an open connection.
func readConnFrame(data []byte) (*connFrame, error) {
	f := &connFrame{
		hello: helloFrame{Type: "hello"},
		watch: watchFrame{Type: "watch"}, peek: peekFrame{Type: "peek"}, action: actionFrame{Type: "action"},
		part: actionPartFrame{Type: "action_part"}, rebase: rebaseFrame{Type: "rebase"}, presence: presenceFrame{Type: "presence"},
	}
	r := connReader{f: f}
	r.action.f, r.presence.f = &f.action, &f.presence
	kind, fault := walk(data, frameShape, &r)
	if fault != nil {
		return nil, errFrame
	}
	f.kind = kind
	bad := false
	switch kind {
	case "hello":
		bad = f.hello.bad
	case "watch":
		bad = f.watch.bad
	case "peek":
		bad = f.peek.bad
	case "action":
		f.action.Ops, bad = r.action.ops, r.action.bad
	case "action_part":
		bad = f.part.bad
	case "rebase":
		bad = f.rebase.bad
	case "presence":
		bad = r.presence.bad
	}
	if bad {
		return nil, errFrame
	}
	return f, nil
}

// idList is Ids or Target[] of Appendix A as a frame carries it. What
// the list holds is checked where it is used, against the rows or columns
// a tab can have: bad marks a text other than "all", and an item can be a
// run that has not two numbers.
type idList struct {
	all, bad bool
	items    []idItem
}

type idItem struct {
	kind uint8
	a, b int64 // an id; first and count of a run; k of {"new": k}
}

const (
	idOne = iota
	idRun
	idNew
	idBadRun // a run of another number of items than two
)

// word reads the text form of a list.
func (l *idList) word(b []byte) {
	l.all = string(b) == "all"
	l.bad = !l.all
}

// appendJSON writes the list so that it reads back as the same list.
func (l *idList) appendJSON(dst []byte) []byte {
	switch {
	case l.all:
		return append(dst, `"all"`...)
	case l.bad:
		return append(dst, `""`...)
	}
	dst = append(dst, '[')
	for i, it := range l.items {
		if i > 0 {
			dst = append(dst, ',')
		}
		switch it.kind {
		case idOne:
			dst = strconv.AppendInt(dst, it.a, 10)
		case idRun:
			dst = strconv.AppendInt(append(dst, '['), it.a, 10)
			dst = strconv.AppendInt(append(dst, ','), it.b, 10)
			dst = append(dst, ']')
		case idNew:
			dst = strconv.AppendInt(append(dst, `{"new":`...), it.a, 10)
			dst = append(dst, '}')
		default:
			dst = append(dst, '[', ']')
		}
	}
	return append(dst, ']')
}

// idsReader fills the list it was pointed at from the parts of its items.
type idsReader struct {
	list *idList
	run  int
	a, b int64
}

func (r *idsReader) begin(slot int) {
	if slot == sIDRun {
		r.run = 0
	}
}

func (r *idsReader) end(slot int) {
	if slot != sIDRun || r.list == nil {
		return
	}
	if r.run == 2 {
		r.list.items = append(r.list.items, idItem{idRun, r.a, r.b})
	} else {
		r.list.items = append(r.list.items, idItem{kind: idBadRun})
	}
}

func (r *idsReader) integer(slot int, n int64) {
	if r.list == nil {
		return // items of a member the frame's kind does not have
	}
	switch slot {
	case sIDOne:
		r.list.items = append(r.list.items, idItem{kind: idOne, a: n})
	case sIDNewCount:
		r.list.items = append(r.list.items, idItem{kind: idNew, a: n})
	case sIDRunItem:
		if r.run == 0 {
			r.a = n
		} else {
			r.b = n
		}
		r.run++
	}
}

// opReader builds the operations of an action from their parts.
type opReader struct {
	idsReader
	ops   []personOp
	op    *personOp
	rect  *rectSpec
	patch patchReader
	at    int
}

// opBit is the bit of a member of an operation in personOp.has.
func opBit(slot int) uint32 { return 1 << (slot - sOpKind) }

func (r *opReader) mark(slot int) {
	if slot >= sOpKind && slot <= sOpBefore {
		r.op.has |= opBit(slot)
	}
}

func (r *opReader) begin(slot int) {
	switch slot {
	case sOp:
		r.ops = append(r.ops, personOp{})
		r.op = &r.ops[len(r.ops)-1]
		return
	case sFrame, sOps:
		return
	}
	r.mark(slot)
	op := r.op
	switch slot {
	case sOpAt:
		r.at = 0
	case sOpRows:
		r.list = &op.Rows
	case sOpCols:
		r.list = &op.Cols
	case sOpIDs:
		r.list = &op.IDs
	case sOpValues:
		op.Values = [][]string{}
	case sOpValuesRow:
		// A paste is a rectangle: the row before says how long this one
		// is. A row costs no more than the one before it took to send.
		width := 0
		if n := len(op.Values); n > 0 {
			width = len(op.Values[n-1])
		}
		op.Values = append(op.Values, make([]string, 0, width))
	case sOpSet:
		r.patch = patchReader{}
	case sOpFrom, sOpTo, sOpSource, sOpTarget:
		r.rect = new(rectSpec)
		switch slot {
		case sOpFrom:
			op.From = r.rect
		case sOpTo:
			op.To = r.rect
		case sOpSource:
			op.Source = r.rect
		default:
			op.Target = r.rect
		}
	case sRectRows:
		r.list = &r.rect.Rows
	case sRectCols:
		r.list = &r.rect.Cols
	default:
		if slot >= sProp && slot < sPropEnd {
			r.patch.begin(slot)
		} else {
			r.idsReader.begin(slot)
		}
	}
}

func (r *opReader) end(slot int) {
	switch {
	case slot == sOp:
		r.op.check()
	case slot == sOpSet:
		if r.op.setBad = r.patch.bad; r.patch.bad == nil {
			r.op.Set = r.patch.patch
		}
	case slot >= sProp && slot < sPropEnd:
		r.patch.end(slot)
	default:
		r.idsReader.end(slot)
	}
}

func (r *opReader) text(slot int, b []byte) {
	r.mark(slot)
	op := r.op
	switch slot {
	case sOpKind:
		op.Op = string(b)
	case sOpText:
		op.Text = string(b)
	case sOpRows:
		op.Rows.word(b)
	case sOpCols:
		op.Cols.word(b)
	case sOpIDs:
		op.IDs.word(b)
	case sRectRows:
		r.rect.Rows.word(b)
	case sRectCols:
		r.rect.Cols.word(b)
	case sOpValue:
		row := &op.Values[len(op.Values)-1]
		*row = append(*row, string(b))
	case sOpMode:
		op.Mode = string(b)
	case sOpSide:
		op.Side = string(b)
	case sOpName:
		op.Name = string(b)
	default:
		r.patch.text(slot, b)
	}
}

func (r *opReader) integer(slot int, n int64) {
	r.mark(slot)
	op := r.op
	switch slot {
	case sOpTab:
		op.Tab = n
	case sOpRow:
		op.Row = n
	case sOpCol:
		op.Col = n
	case sOpAtItem:
		op.At[r.at&1], r.at = n, r.at+1
	case sOpWidth:
		op.Width = n
	case sRectTab:
		r.rect.Tab = n
	case sOpBeside:
		op.Beside = n
	case sOpCount:
		op.Count = n
	case sOpBy:
		op.By = n
	case sOpFrozenRows:
		op.FrozenRows = n
	case sOpFrozenCols:
		op.FrozenCols = n
	case sOpBefore:
		op.Before = n
	case sNumDecimals:
		r.patch.num.decimals = n
	default:
		r.idsReader.integer(slot, n)
	}
}

func (r *opReader) flag(slot int, on bool) {
	r.mark(slot)
	switch slot {
	case sOpCut:
		r.op.Cut = on
	case sOpDesc:
		r.op.Desc = on
	default:
		r.patch.flag(slot, on)
	}
}

func (r *opReader) null(slot int) {
	r.mark(slot)
	if slot == sOpBefore {
		r.op.beforeNull = true
	} else {
		r.patch.null(slot)
	}
}

func (r *opReader) number(int, float64) {}

// UnmarshalJSON reads one operation by its shape, as an action reads each
// of its own.
func (op *personOp) UnmarshalJSON(data []byte) error {
	var r opReader
	if _, fault := walk(data, opShape, &r); fault != nil {
		return errFrame
	}
	*op = r.ops[0]
	return nil
}

// MarshalJSON writes the operation so that it reads back as the same
// operation. The hash of an action is taken over this form.
func (op personOp) MarshalJSON() ([]byte, error) { return op.appendJSON(nil), nil }

// check finds what makes the operation none of section 7.1 although its
// members are well formed: applying it gives the refusal as its result.
// Such a value is an operation of an unknown kind, an id that is no
// positive integer, a side that is neither before nor after, or an index
// of at that is no position.
func (op *personOp) check() {
	if _, known := opShape.kinds[op.Op]; !known {
		op.bad = invalidf("op", "unknown operation %q", op.Op)
		return
	}
	for _, r := range [...]*rectSpec{op.From, op.To} {
		if r != nil && !validID(uint64(max(r.Tab, 0))) {
			op.bad = invalidf("tab", "tab is an id: a positive integer")
		}
	}
	for _, id := range [...]struct {
		slot int
		n    int64
	}{{sOpTab, op.Tab}, {sOpRow, op.Row}, {sOpCol, op.Col}, {sOpBeside, op.Beside}, {sOpBy, op.By}} {
		if op.has&opBit(id.slot) != 0 && !validID(uint64(max(id.n, 0))) {
			name := opMembers[id.slot-sOpKind].name
			op.bad = invalidf(name, "%s is an id: a positive integer", name)
		}
	}
	if op.has&opBit(sOpBefore) != 0 && !op.beforeNull && !validID(uint64(max(op.Before, 0))) {
		op.bad = invalidf("before", "before is an id: a positive integer")
	}
	if op.has&opBit(sOpSide) != 0 && op.Side != "before" && op.Side != "after" {
		op.bad = invalidf("side", "side is before or after")
	}
	if op.has&opBit(sOpAt) != 0 && (op.At[0] < 0 || op.At[0] > math.MaxInt32 || op.At[1] < 0 || op.At[1] > math.MaxInt32) {
		op.bad = invalidf("at", "at is a row index and a column index")
	}
}

// appendJSON writes the members the operation carries, in the order of
// opMembers.
func (op *personOp) appendJSON(dst []byte) []byte {
	dst = appendString(append(dst, `{"op":`...), op.Op)
	for at := 1; at < len(opMembers); at++ {
		slot := sOpKind + at
		if op.has&opBit(slot) == 0 {
			continue
		}
		dst = appendKey(dst, opMembers[at].name, true)
		switch slot {
		case sOpTab:
			dst = strconv.AppendInt(dst, op.Tab, 10)
		case sOpRow:
			dst = strconv.AppendInt(dst, op.Row, 10)
		case sOpCol:
			dst = strconv.AppendInt(dst, op.Col, 10)
		case sOpText:
			dst = appendString(dst, op.Text)
		case sOpAt:
			dst = strconv.AppendInt(append(dst, '['), op.At[0], 10)
			dst = strconv.AppendInt(append(dst, ','), op.At[1], 10)
			dst = append(dst, ']')
		case sOpRows:
			dst = op.Rows.appendJSON(dst)
		case sOpCols:
			dst = op.Cols.appendJSON(dst)
		case sOpIDs:
			dst = op.IDs.appendJSON(dst)
		case sOpValues:
			dst = append(dst, '[')
			for i, line := range op.Values {
				if i > 0 {
					dst = append(dst, ',')
				}
				dst = append(dst, '[')
				for j, text := range line {
					if j > 0 {
						dst = append(dst, ',')
					}
					dst = appendString(dst, text)
				}
				dst = append(dst, ']')
			}
			dst = append(dst, ']')
		case sOpSet:
			dst = appendPatch(dst, op.Set, op.setBad)
		case sOpWidth:
			dst = strconv.AppendInt(dst, op.Width, 10)
		case sOpFrom:
			dst = op.From.appendJSON(dst, true)
		case sOpTo:
			dst = op.To.appendJSON(dst, true)
		case sOpSource:
			dst = op.Source.appendJSON(dst, false)
		case sOpTarget:
			dst = op.Target.appendJSON(dst, false)
		case sOpMode:
			dst = appendString(dst, op.Mode)
		case sOpCut:
			dst = strconv.AppendBool(dst, op.Cut)
		case sOpBeside:
			dst = strconv.AppendInt(dst, op.Beside, 10)
		case sOpSide:
			dst = appendString(dst, op.Side)
		case sOpCount:
			dst = strconv.AppendInt(dst, op.Count, 10)
		case sOpBy:
			dst = strconv.AppendInt(dst, op.By, 10)
		case sOpDesc:
			dst = strconv.AppendBool(dst, op.Desc)
		case sOpFrozenRows:
			dst = strconv.AppendInt(dst, op.FrozenRows, 10)
		case sOpFrozenCols:
			dst = strconv.AppendInt(dst, op.FrozenCols, 10)
		case sOpName:
			dst = appendString(dst, op.Name)
		case sOpBefore:
			if op.beforeNull {
				dst = append(dst, "null"...)
			} else {
				dst = strconv.AppendInt(dst, op.Before, 10)
			}
		}
	}
	return append(dst, '}')
}

func (r *rectSpec) appendJSON(dst []byte, tab bool) []byte {
	dst = append(dst, '{')
	if tab {
		dst = strconv.AppendInt(append(dst, `"tab":`...), r.Tab, 10)
		dst = append(dst, ',')
	}
	dst = r.Rows.appendJSON(append(dst, `"rows":`...))
	dst = r.Cols.appendJSON(append(dst, `,"cols":`...))
	return append(dst, '}')
}

// numberKinds are the kinds of a number format by their names on the
// wire, and numberParts the fields of one as its object carries them.
var numberKinds = [...]string{
	formula.NumberGeneral: "general", formula.NumberFixed: "number", formula.NumberPercent: "percent", formula.NumberCurrency: "currency",
	formula.NumberDate: "date", formula.NumberTime: "time", formula.NumberDateTime: "datetime", formula.NumberText: "text",
}

type numberParts struct {
	kind, symbol, pattern, date, time string
	decimals                          int64
	thousands                         bool
}

// format builds the number format. The shape has given the kind exactly
// its fields; ok is false for a value outside rule 6.
func (n *numberParts) format() (nf formula.NumberFormat, ok bool) {
	kind := slices.Index(numberKinds[:], n.kind)
	if kind < 0 || n.decimals < 0 || n.decimals > 10 {
		return nf, false
	}
	nf.Kind = formula.NumberKind(kind)
	okDate, okTime := true, true
	switch nf.Kind {
	case formula.NumberFixed:
		nf.Decimals, nf.Thousands = uint8(n.decimals), n.thousands
	case formula.NumberPercent:
		nf.Decimals = uint8(n.decimals)
	case formula.NumberCurrency:
		nf.Decimals, nf.Symbol = uint8(n.decimals), n.symbol
	case formula.NumberDate:
		nf.Date, okDate = datePattern(n.pattern)
	case formula.NumberTime:
		nf.Time, okTime = timePattern(n.pattern)
	case formula.NumberDateTime:
		nf.Date, okDate = datePattern(n.date)
		nf.Time, okTime = timePattern(n.time)
	}
	return nf, okDate && okTime && nf.Valid()
}

func datePattern(s string) (formula.DatePattern, bool) {
	for p := formula.DateISO; p <= formula.DateMonth; p++ {
		if p.String() == s {
			return p, true
		}
	}
	return 0, false
}

func timePattern(s string) (formula.TimePattern, bool) {
	for p := formula.TimeHM; p <= formula.TimeAMPM; p++ {
		if p.String() == s {
			return p, true
		}
	}
	return 0, false
}

// patchReader builds a format patch from the parts of its object. A null
// resets its property. bad is set for a value outside rule 6: a color
// that is none of the palette, an alignment or a number format that is
// none of the rule. Applying the patch then gives the refusal.
type patchReader struct {
	patch formatPatch
	bad   *WriteError
	num   numberParts
}

// The refusals of a format, each with one text so that a patch that is
// written again reads back the same.
var (
	errPatchColor  = invalidf("format", "a color is one of the palette, such as \"red-600\"")
	errPatchAlign  = invalidf("format", "an alignment is general, left, center or right")
	errPatchNumber = invalidf("format", "the number format is outside rule 6")
)

func (r *patchReader) begin(slot int) {
	if slot == sProp+propNumber {
		r.num = numberParts{}
		r.patch.mask |= 1 << propNumber
	}
}

func (r *patchReader) end(slot int) {
	if slot != sProp+propNumber {
		return
	}
	nf, ok := r.num.format()
	switch {
	case !ok:
		r.fail(errPatchNumber)
	case nf.Kind != formula.NumberGeneral:
		r.patch.f.Number = nf
	}
}

func (r *patchReader) fail(err *WriteError) {
	if r.bad == nil {
		r.bad = err
	}
}

func (r *patchReader) null(slot int) {
	if slot >= sProp && slot < sPropEnd {
		r.patch.mask |= 1 << (slot - sProp)
	}
}

func (r *patchReader) flag(slot int, on bool) {
	if slot == sNumThousands {
		r.num.thousands = on
		return
	}
	if slot < sProp || slot >= sPropEnd {
		return
	}
	r.patch.mask |= 1 << (slot - sProp)
	switch slot - sProp {
	case propBold:
		r.patch.f.Bold = on
	case propItalic:
		r.patch.f.Italic = on
	case propUnderline:
		r.patch.f.Underline = on
	case propStrike:
		r.patch.f.Strikethrough = on
	case propWrap:
		r.patch.f.Wrap = on
	}
}

func (r *patchReader) text(slot int, b []byte) {
	switch slot {
	case sNumKind:
		r.num.kind = string(b)
	case sNumSymbol:
		r.num.symbol = string(b)
	case sNumPattern:
		r.num.pattern = string(b)
	case sNumDate:
		r.num.date = string(b)
	case sNumTime:
		r.num.time = string(b)
	case sProp + propTextColor, sProp + propFill:
		r.patch.mask |= 1 << (slot - sProp)
		c, ok := formula.ParseColor(string(b))
		if !ok {
			r.fail(errPatchColor)
		} else if slot == sProp+propFill {
			r.patch.f.Fill = c
		} else {
			r.patch.f.TextColor = c
		}
	case sProp + propAlign:
		r.patch.mask |= 1 << propAlign
		at := slices.Index(alignNames[:], string(b))
		if at < 0 {
			r.fail(errPatchAlign)
		} else {
			r.patch.f.Align = formula.Align(at)
		}
	}
}

// appendPatch writes a patch so that it reads back as the same patch: a
// property at its default as null, and a patch that was refused as one
// that is refused for the same reason.
func appendPatch(dst []byte, p formatPatch, bad *WriteError) []byte {
	switch bad {
	case errPatchColor:
		return append(dst, `{"text_color":""}`...)
	case errPatchAlign:
		return append(dst, `{"align":""}`...)
	case errPatchNumber:
		return append(dst, `{"number":{"kind":"percent","decimals":11}}`...)
	}
	dst = append(dst, '{')
	comma := false
	for prop, name := range propNames {
		if !p.has(prop) {
			continue
		}
		dst = appendKey(dst, name, comma)
		comma = true
		start := len(dst)
		switch prop {
		case propBold:
			dst = appendTrue(dst, p.f.Bold)
		case propItalic:
			dst = appendTrue(dst, p.f.Italic)
		case propUnderline:
			dst = appendTrue(dst, p.f.Underline)
		case propStrike:
			dst = appendTrue(dst, p.f.Strikethrough)
		case propWrap:
			dst = appendTrue(dst, p.f.Wrap)
		case propTextColor:
			if p.f.TextColor != 0 {
				dst = appendString(dst, p.f.TextColor.String())
			}
		case propFill:
			if p.f.Fill != 0 {
				dst = appendString(dst, p.f.Fill.String())
			}
		case propAlign:
			if p.f.Align != formula.AlignGeneral {
				dst = appendString(dst, alignNames[p.f.Align])
			}
		case propNumber:
			if p.f.Number != (formula.NumberFormat{}) {
				if data, err := p.f.Number.MarshalJSON(); err == nil {
					dst = append(dst, data...)
				}
			}
		}
		if len(dst) == start {
			dst = append(dst, "null"...)
		}
	}
	return append(dst, '}')
}

// appendTrue appends true, and nothing for false: the caller writes the
// default of a property as null.
func appendTrue(dst []byte, on bool) []byte {
	if on {
		return append(dst, "true"...)
	}
	return dst
}

func parseDecimal(s string) (uint64, bool) {
	n, err := strconv.ParseUint(s, 10, 64)
	return n, err == nil && strconv.FormatUint(n, 10) == s
}

func envelope(kind string, fields ...any) string {
	dst := append([]byte(`{"type":"`), kind...)
	dst = append(dst, '"')
	for i := 0; i+1 < len(fields); i += 2 {
		dst = append(dst, ',', '"')
		dst = append(dst, fields[i].(string)...)
		dst = append(dst, '"', ':')
		switch v := fields[i+1].(type) {
		case json.RawMessage:
			dst = append(dst, v...)
		case []byte:
			dst = append(dst, v...)
		default:
			data, _ := json.Marshal(v)
			dst = append(dst, data...)
		}
	}
	return string(append(dst, '}'))
}

// appendTabInfo appends the TabInfo list: every tab in order with its
// name, counts and frozen counts.
func appendTabInfo(dst []byte, wb *Workbook) []byte {
	dst = append(dst, '[')
	for i, t := range wb.tabs {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, `{"id":`...)
		dst = strconv.AppendUint(dst, uint64(t.id), 10)
		dst = append(dst, `,"name":`...)
		dst = appendString(dst, t.name)
		dst = append(dst, `,"rows":`...)
		dst = strconv.AppendInt(dst, int64(t.rows.len()), 10)
		dst = append(dst, `,"cols":`...)
		dst = strconv.AppendInt(dst, int64(t.cols.len()), 10)
		dst = append(dst, `,"frozen_rows":`...)
		dst = strconv.AppendInt(dst, int64(t.frozenRows), 10)
		dst = append(dst, `,"frozen_cols":`...)
		dst = strconv.AppendInt(dst, int64(t.frozenCols), 10)
		dst = append(dst, '}')
	}
	return append(dst, ']')
}

type agentNote struct {
	tab   TabID
	cells int
}

// tabPart is what a delta carries for the connections that watch one tab.
type tabPart struct {
	widths, cells []byte
}

// deltaRec is one entry of the delta log: everything a state change sends,
// encoded once. The frame of a connection depends only on the tab it
// watches and on whether it is Public, so the hub builds frames on demand
// and may keep them for the next connection (hub.frame).
type deltaRec struct {
	seq     uint64
	at      time.Time
	reset   bool
	from    string // {"session_id": ..., "n": ...}
	date    string
	tabs    []byte
	styles  []byte
	edits   []byte // the structural edits of every tab, in the order they were made
	note    string
	parts   map[TabID]*tabPart
	touched []TabID
	size    int // bytes of the fields above

	// wireEpoch is the epoch of the format list the cells were encoded
	// against. The hub sets it when the entry joins the log.
	wireEpoch uint64

	// Kept frames and their bytes. The hub owns both, under its mutex.
	frames map[frameKey]string
	cached int

	// counts holds the number of delta_part frames of each frame larger
	// than a frame, found once for every connection that sends it.
	counts map[frameKey]*partCount

	// Senders, under the hub's mutex (hub.next, hub.release). An entry
	// that leaves the log while connections send it is charged to the
	// host-wide deltaHeld until the last of them lets go, and revoked
	// when the charge does not fit: its senders then stop and resync.
	holders int
	left    bool
	charged int64
	revoked atomic.Bool
}

// partCount is the number of parts of one frame, or why it cannot be cut.
// done is closed when it is known.
type partCount struct {
	done chan struct{}
	n    int
	err  error
}

type frameKey struct {
	tab    TabID
	public bool
}

// fields returns the fields of the delta frame of a connection that
// watches tab, in their order on the wire, after its type and sequence.
// The values are the entry's own encoded bytes, which never change once
// the entry is in the log; the callers only read them.
func (r *deltaRec) fields(tab TabID, public bool) []deltaField {
	out := make([]deltaField, 0, 9)
	field := func(name string, value []byte) {
		if len(value) > 0 {
			out = append(out, deltaField{name, value})
		}
	}
	if !public {
		field("from", []byte(r.from))
	}
	if r.date != "" {
		field("date", appendString(nil, r.date))
	}
	field("tabs", r.tabs)
	field("struct", r.edits)
	if p := r.parts[tab]; p != nil {
		field("widths", p.widths)
		field("cells", p.cells)
	}
	field("styles", r.styles)
	var others []byte
	for _, id := range r.touched {
		if id == tab {
			continue
		}
		if others == nil {
			others = append(others, '[')
		} else {
			others = append(others, ',')
		}
		others = strconv.AppendUint(others, uint64(id), 10)
	}
	if others != nil {
		field("touched", append(others, ']'))
	}
	if !public {
		field("note", []byte(r.note))
	}
	return out
}

// deltaHead opens a delta frame; its fields and a closing brace follow.
func deltaHead(seq uint64) []byte {
	dst := append(make([]byte, 0, 256), `{"type":"delta","seq":"`...)
	dst = strconv.AppendUint(dst, seq, 10)
	return append(dst, '"')
}

// frameSize returns the length of the frame build makes of fields.
func frameSize(seq uint64, fields []deltaField) int {
	n := len(deltaHead(seq)) + 1
	for _, f := range fields {
		n += len(f.key) + 4 + len(f.value)
	}
	return n
}

// build encodes the entry as the delta frame of a connection that watches
// tab, from fields.
func (r *deltaRec) build(fields []deltaField) string {
	dst := deltaHead(r.seq)
	for _, f := range fields {
		dst = append(dst, ',', '"')
		dst = append(dst, f.key...)
		dst = append(dst, '"', ':')
		dst = append(dst, f.value...)
	}
	return string(append(dst, '}'))
}

// appendStruct appends one structural edit as the StructEdit of Appendix
// A, with the id of its tab. An edit of rows or columns names them by ids,
// and no edit carries a cell, so the entry is the same for every
// connection, whichever tab the connection watches.
func appendStruct(dst []byte, e *structEdit) []byte {
	suffix := "cols"
	if e.rows {
		suffix = "rows"
	}
	dst = append(dst, `{"op":"`...)
	switch e.kind {
	case editInsert:
		dst = append(dst, "insert_"...)
		dst = append(dst, suffix...)
	case editDelete:
		dst = append(dst, "delete_"...)
		dst = append(dst, suffix...)
	case editSort:
		dst = append(dst, "sort"...)
	case editRename:
		dst = append(dst, "rename_tab"...)
	case editTabAdd:
		dst = append(dst, "add_tab"...)
	case editTabDelete:
		dst = append(dst, "delete_tab"...)
	case editTabMove:
		dst = append(dst, "move_tab"...)
	}
	dst = append(dst, `","tab":`...)
	dst = strconv.AppendUint(dst, uint64(e.tab.id), 10)
	switch e.kind {
	case editInsert:
		dst = append(dst, `,"ids":`...)
		dst = appendRuns(dst, e.ids)
		dst = append(dst, `,"after":`...)
		if e.after == 0 {
			dst = append(dst, "null"...)
		} else {
			dst = strconv.AppendUint(dst, e.after, 10)
		}
	case editDelete:
		dst = append(dst, `,"ids":`...)
		dst = appendRuns(dst, e.ids)
	case editSort:
		dst = append(dst, `,"positions":`...)
		positions := make([]uint64, len(e.positions))
		for i, p := range e.positions {
			positions[i] = uint64(p)
		}
		dst = appendRuns(dst, positions)
		dst = append(dst, `,"ids":[`...)
		for i, id := range e.ids {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = strconv.AppendUint(dst, id, 10)
		}
		dst = append(dst, ']')
	case editRename:
		dst = append(dst, `,"name":`...)
		dst = appendString(dst, e.tr.Name)
	}
	return append(dst, '}')
}

// buildDelta encodes what an action changed, from the workbook as the
// action left it: cells with their new values and every value a
// recalculation changed, structure, widths, the tab list and new formats.
// The caller holds the write lock, and calls it before any later action
// changes the workbook, so that the delta is the state of its own sequence.
func buildDelta(x *txn, note *agentNote) *deltaRec {
	wb := x.wb
	rec := &deltaRec{seq: x.seq, parts: map[TabID]*tabPart{}}
	part := func(t *Tab) *tabPart {
		p := rec.parts[t.id]
		if p == nil {
			p = &tabPart{}
			rec.parts[t.id] = p
		}
		return p
	}
	byTab := map[*Tab]map[RowID][]ColID{}
	add := func(t *Tab, row RowID, col ColID) {
		if wb.byID[t.id] != t {
			return
		}
		if _, ok := t.posOf(row, col); !ok {
			return
		}
		rows := byTab[t]
		if rows == nil {
			rows = map[RowID][]ColID{}
			byTab[t] = rows
		}
		rows[row] = append(rows[row], col)
	}
	for ref := range x.cells {
		add(ref.tab, ref.row, ref.col)
	}
	for _, n := range x.values {
		if n.cell == n.tab.cell(n.row, n.col) {
			add(n.tab, n.row, n.col)
		}
	}
	var scratch []byte
	for t, rows := range byTab {
		ids := make([]RowID, 0, len(rows))
		for row := range rows {
			ids = append(ids, row)
		}
		slices.Sort(ids)
		dst := []byte{'['}
		for i, row := range ids {
			cols := rows[row]
			slices.Sort(cols)
			cols = slices.Compact(cols)
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = append(dst, '[')
			dst = strconv.AppendUint(dst, uint64(row), 10)
			dst = append(dst, ',', '[')
			for k, col := range cols {
				if k > 0 {
					dst = append(dst, ',')
				}
				if c := t.cell(row, col); c != nil {
					format := wb.styles.format(c.style)
					dst = appendCellOut(dst, col, c.content, c.f, c.val, format, wb.wire.idx[format], &scratch)
				} else {
					dst = append(dst, '[')
					dst = strconv.AppendUint(dst, uint64(col), 10)
					dst = append(dst, ']')
				}
			}
			dst = append(dst, ']', ']')
		}
		part(t).cells = append(dst, ']')
	}
	for i := range x.structs {
		e := &x.structs[i]
		if rec.edits == nil {
			rec.edits = append(rec.edits, '[')
		} else {
			rec.edits[len(rec.edits)-1] = ','
		}
		rec.edits = append(appendStruct(rec.edits, e), ']')
		// The connections that watch the tab apply the edit to its axes.
		if e.axisEdit() && wb.byID[e.tab.id] == e.tab {
			part(e.tab)
		}
	}
	for t, tt := range x.tabs {
		if wb.byID[t.id] != t {
			continue
		}
		if len(tt.widths) > 0 {
			cols := make([]ColID, 0, len(tt.widths))
			for _, col := range tt.widths {
				if _, live := t.cols.pos[uint64(col)]; live {
					cols = append(cols, col)
				}
			}
			slices.Sort(cols)
			cols = slices.Compact(cols)
			dst := []byte{'['}
			for i, col := range cols {
				if i > 0 {
					dst = append(dst, ',')
				}
				dst = append(dst, '[')
				dst = strconv.AppendUint(dst, uint64(col), 10)
				dst = append(dst, ',')
				dst = strconv.AppendInt(dst, int64(t.widthOf(col)), 10)
				dst = append(dst, ']')
			}
			part(t).widths = append(dst, ']')
		} else if tt.meta || tt.created {
			part(t)
		}
	}
	if x.tabList {
		rec.tabs = appendTabInfo(nil, wb)
	}
	if n := len(wb.wire.list); n > x.wireMark {
		dst := []byte{'['}
		for i, f := range wb.wire.list[x.wireMark:] {
			if i > 0 {
				dst = append(dst, ',')
			}
			if out, err := appendFormat(dst, f); err == nil {
				dst = out
			} else {
				dst = append(dst, '{', '}')
			}
		}
		rec.styles = append(dst, ']')
	}
	if note != nil {
		rec.note = `{"agent":true,"tab":` + strconv.FormatUint(uint64(note.tab), 10) + `,"cells":` + strconv.Itoa(note.cells) + `}`
	}
	// The size counts what the buffers hold in memory, which append may
	// have grown past their length.
	for id, p := range rec.parts {
		rec.touched = append(rec.touched, id)
		rec.size += cap(p.cells) + cap(p.widths)
	}
	slices.Sort(rec.touched)
	rec.size += cap(rec.tabs) + cap(rec.styles) + cap(rec.edits) + cap(rec.touched)*8 + 128
	return rec
}
