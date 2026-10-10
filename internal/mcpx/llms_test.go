package mcpx

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	runtimeref "github.com/gosuda/flats/docs"
	"github.com/gosuda/flats/internal/core"
)

func fetch(t *testing.T, c *http.Client, method, url string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
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

// linkItem is an llms.txt file list entry: "- [name](url)" with optional ": notes".
var linkItem = regexp.MustCompile(`^- \[[^\]]+\]\((https?://[^)\s]+)\)(: .+)?$`)

// checkLLMsFormat enforces https://llmstxt.org: an H1, a blockquote summary,
// free-form text without headings, then H2 sections holding only link lists.
func checkLLMsFormat(t *testing.T, text string) (links []string) {
	t.Helper()
	if !strings.HasPrefix(text, "# Flats\n\n> ") {
		t.Fatalf("llms.txt must start with an H1 title and a blockquote summary:\n%s", text)
	}
	section := ""
	for _, line := range strings.Split(text, "\n")[1:] {
		switch {
		case strings.HasPrefix(line, "## "):
			section = line
		case strings.HasPrefix(line, "#"):
			t.Errorf("unexpected heading %q", line)
		case section != "" && line != "":
			m := linkItem.FindStringSubmatch(line)
			if m == nil {
				t.Errorf("%s holds a non-link line %q", section, line)
				continue
			}
			links = append(links, m[1])
		}
	}
	return links
}

func TestLLMsTxt(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(LLMsHandler(e.svc, Options{Version: "test"}))
	t.Cleanup(srv.Close)
	c := srv.Client()

	code, h, index := fetch(t, c, "GET", srv.URL+LLMsPath)
	if code != 200 || h.Get("Content-Type") != "text/plain; charset=utf-8" || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("llms.txt = %d %v", code, h)
	}
	for _, want := range []string{
		"claude mcp add --transport http flats " + srv.URL + "/mcp",
		"codex mcp add flats --url " + srv.URL + "/mcp",
		runtimeref.URI,
		"(version test)",
		"\n## Docs\n",
		"\n## Optional\n",
	} {
		if !strings.Contains(index, want) {
			t.Errorf("llms.txt is missing %q", want)
		}
	}
	// Every linked document on this host resolves.
	links := checkLLMsFormat(t, index)
	for _, path := range []string{AgentGuidePath, RuntimeReferencePath, ContentTypesPath, LLMsFullPath} {
		if !strings.Contains(strings.Join(links, " "), srv.URL+path) {
			t.Errorf("llms.txt does not link %s", path)
		}
	}
	for _, link := range links {
		if strings.HasPrefix(link, srv.URL) {
			if code, _, _ := fetch(t, c, "GET", link); code != 200 {
				t.Errorf("linked %s = %d", link, code)
			}
		}
	}

	// The agent guide carries exactly what an MCP client sees.
	code, h, guide := fetch(t, c, "GET", srv.URL+AgentGuidePath)
	if code != 200 || h.Get("Content-Type") != "text/markdown; charset=utf-8" {
		t.Fatalf("agent guide = %d %v", code, h)
	}
	if !strings.Contains(guide, e.local.InitializeResult().Instructions) || !strings.Contains(guide, srv.URL+"/mcp") {
		t.Error("agent guide does not carry the MCP server instructions and endpoint")
	}
	tools, err := e.local.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) == 0 {
		t.Fatal("no MCP tools")
	}
	for _, tool := range tools.Tools {
		if !strings.Contains(guide, "- `"+tool.Name+"`") || !strings.Contains(guide, tool.Description) {
			t.Errorf("agent guide is missing tool %s", tool.Name)
		}
	}
	if !strings.Contains(guide, "- `get_flat` (read-only): ") || !strings.Contains(guide, "- `delete_flat` (destructive): ") {
		t.Error("tool annotations are not labeled")
	}

	code, _, full := fetch(t, c, "GET", srv.URL+LLMsFullPath)
	if code != 200 || full != index+"\n---\n\n"+guide {
		t.Fatalf("llms-full.txt must concatenate the index and agent guide (status %d)", code)
	}
	// The agent guide carries every guide topic and refusal page, so llms-full
	// covers both references without repeating them.
	for _, name := range runtimeref.TopicOrder {
		if md, _ := runtimeref.Topic(name); !strings.Contains(guide, md) {
			t.Errorf("agent guide is missing topic.%s", name)
		}
	}
	for _, c := range runtimeref.RefusalCategories() {
		if !strings.Contains(guide, "### refusal."+c+"\n") {
			t.Errorf("agent guide is missing refusal.%s", c)
		}
	}
	code, h, md := fetch(t, c, "GET", srv.URL+RuntimeReferencePath)
	if code != 200 || md != runtimeref.Markdown || h.Get("Content-Type") != "text/markdown; charset=utf-8" {
		t.Fatalf("runtime reference = %d %q", code, h.Get("Content-Type"))
	}
	code, _, contentTypes := fetch(t, c, "GET", srv.URL+ContentTypesPath)
	if code != 200 || contentTypes != runtimeref.ContentTypesMarkdown {
		t.Fatal("content types reference missing")
	}
	for _, want := range []string{"save_document", "get_document", "flats://docs/content-types/v1", "| `type` |"} {
		if !strings.Contains(guide, want) {
			t.Errorf("guide missing %s", want)
		}
	}
	if strings.Contains(guide, "- `get_document` (read-only)") {
		t.Fatal("live read incorrectly labeled read-only")
	}
	for _, path := range LLMsPaths {
		if code, _, _ := fetch(t, c, "POST", srv.URL+path); code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d", path, code)
		}
	}
	flats, err := e.svc.ListFlats(context.Background())
	if err != nil || len(flats) != 0 {
		t.Fatalf("reading documentation changed flat state: %v, %v", flats, err)
	}
}

// The tailnet console node serves TLS; its documents must name https URLs.
func TestLLMsTxtOverTLS(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewTLSServer(LLMsHandler(e.svc, Options{Version: "test"}))
	t.Cleanup(srv.Close)
	if !strings.HasPrefix(srv.URL, "https://") {
		t.Fatal(srv.URL)
	}
	for _, path := range []string{LLMsPath, AgentGuidePath} {
		if _, _, text := fetch(t, srv.Client(), "GET", srv.URL+path); !strings.Contains(text, srv.URL+"/mcp") || strings.Contains(text, "http://") {
			t.Errorf("%s does not name https URLs only", path)
		}
	}
}

func TestLLMsTxtFollowsUploadLimit(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(LLMsHandler(e.svc, Options{Version: "test"}))
	t.Cleanup(srv.Close)
	if _, err := e.svc.UpdateSettings(context.Background(), map[string]string{core.SetUploadMaxBytes: "8192"}); err != nil {
		t.Fatal(err)
	}
	if _, _, text := fetch(t, srv.Client(), "GET", srv.URL+AgentGuidePath); !strings.Contains(text, "8192 bytes") {
		t.Fatal("agent guide did not refresh with host settings")
	}
}
