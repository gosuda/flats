package formula

import "unicode/utf8"

// The ranks of the first pass of CompareText (technical design, section
// 9.3): the digits, then the space and the other ASCII punctuation in the
// order Microsoft documents for the sort of Excel, then the letters, then
// every other code point by its value. Apostrophes and hyphens have no rank:
// the first pass skips them.
const (
	textPunctuation = " !\"#$%&()*,./:;?@[\\]^_`{|}~+<=>"
	letterRank      = 10 + len(textPunctuation)
	otherRank       = letterRank + 26
	skippedRank     = -1
)

// asciiRank is the rank of each ASCII code point after case folding, so a
// capital has the rank of its small letter.
var asciiRank = func() (rank [utf8.RuneSelf]int32) {
	for c := range rank {
		rank[c] = int32(otherRank + c)
	}
	for c := '0'; c <= '9'; c++ {
		rank[c] = c - '0'
	}
	for i := 0; i < len(textPunctuation); i++ {
		rank[textPunctuation[i]] = int32(10 + i)
	}
	for c := 'a'; c <= 'z'; c++ {
		rank[c] = int32(letterRank) + c - 'a'
		rank[c-('a'-'A')] = rank[c]
	}
	rank['\''], rank['-'] = skippedRank, skippedRank
	return rank
}()

// textRank is the rank of a code point in the first pass. A code point that
// folds to ASCII, such as the Kelvin sign, has the rank of its fold.
func textRank(r rune) int32 {
	if r = foldRune(r); r < utf8.RuneSelf {
		return asciiRank[r]
	}
	return int32(otherRank) + r
}

// CompareText orders two texts as rule 22 sorts them, and returns -1, 0 or
// 1. It compares in three passes over the case-folded code points and
// stops at the first that tells the texts apart: by the rank of each code
// point, apostrophes and hyphens skipped, a text that ends first being the
// smaller; then by the number of apostrophes and hyphens, fewer first; then
// by the folded code points. Two texts tie exactly when they are equal
// after case folding, so the order is total and = agrees with it.
func CompareText(a, b string) int {
	order, _ := compareTextCounted(a, b)
	return order
}

// compareTextCounted is CompareText for a caller that counts its work. It
// also returns the bytes it went through: those of both texts that the
// first pass read, which for a pair that ties there is the length of both,
// and those of a that the last pass read. It reads the texts in place and
// allocates nothing.
func compareTextCounted(a, b string) (order, walked int) {
	i, j := 0, 0 // the bytes of a and b the first pass has read
	skippedA, skippedB := 0, 0
	for {
		ra, rb := int32(skippedRank), int32(skippedRank)
		for ra == skippedRank && i < len(a) {
			if c := a[i]; c < utf8.RuneSelf {
				ra, i = asciiRank[c], i+1
			} else {
				r, n := utf8.DecodeRuneInString(a[i:])
				ra, i = textRank(r), i+n
			}
			if ra == skippedRank {
				skippedA++
			}
		}
		for rb == skippedRank && j < len(b) {
			if c := b[j]; c < utf8.RuneSelf {
				rb, j = asciiRank[c], j+1
			} else {
				r, n := utf8.DecodeRuneInString(b[j:])
				rb, j = textRank(r), j+n
			}
			if rb == skippedRank {
				skippedB++
			}
		}
		if ra != rb {
			if ra < rb {
				return -1, i + j
			}
			return 1, i + j
		}
		if ra == skippedRank {
			break // both have ended, so the skipped code points are counted
		}
	}
	switch {
	case skippedA < skippedB:
		return -1, i + j
	case skippedA > skippedB:
		return 1, i + j
	}
	order, last := foldOrder(a, b)
	return order, i + j + last
}
