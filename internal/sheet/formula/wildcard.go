package formula

import (
	"slices"
	"unicode/utf8"
)

// A wildcard pattern is matched without regard to capitals. "*" stands for
// any run of characters, "?" for one character, and "~" before "*", "?" or
// "~" for that character itself.
//
// A pattern that holds a "*" or a "?" as a wildcard has at most
// MaxPatternLen code points: a longer one is #VALUE!. A pattern without a
// wildcard is plain text of any length.
//
// A pattern is matched as it is written. No compiled form is kept for a
// pattern, so a formula with many patterns holds no more than their text.
// The pieces between the "*"s are matched in turn: the first at the start
// of the text, the last at its end when the whole text must match, and
// every other one at the first place it is found after the piece before it.
//
// Matching is work of the calculation, counted in steps, 64 to a unit of
// the budget, and a match stops when the deadline passes. One match of a
// pattern of p code points against a text of t code points takes at most
// 8*(t+p) + 8192 steps, whatever the pattern and the text are. A piece is
// first looked for by trying each place of the text in turn. Once a match
// has spent a pass or so over the text that way, because a piece almost
// matches at many places, each piece is found in one pass over the text
// instead: with a failure table when it has no "?", and otherwise with one
// bit for each of its characters, which the limit on the length of a
// pattern keeps to four words. The Env keeps the last table of each kind
// for the other cells of a range.
//
// The steps of one match add up as follows, for a text of at most four
// bytes to a code point. Trying places: the bytes of the text and of the
// pattern and 64, and the last try, at most 4t + 5p + 65. One pass with a
// table: at most 4t, and p for stepping back over the pieces found. The
// tables: 2 for each character of a piece without "?", and 11 and then 17
// more for a piece with "?", at most 19.5p + 9. The first piece, the last
// one and the "*"s: at most 3p + 3. That is 8t + 28.5p + 77, and a pattern
// with "?" has at most 255 characters.
type patternKind uint8

const (
	patternRune patternKind = iota // one character, folded
	patternOne                     // "?"
	patternMany                    // "*"
)

// patternToken reads the token at the start of a pattern that is not
// empty.
func patternToken(p string) (kind patternKind, r rune, size int) {
	c := p[0]
	switch {
	case c == '*':
		return patternMany, 0, 1
	case c == '?':
		return patternOne, 0, 1
	case c == '~' && len(p) > 1 && (p[1] == '*' || p[1] == '?' || p[1] == '~'):
		return patternRune, rune(p[1]), 2
	case c < utf8.RuneSelf:
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		return patternRune, rune(c), 1
	}
	r, size = utf8.DecodeRuneInString(p)
	return patternRune, foldRune(r), size
}

// foldedRune reads the code point at the start of text that is not empty,
// folded.
func foldedRune(s string) (rune, int) {
	c := s[0]
	if c < utf8.RuneSelf {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		return rune(c), 1
	}
	r, size := utf8.DecodeRuneInString(s)
	return foldRune(r), size
}

// spendStep is how many small steps of work are counted between two
// additions to the budget.
const spendStep = 4096

// spend counts small steps of work, 64 to a unit of the budget. A step is
// a byte of a text that is compared, read as a number, counted, cut,
// searched or copied, a code point that a piece of a pattern is tried
// against or looked for in (maskSteps where it is looked for with bits),
// a bit of a conditional total, a percent sign, or a count of the tables
// that find a number by its rank. A part of a formula is partSteps, a
// whole unit.
func (ev *evaluator) spend(points int) {
	ev.spent += points
	ev.points += points
	if ev.points >= spendStep {
		ev.settle()
	}
}

// settle adds the whole units of the small steps to the budget. It is
// apart from spend, and not inlined into it, so that spend itself is
// inlined and costs no call where it adds nothing.
//
//go:noinline
func (ev *evaluator) settle() {
	ev.env.Budget.Add(int64(ev.points / 64))
	ev.points %= 64
}

// matchPiece matches the piece of the pattern p before its first "*", all
// of p without one, at the start of text. It returns the bytes of p and of
// text that the piece covers. short is set when the text ended before the
// piece did: the piece is then found at no later place either.
func (ev *evaluator) matchPiece(p, text string) (pn, tn int, ok, short bool) {
	steps := 1
	for pn < len(p) {
		kind, r, size := patternToken(p[pn:])
		if kind == patternMany {
			break
		}
		if tn == len(text) {
			ev.spend(steps)
			return 0, 0, false, true
		}
		c, n := foldedRune(text[tn:])
		if kind == patternRune && c != r {
			ev.spend(steps)
			return 0, 0, false, false
		}
		pn, tn = pn+size, tn+n
		steps++
	}
	ev.spend(steps)
	return pn, tn, true, false
}

// allowTries starts a match of a pattern against a text: it sets how much
// the match may spend on trying a piece at each place in turn. Past that,
// findPiece finds each piece in one pass.
func (ev *evaluator) allowTries(pattern, text string) {
	ev.tries = ev.spent + len(text) + len(pattern) + 64
}

// findPiece finds the first place in text where the piece of p before its
// first "*" matches. It returns the bytes and the code points of text
// before that place, and the bytes of p and of text that the piece covers.
func (ev *evaluator) findPiece(p, text string) (at, runes, pn, tn int, ok bool) {
	if kept := ev.env.kept; kept != nil && p != "" {
		switch p {
		case kept.search.pattern:
			return kept.search.find(ev, text, 0, 0)
		case kept.masks.pattern:
			return kept.masks.find(ev, text, 0, 0)
		}
	}
	for ev.spent <= ev.tries {
		pn, tn, ok, short := ev.matchPiece(p, text[at:])
		if ok {
			return at, runes, pn, tn, true
		}
		if short || ev.env.Budget.Exceeded() {
			return 0, 0, 0, 0, false
		}
		_, n := utf8.DecodeRuneInString(text[at:])
		at += n
		runes++
	}
	kept := ev.env.memory()
	if kept.search.set(ev, p) {
		return kept.search.find(ev, text, at, runes)
	}
	if kept.masks.set(ev, p) {
		return kept.masks.find(ev, text, at, runes)
	}
	// A piece with "?" that is too long for the bits: the limit on the
	// length of a pattern rules it out. It is tried at each place.
	for {
		pn, tn, ok, short := ev.matchPiece(p, text[at:])
		if ok {
			return at, runes, pn, tn, true
		}
		if short || ev.env.Budget.Exceeded() {
			return 0, 0, 0, 0, false
		}
		_, n := utf8.DecodeRuneInString(text[at:])
		at += n
		runes++
	}
}

// found returns what findPiece returns for a piece of the given bytes and
// code points that ends at the byte end of text, after runes code points.
func found(text string, end, runes, size, length int) (int, int, int, int, bool) {
	start := end
	for k := length; k > 0; k-- {
		_, n := utf8.DecodeLastRuneInString(text[:start])
		start -= n
	}
	return start, runes - length, size, end - start, true
}

// pieceSearch finds one piece of a wildcard pattern that has no "?" in a
// single pass over a text, with the table of Knuth, Morris and Pratt. An
// Env keeps the table of the piece it searched for last, so the cells of a
// range are all searched with one table.
type pieceSearch struct {
	pattern string  // the pattern from the piece on, as findPiece takes it
	size    int     // the bytes of the piece in pattern
	runes   []rune  // the piece, folded
	fail    []int32 // for each start of the piece, the longest shorter start that it ends with
}

// set makes the table for the piece at the start of p. It reports false
// for a piece that holds a "?".
func (s *pieceSearch) set(ev *evaluator, p string) bool {
	s.pattern, s.runes = "", s.runes[:0]
	size := 0
	for size < len(p) {
		kind, r, n := patternToken(p[size:])
		if kind == patternMany {
			break
		}
		if kind == patternOne {
			ev.spend(len(s.runes) + 1)
			return false
		}
		s.runes = append(s.runes, r)
		size += n
	}
	if cap(s.fail) < len(s.runes) {
		s.fail = make([]int32, len(s.runes))
	}
	s.fail = s.fail[:len(s.runes)]
	k := int32(0)
	for i := 1; i < len(s.runes); i++ {
		for k > 0 && s.runes[i] != s.runes[k] {
			k = s.fail[k-1]
		}
		if s.runes[i] == s.runes[k] {
			k++
		}
		s.fail[i] = k
	}
	s.fail[0] = 0
	s.pattern, s.size = p, size
	ev.spend(2 * len(s.runes))
	return true
}

// find finds the first place of the piece in text at or after the byte at,
// which runes code points come before. It returns what findPiece returns.
func (s *pieceSearch) find(ev *evaluator, text string, at, runes int) (int, int, int, int, bool) {
	matched, steps := 0, 0 // the code points of the piece that match so far
	for i := at; i < len(text); {
		c, n := foldedRune(text[i:])
		for matched > 0 && c != s.runes[matched] {
			matched = int(s.fail[matched-1])
		}
		if c == s.runes[matched] {
			matched++
		}
		i += n
		runes++
		if matched == len(s.runes) {
			// This code point is a step as well, and found steps back
			// over the piece.
			ev.spend(steps + 1 + matched)
			return found(text, i, runes, s.size, matched)
		}
		if steps++; steps == spendStep {
			ev.spend(steps)
			steps = 0
			if ev.env.Budget.Exceeded() {
				return 0, 0, 0, 0, false
			}
		}
	}
	ev.spend(steps)
	return 0, 0, 0, 0, false
}

// bitRow has one bit for each character of a piece of a pattern that holds
// a wildcard: such a pattern has at most MaxPatternLen characters, which
// four words hold.
type bitRow [4]uint64

var _ [len(bitRow{})*64 - MaxPatternLen]struct{} // a bitRow holds a pattern

// runeBits is the characters of a piece that one code point matches.
type runeBits struct {
	r    rune
	bits bitRow
}

// maskSteps is what one code point of the text costs in pieceMasks.find:
// it moves a bitRow.
const maskSteps = len(bitRow{})

// pieceMasks finds one piece of a wildcard pattern that holds "?" in a
// single pass over a text. It keeps a bit for each character of the piece:
// the bit is set while the text up to here ends with the piece up to that
// character. Each code point of the text moves the bits one character on
// and keeps those whose character it matches (Shift-And). An Env keeps the
// bits of the piece it searched for last.
type pieceMasks struct {
	pattern string               // the pattern from the piece on, as findPiece takes it
	size    int                  // the bytes of the piece in pattern
	length  int                  // the characters of the piece
	runes   []runeBits           // for each code point of the piece, in order, the characters it matches
	any     bitRow               // the characters that are "?", which every code point matches
	ascii   [utf8.RuneSelf]uint8 // for a byte of ASCII, capitals too, its place in runes and one more, or 0
}

// set reads the piece at the start of p. It reports false for a piece of
// more characters than a bitRow has bits.
func (s *pieceMasks) set(ev *evaluator, p string) bool {
	s.pattern, s.runes, s.any = "", s.runes[:0], bitRow{}
	size, length := 0, 0
	for size < len(p) {
		kind, r, n := patternToken(p[size:])
		if kind == patternMany {
			break
		}
		if length == len(s.any)*64 {
			ev.spend(length)
			return false
		}
		if kind == patternOne {
			s.any[length>>6] |= 1 << (length & 63)
		} else {
			s.runes = append(s.runes, runeBits{r: r})
			s.runes[len(s.runes)-1].bits[length>>6] = 1 << (length & 63)
		}
		size += n
		length++
	}
	// One entry for each code point, in order, so that find looks a code
	// point up in a few steps.
	slices.SortFunc(s.runes, func(a, b runeBits) int { return int(a.r) - int(b.r) })
	merged := s.runes[:0]
	for _, e := range s.runes {
		if n := len(merged); n > 0 && merged[n-1].r == e.r {
			for w := range e.bits {
				merged[n-1].bits[w] |= e.bits[w]
			}
			continue
		}
		merged = append(merged, e)
	}
	s.runes = merged
	s.ascii = [utf8.RuneSelf]uint8{}
	for i := range s.runes {
		for w := range s.any {
			s.runes[i].bits[w] |= s.any[w]
		}
		if r := s.runes[i].r; r < utf8.RuneSelf {
			s.ascii[r] = uint8(i + 1)
			if r >= 'a' && r <= 'z' {
				s.ascii[r-'a'+'A'] = uint8(i + 1)
			}
		}
	}
	s.pattern, s.size, s.length = p, size, length
	ev.spend(10*length + 16) // reading the piece, and putting its code points in order
	return true
}

// find finds the first place of the piece in text at or after the byte at,
// which runes code points come before. It returns what findPiece returns.
func (s *pieceMasks) find(ev *evaluator, text string, at, runes int) (int, int, int, int, bool) {
	var ends bitRow // the characters of the piece that the text up to here ends with
	last := s.length - 1
	word, bit := last>>6, uint64(1)<<(last&63)
	steps := 0
	for i := at; i < len(text); {
		match := &s.any
		if c := text[i]; c < utf8.RuneSelf {
			if k := s.ascii[c]; k > 0 {
				match = &s.runes[k-1].bits
			}
			i++
		} else {
			r, n := utf8.DecodeRuneInString(text[i:])
			r = foldRune(r)
			for lo, hi := 0, len(s.runes); lo < hi; {
				mid := (lo + hi) / 2
				switch e := &s.runes[mid]; {
				case e.r < r:
					lo = mid + 1
				case e.r > r:
					hi = mid
				default:
					match, lo = &e.bits, hi
				}
			}
			i += n
		}
		// Every character moves on by one, and the first one starts anew.
		ends[3] = (ends[3]<<1 | ends[2]>>63) & match[3]
		ends[2] = (ends[2]<<1 | ends[1]>>63) & match[2]
		ends[1] = (ends[1]<<1 | ends[0]>>63) & match[1]
		ends[0] = (ends[0]<<1 | 1) & match[0]
		runes++
		if ends[word]&bit != 0 {
			// This code point moved the bits as well, and found steps
			// back over the piece.
			ev.spend(steps + maskSteps + s.length)
			return found(text, i, runes, s.size, s.length)
		}
		if steps += maskSteps; steps >= spendStep {
			ev.spend(steps)
			steps = 0
			if ev.env.Budget.Exceeded() {
				return 0, 0, 0, 0, false
			}
		}
	}
	ev.spend(steps)
	return 0, 0, 0, 0, false
}

// findPieces reports whether the pieces of p are found in text one after
// another. p is empty or starts with a "*".
func (ev *evaluator) findPieces(p, text string) bool {
	for {
		stars := 0
		for stars < len(p) && p[stars] == '*' {
			stars++
		}
		ev.spend(stars)
		if p = p[stars:]; p == "" {
			return true
		}
		at, _, pn, tn, ok := ev.findPiece(p, text)
		if !ok {
			return false
		}
		p, text = p[pn:], text[at+tn:]
	}
}

// wildcard is a pattern that was read once and is matched many times.
type wildcard struct {
	pattern string
	star    bool // the pattern holds a "*"
	tail    int  // the bytes of the pattern up to its last "*", that "*" included
	tailLen int  // the code points that the piece after the last "*" stands for
}

// wildcardOf reads a pattern. A pattern that holds a wildcard and is
// longer than MaxPatternLen code points is #VALUE!.
func (ev *evaluator) wildcardOf(pattern string) (wildcard, ErrorCode) {
	w := wildcard{pattern: pattern}
	wild := false
	for i := 0; i < len(pattern); {
		kind, _, size := patternToken(pattern[i:])
		i += size
		wild = wild || kind != patternRune
		if kind == patternMany {
			w.star, w.tail, w.tailLen = true, i, 0
		} else {
			w.tailLen++
		}
	}
	ev.spend(len(pattern))
	if wild && longerThan(pattern, MaxPatternLen) {
		return wildcard{}, ErrValue
	}
	return w, ErrNone
}

// match reports whether the pattern matches all of text.
func (w *wildcard) match(ev *evaluator, text string) bool {
	p := w.pattern
	pn, tn, ok, _ := ev.matchPiece(p, text)
	switch {
	case !ok:
		return false
	case !w.star:
		return tn == len(text)
	}
	// The piece after the last "*" stands at the end of the text.
	text = text[tn:]
	cut := len(text)
	if cut < w.tailLen {
		return false
	}
	ev.spend(w.tailLen) // the steps back, also when the text ends before them
	for n := w.tailLen; n > 0; n-- {
		if cut == 0 {
			return false
		}
		_, size := utf8.DecodeLastRuneInString(text[:cut])
		cut -= size
	}
	if _, end, ok, _ := ev.matchPiece(p[w.tail:], text[cut:]); !ok || end != len(text)-cut {
		return false
	}
	ev.allowTries(p, text)
	return ev.findPieces(p[pn:w.tail], text[:cut])
}

// search returns the code point index of the first place in text, at or
// after the code point index from, where the pattern matches, or -1.
func (w *wildcard) search(ev *evaluator, text string, from int) int {
	start := 0
	ev.spend(from)
	for n := from; n > 0; n-- {
		if start == len(text) {
			return -1
		}
		_, size := utf8.DecodeRuneInString(text[start:])
		start += size
	}
	p := w.pattern
	ev.allowTries(p, text[start:])
	at, runes, pn, tn, ok := ev.findPiece(p, text[start:])
	// What follows the first "*" may match anywhere later. If it does not
	// match after this place, it matches after no later one.
	if !ok || !ev.findPieces(p[pn:], text[start+at+tn:]) {
		return -1
	}
	return from + runes
}
