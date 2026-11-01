package notebook

import (
	"encoding/json"
	"math/big"
	"math/rand/v2"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

type dbQueryVector struct {
	Name        string      `json:"name"`
	Schema      Schema      `json:"schema"`
	View        ViewDef     `json:"view"`
	Rows        []RowValues `json:"rows"`
	Want        []ID        `json:"want"`
	FoldVersion string      `json:"fold_version"`
	FoldPairs   string      `json:"fold_pairs"`
}

func dbQueryVectors(t *testing.T) []dbQueryVector {
	t.Helper()
	data, err := os.ReadFile("testdata/query_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []dbQueryVector
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors) == 0 {
		t.Fatal("no query vectors")
	}
	return vectors
}

func TestDBQueryVectors(t *testing.T) {
	t.Parallel()
	for _, vector := range dbQueryVectors(t) {
		t.Run(vector.Name, func(t *testing.T) {
			t.Parallel()
			properties := map[ID]PropertyDef{}
			for i, p := range vector.Schema.Properties {
				properties[vector.Schema.PropertyIDs[i]] = p.Definition
			}
			if err := vector.View.ValidateSchema(properties); err != nil {
				t.Fatalf("invalid fixture view: %v", err)
			}
			before, err := json.Marshal(vector)
			if err != nil {
				t.Fatal(err)
			}
			if got := OrderRows(vector.Schema, vector.View, vector.Rows); !slices.Equal(got, vector.Want) {
				t.Fatalf("OrderRows = %v, want %v", got, vector.Want)
			}
			after, err := json.Marshal(vector)
			if err != nil || string(before) != string(after) {
				t.Fatalf("query changed its inputs: %v", err)
			}
		})
	}
}

func TestDBQuerySharedFoldData(t *testing.T) {
	t.Parallel()
	vector := dbQueryVectors(t)[0]
	if vector.FoldVersion != "15.0.0" || vector.FoldPairs != dbQueryFoldPairs {
		t.Fatal("Go and browser fold tables differ")
	}
	pairs := []rune(vector.FoldPairs)
	seen := map[rune]bool{}
	for i := 0; i < len(pairs); i += 2 {
		if seen[pairs[i]] || pairs[i] == pairs[i+1] {
			t.Fatalf("invalid fold pair at %d", i)
		}
		seen[pairs[i]] = true
		if got := dbQueryFold(string(pairs[i])); got != string(pairs[i+1]) {
			t.Fatalf("fold %U = %q, want %U", pairs[i], got, pairs[i+1])
		}
	}
	if len(seen) != 1454 {
		t.Fatalf("got %d Unicode 15.0.0 C/S mappings, want 1454", len(seen))
	}
	for _, tt := range []struct{ name, input, want string }{
		{"sigma", "Σςσ", "σσσ"},
		{"compatibility folds", "Kſµ", "ksμ"},
		{"no full folding", "Straße ẞ ﬀ", "straße ß ﬀ"},
		{"no locale folding", "Iİı", "iİı"},
		{"no normalization", "É E\u0301", "é e\u0301"},
		{"supplementary", "𐐀𐐨", "𐐨𐐨"},
		{"Cherokee folds to uppercase", "Ꭰꭰ", "ᎠᎠ"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := dbQueryFold(tt.input); got != tt.want {
				t.Fatalf("fold %q = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestDBQueryOperatorCoverage(t *testing.T) {
	t.Parallel()
	seen := map[PropertyType]map[Operator]bool{}
	for _, vector := range dbQueryVectors(t) {
		properties := map[ID]PropertyType{}
		for i, p := range vector.Schema.Properties {
			properties[vector.Schema.PropertyIDs[i]] = p.Definition.Type
		}
		for _, f := range vector.View.Filter {
			typ := properties[f.Property.Ref.ID]
			if seen[typ] == nil {
				seen[typ] = map[Operator]bool{}
			}
			seen[typ][f.Op] = true
		}
	}
	operators := []Operator{Contains, DoesNotContain, Is, IsNot, IsEmpty, IsNotEmpty,
		Equal, NotEqual, Less, LessEqual, Greater, GreaterEqual, IsAnyOf,
		Before, After, OnOrBefore, OnOrAfter, IsChecked, IsNotChecked}
	for _, typ := range []PropertyType{TitleType, TextType, NumberType, SelectType, MultiSelectType, DateType, CheckboxType, URLType} {
		for _, op := range operators {
			if ValidateOperator(typ, op) == nil && !seen[typ][op] {
				t.Errorf("missing shared vector for %s %s", typ, op)
			}
		}
	}
}

func TestDBQueryFilterApplicability(t *testing.T) {
	t.Parallel()
	for _, vector := range dbQueryVectors(t) {
		for _, f := range vector.View.Filter {
			for _, typ := range []PropertyType{TitleType, TextType, NumberType, SelectType, MultiSelectType, DateType, CheckboxType, URLType} {
				if got, want := validDBFilter(typ, f), f.ValidateFor(typ) == nil; got != want {
					t.Errorf("%s %s applicability = %v, want %v", typ, f.Op, got, want)
				}
			}
		}
	}
	property := dbQueryVectors(t)[0].View.Display.Visible[0]
	for _, tt := range []struct {
		name string
		typ  PropertyType
		op   Operator
		raw  string
	}{
		{"numeric text", NumberType, Equal, `"not a number"`},
		{"invalid date", DateType, Is, `"2024-02-30"`},
		{"null text", TextType, Is, `null`},
		{"empty option reference", SelectType, Is, `{}`},
		{"option string", SelectType, Is, `"not an option reference"`},
		{"null option set", SelectType, IsAnyOf, `null`},
		{"checkbox value", CheckboxType, IsChecked, `true`},
		{"checkbox empty operator", CheckboxType, IsEmpty, ``},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := Filter{Property: property, Op: tt.op, Value: json.RawMessage(tt.raw)}
			if validDBFilter(tt.typ, f) {
				t.Fatal("accepted invalid filter")
			}
		})
	}
}

func TestDBQueryPermutations(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(19, 61))
	for _, vector := range dbQueryVectors(t) {
		t.Run(vector.Name, func(t *testing.T) {
			rows := slices.Clone(vector.Rows)
			for range 8 {
				rng.Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })
				if got := OrderRows(vector.Schema, vector.View, rows); !slices.Equal(got, vector.Want) {
					t.Fatalf("input order affected result: got %v, want %v", got, vector.Want)
				}
			}
		})
	}
}

func TestDBQueryExactDecimalOrder(t *testing.T) {
	t.Parallel()
	vector := dbQueryVectors(t)[0]
	property := vector.Schema.PropertyIDs[0]
	vector.Schema.Properties[0].Definition.Type = NumberType
	vector.View.Sort = []Sort{{Property: Target{Ref: &Ref{ID: property, Inc: 1}}, Direction: "asc"}}
	rng := rand.New(rand.NewPCG(7, 83))
	rows := make([]RowValues, 200)
	exact := make(map[ID]*big.Rat, len(rows))
	for i := range rows {
		digits := make([]byte, 180)
		for j := range digits {
			digits[j] = byte('0' + rng.IntN(10))
		}
		input := string(digits[:90]) + "." + string(digits[90:])
		if i%2 == 0 {
			input = "-" + input
		}
		decimal, err := ParseDecimal(input)
		if err != nil {
			t.Fatal(err)
		}
		id, err := NewID(RowKind)
		if err != nil {
			t.Fatal(err)
		}
		var value any = decimal
		if i%3 == 0 {
			value = input
		}
		rows[i] = RowValues{Row: id, Position: "a0", Cells: map[ID]CellValue{property: {Type: NumberType, Value: value}}}
		exact[id], _ = new(big.Rat).SetString(input)
	}
	want := make([]ID, len(rows))
	for i, row := range rows {
		want[i] = row.Row
	}
	slices.SortFunc(want, func(a, b ID) int { return exact[a].Cmp(exact[b]) })
	if got := OrderRows(vector.Schema, vector.View, rows); !slices.Equal(got, want) {
		t.Fatal("decimal sort differs from exact rational order")
	}
}

func TestDBQueryDoesNotChangeMultiSelect(t *testing.T) {
	t.Parallel()
	for _, vector := range dbQueryVectors(t) {
		if vector.Name != "multi_select sort asc" {
			continue
		}
		property := vector.Schema.PropertyIDs[1]
		before := make([][]ID, len(vector.Rows))
		for i, row := range vector.Rows {
			if value, ok := row.Cells[property].Value.([]ID); ok {
				before[i] = slices.Clone(value)
			}
		}
		_ = OrderRows(vector.Schema, vector.View, vector.Rows)
		for i, row := range vector.Rows {
			if value, ok := row.Cells[property].Value.([]ID); ok && !reflect.DeepEqual(value, before[i]) {
				t.Fatal("query reordered stored option ids")
			}
		}
		return
	}
	t.Fatal("missing multi-select vector")
}

func TestDBQueryLongExactValues(t *testing.T) {
	t.Parallel()
	for _, vector := range dbQueryVectors(t) {
		if vector.Name != "number sort asc" {
			continue
		}
		property := vector.Schema.PropertyIDs[1]
		values := []string{"0." + strings.Repeat("0", 7996) + "1", "0." + strings.Repeat("0", 7996) + "2", strings.Repeat("9", 8000)}
		rows := vector.Rows[:3]
		for i := range rows {
			rows[i].Cells = map[ID]CellValue{property: {Type: NumberType, Value: Decimal(values[i])}}
		}
		want := []ID{rows[0].Row, rows[1].Row, rows[2].Row}
		if got := OrderRows(vector.Schema, vector.View, rows); !slices.Equal(got, want) {
			t.Fatalf("long decimal order = %v, want %v", got, want)
		}
		return
	}
	t.Fatal("missing number vector")
}

func TestDBQueryDirectValues(t *testing.T) {
	t.Parallel()
	for _, vector := range dbQueryVectors(t) {
		switch vector.Name {
		case "number sort asc":
			t.Run("decimal representations", func(t *testing.T) {
				property := vector.Schema.PropertyIDs[1]
				rows := slices.Clone(vector.Rows[:4])
				values := []any{Decimal("+0002.000e0"), "2", Decimal("-0"), "0"}
				for i := range rows {
					rows[i].Position = "a" + string(rune('A'+i))
					rows[i].Cells = map[ID]CellValue{property: {Type: NumberType, Value: values[i]}}
				}
				want := []ID{rows[2].Row, rows[3].Row, rows[0].Row, rows[1].Row}
				if got := OrderRows(vector.Schema, vector.View, rows); !slices.Equal(got, want) {
					t.Fatalf("direct decimal sort = %v, want %v", got, want)
				}
			})
		case "multi_select sort asc":
			t.Run("option sets", func(t *testing.T) {
				property := vector.Schema.PropertyIDs[1]
				options := vector.Schema.Properties[1].OptionOrder
				rows := slices.Clone(vector.Rows[:3])
				values := [][]ID{{options[0], options[0]}, {options[0]}, {options[0], options[1]}}
				for i := range rows {
					rows[i].Position = "a" + string(rune('A'+i))
					rows[i].Cells = map[ID]CellValue{property: {Type: MultiSelectType, Value: values[i]}}
				}
				want := []ID{rows[0].Row, rows[1].Row, rows[2].Row}
				if got := OrderRows(vector.Schema, vector.View, rows); !slices.Equal(got, want) {
					t.Fatalf("direct option-set sort = %v, want %v", got, want)
				}
				if len(values[0]) != 2 || values[0][0] != values[0][1] {
					t.Fatal("query changed the caller's option set")
				}
			})
		}
	}
}
