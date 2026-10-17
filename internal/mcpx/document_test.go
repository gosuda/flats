package mcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	runtimeref "github.com/gosuda/flats/docs"
	"github.com/gosuda/flats/internal/core"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// documentRuntime is a fake docs app: it serves the entry document of the
// content module it was started with as live text.
type documentRuntime struct{}

type documentInstance struct{ http.Handler }

func (documentInstance) Stop() {}

func (documentRuntime) Start(_ context.Context, spec core.RuntimeSpec) (core.Instance, error) {
	b, err := os.ReadFile(filepath.Join(spec.Dir, testDocsApp().ContentModule))
	if err != nil {
		return nil, err
	}
	var content struct {
		Entry     string `json:"entry"`
		Documents []struct {
			Path string `json:"path"`
			Text string `json:"text"`
		} `json:"documents"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(string(b), "export default "), ";\n")), &content); err != nil {
		return nil, err
	}
	return documentInstance{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_docs/api/") && r.URL.Path != "/_docs/api/document" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path != "/_docs/api/document" {
			fmt.Fprint(w, "ok")
			return
		}
		doc := r.URL.Query().Get("doc")
		if doc == "" {
			doc = content.Entry
		}
		for _, d := range content.Documents {
			if d.Path == doc {
				_ = json.NewEncoder(w).Encode(core.Document{Format: 1, Doc: d.Path, Markdown: d.Text, Source: "live", Seq: 1})
				return
			}
		}
		http.NotFound(w, r)
	})}, nil
}

func testDocsApp() core.DocsApp {
	return core.DocsApp{FS: fstest.MapFS{"server.js": &fstest.MapFile{Data: []byte("export default {}")}}, Hash: strings.Repeat("a", 64), Entry: "server.js", ContentModule: "content.js"}
}

func TestDocumentTools(t *testing.T) {
	e := newEnvRuntime(t)
	for name, client := range map[string]*mcp.ClientSession{"local": e.local, "remote": e.remote} {
		t.Run(name, func(t *testing.T) {
			slug := "doc-" + name
			for i, path := range []string{"", "notes/chapter.markdown"} {
				var saved saveFixtureOut
				text, failed := call(t, client, "save_document", map[string]any{"slug": slug, "markdown": "# Hello\n한글 😀", "title": "Title", "path": path, "message": "edit"}, &saved)
				if failed || saved.Flat == nil || saved.Flat.Type != "docs" || !saved.Flat.Running || saved.Flat.Visibility != "private" || saved.Flat.LiveVersion != 0 || saved.Flat.Publication != "" || saved.Flat.Draft != nil || saved.Draft.Revision != 0 || saved.Deploy != nil || !strings.Contains(text, "running now") {
					t.Fatalf("save %d: %s %+v", i, text, saved)
				}
				wantPath := path
				if wantPath == "" {
					wantPath = "index.md"
				}
				if saved.Document != wantPath || saved.Entry != "index.md" || !strings.Contains(text, wantPath) || !strings.Contains(text, "Entry document: index.md") {
					t.Fatalf("saved path/entry missing: %s %+v", text, saved)
				}
				if strings.Contains(text, `"draft":{`) || strings.Contains(text, `"version":{`) {
					t.Fatalf("document save reports a Draft or version: %s", text)
				}
				var out core.Document
				text, failed = call(t, client, "get_document", map[string]any{"slug": slug, "doc": map[bool]string{true: "index.md", false: path}[path == ""]}, &out)
				if failed || out.Source != "live" || out.Markdown != "# Hello\n한글 😀" {
					t.Fatalf("read: %s %+v", text, out)
				}
				approvals, _ := e.svc.ListApprovals(context.Background(), "pending")
				if len(approvals) != 0 {
					t.Fatal("document save requested approval")
				}
				for _, tool := range []struct {
					name string
					args map[string]any
				}{{"list_versions", nil}, {"get_draft", nil}, {"create_version", nil}, {"deploy", map[string]any{"version": 0}}, {"rollback", nil}, {"open_preview", map[string]any{"version": 0}}} {
					args := map[string]any{"slug": slug}
					for k, v := range tool.args {
						args[k] = v
					}
					if text, failed := call(t, client, tool.name, args, nil); !failed || !strings.Contains(text, "this flat is a document") {
						t.Fatalf("%s on a document: %s", tool.name, text)
					}
				}
			}
			var created FlatInfo
			text, failed := call(t, client, "create_flat", map[string]any{"slug": "new-" + name, "name": "Meeting notes", "type": "docs"}, &created)
			if failed || created.Type != "docs" || !created.Running || created.Visibility != "private" || !strings.Contains(text, "running now") {
				t.Fatalf("create document: %s %+v", text, created)
			}
			var fresh core.Document
			if text, failed := call(t, client, "get_document", map[string]any{"slug": "new-" + name}, &fresh); failed || fresh.Markdown != "# Meeting notes\n" {
				t.Fatalf("created document text: %s %+v", text, fresh)
			}
			if text, failed := call(t, client, "create_flat", map[string]any{"slug": "new-" + name, "type": "docs"}, nil); !failed {
				t.Fatalf("existing slug: %s", text)
			}
			if text, failed := call(t, client, "create_flat", map[string]any{"slug": "bad-type-" + name, "type": "wiki"}, nil); !failed || !strings.Contains(text, `"category":"invalid"`) {
				t.Fatalf("bad type: %s", text)
			}
			for _, path := range []string{"../x.md", "x.txt", "_docs/a.md"} {
				if text, failed := call(t, client, "save_document", map[string]any{"slug": slug, "markdown": "bad", "path": path}, nil); !failed || !strings.Contains(text, "fix") && !strings.Contains(text, "Hint") {
					t.Fatalf("bad path: %s", text)
				}
			}
			// Conflict records were replaced by revisions.
			if _, failed := call(t, client, "get_document", map[string]any{"slug": slug, "conflict": 4}, nil); !failed {
				t.Fatal("get_document accepts conflict")
			}
			text, failed = call(t, client, "get_document", map[string]any{"slug": slug, "doc": "absent.md"}, nil)
			if !failed || !strings.Contains(text, `"category":"document_not_found"`) || !strings.Contains(text, "omit doc to read the entry") || strings.Contains(text, "Draft") {
				t.Fatalf("missing document or misleading recovery hint: %s", text)
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
			list, err := client.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			descriptions := make(map[string]string)
			for _, tool := range list.Tools {
				descriptions[tool.Name] = strings.ToLower(tool.Description)
				if tool.Name == "get_document" && (tool.Annotations == nil || tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint) {
					t.Fatal("live reads must advertise possible non-destructive activation")
				}
				if tool.Name == "get_content_types" && (!tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint) {
					t.Fatal("reference tool mutates")
				}
			}
			// Draft replacement must disclose omissions without advertising docs
			// activation. Only explicit save_document bundles activate docs.
			for _, tool := range []string{"save_draft", "save_draft_from_dir"} {
				description := descriptions[tool]
				if !strings.Contains(description, "complete") || !strings.Contains(description, "replac") || !strings.Contains(description, "delet") || !strings.Contains(description, "omitted") || !strings.Contains(description, "documents must use save_document") {
					t.Errorf("%s must disclose complete Draft replacement, deletion of omissions and explicit docs route: %s", tool, description)
				}
			}
			if description := descriptions["save_document"]; !strings.Contains(description, "preserving other documents") || !strings.Contains(description, "replace_bundle=true") || !strings.Contains(description, "omitted files are deleted") || !strings.Contains(description, "runs at once") {
				t.Errorf("single-document preservation and explicit live bundle replacement guidance missing: %s", description)
			}

		})
	}
	f, _ := e.svc.ListFlats(context.Background())
	if len(f) != 0 {
		t.Fatal("reference mutated state")
	}
}

// After a lost response the hint depends on whether the edit is live; it
// never tells the caller to repeat an edit that applied.
func TestEditHintFollowsOutcome(t *testing.T) {
	for err, want := range map[error]string{
		&core.EditOutcomeError{Outcome: core.EditApplied, Seq: 7}:               "live at seq 7; do not repeat it",
		&core.EditOutcomeError{Outcome: core.EditNotApplied}:                    "send the same call again",
		&core.EditOutcomeError{Outcome: core.EditUnknown}:                       "check whether the edit is there before retrying",
		fmt.Errorf("%w: too many live edits", core.ErrUnavailable):              "wait a second and retry",
		fmt.Errorf("%w: stale", core.ErrEditConflict):                           "read get_document {slug, doc, blocks: true} again",
		fmt.Errorf("%w: too large", core.ErrDocumentCapacity):                   "split it into several calls",
		fmt.Errorf("%w: %w: full", core.ErrDocumentCapacity, core.ErrDiskQuota): "the operator must raise the per-flat disk quota",
	} {
		if got := editHint(err, "doc"); !strings.Contains(got, want) {
			t.Errorf("%v: hint %q, want %q", err, got, want)
		}
	}
	if h := editHint(&core.EditOutcomeError{Outcome: core.EditApplied, Seq: 7}, "doc"); strings.Contains(h, "retry") {
		t.Fatal("applied edit hint suggests a retry:", h)
	}
}

type documentFailureRuntime struct{ fail atomic.Bool }

func (rt *documentFailureRuntime) Start(ctx context.Context, spec core.RuntimeSpec) (core.Instance, error) {
	if rt.fail.Load() {
		return nil, fmt.Errorf("test startup failure")
	}
	return (documentRuntime{}).Start(ctx, spec)
}
func TestDocumentSaveStartupHints(t *testing.T) {
	t.Parallel()
	rt := &documentFailureRuntime{}
	rt.fail.Store(true)
	e := newEnvWithRuntime(t, rt)
	text, failed := call(t, e.local, "save_document", map[string]any{"slug": "failed-start", "markdown": "initial"}, nil)
	if !failed || !strings.Contains(text, "no document is running yet") || !strings.Contains(text, "repeat this same save") {
		t.Fatal(text)
	}
	rt.fail.Store(false)
	if text, failed := call(t, e.local, "save_document", map[string]any{"slug": "failed-start", "markdown": "initial"}, nil); failed {
		t.Fatal(text)
	}
	rt.fail.Store(true)
	text, failed = call(t, e.local, "save_document", map[string]any{"slug": "failed-start", "path": "new.md", "markdown": "failed new document"}, nil)
	if !failed || !strings.Contains(text, "running document is unchanged") || !strings.Contains(text, "sibling-document save will not carry this failed change") {
		t.Fatal(text)
	}
}
func TestDocumentSaveTextKeepsPublicNoticeSeparate(t *testing.T) {
	text := documentText("Saved document", FlatInfo{Slug: "handbook", Running: true, Visibility: "public", PublicNotice: "Public notice"}, "Saved path: chapter.md. Entry document: README.md.")
	if !strings.HasPrefix(text, "Saved document handbook.") || !strings.Contains(text, "Entry document: README.md.\nNotice: Public notice") {
		t.Fatal(text)
	}
}
