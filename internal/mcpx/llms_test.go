package mcpx

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	runtimeref "github.com/gosuda/flats/docs"
	"github.com/gosuda/flats/internal/core"
)

func fetch(t *testing.T, method, url string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(b)
}

func TestLLMsTxt(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(LLMsHandler(e.svc, Options{Version: "test"}))
	t.Cleanup(srv.Close)

	code, h, text := fetch(t, "GET", srv.URL+"/llms.txt")
	if code != 200 || h.Get("Content-Type") != "text/plain; charset=utf-8" || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("llms.txt = %d %v", code, h)
	}
	if !strings.HasPrefix(text, "# Flats\n\n> ") {
		t.Fatalf("llms.txt must start with an H1 title and a blockquote summary:\n%s", text)
	}
	for _, want := range []string{
		srv.URL + "/mcp",
		"claude mcp add --transport http flats " + srv.URL + "/mcp",
		srv.URL + RuntimeReferencePath,
		srv.URL + "/llms-full.txt",
		runtimeref.URI,
		"(version test)",
		"## Optional",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("llms.txt is missing %q", want)
		}
	}
	// The agent guide and tool list are exactly what an MCP client sees.
	if !strings.Contains(text, e.local.InitializeResult().Instructions) {
		t.Error("llms.txt does not carry the MCP server instructions")
	}
	tools, err := e.local.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) == 0 {
		t.Fatal("no MCP tools")
	}
	for _, tool := range tools.Tools {
		if !strings.Contains(text, "- `"+tool.Name+"`") || !strings.Contains(text, tool.Description) {
			t.Errorf("llms.txt is missing tool %s", tool.Name)
		}
	}
	if !strings.Contains(text, "- `get_flat` (read-only): ") || !strings.Contains(text, "- `delete_flat` (destructive; waits for operator approval): ") {
		t.Error("tool annotations are not labeled")
	}

	code, _, full := fetch(t, "GET", srv.URL+"/llms-full.txt")
	if code != 200 || !strings.HasPrefix(full, text) || !strings.HasSuffix(full, runtimeref.Markdown) {
		t.Fatalf("llms-full.txt must be llms.txt followed by the runtime reference (status %d)", code)
	}
	code, h, md := fetch(t, "GET", srv.URL+RuntimeReferencePath)
	if code != 200 || md != runtimeref.Markdown || h.Get("Content-Type") != "text/markdown; charset=utf-8" {
		t.Fatalf("runtime reference = %d %q", code, h.Get("Content-Type"))
	}
	if code, _, _ := fetch(t, "POST", srv.URL+"/llms.txt"); code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /llms.txt = %d", code)
	}
	flats, err := e.svc.ListFlats(context.Background())
	if err != nil || len(flats) != 0 {
		t.Fatalf("reading documentation changed flat state: %v, %v", flats, err)
	}
}

func TestLLMsTxtFollowsUploadLimit(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(LLMsHandler(e.svc, Options{Version: "test"}))
	t.Cleanup(srv.Close)
	if _, err := e.svc.UpdateSettings(context.Background(), map[string]string{core.SetUploadMaxBytes: "8192"}); err != nil {
		t.Fatal(err)
	}
	if _, _, text := fetch(t, "GET", srv.URL+"/llms.txt"); !strings.Contains(text, "8192 bytes") {
		t.Fatal("llms.txt did not refresh with host settings")
	}
}
