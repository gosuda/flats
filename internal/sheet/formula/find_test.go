package formula

import (
	"math/rand"
	"strings"
	"testing"
)

// indexText finds what the standard library finds, for texts on both
// sides of the length from which it uses its own table.
func TestIndexTextAgreesWithTheStandardLibrary(t *testing.T) {
	rng := rand.New(rand.NewSource(20261114))
	ev := &evaluator{env: &Env{}}
	pick := func(alphabet string, n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[rng.Intn(len(alphabet))]
		}
		return string(b)
	}
	check := func(s, sub string) {
		t.Helper()
		for _, from := range []int{0, 1, len(s) / 3, len(s) - len(sub), len(s)} {
			if from < 0 || from > len(s) {
				continue
			}
			want := strings.Index(s[from:], sub)
			if want >= 0 {
				want += from
			}
			if got := ev.indexText(s, sub, from); got != want {
				t.Fatalf("indexText(%.20q of %d bytes, %.20q of %d bytes, %d) = %d, want %d", s, len(s), sub, len(sub), from, got, want)
			}
		}
	}
	for i := 0; i < 20000; i++ {
		alphabet := []string{"a", "ab", "abc", "ab가"}[rng.Intn(4)]
		s := pick(alphabet, rng.Intn(600))
		var sub string
		switch n := []int{0, 1, 2, 63, 64, 65, 66, 100, 300}[rng.Intn(9)]; {
		case rng.Intn(3) == 0 || len(s) < n:
			sub = pick(alphabet, n)
		default:
			at := rng.Intn(len(s) - n + 1)
			sub = s[at : at+n] // a text that is there
		}
		check(s, sub)
	}
	// Texts that repeat, where a long text almost matches at many places.
	for _, period := range []string{"a", "ab", "ab" + strings.Repeat("z", 14), "가나다"} {
		for _, n := range []int{5, 20, 200} {
			sub := strings.Repeat(period, n)
			s := strings.Repeat(period, 3*n)
			check(s, sub)
			check(s, sub+"!")
			check(s, "!"+sub)
			check(s+"!"+s, sub+"!"+period)
			check(s[:len(s)/2]+sub+"!"+s, sub+"!")
		}
	}
}

// FIND and SUBSTITUTE go through a text once for a long text to find, also
// one that almost matches at every sixteenth byte, which costs the
// standard library a comparison of its whole length at each such place.
func TestLongTextsAreFoundInOnePass(t *testing.T) {
	period := "ab" + strings.Repeat("z", 14)
	find := strings.Repeat(period, 1023) + "!" // 16,369 bytes
	absent := strings.Repeat(period, 2047)     // 32,752 bytes, without a "!"
	present := strings.Repeat(period, 700) + find + "end"
	twice := strings.Repeat(strings.Repeat(period, 8)+"!", 200)

	ev := &evaluator{env: &Env{}}
	for _, c := range []struct {
		s, sub string
		want   int
	}{
		{absent, find, -1},
		{present, find, 700 * len(period)},
		{twice, strings.Repeat(period, 8) + "!" + period, 0},
		{twice, strings.Repeat(period, 9), -1},
	} {
		before := ev.spent
		if got := ev.indexText(c.s, c.sub, 0); got != c.want {
			t.Errorf("indexText finds a text of %d bytes in one of %d at %d, want %d", len(c.sub), len(c.s), got, c.want)
		}
		if steps, most := ev.spent-before, len(c.s)+2*len(c.sub); steps > most {
			t.Errorf("finding a text of %d bytes in one of %d took %d steps, more than %d", len(c.sub), len(c.s), steps, most)
		}
	}

	sheet := &testSheet{tabs: []*testTab{newTab(t, "Sheet1", map[string]*testCell{
		"A1": textCell(absent), "A2": textCell(present), "A3": textCell(twice),
		"B1": textCell(find), "B2": textCell(strings.Repeat(period, 8) + "!"),
	})}}
	for formula, want := range map[string]Value{
		"=IFERROR(FIND(B1,A1),-1)":        NumberValue(-1),
		"=FIND(B1,A2)":                    NumberValue(float64(700*len(period) + 1)),
		"=IFERROR(FIND(B1,A2,11202),-1)":  NumberValue(-1),
		"=FIND(B2,A3,2)":                  NumberValue(float64(len(period)*8 + 2)),
		`=SUBSTITUTE(A1,B1,"x")`:          TextValue(absent),
		`=SUBSTITUTE(A2,B1,"x")`:          TextValue(strings.Repeat(period, 700) + "xend"),
		`=SUBSTITUTE(A2,B1,"x",1)`:        TextValue(strings.Repeat(period, 700) + "xend"),
		`=SUBSTITUTE(A2,B1,"x",2)`:        TextValue(present),
		`=SUBSTITUTE(A3,B2,"")`:           TextValue(""),
		`=SUBSTITUTE(A3,B2,"가")`:          TextValue(strings.Repeat("가", 200)),
		`=LEN(SUBSTITUTE(A3,B2,"x",200))`: NumberValue(float64(len(twice) - len(period)*8)),
		`=SUBSTITUTE(A3,B2,"x",201)`:      TextValue(twice),
		`=LEN(SUBSTITUTE(A3,"!",B2))`:     ErrorValue(ErrValue), // too long a result
		`=SUBSTITUTE("banana","an","AN")`: TextValue("bANANa"),
		`=SUBSTITUTE("aaaa","aa","b")`:    TextValue("bb"),
	} {
		b := NewBudget(nil)
		if got := mustParse(t, formula, "Z9").Eval(&Env{Grid: sheet, At: pos(t, "Z9"), Budget: b}); got != want {
			t.Errorf("%s = %.40v, want %.40v", formula, got, want)
		}
		// A few passes over the longest of the texts, 64 bytes to a unit.
		if b.Used() > 8*int64(MaxTextLen)/64 {
			t.Errorf("%s counts %d units of work", formula, b.Used())
		}
	}
}
