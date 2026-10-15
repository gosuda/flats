package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/mcpx"
	"github.com/gosuda/flats/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Capture real host handlers without contacting any exposure provider.
type captureNet struct {
	mu       sync.Mutex
	handlers map[string]http.Handler
}

func (n *captureNet) Serve(_ context.Context, host string, h http.Handler, _ bool) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.handlers[host] = h
	return n.URL(host), nil
}
func (n *captureNet) Stop(host string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.handlers, host)
	return nil
}
func (n *captureNet) handler(host string) http.Handler {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.handlers[host]
}
func (*captureNet) SetHidden(string, bool) error { return nil }
func (*captureNet) URL(host string) string       { return "http://" + host + ".localhost" }
func (n *captureNet) Status() core.NetStatus {
	n.mu.Lock()
	defer n.mu.Unlock()
	status := core.NetStatus{Enabled: true}
	for host := range n.handlers {
		status.Hosts = append(status.Hosts, core.HostInfo{Host: host, URL: n.URL(host), State: "ready"})
	}
	return status
}
func (*captureNet) Close() error { return nil }

func hostService(t *testing.T) (*core.Service, *captureNet, *captureNet) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "host.db"))
	if err != nil {
		t.Fatal(err)
	}
	private, public := &captureNet{handlers: map[string]http.Handler{}}, &captureNet{handlers: map[string]http.Handler{}}
	s, err := core.New(t.Context(), core.Config{DataDir: dir, Store: st, Private: private, Public: public,
		Runtime: manager(t), DocsApp: core.DocsApp{FS: FS(), Hash: Hash(), Entry: Entry, ContentModule: ContentModule},
		Logf: t.Logf})
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); st.Close() })
	_, err = s.SaveVersion(t.Context(), "host-doc", []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte(initialText)}}, core.SaveMeta{}, core.ViaMCP)
	if err != nil {
		t.Fatal(err)
	}
	return s, private, public
}
func approveAction(t *testing.T, s *core.Service, action core.ActionResult, err error) {
	t.Helper()
	if err != nil || action.Approval == nil {
		t.Fatal(action, err)
	}
	if _, err := s.Decide(t.Context(), action.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
}
func closedSocket(t *testing.T, x *socket) {
	t.Helper()
	x.SetReadDeadline(time.Now().Add(3 * time.Second))
	var b [2]byte
	_, err := x.reader.Read(b[:])
	if err == nil && b[0]&15 != 8 {
		t.Fatal("connection remains open", b)
	}
	if e, ok := err.(interface{ Timeout() bool }); ok && e.Timeout() {
		t.Fatal("revoked upgraded connection did not close")
	}
}
func TestHostSavedDocumentRunsAtOnce(t *testing.T) {
	t.Parallel()
	s, private, _ := hostService(t)
	if _, err := s.OpenPreview(t.Context(), "host-doc", 0); !errors.Is(err, core.ErrDocsUnversioned) {
		t.Fatal("document preview:", err)
	}
	if as, _ := s.ListApprovals(t.Context(), "pending"); len(as) != 0 {
		t.Fatal("document save requested approval", as)
	}
	srv := httptest.NewServer(private.handler("host-doc"))
	defer srv.Close()
	x := connect(t, srv.URL, "public") // forged access is overwritten by host
	if welcome := x.hello(t, nil); welcome["readonly"] != false {
		t.Fatal(welcome)
	}
	x.Close()
	resp, err := http.Get(srv.URL + "/_docs/api/document")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("saved document is not running", resp.StatusCode)
	}
}
func TestHostPublicSocketRevocation(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"private", "delete", "rename"} {
		t.Run(action, func(t *testing.T) {
			s, private, public := hostService(t)
			if err := s.SetProviderPermission(t.Context(), "host-doc", store.ProviderPortal, true, core.ViaConsole); err != nil {
				t.Fatal(err)
			}
			res, err := s.SetVisibility(t.Context(), "host-doc", store.Public, core.ViaMCP, "test")
			approveAction(t, s, res, err)
			publicSrv := httptest.NewServer(public.handler("host-doc"))
			defer publicSrv.Close()
			privateSrv := httptest.NewServer(private.handler("host-doc"))
			defer privateSrv.Close()
			x, y := connect(t, publicSrv.URL, "private"), connect(t, privateSrv.URL, "public")
			if w := x.hello(t, nil); w["readonly"] != true {
				t.Fatal(w)
			}
			if w := y.hello(t, nil); w["readonly"] != false {
				t.Fatal(w)
			}
			switch action {
			case "private":
				res, err = s.SetVisibility(t.Context(), "host-doc", store.Private, core.ViaMCP, "test")
				approveAction(t, s, res, err)
			case "delete":
				res, err = s.Delete(t.Context(), "host-doc", core.ViaMCP, "test")
				approveAction(t, s, res, err)
			case "rename":
				_, err = s.RenameSlug(t.Context(), "host-doc", "renamed-doc", core.ViaMCP)
				if err != nil {
					t.Fatal(err)
				}
			}
			closedSocket(t, x)
			if action == "private" {
				y.send(t, map[string]any{"t": "ping"})
				y.next(t, "pong") // independent private admission still works
			}
			x.Close()
			y.Close()
		})
	}
}

func TestHostDocumentRevisions(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, private, public := hostService(t)
	srv := httptest.NewServer(private.handler("host-doc"))
	defer srv.Close()
	revs := func() []store.Revision {
		t.Helper()
		page, err := s.ListRevisions(ctx, "host-doc", 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		return page.Revisions
	}
	text := func(n int, path string) string {
		t.Helper()
		b, _, err := s.RevisionFileContent(ctx, "host-doc", n, path)
		if err != nil {
			t.Fatal(n, path, err)
		}
		return string(b)
	}
	if r := revs(); len(r) != 1 || r[0].Kind != "created" || text(1, "index.md") != initialText {
		t.Fatalf("first revision %+v", r)
	}
	// A person edits; the next agent save keeps their text as a revision of
	// its own before it overwrites the lines it changes.
	editor := connect(t, srv.URL, "private")
	editor.hello(t, nil)
	edit(t, editor, "human", fixture(t).Independent)
	editor.Close()
	preserved, err := s.GetDocument(ctx, "host-doc", "")
	if err != nil {
		t.Fatal(err)
	}
	target := strings.Repeat("rewritten\n", 200)
	if _, err = s.SaveVersion(ctx, "host-doc", []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte(target)}}, core.SaveMeta{Message: "rewrite"}, core.ViaMCP); err != nil {
		t.Fatal(err)
	}
	r := revs()
	if len(r) != 3 || r[0].Kind != "agent" || r[0].Message != "rewrite" || len(r[0].Authors) != 1 || r[0].Authors[0] != "Agent" || r[1].Kind != "person" {
		t.Fatalf("revisions after the save %+v", r)
	}
	if text(2, "index.md") != preserved.Markdown || text(3, "index.md") != target {
		t.Fatal("revision texts")
	}
	if rev, files, err := s.GetRevision(ctx, "host-doc", 2); err != nil || len(files) != 2 || files[0].Path != "flats.json" || rev.Files != 2 {
		t.Fatal("revision files", files, err)
	}

	// Rolling back within the running version restores the exact text.
	mcpServer := httptest.NewServer(mcpx.Handler(s, mcpx.Options{Version: "test"}))
	defer mcpServer.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "revision-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: mcpServer.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	var done struct {
		Revision int `json:"revision"`
	}
	if out, failed := callTool(t, session, "rollback_revision", map[string]any{"slug": "host-doc", "revision": 2}, &done); failed || done.Revision != 4 {
		t.Fatal(out)
	}
	if live, _ := s.GetDocument(ctx, "host-doc", ""); live.Markdown != preserved.Markdown {
		t.Fatal("rollback did not restore the text", live.Markdown)
	}
	if r := revs(); r[0].Kind != "rollback" || r[0].RollbackOf != 2 || text(4, "index.md") != preserved.Markdown {
		t.Fatalf("rollback revision %+v", r[0])
	}
	// A later save merges against the restored text: its own change lands
	// and the restored lines it does not touch stay.
	next := strings.Replace(preserved.Markdown, "Base line", "Base line, saved after the rollback", 1)
	if _, err = s.SaveVersion(ctx, "host-doc", []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte(next)}}, core.SaveMeta{}, core.ViaMCP); err != nil {
		t.Fatal(err)
	}
	if live, _ := s.GetDocument(ctx, "host-doc", ""); live.Markdown != next {
		t.Fatal("save after rollback", live.Markdown)
	}

	// A save that adds a document and an asset, then a rollback across that
	// change: a new version without them, restored exactly.
	before := revs()[0].Number
	if _, err = s.SaveVersion(ctx, "host-doc", []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte(next)}, {Path: "more.md", Data: []byte("# More\n")}, {Path: "notes.txt", Data: []byte("asset")}}, core.SaveMeta{}, core.ViaMCP); err != nil {
		t.Fatal(err)
	}
	get := func(path string) int {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if get("/notes.txt") != 200 || get("/more") != 200 {
		t.Fatal("added files not served")
	}
	if out, failed := callTool(t, session, "rollback_revision", map[string]any{"slug": "host-doc", "revision": before}, &done); failed {
		t.Fatal(out)
	}
	if get("/notes.txt") == 200 || get("/more") == 200 {
		t.Fatal("rollback kept files the revision does not have")
	}
	if live, _ := s.GetDocument(ctx, "host-doc", ""); live.Markdown != next {
		t.Fatal("rollback across versions", live.Markdown)
	}
	if r := revs(); r[0].Kind != "rollback" || r[0].RollbackOf != before || r[0].Files != 2 {
		t.Fatalf("rollback revision %+v", r[0])
	}

	// The page's routes: Private only, with write protection.
	call := func(h http.Handler, method, path string, headers map[string]string, body string) (int, string) {
		req := httptest.NewRequest(method, "http://host-doc.localhost"+path, strings.NewReader(body))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, body := call(private.handler("host-doc"), "GET", "/_docs/api/revisions", nil, ""); code != 200 || !strings.Contains(body, `"rollback_of"`) {
		t.Fatal("private history", code, body)
	}
	// A revision names the one before it, which the page compares it with.
	if code, body := call(private.handler("host-doc"), "GET", "/_docs/api/revisions/3", nil, ""); code != 200 || !strings.Contains(body, `"previous":2`) {
		t.Fatal("revision view", code, body)
	}
	if code, body := call(private.handler("host-doc"), "GET", "/_docs/api/revisions/1", nil, ""); code != 200 || !strings.Contains(body, `"previous":0`) {
		t.Fatal("first revision view", code, body)
	}
	if code, body := call(private.handler("host-doc"), "GET", "/_docs/api/revisions/2/document?doc=index.md", nil, ""); code != 200 || !strings.Contains(body, "Person") {
		t.Fatal("revision document", code, body)
	}
	write := map[string]string{"Content-Type": "application/json", "X-Flats-Docs": "1", "Sec-Fetch-Site": "same-origin"}
	if code, _ := call(private.handler("host-doc"), "POST", "/_docs/api/revisions/2/pin", map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "same-origin"}, `{"pinned":true}`); code != 403 {
		t.Fatal("pin without the page header", code)
	}
	if code, _ := call(private.handler("host-doc"), "POST", "/_docs/api/revisions/2/pin", map[string]string{"Content-Type": "application/json", "X-Flats-Docs": "1", "Sec-Fetch-Site": "cross-site"}, `{"pinned":true}`); code != 403 {
		t.Fatal("cross-site pin", code)
	}
	if code, body := call(private.handler("host-doc"), "POST", "/_docs/api/revisions/2/pin", write, `{"pinned":true}`); code != 200 {
		t.Fatal("pin", code, body)
	}
	if rev, _, _ := s.GetRevision(ctx, "host-doc", 2); !rev.Pinned {
		t.Fatal("not pinned")
	}
	// Host operations of the worker stay out of reach of browsers.
	if code, _ := call(private.handler("host-doc"), "GET", "/_docs/host/revisions/outbox", map[string]string{"X-Flats-Host-Op": "revisions"}, ""); code != 403 {
		t.Fatal("worker host operation reachable", code)
	}
	if err := s.SetProviderPermission(ctx, "host-doc", store.ProviderPortal, true, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	res, err := s.SetVisibility(ctx, "host-doc", store.Public, core.ViaMCP, "test")
	approveAction(t, s, res, err)
	if code, _ := call(public.handler("host-doc"), "GET", "/_docs/api/revisions", nil, ""); code != 404 {
		t.Fatal("public history", code)
	}
	// get_document has no conflict records any more.
	h := (&api.Server{Svc: s}).Handler()
	req := httptest.NewRequest("GET", "/api/flats/host-doc/document", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "conflicts") {
		t.Fatal(rec.Code, rec.Body.String())
	}
	// With a revision quota of 1, a rollback still finds and keeps its
	// target while the history it imports first would push it out.
	one := 1
	if err := s.SetRevisionQuotas(ctx, "host-doc", &one, nil); err != nil {
		t.Fatal(err)
	}
	quotaTarget := revs()[0].Number
	editor = connect(t, srv.URL, "private")
	editor.hello(t, nil)
	// A change no revision holds yet: the rollback imports it first.
	quotaEdit := fixture(t).Compact[0]
	edit(t, editor, quotaEdit.ID, quotaEdit.U)
	editor.Close()
	if out, failed := callTool(t, session, "rollback_revision", map[string]any{"slug": "host-doc", "revision": quotaTarget}, &done); failed {
		t.Fatal("rollback under quota 1", out)
	}
	if live, _ := s.GetDocument(ctx, "host-doc", ""); live.Markdown != text(revs()[0].Number, "index.md") {
		t.Fatal("rollback under quota 1 did not restore the text")
	}
	// Only the rollback stays unpinned; revision 2 is pinned.
	if r := revs(); len(r) != 2 || r[0].Kind != "rollback" || r[0].RollbackOf != quotaTarget || !r[1].Pinned {
		t.Fatalf("history under quota 1 %+v", r)
	}
	if holds, _ := s.RevisionQuotas(ctx, "host-doc"); holds.RevisionQuota == nil {
		t.Fatal("quota override lost")
	}
	if err := s.SetRevisionQuotas(ctx, "host-doc", nil, nil); err != nil {
		t.Fatal(err)
	}

	_ = json.Valid
	_ = fmt.Sprint
}
