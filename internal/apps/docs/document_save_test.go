package docs

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/mcpx"
	"github.com/gosuda/flats/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Exercise the MCP surface through the real QuickJS/Yjs app: source snapshots
// alone cannot detect a sibling's merge base advancing past human edits.
func TestHostSingleDocumentSavePreservesCollaborationAndRevisions(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, private, _ := hostService(t)
	manifest := []byte("{\n \"type\":\"docs\", \"name\":\"Shared\", \"entry\":\"index.md\", \"screenshot\":\"images/chart.png\"\n}\n")
	image := []byte{0, 255, 3, 128}
	if _, err := s.SaveVersion(ctx, "host-doc", []bundle.File{{Path: "flats.json", Data: manifest}, {Path: "index.md", Data: []byte(initialText)}, {Path: "guide.md", Data: []byte("# Guide\n")}, {Path: "images/chart.png", Data: image}}, core.SaveMeta{}, core.ViaMCP); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(private.handler("host-doc"))
	defer srv.Close()
	server := httptest.NewServer(mcpx.Handler(s, mcpx.Options{Version: "test"}))
	defer server.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "preserve-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	save := func(path, text, message string) {
		t.Helper()
		if out, failed := callTool(t, session, "save_document", map[string]any{"slug": "host-doc", "path": path, "markdown": text, "message": message}, nil); failed {
			t.Fatal(out)
		}
	}
	live := func(path string) string {
		t.Helper()
		d, err := s.GetDocument(ctx, "host-doc", path)
		if err != nil {
			t.Fatal(err)
		}
		return d.Markdown
	}
	revs := func() []store.Revision {
		t.Helper()
		p, err := s.ListRevisions(ctx, "host-doc", 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		return p.Revisions
	}
	file := func(n int, path string) []byte {
		t.Helper()
		b, _, err := s.RevisionFileContent(ctx, "host-doc", n, path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	checkBundle := func(n int, docs map[string]string) {
		t.Helper()
		_, files, err := s.GetRevision(ctx, "host-doc", n)
		if err != nil || len(files) != len(docs)+2 {
			t.Fatalf("revision %d census: %+v %v", n, files, err)
		}
		for p, want := range docs {
			if got := string(file(n, p)); got != want {
				t.Fatalf("revision %d %s = %q; want %q", n, p, got, want)
			}
		}
		if !bytes.Equal(file(n, "flats.json"), manifest) || !bytes.Equal(file(n, "images/chart.png"), image) {
			t.Fatal("revision lost asset/config")
		}
	}
	editor := connect(t, srv.URL, "private")
	editor.hello(t, nil)
	edit(t, editor, "human", fixture(t).Independent)
	editor.Close()
	human := live("index.md")
	if human == initialText {
		t.Fatal("human fixture made no change")
	}
	// Save B while A has human changes, then submit a source-based independent
	// edit to A. Copying live A into B's bundle would lose those human changes.
	save("guide.md", "# Updated guide\n", "save sibling")
	if got := live("index.md"); got != human {
		t.Fatalf("sibling save changed human text: %q", got)
	}
	r := revs()
	if r[0].Kind != "agent" || r[0].Message != "save sibling" || r[1].Kind != "person" {
		t.Fatalf("revision boundaries %+v", r)
	}
	checkBundle(r[0].Number, map[string]string{"index.md": human, "guide.md": "# Updated guide\n"})
	before := r[0].Number
	agent := strings.Replace(initialText, "Agent line", "Agent updated", 1)
	save("index.md", agent, "merge independent")
	combined := strings.Replace(human, "Agent line", "Agent updated", 1)
	if got := live("index.md"); got != combined {
		t.Fatalf("lost independent edit: %q; want %q", got, combined)
	}
	r = revs()
	checkBundle(r[0].Number, map[string]string{"index.md": combined, "guide.md": "# Updated guide\n"})
	// A live edit and a source save change the same line: the save wins the
	// overlap, retaining the independent human prefix and the overwritten
	// complete live text in the preceding revision.
	humanOverlap := "Human overlap"
	if _, err := s.UpdateDocument(ctx, "host-doc", "index.md", []core.DocumentEditOp{{Op: "replace", Find: "Agent updated", With: &humanOverlap}}, nil, core.ViaMCP); err != nil {
		t.Fatal(err)
	}
	overwritten := strings.Replace(combined, "Agent updated", humanOverlap, 1)
	save("index.md", strings.Replace(agent, "Agent updated", "Saved overlap", 1), "overwrite")
	if got := live("index.md"); got != strings.Replace(combined, "Agent updated", "Saved overlap", 1) {
		t.Fatal(got)
	}
	r = revs()
	checkBundle(r[1].Number, map[string]string{"index.md": overwritten, "guide.md": "# Updated guide\n"})

	// Restore the preserved whole flat, then add a document and save A again.
	// A sibling save must not undo rollback text or steal the entry document.
	if out, failed := callTool(t, session, "rollback_revision", map[string]any{"slug": "host-doc", "revision": before}, nil); failed {
		t.Fatal(out)
	}
	if got := live("index.md"); got != human {
		t.Fatal("rollback", got)
	}
	save("new/chapter.markdown", "# New\n한글 😀", "add document")
	if got := live(""); got != human {
		t.Fatal("new document changed entry/rollback", got)
	}
	afterRollback := strings.Replace(human, "Base line", "Base line after restore", 1)
	save("index.md", afterRollback, "after restore")
	if got := live("index.md"); got != afterRollback {
		t.Fatal("save after restore", got)
	}
	r = revs()
	checkBundle(r[0].Number, map[string]string{"index.md": afterRollback, "guide.md": "# Updated guide\n", "new/chapter.markdown": "# New\n한글 😀"})
	// Refused input does not change the live flat or create a revision.
	count := len(r)
	if out, failed := callTool(t, session, "save_document", map[string]any{"slug": "host-doc", "path": "_docs/bad.md", "markdown": "bad"}, nil); !failed {
		t.Fatal(out)
	}
	if got := revs(); len(got) != count || live("index.md") != afterRollback {
		t.Fatal("failed save mutated content/history")
	}
	// Two callers start together; both changes and both revision boundaries
	// must survive the serialized read/modify/activate operation.
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, path := range []string{"parallel-a.md", "parallel-b.md"} {
		wg.Go(func() {
			<-start
			_, err := s.SaveDocument(ctx, "host-doc", path, "# "+path, "", core.SaveMeta{}, core.ViaMCP)
			errs <- err
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := revs(); len(got) != count+2 {
		t.Fatalf("concurrent revision boundaries: %d, want %d", len(got), count+2)
	}
	if live("parallel-a.md") != "# parallel-a.md" || live("parallel-b.md") != "# parallel-b.md" {
		t.Fatal("concurrent save lost a sibling")
	}

}
