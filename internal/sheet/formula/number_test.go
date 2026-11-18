package formula

import (
	"math"
	"math/rand"
	"strconv"
	"testing"
)

// decimalText writes a decimal the way digits15 writes its result.
func decimalText(d decimal) string {
	if d.zero {
		return "0"
	}
	s := ""
	if d.neg {
		s = "-"
	}
	for _, c := range d.dig {
		s += string('0' + c)
	}
	return s + "e" + strconv.Itoa(d.exp)
}

// sampleNumbers returns numbers of every size, with plain decimals, whole
// numbers and exact ties among them.
func sampleNumbers(rng *rand.Rand, n int) []float64 {
	tenth := 0.1 // a variable, so that sums are rounded as doubles
	out := []float64{
		0, 1, -1, 0.1, 0.3, tenth + 0.2, tenth * 3, 1.0 / 3, 2.0 / 3, 1e15, 1e15 - 1, 999999999999999.9, 123456789012345678,
		math.Ldexp(1, -22), -math.Ldexp(1, -22), // 2.384185791015625e-07: a tie at the 16th digit
		math.Ldexp(1, -1074), math.MaxFloat64, 0.5, 2.5, 1e-10, 1e-11, 9.999995e14, 9.9999999999999995e14,
		1.0000000000000005, 0.99999999999999995, 4.35, 2.675, 1.005, 7.999999999999999, 32767, 86399.0 / 86400,
	}
	for len(out) < n {
		switch rng.Intn(4) {
		case 0:
			out = append(out, math.Float64frombits(rng.Uint64()))
		case 1:
			out = append(out, float64(rng.Int63n(1e15))/math.Pow(10, float64(rng.Intn(18))))
		case 2:
			out = append(out, math.Ldexp(float64(rng.Intn(1<<20)|1), rng.Intn(80)-60)) // short binary fractions tie often
		default:
			out = append(out, (rng.Float64()-0.5)*math.Pow(10, float64(rng.Intn(40)-20)))
		}
		if x := out[len(out)-1]; math.IsNaN(x) || math.IsInf(x, 0) {
			out = out[:len(out)-1]
		}
	}
	return out
}

func TestToDecimalRoundsHalfAwayFromZero(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, x := range sampleNumbers(rng, 200000) {
		if got, want := decimalText(toDecimal(x)), digits15(x); got != want {
			t.Fatalf("toDecimal(%v) = %s, want %s", x, got, want)
		}
		// The largest doubles round to 15 digits that no double holds.
		if r := round15(x); !math.IsInf(r, 0) && digits15(r) != digits15(x) {
			t.Fatalf("round15(%v) = %v, which rounds to %s, want %s", x, r, digits15(r), digits15(x))
		}
	}
	if got := decimalText(toDecimal(math.Ldexp(1, -22))); got != "238418579101563e-7" {
		t.Errorf("the tie 2^-22 rounds to %s, want it away from zero", got)
	}
}

func TestCompareNumbers(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	sign := func(a, b string, x, y float64) int {
		switch {
		case a == b:
			return 0
		case x < y:
			return -1
		}
		return 1
	}
	for _, x := range sampleNumbers(rng, 50000) {
		for _, y := range []float64{x, math.Nextafter(x, math.Inf(1)), math.Nextafter(x, math.Inf(-1)),
			x * (1 + 1e-15), x * (1 - 3e-15), x * (1 + 2e-14), x + 1, -x} {
			if math.IsInf(y, 0) {
				continue
			}
			if got, want := compareNumbers(x, y), sign(digits15(x), digits15(y), x, y); got != want {
				t.Fatalf("compareNumbers(%v, %v) = %d, want %d", x, y, got, want)
			}
		}
	}
	if tenth := 0.1; tenth+0.2 == 0.3 || compareNumbers(tenth+0.2, 0.3) != 0 {
		t.Error("0.1+0.2 and 0.3 differ")
	}
}

func TestGeneralText(t *testing.T) {
	tenth := 0.1
	for x, want := range map[float64]string{
		0: "0", 1234.5: "1234.5", -0.5: "-0.5", tenth + 0.2: "0.3", 1.0 / 3: "0.333333333333333",
		123456789012345: "123456789012345", 999999999999999.9: "1E+15", 1e15: "1E+15", 1234567890123456: "1.23457E+15",
		1e-10: "0.0000000001", 1.5e-10: "0.00000000015", 1e-11: "1E-11", 1.234567e-11: "1.23457E-11",
		-1.5e20: "-1.5E+20", 1e100: "1E+100", 9.999996e20: "1E+21", 1e-300: "1E-300", 0.000001: "0.000001",
		100: "100", 1e14: "100000000000000", 12345678901234.5: "12345678901234.5",
	} {
		if got := generalText(x); got != want {
			t.Errorf("generalText(%v) = %q, want %q", x, got, want)
		}
		if got := string(appendGeneral(nil, x)); got != want {
			t.Errorf("appendGeneral(%v) = %q, want %q", x, got, want)
		}
	}
}

func TestFixedText(t *testing.T) {
	cases := []struct {
		x         float64
		decimals  int
		thousands bool
		shift     int
		want      string
	}{
		{1234.5, 2, true, 0, "1,234.50"},
		{1234.5, 0, false, 0, "1235"},
		{-1234.5, 0, true, 0, "-1,235"},
		{0.5, 0, false, 0, "1"},
		{-0.4, 0, false, 0, "0"},
		{-0.004, 2, false, 0, "0.00"},
		{2.675, 2, false, 0, "2.68"},
		{1.005, 2, false, 0, "1.01"},
		{0.125, 2, false, 2, "12.50"},
		{0.999995, 2, false, 2, "100.00"},
		{999.9996, 3, true, 0, "1,000.000"},
		{1e21, 0, true, 0, "1,000,000,000,000,000,000,000"},
		{1234567.891, 10, true, 0, "1,234,567.8910000000"},
		{0, 2, true, 0, "0.00"},
		{1e-12, 10, false, 0, "0.0000000000"},
		{5e-11, 10, false, 0, "0.0000000001"},
	}
	for _, c := range cases {
		if got := string(appendFixed(nil, c.x, c.decimals, c.thousands, c.shift, "")); got != c.want {
			t.Errorf("appendFixed(%v, %d, %v, %d) = %q, want %q", c.x, c.decimals, c.thousands, c.shift, got, c.want)
		}
	}
}

func TestRoundNumber(t *testing.T) {
	cases := []struct {
		x      float64
		digits int
		mode   roundMode
		want   float64
	}{
		{2.5, 0, roundHalfAway, 3}, {-2.5, 0, roundHalfAway, -3}, {2.4, 0, roundAway, 3}, {-2.4, 0, roundAway, -3},
		{2.9, 0, roundToZero, 2}, {-2.9, 0, roundToZero, -2}, {1234.5678, -2, roundHalfAway, 1200},
		{1250, -2, roundHalfAway, 1300}, {49, -2, roundHalfAway, 0}, {50, -2, roundHalfAway, 100},
		{1, -2, roundAway, 100}, {0, -2, roundAway, 0}, {1, -400, roundAway, math.Inf(1)}, {1, -400, roundHalfAway, 0},
		{1.005, 2, roundHalfAway, 1.01}, {7.999999999999999, 0, roundToZero, 8}, {0.30000000000000004, 400, roundHalfAway, 0.3},
		{2.0000000000000004, 0, roundAway, 2},
	}
	for _, c := range cases {
		if got := roundNumber(c.x, c.digits, c.mode); got != c.want {
			t.Errorf("roundNumber(%v, %d, %d) = %v, want %v", c.x, c.digits, c.mode, got, c.want)
		}
	}
}

func TestSplitDayTime(t *testing.T) {
	cases := []struct {
		x    float64
		days int64
		secs int
	}{
		{0, 0, 0}, {0.5, 0, 43200}, {46000.75, 46000, 64800}, {86399.0 / 86400, 0, 86399},
		{0.999999, 1, 0}, {2.9999999, 3, 0}, {0.5 / 86400, 0, 1}, {0.49 / 86400, 0, 0}, {5e-21, 0, 0},
		{1e15, 1e15, 0}, {46000, 46000, 0}, {52205.0 / 86400, 0, 52205},
	}
	for _, c := range cases {
		if days, secs := splitDayTime(c.x); days != c.days || secs != c.secs {
			t.Errorf("splitDayTime(%v) = %d, %d, want %d, %d", c.x, days, secs, c.days, c.secs)
		}
	}
	// Every second of a day reads back as itself.
	for s := 0; s < daySeconds; s += 7 {
		if _, secs := splitDayTime(46000 + float64(s)/daySeconds); secs != s {
			t.Fatalf("second %d of a day reads as %d", s, secs)
		}
	}
}

func TestCalendar(t *testing.T) {
	for serial, want := range map[int64]string{0: "1899-12-30", 2: "1900-01-01", 61: "1900-03-01", 60: "1900-02-28", 46339: "2026-11-13", lastDateDay: "9999-12-31"} {
		if got := Date(serial).String(); got != want {
			t.Errorf("day %d is %s, want %s", serial, got, want)
		}
		if d, ok := ParseDate(want); want >= "1900" && (!ok || int64(d) != serial) {
			t.Errorf("ParseDate(%s) = %d, %v, want %d", want, d, ok, serial)
		}
	}
	for serial := int64(0); serial <= lastDateDay; serial += 13 {
		y, m, d := civilOf(serial)
		if serialOf(y, m, d) != serial || d < 1 || d > monthLen(y, m) {
			t.Fatalf("day %d is %d-%d-%d, which is day %d", serial, y, m, d, serialOf(y, m, d))
		}
	}
	if y, m, d := DateOf(2026, 11, 13).Civil(); y != 2026 || m != 11 || d != 13 {
		t.Errorf("Civil = %d-%d-%d", y, m, d)
	}
	for _, s := range []string{"", "2026-1-1", "2026-13-01", "2026-02-30", "1899-12-31", "3/15/2026", "2026-11-13 "} {
		if d, ok := ParseDate(s); ok {
			t.Errorf("ParseDate(%q) = %d", s, d)
		}
	}
}
