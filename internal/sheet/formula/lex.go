package formula

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

type tokenKind uint8

const (
	tokSpace tokenKind = iota
	tokNumber
	tokString
	tokError  // an error literal such as #REF!
	tokBadRef // a reference the grid cannot hold, such as A0: kept as typed, #REF!
	tokName
	tokRef
	tokOp
	tokOpen
	tokClose
	tokComma
)

type opCode uint8

const (
	opAdd opCode = iota + 1
	opSub
	opMul
	opDiv
	opPow
	opConcat
	opPercent
	opEq
	opNe
	opLt
	opLe
	opGt
	opGe
)

type token struct {
	kind       tokenKind
	op         opCode
	err        ErrorCode
	start, end int32 // bytes of the source
	slot       int32 // of a tokRef
}

// The lexer keeps a reference only when its rows and columns fit these
// bounds: three column letters and nine row digits. They are wider than the
// grid limits, so that a formula such as =A100001 keeps its text and has
// the value #REF!.
const (
	rowCap = 1_000_000_000
	colCap = 26*26*26 + 26*26 + 26
)

// lexer splits formula text into tokens. In typed mode a reference is A1
// text, read at the cell at. In stored mode a reference is a slot in
// braces.
type lexer struct {
	src    string
	stored bool
	at     Pos
	toks   []token
	slots  []slot
	out    []byte // typed mode: the stored form, which the tokens point into
	// maxToks, when it is not zero, is the most tokens the text may hold:
	// run reports false where another one starts.
	maxToks int
}

func isNameStart(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_'
}

func isNameChar(c byte) bool {
	return isNameStart(c) || isDigit(c) || c == '.'
}

func isLetter(c byte) bool { return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' }

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

var errorLiterals = [...]ErrorCode{ErrDiv0, ErrValue, ErrNum, ErrNA, ErrName, ErrRef, ErrSyntax, ErrCycle, ErrLimit}

// run splits the text after the leading "=". It reports false for text it
// cannot split.
func (lx *lexer) run() bool {
	s := lx.src
	if !lx.stored {
		lx.out = append(make([]byte, 0, len(s)+16), '=')
	}
	for i := 1; i < len(s); {
		if len(lx.toks) == lx.maxToks && lx.maxToks > 0 {
			return false
		}
		c := s[i]
		first := i
		tok := token{slot: -1}
		switch {
		case isSpace(c):
			tok.kind = tokSpace
			for i < len(s) && isSpace(s[i]) {
				i++
			}
		case c == '"':
			end := scanString(s, i)
			if end < 0 {
				return false
			}
			tok.kind = tokString
			i = end
		case c == '#':
			found := false
			for _, e := range errorLiterals {
				if strings.HasPrefix(s[i:], e.String()) {
					tok.kind, tok.err = tokError, e
					i += len(e.String())
					found = true
					break
				}
			}
			if !found {
				return false
			}
		case c == '{':
			if !lx.stored {
				return false
			}
			end, sl, ok := parseSlot(s, i)
			if !ok {
				return false
			}
			tok.kind, tok.slot = tokRef, int32(len(lx.slots))
			lx.slots = append(lx.slots, sl)
			i = end
		default:
			if end, ref, ok := matchA1(s, i); ok {
				switch {
				case ref.bad:
					tok.kind = tokBadRef
				case lx.stored:
					return false // a stored form holds its references in slots
				default:
					tok.kind, tok.slot = tokRef, int32(len(lx.slots))
					lx.slots = append(lx.slots, ref.slot(lx.at))
				}
				i = end
				break
			}
			if end := scanNumberToken(s, i); end > i {
				tok.kind = tokNumber
				i = end
				break
			}
			if isNameStart(c) {
				tok.kind = tokName
				for i < len(s) && isNameChar(s[i]) {
					i++
				}
				break
			}
			n, kind, op := scanOperator(s, i)
			if n == 0 {
				return false
			}
			tok.kind, tok.op = kind, op
			i += n
		}
		if lx.stored {
			tok.start, tok.end = int32(first), int32(i)
		} else {
			tok.start = int32(len(lx.out))
			if tok.kind == tokRef {
				lx.out = lx.slots[tok.slot].appendStored(lx.out)
			} else {
				lx.out = append(lx.out, s[first:i]...)
			}
			tok.end = int32(len(lx.out))
		}
		lx.toks = append(lx.toks, tok)
	}
	return true
}

// scanString returns the end of the text in quotation marks that starts at
// i, or -1 when it is not closed.
func scanString(s string, i int) int {
	for j := i + 1; j < len(s); j++ {
		if s[j] != '"' {
			continue
		}
		if j+1 < len(s) && s[j+1] == '"' {
			j++
			continue
		}
		return j + 1
	}
	return -1
}

// scanNumberToken returns the end of the number that starts at i: digits with
// an optional period, or a period and digits, and an optional exponent.
func scanNumberToken(s string, i int) int {
	j := i
	for j < len(s) && isDigit(s[j]) {
		j++
	}
	if j > i {
		if j < len(s) && s[j] == '.' {
			j++
			for j < len(s) && isDigit(s[j]) {
				j++
			}
		}
	} else {
		if j >= len(s) || s[j] != '.' {
			return i
		}
		k := j + 1
		for k < len(s) && isDigit(s[k]) {
			k++
		}
		if k == j+1 {
			return i
		}
		j = k
	}
	if j < len(s) && (s[j] == 'E' || s[j] == 'e') {
		k := j + 1
		if k < len(s) && (s[k] == '+' || s[k] == '-') {
			k++
		}
		if k < len(s) && isDigit(s[k]) {
			for k < len(s) && isDigit(s[k]) {
				k++
			}
			j = k
		}
	}
	return j
}

func scanOperator(s string, i int) (n int, kind tokenKind, op opCode) {
	switch s[i] {
	case '(':
		return 1, tokOpen, 0
	case ')':
		return 1, tokClose, 0
	case ',':
		return 1, tokComma, 0
	case '+':
		return 1, tokOp, opAdd
	case '-':
		return 1, tokOp, opSub
	case '*':
		return 1, tokOp, opMul
	case '/':
		return 1, tokOp, opDiv
	case '^':
		return 1, tokOp, opPow
	case '&':
		return 1, tokOp, opConcat
	case '%':
		return 1, tokOp, opPercent
	case '=':
		return 1, tokOp, opEq
	case '<':
		if i+1 < len(s) && s[i+1] == '>' {
			return 2, tokOp, opNe
		}
		if i+1 < len(s) && s[i+1] == '=' {
			return 2, tokOp, opLe
		}
		return 1, tokOp, opLt
	case '>':
		if i+1 < len(s) && s[i+1] == '=' {
			return 2, tokOp, opGe
		}
		return 1, tokOp, opGt
	}
	return 0, 0, 0
}

// a1Ref is a reference read from A1 text. Rows and columns count from 1.
type a1Ref struct {
	tab    string // as written, apostrophes included
	kind   slotKind
	c1, c2 int32
	r1, r2 int32
	abs    [4]bool // c1, r1, c2, r2 are written with "$"
	bad    bool    // a row is 0 or has more than nine digits
}

// refEnd reports whether a reference may end at i: the next character is
// not one that would make it a name, a call or a tab name.
func refEnd(s string, i int) bool {
	if i >= len(s) {
		return true
	}
	c := s[i]
	return !isNameChar(c) && c != '(' && c != '!'
}

// scanColumn reads "$"? and one to three letters at i.
func scanColumn(s string, i int) (end int, col int32, abs, ok bool) {
	if i < len(s) && s[i] == '$' {
		abs = true
		i++
	}
	j := i
	for j < len(s) && isLetter(s[j]) {
		col = col*26 + int32(s[j]|0x20-'a') + 1
		j++
		if j-i > 3 {
			return 0, 0, false, false
		}
	}
	return j, col, abs, j > i
}

// scanRow reads "$"? and digits at i. bad is set for a row the lexer does
// not keep as a number.
func scanRow(s string, i int) (end int, row int32, abs, bad, ok bool) {
	if i < len(s) && s[i] == '$' {
		abs = true
		i++
	}
	j := i
	var n int64
	for j < len(s) && isDigit(s[j]) {
		if n < rowCap {
			n = n*10 + int64(s[j]-'0')
		}
		j++
	}
	if j == i {
		return 0, 0, false, false, false
	}
	if n < 1 || n >= rowCap {
		return j, 0, abs, true, true
	}
	return j, int32(n), abs, false, true
}

// bareTabNameLen returns the bytes at the start of s that a tab name
// written without apostrophes may hold: a letter of any script or an
// underscore, and then letters, decimal digits, underscores and periods.
func bareTabNameLen(s string) int {
	n := 0
	for n < len(s) {
		r, size := rune(s[n]), 1
		if r >= utf8.RuneSelf {
			r, size = utf8.DecodeRuneInString(s[n:])
		}
		if !(unicode.IsLetter(r) || r == '_' || (n > 0 && (unicode.IsDigit(r) || r == '.'))) {
			break
		}
		n += size
	}
	return n
}

// bareTabName reports whether a tab name is written without apostrophes:
// all of it is what bareTabNameLen takes, and it reads neither as a cell
// address nor as TRUE or FALSE.
func bareTabName(name string) bool {
	if name == "" || bareTabNameLen(name) != len(name) {
		return false
	}
	if _, ok := isBoolText(name); ok {
		return false
	}
	if ce, _, _, ok := scanColumn(name, 0); ok {
		if re, _, _, _, ok := scanRow(name, ce); ok && re == len(name) {
			return false
		}
	}
	return true
}

// scanTabName reads a tab name and its "!" at i and returns the name as
// written and the position after the "!". The name is in apostrophes, or
// one that bareTabName accepts.
func scanTabName(s string, i int) (tab string, end int, ok bool) {
	if i >= len(s) {
		return "", i, false
	}
	j := i
	if s[i] == '\'' {
		j++
		for {
			if j >= len(s) {
				return "", i, false
			}
			if s[j] == '\'' {
				if j+1 < len(s) && s[j+1] == '\'' {
					j += 2
					continue
				}
				break
			}
			j++
		}
		if j == i+1 {
			return "", i, false
		}
		j++
	} else if j = i + bareTabNameLen(s[i:]); j == i {
		return "", i, false
	}
	if j >= len(s) || s[j] != '!' || (s[i] != '\'' && !bareTabName(s[i:j])) {
		return "", i, false
	}
	return s[i:j], j + 1, true
}

// matchA1 reads a reference in A1 text at i: a cell, a range, whole
// columns or whole rows, with an optional tab name.
func matchA1(s string, i int) (end int, ref a1Ref, ok bool) {
	tab, body, hasTab := scanTabName(s, i)
	if end, ref, ok = matchA1Body(s, body); ok {
		if hasTab {
			ref.tab = tab
		}
		return end, ref, true
	}
	return 0, a1Ref{}, false
}

func matchA1Body(s string, i int) (end int, ref a1Ref, ok bool) {
	if ce, col, cabs, ok := scanColumn(s, i); ok {
		if re, row, rabs, bad, ok := scanRow(s, ce); ok {
			ref = a1Ref{kind: slotCell, c1: col, r1: row, c2: col, r2: row, abs: [4]bool{cabs, rabs, cabs, rabs}, bad: bad}
			if re < len(s) && s[re] == ':' {
				if ce2, col2, cabs2, ok := scanColumn(s, re+1); ok {
					if re2, row2, rabs2, bad2, ok := scanRow(s, ce2); ok && refEnd(s, re2) {
						ref.kind, ref.c2, ref.r2, ref.bad = slotRange, col2, row2, bad || bad2
						ref.abs[2], ref.abs[3] = cabs2, rabs2
						return re2, ref, true
					}
				}
			}
			if refEnd(s, re) {
				return re, ref, true
			}
			return 0, a1Ref{}, false
		}
		if ce < len(s) && s[ce] == ':' {
			if ce2, col2, cabs2, ok := scanColumn(s, ce+1); ok && refEnd(s, ce2) {
				return ce2, a1Ref{kind: slotCols, c1: col, c2: col2, abs: [4]bool{cabs, false, cabs2, false}}, true
			}
		}
		return 0, a1Ref{}, false
	}
	if re, row, rabs, bad, ok := scanRow(s, i); ok && re < len(s) && s[re] == ':' {
		if re2, row2, rabs2, bad2, ok := scanRow(s, re+1); ok && refEnd(s, re2) {
			return re2, a1Ref{kind: slotRows, r1: row, r2: row2, abs: [4]bool{false, rabs, false, rabs2}, bad: bad || bad2}, true
		}
	}
	return 0, a1Ref{}, false
}

// slot turns a reference read at the cell at into its stored form: offsets
// from that cell, and a fixed row or column where "$" was written.
func (r *a1Ref) slot(at Pos) slot {
	sl := slot{tab: r.tab, kind: r.kind}
	sl.a = corner{row: r.r1 - 1, col: r.c1 - 1, colAbs: r.abs[0], rowAbs: r.abs[1]}
	sl.b = corner{row: r.r2 - 1, col: r.c2 - 1, colAbs: r.abs[2], rowAbs: r.abs[3]}
	for _, c := range []*corner{&sl.a, &sl.b} {
		if !c.rowAbs {
			c.row -= at.Row
		}
		if !c.colAbs {
			c.col -= at.Col
		}
	}
	switch r.kind {
	case slotCols:
		sl.a.row, sl.b.row = 0, 0
	case slotRows:
		sl.a.col, sl.b.col = 0, 0
	case slotCell:
		sl.b = corner{}
	}
	return sl
}
