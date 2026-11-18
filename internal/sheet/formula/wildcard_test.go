package formula

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// The oracle is a second matcher of wildcard patterns for the tests: it
// reads a pattern into tokens first and tries again after each "*", where
// the matcher of the package works on the text of the pattern piece by
// piece.

type oracleToken struct {
	kind patternKind
	r    rune // folded
}

func oraclePattern(pattern string) []oracleToken {
	var toks []oracleToken
	for i := 0; i < len(pattern); {
		r, n := utf8.DecodeRuneInString(pattern[i:])
		i += n
		switch {
		case r == '~' && i < len(pattern) && (pattern[i] == '*' || pattern[i] == '?' || pattern[i] == '~'):
			toks = append(toks, oracleToken{patternRune, rune(pattern[i])})
			i++
		case r == '*':
			toks = append(toks, oracleToken{kind: patternMany})
		case r == '?':
			toks = append(toks, oracleToken{kind: patternOne})
		default:
			toks = append(toks, oracleToken{patternRune, foldRune(r)})
		}
	}
	return toks
}

// oracleMatch reports whether the tokens match text: all of it with whole
// set, otherwise a start of it.
func oracleMatch(toks []oracleToken, text string, whole bool) bool {
	ti, pi := 0, 0
	starP, starT := -1, 0
	for {
		if pi == len(toks) && (!whole || ti == len(text)) {
			return true
		}
		if pi < len(toks) && toks[pi].kind == patternMany {
			starP, starT = pi, ti
			pi++
			continue
		}
		if pi < len(toks) && ti < len(text) {
			r, n := utf8.DecodeRuneInString(text[ti:])
			if toks[pi].kind == patternOne || toks[pi].r == foldRune(r) {
				ti += n
				pi++
				continue
			}
		}
		if starP < 0 || starT >= len(text) {
			return false
		}
		_, n := utf8.DecodeRuneInString(text[starT:])
		starT += n
		ti, pi = starT, starP+1
	}
}

// oracleSearch returns the code point index of the first place in text, at
// or after the code point index from, where a start of the rest of the
// text matches, or -1.
func oracleSearch(toks []oracleToken, text string, from int) int {
	at := 0
	for index := 0; ; index++ {
		if index >= from && oracleMatch(toks, text[at:], false) {
			return index
		}
		if at >= len(text) {
			return -1
		}
		_, n := utf8.DecodeRuneInString(text[at:])
		at += n
	}
}

// wildcardEnv is the Env of every match the tests make, so that the table
// it keeps for one pattern meets the next pattern.
var wildcardEnv = &Env{}

// mustWildcard reads a pattern that is not too long for one.
func mustWildcard(t testing.TB, ev *evaluator, pattern string) wildcard {
	t.Helper()
	w, err := ev.wildcardOf(pattern)
	if err != ErrNone {
		t.Fatalf("the pattern %.20q of %d characters is %v", pattern, utf8.RuneCountInString(pattern), err)
	}
	return w
}

// checkWildcard compares a match of the whole text, and a search from
// each of the given places or else from every place, with the oracle.
func checkWildcard(t testing.TB, pattern, text string, from ...int) {
	t.Helper()
	ev := &evaluator{env: wildcardEnv}
	toks := oraclePattern(pattern)
	w := mustWildcard(t, ev, pattern)
	if got, want := w.match(ev, text), oracleMatch(toks, text, true); got != want {
		t.Fatalf("the pattern %q matches %q: %v, want %v", pattern, text, got, want)
	}
	if from == nil {
		for at := 0; at <= utf8.RuneCountInString(text)+1; at++ {
			from = append(from, at)
		}
	}
	for _, at := range from {
		if got, want := w.search(ev, text, at), oracleSearch(toks, text, at); got != want {
			t.Fatalf("the pattern %q is found in %q from %d at %d, want %d", pattern, text, at, got, want)
		}
	}
}

func TestWildcardPatterns(t *testing.T) {
	for _, c := range []struct {
		pattern, text string
		match         bool
		found         int // where SEARCH finds the pattern from the start, or -1
	}{
		{"", "", true, 0},
		{"", "a", false, 0},
		{"a", "A", true, 0},
		{"a", "ba", false, 1},
		{"*", "", true, 0},
		{"*", "abc", true, 0},
		{"**", "abc", true, 0},
		{"?", "", false, -1},
		{"?", "한", true, 0},
		{"?", "ab", false, 0},
		{"a*", "abc", true, 0},
		{"*c", "abc", true, 0},
		{"*c", "abcd", false, 0},
		{"a*c", "abc", true, 0},
		{"a*c", "ac", true, 0},
		{"a*c", "a", false, -1},
		{"a*a", "a", false, -1},
		{"a*a", "aa", true, 0},
		{"*a*", "bab", true, 0},
		{"b*b", "abab", false, 1},
		{"a?c", "abc", true, 0},
		{"a?c", "ac", false, -1},
		{"a??", "abc", true, 0},
		{"*?", "", false, -1},
		{"*?", "a", true, 0},
		{"?*?", "a", false, -1},
		{"?*?", "ab", true, 0},
		{"~*", "*", true, 0},
		{"~*", "a", false, -1},
		{"~?", "?", true, 0},
		{"~?", "a", false, -1},
		{"~~", "~", true, 0},
		{"~", "~", true, 0},
		{"~a", "~a", true, 0},
		{"a~", "a~", true, 0},
		{"~~*", "~abc", true, 0},
		{"~~~*", "~*", true, 0},
		{"~~~*", "~a", false, -1},
		{"*~*", "a*", true, 0},
		{"*~**", "a*b", true, 0},
		{"ab*ab*ab", "abab", false, -1},
		{"ab*ab*ab", "ababab", true, 0},
		{"*ab*ab", "abab", true, 0},
		{"*aab", "aaab", true, 0},
		{"*aab*", "aaabab", true, 0},
		{"a*ba", "aaba", true, 0},
		{"ſ", "S", true, 0},
		{"S*", "ſtraße", true, 0},
		{"*ß", "STRAẞ", true, 0},
		{"*ß", "strass", false, -1},
		{"k", "K", true, 0},
		{"K?", "k한", true, 0},
		{"é*", "École", true, 0},
		{"*한", "대한", true, 0},
		{"한*", "대한", false, 1},
	} {
		ev := &evaluator{env: &Env{}}
		w := mustWildcard(t, ev, c.pattern)
		if got := w.match(ev, c.text); got != c.match {
			t.Errorf("the pattern %q matches %q: %v, want %v", c.pattern, c.text, got, c.match)
		}
		if got := w.search(ev, c.text, 0); got != c.found {
			t.Errorf("the pattern %q is found in %q at %d, want %d", c.pattern, c.text, got, c.found)
		}
		checkWildcard(t, c.pattern, c.text)
	}
}

// Every short pattern over a small alphabet agrees with the oracle on every
// short text, and so do random longer ones over letters that fold.
func TestWildcardAgreesWithTheOracle(t *testing.T) {
	var words func(alphabet string, n int, prefix string, visit func(string))
	words = func(alphabet string, n int, prefix string, visit func(string)) {
		visit(prefix)
		if n == 0 {
			return
		}
		for _, r := range alphabet {
			words(alphabet, n-1, prefix+string(r), visit)
		}
	}
	var texts []string
	words("ab*", 5, "", func(s string) { texts = append(texts, s) })
	ev := &evaluator{env: &Env{}}
	words("ab*?~", 5, "", func(pattern string) {
		toks := oraclePattern(pattern)
		w := mustWildcard(t, ev, pattern)
		for _, text := range texts {
			if w.match(ev, text) != oracleMatch(toks, text, true) || w.search(ev, text, 0) != oracleSearch(toks, text, 0) {
				checkWildcard(t, pattern, text)
			}
		}
	})

	rng := rand.New(rand.NewSource(20261113))
	pick := func(alphabet []string, n int) string {
		var b strings.Builder
		for ; n > 0; n-- {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		return b.String()
	}
	letters := []string{"a", "a", "A", "b", "s", "ſ", "k", "K", "é", "É", "한", "ß", "ẞ"}
	marks := append([]string{"*", "*", "?", "~", "~*", "~?", "~~"}, letters...)
	for i := 0; i < 200000; i++ {
		checkWildcard(t, pick(marks, rng.Intn(10)), pick(append(letters, "*", "?", "~"), rng.Intn(14)))
	}
	// Long runs of one letter, where every place of the text almost
	// matches: with single wildcards, and without, where a piece is found
	// with a table after the first places have failed.
	runs := []string{"a", "a", "a", "a", "a", "a", "b", "*", "?"}
	for i := 0; i < 20000; i++ {
		checkWildcard(t, pick(runs, rng.Intn(24)), pick(runs[:7], rng.Intn(40)))
	}
	piece := func() string {
		letters := []string{"a", "a", "a", "a", "A", "ab", "aab", "b", "\u212a", "k"}
		return pick(letters, 1+rng.Intn(30))
	}
	for i := 0; i < 3000; i++ {
		pattern := piece()
		for n := rng.Intn(4); n > 0; n-- {
			pattern += "*" + piece()
		}
		switch rng.Intn(4) {
		case 0:
			pattern = "*" + pattern
		case 1:
			pattern += "*"
		case 2:
			pattern = "*" + pattern + "*"
		}
		text := pick([]string{"a", "a", "a", "a", "a", "A", "aab", "k", "b"}, rng.Intn(400))
		checkWildcard(t, pattern, text, 0, 1, rng.Intn(len(text)+2), rng.Intn(len(text)+2))
	}
	// A text of one letter with a few others in it, and pieces that hold
	// one other letter: a piece fails at many places before the place it
	// is found at, far enough into the text for a table to find it. A
	// piece without "?" has a failure table and one with "?" has bits.
	tables, masks := 0, 0
	for i := 0; i < 500; i++ {
		n := 100 + rng.Intn(200)
		text := []byte(strings.Repeat("a", n))
		for k := rng.Intn(5); k > 0; k-- {
			text[rng.Intn(n)] = "bBk"[rng.Intn(3)]
		}
		single := i%2 == 1
		piece := func(length int) string {
			at := rng.Intn(length)
			p := []byte(strings.Repeat("a", at) + "b" + strings.Repeat("A", length-at-1))
			if single {
				for k := 1 + rng.Intn(3); k > 0; k-- {
					p[rng.Intn(length)] = '?'
				}
			}
			return string(p)
		}
		length := 8 + rng.Intn(40)
		for _, pattern := range []string{piece(length), "*" + piece(length), piece(length) + "*", "*" + piece(length) + "*", "*" + piece(length) + "*" + piece(length) + "*"} {
			kept := wildcardEnv.memory()
			kept.search.pattern, kept.masks.pattern = "stale", "stale"
			checkWildcard(t, pattern, string(text), rng.Intn(n/4))
			if kept.search.pattern != "stale" {
				tables++
			}
			if kept.masks.pattern != "stale" {
				masks++
			}
		}
	}
	if tables < 250 || masks < 250 {
		t.Errorf("of 2,500 patterns only %d were searched for with a failure table and %d with bits", tables, masks)
	}
	// Pieces with "?" of every length around the ends of the words that
	// hold their bits, up to the longest a pattern may have, with letters
	// of several scripts.
	for _, length := range []int{1, 2, 3, 62, 63, 64, 65, 66, 127, 128, 129, 130, 191, 192, 193, 194, 250, 252, 253} {
		for i := 0; i < 40; i++ {
			letters := []string{"a", "a", "a", "a", "a", "a", "A", "b", "é", "한", "\u212a", "ß"}
			var p, text strings.Builder
			for k := 0; k < length; k++ {
				if rng.Intn(6) == 0 || length < 3 && k == 0 {
					p.WriteString("?")
				} else {
					p.WriteString(letters[rng.Intn(len(letters))])
				}
			}
			for k := 500 + rng.Intn(500); k > 0; k-- {
				text.WriteString(letters[rng.Intn(7)])
			}
			// Half of the texts hold the piece, with other letters where
			// it has "?".
			if i%2 == 0 {
				text.WriteString(strings.ReplaceAll(p.String(), "?", "é"))
				text.WriteString(letters[rng.Intn(len(letters))])
			}
			pattern := []string{"*" + p.String() + "*", "*" + p.String(), p.String() + "*", "*a*" + p.String() + "*"}[i%4]
			if utf8.RuneCountInString(pattern) > MaxPatternLen {
				pattern = "*" + p.String() + "*"
			}
			checkWildcard(t, pattern, text.String(), 0, rng.Intn(400))
		}
	}
}

func FuzzWildcard(f *testing.F) {
	f.Add("a*b?c", "aXbYc")
	f.Add("*~**", "a*b")
	f.Add("ſ*한", "S대한")
	f.Add("\xff*", "\xff\xfe")
	f.Fuzz(func(t *testing.T, pattern, text string) {
		if len(pattern) > 64 || len(text) > 256 {
			return
		}
		checkWildcard(t, pattern, text)
	})
}

// A formula with many criteria holds their text and nothing more: a
// pattern is matched as written, with no memory of its own. The table of a
// piece that almost matches at many places belongs to the Env, which keeps
// one of each kind and uses it again.
func TestWildcardMatchingAllocatesNothing(t *testing.T) {
	ev := &evaluator{env: &Env{}}
	for _, c := range []struct{ pattern, text string }{
		{strings.Repeat("*a?", 84) + "b", strings.Repeat("ab", 10000)},
		{"*" + strings.Repeat("a?", 126) + "b*", strings.Repeat("a", 20000)},
		{"*" + strings.Repeat("a", 252) + "b*", strings.Repeat("a", 20000)},
		{strings.Repeat("a", 5000) + "b", strings.Repeat("a", 20000)},
	} {
		match := func() {
			w, _ := ev.wildcardOf(c.pattern)
			w.match(ev, c.text)
			w.search(ev, c.text, 3)
		}
		match()
		if n := testing.AllocsPerRun(10, match); n != 0 {
			t.Errorf("matching the pattern %.12q allocates %v times", c.pattern, n)
		}
	}
}

// The table an Env keeps is used again for the pattern it was made for,
// as for the cells of one range, and never for another pattern.
func TestPieceTableIsKeptForItsPattern(t *testing.T) {
	env := &Env{}
	ev := &evaluator{env: env}
	run := strings.Repeat("a", 500)
	one := mustWildcard(t, ev, "*"+strings.Repeat("a", 20)+"b*")
	other := mustWildcard(t, ev, "*"+strings.Repeat("a", 19)+"ba*")
	for i, c := range []struct {
		w     *wildcard
		text  string
		match bool
	}{
		{&one, run + "b" + run, true},
		{&one, run + "b", true},
		{&other, run + "b", false},
		{&other, run + "b" + run, true},
		{&one, run + "b", true},
		{&one, run, false},
		{&other, run + "ba", true},
	} {
		if got := c.w.match(ev, c.text); got != c.match {
			t.Errorf("match %d: the pattern %.8q...%q matches: %v, want %v", i, c.w.pattern, c.w.pattern[len(c.w.pattern)-4:], got, c.match)
		}
		if want := c.w.pattern[1:]; env.memory().search.pattern != want {
			t.Errorf("match %d: the Env keeps the table of %.8q, want the one of %.8q", i, env.memory().search.pattern, want)
		}
	}
}

// A pattern that holds a wildcard has at most 255 code points, in every
// function that matches one. Text without a wildcard is no pattern and may
// be as long as a text may be.
func TestPatternLengthLimit(t *testing.T) {
	x255, k255 := strings.Repeat("x", 255), strings.Repeat("가", 255)
	sheet := &testSheet{tabs: []*testTab{newTab(t, "Sheet1", map[string]*testCell{
		"A1": textCell(x255), "A2": textCell(k255), "A3": textCell("x"), "A4": textCell(strings.Repeat("x", 300)),
		"B1": numberCell(1), "B2": numberCell(2), "B3": numberCell(4), "B4": numberCell(8),
		"C1": textCell(strings.Repeat("?", 255)), "C2": textCell(strings.Repeat("?", 256)),
		"C3": textCell(strings.Repeat("가", 254) + "*"), "C4": textCell(strings.Repeat("가", 255) + "*"),
		"C5": textCell(strings.Repeat("x", 300)), "C6": textCell(strings.Repeat("~?", 300)), "C7": textCell(strings.Repeat("~~", 200) + "~*"),
		"C8": textCell(strings.Repeat("x", 255) + "~"), "C9": textCell(strings.Repeat("x", 254) + "~*?"),
	})}}
	val, na := ErrorValue(ErrValue), ErrorValue(ErrNA)
	for formula, want := range map[string]Value{
		"=COUNTIF(A1:A4,C1)":                   NumberValue(2),
		"=COUNTIF(A1:A4,C2)":                   val,
		"=COUNTIF(A1:A4,C3)":                   NumberValue(1),
		"=COUNTIF(A1:A4,C4)":                   val,
		"=COUNTIF(A1:A4,C5)":                   NumberValue(1),
		"=COUNTIF(A1:A4,C6)":                   NumberValue(0),
		"=COUNTIF(A1:A4,C7)":                   NumberValue(0),
		"=COUNTIF(A1:A4,C8)":                   NumberValue(0),
		"=COUNTIF(A1:A4,C9)":                   val,
		`=COUNTIF(A1:A4,"="&C1)`:               NumberValue(2),
		`=COUNTIF(A1:A4,"="&C2)`:               val,
		`=COUNTIF(A1:A4,"<>"&C1)`:              NumberValue(2),
		`=COUNTIF(A1:A4,"<>"&C2)`:              val,
		`=COUNTIF(A1:A4,">"&C2)`:               NumberValue(4),
		`=COUNTIF(A1:A4,"<="&C2)`:              NumberValue(0),
		"=COUNTIFS(A1:A4,C1,B1:B4,1)":          NumberValue(1),
		"=COUNTIFS(B1:B4,1,A1:A4,C2)":          val,
		"=COUNTIFS(A1:A4,C2,Nope!A1,1)":        val,
		"=COUNTIFS(Nope!A1,1,A1:A4,C2)":        ErrorValue(ErrRef),
		"=COUNTIFS(A1:A4,C2,B1:B4,NA())":       val,
		"=COUNTIFS(A1:A4,NA(),B1:B4,C2)":       na,
		"=COUNTIFS(A1:A4,C2,B1:B3,1)":          val,
		"=SUMIF(A1:A4,C1,B1:B4)":               NumberValue(3),
		"=SUMIF(A1:A4,C2,B1:B4)":               val,
		"=SUMIF(A1:A4,C2,Nope!A1)":             val,
		"=SUMIFS(B1:B4,A1:A4,C3)":              NumberValue(2),
		"=SUMIFS(B1:B4,A1:A4,C4)":              val,
		"=AVERAGEIF(A1:A4,C1,B1:B4)":           NumberValue(1.5),
		"=AVERAGEIF(A1:A4,C2,B1:B4)":           val,
		"=AVERAGEIFS(B1:B4,A1:A4,C2)":          val,
		"=MAXIFS(B1:B4,A1:A4,C1)":              NumberValue(2),
		"=MAXIFS(B1:B4,A1:A4,C2)":              val,
		"=MINIFS(B1:B4,A1:A4,C2)":              val,
		"=MATCH(C1,A1:A4,0)":                   NumberValue(1),
		"=MATCH(C2,A1:A4,0)":                   val,
		"=MATCH(C5,A1:A4,0)":                   NumberValue(4),
		"=MATCH(C2,A1:A4,1)":                   na, // a sorted match has no wildcards
		"=MATCH(NA(),A1:A4,0)":                 na,
		"=VLOOKUP(C3,A1:B4,2,FALSE)":           NumberValue(2),
		"=VLOOKUP(C4,A1:B4,2,FALSE)":           val,
		"=VLOOKUP(C4,A1:B4,3,FALSE)":           ErrorValue(ErrRef),
		"=HLOOKUP(C2,A1:B4,2,FALSE)":           val,
		"=HLOOKUP(C1,A1:B4,2,FALSE)":           TextValue(k255),
		`=IFERROR(XLOOKUP(C2,A1:A4,B1:B4),-1)`: NumberValue(-1),
		"=SEARCH(C1,A4)":                       NumberValue(1),
		"=SEARCH(C1,A4,46)":                    NumberValue(46),
		"=SEARCH(C2,A4)":                       val,
		"=SEARCH(C5,A4)":                       NumberValue(1),
		`=IFERROR(SEARCH(C6,A4),-1)`:           NumberValue(-1),
		"=FIND(C2,C2)":                         NumberValue(1),
	} {
		if got := evalOn(t, sheet, formula); got != want {
			t.Errorf("%s = %v, want %v", formula, got, want)
		}
	}
}

// A search with a table counts every character of the text it goes
// through, the one that completes the piece included, and the characters
// it steps back over to the start of the piece. The steps are worked out
// here by hand, from the pattern and the text alone.
//
// A match of "*piece*" costs 1 step for the empty piece before the first
// "*", 1 for the empty piece after the last, 1 for each "*" that is gone
// through, and the search for the piece. A search for a pattern that
// starts with its piece costs the search for the piece and 1 for the "*".
// With a failure table a character of the text is 1 step; with bits it is
// 4, one for each word. A piece that is found costs its length once more.
func TestTableSearchesCountEveryCharacter(t *testing.T) {
	run := strings.Repeat("a", 400) // every place almost matches, so the first match makes the table
	for _, c := range []struct {
		pattern, text string
		search        bool // search from the start instead of matching the whole text
		found         bool
		steps         int
	}{
		// A failure table for the piece "ab", 2 characters.
		{pattern: "*ab*", text: "ab", found: true, steps: 3 + 2*1 + 2 + 1},
		{pattern: "*ab*", text: "xab", found: true, steps: 3 + 3*1 + 2 + 1},
		{pattern: "*ab*", text: "xxxxab!", found: true, steps: 3 + 6*1 + 2 + 1},
		{pattern: "*ab*", text: "가나ab", found: true, steps: 3 + 4*1 + 2 + 1},
		{pattern: "*ab*", text: "xxxx", found: false, steps: 3 + 4*1},
		{pattern: "ab*", text: "xxab", search: true, found: true, steps: 4*1 + 2 + 1},
		{pattern: "ab*", text: "xxxa", search: true, found: false, steps: 4 * 1},
		// Bits for the piece "?b", 2 characters.
		{pattern: "*?b*", text: "ab", found: true, steps: 3 + 2*4 + 2 + 1},
		{pattern: "*?b*", text: "xxab", found: true, steps: 3 + 4*4 + 2 + 1},
		{pattern: "*?b*", text: "가나b", found: true, steps: 3 + 3*4 + 2 + 1},
		{pattern: "*?b*", text: "aaaa", found: false, steps: 3 + 4*4},
		{pattern: "?b*", text: "aab", search: true, found: true, steps: 3*4 + 2 + 1},
		{pattern: "?b*", text: "aaa", search: true, found: false, steps: 3 * 4},
	} {
		env := &Env{}
		ev := &evaluator{env: env}
		w := mustWildcard(t, ev, c.pattern)
		piece := strings.TrimPrefix(c.pattern, "*")
		// The table is made by the kind of call that uses it afterwards.
		call := func(text string) bool {
			if c.search {
				return w.search(ev, text, 0) >= 0
			}
			return w.match(ev, text)
		}
		call(run)
		kept := env.memory()
		if strings.Contains(piece, "?") {
			if kept.masks.pattern != piece {
				t.Fatalf("the pattern %q: the Env keeps bits for %q, want %q", c.pattern, kept.masks.pattern, piece)
			}
		} else if kept.search.pattern != piece {
			t.Fatalf("the pattern %q: the Env keeps the failure table of %q, want %q", c.pattern, kept.search.pattern, piece)
		}
		before := ev.spent
		if got := call(c.text); got != c.found {
			t.Errorf("the pattern %q in %q: found %v, want %v", c.pattern, c.text, got, c.found)
		}
		if steps := ev.spent - before; steps != c.steps {
			t.Errorf("the pattern %q in %q counts %d steps, want %d", c.pattern, c.text, steps, c.steps)
		}
	}
}

// The piece after the last "*" is looked for at the end of the text. The
// steps back to its start are counted also when the text turns out to
// have too few characters for it.
func TestStepsBackToTheLastPieceAreCounted(t *testing.T) {
	for _, c := range []struct {
		pattern, text string
		steps         int
	}{
		// 1 for the empty piece before the "*", 3 for the characters of "aaa".
		{"*aaa", "가", 1 + 3},  // 3 bytes, 1 character, so the text ends first
		{"*aaa", "가나", 1 + 3}, // 6 bytes, 2 characters
		// Then the piece is tried at the end: 1, and 1 for each of the two
		// characters that match before the one that does not.
		{"*aaa", "xaab", 1 + 3 + 1 + 2},
	} {
		ev := &evaluator{env: &Env{}}
		w := mustWildcard(t, ev, c.pattern)
		before := ev.spent
		if w.match(ev, c.text) {
			t.Errorf("the pattern %q matches %q", c.pattern, c.text)
		}
		if steps := ev.spent - before; steps != c.steps {
			t.Errorf("the pattern %q against %q counts %d steps, want %d", c.pattern, c.text, steps, c.steps)
		}
	}
}
