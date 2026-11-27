package mcpx

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// TestSheetGuideExplainsWildcardLimit takes the path of an agent that has
// only the tools and the guide: a formula whose wildcard pattern is one
// character too long is accepted and shows #VALUE!, and the guide the host
// serves says why. The refusals for a tab that does not exist are the ones
// the guide names.
func TestSheetGuideExplainsWildcardLimit(t *testing.T) {
	e := newEnv(t)
	cs := e.local
	limit := formula.MaxPatternLen
	search := func(n int) string { return fmt.Sprintf(`=SEARCH(REPT("?",%d),REPT("a",%d))`, n, n) }
	var out map[string]any
	text, bad := call(t, cs, "update_sheet", map[string]any{"slug": "patterns", "ops": []any{
		map[string]any{"op": "write", "tab": "Sheet1", "at": "A1", "rows": []any{
			[]any{search(limit)},
			[]any{search(limit + 1)},
			[]any{fmt.Sprintf(`=COUNTIF(A1:A2,REPT("*",%d))`, limit+1)},
			[]any{fmt.Sprintf(`=SEARCH(REPT("~?",%d),REPT("?",%d))`, (limit+1)/2, limit+1)},
			[]any{fmt.Sprintf(`=COUNTIF(A1:A2,">"&REPT("?",%d))`, limit+1)},
		}},
	}}, &out)
	if bad {
		t.Fatalf("the write is refused: %s", text)
	}
	if got := fmt.Sprint(out["errors"]); out["formula_errors"] != float64(2) || got != "[map[at:Sheet1!A2 error:#VALUE!] map[at:Sheet1!A3 error:#VALUE!]]" {
		t.Fatalf("formula_errors = %v, errors = %s; want #VALUE! in A2 and A3 alone", out["formula_errors"], got)
	}
	state := out["state"]
	var read map[string]any
	if text, bad := call(t, cs, "get_sheet", map[string]any{"slug": "patterns", "tab": "Sheet1"}, &read); bad {
		t.Fatalf("read: %s", text)
	}
	var got []string
	for _, row := range read["rows"].([]any) {
		cell := row.([]any)[0].(map[string]any)
		got = append(got, fmt.Sprint(cell["value"], " ", cell["error"]))
	}
	if want := []string{"1 <nil>", "<nil> #VALUE!", "<nil> #VALUE!", "1 <nil>", "0 <nil>"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the cells read %v, want %v", got, want)
	}

	guide := strings.Join(strings.Fields(sheetGuide(t, cs)), " ")
	for _, want := range []string{
		fmt.Sprintf("has at most %d characters", limit),
		"A longer one is `#VALUE!`",
		"in the text `SEARCH` looks for",
		"the criteria of every conditional total",
		"The limit does not apply to text without such a wildcard",
	} {
		if !strings.Contains(guide, want) {
			t.Errorf("the served topic.sheet does not say %q", want)
		}
	}

	for _, tc := range []struct {
		tool string
		args map[string]any
		want []string
	}{
		{"get_sheet", map[string]any{"slug": "patterns", "tab": "Gone"},
			[]string{`"category":"not_found"`, `"reason":"tab"`, "refusal.not_found"}},
		{"update_sheet", map[string]any{"slug": "patterns", "state": state, "ops": []any{
			map[string]any{"op": "write", "tab": "Gone", "at": "A1", "rows": []any{[]any{1}}}}},
			[]string{`"category":"conflict"`, `"reason":"tab_changed"`, "refusal.conflict"}},
		{"update_sheet", map[string]any{"slug": "patterns", "state": state, "ops": []any{
			map[string]any{"op": "write", "tab": "Sheet1", "at": "A9", "rows": []any{[]any{1}}},
			map[string]any{"op": "delete_rows", "tab": "Sheet1", "at": 5000, "count": 1}}},
			[]string{`"category":"invalid"`, `"reason":"at"`, "operation 2 (delete_rows)", "refusal.invalid"}},
	} {
		text, bad := call(t, cs, tc.tool, tc.args, nil)
		if !bad {
			t.Fatalf("%s %v was accepted: %s", tc.tool, tc.args, text)
		}
		for _, want := range tc.want {
			if !strings.Contains(text, want) {
				t.Errorf("the refusal of %s lacks %q: %s", tc.tool, want, text)
			}
		}
	}
}
