package docs

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/mcpx"
	"github.com/gosuda/flats/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func blockHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:16]
}

// postEdit sends a raw edit request to the app, as the host or as a client.
func (a *app) postEdit(t *testing.T, headers http.Header, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest("POST", a.srv.URL+"/_docs/api/edit?doc=index.md", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if headers != nil {
		req.Header = headers
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("edit response %d: %s", r.StatusCode, b)
	}
	return r.StatusCode, m
}

var hostEdit = http.Header{"X-Flats-Access": {"private"}, "X-Flats-Host-Op": {"edit"}}

func ops(v ...map[string]any) string {
	b, _ := json.Marshal(map[string]any{"ops": v})
	return string(b)
}

func TestAgentEditAuthorityGuardsAndBroadcast(t *testing.T) {
	data := t.TempDir()
	a := startApp(t, data, initialText, "v1")
	f := fixture(t)
	read := a.document(t)
	if read["hash"] == "" || read["markdown"] != initialText {
		t.Fatal(read)
	}
	sum := sha256.Sum256([]byte(initialText))
	if read["hash"] != hex.EncodeToString(sum[:]) {
		t.Fatal("whole-document hash", read["hash"])
	}
	// Block outline is private/host only.
	status, body, _ := a.getHeaders(t, "/_docs/api/document?blocks=1", http.Header{"X-Flats-Access": {"private"}})
	if status != 200 || !strings.Contains(body, `"hash":"`+blockHash("# Shared")+`"`) || !strings.Contains(body, `"preview":"Base line"`) {
		t.Fatal(status, body)
	}
	if _, body, _ := a.getHeaders(t, "/_docs/api/document?blocks=1", http.Header{"X-Flats-Access": {"public"}}); strings.Contains(body, `"blocks"`) {
		t.Fatal("public read listed blocks")
	}
	if _, body, _ := a.getHeaders(t, "/_docs/api/document?blocks=1&block_offset=1", http.Header{"X-Flats-Access": {"private"}}); !strings.Contains(body, `"blocks_total":2`) || !strings.Contains(body, `"block_offset":1`) || strings.Contains(body, blockHash("# Shared")) {
		t.Fatal("block paging", body)
	}

	// Only the host management channel may edit; browsers (even private) and
	// public routes are refused, and nothing changes.
	attempt := ops(map[string]any{"op": "replace", "find": "Base line", "with": "Base line (agent)"})
	for _, h := range []http.Header{nil, {"X-Flats-Access": {"private"}}, {"X-Flats-Access": {"public"}, "X-Flats-Host-Op": {"edit"}}, {"X-Flats-Access": {"private"}, "X-Flats-Host-Op": {"other"}}} {
		if status, m := a.postEdit(t, h, attempt); status != 403 || m["code"] != "readonly" {
			t.Fatal(h, status, m)
		}
	}
	if status, _, _ := a.get(t, "/_docs/api/edit"); status != 404 {
		t.Fatal("GET edit route", status)
	}

	// A connected private editor and a human edit committed after the agent read.
	x := connect(t, a.srv.URL, "private")
	x.hello(t, nil)
	edit2 := map[string]any{"op": "insert", "section_end": blockHash("# Shared"), "text": "Added by the agent."}
	human := edit(t, x, "person", f.Independent)
	// The agent's guards still hold: its targets were not touched.
	status, m := a.postEdit(t, hostEdit, ops(map[string]any{"op": "replace", "find": "Base line", "with": "Base line (agent)"}, edit2))
	if status != 200 || m["changed"] != true || m["seq"].(float64) != human["seq"].(float64)+1 || !strings.HasPrefix(m["id"].(string), "agent:") {
		t.Fatal(status, m)
	}
	// The connected editor receives the committed agent update without a ping.
	u := x.next(t, "update")
	if u["seq"] != m["seq"] || u["chain"] != m["chain"] {
		t.Fatal("broadcast", u, m)
	}
	live := a.document(t)["markdown"].(string)
	for _, want := range []string{"Person 한국어 🙂", "Base line (agent)\nAgent line\n\nAdded by the agent.\n"} {
		if !strings.Contains(live, want) {
			t.Fatalf("missing %q in %q", want, live)
		}
	}
	if a.document(t)["hash"] != m["hash"] {
		t.Fatal("result hash differs from the live hash")
	}
	// The editor keeps working on top of the agent edit.
	edit(t, x, "peer", f.Updates[1].U)
	if !strings.Contains(a.document(t)["markdown"].(string), "Peer 🌍") {
		t.Fatal("human edit after agent edit lost")
	}

	// Stale, missing and ambiguous guards change nothing: their edit wins.
	before := a.document(t)
	stale := []string{
		ops(map[string]any{"op": "replace_block", "block": blockHash("Base line\nAgent line"), "with": "x"}),
		ops(map[string]any{"op": "replace", "find": "line", "with": "row"}),
		ops(map[string]any{"op": "replace", "find": "Agent line", "with": "ok"}, map[string]any{"op": "replace", "find": "absent text", "with": "x"}),
		`{"ops":[{"op":"insert","at":"end","text":"x"}],"if_hash":"` + strings.Repeat("0", 64) + `"}`,
	}
	for _, body := range stale {
		if status, m := a.postEdit(t, hostEdit, body); status != 409 || m["code"] != "edit_conflict" {
			t.Fatal(body, status, m)
		}
	}
	for _, body := range []string{"not json", `{"ops":[]}`, ops(map[string]any{"op": "rewrite"}), `{"ops":[{"op":"insert","at":"end","text":"x"}],"extra":1}`,
		ops(map[string]any{"op": "replace", "find": "Agent line", "with": "x", "nth": 1})} {
		if status, m := a.postEdit(t, hostEdit, body); status != 400 || m["code"] != "invalid" {
			t.Fatal(body, status, m)
		}
	}
	after := a.document(t)
	if after["seq"] != before["seq"] || after["markdown"] != before["markdown"] {
		t.Fatal("a refused edit changed the document")
	}
	// The whole-document guard accepts the current hash.
	if status, m := a.postEdit(t, hostEdit, `{"ops":[{"op":"replace","find":"Agent line","with":"Agent line"}],"if_hash":"`+after["hash"].(string)+`"}`); status != 200 || m["changed"] != false || m["seq"] != after["seq"] {
		t.Fatal("no-op edit", status, m)
	}

	// Limits: request size, single update size and the 1 MiB document limit.
	if status, m := a.postEdit(t, hostEdit, ops(map[string]any{"op": "insert", "at": "end", "text": strings.Repeat("x", 513*1024)})); status != 413 || m["code"] != "capacity" {
		t.Fatal(status, m)
	}
	if status, m := a.postEdit(t, hostEdit, ops(map[string]any{"op": "insert", "at": "end", "text": strings.Repeat("y", 300*1024)})); status != 422 || m["code"] != "capacity" {
		t.Fatal(status, m)
	}
	for i := 0; ; i++ {
		status, m := a.postEdit(t, hostEdit, ops(map[string]any{"op": "insert", "at": "end", "text": strings.Repeat(string(rune('a'+i)), 250*1024)}))
		if status == 200 {
			if i > 4 {
				t.Fatal("document grew past 1 MiB")
			}
			continue
		}
		if status != 422 || m["code"] != "capacity" || i != 4 {
			t.Fatal(i, status, m)
		}
		break
	}

	// Agent receipts are ordinary committed history.
	db, err := sql.Open("sqlite", filepath.Join(data, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM flats_docs_receipts WHERE doc='index.md' AND id LIKE 'agent:%'").Scan(&n); err != nil || n != 5 {
		t.Fatal("agent receipts", n, err)
	}
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any, out any) (string, bool) {
	t.Helper()
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text + "\n")
		}
	}
	if out != nil && !result.IsError {
		b, _ := json.Marshal(result.StructuredContent)
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatal(err)
		}
	}
	return text.String(), result.IsError
}

func TestHostUpdateDocumentLifecycle(t *testing.T) {
	s, private, public := hostService(t)
	mcpServer := httptest.NewServer(mcpx.Handler(s, mcpx.Options{Version: "test"}))
	defer mcpServer.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "edit-test", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: mcpServer.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	insert := map[string]any{"slug": "host-doc", "ops": []any{map[string]any{"op": "insert", "at": "end", "text": "Agent note."}}}

	// No docs version runs yet: refuse with the save_document + publish hint.
	text, failed := callTool(t, session, "update_document", insert, nil)
	if !failed || !strings.Contains(text, `"category":"not_deployed"`) || !strings.Contains(text, "save_document and publish") {
		t.Fatal(text)
	}
	_, err = s.Deploy(t.Context(), "host-doc", 0, core.ViaMCP)
	approveHost(t, s, err)
	srv := httptest.NewServer(private.handler("host-doc"))
	defer srv.Close()
	editor := connect(t, srv.URL, "private")
	editor.hello(t, nil)

	var read core.Document
	if text, failed := callTool(t, session, "get_document", map[string]any{"slug": "host-doc", "blocks": true}, &read); failed || len(read.Blocks) != 2 || read.Hash == "" {
		t.Fatal(text, read)
	}
	if read.Blocks[1].Hash != blockHash("Base line\nAgent line") || read.Blocks[0].Kind != "heading" || read.Blocks[0].Level != 1 {
		t.Fatal(read.Blocks)
	}
	// A person edits while the agent prepares its edit.
	edit(t, editor, "human", fixture(t).Independent)
	var out core.DocumentEdit
	args := map[string]any{"slug": "host-doc", "ops": []any{
		map[string]any{"op": "replace_block", "block": read.Blocks[1].Hash, "with": "Base line\nAgent line, revised live"},
		map[string]any{"op": "insert", "at": "end", "text": "Agent note."},
	}}
	text, failed = callTool(t, session, "update_document", args, &out)
	if failed || !out.Changed || out.Source != "live" || out.PublicNotice != "" || len(out.Ops) != 2 || out.Ops[0].Op != "replace_block" {
		t.Fatal(text, out)
	}
	if u := editor.next(t, "update"); u["seq"] != float64(out.Seq) {
		t.Fatal("editor did not receive the agent edit", u, out)
	}
	live, err := s.GetDocument(t.Context(), "host-doc", "")
	if err != nil || live.Hash != out.Hash || !strings.Contains(live.Markdown, "Person 한국어 🙂") || !strings.Contains(live.Markdown, "Agent line, revised live\n\nAgent note.\n") {
		t.Fatal(live, err)
	}
	// The same guard is now stale: nothing changes.
	text, failed = callTool(t, session, "update_document", args, nil)
	if !failed || !strings.Contains(text, `"category":"edit_conflict"`) || !strings.Contains(text, "refusal.edit_conflict") {
		t.Fatal(text)
	}
	if again, _ := s.GetDocument(t.Context(), "host-doc", ""); again.Seq != live.Seq {
		t.Fatal("stale edit changed seq")
	}
	// The event log records the edit without its text.
	events, err := s.Events(t.Context(), "host-doc", "document", 0, 10)
	if err != nil || len(events) != 1 || !strings.Contains(events[0].Message, "live edit of index.md via mcp: 2 op(s)") || strings.Contains(events[0].Message, "revised") || strings.Contains(string(events[0].Data), "revised") {
		t.Fatal(events, err)
	}
	// Not a publish: versions and Draft are unchanged.
	versions, _ := s.ListVersions(t.Context(), "host-doc")
	if len(versions) != 1 {
		t.Fatal("live edit published a version", versions)
	}

	// A later version changing other lines keeps the agent edit; one changing
	// the same line wins and keeps the discarded live text for recovery.
	next := strings.Replace(initialText, "# Shared", "# Shared v2", 1)
	if _, err = s.SaveVersion(t.Context(), "host-doc", []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte(next)}}, core.SaveMeta{}, core.ViaMCP); err != nil {
		t.Fatal(err)
	}
	_, err = s.Deploy(t.Context(), "host-doc", 0, core.ViaMCP)
	approveHost(t, s, err)
	merged, err := s.GetDocument(t.Context(), "host-doc", "")
	if err != nil || !strings.Contains(merged.Markdown, "# Shared v2") || !strings.Contains(merged.Markdown, "Agent line, revised live") || !strings.Contains(merged.Markdown, "Agent note.") || len(merged.Conflicts) != 0 {
		t.Fatal(merged, err)
	}
	overlap := strings.Replace(next, "Agent line", "Agent line from v3", 1)
	if _, err = s.SaveVersion(t.Context(), "host-doc", []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte(overlap)}}, core.SaveMeta{}, core.ViaMCP); err != nil {
		t.Fatal(err)
	}
	_, err = s.Deploy(t.Context(), "host-doc", 0, core.ViaMCP)
	approveHost(t, s, err)
	v3, err := s.GetDocument(t.Context(), "host-doc", "")
	if err != nil || !strings.Contains(v3.Markdown, "Agent line from v3") || strings.Contains(v3.Markdown, "revised live") || len(v3.Conflicts) != 1 {
		t.Fatal(v3, err)
	}
	if kept, err := s.GetDocumentConflict(t.Context(), "host-doc", "", v3.Conflicts[0].Generation); err != nil || !strings.Contains(kept.Markdown, "Agent line, revised live") {
		t.Fatal("discarded agent edit not recoverable", kept, err)
	}

	// An explicit empty guard is refused, not dropped into an unguarded edit.
	for _, bad := range []map[string]any{
		{"slug": "host-doc", "if_hash": "", "ops": []any{map[string]any{"op": "insert", "at": "end", "text": "unguarded"}}},
		{"slug": "host-doc", "if_hash": v3.Hash, "ops": []any{map[string]any{"op": "insert", "at": "end", "text": "unguarded", "nth": 0}}},
	} {
		if text, failed := callTool(t, session, "update_document", bad, nil); !failed || !strings.Contains(text, `"category":"invalid"`) {
			t.Fatal(text)
		}
	}
	if again, _ := s.GetDocument(t.Context(), "host-doc", ""); strings.Contains(again.Markdown, "unguarded") {
		t.Fatal("empty guard applied an edit")
	}
	// Public flats: every result says the change is already public.
	if err := s.SetProviderPermission(t.Context(), "host-doc", store.ProviderPortal, true, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	res, err := s.SetVisibility(t.Context(), "host-doc", store.Public, core.ViaMCP, "test")
	approveAction(t, s, res, err)
	text, failed = callTool(t, session, "update_document", insert, &out)
	if failed || out.PublicNotice != core.LiveEditPublicNotice || !strings.Contains(text, "anyone on the internet") || !strings.Contains(text, "already live") {
		t.Fatal(text, out)
	}
	// A public visitor cannot use the edit route even with a forged header.
	publicSrv := httptest.NewServer(public.handler("host-doc"))
	defer publicSrv.Close()
	req, _ := http.NewRequest("POST", publicSrv.URL+"/_docs/api/edit", strings.NewReader(ops(map[string]any{"op": "insert", "at": "end", "text": "forged"})))
	req.Header.Set("X-Flats-Host-Op", "edit")
	req.Header.Set("X-Flats-Access", "private")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal("public route accepted a forged host edit", resp.StatusCode)
	}
	// A private browser request cannot forge it either.
	req, _ = http.NewRequest("POST", srv.URL+"/_docs/api/edit", strings.NewReader(ops(map[string]any{"op": "insert", "at": "end", "text": "forged"})))
	req.Header.Set("X-Flats-Host-Op", "edit")
	if resp, err = http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if final, _ := s.GetDocument(t.Context(), "host-doc", ""); resp.StatusCode != 403 || strings.Contains(final.Markdown, "forged") {
		t.Fatal("private route accepted a forged host edit", resp.StatusCode)
	}

	// Website flats refuse with not_docs; a missing document with document_not_found.
	if _, err := s.UpdateDocument(t.Context(), "host-doc", "absent.md", []core.DocumentEditOp{{Op: "insert", At: "end", Text: "x"}}, nil, core.ViaMCP); !errors.Is(err, core.ErrDocumentNotFound) {
		t.Fatal(err)
	}
	if _, err := s.UpdateDocument(t.Context(), "host-doc", "", nil, nil, core.ViaMCP); core.ErrorCategory(err) != "invalid" {
		t.Fatal(err)
	}
}
