package notebook

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/wsconn"
)

func liveDial(t *testing.T, in *Instance, access string, epoch context.Context) (*wsconn.Conn, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Flats-Access", access)
		r.Header.Set("Origin", "http://"+r.Host)
		if epoch != nil {
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			stop := context.AfterFunc(epoch, cancel)
			defer stop()
			r = r.WithContext(ctx)
		}
		in.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	ws, err := liveConnect(t.Context(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close(1000, ""); <-ws.Done() })
	return ws, server
}
func liveConnect(ctx context.Context, base string) (*wsconn.Conn, error) {
	host := strings.TrimPrefix(base, "http://")
	return wsconn.Dial(ctx, func(ctx context.Context) (net.Conn, error) { return new(net.Dialer).DialContext(ctx, "tcp", host) }, host, "/_notebook/ws")
}

func liveSend(t *testing.T, ws *wsconn.Conn, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteFrame(wsconn.OpText, raw); err != nil {
		t.Fatal(err)
	}
}
func liveReceive(t *testing.T, ws *wsconn.Conn) map[string]json.RawMessage {
	t.Helper()
	_ = ws.NetConn().SetReadDeadline(time.Now().Add(4 * time.Second))
	raw, err := ws.ReadMessage(liveServerFrame)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}
func liveType(fields map[string]json.RawMessage) string {
	var kind string
	_ = json.Unmarshal(fields["type"], &kind)
	return kind
}
func liveUntil(t *testing.T, ws *wsconn.Conn, kind string) map[string]json.RawMessage {
	t.Helper()
	for i := 0; i < 50; i++ {
		f := liveReceive(t, ws)
		if liveType(f) == kind {
			return f
		}
		if liveType(f) == "fatal" || liveType(f) == "reject" {
			t.Fatalf("unexpected frame: %s", mustLiveEnvelope("frame", f))
		}
	}
	t.Fatal("frame not received")
	return nil
}
func liveHelloSend(t *testing.T, ws *wsconn.Conn, session string, page ID) {
	t.Helper()
	hello := map[string]any{"type": "hello", "version": 1, "pending": map[string]any{"count": 0}}
	if session != "" {
		hello["session"] = session
	}
	if page != "" {
		hello["page"] = page
	}
	liveSend(t, ws, hello)
}

func TestLiveSavedAfterCommitAndReconnect(t *testing.T) {
	f := newWriterFixture(t)
	ws, server := liveDial(t, f.in, "private", nil)
	liveHelloSend(t, ws, f.session.Token, f.page.ID)
	liveUntil(t, ws, "welcome")
	liveUntil(t, ws, "snapshot")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.in.hook = func(stage string, _ *sql.Tx) {
		if stage == "commit" {
			once.Do(func() { close(entered); <-release })
		}
	}
	action := writerAction(f.page, "Saved over socket")
	raw, err := EncodeAction(action)
	if err != nil {
		t.Fatal(err)
	}
	var frame map[string]any
	_ = json.Unmarshal(raw, &frame)
	frame["type"] = "action"
	liveSend(t, ws, frame)
	<-entered
	if got := writerCount(t, f.in, "receipts"); got != 0 {
		t.Fatalf("receipt visible before commit: %d", got)
	}
	close(release)
	var sawDelta bool
	var ack map[string]json.RawMessage
	for {
		v := liveReceive(t, ws)
		if liveType(v) == "delta" {
			sawDelta = true
		}
		if liveType(v) == "ack" {
			ack = v
			break
		}
	}
	if !sawDelta {
		t.Fatal("ack preceded canonical delta")
	}
	ws.Close(1000, "")
	<-ws.Done()
	ws2, err := liveConnect(t.Context(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer ws2.Close(1000, "")
	liveHelloSend(t, ws2, f.session.Token, f.page.ID)
	liveUntil(t, ws2, "welcome")
	liveUntil(t, ws2, "snapshot")
	liveSend(t, ws2, frame)
	replay := liveUntil(t, ws2, "ack")
	if string(ack["seq"]) != string(replay["seq"]) || string(ack["summary"]) != string(replay["summary"]) {
		t.Fatalf("different replay: %s / %s", ack, replay)
	}
	if got := writerCount(t, f.in, "receipts"); got != 1 {
		t.Fatalf("replayed action executed again: %d", got)
	}
}

func TestLiveConsecutiveTwoConnections(t *testing.T) {
	f := newWriterFixture(t)
	one, _ := liveDial(t, f.in, "private", nil)
	two, _ := liveDial(t, f.in, "private", nil)
	for _, ws := range []*wsconn.Conn{one, two} {
		liveHelloSend(t, ws, f.session.Token, f.page.ID)
		liveUntil(t, ws, "welcome")
		liveUntil(t, ws, "snapshot")
	}
	for n := 1; n <= 3; n++ {
		a := writerAction(f.page, fmt.Sprint(n))
		a.N = Sequence(n)
		a.ActionID = fmt.Sprint(n)
		if _, err := f.in.SubmitAction(t.Context(), f.session.Token, uint64(n), a); err != nil {
			t.Fatal(err)
		}
	}
	for _, ws := range []*wsconn.Conn{one, two} {
		for seq := Sequence(2); seq <= 4; seq++ {
			v := liveUntil(t, ws, "delta")
			var got Sequence
			_ = json.Unmarshal(v["seq"], &got)
			if got != seq {
				t.Fatalf("seq=%v want %v", got, seq)
			}
		}
	}
}

func TestLivePublicProjectionAndRefusals(t *testing.T) {
	f := newWriterFixture(t)
	ws, _ := liveDial(t, f.in, "public", nil)
	liveHelloSend(t, ws, "", f.page.ID)
	welcome := liveUntil(t, ws, "welcome")
	if _, ok := welcome["session"]; ok {
		t.Fatal("public session")
	}
	snapshot := liveUntil(t, ws, "snapshot")
	var fragment string
	_ = json.Unmarshal(snapshot["data"], &fragment)
	if strings.Contains(fragment, `"inc"`) || strings.Contains(fragment, `"ver"`) {
		t.Fatalf("private stamps: %s", fragment)
	}
	if _, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, writerAction(f.page, "Public live")); err != nil {
		t.Fatal(err)
	}
	delta := liveUntil(t, ws, "delta")
	raw, _ := json.Marshal(delta)
	if !publicDeltaClean(raw) {
		t.Fatalf("private metadata: %s", raw)
	}
	for _, kind := range []string{"action", "presence", "hello"} {
		publicWS, _ := liveDial(t, f.in, "public", nil)
		liveHelloSend(t, publicWS, "", f.page.ID)
		liveUntil(t, publicWS, "welcome")
		frame := map[string]any{"type": kind}
		expected, code := "fatal", `"protocol"`
		if kind == "action" {
			frame["n"] = "1"
			expected, code = "reject", `"forbidden"`
		}
		liveSend(t, publicWS, frame)
		refusal := liveUntil(t, publicWS, expected)
		if string(refusal["code"]) != code {
			t.Fatalf("public %s accepted: %s", kind, refusal)
		}
	}
	if got := writerCount(t, f.in, "receipts"); got != 1 {
		t.Fatalf("public mutation: %d", got)
	}
}

func TestLiveRouteCancellationDeletesSpills(t *testing.T) {
	f := newWriterFixture(t)
	epoch, cancel := context.WithCancel(t.Context())
	ws, _ := liveDial(t, f.in, "public", epoch)
	liveHelloSend(t, ws, "", f.page.ID)
	liveUntil(t, ws, "welcome")
	s, err := f.in.spills.Capture(t.Context(), f.in.ReadTx, Scope{Tree: true, Public: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.in.live.mu.Lock()
	f.in.live.resumes[s.Cursor()] = liveResume{spill: s, public: true}
	f.in.live.mu.Unlock()
	cancel()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := f.in.spills.Lookup(s.Cursor()); !ok {
			if _, err := os.Stat(s.dir); !os.IsNotExist(err) {
				t.Fatalf("spill remains: %v", err)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("route cancellation retained spill")
}

func publicDeltaClean(data []byte) bool {
	var value any
	if json.Unmarshal(data, &value) != nil {
		return false
	}
	return publicValueClean(value)
}
func publicValueClean(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		for k, child := range v {
			switch k {
			case "inc", "ver", "v", "from", "overwrote", "store", "session", "session_id", "editors", "reset":
				return false
			}
			if !publicValueClean(child) {
				return false
			}
		}
	case []any:
		for _, child := range v {
			if !publicValueClean(child) {
				return false
			}
		}
	}
	return true
}

func TestLiveMultipartBoundsAndAssembly(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"changes": strings.Repeat("\\\n한", 300000)})
	var parts []string
	err := liveParts(raw, "delta_part", 3, 2, func(frame []byte) error {
		if len(frame) > liveServerFrame {
			t.Fatalf("frame %d", len(frame))
		}
		var f struct {
			Data string `json:"data"`
		}
		if err := json.Unmarshal(frame, &f); err != nil {
			return err
		}
		parts = append(parts, f.Data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(parts, "") != string(raw) {
		t.Fatal("multipart changed payload")
	}
	var a liveAssembly
	defer a.reset()
	action := `{"type":"action","n":"1","action_id":"x","ops":[]}`
	for i, part := range []string{action[:20], action[20:]} {
		b, _ := json.Marshal(map[string]any{"type": "action_part", "n": "1", "part": i, "parts": 2, "data": part})
		got, err := a.add(b, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 && string(got) != action {
			t.Fatalf("assembled %s", got)
		}
	}
	if liveAssemblyBytes.Load() != 0 {
		t.Fatal("assembly budget leak")
	}
}

func TestLivePresenceNoopAndShutdown(t *testing.T) {
	f := newWriterFixture(t)
	one, _ := liveDial(t, f.in, "private", nil)
	two, _ := liveDial(t, f.in, "private", nil)
	for _, ws := range []*wsconn.Conn{one, two} {
		liveHelloSend(t, ws, f.session.Token, f.page.ID)
		liveUntil(t, ws, "welcome")
		liveUntil(t, ws, "snapshot")
	}
	liveSend(t, one, map[string]any{"type": "presence", "page": f.page.ID, "edit": nil})
	for {
		p := liveUntil(t, two, "presence")
		var editors []livePresence
		_ = json.Unmarshal(p["editors"], &editors)
		if len(editors) > 0 {
			if editors[0].Label != f.session.Label || !editors[0].Verified {
				t.Fatalf("presence %s", p)
			}
			break
		}
	}
	// An accepted empty action still consumes a sequence and has a canonical delta.
	empty := Action{N: 1, ActionID: "empty", Ops: []Operation{}}
	if _, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, empty); err != nil {
		t.Fatal(err)
	}
	for _, ws := range []*wsconn.Conn{one, two} {
		d := liveUntil(t, ws, "delta")
		if string(d["seq"]) != `"2"` {
			t.Fatalf("empty delta %s", d)
		}
	}
	f.in.Shutdown("renamed")
	fatal := liveUntil(t, one, "fatal")
	if string(fatal["code"]) != `"renamed"` {
		t.Fatalf("shutdown %s", fatal)
	}
}

func TestLiveResumeSpillAndCoverage(t *testing.T) {
	f := newWriterFixture(t)
	spill, err := f.in.spills.Capture(t.Context(), f.in.ReadTx, Scope{Tree: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.in.live.mu.Lock()
	f.in.live.resumes[spill.Cursor()] = liveResume{spill: spill, session: f.session.ID}
	f.in.live.mu.Unlock()
	if _, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, writerAction(f.page, "After capture")); err != nil {
		t.Fatal(err)
	}
	ws, _ := liveDial(t, f.in, "private", nil)
	liveSend(t, ws, map[string]any{"type": "hello", "version": 1, "session": f.session.Token, "pending": map[string]any{"count": 0}, "watermark": spill.Sequence, "spill": spill.Cursor(), "next_window": 0})
	liveUntil(t, ws, "welcome")
	delta := liveUntil(t, ws, "delta")
	if string(delta["seq"]) != `"2"` {
		t.Fatalf("resume delta %s", delta)
	}
	snapshot := liveUntil(t, ws, "snapshot")
	var token string
	_ = json.Unmarshal(snapshot["spill"], &token)
	if token != spill.Cursor() {
		t.Fatal("did not resume original spill")
	}
	if string(snapshot["seq"]) != `"1"` {
		t.Fatalf("resume changed snapshot: %s", snapshot)
	}
}

func TestLiveCrossPageMoveUsesCommittedLocations(t *testing.T) {
	f := newWriterFixture(t)
	result := liveReadUpdate(t, f, "move-setup", CreatePageOp{CreateFields: CreateFields{Ref: "other"}, Title: "Other"}, CreateBlockOp{CreateFields: CreateFields{Ref: "block"}, Place: Placement{Page: Target{Ref: &f.page}}, Content: Content{Kind: Paragraph, Text: []Run{{Text: "Moved", Marks: []Mark{}}}}})
	other, block := result.Refs["other"].Ref, result.Refs["block"].Ref
	one, _ := liveDial(t, f.in, "private", nil)
	two, _ := liveDial(t, f.in, "private", nil)
	for i, ws := range []*wsconn.Conn{one, two} {
		page := f.page.ID
		if i == 1 {
			page = other.ID
		}
		liveHelloSend(t, ws, f.session.Token, page)
		liveUntil(t, ws, "welcome")
		liveUntil(t, ws, "snapshot")
	}
	liveReadUpdate(t, f, "move-action", MoveBlocksOp{Blocks: []Target{{Ref: &block}}, Place: Placement{Page: Target{Ref: &other}}})
	for i, ws := range []*wsconn.Conn{one, two} {
		delta := liveUntil(t, ws, "delta")
		var changes []struct {
			Ref     Ref  `json:"ref"`
			Deleted bool `json:"deleted"`
			Page    ID   `json:"page"`
		}
		if err := json.Unmarshal(delta["changes"], &changes); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, change := range changes {
			if change.Ref.ID == block.ID {
				found = true
				if i == 0 && !change.Deleted {
					t.Fatal("old scope kept moved block")
				}
				if i == 1 && (change.Deleted || change.Page != other.ID) {
					t.Fatal("new scope missing moved block")
				}
			}
		}
		if !found {
			t.Fatalf("move absent: %s", delta)
		}
	}
}

func TestLiveQueueOverflowResyncThenClose(t *testing.T) {
	f := newWriterFixture(t)
	ws, _ := liveDial(t, f.in, "private", nil)
	liveHelloSend(t, ws, f.session.Token, f.page.ID)
	liveUntil(t, ws, "welcome")
	liveUntil(t, ws, "snapshot")
	// Retention bytes make lag deterministic without relying on OS socket buffers.
	add := func(n Sequence) {
		t.Helper()
		if err := f.in.write(t.Context(), func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO deltas(seq,effects,source,bytes,at) VALUES(?,?,'',?,?)`, sequenceString(n), `{"changes":[]}`, livePrivateQueue+1, f.in.now().Unix()); err != nil {
				return err
			}
			return setSequence(t.Context(), tx, n)
		}); err != nil {
			t.Fatal(err)
		}
	}
	add(2)
	resync := liveUntil(t, ws, "resync")
	if string(resync["generation"]) != "2" {
		t.Fatalf("resync %s", resync)
	}
	liveUntil(t, ws, "snapshot")
	add(3)
	fatal := liveUntil(t, ws, "fatal")
	if string(fatal["code"]) != `"live_backpressure"` {
		t.Fatalf("second overflow %s", fatal)
	}
}
