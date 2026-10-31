package mcpx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gosuda/flats/internal/contenttype"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/notebook"
	"github.com/gosuda/flats/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const notebookTestPage = "p_aaaaaaaaaaaaaaaaaaaaaaaaaa"
const notebookTestDatabase = "d_aaaaaaaaaaaaaaaaaaaaaaaaaa"

type notebookFake struct {
	mu       sync.Mutex
	reads    int
	writes   int
	revision int
	query    string
	pending  bool
	refusal  error
	bodies   map[string]string
}

func (f *notebookFake) CreateNotebook(_ context.Context, slug, name string, via core.Via) (core.FlatView, error) {
	if via != core.ViaMCP {
		return core.FlatView{}, fmt.Errorf("wrong via")
	}
	return core.FlatView{Flat: store.Flat{Slug: slug, Name: name, Visibility: store.Visibility("private")}, Type: contenttype.Notebook, Running: true, PrivateURL: "http://notes.test"}, nil
}
func (f *notebookFake) UploadLimit() int64 { return 1024 }
func (f *notebookFake) GetNotebook(_ context.Context, slug string, rev int, query []byte) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	f.revision = rev
	f.query = string(query)
	if f.refusal != nil {
		return nil, f.refusal
	}
	if strings.Contains(string(query), `"op_id"`) {
		return json.RawMessage(`{"op_id":"write-1","total":1,"entries":[],"next":null,"complete":true}`), nil
	}
	return json.RawMessage(`{"state":{"store":"test","seq":"9007199254740993"},"revision":2,"pending":true,"page":{"title":"Notes"},"markdown":"# Notes","references":[],"next":"captured","complete":false,"warnings":[]}`), nil
}
func (f *notebookFake) UpdateNotebook(_ context.Context, slug string, body []byte, via core.Via) (core.NotebookWriteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if via != core.ViaMCP || slug != "notes" || strings.Contains(string(body), `"slug"`) {
		return core.NotebookWriteResult{}, fmt.Errorf("wrong core envelope")
	}
	if f.refusal != nil {
		return core.NotebookWriteResult{}, f.refusal
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(body, &fields)
	var id string
	_ = json.Unmarshal(fields["op_id"], &id)
	if f.bodies == nil {
		f.bodies = map[string]string{}
	}
	previous, replay := f.bodies[id]
	if replay && previous != string(body) {
		return core.NotebookWriteResult{}, &notebook.WriteError{Code: "conflict", Reason: "op_id", Detail: "different request"}
	}
	if !replay {
		f.writes++
		f.bodies[id] = string(body)
	}
	raw := json.RawMessage(fmt.Sprintf(`{"state":{"store":"test","seq":"4"},"revision":null,"revision_pending":true,"replay":%t,"created":false,"mapping":{"complete":false,"total":1,"cursor":"mapping"},"applied":1,"warnings":[],"samples":{"warnings":{"total":0,"complete":true}}}`, replay))
	return core.NotebookWriteResult{Result: raw, Pending: f.pending, Revision: 2}, nil
}

func notebookSession(t *testing.T, f *notebookFake, autopilot bool) *mcp.ClientSession {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "notebook-test", Version: "1"}, nil)
	register(srv, &tools{notebook: f, autopilot: autopilot})
	h := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	t.Cleanup(h.Close)
	return connect(t, h.URL)
}

func TestNotebookSchemaAndRoundTrip(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("autopilot-%t", on), func(t *testing.T) {
			fake := &notebookFake{pending: true}
			cs := notebookSession(t, fake, on)
			listed, err := cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			found := map[string]*mcp.Tool{}
			for _, tool := range listed.Tools {
				found[tool.Name] = tool
			}
			for _, name := range []string{"get_notebook", "update_notebook"} {
				tool := found[name]
				if tool == nil || !strings.Contains(tool.Description, "topic.notebook") || tool.OutputSchema == nil || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
					t.Fatalf("missing contract for %s: %+v", name, tool)
				}
			}
			if !found["get_notebook"].Annotations.ReadOnlyHint || found["update_notebook"].Annotations.ReadOnlyHint {
				t.Fatal("wrong read-only annotation")
			}
			var flat FlatInfo
			text, bad := call(t, cs, "create_flat", map[string]any{"slug": "notes", "type": "notebook"}, &flat)
			if bad || flat.Type != "notebook" || !flat.Running || flat.Draft != nil || flat.LiveVersion != 0 || !strings.Contains(text, "one empty top-level page") {
				t.Fatalf("create: %s %+v", text, flat)
			}
			var out map[string]any
			text, bad = call(t, cs, "get_notebook", map[string]any{"slug": "notes", "page": notebookTestPage, "format": "markdown", "limit": 1}, &out)
			if bad || out["state"].(map[string]any)["seq"] != "9007199254740993" || !strings.Contains(text, "More:") {
				t.Fatalf("read: %s %+v", text, out)
			}
			_, bad = call(t, cs, "get_notebook", map[string]any{"slug": "notes", "cursor": "captured"}, &out)
			if bad || fake.query != `{"cursor":"captured"}` {
				t.Fatalf("continuation: %s", fake.query)
			}
			args := map[string]any{"slug": "notes", "op_id": "write-1", "ops": []any{map[string]any{"op": "create_page", "ref": "page", "parent": nil, "before": nil, "title": "Notes"}}}
			text, bad = call(t, cs, "update_notebook", args, &out)
			if bad || out["revision"] != nil || out["revision_pending"] != true || !strings.Contains(text, "committed and live") || !strings.Contains(text, "Do not repeat") {
				t.Fatalf("pending: %s %+v", text, out)
			}
			text, bad = call(t, cs, "update_notebook", args, &out)
			if bad || fake.writes != 1 || out["replay"] != true || !strings.Contains(text, "original result") {
				t.Fatalf("replay: %s %+v", text, out)
			}
			args["message"] = "different"
			text, bad = call(t, cs, "update_notebook", args, nil)
			if !bad || !strings.Contains(text, `"category":"conflict"`) || fake.writes != 1 {
				t.Fatalf("mismatch: %s", text)
			}
			_, bad = call(t, cs, "get_notebook", map[string]any{"slug": "notes", "op_id": "write-1", "cursor": "mapping"}, &out)
			if bad || out["op_id"] != "write-1" {
				t.Fatalf("mapping: %+v", out)
			}
			var historical RevisionOut
			text, bad = call(t, cs, "get_revision", map[string]any{"slug": "notes", "revision": 7, "page": notebookTestPage, "format": "markdown"}, &historical)
			if bad || historical.Notebook["markdown"] != "# Notes" || fake.revision != 7 {
				t.Fatalf("historical: %s %+v", text, historical)
			}
			_, bad = call(t, cs, "get_revision", map[string]any{"slug": "notes", "revision": 7, "cursor": "captured"}, &historical)
			if bad || fake.revision != 7 || fake.query != `{"cursor":"captured"}` {
				t.Fatalf("historical continuation: %s", fake.query)
			}
		})
	}
}

func TestNotebookSchemaRefusals(t *testing.T) {
	fake := &notebookFake{}
	cs := notebookSession(t, fake, false)
	for _, tt := range []struct {
		name, tool string
		args       map[string]any
	}{
		{"unknown create", "create_flat", map[string]any{"slug": "notes", "type": "slides"}},
		{"unknown read", "get_notebook", map[string]any{"slug": "notes", "extra": 1}},
		{"wrong selector", "get_notebook", map[string]any{"slug": "notes", "page": notebookTestDatabase}},
		{"mixed cursor", "get_notebook", map[string]any{"slug": "notes", "cursor": "x", "page": notebookTestPage}},
		{"view conflict", "get_notebook", map[string]any{"slug": "notes", "database": notebookTestDatabase, "view": "v_aaaaaaaaaaaaaaaaaaaaaaaaaa", "sort": []any{}}},
		{"browser creation", "update_notebook", map[string]any{"slug": "notes", "op_id": "x", "ops": []any{map[string]any{"op": "create_page", "ref": "p", "parent": nil, "before": nil, "title": "n", "proposed_id": notebookTestPage}}}},
		{"missing parent", "update_notebook", map[string]any{"slug": "notes", "op_id": "x", "ops": []any{map[string]any{"op": "create_page", "ref": "p", "before": nil, "title": "n"}}}},
		{"bare mutation", "update_notebook", map[string]any{"slug": "notes", "op_id": "x", "ops": []any{map[string]any{"op": "delete_page", "page": notebookTestPage}}}},
		{"unknown operation", "update_notebook", map[string]any{"slug": "notes", "op_id": "x", "ops": []any{map[string]any{"op": "erase"}}}},
		{"irrelevant content", "update_notebook", map[string]any{"slug": "notes", "op_id": "x", "ops": []any{map[string]any{"op": "create_block", "ref": "b", "place": map[string]any{"page": map[string]any{"id": notebookTestPage, "inc": "1"}, "parent": nil, "before": nil}, "content": map[string]any{"kind": "divider", "text": "wrong"}}}}},
		{"path and page", "get_revision", map[string]any{"slug": "notes", "revision": 1, "page": notebookTestPage, "path": "private.json"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tt.tool, Arguments: tt.args})
			if err == nil && !result.IsError {
				t.Fatalf("accepted: %+v", result)
			}
		})
	}
	if fake.reads != 0 || fake.writes != 0 {
		t.Fatalf("refusal reached core: %+v", fake)
	}
	for _, tt := range []struct {
		name, reason string
		retry        bool
	}{
		{"invalid", "field", false}, {"conflict", "read_expired", false}, {"conflict", "result_expired", false}, {"notebook_capacity", "disk_quota", false}, {"notebook_capacity", "item_limit", false}, {"unavailable", "pressure", true}, {"forbidden", "origin", false}, {"not_found", "page", false}, {"not_notebook", "type", false},
	} {
		t.Run(tt.name+"-"+tt.reason, func(t *testing.T) {
			fake.refusal = &notebook.WriteError{Code: tt.name, Reason: tt.reason, Retry: tt.retry, Detail: "refused"}
			text, bad := call(t, cs, "get_notebook", map[string]any{"slug": "notes"}, nil)
			if !bad || !strings.Contains(text, `"category":"`+tt.name+`"`) || !strings.Contains(text, `"reason":"`+tt.reason+`"`) || !strings.Contains(text, "Hint:") {
				t.Fatalf("refusal: %s", text)
			}
		})
	}
}

func TestNotebookRunningText(t *testing.T) {
	for _, running := range []bool{true, false} {
		fi := flatInfo(core.FlatView{Type: contenttype.Notebook, Running: running, LiveVersion: 9, Versions: 9, Publication: "published"})
		if fi.LiveVersion != 0 || fi.Versions != 0 || fi.Publication != "" || fi.Draft != nil {
			t.Fatalf("shell leaked: %+v", fi)
		}
		text := flatText(fi)
		if !strings.Contains(text, "notebook,") || strings.Contains(text, "save_document") || strings.Contains(text, "version(s)") || (!running && !strings.Contains(text, "update_notebook")) {
			t.Fatalf("state text: %s", text)
		}
	}
}

func TestNotebookBodyLimit(t *testing.T) {
	for _, upload := range []int64{0, 1, 2, 3, 1024, 32 << 20, math.MaxInt64} {
		legal, err := notebook.MaxUpdateBodyBytes(upload)
		if err != nil {
			if maxBody(upload) != math.MaxInt64 {
				t.Fatal("overflow not bounded")
			}
			continue
		}
		if maxBody(upload) < legal+1024 {
			t.Fatalf("upload %d: body %d < update %d plus envelope", upload, maxBody(upload), legal)
		}
	}
	upload := int64(0)
	// An operation envelope at exactly the allowed byte limit must fit after
	// the transport adds JSON-RPC fields, including when uploads are disabled.
	prefix := `{"slug":"notes","op_id":"largest","ops":[]}`
	raw := append([]byte(prefix), bytes.Repeat([]byte(" "), notebook.MaxAgentOperationBytes-len(prefix))...)
	if _, err := notebook.DecodeUpdate(raw); err != nil {
		t.Fatal(err)
	}
	rpc := append([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"update_notebook","arguments":`), raw...)
	rpc = append(rpc, []byte(`}}`)...)
	w := httptest.NewRecorder()
	if _, err := io.ReadAll(http.MaxBytesReader(w, io.NopCloser(bytes.NewReader(rpc)), maxBody(upload))); err != nil {
		t.Fatal(err)
	}
}

func TestNotebookGuidanceParity(t *testing.T) {
	e := newEnv(t)
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("autopilot-%t", on), func(t *testing.T) {
			if on {
				setAutopilot(t, e, "", core.OpDeploy)
			}
			cs := connect(t, e.localURL)
			var guide GuideOut
			text, bad := call(t, cs, "guide", map[string]any{"items": []string{"topic.notebook"}}, &guide)
			if bad || len(guide.Items) != 1 {
				t.Errorf("notebook guidance not integrated (unit 11b): %s", text)
			}
		})
	}
}

func TestNotebookCreateEnumRegression(t *testing.T) { TestCreateAndSavePage(t) }

func TestNotebookToolCensus(t *testing.T)            { TestListTools(t) }
func TestNotebookPendingApprovalCensus(t *testing.T) { testMCPToolsPreservePendingApproval(t, true) }
func TestNotebookAutopilotCensus(t *testing.T) {
	t.Run("off-on-off", TestAutopilotOffIdentity)
	t.Run("texts", TestAutopilotOnTextsCensus)
}
