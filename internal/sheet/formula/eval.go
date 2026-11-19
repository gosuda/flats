package formula

import (
	"math"
	"strings"
	"unicode/utf8"
)

// TabRef names a tab to a Grid. The Grid chooses the numbers.
type TabRef uint64

// Grid is the sheet a formula reads.
//
// Value and Filled together tell a blank cell from a formula that returns
// blank: both have the blank value and only the second is filled. A cell
// outside the tab's rows and columns is not filled and has the blank value:
// the evaluator knows that from Dims and asks Value and Filled only for
// cells inside. A cell that holds only a format is not filled.
type Grid interface {
	// Tab finds a tab by its name as a formula writes it, without regard
	// to capitals.
	Tab(name string) (TabRef, bool)
	// Dims returns the rows and columns the tab has.
	Dims(tab TabRef) (rows, cols int32)
	// Value returns the value of a cell: its content, or the calculated
	// value of a formula.
	Value(tab TabRef, p Pos) Value
	// Filled reports whether the cell holds content, a formula included.
	Filled(tab TabRef, p Pos) bool
}

// Date is a calculation date as a day number: day 2 is 1900-01-01.
type Date int32

// Env is what one evaluation needs.
type Env struct {
	Grid   Grid
	Tab    TabRef  // the tab that holds the formula
	At     Pos     // the formula's cell
	Today  Date    // the calculation date, the value of TODAY
	Budget *Budget // counts reads and checks the deadline; nil counts nothing
	// Limited selects limited mode (spec rule 9): a text result longer
	// than LimitedTextLen code points is #LIMIT!.
	Limited bool

	// kept is memory of the evaluator that outlives one formula, so that
	// a pass over a sheet with one Env allocates it once. Like the Budget
	// it serves one goroutine at a time.
	kept *keptMemory
}

// keptMemory is what an Env keeps between formulas.
type keptMemory struct {
	bits   [][]uint64  // bit sets of the conditional totals that are not in use
	search pieceSearch // the table of the piece of a wildcard pattern searched for last
	masks  pieceMasks  // the same for a piece that holds "?"
	plain  textSearch  // the table of the long text that FIND or SUBSTITUTE looked for last
}

// keptBits is the longest bit set an Env keeps: one bit for each cell a
// formula may read.
const keptBits = 1_000_000 / 64

func (env *Env) memory() *keptMemory {
	if env.kept == nil {
		env.kept = new(keptMemory)
	}
	return env.kept
}

// takeBits returns a cleared bit set of the given length.
func (env *Env) takeBits(words int) []uint64 {
	m := env.memory()
	var b []uint64
	if n := len(m.bits); n > 0 {
		b, m.bits = m.bits[n-1], m.bits[:n-1]
	}
	if cap(b) < words {
		return make([]uint64, words)
	}
	b = b[:words]
	clear(b)
	return b
}

// giveBits hands a bit set back for the next call of takeBits.
func (env *Env) giveBits(b []uint64) {
	if m := env.memory(); cap(b) <= keptBits {
		m.bits = append(m.bits, b)
	}
}

// Budget counts the work of a recalculation and checks its deadline
// (technical design, section 10.4). The evaluator adds one unit for each
// cell it reads, one for each part of a formula it calculates, and one for
// every 64 steps of smaller work.
//
// A part of a formula is a value written in it, an operator, a call or a
// reference, whose range may also lie beside the tab and hold no cell. A
// function that goes through the values of its direct arguments again, as
// MEDIAN and the deviations do, counts each of them again as it counts the
// cell of a range, and so does a function that reads a direct value where
// it reads a range. A reference counts again each time it is resolved
// again. The name of a tab that a reference names is a step for each byte.
//
// A byte of a text is a step each time the evaluator goes through it: to
// read the text as a number, to count, cut, search or copy it, or to
// compare it with another. A comparison goes through the bytes the two
// texts have in common before they differ, and the code point that
// decides is one step more. LOWER, UPPER and PROPER count four steps for a
// byte. Matching a wildcard pattern counts the steps of section 10.7: one
// for a code point of the text where a piece is tried or is looked for
// with a failure table, and four where a piece with "?" is looked for with
// bits. A code point is one to four bytes. A percent sign is a step, and
// so is a count in the tables that find a number by its rank. The engine
// adds its own work with Add. A nil *Budget counts nothing.
//
// The package reads no clock: the deadline is the function given to
// NewBudget, called once every 4,096 units and whenever Check is called.
type Budget struct {
	used    int64
	next    int64
	expired func() bool
	over    bool
}

const budgetStep = 4096

// NewBudget returns a budget whose deadline has passed once expired
// reports true. expired may be nil for a budget that only counts.
func NewBudget(expired func() bool) *Budget {
	return &Budget{next: budgetStep, expired: expired}
}

// Add counts units of work and reports whether the work may go on.
func (b *Budget) Add(units int64) bool {
	if b == nil {
		return true
	}
	b.used += units
	if b.used >= b.next {
		b.next = b.used + budgetStep
		if b.expired != nil && b.expired() {
			b.over = true
		}
	}
	return !b.over
}

// Check looks at the deadline now, whatever has been counted since the
// last look, and reports whether the work may go on. A loop whose items
// may count nothing calls it at a fixed number of items. Once the deadline
// has passed it does not look again.
func (b *Budget) Check() bool {
	if b == nil {
		return true
	}
	if !b.over {
		b.next = b.used + budgetStep
		if b.expired != nil && b.expired() {
			b.over = true
		}
	}
	return !b.over
}

// Used returns the units counted so far.
func (b *Budget) Used() int64 {
	if b == nil {
		return 0
	}
	return b.used
}

// Exceeded reports whether the deadline passed. An evaluation that meets
// it stops early, and the value it returns means nothing.
func (b *Budget) Exceeded() bool { return b != nil && b.over }

// Eval calculates the value of the formula. It reads cells through
// env.Grid only, which must not be nil, and reads a range in place.
//
// A text result takes the memory of its own text: it never keeps a longer
// text alive that the calculation built on the way.
//
// A formula that cannot be read, or that calls a function with a number of
// arguments the function does not take, has the value #ERROR!. Eval does
// not look for cycles: the engine finds them from Refs and gives #CYCLE!.
func (f *Formula) Eval(env *Env) Value {
	if f.root == nil {
		return ErrorValue(ErrSyntax)
	}
	ev := evaluator{env: env, f: f}
	v := ev.eval(f.root)
	if v.kind == KindText {
		v = ev.result(v)
	}
	if ev.points > 0 {
		env.Budget.Add(int64(ev.points+63) / 64)
	}
	return v
}

// result is the text value of a formula as the caller keeps it.
func (ev *evaluator) result(v Value) Value {
	if v.str == "" {
		// A text of no bytes cut from a longer one keeps all of it alive.
		return Value{kind: KindText}
	}
	if ev.env.Limited && longerThan(v.str, LimitedTextLen) {
		return ErrorValue(ErrLimit)
	}
	// A text is no longer than a text value may be, also the literal of a
	// stored form that no typed formula could hold.
	if longerThan(v.str, MaxTextLen) {
		return errValue(ErrValue)
	}
	// The caller keeps the result, and a text keeps alive all of the
	// memory it lies in. A function may have cut the text from a longer
	// one that it built on the way, as LEFT does, built it with room to
	// spare, or passed such a text on, so the result is copied into memory
	// of its own size. A reference alone gives the text of a cell, which
	// is one of its own by the same rule, and a formula that is a text alone
	// keeps that text in memory of its own (Parse): those are kept as they
	// are.
	if k := ev.f.root.kind; k != nodeRef && k != nodeText {
		ev.spend(len(v.str)) // the copy goes through the text
		v.str = strings.Clone(v.str)
	}
	return v
}

// longerThan reports whether s has more than n code points. It goes
// through at most 4n bytes. A code point has at most four, so a longer
// text has more than n code points.
func longerThan(s string, n int) bool {
	return len(s) > n && (len(s) > 4*n || utf8.RuneCountInString(s) > n)
}

type evaluator struct {
	env    *Env
	f      *Formula
	points int // small steps of work that the budget does not hold yet
	spent  int // all the small steps of work so far
	tries  int // spent may reach this while a piece of a pattern is tried at each place
}

func errValue(e ErrorCode) Value { return Value{kind: KindError, num: float64(e)} }

// textResult returns the text s, or #VALUE! when it is longer than a text
// value may be.
func (ev *evaluator) textResult(s string) Value {
	ev.touch(len(s))
	if longerThan(s, MaxTextLen) {
		return errValue(ErrValue)
	}
	return TextValue(s)
}

// touch counts the bytes of text a function went through.
func (ev *evaluator) touch(n int) {
	ev.env.Budget.Add(int64(n+63) / 64)
}

// Going through a text is work: comparing it, reading it as a number,
// counting, cutting, searching or copying it. Wherever the evaluator does
// that it counts a small step for each byte, 64 to a unit of the budget,
// with spend, or the bytes at once with touch. The functions below are the
// places most of it happens.
//
// Once the deadline has passed (late), the evaluator calculates no further
// part of a formula: each is #LIMIT! (eval). What is under way ends where
// it next looks. A function or an operator that would go through a text
// gives #LIMIT! instead: the text functions, "&", a text that is read as a
// number (textNumber), an exact lookup and a conditional total. So does a
// function whose pass over a range or over its direct values the deadline
// stopped. A comparison of two texts has no error to give. It compares
// nothing and counts the two as equal (compareText), so the comparison
// operators, SWITCH and a sorted lookup then give an ordinary value that
// is not the answer. Eval still copies a text result for its caller
// (result).
//
// No value of such a pass is used. The engine looks at the budget after
// each Eval and drops the whole pass once the deadline has passed
// (technical design, section 10.4). Any other caller of Eval has to do the
// same.

// late reports whether the deadline has passed.
func (ev *evaluator) late() bool { return ev.env.Budget.Exceeded() }

// walk counts the n bytes of a text that is about to be gone through. It
// reports false, and counts nothing, when the deadline has passed.
func (ev *evaluator) walk(n int) bool {
	if ev.late() {
		return false
	}
	ev.spend(n)
	return true
}

// count returns the code points of s and counts going through it.
func (ev *evaluator) count(s string) int {
	ev.spend(len(s))
	return utf8.RuneCountInString(s)
}

// offset returns the byte offset of code point n of s, or len(s) when s is
// shorter, and counts the bytes before it.
func (ev *evaluator) offset(s string, n int) int {
	i := runeOffset(s, n)
	ev.spend(i)
	return i
}

// number is the number a value counts as where an operator or a function
// needs one.
func (ev *evaluator) number(v Value) (float64, ErrorCode) {
	switch v.kind {
	case KindNumber, KindBool:
		return v.num, ErrNone
	case KindBlank:
		return 0, ErrNone
	case KindText:
		return ev.textNumber(v.str)
	}
	return 0, v.Err()
}

// textNumber reads text as a number (textToNumber) and counts going
// through it. Text that reads as no number is #VALUE!.
func (ev *evaluator) textNumber(s string) (float64, ErrorCode) {
	if !ev.walk(len(s)) {
		return 0, ErrLimit
	}
	if x, ok := textToNumber(s); ok {
		return x, ErrNone
	}
	return 0, ErrValue
}

// compareText orders two texts in the text order of rule 22 (CompareText)
// and counts the bytes it goes through, and one step for the code point
// that decides. After the deadline it compares nothing and returns 0.
func (ev *evaluator) compareText(a, b string) int {
	if ev.late() {
		return 0
	}
	order, walked := compareTextCounted(a, b)
	ev.spend(walked + 1)
	return order
}

func toText(v Value) (string, ErrorCode) {
	switch v.kind {
	case KindText:
		return v.str, ErrNone
	case KindNumber:
		return generalText(v.num), ErrNone
	case KindBool:
		if v.num != 0 {
			return "TRUE", ErrNone
		}
		return "FALSE", ErrNone
	case KindBlank:
		return "", ErrNone
	}
	return "", v.Err()
}

func toBool(v Value) (bool, ErrorCode) {
	switch v.kind {
	case KindBool, KindNumber:
		return v.num != 0, ErrNone
	case KindBlank:
		return false, ErrNone
	case KindText:
		if b, ok := isBoolText(v.str); ok {
			return b, ErrNone
		}
		return false, ErrValue
	}
	return false, v.Err()
}

func (ev *evaluator) num(n *node) (float64, ErrorCode) {
	v := ev.eval(n)
	if v.kind == KindNumber {
		return v.num, ErrNone // most operands, without a call
	}
	return ev.number(v)
}

func (ev *evaluator) text(n *node) (string, ErrorCode) { return toText(ev.eval(n)) }
func (ev *evaluator) truth(n *node) (bool, ErrorCode)  { return toBool(ev.eval(n)) }
func (ev *evaluator) whole(n *node) (float64, ErrorCode) {
	x, err := ev.num(n)
	return math.Trunc(x), err
}

// clampInt brings a whole number of characters or items into the range of
// an int.
func clampInt(x float64) int {
	switch {
	case x > 1<<40:
		return 1 << 40
	case x < -(1 << 40):
		return -(1 << 40)
	}
	return int(x)
}

// partSteps is what a part of a formula counts: one unit, as a cell that
// is read does.
const partSteps = 64

// eval calculates a part of the formula and counts it. After the deadline
// it calculates nothing.
func (ev *evaluator) eval(n *node) Value {
	if ev.late() {
		return errValue(ErrLimit)
	}
	if n.kind == nodeRef {
		return ev.ref(n) // counted where it is resolved
	}
	ev.spend(partSteps)
	switch n.kind {
	case nodeNumber:
		return Value{kind: KindNumber, num: n.num}
	case nodeText:
		return Value{kind: KindText, str: n.str}
	case nodeBool:
		return Value{kind: KindBool, num: n.num}
	case nodeError:
		return errValue(n.err)
	case nodeEmpty:
		return Value{}
	case nodeName:
		return errValue(ErrName)
	case nodeUnary:
		x, err := ev.num(n.args[0])
		if err != ErrNone {
			return errValue(err)
		}
		if n.neg {
			x = -x
		}
		return NumberValue(x)
	case nodePercent:
		x, err := ev.num(n.args[0])
		if err != ErrNone {
			return errValue(err)
		}
		ev.spend(int(n.count)) // a division for each sign
		for i := int32(0); i < n.count; i++ {
			x /= 100
		}
		return NumberValue(x)
	case nodeArith:
		a, err := ev.num(n.args[0])
		if err != ErrNone {
			return errValue(err)
		}
		b, err := ev.num(n.args[1])
		if err != ErrNone {
			return errValue(err)
		}
		return arith(n.op, a, b)
	case nodeConcat:
		a := ev.eval(n.args[0])
		if a.kind == KindError {
			return a
		}
		b := ev.eval(n.args[1])
		if b.kind == KindError {
			return b
		}
		if ev.late() {
			return errValue(ErrLimit)
		}
		s, _ := toText(a)
		t, _ := toText(b)
		switch {
		case s == "":
			return ev.textResult(t)
		case t == "":
			return ev.textResult(s)
		}
		if len(s)+len(t) > MaxTextLen && utf8.RuneCountInString(s)+utf8.RuneCountInString(t) > MaxTextLen {
			ev.touch(len(s) + len(t))
			return errValue(ErrValue)
		}
		return ev.textResult(s + t)
	case nodeCompare:
		a := ev.eval(n.args[0])
		if a.kind == KindError {
			return a
		}
		b := ev.eval(n.args[1])
		if b.kind == KindError {
			return b
		}
		c := ev.compare(a, b)
		switch n.op {
		case opEq:
			return BoolValue(c == 0)
		case opNe:
			return BoolValue(c != 0)
		case opLt:
			return BoolValue(c < 0)
		case opLe:
			return BoolValue(c <= 0)
		case opGt:
			return BoolValue(c > 0)
		}
		return BoolValue(c >= 0)
	case nodeCall:
		return ev.call(n)
	}
	return errValue(ErrSyntax)
}

// ref is the value of a reference where one value is needed: the cell of
// a single reference, and #VALUE! for a range.
func (ev *evaluator) ref(n *node) Value {
	s := &ev.f.slots[n.slot]
	if s.kind != slotCell {
		if _, err := ev.rangeOf(n); err != ErrNone {
			return errValue(err)
		}
		return errValue(ErrValue)
	}
	ev.spend(partSteps + len(s.tab))
	p := Pos{Row: s.a.row, Col: s.a.col}
	if !s.a.rowAbs {
		p.Row += ev.env.At.Row
	}
	if !s.a.colAbs {
		p.Col += ev.env.At.Col
	}
	if !p.InGrid() {
		return errValue(ErrRef)
	}
	tab := ev.env.Tab
	if s.tab != "" {
		var ok bool
		if tab, ok = ev.env.Grid.Tab(s.tabName()); !ok {
			return errValue(ErrRef)
		}
	}
	if rows, cols := ev.env.Grid.Dims(tab); p.Row >= rows || p.Col >= cols {
		return Value{}
	}
	if !ev.env.Budget.Add(1) {
		return errValue(ErrLimit)
	}
	return ev.env.Grid.Value(tab, p)
}

func arith(op opCode, a, b float64) Value {
	switch op {
	case opAdd:
		return NumberValue(a + b)
	case opSub:
		return NumberValue(a - b)
	case opMul:
		return NumberValue(a * b)
	case opDiv:
		if b == 0 {
			return errValue(ErrDiv0)
		}
		return NumberValue(a / b)
	}
	return power(a, b)
}

// power raises a to b. 0 to the power 0 is 1.
func power(a, b float64) Value {
	switch {
	case a == 0 && b == 0:
		return NumberValue(1)
	case a == 0 && b < 0:
		return errValue(ErrDiv0)
	case a < 0 && b != math.Floor(b):
		return errValue(ErrNum)
	}
	return NumberValue(math.Pow(a, b))
}

// kindRank orders the kinds for a comparison: numbers, then text, then
// booleans.
func kindRank(k ValueKind) int {
	switch k {
	case KindNumber:
		return 0
	case KindText:
		return 1
	}
	return 2
}

// compare orders two values that are no errors (technical design, section
// 9.3). A blank value counts as 0, as empty text or as FALSE, whichever
// the other side is. Two texts are in the text order of rule 22
// (compareText), which gives 0 exactly for texts that are equal after case
// folding, so = and <> compare the case-folded texts. Comparing two texts
// is counted.
func (ev *evaluator) compare(a, b Value) int {
	if a.kind == KindBlank && b.kind == KindBlank {
		return 0
	}
	if a.kind == KindBlank {
		a = Value{kind: b.kind}
	}
	if b.kind == KindBlank {
		b = Value{kind: a.kind}
	}
	if ra, rb := kindRank(a.kind), kindRank(b.kind); ra != rb {
		if ra < rb {
			return -1
		}
		return 1
	}
	switch a.kind {
	case KindNumber:
		return compareNumbers(a.num, b.num)
	case KindText:
		return ev.compareText(a.str, b.str)
	}
	switch {
	case a.num < b.num:
		return -1
	case a.num > b.num:
		return 1
	}
	return 0
}

// rangeArg is a range a function reads in place, or a direct value that
// stands for a range of one cell. r1 to r2 and c1 to c2 are the written
// rectangle. Cells outside the tab's rows and cols are blank and are not
// read.
type rangeArg struct {
	tab            TabRef
	r1, c1, r2, c2 int32
	rows, cols     int32
	direct         bool
	val            Value
}

func (rg *rangeArg) nrows() int32 { return rg.r2 - rg.r1 + 1 }
func (rg *rangeArg) ncols() int32 { return rg.c2 - rg.c1 + 1 }
func (rg *rangeArg) cells() int64 { return int64(rg.nrows()) * int64(rg.ncols()) }

func (rg *rangeArg) sameSize(other *rangeArg) bool {
	return rg.nrows() == other.nrows() && rg.ncols() == other.ncols()
}

// lastRow and lastCol bound the part of the range inside the tab.
func (rg *rangeArg) lastRow() int32 { return min(rg.r2, rg.rows-1) }
func (rg *rangeArg) lastCol() int32 { return min(rg.c2, rg.cols-1) }

// inside counts the cells of the range that lie in the tab.
func (rg *rangeArg) inside() int64 {
	h, w := int64(rg.lastRow()-rg.r1+1), int64(rg.lastCol()-rg.c1+1)
	if h <= 0 || w <= 0 {
		return 0
	}
	return h * w
}

// rangeOf resolves a reference node for the formula's cell. The reference
// is a part of the formula, whether its range holds cells or lies beside
// the tab. Finding the tab it names goes through the name, a step for each
// byte.
func (ev *evaluator) rangeOf(n *node) (rangeArg, ErrorCode) {
	s := &ev.f.slots[n.slot]
	ev.spend(partSteps + len(s.tab))
	b, ok := s.bounds(ev.env.At)
	if !ok {
		return rangeArg{}, ErrRef
	}
	rg := rangeArg{tab: ev.env.Tab, r1: b.First.Row, c1: b.First.Col, r2: b.Last.Row, c2: b.Last.Col}
	if s.tab != "" {
		if rg.tab, ok = ev.env.Grid.Tab(s.tabName()); !ok {
			return rangeArg{}, ErrRef
		}
	}
	rg.rows, rg.cols = ev.env.Grid.Dims(rg.tab)
	if b.FullCols {
		rg.r2 = rg.rows - 1
	}
	if b.FullRows {
		rg.c2 = rg.cols - 1
	}
	return rg, ErrNone
}

// rangeArgOf reads an argument where a function reads a range: a
// reference, or a direct value as a range of one cell. A direct value that
// is an error is no cell: it is the error of the argument, as a reference
// that cannot be read is. So #REF!, which a deleted reference leaves in a
// formula, is the result and not a cell that matches or counts for
// nothing.
func (ev *evaluator) rangeArgOf(n *node) (rangeArg, ErrorCode) {
	if n.kind == nodeRef {
		return ev.rangeOf(n)
	}
	v := ev.eval(n)
	if v.kind == KindError {
		return rangeArg{}, v.Err()
	}
	return rangeArg{direct: true, val: v}, ErrNone
}

// cell returns the value at an offset of the range. The read is counted
// before it is made, and after the deadline it is not made. The value is
// then #LIMIT!, and a loop over cells ends where it next looks (late).
func (ev *evaluator) cell(rg *rangeArg, i, j int32) Value {
	if rg.direct {
		if !ev.env.Budget.Add(1) {
			return errValue(ErrLimit)
		}
		return rg.val
	}
	p := Pos{Row: rg.r1 + i, Col: rg.c1 + j}
	if p.Row >= rg.rows || p.Col >= rg.cols {
		return Value{}
	}
	if !ev.env.Budget.Add(1) {
		return errValue(ErrLimit)
	}
	return ev.env.Grid.Value(rg.tab, p)
}

// filled reports whether the cell at an offset of the range holds content.
// Like cell it reads nothing after the deadline, and reports false.
func (ev *evaluator) filled(rg *rangeArg, i, j int32) bool {
	if rg.direct {
		return ev.env.Budget.Add(1) && rg.val.kind != KindBlank
	}
	p := Pos{Row: rg.r1 + i, Col: rg.c1 + j}
	if p.Row >= rg.rows || p.Col >= rg.cols {
		return false
	}
	return ev.env.Budget.Add(1) && ev.env.Grid.Filled(rg.tab, p)
}

// scan calls fn for the value of each cell of the range that lies in the
// tab, row by row, with the cell's offsets. It stops when fn returns false
// or the deadline passes, and reports whether it reached the end.
func (ev *evaluator) scan(rg *rangeArg, fn func(i, j int32, v Value) bool) bool {
	b := ev.env.Budget
	if b.Exceeded() {
		return false
	}
	if rg.direct {
		b.Add(1)
		return fn(0, 0, rg.val)
	}
	g := ev.env.Grid
	lastRow, lastCol := rg.lastRow(), rg.lastCol()
	width := lastCol - rg.c1 + 1
	if width <= 0 {
		return true // the range lies beside the tab
	}
	block := rowBlock(width)
	for first := rg.r1; first <= lastRow; first += block {
		last := min(first+block-1, lastRow)
		if !b.Add(int64(last-first+1) * int64(width)) {
			return false
		}
		for r := first; r <= last; r++ {
			for c := rg.c1; c <= lastCol; c++ {
				if !fn(r-rg.r1, c-rg.c1, g.Value(rg.tab, Pos{Row: r, Col: c})) {
					return false
				}
			}
		}
	}
	return true
}

// rowBlock returns how many rows of a range of the given width are read
// between two additions to the budget: about a thousand cells.
func rowBlock(width int32) int32 {
	return max(1024/width, 1)
}

// numbers calls fn for each number of the arguments of a total: the
// numbers in a referenced range, where text and booleans are passed over,
// and each direct value as a number. It returns the first error it meets.
// When direct is not nil it collects the direct values for another pass.
func (ev *evaluator) numbers(args []*node, direct *[]float64, fn func(x float64)) ErrorCode {
	for _, a := range args {
		if a.kind != nodeRef {
			x, err := ev.num(a)
			if err != ErrNone {
				return err
			}
			if direct != nil {
				*direct = append(*direct, x)
			}
			fn(x)
			continue
		}
		rg, err := ev.rangeOf(a)
		if err != ErrNone {
			return err
		}
		if err := ev.rangeNumbers(&rg, fn); err != ErrNone {
			return err
		}
	}
	return ErrNone
}

// rangeNumbers calls fn for each number of a referenced range, row by row,
// and returns the first error a cell holds.
func (ev *evaluator) rangeNumbers(rg *rangeArg, fn func(x float64)) ErrorCode {
	g, b := ev.env.Grid, ev.env.Budget
	if b.Exceeded() {
		return ErrLimit
	}
	lastRow, lastCol := rg.lastRow(), rg.lastCol()
	width := lastCol - rg.c1 + 1
	if width <= 0 {
		return ErrNone // the range lies beside the tab
	}
	block := rowBlock(width)
	for first := rg.r1; first <= lastRow; first += block {
		last := min(first+block-1, lastRow)
		if !b.Add(int64(last-first+1) * int64(width)) {
			return ErrLimit
		}
		for r := first; r <= last; r++ {
			for c := rg.c1; c <= lastCol; c++ {
				switch v := g.Value(rg.tab, Pos{Row: r, Col: c}); v.kind {
				case KindNumber:
					fn(v.num)
				case KindError:
					return v.Err()
				}
			}
		}
	}
	return ErrNone
}

// numbersAgain is another pass over arguments that numbers went through
// without an error, with the direct values it collected. It counts each
// of them again, and stops when the deadline passes.
func (ev *evaluator) numbersAgain(args []*node, direct []float64, fn func(x float64)) {
	for _, a := range args {
		if ev.late() {
			return
		}
		if a.kind != nodeRef {
			ev.spend(partSteps)
			fn(direct[0])
			direct = direct[1:]
			continue
		}
		rg, err := ev.rangeOf(a)
		if err != ErrNone {
			return
		}
		ev.scan(&rg, func(_, _ int32, v Value) bool {
			if v.kind == KindNumber {
				fn(v.num)
			}
			return true
		})
	}
}

// sum adds numbers with a running compensation, so that the total is the
// one exact arithmetic gives at 15 significant digits.
type sum struct {
	total, comp float64
}

func (s *sum) add(x float64) {
	t := s.total + x
	if math.Abs(s.total) >= math.Abs(x) {
		s.comp += (s.total - t) + x
	} else {
		s.comp += (x - t) + s.total
	}
	s.total = t
}

func (s *sum) value() float64 { return s.total + s.comp }
