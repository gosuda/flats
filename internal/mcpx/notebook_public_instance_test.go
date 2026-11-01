package mcpx

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
)

func TestNotebookPublicInstancePrivateLinks(t *testing.T) {
	e := newEnv(t)
	view, err := e.svc.CreateNotebook(t.Context(), "private-notes", "Notes", core.ViaMCP)
	if err != nil {
		t.Fatal(err)
	}
	initial := integratedNotebookGet(t, e, view.Slug, map[string]any{})
	ws := integratedNotebookDial(t, view.PrivateURL, initial.Tree[0].Ref.ID, "private")
	publicInstanceOn(t, e)
	if err := ws.NetConn().SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		_, err := ws.ReadMessage(1 << 20)
		if err == nil {
			continue
		}
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			t.Fatal("turning on public instance mode left the private notebook socket open")
		}
		break
	}
	for _, path := range []string{"/", "/_notebook/ws", "/_notebook/history"} {
		status, _, body := integratedNotebookHTTP(t, view.PrivateURL, http.MethodGet, path, nil)
		if status != http.StatusForbidden || !strings.Contains(string(body), "Private links are turned off") {
			t.Fatalf("private %s = %d %s", path, status, body)
		}
	}
	if hidden, err := e.svc.GetFlat(t.Context(), view.Slug); err != nil || hidden.PrivateURL != "" || !hidden.PrivateLinksOff || hidden.ConnectionState != "unavailable" {
		t.Fatalf("private notebook in public instance mode = %+v, %v", hidden, err)
	}
	if _, err := e.svc.SetPublicInstance(t.Context(), false, "", "test"); err != nil {
		t.Fatal(err)
	}
	status, _, _ := integratedNotebookHTTP(t, view.PrivateURL, http.MethodGet, "/", nil)
	if status != http.StatusOK {
		t.Fatalf("private notebook did not return after turn off: %d", status)
	}
}

func TestNotebookPublicInstanceMCP(t *testing.T) {
	e := newEnv(t)
	publicInstanceOn(t, e)
	cs := connect(t, e.localURL)
	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "update_notebook" && !strings.Contains(tool.Description, publicCreationNote) {
			t.Fatal("notebook first-save creation omits public instance consent")
		}
	}
	var created FlatInfo
	text, failed := call(t, cs, "create_flat", map[string]any{"slug": "public-notes", "type": "notebook"}, &created)
	if failed || created.Visibility != string(store.Public) || !created.Running || created.PrivateURL != "" || strings.Contains(text, "Private URL") || !strings.Contains(text, core.PublicAccessNotice) {
		t.Fatalf("public notebook creation = %+v, %s", created, text)
	}
	var guide GuideOut
	text, failed = call(t, cs, "guide", map[string]any{"items": []string{"topic.notebook", "topic.index", "topic.instructions"}}, &guide)
	if failed || strings.Contains(text, "A new notebook is Private") || strings.Contains(text, "`private_url`") || strings.Contains(text, "each is Private") || !strings.Contains(text, "Browser History is disabled") {
		t.Fatalf("notebook public instance guidance: %s", text)
	}
}
