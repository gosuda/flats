package notebook

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/wsconn"
)

func TestLiveMalformedActionsCorrelate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frame  any
		number string
	}{
		{"operations", map[string]any{"type": "action", "n": "1", "ops": []any{map[string]any{"op": "unknown"}}}, `"1"`},
		{"multipart", map[string]any{"type": "action_part", "n": "2", "part": 1, "parts": 2, "data": "bad"}, `"2"`},
		{"anonymous", map[string]any{"type": "action", "ops": []any{}}, ""},
		{"frame", "not-json", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWriterFixture(t)
			ws, _ := liveDial(t, f.in, "private", nil)
			liveHelloSend(t, ws, f.session.Token, f.page.ID)
			liveUntil(t, ws, "welcome")
			liveSend(t, ws, tc.frame)
			for {
				_ = ws.NetConn().SetReadDeadline(time.Now().Add(5 * time.Second))
				raw, err := ws.ReadMessage(liveServerFrame)
				if err != nil {
					var closed *wsconn.CloseError
					if tc.name != "anonymous" || !errors.As(err, &closed) || closed.Code != 1008 || closed.Reason != "resend" {
						t.Fatalf("unexpected close: %v", err)
					}
					return
				}
				var v map[string]json.RawMessage
				if err := json.Unmarshal([]byte(raw), &v); err != nil {
					t.Fatal(err)
				}
				switch liveType(v) {
				case "reject":
					if tc.number == "" || string(v["n"]) != tc.number {
						t.Fatalf("uncorrelated reject: %s", v)
					}
					return
				case "fatal":
					if tc.name != "frame" || string(v["code"]) != `"protocol"` {
						t.Fatalf("unexpected fatal: %s", v)
					}
					return
				}
			}
		})
	}
}

func TestLiveReplayWaitsForRequiredPage(t *testing.T) {
	f := newWriterFixture(t)
	a := writerAction(f.page, "replayed title")
	if _, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, a); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	f.in.spills.wrapWriter = func(w io.Writer) io.Writer {
		return liveCaptureGateWriter{w, func() { once.Do(func() { close(entered); <-release }) }}
	}
	ws, _ := liveDial(t, f.in, "private", nil)
	liveHelloSend(t, ws, f.session.Token, f.page.ID)
	liveUntil(t, ws, "welcome")
	<-entered
	raw, _ := EncodeAction(a)
	var frame map[string]any
	_ = json.Unmarshal(raw, &frame)
	frame["type"] = "action"
	liveSend(t, ws, frame)
	// An acknowledgement is not canonical state while capture is blocked.
	_ = ws.NetConn().SetReadDeadline(time.Now().Add(80 * time.Millisecond))
	for {
		msg, err := ws.ReadMessage(liveServerFrame)
		if err != nil {
			break
		}
		if strings.Contains(msg, `"type":"ack"`) {
			unblock()
			t.Fatal("replay ack preceded its replacement base")
		}
	}
	unblock()
	var tree bool
	for {
		v := receiveLiveFrame(t, ws)
		if liveType(v) == "snapshot" {
			var s liveScope
			_ = json.Unmarshal(v["scope"], &s)
			tree = tree || s.Kind == "tree"
		}
		if liveType(v) == "ack" {
			if !tree {
				t.Fatal("replay ack before required tree unit")
			}
			break
		}
	}
}

type liveCaptureGateWriter struct {
	io.Writer
	gate func()
}

func (w liveCaptureGateWriter) Write(p []byte) (int, error) { w.gate(); return w.Writer.Write(p) }

func TestLivePublicResumeRequiresCapturedWatermark(t *testing.T) {
	f := newWriterFixture(t)
	spill, err := f.in.spills.Capture(t.Context(), f.in.ReadTx, Scope{Page: f.page.ID, Public: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.in.live.mu.Lock()
	f.in.live.resumes[spill.Cursor()] = liveResume{spill: spill, public: true}
	f.in.live.mu.Unlock()
	if _, err = f.in.SubmitAction(t.Context(), f.session.Token, 1, writerAction(f.page, "new exposure content")); err != nil {
		t.Fatal(err)
	}
	ws, _ := liveDial(t, f.in, "public", nil)
	liveSend(t, ws, map[string]any{"type": "hello", "version": 1, "pending": map[string]any{"count": 0}, "page": f.page.ID, "spill": spill.Cursor(), "next_window": 0, "watermark": "2"})
	liveUntil(t, ws, "welcome")
	for {
		v := liveUntil(t, ws, "snapshot")
		var scope liveScope
		_ = json.Unmarshal(v["scope"], &scope)
		if scope.Kind == "page" {
			if string(v["spill"]) == fmt.Sprintf("%q", spill.Cursor()) {
				t.Fatal("older Public spill accepted with a newer watermark")
			}
			break
		}
	}
}

func TestLiveHydratedViewNotices(t *testing.T) {
	f := newWriterFixture(t)
	var db, view ID
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		result, err := applyEdit(t.Context(), tx, editOptions{Seq: 2, Now: f.in.now()}, []Operation{CreateDatabaseOp{CreateFields: CreateFields{Ref: "db"}, BlockRef: "block", Place: Placement{Page: Target{Ref: &f.page}}, Title: "Tasks"}})
		if err != nil {
			return err
		}
		db = result.Refs["db"].ID
		view = result.Refs["db.view"].ID
		_, err = tx.ExecContext(t.Context(), `INSERT INTO view_notices(view_id,seq,removed) VALUES(?,'2','["Removed filter"]')`, view)
		if err != nil {
			return err
		}
		return setSequence(t.Context(), tx, 2)
	}); err != nil {
		t.Fatal(err)
	}
	ws, _ := liveDial(t, f.in, "private", nil)
	liveHelloSend(t, ws, f.session.Token, f.page.ID)
	liveUntil(t, ws, "welcome")
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO view_notices(view_id,seq,removed) VALUES(?,'3','["Later removal"]')`, view); err != nil {
			return err
		}
		if err := setSequence(t.Context(), tx, 3); err != nil {
			return err
		}
		return saveDelta(t.Context(), tx, 3, editResult{}, "", f.in.now())
	}); err != nil {
		t.Fatal(err)
	}
	later := liveUntil(t, ws, "view_adjustment")
	if string(later["notice_seq"]) != `"3"` {
		t.Fatalf("later notice=%s", later)
	}
	liveSend(t, ws, map[string]any{"type": "watch", "generation": 2, "page": f.page.ID, "visible_databases": []ID{db}, "pending_scopes": []any{}})
	v := liveUntil(t, ws, "view_adjustment")
	if string(v["view"]) != fmt.Sprintf("%q", view) || string(v["notice_seq"]) != `"2"` || !strings.Contains(string(v["removed"]), "Removed filter") {
		t.Fatalf("notice=%s", v)
	}
	for _, access := range []string{"private", "public"} {
		next, _ := liveDial(t, f.in, access, nil)
		session := f.session.Token
		if access == "public" {
			session = ""
		}
		liveHelloSend(t, next, session, f.page.ID)
		liveUntil(t, next, "welcome")
		liveSend(t, next, map[string]any{"type": "watch", "generation": 2, "page": f.page.ID, "visible_databases": []ID{db}, "pending_scopes": []any{}})
		final := false
		seen := map[string]bool{}
		for {
			frame := receiveLiveFrame(t, next)
			if liveType(frame) == "snapshot" && string(frame["final_window"]) == "true" {
				final = true
			}
			if liveType(frame) == "view_adjustment" {
				if access == "public" || !final {
					t.Fatalf("out-of-order or Public notice: %s", frame)
				}
				seen[string(frame["notice_seq"])] = true
			}
			if access == "public" && final || len(seen) == 2 {
				break
			}
		}
	}
	// Rehydration in one connection does not repeat already delivered notices.
	liveSend(t, ws, map[string]any{"type": "watch", "generation": 3, "page": f.page.ID, "visible_databases": []ID{db}, "pending_scopes": []any{}})
	for {
		frame := receiveLiveFrame(t, ws)
		if liveType(frame) == "view_adjustment" {
			t.Fatalf("duplicate adjustment: %s", frame)
		}
		if liveType(frame) == "snapshot" && string(frame["final_window"]) == "true" {
			break
		}
	}
	_ = ws.NetConn().SetReadDeadline(time.Now().Add(80 * time.Millisecond))
	for {
		raw, err := ws.ReadMessage(liveServerFrame)
		if err != nil {
			break
		}
		if strings.Contains(raw, `"type":"view_adjustment"`) {
			t.Fatalf("duplicate adjustment: %s", raw)
		}
	}
}

func sendLiveAction(t *testing.T, ws *wsconn.Conn, a Action) {
	t.Helper()
	raw, err := EncodeAction(a)
	if err != nil {
		t.Fatal(err)
	}
	var frame map[string]any
	if err = json.Unmarshal(raw, &frame); err != nil {
		t.Fatal(err)
	}
	frame["type"] = "action"
	liveSend(t, ws, frame)
}

func createLiveBlock(t *testing.T, f writerFixture) Ref {
	t.Helper()
	result, err := f.in.Update(t.Context(), Update{Slug: "notes", OpID: "block", Ops: []Operation{CreateBlockOp{CreateFields: CreateFields{Ref: "block"}, Place: Placement{Page: editTarget(f.page)}, Content: Content{Kind: Paragraph, Text: []Run{{Text: "initial"}}}}}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return result.Refs["block"].Ref
}

func TestLiveReplayBypassesUnrelatedCapture(t *testing.T) {
	f := newWriterFixture(t)
	block := createLiveBlock(t, f)
	result, err := f.in.Update(t.Context(), Update{Slug: "notes", OpID: "unrelated-page", Ops: []Operation{CreatePageOp{CreateFields: CreateFields{Ref: "page"}, Title: "Unrelated"}}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	unrelated := result.Refs["page"].Ref
	base := Version(2)
	a := Action{N: 1, ActionID: "content", Ops: []Operation{SetBlockOp{Block: editTarget(block), Content: Content{Kind: Paragraph, Text: []Run{{Text: "required canonical content"}}}, BaseV: &base}}}
	if _, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, a); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer close(release)
	f.in.spills.wrapWriter = func(w io.Writer) io.Writer {
		file := w.(*os.File)
		var blocked bool
		f.in.spills.mu.Lock()
		for _, s := range f.in.spills.spills {
			if s.dir == filepath.Dir(file.Name()) && s.scope.Page == unrelated.ID {
				blocked = true
			}
		}
		f.in.spills.mu.Unlock()
		if !blocked {
			return w
		}
		return liveCaptureGateWriter{w, func() { once.Do(func() { close(entered) }); <-release }}
	}
	ws, _ := liveDial(t, f.in, "private", nil)
	liveSend(t, ws, map[string]any{"type": "hello", "version": 1, "session": f.session.Token, "pending": map[string]any{"count": 1, "first_n": "1"}, "page": unrelated.ID, "pending_scopes": []liveScope{{Kind: "page", ID: f.page.ID}}})
	liveUntil(t, ws, "welcome")
	<-entered
	sendLiveAction(t, ws, a)
	var canonical bool
	for {
		v := receiveLiveFrame(t, ws)
		if liveType(v) == "snapshot" {
			var scope liveScope
			_ = json.Unmarshal(v["scope"], &scope)
			var data string
			_ = json.Unmarshal(v["data"], &data)
			canonical = canonical || scope.ID == f.page.ID && strings.Contains(data, "required canonical content")
		}
		if liveType(v) == "ack" {
			if !canonical {
				t.Fatal("replay ack preceded its required page while unrelated capture was blocked")
			}
			break
		}
	}
}

func TestLiveOverwriteAttributionSurvivesDeltaPruning(t *testing.T) {
	f := newWriterFixture(t)
	block := createLiveBlock(t, f)
	other, err := f.in.NewSession(t.Context(), "another-person", "Another", false)
	if err != nil {
		t.Fatal(err)
	}
	one, _ := liveDial(t, f.in, "private", nil)
	two, _ := liveDial(t, f.in, "private", nil)
	pub, _ := liveDial(t, f.in, "public", nil)
	liveHelloSend(t, one, f.session.Token, f.page.ID)
	liveUntil(t, one, "welcome")
	liveUntil(t, one, "snapshot")
	liveHelloSend(t, two, other.Token, f.page.ID)
	liveUntil(t, two, "welcome")
	liveUntil(t, two, "snapshot")
	liveHelloSend(t, pub, "", f.page.ID)
	liveUntil(t, pub, "welcome")
	liveUntil(t, pub, "snapshot")
	base := Version(2)
	a := Action{N: 1, ActionID: "mine", Ops: []Operation{SetBlockOp{Block: editTarget(block), Content: Content{Kind: Paragraph, Text: []Run{{Text: "MY TEXT"}}}, BaseV: &base}}}
	sendLiveAction(t, one, a)
	liveUntil(t, one, "ack")
	liveUntil(t, two, "delta")
	liveUntil(t, pub, "delta")
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error { _, err := tx.ExecContext(t.Context(), `DELETE FROM deltas`); return err }); err != nil {
		t.Fatal(err)
	}
	b := Action{N: 1, ActionID: "theirs", Ops: []Operation{SetBlockOp{Block: editTarget(block), Content: Content{Kind: Paragraph, Text: []Run{{Text: "THEIR TEXT"}}}, BaseV: &base}}}
	sendLiveAction(t, two, b)
	liveUntil(t, two, "ack")
	d := liveUntil(t, one, "delta")
	var displaced []struct {
		Block   Ref     `json:"block"`
		Version Version `json:"version"`
		Base    Version `json:"base"`
		Source  struct {
			Session string   `json:"session_id"`
			N       Sequence `json:"n"`
		} `json:"source"`
	}
	if err = json.Unmarshal(d["overwrote"], &displaced); err != nil || len(displaced) != 1 {
		t.Fatalf("overwrite missing: %s, %v", d, err)
	}
	if displaced[0].Source.Session != f.session.ID || displaced[0].Source.N != 1 || displaced[0].Version != 3 || displaced[0].Block != block {
		t.Fatalf("wrong displaced receipt: %+v", displaced)
	}
	raw, _ := json.Marshal(d)
	if strings.Contains(string(raw), f.session.Token) || strings.Contains(string(raw), other.Token) {
		t.Fatal("resume token leaked")
	}
	publicDelta := liveUntil(t, pub, "delta")
	var projected any
	raw, _ = json.Marshal(publicDelta)
	_ = json.Unmarshal(raw, &projected)
	if !publicValueClean(projected) {
		t.Fatalf("private attribution in Public: %s", raw)
	}
}
func createLiveDatabase(t *testing.T, f writerFixture, count int) (ID, Ref, Ref) {
	t.Helper()
	var database ID
	var title, last Ref
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		opts := editOptions{Seq: 2, Now: f.in.now()}
		result, err := applyEdit(t.Context(), tx, opts, []Operation{CreateDatabaseOp{CreateFields: CreateFields{Ref: "db"}, BlockRef: "owner", Place: Placement{Page: editTarget(f.page)}, Title: "Tasks"}})
		if err != nil {
			return err
		}
		db := result.Refs["db"]
		database = db.ID
		title = result.Refs["db.title"]
		text := strings.Repeat("x", 400)
		for i := 0; i < count; i++ {
			row := Item{Ref: Ref{ID: spillID(RowKind, uint64(i+1)), Inc: 2}, Ver: 2, V: map[GroupKey]Version{}, Data: Row{Database: database, Position: fmt.Sprintf("%08d", i+1), Cells: map[ID]CellValue{title.ID: {Type: TitleType, Value: text}}}}
			if err := putItem(t.Context(), tx, row); err != nil {
				return err
			}
			last = row.Ref
		}
		return setSequence(t.Context(), tx, 2)
	}); err != nil {
		t.Fatal(err)
	}
	return database, title, last
}

type liveWindowGateWriter struct {
	http.ResponseWriter
	database         ID
	entered, release chan struct{}
	once             *sync.Once
}

func (w liveWindowGateWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err != nil {
		return nil, nil, err
	}
	return &liveWindowGateConn{Conn: c, database: w.database, entered: w.entered, release: w.release, once: w.once}, rw, nil
}

type liveWindowGateConn struct {
	net.Conn
	database         ID
	entered, release chan struct{}
	once             *sync.Once
}

func (c *liveWindowGateConn) Write(p []byte) (int, error) {
	if len(p) > 2 && p[0] == '{' {
		var frame struct {
			Type   string    `json:"type"`
			Scope  liveScope `json:"scope"`
			Window int       `json:"window"`
		}
		if json.Unmarshal(p, &frame) == nil && frame.Type == "snapshot" && frame.Scope.ID == c.database && frame.Window > 0 {
			c.once.Do(func() { close(c.entered); <-c.release })
		}
	}
	return c.Conn.Write(p)
}

func TestLiveSlowWindowsYieldToDeltas(t *testing.T) {
	f := newWriterFixture(t)
	database, _, _ := createLiveDatabase(t, f, 1001)
	entered, release := make(chan struct{}), make(chan struct{})
	var once, unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(release) }) }
	defer unblock()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Flats-Access", "private")
		r.Header.Set("Origin", "http://"+r.Host)
		f.in.ServeHTTP(liveWindowGateWriter{w, database, entered, release, &once}, r)
	}))
	defer server.Close()
	ws, err := liveConnect(t.Context(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close(1000, "")
	var conn *liveConn
	deadline := time.Now().Add(time.Second)
	for conn == nil && time.Now().Before(deadline) {
		f.in.live.mu.Lock()
		for c := range f.in.live.conns {
			conn = c
		}
		f.in.live.mu.Unlock()
		if conn == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if conn == nil {
		t.Fatal("connection not registered")
	}
	conn.ws.Budget().Lower(128 << 10)
	liveHelloSend(t, ws, f.session.Token, f.page.ID)
	liveUntil(t, ws, "welcome")
	liveSend(t, ws, map[string]any{"type": "watch", "generation": 2, "page": f.page.ID, "visible_databases": []ID{database}, "pending_scopes": []any{}})
	// Let a blocked row frame saturate a snapshot-prefetching implementation.
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("row window not started")
	}
	time.Sleep(100 * time.Millisecond)
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		record, err := loadRecord(t.Context(), tx, f.page.ID)
		if err != nil {
			return err
		}
		page := record.Item.Data.(Page)
		page.Title = "concurrent title"
		record.Item.Data = page
		record.Item.Ver = 3
		record.Item.V[TitleGroup] = 3
		if err = putRecord(t.Context(), tx, record); err != nil {
			return err
		}
		if err = setSequence(t.Context(), tx, 3); err != nil {
			return err
		}
		return saveDelta(t.Context(), tx, 3, editResult{Records: map[ID]itemRecord{f.page.ID: record}}, "", f.in.now())
	}); err != nil {
		t.Fatal(err)
	}
	accepted := make(chan bool, 1)
	go func() {
		accepted <- conn.command(liveOutbound{kind: "activity", value: map[string]any{"message": "delivery probe"}})
	}()
	select {
	case ok := <-accepted:
		if !ok {
			t.Fatal("connection closed")
		}
	// The snapshot frame stays blocked until unblock, so a pump that waits on
	// it never accepts the command; the bound only has to outlast a loaded
	// race run.
	case <-time.After(10 * time.Second):
		t.Fatal("snapshot backpressure blocked live pump")
	}
	if used := conn.ws.Budget().Used(); used > liveServerFrame {
		t.Fatalf("snapshot prefetch retained %d bytes", used)
	}
	unblock()
	snapshots := 0
	for {
		v := receiveLiveFrame(t, ws)
		if liveType(v) == "resync" {
			t.Fatal("slow hydration forced resync")
		}
		if liveType(v) == "snapshot" {
			var scope liveScope
			_ = json.Unmarshal(v["scope"], &scope)
			if scope.ID == database {
				snapshots++
			}
		}
		if liveType(v) == "delta" {
			if snapshots > 2 {
				t.Fatalf("delta queued behind %d snapshot parts", snapshots)
			}
			break
		}
	}
	for {
		v := receiveLiveFrame(t, ws)
		if liveType(v) == "resync" {
			t.Fatal("slow hydration forced resync")
		}
		if liveType(v) == "snapshot" && string(v["final_window"]) == "true" {
			break
		}
	}
}

func TestLiveReplayUsesOnlyRequiredRowWindow(t *testing.T) {
	t.Parallel()
	f := newWriterFixture(t)
	database, title, last := createLiveDatabase(t, f, 1)
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		r, err := loadRecord(t.Context(), tx, last.ID)
		if err != nil {
			return err
		}
		row := r.Item.Data.(Row)
		row.Position = "99999999"
		r.Item.Data = row
		return putRecord(t.Context(), tx, r)
	}); err != nil {
		t.Fatal(err)
	}
	text := "required last-window row"
	a := Action{N: 1, ActionID: "row", Ops: []Operation{SetCellOp{Row: editTarget(last), Cell: CellInput{Property: editTarget(title), Value: CellInputValue{Text: &text}}}}}
	if _, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, a); err != nil {
		t.Fatal(err)
	}
	// Add earlier windows after the small receipt-producing edit.
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		if err := f.in.closeGroups(t.Context(), tx, true); err != nil {
			return err
		}
		for i := 2; i <= 2001; i++ {
			row := Item{Ref: Ref{ID: spillID(RowKind, uint64(i)), Inc: 4}, Ver: 4, V: map[GroupKey]Version{}, Data: Row{Database: database, Position: fmt.Sprintf("%08d", i), Cells: map[ID]CellValue{title.ID: {Type: TitleType, Value: "unrelated"}}}}
			if err := putItem(t.Context(), tx, row); err != nil {
				return err
			}
		}
		if err := setSequence(t.Context(), tx, 4); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), `DELETE FROM deltas`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ws, _ := liveDial(t, f.in, "private", nil)
	liveSend(t, ws, map[string]any{"type": "hello", "version": 1, "session": f.session.Token, "pending": map[string]any{"count": 1, "first_n": "1"}, "page": f.page.ID, "pending_scopes": []liveScope{{Kind: "database", ID: database}}})
	liveUntil(t, ws, "welcome")
	sendLiveAction(t, ws, a)
	var required bool
	var units = map[int]bool{}
	for {
		v := receiveLiveFrame(t, ws)
		if liveType(v) == "snapshot" {
			var scope liveScope
			_ = json.Unmarshal(v["scope"], &scope)
			if scope.ID == database {
				var window int
				_ = json.Unmarshal(v["window"], &window)
				units[window] = true
				var data string
				_ = json.Unmarshal(v["data"], &data)
				required = required || strings.Contains(data, "required last-window row")
			}
		}
		if liveType(v) == "ack" {
			if !required || !units[0] || !units[3] {
				t.Fatalf("ack before required schema/row window: %v", units)
			}
			if units[1] || units[2] {
				t.Fatalf("unrelated row windows delayed replay ack: %v", units)
			}
			break
		}
	}
}
func receiveLiveFrame(t *testing.T, ws *wsconn.Conn) map[string]json.RawMessage {
	t.Helper()
	_ = ws.NetConn().SetReadDeadline(time.Now().Add(30 * time.Second))
	raw, err := ws.ReadMessage(liveServerFrame)
	if err != nil {
		t.Fatal(err)
	}
	var frame map[string]json.RawMessage
	if err = json.Unmarshal([]byte(raw), &frame); err != nil {
		t.Fatal(err)
	}
	return frame
}
func TestLiveReplayDeletedDatabaseSettles(t *testing.T) {
	f := newWriterFixture(t)
	database, _, _ := createLiveDatabase(t, f, 1)
	var owner Ref
	if err := f.in.ReadTx(t.Context(), func(tx *sql.Tx, _ Sequence) error {
		var id ID
		if err := tx.QueryRowContext(t.Context(), `SELECT block FROM databases WHERE id=?`, database).Scan(&id); err != nil {
			return err
		}
		record, err := loadRecord(t.Context(), tx, id)
		owner = record.Item.Ref
		return err
	}); err != nil {
		t.Fatal(err)
	}
	a := Action{N: 1, ActionID: "delete-database", Ops: []Operation{DeleteBlocksOp{Blocks: []Target{editTarget(owner)}}}}
	if _, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, a); err != nil {
		t.Fatal(err)
	}
	ws, _ := liveDial(t, f.in, "private", nil)
	liveSend(t, ws, map[string]any{"type": "hello", "version": 1, "session": f.session.Token, "pending": map[string]any{"count": 1, "first_n": "1"}, "page": f.page.ID, "pending_scopes": []liveScope{{Kind: "database", ID: database}}})
	liveUntil(t, ws, "welcome")
	sendLiveAction(t, ws, a)
	var empty bool
	for {
		frame := receiveLiveFrame(t, ws)
		if liveType(frame) == "fatal" || liveType(frame) == "reject" {
			t.Fatalf("deleted database cannot settle: %s", frame)
		}
		if liveType(frame) == "snapshot" {
			var scope liveScope
			_ = json.Unmarshal(frame["scope"], &scope)
			if scope.ID == database {
				var data string
				_ = json.Unmarshal(frame["data"], &data)
				empty = string(frame["final_window"]) == "true" && data == `{"items":[]}`
			}
		}
		if liveType(frame) == "ack" {
			if !empty {
				t.Fatal("ack before authoritative database absence")
			}
			break
		}
	}
}
func TestLiveActionGapStops(t *testing.T) {
	f := newWriterFixture(t)
	ws, _ := liveDial(t, f.in, "private", nil)
	liveHelloSend(t, ws, f.session.Token, f.page.ID)
	liveUntil(t, ws, "welcome")
	a := writerAction(f.page, "gap")
	a.N = 2
	sendLiveAction(t, ws, a)
	frame := liveUntil(t, ws, "fatal")
	if string(frame["code"]) != `"protocol"` {
		t.Fatalf("gap did not stop the connection: %s", frame)
	}
	if writerCount(t, f.in, "receipts") != 0 {
		t.Fatal("gap consumed an action number")
	}
}
