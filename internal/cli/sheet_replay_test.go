package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/mcpx"
	"github.com/gosuda/flats/internal/sheet"
	"github.com/gosuda/flats/internal/store"
)

// TestSheetApplyReplaysAcrossSurfaces sends one call with one op_id
// through `flats sheet apply`, the management API and the update_sheet
// tool, each of them first once. The command builds its body from the file
// and its flags, the tool names the sheet in slug, and the API takes the
// call as written: all three are one request, so the sends after the first
// answer with the stored result and write nothing.
func TestSheetApplyReplaysAcrossSurfaces(t *testing.T) {
	srv, svc, cs := sheetSurfaces(t)
	if _, err := svc.CreateSheet(t.Context(), "budget", "Budget", core.ViaMCP); err != nil {
		t.Fatal(err)
	}

	type reply struct {
		Revision int  `json:"revision"`
		Replay   bool `json:"replay"`
	}
	// Each surface sends the write of row to the first cells of that row
	// under the id, with the message "Budget" and the state.
	write := func(row string) string {
		return `{"op":"write","tab":"Sheet1","at":"A` + row + `","rows":[["Venue",1200,"=B` + row + `*2"]]}`
	}
	surfaces := map[string]func(t *testing.T, id, state, row string) reply{
		"the command": func(t *testing.T, id, state, row string) reply {
			// The file holds neither id, state nor message: the flags do.
			ops := sheetOps(t, "{\n  \"ops\": [\n    "+write(row)+"\n  ]\n}\n")
			r := run(t, srv.URL, "", "--json", "sheet", "apply", "budget", ops, "--op-id", id, "--state", state, "--message", "Budget")
			var out struct {
				Result   reply `json:"result"`
				Revision int   `json:"revision"`
			}
			if err := json.Unmarshal([]byte(r.stdout), &out); err != nil || r.code != ExitOK {
				t.Fatalf("sheet apply: %+v", r)
			}
			return reply{out.Revision, out.Result.Replay}
		},
		"the API": func(t *testing.T, id, state, row string) reply {
			body := `{"message":"Budget","state":"` + state + `","op_id":"` + id + `","ops":[` + write(row) + `]}`
			resp, err := http.Post(srv.URL+"/api/flats/budget/sheet", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			var out struct {
				Result   reply `json:"result"`
				Revision int   `json:"revision"`
			}
			if err := json.Unmarshal(raw, &out); err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("POST = %d %s", resp.StatusCode, raw)
			}
			return reply{out.Revision, out.Result.Replay}
		},
		"the tool": func(t *testing.T, id, state, row string) reply {
			args := `{"slug":"budget","op_id":"` + id + `","state":"` + state + `","message":"Budget","ops":[` + write(row) + `]}`
			res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "update_sheet", Arguments: json.RawMessage(args)})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(res.StructuredContent)
			var out reply
			if err := json.Unmarshal(raw, &out); err != nil || res.IsError {
				content, _ := json.Marshal(res.Content)
				t.Fatalf("update_sheet = %s", content)
			}
			return out
		},
	}
	state := func(t *testing.T) (string, int) {
		t.Helper()
		raw, err := svc.GetSheet(t.Context(), "budget", 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		page, err := svc.ListRevisions(t.Context(), "budget", 0, 50)
		if err != nil {
			t.Fatal(err)
		}
		return out.State, len(page.Revisions)
	}
	row := 0
	for _, order := range [][]string{
		{"the command", "the API", "the tool"},
		{"the API", "the tool", "the command"},
		{"the tool", "the command", "the API"},
	} {
		row++
		t.Run(strings.Join(order, ", "), func(t *testing.T) {
			at, revisions := state(t)
			id, cell := "retry-"+strings.ReplaceAll(order[0], " ", "-"), string(rune('0'+row))
			first := surfaces[order[0]](t, id, at, cell)
			after, n := state(t)
			if first.Replay || first.Revision == 0 || after == at || n != revisions+1 {
				t.Fatalf("the first send through %s = %+v, state %s to %s, %d revisions more", order[0], first, at, after, n-revisions)
			}
			for _, name := range order[1:] {
				again := surfaces[name](t, id, at, cell)
				if !again.Replay || again.Revision != first.Revision {
					t.Fatalf("the send through %s after %s = %+v, want the stored result of revision %d", name, order[0], again, first.Revision)
				}
				if now, n := state(t); now != after || n != revisions+1 {
					t.Fatalf("the send through %s wrote: state %s to %s, %d revisions more", name, after, now, n-revisions-1)
				}
			}
		})
	}
}

// TestSheetApplyAlwaysSendsAnID covers a file whose op_id is empty: the
// command sends an id of its own, as it does for a file without one, so
// that the call can be sent again after a timeout.
func TestSheetApplyAlwaysSendsAnID(t *testing.T) {
	for _, file := range []string{`{"ops":[]}`, `{"op_id":"","ops":[]}`} {
		var sent struct {
			OpID string `json:"op_id"`
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Error(err)
			}
			io.WriteString(w, `{"result":{"state":"s_1.1"},"revision":2,"pending":false}`)
		}))
		r := run(t, srv.URL, "", "sheet", "apply", "budget", sheetOps(t, file))
		srv.Close()
		if r.code != ExitOK || !strings.HasPrefix(sent.OpID, "cli-") || !strings.Contains(r.stderr, "op_id: "+sent.OpID+" ") {
			t.Errorf("%s: sent op_id %q, %+v", file, sent.OpID, r)
		}
	}
}

// TestSheetApplySendsTheFileAsWritten covers the body the command builds
// from the file and its flags: it holds the values of the file byte for
// byte. Writing them again would spell "&", "<" and ">" as six bytes each,
// so a file within the bytes of one call could pass them and be refused by
// the command alone. A file of exactly the bytes of one call applies.
func TestSheetApplySendsTheFileAsWritten(t *testing.T) {
	srv, svc := sheetHost(t)
	cell := strings.Repeat("<&>", 10_000)
	var rows strings.Builder
	for i := range 70 {
		if i > 0 {
			rows.WriteByte(',')
		}
		rows.WriteString(`["` + cell + `"]`)
	}
	ops := sheetOps(t, `{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[`+rows.String()+`]}]}`)
	r := run(t, srv.URL, "", "sheet", "apply", "budget", ops, "--op-id", "signs")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Created sheet budget: revision 2, 70 cells written") {
		t.Fatalf("a file of 2.1 MB of signs: code %d, %.300s %.300s", r.code, r.stdout, r.stderr)
	}
	raw, err := svc.GetSheet(t.Context(), "budget", 0, []byte(`{"tab":"Sheet1","range":"A70"}`))
	var read struct {
		State string     `json:"state"`
		Rows  [][]string `json:"rows"`
	}
	if err != nil || json.Unmarshal(raw, &read) != nil || len(read.Rows) != 1 || read.Rows[0][0] != cell {
		t.Fatalf("the last cell does not hold the text of the file: %v", err)
	}

	// The file is one call: at the bytes of a call it applies, and one
	// byte more is refused before anything is sent.
	limit := int(sheet.MaxUpdateBodyBytes())
	file := func(size int) string {
		head := `{"op_id":"edge-` + fmt.Sprint(size) + `","ops":[`
		tail := `{"op":"write","tab":"Sheet1","at":"B1","rows":[["at the limit"]]}],"state":"` + read.State + `"}`
		return sheetOps(t, head+strings.Repeat(" ", size-len(head)-len(tail))+tail)
	}
	if r := run(t, srv.URL, "", "sheet", "apply", "budget", file(limit+1)); r.code == ExitOK || !strings.Contains(r.stderr, "exceeds the update_sheet limit") {
		t.Fatalf("a file one byte over the limit: %+v", r)
	}
	if r := run(t, srv.URL, "", "sheet", "apply", "budget", file(limit)); r.code != ExitOK || !strings.Contains(r.stdout, "Applied sheet budget: revision 3, 1 cells written") {
		t.Fatalf("a file of exactly the limit: code %d, %.300s %.300s", r.code, r.stdout, r.stderr)
	}
}

// TestSheetCallBytesBoundAcrossSurfaces sends one call of exactly the bytes
// of one call, and of one byte more, through `flats sheet apply`, the
// management API and the update_sheet tool. The tool's arguments carry the
// slug member besides; the bound counts the call without it, so each
// interface applies the first and refuses the second as call_bytes. The
// API also takes a body that ends with a line break after the call.
func TestSheetCallBytesBoundAcrossSurfaces(t *testing.T) {
	srv, svc, cs := sheetSurfaces(t)
	if _, err := svc.CreateSheet(t.Context(), "budget", "Budget", core.ViaMCP); err != nil {
		t.Fatal(err)
	}
	limit := int(sheet.MaxUpdateBodyBytes())
	state := func() string {
		t.Helper()
		raw, err := svc.GetSheet(t.Context(), "budget", 0, nil)
		var out struct {
			State string `json:"state"`
		}
		if err != nil || json.Unmarshal(raw, &out) != nil {
			t.Fatalf("read: %v", err)
		}
		return out.State
	}
	// call is the call in the form the command sends a file: its fields in
	// name order, with nothing between them. Its size comes from the text
	// of its cells, because an MCP client sends the arguments without
	// white space.
	call := func(id, at string, size int) string {
		head := `{"op_id":"` + id + `","ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[`
		if id == "" {
			head = `{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[`
		}
		tail := `]}],"state":"` + at + `"}`
		full := `["` + strings.Repeat("x", 30_000) + `"],`
		rest := size - len(head) - len(tail) - len(`[""]`)
		rows := strings.Repeat(full, rest/len(full))
		return head + rows + `["` + strings.Repeat("x", rest%len(full)) + `"]` + tail
	}
	type outcome struct {
		applied bool
		value   float64 // the value of a call_bytes refusal
		detail  string
	}
	surfaces := map[string]func(t *testing.T, body string) outcome{
		"the command": func(t *testing.T, body string) outcome {
			r := run(t, srv.URL, "", "sheet", "apply", "budget", sheetOps(t, body))
			if r.code == ExitOK {
				return outcome{applied: true}
			}
			if strings.Contains(r.stderr, "exceeds the update_sheet limit") {
				return outcome{value: -1, detail: r.stderr}
			}
			return outcome{detail: r.stderr}
		},
		"the API": func(t *testing.T, body string) outcome {
			resp, err := http.Post(srv.URL+"/api/flats/budget/sheet", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			var out struct {
				Limit string  `json:"limit"`
				Value float64 `json:"value"`
			}
			_ = json.Unmarshal(raw, &out)
			if resp.StatusCode == http.StatusOK {
				return outcome{applied: true}
			}
			if out.Limit == "call_bytes" {
				return outcome{value: out.Value, detail: string(raw)}
			}
			return outcome{detail: fmt.Sprintf("%d %.300s", resp.StatusCode, raw)}
		},
		"the tool": func(t *testing.T, body string) outcome {
			args := `{"slug":"budget",` + body[1:]
			res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "update_sheet", Arguments: json.RawMessage(args)})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(res.Content)
			if !res.IsError {
				return outcome{applied: true}
			}
			text := string(raw)
			if strings.Contains(text, `\"limit\":\"call_bytes\"`) {
				var v float64
				if _, after, ok := strings.Cut(text, `\"value\":`); ok {
					fmt.Sscanf(after, "%g", &v)
				}
				return outcome{value: v, detail: text}
			}
			return outcome{detail: fmt.Sprintf("%.300s", text)}
		},
	}
	for _, name := range []string{"the command", "the API", "the tool"} {
		t.Run(name, func(t *testing.T) {
			id := strings.ReplaceAll(name, " ", "-")
			before := state()
			over := surfaces[name](t, call(id+"-over", before, limit+1))
			want := float64(limit + 1)
			if name == "the command" {
				// The command refuses the file before it sends it.
				want = -1
			}
			if over.applied || over.value != want {
				t.Fatalf("a call one byte over the bound = %+v, want a call_bytes refusal of %v", over, want)
			}
			if state() != before {
				t.Fatal("the refused call changed the sheet")
			}
			if at := surfaces[name](t, call(id+"-at", before, limit)); !at.applied {
				t.Fatalf("a call of exactly the bound = %+v", at)
			}
		})
	}
	before := state()
	if got := surfaces["the API"](t, call("the-API-line", before, limit)+"\n"); !got.applied {
		t.Fatalf("a call of exactly the bound and a line break = %+v", got)
	}

	// The command preserves the values of the file, but the bound is that
	// of the request it sends. An op_id it generates and the fields of its
	// flags count toward it, so a file of exactly the bound without an id,
	// or with one and --message, is refused before it is sent. The API
	// applies the same bytes as they are.
	refused := func(t *testing.T, args ...string) {
		t.Helper()
		before, revisions := state(), revisionCount(t, svc, "budget")
		r := run(t, srv.URL, "", append([]string{"sheet", "apply", "budget"}, args...)...)
		if r.code == ExitOK || !strings.Contains(r.stderr, "with the fields the command adds exceeds the update_sheet limit") {
			t.Fatalf("%v: %+v", args[1:], r)
		}
		if state() != before || revisionCount(t, svc, "budget") != revisions {
			t.Fatalf("%v: the refused file changed the sheet", args[1:])
		}
	}
	at := call("", state(), limit)
	refused(t, sheetOps(t, at))
	refused(t, sheetOps(t, call("the-flag", state(), limit)), "--message", "m")
	if got := surfaces["the API"](t, at); !got.applied {
		t.Fatalf("the API with a call of exactly the bound without an op_id = %+v", got)
	}
}

func revisionCount(t *testing.T, svc *core.Service, slugName string) int {
	t.Helper()
	page, err := svc.ListRevisions(t.Context(), slugName, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	return len(page.Revisions)
}

// sheetSurfaces is a host with the management API and the MCP tools on one
// server, and an MCP client of it.
func sheetSurfaces(t *testing.T) (*httptest.Server, *core.Service, *mcp.ClientSession) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	priv, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := core.New(context.Background(), core.Config{DataDir: filepath.Join(dir, "data"), Store: st, Private: priv, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpx.Handler(svc, mcpx.Options{Version: "test"}))
	mux.Handle("/", (&api.Server{Svc: svc}).Handler())
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { srv.Close(); svc.Close(); priv.Close(); st.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "v0"}, nil)
	cs, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return srv, svc, cs
}
