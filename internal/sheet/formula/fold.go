package formula

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// foldRune returns the simple case fold of r: the one member that stands
// for every code point of r's unicode.SimpleFold orbit (foldReference). It
// costs a table lookup, and the same for a letter with capitals as for one
// without.
func foldRune(r rune) rune {
	if r < utf8.RuneSelf {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}
	if b := uint32(r) >> 8; b < uint32(len(folds.block)) {
		if p := folds.block[b]; p != 0 {
			return folds.pages[p-1][uint8(r)]
		}
	}
	return r
}

// foldTable holds the fold of every code point that does not fold to
// itself, a block of 256 code points to a page.
type foldTable struct {
	block []uint16    // for each block, the place of its page in pages and one more, or 0 for a block that needs none
	pages [][256]rune // for each code point of a block, its fold
}

// folds is built once, when the package is loaded, from the pairs of
// fold_table.go, and is never written afterwards. Evaluations on many
// goroutines read it.
var folds = newFoldTable(foldPairs[:])

// newFoldTable lays out pairs of a code point and its fold, in the order
// of the code points, as pages.
func newFoldTable(pairs []rune) foldTable {
	var t foldTable
	blocks, last := 0, rune(-1)
	for i := 0; i < len(pairs); i += 2 {
		if b := pairs[i] >> 8; b != last {
			blocks, last = blocks+1, b
		}
	}
	t.block = make([]uint16, last+1)
	t.pages = make([][256]rune, 0, blocks)
	for i := 0; i < len(pairs); i += 2 {
		r := pairs[i]
		b := r >> 8
		if t.block[b] == 0 {
			t.pages = append(t.pages, [256]rune{})
			t.block[b] = uint16(len(t.pages))
			page := &t.pages[len(t.pages)-1]
			for low := range page {
				page[low] = b<<8 | rune(low)
			}
		}
		t.pages[t.block[b]-1][uint8(r)] = pairs[i+1]
	}
	return t
}

// foldReference works the simple case fold of r out from the tables of
// the unicode package: the one member that stands for every code point of
// r's unicode.SimpleFold orbit. It follows CaseFolding.txt, statuses C and
// S, so the member is the small letter that is the lower case of its own
// upper case. Capital I with dot above and the dotless i are alone in
// their orbits and fold to themselves. foldRune gives the same from a
// table. The pairs of fold_table.go are generated from this function, and
// TestFoldTable checks them against it for every code point.
func foldReference(r rune) rune {
	if r < utf8.RuneSelf {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}
	if unicode.SimpleFold(r) == r {
		return r
	}
	// Cherokee folds to its capital letters, which are the older ones.
	switch {
	case r >= 0x13A0 && r <= 0x13F5:
		return r
	case r >= 0xAB70 && r <= 0xABBF:
		return r - 0xAB70 + 0x13A0
	case r >= 0x13F8 && r <= 0x13FD:
		return r - 8
	}
	best, bestRank := r, 3
	for m := r; ; {
		rank := 2
		switch {
		case unicode.ToLower(unicode.ToUpper(m)) == m:
			rank = 0
		case unicode.ToLower(m) == m:
			rank = 1
		}
		if rank < bestRank || rank == bestRank && m < best {
			best, bestRank = m, rank
		}
		if m = unicode.SimpleFold(m); m == r {
			return best
		}
	}
}

// FoldName returns s after Unicode simple case folding, one code point at a
// time. Two tab names are the same when their folded forms are equal.
func FoldName(s string) string {
	return strings.Map(foldRune, s)
}

// compareFold orders two texts by their folded code points. Tab names are
// matched with it (equalFold); the text of cells is ordered by CompareText.
func compareFold(a, b string) int {
	order, _ := foldOrder(a, b)
	return order
}

// foldOrder orders two texts by their folded code points. It also returns
// the bytes of a that it went through before the code point that decided.
func foldOrder(a, b string) (order, walked int) {
	whole := len(a)
	for a != "" && b != "" {
		if a[0] < utf8.RuneSelf && b[0] < utf8.RuneSelf {
			x, y := a[0], b[0]
			if x >= 'A' && x <= 'Z' {
				x += 'a' - 'A'
			}
			if y >= 'A' && y <= 'Z' {
				y += 'a' - 'A'
			}
			if x != y {
				if x < y {
					return -1, whole - len(a)
				}
				return 1, whole - len(a)
			}
			a, b = a[1:], b[1:]
			continue
		}
		x, n := utf8.DecodeRuneInString(a)
		y, m := utf8.DecodeRuneInString(b)
		if x, y = foldRune(x), foldRune(y); x != y {
			if x < y {
				return -1, whole - len(a)
			}
			return 1, whole - len(a)
		}
		a, b = a[n:], b[m:]
	}
	switch {
	case a != "":
		return 1, whole - len(a)
	case b != "":
		return -1, whole
	}
	return 0, whole
}

func equalFold(a, b string) bool { return compareFold(a, b) == 0 }
