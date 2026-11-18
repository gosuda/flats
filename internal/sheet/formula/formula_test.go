package formula

import (
	"math"
	"testing"
)

func TestErrorCodeText(t *testing.T) {
	for e := ErrSyntax; e <= ErrLimit; e++ {
		got, ok := ParseErrorCode(e.String())
		if !ok || got != e {
			t.Errorf("ParseErrorCode(%q) = %v, %v", e.String(), got, ok)
		}
	}
	if _, ok := ParseErrorCode("#ref!"); ok {
		t.Error("ParseErrorCode accepts small letters")
	}
}

func TestValueZeroIsBlank(t *testing.T) {
	var v Value
	if !v.IsBlank() || v != (Content{}).Value() {
		t.Errorf("zero Value = %+v, want blank", v)
	}
	if NumberValue(0) != NumberValue(math.Copysign(0, -1)) {
		t.Error("the two zeros are different values")
	}
}

func TestColumnName(t *testing.T) {
	for col, want := range map[int32]string{0: "A", 25: "Z", 26: "AA", 701: "ZZ", 702: "AAA"} {
		if got := ColumnName(col); got != want {
			t.Errorf("ColumnName(%d) = %q, want %q", col, got, want)
		}
	}
	if got := (Pos{Row: 6, Col: 1}).A1(); got != "B7" {
		t.Errorf("A1 = %q, want B7", got)
	}
}
