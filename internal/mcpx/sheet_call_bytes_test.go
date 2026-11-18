package mcpx

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestSheetToolReadsItsArguments sends update_sheet arguments whose bytes
// name a field twice or in another case. The arguments reach the sheet as
// the client wrote them: a field twice is refused, and is not settled by
// the one of the two that a decoded copy of the arguments keeps.
func TestSheetToolReadsItsArguments(t *testing.T) {
	e := newEnv(t)
	cs := connect(t, e.localURL)
	call(t, cs, "create_flat", map[string]any{"slug": "budget", "type": "sheet"}, nil)
	add := `{"op":"add_tab","name":"Extra"}`
	tabs := func() int {
		var out map[string]any
		if _, bad := call(t, cs, "get_sheet", map[string]any{"slug": "budget"}, &out); bad {
			t.Fatal("get_sheet failed")
		}
		list, _ := out["tabs"].([]any)
		return len(list)
	}
	for _, args := range []string{
		`{"slug":"budget","ops":[],"ops":[` + add + `]}`,
		`{"slug":"budget","ops":[` + add + `],"ops":[]}`,
		`{"slug":"other","slug":"budget","ops":[` + add + `]}`,
		`{"slug":"budget","ops":[{"op":"add_tab","name":"Extra","name":"More"}]}`,
		`{"slug":"budget","ops":[{"op":"add_tab","name":"Extra"},{"op":"add_tab","name":"More","op":"add_tab"}]}`,
	} {
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "update_sheet", Arguments: json.RawMessage(args)})
		if err != nil {
			t.Fatalf("%s: protocol error: %v", args, err)
		}
		var text strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				text.WriteString(tc.Text)
			}
		}
		if !res.IsError || !strings.Contains(text.String(), `"category":"invalid"`) {
			t.Errorf("%s: answered %q (error %v), want an invalid call", args, text.String(), res.IsError)
		}
		if tabs() != 1 {
			t.Fatalf("%s changed the sheet", args)
		}
	}
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "update_sheet", Arguments: json.RawMessage(`{"slug":"budget","ops":[` + add + `]}`)})
	if err != nil || res.IsError || tabs() != 2 {
		t.Fatalf("a well-formed call: %v, error %v, %d tabs", err, res != nil && res.IsError, tabs())
	}
}
