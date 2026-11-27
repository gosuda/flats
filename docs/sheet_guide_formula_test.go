package runtimeref

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// guideSheet is the one-tab sheet the guide's formula claims are evaluated
// on.
type guideSheet map[formula.Pos]formula.Value

func (g guideSheet) Tab(name string) (formula.TabRef, bool) {
	return 1, formula.FoldName(name) == formula.FoldName("Sheet1")
}
func (g guideSheet) Dims(formula.TabRef) (rows, cols int32)              { return 20, 5 }
func (g guideSheet) Value(_ formula.TabRef, p formula.Pos) formula.Value { return g[p] }
func (g guideSheet) Filled(_ formula.TabRef, p formula.Pos) bool {
	_, ok := g[p]
	return ok
}

// calc evaluates a formula as typed into E1 of the sheet.
func (g guideSheet) calc(t testing.TB, text string) formula.Value {
	t.Helper()
	at := formula.Pos{Row: 0, Col: 4}
	f, err := formula.Parse(text, at, formula.Limits{})
	if err != nil {
		t.Fatalf("%.80s: %v", text, err)
	}
	return f.Eval(&formula.Env{Grid: g, Tab: 1, At: at})
}

// shown is a value as a read returns it, for messages.
func shown(v formula.Value) string {
	switch v.Kind() {
	case formula.KindError:
		return v.Err().String()
	case formula.KindNumber:
		return strconv.FormatFloat(v.Number(), 'g', -1, 64)
	case formula.KindText:
		return fmt.Sprintf("text of %d characters", v.TextLen())
	case formula.KindBool:
		return strconv.FormatBool(v.Bool())
	}
	return "blank"
}

func isValueError(v formula.Value) bool {
	return v.Kind() == formula.KindError && v.Err() == formula.ErrValue
}

// wildcardGuide returns the part of topic.sheet that explains wildcard
// patterns, and its sentences about where patterns are read and where the
// limit does not apply.
func wildcardGuide(t *testing.T) (block, readIn, exempt string) {
	t.Helper()
	md, _ := Topic("sheet")
	_, block, ok := strings.Cut(md, "\nWildcards: ")
	if !ok {
		t.Fatal("topic.sheet has no paragraph that starts with \"Wildcards: \"")
	}
	block, _, _ = strings.Cut(block, "\nA write over a limit")
	// item returns the list item of the block that starts with lead, as
	// one line.
	item := func(lead string) string {
		_, text, ok := strings.Cut(block, "\n- "+lead)
		if !ok {
			t.Fatalf("the wildcard part of topic.sheet has no item that starts with %q", lead)
		}
		text, _, _ = strings.Cut(text, "\n- ")
		return strings.Join(strings.Fields(text), " ")
	}
	readIn, exempt = item("A pattern is read in "), item("The limit does not apply to ")
	block = strings.Join(strings.Fields(block), " ")
	return block, readIn, exempt
}

var guideFunctionName = regexp.MustCompile("`([A-Z][A-Z0-9]*)`")

func guideFunctions(text string) []string {
	var names []string
	for _, m := range guideFunctionName.FindAllStringSubmatch(text, -1) {
		names = append(names, m[1])
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// TestSheetTopicWildcardLimit checks what the guide says about the length
// of a wildcard pattern against the formula engine: the limit itself, at
// its boundary; every function the limit applies to, found by trying each
// function of the library; and every exception the guide names.
func TestSheetTopicWildcardLimit(t *testing.T) {
	const limit = formula.MaxPatternLen
	long := strings.Repeat("a", limit+45)
	g := guideSheet{
		{Row: 0, Col: 0}: formula.TextValue(long),
		{Row: 1, Col: 0}: formula.NumberValue(1),
		{Row: 2, Col: 0}: formula.TextValue("b"),
		// B1 is as long as the longest pattern that fits, and B2 one longer.
		{Row: 0, Col: 1}: formula.TextValue(strings.Repeat("a", limit)),
		{Row: 1, Col: 1}: formula.TextValue(strings.Repeat("a", limit+1)),
	}
	block, readIn, exempt := wildcardGuide(t)

	// The guide names the limit the engine has, and the error.
	if want := fmt.Sprintf("at most %d characters", limit); !strings.Contains(block, want) {
		t.Errorf("the wildcard part of topic.sheet does not say %q", want)
	}
	if !strings.Contains(block, "`#VALUE!`") {
		t.Error("the wildcard part of topic.sheet does not name the error of a pattern that is too long")
	}
	md, _ := Topic("sheet")
	row := regexp.MustCompile("(?m)^\\| `#VALUE!` \\|.*$").FindString(md)
	for _, want := range []string{"wildcard pattern", "`FIND`", "`SEARCH`", "32,767"} {
		if !strings.Contains(row, want) {
			t.Errorf("the #VALUE! row of topic.sheet does not mention %s: %s", want, row)
		}
	}

	// The boundary: a pattern of the limit works, one more is #VALUE!.
	at, over := strconv.Itoa(limit), strconv.Itoa(limit+1)
	q := func(n string) string { return `REPT("?",` + n + `)` }
	for _, c := range []struct{ formula, want string }{
		{"=SEARCH(" + q(at) + ",B1)", "1"},
		{"=SEARCH(" + q(over) + ",B2)", "#VALUE!"},
		{"=COUNTIF(B1:B2," + q(at) + ")", "1"},
		{"=COUNTIF(B1:B2," + q(over) + ")", "#VALUE!"},
		{"=MATCH(" + q(at) + ",B1:B2,0)", "1"},
		{"=MATCH(" + q(over) + ",B1:B2,0)", "#VALUE!"},
		// The sign of criteria is not part of the pattern.
		{`=COUNTIF(B1:B2,"="&` + q(at) + ")", "1"},
		{`=COUNTIF(B1:B2,"<>"&` + q(at) + ")", "1"},
		{`=COUNTIF(B1:B2,"<>"&` + q(over) + ")", "#VALUE!"},
		// A "~" counts as a character of the pattern.
		{`=SEARCH(REPT("~?",` + strconv.Itoa((limit-1)/2) + `)&"?",REPT("?",` + over + `))`, "1"},
		{`=SEARCH(REPT("~?",` + strconv.Itoa((limit-1)/2) + `)&"??",REPT("?",` + over + `))`, "#VALUE!"},
		// The other causes of #VALUE! that the error table names.
		{`=SEARCH("z","abc")`, "#VALUE!"},
		{`=FIND("z","abc")`, "#VALUE!"},
		{`=LEN(REPT("a",32767))`, "32767"},
		{`=LEN(REPT("a",32768))`, "#VALUE!"},
	} {
		if got := shown(g.calc(t, c.formula)); got != c.want {
			t.Errorf("%s = %s, want %s", c.formula, got, c.want)
		}
	}

	// The functions the limit applies to are those where a pattern one
	// over the limit is #VALUE!, while the longest pattern that fits is
	// not, and neither is a text of the longer length without a wildcard.
	// Every function is tried with the pattern as each of its first
	// arguments and a few values as the others.
	fillers := []string{"A1:A3", "A1", "1", "0"}
	patterns := [3]string{q(at), q(over), `REPT("a",` + over + `)`}
	applies, groups := map[string]string{}, map[string]string{}
	for _, fn := range formula.Functions() {
		groups[fn.Name] = fn.Group
		for n := 1; n <= 4; n++ {
			combos := 1
			for i := 1; i < n; i++ {
				combos *= len(fillers)
			}
			for pos := 0; pos < n; pos++ {
				for c := 0; c < combos && applies[fn.Name] == ""; c++ {
					args, k := make([]string, n), c
					for i := range args {
						if i != pos {
							args[i], k = fillers[k%len(fillers)], k/len(fillers)
						}
					}
					var values [3]formula.Value
					for i, p := range patterns {
						args[pos] = p
						values[i] = g.calc(t, "="+fn.Name+"("+strings.Join(args, ",")+")")
					}
					if !isValueError(values[0]) && isValueError(values[1]) && !isValueError(values[2]) {
						args[pos] = "pattern"
						applies[fn.Name] = fn.Name + "(" + strings.Join(args, ",") + ")"
					}
				}
			}
		}
	}
	if len(groups) != 104 {
		t.Fatalf("the library has %d functions", len(groups))
	}
	var named []string
	for name, call := range applies {
		if groups[name] != "Conditional totals" {
			named = append(named, name)
			t.Logf("the limit applies to %s", call)
		}
	}
	slices.Sort(named)
	for name, group := range groups {
		if group == "Conditional totals" && applies[name] == "" {
			t.Errorf("the limit does not apply to the conditional total %s, and the guide says it applies to every one", name)
		}
	}
	if !strings.Contains(readIn, "every conditional total") {
		t.Errorf("topic.sheet does not say that a pattern is read in the criteria of every conditional total: %s", readIn)
	}
	if got := guideFunctions(readIn); !slices.Equal(got, named) {
		t.Errorf("topic.sheet says a pattern is read by %v beside the conditional totals, and the engine applies the limit to %v", got, named)
	}

	// The exceptions the guide names, each with a text one over the limit.
	exceptions := guideFunctions(exempt)
	for _, name := range exceptions {
		if applies[name] != "" {
			t.Errorf("topic.sheet names %s as free of the limit, and the limit applies to %s", name, applies[name])
		}
	}
	if !slices.Equal(exceptions, []string{"XLOOKUP"}) {
		t.Errorf("the exceptions topic.sheet names are %v: give each one a case below", exceptions)
	}
	for _, want := range []string{"without such a wildcard", "`<`", "`<=`", "`>`", "`>=`", "sorted match"} {
		if !strings.Contains(exempt, want) {
			t.Errorf("the exceptions of topic.sheet do not name %s: %s", want, exempt)
		}
	}
	stars := `REPT("*",` + over + `)`
	for _, c := range []struct{ what, formula, want string }{
		{"text without a wildcard", `=COUNTIF(B1:B2,REPT("a",` + over + `))`, "1"},
		{"text without a wildcard", `=SEARCH(REPT("a",` + over + `),B2)`, "1"},
		{"a wildcard after ~", `=SEARCH(REPT("~?",` + strconv.Itoa((limit+1)/2) + `),REPT("?",` + over + `))`, "1"},
		{"a wildcard after ~", `=COUNTIF(B1:B2,REPT("~*",` + over + `))`, "0"},
		{"criteria after <", `=COUNTIF(B1:B2,"<"&` + q(over) + ")", "0"},
		{"criteria after <=", `=COUNTIF(B1:B2,"<="&` + q(over) + ")", "0"},
		{"criteria after >", `=COUNTIF(B1:B2,">"&` + q(over) + ")", "2"},
		{"criteria after >=", `=COUNTIF(B1:B2,">="&` + q(over) + ")", "2"},
		{"the key of XLOOKUP", "=XLOOKUP(" + stars + `,B1:B2,B1:B2,"none")`, "text of 4 characters"},
		{"the key of a sorted MATCH", "=MATCH(" + stars + ",B1:B2,1)", "#N/A"},
		{"the key of a sorted VLOOKUP", "=VLOOKUP(" + stars + ",B1:B2,1,TRUE)", "#N/A"},
		{"the key of a sorted HLOOKUP", "=HLOOKUP(" + stars + ",B1:B2,1)", "#N/A"},
		// FIND reads no wildcards at all.
		{"the text FIND looks for", "=FIND(" + q(over) + ",B2&" + q(over) + ")", strconv.Itoa(limit + 2)},
	} {
		if got := shown(g.calc(t, c.formula)); got != c.want {
			t.Errorf("%s: %s = %s, want %s", c.what, c.formula, got, c.want)
		}
	}
}
