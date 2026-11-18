package formula

import (
	"strconv"
	"strings"
	"testing"
)

// benchGrid is one tab held as a slice, the cheapest Grid there is, so the
// benchmarks measure the evaluator.
type benchGrid struct {
	rows, cols int32
	cells      []Value
}

func (g *benchGrid) Tab(string) (TabRef, bool)  { return 0, true }
func (g *benchGrid) Dims(TabRef) (int32, int32) { return g.rows, g.cols }
func (g *benchGrid) Value(_ TabRef, p Pos) Value {
	return g.cells[int(p.Row)*int(g.cols)+int(p.Col)]
}
func (g *benchGrid) Filled(_ TabRef, p Pos) bool {
	return !g.cells[int(p.Row)*int(g.cols)+int(p.Col)].IsBlank()
}

// newBenchGrid builds the shape of the reference sheet's large tab: a
// header row, then rows of an id, a name, a quantity, a price and a flag.
func newBenchGrid(rows int32) *benchGrid {
	g := &benchGrid{rows: rows, cols: 8, cells: make([]Value, int(rows)*8)}
	set := func(r, c int32, v Value) { g.cells[int(r)*8+int(c)] = v }
	for c, name := range []string{"Id", "Item", "Qty", "Price", "Done"} {
		set(0, int32(c), TextValue(name))
	}
	for r := int32(1); r < rows; r++ {
		set(r, 0, NumberValue(float64(r)))
		set(r, 1, TextValue("item "+strconv.Itoa(int(r%97))))
		set(r, 2, NumberValue(float64(r%13)+0.5))
		set(r, 3, NumberValue(float64(r%7)*1.25+3))
		set(r, 4, BoolValue(r%3 == 0))
	}
	return g
}

func BenchmarkParse(b *testing.B) {
	for name, text := range map[string]string{
		"row":    "=C5*D5",
		"total":  "=SUM(C2:C20001)",
		"nested": `=IF(SUM(C2:C9)>100,VLOOKUP(A2,Costs!A:D,3,FALSE)*1.1,"n/a "&TEXT(TODAY(),"yyyy-mm-dd"))`,
	} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := Parse(text, Pos{Row: 4, Col: 4}, Limits{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkParseStored(b *testing.B) {
	f, _ := Parse(`=IF(SUM(C2:C9)>100,VLOOKUP(A2,Costs!A:D,3,FALSE)*1.1,"n/a")`, Pos{Row: 4, Col: 4}, Limits{})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := ParseStored(f.Stored()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEval(b *testing.B) {
	g := newBenchGrid(20001)
	for _, c := range []struct{ name, text string }{
		{"row formula", "=C5*D5"},
		{"row formula with a check", `=IF(E5,ROUND(C5*D5,2),"")`},
		{"row sum", "=SUM(C5:D5)"},
		{"SUM of 20,000", "=SUM(C2:C20001)"},
		{"SUM of a whole column", "=SUM(C:C)"},
		{"AVERAGE of 20,000", "=AVERAGE(C2:C20001)"},
		{"COUNTIF of 20,000", `=COUNTIF(C2:C20001,">5")`},
		{"SUMIFS of 20,000", `=SUMIFS(D2:D20001,C2:C20001,">5",B2:B20001,"item 1*")`},
		{"MEDIAN of 20,000", "=MEDIAN(C2:C20001)"},
		{"SUMPRODUCT of 20,000", "=SUMPRODUCT(C2:C20001,D2:D20001)"},
		{"VLOOKUP sorted in 20,000", "=VLOOKUP(15000,A2:D20001,4)"},
		{"VLOOKUP exact in 20,000", "=VLOOKUP(15000,A2:D20001,4,FALSE)"},
		{"MATCH exact in 20,000", "=MATCH(15000,A2:A20001,0)"},
		{"text", `=UPPER(LEFT(B5,4))&"-"&TEXT(D5,"0.00")`},
	} {
		at := Pos{Row: 4, Col: 6}
		f, err := Parse(c.text, at, Limits{})
		if err != nil {
			b.Fatal(err)
		}
		budget := NewBudget(nil)
		env := &Env{Grid: g, At: at, Today: 46339, Budget: budget}
		if v := f.Eval(env); v.Kind() == KindError {
			b.Fatalf("%s = %v", c.text, v)
		}
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				f.Eval(env)
			}
		})
	}
}

// BenchmarkParts measures formulas that read no cell and go through no
// text of a cell: numbers written in the formula, operators and calls, as
// many as a formula holds. Each reports what a unit of its counted work
// costs (technical design, section 10.7).
func BenchmarkParts(b *testing.B) {
	g := newBenchGrid(2)
	for _, c := range []struct{ name, text string }{
		{"SUM of numbers", repeated("=SUM(", "0", ",", ")")},
		{"additions", repeated("=", "0", "+", "")},
		{"comparisons", repeated("=SUM(", "1=1", ",", ")")},
		{"MEDIAN of numbers", repeated("=MEDIAN(", "0", ",", ")")},
		{"STDEV of numbers", repeated("=STDEV(", "0", ",", ")")},
		{"AND of numbers", repeated("=AND(", "1", ",", ")")},
		{"COUNTA of numbers", repeated("=COUNTA(", "0", ",", ")")},
		{"IFS", repeated("=IFS(", "FALSE,0", ",", ",TRUE,1)")},
		{"SWITCH", repeated("=SWITCH(1,", "2,0", ",", ",3)")},
		{"percent signs", "=1" + strings.Repeat("%", MaxFormulaChars-2)},
		{"EXP", repeated("=", "EXP(1)", "+", "")},
		{"POWER", repeated("=", "POWER(1.1,2.5)", "+", "")},
		{"ROUND", repeated("=", "ROUND(1.2345,2)", "+", "")},
		{"DATE", repeated("=", "DATE(2026,1,31)", "+", "")},
		{"DATEDIF", repeated("=", `DATEDIF(1,46000,"md")`, "+", "")},
		{"a number as text", repeated("=", "LEN(1/3)", "+", "")},
		{"CONCAT of numbers", repeated("=LEN(CONCAT(", "1/8", ",", "))")},
		{"TEXT", repeated("=", `LEN(TEXT(1.5,"0.00"))`, "+", "")},
		{"VALUE", repeated("=", `VALUE("1.5")`, "+", "")},
		{"SUMPRODUCT of numbers", repeated("=", "SUMPRODUCT(2,3)", "+", "")},
		{"COUNTIF of a number", repeated("=", "COUNTIF(1,1)", "+", "")},
		{"MATCH of a number", repeated("=", "MATCH(1,1,0)", "+", "")},
		{"LARGE of a number", repeated("=", "LARGE(1,1)", "+", "")},
		{"MEDIAN of a number", repeated("=", "MEDIAN(1)", "+", "")},
		{"SUM of ranges beside the tab", repeated("=SUM(", "J1:K2", ",", ")")},
		{"MEDIAN of ranges beside the tab", repeated("=MEDIAN(1,", "J1:K2", ",", ")")},
	} {
		f, err := Parse(c.text, Pos{}, Limits{})
		if err != nil {
			b.Fatal(c.name, err)
		}
		budget := NewBudget(nil)
		env := &Env{Grid: g, Today: 46339, Budget: budget}
		if v := f.Eval(env); v.Kind() == KindError {
			b.Fatalf("%s = %v", c.name, v)
		}
		units := budget.Used()
		b.Run(c.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				f.Eval(env)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(units), "ns/unit")
			b.ReportMetric(float64(units), "units")
		})
	}
}

func BenchmarkApply(b *testing.B) {
	at := Pos{Row: 4, Col: 4}
	row, _ := Parse("=C5*D5", at, Limits{})
	total, _ := Parse("=SUM(C2:C20001)/$B$1", Pos{Row: 0, Col: 6}, Limits{})
	insert := Transform{Kind: InsertRows, Tab: "Sheet1", At: 0, Count: 1}
	b.Run("a row formula keeps its stored form", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, _, changed := row.Apply("Sheet1", at, insert); changed {
				b.Fatal("changed")
			}
		}
	})
	b.Run("a range grows", func(b *testing.B) {
		b.ReportAllocs()
		grow := Transform{Kind: InsertRows, Tab: "Sheet1", At: 10, Count: 1}
		for i := 0; i < b.N; i++ {
			if _, _, changed := total.Apply("Sheet1", Pos{Row: 0, Col: 6}, grow); !changed {
				b.Fatal("not changed")
			}
		}
	})
}

func BenchmarkShow(b *testing.B) {
	currency := NumberFormat{Kind: NumberCurrency, Symbol: "$", Decimals: 2}
	date := NumberFormat{Kind: NumberDate}
	var buf [64]byte
	for _, c := range []struct {
		name string
		v    Value
		nf   NumberFormat
	}{
		{"whole number in General", NumberValue(1200), NumberFormat{}},
		{"fraction in General", NumberValue(1234.5678), NumberFormat{}},
		{"currency", NumberValue(1234.5678), currency},
		{"date", NumberValue(46339), date},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				AppendShow(buf[:0], c.v, ContentNumber, c.nf)
			}
		})
	}
}

func BenchmarkParseTyped(b *testing.B) {
	for _, text := range []string{"1200.50", "Rent for March", "2026-03-15", "$1,234.50"} {
		b.Run(text, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				ParseTyped(text)
			}
		})
	}
}

// BenchmarkFold measures case folding, the cost of comparing texts without
// regard to capitals: one code point, a tab name, and two long texts that
// are equal up to their last character. Letters outside ASCII that have
// capitals are the case to watch.
func BenchmarkFold(b *testing.B) {
	for _, c := range []struct {
		name  string
		runes []rune
	}{
		{"one code point/Greek and Cyrillic capitals", []rune("ΑΒΓΔΣΩЖЩЯÉÑǅ")},
		{"one code point/Greek and Cyrillic small letters", []rune("αβγδσςωжщяéñ")},
		{"one code point/Hangul", []rune("가나다라마바사아자차카타")},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			var sum rune
			for i := 0; i < b.N; i++ {
				sum += foldRune(c.runes[i%len(c.runes)])
			}
			_ = sum
		})
	}
	b.Run("FoldName/a Greek name of 24 letters", func(b *testing.B) {
		b.ReportAllocs()
		name := strings.Repeat("ΣΑΛΕΣ ", 4)
		for i := 0; i < b.N; i++ {
			FoldName(name)
		}
	})
	const n = MaxTextLen - 1
	for _, c := range []struct{ name, a, b string }{
		{"compare/ASCII, capitals against small", strings.Repeat("A", n) + "y", strings.Repeat("a", n) + "z"},
		{"compare/sigma, capitals against small", strings.Repeat("Σ", n) + "a", strings.Repeat("σ", n) + "b"},
		{"compare/Cyrillic, capitals against small", strings.Repeat("ЖЩ", n/2) + "a", strings.Repeat("жщ", n/2) + "b"},
		{"compare/Greek, small against small", strings.Repeat("σ", n) + "a", strings.Repeat("σ", n) + "b"},
		{"compare/Hangul", strings.Repeat("가", n) + "a", strings.Repeat("가", n) + "b"},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(c.a)))
			for i := 0; i < b.N; i++ {
				if compareFold(c.a, c.b) >= 0 {
					b.Fatal("the order is wrong")
				}
			}
		})
	}
	// A pattern with "?" is matched with bits. Each code point of the
	// text is folded once.
	for _, c := range []struct{ name, pattern, text string }{
		{"match/ASCII text", "*a?z*", strings.Repeat("A", n)},
		{"match/sigma text", "*σ?ω*", strings.Repeat("Σ", n)},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(c.text)))
			ev := &evaluator{env: &Env{}}
			w, _ := ev.wildcardOf(c.pattern)
			for i := 0; i < b.N; i++ {
				if w.match(ev, c.text) {
					b.Fatal("the pattern matches")
				}
			}
		})
	}
}

// BenchmarkFoldTable measures what loading the package costs. The table of
// folds is built once then.
func BenchmarkFoldTable(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if t := newFoldTable(foldPairs[:]); len(t.pages) == 0 {
			b.Fatal("an empty table")
		}
	}
}

// BenchmarkTextToNumber measures reading a long text as a number, as an
// operand of arithmetic is read: digits, digits that are no number after
// all, and words, which are no date either.
func BenchmarkTextToNumber(b *testing.B) {
	const n = MaxTextLen
	for _, c := range []struct{ name, text string }{
		{"zeros", strings.Repeat("0", n)},
		{"nines", strings.Repeat("9", n)},
		{"decimals", "0." + strings.Repeat("123456789", n/9-1)},
		{"digits and a letter", strings.Repeat("7", n-1) + "x"},
		{"words", strings.Repeat("a ", n/2)},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(c.text)))
			for i := 0; i < b.N; i++ {
				textToNumber(c.text)
			}
		})
	}
}
