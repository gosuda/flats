package core

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/board"
	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/store"
	"github.com/gosuda/flats/internal/wsconn"
)

// boardOps decodes update_board operations written as JSON.
func boardOps(t *testing.T, raw string) []engine.AgentOp {
	t.Helper()
	var ops []engine.AgentOp
	if err := json.Unmarshal([]byte(raw), &ops); err != nil {
		t.Fatal(err)
	}
	return ops
}

func newBoard(t *testing.T, s *Service, slugName string) {
	t.Helper()
	if _, err := s.CreateBoard(context.Background(), slugName, "Plan", ViaMCP); err != nil {
		t.Fatal(err)
	}
}

func writeBoard(t *testing.T, s *Service, slugName, opID, ops string) BoardWriteResult {
	t.Helper()
	res, err := s.UpdateBoard(context.Background(), slugName, BoardUpdate{OpID: opID, Ops: boardOps(t, ops)}, ViaMCP)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func boardElements(t *testing.T, s *Service, slugName string, rev int) map[string]map[string]any {
	t.Helper()
	p, err := s.GetBoard(context.Background(), slugName, rev, board.Query{Limit: 2000})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, raw := range p.Elements {
		var el map[string]any
		if err := json.Unmarshal(raw, &el); err != nil {
			t.Fatal(err)
		}
		out[el["id"].(string)] = el
	}
	return out
}

func revisionNumbers(t *testing.T, s *Service, slugName string) []int {
	t.Helper()
	page, err := s.ListRevisions(context.Background(), slugName, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var nums []int
	for _, r := range page.Revisions {
		nums = append(nums, r.Number)
	}
	return nums
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// boardClient is a WebSocket client of a board's canvas protocol.
type boardClient struct {
	t    *testing.T
	ws   *wsconn.Conn
	msgs chan map[string]any
}

// originConn adds an Origin header to the handshake written through it.
type originConn struct {
	net.Conn
	origin string
	done   bool
}

func (c *originConn) Write(p []byte) (int, error) {
	if !c.done && bytes.HasSuffix(p, []byte("\r\n\r\n")) {
		c.done = true
		q := append(slices.Clone(p[:len(p)-2]), []byte("Origin: "+c.origin+"\r\n\r\n")...)
		if _, err := c.Conn.Write(q); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	return c.Conn.Write(p)
}

// dialBoard opens the canvas socket of the board served at base and says
// hello as a new editor.
func dialBoard(t *testing.T, base string) *boardClient {
	t.Helper()
	host := strings.TrimPrefix(base, "http://")
	ws, err := wsconn.Dial(context.Background(), func(ctx context.Context) (net.Conn, error) {
		c, err := net.Dial("tcp", host)
		if err != nil {
			return nil, err
		}
		return &originConn{Conn: c, origin: base}, nil
	}, host, "/_board/ws")
	if err != nil {
		t.Fatal(err)
	}
	c := &boardClient{t: t, ws: ws, msgs: make(chan map[string]any, 256)}
	go func() {
		defer close(c.msgs)
		for {
			raw, err := ws.ReadMessage(64 << 20)
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal([]byte(raw), &m) == nil {
				c.msgs <- m
			}
		}
	}()
	t.Cleanup(func() { ws.Close(1000, "") })
	c.send(map[string]any{"t": "hello", "v": 1, "pending": map[string]any{"count": 0}, "editor": "e1", "name": "Pat"})
	c.expect("welcome")
	return c
}

func (c *boardClient) send(v any) {
	c.t.Helper()
	b, _ := json.Marshal(v)
	if err := c.ws.WriteFrame(wsconn.OpText, b); err != nil {
		c.t.Fatal(err)
	}
}

func (c *boardClient) expect(kind string) map[string]any {
	c.t.Helper()
	return c.expect2(kind)
}

// batch sends ops as batch n, resending it after a transient refusal as the
// canvas does, and returns the ack or permanent reject.
func (c *boardClient) batch(n int, ops ...map[string]any) map[string]any {
	c.t.Helper()
	for range 100 {
		c.send(map[string]any{"t": "ops", "n": n, "ops": ops})
		for {
			m := c.expect2("ack", "reject")
			if m["n"] != float64(n) {
				continue
			}
			if m["retry"] == true {
				time.Sleep(20 * time.Millisecond)
				break
			}
			return m
		}
	}
	c.t.Fatalf("batch %d kept being refused", n)
	return nil
}

func (c *boardClient) expect2(kinds ...string) map[string]any {
	c.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-c.msgs:
			if !ok {
				c.t.Fatalf("socket closed before %v", kinds)
			}
			if slices.Contains(kinds, m["t"].(string)) {
				return m
			}
		case <-timeout:
			c.t.Fatalf("no %v message", kinds)
		}
	}
}

// closed waits for the server to close the socket and returns the fatal
// code it sent first, if any.
func (c *boardClient) closed() (fatal string, ok bool) {
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m, open := <-c.msgs:
			if !open {
				return fatal, true
			}
			if m["t"] == "fatal" {
				fatal, _ = m["code"].(string)
			}
		case <-timeout:
			return fatal, false
		}
	}
}

func sticky(id, text string, x float64) map[string]any {
	return map[string]any{"op": "add", "el": map[string]any{"id": id, "type": "sticky", "x": x, "y": 0, "w": 200, "h": 200, "rot": 0, "text": text}}
}

// TestBoardCreateServeAndEdit creates a board, serves its canvas through
// the flat's route, edits it over the WebSocket and turns the edits into
// revisions of every kind.
func TestBoardCreateServeAndEdit(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	newBoard(t, s, "plan")
	f, err := s.GetFlat(ctx, "plan")
	// The shell version that serves the canvas stays internal, as for a
	// design: a board has no versions (rule 54).
	if err != nil || f.Type != "board" || !f.Running || f.LiveVersion != 0 || f.Versions != 0 || f.VersionCount != 0 || f.Live != nil || f.Publication != "" || f.Draft != nil {
		t.Fatalf("flat = %+v %v", f, err)
	}
	if fs, err := s.ListFlats(ctx); err != nil || len(fs) != 1 || fs[0].LiveVersion != 0 || fs[0].Versions != 0 || fs[0].VersionCount != 0 || !fs[0].Running {
		t.Fatalf("list = %+v %v", fs, err)
	}
	page, err := s.ListRevisions(ctx, "plan", 0, 10)
	if err != nil || len(page.Revisions) != 1 || page.Revisions[0].Kind != "created" {
		t.Fatalf("created revision = %+v %v", page, err)
	}
	if events, _ := s.Events(ctx, "plan", "", 0, 50); slices.ContainsFunc(events, func(e store.Event) bool {
		return strings.Contains(e.Message, "draft revision") || strings.Contains(e.Message, "health check")
	}) {
		t.Fatalf("creating a board logged its internal shell: %+v", events)
	}
	srv := httptest.NewServer(s.siteHandler("plan", false))
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/")
	if err != nil || resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatalf("canvas shell = %v %v", resp, err)
	}
	resp.Body.Close()

	c := dialBoard(t, srv.URL)
	if n := s.state("plan").usage.openStreams.Load(); n != 1 {
		t.Fatalf("open streams with a canvas socket = %d", n)
	}
	if u, err := s.Usage(ctx, "plan", 1); err != nil || u.Now.CPU != UsageNotSeparate {
		t.Fatalf("usage now = %+v %v", u.Now, err)
	}
	if ack := c.batch(1, sticky("e_person0001", "from a person", 0)); ack["t"] != "ack" {
		t.Fatalf("batch = %v", ack)
	}
	// An agent write first closes the person's open group as a revision.
	res := writeBoard(t, s, "plan", "agent-write-1", `[{"op":"add","ref":"$a","el":{"type":"sticky","text":"from an agent"}}]`)
	if res.Revision == nil || *res.Revision != 3 || len(res.IDs.(map[string]string)) != 1 {
		t.Fatalf("agent write = %+v", res)
	}
	if d := c.expect("delta"); d["by"].(map[string]any)["kind"] != "agent" {
		t.Fatalf("agent delta = %v", d)
	}
	// A whole-board replacement is an import revision.
	res = writeBoard(t, s, "plan", "replace-call-1", `[{"op":"add","el":{"id":"e_replaced01","type":"sticky","text":"fresh","x":0,"y":0}}]`)
	if _, err := s.UpdateBoard(ctx, "plan", BoardUpdate{OpID: "replace-call-2", Replace: true, Ops: boardOps(t, `[{"op":"add","el":{"id":"e_replaced02","type":"sticky","text":"only","x":0,"y":0}}]`)}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	page, err = s.ListRevisions(ctx, "plan", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, r := range page.Revisions {
		kinds = append(kinds, r.Kind)
	}
	if strings.Join(kinds, ",") != "import,agent,agent,person,created" {
		t.Fatalf("kinds = %v", kinds)
	}
	if e := page.Revisions[3].Elements; e == nil || e.Added != 1 || e.Changed != 0 || e.Removed != 0 || len(page.Revisions[3].Changes.Added) != 0 {
		t.Fatalf("person revision counts = %+v", page.Revisions[3])
	}
	if e := page.Revisions[0].Elements; e == nil || e.Added != 1 || e.Removed != 3 {
		t.Fatalf("replacement counts = %+v", page.Revisions[0].Elements)
	}
	if got := boardElements(t, s, "plan", 0); len(got) != 1 || got["e_replaced02"] == nil {
		t.Fatalf("after replacement = %v", got)
	}
	if got := boardElements(t, s, "plan", 2); len(got) != 1 || got["e_person0001"]["text"] != "from a person" {
		t.Fatalf("revision 2 = %v", got)
	}
	// content_head is the last imported file list.
	head, err := s.st.Head(ctx, "plan")
	files, _ := s.st.RevisionFiles(ctx, "plan", page.Revisions[0].Number)
	if err != nil || len(head) != len(files) || head[0].Hash != files[0].Hash {
		t.Fatalf("head = %+v, latest files %+v (%v)", head, files, err)
	}
	// A repeated op_id returns the first result and applies nothing again.
	again, err := s.UpdateBoard(ctx, "plan", BoardUpdate{OpID: "agent-write-1", Ops: boardOps(t, `[{"op":"add","ref":"$a","el":{"type":"sticky","text":"from an agent"}}]`)}, ViaMCP)
	if err != nil || !again.Replayed || again.Revision == nil || *again.Revision != 3 {
		t.Fatalf("replay = %+v %v", again, err)
	}
	if _, err := s.UpdateBoard(ctx, "plan", BoardUpdate{OpID: "agent-write-1", Ops: boardOps(t, `[]`)}, ViaMCP); !errors.Is(err, ErrConflict) {
		t.Fatalf("op_id reused for another request: %v", err)
	}
	if len(revisionNumbers(t, s, "plan")) != 5 {
		t.Fatal("a replay created a revision")
	}
}

// TestBoardTypesAndVersionTools covers type stickiness, adoption, the
// refusal of Version tools and of board tools on other flats.
func TestBoardTypesAndVersionTools(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	newBoard(t, s, "plan")
	if _, err := s.SaveVersion(ctx, "plan", []bundle.File{{Path: "index.html", Data: []byte("x")}}, SaveMeta{}, ViaMCP); !errors.Is(err, ErrTypeChange) {
		t.Fatalf("website into a board: %v", err)
	}
	if _, err := s.SaveVersion(ctx, "fresh", []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"board"}`)}}, SaveMeta{}, ViaMCP); !errors.Is(err, ErrBoardUnversioned) {
		t.Fatalf("board manifest on a bundle path: %v", err)
	}
	if _, err := s.WriteDesign(ctx, "plan", DesignWrite{Files: []DesignFile{{Path: "canvas.json", Data: canvasJSON()}}}, ViaMCP); !errors.Is(err, ErrTypeChange) {
		t.Fatalf("design into a board: %v", err)
	}
	if _, err := s.ListVersions(ctx, "plan"); !errors.Is(err, ErrBoardUnversioned) || ErrorCategory(err) != "invalid" {
		t.Fatalf("list versions: %v", err)
	}
	if _, err := s.GetDraft(ctx, "plan"); !errors.Is(err, ErrBoardUnversioned) {
		t.Fatalf("get draft: %v", err)
	}
	if _, err := s.OpenPreview(ctx, "plan", 0); !errors.Is(err, ErrBoardUnversioned) {
		t.Fatalf("preview: %v", err)
	}
	if _, err := s.CreateBoard(ctx, "plan", "", ViaMCP); !errors.Is(err, ErrConflict) {
		t.Fatalf("existing slug: %v", err)
	}
	newDesign(t, s, "studio")
	for _, err := range []error{
		func() error {
			_, err := s.UpdateBoard(ctx, "studio", BoardUpdate{OpID: "not-a-board"}, ViaMCP)
			return err
		}(),
		func() error { _, err := s.GetBoard(ctx, "studio", 0, board.Query{}); return err }(),
		func() error { _, err := s.BoardShares(ctx, "studio", 0); return err }(),
	} {
		if !errors.Is(err, ErrNotBoard) || ErrorCategory(err) != "not_board" {
			t.Fatalf("board tool on a design: %v", err)
		}
	}
	// update_board creates a missing board and adopts an untyped flat.
	res := writeBoard(t, s, "brainstorm", "create-on-write", `[{"op":"add","el":{"type":"sticky","text":"first"}}]`)
	if !res.Created || res.Revision == nil || *res.Revision != 2 {
		t.Fatalf("created by update_board = %+v", res)
	}
	if _, err := s.CreateFlat(ctx, "empty", "Empty", ViaMCP); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetBoard(ctx, "empty", 0, board.Query{}); !errors.Is(err, ErrNotBoard) {
		t.Fatalf("read of an untyped flat: %v", err)
	}
	if res := writeBoard(t, s, "empty", "adopt-on-write", `[]`); !res.Created {
		t.Fatalf("adoption = %+v", res)
	}
	if f, _ := s.GetFlat(ctx, "empty"); f.Type != "board" || f.Name != "Empty" {
		t.Fatalf("adopted = %+v", f)
	}
}

// A refused first write leaves nothing behind: a new slug has no flat and
// no files, an adopted flat stays untyped (rule 33).
func TestBoardRefusedFirstWrite(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	gone := func(slugName string) {
		t.Helper()
		if _, err := s.st.GetFlat(ctx, slugName); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s flat after a refused first write: %v", slugName, err)
		}
		if _, err := os.Stat(s.flatDir(slugName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s directory after a refused first write: %v", slugName, err)
		}
	}
	missing := boardOps(t, `[{"op":"delete","ids":["e_nothere000"]}]`)
	if _, err := s.UpdateBoard(ctx, "fresh", BoardUpdate{OpID: "refused-first", Ops: missing}, ViaMCP); !errors.Is(err, ErrConflict) {
		t.Fatalf("refused write: %v", err)
	}
	gone("fresh")
	if _, err := s.ImportBoard(ctx, "fresh", BoardImport{OpID: "refused-import", Mode: "add", Archive: strings.NewReader("not a zip"), Size: 9}, ViaMCP); !errors.Is(err, ErrInvalid) {
		t.Fatalf("refused import: %v", err)
	}
	gone("fresh")
	if _, err := s.UpdateSettings(ctx, map[string]string{SetDiskQuotaBytes: "100000"}); err != nil {
		t.Fatal(err)
	}
	img := pngBytes(t, 4, 4)
	big := `[{"op":"add","el":{"type":"sticky","text":"` + strings.Repeat("q", 3900) + `"}}` + strings.Repeat(`,{"op":"add","el":{"type":"sticky","text":"`+strings.Repeat("q", 3900)+`"}}`, 40) + `]`
	_, err := s.UpdateBoard(ctx, "fresh", BoardUpdate{OpID: "over-quota-1", Ops: boardOps(t, big), Assets: []BoardAsset{{Ref: "$i", Content: img}}}, ViaMCP)
	if !errors.Is(err, ErrBoardCapacity) {
		t.Fatalf("over-quota first write: %v", err)
	}
	gone("fresh")
	if _, err := s.UpdateSettings(ctx, map[string]string{SetDiskQuotaBytes: "0"}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CreateFlat(ctx, "untyped", "Kept", ViaMCP); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnv(ctx, "untyped", "MODE", "kept", ViaMCP); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateBoard(ctx, "untyped", BoardUpdate{OpID: "refused-adopt", Ops: missing, Assets: []BoardAsset{{Ref: "$i", Content: img}}}, ViaMCP); !errors.Is(err, ErrConflict) {
		t.Fatalf("refused adoption: %v", err)
	}
	f, err := s.GetFlat(ctx, "untyped")
	if err != nil || f.LiveVersion != 0 || f.Name != "Kept" {
		t.Fatalf("flat after a refused adoption = %+v %v", f, err)
	}
	if typ, err := s.flatType(ctx, f.Flat); err != nil || typ != "" {
		t.Fatalf("type after a refused adoption = %q %v", typ, err)
	}
	if env, err := s.ListEnv(ctx, "untyped"); err != nil || len(env) != 1 {
		t.Fatalf("env after a refused adoption = %v %v", env, err)
	}
	for _, p := range []string{s.boardDir("untyped"), s.docsStoreDir("untyped")} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s after a refused adoption: %v", p, err)
		}
	}
	if res := writeBoard(t, s, "untyped", "accepted-adopt", `[{"op":"add","el":{"type":"sticky","text":"now"}}]`); !res.Created || *res.Revision != 2 {
		t.Fatalf("adoption after a refusal = %+v", res)
	}
}

// TestBoardRollbackHoldAndSettlement rolls back through the hold, and
// settles holds after a simulated crash: one whose rollback committed in
// board.db but was not imported, and one whose rollback never committed.
func TestBoardRollbackHoldAndSettlement(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	open := func() *Service {
		s, err := New(ctx, Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Logf: t.Logf})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := open()
	newBoard(t, s, "plan")
	writeBoard(t, s, "plan", "write-one-1", `[{"op":"add","el":{"id":"e_aaaaaaaaaa","type":"sticky","text":"one","x":0,"y":0}}]`)
	writeBoard(t, s, "plan", "write-two-1", `[{"op":"set","id":"e_aaaaaaaaaa","fields":{"text":"two"}}]`)
	n, err := s.RollbackRevision(ctx, "plan", 2, "Agent", ViaMCP)
	if err != nil || n != 4 {
		t.Fatalf("rollback = %d %v", n, err)
	}
	if els := boardElements(t, s, "plan", 0); els["e_aaaaaaaaaa"]["text"] != "one" {
		t.Fatalf("rolled back = %v", els)
	}
	r, _, err := s.GetRevision(ctx, "plan", 4)
	if err != nil || r.Kind != "rollback" || r.RollbackOf != 2 || r.Elements == nil || r.Elements.Changed != 1 {
		t.Fatalf("rollback revision = %+v %v", r, err)
	}
	if holds, _ := s.st.Holds(ctx, "plan"); len(holds) != 0 {
		t.Fatalf("holds after the rollback = %v", holds)
	}
	// A crash after the board committed a rollback and before core
	// imported it, and another before the board committed one.
	in := s.liveBoard("plan")
	_, c, err := s.boardRevisionContent(ctx, "plan", 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []store.Hold{{Op: "committed-op", Number: 3}, {Op: "lost-op", Number: 2}} {
		if err := s.st.ContentTx(ctx, "plan", func(tx *store.ContentTx) error { return tx.AddHold(ctx, h.Op, h.Number) }); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := in.Rollback(ctx, "committed-op", 3, c, "Agent"); err != nil {
		t.Fatal(err)
	}
	s.boardPending.Delete("plan")
	s.Close()
	s = open()
	defer s.Close()
	if holds, _ := s.st.Holds(ctx, "plan"); len(holds) != 0 {
		t.Fatalf("holds after restart = %v", holds)
	}
	page, _ := s.ListRevisions(ctx, "plan", 0, 1)
	if r := page.Revisions[0]; r.Number != 5 || r.Kind != "rollback" || r.RollbackOf != 3 {
		t.Fatalf("imported after restart = %+v", r)
	}
	if els := boardElements(t, s, "plan", 0); els["e_aaaaaaaaaa"]["text"] != "two" {
		t.Fatalf("after restart = %v", els)
	}
}

// TestBoardRetentionPinsAndSweep applies the revision quota, pins and the
// blob sweep with its grace and the board.db references.
func TestBoardRetentionPinsAndSweep(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	newBoard(t, s, "plan")
	for i := range 4 {
		writeBoard(t, s, "plan", "retention-"+string(rune('a'+i))+"-1", `[{"op":"add","el":{"type":"sticky","text":"`+strings.Repeat("x", 100+i)+`"}}]`)
	}
	two, one := 2, 1
	if err := s.SetRevisionQuotas(ctx, "plan", &two, &one); err != nil {
		t.Fatal(err)
	}
	if got := revisionNumbers(t, s, "plan"); !slices.Equal(got, []int{5, 4}) {
		t.Fatalf("quota 2 kept %v", got)
	}
	if err := s.PinRevision(ctx, "plan", 4, true); err != nil {
		t.Fatal(err)
	}
	if err := s.PinRevision(ctx, "plan", 5, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("full pin quota: %v", err)
	}
	writeBoard(t, s, "plan", "retention-e-1", `[{"op":"add","el":{"type":"sticky","text":"e"}}]`)
	if got := revisionNumbers(t, s, "plan"); !slices.Equal(got, []int{6, 5, 4}) {
		t.Fatalf("pinned revision dropped: %v", got)
	}

	// An image only protected after upload is referenced by board.db alone.
	img := pngBytes(t, 3, 2)
	res, err := s.UpdateBoard(ctx, "plan", BoardUpdate{OpID: "upload-only-1", Assets: []BoardAsset{{Ref: "$img", Content: img}}}, ViaMCP)
	if err != nil || res.Assets["$img"] != hashBytes(img) {
		t.Fatalf("asset upload = %+v %v", res, err)
	}
	root := s.storeDir("plan", "board")
	held := storedBlobPath(root, hashBytes(img))
	stray, err := putBlob(root, []byte("nobody references this"))
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := putBlob(root, []byte("recent and unreferenced"))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	for _, p := range []string{held, storedBlobPath(root, stray)} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	s.sweepBlobs(ctx, "plan", "board")
	if _, err := os.Stat(held); err != nil {
		t.Fatalf("sweep deleted an asset board.db protects: %v", err)
	}
	if _, err := os.Stat(storedBlobPath(root, stray)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreferenced old blob kept: %v", err)
	}
	if _, err := os.Stat(storedBlobPath(root, fresh)); err != nil {
		t.Fatalf("sweep ignored the grace period: %v", err)
	}
	// Every revision still reads.
	for _, n := range revisionNumbers(t, s, "plan") {
		boardElements(t, s, "plan", n)
	}
	// An image element names an asset of the same call by its ref.
	res, err = s.UpdateBoard(ctx, "plan", BoardUpdate{OpID: "image-element-1", Assets: []BoardAsset{{Ref: "$pic", Content: img}},
		Ops: boardOps(t, `[{"op":"add","ref":"$i","el":{"type":"image","asset":"$pic","alt":"a picture"}}]`)}, ViaMCP)
	if err != nil {
		t.Fatal(err)
	}
	el := boardElements(t, s, "plan", 0)[res.IDs.(map[string]string)["$i"]]
	if el["asset"] != hashBytes(img) || el["natural"].([]any)[0] != float64(3) {
		t.Fatalf("image element = %v", el)
	}
}

// TestBoardRenameDeleteAndQuota stops the instance on rename and delete,
// telling open canvases why, and refuses an over-quota write.
func TestBoardRenameDeleteAndQuota(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	newBoard(t, s, "plan")
	srv := httptest.NewServer(s.siteHandler("plan", false))
	defer srv.Close()
	c := dialBoard(t, srv.URL)
	if _, err := s.RenameSlug(ctx, "plan", "roadmap", ViaConsole); err != nil {
		t.Fatal(err)
	}
	if code, ok := c.closed(); !ok || code != "renamed" {
		t.Fatalf("rename: closed=%v fatal=%q", ok, code)
	}
	if els := boardElements(t, s, "roadmap", 0); len(els) != 0 {
		t.Fatalf("renamed board = %v", els)
	}
	writeBoard(t, s, "roadmap", "after-rename-1", `[{"op":"add","el":{"type":"sticky","text":"still here"}}]`)
	if got := revisionNumbers(t, s, "roadmap"); !slices.Equal(got, []int{2, 1}) {
		t.Fatalf("revisions after rename = %v", got)
	}

	if _, err := s.UpdateSettings(ctx, map[string]string{SetDiskQuotaBytes: "200000"}); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("q", 3900)
	var ops []string
	for range 60 {
		ops = append(ops, `{"op":"add","el":{"type":"sticky","text":"`+big+`"}}`)
	}
	_, err := s.UpdateBoard(ctx, "roadmap", BoardUpdate{OpID: "too-big-write", Ops: boardOps(t, "["+strings.Join(ops, ",")+"]")}, ViaMCP)
	if !errors.Is(err, ErrBoardCapacity) || ErrorCategory(err) != "board_capacity" {
		t.Fatalf("over-quota write: %v", err)
	}
	if got := revisionNumbers(t, s, "roadmap"); len(got) != 2 {
		t.Fatalf("a refused write recorded a revision: %v", got)
	}
	if _, err := s.UpdateSettings(ctx, map[string]string{SetDiskQuotaBytes: "0"}); err != nil {
		t.Fatal(err)
	}

	srv2 := httptest.NewServer(s.siteHandler("roadmap", false))
	defer srv2.Close()
	c = dialBoard(t, srv2.URL)
	if _, err := s.Delete(ctx, "roadmap", ViaConsole, ""); err != nil {
		t.Fatal(err)
	}
	if code, ok := c.closed(); !ok || code != "deleted" {
		t.Fatalf("delete: closed=%v fatal=%q", ok, code)
	}
	if _, err := s.GetBoard(ctx, "roadmap", 0, board.Query{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted board read: %v", err)
	}
}

// TestBoardImportAndPaging imports a data file exported from a board and
// reads a large board in pages from one snapshot.
func TestBoardImportAndPaging(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	newBoard(t, s, "plan")
	var ops []string
	for i := range 30 {
		ops = append(ops, `{"op":"add","el":{"type":"sticky","text":"n`+string(rune('a'+i%26))+`"}}`)
	}
	writeBoard(t, s, "plan", "thirty-stickies", "["+strings.Join(ops, ",")+"]")
	p, err := s.GetBoard(ctx, "plan", 0, board.Query{Limit: 10})
	if err != nil || len(p.Elements) != 10 || p.Next == "" || p.Revision != 2 {
		t.Fatalf("first page = %d %q %v", len(p.Elements), p.Next, err)
	}
	writeBoard(t, s, "plan", "between-pages", `[{"op":"add","el":{"type":"sticky","text":"later"}}]`)
	seen := len(p.Elements)
	for p.Next != "" {
		if p, err = s.GetBoard(ctx, "plan", 0, board.Query{Cursor: p.Next}); err != nil {
			t.Fatal(err)
		}
		seen += len(p.Elements)
	}
	if seen != 30 {
		t.Fatalf("pages held %d elements, want the 30 of the first read", seen)
	}
	if _, err := s.GetBoard(ctx, "plan", 0, board.Query{Cursor: "0123456789abcdef0123456789abcdef"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("unknown cursor: %v", err)
	}

	srv := httptest.NewServer(s.siteHandler("plan", false))
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/_board/export.zip")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("export = %v %v", resp, err)
	}
	var zipped bytes.Buffer
	if _, err := zipped.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if _, err := zip.NewReader(bytes.NewReader(zipped.Bytes()), int64(zipped.Len())); err != nil {
		t.Fatal(err)
	}
	res, err := s.ImportBoard(ctx, "copy", BoardImport{OpID: "import-copy-1", Mode: "add", Archive: bytes.NewReader(zipped.Bytes()), Size: int64(zipped.Len())}, ViaMCP)
	if err != nil || !res.Created || res.Revision == nil || len(res.IDs.([]string)) != 31 {
		t.Fatalf("import = %+v %v", res, err)
	}
	page, _ := s.ListRevisions(ctx, "copy", 0, 1)
	if r := page.Revisions[0]; r.Kind != "import" || r.Elements.Added != 31 {
		t.Fatalf("import revision = %+v", r)
	}
}

// storedBlobPath is blobPath for hashes the test knows are valid.
func storedBlobPath(root, hash string) string {
	p, ok := blobPath(root, hash)
	if !ok {
		panic("invalid blob hash " + hash)
	}
	return p
}
