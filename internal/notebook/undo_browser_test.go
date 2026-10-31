package notebook

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/wsconn"
)

func undoBrowserAction(t *testing.T, n uint64, name string) Action {
	t.Helper()
	a, err := DecodeAction([]byte(fmt.Sprintf(`{"n":%q,"action_id":%q,"ops":[{"op":%q}]}`, decimalN(n), fmt.Sprintf("control-%d", n), name)))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func undoBrowserSubmit(t *testing.T, f writerFixture, a Action) Ack {
	t.Helper()
	ack, err := f.in.SubmitAction(t.Context(), f.session.Token, uint64(a.N), a)
	if err != nil {
		t.Fatal(err)
	}
	return ack
}

func undoBrowserSummary(t *testing.T, ack Ack, applied, later, gone, structure int) {
	t.Helper()
	raw, err := json.Marshal(ack)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Undo *struct {
			Applied   int `json:"applied"`
			Later     int `json:"skipped_later"`
			Gone      int `json:"skipped_gone"`
			Structure int `json:"skipped_structure"`
		} `json:"undo"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Undo == nil || wire.Undo.Applied != applied || wire.Undo.Later != later || wire.Undo.Gone != gone || wire.Undo.Structure != structure {
		t.Fatalf("undo summary: %s", raw)
	}
}

func TestUndoBrowserCodec(t *testing.T) {
	for _, name := range []string{"undo", "redo"} {
		t.Run(name, func(t *testing.T) {
			a := undoBrowserAction(t, 1, name)
			raw, err := EncodeAction(a)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := DecodeAction(raw)
			if err != nil || !reflect.DeepEqual(a, replay) {
				t.Fatalf("round trip: %+v %v", replay, err)
			}
			if _, err := DecodeOperation([]byte(fmt.Sprintf(`{"op":%q}`, name)), false); err == nil {
				t.Fatal("agent decoded browser control")
			}
			if _, err := EncodeUpdate(Update{Slug: "notes", OpID: "control", Ops: a.Ops}); err == nil {
				t.Fatal("agent encoded browser control")
			}
			if _, err := DecodeUpdate([]byte(fmt.Sprintf(`{"slug":"notes","op_id":"control","ops":[{"op":%q}]}`, name))); err == nil {
				t.Fatal("agent update decoded browser control")
			}
		})
	}
}

func TestUndoBrowserControlsAreStandalone(t *testing.T) {
	for _, ops := range []string{
		`[{"op":"undo","unit_id":"compound"}]`,
		`[{"op":"redo","extra":true}]`,
		`[{"op":"undo"},{"op":"redo"}]`,
	} {
		t.Run(ops, func(t *testing.T) {
			if _, err := DecodeAction([]byte(`{"n":"1","action_id":"control","ops":` + ops + `}`)); err == nil {
				t.Fatal("accepted compound or extended control")
			}
		})
	}
}

func TestUndoBrowserWriterReplayAndNewEdit(t *testing.T) {
	f := newWriterFixture(t)
	undoBrowserSubmit(t, f, writerAction(f.page, "Draft"))
	undo := undoBrowserAction(t, 2, "undo")
	ack := undoBrowserSubmit(t, f, undo)
	undoBrowserSummary(t, ack, 1, 0, 0, 0)
	if got := writerTitle(t, f.in, f.page); got != "First" {
		t.Fatalf("undo title=%q", got)
	}
	before := writerCount(t, f.in, "deltas")
	if replay := undoBrowserSubmit(t, f, undo); !reflect.DeepEqual(ack, replay) {
		t.Fatalf("different receipt: %+v / %+v", ack, replay)
	}
	if writerCount(t, f.in, "deltas") != before {
		t.Fatal("replay mutated canonical state")
	}
	conflict := undo
	conflict.ActionID = "different"
	_, err := f.in.SubmitAction(t.Context(), f.session.Token, 2, conflict)
	writerReason(t, err, "action_conflict", false)
	redo := undoBrowserSubmit(t, f, undoBrowserAction(t, 3, "redo"))
	undoBrowserSummary(t, redo, 1, 0, 0, 0)
	if got := writerTitle(t, f.in, f.page); got != "Draft" {
		t.Fatalf("redo title=%q", got)
	}
	undoBrowserSubmit(t, f, undoBrowserAction(t, 4, "undo"))
	edit := writerAction(f.page, "New draft")
	edit.N, edit.ActionID = 5, "new-edit"
	undoBrowserSubmit(t, f, edit)
	empty := undoBrowserSubmit(t, f, undoBrowserAction(t, 6, "redo"))
	undoBrowserSummary(t, empty, 0, 0, 0, 0)
	if empty.Summary.Ignored != 1 || empty.Summary.Refused != 0 || writerTitle(t, f.in, f.page) != "New draft" {
		t.Fatalf("new edit did not clear redo: %+v", empty)
	}
	if writerCount(t, f.in, "receipts") != 6 || writerCount(t, f.in, "deltas") != 6 {
		t.Fatal("missing durable action boundaries")
	}
}

func TestUndoBrowserEmptyAndLaterEdits(t *testing.T) {
	f := newWriterFixture(t)
	empty := undoBrowserSubmit(t, f, undoBrowserAction(t, 1, "undo"))
	undoBrowserSummary(t, empty, 0, 0, 0, 0)
	if empty.Summary.Ignored != 1 || empty.Summary.Refused != 0 {
		t.Fatalf("empty undo refused: %+v", empty)
	}
	edit := writerAction(f.page, "Mine")
	edit.N = 2
	undoBrowserSubmit(t, f, edit)
	_, err := f.in.Update(t.Context(), Update{Slug: "notes", OpID: "other-write", Ops: []Operation{SetPageTitleOp{Page: Target{Ref: &f.page}, Title: "Theirs"}}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	ack := undoBrowserSubmit(t, f, undoBrowserAction(t, 3, "undo"))
	undoBrowserSummary(t, ack, 0, 1, 0, 0)
	if writerTitle(t, f.in, f.page) != "Theirs" {
		t.Fatal("undo overwrote another writer")
	}
}

func TestUndoBrowserCommitFailureKeepsStacks(t *testing.T) {
	f := newWriterFixture(t)
	undoBrowserSubmit(t, f, writerAction(f.page, "Draft"))
	f.in.hook = func(stage string, tx *sql.Tx) {
		if stage == "commit" {
			if err := tx.Rollback(); err != nil {
				t.Error(err)
			}
		}
	}
	undo := undoBrowserAction(t, 2, "undo")
	_, err := f.in.SubmitAction(t.Context(), f.session.Token, 2, undo)
	writerReason(t, err, "commit", true)
	if writerTitle(t, f.in, f.page) != "Draft" || writerCount(t, f.in, "receipts") != 1 || writerCount(t, f.in, "deltas") != 1 {
		t.Fatal("failed commit exposed undo or its receipt")
	}
	f.in.hook = nil
	undoBrowserSummary(t, undoBrowserSubmit(t, f, undo), 1, 0, 0, 0)
	if writerTitle(t, f.in, f.page) != "First" {
		t.Fatal("failed commit consumed undo entry")
	}
}

func TestUndoBrowserReopenAndRevisionBoundary(t *testing.T) {
	f := newWriterFixture(t)
	undoBrowserSubmit(t, f, writerAction(f.page, "Draft"))
	// Close the edit's revision before undo so its inverse must dirty the file again.
	f.clock.Add(30)
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error { return f.in.closeGroups(t.Context(), tx, false) }); err != nil {
		t.Fatal(err)
	}
	before := writerCount(t, f.in, "outbox")
	undo := undoBrowserAction(t, 2, "undo")
	ack := undoBrowserSubmit(t, f, undo)
	f.clock.Add(30)
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error { return f.in.closeGroups(t.Context(), tx, false) }); err != nil {
		t.Fatal(err)
	}
	entries, err := f.in.Outbox(t.Context(), 0, 100)
	if err != nil || len(entries) != before+1 {
		t.Fatalf("undo revision boundary: %+v %v", entries, err)
	}
	last := entries[len(entries)-1]
	if last.Kind != "person" || !reflect.DeepEqual(last.Authors, []string{"Person"}) || !reflect.DeepEqual(last.Files, entries[0].Files) {
		t.Fatalf("undo authors: %+v", last)
	}
	dir := filepath.Dir(f.in.db.path)
	f.in.Stop()
	in, created, err := openNotebookFixture(t, dir, f.b, Options{Slug: "notes", Now: func() time.Time { return time.Unix(f.clock.Load(), 0) }})
	if err != nil || created {
		t.Fatalf("reopen: %t %v", created, err)
	}
	defer in.Stop()
	f.in = in
	if replay := undoBrowserSubmit(t, f, undo); !reflect.DeepEqual(ack, replay) {
		t.Fatal("reopen lost undo receipt")
	}
	if writerCount(t, in, "outbox") != len(entries) || writerTitle(t, in, f.page) != "First" {
		t.Fatal("replay changed revision or content")
	}
	undoBrowserSummary(t, undoBrowserSubmit(t, f, undoBrowserAction(t, 3, "redo")), 1, 0, 0, 0)
	if writerTitle(t, in, f.page) != "Draft" {
		t.Fatal("reopen lost redo stack")
	}
}

func TestUndoBrowserQuotaRefusalKeepsStacks(t *testing.T) {
	f := newWriterFixture(t)
	var creates []Operation
	var edits []Operation
	unit := "replace-text"
	base := Version(2)
	for i := 0; i < 8; i++ {
		id, err := NewID(BlockKind)
		if err != nil {
			t.Fatal(err)
		}
		ref := Ref{ID: id, Inc: 2}
		edits = append(edits, SetBlockOp{OperationMeta: OperationMeta{UnitID: &unit}, Block: Target{Ref: &ref}, Content: Content{Kind: Code, Code: "small"}, BaseV: &base})
		creates = append(creates, CreateBlockOp{CreateFields: CreateFields{Ref: fmt.Sprintf("block-%d", i), ProposedID: &id}, Place: Placement{Page: Target{Ref: &f.page}}, Content: Content{Kind: Code, Code: strings.Repeat("x", 8000)}})
	}
	if ack := undoBrowserSubmit(t, f, Action{N: 1, ActionID: "create", Ops: creates}); ack.Summary.Applied != len(creates) {
		t.Fatalf("create fixture: %+v", ack)
	}
	if ack := undoBrowserSubmit(t, f, Action{N: 2, ActionID: "replace", Ops: edits}); ack.Summary.Applied != len(edits) {
		t.Fatalf("edit fixture: %+v", ack)
	}
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error { return f.in.closeGroups(t.Context(), tx, true) }); err != nil {
		t.Fatal(err)
	}
	// Keep the inverse budget above the fixture while limiting remaining disk space.
	f.b.other.Store(64 << 20)
	if err := f.in.Remeasure(t.Context()); err != nil {
		t.Fatal(err)
	}
	var limit int64
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		limit = writerQuotaEstimate(t, f.in, tx, f.b.other.Load(), 0) + (16 << 10)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stack := func() string {
		t.Helper()
		var raw string
		if err := f.in.readers.QueryRowContext(t.Context(), `SELECT json_group_array(json_object('session',session,'stack',stack,'n',entry_order,'components',components,'bytes',bytes)) FROM (SELECT * FROM undo ORDER BY session,stack,entry_order)`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	before := stack()
	f.b.quota.Store(limit)
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error { return f.in.checkQuota(t.Context(), tx, f.b.other.Load(), 0) }); err != nil {
		t.Fatalf("initial state does not fit quota: %v", err)
	}
	undo := undoBrowserAction(t, 3, "undo")
	ack := undoBrowserSubmit(t, f, undo)
	if ack.Summary.Refused != 1 || ack.Results[0].Reason != "disk_quota" {
		t.Fatalf("undo must exceed post-write quota: %+v", ack)
	}
	if got := stack(); got != before {
		t.Fatal("quota refusal consumed or changed inverse stacks")
	}
	if replay := undoBrowserSubmit(t, f, undo); !reflect.DeepEqual(ack, replay) {
		t.Fatal("quota receipt changed on replay")
	}
	var effects struct {
		Changes []json.RawMessage `json:"changes"`
	}
	var raw string
	if err := f.in.readers.QueryRowContext(t.Context(), `SELECT effects FROM deltas WHERE seq=?`, sequenceString(ack.Seq)).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(raw), &effects); err != nil || len(effects.Changes) != 0 {
		t.Fatalf("quota refusal leaked effects: %s %v", raw, err)
	}
	f.b.quota.Store(0)
	undoBrowserSummary(t, undoBrowserSubmit(t, f, undoBrowserAction(t, 4, "undo")), 1, 0, 0, 0)
	var alive int
	if err := f.in.readers.QueryRowContext(t.Context(), `SELECT count(*) FROM blocks b JOIN items i ON i.id=b.id WHERE i.alive=1 AND length(json_extract(i.data,'$.content.text'))=8000`).Scan(&alive); err != nil || alive != len(edits) {
		t.Fatalf("refused inverse not available after quota lifted: %d %v", alive, err)
	}
}

func undoBrowserSocketSend(t *testing.T, ws *wsconn.Conn, a Action) {
	t.Helper()
	raw, err := EncodeAction(a)
	if err != nil {
		t.Fatal(err)
	}
	var frame map[string]any
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatal(err)
	}
	frame["type"] = "action"
	liveSend(t, ws, frame)
}

func undoBrowserSocketAck(t *testing.T, ws *wsconn.Conn, n Sequence, title string) Ack {
	t.Helper()
	var deltaSeq Sequence
	var sawTitle bool
	for {
		frame := liveReceive(t, ws)
		switch liveType(frame) {
		case "reject", "fatal":
			t.Fatalf("action rejected: %s", mustLiveEnvelope("frame", frame))
		case "delta":
			if err := json.Unmarshal(frame["seq"], &deltaSeq); err != nil {
				t.Fatal(err)
			}
			var changes []json.RawMessage
			if err := json.Unmarshal(frame["tree"], &changes); err != nil {
				t.Fatal(err)
			}
			for _, change := range changes {
				var page struct {
					Title string `json:"title"`
				}
				if err := json.Unmarshal(change, &page); err != nil {
					t.Fatal(err)
				}
				sawTitle = sawTitle || page.Title == title
			}
		case "ack":
			delete(frame, "type")
			raw, err := json.Marshal(frame)
			if err != nil {
				t.Fatal(err)
			}
			var ack Ack
			if err := json.Unmarshal(raw, &ack); err != nil {
				t.Fatal(err)
			}
			if ack.N != n || deltaSeq != ack.Seq || !sawTitle {
				t.Fatalf("ack before canonical title %q: %s", title, raw)
			}
			return ack
		}
	}
}

func TestUndoBrowserSocketLifecycleAndReconnect(t *testing.T) {
	f := newWriterFixture(t)
	ws, server := liveDial(t, f.in, "private", nil)
	liveHelloSend(t, ws, f.session.Token, f.page.ID)
	liveUntil(t, ws, "welcome")
	liveUntil(t, ws, "snapshot")
	undoBrowserSocketSend(t, ws, writerAction(f.page, "Draft"))
	undoBrowserSocketAck(t, ws, 1, "Draft")
	undo := undoBrowserAction(t, 2, "undo")
	undoBrowserSocketSend(t, ws, undo)
	ack := undoBrowserSocketAck(t, ws, 2, "First")
	undoBrowserSummary(t, ack, 1, 0, 0, 0)
	ws.Close(1000, "")
	<-ws.Done()
	ws, err := liveConnect(t.Context(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close(1000, "")
	liveHelloSend(t, ws, f.session.Token, f.page.ID)
	liveUntil(t, ws, "welcome")
	liveUntil(t, ws, "snapshot")
	undoBrowserSocketSend(t, ws, undo)
	frame := liveUntil(t, ws, "ack")
	delete(frame, "type")
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	var replay Ack
	if err := json.Unmarshal(raw, &replay); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ack, replay) || writerCount(t, f.in, "receipts") != 2 || writerTitle(t, f.in, f.page) != "First" {
		t.Fatalf("socket replay applied undo twice: %+v", replay)
	}
	undoBrowserSocketSend(t, ws, undoBrowserAction(t, 3, "redo"))
	undoBrowserSummary(t, undoBrowserSocketAck(t, ws, 3, "Draft"), 1, 0, 0, 0)
	edit := writerAction(f.page, "New draft")
	edit.N, edit.ActionID = 4, "new-edit"
	undoBrowserSocketSend(t, ws, edit)
	undoBrowserSocketAck(t, ws, 4, "New draft")
}
