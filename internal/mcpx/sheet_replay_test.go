package mcpx

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestSheetReplayAcrossToolAndAPI sends one call with one op_id through
// the management API and through the tool, in both orders and in two
// spellings. The tool's arguments name the sheet in slug and the API names
// it in its path: both are the same request. The second send answers with
// the stored result and its revision, and writes nothing.
func TestSheetReplayAcrossToolAndAPI(t *testing.T) {
	e := newEnv(t)
	cs := connect(t, e.localURL)
	call(t, cs, "create_flat", map[string]any{"slug": "budget", "type": "sheet"}, nil)
	type reply struct {
		revision float64
		state    string
		replay   bool
	}
	viaAPI := func(t *testing.T, body string) reply {
		t.Helper()
		resp, err := http.Post(e.management.URL+"/api/flats/budget/sheet", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out struct {
			Result struct {
				State  string `json:"state"`
				Replay bool   `json:"replay"`
			} `json:"result"`
			Revision float64 `json:"revision"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %s = %d %s", body, resp.StatusCode, raw)
		}
		return reply{out.Revision, out.Result.State, out.Result.Replay}
	}
	viaTool := func(t *testing.T, args string) reply {
		t.Helper()
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "update_sheet", Arguments: json.RawMessage(args)})
		if err != nil {
			t.Fatal(err)
		}
		var text strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				text.WriteString(tc.Text)
			}
		}
		raw, _ := json.Marshal(res.StructuredContent)
		var out struct {
			State    string  `json:"state"`
			Revision float64 `json:"revision"`
			Replay   bool    `json:"replay"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || res.IsError {
			t.Fatalf("update_sheet %s = %s", args, text.String())
		}
		if out.Replay != strings.Contains(text.String(), "already applied") {
			t.Fatalf("update_sheet %s: replay %v, text %q", args, out.Replay, text.String())
		}
		return reply{out.Revision, out.State, out.Replay}
	}
	count := func(t *testing.T) (tabs, revisions int) {
		t.Helper()
		var sheet map[string]any
		if _, bad := call(t, cs, "get_sheet", map[string]any{"slug": "budget"}, &sheet); bad {
			t.Fatal("get_sheet failed")
		}
		page, err := e.svc.ListRevisions(t.Context(), "budget", 0, 50)
		if err != nil {
			t.Fatal(err)
		}
		list, _ := sheet["tabs"].([]any)
		return len(list), len(page.Revisions)
	}
	for _, tt := range []struct {
		name        string
		first, then func(*testing.T, string) reply
		sent, again string
	}{
		{"API, then the tool", viaAPI, viaTool,
			`{"op_id":"retry-1","ops":[{"op":"add_tab","name":"Extra"}]}`,
			`{"slug":"budget","op_id":"retry-1","ops":[{"op":"add_tab","name":"Extra"}]}`},
		{"the tool, then API", viaTool, viaAPI,
			`{"slug":"budget","op_id":"retry-2","ops":[{"op":"add_tab","name":"More"}]}`,
			`{"op_id":"retry-2","ops":[{"op":"add_tab","name":"More"}]}`},
		// The second send spells the same call another way: the order of
		// its fields, white space, a number and a formula object.
		{"API, then the tool in another spelling", viaAPI, viaTool,
			`{"state":"$state","op_id":"retry-3","ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,"=A1+1"]]}]}`,
			`{ "ops": [ {"rows":[[1.0,{"formula":"=A1+1","value":2}]],"at":"A1","tab":"Sheet1","op":"write"} ], "op_id":"retry-3", "slug":"budget", "state":"$state" }`},
		{"the tool, then API in another spelling", viaTool, viaAPI,
			`{"slug":"budget","state":"$state","op_id":"retry-4","ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[[2,"=B1+1"]]}]}`,
			`{ "ops": [ {"rows":[[2.0,{"formula":"=B1+1","value":3}]],"at":"B1","tab":"Sheet1","op":"write"} ], "op_id":"retry-4", "state":"$state" }`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var sheet map[string]any
			call(t, cs, "get_sheet", map[string]any{"slug": "budget"}, &sheet)
			state, _ := sheet["state"].(string)
			tabs, revisions := count(t)
			added := strings.Count(tt.sent, "add_tab")
			first := tt.first(t, strings.ReplaceAll(tt.sent, "$state", state))
			if first.replay || first.revision == 0 || first.state == state {
				t.Fatalf("the first send = %+v", first)
			}
			if n, r := count(t); n != tabs+added || r != revisions+1 {
				t.Fatalf("after the first send: %d tabs and %d revisions, want %d and %d", n, r, tabs+added, revisions+1)
			}
			second := tt.then(t, strings.ReplaceAll(tt.again, "$state", state))
			if !second.replay || second.revision != first.revision || second.state != first.state {
				t.Fatalf("the second send = %+v, want the stored result of %+v", second, first)
			}
			if n, r := count(t); n != tabs+added || r != revisions+1 {
				t.Fatalf("the second send wrote: %d tabs and %d revisions, want %d and %d", n, r, tabs+added, revisions+1)
			}
		})
	}
	// The same id with another request stays a conflict on the other
	// surface too.
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "update_sheet", Arguments: json.RawMessage(`{"slug":"budget","op_id":"retry-1","ops":[{"op":"add_tab","name":"Other"}]}`)})
	if err != nil || !res.IsError {
		t.Fatalf("another request under a stored op_id: %v, error %v", err, res != nil && res.IsError)
	}
	if raw, _ := json.Marshal(res.Content); !strings.Contains(string(raw), "op_id_reuse") {
		t.Fatalf("another request under a stored op_id answered %s", raw)
	}
}
