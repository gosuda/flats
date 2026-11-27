package formula

import (
	"math"
	"unicode/utf8"
)

// Limits of the grid and of calculated text (spec rule 9).
const (
	MaxRows = 100000 // rows in a tab
	MaxCols = 702    // columns in a tab, A to ZZ

	MaxTextLen     = 32767 // code points of one text value
	LimitedTextLen = 255   // code points of a text result in limited mode
	MaxPatternLen  = 255   // code points of a pattern that holds a wildcard

	MaxFormulaChars = 8192 // code points of a formula
	MaxFormulaDepth = 64   // nesting of brackets and calls
	MaxTabNameChars = 31   // code points of a tab name (spec rule 2)
)

// ErrorCode is one of the nine error values of spec rule 11.
type ErrorCode uint8

const (
	ErrNone   ErrorCode = iota
	ErrSyntax           // #ERROR!
	ErrName             // #NAME?
	ErrRef              // #REF!
	ErrValue            // #VALUE!
	ErrDiv0             // #DIV/0!
	ErrNum              // #NUM!
	ErrNA               // #N/A
	ErrCycle            // #CYCLE!
	ErrLimit            // #LIMIT!
)

// String is the error as a cell shows it, such as "#DIV/0!".
func (e ErrorCode) String() string {
	switch e {
	case ErrSyntax:
		return "#ERROR!"
	case ErrName:
		return "#NAME?"
	case ErrRef:
		return "#REF!"
	case ErrValue:
		return "#VALUE!"
	case ErrDiv0:
		return "#DIV/0!"
	case ErrNum:
		return "#NUM!"
	case ErrNA:
		return "#N/A"
	case ErrCycle:
		return "#CYCLE!"
	case ErrLimit:
		return "#LIMIT!"
	}
	return ""
}

// ParseErrorCode reads an error as a cell shows it. Capitals count.
func ParseErrorCode(s string) (ErrorCode, bool) {
	for e := ErrSyntax; e <= ErrLimit; e++ {
		if e.String() == s {
			return e, true
		}
	}
	return ErrNone, false
}

// ValueKind tells what a Value is.
type ValueKind uint8

const (
	KindBlank ValueKind = iota
	KindNumber
	KindText
	KindBool
	KindError
)

// Value is what a cell evaluates to: blank, a number, text, a boolean or an
// error. The zero Value is blank. Dates and times are numbers. Values
// compare with ==.
type Value struct {
	kind ValueKind
	num  float64 // the number; 1 or 0 for a boolean; the ErrorCode of an error
	str  string
}

// NumberValue returns the number x. A number that is not finite gives #NUM!.
func NumberValue(x float64) Value {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return ErrorValue(ErrNum)
	}
	if x == 0 {
		x = 0 // one zero, so that values compare with ==
	}
	return Value{kind: KindNumber, num: x}
}

// TextValue returns the text s. An empty text is the empty string
// constant, which lies in no memory: a text of no bytes cut from a longer
// one would keep all of that alive, where nothing counts it.
func TextValue(s string) Value {
	if s == "" {
		return Value{kind: KindText}
	}
	return Value{kind: KindText, str: s}
}

// BoolValue returns the boolean b.
func BoolValue(b bool) Value {
	if b {
		return Value{kind: KindBool, num: 1}
	}
	return Value{kind: KindBool}
}

// ErrorValue returns the error e.
func ErrorValue(e ErrorCode) Value { return Value{kind: KindError, num: float64(e)} }

// Kind reports what v is.
func (v Value) Kind() ValueKind { return v.kind }

// IsBlank reports whether v is the blank value.
func (v Value) IsBlank() bool { return v.kind == KindBlank }

// Number returns the number of a number value, and 0 for any other kind.
func (v Value) Number() float64 {
	if v.kind == KindNumber {
		return v.num
	}
	return 0
}

// Text returns the text of a text value, and "" for any other kind.
func (v Value) Text() string { return v.str }

// Bool returns the boolean of a boolean value, and false for any other kind.
func (v Value) Bool() bool { return v.kind == KindBool && v.num != 0 }

// Err returns the error of an error value, and ErrNone for any other kind.
func (v Value) Err() ErrorCode {
	if v.kind == KindError {
		return ErrorCode(v.num)
	}
	return ErrNone
}

// TextLen returns the code points of a text value, and 0 for any other kind.
func (v Value) TextLen() int { return utf8.RuneCountInString(v.str) }

// ContentKind tells what a cell holds. The numbers are the kind column of
// the cells table.
type ContentKind uint8

const (
	ContentBlank      ContentKind = iota // 0
	ContentNumber                        // 1
	ContentText                          // 2
	ContentBool                          // 3
	ContentDate                          // 4
	ContentTime                          // 5
	ContentDateTime                      // 6
	ContentFormula                       // 7
	ContentUnreadable                    // 8, formula text the lexer cannot split
)

// Content is what a cell holds (spec rule 4).
//
// Num is the number, the day number of a date, the day fraction of a time,
// their sum for a date with time, and 1 or 0 for a boolean. Text is the text
// of a text cell. For a formula Text is the stored form (Formula.Stored), or
// the typed text for ContentUnreadable. ParseTyped cannot know the cell, so
// the formula content it returns holds the typed text: the caller parses it
// with Parse and keeps Formula.Stored.
type Content struct {
	Kind ContentKind
	Num  float64
	Text string
}

// IsFormula reports whether the content is a formula, readable or not.
func (c Content) IsFormula() bool {
	return c.Kind == ContentFormula || c.Kind == ContentUnreadable
}

// Value returns the value of content that is not a formula: the content
// itself. For a formula it returns the blank value; a formula's value comes
// from Formula.Eval.
func (c Content) Value() Value {
	switch c.Kind {
	case ContentNumber, ContentDate, ContentTime, ContentDateTime:
		return NumberValue(c.Num)
	case ContentText:
		return TextValue(c.Text)
	case ContentBool:
		return BoolValue(c.Num != 0)
	}
	return Value{}
}
