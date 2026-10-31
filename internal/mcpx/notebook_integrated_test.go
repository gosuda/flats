package mcpx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/contenttype"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/notebook"
	"github.com/gosuda/flats/internal/store"
	"github.com/gosuda/flats/internal/wsconn"
)

type integratedNotebookItem struct {
	Ref     notebook.Ref     `json:"ref"`
	Kind    string           `json:"kind"`
	Title   string           `json:"title"`
	Content notebook.Content `json:"content"`
}

type integratedNotebookRead struct {
	State    notebook.State           `json:"state"`
	Revision *int                     `json:"revision"`
	Pending  bool                     `json:"pending"`
	Tree     []integratedNotebookItem `json:"tree"`
	Page     *struct {
		Items []integratedNotebookItem `json:"items"`
	} `json:"page"`
	Next     *string `json:"next"`
	Complete bool    `json:"complete"`
}

func integratedNotebookJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func integratedNotebookGet(t *testing.T, e *env, slug string, query any) integratedNotebookRead {
	t.Helper()
	raw, err := e.svc.GetNotebook(t.Context(), slug, 0, integratedNotebookJSON(t, query))
	if err != nil {
		t.Fatalf("GetNotebook: %v", err)
	}
	var out integratedNotebookRead
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("GetNotebook returned %s: %v", raw, err)
	}
	if !out.Complete || out.Next != nil || out.Pending || out.Revision == nil {
		t.Fatalf("small imported read is incomplete or pending: %s", raw)
	}
	return out
}

func integratedNotebookImage(t *testing.T) ([]byte, string) {
	t.Helper()
	im := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	im.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	var out bytes.Buffer
	if err := png.Encode(&out, im); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(out.Bytes())
	return out.Bytes(), hex.EncodeToString(sum[:])
}

func integratedNotebookDial(t *testing.T, address string, page notebook.ID, access string) *wsconn.Conn {
	t.Helper()
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ws, err := wsconn.Dial(ctx, func(ctx context.Context) (net.Conn, error) {
		c, err := new(net.Dialer).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", u.Port()))
		if err != nil {
			return nil, err
		}
		if err := c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			c.Close()
			return nil, err
		}
		return &withOrigin{Conn: c, origin: strings.TrimSuffix(address, "/")}, nil
	}, u.Host, "/_notebook/ws")
	if err != nil {
		t.Fatalf("notebook WebSocket upgrade through the flat host: %v", err)
	}
	t.Cleanup(func() { ws.Close(1000, ""); <-ws.Done() })
	if err := ws.NetConn().SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	integratedNotebookSend(t, ws, map[string]any{"type": "hello", "version": 1, "pending": map[string]any{"count": 0}, "page": page})
	welcome := integratedNotebookFrame(t, ws, "welcome")
	var got struct {
		Access  string `json:"access"`
		Session string `json:"session"`
	}
	if err := json.Unmarshal(welcome, &got); err != nil || got.Access != access || (got.Session != "") != (access == "private") {
		t.Fatalf("wrong route access: %s, %v", welcome, err)
	}
	for {
		raw := integratedNotebookFrame(t, ws, "snapshot")
		var snapshot struct {
			Scope struct {
				Kind string      `json:"kind"`
				ID   notebook.ID `json:"id"`
			} `json:"scope"`
			Part  int `json:"part"`
			Parts int `json:"parts"`
		}
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.Scope.Kind == "page" && snapshot.Scope.ID == page && snapshot.Part+1 == snapshot.Parts {
			return ws
		}
	}
}

func integratedNotebookSend(t *testing.T, ws *wsconn.Conn, value any) {
	t.Helper()
	if err := ws.NetConn().SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteFrame(wsconn.OpText, integratedNotebookJSON(t, value)); err != nil {
		t.Fatal(err)
	}
}

func integratedNotebookFrame(t *testing.T, ws *wsconn.Conn, kind string) []byte {
	t.Helper()
	if err := ws.NetConn().SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 32; i++ {
		raw, err := ws.ReadMessage(512 << 10)
		if err != nil {
			t.Fatalf("waiting for %s: %v", kind, err)
		}
		var frame struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(raw), &frame); err != nil {
			t.Fatal(err)
		}
		if frame.Type == kind {
			return []byte(raw)
		}
		if frame.Type == "fatal" || frame.Type == "reject" {
			t.Fatalf("unexpected notebook frame while waiting for %s: %s", kind, raw)
		}
	}
	t.Fatalf("did not receive %s within 32 frames", kind)
	return nil
}

func integratedNotebookHTTP(t *testing.T, address, method, path string, body []byte) (int, http.Header, []byte) {
	t.Helper()
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return new(net.Dialer).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", u.Port()))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(t.Context(), method, strings.TrimSuffix(address, "/")+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", strings.TrimSuffix(address, "/"))
	req.Header.Set("X-Flats-Notebook", "1")
	// A client-supplied Private decision must not grant Public writes.
	req.Header.Set("X-Flats-Access", "private")
	req.Header.Set("X-Flats-Public-Access", "edit")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, raw
}

func TestIntegratedNotebookCoreMCPAndLive(t *testing.T) {
	e := newEnv(t)
	const slug = "integrated-notes"
	view, err := e.svc.CreateNotebook(t.Context(), slug, "Integrated notes", core.ViaMCP)
	if err != nil || view.Type != contenttype.Notebook || !view.Running || view.Visibility != store.Private || view.PrivateURL == "" {
		t.Fatalf("CreateNotebook: %+v, %v", view, err)
	}
	initial := integratedNotebookGet(t, e, slug, map[string]any{})
	if len(initial.Tree) != 1 || *initial.Revision != 1 || initial.Tree[0].Ref.Inc == 0 {
		t.Fatalf("new notebook must have one saved page: %+v", initial)
	}
	page := initial.Tree[0].Ref
	ws := integratedNotebookDial(t, view.PrivateURL, page.ID, "private")
	imageBytes, imageHash := integratedNotebookImage(t)
	update := map[string]any{
		"op_id": "core-image", "message": "Image through the complete stack",
		"assets": []any{map[string]any{"ref": "photo", "data": base64.StdEncoding.EncodeToString(imageBytes)}},
		"ops": []any{
			map[string]any{"op": "set_page_title", "page": page, "title": "Field notes"},
			map[string]any{"op": "markdown_to_blocks", "place": map[string]any{"page": page, "parent": nil, "before": nil},
				"markdown":   "Agent paragraph.\n\n![Checked photo](photo.png)",
				"asset_refs": []any{map[string]any{"path": "photo.png", "asset": map[string]any{"tmp": "photo"}}}},
		},
	}
	written, err := e.svc.UpdateNotebook(t.Context(), slug, integratedNotebookJSON(t, update), core.ViaMCP)
	if err != nil {
		t.Fatalf("UpdateNotebook with uploaded Markdown image: %v", err)
	}
	var result notebook.WriteResult
	if err := json.Unmarshal(written.Result, &result); err != nil || written.Pending || written.Revision != 2 || result.Replay || result.Applied != 2 || result.State.Store != initial.State.Store {
		t.Fatalf("committed image update: %+v / %s, %v", written, written.Result, err)
	}
	var delta struct {
		Seq     notebook.Sequence        `json:"seq"`
		Tree    []integratedNotebookItem `json:"tree"`
		Changes []integratedNotebookItem `json:"changes"`
	}
	rawDelta := integratedNotebookFrame(t, ws, "delta")
	if err := json.Unmarshal(rawDelta, &delta); err != nil || delta.Seq != result.State.Seq || len(delta.Tree) != 1 || delta.Tree[0].Title != "Field notes" {
		t.Fatalf("agent update did not arrive as canonical delta: %s, %v", rawDelta, err)
	}
	read := integratedNotebookGet(t, e, slug, map[string]any{"page": page.ID})
	if read.Page == nil || read.State != result.State || *read.Revision != 2 {
		t.Fatalf("GetNotebook missed committed content: %+v", read)
	}
	imageCount, paragraphCount := 0, 0
	for _, item := range read.Page.Items {
		switch item.Content.Kind {
		case "image":
			imageCount++
			if item.Content.Asset != imageHash || item.Content.Alt != "Checked photo" {
				t.Fatalf("wrong checked image: %+v", item)
			}
		case "paragraph":
			paragraphCount++
		}
	}
	if imageCount != 1 || paragraphCount != 1 || !bytes.Contains(rawDelta, []byte(imageHash)) || !bytes.Contains(rawDelta, []byte("Agent paragraph.")) {
		t.Fatalf("image or paragraph missing from read/live update: %+v / %s", read.Page, rawDelta)
	}
	status, headers, downloaded := integratedNotebookHTTP(t, view.PrivateURL, http.MethodGet, "/_notebook/a/"+imageHash+".png", nil)
	if status != http.StatusOK || headers.Get("Content-Type") != "image/png" || headers.Get("Cache-Control") != "no-store" || !bytes.Equal(downloaded, imageBytes) {
		t.Fatalf("checked image not served through core: %d %v %q", status, headers, downloaded)
	}
	var toolRead integratedNotebookRead
	if text, bad := call(t, e.local, "get_notebook", map[string]any{"slug": slug, "page": page.ID}, &toolRead); bad || !reflect.DeepEqual(toolRead, read) {
		t.Fatalf("actual get_notebook differs from core: %s / %+v / %+v", text, toolRead, read)
	}
	var toolWrite notebook.WriteResult
	args := map[string]any{"slug": slug, "op_id": "mcp-title", "ops": []any{map[string]any{"op": "set_page_title", "page": page, "title": "Edited through MCP"}}}
	if text, bad := call(t, e.local, "update_notebook", args, &toolWrite); bad || toolWrite.Revision == nil || *toolWrite.Revision != 3 || toolWrite.RevisionPending || toolWrite.State.Seq <= result.State.Seq {
		t.Fatalf("actual update_notebook: %s / %+v", text, toolWrite)
	}
	rawDelta = integratedNotebookFrame(t, ws, "delta")
	if err := json.Unmarshal(rawDelta, &delta); err != nil || delta.Seq != toolWrite.State.Seq || len(delta.Tree) != 1 || delta.Tree[0].Title != "Edited through MCP" {
		t.Fatalf("MCP write not live through ServeHTTP: %s, %v", rawDelta, err)
	}
	if current := integratedNotebookGet(t, e, slug, map[string]any{}); len(current.Tree) != 1 || current.Tree[0].Title != "Edited through MCP" || current.State != toolWrite.State {
		t.Fatalf("core did not read MCP edit: %+v", current)
	}
	var replay notebook.WriteResult
	if text, bad := call(t, e.local, "update_notebook", args, &replay); bad || !replay.Replay || replay.State != toolWrite.State || replay.Revision == nil || *replay.Revision != 3 {
		t.Fatalf("MCP replay created another edit: %s / %+v", text, replay)
	}
	revision, files, err := e.svc.GetRevision(t.Context(), slug, 2)
	if err != nil || revision.Kind != "agent" || revision.Message != "Image through the complete stack" {
		t.Fatalf("agent outbox was not imported: %+v, %v", revision, err)
	}
	foundHeader, foundImage, foundBlocks := false, false, false
	for _, f := range files {
		switch {
		case f.Path == "pages/"+string(page.ID)+"/page.json":
			foundHeader = true
			data, _, err := e.svc.RevisionFileContent(t.Context(), slug, 2, f.Path)
			if err != nil || !bytes.Contains(data, []byte(`"title":"Field notes"`)) {
				t.Fatalf("revision page did not preserve its earlier title: %s, %v", data, err)
			}
		case f.Path == "assets/"+imageHash+".png":
			foundImage = true
			data, _, err := e.svc.RevisionFileContent(t.Context(), slug, 2, f.Path)
			if err != nil || !bytes.Equal(data, imageBytes) {
				t.Fatalf("imported image bytes differ: %q, %v", data, err)
			}
		case strings.HasPrefix(f.Path, "pages/"+string(page.ID)+"/blocks/"):
			foundBlocks = true
		}
	}
	if !foundHeader || !foundImage || !foundBlocks {
		t.Fatalf("revision omitted changed page content or image: %+v", files)
	}
	revisions, err := e.svc.ListRevisions(t.Context(), slug, 0, 10)
	if err != nil || len(revisions.Revisions) != 3 || revisions.Revisions[0].Number != 3 || revisions.Revisions[2].Number != 1 {
		t.Fatalf("unexpected history after replay: %+v, %v", revisions, err)
	}
	t.Run("page_change_summaries", func(t *testing.T) {
		pages, err := e.st.NotebookRevPages(t.Context(), slug, 2, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(pages) == 0 {
			t.Fatal("import stored no changed-page summaries")
		}
		for _, want := range []struct {
			revision int
			before   string
			after    string
		}{
			{2, initial.Tree[0].Title, "Field notes"},
			{3, "Field notes", "Edited through MCP"},
		} {
			changes := pages[want.revision]
			if len(changes) != 1 || changes[0].Page != string(page.ID) || changes[0].Change != "changed" || changes[0].TitleBefore != want.before || changes[0].TitleAfter != want.after {
				t.Errorf("revision %d changed-page summary = %+v", want.revision, changes)
			}
		}
	})
}

func TestIntegratedNotebookPublicRefusesWrite(t *testing.T) {
	e := newEnv(t)
	const slug = "public-notes"
	var flat FlatInfo
	if text, bad := call(t, e.local, "create_flat", map[string]any{"slug": slug, "name": "Public notes", "type": "notebook"}, &flat); bad || flat.Type != "notebook" || !flat.Running {
		t.Fatalf("actual create_flat notebook: %s / %+v", text, flat)
	}
	before := integratedNotebookGet(t, e, slug, map[string]any{})
	if len(before.Tree) != 1 {
		t.Fatalf("created notebook tree: %+v", before)
	}
	if code, out := e.operatorCall(t, http.MethodPost, "/flats/"+slug+"/providers", `{"provider":"portal","permitted":true}`); code != http.StatusOK {
		t.Fatalf("Public provider grant: %d %+v", code, out)
	}
	var visibility ActionOut
	if text, bad := call(t, e.local, "set_visibility", map[string]any{"slug": slug, "visibility": "public", "reason": "Verify read-only notebook"}, &visibility); bad || visibility.Status != "pending_approval" || visibility.ApprovalID == "" {
		t.Fatalf("visibility approval: %s / %+v", text, visibility)
	}
	e.approve(t, visibility.ApprovalID)
	view, err := e.svc.GetFlat(t.Context(), slug)
	if err != nil || string(view.Visibility) != "public" || view.PublicURL == "" {
		t.Fatalf("approved Public notebook: %+v, %v", view, err)
	}
	page := before.Tree[0].Ref
	ws := integratedNotebookDial(t, view.PublicURL, page.ID, "public")
	integratedNotebookSend(t, ws, map[string]any{"type": "action", "n": "1", "action_id": "public-write", "ops": []any{map[string]any{"op": "set_page_title", "page": page, "title": "Must not apply"}}})
	rejected := integratedNotebookFrame(t, ws, "reject")
	var refusal struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rejected, &refusal); err != nil || refusal.Code != "forbidden" {
		t.Fatalf("Public socket write was not forbidden: %s, %v", rejected, err)
	}
	imageBytes, _ := integratedNotebookImage(t)
	for _, tc := range []struct {
		name, method, path string
		body               []byte
	}{
		{"upload", http.MethodPost, "/_notebook/api/assets", imageBytes},
		{"paste", http.MethodPost, "/_notebook/api/paste", []byte(`{"markdown":"must not paste"}`)},
		{"preview", http.MethodPost, "/_notebook/api/conversion-preview", []byte(`{}`)},
		{"recover", http.MethodPost, "/_notebook/api/recover", []byte(`{}`)},
		{"pin", http.MethodPost, "/_notebook/api/pin", []byte(`{}`)},
		{"rollback", http.MethodPost, "/_notebook/api/rollback", []byte(`{}`)},
		{"history", http.MethodGet, "/_notebook/api/revisions", nil},
		{"revision", http.MethodGet, "/_notebook/api/revisions/1", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, raw := integratedNotebookHTTP(t, view.PublicURL, tc.method, tc.path, tc.body)
			if code != http.StatusForbidden {
				t.Fatalf("Public route with forged access headers: %d %s", code, raw)
			}
		})
	}
	after := integratedNotebookGet(t, e, slug, map[string]any{})
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("Public refusal changed notebook: before %+v, after %+v", before, after)
	}
	revisions, err := e.svc.ListRevisions(t.Context(), slug, 0, 10)
	if err != nil || len(revisions.Revisions) != 1 {
		t.Fatalf("Public attempts created history: %+v, %v", revisions, err)
	}
}
