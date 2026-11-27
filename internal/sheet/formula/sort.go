package formula

// sortRank orders the kinds for a sort: numbers, text, booleans, errors,
// and blank last.
func sortRank(k ValueKind) int {
	switch k {
	case KindNumber:
		return 0
	case KindText:
		return 1
	case KindBool:
		return 2
	case KindError:
		return 3
	}
	return 4
}

// CompareSort orders two values for an ascending sort (spec rule 22):
// numbers from the smallest, then text in the order of CompareText, then
// FALSE, then TRUE, then errors, then blank. It returns -1, 0 or 1. Every
// error ties with every other, and text that differs only in capitals
// ties.
//
// Blank cells come last in both directions: for a descending sort, reverse
// the order of the values that are not blank only.
func CompareSort(a, b Value) int {
	if ra, rb := sortRank(a.kind), sortRank(b.kind); ra != rb {
		if ra < rb {
			return -1
		}
		return 1
	}
	switch a.kind {
	case KindNumber, KindBool:
		switch {
		case a.num < b.num:
			return -1
		case a.num > b.num:
			return 1
		}
	case KindText:
		return CompareText(a.str, b.str)
	}
	return 0
}

// CompareSortCounted is CompareSort for a caller that counts its work. It
// also returns the bytes of text it went through before it could tell, as
// compareTextCounted counts them, and nothing for values of any other kind.
func CompareSortCounted(a, b Value) (order, walked int) {
	if a.kind == KindText && b.kind == KindText {
		return compareTextCounted(a.str, b.str)
	}
	return CompareSort(a, b), 0
}
