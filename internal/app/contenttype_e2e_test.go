package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/expose/provider"
	"github.com/gosuda/flats/internal/mcpx"
	"github.com/gosuda/flats/internal/runtime"
	"github.com/gosuda/flats/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The real manager re-executes this test binary as a disposable worker.
func TestMain(m *testing.M) {
	if os.Getenv("FLATS_CONTENT_TEST_WORKER") == "1" {
		if err := runtime.WorkerMain(os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

const documentTestApp = `import content from "./content.js";
export default { async fetch(request, env) {
 const u=new URL(request.url);
 const access=request.headers.get("x-flats-access");
 if (u.pathname==="/_docs/healthz") return new Response(access==="private" ? "ok" : "untrusted health", {status: access==="private" ? 200 : 500});
 if (u.pathname==="/edit") {env.FILES.put("live", "# People's live edits\n한글 😀");return new Response("edited");}
 if (u.pathname==="/_docs/api/document") {
  if (access!=="private") return new Response("untrusted", {status:403});
  const doc=content.documents.find(d=>d.path===(u.searchParams.get("doc") || content.entry));
  if (!doc) return new Response("missing",{status:404});
  return Response.json({format:1,doc:doc.path,markdown:env.FILES.get("live") || doc.text,epoch:"test",seq:1,source:"live"});
 }
 return new Response("worker:"+access+":"+content.documents[0].text);
}};`

func TestDocsRealRuntimeEndToEnd(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	net, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer net.Close()
	manager, err := provider.New(dir, provider.Options{Local: net})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	rt := &runtime.Manager{DataDir: dir, Env: []string{"FLATS_CONTENT_TEST_WORKER=1"}, Logf: t.Logf}
	app := core.DocsApp{FS: fstest.MapFS{"server.js": &fstest.MapFile{Data: []byte(documentTestApp)}}, Hash: strings.Repeat("b", 64), Entry: "server.js", ContentModule: "content.js"}
	svc, err := core.New(ctx, core.Config{DataDir: dir, Store: st, Private: net, Lifecycle: manager, Runtime: rt, DocsApp: app, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	files := []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte("# Initial\n한글 😀")}, {Path: "image.txt", Data: []byte("host asset")}}
	saved, err := svc.SaveVersion(ctx, "documents", files, core.SaveMeta{}, core.ViaMCP)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Number != 0 || saved.Kind != "server" {
		t.Fatalf("save: %+v", saved)
	}
	draft, err := svc.GetDocument(ctx, "documents", "")
	if err != nil || draft.Source != "draft" || draft.Markdown != "# Initial\n한글 😀" {
		t.Fatalf("draft: %+v %v", draft, err)
	}
	fetchRoute := func(base, path, forged string) (int, string, http.Header) {
		t.Helper()
		r, _ := http.NewRequest("GET", base+path, nil)
		r.Header.Set("X-Flats-Access", forged)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}
	preview, err := svc.OpenPreview(ctx, "documents", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, forged := range []string{"public", "private"} {
		if code, body, _ := fetchRoute(preview.URL, "/", forged); code != 200 || !strings.Contains(body, "worker:private:# Initial") {
			t.Fatalf("Draft preview %d %s", code, body)
		}
	}
	_, err = svc.Deploy(ctx, "documents", 0, core.ViaAPI)
	var pending *core.PendingApproval
	if !errors.As(err, &pending) {
		t.Fatalf("approval: %v", err)
	}
	if _, err := svc.Decide(ctx, pending.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	flat, err := svc.GetFlat(ctx, "documents")
	if err != nil || flat.Type != "docs" || flat.LiveVersion != 1 {
		t.Fatalf("flat: %+v %v", flat, err)
	}
	for _, forged := range []string{"public", "private"} {
		if code, body, _ := fetchRoute(flat.PrivateURL, "/", forged); code != 200 || !strings.Contains(body, "worker:private:# Initial") {
			t.Fatalf("live %d %s", code, body)
		}
	}
	if code, body, headers := fetchRoute(flat.PrivateURL, "/image.txt", "public"); code != 200 || body != "host asset" || headers.Get("ETag") == "" {
		t.Fatalf("asset %d %q %v", code, body, headers)
	}
	if code, body, _ := fetchRoute(flat.PrivateURL, "/edit", "public"); code != 200 || body != "edited" {
		t.Fatalf("edit %d %s", code, body)
	}
	live, err := svc.GetDocument(ctx, "documents", "")
	if err != nil || live.Source != "live" || live.Markdown != "# People's live edits\n한글 😀" {
		t.Fatalf("live: %+v %v", live, err)
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpx.Handler(svc, mcpx.Options{Version: "test"}))
	mux.Handle("/", (&api.Server{Svc: svc}).Handler())
	management := httptest.NewServer(mux)
	defer management.Close()
	for _, prefix := range []string{"/api", "/console/api"} {
		r, _ := http.NewRequest("GET", management.URL+prefix+"/flats/documents/document?doc=index.md", nil)
		r.Header.Set("X-Flats-Console", "1")
		r.Header.Set("X-Flats-Access", "public")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		var out core.Document
		err = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 || out.Markdown != live.Markdown || out.Source != "live" {
			t.Fatalf("API %s: %+v %v", prefix, out, err)
		}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "docs-e2e", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: management.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_document", Arguments: map[string]any{"slug": "documents"}})
	if err != nil || result.IsError {
		t.Fatalf("MCP read %+v %v", result, err)
	}
	b, _ := json.Marshal(result.StructuredContent)
	var out core.Document
	_ = json.Unmarshal(b, &out)
	if out.Markdown != live.Markdown || out.Source != "live" {
		t.Fatalf("MCP document %s", b)
	}
	if _, err := svc.GetDocument(ctx, "documents", "missing.md"); !errors.Is(err, core.ErrDocumentNotFound) {
		t.Fatal(err)
	}
	files[1].Data = []byte("# Current Draft")
	if _, err := svc.SaveVersion(ctx, "documents", files, core.SaveMeta{}, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	preview, err = svc.OpenPreview(ctx, "documents", 0)
	if err != nil {
		t.Fatal(err)
	}
	if code, body, _ := fetchRoute(preview.URL, "/", "public"); code != 200 || !strings.Contains(body, "worker:private:# Current Draft") {
		t.Fatalf("new Draft preview %d %s", code, body)
	}
	live, err = svc.GetDocument(ctx, "documents", "")
	if err != nil || live.Markdown != "# People's live edits\n한글 😀" {
		t.Fatalf("Draft affected live: %+v %v", live, err)
	}
}
