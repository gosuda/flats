package formula

import (
	"errors"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

func pos(t testing.TB, a1 string) Pos {
	t.Helper()
	return parseA1Cell(t, a1)
}

func mustParse(t testing.TB, text, at string) *Formula {
	t.Helper()
	f, err := Parse(text, pos(t, at), Limits{})
	if err != nil {
		t.Fatalf("Parse(%q): %v", text, err)
	}
	return f
}

// TestStoredForm holds the examples of the technical design, section 8.1,
// and the other forms a slot takes.
func TestStoredForm(t *testing.T) {
	cases := []struct {
		at, typed, stored, text string
	}{
		{"E5", "=C5*D5", "={R[0]C[-2]}*{R[0]C[-1]}", ""},
		{"E5", "=SUM(B$2:B4)", "=SUM({R2C[-3]:R[-1]C[-3]})", ""},
		{"E5", "=sum( Costs!B:D )", "=sum( {Costs!C[-3]:C[-1]} )", ""},
		{"E5", "='Q1 costs'!$B$3", "={'Q1 costs'!R3C2}", ""},
		{"E5", "=SUM(B3", "=SUM({R[-2]C[-3]}", ""},
		{"E5", "=$2:5", "={R2:R[0]}", ""},
		{"E5", "=$B:$D", "={C2:C4}", ""},
		{"A1", "=B4:B2", "={R[3]C[1]:R[1]C[1]}", ""},
		{"C3", "=a1+b02", "={R[-2]C[-2]}+{R[-1]C[-1]}", "=A1+B2"},
		{"C3", `="{R[0]C[0]}"&A1`, `="{R[0]C[0]}"&{R[-2]C[-2]}`, ""},
		{"C3", "='It''s'!A1", "={'It''s'!R[-2]C[-2]}", ""},
		{"A1", "=A100001+AAA1", "={R[100000]C[0]}+{R[0]C[702]}", ""},
		{"A1", "=A0+A9999999999", "=A0+A9999999999", ""},
		{"A1", "=#REF!*2", "=#REF!*2", ""},
		{"A1", "= 1 +  2 ", "= 1 +  2 ", ""},
	}
	for _, c := range cases {
		f := mustParse(t, c.typed, c.at)
		if f.Unreadable() {
			t.Errorf("Parse(%q) is unreadable", c.typed)
			continue
		}
		if got := f.Stored(); got != c.stored {
			t.Errorf("Parse(%q at %s).Stored() = %q, want %q", c.typed, c.at, got, c.stored)
		}
		want := c.text
		if want == "" {
			want = c.typed
		}
		at := pos(t, c.at)
		if got := f.Text(at); got != want {
			t.Errorf("Parse(%q at %s).Text() = %q, want %q", c.typed, c.at, got, want)
		}
		if got := f.TextLen(at); got != utf8.RuneCountInString(want) {
			t.Errorf("Parse(%q).TextLen() = %d, want %d", c.typed, got, utf8.RuneCountInString(want))
		}
		g, err := ParseStored(c.stored)
		if err != nil {
			t.Errorf("ParseStored(%q): %v", c.stored, err)
			continue
		}
		if g.Stored() != c.stored || g.Text(at) != want {
			t.Errorf("ParseStored(%q) gives %q and %q", c.stored, g.Stored(), g.Text(at))
		}
	}
}

func TestSyntaxErrorKeepsItsText(t *testing.T) {
	f := mustParse(t, "=SUM(B3", "E1")
	if v := f.Eval(&Env{Grid: &testSheet{}}); v != ErrorValue(ErrSyntax) {
		t.Errorf("value = %v, want #ERROR!", v)
	}
	var refs int
	f.Refs(Pos{}, func(Ref) { refs++ })
	if refs != 0 || f.ReadCells(Pos{}, nil) != 0 {
		t.Errorf("a formula that cannot be read has %d references", refs)
	}
	// The reference still follows structure.
	g, at, changed := f.Apply("Sheet1", pos(t, "E1"), Transform{Kind: InsertRows, Tab: "Sheet1", At: 1, Count: 1})
	if !changed || g.Text(at) != "=SUM(B4" {
		t.Errorf("after an insert the text is %q", g.Text(at))
	}
}

func TestUnreadable(t *testing.T) {
	for _, typed := range []string{`="abc`, "={1,2}", "=A1;B1", "=#ref!", "=A1:B", "='Costs!A1", "=1 $", "=[1]", "=예산", "=예산(1)", "=A1!B1", "=TRUE!A1", "=Cafe\u0301!A1", "=예 산!A1"} {
		f := mustParse(t, typed, "C3")
		if !f.Unreadable() {
			t.Errorf("Parse(%q) is readable, stored %q", typed, f.Stored())
			continue
		}
		at := pos(t, "C3")
		if f.Stored() != typed || f.Text(at) != typed || f.TextLen(at) != utf8.RuneCountInString(typed) {
			t.Errorf("Parse(%q) does not keep its text: %q, %q, %d", typed, f.Stored(), f.Text(at), f.TextLen(at))
		}
		if v := f.Eval(&Env{Grid: &testSheet{}}); v != ErrorValue(ErrSyntax) {
			t.Errorf("Parse(%q) has the value %v", typed, v)
		}
		g, at2, changed := f.Apply("Sheet1", at, Transform{Kind: InsertRows, Tab: "Sheet1", At: 0, Count: 3})
		if g != f || changed || at2 != (Pos{Row: 5, Col: 2}) {
			t.Errorf("Apply changes the unreadable %q", typed)
		}
		if f.Moved(at, Pos{}) != f || f.Cut(at, Pos{}, "A", "B") != f || f.NamesTab() {
			t.Errorf("a transform changes the unreadable %q", typed)
		}
	}
}

func TestParseStoredRefusals(t *testing.T) {
	for _, stored := range []string{
		"", "1+2", "=A1", "={R[0]C[0]", "={R[+1]C[0]}", "={R[01]C[0]}", "={R[-0]C[0]}", "={R0C1}", "={R01C1}",
		"={R[0]}", "={C[0]}", "={R[0]C[0]:R[1]}", "={R[0]:C[1]}", "={R1C1:R2C2:R3C3}", "={}", "={Costs!}",
		"={R[1000000000]C[0]}", "={R[0]C[18278]}", "={R1000000001C1}", "={R1C18279}", `="abc`, "=1 $",
		"={'Costs!R1C1}", "={R[0]C[0]}}", "={r[0]c[0]}",
	} {
		if f, err := ParseStored(stored); err == nil {
			t.Errorf("ParseStored(%q) = %q, want an error", stored, f.Stored())
		} else if !errors.Is(err, ErrStored) {
			t.Errorf("ParseStored(%q) error = %v", stored, err)
		}
	}
	deep := "=" + strings.Repeat("(", MaxFormulaDepth+1) + "1" + strings.Repeat(")", MaxFormulaDepth+1)
	if _, err := ParseStored(deep); !errors.Is(err, ErrStored) {
		t.Errorf("ParseStored of nesting %d: %v, want ErrStored", MaxFormulaDepth+1, err)
	}
	deepest := "=" + strings.Repeat("(", MaxFormulaDepth) + "1" + strings.Repeat(")", MaxFormulaDepth)
	if _, err := ParseStored(deepest); err != nil {
		t.Errorf("ParseStored of nesting %d: %v", MaxFormulaDepth, err)
	}
}

// ParseStored refuses a stored form longer than MaxStoredBytes without
// reading it: a sum of a million numbers costs it no allocation. A form of
// exactly MaxStoredBytes is read when it holds no more tokens than a
// formula can, and a sum of ones of that length, which holds nine times
// as many, is refused (TestStoredTokenBound).
func TestParseStoredLength(t *testing.T) {
	huge := "=1" + strings.Repeat("+1", 1_000_000)
	if _, err := ParseStored(huge); !errors.Is(err, ErrStored) {
		t.Fatalf("ParseStored of %d bytes: %v, want ErrStored", len(huge), err)
	}
	if n := testing.AllocsPerRun(10, func() { _, _ = ParseStored(huge) }); n != 0 {
		t.Errorf("ParseStored of %d bytes allocates %v times, want none", len(huge), n)
	}
	longest := `="` + strings.Repeat("😀", (MaxStoredBytes-3)/4) + `x"`
	over := longest[:len(longest)-1] + `x"`
	if len(longest) != MaxStoredBytes || len(over) != MaxStoredBytes+1 {
		t.Fatalf("the long forms have %d and %d bytes", len(longest), len(over))
	}
	if _, err := ParseStored(over); !errors.Is(err, ErrStored) {
		t.Errorf("ParseStored of %d bytes: %v, want ErrStored", len(over), err)
	}
	if _, err := ParseStored(longest); err != nil {
		t.Errorf("ParseStored of %d bytes: %v", len(longest), err)
	}
	ones := "=1" + strings.Repeat("+1", (MaxStoredBytes-2)/2)
	if _, err := ParseStored(ones); len(ones) != MaxStoredBytes || !errors.Is(err, ErrStored) {
		t.Errorf("ParseStored of a sum of ones of %d bytes: %v, want ErrStored", len(ones), err)
	}
}

// Every reference of a formula typed at the last cell of the grid can be a
// slot several times longer than its text: such a formula of 8,190
// characters is still a stored form ParseStored reads, and LimitAt passes
// it at its own cell. At A1 the same stored form has references past what
// a formula can name, which the writer would have ended, and LimitAt
// refuses it.
func TestStoredFormLongerThanItsText(t *testing.T) {
	far := Pos{Row: MaxRows - 1, Col: MaxCols - 1}
	for _, ref := range []string{"A1", "A1:A1", "1:1", "A:A", "$A1", "A$1"} {
		n := (MaxFormulaChars - 1) / (len(ref) + 1)
		text := "=" + ref + strings.Repeat("+"+ref, n-1)
		typed, err := Parse(text, far, Limits{})
		if err != nil {
			t.Fatalf("Parse of %d references %s: %v", n, ref, err)
		}
		stored := typed.Stored()
		if len(stored) <= len(text) || len(stored) > MaxStoredBytes {
			t.Fatalf("%s: the stored form has %d bytes for %d characters, want more and at most %d", ref, len(stored), len(text), MaxStoredBytes)
		}
		f, err := ParseStored(stored)
		if err != nil {
			t.Fatalf("%s: ParseStored: %v", ref, err)
		}
		if err := f.LimitAt(far); err != nil {
			t.Errorf("%s: LimitAt the cell it was typed at: %v", ref, err)
		}
		if err := f.LimitAt(Pos{}); !errors.Is(err, ErrStored) {
			t.Errorf("%s: LimitAt A1: %v", ref, err)
		}
	}
}

// A text result is no longer than MaxTextLen code points, also the
// literal of a stored form that no typed formula could hold: it is
// #VALUE!.
func TestStoredLiteralOverTheTextLimit(t *testing.T) {
	for _, c := range []struct {
		n    int
		long bool
	}{{MaxTextLen, false}, {MaxTextLen + 1, true}} {
		f, err := ParseStored(`="` + strings.Repeat("x", c.n) + `"`)
		if err != nil {
			t.Fatal(err)
		}
		v := f.Eval(&Env{Budget: NewBudget(nil)})
		if c.long && v.Err() != ErrValue || !c.long && (v.Kind() != KindText || v.TextLen() != c.n) {
			t.Errorf("a literal of %d characters evaluates to %v, error %v, of %d characters", c.n, v.Kind(), v.Err(), v.TextLen())
		}
	}
}

func TestParseLimits(t *testing.T) {
	at := Pos{}
	long := "=" + strings.Repeat("1", 8191)
	if _, err := Parse(long, at, Limits{}); err != nil {
		t.Errorf("a formula of 8,192 characters: %v", err)
	}
	var le *LimitError
	if _, err := Parse(long+"1", at, Limits{}); !errors.As(err, &le) || le.Name != "formula_length" || le.Limit != 8192 || le.Value != 8193 {
		t.Errorf("a formula of 8,193 characters: %v", err)
	}
	// The limit counts code points, not bytes.
	if _, err := Parse(`="`+strings.Repeat("한", 8189)+`"`, at, Limits{}); err != nil {
		t.Errorf("a formula of 8,192 code points: %v", err)
	}
	// An unreadable formula is held to the length as well.
	if _, err := Parse(`="`+strings.Repeat("a", 8192), at, Limits{}); !errors.As(err, &le) || le.Name != "formula_length" {
		t.Errorf("an unreadable formula of 8,194 characters: %v", err)
	}

	nest := func(n int) string { return "=" + strings.Repeat("(", n) + "1" + strings.Repeat(")", n) }
	if f, err := Parse(nest(64), at, Limits{}); err != nil || f.Eval(&Env{Grid: &testSheet{}}) != NumberValue(1) {
		t.Errorf("64 levels: %v", err)
	}
	if _, err := Parse(nest(65), at, Limits{}); !errors.As(err, &le) || le.Name != "formula_depth" || le.Limit != 64 || le.Value != 65 {
		t.Errorf("65 levels: %v", err)
	}
	calls := "=" + strings.Repeat("ABS(", 65) + "1" + strings.Repeat(")", 65)
	if _, err := Parse(calls, at, Limits{}); !errors.As(err, &le) || le.Name != "formula_depth" {
		t.Errorf("65 nested calls: %v", err)
	}
	// Brackets side by side are one level each.
	if _, err := Parse("="+strings.Repeat("(1)+", 200)+"1", at, Limits{}); err != nil {
		t.Errorf("200 brackets side by side: %v", err)
	}
	// A formula with a bracket missing counts the same way.
	if _, err := Parse("="+strings.Repeat("(", 65), at, Limits{}); !errors.As(err, &le) || le.Name != "formula_depth" {
		t.Errorf("65 open brackets: %v", err)
	}
	// Text the lexer cannot split is held to the nesting limit as well:
	// its brackets count wherever the unreadable part stands.
	for name, typed := range map[string]string{
		"an unreadable end":       nest(65) + "@",
		"an unreadable start":     "=@" + nest(65)[1:],
		"an unreadable middle":    "=" + strings.Repeat("(", 33) + "@" + strings.Repeat("(", 32) + "1",
		"a brace":                 "={" + nest(65)[1:],
		"an unknown error":        "=#x+" + nest(65)[1:],
		"an apostrophe":           "='" + nest(65)[1:],
		"a tab name that is none": "=1!A1+" + nest(65)[1:],
		"text that is not closed": nest(65) + `&"a`,
		"brackets after a quotation mark that is not closed": `="` + nest(65)[1:],
		"brackets in apostrophes that name no tab":           "='" + nest(65)[1:] + "'",
		"calls and a bad end":                                "=" + strings.Repeat("ABS(", 65) + "1" + strings.Repeat(")", 65) + "@",
		"brackets after quoted ones":                         `="(((("&` + nest(65)[1:] + "@",
	} {
		_, err := Parse(typed, at, Limits{})
		if !errors.As(err, &le) || le.Name != "formula_depth" || le.Limit != 64 || le.Value != 65 {
			t.Errorf("65 levels with %s: %v", name, err)
		}
	}
	// Brackets inside text in quotation marks and inside a tab name in
	// apostrophes are no nesting, in unreadable text too.
	for name, typed := range map[string]string{
		"64 levels":                    nest(64) + "@",
		"brackets side by side":        "=" + strings.Repeat("(1)+", 200) + "@",
		"brackets in text":             `="` + strings.Repeat("(", 65) + `"@`,
		"brackets in a tab name":       "='" + strings.Repeat("(", 65) + "'!A1+@",
		"brackets in text with quotes": `="a""` + strings.Repeat("(", 65) + `""b"&((1))@`,
		"closed brackets":              "=" + strings.Repeat("(", 40) + strings.Repeat(")", 40) + "@" + strings.Repeat("(", 40),
	} {
		f, err := Parse(typed, at, Limits{})
		if err != nil || !f.Unreadable() || f.Stored() != typed {
			t.Errorf("%s in unreadable text: %v", name, err)
		}
	}
	if _, err := Parse("=((1))@", at, Limits{MaxDepth: 1}); !errors.As(err, &le) || le.Limit != 1 || le.Value != 2 {
		t.Errorf("Limits.MaxDepth is not used for unreadable text: %v", err)
	}
	if _, err := Parse("=((1))", at, Limits{MaxChars: 5}); !errors.As(err, &le) || le.Limit != 5 {
		t.Errorf("Limits.MaxChars is not used: %v", err)
	}
	if _, err := Parse("=((1))", at, Limits{MaxDepth: 1}); !errors.As(err, &le) || le.Limit != 1 || le.Value != 2 {
		t.Errorf("Limits.MaxDepth is not used: %v", err)
	}
}

func TestLongChainsNeedNoDeepStack(t *testing.T) {
	env := &Env{Grid: &testSheet{}}
	if v := mustParse(t, "="+strings.Repeat("-", 4000)+"1", "A1").Eval(env); v != NumberValue(1) {
		t.Errorf("4,000 signs: %v", v)
	}
	if v := mustParse(t, "=1"+strings.Repeat("%", 3), "A1").Eval(env); !agree15(v.Number(), 1e-6) {
		t.Errorf("1%%%%%% = %v", v)
	}
	if v := mustParse(t, "=1"+strings.Repeat("+1", 4000), "A1").Eval(env); v != NumberValue(4001) {
		t.Errorf("4,001 terms: %v", v)
	}
}

func TestFormulaFacts(t *testing.T) {
	cases := []struct {
		typed                   string
		namesTab, today, isDate bool
		refs                    []string
	}{
		{"=A1+B2", false, false, false, []string{"A1", "B2"}},
		{"=TODAY()", false, true, true, nil},
		{"=(TODAY())", false, true, true, nil},
		{"=TODAY()+1", false, true, false, nil},
		{"=IF(A1,TODAY(),1)", false, true, false, []string{"A1"}},
		{"=DATE(2026,1,1)", false, false, true, nil},
		{"=EDATE(A1,1)", false, false, true, []string{"A1"}},
		{"=eomonth(A1,1)", false, false, true, []string{"A1"}},
		{"=+DATE(2026,1,1)", false, false, false, nil},
		{"=SUM(Costs!B:D)+'Q1 costs'!A1", true, false, false, []string{"Costs!B:D", "'Q1 costs'!A1"}},
		{"=SUM(2:5)", false, false, false, []string{"2:5"}},
		{"=B4:B2", false, false, false, []string{"B2:B4"}},
		{"=A100001+ZZ1+AAA1", false, false, false, []string{"ZZ1"}},
		{"=IF(1,A1", false, false, false, nil},
		{"=SUM()", false, false, false, nil},
		{"=TODAY(1)", false, false, false, nil},
	}
	for _, c := range cases {
		f := mustParse(t, c.typed, "Z9")
		if f.NamesTab() != c.namesTab || f.ReadsToday() != c.today || f.DateResult() != c.isDate {
			t.Errorf("%s: NamesTab %v, ReadsToday %v, DateResult %v", c.typed, f.NamesTab(), f.ReadsToday(), f.DateResult())
		}
		var refs []string
		f.Refs(pos(t, "Z9"), func(r Ref) { refs = append(refs, r.String()) })
		if strings.Join(refs, " ") != strings.Join(c.refs, " ") {
			t.Errorf("%s: Refs = %q, want %q", c.typed, refs, c.refs)
		}
	}
}

func TestReadCells(t *testing.T) {
	dims := func(tab string) (int32, int32, bool) {
		switch tab {
		case "":
			return 1000, 26, true
		case "Costs":
			return 5000, 30, true
		}
		return 0, 0, false
	}
	for typed, want := range map[string]int64{
		"=A1":                       1,
		"=SUM(A1:B10)+C1":           21,
		"=SUM(B4:A1)":               8,
		"=SUM(B:D)":                 3000,
		"=SUM(2:5)":                 104,
		"=SUM(Costs!B:D,Costs!1:1)": 15030,
		"=SUM(Gone!A:A)+Gone!A1":    1,
		"=A1+A1":                    2,
		"=A100000:A100001":          0,
		"=SUM(A1":                   0,
		"=1+2":                      0,
	} {
		if got := mustParse(t, typed, "Z1").ReadCells(pos(t, "Z1"), dims); got != want {
			t.Errorf("ReadCells(%s) = %d, want %d", typed, got, want)
		}
	}
}

func TestParseRange(t *testing.T) {
	good := map[string]Range{
		"B3":               {First: Pos{2, 1}, Last: Pos{2, 1}},
		"b3:D9":            {First: Pos{2, 1}, Last: Pos{8, 3}},
		"D9:B3":            {First: Pos{2, 1}, Last: Pos{8, 3}},
		"$B$3:D9":          {First: Pos{2, 1}, Last: Pos{8, 3}},
		"B:D":              {First: Pos{0, 1}, Last: Pos{MaxRows - 1, 3}, FullCols: true},
		"5:2":              {First: Pos{1, 0}, Last: Pos{4, MaxCols - 1}, FullRows: true},
		"Costs!B3":         {Tab: "Costs", First: Pos{2, 1}, Last: Pos{2, 1}},
		"'Q1 costs'!B3:D9": {Tab: "Q1 costs", First: Pos{2, 1}, Last: Pos{8, 3}},
		"'It''s'!A1":       {Tab: "It's", First: Pos{}, Last: Pos{}},
		"ZZ100000":         {First: Pos{MaxRows - 1, MaxCols - 1}, Last: Pos{MaxRows - 1, MaxCols - 1}},
	}
	for a1, want := range good {
		got, err := ParseRange(a1)
		if err != nil || got != want {
			t.Errorf("ParseRange(%q) = %+v, %v, want %+v", a1, got, err, want)
		}
	}
	for a1, want := range map[string]string{"b3:D9": "B3:D9", "B:D": "B:D", "5:2": "2:5", "'Q1 costs'!B3": "'Q1 costs'!B3", "Costs!A1:A1": "Costs!A1", "'It''s'!A1": "'It''s'!A1"} {
		if r, _ := ParseRange(a1); r.String() != want {
			t.Errorf("ParseRange(%q).String() = %q, want %q", a1, r.String(), want)
		}
	}
	for _, a1 := range []string{"", "B", "3", "B3:", ":B3", "B3:D", "AAA1", "A100001", "A0", "B3 ", " B3", "B3:D9:E1", "Costs!", "Costs B3", "'Costs!B3", "SUM(B3)", "B3,D9", "1e5", "A:1"} {
		if r, err := ParseRange(a1); err == nil {
			t.Errorf("ParseRange(%q) = %+v, want an error", a1, r)
		}
	}
}

func TestTabNameNeedsQuotes(t *testing.T) {
	for name, want := range map[string]bool{
		"Costs": false, "Q1_costs.2026": false, "_x": false, "Q1 costs": true, "It's": true, "2026": true,
		"A1": true, "ZZZ99": true, "TRUE": true, "false": true, "a-b": true, "ABCD1": false, "": true,
		"예산": false, "Übersicht": false, "données_2026": false, "표١": false, "_예산.v2": false, "Ωmega": false,
		"Q1 예산": true, "Cafe\u0301": true, "١표": true, "예산!": true, "예'산": true, ".예산": true, "예-산": true,
	} {
		if got := TabNameNeedsQuotes(name); got != want {
			t.Errorf("TabNameNeedsQuotes(%q) = %v, want %v", name, got, want)
		}
		// A name as a formula writes it reads back as the same name.
		r, err := ParseRange(quoteTabName(name) + "!B2")
		if name != "" && (err != nil || r.Tab != name) {
			t.Errorf("the tab name %q reads back as %q, %v", name, r.Tab, err)
		}
	}
}

// randomFormula builds formula text from the pieces of the grammar. It is
// often broken on purpose.
func randomFormula(rng *rand.Rand) string {
	pieces := []string{
		"A1", "$B$2", "c$3", "$D4", "A1:B2", "$A$1:$B$2", "B:D", "$B:D", "2:5", "$2:$5", "Costs!A1", "'Q1 costs'!A1:C3",
		"'It''s'!B:B", "ZZ100000", "A100001", "AAA1", "A0", "1", "2.5", ".5", "1E5", "1e-3", `"text"`, `"say ""hi"""`,
		"예산!A1", "Übersicht!$B:$C", "'Q1 예산'!2:3", "예산", "é",
		`"{R[0]C[0]}"`, "TRUE", "false", "#REF!", "#N/A", "#DIV/0!", "SUM(", "IF(", "ABS(", "TODAY(", "NOPE(", "name",
		"(", ")", ",", "+", "-", "*", "/", "^", "&", "%", "=", "<>", "<", "<=", ">", ">=", " ", "  ",
	}
	var b strings.Builder
	b.WriteByte('=')
	for n := rng.Intn(12) + 1; n > 0; n-- {
		b.WriteString(pieces[rng.Intn(len(pieces))])
	}
	return b.String()
}

// checkRoundTrip checks what holds for every formula the lexer can split:
// the stored form reads back as itself, and the A1 text reads back as the
// same stored form.
func checkRoundTrip(t testing.TB, typed string, at Pos) *Formula {
	t.Helper()
	// Brackets count the same in the text as in its tokens, so readable
	// and unreadable text meet one nesting limit.
	if lx := (lexer{src: typed, at: at}); typed != "" && typed[0] == '=' && lx.run() {
		if got, want := textNesting(typed), nesting(lx.toks); got != want {
			t.Fatalf("textNesting(%q) = %d, its tokens nest %d deep", typed, got, want)
		}
	}
	f, err := Parse(typed, at, Limits{})
	if err != nil {
		var le *LimitError
		if !errors.As(err, &le) {
			t.Fatalf("Parse(%q): %v", typed, err)
		}
		return nil
	}
	text := f.Text(at)
	if got, want := f.TextLen(at), utf8.RuneCountInString(text); got != want {
		t.Fatalf("Parse(%q).TextLen() = %d, the text %q has %d", typed, got, text, want)
	}
	if f.Unreadable() {
		if f.Stored() != typed || text != typed {
			t.Fatalf("the unreadable %q is stored as %q and shown as %q", typed, f.Stored(), text)
		}
		return f
	}
	g, err := ParseStored(f.Stored())
	if err != nil {
		t.Fatalf("Parse(%q).Stored() = %q does not read back: %v", typed, f.Stored(), err)
	}
	if g.Stored() != f.Stored() || g.Text(at) != text {
		t.Fatalf("ParseStored(%q) gives %q and %q, want %q", f.Stored(), g.Stored(), g.Text(at), text)
	}
	if g.NamesTab() != f.NamesTab() || g.ReadsToday() != f.ReadsToday() || g.DateResult() != f.DateResult() {
		t.Fatalf("ParseStored(%q) differs in its facts", f.Stored())
	}
	h, err := Parse(text, at, Limits{MaxChars: 1 << 20})
	if err != nil {
		t.Fatalf("Parse(%q), the text of %q: %v", text, typed, err)
	}
	if h.Stored() != f.Stored() {
		t.Fatalf("the text %q of %q is stored as %q, want %q", text, typed, h.Stored(), f.Stored())
	}
	return f
}

func TestRoundTripRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(20261113))
	for i := 0; i < 20000; i++ {
		at := Pos{Row: int32(rng.Intn(30)), Col: int32(rng.Intn(30))}
		checkRoundTrip(t, randomFormula(rng), at)
	}
}

// TestTabNamesOfAnyScript checks that a tab name written without
// apostrophes may hold letters and decimal digits of any script. Function
// names stay ASCII.
func TestTabNamesOfAnyScript(t *testing.T) {
	at := pos(t, "C3")
	cases := []struct {
		typed, stored, tab, ref string
	}{
		{"=예산!A1", "={예산!R[-2]C[-2]}", "예산", "예산!A1"},
		{"=Übersicht!B2", "={Übersicht!R[-1]C[-1]}", "Übersicht", "Übersicht!B2"},
		{"=SUM(données_2026!$B:D)", "=SUM({données_2026!C2:C[1]})", "données_2026", "données_2026!B:D"},
		{"=표١!A1:B2", "={표١!R[-2]C[-2]:R[-1]C[-1]}", "표١", "표١!A1:B2"},
		{"=_예산.v2!$2:3", "={_예산.v2!R2:R[0]}", "_예산.v2", "_예산.v2!2:3"},
		{"=A1+Ωmega!$A$1", "={R[-2]C[-2]}+{Ωmega!R1C1}", "Ωmega", "Ωmega!A1"},
		{"=Total예산!A1", "={Total예산!R[-2]C[-2]}", "Total예산", "Total예산!A1"},
		// Names that need apostrophes: a space, and a combining mark, which
		// is neither a letter nor a digit.
		{"='Q1 예산'!A1", "={'Q1 예산'!R[-2]C[-2]}", "Q1 예산", "'Q1 예산'!A1"},
		{"='Cafe\u0301'!A1", "={'Cafe\u0301'!R[-2]C[-2]}", "Cafe\u0301", "'Cafe\u0301'!A1"},
		{"='예''산'!A1", "={'예''산'!R[-2]C[-2]}", "예'산", "'예''산'!A1"},
	}
	for _, c := range cases {
		f := checkRoundTrip(t, c.typed, at)
		if f.Unreadable() {
			t.Errorf("Parse(%q) is unreadable", c.typed)
			continue
		}
		if f.Stored() != c.stored || f.Text(at) != c.typed || !f.NamesTab() {
			t.Errorf("Parse(%q): stored %q, text %q, want the stored form %q", c.typed, f.Stored(), f.Text(at), c.stored)
		}
		g, err := ParseStored(c.stored)
		if err != nil || g.Stored() != c.stored || g.Text(at) != c.typed {
			t.Errorf("ParseStored(%q): %v", c.stored, err)
		}
		var named []Ref
		f.Refs(at, func(r Ref) {
			if r.Tab != "" {
				named = append(named, r)
			}
		})
		if len(named) != 1 || named[0].Tab != c.tab || named[0].String() != c.ref {
			t.Errorf("Parse(%q) reports the references %v, want %s", c.typed, named, c.ref)
		}
		if r, err := ParseRange(c.ref); err != nil || r.Tab != c.tab {
			t.Errorf("ParseRange(%q) = %+v, %v", c.ref, r, err)
		}
		if got, want := f.TextLen(at), utf8.RuneCountInString(c.typed); got != want {
			t.Errorf("Parse(%q).TextLen() = %d, want %d", c.typed, got, want)
		}
	}

	// A space ends a name: this is two references side by side, which the
	// lexer splits and the parser refuses.
	if f := mustParse(t, "=Q1 예산!A1", "C3"); f.Unreadable() || f.Stored() != "={R[-2]C[14]} {예산!R[-2]C[-2]}" {
		t.Errorf("=Q1 예산!A1 is stored as %q", f.Stored())
	}

	// A formula reads the tab, whatever the capitals of its name.
	cells := map[Pos]*testCell{{1, 1}: {content: Content{Kind: ContentNumber, Num: 7}}}
	sheet := &testSheet{tabs: []*testTab{
		{name: "Sheet1", rows: 10, cols: 10, cells: map[Pos]*testCell{}},
		{name: "Übersicht", rows: 10, cols: 10, cells: cells},
		{name: "예산", rows: 10, cols: 10, cells: cells},
		{name: "Q1 예산", rows: 10, cols: 10, cells: cells},
	}}
	for formula, want := range map[string]Value{
		"=예산!B2*2":                   NumberValue(14),
		"=übersicht!B2+ÜBERSICHT!B2": NumberValue(14),
		"=SUM(예산!A:C,'Q1 예산'!B2)":    NumberValue(14),
		"='q1 예산'!B2":                NumberValue(7),
		"=없는탭!B2":                    ErrorValue(ErrRef),
		"=예산!B2&합계(1)":               ErrorValue(ErrSyntax),
	} {
		if got := mustParse(t, formula, "C3").Eval(&Env{Grid: sheet, At: at}); got != want {
			t.Errorf("%s = %v, want %v", formula, got, want)
		}
	}

	// Transforms write a name with apostrophes only when it needs them.
	f := mustParse(t, "=Costs!A1+B2", "C3")
	for name, want := range map[string]string{
		"예산":         "=예산!A1+B2",
		"Übersicht":  "=Übersicht!A1+B2",
		"Q1 예산":      "='Q1 예산'!A1+B2",
		"Cafe\u0301": "='Cafe\u0301'!A1+B2",
		"١표":         "='١표'!A1+B2",
	} {
		g, _, changed := f.Apply("Sheet1", at, Transform{Kind: RenameTab, Tab: "costs", Name: name})
		if !changed || g.Text(at) != want {
			t.Errorf("rename to %q gives %s, want %s", name, g.Text(at), want)
		}
		checkRoundTrip(t, g.Text(at), at)
		back, _, _ := g.Apply("Sheet1", at, Transform{Kind: RenameTab, Tab: FoldName(name), Name: "Costs"})
		if back.Stored() != f.Stored() {
			t.Errorf("rename to %q and back gives %q, want %q", name, back.Stored(), f.Stored())
		}
		written := strings.TrimSuffix(strings.TrimPrefix(want, "="), "A1+B2") // the name and its "!"
		cut := mustParse(t, "=A1+B2", "C3").Cut(at, at, name, "Sheet1")
		if got := cut.Text(at); got != "="+written+"A1+"+written+"B2" {
			t.Errorf("a cut from %q gives %s", name, got)
		}
	}
	g, _, _ := mustParse(t, "=예산!A1", "C3").Apply("Sheet1", at, Transform{Kind: InsertRows, Tab: "예산", At: 0, Count: 2})
	if g.Text(at) != "=예산!A3" {
		t.Errorf("an insert in the tab 예산 gives %s", g.Text(at))
	}
}

// textNestingPlain counts as textNesting does and asks scanTabName about
// each apostrophe, which reads on to a far apostrophe from each one.
func textNestingPlain(s string) int {
	depth, deepest := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			if end := scanString(s, i); end > 0 {
				i = end - 1
			}
		case '\'':
			if _, end, ok := scanTabName(s, i); ok {
				i = end - 1
			}
		case '(':
			depth++
			deepest = max(deepest, depth)
		case ')':
			depth = max(depth-1, 0)
		}
	}
	return deepest
}

// The brackets of text that cannot be read are counted in a fixed number
// of passes over it, whatever its apostrophes, and the count is the one
// that asking scanTabName about each apostrophe gives.
func TestTextNestingApostrophes(t *testing.T) {
	rng := rand.New(rand.NewSource(20261113))
	pieces := []string{"'", "'", "'", "''", "'''", "!", "'!", "(", "(", ")", `"`, "a", "A1", "'a'!A1", "''!"}
	for i := 0; i < 300000; i++ {
		var b strings.Builder
		for n := rng.Intn(14); n > 0; n-- {
			b.WriteString(pieces[rng.Intn(len(pieces))])
		}
		if s := b.String(); textNesting(s) != textNestingPlain(s) {
			t.Fatalf("textNesting(%q) = %d, want %d", s, textNesting(s), textNestingPlain(s))
		}
	}
	for name, s := range map[string]string{
		"apostrophes":                 strings.Repeat("'", MaxFormulaChars),
		"pairs between letters":       "'" + strings.Repeat("a''", MaxFormulaChars/3),
		"single ones between letters": strings.Repeat("'a", MaxFormulaChars/2),
		"names without their mark":    strings.Repeat("'a'(", MaxFormulaChars/4),
		"names that are closed late":  strings.Repeat("''a", MaxFormulaChars/3-1) + "'!(",
		"runs of three":               strings.Repeat("'''(", MaxFormulaChars/4),
	} {
		if got, want := textNesting(s), textNestingPlain(s); got != want {
			t.Errorf("%s: textNesting = %d, want %d", name, got, want)
		}
		names := nameScan{s: s}
		for i := 0; i < len(s); i++ {
			if s[i] == '\'' {
				names.end(i)
			}
		}
		if names.read > 3*len(s) {
			t.Errorf("%s: %d characters read for a text of %d", name, names.read, len(s))
		}
	}
}
