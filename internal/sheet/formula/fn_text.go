package formula

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// runeOffset returns the byte offset of code point n of s, or len(s) when s
// is shorter.
func runeOffset(s string, n int) int {
	if n <= 0 {
		return 0
	}
	if n >= len(s) {
		return len(s)
	}
	i := 0
	for ; n > 0 && i < len(s); n-- {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return i
}

// joiner builds the text of CONCAT, CONCATENATE and TEXTJOIN and checks the
// length limit while it builds. Once the text is too long it goes through
// no further piece.
type joiner struct {
	b       strings.Builder
	sep     string
	sepLen  int
	runes   int
	parts   int
	skip    bool // pass over empty pieces
	tooLong bool
}

func (j *joiner) add(ev *evaluator, s string) {
	if j.tooLong || (j.skip && s == "") {
		return
	}
	n := ev.count(s)
	if j.parts > 0 {
		n += j.sepLen
	}
	j.runes += n
	if j.runes > MaxTextLen {
		j.tooLong = true
	}
	if !j.tooLong {
		if j.parts > 0 {
			j.b.WriteString(j.sep)
		}
		j.b.WriteString(s)
	}
	j.parts++
}

// fnJoin joins the texts of the arguments. With ranges set a range gives
// each of its cells; without, a range is #VALUE!. With errorsFirst an
// error anywhere in the arguments comes before the length limit, as in
// TEXTJOIN; without, the text stops being built at the limit.
func (ev *evaluator) fnJoin(a []*node, sep string, skip, ranges, errorsFirst bool) Value {
	if ev.late() {
		return errValue(ErrLimit)
	}
	j := joiner{sep: sep, sepLen: ev.count(sep), skip: skip}
	for _, arg := range a {
		if j.tooLong && !errorsFirst {
			break
		}
		if arg.kind != nodeRef || ev.f.slots[arg.slot].kind == slotCell {
			s, err := ev.text(arg)
			if err != ErrNone {
				return errValue(err)
			}
			if ev.late() {
				return errValue(ErrLimit)
			}
			j.add(ev, s)
			continue
		}
		if !ranges {
			return errValue(ErrValue)
		}
		rg, err := ev.rangeOf(arg)
		if err != ErrNone {
			return errValue(err)
		}
		if err = ev.joinRange(&j, &rg, errorsFirst); err != ErrNone {
			return errValue(err)
		}
	}
	ev.touch(j.b.Len())
	if j.tooLong {
		return errValue(ErrValue)
	}
	return TextValue(j.b.String())
}

// joinRange adds the cells of a range, row by row. A cell outside the tab
// is an empty piece, which counts only when pieces are separated and empty
// ones are kept; such a range is walked cell by cell until the text is too
// long, which it is after at most MaxTextLen separators.
//
// It returns the first error a cell holds, and #LIMIT! when the deadline
// has passed and it met no error. The deadline may have stopped it before
// the last cell, so what it built or found then is not the answer.
func (ev *evaluator) joinRange(j *joiner, rg *rangeArg, errorsFirst bool) ErrorCode {
	err := ErrNone
	add := func(v Value) bool {
		if j.tooLong {
			// Only an error still counts, and only where errors come
			// first. The text of the cell is not looked at.
			err = v.Err()
			return err == ErrNone && !ev.late()
		}
		var s string
		if s, err = toText(v); err != ErrNone {
			return false
		}
		j.add(ev, s)
		return (errorsFirst || !j.tooLong) && !ev.late()
	}
	if j.skip || j.sep == "" || rg.inside() == rg.cells() {
		ev.scan(rg, func(_, _ int32, v Value) bool { return add(v) })
	} else {
		for r := int32(0); r < rg.nrows() && !j.tooLong; r++ {
			for c := int32(0); c < rg.ncols() && !j.tooLong; c++ {
				if !add(ev.cell(rg, r, c)) && (err != ErrNone || ev.late()) {
					return ev.stopped(err)
				}
			}
		}
		if errorsFirst && j.tooLong {
			// An error in a cell not yet reached still comes first.
			ev.scan(rg, func(_, _ int32, v Value) bool { return add(v) })
		}
	}
	return ev.stopped(err)
}

// stopped is the error a pass over cells ends with: the error it met, or
// #LIMIT! when it met none and the deadline has passed.
func (ev *evaluator) stopped(err ErrorCode) ErrorCode {
	if err == ErrNone && ev.late() {
		return ErrLimit
	}
	return err
}

func (ev *evaluator) fnTextJoin(a []*node) Value {
	sep, err := ev.text(a[0])
	if err != ErrNone {
		return errValue(err)
	}
	skip, err := ev.truth(a[1])
	if err != ErrNone {
		return errValue(err)
	}
	return ev.fnJoin(a[2:], sep, skip, true, true)
}

// text2 reads two texts, the left one first.
func (ev *evaluator) text2(a []*node) (s, t string, err ErrorCode) {
	if s, err = ev.text(a[0]); err != ErrNone {
		return "", "", err
	}
	t, err = ev.text(a[1])
	return s, t, err
}

func (ev *evaluator) fnExact(a []*node) Value {
	s, t, err := ev.text2(a)
	switch {
	case err != ErrNone:
		return errValue(err)
	case len(s) != len(t):
		return BoolValue(false)
	case !ev.walk(len(s)):
		return errValue(ErrLimit)
	}
	return BoolValue(s == t)
}

// fnFind is FIND, and SEARCH when wild is set. Positions count code
// points.
func (ev *evaluator) fnFind(a []*node, wild bool) Value {
	needle, hay, err := ev.text2(a)
	if err != ErrNone {
		return errValue(err)
	}
	start := 1.0
	if len(a) > 2 {
		if start, err = ev.whole(a[2]); err != ErrNone {
			return errValue(err)
		}
	}
	if ev.late() {
		return errValue(ErrLimit)
	}
	ev.touch(len(hay))
	hayLen := utf8.RuneCountInString(hay)
	if start < 1 || start > float64(max(hayLen, 1)) || (hay == "" && needle != "") {
		return errValue(ErrValue)
	}
	from := int(start) - 1
	if wild {
		pattern, err := ev.wildcardOf(needle)
		if err != ErrNone {
			return errValue(err)
		}
		pos := pattern.search(ev, hay, from)
		switch {
		case pos >= 0:
			return NumberValue(float64(pos + 1))
		case ev.late():
			return errValue(ErrLimit) // the search may have stopped before the place
		}
		return errValue(ErrValue)
	}
	offset := ev.offset(hay, from)
	i := ev.indexText(hay, needle, offset)
	if i < 0 {
		return errValue(ErrValue)
	}
	return NumberValue(float64(from + ev.count(hay[offset:i]) + 1))
}

// shortSearch is the longest text, in bytes, that indexText leaves to the
// standard library.
const shortSearch = 64

// indexText returns the byte at which sub first stands in s at or after
// the byte from, or -1. Capitals count.
//
// The standard library finds a short sub: at each place of s that costs
// the length of sub at the most. For a long sub its search can cost a
// sixteenth of that at each place, when sub almost matches at every
// sixteenth byte, so a long sub is found with a failure table instead, in
// one pass over s. The Env keeps the table of the last sub.
//
// The bytes of s that the search goes through are counted, with either
// search. A sub that is longer than the rest of s is not looked at.
func (ev *evaluator) indexText(s, sub string, from int) int {
	if len(sub) > len(s)-from {
		return -1
	}
	if len(sub) <= shortSearch {
		i := strings.Index(s[from:], sub)
		if i < 0 {
			ev.spend(len(s) - from)
			return -1
		}
		ev.spend(i + len(sub))
		return from + i
	}
	search := &ev.env.memory().plain
	search.set(ev, sub)
	return search.index(ev, s, from)
}

// textSearch finds one text in others byte by byte, with the table of
// Knuth, Morris and Pratt.
type textSearch struct {
	sub  string
	fail []int32 // for each start of sub, the longest shorter start that it ends with
}

func (t *textSearch) set(ev *evaluator, sub string) {
	if t.sub == sub && len(t.fail) == len(sub) {
		return
	}
	if cap(t.fail) < len(sub) {
		t.fail = make([]int32, len(sub))
	}
	t.sub, t.fail = sub, t.fail[:len(sub)]
	k := int32(0)
	t.fail[0] = 0
	for i := 1; i < len(sub); i++ {
		for k > 0 && sub[i] != sub[k] {
			k = t.fail[k-1]
		}
		if sub[i] == sub[k] {
			k++
		}
		t.fail[i] = k
	}
	ev.spend(2 * len(sub))
}

func (t *textSearch) index(ev *evaluator, s string, from int) int {
	sub, matched := t.sub, 0
	for i := from; i < len(s); i++ {
		c := s[i]
		for matched > 0 && c != sub[matched] {
			matched = int(t.fail[matched-1])
		}
		if c == sub[matched] {
			matched++
		}
		if matched == len(sub) {
			ev.spend(i + 1 - from)
			return i + 1 - matched
		}
	}
	ev.spend(len(s) - from)
	return -1
}

// countArg reads the count argument i of a text function, or def when it
// is not given. A negative count is #VALUE!.
func (ev *evaluator) countArg(a []*node, i int, def int) (int, ErrorCode) {
	if len(a) <= i {
		return def, ErrNone
	}
	x, err := ev.whole(a[i])
	switch {
	case err != ErrNone:
		return 0, err
	case x < 0:
		return 0, ErrValue
	}
	return clampInt(x), ErrNone
}

func (ev *evaluator) fnLeftRight(a []*node, right bool) Value {
	s, err := ev.text(a[0])
	if err != ErrNone {
		return errValue(err)
	}
	n, err := ev.countArg(a, 1, 1)
	switch {
	case err != ErrNone:
		return errValue(err)
	case ev.late():
		return errValue(ErrLimit)
	case !right:
		return TextValue(s[:ev.offset(s, n)])
	}
	if total := ev.count(s); n < total {
		return TextValue(s[ev.offset(s, total-n):])
	}
	return TextValue(s)
}

func (ev *evaluator) fnMid(a []*node) Value {
	s, err := ev.text(a[0])
	if err != ErrNone {
		return errValue(err)
	}
	start, err := ev.whole(a[1])
	if err != ErrNone {
		return errValue(err)
	}
	n, err := ev.countArg(a, 2, 0)
	switch {
	case err != ErrNone:
		return errValue(err)
	case start < 1:
		return errValue(ErrValue)
	case ev.late():
		return errValue(ErrLimit)
	}
	rest := s[ev.offset(s, clampInt(start)-1):]
	return TextValue(rest[:ev.offset(rest, n)])
}

func (ev *evaluator) fnLen(a []*node) Value {
	s, err := ev.text(a[0])
	switch {
	case err != ErrNone:
		return errValue(err)
	case ev.late():
		return errValue(ErrLimit)
	}
	ev.touch(len(s))
	return NumberValue(float64(utf8.RuneCountInString(s)))
}

type caseMode uint8

const (
	caseLower caseMode = iota
	caseUpper
	caseProper
)

// caseSteps is how many steps of work a byte of text counts in LOWER, UPPER
// and PROPER. Looking up the case of a letter takes about four times as
// long as comparing one.
const caseSteps = 4

// fnCase is LOWER, UPPER and PROPER. They use the simple case mapping, one
// code point to one code point, so the length never changes. PROPER starts
// a word after any character that is no letter.
func (ev *evaluator) fnCase(a []*node, mode caseMode) Value {
	s, err := ev.text(a[0])
	switch {
	case err != ErrNone:
		return errValue(err)
	case ev.late():
		return errValue(ErrLimit)
	}
	ev.touch(caseSteps * len(s))
	prevLetter := false
	return TextValue(strings.Map(func(r rune) rune {
		switch {
		case mode == caseLower, mode == caseProper && prevLetter:
			prevLetter = unicode.IsLetter(r)
			return unicode.ToLower(r)
		}
		prevLetter = unicode.IsLetter(r)
		return unicode.ToUpper(r)
	}, s))
}

func (ev *evaluator) fnReplace(a []*node) Value {
	s, err := ev.text(a[0])
	if err != ErrNone {
		return errValue(err)
	}
	start, err := ev.whole(a[1])
	if err != ErrNone {
		return errValue(err)
	}
	n, err := ev.countArg(a, 2, 0)
	if err != ErrNone {
		return errValue(err)
	}
	with, err := ev.text(a[3])
	switch {
	case err != ErrNone:
		return errValue(err)
	case start < 1:
		return errValue(ErrValue)
	case ev.late():
		return errValue(ErrLimit)
	}
	head := ev.offset(s, clampInt(start)-1)
	tail := head + ev.offset(s[head:], n)
	return ev.textResult(s[:head] + with + s[tail:])
}

func (ev *evaluator) fnRept(a []*node) Value {
	s, err := ev.text(a[0])
	if err != ErrNone {
		return errValue(err)
	}
	n, err := ev.countArg(a, 1, 0)
	switch {
	case err != ErrNone:
		return errValue(err)
	case ev.late():
		return errValue(ErrLimit)
	}
	if runes := ev.count(s); runes > 0 && n > MaxTextLen/runes {
		return errValue(ErrValue)
	}
	if s == "" {
		return TextValue("")
	}
	ev.touch(len(s) * n)
	return TextValue(strings.Repeat(s, n))
}

func (ev *evaluator) fnSubstitute(a []*node) Value {
	s, err := ev.text(a[0])
	if err != ErrNone {
		return errValue(err)
	}
	old, err := ev.text(a[1])
	if err != ErrNone {
		return errValue(err)
	}
	with, err := ev.text(a[2])
	if err != ErrNone {
		return errValue(err)
	}
	which := 0.0
	if len(a) > 3 {
		if which, err = ev.whole(a[3]); err != ErrNone {
			return errValue(err)
		}
		if which < 1 {
			return errValue(ErrValue)
		}
	}
	if old == "" {
		return TextValue(s)
	}
	if ev.late() {
		return errValue(ErrLimit)
	}
	ev.touch(len(s))
	if len(a) <= 3 {
		count := 0
		for pos := 0; ; count++ {
			i := ev.indexText(s, old, pos)
			if i < 0 {
				break
			}
			pos = i + len(old)
		}
		if count == 0 {
			return TextValue(s)
		}
		grow := ev.count(with) - ev.count(old)
		if grow > 0 && ev.count(s)+count*grow > MaxTextLen {
			return errValue(ErrValue)
		}
		var b strings.Builder
		b.Grow(len(s) + count*(len(with)-len(old)))
		pos := 0
		for i := ev.indexText(s, old, 0); i >= 0; i = ev.indexText(s, old, pos) {
			b.WriteString(s[pos:i])
			b.WriteString(with)
			pos = i + len(old)
		}
		b.WriteString(s[pos:])
		return ev.textResult(b.String())
	}
	pos := 0
	for k := 1.0; ; k++ {
		i := ev.indexText(s, old, pos)
		if i < 0 {
			return TextValue(s)
		}
		pos = i
		if k == which {
			break
		}
		pos += len(old)
	}
	if ev.count(s)-ev.count(old)+ev.count(with) > MaxTextLen {
		return errValue(ErrValue)
	}
	return ev.textResult(s[:pos] + with + s[pos+len(old):])
}

// fnTrim drops the ASCII spaces at the ends and keeps one between words.
func (ev *evaluator) fnTrim(a []*node) Value {
	s, err := ev.text(a[0])
	switch {
	case err != ErrNone:
		return errValue(err)
	case ev.late():
		return errValue(ErrLimit)
	}
	ev.touch(len(s))
	if !strings.HasPrefix(s, " ") && !strings.HasSuffix(s, " ") && !strings.Contains(s, "  ") {
		return TextValue(s)
	}
	var b strings.Builder
	b.Grow(len(s))
	for s != "" {
		end := strings.IndexByte(s, ' ')
		if end < 0 {
			end = len(s)
		}
		if end > 0 {
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(s[:end])
		}
		s = s[min(end+1, len(s)):]
	}
	return TextValue(b.String())
}

// textFormat is a pattern TEXT accepts: a number format of rule 6, where
// a currency prefix may also stand before a form without a thousands
// separator.
type textFormat struct {
	nf        NumberFormat
	thousands bool
}

// parseTextPattern reads the patterns of TEXT: 0 and 0.00 forms with 0 to
// 10 decimals, #,##0 forms, 0% forms, a currency prefix of 1 to 3 letters
// or currency signs before either, and the date and time patterns.
func parseTextPattern(p string) (textFormat, bool) {
	if dp, ok := parseDatePattern(p); ok {
		return textFormat{nf: NumberFormat{Kind: NumberDate, Date: dp}}, true
	}
	if tp, ok := parseTimePattern(p); ok {
		return textFormat{nf: NumberFormat{Kind: NumberTime, Time: tp}}, true
	}
	for i, dp := range datePatternText {
		if strings.HasPrefix(p, dp) && len(p) > len(dp) && p[len(dp)] == ' ' {
			if tp, ok := parseTimePattern(p[len(dp)+1:]); ok {
				return textFormat{nf: NumberFormat{Kind: NumberDateTime, Date: DatePattern(i), Time: tp}}, true
			}
		}
	}
	symbol, rest := "", p
	for n := 0; n <= 3; n++ {
		if decimals, thousands, percent, ok := parseNumberPattern(rest); ok && !(percent && (symbol != "" || thousands)) {
			switch {
			case percent:
				return textFormat{nf: NumberFormat{Kind: NumberPercent, Decimals: decimals}}, true
			case symbol != "":
				return textFormat{nf: NumberFormat{Kind: NumberCurrency, Symbol: symbol, Decimals: decimals}, thousands: thousands}, true
			}
			return textFormat{nf: NumberFormat{Kind: NumberFixed, Decimals: decimals, Thousands: thousands}, thousands: thousands}, true
		}
		r, size := utf8.DecodeRuneInString(rest)
		if rest == "" || !isSymbolRune(r) {
			break
		}
		symbol, rest = p[:len(symbol)+size], rest[size:]
	}
	return textFormat{}, false
}

// parseNumberPattern reads "0", "0.00", "#,##0", "#,##0.00" and the same
// with "%" at the end.
func parseNumberPattern(p string) (decimals uint8, thousands, percent, ok bool) {
	if strings.HasPrefix(p, "#,##") {
		thousands = true
		p = p[4:]
	}
	if !strings.HasPrefix(p, "0") {
		return 0, false, false, false
	}
	p = p[1:]
	if strings.HasPrefix(p, ".") {
		// A pattern has at most 10 decimals. An eleventh ends the reading,
		// so a long pattern is not gone through.
		n := 1
		for n < len(p) && n <= 11 && p[n] == '0' {
			n++
		}
		if n == 1 || n > 11 {
			return 0, false, false, false
		}
		decimals = uint8(n - 1)
		p = p[n:]
	}
	if p == "%" {
		return decimals, thousands, true, true
	}
	return decimals, thousands, false, p == ""
}

// isSymbolRune reports whether r may be part of a currency symbol: a
// letter or a currency sign.
func isSymbolRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.Is(unicode.Sc, r)
}

// fnText shows a number in a pattern. Text that is no number comes back
// unchanged, and the pattern is checked first.
func (ev *evaluator) fnText(a []*node) Value {
	v := ev.eval(a[0])
	if v.kind == KindError {
		return v
	}
	pattern, err := ev.text(a[1])
	if err != ErrNone {
		return errValue(err)
	}
	tf, ok := parseTextPattern(pattern)
	if !ok {
		return errValue(ErrValue)
	}
	x, err := ev.number(v)
	switch {
	case err == ErrLimit:
		return errValue(err)
	case err != ErrNone:
		return v // text that is no number
	}
	var buf [48]byte
	return TextValue(string(appendNumber(buf[:0], x, tf.nf, tf.thousands)))
}

// fnValue reads text as typed entry reads it. A boolean is no number here.
func (ev *evaluator) fnValue(a []*node) Value {
	v := ev.eval(a[0])
	if v.kind == KindBool {
		return errValue(ErrValue)
	}
	x, err := ev.number(v)
	if err != ErrNone {
		return errValue(err)
	}
	return NumberValue(x)
}
