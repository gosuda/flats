package notebook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestPropertyDefinitions(t *testing.T) {
	t.Parallel()
	for _, typ := range []PropertyType{TitleType, TextType, NumberType, SelectType, MultiSelectType, DateType, CheckboxType, URLType} {
		t.Run(string(typ), func(t *testing.T) {
			p := PropertyDef{Name: "한글 e\u0301", Type: typ}
			if typ == NumberType {
				p.Format = PercentFormat
			}
			data, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			got, err := DecodePropertyDef(data)
			if err != nil || got != p {
				t.Fatalf("definition round trip = %#v, %v", got, err)
			}
		})
	}
	for _, raw := range []string{`{}`, `null`, `{"name":"A","type":"formula"}`, `{"name":"A","type":"text","format":"plain"}`, `{"name":"A","type":"text","format":""}`, `{"name":"A","type":"number","format":"money"}`, `{"name":"A","type":"number","format":null}`, `{"name":"A","type":"text","options":[]}`, `{"name":"A","type":"text","Name":"B"}`} {
		t.Run("invalid/"+raw, func(t *testing.T) {
			if _, err := DecodePropertyDef([]byte(raw)); err == nil {
				t.Fatalf("accepted %s", raw)
			}
		})
	}
}
func TestPlainNumberFormatCanonical(t *testing.T) {
	t.Parallel()
	omitted, err := DecodePropertyDef([]byte(`{"name":"Number","type":"number"}`))
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := DecodePropertyDef([]byte(`{"name":"Number","type":"number","format":"plain"}`))
	if err != nil {
		t.Fatal(err)
	}
	if omitted != explicit {
		t.Fatalf("plain formats differ: %#v != %#v", omitted, explicit)
	}
	first, err := CanonicalJSON(omitted)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CanonicalJSON(PropertyDef{Name: "Number", Type: NumberType, Format: PlainFormat})
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("plain canonical encodings differ: %s != %s: %v", first, second, err)
	}
}

func TestDecimalCanonicalization(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, input, want string }{
		{"zeros", "-0.000e100", "0"}, {"zero enormous exponent", "0e999999999999999999999999999999999", "0"}, {"plus and zeros", "+000123.45000", "123.45"}, {"positive exponent", "001.2300e2", "123"}, {"negative exponent", "123e-5", "0.00123"}, {"fraction", ".1200", "0.12"}, {"trailing dot", "12.", "12"}, {"positive integer padding", "1200e2", "120000"}, {"negative fraction", "-001.200e-3", "-0.0012"}, {"exact precision", "9007199254740993.000000000000000001", "9007199254740993.000000000000000001"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDecimal(tt.input)
			if err != nil || string(got) != tt.want {
				t.Fatalf("ParseDecimal(%q) = %q, %v", tt.input, got, err)
			}
			again, err := ParseDecimal(string(got))
			if err != nil || again != got {
				t.Fatalf("not idempotent: %q, %v", again, err)
			}
		})
	}
	for _, s := range []string{"", "NaN", "Inf", "Infinity", "1_000", "1,000", "0x10", "1 2", " 1", "1 ", "1e", "1e+", ".", "--1", "1e99999999999999999999", "1e9223372036854775807", "1e-9223372036854775808"} {
		t.Run("invalid/"+s, func(t *testing.T) {
			if _, err := ParseDecimal(s); err == nil {
				t.Fatalf("accepted %q", s)
			}
		})
	}
	for _, s := range []string{strings.Repeat("1", MaxCellCodePoints), "0." + strings.Repeat("0", MaxCellCodePoints-3) + "1", "1e8000", "1e-8000", "-" + strings.Repeat("1", MaxCellCodePoints), strings.Repeat("1", MaxCellCodePoints+1)} {
		if _, err := ParseDecimal(s); err != nil {
			t.Fatal(err)
		}
	}
}
func TestDecimalExpansionCap(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "positive integer at cap", input: "1e15999", want: "1" + strings.Repeat("0", 15999)},
		{name: "negative integer at cap", input: "-1e15998", want: "-1" + strings.Repeat("0", 15998)},
		{name: "positive fraction at cap", input: "1e-15998", want: "0." + strings.Repeat("0", 15997) + "1"},
		{name: "negative fraction at cap", input: "-1e-15997", want: "-0." + strings.Repeat("0", 15996) + "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseDecimal(test.input)
			if err != nil || string(got) != test.want {
				t.Fatalf("ParseDecimal(%q): length %d, error %v", test.input, len(got), err)
			}
		})
	}
	for _, input := range []string{"-1e15999", "1e16000", "1e-15999", "-1e-15998"} {
		t.Run("over cap/"+input, func(t *testing.T) {
			if _, err := ParseDecimal(input); err == nil {
				t.Fatalf("accepted expansion above cap: %s", input)
			}
		})
	}
}

func TestCalendarDates(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"0001-01-01", "9999-12-31", "2000-02-29", "2024-02-29T23:59:59"} {
		t.Run(s, func(t *testing.T) {
			if _, err := ParseCalendarDate(s); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, s := range []string{"0000-01-01", "10000-01-01", "1900-02-29", "2023-02-29", "2024-04-31", "2024-1-01", "2024-01-01T24:00:00", "2024-01-01T00:60:00", "2024-01-01T00:00:60", "2024-01-01T00:00:00Z", "2024-01-01T00:00:00+09:00", "2024-01-01T00:00:00.1", "2024-01-01T00:00"} {
		t.Run("invalid/"+s, func(t *testing.T) {
			if _, err := ParseCalendarDate(s); err == nil {
				t.Fatalf("accepted %q", s)
			}
		})
	}
}
func TestCellValuesRoundTrip(t *testing.T) {
	t.Parallel()
	for i, raw := range vocabFixtures(t, "cells") {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			value, err := DecodeCellValue(raw)
			if err != nil {
				t.Fatal(err)
			}
			data, err := EncodeCellValue(value)
			if err != nil {
				t.Fatal(err)
			}
			got, err := DecodeCellValue(data)
			if err != nil {
				t.Fatal(err)
			}
			again, err := EncodeCellValue(got)
			if err != nil || !bytes.Equal(data, again) {
				t.Fatalf("unstable cell: %s -> %s, %v", data, again, err)
			}
		})
	}
	input := CellValue{Type: MultiSelectType, Value: []ID{vocabID(OptionKind, 2), vocabID(OptionKind, 0), vocabID(OptionKind, 2)}}
	canonical, err := input.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	want := []ID{vocabID(OptionKind, 0), vocabID(OptionKind, 2)}
	if !reflect.DeepEqual(canonical.Value, want) {
		t.Fatalf("canonical option set = %#v", canonical.Value)
	}
	if input.Value.([]ID)[0] != vocabID(OptionKind, 2) {
		t.Fatal("canonicalizer mutated caller's set")
	}
}
func TestCellValuesRejectMalformed(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{}`, `null`, `{"type":"text"}`, `{"type":"text","value":null}`, `{"type":"number","value":1}`, `{"type":"number","value":"NaN"}`, `{"type":"number","value":" 1 "}`, `{"type":"date","value":"2025-02-29"}`, `{"type":"checkbox","value":1}`, `{"type":"checkbox","value":null}`, `{"type":"url","value":"mailto:a@example.com"}`, `{"type":"url","value":"javascript:alert(1)"}`, `{"type":"url","value":""}`, `{"type":"select","value":{"id":"` + string(vocabID(OptionKind, 0)) + `","inc":"7"}}`, `{"type":"select","value":"` + string(vocabID(PageKind, 0)) + `"}`, `{"type":"multi_select","value":null}`, `{"type":"multi_select","value":["` + string(vocabID(PropertyKind, 0)) + `"]}`, `{"type":"text","value":"a","extra":0}`, `{"type":"formula","value":"a"}`} {
		t.Run(raw, func(t *testing.T) {
			if _, err := DecodeCellValue([]byte(raw)); err == nil {
				t.Fatalf("accepted %s", raw)
			}
		})
	}
	for _, v := range []CellValue{{Type: NumberType, Value: 1.5}, {Type: CheckboxType, Value: "true"}, {Type: TextType, Value: string([]byte{0xff})}} {
		if _, err := EncodeCellValue(v); err == nil {
			t.Fatalf("encoded invalid value %#v", v)
		}
	}
}
func TestOperatorsClosedApplicabilityTable(t *testing.T) {
	t.Parallel()
	expected := map[PropertyType][]Operator{
		TitleType: {Contains, DoesNotContain, Is, IsNot, IsEmpty, IsNotEmpty}, TextType: {Contains, DoesNotContain, Is, IsNot, IsEmpty, IsNotEmpty}, URLType: {Contains, DoesNotContain, Is, IsNot, IsEmpty, IsNotEmpty}, NumberType: {Equal, NotEqual, Less, LessEqual, Greater, GreaterEqual, IsEmpty, IsNotEmpty}, SelectType: {Is, IsNot, IsAnyOf, IsEmpty, IsNotEmpty}, MultiSelectType: {Contains, DoesNotContain, IsEmpty, IsNotEmpty}, DateType: {Is, Before, After, OnOrBefore, OnOrAfter, IsEmpty, IsNotEmpty}, CheckboxType: {IsChecked, IsNotChecked},
	}
	all := []Operator{Contains, DoesNotContain, Is, IsNot, IsEmpty, IsNotEmpty, Equal, NotEqual, Less, LessEqual, Greater, GreaterEqual, IsAnyOf, Before, After, OnOrBefore, OnOrAfter, IsChecked, IsNotChecked, "==", "!=", "eq", "exists", "IS", ""}
	for typ, allowed := range expected {
		t.Run(string(typ), func(t *testing.T) {
			for _, op := range all {
				want := false
				for _, a := range allowed {
					if a == op {
						want = true
					}
				}
				err := ValidateOperator(typ, op)
				if (err == nil) != want {
					t.Fatalf("%s %q = %v, allowed=%v", typ, op, err, want)
				}
			}
		})
	}
}
func vocabView() ViewDef {
	return ViewDef{Name: "Table", Display: Display{Layout: "table", Visible: []Target{vocabTarget(PropertyKind, 0)}, Widths: []Width{{Property: vocabTarget(PropertyKind, 0), PX: 240}}}, Sort: []Sort{{Property: vocabTarget(PropertyKind, 0), Direction: "asc"}}, Filter: []Filter{{Property: vocabTarget(PropertyKind, 0), Op: Contains, Value: json.RawMessage(`"draft"`)}}}
}
func TestViewsRoundTrip(t *testing.T) {
	t.Parallel()
	table := vocabView()
	kanban := vocabView()
	group := vocabTarget(PropertyKind, 1)
	kanban.Name = "Board"
	kanban.Display = Display{Layout: "kanban", Visible: []Target{vocabTarget(PropertyKind, 0)}, GroupBy: &group, ShowEmpty: false}
	for _, v := range []ViewDef{table, kanban} {
		t.Run(v.Name, func(t *testing.T) {
			data, err := CanonicalJSON(v)
			if err != nil {
				t.Fatal(err)
			}
			got, err := DecodeViewDef(data)
			if err != nil {
				t.Fatal(err)
			}
			again, err := CanonicalJSON(got)
			if err != nil || !bytes.Equal(data, again) {
				t.Fatalf("view round trip %s -> %s (%v)", data, again, err)
			}
			schema := map[ID]PropertyDef{vocabID(PropertyKind, 0): {Type: TitleType}, vocabID(PropertyKind, 1): {Type: SelectType}}
			if err := got.ValidateSchema(schema); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestViewsRejectMalformed(t *testing.T) {
	t.Parallel()
	valid, err := json.Marshal(vocabView())
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ name, old, new string }{
		{"layout", `"table"`, `"gallery"`}, {"direction", `"asc"`, `"ascending"`}, {"width", `"px":240`, `"px":79`}, {"large width", `"px":240`, `"px":1201`}, {"noninteger width", `"px":240`, `"px":240.5`}, {"operator", `"contains"`, `"equals"`}, {"null value", `"draft"`, `null`}, {"missing value", `,"value":"draft"`, ``}, {"irrelevant value", `"contains"`, `"is empty"`}, {"wrong property", `"pr_`, `"p_`}, {"unknown field", `"name":"Table"`, `"name":"Table","html":"x"`}, {"missing name", `"name":"Table",`, ``}, {"wrong case", `"name":"Table"`, `"name":"Table","Name":"Oops"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := strings.ReplaceAll(string(valid), tt.old, tt.new)
			if raw == string(valid) {
				t.Fatalf("mutation did not match %s", tt.old)
			}
			if _, err := DecodeViewDef([]byte(raw)); err == nil {
				t.Fatalf("accepted %s", raw)
			}
		})
	}
	for _, raw := range []string{`{"layout":"table","visible":[],"widths":[],"show_empty":false}`, `{"layout":"kanban","visible":[],"group_by":{"tmp":"g"},"show_empty":false,"widths":[]}`, `{"layout":"kanban","visible":[],"group_by":{"tmp":"g"}}`} {
		var d Display
		if err := json.Unmarshal([]byte(raw), &d); err == nil {
			t.Fatalf("accepted display %s", raw)
		}
	}
	v := vocabView()
	if err := v.ValidateSchema(map[ID]PropertyDef{vocabID(PropertyKind, 0): {Type: NumberType}}); err == nil {
		t.Fatal("contains allowed for numeric property")
	}
	v.Filter[0] = Filter{Property: vocabTarget(PropertyKind, 0), Op: Equal, Value: json.RawMessage(`"NaN"`)}
	if err := v.ValidateSchema(map[ID]PropertyDef{vocabID(PropertyKind, 0): {Type: NumberType}}); err == nil {
		t.Fatal("nonfinite filter operand accepted")
	}
	v = vocabView()
	v.Display.Visible = append(v.Display.Visible, v.Display.Visible[0])
	if err := v.Validate(); err == nil {
		t.Fatal("duplicate visible accepted")
	}
}
func TestFilterOperandCodecs(t *testing.T) {
	t.Parallel()
	property := vocabTarget(PropertyKind, 0)
	for _, tt := range []struct {
		name  string
		op    Operator
		raw   string
		valid bool
	}{
		{"number", Equal, `"+001.2300e2"`, true}, {"nan", Equal, `"NaN"`, false}, {"number option", Less, `{"tmp":"option"}`, false}, {"option set", IsAnyOf, `[{"tmp":"option"}]`, true}, {"option set string", IsAnyOf, `"option"`, false}, {"contains set", Contains, `[{"tmp":"option"}]`, false}, {"bad date", Before, `"2024-02-30"`, false}, {"wall date", Before, `"2024-02-29T12:00:00"`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			value := Filter{Property: property, Op: tt.op, Value: json.RawMessage(tt.raw)}
			raw, err := json.Marshal(value)
			if (err == nil) != tt.valid {
				t.Fatalf("%s: %v", tt.raw, err)
			}
			if err != nil {
				return
			}
			var got Filter
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if tt.op == Equal && string(got.Value) != `"123"` {
				t.Fatalf("noncanonical numeric filter: %s", got.Value)
			}
		})
	}
	v := vocabView()
	v.Display.Visible = []Target{}
	if err := v.ValidateSchema(map[ID]PropertyDef{vocabID(PropertyKind, 0): {Type: TitleType}}); err == nil {
		t.Fatal("table hiding title accepted")
	}
}
func FuzzViewDef(f *testing.F) {
	data, err := json.Marshal(vocabView())
	if err != nil {
		f.Fatal(err)
	}
	f.Add(data)
	f.Add([]byte(`{"name":"Board","display":{"layout":"kanban","visible":[],"group_by":{"tmp":"group"},"show_empty":false},"sort":[],"filter":[]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		v, err := DecodeViewDef(data)
		if err != nil {
			return
		}
		first, err := CanonicalJSON(v)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeViewDef(first)
		if err != nil {
			t.Fatal(err)
		}
		second, err := CanonicalJSON(got)
		if err != nil || !bytes.Equal(first, second) {
			t.Fatalf("unstable view: %s -> %s (%v)", first, second, err)
		}
	})
}
func FuzzCellValue(f *testing.F) {
	for _, raw := range vocabFixtures(f, "cells") {
		f.Add([]byte(raw))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		v, err := DecodeCellValue(data)
		if err != nil {
			return
		}
		first, err := EncodeCellValue(v)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeCellValue(first)
		if err != nil {
			t.Fatal(err)
		}
		second, err := EncodeCellValue(got)
		if err != nil || !bytes.Equal(first, second) {
			t.Fatalf("unstable cell: %s -> %s (%v)", first, second, err)
		}
	})
}

func TestDecimalExponentCannotAmplify(t *testing.T) {
	for _, in := range []string{"1e10000000", "1e-10000000", "1e1000000000000", "-9.5e-20000", "1e16001"} {
		if d, err := ParseDecimal(in); err == nil {
			t.Fatalf("ParseDecimal(%q) = %d bytes, want refusal", in, len(d))
		}
	}
	for _, in := range []string{"1e8001", "1e-8001", "1e15999"} {
		if _, err := ParseDecimal(in); err != nil {
			t.Fatalf("ParseDecimal(%q): %v", in, err)
		}
	}
	retained := "1" + strings.Repeat("0", 9000)
	d, err := ParseDecimal(retained)
	if err != nil || string(d) != retained {
		t.Fatalf("retained 9,001-digit decimal did not round-trip: %v", err)
	}
	retained = "9" + strings.Repeat("0", 20000)
	if d, err := ParseDecimal(retained); err != nil || string(d) != retained {
		t.Fatalf("retained 20,001-digit decimal did not round-trip: %v", err)
	}
	var v Decimal
	if err := json.Unmarshal([]byte(`"1e10000000"`), &v); err == nil {
		t.Fatalf("decoding a filter operand accepted an amplifying exponent")
	}
}
