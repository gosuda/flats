package notebook

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/wsconn"
)

func TestLiveUndecodableActionResends(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    any
	}{
		{"missing", nil},
		{"object", map[string]bool{"bad": true}},
		{"zero", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWriterFixture(t)
			ws, _ := liveDial(t, f.in, "private", nil)
			liveHelloSend(t, ws, f.session.Token, f.page.ID)
			liveUntil(t, ws, "welcome")
			liveSend(t, ws, map[string]any{"type": "action", "n": tc.n, "ops": []any{}})
			for {
				raw, err := ws.ReadMessage(liveServerFrame)
				if err != nil {
					var closed *wsconn.CloseError
					if !errors.As(err, &closed) || closed.Code != 1008 || closed.Reason != "resend" {
						t.Fatalf("expected retryable protocol close, got %v", err)
					}
					break
				}
				if strings.Contains(raw, `"type":"fatal"`) || strings.Contains(raw, `"type":"reject"`) {
					t.Fatalf("undecodable action terminated the session: %s", raw)
				}
			}
			resumed, _ := liveDial(t, f.in, "private", nil)
			liveHelloSend(t, resumed, f.session.Token, f.page.ID)
			liveUntil(t, resumed, "welcome")
			a := writerAction(f.page, "resubmitted")
			sendLiveAction(t, resumed, a)
			liveUntil(t, resumed, "ack")
			sendLiveAction(t, resumed, a)
			liveUntil(t, resumed, "ack")
			a.N, a.ActionID = 2, "following"
			sendLiveAction(t, resumed, a)
			liveUntil(t, resumed, "ack")
			if got := writerCount(t, f.in, "receipts"); got != 2 {
				t.Fatalf("replayed action was not exactly once: %d receipts", got)
			}
		})
	}
}

func TestLiveRefusedAssemblyRetainsAction(t *testing.T) {
	f := newWriterFixture(t)
	ws, _ := liveDial(t, f.in, "private", nil)
	liveHelloSend(t, ws, f.session.Token, f.page.ID)
	liveUntil(t, ws, "welcome")
	a := writerAction(f.page, "multipart retry")
	raw, err := EncodeAction(a)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	fields["type"] = json.RawMessage(`"action"`)
	raw, _ = json.Marshal(fields)
	part := func(index int) map[string]any {
		data := raw[:len(raw)/2]
		if index == 1 {
			data = raw[len(raw)/2:]
		}
		return map[string]any{"type": "action_part", "n": "1", "parts": 2, "part": index, "data": string(data)}
	}
	liveAssemblyBytes.Add(liveHostAssemblyMax)
	var budgetOnce sync.Once
	releaseBudget := func() { budgetOnce.Do(func() { liveAssemblyBytes.Add(-liveHostAssemblyMax) }) }
	defer releaseBudget()
	liveSend(t, ws, part(0))
	refusal := liveUntil(t, ws, "reject")
	releaseBudget()
	if string(refusal["n"]) != `"1"` || string(refusal["code"]) != `"unavailable"` || string(refusal["retry"]) != "true" {
		t.Fatalf("expected correlated retryable refusal: %s", refusal)
	}
	liveSend(t, ws, part(1))
	liveSend(t, ws, part(0))
	liveSend(t, ws, part(1))
	liveUntil(t, ws, "ack")
	liveSend(t, ws, part(0))
	liveSend(t, ws, part(1))
	liveUntil(t, ws, "ack")
	a.N, a.ActionID = 2, "following"
	sendLiveAction(t, ws, a)
	liveUntil(t, ws, "ack")
	if got := writerCount(t, f.in, "receipts"); got != 2 {
		t.Fatalf("retry did not apply exactly once: %d receipts", got)
	}
}

func TestLiveReplayDoesNotBlockCommands(t *testing.T) {
	t.Run("new action", func(t *testing.T) { assertLiveReplayCommands(t, false) })
	t.Run("replayed action", func(t *testing.T) { assertLiveReplayCommands(t, true) })
}

func assertLiveReplayCommands(t *testing.T, replaySecond bool) {
	f := newWriterFixture(t)
	block := createLiveBlock(t, f)
	base := Version(2)
	a := Action{N: 1, ActionID: "content", Ops: []Operation{SetBlockOp{Block: editTarget(block), Content: Content{Kind: Paragraph, Text: []Run{{Text: "required canonical content"}}}, BaseV: &base}}}
	if _, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, a); err != nil {
		t.Fatal(err)
	}
	second := writerAction(f.page, "independent tree title")
	second.N, second.ActionID = 2, "second"
	if replaySecond {
		if _, err := f.in.SubmitAction(t.Context(), f.session.Token, 2, second); err != nil {
			t.Fatal(err)
		}
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	f.in.spills.wrapWriter = func(w io.Writer) io.Writer {
		file := w.(*os.File)
		blocked := false
		f.in.spills.mu.Lock()
		for _, spill := range f.in.spills.spills {
			blocked = blocked || spill.dir == filepath.Dir(file.Name()) && spill.scope.Page == f.page.ID
		}
		f.in.spills.mu.Unlock()
		if blocked {
			return liveCaptureGateWriter{w, func() { once.Do(func() { close(entered) }); <-release }}
		}
		return w
	}
	ws, _ := liveDial(t, f.in, "private", nil)
	liveHelloSend(t, ws, f.session.Token, f.page.ID)
	liveUntil(t, ws, "welcome")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("required capture did not start")
	}
	sendLiveAction(t, ws, a)
	sendLiveAction(t, ws, second)
	var delta bool
	for {
		v := liveReceive(t, ws)
		if liveType(v) == "delta" {
			delta = true
		}
		if liveType(v) == "ack" {
			if string(v["n"]) == `"1"` {
				t.Fatal("replay ack preceded its required capture")
			}
			if string(v["n"]) == `"2"` {
				if !replaySecond && !delta {
					t.Fatal("new action ack preceded its delta")
				}
				break
			}
		}
	}
	liveSend(t, ws, map[string]any{"type": "watch", "generation": 2, "page": f.page.ID, "visible_databases": []ID{}, "pending_scopes": []liveScope{}})
	// A further action must pass the read loop while the first replay waits.
	second = writerAction(f.page, "third action saved")
	second.N, second.ActionID = 3, "third"
	sendLiveAction(t, ws, second)
	for {
		v := liveReceive(t, ws)
		if liveType(v) == "ack" {
			if string(v["n"]) != `"3"` {
				t.Fatalf("unexpected ack while capture blocked: %s", v)
			}
			break
		}
	}
	unblock()
	canonical := false
	for {
		v := liveReceive(t, ws)
		if liveType(v) == "snapshot" {
			var data string
			_ = json.Unmarshal(v["data"], &data)
			canonical = canonical || string(v["generation"]) == "2" && strings.Contains(data, "required canonical content")
		}
		if liveType(v) == "ack" && string(v["n"]) == `"1"` {
			if !canonical {
				t.Fatal("replay ack did not follow its canonical page")
			}
			break
		}
	}
}

type liveSocketGateWriter struct {
	http.ResponseWriter
	entered, release chan struct{}
	once             *sync.Once
}

func (w liveSocketGateWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err != nil {
		return nil, nil, err
	}
	return &liveSocketGateConn{Conn: conn, entered: w.entered, release: w.release, once: w.once}, rw, nil
}

type liveSocketGateConn struct {
	net.Conn
	entered, release chan struct{}
	once             *sync.Once
}

func (c *liveSocketGateConn) Write(p []byte) (int, error) {
	if len(p) > 0 && p[0] == '{' {
		c.once.Do(func() { close(c.entered); <-c.release })
	}
	return c.Conn.Write(p)
}

func TestLivePublicWithdrawalDiscardsQueuedContent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel func(*liveConn)
	}{
		{"withdrawal", func(*liveConn) {}},
		{"lifetime cancellation", func(c *liveConn) { c.cancel() }},
		{"graceful close", func(c *liveConn) { c.ws.Close(1001, "unavailable") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWriterFixture(t)
			entered, release := make(chan struct{}), make(chan struct{})
			var once, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r.Header.Set("X-Flats-Access", "public")
				r.Header.Set("Origin", "http://"+r.Host)
				f.in.ServeHTTP(liveSocketGateWriter{w, entered, release, &once}, r)
			}))
			defer server.Close()
			ws, err := liveConnect(t.Context(), server.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer ws.Close(1000, "")
			liveHelloSend(t, ws, "", f.page.ID)
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("transport not gated")
			}
			f.in.live.mu.Lock()
			var conn *liveConn
			for c := range f.in.live.conns {
				conn = c
			}
			f.in.live.mu.Unlock()
			if conn == nil {
				t.Fatal("Public connection not registered")
			}
			if err := conn.ws.Send(`{"type":"snapshot","data":"queued private-after-withdrawal marker"}`); err != nil {
				t.Fatal(err)
			}
			tc.cancel(conn)
			f.in.CancelPublic()
			unblock()
			_ = ws.NetConn().SetReadDeadline(time.Now().Add(3 * time.Second))
			for {
				raw, err := ws.ReadMessage(liveServerFrame)
				if err != nil {
					var timeout net.Error
					if errors.As(err, &timeout) && timeout.Timeout() {
						t.Fatal("withdrawal did not abort the transport")
					}
					break
				}
				t.Fatalf("Public payload delivered after withdrawal: %s", raw)
			}
		})
	}
}

func TestLiveReplayDeletesSchemaBeforeAck(t *testing.T) {
	f := newWriterFixture(t)
	database, title, _ := createLiveDatabase(t, f, 1)
	result, err := f.in.Update(t.Context(), Update{Slug: "notes", OpID: "schema", Ops: []Operation{
		CreatePropertyOp{CreateFields: CreateFields{Ref: "extra"}, Database: editTarget(Ref{ID: database, Inc: 2}), Definition: PropertyDef{Name: "Extra", Type: SelectType}},
		CreateOptionOp{CreateFields: CreateFields{Ref: "option"}, Property: Target{Tmp: "extra"}, Name: "Gone", Color: Gray},
		CreateViewOp{CreateFields: CreateFields{Ref: "view"}, Database: editTarget(Ref{ID: database, Inc: 2}), Definition: ViewDef{Name: "Extra view", Display: Display{Layout: "table", Visible: []Target{editTarget(title)}, Widths: []Width{}}, Sort: []Sort{}, Filter: []Filter{}}},
	}}, 0)
	if err != nil {
		t.Fatalf("schema setup: %#v", err)
	}
	property, option, view := result.Refs["extra"].Ref, result.Refs["option"].Ref, result.Refs["view"].Ref
	release := make(chan struct{})
	defer close(release)
	f.in.spills.wrapWriter = func(w io.Writer) io.Writer {
		file := w.(*os.File)
		blocked := false
		f.in.spills.mu.Lock()
		for _, spill := range f.in.spills.spills {
			blocked = blocked || spill.dir == filepath.Dir(file.Name()) && spill.scope.Database == database
		}
		f.in.spills.mu.Unlock()
		if blocked {
			return liveCaptureGateWriter{w, func() { <-release }}
		}
		return w
	}
	initial, _ := liveDial(t, f.in, "private", nil)
	liveHelloSend(t, initial, f.session.Token, f.page.ID)
	liveUntil(t, initial, "welcome")
	var frames []map[string]json.RawMessage
	for {
		frame := liveReceive(t, initial)
		frames = append(frames, frame)
		var scope liveScope
		_ = json.Unmarshal(frame["scope"], &scope)
		var part, parts int
		_ = json.Unmarshal(frame["part"], &part)
		_ = json.Unmarshal(frame["parts"], &parts)
		if scope.Kind == "page" && part+1 == parts {
			break
		}
	}
	initial.Close(1000, "")
	a := Action{N: 1, ActionID: "delete-schema", Ops: []Operation{DeleteOptionOp{Option: editTarget(option)}, DeletePropertyOp{Property: editTarget(property)}, DeleteViewOp{View: editTarget(view)}}}
	if _, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, a); err != nil {
		t.Fatal(err)
	}
	ws, _ := liveDial(t, f.in, "private", nil)
	liveHelloSend(t, ws, f.session.Token, "")
	liveUntil(t, ws, "welcome")
	sendLiveAction(t, ws, a)
	var schemaAbsent bool
	for {
		frame := liveReceive(t, ws)
		frames = append(frames, frame)
		if liveType(frame) == "snapshot" {
			var scope liveScope
			_ = json.Unmarshal(frame["scope"], &scope)
			if scope.Kind == "page" && scope.ID == f.page.ID {
				var data string
				_ = json.Unmarshal(frame["data"], &data)
				schemaAbsent = !strings.Contains(data, string(property.ID)) && !strings.Contains(data, string(option.ID)) && !strings.Contains(data, string(view.ID))
			}
			if scope.Kind == "database" && string(frame["window"]) != "0" {
				t.Fatal("schema replay waited for unrelated rows")
			}
		}
		if liveType(frame) == "ack" {
			if !schemaAbsent {
				t.Fatal("replay ack preceded authoritative schema absence")
			}
			break
		}
	}
	payload, err := json.Marshal(map[string]any{"frames": frames, "removed": []ID{property.ID, option.ID, view.ID}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "node", "--input-type=module", "-e", `
import { Canonical } from './web/state.js';
import { readFileSync } from 'node:fs';
const {frames, removed} = JSON.parse(readFileSync(0, 'utf8'));
const canonical = new Canonical();
let before = false, acked = false;
const fragments = new Map();
for (const frame of frames) {
  if (frame.type === 'snapshot') {
    const key = JSON.stringify([frame.scope, frame.seq, frame.window]);
    const parts = fragments.get(key) || [];
    parts[frame.part] = frame.data;
    fragments.set(key, parts);
    if (parts.filter(x => x !== undefined).length !== frame.parts) continue;
    fragments.delete(key);
    canonical.install(frame.scope, frame.seq, JSON.parse(parts.join('')).items, {
      window: frame.window ?? null, final: frame.window == null || frame.final_window
    });
    before ||= removed.every(id => canonical.get(id));
  }
  if (frame.type === 'ack') {
    if (!before) throw new Error('stale schema was never hydrated');
    if (removed.some(id => canonical.get(id))) throw new Error('deleted schema survived replay ack');
    acked = true;
  }
}
if (!acked) throw new Error('no replay acknowledgement');
`)
	cmd.Stdin = bytes.NewReader(payload)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("canonical receipt hydration: %v\n%s", err, output)
	}
	if err := f.in.ReadTx(t.Context(), func(tx *sql.Tx, _ Sequence) error {
		var alive int
		if err := tx.QueryRowContext(t.Context(), `SELECT count(*) FROM items WHERE id IN (?,?,?) AND alive=1`, property.ID, option.ID, view.ID).Scan(&alive); err != nil {
			return err
		}
		if alive != 0 {
			t.Fatal("schema deletion was not durable")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
