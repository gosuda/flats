package mcpx

import (
	"context"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func receiptText(r *mcp.CallToolResult) string {
	var text strings.Builder
	for _, c := range r.Content {
		if c, ok := c.(*mcp.TextContent); ok {
			text.WriteString(c.Text)
		}
	}
	return text.String()
}

func TestCommittedDocumentReceiptSurvivesMetadataChanges(t *testing.T) {
	for _, change := range []string{"deleted", "slug-reused", "malformed-manifest"} {
		t.Run(change, func(t *testing.T) {
			e := newEnvRuntime(t)
			ctx := context.Background()
			v, err := e.svc.SaveDocument(ctx, "receipt", "index.md", "# Saved", "", core.SaveMeta{}, core.ViaMCP)
			if err != nil {
				t.Fatal(err)
			}
			if change == "malformed-manifest" {
				// A display-field decode failure after commit is not a failed save.
				v.Manifest = []byte(`{"entry":`)
			} else {
				// Deterministically model metadata changing after the storage lock
				// was released, without relying on a concurrent scheduling race.
				if _, err := e.svc.Delete(ctx, "receipt", core.ViaConsole, ""); err != nil {
					t.Fatal(err)
				}
				if change == "slug-reused" {
					if _, err := e.svc.SaveVersion(ctx, "receipt", []bundle.File{{Path: "index.html", Data: []byte("new website")}}, core.SaveMeta{}, core.ViaMCP); err != nil {
						t.Fatal(err)
					}
				}
				// The captured manifest alone must also select the document model
				// for the shared bundle receipt, without its request-context flag.
				r, out, err := (&tools{svc: e.svc}).saved(ctx, "receipt", nil, v, nil, false)
				if err != nil || out.Flat != nil || out.Draft.Revision != 0 || strings.Contains(receiptText(r), "create_version") {
					t.Fatalf("captured model lost: %+v %v", out, err)
				}
				if change == "deleted" {
					_, _, err := (&tools{svc: e.svc}).saved(ctx, "receipt", nil, v, core.ErrUnavailable, true)
					if err == nil || !strings.Contains(err.Error(), "check get_flat and get_logs") || strings.Contains(err.Error(), "Draft") || strings.Contains(err.Error(), "no document content was saved") {
						t.Fatalf("unknown post-write state reported as no save: %v", err)
					}
				}
			}
			r, out, err := (&tools{svc: e.svc}).savedDocument(ctx, "receipt", "index.md", v, nil)
			if err != nil || r.IsError || out.Document != "index.md" || out.Draft.Revision != 0 {
				t.Fatalf("committed save reported failure/wrong model: %+v %v", out, err)
			}
			text := receiptText(r)
			if strings.Contains(text, "Draft") || strings.Contains(text, "create_version") || strings.Contains(text, `"version":{`) {
				t.Fatalf("document receipt advertises Version actions: %s", text)
			}
			if change == "malformed-manifest" {
				if out.Entry != "" || !strings.Contains(text, "Saved path: index.md") {
					t.Fatalf("failed display decode was not omitted: %s %+v", text, out)
				}
			} else if out.Entry != "index.md" || out.Flat != nil || !strings.Contains(text, "do not repeat this save") || !strings.Contains(text, `"doc":"index.md"`) {
				t.Fatalf("captured document path/entry lost: %s %+v", text, out)
			}
		})
	}
}

func TestDocumentQuotaRefusalsUseDocumentModel(t *testing.T) {
	for _, mode := range []string{"single-path", "bundle"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnvRuntime(t)
			if _, err := e.svc.UpdateSettings(context.Background(), map[string]string{core.SetDiskQuotaBytes: "1"}); err != nil {
				t.Fatal(err)
			}
			args := map[string]any{"slug": "overquota", "markdown": "# Too large"}
			if mode == "bundle" {
				args = map[string]any{"slug": "overquota", "replace_bundle": true, "files": []any{
					file("index.md", "# Too large", ""), file("flats.json", `{"type":"docs"}`, ""),
				}}
			}
			text, failed := call(t, e.local, "save_document", args, nil)
			if !failed || !strings.Contains(text, "disk quota") || !strings.Contains(text, "no document content was saved") || strings.Contains(text, "Draft") || strings.Contains(text, "old versions") || strings.Contains(text, "create_version") {
				t.Fatalf("misleading document quota refusal: %s", text)
			}
		})
	}
}

func TestDocumentSaveRequiresExplicitContent(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "remote"}[remote], func(t *testing.T) {
			e := newEnvRuntime(t)
			client := e.local
			if remote {
				client = e.remote
			}
			ctx := context.Background()
			raw := func(args map[string]any) (*mcp.CallToolResult, error) {
				return client.CallTool(ctx, &mcp.CallToolParams{Name: "save_document", Arguments: args})
			}
			if r, err := raw(map[string]any{"slug": "required", "markdown": "# Keep"}); err != nil || r.IsError {
				t.Fatalf("seed document: %+v %v", r, err)
			}
			before, err := e.st.GetDraft(ctx, "required")
			if err != nil {
				t.Fatal(err)
			}
			files := []any{file("index.md", "# Bundle", ""), file("flats.json", `{"type":"docs"}`, "")}
			for _, args := range []map[string]any{
				{"slug": "required"},
				{"slug": "required", "markdown": nil},
				{"slug": "required", "files": files},
				{"slug": "required", "replace_bundle": true},
				{"slug": "required", "replace_bundle": true, "files": []any{}},
				{"slug": "required", "files": []any{}, "markdown": ""},
				{"slug": "required", "replace_bundle": true, "files": files, "markdown": ""},
			} {
				// Raw SDK calls deliberately bypass the fixture's convenience
				// helper so absence/null/empty values reach the public schema.
				r, callErr := raw(args)
				after, sourceErr := e.st.GetDraft(ctx, "required")
				doc, liveErr := e.svc.GetDocument(ctx, "required", "index.md")
				if callErr == nil && (r == nil || !r.IsError) {
					t.Fatalf("accepted missing or mixed content: %+v; live text %q (error %v), source revision %d -> %d", args, doc.Markdown, liveErr, before.Revision, after.Revision)
				}
				if sourceErr != nil || after.Hash != before.Hash || after.Revision != before.Revision {
					t.Fatalf("refusal changed saved source: %+v %v", after, sourceErr)
				}
				if liveErr != nil || doc.Markdown != "# Keep" {
					t.Fatalf("refusal changed live document: %+v %v", doc, liveErr)
				}
			}
			// Presence, rather than nonempty text, is required: clearing is valid
			// only when the client explicitly supplies the empty string.
			if r, err := raw(map[string]any{"slug": "required", "markdown": ""}); err != nil || r.IsError {
				t.Fatalf("explicit empty text refused: %+v %v", r, err)
			}
			if doc, err := e.svc.GetDocument(ctx, "required", "index.md"); err != nil || doc.Markdown != "" {
				t.Fatalf("intentional clear did not apply: %+v %v", doc, err)
			}
			if r, err := raw(map[string]any{"slug": "required", "replace_bundle": true, "files": files}); err != nil || r.IsError {
				t.Fatalf("explicit files-only bundle refused: %+v %v", r, err)
			}
			if doc, err := e.svc.GetDocument(ctx, "required", "index.md"); err != nil || doc.Markdown != "# Bundle" {
				t.Fatalf("explicit bundle did not apply: %+v %v", doc, err)
			}
		})
	}
}
