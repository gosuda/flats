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
func approveHost(t *testing.T, s *core.Service, err error) {
	t.Helper()
	var pending *core.PendingApproval
	if !errors.As(err, &pending) {
		t.Fatal("expected approval", err)
	}
	if _, err := s.Decide(t.Context(), pending.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
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
func TestHostDraftPreviewAndRevocation(t *testing.T) {
	s, private, _ := hostService(t)
	p, err := s.OpenPreview(t.Context(), "host-doc", 0)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(private.handler(p.Host))
	defer srv.Close()
	x := connect(t, srv.URL, "public") // forged access is overwritten by host
	if welcome := x.hello(t, nil); welcome["readonly"] != false {
		t.Fatal(welcome)
	}
	resp, err := http.Get(srv.URL + "/_docs/api/document")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("Draft generation 0 rejected", resp.StatusCode)
	}
	if err := s.ClosePreview(t.Context(), p.Host); err != nil {
		t.Fatal(err)
	}
	closedSocket(t, x)
	resp, err = http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatal("stale preview handler admitted request", resp.StatusCode)
	}
}
func TestHostPublicSocketRevocation(t *testing.T) {
	for _, action := range []string{"private", "delete", "rename"} {
		t.Run(action, func(t *testing.T) {
			s, private, public := hostService(t)
			_, err := s.Deploy(t.Context(), "host-doc", 0, core.ViaMCP)
			approveHost(t, s, err)
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

func TestHostAPIAndMCPConflictRecovery(t *testing.T) {
	s, private, _ := hostService(t)
	_, err := s.Deploy(t.Context(), "host-doc", 0, core.ViaMCP)
	approveHost(t, s, err)
	srv := httptest.NewServer(private.handler("host-doc"))
	defer srv.Close()
	editor := connect(t, srv.URL, "private")
	editor.hello(t, nil)
	edit(t, editor, "human", fixture(t).Independent)
	editor.Close()
	preserved, err := s.GetDocument(t.Context(), "host-doc", "")
	if err != nil {
		t.Fatal(err)
	}
	target := strings.Repeat("rewritten\n", 200)
	_, err = s.SaveVersion(t.Context(), "host-doc", []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte(target)}}, core.SaveMeta{}, core.ViaMCP)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Deploy(t.Context(), "host-doc", 0, core.ViaMCP)
	approveHost(t, s, err)
	live, err := s.GetDocument(t.Context(), "host-doc", "")
	if err != nil || len(live.Conflicts) != 1 || live.Conflicts[0].Bytes != int64(len(preserved.Markdown)) {
		t.Fatal(live, err)
	}
	generation := live.Conflicts[0].Generation
	recovered, err := s.GetDocumentConflict(t.Context(), "host-doc", "", generation)
	if err != nil || recovered.Markdown != preserved.Markdown || recovered.Source != "conflict" || recovered.Generation != generation {
		t.Fatal(recovered, err)
	}
	h := (&api.Server{Svc: s}).Handler()
	for _, query := range []string{"", fmt.Sprintf("?conflict=%d", generation)} {
		req := httptest.NewRequest("GET", "/api/flats/host-doc/document"+query, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatal(rec.Code, rec.Body.String())
		}
		var out core.Document
		if err = json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if query == "" {
			if len(out.Conflicts) != 1 || out.Conflicts[0].Bytes != int64(len(preserved.Markdown)) || strings.Contains(rec.Body.String(), `"markdown":""`) {
				t.Fatal(rec.Body.String())
			}
		} else if out.Markdown != preserved.Markdown {
			t.Fatal(out)
		}
	}
	mcpServer := httptest.NewServer(mcpx.Handler(s, mcpx.Options{Version: "test"}))
	defer mcpServer.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "conflict-test", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: mcpServer.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, conflict := range []int64{0, generation, generation + 999} {
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_document", Arguments: map[string]any{"slug": "host-doc", "conflict": conflict}})
		if err != nil {
			t.Fatal(err)
		}
		if conflict == generation+999 {
			if !result.IsError {
				t.Fatal("missing generation accepted")
			}
			continue
		}
		if result.IsError {
			t.Fatal(result)
		}
		data, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		var out core.Document
		if err = json.Unmarshal(data, &out); err != nil {
			t.Fatal(err)
		}
		if conflict == 0 {
			if len(out.Conflicts) != 1 || out.Conflicts[0].Bytes != int64(len(preserved.Markdown)) {
				t.Fatal(string(data))
			}
		} else if out.Markdown != preserved.Markdown || out.Generation != generation {
			t.Fatal(string(data))
		}
	}
}
