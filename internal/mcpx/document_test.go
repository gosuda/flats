package mcpx

import (
	"context"
	"strings"
	"testing"

	runtimeref "github.com/gosuda/flats/docs"
	"github.com/gosuda/flats/internal/core"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type documentRuntime struct{}

func (documentRuntime) Start(context.Context, core.RuntimeSpec) (core.Instance, error) {
	panic("Draft save must not start runtime")
}

func TestDocumentTools(t *testing.T) {
	e := newEnvRuntime(t)
	for name, client := range map[string]*mcp.ClientSession{"local": e.local, "remote": e.remote} {
		t.Run(name, func(t *testing.T) {
			slug := "doc-" + name
			for i, path := range []string{"", "notes/chapter.markdown"} {
				var saved SaveOut
				text, failed := call(t, client, "save_document", map[string]any{"slug": slug, "markdown": "# Hello\n한글 😀", "title": "Title", "path": path, "message": "edit"}, &saved)
				if failed || saved.Version.Type != "docs" || saved.Draft.Type != "docs" || saved.Draft.Revision != i+1 || saved.Version.Number != 0 || saved.Version.Published || saved.Deploy != nil {
					t.Fatalf("save: %s %+v", text, saved)
				}
				var out core.Document
				text, failed = call(t, client, "get_document", map[string]any{"slug": slug}, &out)
				if failed || out.Source != "draft" || out.Markdown != "# Hello\n한글 😀" {
					t.Fatalf("read: %s %+v", text, out)
				}
				flat, _ := e.svc.GetFlat(context.Background(), slug)
				if flat.LiveVersion != 0 || flat.Type != "docs" {
					t.Fatalf("flat: %+v", flat)
				}
				versions, _ := e.svc.ListVersions(context.Background(), slug)
				approvals, _ := e.svc.ListApprovals(context.Background(), "pending")
				if len(versions) != 0 || len(approvals) != 0 {
					t.Fatal("document save published or requested approval")
				}
			}
			for _, path := range []string{"../x.md", "x.txt", "_docs/a.md"} {
				if text, failed := call(t, client, "save_document", map[string]any{"slug": slug, "markdown": "bad", "path": path}, nil); !failed || !strings.Contains(text, "fix") && !strings.Contains(text, "Hint") {
					t.Fatalf("bad path: %s", text)
				}
			}
			for _, generation := range []int64{-1, 4} {
				text, failed := call(t, client, "get_document", map[string]any{"slug": slug, "conflict": generation}, nil)
				category := "document_not_found"
				if generation < 0 {
					category = "invalid"
				}
				if !failed || !strings.Contains(text, `"category":"`+category+`"`) {
					t.Fatal(text)
				}
			}
			text, failed := call(t, client, "get_document", map[string]any{"slug": slug, "doc": "absent.md"}, nil)
			if !failed || !strings.Contains(text, `"category":"document_not_found"`) {
				t.Fatalf("missing document: %s", text)
			}
		})
	}
}

func TestContentTypesDiscovery(t *testing.T) {
	e := newEnv(t)
	for name, client := range map[string]*mcp.ClientSession{"local": e.local, "remote": e.remote} {
		t.Run(name, func(t *testing.T) {
			if ins := client.InitializeResult().Instructions; !strings.Contains(ins, runtimeref.ContentTypesURI) || !strings.Contains(ins, "guide topic.docs") || !strings.Contains(ins, "get_content_types") {
				t.Fatal("missing discovery instructions")
			}
			read, err := client.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: runtimeref.ContentTypesURI})
			if err != nil || len(read.Contents) != 1 || read.Contents[0].Text != runtimeref.ContentTypesMarkdown {
				t.Fatalf("resource: %+v %v", read, err)
			}
			var out referenceOut
			text, failed := call(t, client, "get_content_types", map[string]any{}, &out)
			if failed || text != out.Markdown || out.Markdown != runtimeref.ContentTypesMarkdown || out.URI != runtimeref.ContentTypesURI || out.DocumentationVersion != "1" || out.UploadLimitBytes != e.svc.UploadLimit() {
				t.Fatalf("reference: %+v", out)
			}
			list, _ := client.ListTools(context.Background(), nil)
			for _, tool := range list.Tools {
				if tool.Name == "get_document" && (tool.Annotations == nil || tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint) {
					t.Fatal("live reads must advertise possible non-destructive activation")
				}
				if tool.Name == "get_content_types" && (!tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint) {
					t.Fatal("reference tool mutates")
				}
			}
		})
	}
	f, _ := e.svc.ListFlats(context.Background())
	if len(f) != 0 {
		t.Fatal("reference mutated state")
	}
}
