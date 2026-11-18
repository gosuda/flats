package sheet

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestLogicalFunctionsNeedALogicalValue writes AND, OR and XOR over cells
// that hold no logical value, text and blank cells alone, and reads them
// back as get_sheet returns them. Without a logical value among the
// arguments the result is #VALUE!, also inside IF; a real logical value in
// a range or a direct argument gives a boolean.
func TestLogicalFunctionsNeedALogicalValue(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["pending","x",true,"=\"\""],[null,"y",null,"=H9"]]}]}`)
	cases := []struct{ formula, want string }{
		{`=AND(A1:A2)`, `{"error":"#VALUE!"}`},
		{`=OR(A1:A2)`, `{"error":"#VALUE!"}`},
		{`=XOR(A1:A2)`, `{"error":"#VALUE!"}`},
		{`=AND(A2:A3)`, `{"error":"#VALUE!"}`},
		{`=OR(B1:B2)`, `{"error":"#VALUE!"}`},
		{`=XOR(A1:B2)`, `{"error":"#VALUE!"}`},
		{`=AND(D1:D2)`, `{"error":"#VALUE!"}`},
		{`=AND(A1)`, `{"error":"#VALUE!"}`},
		{`=OR(A2)`, `{"error":"#VALUE!"}`},
		{`=IF(AND(A1:A2),"ready","waiting")`, `{"error":"#VALUE!"}`},
		{`=IFERROR(XOR(A1:A2),"none")`, `{"value":"none"}`},
		{`=AND(A1:A2,TRUE)`, `{"value":true}`},
		{`=OR(A1:A2,C1)`, `{"value":true}`},
		{`=XOR(A1:C1)`, `{"value":true}`},
		{`=AND(A1:A2,"TRUE")`, `{"value":true}`},
		{`=OR(A1:A2,0)`, `{"value":false}`},
	}
	rows := make([]string, len(cases))
	for i, c := range cases {
		rows[i] = jsonf("[%q]", c.formula)
	}
	e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"F1","rows":[%s]}]}`, strings.Join(rows, ",")))
	out := e.read(ReadQuery{Tab: "Sheet1", Range: jsonf("F1:F%d", len(cases))})
	got, _ := out["rows"].([]any)
	if len(got) != len(cases) {
		t.Fatalf("the read has %d rows", len(got))
	}
	for i, c := range cases {
		cell, _ := got[i].([]any)[0].(map[string]any)
		delete(cell, "formula")
		data, _ := json.Marshal(cell)
		if string(data) != c.want {
			t.Errorf("%s gives %s, want %s", c.formula, data, c.want)
		}
	}
}
