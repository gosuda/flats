package formula

import (
	"slices"
	"testing"
)

// Tabs reports the name of every reference written with one, as the tab is
// named and not as the formula spells it, also for a reference that lies
// outside the grid at the formula's cell and for one in an argument that
// is never taken.
func TestTabs(t *testing.T) {
	for text, want := range map[string][]string{
		"=A1+B2*2":                            nil,
		"=A1+Data!B2+'My tab'!C3:D4":          {"Data", "My tab"},
		"=IF(TRUE,1,Other!A1)+'It''s'!A:A":    {"Other", "It's"},
		"=SUM(Data!1:3,data!A1,DATA!$B$2)":    {"Data", "data", "DATA"},
		"=Data!A1+Data!B1":                    {"Data", "Data"},
		`="Data!A1"&'Sheet 2'!A1&"'x'!B2"&A1`: {"Sheet 2"},
	} {
		f, err := Parse(text, Pos{Row: 4, Col: 2}, Limits{})
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		var got []string
		f.Tabs(func(name string) { got = append(got, name) })
		if !slices.Equal(got, want) {
			t.Errorf("%s names %q, want %q", text, got, want)
		}
	}
	// A reference three rows up lies outside the grid for the first row.
	// Refs passes over it there. Tabs asks for no cell and reports it,
	// because a rename rewrites it wherever the formula lies.
	f, err := Parse("=Data!A1", Pos{Row: 3}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	refs := 0
	f.Refs(Pos{}, func(Ref) { refs++ })
	var names []string
	f.Tabs(func(name string) { names = append(names, name) })
	if refs != 0 || !slices.Equal(names, []string{"Data"}) {
		t.Errorf("=Data!A1 of row 4, read in row 1: Refs reports %d references and Tabs %q", refs, names)
	}
}
