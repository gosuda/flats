package formula

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

// textOrderPairs are the pairs of O-01, each with a smaller first text, and
// a few more: hyphens and apostrophes, Latin and Hangul, and a text that
// ends first.
var textOrderPairs = [][2]string{
	{"coop", "co-op"},
	{"ab", "a-c"},
	{"ab", "a'b"},
	{"ab", "a-b"},
	{"a'b", "a-b"},
	{"-ab", "a-b"},
	{"a", "a-"},
	{"co-op", "cop"},
	{"zebra", "가방"},
	{"가", "나"},
	{"Zoo", "가"},
	{"ab", "abc"},
	{"a", "A1"},
	{"a b", "ab"},
	{"a1", "aa"},
	{"a~", "a+"},
	{"a>", "aa"},
	{"STRASSE", "Straße"},
	{"z", "\t"},
	{"\t", "é"},
	{"é", "가"},
	{"가", "😀"},
}

// O-01: the text order of rule 22. Digits, then the space and the other
// ASCII punctuation in Excel's order, then letters with capitals ignored,
// then every other code point by its value. Apostrophes and hyphens count
// only when the rest ties: fewer come first, then the code points decide.
func TestCompareText(t *testing.T) {
	var ascending []string
	for c := '0'; c <= '9'; c++ {
		ascending = append(ascending, string(c))
	}
	for _, c := range " !\"#$%&()*,./:;?@[\\]^_`{|}~+<=>" {
		ascending = append(ascending, string(c))
	}
	for c := 'a'; c <= 'z'; c++ {
		letter := c
		if c%2 == 0 {
			letter -= 'a' - 'A' // capitals sort with their small letters
		}
		ascending = append(ascending, string(letter))
	}
	ascending = append(ascending, "\x01", "\t", "\x7f", "ß", "é", "Ω", "가", "나", "힣", "😀")
	for i, a := range ascending {
		for j, b := range ascending {
			if got, want := CompareText(a, b), cmpInt(i, j); got != want {
				t.Errorf("CompareText(%q, %q) = %d, want %d", a, b, got, want)
			}
		}
	}
	for _, p := range textOrderPairs {
		if got := CompareText(p[0], p[1]); got != -1 {
			t.Errorf("CompareText(%q, %q) = %d, want -1", p[0], p[1], got)
		}
		if got := CompareText(p[1], p[0]); got != 1 {
			t.Errorf("CompareText(%q, %q) = %d, want 1", p[1], p[0], got)
		}
		if got := CompareSort(TextValue(p[0]), TextValue(p[1])); got != -1 {
			t.Errorf("CompareSort(%q, %q) = %d, want -1", p[0], p[1], got)
		}
		if got, _ := CompareSortCounted(TextValue(p[1]), TextValue(p[0])); got != 1 {
			t.Errorf("CompareSortCounted(%q, %q) = %d, want 1", p[1], p[0], got)
		}
	}
	for _, tie := range [][2]string{{"a", "A"}, {"apple", "APPLE"}, {"co-op", "CO-OP"}, {"straße", "STRAẞE"}, {"k", "K"}, {"", ""}} {
		if CompareText(tie[0], tie[1]) != 0 || CompareText(tie[1], tie[0]) != 0 {
			t.Errorf("%q and %q do not tie", tie[0], tie[1])
		}
	}
}

func cmpInt(i, j int) int {
	switch {
	case i < j:
		return -1
	case i > j:
		return 1
	}
	return 0
}

// The order is total: two texts tie exactly when they are equal after case
// folding, the order of two texts is the reverse of the order the other
// way round, and a sort by it puts every pair in order.
func TestCompareTextIsATotalOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(20261119))
	pieces := []string{"a", "A", "b", "-", "'", " ", "1", "ß", "ẞ", "가", "~", "+", "k", "K"}
	texts := make([]string, 3000)
	for i := range texts {
		var b strings.Builder
		for k := rng.Intn(6); k > 0; k-- {
			b.WriteString(pieces[rng.Intn(len(pieces))])
		}
		texts[i] = b.String()
	}
	for i := 0; i+1 < len(texts); i++ {
		a, b := texts[i], texts[i+1]
		order := CompareText(a, b)
		if order != -CompareText(b, a) {
			t.Fatalf("CompareText(%q, %q) = %d both ways", a, b, order)
		}
		if (order == 0) != (FoldName(a) == FoldName(b)) {
			t.Fatalf("CompareText(%q, %q) = %d", a, b, order)
		}
	}
	slices.SortStableFunc(texts, CompareText)
	for i := range texts {
		for j := i + 1; j < len(texts) && j < i+50; j++ {
			if CompareText(texts[i], texts[j]) > 0 {
				t.Fatalf("after the sort %q comes before %q", texts[i], texts[j])
			}
		}
	}
}

// The counted form goes through the texts in place, takes no memory and
// counts what it went through: a pair that ties after the first pass costs
// the length of both.
func TestCompareTextCountsItsWork(t *testing.T) {
	long := strings.Repeat("ab-c'", 6000)
	upper := strings.ToUpper(long)
	if order, walked := compareTextCounted(long, upper); order != 0 || walked < len(long)+len(upper) {
		t.Errorf("a tie of %d bytes each = %d after %d bytes", len(long), order, walked)
	}
	fewer := strings.ReplaceAll(long, "-", "")
	if order, walked := compareTextCounted(fewer, long); order != -1 || walked < len(long)+len(fewer) {
		t.Errorf("a text with fewer hyphens = %d after %d bytes", order, walked)
	}
	if order, walked := compareTextCounted("b"+long, "a"+long); order != 1 || walked > 4 {
		t.Errorf("texts that differ at once = %d after %d bytes", order, walked)
	}
	if n := testing.AllocsPerRun(10, func() { compareTextCounted(long, upper) }); n != 0 {
		t.Errorf("comparing two texts allocates %v times", n)
	}
}

// O-02 in the engine: <, criteria and the sorted approximate match agree
// with the sort, and = holds only for texts equal apart from capitals.
func TestTextOrderInFormulas(t *testing.T) {
	var keys []string
	for _, p := range textOrderPairs {
		keys = append(keys, p[0], p[1])
	}
	keys = append(keys, "b", "B", "a-b", "A'B")
	slices.SortStableFunc(keys, func(a, b string) int { return CompareSort(TextValue(a), TextValue(b)) })
	cells := map[string]*testCell{}
	for i, k := range keys {
		cells[fmt.Sprintf("A%d", i+1)] = textCell(k)
	}
	sheet := &testSheet{tabs: []*testTab{newTab(t, "Sheet1", cells)}}
	column := fmt.Sprintf("A1:A%d", len(keys))
	quote := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	for _, p := range textOrderPairs {
		a, b := quote(p[0]), quote(p[1])
		for formula, want := range map[string]Value{
			"=" + a + "<" + b:  BoolValue(true),
			"=" + b + "<" + a:  BoolValue(false),
			"=" + a + "<=" + b: BoolValue(true),
			"=" + b + ">" + a:  BoolValue(true),
			"=" + a + "=" + b:  BoolValue(false),
			"=" + a + "<>" + b: BoolValue(true),
		} {
			if got := evalOn(t, sheet, formula); got != want {
				t.Errorf("%s = %v, want %v", formula, got, want)
			}
		}
	}
	for formula, want := range map[string]Value{
		`="ab"<"a-b"`:      BoolValue(true),
		`="ab"<"a'b"`:      BoolValue(true),
		`="ab"="a-b"`:      BoolValue(false),
		`="a"="A"`:         BoolValue(true),
		`="co-op"="CO-OP"`: BoolValue(true),
		`="a"<"A"`:         BoolValue(false),
		`="Zoo"<"가"`:       BoolValue(true),
	} {
		if got := evalOn(t, sheet, formula); got != want {
			t.Errorf("%s = %v, want %v", formula, got, want)
		}
	}
	// Text criteria with <, <=, > and >= count what the sort puts on each
	// side.
	for _, sign := range []string{"<", "<=", ">", ">="} {
		for _, key := range []string{"b", "a-b", "co-op", "가", "a"} {
			want := 0
			for _, k := range keys {
				c := CompareText(k, key)
				if sign == "<" && c < 0 || sign == "<=" && c <= 0 || sign == ">" && c > 0 || sign == ">=" && c >= 0 {
					want++
				}
			}
			formula := "=COUNTIF(" + column + "," + quote(sign+key) + ")"
			if got := evalOn(t, sheet, formula); got != NumberValue(float64(want)) {
				t.Errorf("%s = %v, want %d", formula, got, want)
			}
		}
	}
	// MATCH with type 1 on the sorted column finds each key: the last cell
	// that is not after it.
	for _, key := range keys {
		want := 0
		for i, k := range keys {
			if CompareText(k, key) <= 0 {
				want = i + 1
			}
		}
		formula := "=MATCH(" + quote(key) + "," + column + ",1)"
		if got := evalOn(t, sheet, formula); got != NumberValue(float64(want)) {
			t.Errorf("%s = %v, want %d", formula, got, want)
		}
		if got := evalOn(t, sheet, "=INDEX("+column+","+fmt.Sprint(want)+")="+quote(key)); got != BoolValue(true) {
			t.Errorf("MATCH of %q finds a cell that is not the key", key)
		}
	}
}
