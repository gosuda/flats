package formula

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Limits bounds a typed formula. A field that is zero or negative takes its
// default: MaxFormulaChars and MaxFormulaDepth.
type Limits struct{ MaxChars, MaxDepth int }

// LimitError is the error of Parse for a formula that is too long or nested
// too deep. Name is the limit's name of the technical design, section 11:
// "formula_length" or "formula_depth".
type LimitError struct {
	Name  string
	Limit int // the limit
	Value int // what the formula has
}

func (e *LimitError) Error() string {
	return "formula: " + e.Name + " is " + strconv.Itoa(e.Value) + ", over the limit of " + strconv.Itoa(e.Limit)
}

// ErrStored is the error of ParseStored for text that is no stored form.
var ErrStored = errors.New("formula: not a stored formula")

// MaxStoredBytes bounds the stored form of a formula a cell holds, which
// ParseStored reads, so that reading stored text is bounded work. A formula
// whose text at its cell is within MaxFormulaChars, and whose references
// all lie within what a formula can name there, takes at most nine bytes
// of stored form for each code point of that text: a reference that
// writes as A1 can be the slot {R[-99999]C[-701]} of 18 bytes, a tab name
// takes its bytes in the slot and its code points in the text, and a code
// point of the text around the references takes at most four bytes. A
// copy, a move or an undo that puts a formula where a reference lies past
// what a formula can name turns the reference into #REF! (Moved, Placed),
// so each reference shows its tab name in the text, and the writer keeps
// the text of each formula within the limit.
const MaxStoredBytes = 9 * MaxFormulaChars

// MaxRetainedBytes bounds the stored forms that ParseRetained reads: those
// an undo entry keeps, which a rename can make longer than a formula may
// be at its cell. It bounds the room of a form: its bytes once each
// reference has become the longest slot a transform can make of it. A
// transform keeps the text around the references and their number, and it
// writes the name of a tab into a reference only in a rename or a cut. A
// transform never gives a form more room, so every form made of one
// ParseRetained read is read again.
//
// A formula Parse took has a room of at most MaxRetainedBytes: of its
// MaxFormulaChars code points each reference takes two at least, and its
// other text and the tab names written in it take at most four bytes for
// each.
const MaxRetainedBytes = 4*MaxFormulaChars + MaxFormulaChars/2*maxSlotBytes

// maxStoredTokens bounds the tokens of a stored form. Each token writes at
// least one code point of text at any cell, so a formula Parse took has no
// more, and a transform keeps their number: a reference it ends becomes
// the one token #REF!. A form with more tokens is refused once the lexer
// reaches the next one, before the rest is read and before a tree is
// built.
const maxStoredTokens = MaxFormulaChars

const (
	maxRowPart   = len("R[-]") + 9 // an offset under rowCap has 9 digits
	maxColPart   = len("C[-]") + 5 // and one under colCap 5
	maxQuotedTab = len("''") + 4*MaxTabNameChars
	// maxSlotBytes is the longest slot a transform writes:
	// {'name'!R[-n]C[-n]:R[-n]C[-n]} with the name of a tab. A slot whose
	// name is longer, as a name typed in the formula can be, keeps it, and
	// its room is longer by as much.
	maxSlotBytes = len("{!:}") + maxQuotedTab + 2*(maxRowPart+maxColPart)
)

// Formula is a parsed formula in its stored form: the typed text with each
// reference held as offsets from the formula's cell, or as a fixed row or
// column where the reference was written with "$". It is immutable.
type Formula struct {
	stored     string
	lits       []string // the text around the slots: one piece more than slots
	slots      []slot
	root       *node // nil when the value is #ERROR!
	litRunes   int   // code points of lits
	size       int32 // what Size returns
	unreadable bool
	form       bool // a TextForm: lits, slots and root are left out
	today      bool
	dateResult bool
}

type nodeKind uint8

const (
	nodeNumber nodeKind = iota
	nodeText
	nodeBool
	nodeError
	nodeEmpty   // an argument left empty
	nodeName    // a name that is no call: #NAME?
	nodeRef     // slot
	nodeUnary   // leading signs
	nodePercent // count trailing percent signs
	nodeArith
	nodeConcat
	nodeCompare
	nodeCall
)

type node struct {
	kind  nodeKind
	op    opCode
	fn    fnID
	neg   bool      // of a nodeUnary: an odd number of minus signs
	err   ErrorCode // of a nodeError
	slot  int32     // of a nodeRef
	count int32     // of a nodePercent
	num   float64   // of a nodeNumber; 1 for a true nodeBool
	str   string    // of a nodeText
	args  []*node
}

// Parse reads the typed text of a formula, "=" included, written in the
// cell at.
//
// A syntax error is not a Go error: the formula is returned and its value
// is #ERROR!. The only error is a *LimitError for a formula longer than
// lim.MaxChars code points or nested deeper than lim.MaxDepth. Both limits
// hold for text that cannot be read as well.
func Parse(text string, at Pos, lim Limits) (*Formula, error) {
	if lim.MaxChars <= 0 {
		lim.MaxChars = MaxFormulaChars
	}
	if lim.MaxDepth <= 0 {
		lim.MaxDepth = MaxFormulaDepth
	}
	chars := utf8.RuneCountInString(text)
	if chars > lim.MaxChars {
		return nil, &LimitError{Name: "formula_length", Limit: lim.MaxChars, Value: chars}
	}
	lx := lexer{src: text, at: at}
	readable := text != "" && text[0] == '=' && lx.run()
	depth := 0
	if readable {
		depth = nesting(lx.toks)
	} else {
		depth = textNesting(text)
	}
	if depth > lim.MaxDepth {
		return nil, &LimitError{Name: "formula_depth", Limit: lim.MaxDepth, Value: depth}
	}
	if !readable {
		return newUnreadable(text, chars), nil
	}
	f := &Formula{stored: string(lx.out), slots: lx.slots}
	f.finish(lx.toks, lim.MaxDepth)
	return f, nil
}

// ParseStored reads the stored form of a formula a cell holds, as
// Formula.Stored returned it. It returns ErrStored for text that is no
// stored form, and for a form longer than MaxStoredBytes, with more tokens
// than a formula of MaxFormulaChars code points or nested deeper than
// MaxFormulaDepth. No formula the writer keeps in a cell is any of these,
// and a transform changes the references of a form, never its nesting. It
// reads no byte of a text longer than MaxStoredBytes. Formula.LimitAt
// checks the form at its cell.
//
// Text of an unreadable formula (Formula.Unreadable, content kind 8) is not
// a stored form: read it again with Parse, at any position.
func ParseStored(stored string) (*Formula, error) {
	if len(stored) > MaxStoredBytes {
		return nil, ErrStored
	}
	lx := lexer{src: stored, stored: true, maxToks: maxStoredTokens}
	if !lx.read() {
		return nil, ErrStored
	}
	return lx.formula(), nil
}

// ParseRetained is ParseStored for a form an undo entry keeps, whose text
// at its cell can be longer than a formula may be. It refuses a form with
// more room than MaxRetainedBytes, and reads no byte of a text longer than
// that.
func ParseRetained(stored string) (*Formula, error) {
	if len(stored) > MaxRetainedBytes {
		return nil, ErrStored
	}
	lx := lexer{src: stored, stored: true, maxToks: maxStoredTokens}
	if !lx.read() || room(stored, lx.toks, lx.slots) > MaxRetainedBytes {
		return nil, ErrStored
	}
	return lx.formula(), nil
}

// parseStored is ParseStored without its bounds, for the form a transform
// made of a formula in memory.
func parseStored(stored string) (*Formula, error) {
	lx := lexer{src: stored, stored: true}
	if !lx.read() {
		return nil, ErrStored
	}
	return lx.formula(), nil
}

// read lexes a stored form and checks its nesting.
func (lx *lexer) read() bool {
	return lx.src != "" && lx.src[0] == '=' && lx.run() && nesting(lx.toks) <= MaxFormulaDepth
}

func (lx *lexer) formula() *Formula {
	f := &Formula{stored: lx.src, slots: lx.slots}
	f.finish(lx.toks, MaxFormulaDepth)
	return f
}

// room returns the bytes of a stored form once each of its slots has
// become the longest one a transform can make of it.
func room(stored string, toks []token, slots []slot) int {
	n := len(stored)
	for i := range toks {
		if t := &toks[i]; t.kind == tokRef {
			n += maxSlotBytes + max(len(slots[t.slot].tab)-maxQuotedTab, 0) - int(t.end-t.start)
		}
	}
	return n
}

// LimitAt returns an error when the formula cannot be one the writer
// keeps in the cell at, as a stored form that Parse did not take can be:
// a *LimitError when its text there is longer than MaxFormulaChars, and
// ErrStored when a reference lies past what a formula can name there,
// which the writer turns into #REF! (Moved). It measures the text only
// when the stored form is long enough for that.
func (f *Formula) LimitAt(at Pos) error {
	for i := range f.slots {
		if _, ok := f.slots[i].resolve(at); !ok {
			return ErrStored
		}
	}
	if maxTextRatio*len(f.stored) <= MaxFormulaChars {
		return nil
	}
	if n := f.TextLen(at); n > MaxFormulaChars {
		return &LimitError{Name: "formula_length", Limit: MaxFormulaChars, Value: n}
	}
	return nil
}

// newUnreadable returns the formula of text the lexer cannot split.
func newUnreadable(text string, chars int) *Formula {
	f := &Formula{stored: text, lits: []string{text}, litRunes: chars, unreadable: true}
	f.measure(0)
	return f
}

// nesting returns the deepest level of brackets and calls in the tokens.
func nesting(toks []token) int {
	depth, deepest := 0, 0
	for i := range toks {
		switch toks[i].kind {
		case tokOpen:
			depth++
			deepest = max(deepest, depth)
		case tokClose:
			depth = max(depth-1, 0)
		}
	}
	return deepest
}

// textNesting returns the deepest level of brackets in text the lexer
// cannot split, so that such text is held to the nesting limit too. It
// counts what nesting counts in tokens: a bracket in text in quotation
// marks, or in a tab name in apostrophes, is none.
func textNesting(s string) int {
	depth, deepest := 0, 0
	names := nameScan{s: s}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			if end := scanString(s, i); end > 0 {
				i = end - 1
			}
		case '\'':
			if end := names.end(i); end > 0 {
				i = end - 1
			}
		case '(':
			depth++
			deepest = max(deepest, depth)
		case ')':
			depth = max(depth-1, 0)
		}
	}
	return deepest
}

// nameScan finds the end of a tab name in apostrophes for each apostrophe
// of a text in turn. It gives what scanTabName gives. Asked from the start
// of the text to its end, it goes through the text a fixed number of
// times, where scanTabName would read on to a far apostrophe from each
// one.
//
// Inside a name two apostrophes stand for one. So a name opened at an
// apostrophe closes at the last one of the first run of apostrophes of odd
// length after it, where the rest of its own run counts as the first run.
type nameScan struct {
	s       string
	runEnd  int // the end of the run of apostrophes measured last
	oddFrom int // odd holds for every place from oddFrom to the run it names
	odd     int // the start of the first run of odd length at or after oddFrom, len(s) for none
	oddEnd  int // the end of that run
	read    int // the characters read so far, which the tests bound
}

// end returns the place after the "!" of the tab name in apostrophes that
// starts at i, or 0 when none starts there. It is asked for apostrophes
// from left to right.
func (n *nameScan) end(i int) int {
	s := n.s
	if i >= n.runEnd {
		n.runEnd = i
		for n.runEnd < len(s) && s[n.runEnd] == '\'' {
			n.runEnd++
		}
		n.read += n.runEnd - i
	}
	closing := n.runEnd - 1
	switch rest := closing - i; {
	case rest == 1:
		return 0 // an empty name
	case rest%2 == 0:
		// The rest of the run is pairs. The name closes at a later run.
		if n.runEnd > n.odd || n.runEnd < n.oddFrom || n.oddEnd == 0 {
			n.oddFrom, n.odd, n.oddEnd = n.runEnd, len(s), len(s)
			for j := n.runEnd; j < len(s); j++ {
				if s[j] != '\'' {
					continue
				}
				k := j
				for k < len(s) && s[k] == '\'' {
					k++
				}
				if (k-j)%2 == 1 {
					n.odd, n.oddEnd = j, k
					break
				}
				j = k
			}
			n.read += n.oddEnd - n.oddFrom
		}
		if n.odd == len(s) {
			return 0 // the name is not closed
		}
		closing = n.oddEnd - 1
	}
	if closing+1 < len(s) && s[closing+1] == '!' {
		return closing + 2
	}
	return 0
}

// finish cuts the stored text into the pieces around the slots and builds
// the tree. toks hold positions in f.stored.
func (f *Formula) finish(toks []token, maxDepth int) {
	f.lits = make([]string, 0, len(f.slots)+1)
	prev := 0
	n := 0
	for i := range toks {
		t := toks[i]
		if t.kind == tokRef {
			f.lits = append(f.lits, f.stored[prev:t.start])
			prev = int(t.end)
			// A tab name read from typed text points into that text, which
			// the formula would keep alive. The stored form holds the same
			// name right after the brace.
			if s := &f.slots[t.slot]; s.tab != "" {
				from := int(t.start) + 1
				s.tab = f.stored[from : from+len(s.tab)]
			}
		}
		if t.kind != tokSpace {
			toks[n] = t
			n++
		}
	}
	f.lits = append(f.lits, f.stored[prev:])
	for _, l := range f.lits {
		f.litRunes += utf8.RuneCountInString(l)
	}
	p := parser{src: f.stored, toks: toks[:n], maxDepth: maxDepth}
	root := p.expr()
	if p.failed || p.pos != len(p.toks) {
		f.measure(0)
		return
	}
	if root.kind == nodeText && root.str != "" && !p.unescaped {
		// A formula that is one text has that text as its value, and the
		// value leaves the formula with the cell's: into the cells that
		// read it, a capture, a pasted value or an undo payload, after the
		// formula is gone. A text of its own keeps no more than itself
		// alive there, where a part of the stored text would keep all of
		// it. primary leaves an empty text no part of it.
		root.str = strings.Clone(root.str)
		p.copied += textBytes(len(root.str))
	}
	f.root = root
	f.today = p.today
	f.dateResult = root.kind == nodeCall &&
		(root.fn == fnDATE || root.fn == fnTODAY || root.fn == fnEDATE || root.fn == fnEOMONTH)
	f.measure(p.copied)
}

// Stored returns the stored form, such as "={R[0]C[-2]}*{R[0]C[-1]}".
func (f *Formula) Stored() string { return f.stored }

// Unreadable reports whether the lexer could not split the typed text. Such
// a formula is stored as typed (content kind 8), has the value #ERROR!, has
// no references and is not changed by any transform.
func (f *Formula) Unreadable() bool { return f.unreadable }

// ReadsToday reports whether the formula calls TODAY anywhere.
func (f *Formula) ReadsToday() bool { return f.today }

// DateResult reports whether the outermost function is DATE, TODAY, EDATE
// or EOMONTH, so that a cell without a number format shows a date.
func (f *Formula) DateResult() bool { return f.dateResult }

// parser builds the tree of the grammar of the technical design, section
// 9.2, from tokens without spaces.
type parser struct {
	src       string
	toks      []token
	pos       int
	depth     int
	maxDepth  int
	failed    bool
	today     bool
	copied    int64 // bytes of the texts built for nodes, which hold no part of src
	unescaped bool  // a text was built without its doubled quotation marks
}

func (p *parser) fail() *node {
	p.failed = true
	return nil
}

// op returns the operator at the current token when it lies between lo and
// hi, and steps over it.
func (p *parser) op(lo, hi opCode) opCode {
	if p.failed || p.pos >= len(p.toks) {
		return 0
	}
	t := &p.toks[p.pos]
	if t.kind != tokOp || t.op < lo || t.op > hi {
		return 0
	}
	p.pos++
	return t.op
}

func (p *parser) at(kind tokenKind) bool {
	return !p.failed && p.pos < len(p.toks) && p.toks[p.pos].kind == kind
}

func (p *parser) binary(kind nodeKind, op opCode, left, right *node) *node {
	if p.failed {
		return nil
	}
	return &node{kind: kind, op: op, args: []*node{left, right}}
}

func (p *parser) expr() *node {
	n := p.concat()
	for {
		op := p.op(opEq, opGe)
		if op == 0 {
			return n
		}
		n = p.binary(nodeCompare, op, n, p.concat())
	}
}

func (p *parser) concat() *node {
	n := p.sum()
	for p.op(opConcat, opConcat) != 0 {
		n = p.binary(nodeConcat, opConcat, n, p.sum())
	}
	return n
}

func (p *parser) sum() *node {
	n := p.product()
	for {
		op := p.op(opAdd, opSub)
		if op == 0 {
			return n
		}
		n = p.binary(nodeArith, op, n, p.product())
	}
}

func (p *parser) product() *node {
	n := p.power()
	for {
		op := p.op(opMul, opDiv)
		if op == 0 {
			return n
		}
		n = p.binary(nodeArith, op, n, p.power())
	}
}

func (p *parser) power() *node {
	n := p.percent()
	for p.op(opPow, opPow) != 0 {
		n = p.binary(nodeArith, opPow, n, p.percent())
	}
	return n
}

func (p *parser) percent() *node {
	n := p.unary()
	count := int32(0)
	for p.op(opPercent, opPercent) != 0 {
		count++
	}
	if count == 0 || p.failed {
		return n
	}
	return &node{kind: nodePercent, count: count, args: []*node{n}}
}

func (p *parser) unary() *node {
	signs, neg := 0, false
	for {
		op := p.op(opAdd, opSub)
		if op == 0 {
			break
		}
		signs++
		if op == opSub {
			neg = !neg
		}
	}
	n := p.primary()
	if signs == 0 || p.failed {
		return n
	}
	return &node{kind: nodeUnary, neg: neg, args: []*node{n}}
}

func (p *parser) primary() *node {
	if p.failed || p.pos >= len(p.toks) {
		return p.fail()
	}
	t := &p.toks[p.pos]
	p.pos++
	text := p.src[t.start:t.end]
	switch t.kind {
	case tokRef:
		return &node{kind: nodeRef, slot: t.slot}
	case tokBadRef:
		return &node{kind: nodeError, err: ErrRef}
	case tokNumber:
		x, _ := strconv.ParseFloat(text, 64)
		if math.IsInf(x, 0) {
			return &node{kind: nodeError, err: ErrNum}
		}
		return &node{kind: nodeNumber, num: x}
	case tokString:
		text = text[1 : len(text)-1]
		if strings.Contains(text, `""`) {
			text = strings.ReplaceAll(text, `""`, `"`)
			p.copied += textBytes(len(text))
			p.unescaped = true
		}
		if text == "" {
			// "" written in a formula is no part of its stored text, which
			// may be long, so that a value it gives keeps none of it alive.
			return &node{kind: nodeText}
		}
		return &node{kind: nodeText, str: text}
	case tokError:
		return &node{kind: nodeError, err: t.err}
	case tokOpen:
		if p.depth++; p.depth > p.maxDepth {
			return p.fail()
		}
		n := p.expr()
		if !p.at(tokClose) {
			return p.fail()
		}
		p.pos++
		p.depth--
		return n
	case tokName:
		if p.at(tokOpen) {
			p.pos++
			return p.call(text)
		}
		if b, ok := isBoolText(text); ok {
			n := &node{kind: nodeBool}
			if b {
				n.num = 1
			}
			return n
		}
		return &node{kind: nodeName}
	}
	return p.fail()
}

// call reads the arguments of a call after its opening bracket. A call
// with a number of arguments the function does not take makes the whole
// formula #ERROR!.
func (p *parser) call(name string) *node {
	if p.depth++; p.depth > p.maxDepth {
		return p.fail()
	}
	n := &node{kind: nodeCall, fn: fnUnknown}
	if p.at(tokClose) {
		p.pos++
	} else {
		for {
			if p.at(tokComma) || p.at(tokClose) {
				n.args = append(n.args, &node{kind: nodeEmpty})
			} else {
				n.args = append(n.args, p.expr())
			}
			if p.at(tokClose) {
				p.pos++
				break
			}
			if !p.at(tokComma) {
				return p.fail()
			}
			p.pos++
		}
	}
	p.depth--
	fn, ok := lookupFunc(name)
	if !ok {
		return n
	}
	if !funcTable[fn].takes(len(n.args)) {
		return p.fail()
	}
	n.fn = fn
	if fn == fnTODAY {
		p.today = true
	}
	return n
}
