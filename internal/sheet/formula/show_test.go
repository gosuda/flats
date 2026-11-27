package formula

import (
	"encoding/json"
	"strings"
	"testing"
)

func numberFormat(t testing.TB, object string) NumberFormat {
	t.Helper()
	var nf NumberFormat
	if err := json.Unmarshal([]byte(object), &nf); err != nil {
		t.Fatalf("number format %s: %v", object, err)
	}
	return nf
}

func TestNumberFormatJSON(t *testing.T) {
	// The objects of the reference table's README.
	for _, object := range []string{
		`{"kind":"general"}`,
		`{"kind":"number","decimals":2,"thousands":true}`,
		`{"kind":"number","decimals":0,"thousands":false}`,
		`{"kind":"percent","decimals":2}`,
		`{"kind":"currency","symbol":"$","decimals":2}`,
		`{"kind":"currency","symbol":"CHF","decimals":0}`,
		`{"kind":"currency","symbol":"₩","decimals":10}`,
		`{"kind":"date","pattern":"yyyy-mm-dd"}`,
		`{"kind":"date","pattern":"m/d/yyyy"}`,
		`{"kind":"date","pattern":"mmm d, yyyy"}`,
		`{"kind":"time","pattern":"hh:mm"}`,
		`{"kind":"time","pattern":"hh:mm:ss"}`,
		`{"kind":"time","pattern":"h:mm AM/PM"}`,
		`{"kind":"datetime","date":"m/d/yyyy","time":"h:mm AM/PM"}`,
		`{"kind":"text"}`,
	} {
		nf := numberFormat(t, object)
		if !nf.Valid() {
			t.Errorf("%s is not valid: %+v", object, nf)
		}
		out, err := json.Marshal(nf)
		if err != nil || string(out) != object {
			t.Errorf("%s is written as %s, %v", object, out, err)
		}
	}
	if (NumberFormat{}) != numberFormat(t, `{"kind":"general"}`) {
		t.Error("the zero NumberFormat is not General")
	}
	for _, object := range []string{
		`null`, `"general"`, `{}`, `{"kind":"money"}`, `{"kind":"general","decimals":2}`,
		`{"kind":"number","decimals":2}`, `{"kind":"number","decimals":11,"thousands":true}`,
		`{"kind":"number","decimals":-1,"thousands":true}`, `{"kind":"number","decimals":2,"thousands":true,"symbol":"$"}`,
		`{"kind":"percent"}`, `{"kind":"percent","decimals":2,"thousands":true}`,
		`{"kind":"currency","decimals":2}`, `{"kind":"currency","symbol":"","decimals":2}`,
		`{"kind":"currency","symbol":"ABCD","decimals":2}`, `{"kind":"currency","symbol":"1","decimals":2}`,
		`{"kind":"currency","symbol":"$","decimals":2,"thousands":false}`,
		`{"kind":"date"}`, `{"kind":"date","pattern":"dd/mm/yyyy"}`, `{"kind":"date","pattern":"hh:mm"}`,
		`{"kind":"time","pattern":"yyyy-mm-dd"}`, `{"kind":"datetime","date":"yyyy-mm-dd"}`,
		`{"kind":"datetime","pattern":"yyyy-mm-dd hh:mm"}`, `{"kind":"text","pattern":"@"}`,
		`{"kind":"general","extra":1}`, `{"kind":"Number","decimals":2,"thousands":true}`,
	} {
		var nf NumberFormat
		if err := json.Unmarshal([]byte(object), &nf); err == nil {
			t.Errorf("%s is read as %+v, want an error", object, nf)
		}
	}
	for _, nf := range []NumberFormat{
		{Kind: NumberGeneral, Decimals: 2}, {Kind: NumberFixed, Decimals: 11}, {Kind: NumberCurrency, Decimals: 2},
		{Kind: NumberCurrency, Symbol: "$", Thousands: true}, {Kind: NumberDate, Date: 3}, {Kind: NumberTime, Time: 3},
		{Kind: NumberText + 1}, {Kind: NumberPercent, Symbol: "%"}, {Kind: NumberDate, Time: TimeHMS},
	} {
		if nf.Valid() {
			t.Errorf("%+v is valid", nf)
		}
		if _, err := json.Marshal(nf); err == nil {
			t.Errorf("%+v is written as JSON", nf)
		}
	}
}

func TestColors(t *testing.T) {
	seen := map[string]bool{}
	for c := Color(1); c <= PaletteColors; c++ {
		name := c.String()
		got, ok := ParseColor(name)
		if !ok || got != c || seen[name] {
			t.Errorf("color %d is %q, which reads as %d, %v", c, name, got, ok)
		}
		seen[name] = true
	}
	if !seen["red-600"] || !seen["gray-100"] || !seen["magenta-800"] {
		t.Errorf("the palette lacks a color: %v", seen)
	}
	for _, name := range []string{"", "red", "red-500", "pink-600", "red-600 ", "RED-600", "-600"} {
		if c, ok := ParseColor(name); ok {
			t.Errorf("ParseColor(%q) = %d", name, c)
		}
	}
	if Color(0).String() != "" || Color(55).String() != "" {
		t.Error("a number that is no palette color has a name")
	}
}

func TestShow(t *testing.T) {
	day := float64(DateOf(2026, 3, 15))
	general := NumberFormat{}
	cases := []struct {
		v    Value
		kind ContentKind
		nf   string
		want string
	}{
		{Value{}, ContentBlank, "", ""},
		{Value{}, ContentFormula, `{"kind":"number","decimals":2,"thousands":true}`, ""},
		{TextValue("Rent"), ContentText, `{"kind":"number","decimals":2,"thousands":true}`, "Rent"},
		{TextValue("12"), ContentText, `{"kind":"percent","decimals":0}`, "12"},
		{BoolValue(true), ContentBool, `{"kind":"currency","symbol":"$","decimals":2}`, "TRUE"},
		{ErrorValue(ErrDiv0), ContentFormula, `{"kind":"date","pattern":"m/d/yyyy"}`, "#DIV/0!"},
		{NumberValue(1234.5), ContentNumber, "", "1234.5"},
		{NumberValue(1234.5), ContentNumber, `{"kind":"number","decimals":2,"thousands":true}`, "1,234.50"},
		{NumberValue(1234.5), ContentNumber, `{"kind":"percent","decimals":2}`, "123450.00%"},
		{NumberValue(1234.5), ContentNumber, `{"kind":"currency","symbol":"$","decimals":2}`, "$1,234.50"},
		{NumberValue(-1234.5), ContentNumber, `{"kind":"currency","symbol":"CHF","decimals":0}`, "-CHF1,235"},
		{NumberValue(1234.5), ContentNumber, `{"kind":"date","pattern":"yyyy-mm-dd"}`, "1903-05-18"},
		{NumberValue(1234.5), ContentNumber, `{"kind":"time","pattern":"hh:mm"}`, "12:00"},
		{NumberValue(1234.5), ContentNumber, `{"kind":"datetime","date":"yyyy-mm-dd","time":"hh:mm"}`, "1903-05-18 12:00"},
		{NumberValue(1234.5), ContentNumber, `{"kind":"text"}`, "1234.5"},
		// Content that is a date or a time shows as one without a number format.
		{NumberValue(day), ContentDate, "", "2026-03-15"},
		{NumberValue(0.60625), ContentTime, "", "14:33:00"},
		{NumberValue(day + 0.60625), ContentDateTime, "", "2026-03-15 14:33:00"},
		{NumberValue(day), ContentNumber, "", "46096"},
		{NumberValue(day), ContentFormula, "", "46096"},
		{NumberValue(day), ContentDate, `{"kind":"text"}`, "46096"},
		{NumberValue(day), ContentDate, `{"kind":"number","decimals":1,"thousands":true}`, "46,096.0"},
		{NumberValue(day), ContentDate, `{"kind":"date","pattern":"mmm d, yyyy"}`, "Mar 15, 2026"},
		{NumberValue(-1.5), ContentDate, "", "-1.5"},
		{NumberValue(-1.5), ContentNumber, `{"kind":"time","pattern":"h:mm AM/PM"}`, "-1.5"},
		{NumberValue(0), ContentNumber, `{"kind":"date","pattern":"yyyy-mm-dd"}`, "1899-12-30"},
		{NumberValue(lastDateDay + 0.5), ContentNumber, `{"kind":"datetime","date":"m/d/yyyy","time":"h:mm AM/PM"}`, "12/31/9999 12:00 PM"},
		{NumberValue(lastDateDay + 1), ContentNumber, `{"kind":"date","pattern":"yyyy-mm-dd"}`, "2958466"},
		{NumberValue(1e300), ContentNumber, `{"kind":"time","pattern":"hh:mm:ss"}`, "1E+300"},
		{NumberValue(0.999999), ContentNumber, `{"kind":"datetime","date":"yyyy-mm-dd","time":"hh:mm:ss"}`, "1899-12-31 00:00:00"},
	}
	for _, c := range cases {
		nf := general
		if c.nf != "" {
			nf = numberFormat(t, c.nf)
		}
		if got := Show(c.v, c.kind, nf); got != c.want {
			t.Errorf("Show(%v, %d, %s) = %q, want %q", c.v, c.kind, c.nf, got, c.want)
		}
		if got := string(AppendShow([]byte("x"), c.v, c.kind, nf)); got != "x"+c.want {
			t.Errorf("AppendShow(%v, %d, %s) = %q, want %q", c.v, c.kind, c.nf, got, "x"+c.want)
		}
	}
}

// csvCase is one export case of the technical design, section 22.2: what a
// cell shows and the exact CSV field.
type csvCase struct {
	id    string
	shown string
	want  string
}

// TestCSVFields holds the field cases E-01 to E-84. The rows that name a
// formula or a typed entry get their shown text from the engine.
func TestCSVFields(t *testing.T) {
	var cases []csvCase
	add := func(id, shown, want string) { cases = append(cases, csvCase{id, shown, want}) }
	today, _ := ParseDate("2026-11-13")
	typed := func(text string, nf string) string {
		t.Helper()
		content, sets := ParseTyped(text)
		format := NumberFormat{}
		if sets != nil {
			format = *sets
		}
		if nf != "" {
			format = numberFormat(t, nf)
		}
		return Show(content.Value(), content.Kind, format)
	}
	calculated := func(formula string, nf string) string {
		t.Helper()
		var format *NumberFormat
		if nf != "" {
			f := numberFormat(t, nf)
			format = &f
		}
		return formulaResult(t, &refRow{}, formula, today, format).shown
	}

	// E-01 to E-42: each first character that a program could read as the
	// start of a formula, with 0, 1 and 3 leading spaces, as typed text
	// and as text a formula calculates.
	n := 0
	for _, first := range []string{"=", "+", "-", "@", "\t", "\r", "\n"} {
		for _, spaces := range []int{0, 1, 3} {
			text := strings.Repeat(" ", spaces) + first + "x"
			want := "'" + text
			if strings.ContainsAny(first, "\t\r\n") {
				want = `"` + want + `"`
			}
			n++
			add("E-typed", typed("'"+text, ""), want)
			n++
			add("E-calculated", calculated(`=REPT(" ",`+string(rune('0'+spaces))+`)&"`+first+`x"`, ""), want)
		}
	}
	if n != 42 {
		t.Fatalf("E-01 to E-42 are %d cases", n)
	}

	// E-43 to E-58: a negative number under each number format, typed and
	// calculated. None gets an apostrophe.
	for _, c := range []struct{ nf, want string }{
		{"", "-1234.5"},
		{`{"kind":"general"}`, "-1234.5"},
		{`{"kind":"number","decimals":2,"thousands":true}`, `"-1,234.50"`},
		{`{"kind":"number","decimals":2,"thousands":false}`, "-1234.50"},
		{`{"kind":"number","decimals":0,"thousands":true}`, `"-1,235"`},
		{`{"kind":"percent","decimals":1}`, "-123450.0%"},
		{`{"kind":"currency","symbol":"$","decimals":2}`, `"-$1,234.50"`},
		{`{"kind":"text"}`, "-1234.5"},
	} {
		add("E-43 typed "+c.nf, typed("-1234.5", c.nf), c.want)
		add("E-43 calculated "+c.nf, calculated("=-2469/2", c.nf), c.want)
	}

	// E-59 to E-64: negative currency. A symbol made of letters makes the
	// field text, so it gets the apostrophe.
	add("E-59", typed("-5", `{"kind":"currency","symbol":"€","decimals":2}`), "-€5.00")
	add("E-60", typed("-5", `{"kind":"currency","symbol":"£","decimals":0}`), "-£5")
	add("E-61", typed("-1234", `{"kind":"currency","symbol":"¥","decimals":0}`), `"-¥1,234"`)
	add("E-62", calculated("=-5", `{"kind":"currency","symbol":"₩","decimals":0}`), "-₩5")
	add("E-63", typed("-5", `{"kind":"currency","symbol":"CHF","decimals":2}`), "'-CHF5.00")
	add("E-64", calculated("=-1234", `{"kind":"currency","symbol":"kr","decimals":0}`), `"'-kr1,234"`)

	// E-65 to E-68: a negative number under a date and a time format, one
	// that General shows with an exponent, and text that reads as a number.
	add("E-65", typed("-5", `{"kind":"date","pattern":"yyyy-mm-dd"}`), "-5")
	add("E-65 time", calculated("=-0.5", `{"kind":"time","pattern":"hh:mm"}`), "-0.5")
	add("E-66", calculated("=-1.5*10^20", ""), "-1.5E+20")
	add("E-67", typed("'-5", ""), "-5")
	add("E-68", calculated(`="-"&"5"`, ""), "-5")

	// E-69 to E-76: characters that need quotation marks.
	add("E-69", "a,b", `"a,b"`)
	add("E-70", "a;b", `"a;b"`)
	add("E-71", "a\tb", "\"a\tb\"")
	add("E-72", `say "hi"`, `"say ""hi"""`)
	add("E-73", "a\nb", "\"a\nb\"")
	add("E-74", "a\r\nb", "\"a\r\nb\"")
	add("E-75", "a;=1+1", `"a;=1+1"`)
	add("E-76", "=1,2", `"'=1,2"`)
	add("E-76 plus", "+1,2", `"'+1,2"`)
	add("E-76 quote", `@"x"`, `"'@""x"""`)

	// E-77 to E-84: booleans, errors, text outside ASCII, text typed with
	// an apostrophe and a blank cell.
	add("E-77", calculated("=1=1", ""), "TRUE")
	add("E-77 typed", typed("false", ""), "FALSE")
	for e := ErrSyntax; e <= ErrLimit; e++ {
		add("E-78", Show(ErrorValue(e), ContentFormula, NumberFormat{}), e.String())
	}
	add("E-78 calculated", calculated("=1/0", ""), "#DIV/0!")
	add("E-79", typed("예산", ""), "예산")
	add("E-80", typed("😀", ""), "😀")
	add("E-81", typed("'007", ""), "007")
	add("E-82", typed("''tis", ""), "'tis")
	add("E-83", typed("", ""), "")
	add("E-84", calculated("=A1", ""), "")

	// More of the number form of rule 5: these read as numbers.
	for _, field := range []string{"-5", "-5.5", "-1,234", "-1,234,567.89", "-$5", "-€1,000.50", "-5%", "-0.5%", "-1.5E+20", "-1e5", "-2E-3", "-£5E3", "-007"} {
		want := field
		if strings.Contains(field, ",") {
			want = `"` + field + `"`
		}
		add("number "+field, field, want)
	}
	// And these do not.
	for _, field := range []string{"-", "--5", "- 5", "-5-", "-.5", "-5.", "-1,23", "-12,345,6", "-1234,567", "-$$5", "-5$", "-CHF5", "-5 %", "-5%%", "-1E", "-1E+", "-5E3%", "-₩", "-x", " -5", "-5 ", "-٥", "-5\n"} {
		want := "'" + field
		if strings.ContainsAny(field, ",\n") {
			want = `"` + want + `"`
		}
		add("text "+field, field, want)
	}
	// Only the space counts as a leading space.
	add("no-break space", " =1", " =1")
	add("spaces only", "   ", "   ")
	add("plain", "Rent", "Rent")
	add("apostrophe inside", "it's", "it's")
	add("leading apostrophe", "'=1", "'=1")

	for _, c := range cases {
		if got := string(AppendCSVField(nil, c.shown)); got != c.want {
			t.Errorf("%s: the field for %q is %q, want %q", c.id, c.shown, got, c.want)
		}
		if got := string(AppendCSVField([]byte("a,"), c.shown)); got != "a,"+c.want {
			t.Errorf("%s: appended to a row the field for %q is %q", c.id, c.shown, got)
		}
	}
}

func TestFunctions(t *testing.T) {
	fns := Functions()
	if len(fns) != 104 {
		t.Fatalf("Functions() has %d functions, want 104", len(fns))
	}
	groups := map[string]int{}
	for i, fn := range fns {
		if i > 0 && fns[i-1].Name >= fn.Name {
			t.Errorf("%s and %s are not in order", fns[i-1].Name, fn.Name)
		}
		id, ok := lookupFunc(strings.ToLower(fn.Name))
		if !ok || int(id) != i || funcTable[id].name != fn.Name {
			t.Errorf("lookupFunc(%q) = %d, %v, want %d", strings.ToLower(fn.Name), id, ok, i)
		}
		groups[fn.Group]++
		spec := &funcTable[id]
		if fn.Description == "" || !strings.HasSuffix(fn.Description, ".") {
			t.Errorf("%s has the description %q", fn.Name, fn.Description)
		}
		// The argument names agree with the numbers of arguments.
		required, optional, repeats := 0, 0, false
		for _, arg := range fn.Args {
			switch {
			case strings.HasSuffix(arg, "...]"):
				repeats = true
				optional += strings.Count(arg, ",")
			case strings.HasPrefix(arg, "["):
				optional++
			default:
				required++
			}
		}
		if required != spec.min {
			t.Errorf("%s names %d required arguments %q and takes at least %d", fn.Name, required, fn.Args, spec.min)
		}
		if repeats != (spec.max == anyCount) || (!repeats && required+optional != spec.max) {
			t.Errorf("%s names the arguments %q and takes at most %d", fn.Name, fn.Args, spec.max)
		}
	}
	want := map[string]int{"Math": 21, "Statistics": 14, "Conditional totals": 8, "Logic": 18, "Text": 19, "Lookup": 10, "Date and time": 14}
	for group, n := range want {
		if groups[group] != n {
			t.Errorf("the group %s has %d functions, want %d", group, groups[group], n)
		}
	}
	if len(groups) != len(want) {
		t.Errorf("groups: %v", groups)
	}
	fns[0].Name = "changed"
	if Functions()[0].Name != "ABS" {
		t.Error("Functions returns shared memory")
	}
	for _, name := range []string{"", "SUMX", "SU", "sum ", "CONCATENATEX", "Σ"} {
		if id, ok := lookupFunc(name); ok {
			t.Errorf("lookupFunc(%q) = %d", name, id)
		}
	}
	// Every function is wired: none answers #NAME? to a call with the
	// fewest arguments it takes.
	env := &Env{Grid: &testSheet{tabs: []*testTab{{name: "Sheet1", rows: 10, cols: 10, cells: map[Pos]*testCell{}}}}}
	for id := range funcTable {
		spec := &funcTable[id]
		args := make([]*node, spec.min)
		for i := range args {
			args[i] = &node{kind: nodeNumber, num: 1}
		}
		if v := (&Formula{root: &node{kind: nodeCall, fn: fnID(id), args: args}}).Eval(env); v == ErrorValue(ErrName) {
			t.Errorf("%s is not wired to an implementation", spec.name)
		}
	}
}

func TestBudget(t *testing.T) {
	var nilBudget *Budget
	if !nilBudget.Add(1e9) || nilBudget.Used() != 0 || nilBudget.Exceeded() {
		t.Error("a nil budget counts or stops")
	}
	checks := 0
	b := NewBudget(func() bool {
		checks++
		return checks >= 3
	})
	for i := 0; i < 3*budgetStep-1; i++ {
		if !b.Add(1) {
			t.Fatalf("the budget stops after %d units and %d checks", b.Used(), checks)
		}
	}
	if b.Add(1) || !b.Exceeded() || checks != 3 || b.Used() != 3*budgetStep {
		t.Errorf("after %d units: %d checks, exceeded %v", b.Used(), checks, b.Exceeded())
	}

	// A formula that reads a large range stops soon after the deadline.
	// Each function is calculated alone: in one formula the first to stop
	// would end the others before they read a cell.
	// TestEachRangeFunctionMeetsTheDeadline has every function.
	tab := &testTab{name: "Sheet1", rows: MaxRows, cols: MaxCols, cells: map[Pos]*testCell{}}
	sheet := &testSheet{tabs: []*testTab{tab}}
	for _, formula := range []string{"=SUM(A:ZZ)", "=COUNTIF(A:ZZ,1)", "=MEDIAN(A:ZZ)", "=COUNTA(A:ZZ)", "=SUMPRODUCT(A:ZZ,A:ZZ)"} {
		over := NewBudget(func() bool { return true })
		mustParse(t, formula, "A1").Eval(&Env{Grid: sheet, Budget: over})
		if !over.Exceeded() || over.Used() > 4*budgetStep {
			t.Errorf("%s went on for %d units past its deadline", formula, over.Used())
		}
	}
	counting := NewBudget(nil)
	mustParse(t, "=SUM(B2:B101)+LEN(REPT(\"a\",6400))", "A1").Eval(&Env{Grid: sheet, Budget: counting})
	if got := counting.Used(); got < 200 || got > 400 {
		t.Errorf("100 cells and 6,400 characters count %d units", got)
	}
}

func TestLimitedMode(t *testing.T) {
	sheet := &testSheet{tabs: []*testTab{{name: "Sheet1", rows: 10, cols: 10, cells: map[Pos]*testCell{}}}}
	for formula, want := range map[string]Value{
		`=REPT("b",255)`:      TextValue(strings.Repeat("b", 255)),
		`=REPT("b",256)`:      ErrorValue(ErrLimit),
		`=REPT("한",255)`:      TextValue(strings.Repeat("한", 255)),
		`=LEN(REPT("b",300))`: NumberValue(300),
		`=IF(ISERROR(#LIMIT!),"short",REPT("b",300))`: TextValue("short"),
		`=#LIMIT!&"x"`: ErrorValue(ErrLimit),
	} {
		if got := mustParse(t, formula, "A1").Eval(&Env{Grid: sheet, Limited: true}); got != want {
			t.Errorf("limited mode: %s = %v, want %v", formula, got, want)
		}
	}
	if got := mustParse(t, `=REPT("b",256)`, "A1").Eval(&Env{Grid: sheet}); got.TextLen() != 256 {
		t.Errorf("outside limited mode the text has %d characters", got.TextLen())
	}
	// The 32,767 character rule holds for every text value on the way.
	for formula, want := range map[string]Value{
		`=LEN(REPT("a",32767))`:                      NumberValue(32767),
		`=REPT("a",32768)`:                           ErrorValue(ErrValue),
		`=LEN(REPT("a",32767)&"b")`:                  ErrorValue(ErrValue),
		`=LEN(REPT("한",32767))`:                      NumberValue(32767),
		`=LEN(CONCAT(REPT("a",32767),"b"))`:          ErrorValue(ErrValue),
		`=LEN(SUBSTITUTE(REPT("a",20000),"a","bb"))`: ErrorValue(ErrValue),
		`=LEN(REPLACE(REPT("a",32767),1,0,"b"))`:     ErrorValue(ErrValue),
		`=LEN(TEXTJOIN(",",FALSE,A1:J10,A1:ZZ500))`:  ErrorValue(ErrValue),
		`=LEN(TEXTJOIN(",",TRUE,A1:ZZ500))`:          NumberValue(0),
	} {
		if got := mustParse(t, formula, "Z99").Eval(&Env{Grid: sheet, At: pos(t, "Z99")}); got != want {
			t.Errorf("%s = %v, want %v", formula, got, want)
		}
	}
}

// A range beside or under the tab holds only blank cells. No function reads
// them, counts work for them or goes through them one by one.
func TestRangesOutsideTheTab(t *testing.T) {
	cells := map[Pos]*testCell{{0, 0}: {content: Content{Kind: ContentNumber, Num: 4}}}
	sheet := &testSheet{tabs: []*testTab{{name: "Sheet1", rows: 10, cols: 3, cells: cells}}}
	for formula, want := range map[string]Value{
		"=SUM(E1:ZZ100000)":                    NumberValue(0),
		"=SUM(A1:ZZ100000)":                    NumberValue(4),
		"=SUM(A20:C100000)+A1":                 NumberValue(4),
		"=COUNTA(E1:ZZ100000,A1:A100000)":      NumberValue(1),
		"=COUNT(E:ZZ)+COUNT(20:30)":            NumberValue(0),
		"=COUNTBLANK(A1:ZZ100000)":             NumberValue(70200000 - 1),
		`=COUNTIF(A1:ZZ100000,"")`:             NumberValue(70200000 - 1),
		`=COUNTIF(A1:ZZ100000,4)`:              NumberValue(1),
		`=SUMIF(E1:E100000,"",A1:A100000)`:     NumberValue(4),
		"=MEDIAN(E1:ZZ100000,7)":               NumberValue(7),
		"=SUMPRODUCT(A1:A100000,E1:E100000)":   NumberValue(0),
		"=ROWS(E1:ZZ100000)*COLUMNS(E1:ZZ9)":   NumberValue(100000 * 698),
		"=INDEX(A1:ZZ100000,99999,700)&\"x\"":  TextValue("x"),
		"=ISBLANK(ZZ100000)":                   BoolValue(true),
		"=ZZ100000":                            {},
		`=MATCH(4,A1:A100000,0)`:               NumberValue(1),
		`=IFERROR(MATCH(5,E1:E100000,0),"no")`: TextValue("no"),
		`=CONCAT(E1:ZZ100000)`:                 TextValue(""),
		`=AND(E1:ZZ100000)`:                    ErrorValue(ErrValue),
	} {
		budget := NewBudget(nil)
		if got := mustParse(t, formula, "B5").Eval(&Env{Grid: sheet, At: pos(t, "B5"), Budget: budget}); got != want {
			t.Errorf("%s = %v, want %v", formula, got, want)
		}
		if used := budget.Used(); used < 0 || used > 100 {
			t.Errorf("%s counts %d units of work on a tab of 30 cells", formula, used)
		}
	}
}
